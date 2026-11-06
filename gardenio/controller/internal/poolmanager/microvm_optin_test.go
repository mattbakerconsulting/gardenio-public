package poolmanager

import (
	"context"
	"net/http/httptest"
	"testing"

	"log/slog"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// The lambda_microvm warm pool is a real, continuously-billing cost knob: an
// idle MicroVM member bills for its whole standby life and burns a wall-clock
// lifetime cap. The plan-of-record contract (feature ka-0000005825, cost model
// §3c, Phase 4) is therefore "default 0/off per project, explicit opt-in". These
// tests pin the Reconcile-level opt-in guard: a MicroVM pool with no configured
// min_size mints ZERO members (EffectivePools' K8s default of 2 is NOT applied),
// while an explicitly-sized MicroVM pool and every K8s pool behave exactly as
// before.

// newReconcileManager wires a Manager against a reconcileMock daemon.
func newReconcileManager(t *testing.T, cfg *config.Config) (*Manager, *reconcileMock) {
	t.Helper()
	mock := &reconcileMock{t: t}
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return New(daemon, cfg, slog.Default()), mock
}

// A lambda_microvm pool the operator ENABLED but never sized (min_size unset) is
// NOT opted in: the guard keeps it at 0/off and mints nothing, rather than
// spinning up the K8s default of 2 continuously-billing VMs.
func TestReconcile_MicrovmPoolUnsizedStaysOff(t *testing.T) {
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {
			ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM,
			// Enabled, but MinSize deliberately 0 → not opted in.
			PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
				Enabled: true, MaxSize: 5, Role: "thread", Mode: "crew",
			},
		},
	}}
	pm, mock := newReconcileManager(t, cfg)

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if got := mock.created.Load(); got != 0 {
		t.Errorf("un-opted-in MicroVM pool created %d members, want 0 (default off)", got)
	}
}

// A lambda_microvm pool with an EXPLICIT min_size is opted in and mints exactly
// that many members — the whole point of the feature once cost is accepted.
func TestReconcile_MicrovmPoolExplicitMinMints(t *testing.T) {
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {
			ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM,
			PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
				Enabled: true, MinSize: 1, MaxSize: 2, Role: "thread", Mode: "crew",
			},
		},
	}}
	pm, mock := newReconcileManager(t, cfg)

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if got := mock.created.Load(); got != 1 {
		t.Errorf("opted-in MicroVM pool (min_size=1) created %d members, want 1", got)
	}
}

// A K8s pool (the default backend) is unaffected by the MicroVM guard: an unset
// min_size still gets EffectivePools' default of 2, because idle K8s pods are
// free and the legacy warm-pool behaviour must be byte-for-byte preserved.
func TestReconcile_KubernetesPoolUnsizedStillDefaultsToTwo(t *testing.T) {
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {
			// No ExecutionTarget → kubernetes; MinSize unset.
			PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
				Enabled: true, MaxSize: 5, Role: "thread", Mode: "crew",
			},
		},
	}}
	pm, mock := newReconcileManager(t, cfg)

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if got := mock.created.Load(); got != 2 {
		t.Errorf("K8s pool with unset min_size created %d members, want 2 (default preserved)", got)
	}
}

// Removing the opt-in (min_size back to 0) on a MicroVM pool that HAS live warm
// members drains them to zero: the guard drops the pool from the enabled set, so
// the existing prewarmed members are closed as orphans (the §4 cost-guard/
// default-off drain). This is the recover-from-cost path — stop paying the
// moment the operator un-sizes the pool.
func TestReconcile_MicrovmPoolUnsizedDrainsExistingMembers(t *testing.T) {
	mock := &reconcileMock{t: t, agents: []map[string]any{
		{
			"id":     "gd-warm-mv",
			"title":  "warm-mv",
			"type":   "agent",
			"status": "open",
			"fields": map[string]string{
				"agent":            "prewarmed-warm-mv",
				"agent_state":      "prewarmed",
				"project":          "gardenio",
				"role":             "thread",
				"execution_target": podmanager.ExecutionTargetLambdaMicroVM,
			},
		},
	}}
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectCache: map[string]config.ProjectCacheEntry{
		"gardenio": {
			ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM,
			// Enabled but un-sized → not opted in; the live member must drain.
			PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
				Enabled: true, MaxSize: 1, Role: "thread", Mode: "crew",
			},
		},
	}}
	pm := New(daemon, cfg, slog.Default())

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if got := mock.closed.Load(); got != 1 {
		t.Errorf("un-opted-in MicroVM pool closed %d live members, want 1 (drain to zero)", got)
	}
	if got := mock.created.Load(); got != 0 {
		t.Errorf("un-opted-in MicroVM pool created %d members, want 0", got)
	}
}

// The opt-in is resolved per (project,role) at the SAME execution-target layering
// the spawn uses: a role override that flips the pool onto lambda_microvm makes
// the guard apply even though the project is K8s. An unsized such pool stays off.
func TestReconcile_MicrovmViaRoleOverrideUnsizedStaysOff(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {
				// Project is K8s; the role override below promotes it to MicroVM.
				PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
					Enabled: true, MaxSize: 5, Role: "gardenio-microvms", Mode: "crew",
				},
			},
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"gardenio-microvms": {ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM},
		},
	}
	pm, mock := newReconcileManager(t, cfg)

	if err := pm.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if got := mock.created.Load(); got != 0 {
		t.Errorf("role-promoted MicroVM pool created %d members, want 0 (default off)", got)
	}
}
