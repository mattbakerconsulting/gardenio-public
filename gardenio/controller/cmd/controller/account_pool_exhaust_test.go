package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// account_pool_exhaust_test.go — tests for the pool-exhaustion escape hatch and
// account-cache validators added for ka-0000002560 (no escape hatch when an
// entire project account pool exhausts simultaneously).

const litellmURLTest = "https://litellm.app.devops.acme.example"

// poolAcct is a terse builder for an exhaustion-test AccountCacheEntry: a
// LiteLLM-proxied account (base_url + key) with the given status / throttle /
// labels. Mirrors the `acct` helper but always sets a base_url + key so the
// account is otherwise spawnable (so the ONLY reason it is blocked is capacity).
func poolAcct(status, throttle string, labels ...string) config.AccountCacheEntry {
	return config.AccountCacheEntry{
		Provider:       "anthropic",
		BaseURL:        litellmURLTest,
		LiteLLMKey:     "sk-" + status,
		Status:         status,
		ThrottleStatus: throttle,
		Labels:         labels,
	}
}

// TestApplyAccountConfig_PoolFullyExhausted_BlocksSpawn is the core regression
// guard for ka-0000002560: when every account a project resolves to is
// capacity-exhausted (and DEFAULT_ACCOUNT is unset, as on prod during the
// 2026-06-29 incident), applyAccountConfig must REFUSE to bake the known-dead
// key — it sets SpawnBlocked and injects no ANTHROPIC_* env — instead of
// returning the exhausted primary and spawning a doomed pod.
func TestApplyAccountConfig_PoolFullyExhausted_BlocksSpawn(t *testing.T) {
	// Mirror the monorepo pool: gardenio2 -> gardenio3 -> matthewbaker-acme,
	// all exhausted, carrying throttle_reset_at stamps. The terminal
	// DEFAULT_ACCOUNT fallback was removed in Phase 3 (ka-0000007959), so an
	// all-exhausted pool blocks the spawn.
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"monorepo": {Account: "gardenio2"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio2": func() config.AccountCacheEntry {
				e := poolAcct("exhausted", "rejected", "project:monorepo")
				e.Fallback = "gardenio3"
				e.ThrottleResetAt = "2026-06-29T22:00:00Z"
				return e
			}(),
			"gardenio3": func() config.AccountCacheEntry {
				e := poolAcct("exhausted", "rejected", "project:monorepo")
				e.Fallback = "matthewbaker-acme"
				e.ThrottleResetAt = "2026-07-06T17:00:00Z"
				return e
			}(),
			"matthewbaker-acme": func() config.AccountCacheEntry {
				e := poolAcct("exhausted", "rejected", "project:monorepo")
				e.Fallback = "gardenio2" // circular — the incident topology
				e.ThrottleResetAt = "2026-07-06T18:00:00Z"
				return e
			}(),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "monorepo", Role: "crew", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	if !spec.SpawnBlocked {
		t.Fatalf("expected SpawnBlocked=true when the whole pool is exhausted, got false")
	}
	if spec.SpawnBlockReason == "" {
		t.Error("expected a non-empty SpawnBlockReason")
	}
	// The earliest reset_at across the pool must surface in the message.
	if want := "2026-06-29T22:00:00Z"; !strings.Contains(spec.SpawnBlockReason, want) {
		t.Errorf("SpawnBlockReason should name the earliest reset %q, got %q", want, spec.SpawnBlockReason)
	}
	// No credential env must be baked onto a dead key.
	if _, ok := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]; ok {
		t.Error("must NOT bake ANTHROPIC_CUSTOM_HEADERS when the pool is fully exhausted")
	}
	if spec.ResolvedAccount != "" {
		t.Errorf("must not record a ResolvedAccount for a blocked spawn, got %q", spec.ResolvedAccount)
	}
}

// TestApplyAccountConfig_PoolExhausted_ResetUnknown verifies the message
// degrades to "until unknown" when no exhausted pool account carries a reset.
func TestApplyAccountConfig_PoolExhausted_ResetUnknown(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{"p": {Account: "a"}},
		AccountCache: map[string]config.AccountCacheEntry{
			"a": poolAcct("exhausted", "rejected", "project:p"), // no ThrottleResetAt
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "p", Role: "crew", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)
	if !spec.SpawnBlocked {
		t.Fatalf("expected SpawnBlocked=true")
	}
	if !strings.Contains(spec.SpawnBlockReason, "unknown") {
		t.Errorf("expected reason to say 'unknown' when no reset is known, got %q", spec.SpawnBlockReason)
	}
}

// TestApplyAccountConfig_HealthyFallbackNotBlocked confirms the escape hatch is
// NARROW: when the chain still has a healthy account, the happy-path redirect is
// unchanged and SpawnBlocked stays false. This guards against the new guard
// over-firing on the normal degraded-but-recoverable case.
func TestApplyAccountConfig_HealthyFallbackNotBlocked(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{"monorepo": {Account: "gardenio2"}},
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio2": func() config.AccountCacheEntry {
				e := poolAcct("exhausted", "rejected", "project:monorepo")
				e.Fallback = "gardenio3"
				return e
			}(),
			"gardenio3": poolAcct("active", "", "project:monorepo"), // healthy successor
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "monorepo", Role: "crew", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	if spec.SpawnBlocked {
		t.Fatalf("must NOT block when a healthy fallback exists; reason=%q", spec.SpawnBlockReason)
	}
	if spec.ResolvedAccount != "gardenio3" {
		t.Errorf("expected redirect to healthy gardenio3, got %q", spec.ResolvedAccount)
	}
	if got := liteLLMKeyFromHeader(spec.Env["ANTHROPIC_CUSTOM_HEADERS"]); got != "sk-active" {
		t.Errorf("expected the healthy account's key baked, got %q", got)
	}
}

// Phase 2 (ka-0000004094, account-routing.md §7) removed the DefaultAccount
// fall-through and the per-account fallback-chain redirect from the spawn path.
// The former TestApplyAccountConfig_DefaultAccountRescuesExhaustedPool and
// TestApplyAccountConfig_GardenioDefaultsToMatthewbaker1110 both asserted that a
// healthy cfg.DefaultAccount rescues a project with an empty/exhausted pool —
// behavior that no longer exists. A project with no healthy resolvable account
// (role-pin / spread / opus-fallback all declining) now either spawn-blocks
// (credentialed accounts present but all gated — TestApplyAccountConfig_
// PoolFullyExhausted_BlocksSpawn) or is a benign no-credential Debug (no account
// beads at all). Both deleted tests were retired with the DefaultAccount rescue
// path they covered; the DEFAULT_ACCOUNT config/env/helm removal itself landed
// in Phase 3 (ka-0000007959).

// TestApplyAccountConfig_ExclusiveExhaustedNotBlocked confirms an exhausted
// EXCLUSIVE account (exclusive_to_role) is exempt from the escape hatch: its
// design is to surface the failure on its reserved role, not be blocked here, so
// the prior behavior (resolve-and-fail-visibly) is preserved. The bedrock
// account is keyless-by-design in many configs, so we give it a key to isolate
// the exclusivity behavior from the keyless guard.
func TestApplyAccountConfig_ExclusiveExhaustedNotBlocked(t *testing.T) {
	bedrock := config.AccountCacheEntry{
		Provider: "anthropic", BaseURL: litellmURLTest, LiteLLMKey: "sk-bedrock",
		Status: "exhausted", Model: "bedrock-claude", ExclusiveToRole: "fsm-triage",
	}
	cfg := &config.Config{
		RoleCache:    map[string]config.RoleCacheEntry{"fsm-triage": {Account: "litellm-bedrock"}},
		AccountCache: map[string]config.AccountCacheEntry{"litellm-bedrock": bedrock},
	}
	spec := &podmanager.AgentPodSpec{Project: "monorepo", Role: "fsm-triage", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)

	if spec.SpawnBlocked {
		t.Fatalf("exhausted EXCLUSIVE account must not be pool-blocked (it surfaces failure on its role), reason=%q", spec.SpawnBlockReason)
	}
	if spec.ResolvedAccount != "litellm-bedrock" {
		t.Errorf("fsm-triage must stay on its exclusive account, got %q", spec.ResolvedAccount)
	}
}

// TestApplyAccountConfig_KeylessNotPoolBlocked confirms the escape hatch does
// NOT fire on the keyless-LiteLLM misconfiguration: that account is unfit
// (!isHealthyAuthorized) but NOT capacity-exhausted, so the existing
// proceed-with-warn path owns it and SpawnBlocked stays false. Guards the
// NARROWER-than-!isHealthyAuthorized contract of accountCapacityExhausted.
func TestApplyAccountConfig_KeylessNotPoolBlocked(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{"p": {Account: "keyless"}},
		AccountCache: map[string]config.AccountCacheEntry{
			// base_url set, no key -> needsRedirect but NOT capacity-exhausted.
			"keyless": {Provider: "anthropic", BaseURL: litellmURLTest, LiteLLMKey: "", Status: "active", Labels: []string{"project:p"}},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "p", Role: "crew", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)
	if spec.SpawnBlocked {
		t.Errorf("keyless misconfig must NOT trip the capacity escape hatch (handled by the proceed/redirect path), reason=%q", spec.SpawnBlockReason)
	}
}

// TestApplyAccountConfig_HappyPathUnaffected is a belt-and-suspenders check that
// a fully healthy single account spawns exactly as before (no SpawnBlocked, env
// baked).
func TestApplyAccountConfig_HappyPathUnaffected(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{"p": {Account: "healthy"}},
		AccountCache: map[string]config.AccountCacheEntry{
			"healthy": poolAcct("active", "", "project:p"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "p", Role: "crew", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)
	if spec.SpawnBlocked {
		t.Fatalf("healthy account must not be blocked")
	}
	if spec.ResolvedAccount != "healthy" {
		t.Errorf("expected ResolvedAccount=healthy, got %q", spec.ResolvedAccount)
	}
}

// --- accountCapacityExhausted unit tests ---

func TestAccountCapacityExhausted(t *testing.T) {
	tests := []struct {
		name string
		acct config.AccountCacheEntry
		want bool
	}{
		{"exhausted status", config.AccountCacheEntry{Status: "exhausted"}, true},
		{"active healthy", config.AccountCacheEntry{Status: "active", LiteLLMKey: "k"}, false},
		{"empty status healthy", config.AccountCacheEntry{LiteLLMKey: "k"}, false},
		{"keyless not capacity-exhausted", config.AccountCacheEntry{BaseURL: "u", LiteLLMKey: ""}, false},
		{"benign overage out_of_credits", config.AccountCacheEntry{OverageDisabledReason: "out_of_credits"}, false},
		{"benign overage org_spend_cap_reached", config.AccountCacheEntry{OverageDisabledReason: "org_spend_cap_reached"}, false},
		// Billing-aware overage gate: on a METERED account the overage path IS the
		// billing path, so an unrecognized disabled reason blocks. On a
		// subscription account (our whole fleet, and the empty default) the same
		// reason says nothing about the plan quota and must NOT block — the
		// 2026-08-28 org_level_disabled outage.
		{"blocking overage reason on metered", config.AccountCacheEntry{BillingType: "metered", OverageDisabledReason: "payment_method_required"}, true},
		{"same reason on subscription is benign", config.AccountCacheEntry{OverageDisabledReason: "payment_method_required"}, false},
		{"org_level_disabled on subscription is benign", config.AccountCacheEntry{OverageDisabledReason: "org_level_disabled"}, false},
		{"over budget", config.AccountCacheEntry{MaxBudget: "10", TotalCostUSD: "12", SpendAtLastReset: "0"}, true},
		{"under budget", config.AccountCacheEntry{MaxBudget: "10", TotalCostUSD: "3", SpendAtLastReset: "0"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountCapacityExhausted(tc.acct); got != tc.want {
				t.Errorf("accountCapacityExhausted(%+v) = %v, want %v", tc.acct, got, tc.want)
			}
		})
	}
}

// --- earliestPoolResetAt unit tests ---

func TestEarliestPoolResetAt(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"a": {Labels: []string{"project:p"}, ThrottleResetAt: "2026-06-29T22:00:00Z"},
			"b": {Labels: []string{"project:p"}, ThrottleResetAt: "2026-06-29T20:00:00Z"}, // earliest
			"c": {Labels: []string{"project:p"}, ThrottleResetAt: ""},                     // no reset
			"d": {Labels: []string{"project:other"}, ThrottleResetAt: "2026-01-01T00:00:00Z"},
		},
	}
	if got := earliestPoolResetAt(cfg, "p"); got != "2026-06-29T20:00:00Z" {
		t.Errorf("earliestPoolResetAt(p) = %q, want 2026-06-29T20:00:00Z", got)
	}
	if got := earliestPoolResetAt(cfg, "none"); got != "" {
		t.Errorf("earliestPoolResetAt(none) = %q, want empty", got)
	}
	if got := earliestPoolResetAt(cfg, ""); got != "" {
		t.Errorf("earliestPoolResetAt(empty project) = %q, want empty", got)
	}
}
