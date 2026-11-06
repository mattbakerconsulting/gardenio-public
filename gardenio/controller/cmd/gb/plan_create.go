// Command `gb plan create` originates a from-scratch plan the way an operator
// does with the browser "+ New plan" affordance — a one-line goal in, a live
// /plans plan out (task 3a, ka-0000006081; design ka-0000005773 §4).
//
// It composes STRAIGHT against the beads daemon via createPlanEpicDaemon
// (plan_write.go): the born-on 'plan' + 'project:<project>' labels AND
// plan_state=draft ride ONE CreateBead transaction (the invisible-orphan guard
// R2), an idempotency key collapses a double-submit to exactly one epic, and the
// epic is read back and shape-asserted before returning. This is the daemon-direct
// replacement for the earlier POST /plans path, which 401'd from an agent pod
// against an OIDC-gated viewer (ka-0000006389); it now needs only BEADS_AUTH_TOKEN.
//
// It does NOT chain the author assistant (the reactive research/draft loop the
// browser create fires) — that seam is re-homed by ka-0000007841. `gb plan create`
// here mints + verifies the plan epic; the assistant collaboration is not yet
// driven from the daemon path.
package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var (
	planCreateGoal    string
	planCreateProject string
)

var planCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Originate a plan from a one-line goal (mint the epic + labels transactionally, daemon-direct)",
	Long: `Originate a NEW plan from a one-line goal. Mints a type=epic with the
'plan' and 'project:<project>' labels AND plan_state=draft born-on in ONE atomic
write, collapses a double-submit to a single epic via an idempotency key,
verifies the epic reads back as a discoverable, gated plan (the invisible-orphan
guard, R2), and prints the new plan id. The epic is stamped as the
--operated-by oidc:<email> operator so the kbeads audit resolver accepts the write.

Note: this does NOT chain the browser's author assistant (the reactive
research/draft loop) — that seam is re-homed by ka-0000007841. The plan is born
plan_state=draft; move it through the lifecycle from the cockpit.`,
	Args: cobra.NoArgs,
	RunE: runPlanCreate,
}

func init() {
	planCmd.AddCommand(planCreateCmd)
	planCreateCmd.Flags().StringVar(&planCreateGoal, "goal", "", "one-line plan goal — the seed the assistant refines (required)")
	planCreateCmd.Flags().StringVar(&planCreateProject, "project", "", "project the plan belongs to, e.g. gardenio (required)")
	planCreateCmd.SilenceUsage = true
}

func runPlanCreate(cmd *cobra.Command, _ []string) error {
	goal := strings.TrimSpace(planCreateGoal)
	if goal == "" {
		return errors.New("--goal is required")
	}
	project := strings.TrimSpace(planCreateProject)
	if project == "" {
		return errors.New("--project is required")
	}

	email, err := operatorEmailForViewer(operatedBy)
	if err != nil {
		return err
	}

	epic, err := createPlanEpicDaemon(cmd.Context(), planCmdWriter(), goal, project, email)
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(map[string]any{
			"id":       epic.ID,
			"goal":     goal,
			"project":  project,
			"operator": canonicalPlanOperator(email),
			"status":   "ok",
		})
		return nil
	}
	fmt.Printf("created plan %s (%s): %s\n", epic.ID, project, goal)
	return nil
}
