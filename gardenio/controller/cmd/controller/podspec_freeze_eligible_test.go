package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// Freeze-eligibility signal (fast-hot-resume epic ka-0000003080, task
// ka-0000005754): the controller injects BOAT_FREEZE_ELIGIBLE=true into the pod
// env at spawn iff freeze/thaw is enabled AND the spec resolved to the runsc
// RuntimeClass — mirroring its own shouldFreeze gate. `gb stop` reads it to
// decide SUSPEND (agent_state=stopping → freeze) vs terminal done.

// freezeEligibleCfg builds a config whose `gvisor-poc` role pins
// runtime_class=runsc, with freeze/thaw toggled per the argument.
func freezeEligibleCfg(freezeThaw bool) *config.Config {
	return &config.Config{
		Namespace:         "gardenio",
		FreezeThawEnabled: freezeThaw,
		ProjectCache:      map[string]config.ProjectCacheEntry{},
		RoleCache: map[string]config.RoleCacheEntry{
			"gvisor-poc":    {RuntimeClassName: podmanager.RuntimeClassRunsc},
			"plan-gardener": {RuntimeClassName: podmanager.RuntimeClassRunsc},
		},
	}
}

func runscEvent() subscriber.Event {
	return subscriber.Event{
		Project:   "gardenio",
		Role:      "gvisor-poc",
		AgentName: "canary",
		Metadata:  map[string]string{},
	}
}

func runcEvent() subscriber.Event {
	return subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "worker",
		Metadata:  map[string]string{},
	}
}

func TestBuildAgentPodSpec_FreezeEligible_RunscAndEnabled(t *testing.T) {
	cfg := freezeEligibleCfg(true)
	spec := buildAgentPodSpec(cfg, runscEvent())

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected RuntimeClassName=runsc, got %q", spec.RuntimeClassName)
	}
	if got := spec.Env[EnvFreezeEligible]; got != "true" {
		t.Errorf("%s = %q, want \"true\" for runsc + freeze/thaw enabled", EnvFreezeEligible, got)
	}
}

func TestBuildAgentPodSpec_FreezeEligible_RunscButDisabled(t *testing.T) {
	// runsc resolved, but freeze/thaw is off — NOT eligible.
	cfg := freezeEligibleCfg(false)
	spec := buildAgentPodSpec(cfg, runscEvent())

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected RuntimeClassName=runsc, got %q", spec.RuntimeClassName)
	}
	if got, ok := spec.Env[EnvFreezeEligible]; ok {
		t.Errorf("%s set to %q, want UNSET when freeze/thaw disabled", EnvFreezeEligible, got)
	}
}

func TestBuildAgentPodSpec_FreezeEligible_RuncStaysIneligible(t *testing.T) {
	// freeze/thaw enabled but the agent is runc (no runsc role) — NOT eligible.
	cfg := freezeEligibleCfg(true)
	spec := buildAgentPodSpec(cfg, runcEvent())

	if spec.RuntimeClassName == podmanager.RuntimeClassRunsc {
		t.Fatalf("crew role should not resolve to runsc")
	}
	if got, ok := spec.Env[EnvFreezeEligible]; ok {
		t.Errorf("%s set to %q, want UNSET for a runc agent", EnvFreezeEligible, got)
	}
}

// The reconciler path (BuildSpecFromBeadInfo) must inject the same signal so a
// runsc agent recreated/reconciled out-of-band still suspends on gb stop.
func TestBuildSpecFromBeadInfo_FreezeEligible_RunscAndEnabled(t *testing.T) {
	cfg := freezeEligibleCfg(true)
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "", "gvisor-poc", "canary", map[string]string{})

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected RuntimeClassName=runsc, got %q", spec.RuntimeClassName)
	}
	if got := spec.Env[EnvFreezeEligible]; got != "true" {
		t.Errorf("%s = %q, want \"true\" (reconciler path)", EnvFreezeEligible, got)
	}
}

func TestBuildSpecFromBeadInfo_FreezeEligible_RuncStaysIneligible(t *testing.T) {
	cfg := freezeEligibleCfg(true)
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "", "crew", "worker", map[string]string{})

	if got, ok := spec.Env[EnvFreezeEligible]; ok {
		t.Errorf("%s set to %q, want UNSET for a runc agent (reconciler path)", EnvFreezeEligible, got)
	}
}

// WS3 canary role allowlist (task ka-0000000138): when FREEZE_THAW_ROLE_ALLOWLIST
// is non-empty, a runsc agent is freeze-eligible ONLY if its role is listed —
// the per-role dial that lets the operator widen freeze/thaw one role at a time
// on a substrate where FreezeThawEnabled is already globally on. An empty
// allowlist preserves the pre-WS3 behaviour (every runsc role eligible).

func TestBuildAgentPodSpec_FreezeEligible_RoleInAllowlist(t *testing.T) {
	cfg := freezeEligibleCfg(true)
	cfg.FreezeThawRoleAllowlist = []string{"gvisor-poc", "plan-gardener"}

	// plan-gardener is runsc + allow-listed → eligible.
	spec := buildAgentPodSpec(cfg, subscriber.Event{
		Project: "gardenio", Role: "plan-gardener", AgentName: "cap", Metadata: map[string]string{},
	})
	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected plan-gardener to resolve to runsc, got %q", spec.RuntimeClassName)
	}
	if got := spec.Env[EnvFreezeEligible]; got != "true" {
		t.Errorf("%s = %q, want \"true\" for an allow-listed runsc role", EnvFreezeEligible, got)
	}
}

func TestBuildAgentPodSpec_FreezeEligible_RoleNotInAllowlist(t *testing.T) {
	cfg := freezeEligibleCfg(true)
	// Only gvisor-poc is allow-listed; plan-gardener is runsc but NOT listed.
	cfg.FreezeThawRoleAllowlist = []string{"gvisor-poc"}

	spec := buildAgentPodSpec(cfg, subscriber.Event{
		Project: "gardenio", Role: "plan-gardener", AgentName: "cap", Metadata: map[string]string{},
	})
	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Fatalf("expected plan-gardener to resolve to runsc, got %q", spec.RuntimeClassName)
	}
	if got, ok := spec.Env[EnvFreezeEligible]; ok {
		t.Errorf("%s set to %q, want UNSET for a runsc role absent from the allowlist", EnvFreezeEligible, got)
	}
}

func TestBuildAgentPodSpec_FreezeEligible_EmptyAllowlistUnchanged(t *testing.T) {
	cfg := freezeEligibleCfg(true) // FreezeThawRoleAllowlist nil (default)

	// gvisor-poc still eligible with an empty allowlist (pre-WS3 behaviour).
	spec := buildAgentPodSpec(cfg, runscEvent())
	if got := spec.Env[EnvFreezeEligible]; got != "true" {
		t.Errorf("%s = %q, want \"true\" with an empty allowlist (no role filter)", EnvFreezeEligible, got)
	}
}
