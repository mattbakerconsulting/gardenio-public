package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/formula"
	"github.com/spf13/cobra"
)

// The formula definition types and the resolve/expand engine live in the
// shared internal/formula package so the daemon's pour endpoint
// (POST /v1/formulas/{id}/pour) and the kd CLI run the SAME pipeline
// (ka-0000000251). The CLI keeps its historical names as aliases so command
// code and the extensive test suite stay unchanged.
type (
	// FormulaVarDef defines a variable that a formula accepts.
	FormulaVarDef = formula.VarDef
	// varDefList is a tolerant []FormulaVarDef accepting the legacy
	// double-encoded string form (ka-0000001116).
	varDefList = formula.VarDefList
	// stepList is a tolerant []FormulaStep accepting the legacy
	// double-encoded string form (ka-0000001118).
	stepList = formula.StepList
	// GateOption defines a choice for a decision gate.
	GateOption = formula.GateOption
	// StepGate defines a gate or decision point in a formula step.
	StepGate = formula.Gate
	// StepLoop defines iteration for a formula step.
	StepLoop = formula.Loop
	// BranchCase defines a single case in a branch step.
	BranchCase = formula.BranchCase
	// StepBranch defines a conditional expansion.
	StepBranch = formula.Branch
	// StepOnComplete defines a deferred expansion on step completion.
	StepOnComplete = formula.OnComplete
	// FormulaStep defines a work item to create when a formula is applied.
	FormulaStep = formula.Step
)

var formulaCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a formula from a JSON definition",
	Long: `Create a reusable formula bead from a JSON file or inline JSON.

The formula content is a JSON object with "vars" and "steps" arrays. Step titles,
descriptions, and depends_on refs support {{variable}} substitution:

  {
    "vars": [
      {"name": "component", "description": "Component name", "required": true},
      {"name": "version", "pattern": "^\\d{4}\\.\\d+\\.\\d+$"}
    ],
    "steps": [
      {"id": "design", "title": "Design {{component}}", "type": "task"},
      {"id": "implement", "title": "Implement {{component}}", "type": "task", "needs": ["design"]}
    ],
    "phase": "liquid"
  }

The full DSL — every var/step/top-level/aspect field, and the extends, expand,
children, loop, branch, gate, on_complete, requires_skills, and waits_for
composition primitives with worked examples — is in docs/formula-reference.md
(advice ka-0000004524). Preview an expansion without creating beads via
'kd formula cook'.

Examples:
  kd formula create "Feature workflow" --file formula.json
  kd formula create "Bug fix" --file bugfix.json --label project:gardenio
  echo '{"steps":[...]}' | kd formula create "Quick formula" --file -`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		filePath, _ := cmd.Flags().GetString("file")
		description, _ := cmd.Flags().GetString("description")
		priority, _ := cmd.Flags().GetInt("priority")
		labels, _ := cmd.Flags().GetStringSlice("label")
		assignee, _ := cmd.Flags().GetString("assignee")

		if filePath == "" {
			return fmt.Errorf("--file is required: provide a JSON file path or - for stdin")
		}

		var data []byte
		var err error
		if filePath == "-" {
			data, err = os.ReadFile("/dev/stdin")
		} else {
			data, err = os.ReadFile(filePath)
		}
		if err != nil {
			return fmt.Errorf("reading formula file: %w", err)
		}

		// Parse and validate the formula content.
		var content struct {
			FormulaType    string     `json:"formula_type,omitempty"`
			Extends        []string   `json:"extends,omitempty"`
			Vars           varDefList `json:"vars"`  // tolerant: accepts array or legacy string form (ka-0000001116)
			Steps          stepList   `json:"steps"` // tolerant: accepts array or legacy string form (ka-0000005613)
			DefaultRoles   []string   `json:"default_roles,omitempty"`
			RequiresSkills []string   `json:"requires_skills,omitempty"`
			Phase          string     `json:"phase,omitempty"`
			// Workflow-only:
			Aspects []string `json:"aspects,omitempty"`
			// Aspect-only:
			Match  string        `json:"match,omitempty"`
			Before []FormulaStep `json:"before,omitempty"`
			After  []FormulaStep `json:"after,omitempty"`
		}
		if err := json.Unmarshal(data, &content); err != nil {
			return fmt.Errorf("parsing formula JSON: %w", err)
		}

		// Validate formula_type (default: workflow).
		switch content.FormulaType {
		case "", "workflow", "expansion", "aspect":
			// valid
		default:
			return fmt.Errorf("invalid formula_type %q: must be workflow, expansion, or aspect", content.FormulaType)
		}

		// Aspect formula validation.
		if content.FormulaType == "aspect" {
			if content.Match == "" {
				return fmt.Errorf("aspect formulas must have a match pattern")
			}
			if len(content.Before) == 0 && len(content.After) == 0 {
				return fmt.Errorf("aspect formulas must have at least one before or after step")
			}
			// Aspect formulas don't use steps — they use before/after.
			if len(content.Steps) > 0 {
				return fmt.Errorf("aspect formulas use before/after, not steps")
			}
		} else {
			// Non-aspect formulas must have steps (or extends).
			if len(content.Steps) == 0 && len(content.Extends) == 0 {
				return fmt.Errorf("formula must have at least one step (or use extends to inherit steps)")
			}
			// Non-aspect formulas should not have aspect-only fields.
			if content.Match != "" {
				return fmt.Errorf("match field is only valid for aspect formulas")
			}
			if len(content.Before) > 0 || len(content.After) > 0 {
				return fmt.Errorf("before/after fields are only valid for aspect formulas")
			}
		}

		// Validate extends references are valid bead IDs (basic format check).
		for _, ext := range content.Extends {
			if ext == "" {
				return fmt.Errorf("extends entry cannot be empty")
			}
		}

		// Merge `needs` into `depends_on` (needs is a human-friendly alias).
		for i := range content.Steps {
			if len(content.Steps[i].Needs) > 0 {
				seen := make(map[string]bool, len(content.Steps[i].DependsOn))
				for _, d := range content.Steps[i].DependsOn {
					seen[d] = true
				}
				for _, n := range content.Steps[i].Needs {
					if !seen[n] {
						content.Steps[i].DependsOn = append(content.Steps[i].DependsOn, n)
						seen[n] = true
					}
				}
			}
		}

		// Validate step IDs are unique (globally) and depends_on references exist.
		allStepIDs := make(map[string]bool)
		if err := collectStepIDs(content.Steps, allStepIDs); err != nil {
			return err
		}
		// When this formula `extends` parents, its new steps may legitimately
		// depend on inherited (parent) step IDs. Resolve the inheritance chain
		// and fold parent step IDs into the known-ID set before dep validation —
		// mirroring what `formula apply`/`cook` already do via resolveInheritance.
		// Without this, any extends-based formula whose new steps re-point onto a
		// parent step is rejected here even though it cooks and applies cleanly.
		if len(content.Extends) > 0 {
			childFields := formulaFields{
				FormulaType: content.FormulaType,
				Extends:     content.Extends,
				Vars:        content.Vars,
				Steps:       stepList(content.Steps),
			}
			merged, err := resolveInheritance(context.Background(), "create:"+name, childFields)
			if err != nil {
				return fmt.Errorf("resolving extends chain for dependency validation: %w", err)
			}
			// Fold every resolved (parent + child) step ID into the known set.
			// Use a fresh map: the merged set already deduplicates same-ID
			// overrides, so we must not re-run the duplicate check against the
			// child IDs already in allStepIDs.
			for id := range collectAllStepIDs(merged.Steps) {
				allStepIDs[id] = true
			}
		}
		if err := validateStepDeps(content.Steps, allStepIDs); err != nil {
			return err
		}

		// Validate variable patterns compile.
		for _, vd := range content.Vars {
			if vd.Pattern != "" {
				if _, err := regexp.Compile(vd.Pattern); err != nil {
					return fmt.Errorf("variable %q has invalid pattern %q: %w", vd.Name, vd.Pattern, err)
				}
			}
		}

		// Validate loop syntax on steps.
		if err := validateStepLoops(content.Steps); err != nil {
			return err
		}

		// Validate gate definitions on steps.
		if err := validateStepGates(content.Steps); err != nil {
			return err
		}

		// Validate expand steps.
		if err := validateStepExpands(content.Steps); err != nil {
			return err
		}

		// Validate branch steps.
		if err := validateStepBranches(content.Steps); err != nil {
			return err
		}

		// Validate waits_for fields.
		if err := validateStepWaitsFor(content.Steps); err != nil {
			return err
		}

		// Validate on_complete definitions.
		if err := validateStepOnComplete(content.Steps); err != nil {
			return err
		}

		// Build the fields JSON.
		fields := map[string]any{
			"vars":  content.Vars,
			"steps": content.Steps,
		}
		if content.FormulaType != "" {
			fields["formula_type"] = content.FormulaType
		}
		if len(content.Extends) > 0 {
			fields["extends"] = content.Extends
		}
		if content.Phase != "" {
			fields["phase"] = content.Phase
		}
		if len(content.DefaultRoles) > 0 {
			fields["default_roles"] = content.DefaultRoles
		}
		if len(content.RequiresSkills) > 0 {
			fields["requires_skills"] = content.RequiresSkills
		}
		if assignee != "" {
			fields["assigned_agent"] = assignee
		}
		if len(content.Aspects) > 0 {
			fields["aspects"] = content.Aspects
		}
		if content.Match != "" {
			fields["match"] = content.Match
		}
		if len(content.Before) > 0 {
			fields["before"] = content.Before
		}
		if len(content.After) > 0 {
			fields["after"] = content.After
		}
		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encoding fields: %w", err)
		}

		// Warn if a formula with the same title already exists.
		dupCheck, dupErr := beadsClient.ListBeads(context.Background(), &client.ListBeadsRequest{
			Type:   []string{"formula"},
			Status: []string{"open", "in_progress"},
			Search: name,
			Labels: labels,
			Limit:  100,
		})
		if dupErr == nil {
			for _, eb := range dupCheck.Beads {
				if eb.Title == name {
					fmt.Fprintf(os.Stderr, "Warning: formula %q already exists (%s, created %s)\n",
						name, eb.ID, eb.CreatedAt.Format("2006-01-02"))
					fmt.Fprintf(os.Stderr, "Consider updating it with: kd formula update %s --file <file>\n", eb.ID)
					fmt.Fprintln(os.Stderr, "Or close the old one first: kd close "+eb.ID)
				}
			}
		}

		req := &client.CreateBeadRequest{
			Title:       name,
			Description: description,
			Type:        "formula",
			Priority:    priority,
			Labels:      labels,
			CreatedBy:   actor,
			Fields:      fieldsJSON,
		}

		bead, err := beadsClient.CreateBead(context.Background(), req)
		if err != nil {
			return fmt.Errorf("creating formula: %w", err)
		}

		if jsonOutput {
			printBeadJSON(bead)
		} else {
			fmt.Printf("Created formula %s: %s\n", bead.ID, bead.Title)
			fmt.Printf("  Steps: %d\n", len(content.Steps))
			fmt.Printf("  Vars:  %d\n", len(content.Vars))
		}
		return nil
	},
}

// collectStepIDs recursively collects all step IDs and validates uniqueness and required fields.
func collectStepIDs(steps []FormulaStep, seen map[string]bool) error {
	for _, s := range steps {
		if s.ID == "" {
			return fmt.Errorf("every step must have an id")
		}
		if s.Title == "" {
			return fmt.Errorf("step %q must have a title", s.ID)
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
		if len(s.Children) > 0 {
			if err := collectStepIDs(s.Children, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectAllStepIDs returns the set of every step ID (recursing into children)
// without enforcing uniqueness or required fields. Used when folding the
// resolved extends chain into the known-ID set for dependency validation —
// the chain has already been merge-deduplicated, so the strict collectStepIDs
// uniqueness check would spuriously fail on child IDs already counted.
func collectAllStepIDs(steps []FormulaStep) map[string]bool {
	ids := make(map[string]bool)
	var walk func([]FormulaStep)
	walk = func(ss []FormulaStep) {
		for _, s := range ss {
			if s.ID != "" {
				ids[s.ID] = true
			}
			if len(s.Children) > 0 {
				walk(s.Children)
			}
		}
	}
	walk(steps)
	return ids
}

// validateStepDeps recursively validates depends_on references against the global step ID set.
func validateStepDeps(steps []FormulaStep, allIDs map[string]bool) error {
	for _, s := range steps {
		for _, dep := range s.DependsOn {
			if !allIDs[dep] {
				return fmt.Errorf("step %q depends_on unknown step %q", s.ID, dep)
			}
		}
		if len(s.Children) > 0 {
			if err := validateStepDeps(s.Children, allIDs); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateStepLoops recursively validates loop definitions on all steps.
func validateStepLoops(steps []FormulaStep) error {
	for _, s := range steps {
		if s.Loop != nil {
			if s.Loop.Var == "" {
				return fmt.Errorf("step %q: loop must specify a var name", s.ID)
			}
			modes := 0
			if len(s.Loop.Values) > 0 {
				modes++
			}
			if s.Loop.Count > 0 {
				modes++
			}
			if s.Loop.Range != "" {
				modes++
				if _, _, err := parseRange(s.Loop.Range); err != nil {
					return fmt.Errorf("step %q: %w", s.ID, err)
				}
			}
			if modes == 0 {
				return fmt.Errorf("step %q: loop must specify values, count, or range", s.ID)
			}
			if modes > 1 {
				return fmt.Errorf("step %q: loop must use exactly one of values, count, or range", s.ID)
			}
		}
		if len(s.Children) > 0 {
			if err := validateStepLoops(s.Children); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateStepGates recursively validates gate definitions on all steps.
func validateStepGates(steps []FormulaStep) error {
	for _, s := range steps {
		if s.Gate != nil {
			switch s.Gate.Type {
			case "decision":
				if s.Gate.Prompt == "" {
					return fmt.Errorf("step %q: decision gate must have a prompt", s.ID)
				}
			case "sync", "manual":
				// valid, no extra requirements
			default:
				return fmt.Errorf("step %q: invalid gate type %q (must be decision, sync, or manual)", s.ID, s.Gate.Type)
			}
		}
		if len(s.Children) > 0 {
			if err := validateStepGates(s.Children); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateStepExpands recursively validates expand fields on all steps.
func validateStepExpands(steps []FormulaStep) error {
	for _, s := range steps {
		if s.Expand != "" {
			// Expand steps should not also have a title (they get replaced by expansion content).
			// But we allow it for display purposes (e.g. dry-run output).
			if s.Loop != nil {
				return fmt.Errorf("step %q: expand and loop cannot be used together", s.ID)
			}
			if s.Gate != nil {
				return fmt.Errorf("step %q: expand and gate cannot be used together", s.ID)
			}
			if len(s.Children) > 0 {
				return fmt.Errorf("step %q: expand and children cannot be used together", s.ID)
			}
			if s.Branch != nil {
				return fmt.Errorf("step %q: expand and branch cannot be used together", s.ID)
			}
			if s.OnComplete != nil {
				return fmt.Errorf("step %q: expand and on_complete cannot be used together", s.ID)
			}
		}
		if s.Branch != nil {
			if s.Loop != nil {
				return fmt.Errorf("step %q: branch and loop cannot be used together", s.ID)
			}
			if s.Gate != nil {
				return fmt.Errorf("step %q: branch and gate cannot be used together", s.ID)
			}
			if len(s.Children) > 0 {
				return fmt.Errorf("step %q: branch and children cannot be used together", s.ID)
			}
		}
		if len(s.Children) > 0 {
			if err := validateStepExpands(s.Children); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateStepBranches recursively validates branch definitions on all steps.
func validateStepBranches(steps []FormulaStep) error {
	for _, s := range steps {
		if s.Branch != nil {
			if s.Branch.On == "" {
				return fmt.Errorf("step %q: branch must specify an 'on' variable expression", s.ID)
			}
			if len(s.Branch.Cases) == 0 {
				return fmt.Errorf("step %q: branch must have at least one case", s.ID)
			}
			for val, bc := range s.Branch.Cases {
				if bc.Expand == "" {
					return fmt.Errorf("step %q: branch case %q must specify an expand formula ID", s.ID, val)
				}
			}
			if s.Branch.Default != nil && s.Branch.Default.Expand == "" {
				return fmt.Errorf("step %q: branch default must specify an expand formula ID", s.ID)
			}
		}
		if len(s.Children) > 0 {
			if err := validateStepBranches(s.Children); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateStepWaitsFor recursively validates waits_for fields on all steps.
func validateStepWaitsFor(steps []FormulaStep) error {
	for _, s := range steps {
		if s.WaitsFor != "" {
			switch s.WaitsFor {
			case "all-children":
				if len(s.Children) == 0 {
					return fmt.Errorf("step %q: waits_for=all-children requires children", s.ID)
				}
			default:
				return fmt.Errorf("step %q: invalid waits_for %q (must be all-children)", s.ID, s.WaitsFor)
			}
		}
		if len(s.Children) > 0 {
			if err := validateStepWaitsFor(s.Children); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateStepOnComplete recursively validates on_complete definitions on all steps.
func validateStepOnComplete(steps []FormulaStep) error {
	for _, s := range steps {
		if s.OnComplete != nil {
			if s.OnComplete.ForEach == "" {
				return fmt.Errorf("step %q: on_complete must specify a for_each expression", s.ID)
			}
			if s.OnComplete.Var == "" {
				return fmt.Errorf("step %q: on_complete must specify a var name", s.ID)
			}
			if s.OnComplete.Expand == "" {
				return fmt.Errorf("step %q: on_complete must specify an expand formula ID", s.ID)
			}
			// on_complete is mutually exclusive with expand, branch, loop, and gate.
			if s.Expand != "" {
				return fmt.Errorf("step %q: on_complete and expand cannot be used together", s.ID)
			}
			if s.Branch != nil {
				return fmt.Errorf("step %q: on_complete and branch cannot be used together", s.ID)
			}
			if s.Loop != nil {
				return fmt.Errorf("step %q: on_complete and loop cannot be used together", s.ID)
			}
			if s.Gate != nil {
				return fmt.Errorf("step %q: on_complete and gate cannot be used together", s.ID)
			}
		}
		if len(s.Children) > 0 {
			if err := validateStepOnComplete(s.Children); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseRange parses a "start..end" range string into start and end integers (inclusive).
func parseRange(r string) (int, int, error) {
	return formula.ParseRange(r)
}

func init() {
	formulaCreateCmd.Flags().StringP("file", "f", "", "JSON file with formula definition (use - for stdin)")
	formulaCreateCmd.Flags().StringP("description", "d", "", "formula description")
	formulaCreateCmd.Flags().IntP("priority", "p", 2, "priority (0-4)")
	formulaCreateCmd.Flags().StringSliceP("label", "l", nil, "labels (repeatable)")
	formulaCreateCmd.Flags().String("assignee", "", "agent to assign molecules to when this formula is poured")
	// Accept --labels as an alias of --label (ka-0000000987).
	aliasLabelFlags(formulaCreateCmd)
}
