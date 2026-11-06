package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var formulaShowCmd = &cobra.Command{
	Use:   "show <formula-id>",
	Short: "Show formula details including vars and steps",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		bead, err := beadsClient.GetBead(context.Background(), id)
		if err != nil {
			return fmt.Errorf("getting formula: %w", err)
		}

		if string(bead.Type) != "formula" {
			return fmt.Errorf("bead %s is type %q, not formula", id, bead.Type)
		}

		if jsonOutput {
			printBeadJSON(bead)
			return nil
		}

		printBeadTable(bead)

		if len(bead.Fields) == 0 {
			return nil
		}

		var fields formulaFields
		if err := json.Unmarshal(bead.Fields, &fields); err != nil {
			return nil
		}

		formulaType := fields.FormulaType
		if formulaType == "" {
			formulaType = "workflow"
		}
		fmt.Printf("\nFormula Type: %s\n", formulaType)

		if len(fields.Extends) > 0 {
			fmt.Printf("Extends: %s\n", strings.Join(fields.Extends, ", "))
		}

		if fields.Phase != "" {
			fmt.Printf("Phase: %s\n", fields.Phase)
		}

		if fields.AssignedAgent != "" {
			fmt.Printf("Assigned Agent: %s\n", fields.AssignedAgent)
		}

		if len(fields.Vars) > 0 {
			fmt.Println("\nVariables:")
			for _, v := range fields.Vars {
				req := ""
				if v.Required {
					req = " (required)"
				}
				def := ""
				if v.Default != "" {
					def = fmt.Sprintf(" [default: %s]", v.Default)
				}
				fmt.Printf("  {{%s}}%s%s", v.Name, req, def)
				if v.Description != "" {
					fmt.Printf(" — %s", v.Description)
				}
				if len(v.Enum) > 0 {
					fmt.Printf(" [enum: %s]", strings.Join(v.Enum, "|"))
				}
				if v.Pattern != "" {
					fmt.Printf(" [pattern: %s]", v.Pattern)
				}
				fmt.Println()
			}
		}

		if len(fields.Steps) > 0 {
			fmt.Println("\nSteps:")
			var printSteps func(steps []FormulaStep, indent string)
			printSteps = func(steps []FormulaStep, indent string) {
				for _, s := range steps {
					typ := s.Type
					if typ == "" {
						typ = "task"
					}
					deps := ""
					if len(s.DependsOn) > 0 {
						deps = fmt.Sprintf(" (after: %s)", strings.Join(s.DependsOn, ", "))
					}
					cond := ""
					if s.Condition != "" {
						cond = fmt.Sprintf(" [if %s]", s.Condition)
					}
					fmt.Printf("%s%s: %s [%s]%s%s\n", indent, s.ID, s.Title, typ, deps, cond)
					if len(s.Children) > 0 {
						printSteps(s.Children, indent+"  ")
					}
				}
			}
			printSteps(fields.Steps, "  ")
		}

		return nil
	},
}
