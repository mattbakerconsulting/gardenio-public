package main

// Per-session slowest tool calls (plan gardenio/plans/agent-tool-latency-attribution.md
// P4, bead gd-YcmtujOTrC).
//
// coop already writes every hook payload — duration_ms and the full tool_input
// included — to <state>/coop/sessions/<uuid>/hook_events.jsonl on the pod. At
// session close we stream that file once through a bounded top-N min-heap and
// write the survivors to the session bead's `slowest_calls` field. No NATS, no
// Postgres, no S3: the S3 archive is OFF in gardenio-k3s, so this is the one
// path that works on every cluster. The fleet-wide, queryable-by-cmd_class
// half of P4 is the slow_tool_calls ledger P2 already ships.
//
// Redaction: coop's file is raw (the S3 audit calls hook_events.jsonl
// Confidential for exactly this reason), so every Args string is scrubbed with
// the same toollatency.RedactText/RedactJSON the controller-side consumer uses
// BEFORE it can reach a bead. Nothing un-redacted is retained past this file's
// heap push.

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/toollatency"
)

const (
	// sessionSlowestN is how many calls a session bead keeps.
	sessionSlowestN = 10
	// sessionSlowArgsMaxBytes caps one entry's redacted args: 10 entries stay
	// well under the bead field size budget while a long pipeline is still
	// readable.
	sessionSlowArgsMaxBytes = 4096
	// sessionSlowPendingMax bounds the Pre→Post pairing map. Unmatched Pre
	// events (an aborted call) would otherwise accumulate for the whole
	// session; on overflow the map is dropped, which only costs derived
	// durations for calls still in flight at that instant.
	sessionSlowPendingMax = 4096
)

// hookEventLine is the subset of one hook_events.jsonl line this reader needs
// (coop event_log.rs HookEntry wrapping the raw hook payload). Everything else —
// notably the potentially huge tool_response — is skipped by the decoder
// without being retained.
type hookEventLine struct {
	JSON struct {
		Event string `json:"event"`
		Data  struct {
			ToolName   string          `json:"tool_name"`
			ToolInput  json.RawMessage `json:"tool_input"`
			ToolUseID  string          `json:"tool_use_id"`
			DurationMs *float64        `json:"duration_ms"`
		} `json:"data"`
	} `json:"json"`
	TimestampMs int64 `json:"timestamp_ms"`
}

// slowHeap is a min-heap on DurationMs: the root is the fastest of the current
// top-N, so a new call only has to beat the root to displace it.
type slowHeap []beadsapi.SessionSlowCall

func (h slowHeap) Len() int           { return len(h) }
func (h slowHeap) Less(i, j int) bool { return h[i].DurationMs < h[j].DurationMs }
func (h slowHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *slowHeap) Push(x any)        { *h = append(*h, x.(beadsapi.SessionSlowCall)) }
func (h *slowHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// slowCollector accumulates the top-N slowest calls across any number of
// hook_events.jsonl files.
type slowCollector struct {
	n    int
	h    slowHeap
	pend map[string]int64 // tool_use_id -> PreToolUse timestamp_ms
}

func newSlowCollector(n int) *slowCollector {
	return &slowCollector{n: n, pend: make(map[string]int64)}
}

// wouldKeep is the cheap duration test run before a call's args are redacted,
// so a long session does not pay the redaction regexes for every call.
func (s *slowCollector) wouldKeep(durationMs int64) bool {
	return len(s.h) < s.n || durationMs > s.h[0].DurationMs
}

func (s *slowCollector) push(c beadsapi.SessionSlowCall) {
	if len(s.h) < s.n {
		heap.Push(&s.h, c)
		return
	}
	if c.DurationMs > s.h[0].DurationMs {
		s.h[0] = c
		heap.Fix(&s.h, 0)
	}
}

// result returns the collected calls slowest-first.
func (s *slowCollector) result() []beadsapi.SessionSlowCall {
	out := make([]beadsapi.SessionSlowCall, len(s.h))
	copy(out, s.h)
	sort.SliceStable(out, func(i, j int) bool { return out[i].DurationMs > out[j].DurationMs })
	return out
}

// scan streams one hook_events.jsonl through the collector, considering only
// events at or after since (zero = all). A malformed or half-written trailing
// line stops the scan and keeps what was collected — the file is appended to
// live, and a json.Decoder cannot resync mid-stream.
func (s *slowCollector) scan(r io.Reader, since time.Time) {
	dec := json.NewDecoder(r)
	sinceMs := int64(0)
	if !since.IsZero() {
		sinceMs = since.UnixMilli()
	}
	for {
		var line hookEventLine
		if err := dec.Decode(&line); err != nil {
			return
		}
		if line.TimestampMs < sinceMs {
			continue
		}
		d := line.JSON.Data
		switch line.JSON.Event {
		case "pre_tool_use", "before_tool":
			if d.ToolUseID == "" {
				continue
			}
			if len(s.pend) >= sessionSlowPendingMax {
				s.pend = make(map[string]int64)
			}
			s.pend[d.ToolUseID] = line.TimestampMs
		case "post_tool_use", "after_tool":
			var ms int64
			source := metricsSourceClaude
			if d.DurationMs != nil && *d.DurationMs > 0 {
				ms = int64(*d.DurationMs)
			} else if pre, ok := s.pend[d.ToolUseID]; ok && d.ToolUseID != "" && line.TimestampMs > pre {
				ms = line.TimestampMs - pre
				source = metricsSourceDerived
			}
			if d.ToolUseID != "" {
				delete(s.pend, d.ToolUseID)
			}
			if ms <= 0 || !s.wouldKeep(ms) {
				continue
			}
			s.push(buildSlowCall(d.ToolName, d.ToolUseID, d.ToolInput, ms, source, line.TimestampMs))
		}
	}
}

// Mirror metrics.ToolLatencySource{Claude,Derived} (gb does not import the
// Prometheus metrics package just for two strings).
const (
	metricsSourceClaude  = "claude"
	metricsSourceDerived = "derived"
)

func buildSlowCall(tool, toolUseID string, input json.RawMessage, ms int64, source string, tsMs int64) beadsapi.SessionSlowCall {
	c := beadsapi.SessionSlowCall{
		TS:         time.UnixMilli(tsMs).UTC().Format(time.RFC3339),
		Tool:       tool,
		ToolUseID:  toolUseID,
		DurationMs: ms,
		Source:     source,
	}
	args := ""
	if tool == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &in)
		c.CmdClass = toollatency.ClassifyCmd(in.Command)
		args = toollatency.RedactText(in.Command)
	} else if len(input) > 0 && string(input) != "null" {
		args = string(toollatency.RedactJSON(input))
	}
	c.Args, c.TruncatedArgs = truncateUTF8(args, sessionSlowArgsMaxBytes)
	return c
}

// truncateUTF8 cuts s to at most max bytes on a rune boundary.
func truncateUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// coopSessionHookLogs returns every hook_events.jsonl under coopStateDir that
// was modified at or after since. A pod can run several coop sessions over its
// life (restarts share the PVC), so the mtime filter plus the per-line
// timestamp filter in scan scopes the result to THIS session bead.
func coopSessionHookLogs(coopStateDir string, since time.Time) []string {
	matches, err := filepath.Glob(filepath.Join(coopStateDir, "sessions", "*", "hook_events.jsonl"))
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range matches {
		fi, err := os.Stat(p)
		if err != nil || (!since.IsZero() && fi.ModTime().Before(since)) {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// collectSessionSlowestCalls returns the sessionSlowestN slowest calls since
// `since` across the coop hook logs under coopStateDir, slowest first.
func collectSessionSlowestCalls(coopStateDir string, since time.Time) []beadsapi.SessionSlowCall {
	col := newSlowCollector(sessionSlowestN)
	for _, p := range coopSessionHookLogs(coopStateDir, since) {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		col.scan(f, since)
		f.Close()
	}
	return col.result()
}

// defaultCoopStateDir mirrors where the restart loops point coop's state:
// <BOAT_WORKSPACE>/.state/coop.
func defaultCoopStateDir() string {
	return filepath.Join(envOr("BOAT_WORKSPACE", "/home/agent/workspace"), ".state", "coop")
}

// recordSessionSlowestCalls computes the session's slowest calls and writes
// them to the session bead. Best-effort and silent when there is nothing to
// record: it must never delay or fail the session close. Called BEFORE the
// close so the field lands on a still-open bead.
func recordSessionSlowestCalls(ctx context.Context, sessionBeadID, coopStateDir string, since time.Time) {
	if daemon == nil || sessionBeadID == "" {
		return
	}
	calls := collectSessionSlowestCalls(coopStateDir, since)
	if len(calls) == 0 {
		return
	}
	raw, err := json.Marshal(calls)
	if err != nil {
		return
	}
	if err := daemon.UpdateBeadFieldsTyped(ctx, sessionBeadID, map[string]any{
		beadsapi.SessionSlowestCallsField: string(raw),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record slowest tool calls on session %s: %v\n", sessionBeadID, err)
	}
}

// sessionStartedAt parses the session bead's started_at field (zero when
// absent/unparseable, which widens the scan to the whole file rather than
// dropping data).
func sessionStartedAt(fields map[string]string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(fields["started_at"]))
	if err != nil {
		return time.Time{}
	}
	return t
}
