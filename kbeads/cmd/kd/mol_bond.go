package main

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

func newBondCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bond <molecule-a> <molecule-b>",
		Short: "Link two molecules with a dependency",
		Long: `Bond two molecules together by creating a dependency between them.

Bond types:
  sequential (default) — B runs after A completes
  parallel             — B runs alongside A (related link)
  conditional          — B activates only if A fails

Examples:
  kd mol bond ka-abc ka-def                      # B blocked by A (sequential)
  kd mol bond ka-abc ka-def --type parallel      # Related link
  kd mol bond ka-abc ka-def --type conditional   # B runs only if A fails`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			idA, idB := args[0], args[1]
			bondType, _ := cmd.Flags().GetString("type")
			ctx := context.Background()

			// Resolve bond type to dependency type.
			depType := "blocks" // sequential default
			switch bondType {
			case "sequential":
				depType = "blocks"
			case "parallel":
				depType = "related"
			case "conditional":
				depType = "conditional"
			default:
				if bondType != "" {
					return fmt.Errorf("unknown bond type %q (use sequential, parallel, or conditional)", bondType)
				}
			}

			_, err := beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
				BeadID:      idB,
				DependsOnID: idA,
				Type:        depType,
				CreatedBy:   actor,
			})
			if err != nil {
				return fmt.Errorf("bonding molecules: %w", err)
			}

			fmt.Printf("Bonded %s → %s (%s)\n", idA, idB, depType)
			return nil
		},
	}
	cmd.Flags().String("type", "sequential", "bond type: sequential (blocks), parallel (related), or conditional (on-failure)")
	return cmd
}
