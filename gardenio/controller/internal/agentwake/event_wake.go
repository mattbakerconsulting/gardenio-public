package agentwake

// Event-wake (persistent-agents Phase 3, task ka-0000000027). The third way a
// parked persistent identity comes back, alongside the controller
// wake-scanner's TIMER wake (next_respawn_at came due) and `gb wake`'s MANUAL
// wake. When an external EVENT lands on a dormant identity (a mention, a task
// assignment, a decision or escalation it filed being resolved, the CI run it
// waits on concluding) the identity is revived in place and wake_reason
// records WHY.
//
// Two properties distinguish an event-wake from the scanner/`gb wake` revive:
//
//  1. It stamps wake_reason so the controller can surface it to the recreated
//     pod as BOAT_WAKE_REASON: the woken agent reads it for trigger
//     diagnostics ("why am I awake?").
//
//  2. Events reset TTL, NOT budget. The scanner and `gb wake` BUMP respawn_count
//     on every revive, because that budget (respawn_count < max_respawns) is the
//     hard backstop against a self-wake TIMER thrash loop. An event-wake is driven
//     by an EXTERNAL actor, not the identity waking itself, so it is not thrash:
//     it must NOT consume the respawn budget, and it is NOT gated by it. It
//     simply clears next_respawn_at (the timer is moot once the identity is
//     awake) and leaves respawn_count untouched.
//
// This path only WRITES to the bead (reopen + wake metadata); the controller
// reconciler's existing cold-start create-missing loop observes the reopened
// bead and recreates the pod under the same name, which `claude --resume`s the
// conversation. No new pod primitive, no new spawn door.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// EventWakeClient is the daemon subset the event-wake path needs. A narrow
// interface (rather than the full BeadClient) keeps the primitive unit-testable
// with a tiny fake and documents that event-wake only reopens + comments — it
// never creates pods or spawns.
type EventWakeClient interface {
	UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// IsDormantParkedIdentity reports whether a bead is a parked persistent identity
// eligible for an event-wake: a CLOSED type=agent bead carrying successor=same.
// This is the exact selection the wake-scanner and `gb wake` gate on, so the
// three wake paths agree on what "dormant" means. A live agent (open bead), a
// non-agent bead, or a terminally-closed agent with no wake intent is NOT
// dormant and is left alone — an event-wake never resurrects an identity that
// was not parked to persist.
func IsDormantParkedIdentity(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	return b.Type == "agent" &&
		b.Status == "closed" &&
		b.Fields[beadsapi.FieldSuccessor] == beadsapi.SuccessorSame
}

// EventWakeDormant revives a parked persistent identity in response to an
// external event, stamping wake_reason=reason. It reopens the bead (status=open)
// and, in the SAME atomic PATCH (RFC-7386 merge on `fields`; nil deletes):
//   - sets wake_reason=reason (surfaced to the pod as BOAT_WAKE_REASON),
//   - deletes next_respawn_at (the timer wake is moot — the identity is awake),
//   - deletes the terminal agent_state (dormant → cleared, so the recreated pod
//     boots clean and the viewer's dormant bucket empties without flapping).
//
// Deliberately does NOT touch respawn_count: events reset TTL, not budget (see
// the file header). The revive shape is otherwise identical to WakeScanner.wakeOne
// and `gb wake` so the paths never drift.
//
// Returns (true, nil) when it woke the identity. Returns (false, nil) — a
// no-op, not an error — when the bead is not a dormant parked identity (the
// common case: the agent is still live, or was never parked to persist), so the
// caller falls back to its normal spawn path. A goal-unachievable identity is
// also a no-op: it declared its goal deterministically unachievable and must
// never be revived, no matter what wake metadata survives (mirrors wakeOne's
// backstop, ka-0000005843).
func EventWakeDormant(ctx context.Context, daemon EventWakeClient, logger *slog.Logger, bead *beadsapi.BeadDetail, reason, actor string) (bool, error) {
	if daemon == nil || !IsDormantParkedIdentity(bead) {
		return false, nil
	}
	if strings.EqualFold(strings.TrimSpace(bead.Fields[beadsapi.FieldGoalUnachievable]), "true") {
		if logger != nil {
			logger.Info("event-wake: goal_unachievable set, not reviving dormant identity",
				"bead", bead.ID, "agent", bead.Fields["agent"], "reason", reason)
		}
		return false, nil
	}

	open := "open"
	if err := daemon.UpdateBead(ctx, bead.ID, beadsapi.UpdateBeadRequest{
		Status: &open,
		Fields: map[string]any{
			beadsapi.FieldWakeReason:     reason,
			beadsapi.FieldNextRespawnAt:  nil, // timer moot once awake (idempotent clear)
			beadsapi.FieldAgentState:     nil, // dormant → cleared (enum has no "" member → delete)
			beadsapi.FieldStopRequested:  "",  // else the woken pod re-closes itself at boot (gd-NGlLifFXbO)
			beadsapi.FieldSpawnTaskClaim: nil, // a wake is a fresh dispatch: the controller claims the task again (gd-Yo28zKgkGx)
			// respawn_count deliberately untouched — events reset TTL, not budget.
		},
	}); err != nil {
		return false, fmt.Errorf("event-waking dormant agent %s (reason=%s): %w", bead.ID, reason, err)
	}

	if logger != nil {
		logger.Info("event-wake: revived dormant persistent identity",
			"bead", bead.ID, "agent", bead.Fields["agent"], "project", bead.Fields["project"],
			"reason", reason, "actor", actor)
	}

	// Audit comment mirrors the wake-scanner / `gb wake` revive trail so the
	// three wake paths read consistently. Best-effort — a comment failure does
	// not undo the wake.
	agent := strings.TrimSpace(bead.Fields["agent"])
	if agent == "" {
		agent = bead.ID
	}
	comment := fmt.Sprintf(
		"Event-woken (reason=`%s`) by %s: parked persistent identity `%s` reopened so the "+
			"reconciler recreates the pod under the same name and `claude --resume`s the "+
			"conversation. Events reset TTL, not the respawn budget — respawn_count is "+
			"unchanged. (persistent-agents Phase 3 event-wake, ka-0000000027)",
		reason, actorOrSystem(actor), agent)
	if err := daemon.AddComment(ctx, bead.ID, actorOrSystem(actor), comment); err != nil && logger != nil {
		logger.Warn("event-wake: revived bead but audit comment failed", "bead", bead.ID, "error", err)
	}
	return true, nil
}

// actorOrSystem returns actor, or "gardenio" when actor is empty, so a
// machine-origin event-wake still has a non-empty comment author (the daemon's
// fail-closed audit-operator resolver rejects an empty author on a write).
func actorOrSystem(actor string) string {
	if a := strings.TrimSpace(actor); a != "" {
		return a
	}
	return "gardenio"
}
