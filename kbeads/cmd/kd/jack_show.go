package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var jackShowCmd = &cobra.Command{
	Use:   "show <jack-id>",
	Short: "Show details of a jack",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		bead, err := beadsClient.GetBead(context.Background(), id)
		if err != nil {
			return fmt.Errorf("getting jack %s: %w", id, err)
		}

		if jsonOutput {
			printBeadJSON(bead)
			return nil
		}

		var fields map[string]any
		if len(bead.Fields) > 0 {
			_ = json.Unmarshal(bead.Fields, &fields)
		}
		if fields == nil {
			fields = make(map[string]any)
		}

		target, _ := fields["jack_target"].(string)
		reason, _ := fields["jack_reason"].(string)
		revertPlan, _ := fields["jack_revert_plan"].(string)
		ttlStr, _ := fields["jack_ttl"].(string)
		expiresStr, _ := fields["jack_expires_at"].(string)
		extCount := 0
		if v, ok := fields["jack_extension_count"].(float64); ok {
			extCount = int(v)
		}
		reverted, _ := fields["jack_reverted"].(bool)
		closedReason, _ := fields["jack_closed_reason"].(string)

		fmt.Printf("ID:         %s [JACK]\n", bead.ID)
		fmt.Printf("Title:      %s\n", bead.Title)
		fmt.Printf("Status:     %s\n", bead.Status)
		fmt.Printf("Priority:   %d\n", bead.Priority)
		fmt.Printf("Target:     %s\n", target)
		fmt.Printf("TTL:        %s\n", ttlStr)

		if expiresAt, err := time.Parse(time.RFC3339, expiresStr); err == nil {
			now := time.Now().UTC()
			if now.After(expiresAt) {
				fmt.Printf("Expiry:     EXPIRED (%s ago)\n", now.Sub(expiresAt).Truncate(time.Second))
			} else {
				fmt.Printf("Expiry:     %s (%s remaining)\n",
					expiresAt.Format("2006-01-02 15:04:05"),
					expiresAt.Sub(now).Truncate(time.Second))
			}
		}

		if extCount > 0 {
			fmt.Printf("Extensions: %d/5\n", extCount)
		}

		if reason != "" {
			fmt.Printf("Reason:     %s\n", reason)
		}

		revertLabel := "Revert Plan"
		if reverted {
			revertLabel = "Revert Plan [REVERTED]"
		}
		fmt.Printf("%s: %s\n", revertLabel, revertPlan)

		if closedReason != "" {
			fmt.Printf("Close Reason: %s\n", closedReason)
		}

		if len(bead.Labels) > 0 {
			fmt.Printf("Labels:     %s\n", strings.Join(bead.Labels, ", "))
		}

		// Show changes.
		if v, ok := fields["jack_changes"]; ok {
			if arr, ok := v.([]any); ok && len(arr) > 0 {
				fmt.Printf("\nChanges (%d):\n", len(arr))
				for i, ch := range arr {
					m, ok := ch.(map[string]any)
					if !ok {
						continue
					}
					ts, _ := m["timestamp"].(string)
					action, _ := m["action"].(string)
					tgt, _ := m["target"].(string)
					cmdStr, _ := m["cmd"].(string)
					before, _ := m["before"].(string)
					after, _ := m["after"].(string)

					fmt.Printf("  %d. [%s] %s", i+1, ts, action)
					if tgt != "" {
						fmt.Printf(" %s", tgt)
					}
					fmt.Println()
					if cmdStr != "" {
						fmt.Printf("     cmd: %s\n", cmdStr)
					}
					if before != "" {
						fmt.Printf("     before: %s\n", before)
					}
					if after != "" {
						fmt.Printf("     after:  %s\n", after)
					}
				}
			}
		}

		fmt.Printf("\nCreated By: %s\n", bead.CreatedBy)
		if !bead.CreatedAt.IsZero() {
			fmt.Printf("Created At: %s\n", bead.CreatedAt.Format("2006-01-02 15:04:05"))
		}
		return nil
	},
}
