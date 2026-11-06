// Package claudemore holds the record/replay substrate for the claudemore proxy
// (epic ka-0000006108). It is the Go counterpart to coop's record.rs/s3.rs and is
// deliberately kept free of net/http and cmd wiring so the same schema,
// canonicalization, redaction, and S3 store can be reused by the Phase-2 REPLAY
// path (ka-0000001159) without pulling in the LIVE proxy.
//
// This file is the redaction layer (claudemore F16 / R4-SEC-3): a real reasoning
// agent can echo a provider credential straight into the request/response body
// that claudemore captures. Cassettes are persisted to S3 and, in REPLAY, read
// back and diffed, so a leaked key would be durable and widely visible. The
// redaction below is applied to EVERY byte that reaches a cassette before it is
// hashed or written, so no `sk-ant-` / litellm `sk-`/`vk-` / SCM-token shaped
// secret can survive into the store.
//
// The regexes are ported verbatim (same alternation, same body-length floors)
// from coop `crates/cli/src/record.rs` so the two redactors cannot drift: coop
// redacts the terminal recording, claudemore redacts the LLM wire, and R4-SEC-3
// requires both to enforce identical shapes. The JWT-shape and whitespace-delimited
// `Bearer <token>` rules are Go-side additions (no coop counterpart yet).
package claudemore

import (
	"regexp"
	"sync"
)

// RedactedValue is the marker substituted for a redacted secret. Matches coop's
// `<redacted>` so a downstream reader sees one convention across both stores.
const RedactedValue = "<redacted>"

var (
	// keywordRedactRE matches `key=value` / `key: value` shapes for common
	// secret keywords and captures the keyword+delimiter prefix (group 1) so the
	// substitution keeps it and replaces only the trailing value. Ported from
	// coop record.rs `redaction_regex`.
	keywordRedactRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)((?:api[_-]?key|password|token|secret|bearer)\s*[:=]\s*)\S+`)
	})

	// credentialShapeRE matches a BARE provider credential by its own recognizable
	// prefix, no keyword required — the case R4-SEC-3 is about, where a reasoning
	// agent prints a raw key. Ported from coop record.rs `credential_shape_regex`:
	//
	//   - sk-ant- / ghp_ / glpat- / github_pat_  →  >=8-char body
	//   - sk- / vk- (bare litellm prefixes)       →  >=16-char body
	//
	// The two body-length floors keep false positives out: the distinctive
	// prefixes tolerate an 8-char body, while the short 2-char litellm prefixes
	// require 16 so an ordinary hyphenated identifier (`task-sk-workaround`) is
	// never mistaken for a key — real litellm keys are 20+ chars. `sk-ant-` is
	// listed before the shorter `sk-` alternative so the more specific prefix
	// wins. Go's regexp uses leftmost-longest for POSITION but leftmost-FIRST for
	// alternation within a match, so ordering the alternatives longest-first is
	// what makes `sk-ant-…` redact whole rather than leaving `ant-…` behind.
	credentialShapeRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`\b((sk-ant-|ghp_|glpat-|github_pat_)[A-Za-z0-9_-]{8,}|(sk-|vk-)[A-Za-z0-9_-]{16,})`)
	})

	// jwtRE matches a JSON Web Token by shape: three dot-separated base64url
	// segments whose header starts `eyJ` (base64 of `{"`). OpenAI/ChatGPT OAuth
	// access and id tokens and most OIDC bearer tokens are JWTs, so this catches
	// them wherever they appear (headers, JSON strings, prose), not only after a
	// keyword. The signature segment may be empty (`alg: none`). The 4-char body
	// floors on the first two segments keep `eyJ` followed by a stray dot in
	// prose from matching.
	jwtRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
	})

	// bearerRE matches an HTTP `Bearer <token>` credential: the scheme word, then
	// WHITESPACE (not ':'/'=' — that is keywordRedactRE's shape), then an opaque
	// token. The group-1 prefix is kept so the output reads `Bearer <redacted>`.
	// The 16-char token floor (RFC 6750 b64token charset) keeps ordinary prose
	// such as "bearer authentication" or "bearer of news" intact.
	bearerRE = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]{16,}`)
	})
)

// RedactBytes returns a copy of b with secret-shaped substrings replaced by
// RedactedValue. It runs four passes: the whitespace-delimited `Bearer <token>`
// pass first (so `token: Bearer abc…` cannot leave the real token behind once the
// keyword pass has consumed only the word `Bearer`), then the keyword-prefixed
// pass (so `token=sk-…` collapses to a single marker), then the JWT-shape pass,
// then the bare credential-shape pass (F16). A value already replaced with the
// marker no longer matches any regex. Non-secret bytes are preserved verbatim.
//
// This is the single chokepoint every cassette byte passes through before it is
// hashed or persisted. It is intentionally allocation-cheap on the common case:
// when neither regex matches, the input is returned unchanged.
func RedactBytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	out := bearerRE().ReplaceAll(b, []byte("${1}"+RedactedValue))
	out = keywordRedactRE().ReplaceAll(out, []byte("${1}"+RedactedValue))
	out = jwtRE().ReplaceAll(out, []byte(RedactedValue))
	out = credentialShapeRE().ReplaceAll(out, []byte(RedactedValue))
	return out
}

// RedactString is the string convenience wrapper around RedactBytes.
func RedactString(s string) string {
	if s == "" {
		return s
	}
	return string(RedactBytes([]byte(s)))
}

// ContainsSecretShape reports whether b still contains a bare provider-credential
// shape (sk-ant-/sk-/vk-/ghp_/glpat-/github_pat_), a JWT, or a `Bearer <token>`
// credential — i.e. every shape RedactBytes redacts without a `key=` prefix. It is the assertion primitive
// behind the A4 / R4-SEC-3 acceptance test: after redaction, a cassette must
// return false here on a deliberately key-echoing session. It does not check the
// keyword pass — that depends on a `key=` prefix and is a redaction convenience,
// not a leak invariant.
func ContainsSecretShape(b []byte) bool {
	return credentialShapeRE().Match(b) || jwtRE().Match(b) || bearerRE().Match(b)
}
