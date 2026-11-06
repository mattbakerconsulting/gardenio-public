package main

import (
	"context"
	"encoding/json"
	"fmt"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:     "status",
	Short:   "Show database overview and bead counts by status",
	GroupID: "system",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		statuses := []string{"open", "in_progress", "blocked", "deferred", "partial", "closed"}
		counts := make(map[string]int, len(statuses))
		var total int

		for _, s := range statuses {
			resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
				Status: []string{s},
				Limit:  0,
			})
			if err != nil {
				return fmt.Errorf("querying %s beads: %w", s, err)
			}
			counts[s] = resp.Total
			total += resp.Total
		}

		if jsonOutput {
			out := map[string]int{
				"open":        counts["open"],
				"in_progress": counts["in_progress"],
				"blocked":     counts["blocked"],
				"deferred":    counts["deferred"],
				"partial":     counts["partial"],
				"closed":      counts["closed"],
				"total":       total,
			}
			data, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling JSON: %w", err)
			}
			fmt.Println(string(data))
		} else {
			fmt.Println("Beads Status")
			fmt.Printf("  Open:        %d\n", counts["open"])
			fmt.Printf("  In Progress: %d\n", counts["in_progress"])
			fmt.Printf("  Blocked:     %d\n", counts["blocked"])
			fmt.Printf("  Deferred:    %d\n", counts["deferred"])
			fmt.Printf("  Partial:     %d\n", counts["partial"])
			fmt.Printf("  Closed:      %d\n", counts["closed"])
			fmt.Printf("  Total:       %d\n", total)
		}
		return nil
	},
}
