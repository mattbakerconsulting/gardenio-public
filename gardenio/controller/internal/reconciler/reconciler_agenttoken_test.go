package reconciler

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// The reconciler is a second pod-creation door. With PER_AGENT_TOKENS on it
// must mount the agent's own beads token on every pod it creates (spot-loss
// recreate, image update, spawn made while the controller was down), fail closed
// when the mint fails, and release a token it minted when the pod is then not
// created. applyCommonConfig sets the SHARED secret on the built spec; the hook
// is what replaces it.

const sharedBeadsTokenSecret = "kd-beads-token"

func tokenBead() beadsapi.AgentBead {
	return beadsapi.AgentBead{ID: "gd-Tok1", Project: "gardenio", Mode: "crew", Role: "crew", AgentName: "tokagent"}
}

func sharedTokenSpecBuilder() SpecBuilder {
	base := simpleSpecBuilder("img:v1")
	return func(cfgArg *config.Config, project, mode, role, agentName string, metadata map[string]string) podmanager.AgentPodSpec {
		spec := base(cfgArg, project, mode, role, agentName, metadata)
		spec.DaemonTokenSecret = sharedBeadsTokenSecret
		return spec
	}
}

func TestReconcile_AgentTokenApplier_CreatedPodCarriesPerAgentTokenNotShared(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{tokenBead()}}
	mgr := &mockManager{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), sharedTokenSpecBuilder())

	var gotName string
	r.SetAgentTokenApplier(AgentTokenApplier{
		Apply: func(_ context.Context, spec *podmanager.AgentPodSpec, beadID, agentName string) (bool, error) {
			gotName = agentName
			spec.DaemonTokenSecret = "agent-token-" + beadID
			return true, nil
		},
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 1 {
		t.Fatalf("created = %d, want 1", len(mgr.created))
	}
	if got := mgr.created[0].DaemonTokenSecret; got != "agent-token-gd-Tok1" {
		t.Errorf("reconciler-created pod DaemonTokenSecret = %q, want the per-agent Secret", got)
	}
	if mgr.created[0].DaemonTokenSecret == sharedBeadsTokenSecret {
		t.Error("reconciler-created pod must never carry the shared beads token")
	}
	if gotName != "tokagent" {
		t.Errorf("Apply agentName = %q, want tokagent", gotName)
	}
}

func TestReconcile_AgentTokenApplier_MintFailureCreatesNoPodAndDefers(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{tokenBead()}}
	mgr := &mockManager{}
	updater := &mockStateUpdater{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), sharedTokenSpecBuilder())
	r.SetStateUpdater(updater)
	r.SetAgentTokenApplier(AgentTokenApplier{
		Apply: func(context.Context, *podmanager.AgentPodSpec, string, string) (bool, error) {
			return false, errors.New("mint failed")
		},
		Release: func(context.Context, podmanager.AgentPodSpec, string) {
			t.Error("Release must not run when Apply failed (nothing was minted)")
		},
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 0 {
		t.Fatalf("a failed mint must create NO pod (no shared-token fallback), created %d", len(mgr.created))
	}
	if len(updater.updates) != 1 || updater.updates[0].state != "queued" {
		t.Errorf("refused bead must be marked queued so it is retried, got %+v", updater.updates)
	}
}

func TestReconcile_AgentTokenApplier_ReleasedWhenCreateFails(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{tokenBead()}}
	mgr := &mockManager{createErr: errors.New("transient api blip")}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), sharedTokenSpecBuilder())
	var released []string
	r.SetAgentTokenApplier(AgentTokenApplier{
		Apply: func(_ context.Context, spec *podmanager.AgentPodSpec, beadID, _ string) (bool, error) {
			spec.DaemonTokenSecret = "agent-token-" + beadID
			return true, nil
		},
		Release: func(_ context.Context, spec podmanager.AgentPodSpec, beadID string) {
			released = append(released, beadID+"@"+spec.DaemonTokenSecret)
		},
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0] != "gd-Tok1@agent-token-gd-Tok1" {
		t.Fatalf("a minted token must be released when CreateAgentPod fails, got %v", released)
	}
}

// A refreshed (not newly created) Secret may belong to a live pod: Release must
// not run for it.
func TestReconcile_AgentTokenApplier_NotReleasedWhenNotMintedByThisPass(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{tokenBead()}}
	mgr := &mockManager{createErr: errors.New("transient api blip")}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), sharedTokenSpecBuilder())
	r.SetAgentTokenApplier(AgentTokenApplier{
		Apply: func(_ context.Context, spec *podmanager.AgentPodSpec, beadID, _ string) (bool, error) {
			spec.DaemonTokenSecret = "agent-token-" + beadID
			return false, nil
		},
		Release: func(context.Context, podmanager.AgentPodSpec, string) {
			t.Error("Release must not run for a Secret this pass did not create")
		},
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Zero value (flag off) leaves the shared secret in place.
func TestReconcile_AgentTokenApplier_UnsetIsNoop(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{tokenBead()}}
	mgr := &mockManager{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), sharedTokenSpecBuilder())
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 1 || mgr.created[0].DaemonTokenSecret != sharedBeadsTokenSecret {
		t.Fatalf("flag-off reconcile must pass the spec through unchanged, got %+v", mgr.created)
	}
}
