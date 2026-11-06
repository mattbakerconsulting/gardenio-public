// Out-of-band operator re-arm normalization (bug ka-0000004191).
//
// Every ENGINE path that revives a gardener applies an atomic durable reset:
// the no_progress_for re-arm (gardenerResumeOrEscalate), the dormant wake
// (gardenerWakeFromDormant), the second-life grant (gardenerGrantSecondLife) and
// the stalled-open heal (healStalledOpenGardeners) all clear ProgressState,
// stamp RunsFinalizedAfter, zero the resume bookkeeping and refill RunBudget
// in one write — because each of those fields alone re-wedges the gardener:
//
//   - a cleared ProgressState without RunsFinalizedAfter is rebuilt from the
//     gardener's immutable closed-iteration history on the very next cycle
//     (gardenerFinalizeRunsFromIterations), re-tripping no_progress_for;
//   - a stale ResumesWithoutProgress >= ResumeBudget makes the very next
//     premature stop escalate-close the gardener within one reconcile (~30s);
//   - an exhausted RunBudget re-trips the run_budget gate / max_runs kind.
//
// An OPERATOR re-arm at the bead level — `kd update --status open` plus
// enable, the recovery the watchdog playbook and operator decisions reach for
// — hits none of that machinery. The reopened gardener either re-closes within
// a cycle (stale counters) or goes dark in the bare-reconcile no-op with
// last_checked frozen. This is the "re-arm alone insufficient" wedge from
// ka-0000004191 (triage-intake gardener ka-0000003783).
//
// The fingerprint is unambiguous: the engine ONLY stamps the terminate
// markers (gardener_terminated_slot/kind, gardener_stuck_escalated) in the same
// CloseBead call that closes the gardener, and its one sanctioned reopen path
// (gardenerGrantSecondLife) clears them in the same write that reopens. So an
// OPEN gardener still carrying any terminate marker can only mean an
// out-of-band reopen — an operator-level re-arm the engine must finish.

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// gardenerRearmFromOperatorReopen finishes an out-of-band operator re-arm: an
// OPEN gardener trigger whose bead still carries the terminate/escalate markers
// gets the same durable reset every engine revive path applies, plus the
// markers cleared so the fingerprint (and this normalization) is one-shot.
//
// Reset set mirrors gardenerGrantSecondLife minus the counters that rewrite
// history (RunsSpawned stays; RunBudget is topped up to RunsSpawned+refill the
// way the resume re-arm does, so the max_runs gate stops firing without
// zeroing the mirrored top-level run counter).
//
// Called from evalOne AFTER the blocked_by gate (a reopened gardener parked
// behind an open decision stays parked, markers intact, until the operator
// clears it — mirroring the stalled-open heal's blocked_by skip) and BEFORE
// gardenerDiscoverProgress, so this cycle's iteration finalize already honors
// the fresh RunsFinalizedAfter baseline instead of rebuilding the stale
// zero-score tail.
//
// Returns skip=true when the normalization is NEEDED but did not persist
// (marshal/write failure): the caller must skip this trigger for the cycle.
// Proceeding would run the pre-fire gates against the stale counters — a
// ResumesWithoutProgress already at budget escalate-CLOSES the gardener on the
// spot, silently reversing the operator's re-arm. The markers survive the
// failed write, so the next cycle retries the whole normalization. Returns
// skip=false when there is nothing to normalize or the reset persisted.
func gardenerRearmFromOperatorReopen(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) (skip bool) {
	if trig.Gardener == nil {
		return false
	}
	if trig.GardenerTerminatedSlot == "" && trig.GardenerTerminatedKind == "" && !trig.GardenerStuckEscalated {
		return false
	}
	if logger == nil {
		logger = slog.Default()
	}
	// Attribute the writes to the trigger's human operator — comment-create is
	// fail-closed on attribution server-side (ka-0000007275).
	ctx = ctxWithTriggerOperator(ctx, trig)
	cap := trig.Gardener

	updated := rearmedGardenerSpec(cap)

	raw, err := json.Marshal(updated)
	if err != nil {
		logger.Warn("gardener operator-reopen: marshal failed — skipping this cycle so stale counters cannot escalate-close the re-armed gardener",
			"trigger", trig.ID, "error", err)
		return true
	}

	// One write: the reset gardener blob plus the cleared markers. Clearing the
	// markers is what makes this one-shot — the next LoadTriggers parses a
	// clean bead and the fingerprint no longer matches. The blob and markers
	// must land together: markers cleared without the blob reset would strand
	// the stale counters invisibly; blob reset without the markers would
	// re-normalize (harmlessly, but noisily) every cycle. Only the three
	// markers registered on the type:trigger schema are cleared — an
	// unregistered key (e.g. gardener_cred_crashlooped) would 400 the whole
	// PATCH (ka-0000007275 / ka-0000002027).
	fields := map[string]string{
		"gardener":                 string(raw),
		"gardener_terminated_slot": "",
		"gardener_terminated_kind": "",
		"gardener_stuck_escalated": "",
	}
	if err := daemon.UpdateBeadFields(ctx, trig.ID, fields); err != nil {
		logger.Warn("gardener operator-reopen: UpdateBeadFields failed — skipping this cycle (markers intact, retrying next cycle)",
			"trigger", trig.ID, "error", err)
		return true
	}

	comment := fmt.Sprintf(
		"gardener re-armed from operator reopen: bead is open but still carried terminate markers (slot=%q kind=%q stuck_escalated=%t) — the engine only stamps those when it closes a gardener, so this is a bead-level operator re-arm. Applied the durable reset (ProgressState cleared, runs_finalized_after stamped, wall_clock_since stamped to now for a fresh wall_clock window, resume bookkeeping zeroed, run_budget %d→%d) and cleared the markers so the re-arm sticks instead of re-tripping max_runs/no_progress_for/wall_clock within one cycle (ka-0000004191, ka-0000006465).",
		trig.GardenerTerminatedSlot, trig.GardenerTerminatedKind, trig.GardenerStuckEscalated,
		cap.RunBudget, updated.RunBudget)
	if err := daemon.AddComment(ctx, trig.ID, "trigger-engine", comment); err != nil {
		logger.Warn("gardener operator-reopen: AddComment failed", "trigger", trig.ID, "error", err)
	}

	recordGardenerResume(trig.ID, resumeActionReopen, trig.GardenerTerminatedKind)
	setGardenerResumesWithoutProgress(trig.ID, 0)

	// Mutate in place so this cycle's later hooks (finalize, pre-fire gates)
	// already see the normalized state and the gardener can fire NOW rather
	// than one cycle late. updated is a copy of *trig.Gardener, so this
	// changes only the reset fields.
	*trig.Gardener = updated

	logger.Info("gardener re-armed from operator reopen (terminate markers cleared, fresh window)",
		"trigger", trig.ID, "title", trig.Title,
		"terminated_slot", trig.GardenerTerminatedSlot, "terminated_kind", trig.GardenerTerminatedKind,
		"stuck_escalated", trig.GardenerStuckEscalated, "run_budget", updated.RunBudget)
	return false
}

// rearmedGardenerSpec is cap with the operator-reopen reset applied: a
// refilled run budget, zeroed failure and resume bookkeeping, a fresh
// finalize and wall_clock baseline, and the dormant state cleared.
func rearmedGardenerSpec(cap *GardenerSpec) GardenerSpec {
	refill := cap.RunBudgetRefill
	if refill <= 0 {
		refill = defaultRunBudgetRefill
	}
	updated := *cap
	updated.RunBudget = cap.RunsSpawned + refill
	updated.ConsecutiveFailures = 0
	updated.ResumesWithoutProgress = 0
	updated.LastResumeReason = ""
	updated.LastResumeRunsSpawned = 0
	updated.ProgressState = nil
	updated.RunsFinalizedAfter = time.Now()
	// Stamp the per-bead wall_clock baseline to now (bug ka-0000006465). Without
	// this, an operator reopen of a gardener that hard-stopped on wall_clock is
	// structurally undeliverable: the baseline stays at max(created_at, epoch),
	// so gardenerPreFireCheck re-trips the same expired wall_clock on the very
	// next reconcile and re-closes the gardener — the exact reopen→re-close loop
	// this normalization exists to end. Advancing the baseline here grants the
	// re-armed gardener a genuinely fresh wall_clock window on the same bead.
	updated.WallClockSince = time.Now()
	updated.DormantOnEscalation = false
	updated.DormantBlocker = ""
	updated.DormantBlockerKind = ""
	updated.DormantActivityDigest = ""
	updated.DormantSince = time.Time{}
	return updated
}
