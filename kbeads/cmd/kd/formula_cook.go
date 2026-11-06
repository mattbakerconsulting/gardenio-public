package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

func newCookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cook <formula-id-or-file>",
		Short: "Compile a formula without creating beads",
		Long: `Compile a formula to preview the expanded step graph without creating any beads.

Cook runs the Resolve and Expand phases of formula instantiation, then prints
the resulting steps with variable substitution applied. Unlike --dry-run, cook
shows ALL steps including those skipped by conditions.

Accepts either a bead ID (ka-xxx) or a file path (.json) to load a formula
from the filesystem. File-based formulas support extends (resolved from DB).

Use cook to debug formula expansion, verify variable substitution, and preview
the step graph before pouring or wisping.

Examples:
  kd formula cook ka-abc --var component=auth
  kd formula cook ka-abc --var component=auth --json     # machine-readable
  kd formula cook ka-abc --var component=auth --verbose   # show skipped conditions
  kd formula cook ./formulas/deploy.json --var env=prod   # cook from file`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			formulaID := args[0]
			varPairs, _ := cmd.Flags().GetStringArray("var")
			verbose, _ := cmd.Flags().GetBool("verbose")
			ctx := context.Background()

			// ── Resolve ─────────────────────────────────────
			var bead *model.Bead
			var fields formulaFields

			if isFilePath(formulaID) {
				var err error
				bead, fields, err = loadFormulaFromFile(formulaID)
				if err != nil {
					return err
				}
			} else {
				var err error
				bead, err = beadsClient.GetBead(ctx, formulaID)
				if err != nil {
					return fmt.Errorf("resolving formula: %w", err)
				}
				if string(bead.Type) != "formula" {
					return fmt.Errorf("bead %s is type %q, not formula", formulaID, bead.Type)
				}

				if len(bead.Fields) == 0 {
					return fmt.Errorf("formula %s has no fields (empty formula)", formulaID)
				}
				if err := json.Unmarshal(bead.Fields, &fields); err != nil {
					return fmt.Errorf("parsing formula fields: %w", err)
				}
			}

			// Resolve inheritance chain.
			if len(fields.Extends) > 0 {
				var err error
				fields, err = resolveInheritance(ctx, formulaID, fields)
				if err != nil {
					return fmt.Errorf("resolving inheritance: %w", err)
				}
			}

			// Parse vars.
			vars := make(map[string]string, len(varPairs))
			for _, p := range varPairs {
				k, v, ok := splitField(p)
				if !ok {
					return fmt.Errorf("invalid --var %q: expected key=value", p)
				}
				vars[k] = v
			}

			// Apply defaults and validate.
			for _, vd := range fields.Vars {
				if _, ok := vars[vd.Name]; !ok {
					if vd.Default != "" {
						vars[vd.Name] = vd.Default
					} else if vd.Required {
						return fmt.Errorf("required variable {{%s}} not provided (use --var %s=<value>)", vd.Name, vd.Name)
					}
				}
			}

			// Apply aspects before expansion.
			if len(fields.Aspects) > 0 {
				var err error
				fields.Steps, err = applyAspects(ctx, fields.Aspects, fields.Steps)
				if err != nil {
					return fmt.Errorf("applying aspects: %w", err)
				}
			}

			// Merge needs into depends_on.
			for i := range fields.Steps {
				if len(fields.Steps[i].Needs) > 0 {
					seen := make(map[string]bool, len(fields.Steps[i].DependsOn))
					for _, d := range fields.Steps[i].DependsOn {
						seen[d] = true
					}
					for _, n := range fields.Steps[i].Needs {
						if !seen[n] {
							fields.Steps[i].DependsOn = append(fields.Steps[i].DependsOn, n)
							seen[n] = true
						}
					}
				}
			}

			// ── Expand ──────────────────────────────────────
			type cookedStep struct {
				FormulaStep
				skip      bool
				condition string // original condition for display
				children  []cookedStep
			}

			var cookSteps func(fsteps []FormulaStep) ([]cookedStep, error)
			cookSteps = func(fsteps []FormulaStep) ([]cookedStep, error) {
				var result []cookedStep
				for _, s := range fsteps {
					// Branch steps: evaluate and inline matching case.
					if s.Branch != nil {
						inlinedSteps, err := expandBranch(ctx, s, vars)
						if err != nil {
							return nil, fmt.Errorf("branching step %q: %w", s.ID, err)
						}
						for _, is := range inlinedSteps {
							cs := cookedStep{
								FormulaStep: is,
								condition:   is.Condition,
							}
							if is.Condition != "" {
								if !evaluateCondition(is.Condition, vars) {
									cs.skip = true
								}
							}
							cs.Title = substituteVars(is.Title, vars)
							cs.Description = substituteVars(is.Description, vars)
							cs.Assignee = substituteVars(is.Assignee, vars)
							result = append(result, cs)
						}
						continue
					}

					// Inline expansions.
					if s.Expand != "" {
						inlinedSteps, err := inlineExpansion(ctx, s, vars)
						if err != nil {
							return nil, fmt.Errorf("expanding step %q: %w", s.ID, err)
						}
						for _, is := range inlinedSteps {
							cs := cookedStep{
								FormulaStep: is,
								condition:   is.Condition,
							}
							if is.Condition != "" {
								if !evaluateCondition(is.Condition, vars) {
									cs.skip = true
								}
							}
							cs.Title = substituteVars(is.Title, vars)
							cs.Description = substituteVars(is.Description, vars)
							cs.Assignee = substituteVars(is.Assignee, vars)
							result = append(result, cs)
						}
						continue
					}

					// Expand loops.
					if s.Loop != nil {
						loopVals := expandLoopValues(s.Loop)
						for _, val := range loopVals {
							loopVars := make(map[string]string, len(vars)+1)
							for k, v := range vars {
								loopVars[k] = v
							}
							loopVars[s.Loop.Var] = val

							iteration := s
							iteration.Loop = nil
							iteration.ID = substituteVars(s.ID, loopVars)

							cs := cookedStep{
								FormulaStep: iteration,
								condition:   s.Condition,
							}
							if s.Condition != "" {
								if !evaluateCondition(s.Condition, loopVars) {
									cs.skip = true
								}
							}
							cs.Title = substituteVars(s.Title, loopVars)
							cs.Description = substituteVars(s.Description, loopVars)
							cs.Assignee = substituteVars(s.Assignee, loopVars)
							if len(s.Children) > 0 {
								children, cerr := cookSteps(s.Children)
								if cerr != nil {
									return nil, cerr
								}
								cs.children = children
							}
							result = append(result, cs)
						}
						continue
					}

					cs := cookedStep{
						FormulaStep: s,
						condition:   s.Condition,
					}
					if s.Condition != "" {
						if !evaluateCondition(s.Condition, vars) {
							cs.skip = true
						}
					}
					cs.Title = substituteVars(s.Title, vars)
					cs.Description = substituteVars(s.Description, vars)
					cs.Assignee = substituteVars(s.Assignee, vars)
					if len(s.Children) > 0 {
						children, cerr := cookSteps(s.Children)
						if cerr != nil {
							return nil, cerr
						}
						cs.children = children
					}
					result = append(result, cs)
				}
				return result, nil
			}

			steps, err := cookSteps(fields.Steps)
			if err != nil {
				return fmt.Errorf("expanding steps: %w", err)
			}

			// Count active/skipped recursively.
			var countCook func(steps []cookedStep) (int, int)
			countCook = func(steps []cookedStep) (int, int) {
				a, sk := 0, 0
				for _, s := range steps {
					if s.skip {
						sk++
					} else {
						a++
					}
					ca, csk := countCook(s.children)
					a += ca
					sk += csk
				}
				return a, sk
			}
			active, skipped := countCook(steps)

			formulaType := fields.FormulaType
			if formulaType == "" {
				formulaType = "workflow"
			}

			// ── Output ──────────────────────────────────────
			if jsonOutput {
				type jsonStep struct {
					ID          string          `json:"id"`
					Title       string          `json:"title"`
					Type        string          `json:"type,omitempty"`
					DependsOn   []string        `json:"depends_on,omitempty"`
					Condition   string          `json:"condition,omitempty"`
					Skipped     bool            `json:"skipped,omitempty"`
					Description string          `json:"description,omitempty"`
					Assignee    string          `json:"assignee,omitempty"`
					Roles       []string        `json:"roles,omitempty"`
					OnComplete  *StepOnComplete `json:"on_complete,omitempty"`
					Gate        *StepGate       `json:"gate,omitempty"`
					Children    []*jsonStep     `json:"children,omitempty"`
				}
				var flattenJSON func(steps []cookedStep) []*jsonStep
				flattenJSON = func(steps []cookedStep) []*jsonStep {
					var result []*jsonStep
					for _, s := range steps {
						if !verbose && s.skip {
							continue
						}
						js := &jsonStep{
							ID:          s.ID,
							Title:       s.Title,
							Type:        cookStepType(s.FormulaStep),
							DependsOn:   s.DependsOn,
							Condition:   s.condition,
							Skipped:     s.skip,
							Description: s.Description,
							Assignee:    s.Assignee,
							Roles:       s.Roles,
							OnComplete:  s.OnComplete,
							Gate:        s.Gate,
						}
						if len(s.children) > 0 {
							js.Children = flattenJSON(s.children)
						}
						result = append(result, js)
					}
					return result
				}
				result := map[string]any{
					"formula_id":   formulaID,
					"formula_type": formulaType,
					"title":        substituteVars(bead.Title, vars),
					"vars":         vars,
					"steps":        flattenJSON(steps),
					"active":       active,
					"skipped":      skipped,
				}
				data, _ := json.MarshalIndent(result, "", "  ")
				fmt.Println(string(data))
				return nil
			}

			fmt.Printf("Formula: %s (%s)\n", bead.Title, formulaID)
			fmt.Printf("Type: %s\n", formulaType)
			if fields.Phase != "" {
				fmt.Printf("Phase: %s\n", fields.Phase)
			}
			fmt.Printf("Variables: %v\n", vars)
			fmt.Printf("\nCompiled steps (%d active", active)
			if skipped > 0 {
				fmt.Printf(", %d skipped", skipped)
			}
			fmt.Println("):")

			var printCookSteps func(steps []cookedStep, indent string)
			printCookSteps = func(steps []cookedStep, indent string) {
				for _, s := range steps {
					if s.skip && !verbose {
						continue
					}
					prefix := indent
					if s.skip {
						prefix = indent + "[skipped] "
					}
					typ := cookStepType(s.FormulaStep)
					deps := ""
					if len(s.DependsOn) > 0 {
						deps = fmt.Sprintf(" (after: %s)", strings.Join(s.DependsOn, ", "))
					}
					cond := ""
					if s.condition != "" {
						cond = fmt.Sprintf(" [if %s]", s.condition)
					}
					roles := s.Roles
					if len(roles) == 0 {
						roles = fields.DefaultRoles
					}
					roleStr := ""
					if len(roles) > 0 {
						roleStr = " → " + strings.Join(roles, ", ")
					}
					ocStr := ""
					if s.OnComplete != nil {
						ocStr = fmt.Sprintf(" {on_complete: expand=%s for_each=%s}", s.OnComplete.Expand, s.OnComplete.ForEach)
					}
					fmt.Printf("%s%s: %s [%s]%s%s%s%s%s\n", prefix, s.ID, s.Title, typ, deps, cond, roleStr, ocStr, cookGateString(s.Gate))
					printCookSteps(s.children, indent+"  ")
				}
			}
			printCookSteps(steps, "  ")

			return nil
		},
	}
	// StringArray keeps comma-containing var values intact (ka-0000003349).
	cmd.Flags().StringArray("var", nil, "variable substitution (key=value, repeatable; one value per flag, commas kept verbatim)")
	cmd.Flags().Bool("verbose", false, "show skipped steps with their conditions")
	return cmd
}

// cookStepType is the bead type a step pours as: a decision gate turns the
// step into a decision bead (runFormulaApply), every other step keeps its
// declared type, defaulting to task. Manual and sync gate steps stay tasks.
func cookStepType(s FormulaStep) string {
	if s.Gate != nil && s.Gate.Type == "decision" {
		return "decision"
	}
	if s.Type == "" {
		return "task"
	}
	return s.Type
}

// cookGateString renders a step's gate for the cook preview, e.g.
// ` {gate: manual, timeout 24h}` or ` {gate: decision "Ship it?" [yes|no]}`.
func cookGateString(g *StepGate) string {
	if g == nil {
		return ""
	}
	parts := []string{"gate: " + g.Type}
	if g.Type == "decision" {
		d := fmt.Sprintf("%q", g.Prompt)
		if len(g.Options) > 0 {
			ids := make([]string, len(g.Options))
			for i, o := range g.Options {
				ids[i] = o.ID
			}
			d += " [" + strings.Join(ids, "|") + "]"
		}
		parts[0] += " " + d
	}
	if g.Timeout != "" {
		parts = append(parts, "timeout "+g.Timeout)
	}
	return " {" + strings.Join(parts, ", ") + "}"
}
