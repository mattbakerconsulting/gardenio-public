package main

// plans_index_enrich.go — the /plans index batch-enrich: step rollups, current-or-next step, last activity and creator chips for each plan card.
// Split out of plans.go (gd-Wl6LuCnvlH); see plans.go for the Plans surface overview.

import (
	"context"
	"html/template"
	"net/url"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"golang.org/x/sync/errgroup"
)

// planIndexEnrich is the S0 batch-enrich seam: it turns the plan epics returned
// by the index query into fully-populated planRowView cards. Every per-plan
// signal the card needs — the step rollup (S existing), the open-decision count
// (folded into the step-data walk, ka-0000006056), the current-or-next step
// (S3), and the bound assistant's state (S4) — is derived here so the handler
// body stays a single pass.
//
// The heavy per-plan lookups (children walk for the current/next step AND the
// rendered-step progress rollup; the assistant-state join; the open-decision
// count) are ALL hoisted to ONE bulk call each across the whole page rather than
// one-per-row, so the whole enrich pass is a small BOUNDED number of daemon
// round-trips regardless of how many plans (up to plansListLimit) are on the
// page. There is no longer any per-row fan-out.
//
// This is the thin wrapper: it discards the Phase-1 timing split so the many
// non-handler callers (bulk re-render, single-row toggle, tests) stay unchanged.
// handlePlans calls planIndexEnrichTimed directly to get the section breakdown.
func (s *Server) planIndexEnrich(ctx context.Context, beads []*beadsapi.BeadDetail) []planRowView {
	rows, _ := s.planIndexEnrichTimed(ctx, beads)
	return rows
}

// planIndexEnrichTimed is planIndexEnrich with per-section wall-clock
// instrumentation (Phase 1, ka-0000005453): it MEASURES the split across the
// bulk joins so the actual hotspot is known, not guessed. The returned
// *planEnrichTiming is never nil. It carries two complementary breakdowns:
//
//   - Sections — the SEQUENTIAL phases: "wave1" (the independent joins),
//     "wave2" (the joins that depend on wave 1's results), and "assemble" (the
//     row-assembly pass). These run one after another (a barrier separates the
//     waves), so they sum to the enrich wall clock (Total()). Each wave's section
//     time is the slowest branch in that wave, not the sum of its joins.
//
//   - Joins — EACH individual bulk join's own wall clock (join_slugs,
//     join_assistants, join_stepdata, join_gardeners in wave 1; join_stalls,
//     join_escalations, join_crews, join_mrs in wave 2). These OVERLAP within a
//     wave (they fan out concurrently), so they do NOT sum to the wave time; they
//     name WHICH join dominates its wave. Read against PlanCount across pages, a
//     join that stays flat as plan count grows proves no per-row fan-out survived
//     in it, and a join that grows pinpoints exactly which join regressed.
//
// handlePlans stitches in the list-query time and emits the whole split as a
// Server-Timing header and a debug log line.
func (s *Server) planIndexEnrichTimed(ctx context.Context, beads []*beadsapi.BeadDetail) ([]planRowView, *planEnrichTiming) {
	timing := &planEnrichTiming{}
	lap := newEnrichLapper(timing)

	rows := make([]planRowView, 0, len(beads))
	ids := make([]string, 0, len(beads))
	// statusByEpic carries each plan epic's OWN raw status into the stall join so
	// a terminal (closed/deferred) plan is never flagged stalled — the false-stall
	// the audit surfaced (ka-0000005352 iteration): a closed plan with a leftover
	// un-closed child step rendered a loud ⚠ badge in the Closed lane.
	statusByEpic := make(map[string]string, len(beads))
	for _, b := range beads {
		if b == nil {
			continue
		}
		ids = append(ids, b.ID)
		statusByEpic[b.ID] = b.Status
	}
	// WAVE 1 — the four INDEPENDENT joins (Phase 3, ka-0000002624 + ka-0000004074).
	// None of these reads another's result, so they fan out under one errgroup and
	// the wall clock is the slowest branch rather than the sum of ~12 serial hops:
	//   - the /u/{slug} homepage-slug populate (mirrors handleActive; without it
	//     the human creator chip falls back to the ?operated_by= filter link),
	//   - the bulk assistant-state join (S4),
	//   - the batched children walk (S3) that yields BOTH the current/next step and
	//     the rendered-step progress rollup (ka-0000004410),
	//   - the ONE gardeners list (ka-0000004720) joining every plan epic to its bound
	//     plan gardener, SHARED between the unified loop+agent chip and the stall
	//     signal.
	// Each helper is best-effort — a daemon error degrades to an empty/zero result
	// and the card still renders — so no branch returns a non-nil error (that would
	// cancel its siblings via the shared context). Sharing one errgroup is safe
	// because every branch writes a DISTINCT result variable (or, for the slug
	// populate, a distinct per-bead field) that no sibling reads; the barrier at
	// g.Wait() is what wave 2 depends on.
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	var (
		assistants map[string]*beadsapi.BeadDetail
		stepData   map[string]*planIndexStepInfo
		gardeners  map[string]*beadsapi.BeadDetail
	)
	wave1, wave1ctx := errgroup.WithContext(ctx)
	wave1.SetLimit(planEnrichJoinConcurrency)
	wave1.Go(func() error {
		timing.timeJoin("join_slugs", func() { s.populateOperatedBySlugs(wave1ctx, beads) })
		return nil
	})
	wave1.Go(func() error {
		timing.timeJoin("join_assistants", func() { assistants = s.planAssistantBulk(wave1ctx, ids) })
		return nil
	})
	wave1.Go(func() error {
		timing.timeJoin("join_stepdata", func() { stepData = s.planIndexStepData(wave1ctx, beads) })
		return nil
	})
	wave1.Go(func() error {
		timing.timeJoin("join_gardeners", func() { gardeners = s.planGardenersByEpic(wave1ctx, want) })
		return nil
	})
	_ = wave1.Wait()
	lap.lap("wave1")

	// BARRIER — wave 2's candidate sets are derived from wave 1's stepData, so they
	// can only be built AFTER the wave1.Wait() above. escWalk is the escalation
	// candidate set (each plan's rendered steps + the epic itself); stepWalk is the
	// crew/MR candidate set (rendered step ids ONLY — a crew is `assigned` to a
	// step, never to the plan epic).
	escWalk := make(map[string][]string, len(ids))
	stepWalk := make(map[string][]string, len(ids))
	// crewStepMeta is the page-wide step-id → (number, title) map the crew-face
	// step tags read (ka-0000004037, iter-15) — folded from the per-plan Grid
	// planIndexStepData already walked, so it costs no extra daemon call. Step ids
	// are unique across plans (a step belongs to one plan), so one flat map is
	// unambiguous. It exists only for the crew join; the other wave-2 joins ignore it.
	crewStepMeta := make(map[string]crewStep)
	for _, id := range ids {
		cand := []string{id}
		if sd := stepData[id]; sd != nil {
			cand = append(cand, sd.RenderedStepIDs...)
			stepWalk[id] = sd.RenderedStepIDs
			for sid, meta := range gridStepMeta(sd.Grid) {
				crewStepMeta[sid] = meta
			}
		}
		escWalk[id] = cand
	}
	// blockedIDs is the SCOPED candidate set for the blocker-state join
	// (ka-0000005352 iter-55): only plans whose OWN raw status is "blocked" carry
	// a blocker facet, so the join walks just those — a non-blocked plan would
	// waste the walk. Empty ⇒ planBlockedBulk issues ZERO daemon calls (the common
	// case). Reads statusByEpic (built pre-wave1, no wave-1 dependency), but the
	// join runs in wave 2 alongside the other blocks-edge walks for parallelism.
	blockedIDs := make([]string, 0)
	for _, id := range ids {
		if strings.EqualFold(strings.TrimSpace(statusByEpic[id]), "blocked") {
			blockedIDs = append(blockedIDs, id)
		}
	}

	// WAVE 2 — the four joins that DEPEND on wave 1's results (Phase 3,
	// ka-0000004074). They read wave 1's stepData/assistants/gardeners (and the
	// stepData-derived escWalk/stepWalk) but nothing from each other, so they fan
	// out under a SECOND errgroup after the barrier:
	//   - the stall signal (ka-0000005581) — joins stepData + assistants + gardeners
	//     with one agent-liveness batch,
	//   - the blocked-by-escalation signal (epic ka-0000005480) — two bulk calls
	//     over escWalk (one forward blocks-edge batch, one blocker hydration batch),
	//   - the live-crew tally (ka-0000004801) — two bulk calls over stepWalk (one
	//     reverse assigned-edge batch, one crew-agent hydration batch),
	//   - the open-MR pills — two bulk calls over stepWalk (one forward
	//     produces-edge batch, one MR-bead hydration batch), mirroring planCrewBulk.
	// All four read wave 1's maps concurrently but never mutate them, and each
	// writes a DISTINCT result variable, so the wave is race-free. Best-effort as
	// before, so no branch propagates an error.
	var (
		stalls      map[string]planStallInfo
		escalations map[string]planEscalationInfo
		crews       map[string]planCrewRollup
		openMRs     map[string]planMRInfo
		blockers    map[string]planBlockerInfo
	)
	wave2, wave2ctx := errgroup.WithContext(ctx)
	wave2.SetLimit(planEnrichJoinConcurrency)
	wave2.Go(func() error {
		timing.timeJoin("join_stalls", func() { stalls = s.planStallBulk(wave2ctx, ids, statusByEpic, stepData, assistants, gardeners) })
		return nil
	})
	wave2.Go(func() error {
		timing.timeJoin("join_escalations", func() { escalations = s.planEscalationsBulk(wave2ctx, escWalk) })
		return nil
	})
	wave2.Go(func() error {
		// Blocker-state signal (ka-0000005352 iter-55) — one forward blocks-edge
		// batch + one blocker-hydration batch over the raw status=blocked plans
		// ONLY (blockedIDs); zero daemon calls when none is blocked.
		timing.timeJoin("join_blockers", func() { blockers = s.planBlockedBulk(wave2ctx, blockedIDs) })
		return nil
	})
	wave2.Go(func() error {
		timing.timeJoin("join_crews", func() { crews = s.planCrewBulk(wave2ctx, stepWalk, crewStepMeta) })
		return nil
	})
	wave2.Go(func() error {
		timing.timeJoin("join_mrs", func() { openMRs = s.planOpenMRsBulk(wave2ctx, stepWalk) })
		return nil
	})
	_ = wave2.Wait()
	lap.lap("wave2")

	for _, b := range beads {
		if b == nil {
			continue
		}
		row := planRowView{
			ID:                b.ID,
			Title:             b.Title,
			ShortTitle:        b.ShortTitle,
			Heraldry:          bearerArms(b),
			Badge:             beadBadge(b),
			Project:           projectFromLabels(b.Labels),
			Status:            b.Status,
			Priority:          b.Priority,
			PlanState:         string(planStateOf(b)),
			LifecycleChip:     string(planLifecycleChipState(planStateOf(b), b.Status)),
			OperatedBy:        b.OperatedBy,
			OperatedByDisplay: b.OperatedByDisplay,
			OperatedByEmail:   b.OperatedByEmail,
			OperatedBySlug:    b.OperatedBySlug,
			CreatedBy:         b.CreatedBy,
		}
		// Awaiting-approval wait-age on the READY lifecycle chip (ka-0000005352
		// iter-47). Ages on the epic's OWN updated_at (b.UpdatedAt) — the
		// parked-authoring ready plan's last write to the epic itself is the
		// `plan_state → ready` transition — DELIBERATELY not the folded lastActivity
		// the freshness stamp uses (that CTA-vs-freshness distinction is the whole
		// point; see planReadyWaitAge). Reads the reconciled LifecycleChip so a
		// terminal plan whose stale plan_state still says "ready" carries no wait-age
		// (planLifecycleChipState already suppressed the chip). Zero extra daemon call.
		row.ReadyWaitAge, row.ReadyWaitStale = planReadyWaitAge(planState(row.LifecycleChip), b.UpdatedAt, s.now())
		// "Last change" folds the epic's own updated_at with the newest activity
		// across its rendered steps/phases (ka-0000002873), matching the detail
		// header. The epic's own updated_at is the floor when it has no steps.
		lastActivity := b.UpdatedAt
		// lastShip is the newest step-CLOSE instant over the plan's rendered steps
		// (ka-0000005352 iter-59) — the progress-recency primitive for the "last ship
		// · Nd" facet. Unlike lastActivity it does NOT floor on the epic's own
		// updated_at: a plan that has closed no step has no delivery cadence to age,
		// so it stays zero (facet self-suppresses) rather than reading the epic touch.
		var lastShip time.Time
		if sd := stepData[b.ID]; sd != nil {
			if sd.StepsTotal > 0 {
				row.StepsTotal = sd.StepsTotal
				row.StepsClosed = sd.StepsClosed
				row.StepsOpen = sd.StepsTotal - sd.StepsClosed
				// The SAME percent primitive the detail header (planProgress) and the
				// per-phase chips (planPhases) use, so the index card % equals
				// /plans/{id} by construction, not by a hand-copied formula that could
				// drift (ka-0000007284).
				row.PctDone = planPercent(sd.StepsClosed, sd.StepsTotal)
			}
			// A TERMINAL plan (closed/deferred) renders NO current/next step
			// (ka-0000000857, iteration 25). CurrentOrNextStep drives two
			// live-work call-to-action affordances on the card — the "NEXT #N"
			// now/next chip (plan_card.html) and, for an operator, the actionable
			// "Dispatch #N →" button that cold-spawns a gardener onto that step. On
			// a finished (closed) or intentionally-parked (deferred) plan both are
			// a card-vs-truth contradiction — the mission's false-freshness family
			// (iter-19/23 killed the same shape for the ⚠ stall badge; iter-24 for
			// the "last change" stamp): a closed plan's leftover un-closed child
			// step is not work anyone is expected to pick up, yet the card told the
			// operator to dispatch it. Live proof (2026-08-06 pour): ka-0000000539 /
			// ka-0000005928 / ka-0000000492 / ka-0000002350 all closed yet each
			// rendered "next #N" + a Dispatch button. Suppress the projection at
			// the SAME single seam iter-7 used for the stall (planStatusTerminal),
			// so the page render AND the single-row re-render (renderPlanRowWith)
			// agree and the handler's own terminal guard (handlePlanRowDispatch)
			// can never be reached from a live button. Grid/progress/tally are
			// UNTOUCHED — a closed plan's task-grid + "N/N steps · 100%" are honest
			// history, not a call to act. Zero extra daemon cost (status is already
			// on the row).
			if !planStatusTerminal(b.Status) {
				row.CurrentOrNextStep = sd.NextStep
				// Current/next step wait-age (ka-0000005352 iter-43 + iter-48): fold
				// the step's age onto the now/next chip face — the temporal facet
				// every sibling wait-age already carries (decision · 32d, escalation
				// · 40d, stall · 3d, stuck/queued crew · 6h/· 12m, MR · 8d). setStepAge
				// keys off Kind(): the "now" arm gets its running-age ("how long has
				// this step been running", iter-43); the "blocked" arm gets its
				// wedge-age ("how long has this step been wedged", iter-48 — the arm
				// iter-43 deferred, whose wedge-age is surfaced by NO other card signal
				// because deriveStall counts only in_progress steps so the ⚠ badge
				// never fires for a blocked step). Both derive off the step's updatedAt
				// stamped at pick time via the SAME fmtTimeAgo + planStallStaleThreshold
				// the stall badge uses, so a long-grinding or long-wedged step reads its
				// age in the shared vocabulary and tints loud past the same 24h bar. The
				// "next" (ready, not-started) arm stays ageless — its wait is the plan
				// freshness stamp's + READY chip's job, and it is not wedged. Zero extra
				// daemon call — updatedAt is already on the step bead. Set on the SAME
				// single enrich seam as CurrentOrNextStep so the page render and the
				// single-row re-render (renderPlanRowWith) agree.
				row.CurrentOrNextStep.setStepAge(s.now())
			}
			row.Grid = sd.Grid
			// Phase-position milestone (ka-0000005352 iter-58): which phase of how
			// many a MULTI-PHASE plan is currently working, derived off the SAME
			// grid so it can never disagree with the phase rows the card renders.
			// Returns (0, 0) for a plan with fewer than two real phases, so the
			// template's `PhaseTotal >= 2` gate leaves a single-phase/flat plan
			// exactly as before. Applied on THIS enrich seam (like Grid/tally
			// above) so the page render and the single-row SSE re-render agree.
			// Not gated on terminal status: a closed multi-phase plan's "phase
			// M/M" is honest completed-milestone history (like the 100% bar beside
			// it), not a call to act — the same treatment the grid/progress rollup
			// already get on a terminal card (iter-25's note: "Grid/progress/tally
			// are UNTOUCHED — honest history").
			row.PhaseCur, row.PhaseTotal = planPhasePosition(sd.Grid)
			// Live step-status breakdown (ka-0000000851) off the SAME batched grid,
			// so the tally never disagrees with the progress bar or the timeline.
			tally := tallyPlanGrid(sd.Grid)
			row.StepsInProgress = tally.InProgress
			row.StepsBlocked = tally.Blocked
			// The not-started remainder (ka-0000007226): the still-open/deferred
			// backlog the segmented progress bar paints gray. StepsTotal minus the
			// three explicit buckets, clamped at 0 — the four segments then
			// partition StepsTotal exactly (see the field doc for the double-parent
			// edge that motivates the clamp). Derived, so it costs nothing.
			row.StepsNotStarted = planStepsNotStarted(row.StepsTotal, row.StepsClosed, row.StepsInProgress, row.StepsBlocked)
			// Ghost-done marker (ka-0000005352 iter-41): every rendered step closed
			// but the plan epic is still non-terminal — a plan that finished all its
			// steps but was never closed. Gated on !planStatusTerminal so a genuinely
			// CLOSED/DEFERRED plan's 100% bar stays honest history (no marker), and on
			// the in-flight buckets being empty so a double-parented grid step that
			// still tallies "▶/⛔" never renders a "done" marker that contradicts the
			// tally beside it. Derived off the rollup already on the row — zero fan-out.
			row.StepsAllDone = !planStatusTerminal(b.Status) &&
				row.StepsTotal > 0 && row.StepsClosed >= row.StepsTotal &&
				row.StepsInProgress == 0 && row.StepsBlocked == 0
			// Open-decision "N to answer" chip, folded from the SAME batched child
			// walk (Phase 2, ka-0000004522) — no per-row planOpenDecisionCount
			// round-trip. sd is nil only when the plan has neither steps nor open
			// decisions, in which case 0 is exactly right.
			row.OpenDecisions = sd.OpenDecisions
			// Carry the surfaced open-decision ids for the Layer-1 unhomed subtract
			// (plans_unhomed.go, ka-0000004983) — same batched walk, no extra call.
			row.openDecisionIDs = sd.OpenDecisionIDs
			// Oldest open-decision wait-age (ka-0000004999): fold the age into the
			// "N to answer" chip so the TOP attention signal carries the urgency
			// sortPlanRows already ranks on (oldest-waiting-first, the >30d tail).
			// Read off the decision beads this walk already hydrated — zero extra
			// daemon call. A fresh decision with an unset stamp leaves both empty,
			// so the chip reads exactly as before (count only).
			if !sd.OldestDecisionAt.IsZero() {
				row.OldestDecisionAge = fmtTimeAgo(sd.OldestDecisionAt, s.now())
				row.OpenDecisionStale = s.now().Sub(sd.OldestDecisionAt) >= planDecisionStaleThreshold
			}
			lastActivity = laterTime(lastActivity, sd.LastActivity)
			// Progress-recency: the newest step-close instant off the SAME batched
			// walk (ka-0000005352 iter-59), captured here for the "last ship · Nd"
			// facet computed just below (after FreshnessStale, which it gates on).
			lastShip = sd.LastShip
		}
		// Fold the plan's crew heartbeats into the freshness stamp (ka-0000004310,
		// iter-24): a working crew's updated_at moves every cycle while the STEP
		// task bead it works sits static until a status change, so a step claimed a
		// month ago whose crew is heartbeating right now rendered a pulsing green
		// "● N crew · M working" live chip BESIDE "updated 1 month ago" — a
		// card-vs-truth self-contradiction in the mission's false-freshness family
		// (the temporal twin of the iter-19/23 false-stall). crews[b.ID] is already
		// hydrated in wave2, so this is zero extra daemon call; laterTime keeps it
		// monotonic (a stale closed crew never regresses the stamp). This also
		// correctly re-freshens the within-tier attention rank (sortPlanRows reads
		// lastActivityAt) so a plan with live crew sorts as recently-active.
		lastActivity = laterTime(lastActivity, crews[b.ID].newestActivityAt)
		row.Ago = humanizeTime(lastActivity)
		row.lastActivityAt = lastActivity
		if !lastActivity.IsZero() {
			row.AgoRFC = lastActivity.UTC().Format(time.RFC3339)
			// Compact age for the head-row stamp (ka-0000005352 iter-52) — the
			// same "7h"/"2d" vocabulary the card's sibling wait-age facets use.
			// fmtTimeAgo tops out at a "d" unit, so a multi-week/month plan reads
			// e.g. "21d"; the precise instant + humanized long form still live in
			// the AgoRFC tooltip. Empty for an undatable plan (fmtTimeAgo's
			// zero-time contract), exactly like Ago.
			row.AgoCompact = fmtTimeAgo(lastActivity, s.now())
		}
		// Loud-tint the freshness stamp when an ACTIVE plan has gone stale
		// (ka-0000005352 iter-38) — the last card age-signal with no stale
		// escalation. Gated on the plan's OWN status so a terminal plan's honest
		// "3 weeks ago" history stays calm (planFreshnessStale). Derived off the
		// lastActivity just computed — zero extra daemon call.
		row.FreshnessStale = planFreshnessStale(b.Status, lastActivity, s.now())
		// Progress-recency "last ship · Nd" facet (ka-0000005352 iter-59): the
		// delivery-momentum twin of the freshness stamp above. Gated to the
		// non-redundant case — an in-flight plan that reads FRESH (FreshnessStale
		// false) yet has shipped no step in a while, with open steps still left to
		// ship — so it earns its ink only where no prior signal tells the story
		// (planLastShipAge documents each gate). Computed AFTER FreshnessStale (which
		// it reads) and StepsOpen (set in the sd block above) so both inputs are
		// final. Zero extra daemon call — lastShip rode the same batched child walk.
		row.LastShipAge, row.LastShipStale = planLastShipAge(b.Status, lastShip, row.StepsOpen, row.FreshnessStale, s.now())
		if a := assistants[b.ID]; a != nil {
			row.AssistantID = a.ID
			row.AssistantURL, row.AssistantExternal = s.assistantLinkURL(a)
			switch {
			case s.assistantIsLive(a):
				row.AssistantState = string(planAssistantStateLive)
			case assistantIsQueued(a):
				// Woken but spawn deferred on an exhausted pool — a persistent
				// capacity wait, surfaced distinctly on the index too so the card
				// doesn't read as idle parked (ka-0000003525).
				row.AssistantState = string(planAssistantStateQueued)
			default:
				row.AssistantState = string(planAssistantStateParked)
			}
		} else {
			row.AssistantState = string(planAssistantStateNone)
		}
		row.Stall = stalls[b.ID]
		// Blocker-state signal (ka-0000005352 iter-55): the forward blocks-edge
		// blocker rollup for a raw status=blocked plan. Zero value (no facet) for
		// every non-blocked plan — planBlockedBulk only keys the blocked ids. The
		// oldest-open wait-age + loud-tint are precomputed here off s.now() (the
		// escalation-age idiom) since the template cannot call OldestAge(now).
		row.Blocked = blockers[b.ID]
		row.BlockedOldestAge = row.Blocked.OldestAge(s.now())
		row.BlockedOldestStale = row.Blocked.OldestStale(s.now())
		row.Escalations = escalations[b.ID]
		// Oldest blocking-escalation wait-age (ka-0000003887): fold the age into
		// the ⛔ badge so the #2 attention signal carries the urgency
		// planAttentionEscalation ranks on (oldest-waiting-first). Projected off
		// the refs planEscalationsBulk already hydrated + sorted — zero extra
		// daemon call; a fresh escalation with an unset stamp leaves both empty,
		// so the badge reads exactly as before (count only).
		row.EscalationOldestAge = row.Escalations.OldestAge(s.now())
		row.EscalationStale = row.Escalations.OldestStale(s.now())
		// Oldest blocking-escalation operation type on the badge face
		// (iteration 37): name WHAT the operator must do, the badge's real
		// discriminator, beside the wait-age iter-8 folded on. Pure projection
		// off the refs planEscalationsBulk already hydrated + sorted — zero
		// extra daemon call; an escalation with no/unrecognized operation_type
		// leaves this empty, so the badge reads exactly as before (age + count).
		row.EscalationOldestOp = row.Escalations.OldestOperationLabel()
		row.Crew = crews[b.ID]
		// Quiet the LIVE-NOW crew signals on a TERMINAL (closed/deferred) plan
		// (iteration 30, ka-0000005352): a finished plan cannot have a crew
		// "working now", yet a zombie crew (an open agent bead still carrying an
		// alive agent_state whose heartbeat stopped when the plan closed but whose
		// bead was never reaped) was tallied Working, pulsed the green live-dot,
		// and rendered a live face — so the card read "● N crew · M working"
		// beside "updated 3 weeks ago". Same card-vs-truth family as the terminal
		// reconciles above (iter-25 CurrentOrNextStep, iter-27/28 lifecycle chip);
		// this is the crew LIVE signal, the last unreconciled forward/live facet
		// on a terminal plan card. Display-only: the folded crew reads "done", the
		// pulse + live faces go quiet, and the honest RECORD (Total, spend, the
		// failed/stuck-closed history) still renders — like the closed plan's
		// task-grid + "N/N steps · 100%" that stay honest history. Applied at the
		// SAME shared enrich seam iter-25 used so the page render AND the
		// single-row re-render (renderPlanRowWith) agree; zero extra daemon call
		// (status is already on the row). MUST come before reconcileStallWithCrew
		// so the stall reconcile reads the reconciled HasLive() — a terminal plan
		// with only a zombie "working" crew is no longer treated as live-crewed.
		row.Crew = row.Crew.reconcileTerminal(b.Status)
		// Reconcile the ⚠ stalled badge with the crew chip just set (iteration
		// 19, ka-0000005352): a gardener-paused/no-gardener stall is a false alarm
		// when the AUTHORITATIVE crew rollup says a crew is working the plan right
		// now — otherwise the same card renders "● N crew · M working" AND
		// "⚠ stalled · no gardener", contradicting itself. Uses the SAME
		// Crew.HasLive() the chip's live-dot reads, so the badge and the chip can
		// never disagree; zero extra daemon call (both are in hand post-wave2). A
		// dead-agent stall is untouched (an orphaned step under a dead agent is a
		// real stall even with other live crew).
		// crewQueued (iteration 51): a gardener-state stall is ALSO a false alarm
		// when the plan carries crew merely QUEUED in the pool — the gardener
		// dispatched them, so "· no gardener" contradicts the card's own
		// "● N crew · K queued" facet. Reads the SAME Idle tally that facet
		// renders (Crew.HasQueued()), so the badge and the chip agree. A dead-agent
		// orphan is untouched by queued crew (it stays gated on HasLive()).
		row.Stall = reconcileStallWithCrew(row.Stall, row.Crew.HasLive(), row.Crew.HasQueued())
		// Reconcile the ⚠ stalled badge with the ⛔ escalation badge just set
		// (iteration 26, ka-0000005352): a gardener-paused/no-gardener stall is a
		// redundant, contradictory face on a card ALREADY blocked by an open
		// escalation — the ⛔ badge (the harder stop that outranks stall) says a
		// privileged operator must act, while "· no gardener" says bind a gardener,
		// which cannot clear the escalation. Suppress the stall badge FACE for
		// that case (Stalled stays true: same lane, same ranker tier — zero extra
		// daemon call, row.Escalations already in hand). A dead-agent orphan is a
		// real system failure independent of the escalation, so it stays loud.
		row.Stall = reconcileStallWithEscalation(row.Stall, row.Escalations.Count > 0)
		// Reconcile the ⚠ stalled badge with the plan's PRE-BUILD lifecycle
		// (iteration 49, ka-0000005352): a gardener-paused/no-gardener stall is a
		// false alarm on a plan still in draft/ready — such a plan has no enabled
		// gardener BY DESIGN (gardenerEnableGatedByState, the R6 build gate: nothing
		// builds until "Approve & build"), so "· no gardener" implies binding a
		// gardener the lifecycle deliberately holds back, contradicting the
		// READY/DRAFT lifecycle chip + Approve&build button already on the card.
		// Suppress the stall badge FACE for that case (Stalled stays true: same
		// lane, same ranker tier — a ready plan already outranks stalled). Reads the
		// RAW plan_state (planStateOf) the R6 gate itself keys on; zero extra daemon
		// call (b is in hand). A dead-agent orphan is a real system failure
		// independent of the lifecycle, so it stays loud.
		row.Stall = reconcileStallWithLifecycle(row.Stall, gardenerEnableGatedByState(planStateOf(b)))
		// Quiet the loud MR-pill wait-age tint on a terminal plan (ka-0000005352
		// iter-45): a CLOSED/DEFERRED plan can carry a leftover child step with an
		// open, aged MR bead, so the iter-40 plan-card-mr-stale loud-amber tint fired
		// on a DONE plan while the plan's OWN freshness stamp (iter-38) correctly read
		// calm history on the same card. reconcileTerminal clears OpenStale so the
		// pill drops only the loud "act now" tint — its click-through, CI glyph, and
		// honest "· 8d" age still render as backward-looking history, like the closed
		// plan's task-grid + "N/N · 100%". The MR twin of planFreshnessStale (iter-38)
		// + ShowEscalationBadge (iter-39). Applied at the SAME shared enrich seam so
		// the page render and the single-row SSE re-render (renderPlanRowWith) agree;
		// zero extra daemon call (status is already on the row).
		row.MRs = openMRs[b.ID].reconcileTerminal(b.Status)
		// Unified plan-gardener loop + assistant liveness chip (ka-0000004720).
		// The loop state comes from the shared gardeners map via the SAME pure
		// helper the /plans/{id} cockpit uses (planGardenerLoopState), and the
		// agent facet is read back from row.AssistantState just set above — the
		// authoritative string the row's assistant chip renders — so the unified
		// chip can never disagree with the rendered assistant liveness (mirrors
		// planAgentPanelData reading the agent facet back off the lane bag). The
		// "waking" facet is a transient post-spawn signal set only on the detail
		// page's own POST/SSE path, so the index never shows it — but "queued"
		// (a woken spawn deferred on an exhausted pool, ka-0000003525) IS a
		// persistent state, so the index surfaces live/queued/parked/none, exactly
		// like planChipAgentState on a gardener-bar re-render.
		row.GardenerState = planGardenerLoopState(gardeners[b.ID])
		// Firing-mode glyph (Task 8, ka-0000006020): the SAME classifier the
		// cockpit uses, folded through the SAME chip helper, so the index card's
		// ⏱/⚡ glyph and the /plans/{id} card's glyph can never disagree. Empty for
		// a gardener-less plan (nil bead → "") — the shared partial then renders no
		// glyph.
		var firingMode string
		if row.GardenerState != planGardenerStateNone {
			row.GardenerID = gardeners[b.ID].ID
			firingMode = gardenerTriggerSubKind(gardeners[b.ID])
		}
		panelState := planAgentStateFrom(
			planGardenerView{State: row.GardenerState, FiringMode: firingMode},
			planAssistantState(row.AssistantState),
		)
		// Terminal-reconcile the plan-agent chip (ka-0000005352 iter-54): clear the
		// "not started" none-face on a closed/deferred plan so a FINISHED plan no
		// longer reads "closed · 100% · ✓ steps done · not started" at once. The last
		// unreconciled automation signal on a terminal card — the twin of the iter-27
		// lifecycle chip / iter-38 freshness / iter-39 escalation / iter-45 MR-pill /
		// iter-46 decision reconciles, applied at this SAME shared enrich seam so the
		// page render and the single-row SSE re-render agree. An ACTIVE gardener-less
		// plan keeps "not started" (honest — nothing drives it); a terminal plan with
		// a real loop word (retired/paused) keeps its honest word (see reconcileTerminal).
		row.Chip = planAgentChip(panelState).reconcileTerminal(b.Status)
		// Forward-looking next-fire facet on an armed-idle cron gardener (ka-0000005352
		// iter-56): fold "· fires in 12m" onto the chip when it reads idle (running
		// loop, no live iteration) AND the gardener fires on a clock. setNextFire is
		// gated internally on Tone=="idle" && FiringMode=="cron", so this is a no-op
		// for every other chip; the cron/tz come off the SAME gardener bead the chip's
		// firing-mode glyph already read (gardeners[b.ID]), so it costs zero extra
		// daemon call. Applied AFTER reconcileTerminal (a terminal plan's none-chip is
		// already cleared and is never "idle", so the two never both fire). Gated on a
		// bound gardener so a gardener-less plan reads exactly as before.
		if capBead := gardeners[b.ID]; capBead != nil {
			row.Chip = row.Chip.setNextFire(capBead.Fields["cron"], capBead.Fields["timezone"], s.now())
		}
		// Close-out signals (Matt's ask, thread ka-0000001785; decision
		// ka-0000001273 chose build-now — tasks ka-0000004467 + ka-0000005361).
		// The DAEMON already flags a fully-drained plan epic `autoclose-ready`
		// instead of hard-closing it (kbeads flagPlanEpicReady — the operator's
		// Mark-complete is the gate), and the viewer never read the label: the
		// signal was computed, commented, published, and thrown away, so the index
		// accumulated every plan ever made. Read it here off b.Labels — already in
		// hand, zero extra daemon call.
		//
		// TERMINAL-RECONCILED (the iter-25/27/38/41 family): a CLOSED/DEFERRED plan
		// is already done and lanes into Closed, so it carries no close-out flag
		// here — otherwise a retired plan would render "Done?" beside "closed",
		// the same card-vs-truth contradiction those iterations killed. (A closed
		// plan can genuinely still carry the label: the daemon stamps it at drain
		// time and never removes it on the later close.)
		row.CloseoutFlagged = !planStatusTerminal(b.Status) && hasLabel(b, planCloseoutReadyLabel)
		// Derive the badge LAST — it reads CloseoutFlagged just set, StepsAllDone,
		// StepsOpen, and the RAW PlanState (the legality gate must see the true
		// stored value, exactly as the board lane + Reopen lever do; a reconciled
		// display state would mis-gate the lever). Applied at this SAME shared
		// enrich seam so the page render, the single-row OOB re-render
		// (renderPlanRowWith), and the bulk response can never disagree.
		row.Closeout = planCloseoutBadgeFor(row)
		row.SearchText = planCardSearchText(row)
		rows = append(rows, row)
	}
	lap.lap("assemble")
	timing.PlanCount = len(ids)
	return rows, timing
}

// planCardSearchText builds the lowercased substring haystack for a plan card's
// client-side search (Matt's "progressive search bar" ask). Mirrors
// mapsCardSearchText: it folds the fields an operator would type to find a plan
// — title, bead id, project, status, the operator attribution (display name,
// email, slug, raw id, creating agent), and the current/next step's title —
// into one space-joined, lowercased string. Empty parts are skipped so the
// haystack carries no stray separators. Pure so it stays unit-testable.
func planCardSearchText(r planRowView) string {
	fields := []string{
		r.Title, r.ID, r.Project, r.Status,
		r.OperatedByDisplay, r.OperatedByEmail, r.OperatedBySlug, r.OperatedBy, r.CreatedBy,
	}
	if r.CurrentOrNextStep != nil {
		fields = append(fields, r.CurrentOrNextStep.Title)
	}
	parts := make([]string, 0, len(fields))
	for _, p := range fields {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// planIndexStepData derives, for every plan on the page in ONE reverse-dependency
// + children batch keyed by epic id, both the current-or-next step chip and the
// rendered-step progress rollup. The current/next precedence (S3):
//
//  1. an in_progress issue child (work happening right now) — lowest stable
//     number first; else
//  2. the lowest-numbered still-open issue child (what's next); else
//  3. nil (no open steps — the plan is done or has none, and the progress bar
//     already tells that story).
//
// Deliberately NOT planTimeline(): that adds the MR resolver + blocker walk per
// step, which across up to plansListLimit plans is an N× fan-out. Here we only
// read the children's status + stable creation-order number, which is all the
// compact card chip needs. Best-effort: a daemon error drops the plan from the
// map (its card renders with no current/next chip and no progress chip).
//
// The SAME batched child walk yields the card's progress rollup (Total/Closed
// over the RENDERED step set — ka-0000004410), so the index card counts exactly
// the rows the detail timeline shows, and index % == detail %. This replaces the
// former per-row deriveOwnedEpicRollup CountOnly pair (a transitive subtree count
// that over-counted non-rendered issue descendants), removing 2 daemon calls per
// plan.
//
// Phase descent (ka-0000005926): when a plan declares phase sub-epics
// (fields.phase_order), its steps are GRANDCHILDREN under the phases, so a
// second tier is walked. Both tiers are ONE batched round-trip each across the
// whole page (a reverse-deps + get over the union of all phase sub-epics), so
// the index stays a bounded number of daemon calls regardless of plan count.
func (s *Server) planIndexStepData(ctx context.Context, epics []*beadsapi.BeadDetail) map[string]*planIndexStepInfo {
	out := make(map[string]*planIndexStepInfo, len(epics))
	epicIDs := make([]string, 0, len(epics))
	orderByEpic := make(map[string][]string, len(epics))
	// epicByID lets the per-plan step pick read the plan epic's own
	// fields.step_order (the flat-step overlay) so the index chip's #N matches
	// the detail timeline (ka-0000007445 / ka-0000007589).
	epicByID := make(map[string]*beadsapi.BeadDetail, len(epics))
	for _, b := range epics {
		if b == nil {
			continue
		}
		epicIDs = append(epicIDs, b.ID)
		orderByEpic[b.ID] = planPhaseOrder(b)
		epicByID[b.ID] = b
	}
	if len(epicIDs) == 0 {
		return out
	}
	// Tier 1: reverse deps for every plan epic in one batch → direct child ids.
	revByEpic, err := s.daemon.BatchGetDependencies(ctx, epicIDs, "reverse")
	if err != nil {
		s.logger.Debug("plans index: batch reverse deps", "error", err)
		return out
	}
	// Child-type hint from the reverse-dep rows (ka-0000003113): the daemon
	// stamps each parent-child edge's DEPENDENT (child) type onto BeadType, so
	// we can skip hydrating children the walk provably discards. A plan accrues
	// many parent-child children that are NOT steps — sessions, messages, its
	// bound plan-gardener agent, docs, bundles — and the walk below keeps only
	// issue/phase/decision children; hydrating the rest as full bead records
	// (fields + description) was ~68% wasted payload on the /plans index. First
	// non-empty type per id wins; "" means the daemon did not report a type
	// (older daemon, or a dangling/soft-deleted edge) and is treated as unknown.
	childTypeByID := make(map[string]string)
	for _, epicID := range epicIDs {
		for _, dep := range revByEpic[epicID] {
			if dep.Type != "parent-child" || dep.BeadType == "" {
				continue
			}
			if _, ok := childTypeByID[dep.BeadID]; !ok {
				childTypeByID[dep.BeadID] = dep.BeadType
			}
		}
	}
	childIDsByEpic := make(map[string][]string, len(epicIDs))
	unionIDs := make([]string, 0, len(epicIDs)*4)
	seen := make(map[string]struct{})
	for _, epicID := range epicIDs {
		ids := dedupedParentChildIDs(revByEpic[epicID])
		// childIDsByEpic keeps the FULL child-id list unchanged; only the
		// hydration union is trimmed. A child we skip hydrating is absent from
		// the `children` map, and every downstream consumer already guards a nil
		// lookup and type-gates before use — so a skipped non-step child yields
		// the exact same walk result it does today when hydrated-then-discarded.
		childIDsByEpic[epicID] = ids
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			// Hydrate step-relevant children (issue/phase/decision) and any child
			// whose type the daemon did not report (unknown ⇒ never drop a real
			// step). Known non-step children are skipped.
			if planStepChildHydrate(childTypeByID[id]) {
				unionIDs = append(unionIDs, id)
			}
		}
	}
	if len(unionIDs) == 0 {
		return out
	}
	children, err := s.daemon.BatchGetBeads(ctx, unionIDs)
	if err != nil {
		s.logger.Debug("plans index: batch get children", "error", err)
		return out
	}

	// Tier 2: for plans with phase_order, batch-descend into the phase
	// sub-epics that resolve to a direct-child epic. One reverse-deps + one
	// get over the union of all phase sub-epics across the whole page.
	phaseSubIDs := make([]string, 0)
	phaseSubSeen := make(map[string]struct{})
	for _, epicID := range epicIDs {
		for _, pid := range orderByEpic[epicID] {
			c := children[pid]
			if !isPhaseContainer(c) {
				continue
			}
			if _, dup := phaseSubSeen[pid]; dup {
				continue
			}
			phaseSubSeen[pid] = struct{}{}
			phaseSubIDs = append(phaseSubIDs, pid)
		}
	}
	grandchildIDsBySub := make(map[string][]string, len(phaseSubIDs))
	var grandchildren map[string]*beadsapi.BeadDetail
	if len(phaseSubIDs) > 0 {
		if revBySub, derr := s.daemon.BatchGetDependencies(ctx, phaseSubIDs, "reverse"); derr == nil {
			gUnion := make([]string, 0)
			gSeen := make(map[string]struct{})
			for _, sub := range phaseSubIDs {
				ids := dedupedParentChildIDs(revBySub[sub])
				grandchildIDsBySub[sub] = ids
				for _, id := range ids {
					if _, dup := gSeen[id]; dup {
						continue
					}
					gSeen[id] = struct{}{}
					gUnion = append(gUnion, id)
				}
			}
			if len(gUnion) > 0 {
				if g, gerr := s.daemon.BatchGetBeads(ctx, gUnion); gerr == nil {
					grandchildren = g
				}
			}
		}
	}

	for _, epicID := range epicIDs {
		// Ordered candidate step set: for a phased plan, walk phase sub-epics in
		// phase_order (their issue children), then the unphased direct children.
		// For an unphased plan this is just the direct children — identical to
		// the pre-phase behavior. The same data yields the rendered-step rollup.
		order := orderByEpic[epicID]
		direct := childIDsByEpic[epicID]
		total, closed, lastActivity, lastShip := renderedStepRollup(order, direct, grandchildIDsBySub, children, grandchildren)
		st := pickPlanCurrentOrNextStep(epicByID[epicID], order, direct, grandchildIDsBySub, children, grandchildren)
		// Open-decision tally off the SAME batched child walk (Phase 2,
		// ka-0000004522): clarifying-question decisions are parent-child children
		// of the plan epic (tier-1) or, rarely, a phase sub-epic (tier-2), both of
		// which the reverse-deps walk above already fetched into children /
		// grandchildren. Counting them here removes the former per-row
		// planOpenDecisionCount CountOnly round-trip (up to plansListLimit serial
		// daemon calls) at ZERO new round-trip cost — the N+1 this phase kills.
		directDecs, directOldest, directDecIDs := countOpenDecisionChildren(direct, children)
		phaseDecs, phaseOldest, phaseDecIDs := countPhaseDecisionGrandchildren(order, direct, grandchildIDsBySub, grandchildren)
		openDecs := directDecs + phaseDecs
		// The open-decision ids from both tiers — the SAME parent-child membership
		// the count above tallies — feed the Layer-1 unhomed backstop's subtract
		// (plans_unhomed.go, ka-0000004983) off this same batched walk, so the
		// "already surfaced on this plan" set never drifts from the rendered count.
		openDecIDs := append(append([]string(nil), directDecIDs...), phaseDecIDs...)
		// The oldest open-decision creation instant across both tiers — the
		// wait-age the card surfaces on the "N to answer" chip (ka-0000004999).
		// Zero when the plan has no open decision.
		oldestDecisionAt := earlierNonZeroTime(directOldest, phaseOldest)
		// Keep the entry when the plan has open decisions even if it renders no
		// steps yet — otherwise the "N to answer" chip would be lost for a
		// question-only plan (a draft with a clarifying question but no steps).
		if st == nil && total == 0 && lastActivity.IsZero() && openDecs == 0 {
			continue
		}
		out[epicID] = &planIndexStepInfo{
			NextStep:         st,
			StepsTotal:       total,
			StepsClosed:      closed,
			OpenDecisions:    openDecs,
			OpenDecisionIDs:  openDecIDs,
			OldestDecisionAt: oldestDecisionAt,
			LastActivity:     lastActivity,
			LastShip:         lastShip,
			InProgressSteps:  collectInProgressStepRefs(order, direct, grandchildIDsBySub, children, grandchildren),
			RenderedStepIDs:  collectRenderedStepIDs(order, direct, grandchildIDsBySub, children, grandchildren),
			Grid:             buildPlanGrid(epicByID[epicID], order, direct, grandchildIDsBySub, children, grandchildren),
		}
	}
	return out
}

// planIndexStepInfo is the per-plan projection planIndexStepData derives from
// the shared batched child walk: the current-or-next step chip AND the progress
// rollup over the RENDERED step set (ka-0000004410). Both come from one walk, so
// the index card's chip and progress bar are consistent with each other and with
// the detail page. NextStep is nil when no step is open; StepsTotal is 0 when the
// plan renders no timeline rows (its progress chip then suppresses itself).
type planIndexStepInfo struct {
	NextStep    *planCardStep
	StepsTotal  int
	StepsClosed int
	// OpenDecisions is the count of open clarifying-question decisions parented
	// to the plan (tier-1 direct children) or its phase sub-epics (tier-2
	// grandchildren), derived from the SAME batched child walk this struct is
	// built from (Phase 2, ka-0000004522). It replaces the former per-row
	// planOpenDecisionCount CountOnly round-trip, so the "N to answer" card chip
	// costs no extra daemon call.
	OpenDecisions int
	// OpenDecisionIDs are the bead ids of those same open decisions (both tiers) —
	// the parent-child membership the Layer-1 unhomed backstop subtracts as
	// "already surfaced on this plan" (plans_unhomed.go, ka-0000004983). Collected
	// on the SAME batched child walk as the count, so it adds no daemon round-trip
	// and can never drift from what the "N to answer" chip counts.
	OpenDecisionIDs []string
	// OldestDecisionAt is the creation instant of the OLDEST open decision across
	// both tiers (ka-0000004999) — the wait-age the card surfaces on the "N to
	// answer" chip so an operator can tell a decision rotting >30 days from a
	// fresh one (the tail sortPlanRows drains oldest-first). Read from the SAME
	// decision beads this walk already hydrated (BeadDetail.CreatedAt), so it
	// costs no extra daemon call. Zero when the plan has no open decision.
	OldestDecisionAt time.Time
	// LastActivity is the newest updated_at over the plan's rendered step set
	// and phase sub-epics (ka-0000002873). The card folds it with the epic's own
	// updated_at to show a "last change" stamp that tracks real child activity,
	// not the epic's rarely-moving own timestamp.
	LastActivity time.Time
	// LastShip is the newest step-CLOSE instant over the plan's rendered step set
	// (ka-0000005352 iter-59) — the newest closed_at (updated_at fallback) over the
	// closed steps. It feeds the card's "last ship" progress-recency facet, which
	// answers "when did this plan last CLOSE a step" as distinct from LastActivity's
	// "when was this plan last TOUCHED". The two diverge precisely on the delivery-
	// drought case: a plan whose crew heartbeat and comments keep LastActivity fresh
	// (below the freshness-stamp bar → calm) while it has closed no step in weeks.
	// Zero when the plan has closed no rendered step (a plan with only open steps
	// reads no facet). Derived off the SAME batched child walk — no extra daemon call.
	LastShip time.Time
	// InProgressSteps carries the in_progress rendered steps' assignee +
	// last-moved stamps, feeding the stalled-plan badge's dead-agent check
	// (planStallBulk, ka-0000005581) off the same batched walk.
	InProgressSteps []stallStepRef
	// RenderedStepIDs are the rendered steps' bead ids — the blocked-work
	// candidate set the escalation aggregation walks (planEscalationsBulk,
	// epic ka-0000005480), derived from the same batched children.
	RenderedStepIDs []string
	// Grid is the per-phase task-square grid rendered on the card (buildPlanGrid,
	// ka-0000001226), derived from the same batched children as the rollup.
	Grid []planGridPhase
}

// renderedStepRollup counts the steps the /plans cockpit timeline actually
// renders for a plan — the denominator the index card's progress chip must
// match, and the same set the detail page's planProgress counts (ka-0000004410).
// It mirrors planTimeline + planPhases membership EXACTLY over the same batched
// child data planIndexStepData already walked, so it costs no extra round-trip:
//
//   - phase steps: each phase sub-epic (a direct child epic listed in
//     phase_order) contributes its OWN direct issue-type children;
//   - flat steps: direct issue-type children NOT claimed by a phase sub-epic.
//
// It deliberately does NOT descend the full transitive subtree the way
// deriveOwnedEpicRollup does: issue-kind grandchildren under a step, and the
// descendants of a freestanding (non-phase) sub-plan epic, are counted by that
// rollup but never rendered as timeline rows. Counting them was the
// 23/27-header-vs-25-rendered-rows divergence this replaces. Returns (total,
// closed) over the rendered set, plus lastActivity — the newest updated_at over
// that rendered set AND the phase sub-epics themselves (ka-0000002873), the same
// fold the detail page's planLastActivity applies over its view structs, so the
// index card and the detail header show the same "last change" instant.
func renderedStepRollup(order, directChildIDs []string, grandchildIDsBySub map[string][]string, direct, grandchildren map[string]*beadsapi.BeadDetail) (total, closed int, lastActivity, lastShip time.Time) {
	// phaseStepIDs mirrors planPhases: the union of issue-type step ids claimed
	// by any phase. planTimeline's flat list excludes them so a step that is both
	// a phase child and a direct child renders (and counts) only under the phase.
	// Within the phase pass we count per phase WITHOUT cross-phase dedupe — that
	// matches planPhases (an independent buildStepRows per phase) and planProgress
	// (a sum of per-phase StepsTotal), so a step double-parented across two phases
	// renders — and counts — twice in both the index and the detail.
	phaseStepIDs := make(map[string]struct{})
	phaseSet := make(map[string]struct{}, len(order))
	for _, pid := range order {
		c := direct[pid]
		if !isPhaseContainer(c) {
			continue // a phase_order id that isn't a current direct-child container
		}
		if _, dup := phaseSet[pid]; dup {
			continue
		}
		phaseSet[pid] = struct{}{}
		// The phase sub-epic's OWN updated_at counts as plan activity (a status
		// flip on the phase itself), mirroring planPhaseView.UpdatedAt on the
		// detail side.
		lastActivity = laterTime(lastActivity, c.UpdatedAt)
		t, cl, la, ls := countIssueChildren(grandchildIDsBySub[pid], grandchildren, phaseStepIDs)
		total += t
		closed += cl
		lastActivity = laterTime(lastActivity, la)
		lastShip = laterTime(lastShip, ls)
	}
	// Flat steps: direct children that are neither a phase sub-epic nor a step
	// already claimed by a phase — exactly planTimeline's flatIDs set.
	unphased := make([]string, 0, len(directChildIDs))
	for _, cid := range directChildIDs {
		if _, isPhase := phaseSet[cid]; isPhase {
			continue
		}
		if _, inPhase := phaseStepIDs[cid]; inPhase {
			continue
		}
		unphased = append(unphased, cid)
	}
	t, cl, la, ls := countIssueChildren(unphased, direct, nil)
	return total + t, closed + cl, laterTime(lastActivity, la), laterTime(lastShip, ls)
}

// countIssueChildren tallies (total, closed) over the issue-type beads among
// ids, resolved against beads — the same nil + isIssueType filter buildStepRows
// applies, so the count equals the number of rows buildStepRows would emit for
// that id set. When record is non-nil, each counted issue id is added to it (a
// repeat is still counted) so the caller can later EXCLUDE those ids from the
// flat pass, mirroring planPhases's phaseStepIDs / planTimeline's flat dedupe.
// lastActivity is the newest updated_at over the counted children, folded into
// the plan's "last change" stamp (ka-0000002873). lastShip is the newest CLOSE
// instant over the counted children — the newest closed_at (its own updated_at
// as the fallback when closed_at is absent/unparseable) over the closed steps —
// folded into the plan's "last ship" progress-recency facet (ka-0000005352
// iter-59). It is deliberately distinct from lastActivity: a step's updated_at
// moves on any edit/comment/reopen, so lastActivity tracks "last touched", while
// lastShip advances ONLY when a step actually reaches closed — "last forward
// progress". The two diverge exactly on the delivery-drought case the facet
// surfaces (a plan churning crew/comments for weeks while closing no step).
func countIssueChildren(ids []string, beads map[string]*beadsapi.BeadDetail, record map[string]struct{}) (total, closed int, lastActivity, lastShip time.Time) {
	for _, id := range ids {
		c := beads[id]
		if c == nil || !isIssueType(c.Type) {
			continue
		}
		if record != nil {
			record[id] = struct{}{}
		}
		total++
		if c.Status == "closed" {
			closed++
			// Newest step-close instant: prefer the immutable closed_at stamp;
			// fall back to the step's updated_at (which, for a closed step never
			// re-edited, IS its close instant). A closed step with neither datable
			// contributes nothing, exactly as the facet's undatable-reads-as-before
			// contract requires.
			shipAt := c.UpdatedAt
			if at, ok := parseThreadClosedAt(c.ClosedAt); ok {
				shipAt = at
			}
			lastShip = laterTime(lastShip, shipAt)
		}
		lastActivity = laterTime(lastActivity, c.UpdatedAt)
	}
	return total, closed, lastActivity, lastShip
}

// countOpenDecisionChildren tallies the open clarifying-question decisions among
// a plan's DIRECT children (tier-1) — the common case, since a plan's decisions
// are parent-child children of the plan epic itself. It reads the already-fetched
// children map (the reverse-deps + get planIndexStepData walked for the whole
// page), so it adds no daemon round-trip. This is the batch equivalent of the
// former per-row planOpenDecisionCount(Parent=epicID) CountOnly query for the
// direct-child tier (Phase 2, ka-0000004522 / ka-0000006056).
//
// It also returns the ids of those open decisions — the SAME parent-child
// membership the "N to answer" chip counts — so the Layer-1 unhomed backstop
// (plans_unhomed.go, ka-0000004983) can subtract the already-surfaced set off
// the same walk, never a re-query that could drift from the rendered count.
func countOpenDecisionChildren(directChildIDs []string, children map[string]*beadsapi.BeadDetail) (int, time.Time, []string) {
	n := 0
	var oldest time.Time
	var ids []string
	for _, id := range directChildIDs {
		c := children[id]
		if c != nil && c.Type == "decision" && c.Status == "open" {
			n++
			oldest = earlierNonZeroTime(oldest, c.CreatedAt)
			ids = append(ids, c.ID)
		}
	}
	return n, oldest, ids
}

// countPhaseDecisionGrandchildren tallies open decisions parented to a plan's
// PHASE sub-epics (tier-2) — the rare phased-plan case where a clarifying
// question hangs off a phase rather than the plan epic (ka-0000007691). It reads
// the same batched grandchildren map planIndexStepData already fetched for phased
// plans, so it too costs no extra round-trip. Only phase_order ids that resolve
// to a current direct-child epic are descended, mirroring the step rollup's
// phase-membership rule, so a stale phase id never double-counts.
func countPhaseDecisionGrandchildren(order, directChildIDs []string, grandchildIDsBySub map[string][]string, grandchildren map[string]*beadsapi.BeadDetail) (int, time.Time, []string) {
	if len(grandchildren) == 0 {
		return 0, time.Time{}, nil
	}
	direct := make(map[string]struct{}, len(directChildIDs))
	for _, id := range directChildIDs {
		direct[id] = struct{}{}
	}
	n := 0
	var oldest time.Time
	var ids []string
	seen := make(map[string]struct{}, len(order))
	for _, pid := range order {
		if _, ok := direct[pid]; !ok {
			continue // a phase_order id that is no longer a direct child
		}
		if _, dup := seen[pid]; dup {
			continue
		}
		seen[pid] = struct{}{}
		for _, gid := range grandchildIDsBySub[pid] {
			c := grandchildren[gid]
			if c != nil && c.Type == "decision" && c.Status == "open" {
				n++
				oldest = earlierNonZeroTime(oldest, c.CreatedAt)
				ids = append(ids, c.ID)
			}
		}
	}
	return n, oldest, ids
}

// laterTime returns the later of two instants. Zero-safe: a zero time never
// wins over a non-zero one, so folding a set of updated_at stamps (some of
// which may be unset) yields the newest real one.
func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// earlierNonZeroTime returns the earlier of two instants, treating a zero time
// as "unset" so it never wins — folding a set of created_at stamps (some of
// which may be unset) over this yields the OLDEST real one. It is the min-side
// mirror of laterTime, used to find the oldest open decision's wait-age
// (ka-0000004999) without a zero stamp spuriously reading as "waiting since the
// epoch".
func earlierNonZeroTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if b.Before(a) {
		return b
	}
	return a
}

// planLastActivity returns the most recent modification instant across the plan
// epic and every step/phase the timeline renders — the plan's true "last
// change" signal (ka-0000002873). The epic's OWN updated_at barely moves once a
// plan is drafted (all the churn is in the child steps and phases), so a stamp
// built from it alone reads like a stale creation time — the exact complaint
// this fixes. Folding in the rendered children makes the stamp reflect the
// newest actual activity.
//
// It reads only the already-materialized view structs, so it costs no extra
// daemon call, and it folds over EXACTLY the rendered set the /plans index also
// folds (renderedStepRollup: epic + phase sub-epics + rendered issue children),
// so the index card and the detail header show the same instant for one plan.
func planLastActivity(epicUpdatedAt time.Time, steps []planStepView, phases []planPhaseView) time.Time {
	latest := epicUpdatedAt
	for _, st := range steps {
		latest = laterTime(latest, st.UpdatedAt)
	}
	for _, p := range phases {
		latest = laterTime(latest, p.UpdatedAt)
		for _, st := range p.Steps {
			latest = laterTime(latest, st.UpdatedAt)
		}
	}
	return latest
}

// pickPlanCurrentOrNextStep applies the S3 current/next precedence across a
// plan's candidate GROUPS in precedence order — each phase (in phase_order)
// then the unphased remainder. When there are no phases it reduces to
// pickCurrentOrNextStep over the direct children.
//
// Each group is reduced with the SAME per-group numbering the detail page uses
// (buildStepRows numbers steps 1..N WITHIN a phase), so the chip's `#N` matches
// the number that step shows on /plans/<id>. We deliberately do NOT concatenate
// every phase's steps into one globally-sorted list and renumber — that made
// the chip show a step's global rank (e.g. "#4") while the detail page showed
// its per-phase rank ("#2") for the same step (ka-0000005926 review). Numbering
// per group also isolates a group whose grandchildren failed to resolve: it
// yields nil for that group and we move on, rather than dropping every phase
// step from consideration.
//
// Precedence: the first in_progress step in phase order wins ("now"); absent
// any, the first still-open step in phase order ("next"). in_progress in a
// later phase still beats a not-started step in an earlier phase, matching the
// pre-phase "pass-1 in_progress before pass-2 open" contract.
// planEpic is the plan epic bead itself: its fields.step_order orders the
// UNPHASED flat steps (the phased steps are ordered by each phase sub-epic's
// own step_order — read from direct[pid] below), mirroring how buildStepRows
// sources the two levels (ka-0000007445). Nil is tolerated (no reorder overlay).
func pickPlanCurrentOrNextStep(planEpic *beadsapi.BeadDetail, order, directChildIDs []string, grandchildIDsBySub map[string][]string, direct, grandchildren map[string]*beadsapi.BeadDetail) *planCardStep {
	if len(order) == 0 {
		return pickCurrentOrNextStep(directChildIDs, direct, planStepOrder(planEpic))
	}
	phaseSet := make(map[string]struct{}, len(order))
	var firstOpen *planCardStep
	consider := func(card *planCardStep) *planCardStep {
		if card == nil {
			return nil
		}
		if card.Status == "in_progress" {
			return card // "now" — highest precedence, return immediately
		}
		if firstOpen == nil {
			firstOpen = card
		}
		return nil
	}
	for _, pid := range order {
		c := direct[pid]
		if !isPhaseContainer(c) {
			continue
		}
		phaseSet[pid] = struct{}{}
		// A phase's steps are ordered by the PHASE sub-epic's own step_order.
		if now := consider(pickCurrentOrNextStep(grandchildIDsBySub[pid], grandchildren, planStepOrder(c))); now != nil {
			return now
		}
	}
	// Unphased leftovers: direct issue children not claimed by a phase, ordered
	// by the plan epic's own step_order.
	unphased := make([]string, 0, len(directChildIDs))
	for _, cid := range directChildIDs {
		if _, isPhase := phaseSet[cid]; isPhase {
			continue // a phase sub-epic itself is not a step
		}
		unphased = append(unphased, cid)
	}
	if now := consider(pickCurrentOrNextStep(unphased, direct, planStepOrder(planEpic))); now != nil {
		return now
	}
	return firstOpen
}

// pickCurrentOrNextStep applies the S3 precedence over one plan's direct issue
// children. childIDs are in the epic's reverse-dep edge order; the stable step
// Number is assigned 1..N over the SAME display order buildStepRows uses
// (explicit step_order overlay first, then creation order — sortStepBeads) so
// it matches the detail-page numbering (ka-0000007589). stepOrder is the parent
// epic's fields.step_order (nil ⇒ pure creation order, the pre-reorder
// behavior). Uses an explicit two-pass form (in_progress first, then
// lowest-open) rather than a status-sorted query so a re-opened middle step
// never masquerades as "current".
func pickCurrentOrNextStep(childIDs []string, children map[string]*beadsapi.BeadDetail, stepOrder []string) *planCardStep {
	// Materialize the issue children in the same display order buildStepRows
	// uses so Number is deterministic and matches the detail page's per-group
	// numbering.
	steps := make([]*beadsapi.BeadDetail, 0, len(childIDs))
	for _, id := range childIDs {
		c := children[id]
		if c == nil || !isIssueType(c.Type) {
			continue
		}
		steps = append(steps, c)
	}
	if len(steps) == 0 {
		return nil
	}
	// The SAME ordering primitive the detail timeline uses (explicit step_order,
	// then creation order + ID tie-break) — the two daemon child endpoints have
	// no ORDER BY, so this keeps #N from transposing between index and detail.
	sortStepBeads(steps, stepOrder)
	// Pass 1: the first in_progress step (work happening now).
	// Pass 2: the first still-open step (what's next).
	var firstOpen *planCardStep
	for i, c := range steps {
		if c.Status == "closed" {
			continue
		}
		card := &planCardStep{ID: c.ID, Number: i + 1, Title: c.Title, ShortTitle: c.ShortTitle, Status: c.Status, updatedAt: c.UpdatedAt}
		if c.Status == "in_progress" {
			return card
		}
		if firstOpen == nil {
			firstOpen = card
		}
	}
	return firstOpen
}

// planCreatorChip renders the compact "who created / owns this plan" chip for
// one index card (S2, ka-0000002728). It reuses the shared operator-chip
// resolver so the mine-vs-not-mine + human-vs-agent skin matches every other
// surface, but rewrites the filter href to the /plans index (not /search) so a
// click scopes THIS surface by that operator — parity with the S1 header chip's
// ?operated_by= links. Compact (initials) to keep the card meta row tidy.
//
// Registered as the `planCreatorChip` template func; the template calls
//
//	{{planCreatorChip . $.BasePath $.ViewerEmail}}
func planCreatorChip(row planRowView, basePath, viewerEmail string) template.HTML {
	return planCreatorChipScoped(row, basePath, viewerEmail, "")
}

// planCreatorChipScoped is planCreatorChip with the board's active operator
// scope (ka-0000005352 iter-61): the resolved ?operated_by= value the /plans
// list query is filtered to (the page dot's .EffectiveOperatedBy). When the
// card's own resolved operator matches that scope, the chip is SUPPRESSED —
// it returns the empty string, exactly like resolveOperatorChip's "self" /
// "suppressed" kinds already do.
//
// Why: the /plans index is default-scoped to the viewer's OWN operator id (the
// S1 "filter-by-me by default" cold load — handlePlans applies
// resolveDefaultOperatedBy when the URL carries no operated_by key), and a
// ?operated_by=X link scopes it to one operator. On such a board EVERY rendered
// card shares that operator, the header chip already reads "Showing your N
// plans" / "Showing work operated by X", and this per-card chip's own filter
// href (/plans?operated_by=X) points right back at the same scoped page — so
// the chip is pure redundant ink AND a dead-end self-link. Suppress it there —
// the priority-pill / project-chip (iter-60) "render only where the value
// discriminates" density discipline, in the mission's "more info in LESS space"
// spirit: every card reclaims the chip's width from the meta row (which the
// latest audit flagged as near its wrapping ceiling), with zero information loss
// (the scope operator is named once in the header chip).
//
// scope is the resolved operator id the board is filtered to ("" on an
// unscoped/"all"/anonymous board). It is compared against the card's own
// resolved operator RawID (both live in the same canonical operator-id space:
// the epic's stamped operated_by and the resolveDefaultOperatedBy /
// resolveOperatedByParam scope), case-insensitively — so an oidc:/slack: id
// matches regardless of header casing. An UNSCOPED board (empty scope) renders
// the chip exactly as before — there the creator genuinely discriminates a
// mixed board. A card whose operator DIFFERS from the scope also keeps its
// (discriminating) chip: a defensive guard, since a scoped page never lists a
// card whose operator differs, but if one ever did its chip still renders.
func planCreatorChipScoped(row planRowView, basePath, viewerEmail, scope string) template.HTML {
	// Resolve with an EMPTY basePath so the resolver does not stamp a
	// /search filter href; we set the /plans-scoped href ourselves below.
	chip := resolveOperatorChip(row.OperatedBy, row.OperatedByDisplay, row.OperatedByEmail, row.OperatedBySlug, row.CreatedBy, "", viewerEmail)
	// Suppress a redundant self-referential chip on a board already scoped to
	// this operator (iter-61). Gate on a non-empty resolved RawID so an
	// unattributed card (Kind empty/scheduler, RawID "") never matches an empty
	// scope and is left to render its em-dash exactly as before.
	if s := strings.TrimSpace(scope); s != "" && chip.RawID != "" && strings.EqualFold(chip.RawID, s) {
		return ""
	}
	// Point the filter link at the /plans index so clicking a creator scopes
	// the plans list to their plans (parity with the S1 header chip), rather
	// than bouncing to /search. basePath may legitimately be "" (root-mounted
	// viewer), so gate only on a resolved operator id. Slug-bearing human chips
	// still prefer /u/{slug} inside renderOperatorChipCompact; this only affects
	// the FilterHref fallback (agent chips and slug-less humans).
	if chip.RawID != "" {
		chip.FilterHref = basePath + "/plans?operated_by=" + url.QueryEscape(chip.RawID)
	}
	return renderOperatorChipCompact(chip)
}
