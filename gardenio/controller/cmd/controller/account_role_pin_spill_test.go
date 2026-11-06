package main

import (
	"testing"

	gardeniometrics "gardenio/controller/internal/metrics"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// account_role_pin_spill_test.go — tests for the role-pin exhaustion spill
// (ka-0000001307). A role that PINS a single account (RoleCacheEntry.Account)
// bypasses the spread pool, so when that pinned account is capacity-exhausted at
// spawn AND the project has other healthy accounts, applyAccountConfig must
// SPILL to the healthy spread pool instead of parking the agent queued
// indefinitely (the release-lane stalls ka-0000006248 / ka-0000007145).

// pinnedRoleCfg builds a config where role `pinned-lane` pins account `pinned`
// (exhausted) and the project also has healthy accounts `healthy1`/`healthy2`.
func pinnedRoleCfg() *config.Config {
	return &config.Config{
		LiteLLMURL: litellmURLTest,
		RoleCache: map[string]config.RoleCacheEntry{
			"pinned-lane": {Account: "pinned"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"pinned": func() config.AccountCacheEntry {
				e := poolAcct("exhausted", "rejected", "project:gardenio")
				e.ThrottleResetAt = "2026-08-20T00:00:00Z"
				return e
			}(),
			"healthy1": poolAcct("active", "", "project:gardenio"),
			"healthy2": poolAcct("active", "", "project:gardenio"),
		},
	}
}

// TestApplyAccountConfig_RolePinExhausted_SpillsToHealthyPool is the core
// regression guard: a role-pinned, exhausted account with healthy siblings must
// spill onto a healthy account rather than SpawnBlock.
func TestApplyAccountConfig_RolePinExhausted_SpillsToHealthyPool(t *testing.T) {
	cfg := pinnedRoleCfg()
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "pinned-lane", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	if spec.SpawnBlocked {
		t.Fatalf("must NOT block when the pinned account is exhausted but healthy siblings exist; reason=%q", spec.SpawnBlockReason)
	}
	if spec.ResolvedAccount != "healthy1" && spec.ResolvedAccount != "healthy2" {
		t.Fatalf("expected spill onto a healthy account, got %q", spec.ResolvedAccount)
	}
	if spec.AccountLayer != gardeniometrics.AccountLayerRolePinSpill {
		t.Errorf("expected AccountLayer=%q, got %q", gardeniometrics.AccountLayerRolePinSpill, spec.AccountLayer)
	}
	// The healthy account's key must be baked, not the dead pinned one.
	if got := liteLLMKeyFromHeader(spec.Env["ANTHROPIC_CUSTOM_HEADERS"]); got != "sk-active" {
		t.Errorf("expected a healthy account's key baked, got %q", got)
	}
}

// TestApplyAccountConfig_RolePinExhausted_NoSpillOptOut confirms a role that
// sets no_account_spill=true keeps the old block-and-wait behavior even when
// healthy siblings exist — the deliberate pin-forever lane.
func TestApplyAccountConfig_RolePinExhausted_NoSpillOptOut(t *testing.T) {
	cfg := pinnedRoleCfg()
	cfg.RoleCache["pinned-lane"] = config.RoleCacheEntry{Account: "pinned", NoAccountSpill: true}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "pinned-lane", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	if !spec.SpawnBlocked {
		t.Fatalf("a no_account_spill role must still block when its pinned account is exhausted, got SpawnBlocked=false")
	}
	if spec.AccountLayer != gardeniometrics.AccountLayerEscapeHatchBlocked {
		t.Errorf("expected AccountLayer=%q, got %q", gardeniometrics.AccountLayerEscapeHatchBlocked, spec.AccountLayer)
	}
	if spec.ResolvedAccount != "" {
		t.Errorf("must not record a ResolvedAccount for a blocked spawn, got %q", spec.ResolvedAccount)
	}
}

// TestApplyAccountConfig_RolePinExhausted_NoHealthyPeersStillBlocks confirms the
// spill does not manufacture a healthy account: when the pinned account AND all
// project siblings are exhausted, the spawn still blocks (genuine pool
// exhaustion — the case the escape hatch was designed for).
func TestApplyAccountConfig_RolePinExhausted_NoHealthyPeersStillBlocks(t *testing.T) {
	cfg := pinnedRoleCfg()
	cfg.AccountCache["healthy1"] = poolAcct("exhausted", "rejected", "project:gardenio")
	cfg.AccountCache["healthy2"] = poolAcct("exhausted", "rejected", "project:gardenio")
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "pinned-lane", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	if !spec.SpawnBlocked {
		t.Fatalf("must block when the whole pool (pinned + siblings) is exhausted, got SpawnBlocked=false")
	}
	if spec.AccountLayer != gardeniometrics.AccountLayerEscapeHatchBlocked {
		t.Errorf("expected AccountLayer=%q, got %q", gardeniometrics.AccountLayerEscapeHatchBlocked, spec.AccountLayer)
	}
}

// TestApplyAccountConfig_ExclusiveExhausted_DoesNotSpill confirms an exhausted
// EXCLUSIVE account (exclusive_to_role) never spills: its design is to surface
// the failure on its reserved role. It resolves-and-fails-visibly on its own
// account (not SpawnBlocked, not spilled), exactly as before the spill existed.
func TestApplyAccountConfig_ExclusiveExhausted_DoesNotSpill(t *testing.T) {
	cfg := &config.Config{
		LiteLLMURL: litellmURLTest,
		RoleCache:  map[string]config.RoleCacheEntry{"fsm-triage": {Account: "litellm-bedrock"}},
		AccountCache: map[string]config.AccountCacheEntry{
			"litellm-bedrock": {
				Provider: "anthropic", BaseURL: litellmURLTest, LiteLLMKey: "sk-bedrock",
				Status: "exhausted", Model: "bedrock-claude", ExclusiveToRole: "fsm-triage",
			},
			// A healthy general-pool account the spill would grab if the exclusive
			// exemption were broken.
			"healthy1": poolAcct("active", "", "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "fsm-triage", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	if spec.SpawnBlocked {
		t.Fatalf("exhausted EXCLUSIVE account must not be pool-blocked, reason=%q", spec.SpawnBlockReason)
	}
	if spec.ResolvedAccount != "litellm-bedrock" {
		t.Errorf("fsm-triage must stay on its exclusive account (no spill), got %q", spec.ResolvedAccount)
	}
	if spec.AccountLayer == gardeniometrics.AccountLayerRolePinSpill {
		t.Errorf("exclusive account must never record a role-pin spill")
	}
}

// TestApplyAccountConfig_SpreadExhausted_DoesNotSpill confirms the spill is
// scoped to a ROLE PIN: an ordinary (unpinned) project agent whose spread pick
// lands exhausted is the whole-pool-exhausted case and must still block — the
// spill would just re-run the same selector. Here the ONLY project account is
// exhausted, so the spread pick would be empty and applyAccountConfig blocks via
// the no-credential-warn path; the point is that layer is never role-pin-spill.
func TestApplyAccountConfig_SpreadExhausted_DoesNotSpill(t *testing.T) {
	cfg := &config.Config{
		LiteLLMURL: litellmURLTest,
		AccountCache: map[string]config.AccountCacheEntry{
			"only": poolAcct("exhausted", "rejected", "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	if !spec.SpawnBlocked {
		t.Fatalf("an unpinned agent with an all-exhausted pool must block, got SpawnBlocked=false")
	}
	if spec.AccountLayer == gardeniometrics.AccountLayerRolePinSpill {
		t.Errorf("an unpinned spread agent must never record a role-pin spill")
	}
}

// TestApplyAccountConfig_RolePinHealthy_NoSpill confirms the happy path is
// unchanged: when the pinned account is healthy, the agent stays on it and the
// layer is the ordinary role-pin (never a spill).
func TestApplyAccountConfig_RolePinHealthy_NoSpill(t *testing.T) {
	cfg := pinnedRoleCfg()
	cfg.AccountCache["pinned"] = poolAcct("active", "", "project:gardenio")
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "pinned-lane", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	if spec.SpawnBlocked {
		t.Fatalf("healthy pinned account must not block, reason=%q", spec.SpawnBlockReason)
	}
	if spec.ResolvedAccount != "pinned" {
		t.Errorf("expected to stay on the healthy pinned account, got %q", spec.ResolvedAccount)
	}
	if spec.AccountLayer != gardeniometrics.AccountLayerRolePin {
		t.Errorf("expected AccountLayer=%q, got %q", gardeniometrics.AccountLayerRolePin, spec.AccountLayer)
	}
}

// TestApplyAccountConfig_RolePinSpill_RespectsWhitelist confirms the spilled
// account still passes the role's account_whitelist: if the only healthy sibling
// is outside the whitelist, the spread selector excludes it and the spawn
// blocks (fail-closed) rather than spilling onto a forbidden account.
func TestApplyAccountConfig_RolePinSpill_RespectsWhitelist(t *testing.T) {
	cfg := pinnedRoleCfg()
	// Whitelist names only the (exhausted) pinned account; the healthy siblings
	// are outside it, so the spill pool is empty → block.
	cfg.RoleCache["pinned-lane"] = config.RoleCacheEntry{
		Account:          "pinned",
		AccountWhitelist: []string{"pinned"},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "pinned-lane", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	if !spec.SpawnBlocked {
		t.Fatalf("must block: the only healthy siblings are outside the role whitelist, so the spill pool is empty")
	}
	if spec.AccountLayer == gardeniometrics.AccountLayerRolePinSpill {
		t.Errorf("must not spill onto a whitelist-forbidden account")
	}
}

// TestRoleAllowsAccountSpill unit-tests the opt-out helper directly.
func TestRoleAllowsAccountSpill(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"spillable":   {Account: "a"},
			"pinned-hard": {Account: "b", NoAccountSpill: true},
		},
	}
	cases := []struct {
		role string
		want bool
	}{
		{"spillable", true},
		{"pinned-hard", false},
		{"unknown-role", true},          // no bead → no opt-out
		{"", true},                      // empty role → default allow
		{"spillable,pinned-hard", false}, // any opting-out role wins (strictest)
		{"spillable,other", true},
	}
	for _, tc := range cases {
		if got := roleAllowsAccountSpill(cfg, tc.role); got != tc.want {
			t.Errorf("roleAllowsAccountSpill(%q) = %v, want %v", tc.role, got, tc.want)
		}
	}
	// Empty RoleCache → always allow.
	if !roleAllowsAccountSpill(&config.Config{}, "anything") {
		t.Error("empty RoleCache must default to spill-allowed")
	}
}
