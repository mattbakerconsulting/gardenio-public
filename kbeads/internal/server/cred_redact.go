package server

import "regexp"

// credShapeRe matches bare provider/credential SHAPES that a reasoning agent can
// echo into durable bead prose (comment text, description, notes) with no
// key=/token: prefix — claudemore F17 (R4-SEC-4).
//
// Unlike the keyword-based authRedactRe (which only fires on "Authorization:
// Bearer …"), this pass recognizes a credential by its own recognizable prefix
// so it is stripped whole regardless of surrounding text:
//
//   - sk-ant-…              Anthropic API / OAuth keys.
//   - sk-… / vk-…           LiteLLM sk-shaped and virtual keys (the
//                           x-litellm-api-key value the controller injects;
//                           see claudemore-r4-derisking.md F17).
//   - ghp_… / glpat-… /     SCM tokens that also ride the agent pod env (per the
//     github_pat_…          buildCoopEnv audit) and can be pasted the same way.
//
// The trailing run captures the base64url/hex body these credentials use and
// stops at whitespace or any other delimiter, so only the token itself is
// replaced and the surrounding prose is preserved. Two body-length floors keep
// false positives out of durable prose:
//
//   - Distinctive prefixes (sk-ant-, ghp_, glpat-, github_pat_) require a
//     >=8-char body — they are unambiguous, so a short floor is safe.
//   - The bare 2-char litellm prefixes (sk-, vk-) require a >=16-char body, so
//     an ordinary hyphenated identifier (e.g. "task-sk-workaround") is never
//     mistaken for a key. Real litellm keys are 20+ chars, so this never
//     under-redacts a genuine credential.
//
// sk-ant- is matched before the shorter sk- alternative so the more specific
// prefix wins. This is intentionally applied ONLY to agent-authored PROSE, never
// to the structured `fields` JSON — account beads legitimately store a
// litellm_key in a field, and redacting there would corrupt the
// credential-distribution path.
var credShapeRe = regexp.MustCompile(`\b(?:(?:sk-ant-|ghp_|glpat-|github_pat_)[A-Za-z0-9_-]{8,}|(?:sk-|vk-)[A-Za-z0-9_-]{16,})`)

// credRedactReplacement is the marker left in place of a redacted credential.
// It matches the coop recording redaction marker so a grep for leaked-then-
// redacted credentials reads the same across the recording and bead surfaces.
const credRedactReplacement = "<redacted>"

// redactCredShapes replaces any bare credential-shaped token in s with the
// redaction marker. It is a no-op for text that contains no such shape, so it is
// safe to call unconditionally on every prose write. Empty input returns empty.
func redactCredShapes(s string) string {
	if s == "" {
		return s
	}
	return credShapeRe.ReplaceAllString(s, credRedactReplacement)
}
