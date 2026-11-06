package main

import (
	"context"
	"fmt"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

func newGCCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Garbage-collect old ephemeral wisps",
		Long: `Clean up ephemeral wisps (molecules with ephemeral=true) that are
older than a given age. By default, only closed wisps are collected.

Examples:
  kd mol gc --age 1h             # burn closed wisps older than 1 hour
  kd mol gc --age 24h            # burn closed wisps older than 24 hours
  kd mol gc --age 7d             # burn closed wisps older than 7 days
  kd mol gc --age 1h --dry-run   # preview what would be cleaned
  kd mol gc --age 1h --all       # include open/in_progress wisps too

Duration supports: s (seconds), m (minutes), h (hours), d (days).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ageStr, _ := cmd.Flags().GetString("age")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			all, _ := cmd.Flags().GetBool("all")
			ctx := context.Background()

			if ageStr == "" {
				return fmt.Errorf("--age is required (e.g., --age 1h, --age 24h, --age 7d)")
			}

			age, err := parseDuration(ageStr)
			if err != nil {
				return fmt.Errorf("invalid --age %q: %w", ageStr, err)
			}

			cutoff := time.Now().Add(-age)

			// List molecules. Default: closed only. With --all: all statuses.
			statuses := []string{"closed"}
			if all {
				statuses = []string{"open", "in_progress", "closed", "deferred"}
			}

			resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
				Type:   []string{"molecule"},
				Status: statuses,
				FieldFilters: map[string]string{
					"ephemeral": "true",
				},
			})
			if err != nil {
				return fmt.Errorf("listing wisps: %w", err)
			}

			// Filter by age.
			var candidates []*struct {
				ID    string
				Title string
			}
			for _, b := range resp.Beads {
				if b.CreatedAt.Before(cutoff) {
					candidates = append(candidates, &struct {
						ID    string
						Title string
					}{ID: b.ID, Title: b.Title})
				}
			}

			if len(candidates) == 0 {
				fmt.Printf("No wisps older than %s found\n", ageStr)
				return nil
			}

			if dryRun {
				fmt.Printf("Would GC %d wisp(s) older than %s:\n", len(candidates), ageStr)
				for _, c := range candidates {
					fmt.Printf("  %s: %s\n", c.ID, c.Title)
				}
				return nil
			}

			totalChildren := 0
			for _, c := range candidates {
				// Get child beads.
				revDeps, err := beadsClient.GetReverseDependencies(ctx, c.ID)
				if err != nil {
					fmt.Printf("  warning: failed to get children of %s: %v\n", c.ID, err)
					continue
				}
				var childIDs []string
				for _, dep := range revDeps {
					if string(dep.Type) == "parent-child" {
						childIDs = append(childIDs, dep.BeadID)
					}
				}

				// Delete children first, then the molecule.
				for _, cid := range childIDs {
					if err := beadsClient.DeleteBead(ctx, cid); err != nil {
						fmt.Printf("  warning: failed to delete child %s: %v\n", cid, err)
					}
				}
				if err := beadsClient.DeleteBead(ctx, c.ID); err != nil {
					fmt.Printf("  warning: failed to delete wisp %s: %v\n", c.ID, err)
				}
				totalChildren += len(childIDs)
			}

			fmt.Printf("GC'd %d wisp(s) (%d child beads)\n", len(candidates), totalChildren)
			return nil
		},
	}
	cmd.Flags().String("age", "", "minimum age of wisps to collect (e.g., 1h, 24h, 7d)")
	cmd.Flags().Bool("dry-run", false, "preview what would be cleaned without deleting")
	cmd.Flags().Bool("all", false, "include open/in_progress wisps (default: closed only)")
	return cmd
}

// parseDuration parses a duration string with support for 'd' (days).
func parseDuration(s string) (time.Duration, error) {
	// Handle days suffix which time.ParseDuration doesn't support.
	if len(s) > 1 && s[len(s)-1] == 'd' {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err == nil {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}

	// Also handle inline JSON field filtering response format.
	// Try to parse as a fields JSON blob that might contain ephemeral.
	return time.ParseDuration(s)
}
