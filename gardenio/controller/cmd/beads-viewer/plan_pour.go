package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// POST /plans/{id}/pours — the plan-cockpit-native write-proxy for pouring a
// formula INTO a plan as a phase (epic ka-0000000759, task ka-0000003984). It is
// the plan-scoped counterpart of the standalone /formulas/{id}/pour form
// (formula_pour.go): the plan id comes from the URL (the plan to emplace into)
// and the formula id + vars come from the request body, so the plan cockpit's
// "Pour a formula" disclosure (sibling ka-0000007748 / ka-0000003236) and its
// Preview action (sibling ka-0000001848) submit here rather than round-tripping
// through the freestanding form.
//
// Browser write-proxy (carve-out ka-0000004319; canonical banner in
// plans_lifecycle.go). It owns NO plan invariant: it stamps the operator, maps
// the request onto a daemon PourFormula call with target_plan = this plan, and
// returns whatever the daemon did. Every emplacement invariant — target is a
// real, open plan epic; project authorization; the molecule create + phase_order
// append transaction; the duplicate-molecule and unresolved-placeholder guards —
// lives in the daemon's handlePourFormula (http_formula_pour.go, ka-0000004940),
// exactly as it does for `gb plan pour` and the freestanding Pour button. Same
// daemon composition as gb: no new service, no shared state beyond the kbeads
// daemon connection.

// planPourInput is the normalized pour request, decoded from either a browser
// form (var_<name> fields, preview=1) or a JSON body (vars map, dry_run). It
// carries only pass-through pour knobs — no plan-domain state.
type planPourInput struct {
	FormulaID  string
	Vars       map[string]string
	PhaseTitle string // optional phase-title override (ka-0000003236); blank = formula title
	Position   *int   // optional 0-based phase_order slot (ka-0000001001); nil = tail append
	Ephemeral  bool
	Force      bool
	DryRun     bool
}

// planPourResult is the JSON response body. Molecule is the emplaced molecule
// bead id — the field the acceptance criterion calls out "for the MR comment"
// (task ka-0000003984). On a dry run it is empty and StepPlan carries the
// would-create step tree instead.
type planPourResult struct {
	Plan       string                  `json:"plan"`
	Formula    string                  `json:"formula"`
	Molecule   string                  `json:"molecule,omitempty"`
	Steps      map[string]string       `json:"steps,omitempty"`
	StepPlan   []beadsapi.PourPlanStep `json:"step_plan,omitempty"`
	TotalSteps int                     `json:"total_steps"`
	DryRun     bool                    `json:"dry_run,omitempty"`
	Warning    string                  `json:"warning,omitempty"`
}

// handlePlanPour handles POST /plans/{id}/pours. Operator-gated at the route
// layer (operatorOnly wrapper in RegisterRoutes); it re-checks the operator
// email defense-in-depth, stamps it on the daemon ctx (without which the kbeads
// audit resolver fail-closes with HTTP 400, R2 ka-0000005772), and delegates to
// the daemon pour with target_plan = the URL plan id.
//
// Response, by content negotiation:
//   - dry run (preview): always JSON — a data preview of the step tree the pour
//     would create ({step_plan, total_steps, dry_run:true, warning}). The daemon
//     ignores target_plan on a dry run, so nothing is written.
//   - real pour, JSON caller (Accept: application/json / ?format=json / JSON
//     body): 201 with {molecule, steps, total_steps, warning} — the molecule id
//     for the MR comment.
//   - real pour, browser form: 303 See Other → /plans/{id}, the classic
//     POST-redirect-GET so the operator lands on the cockpit with the new phase
//     in view (mirrors the freestanding form's emplace redirect).
func (s *Server) handlePlanPour(w http.ResponseWriter, r *http.Request) {
	planID := strings.TrimSpace(r.PathValue("id"))
	if planID == "" {
		http.Error(w, "Missing plan id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}

	input, jsonReq, err := decodePlanPourInput(r)
	if err != nil {
		http.Error(w, "Invalid pour request.", http.StatusBadRequest)
		return
	}
	// The cockpit disclosure submits over HTMX (the "⚗ Pour a formula" form,
	// ka-0000001848): Preview re-renders the plan_pour_body fragment inline with
	// the dry-run step plan, and a real pour navigates the browser to the
	// refreshed cockpit via HX-Redirect. A JSON caller (gb-style / Accept) still
	// gets JSON. htmxForm is the HTMX-and-not-JSON case.
	htmxForm := !jsonReq && r.Header.Get("HX-Request") == "true"
	if input.FormulaID == "" {
		if htmxForm {
			s.renderPlanPourBody(w, r, planPourBodyView{
				PlanID:  planID,
				Formula: s.loadPlanPourChoice(r.Context(), ""),
				Error:   "Pick a formula to pour.",
			})
			return
		}
		http.Error(w, "A formula id is required to pour.", http.StatusBadRequest)
		return
	}
	respondJSON := jsonReq || wantsJSON(r)

	// Stamp the operator so the daemon's molecule/step creates + phase_order
	// append carry X-Operated-By (mirrors every 3e write verb, ka-0000005772).
	ctx := beadsapi.WithOperator(r.Context(), canonicalOIDC(email))

	// target_plan = this plan: the daemon parents the poured molecule under the
	// plan epic and appends it to phase_order in one server-side request, so it
	// renders as a first-class phase. All emplacement invariants are the daemon's
	// (this handler owns none). target_plan is ignored on a dry run.
	resp, perr := s.daemon.PourFormula(ctx, input.FormulaID, beadsapi.PourFormulaRequest{
		Vars:             input.Vars,
		Ephemeral:        input.Ephemeral,
		Force:            input.Force,
		DryRun:           input.DryRun,
		TargetPlan:       planID,
		TargetPhaseTitle: input.PhaseTitle,
		TargetPosition:   input.Position,
	})
	if perr != nil {
		s.logger.Error("plan pour", "plan", planID, "formula", input.FormulaID, "actor", email, "error", perr)
		if htmxForm {
			// Re-render the disclosure body with the daemon's own message (missing
			// var, duplicate molecule, closed plan …) so the operator can fix and
			// retry in place — with their var values sticky.
			s.renderPlanPourBody(w, r, s.planPourBodyFromForm(r, planID, input, pourErrorMessage(perr)))
			return
		}
		s.writePlanPourError(w, respondJSON, perr)
		return
	}

	// A dry run is a data preview — nothing was written. For the cockpit HTMX
	// Preview, re-render the disclosure body with the step plan inline (the
	// Preview centerpiece, ka-0000001848). For a programmatic caller, answer with
	// the step plan as JSON — the stable contract the write-proxy exposes.
	if input.DryRun {
		if htmxForm {
			view := s.planPourBodyFromForm(r, planID, input, "")
			view.Plan = resp.Plan
			view.PlanTotal = resp.TotalSteps
			view.Warning = resp.Warning
			s.renderPlanPourBody(w, r, view)
			return
		}
		writeJSONStatus(w, http.StatusOK, planPourResult{
			Plan:       planID,
			Formula:    input.FormulaID,
			StepPlan:   resp.Plan,
			TotalSteps: resp.TotalSteps,
			DryRun:     true,
			Warning:    resp.Warning,
		})
		return
	}

	molID := resp.MoleculeID()
	if molID == "" {
		// The daemon returned success but no molecule — surface it rather than
		// redirecting to a cockpit with nothing new (never silently succeed).
		s.logger.Error("plan pour: daemon returned no molecule id", "plan", planID, "formula", input.FormulaID, "actor", email)
		if htmxForm {
			s.renderPlanPourBody(w, r, s.planPourBodyFromForm(r, planID, input, "Daemon returned no molecule id."))
			return
		}
		s.writePlanPourError(w, respondJSON, errors.New("daemon returned no molecule id"))
		return
	}
	s.logger.Info("plan pour: emplaced phase", "plan", planID, "formula", input.FormulaID, "molecule", molID, "steps", resp.TotalSteps, "actor", email)

	if respondJSON {
		writeJSONStatus(w, http.StatusCreated, planPourResult{
			Plan:       planID,
			Formula:    input.FormulaID,
			Molecule:   molID,
			Steps:      resp.Steps,
			TotalSteps: resp.TotalSteps,
			Warning:    resp.Warning,
		})
		return
	}
	dest := s.basePath + "/plans/" + url.PathEscape(planID)
	if htmxForm {
		// HTMX form submit: navigate the whole browser to the refreshed cockpit so
		// the operator sees the new phase in context. HX-Redirect drives a full
		// navigation rather than swapping the cockpit HTML into #plan-pour-body.
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	// Plain browser form submit: POST-redirect-GET to the cockpit (like the
	// freestanding emplace pour).
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// planPourBodyFromForm rebuilds the plan_pour_body view for an HTMX re-render
// after a submit — re-loading the formula's var definitions and re-applying the
// submitted var values (sticky), plus the ephemeral/force toggles, so a Preview
// or an error re-render shows the operator's inputs unchanged. r.ParseForm has
// run inside decodePlanPourInput by this point.
func (s *Server) planPourBodyFromForm(r *http.Request, planID string, input planPourInput, errMsg string) planPourBodyView {
	choice := s.loadPlanPourChoice(r.Context(), input.FormulaID)
	// Re-apply the submitted values onto the freshly-loaded var fields.
	for i := range choice.Vars {
		choice.Vars[i].Value = input.Vars[choice.Vars[i].Name]
	}
	return planPourBodyView{
		PlanID:    planID,
		Formula:   choice,
		Ephemeral: input.Ephemeral,
		Force:     input.Force,
		Error:     errMsg,
	}
}

// decodePlanPourInput normalizes the request into a planPourInput, accepting
// both a JSON body (application/json) and a browser form. It returns
// jsonRequest=true when the body was JSON so the caller defaults the response to
// JSON. Var collection is schema-free — every var_<name> form field (or the JSON
// vars map) is passed straight through; the daemon validates required vars and
// unresolved placeholders, so this proxy needs no knowledge of the formula.
func decodePlanPourInput(r *http.Request) (planPourInput, bool, error) {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "application/json" {
		var body struct {
			FormulaID  string            `json:"formula_id"`
			Formula    string            `json:"formula"`
			Vars       map[string]string `json:"vars"`
			PhaseTitle string            `json:"phase_title"`
			Position   *int              `json:"position"`
			Ephemeral  bool              `json:"ephemeral"`
			Force      bool              `json:"force"`
			DryRun     bool              `json:"dry_run"`
		}
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
				return planPourInput{}, true, err
			}
		}
		fid := strings.TrimSpace(body.FormulaID)
		if fid == "" {
			fid = strings.TrimSpace(body.Formula)
		}
		return planPourInput{
			FormulaID:  fid,
			Vars:       body.Vars,
			PhaseTitle: strings.TrimSpace(body.PhaseTitle),
			Position:   body.Position,
			Ephemeral:  body.Ephemeral,
			Force:      body.Force,
			DryRun:     body.DryRun,
		}, true, nil
	}

	if err := r.ParseForm(); err != nil {
		return planPourInput{}, false, err
	}
	vars := map[string]string{}
	for k, v := range r.PostForm {
		if !strings.HasPrefix(k, "var_") || len(v) == 0 {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(k, "var_"))
		val := strings.TrimSpace(v[0])
		if name != "" && val != "" {
			vars[name] = val
		}
	}
	// position is optional: an empty field means tail append (the default); a
	// present, well-formed value emplaces at that slot (ka-0000001001). A
	// malformed value is a caller mistake surfaced as a decode error rather than
	// silently ignored, so a typo'd slot never pours to the wrong place.
	var position *int
	if raw := strings.TrimSpace(r.PostForm.Get("position")); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil {
			return planPourInput{}, false, fmt.Errorf("position %q is not an integer", raw)
		}
		position = &n
	}
	return planPourInput{
		FormulaID:  strings.TrimSpace(r.PostForm.Get("formula")),
		Vars:       vars,
		PhaseTitle: strings.TrimSpace(r.PostForm.Get("phase_title")),
		Position:   position,
		Ephemeral:  r.PostForm.Get("ephemeral") == "1",
		Force:      r.PostForm.Get("force") == "1",
		// The freestanding form names the dry-run action "preview"; accept
		// "dry_run" too for a programmatic form caller.
		DryRun: r.PostForm.Get("preview") == "1" || r.PostForm.Get("dry_run") == "1",
	}, false, nil
}

// writePlanPourError surfaces a daemon pour failure. JSON callers get a JSON
// {error} at the daemon's own status (missing var → 400, duplicate molecule →
// 409, closed/absent plan → 400/409); browser callers get the plain-text
// message. The daemon's own message is preserved (via pourErrorMessage) so the
// operator sees "duplicate molecule detected: gd-…" rather than a generic
// failure.
func (s *Server) writePlanPourError(w http.ResponseWriter, respondJSON bool, err error) {
	status := http.StatusBadGateway
	var apiErr *beadsapi.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode != 0 {
		status = apiErr.StatusCode
	}
	msg := pourErrorMessage(err)
	if respondJSON {
		writeJSONStatus(w, status, map[string]string{"error": msg})
		return
	}
	http.Error(w, msg, status)
}

// writeJSONStatus writes v as a JSON response at the given status.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
