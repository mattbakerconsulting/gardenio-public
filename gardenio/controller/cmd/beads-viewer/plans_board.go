package main

// plans_board.go — the Jira-style /plans board (Matt's ask, thread
// ka-0000005903: "the plans index should look more like JiRA with lanes and
// when you click on a card the plan pops out from the side instead of being a
// whole new page"). Two pieces live here:
//
//  1. The LANES the board view sections plan cards into (one column per WORK
//     STATE, Jira-style). The board is the ?view= default; the historical flat
//     grid (+ its ?group= sectioning) survives as ?view=grid. Lanes are
//     DERIVED, not raw bead statuses (Matt's follow-ups in the same thread:
//     "in progress should be all approved plans with complete steps" and
//     "stalled/blocked should be a column") — planBoardLaneKey is the single
//     derivation.
//  2. The card PEEK fragment (GET /plans/{id}/peek): the slide-out side panel
//     a card click opens IN PLACE of navigating to /plans/{id}. It renders the
//     plan's header signals + the live step timeline (the same plan_steps
//     partial the cockpit uses, so step Done/Reopen/Add work right in the
//     drawer) with an "open full cockpit" escape hatch. plans_board.js owns
//     the client side: click interception, drawer open/close, ?peek= history.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// plansBoardClosedLimit caps the extra recently-closed fetch that fills the
// board's Done lane on the default (status=active) view. The active-status
// index query can't see closed plans, but a Jira-style board without a Done
// column reads broken — so the board pays ONE extra bounded list call for the
// freshest closed plans rather than unbounding the main query.
const plansBoardClosedLimit = 20

// planBoardLane is one column on the /plans board.
type planBoardLane struct {
	// Key is the stable lane token (open / in_progress / stalled / deferred /
	// closed on the derived board; a raw status on a narrowed ?status= view) —
	// used for CSS ids and the lane's data attribute.
	Key string
	// Label is the human column heading (planBoardLaneLabel).
	Label string
	// Rows are the plan cards in this lane, in index-query order.
	Rows []planRowView
	// Truncated marks the closed lane when the bounded recent-closed fetch was
	// clipped — the lane footer then says "showing the N most recent".
	Truncated bool
}

// planBoardLaneStalled is the derived Stalled / Blocked lane key — the column
// Matt asked for ("also stalled/blocked should be a column"). It unions the
// raw blocked status with the two derived attention signals the cards already
// badge: the ⚠ stalled signal (plans_stall.go) and ⛔ blocked-by-escalation
// (plans_escalations.go), so the column and the badges can never disagree.
const planBoardLaneStalled = "stalled"

// planBoardLaneOrder is the fixed left-to-right column order — the Jira
// backlog→done reading direction. Deferred sits between stalled and closed:
// parked work, not yet done. The "Done?" close-out lane
// (planCloseoutBoardLane) sits immediately BEFORE closed — it is the last stop
// on the ramp: still-open plans that LOOK finished, one operator tap from
// actually being closed.
var planBoardLaneOrder = []string{"open", "in_progress", planBoardLaneStalled, "deferred", planCloseoutBoardLane, "closed"}

// planBoardCoreLanes always render (even empty) on the full board — an empty
// In progress (or Stalled) column is a real signal on a board. Deferred is
// rare enough that an empty column would be noise; it only appears when
// populated (or when the ?status= filter names it directly).
// The close-out lane is deliberately NOT core: an empty "Done?" column would be
// pure noise on a board with nothing to retire (the common steady state once the
// backlog is drained), and unlike an empty In progress / Stalled — where the
// absence of work IS the signal — "nothing looks finished" needs no column. It
// appears the moment a plan drains.
var planBoardCoreLanes = map[string]bool{
	"open":               true,
	"in_progress":        true,
	planBoardLaneStalled: true,
	"closed":             true,
}

// planBoardLaneLabel humanizes a lane key for its column heading. The derived
// stalled + close-out lanes have no bead status behind them; every other key is
// a status.
func planBoardLaneLabel(key string) string {
	switch key {
	case planBoardLaneStalled:
		return "Stalled / Blocked"
	case planCloseoutBoardLane:
		// The question mark is deliberate: unlike Closed, these plans are still
		// OPEN — the lane asks the operator "is this one done?", it does not assert
		// that it is.
		return "Done?"
	}
	return planStatusLabel(key)
}

// planBoardLaneKey derives the board column for one enriched plan card. Lanes
// are WORK STATES, not raw bead statuses (Matt: "in progress should be all
// approved plans with complete steps" / "stalled/blocked should be a column")
// — a plan epic almost never gets literally claimed in_progress, so a
// status-keyed board dumped every actively-building plan into Open.
//
// Precedence, most- to least-terminal:
//
//	closed / deferred — raw status, work over or parked.
//	stalled           — blocked status, the ⚠ stalled signal, or an open
//	                    blocking escalation. Attention trumps progress: a
//	                    building plan whose agent died belongs here, not in
//	                    In progress where it would look healthy.
//	closeout ("Done?")— the plan is DRAINED: the daemon flagged it
//	                    `autoclose-ready`, or every rendered step is closed while
//	                    the epic is still open (planNeedsCloseout). This is the
//	                    exit ramp Matt asked for (thread ka-0000001785): a
//	                    finished plan's ONE remaining action is to retire it, so
//	                    it must not stay scattered across Open / In progress
//	                    where it reads as live work.
//
//	                    DELIBERATELY RANKED BELOW STALLED, not above. A drained
//	                    plan is almost never stall-flagged anyway (deriveStall's
//	                    unwatched branches all require openWork, which is false
//	                    when every step is closed, and its dead-step branch needs
//	                    an in_progress step, which StepsAllDone excludes) — so in
//	                    practice the only overlap is a drained plan carrying an
//	                    OPEN BLOCKING ESCALATION. That escalation is a real
//	                    outstanding privileged human op; laning such a plan into
//	                    "Done?" would hide the ⛔ badge behind a "retire me"
//	                    invitation and invite closing a plan with unfinished
//	                    off-subtree business. It keeps the Stalled lane, and its
//	                    close-out BADGE still renders there (the badge is gated on
//	                    planNeedsCloseout, not on the lane), so the signal is
//	                    surfaced without overriding the harder stop.
//	in_progress       — work is genuinely in flight: the epic literally
//	                    claimed; OR the plan is approved for build
//	                    (plan_state=building — the operator's deliberate
//	                    "Approve & build"); OR a legacy/ungated plan (no
//	                    plan_state) with ≥1 completed step, the observable
//	                    evidence of work for plans that predate the approval
//	                    lifecycle.
//	open              — everything else: unstarted plans, and draft/ready
//	                    plans still being authored (never "in progress",
//	                    whatever their steps say — they aren't approved yet).
func planBoardLaneKey(r planRowView) string {
	switch strings.ToLower(strings.TrimSpace(r.Status)) {
	case "closed":
		return "closed"
	case "deferred":
		return "deferred"
	case "blocked":
		return planBoardLaneStalled
	}
	if r.Escalations.Count > 0 || r.Stall.Stalled {
		return planBoardLaneStalled
	}
	// The exit ramp: a drained plan lanes into "Done?" rather than looking like
	// live work in Open / In progress. Placed AFTER the stalled check so an open
	// blocking escalation still wins (see the precedence doc above), and BEFORE
	// the in_progress checks — a plan_state=building plan whose steps have all
	// closed is finished, not in progress, and that is the single most common
	// shape in the measured pile.
	if planNeedsCloseout(r) {
		return planCloseoutBoardLane
	}
	if strings.EqualFold(strings.TrimSpace(r.Status), "in_progress") {
		return "in_progress"
	}
	switch planState(r.PlanState) {
	case planStateBuilding:
		return "in_progress"
	case planStateDraft, planStateReady:
		return "open"
	}
	if r.StepsClosed > 0 {
		return "in_progress"
	}
	return "open"
}

// planBoardLanes sections the enriched plan rows into board columns. rows is
// the main index result (already status-filtered by the daemon); closedRows is
// the extra Done-lane fetch. statusFilter decides the shape: the full-scope
// views (active/all) render the DERIVED board (planBoardLaneKey); a narrowed
// single-status filter renders just that raw-status column — derivation is
// deliberately bypassed there, because scattering a raw lifecycle slice into
// derived lanes outside the filter's scope would make rows silently vanish.
func planBoardLanes(rows, closedRows []planRowView, statusFilter string, closedTruncated bool) []planBoardLane {
	if statusFilter != planStatusActive && statusFilter != planStatusAll {
		return planBoardNarrowedLanes(rows, statusFilter, closedTruncated)
	}

	byLane := map[string][]planRowView{}
	for _, r := range rows {
		key := planBoardLaneKey(r)
		byLane[key] = append(byLane[key], r)
	}
	byLane["closed"] = append(byLane["closed"], closedRows...)

	lanes := make([]planBoardLane, 0, len(planBoardLaneOrder))
	for _, key := range planBoardLaneOrder {
		laneRows := byLane[key]
		if len(laneRows) == 0 && !planBoardCoreLanes[key] {
			continue
		}
		lanes = append(lanes, planBoardLane{
			Key:       key,
			Label:     planBoardLaneLabel(key),
			Rows:      laneRows,
			Truncated: key == "closed" && closedTruncated,
		})
	}
	return lanes
}

// planBoardNarrowedLanes renders a single-status ?status= filter as exactly
// that raw-status column — always, even empty, so "Closed, nothing there" is
// visible rather than a blank page.
//
// The ?status=stale SWEEP is special (task ka-0000004351): it is a DERIVED cut
// across the whole active set (open/in_progress/blocked/deferred), already
// subtracted to only the FreshnessStale rows in handlePlans. Scattering that one
// swept pile back across four raw-status columns would fracture the sweep and
// bury a 3-plan sweep in mostly-empty lanes; the whole point is ONE list the
// operator multi-selects and bulk-retires. So it renders as a single "Stale"
// lane rather than a per-status board.
func planBoardNarrowedLanes(rows []planRowView, statusFilter string, closedTruncated bool) []planBoardLane {
	if statusFilter == planStatusStale {
		return []planBoardLane{{
			Key:   planStatusStale,
			Label: planStatusLabel(planStatusStale),
			Rows:  rows,
		}}
	}
	byStatus := map[string][]planRowView{}
	for _, r := range rows {
		key := strings.ToLower(strings.TrimSpace(r.Status))
		byStatus[key] = append(byStatus[key], r)
	}
	scope := planStatusEnum(statusFilter)
	lanes := make([]planBoardLane, 0, len(scope))
	for _, status := range scope {
		lanes = append(lanes, planBoardLane{
			Key:       status,
			Label:     planStatusLabel(status),
			Rows:      byStatus[status],
			Truncated: status == "closed" && closedTruncated,
		})
	}
	return lanes
}

// planBoardClosedRows issues the bounded recently-closed fetch for the board's
// Done lane, mirroring the main index query's project + operator scoping so
// the lane shows the same universe the rest of the board does. Returns
// (nil, false) when the fetch fails — the board then simply renders an empty
// Done lane, never an error page.
func (s *Server) planBoardClosedRows(ctx context.Context, baseQuery beadsapi.ListBeadsQuery, effectiveOperatedBy string, operatedByIsUnowned bool) ([]*beadsapi.BeadDetail, bool) {
	q := baseQuery
	q.Statuses = []string{"closed"}
	q.Limit = plansBoardClosedLimit
	var (
		res *beadsapi.ListBeadsResult
		err error
	)
	if !operatedByIsUnowned && effectiveOperatedBy != "" {
		res, err = s.plansInvolvingOperator(ctx, q, effectiveOperatedBy)
	} else {
		res, err = s.daemon.ListBeadsFiltered(ctx, q)
	}
	if err != nil || res == nil {
		s.logger.Debug("plans board: recent-closed lane query", "error", err)
		return nil, false
	}
	return res.Beads, res.Total > len(res.Beads)
}

// handlePlanPeek renders the slide-out drawer fragment for one plan — the
// "pops out from the side instead of being a whole new page" surface. It is a
// deliberately lighter cut of handlePlanDetail: header signals (status,
// project, lifecycle state, progress, open decisions) + the SAME live
// plan_steps timeline partial the cockpit renders — so Done/Reopen/Add/
// dispatch work from the drawer via their existing HTMX routes, which all
// target #plan-steps (present here too).
//
// Beyond the timeline, the drawer now surfaces the three "who's on it / is it
// live" signals Matt asked for (thread ka-0000002125): the unified plan-agent
// CHIP in the header (gardener running/idle/paused + a live-iteration dot), the
// CREW roster below the steps (the agents the gardener dispatched + how each is
// going), and the plan-agent CHAT transcript at the bottom. All three reuse the
// SAME builders + partials the full cockpit renders (planGardenerBarData → the
// unified chip, planCrew → the roster, planTranscriptView → the conversation),
// so the preview can never disagree with the page. They stay READ-ONLY here —
// the gardener levers (Pause/Resume/Wake), the compose box, and the critique
// cockpit remain on the full page; the drawer links out for those.
func (s *Server) handlePlanPeek(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "missing plan id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	bead, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		if beadMissing(err) {
			http.Error(w, "Plan not found", http.StatusNotFound)
			return
		}
		s.logger.Error("plan peek: get bead", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if bead == nil || bead.Type != "epic" {
		http.Error(w, "Plan not found", http.StatusNotFound)
		return
	}
	s.renderPlanPeek(w, r, bead)
}

// renderPlanPeek renders the slide-out drawer fragment for one plan epic. Split
// out of handlePlanPeek so the lifecycle handlers (complete / reopen) can
// re-render the drawer in place when their button lives in the peek, exactly as
// handlePlanPeek does on the initial GET — the drawer's Mark complete / Reopen
// levers target #plan-peek-body and swap this same fragment.
func (s *Server) renderPlanPeek(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail) {
	ctx := r.Context()
	id := bead.ID
	isOperator := viewerCanOperate(r)
	steps, phases := s.planTimeline(ctx, id)
	total, closed, pct := planProgress(steps, phases)
	st := planStateOf(bead)
	planClosed := statusOpenSortKey(bead.Status) != 0
	r = s.prewarmAgentLinksForNames(r, planTimelineAssignees(steps, phases))

	// The gardener-live chip: the SAME unified plan-agent chip the /plans index
	// card and the /plans/{id} cockpit header render (planGardenerBarData folds the
	// gardener loop state + the bound iteration's liveness into one honest chip —
	// "running · live", "idle", "paused", …). Best-effort: the builder degrades to
	// a "none" chip on any lookup miss, so the drawer always has a chip.
	chip, _ := s.planGardenerBarData(ctx, id, isOperator, "")["Chip"].(planAgentChipView)
	// Terminal-reconcile the chip's "not started" none-face (ka-0000005352 iter-54):
	// the peek drawer renders the SAME plan_agent_chip partial as the index card, so
	// a closed/deferred gardener-less plan must drop the "not started" lie here too —
	// matching the index card's own reconcile (planAgentChipView.reconcileTerminal)
	// and the lifecycle-chip + crew terminal reconciles right beside it. The
	// plan_peek partial already gates on {{if .Label}}, so a cleared Label renders no
	// pill. Display-only; bead.Status is already in hand — zero extra daemon call.
	chip = chip.reconcileTerminal(bead.Status)
	// The crew roster: the agents the plan gardener dispatched onto the steps above
	// + how each is going, walked over the SAME rendered timeline (two batched
	// daemon calls; a failure degrades to an empty roster).
	// id is the epic bead id, threaded so the peek roster surfaces epic-anchored
	// crew too (delivery-drive-gardener pattern, ka-0000005352), matching the index.
	crew := s.planCrew(ctx, id, steps, phases)
	// Quiet the LIVE-NOW crew signals on a TERMINAL plan (iteration 30,
	// ka-0000005352): the peek drawer renders the SAME plan_crew partial as the
	// cockpit, so a zombie "working" crew on a closed plan must read "done" here
	// too — no pulse — matching the index chip + cockpit. bead.Status is already
	// in hand; zero extra daemon call.
	crew = crew.reconcileTerminal(bead.Status)
	// The "last change" freshness stamp (ka-0000005352 iter-73): the peek header
	// folds nearly every index-card FACE triage signal (status · lifecycle · gardener
	// · crew+stuck · project · decisions · priority iter-72) but dropped the ONE
	// temporal signal both the index card head (.plan-card-updated, plan_card.html)
	// AND the cockpit header (.plan-header-updated, plan_detail.html) carry — so an
	// operator opening a peek to TRIAGE could not tell whether the plan moved 5m ago
	// or has been dead 3 weeks, nor whether it is stale (>7d no activity). Compute the
	// SAME "last change" instant both surfaces render: planLastActivity over the epic
	// + rendered steps/phases, then fold in the crew heartbeats (ka-0000004310) so a
	// step claimed a month ago whose crew is heartbeating now reads fresh — the
	// false-freshness family the index card fixes. Every input is ALREADY in hand
	// (bead.UpdatedAt, steps/phases, crew just fetched), so zero extra daemon call;
	// laterTime keeps the stamp monotonic. FreshnessStale gates on the plan's OWN
	// status (planFreshnessStale) so a terminal plan's honest "3 weeks ago" history
	// stays calm — matching the index card's own loud-tint gate (iter-38).
	lastActivity := laterTime(planLastActivity(bead.UpdatedAt, steps, phases), crew.newestActivityAt)
	freshAgo := fmtTimeAgo(lastActivity, s.now())
	freshRFC := ""
	if !lastActivity.IsZero() {
		freshRFC = lastActivity.UTC().Format(time.RFC3339)
	}
	freshStale := planFreshnessStale(bead.Status, lastActivity, s.now())
	// Blocked-by-escalation badge (ka-0000005352 iter-74): the peek header folds
	// nearly every index-card FACE triage signal — status · lifecycle · priority
	// (iter-72) · gardener · crew+stuck (iter-65/71) · project · open-decision chip ·
	// freshness (iter-73) — but dropped the ⛔ "blocked by escalation" badge, the #2
	// attention tier (planAttentionEscalation): a HARDER stop than a ⚠ stall or an
	// open decision, "a privileged operator must act before the blocked work can
	// move". Both the index card FACE (plan_card.html) and the cockpit banner carry
	// it, yet the peek — which an operator opens precisely to TRIAGE — showed the
	// open-decision chip but nothing about a work-blocking escalation. Derive the
	// SAME signal the cockpit does (plans.go, the escalations banner call): one
	// planEscalationsBulk over planEscalationWalkIDs(id, steps, phases) — for a
	// single plan the bulk helper degenerates to one forward blocks-edge batch + one
	// blocker-hydration batch, the same bounded cost the cockpit pays. Gate the FACE
	// on ShowEscalationBadge (Count>0 AND not terminal — the iter-39 terminal
	// reconcile, so a finished plan whose leftover open steps still carry an open
	// escalation adds no false "act now"), and project the oldest-first ref head into
	// the SAME op-label + wait-age + >7d stale-tint the index badge renders. The
	// .plan-esc-badge CSS (+ -op/-age/-stale + its html[data-skin^="dark"] override)
	// already lives in plans.html, which hosts the #plan-peek-body mount, so this
	// needs no new rule and no dark override — pure index-face→peek-header parity for
	// the escalation signal, the attention-tier twin of the decision chip.
	escalations := s.planEscalationsBulk(ctx, map[string][]string{
		id: planEscalationWalkIDs(id, steps, phases),
	})[id]
	showEscalationBadge := escalations.Count > 0 && !planStatusTerminal(bead.Status)
	// Blocked ("stalled") badge on the peek header (ka-0000005352 iter-75): the peek
	// header folds the #1 open-decision chip AND the #2 ⛔ escalation badge (iter-74)
	// but STILL dropped the #3 ⚠ stall badge (planAttentionStalled) — both the index
	// card FACE (plan_card.html) and the cockpit header (plan_detail.html) carry it,
	// yet the peek, opened precisely to TRIAGE, showed the decision + escalation
	// signals but nothing about a stalled plan (a dead-agent orphan, a paused gardener,
	// or no gardener bound). Derive the SAME planStallInfo the cockpit does
	// (planStallForDetail → the reconcileStallWith{Crew,Escalation,Lifecycle} chain,
	// plans.go handlePlanDetail) so the peek header ⚠ badge and the cockpit header ⚠
	// badge agree by construction (advice ka-0000005924). Liveness rides the resolver
	// already prewarmed above (prewarmAgentLinksForNames over the timeline assignees),
	// so alive() adds no fetch; the assistant-live signal and the gardeners-by-epic
	// lookup are the same bounded per-detail cost the cockpit already pays for one
	// plan. Gated on Stall.ShowBadge in the template exactly like the index card + the
	// cockpit, so a healthy plan adds no ink and a terminal plan (deriveStall's
	// planTerminal guard) never fires. The .plan-stall-badge CSS (+ -reason/-age/
	// -stale + its html[data-skin^="dark"] override) already lives in plans.html,
	// which hosts the #plan-peek-body mount — so no new rule and no dark override,
	// pure index-face/cockpit-header → peek-header parity for the stall signal,
	// completing the attention-tier trio (decision · escalation · stall) on the peek.
	resolver := agentResolverFromContext(ctx)
	alive := func(name string) bool {
		if resolver == nil {
			return false
		}
		info, ok := resolver.ResolveInfo(name)
		return ok && info.Alive
	}
	assistantLive := s.assistantIsLive(s.planAssistant(ctx, id))
	stall := s.planStallForDetail(ctx, id, bead.Status, steps, phases, total > closed, assistantLive, alive)
	// Same reconcile chain the cockpit applies, in the same order, off the SAME crew
	// + escalation signals already in hand here (zero extra daemon call): a false
	// gardener-state stall is cleared when the roster proves work is live OR queued
	// (iter-19/51), a dead-agent orphan coexisting with live crew is re-scoped to the
	// softer "⚠ N orphaned" (iter-23), a gardener-state stall is suppressed when an
	// escalation already blocks the plan (iter-26), and a pre-build draft/ready plan
	// with no gardener BY DESIGN drops the redundant face (iter-49).
	stall = reconcileStallWithCrew(stall, crew.HasLive(), crew.HasQueued())
	stall = reconcileStallWithEscalation(stall, escalations.Count > 0)
	stall = reconcileStallWithLifecycle(stall, gardenerEnableGatedByState(planStateOf(bead)))
	// The plan-agent chat transcript: every operator + agent turn persisted as a
	// durable type=message bead, rendered read-only through the shared bead_chat
	// direction-bubble partial (empty view → the partial paints nothing).
	transcript := s.planTranscriptView(ctx, id)

	s.renderPartial(w, r, "templates/partials/plan_peek.html", "plan_peek", map[string]any{
		"PlanID":  id,
		"InPeek":  true, // step and crew heads stay h3 under the drawer's h2 (aria-level 2 on the cockpit)
		"Title":   bead.Title,
		"Project": projectFromLabels(bead.Labels),
		"Status":  bead.Status,
		// The plan's arms in canonical form; the peek header draws them in the
		// monogram's place (arms.go).
		"Heraldry": bearerArms(bead),
		// Priority pill (ka-0000005352 iter-72): the plan epic's 0-4 priority, the
		// LAST index-card-FACE triage signal the peek header still dropped (the
		// status/state/gardener/crew/project/decision chips already fold here). An
		// operator opens a peek to TRIAGE, so whether the plan is P0-urgent or
		// P3-shelved belongs in the header scan, not only on the index card behind
		// the drawer. bead.Priority is already in hand (zero extra daemon call); the
		// template gates on `ne .Priority 2` exactly like the index card face
		// (plan_card.html), so a routine P2 adds no ink and only an off-baseline
		// priority pops. The .plan-prio-chip-* CSS already lives in plans.html (which
		// hosts the #plan-peek-body mount), so no new rule and no dark override.
		"Priority": bead.Priority,
		// "Last change" freshness stamp (ka-0000005352 iter-73) — the same three
		// keys the index card head renders (AgoCompact / AgoRFC-tooltip /
		// FreshnessStale loud-tint), computed above off data already in hand.
		"FreshAgo":   freshAgo,
		"FreshRFC":   freshRFC,
		"FreshStale": freshStale,
		// Blocked-by-escalation badge (ka-0000005352 iter-74) — the SAME keys the
		// index card row exposes: the terminal-reconciled ShowEscalationBadge FACE
		// gate, the raw info (Count + refs for the tooltip), and the oldest ref's
		// op-label + wait-age + stale-tint projections. All computed above off the one
		// planEscalationsBulk the cockpit banner also pays, so header ⛔ badge and
		// cockpit banner can never disagree. Empty/false when no escalation blocks the
		// plan, so a healthy plan adds no ink.
		"ShowEscalationBadge": showEscalationBadge,
		"Escalations":         escalations,
		"EscalationOldestOp":  escalations.OldestOperationLabel(),
		"EscalationOldestAge": escalations.OldestAge(s.now()),
		"EscalationStale":     escalations.OldestStale(s.now()),
		// Stall badge (ka-0000005352 iter-75) — the SAME planStallInfo the cockpit
		// header renders (derived + reconciled above). The template gates on
		// .Stall.ShowBadge exactly like the index card + cockpit, projecting the
		// Orphaned/HeadlineCount/HeadlineLabel/FaceLabel/AgeLabel/AgeStale facets.
		// Empty/false ShowBadge when the plan is healthy or terminal → no ink.
		"Stall": stall,
		// Reconcile the DISPLAY chip against the epic's terminal status
		// (ka-0000005352 iter-28, planLifecycleChipState) — the same iter-27 fix the
		// index card + cockpit header carry, extended to the peek drawer so a
		// closed/deferred plan never shows a stale "building"/"draft"/"ready" chip
		// here. Display-only: the raw st still drives the ShowMarkComplete/ShowReopen
		// levers below (a terminal plan must still offer Reopen), exactly as iter-27
		// kept the board lane + Reopen lever on the raw planStateOf.
		"PlanState":     string(planLifecycleChipState(st, bead.Status)),
		"IsOperator":    isOperator,
		"Steps":         steps,
		"Phases":        phases,
		"HasPhases":     len(phases) > 0,
		"StepsTotal":    total,
		"StepsClosed":   closed,
		"StepsOpen":     total - closed,
		"PctDone":       pct,
		"OpenDecisions": s.planOpenDecisionCount(ctx, id),
		"ViewerEmail":   oidcauth.EmailFromContext(r.Context()),
		"Chip":          chip,
		"Crew":          crew,
		"Transcript":    transcript,
		// The "⚗ Pour a formula" disclosure (embedded via plan_steps → plan_pour)
		// lazy-loads its own project-scoped picker on first open via
		// GET /plans/{id}/pours/new (plan_pour.go), so the peek drawer injects no
		// formula data here — the disclosure works identically from the drawer and
		// the cockpit without a per-render formula query (ka-0000001848).
		// Terminal completion levers (task ka-0000000616): mirror the cockpit's
		// gating so the drawer offers Mark complete whenever plan_state=complete is
		// a legal next state (building OR unset — planStateCanComplete, bug
		// ka-0000007777) and the epic is still open, Abandon (the cheap exit, task
		// ka-0000001377) from any non-terminal working state, and Reopen once
		// terminal (plan_state=complete/abandoned OR the epic was closed by hand).
		"ShowMarkComplete":   planStateCanComplete(st) && !planClosed,
		"ShowAbandon":        (st == planStateDraft || st == planStateReady || st == planStateBuilding) && !planClosed,
		"ShowReopen":         st == planStateComplete || st == planStateAbandoned || planClosed,
		"OpenStepsRemaining": total - closed,
	})
}
