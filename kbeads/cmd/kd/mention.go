package main

import (
	"github.com/spf13/cobra"
)

// mentionCmd is a top-level alias for `kd refs mentions <id>`. Operators ask
// "what does this bead mention?" naturally, and the Slack ask phrasing in the
// E3 plan (gardenio/plans/typed-mentions-graph-edges-and-cli.md) reaches for
// `kd … mentions` rather than `kd refs mentions`. Keep `kd refs` available
// for the precise crowd; surface this alias so the natural phrasing works.
var mentionCmd = &cobra.Command{
	Use:     "mention <bead-id>",
	Short:   "List the ka-IDs that <bead-id>'s text references (alias for `kd refs mentions`)",
	Long:    refsMentionsCmd.Long,
	GroupID: "beads",
	Args:    cobra.ExactArgs(1),
	RunE:    refsMentionsCmd.RunE,
}

func init() {
	// Mirror the same flags the underlying refs subcommand exposes so the
	// alias is a true drop-in. Keep this block in sync with refs.go's init().
	mentionCmd.Flags().Int("limit", 50, "page size (max 500)")
	mentionCmd.Flags().Int("offset", 0, "number of rows to skip")
	mentionCmd.Flags().String("type", "", "filter by typed-mention relationship verb (e.g. parent, child, blocks). Use 'mentions' or '' for untyped only.")
}
