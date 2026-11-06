package bridge

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// wrapUpTLDRDisplayCap bounds the featured TLDR headline on the card. The
// writer field (gb stop --wrapup, bead ka-0000005783) already validates tldr
// to ~500 chars; this is a tighter display cap so the headline stays roughly
// one line. Used by the mrkdwn section-style render path (formatWrapUpSlack).
const wrapUpTLDRDisplayCap = 300

// wrapUpTLDRHeaderCap bounds the tldr when it is promoted to a Slack *header*
// block on the agent card (buildWrapUpAgentCardBlocks). Slack rejects header
// text longer than 150 chars; we cap a few short of that to leave room for the
// leading outcome glyph and the truncation ellipsis. Longer tldrs are clipped
// for the banner — the full text still lives in the wrap-up field/comment.
const wrapUpTLDRHeaderCap = 140

// slackHeaderTextCap is Slack's hard limit on header-block text. Exceeding it
// fails the whole chat.update with `invalid_blocks`, which does NOT degrade to
// a plainer card — the card is simply never updated, and the prune pass later
// compacts it to the bare "agent finished" line. That is exactly how every
// gardener-style agent lost its tldr banner (ka-0000006181): the composed header
// was glyph + " " + up-to-140-char tldr, and an outcome glyph that
// outcomeHeaderGlyph left unmapped passed through as its 24-char `:shortcode:`
// literal, pushing the total to 165.
const slackHeaderTextCap = 150

// slackHeaderText composes the wrap-up banner (outcome glyph + tldr) and
// guarantees the result fits Slack's header cap no matter how long the glyph
// renders. Truncation is rune-aware: `truncateText` slices bytes, so a cut
// landing mid-rune (an em-dash or emoji inside a tldr) emits invalid UTF-8 —
// a second, independent source of `invalid_blocks`.
func slackHeaderText(glyph, tldr string) string {
	text := strings.TrimSpace(glyph + " " + tldr)
	return truncateRunes(text, slackHeaderTextCap)
}

// truncateRunes clips s to at most maxLen runes, appending an ellipsis when it
// clips. Unlike truncateText it never splits a multi-byte rune.
func truncateRunes(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return string(r[:maxLen])
	}
	return string(r[:maxLen-3]) + "..."
}

// priorTurnTLDRPrefix marks a headline that was recovered from prior_wrapup —
// the wrap-up the resume-in-place path archived before nil-deleting the live
// `wrapup` field (ka-0000003692). It MUST be visible so a previous turn's
// summary can never masquerade as the current turn's outcome; that visibility
// is what preserves the anti-stale intent of the original nil-delete while
// still giving the operator a headline for the whole resumed turn instead of a
// blank card.
const priorTurnTLDRPrefix = "Previous turn: "

// priorWrapupTLDR extracts the tl;dr from the archived prior_wrapup field, if
// any. prior_wrapup stores the full JSON `wrapup` payload of the previous turn
// (bot_agent_kill.go revive path), so it parses with the same decoder as the
// live field. Returns "" when there is no archived wrap-up or it carries no
// tldr — callers gate the fallback on the presence of prior_wrapup itself, NOT
// on prior_agent_id (which the same-id revive deliberately deletes).
func priorWrapupTLDR(fields map[string]string) string {
	if fields == nil {
		return ""
	}
	pw := strings.TrimSpace(fields["prior_wrapup"])
	if pw == "" {
		return ""
	}
	w, ok := parseWrapUp(pw)
	if !ok {
		return ""
	}
	return strings.TrimSpace(w.TLDR)
}

// parsedWrapUp is the bridge-side decode of the agent bead's `wrapup` field
// (written by `gb stop --wrapup '{...}'`). It mirrors the producer-side WrapUp
// struct in cmd/gb/wrapup.go — only the fields the Slack card renders.
type parsedWrapUp struct {
	// TLDR is a validated one-line summary (gb stop --wrapup, bead
	// ka-0000005783). When present it is surfaced as the FIRST/featured line on
	// every render path (bead ka-0000006621).
	TLDR            string            `json:"tldr"`
	Accomplishments string            `json:"accomplishments"`
	Blockers        string            `json:"blockers"`
	HandoffNotes    string            `json:"handoff_notes"`
	BeadsClosed     []string          `json:"beads_closed"`
	PullRequests    []string          `json:"pull_requests"`
	Custom          map[string]string `json:"custom"`

	// GoalSatisfied / RespawnAfter mirror the producer-side WrapUp fields
	// (cmd/gb/wrapup.go). When an agent has a terminal_goal and answers
	// goal_satisfied:"no", the reconciler re-spawns this identity — so the run
	// did not "end", it is *progressing* and will resume. RespawnAfter is the
	// requested delay ("10m", "1h", or "immediate"). These drive the
	// :hourglass_flowing_sand: progressing outcome (bead ka-0000000754).
	GoalSatisfied string `json:"goal_satisfied"`
	RespawnAfter  string `json:"respawn_after"`

	// GoalUnachievable is the crash-loop circuit breaker (P3, ka-0000005843):
	// the run declared its goal DETERMINISTICALLY impossible, so it is terminal
	// and will NOT resume — even if goal_satisfied:"no" is also set. It overrides
	// the progressing classifier so the card never falsely promises a respawn.
	GoalUnachievable bool `json:"goal_unachievable"`

	// RestartWhen is the agent's own free-text declaration of when/why it
	// expects to resume — the "wake-trigger bead" arm of the restart-condition
	// card (bead ka-0000007398, parent ka-0000000754): e.g. "when ka-X merges"
	// or "when the operator resolves decision ka-Y". Rendered verbatim as the
	// restart-condition line and (like respawn_after) counted as a *progressing*
	// signal, since the agent has stated it will come back. The producer-side
	// nudge that populates this field is sibling task ka-0000005780; until it
	// ships no agent sets it, so this field is fully inert (mirrors how the
	// consumer read `successor` before its producer landed). Free text — kept
	// as-declared so a bead id inside it survives to the reader.
	RestartWhen string `json:"restart_when"`

	// Successor is the persistent-agents succession intent (ka-0000005086,
	// design memo §2.5): one of {none, same, new}, empty == none. Only "new"
	// is live in Phase 1 — it means this agent's work continues on a
	// freshly-minted successor agent (new name + new card), and the predecessor
	// is closed with close_reason "superseded: continued as <id>". The bridge
	// reads it to render the forward "🔗 Continued as <successor>" link on the
	// retired card (successorContinuationLine). "same" records intent only until
	// the Phase 2 wake scanner lands (task ka-0000002976) — it renders exactly
	// like a terminal card today. Empty/none is fully inert, so this field is
	// safe to read before the producer-side enum (task ka-0000002751) ships.
	Successor string `json:"successor"`
}

// parseWrapUp decodes a wrapup JSON string into a parsedWrapUp. The second
// return is false when the payload is empty or not valid JSON, letting callers
// fall back to rendering the raw text.
func parseWrapUp(wrapupJSON string) (*parsedWrapUp, bool) {
	wrapupJSON = strings.TrimSpace(wrapupJSON)
	if wrapupJSON == "" {
		return nil, false
	}
	var w parsedWrapUp
	if err := json.Unmarshal([]byte(wrapupJSON), &w); err != nil {
		return nil, false
	}
	return &w, true
}

// wrapUpNegativeSentinels are the filler values agents commonly type into a
// free-text wrap-up field to mean "nothing here". Treating them as real
// content mislabels an otherwise-clean run — e.g. `"blockers":"none"` would
// stamp a card ⛔ blocked even though the agent merged work and was never
// blocked. wrapUpFieldEmpty folds these to empty so they don't drive the
// outcome classifier.
var wrapUpNegativeSentinels = map[string]bool{
	"none": true, "n/a": true, "na": true, "nil": true, "null": true,
	"-": true, "—": true, "no": true, "nope": true, "nothing": true,
}

// wrapUpFieldEmpty reports whether a free-text wrap-up field is effectively
// empty — either literally blank or one of the filler sentinels agents use to
// say "nothing". Comparison is trimmed, lowercased, and ignores a trailing
// period (so "none." matches too).
func wrapUpFieldEmpty(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.TrimRight(t, ".")
	t = strings.TrimSpace(t)
	return t == "" || wrapUpNegativeSentinels[t]
}

// wrapUpProgressing reports whether a wrap-up declares that the run is not
// finished but expects to resume — i.e. the controller will re-spawn this
// identity. Two self-declared signals from the producer-side WrapUp drive it
// (cmd/gb/wrapup.go):
//
//   - goal_satisfied=="no": the agent has a terminal_goal it did not meet this
//     run, so the reconciler re-spawns it (budget-capped). The literal "no" is
//     the only resume signal; "yes"/empty are not.
//   - respawn_after non-empty: the agent explicitly requested a re-spawn delay.
//   - restart_when non-empty: the agent declared a wake-trigger condition (the
//     "wake-trigger bead" arm, bead ka-0000007398) — it has stated it will come
//     back, so the run is progressing even without a respawn timer.
//
// This is purely wrap-up-driven so it stays unit-testable without bead access;
// the gardener-refire resume path surfaces via the restart condition line from
// the agent bead's schedule_* fields (restartConditionLine), not this
// classifier — a gardener run wraps with goal_satisfied unset yet still refires.
func wrapUpProgressing(w *parsedWrapUp) bool {
	if w == nil {
		return false
	}
	// goal_unachievable is terminal and overrides every resume signal below: a
	// deterministically-unachievable run will NOT respawn, so the card must not
	// render a progressing "will resume" glyph even when goal_satisfied:"no" or a
	// respawn_after is also present (P3, ka-0000005843).
	if w.GoalUnachievable {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(w.GoalSatisfied), "no") {
		return true
	}
	if strings.TrimSpace(w.RespawnAfter) != "" {
		return true
	}
	return strings.TrimSpace(w.RestartWhen) != ""
}

// Successor enum values, mirroring the producer-side WrapUp.Successor
// (cmd/gb/wrapup.go, task ka-0000002751) and the design memo §2.5. Empty is
// treated as successorNone everywhere.
const (
	successorNone = "none"
	successorSame = "same"
	successorNew  = "new"
)

// wrapUpSuccessor normalizes a wrap-up's succession intent to a lowercase enum
// value ("none" | "same" | "new"), folding empty/absent to "none". Whitespace
// and case are ignored so `"New"` and `" new "` both resolve to "new".
func wrapUpSuccessor(w *parsedWrapUp) string {
	if w == nil {
		return successorNone
	}
	switch strings.ToLower(strings.TrimSpace(w.Successor)) {
	case successorSame:
		return successorSame
	case successorNew:
		return successorNew
	default:
		// Anything unrecognized (including "" and stray values a mis-written
		// wrap-up might carry) is inert — the card renders as a normal terminal.
		return successorNone
	}
}

// wrapUpSuccessorNew reports whether a wrap-up declares successor=new — the one
// live Phase 1 succession mode. When true the card is retired in favor of a
// freshly-minted successor agent, and the bridge renders a forward
// "🔗 Continued as <successor>" link on the retired card.
func wrapUpSuccessorNew(w *parsedWrapUp) bool {
	return wrapUpSuccessor(w) == successorNew
}

// supersededCloseReasonPrefix is the close_reason the successor-mint path
// (task ka-0000006931) stamps on a predecessor agent bead when it is retired in
// favor of a new successor: "superseded: continued as <successor-id>". The
// bridge parses the trailing id back out to render the forward continuation
// link, so the exact prefix is a load-bearing contract between the two tasks.
const supersededCloseReasonPrefix = "superseded: continued as "

// successorIDFromCloseReason extracts the successor bead/agent id from a
// predecessor's "superseded: continued as <id>" close_reason (design memo §2.5,
// written by task ka-0000006931). Returns "" when the reason is empty or does
// not carry the superseded prefix, so callers can fall back to a bare
// "Continued in a new card" line with no link. Matching is case-insensitive on
// the prefix; the id itself is preserved verbatim.
func successorIDFromCloseReason(closeReason string) string {
	cr := strings.TrimSpace(closeReason)
	if len(cr) < len(supersededCloseReasonPrefix) {
		return ""
	}
	if !strings.EqualFold(cr[:len(supersededCloseReasonPrefix)], supersededCloseReasonPrefix) {
		return ""
	}
	return strings.TrimSpace(cr[len(supersededCloseReasonPrefix):])
}

// wrapUpOutcome classifies a terminal agent run into a theme glyph + label
// for the wrap-up card header. The outcome reflects both the agent's lifecycle
// state and the contents of its wrap-up (glyphs shown for the Garden theme):
//
//	🌳 done        — clean finish, no blockers, no handoff
//	🥀 blocked     — wrap-up reported blockers
//	🌾 progressing — run paused but the controller will resume it (respawn armed)
//	🌿 continued   — successor=new: work continues on a freshly-minted agent
//	🪴 handed-off  — wrap-up has handoff notes but no blockers (work passed on)
//	🍄 aborted     — agent reached a failed lifecycle state
//
// Blockers/HandoffNotes are matched via wrapUpFieldEmpty, so filler values like
// "none" or "n/a" do NOT count as real content — an agent that handed off work
// and wrote `"blockers":"none"` lands on handed-off, not blocked.
//
// Precedence: aborted > blocked > progressing > continued > handed-off > done.
// Blocked outranks progressing so a genuine operator-attention signal is never
// masked by a pending respawn; progressing outranks continued/handed-off/done
// because a respawn resumes THIS identity whereas continued mints a fresh one;
// continued outranks handed-off because succession is a stronger, structural
// handoff (the successor card carries the work forward, bead ka-0000005086).
//
// glyph is the theme's unicode glyph; label is the human-readable word the
// glyph must stay glued to wherever it is rendered.
func wrapUpOutcome(th Theme, state string, w *parsedWrapUp) (glyph, label string) {
	if state == "failed" {
		return th.Glyph(LifecycleAborted), "aborted"
	}
	if w != nil {
		if !wrapUpFieldEmpty(w.Blockers) {
			return th.Glyph(LifecycleBlocked), "blocked"
		}
		if wrapUpProgressing(w) {
			return th.Glyph(LifecycleRestarted), "progressing"
		}
		if wrapUpSuccessorNew(w) {
			return th.Glyph(LifecycleContinued), "continued"
		}
		if !wrapUpFieldEmpty(w.HandoffNotes) {
			return th.Glyph(LifecycleHandedOff), "handed-off"
		}
	}
	return th.Glyph(LifecycleDone), "done"
}

// platformFailureLabel / platformFailureTLDR describe a zero-inference-turn
// agent — one that never got a model response before it despawned
// (ka-0000000315). It renders with the theme's aborted glyph: the locked glyph
// vocabulary has no dedicated platform-failure glyph.
const (
	platformFailureLabel = "platform failure"
	platformFailureTLDR  = "Platform failure — the agent never got an inference turn (no model response before teardown)"
)

// zeroInferenceTurn reports whether an agent bead's usage fields prove the run
// never completed a single inference turn — a hard platform failure (a model
// outage such as the 2026-07-25 opus-5 incident, or a pod-startup infra fault
// before first inference) rather than an agent that did real work and forgot to
// wrap up. It is the mirror of stopGateFallbackTLDR's `toolCalls > 0` branch
// (ka-0000000083): the never-started case instead of the did-work case.
//
// The signal is total_request_count >= 1 (a request was actually attempted) yet
// total_input_tokens == 0 AND total_tool_calls == 0 (no turn's context ever
// reached the model and no tool ever ran) — exactly the state the bug reported
// (total_request_count=1, total_input_tokens=0, total_tool_calls=0). Requiring
// a counted request keeps legacy/absent-usage beads (all three fields empty)
// out of the classification so their existing headline is preserved.
func zeroInferenceTurn(fields map[string]string) bool {
	if fields == nil {
		return false
	}
	if parseTurnField(fields["total_request_count"]) < 1 {
		return false
	}
	return parseTurnField(fields["total_input_tokens"]) == 0 &&
		parseTurnField(fields["total_tool_calls"]) == 0
}

// hasHumanWrapUp reports whether a parsed wrap-up carries a genuine,
// agent-authored tl;dr (not the synthesized fallback the close path writes when
// none was submitted). A zero-inference agent provably cannot have produced one
// — it never got a turn — so this is a defense-in-depth guard: it ensures the
// platform-failure override never clobbers a real human-delivered headline even
// if the usage fields are ever stale or mis-stamped (ka-0000000315).
func hasHumanWrapUp(w *parsedWrapUp) bool {
	if w == nil || strings.TrimSpace(w.TLDR) == "" {
		return false
	}
	return strings.TrimSpace(w.Custom["synthesized"]) != "true"
}

// closeReasonTLDR derives a one-line tl;dr headline from a terminal agent
// bead's close_reason when the bead carries no wrap-up tldr of its own
// (ka-0000000101). It is the render-side complement to the synthesized
// fallback wrap-up written at close time (gb hook stop-gate): together they
// guarantee a closed agent card always has a headline rather than rendering
// metadata-only.
//
// The mapping recognizes the common machine-written close reasons (the
// stop_gate_* family from closeAgentBeadOnExit, plus reaper/teardown reasons
// like context_cancelled / podless_leak / stale / orphan) and renders a human
// phrase. Anything unrecognized is surfaced verbatim so no signal is lost.
// Returns "" for an empty reason so callers can fall through to metadata-only.
func closeReasonTLDR(closeReason string) string {
	cr := strings.TrimSpace(closeReason)
	if cr == "" {
		return ""
	}
	lower := strings.ToLower(cr)
	switch {
	case strings.HasPrefix(lower, "superseded: continued as"):
		// successor=new retirement (design memo §2.5, task ka-0000006931): the
		// predecessor was closed in favor of a freshly-minted successor. This is
		// not a failure or an idle-out — name the continuation so the card reads
		// as a deliberate handoff rather than an unexplained close. The forward
		// link to the successor is rendered separately (successorContinuationLine).
		return "Work continued on a new agent card"
	case strings.HasPrefix(lower, "stop_gate_") && strings.Contains(lower, "_involuntary"):
		// An account-exhaustion / rotation / pod-teardown death mid-work. The
		// write side (closeAgentBeadOnExit) stamps `stop_gate_<reason>_involuntary`
		// precisely so this render never reads "idled out" — the agent had real
		// work in flight (ka-0000003464 item 3a). Note this case MUST precede the
		// bare `stop_gate_cooldown` prefix below, which the involuntary reason
		// would otherwise match and mislabel.
		return "Session ended involuntarily mid-work (account exhaustion/rotation) without an explicit wrap-up"
	case strings.HasPrefix(lower, "stop_gate_cooldown"):
		return "Session idled out without an explicit wrap-up"
	case strings.HasPrefix(lower, "stop_gate_escape_hatch"):
		return "Session stopped after repeated stop-gate blocks without an explicit wrap-up"
	case strings.HasPrefix(lower, "stop_gate_rate_limited"):
		return "Session ended rate-limited without an explicit wrap-up"
	case strings.HasPrefix(lower, "stop_gate_"):
		return "Session closed by the stop gate without an explicit wrap-up"
	case strings.HasPrefix(lower, "upstream_api_error"):
		// An outage-driven death (ka-0000000202): the Claude upstream was failing
		// (5xx / overloaded / 529 / transient network drop) and the agent was torn
		// down before it recovered. Name the cause honestly so the card does not
		// read as agent fault or routine node churn.
		return "Died during a Claude upstream outage (5xx / overloaded) — work lost through no fault of the agent"
	case strings.HasPrefix(lower, "rate_limit_livelock"):
		// A rate-limit livelock give-up (ka-0000001209): every re-inject landed
		// back on a rate limit with no successful turn, so the harness gave up
		// after the bound and requeued the task. Usually an account spend cap,
		// but also any permanent error coop classes as a rate limit, so the card
		// does not assert the cause; the close_reason carries the last error
		// (gd-a0jdFgwngn). It reads as a limit, not an idle-out or agent fault.
		return "Gave up after every retry hit a rate limit with no successful turn — task requeued for a fresh agent"
	case strings.HasPrefix(lower, "context_cancelled"):
		return "Pod terminated before an explicit wrap-up was submitted"
	case strings.HasPrefix(lower, "podless_leak"):
		return "Reaped: agent pod was gone before it wrapped up"
	case strings.HasPrefix(lower, "stale"):
		return "Reaped as stale before an explicit wrap-up"
	case strings.HasPrefix(lower, "orphan"):
		return "Reaped as orphaned before an explicit wrap-up"
	default:
		return fmt.Sprintf("Closed: %s", cr)
	}
}

// failureReasonTLDR derives a one-line tl;dr from a terminal agent bead's
// failure_reason / agent_state when it carries neither a wrap-up tldr nor a
// recognizable close_reason (Fix B, ka-0000005547). This rescues legacy beads
// closed before the write-side synthesize-fallback fix — most notably the
// gvisor-poc HOLD canaries, which have an empty close_reason,
// failure_reason=killed-by-operator, and agent_state=killed, so the
// close_reason path never fired and the card rendered metadata-only.
//
// Precedence: the more specific failure_reason is consulted first, then the
// agent_state. Returns "" when neither yields a known mapping so callers can
// fall through to metadata-only.
func failureReasonTLDR(failureReason, agentState string) string {
	fr := strings.ToLower(strings.TrimSpace(failureReason))
	switch {
	case fr == "":
		// no failure_reason — fall through to agent_state below
	case strings.HasPrefix(fr, "killed-by-operator"), strings.HasPrefix(fr, "killed_by_operator"):
		return "Killed by operator"
	case strings.HasPrefix(fr, "killed"):
		return "Pod killed before wrap-up"
	default:
		return fmt.Sprintf("Failed: %s", strings.TrimSpace(failureReason))
	}

	switch strings.ToLower(strings.TrimSpace(agentState)) {
	case "killed":
		return "Pod killed before wrap-up"
	case "failed":
		return "Agent failed before an explicit wrap-up"
	default:
		return ""
	}
}

// restartConditionMarker is the leading mrkdwn glyph + label every
// restart-condition line shares ("↻ *Restart:* …"). Factored out so the three
// mechanism-specific renderers stay in lockstep.
func restartConditionMarker(th Theme) string {
	return th.Glyph(LifecycleRestarted) + " *Restart:* "
}

// restartConditionLine renders a single human-readable line describing when and
// why the controller will bring this agent identity back — the operator's
// "restart condition" (bead ka-0000000754, Matt's "think how cool it would be
// if the agent printed its restart condition"). Returns "" when no restart is
// expected, so callers can omit the line entirely.
//
// It covers the three live resume mechanisms named on the parent epic
// (ka-0000000754), each rendered as its own literal line:
//
//	respawn        "Resumes ~20:30 (respawn 2/5)"      — reconciler respawn: a
//	               terminal_goal not met (goal_satisfied:no) or an explicit
//	               respawn_after; next_respawn_at / respawn_count / max_respawns
//	               are the controller's authoritative scheduled-respawn state.
//	wake-trigger   "Wakes when ka-X merges"            — the agent's own declared
//	               restart_when (bead ka-0000007398); populated by sibling nudge
//	               ka-0000005780, inert until then.
//	gardener refire "Gardener refires (run 3/8)"         — a scheduled gardener loop
//	               that has runs left in its budget (schedule_runs_spawned /
//	               schedule_max_runs).
//
// Precedence: respawn > wake-trigger > gardener refire. A concrete
// controller-scheduled respawn is the strongest, most specific promise; an
// agent's explicitly-named wake condition outranks the generic gardener cadence.
//
// fields may be nil (e.g. legacy card re-render without bead access); the line
// then degrades to the wrap-up-only respawn form. The returned string is plain
// mrkdwn with a leading ↻ glyph and no trailing newline.
func restartConditionLine(th Theme, w *parsedWrapUp, fields map[string]string) string {
	if line := respawnRestartLine(th, w, fields); line != "" {
		return line
	}
	if line := wakeTriggerRestartLine(th, w); line != "" {
		return line
	}
	return gardenerRefireRestartLine(th, fields)
}

// respawnRestartLine renders the reconciler-respawn arm of the restart
// condition, or "" when no respawn is armed. Armed-ness here is deliberately
// NARROWER than wrapUpProgressing: only next_respawn_at (controller state),
// goal_satisfied:no, or respawn_after count — NOT restart_when, which is the
// wake-trigger arm and must not be mislabeled "Resumes when respawn fires".
func respawnRestartLine(th Theme, w *parsedWrapUp, fields map[string]string) string {
	var nextAt, respawnCount, maxRespawns string
	if fields != nil {
		nextAt = strings.TrimSpace(fields["next_respawn_at"])
		respawnCount = strings.TrimSpace(fields["respawn_count"])
		maxRespawns = strings.TrimSpace(fields["max_respawns"])
	}

	respawnAfter := ""
	goalUnmet := false
	if w != nil {
		respawnAfter = strings.TrimSpace(w.RespawnAfter)
		goalUnmet = strings.EqualFold(strings.TrimSpace(w.GoalSatisfied), "no")
	}
	if nextAt == "" && respawnAfter == "" && !goalUnmet {
		return ""
	}

	// When the respawn lands: prefer the controller's concrete timestamp, fall
	// back to the agent's requested delay, else a generic phrasing.
	when := "Resumes when respawn fires"
	switch {
	case nextAt != "":
		when = fmt.Sprintf("Resumes ~%s", restartWhenDisplay(nextAt))
	case respawnAfter != "":
		if strings.EqualFold(respawnAfter, "immediate") || respawnAfter == "0" {
			when = "Resumes immediately"
		} else {
			when = fmt.Sprintf("Resumes after %s", respawnAfter)
		}
	}

	// Budget annotation: "(respawn 2/5)" when both counters are known, else
	// whichever single counter we have.
	line := restartConditionMarker(th) + when
	if budget := restartBudgetDisplay(respawnCount, maxRespawns); budget != "" {
		line += " " + budget
	}
	return line
}

// wakeTriggerRestartLine renders the wake-trigger arm from the agent's own
// declared restart_when (bead ka-0000007398). The text is rendered verbatim
// after a "Wakes " lead-in, unless the agent already phrased it as a full
// clause ("Wakes when …"/"Waiting …"/"Resumes …"), in which case it is used
// as-is so we don't produce "Wakes Wakes when …". Returns "" when unset (the
// steady state until the producer nudge ka-0000005780 ships).
func wakeTriggerRestartLine(th Theme, w *parsedWrapUp) string {
	if w == nil {
		return ""
	}
	rw := strings.TrimSpace(w.RestartWhen)
	if rw == "" {
		return ""
	}
	lower := strings.ToLower(rw)
	switch {
	case strings.HasPrefix(lower, "wake"),
		strings.HasPrefix(lower, "wait"),
		strings.HasPrefix(lower, "resume"):
		return restartConditionMarker(th) + rw
	default:
		return restartConditionMarker(th) + "Wakes " + rw
	}
}

// gardenerRefireRestartLine renders the gardener-loop arm: a scheduled gardener
// that has runs remaining in its budget refires on its cadence. Driven purely
// by the schedule_* fields stamped on a gardener agent bead at spawn
// (stampGardenerLoopContext / scheduler.spawnScheduledAgent). Returns "" for a
// non-gardener agent (no schedule_runs_spawned), or a gardener on its LAST run
// (runs_spawned >= max_runs with a positive cap) — that run will not refire,
// so promising a restart would be wrong. A non-positive/absent cap is treated
// as an unbounded (standing) loop that always refires.
func gardenerRefireRestartLine(th Theme, fields map[string]string) string {
	if fields == nil {
		return ""
	}
	spawnedStr := strings.TrimSpace(fields["schedule_runs_spawned"])
	if spawnedStr == "" {
		return "" // not a gardener run
	}
	spawned, err := strconv.Atoi(spawnedStr)
	if err != nil {
		return ""
	}
	maxRuns, _ := strconv.Atoi(strings.TrimSpace(fields["schedule_max_runs"]))

	// Bounded budget already fully spent → this is the final run, no refire.
	if maxRuns > 0 && spawned >= maxRuns {
		return ""
	}

	line := restartConditionMarker(th) + "Gardener refires"
	switch {
	case maxRuns > 0:
		line += fmt.Sprintf(" (run %d/%d)", spawned, maxRuns)
	case spawned > 0:
		line += fmt.Sprintf(" (run %d)", spawned)
	}
	return line
}

// restartWhenDisplay normalizes an RFC3339 next_respawn_at into a compact
// display string. On parse failure it returns the raw value so nothing is lost.
func restartWhenDisplay(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.UTC().Format("15:04 MST")
}

// restartBudgetDisplay formats the respawn budget counters into "(respawn N/M)"
// (or a partial form), or "" when neither counter is a usable integer.
func restartBudgetDisplay(count, max string) string {
	switch {
	case count != "" && max != "":
		return fmt.Sprintf("(respawn %s/%s)", count, max)
	case count != "":
		return fmt.Sprintf("(respawn %s)", count)
	case max != "":
		return fmt.Sprintf("(respawn cap %s)", max)
	}
	return ""
}

// successorContinuationLine renders the forward "🔗 Continued as <successor>"
// line the bridge stamps on a predecessor agent's terminal wrap-up card when it
// declared successor=new (design memo §2.5, task ka-0000005086). It is the
// operator-visible half of card chaining: the retired card points at the fresh
// successor so a life-thread stays navigable in Slack, mirroring the
// beads-viewer's display-only succession edge (task ka-0000007852).
//
// Returns "" unless the wrap-up declares successor=new, so callers can omit the
// line entirely — the feature is fully inert until an agent actually chains.
//
// The successor id comes from the predecessor's own close_reason
// ("superseded: continued as <id>", written by task ka-0000006931), passed in
// as closeReason. When that id is present and looks like a bead id it is
// rendered as a beads-viewer deep link; otherwise the line degrades to a
// linkless "Continued in a new card" so nothing is lost before the mint path
// ships. b may be nil (beadLink is nil-safe), keeping this unit-testable.
func (b *Bot) successorContinuationLine(th Theme, w *parsedWrapUp, closeReason string) string {
	if !wrapUpSuccessorNew(w) {
		return ""
	}
	successorID := successorIDFromCloseReason(closeReason)
	if successorID == "" {
		return th.Glyph(LifecycleContinued) + " *Continued* in a new card"
	}
	return fmt.Sprintf("%s *Continued as* %s", th.Glyph(LifecycleContinued), b.beadLink(successorID, successorID))
}

// formatWrapUpSlack parses a structured wrapup JSON string and renders it as
// a Slack-formatted text block. Returns an empty string if parsing fails.
//
// This is the legacy thread-reply renderer (used by postThreadStateReply and
// postScheduleStateNotification). The agent card uses the richer block layout
// in buildWrapUpAgentCardBlocks instead.
//
// When the wrap-up carries a validated `tldr` field (gb stop --wrapup, bead
// ka-0000005783), it is surfaced as the FIRST line — a prominent one-line
// summary the operator reads without scrolling (bead ka-0000006621). Rendered
// defensively: if tldr is absent/empty the output is unchanged — no empty top
// line.
func (b *Bot) formatWrapUpSlack(th Theme, wrapupJSON string) string {
	w, ok := parseWrapUp(wrapupJSON)
	if !ok {
		return fmt.Sprintf("\n> %s", truncateText(wrapupJSON, 500))
	}

	var parts []string
	// Featured TLDR headline first, when present. Capped, then escaped exactly
	// once for mrkdwn so a long or markup-bearing summary can't distort the
	// card. The tldr is stored RAW since ka-0000005806, but legacy beads carry
	// pre-escaped "&lt;"/"&gt;"; unescape first so escapeSlackMrkdwn produces a
	// single, correct escape for both (mirrors the header path's
	// unescapeSlackMrkdwn at bot_agents.go).
	if tldr := strings.TrimSpace(w.TLDR); tldr != "" {
		parts = append(parts, fmt.Sprintf("%s *%s*", th.Semantic(SemNote),
			escapeSlackMrkdwn(unescapeSlackMrkdwn(truncateText(tldr, wrapUpTLDRDisplayCap)))))
	}
	if w.Accomplishments != "" {
		parts = append(parts, fmt.Sprintf("*Accomplishments:* %s", truncateText(w.Accomplishments, 500)))
	}
	if w.Blockers != "" {
		parts = append(parts, fmt.Sprintf("*Blockers:* %s", truncateText(w.Blockers, 300)))
	}
	if w.HandoffNotes != "" {
		parts = append(parts, fmt.Sprintf("*Handoff:* %s", truncateText(w.HandoffNotes, 300)))
	}
	// Restart condition (wrap-up-only form; this legacy thread-reply path has no
	// bead fields). Surfaces "↻ Restart: …" when a respawn is self-declared.
	if rc := restartConditionLine(th, w, nil); rc != "" {
		parts = append(parts, rc)
	}
	if len(w.BeadsClosed) > 0 {
		linked := make([]string, len(w.BeadsClosed))
		for i, id := range w.BeadsClosed {
			linked[i] = b.beadLink(id, id)
		}
		parts = append(parts, fmt.Sprintf("*Beads closed:* %s", strings.Join(linked, ", ")))
	}
	if len(w.PullRequests) > 0 {
		parts = append(parts, fmt.Sprintf("*PRs:* %s", strings.Join(w.PullRequests, ", ")))
	}
	for k, v := range w.Custom {
		parts = append(parts, fmt.Sprintf("*%s:* %s", k, truncateText(v, 200)))
	}

	if len(parts) == 0 {
		return ""
	}
	return "\n" + strings.Join(parts, "\n")
}
