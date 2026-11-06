// Free-function gardener stop-spec evaluator. Counterpart to
// (*Scheduler).evaluateStopEntry that takes a minimal context so the bridge's
// type=trigger gardener fire path (Phase 3b/B6) can reuse the same StopSpec
// shape without depending on *Scheduler or Schedule.
//
// Kinds covered here are the subset that work off gardener counters and
// daemon-side reads. no_progress_for is now supported on the trigger path in
// BOTH forms — count-based (`runs`) and duration-based (`duration`/`<minutes>`)
// — off the ProgressState the bridge fire path finalizes each run into. The
// remaining scheduler-only kinds (no_actionable_state,
// yielded_then_woken_then_idle) are deferred and treated as not-met when seen on
// a trigger gardener — they require run-state machinery that does not yet exist
// on type=trigger.

package scheduler

import (
	"context"
	"hash/fnv"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler/conditions"
	"gardenio/controller/internal/scheduler/progress"
)

// GardenerStopContext is the per-trigger input needed to evaluate a StopSpec
// without depending on *Scheduler. The bridge's gardener-shaped fire path
// populates this from a TriggerDef + GardenerSpec.
type GardenerStopContext struct {
	// BeadID is the gardener (trigger) bead ID. Used by manual /
	// manual_force / specific_bead_closed evaluators and as the baseline
	// reference for wall_clock.
	BeadID string
	// Project scopes bead_query evaluation.
	Project string
	// RunBudget is the soft cap on runs (max_runs equivalent for triggers).
	// 0 means unlimited.
	RunBudget int
	// RunsSpawned is the count of runs spawned by this gardener so far.
	// Used for the max_runs kind on gardener-shaped triggers, which gate on
	// "have we already spawned the budgeted number of runs" rather than
	// "have we completed them" (the scheduler distinction matters for
	// long-running gardeners but for triggers a spawn is the decision point).
	RunsSpawned int
	// ConsecutiveFailures is the gardener's consecutive-failure streak.
	ConsecutiveFailures int
	// CreatedAt is the trigger bead's creation time, used as the wall_clock
	// baseline (floored at gardenerWallClockGrandfatherEpoch — see
	// gardenerWallClockBaseline). Zero means wall_clock is not enforceable.
	CreatedAt time.Time
	// WallClockSince is a per-bead, engine-owned wall_clock baseline OVERRIDE
	// stamped by the revival paths (gardenerRearmFromOperatorReopen,
	// gardenerGrantSecondLife) to time.Now() on each revive. It is the fix for
	// bug ka-0000006465: before it, the wall_clock baseline was max(CreatedAt,
	// epoch) with no resettable term, so a gardener that hard-stopped on
	// wall_clock could NEVER be revived in place — created_at is immutable and
	// the grandfather epoch is a global constant, so a reopen re-tripped the
	// same expired window on the next reconcile (a reopen→re-close loop). Now
	// the baseline is max(WallClockSince, CreatedAt, epoch): stamping
	// WallClockSince on revive grants a genuinely fresh window measured from the
	// revive instant while keeping the whole run/comment ledger on one bead.
	// Zero (the common case — never revived) leaves the baseline unchanged, so
	// behavior for gardeners that were never revived is identical to before.
	WallClockSince time.Time
	// ProgressState is the gardener's serialized progress.State (run records +
	// credited events). Used by the no_progress_for stop kind to count
	// consecutive zero-score runs. Empty means no progress state has been
	// recorded yet (no runs finalized), so no_progress_for cannot fire.
	ProgressState string
	// IsStanding marks a PERPETUAL (standing) gardener — one with no terminal
	// epic, whose terminal_goal never self-declares done. For such a gardener the
	// max_runs kind is a NO-OP (bug ka-0000004754): a run-count ceiling can never
	// legitimately terminate a perpetual gardener (the bridge re-arms it in place
	// forever, ka-0000006241), so tripping it only churns an identical re-arm
	// comment on every lap. The terminal hard_stop kinds (consecutive_failures,
	// wall_clock) still fire for a standing gardener — only the run-count loop_end
	// is suppressed. Mirrors the direct run_budget gate exemption in
	// gardener_evaluator.gardenerPreFireCheck (gate 1).
	IsStanding bool
}

// gardenerWallClockGrandfatherEpoch is a migration floor on the wall_clock
// baseline (ka-0000006967). Populating GardenerStopContext.CreatedAt turned the
// gardener wall_clock stop kind from a silent no-op into a real enforcer for the
// first time — and a hard_stop wall_clock match closes the gardener TERMINALLY
// (gardener_evaluator.gardenerTerminate — no resume-or-escalate; a hard_stop
// match has Premature=false so the resume branch is skipped). Many live gardener
// beads predate this wiring by weeks; measuring their wall_clock from the true
// bead creation time would fire on the very first post-deploy reconcile and
// permanently close healthy, actively-progressing gardeners — exactly the "do
// not disable live gardeners mid-flight" hazard the bounds guard's
// warn-with-grace was written to avoid (gardener_bounds_guard.go).
//
// A GLOBAL floor (rather than a per-bead baseline stamped only on the gardeners
// a backfill rewrites) is deliberate: the wiring endangers TWO cohorts, and
// only a global floor covers both.
//  1. Unbounded gardeners that a data backfill gives a fresh wall_clock — a
//     per-bead field could protect these.
//  2. Gardeners that ALREADY carried a correct, bounded hard_stop wall_clock
//     (e.g. the default `gb gardener start` 168h) and are simply older than it.
//     Their wall_clock was inert only because CreatedAt was never plumbed; the
//     instant it is, they retro-close. A data backfill does NOT rewrite those
//     beads, so a per-bead baseline never reaches them — the global floor
//     grandfathers them for free.
//
// The floor gives every pre-epoch gardener a full fresh wall_clock window
// measured from the rollout instant instead of from its (possibly ancient)
// creation time. Gardeners created after the epoch are measured from their real
// CreatedAt, so the floor is invisible to them. It only ever EXTENDS a window,
// never shortens one, so it cannot cause a premature close.
//
// THE CLIFF (ka-0000000134): flooring the WHOLE pre-epoch cohort to one shared
// instant is per-gardener safe (each window only extends) but has a cohort-level
// failure the original comment did not anticipate — every pre-epoch gardener now
// measures the SAME baseline, so their windows expire on the SAME DAY rather
// than staggered by creation date. With five live pre-epoch gardeners carrying a
// 720h window, all five closed simultaneously on epoch+720h (2026-08-02) —
// including the meta-gardener that scores gardener effectiveness and the weekly
// gardener-audit roll-up, i.e. the two lanes that would notice gardeners going
// dark were themselves in the cohort. gardenerWallClockBaseline defends against
// this by adding a deterministic per-gardener stagger offset to the FLOORED
// baseline only (post-epoch gardeners are untouched); see its doc comment. The
// offset is non-negative, so the "only ever EXTENDS, never shortens" invariant
// holds for the cohort too.
//
// Set to the ka-0000006967 rollout date (UTC midnight). Once every live gardener
// predating the wiring has aged out or been re-created, this constant is inert
// and may be removed.
var gardenerWallClockGrandfatherEpoch = time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)

// GardenerWallClockGrandfatherEpoch is the exported read of the migration floor,
// for out-of-package callers that must decide whether a gardener is
// "grandfathered" (its wall_clock baseline was floored to the epoch) — notably
// `gb gardener status`, which flags the floor in its readout (ka-0000001868). It
// reads the same var the engine floors against and the test seam overrides
// (SetWallClockGrandfatherEpochForTest), so callers stay in lockstep with the
// engine's floor decision under test as in production.
func GardenerWallClockGrandfatherEpoch() time.Time { return gardenerWallClockGrandfatherEpoch }

// gardenerWallClockBaseline returns the instant a gardener's wall_clock duration
// is measured from: the LATEST of the per-bead revival override (wallClockSince,
// stamped to now by the reopen / second-life paths — bug ka-0000006465) and the
// bead's CreatedAt, floored at the grandfather epoch (see
// gardenerWallClockGrandfatherEpoch) and — when that floor is applied — offset by
// a deterministic per-gardener stagger (ka-0000000134).
//
// REVIVAL OVERRIDE (ka-0000006465). The wallClockSince term is what makes
// in-place gardener revival possible: a gardener that hard-stopped on wall_clock
// is revived by stamping wallClockSince to the revive instant, which advances
// the baseline past the expired window without touching the immutable
// created_at. Because it only ever EXTENDS the baseline forward (it is max'd
// with CreatedAt), a gardener that was never revived (zero wallClockSince)
// measures its window exactly as before. A zero CreatedAt with a zero
// wallClockSince yields a zero baseline (wall_clock stays not-enforceable — the
// caller treats that as not-met), preserving the pre-wiring behavior for
// triggers whose creation time the daemon did not surface. A non-zero
// wallClockSince makes wall_clock enforceable (a revived gardener has a concrete
// window start) even when CreatedAt is zero. In production wallClockSince is
// always time.Now() at revive — well past the epoch — so a revived gardener
// measures from its revive instant, unfloored and unstaggered (revive instants
// are already naturally staggered across operators/reconciles).
//
// PER-GARDENER STAGGER (ka-0000000134). When (and ONLY when) the epoch floor is
// applied — i.e. the latest per-bead term is before the epoch, so the gardener
// would otherwise inherit the single shared baseline — a deterministic,
// non-negative offset in [0, window) is added so the grandfathered cohort
// de-synchronizes. Without it, every pre-epoch gardener sharing a window length
// closes on the same day (the 2026-08-02 five-gardener cliff); with it, close
// dates spread across a full window. The offset is:
//   - deterministic: seeded from an FNV-1a hash of the bead id, so the same
//     gardener always gets the same baseline (no churn across reconciles) and
//     the value is independently reproducible for tests/audits. Mirrors the
//     gardenermode.StaggeredCron spawn-herd stagger.
//   - non-negative: it only ever pushes the baseline LATER, so the window still
//     only EXTENDS, never shortens — the load-bearing "cannot cause a premature
//     close" guarantee is preserved for the cohort exactly as for the floor.
//   - floor-scoped: applied only when the latest per-bead term is before the
//     epoch. Post-epoch gardeners (and revived gardeners, whose wallClockSince is
//     post-epoch) measure from their real baseline untouched — their instants
//     already differ, so there is no herd to break — keeping the floor and the
//     stagger both invisible to them.
//
// The stagger range is the wall_clock window itself, so a cohort spreads over a
// full window's worth of close dates — the maximum spread available without a
// second tunable. A sub-hour or non-positive window yields a zero offset (the
// floor alone), since there is no meaningful herd to break for such a gardener.
//
// SetWallClockGrandfatherEpochForTest overrides the grandfather epoch for the
// duration of a test and restores it via t.Cleanup. It exists so callers in
// other packages (e.g. the bridge gardener fire-path tests) can pin the epoch to
// a fixed instant and assert the wall_clock "elapsed" / "grandfathered"
// directions deterministically, independent of where the real clock falls
// relative to the production rollout date. Test-only: production never mutates
// the epoch.
func SetWallClockGrandfatherEpochForTest(t testingTB, epoch time.Time) {
	t.Helper()
	prev := gardenerWallClockGrandfatherEpoch
	gardenerWallClockGrandfatherEpoch = epoch
	t.Cleanup(func() { gardenerWallClockGrandfatherEpoch = prev })
}

// testingTB is the minimal subset of *testing.T that
// SetWallClockGrandfatherEpochForTest needs. Declaring it locally avoids
// importing "testing" into non-test code.
type testingTB interface {
	Helper()
	Cleanup(func())
}

func gardenerWallClockBaseline(beadID string, createdAt, wallClockSince time.Time, window time.Duration) time.Time {
	// The pre-wiring no-op: no creation time surfaced AND never revived ⇒
	// wall_clock is not enforceable (caller treats a zero baseline as not-met).
	if createdAt.IsZero() && wallClockSince.IsZero() {
		return time.Time{}
	}
	// The latest per-bead term (ka-0000006465): a revived gardener measures from
	// its wallClockSince revive instant; every other gardener from its true
	// created_at. Only ever extends forward, so a never-revived gardener (zero
	// wallClockSince) is unchanged.
	latest := createdAt
	if wallClockSince.After(latest) {
		latest = wallClockSince
	}
	if latest.Before(gardenerWallClockGrandfatherEpoch) {
		// Floored cohort: add the deterministic per-gardener stagger so the
		// grandfathered gardeners do not all close on the same day (the
		// ka-0000000134 cliff). The offset is non-negative, so the floored
		// window still only extends. In production a revived gardener's
		// wallClockSince is post-epoch, so revivals are never floored/staggered.
		return gardenerWallClockGrandfatherEpoch.Add(gardenerWallClockStaggerOffset(beadID, window))
	}
	return latest
}

// gardenerWallClockStaggerOffset returns a deterministic, non-negative offset in
// [0, window) seeded from an FNV-1a hash of beadID, at whole-hour resolution.
// It de-synchronizes the grandfathered (floored) gardener cohort so a shared
// window length no longer collapses their close dates onto one day
// (ka-0000000134). Same beadID → same offset forever (no reconcile churn);
// mirrors gardenermode.StaggeredCron's FNV-1a spawn-herd stagger. A non-positive
// or sub-hour window yields zero (nothing to stagger).
func gardenerWallClockStaggerOffset(beadID string, window time.Duration) time.Duration {
	windowHours := int64(window / time.Hour)
	if windowHours <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(beadID))
	return time.Duration(int64(h.Sum32())%windowHours) * time.Hour
}

// GardenerStopDaemon is the daemon-side surface the free-function stop
// evaluator needs. Subset of beadsapi.Client that *beadsapi.Client satisfies
// directly so callers don't need to wrap it.
type GardenerStopDaemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	conditions.BeadClient
}

// mrURLResolver is the optional capability the specific_mr_merged stop kind
// needs: resolve a canonical MR URL to its type=merge-request mirror bead.
// *beadsapi.Client implements it (MRBeadByURL); the gardener fire path's
// gardenerDaemonStopAdapter forwards it. A GardenerStopDaemon that does NOT
// implement it (some minimal test mocks) makes specific_mr_merged evaluate to
// not-met — fail-open, so the gardener keeps running rather than mis-closing.
type mrURLResolver interface {
	MRBeadByURL(ctx context.Context, mrURL string) (*beadsapi.BeadDetail, error)
}

// EvaluateGardenerStopSpec evaluates hard_stop then loop_end against a gardener
// trigger's current state. Returns (match, true) on the first firing entry.
// run_end is not evaluated here — it gates the running run, not the loop.
//
// Errors per entry are downgraded to not-met (fail-open) so a flaky daemon
// cannot wedge gardener forward progress.
func EvaluateGardenerStopSpec(ctx context.Context, daemon GardenerStopDaemon, logger *slog.Logger, sctx GardenerStopContext, spec StopSpec) (StopMatch, bool) {
	if spec.IsZero() {
		return StopMatch{}, false
	}
	if logger == nil {
		logger = slog.Default()
	}
	for _, e := range spec.HardStop.Any {
		if EvaluateGardenerStopEntry(ctx, daemon, logger, sctx, e) {
			return StopMatch{Slot: StopSlotHardStop, Kind: e.Kind}, true
		}
	}
	for _, e := range spec.LoopEnd.Any {
		if EvaluateGardenerStopEntry(ctx, daemon, logger, sctx, e) {
			// Never-started guard (ka-0000005388): a bead_query
			// goal-completion check that matches at RunsSpawned==0 is
			// indistinguishable from "the work graph was never linked"
			// (e.g. children added with bad CLI syntax that silently
			// no-op'd) vs "instantly done". Closing here would let a
			// gardener declare its goal complete before firing once. The
			// cron preflight already requires children>0 to arm; apply the
			// same evidence-of-work bar to the goal-completion evaluation.
			// Treat as misconfiguration: warn and do NOT close.
			//
			// EXCEPTION (Matt's ask, thread ka-0000001290): a CLOSED parent
			// epic resolves the ambiguity — an operator (or the work itself)
			// closed the plan, so "done" is unambiguous even at 0 runs. Without
			// this, a plan epic closed before its gardener ever fired strands the
			// gardener open+enabled forever (it keeps waking on cron, reading
			// "running" on /plans, and re-firing) because the guard suppresses
			// the very close that would retire it. A closed root is exactly the
			// terminal signal gardenerStopParentClosed detects.
			if e.Kind == StopKindBeadQuery && sctx.RunsSpawned == 0 &&
				!gardenerStopParentClosed(ctx, daemon, e.Parent) {
				logger.Warn("gardener loop_end bead_query matched at 0 runs — suppressing close (never-started guard); check that the work graph is actually linked to the parent",
					"trigger", sctx.BeadID, "parent", e.Parent, "op", e.Op, "value", e.Value)
				continue
			}
			return StopMatch{
				Slot:      StopSlotLoopEnd,
				Kind:      e.Kind,
				Premature: !stopKindIsTerminal(e.Kind),
			}, true
		}
	}
	return StopMatch{}, false
}

// gardenerStopParentClosed reports whether the loop_end bead_query's parent bead
// is itself closed — the unambiguous "this plan is done" signal that overrides
// the never-started guard (see EvaluateGardenerStopSpec). It mirrors the
// closed-root-epic short-circuit conditions.EvaluateCondition already applies
// when residual open work lingers under a closed root (ka-0000008051), but is
// evaluated HERE at the guard site because the guard fires precisely when the
// subtree count is already zero (so that short-circuit never runs). Fail-safe:
// an empty parent or any read error returns false so the guard's default
// (suppress the close) still applies — we only override on a POSITIVE "closed".
func gardenerStopParentClosed(ctx context.Context, daemon GardenerStopDaemon, parent string) bool {
	if strings.TrimSpace(parent) == "" || daemon == nil {
		return false
	}
	detail, err := daemon.GetBead(ctx, parent)
	if err != nil || detail == nil {
		return false
	}
	return detail.Status == "closed"
}

// EvaluateGardenerStopEntry evaluates a single StopEntry against a
// GardenerStopContext. Returns false on any error (fail-open) and on kinds
// that depend on scheduler-only state (no_progress_for, no_actionable_state,
// yielded_then_woken_then_idle).
func EvaluateGardenerStopEntry(ctx context.Context, daemon GardenerStopDaemon, logger *slog.Logger, sctx GardenerStopContext, e StopEntry) bool {
	if logger == nil {
		logger = slog.Default()
	}
	switch e.Kind {
	case StopKindMaxRuns:
		// Gardener-shape: gate on RunsSpawned vs RunBudget. For
		// type=trigger gardeners a spawn is the load-bearing event;
		// completion tracking lives on the spawned bead, not the
		// gardener. Mirror the per-run debit by gating on spawns.
		//
		// STANDING gardeners are EXEMPT (bug ka-0000004754): a run-count ceiling
		// can never legitimately terminate a perpetual gardener — the bridge
		// re-arms it in place forever (ka-0000006241), so every trip is pure
		// churn (an identical re-arm comment per lap, 33+ observed on a prod
		// lane). Return not-met so the loop_end never fires on run count; the
		// hard_stop brakes (consecutive_failures, wall_clock) remain the bounds.
		if sctx.IsStanding {
			return false
		}
		return sctx.RunBudget > 0 && sctx.RunsSpawned >= sctx.RunBudget

	case StopKindWallClock:
		d, err := time.ParseDuration(e.Duration)
		if err != nil || d <= 0 {
			return false
		}
		baseline := gardenerWallClockBaseline(sctx.BeadID, sctx.CreatedAt, sctx.WallClockSince, d)
		if baseline.IsZero() {
			return false
		}
		return time.Since(baseline) >= d

	case StopKindBeadQuery:
		met, err := conditions.EvaluateCondition(ctx, daemon, conditions.Condition{
			Type:                      "bead_query",
			Op:                        e.Op,
			Value:                     e.Value,
			Project:                   e.Project,
			Types:                     e.Types,
			Statuses:                  e.Statuses,
			Labels:                    e.Labels,
			Parent:                    e.Parent,
			ExcludeBlockedByOpenTypes: e.ExcludeBlockedByOpenTypes,
		}, sctx.Project)
		if err != nil {
			logger.Warn("gardener stop bead_query evaluator error",
				"trigger", sctx.BeadID, "error", err)
			return false
		}
		return met

	case StopKindSpecificBeadClosed:
		if e.BeadID == "" {
			return false
		}
		detail, err := daemon.GetBead(ctx, e.BeadID)
		if err != nil || detail == nil {
			return false
		}
		return detail.Status == "closed"

	case StopKindGatePresent:
		return gardenerGateExists(ctx, daemon, e.Value)
	case StopKindGateAbsent:
		return !gardenerGateExists(ctx, daemon, e.Value)

	case StopKindConsecutiveFailures:
		threshold, err := strconv.Atoi(strings.TrimSpace(e.Value))
		if err != nil || threshold <= 0 {
			return false
		}
		return sctx.ConsecutiveFailures >= threshold

	case StopKindManual:
		field := e.ReasonField
		if field == "" {
			field = "operator_close_reason"
		}
		detail, err := daemon.GetBead(ctx, sctx.BeadID)
		if err != nil || detail == nil {
			return false
		}
		return strings.TrimSpace(detail.Fields[field]) != ""

	case StopKindManualForce:
		label := e.Label
		if label == "" {
			label = "force-close"
		}
		detail, err := daemon.GetBead(ctx, sctx.BeadID)
		if err != nil || detail == nil {
			return false
		}
		return slices.Contains(detail.Labels, label)

	case StopKindNoProgressFor:
		// Progress-aware: fire when the last N runs all scored zero. The
		// bridge fire path now finalizes each run into ProgressState
		// (gardenerAgentTracker → progress.FinalizeRun), so the RunRecords
		// this needs are populated on type=trigger gardeners. Without enough
		// runs to judge (observed < window) we stay not-met — a fresh
		// gardener is not "wedged".
		state, err := progress.ParseState(sctx.ProgressState)
		if err != nil {
			logger.Warn("gardener stop no_progress_for: parse state failed",
				"trigger", sctx.BeadID, "error", err)
			return false
		}
		if window := e.Runs; window > 0 {
			// Count-based window: the last N runs must ALL be barren.
			zero, observed := state.ZeroScoreRunsInLastN(window)
			return observed >= window && zero >= window
		}
		// Duration-based window (`no_progress_for: <minutes>` — ka-0000005141):
		// fire when EVERY run that closed within the last <duration> scored zero.
		// The score read is post-FinalizeRun, so a run rescued by an in-window
		// credited event (mr_merged / child_closed / decision_resolved) already
		// carries a non-zero score and defuses the wedge — a gardener that keeps
		// dispatching crew but merges nothing over the whole window trips, while
		// one that merges even once inside it does not. Requires at least one
		// closed run in the window: an idle stretch with no finalized runs is not
		// evidence of a wedge (a fresh gardener is not "stuck").
		d, err := time.ParseDuration(strings.TrimSpace(e.Duration))
		if err != nil || d <= 0 {
			return false
		}
		zero, observed := state.ZeroScoreRunsSince(time.Now().Add(-d))
		return observed >= 1 && zero == observed

	case StopKindGoalSatisfied:
		// Master gate (bead ka-0000005353): the gardener has declared its
		// terminal_goal met. Fires when the freshest finalized run recorded
		// goal_satisfied=="yes" in its wrap-up. This is the canonical
		// "gardener declares itself done" — the ONLY loop_end signal that
		// should ever close a gardener whose goal might otherwise be unmet.
		// No runs yet ⇒ not done (a fresh gardener has not declared anything).
		state, err := progress.ParseState(sctx.ProgressState)
		if err != nil {
			logger.Warn("gardener stop goal_satisfied: parse state failed",
				"trigger", sctx.BeadID, "error", err)
			return false
		}
		if len(state.Runs) == 0 {
			return false
		}
		last := state.Runs[len(state.Runs)-1]
		return strings.EqualFold(strings.TrimSpace(last.GoalSatisfied), "yes")

	case StopKindNoActionableState, StopKindYieldedThenWokenThenIdle:
		// Scheduler-only kinds — depend on run-state machinery (live crew
		// scan / yield tracking) not present on type=trigger gardeners.
		// Fail-open until the bridge grows the same primitives.
		return false

	case StopKindSpecificMRMerged:
		// First-class "stop when THIS MR is merged" (task ka-0000005598, the
		// `--stop-on-mr-merged <mr_url>` flag). Resolve the target URL to its
		// canonical type=merge-request mirror bead and fire when it reports
		// merged. "Merged" mirrors gardenerHasUnmergedTrackedMR / the progress
		// evaluator exactly (mr_outcome == "merged" OR mr_state == "merged").
		//
		// Fail-open toward NOT-met: an empty URL, a daemon requiring an MR
		// resolver it does not implement, a lookup error, or a not-yet-mirrored
		// MR all return false so the gardener keeps running rather than closing on
		// a transient read failure. This is a TERMINAL loop_end kind (a match
		// closes the gardener as success), so biasing toward not-met means the
		// worst case is one extra cycle, never a premature close.
		url := strings.TrimSpace(e.MRURL)
		if url == "" {
			return false
		}
		resolver, ok := daemon.(mrURLResolver)
		if !ok {
			logger.Warn("gardener stop specific_mr_merged: daemon does not resolve MR URLs — treating as not-met",
				"trigger", sctx.BeadID, "mr_url", url)
			return false
		}
		mr, err := resolver.MRBeadByURL(ctx, url)
		if err != nil {
			logger.Warn("gardener stop specific_mr_merged: MRBeadByURL failed — treating as not-met (fail-open)",
				"trigger", sctx.BeadID, "mr_url", url, "error", err)
			return false
		}
		if mr == nil {
			// No mirror bead yet (MR not synced): not merged as far as we know.
			return false
		}
		return mr.Fields["mr_outcome"] == "merged" || mr.Fields["mr_state"] == "merged"

	case StopKindMRMerged:
		// Park-not-close marker (ka-0000007387). NEVER handled here: the
		// bridge gardener fire path intercepts mr_merged in a dedicated
		// pre-fire gate (gardenerBlockOnMR) that PARKS the gardener dormant
		// while tracked MRs are unmerged, rather than lowering it into a
		// terminate/resume decision. Returning not-met keeps it out of the
		// generic loop_end close path entirely. The dedicated gate — not this
		// evaluator — owns the MR-merge decision, because parking (dormant,
		// stays armed) is categorically different from stopping (close/resume).
		return false

	default:
		logger.Warn("gardener stop: unknown kind, treating as not-met",
			"trigger", sctx.BeadID, "kind", e.Kind)
		return false
	}
}

// GardenerWallClockBaseline is the exported view of the instant a gardener's
// hard_stop wall_clock is measured from — IDENTICAL to the ENGINE's baseline
// (gardenerWallClockBaseline), which it delegates to directly so the two can
// never drift. It exists so out-of-package callers — notably `gb gardener
// status` (ka-0000007024) — reproduce the engine's baseline EXACTLY rather than
// re-deriving the floor and diverging from it.
//
// It takes beadID and window because the exported view must include the
// per-gardener STAGGER offset the engine adds to the grandfathered (epoch-floored)
// cohort (ka-0000000134). That offset is seeded from an FNV-1a hash of beadID
// and scaled to window (see gardenerWallClockStaggerOffset), so both are required
// to reconstruct it.
//
// WHY THE STAGGER MUST BE INCLUDED (ka-0000001868). An earlier version dropped
// the stagger, on the theory that the unstaggered floor is the EARLIEST possible
// expiry and therefore a "conservative" lower bound that could only under-warn.
// That reasoning is inverted. Because the unstaggered baseline is the earliest,
// its expiry is the FIRST to cross "already past" — so it claims
// expired-when-live for EVERY grandfathered gardener whose real (staggered)
// expiry is still in the future. A lower bound sitting in the past implies
// nothing about whether the real expiry is past; it is exactly the wrong bound
// for an is-this-expired check. The concrete failure: eight live gardeners all
// reported EXPIRED at 2026-08-02T00:00:00Z (the shared unstaggered floor) while
// their real staggered expiries ran Aug 4–28 and every one was still firing.
// Delegating to the engine function makes the CLI verdict provably match the
// engine's fire decision.
//
// A zero createdAt yields the zero time (wall_clock not enforceable). It does
// NOT account for the per-bead wall_clock_since revival override — use
// GardenerWallClockBaselineSince when the caller has it (e.g. the enable guard,
// which must not re-flag a just-rebaselined gardener as expired). Kept as a thin
// wrapper so callers that never had a wall_clock_since to pass are unchanged.
func GardenerWallClockBaseline(beadID string, createdAt time.Time, window time.Duration) time.Time {
	return GardenerWallClockBaselineSince(beadID, createdAt, time.Time{}, window)
}

// GardenerWallClockBaselineSince is GardenerWallClockBaseline plus the per-bead
// wall_clock_since revival override (bug ka-0000006465): the instant a gardener's
// hard_stop wall_clock is measured from is max(wallClockSince, createdAt),
// floored at the grandfather epoch and — when that floor applies — offset by the
// per-gardener stagger (ka-0000000134). It delegates to the ENGINE's
// gardenerWallClockBaseline so the exported view is IDENTICAL to what the engine
// enforces at fire time, including the stagger.
//
// It exists so a caller reviving a gardener — notably `gb trigger enable`
// (ka-0000005774) — reproduces the SAME "is this window already expired?"
// verdict the engine will reach, INCLUDING a fresh window a --rebaseline just
// stamped. Without the wallClockSince term, a gardener that was correctly
// rebaselined to now would still read EXPIRED off its ancient created_at, so
// the guard could never confirm the revive it just performed is safe.
func GardenerWallClockBaselineSince(beadID string, createdAt, wallClockSince time.Time, window time.Duration) time.Time {
	return gardenerWallClockBaseline(beadID, createdAt, wallClockSince, window)
}

// GardenerWallClockExpirySince returns the instant a gardener's hard_stop
// wall_clock window elapses, honouring the per-bead wall_clock_since revival
// override — baseline = max(wallClockSince, createdAt, epoch) [+stagger when
// floored], expiry = baseline + window. ok=false when the baseline is not
// computable (zero createdAt AND zero wallClockSince) or the window is
// non-positive.
//
// It is the shared primitive the CLI uses to tell an operator whether
// reviving/authoring a gardener with this duration would land the ceiling in
// the past (an instant self-close) rather than the future. Because the baseline
// INCLUDES the engine's per-gardener stagger (ka-0000001868), this expiry equals
// the instant the engine's StopKindWallClock actually fires — so an already-past
// expiry is a sound "this will re-close on the next reconcile" signal, and a
// future expiry never false-alarms a live gardener. An already rebaselined
// gardener (wall_clock_since in the future) reports its real, still-open window
// rather than a stale expiry off created_at.
func GardenerWallClockExpirySince(beadID string, createdAt, wallClockSince time.Time, window time.Duration) (expiry time.Time, ok bool) {
	baseline := GardenerWallClockBaselineSince(beadID, createdAt, wallClockSince, window)
	if baseline.IsZero() || window <= 0 {
		return time.Time{}, false
	}
	return baseline.Add(window), true
}

// gardenerGateExists reports whether an open gate bead with the given title
// exists. Mirrors (*Scheduler).gateExists for the free-function path.
func gardenerGateExists(ctx context.Context, daemon GardenerStopDaemon, title string) bool {
	if title == "" {
		return false
	}
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"gate"},
		Statuses: []string{"open", "in_progress"},
		Search:   title,
		Limit:    16,
	})
	if err != nil || result == nil {
		return false
	}
	for _, b := range result.Beads {
		if b.Title == title {
			return true
		}
	}
	return false
}
