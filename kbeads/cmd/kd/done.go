package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var doneCmd = &cobra.Command{
	Use:     "done <id>...",
	Short:   "Mark beads as done and close them",
	GroupID: "workflow",
	Args:    cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		comment, _ := cmd.Flags().GetString("comment")

		for _, id := range args {
			if comment != "" {
				_, err := beadsClient.AddComment(context.Background(), id, actor, comment)
				if err != nil {
					return fmt.Errorf("adding comment to %s: %w", id, err)
				}
			}

			bead, err := beadsClient.CloseBead(context.Background(), id, actor)
			if err != nil {
				return fmt.Errorf("closing %s: %w", id, err)
			}

			if jsonOutput {
				printBeadJSON(bead)
			} else {
				// Terse single-line success for every done (ka-0000005647),
				// mirroring `kd close`: the single-id path no longer dumps the
				// full bead table, so agents stop reflexively masking output
				// with `2>/dev/null`/`tail`. Use --json for the structured form.
				fmt.Printf("Done %s\n", bead.ID)
			}
		}
		return nil
	},
}

// aliasDoneReasonFlags normalizes `--reason` → `--comment` on `kd done` so the
// spelling agents learn on `kd close` (--reason, ka-0000007746) also works here:
// `kd done` and `kd close` both add a comment before closing, so the affordance
// is now spelled the same across both. `--comment`/`-m` remain the canonical
// form. Attached PER COMMAND, never global. Mirrors aliasCloseReasonFlags
// (close.go), which folds the opposite direction (--comment → --reason).
func aliasDoneReasonFlags(cmd *cobra.Command) {
	cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == "reason" {
			name = "comment"
		}
		return pflag.NormalizedName(name)
	})
}

func init() {
	doneCmd.Flags().StringP("comment", "m", "", "completion comment to add before closing (alias: --reason)")
	aliasDoneReasonFlags(doneCmd)
}
