// Command `gb plan pour` emplaces a formula pour as a PHASE of a plan, straight
// against the beads daemon — the CLI counterpart of the browser pour form's
// "Emplace into plan" field (ka-0000000051, epic ka-0000000759).
//
// A /plans phase is a direct-child container of the plan epic whose id is listed
// in the plan's fields.phase_order (membership + display order; design
// ka-0000001034). Pouring a formula already produces exactly that shape — a
// molecule (a container) with its steps as issue children — so emplacing it is:
// pour with a target plan, and the daemon parents the molecule under the plan and
// appends its id to phase_order in one server-side request (http_formula_pour.go,
// ka-0000004940). The container stays type=molecule; the viewer's isPhaseContainer
// predicate renders it as a phase (resolved decision ka-0000003759).
//
// This composes DAEMON-DIRECT via daemon.PourFormula (the same POST
// /v1/formulas/{id}/pour the browser Pour button uses), so it needs only
// BEADS_AUTH_TOKEN and runs from an agent pod against an OIDC-gated deployment —
// matching every other `gb plan` write verb (ka-0000006389). The daemon endpoint
// is operator-role gated and createBead needs the operator stamped on ctx, so the
// verb runs on behalf of the --operated-by oidc:<email> operator.
package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"gardenio/controller/internal/beadsapi"
)

var (
	planPourFormula   string
	planPourVars      []string
	planPourForce     bool
	planPourEphemeral bool
	planPourPosition  int
)

var planPourCmd = &cobra.Command{
	Use:   "pour <plan-epic-id> --formula <formula-id> [--var k=v ...]",
	Short: "Pour a formula into a plan as a new phase, daemon-direct",
	Long: `Pour the formula --formula into the plan epic <plan-epic-id> as a new PHASE:
the daemon instantiates the formula (a molecule + its step children) and, because
a target plan is given, parents the molecule under the plan and appends its id to
the plan's fields.phase_order — so it renders as a first-class phase on the /plans
timeline, its steps being the molecule's step children.

The container stays a type=molecule (keeping its /molecule execution view, kd mol
verbs, wisp GC, dedup guard, and formula lineage); the plan cockpit's phase
predicate accepts it as a phase.

Supply formula variables with repeated --var k=v. --force overrides the
duplicate-molecule guard; --ephemeral pours a wisp (vapor phase) instead of a
persistent molecule.

Composes straight against the beads daemon (POST /v1/formulas/{id}/pour with a
target plan) with only BEADS_AUTH_TOKEN, stamped on behalf of the --operated-by
oidc:<email> operator — like every other 'gb plan' write verb.`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanPour,
}

func init() {
	planCmd.AddCommand(planPourCmd)
	planPourCmd.Flags().StringVar(&planPourFormula, "formula", "", "formula bead id to pour (required)")
	planPourCmd.Flags().StringArrayVar(&planPourVars, "var", nil, "formula variable as k=v (repeatable)")
	planPourCmd.Flags().BoolVar(&planPourForce, "force", false, "override the duplicate-molecule guard")
	planPourCmd.Flags().BoolVar(&planPourEphemeral, "ephemeral", false, "pour a wisp (vapor phase) instead of a persistent molecule")
	planPourCmd.Flags().IntVar(&planPourPosition, "position", -1, "0-based slot in the plan's phase_order to emplace the new phase at (default: append at the end)")
	planPourCmd.SilenceUsage = true
}

func runPlanPour(cmd *cobra.Command, args []string) error {
	planID := strings.TrimSpace(args[0])
	if planID == "" {
		return errors.New("plan epic id is required")
	}
	formulaID := strings.TrimSpace(planPourFormula)
	if formulaID == "" {
		return errors.New("--formula is required")
	}
	vars, err := parsePlanPourVars(planPourVars)
	if err != nil {
		return err
	}
	email, err := operatorEmailForViewer(operatedBy)
	if err != nil {
		return err
	}

	// --position is opt-in: the flag default -1 means "unset" (tail append, the
	// prior behaviour); a value >= 0 emplaces the phase at that slot (ka-0000001001).
	// A negative value other than the -1 sentinel is a caller mistake — surface it
	// rather than silently appending.
	var position *int
	if cmd.Flags().Changed("position") {
		if planPourPosition < 0 {
			return fmt.Errorf("--position must be >= 0 (got %d)", planPourPosition)
		}
		position = &planPourPosition
	}

	// Stamp the operator on ctx so the daemon's createBead (molecule + steps) and
	// the phase_order append carry the operator the audit resolver requires — the
	// same stamp every daemon-direct plan write applies (withPlanOperator).
	ctx := withPlanOperator(cmd.Context(), email)
	resp, err := daemon.PourFormula(ctx, formulaID, beadsapi.PourFormulaRequest{
		Vars:           vars,
		Force:          planPourForce,
		Ephemeral:      planPourEphemeral,
		TargetPlan:     planID,
		TargetPosition: position,
	})
	if err != nil {
		return fmt.Errorf("pouring formula %s into plan %s: %w", formulaID, planID, err)
	}
	molID := resp.MoleculeID()

	if jsonOutput {
		printJSON(map[string]any{
			"plan":     planID,
			"formula":  formulaID,
			"molecule": molID,
			"steps":    resp.TotalSteps,
			"operator": canonicalPlanOperator(email),
			"status":   "ok",
		})
		return nil
	}
	fmt.Printf("poured formula %s into plan %s as phase %s (%d step%s)\n",
		formulaID, planID, molID, resp.TotalSteps, plural(resp.TotalSteps))
	return nil
}

// parsePlanPourVars parses repeated --var k=v flags into the pour var map. A flag
// missing '=' (or with an empty key) is a caller mistake surfaced here rather than
// silently dropped, so a typo'd var never pours with a missing substitution.
func parsePlanPourVars(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	vars := make(map[string]string, len(raw))
	for _, kv := range raw {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("--var %q is not in k=v form", kv)
		}
		key := strings.TrimSpace(kv[:eq])
		if key == "" {
			return nil, fmt.Errorf("--var %q has an empty key", kv)
		}
		vars[key] = kv[eq+1:]
	}
	return vars, nil
}
