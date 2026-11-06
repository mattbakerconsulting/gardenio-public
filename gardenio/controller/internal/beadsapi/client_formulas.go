package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// PourFormulaRequest is the JSON body for POST /v1/formulas/{id}/pour — the
// daemon-side formula instantiation endpoint (ka-0000000251). Mirrors the kd
// CLI's `formula pour` knobs.
type PourFormulaRequest struct {
	Vars           map[string]string `json:"vars,omitempty"`
	Labels         []string          `json:"labels,omitempty"`
	Assignee       string            `json:"assignee,omitempty"`
	Ephemeral      bool              `json:"ephemeral,omitempty"`
	Force          bool              `json:"force,omitempty"`
	DryRun         bool              `json:"dry_run,omitempty"`
	TriggerBeadIDs []string          `json:"trigger_bead_ids,omitempty"`
	SourceBeadIDs  []string          `json:"source_bead_ids,omitempty"`
	// TargetPlan emplaces the poured molecule as a phase of the named plan epic
	// (parents it + appends to the plan's phase_order). Blank = freestanding
	// molecule. Ignored on a dry run. See http_formula_pour.go (ka-0000004940).
	TargetPlan string `json:"target_plan,omitempty"`
	// TargetPhaseTitle overrides the poured molecule's title (ka-0000003236). The
	// molecule's title renders as the plan PHASE title when emplaced, so this is
	// the pour form's "Phase title" field. Blank keeps the var-substituted formula
	// title as the default. Ignored on a dry run. See http_formula_pour.go.
	TargetPhaseTitle string `json:"target_phase_title,omitempty"`
	// TargetPosition emplaces the poured phase at this 0-based slot in the target
	// plan's phase_order instead of appending it at the end — the "target cell"
	// generalization (ka-0000001001). Requires TargetPlan; must be in
	// [0, len(phase_order)]. nil = tail append (the default). Ignored on a dry run.
	// See http_formula_pour.go.
	TargetPosition *int `json:"target_position,omitempty"`
}

// PourPlanStep is one step in the pour plan / created-step echo.
type PourPlanStep struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Type      string         `json:"type"`
	DependsOn []string       `json:"depends_on,omitempty"`
	BeadID    string         `json:"bead_id,omitempty"`
	Children  []PourPlanStep `json:"children,omitempty"`
}

// PourMolecule is the created molecule echo in a pour response.
type PourMolecule struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// PourFormulaResponse is the daemon's response to a pour.
type PourFormulaResponse struct {
	Molecule   *PourMolecule     `json:"molecule,omitempty"`
	Steps      map[string]string `json:"steps,omitempty"`
	Plan       []PourPlanStep    `json:"plan"`
	TotalSteps int               `json:"total_steps"`
	DryRun     bool              `json:"dry_run,omitempty"`
	Warning    string            `json:"warning,omitempty"`
}

// MoleculeID returns the created molecule's bead id ("" on dry-run).
func (r *PourFormulaResponse) MoleculeID() string {
	if r == nil || r.Molecule == nil {
		return ""
	}
	return r.Molecule.ID
}

// PourFormula instantiates a formula server-side (POST /v1/formulas/{id}/pour),
// creating a molecule + step beads in one daemon round-trip. Operator-role
// gated daemon-side; with write-bearer enforcement enabled the per-request
// user bearer attributes the created beads to the human operator.
func (c *Client) PourFormula(ctx context.Context, formulaID string, req PourFormulaRequest) (*PourFormulaResponse, error) {
	var resp PourFormulaResponse
	path := "/v1/formulas/" + url.PathEscape(formulaID) + "/pour"
	if err := c.doJSON(ctx, http.MethodPost, path, req, &resp); err != nil {
		return nil, fmt.Errorf("pouring formula %s: %w", formulaID, err)
	}
	return &resp, nil
}
