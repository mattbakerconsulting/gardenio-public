// Package contextwindow centralizes the context-window accounting used to turn
// an agent's raw context_tokens reading into an occupancy percentage on the
// agent cards. It is the single source of truth shared by the two render seams
// — the beads-viewer (cmd/beads-viewer/agent_usage.go) and the Slack bridge
// (internal/bridge/bot_agents_usage.go) — which previously each hard-coded their
// own 200k denominator and so could silently drift apart (ka-0000005244).
//
// Three facts live here:
//
//   - The per-model raw window (WindowTokensForModel): the fleet default is a
//     200k window (Opus/Sonnet/Haiku 4.x); the long-context models (Fable, and
//     the opt-in 1M beta) run a 1M window. Reading the agent bead's "model"
//     field lets the percentage reflect the model's REAL window instead of
//     pinning every agent to 200k.
//   - The auto-compact threshold (EffectiveWindowTokens): Claude Code compacts
//     BEFORE the window is literally full — it reserves headroom for the model's
//     own output AND for the compaction summary it is about to write — so the
//     token level at which compaction actually fires sits an ABSOLUTE distance
//     below the raw window, not a proportional one. Occupancy is most meaningful
//     measured against THIS level, because that is the wall an agent hits.
//   - The percentage itself (OccupancyPctForAgent): ctx as a whole-number percent
//     of the auto-compact threshold. 100% means compaction is imminent.
//
// The raw token count remains the authoritative figure shown alongside the
// percentage; the percentage is explicitly an approximation.
package contextwindow

import (
	"strconv"
	"strings"
)

const (
	// DefaultWindowTokens is the context-window size for the fleet-default
	// Claude models (Opus / Sonnet / Haiku 4.x), which run a 200k window. This
	// is the fallback for an empty or unrecognized model — an agent riding its
	// account's default model carries no "model" field, and the fleet default
	// has a 200k window, so the fallback is correct for the common case.
	DefaultWindowTokens = 200_000

	// LargeWindowTokens is the 1M-token window served by the long-context models
	// (Claude Fable 5, whose alias resolves to a 1M-context model, and the opt-in
	// 1M beta on other families).
	LargeWindowTokens = 1_000_000
)

// Claude Code's auto-compaction fires an ABSOLUTE distance below the raw window,
// not a fixed fraction of it. The threshold the shipped binary (Claude Code
// 2.1.236) computes is:
//
//	threshold = min(configuredWindow, modelWindow)
//	          − min(maxOutputTokens, 20_000)   // reserve for the model's output
//	          − 13_000                          // reserve for the compaction summary
//
// (derived from the shipped binary's internals — see gardenio/docs/
// compaction-settings.md §2 "Threshold math"; treat the exact reserves as
// observed-not-documented and re-check after a Claude Code upgrade.)
//
// Modeling this reserve as a fraction (the previous AutoCompactFraction = 0.92)
// is wrong in BOTH directions because the reserve is absolute, not proportional:
//
//   - 200k window: 0.92 reserves only 16k, so the gauge read "fine" up to 184k
//     while compaction actually fires at ~167k — it OVERSTATED headroom by ~17k,
//     the opposite of the constant's stated conservative intent (ka-0000001225).
//   - 1M window: 0.92 reserves 80k, far more than the ~33k the harness actually
//     holds back, so the same gauge read hot (compaction at 920k) ~47k before
//     the real wall at 967k — UNDERSTATING headroom on long-context models.
//
// One fraction cannot be correct for both windows; the absolute-reserve model
// below is right-shaped for every window size. The reserves are named constants
// so they stay trivial to retune when a Claude Code upgrade moves them.
const (
	// OutputReserveTokens is Claude Code's reserve for the model's own output,
	// min(maxOutputTokens, 20_000). The fleet default max-output is ≥20k, so the
	// 20k ceiling is both the common case and the conservative choice — a smaller
	// max-output only reserves LESS and pushes the threshold HIGHER, so assuming
	// 20k never makes the gauge read cooler than reality.
	OutputReserveTokens = 20_000

	// SummaryReserveTokens is the headroom Claude Code holds back for the
	// compaction summary it writes as it compacts.
	SummaryReserveTokens = 13_000
)

// WindowTokensForModel returns the raw context-window size for a model id or
// shorthand (the value of CLAUDE_MODEL, or the agent bead's "model" field). The
// match is a substring on the family name — mirroring the controller's
// modelToGroup — so a new dated snapshot (e.g. claude-fable-5-YYYYMMDD) or a
// bare alias flows through without a code change. An empty or unrecognized model
// falls back to the fleet-default 200k window (fail-safe: an unknown model is
// scored against the common window rather than being stranded).
func WindowTokensForModel(model string) int64 {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(m, "fable"):
		return LargeWindowTokens
	case strings.Contains(m, "1m"):
		// Opt-in 1M beta marker on any family (e.g. an "-1m" suffix or a "[1m]"
		// tag), kept forward-compatible for when a non-Fable family runs the
		// long-context beta.
		return LargeWindowTokens
	default:
		return DefaultWindowTokens
	}
}

// EffectiveWindowTokens returns the auto-compaction threshold for a model — the
// token level at which compaction actually fires — which is what an occupancy
// percentage is measured against. It is the model's raw window minus the fixed
// output and summary reserves Claude Code holds back before compacting.
//
// For a 200k window that is 200_000 − 20_000 − 13_000 = 167_000 (~83.5%); for a
// 1M window it is 1_000_000 − 20_000 − 13_000 = 967_000 (~96.7%) — the same
// absolute reserve, a different fraction, which is the whole point.
//
// The reserves are guarded against underflow: a window smaller than the combined
// reserve would yield a non-positive threshold, so such a window falls back to
// its raw size rather than a nonsensical zero/negative denominator. No shipping
// model is that small; the guard is defensive.
func EffectiveWindowTokens(model string) int64 {
	w := WindowTokensForModel(model)
	threshold := w - OutputReserveTokens - SummaryReserveTokens
	if threshold <= 0 {
		return w
	}
	return threshold
}

// HarnessCodex and HarnessGemini are the agent bead's `harness` values for
// Codex and Gemini pods. The window tables and compaction reserves above are
// Claude Code's; none of them describe either.
const (
	HarnessCodex  = "codex"
	HarnessGemini = "gemini"
)

// usesReportedWindow reports whether a harness's context window must come from
// the harness itself rather than the Claude tables.
func usesReportedWindow(harness string) bool {
	switch strings.TrimSpace(harness) {
	case HarnessCodex, HarnessGemini:
		return true
	}
	return false
}

// FieldModelContextWindow is the optional agent-bead field carrying the raw
// context window a harness itself reported (Codex app-server's
// thread/tokenUsage/updated modelContextWindow, ADR 0004 R7; Gemini ACP's
// usage_update.size, ADR 0005 R6). Nothing writes it
// yet; when something does, Codex agents start getting a percentage with no
// change here.
const FieldModelContextWindow = "model_context_window"

// AgentWindow resolves the raw window and the compaction threshold an agent's
// occupancy percentage is measured against, from its bead fields (harness,
// model, model_context_window). known is false when no honest figure exists.
//
// Claude (empty or any non-codex/gemini harness) is exactly WindowTokensForModel /
// EffectiveWindowTokens, so existing agents render unchanged. Codex and Gemini
// have no table: guessing a Claude window for a gpt-* or gemini-* model would
// print a confident, wrong percentage, so it is "unknown" unless the harness
// reported its window. A reported window is used as the threshold directly —
// the harness's auto-compact limit is one we do not know — which overstates headroom
// at worst slightly, never invents a wall.
func AgentWindow(fields map[string]string) (raw, threshold int64, known bool) {
	if !usesReportedWindow(fields["harness"]) {
		m := fields["model"]
		return WindowTokensForModel(m), EffectiveWindowTokens(m), true
	}
	w, err := strconv.ParseInt(strings.TrimSpace(fields[FieldModelContextWindow]), 10, 64)
	if err != nil || w <= 0 {
		return 0, 0, false
	}
	return w, w, true
}

// OccupancyPctForAgent returns the raw context reading `used` (an agent bead's
// context_tokens) as a whole-number percentage of the auto-compaction threshold
// for the agent's harness and model, and (0, false) — no estimate — for a Codex
// or Gemini agent whose window is unknown. It is 0 when used is non-positive. The
// result can exceed 100 briefly (an agent can overshoot the threshold within a
// turn before compaction lands), which is intended: the bar should read "over"
// rather than imply headroom that is gone.
func OccupancyPctForAgent(fields map[string]string, used int64) (pct int, known bool) {
	_, threshold, known := AgentWindow(fields)
	if !known {
		return 0, false
	}
	if used <= 0 || threshold <= 0 {
		return 0, true
	}
	return int(used * 100 / threshold), true
}
