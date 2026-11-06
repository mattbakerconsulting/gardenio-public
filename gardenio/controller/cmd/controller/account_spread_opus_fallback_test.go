package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// TestFallbackModelGroupAccount_FableExhaustedToOpus is the core MR5 behavior
// (decision ka-0000003299 = opus fallback): when a fable role's whole pool is
// fable-exhausted but an opus window has capacity, the fallback returns that
// account and the opus fallback model.
func TestFallbackModelGroupAccount_FableExhaustedToOpus(t *testing.T) {
	// One account: fable window tapped (util 1.0), opus window fresh (0.1).
	a := acct("sk-a", "active", "", "project:gardenio")
	a.ModelGroupLimits = mgLimits(
		modelGroupLimitEntry{Window: "7d", Group: "fable", Utilization: 1.0},
		modelGroupLimitEntry{Window: "7d", Group: "opus", Utilization: 0.1},
	)
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{"acct-a": a},
		RoleCache:    map[string]config.RoleCacheEntry{"fabler": {Model: "claude-fable-5"}},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "fabler"}

	// Direct spread is empty (fable-gated); the fallback finds the opus-fresh account.
	if got := selectAccount(cfg, "gardenio", spec, nil); got != "" {
		t.Fatalf("precondition: fable spread should be empty, got %q", got)
	}
	acctName, model := fallbackModelGroupAccount(cfg, spec, nil)
	if acctName != "acct-a" {
		t.Errorf("fallback account = %q, want acct-a", acctName)
	}
	if model != fallbackModel {
		t.Errorf("fallback model = %q, want %q", model, fallbackModel)
	}
}

// TestFallbackModelGroupAccount_OpusRequestNoFallback proves an opus (or unmapped)
// request does NOT fall back — opus is the degrade target, so an exhausted opus
// pool is genuine pool-exhaustion handled by the SpawnBlocked escape hatch, not a
// silent re-downgrade.
func TestFallbackModelGroupAccount_OpusRequestNoFallback(t *testing.T) {
	a := acct("sk-a", "active", "", "project:gardenio")
	a.ModelGroupLimits = mgLimits(modelGroupLimitEntry{Window: "7d", Group: "opus", Utilization: 1.0})
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{"acct-a": a},
		RoleCache:    map[string]config.RoleCacheEntry{"opuser": {Model: "claude-opus-4-8"}},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "opuser"}
	if acctName, _ := fallbackModelGroupAccount(cfg, spec, nil); acctName != "" {
		t.Errorf("opus request must not fall back, got account %q", acctName)
	}
}

// TestFallbackModelGroupAccount_NoOpusCapacity proves that when the opus window is
// ALSO exhausted, there is no fallback — defer to the escape hatch.
func TestFallbackModelGroupAccount_NoOpusCapacity(t *testing.T) {
	a := acct("sk-a", "active", "", "project:gardenio")
	a.ModelGroupLimits = mgLimits(
		modelGroupLimitEntry{Window: "7d", Group: "fable", Utilization: 1.0},
		modelGroupLimitEntry{Window: "7d", Group: "opus", Utilization: 1.0},
	)
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{"acct-a": a},
		RoleCache:    map[string]config.RoleCacheEntry{"fabler": {Model: "claude-fable-5"}},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "fabler"}
	if acctName, _ := fallbackModelGroupAccount(cfg, spec, nil); acctName != "" {
		t.Errorf("no opus capacity → no fallback, got account %q", acctName)
	}
}

// TestFallbackModelGroupAccount_PrefersOpusFreshAccount proves the fallback spreads
// across opus capacity: given a fable-role pool where account X is opus-fresh and
// account Y is opus-tapped, the fallback lands on X.
func TestFallbackModelGroupAccount_PrefersOpusFreshAccount(t *testing.T) {
	fableTappedOpusFresh := acct("sk-x", "active", "", "project:gardenio")
	fableTappedOpusFresh.ModelGroupLimits = mgLimits(
		modelGroupLimitEntry{Window: "7d", Group: "fable", Utilization: 1.0},
		modelGroupLimitEntry{Window: "7d", Group: "opus", Utilization: 0.1},
	)
	bothTapped := acct("sk-y", "active", "", "project:gardenio")
	bothTapped.ModelGroupLimits = mgLimits(
		modelGroupLimitEntry{Window: "7d", Group: "fable", Utilization: 1.0},
		modelGroupLimitEntry{Window: "7d", Group: "opus", Utilization: 1.0},
	)
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{"acct-x": fableTappedOpusFresh, "acct-y": bothTapped},
		RoleCache:    map[string]config.RoleCacheEntry{"fabler": {Model: "claude-fable-5"}},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "fabler"}
	acctName, _ := fallbackModelGroupAccount(cfg, spec, nil)
	if acctName != "acct-x" {
		t.Errorf("fallback should land on the opus-fresh account, got %q", acctName)
	}
}

// TestOpusFallback_EndToEnd proves the WHOLE spawn path: a fable role whose pool is
// fable-exhausted (opus fresh) lands on the account AND the pod is downgraded to
// run opus — ANTHROPIC_MODEL/CLAUDE_MODEL/pin all opus, and the cost tag re-tagged.
func TestOpusFallback_EndToEnd(t *testing.T) {
	a := config.AccountCacheEntry{
		Provider:   "anthropic",
		BaseURL:    "http://litellm:4000",
		Credential: "secret:gardenio-oauth",
		Model:      "claude-fable-5", // account defaults to fable
		LiteLLMKey: "sk-key",
		Labels:     []string{"project:gardenio"},
		ModelGroupLimits: mgLimits(
			modelGroupLimitEntry{Window: "7d", Group: "fable", Utilization: 1.0},
			modelGroupLimitEntry{Window: "7d", Group: "opus", Utilization: 0.1},
		),
	}
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{"gardenio": {Account: "acct-a"}},
		RoleCache:    map[string]config.RoleCacheEntry{"fabler": {Model: "claude-fable-5"}},
		AccountCache: map[string]config.AccountCacheEntry{"acct-a": a},
	}
	ev := subscriber.Event{Project: "gardenio", Role: "fabler", AgentName: "f1", Metadata: map[string]string{}}
	spec := buildAgentPodSpec(cfg, ev)

	if spec.Env["ANTHROPIC_MODEL"] != fallbackModel || spec.Env["CLAUDE_MODEL"] != fallbackModel {
		t.Errorf("pod model not downgraded to opus: ANTHROPIC_MODEL=%q CLAUDE_MODEL=%q",
			spec.Env["ANTHROPIC_MODEL"], spec.Env["CLAUDE_MODEL"])
	}
	if spec.Env[EnvModelPinned] != fallbackModel {
		t.Errorf("downgraded model not pinned: %q", spec.Env[EnvModelPinned])
	}
	if spec.ResolvedModel != fallbackModel {
		t.Errorf("ResolvedModel = %q, want %q", spec.ResolvedModel, fallbackModel)
	}
	// Cost attribution follows the downgrade (MR4 retag runs inside applyAgentModelOverride).
	if h := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]; !strings.Contains(h, "model:"+podmanager.SanitizeRole(fallbackModel)) {
		t.Errorf("cost tag not re-tagged to fallback model:\n%s", h)
	}
}

// TestOpusFallback_NoDowngradeOnHealthyPool proves the common path is untouched: a
// fable role with a fable-fresh account runs fable, no downgrade.
func TestOpusFallback_NoDowngradeOnHealthyPool(t *testing.T) {
	a := config.AccountCacheEntry{
		Provider:         "anthropic",
		BaseURL:          "http://litellm:4000",
		Credential:       "secret:gardenio-oauth",
		Model:            "claude-fable-5",
		LiteLLMKey:       "sk-key",
		Labels:           []string{"project:gardenio"},
		ModelGroupLimits: mgLimits(modelGroupLimitEntry{Window: "7d", Group: "fable", Utilization: 0.2}),
	}
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{"gardenio": {Account: "acct-a"}},
		RoleCache:    map[string]config.RoleCacheEntry{"fabler": {Model: "claude-fable-5"}},
		AccountCache: map[string]config.AccountCacheEntry{"acct-a": a},
	}
	ev := subscriber.Event{Project: "gardenio", Role: "fabler", AgentName: "f1", Metadata: map[string]string{}}
	spec := buildAgentPodSpec(cfg, ev)
	if spec.Env["CLAUDE_MODEL"] != "claude-fable-5" {
		t.Errorf("healthy fable pool must run fable, got %q", spec.Env["CLAUDE_MODEL"])
	}
	if spec.ModelDowngrade != "" {
		t.Errorf("no downgrade expected on healthy pool, got %q", spec.ModelDowngrade)
	}
}
