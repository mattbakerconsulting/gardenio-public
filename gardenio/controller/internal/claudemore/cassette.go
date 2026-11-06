package claudemore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// CassetteVersion is the schema version stamped into every cassette. Bump it
// when the on-disk shape changes incompatibly so a REPLAY reader can refuse a
// cassette it cannot interpret rather than silently mis-serve one.
const CassetteVersion = 1

// Cassette is one recorded Anthropic Messages-API round-trip: the canonicalized
// request that keys it, and the upstream response served back byte-for-byte in
// REPLAY. It is the Go counterpart to a coop recording entry, but scoped to the
// LLM wire rather than the terminal screen.
//
// Every string/byte field that can carry model or agent output has already been
// through RedactBytes before it lands here — a Cassette is a redaction-safe
// artifact by construction (A4 / R4-SEC-3). RecordedAtUnixMs is informational
// only and is deliberately excluded from RequestHash so re-recording the same
// input yields the same hash (A3).
type Cassette struct {
	Version          int              `json:"version"`
	RecordedAtUnixMs int64            `json:"recorded_at_unix_ms"`
	RequestHash      string           `json:"request_hash"`
	Model            string           `json:"model,omitempty"`
	Request          CassetteRequest  `json:"request"`
	Response         CassetteResponse `json:"response"`
}

// CassetteRequest is the redacted, canonical view of the inbound request. Body
// is canonical JSON (object keys sorted recursively) so a byte comparison of two
// re-records of the same logical request is stable. Headers holds only the
// semantic allowlist (see hashHeaderAllowlist); volatile/secret headers
// (Authorization, x-litellm-*, x-request-id) are dropped, never stored.
type CassetteRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body"`
}

// CassetteResponse is the redacted upstream response to serve in REPLAY. Body is
// captured verbatim (post-redaction) — for a streaming (SSE) response it is the
// concatenated event stream, replayed as-is with FlushInterval so the real
// `claude` binary's stream parser sees an identical wire. Streaming records
// whether the upstream set text/event-stream so REPLAY can reproduce the
// Content-Type and flush cadence.
type CassetteResponse struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers,omitempty"`
	Streaming  bool              `json:"streaming"`
	Body       []byte            `json:"body"`
}

// hashHeaderAllowlist is the set of request headers that materially change the
// Anthropic API's behavior and therefore belong in the request identity. Auth
// and per-request/volatile headers are excluded so the hash is stable across
// re-record. Keys are canonical lower-case.
var hashHeaderAllowlist = map[string]bool{
	"anthropic-version": true,
	"anthropic-beta":    true,
}

// volatileBodyFields are top-level request-body keys stripped before hashing
// (only for the hash — the full canonical body is still stored). `metadata`
// carries Anthropic's optional per-session user_id, which varies run-to-run
// without changing the model's reasoning; including it would break the
// re-record hash-stability property (A3) that REPLAY keys on.
var volatileBodyFields = map[string]bool{
	"metadata": true,
}

// volatileNestedKeys are object keys stripped RECURSIVELY (at every depth) from
// the hash body — not just top-level like volatileBodyFields. The `claude`
// client injects `cache_control` breakpoints ({"type":"ephemeral","ttl":"1h"})
// into `system[]` and `messages[].content[]` blocks, and places them at
// run-to-run-VOLATILE positions that depend on the runtime cache/token state at
// launch. They are a caching hint with ZERO effect on the model's response —
// exactly like metadata.user_id — so two record/replay launches with
// breakpoints in different spots must still collide. Stripped for the hash only;
// the stored body keeps them for fidelity.
var volatileNestedKeys = map[string]bool{
	"cache_control": true,
}

// ccVersionRE normalizes the per-launch `cc_version=<x>` token the `claude`
// client prepends to `system[0]` in an `x-anthropic-billing-header` line
// (e.g. `cc_version=2.1.211.ab5`). The `<suffix>` (.ab5/.160/.008) varies per
// launch, so the raw system[0] text — and therefore the hash — would differ
// run-to-run. Like cache_control this is a billing hint with no effect on the
// model's response, so the hash body normalizes it to a stable placeholder.
// Stored body keeps the original for fidelity. Applied to any string value in
// the hash body so it survives the client placing the header at any depth.
var ccVersionRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`cc_version=[^;\s]+`)
})

const ccVersionPlaceholder = "cc_version=<normalized>"

// RequestIdentity is the minimal input needed to canonicalize and hash a
// request. It is what the proxy hands the recorder and what a REPLAY lookup is
// built from — keeping it a plain struct (no net/http) is what lets the Phase-2
// REPLAY path reuse this package.
type RequestIdentity struct {
	Method  string
	Path    string
	Headers map[string]string // raw inbound headers (any case); filtered here
	Body    []byte            // raw inbound body
}

// CanonicalizeRequest redacts and canonicalizes a request into the stored
// CassetteRequest, the extracted model tag (for versioning-by-model), and the
// stable request hash REPLAY keys on. It never returns an error for a
// non-JSON body — such a request is hashed over its raw redacted bytes so the
// path degrades gracefully rather than dropping the exchange.
func CanonicalizeRequest(id RequestIdentity) (req CassetteRequest, model string, hash string, err error) {
	redactedBody := RedactBytes(id.Body)

	// Decode the body ONCE and derive both the stored canonical form (with
	// volatile fields, for fidelity) and the hash body (volatile fields stripped
	// so two re-records of the same logical input collide).
	storedBody, hashBody, model := canonicalizeBody(redactedBody)
	headers := filterHashHeaders(id.Headers)

	req = CassetteRequest{
		Method:  id.Method,
		Path:    id.Path,
		Headers: headers,
		Body:    storedBody,
	}

	h, err := hashRequest(id.Method, id.Path, headers, hashBody)
	if err != nil {
		return CassetteRequest{}, "", "", err
	}
	return req, model, h, nil
}

// canonicalizeBody parses body as JSON and re-marshals it with object keys
// sorted recursively, giving a byte-stable canonical form (encoding/json sorts
// map keys and, with UseNumber, preserves numeric literals rather than routing
// them through float64). It returns two forms from a SINGLE decode: `stored`
// keeps every field for fidelity; `hashBody` has the volatile top-level fields
// (see volatileBodyFields) removed so the request hash is stable across
// re-record.
//
// The returned RawMessages are ALWAYS valid JSON so the cassette marshals
// cleanly (an invalid json.RawMessage would fail Cassette.Marshal and silently
// drop the exchange). An empty body becomes JSON `null`; a non-JSON body is
// stored as a JSON string of the (already-redacted) raw text, which round-trips
// losslessly and keeps the model tag empty.
func canonicalizeBody(body []byte) (stored, hashBody json.RawMessage, model string) {
	if len(bytes.TrimSpace(body)) == 0 {
		return json.RawMessage("null"), json.RawMessage("null"), ""
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		enc := jsonStringOrNull(body)
		return enc, enc, ""
	}
	if obj, ok := v.(map[string]any); ok {
		if m, ok := obj["model"].(string); ok {
			model = m
		}
	}
	storedBytes, err := json.Marshal(v)
	if err != nil {
		enc := jsonStringOrNull(body)
		return enc, enc, model
	}
	stored = json.RawMessage(storedBytes)

	// hashBody: strip volatile fields for a stable re-record hash. Marshal
	// `stored` FIRST (above), then mutate the decoded value in place for the hash
	// form — the stored bytes are already captured, so the mutation cannot affect
	// them. Three volatility sources are neutralized: top-level volatileBodyFields
	// (metadata), nested volatileNestedKeys at every depth (cache_control), and
	// the per-launch cc_version token inside any string value.
	hashBody = stored
	changed := false
	if obj, ok := v.(map[string]any); ok && len(volatileBodyFields) > 0 {
		for k := range volatileBodyFields {
			if _, present := obj[k]; present {
				delete(obj, k)
				changed = true
			}
		}
	}
	if nv, nchanged := stripVolatileForHash(v); nchanged {
		v = nv
		changed = true
	}
	if changed {
		if hb, err := json.Marshal(v); err == nil {
			hashBody = json.RawMessage(hb)
		}
	}
	return stored, hashBody, model
}

// stripVolatileForHash recursively neutralizes the per-launch-volatile fields
// that must not enter the request hash: it deletes any key in volatileNestedKeys
// (cache_control) at EVERY depth, and normalizes any cc_version=<x> token inside
// a string value to a stable placeholder. It returns the (possibly replaced)
// value and whether anything changed. Maps and slices are mutated in place;
// strings are immutable so a normalized string is returned for the caller to
// store back into its parent container. The caller has already captured the
// stored bytes, so mutating the decoded value here cannot perturb fidelity.
func stripVolatileForHash(v any) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		changed := false
		for k := range volatileNestedKeys {
			if _, present := t[k]; present {
				delete(t, k)
				changed = true
			}
		}
		for k, child := range t {
			if nc, cc := stripVolatileForHash(child); cc {
				t[k] = nc
				changed = true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, child := range t {
			if nc, cc := stripVolatileForHash(child); cc {
				t[i] = nc
				changed = true
			}
		}
		return t, changed
	case string:
		if strings.Contains(t, "cc_version=") {
			norm := ccVersionRE().ReplaceAllString(t, ccVersionPlaceholder)
			if norm != t {
				return norm, true
			}
		}
		return t, false
	default:
		return v, false
	}
}

// jsonStringOrNull encodes raw bytes as a JSON string so a non-JSON body still
// yields a valid, marshalable RawMessage. Falls back to JSON `null` on the
// (practically impossible) encode error so the caller never holds invalid JSON.
func jsonStringOrNull(body []byte) json.RawMessage {
	enc, err := json.Marshal(string(body))
	if err != nil {
		return json.RawMessage("null")
	}
	return json.RawMessage(enc)
}

// filterHashHeaders lower-cases and keeps only the semantic allowlist headers,
// dropping everything volatile or secret. The result is used both for storage
// and for the hash so the two agree.
func filterHashHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	var out map[string]string
	for k, v := range in {
		lk := strings.ToLower(strings.TrimSpace(k))
		if !hashHeaderAllowlist[lk] {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[lk] = v
	}
	return out
}

// hashRequest computes the stable SHA-256 hash (hex) over a deterministic
// projection of the request. Headers are emitted in sorted order; the body is
// the pre-canonicalized (and volatility-stripped) JSON, embedded verbatim. The
// projection is written by hand rather than via a struct so the field and
// header ordering is unambiguous and cannot drift with struct-tag changes.
func hashRequest(method, path string, headers map[string]string, body json.RawMessage) (string, error) {
	h := sha256.New()
	// Domain separator + versioned so a schema change forces a distinct hash
	// space rather than colliding with v1 cassettes.
	if _, err := fmt.Fprintf(h, "claudemore-req\x00v%d\x00", CassetteVersion); err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(h, "%s\x00%s\x00", method, path); err != nil {
		return "", err
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := fmt.Fprintf(h, "%s\x00%s\x00", k, headers[k]); err != nil {
			return "", err
		}
	}
	if _, err := h.Write([]byte("body\x00")); err != nil {
		return "", err
	}
	if _, err := h.Write(body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// NewCassette assembles a redaction-safe Cassette from a canonicalized request
// and the redacted response. recordedAtUnixMs is stamped for provenance and is
// intentionally not part of RequestHash.
func NewCassette(req CassetteRequest, model, hash string, resp CassetteResponse, recordedAtUnixMs int64) Cassette {
	return Cassette{
		Version:          CassetteVersion,
		RecordedAtUnixMs: recordedAtUnixMs,
		RequestHash:      hash,
		Model:            model,
		Request:          req,
		Response:         resp,
	}
}

// Marshal serializes the cassette to indented JSON for S3 storage. Indented so a
// human reviewing a cassette in the store (or a diff during re-record review)
// can read it.
func (c Cassette) Marshal() ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}

// UnmarshalCassette parses a stored cassette, rejecting a version this build
// cannot interpret so REPLAY fails loudly rather than mis-serving.
func UnmarshalCassette(b []byte) (Cassette, error) {
	var c Cassette
	if err := json.Unmarshal(b, &c); err != nil {
		return Cassette{}, fmt.Errorf("decode cassette: %w", err)
	}
	if c.Version != CassetteVersion {
		return Cassette{}, fmt.Errorf("unsupported cassette version %d (this build reads v%d)", c.Version, CassetteVersion)
	}
	return c, nil
}
