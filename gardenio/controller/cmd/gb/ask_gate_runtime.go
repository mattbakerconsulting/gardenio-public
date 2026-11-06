package main

// ask_gate_runtime.go — runtime wiring for the gb stop ask-gate: the SOFT
// one-shot marker (anti-loop invariant), the decision-OR-escalation handle
// check, ask-specific remediation, and structured telemetry.
//
// Design invariants (epic ka-0000002541):
//   - SOFT + one-shot: a detected ask fires exactly ONE loud nudge; a second
//     identical stop self-releases. A never-respawn despawn-class role
//     (thread/crew/auto-inspector/fsm-triage) therefore gets one nudge and is
//     NEVER trapped in a restart loop. This is the anti-ka-0000007734 invariant.
//   - Decision OR escalation: requireOperatorHandle passes if EITHER handle
//     exists, authored by this agent. The boundary between the two is fuzzy;
//     the goal is simply that a handle exists.
//   - Always a zero-decision exit: every remediation names the rephrase path
//     and --force, so a status-note-misclassified-as-ask is never wedged.
//   - Fail-open on daemon-unreachable: a list-call error never blocks a stop.
//   - WARN default (ship_soft): in warn mode the gate detects, nudges, and
//     records telemetry but ALWAYS allows the stop.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// askGateMarkerFile records that the ask-gate already nudged this session, so a
// second identical stop self-releases (the one-shot invariant). Mirrors the
// hook.go stopGateCooldownFile machinery. Variable (not const) so tests can
// override it.
var askGateMarkerFile = "/tmp/gb-stop-ask-gate-nudged"

// despawnClassRoles are roles whose agents NEVER respawn after gb stop. For
// these, a hard block would be a permanent trap (the pod is gone, nothing
// re-reads the gate), so the gate is ALWAYS soft one-shot for them regardless
// of mode and the remediation prefers escalation (a decision has no live agent
// to feed the answer back to). Read from BOAT_ROLE.
var despawnClassRoles = map[string]bool{
	"thread":         true,
	"crew":           true,
	"auto-inspector": true,
	"fsm-triage":     true,
	"jira-mention":   true,
}

// isDespawnClassRole reports whether the current BOAT_ROLE is (or includes) a
// never-respawn role. BOAT_ROLE is a first-class COMMA-SEPARATED multi-role
// string across the fleet (podmanager preserves "crew,thread" verbatim), so a
// whole-string lookup would miss a despawn-class role buried in a multi-role
// value. We split on "," and return true if ANY component is despawn-class —
// matching the controller-wide multi-role handling and keeping the soft-only
// guarantee for any agent that carries a despawn-class role.
func isDespawnClassRole() bool {
	for r := range strings.SplitSeq(os.Getenv("BOAT_ROLE"), ",") {
		if despawnClassRoles[strings.TrimSpace(r)] {
			return true
		}
	}
	return false
}

// askGateAlreadyNudged reports whether this session already received the
// one-shot ask-gate nudge (marker file present).
func askGateAlreadyNudged() bool {
	_, err := os.Stat(askGateMarkerFile)
	return err == nil
}

// recordAskGateNudge writes the one-shot marker so the next identical stop
// self-releases. It RETURNS the write error so the caller can fail OPEN if the
// marker cannot be persisted: the one-shot self-release relies entirely on the
// marker being readable next pass, so a silent write failure (read-only /tmp,
// full disk, restricted seccomp) would otherwise turn an enforce-mode block
// into an unbounded restart loop with no escape hatch — the exact
// anti-ka-0000007734 invariant the gate must never violate.
func recordAskGateNudge() error {
	return os.WriteFile(askGateMarkerFile, fmt.Appendf(nil, "%d\n", time.Now().Unix()), 0o644)
}

// clearAskGateNudge removes the one-shot marker (called on a clean,
// ask-free stop so a later genuine ask in the same long-lived session can
// still fire once).
func clearAskGateNudge() {
	_ = os.Remove(askGateMarkerFile)
}

// askGateResult is the outcome of evaluating the ask-gate, returned for
// telemetry and to decide whether to block the stop.
type askGateResult struct {
	detected bool
	kind     AskKind
	// filed records how the ask was satisfied or why it passed:
	// "decision" | "escalation" | "referenced_open" | "none".
	filed string
	// block is true only when the gate should reject this stop (enforce mode,
	// first fire, ask unhandled). Always false in warn mode and on the
	// self-release second pass.
	block bool
	// disposition is the telemetry verb for the structured log:
	// "filed" | "warned" | "blocked" | "released" | "forced" | "clean".
	disposition string
}

// evaluateAskGate is the single entry point the stop flow calls. It detects an
// operator ask in the wrap-up, checks whether the agent already filed (or
// referenced) a handle, applies the SOFT one-shot + mode policy, emits the
// remediation nudge and structured telemetry, and reports whether to block.
//
// It NEVER returns block=true in warn mode, on a self-release second pass, or
// when --force was passed. It fails open (no block, disposition="clean") if the
// wrap-up carries no ask.
func evaluateAskGate(ctx context.Context, actor, agentID string, wrapup *WrapUp, force bool) askGateResult {
	mode := askGateMode()
	if mode == AskGateOff {
		return askGateResult{disposition: "clean"}
	}

	kind := classifyAskKind(wrapup)
	if kind == AskNone {
		// No ask — clean stop. Clear any stale one-shot marker so a future
		// genuine ask in a long-lived (respawning) session can still fire.
		clearAskGateNudge()
		return askGateResult{disposition: "clean"}
	}

	res := askGateResult{detected: true, kind: kind}

	// Handle check: did this agent already file a decision or escalation, or
	// reference an open one? Fail-open on daemon errors.
	handle := findOperatorHandle(ctx, actor, wrapup)
	if handle != "" {
		res.filed = handle
		res.disposition = "filed"
		emitAskGateTelemetry(ctx, agentID, actor, res)
		stampAskGateBead(ctx, agentID, res)
		clearAskGateNudge()
		return res
	}
	// Gardener-handoff bucket (audit taxonomy B, ka-0000000039). A crew that
	// punts the ask UP to its gardener AND backs that punt with a REAL referenced
	// handle — a gb mail it sent, or an issue bead it handed off (assigned to
	// another agent) — has filed a handle, just to its gardener rather than the
	// operator. Accept it so the crew is not stranded/nudged. BUT the audit
	// showed gardeners then ALSO sometimes never file: a punt left only as a
	// referenced bead + prose in handoff_notes is often dropped because nobody
	// re-reads it. So when the gate accepts a gardener-handoff handle it ALSO
	// emits a squawk, surfacing the ask in Slack so the gardener (and operator)
	// inherit a VISIBLE handle. Best-effort and never blocks.
	if ref := gardenerHandoffRef(ctx, actor, wrapup); ref != "" {
		emitGardenerPuntSquawk(ctx, actor, ref, wrapup)
		res.filed = "gardener_handoff"
		res.disposition = "handed_off"
		emitAskGateTelemetry(ctx, agentID, actor, res)
		stampAskGateBead(ctx, agentID, res)
		clearAskGateNudge()
		return res
	}
	res.filed = "none"

	// --force always releases (explicit override), but still record telemetry
	// so a forced-after-flag rate is measurable.
	if force {
		res.disposition = "forced"
		emitAskGateTelemetry(ctx, agentID, actor, res)
		stampAskGateBead(ctx, agentID, res)
		return res
	}

	// SOFT one-shot: a second identical stop self-releases. Despawn-class roles
	// are ALWAYS soft one-shot (never trap a never-respawn pod), and so is warn
	// mode. Only enforce mode + non-despawn + first fire blocks.
	if askGateAlreadyNudged() {
		res.disposition = "released"
		emitAskGateTelemetry(ctx, agentID, actor, res)
		stampAskGateBead(ctx, agentID, res)
		clearAskGateNudge()
		return res
	}

	// First fire: emit the loud, ask-specific nudge and record the one-shot
	// marker. If the marker cannot be persisted, we MUST fail OPEN (warn, not
	// block): without a readable marker the self-release on the next pass cannot
	// fire, so blocking would loop forever. The marker write failure is the only
	// path that can defeat the one-shot, so it is the only one that downgrades a
	// would-be block to a warn.
	emitAskRemediation(os.Stderr, kind, wrapup)
	markerPersisted := recordAskGateNudge() == nil

	if mode == AskGateEnforce && !isDespawnClassRole() && markerPersisted {
		res.block = true
		res.disposition = "blocked"
	} else {
		res.disposition = "warned"
		if !markerPersisted {
			fmt.Fprintf(os.Stderr, "[ask-gate] one-shot marker could not be persisted (%s) — failing open (warn) to avoid a block loop\n", askGateMarkerFile)
		}
	}
	emitAskGateTelemetry(ctx, agentID, actor, res)
	stampAskGateBead(ctx, agentID, res)
	return res
}

// findOperatorHandle returns a non-empty handle descriptor if this agent has a
// usable operator re-entry handle for the detected ask:
//   - "decision"        — agent authored an open decision bead, OR
//   - "escalation"      — agent authored an escalation bead, OR
//   - "referenced_open" — the wrap-up names a gd- id that is a live
//     decision/escalation (the healthy "defer to an open handle" bucket).
//
// Fails OPEN: a daemon-unreachable error on either list call returns "" only if
// neither check succeeded, and the caller treats a best-effort miss as
// non-fatal in warn mode. To avoid blocking a real stop on a flaky daemon, a
// list error is treated as "cannot prove absence" → we return a sentinel that
// callers map to pass. We implement that by returning "daemon_unreachable".
func findOperatorHandle(ctx context.Context, actor string, wrapup *WrapUp) string {
	// Fail open when we cannot reliably attribute beads to this agent: no
	// daemon, an empty actor, or the literal "unknown" sentinel that
	// defaultActor() returns when KD_ACTOR is unset and git user.name is empty.
	// With actor=="unknown" the created_by join below would never match a real
	// decision/escalation, so we'd wrongly classify a handled stop as stranded
	// and (in enforce mode) block it. Treat these as unverifiable → pass.
	if daemon == nil || actor == "" || actor == "unknown" {
		return "unverifiable"
	}

	sawError := false

	// 1. Decision authored by this agent.
	if decisions, err := daemon.ListDecisionBeads(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Note: ask-gate could not list decisions: %v\n", err)
		sawError = true
	} else {
		for _, b := range decisions {
			if b != nil && b.CreatedBy == actor {
				return "decision"
			}
		}
	}

	// 2. Escalation authored by this agent.
	if escs, err := daemon.ListEscalations(ctx, "", 0); err != nil {
		fmt.Fprintf(os.Stderr, "Note: ask-gate could not list escalations: %v\n", err)
		sawError = true
	} else {
		for _, e := range escs {
			if e.Escalation != nil && e.Escalation.CreatedBy == actor {
				return "escalation"
			}
		}
	}

	// 3. Referenced an already-open handle by id (the 47% healthy "defer"
	// bucket). Corroborate any gd- id in the wrap-up against the live
	// decision/escalation sets.
	if refs := wrapupReferencesKDHandle(wrapup); len(refs) > 0 {
		if openHandleExists(ctx, refs) {
			return "referenced_open"
		}
	}

	// If a list call errored and we found nothing, we cannot prove the ask is
	// unhandled — fail open so a flaky daemon never wedges a stop.
	if sawError {
		return "unverifiable"
	}
	return ""
}

// openHandleExists reports whether any of the referenced gd- ids is a live
// (open) decision or escalation bead. Best-effort: errors are swallowed.
func openHandleExists(ctx context.Context, refs []string) bool {
	if daemon == nil {
		return false
	}
	refSet := make(map[string]bool, len(refs))
	for _, r := range refs {
		refSet[r] = true
	}
	// Check live decisions.
	if decisions, err := daemon.ListDecisionBeads(ctx); err == nil {
		for _, b := range decisions {
			if b != nil && refSet[b.ID] && !beadIsClosed(b.Status) {
				return true
			}
		}
	}
	// Check live escalations.
	if escs, err := daemon.ListEscalations(ctx, "", 0); err == nil {
		for _, e := range escs {
			if e.Escalation != nil && refSet[e.Escalation.ID] && !beadIsClosed(e.Escalation.Status) {
				return true
			}
		}
	}
	return false
}

// gardenerHandoffRef returns a live, referenced gd- handle that backs a crew's
// punt-to-gardener — a gb mail the crew SENT (type=mail authored by this actor)
// or an issue bead the crew HANDED OFF (assigned to an agent other than itself)
// — else "". It is the corroboration half of the taxonomy-B follow-up
// (ka-0000000039): the wrap-up's PROSE (wrapupPuntsToGardener) is what asserts the
// recipient is the gardener, and this confirms a REAL handoff artifact exists so
// the ask is genuinely handed off, not stranded as bare prose.
//
// It is deliberately NOT a gardener-identity match: a gardener's canonical
// gardener_id is a trigger-bead id (or a synthetic bucket), not the agent handle
// a mail is addressed to, so an exact recipient match is impossible for the mail
// path and unreliable for the handoff path. The punt-to-gardener prose gate plus
// a live, crew-authored mail (or a bead assigned away from the crew) is the
// robust signal. Fail-closed on a lookup miss: a bead that cannot be read or is
// already closed does not count.
func gardenerHandoffRef(ctx context.Context, actor string, wrapup *WrapUp) string {
	if daemon == nil || !wrapupPuntsToGardener(wrapup) {
		return ""
	}
	refs := wrapupReferencesKDHandle(wrapup)
	if len(refs) == 0 {
		return ""
	}
	self := strings.TrimSpace(actor)
	for _, id := range refs {
		b, err := daemon.GetBead(ctx, id)
		if err != nil || b == nil || beadIsClosed(b.Status) {
			continue
		}
		// A gb mail the crew SENT (to its gardener): a live type=mail bead this
		// agent authored.
		if strings.EqualFold(strings.TrimSpace(b.Type), "mail") &&
			self != "" && strings.EqualFold(strings.TrimSpace(b.CreatedBy), self) {
			return id
		}
		// A bead HANDED OFF: live and assigned to an agent other than the crew.
		if a := strings.TrimSpace(b.Assignee); a != "" && !strings.EqualFold(a, self) {
			return id
		}
	}
	return ""
}

// emitGardenerPuntSquawk posts an operator/gardener-visible squawk so a crew's
// accepted punt-to-gardener inherits a VISIBLE handle (ka-0000000039). Mirrors
// runSquawk (create a type=message bead, then close it) so the Slack bridge
// relays it. Best-effort: every failure is logged to stderr and swallowed — a
// squawk hiccup must never wedge a gb stop.
func emitGardenerPuntSquawk(ctx context.Context, actor, ref string, wrapup *WrapUp) {
	if daemon == nil {
		return
	}
	text := fmt.Sprintf("Gardener handoff (%s): a crew punted an operator-directed ask up to its gardener — "+
		"gardener, please pick it up and file a decision/escalation if operator input is needed.", ref)
	if snippet := firstOperatorAskSnippet(wrapup); snippet != "" {
		text = fmt.Sprintf("Gardener handoff (%s): %s — gardener, please pick it up and file a "+
			"decision/escalation if operator input is needed.", ref, snippet)
	}
	if len(text) > 500 {
		text = text[:497] + "..."
	}
	fields, err := json.Marshal(map[string]string{"source_agent": actor, "text": text})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: ask-gate could not marshal gardener-punt squawk: %v\n", err)
		return
	}
	id, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     text,
		Type:      "message",
		Kind:      "data",
		Labels:    []string{"squawk", "gardener-handoff", fmt.Sprintf("from:%s", actor)},
		Fields:    json.RawMessage(fields),
		CreatedBy: actor,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Note: ask-gate could not emit gardener-punt squawk: %v\n", err)
		return
	}
	if err := daemon.CloseBead(ctx, id, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Note: ask-gate could not close gardener-punt squawk %s: %v\n", id, err)
	}
}

// beadIsClosed reports whether a status string denotes a closed/resolved bead.
func beadIsClosed(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "closed", "resolved", "done", "cancelled", "canceled":
		return true
	}
	return false
}

// emitAskRemediation writes the loud, ask-specific nudge to w. It NAMES the
// detected ask shape, points at the right command, and ALWAYS offers a
// zero-decision exit (rephrase the wrap-up, or --force) — the explicit
// anti-ka-0000007734 loop escape.
func emitAskRemediation(w io.Writer, kind AskKind, wrapup *WrapUp) {
	snippet := firstOperatorAskSnippet(wrapup)

	fmt.Fprintf(w, "\n[ask-gate] Your wrap-up parks an operator-directed ask but files no decision or escalation:\n")
	if snippet != "" {
		fmt.Fprintf(w, "    > %s\n", snippet)
	}
	fmt.Fprintf(w, "An operator-directed ask left only in tldr/blockers/handoff_notes gives the operator NO re-entry handle")
	if isDespawnClassRole() {
		fmt.Fprintf(w, " — and your role never respawns, so it is lost forever")
	}
	fmt.Fprintf(w, ".\n\n")

	switch kind {
	case AskEscalation:
		fmt.Fprintf(w, "This looks like a DO-ask (the operator must perform a privileged action). File an escalation:\n")
		fmt.Fprintf(w, "  gb escalation create --title \"<action>\" --operation-type <type> \\\n")
		fmt.Fprintf(w, "    --operation-spec '{...}' --requesting-task <your-task-bead> [--script ...]\n")
	default:
		fmt.Fprintf(w, "This looks like a CHOOSE-ask (the operator must pick between divergent next actions). File a decision:\n")
		fmt.Fprintf(w, "  gb decision create --prompt \"<question>\" \\\n")
		fmt.Fprintf(w, "    --options '[{\"id\":\"a\",\"short\":\"...\",\"label\":\"...\",\"artifact_type\":\"plan\"},{\"id\":\"b\",...}]' && gb yield\n")
		fmt.Fprintf(w, "(If the operator must instead DO something privileged, use `gb escalation create`.)\n")
	}

	// Crew-punt-to-gardener visibility (T7, ka-0000000039): if the handoff punts
	// the ask UP to a gardener, that is a recognized pattern — but the audit
	// showed gardeners then ALSO sometimes never file. Remind the agent to leave
	// the gardener a VISIBLE handle (a squawk or a handed-off bead the gardener
	// will see), not just prose in handoff_notes.
	if wrapupPuntsToGardener(wrapup) {
		fmt.Fprintf(w, "You appear to be punting this to your gardener. Gardeners sometimes miss a prose handoff —\n")
		fmt.Fprintf(w, "leave a VISIBLE handle: `gb squawk` the ask (or hand off a bead addressed to the gardener) so it is not lost.\n")
	}

	fmt.Fprintf(w, "\nIf this was NOT an ask (a status note, a rhetorical question, a reference to an already-open handle):\n")
	fmt.Fprintf(w, "  restate the wrap-up WITHOUT the question, or re-run with --force to stop anyway.\n")
	fmt.Fprintf(w, "(This is a one-shot nudge: an identical stop will be allowed through.)\n\n")
}

// firstOperatorAskSnippet returns a short excerpt of the first forward-looking
// field that triggered detection, for the remediation message.
func firstOperatorAskSnippet(w *WrapUp) string {
	if w == nil {
		return ""
	}
	for _, f := range []string{w.Blockers, w.HandoffNotes, w.TLDR} {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		// Single-shot classification on just this field.
		if wrapupHasOperatorAsk(&WrapUp{Blockers: f}) {
			return truncateSnippet(f, 160)
		}
	}
	// Fall back to the first non-empty field.
	for _, f := range []string{w.Blockers, w.HandoffNotes, w.TLDR} {
		if s := strings.TrimSpace(f); s != "" {
			return truncateSnippet(s, 160)
		}
	}
	return ""
}

func truncateSnippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ") // collapse whitespace
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// emitAskGateTelemetry writes a structured one-line log so the 23%-stranded
// rate is measurable post-ship and the cue table can be tuned from real
// closures (T6). Format is key=value pairs on a single line, prefixed for grep.
func emitAskGateTelemetry(_ context.Context, agentID, actor string, res askGateResult) {
	fmt.Fprintf(os.Stderr,
		"[ask-gate-telemetry] ask_detected=%t ask_kind=%s role=%s mode=%s disposition=%s filed=%s agent=%s actor=%s\n",
		res.detected, res.kind, envOr("BOAT_ROLE", "unknown"), askGateMode(),
		res.disposition, res.filed, agentID, actor,
	)
}

// stampAskGateBead records stranded_ask_detected / stranded_ask_filed on the
// agent bead so the stranded rate is queryable in beads-viewer and the cue
// table is tunable from real data (T6). Best-effort: failures are non-fatal and
// never block a stop. Only stamps when an ask was detected (keeps the field
// space clean for the common no-ask stop).
func stampAskGateBead(ctx context.Context, agentID string, res askGateResult) {
	if daemon == nil || agentID == "" || !res.detected {
		return
	}
	// "filed" is true ONLY when a real handle exists (the agent authored a
	// decision/escalation or referenced an open one). "unverifiable" is the
	// fail-open sentinel — "could not prove the ask is unhandled" — and is
	// recorded as its own distinct value so post-ship analysis can separate
	// genuinely-handled asks from un-checkable ones (it does NOT inflate the
	// filed=true rate).
	var filed string
	switch res.filed {
	case "decision", "escalation", "referenced_open":
		filed = "true"
	case "gardener_handoff":
		// A handle exists, but it was handed to the GARDENER rather than the
		// operator. Recorded as its own value so post-ship analysis can measure
		// the "gardeners then also sometimes never file" tail (ka-0000000039)
		// separately from a direct operator handle.
		filed = "gardener_handoff"
	case "unverifiable":
		filed = "unverifiable"
	default:
		filed = "false"
	}
	fields := map[string]string{
		"stranded_ask_detected":    "true",
		"stranded_ask_kind":        res.kind.String(),
		"stranded_ask_disposition": res.disposition,
		"stranded_ask_filed":       filed,
	}
	if err := daemon.UpdateBeadFields(ctx, agentID, fields); err != nil {
		// Non-fatal — telemetry already captured the event in the log line.
		fmt.Fprintf(os.Stderr, "Note: ask-gate could not stamp agent bead %s: %v\n", agentID, err)
	}
}

// compile-time assurance the beadsapi import is used even if a refactor drops
// the only reference.
var _ = beadsapi.EscalationDetail{}
