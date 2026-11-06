// Package bridge — Grafana dashboard ConfigMap write path.
//
// The default (and least-privileged) beads→Grafana write path: render a
// dashboard model into a ConfigMap labeled grafana_dashboard="1" so the Grafana
// sidecar provisions it. This path touches only the k8s API — no Grafana
// credentials are required (see W2 design doc §7).
//
// Design: gardenio/docs/research/obs-w2-grafana-bridge.md (epic ka-0000006776).
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// grafanaDashboardLabel is the sidecar discovery label. Matches the
	// shipped helm/gardenio/templates/controller/grafana-dashboard-configmap.yaml.
	grafanaDashboardLabel = "grafana_dashboard"
	grafanaDashboardValue = "1"

	// managedByLabel marks ConfigMaps the grafana-bridge owns so it can list
	// only its own dashboards and never clobber the chart-shipped bundle CM.
	grafanaManagedByLabel = "app.kubernetes.io/managed-by"
	grafanaManagedByValue = "grafana-bridge"

	// uidLabel records the dashboard uid for cheap lookup.
	grafanaUIDLabel = "gardenio.io/dashboard-uid"

	// grafanaBeadAnnotation records the dashboard bead a bridge ConfigMap is
	// provisioned from, so Upsert can refuse a different bead claiming the uid.
	grafanaBeadAnnotation = "gardenio.io/dashboard-bead"

	// cmNamePrefix prefixes per-dashboard ConfigMap names.
	cmNamePrefix = "grafana-dashboard-"
)

// k8sNameSanitizer keeps only DNS-1123-safe characters for ConfigMap names.
var k8sNameSanitizer = regexp.MustCompile(`[^a-z0-9-]`)

// GrafanaConfigMapWriter upserts per-dashboard ConfigMaps for the Grafana
// sidecar to provision. One ConfigMap per dashboard uid keeps the chart-shipped
// bundle CM untouched and lets the bridge own its dashboards independently.
type GrafanaConfigMapWriter struct {
	client    kubernetes.Interface
	namespace string
	logger    *slog.Logger
}

// NewGrafanaConfigMapWriter constructs a writer bound to a namespace.
func NewGrafanaConfigMapWriter(client kubernetes.Interface, namespace string, logger *slog.Logger) *GrafanaConfigMapWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &GrafanaConfigMapWriter{client: client, namespace: namespace, logger: logger}
}

// configMapName derives a stable, DNS-1123-safe ConfigMap name from a uid.
func configMapName(uid string) string {
	s := strings.ToLower(strings.TrimSpace(uid))
	s = k8sNameSanitizer.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "unnamed"
	}
	name := cmNamePrefix + s
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

// configMapKey is the data key the model JSON lives under inside the CM.
func configMapKey(uid string) string {
	s := strings.ToLower(strings.TrimSpace(uid))
	s = k8sNameSanitizer.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "dashboard"
	}
	return s + ".json"
}

// Upsert writes the dashboard model JSON into a grafana_dashboard="1"
// ConfigMap stamped with beadID, creating or overwriting as needed. It returns
// the ConfigMap name and the data key used. It refuses (ErrDashboardUIDOwned)
// to overwrite a ConfigMap of that name the bridge does not manage, or one
// stamped with a different bead; an unstamped bridge ConfigMap is claimed.
func (w *GrafanaConfigMapWriter) Upsert(ctx context.Context, uid, beadID string, model []byte) (cmName, dataKey string, err error) {
	if strings.TrimSpace(uid) == "" {
		return "", "", fmt.Errorf("grafana configmap: empty dashboard uid")
	}
	name := configMapName(uid)
	key := configMapKey(uid)
	labels := map[string]string{
		grafanaDashboardLabel: grafanaDashboardValue,
		grafanaManagedByLabel: grafanaManagedByValue,
		grafanaUIDLabel:       configMapToken(uid),
	}
	annotations := map[string]string{grafanaBeadAnnotation: beadID}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   w.namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Data: map[string]string{key: string(model)},
	}
	_, err = w.client.CoreV1().ConfigMaps(w.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := w.client.CoreV1().ConfigMaps(w.namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return "", "", fmt.Errorf("grafana configmap: get existing %q: %w", name, getErr)
		}
		if existing.Labels[grafanaManagedByLabel] != grafanaManagedByValue {
			return "", "", fmt.Errorf("grafana configmap %q is not bridge-managed: %w", name, ErrDashboardUIDOwned)
		}
		if owner := existing.Annotations[grafanaBeadAnnotation]; owner != "" && owner != beadID {
			return "", "", fmt.Errorf("grafana configmap %q is provisioned from bead %s, not %s: %w", name, owner, beadID, ErrDashboardUIDOwned)
		}
		maps.Copy(existing.Labels, labels)
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		maps.Copy(existing.Annotations, annotations)
		if existing.Data == nil {
			existing.Data = map[string]string{}
		}
		existing.Data[key] = string(model)
		if _, upErr := w.client.CoreV1().ConfigMaps(w.namespace).Update(ctx, existing, metav1.UpdateOptions{}); upErr != nil {
			return "", "", fmt.Errorf("grafana configmap: update %q: %w", name, upErr)
		}
		w.logger.Info("grafana configmap upserted (update)", "name", name, "uid", uid, "key", key)
		return name, key, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("grafana configmap: create %q: %w", name, err)
	}
	w.logger.Info("grafana configmap upserted (create)", "name", name, "uid", uid, "key", key)
	return name, key, nil
}

// ChartOwnsDashboard reports whether a chart-shipped (non-bridge)
// grafana_dashboard ConfigMap already provisions this uid's dashboard.
//
// Why the bridge must defer when it does (ka-0000002774): the Grafana sidecar
// flattens EVERY grafana_dashboard="1" ConfigMap's data keys into a single
// provisioning folder and then keys each provisioned dashboard by its embedded
// model uid. Two ConfigMaps whose models carry the SAME uid — even under
// different data-key filenames — are a duplicate-uid collision. Grafana's
// response is severe: it logs "the same UID is used more than once" and revokes
// database write permission for the ENTIRE sidecar provider, so NO
// sidecar-provisioned dashboard (chart or bridge) can be saved until the
// duplicate is gone. The chart bundle ConfigMap is the maintained source
// (rebaked by every release and by the dashboards-sync job), so for
// chart-shipped dashboards the bridge defers to it instead of shadowing.
//
// Matching by embedded uid, not by data-key filename (ka-0000004471): the chart
// bundle stores dashboards under human-chosen data keys that are NOT uniformly
// "<uid>.json" — e.g. uid "cicd-minutes" ships under "gardenio-cicd-minutes.json"
// and uid "kbeads-operated-by" under "operated-by.json". The bridge writes
// "<uid>.json" (configMapKey), so a filename-only probe misses these chart CMs,
// never fires RemoveOwn, and both ConfigMaps land in the sidecar folder under
// the same uid — the exact dup-uid lockout above. So we parse each candidate
// ConfigMap's model JSON and compare the embedded TOP-LEVEL uid, which is the
// value Grafana actually dedupes on. As a cheap fallback (and for values that
// fail to parse) we also accept the legacy "<uid>.json" data-key match.
//
// A ConfigMap counts as a chart owner only if it is NOT bridge-managed
// (app.kubernetes.io/managed-by != grafana-bridge). The bridge's own ConfigMap
// for this uid is ignored.
func (w *GrafanaConfigMapWriter) ChartOwnsDashboard(ctx context.Context, uid string) (bool, error) {
	wantUID := strings.TrimSpace(uid)
	if wantUID == "" {
		return false, nil
	}
	key := configMapKey(uid)
	ownName := configMapName(uid)
	list, err := w.client.CoreV1().ConfigMaps(w.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: grafanaDashboardLabel + "=" + grafanaDashboardValue,
	})
	if err != nil {
		return false, fmt.Errorf("grafana configmap: list dashboards in %q: %w", w.namespace, err)
	}
	for i := range list.Items {
		cm := &list.Items[i]
		if cm.Name == ownName {
			continue // our own (possibly stale) shadow — not a chart owner
		}
		if cm.Labels[grafanaManagedByLabel] == grafanaManagedByValue {
			continue // another bridge-managed ConfigMap — not the chart
		}
		// Primary match: any data value whose model JSON embeds this uid, under
		// whatever data-key filename the chart chose. This is what Grafana
		// actually dedupes on, so it catches every chart bundle key shape.
		for _, val := range cm.Data {
			if dashboardModelUID(val) == wantUID {
				return true, nil
			}
		}
		// Legacy fallback: the "<uid>.json" data key, for values that do not
		// parse as a dashboard model (kept so a malformed-but-named chart CM is
		// still recognised as owner).
		if _, ok := cm.Data[key]; ok {
			return true, nil
		}
	}
	return false, nil
}

// dashboardModelUID extracts the top-level "uid" from a Grafana dashboard model
// JSON document. It returns "" when the value is not a JSON object or has no
// top-level uid — matching only the field Grafana keys provisioning on, never a
// nested datasource uid (e.g. "${DS_PROMETHEUS}"). Decoding only the top-level
// field keeps this cheap even for large models.
func dashboardModelUID(model string) string {
	var doc struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal([]byte(model), &doc); err != nil {
		return ""
	}
	return strings.TrimSpace(doc.UID)
}

// RemoveOwn deletes the bridge-managed ConfigMap for this uid, so a shadow the
// bridge wrote before a dashboard became chart-owned stops competing with the
// chart bundle ConfigMap in the sidecar folder. It is a no-op when the ConfigMap
// is absent, and it NEVER deletes a ConfigMap the bridge does not own — the
// managed-by guard means it can only remove the bridge's own copy.
func (w *GrafanaConfigMapWriter) RemoveOwn(ctx context.Context, uid string) error {
	name := configMapName(uid)
	existing, err := w.client.CoreV1().ConfigMaps(w.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("grafana configmap: get %q: %w", name, err)
	}
	if existing.Labels[grafanaManagedByLabel] != grafanaManagedByValue {
		return nil // not ours — refuse to delete a foreign ConfigMap
	}
	if err := w.client.CoreV1().ConfigMaps(w.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("grafana configmap: delete %q: %w", name, err)
	}
	w.logger.Info("grafana configmap removed (chart owns dashboard)", "name", name, "uid", uid)
	return nil
}

// Exists reports whether the bridge's own ConfigMap for this uid is present with
// a non-empty model under the expected data key. SyncDashboard uses it to verify
// the artifact its hash loop-breaker short-circuits on still exists, so a
// ConfigMap deleted out-of-band (e.g. to clear a dup-uid sidecar lockout) is
// re-provisioned on the next sync/catch-up even though the bead's stamped
// dashboard_hash is unchanged (ka-0000002299).
//
// It checks the specific per-uid ConfigMap name/key the bridge writes, not the
// chart-ownership List — a present-but-empty CM (no data under configMapKey(uid))
// counts as absent because it provisions nothing, so the sync path rewrites it.
// A NotFound is a clean false with no error. Any other Get error is surfaced so
// the caller can decide how to treat an unverifiable probe (SyncDashboard fails
// closed toward writing).
func (w *GrafanaConfigMapWriter) Exists(ctx context.Context, uid string) (bool, error) {
	if strings.TrimSpace(uid) == "" {
		return false, nil
	}
	name := configMapName(uid)
	key := configMapKey(uid)
	cm, err := w.client.CoreV1().ConfigMaps(w.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("grafana configmap: get %q: %w", name, err)
	}
	return strings.TrimSpace(cm.Data[key]) != "", nil
}

// ProvisionedBead returns the dashboard bead the bridge's own ConfigMap for
// uid is stamped with (the gardenio.io/dashboard-bead annotation), or "" when
// that ConfigMap is absent, not bridge-managed, or unstamped.
func (w *GrafanaConfigMapWriter) ProvisionedBead(ctx context.Context, uid string) (string, error) {
	if strings.TrimSpace(uid) == "" {
		return "", nil
	}
	name := configMapName(uid)
	cm, err := w.client.CoreV1().ConfigMaps(w.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("grafana configmap: get %q: %w", name, err)
	}
	if cm.Labels[grafanaManagedByLabel] != grafanaManagedByValue {
		return "", nil
	}
	return cm.Annotations[grafanaBeadAnnotation], nil
}

// configMapToken sanitizes an arbitrary string into a label-value-safe token
// (label values must match (([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]) and be
// ≤63 chars). Mirrors the snapshot-store helper.
func configMapToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = regexp.MustCompile(`[^a-z0-9_.-]`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_.")
	if s == "" {
		s = "unnamed"
	}
	if len(s) > 63 {
		s = s[:63]
		s = strings.Trim(s, "-_.")
	}
	return s
}
