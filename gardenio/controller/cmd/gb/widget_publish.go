// `gb widget publish` is the operator-only label flip from
// widget-stage:draft → widget-stage:stable (DEC-S6 ka-0000006220).
// Authorization is enforced server-side at the daemon's label-write
// path; the CLI ensures we never flip a bead that fails strict
// validation.
//
// Workflow:
//
//   1. Load the widget-type bead.
//   2. Run gb widget validate --strict (skipped under --force).
//   3. Remove widget-stage:draft, add widget-stage:stable.
//   4. File an audit comment ("published by <actor>; <validate summary>").
package main

import (
	"fmt"
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

const (
	widgetStageDraft      = "widget-stage:draft"
	widgetStageStable     = "widget-stage:stable"
	widgetStageDeprecated = "widget-stage:deprecated"
)

// resolveWidgetTypeBead resolves a slug-or-id argument to a widget-type
// bead. Distinct from resolveWidgetInput (which may load a file): publish
// and deprecate must operate on a real bead.
func resolveWidgetTypeBead(cmd *cobra.Command, arg string) (*beadsapi.BeadDetail, error) {
	if (strings.HasPrefix(arg, "gd-") || strings.HasPrefix(arg, "ka-")) {
		bead, err := daemon.GetBead(cmd.Context(), arg)
		if err != nil {
			return nil, fmt.Errorf("loading widget-type bead %s: %w", arg, err)
		}
		if bead.Type != "widget-type" {
			return nil, fmt.Errorf("%s is type=%s, not widget-type", arg, bead.Type)
		}
		return bead, nil
	}
	all, err := listWidgetTypesBySlug(cmd, arg)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no widget-type bead with slug %q", arg)
	}
	return pickWidgetTypeVersion(all, ""), nil
}

// flipStageLabel removes any existing widget-stage:* label and adds
// `to`. Best-effort: a removal that fails because the label is already
// absent is non-fatal.
func flipStageLabel(cmd *cobra.Command, beadID string, currentLabels []string, to string) error {
	for _, l := range currentLabels {
		if !strings.HasPrefix(l, "widget-stage:") || l == to {
			continue
		}
		if err := daemon.RemoveLabel(cmd.Context(), beadID, l); err != nil {
			fmt.Fprintf(os.Stderr, "warning: removing label %s from %s: %v\n", l, beadID, err)
		}
	}
	return daemon.AddLabel(cmd.Context(), beadID, to)
}

// ── widget publish command ────────────────────────────────────────────

var widgetPublishCmd = &cobra.Command{
	Use:   "publish <slug-or-id>",
	Short: "(operator-only) Validate + flip a widget-type from draft to stable",
	Long: `Validate the widget-type with --strict, then flip its stage label
from widget-stage:draft to widget-stage:stable. Records an audit comment
on the bead.

Authorization: requires RoleOperator. The daemon enforces this on the
label-write path; agents will see a 403 if they try.

Flags:
  --note <text>   Note recorded in the audit comment.
  --force         Skip validate --strict (still records an audit note).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bead, err := resolveWidgetTypeBead(cmd, args[0])
		if err != nil {
			return err
		}

		note, _ := cmd.Flags().GetString("note")
		force, _ := cmd.Flags().GetBool("force")

		var validateSummary string
		if !force {
			doc := widgetDocFromBead(bead)
			res := validateWidget(doc, true)
			if !res.OK {
				for _, f := range res.Findings {
					if f.Severity == "error" {
						fmt.Fprintln(os.Stderr, f.String())
					}
				}
				return fmt.Errorf("validate --strict failed: %d error(s); fix or pass --force", res.Errors)
			}
			validateSummary = fmt.Sprintf("validate --strict: PASS (%d warnings)", res.Warnings)
		} else {
			validateSummary = "validate --strict: SKIPPED (--force)"
		}

		if err := flipStageLabel(cmd, bead.ID, bead.Labels, widgetStageStable); err != nil {
			return fmt.Errorf("flipping stage label: %w", err)
		}

		comment := buildAuditComment("published", bead, note, validateSummary)
		if err := daemon.AddComment(cmd.Context(), bead.ID, actor, comment); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to record audit comment on %s: %v\n", bead.ID, err)
		}

		if jsonOutput {
			printJSON(map[string]any{
				"id":      bead.ID,
				"slug":    bead.Fields["slug"],
				"version": bead.Fields["widget_version"],
				"stage":   "stable",
			})
			return nil
		}
		fmt.Printf("Published widget-type %s (slug=%s) — flipped to widget-stage:stable.\n",
			bead.ID, bead.Fields["slug"])
		fmt.Println(validateSummary)
		return nil
	},
}

// buildAuditComment formats a publish/deprecate audit comment. The
// shape is intentionally simple so future automation can grep-extract
// decisions without a parser.
func buildAuditComment(action string, bead *beadsapi.BeadDetail, note, validateSummary string) string {
	parts := []string{
		fmt.Sprintf("Widget %s by %s.", action, actor),
	}
	if validateSummary != "" {
		parts = append(parts, validateSummary)
	}
	if note != "" {
		parts = append(parts, fmt.Sprintf("Note: %s", note))
	}
	if v := bead.Fields["widget_version"]; v != "" {
		parts = append(parts, fmt.Sprintf("widget_version=%s", v))
	}
	return strings.Join(parts, " ")
}
