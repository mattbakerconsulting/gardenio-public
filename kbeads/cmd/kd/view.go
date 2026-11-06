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

// depConfig controls optional dependency sub-sections displayed below beads.
type depConfig struct {
	Types  []string `json:"types,omitempty"`  // dep types to include; empty = all
	Fields []string `json:"fields,omitempty"` // fields of resolved target bead; default: id,title,status
}

// viewConfig is the client-side interpretation of a view:{name} config value.
type viewConfig struct {
	Filter  viewFilter `json:"filter"`
	Sort    string     `json:"sort"`
	Columns []string   `json:"columns"`
	Limit   int        `json:"limit"`
	Deps    *depConfig `json:"deps,omitempty"`
}

type viewFilter struct {
	Status   []string          `json:"status"`
	Type     []string          `json:"type"`
	Kind     []string          `json:"kind"`
	Labels   []string          `json:"labels"`
	Assignee string            `json:"assignee"`
	Search   string            `json:"search"`
	Priority *int              `json:"priority"`
	Fields   map[string]string `json:"fields,omitempty"`
}

var viewCmd = &cobra.Command{
	Use:     "view <name>",
	Short:   "Run a saved view (named query)",
	GroupID: "views",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		limitOverride, _ := cmd.Flags().GetInt("limit")

		// 1. Fetch the view config.
		cfg, err := resolveConfigBead(context.Background(), "view:"+name)
		if err != nil {
			return fmt.Errorf("getting view config %q: %w", name, err)
		}

		var vc viewConfig
		if err := json.Unmarshal(cfg.Value, &vc); err != nil {
			return fmt.Errorf("parsing view config %q: %w", name, err)
		}

		// 2. Build the ListBeads request.
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
		if len(vc.Filter.Fields) > 0 {
			req.FieldFilters = vc.Filter.Fields
		}
		if limitOverride > 0 {
			req.Limit = limitOverride
		}

		// 3. Call ListBeads.
		resp, err := beadsClient.ListBeads(context.Background(), req)
		if err != nil {
			return fmt.Errorf("querying view %q: %w", name, err)
		}

		// 4. Display results.
		if jsonOutput {
			printBeadListJSONWithTotal(resp.Beads, resp.Total, jsonOutput)
		} else if len(vc.Columns) > 0 {
			printBeadListColumns(resp.Beads, resp.Total, vc.Columns)
		} else {
			printBeadListTable(resp.Beads, resp.Total)
		}

		// 5. Optional dependency sub-sections.
		if !jsonOutput && vc.Deps != nil && len(resp.Beads) > 0 {
			printViewDeps(resp.Beads, vc.Deps)
		}
		return nil
	},
}

// expandVar replaces well-known variables in filter values.
func expandVar(s string) string {
	s = strings.ReplaceAll(s, "$BEADS_ACTOR", actor)
	return s
}

// printBeadListColumns prints beads using a custom set of columns.
func printBeadListColumns(beads []*model.Bead, total int, columns []string, scope ...string) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// Header
	headers := make([]string, len(columns))
	for i, c := range columns {
		headers[i] = strings.ToUpper(c)
	}
	fmt.Fprintln(tw, strings.Join(headers, "\t"))

	// Rows
	for _, b := range beads {
		vals := make([]string, len(columns))
		for i, col := range columns {
			vals[i] = beadField(b, col)
		}
		fmt.Fprintln(tw, strings.Join(vals, "\t"))
	}
	tw.Flush()
	fmt.Printf("\n%s\n", listFooter(len(beads), total, scopeArg(scope)))
}

// beadField returns the string value of a bead field by column name.
func beadField(b *model.Bead, col string) string {
	switch strings.ToLower(col) {
	case "id":
		return b.ID
	case "title":
		title := b.Title
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		return title
	case "status":
		return string(b.Status)
	case "type":
		return string(b.Type)
	case "kind":
		return string(b.Kind)
	case "priority":
		return fmt.Sprintf("%d", b.Priority)
	case "assignee":
		return b.Assignee
	case "owner":
		return b.Owner
	case "created_by":
		return b.CreatedBy
	case "labels":
		return strings.Join(b.Labels, ",")
	default:
		// Check typed fields (e.g., jira_key, jira_url).
		if len(b.Fields) > 0 {
			var fields map[string]string
			if json.Unmarshal(b.Fields, &fields) == nil {
				if v, ok := fields[col]; ok {
					return v
				}
			}
		}
		return ""
	}
}

// printViewDeps prints dependency sub-sections for each bead in the list.
func printViewDeps(beads []*model.Bead, dc *depConfig) {
	fmt.Println()
	for _, b := range beads {
		deps, err := fetchAndResolveDeps(context.Background(), beadsClient, b.ID, dc.Types)
		if err != nil || len(deps) == 0 {
			continue
		}
		fmt.Printf("  %s dependencies:\n", b.ID)
		printDepSubSection(deps, dc.Fields)
		fmt.Println()
	}
}

func init() {
	viewCmd.Flags().Int("limit", 0, "override the view's limit")
}
