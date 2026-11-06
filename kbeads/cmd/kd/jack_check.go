package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var jackCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Check for expired jacks",
	RunE: func(cmd *cobra.Command, args []string) error {
		autoEscalate, _ := cmd.Flags().GetBool("auto-escalate")

		// List all in_progress jacks.
		resp, err := beadsClient.ListBeads(context.Background(), &client.ListBeadsRequest{
			Type:   []string{"jack"},
			Status: []string{"in_progress"},
			Limit:  100,
		})
		if err != nil {
			return fmt.Errorf("listing jacks: %w", err)
		}

		now := time.Now().UTC()
		expired := 0
		active := 0

		for _, bead := range resp.Beads {
			var fields map[string]any
			if len(bead.Fields) > 0 {
				_ = json.Unmarshal(bead.Fields, &fields)
			}

			expiresStr, _ := fields["jack_expires_at"].(string)
			expiresAt, err := time.Parse(time.RFC3339, expiresStr)
			if err != nil {
				continue
			}

			target, _ := fields["jack_target"].(string)

			if now.After(expiresAt) {
				expired++
				fmt.Printf("EXPIRED  %s  target=%s  expired=%s ago\n",
					bead.ID, target, now.Sub(expiresAt).Truncate(time.Second))

				if autoEscalate {
					escalated, _ := fields["jack_escalated"].(bool)
					if !escalated {
						// Mark as escalated.
						fields["jack_escalated"] = true
						fields["jack_escalated_at"] = now.Format(time.RFC3339)
						fieldsJSON, _ := json.Marshal(fields)
						_, _ = beadsClient.UpdateBead(context.Background(), bead.ID, &client.UpdateBeadRequest{
							Fields: fieldsJSON,
						})
						fmt.Printf("  → Escalated (marked jack_escalated=true)\n")
					}
				}
			} else {
				active++
				remaining := expiresAt.Sub(now).Truncate(time.Second)
				fmt.Printf("ACTIVE   %s  target=%s  remaining=%s\n",
					bead.ID, target, remaining)
			}
		}

		fmt.Printf("\nSummary: %d expired, %d active\n", expired, active)
		return nil
	},
}

func init() {
	jackCheckCmd.Flags().Bool("auto-escalate", false, "mark expired jacks as escalated")
}
