package config

import "testing"

// WS3 freeze/thaw canary role allowlist (task ka-0000000138, epic
// ka-0000004429). FreezeRoleAllowed is the per-role dial that lets the operator
// widen freeze/thaw one role at a time on a substrate where FreezeThawEnabled is
// already globally on. An EMPTY allowlist must preserve pre-WS3 behaviour (every
// role allowed) so shipping the flag is a zero-blast-radius no-op.

func TestFreezeRoleAllowed_EmptyAllowlistAllowsEveryRole(t *testing.T) {
	c := &Config{} // FreezeThawRoleAllowlist nil
	for _, role := range []string{"gvisor-poc", "plan-gardener", "thread", "crew", ""} {
		if !c.FreezeRoleAllowed(role) {
			t.Errorf("empty allowlist should allow role %q (pre-WS3 behaviour)", role)
		}
	}
}

func TestFreezeRoleAllowed_NonEmptyNarrowsToListedRoles(t *testing.T) {
	c := &Config{FreezeThawRoleAllowlist: []string{"gvisor-poc", "plan-gardener"}}

	if !c.FreezeRoleAllowed("gvisor-poc") {
		t.Error("gvisor-poc is in the allowlist, want allowed")
	}
	if !c.FreezeRoleAllowed("plan-gardener") {
		t.Error("plan-gardener is in the allowlist, want allowed")
	}
	// A runsc role that is NOT listed keeps cold-start (not frozen).
	if c.FreezeRoleAllowed("thread") {
		t.Error("thread is not in the allowlist, want NOT allowed")
	}
	if c.FreezeRoleAllowed("crew") {
		t.Error("crew is not in the allowlist, want NOT allowed")
	}
}

func TestFreezeRoleAllowed_EmptyRoleNeverMatchesNonEmptyAllowlist(t *testing.T) {
	c := &Config{FreezeThawRoleAllowlist: []string{"plan-gardener"}}
	if c.FreezeRoleAllowed("") {
		t.Error("empty role must never match a non-empty allowlist")
	}
}

// The spawn-time seam sees the raw bead role ("crew,thread"); the controller
// shouldFreeze seam reads the SanitizeRole'd pod label ("crew-thread").
// Normalizing both sides (comma→dash) makes one allowlist entry match at both
// seams regardless of which form the operator authored.
func TestFreezeRoleAllowed_CommaDashSanitizationTolerated(t *testing.T) {
	// Allowlist authored with the label (sanitized) form matches the raw role.
	c := &Config{FreezeThawRoleAllowlist: []string{"crew-thread"}}
	if !c.FreezeRoleAllowed("crew,thread") {
		t.Error("raw multi-role crew,thread should match sanitized allowlist entry crew-thread")
	}
	// Allowlist authored with the raw form matches the label (sanitized) role.
	c = &Config{FreezeThawRoleAllowlist: []string{"crew,thread"}}
	if !c.FreezeRoleAllowed("crew-thread") {
		t.Error("sanitized crew-thread should match raw allowlist entry crew,thread")
	}
}

func TestFreezeRoleAllowed_TrimsWhitespaceInAllowlistEntries(t *testing.T) {
	// envCSVOr already trims, but FreezeRoleAllowed must not be fooled by a
	// stray space if the slice is built by hand.
	c := &Config{FreezeThawRoleAllowlist: []string{" plan-gardener "}}
	if !c.FreezeRoleAllowed("plan-gardener") {
		t.Error("whitespace around an allowlist entry should be tolerated")
	}
}

func TestLoad_FreezeThawRoleAllowlist_ParsedFromEnv(t *testing.T) {
	t.Setenv("FREEZE_THAW_ROLE_ALLOWLIST", "gvisor-poc, plan-gardener ,thread")
	cfg := Parse()
	got := cfg.FreezeThawRoleAllowlist
	want := []string{"gvisor-poc", "plan-gardener", "thread"}
	if len(got) != len(want) {
		t.Fatalf("FreezeThawRoleAllowlist = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("FreezeThawRoleAllowlist[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoad_FreezeThawRoleAllowlist_DefaultsEmpty(t *testing.T) {
	t.Setenv("FREEZE_THAW_ROLE_ALLOWLIST", "")
	cfg := Parse()
	if len(cfg.FreezeThawRoleAllowlist) != 0 {
		t.Errorf("unset FREEZE_THAW_ROLE_ALLOWLIST should yield empty slice, got %v", cfg.FreezeThawRoleAllowlist)
	}
}
