// Second-life adjudication for gardeners (bead ka-0000004010, epic ka-0000001575).
//
// The resume budget in gardener_evaluator.go (ResumeBudget /
// ResumesWithoutProgress) re-arms a gardener that is STILL OPEN — a premature
// loop_end (out of runs/time/patience) converts to a re-arm instead of a close
// so an unmet terminal_goal is not silently abandoned. Second-life is the
// complementary, COARSER brake one level up: it revives a gardener that has
// ALREADY been CLOSED on max_runs without meeting its goal (the "disappointed
// gardener" cohort). The reaper's §5b second-life-review lane adjudicates that
// cohort and, when it decides a gardener has earned another loop, calls
// gardenerGrantSecondLife.
//
// Mechanic: REOPEN-SAME (not clone-fresh). Granting a second life reopens the
// SAME trigger/schedule bead — status back to open, RunsSpawned reset, RunBudget
// bumped (bounded), SecondLifeCount incremented, and the terminate markers
// cleared — so the gardener's whole history (runs, comments, lineage) stays on one
// bead for audit-trail continuity. A clone-fresh bead would lose that thread.
//
// Guardrail (Matt's explicit "don't resurrect the doomed"): MaxSecondLives caps
// resurrections — the direct mirror of terminal-goals max_respawns. Once
// SecondLifeCount >= MaxSecondLives (or MaxSecondLives < 0, second-life
// disabled), a grant does NOT reopen — it ESCALATES to an operator decision by
// flagging the bead + commenting, exactly as the resume-budget-exhausted path
// escalates rather than looping into the void (advice ka-0000000974). The engine
// only FLAGS the escalation — it never authors the decision bead itself (no agent
// context here); the reaper / resumer watchdog opens the decision for the
// operator, the same division of labor gardenerResumeOrEscalate uses.

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Second-life defaults (bead ka-0000004010). A gardener with no explicit
// MaxSecondLives gets defaultMaxSecondLives resurrections — the same default-5
// cap terminal-goals max_respawns uses. Each granted life refills RunBudget by
// defaultRunBudgetRefill (shared with the resume path) so the reopened gardener
// has headroom to fire again.
const (
	defaultMaxSecondLives = 5
	// gardenerSecondLifeExhaustedLabel marks a gardener that has spent its entire
	// second-life budget while its terminal_goal is still unmet. The reaper's
	// second-life-review lane / resumer watchdog lists gardeners carrying this
	// label and opens an operator decision (grant more lives / redefine goal /
	// rest). The engine only flags — it never creates the decision bead itself
	// (mirrors gardenerStuckEscalateLabel for the resume path).
	gardenerSecondLifeExhaustedLabel = "gardener-second-life-exhausted"
)

// gardenerSecondLifeBudget reports a gardener's effective second-life cap and
// whether second-life is disabled. A zero MaxSecondLives means "use the
// default"; a negative value disables second-life entirely (a max_runs close
// stays closed — the legacy behavior, an explicit opt-out).
func gardenerSecondLifeBudget(cap *GardenerSpec) (budget int, disabled bool) {
	if cap.MaxSecondLives < 0 {
		return 0, true
	}
	if cap.MaxSecondLives == 0 {
		return defaultMaxSecondLives, false
	}
	return cap.MaxSecondLives, false
}

// gardenerGrantSecondLife revives a gardener that was CLOSED on max_runs without
// meeting its terminal_goal, OR escalates to an operator decision once the
// second-life budget is spent. It is the coarse, post-closure analogue of
// gardenerResumeOrEscalate (which re-arms an OPEN gardener).
//
// Grant (SecondLifeCount < budget and second-life not disabled):
//   - reopen the SAME bead (status=open) — the reopen seam the schedule source's
//     wedged-trigger self-heal uses (ka-0000006681);
//   - reset RunsSpawned to 0 so the gardener starts a fresh run window;
//   - bump RunBudget (bounded by the refill) so the max_runs gate stops firing;
//   - increment SecondLifeCount (cumulative across the gardener's whole history);
//   - clear the terminate markers (gardener_terminated_slot/kind,
//     gardener_stuck_escalated) so the revived gardener is not mistaken for a
//     still-terminated one;
//   - persist the updated gardener blob + reset top-level counters.
//
// Escalate (SecondLifeCount >= budget, or second-life disabled): leave the bead
// CLOSED, flag it gardener-second-life-exhausted, and comment — the operator
// (via reaper / watchdog) decides whether to grant more lives. This is the
// don't-resurrect-the-doomed brake.
//
// Returns true when a second life was granted (bead reopened), false when the
// grant escalated instead. Best-effort: write failures are logged, not
// propagated — the caller's adjudication state is the source of truth.
func gardenerGrantSecondLife(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, reason string) bool {
	if logger == nil {
		logger = slog.Default()
	}
	// Operator attribution for the comment/label writes below — comment-create
	// is fail-closed on attribution server-side (ka-0000007275).
	ctx = ctxWithTriggerOperator(ctx, trig)
	if trig.Gardener == nil {
		// No gardener block to carry second-life state — nothing to revive.
		logger.Warn("gardener second-life: no gardener block, cannot grant",
			"trigger", trig.ID)
		return false
	}
	cap := trig.Gardener

	budget, disabled := gardenerSecondLifeBudget(cap)

	// Exhausted (or disabled) → escalate, do NOT reopen. The doomed stay dead
	// until an operator says otherwise.
	if disabled || cap.SecondLifeCount >= budget {
		comment := fmt.Sprintf(
			"gardener second-life DENIED: second_life_count=%d, max_second_lives=%d — budget spent (or second-life disabled). Leaving closed and escalating to operator decision (don't-resurrect-the-doomed guardrail). Reason offered: %s",
			cap.SecondLifeCount, budget, reason)
		if err := daemon.AddComment(ctx, trig.ID, "trigger-engine", comment); err != nil {
			logger.Warn("gardener second-life escalate: AddComment failed",
				"trigger", trig.ID, "error", err)
		}
		// Best-effort here (unlike the resume/cred-crashloop escalates, which
		// gate their close on the label landing): the bead is ALREADY closed on
		// this path, and the reaper's second-life-review lane re-adjudicates the
		// closed-on-max_runs cohort every sweep, so a failed label write is
		// retried on the next adjudication. gardenerAddLabel logs the failure.
		_ = gardenerAddLabel(ctx, daemon, logger, trig.ID, gardenerSecondLifeExhaustedLabel)
		recordGardenerSecondLife(trig.ID, secondLifeActionEscalate)
		logger.Info("gardener second-life escalated (budget exhausted)",
			"trigger", trig.ID, "second_life_count", cap.SecondLifeCount, "max_second_lives", budget)
		return false
	}

	// Grant: reopen the SAME bead with reset counters and bumped budget.
	refill := cap.RunBudgetRefill
	if refill <= 0 {
		refill = defaultRunBudgetRefill
	}
	updated := *cap
	updated.SecondLifeCount = cap.SecondLifeCount + 1
	updated.RunsSpawned = 0
	updated.RunBudget = refill
	// Clear the resume/dormant runtime state so the revived gardener gets a clean
	// window — a fresh life should not inherit the stale fruitless-resume streak
	// or no-progress history that helped close it. RunsFinalizedAfter makes the
	// ProgressState reset DURABLE (ka-0000003265): without it,
	// gardenerFinalizeRunsFromIterations rebuilds the pre-revival zero-score tail
	// from the immutable closed-iteration history on the next cycle, so a revived
	// gardener that scored zero before could instantly re-trip no_progress_for and
	// dead-lock again instead of getting the fresh window a second life is for.
	updated.ConsecutiveFailures = 0
	updated.ResumesWithoutProgress = 0
	updated.LastResumeReason = ""
	updated.LastResumeRunsSpawned = 0
	updated.ProgressState = nil
	updated.RunsFinalizedAfter = time.Now()
	// Stamp the per-bead wall_clock baseline to now (bug ka-0000006465). A
	// second life reopens the SAME bead, so the wall_clock baseline would
	// otherwise stay pinned at max(created_at, epoch) — and a gardener closed on
	// (or old enough to trip) hard_stop wall_clock would re-close on the next
	// reconcile, silently undoing the grant. Advancing the baseline gives the
	// revived gardener a genuinely fresh wall_clock window, exactly as the
	// run_budget refill gives it a fresh run window.
	updated.WallClockSince = time.Now()
	updated.DormantOnEscalation = false
	updated.DormantBlocker = ""
	updated.DormantBlockerKind = ""
	updated.DormantActivityDigest = ""
	updated.DormantSince = time.Time{}

	raw, err := json.Marshal(updated)
	if err != nil {
		logger.Warn("gardener second-life grant: marshal failed — not reviving",
			"trigger", trig.ID, "error", err)
		return false
	}

	// Reopen the bead first (status=open), then persist the gardener blob + reset
	// the mirrored top-level counters and clear the terminate markers. Reopen is
	// the load-bearing step; if it fails we abort without touching counters so a
	// retry sees a consistent closed gardener.
	open := "open"
	if err := daemon.UpdateBead(ctx, trig.ID, beadsapi.UpdateBeadRequest{Status: &open}); err != nil {
		logger.Warn("gardener second-life grant: reopen (UpdateBead) failed — not reviving",
			"trigger", trig.ID, "error", err)
		return false
	}

	fields := map[string]string{
		"gardener": string(raw),
		// Reset ONLY runs_spawned; runs_completed is intentionally NOT zeroed.
		// The two counters live on independent paths (gardenerPostFire bumps
		// runs_spawned at fire; completion accounting bumps runs_completed when a
		// spawned agent closes), so after a second life a gardener legitimately
		// reads runs_completed > runs_spawned — the pre-reset completions still
		// count, the pre-reset spawns are forgiven for the fresh run_budget lap.
		// This is cosmetic counter divergence, NOT a bug (noted for the next
		// auditor: bug ka-0000004754 observed runs_spawned=352, runs_completed=407
		// on a second-life gardener and had to re-derive that this is expected).
		"runs_spawned": "0",
		// Clear the terminate markers so the revived gardener is not mistaken for
		// a still-terminated one by the cohort scans / beads-viewer.
		"gardener_terminated_slot": "",
		"gardener_terminated_kind": "",
		"gardener_stuck_escalated": "",
	}
	if err := daemon.UpdateBeadFields(ctx, trig.ID, fields); err != nil {
		// The bead is already reopened but the incremented SecondLifeCount /
		// reset RunBudget did NOT persist. Do NOT mutate in-memory state or
		// count the grant — that would make the metric and the cap observe a
		// life that the persisted blob never recorded (the don't-resurrect-the-
		// doomed cap would stop bounding observable behavior). Return false:
		// the gardener is left OPEN with stale counters, so the next reconcile's
		// run_budget gate routes it through gardenerResumeOrEscalate and the
		// grant is effectively retried/superseded — no divergence, no
		// double-count.
		logger.Warn("gardener second-life grant: UpdateBeadFields failed after reopen — not counting grant; gardener left open for the resume gate to re-adjudicate",
			"trigger", trig.ID, "error", err)
		return false
	}

	// Re-ARM the revived gardener (bug ka-0000001423). Terminal-close now disarms
	// the trigger (enabled=false) so a retired lane reads closed+disabled; a
	// second-life reopen must therefore flip enabled back to true or the revived
	// gardener stays filtered out of LoadTriggers (which drops enabled=false) and
	// never fires again — a silent revival that reopens the bead but leaves it
	// dark. Separate typed write because enabled is boolean-typed and cannot ride
	// the string field map above (ka-0000003627). Best-effort: a failed re-arm
	// leaves the gardener open-but-disabled, which the operator-reopen
	// normalization / operator can still finish by enabling — strictly better
	// than the pre-disarm world only if it lands, so log loudly on failure.
	if err := daemon.UpdateBeadFieldsTyped(ctx, trig.ID, map[string]any{"enabled": true}); err != nil {
		logger.Warn("gardener second-life grant: re-arm (enabled=true) write failed — gardener reopened but still disabled; enable it to resume firing",
			"trigger", trig.ID, "error", err)
	}

	comment := fmt.Sprintf(
		"gardener GRANTED second life %d/%d: reopened (status→open), runs_spawned reset 0, run_budget→%d. Terminal_goal unmet but trending — continuing on the SAME bead for audit-trail continuity. Reason: %s",
		updated.SecondLifeCount, budget, updated.RunBudget, reason)
	if err := daemon.AddComment(ctx, trig.ID, "trigger-engine", comment); err != nil {
		logger.Warn("gardener second-life grant: AddComment failed",
			"trigger", trig.ID, "error", err)
	}

	// Mutate in place so later gates this cycle observe the revived state.
	trig.Gardener.SecondLifeCount = updated.SecondLifeCount
	trig.Gardener.RunsSpawned = updated.RunsSpawned
	trig.Gardener.RunBudget = updated.RunBudget
	trig.Gardener.ConsecutiveFailures = updated.ConsecutiveFailures
	trig.Gardener.ResumesWithoutProgress = updated.ResumesWithoutProgress
	trig.Gardener.LastResumeReason = updated.LastResumeReason
	trig.Gardener.LastResumeRunsSpawned = updated.LastResumeRunsSpawned
	trig.Gardener.ProgressState = updated.ProgressState
	trig.Gardener.RunsFinalizedAfter = updated.RunsFinalizedAfter
	trig.Gardener.WallClockSince = updated.WallClockSince
	trig.Gardener.DormantOnEscalation = updated.DormantOnEscalation
	trig.Gardener.DormantBlocker = updated.DormantBlocker
	trig.Gardener.DormantBlockerKind = updated.DormantBlockerKind
	trig.Gardener.DormantActivityDigest = updated.DormantActivityDigest

	recordGardenerSecondLife(trig.ID, secondLifeActionGrant)
	logger.Info("gardener granted second life (reopened)",
		"trigger", trig.ID, "second_life_count", updated.SecondLifeCount,
		"max_second_lives", budget, "run_budget", updated.RunBudget)
	return true
}
