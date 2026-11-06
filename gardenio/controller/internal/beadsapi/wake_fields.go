package beadsapi

import "strconv"

// Persistent-agents wake/succession field keys (design memo ka-0000001733,
// epic ka-0000006566). These mirror the string constants of the same name in
// cmd/gb (wrapup.go AgentBeadField*, SuccessorSame) — both layers must agree on
// the field name, because the in-pod idle-TTL park path (cmd/gb) WRITES them and
// the controller wake-scanner (internal/statusreporter, task ka-0000004902)
// READS them. Kept here in the shared beadsapi package so the controller-side
// reader never re-declares a drifting copy.
const (
	// FieldSuccessor holds the succession intent stamped at close: "same"
	// records the parked-identity wake intent the scanner keys off. Mirrors
	// cmd/gb SuccessorSame / the type:agent "successor" field registered in
	// internal/bridge/init.go.
	FieldSuccessor = "successor"

	// SuccessorSame is the FieldSuccessor value a persistent agent's idle-TTL
	// park stamps to mark "recreate this same identity after the wake"
	// (cmd/gb SuccessorSame). The wake-scanner only revives beads carrying it.
	SuccessorSame = "same"

	// FieldNextRespawnAt holds the RFC3339 timestamp at which the controller
	// should recreate a parked identity's pod. The idle-TTL park writes it; the
	// wake-scanner scans for next_respawn_at ≤ now and CLEARS it (delete) as it
	// reopens the bead, so the clear is idempotent — a re-run no longer matches.
	// Mirrors cmd/gb AgentBeadFieldNextRespawnAt.
	FieldNextRespawnAt = "next_respawn_at"

	// FieldRespawnCount holds the number of times this identity has been woken
	// (string-coded int). The wake-scanner increments it on each successful wake
	// and gates on it against FieldMaxRespawns to bound infinite self-wake loops
	// (the per-identity respawn budget). Mirrors cmd/gb AgentBeadFieldRespawnCount.
	FieldRespawnCount = "respawn_count"

	// FieldMaxRespawns holds the per-identity override for the respawn budget
	// cap (string-coded int). Absent → DefaultMaxRespawns. Mirrors cmd/gb
	// AgentBeadFieldMaxRespawns.
	FieldMaxRespawns = "max_respawns"

	// FieldAgentState is the agent lifecycle enum field. The wake-scanner clears
	// it (delete) when reopening a parked bead so the recreated pod boots at a
	// clean empty state — agent_state is an enum with no "" member, so the clear
	// MUST be a delete, not an empty string.
	FieldAgentState = "agent_state"

	// FieldStopRequested is the flag `gb stop` sets FIRST on its own bead
	// (cmd/gb/stop.go markStopRequested). gb init's shouldExitBeforeStart
	// re-closes and exits a pod whose bead still reads "true", so every revive
	// must clear it, or the woken pod closes itself at boot without running a
	// turn (gd-NGlLifFXbO). Cleared to "" like the bridge's thread revive and
	// planagent Wake: the gate only exits on the literal "true".
	FieldStopRequested = "stop_requested"

	// FieldWakeReason records WHY a parked persistent identity was last revived
	// (persistent-agents Phase 3 event-wake path, task ka-0000000027). Every
	// engine wake path STAMPS it in the SAME atomic reopen that clears
	// next_respawn_at + agent_state, so the reason travels with the reopened bead:
	//   - the controller wake-scanner stamps WakeReasonTimer (next_respawn_at came due),
	//   - `gb wake` stamps WakeReasonManual (operator on-demand),
	//   - the bridge event-wake path stamps WakeReasonMention / WakeReasonAssignment /
	//     WakeReasonDecision when a mention, assignment, or resolved decision revives
	//     a dormant identity.
	// The controller then SURFACES it to the recreated pod as the BOAT_WAKE_REASON
	// env var (podspec builders) so the woken agent can read why it was woken for
	// trigger diagnostics. It is a plain string field (type:agent config), so the
	// reason values below are accepted on write without an enum registration.
	FieldWakeReason = "wake_reason"

	// AgentStateDormant is the agent_state a PARKED persistent identity carries
	// while it awaits its wake (persistent-agents Phase 2, task ka-0000004675).
	// The idle-TTL park path (cmd/gb idleTTLDespawn) stamps it in the SAME close
	// write that sets successor=same + next_respawn_at, so a parked bead closes
	// as agent_state=dormant rather than agent_state=done — this is the single,
	// ENGINE-SET signal the viewer buckets on. It is deliberately NOT derived by
	// the viewer from (successor=same ∧ future next_respawn_at ∧ closed): both the
	// wake-scanner AND `gb wake` CLEAR agent_state (delete) in the same atomic
	// reopen that clears next_respawn_at, so keying the viewer's dormant bucket on
	// this one engine-owned field means the row transitions dormant → (cleared) →
	// working monotonically and never FLAPS across the respawn window. The daemon
	// registers agent_state as a plain string (type:agent config), so this
	// out-of-builtin-enum value is accepted on write.
	AgentStateDormant = "dormant"

	// FieldCloseReason records why a bead closed. The idle-TTL park stamps
	// "idle_ttl_parked"; the wake-scanner reads it only for audit logging.
	FieldCloseReason = "close_reason"

	// FieldGoalUnachievable is stamped "true" on an agent bead that stopped with
	// goal_unachievable=true (P3, ka-0000005843). The goal-unachievable close
	// already clears the wake metadata (successor→none, next_respawn_at→""), so
	// such a bead should never reach the wake-scanner's due set — but the scanner
	// reads this field as a defensive backstop and refuses to revive a
	// deterministically-failing identity even if a stray successor=same survives.
	// Mirrors cmd/gb AgentBeadFieldGoalUnachievable.
	FieldGoalUnachievable = "goal_unachievable"

	// FieldLineageRootID anchors a succession CHAIN: the first agent bead of a
	// lineage. Every successor=new mint (cmd/gb stop_successor.go) carries it
	// forward unchanged, so counting agent beads sharing a lineage_root_id gives
	// the chain's total length. This is the key the per-lineage continuation
	// budget (ka-0000001361) counts on — distinct from the per-BEAD respawn_count
	// the wake-scanner bumps, which resets to zero on every fresh successor bead
	// and so cannot bound a successor=new self-spawn loop by itself.
	FieldLineageRootID = "lineage_root_id"

	// FieldMaxLineageContinuations holds a per-lineage override for the
	// continuation-budget ceiling (string-coded int). Read from the predecessor
	// bead and carried forward to each successor so the whole chain shares one
	// ceiling. Absent → DefaultMaxLineageContinuations. Mirrors the max_respawns
	// override shape.
	FieldMaxLineageContinuations = "max_lineage_continuations"

	// WakeReason* are the canonical FieldWakeReason values (design memo
	// ka-0000001733 §"wake_reason (timer|mention|assignment|decision|schedule|
	// manual)"). Kept as constants so the wake-scanner, `gb wake`, and the bridge
	// event-wake path never drift on the string, and so the podspec test can
	// assert the exact value surfaced to BOAT_WAKE_REASON.
	//
	// WakeReasonTimer is stamped by the controller wake-scanner when a parked
	// identity's next_respawn_at came due (the idle-TTL timer fired).
	WakeReasonTimer = "timer"
	// WakeReasonManual is stamped by `gb wake` — an operator woke the identity
	// on demand, ahead of its next_respawn_at.
	WakeReasonManual = "manual"
	// WakeReasonMention is stamped by the bridge when a Slack/GitLab mention of a
	// dormant identity revives it.
	WakeReasonMention = "mention"
	// WakeReasonAssignment is stamped by the bridge when a task assignment revives
	// a dormant identity.
	WakeReasonAssignment = "assignment"
	// WakeReasonDecision is stamped by the bridge when a resolved decision revives
	// the dormant identity that filed it (Decisions.respawnAgentForDecision).
	WakeReasonDecision = "decision"
	// WakeReasonEscalation is stamped by the bridge when a resolved (success or
	// partial) escalation revives the dormant identity that filed it
	// (Escalations.respawnRequestingAgent), or wakes a fresh follow-through crew
	// on the requesting task when the identity itself cannot be resumed. Mirrors
	// WakeReasonDecision's contract: gd-t0gm8q62Ac closed the gap where a
	// resolved escalation's verdict reached nobody because the requesting crew
	// had already gone terminal.
	WakeReasonEscalation = "escalation"
	// WakeReasonCI is stamped by the bridge when the CI run an agent parked on
	// (`gb wait ci`, the ci_wait_* fields on its own bead) concludes or times
	// out, reviving the dormant identity in place with the conclusions in hand
	// (plan wake-on-ci-completion.md §2.4, task gd-AwuRDezONo).
	WakeReasonCI = "ci"
)

// DefaultMaxRespawns is the per-identity wake budget applied when a parked
// agent bead carries no max_respawns override. It mirrors the default cap
// documented on cmd/gb AgentBeadFieldMaxRespawns ("Default cap is 5") so the
// wake-scanner's budget gate and the wrap-up budget display agree.
const DefaultMaxRespawns = 5

// DefaultMaxLineageContinuations bounds the total length of a single
// succession CHAIN — how many times a lineage may mint a fresh successor=new
// agent before the budget is spent (ka-0000001361). It is a self-spawn-loop
// backstop, not an expected limit: a real hand-off chain is a handful of links,
// so 20 leaves ample headroom while still capping a runaway loop that would
// otherwise mint a fresh bead+pod on every stop forever.
//
// Deliberately larger than DefaultMaxRespawns (the per-bead wake budget): the
// two count different things. A single persistent identity may legitimately
// park/wake up to DefaultMaxRespawns times AND, across the lineage, be continued
// several times by successor=new; the chain budget bounds the OUTER loop.
const DefaultMaxLineageContinuations = 20

// ReviveAgentFields is the fields half of the one atomic revive PATCH that
// brings an agent identity back: it deletes next_respawn_at (so a later
// wake-scanner pass no longer matches), deletes agent_state (the enum has no ""
// member, and a cleared state is what lets the reconciler recreate the pod
// under the same name), clears stop_requested (FieldStopRequested: left "true"
// by the `gb stop` that parked it, it makes the revived pod re-close itself at
// boot), deletes spawn_task_claim (FieldSpawnTaskClaim: a revive is a fresh
// dispatch, so the controller claims the task again at pod create; a send-back
// may have cleared the assignee while the agent was parked, gd-D83DkxrquP),
// sets respawn_count to respawnCount and stamps reason as wake_reason
// (surfaced to the pod as BOAT_WAKE_REASON). The wake-scanner, `gb wake` and
// the garden's Wake/Restart (internal/gardenhub wake.go) all send it, so the
// three revive paths cannot drift.
func ReviveAgentFields(respawnCount int, reason string) map[string]any {
	return map[string]any{
		FieldNextRespawnAt:  nil,
		FieldAgentState:     nil,
		FieldStopRequested:  "",
		FieldSpawnTaskClaim: nil,
		FieldRespawnCount:   strconv.Itoa(respawnCount),
		FieldWakeReason:     reason,
	}
}
