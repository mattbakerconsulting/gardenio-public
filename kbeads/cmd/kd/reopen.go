package main

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var reopenCmd = &cobra.Command{
	Use:     "reopen <id>...",
	Short:   "Reopen one or more closed beads",
	GroupID: "workflow",
	Args:    cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		open := "open"
		for _, id := range args {
			bead, err := beadsClient.UpdateBead(context.Background(), id, &client.UpdateBeadRequest{
				Status: &open,
			})
			if err != nil {
				return fmt.Errorf("reopening %s: %w", id, err)
			}

			if jsonOutput {
				printBeadJSON(bead)
			} else {
				if len(args) > 1 {
					fmt.Printf("Reopened %s\n", bead.ID)
				} else {
					printBeadTable(bead)
				}
			}
		}
		return nil
	},
}
