package main

// plans_detail.go — the /plans/{id} cockpit handler and its timeline, blocker, progress and narrative helpers.
// Split out of plans.go (gd-Wl6LuCnvlH); see plans.go for the Plans surface overview.

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/plans"
)

// handlePlanDetail renders /plans/{id} — the one-page plan cockpit: the
// epic->children constellation, the editable TODO step list, the
// clarifying-question decisions, the build-progress rollup, and the plan
// narrative (its child bundle doc). Renders ANY epic (so it is graceful and
// testable even before the `plan` label is applied); an unlabeled epic shows a
// one-click "mark as plan" affordance for operators.
func (s *Server) handlePlanDetail(w http.ResponseWriter, r *http.Request) {
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
		s.logger.Error("plan detail: get bead", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if bead != nil && bead.Type == "bundle" && r.URL.Query().Get("bead") != "1" {
		// A plan can be "published" as a bare `type=bundle` doc with no epic
		// parent (e.g. via `gb bundle create --kind plan`), and the link
		// handed to the operator is still a /plans/<id> URL. Mirror
		// handleBeadShow's bundle redirect here instead of 404ing, so that
		// link always resolves (ka gd-Ip5pI9fhN8) — the bundle's own reading
		// surface at /bundle/<id> is a graceful landing for it.
		http.Redirect(w, r, s.basePath+"/bundle/"+url.PathEscape(id), http.StatusFound)
		return
	}
	if bead == nil || bead.Type != "epic" {
		// Only epics are full plan cockpits. A deep link to any other non-epic
		// bead 404s here rather than rendering a misleading empty cockpit; the
		// canonical bead page (/{id}) remains the home for every other type.
		http.Error(w, "Plan not found", http.StatusNotFound)
		return
	}

	isOperator := viewerCanOperate(r)
	isPlan := hasLabel(bead, planLabel)

	// The plan cockpit no longer renders the constellation (Matt, ka-0000001134:
	// "i dont like the constellation for this; we need a better kind of view").
	// The plan graph IS the step timeline (plan_steps.html) now — a linear plan
	// reads far better as an ordered, connected sequence than as a force graph,
	// and the constellation was a lossy, capped duplicate of the same children.
	// Steps + phase groups in one fetch (ka-0000005926). With phases the flat
	// steps are the unphased remainder; phases carry their own grouped steps.
	steps, phases := s.planTimeline(ctx, id)
	// Progress over exactly the rendered steps (flat + phased), so the header
	// chip denominator equals the visible ✓ rows (ka-0000004410). The /plans
	// index row computes the same rendered set, so index % and detail % agree.
	total, closed, pct := planProgress(steps, phases)
	// One batched agent-bead lookup covers every step assignee (flat + phased)
	// so the timeline's agentLink renders resolve from cache (ka-0000000899).
	r = s.prewarmAgentLinksForNames(r, planTimelineAssignees(steps, phases))

	decisions := s.planDecisions(ctx, id, isOperator)
	openDecisions := 0
	for _, d := range decisions {
		if !d.Resolved {
			openDecisions++
		}
	}

	// The narrative fragment (summary + bundle docs) uses the same data
	// builder as the standalone /plans/{id}/narrative route and the SSE
	// re-render, so all three paths render it identically (S3, ka-0000002473).
	narrativeData, _ := s.planNarrativeData(ctx, id, isOperator, "")

	// The unified plan-agent panel (step 4, ka-0000001218): ONE data bag merging
	// the assistant lane (chip + prompt + transcript + activity — S2,
	// ka-0000007556) and the gardener control bar (status chip + pause/resume +
	// Wake — Step 2, ka-0000002775) that handlePlanDetail used to build as two
	// separate bags. planAgentPanelData composes the SAME two builders the
	// standalone fragments + SSE re-renders use, folds them into one structure,
	// and derives the unified loop×agent state (PanelState) that steps 5+6 render
	// as a single chip + card. BasePath is folded in by the composer so the
	// template passes the bag straight through to BOTH partials (rather than
	// re-projecting individual keys and silently dropping new ones like
	// MaxLen/Delivery — the 2d regression, ka-0000004026).
	agentPanel := s.planAgentPanelData(ctx, id, isOperator, "", planDeliveryNone, "")

	// "Last change" (ka-0000002873): the newest activity across the plan epic
	// and its rendered steps/phases, so the header shows when the plan actually
	// last moved — not just when the epic bead itself was last touched (which is
	// usually its creation, since the churn lives in the child steps). Folds
	// over the exact rendered set the /plans index folds. The crew heartbeats are
	// folded in below once the roster is fetched (ka-0000004310), so both surfaces
	// agree on the same instant.
	lastActivity := planLastActivity(bead.UpdatedAt, steps, phases)

	// Stalled-plan warning (ka-0000005581): identical predicate to the index
	// badge, computed over the timeline just fetched. Liveness rides the
	// prewarmed agentResolver (every step assignee is already in its cache),
	// so this adds only the single gardeners-by-epic lookup.
	assistantLive := agentPanel["State"] == string(planAssistantStateLive)
	resolver := agentResolverFromContext(r.Context())
	alive := func(name string) bool {
		if resolver == nil {
			return false
		}
		info, ok := resolver.ResolveInfo(name)
		return ok && info.Alive
	}

	// The crew roster (ka-0000000150): the agents the plan gardener has dispatched
	// onto steps + how each is going. Walks the SAME rendered steps the timeline
	// shows, so the roster's step labels match the timeline. Two batched daemon
	// calls (assigned-edge sweep + agent hydration); a failure degrades to an
	// empty roster ("no crew yet"), never a partial one. Computed BEFORE the stall
	// so the reconcile below can read its authoritative liveness (iteration 19).
	// id is the epic bead id, threaded so the walk also surfaces crew anchored to
	// the epic itself (delivery-drive-gardener pattern, ka-0000005352).
	crew := s.planCrew(ctx, id, steps, phases)
	// Quiet the LIVE-NOW crew signals on a TERMINAL (closed/deferred) plan
	// (iteration 30, ka-0000005352) — the cockpit twin of the index enrich
	// reconcile, so the roster header + member pulses agree with the index chip.
	// A finished plan cannot have a crew "working now"; a zombie crew (open bead,
	// stale alive state) otherwise pulsed the header "M working" dot + its own
	// live-dot. The record (Total/spend/failed/each crew row) is untouched. Done
	// BEFORE the stall reconcile so it reads the reconciled HasLive(). Zero extra
	// daemon call — bead.Status is already in hand.
	crew = crew.reconcileTerminal(bead.Status)

	// Fold the crew heartbeats into the "last change" stamp (ka-0000004310, iter-24)
	// now that the roster is in hand: a working crew's updated_at moves every cycle
	// while the STEP task bead it works sits static, so without this the cockpit
	// header would render "updated 1 month ago" beside a live "M working" roster —
	// the same card-vs-truth false-freshness the index card fixes. crew is already
	// fetched (zero extra daemon call); laterTime keeps the stamp monotonic. Folded
	// on BOTH surfaces off the same crew beads so the index card and the cockpit
	// header show the SAME instant for one plan.
	lastActivity = laterTime(lastActivity, crew.newestActivityAt)
	lastChangeAgo := humanizeTime(lastActivity)
	lastChangeRFC := ""
	if !lastActivity.IsZero() {
		lastChangeRFC = lastActivity.UTC().Format(time.RFC3339)
	}

	// Stalled-plan warning (ka-0000005581): identical predicate to the index
	// badge, computed over the timeline just fetched. Liveness rides the
	// prewarmed agentResolver (every step assignee is already in its cache),
	// so this adds only the single gardeners-by-epic lookup. Reconciled against the
	// crew roster just fetched (ka-0000005352 iteration 19): a gardener-paused/
	// no-gardener stall is a false alarm when the AUTHORITATIVE roster says a crew
	// is working right now — the cockpit ⚠ badge then agrees with the crew header,
	// exactly as the index card's badge agrees with its crew chip. Zero extra
	// daemon call (crew is already in hand); a dead-agent stall is untouched.
	stall := s.planStallForDetail(ctx, id, bead.Status, steps, phases, total > closed, assistantLive, alive)
	// crewQueued (iteration 51): clear a false gardener-state stall when the roster
	// carries queued crew too, not only working crew — the cockpit badge then
	// agrees with the index card + the crew header's own queued facet.
	stall = reconcileStallWithCrew(stall, crew.HasLive(), crew.HasQueued())

	// Blocked-by-escalation banner (epic ka-0000005480, WS4): the same signal
	// the index badge derives, computed over the timeline just fetched. One
	// plan → the bulk helper degenerates to one blocks-edge batch + one
	// hydration batch.
	escalations := s.planEscalationsBulk(ctx, map[string][]string{
		id: planEscalationWalkIDs(id, steps, phases),
	})[id]
	// Reconcile the cockpit ⚠ stalled badge with the ⛔ escalation banner just
	// derived (iteration 26, ka-0000005352): suppress the redundant gardener-state
	// stall face when an escalation already blocks the plan, exactly as the index
	// card does. Runs after the escalation signal is in hand; a dead-agent orphan
	// stays loud. Keeps the cockpit header badge agreeing with the index card.
	stall = reconcileStallWithEscalation(stall, escalations.Count > 0)
	// Reconcile the cockpit ⚠ stalled badge with the plan's PRE-BUILD lifecycle
	// (iteration 49, ka-0000005352): suppress the redundant gardener-state stall
	// face when the plan is still draft/ready and has no gardener BY DESIGN
	// (gardenerEnableGatedByState, the R6 build gate), exactly as the index card
	// does — the READY/DRAFT lifecycle chip already explains why no gardener is
	// bound. Keeps the cockpit header badge agreeing with the index card. A
	// dead-agent orphan stays loud.
	stall = reconcileStallWithLifecycle(stall, gardenerEnableGatedByState(planStateOf(bead)))

	data := map[string]any{
		"Bead":        bead,
		"OGImage":     badgeOGImage(r, s.basePath, bead),
		"PlanID":      id,
		"ShippedIn":   s.releaseShippedIn(ctx, bead),
		"Title":       bead.Title,
		"Project":     projectFromLabels(bead.Labels),
		"Status":      bead.Status,
		"IsOperator":  isOperator,
		"CanShare":    planShareOffered(r, projectFromLabels(bead.Labels)),
		"IsPlan":      isPlan,
		"Steps":       steps,
		"Phases":      phases,
		"HasPhases":   len(phases) > 0,
		"StepsTotal":  total,
		"StepsClosed": closed,
		"StepsOpen":   total - closed,
		"PctDone":     pct,
		// Flat steps titled "Phase N" that were probably meant to be phase
		// sub-epics (ka-0000002630) — nil when none, so the hint self-suppresses.
		"PhaseHint":     misfiledPhaseSteps(steps),
		"Decisions":     decisions,
		"OpenDecisions": openDecisions,
		"Narrative":     narrativeData,
		// Agent is the ONE merged plan-agent panel bag (step 4, ka-0000001218):
		// the assistant lane keys + Gardener (planGardenerView) + PanelState
		// (unified loop×agent state) + BasePath, all on one structure. The
		// detail template passes it to both the plan_gardener and plan_assistant
		// partials, each reading the keys it needs.
		"Agent":         agentPanel,
		"LastChangeAgo": lastChangeAgo,
		"LastChangeRFC": lastChangeRFC,
		"Stall":         stall,
		"Escalations":   escalations,
		"Crew":          crew,
		// R7 (garden_retire.go): "Open it in the garden" while the cockpit retires.
		"GardenHref": s.gardenBanner(r, retirePlans, gardenPlanTarget),
	}
	// Automated crit-style inline plan critique (prototype, task ka-0000002614):
	// computed PROOF / coverage / design-review findings rendered inline on the
	// cockpit with collapsible detail. Dark-launched
	// (BEADS_VIEWER_PLAN_CRITIQUE_FINDINGS=1); when off the bag key is nil and
	// the panel self-suppresses, so the page is byte-identical to today. ZERO
	// extra daemon calls — every input is a value already assembled above
	// (bead, steps, phases, decisions, narrative bundle, escalations, state).
	if s.planCritiqueFindingsEnabled {
		var proseView *bundleView
		if narrativeData != nil {
			if pv, ok := narrativeData["Prose"].(*bundleView); ok {
				proseView = pv
			}
		}
		findings := computePlanCritiqueFindings(bead, steps, phases, decisions, proseView, escalations, planStateOf(bead))
		data["CritiqueFindings"] = buildPlanCritiqueFindingsData(id, s.basePath, findings)
	}
	// The "⚗ Pour a formula" disclosure (epic ka-0000000759, Phase 3) lazy-loads
	// its own project-scoped picker via GET /plans/{id}/pours/new on first open
	// (plan_pour.go), so the cockpit page render injects nothing here — keeping
	// the formula-list query off the hot page/SSE path (ka-0000001848).
	s.render(w, r, "plan_detail.html", data)
}

// planTimeline is the one fetch that feeds both the flat step list and the
// phase groups. It reads the plan epic + its direct children once, then:
//   - if the epic declares fields.phase_order, the listed direct-child epics
//     become ordered PHASE groups (planPhases descends one level for their
//     steps) and the returned flat steps are only the UNPHASED direct issue
//     children (a plan may legitimately mix loose steps and phases);
//   - otherwise phases is nil and the flat steps are all direct issue children,
//     exactly as before (ka-0000005926).
//
// Both slices are fully enriched (MR pills + open-blocker chips) in the same
// batched idiom. Degrades to (nil, nil) on any fetch error.
func (s *Server) planTimeline(ctx context.Context, epicID string) ([]planStepView, []planPhaseView) {
	epic, err := s.daemon.GetBead(ctx, epicID)
	if err != nil || epic == nil {
		s.logger.Debug("plan timeline: get epic", "epic", epicID, "error", err)
		return nil, nil
	}
	revDeps, err := s.daemon.GetReverseDependencies(ctx, epicID)
	if err != nil {
		s.logger.Debug("plan timeline: reverse deps", "epic", epicID, "error", err)
		return nil, nil
	}
	ids := dedupedParentChildIDs(revDeps)
	if len(ids) == 0 {
		return nil, nil
	}
	children, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		s.logger.Debug("plan timeline: batch get", "epic", epicID, "error", err)
		return nil, nil
	}

	// The plan's ONE narrative bundle (a direct type=bundle child) is the
	// cross-ref target for the § section chips (ask 2). Located from the
	// already-fetched children — no extra round-trip.
	bundleID := directBundleID(ids, children)

	// Phase groups (ordered, membership = phase_order). phaseStepIDs is the set
	// of step ids already claimed by a phase; the flat list excludes them so a
	// step never renders twice.
	phases, phaseStepIDs := s.planPhases(ctx, epic, children, bundleID)

	// Flat steps = direct issue children NOT claimed by a phase. When there are
	// no phases this is every direct issue child (the pre-phase behavior).
	flatIDs := make([]string, 0, len(ids))
	for _, cid := range ids {
		if _, inPhase := phaseStepIDs[cid]; inPhase {
			continue
		}
		flatIDs = append(flatIDs, cid)
	}
	// The flat (unphased) steps' explicit order lives on the plan epic itself
	// (ka-0000007445) — the symmetric sibling of the phase steps, whose order
	// lives on each phase sub-epic.
	steps := buildStepRows(flatIDs, children, planStepOrder(epic))
	s.enrichStepRows(ctx, steps, children)
	enrichStepSectionRefs(steps, children, bundleID)
	return steps, phases
}

// directBundleID returns the id of the plan's narrative bundle — the first
// direct type=bundle child — or "" when the plan has none. One bundle per plan
// is the accepted invariant (operator decision, thread ka-0000003180).
func directBundleID(childIDs []string, children map[string]*beadsapi.BeadDetail) string {
	return plans.BundleID(childIDs, children)
}

// enrichPlanStepBlockers fills each step's BlockedBy chips from one batched
// forward blocks-edge walk over the step set. A forward `blocks` dep (step →
// blocker) means the step is waiting on the blocker; only OPEN blockers
// surface — a closed blocker is history, not a "waiting on" signal. Blocker
// beads already fetched with the step batch are reused, so intra-plan blocks
// chains (the common case) cost no extra round-trip; only out-of-plan blocker
// IDs trigger a second BatchGetBeads. Errors degrade to "no chips".
func (s *Server) enrichPlanStepBlockers(ctx context.Context, steps []planStepView, known map[string]*beadsapi.BeadDetail) {
	if len(steps) == 0 {
		return
	}
	stepIDs := make([]string, 0, len(steps))
	for _, st := range steps {
		stepIDs = append(stepIDs, st.ID)
	}
	depsByID, err := s.daemon.BatchGetDependencies(ctx, stepIDs, "forward")
	if err != nil {
		s.logger.Debug("plan steps: blockers walk", "error", err)
		return
	}
	fetch := make([]string, 0)
	seen := make(map[string]struct{})
	for _, id := range stepIDs {
		for _, d := range depsByID[id] {
			if d.Type != "blocks" || d.DependsOnID == "" {
				continue
			}
			if _, ok := known[d.DependsOnID]; ok {
				continue
			}
			if _, dup := seen[d.DependsOnID]; dup {
				continue
			}
			seen[d.DependsOnID] = struct{}{}
			fetch = append(fetch, d.DependsOnID)
		}
	}
	blockers := known
	if len(fetch) > 0 {
		got, gerr := s.daemon.BatchGetBeads(ctx, fetch)
		if gerr != nil {
			s.logger.Debug("plan steps: blocker beads", "error", gerr)
			got = nil
		}
		// Merge into a fresh map rather than mutating the caller's children map.
		blockers = make(map[string]*beadsapi.BeadDetail, len(known)+len(got))
		maps.Copy(blockers, known)
		maps.Copy(blockers, got)
	}
	for i := range steps {
		for _, d := range depsByID[steps[i].ID] {
			if d.Type != "blocks" || d.DependsOnID == "" {
				continue
			}
			b := blockers[d.DependsOnID]
			if b == nil || b.Status == "closed" {
				// Unknown blocker (fetch failed / absent) or already closed —
				// either way there is nothing actionable to chip.
				continue
			}
			steps[i].BlockedBy = append(steps[i].BlockedBy, planStepBlockerView{
				ID:    b.ID,
				Title: b.Title,
				// Escalation blockers chip distinctly (epic ka-0000005480, WS5):
				// a privileged-op hard stop reads differently from an ordinary
				// bead dependency.
				IsEscalation:  b.Type == "escalation",
				OperationType: b.Fields["operation_type"],
			})
		}
	}
}

// planStepAssignees collects assignee names across steps for the agent-link
// prewarm batch (ka-0000000899 — one daemon call per render, not one per row).
// Prewarm dedupes and drops non-agent-shaped names itself.
func planStepAssignees(steps []planStepView) []string {
	names := make([]string, 0, len(steps))
	for _, st := range steps {
		if st.Assignee != "" {
			names = append(names, st.Assignee)
		}
	}
	return names
}

// planTimelineAssignees collects assignee names across BOTH the flat steps and
// every phase's steps (and the phase sub-epics' own assignees) for the single
// agent-link prewarm batch, so a phase-grouped render still resolves agent
// links in one daemon call.
func planTimelineAssignees(steps []planStepView, phases []planPhaseView) []string {
	names := planStepAssignees(steps)
	for _, p := range phases {
		if p.Assignee != "" {
			names = append(names, p.Assignee)
		}
		names = append(names, planStepAssignees(p.Steps)...)
	}
	return names
}

// planProgress returns the plan's progress counts over EXACTLY the steps the
// timeline renders: the flat/unphased rows plus every phase's rows. The header
// chip denominator therefore equals the number of visible ✓ rows (ka-0000004410).
//
// It deliberately does NOT roll up the transitive issue subtree
// (deriveOwnedEpicRollup): that counted issue-kind descendants the timeline
// never shows — grandchildren under a step, the children of a freestanding
// sub-plan epic — so a plan like acme-scif read "23/27 · 85%" in the header while
// the timeline rendered only 25 rows, and a user counting checkmarks (23 of 25)
// could not reconcile it against 27. The /plans index card computes the SAME
// rendered set (renderedStepRollup over the shared child walk), so the index %
// and the detail % still agree by construction — the Q3 unification
// (decision ka-0000000700) is preserved, now anchored on the rendered rows
// rather than on the transitive count that diverged from them.
//
// The count spans BOTH the flat/unphased steps AND every phase's steps
// (ka-0000005926): a phased plan's steps live under phase sub-epics, so counting
// only the flat slice would undercount a mixed plan — the header chip would then
// disagree with the sum of the per-phase progress chips.
func planProgress(steps []planStepView, phases []planPhaseView) (total, closed, pct int) {
	for _, st := range steps {
		total++
		if st.Closed {
			closed++
		}
	}
	for _, p := range phases {
		total += p.StepsTotal
		closed += p.StepsClosed
	}
	return total, closed, planPercent(closed, total)
}

// planPercent is the ONE percent-complete primitive every /plans progress chip
// derives its % through: the /plans/{id} header (planProgress), the per-phase
// rollup (planPhases), and the /plans index card (planIndexEnrichTimed row). It
// returns the integer percentage of `total` rendered steps that are `closed`,
// FLOORED (int truncation — 23 of 25 reads 92, not a rounded-up 92).
//
// Routing all three call sites through this function is the lockstep guarantee
// (ka-0000007284). The parent fix (ka-0000004410 / MR !5250) made the index and
// detail COUNT the same rendered step set, but each surface still floored its own
// hand-copied `int(float64(closed)/float64(total)*100)`. Three independent copies
// of a rounding expression is exactly how the index % and the detail % drift
// apart the day one copy is "improved" (e.g. to math.Round) and the others are
// missed — the divergence class this task closes. With one primitive the % is
// identical by construction, and TestPlanPercent_LockstepAcrossSurfaces pins that
// every surface routes through it.
//
// total<=0 yields 0 — a plan with no rendered steps shows no progress — which
// preserves the behavior of the `if total > 0` guards this replaces (a bare
// division would divide by zero and yield NaN → a garbage int cast).
func planPercent(closed, total int) int {
	if total <= 0 {
		return 0
	}
	return int(float64(closed) / float64(total) * 100)
}

// planProse locates the plan's narrative bundle (a child type=bundle bead) and
// renders it with the EXISTING bundle renderer, so plan prose is exactly the
// bundle experience Matt already has — no reinvention. Returns (view, bundleID);
// a nil view means the plan has no bundle yet and the template falls back to the
// epic description as the narrative.
func (s *Server) planProse(ctx context.Context, epicID string, revDeps []beadsapi.Dependency) (*bundleView, string) {
	ids := dedupedParentChildIDs(revDeps)
	if len(ids) == 0 {
		return nil, ""
	}
	children, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		s.logger.Debug("plan prose: batch get", "epic", epicID, "error", err)
		return nil, ""
	}
	var bundle *beadsapi.BeadDetail
	for _, cid := range ids {
		c := children[cid]
		if c != nil && c.Type == "bundle" {
			bundle = c
			break
		}
	}
	if bundle == nil {
		return nil, ""
	}
	// The bundle's own doc children hang off the bundle bead, so fetch its
	// reverse deps + mentions to render it exactly as /bundle/{id} would.
	bRevDeps, _ := s.daemon.GetReverseDependencies(ctx, bundle.ID)
	bMentions, _ := s.daemon.GetMentions(ctx, bundle.ID, 50)
	view := buildBundleView(ctx, bundle, bMentions, bRevDeps, s.daemon, s.newBundleRenderCtx())
	return view, bundle.ID
}
