package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var taintDescribeCmd = &cobra.Command{
	Use:   "describe <name>",
	Short: "Show one taint definition by name",
	Long: `Show one taint by name. The owner team or an admin sees the full row,
including scrub_policy; anyone else sees name and description only, never
the policy or grant detail (ADR docs/adr/taints.md §5, §9.1).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		ctx := context.Background()

		t, err := beadsClient.GetTaint(ctx, name)
		if err != nil {
			return fmt.Errorf("getting taint: %w", err)
		}

		if jsonOutput {
			emitJSON(t)
			return nil
		}

		fmt.Printf("Name:        %s\n", t.Name)
		fmt.Printf("Description: %s\n", t.Description)
		if t.OwnerTeam == "" && len(t.ScrubPolicy) == 0 {
			// Restricted view: neither the owner team nor an admin.
			return nil
		}
		fmt.Printf("Owner team:  %s\n", t.OwnerTeam)
		fmt.Printf("Effects:     {%s}\n", strings.Join(t.Effects, ", "))
		if t.RetiredAt != nil {
			fmt.Printf("Retired at:  %s\n", t.RetiredAt.Format("2006-01-02T15:04:05Z07:00"))
		}
		if len(t.ScrubPolicy) > 0 {
			fmt.Printf("Scrub policy:\n%s\n", string(t.ScrubPolicy))
		}
		return nil
	},
}
