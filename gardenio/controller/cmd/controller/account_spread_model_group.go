package main

import (
	"encoding/json"
	"strings"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// account_spread_model_group.go — fable-aware landing (ka-0000003806, epic
// ka-0000000266, MR3). Operator decision ka-0000003299 chose HARD-GATE: an account
// whose rate-limit window for the SPEC'S model group is exhausted is excluded from
// that spec's spread pool, exactly like a globally-rejected account is today —
// per-group instead of aggregate. This stops a Fable agent landing on an account
// whose Fable weekly window is tapped while its Opus window sits idle, which the
// aggregate 5h/7d gate cannot see (a Fable-exhausted account reads throttle-allowed
// on the aggregate axis and would otherwise be picked as lowest-load, 429-ing every
// Fable agent on turn 1).

// modelGroupLimitEntry mirrors the controller probe's persisted per-group JSON
// (ratelimit_probe.go modelGroupLimit) for READ on the spawn path. Kept local to
// the spread selector so the landing gate has no dependency on the probe's writer
// type. Only the fields the gate needs are decoded.
type modelGroupLimitEntry struct {
	Window      string  `json:"window"`
	Group       string  `json:"group"`
	Utilization float64 `json:"utilization"`
	Status      string  `json:"status"`
}

// modelGroupExhaustedUtil is the per-group utilization at which a group's window is
// treated as exhausted for landing, matching the aggregate exhaustedUtilization
// (1.0): at or above its rolling budget any further use hits a rejection. A
// per-group status of "rejected" is likewise treated as exhausted regardless of
// the numeric utilization.
const modelGroupExhaustedUtil = exhaustedUtilization

// modelToGroup maps a resolved model name to its Anthropic model GROUP — the unit
// Anthropic splits subscription limits by. It is intentionally token-based (a
// substring match on the family name) rather than an exhaustive table so a new
// dated snapshot (claude-opus-4-9-YYYYMMDD) or alias flows through without a code
// change, mirroring the deliberately-generic probe parser. Returns "" when the
// model's group cannot be determined — the caller then applies NO per-group gate
// (fail-open: an unknown model is never stranded by a group it can't be mapped to).
//
// The recognized families track claudeCodeModelAliases (litellm_aliases.go):
// fable, opus, sonnet, haiku. "glm"/bedrock models return "" — they are metered
// Bedrock groups with no Anthropic subscription window, structurally probe-skipped,
// so a per-group subscription gate never applies to them.
func modelToGroup(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}
	switch {
	case strings.Contains(m, "fable"):
		return "fable"
	case strings.Contains(m, "opus"):
		return "opus"
	case strings.Contains(m, "sonnet"):
		return "sonnet"
	case strings.Contains(m, "haiku"):
		return "haiku"
	default:
		return ""
	}
}

// parseModelGroupLimits decodes an account's model_group_limits JSON into a map
// keyed "<window>_<group>". Returns nil on empty/malformed input (older accounts,
// or a corrupt field) so the caller applies no per-group gate — a decode failure
// must never strand an account.
func parseModelGroupLimits(raw string) map[string]modelGroupLimitEntry {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var m map[string]modelGroupLimitEntry
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

// accountModelGroupExhausted reports whether the account's rate-limit window for
// the given model GROUP is exhausted — the hard-gate signal (decision
// ka-0000003299). It is deliberately fail-OPEN: it returns false (do NOT exclude)
// whenever it lacks a confident exhaustion signal, so the gate can only ever
// TIGHTEN landing for a group the probe has explicitly reported as tapped, never
// strand an account on missing/ambiguous data.
//
// Returns false when:
//   - group == "" (model's group is unknown / not subscription-gated — e.g. glm),
//   - the account carries no model_group_limits (older account / probe not run),
//   - no window for this group is present in the split.
//
// Returns true only when a present window for the group is at/over
// modelGroupExhaustedUtil OR carries a per-group status of "rejected". Any window
// of the group being exhausted exhausts the group (a 5h OR 7d wall both block).
func accountModelGroupExhausted(acct config.AccountCacheEntry, group string) bool {
	if group == "" {
		return false
	}
	limits := parseModelGroupLimits(acct.ModelGroupLimits)
	if len(limits) == 0 {
		return false
	}
	for _, e := range limits {
		if !strings.EqualFold(e.Group, group) {
			continue
		}
		if strings.EqualFold(e.Status, "rejected") {
			return true
		}
		if e.Utilization >= modelGroupExhaustedUtil {
			return true
		}
	}
	return false
}

// specModelGroupFor returns the model GROUP a spawning spec will actually use on a
// GIVEN candidate account, so the landing gate can test the right per-group window.
// Precedence mirrors the pod's real model resolution (global < project < role <
// agent), which is exactly resolveModelOverride plus the account default:
//
//   - a role/project model override (resolveModelOverride) pins the group for
//     EVERY candidate — a fable role is fable no matter which account backs it, so
//     every candidate is tested on its fable window;
//   - otherwise the candidate's OWN account default model decides the group — an
//     account defaulting to fable is tested on fable, an opus-default on opus;
//   - the agent-level metadata override (highest precedence) is threaded in via
//     specRequestedModel when the spawn path knows it.
//
// Returns "" when no group can be determined (no override, no account model, no
// global) — the caller then applies no gate for that candidate.
func specModelGroupFor(cfg *config.Config, project, role, specRequestedModel string, acct config.AccountCacheEntry) string {
	// Agent-level metadata override wins (highest precedence).
	if g := modelToGroup(specRequestedModel); g != "" {
		return g
	}
	// Role/project override pins the group across all candidates.
	if g := modelToGroup(resolveModelOverride(cfg, project, role)); g != "" {
		return g
	}
	// Fall back to this candidate account's own default model.
	if g := modelToGroup(acct.Model); g != "" {
		return g
	}
	// Last resort: the controller-global model.
	return modelToGroup(cfg.ClaudeModel)
}

// fallbackModelGroup is the model group the fable-aware landing gate falls back to
// when the requested group's pool is fully exhausted (decision ka-0000003299 =
// opus fallback). Opus is the shared all-models window that every gardenio account
// backs, so it is the safe universal degrade target.
const fallbackModelGroup = "opus"

// fallbackModel is the concrete model the opus fallback pins. It mirrors the fleet
// default (claudeCodeModelAliases / the account default flipped opus-4-8 -> opus-5 in
// ka-0000003576, then opus-5 -> opus-5.5 in plan:opus55-release) so the downgraded
// pod runs the same opus the rest of the fleet does.
const fallbackModel = "claude-opus-5-5"

// fallbackModelGroupAccount implements the opus fallback (decision ka-0000003299):
// when selectAccount returns "" because every account in the SPEC'S model
// group is exhausted, retry the spread as if the agent requested opus, and if an
// opus-capable account has capacity return (account, fallbackModel). The caller
// lands the pod there and forces the model to fallbackModel via applyModelDowngrade.
//
// It returns ("", "") — no fallback, let the caller take the legacy path — when:
//   - the spec's requested group is ALREADY opus (or unknown): opus is the degrade
//     target, so there is nothing to fall back to; an exhausted opus pool is the
//     genuine pool-exhaustion the SpawnBlocked escape hatch handles downstream.
//   - the opus retry also finds nothing (opus pool likewise exhausted / no
//     opus-capable account): same terminal case, defer to the escape hatch.
//
// The retry reuses selectAccount unchanged by cloning the spec with
// RequestedModel forced to fallbackModel — so the per-candidate gate keys on the
// opus window, and every other guard (aggregate throttle, auth, whitelist,
// exclusive, served-model) still applies. No new exclusion logic; it is the exact
// same selector asked a different question.
func fallbackModelGroupAccount(cfg *config.Config, spec *podmanager.AgentPodSpec, liveCounts map[string]int) (account, model string) {
	// Determine the group the spec actually wanted. Use a representative candidate
	// only to resolve the account-default tier of specModelGroupFor; the override
	// tiers (agent/role/project) dominate and are account-independent, and when the
	// group comes solely from an account default the pool is heterogeneous anyway —
	// so a role/agent/project override is what makes a whole-group fallback
	// meaningful. Resolve it from the overrides directly.
	requestedGroup := modelToGroup(spec.RequestedModel)
	if requestedGroup == "" {
		requestedGroup = modelToGroup(resolveModelOverride(cfg, spec.Project, spec.Role))
	}
	// Only fall back for a NON-opus, known group. If the request is opus (or
	// unmapped), there is no lower-tier group to degrade to — defer to the
	// pool-exhaustion escape hatch.
	if requestedGroup == "" || requestedGroup == fallbackModelGroup {
		return "", ""
	}

	// Retry the spread as an opus request: clone the spec with the model forced to
	// opus so specModelGroupFor resolves the opus window for every candidate.
	probe := *spec
	probe.RequestedModel = fallbackModel
	if acct := selectAccount(cfg, spec.Project, &probe, liveCounts); acct != "" {
		return acct, fallbackModel
	}
	return "", ""
}

// applyModelDowngrade forces the pod's model to spec.ModelDowngrade when the
// fable-aware landing gate fell back to opus (decision ka-0000003299). It is
// applied LAST by the spec builders — after applyModelOverride and
// applyAgentModelOverride — so the opus fallback authoritatively overrides the
// role/agent model that requested the exhausted group. A no-op when no downgrade
// was recorded (the common path). Reuses applyAgentModelOverride so the model is
// stamped on ANTHROPIC_MODEL/CLAUDE_MODEL, pinned (BOAT_MODEL_PINNED) against a
// coopmux reload, re-tagged for cost (retagLiteLLMModel), and recorded on
// ResolvedModel — identical treatment to any other authoritative model override.
func applyModelDowngrade(spec *podmanager.AgentPodSpec) {
	if spec.ModelDowngrade == "" {
		return
	}
	applyAgentModelOverride(spec, spec.ModelDowngrade)
}
