package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// pourVarField is one variable input on the pour form, projected from the
// formula's `vars` definition.
type pourVarField struct {
	Name        string
	Description string
	Required    bool
	Default     string
	Enum        []string
	Value       string // sticky value on re-render after an error
}

// pourFormView is the template payload for formula_pour.html.
type pourFormView struct {
	FormulaID   string
	Title       string
	Description string
	Phase       string // recommended phase: vapor / liquid / ""
	StepCount   int
	Vars        []pourVarField
	Ephemeral   bool
	Force       bool
	Error       string
	Warning     string
	Plan        []beadsapi.PourPlanStep // dry-run preview, when requested
	PlanTotal   int
	// TargetPlan is the optional plan-epic id to EMPLACE the poured molecule into
	// as a phase (ka-0000003125). Blank = today's freestanding molecule. Sticky on
	// re-render after an error, like the var values.
	TargetPlan string
}

// loadPourFormula fetches the formula bead and projects the pour form view.
// Returns nil (with the response already written) on error.
func (s *Server) loadPourFormula(w http.ResponseWriter, r *http.Request) *pourFormView {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing formula id", http.StatusBadRequest)
		return nil
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Formula not found", http.StatusNotFound)
			return nil
		}
		s.logger.Error("loading formula for pour", "id", id, "error", err)
		http.Error(w, "Failed to load formula", http.StatusInternalServerError)
		return nil
	}
	if bead.Type != "formula" {
		http.Error(w, "Not a formula", http.StatusBadRequest)
		return nil
	}

	view := &pourFormView{
		FormulaID:   bead.ID,
		Title:       bead.Title,
		Description: bead.Description,
		Phase:       strings.TrimSpace(bead.Fields["phase"]),
		StepCount:   countJSONArray(bead.Fields["steps"]),
	}
	view.Vars = pourVarFields(bead)
	// Wisp-phase formulas default the ephemeral toggle on.
	view.Ephemeral = view.Phase == "vapor"
	return view
}

// pourVarFields projects a formula bead's `vars` definition into the pour form's
// input fields, seeding each Value with its default. Shared by the freestanding
// pour form (loadPourFormula) and the plan-cockpit pour disclosure
// (planPourFormulaChoice), so both render the same var inputs from one source.
// Tolerates an absent or malformed `vars` field (returns nil).
func pourVarFields(bead *beadsapi.BeadDetail) []pourVarField {
	var defs []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Required    bool     `json:"required"`
		Default     string   `json:"default"`
		Enum        []string `json:"enum"`
	}
	if raw := bead.Fields["vars"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &defs)
	}
	var fields []pourVarField
	for _, d := range defs {
		fields = append(fields, pourVarField{
			Name:        d.Name,
			Description: d.Description,
			Required:    d.Required,
			Default:     d.Default,
			Enum:        d.Enum,
			Value:       d.Default,
		})
	}
	return fields
}

// handleFormulaPourForm renders GET /formulas/{id}/pour — the operator's
// confirm-before-pour page: one input per formula var, ephemeral/force
// toggles, and a Preview (dry-run) action. Read-scoped like the catalog; the
// POST is what mutates and is operator-gated.
func (s *Server) handleFormulaPourForm(w http.ResponseWriter, r *http.Request) {
	view := s.loadPourFormula(w, r)
	if view == nil {
		return
	}
	if !viewerCanOperateAuthed(r) {
		view.Error = "Pouring requires the operator role."
	}
	s.renderPourForm(w, r, view, http.StatusOK)
}

// handleFormulaPour handles POST /formulas/{id}/pour. Collects var_<name>
// form values into the daemon pour request. preview=1 runs a dry-run and
// re-renders the form with the step plan; otherwise on success it redirects
// to the created molecule's bead page.
func (s *Server) handleFormulaPour(w http.ResponseWriter, r *http.Request) {
	view := s.loadPourFormula(w, r)
	if view == nil {
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		view.Error = "Invalid form submission."
		s.renderPourForm(w, r, view, http.StatusBadRequest)
		return
	}

	req := beadsapi.PourFormulaRequest{
		Vars:       map[string]string{},
		Ephemeral:  r.FormValue("ephemeral") == "1",
		Force:      r.FormValue("force") == "1",
		DryRun:     r.FormValue("preview") == "1",
		TargetPlan: strings.TrimSpace(r.FormValue("target_plan")),
	}
	view.TargetPlan = req.TargetPlan // sticky on re-render
	for i := range view.Vars {
		v := strings.TrimSpace(r.FormValue("var_" + view.Vars[i].Name))
		view.Vars[i].Value = v // sticky on re-render
		if v != "" {
			req.Vars[view.Vars[i].Name] = v
		}
	}
	view.Ephemeral = req.Ephemeral
	view.Force = req.Force

	resp, err := s.daemon.PourFormula(r.Context(), view.FormulaID, req)
	if err != nil {
		s.logger.Error("pouring formula", "id", view.FormulaID, "actor", email, "error", err)
		view.Error = pourErrorMessage(err)
		s.renderPourForm(w, r, view, http.StatusOK)
		return
	}

	if req.DryRun {
		view.Plan = resp.Plan
		view.PlanTotal = resp.TotalSteps
		view.Warning = resp.Warning
		s.renderPourForm(w, r, view, http.StatusOK)
		return
	}

	molID := resp.MoleculeID()
	if molID == "" {
		view.Error = "Daemon returned no molecule id."
		s.renderPourForm(w, r, view, http.StatusOK)
		return
	}
	s.logger.Info("formula poured", "formula", view.FormulaID, "molecule", molID, "actor", email, "steps", resp.TotalSteps, "target_plan", req.TargetPlan)
	// On an emplaced pour, land on the plan cockpit so the operator sees the new
	// phase in context; otherwise the freestanding molecule's own bead page.
	if req.TargetPlan != "" {
		http.Redirect(w, r, s.basePath+"/plans/"+req.TargetPlan, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.basePath+"/"+molID, http.StatusSeeOther)
}

// pourErrorMessage maps a daemon pour error onto an operator-readable line,
// surfacing the daemon's own message (missing var, duplicate molecule, …)
// instead of a generic failure.
func pourErrorMessage(err error) string {
	var apiErr *beadsapi.APIError
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		return apiErr.Message
	}
	return "Pour failed — see server logs."
}

// renderPourForm renders the pour form page with the operator affordance
// flag set from the request role.
func (s *Server) renderPourForm(w http.ResponseWriter, r *http.Request, view *pourFormView, status int) {
	email := oidcauth.EmailFromContext(r.Context())
	data := map[string]any{
		"View":        view,
		"IsOperator":  viewerCanOperateAuthed(r),
		"ViewerEmail": email,
		"InboxCount":  s.inboxCountForViewer(r.Context(), email),
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, r, "formula_pour.html", data)
}
