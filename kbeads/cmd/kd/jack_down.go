package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var jackDownCmd = &cobra.Command{
	Use:   "down <jack-id>",
	Short: "Close a jack (revert and close change permit)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		reason, _ := cmd.Flags().GetString("reason")
		skipRevert, _ := cmd.Flags().GetBool("skip-revert-check")

		if reason == "" {
			return fmt.Errorf("--reason is required")
		}
		if skipRevert && len(reason) < 10 {
			return fmt.Errorf("--skip-revert-check requires a reason of at least 10 characters")
		}

		// Fetch current bead to get existing fields.
		bead, err := beadsClient.GetBead(context.Background(), id)
		if err != nil {
			return fmt.Errorf("getting jack %s: %w", id, err)
		}

		if string(bead.Type) != "jack" {
			return fmt.Errorf("%s is not a jack (type=%s)", id, bead.Type)
		}

		// Merge new fields into existing fields.
		var fields map[string]any
		if len(bead.Fields) > 0 {
			_ = json.Unmarshal(bead.Fields, &fields)
		}
		if fields == nil {
			fields = make(map[string]any)
		}
		fields["jack_reverted"] = !skipRevert
		fields["jack_closed_reason"] = reason
		fields["jack_closed_at"] = time.Now().UTC().Format(time.RFC3339)

		fieldsJSON, _ := json.Marshal(fields)

		// Close the bead.
		_, err = beadsClient.UpdateBead(context.Background(), id, &client.UpdateBeadRequest{
			Fields: fieldsJSON,
		})
		if err != nil {
			return fmt.Errorf("updating jack fields: %w", err)
		}

		_, err = beadsClient.CloseBead(context.Background(), id, actor)
		if err != nil {
			return fmt.Errorf("closing jack %s: %w", id, err)
		}

		if jsonOutput {
			bead, _ = beadsClient.GetBead(context.Background(), id)
			printBeadJSON(bead)
		} else {
			fmt.Printf("Jack closed: %s\n", id)
			fmt.Printf("  Reason: %s\n", reason)
			if skipRevert {
				fmt.Println("  Revert check: skipped")
			} else {
				fmt.Println("  Reverted: yes")
			}
		}
		return nil
	},
}

func init() {
	jackDownCmd.Flags().StringP("reason", "r", "", "reason for closing (required)")
	jackDownCmd.Flags().Bool("skip-revert-check", false, "close without verifying revert")
}
