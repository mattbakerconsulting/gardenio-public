package bridge

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Evaluate loads and evaluates triggers for all registered sources.
// After evaluation, it prunes seen entries for triggers that no longer exist.
func (e *TriggerEngine) Evaluate(ctx context.Context) {
	// Quiet-gardener watch (gd-bo2RUmtpHb). An event-mode plan gardener that has
	// not run in 2h while its plan has a ready, unassigned step is woken with a
	// plan-activity event and the wake is noted on the plan; a second wake of
	// the same gardener within a day files ONE deduped operator escalation.
	// It runs BEFORE the source passes, unlike the sweeps below: the wake is an
	// event injected into the multi source's buffer, which the runner drains
	// after this Evaluate, so only a multi pass later in this same cycle can
	// take it into the gardener's debounce window (gd-yNp2s5Go85).
	// Self-throttled to every 15 minutes, pause-aware, and a no-op unless
	// EnableQuietGardenerSweep was called.
	e.SweepQuietGardeners(ctx)

	gate := e.loadGateState(ctx)
	activeTriggers := make(map[string]bool)
	for _, source := range e.sources {
		ids := e.evaluateSource(ctx, source, gate)
		for _, id := range ids {
			activeTriggers[id] = true
		}
	}
	e.pruneStaleTriggers(activeTriggers)

	// Age out INNER dedup keys of still-active triggers so a long-lived
	// bead-state trigger's seen set cannot grow unbounded and OOM the
	// single-replica engine (ka-0000003049). Self-throttled to
	// triggerSeenGCInterval, so calling it every cycle is cheap.
	e.gcSeenKeys()

	// Engine-side gardener-liveness heartbeat invariant (B2, ka-0000001541).
	// Runs AFTER the source passes so this cycle's freshly-stamped last_checked
	// values are visible: a gardener that fired this cycle reads as fresh, and
	// only a gardener the engine no longer sees (source-closed, disabled, or
	// dropped) trips the staleness check. No-op unless livenessEnabled.
	//
	// The liveness, check-liveness, pouring-stall and dark-lane sweeps below
	// read the same open-trigger roster; share one read for this cycle
	// (gd-zXdZq4Traf). Taken after the source passes, so this cycle's stamps
	// are in it, exactly as each sweep's own read was.
	e.sweepRoster = &sweepRosterMemo{}
	defer func() { e.sweepRoster = nil }()
	e.SweepGardenerLiveness(ctx)

	// Steady-state poll parity for an operator-configured watched-id allow-list
	// (bug ka-0000004451). The main sweep above is open-only by design (widening
	// it to closed would flood on ~130 retired lineages), and the declarative
	// assert ka-0000002431 is transition-only — it misses a single close event
	// on any SSE/reconciler gap and then never re-fires, since a closed gardener
	// emits no further lifecycle event. This pass re-checks the explicit id list
	// across ALL statuses every cycle, so a watched gardener that self-closed
	// (max_runs/no_progress_for) or was disabled is caught regardless of whether
	// its transition was ever delivered. No-op unless livenessEnabled AND the
	// allow-list is non-empty.
	e.SweepGardenerLivenessAllowList(ctx)

	// CHECK-liveness sweep for NON-gardener triggers (ka-0000004245). The two
	// gardener sweeps above skip anything without a `gardener` block, so a plain
	// intake trigger whose last_checked stopped advancing was invisible to
	// every existing detector — including trigger-starved, which watches the
	// INVERSE symptom (last_checked advancing too fast). Runs after the source
	// passes so this cycle's fresh stamps are visible and only a genuinely
	// unchecked lane trips. No-op unless checkLivenessEnabled.
	e.SweepTriggerCheckLiveness(ctx)

	// Engine-side "pouring-but-never-worked" stall sweep (ka-0000000957). The
	// sibling of the liveness sweep for the ORTHOGONAL failure: an apply_formula
	// trigger whose last_checked stays fresh (so the liveness sweep reads it as
	// alive) while the molecules it pours accumulate open with unworked step
	// beads. Runs after the source passes so this cycle's fires are reflected;
	// each trigger's heavier molecule walk is throttled to once per recheck TTL.
	// No-op unless pouringStallEnabled.
	e.SweepPouringButNeverWorked(ctx)

	// Engine-side dark-lane-by-concurrency detector (bug ka-0000002023). The
	// third sibling: catches a spawn-producing trigger that is enabled and
	// actively evaluated (last_checked fresh — invisible to the liveness sweep)
	// yet has not FIRED in many cadences because a wedged agent pins its
	// max_concurrent lease. Runs after the source pass so this cycle's fires are
	// reflected in last_run. No-op unless darkLaneEnabled.
	e.SweepDarkLaneByConcurrency(ctx)

	// Engine-side credential-wedged-agent detector (bug ka-0000000592). The
	// fourth sibling: catches an agent parked in agent_state=rate_limited/
	// budget_exceeded with zero completed turns while SyncAll keeps churning its
	// updated_at fresh — the credential-exhaustion dimension of the same
	// "invisible to updated_at" gap ka-0000002023 named for churn. WARN-logs +
	// one-shot-comments on the wedged agent bead (never spawns/kills). No-op
	// unless credWedgeEnabled.
	e.SweepCredentialWedge(ctx)

	// Closed+enabled dark-trigger DRAIN sweep (bug ka-0000002082). Unlike the
	// three ALERT sweeps above, this one is the CAPACITY fix that drains the
	// closed+enabled pool the liveness SCOPE BOUNDARY hands to trigger-reviver:
	// it auto-archives retired-lineage triggers (gardener-epic CLOSED) and files
	// ONE batched archive decision for the unanchored cohort, so the reviver's
	// scarce 1/cycle human-decision budget reaches the genuinely-uncovered lanes.
	// Self-throttled to once per hour (it mutates beads), pause-aware, and a
	// no-op unless closedEnabledDrainEnabled.
	e.SweepClosedEnabledDrain(ctx)

	// Gardener-reaper sweep (task ka-0000000102). The sixth sibling and the
	// missing quadrant of the drain sweep above: where SweepClosedEnabledDrain
	// drains CLOSED+enabled retired lineages, this one closes OPEN gardener
	// triggers whose pinned epic is CLOSED — the enabled-but-inert (category C)
	// and dead-disabled (category A) gardeners from audit ka-0000001071 that no
	// other sweep reaps (liveness only ALERTS and skips closed-epic gardeners;
	// the drain sweep's roster is status=closed). Skips standing/perpetual
	// gardeners by machine marker AND terminal_goal prose (ka-0000006241 safety).
	// Self-throttled to once per hour (it mutates beads), pause-aware, and a
	// no-op unless closedEpicReaperEnabled.
	e.SweepClosedEpicGardeners(ctx)

	// Uncovered-open-lane roll-up report (bug ka-0000002082 fix 4). The read-only
	// counterpart of the drain sweep above: it surfaces the class the drain sweep
	// DELIBERATELY leaves untouched — closed+enabled triggers on an OPEN
	// gardener-epic that still has live children and no live successor gardener —
	// as ONE living type=doc report, so the reviver reads one signal instead of
	// filing one revival-review decision per lane. Mutates no triggers, files no
	// decisions; self-throttled to once per day, pause-aware, and a no-op unless
	// uncoveredOpenLaneRollupEnabled.
	e.SweepUncoveredOpenLaneRollup(ctx)

	// Age-based ESCALATION of stale revival-review decisions (bug ka-0000002082
	// fix 3). The third leg of the same bug: the drain sweep above reduces the
	// INFLOW of revival-review decisions and the roll-up decouples the
	// uncovered-open lanes, but the decisions that ARE filed still park at their
	// filed priority forever (measured: 10 days at P2 behind newer work). This
	// sweep raises a stale open revival-review up the P2→P1→P0 ladder so it
	// surfaces above newer work in the operator queue. Mutates only the priority
	// attribute (reversible, audited), self-throttled to once per hour, pause-
	// aware, and a no-op unless staleRevivalReviewEscalateEnabled.
	e.SweepStaleRevivalReviewEscalate(ctx)

	// Daily dormant-gardener safety net (gd-kkrCdlf4iO). A gardener dormant over
	// 24h whose live dormancy probe says it has actionable children should have
	// woken on its own cycle (bug gd-FUC635HJ2E); a hit means its wake path is
	// not running. Files ONE deduped operator escalation naming them, never
	// wakes or spawns. Self-throttled to once per day, pause-aware, and a no-op
	// unless dormantGardenerSweepEnabled.
	e.SweepDormantGardeners(ctx)
}

// EvaluateSource loads and evaluates triggers for a specific source by name.
func (e *TriggerEngine) EvaluateSource(ctx context.Context, sourceName string) {
	source, ok := e.sources[sourceName]
	if !ok {
		e.logger.Warn("triggers: unknown source", "source", sourceName)
		return
	}
	_ = e.evaluateSource(ctx, source, e.loadGateState(ctx))
}

func (e *TriggerEngine) evaluateSource(ctx context.Context, source TriggerSource, gate triggerGateState) []string {
	triggers, err := source.LoadTriggers(ctx, e.daemon)
	if err != nil {
		e.logger.Error("triggers: failed to load triggers",
			"source", source.Name(), "error", err)
		return nil
	}

	isScheduleSource := scheduleSourceNames[source.Name()]

	e.logger.Info("triggers: cycle start",
		"source", source.Name(), "trigger_count", len(triggers),
		"globally_paused", gate.globallyPaused, "projects_disabled", len(gate.projectDisabled),
		"scheduler_paused", gate.schedulerPaused, "projects_schedules_disabled", len(gate.projectSchedulesDisabled),
		"is_schedule_source", isScheduleSource)

	ids := make([]string, 0, len(triggers))
	evaluated := 0
	skippedGated := 0
	for _, trig := range triggers {
		ids = append(ids, trig.ID)
		if gate.skipTrigger(isScheduleSource, trig) {
			skippedGated++
			e.logger.Info("triggers: skipped (paused)",
				"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
				"project", trig.Project, "globally_paused", gate.globallyPaused,
				"scheduler_paused", gate.schedulerPaused)
			continue
		}
		e.evalOne(ctx, source, trig)
		evaluated++
	}

	e.logger.Info("triggers: cycle complete",
		"source", source.Name(), "trigger_count", len(triggers),
		"evaluated", evaluated, "skipped_gated", skippedGated)

	// Per-source starvation-state prune. pruneStaleTriggers only runs on the
	// all-sources Evaluate path, but single-source deployments (the
	// jira-bridge drives EvaluateSource("jira") directly) never call it — a
	// trigger closed while starved would keep publishing starved=1 and page
	// forever. Pruning here, scoped to entries this source owns, drops the
	// series as soon as the trigger stops loading (trigger_starve.go).
	e.pruneStarveForSource(source.Name(), ids)
	return ids
}

// pruneStarveForSource deletes starvation-detector state (and its published
// gauge series) for triggers this source previously reported on but no longer
// loads. Entries currently owned by a different source are left alone so
// multi-source engines cannot prune each other's state.
func (e *TriggerEngine) pruneStarveForSource(sourceName string, activeIDs []string) {
	active := make(map[string]bool, len(activeIDs))
	for _, id := range activeIDs {
		active[id] = true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, st := range e.starve {
		if st.source == sourceName && !active[id] {
			triggerCooldownSkipStreak.DeleteLabelValues(st.source, id)
			triggerStarvedGauge.DeleteLabelValues(st.source, id)
			delete(e.starve, id)
		}
	}
}

// triggerGateState is a one-cycle snapshot of trigger pause state. It captures
// the global triggers-pause gate bead state and the per-project
// triggers_enabled flag so the engine can make a single eval-loop decision per
// trigger without re-querying for every match. It also captures the
// scheduler-pause gate and per-project schedules_enabled flag so
// schedule-flavored sources (trigger:schedule, schedule-mirror) honor
// `/schedule off` the same way type=schedule beads already do.
type triggerGateState struct {
	globallyPaused           bool
	projectDisabled          map[string]bool
	schedulerPaused          bool
	projectSchedulesDisabled map[string]bool
}

// skip reports whether triggers for the given project should be skipped this
// evaluation cycle. Empty project means "no project field set on the trigger",
// in which case only the global gate applies.
func (g triggerGateState) skip(project string) bool {
	if g.globallyPaused {
		return true
	}
	if project == "" {
		return false
	}
	return g.projectDisabled[project]
}

// skipForSchedule reports whether a schedule-flavored trigger source
// (trigger:schedule, schedule-mirror) should be skipped this evaluation
// cycle. It is the union of the regular trigger gate (triggers-pause /
// triggers_enabled) and the schedule subsystem's pause controls
// (scheduler-pause / schedules_enabled). This lets `/schedule off`
// silence cron-driven trigger beads in addition to type=schedule beads —
// the operator contract Matt asked for when wiring schedule-flavored
// triggers into the bridge engine.
func (g triggerGateState) skipForSchedule(project string) bool {
	if g.skip(project) {
		return true
	}
	if g.schedulerPaused {
		return true
	}
	if project == "" {
		return false
	}
	return g.projectSchedulesDisabled[project]
}

// skipTrigger is the per-trigger pause decision evaluateSource makes. A trigger
// from a schedule-flavored source, and any gardener trigger whatever source
// loaded it (scheduleGatedGardener), gets the schedule gates on top of the
// trigger gates (skipForSchedule); everything else gets the trigger gates only.
func (g triggerGateState) skipTrigger(isScheduleSource bool, trig TriggerDef) bool {
	if isScheduleSource || scheduleGatedGardener(trig) {
		return g.skipForSchedule(trig.Project)
	}
	return g.skip(trig.Project)
}

// stewardMarkerLabel is steward.MarkerLabel, the label on a land's steward
// trigger. bridge cannot import internal/steward (steward imports bridge);
// TestSteward_NotGatedBySchedulesOff pins the copy against a steward exactly as
// steward.Bind writes it.
const stewardMarkerLabel = "steward"

// scheduleGatedGardener reports whether trig is a gardener that `/schedule off`
// (the scheduler-pause gate and a project's schedules_enabled=false) must
// silence even though its source is not schedule-flavored (gd-gCN4m4DYxc).
//
// A plan gardener used to be a trigger:schedule bead, so `/schedule off
// <project>` stopped it through the schedule source. Event mode (PR #1907, the
// default for new plan gardeners) makes it a trigger:multi bead, which the
// multi source loads and which therefore got only the triggers-pause gate: an
// operator who quieted one project still had its plan gardeners waking on plan
// activity and on the daily watchdog. Keying the gate on the gardener block
// rather than the source keeps a gardener's pause controls the same in both
// modes.
//
// Two kinds of trigger keep their old gating:
//   - a non-gardener trigger:multi bead is event-driven and is managed by
//     `/trigger`, like jira/gitlab/webhook triggers;
//   - a land's steward (stewardMarkerLabel). It was always trigger:multi, so
//     `/schedule off <project>` never applied to it, and tenant lands are
//     provisioned with schedules_enabled=false (lands/provision), so gating it
//     here would silence the steward of any such land whose triggers are
//     enabled. Its own controls are the land freeze and lands-pause.
func scheduleGatedGardener(trig TriggerDef) bool {
	return trig.Gardener != nil && !slices.Contains(trig.Labels, stewardMarkerLabel)
}

// loadGateState fetches the global triggers-pause gate and the per-project
// TriggersEnabled flag once per evaluation cycle. Errors are logged and
// downgraded to "not paused" so a daemon hiccup doesn't silently freeze the
// trigger system.
func (e *TriggerEngine) loadGateState(ctx context.Context) triggerGateState {
	state := triggerGateState{}

	gate, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"gate"},
		Statuses: []string{"open", "in_progress"},
		Search:   triggersPauseTitle,
		Limit:    5,
	})
	if err != nil {
		e.logger.Warn("triggers: failed to check global pause gate", "error", err)
	} else if gate != nil {
		for _, b := range gate.Beads {
			if b != nil && b.Title == triggersPauseTitle {
				state.globallyPaused = true
				break
			}
		}
	}

	schedGate, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"gate"},
		Statuses: []string{"open", "in_progress"},
		Search:   schedulerPauseTitleForTriggers,
		Limit:    5,
	})
	if err != nil {
		e.logger.Warn("triggers: failed to check scheduler pause gate", "error", err)
	} else if schedGate != nil {
		for _, b := range schedGate.Beads {
			if b != nil && b.Title == schedulerPauseTitleForTriggers {
				state.schedulerPaused = true
				break
			}
		}
	}

	projects, err := e.daemon.ListProjectBeads(ctx)
	if err != nil {
		e.logger.Warn("triggers: failed to load project beads for gate check", "error", err)
		return state
	}
	for name, info := range projects {
		if !info.TriggersEnabled {
			if state.projectDisabled == nil {
				state.projectDisabled = make(map[string]bool)
			}
			state.projectDisabled[name] = true
		}
		if !info.SchedulesEnabled {
			if state.projectSchedulesDisabled == nil {
				state.projectSchedulesDisabled = make(map[string]bool)
			}
			state.projectSchedulesDisabled[name] = true
		}
	}
	return state
}

// triggerBlockedBy returns the subset of trig.BlockedBy bead IDs that are
// still open (i.e. blocking the trigger). An empty result means the trigger is
// free to evaluate.
//
// Semantics:
//   - Empty BlockedBy → never blocked.
//   - Each referenced bead is fetched via GetBead. A bead with status "open"
//     or "in_progress" is considered pending; any other status (closed,
//     deleted, completed, etc.) is considered resolved.
//   - GetBead errors are logged at WARN and downgraded to "not pending" so a
//     daemon hiccup doesn't silently freeze the trigger. The expected
//     references are operator-managed decision / escalation beads — a missing
//     row almost always means the decision was deleted/closed and the trigger
//     should resume.
func (e *TriggerEngine) triggerBlockedBy(ctx context.Context, trig TriggerDef) []string {
	if len(trig.BlockedBy) == 0 {
		return nil
	}
	var pending []string
	for _, id := range trig.BlockedBy {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		bead, err := e.daemon.GetBead(ctx, id)
		if err != nil {
			e.logger.Warn("triggers: blocked_by lookup failed",
				"trigger", trig.ID, "blocked_by", id, "error", err)
			continue
		}
		if bead == nil {
			continue
		}
		if isOpenStatus(bead.Status) {
			pending = append(pending, id)
		}
	}
	return pending
}

// isOpenStatus reports whether a bead status represents an unresolved bead
// from the trigger gate's perspective. Open and in_progress are the standard
// "still pending" statuses in beads.
func isOpenStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "open", "in_progress", "in-progress", "inprogress":
		return true
	}
	return false
}

// eventReadySource is an optional interface a TriggerSource may implement to
// report whether a trigger has a matching buffered event waiting (read-only).
// The multi-source path implements it so an event-eligible trigger can bypass
// the per-trigger cooldown gate when a real event is pending. See bug
// ka-0000005988 and MultiTriggerSource.EventReady.
type eventReadySource interface {
	EventReady(ctx context.Context, trig TriggerDef) bool
}

// cronDueSource is an optional interface a TriggerSource may implement to
// report whether a trigger has a cron entry due right now (in-window and past
// last_run; read-only). The multi-source and schedule-mirror paths implement
// it so a cron-due trigger can bypass the per-trigger cooldown gate — without
// it, a long cooldown that divides 24h phase-locks the evaluation slots
// outside the cron's active window and the trigger never fires again while
// looking alive. See bug ka-0000004307 and scheduler.HasDueCronEntry.
type cronDueSource interface {
	CronDue(ctx context.Context, trig TriggerDef) bool
}

// assertRecoverySource is an optional interface a TriggerSource may implement to
// report the assert violation dedup keys whose invariant now HOLDS again, so the
// engine can re-arm them by dropping them from `seen` — the recovery half of the
// per-episode assert dedup (bug ka-0000006401). Without it an episode-stable
// violation key would suppress the NEXT genuine violation until the 24h seen-key
// TTL. The bead source implements it; other sources leave it unimplemented and
// the engine skips the re-arm step for them.
type assertRecoverySource interface {
	RecoveredKeys(ctx context.Context, trig TriggerDef) []string
}

// evalOne evaluates a single trigger: runs the source query, finds new matches, fires actions.
func (e *TriggerEngine) evalOne(ctx context.Context, source TriggerSource, trig TriggerDef) {
	// Out-of-band last_checked detector (ka-0000006237). A WARN here means
	// some OTHER writer advanced this trigger's last_checked since our
	// previous cycle — if it keeps happening while we cooldown-skip, this
	// engine is being starved and will never fire.
	if foreign, prev := e.noteForeignLastChecked(trig); foreign {
		e.logger.Warn("triggers: last_checked advanced out-of-band — another evaluator is stamping this trigger (possible cross-env cooldown starvation)",
			"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
			"previous", prev.UTC().Format(time.RFC3339),
			"observed", trig.LastChecked.UTC().Format(time.RFC3339))
	}

	// Cooldown check. Honors per-trigger cooldown_minutes; floor is triggerCooldown.
	//
	// Event-eligible bypass (bug ka-0000005988): the cooldown gate throttles
	// cron/idle re-eval, but it must NOT swallow event wakes. A multi-source
	// trigger with a matching buffered event would otherwise be cooldown-skipped
	// here BEFORE Evaluate runs — the event is then drained at cycle end and
	// silently lost. When the source reports a pending matching event, skip the
	// cooldown gate so Evaluate consumes the event this cycle. The per-entry
	// throttle (checked inside HasPendingEventMatch) still rate-limits event
	// fires, so this does not reopen a thundering-herd hole.
	//
	// Cron-due bypass (bug ka-0000004307): the cooldown gate must not swallow a
	// due cron window either. Every evaluation stamps last_checked (fire or
	// not), so evaluations recur at fixed wall-clock offsets — and a cooldown
	// that divides 24h evenly (e.g. 240m) phase-locks those offsets. When none
	// of them lands inside the cron's active window (the source's InWindow
	// check suppresses catch-up fires outside it), the trigger silently ceases
	// firing forever while last_checked stays fresh: gardener ka-0000005204 was
	// dark 6 days this way. When the source reports a cron entry due right now,
	// bypass the cooldown so this window's fire is evaluated; the seen-set
	// dedup and last_run suppression still prevent re-firing a window that was
	// already consumed.
	if lastEval := e.lastEvaluated(trig); !lastEval.IsZero() && time.Since(lastEval) < effectiveCooldown(trig) {
		bypass := ""
		// Test-only bypass (ka-0000007982): cmd/trigger-probe marks the trigger
		// it deliberately fires via IgnoreCooldown so a last_checked stamp
		// written by ANOTHER evaluator on the same daemon (the E2E namespace's
		// live 30s-poll trigger-engine) cannot cooldown-starve the probe's
		// single injected event. Never set by the production engine.
		if e.cooldownIgnored(trig.ID) {
			bypass = "cooldown ignored (test seam)"
		}
		if bypass == "" {
			if ers, ok := source.(eventReadySource); ok && ers.EventReady(ctx, trig) {
				bypass = "pending event"
			}
		}
		if bypass == "" {
			if cds, ok := source.(cronDueSource); ok && cds.CronDue(ctx, trig) {
				bypass = "due cron window"
			}
		}
		if bypass == "" {
			e.logger.Info("triggers: skipped (cooldown)",
				"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
				"last_checked", trig.LastChecked.UTC().Format(time.RFC3339),
				"age", time.Since(trig.LastChecked).String(),
				"cooldown", effectiveCooldown(trig).String())
			// Deliberately do NOT stamp here: the cooldown-skip path returns with
			// zero evaluation side effects so the out-of-band-advance detector
			// (ka-0000006237) can attribute ANY last_checked movement to a foreign
			// evaluator. Cooldown is already self-evident from a fresh last_checked
			// plus cooldown_minutes, and it is NOT the dark-gardener case this bead
			// targets (that one reaches Evaluate; see the fold-in stamps below).
			e.noteCooldownSkip(ctx, source, trig)
			return
		}
		e.logger.Info("triggers: cooldown bypassed",
			"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
			"reason", bypass, "cooldown", effectiveCooldown(trig).String())
	}

	// Past the cooldown gate: the engine has its fire window this cycle, so
	// the trigger is not cooldown-starved regardless of what evaluation
	// produces below (fired / no-new-matches / blocked / error). Reset the
	// starvation streak (Alert B of epic ka-0000005107, trigger_starve.go).
	e.noteCooldownPassed(source, trig)

	// blocked_by gate: pause the trigger while ANY referenced decision /
	// escalation bead is still open. Deliberately runs BEFORE source.Evaluate
	// and does NOT update last_checked — that way the trigger resumes on the
	// very next evaluation cycle after the referenced bead closes (no
	// controller restart, no cooldown delay).
	if pending := e.triggerBlockedBy(ctx, trig); len(pending) > 0 {
		e.logger.Info("triggers: blocked by open decision",
			"trigger", trig.ID, "blocked_by", trig.BlockedBy, "pending", pending)
		// Deliberately write NOTHING here (not even last_outcome): the trigger
		// must resume on the very next cycle the instant the referenced bead
		// closes, with no last_checked advance and no cooldown delay — the
		// contract asserted by TestTriggerEngine_BlockedByOpenDecision. This
		// case is already diagnosable without a stamp: the open blocker shows on
		// the trigger's blocked_by field and as an open decision/escalation bead.
		return
	}

	// Out-of-band operator re-arm normalization (bug ka-0000004191). An OPEN
	// gardener still carrying the terminate/escalate markers was reopened at
	// the bead level (the engine only stamps those in the CloseBead that
	// closes a gardener, and its own reopen path clears them atomically). Such
	// a re-arm is otherwise structurally insufficient: the stale resume
	// bookkeeping either escalate-re-closes the gardener within one cycle or
	// deadlocks it in the bare-reconcile no-op, and a manually-cleared
	// ProgressState is rebuilt from immutable iteration history by the
	// finalize hook below. Apply the same durable reset every engine revive
	// path uses, and clear the markers. MUST run after the blocked_by gate
	// (a reopened gardener parked on an open decision stays parked) and before
	// gardenerDiscoverProgress / gardenerFinalizeRunsFromIterations so this
	// cycle already honors the fresh RunsFinalizedAfter baseline. A true
	// return means the reset was needed but did not persist — skip the cycle
	// so the pre-fire gates cannot escalate-close on the stale counters.
	if trig.Gardener != nil {
		if gardenerRearmFromOperatorReopen(ctx, e.daemon, e.logger, trig) {
			e.logger.Warn("triggers: operator-reopen normalization failed to persist, skipping cycle",
				"trigger", trig.ID)
			return
		}
	}

	// Gardener progress discovery (Phase 3b/B7-c2, ka-0000003115). MUST run
	// BEFORE gardenerAgentTracker's FinalizeRun (bug ka-0000006914): FinalizeRun
	// rescues a self-reported score-0 run when a credited progress event
	// (mr_merged / child_closed / decision_resolved) landed inside the run
	// window. Those events only enter state.Events here, so discovery has to
	// happen first or the just-closed run's outcome event is invisible to the
	// rescue and the run is mis-scored as barren — falsely advancing
	// no_progress_for. Also runs before the stop-spec / exit_condition gates so
	// progress-driven kinds see fresh state on this cycle. No-op when the
	// gardener has no Progress spec.
	if trig.Gardener != nil {
		gardenerDiscoverProgress(ctx, e.daemon, e.logger, trig, time.Now())
	}

	// Gardener post-run bookkeeping (Phase 3b/B7-c1, ka-0000003913). Runs
	// inline at the start of evalOne so the bumped counters are visible to
	// the gardener pre-fire stop check below (max_runs / consecutive_failures
	// stop kinds read RunsCompleted / ConsecutiveFailures via
	// GardenerStopContext). No-op for non-gardener triggers and for gardeners
	// that have not yet spawned an agent. Runs AFTER gardenerDiscoverProgress so
	// FinalizeRun's event-aware no-progress rescue sees this cycle's discovered
	// events (bug ka-0000006914).
	if trig.Gardener != nil && trig.Gardener.LastAgentID != "" {
		gardenerAgentTracker(ctx, e.daemon, e.logger, trig)
		// Close the per-run task bead now that the agent has terminated.
		// Mirrors scheduler.closePerRunTaskBead (Phase 3b/B7-c2,
		// ka-0000003115). Shares the same free helper used by the scheduler.
		gardenerClosePerRunTaskBead(ctx, e.daemon, e.logger, trig)
	}

	// Iteration-bead-driven run finalization (bug ka-0000006915). The
	// LastAgentID tracker above only finalizes the single most-recently-stamped
	// agent — which silently drops runs for subtree-mode (assign_bead) gardeners
	// that fire concurrently (max_concurrent>1, no one_spawn_at_a_time), because
	// each fire overwrites the lone LastAgentID pointer before its predecessors
	// are observed closed, and never runs at all when LastAgentID is empty. This
	// reconciler finalizes EVERY closed iteration bead the gardener owns into
	// ProgressState.Runs, deduped by AgentID, so /maps run glyphs and the
	// no_progress_for / goal_satisfied stop kinds (which read ProgressState.Runs)
	// work for the common subtree gardener. Additive and idempotent — composes
	// with gardenerAgentTracker without double-counting. Runs AFTER
	// gardenerDiscoverProgress so FinalizeRun's event-aware rescue sees this
	// cycle's discovered events; runs BEFORE the pre-fire stop gates below so
	// they judge against fresh runs this cycle. No-op without a Progress spec.
	if trig.Gardener != nil {
		gardenerFinalizeRunsFromIterations(ctx, e.iterationDaemon(), e.logger, trig)
	}

	// Iteration-bead-driven run_log reconciliation (task ka-0000005584, audit
	// finding on Phase 3b gardener ka-0000003669: 84 runs_completed but a 7-entry
	// run_log). run_log is appended ONLY by gardenerAgentTracker's single-pointer
	// fast path (LastAgentID), so a concurrent gardener overwrites that pointer
	// before predecessors are seen closed and drops all but the last run from the
	// log even as runs_completed keeps climbing — the "N completed but sparse
	// run_log" divergence. This heals run_log from EVERY closed iteration bead the
	// gardener owns, deduped by agent id. Unlike the ProgressState finalizer above
	// it is NOT gated on a Progress spec (run_log is a pure visibility artifact
	// that must fill for progress-less gardeners like Phase 3b). Additive and
	// idempotent — composes with gardenerAgentTracker's fast path.
	if trig.Gardener != nil {
		gardenerReconcileRunLogFromIterations(ctx, e.iterationDaemon(), e.logger, trig)
	}

	// Async-attribution reconciliation (tune task ka-0000001254,
	// gardener-effectiveness-rubric §D1). A delegating gardener's dispatched crew
	// close the epic's children ASYNCHRONOUSLY — often in the gap AFTER the
	// dispatching run's closed_at but BEFORE the next run starts — so the
	// child_closed / mr_merged / decision_resolved event lands in NEITHER run's
	// [startedAt, closedAt] FinalizeRun rescue window and BOTH runs record a false
	// score-0. That collapses D1 (progress yield) to a C/D grade even while D2
	// (throughput) reads 5 (the First Mate symptom, ka-0000000498: D2=5, D1=1).
	// This pass re-scores each recorded score-0 run against events discovered THIS
	// cycle, crediting the run whose attribution interval [StartedAt,
	// nextRun.StartedAt) the event fell in. MUST run AFTER gardenerDiscoverProgress
	// (events present) and AFTER the two finalizers (runs present); additive,
	// idempotent, and monotone (lifts 0 -> 1 only). No-op without a Progress spec.
	if trig.Gardener != nil {
		gardenerReconcileAsyncProgress(ctx, e.daemon, e.logger, trig)
	}

	// Unbacked gardener-prose guard (bug ka-0000006735). A trigger whose prompt
	// claims a run_budget/terminal_goal but carries NO gardener block bypasses
	// the entire gardener gate stack below — the budget/goal are pure prose the
	// engine never reads. Warn loudly (once per trigger) so operators can spot
	// the footgun in the controller logs and back it with a real gate.
	e.warnUnbackedGardenerProse(trig)

	// Repo-less git-path guard, firing-time half (bug ka-0000004796, task
	// ka-0000004167). A project-less agent-spawning trigger (spawn_agent OR
	// apply_formula) whose prompt names a git path spawns a repo-less pod where
	// that path 404s. Warn (once per trigger) so operators can spot the footgun
	// in the controller logs and inline the content into an advice/bundle.
	// Suppressed when the prompt already acknowledges the repo-less runtime.
	e.warnRepolessRepoPaths(trig)

	// Global bounded-stop guard, firing-time half (ka-0000006744). A live
	// gardener bead lacking a positive run_budget + bounded hard_stop wall_clock
	// is warned (once per trigger) but still allowed to fire — warn-with-grace
	// so gardeners predating this guard are not disabled mid-flight. New gardeners
	// are fail-closed at authoring time; the fleet-wide stop is the explicit
	// `gardeners-pause` kill-switch gate (enforced in gardenerPreFireCheck).
	e.warnUnboundedGardener(trig)

	// Standing-marker guard, firing-time half (bug ka-0000004311). A live
	// gardener whose terminal_goal DECLARES it perpetual but carries no
	// machine-readable standing marker is warned (once per trigger) but still
	// allowed to fire — warn-with-grace so the marker-less cohort predating this
	// guard is neither disabled nor auto-modified mid-flight. New gardeners are
	// fail-closed at authoring time (cmd/gb/{gardener,trigger}.go).
	e.warnStandingProseWithoutMarker(trig)

	// Gardener pre-fire gates (Phase 3b/B6). Triggers without a Gardener block
	// bypass these gates entirely — source-only triggers are unchanged.
	// Gardeners gate on run_budget exhaustion, one_spawn_at_a_time concurrency,
	// and stop-spec match. A matched stop spec closes the trigger; the other
	// two gates just skip this evaluation cycle.
	if trig.Gardener != nil {
		if skip, reason := gardenerPreFireCheck(ctx, e.daemon, e.logger, trig); skip {
			e.logger.Info("triggers: gardener pre-fire skip",
				"trigger", trig.ID, "reason", reason)
			// Deliberately no stamp: the gardener pre-fire gates (dormancy,
			// run_budget, one_spawn_at_a_time, stop-spec) return with zero writes
			// so a dormant/wedged gardener resumes on the very next cycle once its
			// blocker clears — the contract pinned by the DormantStays* /
			// SkipsFireSilently tests. Each reason is already independently
			// diagnosable (dormancy park-markers, blob run counters, the live
			// pinned agent), so it does not need last_outcome. The dark-gardener
			// case this bead targets reaches Evaluate below, where the outcome
			// IS folded into the once-per-cycle last_checked stamp.
			return
		}
		// Gardener exit_condition (Phase 3b/B7-c2, ka-0000003115). Mirrors
		// scheduler.evaluateExitCondition: a met exit_condition terminates the
		// gardener via gardenerTerminate(loop_end:exit_condition) and skips fire.
		// Runs AFTER pre-fire so a matched stop_spec wins on slot precedence
		// (hard_stop > loop_end), consistent with scheduler behavior.
		if gardenerEvaluateExitCondition(ctx, e.daemon, e.logger, trig) {
			return
		}
	}

	// Bootstrap guard for triggers created AFTER engine startup (so catchUpSource
	// didn't see them). If this is the first time the engine is evaluating this
	// trigger — last_checked unset AND no in-memory seen state — seed the seen
	// set from prior beads OR, if there are none, from all current matches
	// WITHOUT firing. This prevents a thundering herd of actions for pre-existing
	// events (e.g. the 100 historical failed pipelines GitLab returns for a
	// freshly-created pipeline-failure trigger). Mirrors the bootstrap logic in
	// catchUpSource, applied at evaluation time so runtime-created triggers are
	// also protected. Event-driven sources (slack-reaction, bead-state) opt out
	// because their Evaluate returns fresh buffered events, not historical state.
	if sourceNeedsBootstrap(source) {
		e.mu.Lock()
		_, hasSeen := e.seen[trig.ID]
		e.mu.Unlock()
		if trig.LastChecked.IsZero() && !hasSeen {
			if e.bootstrapTrigger(ctx, source, trig) {
				return
			}
		}
	}

	// Run the source-specific evaluation.
	matches, err := source.Evaluate(ctx, trig)
	if err != nil {
		e.logger.Error("triggers: evaluation failed",
			"source", source.Name(), "trigger", trig.ID, "title", trig.Title, "error", err)
		return
	}

	// Re-arm recovered assert violations (bug ka-0000006401). An assert trigger's
	// violation dedup key is episode-stable (assertViolationKey), so a continuous
	// violation collapses to ONE alert — but that same stability means the key
	// lingers in `seen` after the invariant recovers, silently suppressing the
	// NEXT genuine violation until the 24h TTL evicts it. When the source reports
	// the invariant now holds for a watched bead, drop its clause keys from `seen`
	// (and the GC mirror) so a fresh violation re-alerts once per episode, matching
	// SweepGardenerLiveness.markFresh. Runs before the seen-set filter below; on any
	// given poll a bead is either violating or holding, never both, so a recovered
	// key cannot race this cycle's own fresh violation of the same key.
	if ars, ok := source.(assertRecoverySource); ok {
		if recovered := ars.RecoveredKeys(ctx, trig); len(recovered) > 0 {
			e.mu.Lock()
			cleared := 0
			if keys := e.seen[trig.ID]; keys != nil {
				for _, k := range recovered {
					if keys[k] {
						delete(keys, k)
						cleared++
					}
					if at := e.seenAt[trig.ID]; at != nil {
						delete(at, k)
					}
				}
			}
			e.mu.Unlock()
			if cleared > 0 {
				e.logger.Info("triggers: re-armed recovered assert violations",
					"source", source.Name(), "trigger", trig.ID, "cleared", cleared)
			}
		}
	}

	// Filter to new matches (not yet seen by this trigger).
	e.mu.Lock()
	if e.seen[trig.ID] == nil {
		e.seen[trig.ID] = make(map[string]bool)
	}
	seenSet := e.seen[trig.ID]
	e.mu.Unlock()

	var newMatches []TriggerMatch
	for _, m := range matches {
		if !seenSet[m.Key] {
			newMatches = append(newMatches, m)
		}
	}

	// orphanNotes names, for last_outcome, every suppressed match whose fire
	// filed a task but spawned no agent (gd-CZnK7jeKSG, trigger_orphan_fire.go).
	// refired is true when this cycle's matches are such re-fires, which the
	// pre-fire refresh below must not suppress again.
	var orphanNotes []string
	refired := false
	if len(newMatches) == 0 && len(matches) > 0 {
		newMatches, orphanNotes = e.refireOrphanedMatches(ctx, source, trig, matches)
		refired = len(newMatches) > 0
	}

	if len(newMatches) == 0 {
		// Split the two indistinguishable-from-outside cases the ka-0000001513
		// investigation had to separate by hand (source read + live cron
		// experiment): the source produced NO matches at all (a schedule
		// trigger out of window / preflight-failed / already-fired-this-window),
		// versus the source DID produce matches but the engine seen-set
		// dedup-suppressed every one (the fire window was already consumed).
		outcome := "no source matches (out of window / preflight not met / already fired)"
		if len(matches) > 0 {
			outcome = fmt.Sprintf("all %d match(es) deduped by seen-set (window already fired)", len(matches))
		}
		if err := e.stampCheckedOutcome(ctx, trig.ID, withOrphanNotes(outcome, orphanNotes)); err != nil {
			e.logger.Warn("triggers: failed to update last_checked",
				"trigger", trig.ID, "error", err)
		}
		e.logger.Info("triggers: evaluated, no new matches",
			"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
			"total_matches", len(matches), "seen_size", len(seenSet))
		return
	}

	// Pre-fire dedup refresh against persisted state (ka-0000000151).
	//
	// The in-memory seen[] only gets stamped AFTER a successful fireAction.
	// If a prior tick's fireAction partial-failed — e.g. createTaskBead
	// succeeded but SpawnAgent errored on an invalid field name, as in
	// ka-0000000883 — the task bead exists but seenSet[m.Key] is never set.
	// Subsequent ticks then create another duplicate task bead for the same
	// fire key, producing the 3x dups observed for schedule-mirror cron fires.
	// This also handles engine restarts that lose the in-memory seen state
	// before catch-up rehydrates it.
	//
	// Refresh now-and-only-now (when we'd otherwise fire) so the extra
	// daemon round-trip is paid only on non-empty match sets, not every
	// 30s idle tick.
	//
	// A persisted task key is not proof the fire spawned: when SpawnAgent never
	// landed, refireOrphanedMatches puts the window back (gd-CZnK7jeKSG).
	// Matches it already put back this cycle skip the refresh.
	if priorKeys, err := source.CatchUpKeys(ctx, e.daemon, trig.ID); !refired && err == nil && len(priorKeys) > 0 {
		e.mu.Lock()
		for _, k := range priorKeys {
			seenSet[k] = true
		}
		e.mu.Unlock()
		var refiltered, suppressed []TriggerMatch
		for _, m := range newMatches {
			if !seenSet[m.Key] {
				refiltered = append(refiltered, m)
			} else {
				suppressed = append(suppressed, m)
			}
		}
		if len(suppressed) > 0 {
			e.logger.Info("triggers: pre-fire dedup refresh suppressed re-fire",
				"source", source.Name(), "trigger", trig.ID,
				"suppressed", len(suppressed))
			var orphans []TriggerMatch
			orphans, orphanNotes = e.refireOrphanedMatches(ctx, source, trig, suppressed)
			refiltered = append(refiltered, orphans...)
		}
		newMatches = refiltered
		if len(newMatches) == 0 {
			if err := e.stampCheckedOutcome(ctx, trig.ID, withOrphanNotes("all matches deduped by persisted state (already fired)", orphanNotes)); err != nil {
				e.logger.Warn("triggers: failed to update last_checked",
					"trigger", trig.ID, "error", err)
			}
			e.logger.Info("triggers: evaluated, dedup-suppressed all matches",
				"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
				"total_matches", len(matches))
			return
		}
	}

	e.logger.Info("triggers: new matches",
		"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
		"new", len(newMatches), "total", len(matches))

	// Fire action for each new match, respecting max_concurrent.
	//
	// For spawn-producing actions, max_concurrent is a LIVE lease, not a
	// per-batch cap: we seed `fired` with the number of agents this trigger
	// already has alive (status open/in_progress, spawned_by:trigger:<id>).
	// Without this, max_concurrent only bounded fires within a single evalOne
	// batch — it never counted agents still running from a prior fire, nor
	// agents spawned by a SIBLING source of the same multi-entry trigger
	// (cron + bead_event firing near-simultaneously each ran their own
	// evalOne with fired reset to 0). That defeated single-threading:
	// gardener-release-owner-v3 routinely had 3-4 instances alive at once
	// (ka-0000003120). Counting live agents up-front closes the race for
	// every spawn-producing trigger, gardener or not.
	//
	// Fail-open: a daemon error counting live agents is logged and treated
	// as zero so a transient hiccup can't wedge a trigger forever (same
	// posture as the gardener one_spawn_at_a_time gate).
	fired := 0
	// spawned counts fires this engine actually performed THIS cycle, distinct
	// from `fired` which is seeded with the live-agent lease below — so the
	// outcome stamp can tell a real fire apart from a cycle where the
	// concurrency lease was already full and every match was deferred (bug
	// ka-0000001513: a gardener pinned dark by a wedged agent looks identical to
	// a healthy one on last_checked alone).
	spawned := 0
	deferred := false
	// fireErr holds the last fireAction error this cycle (bug ka-0000002991), so
	// the outcome stamp below can name the FAILURE cause rather than the vague
	// "all fire attempts failed" — the exact ambiguity two agents burned full
	// sessions on in ka-0000004529, mis-diagnosing a hard spawn error as a
	// silent decline.
	var fireErr error
	spawnsAgent := trig.Action == TriggerActionSpawnAgent || trig.Action == TriggerActionApplyFormula
	// effectiveCap bounds how many agents this trigger may hold live at once — the
	// number the fire loop below both seeds `fired` against AND stops at. It is NOT
	// always trig.MaxConcurrent (default 3):
	//
	//   - GARDENER trigger: the single-live-instance cap governs
	//     (gardenerMaxConcurrentRuns, default 1, overridable via
	//     max_concurrent_runs). Binding the loop to MaxConcurrent instead let a
	//     burst of >=2 matching events in ONE drain cycle spawn multiple concurrent
	//     gardeners — the gardenerPreFireCheck gate only samples the live count ONCE
	//     per cycle (before Evaluate, when an idle gardener reads 0 live), so the
	//     per-match loop must re-bound against the gardener cap or it defeats the
	//     documented cap of 1 via the in-cycle path (bug ka-0000003387). This is
	//     the loop-side analogue of the gardenerAtConcurrentRunsCap pre-fire gate.
	//
	//   - NON-GARDENER trigger with max_concurrent_runs set: honored verbatim, per
	//     the MaxConcurrentRuns doc contract ("honored verbatim for both shapes").
	//     Previously the field parsed but was dead for spawn_agent triggers — the
	//     generic loop always used MaxConcurrent (runner-up half of ka-0000003387).
	//
	//   - otherwise: the legacy MaxConcurrent live-lease behavior, unchanged.
	effectiveCap := trig.MaxConcurrent
	switch {
	case trig.Gardener != nil:
		effectiveCap = gardenerMaxConcurrentRuns(trig)
	case trig.MaxConcurrentRuns > 0:
		effectiveCap = trig.MaxConcurrentRuns
	}
	if spawnsAgent && effectiveCap > 0 {
		if alive := e.liveSpawnCount(ctx, trig); alive > 0 {
			fired = alive
			e.logger.Info("triggers: live concurrency lease seeded",
				"trigger", trig.ID, "alive", alive, "effective_cap", effectiveCap,
				"max_concurrent", trig.MaxConcurrent)
		}
	}
	for _, m := range newMatches {
		if fired >= effectiveCap {
			e.logger.Info("triggers: concurrency cap reached, deferring remaining",
				"trigger", trig.ID, "fired", fired, "remaining", len(newMatches)-fired,
				"effective_cap", effectiveCap, "max_concurrent", trig.MaxConcurrent)
			deferred = true
			break
		}

		agentID, err := e.fireAction(ctx, trig, m)
		if err != nil {
			e.logger.Error("triggers: action failed",
				"trigger", trig.ID, "match", m.Key, "action", trig.Action, "error", err)
			// Persist the fire error onto the trigger bead so a gardener (or any
			// trigger) that MATCHES EVERY CYCLE AND FAILS is no longer
			// byte-indistinguishable from an idle/gated one in the data plane
			// (bug ka-0000002991). fireAction spawns no agent on error, so
			// gardenerAgentTracker never sees a closed agent and never advances
			// consecutive_failures — meaning a hard_stop.consecutive_failures
			// self-block could never engage on a spawn that keeps 400ing. The
			// sibling last_outcome seam (ka-0000001513) does NOT cover this: it
			// records why a trigger DECLINED to fire; here the trigger did fire
			// and the action raised. Bump the failure streak for gardeners and
			// stamp last_fire_error / last_fire_error_at for everything.
			fireErr = err
			prevFireError := trig.LastFireError
			e.stampFireError(ctx, &trig, err)
			// A privileged-spawn 403 means the trigger's seal is missing or
			// stale: say so once, before the hard_stop quietly closes a
			// gardener (gd-c4Xz4i95F7).
			e.raiseSpawnDenied(ctx, trig, prevFireError, err)
			continue
		}

		e.mu.Lock()
		seenSet[m.Key] = true
		e.mu.Unlock()
		e.clearOrphanRefire(trig.ID, m.Key)
		fired++
		spawned++

		// Gardener post-fire bookkeeping: bump RunsSpawned and stamp
		// LastAgentID. Persists the updated Gardener JSON back to the
		// trigger bead so the run_budget / one_spawn_at_a_time gates
		// see fresh state on the next evaluation cycle.
		//
		// Non-gardener spawn-producing fires get the plain last_run /
		// last_agent_id stamp: historically ONLY gardenerPostFire wrote
		// last_run, so a plain apply_formula schedule trigger looked frozen
		// (stale last_run) while pouring on every cron window — the
		// looked-alive-but-stalled confusion in the FLAT incident
		// (ka-0000001622). Stamping here also gives schedule-driven pours
		// restart continuity: molecules carry no schedule_fire_key for
		// CatchUpKeys to recover, so last_run is the only persisted
		// suppression for an already-poured cron window.
		if trig.Gardener != nil {
			gardenerPostFire(ctx, e.daemon, e.logger, trig, agentID, m.Key)
		} else if spawnsAgent {
			e.stampLastRun(ctx, trig, agentID, m.Key)
		}
	}

	// A cleanly successful fire cycle clears any stale last_fire_error so a
	// resolved failure can never masquerade as a live one on `gb gardener status`
	// (bug ka-0000002991). Guarded on a non-empty prior value so the common
	// healthy path writes nothing and fires no SSE event, and on fireErr==nil so
	// a mixed cycle (some matches failed, some spawned) keeps the fresh error
	// stampFireError just recorded rather than flip-flopping it. consecutive_-
	// failures is deliberately NOT reset here — a successful FIRE is not yet a
	// successful RUN; the run-completion `done` path (gardenerAgentTracker) owns
	// that reset.
	if spawned > 0 && fireErr == nil && trig.LastFireError != "" {
		e.clearFireError(ctx, trig.ID)
	}

	var outcome string
	switch {
	case spawned > 0:
		outcome = fmt.Sprintf("fired: spawned %d", spawned)
	case deferred:
		// Every match this cycle was deferred because the live concurrency lease
		// was already full — the dark-by-wedged-agent case. Name it so a gardener
		// stuck behind a pinned lease reads a decline reason, not a stale fire.
		outcome = "deferred: max_concurrent lease full"
	case fireErr != nil:
		// Matches existed and every fireAction errored (already logged per match
		// AND persisted to last_fire_error). Fold the error into the outcome so
		// `kd show` / the SSE feed name the failure inline, not just a bare
		// "all fire attempts failed" that reads like a decline (bug
		// ka-0000002991).
		outcome = "no spawn: fire failed: " + truncateText(fireErr.Error(), maxFireErrorLen)
	default:
		// Matches existed but every fireAction errored (already logged per
		// match); record that the cycle produced no spawn despite matches.
		outcome = "no spawn: all fire attempts failed"
	}
	if err := e.stampCheckedOutcome(ctx, trig.ID, withOrphanNotes(outcome, orphanNotes)); err != nil {
		e.logger.Warn("triggers: failed to update last_checked",
			"trigger", trig.ID, "error", err)
	}
	e.logger.Info("triggers: evaluated, fired actions",
		"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
		"fired", fired, "new_matches", len(newMatches))
}
