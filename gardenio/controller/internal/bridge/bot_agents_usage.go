package bridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/contextwindow"
)

// formatAgentContextChip renders the context-window occupancy chip for an agent
// card from the bead's context_tokens reading.
//
// For a LIVE agent this is the current occupancy (":brain: 128.0k ctx (69%)").
// The percentage is against the model's auto-compaction threshold (see
// internal/contextwindow), so 128k of a 200k-window model reads ~69% — near
// where compaction fires — not 64% of the raw window.
// For a CLOSED agent the statusreporter has stopped mirroring the field, so the
// value is frozen at the LAST reading before teardown — still the most useful
// context figure the operator can get for a finished run, so it renders as
// ":brain: 128.0k ctx at exit (69%)" rather than being suppressed. Suppressing
// it is what made the cost/turns/context rollup vanish from every completed
// agent's card (ka-0000003171); the "at exit" qualifier is what keeps it honest
// (it is a final reading, not a live one).
//
// Returns "" when context_tokens is absent or zero (no request ever landed).
func formatAgentContextChip(th Theme, fields map[string]string, live bool) string {
	n := parseTurnField(fields["context_tokens"])
	if n <= 0 {
		return ""
	}
	// Percentage is measured against the model's auto-compaction threshold — the
	// level at which compaction actually fires — on its real window, shared with
	// the beads-viewer render seam via internal/contextwindow so the two surfaces
	// can never disagree (ka-0000005244). The raw count stays authoritative.
	pct, known := contextwindow.OccupancyPctForAgent(fields, n)
	label := "ctx"
	if !live {
		label = "ctx at exit"
	}
	if !known {
		// Codex agent with no reported window: the raw count only, no guess.
		return fmt.Sprintf("%s %s %s", th.Semantic(SemContext), formatTokenCount(n), label)
	}
	return fmt.Sprintf("%s %s %s (%d%%)", th.Semantic(SemContext), formatTokenCount(n), label, pct)
}

// formatAgentTurns renders the round-trip count chip for an agent card, e.g.
// ":repeat: 42". While the agent is live it prefers the in-pod live_turns
// counter (which advances with each round-trip) but falls back to the
// authoritative total_request_count when live_turns is zero/absent — e.g. a
// live multi-session agent whose current session has not yet had its live_turns
// mirrored still shows the round-trips its earlier sessions accumulated. Once
// closed it reads total_request_count only, so a now-stale live_turns can never
// double-count. This live->live_turns-else-total selection is identical to the
// beads-viewer render seam (buildAgentUsageView in agent_usage.go) so both
// surfaces render the same turn count for the same agent bead (Phase 4
// cross-surface parity, ka-0000003042). Returns "" when neither field carries a
// positive count (no request has landed yet).
func formatAgentTurns(th Theme, fields map[string]string, live bool) string {
	n := parseTurnField(fields["total_request_count"])
	if live {
		if lt := parseTurnField(fields["live_turns"]); lt > 0 {
			n = lt
		}
	}
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%s %s", th.Semantic(SemTurns), formatTokenCount(n))
}

// formatAgentTTFT renders the time-to-first-token chip for an agent card from
// the bead's ttft_ms reading: the in-container boot→first-token latency (from
// SessionStart proof-of-life to the agent's first emitted token), which excludes
// the spawn queue / image-pull / network delay that precede boot (ka-0000003911).
// Sub-10s values keep one decimal (":zap: TTFT 1.2s"); larger cold-boot values
// round to whole seconds (":zap: TTFT 12s"), matching formatTTFT's scale.
// Returns "" when ttft_ms is absent or non-positive (never measured), so the
// chip only ever renders a real reading.
func formatAgentTTFT(th Theme, fields map[string]string) string {
	ms := parseTurnField(fields["ttft_ms"])
	if ms <= 0 {
		return ""
	}
	return fmt.Sprintf("%s TTFT %s", th.Semantic(SemLatency), formatTTFT(time.Duration(ms)*time.Millisecond))
}

// parseTurnField parses a turn-count field, returning 0 on empty/invalid/
// negative — matching the beads-viewer parseTokenField so both render seams
// treat the same field values identically.
func parseTurnField(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// formatAgentUsageLine builds the combined context-size + cost + turns line
// (the :brain: ctx · :moneybag: cost · :repeat: turns triad) shown on the Slack
// agent card. It renders for BOTH live and completed agents — an operator wants
// the rollup on a finished run most of all, since that is when the numbers are
// final (ka-0000003171).
//
// Live vs closed changes what each chip MEANS, not whether it renders:
//   - context is the current occupancy while live, and the frozen last reading
//     ("ctx at exit") once closed.
//   - cost is the reconciled total_cost_usd plus the in-pod live estimate of the
//     current session while live; once closed it is total_cost_usd ALONE, since
//     the session-close reconciler folds the final session into that total and
//     adding the now-stale live estimate would double-count.
//   - turns prefers the live counter while live, and the authoritative
//     total_request_count once closed.
//
// Each chip is independently omitted when its data is absent, so the separators
// only ever join chips that are actually present. Returns "" when there is
// nothing to show.
func formatAgentUsageLine(th Theme, fields map[string]string, live bool) string {
	if fields == nil {
		return ""
	}

	var parts []string
	if ctx := formatAgentContextChip(th, fields, live); ctx != "" {
		parts = append(parts, ctx)
	}

	cost := parseUSD(fields["total_cost_usd"])
	if live {
		cost += parseUSD(fields["live_cost_usd"])
	}
	if cost > 0 {
		parts = append(parts, fmt.Sprintf("%s $%.2f", th.Semantic(SemCost), cost))
	}

	if turns := formatAgentTurns(th, fields, live); turns != "" {
		parts = append(parts, turns)
	}

	if ttft := formatAgentTTFT(th, fields); ttft != "" {
		parts = append(parts, ttft)
	}

	return strings.Join(parts, " · ")
}

// parseUSD parses a USD field, returning 0 on empty/invalid/negative.
func parseUSD(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// formatAgentContextTurns renders the context + turns chips WITHOUT cost, for
// the surfaces that already carry a separate cost line (the FULL agent card, the
// wrap-up card, the dashboard rows). Kept beside formatAgentUsageLine so the two
// share one live-vs-closed field selection and can never disagree about the same
// bead. Returns "" when neither chip has data.
func formatAgentContextTurns(th Theme, fields map[string]string, live bool) string {
	if fields == nil {
		return ""
	}
	var parts []string
	if c := formatAgentContextChip(th, fields, live); c != "" {
		parts = append(parts, c)
	}
	if t := formatAgentTurns(th, fields, live); t != "" {
		parts = append(parts, t)
	}
	if ttft := formatAgentTTFT(th, fields); ttft != "" {
		parts = append(parts, ttft)
	}
	return strings.Join(parts, " · ")
}

// agentRollup bundles the three pre-rendered usage lines an agent card needs, so
// the card render paths resolve them from ONE bead fetch.
type agentRollup struct {
	// Cost is the $/in/out/sessions summary (formatCostSummary) for the surfaces
	// that carry a dedicated cost line: the FULL card and the wrap-up card.
	Cost string
	// Usage is the full context · cost · turns triad for the COMPACT thread card,
	// which has no separate cost line.
	Usage string
	// Context is the context · turns pair WITHOUT cost, for the surfaces whose
	// Cost line already carries the dollar figure.
	Context string
	// Local is the laptop badge (nil for any other agent). It rides the
	// rollup because it is read from the same bead fetch.
	Local *localBadge
}

// fetchAgentRollup resolves all three usage lines for an agent card from a single
// bead fetch. Each card flush previously issued THREE identical daemon lookups
// for the same bead (one per line) and let the three lines disagree if the bead
// changed between them.
//
// The lookup is status-AGNOSTIC, which is load-bearing: the superseded
// per-line fetchers used FindAgentBead, whose active-status filter meant a
// COMPLETED agent — bead closed within seconds of `gb stop` — resolved to
// ErrAgentNotFound and had its whole rollup silently vanish from the card
// (ka-0000003171). Harmless for the live callers: an open bead resolves
// identically either way.
//
// Liveness is read from the bead's agent_state and governs what each figure
// MEANS (see formatAgentUsageLine) — not whether the line renders. Returns a zero
// agentRollup when the bead cannot be resolved, and one carrying only Local
// when cost reporting is suppressed.
func (b *Bot) fetchAgentRollup(ctx context.Context, th Theme, agent string) agentRollup {
	if b.daemon == nil {
		return agentRollup{}
	}
	detail, err := b.daemon.FindAgentBeadAnyStatus(ctx, extractAgentName(agent))
	if err != nil || detail == nil {
		return agentRollup{}
	}
	var r agentRollup
	if !b.notifConfig.SuppressCostReporting {
		r = buildAgentRollup(th, detail.Fields, agentStateIsLive(detail.Fields["agent_state"]))
	}
	r.Local = b.localBadgeForAgent(ctx, detail.Fields)
	return r
}

// buildAgentRollup renders the three usage lines from one already-fetched fields
// map. Split from fetchAgentRollup so callers that ALREADY hold the bead's fields
// — the card-prune loop, which fetches the bead to recover the wrap-up — render
// the identical rollup without a second lookup. That reuse is the actual fix for
// the prune loop stripping cost off a wrap-up card it had moments earlier
// (ka-0000003171).
func buildAgentRollup(th Theme, fields map[string]string, live bool) agentRollup {
	return agentRollup{
		Cost:    formatCostSummary(fields),
		Usage:   formatAgentUsageLine(th, fields, live),
		Context: formatAgentContextTurns(th, fields, live),
	}
}

// agentStateIsLive reports whether an agent_state denotes a still-running
// agent (context size is meaningful and the live cost estimate is current).
func agentStateIsLive(state string) bool {
	switch state {
	case "working", "spawning", "yielding", "idle", "rate_limited":
		return true
	default:
		return false
	}
}
