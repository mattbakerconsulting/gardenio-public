package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestParseSessionUsage_BasicCounts(t *testing.T) {
	// Minimal JSONL with one assistant message containing usage and one tool_use.
	// No response_cost fields, so TotalCostUSD should be 0.
	lines := []string{
		// Assistant message with tool_use and usage.
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Read","input":{}}],"usage":{"input_tokens":1000,"output_tokens":200,"cache_creation_input_tokens":50,"cache_read_input_tokens":800}}}`,
		// Result entry with usage.
		`{"type":"result","usage":{"input_tokens":500,"output_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":400}}`,
		// Error result.
		`{"type":"result","is_error":true,"usage":{"input_tokens":100,"output_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`,
		// Summary (compaction, legacy format).
		`{"type":"summary","summary":"Context was summarized."}`,
		// Compact boundary (compaction, current format — verbatim shape from a
		// live transcript, ka-0000004139).
		`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","isMeta":false,"compactMetadata":{"trigger":"auto","preTokens":167061},"level":"info"}`,
		// The compact summary user message that follows a boundary must NOT be
		// counted — it pairs 1:1 with the boundary above.
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued from a previous conversation..."}}`,
		// Other system subtypes must not count.
		`{"type":"system","subtype":"turn_duration","content":"5s","level":"info"}`,
	}

	f, err := os.CreateTemp("", "session-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	usage, err := parseSessionUsage(f.Name())
	if err != nil {
		t.Fatalf("parseSessionUsage: %v", err)
	}

	if usage.InputTokens != 1600 {
		t.Errorf("input_tokens: expected 1600, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 310 {
		t.Errorf("output_tokens: expected 310, got %d", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 1200 {
		t.Errorf("cache_read_tokens: expected 1200, got %d", usage.CacheReadTokens)
	}
	if usage.CacheWriteTokens != 50 {
		t.Errorf("cache_write_tokens: expected 50, got %d", usage.CacheWriteTokens)
	}
	if usage.RequestCount != 3 {
		t.Errorf("request_count: expected 3, got %d", usage.RequestCount)
	}
	if usage.ToolCallCount != 1 {
		t.Errorf("tool_call_count: expected 1, got %d", usage.ToolCallCount)
	}
	if usage.ErrorCount != 1 {
		t.Errorf("error_count: expected 1, got %d", usage.ErrorCount)
	}
	// One legacy "summary" entry + one current "compact_boundary" entry; the
	// isCompactSummary user message and turn_duration system entry don't count.
	if usage.CompactionCount != 2 {
		t.Errorf("compaction_count: expected 2, got %d", usage.CompactionCount)
	}
	// No response_cost in entries — cost comes exclusively from LiteLLM.
	if usage.TotalCostUSD != 0 {
		t.Errorf("total_cost_usd should be 0 without response_cost, got %f", usage.TotalCostUSD)
	}
	// ContextTokens is a LEVEL, not a sum: the latest usage-bearing entry wins.
	// Last entry with usage is the error result: 100 + 0 + 0 (ka-0000002524).
	if usage.ContextTokens != 100 {
		t.Errorf("context_tokens: expected 100 (latest-turn level), got %d", usage.ContextTokens)
	}
	// BootContextTokens is the resident context of the FIRST usage-bearing entry:
	// the assistant message, 1000 + 800 + 50 = 1850 (ka-0000003733). It is set
	// once at turn 0 and never overwritten by later turns.
	if usage.BootContextTokens != 1850 {
		t.Errorf("boot_context_tokens: expected 1850 (turn-0 resident), got %d", usage.BootContextTokens)
	}
}

// TestParseSessionUsage_BootContextIsTurnZero verifies BootContextTokens is
// pinned to the FIRST turn's resident context (input + cache_read +
// cache_creation) and is NOT overwritten by a later, larger turn — the boot size
// is a turn-0 property, not a running max (epic ka-0000003733, Part B).
func TestParseSessionUsage_BootContextIsTurnZero(t *testing.T) {
	lines := []string{
		// Turn 0: cold-cache boot — a big cache_creation write. resident = 5000+0+65000 = 70000.
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5000,"output_tokens":10,"cache_creation_input_tokens":65000,"cache_read_input_tokens":0}}}`,
		// Turn 1: warm cache, larger resident. Must NOT clobber boot.
		`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"more"}],"usage":{"input_tokens":2000,"output_tokens":20,"cache_creation_input_tokens":1000,"cache_read_input_tokens":90000}}}`,
	}

	f, err := os.CreateTemp("", "session-boot-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	usage, err := parseSessionUsage(f.Name())
	if err != nil {
		t.Fatalf("parseSessionUsage: %v", err)
	}
	if usage.BootContextTokens != 70000 {
		t.Errorf("boot_context_tokens: expected 70000 (turn-0 resident), got %d", usage.BootContextTokens)
	}
	// ContextTokens is the LATEST turn's resident: 2000 + 90000 + 1000 = 93000.
	if usage.ContextTokens != 93000 {
		t.Errorf("context_tokens: expected 93000 (latest-turn level), got %d", usage.ContextTokens)
	}
}

// TestParseSessionUsage_DedupesMultiBlockMessage is the aggregate-path
// regression for gd-TQhMwZhlBN: Claude Code writes one assistant message (one
// API request) across multiple JSONL lines — one per content block — each
// repeating the identical message.usage. parseSessionUsage must collapse them by
// message.id so token totals and RequestCount reflect requests, not logged
// blocks. Crucially, the per-block counters (ToolCallCount) must NOT be deduped:
// each tool_use lives on its own line and must still be counted.
func TestParseSessionUsage_DedupesMultiBlockMessage(t *testing.T) {
	// One assistant message (msg_A) spans 3 content-block lines with identical
	// usage — a thinking block, a text block, and a tool_use block. A second
	// message (msg_B) is a single line. A result entry has no message.id.
	lines := []string{
		`{"type":"assistant","message":{"id":"msg_A","role":"assistant","content":[{"type":"thinking","thinking":"hmm"}],"usage":{"input_tokens":100,"output_tokens":40,"cache_read_input_tokens":900,"cache_creation_input_tokens":0}}}`,
		`{"type":"assistant","message":{"id":"msg_A","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":100,"output_tokens":40,"cache_read_input_tokens":900,"cache_creation_input_tokens":0}}}`,
		`{"type":"assistant","message":{"id":"msg_A","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}],"usage":{"input_tokens":100,"output_tokens":40,"cache_read_input_tokens":900,"cache_creation_input_tokens":0}}}`,
		`{"type":"assistant","message":{"id":"msg_B","role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Write","input":{}}],"usage":{"input_tokens":50,"output_tokens":20,"cache_read_input_tokens":500,"cache_creation_input_tokens":0}}}`,
		`{"type":"result","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":0}}`,
	}

	f, err := os.CreateTemp("", "session-dedup-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	usage, err := parseSessionUsage(f.Name())
	if err != nil {
		t.Fatalf("parseSessionUsage: %v", err)
	}

	// 3 distinct requests: msg_A (collapsed), msg_B, and the result entry —
	// NOT 5 (the raw usage-bearing line count).
	if usage.RequestCount != 3 {
		t.Errorf("request_count: expected 3 (msg_A collapsed + msg_B + result), got %d", usage.RequestCount)
	}
	// Output must count msg_A once: 40 + 20 + 5 = 65, NOT 40*3 + 20 + 5 = 145.
	if usage.OutputTokens != 65 {
		t.Errorf("output_tokens: expected 65 (msg_A counted once), got %d", usage.OutputTokens)
	}
	// Input: 100 (msg_A once) + 50 + 10 = 160, NOT 100*3 + 50 + 10 = 360.
	if usage.InputTokens != 160 {
		t.Errorf("input_tokens: expected 160 (msg_A counted once), got %d", usage.InputTokens)
	}
	// Cache read: 900 (msg_A once) + 500 + 100 = 1500, NOT 900*3 + 500 + 100.
	if usage.CacheReadTokens != 1500 {
		t.Errorf("cache_read_tokens: expected 1500 (msg_A counted once), got %d", usage.CacheReadTokens)
	}
	// Regression guard: tool calls are counted PER BLOCK, so both t1 (on a
	// deduped msg_A line) and t2 must be counted — the dedup must NOT collapse
	// the per-block counter.
	if usage.ToolCallCount != 2 {
		t.Errorf("tool_call_count: expected 2 (per-block, dedup must not drop msg_A's tool_use), got %d", usage.ToolCallCount)
	}
}

func TestParseSessionUsage_EmptyLog(t *testing.T) {
	f, err := os.CreateTemp("", "session-empty-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Close()

	usage, err := parseSessionUsage(f.Name())
	if err != nil {
		t.Fatalf("parseSessionUsage: %v", err)
	}

	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Errorf("empty log should have zero tokens")
	}
}

func TestParseSessionUsage_MalformedLines(t *testing.T) {
	lines := []string{
		"not json",
		"",
		`{"type":"result","usage":{"input_tokens":100,"output_tokens":50}}`,
		"also not json {{{",
	}

	f, err := os.CreateTemp("", "session-malformed-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	usage, err := parseSessionUsage(f.Name())
	if err != nil {
		t.Fatalf("parseSessionUsage: %v", err)
	}

	// Should still parse the valid line.
	if usage.InputTokens != 100 {
		t.Errorf("input_tokens: expected 100, got %d", usage.InputTokens)
	}
}

func TestParseSessionUsage_MultipleToolCalls(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}},{"type":"tool_use","id":"t2","name":"Write","input":{}},{"type":"text","text":"hello"}],"usage":{"input_tokens":500,"output_tokens":100}}}`,
	}

	f, err := os.CreateTemp("", "session-tools-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	usage, err := parseSessionUsage(f.Name())
	if err != nil {
		t.Fatalf("parseSessionUsage: %v", err)
	}

	if usage.ToolCallCount != 2 {
		t.Errorf("tool_call_count: expected 2, got %d", usage.ToolCallCount)
	}
}

// TestParseSessionUsage_Interrupts counts the user's own interrupt markers,
// in either content shape, and keeps the last one's time; tool output that
// prints the marker and meta entries do not count (gd-3HAERLWMLu).
func TestParseSessionUsage_Interrupts(t *testing.T) {
	lines := []string{
		`{"type":"user","timestamp":"2026-10-28T19:07:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"is_interrupt := t.contains(\"[Request interrupted by user]\")"}]}}`,
		`{"type":"user","isMeta":true,"timestamp":"2026-10-28T19:07:02.000Z","message":{"role":"user","content":"[Request interrupted by user]"}}`,
		`{"type":"user","timestamp":"2026-10-28T19:07:03.000Z","message":{"role":"user","content":"please grep for [Request interrupted by user]"}}`,
		`{"type":"user","timestamp":"2026-10-28T19:07:05.000Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}`,
		`{"type":"user","timestamp":"2026-10-28T19:07:12.402Z","message":{"role":"user","content":"[Request interrupted by user]"}}`,
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	usage, err := parseSessionUsage(path)
	if err != nil {
		t.Fatalf("parseSessionUsage: %v", err)
	}
	if usage.InterruptCount != 2 {
		t.Errorf("interrupt_count: expected 2, got %d", usage.InterruptCount)
	}
	if usage.LastInterruptAt != "2026-10-28T19:07:12.402Z" {
		t.Errorf("last_interrupt_at: expected 2026-10-28T19:07:12.402Z, got %q", usage.LastInterruptAt)
	}
}

func TestParseSessionUsage_FileNotFound(t *testing.T) {
	_, err := parseSessionUsage("/nonexistent/path.jsonl")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestExtractModel_TopLevel(t *testing.T) {
	entry := map[string]json.RawMessage{
		"model": json.RawMessage(`"claude-sonnet-4-6"`),
	}
	if m := extractModel(entry); m != "claude-sonnet-4-6" {
		t.Errorf("expected claude-sonnet-4-6, got %q", m)
	}
}

func TestExtractModel_NestedInMessage(t *testing.T) {
	entry := map[string]json.RawMessage{
		"message": json.RawMessage(`{"model":"claude-opus-4-6","role":"assistant"}`),
	}
	if m := extractModel(entry); m != "claude-opus-4-6" {
		t.Errorf("expected claude-opus-4-6, got %q", m)
	}
}

func TestExtractModel_TopLevelTakesPrecedence(t *testing.T) {
	entry := map[string]json.RawMessage{
		"model":   json.RawMessage(`"claude-sonnet-4-6"`),
		"message": json.RawMessage(`{"model":"claude-opus-4-6"}`),
	}
	if m := extractModel(entry); m != "claude-sonnet-4-6" {
		t.Errorf("top-level model should take precedence, got %q", m)
	}
}

func TestExtractModel_Missing(t *testing.T) {
	entry := map[string]json.RawMessage{
		"type": json.RawMessage(`"result"`),
	}
	if m := extractModel(entry); m != "" {
		t.Errorf("expected empty string, got %q", m)
	}
}

func mustEntry(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var entry map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &entry); err != nil {
		t.Fatalf("unmarshal fixture entry: %v", err)
	}
	return entry
}

// TestIsResumeBoundary covers both resume-marker shapes the detector recognizes
// and the non-markers it must ignore — the structural signals that make
// redundant_context_spikes resume-aware (bug gd-bpJOr1dotX).
func TestIsResumeBoundary(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "synthetic assistant marker (message.model)",
			line: `{"type":"assistant","message":{"model":"<synthetic>","stop_reason":"stop_sequence","usage":{"input_tokens":0}}}`,
			want: true,
		},
		{
			name: "synthetic assistant marker (top-level model)",
			line: `{"type":"assistant","model":"<synthetic>","message":{"usage":{"input_tokens":0}}}`,
			want: true,
		},
		{
			name: "isMeta continue user marker (string content)",
			line: `{"type":"user","isMeta":true,"message":{"role":"user","content":"Continue from where you left off."}}`,
			want: true,
		},
		{
			name: "isMeta continue user marker (block content)",
			line: `{"type":"user","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"Continue from where you left off."}]}}`,
			want: true,
		},
		{
			name: "ordinary assistant turn is not a boundary",
			line: `{"type":"assistant","message":{"model":"claude-opus-4-8","usage":{"input_tokens":100,"cache_creation_input_tokens":50000}}}`,
			want: false,
		},
		{
			name: "non-meta continue text is not a boundary",
			line: `{"type":"user","message":{"role":"user","content":"Continue from where you left off."}}`,
			want: false,
		},
		{
			name: "isMeta message without the continue text is not a boundary",
			line: `{"type":"user","isMeta":true,"message":{"role":"user","content":"You have been spawned with a task."}}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isResumeBoundary(mustEntry(t, tc.line)); got != tc.want {
				t.Errorf("isResumeBoundary = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseSessionTokenSignals_ResumeReprimeExempted drives the whole parse path
// end-to-end on a synthetic transcript: a steady baseline, a resume boundary
// (synthetic marker), then a large re-prime. The re-prime must be exempt, so the
// session reports zero redundant_context_spikes — the exact behavior that drains
// bug gd-bpJOr1dotX.
func TestParseSessionTokenSignals_ResumeReprimeExempted(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"id":"m1","model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":100,"cache_read_input_tokens":18000,"cache_creation_input_tokens":20000,"cache_creation":{"ephemeral_1h_input_tokens":20000,"ephemeral_5m_input_tokens":0}}}}`,
		`{"type":"assistant","message":{"id":"m2","model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":100,"cache_read_input_tokens":18000,"cache_creation_input_tokens":20000,"cache_creation":{"ephemeral_1h_input_tokens":20000,"ephemeral_5m_input_tokens":0}}}}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"Continue from where you left off."}}`,
		`{"type":"assistant","message":{"id":"syn","model":"<synthetic>","stop_reason":"stop_sequence","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0}}}}`,
		`{"type":"assistant","message":{"id":"m3","model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":100,"cache_read_input_tokens":18000,"cache_creation_input_tokens":90000,"cache_creation":{"ephemeral_1h_input_tokens":90000,"ephemeral_5m_input_tokens":0}}}}`,
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "resume.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write temp fixture: %v", err)
	}

	ts, err := parseSessionTokenSignals(path)
	if err != nil {
		t.Fatalf("parseSessionTokenSignals: %v", err)
	}
	if ts.RedundantContextSpikes != 0 {
		t.Errorf("post-resume re-prime must be exempt, got %d spikes", ts.RedundantContextSpikes)
	}

	// Control: strip the resume boundary markers and the same 90k write is a
	// genuine spike, proving the exemption is what suppressed it.
	noMarkers := []string{lines[0], lines[1], lines[4]}
	ctrlPath := filepath.Join(dir, "no_resume.jsonl")
	if err := os.WriteFile(ctrlPath, []byte(strings.Join(noMarkers, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write control fixture: %v", err)
	}
	ctrl, err := parseSessionTokenSignals(ctrlPath)
	if err != nil {
		t.Fatalf("parseSessionTokenSignals(control): %v", err)
	}
	if ctrl.RedundantContextSpikes != 1 {
		t.Errorf("without a resume marker the 90k write must spike, got %d", ctrl.RedundantContextSpikes)
	}
}

func TestExtractResponseCost_Found(t *testing.T) {
	entry := map[string]json.RawMessage{
		"response_cost": json.RawMessage(`0.0125`),
	}
	cost, ok := extractResponseCost(entry)
	if !ok {
		t.Error("expected response_cost to be found")
	}
	if math.Abs(cost-0.0125) > 0.0001 {
		t.Errorf("expected 0.0125, got %f", cost)
	}
}

func TestExtractResponseCost_LiteLLMField(t *testing.T) {
	entry := map[string]json.RawMessage{
		"_litellm_response_cost": json.RawMessage(`0.05`),
	}
	cost, ok := extractResponseCost(entry)
	if !ok {
		t.Error("expected _litellm_response_cost to be found")
	}
	if math.Abs(cost-0.05) > 0.0001 {
		t.Errorf("expected 0.05, got %f", cost)
	}
}

func TestExtractResponseCost_NotFound(t *testing.T) {
	entry := map[string]json.RawMessage{
		"type": json.RawMessage(`"result"`),
	}
	_, ok := extractResponseCost(entry)
	if ok {
		t.Error("expected response_cost not found")
	}
}

func TestExtractResponseCost_ZeroIsNotUsed(t *testing.T) {
	entry := map[string]json.RawMessage{
		"response_cost": json.RawMessage(`0`),
	}
	_, ok := extractResponseCost(entry)
	if ok {
		t.Error("zero cost should not be used")
	}
}

func TestAccumulateUsage_ResponseCost(t *testing.T) {
	// When response_cost is present, it should be used.
	entry := map[string]json.RawMessage{
		"model":         json.RawMessage(`"claude-opus-4-6"`),
		"response_cost": json.RawMessage(`0.42`),
		"usage":         json.RawMessage(`{"input_tokens":1000000,"output_tokens":100000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`),
	}

	var usage beadsapi.SessionUsage
	accumulateUsage(&usage, entry)

	if math.Abs(usage.TotalCostUSD-0.42) > 0.001 {
		t.Errorf("response_cost should be used, expected $0.42, got $%.6f", usage.TotalCostUSD)
	}

	// Tokens should still be accumulated.
	if usage.InputTokens != 1000000 {
		t.Errorf("input_tokens: expected 1000000, got %d", usage.InputTokens)
	}
}

func TestAccumulateUsage_NoResponseCost(t *testing.T) {
	// Without response_cost, cost should be 0 (no token-based fallback).
	entry := map[string]json.RawMessage{
		"model": json.RawMessage(`"claude-opus-4-6"`),
		"usage": json.RawMessage(`{"input_tokens":1000000,"output_tokens":100000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`),
	}

	var usage beadsapi.SessionUsage
	accumulateUsage(&usage, entry)

	if usage.TotalCostUSD != 0 {
		t.Errorf("cost should be 0 without response_cost, got $%.6f", usage.TotalCostUSD)
	}

	// Tokens should still be accumulated.
	if usage.InputTokens != 1000000 {
		t.Errorf("input_tokens: expected 1000000, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 100000 {
		t.Errorf("output_tokens: expected 100000, got %d", usage.OutputTokens)
	}
}

func TestParseSessionUsage_LiteLLMResponseCost(t *testing.T) {
	// Entries with response_cost should use that for cost.
	lines := []string{
		`{"type":"assistant","model":"claude-opus-4-6","response_cost":0.25,"message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5000,"output_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":3000}}}`,
		`{"type":"result","response_cost":0.10,"usage":{"input_tokens":2000,"output_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":1000}}`,
	}

	f, err := os.CreateTemp("", "session-litellm-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	usage, err := parseSessionUsage(f.Name())
	if err != nil {
		t.Fatalf("parseSessionUsage: %v", err)
	}

	// Cost should be sum of response_cost values: 0.25 + 0.10 = 0.35.
	if math.Abs(usage.TotalCostUSD-0.35) > 0.001 {
		t.Errorf("litellm cost: expected $0.35, got $%.6f", usage.TotalCostUSD)
	}

	// Token counts should still be accumulated regardless of cost source.
	if usage.InputTokens != 7000 {
		t.Errorf("input_tokens: expected 7000, got %d", usage.InputTokens)
	}
	if usage.RequestCount != 2 {
		t.Errorf("request_count: expected 2, got %d", usage.RequestCount)
	}
}

func TestParseSessionUsage_MixedCostAndNoCost(t *testing.T) {
	// Some entries have response_cost, some don't. Only entries with
	// response_cost contribute to TotalCostUSD.
	lines := []string{
		`{"type":"assistant","model":"claude-opus-4-6","response_cost":0.50,"message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5000,"output_tokens":200}}}`,
		`{"type":"result","usage":{"input_tokens":2000,"output_tokens":100}}`,
	}

	f, err := os.CreateTemp("", "session-mixed-cost-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	usage, err := parseSessionUsage(f.Name())
	if err != nil {
		t.Fatalf("parseSessionUsage: %v", err)
	}

	// Only the first entry has response_cost.
	if math.Abs(usage.TotalCostUSD-0.50) > 0.001 {
		t.Errorf("cost: expected $0.50, got $%.6f", usage.TotalCostUSD)
	}

	// Both entries contribute tokens.
	if usage.InputTokens != 7000 {
		t.Errorf("input_tokens: expected 7000, got %d", usage.InputTokens)
	}
	if usage.RequestCount != 2 {
		t.Errorf("request_count: expected 2, got %d", usage.RequestCount)
	}
}

// ── Token signals (--token-signals) ──────────────────────────────────────

func TestExtractUsageJSON_TopLevel(t *testing.T) {
	entry := map[string]json.RawMessage{
		"usage": json.RawMessage(`{"input_tokens":100}`),
	}
	raw, ok := extractUsageJSON(entry)
	if !ok {
		t.Fatal("expected top-level usage to be found")
	}
	if !strings.Contains(string(raw), "input_tokens") {
		t.Errorf("unexpected usage json: %s", raw)
	}
}

func TestExtractUsageJSON_NestedInMessage(t *testing.T) {
	entry := map[string]json.RawMessage{
		"message": json.RawMessage(`{"role":"assistant","usage":{"input_tokens":42}}`),
	}
	raw, ok := extractUsageJSON(entry)
	if !ok {
		t.Fatal("expected message.usage to be found")
	}
	if !strings.Contains(string(raw), "42") {
		t.Errorf("unexpected usage json: %s", raw)
	}
}

func TestExtractUsageJSON_Missing(t *testing.T) {
	entry := map[string]json.RawMessage{"type": json.RawMessage(`"result"`)}
	if _, ok := extractUsageJSON(entry); ok {
		t.Error("expected no usage block")
	}
}

func TestParseTurn_TopLevelAndEphemeral(t *testing.T) {
	// Mirrors the real Claude Code usage block shape (nested cache_creation).
	entry := map[string]json.RawMessage{
		"usage": json.RawMessage(`{
			"input_tokens":3622,
			"output_tokens":495,
			"cache_read_input_tokens":46986,
			"cache_creation_input_tokens":30386,
			"cache_creation":{"ephemeral_1h_input_tokens":30386,"ephemeral_5m_input_tokens":0}
		}`),
	}
	turn, ok := parseTurn(entry)
	if !ok {
		t.Fatal("expected a turn")
	}
	if turn.Input != 3622 || turn.Output != 495 {
		t.Errorf("input/output: got %d/%d", turn.Input, turn.Output)
	}
	if turn.CacheRead != 46986 || turn.CacheCreate != 30386 {
		t.Errorf("cache read/create: got %d/%d", turn.CacheRead, turn.CacheCreate)
	}
	if turn.Ephemeral1h != 30386 || turn.Ephemeral5m != 0 {
		t.Errorf("ephemeral 1h/5m: got %d/%d", turn.Ephemeral1h, turn.Ephemeral5m)
	}
}

func TestParseTurn_NestedInMessage(t *testing.T) {
	entry := map[string]json.RawMessage{
		"message": json.RawMessage(`{"role":"assistant","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}`),
	}
	turn, ok := parseTurn(entry)
	if !ok {
		t.Fatal("expected a turn")
	}
	if turn.Input != 10 || turn.Output != 20 || turn.CacheRead != 30 || turn.CacheCreate != 40 {
		t.Errorf("unexpected turn: %+v", turn)
	}
}

func TestParseTurn_NoUsage(t *testing.T) {
	entry := map[string]json.RawMessage{"type": json.RawMessage(`"summary"`)}
	if _, ok := parseTurn(entry); ok {
		t.Error("expected no turn for entry without usage")
	}
}

func TestExtractMessageID(t *testing.T) {
	withID := map[string]json.RawMessage{
		"message": json.RawMessage(`{"id":"msg_123","role":"assistant"}`),
	}
	if id := extractMessageID(withID); id != "msg_123" {
		t.Errorf("expected msg_123, got %q", id)
	}
	// Result entries have no message.id.
	noMsg := map[string]json.RawMessage{"type": json.RawMessage(`"result"`)}
	if id := extractMessageID(noMsg); id != "" {
		t.Errorf("expected empty id for result entry, got %q", id)
	}
	// Message without id field.
	noID := map[string]json.RawMessage{"message": json.RawMessage(`{"role":"assistant"}`)}
	if id := extractMessageID(noID); id != "" {
		t.Errorf("expected empty id, got %q", id)
	}
}

func TestBuildTokenSignals_Empty(t *testing.T) {
	ts := buildTokenSignals(nil, 0)
	if ts.TurnCount != 0 {
		t.Errorf("turn_count: expected 0, got %d", ts.TurnCount)
	}
	if ts.CacheHitRatio != 0 || ts.OutputPerRequest != 0 || ts.PeakContextTokens != 0 {
		t.Error("expected zero-valued signals for empty session")
	}
}

func TestBuildTokenSignals_RatiosAndPeak(t *testing.T) {
	turns := []turnUsage{
		{Input: 1000, Output: 100, CacheRead: 0, CacheCreate: 500},
		{Input: 0, Output: 300, CacheRead: 4000, CacheCreate: 0},
	}
	ts := buildTokenSignals(turns, 0)

	if ts.TurnCount != 2 {
		t.Errorf("turn_count: expected 2, got %d", ts.TurnCount)
	}
	// cache_read=4000, input=1000, cache_creation=500 => 4000/5500 = 0.7272...
	if math.Abs(ts.CacheHitRatio-0.72727) > 0.0001 {
		t.Errorf("cache_hit_ratio: expected ~0.72727, got %f", ts.CacheHitRatio)
	}
	// output total 400 / 2 turns = 200.
	if math.Abs(ts.OutputPerRequest-200) > 0.001 {
		t.Errorf("output_per_request: expected 200, got %f", ts.OutputPerRequest)
	}
	// peak = max(1000+0+500=1500, 0+4000+0=4000) = 4000.
	if ts.PeakContextTokens != 4000 {
		t.Errorf("peak_context_tokens: expected 4000, got %d", ts.PeakContextTokens)
	}
	// arrays preserve per-turn order.
	if len(ts.CacheReadTokens) != 2 || ts.CacheReadTokens[1] != 4000 {
		t.Errorf("cache_read array: %v", ts.CacheReadTokens)
	}
}

func TestBuildTokenSignals_RedundantSpikes(t *testing.T) {
	// A steady baseline of 20k, then a 60k write: > 2× the running median 20k AND
	// over the minSpikeTokens floor, so it is a real spike on both tests.
	turns := []turnUsage{
		{CacheCreate: 20000}, {CacheCreate: 20000}, {CacheCreate: 20000}, {CacheCreate: 60000},
	}
	ts := buildTokenSignals(turns, 0)
	if ts.RedundantContextSpikes != 1 {
		t.Errorf("redundant_context_spikes: expected 1, got %d", ts.RedundantContextSpikes)
	}
}

// TestBuildTokenSignals_SmallWritesAreNotSpikes is the regression for bug
// ka-0000003202: the >2×-running-median test is scale-free, so on a session
// whose typical rebuild is small an ordinary source-file read used to count as a
// "re-prime balloon". These writes are all disproportionate (>2× the ~1.5k
// median) but none clears minSpikeTokens, so none is a spike.
//
// The magnitudes are the real ones measured on session ka-0000005126/7622b506:
// a 0.9909-cache-hit feature build whose Efficiency dimension was capped on 4
// spikes of 3687/5153/5278/5434 tokens against a ~1.4k–1.8k median write.
func TestBuildTokenSignals_SmallWritesAreNotSpikes(t *testing.T) {
	turns := []turnUsage{
		{CacheCreate: 1487}, {CacheCreate: 1713}, {CacheCreate: 1772}, {CacheCreate: 1541},
		{CacheCreate: 3687}, // >2× median, but under the floor
		{CacheCreate: 5153}, // ditto
		{CacheCreate: 5278}, // ditto
		{CacheCreate: 5434}, // ditto
	}
	ts := buildTokenSignals(turns, 0)
	if ts.RedundantContextSpikes != 0 {
		t.Errorf("redundant_context_spikes: expected 0 (all writes under the %d floor), got %d",
			minSpikeTokens, ts.RedundantContextSpikes)
	}
}

// TestBuildTokenSignals_SpikeFloorBoundary pins minSpikeTokens as an inclusive
// floor (>=): a write exactly at the floor that also clears 2× the median is a
// spike; one token below it is not.
func TestBuildTokenSignals_SpikeFloorBoundary(t *testing.T) {
	baseline := []turnUsage{{CacheCreate: 1000}, {CacheCreate: 1000}}

	atFloor := buildTokenSignals(append(baseline, turnUsage{CacheCreate: minSpikeTokens}), 0)
	if atFloor.RedundantContextSpikes != 1 {
		t.Errorf("write exactly at the floor must count (>=), got %d", atFloor.RedundantContextSpikes)
	}

	belowFloor := buildTokenSignals(append(baseline, turnUsage{CacheCreate: minSpikeTokens - 1}), 0)
	if belowFloor.RedundantContextSpikes != 0 {
		t.Errorf("write one token below the floor must not count, got %d", belowFloor.RedundantContextSpikes)
	}
}

// TestBuildTokenSignals_LargeWritesStillNeedDisproportion is the other half of
// the AND: clearing the absolute floor is not sufficient on its own. A session
// whose every write is legitimately large (well over minSpikeTokens) but uniform
// has no spike, because nothing exceeds 2× the running median.
func TestBuildTokenSignals_LargeWritesStillNeedDisproportion(t *testing.T) {
	turns := []turnUsage{
		{CacheCreate: 40000}, {CacheCreate: 41000}, {CacheCreate: 39000}, {CacheCreate: 40500},
	}
	ts := buildTokenSignals(turns, 0)
	if ts.RedundantContextSpikes != 0 {
		t.Errorf("uniform large writes must not spike, got %d", ts.RedundantContextSpikes)
	}
}

// TestBuildTokenSignals_SpikeBaselineIsNonzeroWrites is the regression for the
// high-cache-hit blind spot: a session that cache-hits (cache_creation==0) most
// turns but re-primes with a typical ~50k write, then balloons to 500k. The
// balloon must be flagged even though zero-writes dominate the series — because
// the baseline is the median of prior NONZERO writes, not of all prior turns.
func TestBuildTokenSignals_SpikeBaselineIsNonzeroWrites(t *testing.T) {
	turns := []turnUsage{
		{CacheCreate: 50000},               // first write: no baseline yet, not flagged
		{CacheCreate: 0}, {CacheCreate: 0}, // cache-hitting turns
		{CacheCreate: 50000}, // == median 50000, not > 2×, not flagged
		{CacheCreate: 0}, {CacheCreate: 0},
		{CacheCreate: 500000}, // balloon: 500k > 2×50k → the one real spike
	}
	ts := buildTokenSignals(turns, 0)
	if ts.RedundantContextSpikes != 1 {
		t.Errorf("redundant_context_spikes: expected 1 (the 500k balloon), got %d", ts.RedundantContextSpikes)
	}
}

func TestBuildTokenSignals_EqualReprimesAreNotSpikes(t *testing.T) {
	// Equal-size re-primes are the normal steady state, not spikes: each write
	// equals the running median, so none exceeds 2× it.
	turns := []turnUsage{
		{CacheCreate: 50000}, {CacheCreate: 0}, {CacheCreate: 50000}, {CacheCreate: 0}, {CacheCreate: 50000},
	}
	ts := buildTokenSignals(turns, 0)
	if ts.RedundantContextSpikes != 0 {
		t.Errorf("redundant_context_spikes: expected 0 for equal re-primes, got %d", ts.RedundantContextSpikes)
	}
}

// TestBuildTokenSignals_ResumeReprimeIsNotASpike is the regression pin for bug
// gd-bpJOr1dotX: the first cache-creation after a cron-wake resume boundary
// re-caches the continuing conversation whose 5-min ephemeral cache expired
// across the idle gap. Its write is large and disproportionate (both spike
// tests fire) but it is legitimate cold-cache cost, not a redundant re-fetch, so
// a turn flagged IsResumeReprime must not count as a spike.
func TestBuildTokenSignals_ResumeReprimeIsNotASpike(t *testing.T) {
	// A steady 20k baseline, then a 60k re-prime that is flagged as a resume
	// re-prime: >2× median and over the floor, but exempt.
	turns := []turnUsage{
		{CacheCreate: 20000}, {CacheCreate: 20000}, {CacheCreate: 20000},
		{CacheCreate: 60000, IsResumeReprime: true},
	}
	ts := buildTokenSignals(turns, 0)
	if ts.RedundantContextSpikes != 0 {
		t.Errorf("redundant_context_spikes: resume re-prime must be exempt, got %d", ts.RedundantContextSpikes)
	}

	// Control: the identical write NOT flagged is a genuine spike, proving the
	// exemption — not some other guard — is what suppresses it.
	turns[3].IsResumeReprime = false
	if got := buildTokenSignals(turns, 0).RedundantContextSpikes; got != 1 {
		t.Errorf("the same unflagged write must spike, got %d", got)
	}
}

// TestBuildTokenSignals_ResumeReprimeStaysOutOfMedian pins the second half of the
// fix: an exempt re-prime is also kept out of the running-median baseline, so a
// genuine mid-run balloon that follows is still judged against the representative
// per-turn rebuild size — not against an inflated median the re-prime would have
// created.
func TestBuildTokenSignals_ResumeReprimeStaysOutOfMedian(t *testing.T) {
	turns := []turnUsage{
		{CacheCreate: 20000}, {CacheCreate: 20000},
		{CacheCreate: 120000, IsResumeReprime: true}, // exempt; must not enter the median
		{CacheCreate: 60000},                         // 3× the 20k median => genuine spike
	}
	ts := buildTokenSignals(turns, 0)
	if ts.RedundantContextSpikes != 1 {
		t.Errorf("a real mid-run balloon after a re-prime must still spike against the 20k median, got %d",
			ts.RedundantContextSpikes)
	}
}

func TestBuildTokenSignals_EphemeralSplit(t *testing.T) {
	turns := []turnUsage{
		{CacheCreate: 30386, Ephemeral1h: 30386, Ephemeral5m: 0},
		{CacheCreate: 5000, Ephemeral1h: 0, Ephemeral5m: 5000},
	}
	ts := buildTokenSignals(turns, 0)
	if ts.EphemeralCacheCreation.Ephemeral1hInputTokens != 30386 {
		t.Errorf("ephemeral_1h: expected 30386, got %d", ts.EphemeralCacheCreation.Ephemeral1hInputTokens)
	}
	if ts.EphemeralCacheCreation.Ephemeral5mInputTokens != 5000 {
		t.Errorf("ephemeral_5m: expected 5000, got %d", ts.EphemeralCacheCreation.Ephemeral5mInputTokens)
	}
}

func TestBuildTokenSignals_CompactionCountPassthrough(t *testing.T) {
	ts := buildTokenSignals([]turnUsage{{Input: 1}}, 3)
	if ts.CompactionCount != 3 {
		t.Errorf("compaction_count: expected 3, got %d", ts.CompactionCount)
	}
}

func TestMedianInt64(t *testing.T) {
	if m := medianInt64(nil); m != 0 {
		t.Errorf("empty median: expected 0, got %f", m)
	}
	if m := medianInt64([]int64{5}); m != 5 {
		t.Errorf("single median: expected 5, got %f", m)
	}
	if m := medianInt64([]int64{3, 1, 2}); m != 2 {
		t.Errorf("odd median: expected 2, got %f", m)
	}
	if m := medianInt64([]int64{1, 2, 3, 4}); m != 2.5 {
		t.Errorf("even median: expected 2.5, got %f", m)
	}
	// Ensure the input slice is not mutated (defensive copy).
	src := []int64{3, 1, 2}
	_ = medianInt64(src)
	if src[0] != 3 || src[1] != 1 || src[2] != 2 {
		t.Errorf("medianInt64 mutated its input: %v", src)
	}
}

// TestParseSessionTokenSignals_GardenerSweepReproduction reproduces the live
// gardener-sweep totals cited on epic ka-0000003617: cache_read 3,593,355 /
// output 81,278 / cache_creation 300,004, with a fresh input tuned so the
// cache-hit ratio lands at ≈ 0.917.
func TestParseSessionTokenSignals_GardenerSweepReproduction(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":25241,"output_tokens":1278,"cache_read_input_tokens":593355,"cache_creation_input_tokens":300004,"cache_creation":{"ephemeral_1h_input_tokens":300004,"ephemeral_5m_input_tokens":0}}}}`,
		`{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":0,"output_tokens":80000,"cache_read_input_tokens":3000000,"cache_creation_input_tokens":0}}}`,
		`{"type":"summary","summary":"Context was summarized."}`,
	}
	f := writeTempJSONL(t, lines)
	defer os.Remove(f)

	ts, err := parseSessionTokenSignals(f)
	if err != nil {
		t.Fatalf("parseSessionTokenSignals: %v", err)
	}

	var sumRead, sumOut, sumCreate int64
	for _, v := range ts.CacheReadTokens {
		sumRead += v
	}
	for _, v := range ts.OutputTokens {
		sumOut += v
	}
	for _, v := range ts.CacheCreationTokens {
		sumCreate += v
	}
	if sumRead != 3593355 {
		t.Errorf("cache_read total: expected 3593355, got %d", sumRead)
	}
	if sumOut != 81278 {
		t.Errorf("output total: expected 81278, got %d", sumOut)
	}
	if sumCreate != 300004 {
		t.Errorf("cache_creation total: expected 300004, got %d", sumCreate)
	}
	if math.Abs(ts.CacheHitRatio-0.917) > 0.001 {
		t.Errorf("cache_hit_ratio: expected ≈0.917, got %f", ts.CacheHitRatio)
	}
	if ts.TurnCount != 2 {
		t.Errorf("turn_count: expected 2, got %d", ts.TurnCount)
	}
	if ts.PeakContextTokens != 3000000 {
		t.Errorf("peak_context_tokens: expected 3000000, got %d", ts.PeakContextTokens)
	}
	if math.Abs(ts.OutputPerRequest-40639) > 0.001 {
		t.Errorf("output_per_request: expected 40639, got %f", ts.OutputPerRequest)
	}
	if ts.CompactionCount != 1 {
		t.Errorf("compaction_count: expected 1, got %d", ts.CompactionCount)
	}
	if ts.EphemeralCacheCreation.Ephemeral1hInputTokens != 300004 {
		t.Errorf("ephemeral_1h: expected 300004, got %d", ts.EphemeralCacheCreation.Ephemeral1hInputTokens)
	}
}

func TestParseSessionTokenSignals_FileNotFound(t *testing.T) {
	if _, err := parseSessionTokenSignals("/nonexistent/path.jsonl"); err == nil {
		t.Error("expected error for nonexistent file")
	}
}

// TestParseSessionTokenSignals_DedupesMultiBlockMessage is the blocker
// regression: Claude Code writes one assistant message (one API request) across
// multiple JSONL lines — one per content block — each repeating the identical
// message.usage. The per-turn pass must collapse them by message.id into a
// single turn, so counts and totals reflect requests, not logged blocks.
func TestParseSessionTokenSignals_DedupesMultiBlockMessage(t *testing.T) {
	// One assistant message (msg_A) spans 3 content-block lines with identical
	// usage; a second message (msg_B) is a single line; a result entry has no id.
	lines := []string{
		`{"type":"assistant","message":{"id":"msg_A","role":"assistant","content":[{"type":"thinking"}],"usage":{"input_tokens":100,"output_tokens":40,"cache_read_input_tokens":900,"cache_creation_input_tokens":0}}}`,
		`{"type":"assistant","message":{"id":"msg_A","role":"assistant","content":[{"type":"text"}],"usage":{"input_tokens":100,"output_tokens":40,"cache_read_input_tokens":900,"cache_creation_input_tokens":0}}}`,
		`{"type":"assistant","message":{"id":"msg_A","role":"assistant","content":[{"type":"tool_use"}],"usage":{"input_tokens":100,"output_tokens":40,"cache_read_input_tokens":900,"cache_creation_input_tokens":0}}}`,
		`{"type":"assistant","message":{"id":"msg_B","role":"assistant","content":[{"type":"text"}],"usage":{"input_tokens":50,"output_tokens":20,"cache_read_input_tokens":500,"cache_creation_input_tokens":0}}}`,
		`{"type":"result","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":0}}`,
	}
	f := writeTempJSONL(t, lines)
	defer os.Remove(f)

	ts, err := parseSessionTokenSignals(f)
	if err != nil {
		t.Fatalf("parseSessionTokenSignals: %v", err)
	}

	// 3 distinct requests: msg_A (collapsed), msg_B, and the result entry.
	if ts.TurnCount != 3 {
		t.Errorf("turn_count: expected 3 (msg_A collapsed + msg_B + result), got %d", ts.TurnCount)
	}
	// Output must be 40 (msg_A once) + 20 + 5 = 65, NOT 40*3 + 20 + 5 = 145.
	var sumOut int64
	for _, v := range ts.OutputTokens {
		sumOut += v
	}
	if sumOut != 65 {
		t.Errorf("output total: expected 65 (msg_A counted once), got %d", sumOut)
	}
	if len(ts.OutputTokens) != 3 {
		t.Errorf("output array length: expected 3, got %d", len(ts.OutputTokens))
	}
}

func TestFileExists(t *testing.T) {
	// Existing file should return true.
	f, err := os.CreateTemp("", "fileexists-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	if !fileExists(f.Name()) {
		t.Error("fileExists should return true for existing file")
	}

	// Nonexistent file should return false.
	if fileExists("/nonexistent/path/file.txt") {
		t.Error("fileExists should return false for nonexistent file")
	}

	// Directory should return false.
	dir := t.TempDir()
	if fileExists(dir) {
		t.Error("fileExists should return false for directories")
	}
}
