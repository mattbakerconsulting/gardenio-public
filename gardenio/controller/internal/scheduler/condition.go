package scheduler

import (
	"context"

	"gardenio/controller/internal/scheduler/conditions"
)

// Condition is the legacy single-leaf condition shape. The canonical
// definition (and all evaluation logic) now lives in
// gardenio/controller/internal/scheduler/conditions per
// gardenio/plans/epic-mention-looping-gardener.md §T1. This alias keeps
// existing call sites (cmd/gb/schedule.go, internal/bridge/...) compiling
// while new code reaches for the conditions.Expression DSL with all/any/not
// composition and field leaves.
type Condition = conditions.Condition

// ConditionClient is the daemon-side query interface needed to evaluate a
// Condition. Re-exported for backward compatibility.
type ConditionClient = conditions.BeadClient

// ParseCondition unmarshals a single Condition from a JSON string.
func ParseCondition(raw string) (Condition, error) {
	return conditions.ParseCondition(raw)
}

// ParseConditions unmarshals a JSON array of Conditions.
func ParseConditions(raw string) ([]Condition, error) {
	return conditions.ParseConditions(raw)
}

// EvaluateCondition runs a single Condition against the bead store. Returns
// (met, error). On query/parse failure it returns (false, error) so callers
// can decide whether to fail-open (proceed) or fail-closed (block).
func EvaluateCondition(ctx context.Context, client ConditionClient, c Condition, defaultProject string) (bool, error) {
	return conditions.EvaluateCondition(ctx, client, c, defaultProject)
}

// EvaluateConditions runs every Condition and returns (true, nil) only if
// they all pass. An empty slice returns (true, nil) — "no conditions" means
// "no constraint". On the first error or first failing condition it returns
// immediately.
func EvaluateConditions(ctx context.Context, client ConditionClient, conds []Condition, defaultProject string) (bool, error) {
	return conditions.EvaluateConditions(ctx, client, conds, defaultProject)
}

// CompareCount applies op to count vs the integer parsed from valueStr.
// Returns false on parse failure or unknown op.
func CompareCount(count int, op, valueStr string) bool {
	return conditions.CompareCount(count, op, valueStr)
}
