package reconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// gvisorSpecBuilder routes beads whose role is "runsc" into the gVisor
// namespace, mirroring routeGVisorNamespace: a runsc agent's pod lands in
// cfg.GVisorNamespace, everything else stays in cfg.Namespace. This lets the
// cap tests assert that gVisor-namespace pods are budgeted separately from the
// per-project cap (ka-0000004152).
func gvisorSpecBuilder(image string) SpecBuilder {
	return func(cfg *config.Config, project, mode, role, agentName string, _ map[string]string) podmanager.AgentPodSpec {
		ns := cfg.Namespace
		if role == "runsc" && cfg.GVisorNamespace != "" {
			ns = cfg.GVisorNamespace
		}
		return podmanager.AgentPodSpec{
			Project:   project,
			Mode:      mode,
			Role:      role,
			AgentName: agentName,
			Image:     image,
			Namespace: ns,
		}
	}
}

// gvisorCapConfig is testConfig with the per-project cap saturated and the
// gVisor namespace configured, so a runsc sub-pool can only schedule if its
// pods are exempt from the per-project cap.
func gvisorCapConfig() *config.Config {
	cfg := testConfig("gardenio")
	cfg.CoopBurstLimit = 10
	cfg.GVisorNamespace = "gardenio-gvisor"
	cfg.CoopMaxPodsPerProject = 1 // saturated by one existing gardenio pod
	return cfg
}

// TestReconcile_GVisorPodNotCappedByPerProject is the core CAP STARVATION
// regression (ka-0000004152): with the gardenio per-project cap saturated, a
// runsc bead destined for the gVisor namespace must still be created — its pod
// is budgeted separately, not against the per-project cap.
func TestReconcile_GVisorPodNotCappedByPerProject(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{
		// A normal gardenio agent (already running, saturates per-project cap=1).
		{ID: "bd-norm", Project: "gardenio", Mode: "crew", Role: "crew", AgentName: "norm"},
		// A runsc pool agent that should land in the gVisor namespace.
		{ID: "bd-runsc", Project: "gardenio", Mode: "crew", Role: "runsc", AgentName: "warm"},
	}}
	mgr := &mockManager{podsByNS: map[string][]corev1.Pod{
		"gardenio": {
			makePod("crew-gardenio-crew-norm", "gardenio", "crew", "gardenio", "crew", "norm", corev1.PodRunning),
		},
		"gardenio-gvisor": nil,
	}}

	r := New(lister, mgr, gvisorCapConfig(), testLogger(), gvisorSpecBuilder("img:v1"))
	r.SetExtraNamespaces("gardenio-gvisor")
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The runsc pod must be created (exempt from the saturated per-project cap);
	// the normal agent already has a pod, so it is not recreated.
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 pod created (the runsc pod), got %d", len(mgr.created))
	}
	if mgr.created[0].AgentName != "warm" || mgr.created[0].Namespace != "gardenio-gvisor" {
		t.Errorf("expected runsc pod 'warm' in gardenio-gvisor, got %q in %q",
			mgr.created[0].AgentName, mgr.created[0].Namespace)
	}
}

// TestReconcile_GVisorBudgetCapsGVisorPods verifies the distinct gVisor budget
// (CoopMaxPodsGVisor) caps gVisor-namespace pods when set: with one runsc pod
// already running and the budget at 1, a second runsc bead is deferred.
func TestReconcile_GVisorBudgetCapsGVisorPods(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{
		{ID: "bd-r1", Project: "gardenio", Mode: "crew", Role: "runsc", AgentName: "warm1"},
		{ID: "bd-r2", Project: "gardenio", Mode: "crew", Role: "runsc", AgentName: "warm2"},
	}}
	mgr := &mockManager{podsByNS: map[string][]corev1.Pod{
		"gardenio": nil,
		"gardenio-gvisor": {
			makePod("crew-gardenio-runsc-warm1", "gardenio-gvisor", "crew", "gardenio", "runsc", "warm1", corev1.PodRunning),
		},
	}}

	cfg := gvisorCapConfig()
	cfg.CoopMaxPodsGVisor = 1 // one gVisor pod already running → at budget

	r := New(lister, mgr, cfg, testLogger(), gvisorSpecBuilder("ghcr.io/org/agent:v1"))
	r.SetExtraNamespaces("gardenio-gvisor")
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 0 {
		t.Errorf("expected 0 created (gVisor budget=1 saturated), got %d", len(mgr.created))
	}
}

// TestReconcile_GVisorPodsCountTowardGlobalCap verifies gVisor pods still count
// against the global CoopMaxPods ceiling — the separate budget exempts them
// only from the PER-PROJECT cap, not the global one.
func TestReconcile_GVisorPodsCountTowardGlobalCap(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{
		// Two normal agents that already have running pods (desired, so counted
		// as active toward the global cap), plus one runsc pool bead.
		{ID: "bd-a", Project: "gardenio", Mode: "crew", Role: "crew", AgentName: "a"},
		{ID: "bd-b", Project: "gardenio", Mode: "crew", Role: "crew", AgentName: "b"},
		{ID: "bd-r1", Project: "gardenio", Mode: "crew", Role: "runsc", AgentName: "warm1"},
	}}
	mgr := &mockManager{podsByNS: map[string][]corev1.Pod{
		"gardenio": {
			makePod("crew-gardenio-crew-a", "gardenio", "crew", "gardenio", "crew", "a", corev1.PodRunning),
			makePod("crew-gardenio-crew-b", "gardenio", "crew", "gardenio", "crew", "b", corev1.PodRunning),
		},
		"gardenio-gvisor": nil,
	}}

	cfg := gvisorCapConfig()
	cfg.CoopMaxPods = 2       // global cap already met by the 2 gardenio pods
	cfg.CoopMaxPodsGVisor = 0 // no separate gVisor ceiling

	r := New(lister, mgr, cfg, testLogger(), gvisorSpecBuilder("ghcr.io/org/agent:v1"))
	r.SetExtraNamespaces("gardenio-gvisor")
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 0 {
		t.Errorf("expected 0 created (global cap reached), got %d", len(mgr.created))
	}
}

// TestReconcile_GVisorDeferredMarkedQueued verifies a gVisor pod deferred by its
// budget is marked agent_state=queued (reason max_pods_gvisor) so it is visible
// and — being a pool member in production — counted by the pool next pass.
func TestReconcile_GVisorDeferredMarkedQueued(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{
		{ID: "bd-r1", Project: "gardenio", Mode: "crew", Role: "runsc", AgentName: "warm1"},
		{ID: "bd-r2", Project: "gardenio", Mode: "crew", Role: "runsc", AgentName: "warm2"},
	}}
	mgr := &mockManager{podsByNS: map[string][]corev1.Pod{
		"gardenio": nil,
		"gardenio-gvisor": {
			makePod("crew-gardenio-runsc-warm1", "gardenio-gvisor", "crew", "gardenio", "runsc", "warm1", corev1.PodRunning),
		},
	}}

	cfg := gvisorCapConfig()
	cfg.CoopMaxPodsGVisor = 1

	su := &mockStateUpdater{}
	r := New(lister, mgr, cfg, testLogger(), gvisorSpecBuilder("ghcr.io/org/agent:v1"))
	r.SetExtraNamespaces("gardenio-gvisor")
	r.SetStateUpdater(su)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := ""
	for _, u := range su.updates {
		if u.beadID == "bd-r2" {
			got = u.state
		}
	}
	if got != "queued" {
		t.Errorf("expected bd-r2 marked queued, got %q (all: %v)", got, su.updates)
	}
}
