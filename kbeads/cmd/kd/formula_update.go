package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// runFormulaUpdate replaces a formula's fields from a JSON file and/or updates
// the assigned agent.
func runFormulaUpdate(id, filePath, assignee string, clearAssignee bool) error {
	if filePath == "" && assignee == "" && !clearAssignee {
		return fmt.Errorf("--file or --assignee is required")
	}

	// Validate the bead is a formula.
	ctx := context.Background()
	bead, err := beadsClient.GetBead(ctx, id)
	if err != nil {
		return fmt.Errorf("getting formula: %w", err)
	}
	if string(bead.Type) != "formula" {
		return fmt.Errorf("bead %s is type %q, not formula", id, bead.Type)
	}

	// Start from existing fields when no file is provided.
	var fields map[string]any

	if filePath != "" {
		// Read input.
		var data []byte
		if filePath == "-" {
			data, err = os.ReadFile("/dev/stdin")
		} else {
			data, err = os.ReadFile(filePath)
		}
		if err != nil {
			return fmt.Errorf("reading file: %w", err)
		}

		// Parse and validate.
		var content struct {
			Extends []string      `json:"extends,omitempty"`
			Vars    varDefList    `json:"vars"` // tolerant: accepts array or legacy string form (ka-0000001116)
			Steps   []FormulaStep `json:"steps"`
		}
		if err := json.Unmarshal(data, &content); err != nil {
			return fmt.Errorf("parsing formula JSON: %w", err)
		}

		// A formula may legitimately carry zero OWN steps when it `extends`
		// parents and only overrides vars/phase — mirror formula create's rule.
		if len(content.Steps) == 0 && len(content.Extends) == 0 {
			return fmt.Errorf("formula must have at least one step")
		}

		// Validate step IDs are unique and required fields are present.
		stepIDs := make(map[string]bool, len(content.Steps))
		if err := collectStepIDs(content.Steps, stepIDs); err != nil {
			return err
		}
		// When this formula `extends` parents, its steps may depend_on a step
		// inherited from a parent. Resolve the inheritance chain and fold the
		// parent step IDs into the known-ID set before dependency validation —
		// mirroring what formula create/cook/apply already do via
		// resolveInheritance. Without this, dumping an extending formula and
		// updating it straight back (zero edits) is rejected with "depends_on
		// unknown step", making any extends-based formula un-editable through
		// the CLI (ka-0000005592).
		if len(content.Extends) > 0 {
			childFields := formulaFields{
				Extends: content.Extends,
				Vars:    content.Vars,
				Steps:   stepList(content.Steps),
			}
			merged, mErr := resolveInheritance(ctx, "update:"+id, childFields)
			if mErr != nil {
				return fmt.Errorf("resolving extends chain for dependency validation: %w", mErr)
			}
			// Fold every resolved (parent + child) step ID into the known set.
			// The merged set is already dedup-merged, so use collectAllStepIDs
			// (no uniqueness re-check) rather than the strict collectStepIDs.
			for id := range collectAllStepIDs(merged.Steps) {
				stepIDs[id] = true
			}
		}
		if err := validateStepDeps(content.Steps, stepIDs); err != nil {
			return err
		}

		// Preserve ALL top-level formula fields from the file (phase,
		// formula_type, default_roles, requires_skills, aspects, extends,
		// assigned_agent, etc.), not just vars/steps. Rebuilding a
		// {vars,steps}-only map silently dropped every other field — e.g. a
		// `phase: liquid` flip never persisted (ka-0000005050).
		fields = make(map[string]any)
		if err := json.Unmarshal(data, &fields); err != nil {
			return fmt.Errorf("parsing formula JSON fields: %w", err)
		}
		// Write the validated/normalized vars+steps back over the raw decode
		// so tolerant legacy forms are canonicalized.
		fields["vars"] = content.Vars
		fields["steps"] = content.Steps
	} else {
		// Preserve existing fields.
		fields = make(map[string]any)
		if len(bead.Fields) > 0 {
			if err := json.Unmarshal(bead.Fields, &fields); err != nil {
				return fmt.Errorf("parsing existing fields: %w", err)
			}
		}
	}

	// Update assigned_agent.
	if clearAssignee {
		delete(fields, "assigned_agent")
	} else if assignee != "" {
		fields["assigned_agent"] = assignee
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encoding fields: %w", err)
	}

	req := &client.UpdateBeadRequest{
		Fields: fieldsJSON,
	}

	_, err = beadsClient.UpdateBead(ctx, id, req)
	if err != nil {
		return fmt.Errorf("updating formula: %w", err)
	}

	if jsonOutput {
		fmt.Printf("{\"id\":%q}\n", id)
	} else {
		fmt.Printf("Updated formula %s\n", id)
	}
	return nil
}

var formulaUpdateCmd = &cobra.Command{
	Use:   "update <formula-id>",
	Short: "Update formula definition or assignment",
	Long: `Update an existing formula's definition and/or agent assignment.

Use --file to replace the vars and steps. Use --assignee to set the
default agent assignment for molecules created from this formula.

Examples:

  kd formula update ka-abc123 --file formula.json
  kd formula update ka-abc123 --assignee my-agent
  kd formula update ka-abc123 --assignee ""`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		filePath, _ := cmd.Flags().GetString("file")
		assignee, _ := cmd.Flags().GetString("assignee")
		clearAssignee := cmd.Flags().Changed("assignee") && assignee == ""
		return runFormulaUpdate(args[0], filePath, assignee, clearAssignee)
	},
}

func init() {
	formulaUpdateCmd.Flags().StringP("file", "f", "", "JSON file with formula definition (use - for stdin)")
	formulaUpdateCmd.Flags().String("assignee", "", "agent to assign molecules to when this formula is poured (empty string to clear)")
}
