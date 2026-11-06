package reconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// landSpecBuilder places project acme's pods in land-acme, as the land fence
// does, and every other pod in cfg.Namespace.
func landSpecBuilder(cfg *config.Config, project, mode, role, agentName string, _ map[string]string) podmanager.AgentPodSpec {
	ns := cfg.Namespace
	if project == "acme" {
		ns = "land-acme"
	}
	return podmanager.AgentPodSpec{
		Project: project, Mode: mode, Role: role, AgentName: agentName,
		Image: "img:v1", Namespace: ns,
	}
}

// A tenant land's live pod counts toward the fleet-wide CoopMaxPods and is
// not re-created as missing: the reconciler sweeps the land namespaces its
// source names (gd-NDJIOutLsv).
func TestReconcile_LandPodsCountTowardGlobalCap(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{
		{ID: "bd-land", Project: "acme", Mode: "crew", Role: "crew", AgentName: "t1"},
		{ID: "bd-fleet", Project: "gardenio", Mode: "crew", Role: "crew", AgentName: "f1"},
	}}
	mgr := &mockManager{podsByNS: map[string][]corev1.Pod{
		"gardenio": nil,
		"land-acme": {
			makePod("crew-acme-crew-t1", "land-acme", "crew", "acme", "crew", "t1", corev1.PodRunning),
		},
	}}
	cfg := testConfig("gardenio")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 1

	r := New(lister, mgr, cfg, testLogger(), landSpecBuilder)
	r.SetNamespaceSource(func() []string { return []string{"land-acme", "land-gone", ""} })
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgr.created) != 0 {
		t.Errorf("created %d pods, want 0 (the land pod fills CoopMaxPods=1)", len(mgr.created))
	}
	if len(mgr.deleted) != 0 {
		t.Errorf("deleted %v, want none (the land pod's bead is open)", mgr.deleted)
	}
}
