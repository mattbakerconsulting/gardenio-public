package main

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var unclaimCmd = &cobra.Command{
	Use:     "unclaim <id>...",
	Short:   "Unclaim one or more beads",
	GroupID: "workflow",
	Args:    cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		empty := ""
		open := "open"
		for _, id := range args {
			bead, err := beadsClient.UpdateBead(context.Background(), id, &client.UpdateBeadRequest{
				Assignee: &empty,
				Status:   &open,
			})
			if err != nil {
				return fmt.Errorf("unclaiming %s: %w", id, err)
			}

			if jsonOutput {
				printBeadJSON(bead)
			} else {
				if len(args) > 1 {
					fmt.Printf("Unclaimed %s\n", bead.ID)
				} else {
					printBeadTable(bead)
				}
			}
		}
		return nil
	},
}
