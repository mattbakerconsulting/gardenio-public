package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

var molShowCmd = &cobra.Command{
	Use:   "show <molecule-id>",
	Short: "Show molecule details with its child beads",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		ctx := context.Background()

		bead, err := beadsClient.GetBead(ctx, id)
		if err != nil {
			return fmt.Errorf("getting molecule: %w", err)
		}

		if string(bead.Type) != "molecule" {
			return fmt.Errorf("bead %s is type %q, not molecule", id, bead.Type)
		}

		if jsonOutput {
			printBeadJSON(bead)
			return nil
		}

		printBeadTable(bead)

		// Show formula source.
		if len(bead.Fields) > 0 {
			var fields struct {
				FormulaID   string          `json:"formula_id"`
				AppliedVars json.RawMessage `json:"applied_vars"`
			}
			if json.Unmarshal(bead.Fields, &fields) == nil {
				if fields.FormulaID != "" {
					fmt.Printf("\nFormula:     %s\n", fields.FormulaID)
				}
				if len(fields.AppliedVars) > 0 {
					var vars map[string]string
					if json.Unmarshal(fields.AppliedVars, &vars) == nil && len(vars) > 0 {
						fmt.Println("Variables:")
						for k, v := range vars {
							fmt.Printf("  %s = %s\n", k, v)
						}
					}
				}
			}
		}

		// Show child beads (reverse deps where type = parent-child).
		revDeps, err := beadsClient.GetReverseDependencies(ctx, id)
		if err == nil && len(revDeps) > 0 {
			fmt.Println("\nSteps:")
			for _, dep := range revDeps {
				if string(dep.Type) != "parent-child" {
					continue
				}
				child, err := beadsClient.GetBead(ctx, dep.BeadID)
				if err != nil {
					fmt.Printf("  %s (could not resolve)\n", dep.BeadID)
					continue
				}
				fmt.Printf("  %s [%s] %s — %s\n",
					child.ID, child.Status, child.Type, child.Title)
			}
		}

		return nil
	},
}
