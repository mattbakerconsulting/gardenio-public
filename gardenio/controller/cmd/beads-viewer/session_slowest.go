package main

import (
	"fmt"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// slowCallRow is one rendered row of the session page's "Slowest tool calls"
// table (plan gardenio/plans/agent-tool-latency-attribution.md P4, bead
// gd-YcmtujOTrC). Args is the already-redacted command line / tool input that
// `gb session close` wrote — the viewer displays it verbatim and never
// re-derives it.
type slowCallRow struct {
	Rank      int
	Duration  string
	Tool      string
	CmdClass  string
	Args      string
	Truncated bool
	Derived   bool // duration came from Pre/Post pairing, not Claude's native duration_ms
	When      string
}

// buildSessionSlowCalls projects a session bead's slowest_calls field into
// table rows. nil (suppressing the whole section) when the session recorded
// none — a session that predates the field, or one with no timed tool calls.
func buildSessionSlowCalls(bead *beadsapi.BeadDetail) []slowCallRow {
	calls := beadsapi.ParseSessionSlowCalls(bead.Fields[beadsapi.SessionSlowestCallsField])
	if len(calls) == 0 {
		return nil
	}
	rows := make([]slowCallRow, 0, len(calls))
	for i, c := range calls {
		when := ""
		if t, err := time.Parse(time.RFC3339, c.TS); err == nil {
			when = t.UTC().Format("15:04:05")
		}
		rows = append(rows, slowCallRow{
			Rank:      i + 1,
			Duration:  fmtCallDurationMs(c.DurationMs),
			Tool:      c.Tool,
			CmdClass:  c.CmdClass,
			Args:      c.Args,
			Truncated: c.TruncatedArgs,
			Derived:   c.Source == "derived",
			When:      when,
		})
	}
	return rows
}

// fmtCallDurationMs renders a tool-call duration: "850ms", "12.4s", "3m 05s".
func fmtCallDurationMs(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		s := ms / 1000
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	}
}
