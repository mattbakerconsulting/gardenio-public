// Engine-side closed+enabled dark-trigger DRAIN sweep (bug ka-0000002082).
//
// This is the fourth sibling of SweepGardenerLiveness (gardener_liveness.go),
// SweepDarkLaneByConcurrency (trigger_dark_lane.go) and
// SweepPouringButNeverWorked (gardener_pouring_stall.go). Where those three are
// read-only ALERT sweeps, this one is the CAPACITY fix for the closed+enabled
// dark-trigger class the liveness SCOPE BOUNDARY (gardener_liveness.go:18-28)
// hands to trigger-reviver.
//
// THE ARITHMETIC PROBLEM (the bug). trigger-reviver (ka-0000006342) owns the
// closed+enabled class, but its only output is ONE operator decision per 4h
// cycle. Measured 2026-07-28: 137 closed+enabled non-manual triggers, filed
// ~5–6 revival-review decisions/day while the operator resolved ~1/day, so the
// queue grew monotonically (16 open, oldest 10 days unanswered). A 1-per-cycle
// human-gated sweep cannot drain a 137-deep pool: a full oldest-first pass is
// ~23 days and the 7-day per-trigger re-review window re-admits triggers before
// the first pass finishes, so the tail is never reached. Meanwhile ~60% of the
// resolved verdicts were `archive` — a low-risk, reversible bookkeeping action
// spending a human decision each time.
//
// THE ENGINE IS THE RIGHT HOME. The core of the bug is arithmetic: the agent
// processes ONE candidate per cycle while the engine already iterates the whole
// trigger roster every cycle for free. Two classes of candidate need no
// per-trigger human judgement at all, and this sweep drains them deterministically
// with zero agent tokens:
//
//   1. AUTO-ARCHIVE (fix 1) — a closed+enabled trigger carrying `gardener-epic:<E>`
//      where epic E is CLOSED is a lane whose gardener lineage was intentionally
//      retired (the reviver already SKIPS these as retired). Instead of leaving
//      them enabled forever in the survey pool, the sweep sets `enabled=false`
//      and comments an audit line, so the candidate leaves the pool permanently
//      (it becomes closed+disabled — dark to neither reviver class). Capped per
//      run and reversible, mirroring the existing 1-mirror-enable/cycle autonomy
//      discipline (throttled, capped, commented, notified). Measured cohort: 72
//      of the 137.
//
//   2. GROUPED DECISION (fix 2) — a closed+enabled trigger with NO gardener-epic
//      label is genuinely ambiguous (probes, scratch, retired standalone
//      gardeners). Rather than one operator decision PER trigger, the sweep files
//      ONE grouped decision per outstanding batch listing every candidate, with
//      an archive-all / keep-all choice (and a free-text opt-out list), so one
//      operator action clears many rows. Deduped to at most one open grouped
//      decision at a time — the direct antidote to "files faster than resolved".
//      Measured cohort: 19 of the 137. The sweep EXECUTES the archive itself on
//      resolution (restart-safe via a marker on the decision bead), so the
//      operator's single click is the whole action.
//
//   3. ENGINE-RETIRED DISARM (fix 3, bug ka-0000001423) — a closed+enabled
//      trigger carrying a non-empty `gardener_terminated_kind` field is one the
//      ENGINE itself retired on an author-declared stop condition (loop_end/
//      bead_query, no_progress_for, wall_clock, max_runs, consecutive_failures).
//      That marker is the engine's own unambiguous record of an intentional
//      retirement — a DIRECT signal, strictly stronger than fix 1's closed-epic
//      inference — so the sweep sets `enabled=false` on it regardless of anchor-
//      epic status, no human judgement needed. This is the BACKFILL half of the
//      root-cause fix; the forward half (gardenerDisarmOnClose) disarms new
//      retirements at the close so the phantom class stops being minted. Measured
//      cohort: 40 of the closed+enabled pool carried a terminate marker, and 29
//      of those sit under an OPEN anchor epic — exactly the rows fix 1 skips and
//      the reviver otherwise re-triages every 4h forever. Capped/throttled/
//      audited/reversible, same discipline as fix 1.
//
// DELIBERATELY OUT OF SCOPE. A closed+enabled trigger anchored to an OPEN epic
// AND carrying NO engine terminate marker is NOT touched by this sweep. Of the
// 33 OPEN anchor epics measured, 30 still had open children — these are
// GENUINELY-UNCOVERED lanes that should be REVIVED, not archived, and putting
// them in a default-archive batch would be actively harmful. (Fix 3 is NOT a
// counterexample: it disables only beads carrying the engine's OWN terminate
// marker, which is proof of an intentional retirement — categorically different
// from a marker-less open-epic lane that merely went dark.) The genuinely-
// uncovered lanes are left to trigger-reviver's per-trigger revival-review (its
// 1/cycle budget now reaches them, because fixes 1+2+3 drain the low-value rows
// out of the pool first) and to the uncovered-open-lane roll-up (bug
// ka-0000002082 fix 4, a separate follow-up).
//
// Net effect: the closed+enabled phantom class collapses to the genuinely-
// uncovered set — fix 3 disarms the engine-retired rows (incl. the open-epic
// ones fix 1 could not), fix 1 archives the closed-epic rows, fix 2 batches the
// unanchored ambiguous ones into one operator decision, and the reviver's scarce
// human-decision budget is freed for the lanes that actually need a human.

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Closed+enabled drain sweep tunables.
const (
	// closedEnabledDrainRosterLimit bounds the closed+enabled candidate roster
	// pulled per run. The pool is ~137 today and this sweep SHRINKS it (an
	// archived trigger drops out of the `enabled=true` server-side filter next
	// run), so this cap only guards a runaway query; a breach is logged loudly
	// rather than silently truncated (advice ka-0000003555).
	closedEnabledDrainRosterLimit = 1000

	// closedEnabledDrainArchiveCap bounds how many triggers the sweep
	// auto-archives per run. Modest on purpose (mirrors the 1-mirror-enable/cycle
	// discipline's restraint): 20/run at the throttle below drains the ~72
	// closed-epic cohort in a few runs, not in one big irreversible sweep — so a
	// classification bug is caught by an operator noticing a handful of odd
	// archives long before the whole cohort is touched.
	closedEnabledDrainArchiveCap = 20

	// closedEnabledDrainDecisionCap bounds how many candidates one grouped
	// decision lists, keeping the Slack Block Kit message within limits. A batch
	// larger than this is split across successive decisions (the next runs after
	// the first is resolved).
	closedEnabledDrainDecisionCap = 40

	// closedEnabledDrainThrottle is the minimum wall-clock between full sweep
	// runs. The sweep MUTATES beads and files/executes decisions, so it must not
	// run on the engine's 30s Evaluate cadence: a 1h throttle bounds the
	// per-epic GetBead cost, gives an operator time to notice a bad archive
	// before the next batch, and paces decision execution to at most 1h after
	// resolution.
	closedEnabledDrainThrottle = time.Hour

	// closedEnabledDrainReviewCooldown is how long a candidate the operator
	// chose to KEEP (or opted out of an archive batch) is excluded from a new
	// grouped decision, so a declined batch is not immediately re-proposed.
	// Matches the reviver's 7-day per-trigger re-review window.
	closedEnabledDrainReviewCooldown = 7 * 24 * time.Hour

	// closedEnabledDrainMarkerLabel tags every grouped decision this sweep files
	// so it can find its own open/resolved batches (dedup + execute-on-resolve)
	// without scanning unrelated decisions.
	closedEnabledDrainMarkerLabel = "closed-enabled-drain"

	// closedEnabledDrainActor is the synthetic author stamped on the sweep's
	// writes (archive comments, decision beads), mirroring the sibling sweeps'
	// CreatedBy ("liveness-sweep", "dark-lane-sweep", …).
	closedEnabledDrainActor = "closed-enabled-drain-sweep"

	// closedEnabledDrainSystemOperator is the X-System-Operator source label the
	// sweep stamps on its writes so the daemon can attribute this TRUE
	// machine-origin cron write (no human behind it) instead of fail-closing on
	// "operator could not be resolved" (ka-0000007257). The engine authenticates
	// with the static primary token, on which the daemon honors X-System-Operator
	// (interceptors.go); under the authoring-human gate the daemon collapses every
	// system:<source> override into the single system:machine sink. Without this
	// the sweep's CreateBead (grouped decision) AND its per-archive AddComment
	// both 400, because both call resolveAuditOperatorRequired and the engine
	// sends no operator header — the archive UpdateBead survives only because it
	// preserves the bead's existing operated_by. Must satisfy the daemon's
	// sanitizeSystemOperatorHeader rules: ASCII lowercase / digits / '-'.
	closedEnabledDrainSystemOperator = "closed-enabled-drain"
)

// Decision/trigger bead fields the sweep reads and writes so the whole flow is
// restart-safe: the state that matters lives on beads, not in memory.
const (
	// drainCandidateIDsField holds the JSON array of trigger IDs a grouped
	// decision proposes to archive, so execution on resolution is deterministic
	// regardless of how the pool moved since the decision was filed.
	drainCandidateIDsField = "drain_candidate_ids"
	// drainExecutedAtField is stamped (RFC3339) once the sweep has executed a
	// resolved grouped decision, so a decision is executed exactly once even
	// across engine restarts.
	drainExecutedAtField = "drain_executed_at"
	// drainReviewedAtField is stamped (RFC3339) on a trigger the operator chose
	// to KEEP (or opted out of a batch), starting its re-propose cooldown.
	drainReviewedAtField = "drain_reviewed_at"
)

// drainOptionArchiveAll / drainOptionKeepAll are the two grouped-decision option
// ids. The daemon's built-in "Other" free-text response is treated as an
// opt-out list (see executeDrainDecision).
const (
	drainOptionArchiveAll = "archive-all"
	drainOptionKeepAll    = "keep-all"
)

// drainBeadIDPattern matches a ka-style bead id inside a free-text opt-out response.
var drainBeadIDPattern = regexp.MustCompile(`(?:gd|ka)-[A-Za-z0-9]+`)

// closedEnabledDrainSweepState throttles the whole sweep to once per
// closedEnabledDrainThrottle. Unlike the alert sweeps' per-trigger dedup maps,
// this sweep's idempotency lives on the beads it writes (enabled=false is
// self-deduping; drain_executed_at gates decision execution), so the only
// in-memory state needed is the last-run clock.
type closedEnabledDrainSweepState struct {
	mu        sync.Mutex
	lastRunAt time.Time
}

func newClosedEnabledDrainSweepState() *closedEnabledDrainSweepState {
	return &closedEnabledDrainSweepState{}
}

// dueForRun reports whether the throttle interval has elapsed since the last
// run, and if so records now as the new last-run time (so concurrent callers
// cannot both pass). A zero lastRunAt (first ever call) is always due.
func (s *closedEnabledDrainSweepState) dueForRun(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastRunAt.IsZero() && now.Sub(s.lastRunAt) < closedEnabledDrainThrottle {
		return false
	}
	s.lastRunAt = now
	return true
}

// gardenerEpicLabel returns the epic id a trigger bead is anchored to via a
// `gardener-epic:<id>` label, or "" when it carries none. Mirrors the resolution
// used by cmd/gb/gardener.go and beads-viewer/gardeners_owned_epic.go.
func gardenerEpicLabel(bead *beadsapi.BeadDetail) string {
	if bead == nil {
		return ""
	}
	for _, l := range bead.Labels {
		if rest, ok := strings.CutPrefix(l, "gardener-epic:"); ok {
			if id := strings.TrimSpace(rest); id != "" {
				return id
			}
		}
	}
	return ""
}

// SweepClosedEnabledDrain performs the closed+enabled dark-trigger drain pass
// (bug ka-0000002082). Once per throttle interval it:
//
//  1. auto-archives (enabled=false + audit comment) up to a per-run cap of
//     closed+enabled triggers whose gardener-epic is CLOSED (fix 1);
//  2. executes any resolved-but-unexecuted grouped decision it previously filed,
//     archiving the operator-approved candidates (fix 2 completion);
//  3. files ONE grouped decision for the no-gardener-epic-label ambiguous cohort
//     when none is outstanding (fix 2), so one operator action clears many rows.
//
// No-op unless closedEnabledDrainEnabled. Skips entirely under a global
// triggers-pause / gardeners-pause. Errors are logged and downgraded so a daemon
// hiccup cannot wedge the engine, and every mutation is reversible (enabled=true
// re-arms a trigger) and audited (a comment per archive, a notify per run).
func (e *TriggerEngine) SweepClosedEnabledDrain(ctx context.Context) {
	if !e.closedEnabledDrainEnabled {
		return
	}
	now := e.now()
	if !e.closedEnabledDrainState.dueForRun(now) {
		return
	}

	// A deliberate global pause means the operator is holding the fleet; the
	// engine must not be mutating trigger beads or filing operator decisions
	// under it. Mirrors the pouring-stall sweep's pause guards.
	if gardenersPauseEngaged(ctx, e.daemon, e.logger) {
		e.logger.Info("closed-enabled-drain: gardeners-pause engaged, skipping")
		return
	}
	if e.loadGateState(ctx).globallyPaused {
		e.logger.Info("closed-enabled-drain: triggers-pause engaged, skipping")
		return
	}

	// Attribute every write this sweep makes to the machine operator. This is a
	// TRUE machine-origin cron write with no human behind it, so the daemon's
	// fail-closed audit-operator resolver would otherwise 400 the grouped-decision
	// CreateBead AND each per-archive AddComment with "operator could not be
	// resolved" (ka-0000007257). WithSystemOperator sends X-System-Operator, which
	// the daemon honors on the engine's static-primary token and (under the
	// authoring-human gate) collapses into the single system:machine sink — the
	// sanctioned attribution for cron sweeps per the "real operator for every
	// bead" directive. It is last-resort on the daemon: any per-call human
	// operator still wins, so this never mis-attributes a human write. Set once
	// here so all downstream helpers (archive comments, decision create, cooldown
	// PATCHes) inherit it.
	ctx = beadsapi.WithSystemOperator(ctx, closedEnabledDrainSystemOperator)

	// The closed+enabled pool via a server-side field filter, so the candidate
	// set SHRINKS as the sweep archives (a just-archived trigger has
	// enabled=false and drops out next run) rather than re-scanning an
	// ever-growing closed-trigger table.
	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"closed"},
		Fields:   map[string]string{"enabled": "true"},
		Limit:    closedEnabledDrainRosterLimit,
	})
	if err != nil {
		e.logger.Warn("closed-enabled-drain: failed to load closed+enabled roster", "error", err)
		return
	}
	if res.Total > len(res.Beads) {
		// A silent truncation here would under-count the pool and read as
		// "drained" when it is not (advice ka-0000003555). Make it loud.
		e.logger.Warn("closed-enabled-drain: roster truncated by limit — pool larger than one page",
			"returned", len(res.Beads), "total", res.Total, "limit", closedEnabledDrainRosterLimit)
	}

	// Classify the pool. epicStatus memoizes GetBead per distinct anchor epic so
	// two triggers on the same epic cost one lookup.
	epicStatus := make(map[string]string)
	var terminatedKindCohort []*beadsapi.BeadDetail // fix 3: engine-retired (gardener_terminated_kind set) → auto-archive
	var closedEpicCohort []*beadsapi.BeadDetail     // fix 1: gardener-epic CLOSED → auto-archive
	var noLabelCohort []*beadsapi.BeadDetail        // fix 2: no gardener-epic label → grouped decision
	for _, bead := range res.Beads {
		if bead == nil {
			continue
		}
		// Defensive: the reviver skips manual + E2E-fixture triggers; so do we.
		// (Live data shows none manual among closed+enabled, but a future one
		// must not be auto-archived.)
		if bead.Fields["trigger_mode"] == "manual" || isE2EFixture(bead) {
			continue
		}
		// Fix 3 (bug ka-0000001423): a non-empty gardener_terminated_kind is the
		// engine's OWN unambiguous record that it retired this gardener on an
		// author-declared stop condition (loop_end/bead_query, no_progress_for,
		// wall_clock, max_runs, consecutive_failures, …). That is a DIRECT signal,
		// strictly stronger than fix 1's closed-anchor-epic INFERENCE — and it is
		// exactly the phantom class this sweep otherwise leaves stranded: 29 of
		// the 40 measured engine-retired beads sit under an OPEN anchor epic, so
		// fix 1 skips them and they re-survey every reviver cycle forever. The
		// source fix (gardenerDisarmOnClose) disarms new retirements at the close;
		// this cohort BACKFILLS the ones closed before that shipped AND is
		// defense-in-depth if the source disarm ever regresses. Classified BEFORE
		// the epic-label branch so an engine-retired bead is disabled by its own
		// terminate marker regardless of anchor-epic status. Reversible (reopen +
		// `gb trigger enable` re-arms), capped, throttled, audited — same
		// discipline as fix 1.
		if strings.TrimSpace(bead.Fields["gardener_terminated_kind"]) != "" {
			terminatedKindCohort = append(terminatedKindCohort, bead)
			continue
		}
		epicID := gardenerEpicLabel(bead)
		if epicID == "" {
			noLabelCohort = append(noLabelCohort, bead)
			continue
		}
		status, ok := epicStatus[epicID]
		if !ok {
			if epic, gerr := e.daemon.GetBead(ctx, epicID); gerr == nil && epic != nil {
				status = epic.Status
			} else {
				status = "" // unknown/missing → treat as NOT closed (conservative: never auto-archive)
			}
			epicStatus[epicID] = status
		}
		if status == "closed" {
			closedEpicCohort = append(closedEpicCohort, bead)
		}
		// An OPEN (or unknown) anchor epic is DELIBERATELY left untouched — see
		// the file header's "out of scope": those may be genuinely-uncovered
		// lanes that should be revived, not archived.
	}

	terminatedArchived := e.autoArchiveTerminatedKindCohort(ctx, terminatedKindCohort, now)
	archived := e.autoArchiveClosedEpicCohort(ctx, closedEpicCohort, now)
	// executePendingDrainDecisions returns the candidate IDs it just archived or
	// cooldowned, so maybeFileDrainDecision can exclude them: a resolved
	// decision's candidates are still in THIS run's pool snapshot, and re-listing
	// them in a fresh decision (or re-proposing a just-archived trigger) would be
	// wrong. The daemon reflects the writes on the NEXT run's pool query; the
	// touched set covers the within-run gap.
	executed, touched := e.executePendingDrainDecisions(ctx, now)
	filed := e.maybeFileDrainDecision(ctx, noLabelCohort, touched, now)

	e.logger.Info("closed-enabled-drain: complete",
		"pool", len(res.Beads),
		"terminated_kind_cohort", len(terminatedKindCohort),
		"closed_epic_cohort", len(closedEpicCohort),
		"no_label_cohort", len(noLabelCohort),
		"terminated_kind_archived", terminatedArchived,
		"auto_archived", archived,
		"decisions_executed", executed,
		"decision_filed", filed)

	// Silent-zero guard (advice ka-0000003555): a cohort with candidates that
	// produced NO archive / NO filed decision is the exact signature that hid
	// this bug for two days — the "complete" INFO line read as "nothing to do"
	// while every write was actually 400ing. Escalate to WARN so a cohort that
	// stays non-empty across runs with zero effect is loud, not silent. The
	// no-label cohort is only flagged when a decision was ALSO not filed (an
	// existing open decision or a full cooldown is a legitimate zero).
	if len(terminatedKindCohort) > 0 && terminatedArchived == 0 {
		e.logger.Warn("closed-enabled-drain: terminated-kind cohort non-empty but archived 0 — every archive write may be failing",
			"cohort", len(terminatedKindCohort))
	}
	if len(closedEpicCohort) > 0 && archived == 0 {
		e.logger.Warn("closed-enabled-drain: closed-epic cohort non-empty but archived 0 — every archive write may be failing",
			"cohort", len(closedEpicCohort))
	}
	if len(noLabelCohort) > 0 && filed == "" && executed == 0 {
		e.logger.Warn("closed-enabled-drain: no-label cohort non-empty but no decision filed or executed — decision create may be failing (or all candidates are in cooldown / a decision is already open)",
			"cohort", len(noLabelCohort))
	}

	if e.closedEnabledDrainNotify && e.notifier != nil && (terminatedArchived > 0 || archived > 0 || executed > 0 || filed != "") {
		e.notifier(ctx, "closed-enabled-drain", e.drainRunSummary(terminatedArchived, archived, executed, filed, len(terminatedKindCohort), len(closedEpicCohort), len(noLabelCohort)), "")
	}
}

// autoArchiveTerminatedKindCohort disables (enabled=false) up to the per-run cap
// of closed+enabled triggers the engine ITSELF retired on an author-declared
// stop condition — those carrying a non-empty gardener_terminated_kind marker
// (bug ka-0000001423). Returns the count actually archived. This is the backfill
// half of the fix whose forward half is gardenerDisarmOnClose: the source path now
// disarms new retirements at the close, this drains the ones closed before that
// shipped (and re-drains any that slip through). Reversible: reopening the bead
// and `gb trigger enable` re-arm it (the sanctioned revival flow re-enables
// automatically). Shares the per-run cap / oldest-first ordering / audit-comment
// discipline with autoArchiveClosedEpicCohort.
func (e *TriggerEngine) autoArchiveTerminatedKindCohort(ctx context.Context, cohort []*beadsapi.BeadDetail, now time.Time) int {
	if len(cohort) == 0 {
		return 0
	}
	// Oldest-updated first, deterministic per-run cap application.
	sort.SliceStable(cohort, func(i, j int) bool {
		return cohort[i].UpdatedAt.Before(cohort[j].UpdatedAt)
	})

	archived := 0
	for _, bead := range cohort {
		if archived >= closedEnabledDrainArchiveCap {
			e.logger.Info("closed-enabled-drain: hit per-run archive cap (terminated-kind cohort), remaining drains next run",
				"cap", closedEnabledDrainArchiveCap, "remaining", len(cohort)-archived)
			break
		}
		kind := strings.TrimSpace(bead.Fields["gardener_terminated_kind"])
		slot := strings.TrimSpace(bead.Fields["gardener_terminated_slot"])
		if err := e.daemon.UpdateBeadFieldsTyped(ctx, bead.ID, map[string]any{"enabled": false}); err != nil {
			e.logger.Warn("closed-enabled-drain: failed to auto-archive engine-retired trigger",
				"trigger", bead.ID, "kind", kind, "error", err)
			continue
		}
		archived++
		e.logger.Info("closed-enabled-drain: auto-archived engine-retired trigger",
			"trigger", bead.ID, "title", bead.Title, "terminated_slot", slot, "terminated_kind", kind)
		audit := fmt.Sprintf(
			"%s: auto-archived (enabled=false) — the engine retired this gardener on an author-declared stop "+
				"condition (gardener_terminated_slot=%s gardener_terminated_kind=%s), so it is a CORRECT retirement, "+
				"not an accidentally-dark lane. Backfills the closed+enabled phantom class the source fix now "+
				"prevents at the close (bug ka-0000001423). Reversible: reopen + `gb trigger enable %s` re-arms it. "+
				"This removes the trigger from the trigger-reviver survey pool.",
			closedEnabledDrainActor, slotOrDash(slot), kind, bead.ID)
		if err := e.daemon.AddComment(ctx, bead.ID, closedEnabledDrainActor, audit); err != nil {
			e.logger.Warn("closed-enabled-drain: failed to comment engine-retired archive audit line",
				"trigger", bead.ID, "error", err)
		}
	}
	return archived
}

// slotOrDash renders an empty terminated-slot as an em-dash for the audit line
// so a marker that carries a kind but no slot still reads cleanly.
func slotOrDash(slot string) string {
	if slot == "" {
		return "—"
	}
	return slot
}

// autoArchiveClosedEpicCohort disables (enabled=false) up to the per-run cap of
// closed+enabled triggers whose anchor epic is CLOSED, commenting an audit line
// on each. Returns the count actually archived. Reversible: `gb trigger enable`
// (enabled=true) re-arms any of them.
func (e *TriggerEngine) autoArchiveClosedEpicCohort(ctx context.Context, cohort []*beadsapi.BeadDetail, now time.Time) int {
	if len(cohort) == 0 {
		return 0
	}
	// Oldest-updated first, so the sweep works down the staleest lanes and the
	// per-run cap is applied deterministically.
	sort.SliceStable(cohort, func(i, j int) bool {
		return cohort[i].UpdatedAt.Before(cohort[j].UpdatedAt)
	})

	archived := 0
	for _, bead := range cohort {
		if archived >= closedEnabledDrainArchiveCap {
			e.logger.Info("closed-enabled-drain: hit per-run archive cap, remaining cohort drains next run",
				"cap", closedEnabledDrainArchiveCap, "remaining", len(cohort)-archived)
			break
		}
		epicID := gardenerEpicLabel(bead)
		if err := e.daemon.UpdateBeadFieldsTyped(ctx, bead.ID, map[string]any{"enabled": false}); err != nil {
			e.logger.Warn("closed-enabled-drain: failed to auto-archive trigger",
				"trigger", bead.ID, "error", err)
			continue
		}
		archived++
		e.logger.Info("closed-enabled-drain: auto-archived closed-epic trigger",
			"trigger", bead.ID, "title", bead.Title, "anchor_epic", epicID)
		audit := fmt.Sprintf(
			"%s: auto-archived (enabled=false) — anchor epic %s is CLOSED, so this closed+enabled "+
				"trigger's gardener lineage was intentionally retired, not accidentally dark (bug ka-0000002082 "+
				"fix 1). Reversible: `gb trigger enable %s` re-arms it. This removes the trigger from the "+
				"trigger-reviver survey pool so its 1/cycle human-decision budget reaches genuinely-uncovered lanes.",
			closedEnabledDrainActor, epicID, bead.ID)
		if err := e.daemon.AddComment(ctx, bead.ID, closedEnabledDrainActor, audit); err != nil {
			e.logger.Warn("closed-enabled-drain: failed to comment archive audit line",
				"trigger", bead.ID, "error", err)
		}
	}
	return archived
}

// executePendingDrainDecisions finds grouped decisions this sweep previously
// filed that are now RESOLVED but not yet executed, and archives the
// operator-approved candidates. Returns the number of decisions executed and the
// SET of candidate IDs it touched this run (archived or cooldowned), so the
// caller can exclude them from a freshly-filed decision within the same run.
//
// Restart-safe: the candidate IDs live on the decision bead
// (drain_candidate_ids) and drainExecutedAtField gates each decision to
// exactly-once execution, so an engine restart between resolution and execution
// re-picks the pending decision cleanly.
func (e *TriggerEngine) executePendingDrainDecisions(ctx context.Context, now time.Time) (int, map[string]bool) {
	touched := map[string]bool{}
	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"closed"},
		Labels:   []string{closedEnabledDrainMarkerLabel},
		Limit:    100,
	})
	if err != nil {
		e.logger.Warn("closed-enabled-drain: failed to load resolved grouped decisions", "error", err)
		return 0, touched
	}
	executed := 0
	for _, dec := range res.Beads {
		if dec == nil || dec.Fields[drainExecutedAtField] != "" {
			continue // already executed (or not ours to execute)
		}
		e.executeDrainDecision(ctx, dec, now, touched)
		executed++
	}
	return executed, touched
}

// executeDrainDecision applies one resolved grouped decision. The operator's
// choice maps to:
//
//   - archive-all           → archive every still-qualifying candidate.
//   - keep-all              → archive none; cooldown every candidate.
//   - Other (free text)     → treat any kd-ids named in the response as an
//     OPT-OUT list (kept + cooldowned); archive the rest. No ids named ⇒ safe
//     default of keep-all.
//
// Every candidate is re-validated (still closed+enabled) before archiving, so a
// trigger reopened/re-armed between filing and resolution is never clobbered.
// Each candidate id it acts on is recorded in `touched` so the caller can
// exclude it from a decision filed later in the same run.
func (e *TriggerEngine) executeDrainDecision(ctx context.Context, dec *beadsapi.BeadDetail, now time.Time, touched map[string]bool) {
	var candidateIDs []string
	if raw := dec.Fields[drainCandidateIDsField]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &candidateIDs); err != nil {
			e.logger.Warn("closed-enabled-drain: decision has unparseable candidate list",
				"decision", dec.ID, "error", err)
		}
	}

	chosen := normalizeDrainChoice(dec.Fields["chosen"])
	responseText := dec.Fields["response_text"]

	// Determine the opt-out set: explicit keep-all keeps everything; an "Other"
	// free-text response names the kept ids; archive-all keeps nothing.
	optedOut := map[string]bool{}
	archiveApproved := false
	switch {
	case chosen == drainOptionArchiveAll:
		archiveApproved = true
		for _, id := range drainBeadIDPattern.FindAllString(responseText, -1) {
			optedOut[id] = true // archive-all may still carry "except ka-X" in the note
		}
	case chosen == drainOptionKeepAll:
		archiveApproved = false
	default:
		// "Other" / free text / unmatched: archive only if the operator named an
		// explicit archive intent; otherwise treat as keep-all (conservative).
		ids := drainBeadIDPattern.FindAllString(responseText, -1)
		if len(ids) > 0 && strings.Contains(strings.ToLower(responseText), "archive") {
			archiveApproved = true
			// In free-text mode the named ids are the OPT-OUT list.
			for _, id := range ids {
				optedOut[id] = true
			}
		}
	}

	archived, kept := 0, 0
	for _, id := range candidateIDs {
		touched[id] = true
		if archiveApproved && !optedOut[id] {
			if e.archiveDecisionCandidate(ctx, id, dec.ID) {
				archived++
				continue
			}
		}
		// Kept (either by choice or because re-validation failed) → cooldown so
		// it is not immediately re-proposed.
		if err := e.daemon.UpdateBeadFields(ctx, id, map[string]string{drainReviewedAtField: now.UTC().Format(time.RFC3339)}); err != nil {
			e.logger.Warn("closed-enabled-drain: failed to cooldown kept candidate", "trigger", id, "error", err)
		}
		kept++
	}

	if err := e.daemon.UpdateBeadFields(ctx, dec.ID, map[string]string{drainExecutedAtField: now.UTC().Format(time.RFC3339)}); err != nil {
		e.logger.Warn("closed-enabled-drain: failed to mark decision executed", "decision", dec.ID, "error", err)
	}
	e.logger.Info("closed-enabled-drain: executed grouped decision",
		"decision", dec.ID, "chosen", chosen, "archived", archived, "kept", kept)
}

// normalizeDrainChoice maps a resolved `chosen` value back to one of the two
// canonical option ids. The Slack button path stores the option id directly
// (bot_decisions_modal.go resolveOptionChoice), but the web-UI path stores the
// raw selected string, which may be the human "short"/"label" text. Recognising
// both keeps execution correct regardless of resolution surface; anything
// unrecognised falls through to the "Other"/free-text arm.
func normalizeDrainChoice(chosen string) string {
	c := strings.ToLower(strings.TrimSpace(chosen))
	switch c {
	case drainOptionArchiveAll, "archive all":
		return drainOptionArchiveAll
	case drainOptionKeepAll, "keep all":
		return drainOptionKeepAll
	default:
		return c
	}
}

// archiveDecisionCandidate re-validates one grouped-decision candidate is still
// a closed+enabled trigger and, if so, archives it (enabled=false + audit
// comment). Returns true when it archived. A candidate that changed shape since
// the decision was filed (reopened, already disabled, deleted) is left alone.
func (e *TriggerEngine) archiveDecisionCandidate(ctx context.Context, id, decisionID string) bool {
	bead, err := e.daemon.GetBead(ctx, id)
	if err != nil || bead == nil {
		e.logger.Warn("closed-enabled-drain: candidate lookup failed, skipping archive", "trigger", id, "error", err)
		return false
	}
	if bead.Type != "trigger" || bead.Status != "closed" || bead.Fields["enabled"] == "false" {
		// No longer a closed+enabled trigger — do not clobber whatever it became.
		e.logger.Info("closed-enabled-drain: candidate no longer closed+enabled, skipping",
			"trigger", id, "status", bead.Status, "enabled", bead.Fields["enabled"])
		return false
	}
	if err := e.daemon.UpdateBeadFieldsTyped(ctx, id, map[string]any{"enabled": false}); err != nil {
		e.logger.Warn("closed-enabled-drain: failed to archive approved candidate", "trigger", id, "error", err)
		return false
	}
	audit := fmt.Sprintf(
		"%s: auto-archived (enabled=false) per operator-resolved grouped decision %s (bug ka-0000002082 fix 2). "+
			"Reversible: `gb trigger enable %s` re-arms it.",
		closedEnabledDrainActor, decisionID, id)
	if err := e.daemon.AddComment(ctx, id, closedEnabledDrainActor, audit); err != nil {
		e.logger.Warn("closed-enabled-drain: failed to comment grouped-archive audit line", "trigger", id, "error", err)
	}
	return true
}

// maybeFileDrainDecision files ONE grouped archive decision for the
// no-gardener-epic-label cohort, but only when no grouped decision is currently
// outstanding (open, or resolved-but-unexecuted). Returns the new decision id,
// or "" when nothing was filed (dedup, empty cohort, or all in cooldown). This
// one-open-at-a-time dedup is the direct fix for "files faster than resolved".
func (e *TriggerEngine) maybeFileDrainDecision(ctx context.Context, cohort []*beadsapi.BeadDetail, touched map[string]bool, now time.Time) string {
	// Dedup: at most one outstanding grouped decision. An OPEN one means the
	// operator has not answered the last batch; a resolved-but-unexecuted one is
	// handled by executePendingDrainDecisions THIS run and must not race a new
	// filing.
	open, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Labels:   []string{closedEnabledDrainMarkerLabel},
		Limit:    10,
	})
	if err != nil {
		e.logger.Warn("closed-enabled-drain: failed to check for open grouped decision", "error", err)
		return ""
	}
	if len(open.Beads) > 0 {
		return "" // a batch is already awaiting the operator — do not pile on.
	}

	// Filter out candidates still in their keep cooldown or just touched by a
	// decision executed earlier this run, then cap the batch.
	var candidates []*beadsapi.BeadDetail
	for _, bead := range cohort {
		if touched[bead.ID] {
			continue
		}
		if reviewed := parseLivenessTimestamp(bead.Fields[drainReviewedAtField]); !reviewed.IsZero() && now.Sub(reviewed) < closedEnabledDrainReviewCooldown {
			continue
		}
		candidates = append(candidates, bead)
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].UpdatedAt.Before(candidates[j].UpdatedAt)
	})
	capped := false
	if len(candidates) > closedEnabledDrainDecisionCap {
		candidates = candidates[:closedEnabledDrainDecisionCap]
		capped = true
	}

	decID, err := e.fileDrainDecision(ctx, candidates, capped, len(cohort))
	if err != nil {
		e.logger.Warn("closed-enabled-drain: failed to file grouped decision", "error", err)
		return ""
	}
	e.logger.Info("closed-enabled-drain: filed grouped archive decision",
		"decision", decID, "candidates", len(candidates), "capped", capped)
	return decID
}

// fileDrainDecision creates the grouped archive decision bead: a type=decision
// with archive-all / keep-all options, the candidate list rendered in the body,
// the candidate IDs stashed for deterministic execution, and the marker label
// for dedup + execute-on-resolve. Best-effort on the candidate-ids stash — the
// decision still surfaces even if that follow-up write fails.
func (e *TriggerEngine) fileDrainDecision(ctx context.Context, candidates []*beadsapi.BeadDetail, capped bool, cohortTotal int) (string, error) {
	ids := make([]string, len(candidates))
	var body strings.Builder
	body.WriteString(fmt.Sprintf(
		"trigger-reviver drain: %d closed+enabled trigger(s) carry NO gardener-epic anchor, so they cannot be "+
			"auto-classified as retired. Archiving a trigger sets enabled=false (reversible via `gb trigger enable`) "+
			"and removes it from the dark-trigger survey pool.\n\nCandidates:\n", len(candidates)))
	for i, bead := range candidates {
		ids[i] = bead.ID
		proj := bead.Fields["project"]
		if proj == "" {
			proj = "—"
		}
		body.WriteString(fmt.Sprintf("  • %s [%s] %s\n", bead.ID, proj, truncateForDecision(bead.Title, 80)))
	}
	if capped {
		body.WriteString(fmt.Sprintf("\n(%d more in the cohort not shown — the next batch follows once this one is resolved.)\n", cohortTotal-len(candidates)))
	}
	body.WriteString("\nChoose *Archive all* to disable every candidate above, or *Keep all* to leave them and " +
		"re-review in 7 days. To archive most but keep a few, choose *Other* and list the kd-ids to KEEP " +
		"(e.g. \"archive except ka-AAA ka-BBB\").")

	options := []map[string]string{
		{"id": drainOptionArchiveAll, "short": "Archive all", "label": fmt.Sprintf("Disable all %d candidate trigger(s) (reversible)", len(candidates)), "artifact_type": "report"},
		{"id": drainOptionKeepAll, "short": "Keep all", "label": "Leave all enabled; re-review in 7 days", "artifact_type": "report"},
	}
	optionsJSON, err := json.Marshal(options)
	if err != nil {
		return "", fmt.Errorf("marshal options: %w", err)
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("marshal candidate ids: %w", err)
	}

	prompt := fmt.Sprintf("trigger-reviver drain: archive %d unanchored closed+enabled trigger(s)? (batched — bug ka-0000002082)", len(candidates))
	fields := map[string]string{
		"options":              string(optionsJSON),
		"prompt":               prompt,
		"context":              body.String(),
		"requested_by":         e.drainRequestedBy(),
		drainCandidateIDsField: string(idsJSON),
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("marshal fields: %w", err)
	}

	return e.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       prompt,
		Type:        "decision",
		Kind:        "data",
		Description: body.String(),
		Priority:    2,
		CreatedBy:   closedEnabledDrainActor,
		Labels:      []string{"project:gardenio", closedEnabledDrainMarkerLabel},
		Fields:      fieldsJSON,
	})
}

// drainRequestedBy attributes the grouped decision to the engine's designated
// human operator when one is configured, else the sweep itself. A human
// requested_by keeps the decision attributable in the operator queue.
func (e *TriggerEngine) drainRequestedBy() string {
	if e.defaultOperator != "" {
		return e.defaultOperator
	}
	return closedEnabledDrainActor
}

// drainRunSummary builds the one-line notify summary for a run that did
// something.
func (e *TriggerEngine) drainRunSummary(terminatedArchived, archived, executed int, filed string, terminatedKindCohort, closedEpicCohort, noLabelCohort int) string {
	filedPart := "none"
	if filed != "" {
		filedPart = filed
	}
	return fmt.Sprintf(
		"🧹 closed-enabled-drain: disabled %d engine-retired trigger(s) (cohort %d, bug ka-0000001423), "+
			"auto-archived %d closed-epic trigger(s) (cohort %d), executed %d resolved grouped decision(s), "+
			"filed grouped decision=%s (unanchored cohort %d). Pool drains toward the genuinely-uncovered lanes (bug ka-0000002082).",
		terminatedArchived, terminatedKindCohort, archived, closedEpicCohort, executed, filedPart, noLabelCohort)
}

// truncateForDecision shortens a title for the decision body, adding an ellipsis
// when clipped.
func truncateForDecision(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
