package main

import (
	"context"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// gardenPlans is the plan cockpit's reader (gardenhub.PlanReader, Lands workbench-glass spec WB-7 and WB-8, step 5.9):
// the same builders handlePlanDetail composes for /plans/{id} (the step timeline with its phases, progress, the
// clarifying-question decisions, the escalations that hold up a step, the crew roster and the narrative), on the
// request's bearer, so the daemon cuts the answer as it cuts the page. Pour lists the formulas the pour disclosure
// offers (plan_pour_form.go).
type gardenPlans struct{ s *Server }

// Plan implements gardenhub.PlanReader.
func (g gardenPlans) Plan(ctx context.Context, id string) (*gardenhub.PlanDoc, error) {
	s := g.s
	bead, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		return nil, err
	}
	// Only an epic has a cockpit (handlePlanDetail 404s the rest; a bundle published as a plan reads as a bundle).
	if bead == nil || bead.Type != "epic" {
		return nil, gardenhub.ErrNotPlan
	}
	steps, phases := s.planTimeline(ctx, id)
	total, closed, pct := planProgress(steps, phases)
	doc := &gardenhub.PlanDoc{
		ID: bead.ID, Title: bead.Title, ShortTitle: bead.ShortTitle, Project: projectFromLabels(bead.Labels),
		Status: bead.Status, State: strings.TrimSpace(bead.Fields["plan_state"]), Plan: hasLabel(bead, planLabel),
		Total: total, Closed: closed, Pct: pct,
	}
	for _, st := range steps {
		doc.Steps = append(doc.Steps, gardenPlanStep(st))
	}
	for _, ph := range phases {
		p := gardenhub.PlanPhase{
			ID: ph.ID, Number: ph.Number, Title: ph.Title, Status: ph.Status, Closed: ph.Closed,
			Total: ph.StepsTotal, Done: ph.StepsClosed, Formula: firstNonBlank(ph.FormulaTitle, ph.FormulaID),
			Steps: make([]gardenhub.PlanStep, 0, len(ph.Steps)),
		}
		for _, st := range ph.Steps {
			p.Steps = append(p.Steps, gardenPlanStep(st))
		}
		doc.Phases = append(doc.Phases, p)
	}

	// Decisions: the cockpit's own (planDecisions' beads and its canonical decision view), with when each was asked and of
	// whom: the viewer (by operator id, as the Queues panel says "asked of you", or by address), else the operator by name
	// (the operator chip's resolution, never a raw id).
	viewerEmail := oidcauth.EmailFromContext(ctx)
	viewerOp, _, err := s.resolveInboxViewerOperator(ctx, viewerEmail)
	if err != nil {
		viewerOp = 0 // no "mine" by id; the address still matches
	}
	for _, b := range s.planDecisionBeads(ctx, id) {
		if b == nil {
			continue
		}
		d := gardenhub.PlanDecision{ID: b.ID, Title: b.Title, Prompt: b.Fields["prompt"], Status: b.Status, Resolved: b.Status == "closed", Since: unixSec(b.CreatedAt)}
		if chip := resolveOperatorChip(b.OperatedBy, b.OperatedByDisplay, b.OperatedByEmail, "", b.CreatedBy, "", viewerEmail); chip.Kind == "human" {
			d.Mine = chip.IsViewerSelf || (viewerOp != 0 && b.OperatedByID != nil && *b.OperatedByID == viewerOp)
			if !d.Mine && chip.Display != chip.RawID {
				d.AskedOf = chip.Display
			}
		}
		if dv := buildDecisionView(b, s.decisionMarkdown); dv != nil {
			d.RequestedBy = dv.RequestedBy
			d.Resolved = d.Resolved || dv.Resolved
			for _, o := range dv.Options {
				d.Options = append(d.Options, gardenhub.QueueOption{ID: o.ID, Text: firstNonBlank(o.Short, o.Label, o.ID), Label: o.Label})
				if o.ID != "" && o.ID == dv.Resolution.OptionID {
					d.Chosen = firstNonBlank(o.Label, o.Short, o.ID)
				}
			}
			if d.Chosen == "" {
				d.Chosen = strings.TrimSpace(dv.Resolution.ResponseText)
			}
			if dv.Resolution.Canceled {
				d.Chosen = "dismissed"
			}
		}
		doc.Decisions = append(doc.Decisions, d)
	}

	for _, e := range s.planEscalationsBulk(ctx, map[string][]string{id: planEscalationWalkIDs(id, steps, phases)})[id].Escalations {
		doc.Escalations = append(doc.Escalations, gardenhub.PlanEscalation{ID: e.ID, Title: e.Title, Operation: e.OperationType, Sensitivity: e.Sensitivity, Audience: e.Audience, Since: unixSec(e.createdAt)})
	}

	// The crew, quieted on a finished plan as the page does (reconcileTerminal).
	crew := s.planCrew(ctx, id, steps, phases).reconcileTerminal(bead.Status)
	c := &gardenhub.PlanCrew{Total: crew.Total, Working: crew.Working, Starting: crew.Starting, Idle: crew.Idle, Done: crew.Done, Failed: crew.Failed, Members: []gardenhub.PlanCrewMember{}}
	for _, m := range crew.Members {
		pm := gardenhub.PlanCrewMember{ID: m.ID, Name: m.Name, Role: m.Role, State: m.AgentState, Bucket: m.Bucket, Live: m.Live, StepN: m.StepNumber, Outcome: m.Outcome, Since: unixSec(m.updatedAt)}
		if m.StepID != "" {
			pm.Step = &gardenhub.QueueRef{ID: m.StepID, Title: m.StepTitle}
		}
		c.Members = append(c.Members, pm)
	}
	doc.Crew = c
	doc.LastChange = unixSec(laterTime(planLastActivity(bead.UpdatedAt, steps, phases), crew.newestActivityAt))

	// The narrative: the epic's description and its bundle's docs (planNarrativeData, the fragment's own builder).
	doc.Narrative.Summary = bead.Description
	if nd, ok := s.planNarrativeData(ctx, id, false, ""); ok {
		if pv, _ := nd["Prose"].(*bundleView); pv != nil {
			doc.Narrative.Bundle, doc.Narrative.BundleTitle = pv.BundleID, pv.Title
			for _, d := range pv.Docs {
				doc.Narrative.Docs = append(doc.Narrative.Docs, gardenhub.PlanNarrativeDoc{ID: d.BeadID, Headline: d.Headline, Format: d.Format, Content: d.Content, URL: d.ContentURL})
			}
		}
	}
	return doc, nil
}

func gardenPlanStep(st planStepView) gardenhub.PlanStep {
	p := gardenhub.PlanStep{
		ID: st.ID, Number: st.Number, Title: st.Title, ShortTitle: st.ShortTitle, Type: st.Type, Status: st.Status,
		Closed: st.Closed, Priority: st.Priority, Assignee: st.Assignee, Updated: unixSec(st.UpdatedAt),
	}
	if st.MRState != "" || st.MRURL != "" {
		p.MR = &gardenhub.PlanMR{State: st.MRState, URL: st.MRURL}
	}
	for _, b := range st.BlockedBy {
		p.BlockedBy = append(p.BlockedBy, gardenhub.PlanBlocker{ID: b.ID, Title: b.Title, Escalation: b.IsEscalation, Operation: b.OperationType})
	}
	return p
}

// Pour implements gardenhub.PlanReader: the formulas of the plan's project (pourFormulaOptions' list), each with the
// variables its pour form asks for (pourVarFields).
func (g gardenPlans) Pour(ctx context.Context, id string) (*gardenhub.PourDoc, error) {
	s := g.s
	plan, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		return nil, err
	}
	if plan == nil || plan.Type != "epic" {
		return nil, gardenhub.ErrNotPlan
	}
	project := projectFromLabels(plan.Labels)
	doc := &gardenhub.PourDoc{Plan: id, Project: project, Formulas: []gardenhub.PourFormula{}}
	q := beadsapi.ListBeadsQuery{Types: []string{"formula"}, Limit: formulaListLimit, NoTotal: true}
	if project != "" {
		q.Labels = []string{"project:" + project}
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, q)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return doc, nil
	}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		f := gardenhub.PourFormula{ID: b.ID, Title: b.Title, Steps: countJSONArray(b.Fields["steps"]), Vapor: strings.TrimSpace(b.Fields["phase"]) == "vapor"}
		for _, v := range pourVarFields(b) {
			f.Vars = append(f.Vars, gardenhub.PourVar{Name: v.Name, Note: v.Description, Required: v.Required, Default: v.Default, Enum: v.Enum})
		}
		doc.Formulas = append(doc.Formulas, f)
	}
	return doc, nil
}
