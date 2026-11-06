// Engine-side gardener-reaper sweep (task ka-0000000102, epic ka-0000005180).
//
// THE GAP THIS FILLS. A gardener trigger's lifecycle is not coupled to its
// EPIC's lifecycle. A gardener declares on_loop_end=close (or bead_query stop)
// so it retires itself when the epic drains — but that stop only fires WHILE
// the loop is running. Once the epic is CLOSED, the cron preflight
// bead_query(open children > 0) is permanently false, so the loop never runs
// again to reach its own close. The gardener sits OPEN forever — enabled but
// inert (category C) — and a gardener that was operator-DISABLED before its epic
// closed (category A) has no loop at all, so nothing reaps it either. Gardener
// hygiene audit ka-0000001071 found 4 category-C offenders in one cycle and
// closed them by hand; the twice-daily remediate-gardeners gardener then ran 8
// consecutive fully-clean cycles finding nothing but this lifecycle-coupling
// gap. This sweep turns that manual sweep into engine-side self-healing.
//
// SIBLING SWEEPS, DISJOINT SCOPE. This is the sixth sweep in the family
// (SweepGardenerLiveness, SweepGardenerLivenessAllowList, SweepPouringButNeverWorked,
// SweepDarkLaneByConcurrency, SweepCredentialWedge, SweepClosedEnabledDrain).
// Two of them are adjacent and each DELIBERATELY excludes this cohort:
//
//   - SweepGardenerLiveness (gardener_liveness.go) ALERTS on an OPEN+enabled
//     gardener going dark, but only NOTIFIES — it never closes, and it skips a
//     gardener whose frontier epic is closed (a closed epic means "done", not
//     "dark"). So it sees category C but does nothing about it, by design.
//   - SweepClosedEnabledDrain (closed_enabled_drain.go) MUTATES, but its roster
//     is Statuses:["closed"] — it drains CLOSED+enabled retired lineages. An
//     OPEN gardener on a closed epic is never in its pool.
//
// The reaper is the missing quadrant: OPEN gardener trigger (enabled OR
// disabled) whose PINNED epic is CLOSED. That is the unambiguous "goal met,
// lane retired, but the trigger never got the memo" case — no human judgement
// needed, so the engine closes it deterministically with zero agent tokens.
//
// COORDINATION WITH trigger-reviver (ka-0000006342). The reviver files
// revive/archive DECISIONS for two DARK classes: (a) OPEN+disabled and
// (b) CLOSED+enabled. Both are disjoint from the reaper: the reaper acts only on
// OPEN triggers whose pinned epic is CLOSED. A gardener on an OPEN epic — the
// reviver's genuinely-uncovered-lane territory — is never touched here.
//
// STANDING-GARDENER SAFETY (load-bearing). A STANDING (perpetual) gardener may be
// legitimately pinned to a CLOSED epic and must NEVER be reaped — bug
// ka-0000006241 / operator decision ka-0000000928 are the incident where a
// give-up close "systematically reaped exactly the gardeners that must never
// die," taking the release lane dark. Worse, the engine reads standing-ness from
// MACHINE markers only (gardenerIsStanding: the blob's standing:true or the
// perpetual:true label), while authors also declare it in terminal_goal PROSE.
// The live category-C candidate ka-0000001526 is exactly this trap: an
// open+enabled gardener on a CLOSED epic whose terminal_goal says "STANDING
// monthly measure-only guard ... Do NOT stale-stop for the closed parent epic:
// operator Matt Baker explicitly revived this standing guard" — with NO machine
// marker. Reaping it would repeat the incident. So the reaper skips a gardener
// that is standing by EITHER machine marker (gardenerIsStanding) OR terminal_goal
// prose (TerminalGoalDeclaresStanding, the same detector the authoring/firing
// guard ka-0000004311 uses). The prose skip is a superset safety net: a
// prose-declared-but-unmarked standing gardener is precisely the shape that
// slips past gardenerIsStanding.
//
// DISCIPLINE (mirrors closed_enabled_drain.go). Throttled to once per hour (it
// mutates beads), pause-aware (skips under gardeners-pause / triggers-pause),
// per-run capped, and every close is reversible (reopen + `gb trigger enable`
// re-arms) and audited (a comment records the epic that closed and how to
// revert). Config-gated (ClosedEpicReaperEnabled) and shipped DEFAULT-OFF so an
// operator can read the dry-run "would reap" log lines and confirm the closed-
// epic + standing guards match only genuine retirements before arming a
// destructive close path — the rollout discipline task ka-0000003021 names.

package bridge

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// Gardener-epic reaper sweep tunables.
const (
	// closedEpicReaperRosterLimit bounds the OPEN-trigger roster pulled per run.
	// The live open-trigger fleet is tens of beads; this cap only guards a
	// runaway query. A breach is logged loudly rather than silently truncated
	// (advice ka-0000003555).
	closedEpicReaperRosterLimit = 500

	// closedEpicReaperCap bounds how many gardeners the sweep closes per run.
	// Modest on purpose (mirrors closed_enabled_drain's 20/run restraint): the
	// steady-state cohort is a handful, so a cap this size drains it in one run
	// while still bounding the blast radius of a classification bug — an operator
	// notices a few odd closes long before a whole cohort is touched.
	closedEpicReaperCap = 20

	// closedEpicReaperThrottle is the minimum wall-clock between full sweep runs.
	// The sweep MUTATES beads (closes gardeners), so it must not run on the
	// engine's ~30s Evaluate cadence: a 1h throttle bounds the per-epic GetBead
	// cost and gives an operator time to notice a bad close before the next run.
	closedEpicReaperThrottle = time.Hour

	// closedEpicReaperActor is the synthetic author stamped on the sweep's writes
	// (close comments), mirroring the sibling sweeps' CreatedBy.
	closedEpicReaperActor = "gardener-epic-reaper-sweep"

	// closedEpicReaperSystemOperator is the X-System-Operator source label the
	// sweep stamps on its writes so the daemon attributes this TRUE machine-origin
	// cron write (no human behind it) instead of fail-closing on "operator could
	// not be resolved" (ka-0000007257). Same discipline as
	// closedEnabledDrainSystemOperator: the daemon honors X-System-Operator on the
	// engine's static-primary token and collapses it into the system:machine sink.
	// Without it the per-close AddComment 400s (resolveAuditOperatorRequired sends
	// no operator header); the CloseBead survives only because it preserves the
	// bead's existing operated_by. Must be ASCII lowercase / digits / '-'.
	closedEpicReaperSystemOperator = "gardener-epic-reaper"

	// closedEpicReaperStopKind is the gardener_terminated_kind stamped on a reaped
	// gardener, so the close is legible as an intentional lifecycle retirement to
	// the drain sweep's terminated-kind backfill and to `gb gardener status`,
	// distinct from a self-close on a declared stop entry.
	closedEpicReaperStopKind = "epic_closed"
)

// closedEpicReaperSweepState throttles the whole sweep to once per
// closedEpicReaperThrottle. Idempotency lives on the beads it writes (a closed
// gardener drops out of the OPEN-status roster next run), so the only in-memory
// state needed is the last-run clock — mirroring closedEnabledDrainSweepState.
type closedEpicReaperSweepState struct {
	mu        sync.Mutex
	lastRunAt time.Time
}

func newClosedEpicReaperSweepState() *closedEpicReaperSweepState {
	return &closedEpicReaperSweepState{}
}

// dueForRun reports whether the throttle interval has elapsed since the last
// run, and if so records now as the new last-run time (so concurrent callers
// cannot both pass). A zero lastRunAt (first ever call) is always due.
func (s *closedEpicReaperSweepState) dueForRun(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastRunAt.IsZero() && now.Sub(s.lastRunAt) < closedEpicReaperThrottle {
		return false
	}
	s.lastRunAt = now
	return true
}

// SweepClosedEpicGardeners performs the gardener-reaper pass (task ka-0000000102).
// Once per throttle interval it closes up to a per-run cap of OPEN gardener
// triggers whose pinned epic (assign_bead / gardener-epic:<id>) is CLOSED —
// categories A (disabled) and C (enabled-but-inert) from audit ka-0000001071.
//
// No-op unless closedEpicReaperEnabled. Skips entirely under a global
// gardeners-pause / triggers-pause. Errors are logged and downgraded so a daemon
// hiccup cannot wedge the engine, and every close is reversible (reopen +
// `gb trigger enable` re-arms) and audited (a comment per close).
func (e *TriggerEngine) SweepClosedEpicGardeners(ctx context.Context) {
	if !e.closedEpicReaperEnabled {
		return
	}
	now := e.now()
	if !e.closedEpicReaperState.dueForRun(now) {
		return
	}

	// A deliberate global pause means the operator is holding the fleet; the
	// engine must not be closing gardener beads under it. Mirrors the drain and
	// pouring-stall sweeps' pause guards.
	if gardenersPauseEngaged(ctx, e.daemon, e.logger) {
		e.logger.Info("gardener-epic-reaper: gardeners-pause engaged, skipping")
		return
	}
	if e.loadGateState(ctx).globallyPaused {
		e.logger.Info("gardener-epic-reaper: triggers-pause engaged, skipping")
		return
	}

	// Attribute every write this sweep makes to the machine operator (see the
	// closedEpicReaperSystemOperator doc): a TRUE machine-origin cron write with
	// no human behind it, so the per-close AddComment would otherwise 400. Set
	// once here so the close + comment inherit it.
	ctx = beadsapi.WithSystemOperator(ctx, closedEpicReaperSystemOperator)

	// The OPEN trigger roster. A closed gardener drops out of this server-side
	// status filter next run, so the candidate set SHRINKS as the sweep closes
	// rather than re-scanning.
	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Limit:    closedEpicReaperRosterLimit,
	})
	if err != nil {
		e.logger.Warn("gardener-epic-reaper: failed to load open-trigger roster", "error", err)
		return
	}
	if res.Total > len(res.Beads) {
		// A silent truncation here would under-count the pool and read as
		// "drained" when it is not (advice ka-0000003555). Make it loud.
		e.logger.Warn("gardener-epic-reaper: roster truncated by limit — pool larger than one page",
			"returned", len(res.Beads), "total", res.Total, "limit", closedEpicReaperRosterLimit)
	}

	// Classify: collect gardeners whose pinned epic is CLOSED. epicStatus memoizes
	// GetBead per distinct epic so two gardeners on the same epic cost one lookup.
	epicStatus := make(map[string]string)
	var cohort []reaperCandidate
	scanned := 0
	for _, bead := range res.Beads {
		if bead == nil {
			continue
		}
		trig := ParseTriggerDef(bead, "gardener-epic-reaper")

		// Only gardeners carry an epic-coupled lifecycle. A plain source-only
		// trigger has no frontier epic and no "epic closed ⇒ retire" contract.
		if !isGardenerTrigger(bead) {
			continue
		}
		// Defensive: never auto-close a manual trigger or an E2E fixture (whose
		// frozen state is intentional). Mirrors the drain sweep's guards.
		if bead.Fields["trigger_mode"] == "manual" || isE2EFixture(bead) {
			continue
		}
		// STANDING gardeners must NEVER be reaped, even on a closed epic — bug
		// ka-0000006241. Skip on EITHER a machine marker OR a terminal_goal prose
		// declaration (ka-0000004311): the prose case (e.g. ka-0000001526) is
		// exactly the shape that slips past the machine-marker check.
		if gardenerIsStanding(trig) {
			continue
		}
		if trig.Gardener != nil {
			if declared, _ := TerminalGoalDeclaresStanding(trig.Gardener.TerminalGoal); declared {
				e.logger.Info("gardener-epic-reaper: skipping prose-declared standing gardener (no machine marker) — not reaping despite closed epic",
					"gardener", bead.ID, "title", bead.Title)
				continue
			}
		}

		scanned++

		epicID := gardenerFrontierEpic(ctx, e.daemon, e.logger, trig)
		if epicID == "" {
			continue // no pinned epic ⇒ no closed-epic contract to enforce
		}
		status, ok := epicStatus[epicID]
		if !ok {
			if epic, gerr := e.daemon.GetBead(ctx, epicID); gerr == nil && epic != nil {
				status = epic.Status
			} else {
				// Unknown/missing epic → treat as NOT closed (conservative: never
				// reap a gardener whose epic we could not resolve).
				status = ""
				if gerr != nil {
					e.logger.Warn("gardener-epic-reaper: epic lookup failed, not reaping",
						"gardener", bead.ID, "epic", epicID, "error", gerr)
				}
			}
			epicStatus[epicID] = status
		}
		if status == "closed" {
			cohort = append(cohort, reaperCandidate{trig: trig, epicID: epicID})
		}
	}

	reaped := e.reapClosedEpicCohort(ctx, cohort)

	e.logger.Info("gardener-epic-reaper: complete",
		"open_gardeners_scanned", scanned, "closed_epic_cohort", len(cohort), "reaped", reaped)

	// Silent-zero guard (advice ka-0000003555): a non-empty cohort that reaped
	// nothing is the signature of every close write failing. Make it loud.
	if len(cohort) > 0 && reaped == 0 {
		e.logger.Warn("gardener-epic-reaper: cohort non-empty but reaped 0 — every close write may be failing",
			"cohort", len(cohort))
	}

	if e.closedEpicReaperNotify && e.notifier != nil && reaped > 0 {
		e.notifier(ctx, "gardener-epic-reaper", fmt.Sprintf(
			"🪝 gardener-epic-reaper: auto-closed %d OPEN gardener trigger(s) whose pinned epic is CLOSED "+
				"(cohort %d, categories A+C, task ka-0000000102). Reversible: reopen + `gb trigger enable`.",
			reaped, len(cohort)), "")
	}
}

// reaperCandidate pairs a gardener trigger with the closed epic that makes it
// reapable, so the close comment can name the epic without a second resolution.
type reaperCandidate struct {
	trig   TriggerDef
	epicID string
}

// reapClosedEpicCohort closes up to the per-run cap of gardeners whose pinned
// epic is closed, commenting an audit line on each. Returns the count actually
// closed. Reversible: reopening the bead and `gb trigger enable` re-arm it.
func (e *TriggerEngine) reapClosedEpicCohort(ctx context.Context, cohort []reaperCandidate) int {
	if len(cohort) == 0 {
		return 0
	}
	// Deterministic per-run cap application: oldest-created gardener first (a
	// stable proxy for "longest lingering").
	sort.SliceStable(cohort, func(i, j int) bool {
		return cohort[i].trig.CreatedAt.Before(cohort[j].trig.CreatedAt)
	})

	reaped := 0
	for _, c := range cohort {
		if reaped >= closedEpicReaperCap {
			e.logger.Info("gardener-epic-reaper: hit per-run cap, remaining cohort reaps next run",
				"cap", closedEpicReaperCap, "remaining", len(cohort)-reaped)
			break
		}
		if e.reapOneClosedEpicGardener(ctx, c) {
			reaped++
		}
	}
	return reaped
}

// reapOneClosedEpicGardener closes a single gardener whose pinned epic is closed:
// an audit comment BEFORE the close (so the provenance survives even if the
// close's own field write is all the operator later sees), then a CloseBead
// stamping the terminate markers, then a disarm so the retired lane presents as
// closed+disabled rather than re-entering the closed+enabled phantom class the
// drain sweep exists to clean up (bug ka-0000001423). Returns true when it
// closed. Every step is best-effort with a warn — a daemon hiccup on one
// gardener must not wedge the rest of the cohort.
func (e *TriggerEngine) reapOneClosedEpicGardener(ctx context.Context, c reaperCandidate) bool {
	id := c.trig.ID
	audit := fmt.Sprintf(
		"%s: auto-closed — pinned epic %s is CLOSED, so this gardener's goal is met and its lane retired, "+
			"but a closed epic makes the cron preflight permanently false so the loop never runs again to close "+
			"itself (audit ka-0000001071 categories A/C, task ka-0000000102). Reversible: reopen + `gb trigger enable %s` re-arms it.",
		closedEpicReaperActor, c.epicID, id)
	if err := e.daemon.AddComment(ctx, id, closedEpicReaperActor, audit); err != nil {
		e.logger.Warn("gardener-epic-reaper: AddComment failed (continuing to close)",
			"gardener", id, "epic", c.epicID, "error", err)
	}

	if err := e.daemon.CloseBead(ctx, id, map[string]string{
		"gardener_terminated_slot": scheduler.StopSlotLoopEnd,
		"gardener_terminated_kind": closedEpicReaperStopKind,
	}); err != nil {
		e.logger.Warn("gardener-epic-reaper: CloseBead failed",
			"gardener", id, "epic", c.epicID, "error", err)
		return false
	}

	// Disarm the retired lane (enabled=false) so it does not present as
	// closed+enabled. Separate typed write — enabled is boolean-typed and cannot
	// ride the string field-map above. Non-fatal (the drain sweep's
	// terminated-kind backfill would catch it), mirroring gardenerDisarmOnClose.
	gardenerDisarmOnClose(ctx, e.daemon, e.logger, id, scheduler.StopSlotLoopEnd, closedEpicReaperStopKind)

	e.logger.Info("gardener-epic-reaper: reaped gardener on closed epic",
		"gardener", id, "title", c.trig.Title, "epic", c.epicID,
		"was_enabled", c.trig.Enabled)
	return true
}
