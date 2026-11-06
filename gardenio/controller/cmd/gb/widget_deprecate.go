// `gb widget deprecate` is the operator-only retirement verb. Sets
// widget-stage:deprecated on the type bead, persists a `replaced_by`
// pointer field, and records an audit comment with the operator note.
//
// Existing widget-instances continue to render with a DEPRECATED chip
// (renderer-side; surfaced in P5/P6 work). New `gb widget emit` against
// a deprecated type is rejected unless --allow-deprecated.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// ── widget deprecate command ──────────────────────────────────────────

var widgetDeprecateCmd = &cobra.Command{
	Use:   "deprecate <slug-or-id>",
	Short: "(operator-only) Mark a widget-type deprecated with a replacement pointer",
	Long: `Flag a widget-type as deprecated and persist a pointer to the
replacement slug. Existing widget-instances continue rendering (with a
DEPRECATED chip surfaced by the renderer); new emissions are rejected
by ` + "`gb widget emit`" + ` unless --allow-deprecated is set.

Authorization: requires RoleOperator. The daemon enforces this on the
label-write path.

Flags:
  --replaced-by <slug>   Required. Slug of the replacement widget-type.
  --note <text>          Required. Operator note recorded in the audit comment.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		replacedBy, _ := cmd.Flags().GetString("replaced-by")
		note, _ := cmd.Flags().GetString("note")
		if strings.TrimSpace(replacedBy) == "" {
			return fmt.Errorf("--replaced-by is required (slug of the replacement widget-type)")
		}
		if strings.TrimSpace(note) == "" {
			return fmt.Errorf("--note is required (operator rationale)")
		}

		bead, err := resolveWidgetTypeBead(cmd, args[0])
		if err != nil {
			return err
		}

		// Verify the replacement exists, fail-loud (avoid dangling pointer).
		repl, err := listWidgetTypesBySlug(cmd, replacedBy)
		if err != nil {
			return fmt.Errorf("looking up --replaced-by %q: %w", replacedBy, err)
		}
		if len(repl) == 0 {
			return fmt.Errorf("--replaced-by %q does not match any registered widget-type", replacedBy)
		}

		// Persist the replaced_by pointer in the bead's fields.
		if err := daemon.UpdateBeadFields(cmd.Context(), bead.ID, map[string]string{
			"replaced_by": replacedBy,
		}); err != nil {
			return fmt.Errorf("setting replaced_by field: %w", err)
		}

		if err := flipStageLabel(cmd, bead.ID, bead.Labels, widgetStageDeprecated); err != nil {
			return fmt.Errorf("flipping stage label: %w", err)
		}

		comment := buildAuditComment(
			fmt.Sprintf("deprecated (replaced_by=%s)", replacedBy),
			bead, note, "",
		)
		if err := daemon.AddComment(cmd.Context(), bead.ID, actor, comment); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to record audit comment on %s: %v\n", bead.ID, err)
		}

		if jsonOutput {
			b, _ := json.Marshal(map[string]any{
				"id":          bead.ID,
				"slug":        bead.Fields["slug"],
				"stage":       "deprecated",
				"replaced_by": replacedBy,
			})
			fmt.Println(string(b))
			return nil
		}
		fmt.Printf("Deprecated widget-type %s (slug=%s) — replaced_by=%s.\n",
			bead.ID, bead.Fields["slug"], replacedBy)
		return nil
	},
}
