// Package bridge — gardener-gate guard.
//
// A trigger bead whose prompt asserts a "run budget" or "terminal goal" LOOKS
// gated, but the trigger engine only enforces those limits when a machine-
// readable backing exists: either a `gardener` block (the full gardener gate
// stack — run_budget exhaustion, terminal-goal stop-spec, one_spawn_at_a_time)
// or a `conditions` pre-flight (the bare-shape bead_query gate that
// `gb gardener start` writes). With neither, the budget/goal are pure prose the
// engine never reads and the loop is effectively unbounded.
//
// This file provides the detector shared by the trigger engine (warn-once at
// evaluation time) and the `gb trigger create` CLI (loud warning at creation
// time). See bug ka-0000006735 and advice ka-0000000974 ("every gardener MUST
// declare a run_budget + progress-aware/bead_query loop_end").
package bridge

import (
	"strings"
)

// gardenerGateMarkers are case-insensitive prompt substrings that signal the
// author INTENDED a machine-enforced loop budget or terminal goal. Both the
// underscore field forms (run_budget, terminal_goal, max_runs) and the prose
// forms ("run budget", "terminal goal") are matched, since authors copy either
// shape into prompts.
var gardenerGateMarkers = []string{
	"run_budget",
	"run budget",
	"terminal_goal",
	"terminal goal",
	"max_runs",
	"max runs",
}

// GardenerGateProse returns the gardener-gate markers present in prompt,
// de-duplicated and in the canonical order of gardenerGateMarkers. An empty
// result means the prompt makes no budget/goal claim. Matching is
// case-insensitive.
func GardenerGateProse(prompt string) []string {
	if prompt == "" {
		return nil
	}
	lower := strings.ToLower(prompt)
	var found []string
	for _, m := range gardenerGateMarkers {
		if strings.Contains(lower, m) {
			found = append(found, m)
		}
	}
	return found
}

// UnbackedGardenerProse reports whether a trigger prompt claims a run budget or
// terminal goal while NOTHING actually enforces it. When unbacked is true the
// trigger only LOOKS gated — the prose is fiction the engine never reads.
//
//   - hasGardener — a non-empty `gardener` block is present (the full machine
//     gate stack backs every budget/goal claim).
//   - hasGate    — a non-empty `conditions` pre-flight is present (the
//     bare-shape bead_query gate `gb gardener start` writes; it stops the loop
//     when the target query drains, which backs a terminal-goal claim).
//
// markers is always the set found by GardenerGateProse, so callers can name the
// offending language in the warning even when the trigger is properly backed.
func UnbackedGardenerProse(prompt string, hasGardener, hasGate bool) (markers []string, unbacked bool) {
	markers = GardenerGateProse(prompt)
	if len(markers) == 0 {
		return nil, false
	}
	if hasGardener || hasGate {
		return markers, false
	}
	return markers, true
}

// FieldsUnbackedGardenerProse applies UnbackedGardenerProse to a CLI-side fields
// map (the `map[string]any` assembled by `gb trigger create`). Non-string or
// empty gardener/conditions entries are treated as absent.
func FieldsUnbackedGardenerProse(fields map[string]any) (markers []string, unbacked bool) {
	prompt, _ := fields["prompt"].(string)
	hasGardener := nonEmptyStringField(fields["gardener"])
	hasGate := nonEmptyStringField(fields["conditions"])
	return UnbackedGardenerProse(prompt, hasGardener, hasGate)
}

// nonEmptyStringField reports whether v is a non-blank string.
func nonEmptyStringField(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

// GardenerGateWarning renders the loud, actionable warning shown when a trigger
// claims a budget/goal with no machine backing. markers should be the set
// returned by the *UnbackedGardenerProse helpers.
func GardenerGateWarning(triggerID string, markers []string) string {
	id := triggerID
	if id == "" {
		id = "<new>"
	}
	return strings.Join([]string{
		"⚠️  trigger " + id + " prompt claims " + strings.Join(markers, "/") +
			" but carries no machine-enforced backing (no `gardener` block, no `conditions` pre-flight).",
		"    The engine NEVER reads budget/goal prose — this loop is effectively unbounded.",
		"    Back it with one of:",
		"      • `gb gardener start <epic-id>` — writes a bead_query pre-flight that stops the loop when the epic drains, or",
		"      • a `gardener` block carrying run_budget + a loop_end stop spec (advice ka-0000000974).",
	}, "\n")
}
