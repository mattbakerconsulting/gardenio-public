// Commands `gb plan add-phase` and `gb plan reorder-phase` give the machine
// operator (a steering gardener) the phase verbs the browser cockpit already has
// but the CLI lacked (ka-0000002361). A /plans PHASE is an ordered sub-epic that
// groups a plan's steps — a direct-child type=epic of the plan whose id is
// listed in the plan's fields.phase_order (membership + display order, one JSON
// array; design ka-0000001034). The browser exposes "＋ New phase" and the ▲▼
// reorder arrows; these two verbs are their CLI counterparts.
//
// Both compose STRAIGHT against the beads daemon via the plan_write.go helpers
// (addPlanPhase / reorderPlanPhase): add-phase mints the phase epic, links it
// parent-child, and grows phase_order with the atomic append-if-absent primitive;
// reorder-phase permutes phase_order through the compare-and-swap primitive with
// retry (ka-0000000988). This is the daemon-direct replacement for the earlier
// POST /plans/{id}/phases[…/reorder] path, which 401'd from an agent pod against
// an OIDC-gated viewer (ka-0000006389); both now need only BEADS_AUTH_TOKEN and
// are stamped on behalf of the --operated-by operator.
package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var (
	planAddPhaseTitle   string
	planReorderPhaseDir string
)

var planAddPhaseCmd = &cobra.Command{
	Use:   "add-phase <plan-epic-id>",
	Short: "Add a phase (a step-grouping sub-epic) to a plan, daemon-direct",
	Long: `Add a PHASE to the plan epic <plan-epic-id>: mint a new type=epic child
of the plan, link it parent-child, and APPEND its id to the plan's
fields.phase_order (via the atomic append-if-absent primitive) so it renders as
an inline (initially empty) phase on the timeline. Move steps into it with
'gb plan add-step --phase <this-phase-id>'.`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanAddPhase,
}

var planReorderPhaseCmd = &cobra.Command{
	Use:   "reorder-phase <plan-epic-id> <phase-epic-id>",
	Short: "Move a phase up or down within a plan's phase order, daemon-direct",
	Long: `Move the phase <phase-epic-id> one position within the plan's phase order:
permute fields.phase_order through the compare-and-swap primitive (with retry on
a concurrent edit). --dir up moves the phase toward the front of the timeline,
--dir down toward the end. A move off the boundary (already first/last) is a
harmless no-op.`,
	Args: cobra.ExactArgs(2),
	RunE: runPlanReorderPhase,
}

func init() {
	planCmd.AddCommand(planAddPhaseCmd)
	planAddPhaseCmd.Flags().StringVar(&planAddPhaseTitle, "title", "", "phase title, e.g. \"Research & design\" (required)")
	planAddPhaseCmd.SilenceUsage = true

	planCmd.AddCommand(planReorderPhaseCmd)
	planReorderPhaseCmd.Flags().StringVar(&planReorderPhaseDir, "dir", "", "direction to move the phase: up or down (required)")
	planReorderPhaseCmd.SilenceUsage = true
}

func runPlanAddPhase(cmd *cobra.Command, args []string) error {
	planID := strings.TrimSpace(args[0])
	if planID == "" {
		return errors.New("plan epic id is required")
	}
	title := strings.TrimSpace(planAddPhaseTitle)
	if title == "" {
		return errors.New("--title is required")
	}
	email, err := operatorEmailForViewer(operatedBy)
	if err != nil {
		return err
	}

	res, err := addPlanPhase(cmd.Context(), planCmdWriter(), planID, title, email)
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(map[string]any{
			"id":       res.ID,
			"plan":     planID,
			"title":    title,
			"operator": canonicalPlanOperator(email),
			"status":   "ok",
		})
		return nil
	}
	fmt.Printf("added phase %s to plan %s: %s\n", res.ID, planID, title)
	fmt.Printf("  add steps to it: gb plan add-step %s --phase %s --title \"…\"\n", planID, res.ID)
	return nil
}

func runPlanReorderPhase(cmd *cobra.Command, args []string) error {
	planID := strings.TrimSpace(args[0])
	phaseID := strings.TrimSpace(args[1])
	if planID == "" || phaseID == "" {
		return errors.New("plan epic id and phase epic id are both required")
	}
	dir := strings.TrimSpace(planReorderPhaseDir)
	if dir != "up" && dir != "down" {
		return errors.New("--dir must be up or down")
	}
	email, err := operatorEmailForViewer(operatedBy)
	if err != nil {
		return err
	}

	res, err := reorderPlanPhase(cmd.Context(), planCmdWriter(), planID, phaseID, dir, email)
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(map[string]any{
			"plan":     planID,
			"phase":    phaseID,
			"dir":      dir,
			"order":    res.Order,
			"operator": canonicalPlanOperator(email),
			"status":   res.Status,
		})
		return nil
	}
	if res.Status == "noop" {
		fmt.Printf("phase %s already at the %s boundary in plan %s — no change\n", phaseID, dir, planID)
		return nil
	}
	fmt.Printf("moved phase %s %s in plan %s\n", phaseID, dir, planID)
	return nil
}
