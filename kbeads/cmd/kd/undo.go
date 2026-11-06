package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

var undoCmd = &cobra.Command{
	Use:     "undo <audit-row-id>",
	Short:   "Reverse one mutation_history entry (admin)",
	Long:    "Reverse a single mutation by its audit row id (as printed by `kd audit bead`). The bead's current value for that field must still equal the mutation's recorded new_value, and the mutation must be within the undo retention window. Requires admin role.",
	GroupID: "beads",
	Args:    cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("audit-row-id must be a number: %w", err)
		}

		resp, err := beadsClient.UndoMutation(context.Background(), id)
		if err != nil {
			return fmt.Errorf("undoing mutation %d: %w", id, err)
		}

		if jsonOutput {
			data, err := json.MarshalIndent(resp, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling JSON: %w", err)
			}
			fmt.Println(string(data))
			return nil
		}

		mut := resp.Undone
		fmt.Printf("Undid mutation %d on %s: %s.%s %q <- %q\n",
			mut.ID, mut.BeadID, mut.BeadID, mut.Field, mut.OldValue, mut.NewValue)
		return nil
	},
}
