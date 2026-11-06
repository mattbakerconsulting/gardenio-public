package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var molListCmd = &cobra.Command{
	Use:   "list",
	Short: "List molecules",
	Long: `List molecules (instantiated formulas).

Examples:
  kd mol list
  kd mol list --label project:gardenio
  kd mol list --status closed`,
	RunE: func(cmd *cobra.Command, args []string) error {
		labels, _ := cmd.Flags().GetStringSlice("label")
		statuses, _ := cmd.Flags().GetStringSlice("status")

		if len(statuses) == 0 {
			statuses = []string{"open", "in_progress"}
		}

		req := &client.ListBeadsRequest{
			Type:   []string{"molecule"},
			Status: statuses,
			Labels: labels,
			Limit:  100,
		}

		resp, err := beadsClient.ListBeads(context.Background(), req)
		if err != nil {
			return fmt.Errorf("listing molecules: %w", err)
		}

		if jsonOutput {
			printBeadListJSONWithTotal(resp.Beads, resp.Total, jsonOutput)
		} else {
			printMolList(resp.Beads, resp.Total)
		}
		return nil
	},
}

func printMolList(beads []*model.Bead, total int) {
	if len(beads) == 0 {
		fmt.Println("No molecules found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tTITLE\tFORMULA\tASSIGNEE")
	for _, b := range beads {
		title := b.Title
		if len(title) > 40 {
			title = title[:37] + "..."
		}

		formulaID := molFormulaID(b)

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			b.ID, b.Status, title, formulaID, b.Assignee)
	}
	w.Flush()
	fmt.Printf("\n%d molecules (%d total)\n", len(beads), total)
}

// molFormulaID extracts the formula_id from a molecule's fields.
func molFormulaID(b *model.Bead) string {
	if len(b.Fields) == 0 {
		return ""
	}
	var f struct {
		FormulaID string `json:"formula_id"`
	}
	if json.Unmarshal(b.Fields, &f) == nil {
		return f.FormulaID
	}
	return ""
}

func init() {
	molListCmd.Flags().StringSliceP("label", "l", nil, "filter by label (repeatable)")
	molListCmd.Flags().StringSliceP("status", "s", nil, "filter by status (default: open, in_progress)")
	// Accept --labels as an alias of --label (ka-0000000987).
	aliasLabelFlags(molListCmd)
}
