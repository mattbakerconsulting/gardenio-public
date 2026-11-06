package bridge

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// This file is the harness-aware half of the Slack `--model` override (ADR 0004
// section 6, gd-0D8aGbRwYp). A thread mention may now say which agent CLI to run
// (`--harness codex`, or the shorthand `--model codex`) and, for Codex, which
// Codex model. The alias table and allowlist in bot_mentions_model.go are
// Claude-only; this file selects between them by harness and REJECTS a mismatch
// (a Claude id on Codex degrades silently — Codex accepts unknown ids — so it is
// never passed through).

// parseHarnessOverride extracts a leading harness override from the mention
// text, returning the raw harness token and the remaining prompt. Same three
// syntaxes and leading-token-only rule as parseModelOverride:
//
//   - "harness:codex fix it"   → ("codex", "fix it")
//   - "--harness codex fix it" → ("codex", "fix it")
//   - "--harness=codex fix it" → ("codex", "fix it")
//
// The token is not validated here (see resolveHarnessModel). Returns
// ("", text) when absent or malformed.
func parseHarnessOverride(text string) (string, string) {
	words := strings.Fields(text)
	if len(words) == 0 {
		return "", text
	}
	if v, ok := strings.CutPrefix(words[0], "harness:"); ok && v != "" {
		return v, strings.TrimSpace(strings.Join(words[1:], " "))
	}
	if v, ok := strings.CutPrefix(words[0], "--harness="); ok && v != "" {
		return v, strings.TrimSpace(strings.Join(words[1:], " "))
	}
	if words[0] == "--harness" && len(words) >= 2 {
		return words[1], strings.TrimSpace(strings.Join(words[2:], " "))
	}
	return "", text
}

// parseModelHarnessFlags consumes leading --model and --harness tokens in
// either order (`--harness codex --model gpt-x` or the reverse), returning both
// raw tokens and the remaining prompt. It is parseModelOverride +
// parseHarnessOverride run until neither matches, so a single flag behaves
// exactly as before.
func parseModelHarnessFlags(text string) (rawModel, rawHarness, rest string) {
	rest = text
	for i := 0; i < 2; i++ {
		if rawModel == "" {
			if m, r := parseModelOverride(rest); m != "" {
				rawModel, rest = m, r
				continue
			}
		}
		if rawHarness == "" {
			if h, r := parseHarnessOverride(rest); h != "" {
				rawHarness, rest = h, r
				continue
			}
		}
		break
	}
	return rawModel, rawHarness, rest
}

// codexModelIDPattern is the shape of an OpenAI/Codex model id an operator may
// type (gpt-5-codex, o3, codex-mini-latest ...). ADR 0004 pins no concrete id —
// the subscription's model list is discovered, not assumed — so this is a
// shape check that stops Claude ids, typos with spaces/punctuation and shell
// junk, not an allowlist.
var codexModelIDPattern = regexp.MustCompile(`^(gpt|codex|o[0-9])[a-z0-9._-]{0,62}$`)

// geminiModelIDPattern is the shape of a Gemini model id (`gemini-…`). Like the
// Codex pattern it is a shape check, not an allowlist: the CLI does no local
// validation of `-m` and the live API's reply to an unknown id is not yet
// measured (ADR 0005 R6/§9), so a stricter allowlist would only guess.
var geminiModelIDPattern = regexp.MustCompile(`^gemini[a-z0-9._-]{0,62}$`)

// harnessModelProfile is one non-Claude harness's slice of the Slack `--model`
// vocabulary (ADR 0005 §5 D: a table, not a third copy of an if-chain). Claude
// keeps its own alias table and allowlist in bot_mentions_model.go.
type harnessModelProfile struct {
	// Label is the harness's display name in operator-facing errors.
	Label string
	// IDPattern is the shape of a model id for this harness.
	IDPattern *regexp.Regexp
	// ExampleID is shown in "pass a model id (e.g. ...)".
	ExampleID string
	// Aliases maps short names to full model ids for this harness. Both are
	// empty today: ADR 0004 pins no Codex id, and ADR 0005 R6 reads Gemini's id
	// list from the binary but cannot say which ids a key may call until the
	// live pass (gd-AkOqbNHAym). Adding `"flash": "gemini-…"` here is the whole
	// change once that lands; the resolver already consults the table.
	Aliases map[string]string
}

// harnessModelProfiles is keyed by the beadsapi harness value. Order of
// foreignHarnessOrder fixes which message wins when a token matches no pattern
// but one other harness would claim it.
var harnessModelProfiles = map[string]harnessModelProfile{
	beadsapi.HarnessCodex:  {Label: "Codex", IDPattern: codexModelIDPattern, ExampleID: "gpt-…"},
	beadsapi.HarnessGemini: {Label: "Gemini", IDPattern: geminiModelIDPattern, ExampleID: "gemini-…"},
}

var foreignHarnessOrder = []string{beadsapi.HarnessCodex, beadsapi.HarnessGemini}

// isHarnessShorthand reports whether `--model <m>` merely names a harness.
func isHarnessShorthand(m string) bool {
	return m == beadsapi.HarnessClaude || harnessModelProfiles[m].IDPattern != nil
}

// foreignHarnessOf returns the non-Claude harness (other than except) whose
// model-id shape m matches, or "".
func foreignHarnessOf(m, except string) string {
	for _, h := range foreignHarnessOrder {
		if h != except && harnessModelProfiles[h].IDPattern.MatchString(m) {
			return h
		}
	}
	return ""
}

// resolveProfileModel validates m for a profile harness: Claude ids and other
// harnesses' ids are refused (a Claude or Codex id on Gemini, or the reverse,
// would not be understood by the CLI and could degrade silently), a short alias
// resolves to its id, and anything else must match the harness's id shape.
func resolveProfileModel(harness, m, rawModel string) (string, string) {
	p := harnessModelProfiles[harness]
	if full, ok := p.Aliases[m]; ok {
		return full, ""
	}
	if isClaudeModelToken(m) {
		return "", fmt.Sprintf("`%s` is a Claude model and cannot run on the %s harness; pass a %s model id (e.g. `%s`) or omit `--model` to use the account default.", m, p.Label, p.Label, p.ExampleID)
	}
	if other := foreignHarnessOf(m, harness); other != "" {
		return "", fmt.Sprintf("`%s` is a %s model and cannot run on the %s harness; pass a %s model id (e.g. `%s`) or omit `--model` to use the account default.", m, harnessModelProfiles[other].Label, p.Label, p.Label, p.ExampleID)
	}
	if !p.IDPattern.MatchString(m) {
		return "", fmt.Sprintf("Unrecognized %s model %q. Pass a %s model id (e.g. `%s`) or omit `--model` to use the account default.", p.Label, strings.TrimSpace(rawModel), p.Label, p.ExampleID)
	}
	return m, ""
}

// harnessSelection is the resolved per-spawn agent-CLI choice. Harness is ""
// when the operator expressed no harness preference (the role's resolved
// harness applies); Model is "" for "use the account default".
type harnessSelection struct {
	Harness string
	Model   string
}

// isClaudeModelToken reports whether the lower-cased token is a Claude-side
// alias or allowlisted full id.
func isClaudeModelToken(m string) bool {
	if _, ok := shortModelAliases[m]; ok {
		return true
	}
	_, ok := fullModelAllowlist[m]
	return ok
}

// resolveHarnessModel resolves the raw --harness and --model tokens against
// each other. It returns a hard error message (operator-facing, no spawn) for:
// an unknown harness, a Claude model on codex, a Codex model on claude (or with
// no harness named), a conflicting `--model codex` + `--harness claude`, and a
// token that is neither. `--model codex` (and `--model claude`) is accepted as
// shorthand for the harness of the same name with no model pin.
//
// With no harness involved it defers to normalizeModel exactly as before, and
// reports ok=false with an empty errMsg for an unrecognized token so the caller
// keeps its existing warn-and-spawn-on-default behaviour.
func resolveHarnessModel(rawHarness, rawModel string) (sel harnessSelection, unrecognizedModel bool, errMsg string) {
	harness, err := beadsapi.ParseHarness(rawHarness)
	if err != nil {
		return sel, false, fmt.Sprintf("Unrecognized harness %q. Valid options: `claude`, `codex`, `gemini`.", strings.TrimSpace(rawHarness))
	}
	m := strings.ToLower(strings.TrimSpace(rawModel))

	// `--model codex` / `--model claude` / `--model gemini` name a harness, not a model.
	if isHarnessShorthand(m) {
		if harness != "" && harness != m {
			return sel, false, fmt.Sprintf("Conflicting overrides: `--harness %s` with `--model %s`. Pick one harness.", harness, m)
		}
		return harnessSelection{Harness: m}, false, ""
	}

	if _, ok := harnessModelProfiles[harness]; ok && m != "" {
		model, msg := resolveProfileModel(harness, m, rawModel)
		if msg != "" {
			return sel, false, msg
		}
		return harnessSelection{Harness: harness, Model: model}, false, ""
	}

	switch {
	case m == "":
		return harnessSelection{Harness: harness}, false, ""
	default: // claude, or no harness named
		if resolved, ok := normalizeModel(m); ok {
			return harnessSelection{Harness: harness, Model: resolved}, false, ""
		}
		if other := foreignHarnessOf(m, ""); other != "" {
			return sel, false, fmt.Sprintf("`%s` looks like a %s model; add `--harness %s` (or `--model %s`) to run it. Claude options: %s.", m, harnessModelProfiles[other].Label, other, other, allowedModelHint())
		}
		return harnessSelection{Harness: harness}, true, ""
	}
}

// roleHarnessLister is the optional daemon surface harnessAllowedForRole needs.
// *beadsapi.Client satisfies it; fakes that do not are treated as "role catalog
// unavailable" and fail open to the controller's own spawn-time validation.
type roleHarnessLister interface {
	ListRoleBeads(ctx context.Context) (map[string]beadsapi.RoleInfo, error)
}

// harnessAllowedForRole reports whether a per-spawn harness override may run on
// role in project, plus the harnesses the role does allow (for the error text).
// A role always allows its own resolved harness (role.harness > project.harness
// > claude); `allowed_harnesses` on the role bead adds more. So a default role
// is claude-only and a Codex-capable one is opted in explicitly. When the role
// catalog cannot be read it fails open (allowed=true): the controller refuses a
// harness/account mismatch at spawn anyway, and a daemon blip should not turn a
// valid mention into a refusal.
func (b *Bot) harnessAllowedForRole(ctx context.Context, project, role, harness string) (allowed bool, allowedSet []string) {
	own := beadsapi.DefaultHarness
	var extra []string
	if projects, err := b.daemon.ListProjectBeads(ctx); err == nil {
		if p, ok := projects[project]; ok {
			if h, err := beadsapi.ParseHarness(p.Harness); err == nil && h != "" {
				own = h
			}
		}
	}
	rl, ok := b.daemon.(roleHarnessLister)
	if !ok {
		return true, nil
	}
	roles, err := rl.ListRoleBeads(ctx)
	if err != nil {
		b.logger.Warn("harness override: role catalog unavailable, deferring to controller validation",
			"role", role, "error", err)
		return true, nil
	}
	if ri, ok := roles[role]; ok {
		if h, err := beadsapi.ParseHarness(ri.Harness); err == nil && h != "" {
			own = h
		}
		extra = ri.AllowedHarnesses
	}
	allowedSet = append([]string{own}, extra...)
	for _, h := range allowedSet {
		if h == harness {
			return true, allowedSet
		}
	}
	return false, allowedSet
}

// harnessRoleRefusal is the operator-facing text for a harness the target role
// does not allow.
func harnessRoleRefusal(role, harness string, allowedSet []string) string {
	return fmt.Sprintf("Role `%s` in this channel is %s-only and cannot run on `%s`. Use `--role <name>` for a role that allows it, or add `%s` to that role's `allowed_harnesses`.",
		role, strings.Join(allowedSet, "/"), harness, harness)
}
