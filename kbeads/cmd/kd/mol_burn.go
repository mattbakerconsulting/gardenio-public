package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newBurnCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "burn <molecule-id>",
		Short: "Delete a molecule and its children without a digest",
		Long: `Burn a molecule, deleting it and all child beads with no trace.

Unlike squash (which creates a summary before closing), burn completely
removes the molecule and its children. Use this for:
  - Abandoned or failed workflows
  - Test/debug molecules you don't want to preserve

Examples:
  kd mol burn ka-abc123
  kd mol burn ka-abc123 --dry-run
  kd mol burn ka-abc123 --force`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			force, _ := cmd.Flags().GetBool("force")
			ctx := context.Background()

			bead, err := beadsClient.GetBead(ctx, id)
			if err != nil {
				return fmt.Errorf("getting molecule: %w", err)
			}
			if string(bead.Type) != "molecule" {
				return fmt.Errorf("bead %s is type %q, not molecule", id, bead.Type)
			}

			// Get child beads.
			revDeps, err := beadsClient.GetReverseDependencies(ctx, id)
			if err != nil {
				return fmt.Errorf("getting children: %w", err)
			}
			var childIDs []string
			for _, dep := range revDeps {
				if string(dep.Type) == "parent-child" {
					childIDs = append(childIDs, dep.BeadID)
				}
			}

			if dryRun {
				fmt.Printf("Would burn molecule %s: %s\n", id, bead.Title)
				fmt.Printf("  Children to delete: %d\n", len(childIDs))
				for _, cid := range childIDs {
					fmt.Printf("    %s\n", cid)
				}
				return nil
			}

			if !force && len(childIDs) > 0 {
				fmt.Printf("About to delete molecule %s and %d children. Use --force to confirm.\n", id, len(childIDs))
				return nil
			}

			// Delete children first, then the molecule.
			for _, cid := range childIDs {
				if err := beadsClient.DeleteBead(ctx, cid); err != nil {
					fmt.Printf("  warning: failed to delete child %s: %v\n", cid, err)
				}
			}
			if err := beadsClient.DeleteBead(ctx, id); err != nil {
				return fmt.Errorf("deleting molecule: %w", err)
			}

			fmt.Printf("Burned molecule %s (%d children deleted)\n", id, len(childIDs))
			return nil
		},
	}
	cmd.Flags().Bool("dry-run", false, "preview what would be deleted")
	cmd.Flags().Bool("force", false, "skip confirmation for destructive delete")
	return cmd
}
