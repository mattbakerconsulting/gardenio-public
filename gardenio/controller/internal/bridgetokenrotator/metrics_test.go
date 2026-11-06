package bridgetokenrotator

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/beadsapi"
)

// seedSecret builds a minimal bridge-fleet-tokens Secret so a real (non-dry-run)
// Rotate can Get→mutate→Update it without hitting the not-found guard.
func seedSecret(ns, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{},
	}
}

// TestMetricsNilSafe verifies the observe* helpers no-op on a nil sink, so the
// rotator's disabled/no-metrics path (and every existing test) stays safe.
func TestMetricsNilSafe(t *testing.T) {
	var m *Metrics
	// Each of these would panic on a real nil-map deref if not guarded.
	m.observeExpiry("slack-bridge", 123, 456)
	m.observeCheckOK(789)
	m.observeCheckError()
	m.observeRotation("slack-bridge", rotationResultSuccess)
}

// TestMetricsRegisterAndCheckPublishesExpiry drives one Check tick with metrics
// attached and asserts the load-bearing expiry series is written for EVERY
// configured fleet — the fresh one included — not just the rotation candidates.
func TestMetricsRegisterAndCheckPublishesExpiry(t *testing.T) {
	now := time.Date(2026, 5, 25, 22, 0, 0, 0, time.UTC)
	freshExp := now.Add(30 * 24 * time.Hour)
	staleExp := now.Add(5 * 24 * time.Hour)
	d := &fakeDaemon{infos: []beadsapi.BridgeTokenInfo{
		{Fleet: "slack-bridge", ExpiresAt: freshExp},
		{Fleet: "gitlab-bridge", ExpiresAt: staleExp},
		{Fleet: "jira-bridge"}, // legacy, no TTL → expires_at 0 (maximally overdue)
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

	reg := prometheus.NewRegistry()
	m := NewMetrics()
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.SetMetrics(m)

	if _, err := r.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}

	// The fresh fleet (not a rotation candidate) must still carry an expiry
	// series — that is exactly the value the overdue alert reads.
	if got := testutil.ToFloat64(m.expiresAt.WithLabelValues("slack-bridge")); got != float64(freshExp.Unix()) {
		t.Errorf("slack-bridge expires_at = %v, want %v", got, freshExp.Unix())
	}
	if got := testutil.ToFloat64(m.expiresAt.WithLabelValues("gitlab-bridge")); got != float64(staleExp.Unix()) {
		t.Errorf("gitlab-bridge expires_at = %v, want %v", got, staleExp.Unix())
	}
	// Legacy token → epoch 0, which reads as maximally overdue.
	if got := testutil.ToFloat64(m.expiresAt.WithLabelValues("jira-bridge")); got != 0 {
		t.Errorf("jira-bridge (no TTL) expires_at = %v, want 0", got)
	}
	// Deadman stamped on the successful check.
	if got := testutil.ToFloat64(m.lastCheckTS); got != float64(now.Unix()) {
		t.Errorf("last_check = %v, want %v", got, now.Unix())
	}
}

// TestMetricsCheckErrorLeavesGaugesStale asserts a failed daemon list increments
// the check-error counter and does NOT zero a previously-published expiry gauge
// — a transient daemon blip must not trip the overdue alert.
func TestMetricsCheckErrorLeavesGaugesStale(t *testing.T) {
	now := time.Date(2026, 5, 25, 22, 0, 0, 0, time.UTC)
	exp := now.Add(20 * 24 * time.Hour)
	d := &fakeDaemon{infos: []beadsapi.BridgeTokenInfo{{Fleet: "slack-bridge", ExpiresAt: exp}}}
	r, err := New(Config{
		Enabled:   true,
		Namespace: "ns",
		Threshold: 14 * 24 * time.Hour,
		Fleets:    []BridgeSpec{{Fleet: "slack-bridge"}},
	}, d, fake.NewClientset(), silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.SetNowFunc(func() time.Time { return now })
	m := NewMetrics()
	if err := m.Register(prometheus.NewRegistry()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.SetMetrics(m)

	// First tick succeeds and publishes the expiry.
	if _, err := r.Check(context.Background()); err != nil {
		t.Fatalf("Check (ok): %v", err)
	}
	want := float64(exp.Unix())
	if got := testutil.ToFloat64(m.expiresAt.WithLabelValues("slack-bridge")); got != want {
		t.Fatalf("expires_at after ok check = %v, want %v", got, want)
	}

	// Next tick: daemon errors. The gauge must remain at its last value.
	d.listErr = context.DeadlineExceeded
	if _, err := r.Check(context.Background()); err == nil {
		t.Fatalf("Check (err) expected error, got nil")
	}
	if got := testutil.ToFloat64(m.checkErrors); got != 1 {
		t.Errorf("check_errors_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.expiresAt.WithLabelValues("slack-bridge")); got != want {
		t.Errorf("expires_at must stay stale on check error, got %v want %v", got, want)
	}
}

// TestMetricsRotationCounters exercises the tick loop and asserts the rotation
// counter records dry_run in observe mode and success on a real write.
func TestMetricsRotationCounters(t *testing.T) {
	now := time.Date(2026, 5, 25, 22, 0, 0, 0, time.UTC)
	// gitlab-bridge is stale → a rotation candidate.
	mkDaemon := func() *fakeDaemon {
		return &fakeDaemon{infos: []beadsapi.BridgeTokenInfo{
			{Fleet: "gitlab-bridge", ExpiresAt: now.Add(2 * 24 * time.Hour)},
		}}
	}

	t.Run("dry_run", func(t *testing.T) {
		r, err := New(Config{
			Enabled:   true,
			DryRun:    true,
			Namespace: "ns",
			Threshold: 14 * 24 * time.Hour,
			Fleets:    []BridgeSpec{{Fleet: "gitlab-bridge"}},
		}, mkDaemon(), fake.NewClientset(), silentLogger())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		r.SetNowFunc(func() time.Time { return now })
		m := NewMetrics()
		_ = m.Register(prometheus.NewRegistry())
		r.SetMetrics(m)

		r.tick(context.Background())

		if got := testutil.ToFloat64(m.rotations.WithLabelValues("gitlab-bridge", rotationResultDryRun)); got != 1 {
			t.Errorf("dry_run rotations = %v, want 1", got)
		}
		if got := testutil.ToFloat64(m.rotations.WithLabelValues("gitlab-bridge", rotationResultSuccess)); got != 0 {
			t.Errorf("success rotations = %v, want 0 in dry-run", got)
		}
	})

	t.Run("success", func(t *testing.T) {
		k8s := fake.NewClientset(seedSecret("ns", "bridge-fleet-tokens"))
		r, err := New(Config{
			Enabled:   true,
			DryRun:    false,
			Namespace: "ns",
			Threshold: 14 * 24 * time.Hour,
			Fleets:    []BridgeSpec{{Fleet: "gitlab-bridge"}},
		}, mkDaemon(), k8s, silentLogger())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		r.SetNowFunc(func() time.Time { return now })
		r.SetGenerateTokenFunc(func() (string, error) { return "fresh-tok", nil })
		m := NewMetrics()
		_ = m.Register(prometheus.NewRegistry())
		r.SetMetrics(m)

		r.tick(context.Background())

		if got := testutil.ToFloat64(m.rotations.WithLabelValues("gitlab-bridge", rotationResultSuccess)); got != 1 {
			t.Errorf("success rotations = %v, want 1", got)
		}
		if got := testutil.ToFloat64(m.rotations.WithLabelValues("gitlab-bridge", rotationResultDryRun)); got != 0 {
			t.Errorf("dry_run rotations = %v, want 0 on real write", got)
		}
	})
}
