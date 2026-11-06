package reconciler

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// The reconciler is a second pod-creation door (spot-loss
// recreates, beads created during controller downtime, retries of spawns the
// event door refused). With per-agent GitHub App tokens enabled it must run the
// same git-credential hook as the event door, fail closed on refusal, and
// release the minted credential when the pod create then fails (gd-BHLDnjX1Hw).

func gitCredBead() beadsapi.AgentBead {
	return beadsapi.AgentBead{ID: "bd-gh", Project: "gardenio", Mode: "crew", Role: "crew", AgentName: "ghagent"}
}

// sharedPATSpecBuilder mimics BuildSpecFromBeadInfo for a GitHub project whose
// project bead maps the shared PAT as both the credentials file and env.
func sharedPATSpecBuilder() SpecBuilder {
	base := simpleSpecBuilder("img:v1")
	return func(cfg *config.Config, project, mode, role, agentName string, metadata map[string]string) podmanager.AgentPodSpec {
		spec := base(cfg, project, mode, role, agentName, metadata)
		spec.GitURL = "https://github.com/joyousaversion/gardenio.git"
		spec.GitCredentialsSecret = "gardenio-git-credentials"
		spec.SecretEnv = []podmanager.SecretEnvSource{
			{EnvName: "GIT_TOKEN", SecretName: "gardenio-git-credentials", SecretKey: "token"},
		}
		return spec
	}
}

func TestReconcile_GitCredentialApplier_CreatedPodCarriesPerAgentSecretNotSharedPAT(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{gitCredBead()}}
	mgr := &mockManager{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), sharedPATSpecBuilder())

	var appliedFor []string
	r.SetGitCredentialApplier(GitCredentialApplier{
		Apply: func(_ context.Context, spec *podmanager.AgentPodSpec, beadID string) (bool, string) {
			appliedFor = append(appliedFor, beadID)
			spec.GitCredentialsSecret = "agent-github-token-" + beadID
			spec.SecretEnv = nil
			return false, ""
		},
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 1 {
		t.Fatalf("created = %d, want 1", len(mgr.created))
	}
	got := mgr.created[0]
	if got.GitCredentialsSecret != "agent-github-token-bd-gh" {
		t.Errorf("reconciler-created pod must carry the per-agent Secret, got %q", got.GitCredentialsSecret)
	}
	if got.GitCredentialsSecret == "gardenio-git-credentials" {
		t.Error("reconciler-created pod must never carry the shared project credential")
	}
	for _, se := range got.SecretEnv {
		if se.SecretName == "gardenio-git-credentials" {
			t.Errorf("shared PAT leaked into SecretEnv: %+v", se)
		}
	}
	if len(appliedFor) != 1 || appliedFor[0] != "bd-gh" {
		t.Errorf("hook must be called once with the bead id, got %v", appliedFor)
	}
}

func TestReconcile_GitCredentialApplier_RefusalCreatesNoPodAndDefers(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{gitCredBead()}}
	mgr := &mockManager{}
	updater := &mockStateUpdater{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), sharedPATSpecBuilder())
	r.SetStateUpdater(updater)
	r.SetGitCredentialApplier(GitCredentialApplier{
		Apply: func(context.Context, *podmanager.AgentPodSpec, string) (bool, string) {
			return true, "mint failed"
		},
		Release: func(context.Context, podmanager.AgentPodSpec, string) {
			t.Error("Release must not run when Apply refused (nothing was created)")
		},
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 0 {
		t.Fatalf("a refused credential must create NO pod (no shared-PAT fallback), created %d: %+v", len(mgr.created), mgr.created)
	}
	if len(updater.updates) != 1 || updater.updates[0].beadID != "bd-gh" || updater.updates[0].state != "queued" {
		t.Errorf("refused bead must be marked queued so it is visible and retried, got %+v", updater.updates)
	}
}

func TestReconcile_GitCredentialApplier_ReleasedWhenCreateFails(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{gitCredBead()}}
	mgr := &mockManager{createErr: errors.New("transient api blip")}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), sharedPATSpecBuilder())
	var released []string
	r.SetGitCredentialApplier(GitCredentialApplier{
		Apply: func(_ context.Context, spec *podmanager.AgentPodSpec, beadID string) (bool, string) {
			spec.GitCredentialsSecret = "agent-github-token-" + beadID
			return false, ""
		},
		Release: func(_ context.Context, spec podmanager.AgentPodSpec, beadID string) {
			released = append(released, beadID+"@"+spec.GitCredentialsSecret)
		},
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0] != "bd-gh@agent-github-token-bd-gh" {
		t.Fatalf("minted credential must be released when CreateAgentPod fails, got %v", released)
	}
}

// Beads deferred by a cap must not have a token minted for them: Apply runs
// only for a pod that is actually about to be created.
func TestReconcile_GitCredentialApplier_NotCalledForCapDeferredBead(t *testing.T) {
	// A second desired bead already has a running pod, so the single slot is full.
	lister := &mockLister{beads: []beadsapi.AgentBead{
		{ID: "bd-live", Project: "gardenio", Mode: "crew", Role: "crew", AgentName: "live"},
		gitCredBead(),
	}}
	mgr := &mockManager{pods: []corev1.Pod{
		makePod("crew-gardenio-crew-live", "gardenio", "crew", "gardenio", "crew", "live", corev1.PodRunning),
	}}
	cfg := testConfig("gardenio")
	cfg.CoopMaxPods = 1
	r := New(lister, mgr, cfg, testLogger(), sharedPATSpecBuilder())
	r.SetGitCredentialApplier(GitCredentialApplier{
		Apply: func(context.Context, *podmanager.AgentPodSpec, string) (bool, string) {
			t.Error("Apply must not run for a bead deferred by the max-pods cap")
			return false, ""
		},
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 0 {
		t.Fatalf("created = %d, want 0 (cap reached)", len(mgr.created))
	}
}

// Zero value (flag off) leaves behavior untouched.
func TestReconcile_GitCredentialApplier_UnsetIsNoop(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{gitCredBead()}}
	mgr := &mockManager{}
	r := New(lister, mgr, testConfig("gardenio"), testLogger(), sharedPATSpecBuilder())
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mgr.created) != 1 || mgr.created[0].GitCredentialsSecret != "gardenio-git-credentials" {
		t.Fatalf("flag-off reconcile must pass the spec through unchanged, got %+v", mgr.created)
	}
}
