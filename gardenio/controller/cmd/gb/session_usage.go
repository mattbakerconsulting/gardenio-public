package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// parseSessionUsage reads a Claude Code JSONL session log and aggregates
// usage metrics: token counts, cost, tool calls, errors, and compactions.
//
// One API request = one assistant message, but Claude Code writes that message
// across MULTIPLE JSONL lines — one per content block (thinking / text / each
// tool_use) — and every one of those lines repeats the identical message.usage.
// So the usage/cost/request accumulation is deduped by message.id (keeping the
// first usage-bearing line per distinct assistant message), exactly as the
// per-turn signals pass does (parseSessionTokenSignals). Without this, the token
// totals and RequestCount over-count by the content-block fan-out (~2-5× on real
// transcripts). Result entries carry no message.id and each counts as its own
// request. This dedup applies ONLY to accumulateUsage; the per-block/per-entry
// counters (countToolCalls / countThinkingTokens / countErrors /
// countCompactions) stay per-line because each line carries a DISTINCT content
// block — deduping them would under-count tool calls.
func parseSessionUsage(logPath string) (*beadsapi.SessionUsage, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("opening session log: %w", err)
	}
	defer f.Close()

	var usage beadsapi.SessionUsage
	seen := make(map[string]bool) // message.ids already accumulated

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024) // 1MB max line

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry map[string]json.RawMessage
		if err := json.Unmarshal(line, &entry); err != nil {
			continue // skip malformed lines
		}

		// Extract usage from assistant messages and result entries, collapsing
		// the repeated content-block lines of one assistant message (same
		// message.id, identical usage) into a single request. Entries with no id
		// (result entries) each count once.
		if id := extractMessageID(entry); id != "" {
			if !seen[id] {
				seen[id] = true
				accumulateUsage(&usage, entry)
			}
		} else {
			accumulateUsage(&usage, entry)
		}

		// Count thinking tokens from assistant message content blocks.
		countThinkingTokens(&usage, entry)

		// Count tool calls.
		countToolCalls(&usage, entry)

		// Count errors.
		countErrors(&usage, entry)

		// Count compactions.
		countCompactions(&usage, entry)

		// Count user interrupts.
		countInterrupts(&usage, entry)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning session log: %w", err)
	}

	return &usage, nil
}

// extractModel reads the model ID from a JSONL entry. It checks top-level
// "model" first, then "message.model".
func extractModel(entry map[string]json.RawMessage) string {
	if raw, ok := entry["model"]; ok {
		var m string
		if json.Unmarshal(raw, &m) == nil && m != "" {
			return m
		}
	}
	if msgRaw, ok := entry["message"]; ok {
		var msg struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(msgRaw, &msg) == nil && msg.Model != "" {
			return msg.Model
		}
	}
	return ""
}

// isResumeBoundary reports whether a JSONL entry is a session resume-boundary
// marker — the harness emits one on each cron re-fire of a resume-in-place
// gardener. Two equivalent shapes, either is sufficient (verified present 6× on
// the resumed control ka-0000005428/98c9b8f5 and 0× on non-resumed controls):
//
//   - Structural (preferred, prose-independent): an assistant entry whose model
//     is "<synthetic>" — the "No response requested." placeholder the harness
//     writes between the "Continue from where you left off." user message and the
//     fresh re-prime turn.
//   - Corroborating: an isMeta user message whose text contains
//     "Continue from where you left off".
//
// The turn immediately following such a marker re-primes a cold ephemeral cache;
// its large cache_creation is legitimate resume cost, not redundant re-fetch, so
// the spike detector exempts it (see parseSessionTokenSignals / buildTokenSignals).
func isResumeBoundary(entry map[string]json.RawMessage) bool {
	if extractModel(entry) == "<synthetic>" {
		return true
	}
	return isMetaContinueMarker(entry)
}

// isMetaContinueMarker reports whether entry is an isMeta user message whose
// content contains "Continue from where you left off" — the corroborating resume
// signal. Content may be a plain string or an array of content blocks.
func isMetaContinueMarker(entry map[string]json.RawMessage) bool {
	var isMeta bool
	if raw, ok := entry["isMeta"]; ok {
		_ = json.Unmarshal(raw, &isMeta)
	}
	if !isMeta {
		return false
	}
	msgRaw, ok := entry["message"]
	if !ok {
		return false
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msgRaw, &msg) != nil {
		return false
	}
	return strings.Contains(string(msg.Content), "Continue from where you left off")
}

// extractResponseCost checks if a JSONL entry contains a LiteLLM-provided
// response cost field ("response_cost" or "_litellm_response_cost"). Returns
// the cost and true if found, or 0 and false otherwise.
func extractResponseCost(entry map[string]json.RawMessage) (float64, bool) {
	for _, key := range []string{"response_cost", "_litellm_response_cost"} {
		if raw, ok := entry[key]; ok {
			var cost float64
			if json.Unmarshal(raw, &cost) == nil && cost > 0 {
				return cost, true
			}
		}
	}
	return 0, false
}

// accumulateUsage extracts token counts and cost from a JSONL entry.
// Claude Code session logs contain "usage" at the top level (result entries)
// or nested under "message.usage" (assistant entries).
//
// Cost here is the in-pod estimate: it sums LiteLLM's per-turn response_cost
// field (extractResponseCost) straight out of the session JSONL — never
// tokens×rate. Entries without response_cost contribute zero cost. This feeds
// SessionUsage.TotalCostUSD for the `gb session` displays and session-bead
// writes, but it is NOT the authoritative total. The controller overwrites the
// authoritative figures from LiteLLM directly: per-session agent cost via
// reconcile_session_spend.go (GET /spend/tags) and per-account totals via
// spend_sync.go (GET /key/info + /spend/logs), which supersede this estimate
// at session close.
func accumulateUsage(usage *beadsapi.SessionUsage, entry map[string]json.RawMessage) {
	usageJSON, ok := extractUsageJSON(entry)
	if !ok {
		return
	}

	var u struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	}
	if json.Unmarshal(usageJSON, &u) != nil {
		return
	}

	usage.InputTokens += u.InputTokens
	usage.OutputTokens += u.OutputTokens
	usage.CacheReadTokens += u.CacheReadInputTokens
	usage.CacheWriteTokens += u.CacheCreationInputTokens

	// Live context-window occupancy: a LEVEL, not a sum — the latest
	// usage-bearing request wins (mirrors coop usage.rs::accumulate).
	residentContext := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	usage.ContextTokens = residentContext

	// Boot context: the resident context of the FIRST usage-bearing request —
	// the tokenized boot size (system prompt + MCP tool defs + skills + CLAUDE.md
	// + gb prime + thread ctx) paid before any work (epic ka-0000003733). Set
	// once, on RequestCount==0 (before the increment below), so it captures turn
	// 0 and is never overwritten by later turns. Callers dedupe by message.id
	// before calling accumulateUsage, so the first call here is genuinely turn 0.
	if usage.RequestCount == 0 {
		usage.BootContextTokens = residentContext
	}

	usage.RequestCount++

	// Use LiteLLM-reported cost if available.
	if cost, ok := extractResponseCost(entry); ok {
		usage.TotalCostUSD += cost
	}
}

// extractUsageJSON locates the usage block within a JSONL entry. Claude Code
// session logs carry "usage" at the top level (result entries) or nested under
// "message.usage" (assistant entries). Returns the raw usage JSON and true if
// found. This is the shared lookup used by both the aggregate accumulator
// (accumulateUsage) and the per-turn pass (parseTurn).
func extractUsageJSON(entry map[string]json.RawMessage) (json.RawMessage, bool) {
	if raw, ok := entry["usage"]; ok {
		return raw, true
	}
	if msgRaw, ok := entry["message"]; ok {
		var msg map[string]json.RawMessage
		if json.Unmarshal(msgRaw, &msg) == nil {
			if raw, ok := msg["usage"]; ok {
				return raw, true
			}
		}
	}
	return nil, false
}

// countToolCalls counts tool_use content blocks in assistant messages.
func countToolCalls(usage *beadsapi.SessionUsage, entry map[string]json.RawMessage) {
	msgRaw, ok := entry["message"]
	if !ok {
		return
	}
	var msg struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msgRaw, &msg) != nil || msg.Role != "assistant" {
		return
	}
	for _, c := range msg.Content {
		var block struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(c, &block) == nil && block.Type == "tool_use" {
			usage.ToolCallCount++
		}
	}
}

// countErrors counts tool_result entries with is_error=true.
func countErrors(usage *beadsapi.SessionUsage, entry map[string]json.RawMessage) {
	// Check for "type": "result" with "is_error": true.
	typeRaw, ok := entry["type"]
	if !ok {
		return
	}
	var entryType string
	if json.Unmarshal(typeRaw, &entryType) != nil {
		return
	}
	if entryType == "result" {
		if errRaw, ok := entry["is_error"]; ok {
			var isError bool
			if json.Unmarshal(errRaw, &isError) == nil && isError {
				usage.ErrorCount++
			}
		}
	}
}

// countCompactions counts context compaction events. Current Claude Code
// writes one {"type":"system","subtype":"compact_boundary"} line per
// compaction (auto or manual), with compactMetadata.trigger/preTokens.
// The paired isCompactSummary user message is deliberately NOT counted —
// every boundary is followed by one, so counting both would double-count
// (ka-0000004139: the boundary subtype was unhandled, so compaction_count
// read 0 fleet-wide even on transcripts with real auto-compactions).
// The older formats — a top-level "summary" entry, or a system message
// whose message.content mentions "summarized"/"compacted" — are kept so
// archived transcripts still measure.
func countCompactions(usage *beadsapi.SessionUsage, entry map[string]json.RawMessage) {
	typeRaw, ok := entry["type"]
	if !ok {
		return
	}
	var entryType string
	if json.Unmarshal(typeRaw, &entryType) != nil {
		return
	}
	if entryType == "summary" {
		usage.CompactionCount++
		return
	}

	if entryType == "system" {
		if subRaw, ok := entry["subtype"]; ok {
			var subtype string
			if json.Unmarshal(subRaw, &subtype) == nil && subtype == "compact_boundary" {
				usage.CompactionCount++
				return
			}
		}
	}

	// Also check for system messages about compaction.
	msgRaw, ok := entry["message"]
	if !ok {
		return
	}
	var msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if json.Unmarshal(msgRaw, &msg) == nil && msg.Role == "system" {
		if strings.Contains(msg.Content, "summarized") || strings.Contains(msg.Content, "compacted") {
			usage.CompactionCount++
		}
	}
}

// interruptMarker starts the user-role text Claude Code writes when the user
// interrupts a turn: "[Request interrupted by user]", or "[Request interrupted
// by user for tool use]" beside a rejected tool call.
const interruptMarker = "[Request interrupted by user"

// countInterrupts counts interrupt markers and keeps the last one's timestamp.
// Only a user message's own text block (or string content) that starts with
// the marker counts, so a tool_result that merely prints the string (an agent
// grepping coop's parser) does not. Nothing of the text is kept.
func countInterrupts(usage *beadsapi.SessionUsage, entry map[string]json.RawMessage) {
	var entryType string
	var isMeta bool
	if json.Unmarshal(entry["type"], &entryType) != nil || entryType != "user" {
		return
	}
	if raw, ok := entry["isMeta"]; ok && json.Unmarshal(raw, &isMeta) == nil && isMeta {
		return
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(entry["message"], &msg) != nil {
		return
	}
	interrupted := false
	var text string
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	switch {
	case json.Unmarshal(msg.Content, &text) == nil:
		interrupted = strings.HasPrefix(strings.TrimSpace(text), interruptMarker)
	case json.Unmarshal(msg.Content, &blocks) == nil:
		for _, b := range blocks {
			if b.Type == "text" && strings.HasPrefix(strings.TrimSpace(b.Text), interruptMarker) {
				interrupted = true
				break
			}
		}
	}
	if !interrupted {
		return
	}
	usage.InterruptCount++
	var ts string
	if json.Unmarshal(entry["timestamp"], &ts) == nil && ts != "" {
		usage.LastInterruptAt = ts
	}
}

// countThinkingTokens estimates thinking tokens from assistant message content
// blocks of type "thinking". The Anthropic API includes thinking tokens within
// output_tokens but does not report them separately in the usage block.
// This function parses the thinking content text and estimates tokens at ~4
// characters per token (approximate BPE average for English text).
//
// IMPORTANT: the result is an ESTIMATE, not a measured value, and is
// conceptually double-counted — the real thinking tokens are already inside
// output_tokens. Surfaces that display ThinkingTokens MUST label it as an
// estimate (e.g. "thinking (est)") and MUST NOT sum it into authoritative
// token totals.
func countThinkingTokens(usage *beadsapi.SessionUsage, entry map[string]json.RawMessage) {
	msgRaw, ok := entry["message"]
	if !ok {
		return
	}
	var msg struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msgRaw, &msg) != nil || msg.Role != "assistant" {
		return
	}
	for _, c := range msg.Content {
		var block struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
		}
		if json.Unmarshal(c, &block) == nil && block.Type == "thinking" && len(block.Thinking) > 0 {
			// Estimate ~4 characters per token (BPE average).
			usage.ThinkingTokens += int64(len(block.Thinking)+3) / 4
		}
	}
}

// turnUsage captures the per-turn token counts pulled from a single
// usage-bearing JSONL entry (one assistant message or one result entry). Unlike
// the aggregate SessionUsage, these are kept per-turn so cache-hit ratio,
// peak-context, and redundant-context-spike detection can be derived.
type turnUsage struct {
	Input       int64
	Output      int64
	CacheRead   int64
	CacheCreate int64
	Ephemeral1h int64
	Ephemeral5m int64
	// IsResumeReprime marks the first usage-bearing turn after a resume
	// boundary (a cron-wake "Continue from where you left off" re-fire). Its
	// large cache_creation is the irreducible cost of re-caching the continuing
	// conversation whose 5-min ephemeral cache expired across the idle gap — not
	// a redundant re-fetch — so the spike detector must exclude it (both from the
	// spike test and from the running-median baseline). See buildTokenSignals.
	IsResumeReprime bool
}

// ephemeralSplit breaks cache-creation writes into 1h vs 5m TTL buckets. A
// 1h-heavy split is a real efficiency signal (1h writes bill at a higher
// multiple of the base input rate than 5m), but this struct only reports the
// token split — it never prices it. Authoritative USD stays with LiteLLM.
type ephemeralSplit struct {
	Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
	Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
}

// TokenSignals is the `--token-signals` JSON block: per-turn token arrays plus
// derived efficiency signals. Every field is a token count or a token/token
// ratio — NEVER a USD figure computed from tokens. Authoritative cost stays
// with the LiteLLM response_cost passthrough in SessionUsage.TotalCostUSD; the
// cost-source AST guard (internal/metrics/cost_source_guard_test.go) forbids any
// token×rate math in this tree, so none appears here.
type TokenSignals struct {
	TurnCount              int            `json:"turn_count"`
	InputTokens            []int64        `json:"input_tokens"`
	OutputTokens           []int64        `json:"output_tokens"`
	CacheReadTokens        []int64        `json:"cache_read_tokens"`
	CacheCreationTokens    []int64        `json:"cache_creation_tokens"`
	CacheHitRatio          float64        `json:"cache_hit_ratio"`
	OutputPerRequest       float64        `json:"output_per_request"`
	PeakContextTokens      int64          `json:"peak_context_tokens"`
	CompactionCount        int64          `json:"compaction_count"`
	RedundantContextSpikes int64          `json:"redundant_context_spikes"`
	EphemeralCacheCreation ephemeralSplit `json:"ephemeral_cache_creation"`
}

// parseSessionTokenSignals reads a Claude Code JSONL session log and derives the
// per-turn token signals. It is a separate pass from parseSessionUsage (which
// only keeps aggregate totals for the four existing callers) so those code paths
// are untouched; `gb session analyze --token-signals` runs both.
//
// One API request = one turn. Claude Code writes a single assistant message
// (one request) across MULTIPLE JSONL lines — one per content block (thinking /
// text / each tool_use) — and every one of those lines repeats the identical
// message.usage. Counting per line therefore inflates turn_count and the arrays
// and skews the ratios (verified live: 140 usage-bearing lines vs 57 distinct
// message.id, a 2.9× output over-count). So we dedupe by message.id, keeping the
// first usage-bearing line per distinct assistant message. Result entries carry
// no message.id and each counts as its own request.
func parseSessionTokenSignals(logPath string) (*TokenSignals, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("opening session log: %w", err)
	}
	defer f.Close()

	var turns []turnUsage
	var compactions beadsapi.SessionUsage // reuse countCompactions for the count
	seen := make(map[string]bool)         // message.ids already counted

	// pendingResume is armed by a resume boundary marker (a "<synthetic>"-model
	// assistant entry the harness emits on each cron re-fire, corroborated by the
	// isMeta "Continue from where you left off" user message). It is consumed by
	// the next real usage-bearing turn, which is stamped IsResumeReprime so the
	// spike detector can exempt its cold-cache re-prime. The synthetic marker
	// itself carries an all-zero usage block, so it never becomes a counted spike
	// and is never itself stamped as the re-prime.
	pendingResume := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024) // 1MB max line

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(line, &entry); err != nil {
			continue // skip malformed lines
		}
		if isResumeBoundary(entry) {
			pendingResume = true
		}
		if t, ok := parseTurn(entry); ok {
			// Collapse the repeated content-block lines of one assistant message
			// (same message.id, identical usage) into a single turn. Entries with
			// no id (result entries) each count once.
			id := extractMessageID(entry)
			if id == "" || !seen[id] {
				if id != "" {
					seen[id] = true
				}
				// The synthetic resume marker is itself a usage-bearing entry
				// (all-zero) — it must not consume the pending flag. Only stamp a
				// real re-prime turn (nonzero cache creation) and clear the flag.
				if pendingResume && t.CacheCreate > 0 {
					t.IsResumeReprime = true
					pendingResume = false
				}
				turns = append(turns, t)
			}
		}
		countCompactions(&compactions, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning session log: %w", err)
	}

	return buildTokenSignals(turns, compactions.CompactionCount), nil
}

// extractMessageID returns the assistant message id ("message.id") for an entry,
// or "" if absent (result entries, malformed lines). It is the dedup key that
// collapses a multi-content-block assistant message into a single turn.
func extractMessageID(entry map[string]json.RawMessage) string {
	msgRaw, ok := entry["message"]
	if !ok {
		return ""
	}
	var msg struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(msgRaw, &msg) != nil {
		return ""
	}
	return msg.ID
}

// parseTurn extracts one turn's token counts from a usage-bearing entry. Returns
// false for entries with no usage block. Callers dedupe by message.id so that
// the multiple content-block lines of one assistant message collapse to a single
// turn (see parseSessionTokenSignals).
func parseTurn(entry map[string]json.RawMessage) (turnUsage, bool) {
	usageJSON, ok := extractUsageJSON(entry)
	if !ok {
		return turnUsage{}, false
	}
	var u struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreation            struct {
			Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
			Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
		} `json:"cache_creation"`
	}
	if json.Unmarshal(usageJSON, &u) != nil {
		return turnUsage{}, false
	}
	return turnUsage{
		Input:       u.InputTokens,
		Output:      u.OutputTokens,
		CacheRead:   u.CacheReadInputTokens,
		CacheCreate: u.CacheCreationInputTokens,
		Ephemeral1h: u.CacheCreation.Ephemeral1hInputTokens,
		Ephemeral5m: u.CacheCreation.Ephemeral5mInputTokens,
	}, true
}

// minSpikeTokens is the absolute floor a turn's cache creation must clear to
// count as a redundant_context_spike, on top of the >2× running-median test.
//
// The median test alone is SCALE-FREE, and that was a false-positive generator:
// on a session whose typical rebuild is small (~1.5k tokens), an ordinary `sed`/
// `grep` read of a source file writes ~5k and trips ">2× the median" without
// re-priming anything. Measured on session ka-0000005126/7622b506 (bug
// ka-0000003202): 4 spikes of 3687/5153/5278/5434 tokens against a running
// median of ~1.4k–1.8k capped the Efficiency dimension of a session with a
// cache_hit_ratio of 0.9909 — definitionally the opposite of re-priming.
//
// 10000 sits well above a routine source read and well below a genuine re-prime
// balloon (the captured bloated fixture's balloons are 50k against a 10k
// baseline, and still fire). A spike must be BOTH disproportionate (>2× median)
// and absolutely large; either test alone misfires — the ratio alone flags noise
// on small-baseline sessions, the floor alone flags a session whose every write
// is legitimately large.
const minSpikeTokens = 10000

// buildTokenSignals derives the TokenSignals block from the per-turn series.
//
// All arithmetic here is token+token or token/token — additions for context
// size, divisions for ratios. There is deliberately no token×rate product: the
// cost-source guard forbids it, and cost is LiteLLM's job, not this analyzer's.
func buildTokenSignals(turns []turnUsage, compactionCount int64) *TokenSignals {
	ts := &TokenSignals{
		TurnCount:           len(turns),
		CompactionCount:     compactionCount,
		InputTokens:         make([]int64, 0, len(turns)),
		OutputTokens:        make([]int64, 0, len(turns)),
		CacheReadTokens:     make([]int64, 0, len(turns)),
		CacheCreationTokens: make([]int64, 0, len(turns)),
	}

	var totalInput, totalOutput, totalCacheRead, totalCacheCreate int64
	var total1h, total5m int64
	// Baseline for spike detection: the median of prior turns' NONZERO cache
	// creation. Using nonzero writes (not all prior turns) matters because a
	// healthy long session cache-hits most turns (cache_creation == 0); a median
	// taken over all-prior would sit at 0 and, with the earlier >0 guard, blind
	// the detector to genuine re-prime balloons on exactly the high-cache-hit
	// sessions the signal targets. Comparing against the typical *write* size
	// instead flags a turn whose context rebuild is >2× the usual rebuild.
	nonzeroCacheCreates := make([]int64, 0, len(turns))

	for _, t := range turns {
		ts.InputTokens = append(ts.InputTokens, t.Input)
		ts.OutputTokens = append(ts.OutputTokens, t.Output)
		ts.CacheReadTokens = append(ts.CacheReadTokens, t.CacheRead)
		ts.CacheCreationTokens = append(ts.CacheCreationTokens, t.CacheCreate)

		totalInput += t.Input
		totalOutput += t.Output
		totalCacheRead += t.CacheRead
		totalCacheCreate += t.CacheCreate
		total1h += t.Ephemeral1h
		total5m += t.Ephemeral5m

		// peak_context_tokens = max over turns of the resident context
		// (fresh input + cache read + cache creation) at that turn.
		residentContext := t.Input + t.CacheRead + t.CacheCreate
		if residentContext > ts.PeakContextTokens {
			ts.PeakContextTokens = residentContext
		}

		// redundant_context_spikes: a turn whose cache creation exceeds 2× the
		// running median of prior nonzero cache writes AND clears the absolute
		// minSpikeTokens floor signals a re-primed / bloated context relative to
		// the session's typical rebuild size. The empty-baseline check leaves the
		// first write unflagged (nothing to compare against yet); the floor keeps
		// the ratio test from firing on trivially small writes (see
		// minSpikeTokens).
		// A resume re-prime (the first turn after a cron-wake resume boundary) is
		// exempt: its large cache_creation re-caches the continuing conversation
		// whose 5-min ephemeral cache expired across the idle gap — legitimate
		// cold-cache cost, not redundant re-fetch. It is kept out of the median
		// baseline too, so the baseline stays representative of real per-turn
		// rebuilds and a genuine mid-run balloon (with no resume marker before it)
		// is still caught against the correct baseline.
		if !t.IsResumeReprime && len(nonzeroCacheCreates) > 0 && t.CacheCreate >= minSpikeTokens {
			runningMedian := medianInt64(nonzeroCacheCreates)
			if float64(t.CacheCreate) > 2*runningMedian {
				ts.RedundantContextSpikes++
			}
		}
		if t.CacheCreate > 0 && !t.IsResumeReprime {
			nonzeroCacheCreates = append(nonzeroCacheCreates, t.CacheCreate)
		}
	}

	// cache_hit_ratio = cache_read / (cache_read + input + cache_creation).
	// Pure token/token ratio; the higher, the more the session rode cache.
	if denom := totalCacheRead + totalInput + totalCacheCreate; denom > 0 {
		ts.CacheHitRatio = float64(totalCacheRead) / float64(denom)
	}
	if ts.TurnCount > 0 {
		ts.OutputPerRequest = float64(totalOutput) / float64(ts.TurnCount)
	}
	ts.EphemeralCacheCreation = ephemeralSplit{
		Ephemeral1hInputTokens: total1h,
		Ephemeral5mInputTokens: total5m,
	}
	return ts
}

// medianInt64 returns the median of a slice of int64 values (0 for empty). It
// copies before sorting so the caller's slice order is preserved.
func medianInt64(vals []int64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := make([]int64, len(vals))
	copy(s, vals)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return float64(s[n/2])
	}
	return float64(s[n/2-1]+s[n/2]) / 2
}
