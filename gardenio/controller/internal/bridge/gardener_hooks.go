// Gardener-loop reconcile hooks for the bridge TriggerEngine (Phase 3b/B7-c2,
// ka-0000003115). These three free functions absorb the scheduler.Reconcile
// behaviors that gardener-shaped triggers must mirror to achieve identical
// observable behavior with type=schedule gardeners:
//
//  1. gardenerClosePerRunTaskBead — closes the per-run task bead left open by
//     a terminated agent (scheduler analogue: closePerRunTaskBead via
//     ClosePerRunTaskBead).
//  2. gardenerDiscoverProgress — runs the progress.Evaluator against the
//     gardener's Progress spec and persists newly-observed events into
//     Gardener.ProgressState (scheduler analogue: discoverProgress).
//  3. gardenerEvaluateExitCondition — parses Gardener.ExitCondition, evaluates
//     against the bead store, and terminates the gardener on match
//     (scheduler analogue: evaluateExitCondition).
//
// Each hook is fail-open: parse / daemon errors log a warning and return
// without aborting evalOne. The wiring in evalOne is documented inline above
// the call sites.

package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/progress"
)

// gardenerIterationFinalizeLimit bounds how many of a gardener's iteration agent
// beads gardenerFinalizeRunsFromIterations pulls per owner-key query: the newest
// by created_at, so a gardener past this many runs still reconciles its latest
// ones (gd-Ahi1H2jR9a). Matches the beads-viewer detail page's
// gardenerIterationPageLimit so the engine and the UI reconcile from the same
// horizon. The daemon-side field filter is backed by the
// partial B-tree index from migration 031, so this is a single indexed scan.
const gardenerIterationFinalizeLimit = 500

// gardenerClosePerRunTaskBead closes the per-run task bead created for the
// gardener's most recently terminated agent. Mirrors
// scheduler.(*Scheduler).closePerRunTaskBead — the same free helper
// (scheduler.ClosePerRunTaskBead) is shared so policy stays in one place.
//
// No-op when:
//   - the trigger has no Gardener block,
//   - Gardener.LastAgentID is unset (no agent to inspect),
//   - the gardener has an AssignBead (subtree mode — per-run beads not used),
//   - the daemon GetBead fails or the agent is not yet closed (the next
//     reconcile cycle will retry once the agent terminates).
func gardenerClosePerRunTaskBead(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) {
	if trig.Gardener == nil || trig.Gardener.LastAgentID == "" {
		return
	}
	if trig.Gardener.AssignBead != "" {
		// Subtree-mode gardeners share AssignBead across runs; there is no
		// per-run task bead to close. Mirrors the AssignBead == "" guard
		// inside scheduler.ClosePerRunTaskBead.
		return
	}
	agentDetail, err := daemon.GetBead(ctx, trig.Gardener.LastAgentID)
	if err != nil || agentDetail == nil {
		return
	}
	if agentDetail.Status != "closed" {
		return
	}
	scheduler.ClosePerRunTaskBead(ctx, daemon, logger, "trigger", trig.ID, trig.Gardener.AssignBead, agentDetail)
}

// gardenerDiscoverProgress drives the progress.Evaluator against the gardener's
// Progress spec and persists newly-credited events back to the trigger bead's
// gardener JSON. Mirrors scheduler.(*Scheduler).discoverProgress.
//
// No-op when Gardener or Gardener.Progress is empty — the common case for
// source-only gardeners. Parse / evaluator failures log a warning and return
// without persisting; the next cycle will re-attempt.
func gardenerDiscoverProgress(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, now time.Time) {
	if trig.Gardener == nil || len(trig.Gardener.Progress) == 0 {
		return
	}
	spec, err := progress.ParseSpec(string(trig.Gardener.Progress))
	if err != nil {
		logger.Warn("gardener progress: parse spec failed",
			"trigger", trig.ID, "error", err)
		return
	}
	if spec.IsZero() {
		return
	}
	state, _ := progress.ParseState(string(trig.Gardener.ProgressState))
	priorEvents := len(state.Events)

	ev := &progress.Evaluator{Client: daemon}
	state, err = ev.Discover(ctx, spec, progress.EvalContext{
		AssignBead: trig.Gardener.AssignBead,
		ScheduleID: trig.ID,
		State:      state,
		Now:        now,
	}, func(format string, args ...any) {
		logger.Warn("gardener progress evaluator: "+format, args...)
	})
	if err != nil {
		logger.Warn("gardener progress evaluator failed",
			"trigger", trig.ID, "error", err)
		return
	}
	if len(state.Events) == priorEvents {
		// Nothing new; persist nothing. Mirrors scheduler.discoverProgress.
		return
	}
	marshalled, mErr := state.Marshal()
	if mErr != nil {
		logger.Warn("gardener progress: state marshal failed",
			"trigger", trig.ID, "error", mErr)
		return
	}

	// Persist into Gardener.ProgressState. We round-trip the full Gardener JSON
	// so we don't clobber sibling counters (RunsSpawned, ConsecutiveFailures)
	// that other hooks update in the same cycle.
	updated := *trig.Gardener
	updated.ProgressState = json.RawMessage(marshalled)
	raw, jErr := json.Marshal(updated)
	if jErr != nil {
		logger.Warn("gardener progress: marshal gardener failed",
			"trigger", trig.ID, "error", jErr)
		return
	}
	if err := daemon.UpdateBeadFields(ctx, trig.ID, map[string]string{
		"gardener": string(raw),
	}); err != nil {
		logger.Warn("gardener progress: UpdateBeadFields failed",
			"trigger", trig.ID, "error", err)
		return
	}
	// Mutate in place so later hooks in this cycle see the bumped state.
	trig.Gardener.ProgressState = updated.ProgressState
}

// gardenerFinalizeRunsFromIterations reconciles ProgressState.Runs against the
// gardener's real closed iteration agent beads (bug ka-0000006915).
//
// Why this exists: gardenerAgentTracker only finalizes the SINGLE agent named in
// Gardener.LastAgentID, and the whole tracker is gated on LastAgentID != ""
// (triggers.go). Subtree-mode (assign_bead) gardeners overwhelmingly run
// concurrently — max_concurrent>1, no one_spawn_at_a_time — so each fire
// overwrites the single LastAgentID pointer before its predecessors are seen
// closed, and most iterations never reach FinalizeRun. The only gardeners whose
// ProgressState.Runs was ever populated were the two serialized ones whose lone
// pointer round-tripped cleanly. /maps run glyphs (and the no_progress_for stop
// kind) ride on ProgressState.Runs, so they were empty for ~every real gardener.
//
// This finalizer is iteration-bead-driven rather than pointer-driven: it queries
// the gardener's closed iterations the same way the detail page does (by
// trigger_id / schedule_id), then FinalizeRun's each one that is not already
// recorded in ProgressState.Runs, deduped by AgentID. It is additive and
// idempotent — re-running it credits no run twice — so it composes with
// gardenerAgentTracker (which still runs for the LastAgentID fast path) without
// double-counting.
//
// No-op when the gardener has no Progress spec: gardeners without one don't use
// progress-aware stops and shouldn't accrue RunRecords (mirrors the Progress
// gate in gardenerAgentTracker). Fail-open on every daemon/parse error.
//
// CALLER ORDERING CONTRACT: like gardenerAgentTracker, this must run AFTER
// gardenerDiscoverProgress so FinalizeRun's event-aware no-progress rescue
// (HasCreditedEventInWindow) sees this cycle's freshly discovered events.
func gardenerFinalizeRunsFromIterations(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) {
	if trig.Gardener == nil || len(trig.Gardener.Progress) == 0 {
		return
	}
	// Only finalize when a non-zero Progress spec is configured, matching the
	// gardenerAgentTracker gate — a gardener that opts out of progress tracking
	// should not accrue RunRecords.
	spec, err := progress.ParseSpec(string(trig.Gardener.Progress))
	if err != nil || spec.IsZero() {
		return
	}

	iters := gardenerClosedIterations(ctx, daemon, logger, trig.ID)
	if len(iters) == 0 {
		return
	}

	state, _ := progress.ParseState(string(trig.Gardener.ProgressState))

	// Index the runs already recorded so we credit each iteration at most once.
	// AgentID is the iteration agent bead id — unique per run and the same key
	// gardenerAgentTracker stamps via FinalizeRun, so the two writers dedup
	// against each other.
	recorded := make(map[string]bool, len(state.Runs))
	for _, r := range state.Runs {
		if r.AgentID != "" {
			recorded[r.AgentID] = true
		}
	}

	// Oldest-first so the appended RunRecords land in chronological order
	// (State.Runs is newest-last), matching gardenerAgentTracker's incremental
	// append and keeping ZeroScoreRunsInLastN's tail window meaningful.
	slices.SortFunc(iters, func(a, b *beadsapi.BeadDetail) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})

	// Durable finalization low-water mark (ka-0000003265). A no_progress_for
	// re-arm (or the stalled-trigger heal) resets ProgressState and stamps
	// RunsFinalizedAfter=now so the gardener gets a genuinely fresh no-progress
	// window. Without honoring it here, this iteration-driven rebuild would
	// re-credit the pre-reset zero-score tail from the immutable closed-iteration
	// history every cycle, instantly undoing the reset and re-wedging the gardener
	// in no_progress_for (its last_checked frozen because evalOne returns from the
	// pre-fire stop gate before stamping it). Skip iterations created before the
	// baseline so the reset sticks. Zero baseline ⇒ credit everything (legacy).
	baseline := trig.Gardener.RunsFinalizedAfter

	var fresh []*beadsapi.BeadDetail
	for _, it := range iters {
		if it.ID == "" || recorded[it.ID] {
			continue
		}
		if !baseline.IsZero() && it.CreatedAt.Before(baseline) {
			continue
		}
		// Skip never-ran phantoms (closed in a pre-pod state: queued/expired/…):
		// they never got a pod and never made progress, so FinalizeRun'ing a
		// barren score-0 run for them would falsely advance no_progress_for —
		// the same defect fixed in gardenerAgentTracker (bug ka-0000004058). Mark
		// recorded so a later cycle does not reconsider the same iteration.
		if gardenerRunNeverStarted(it.Fields["agent_state"]) {
			recorded[it.ID] = true
			continue
		}
		fresh = append(fresh, it)
	}

	// State.Runs keeps the newest progress.MaxRuns. An iteration older than
	// that window was evicted, not missed: crediting it again would push it in
	// at the newest end, evict a real recent run, and rewrite the gardener blob
	// every cycle (the run_log analogue is gd-YzUQBV7xa1). Credit only what the
	// capped Runs keeps, and sort first so AppendRun's cap drops the oldest.
	slices.SortStableFunc(state.Runs, func(a, b progress.RunRecord) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	if over := len(state.Runs) + len(fresh) - progress.MaxRuns; over > 0 {
		starts := make([]time.Time, 0, len(state.Runs)+len(fresh))
		for _, r := range state.Runs {
			starts = append(starts, r.StartedAt)
		}
		for _, it := range fresh {
			starts = append(starts, it.CreatedAt)
		}
		slices.SortFunc(starts, time.Time.Compare)
		cutoff := starts[over]
		fresh = slices.DeleteFunc(fresh, func(it *beadsapi.BeadDetail) bool {
			return it.CreatedAt.Before(cutoff)
		})
	}

	appended := 0
	for _, it := range fresh {
		endedAt := it.UpdatedAt
		if endedAt.IsZero() {
			endedAt = time.Now()
		}
		progress.FinalizeRun(&state, it.ID, it.Fields["wrapup"], it.Fields["agent_state"], it.CreatedAt, endedAt)
		appended++
	}
	if appended == 0 {
		return
	}
	// A concurrent gardener's fast path can record a newer run before an older
	// one closes; keep Runs newest-last for ZeroScoreRunsInLastN.
	slices.SortStableFunc(state.Runs, func(a, b progress.RunRecord) int {
		return a.StartedAt.Compare(b.StartedAt)
	})

	marshalled, mErr := state.Marshal()
	if mErr != nil {
		logger.Warn("gardener finalize-iterations: progress state marshal failed",
			"trigger", trig.ID, "error", mErr)
		return
	}

	// Round-trip the full Gardener JSON so we don't clobber sibling counters
	// other hooks updated this cycle (RunsCompleted, ConsecutiveFailures, ...).
	updated := *trig.Gardener
	updated.ProgressState = json.RawMessage(marshalled)
	raw, jErr := json.Marshal(updated)
	if jErr != nil {
		logger.Warn("gardener finalize-iterations: marshal gardener failed",
			"trigger", trig.ID, "error", jErr)
		return
	}
	if err := daemon.UpdateBeadFields(ctx, trig.ID, map[string]string{
		"gardener": string(raw),
	}); err != nil {
		logger.Warn("gardener finalize-iterations: UpdateBeadFields failed",
			"trigger", trig.ID, "error", err)
		return
	}
	logger.Info("gardener finalize-iterations: recorded runs from iteration beads",
		"trigger", trig.ID, "appended", appended, "total_runs", len(state.Runs))

	// Mutate in place so later gates in this evalOne cycle (no_progress_for /
	// goal_satisfied via GardenerStopContext.ProgressState) see the runs.
	trig.Gardener.ProgressState = updated.ProgressState
}

// gardenerReconcileRunLogFromIterations reconciles the gardener's rolling
// `run_log` against its real closed iteration agent beads (task ka-0000005584,
// audit finding on Phase 3b Convergence gardener ka-0000003669: 84 runs_completed
// but a 7-entry run_log).
//
// Why this exists: the ONLY writer of run_log is gardenerAgentTracker, and it
// appends exactly ONE entry per evalOne cycle — for the single agent named in
// Gardener.LastAgentID. A concurrent gardener (max_concurrent>1, or any gardener
// whose fires outpace its reconcile cadence) overwrites that lone pointer before
// its predecessors are observed closed, so every run except the last-stamped one
// silently never lands in run_log. runs_completed keeps climbing (that counter is
// bumped on the same single-pointer path but is monotonic and never regresses),
// which is exactly the "N completed but empty/sparse run_log" divergence the
// audit flagged: the counter and the log drift apart.
//
// This is the run_log analogue of gardenerFinalizeRunsFromIterations, which heals
// ProgressState.Runs the same iteration-bead-driven way. It deliberately differs
// in two ways:
//
//   - NOT gated on a Progress spec. run_log is a pure visibility artifact read by
//     `gb gardener status` (last_run, run trajectory) and the /health gardeners
//     tile — it must fill for EVERY gardener, including the many (like Phase 3b)
//     that carry no Progress spec and so are invisible to the ProgressState
//     finalizer.
//   - Records never-ran phantoms too. gardenerAgentTracker appends a run_log entry
//     even for a queued/expired agent (visibility) while skipping its counters;
//     run_log mirrors that — a queue death is still a fire worth showing. Only the
//     progress finalizer skips phantoms (scoring one 0 would falsely advance
//     no_progress_for, bug ka-0000004058); run_log has no such hazard.
//
// Additive, idempotent, and deduped by agent id against the existing run_log, so
// it composes with gardenerAgentTracker's fast path without double-recording. The
// rebuilt log is sorted chronologically (newest-last) and capped at
// scheduler.MaxRunLogEntries, matching RunLog.Append's rolling bound. Fail-open on
// every daemon / parse / marshal error.
func gardenerReconcileRunLogFromIterations(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) {
	if trig.Gardener == nil {
		return
	}
	capSpec := trig.Gardener

	iters := gardenerClosedIterations(ctx, daemon, logger, trig.ID)
	if len(iters) == 0 {
		return
	}

	runLog, err := scheduler.ParseRunLog(string(capSpec.RunLog))
	if err != nil {
		// A malformed run_log must not wedge the gardener; log and rebuild from
		// the iteration beads (the ground truth) rather than propagate.
		logger.Warn("gardener reconcile-runlog: parse run_log failed, rebuilding from iterations",
			"trigger", trig.ID, "error", err)
		runLog = nil
	}

	// Index the agents already present so each iteration is recorded at most
	// once — the same dedup key (agent bead id) gardenerAgentTracker stamps into
	// RunLogEntry.Agent, so the fast path and this reconciler dedup against each
	// other.
	recorded := make(map[string]bool, len(runLog))
	for _, e := range runLog {
		if e.Agent != "" {
			recorded[e.Agent] = true
		}
	}

	// Union the existing log with every iteration not yet in it, then sort by
	// StartedAt (stable: existing entries win ties) and keep the newest
	// MaxRunLogEntries. Appending one-by-one instead would re-add iterations that
	// already fell off the cap on every pass (they look unrecorded), evicting
	// other entries and rewriting the gardener blob each evaluation.
	merged := slices.Clone(runLog)
	for _, it := range iters {
		if it.ID == "" || recorded[it.ID] {
			continue
		}
		startedAt := it.CreatedAt
		endedAt := it.UpdatedAt
		if endedAt.IsZero() {
			endedAt = time.Now()
		}
		var durationS int
		if !startedAt.IsZero() && endedAt.After(startedAt) {
			durationS = int(endedAt.Sub(startedAt).Seconds())
		}
		merged = append(merged, scheduler.RunLogEntry{
			Agent:         it.ID,
			StartedAt:     startedAt,
			EndedAt:       endedAt,
			DurationS:     durationS,
			Outcome:       it.Fields["agent_state"],
			CloseReason:   it.Fields["close_reason"],
			WrapupSummary: gardenerWrapUpAccomplishments(it.Fields["wrapup"]),
		})
		recorded[it.ID] = true
	}
	slices.SortStableFunc(merged, func(a, b scheduler.RunLogEntry) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	if len(merged) > scheduler.MaxRunLogEntries {
		merged = merged[len(merged)-scheduler.MaxRunLogEntries:]
	}

	// Nothing to write when the capped result is the stored log: iterations older
	// than the retained window are not "unrecorded", just evicted.
	if slices.EqualFunc(merged, runLog, func(a, b scheduler.RunLogEntry) bool { return a == b }) {
		return
	}
	appended := 0
	existing := make(map[string]bool, len(runLog))
	for _, e := range runLog {
		existing[e.Agent] = true
	}
	for _, e := range merged {
		if !existing[e.Agent] {
			appended++
		}
	}
	runLog = scheduler.RunLog(merged)

	marshalled, mErr := runLog.Marshal()
	if mErr != nil {
		logger.Warn("gardener reconcile-runlog: marshal run_log failed",
			"trigger", trig.ID, "error", mErr)
		return
	}

	// Round-trip the full Gardener JSON so we don't clobber sibling counters
	// other hooks updated this cycle (RunsCompleted, ConsecutiveFailures, ...).
	updated := *capSpec
	updated.RunLog = json.RawMessage(marshalled)
	raw, jErr := json.Marshal(updated)
	if jErr != nil {
		logger.Warn("gardener reconcile-runlog: marshal gardener failed",
			"trigger", trig.ID, "error", jErr)
		return
	}
	if err := daemon.UpdateBeadFields(ctx, trig.ID, map[string]string{
		"gardener": string(raw),
	}); err != nil {
		logger.Warn("gardener reconcile-runlog: UpdateBeadFields failed",
			"trigger", trig.ID, "error", err)
		return
	}
	logger.Info("gardener reconcile-runlog: recorded runs from iteration beads",
		"trigger", trig.ID, "appended", appended, "total_entries", len(runLog))

	// Mutate in place so any later reader in this evalOne cycle observes the
	// healed run_log (e.g. cleanRunLog-driven trajectory checks).
	trig.Gardener.RunLog = updated.RunLog
}

// gardenerReconcileAsyncProgress re-scores already-recorded score-0 RunRecords
// against credited progress events discovered THIS cycle, crediting a run for a
// child_closed / mr_merged / decision_resolved that landed in its attribution
// interval [StartedAt, nextRun.StartedAt) — including the gap AFTER the run's
// own closed_at (async-attribution gap, gardener-effectiveness-rubric §D1, tune
// task ka-0000001254).
//
// Why a SEPARATE pass from FinalizeRun's rescue: FinalizeRun rescues a run only
// at finalize time, from events already in state.Events. A delegating gardener's
// dispatched crew close the epic's children asynchronously — often AFTER the
// dispatching run's closed_at, in the gap before the next run starts — so the
// crediting event does not exist yet when the run is finalized and never lands
// in any [startedAt, closedAt] window. That produces a false score-0 tail on a
// gardener that is demonstrably closing work (D2=5, D1=1 — the First Mate
// symptom, ka-0000000498). This pass revisits the recorded runs on the LATER
// reconcile cycle, once the event has been discovered into state.Events, and
// lifts the run that owns the interval the event fell in.
//
// MUST run AFTER gardenerDiscoverProgress (so this cycle's events are in
// state.Events) and AFTER gardenerAgentTracker / gardenerFinalizeRunsFromIterations
// (so every run this cycle finalized is present to be reconsidered). Additive,
// idempotent, and monotone (only lifts 0 -> 1), so it composes with both writers
// without double-counting. No-op without a Progress spec; fail-open on every
// daemon/parse error.
func gardenerReconcileAsyncProgress(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) {
	if trig.Gardener == nil || len(trig.Gardener.Progress) == 0 {
		return
	}
	spec, err := progress.ParseSpec(string(trig.Gardener.Progress))
	if err != nil || spec.IsZero() {
		return
	}
	state, err := progress.ParseState(string(trig.Gardener.ProgressState))
	if err != nil {
		logger.Warn("gardener reconcile-async-progress: parse state failed",
			"trigger", trig.ID, "error", err)
		return
	}
	lifted := state.ReconcileAsyncProgress()
	if lifted == 0 {
		return
	}
	marshalled, mErr := state.Marshal()
	if mErr != nil {
		logger.Warn("gardener reconcile-async-progress: state marshal failed",
			"trigger", trig.ID, "error", mErr)
		return
	}
	// Round-trip the full Gardener JSON so we don't clobber sibling counters
	// other hooks updated this cycle (RunsCompleted, ConsecutiveFailures, ...).
	updated := *trig.Gardener
	updated.ProgressState = json.RawMessage(marshalled)
	raw, jErr := json.Marshal(updated)
	if jErr != nil {
		logger.Warn("gardener reconcile-async-progress: marshal gardener failed",
			"trigger", trig.ID, "error", jErr)
		return
	}
	if err := daemon.UpdateBeadFields(ctx, trig.ID, map[string]string{
		"gardener": string(raw),
	}); err != nil {
		logger.Warn("gardener reconcile-async-progress: UpdateBeadFields failed",
			"trigger", trig.ID, "error", err)
		return
	}
	logger.Info("gardener reconcile-async-progress: credited async-attributed runs",
		"trigger", trig.ID, "lifted", lifted)
	// Mutate in place so later gates in this evalOne cycle (no_progress_for /
	// goal_satisfied via GardenerStopContext.ProgressState) see the lifted scores.
	trig.Gardener.ProgressState = updated.ProgressState
}

// gardenerClosedIterations returns the gardener's closed iteration agent beads,
// queried by the two owner-key fields the spawn paths stamp: trigger_id (bridge
// path) and schedule_id (legacy scheduler path). Mirrors the dual-key union the
// beads-viewer detail page uses (gardenerIterations) so the engine reconciles
// from the same iteration set the UI shows. Deduped by bead id. Fail-open: any
// daemon error is logged and contributes no beads. A daemon that carries the
// engine's closed-iteration cache (gardener_iteration_cache.go) answers from it.
func gardenerClosedIterations(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, gardenerID string) []*beadsapi.BeadDetail {
	if gardenerID == "" {
		return nil
	}
	if src, ok := daemon.(closedIterationSource); ok {
		return src.closedIterations(ctx, logger, gardenerID)
	}
	out := make([]*beadsapi.BeadDetail, 0, gardenerIterationFinalizeLimit)
	seen := make(map[string]bool)
	for _, key := range gardenerIterationOwnerKeys {
		beads, _ := listClosedIterations(ctx, daemon, logger, gardenerID, key)
		for _, b := range beads {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			out = append(out, b)
		}
	}
	return out
}

// listClosedIterations lists the closed agents whose owner key names the
// gardener: the newest gardenerIterationFinalizeLimit by created_at, newest
// first. The reconcilers keep only a gardener's latest runs (run_log the newest
// 50, ProgressState.Runs the newest 64), so the oldest are the ones to leave
// out; with the oldest N a gardener past N runs never reconciled a new one
// (gd-Ahi1H2jR9a). ok is false when the daemon read failed.
func listClosedIterations(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, gardenerID, key string) (beads []*beadsapi.BeadDetail, ok bool) {
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"closed"},
		Fields:   map[string]string{key: gardenerID},
		Sort:     "-created_at",
		Limit:    gardenerIterationFinalizeLimit,
		// Iterations are read for ids, timestamps, agent_state, wrapup and
		// close_reason only; up to 500 closed agents per call (gd-zXdZq4Traf).
		Omit:       leanAgentOmit,
		OmitFields: leanAgentOmitFields,
	})
	if err != nil {
		logger.Warn("gardener finalize-iterations: list iterations failed",
			"trigger", gardenerID, "key", key, "error", err)
		return nil, false
	}
	if res == nil {
		return nil, true
	}
	beads = make([]*beadsapi.BeadDetail, 0, len(res.Beads))
	for _, b := range res.Beads {
		// Defensive client-side recheck: the mock daemon (and a permissive
		// real daemon) may return adjacent agents; only accept beads this
		// gardener actually owns and that are closed.
		if b == nil || b.ID == "" || b.Status != "closed" || b.Fields[key] != gardenerID {
			continue
		}
		beads = append(beads, b)
	}
	return beads, true
}

// gardenerEvaluateExitCondition parses and evaluates the gardener's
// ExitCondition. On match it terminates the gardener via gardenerTerminate
// (slot=loop_end, kind=exit_condition) and returns true; the caller must skip
// the fire path. Mirrors scheduler.(*Scheduler).evaluateExitCondition.
//
// Fail-open: parse / evaluator failures log a warning and return false so a
// transient daemon hiccup cannot permanently wedge the gardener.
func gardenerEvaluateExitCondition(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) bool {
	if trig.Gardener == nil || trig.Gardener.ExitCondition == "" {
		return false
	}
	cond, err := scheduler.ParseCondition(trig.Gardener.ExitCondition)
	if err != nil {
		logger.Warn("gardener exit_condition parse failed, ignoring",
			"trigger", trig.ID, "error", err)
		return false
	}
	met, err := scheduler.EvaluateCondition(ctx, daemon, cond, trig.Project)
	if err != nil {
		logger.Warn("gardener exit_condition evaluation failed, ignoring",
			"trigger", trig.ID, "error", err)
		return false
	}
	if !met {
		return false
	}
	gardenerTerminate(ctx, daemon, logger, trig, scheduler.StopSlotLoopEnd, "exit_condition")
	logger.Info("gardener auto-closed: exit_condition met",
		"trigger", trig.ID, "title", trig.Title)
	return true
}
