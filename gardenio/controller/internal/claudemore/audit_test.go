package claudemore

import (
	"strings"
	"testing"
)

// mkCassette assembles a cassette straight from request/response bodies for the
// audit tests — it bypasses CanonicalizeRequest because the audit reads the
// STORED bodies, which are what a real cassette holds after redaction.
func mkCassette(reqBody, respBody string, streaming bool) Cassette {
	return Cassette{
		Version:     CassetteVersion,
		RequestHash: "hash-abc",
		Model:       "claude-opus-4-8",
		Request:     CassetteRequest{Method: "POST", Path: "/v1/messages", Body: []byte(reqBody)},
		Response:    CassetteResponse{StatusCode: 200, Streaming: streaming, Body: []byte(respBody)},
	}
}

// ── native identity (metadata.user_id — the header-archaeology replacement) ──

func TestAuditCassette_NativeUserID(t *testing.T) {
	c := mkCassette(
		`{"model":"claude-opus-4-8","metadata":{"user_id":"agent-crew-run7"},"messages":[]}`,
		`{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`,
		false,
	)
	a := AuditCassette(c)
	if a.SessionUserID != "agent-crew-run7" {
		t.Errorf("SessionUserID = %q, want agent-crew-run7", a.SessionUserID)
	}
	if a.RequestHash != "hash-abc" || a.Model != "claude-opus-4-8" {
		t.Errorf("hash/model not carried: %q/%q", a.RequestHash, a.Model)
	}
}

func TestAuditCassette_NoMetadata(t *testing.T) {
	c := mkCassette(`{"model":"m","messages":[]}`, `{"content":[]}`, false)
	if got := AuditCassette(c).SessionUserID; got != "" {
		t.Errorf("SessionUserID = %q, want empty when no metadata", got)
	}
}

func TestAuditCassette_MalformedRequestBody(t *testing.T) {
	// A non-JSON request body must degrade to empty identity, not panic.
	c := mkCassette(`not json at all`, `{"content":[]}`, false)
	if got := AuditCassette(c).SessionUserID; got != "" {
		t.Errorf("SessionUserID = %q, want empty on malformed request", got)
	}
}

// ── non-streaming response: content blocks, tool_use audit, usage ──

func TestAuditCassette_NonStreamingFull(t *testing.T) {
	resp := `{
	  "content":[
	    {"type":"thinking","thinking":"Let me pick the right bead."},
	    {"type":"text","text":"Claiming it now."},
	    {"type":"tool_use","id":"toolu_01","name":"Bash"},
	    {"type":"tool_use","id":"toolu_02","name":"kd_claim"}
	  ],
	  "stop_reason":"tool_use",
	  "usage":{"input_tokens":1200,"output_tokens":80,"cache_read_input_tokens":900,"cache_creation_input_tokens":300}
	}`
	a := AuditCassette(mkCassette(`{"model":"m"}`, resp, false))

	if a.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", a.StopReason)
	}
	if len(a.ToolUses) != 2 || a.ToolUses[0].Name != "Bash" || a.ToolUses[1].Name != "kd_claim" {
		t.Errorf("ToolUses = %+v, want [Bash kd_claim] in order", a.ToolUses)
	}
	if a.ToolUses[0].ID != "toolu_01" {
		t.Errorf("tool_use ID not captured: %q", a.ToolUses[0].ID)
	}
	if a.ThinkingText != "Let me pick the right bead." {
		t.Errorf("ThinkingText = %q", a.ThinkingText)
	}
	if a.ThinkingChars != len("Let me pick the right bead.") {
		t.Errorf("ThinkingChars = %d, want %d", a.ThinkingChars, len("Let me pick the right bead."))
	}
	if a.TextChars != len("Claiming it now.") {
		t.Errorf("TextChars = %d, want %d", a.TextChars, len("Claiming it now."))
	}
	want := TokenUsage{InputTokens: 1200, OutputTokens: 80, CacheReadInputTokens: 900, CacheCreationInputTokens: 300}
	if a.Usage != want {
		t.Errorf("Usage = %+v, want %+v", a.Usage, want)
	}
	if a.Streaming {
		t.Error("Streaming should be false for a JSON body")
	}
}

func TestAuditCassette_MultipleThinkingBlocksJoined(t *testing.T) {
	resp := `{"content":[
	  {"type":"thinking","thinking":"first"},
	  {"type":"thinking","thinking":"second"}
	]}`
	a := AuditCassette(mkCassette(`{}`, resp, false))
	if a.ThinkingText != "first\nsecond" {
		t.Errorf("ThinkingText = %q, want joined with newline", a.ThinkingText)
	}
	if a.ThinkingChars != len("first\nsecond") {
		t.Errorf("ThinkingChars = %d", a.ThinkingChars)
	}
}

func TestAuditCassette_TextOnlyNoTools(t *testing.T) {
	a := AuditCassette(mkCassette(`{}`, `{"content":[{"type":"text","text":"just text"}],"stop_reason":"end_turn"}`, false))
	if len(a.ToolUses) != 0 {
		t.Errorf("ToolUses = %+v, want none", a.ToolUses)
	}
	if a.ThinkingChars != 0 {
		t.Errorf("ThinkingChars = %d, want 0", a.ThinkingChars)
	}
	if a.TextChars != len("just text") {
		t.Errorf("TextChars = %d", a.TextChars)
	}
}

// ── streaming (SSE) response ──

func TestAuditCassette_StreamingFull(t *testing.T) {
	// A realistic Anthropic SSE stream: message_start (input+cache usage), a
	// thinking block streamed as deltas, a text block, a tool_use block, then
	// message_delta with the terminal stop_reason + final output_tokens.
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":500,"cache_read_input_tokens":400,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Reasoning "}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"about it."}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Done."}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_09","name":"Write"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	a := AuditCassette(mkCassette(`{"metadata":{"user_id":"stream-run"}}`, sse, true))

	if !a.Streaming {
		t.Error("Streaming should be true")
	}
	if a.SessionUserID != "stream-run" {
		t.Errorf("SessionUserID = %q", a.SessionUserID)
	}
	if a.ThinkingText != "Reasoning about it." {
		t.Errorf("ThinkingText = %q, want concatenated deltas", a.ThinkingText)
	}
	if a.ThinkingChars != len("Reasoning about it.") {
		t.Errorf("ThinkingChars = %d", a.ThinkingChars)
	}
	if a.TextChars != len("Done.") {
		t.Errorf("TextChars = %d, want %d", a.TextChars, len("Done."))
	}
	if len(a.ToolUses) != 1 || a.ToolUses[0].Name != "Write" || a.ToolUses[0].ID != "toolu_09" {
		t.Errorf("ToolUses = %+v, want [{Write toolu_09}]", a.ToolUses)
	}
	if a.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", a.StopReason)
	}
	// input+cache from message_start, output from message_delta (authoritative).
	want := TokenUsage{InputTokens: 500, OutputTokens: 42, CacheReadInputTokens: 400}
	if a.Usage != want {
		t.Errorf("Usage = %+v, want %+v", a.Usage, want)
	}
}

func TestAuditCassette_StreamingSniffedWithoutFlag(t *testing.T) {
	// Streaming flag unset, but the body is clearly an event stream — the sniff
	// fallback must still fold it rather than trying to JSON-parse the whole thing.
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hey\"}}\n\n"
	a := AuditCassette(mkCassette(`{}`, sse, false /* flag deliberately off */))
	if a.TextChars != len("hey") {
		t.Errorf("TextChars = %d, want %d (sniff should have folded the stream)", a.TextChars, len("hey"))
	}
	if a.Usage.InputTokens != 10 {
		t.Errorf("Usage.InputTokens = %d, want 10", a.Usage.InputTokens)
	}
}

func TestAuditCassette_StreamingDoneSentinelAndCRLF(t *testing.T) {
	// OpenAI-style [DONE] sentinel + CRLF line endings must not break folding.
	sse := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\r\n\r\ndata: [DONE]\r\n\r\n"
	a := AuditCassette(mkCassette(`{}`, sse, true))
	if a.TextChars != 1 {
		t.Errorf("TextChars = %d, want 1", a.TextChars)
	}
}

// ── robustness / edge cases (observability must never panic or gate) ──

func TestAuditCassette_EmptyResponseBody(t *testing.T) {
	a := AuditCassette(mkCassette(`{"metadata":{"user_id":"u"}}`, ``, false))
	if a.SessionUserID != "u" {
		t.Errorf("SessionUserID = %q, want u even with empty response", a.SessionUserID)
	}
	if a.TextChars != 0 || a.ThinkingChars != 0 || len(a.ToolUses) != 0 {
		t.Errorf("expected zero-value projection for empty body, got %+v", a)
	}
}

func TestAuditCassette_MalformedResponseBody(t *testing.T) {
	// Garbage response body: identity still extracted, response projection zero.
	a := AuditCassette(mkCassette(`{"metadata":{"user_id":"u"}}`, `{ this is not json`, false))
	if a.SessionUserID != "u" {
		t.Errorf("SessionUserID = %q", a.SessionUserID)
	}
	if a.StopReason != "" || a.TextChars != 0 {
		t.Errorf("expected empty projection on malformed body, got %+v", a)
	}
}

func TestAuditCassette_MalformedSSEEventSkipped(t *testing.T) {
	// A bad event in the middle must be skipped, good ones still folded.
	sse := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"a\"}}\n\n" +
		"data: {not valid json}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"b\"}}\n\n"
	a := AuditCassette(mkCassette(`{}`, sse, true))
	if a.TextChars != 2 {
		t.Errorf("TextChars = %d, want 2 (bad event skipped, a+b folded)", a.TextChars)
	}
}

// ── redaction-safety: the audit only lifts already-redacted bytes ──

func TestAuditCassette_RedactionSafe(t *testing.T) {
	// A cassette is stored AFTER RedactBytes, so a secret the model echoed is
	// already the marker. Prove the audit surfaces the redacted form, never a
	// live-shaped secret — the A4/R4-SEC-3 invariant, asserted with the same
	// ContainsSecretShape primitive the recorder tests use.
	redactedThinking := RedactString("my key is sk-ant-abcdefgh12345678 and vk-0123456789abcdef0123")
	if ContainsSecretShape([]byte(redactedThinking)) {
		t.Fatalf("test precondition: redaction should have scrubbed the secret shapes: %q", redactedThinking)
	}
	respJSON := `{"content":[{"type":"thinking","thinking":` + jsonQuote(redactedThinking) + `}]}`
	a := AuditCassette(mkCassette(`{}`, respJSON, false))
	if ContainsSecretShape([]byte(a.ThinkingText)) {
		t.Errorf("audit surfaced a secret-shaped string: %q", a.ThinkingText)
	}
	if !strings.Contains(a.ThinkingText, RedactedValue) {
		t.Errorf("expected the redaction marker in surfaced thinking, got %q", a.ThinkingText)
	}
}

// jsonQuote is a tiny helper to embed a string as a JSON string literal in the
// hand-authored response bodies above.
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
