package claudemore

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCanonicalizeRequest_HashStableAcrossKeyOrder is the A3 core: two logically
// identical requests whose JSON differs only in object-key ordering (and
// whitespace) must produce the SAME request hash — the property REPLAY keys on.
func TestCanonicalizeRequest_HashStableAcrossKeyOrder(t *testing.T) {
	a := RequestIdentity{
		Method:  "POST",
		Path:    "/v1/messages",
		Headers: map[string]string{"anthropic-version": "2023-06-01"},
		Body:    []byte(`{"model":"claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`),
	}
	b := RequestIdentity{
		Method:  "POST",
		Path:    "/v1/messages",
		Headers: map[string]string{"anthropic-version": "2023-06-01"},
		// Same content, keys reordered + reindented.
		Body: []byte("{\n  \"messages\": [ { \"content\": \"hi\", \"role\": \"user\" } ],\n  \"max_tokens\": 100,\n  \"model\": \"claude-sonnet-5\"\n}"),
	}

	_, modelA, hashA, err := CanonicalizeRequest(a)
	if err != nil {
		t.Fatalf("canonicalize a: %v", err)
	}
	_, _, hashB, err := CanonicalizeRequest(b)
	if err != nil {
		t.Fatalf("canonicalize b: %v", err)
	}
	if hashA != hashB {
		t.Errorf("hash not stable across key order:\n a=%s\n b=%s", hashA, hashB)
	}
	if modelA != "claude-sonnet-5" {
		t.Errorf("model tag: got %q want claude-sonnet-5", modelA)
	}
	if hashA == "" {
		t.Error("empty hash")
	}
}

// TestCanonicalizeRequest_HashDiffersOnContent ensures a real content change
// (different prompt) changes the hash — the store must not collide distinct
// requests onto one cassette.
func TestCanonicalizeRequest_HashDiffersOnContent(t *testing.T) {
	base := func(prompt string) RequestIdentity {
		return RequestIdentity{
			Method: "POST", Path: "/v1/messages",
			Body: []byte(`{"model":"m","messages":[{"role":"user","content":"` + prompt + `"}]}`),
		}
	}
	_, _, h1, _ := CanonicalizeRequest(base("what is 2+2"))
	_, _, h2, _ := CanonicalizeRequest(base("what is 3+3"))
	if h1 == h2 {
		t.Error("distinct prompts hashed to the same cassette key")
	}
}

// TestCanonicalizeRequest_IgnoresVolatileMetadata proves the per-session
// metadata field (which varies run-to-run) does not perturb the hash, so a
// re-record of the same logical request still collides.
func TestCanonicalizeRequest_IgnoresVolatileMetadata(t *testing.T) {
	withMeta := RequestIdentity{
		Method: "POST", Path: "/v1/messages",
		Body: []byte(`{"model":"m","metadata":{"user_id":"agent-abc-run1"},"messages":[]}`),
	}
	otherMeta := RequestIdentity{
		Method: "POST", Path: "/v1/messages",
		Body: []byte(`{"model":"m","metadata":{"user_id":"agent-xyz-run2"},"messages":[]}`),
	}
	_, _, h1, _ := CanonicalizeRequest(withMeta)
	_, _, h2, _ := CanonicalizeRequest(otherMeta)
	if h1 != h2 {
		t.Errorf("volatile metadata changed the hash:\n h1=%s\n h2=%s", h1, h2)
	}
}

// TestCanonicalizeRequest_IgnoresNestedCacheControl proves that cache_control
// breakpoints ({"type":"ephemeral","ttl":"1h"}) injected by the `claude` client
// at run-to-run-VOLATILE positions inside system[] and messages[].content[] do
// NOT perturb the request hash. Two logically identical launches whose
// breakpoints land in different spots must still collide (the REPLAY key
// property, A3). The cache_control blocks are a caching hint with no effect on
// the model's response — exactly like metadata.user_id.
func TestCanonicalizeRequest_IgnoresNestedCacheControl(t *testing.T) {
	// Launch A: breakpoint on the last system block and the first content block.
	launchA := RequestIdentity{
		Method: "POST", Path: "/v1/messages",
		Body: []byte(`{"model":"m","system":[{"type":"text","text":"sys0"},{"type":"text","text":"sys1","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`),
	}
	// Launch B: SAME logical content, breakpoints in DIFFERENT spots (on sys0 and
	// no message breakpoint) — the volatile placement the client actually varies.
	launchB := RequestIdentity{
		Method: "POST", Path: "/v1/messages",
		Body: []byte(`{"model":"m","system":[{"type":"text","text":"sys0","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"sys1"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
	}
	reqA, _, hA, err := CanonicalizeRequest(launchA)
	if err != nil {
		t.Fatalf("canonicalize a: %v", err)
	}
	_, _, hB, err := CanonicalizeRequest(launchB)
	if err != nil {
		t.Fatalf("canonicalize b: %v", err)
	}
	if hA != hB {
		t.Errorf("nested cache_control changed the hash:\n hA=%s\n hB=%s", hA, hB)
	}
	// The STORED body must KEEP cache_control for fidelity — only the hash strips.
	if !strings.Contains(string(reqA.Body), "cache_control") {
		t.Errorf("cache_control was stripped from the STORED body (should be kept for fidelity):\n%s", reqA.Body)
	}
}

// TestCanonicalizeRequest_NestedCacheControlStillHashesContent guards against
// over-stripping: removing cache_control for the hash must NOT erase the real
// content next to it — a genuine prompt change still changes the hash.
func TestCanonicalizeRequest_NestedCacheControlStillHashesContent(t *testing.T) {
	base := func(prompt string) RequestIdentity {
		return RequestIdentity{
			Method: "POST", Path: "/v1/messages",
			Body: []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"` + prompt + `","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`),
		}
	}
	_, _, h1, _ := CanonicalizeRequest(base("what is 2+2"))
	_, _, h2, _ := CanonicalizeRequest(base("what is 3+3"))
	if h1 == h2 {
		t.Error("distinct prompts (both carrying cache_control) hashed to the same cassette key")
	}
}

// TestCanonicalizeRequest_NormalizesCCVersion proves the per-launch cc_version
// billing token the client prepends to system[0]
// (`x-anthropic-billing-header: cc_version=2.1.211.<suffix>; …`) does not perturb
// the hash: two launches whose suffix differs (.ab5 vs .160) must collide, while
// the stored body keeps the original text for fidelity.
func TestCanonicalizeRequest_NormalizesCCVersion(t *testing.T) {
	launch := func(suffix string) RequestIdentity {
		return RequestIdentity{
			Method: "POST", Path: "/v1/messages",
			Body: []byte(`{"model":"m","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.211.` + suffix + `; foo=bar\nYou are a helpful assistant."}],"messages":[]}`),
		}
	}
	reqA, _, hA, err := CanonicalizeRequest(launch("ab5"))
	if err != nil {
		t.Fatalf("canonicalize ab5: %v", err)
	}
	_, _, hB, err := CanonicalizeRequest(launch("160"))
	if err != nil {
		t.Fatalf("canonicalize 160: %v", err)
	}
	if hA != hB {
		t.Errorf("per-launch cc_version suffix changed the hash:\n hA=%s\n hB=%s", hA, hB)
	}
	// Stored body keeps the ORIGINAL cc_version for fidelity (only the hash body
	// is normalized).
	if !strings.Contains(string(reqA.Body), "cc_version=2.1.211.ab5") {
		t.Errorf("stored body lost the original cc_version (should keep it for fidelity):\n%s", reqA.Body)
	}
}

// TestCanonicalizeRequest_CCVersionStillHashesSystemContent guards against the
// normalization eating the surrounding system prompt: a genuine change to the
// system text (next to the same cc_version token) still changes the hash.
func TestCanonicalizeRequest_CCVersionStillHashesSystemContent(t *testing.T) {
	launch := func(sysText string) RequestIdentity {
		return RequestIdentity{
			Method: "POST", Path: "/v1/messages",
			Body: []byte(`{"model":"m","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.211.ab5; foo=bar\n` + sysText + `"}],"messages":[]}`),
		}
	}
	_, _, h1, _ := CanonicalizeRequest(launch("You are agent A."))
	_, _, h2, _ := CanonicalizeRequest(launch("You are agent B."))
	if h1 == h2 {
		t.Error("distinct system prompts (same cc_version) hashed to the same cassette key")
	}
}

// TestCanonicalizeRequest_DropsVolatileHeaders proves Authorization / x-litellm-*
// (secret + volatile) never enter the stored cassette or the hash.
func TestCanonicalizeRequest_DropsVolatileHeaders(t *testing.T) {
	id := RequestIdentity{
		Method: "POST", Path: "/v1/messages",
		Headers: map[string]string{
			"Authorization":     "Bearer sk-ant-oat01-SECRETSECRETSECRET",
			"x-litellm-api-key": "sk-1234567890abcdefABCDEF", // gitleaks:allow: fake key
			"x-request-id":      "req-volatile-123",
			"anthropic-version": "2023-06-01",
		},
		Body: []byte(`{"model":"m","messages":[]}`),
	}
	req, _, _, err := CanonicalizeRequest(id)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if _, ok := req.Headers["authorization"]; ok {
		t.Error("Authorization survived into stored cassette")
	}
	if _, ok := req.Headers["x-litellm-api-key"]; ok {
		t.Error("x-litellm-api-key survived into stored cassette")
	}
	if _, ok := req.Headers["x-request-id"]; ok {
		t.Error("volatile x-request-id survived into stored cassette")
	}
	if req.Headers["anthropic-version"] != "2023-06-01" {
		t.Errorf("allowlisted header dropped: %+v", req.Headers)
	}
}

// TestCanonicalizeRequest_HashVariesByAllowlistedHeader confirms a semantic
// header (anthropic-beta) is part of the identity.
func TestCanonicalizeRequest_HashVariesByAllowlistedHeader(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	_, _, h1, _ := CanonicalizeRequest(RequestIdentity{Method: "POST", Path: "/v1/messages", Body: body})
	_, _, h2, _ := CanonicalizeRequest(RequestIdentity{
		Method: "POST", Path: "/v1/messages", Body: body,
		Headers: map[string]string{"anthropic-beta": "tools-2024-04-04"},
	})
	if h1 == h2 {
		t.Error("anthropic-beta header did not affect the request hash")
	}
}

// TestCanonicalizeRequest_RedactsBodyBeforeStore is A4 at the canonicalization
// layer: a request body echoing a key is redacted in the STORED cassette body.
func TestCanonicalizeRequest_RedactsBodyBeforeStore(t *testing.T) {
	id := RequestIdentity{
		Method: "POST", Path: "/v1/messages",
		Body: []byte(`{"model":"m","messages":[{"role":"user","content":"my key is sk-ant-oat01-LEAKLEAKLEAK123"}]}`),
	}
	req, _, _, err := CanonicalizeRequest(id)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if strings.Contains(string(req.Body), "LEAKLEAKLEAK123") {
		t.Errorf("secret survived into stored request body: %s", req.Body)
	}
	if ContainsSecretShape(req.Body) {
		t.Errorf("request body still has a secret shape: %s", req.Body)
	}
}

// TestCanonicalizeRequest_NonJSONBody proves a non-JSON body degrades gracefully:
// it is still redacted and hashed, stored as a valid (marshalable) JSON string
// that round-trips losslessly, rather than being dropped.
func TestCanonicalizeRequest_NonJSONBody(t *testing.T) {
	id := RequestIdentity{Method: "POST", Path: "/v1/messages", Body: []byte("not json at all")}
	req, model, hash, err := CanonicalizeRequest(id)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if hash == "" {
		t.Error("non-JSON body produced empty hash")
	}
	if model != "" {
		t.Errorf("non-JSON body should have no model, got %q", model)
	}
	// Stored as a JSON string that decodes back to the original raw text.
	var decoded string
	if err := json.Unmarshal(req.Body, &decoded); err != nil {
		t.Fatalf("stored non-JSON body is not valid JSON: %v (%q)", err, req.Body)
	}
	if decoded != "not json at all" {
		t.Errorf("non-JSON body did not round-trip: %q", decoded)
	}
	// The whole cassette must marshal cleanly (the regression this guards).
	c := NewCassette(req, model, hash, CassetteResponse{StatusCode: 200}, 1)
	if _, err := c.Marshal(); err != nil {
		t.Fatalf("cassette with non-JSON body failed to marshal: %v", err)
	}
}

// TestCanonicalizeRequest_EmptyBodyMarshals guards the empty/bodyless request
// (e.g. GET) case: the stored body must be valid JSON so the cassette marshals.
func TestCanonicalizeRequest_EmptyBodyMarshals(t *testing.T) {
	for _, body := range [][]byte{nil, []byte(""), []byte("   ")} {
		req, model, hash, err := CanonicalizeRequest(RequestIdentity{Method: "GET", Path: "/x", Body: body})
		if err != nil {
			t.Fatalf("canonicalize %q: %v", body, err)
		}
		c := NewCassette(req, model, hash, CassetteResponse{StatusCode: 200}, 1)
		if _, err := c.Marshal(); err != nil {
			t.Errorf("empty body %q made cassette unmarshalable: %v", body, err)
		}
	}
}

// TestCassette_RoundTrip covers Marshal/Unmarshal and the version guard.
func TestCassette_RoundTrip(t *testing.T) {
	req := CassetteRequest{Method: "POST", Path: "/v1/messages", Body: json.RawMessage(`{"model":"m"}`)}
	c := NewCassette(req, "m", "deadbeef", CassetteResponse{StatusCode: 200, Body: []byte("ok")}, 1700000000000)
	b, err := c.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := UnmarshalCassette(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RequestHash != "deadbeef" || got.Model != "m" || got.Response.StatusCode != 200 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if string(got.Response.Body) != "ok" {
		t.Errorf("response body lost: %q", got.Response.Body)
	}
}

func TestUnmarshalCassette_RejectsBadVersion(t *testing.T) {
	if _, err := UnmarshalCassette([]byte(`{"version":999,"request_hash":"x"}`)); err == nil {
		t.Error("expected error for unsupported version")
	}
}

// TestNewCassette_TimestampNotInHash guards the A3 invariant at the schema level:
// RecordedAtUnixMs must not be an input to RequestHash (the hash is computed
// upstream in CanonicalizeRequest and passed in), so two records of the same
// request at different times share a hash.
func TestNewCassette_TimestampNotInHash(t *testing.T) {
	id := RequestIdentity{Method: "POST", Path: "/v1/messages", Body: []byte(`{"model":"m","messages":[]}`)}
	req, model, hash, _ := CanonicalizeRequest(id)
	c1 := NewCassette(req, model, hash, CassetteResponse{StatusCode: 200}, 1)
	c2 := NewCassette(req, model, hash, CassetteResponse{StatusCode: 200}, 999999)
	if c1.RequestHash != c2.RequestHash {
		t.Error("timestamp leaked into request hash")
	}
}
