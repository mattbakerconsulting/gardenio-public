package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// gVisor freeze/thaw production pilot — opt-in class selection + feature flag
// (epic ka-0000006277 step 5, task ka-0000006164). PILOT_FREEZE_CLASS names the
// ONE agent class routed onto the runsc pool + freeze/thaw lifecycle; every other
// class stays on the default runc path. Clearing the flag reverts the class to
// stateless-restartable cold-start (rollback).

// pilotCfg builds a config with the pilot class set to plan-gardener (the chosen
// pilot class) and freeze/thaw enabled per the argument. No role/project
// RuntimeClass is pre-pinned, so the ONLY thing that can route plan-gardener onto
// runsc here is the pilot flag itself.
func pilotCfg(freezeThaw bool, pilotClass string) *config.Config {
	return &config.Config{
		Namespace:         "gardenio",
		FreezeThawEnabled: freezeThaw,
		PilotFreezeClass:  pilotClass,
		ProjectCache:      map[string]config.ProjectCacheEntry{},
		RoleCache:         map[string]config.RoleCacheEntry{},
	}
}

func pilotEvent(role string) subscriber.Event {
	return subscriber.Event{
		Project:   "gardenio",
		Role:      role,
		AgentName: "pilot-agent",
		Metadata:  map[string]string{},
	}
}

// The selected pilot class, when the flag is set AND freeze/thaw is enabled, is
// force-pinned to runsc and marked freeze-eligible — with no role/project
// RuntimeClass configured at all. This is the single-flag opt-in.
func TestPilotFreezeClass_SelectedClassPinnedAndEligible(t *testing.T) {
	cfg := pilotCfg(true, "plan-gardener")

	for _, build := range []struct {
		name string
		spec podmanager.AgentPodSpec
	}{
		{"events", buildAgentPodSpec(cfg, pilotEvent("plan-gardener"))},
		{"reconciler", BuildSpecFromBeadInfo(cfg, "gardenio", "", "plan-gardener", "pilot-agent", map[string]string{})},
	} {
		if build.spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
			t.Errorf("[%s] RuntimeClassName = %q, want runsc (pilot class pinned)", build.name, build.spec.RuntimeClassName)
		}
		if got := build.spec.Env[EnvFreezeEligible]; got != "true" {
			t.Errorf("[%s] %s = %q, want \"true\" (pilot class freeze-eligible)", build.name, EnvFreezeEligible, got)
		}
	}
}

// Every OTHER class stays on the default runc path — untouched by the pilot flag.
func TestPilotFreezeClass_OtherClassesStayRunc(t *testing.T) {
	cfg := pilotCfg(true, "plan-gardener")
	spec := buildAgentPodSpec(cfg, pilotEvent("crew"))

	if spec.RuntimeClassName == podmanager.RuntimeClassRunsc {
		t.Errorf("non-pilot role crew was pinned to runsc; want default (empty) runc path")
	}
	if got := spec.Env[EnvFreezeEligible]; got == "true" {
		t.Errorf("non-pilot role crew was marked freeze-eligible; want not eligible")
	}
}

// ROLLBACK: clearing PILOT_FREEZE_CLASS reverts the class to the default runc /
// stateless-restartable path even while FreezeThawEnabled stays on.
func TestPilotFreezeClass_EmptyFlagIsRollback(t *testing.T) {
	cfg := pilotCfg(true, "")
	spec := buildAgentPodSpec(cfg, pilotEvent("plan-gardener"))

	if spec.RuntimeClassName == podmanager.RuntimeClassRunsc {
		t.Errorf("pilot class pinned to runsc with empty PILOT_FREEZE_CLASS; want rollback to runc")
	}
	if got := spec.Env[EnvFreezeEligible]; got == "true" {
		t.Errorf("pilot class freeze-eligible with empty PILOT_FREEZE_CLASS; want not eligible")
	}
}

// The pilot pin is gated on FreezeThawEnabled: naming the class but leaving the
// master freeze switch off does NOT route the class onto runsc (second rollback
// lever).
func TestPilotFreezeClass_GatedOnFreezeThawEnabled(t *testing.T) {
	cfg := pilotCfg(false, "plan-gardener")
	spec := buildAgentPodSpec(cfg, pilotEvent("plan-gardener"))

	if spec.RuntimeClassName == podmanager.RuntimeClassRunsc {
		t.Errorf("pilot class pinned to runsc with FreezeThawEnabled=false; want runc")
	}
}

// An explicit per-agent runtime_class override still wins over the pilot pin
// (precedence: project < role < pilot < agent). Here the agent opts OUT of runsc
// even though its role is the pilot class.
func TestPilotFreezeClass_AgentMetadataOverrideWins(t *testing.T) {
	cfg := pilotCfg(true, "plan-gardener")
	ev := pilotEvent("plan-gardener")
	ev.Metadata["runtime_class"] = "runc-alt"
	spec := buildAgentPodSpec(cfg, ev)

	if spec.RuntimeClassName != "runc-alt" {
		t.Errorf("RuntimeClassName = %q, want per-agent override \"runc-alt\" to win over pilot pin", spec.RuntimeClassName)
	}
}

// The pilot flag is self-contained: the selected class is freeze-eligible even
// when a non-empty FreezeThawRoleAllowlist narrows every OTHER role — the
// operator need not also add the class to the allowlist.
func TestPilotFreezeClass_SelfContainedUnderNonEmptyAllowlist(t *testing.T) {
	cfg := pilotCfg(true, "plan-gardener")
	cfg.FreezeThawRoleAllowlist = []string{"some-other-role"}
	spec := buildAgentPodSpec(cfg, pilotEvent("plan-gardener"))

	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		t.Errorf("RuntimeClassName = %q, want runsc (pilot class pinned)", spec.RuntimeClassName)
	}
	if got := spec.Env[EnvFreezeEligible]; got != "true" {
		t.Errorf("%s = %q, want \"true\" — pilot class allowed despite non-empty allowlist excluding it", EnvFreezeEligible, got)
	}
}
