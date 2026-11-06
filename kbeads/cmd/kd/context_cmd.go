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

// contextConfig is the client-side interpretation of a context:{name} config value.
type contextConfig struct {
	Sections []contextSection `json:"sections"`
}

type contextSection struct {
	Header string   `json:"header"`
	View   string   `json:"view"`
	Format string   `json:"format"` // "table" (default), "list", "count", "detail", "tree"
	Fields []string `json:"fields"` // for "list" and "detail" formats
	Depth  int      `json:"depth"`  // for "tree" format; default 3
}

var contextCmd = &cobra.Command{
	Use:     "context <name>",
	Short:   "Compose and render a context template",
	GroupID: "views",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		// 1. Fetch the context config.
		ctxCfg, err := resolveConfigBead(context.Background(), "context:"+name)
		if err != nil {
			return fmt.Errorf("getting context config %q: %w", name, err)
		}

		var cc contextConfig
		if err := json.Unmarshal(ctxCfg.Value, &cc); err != nil {
			return fmt.Errorf("parsing context config %q: %w", name, err)
		}

		// 2. Render each section.
		for i, section := range cc.Sections {
			if i > 0 {
				fmt.Println()
			}
			if section.Header != "" {
				fmt.Println(section.Header)
				fmt.Println()
			}

			// Resolve the named view.
			viewCfg, err := resolveConfigBead(context.Background(), "view:"+section.View)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error loading view %q: %v\n", section.View, err)
				continue
			}

			var vc viewConfig
			if err := json.Unmarshal(viewCfg.Value, &vc); err != nil {
				fmt.Fprintf(os.Stderr, "Error parsing view %q: %v\n", section.View, err)
				continue
			}

			// Build and execute the query.
			req := &client.ListBeadsRequest{
				Status:   vc.Filter.Status,
				Type:     vc.Filter.Type,
				Kind:     vc.Filter.Kind,
				Labels:   vc.Filter.Labels,
				Assignee: expandVar(vc.Filter.Assignee),
				Search:   vc.Filter.Search,
				Sort:     vc.Sort,
				Limit:    vc.Limit,
				Priority: vc.Filter.Priority,
			}

			resp, err := beadsClient.ListBeads(context.Background(), req)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error querying view %q: %v\n", section.View, err)
				continue
			}

			// Format output.
			switch section.Format {
			case "count":
				fmt.Printf("%d beads\n", resp.Total)
			case "list":
				printSectionList(resp.Beads, section.Fields)
			case "detail":
				printSectionDetail(resp.Beads, section.Fields, vc.Deps)
			case "tree":
				depth := section.Depth
				if depth <= 0 {
					depth = 3
				}
				printSectionTree(resp.Beads, depth, vc.Deps)
			default: // "table" or empty
				if len(vc.Columns) > 0 {
					printBeadListColumns(resp.Beads, resp.Total, vc.Columns)
				} else {
					printSectionTable(resp.Beads)
				}
				if vc.Deps != nil && len(resp.Beads) > 0 {
					printViewDeps(resp.Beads, vc.Deps)
				}
			}
		}
		return nil
	},
}

// printSectionList prints beads as bullet points with selected fields.
func printSectionList(beads []*model.Bead, fields []string) {
	if len(fields) == 0 {
		fields = []string{"id", "title", "status"}
	}
	for _, b := range beads {
		parts := make([]string, len(fields))
		for i, f := range fields {
			parts[i] = beadField(b, f)
		}
		fmt.Printf("- %s\n", strings.Join(parts, " | "))
	}
}

// printSectionTable prints beads as a compact table (no total footer).
func printSectionTable(beads []*model.Bead) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tTYPE\tPRIORITY\tTITLE\tASSIGNEE")
	for _, b := range beads {
		title := b.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			b.ID,
			b.Status,
			b.Type,
			b.Priority,
			title,
			b.Assignee,
		)
	}
	w.Flush()
}

// printSectionDetail prints each bead with full show-style output.
func printSectionDetail(beads []*model.Bead, fields []string, dc *depConfig) {
	for i, b := range beads {
		if i > 0 {
			fmt.Println("---")
		}
		// Fetch full bead (with deps + comments).
		fullBead, err := beadsClient.GetBead(context.Background(), b.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error fetching %s: %v\n", b.ID, err)
			continue
		}
		printBeadTableFiltered(fullBead, fields)
		printComments(fullBead.Comments)
		if dc != nil {
			resolved := resolveBeadDeps(context.Background(), beadsClient, fullBead.Dependencies, dc.Types)
			if len(resolved) > 0 {
				fmt.Println()
				fmt.Println("  Dependencies:")
				printDepSubSection(resolved, dc.Fields)
			}
		}
	}
}

// printSectionTree prints each bead with an ASCII dependency tree.
func printSectionTree(beads []*model.Bead, depth int, dc *depConfig) {
	var depTypes []string
	if dc != nil {
		depTypes = dc.Types
	}
	for i, b := range beads {
		if i > 0 {
			fmt.Println()
		}
		// Fetch full bead for embedded deps.
		fullBead, err := beadsClient.GetBead(context.Background(), b.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error fetching %s: %v\n", b.ID, err)
			continue
		}
		fmt.Printf("%s [%s] %s\n", fullBead.ID, string(fullBead.Status), fullBead.Title)
		deps := fullBead.Dependencies
		if len(depTypes) > 0 {
			deps = filterDepsByType(deps, depTypes)
		}
		printDepTree(deps, "", depth-1)
	}
}

// filterDepsByType returns only dependencies whose type is in the given set.
func filterDepsByType(deps []*model.Dependency, types []string) []*model.Dependency {
	typeSet := make(map[string]bool, len(types))
	for _, t := range types {
		typeSet[t] = true
	}
	var filtered []*model.Dependency
	for _, d := range deps {
		if typeSet[string(d.Type)] {
			filtered = append(filtered, d)
		}
	}
	return filtered
}
