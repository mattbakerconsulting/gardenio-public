package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

var deleteCmd = &cobra.Command{
	Use:     "delete <id>...",
	Short:   "Delete one or more beads",
	GroupID: "beads",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		for _, id := range args {
			if err := beadsClient.DeleteBead(context.Background(), id); err != nil {
				return fmt.Errorf("deleting %s: %w", id, err)
			}

			fmt.Printf("Deleted %s\n", id)
		}
		return nil
	},
}
