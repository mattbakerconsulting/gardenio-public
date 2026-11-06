package main

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Tool-latency ledger view (plan gardenio/plans/agent-tool-latency-attribution.md
// P5, bead gd-GdEpjNcXXh): the slowest agent tool calls in a window, grouped by
// cmd_class, each row expandable to its full redacted argv and its
// agent/session. Reads the slow_tool_calls ledger the P2 HOOK_EVENTS consumer
// writes (GET /v1/metrics/slow-tool-calls, slowest-first).

// toolLatencyLimit is the ledger page size requested per view. The daemon caps
// at 500; the view is a "top offenders" list, not an export.
const toolLatencyLimit = 200

// toolLatencyDefaultWindow is used when ?window is absent or unrecognised.
const toolLatencyDefaultWindow = "24h"

// toolLatencyWindows are the selectable windows, in display order.
var toolLatencyWindows = []struct {
	Key string
	Dur time.Duration
}{
	{"1h", time.Hour},
	{"6h", 6 * time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
}

// toolLatencyRowView is one expandable ledger row.
type toolLatencyRowView struct {
	DurationDisplay string
	ToolName        string
	Summary         string // first line of the command, truncated; falls back to tool name
	Command         string // full redacted argv (Bash) — empty for non-Bash tools
	ToolInput       string // raw redacted tool_input JSON, shown when there is no Command
	When            string // relative "ago"
	WhenAbs         string // RFC3339, for the title tooltip
	AgentBeadID     string
	SessionID       string
	Project         string
	Role            string
	Source          string
	ExitStatus      string
	OutputBytes     int
	IsSidechain     bool
}

// toolLatencyGroupView is one cmd_class section.
type toolLatencyGroupView struct {
	Class        string
	Count        int
	TotalDisplay string
	MaxDisplay   string
	Rows         []toolLatencyRowView
}

// handleToolLatency renders GET /tool-latency?window=24h&class=<cmd_class>.
func (s *Server) handleToolLatency(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	window, dur := resolveToolLatencyWindow(q.Get("window"))
	class := strings.TrimSpace(q.Get("class"))

	type windowOpt struct {
		Key      string
		Selected bool
	}
	opts := make([]windowOpt, 0, len(toolLatencyWindows))
	for _, wd := range toolLatencyWindows {
		opts = append(opts, windowOpt{Key: wd.Key, Selected: wd.Key == window})
	}
	data := map[string]any{
		"Window":  window,
		"Windows": opts,
		"Class":   class,
		"Groups":  []toolLatencyGroupView{},
		"Total":   0,
	}

	now := time.Now()
	rows, err := s.daemon.ListSlowToolCalls(r.Context(), beadsapi.SlowToolCallFilter{
		Since:    now.Add(-dur),
		CmdClass: class,
		Limit:    toolLatencyLimit,
	})
	if err != nil {
		s.logger.Error("listing slow tool calls", "error", err)
		data["Error"] = "Failed to load the slow tool-call ledger."
		s.render(w, r, "tool_latency.html", data)
		return
	}
	data["Groups"] = groupSlowToolCalls(rows, now)
	data["Total"] = len(rows)
	s.render(w, r, "tool_latency.html", data)
}

// resolveToolLatencyWindow maps ?window to a known key + duration, defaulting
// when absent or unrecognised so a hand-edited URL never errors.
func resolveToolLatencyWindow(key string) (string, time.Duration) {
	for _, wd := range toolLatencyWindows {
		if wd.Key == key {
			return wd.Key, wd.Dur
		}
	}
	for _, wd := range toolLatencyWindows {
		if wd.Key == toolLatencyDefaultWindow {
			return wd.Key, wd.Dur
		}
	}
	return toolLatencyDefaultWindow, 24 * time.Hour
}

// groupSlowToolCalls buckets ledger rows (already slowest-first) by cmd_class.
// Groups are ordered by their slowest call, descending, so the worst class
// leads; rows inside a group keep the ledger's slowest-first order. Rows with
// no cmd_class (non-Bash tools) land under "(tool)".
func groupSlowToolCalls(rows []*beadsapi.SlowToolCallRow, now time.Time) []toolLatencyGroupView {
	type agg struct {
		view  toolLatencyGroupView
		total int64
		max   int64
	}
	byClass := map[string]*agg{}
	var order []string
	for _, row := range rows {
		if row == nil {
			continue
		}
		class := row.CmdClass
		if class == "" {
			class = "(tool)"
		}
		a, ok := byClass[class]
		if !ok {
			a = &agg{view: toolLatencyGroupView{Class: class}}
			byClass[class] = a
			order = append(order, class)
		}
		a.view.Rows = append(a.view.Rows, toolLatencyRowFromLedger(row, now))
		a.view.Count++
		a.total += row.DurationMs
		if row.DurationMs > a.max {
			a.max = row.DurationMs
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return byClass[order[i]].max > byClass[order[j]].max })
	out := make([]toolLatencyGroupView, 0, len(order))
	for _, c := range order {
		a := byClass[c]
		a.view.TotalDisplay = fmtDurationMs(a.total)
		a.view.MaxDisplay = fmtDurationMs(a.max)
		out = append(out, a.view)
	}
	return out
}

func toolLatencyRowFromLedger(row *beadsapi.SlowToolCallRow, now time.Time) toolLatencyRowView {
	v := toolLatencyRowView{
		DurationDisplay: fmtDurationMs(row.DurationMs),
		ToolName:        row.ToolName,
		Command:         row.CommandRedacted,
		When:            fmtTimeAgo(row.TS, now),
		AgentBeadID:     row.AgentBeadID,
		SessionID:       row.SessionID,
		Project:         row.Project,
		Role:            row.Role,
		Source:          row.Source,
		OutputBytes:     row.OutputBytes,
		IsSidechain:     row.IsSidechain,
	}
	if !row.TS.IsZero() {
		v.WhenAbs = row.TS.UTC().Format(time.RFC3339)
	}
	if row.ExitStatus != nil {
		v.ExitStatus = strconv.Itoa(*row.ExitStatus)
	}
	if row.CommandRedacted == "" && len(row.ToolInputRedacted) > 0 {
		v.ToolInput = string(row.ToolInputRedacted)
	}
	v.Summary = toolLatencySummary(row)
	return v
}

// toolLatencySummary is the collapsed-row label: the first line of the command,
// truncated; the tool name when the call carried no command.
func toolLatencySummary(row *beadsapi.SlowToolCallRow) string {
	cmd := row.CommandRedacted
	if cmd == "" {
		return row.ToolName
	}
	if i := strings.IndexByte(cmd, '\n'); i >= 0 {
		cmd = cmd[:i] + " …"
	}
	const maxRunes = 120
	if r := []rune(cmd); len(r) > maxRunes {
		cmd = string(r[:maxRunes]) + "…"
	}
	return cmd
}

// fmtDurationMs renders a millisecond duration compactly: "840ms", "12.3s",
// "4m 05s", "1h 02m".
func fmtDurationMs(ms int64) string {
	switch {
	case ms < 1000:
		return strconv.FormatInt(ms, 10) + "ms"
	case ms < 60_000:
		return strconv.FormatFloat(float64(ms)/1000, 'f', 1, 64) + "s"
	case ms < 3_600_000:
		m, sec := ms/60_000, (ms/1000)%60
		return strconv.FormatInt(m, 10) + "m " + pad2(sec) + "s"
	default:
		h, m := ms/3_600_000, (ms/60_000)%60
		return strconv.FormatInt(h, 10) + "h " + pad2(m) + "m"
	}
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}
