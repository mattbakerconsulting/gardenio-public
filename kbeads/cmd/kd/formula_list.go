package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var formulaListCmd = &cobra.Command{
	Use:   "list",
	Short: "List formulas",
	Long: `List available formulas.

By default, filters to the current project (from $KD_PROJECT or $BOAT_PROJECT).
Use --all-projects to see formulas across all projects.

Examples:
  kd formula list
  kd formula list -l role:crew
  kd formula list --all-projects
  kd formula list --json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		labels, _ := cmd.Flags().GetStringSlice("label")
		project, _ := cmd.Flags().GetString("project")
		allProjects, _ := cmd.Flags().GetBool("all-projects")

		if !allProjects && project != "" {
			labels = append(labels, "project:"+project)
		}

		req := &client.ListBeadsRequest{
			Type:   []string{"formula"},
			Status: []string{"open", "in_progress"},
			Labels: labels,
			Limit:  100,
		}

		resp, err := beadsClient.ListBeads(context.Background(), req)
		if err != nil {
			return fmt.Errorf("listing formulas: %w", err)
		}

		if jsonOutput {
			printBeadListJSONWithTotal(resp.Beads, resp.Total, jsonOutput)
		} else {
			printFormulaList(resp.Beads, resp.Total)
		}
		return nil
	},
}

func printFormulaList(beads []*model.Bead, total int) {
	if len(beads) == 0 {
		fmt.Println("No formulas found.")
		return
	}

	// Sort by title then by creation date (newest last) so duplicates are grouped.
	sort.Slice(beads, func(i, j int) bool {
		if beads[i].Title != beads[j].Title {
			return beads[i].Title < beads[j].Title
		}
		return beads[i].CreatedAt.Before(beads[j].CreatedAt)
	})

	// Track duplicate titles.
	titleCount := map[string]int{}
	for _, b := range beads {
		titleCount[b.Title]++
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tSTEPS\tVARS\tAGENT\tCREATED\tLABELS")
	for _, b := range beads {
		title := b.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}

		steps, vars, agent := formulaFieldSummary(b)
		lblStr := strings.Join(b.Labels, ", ")
		created := b.CreatedAt.Format("2006-01-02")

		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\t%s\n", b.ID, title, steps, vars, agent, created, lblStr)
	}
	w.Flush()
	fmt.Printf("\n%d formulas (%d total)\n", len(beads), total)

	// Warn about duplicate titles.
	var dups []string
	for title, count := range titleCount {
		if count > 1 {
			dups = append(dups, fmt.Sprintf("  %q (%d copies)", title, count))
		}
	}
	if len(dups) > 0 {
		sort.Strings(dups)
		fmt.Fprintf(os.Stderr, "\nWarning: duplicate formula titles found:\n%s\n", strings.Join(dups, "\n"))
		fmt.Fprintln(os.Stderr, "Consider closing older versions with: kd close <id>")
	}
}

// formulaFieldSummary extracts step count, var count, and assigned agent from a formula bead's fields.
func formulaFieldSummary(b *model.Bead) (steps int, vars int, agent string) {
	if len(b.Fields) == 0 {
		return 0, 0, ""
	}
	var f struct {
		Steps         []json.RawMessage `json:"steps"`
		Vars          []json.RawMessage `json:"vars"`
		AssignedAgent string            `json:"assigned_agent"`
	}
	if json.Unmarshal(b.Fields, &f) == nil {
		steps = len(f.Steps)
		vars = len(f.Vars)
		agent = f.AssignedAgent
	}
	return
}

func init() {
	formulaListCmd.Flags().StringSliceP("label", "l", nil, "filter by label (repeatable)")
	formulaListCmd.Flags().String("project", defaultProject(), "filter by project label (default: $KD_PROJECT or $BOAT_PROJECT)")
	formulaListCmd.Flags().Bool("all-projects", false, "show formulas from all projects (disables project filter)")
	// Accept --labels as an alias of --label (ka-0000000987).
	aliasLabelFlags(formulaListCmd)
}
