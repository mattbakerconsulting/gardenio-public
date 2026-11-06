// Package main — `gb trigger create bead` subcommand.
//
// Creates `trigger:bead` beads — event-driven triggers that fire on bead
// lifecycle SSE events (created, updated, closed). Mirrors the webhook
// subcommand's `--condition` shape, but the comparand is a bead field or
// label rather than a JSON path.
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

var (
	beadOpts       commonTriggerOpts
	beadEventTypes []string
	beadConditions []string
)

var triggerCreateBeadCmd = &cobra.Command{
	Use:   "bead",
	Short: "Create a bead-state trigger (fires on bead lifecycle events)",
	Long: `Create a trigger fired by bead lifecycle SSE events
(beads.bead.created, beads.bead.updated, beads.bead.closed).

Conditions use the form field:op:value, where op is one of:
  ==, !=, in, not_in, contains, present, absent, field_changed, changed_to,
  older_than, newer_than, is_involuntary_death

Field may be a top-level bead field (type, status, title, assignee, created_by,
closed_by, id), a key in the bead's extensible Fields map, or "label:<name>"
to check label presence/absence. End the label key with ":" for prefix
matching (e.g. "label:project:" matches any "project:<name>" label).

The "field_changed" and "changed_to" operators query the BeadUpdated event's
Changes map and only match on real field transitions — they suppress firing
on unrelated updates (e.g. comment-add) where the named field is unchanged.
"field_changed" is unary (no value); "changed_to" requires the new value.

The "older_than" and "newer_than" operators evaluate the age of a timestamp
field (RFC3339, e.g. last_checked); the value is a Go duration ("2h", "90m").
"older_than" matches when the field is stale (now - field >= value);
"newer_than" is the inverse (fresh). Both fail closed: a missing field or an
unparseable timestamp/duration evaluates to false, so a broken freshness
assertion never silently passes.

The "is_involuntary_death" operator (auto-forensics, epic ka-0000005447) fires
when close_reason denotes a counted involuntary mid-work death. It delegates to
the SAME predicate that gates gardenio_agent_involuntary_deaths_total, so a
trigger keyed on it fires on exactly the population the metric counts. The value
is OPTIONAL and narrows to one death family (stop_gate_cooldown,
context_cancelled, …); empty matches any. Fail-closed on a clean/unclassified
reason, so it stays silent on healthy despawns.

Examples:
  gb trigger create bead \
    --event-types created,updated \
    --condition type:==:task \
    --condition label:needs-review:present \
    --action notify \
    --prompt 'New review-ready bead {{bead_id}}: {{bead_title}}'

  gb trigger create bead \
    --event-types updated \
    --condition status:changed_to:in_progress \
    --action notify \
    --prompt 'Bead {{bead_id}} just entered in_progress'

  gb trigger create bead \
    --event-types closed \
    --condition status:==:closed \
    --condition label:project:gardenio:present

  # Auto-forensics: spawn a forensics agent on any crew that dies mid-work
  # (epic ka-0000005447). The spawned agent runs
  # 'gb session death-report --bead {{bead_id}}' to pull the dead agent's own
  # S3 recording, summarize the last N turns, and squawk a report.
  gb trigger create bead \
    --title 'Auto-forensics: involuntary agent death' \
    --event-types closed \
    --condition type:==:agent \
    --condition close_reason:is_involuntary_death \
    --action spawn_agent --role crew --agent-project gardenio \
    --prompt 'Forensics: agent {{bead_id}} died involuntarily ({{death_family}}). Run: gb session death-report --bead {{bead_id}} -n 3 — then squawk a 3-line report (agent, family, last words).'`,
	RunE: runTriggerCreateBead,
}

func runTriggerCreateBead(cmd *cobra.Command, args []string) error {
	fields, labels, err := buildBeadFields(beadOpts, beadEventTypes, beadConditions)
	if err != nil {
		return err
	}

	id, err := createTriggerBead(cmd.Context(), beadOpts.title, "trigger:bead", fields, labels, beadOpts.extraLabels)
	if err != nil {
		return err
	}

	printTriggerCreated(cmd, id, "bead", map[string]string{
		"event_types": strings.Join(beadEventTypes, ","),
		"conditions":  fmt.Sprintf("%d", len(beadConditions)),
		"test":        "kd close <some-bead> # if 'closed' is in --event-types and conditions match, the trigger should fire",
	})
	return nil
}

// buildBeadFields constructs the bead field map and label list for a
// trigger:bead trigger. Extracted so the field shape can be unit-tested
// without hitting the kbeads server.
func buildBeadFields(o commonTriggerOpts, eventTypes, conditions []string) (map[string]any, []string, error) {
	return buildBeadFieldsWithExprs(o, eventTypes, conditions, nil)
}

// buildBeadFieldsWithExprs is buildBeadFields plus CEL expression conditions
// (feature F4, ka-0000008047). Each expr in `exprs` becomes a
// `{"expr": "<CEL>"}` condition appended to the same `conditions` JSON array
// the field/op/value conditions use — the two kinds AND together at eval time.
// Every expression is compiled-on-write here (bridge.CompileExpr) so a
// malformed expression is rejected at authoring time, not at runtime.
func buildBeadFieldsWithExprs(o commonTriggerOpts, eventTypes, conditions, exprs []string) (map[string]any, []string, error) {
	fields := map[string]any{}

	if et := normalizeBeadEventTypes(eventTypes); et != "" {
		fields["event_types"] = et
	}

	var conds []beadConditionJSON
	if len(conditions) > 0 {
		parsed, err := parseBeadConditions(conditions)
		if err != nil {
			return nil, nil, err
		}
		conds = append(conds, parsed...)
	}
	for _, expr := range exprs {
		expr = strings.TrimSpace(expr)
		if expr == "" {
			continue
		}
		// Compile-on-write: reject malformed expressions at authoring time.
		if _, err := bridge.CompileExpr(expr); err != nil {
			return nil, nil, fmt.Errorf("invalid CEL expression %q: %w", expr, err)
		}
		conds = append(conds, beadConditionJSON{Expr: expr})
	}
	if len(conds) > 0 {
		j, err := json.Marshal(conds)
		if err != nil {
			return nil, nil, fmt.Errorf("marshalling conditions: %w", err)
		}
		fields["conditions"] = string(j)
	}

	if err := applyCommonFields(o, fields); err != nil {
		return nil, nil, err
	}
	labels := []string{"trigger:bead"}
	return fields, labels, nil
}

// buildAssertViolationFields constructs the bead fields for a fire-on-violation
// assert trigger (feature F5, ka-0000008157). The CEL `exprs` become the
// invariant stored in the `assert` field (compiled-on-write here); the
// field/op/value `conditions` become the event pre-filter stored in
// `conditions`. The on_violation modifier selects the non-spawn alert fan-out.
// The action is expected to already be "assert" on `o` (set by the caller).
func buildAssertViolationFields(o commonTriggerOpts, eventTypes, conditions, exprs []string, onViolation string) (map[string]any, []string, error) {
	if len(exprs) == 0 {
		return nil, nil, fmt.Errorf("assert trigger requires at least one --expr (the invariant)")
	}
	fields := map[string]any{}

	if et := normalizeBeadEventTypes(eventTypes); et != "" {
		fields["event_types"] = et
	}

	// Pre-filter conditions (which beads to evaluate the invariant against).
	if len(conditions) > 0 {
		conds, err := parseBeadConditions(conditions)
		if err != nil {
			return nil, nil, err
		}
		j, err := json.Marshal(conds)
		if err != nil {
			return nil, nil, fmt.Errorf("marshalling conditions: %w", err)
		}
		fields["conditions"] = string(j)
	}

	// Invariant clauses, compiled-on-write so a malformed assertion is rejected
	// here, not at runtime.
	var asserts []beadConditionJSON
	for _, expr := range exprs {
		expr = strings.TrimSpace(expr)
		if expr == "" {
			continue
		}
		if _, err := bridge.CompileExpr(expr); err != nil {
			return nil, nil, fmt.Errorf("invalid assert CEL expression %q: %w", expr, err)
		}
		asserts = append(asserts, beadConditionJSON{Expr: expr})
	}
	if len(asserts) == 0 {
		return nil, nil, fmt.Errorf("assert trigger requires at least one non-empty --expr")
	}
	j, err := json.Marshal(asserts)
	if err != nil {
		return nil, nil, fmt.Errorf("marshalling assert clauses: %w", err)
	}
	fields["assert"] = string(j)
	fields["on_violation"] = onViolation

	if err := applyCommonFields(o, fields); err != nil {
		return nil, nil, err
	}
	labels := []string{"trigger:bead"}
	return fields, labels, nil
}

// normalizeBeadEventTypes splits a slice of CSV/repeated event-type values
// into a comma-joined, deduped string. Each entry may be either a short name
// (created, updated, closed) or the full SSE topic
// (beads.bead.created, etc.) — the source accepts both.
func normalizeBeadEventTypes(in []string) string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(in))
	for _, raw := range in {
		for p := range strings.SplitSeq(raw, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

// beadConditionJSON is the on-the-wire shape consumed by
// trigger_source_bead.go's beadCondition. Re-declared locally so the CLI does
// not depend on an internal/ package's unexported type.
type beadConditionJSON struct {
	Field string `json:"field,omitempty"`
	Op    string `json:"op,omitempty"`
	Value string `json:"value,omitempty"`
	// Expr is a CEL expression condition (feature F4, ka-0000008047). When
	// set, Field/Op/Value are omitted and the evaluator runs the sandboxed
	// CEL predicate instead. See internal/bridge/trigger_expr.go.
	Expr string `json:"expr,omitempty"`
}

// beadConditionOps lists the operators recognised by parseBeadConditions.
// Two-token forms (field:op) are valid for "present", "absent" and
// "field_changed"; all others require a three-token form (field:op:value).
// The set mirrors the operators supported by trigger_source_bead.go's
// matchConditions.
var beadConditionOps = map[string]bool{
	"==": true, "eq": true,
	"!=": true, "ne": true,
	"in": true, "not_in": true,
	"contains": true,
	"present":  true, "absent": true,
	"field_changed": true, "changed_to": true,
	// Time-relative (freshness) ops: field:op:<go-duration>, e.g.
	// last_checked:older_than:2h (decision ka-0000001680 JSON-DSL stopgap).
	"older_than": true, "newer_than": true,
	// Involuntary-death classifier op (ka-0000002929, epic ka-0000005447
	// auto-forensics): close_reason:is_involuntary_death[:family]. Unary — the
	// optional value narrows to one death family. Delegates to the SAME predicate
	// (gardeniometrics.InvoluntaryDeathFamily) that gates
	// gardenio_agent_involuntary_deaths_total.
	"is_involuntary_death": true,
}

// parseBeadConditions parses --condition field:op[:value] flags into the JSON
// structure stored on the trigger bead.
//
// Bead fields may themselves contain a colon (e.g. "label:needs-review",
// "label:project:gardenio"), so a naive SplitN(":", 3) does not work. Instead,
// we split on every colon and walk the tokens from the left, collecting
// field segments until we hit a known operator. The remainder (if any) is
// the value.
func parseBeadConditions(in []string) ([]beadConditionJSON, error) {
	out := make([]beadConditionJSON, 0, len(in))
	for _, s := range in {
		c, err := parseBeadCondition(s)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// parseBeadCondition parses a single field:op[:value] expression, treating
// the first segment matching a known operator as the operator (the
// operator-aware split lets fields like "label:project:gardenio" round-trip).
func parseBeadCondition(s string) (beadConditionJSON, error) {
	tokens := strings.Split(s, ":")
	if len(tokens) < 2 {
		return beadConditionJSON{}, fmt.Errorf("invalid --condition %q (want field:op[:value])", s)
	}
	opIdx := -1
	for i := 1; i < len(tokens); i++ {
		if beadConditionOps[strings.TrimSpace(tokens[i])] {
			opIdx = i
			break
		}
	}
	if opIdx < 1 {
		return beadConditionJSON{}, fmt.Errorf("invalid --condition %q (no recognised operator; want one of ==, !=, in, not_in, contains, present, absent, field_changed, changed_to, older_than, newer_than, is_involuntary_death)", s)
	}
	c := beadConditionJSON{
		Field: strings.TrimSpace(strings.Join(tokens[:opIdx], ":")),
		Op:    strings.TrimSpace(tokens[opIdx]),
	}
	if opIdx < len(tokens)-1 {
		c.Value = strings.Join(tokens[opIdx+1:], ":")
	}
	if c.Field == "" {
		return beadConditionJSON{}, fmt.Errorf("invalid --condition %q (field is required)", s)
	}
	switch c.Op {
	case "present", "absent", "field_changed", "is_involuntary_death":
		// Value is optional for unary operators. For is_involuntary_death an
		// optional value narrows to a single death family; empty matches any.
	default:
		if c.Value == "" {
			return beadConditionJSON{}, fmt.Errorf("invalid --condition %q (operator %q requires a value)", s, c.Op)
		}
	}
	return c, nil
}
