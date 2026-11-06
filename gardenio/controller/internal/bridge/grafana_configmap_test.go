package bridge

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGrafanaConfigMapWriter_CreateAndUpdate(t *testing.T) {
	client := fake.NewClientset()
	w := NewGrafanaConfigMapWriter(client, "gardenio", slog.Default())
	ctx := context.Background()

	uid := "Gardenio-Cost.Overview"
	name, key, err := w.Upsert(ctx, uid, "gd-dash", []byte(`{"uid":"x","version":1}`))
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	if name == "" || key == "" {
		t.Fatalf("empty name/key: %q %q", name, key)
	}

	cm, err := client.CoreV1().ConfigMaps("gardenio").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm: %v", err)
	}
	if cm.Labels[grafanaDashboardLabel] != grafanaDashboardValue {
		t.Fatalf("missing sidecar label: %v", cm.Labels)
	}
	if cm.Labels[grafanaManagedByLabel] != grafanaManagedByValue {
		t.Fatalf("missing managed-by label: %v", cm.Labels)
	}
	if cm.Data[key] != `{"uid":"x","version":1}` {
		t.Fatalf("unexpected data: %v", cm.Data)
	}

	// Update: same uid, new model — must overwrite in place, not error.
	name2, key2, err := w.Upsert(ctx, uid, "gd-dash", []byte(`{"uid":"x","version":2}`))
	if err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if name2 != name || key2 != key {
		t.Fatalf("name/key changed on update: %q/%q vs %q/%q", name2, key2, name, key)
	}
	cm2, _ := client.CoreV1().ConfigMaps("gardenio").Get(ctx, name, metav1.GetOptions{})
	if cm2.Data[key] != `{"uid":"x","version":2}` {
		t.Fatalf("update did not overwrite: %v", cm2.Data)
	}

	// Still exactly one ConfigMap for this dashboard.
	list, _ := client.CoreV1().ConfigMaps("gardenio").List(ctx, metav1.ListOptions{})
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 configmap, got %d", len(list.Items))
	}
}

func TestGrafanaConfigMapWriter_EmptyUID(t *testing.T) {
	w := NewGrafanaConfigMapWriter(fake.NewClientset(), "gardenio", slog.Default())
	if _, _, err := w.Upsert(context.Background(), "  ", "gd-dash", []byte(`{}`)); err == nil {
		t.Fatal("expected error for empty uid")
	}
}

// TestChartOwnsDashboard covers the sidecar-collision guard (ka-0000002774): a
// chart bundle ConfigMap that carries the "<uid>.json" data key is a chart owner;
// the bridge's own ConfigMap and other bridge-managed ConfigMaps are not.
func TestChartOwnsDashboard(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	w := NewGrafanaConfigMapWriter(client, "gardenio-system", slog.Default())

	// A chart bundle ConfigMap that stores perf-traces under gardenio-perf-traces.json,
	// exactly as templates/vtsingle/grafana-dashboard-configmap.yaml renders it.
	if _, err := client.CoreV1().ConfigMaps("gardenio-system").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gardenio-victoria-traces-grafana-dashboard",
			Labels: map[string]string{grafanaDashboardLabel: grafanaDashboardValue},
		},
		Data: map[string]string{"gardenio-perf-traces.json": `{"uid":"gardenio-perf-traces"}`},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed chart cm: %v", err)
	}

	owned, err := w.ChartOwnsDashboard(ctx, "gardenio-perf-traces")
	if err != nil {
		t.Fatalf("chart-owns: %v", err)
	}
	if !owned {
		t.Fatal("expected chart to own gardenio-perf-traces (chart bundle CM carries the key)")
	}

	// A uid with no chart bundle CM is not chart-owned. gardenio-costs-candidate
	// is catalog-generated + bridge-provisioned (kbeads dashboardCatalog(), the
	// authoritative ownership statement) — "not chart-owned" and
	// "catalog-generated" are the same fact, not a contradiction (ka-0000003225).
	owned, err = w.ChartOwnsDashboard(ctx, "gardenio-costs-candidate")
	if err != nil {
		t.Fatalf("chart-owns catalog-generated: %v", err)
	}
	if owned {
		t.Fatal("expected gardenio-costs-candidate to be catalog-generated/bridge-provisioned (no chart bundle CM), not chart-owned")
	}

	// The bridge's OWN ConfigMap must not count as a chart owner (else it would
	// wrongly defer to itself).
	if _, _, err := w.Upsert(ctx, "gb-only", "gd-dash", []byte(`{"uid":"gb-only"}`)); err != nil {
		t.Fatalf("upsert gb-only: %v", err)
	}
	owned, err = w.ChartOwnsDashboard(ctx, "gb-only")
	if err != nil {
		t.Fatalf("chart-owns gb-only: %v", err)
	}
	if owned {
		t.Fatal("bridge-managed ConfigMap must not count as a chart owner")
	}
}

// TestChartOwnsDashboard_ChartBundleKeyShape is the ka-0000004471 regression: a
// chart bundle ConfigMap that stores a dashboard under "gardenio-<uid>.json" (the
// real chart bundle key shape, e.g. "gardenio-cicd-minutes.json" for uid
// "cicd-minutes") must be recognised as chart owner even though the data key is
// NOT the "<uid>.json" the bridge would write. Before the fix the probe matched
// only "<uid>.json", missed this CM, and left the bridge shadow in place →
// duplicate-uid lockout of the whole sidecar provider.
func TestChartOwnsDashboard_ChartBundleKeyShape(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	w := NewGrafanaConfigMapWriter(client, "gardenio-system", slog.Default())

	// Real chart bundle shape: name matches the templated agents bundle CM, the
	// data key is "gardenio-cicd-minutes.json" (grafana-dashboard-configmap.yaml),
	// and the model's embedded top-level uid is "cicd-minutes".
	if _, err := client.CoreV1().ConfigMaps("gardenio-system").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gardenio-agents-grafana-dashboard",
			Labels: map[string]string{grafanaDashboardLabel: grafanaDashboardValue},
		},
		Data: map[string]string{
			"gardenio-cicd-minutes.json": `{"title":"CI/CD — Compute Minutes","uid":"cicd-minutes","version":1}`,
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed chart bundle cm: %v", err)
	}

	owned, err := w.ChartOwnsDashboard(ctx, "cicd-minutes")
	if err != nil {
		t.Fatalf("chart-owns: %v", err)
	}
	if !owned {
		t.Fatal("expected chart to own cicd-minutes: chart bundle CM keyed gardenio-cicd-minutes.json embeds uid=cicd-minutes")
	}

	// End-to-end: a bridge shadow for the same uid must be removable once the
	// chart is recognised as owner (the RemoveOwn path the bug disabled).
	if _, _, err := w.Upsert(ctx, "cicd-minutes", "gd-dash", []byte(`{"uid":"cicd-minutes","version":99}`)); err != nil {
		t.Fatalf("upsert shadow: %v", err)
	}
	owned, err = w.ChartOwnsDashboard(ctx, "cicd-minutes")
	if err != nil {
		t.Fatalf("chart-owns after shadow: %v", err)
	}
	if !owned {
		t.Fatal("chart must still own cicd-minutes even after the bridge writes a competing shadow")
	}
	if err := w.RemoveOwn(ctx, "cicd-minutes"); err != nil {
		t.Fatalf("remove own: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps("gardenio-system").Get(ctx, configMapName("cicd-minutes"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected bridge shadow deleted, got err=%v", err)
	}
	// The chart bundle CM must survive.
	if _, err := client.CoreV1().ConfigMaps("gardenio-system").Get(ctx, "gardenio-agents-grafana-dashboard", metav1.GetOptions{}); err != nil {
		t.Fatalf("chart bundle CM must survive RemoveOwn, got err=%v", err)
	}
}

// TestChartOwnsDashboard_EmbeddedUIDVariants covers the by-embedded-uid match
// for the other chart key shapes (uid != file stem) and confirms a nested
// datasource uid never spoofs ownership.
func TestChartOwnsDashboard_EmbeddedUIDVariants(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	w := NewGrafanaConfigMapWriter(client, "gardenio-system", slog.Default())

	// uid "kbeads-operated-by" ships under data key "operated-by.json".
	if _, err := client.CoreV1().ConfigMaps("gardenio-system").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "chart-operated-by",
			Labels: map[string]string{grafanaDashboardLabel: grafanaDashboardValue},
		},
		Data: map[string]string{"operated-by.json": `{"uid":"kbeads-operated-by"}`},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed operated-by cm: %v", err)
	}
	owned, err := w.ChartOwnsDashboard(ctx, "kbeads-operated-by")
	if err != nil {
		t.Fatalf("chart-owns operated-by: %v", err)
	}
	if !owned {
		t.Fatal("expected chart to own kbeads-operated-by via embedded uid (data key is operated-by.json)")
	}

	// A dashboard whose ONLY uid occurrences are nested datasource refs must not
	// be treated as owning some other uid. Here the top-level uid is
	// "gardenio-fleet" and a panel references datasource uid "victoriametrics";
	// probing for "victoriametrics" must be false.
	if _, err := client.CoreV1().ConfigMaps("gardenio-system").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "chart-fleet",
			Labels: map[string]string{grafanaDashboardLabel: grafanaDashboardValue},
		},
		Data: map[string]string{
			"gardenio-fleet.json": `{"uid":"gardenio-fleet","panels":[{"datasource":{"type":"prometheus","uid":"victoriametrics"}}]}`,
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed fleet cm: %v", err)
	}
	owned, err = w.ChartOwnsDashboard(ctx, "victoriametrics")
	if err != nil {
		t.Fatalf("chart-owns victoriametrics: %v", err)
	}
	if owned {
		t.Fatal("a nested datasource uid must not make ChartOwnsDashboard true")
	}
	// ...but the top-level uid of that same CM is owned.
	owned, err = w.ChartOwnsDashboard(ctx, "gardenio-fleet")
	if err != nil {
		t.Fatalf("chart-owns fleet: %v", err)
	}
	if !owned {
		t.Fatal("expected chart to own gardenio-fleet via its top-level uid")
	}
}

// TestDashboardModelUID unit-checks the top-level-uid extractor.
func TestDashboardModelUID(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  string
	}{
		{"simple", `{"uid":"abc"}`, "abc"},
		{"trims", `{"uid":"  spaced  "}`, "spaced"},
		{"nested only", `{"panels":[{"datasource":{"uid":"ds"}}]}`, ""},
		{"top wins over nested", `{"uid":"top","panels":[{"datasource":{"uid":"ds"}}]}`, "top"},
		{"not json", `not json at all`, ""},
		{"json array", `["uid","x"]`, ""},
		{"empty uid", `{"uid":""}`, ""},
	}
	for _, tc := range cases {
		if got := dashboardModelUID(tc.model); got != tc.want {
			t.Errorf("%s: dashboardModelUID(%q) = %q, want %q", tc.name, tc.model, got, tc.want)
		}
	}
}

// TestRemoveOwn covers shadow cleanup: the bridge deletes only its own ConfigMap
// and never a foreign (chart-owned) one.
func TestRemoveOwn(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	w := NewGrafanaConfigMapWriter(client, "gardenio-system", slog.Default())

	// Bridge writes a shadow, then removes it.
	if _, _, err := w.Upsert(ctx, "gardenio-perf-traces", "gd-dash", []byte(`{"uid":"gardenio-perf-traces"}`)); err != nil {
		t.Fatalf("upsert shadow: %v", err)
	}
	if err := w.RemoveOwn(ctx, "gardenio-perf-traces"); err != nil {
		t.Fatalf("remove own: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps("gardenio-system").Get(ctx, configMapName("gardenio-perf-traces"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected shadow ConfigMap deleted, got err=%v", err)
	}

	// Absent ConfigMap: no-op, no error.
	if err := w.RemoveOwn(ctx, "never-existed"); err != nil {
		t.Fatalf("remove absent: %v", err)
	}

	// A foreign (chart) ConfigMap that happens to share the derived name must NOT
	// be deleted — the managed-by guard protects it.
	foreign := configMapName("foreign")
	if _, err := client.CoreV1().ConfigMaps("gardenio-system").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   foreign,
			Labels: map[string]string{grafanaDashboardLabel: grafanaDashboardValue},
		},
		Data: map[string]string{"foreign.json": `{}`},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed foreign cm: %v", err)
	}
	if err := w.RemoveOwn(ctx, "foreign"); err != nil {
		t.Fatalf("remove foreign: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps("gardenio-system").Get(ctx, foreign, metav1.GetOptions{}); err != nil {
		t.Fatalf("foreign ConfigMap must survive RemoveOwn, got err=%v", err)
	}
}

// TestExists covers the ConfigMap presence probe (ka-0000002299): the bridge's
// own CM is present after Upsert, absent after an out-of-band delete, and a
// present-but-empty CM (no data under the expected key) counts as absent so the
// sync path rewrites it.
func TestExists(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	w := NewGrafanaConfigMapWriter(client, "gardenio-system", slog.Default())
	uid := "cicd-minutes"

	// Absent before any write.
	if present, err := w.Exists(ctx, uid); err != nil || present {
		t.Fatalf("expected absent CM: present=%v err=%v", present, err)
	}

	// Present after Upsert.
	if _, _, err := w.Upsert(ctx, uid, "gd-dash", []byte(`{"uid":"cicd-minutes","version":1}`)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if present, err := w.Exists(ctx, uid); err != nil || !present {
		t.Fatalf("expected present CM after upsert: present=%v err=%v", present, err)
	}

	// Absent after an out-of-band delete (the incident: manual CM delete).
	if err := client.CoreV1().ConfigMaps("gardenio-system").Delete(ctx, configMapName(uid), metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete cm: %v", err)
	}
	if present, err := w.Exists(ctx, uid); err != nil || present {
		t.Fatalf("expected absent CM after delete: present=%v err=%v", present, err)
	}

	// A CM that exists but has no data under the expected key counts as absent.
	empty := configMapName("empty-uid")
	if _, err := client.CoreV1().ConfigMaps("gardenio-system").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   empty,
			Labels: map[string]string{grafanaDashboardLabel: grafanaDashboardValue, grafanaManagedByLabel: grafanaManagedByValue},
		},
		Data: map[string]string{"unrelated.json": `{}`},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed empty cm: %v", err)
	}
	if present, err := w.Exists(ctx, "empty-uid"); err != nil || present {
		t.Fatalf("present-but-keyless CM must count as absent: present=%v err=%v", present, err)
	}

	// Empty uid is a clean false, no error.
	if present, err := w.Exists(ctx, "  "); err != nil || present {
		t.Fatalf("empty uid: present=%v err=%v", present, err)
	}
}

func TestConfigMapNameSanitization(t *testing.T) {
	cases := map[string]string{
		"abc":           cmNamePrefix + "abc",
		"Gardenio-Cost": cmNamePrefix + "gardenio-cost",
		"a/b c":         cmNamePrefix + "a-b-c",
	}
	for in, want := range cases {
		if got := configMapName(in); got != want {
			t.Errorf("configMapName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Upsert writes over only the bridge's own ConfigMap for the uid: an unstamped
// one is claimed, while one stamped with another bead, or not bridge-managed,
// is refused with ErrDashboardUIDOwned (gd-rbEoXGobIg).
func TestGrafanaConfigMapWriter_UpsertOwnership(t *testing.T) {
	ctx := context.Background()
	seed := func(t *testing.T, labels, annotations map[string]string) (*fake.Clientset, *GrafanaConfigMapWriter) {
		t.Helper()
		client := fake.NewClientset()
		if _, err := client.CoreV1().ConfigMaps("gardenio-system").Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        configMapName("board"),
				Labels:      labels,
				Annotations: annotations,
			},
			Data: map[string]string{configMapKey("board"): `{"uid":"board","title":"old"}`},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed cm: %v", err)
		}
		return client, NewGrafanaConfigMapWriter(client, "gardenio-system", slog.Default())
	}
	bridgeLabels := map[string]string{grafanaDashboardLabel: grafanaDashboardValue, grafanaManagedByLabel: grafanaManagedByValue}

	t.Run("unstamped bridge cm is claimed", func(t *testing.T) {
		client, w := seed(t, bridgeLabels, nil)
		if _, _, err := w.Upsert(ctx, "board", "gd-a", []byte(`{"uid":"board"}`)); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		cm, _ := client.CoreV1().ConfigMaps("gardenio-system").Get(ctx, configMapName("board"), metav1.GetOptions{})
		if cm.Annotations[grafanaBeadAnnotation] != "gd-a" {
			t.Fatalf("expected cm stamped gd-a, got %v", cm.Annotations)
		}
	})
	t.Run("other bead's cm is refused", func(t *testing.T) {
		client, w := seed(t, bridgeLabels, map[string]string{grafanaBeadAnnotation: "gd-a"})
		if _, _, err := w.Upsert(ctx, "board", "gd-b", []byte(`{"uid":"board"}`)); !errors.Is(err, ErrDashboardUIDOwned) {
			t.Fatalf("expected ErrDashboardUIDOwned, got %v", err)
		}
		cm, _ := client.CoreV1().ConfigMaps("gardenio-system").Get(ctx, configMapName("board"), metav1.GetOptions{})
		if cm.Data[configMapKey("board")] != `{"uid":"board","title":"old"}` {
			t.Fatalf("refused upsert changed data: %v", cm.Data)
		}
	})
	t.Run("non-bridge cm is refused", func(t *testing.T) {
		_, w := seed(t, map[string]string{grafanaDashboardLabel: grafanaDashboardValue}, nil)
		if _, _, err := w.Upsert(ctx, "board", "gd-a", []byte(`{"uid":"board"}`)); !errors.Is(err, ErrDashboardUIDOwned) {
			t.Fatalf("expected ErrDashboardUIDOwned, got %v", err)
		}
	})
}

func TestProvisionedBead(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	w := NewGrafanaConfigMapWriter(client, "gardenio", slog.Default())
	if got, err := w.ProvisionedBead(ctx, "board"); err != nil || got != "" {
		t.Fatalf("absent cm: got %q, %v", got, err)
	}
	if _, _, err := w.Upsert(ctx, "board", "gd-a", []byte(`{"uid":"board"}`)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got, err := w.ProvisionedBead(ctx, "board"); err != nil || got != "gd-a" {
		t.Fatalf("bridge cm: got %q, %v; want gd-a", got, err)
	}
	if _, err := client.CoreV1().ConfigMaps("gardenio").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        configMapName("chart"),
			Labels:      map[string]string{grafanaDashboardLabel: grafanaDashboardValue},
			Annotations: map[string]string{grafanaBeadAnnotation: "gd-x"},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got, _ := w.ProvisionedBead(ctx, "chart"); got != "" {
		t.Fatalf("non-bridge cm annotation must be ignored, got %q", got)
	}
}
