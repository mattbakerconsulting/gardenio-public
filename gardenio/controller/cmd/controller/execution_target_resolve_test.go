package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// baseExecTargetCfg returns a config with empty project/role caches so each test
// populates only what it needs.
func baseExecTargetCfg() *config.Config {
	return &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{},
		RoleCache:    map[string]config.RoleCacheEntry{},
	}
}

func newExecTargetSpec(project, role string) *podmanager.AgentPodSpec {
	return &podmanager.AgentPodSpec{
		Project: project,
		Role:    role,
		BeadID:  "gd-task-1",
		Env:     map[string]string{},
	}
}

// TestResolveExecutionTarget_DefaultKubernetes: no layer sets a target => the
// existing pod path (kubernetes) is selected. This is the can't-strand-existing
// -agents guarantee: every agent that predates the field keeps spawning pods.
func TestResolveExecutionTarget_DefaultKubernetes(t *testing.T) {
	cfg := baseExecTargetCfg()
	spec := newExecTargetSpec("p", "dev")
	got := resolveExecutionTarget(cfg, spec, map[string]string{})
	if got != podmanager.ExecutionTargetKubernetes {
		t.Errorf("default target: got %q, want %q", got, podmanager.ExecutionTargetKubernetes)
	}
	if !podmanager.IsKubernetesTarget(got) {
		t.Errorf("IsKubernetesTarget(%q) = false, want true", got)
	}
}

// TestResolveExecutionTarget_ProjectMicroVM: a project opt-in selects the
// MicroVM backend for all its agents.
func TestResolveExecutionTarget_ProjectMicroVM(t *testing.T) {
	cfg := baseExecTargetCfg()
	cfg.ProjectCache["p"] = config.ProjectCacheEntry{ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM}
	spec := newExecTargetSpec("p", "dev")
	got := resolveExecutionTarget(cfg, spec, map[string]string{})
	if got != podmanager.ExecutionTargetLambdaMicroVM {
		t.Errorf("project target: got %q, want %q", got, podmanager.ExecutionTargetLambdaMicroVM)
	}
	if podmanager.IsKubernetesTarget(got) {
		t.Errorf("IsKubernetesTarget(%q) = true, want false", got)
	}
}

// TestResolveExecutionTarget_RoleOverridesProject: role takes precedence over
// project — the gardenio-microvms role can opt into MicroVM execution regardless
// of which project it runs in.
func TestResolveExecutionTarget_RoleOverridesProject(t *testing.T) {
	cfg := baseExecTargetCfg()
	cfg.ProjectCache["p"] = config.ProjectCacheEntry{ExecutionTarget: podmanager.ExecutionTargetKubernetes}
	cfg.RoleCache["gardenio-microvms"] = config.RoleCacheEntry{ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM}
	spec := newExecTargetSpec("p", "gardenio-microvms")
	got := resolveExecutionTarget(cfg, spec, map[string]string{})
	if got != podmanager.ExecutionTargetLambdaMicroVM {
		t.Errorf("role-over-project: got %q, want %q", got, podmanager.ExecutionTargetLambdaMicroVM)
	}
}

// TestResolveExecutionTarget_AgentOverridesRole: agent metadata is the highest
// precedence layer and can force a specific backend, including forcing back to
// kubernetes when a role opted into MicroVM.
func TestResolveExecutionTarget_AgentOverridesRole(t *testing.T) {
	cfg := baseExecTargetCfg()
	cfg.RoleCache["gardenio-microvms"] = config.RoleCacheEntry{ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM}
	spec := newExecTargetSpec("p", "gardenio-microvms")
	got := resolveExecutionTarget(cfg, spec, map[string]string{"execution_target": podmanager.ExecutionTargetKubernetes})
	if got != podmanager.ExecutionTargetKubernetes {
		t.Errorf("agent-over-role: got %q, want %q", got, podmanager.ExecutionTargetKubernetes)
	}
}

// TestResolveExecutionTarget_FirstMatchingRoleWins: for a multi-role agent, the
// first role that sets a target wins, mirroring resolveZitiAccess' iteration.
func TestResolveExecutionTarget_FirstMatchingRoleWins(t *testing.T) {
	cfg := baseExecTargetCfg()
	cfg.RoleCache["gardenio-microvms"] = config.RoleCacheEntry{ExecutionTarget: podmanager.ExecutionTargetLambdaMicroVM}
	cfg.RoleCache["dev"] = config.RoleCacheEntry{ExecutionTarget: podmanager.ExecutionTargetKubernetes}
	spec := newExecTargetSpec("p", "gardenio-microvms, dev")
	got := resolveExecutionTarget(cfg, spec, map[string]string{})
	if got != podmanager.ExecutionTargetLambdaMicroVM {
		t.Errorf("first-matching-role: got %q, want %q", got, podmanager.ExecutionTargetLambdaMicroVM)
	}
}

// TestResolveExecutionTarget_Probe: the probe target (probe-pods plan
// ka-0000000544) is a recognised value and resolves to itself — NOT the
// kubernetes fallback — so an agent metadata layer that sets execution_target
// =probe reaches the probe-aware fail-closed branch in handleNonK8sSpawn rather
// than silently spawning a coop agent pod.
func TestResolveExecutionTarget_Probe(t *testing.T) {
	cfg := baseExecTargetCfg()
	spec := newExecTargetSpec("p", "dev")
	got := resolveExecutionTarget(cfg, spec, map[string]string{"execution_target": podmanager.ExecutionTargetProbe})
	if got != podmanager.ExecutionTargetProbe {
		t.Errorf("probe target: got %q, want %q", got, podmanager.ExecutionTargetProbe)
	}
	if podmanager.IsKubernetesTarget(got) {
		t.Errorf("IsKubernetesTarget(%q) = true, want false (probe must not reach CreateAgentPod)", got)
	}
	if !podmanager.IsProbeTarget(got) {
		t.Errorf("IsProbeTarget(%q) = false, want true", got)
	}
}

// TestResolveExecutionTarget_UnknownFallsBackToKubernetes: an unrecognized value
// (typo, future backend not yet supported) never strands an agent — it falls
// back to the safe kubernetes pod path.
func TestResolveExecutionTarget_UnknownFallsBackToKubernetes(t *testing.T) {
	cfg := baseExecTargetCfg()
	cfg.ProjectCache["p"] = config.ProjectCacheEntry{ExecutionTarget: "firecracker-typo"}
	spec := newExecTargetSpec("p", "dev")
	got := resolveExecutionTarget(cfg, spec, map[string]string{})
	if got != podmanager.ExecutionTargetKubernetes {
		t.Errorf("unknown target: got %q, want %q", got, podmanager.ExecutionTargetKubernetes)
	}
}
