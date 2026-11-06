package main

import (
	"context"
	"fmt"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var deferCmd = &cobra.Command{
	Use:     "defer <id>...",
	Short:   "Defer one or more beads",
	GroupID: "workflow",
	Args:    cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		until, _ := cmd.Flags().GetString("until")

		var deferUntil *time.Time
		if until != "" {
			t, err := time.Parse(time.RFC3339, until)
			if err != nil {
				return fmt.Errorf("parsing --until: %w", err)
			}
			deferUntil = &t
		}

		statusVal := "deferred"
		for _, id := range args {
			req := &client.UpdateBeadRequest{
				Status:     &statusVal,
				DeferUntil: deferUntil,
			}
			bead, err := beadsClient.UpdateBead(ctx, id, req)
			if err != nil {
				return fmt.Errorf("deferring %s: %w", id, err)
			}
			if jsonOutput {
				printBeadJSON(bead)
			} else {
				printBeadTable(bead)
				if len(args) > 1 {
					fmt.Println()
				}
			}
		}
		return nil
	},
}

var undeferCmd = &cobra.Command{
	Use:     "undefer <id>...",
	Short:   "Undefer one or more beads (set status to open, clear defer_until)",
	GroupID: "workflow",
	Args:    cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		statusVal := "open"
		clearDefer := time.Time{}

		for _, id := range args {
			req := &client.UpdateBeadRequest{
				Status:     &statusVal,
				DeferUntil: &clearDefer,
			}
			bead, err := beadsClient.UpdateBead(ctx, id, req)
			if err != nil {
				return fmt.Errorf("undeferring %s: %w", id, err)
			}
			if jsonOutput {
				printBeadJSON(bead)
			} else {
				printBeadTable(bead)
				if len(args) > 1 {
					fmt.Println()
				}
			}
		}
		return nil
	},
}

func init() {
	deferCmd.Flags().String("until", "", "defer until date (RFC3339 format, e.g. 2025-03-01T00:00:00Z)")
}
