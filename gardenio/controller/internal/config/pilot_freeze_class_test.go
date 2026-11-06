package config

import "testing"

// gVisor freeze/thaw production pilot — single opt-in class selector
// (PilotFreezeClass, epic ka-0000006277 step 5, task ka-0000006164).
// IsPilotFreezeClass is the pure name check; FreezeRoleAllowed must always permit
// the pilot class so the one flag is self-contained.

func TestIsPilotFreezeClass_Matching(t *testing.T) {
	c := &Config{PilotFreezeClass: "plan-gardener"}
	if !c.IsPilotFreezeClass("plan-gardener") {
		t.Error("plan-gardener should match the pilot class")
	}
	if c.IsPilotFreezeClass("crew") {
		t.Error("crew should NOT match the pilot class")
	}
	if c.IsPilotFreezeClass("") {
		t.Error("empty role should never match")
	}
}

func TestIsPilotFreezeClass_EmptyFlagNeverMatches(t *testing.T) {
	c := &Config{PilotFreezeClass: ""}
	for _, role := range []string{"plan-gardener", "crew", "thread", ""} {
		if c.IsPilotFreezeClass(role) {
			t.Errorf("empty PilotFreezeClass should never match role %q (pilot off)", role)
		}
	}
}

// Matching tolerates the comma→dash label sanitization, mirroring
// FreezeRoleAllowed, so a class authored either way matches at both seams.
func TestIsPilotFreezeClass_CommaDashNormalized(t *testing.T) {
	c := &Config{PilotFreezeClass: "crew,thread"}
	if !c.IsPilotFreezeClass("crew,thread") {
		t.Error("raw comma role should match")
	}
	if !c.IsPilotFreezeClass("crew-thread") {
		t.Error("label-sanitized dash role should match the comma-authored pilot class")
	}
}

// The pilot class is freeze-allowed even under a non-empty allowlist that
// excludes it — the single flag does not require also editing the allowlist.
func TestFreezeRoleAllowed_PilotClassAllowedDespiteNonEmptyAllowlist(t *testing.T) {
	c := &Config{
		PilotFreezeClass:        "plan-gardener",
		FreezeThawRoleAllowlist: []string{"some-other-role"},
	}
	if !c.FreezeRoleAllowed("plan-gardener") {
		t.Error("pilot class must be freeze-allowed even when the allowlist excludes it")
	}
	// The pilot allow does not widen beyond the one named class.
	if c.FreezeRoleAllowed("crew") {
		t.Error("a non-pilot role absent from the allowlist must stay NOT allowed")
	}
}
