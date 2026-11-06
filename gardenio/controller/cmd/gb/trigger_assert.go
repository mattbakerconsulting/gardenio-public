// Package main — `gb trigger create assert` + `gb trigger test` subcommands.
//
// The `assert` subcommand is sugar over `create bead`: it creates a
// trigger:bead whose condition is a sandboxed CEL expression (feature F4,
// ka-0000008047). It is the ergonomic front door for assert-and-notify-on-
// violation triggers — "fire when this invariant over a bead (and its watched
// set) is TRUE". The expression is compiled-on-write so a malformed assertion
// is rejected here, not at runtime.
//
// The `test` subcommand is a dry-run: it compiles the trigger's CEL
// expression(s) and evaluates them against a real bead fetched from the
// daemon, printing the per-expression match result without firing the action.
// Authors use it to confirm an expression matches (or doesn't) the beads they
// expect before enabling the trigger.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

var (
	assertOpts        commonTriggerOpts
	assertEventTypes  []string
	assertExprs       []string
	assertConditions  []string
	assertOnViolation string
)

var triggerCreateAssertCmd = &cobra.Command{
	Use:   "assert",
	Short: "Create a bead-state trigger guarded by a sandboxed CEL expression",
	Long: `Create a trigger:bead guarded by a sandboxed CEL expression (--expr).
Each --expr is checked against a typed, I/O-free context — bead / event / now /
watched — and MUST evaluate to bool; a malformed or non-bool expression is
compiled-on-write and rejected here, at create time. Multiple --expr flags AND
together; --condition (field:op:value) clauses AND in too. Evaluation is
cost- and time-bounded, deterministic, and FAIL-CLOSED.

Two firing modes — this is the choice to make:

  Default (fire-on-match): without --on-violation, the trigger fires when the
  expression is TRUE — "notify on the beads that match this predicate".

  Assert (fire-on-violation): with --on-violation, the --expr clauses become an
  INVARIANT expected to hold; the trigger fires BECAUSE it is VIOLATED (any
  clause FALSE or a fail-closed error). A satisfied invariant stays silent.
  --condition becomes the event pre-filter; the alert can template {{violated}}
  and {{values}}; --on-violation picks the non-spawn action (notify |
  create_bead | both) and no agent is spawned.

See gardenio/docs/triggers.md (the "bead" source + "assert" action sections) for
the full CEL context — every bead/event/now/watched field, quantifiers, the
compile-on-write / fail-closed guarantees, and worked expressions. Dry-run an
expression against a real bead with 'gb trigger test <trigger-id> --bead <id>'.`,
	RunE: runTriggerCreateAssert,
}

func runTriggerCreateAssert(cmd *cobra.Command, args []string) error {
	if len(assertExprs) == 0 {
		return fmt.Errorf("--expr is required (at least one CEL expression)")
	}

	// Assert mode (feature F5, ka-0000008157): --on-violation flips the
	// semantic from fire-on-match to fire-on-violation. The --expr clauses
	// become the invariant (stored in the `assert` field), --condition becomes
	// the event pre-filter (stored in `conditions`), and the action is the
	// non-spawn `assert` fan-out.
	if assertOnViolation != "" {
		return runTriggerCreateAssertViolation(cmd)
	}

	fields, labels, err := buildBeadFieldsWithExprs(assertOpts, assertEventTypes, assertConditions, assertExprs)
	if err != nil {
		return err
	}

	id, err := createTriggerBead(cmd.Context(), assertOpts.title, "trigger:bead", fields, labels, assertOpts.extraLabels)
	if err != nil {
		return err
	}

	printTriggerCreated(cmd, id, "assert", map[string]string{
		"event_types": strings.Join(assertEventTypes, ","),
		"exprs":       fmt.Sprintf("%d", len(assertExprs)),
		"test":        fmt.Sprintf("gb trigger test %s --bead <some-bead-id>", id),
	})
	return nil
}

// runTriggerCreateAssertViolation creates a fire-on-violation assert trigger
// (feature F5, ka-0000008157). The --expr clauses are the invariant (stored in
// the `assert` field), --condition clauses are the event pre-filter (stored in
// `conditions`), and the action is `assert` with the chosen on_violation
// fan-out.
func runTriggerCreateAssertViolation(cmd *cobra.Command) error {
	switch assertOnViolation {
	case "notify", "create_bead", "both":
	default:
		return fmt.Errorf("invalid --on-violation %q (want notify|create_bead|both)", assertOnViolation)
	}

	// The assert action is its own non-spawn action; force it onto the opts so
	// applyCommonFields stamps action=assert. Any user-supplied --action is
	// overridden here because --on-violation is the explicit assert opt-in.
	o := assertOpts
	o.action = "assert"

	fields, labels, err := buildAssertViolationFields(o, assertEventTypes, assertConditions, assertExprs, assertOnViolation)
	if err != nil {
		return err
	}

	id, err := createTriggerBead(cmd.Context(), o.title, "trigger:bead", fields, labels, o.extraLabels)
	if err != nil {
		return err
	}

	printTriggerCreated(cmd, id, "assert", map[string]string{
		"event_types":  strings.Join(assertEventTypes, ","),
		"assert_exprs": fmt.Sprintf("%d", len(assertExprs)),
		"on_violation": assertOnViolation,
		"test":         fmt.Sprintf("gb trigger test %s --bead <some-bead-id>  # reports invariant match; fires when VIOLATED", id),
	})
	return nil
}

// --- gb trigger test -------------------------------------------------------

var triggerTestBead string

var triggerTestCmd = &cobra.Command{
	Use:   "test <trigger-id>",
	Short: "Dry-run a trigger's CEL expression(s) against a bead",
	Long: `Evaluate a trigger:bead's CEL expression condition(s) against a real
bead, WITHOUT firing the trigger's action. Prints, per expression, whether it
matched. Fetches the target bead from the daemon (--bead) so the typed context
mirrors what the live evaluator sees (bead fields, labels, event type).

The watched list is NOT resolved in dry-run mode (it requires the trigger
engine's bounded subgraph resolver) — expressions that quantify over ` + "`watched`" + `
see an empty list here. Field/op/value and time-relative conditions on the
trigger are reported but only the CEL expressions are evaluated.

Examples:
  gb trigger test ka-AbCdEf --bead ka-123456
  gb trigger test ka-AbCdEf --bead ka-123456 --event updated`,
	Args: cobra.ExactArgs(1),
	RunE: runTriggerTest,
}

var triggerTestEvent string

func runTriggerTest(cmd *cobra.Command, args []string) error {
	triggerID := args[0]
	if triggerTestBead == "" {
		return fmt.Errorf("--bead <bead-id> is required (the bead to evaluate the expression against)")
	}
	ctx := cmd.Context()

	trig, err := daemon.GetBead(ctx, triggerID)
	if err != nil {
		return fmt.Errorf("fetching trigger %s: %w", triggerID, err)
	}
	if trig == nil {
		return fmt.Errorf("trigger %s not found", triggerID)
	}
	if trig.Type != "trigger" {
		return fmt.Errorf("bead %s is type=%q, expected type=trigger", triggerID, trig.Type)
	}

	// An assert trigger (feature F5) carries its CEL invariant in the `assert`
	// field, NOT in `conditions` (which, for an assert, is only the event
	// pre-filter). Detect that shape and test the invariant with the correct
	// fire-on-VIOLATION semantic. A default fire-on-match trigger keeps the
	// CEL in `conditions` and tests with the fire-on-MATCH semantic.
	assertClauses, err := parseTriggerConditions(trig.Fields["assert"])
	if err != nil {
		return fmt.Errorf("parsing trigger assert clauses: %w", err)
	}
	isAssert := len(assertClauses) > 0

	var exprConds []beadConditionJSON
	if isAssert {
		exprConds = make([]beadConditionJSON, 0, len(assertClauses))
		for _, c := range assertClauses {
			if strings.TrimSpace(c.Expr) != "" {
				exprConds = append(exprConds, c)
			}
		}
		if len(exprConds) == 0 {
			return fmt.Errorf("trigger %s assert field carries no CEL expression clause (nothing to test); assert=%q", triggerID, trig.Fields["assert"])
		}
	} else {
		conds, cerr := parseTriggerConditions(trig.Fields["conditions"])
		if cerr != nil {
			return cerr
		}
		exprConds = make([]beadConditionJSON, 0, len(conds))
		for _, c := range conds {
			if strings.TrimSpace(c.Expr) != "" {
				exprConds = append(exprConds, c)
			}
		}
		if len(exprConds) == 0 {
			return fmt.Errorf("trigger %s carries no CEL expression to test (no assert invariant and no expr condition); conditions=%q", triggerID, trig.Fields["conditions"])
		}
	}

	target, err := daemon.GetBead(ctx, triggerTestBead)
	if err != nil {
		return fmt.Errorf("fetching target bead %s: %w", triggerTestBead, err)
	}
	if target == nil {
		return fmt.Errorf("target bead %s not found", triggerTestBead)
	}

	eventType := triggerTestEvent
	if eventType == "" {
		eventType = "updated"
	}
	beadEvent := bridge.BeadDetailToEvent(target)
	now := time.Now()

	type exprResult struct {
		Expr    string `json:"expr"`
		Matched bool   `json:"matched"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]exprResult, 0, len(exprConds))
	allMatched := true
	for _, c := range exprConds {
		r := exprResult{Expr: c.Expr}
		compiled, cerr := bridge.CompileExpr(c.Expr)
		if cerr != nil {
			r.Error = cerr.Error()
			r.Matched = false
			allMatched = false
			results = append(results, r)
			continue
		}
		res := compiled.Eval(ctx, beadEvent, eventType, now, nil)
		if res.Err != nil {
			r.Error = res.Err.Error()
			r.Matched = false
			allMatched = false
		} else {
			r.Matched = res.Matched
			if !res.Matched {
				allMatched = false
			}
		}
		results = append(results, r)
	}

	// For an assert trigger the invariant is expected to HOLD; the trigger
	// fires when it is VIOLATED (any clause false or fail-closed). For a
	// default trigger the expression is the firing predicate; it fires on a
	// full MATCH. wouldFire collapses both into "would this trigger act".
	wouldFire := allMatched
	if isAssert {
		wouldFire = !allMatched
	}

	if jsonOutput {
		printJSON(map[string]any{
			"trigger":     triggerID,
			"bead":        triggerTestBead,
			"event":       eventType,
			"mode":        map[bool]string{true: "assert", false: "match"}[isAssert],
			"all_matched": allMatched,
			"would_fire":  wouldFire,
			"results":     results,
		})
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Trigger %s vs bead %s (event=%s, mode=%s):\n",
		triggerID, triggerTestBead, eventType, map[bool]string{true: "assert", false: "match"}[isAssert])
	for _, r := range results {
		// In assert mode a clause that holds (matched) is GOOD (silent); a
		// clause that fails (no-match / error) is the VIOLATION that fires.
		var status string
		switch {
		case r.Error != "":
			if isAssert {
				status = "✗ ERROR (fail-closed → VIOLATED → fires)"
			} else {
				status = "✗ ERROR (fail-closed → no-match)"
			}
		case r.Matched:
			if isAssert {
				status = "✓ holds"
			} else {
				status = "✓ MATCH"
			}
		default:
			if isAssert {
				status = "✗ VIOLATED"
			} else {
				status = "✗ no-match"
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\n    expr: %s\n", status, r.Expr)
		if r.Error != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "    error: %s\n", r.Error)
		}
	}
	var verdict string
	switch {
	case isAssert && wouldFire:
		verdict = "WOULD fire (invariant VIOLATED)"
	case isAssert:
		verdict = "would NOT fire (invariant holds)"
	case wouldFire:
		verdict = "WOULD fire (all expressions matched)"
	default:
		verdict = "would NOT fire"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Result: %s\n", verdict)
	if triggerTestEvent == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "(note: --event defaulted to \"updated\"; `watched` is empty in dry-run)")
	}
	return nil
}

// parseTriggerConditions unmarshals the trigger bead's conditions JSON field
// into the local beadConditionJSON shape. Empty field yields no conditions.
func parseTriggerConditions(condJSON string) ([]beadConditionJSON, error) {
	if strings.TrimSpace(condJSON) == "" {
		return nil, nil
	}
	var conds []beadConditionJSON
	if err := json.Unmarshal([]byte(condJSON), &conds); err != nil {
		return nil, fmt.Errorf("parsing trigger conditions JSON: %w", err)
	}
	return conds, nil
}
