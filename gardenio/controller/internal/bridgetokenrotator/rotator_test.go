package bridgetokenrotator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	appsv1 "k8s.io/api/apps/v1"

	"gardenio/controller/internal/beadsapi"
)

type fakeDaemon struct {
	infos       []beadsapi.BridgeTokenInfo
	listErr     error
	registerErr error
	registered  []registerCall
}

type registerCall struct {
	token      string
	fleet      string
	subject    string
	access     time.Duration
	refresh    time.Duration
	privileged bool
}

func (f *fakeDaemon) ListBridgeTokensDetailed(ctx context.Context) ([]beadsapi.BridgeTokenInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.infos, nil
}

func (f *fakeDaemon) RegisterBridgeTokenWithRefresh(ctx context.Context, token, fleet, project, subject string, accessTTL, refreshTTL time.Duration, privileged bool) (*beadsapi.BridgeTokenWithRefresh, error) {
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	f.registered = append(f.registered, registerCall{token, fleet, subject, accessTTL, refreshTTL, privileged})
	return &beadsapi.BridgeTokenWithRefresh{RefreshToken: "refresh-" + token}, nil
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewDefaults(t *testing.T) {
	r, err := New(Config{
		Enabled:   true,
		Namespace: "gardenio-e2e",
		Fleets:    []BridgeSpec{{Fleet: "slack-bridge"}},
	}, &fakeDaemon{}, fake.NewClientset(), silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.cfg.Interval != time.Hour {
		t.Errorf("Interval default: got %v", r.cfg.Interval)
	}
	if r.cfg.Threshold != 14*24*time.Hour {
		t.Errorf("Threshold default: got %v", r.cfg.Threshold)
	}
	if r.cfg.AccessTTL != DefaultAccessTTL {
		t.Errorf("AccessTTL default: got %v", r.cfg.AccessTTL)
	}
	if r.cfg.SecretName != "bridge-fleet-tokens" {
		t.Errorf("SecretName default: got %q", r.cfg.SecretName)
	}
	if r.cfg.FleetMapKey != "fleet-map" {
		t.Errorf("FleetMapKey default: got %q", r.cfg.FleetMapKey)
	}
}

func TestNewValidates(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		k8s  bool // nil k8s when false
		want string
	}{
		{
			name: "no fleets",
			cfg:  Config{Enabled: true, Namespace: "ns"},
			k8s:  true,
			want: "no fleets configured",
		},
		{
			name: "no namespace",
			cfg:  Config{Enabled: true, Fleets: []BridgeSpec{{Fleet: "x"}}},
			k8s:  true,
			want: "namespace is empty",
		},
		{
			name: "no k8s in write mode",
			cfg:  Config{Enabled: true, Namespace: "ns", Fleets: []BridgeSpec{{Fleet: "x"}}},
			k8s:  false,
			want: "k8s client is nil",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var k8s kubernetes.Interface
			if tc.k8s {
				k8s = fake.NewClientset()
			}
			_, err := New(tc.cfg, &fakeDaemon{}, k8s, silentLogger())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestNewDisabledSkipsValidation(t *testing.T) {
	_, err := New(Config{Enabled: false}, nil, nil, silentLogger())
	if err != nil {
		t.Fatalf("disabled rotator should not require deps: %v", err)
	}
}

func TestCheckRotationCandidates(t *testing.T) {
	now := time.Date(2026, 5, 25, 22, 0, 0, 0, time.UTC)
	d := &fakeDaemon{infos: []beadsapi.BridgeTokenInfo{
		// Fresh — 30d to go, no rotation.
		{Fleet: "slack-bridge", ExpiresAt: now.Add(30 * 24 * time.Hour)},
		// Stale — 5d to go, rotate.
		{Fleet: "gitlab-bridge", ExpiresAt: now.Add(5 * 24 * time.Hour)},
		// Legacy — no TTL, always rotate.
		{Fleet: "jira-bridge"},
		// Unrelated fleet — ignored.
		{Fleet: "default", ExpiresAt: now.Add(365 * 24 * time.Hour)},
	}}
	r, err := New(Config{
		Enabled:   true,
		Namespace: "ns",
		Threshold: 14 * 24 * time.Hour,
		Fleets: []BridgeSpec{
			{Fleet: "slack-bridge"},
			{Fleet: "gitlab-bridge"},
			{Fleet: "jira-bridge"},
		},
	}, d, fake.NewClientset(), silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.SetNowFunc(func() time.Time { return now })

	cands, err := r.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.Fleet.Fleet] = true
	}
	if got["slack-bridge"] {
		t.Errorf("slack-bridge should not be rotated yet")
	}
	if !got["gitlab-bridge"] {
		t.Errorf("gitlab-bridge should be a candidate (< threshold)")
	}
	if !got["jira-bridge"] {
		t.Errorf("jira-bridge (no TTL) should be a candidate")
	}
}

func TestCheckSkipsUnregisteredFleets(t *testing.T) {
	d := &fakeDaemon{infos: nil}
	r, err := New(Config{
		Enabled:   true,
		Namespace: "ns",
		Fleets:    []BridgeSpec{{Fleet: "slack-bridge"}},
	}, d, fake.NewClientset(), silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cands, err := r.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected no candidates for unregistered fleet, got %+v", cands)
	}
}

func TestRotateDryRun(t *testing.T) {
	d := &fakeDaemon{}
	k8s := fake.NewClientset()
	r, err := New(Config{
		Enabled:   true,
		DryRun:    true,
		Namespace: "ns",
		Fleets:    []BridgeSpec{{Fleet: "slack-bridge"}},
	}, d, k8s, silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.SetGenerateTokenFunc(func() (string, error) { return "should-not-be-used", nil })

	cand := Candidate{Fleet: BridgeSpec{Fleet: "slack-bridge"}}
	if err := r.Rotate(context.Background(), cand); err != nil {
		t.Fatalf("Rotate dry-run: %v", err)
	}
	if len(d.registered) != 0 {
		t.Fatalf("dry-run should not register, got %+v", d.registered)
	}
}

func TestRotateWritesSecretAndBouncesDeployment(t *testing.T) {
	now := time.Date(2026, 5, 25, 22, 0, 0, 0, time.UTC)
	d := &fakeDaemon{}
	// Pre-seed the bridge-fleet-tokens secret and the slack-bridge Deployment.
	k8s := fake.NewClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bridge-fleet-tokens", Namespace: "ns"},
			Data: map[string][]byte{
				"slack-bridge":         []byte("old-access"),
				"slack-bridge-refresh": []byte("old-refresh"),
				"fleet-map":            []byte("old-access:slack-bridge"),
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "slack-bridge", Namespace: "ns"},
		},
	)
	r, err := New(Config{
		Enabled:   true,
		Namespace: "ns",
		Fleets:    []BridgeSpec{{Fleet: "slack-bridge"}},
	}, d, k8s, silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.SetNowFunc(func() time.Time { return now })
	r.SetGenerateTokenFunc(func() (string, error) { return "new-access", nil })

	cand := Candidate{
		Fleet:     BridgeSpec{Fleet: "slack-bridge"},
		ExpiresAt: now.Add(5 * 24 * time.Hour),
		Remaining: 5 * 24 * time.Hour,
	}
	if err := r.Rotate(context.Background(), cand); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Daemon was called with bridge:slack subject + 720h access TTL.
	if len(d.registered) != 1 {
		t.Fatalf("expected 1 register call, got %d", len(d.registered))
	}
	call := d.registered[0]
	if call.token != "new-access" || call.fleet != "slack-bridge" || call.subject != "bridge:slack" {
		t.Fatalf("unexpected register call: %+v", call)
	}
	if call.access != DefaultAccessTTL {
		t.Fatalf("expected access TTL %v, got %v", DefaultAccessTTL, call.access)
	}

	// Secret updated.
	sec, err := k8s.CoreV1().Secrets("ns").Get(context.Background(), "bridge-fleet-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(sec.Data["slack-bridge"]) != "new-access" {
		t.Errorf("access token not rotated: %q", sec.Data["slack-bridge"])
	}
	if string(sec.Data["slack-bridge-refresh"]) != "refresh-new-access" {
		t.Errorf("refresh token not rotated: %q", sec.Data["slack-bridge-refresh"])
	}
	if !strings.Contains(string(sec.Data["fleet-map"]), "new-access:slack-bridge") {
		t.Errorf("fleet-map not rebuilt: %q", sec.Data["fleet-map"])
	}

	// Deployment annotation stamped.
	dep, err := k8s.AppsV1().Deployments("ns").Get(context.Background(), "slack-bridge", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if ann := dep.Spec.Template.Annotations[RotationAnnotation]; ann == "" {
		t.Errorf("rotation annotation missing")
	}
}

func TestRotateMissingSecretErrors(t *testing.T) {
	d := &fakeDaemon{}
	k8s := fake.NewClientset() // no secret pre-seeded
	r, _ := New(Config{
		Enabled:   true,
		Namespace: "ns",
		Fleets:    []BridgeSpec{{Fleet: "slack-bridge"}},
	}, d, k8s, silentLogger())
	r.SetGenerateTokenFunc(func() (string, error) { return "tok", nil })

	err := r.Rotate(context.Background(), Candidate{Fleet: BridgeSpec{Fleet: "slack-bridge"}})
	if err == nil || !strings.Contains(err.Error(), "operator must seed") {
		t.Fatalf("expected seed-required error, got %v", err)
	}
}

// TestRotateForwardsPrivileged asserts the BridgeSpec.Privileged flag is
// passed through to the daemon registration so privileged service fleets
// (e.g. gitlab-bridge) get the ownership-bypass token (ka-0000001946), while
// ordinary fleets do not.
func TestRotateForwardsPrivileged(t *testing.T) {
	for _, privileged := range []bool{true, false} {
		d := &fakeDaemon{}
		k8s := fake.NewClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bridge-fleet-tokens", Namespace: "ns"},
			Data:       map[string][]byte{"gitlab-bridge": []byte("old")},
		})
		spec := BridgeSpec{Fleet: "gitlab-bridge", Privileged: privileged}
		r, _ := New(Config{
			Enabled:   true,
			Namespace: "ns",
			Fleets:    []BridgeSpec{spec},
		}, d, k8s, silentLogger())
		r.SetGenerateTokenFunc(func() (string, error) { return "tok", nil })

		if err := r.Rotate(context.Background(), Candidate{Fleet: spec}); err != nil {
			t.Fatalf("Rotate(privileged=%v): %v", privileged, err)
		}
		if len(d.registered) != 1 {
			t.Fatalf("privileged=%v: expected 1 register call, got %d", privileged, len(d.registered))
		}
		if got := d.registered[0].privileged; got != privileged {
			t.Errorf("privileged=%v: register call privileged = %v, want %v", privileged, got, privileged)
		}
	}
}

func TestRotateRegisterFailureDoesNotPatchK8s(t *testing.T) {
	d := &fakeDaemon{registerErr: errors.New("daemon down")}
	k8s := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bridge-fleet-tokens", Namespace: "ns"},
		Data:       map[string][]byte{"slack-bridge": []byte("old")},
	})
	r, _ := New(Config{
		Enabled:   true,
		Namespace: "ns",
		Fleets:    []BridgeSpec{{Fleet: "slack-bridge"}},
	}, d, k8s, silentLogger())
	r.SetGenerateTokenFunc(func() (string, error) { return "tok", nil })

	err := r.Rotate(context.Background(), Candidate{Fleet: BridgeSpec{Fleet: "slack-bridge"}})
	if err == nil {
		t.Fatal("expected error from daemon failure")
	}
	sec, _ := k8s.CoreV1().Secrets("ns").Get(context.Background(), "bridge-fleet-tokens", metav1.GetOptions{})
	if string(sec.Data["slack-bridge"]) != "old" {
		t.Errorf("secret should not be mutated on daemon failure, got %q", sec.Data["slack-bridge"])
	}
}

func TestDeriveSubject(t *testing.T) {
	tests := map[string]string{
		"slack-bridge":  "bridge:slack",
		"gitlab-bridge": "bridge:gitlab",
		"jira-bridge":   "bridge:jira",
		"custom":        "bridge:custom",
	}
	for in, want := range tests {
		if got := deriveSubject(in); got != want {
			t.Errorf("deriveSubject(%q) = %q, want %q", in, got, want)
		}
	}
}
