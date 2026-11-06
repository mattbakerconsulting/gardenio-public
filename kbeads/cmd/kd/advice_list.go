package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var adviceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List advice beads",
	Long: `List advice beads with optional label filtering.

Use --for to auto-match advice for an agent context (matches global + project + role + agent labels).

Examples:
  kd advice list
  kd advice list --label project:beads
  kd advice list --for agent:arch-eel`,
	RunE: func(cmd *cobra.Command, args []string) error {
		labels, _ := cmd.Flags().GetStringSlice("label")
		forAgent, _ := cmd.Flags().GetString("for")

		if forAgent != "" {
			labels = append(labels, forAgent)
		}

		req := &client.ListBeadsRequest{
			Type:   []string{"advice"},
			Labels: labels,
			Status: []string{"open", "in_progress"},
			Limit:  100,
		}

		resp, err := beadsClient.ListBeads(context.Background(), req)
		if err != nil {
			return fmt.Errorf("listing advice: %w", err)
		}

		if jsonOutput {
			printBeadListJSONWithTotal(resp.Beads, resp.Total, jsonOutput)
		} else {
			printAdviceList(resp.Beads, resp.Total)
		}
		return nil
	},
}

func printAdviceList(beads []*model.Bead, total int) {
	if len(beads) == 0 {
		fmt.Println("No advice beads found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPRI\tTITLE\tLABELS\tHOOK")
	for _, b := range beads {
		title := b.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}

		lbls := strings.Join(b.Labels, ", ")
		hook := ""
		if len(b.Fields) > 0 {
			var f map[string]any
			if json.Unmarshal(b.Fields, &f) == nil {
				if cmd, ok := f["hook_trigger"].(string); ok && cmd != "" {
					hook = cmd
				}
			}
		}

		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", b.ID, b.Priority, title, lbls, hook)
	}
	w.Flush()
	fmt.Printf("\n%d advice beads (%d total)\n", len(beads), total)
}

func init() {
	adviceListCmd.Flags().StringSliceP("label", "l", nil, "filter by label (repeatable)")
	adviceListCmd.Flags().String("for", "", "match advice for agent context (e.g. agent:arch-eel)")
	// Accept --labels as an alias of --label (ka-0000000987).
	aliasLabelFlags(adviceListCmd)
}
