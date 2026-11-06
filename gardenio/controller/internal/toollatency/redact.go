package toollatency

import (
	"encoding/json"
	"regexp"
)

// redactedPlaceholder replaces a matched secret value wherever RedactText or
// RedactJSON finds one. Fixed-width and content-free so a redacted row never
// leaks even the secret's length.
const redactedPlaceholder = "[REDACTED]"

// secretKeyRe matches env-style KEY=value assignments and KEY: value /
// "key": "value" pairs whose key name looks secret-ish (token, key, secret,
// password, credential...), capturing the key+separator in group 1 so the
// replacement can preserve it and only blank the value. Case-insensitive;
// matches common separators (=, :, : ") so it covers shell env assignments,
// YAML-ish key: value, and loose JSON-like text in a command line.
//
// Deliberately excludes a bare "auth" keyword — "Authorization" and "author"
// both contain it, and the Authorization-header case is already covered
// specifically by bearerTokenRe below (plus any literal *_token/*_key
// variant here); a generic "auth" match redacted the scheme word itself
// ("Bearer") as a false positive during development.
var secretKeyRe = regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(?:token|secret|password|passwd|api[_-]?key|credential)[A-Za-z0-9_]*\s*[=:]\s*"?)[^\s"]+`)

// bearerTokenRe matches an HTTP "Bearer <token>" credential, case-insensitive
// on the scheme.
var bearerTokenRe = regexp.MustCompile(`(?i)\b(Bearer\s+)\S+`)

// flagValueSecretRe matches `--password <value>` / `--token=<value>`-style
// CLI flags whose name looks secret-ish, covering both "--flag value" and
// "--flag=value" forms.
var flagValueSecretRe = regexp.MustCompile(`(?i)(--?[A-Za-z0-9-]*(?:token|secret|password|passwd|api[_-]?key|credential)[A-Za-z0-9-]*(?:[= ]))(\S+)`)

// knownTokenPrefixRe matches well-known API token formats by their
// distinctive prefix, regardless of surrounding context — these are
// unambiguous enough that no key name or flag is needed to recognize them.
var knownTokenPrefixRe = regexp.MustCompile(`\b(ghp_|gho_|ghu_|ghs_|ghr_|github_pat_|sk-|sk-ant-|xox[baprs]-|AKIA|ASIA)[A-Za-z0-9_-]{8,}`)

// RedactText scrubs secret-shaped substrings out of a plain-text command
// line (or any free text), replacing each match's value with
// redactedPlaceholder. This is the second of two independent redaction
// layers (plan §P2 "redaction is a precondition"): the producer
// (gb hook relay) applies the same scrub before the event ever leaves the
// agent pod; the consumer applies it again here, in-pod-independent, before
// any row reaches the ledger — defense in depth, not a single point of
// failure.
//
// Redaction is deliberately pattern-based and conservative about false
// positives it can't help (a value that merely LOOKS like a secret gets
// redacted too) rather than about false negatives — argv must never leak a
// real credential into a durable, queryable table.
func RedactText(s string) string {
	s = knownTokenPrefixRe.ReplaceAllString(s, redactedPlaceholder)
	s = bearerTokenRe.ReplaceAllString(s, "${1}"+redactedPlaceholder)
	s = flagValueSecretRe.ReplaceAllString(s, "${1}"+redactedPlaceholder)
	s = secretKeyRe.ReplaceAllString(s, "${1}"+redactedPlaceholder)
	return s
}

// RedactJSON scrubs secret-shaped values out of a JSON document (a
// PostToolUse tool_input payload), recursively: any object key that looks
// secret-ish (same key vocabulary as secretKeyRe: token/secret/password/
// api_key/credential/auth) has its value replaced with redactedPlaceholder
// regardless of type; every string value anywhere in the structure (object,
// array, or bare) additionally passes through RedactText, so a secret
// embedded in a free-text field (e.g. a Bash tool_input's "command") is still
// caught even when its containing key is innocuous.
//
// Malformed JSON is returned unchanged but still passed through RedactText
// as plain text — a best-effort caller should never lose redaction because
// the payload didn't parse.
func RedactJSON(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []byte(RedactText(string(raw)))
	}
	redacted := redactValue(v)
	out, err := json.Marshal(redacted)
	if err != nil {
		return []byte(RedactText(string(raw)))
	}
	return out
}

func redactValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			if secretKeyNameRe.MatchString(k) {
				out[k] = redactedPlaceholder
				continue
			}
			out[k] = redactValue(child)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = redactValue(child)
		}
		return out
	case string:
		return RedactText(val)
	default:
		return val
	}
}

// secretKeyNameRe matches a bare JSON object key that looks secret-ish, used
// to blank the whole value (any type) rather than relying on RedactText
// finding a pattern inside it — a secret stored as a plain string value under
// an obviously-named key ("token": "abc123") is blanked unconditionally. See
// secretKeyRe above for why a bare "auth" keyword is excluded.
var secretKeyNameRe = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[_-]?key|credential)`)
