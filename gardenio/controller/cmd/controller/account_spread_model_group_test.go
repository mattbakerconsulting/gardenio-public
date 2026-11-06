package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// mgLimits builds a model_group_limits JSON field for a test account.
func mgLimits(entries ...modelGroupLimitEntry) string {
	m := make(map[string]modelGroupLimitEntry, len(entries))
	for _, e := range entries {
		m[e.Window+"_"+e.Group] = e
	}
	return encodeModelGroupLimits(toWriterMap(m))
}

// toWriterMap adapts the reader-side entry map to the writer type so tests can
// reuse the controller's own encoder for a realistic on-bead string.
func toWriterMap(m map[string]modelGroupLimitEntry) map[string]modelGroupLimit {
	out := make(map[string]modelGroupLimit, len(m))
	for k, e := range m {
		out[k] = modelGroupLimit{Window: e.Window, Group: e.Group, Utilization: e.Utilization, Status: e.Status}
	}
	return out
}

func TestModelToGroup(t *testing.T) {
	cases := map[string]string{
		"claude-fable-5":          "fable",
		"claude-opus-4-8":         "opus",
		"claude-opus-4-8-oauth":   "opus",
		"claude-sonnet-4-6":       "sonnet",
		"claude-haiku-4-5":        "haiku",
		"glm-5":                   "", // Bedrock, not subscription-gated
		"bedrock-glm-5":           "",
		"":                        "",
		"some-unknown-model-name": "",
	}
	for model, want := range cases {
		if got := modelToGroup(model); got != want {
			t.Errorf("modelToGroup(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestAccountModelGroupExhausted(t *testing.T) {
	// Fable window at 100% util, opus idle.
	a := config.AccountCacheEntry{ModelGroupLimits: mgLimits(
		modelGroupLimitEntry{Window: "7d", Group: "fable", Utilization: 1.0},
		modelGroupLimitEntry{Window: "7d", Group: "opus", Utilization: 0.1},
	)}
	if !accountModelGroupExhausted(a, "fable") {
		t.Error("fable window at 1.0 should be exhausted")
	}
	if accountModelGroupExhausted(a, "opus") {
		t.Error("opus window at 0.1 should NOT be exhausted")
	}
	// Status=rejected exhausts regardless of numeric util.
	b := config.AccountCacheEntry{ModelGroupLimits: mgLimits(
		modelGroupLimitEntry{Window: "5h", Group: "fable", Utilization: 0.2, Status: "rejected"},
	)}
	if !accountModelGroupExhausted(b, "fable") {
		t.Error("fable status=rejected should be exhausted")
	}
	// Fail-open: unknown group, empty field, malformed field.
	if accountModelGroupExhausted(a, "") {
		t.Error("empty group must never exclude")
	}
	if accountModelGroupExhausted(config.AccountCacheEntry{}, "fable") {
		t.Error("no model_group_limits must never exclude")
	}
	if accountModelGroupExhausted(config.AccountCacheEntry{ModelGroupLimits: "not json"}, "fable") {
		t.Error("malformed field must never exclude")
	}
}

// TestSelectSpreadAccount_HardGateFableRole is the core MR3 behavior (decision
// ka-0000003299): a FABLE role must skip an account whose fable window is exhausted
// even though that account is otherwise healthy (throttle-allowed, 0 live pods) and
// would win the least-loaded pick.
func TestSelectSpreadAccount_HardGateFableRole(t *testing.T) {
	fableTapped := acct("sk-tapped", "active", "", "project:gardenio")
	fableTapped.ModelGroupLimits = mgLimits(modelGroupLimitEntry{Window: "7d", Group: "fable", Utilization: 1.0})
	fableFresh := acct("sk-fresh", "active", "", "project:gardenio")
	fableFresh.ModelGroupLimits = mgLimits(modelGroupLimitEntry{Window: "7d", Group: "fable", Utilization: 0.2})

	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-tapped": fableTapped,
			"acct-fresh":  fableFresh,
		},
		// A role that pins the fable model, so the gate keys on the fable window.
		RoleCache: map[string]config.RoleCacheEntry{
			"fabler": {Model: "claude-fable-5"},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "fabler"}

	// Even though acct-tapped has 0 live pods (lowest load), it is fable-exhausted →
	// excluded; the fresh account wins despite carrying load.
	got := selectAccount(cfg, "gardenio", spec, map[string]int{"acct-tapped": 0, "acct-fresh": 5})
	if got != "acct-fresh" {
		t.Errorf("fable role should skip fable-exhausted acct-tapped, got %q", got)
	}

	// When the fable window is the ONLY signal and every account is fable-tapped,
	// the gate excludes all → "" (caller falls through to legacy path / MR5 fallback).
	cfgAllTapped := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{"acct-tapped": fableTapped},
		RoleCache:    cfg.RoleCache,
	}
	if got := selectAccount(cfgAllTapped, "gardenio", spec, nil); got != "" {
		t.Errorf("all-fable-tapped pool should return empty, got %q", got)
	}
}

// TestSelectSpreadAccount_HardGateDoesNotAffectOpus proves the gate is
// group-scoped: an OPUS role is unaffected by a fable-exhausted window on the same
// account (the exact cross-group blindness this epic fixes — but in reverse).
func TestSelectSpreadAccount_HardGateDoesNotAffectOpus(t *testing.T) {
	// One account: fable tapped, opus fresh.
	a := acct("sk-a", "active", "", "project:gardenio")
	a.Model = "claude-opus-4-8" // account default is opus
	a.ModelGroupLimits = mgLimits(
		modelGroupLimitEntry{Window: "7d", Group: "fable", Utilization: 1.0},
		modelGroupLimitEntry{Window: "7d", Group: "opus", Utilization: 0.1},
	)
	cfg := &config.Config{AccountCache: map[string]config.AccountCacheEntry{"acct-a": a}}
	// No role/project override → the account default (opus) decides the group.
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}
	if got := selectAccount(cfg, "gardenio", spec, nil); got != "acct-a" {
		t.Errorf("opus-default spec must NOT be excluded by a fable-exhausted window, got %q", got)
	}
}

// TestSelectSpreadAccount_HardGateFailOpen proves an account with no per-group data
// is never excluded — the aggregate gate still governs it, so older accounts and
// mid-rollout accounts keep landing agents.
func TestSelectSpreadAccount_HardGateFailOpen(t *testing.T) {
	a := acct("sk-a", "active", "", "project:gardenio") // no ModelGroupLimits
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{"acct-a": a},
		RoleCache:    map[string]config.RoleCacheEntry{"fabler": {Model: "claude-fable-5"}},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "fabler"}
	if got := selectAccount(cfg, "gardenio", spec, nil); got != "acct-a" {
		t.Errorf("account without per-group data must not be gated, got %q", got)
	}
}

// TestSelectSpreadAccount_HardGateAgentMetadataModel proves the agent-level model
// request (spec.RequestedModel, highest precedence) drives the gate even with no
// role/project override.
func TestSelectSpreadAccount_HardGateAgentMetadataModel(t *testing.T) {
	a := acct("sk-a", "active", "", "project:gardenio")
	a.Model = "claude-opus-4-8" // default opus, but the agent requests fable
	a.ModelGroupLimits = mgLimits(modelGroupLimitEntry{Window: "7d", Group: "fable", Utilization: 1.0})
	cfg := &config.Config{AccountCache: map[string]config.AccountCacheEntry{"acct-a": a}}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", RequestedModel: "claude-fable-5"}
	if got := selectAccount(cfg, "gardenio", spec, nil); got != "" {
		t.Errorf("agent requesting fable must be gated off the fable-exhausted account, got %q", got)
	}
}
