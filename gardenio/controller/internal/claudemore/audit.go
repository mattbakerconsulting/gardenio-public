package claudemore

import (
	"bytes"
	"encoding/json"
	"strings"
)

// audit.go is the Phase-7 S6 observability SUBSTRATE (R5 synthesis §4 Stretch S6,
// epic ka-0000006108): a pure, I/O-free extractor that turns a recorded
// Messages-API exchange (a Cassette) into a compact observability projection.
//
// WHY THIS LIVES ON THE PROXY (R1 §5). claudemore is the single vantage point
// that sees, on the LLM wire, BOTH the NATIVE Claude-Code identity the client
// stamps into `metadata.user_id` AND the model's reasoning plaintext (streamed as
// `thinking_delta` events before Anthropic strips it on persist — see
// gardenio/docs/thinking-capture-spike.md). Today that reasoning plaintext is
// recovered by a bolt-on litellm logging callback
// (helm/gardenio/files/litellm/litellm_thinking_callback.py) that scrapes the
// agent name out of an `x-litellm-tags` header ("header archaeology"). A CC-aware
// proxy that already parses the exchange can key the same observability to native
// session identity instead — the R1 §5 "production half pays for itself" value,
// and the substrate the beads-viewer S6 surface renders.
//
// COST FENCE (R5 §5.2). This SURFACES the `usage` block the model already
// reported for observability; it does NOT meter or estimate spend. litellm
// remains the exclusive cost source-of-truth (litellm-only-cost-estimation).
// Token counts here are "what the model said it used", never a dollar figure.
//
// REDACTION-SAFE BY CONSTRUCTION (A4 / R4-SEC-3). A Cassette's request and
// response bodies have already passed through RedactBytes before they were stored
// (see recorder.go buildCassette + CanonicalizeRequest), so every string this
// extractor lifts out — thinking text, tool names, the user_id — is drawn from
// bytes that can no longer carry an `sk-ant-`/`vk-` shaped secret. This
// primitive is deliberately kept free of net/http and S3 wiring so a caller (the
// beads-viewer cassette reader) can compose it without pulling in the LIVE proxy.

// ToolUseRef names one `tool_use` content block the model emitted, in wire
// order. Only the identity (Name + block ID) is captured — the tool INPUT is
// deliberately omitted: the wire-level audit answers "which tools did the model
// choose to call", not "with what arguments", and an input can be large and
// carry embedded content. Name is the recognizable label (Bash, Read, Agent, a
// Skill/MCP tool); ID is Anthropic's per-block `toolu_…` id, useful for
// correlating a call with its later tool_result.
type ToolUseRef struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

// TokenUsage mirrors the Anthropic `usage` block the model reports for a
// response. Surfaced for OBSERVABILITY only (see the cost-fence note above); it
// is not a spend figure and must not be turned into one — litellm owns cost.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// WireAudit is the observability projection of one recorded Messages-API
// round-trip. It is what the S6 surface renders / a native NATS publisher emits.
// Every field is derived purely from the (already-redacted) cassette bytes.
type WireAudit struct {
	// RequestHash + Model tie the audit back to the exact cassette it came from
	// (stable across re-record, so a drift alert or a viewer row names it).
	RequestHash string `json:"request_hash"`
	Model       string `json:"model,omitempty"`
	// SessionUserID is the NATIVE Claude-Code identity from request
	// `metadata.user_id` — the header-archaeology replacement (R1 §5). Empty when
	// the client sent no metadata.
	SessionUserID string `json:"session_user_id,omitempty"`
	// Streaming echoes how the response was served (SSE vs a single JSON body).
	Streaming bool `json:"streaming"`
	// ToolUses are the model's `tool_use` blocks in wire order (the tool_use
	// audit). Empty for a pure-text/thinking turn.
	ToolUses []ToolUseRef `json:"tool_uses,omitempty"`
	// ThinkingText is the concatenated reasoning plaintext the model streamed
	// (the value the litellm callback exists to recover). Redaction-safe.
	// ThinkingChars is its length — a compact metric a surface can show without
	// rendering the whole trace.
	ThinkingText  string `json:"thinking_text,omitempty"`
	ThinkingChars int    `json:"thinking_chars"`
	// TextChars is the length of the model's user-visible (non-thinking) text.
	TextChars int `json:"text_chars"`
	// StopReason is Anthropic's terminal reason (end_turn / tool_use / max_tokens
	// / stop_sequence). Empty if the response carried none.
	StopReason string `json:"stop_reason,omitempty"`
	// Usage is the reported token accounting (observability only — see cost fence).
	Usage TokenUsage `json:"usage"`
}

// AuditCassette extracts the observability projection from a recorded exchange.
// It is pure and best-effort: a field that is absent or unparseable is left at
// its zero value rather than raising — a cassette is a valid stored artifact and
// observability must never be a gate (R5 §5.1 "never a per-PR hard gate"). It
// dispatches on the recorded response shape: an SSE (streaming) body is folded
// event-by-event, a non-streaming body is parsed as one Messages response
// object.
func AuditCassette(c Cassette) WireAudit {
	a := WireAudit{
		RequestHash:   c.RequestHash,
		Model:         c.Model,
		SessionUserID: extractUserID(c.Request.Body),
		Streaming:     c.Response.Streaming,
	}

	body := []byte(c.Response.Body)
	if c.Response.Streaming || looksLikeSSE(body) {
		auditStreamingBody(body, &a)
	} else {
		auditJSONBody(body, &a)
	}
	return a
}

// extractUserID pulls request `metadata.user_id` — the native session identity
// the `claude` client stamps per launch. Best-effort: any decode miss yields "".
func extractUserID(reqBody json.RawMessage) string {
	if len(bytes.TrimSpace(reqBody)) == 0 {
		return ""
	}
	var req struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return ""
	}
	return req.Metadata.UserID
}

// messageResponse is the subset of a non-streaming Anthropic Messages response
// the audit reads. Unknown fields are ignored.
type messageResponse struct {
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      *usageBlock    `json:"usage"`
}

// contentBlock is one entry in a response `content[]` array (non-streaming) or a
// streaming `content_block_start`'s `content_block`. Only the discriminant plus
// the fields the audit surfaces are decoded.
type contentBlock struct {
	Type     string `json:"type"` // text | thinking | tool_use | redacted_thinking
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
	Name     string `json:"name"`
	ID       string `json:"id"`
}

// usageBlock is the Anthropic `usage` accounting. Pointer-decoded so an absent
// block is distinguishable from an all-zero one (only present values overwrite).
type usageBlock struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// auditJSONBody folds a non-streaming Messages response into the audit.
func auditJSONBody(body []byte, a *WireAudit) {
	if len(bytes.TrimSpace(body)) == 0 {
		return
	}
	var resp messageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}
	for _, blk := range resp.Content {
		switch blk.Type {
		case "text":
			a.TextChars += len(blk.Text)
		case "thinking":
			appendThinking(a, blk.Thinking)
		case "tool_use":
			a.ToolUses = append(a.ToolUses, ToolUseRef{Name: blk.Name, ID: blk.ID})
		}
	}
	a.StopReason = resp.StopReason
	if resp.Usage != nil {
		a.Usage = TokenUsage{
			InputTokens:              resp.Usage.InputTokens,
			OutputTokens:             resp.Usage.OutputTokens,
			CacheCreationInputTokens: resp.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     resp.Usage.CacheReadInputTokens,
		}
	}
}

// sseEvent is the union of the streaming event shapes the audit folds. A single
// struct decodes them all because the field sets are disjoint by `type` and JSON
// ignores absent keys.
type sseEvent struct {
	Type string `json:"type"`
	// message_start
	Message *struct {
		Usage *usageBlock `json:"usage"`
	} `json:"message"`
	// content_block_start
	ContentBlock *contentBlock `json:"content_block"`
	// content_block_delta
	Delta *struct {
		Type       string `json:"type"` // thinking_delta | text_delta | input_json_delta | signature_delta
		Thinking   string `json:"thinking"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"` // present on message_delta's delta
	} `json:"delta"`
	// message_delta carries the final usage (cumulative output_tokens)
	Usage *usageBlock `json:"usage"`
}

// auditStreamingBody folds an SSE event stream into the audit. It accumulates
// thinking/text deltas, records each tool_use as its block opens, and takes
// input+cache tokens from message_start and the final output_tokens from
// message_delta (streaming reports input once up front and output cumulatively at
// the end). Malformed or unknown events are skipped.
func auditStreamingBody(body []byte, a *WireAudit) {
	var thinking, text strings.Builder
	for _, data := range sseDataPayloads(body) {
		var ev sseEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				a.Usage.InputTokens = ev.Message.Usage.InputTokens
				a.Usage.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
				a.Usage.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens
				// message_start may also carry an initial output_tokens (often 1..few).
				a.Usage.OutputTokens = ev.Message.Usage.OutputTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				a.ToolUses = append(a.ToolUses, ToolUseRef{
					Name: ev.ContentBlock.Name, ID: ev.ContentBlock.ID,
				})
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "thinking_delta":
				thinking.WriteString(ev.Delta.Thinking)
			case "text_delta":
				text.WriteString(ev.Delta.Text)
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				a.StopReason = ev.Delta.StopReason
			}
			// The message_delta usage is the authoritative final output count.
			if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
				a.Usage.OutputTokens = ev.Usage.OutputTokens
			}
		}
	}
	if thinking.Len() > 0 {
		appendThinking(a, thinking.String())
	}
	a.TextChars += text.Len()
}

// appendThinking accumulates reasoning plaintext across blocks, keeping the
// concatenated text and its char count in sync. Multiple thinking blocks are
// joined with a newline so a two-block trace reads as two paragraphs.
func appendThinking(a *WireAudit, s string) {
	if s == "" {
		return
	}
	if a.ThinkingText != "" {
		a.ThinkingText += "\n"
	}
	a.ThinkingText += s
	a.ThinkingChars = len(a.ThinkingText)
}

// sseDataPayloads extracts each event's `data:` JSON payload from a raw SSE body.
// Per the SSE spec an event may carry multiple `data:` lines that concatenate
// with newlines; Anthropic sends one line per event, but this handles both by
// buffering `data:` lines and flushing at the blank line that terminates an
// event (and at end-of-stream). The `[DONE]` sentinel and non-`data:` lines
// (`event:`, `id:`, comments) are ignored. Returns each payload as raw bytes for
// the caller to json.Unmarshal.
func sseDataPayloads(body []byte) [][]byte {
	var out [][]byte
	var buf strings.Builder
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		payload := buf.String()
		buf.Reset()
		if strings.TrimSpace(payload) == "[DONE]" {
			return
		}
		out = append(out, []byte(payload))
	}
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" { // blank line terminates an event
			flush()
			continue
		}
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			// A single leading space after the colon is part of the SSE framing,
			// not the data — strip exactly one.
			after = strings.TrimPrefix(after, " ")
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(after)
		}
		// event:/id:/retry:/comment lines carry no payload for the audit.
	}
	flush() // stream may end without a trailing blank line
	return out
}

// looksLikeSSE is a defensive fallback for a cassette whose Streaming flag is
// unset but whose body is nonetheless an event stream (hand-authored cassette,
// or an upstream that omitted the text/event-stream content-type). It sniffs the
// first non-empty line for the SSE `event:`/`data:` framing.
func looksLikeSSE(body []byte) bool {
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		return strings.HasPrefix(line, "data:") || strings.HasPrefix(line, "event:")
	}
	return false
}
