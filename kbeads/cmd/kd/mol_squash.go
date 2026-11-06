package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newSquashCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "squash <molecule-id>",
		Short: "Condense molecule into a digest and close children",
		Long: `Squash a molecule's children into a summary digest.

This command:
  1. Collects all child beads of the molecule
  2. Creates a summary comment on the molecule
  3. Closes all child beads
  4. Optionally closes the molecule itself

Use --summary to provide a description of what was accomplished.
Without --summary, child titles are concatenated as the digest.

Examples:
  kd mol squash ka-abc123
  kd mol squash ka-abc123 --summary "Completed auth refactor"
  kd mol squash ka-abc123 --close-mol --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			summary, _ := cmd.Flags().GetString("summary")
			closeMol, _ := cmd.Flags().GetBool("close-mol")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
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

			var children []struct{ id, title, status string }
			for _, dep := range revDeps {
				if string(dep.Type) != "parent-child" {
					continue
				}
				child, err := beadsClient.GetBead(ctx, dep.BeadID)
				if err != nil {
					continue
				}
				children = append(children, struct{ id, title, status string }{
					child.ID, child.Title, string(child.Status),
				})
			}

			// Build digest text.
			digest := summary
			if digest == "" {
				var titles []string
				for _, c := range children {
					titles = append(titles, fmt.Sprintf("- [%s] %s", c.status, c.title))
				}
				digest = fmt.Sprintf("Squash digest for %s:\n%s", bead.Title, strings.Join(titles, "\n"))
			}

			if dryRun {
				fmt.Printf("Would squash molecule %s: %s\n", id, bead.Title)
				fmt.Printf("  Children: %d\n", len(children))
				fmt.Printf("  Digest:\n%s\n", digest)
				if closeMol {
					fmt.Println("  Would also close the molecule")
				}
				return nil
			}

			// Add digest as comment on the molecule.
			_, err = beadsClient.AddComment(ctx, id, actor, digest)
			if err != nil {
				return fmt.Errorf("adding digest comment: %w", err)
			}

			// Close all open children.
			closed := 0
			for _, c := range children {
				if c.status == "closed" {
					continue
				}
				if _, err := beadsClient.CloseBead(ctx, c.id, actor); err != nil {
					fmt.Printf("  warning: failed to close %s: %v\n", c.id, err)
				} else {
					closed++
				}
			}

			// Optionally close the molecule.
			if closeMol {
				if _, err := beadsClient.CloseBead(ctx, id, actor); err != nil {
					return fmt.Errorf("closing molecule: %w", err)
				}
			}

			fmt.Printf("Squashed molecule %s: %d children closed\n", id, closed)
			if closeMol {
				fmt.Println("  Molecule closed")
			}
			return nil
		},
	}
	cmd.Flags().String("summary", "", "summary text for the digest (default: auto-generated from children)")
	cmd.Flags().Bool("close-mol", false, "also close the molecule itself")
	cmd.Flags().Bool("dry-run", false, "preview what would be squashed")
	return cmd
}
