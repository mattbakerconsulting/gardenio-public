package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var treeCmd = &cobra.Command{
	Use:     "tree <bead-id>",
	Short:   "Show dependency tree (or flat list) for a bead",
	GroupID: "views",
	Args:    cobra.ExactArgs(1),
	RunE:    runTreeCmd,
}

// runTreeCmd is the shared RunE for both the top-level `kd tree` and the
// `kd dep tree` pass-through (dep.go). Keeping it a named function — rather
// than an inline closure — lets both command paths resolve to the exact same
// logic, so the dep-group alias can never drift from the canonical command.
func runTreeCmd(cmd *cobra.Command, args []string) error {
	beadID := args[0]
	depth, _ := cmd.Flags().GetInt("depth")
	flat, _ := cmd.Flags().GetBool("flat")
	filterType, _ := cmd.Flags().GetString("type")

	if flat {
		return runTreeFlat(beadID, filterType)
	}
	return runTreeGraph(beadID, depth, filterType)
}

// addTreeFlags registers the `tree` flag set on a command. Shared so the
// top-level `kd tree` and the `kd dep tree` pass-through expose an identical
// flag contract.
func addTreeFlags(cmd *cobra.Command) {
	cmd.Flags().Int("depth", 3, "maximum depth to traverse")
	cmd.Flags().Bool("flat", false, "flat table instead of ASCII tree")
	cmd.Flags().StringP("type", "t", "", "filter by dependency type (e.g. parent-child, blocks)")
}

func runTreeGraph(beadID string, depth int, filterType string) error {
	bead, err := beadsClient.GetBead(context.Background(), beadID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%s [%s] %s\n", bead.ID, string(bead.Status), bead.Title)

	deps := bead.Dependencies
	if filterType != "" {
		deps = filterDepsByType(deps, []string{filterType})
	}
	printDepTree(deps, "", depth-1)
	return nil
}

func runTreeFlat(beadID string, filterType string) error {
	var types []string
	if filterType != "" {
		types = []string{filterType}
	}

	deps, err := fetchAndResolveDeps(context.Background(), beadsClient, beadID, types)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if len(deps) == 0 {
		fmt.Println("No dependencies found.")
		return nil
	}

	if jsonOutput {
		type jsonChild struct {
			DependsOnID string `json:"depends_on_id"`
			Type        string `json:"type"`
			Status      string `json:"status,omitempty"`
			Title       string `json:"title,omitempty"`
		}
		var out []jsonChild
		for _, rd := range deps {
			jc := jsonChild{
				DependsOnID: rd.Dep.DependsOnID,
				Type:        string(rd.Dep.Type),
			}
			if rd.Bead != nil {
				jc.Status = string(rd.Bead.Status)
				jc.Title = rd.Bead.Title
			}
			out = append(out, jc)
		}
		emitJSON(out)
	} else {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "DEPENDS_ON\tTYPE\tSTATUS\tTITLE")
		for _, rd := range deps {
			status := "(unknown)"
			title := "(error fetching)"
			if rd.Bead != nil {
				status = string(rd.Bead.Status)
				title = rd.Bead.Title
				if len(title) > 50 {
					title = title[:47] + "..."
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				rd.Dep.DependsOnID,
				string(rd.Dep.Type),
				status,
				title,
			)
		}
		w.Flush()
	}
	return nil
}

func printDepTree(deps []*model.Dependency, prefix string, remainingDepth int) {
	for i, dep := range deps {
		isLast := i == len(deps)-1

		connector := "├── "
		childPrefix := prefix + "│   "
		if isLast {
			connector = "└── "
			childPrefix = prefix + "    "
		}

		depBead, err := beadsClient.GetBead(context.Background(), dep.DependsOnID)
		if err != nil {
			fmt.Printf("%s%s%s: %s (error fetching)\n", prefix, connector, string(dep.Type), dep.DependsOnID)
			continue
		}

		fmt.Printf("%s%s%s: %s [%s] %s\n",
			prefix, connector,
			string(dep.Type),
			depBead.ID,
			string(depBead.Status),
			depBead.Title,
		)

		if remainingDepth > 0 {
			childDeps := depBead.Dependencies
			if len(childDeps) > 0 {
				printDepTree(childDeps, childPrefix, remainingDepth-1)
			}
		}
	}
}

func init() {
	addTreeFlags(treeCmd)
}
