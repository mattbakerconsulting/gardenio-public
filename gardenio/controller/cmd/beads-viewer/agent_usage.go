package main

import (
	"fmt"
	"strconv"

	"gardenio/controller/internal/contextwindow"
	"gardenio/controller/internal/planspend"
)

// agentUsageView is the pre-formatted context-size + cost summary rendered on
// the agent cards (bead-show agent card, /agents rows). All fields are strings
// so templates stay dumb; empties render nothing.
type agentUsageView struct {
	// ContextLabel is the humanized context-window occupancy, e.g. "128k". Empty
	// when no context_tokens has been reported yet. For a closed agent this is
	// the frozen last reading before teardown — ContextAtExit says which.
	ContextLabel string
	// ContextPct is the occupancy as a whole-number percent of the model's
	// auto-compaction threshold (0–100+) — 100% means compaction is imminent,
	// not that the raw window is full (see internal/contextwindow). Zero when
	// unknown.
	ContextPct int
	// ContextTitle is the hover detail, e.g. "128,000 tokens (~69% of the
	// ~184k auto-compact threshold on a 200k window)".
	ContextTitle string
	// ContextAtExit marks ContextLabel as a completed run's FINAL reading rather
	// than a live one, so the chip can qualify itself ("128k ctx at exit") and
	// never be misread as a still-advancing figure.
	ContextAtExit bool
	// CostLabel is the formatted dollar figure, e.g. "$1.23". Empty when no
	// cost is known.
	CostLabel string
	// TurnsLabel is the humanized server round-trip count for the 🔁 chip, e.g.
	// "47". Empty when no turn count is known. While the agent is live this
	// tracks live_turns; once closed it shows the authoritative
	// total_request_count. See buildAgentUsageView for the fallback.
	TurnsLabel string
	// HasContext / HasCost / HasTurns gate rendering of each chip independently.
	HasContext bool
	HasCost    bool
	HasTurns   bool
}

// HasAny reports whether a context, cost, or turns figure is present, so a
// template can skip the whole usage strip when there is nothing to show.
func (v agentUsageView) HasAny() bool { return v.HasContext || v.HasCost || v.HasTurns }

// ContextLevel classifies context-window occupancy into a severity band, the
// single source of truth for the warn/high coloring shared by the compact usage
// chip AND the always-visible context gauge (ka-0000006399) so the two surfaces
// can never disagree on when the window is "getting full". Returns "high" at
// >=80%, "warn" at >=60%, "ok" below that, and "" when no context is known.
func (v agentUsageView) ContextLevel() string {
	if !v.HasContext {
		return ""
	}
	switch {
	case v.ContextPct >= 80:
		return "high"
	case v.ContextPct >= 60:
		return "warn"
	default:
		return "ok"
	}
}

// ContextBarPct clamps ContextPct to the 0..100 range for use as a CSS width
// percentage on the gauge fill. ContextPct is derived against the auto-compact
// threshold rather than the raw window (see internal/contextwindow), so it can
// legitimately read slightly over 100 right before compaction fires; the gauge
// fill must not overflow its track, so we cap it here while the numeric label
// still shows the true percent.
func (v agentUsageView) ContextBarPct() int {
	if v.ContextPct < 0 {
		return 0
	}
	if v.ContextPct > 100 {
		return 100
	}
	return v.ContextPct
}

// buildAgentUsageView derives the context-size + cost summary from an agent
// bead's fields. `live` indicates the agent is in an active/alive state, which
// governs what each figure MEANS — not whether it renders (ka-0000003171: an
// operator wants the rollup on a COMPLETED run most of all, since that is when
// the numbers are final):
//
//   - Context is the current occupancy while live. Once closed the
//     statusreporter stops mirroring the field, so the value is frozen at the
//     last reading before teardown; it still renders, flagged ContextAtExit so
//     the chip can qualify itself rather than imply a live figure.
//   - Cost combines the reconciled total_cost_usd (accumulated from *closed*
//     sessions by the controller's session-close reconciler) with the in-pod
//     live_cost_usd estimate of the *current* session. The live estimate is
//     only added while the agent is live; once it closes, the reconciler folds
//     the final session cost into total_cost_usd, so adding the now-stale live
//     value would double-count. See reconcile_session_spend.go.
//   - Turns prefer the live counter while live, else total_request_count.
//
// Kept in lockstep with the Slack seam (formatAgentUsageLine in
// internal/bridge/bot_agents_usage.go) per the Phase 4 cross-surface parity
// contract, so both surfaces render the same figures for the same bead.
func buildAgentUsageView(fields map[string]string, live bool) agentUsageView {
	var v agentUsageView

	if ctx := parseTokenField(fields["context_tokens"]); ctx > 0 {
		v.HasContext = true
		v.ContextLabel = humanizeTokenCount(ctx)
		v.ContextAtExit = !live
		// Percentage is measured against the auto-compaction threshold — the
		// level at which compaction actually fires — on the model's real window,
		// not the raw ceiling. The raw token count above stays authoritative.
		// A Codex agent whose window is unknown gets the raw count only
		// (ContextPct stays 0, which the templates already read as "unknown").
		pct, known := contextwindow.OccupancyPctForAgent(fields, ctx)
		rawWindow, threshold, _ := contextwindow.AgentWindow(fields)
		v.ContextPct = pct
		atExit := ""
		if !live {
			atExit = " at exit"
		}
		switch {
		case !known:
			v.ContextTitle = fmt.Sprintf("%s tokens%s (context window unknown for this model)", groupThousands(ctx), atExit)
		case live:
			v.ContextTitle = fmt.Sprintf("%s tokens (~%d%% of the ~%dk auto-compact threshold on a %dk window)",
				groupThousands(ctx), pct, threshold/1000, rawWindow/1000)
		default:
			v.ContextTitle = fmt.Sprintf("%s tokens at exit (~%d%% of the ~%dk auto-compact threshold on a %dk window) — final reading before teardown",
				groupThousands(ctx), pct, threshold/1000, rawWindow/1000)
		}
	}

	if cost := agentSpendUSD(fields, live); cost > 0 {
		v.HasCost = true
		v.CostLabel = fmtUSD(cost)
	}

	// Turns: while live, prefer the in-pod live_turns counter (advancing with
	// each round-trip); fall back to the authoritative total_request_count when
	// live_turns is zero/absent or the agent has closed. A closed agent keeps
	// total_request_count, so its turn count survives after the live field stops
	// updating.
	turns := parseTokenField(fields["total_request_count"])
	if live {
		if lt := parseTokenField(fields["live_turns"]); lt > 0 {
			turns = lt
		}
	}
	if turns > 0 {
		v.HasTurns = true
		v.TurnsLabel = humanizeTokenCount(turns)
	}

	return v
}

// parseTokenField parses a token-count field, returning 0 on empty/invalid.
func parseTokenField(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// parseCostField parses a USD field, returning 0 on empty/invalid.
func parseCostField(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// agentSpendUSD is the single source of truth for one agent bead's dollar spend,
// shared by the agent-card usage strip (buildAgentUsageView) and the plan crew
// spend rollup (planCrewBulk / planCrew) so the two surfaces can never disagree
// on what a crew cost. The reconciled total_cost_usd (folded from CLOSED sessions
// by the controller's session-close reconciler) is authoritative; the in-pod
// live_cost_usd estimate of the CURRENT session is added ONLY while the agent is
// live, because once it closes the reconciler folds the final session cost into
// total_cost_usd and adding the now-stale live value would double-count (see
// reconcile_session_spend.go / buildAgentUsageView's cost note). Returns 0 when
// no cost is known. It is planspend.AgentSpendUSD, which the controller's
// per-plan rollup (plan_spend_usd, gd-kcy4IFNzv5) also sums, so /plans and
// /garden agree on a plan's crew spend.
func agentSpendUSD(fields map[string]string, live bool) float64 {
	return planspend.AgentSpendUSD(fields, live)
}

// humanizeTokenCount renders a token count compactly: 1234 → "1.2k",
// 1234567 → "1.2M", 500 → "500".
func humanizeTokenCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// groupThousands renders an integer with comma thousands separators for the
// hover title (e.g. 128000 → "128,000").
func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	// Insert commas from the right.
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
