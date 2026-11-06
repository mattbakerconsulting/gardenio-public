package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var adviceShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show details of an advice bead",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		bead, err := beadsClient.GetBead(context.Background(), id)
		if err != nil {
			return fmt.Errorf("getting advice %s: %w", id, err)
		}

		if jsonOutput {
			printBeadJSON(bead)
			return nil
		}

		fmt.Printf("ID:          %s\n", bead.ID)
		fmt.Printf("Title:       %s\n", bead.Title)
		fmt.Printf("Priority:    %d\n", bead.Priority)
		fmt.Printf("Status:      %s\n", bead.Status)
		if len(bead.Labels) > 0 {
			fmt.Printf("Labels:      %s\n", strings.Join(bead.Labels, ", "))
		}
		if bead.Description != "" {
			fmt.Printf("Description: %s\n", bead.Description)
		}

		// Show advice-specific fields.
		if len(bead.Fields) > 0 {
			var f map[string]any
			if json.Unmarshal(bead.Fields, &f) == nil {
				if v, ok := f["hook_command"]; ok {
					fmt.Printf("Hook Cmd:    %v\n", v)
				}
				if v, ok := f["hook_trigger"]; ok {
					fmt.Printf("Hook When:   %v\n", v)
				}
				if v, ok := f["hook_timeout"]; ok {
					fmt.Printf("Hook Timeout: %v\n", v)
				}
				if v, ok := f["hook_on_failure"]; ok {
					fmt.Printf("Hook Fail:   %v\n", v)
				}
				if v, ok := f["subscriptions"]; ok {
					fmt.Printf("Subscribe:   %v\n", v)
				}
				if v, ok := f["subscriptions_exclude"]; ok {
					fmt.Printf("Exclude:     %v\n", v)
				}
			}
		}

		fmt.Printf("Created By:  %s\n", bead.CreatedBy)
		if !bead.CreatedAt.IsZero() {
			fmt.Printf("Created At:  %s\n", bead.CreatedAt.Format("2006-01-02 15:04:05"))
		}

		return nil
	},
}
