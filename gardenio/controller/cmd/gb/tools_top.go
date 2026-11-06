package main

// gb tools top — surface the slowest tool/command invocations.
//
// Session mode (default) reads this pod's local coop hook event log
// (XDG_STATE_HOME/coop/sessions/<id>/hook_events.jsonl) directly off disk —
// no cluster access required. Fleet mode is meant to query the P2 slow-call
// ledger (gd-sWwPnwkZtw); until that lands it falls back to replaying the
// HOOK_EVENTS bus (GET /v1/bus/events?stream=hooks&since=), which only
// carries duration_ms once P0 (gd-lGvUZtjpRQ) forwards it through the hook
// relay — until then fleet mode reports call counts without latency.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var (
	toolsTopFleet   bool
	toolsTopSession bool
	toolsTopBy      string
	toolsTopSince   string
	toolsTopLimit   int
	toolsTopTimeout time.Duration
)

var toolsCmd = &cobra.Command{
	Use:     "tools",
	Short:   "Inspect tool-call latency",
	GroupID: "orchestration",
}

var toolsTopCmd = &cobra.Command{
	Use:   "top",
	Short: "Show the slowest tool/command invocations",
	Long: `Show the slowest tool/command invocations.

Session mode (default) reads this pod's local coop hook event log — no
cluster access needed. Fleet mode queries the fleet-wide slow-call ledger;
until it lands this falls back to replaying the HOOK_EVENTS bus, which only
has per-call duration once the hook relay forwards it (see task gd-lGvUZtjpRQ).

--by command (default) lists individual calls with their full command line.
--by class rolls calls up by cmd_class (the Bash command's first token, or
the tool name for non-Bash tools).`,
	RunE: runToolsTop,
}

func init() {
	toolsTopCmd.Flags().BoolVar(&toolsTopSession, "session", false, "scope to this pod's local session (default)")
	toolsTopCmd.Flags().BoolVar(&toolsTopFleet, "fleet", false, "scope to the fleet-wide ledger (falls back to a bounded HOOK_EVENTS bus replay)")
	toolsTopCmd.Flags().StringVar(&toolsTopBy, "by", "command", `group by: "command" (full command lines) or "class" (rolled-up cmd_class)`)
	toolsTopCmd.Flags().StringVar(&toolsTopSince, "since", "24h", "look back this far (Go duration syntax, e.g. 30m, 24h)")
	toolsTopCmd.Flags().IntVar(&toolsTopLimit, "limit", 20, "max rows to show")
	toolsTopCmd.Flags().DurationVar(&toolsTopTimeout, "timeout", 5*time.Second, "fleet mode only: how long to collect the bus replay before reporting")
	toolsTopCmd.MarkFlagsMutuallyExclusive("session", "fleet")
	toolsCmd.AddCommand(toolsTopCmd)
}

func runToolsTop(cmd *cobra.Command, args []string) error {
	if toolsTopBy != "command" && toolsTopBy != "class" {
		return fmt.Errorf(`--by must be "command" or "class", got %q`, toolsTopBy)
	}
	lookback, err := time.ParseDuration(toolsTopSince)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	cutoff := time.Now().Add(-lookback)

	var records []toolCallRecord
	if toolsTopFleet {
		records, err = runToolsTopFleet(cmd.Context(), cutoff)
	} else {
		records, err = runToolsTopSession(cutoff)
	}
	if err != nil {
		return err
	}

	printToolCalls(cmd.OutOrStdout(), records, toolsTopBy, toolsTopLimit)
	return nil
}

// toolCallRecord is one PostToolUse call, normalized from either the local
// coop hook log (session mode) or a relayed HOOK_EVENTS bus event (fleet
// mode fallback).
type toolCallRecord struct {
	When        time.Time
	Tool        string
	Class       string
	Command     string
	DurationMs  float64
	HasDuration bool
}

// toolHookFields is the subset of a PostToolUse hook payload this command
// reads. It decodes identically from the raw Claude Code hook JSON (session
// mode) and from a relayed hookRelayEvent (fleet mode) — both use the same
// tool_name/tool_input field names. DurationMs is a pointer so its absence
// (not yet forwarded by the hook relay, pre-P0) is distinguishable from 0ms.
type toolHookFields struct {
	ToolName   string          `json:"tool_name"`
	ToolInput  json.RawMessage `json:"tool_input,omitempty"`
	DurationMs *float64        `json:"duration_ms,omitempty"`
}

func buildToolCallRecord(f toolHookFields, when time.Time) toolCallRecord {
	command, class := extractCommandAndClass(f.ToolName, f.ToolInput)
	rec := toolCallRecord{When: when, Tool: f.ToolName, Class: class, Command: command}
	if f.DurationMs != nil {
		rec.DurationMs = *f.DurationMs
		rec.HasDuration = true
	}
	return rec
}

var envAssignmentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// bashCmdClass extracts the command class from a Bash command line: the
// first non-env-assignment token, with any path prefix stripped (so
// "/usr/bin/git" and "git" both classify as "git"). Mirrors the P2 ledger's
// cmd_class derivation (gd-sWwPnwkZtw) closely enough for local/session use;
// the centralized ledger remains the source of truth once it ships.
func bashCmdClass(command string) string {
	for _, f := range strings.Fields(command) {
		if envAssignmentRe.MatchString(f) {
			continue
		}
		return filepath.Base(f)
	}
	return "bash"
}

// extractCommandAndClass renders a display command line and a cmd_class for
// a tool call. Bash gets its real command line; the handful of other tools
// agents spend the most time in get a compact, recognizable summary; anything
// else falls back to the tool name plus a truncated tool_input.
func extractCommandAndClass(toolName string, toolInput json.RawMessage) (command, class string) {
	switch toolName {
	case "Bash":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(toolInput, &in) == nil && in.Command != "" {
			return in.Command, bashCmdClass(in.Command)
		}
	case "Read", "Edit", "Write", "NotebookEdit":
		var in struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(toolInput, &in) == nil && in.FilePath != "" {
			return fmt.Sprintf("%s %s", toolName, in.FilePath), toolName
		}
	case "Grep", "Glob":
		var in struct {
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal(toolInput, &in) == nil && in.Pattern != "" {
			return fmt.Sprintf("%s %q", toolName, in.Pattern), toolName
		}
	}
	compact := strings.TrimSpace(string(toolInput))
	if compact == "" || compact == "null" {
		return toolName, toolName
	}
	const maxCompactLen = 200
	if len(compact) > maxCompactLen {
		compact = compact[:maxCompactLen] + "…"
	}
	return fmt.Sprintf("%s %s", toolName, compact), toolName
}

// runToolsTopSession reads this pod's local coop hook event log.
func runToolsTopSession(cutoff time.Time) ([]toolCallRecord, error) {
	path, err := findSessionHookLog()
	if err != nil {
		return nil, err
	}
	return collectSessionToolCalls(path, cutoff)
}

// findSessionHookLog locates the hook_events.jsonl of the most recently
// active coop session under XDG_STATE_HOME (or $HOME/.local/state when
// unset, matching coop's own fallback in run.rs). A pod normally has exactly
// one live coop session; picking the most recently modified log is the
// simplest correct choice when a resume leaves older session dirs behind.
func findSessionHookLog() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/root"
		}
		base = filepath.Join(home, ".local", "state")
	}
	sessionsDir := filepath.Join(base, "coop", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return "", fmt.Errorf("reading coop sessions dir %s: %w", sessionsDir, err)
	}

	var newestPath string
	var newestMod time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(sessionsDir, e.Name(), "hook_events.jsonl")
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if newestPath == "" || info.ModTime().After(newestMod) {
			newestMod = info.ModTime()
			newestPath = p
		}
	}
	if newestPath == "" {
		return "", fmt.Errorf("no hook_events.jsonl found under %s", sessionsDir)
	}
	return newestPath, nil
}

// collectSessionToolCalls parses coop's local hook event log (one JSON
// object per line: {"hook_seq", "json", "timestamp_ms"}, where "json" is the
// raw Claude Code hook payload) and extracts PostToolUse calls at or after
// cutoff.
func collectSessionToolCalls(path string, cutoff time.Time) ([]toolCallRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	var records []toolCallRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry struct {
			TimestampMs int64           `json:"timestamp_ms"`
			JSON        json.RawMessage `json:"json"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		when := time.UnixMilli(entry.TimestampMs)
		if when.Before(cutoff) {
			continue
		}
		var payload struct {
			HookEventName string `json:"hook_event_name"`
			toolHookFields
		}
		if json.Unmarshal(entry.JSON, &payload) != nil {
			continue
		}
		if payload.HookEventName != "PostToolUse" || payload.ToolName == "" {
			continue
		}
		records = append(records, buildToolCallRecord(payload.toolHookFields, when))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning %s: %w", path, err)
	}
	return records, nil
}

// runToolsTopFleet queries the fleet-wide slow-call ledger (gd-sWwPnwkZtw).
// That ledger doesn't exist yet, so this currently always takes the replay
// fallback: a bounded replay of the HOOK_EVENTS bus. Swap in a ledger query
// here once gd-sWwPnwkZtw ships, keeping this as the fallback path.
func runToolsTopFleet(ctx context.Context, cutoff time.Time) ([]toolCallRecord, error) {
	records, err := collectFleetToolCallsViaBusReplay(ctx, cutoff, toolsTopTimeout)
	if err != nil {
		return nil, err
	}
	if len(records) > 0 && !anyRecordHasDuration(records) {
		fmt.Fprintln(os.Stderr, "note: duration_ms is not yet forwarded fleet-wide (pending gd-lGvUZtjpRQ/gd-sWwPnwkZtw) — showing call counts only")
	}
	return records, nil
}

func anyRecordHasDuration(records []toolCallRecord) bool {
	for _, r := range records {
		if r.HasDuration {
			return true
		}
	}
	return false
}

// collectFleetToolCallsViaBusReplay replays the HOOK_EVENTS bus from cutoff
// via GET /v1/bus/events?stream=hooks&since=, collecting PostToolUse events
// for up to `timeout` before returning whatever was gathered. There is no
// explicit "replay caught up" signal on this endpoint (it transitions
// straight from historical replay into live tailing), so a bounded
// wall-clock window is the simplest correct way to turn it into a one-shot
// CLI report.
func collectFleetToolCallsViaBusReplay(ctx context.Context, cutoff time.Time, timeout time.Duration) ([]toolCallRecord, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := strings.TrimRight(httpURL, "/")
	reqURL := fmt.Sprintf("%s/v1/bus/events?stream=hooks&since=%s", base, url.QueryEscape(cutoff.UTC().Format(time.RFC3339)))

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building bus replay request: %w", err)
	}
	if tok := tokenFlag; tok == "" {
		tok = defaultToken()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if reqCtx.Err() != nil {
			// Bounded collection window elapsed before the connection even
			// completed — treat as "nothing collected", not a hard failure.
			return nil, nil
		}
		return nil, fmt.Errorf("querying bus replay: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("bus replay returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	records, err := parseBusReplayStream(resp.Body)
	if err != nil && reqCtx.Err() != nil {
		// The bounded timeout tore down the read mid-stream — expected, not
		// an error. Keep whatever was parsed before that.
		return records, nil
	}
	return records, err
}

// parseBusReplayStream parses the SSE body of GET /v1/bus/events into
// PostToolUse tool-call records. Split out from
// collectFleetToolCallsViaBusReplay so the parsing logic is testable without
// a live daemon.
func parseBusReplayStream(body io.Reader) ([]toolCallRecord, error) {
	var records []toolCallRecord
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue
		}
		var evt struct {
			Type    string         `json:"type"`
			TS      string         `json:"ts"`
			Payload toolHookFields `json:"payload"`
		}
		if json.Unmarshal([]byte(data), &evt) != nil {
			continue
		}
		if evt.Type != "PostToolUse" || evt.Payload.ToolName == "" {
			continue
		}
		when, err := time.Parse(time.RFC3339, evt.TS)
		if err != nil {
			when = time.Now()
		}
		records = append(records, buildToolCallRecord(evt.Payload, when))
	}
	return records, scanner.Err()
}

// printToolCalls renders records as either individual full command lines
// (sorted slowest-first) or rolled up by cmd_class (sorted by total time).
func printToolCalls(w io.Writer, records []toolCallRecord, by string, limit int) {
	if len(records) == 0 {
		fmt.Fprintln(w, "no tool calls in the selected window")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	defer tw.Flush()

	if by == "class" {
		printToolCallsByClass(tw, records, limit)
		return
	}
	printToolCallsByCommand(tw, records, limit)
}

func printToolCallsByCommand(tw *tabwriter.Writer, records []toolCallRecord, limit int) {
	sorted := make([]toolCallRecord, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].HasDuration != sorted[j].HasDuration {
			return sorted[i].HasDuration // duration-bearing rows sort first
		}
		return sorted[i].DurationMs > sorted[j].DurationMs
	})
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[:limit]
	}

	fmt.Fprintln(tw, "DURATION\tTOOL\tWHEN\tCOMMAND")
	for _, r := range sorted {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", formatDurationMs(r), r.Tool, r.When.Local().Format(time.RFC3339), r.Command)
	}
}

type classAgg struct {
	Class       string
	Count       int
	TotalMs     float64
	MaxMs       float64
	HasDuration bool
}

func printToolCallsByClass(tw *tabwriter.Writer, records []toolCallRecord, limit int) {
	byClass := make(map[string]*classAgg)
	var order []string
	for _, r := range records {
		agg, ok := byClass[r.Class]
		if !ok {
			agg = &classAgg{Class: r.Class}
			byClass[r.Class] = agg
			order = append(order, r.Class)
		}
		agg.Count++
		if r.HasDuration {
			agg.HasDuration = true
			agg.TotalMs += r.DurationMs
			if r.DurationMs > agg.MaxMs {
				agg.MaxMs = r.DurationMs
			}
		}
	}

	aggs := make([]*classAgg, 0, len(order))
	for _, class := range order {
		aggs = append(aggs, byClass[class])
	}
	sort.SliceStable(aggs, func(i, j int) bool {
		if aggs[i].HasDuration != aggs[j].HasDuration {
			return aggs[i].HasDuration
		}
		if aggs[i].TotalMs != aggs[j].TotalMs {
			return aggs[i].TotalMs > aggs[j].TotalMs
		}
		return aggs[i].Count > aggs[j].Count
	})
	if limit > 0 && len(aggs) > limit {
		aggs = aggs[:limit]
	}

	fmt.Fprintln(tw, "CLASS\tCOUNT\tTOTAL\tAVG\tMAX")
	for _, a := range aggs {
		if !a.HasDuration {
			fmt.Fprintf(tw, "%s\t%d\tn/a\tn/a\tn/a\n", a.Class, a.Count)
			continue
		}
		avg := a.TotalMs / float64(a.Count)
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", a.Class, a.Count, formatMs(a.TotalMs), formatMs(avg), formatMs(a.MaxMs))
	}
}

func formatDurationMs(r toolCallRecord) string {
	if !r.HasDuration {
		return "n/a"
	}
	return formatMs(r.DurationMs)
}

func formatMs(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.2fs", ms/1000)
	}
	return fmt.Sprintf("%.0fms", ms)
}
