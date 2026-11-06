package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// R4-DET-4: explicit per-account model pin (task ka-0000004991, epic
// ka-0000000278 — Phase 0 claudemore guardrails).
//
// The account bead's `fields.model` flows AccountInfo.Model → AccountCacheEntry.Model
// → applyAccountConfig → spec.Env["CLAUDE_MODEL"]/["ANTHROPIC_MODEL"]. When it is
// LEFT EMPTY the pod inherits the controller-global cfg.ClaudeModel — a default
// Anthropic can change silently under us. R4-DET-4 says that silent default is
// NOT acceptable for the claudemore e2e account: the pin is load-bearing and must
// be an EXPLICIT, deterministic value.
//
// These tests are the regression lock for that guarantee. They pin the two ends
// of the determinism contract at the pod-env level (both spawn paths):
//
//   - a PINNED account (fields.model set) → the pod runs EXACTLY that model, and
//     nothing (global default, alias pins) overwrites CLAUDE_MODEL/ANTHROPIC_MODEL;
//   - a DEFAULT account (fields.model empty) → the pod falls back to the
//     controller-global cfg.ClaudeModel, and to that ONLY — never a bare/unset
//     model that would let the harness pick an Anthropic-chosen default.
//
// The existing podspec_model_override_test.go covers the project/role/agent
// OVERRIDE precedence on top of the account default; this file locks the account
// tier itself — the base R4-DET-4 requires — including the "empty → global, never
// silent default" invariant the override tests do not assert directly.

// e2eAccountCfg builds a config whose single project resolves to one account.
// accountModel is the account bead's fields.model (empty = default account);
// globalModel is the controller-global cfg.ClaudeModel fallback. No project/role
// model override is set, so the account tier is what decides the pod model.
func e2eAccountCfg(accountModel, globalModel string) *config.Config {
	return &config.Config{
		ClaudeModel: globalModel,
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {Account: "e2e-acct"},
		},
		AccountCache: map[string]config.AccountCacheEntry{
			"e2e-acct": {
				Provider:   "anthropic",
				BaseURL:    "http://litellm:4000",
				Credential: "secret:e2e-oauth",
				Model:      accountModel,
				LiteLLMKey: "sk-e2e",
				Labels:     []string{"project:gardenio"},
			},
		},
	}
}

func e2eEvent() subscriber.Event {
	return subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "e2e-agent",
		Metadata:  map[string]string{},
	}
}

// assertEnvModel asserts BOTH ANTHROPIC_MODEL and CLAUDE_MODEL equal want. Claude
// Code reads CLAUDE_MODEL as the primary session model; ANTHROPIC_MODEL must agree
// so no downstream (coop reload, alias resolver) has a second source of truth.
func assertEnvModel(t *testing.T, spec podmanager.AgentPodSpec, want string) {
	t.Helper()
	if got := spec.Env["CLAUDE_MODEL"]; got != want {
		t.Errorf("CLAUDE_MODEL: got %q, want %q", got, want)
	}
	if got := spec.Env["ANTHROPIC_MODEL"]; got != want {
		t.Errorf("ANTHROPIC_MODEL: got %q, want %q", got, want)
	}
}

// TestAccountModelPin_Pinned_UsesSpecifiedModel: an account bead that pins
// fields.model makes every pod on it run EXACTLY that model — the load-bearing
// R4-DET-4 guarantee. Verified on both spawn paths (event → buildAgentPodSpec and
// reconciler/restart → BuildSpecFromBeadInfo) so a restart never re-derives a
// different model.
func TestAccountModelPin_Pinned_UsesSpecifiedModel(t *testing.T) {
	const pinned = "claude-opus-4-8-oauth"
	// A global default DIFFERENT from the pin, so a passing test proves the account
	// pin — not the global — is what the pod ran.
	cfg := e2eAccountCfg(pinned, "claude-sonnet-5")

	spec := buildAgentPodSpec(cfg, e2eEvent())
	assertEnvModel(t, spec, pinned)

	// Reconciler / restart path must resolve identically.
	spec = BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "e2e-agent", map[string]string{})
	assertEnvModel(t, spec, pinned)
}

// TestAccountModelPin_Default_FallsBackToGlobal: an account bead with NO
// fields.model (the default account) falls back to the controller-global
// cfg.ClaudeModel — and to that explicit value ONLY. This is the negative half of
// R4-DET-4: the fallback is a controller-owned deterministic model, never a
// bare/unset CLAUDE_MODEL that would let the harness resolve an Anthropic-chosen
// (silently-changeable) default.
//
// NOTE the asymmetry this pins deliberately: on the global-default path
// applyCommonConfig sets ONLY CLAUDE_MODEL (the primary Claude Code reads);
// ANTHROPIC_MODEL is set to a matching value ONLY when an account/override pins a
// model (podspec.go: `if acct.Model != ""`). So an unpinned account is second-class
// — its ANTHROPIC_MODEL stays unset. That is exactly why the claudemore e2e account
// PINS fields.model (TestAccountModelPin_Pinned_UsesSpecifiedModel) rather than
// leaning on the global: only a pin makes BOTH env vars deterministic.
func TestAccountModelPin_Default_FallsBackToGlobal(t *testing.T) {
	const global = "claude-opus-4-8-oauth"
	cfg := e2eAccountCfg("", global)

	// The deterministic contract for an unpinned account: CLAUDE_MODEL (the
	// primary session model) is the controller-global, on both spawn paths.
	spec := buildAgentPodSpec(cfg, e2eEvent())
	if got := spec.Env["CLAUDE_MODEL"]; got != global {
		t.Errorf("CLAUDE_MODEL: got %q, want %q (global fallback)", got, global)
	}
	// ANTHROPIC_MODEL is NOT force-set on this path — pinning it is what fields.model
	// (or a role/project/agent override) buys. Lock that so a future change which
	// starts stamping it here is a conscious decision, not an accident.
	if got := spec.Env["ANTHROPIC_MODEL"]; got != "" {
		t.Errorf("ANTHROPIC_MODEL: got %q, want unset on the unpinned global-fallback path", got)
	}

	spec = BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "e2e-agent", map[string]string{})
	if got := spec.Env["CLAUDE_MODEL"]; got != global {
		t.Errorf("CLAUDE_MODEL (reconciler path): got %q, want %q (global fallback)", got, global)
	}
}

// TestAccountModelPin_Pinned_BeatsGlobalDefault: the account pin must OVERWRITE
// the controller-global default that applyCommonConfig stamps first. applyCommonConfig
// sets CLAUDE_MODEL = cfg.ClaudeModel, THEN applyAccountConfig overwrites it with
// the account model — so an account pinning opus on a fable-default fleet runs
// opus, not the global fable. This is the exact ordering R4-DET-4 depends on:
// the pin is authoritative over the mutable global default.
func TestAccountModelPin_Pinned_BeatsGlobalDefault(t *testing.T) {
	const pinned = "claude-opus-4-8-oauth"
	cfg := e2eAccountCfg(pinned, "claude-fable-5-oauth")

	spec := buildAgentPodSpec(cfg, e2eEvent())
	assertEnvModel(t, spec, pinned)
	// No override in play, so no BOAT_MODEL_PINNED pin is stamped — the account
	// default is freely re-derivable on rotation (only role/project/agent
	// OVERRIDES pin against a coopmux reload; see podspec_model_override_test.go).
	if pin, ok := spec.Env[EnvModelPinned]; ok {
		t.Errorf("account default must not stamp BOAT_MODEL_PINNED; got %q", pin)
	}
}

// TestAccountModelPin_Default_NoGlobal_LeavesModelUnset documents the boundary
// case R4-DET-4 exists to PREVENT: an account with no fields.model AND no
// controller-global cfg.ClaudeModel leaves CLAUDE_MODEL unset, so the harness
// falls through to whatever default Anthropic ships. This is precisely the
// silently-changeable default the claudemore e2e account must never rely on —
// hence the e2e harness pins fields.model explicitly rather than depending on a
// global being present. The assertion pins the (undesirable-but-real) behavior so
// a future change that starts injecting a hidden default here is caught.
func TestAccountModelPin_Default_NoGlobal_LeavesModelUnset(t *testing.T) {
	cfg := e2eAccountCfg("", "")

	spec := buildAgentPodSpec(cfg, e2eEvent())
	if got, ok := spec.Env["CLAUDE_MODEL"]; ok && got != "" {
		t.Errorf("CLAUDE_MODEL: expected unset with no account/global model, got %q", got)
	}
	if got, ok := spec.Env["ANTHROPIC_MODEL"]; ok && got != "" {
		t.Errorf("ANTHROPIC_MODEL: expected unset with no account/global model, got %q", got)
	}
}
