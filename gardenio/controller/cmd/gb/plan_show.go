// Command `gb plan show` prints a plan's resolved timeline — phases, steps, and
// lifecycle state — reading STRAIGHT from the daemon (epic ka-0000003451, task
// ka-0000004997). Unlike every `gb plan` WRITE verb, which POSTs to the
// operator-gated beads-viewer and 401s from an agent pod against an OIDC-gated
// viewer (ka-0000006389), this read path uses only the *beadsapi.Client `gb`
// already carries and BEADS_AUTH_TOKEN — no viewer URL, no operator header. It
// is the first decoupled plan READ surface (the beachhead the epic proves) and
// the verb the contract test ka-0000004277 drives.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var planShowCmd = &cobra.Command{
	Use:   "show <plan-epic-id>",
	Short: "Show a plan's phases + steps, read directly from the daemon (no viewer)",
	Long: `Print the resolved timeline of the plan epic <plan-epic-id> — its ordered
phases, each phase's steps, and the flat/unphased steps — reading STRAIGHT from
the beads daemon.

Unlike the 'gb plan' write verbs (create / add-step / add-phase / complete /
reopen), which POST to the operator-gated beads-viewer, this read path talks
only to the daemon via the client gb already carries. It needs just
BEADS_AUTH_TOKEN — the exact environment where the write verbs 401 against an
OIDC-gated viewer — and takes no --viewer-url. Membership and
ordering (phase_order / step_order) match the browser cockpit exactly.

With --json, emits the full read model (id, title, status, plan_state, project,
phases[], steps[]) for scripting.`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanShow,
}

func init() {
	planCmd.AddCommand(planShowCmd)
	planShowCmd.SilenceUsage = true
}

func runPlanShow(cmd *cobra.Command, args []string) error {
	planID := strings.TrimSpace(args[0])
	if planID == "" {
		return fmt.Errorf("plan epic id is required")
	}
	view, err := resolvePlanRead(cmd.Context(), daemon, planID)
	if err != nil {
		return err
	}
	if jsonOutput {
		printJSON(view)
		return nil
	}
	printPlanReadView(view)
	return nil
}

// printPlanReadView renders the human-readable plan timeline: a header line,
// then phases (numbered, each with its indented steps) followed by the flat
// steps. A step line shows its 1..N number, a done/open marker, id, type, and
// title. Kept deliberately plain (no color, no box-drawing) so it reads well in
// a gardener's log and is trivial to eyeball in CI.
func printPlanReadView(v *planReadView) {
	header := fmt.Sprintf("%s  %s  [%s", v.ID, withArms(v.Heraldry, v.Title), v.Status)
	if v.PlanState != "" {
		header += "/" + v.PlanState
	}
	header += "]"
	if v.Project != "" {
		header += "  project:" + v.Project
	}
	fmt.Println(header)

	if len(v.Phases) == 0 && len(v.Steps) == 0 && len(v.Unreadable) == 0 {
		fmt.Println("  (no steps)")
		return
	}

	for _, ph := range v.Phases {
		fmt.Printf("\n  Phase %d: %s  %s  [%s]\n", ph.Number, ph.ID, withArms(ph.Heraldry, ph.Title), ph.Status)
		if len(ph.Steps) == 0 && len(ph.Unreadable) == 0 {
			fmt.Println("    (no steps)")
			continue
		}
		for _, st := range ph.Steps {
			printPlanStepLine("    ", st)
		}
		printUnreadableChildren(os.Stdout, "    ", ph.Unreadable)
	}

	if len(v.Steps) > 0 {
		if len(v.Phases) > 0 {
			fmt.Println("\n  Steps (unphased):")
		}
		for _, st := range v.Steps {
			printPlanStepLine("  ", st)
		}
	}
	printUnreadableChildren(os.Stdout, "  ", v.Unreadable)
}

// printPlanStepLine prints one step row under the given indent: "[x] #N gd-…
// (type) title" — [x] when closed, [ ] otherwise.
func printPlanStepLine(indent string, st planStepRead) {
	mark := " "
	if st.Closed {
		mark = "x"
	}
	fmt.Printf("%s[%s] #%d %s (%s) %s\n", indent, mark, st.Number, st.ID, st.Type, withArms(st.Heraldry, st.Title))
}
