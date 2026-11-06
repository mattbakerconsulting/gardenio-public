package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// refsCmd is the parent for the bead-reference read APIs. Mirrors
// `kd dep` for symmetry. See kbeads/docs/design-bead-references.md § 5a.
var refsCmd = &cobra.Command{
	Use:     "refs",
	Short:   "Inspect bead-id references parsed from bead text",
	Long:    "Read ka-0000001713 references the server has extracted from bead text fields and comments. Use 'mentioned-by' to see who points at a bead, and 'mentions' to see what a bead points at.",
	GroupID: "beads",
}

var refsMentionedByCmd = &cobra.Command{
	Use:   "mentioned-by <bead-id>",
	Short: "List beads whose text references <bead-id>",
	Long:  "Returns one row per (source bead, source field). A bead that references the target in both its description and a comment appears twice with different source_field values. Soft-deleted source beads are filtered out.\n\nUse --type=<verb> to filter by typed-mention relationship (parent/child/blocks/etc.). --type=mentions or --type='' returns only untyped (plain ka-0000001713) references.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRefsList(cmd, args[0], beadsClient.ListBeadsMentioning, "MENTIONED_BY")
	},
}

var refsMentionsCmd = &cobra.Command{
	Use:   "mentions <bead-id>",
	Short: "List the ka-IDs that <bead-id>'s text references",
	Long:  "Returns one row per outgoing reference. Targets that don't (yet) exist as beads — typos, cross-fleet IDs, or not-yet-created beads — are still listed; their title/type/status will be blank.\n\nUse --type=<verb> to filter by typed-mention relationship (parent/child/blocks/etc.). --type=mentions or --type='' returns only untyped (plain ka-0000001713) references.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRefsList(cmd, args[0], beadsClient.ListBeadReferencesFrom, "MENTIONS")
	},
}

// listRefsFn is the shape of both ListBeadsMentioning and ListBeadReferencesFrom
// on BeadsClient — a small alias so the two subcommands share rendering code.
type listRefsFn func(ctx context.Context, beadID string, limit, offset int, relationshipType *string) (*client.ListReferencesResponse, error)

// parseRefsTypeFilter turns the --type flag into the three-state filter the
// store/HTTP layers expect: nil = no filter; non-nil "" = NULL relationship_type
// only; non-nil "<verb>" = exact match. The user-friendly alias --type=mentions
// is mapped to "" here (mirrors server-side parseRelationshipTypeFilter).
func parseRefsTypeFilter(cmd *cobra.Command) *string {
	flag := cmd.Flags().Lookup("type")
	if flag == nil || !flag.Changed {
		return nil
	}
	v := flag.Value.String()
	if v == "mentions" {
		v = ""
	}
	return &v
}

func runRefsList(cmd *cobra.Command, beadID string, fn listRefsFn, header string) error {
	limit, _ := cmd.Flags().GetInt("limit")
	offset, _ := cmd.Flags().GetInt("offset")
	relType := parseRefsTypeFilter(cmd)

	resp, err := fn(context.Background(), beadID, limit, offset, relType)
	if err != nil {
		return fmt.Errorf("listing references: %w", err)
	}

	if jsonOutput {
		emitJSON(resp)
		return nil
	}

	if resp == nil || len(resp.References) == 0 {
		fmt.Println("No references found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "%s\tTYPE\tSTATUS\tSOURCE_FIELD\tREL\tTITLE\n", header)
	for _, r := range resp.References {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID,
			refOrDash(r.Type),
			refOrDash(r.Status),
			r.SourceField,
			relOrDash(r.RelationshipType),
			refOrDash(r.Title),
		)
	}
	w.Flush()

	if resp.Total > len(resp.References) {
		fmt.Printf("\nshowing %d of %d (offset=%d, limit=%d)\n",
			len(resp.References), resp.Total, resp.Offset, resp.Limit)
	}
	return nil
}

// refOrDash renders a "—" for empty fields, which happens when a forward
// reference points at a bead that doesn't exist (yet). Keeps tabular output
// from collapsing on missing fields.
func refOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// relOrDash renders the relationship_type column: untyped (NULL) shows "—",
// typed shows the verb. Mirrors refOrDash for the *string case.
func relOrDash(s *string) string {
	if s == nil {
		return "—"
	}
	return *s
}

// _ ensures the model import isn't dropped by goimports when used only via
// client.ListReferencesResponse — the references slice element is
// *model.BeadReferenceSummary.
var _ = (*model.BeadReferenceSummary)(nil)

func init() {
	for _, c := range []*cobra.Command{refsMentionedByCmd, refsMentionsCmd} {
		c.Flags().Int("limit", 50, "page size (max 500)")
		c.Flags().Int("offset", 0, "number of rows to skip")
		c.Flags().String("type", "", "filter by typed-mention relationship verb (e.g. parent, child, blocks). Use 'mentions' or '' for untyped only.")
	}
	refsCmd.AddCommand(refsMentionedByCmd)
	refsCmd.AddCommand(refsMentionsCmd)
}
