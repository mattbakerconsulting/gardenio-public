package bridge

import (
	"sort"
	"strings"
)

// This file defines the Slack `--model` override vocabulary for the bridge.
// It is deliberately client-go-free so the bridge package keeps its "zero K8s
// dependencies" property (see gardenio/CLAUDE.md). The controller-side source of
// truth for the routable model set is cmd/controller/litellm_aliases.go
// (claudeCodeModelAliases) and the chart's router_settings.model_group_alias
// (helm/gardenio/templates/litellm/configmap.yaml). TestBridgeAllowlistMatchesAliasMap
// is a drift guard proving the bridge targets are a subset of both.

// shortModelAliases maps operator-friendly short names an operator can type in
// Slack (`--model opus`) to the canonical alias-KEY model id the fleet routes
// on. These KEYS — never the "-oauth" model-GROUP names — are what get baked
// into ANTHROPIC_MODEL for the spawned agent: the LiteLLM router resolves the
// key to its "-oauth" group via model_group_alias, so emitting the key (not the
// group) keeps the bridge decoupled from routing internals.
var shortModelAliases = map[string]string{
	// `fable` is the high-intelligence tier selector; it tracks the current
	// Fable model — 5.1 since ka-0000002744 (claude-fable-5 stays selectable by
	// full id below for rollback and for reproducing older sessions).
	"fable": "claude-fable-5-1",
	"opus":  "claude-opus-5-5",
	// `sonnet` tracks the current Sonnet — 5 since ka-0000004084 (claude-sonnet-4-6
	// stays selectable by full id below).
	"sonnet": "claude-sonnet-5",
	"haiku":  "claude-haiku-4-5",
	// GLM 5 on AWS Bedrock (epic ka-0000002312; Matt decision ka-0000003230).
	// `--model=glm` resolves to the glm-5 alias-KEY, which the LiteLLM router
	// maps to the bedrock-glm-5 model group (chart + prod overlay
	// model_group_alias). Unlike the Anthropic aliases, glm-5 routes through
	// Bedrock via LiteLLM's Anthropic->Bedrock translation, not the Anthropic
	// API. Company policy forbids managed GLM APIs (OpenRouter/Z.ai), so Bedrock
	// is the only policy-compliant GLM path.
	"glm": "glm-5",
}

// fullModelAllowlist is the set of canonical full model ids an operator may
// select directly (e.g. `--model claude-opus-4-7`). Every short alias target
// above is also a member. Membership is intentionally a subset of both the
// controller alias map keys and the chart model_group_alias keys — the drift
// guard enforces this. Dated snapshot ids (…-20250514) are deliberately
// excluded: operators pick the rolling alias, not a pinned snapshot.
var fullModelAllowlist = map[string]struct{}{
	"claude-fable-5-1":  {},
	"claude-fable-5":    {},
	"claude-opus-5-5":   {},
	"claude-opus-5":     {},
	"claude-opus-4-8":   {},
	"claude-opus-4-7":   {},
	"claude-opus-4-6":   {},
	"claude-sonnet-5":   {},
	"claude-sonnet-4-6": {},
	"claude-haiku-4-5":  {},
	// GLM 5 on Bedrock (epic ka-0000002312) — also the `glm` short-alias target,
	// so a resolved `--model=glm` is independently selectable as `--model=glm-5`.
	"glm-5": {},
}

// normalizeModel resolves a raw operator-typed model token to a canonical
// alias-KEY model id suitable for ANTHROPIC_MODEL. It lowercases and trims the
// input, then:
//
//  1. resolves a short alias (opus → claude-opus-5), else
//  2. passes through a recognized full id (claude-opus-4-7 → claude-opus-4-7), else
//  3. returns ("", false) for anything unrecognized.
//
// The result is always an alias KEY form — never a "-oauth" model-group name.
// The token is NOT otherwise sanitized: it is a model id destined for an env
// value, so no comma→dash munging or similar rewriting is performed. An empty
// or all-whitespace input returns ("", false).
func normalizeModel(raw string) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(raw))
	if m == "" {
		return "", false
	}
	if full, ok := shortModelAliases[m]; ok {
		return full, true
	}
	if _, ok := fullModelAllowlist[m]; ok {
		return m, true
	}
	return "", false
}

// allowedModelHint returns a sorted, comma-separated, backtick-wrapped list of
// the short model aliases an operator may type in a Slack `--model` override.
// It is used to build the "did you mean" hint the bridge posts when an operator
// selects an unrecognized model. Only the short aliases are surfaced (opus,
// sonnet, …) — the full-id allowlist is an advanced escape hatch and would only
// clutter the hint. Sorted for a stable, testable ordering.
func allowedModelHint() string {
	names := make([]string, 0, len(shortModelAliases))
	for name := range shortModelAliases {
		names = append(names, "`"+name+"`")
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// NormalizeModel is normalizeModel for callers outside the bridge: beads-viewer's
// quick-spawn (the in-garden bead panel's "Put an agent on this", Lands WB-12)
// takes the same model vocabulary as a Slack `--model` override.
func NormalizeModel(raw string) (string, bool) { return normalizeModel(raw) }

// ModelAliasNames returns the short model aliases (fable, opus, …), sorted: the
// choices a model picker offers. The full-id allowlist stays an escape hatch.
func ModelAliasNames() []string {
	names := make([]string, 0, len(shortModelAliases))
	for name := range shortModelAliases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
