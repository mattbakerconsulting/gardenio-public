package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/plans"
)

// Phases (ka-0000005926, operator decision ka-0000001034). A plan can group its
// work into ordered PHASE sub-epics — research / design / implementation /
// validation / testing, agents choose the vocab — "much like how we run epics
// today." Each phase is an ordinary direct-child `type=epic` of the plan epic;
// its task/bug/feature/chore children are that phase's steps.
//
// MEMBERSHIP + ORDER both come from ONE field on the plan epic:
// `fields.phase_order`, a JSON array of the phase sub-epic ids in display
// order. A direct-child epic renders as an inline phase IFF its id appears in
// phase_order; a child epic NOT listed there stays a freestanding sub-plan
// (today's behavior, unbroken). This is the phase-detection fork Matt resolved:
// phase-iff-in-list, not every-child-epic (ka-0000001034). The list is
// operator-reorderable (handlePlanPhasesReorder) by rewriting the whole array.
//
// The progress % counts the RENDERED steps: planProgress sums the flat/unphased
// rows plus each phase's own step rows (ka-0000004410). A phase's steps are its
// DIRECT issue children — the rows it renders — so grouping work into phases
// keeps every visible row in the total; it changes the GROUPING, not the count.

// planPhaseFieldKey is the plan-epic field holding the ordered+membership list
// of phase sub-epic ids (a JSON array, decoded from the string-map value).
const planPhaseFieldKey = plans.PhaseOrderField

// planStepFieldKey is the epic field holding the explicit display order of an
// epic's issue-type STEP children — the symmetric sibling of phase_order for
// steps (in-cockpit reorder, ka-0000007445). Unlike phase_order it is ADVISORY,
// not a membership set: a step whose id is absent still renders (after every
// listed step, in creation order). It is stored on whichever epic is the step's
// DIRECT parent — the plan epic for flat steps, the phase sub-epic for phased
// steps — so buildStepRows (detail timeline) and pickCurrentOrNextStep (index
// chip) consult the same list and assign the same #N (ka-0000007589).
const planStepFieldKey = plans.StepOrderField

// planPhaseView is one ordered phase group on the plan cockpit: a direct-child
// sub-epic listed in the plan's phase_order, with its own lifecycle metadata,
// a per-phase progress rollup, and its issue children as steps. Reorder arrows
// (operator-only) key off Number / IsFirst / IsLast.
type planPhaseView struct {
	ID       string
	Title    string
	Status   string
	Closed   bool
	Assignee string
	// UpdatedAt is the phase sub-epic's own last-modified instant, folded into
	// the plan-wide "last change" rollup (planLastActivity, ka-0000002873) so a
	// phase status flip counts as plan activity even before any of its steps
	// change. Not rendered on the phase header itself.
	UpdatedAt time.Time
	// Number is the 1..N position in phase_order (display + reorder anchor).
	Number  int
	IsFirst bool
	IsLast  bool
	// Per-phase step rollup over the sub-epic's OWN issue children (the steps
	// rendered under it). Deliberately the direct-children count, not a
	// transitive subtree — a phase's steps are its direct task children.
	StepsTotal  int
	StepsClosed int
	StepsOpen   int
	PctDone     int
	// Steps are the phase sub-epic's issue children, in stable creation order,
	// numbered 1..N WITHIN the phase.
	Steps []planStepView

	// Provenance (poured-phase chrome, ka-0000002220). A phase whose container is
	// a poured MOLECULE (isPhaseContainer accepts type=molecule, resolved decision
	// ka-0000003759) carries the recipe it was poured from plus its pour-time var
	// bindings — the same formula_id / applied_vars fields the /molecule execution
	// view reads (molecule_view.go). These drive an inline provenance CHIP on the
	// existing phase-header row (NO new card row — the info-density gate), linking
	// the formula recipe and surfacing the applied vars. All three are empty/nil
	// for a hand-authored epic phase, so the chip suppresses itself there.
	//
	// FormulaID   — the recipe bead id (the chip's formula deep-link target).
	// FormulaTitle — the recipe's human title when resolvable (falls back to the id).
	// AppliedVars — the pour-time bindings, sorted by name (parseAppliedVars).
	FormulaID    string
	FormulaTitle string
	AppliedVars  []formulaVar

	// Heraldry is the phase's arms (canonical, via bearerArms) and ArmsBearer
	// whether its type may bear them: an epic phase without arms shows "no arms
	// yet" in the header's arms slot, a poured molecule shows neither
	// (gd-YvWv4mR4fu).
	Heraldry   string
	ArmsBearer bool
}

// planPhaseOrder decodes the plan epic's ordered phase sub-epic ids from
// fields.phase_order; nil (the flat timeline) when absent or unparseable. See
// plans.PhaseOrder.
func planPhaseOrder(epic *beadsapi.BeadDetail) []string {
	return plans.PhaseOrder(epic)
}

// planStepOrder decodes an epic's explicit step display order from
// fields.step_order (ka-0000007445); nil (creation order) when absent. See
// plans.StepOrder.
func planStepOrder(epic *beadsapi.BeadDetail) []string {
	return plans.StepOrder(epic)
}

// sortStepBeads orders one group's step beads by the step_order overlay, then
// creation order. It is the one ordering primitive buildStepRows (detail
// timeline) and pickCurrentOrNextStep (index chip) share, so a step's #N is
// identical on both surfaces (ka-0000007589). See plans.SortStepBeads.
func sortStepBeads(steps []*beadsapi.BeadDetail, order []string) {
	plans.SortStepBeads(steps, order)
}

// resolvedPhaseIDs filters a raw phase_order down to the ids that actually
// render as phases (deduped, current direct-child containers only). planPhases
// numbers the phases over this subset and the phase reorder swaps over it
// (ka-0000002075). See plans.ResolvedPhaseIDs.
func resolvedPhaseIDs(order []string, directChildren map[string]*beadsapi.BeadDetail) []string {
	return plans.ResolvedPhaseIDs(order, directChildren)
}

// planPhases builds the ordered phase groups for a plan, given the plan epic and
// its already-fetched direct children (the same map planSteps builds). It reads
// phase_order for membership+order, keeps only the listed ids that resolve to a
// direct-child epic (skipping stale/deleted ids and non-epics), then descends
// ONE level into each phase sub-epic to fetch its issue children as steps.
//
// Cost: one BatchGetDependencies(reverse) over the phase sub-epic ids + one
// BatchGetBeads over the union of their children — 2 batched round-trips
// regardless of phase count. Enrichment (MR pills, blocker chips) runs over the
// combined grandchild step set in the same batched idiom as planSteps.
//
// Returns (phases, phaseStepIDs) where phaseStepIDs is the flat set of every
// step id that landed under a phase — the detail handler uses it to compute the
// UNPHASED remainder (direct issue children not under any phase) without
// re-deriving membership.
func (s *Server) planPhases(ctx context.Context, epic *beadsapi.BeadDetail, directChildren map[string]*beadsapi.BeadDetail, bundleID string) ([]planPhaseView, map[string]struct{}) {
	order := planPhaseOrder(epic)
	if len(order) == 0 {
		return nil, nil
	}

	// Resolve the ordered ids to direct-child sub-epics (resolvedPhaseIDs skips
	// any id that is not a current direct-child epic — deleted, re-parented, or
	// a typo — deduped first-occurrence-wins). phase_order is advisory display
	// metadata, not an authoritative edge. This is the SAME resolved subset the
	// reorder handler computes its swap over (ka-0000002075), so the ▲▼ arrows /
	// IsFirst / IsLast / Number and the reorder write always agree.
	subEpicIDs := resolvedPhaseIDs(order, directChildren)
	if len(subEpicIDs) == 0 {
		return nil, nil
	}
	type phaseEpic struct {
		id   string
		epic *beadsapi.BeadDetail
	}
	phaseEpics := make([]phaseEpic, 0, len(subEpicIDs))
	for _, id := range subEpicIDs {
		// directChildren[id] is guaranteed a non-nil epic — resolvedPhaseIDs
		// filtered on exactly that.
		phaseEpics = append(phaseEpics, phaseEpic{id: id, epic: directChildren[id]})
	}

	// One batched reverse-deps over all phase sub-epics → their child ids.
	childIDsBySub := make(map[string][]string, len(subEpicIDs))
	unionIDs := make([]string, 0, len(subEpicIDs)*4)
	unionSeen := make(map[string]struct{})
	if revBySub, err := s.daemon.BatchGetDependencies(ctx, subEpicIDs, "reverse"); err != nil {
		s.logger.Debug("plan phases: batch reverse deps", "epic", epic.ID, "error", err)
	} else {
		for _, subID := range subEpicIDs {
			ids := dedupedParentChildIDs(revBySub[subID])
			childIDsBySub[subID] = ids
			for _, id := range ids {
				if _, dup := unionSeen[id]; dup {
					continue
				}
				unionSeen[id] = struct{}{}
				unionIDs = append(unionIDs, id)
			}
		}
	}

	// One batched get over the union of all phase children.
	var grandchildren map[string]*beadsapi.BeadDetail
	if len(unionIDs) > 0 {
		got, err := s.daemon.BatchGetBeads(ctx, unionIDs)
		if err != nil {
			s.logger.Debug("plan phases: batch get children", "epic", epic.ID, "error", err)
		} else {
			grandchildren = got
		}
	}

	phaseStepIDs := make(map[string]struct{})
	phases := make([]planPhaseView, 0, len(phaseEpics))
	// Collect every step across phases for a single combined MR + blocker
	// enrichment pass (batched, matching planSteps). Enriched rows are
	// scattered back into each phase's Steps slice by id afterward, so we only
	// need the flat set here — not a phase-index sidecar.
	var allSteps []planStepView

	for pIdx, pe := range phaseEpics {
		// Each phase sub-epic carries its OWN step_order overlay for the steps
		// rendered under it (ka-0000007445) — reordering a step within a phase
		// rewrites the phase epic's field, not the plan epic's.
		steps := buildStepRows(childIDsBySub[pe.id], grandchildren, planStepOrder(pe.epic))
		total, closed := 0, 0
		for i := range steps {
			phaseStepIDs[steps[i].ID] = struct{}{}
			total++
			if steps[i].Closed {
				closed++
			}
			allSteps = append(allSteps, steps[i])
		}
		// The shared floor-percent primitive (ka-0000007284): a phase's own % is
		// computed exactly as the plan header % (planProgress) and the index card %
		// (planIndexEnrichTimed), so a phase chip can never round differently from
		// the surfaces that sum it.
		pct := planPercent(closed, total)
		// Poured-phase provenance (ka-0000002220): a molecule container carries the
		// recipe it was poured from + its pour-time var bindings, the same fields
		// the /molecule execution view reads. A hand-authored epic phase has
		// neither, so FormulaID stays "" and the chip suppresses itself. FormulaTitle
		// is resolved in one batched pass below.
		phases = append(phases, planPhaseView{
			ID:          pe.id,
			Title:       pe.epic.Title,
			Status:      pe.epic.Status,
			Closed:      pe.epic.Status == "closed",
			Assignee:    pe.epic.Assignee,
			UpdatedAt:   pe.epic.UpdatedAt,
			Number:      pIdx + 1,
			IsFirst:     pIdx == 0,
			IsLast:      pIdx == len(phaseEpics)-1,
			StepsTotal:  total,
			StepsClosed: closed,
			StepsOpen:   total - closed,
			PctDone:     pct,
			Steps:       steps,
			FormulaID:   strings.TrimSpace(pe.epic.Fields["formula_id"]),
			AppliedVars: parseAppliedVars(pe.epic.Fields["applied_vars"]),
			Heraldry:    bearerArms(pe.epic),
			ArmsBearer:  armsBearerTypes[pe.epic.Type],
		})
	}

	// Resolve each poured phase's recipe title in ONE batched fetch (the chip
	// deep-links the formula and shows its human title). Best-effort: a phase with
	// no formula_id is skipped, and a failed/absent formula fetch leaves the chip
	// linking the bare id — the phase always renders. This keeps planPhases at a
	// bounded, batched round-trip count (formula titles fetched once, not per phase).
	s.resolvePhaseFormulaTitles(ctx, phases)

	// Combined enrichment over the full phase-step set (MR pills + blocker
	// chips + § cross-ref chips), then scatter results back into each phase's
	// Steps slice by id. grandchildren is the "known" bead map for intra-phase
	// blocker reuse and the description source for the cross-ref scan.
	s.enrichStepRows(ctx, allSteps, grandchildren)
	enrichStepSectionRefs(allSteps, grandchildren, bundleID)
	byID := make(map[string]planStepView, len(allSteps))
	for _, st := range allSteps {
		byID[st.ID] = st
	}
	for pi := range phases {
		for si := range phases[pi].Steps {
			if enriched, ok := byID[phases[pi].Steps[si].ID]; ok {
				phases[pi].Steps[si] = enriched
			}
		}
	}

	return phases, phaseStepIDs
}

// resolvePhaseFormulaTitles fills FormulaTitle on every poured phase (one whose
// FormulaID is set) from ONE batched formula fetch, so the provenance chip can
// show the recipe's human title instead of only its bead id (ka-0000002220).
// Best-effort: a phase with no formula_id is untouched; a formula that fails to
// fetch (deleted recipe, error) leaves FormulaTitle empty and the chip links the
// bare id. Distinct formula ids are fetched once, so N phases pouring the same
// recipe cost one lookup, not N.
func (s *Server) resolvePhaseFormulaTitles(ctx context.Context, phases []planPhaseView) {
	ids := make([]string, 0, len(phases))
	seen := make(map[string]struct{}, len(phases))
	for i := range phases {
		fid := phases[i].FormulaID
		if fid == "" {
			continue
		}
		if _, dup := seen[fid]; dup {
			continue
		}
		seen[fid] = struct{}{}
		ids = append(ids, fid)
	}
	if len(ids) == 0 {
		return
	}
	formulas, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		s.logger.Debug("plan phases: batch get formula titles", "error", err)
		return
	}
	for i := range phases {
		if f := formulas[phases[i].FormulaID]; f != nil {
			phases[i].FormulaTitle = f.Title
		}
	}
}

// handlePlanPhaseReorder moves one phase sub-epic up or down within the plan
// epic's fields.phase_order and re-renders the #plan-steps fragment. The
// reordered array is written back through the daemon's compare-and-swap primitive
// (ReorderBeadArrayField, ka-0000000988): a reorder is a blind full-array rewrite
// that under naive read-modify-write silently drops a concurrent phase add, so
// the CAS makes the rewrite conditional on the stored order being unchanged and
// reorderArrayFieldWithRetry re-reads + re-derives on a 409. Operator-gated at the
// route layer; re-checks the email defense-in-depth and stamps the operator on
// the ctx (kbeads audit resolver).
//
// Browser write-proxy (carve-out ka-0000004319; canonical banner in
// plans_lifecycle.go). Owns no plan invariant: the CAS reorder primitive is the
// daemon's (kbeads http_beads.go ReorderBeadArrayField), and this is the SAME
// compare-and-swap-with-retry composition `gb plan reorder-phase` performs
// daemon-direct (cmd/gb/plan_write.go reorderPlanPhase).
func (s *Server) handlePlanPhaseReorder(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	phaseID := strings.TrimSpace(r.PathValue("phase"))
	if id == "" || phaseID == "" {
		http.Error(w, "Missing plan or phase id", http.StatusBadRequest)
		return
	}
	dir := strings.TrimSpace(r.URL.Query().Get("dir"))
	if dir != "up" && dir != "down" {
		http.Error(w, "dir must be up or down", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	// A reorder that lands and a boundary/absent no-op both re-render the same
	// current #plan-steps fragment, so a double-click at the edge is harmless.
	s.respondPlanStepsWrite(w, r, id, s.plansService(r).ReorderPhase(r.Context(), id, phaseID, dir, planActor(email)))
}

// buildStepRows materializes a set of issue children (by id, resolved against a
// bead map) into planStepView rows in display order, numbered 1..N over that
// order. Extracted from planSteps so both the flat timeline and each phase group
// number their steps identically. order is the parent epic's explicit
// fields.step_order overlay (ka-0000007445); nil/empty ⇒ pure creation order
// (the pre-reorder behavior). MR/blocker enrichment is applied separately by the
// caller (enrichStepRows) so it can be batched across phases.
func buildStepRows(childIDs []string, beads map[string]*beadsapi.BeadDetail, order []string) []planStepView {
	stepBeads := make([]*beadsapi.BeadDetail, 0, len(childIDs))
	for _, cid := range childIDs {
		c := beads[cid]
		if c == nil || !isIssueType(c.Type) {
			continue
		}
		stepBeads = append(stepBeads, c)
	}
	if len(stepBeads) == 0 {
		return nil
	}
	// Order by the explicit step_order overlay first, then stable creation order
	// for the remainder (sortStepBeads) — the SAME primitive pickCurrentOrNextStep
	// uses, so the index chip's #N matches the detail row's #N (ka-0000007589).
	sortStepBeads(stepBeads, order)
	steps := make([]planStepView, 0, len(stepBeads))
	for i, c := range stepBeads {
		st := planStepView{
			ID:         c.ID,
			Title:      c.Title,
			ShortTitle: c.ShortTitle,
			Type:       c.Type,
			Status:     c.Status,
			Priority:   c.Priority,
			Closed:     c.Status == "closed",
			Number:     i + 1,
			IsFirst:    i == 0,
			IsLast:     i == len(stepBeads)-1,
			Assignee:   c.Assignee,
			UpdatedAt:  c.UpdatedAt,
		}
		if !c.CreatedAt.IsZero() {
			st.CreatedAgo = humanizeTime(c.CreatedAt)
			st.CreatedAtRFC = c.CreatedAt.UTC().Format(time.RFC3339)
		}
		if st.Closed {
			if at, ok := parseThreadClosedAt(c.ClosedAt); ok {
				st.ClosedAgo = humanizeTime(at)
				st.ClosedAtRFC = at.UTC().Format(time.RFC3339)
			}
		}
		steps = append(steps, st)
	}
	return steps
}

// enrichStepSectionRefs fills each step's SectionRef cross-ref chip (ask 2,
// ka-0000005926) by scanning its description for a `see ka-BUNDLE#section` ref
// whose bare bead id is the plan's narrative bundle. The FIRST such anchored ref
// wins. No-op when the plan has no bundle, the step bead is unknown, or the
// description carries no matching anchored ref — the chip suppresses itself.
//
// Anchors are render-only by construction: the kbeads reference extractor
// captures only the bare ka-id (the `#anchor` is never part of the match), so
// there is no stored edge to read — the ref must be lifted from the description
// text here, reusing the same beadIDRe / splitBeadAnchor / anchorHrefSuffix
// pipeline the bundle linkifier uses so `#C1.1` / `#c1-1` resolve identically.
func enrichStepSectionRefs(steps []planStepView, beads map[string]*beadsapi.BeadDetail, bundleID string) {
	if bundleID == "" || len(steps) == 0 {
		return
	}
	for i := range steps {
		b := beads[steps[i].ID]
		if b == nil || b.Description == "" {
			continue
		}
		if ref, ok := firstBundleSectionRef(b.Description, bundleID); ok {
			steps[i].SectionRef = ref
		}
	}
}

// firstBundleSectionRef finds the first `ka-BUNDLE#anchor` reference in text
// whose bare id equals bundleID AND that carries a non-empty anchor label, and
// returns it as a chip view. A bare `ka-BUNDLE` with no `#anchor` is ignored —
// the chip's whole point is to deep-link a SECTION, and the bundle itself is
// already the narrative rendered below the steps.
func firstBundleSectionRef(text, bundleID string) (planSectionRefView, bool) {
	for _, match := range beadIDRe.FindAllString(text, -1) {
		id, label := splitBeadAnchor(match)
		if id != bundleID || label == "" {
			continue
		}
		suffix := anchorHrefSuffix(label)
		if suffix == "" {
			continue
		}
		// suffix is "#<slug>" (anchorHrefSuffix always prefixes `#`); Slug is that
		// bare token, matching the `id` minted on the narrative section heading so
		// plan_gutter.js can map a highlighted passage back to this step.
		return planSectionRefView{Label: label, Href: bundleID + suffix, Slug: strings.TrimPrefix(suffix, "#")}, true
	}
	return planSectionRefView{}, false
}

// enrichStepRows fills the linked-MR pill and open-blocker chips on a step set
// in the same batched idiom planSteps uses inline: one produces-walk via the
// inbox MR resolver, one forward blocks-edge walk. known is the already-fetched
// bead map (phase children) reused for intra-set blocker resolution. Safe on an
// empty slice.
func (s *Server) enrichStepRows(ctx context.Context, steps []planStepView, known map[string]*beadsapi.BeadDetail) {
	if len(steps) == 0 {
		return
	}
	stepBeads := make([]*beadsapi.BeadDetail, 0, len(steps))
	for i := range steps {
		if b := known[steps[i].ID]; b != nil {
			stepBeads = append(stepBeads, b)
		}
	}
	mr := newInboxMRResolver()
	mr.Prewarm(ctx, s.daemon, stepBeads)
	for i := range steps {
		info := mr.Lookup(steps[i].ID)
		steps[i].MRState = info.State
		steps[i].MRURL = info.URL
	}
	s.enrichPlanStepBlockers(ctx, steps, known)
	s.enrichStepAgents(ctx, steps)
}

// enrichStepAgents overrides each step's displayed Assignee with the agent that
// most recently worked it, resolved from the step's `assigned` reverse-dep edges
// rather than trusting the raw assignee scalar.
//
// WHY (ka-0000000647): the assignee scalar goes STALE when more than one agent
// works a step. `kd claim` refuses to overwrite a step still assigned to a
// different agent (checkClaimable, kbeads/cmd/kd/claim.go), so a second /
// respawned crew leaves the scalar pointing at the FIRST agent — the /plans
// "Build steps" chip then shows the oldest agent, not the current one. Every
// spawn stamps a fresh `assigned` edge (agent -> step) via SpawnAgent, so the
// live / most-recent worker is the best of those edges. This mirrors
// planAssistant/pickBetterAssistant, which solves the identical multi-bind
// problem for the plan assistant.
//
// Preference: a live agent (agent_state alive AND not stale) beats a parked one;
// among same-liveness beads the freshest UpdatedAt wins. A step with no
// `assigned` edge — or whose agent beads can't be fetched — keeps its scalar
// assignee (so a human-assigned step, which never gets an agent edge, is
// untouched).
//
// Batched: ONE reverse-dep walk over the whole step set + ONE BatchGetBeads over
// the distinct agent bead ids, so a plan with N steps costs 2 round-trips, not
// 2N. Errors degrade to "keep the scalar" — the chip never breaks.
func (s *Server) enrichStepAgents(ctx context.Context, steps []planStepView) {
	if len(steps) == 0 {
		return
	}
	stepIDs := make([]string, 0, len(steps))
	for i := range steps {
		stepIDs = append(stepIDs, steps[i].ID)
	}
	depsByStep, err := s.daemon.BatchGetDependencies(ctx, stepIDs, "reverse")
	if err != nil {
		s.logger.Debug("plan steps: agent edge walk", "error", err)
		return
	}
	// Collect the distinct agent-bead ids across every step's `assigned` edges.
	agentIDs := make([]string, 0)
	seen := make(map[string]struct{})
	for _, id := range stepIDs {
		for _, d := range depsByStep[id] {
			if d.Type != "assigned" || d.BeadID == "" {
				continue
			}
			if _, dup := seen[d.BeadID]; dup {
				continue
			}
			seen[d.BeadID] = struct{}{}
			agentIDs = append(agentIDs, d.BeadID)
		}
	}
	if len(agentIDs) == 0 {
		return
	}
	agents, err := s.daemon.BatchGetBeads(ctx, agentIDs)
	if err != nil {
		s.logger.Debug("plan steps: batch get agent beads", "error", err)
		return
	}
	now := s.now()
	for i := range steps {
		var best *beadsapi.BeadDetail
		for _, d := range depsByStep[steps[i].ID] {
			if d.Type != "assigned" || d.BeadID == "" {
				continue
			}
			cand := agents[d.BeadID]
			if cand == nil || cand.Type != "agent" {
				continue
			}
			best = pickLiveOrFreshestAgent(best, cand, now)
		}
		if best == nil {
			continue
		}
		name := strings.TrimSpace(best.Fields["agent"])
		if name == "" {
			name = strings.TrimSpace(best.Title)
		}
		if name != "" {
			steps[i].Assignee = name
		}
	}
}

// pickLiveOrFreshestAgent returns the agent bead that should represent a step
// when more than one worked it: a live one (agent_state in agentStateAlive AND
// not stale) beats a parked one, and among same-liveness beads the freshest
// UpdatedAt wins. Generic sibling of pickBetterAssistant (which adds the
// plan-assistant role/plan-binding scope) — here any agent carrying an
// `assigned` edge to the step is a candidate. now is threaded in (rather than
// calling s.now()) so the choice is deterministic across a single render.
func pickLiveOrFreshestAgent(cur, cand *beadsapi.BeadDetail, now time.Time) *beadsapi.BeadDetail {
	if cur == nil {
		return cand
	}
	if cand == nil {
		return cur
	}
	curLive := isAgentStateIn(cur.Fields["agent_state"], agentStateAlive) && !isAgentStale(cur, now)
	candLive := isAgentStateIn(cand.Fields["agent_state"], agentStateAlive) && !isAgentStale(cand, now)
	if curLive != candLive {
		if candLive {
			return cand
		}
		return cur
	}
	if cand.UpdatedAt.After(cur.UpdatedAt) {
		return cand
	}
	return cur
}
