package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// The window view (narrative PW-D4): GET /v1/windows/self/view returns this
// projection of the plan a window is on, and nothing else.
//
// The closure is the plan epic, its parent-child descendants except beads of
// type window, and the type=agent beads reached by a reverse assigned edge
// from a closure bead. The narrative bundle and its docs and the plan's
// decision and escalation children are descendants, so the one subtree query
// holds them.
//
// The projection is a struct-field allowlist: every value below is copied
// field by field from a bead into a type that has no other field, so a field
// added to a bead never leaves the daemon until someone adds it here. It
// carries no bead id (steps are positional), no description, decision prompt
// or context, comment, transcript, spend, human name or email (created_by,
// operated_by, requested_by, assignee), MR URL or session id, and never an
// escalation's title (a bridge escalation's title embeds the question or the
// command). Render-time hygiene over the free text (titles, narrative) is the
// viewer's (PW.2).

// windowClosureMax bounds the subtree a view walks. A plan is plan → phase →
// step (→ decision, escalation), plus bundle → doc; past this the view is
// refused rather than served partial.
const windowClosureMax = 2000

// windowRecentMax is the length of the Recent strip.
const windowRecentMax = 10

// errWindowTargetGone means the window's plan no longer resolves to an epic.
var errWindowTargetGone = errors.New("window target is not a plan epic")

// errWindowClosureTooLarge means the plan's subtree exceeds windowClosureMax.
var errWindowClosureTooLarge = errors.New("window closure too large")

// windowView is the body of GET /v1/windows/self/view.
type windowView struct {
	Plan        windowPlan        `json:"plan"`
	Phases      []windowPhase     `json:"phases"`
	Steps       []windowStep      `json:"steps"`
	Decisions   []windowDecision  `json:"decisions"`
	Escalations windowEscalations `json:"escalations"`
	Crew        []windowAgent     `json:"crew"`
	Narrative   windowNarrative   `json:"narrative"`
	Recent      []windowChange    `json:"recent"`
	ExpiresAt   time.Time         `json:"expires_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// windowPlan is the plan's headline: title, state, current phase, progress.
// CurrentPhase is the 1-based number of the first phase with a step not
// closed (else the first phase not closed), 0 when there is none.
type windowPlan struct {
	Title        string `json:"title"`
	ShortTitle   string `json:"short_title,omitempty"`
	Heraldry     string `json:"heraldry,omitempty"`
	Status       string `json:"status"`
	State        string `json:"state,omitempty"`
	CurrentPhase int    `json:"current_phase"`
	StepsDone    int    `json:"steps_done"`
	StepsTotal   int    `json:"steps_total"`
}

// windowPhase is one phase in phase_order with its steps.
type windowPhase struct {
	Number     int          `json:"number"`
	Title      string       `json:"title"`
	ShortTitle string       `json:"short_title,omitempty"`
	Status     string       `json:"status"`
	Steps      []windowStep `json:"steps"`
}

// windowStep is one step, numbered within its phase (or the flat list).
type windowStep struct {
	Number     int        `json:"number"`
	Title      string     `json:"title"`
	ShortTitle string     `json:"short_title,omitempty"`
	Status     string     `json:"status"`
	Kind       string     `json:"kind"`
	DoneAt     *time.Time `json:"done_at,omitempty"`
}

// windowDecision is a decision in the closure: its title, its state (open,
// resolved or cancelled) and, once resolved, the chosen option's label.
type windowDecision struct {
	Title  string `json:"title"`
	State  string `json:"state"`
	Chosen string `json:"chosen,omitempty"`
}

// windowEscalations counts the closure's escalations. Items carry the
// operation type, sensitivity and state, never a title.
type windowEscalations struct {
	Open  int                `json:"open"`
	Total int                `json:"total"`
	Items []windowEscalation `json:"items"`
}

type windowEscalation struct {
	OperationType string `json:"operation_type"`
	Sensitivity   string `json:"sensitivity,omitempty"`
	State         string `json:"state"`
}

// windowAgent is one open agent on the plan: its name and whether it is
// working now.
type windowAgent struct {
	Name string `json:"name"`
	Live bool   `json:"live"`
}

// windowNarrative is the plan's narrative bundle: its headline and its
// markdown docs in position order.
type windowNarrative struct {
	Headline string      `json:"headline,omitempty"`
	Docs     []windowDoc `json:"docs"`
}

type windowDoc struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

// windowChange is one row of the Recent strip. Subject is the step's or
// decision's title, or an escalation's operation type.
type windowChange struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Subject string    `json:"subject"`
}

// Recent strip kinds.
const (
	windowChangeStepClosed        = "step_closed"
	windowChangeDecisionOpened    = "decision_opened"
	windowChangeDecisionResolved  = "decision_resolved"
	windowChangeDecisionCancelled = "decision_cancelled"
	windowChangeEscalationOpened  = "escalation_opened"
	windowChangeEscalationClosed  = "escalation_closed"
)

// windowLiveAgentStates are the agent_state values the crew strip shows live.
var windowLiveAgentStates = map[string]bool{
	"spawning": true, "assigning": true, "working": true, "resuming": true,
}

// buildWindowView walks the closure of the window's plan and projects it. It
// reads the store directly: the window principal has no read scope of its
// own, and the closure is the whole of what it may see.
func (s *BeadsServer) buildWindowView(ctx context.Context, wf windowFieldSet) (*windowView, error) {
	plan, err := s.store.GetBead(ctx, wf.Target)
	if err != nil || plan == nil || plan.Type != model.TypeEpic {
		return nil, errWindowTargetGone
	}
	v, _, err := s.projectWindowPlan(ctx, plan, windowPlanFilter(), windowClosureMax)
	if err != nil {
		return nil, err
	}
	v.ExpiresAt = wf.expiresAt()
	return v, nil
}

// projectWindowPlan walks a plan's closure through base (windowPlanFilter or
// windowLandFilter), refusing one past limit beads with
// errWindowClosureTooLarge, and projects it. It returns the closure's size.
// ExpiresAt is the caller's.
func (s *BeadsServer) projectWindowPlan(ctx context.Context, plan *model.Bead, base model.BeadFilter, limit int) (*windowView, int, error) {
	desc, err := s.windowPlanSubtree(ctx, plan, base, limit)
	if err != nil {
		return nil, 0, err
	}
	reads, err := s.readWindowClosure(ctx, windowClosureIDs(plan, desc))
	if err != nil {
		return nil, 0, err
	}
	return projectWindowClosure(plan, desc, reads), len(desc), nil
}

// windowPlanFilter is a plan window's subtree read: every descendant but
// windows.
func windowPlanFilter() model.BeadFilter {
	return model.BeadFilter{ExcludeTypes: []model.BeadType{typeWindow}}
}

// windowLandFilter is a land view's subtree read: only what landPlanOf keeps.
// That is the phases and steps, the open decisions and escalations, and the
// fields the projection reads. Closed decisions and escalations, evidence,
// reports, docs and the bundle feed only a plan window's Recent and
// narrative, so the read does not grow with a plan's history (gd-FC7tIACC5f).
func windowLandFilter() model.BeadFilter {
	return model.BeadFilter{
		Type: []model.BeadType{
			model.TypeEpic, model.TypeMolecule, model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeChore,
			model.TypeDecision, model.TypeEscalation,
		},
		ExcludeClosedTypes: []model.BeadType{model.TypeDecision, model.TypeEscalation},
		FieldsOnly:         []string{"plan_state", "phase_order", "step_order", "operation_type", "sensitivity"},
		OmitDescription:    true,
		OmitNotes:          true,
		NoTotal:            true,
	}
}

// windowPlanSubtree lists a plan's parent-child descendants through base,
// refusing more than limit with errWindowClosureTooLarge.
func (s *BeadsServer) windowPlanSubtree(ctx context.Context, plan *model.Bead, base model.BeadFilter, limit int) ([]*model.Bead, error) {
	f := base
	f.ParentSubtreeID, f.Limit = plan.ID, limit+1
	desc, _, err := s.store.ListBeads(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("listing plan subtree: %w", err)
	}
	if len(desc) > limit {
		return nil, errWindowClosureTooLarge
	}
	return desc, nil
}

// windowClosureReads is what a projection reads besides the closure's beads:
// each bead's parent edges, the assigned edges onto it, and the agents on
// those edges. One read serves a single plan or a whole land.
type windowClosureReads struct {
	parents  map[string][]*model.Dependency
	assigned map[string][]*model.Dependency
	agents   map[string]*model.Bead
}

// windowClosureIDs is plan and desc's ids, deduped, without windows.
func windowClosureIDs(plan *model.Bead, desc []*model.Bead) []string {
	seen := make(map[string]bool, len(desc)+1)
	ids := make([]string, 0, len(desc)+1)
	for _, b := range append([]*model.Bead{plan}, desc...) {
		if b == nil || b.Type == typeWindow || seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		ids = append(ids, b.ID)
	}
	return ids
}

// readWindowClosure reads the edges and agents of ids in three queries,
// however many plans the ids span.
func (s *BeadsServer) readWindowClosure(ctx context.Context, ids []string) (*windowClosureReads, error) {
	parents, err := s.store.GetDependenciesForBeads(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reading closure edges: %w", err)
	}
	rev, err := s.store.GetReverseDependenciesForBeads(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reading assigned edges: %w", err)
	}
	seen := map[string]bool{}
	var agentIDs []string
	for _, id := range ids {
		for _, d := range rev[id] {
			if d == nil || d.Type != windowAssignedDep || seen[d.BeadID] {
				continue
			}
			seen[d.BeadID] = true
			agentIDs = append(agentIDs, d.BeadID)
		}
	}
	// Only the open agents: an assigned edge outlives its agent, so the closed
	// ones grow with the plan's history and windowCrew drops them anyway.
	agents := map[string]*model.Bead{}
	if len(agentIDs) > 0 {
		got, _, err := s.store.ListBeads(ctx, model.BeadFilter{
			IDs:                agentIDs,
			Type:               []model.BeadType{typeAgent},
			ExcludeClosedTypes: []model.BeadType{typeAgent},
			FieldsOnly:         []string{"agent", "agent_state"},
			OmitDescription:    true,
			OmitNotes:          true,
			NoTotal:            true,
		})
		if err != nil {
			return nil, fmt.Errorf("reading crew: %w", err)
		}
		for _, a := range got {
			if a != nil {
				agents[a.ID] = a
			}
		}
	}
	return &windowClosureReads{parents: parents, assigned: rev, agents: agents}, nil
}

// projectWindowClosure projects a plan from its closure (desc) and the reads
// over it. It reads nothing.
func projectWindowClosure(plan *model.Bead, desc []*model.Bead, reads *windowClosureReads) *windowView {
	byID := make(map[string]*model.Bead, len(desc)+1)
	ids := make([]string, 0, len(desc)+1)
	for _, b := range append([]*model.Bead{plan}, desc...) {
		if b == nil || b.Type == typeWindow || byID[b.ID] != nil {
			continue
		}
		byID[b.ID] = b
		ids = append(ids, b.ID)
	}
	children := map[string][]*model.Bead{}
	for _, id := range ids {
		for _, d := range reads.parents[id] {
			if d == nil || !model.IsParentChildDepType(string(d.Type)) || byID[d.DependsOnID] == nil {
				continue
			}
			children[d.DependsOnID] = append(children[d.DependsOnID], byID[id])
		}
	}
	for _, kids := range children {
		slices.SortStableFunc(kids, func(a, b *model.Bead) int {
			return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
		})
	}

	v := &windowView{
		Plan: windowPlan{
			Title: plan.Title, ShortTitle: plan.ShortTitle, Heraldry: plan.Heraldry,
			Status: string(plan.Status), State: beadFieldString(plan, "plan_state"),
		},
		Phases: []windowPhase{}, Steps: []windowStep{}, Decisions: []windowDecision{},
		Escalations: windowEscalations{Items: []windowEscalation{}},
		Crew:        []windowAgent{}, Narrative: windowNarrative{Docs: []windowDoc{}},
	}
	var recent []windowChange

	// Phases: the phase_order ids that are direct epic children, deduped.
	claimed := map[string]bool{}
	direct := map[string]bool{}
	for _, c := range children[plan.ID] {
		direct[c.ID] = true
	}
	for _, pid := range beadFieldIDList(plan, "phase_order") {
		ph := byID[pid]
		if ph == nil || !direct[pid] || claimed[pid] || (ph.Type != model.TypeEpic && ph.Type != model.TypeMolecule) {
			continue
		}
		claimed[pid] = true
		steps := windowSteps(children[pid], beadFieldIDList(ph, "step_order"), claimed, &recent)
		v.Phases = append(v.Phases, windowPhase{
			Number: len(v.Phases) + 1, Title: ph.Title, ShortTitle: ph.ShortTitle,
			Status: string(ph.Status), Steps: steps,
		})
	}
	v.Steps = windowSteps(children[plan.ID], beadFieldIDList(plan, "step_order"), claimed, &recent)

	for i, ph := range v.Phases {
		open := slices.ContainsFunc(ph.Steps, func(st windowStep) bool { return st.Status != string(model.StatusClosed) })
		if open && v.Plan.CurrentPhase == 0 {
			v.Plan.CurrentPhase = i + 1
		}
	}
	if v.Plan.CurrentPhase == 0 {
		for i, ph := range v.Phases {
			if ph.Status != string(model.StatusClosed) {
				v.Plan.CurrentPhase = i + 1
				break
			}
		}
	}
	for _, steps := range append([][]windowStep{v.Steps}, phaseSteps(v.Phases)...) {
		for _, st := range steps {
			v.Plan.StepsTotal++
			if st.Status == string(model.StatusClosed) {
				v.Plan.StepsDone++
			}
		}
	}

	// Decisions, escalations and the narrative, anywhere in the closure, in
	// creation order.
	ordered := slices.Clone(desc)
	slices.SortStableFunc(ordered, func(a, b *model.Bead) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	for _, b := range ordered {
		switch b.Type {
		case model.TypeDecision:
			d := windowDecisionOf(b)
			v.Decisions = append(v.Decisions, d)
			recent = append(recent, windowChange{At: b.CreatedAt, Kind: windowChangeDecisionOpened, Subject: b.Title})
			if b.ClosedAt != nil {
				kind := windowChangeDecisionResolved
				if d.State == "cancelled" {
					kind = windowChangeDecisionCancelled
				}
				recent = append(recent, windowChange{At: *b.ClosedAt, Kind: kind, Subject: b.Title})
			}
		case model.TypeEscalation:
			e := windowEscalation{
				OperationType: beadFieldString(b, "operation_type"),
				Sensitivity:   beadFieldString(b, "sensitivity"),
				State:         "open",
			}
			if b.Status == model.StatusClosed {
				e.State = "closed"
			} else {
				v.Escalations.Open++
			}
			v.Escalations.Total++
			v.Escalations.Items = append(v.Escalations.Items, e)
			recent = append(recent, windowChange{At: b.CreatedAt, Kind: windowChangeEscalationOpened, Subject: e.OperationType})
			if b.ClosedAt != nil {
				recent = append(recent, windowChange{At: *b.ClosedAt, Kind: windowChangeEscalationClosed, Subject: e.OperationType})
			}
		}
	}
	if bundle := windowNarrativeBundle(children[plan.ID]); bundle != nil {
		v.Narrative.Headline = beadFieldString(bundle, "headline")
		v.Narrative.Docs = windowDocs(children[bundle.ID])
	}

	v.Crew = windowCrew(ids, reads)

	slices.SortStableFunc(recent, func(a, b windowChange) int { return b.At.Compare(a.At) })
	v.Recent = recent[:min(len(recent), windowRecentMax)]
	if v.Recent == nil {
		v.Recent = []windowChange{}
	}
	for _, b := range byID {
		if b.UpdatedAt.After(v.UpdatedAt) {
			v.UpdatedAt = b.UpdatedAt
		}
	}
	return v
}

// The land view (LW.1 gd-BXNhQf4ZqV, plan gd-4iVYF9tGsn): a land window's
// GET /v1/windows/self/view returns this projection of the land's plans, and
// nothing else. It is built from the store, never from the farm document
// beads-viewer builds with service credentials.
//
// A plan is in the land when it is a type=epic labelled plan whose one
// project label is the window's land; a plan with a second project label is
// in no land and never shows. The view holds every open plan (at most
// windowLandPlansMax, the most recently updated) in creation order, then the
// last windowLandClosedMax closed ones, newest first. Each plan is projected
// by projectWindowClosure and cut down to the allowlist below: the headline, the
// phases and steps, the open decisions' titles, counts of open decisions and
// escalations (never an escalation's title), and the crew. It carries no bead
// id, no land but its own, no narrative, no Recent, no cost, comment, body or
// email. The walk reads only the beads windowLandFilter keeps, bounded by
// windowLandClosureMax across the land; a plan past what is left of it (or
// past windowClosureMax alone) shows only its headline, with withheld set.

const (
	// windowLandPlansMax caps the open plans a land view shows.
	windowLandPlansMax = 60
	// windowLandClosedMax is how many closed plans a land view shows.
	windowLandClosedMax = 3
	// windowLandClosureMax bounds the beads a land view walks across its
	// plans' closures.
	windowLandClosureMax = 5 * windowClosureMax
)

// windowLandView is the body of GET /v1/windows/self/view for a land window.
// OpenPlans counts the land's open plans, which may exceed those shown (a plan
// in two lands that falls past windowLandPlansMax counts, but never shows).
type windowLandView struct {
	Land      string           `json:"land"`
	Plans     []windowLandPlan `json:"plans"`
	OpenPlans int              `json:"open_plans"`
	ExpiresAt time.Time        `json:"expires_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// windowLandPlan is one plan in a land view: windowPlan's headline, then the
// plan's phases and unphased steps, its open decisions by title, its open
// decision and escalation counts, and its crew. Withheld means the plan's
// closure was past the walk's bound, so only the headline is filled.
type windowLandPlan struct {
	windowPlan
	Phases          []windowPhase `json:"phases"`
	Steps           []windowStep  `json:"steps"`
	OpenDecisions   int           `json:"open_decisions"`
	OpenEscalations int           `json:"open_escalations"`
	Decisions       []string      `json:"decisions"`
	Crew            []windowAgent `json:"crew"`
	Withheld        bool          `json:"withheld,omitempty"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// buildLandWindowView projects the land a window is on (see above). The
// window's target must still be the land's type=project bead; that is checked
// on every read. The plans come from s.landViews, so a land's are read at most
// once per windowLandViewTTL, however many windows read them.
func (s *BeadsServer) buildLandWindowView(ctx context.Context, wf windowFieldSet) (*windowLandView, error) {
	proj, err := s.store.GetBead(ctx, wf.Target)
	if err != nil || proj == nil || proj.Type != model.TypeProject || wf.Land == "" || projectNameFromTitle(proj.Title) != wf.Land {
		return nil, errWindowTargetGone
	}
	land := wf.Land
	v, err := s.landViews.get(ctx, land, func(ctx context.Context) (*windowLandView, error) {
		return s.readLandWindowView(ctx, land)
	})
	if err != nil {
		return nil, err
	}
	out := *v
	out.ExpiresAt = wf.expiresAt()
	if proj.UpdatedAt.After(out.UpdatedAt) {
		out.UpdatedAt = proj.UpdatedAt
	}
	return &out, nil
}

// readLandWindowView reads and projects land's plans. ExpiresAt is the
// caller's, and UpdatedAt is the plans' alone.
func (s *BeadsServer) readLandWindowView(ctx context.Context, land string) (*windowLandView, error) {
	open, openTotal, err := s.landWindowPlans(ctx, land, []model.Status{
		model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred, model.StatusPartial,
	}, windowLandPlansMax)
	if err != nil {
		return nil, err
	}
	closed, _, err := s.landWindowPlans(ctx, land, []model.Status{model.StatusClosed}, windowLandClosedMax)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(open, func(a, b *model.Bead) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	v := &windowLandView{
		Land: land, Plans: make([]windowLandPlan, 0, len(open)+len(closed)),
		OpenPlans: openTotal,
	}
	plans, err := s.projectLandPlans(ctx, append(open, closed...))
	if err != nil {
		return nil, err
	}
	for _, lp := range plans {
		v.Plans = append(v.Plans, lp)
		if lp.UpdatedAt.After(v.UpdatedAt) {
			v.UpdatedAt = lp.UpdatedAt
		}
	}
	return v, nil
}

// landWindowPlans lists the land's plans in statuses, most recently updated
// first, at most limit, with the store's total. A bead the label filter let
// through that is not a plan epic in exactly this one land is dropped (and
// not counted) here, so no other land's plan reaches the view.
func (s *BeadsServer) landWindowPlans(ctx context.Context, land string, statuses []model.Status, limit int) ([]*model.Bead, int, error) {
	beads, total, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:            []model.BeadType{model.TypeEpic},
		Labels:          []string{"plan", "project:" + land},
		Status:          statuses,
		Sort:            "-updated_at",
		Limit:           limit,
		FieldsOnly:      []string{"plan_state", "phase_order", "step_order"},
		OmitDescription: true,
		OmitNotes:       true,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("listing the land's plans: %w", err)
	}
	out := beads[:0:0]
	for _, b := range beads {
		if b == nil || b.Type != model.TypeEpic || !slices.Contains(b.Labels, "plan") || !slices.Equal(projectLabelValues(b.Labels), []string{land}) {
			total--
			continue
		}
		out = append(out, b)
	}
	return out, max(total, len(out)), nil
}

// projectLandPlans projects a land's plans in order, each within what is left
// of windowLandClosureMax (and windowClosureMax alone). It reads every plan's
// closure at once: one subtree query over all the plans, then one
// readWindowClosure, so the read count does not grow with the plans (the
// gardenio land, 31 plans and hundreds of steps, timed out at four queries a
// plan, gd-rXMNcFrzDp). A land whose closures together pass the bound is
// walked plan by plan instead, so the bound still decides which plans are
// withheld.
func (s *BeadsServer) projectLandPlans(ctx context.Context, plans []*model.Bead) ([]windowLandPlan, error) {
	out := make([]windowLandPlan, 0, len(plans))
	if len(plans) == 0 {
		return out, nil
	}
	roots := make([]string, 0, len(plans))
	for _, p := range plans {
		roots = append(roots, p.ID)
	}
	f := windowLandFilter()
	f.ParentSubtreeIDs, f.Limit = roots, windowLandClosureMax+1
	desc, _, err := s.store.ListBeads(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("listing the land's plan subtrees: %w", err)
	}
	if len(desc) > windowLandClosureMax {
		return s.projectLandPlansOneByOne(ctx, plans)
	}

	byID := make(map[string]*model.Bead, len(desc)+len(plans))
	for _, b := range append(slices.Clone(plans), desc...) {
		if b != nil && b.Type != typeWindow && byID[b.ID] == nil {
			byID[b.ID] = b
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	reads, err := s.readWindowClosure(ctx, ids)
	if err != nil {
		return nil, err
	}
	kids := map[string][]string{}
	for _, id := range ids {
		for _, d := range reads.parents[id] {
			if d != nil && model.IsParentChildDepType(string(d.Type)) && byID[d.DependsOnID] != nil {
				kids[d.DependsOnID] = append(kids[d.DependsOnID], id)
			}
		}
	}

	budget := windowLandClosureMax
	for _, plan := range plans {
		limit := max(min(budget, windowClosureMax), 0)
		closure := windowSubtree(plan.ID, kids, byID)
		if len(closure) > limit {
			budget -= limit
			out = append(out, withheldLandPlan(plan))
			continue
		}
		budget -= len(closure)
		out = append(out, landPlanOf(plan, projectWindowClosure(plan, closure, reads)))
	}
	return out, nil
}

// projectLandPlansOneByOne is projectLandPlans for a land past the bound: one
// walk per plan, each stopping at what is left of it.
func (s *BeadsServer) projectLandPlansOneByOne(ctx context.Context, plans []*model.Bead) ([]windowLandPlan, error) {
	out := make([]windowLandPlan, 0, len(plans))
	budget := windowLandClosureMax
	for _, plan := range plans {
		lp, walked, err := s.projectLandPlan(ctx, plan, min(budget, windowClosureMax))
		if err != nil {
			return nil, err
		}
		budget -= walked
		out = append(out, lp)
	}
	return out, nil
}

// windowSubtree is root's descendants through kids (parent id → child ids),
// without root itself, as the subtree query would list them.
func windowSubtree(root string, kids map[string][]string, byID map[string]*model.Bead) []*model.Bead {
	seen := map[string]bool{root: true}
	var out []*model.Bead
	queue := []string{root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, c := range kids[id] {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, byID[c])
			queue = append(queue, c)
		}
	}
	return out
}

// projectLandPlan projects one plan of a land view within a walk of at most
// limit beads, and returns how many it read. Past the limit it fills only the
// headline and sets Withheld, and the read still counts against the land's
// bound, so a land of oversized plans reads at most windowLandClosureMax.
func (s *BeadsServer) projectLandPlan(ctx context.Context, plan *model.Bead, limit int) (windowLandPlan, int, error) {
	v, walked, err := s.projectWindowPlan(ctx, plan, windowLandFilter(), max(limit, 0))
	switch {
	case errors.Is(err, errWindowClosureTooLarge):
		return withheldLandPlan(plan), max(limit, 0), nil
	case err != nil:
		return windowLandPlan{}, 0, err
	}
	return landPlanOf(plan, v), walked, nil
}

// withheldLandPlan is a land plan past the walk's bound: the headline only.
func withheldLandPlan(plan *model.Bead) windowLandPlan {
	lp := landPlanHeadline(plan)
	lp.Withheld = true
	return lp
}

func landPlanHeadline(plan *model.Bead) windowLandPlan {
	return windowLandPlan{
		windowPlan: windowPlan{
			Title: plan.Title, ShortTitle: plan.ShortTitle, Heraldry: plan.Heraldry,
			Status: string(plan.Status), State: beadFieldString(plan, "plan_state"),
		},
		Phases: []windowPhase{}, Steps: []windowStep{}, Decisions: []string{}, Crew: []windowAgent{},
		UpdatedAt: plan.UpdatedAt,
	}
}

// landPlanOf cuts a plan's window view down to the land view's allowlist.
func landPlanOf(plan *model.Bead, v *windowView) windowLandPlan {
	lp := landPlanHeadline(plan)
	lp.windowPlan = v.Plan
	lp.Phases, lp.Steps, lp.Crew = v.Phases, v.Steps, v.Crew
	for _, d := range v.Decisions {
		if d.State == "open" {
			lp.OpenDecisions++
			lp.Decisions = append(lp.Decisions, d.Title)
		}
	}
	lp.OpenEscalations = v.Escalations.Open
	if v.UpdatedAt.After(lp.UpdatedAt) {
		lp.UpdatedAt = v.UpdatedAt
	}
	return lp
}

// windowSteps projects one group's work-item children in step_order (listed
// steps first, then creation order), skipping any a phase claimed, and adds a
// Recent row for each closed one.
func windowSteps(kids []*model.Bead, order []string, claimed map[string]bool, recent *[]windowChange) []windowStep {
	rank := map[string]int{}
	for i, id := range order {
		if _, dup := rank[id]; !dup {
			rank[id] = i
		}
	}
	var steps []*model.Bead
	for _, c := range kids {
		if claimed[c.ID] || !windowStepType(c.Type) {
			continue
		}
		claimed[c.ID] = true
		steps = append(steps, c)
	}
	slices.SortStableFunc(steps, func(a, b *model.Bead) int {
		ra, oka := rank[a.ID]
		rb, okb := rank[b.ID]
		switch {
		case oka && okb:
			return cmp.Compare(ra, rb)
		case oka:
			return -1
		case okb:
			return 1
		}
		return 0
	})
	out := make([]windowStep, 0, len(steps))
	for i, b := range steps {
		st := windowStep{Number: i + 1, Title: b.Title, ShortTitle: b.ShortTitle, Status: string(b.Status), Kind: string(b.Type)}
		if b.Status == model.StatusClosed && b.ClosedAt != nil {
			at := *b.ClosedAt
			st.DoneAt = &at
			*recent = append(*recent, windowChange{At: at, Kind: windowChangeStepClosed, Subject: b.Title})
		}
		out = append(out, st)
	}
	return out
}

// windowStepType reports whether a child renders as a step: a work item.
func windowStepType(t model.BeadType) bool {
	switch t {
	case model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeChore:
		return true
	}
	return false
}

func phaseSteps(phases []windowPhase) [][]windowStep {
	out := make([][]windowStep, 0, len(phases))
	for _, ph := range phases {
		out = append(out, ph.Steps)
	}
	return out
}

// windowDecisionOf projects a decision: open, resolved with the chosen
// option's label (or its id when the options carry no label), or cancelled.
func windowDecisionOf(b *model.Bead) windowDecision {
	d := windowDecision{Title: b.Title, State: "open"}
	if b.Status != model.StatusClosed {
		return d
	}
	chosen := beadFieldString(b, "chosen")
	if chosen == "" {
		d.State = "cancelled"
		return d
	}
	d.State = "resolved"
	d.Chosen = chosen
	var f struct {
		Options []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"options"`
	}
	if json.Unmarshal(b.Fields, &f) == nil {
		for _, o := range f.Options {
			if o.ID == chosen && strings.TrimSpace(o.Label) != "" {
				d.Chosen = o.Label
				break
			}
		}
	}
	return d
}

// windowNarrativeBundle is the plan's narrative: the first direct bundle child
// of kind plan, else the first direct bundle child.
func windowNarrativeBundle(kids []*model.Bead) *model.Bead {
	var first *model.Bead
	for _, c := range kids {
		if c.Type != model.TypeBundle {
			continue
		}
		if beadFieldString(c, "kind") == "plan" {
			return c
		}
		if first == nil {
			first = c
		}
	}
	return first
}

// windowDocs projects a bundle's markdown docs in position order. Other
// formats (links, html, images, diffs) and content_url never leave.
func windowDocs(kids []*model.Bead) []windowDoc {
	type ranked struct {
		pos int
		doc windowDoc
	}
	var docs []ranked
	for _, c := range kids {
		if c.Type != typeDoc || beadFieldString(c, "format") != "md" {
			continue
		}
		var f struct {
			Position int    `json:"position"`
			Content  string `json:"content"`
		}
		_ = json.Unmarshal(c.Fields, &f)
		docs = append(docs, ranked{pos: f.Position, doc: windowDoc{Title: c.Title, Content: f.Content}})
	}
	slices.SortStableFunc(docs, func(a, b ranked) int { return cmp.Compare(a.pos, b.pos) })
	out := make([]windowDoc, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.doc)
	}
	return out
}

// windowCrew returns the open type=agent beads with an assigned edge to a
// closure bead, by name.
func windowCrew(ids []string, reads *windowClosureReads) []windowAgent {
	seen := map[string]bool{}
	crew := []windowAgent{}
	for _, id := range ids {
		for _, d := range reads.assigned[id] {
			if d == nil || d.Type != windowAssignedDep || seen[d.BeadID] {
				continue
			}
			seen[d.BeadID] = true
			a := reads.agents[d.BeadID]
			if a == nil || a.Type != "agent" || a.Status == model.StatusClosed {
				continue
			}
			name := beadFieldString(a, "agent")
			if name == "" {
				name = a.Title
			}
			crew = append(crew, windowAgent{Name: name, Live: windowLiveAgentStates[beadFieldString(a, "agent_state")]})
		}
	}
	slices.SortFunc(crew, func(a, b windowAgent) int { return strings.Compare(a.Name, b.Name) })
	return crew
}

// windowAssignedDep is the edge an agent bead holds to the bead it works on.
const windowAssignedDep model.DependencyType = "assigned"

// beadFieldString reads one string field off a bead, "" when absent.
func beadFieldString(b *model.Bead, key string) string {
	v, _, _ := exactFieldString(b.Fields, key)
	return strings.TrimSpace(v)
}

// beadFieldIDList decodes a JSON-array id list field (phase_order,
// step_order), stored either as an array or as its JSON text.
func beadFieldIDList(b *model.Bead, key string) []string {
	var f map[string]json.RawMessage
	if json.Unmarshal(b.Fields, &f) != nil {
		return nil
	}
	raw := f[key]
	var ids []string
	if json.Unmarshal(raw, &ids) != nil {
		var text string
		if json.Unmarshal(raw, &text) != nil || json.Unmarshal([]byte(text), &ids) != nil {
			return nil
		}
	}
	return slices.DeleteFunc(ids, func(id string) bool { return id == "" })
}
