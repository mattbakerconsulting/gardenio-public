// Gardener semantics layer for the bridge TriggerEngine (Phase 3b/B6).
//
// A trigger bead with a non-nil Gardener gets pre-fire gates (run_budget,
// one_spawn_at_a_time, stop spec) and post-fire bookkeeping (RunsSpawned,
// LastAgentID) layered on the unchanged source.Evaluate → action path. Stop
// evaluation reuses scheduler.EvaluateGardenerStopSpec.

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/wall"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
	"gardenio/controller/internal/scheduler/progress"
)

// gardenerSkipReason enumerates why a gardener trigger was skipped pre-fire,
// for logs and tests.
type gardenerSkipReason string

const (
	gardenerSkipNone      gardenerSkipReason = ""
	gardenerSkipRunBudget gardenerSkipReason = "run_budget_exhausted"
	gardenerSkipOneSpawn  gardenerSkipReason = "one_spawn_at_a_time"
	// gardenerSkipConcurrentRuns: the trigger already has its concurrent-run cap
	// (default 1) of live agents spawned_by:trigger:<id>; it defers to the next
	// tick (ka-0000003097).
	gardenerSkipConcurrentRuns gardenerSkipReason = "max_concurrent_runs"
	gardenerSkipStopMatched    gardenerSkipReason = "stop_matched"
	gardenerSkipStopParseError gardenerSkipReason = "stop_parse_error"
	// gardenerSkipDormant: parked behind an open operator blocker; stays open +
	// enabled but does not fire (ka-0000003082).
	gardenerSkipDormant gardenerSkipReason = "dormant_on_escalation"
	// gardenerSkipPaused: the operator-engaged gardeners-pause gate is open, halting
	// all gardener firing fleet-wide (ka-0000006744).
	gardenerSkipPaused gardenerSkipReason = "gardeners_paused"
	// gardenerSkipDarkMRFleetCap: fleet-wide open dark MRs are at
	// darkMaxConcurrentOpenDarkMRs; self-releasing (R6-FU6c, ka-0000002822).
	gardenerSkipDarkMRFleetCap gardenerSkipReason = "dark_mr_fleet_cap"
	// gardenerSkipDarkMRPerGardenerCap: this gardener produced
	// darkMaxMRsPerGardenerWindow dark MRs inside darkMRPerGardenerWindow;
	// self-releasing (R6-FU6c, ka-0000002822).
	gardenerSkipDarkMRPerGardenerCap gardenerSkipReason = "dark_mr_per_gardener_cap"
	// gardenerSkipDarkAgentBudget: a live dark agent this gardener spawned crossed
	// its per-agent/day token ceiling and was moved to budget_exceeded (R6-FU6c,
	// ka-0000001323).
	gardenerSkipDarkAgentBudget gardenerSkipReason = "dark_agent_budget_exceeded"
	// gardenerSkipCredCrashloop: the last credCrashloopThreshold spawns all closed
	// not_logged_in, so the gardener escalates instead of respawning (ka-0000006377,
	// ka-0000003924, ka-0000007394).
	gardenerSkipCredCrashloop gardenerSkipReason = "credential_crashloop"
	// gardenerSkipLandWall: the Lands spawn wall (internal/lands/wall, Track B
	// §3.5, step 8.3) refuses the gardener's tenant land; self-releasing.
	gardenerSkipLandWall gardenerSkipReason = "land_spawn_wall"
	// gardenerSkipDailyRunCap: MaxRunsPerDay runs spawned today (UTC);
	// self-releasing at the next UTC day.
	gardenerSkipDailyRunCap gardenerSkipReason = "max_runs_per_day"
)

// gardenerRunsDay is the UTC day a run counts toward for MaxRunsPerDay.
func gardenerRunsDay(now time.Time) string {
	return now.UTC().Format(time.DateOnly)
}

// GardenerDailyRunCapHit reports whether g has spawned its MaxRunsPerDay runs
// on now's UTC day. A gardener with no daily cap is never capped.
func GardenerDailyRunCapHit(g *GardenerSpec, now time.Time) bool {
	if g == nil || g.MaxRunsPerDay <= 0 {
		return false
	}
	return g.RunsDay == gardenerRunsDay(now) && g.RunsToday >= g.MaxRunsPerDay
}

// gardenerTenantSpawns reports the lands.tenantSpawns setting
// (LANDS_TENANT_SPAWNS, default off) for the gardener spawn wall. A var so
// tests can turn it on.
var gardenerTenantSpawns = wall.TenantSpawnsFromEnv

// gardenerDaemon is the subset of TriggerBeadClient the gardener evaluator
// needs, as an interface so the helpers test without TriggerEngine plumbing.
type gardenerDaemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	// UpdateBeadFieldsTyped patches fields with their JSON types. The run
	// counters are integer-typed on type:trigger; sent as strings kbeads rejects
	// the whole PATCH (ka-0000007976).
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	AddComment(ctx context.Context, beadID, author, text string) error
	// AddLabel stamps a real label (POST /v1/beads/{id}/labels). Never emulate it
	// with a "labels" key via UpdateBeadFields: the closed type:trigger schema
	// rejects it and the whole PATCH 400s (ka-0000007275).
	AddLabel(ctx context.Context, beadID, label string) error
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
	// UpdateBead patches mutable bead attributes (status, assignee, …), e.g. to
	// reopen a closed gardener (gardenerGrantSecondLife, ka-0000004010,
	// ka-0000006681).
	UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error
	// MRBeadForTask backs the progress evaluator's mr_merged kind (Phase
	// 3b/B7-c2), via the `produces` dep edge.
	MRBeadForTask(ctx context.Context, taskBeadID string) (*beadsapi.BeadDetail, error)
	conditions.BeadClient
}

// gardenerPreFireCheck runs the gardener pre-fire gates in the numbered order
// below; some only defer, others resume, escalate, park or close the gardener.
//
// Returns (skip, reason). skip=true means evalOne should not proceed to
// source.Evaluate; reason is the structured cause for logs and tests.
func gardenerPreFireCheck(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) (bool, gardenerSkipReason) {
	if logger == nil {
		logger = slog.Default()
	}
	if trig.Gardener == nil {
		return false, gardenerSkipNone
	}
	capSpec := trig.Gardener

	// Attribute gate writes to the trigger's operator: comment-create is
	// fail-closed on operator attribution server-side (ka-0000007275).
	ctx = ctxWithTriggerOperator(ctx, trig)

	// (0a) Global kill-switch: an open `gardeners-pause` gate halts all gardener
	// firing (ka-0000006744). Unbounded gardeners are not disabled here; see
	// warnUnboundedGardener in triggers.go.
	if gardenersPauseEngaged(ctx, daemon, logger) {
		logger.Warn("gardener pre-fire: gardeners-pause kill-switch engaged, suppressing fire",
			"trigger", trig.ID, "gate", gardenersPauseTitle)
		return true, gardenerSkipPaused
	}

	// (0a-ii) Lands spawn wall (step 8.3), checked for the role the fire spawns
	// (trig.Role), so a land's steward still fires with tenant spawns off
	// (gd-KOAoUELMWo). A failed land read fails open: the controller's spawn
	// admission still fails closed.
	if r, err := wall.Check(ctx, daemon, trig.Project, trig.Role, gardenerTenantSpawns(), time.Now()); err != nil {
		logger.Warn("gardener pre-fire: land read failed, spawn wall not applied (the controller admission still is)",
			"trigger", trig.ID, "project", trig.Project, "error", err)
	} else if r.Refused() {
		logger.Warn("gardener pre-fire: Lands spawn wall refuses the land, suppressing fire",
			"trigger", trig.ID, "project", trig.Project, "code", r.Code, "reason", r.Reason)
		return true, gardenerSkipLandWall
	}

	// (0b) Dark-only blast-radius caps (R6-FU6c): MR rate (dark_mr_cap.go,
	// ka-0000002822) and per-agent/day token spend (dark_agent_budget.go,
	// ka-0000001323), dimensions run_budget does not bound.
	if IsDarkGardener(trig.Labels, trig.Project) {
		// (0b-i) Fleet-wide open-dark-MR and per-gardener MR-rate caps. Fail
		// closed on a confirmed count, open on a read error; self-releasing.
		if d := evaluateDarkMRCaps(ctx, daemon, logger, trig); d.Skip {
			return true, d.Reason
		}

		// (0b-ii) Per-agent/day token budget (LiteLLM cost): an over-budget agent
		// goes to budget_exceeded and spawns are suppressed this cycle.
		if exceeded, agents := enforceDarkAgentDailyBudget(ctx, daemon, logger, trig, time.Now()); exceeded {
			logger.Warn("gardener pre-fire: dark per-agent/day token budget exceeded, suppressing spawn",
				"trigger", trig.ID, "agents", agents)
			return true, gardenerSkipDarkAgentBudget
		}
	}

	// (0) Dormant: parked behind a blocker chosen by DormantBlockerKind
	// (ka-0000003082, ka-0000007387). Stays open + enabled so cron can wake it;
	// once the blocker clears it wakes and falls through to the normal gates.
	if capSpec.DormantOnEscalation {
		if gardenerDormantStillBlocked(ctx, daemon, logger, trig) {
			return true, gardenerSkipDormant
		}
		gardenerWakeFromDormant(ctx, daemon, logger, trig)
	}

	// (1) run_budget: exhaustion is a premature stop, so it goes through
	// resume-or-escalate (ka-0000005353). Standing gardeners skip this gate:
	// run_budget is no bound for them and re-arming only churns comments
	// (ka-0000004754, ka-0000006241).
	if capSpec.RunBudget > 0 && capSpec.RunsSpawned >= capSpec.RunBudget && !gardenerIsStanding(trig) {
		gardenerResumeOrEscalate(ctx, daemon, logger, trig, scheduler.StopKindMaxRuns)
		return true, gardenerSkipRunBudget
	}

	// (2) one_spawn_at_a_time: block while LastAgentID is still running. Fail
	// open on a daemon error or missing bead so a stuck gardener can recover.
	if capSpec.OneSpawnAtATime && capSpec.LastAgentID != "" {
		// staleAfter 0 → 3h default; zero clock → time.Now.
		if gardenerAgentStillRunning(ctx, daemon, logger, capSpec.LastAgentID, trig.LeaseStaleAfter, time.Time{}) {
			return true, gardenerSkipOneSpawn
		}
	}

	// (2a) Single-live-instance default (ka-0000003097): see
	// gardenerSkipConcurrentRuns. Unlike one_spawn_at_a_time it needs no opt-in
	// and counts every live agent the trigger spawned, not just LastAgentID.
	if gardenerAtConcurrentRunsCap(ctx, daemon, logger, trig, time.Time{}) {
		return true, gardenerSkipConcurrentRuns
	}

	// (2a-ii) Runs-per-day cap (Lands Track B §4.3) for event-woken gardeners
	// such as a land's steward; defers to the next UTC day.
	if GardenerDailyRunCapHit(capSpec, time.Now()) {
		logger.Info("gardener pre-fire: runs-per-day cap reached, suppressing fire until the next UTC day",
			"trigger", trig.ID, "max_runs_per_day", capSpec.MaxRunsPerDay, "runs_today", capSpec.RunsToday)
		return true, gardenerSkipDailyRunCap
	}

	// (2b) Credential crashloop (ka-0000006377): see gardenerSkipCredCrashloop.
	// Before the stop gate so a gardener without a stop spec cannot mask it.
	if gardenerCredCrashlooping(capSpec) {
		gardenerEscalateCredCrashloop(ctx, daemon, logger, trig)
		return true, gardenerSkipCredCrashloop
	}

	// (2c) Proactive dormancy (ka-0000007387), ahead of the stop/resume
	// machinery so a gardener that never trips a stop still parks:
	//   (2c-i) declared {"kind":"mr_merged"} in stop.loop_end and a tracked MR
	//     under the parent is unmerged;
	//   (2c-ii) its whole open frontier is behind open operator blockers.
	// Both fail toward not parking.
	if !capSpec.DormantOnEscalation {
		if parent, active := gardenerMRMergeBlockActive(ctx, daemon, logger, trig); active {
			gardenerParkDormant(ctx, daemon, logger, trig, scheduler.StopKindMRMerged, parent, gardenerDormantKindMRMerged,
				fmt.Sprintf("gardener dormant: declared block-until-MR-merged and a tracked MR under %s is not yet merged — staying armed (open + enabled) instead of spawning. Auto-resumes when every tracked MR under %s merges (ka-0000007387).",
					parent, parent))
			return true, gardenerSkipDormant
		}
		if blocker, wedged := gardenerFrontierFullyWedged(ctx, daemon, logger, trig); wedged {
			gardenerParkDormant(ctx, daemon, logger, trig, gardenerDormantKindOperatorWedge, blocker, gardenerDormantKindOperatorWedge,
				fmt.Sprintf("gardener dormant: entire open frontier is single-threaded behind open operator blocker %s (zero actionable children) — staying armed (open + enabled) instead of spawning. Auto-resumes when the frontier is no longer fully wedged (ka-0000007387).",
					blocker))
			return true, gardenerSkipDormant
		}
	}

	// (3) Stop spec, hard_stop > loop_end, evaluated against gardener counters.
	if len(capSpec.Stop) > 0 {
		spec, err := scheduler.ParseStopSpec(string(capSpec.Stop))
		if err != nil {
			logger.Warn("gardener stop: parse error, treating as no stop",
				"trigger", trig.ID, "error", err)
			return false, gardenerSkipStopParseError
		}
		if spec.IsZero() {
			return false, gardenerSkipNone
		}
		sctx := scheduler.GardenerStopContext{
			BeadID:              trig.ID,
			Project:             trig.Project,
			RunBudget:           capSpec.RunBudget,
			RunsSpawned:         capSpec.RunsSpawned,
			ConsecutiveFailures: capSpec.ConsecutiveFailures,
			ProgressState:       string(capSpec.ProgressState),
			// CreatedAt is the wall_clock baseline (ka-0000006967), floored by
			// gardenerWallClockBaseline's grandfather epoch.
			CreatedAt: trig.CreatedAt,
			// WallClockSince is stamped on revive so wall_clock measures from
			// max(WallClockSince, CreatedAt, epoch) (ka-0000006465).
			WallClockSince: capSpec.WallClockSince,
			// IsStanding exempts a standing gardener from max_runs, mirroring
			// gate 1 (ka-0000004754); hard_stop still applies.
			IsStanding: gardenerIsStanding(trig),
		}
		match, fired := scheduler.EvaluateGardenerStopSpec(ctx, gardenerDaemonStopAdapter{d: daemon}, logger, sctx, spec)
		if fired {
			// A premature loop_end must not abandon an unmet terminal_goal:
			// resume-or-escalate instead of closing (ka-0000005353).
			if match.Slot == scheduler.StopSlotLoopEnd && match.Premature {
				gardenerResumeOrEscalate(ctx, daemon, logger, trig, match.Kind)
				return true, gardenerSkipStopMatched
			}
			// A standing gardener's wall_clock hard_stop is a lifecycle ceiling,
			// not a failure: renew it in place (ka-0000004573).
			if match.Slot == scheduler.StopSlotHardStop &&
				match.Kind == scheduler.StopKindWallClock &&
				gardenerIsStanding(trig) {
				gardenerRenewStandingWallClock(ctx, daemon, logger, trig)
				return true, gardenerSkipStopMatched
			}
			gardenerTerminate(ctx, daemon, logger, trig, match.Slot, match.Kind)
			return true, gardenerSkipStopMatched
		}
	}

	return false, gardenerSkipNone
}

// Resume defaults (bead ka-0000005353). A gardener with no explicit
// ResumeBudget gets defaultResumeBudget fruitless resumes before escalation;
// each re-arm adds defaultRunBudgetRefill runs of headroom.
const (
	defaultResumeBudget    = 5
	defaultRunBudgetRefill = 10
	// gardenerStuckEscalateLabel marks a gardener that spent its resume budget
	// with terminal_goal unmet. A plan gardener keeps it while dormant on plan
	// activity (gd-dNxyjFLGRJ); beads-viewer shows it as stuck.
	gardenerStuckEscalateLabel = "gardener-stuck-escalate"
)

// gardenerResumeOrEscalate handles a premature stop (out of runs/time/patience,
// not goal done). While ResumesWithoutProgress < the resume budget it re-arms:
// refills RunBudget, resets ConsecutiveFailures, counts the resume, leaves the
// bead open. Once spent, a gardener driving an open plan epic goes dormant until
// plan activity (gardenerParkOnPlanActivity, gd-dNxyjFLGRJ); one with no plan
// escalates: stuck label, comment, close. A negative ResumeBudget disables
// resume (premature stops close).
//
// Sticky conditions (no_progress_for, wall_clock) re-match every reconcile
// tick, so a resume is counted only when RunsSpawned has advanced past
// LastResumeRunsSpawned (ka-0000002690). Escalation is not gated on a new run.
//
// A plan that moved since the last counted resume (gardenerPlanProgressSinceResume)
// is progress: the gardener re-arms with ResumesWithoutProgress reset to 0, even
// with the budget spent or no new run, because the moved plan may be new ready
// work, such as a dead crew's step the stop gate reopened (gd-WE8g4FFEHm).
func gardenerResumeOrEscalate(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, kind string) {
	// Operator attribution for comment-create (ka-0000007275).
	ctx = ctxWithTriggerOperator(ctx, trig)
	if trig.Gardener == nil {
		// No block to carry resume state: close rather than loop unbounded.
		gardenerTerminate(ctx, daemon, logger, trig, scheduler.StopSlotLoopEnd, kind)
		return
	}
	cap := trig.Gardener

	budget := cap.ResumeBudget
	if budget < 0 {
		gardenerTerminate(ctx, daemon, logger, trig, scheduler.StopSlotLoopEnd, kind)
		return
	}
	if budget == 0 {
		budget = defaultResumeBudget
	}

	spent := cap.ResumesWithoutProgress >= budget
	bare := cap.RunsSpawned <= cap.LastResumeRunsSpawned && cap.LastResumeReason != ""
	planDigest, planMoved := cap.PlanProgressDigest, false
	if spent || !bare {
		planDigest, planMoved = gardenerPlanProgressSinceResume(ctx, daemon, logger, trig)
	}

	if spent && !planMoved {
		// A standing gardener (no terminal epic) scoring zero in a quiet
		// period is not stuck: re-arm in place instead of escalating
		// (ka-0000006241, ka-0000000928).
		if gardenerIsStanding(trig) {
			gardenerRearmStandingInPlace(ctx, daemon, logger, trig, kind, budget)
			return
		}
		// If the whole frontier is wedged behind open operator blockers, go
		// dormant rather than close, so it wakes when the blocker clears
		// (ka-0000003082, gd-FUC635HJ2E).
		if blocker, wedged := gardenerFrontierFullyWedged(ctx, daemon, logger, trig); wedged {
			gardenerGoDormant(ctx, daemon, logger, trig, kind, blocker)
			return
		}
		// A plan gardener never closes on a spent budget: closing silently
		// orphaned the plan (gd-dNxyjFLGRJ). It parks until the plan moves.
		if gardenerParkOnPlanActivity(ctx, daemon, logger, trig, kind) {
			return
		}
		// Escalate (ka-0000000974). The label is the watchdog's only handle,
		// so it lands before the close; on failure stay open and retry next
		// tick (ka-0000007275, ka-0000001510).
		if err := gardenerAddLabel(ctx, daemon, logger, trig.ID, gardenerStuckEscalateLabel); err != nil {
			logger.Warn("gardener escalate: label write failed — NOT closing; leaving open to retry escalate next reconcile",
				"trigger", trig.ID, "kind", kind, "error", err)
			return
		}
		reason := fmt.Sprintf(
			"gardener stuck: premature stop %q fired %d× with no progress and terminal_goal unmet, and it has no open plan to wait on; closing with the gardener-stuck-escalate label for the operator",
			kind, cap.ResumesWithoutProgress)
		if err := daemon.AddComment(ctx, trig.ID, "trigger-engine", reason); err != nil {
			logger.Warn("gardener escalate: AddComment failed", "trigger", trig.ID, "error", err)
		}
		// gardener_stuck_escalated=true is the watchdog's fallback handle.
		updated := *cap
		updated.LastResumeReason = kind
		updated.LastResumeRunsSpawned = cap.RunsSpawned
		if raw, err := json.Marshal(updated); err == nil {
			fields := map[string]string{
				"gardener":                 string(raw),
				"gardener_terminated_slot": scheduler.StopSlotLoopEnd,
				"gardener_terminated_kind": kind,
				"gardener_stuck_escalated": "true",
			}
			if err := daemon.CloseBead(ctx, trig.ID, fields); err != nil {
				logger.Warn("gardener escalate: CloseBead failed", "trigger", trig.ID, "error", err)
			}
			// Disarm so it presents closed+disabled (ka-0000001423); enabled is
			// boolean-typed, hence a separate write.
			gardenerDisarmOnClose(ctx, daemon, logger, trig.ID, scheduler.StopSlotLoopEnd, kind)
		}
		recordGardenerResume(trig.ID, resumeActionEscalate, kind)
		setGardenerResumesWithoutProgress(trig.ID, cap.ResumesWithoutProgress)
		logger.Info("gardener escalated (resume budget exhausted)",
			"trigger", trig.ID, "kind", kind, "resumes", cap.ResumesWithoutProgress, "budget", budget)
		return
	}

	// Bare-reconcile no-op (ka-0000002690): no new run since the last counted
	// resume, so don't count, comment or reset; only top up RunBudget if it fell
	// below the refill floor. A first-ever premature stop (LastResumeReason=="")
	// falls through to the counting re-arm.
	if bare && !planMoved {
		refill := cap.RunBudgetRefill
		if refill <= 0 {
			refill = defaultRunBudgetRefill
		}
		if want := cap.RunsSpawned + refill; cap.RunBudget < want {
			updated := *cap
			updated.RunBudget = want
			if raw, mErr := json.Marshal(updated); mErr == nil {
				if err := daemon.UpdateBeadFields(ctx, trig.ID, map[string]string{"gardener": string(raw)}); err != nil {
					logger.Warn("gardener resume: bare-reconcile budget top-up UpdateBeadFields failed",
						"trigger", trig.ID, "error", err)
				}
				trig.Gardener.RunBudget = updated.RunBudget
			}
		}
		logger.Info("gardener resume: bare reconcile (no new run since last resume) — not counting fruitless resume",
			"trigger", trig.ID, "kind", kind, "resume", cap.ResumesWithoutProgress, "budget", budget,
			"runs_spawned", cap.RunsSpawned, "last_resume_runs_spawned", cap.LastResumeRunsSpawned)
		return
	}

	// Re-arm: refill run budget, reset the failure streak, count the resume.
	refill := cap.RunBudgetRefill
	if refill <= 0 {
		refill = defaultRunBudgetRefill
	}
	updated := *cap
	updated.RunBudget = cap.RunsSpawned + refill
	updated.ConsecutiveFailures = 0
	updated.ResumesWithoutProgress = cap.ResumesWithoutProgress + 1
	if planMoved {
		updated.ResumesWithoutProgress = 0
	}
	updated.PlanProgressDigest = planDigest
	updated.LastResumeReason = kind
	updated.LastResumeRunsSpawned = cap.RunsSpawned

	// A no_progress_for re-arm clears the no-progress window, or the next
	// reconcile re-matches it. RunsFinalizedAfter makes the reset durable:
	// gardenerFinalizeRunsFromIterations would otherwise rebuild the same
	// zero-score tail from iteration history (ka-0000003265).
	if kind == scheduler.StopKindNoProgressFor {
		updated.ProgressState = nil
		updated.RunsFinalizedAfter = time.Now()
	}

	raw, err := json.Marshal(updated)
	if err != nil {
		logger.Warn("gardener resume: marshal failed", "trigger", trig.ID, "error", err)
		gardenerTerminate(ctx, daemon, logger, trig, scheduler.StopSlotLoopEnd, kind)
		return
	}
	comment := fmt.Sprintf(
		"gardener resumed: premature stop %q, terminal_goal unmet — re-armed (run_budget %d→%d, resume %d/%d). Goal not declared done; continuing.",
		kind, cap.RunBudget, updated.RunBudget, updated.ResumesWithoutProgress, budget)
	if planMoved {
		comment = fmt.Sprintf(
			"gardener resumed: premature stop %q, and the plan moved since the last resume (a step closed, started, returned to ready or was added, or a step's PR merged). That is progress, so the resume does not count (resume 0/%d, run_budget %d→%d; gd-WE8g4FFEHm).",
			kind, budget, cap.RunBudget, updated.RunBudget)
	}
	if err := daemon.AddComment(ctx, trig.ID, "trigger-engine", comment); err != nil {
		logger.Warn("gardener resume: AddComment failed", "trigger", trig.ID, "error", err)
	}
	if err := daemon.UpdateBeadFields(ctx, trig.ID, map[string]string{"gardener": string(raw)}); err != nil {
		logger.Warn("gardener resume: UpdateBeadFields failed", "trigger", trig.ID, "error", err)
	}
	recordGardenerResume(trig.ID, resumeActionRearm, kind)
	setGardenerResumesWithoutProgress(trig.ID, updated.ResumesWithoutProgress)
	// Later gates this cycle see the re-armed state.
	trig.Gardener.RunBudget = updated.RunBudget
	trig.Gardener.ConsecutiveFailures = updated.ConsecutiveFailures
	trig.Gardener.ResumesWithoutProgress = updated.ResumesWithoutProgress
	trig.Gardener.LastResumeReason = updated.LastResumeReason
	trig.Gardener.LastResumeRunsSpawned = updated.LastResumeRunsSpawned
	trig.Gardener.ProgressState = updated.ProgressState
	trig.Gardener.PlanProgressDigest = updated.PlanProgressDigest
	logger.Info("gardener resumed (premature stop, goal unmet)",
		"trigger", trig.ID, "kind", kind, "resume", updated.ResumesWithoutProgress, "budget", budget,
		"run_budget", updated.RunBudget, "plan_moved", planMoved)
}

// gardenerPlanProgressSinceResume fingerprints the open plan a gardener drives
// (gardenerPlanProgressDigest) and reports whether it changed since the
// fingerprint stamped at the last counted resume (gd-WE8g4FFEHm). A standing
// gardener, one with no open plan, or a failed read keeps the stamped
// fingerprint and reports false; the first fingerprint is a baseline, never
// progress.
func gardenerPlanProgressSinceResume(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) (digest string, moved bool) {
	if trig.Gardener == nil {
		return "", false
	}
	prev := trig.Gardener.PlanProgressDigest
	if gardenerIsStanding(trig) {
		return prev, false
	}
	epic := gardenerFrontierEpic(ctx, daemon, logger, trig)
	if epic == "" || epic == trig.ID {
		return prev, false
	}
	if detail, err := daemon.GetBead(ctx, epic); err != nil || detail == nil || detail.Status == "closed" {
		return prev, false
	}
	digest, err := gardenerPlanProgressDigest(ctx, daemon, epic)
	if err != nil {
		logger.Warn("gardener resume: plan progress read failed, counting the resume as without progress",
			"trigger", trig.ID, "epic", epic, "error", err)
		return prev, false
	}
	return digest, prev != "" && prev != digest
}

// gardenerRearmStandingInPlace re-arms a standing gardener whose resume budget
// is spent instead of escalating (ka-0000006241). Like the counting re-arm in
// gardenerResumeOrEscalate, except ResumesWithoutProgress resets to 0, so it
// never reaches the give-up threshold; only hard_stop closes it.
//
// Best-effort and never closes: a failed write is logged and retried next tick.
func gardenerRearmStandingInPlace(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, kind string, budget int) {
	cap := trig.Gardener
	if cap == nil {
		logger.Warn("gardener standing re-arm: no gardener block — skipping (leaving open)",
			"trigger", trig.ID, "kind", kind)
		return
	}

	refill := cap.RunBudgetRefill
	if refill <= 0 {
		refill = defaultRunBudgetRefill
	}
	updated := *cap
	updated.RunBudget = cap.RunsSpawned + refill
	updated.ConsecutiveFailures = 0
	updated.ResumesWithoutProgress = 0
	updated.LastResumeReason = kind
	updated.LastResumeRunsSpawned = cap.RunsSpawned

	// Durable no-progress window reset, as in gardenerResumeOrEscalate
	// (ka-0000003265).
	if kind == scheduler.StopKindNoProgressFor {
		updated.ProgressState = nil
		updated.RunsFinalizedAfter = time.Now()
	}

	raw, err := json.Marshal(updated)
	if err != nil {
		logger.Warn("gardener standing re-arm: marshal failed — leaving gardener open to retry next reconcile",
			"trigger", trig.ID, "kind", kind, "error", err)
		return
	}
	comment := fmt.Sprintf(
		"standing gardener re-armed in place: premature stop %q spent the resume budget (%d/%d) with no progress, but this is a PERPETUAL gardener (standing terminal_goal, no terminal epic) — a quiet period with nothing to do is normal, not stuck. Re-armed (run_budget→%d, fruitless-resume counter reset 0) instead of escalating; terminal hard_stop brakes (consecutive_failures/wall_clock) still apply (bug ka-0000006241).",
		kind, cap.ResumesWithoutProgress, budget, updated.RunBudget)
	if err := daemon.AddComment(ctx, trig.ID, "trigger-engine", comment); err != nil {
		logger.Warn("gardener standing re-arm: AddComment failed", "trigger", trig.ID, "error", err)
	}
	if err := daemon.UpdateBeadFields(ctx, trig.ID, map[string]string{"gardener": string(raw)}); err != nil {
		logger.Warn("gardener standing re-arm: UpdateBeadFields failed", "trigger", trig.ID, "error", err)
	}
	recordGardenerResume(trig.ID, resumeActionRearm, kind)
	setGardenerResumesWithoutProgress(trig.ID, 0)
	// Later gates this cycle see the re-armed state.
	trig.Gardener.RunBudget = updated.RunBudget
	trig.Gardener.ConsecutiveFailures = updated.ConsecutiveFailures
	trig.Gardener.ResumesWithoutProgress = updated.ResumesWithoutProgress
	trig.Gardener.LastResumeReason = updated.LastResumeReason
	trig.Gardener.LastResumeRunsSpawned = updated.LastResumeRunsSpawned
	trig.Gardener.ProgressState = updated.ProgressState
	logger.Info("standing gardener re-armed in place (perpetual — no give-up close)",
		"trigger", trig.ID, "kind", kind, "budget", budget, "run_budget", updated.RunBudget)
}

// gardenerRenewStandingWallClock renews a standing gardener's wall_clock
// hard_stop window in place instead of closing it (ka-0000004573,
// ka-0000006579): for a perpetual gardener a 720h age-out is a lifecycle
// ceiling, not a failure, and a close has no successor or alert.
//
// It sets WallClockSince=now (as the revive paths do, ka-0000006465), refills
// RunBudget and comments (mirrored to Slack, ka-0000000409). It never closes or
// disarms; the consecutive_failures hard_stop still closes. Best-effort: a
// failed write is logged and retried next tick.
func gardenerRenewStandingWallClock(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) {
	// Operator attribution for comment-create (ka-0000007275).
	ctx = ctxWithTriggerOperator(ctx, trig)
	cap := trig.Gardener
	if cap == nil {
		logger.Warn("gardener standing wall_clock renew: no gardener block — skipping (leaving open)",
			"trigger", trig.ID)
		return
	}

	refill := cap.RunBudgetRefill
	if refill <= 0 {
		refill = defaultRunBudgetRefill
	}
	updated := *cap
	updated.WallClockSince = time.Now()
	// It is also far past its run-count ceiling; refill so max_runs doesn't trip.
	if want := cap.RunsSpawned + refill; updated.RunBudget < want {
		updated.RunBudget = want
	}
	// A fresh window starts with clean streaks, as on revive.
	updated.ConsecutiveFailures = 0
	updated.ResumesWithoutProgress = 0

	raw, err := json.Marshal(updated)
	if err != nil {
		logger.Warn("gardener standing wall_clock renew: marshal failed — leaving gardener open to retry next reconcile",
			"trigger", trig.ID, "error", err)
		return
	}
	comment := fmt.Sprintf(
		"standing gardener wall_clock RENEWED in place: hit its DESIGNED %s hard_stop ceiling, but this is a PERPETUAL gardener (standing terminal_goal, no terminal epic) — a designed age-out is lifecycle hygiene, not a break. Renewed the wall_clock window (fresh baseline, run_budget→%d) instead of closing; the lane stays open + enabled so it never goes dark on the ceiling (bug ka-0000004573). consecutive_failures hard_stop still closes a genuinely broken standing gardener.",
		GardenerMaxWallClock, updated.RunBudget)
	if err := daemon.AddComment(ctx, trig.ID, "trigger-engine", comment); err != nil {
		logger.Warn("gardener standing wall_clock renew: AddComment failed", "trigger", trig.ID, "error", err)
	}
	if err := daemon.UpdateBeadFields(ctx, trig.ID, map[string]string{"gardener": string(raw)}); err != nil {
		logger.Warn("gardener standing wall_clock renew: UpdateBeadFields failed", "trigger", trig.ID, "error", err)
	}
	recordGardenerResume(trig.ID, resumeActionRenew, scheduler.StopKindWallClock)
	// Later gates this cycle see the renewed state.
	trig.Gardener.WallClockSince = updated.WallClockSince
	trig.Gardener.RunBudget = updated.RunBudget
	trig.Gardener.ConsecutiveFailures = updated.ConsecutiveFailures
	trig.Gardener.ResumesWithoutProgress = updated.ResumesWithoutProgress
	logger.Info("standing gardener wall_clock renewed in place (perpetual — no age-out close)",
		"trigger", trig.ID, "run_budget", updated.RunBudget)
}

// gardenerFrontierEpic resolves the epic a gardener drives: the AssignBead field
// for legacy gardeners, else the gardener-epic:<id> label on the trigger bead.
// Returns "" when the gardener is not pinned to an epic (no frontier to inspect)
// or the trigger bead could not be read.
func gardenerFrontierEpic(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) string {
	epic, err := gardenerFrontierEpicErr(ctx, daemon, trig)
	if err != nil {
		logger.Warn("gardener frontier: GetBead failed", "trigger", trig.ID, "error", err)
	}
	return epic
}

// gardenerFrontierEpicErr is gardenerFrontierEpic for a caller that must tell
// "no epic" from "could not read the trigger bead".
func gardenerFrontierEpicErr(ctx context.Context, daemon gardenerDaemon, trig TriggerDef) (string, error) {
	if trig.Gardener != nil && strings.TrimSpace(trig.Gardener.AssignBead) != "" {
		return strings.TrimSpace(trig.Gardener.AssignBead), nil
	}
	detail, err := daemon.GetBead(ctx, trig.ID)
	if err != nil {
		return "", err
	}
	if detail == nil {
		return "", nil
	}
	for _, l := range detail.Labels {
		if rest, ok := strings.CutPrefix(l, "gardener-epic:"); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", nil
}

// gardenerFrontierBlocker returns the ID of an open escalation/decision under
// the gardener's open epic, or "" if none (ka-0000003082). A closed epic means
// done, not wedged. Any daemon error returns "" so a flaky read cannot trap a
// gardener in dormancy.
func gardenerFrontierBlocker(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) string {
	epic := gardenerFrontierEpic(ctx, daemon, logger, trig)
	if epic == "" {
		return ""
	}
	if epicDetail, err := daemon.GetBead(ctx, epic); err != nil || epicDetail == nil {
		return ""
	} else if epicDetail.Status == "closed" {
		return ""
	}
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent:   epic,
		Types:    []string{"escalation", "decision"},
		Statuses: []string{"open", "in_progress"},
		Limit:    64,
	})
	if err != nil {
		logger.Warn("gardener frontier blocker: ListBeadsFiltered failed",
			"trigger", trig.ID, "epic", epic, "error", err)
		return ""
	}
	if res == nil {
		return ""
	}
	for _, b := range res.Beads {
		if b == nil || b.ID == "" {
			continue
		}
		return b.ID
	}
	return ""
}

// gardenerGoDormant parks a resume-exhausted gardener whose frontier is fully
// wedged, instead of closing it (ka-0000003082). It leaves DormantBlockerKind
// empty, which wakes on the operator_wedge predicate (gd-FUC635HJ2E); the
// proactive paths (ka-0000007387) call gardenerParkDormant with a kind.
func gardenerGoDormant(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, kind, blocker string) {
	gardenerParkDormant(ctx, daemon, logger, trig, kind, blocker, "",
		fmt.Sprintf(
			"gardener dormant: premature stop %q fired with no progress, but frontier is wedged behind open operator blocker %s — staying armed (open + enabled) instead of closing. Auto-resumes when %s clears (bug ka-0000003082).",
			kind, blocker, blocker))
}

// gardenerParkOnPlanActivity parks a resume-spent gardener that drives an open
// plan epic as dormant kind resume_budget_spent, stamping the plan's activity
// digest so the next change under the plan wakes it (gardenerPlanStillQuiet)
// with a fresh run budget. It keeps the stuck label for visibility and never
// closes. It returns false, leaving the caller to escalate-close, only when
// there is positively no open plan to watch (no epic bound, or the epic read
// back closed or empty). Any failed read returns true with nothing written, so
// the next tick retries rather than closing the plan's gardener.
func gardenerParkOnPlanActivity(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, kind string) bool {
	epic, err := gardenerFrontierEpicErr(ctx, daemon, trig)
	if err != nil {
		logger.Warn("gardener dormant: trigger read failed — NOT closing; retrying next reconcile",
			"trigger", trig.ID, "error", err)
		return true
	}
	if epic == "" || epic == trig.ID {
		return false
	}
	detail, err := daemon.GetBead(ctx, epic)
	if err != nil {
		logger.Warn("gardener dormant: plan read failed — NOT closing; retrying next reconcile",
			"trigger", trig.ID, "epic", epic, "error", err)
		return true
	}
	if detail == nil || detail.Status == "closed" {
		return false
	}
	digest, err := gardenerPlanActivityDigest(ctx, daemon, epic)
	if err != nil {
		logger.Warn("gardener dormant: plan subtree read failed — NOT closing; retrying next reconcile",
			"trigger", trig.ID, "epic", epic, "error", err)
		return true
	}
	if err := gardenerAddLabel(ctx, daemon, logger, trig.ID, gardenerStuckEscalateLabel); err != nil {
		logger.Warn("gardener dormant: stuck label write failed, parking anyway",
			"trigger", trig.ID, "error", err)
	}
	trig.Gardener.DormantActivityDigest = digest
	trig.Gardener.LastResumeRunsSpawned = trig.Gardener.RunsSpawned
	gardenerParkDormant(ctx, daemon, logger, trig, kind, epic, gardenerDormantKindResumeBudgetSpent,
		fmt.Sprintf(
			"gardener stuck: premature stop %q fired %d× with no progress and terminal_goal unmet. Dormant, not closed: it spawns nothing until plan %s changes (a step closes, reopens, starts, returns to ready or is added), then wakes with a fresh run budget (gd-dNxyjFLGRJ).",
			kind, trig.Gardener.ResumesWithoutProgress, epic))
	return true
}

// gardenerParkDormant stamps DormantOnEscalation, DormantBlocker and
// DormantBlockerKind, comments on the trigger and its epic, and counts the park,
// without closing the bead. blockerKind selects the wake predicate (see
// DormantBlockerKind on GardenerSpec, ka-0000007387).
func gardenerParkDormant(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, kind, blocker, blockerKind, comment string) {
	cap := trig.Gardener
	if cap == nil {
		// Nothing to persist dormancy onto: close rather than spin unbounded.
		gardenerTerminate(ctx, daemon, logger, trig, scheduler.StopSlotLoopEnd, kind)
		return
	}
	updated := *cap
	updated.DormantOnEscalation = true
	updated.DormantBlocker = blocker
	updated.DormantBlockerKind = blockerKind
	updated.DormantSince = time.Now()
	updated.LastResumeReason = kind

	if err := daemon.AddComment(ctx, trig.ID, "trigger-engine", comment); err != nil {
		logger.Warn("gardener dormant: AddComment failed", "trigger", trig.ID, "error", err)
	}
	// Operators read the epic, not the trigger bead (gd-FUC635HJ2E).
	if epic := gardenerFrontierEpic(ctx, daemon, logger, trig); epic != "" && epic != trig.ID {
		epicComment := fmt.Sprintf("gardener %s parked behind %s and will not spawn until it clears. %s", trig.ID, blocker, comment)
		if blockerKind == gardenerDormantKindResumeBudgetSpent {
			epicComment = fmt.Sprintf("gardener %s is dormant and will not spawn until a step under this plan changes. %s", trig.ID, comment)
		}
		if err := daemon.AddComment(ctx, epic, "trigger-engine", epicComment); err != nil {
			logger.Warn("gardener dormant: epic AddComment failed", "trigger", trig.ID, "epic", epic, "error", err)
		}
	}
	if raw, err := json.Marshal(updated); err == nil {
		if err := daemon.UpdateBeadFields(ctx, trig.ID, map[string]string{"gardener": string(raw)}); err != nil {
			logger.Warn("gardener dormant: UpdateBeadFields failed", "trigger", trig.ID, "error", err)
		}
	} else {
		logger.Warn("gardener dormant: marshal failed", "trigger", trig.ID, "error", err)
	}
	recordGardenerResume(trig.ID, resumeActionDormant, kind)
	trig.Gardener.DormantOnEscalation = true
	trig.Gardener.DormantBlocker = blocker
	trig.Gardener.DormantBlockerKind = blockerKind
	trig.Gardener.DormantSince = updated.DormantSince
	logger.Info("gardener dormant (staying armed)",
		"trigger", trig.ID, "kind", kind, "blocker", blocker, "blocker_kind", blockerKind)
}

// gardenerWakeFromDormant clears the dormant flags, ProgressState and
// ResumesWithoutProgress so zero-score runs accrued while wedged don't re-trip
// no_progress_for. RunsFinalizedAfter makes the reset durable (ka-0000003265).
// A resume_budget_spent wake also refills RunBudget and clears the failure
// streak, since that park was reached by spending both (gd-dNxyjFLGRJ).
func gardenerWakeFromDormant(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) {
	cap := trig.Gardener
	if cap == nil {
		return
	}
	blocker := cap.DormantBlocker
	blockerKind := cap.DormantBlockerKind
	updated := *cap
	updated.DormantOnEscalation = false
	updated.DormantBlocker = ""
	updated.DormantBlockerKind = ""
	updated.DormantActivityDigest = ""
	updated.DormantSince = time.Time{}
	updated.ProgressState = nil
	updated.RunsFinalizedAfter = time.Now()
	updated.ResumesWithoutProgress = 0

	comment := fmt.Sprintf(
		"gardener resumed from dormant: blocker %s (%s) cleared — re-armed with a fresh no-progress window (bug ka-0000003082 / ka-0000007387).",
		blocker, gardenerDormantBlockerKindLabel(blockerKind))
	if blockerKind == gardenerDormantKindResumeBudgetSpent {
		refill := cap.RunBudgetRefill
		if refill <= 0 {
			refill = defaultRunBudgetRefill
		}
		if want := cap.RunsSpawned + refill; updated.RunBudget < want {
			updated.RunBudget = want
		}
		updated.ConsecutiveFailures = 0
		updated.LastResumeRunsSpawned = cap.RunsSpawned
		comment = fmt.Sprintf(
			"gardener woke from dormant: plan %s changed — re-armed with a fresh run budget (run_budget %d→%d) and resume budget (gd-dNxyjFLGRJ).",
			blocker, cap.RunBudget, updated.RunBudget)
	}
	if err := daemon.AddComment(ctx, trig.ID, "trigger-engine", comment); err != nil {
		logger.Warn("gardener wake: AddComment failed", "trigger", trig.ID, "error", err)
	}
	if raw, err := json.Marshal(updated); err == nil {
		if err := daemon.UpdateBeadFields(ctx, trig.ID, map[string]string{"gardener": string(raw)}); err != nil {
			logger.Warn("gardener wake: UpdateBeadFields failed", "trigger", trig.ID, "error", err)
		}
	} else {
		logger.Warn("gardener wake: marshal failed", "trigger", trig.ID, "error", err)
	}
	setGardenerResumesWithoutProgress(trig.ID, 0)
	// Later gates this cycle see the woken state.
	trig.Gardener.DormantOnEscalation = false
	trig.Gardener.DormantBlocker = ""
	trig.Gardener.DormantBlockerKind = ""
	trig.Gardener.DormantActivityDigest = ""
	trig.Gardener.DormantSince = time.Time{}
	trig.Gardener.ProgressState = nil
	trig.Gardener.RunsFinalizedAfter = updated.RunsFinalizedAfter
	trig.Gardener.ResumesWithoutProgress = 0
	trig.Gardener.RunBudget = updated.RunBudget
	trig.Gardener.ConsecutiveFailures = updated.ConsecutiveFailures
	trig.Gardener.LastResumeRunsSpawned = updated.LastResumeRunsSpawned
	logger.Info("gardener resumed from dormant (blocker cleared)",
		"trigger", trig.ID, "blocker", blocker, "blocker_kind", blockerKind)
}

// gardenerDormantBlockerKindLabel renders a human-readable label for a dormancy
// kind in comments/logs. Empty kind is the legacy operator-escalation dormancy.
func gardenerDormantBlockerKindLabel(kind string) string {
	switch kind {
	case gardenerDormantKindMRMerged:
		return "block-until-MR-merged"
	case gardenerDormantKindOperatorWedge:
		return "operator-wedged frontier"
	case gardenerDormantKindResumeBudgetSpent:
		return "resume budget spent, waiting for plan activity"
	default:
		return "operator escalation/decision"
	}
}

// gardenerAddLabel adds a label via the daemon's label endpoint, which emits the
// beads.label.added event the resumer watchdog wakes on; a no-op if present.
// Returns the error so escalate callers can skip the close when the label did
// not land (ka-0000007275). See AddLabel on gardenerDaemon.
func gardenerAddLabel(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, beadID, label string) error {
	if detail, err := daemon.GetBead(ctx, beadID); err == nil && detail != nil &&
		slices.Contains(detail.Labels, label) {
		return nil // already present
	}
	if err := daemon.AddLabel(ctx, beadID, label); err != nil {
		logger.Warn("gardener label: AddLabel failed", "bead", beadID, "label", label, "error", err)
		return err
	}
	return nil
}

// credCrashloopThreshold is how many consecutive not_logged_in closes trip
// gardenerCredCrashlooping. It mirrors the per-pod LoginRequiredWatchdog
// threshold so a brief credential-provisioning lag is tolerated
// (ka-0000003924, ka-0000007394).
const credCrashloopThreshold = 3

// credCrashloopCloseReason is the close_reason the per-pod watchdog stamps on a
// credential-starved agent (ka-0000003924).
const credCrashloopCloseReason = "not_logged_in"

// gardenerCredCrashlooping reports whether the last credCrashloopThreshold
// run_log entries all closed with credCrashloopCloseReason. Fewer recorded runs
// is never a crashloop. The run_log is filled by gardenerAgentTracker earlier
// in the same evalOne cycle.
func gardenerCredCrashlooping(capSpec *GardenerSpec) bool {
	if capSpec == nil || len(capSpec.RunLog) == 0 {
		return false
	}
	runLog, err := scheduler.ParseRunLog(string(capSpec.RunLog))
	if err != nil || len(runLog) < credCrashloopThreshold {
		return false
	}
	for _, entry := range runLog[len(runLog)-credCrashloopThreshold:] {
		if entry.CloseReason != credCrashloopCloseReason {
			return false
		}
	}
	return true
}

// gardenerEscalateCredCrashloop escalates like gardenerResumeOrEscalate
// (label, comment, close, disarm). It never re-arms: the credential is broken
// (ka-0000007394). Failures are logged, not propagated.
func gardenerEscalateCredCrashloop(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) {
	// Label before close; on failure stay open and retry next tick
	// (ka-0000007275).
	if err := gardenerAddLabel(ctx, daemon, logger, trig.ID, gardenerStuckEscalateLabel); err != nil {
		logger.Warn("gardener cred-crashloop: label write failed — NOT closing; leaving open to retry escalate next reconcile",
			"trigger", trig.ID, "error", err)
		return
	}
	reason := fmt.Sprintf(
		"gardener credential-crashloop: last %d spawns all closed close_reason=%s (credential-starved, zero productive work); escalating to operator instead of respawning (resumer watchdog will open a decision). Per-pod self-termination is ka-0000003924; this is the cross-spawn suppressor ka-0000006377.",
		credCrashloopThreshold, credCrashloopCloseReason)
	if err := daemon.AddComment(ctx, trig.ID, "trigger-engine", reason); err != nil {
		logger.Warn("gardener cred-crashloop: AddComment failed", "trigger", trig.ID, "error", err)
	}
	if err := daemon.CloseBead(ctx, trig.ID, map[string]string{
		"gardener_terminated_slot":  scheduler.StopSlotHardStop,
		"gardener_terminated_kind":  string(gardenerSkipCredCrashloop),
		"gardener_stuck_escalated":  "true",
		"gardener_cred_crashlooped": "true",
	}); err != nil {
		logger.Warn("gardener cred-crashloop: CloseBead failed", "trigger", trig.ID, "error", err)
	}
	// Disarm (ka-0000001423, ka-0000003627).
	gardenerDisarmOnClose(ctx, daemon, logger, trig.ID, scheduler.StopSlotHardStop, string(gardenerSkipCredCrashloop))
	logger.Warn("gardener escalated (credential crashloop)",
		"trigger", trig.ID, "threshold", credCrashloopThreshold, "close_reason", credCrashloopCloseReason)
}

// gardenerAgentStillRunning reports whether the agent bead is non-terminal and
// not stale. Fail-open: an error or missing bead is "not running". An agent
// whose updated_at is older than staleAfter (0 → 3h) is "not running", so a
// hung agent can't hold the gate forever; the bead is left untouched
// (ka-0000001095). A zero updated_at is not stale. A zero now means time.Now.
func gardenerAgentStillRunning(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, agentID string, staleAfter time.Duration, now time.Time) bool {
	detail, err := daemon.GetBead(ctx, agentID)
	if err != nil {
		logger.Warn("gardener one_spawn_at_a_time: GetBead failed, treating as not-running",
			"agent", agentID, "error", err)
		return false
	}
	if detail == nil {
		return false
	}
	switch detail.Status {
	case "open", "in_progress":
		threshold := resolveLeaseStale(staleAfter)
		if now.IsZero() {
			now = time.Now()
		}
		if !detail.UpdatedAt.IsZero() && now.Sub(detail.UpdatedAt) > threshold {
			logger.Warn("gardener one_spawn_at_a_time: prior agent stale past lease bound — treating as not-running so the gardener can resume",
				"agent", agentID, "agent_name", detail.Fields["agent"],
				"updated_at", detail.UpdatedAt.UTC().Format(time.RFC3339),
				"stale_for", now.Sub(detail.UpdatedAt).Truncate(time.Minute).String(),
				"threshold", threshold.String())
			return false
		}
		return true
	default:
		return false
	}
}

// gardenerMaxConcurrentRuns returns the trigger's live-run cap:
// max_concurrent_runs if positive, else 1 (ka-0000003097).
func gardenerMaxConcurrentRuns(trig TriggerDef) int {
	if trig.MaxConcurrentRuns > 0 {
		return trig.MaxConcurrentRuns
	}
	return 1
}

// gardenerAtConcurrentRunsCap reports whether a gardener trigger already has
// gardenerMaxConcurrentRuns live agents (ka-0000003097); false for a
// non-gardener. Live agents carry spawned_by:trigger:<id> or, for a resumed one,
// triggerLeaseField (gd-H17IlFclcl); stale holders don't count, as in
// liveSpawnCount. Fail-open on a daemon error. A zero now means time.Now.
func gardenerAtConcurrentRunsCap(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, now time.Time) bool {
	if trig.Gardener == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	holders, err := listTriggerLeaseAgents(ctx, daemon, trig.ID)
	if err != nil {
		logger.Warn("gardener max_concurrent_runs: live-agent count failed, treating as under cap",
			"trigger", trig.ID, "error", err)
		return false
	}
	threshold := resolveLeaseStale(trig.LeaseStaleAfter)
	live := 0
	for _, b := range holders {
		if leaseHolderStale(b, threshold, now) || leaseHolderOverAgeCeiling(b, threshold, now) {
			continue
		}
		live++
	}
	capN := gardenerMaxConcurrentRuns(trig)
	if live >= capN {
		logger.Info("gardener max_concurrent_runs: at live-run cap, deferring fire",
			"trigger", trig.ID, "live", live, "cap", capN)
		return true
	}
	return false
}

// gardenerTerminate stamps a terminating comment on the trigger bead and closes
// it. Errors are logged but not propagated — the gardener's running state on
// the engine is the source of truth for "don't fire again".
func gardenerTerminate(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, slot, kind string) {
	reason := fmt.Sprintf("gardener terminated: %s:%s", slot, kind)
	if err := daemon.AddComment(ctx, trig.ID, "trigger-engine", reason); err != nil {
		logger.Warn("gardener terminate: AddComment failed",
			"trigger", trig.ID, "slot", slot, "kind", kind, "error", err)
	}
	// Gardener succession (C4, gardener-chaining design §3.2): spawn the
	// `chain.to` heir before closing, while this bead's chain/labels are still
	// readable.
	maybeSpawnChainHeir(ctx, daemon, logger, trig, slot, kind)
	if err := daemon.CloseBead(ctx, trig.ID, map[string]string{
		"gardener_terminated_slot": slot,
		"gardener_terminated_kind": kind,
	}); err != nil {
		logger.Warn("gardener terminate: CloseBead failed",
			"trigger", trig.ID, "slot", slot, "kind", kind, "error", err)
	}
	gardenerDisarmOnClose(ctx, daemon, logger, trig.ID, slot, kind)
}

// gardenerDisarmOnClose sets `enabled`=false after an engine-authored close so
// a retired lane reads closed+disabled (ka-0000001423). It is a separate typed
// write because `enabled` is boolean-typed and a string value would 400 the
// whole CloseBead map (ka-0000003627, ka-0000007976). Best-effort: LoadTriggers
// skips closed beads anyway, and closed_enabled_drain.go backfills a failure.
// Revival paths re-enable (gardener_operator_reopen.go, gardenerGrantSecondLife).
func gardenerDisarmOnClose(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, beadID, slot, kind string) {
	if err := daemon.UpdateBeadFieldsTyped(ctx, beadID, map[string]any{"enabled": false}); err != nil {
		logger.Warn("gardener terminate: disarm (enabled=false) write failed — gardener closed but still armed; drain sweep will backfill",
			"trigger", beadID, "slot", slot, "kind", kind, "error", err)
	}
}

// gardenerPostFire, after a successful fire, bumps RunsSpawned (and the daily
// count), stamps LastAgentID and persists the blob. In the same write it
// mirrors runs_spawned, last_agent_id and last_run to top-level fields for
// external readers (ka-0000002275); last_checked, not last_run, is the
// liveness signal.
//
// A non-empty matchKey is saved as last_match so a schedule-sourced gardener's
// catch-up skips matches <= it across a restart (gd-pLf5LSHDEh).
func gardenerPostFire(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, agentID, matchKey string) {
	if trig.Gardener == nil {
		return
	}
	updated := *trig.Gardener
	updated.RunsSpawned++
	// The daily count is kept only for a gardener that has a daily cap, so
	// every other gardener's blob stays byte-identical.
	if updated.MaxRunsPerDay > 0 {
		if today := gardenerRunsDay(time.Now()); updated.RunsDay != today {
			updated.RunsDay, updated.RunsToday = today, 0
		}
		updated.RunsToday++
	}
	if agentID != "" {
		updated.LastAgentID = agentID
		// The first agent is the lineage root, fixed thereafter
		// (ka-0000006931; see stampGardenerLoopContext).
		if updated.LineageRoot == "" {
			updated.LineageRoot = agentID
		}
	}
	raw, err := json.Marshal(updated)
	if err != nil {
		logger.Warn("gardener post-fire: marshal failed",
			"trigger", trig.ID, "error", err)
		return
	}
	// Typed write: runs_spawned is integer-typed (ka-0000007976).
	fields := map[string]any{
		"gardener":     string(raw),
		"runs_spawned": updated.RunsSpawned,
		"last_run":     time.Now().UTC().Format(time.RFC3339),
	}
	if updated.LastAgentID != "" {
		fields["last_agent_id"] = updated.LastAgentID
	}
	if matchKey != "" {
		fields["last_match"] = matchKey
	}
	if err := daemon.UpdateBeadFieldsTyped(ctx, trig.ID, fields); err != nil {
		logger.Warn("gardener post-fire: UpdateBeadFieldsTyped failed",
			"trigger", trig.ID, "error", err)
		return
	}
	// Later gates this cycle see the bumped counters.
	trig.Gardener.RunsSpawned = updated.RunsSpawned
	trig.Gardener.RunsDay = updated.RunsDay
	trig.Gardener.RunsToday = updated.RunsToday
	trig.Gardener.LastAgentID = updated.LastAgentID
	trig.Gardener.LineageRoot = updated.LineageRoot
}

// gardenerDaemonStopAdapter wraps a gardenerDaemon as a
// scheduler.GardenerStopDaemon.
type gardenerDaemonStopAdapter struct {
	d gardenerDaemon
}

func (a gardenerDaemonStopAdapter) GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	return a.d.GetBead(ctx, beadID)
}

func (a gardenerDaemonStopAdapter) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return a.d.ListBeadsFiltered(ctx, q)
}

// GetBeadStatus satisfies the conditions package's optional beadStatusClient,
// so a parent-scoped bead_query loop_end stops once its root epic closes
// (ka-0000008051).
func (a gardenerDaemonStopAdapter) GetBeadStatus(ctx context.Context, beadID string) (string, error) {
	detail, err := a.d.GetBead(ctx, beadID)
	if err != nil {
		return "", err
	}
	if detail == nil {
		return "", nil
	}
	return detail.Status, nil
}

// gardenerBatchDepDaemon is the optional batch-fetch surface for the
// exclude_blocked_by_open_types filter (ka-0000002198). Kept off gardenerDaemon
// so lean mocks need not implement it; without it the filter excludes nothing.
type gardenerBatchDepDaemon interface {
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// errGardenerDaemonNoBatch signals the wrapped daemon can't batch-fetch deps.
// The conditions filter treats any error here as fail-safe (keep full count).
var errGardenerDaemonNoBatch = fmt.Errorf("gardener daemon does not support batch dependency fetch")

// BatchGetDependencies and BatchGetBeads satisfy the conditions package's
// optional blockedByResolver capability so the exclude_blocked_by_open_types
// loop_end filter works on the live gardener fire path, not just in unit tests.
func (a gardenerDaemonStopAdapter) BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	if b, ok := a.d.(gardenerBatchDepDaemon); ok {
		return b.BatchGetDependencies(ctx, ids, direction)
	}
	return nil, errGardenerDaemonNoBatch
}

// gardenerMRURLDaemon is the optional MR-URL surface for the specific_mr_merged
// stop kind (ka-0000005598), kept off gardenerDaemon like gardenerBatchDepDaemon.
type gardenerMRURLDaemon interface {
	MRBeadByURL(ctx context.Context, mrURL string) (*beadsapi.BeadDetail, error)
}

// MRBeadByURL satisfies the scheduler's mrURLResolver. Without the capability
// it returns (nil, nil): "no mirror bead yet", i.e. not met.
func (a gardenerDaemonStopAdapter) MRBeadByURL(ctx context.Context, mrURL string) (*beadsapi.BeadDetail, error) {
	if m, ok := a.d.(gardenerMRURLDaemon); ok {
		return m.MRBeadByURL(ctx, mrURL)
	}
	return nil, nil
}

func (a gardenerDaemonStopAdapter) BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	if b, ok := a.d.(gardenerBatchDepDaemon); ok {
		return b.BatchGetBeads(ctx, ids)
	}
	return nil, errGardenerDaemonNoBatch
}

// gardenerRunNeverStarted reports whether a closed agent's agent_state is
// pre-pod (never reached "working"), so it must not count as a completed or
// zero-progress run (ka-0000004058). Keep in sync with
// statusreporter.isStartupState and reconciler expireDeferredBeads ("expired").
func gardenerRunNeverStarted(agentState string) bool {
	switch agentState {
	case "", "queued", "expired", "spawning", "assigning", "prewarmed":
		return true
	default:
		return false
	}
}

// gardenerAgentTracker mirrors scheduler.trackLastAgentStatus for bridge
// gardener triggers (Phase 3b/B7-c1, ka-0000003913). When the trigger has a
// non-empty LastAgentID, it checks that agent's status; on terminal (closed)
// state it:
//
//   - appends a scheduler.RunLogEntry to Gardener.RunLog (rolling cap)
//   - bumps Gardener.RunsCompleted
//   - on agent_state="failed" increments Gardener.ConsecutiveFailures
//   - on agent_state="done" resets Gardener.ConsecutiveFailures to 0
//   - records LastCheckedAgent to dedup repeat invocations
//   - persists the updated Gardener JSON via UpdateBeadFieldsTyped
//
// The max_runs / consecutive_failures stop kinds read these counters next
// cycle. Failures are fail-open and logged, like gardenerPostFire.
func gardenerAgentTracker(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) {
	if trig.Gardener == nil || trig.Gardener.LastAgentID == "" {
		return
	}
	capSpec := trig.Gardener

	if capSpec.LastCheckedAgent == capSpec.LastAgentID {
		return
	}

	detail, err := daemon.GetBead(ctx, capSpec.LastAgentID)
	if err != nil || detail == nil {
		return
	}

	if detail.Status != "closed" {
		return
	}

	agentState := detail.Fields["agent_state"]

	// A never-ran phantom still gets a RunLog entry and dedup stamp, but no
	// completion, failure-streak or progress credit (ka-0000004058).
	neverStarted := gardenerRunNeverStarted(agentState)

	runLog, _ := scheduler.ParseRunLog(string(capSpec.RunLog))
	endedAt := time.Now()
	if !detail.UpdatedAt.IsZero() {
		endedAt = detail.UpdatedAt
	}
	runLog.Append(scheduler.RunLogEntry{
		Agent:         capSpec.LastAgentID,
		EndedAt:       endedAt,
		Outcome:       agentState,
		CloseReason:   detail.Fields["close_reason"],
		WrapupSummary: gardenerWrapUpAccomplishments(detail.Fields["wrapup"]),
	})

	// Copy by value so a marshal failure leaves the in-memory spec untouched.
	updated := *capSpec
	updated.LastCheckedAgent = capSpec.LastAgentID
	if !neverStarted {
		updated.RunsCompleted++
		switch agentState {
		case "failed":
			updated.ConsecutiveFailures++
		case "done":
			updated.ConsecutiveFailures = 0
		}
	}

	if marshalled, mErr := runLog.Marshal(); mErr == nil {
		updated.RunLog = json.RawMessage(marshalled)
	}

	// Record a per-run ProgressScore for no_progress_for; a missing wrap-up
	// scores 0. Only with a Progress spec, and never for a phantom.
	if len(capSpec.Progress) > 0 && !neverStarted {
		startedAt := detail.CreatedAt
		progState, _ := progress.ParseState(string(updated.ProgressState))
		progress.FinalizeRun(&progState, capSpec.LastAgentID, detail.Fields["wrapup"], agentState, startedAt, endedAt)
		if marshalled, mErr := progState.Marshal(); mErr == nil {
			updated.ProgressState = json.RawMessage(marshalled)
		} else {
			logger.Warn("gardener tracker: progress state marshal failed",
				"trigger", trig.ID, "agent", capSpec.LastAgentID, "error", mErr)
		}
		// Real progress resets the fruitless-resume counter (ka-0000005353).
		if len(progState.Runs) > 0 && progState.Runs[len(progState.Runs)-1].ProgressScore > 0 {
			updated.ResumesWithoutProgress = 0
		}
	}

	raw, mErr := json.Marshal(updated)
	if mErr != nil {
		logger.Warn("gardener tracker: marshal failed",
			"trigger", trig.ID, "agent", capSpec.LastAgentID, "error", mErr)
		return
	}
	// Mirror counters to top-level fields (ka-0000002275), typed because they
	// are integer-typed, as in gardenerPostFire (ka-0000007976, MR !5466,
	// ka-0000004023).
	if err := daemon.UpdateBeadFieldsTyped(ctx, trig.ID, map[string]any{
		"gardener":             string(raw),
		"runs_completed":       updated.RunsCompleted,
		"consecutive_failures": updated.ConsecutiveFailures,
	}); err != nil {
		logger.Warn("gardener tracker: UpdateBeadFieldsTyped failed",
			"trigger", trig.ID, "agent", capSpec.LastAgentID, "error", err)
		return
	}

	// Later pre-fire gates this cycle see the bumped counters.
	trig.Gardener.RunsCompleted = updated.RunsCompleted
	trig.Gardener.ConsecutiveFailures = updated.ConsecutiveFailures
	trig.Gardener.LastCheckedAgent = updated.LastCheckedAgent
	trig.Gardener.RunLog = updated.RunLog
	trig.Gardener.ProgressState = updated.ProgressState
}

// gardenerWrapUpAccomplishments extracts a short wrap-up summary for the
// RunLogEntry, mirroring the scheduler's helper; "" on absence or parse error.
func gardenerWrapUpAccomplishments(wrapupJSON string) string {
	if wrapupJSON == "" {
		return ""
	}
	var w struct {
		Accomplishments string `json:"accomplishments"`
	}
	if err := json.Unmarshal([]byte(wrapupJSON), &w); err != nil || w.Accomplishments == "" {
		return ""
	}
	s := w.Accomplishments
	const maxLen = 300
	if len(s) > maxLen {
		s = s[:maxLen-3] + "..."
	}
	return "Wrap-up: " + s
}
