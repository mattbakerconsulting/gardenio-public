package main

import (
	"context"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// The plan-cockpit "⚗ Pour a formula" disclosure (epic ka-0000000759, Phase 3).
// The write side is handlePlanPour (plan_pour.go); this file is the READ side
// that renders the disclosure's HTMX fragments:
//
//   - plan_pour_picker: the project-scoped formula <select>, lazy-loaded the
//     first time the operator opens the disclosure.
//   - plan_pour_body: the chosen formula's var inputs + Preview/Pour actions,
//     and — after a Preview (ka-0000001848) — the dry-run step plan.
//
// Both are served by GET /plans/{id}/pours/new: no ?formula → the picker; a
// ?formula=<id> → the body for that formula. Preview re-renders plan_pour_body
// (handlePlanPour's HTMX branch), so the disclosure stays open across a preview.

// pourFormulaOption is one <option> in the plan-cockpit formula picker — a
// type=formula recipe scoped to the plan's project.
type pourFormulaOption struct {
	ID        string
	Title     string
	StepCount int
}

// planPourChoice is the chosen formula projected into the pour body: its id +
// the var inputs (shared projection with the freestanding form via
// pourVarFields). Value on each var is sticky across a Preview re-render.
type planPourChoice struct {
	ID   string
	Vars []pourVarField
}

// planPourBodyView is the data bag for the plan_pour_body fragment. Plan is the
// dry-run step preview, populated only after a Preview; nothing is written to
// produce it.
type planPourBodyView struct {
	PlanID    string
	Formula   planPourChoice
	Ephemeral bool
	Force     bool
	Plan      []beadsapi.PourPlanStep
	PlanTotal int
	Warning   string
	Error     string
}

// handlePlanPourForm serves GET /plans/{id}/pours/new — the read-only fragments
// behind the pour disclosure. Operator-gated at the route layer. With no
// ?formula it renders the project-scoped picker (plan_pour_picker); with
// ?formula=<id> it renders that formula's var inputs + actions (plan_pour_body).
func (s *Server) handlePlanPourForm(w http.ResponseWriter, r *http.Request) {
	planID := strings.TrimSpace(r.PathValue("id"))
	if planID == "" {
		http.Error(w, "Missing plan id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	// The plan must be a real plan epic — a stray ?formula against a non-plan is
	// a 404, matching handlePlanDetail (only epics are plans). This also gives us
	// the plan's project label to scope the picker.
	plan, err := s.daemon.GetBead(ctx, planID)
	if err != nil || plan == nil || plan.Type != "epic" {
		http.Error(w, "Plan not found", http.StatusNotFound)
		return
	}
	project := projectFromLabels(plan.Labels)

	// Absent formula param = the initial panel load (the disclosure just opened)
	// → render the picker. Present-but-empty = the operator re-selected the blank
	// <option>, which hx-targets #plan-pour-body → render just the empty body
	// prompt (never the whole picker, which would nest a second picker).
	if !r.URL.Query().Has("formula") {
		s.renderPartial(w, r, "templates/partials/plan_pour.html", "plan_pour_picker", map[string]any{
			"PlanID":   planID,
			"Project":  project,
			"Formulas": s.pourFormulaOptions(ctx, project),
		})
		return
	}
	formulaID := strings.TrimSpace(r.URL.Query().Get("formula"))
	if formulaID == "" {
		s.renderPartial(w, r, "templates/partials/plan_pour.html", "plan_pour_body_empty", nil)
		return
	}

	// A formula was chosen → its var inputs + actions. Load the formula bead so
	// we render exactly the vars the daemon will expect (shared with the
	// freestanding form via pourVarFields).
	view, ok := s.planPourFormulaChoice(ctx, planID, project, formulaID)
	if !ok {
		s.renderPartial(w, r, "templates/partials/plan_pour.html", "plan_pour_body", map[string]any{
			"PlanID":  planID,
			"Formula": planPourChoice{ID: formulaID},
			"Error":   "Formula not found (or not in this plan's project).",
		})
		return
	}
	s.renderPlanPourBody(w, r, view)
}

// pourFormulaOptions lists the type=formula recipes for the picker, scoped to
// the plan's project label (the same ListBeadsFiltered pattern handleFormulas
// uses). Best-effort: a daemon error yields an empty list (the disclosure shows
// its "no formulas" state) rather than failing the whole cockpit.
func (s *Server) pourFormulaOptions(ctx context.Context, project string) []pourFormulaOption {
	q := beadsapi.ListBeadsQuery{
		Types: []string{"formula"},
		Limit: formulaListLimit,
	}
	if project != "" {
		q.Labels = []string{"project:" + project}
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, q)
	if err != nil || res == nil {
		if err != nil {
			s.logger.Error("plan pour picker: listing formulas", "project", project, "error", err)
		}
		return nil
	}
	opts := make([]pourFormulaOption, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		opts = append(opts, pourFormulaOption{
			ID:        b.ID,
			Title:     b.Title,
			StepCount: countJSONArray(b.Fields["steps"]),
		})
	}
	return opts
}

// planPourFormulaChoice loads the chosen formula and projects its var inputs.
// Returns (view, false) when the bead is absent, not a formula, or out of the
// plan's project scope — the daemon enforces project authz on the pour itself,
// but scoping the READ keeps a cross-project formula out of the picker's reach.
func (s *Server) planPourFormulaChoice(ctx context.Context, planID, project, formulaID string) (planPourBodyView, bool) {
	bead, err := s.daemon.GetBead(ctx, formulaID)
	if err != nil || bead == nil || bead.Type != "formula" {
		return planPourBodyView{}, false
	}
	if project != "" && projectFromLabels(bead.Labels) != project {
		return planPourBodyView{}, false
	}
	vars := pourVarFields(bead)
	return planPourBodyView{
		PlanID:  planID,
		Formula: planPourChoice{ID: formulaID, Vars: vars},
		// Wisp-phase formulas default the ephemeral toggle on (mirrors the
		// freestanding form's loadPourFormula).
		Ephemeral: strings.TrimSpace(bead.Fields["phase"]) == "vapor",
	}, true
}

// renderPlanPourBody renders the plan_pour_body fragment from a view. Shared by
// the GET form route and handlePlanPour's HTMX Preview re-render.
func (s *Server) renderPlanPourBody(w http.ResponseWriter, r *http.Request, view planPourBodyView) {
	s.renderPartial(w, r, "templates/partials/plan_pour.html", "plan_pour_body", map[string]any{
		"PlanID":    view.PlanID,
		"Formula":   view.Formula,
		"Ephemeral": view.Ephemeral,
		"Force":     view.Force,
		"Plan":      view.Plan,
		"PlanTotal": view.PlanTotal,
		"Warning":   view.Warning,
		"Error":     view.Error,
	})
}

// loadPlanPourChoice re-loads the chosen formula's var definitions for a POST
// re-render, so a Preview shows the same inputs the GET form did. Returns a bare
// choice (id only) when the formula can't be loaded — the daemon error surfaces
// separately, and the body still renders its (empty) form.
func (s *Server) loadPlanPourChoice(ctx context.Context, formulaID string) planPourChoice {
	bead, err := s.daemon.GetBead(ctx, formulaID)
	if err != nil || bead == nil || bead.Type != "formula" {
		return planPourChoice{ID: formulaID}
	}
	return planPourChoice{ID: formulaID, Vars: pourVarFields(bead)}
}
