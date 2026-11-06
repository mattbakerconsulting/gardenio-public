// `gb widget promote` is the canonical Stage-B verb for marking a
// widget-type stable (DEC-S6 ka-0000006220, Stage-B B4 ka-0000002878).
//
// It mirrors `gb widget publish` (which retains its existing semantics
// for compatibility): validate, then flip widget-stage:draft →
// widget-stage:stable, then record an audit comment. The operator-facing
// name is `promote` so the lifecycle vocabulary matches the synthesis
// decision and the beads-viewer chip surface.
//
// Authorization is enforced server-side at the daemon's label-write
// path; agents without RoleOperator will see a 403.
package main

import (
	"fmt"
	"os"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// widgetPromoteCmd is registered in widget.go's init().
var widgetPromoteCmd = &cobra.Command{
	Use:   "promote <slug-or-id>",
	Short: "(operator-only) Promote a widget-type to widget-stage:stable",
	Long: `Promote a widget-type from draft to stable.

Runs the strict validate gate (` + "`gb widget validate --strict`" + `) by
default, flips the lifecycle label from widget-stage:draft to
widget-stage:stable, and records an audit comment on the bead. After
promotion, beads-viewer renders the stable stage chip on every widget
card that references this widget-type slug.

This is the canonical Stage-B name for the publish/promote flip; ` +
		"`gb widget publish`" + ` remains available as the legacy alias.

Authorization: requires RoleOperator. The daemon enforces this on the
label-write path; agents will see a 403 if they try.

Examples:
  # By bead id, with a note.
  gb widget promote ka-0000006263 --note "B4 dogfood, week-1 stable"

  # By slug (latest version wins).
  gb widget promote gardener-cycle-summary-htmx --note "earned its keep"

  # Skip the validate --strict gate (still records the audit note).
  gb widget promote ka-0000006263 --force --note "operator override"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		bead, err := resolveWidgetTypeBead(cmd, args[0])
		if err != nil {
			return err
		}

		note, _ := cmd.Flags().GetString("note")
		force, _ := cmd.Flags().GetBool("force")

		validateSummary, err := runPromoteValidate(bead, force)
		if err != nil {
			return err
		}

		if err := flipStageLabel(cmd, bead.ID, bead.Labels, widgetStageStable); err != nil {
			return fmt.Errorf("flipping stage label: %w", err)
		}

		comment := buildAuditComment("promoted", bead, note, validateSummary)
		if err := daemon.AddComment(cmd.Context(), bead.ID, actor, comment); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to record audit comment on %s: %v\n", bead.ID, err)
		}

		if jsonOutput {
			printJSON(map[string]any{
				"id":      bead.ID,
				"slug":    bead.Fields["slug"],
				"version": bead.Fields["widget_version"],
				"stage":   "stable",
				"action":  "promote",
			})
			return nil
		}
		fmt.Printf("Promoted widget-type %s (slug=%s) — flipped to widget-stage:stable.\n",
			bead.ID, bead.Fields["slug"])
		fmt.Println(validateSummary)
		return nil
	},
}

// runPromoteValidate runs `gb widget validate --strict` against the bead
// unless force is set. Returns a one-line summary suitable for the audit
// comment, or an error if the strict gate fails (and force is false).
func runPromoteValidate(bead *beadsapi.BeadDetail, force bool) (string, error) {
	if force {
		return "validate --strict: SKIPPED (--force)", nil
	}
	doc := widgetDocFromBead(bead)
	res := validateWidget(doc, true)
	if !res.OK {
		for _, f := range res.Findings {
			if f.Severity == "error" {
				fmt.Fprintln(os.Stderr, f.String())
			}
		}
		return "", fmt.Errorf("validate --strict failed: %d error(s); fix or pass --force", res.Errors)
	}
	return fmt.Sprintf("validate --strict: PASS (%d warnings)", res.Warnings), nil
}
