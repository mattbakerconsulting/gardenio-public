package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

var restoreCmd = &cobra.Command{
	Use:     "restore <id>...",
	Short:   "Restore one or more soft-deleted beads (admin)",
	Long:    "Restore beads previously soft-deleted with `kd delete`. The bead must still be within the soft-delete retention window. Requires admin role.",
	GroupID: "beads",
	Args:    cobra.MinimumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		for _, id := range args {
			b, err := beadsClient.RestoreBead(context.Background(), id)
			if err != nil {
				return fmt.Errorf("restoring %s: %w", id, err)
			}
			fmt.Printf("Restored %s\n", b.ID)
		}
		return nil
	},
}
