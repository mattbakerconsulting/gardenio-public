package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var similarCmd = &cobra.Command{
	Use:   "similar <bead-id>",
	Short: "Find beads similar to the given bead (semantic search)",
	Long: `Find beads similar to the given bead using vector similarity search.

The server looks up the bead's stored embedding and returns the nearest
neighbors by cosine similarity. The source bead is excluded from results.

Requires the server to have an embedding indexed for the bead. If no
embedding exists yet, the server returns 409 Conflict — run embedding
backfill (` + "`kd embed sync`" + `) or wait for async indexing to catch up.

Examples:
  kd similar ka-abc123                       # top 10 neighbors
  kd similar ka-abc123 --limit 20            # top 20
  kd similar ka-abc123 --min-score 0.75      # drop low-confidence matches`,
	GroupID: "beads",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		limit, _ := cmd.Flags().GetInt("limit")
		minScore, _ := cmd.Flags().GetFloat64("min-score")

		resp, err := beadsClient.FindSimilar(context.Background(), id, limit, minScore)
		if err != nil {
			return fmt.Errorf("find similar: %w", err)
		}

		if jsonOutput {
			data, _ := json.MarshalIndent(resp, "", "  ")
			fmt.Println(string(data))
			return nil
		}

		if len(resp.Results) == 0 {
			fmt.Fprintf(os.Stdout, "No similar beads found for %s.\n", id)
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SCORE\tID\tSTATUS\tTYPE\tTITLE")
		for _, r := range resp.Results {
			if r == nil || r.Bead == nil {
				continue
			}
			b := r.Bead
			title := b.Title
			if len(title) > 60 {
				title = title[:57] + "..."
			}
			fmt.Fprintf(w, "%.3f\t%s\t%s\t%s\t%s\n",
				r.Score, b.ID, b.Status, b.Type, title)
		}
		w.Flush()
		fmt.Fprintf(os.Stdout, "\n%d similar beads (source: %s)\n", len(resp.Results), id)
		return nil
	},
}

func init() {
	similarCmd.Flags().Int("limit", 10, "maximum number of neighbors to return (1-100)")
	similarCmd.Flags().Float64("min-score", 0, "drop results with cosine similarity below this threshold (0-1)")
}
