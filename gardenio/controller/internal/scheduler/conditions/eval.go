package conditions

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// BeadClient is the daemon-side query interface needed to evaluate
// bead_query / gate / ready_queue / active_agents leaves. The production
// implementation is *beadsapi.Client; tests can stub it.
type BeadClient interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// AncestorClient resolves the in_subtree_of operator. Returns true when
// child is a descendant of parent via parent-child dependencies.
type AncestorClient interface {
	IsAncestor(ctx context.Context, parent, child string) (bool, error)
}

// Env carries the resources an Expression needs at evaluation time. Field
// leaves consult Payload (and optionally Ancestor for in_subtree_of); daemon
// leaves consult Bead.
type Env struct {
	// Bead is the daemon client for daemon-side leaves. Required for
	// evaluating any expression with a daemon leaf.
	Bead BeadClient
	// Ancestor resolves in_subtree_of. Required only when a field leaf uses
	// the in_subtree_of operator.
	Ancestor AncestorClient
	// DefaultProject is used when a daemon leaf's Project field is empty.
	DefaultProject string
	// Payload is the in-process value tree that field leaves match against.
	// Top-level keys are addressable as the leaf's Field path; dotted paths
	// walk nested map[string]any subtrees; "label:<name>" addresses
	// membership in payload["labels"] (which must be []string or
	// []interface{} of strings).
	Payload map[string]any
}

// Evaluate evaluates an Expression and returns (matched, error). On any
// transport / parse error from the daemon path, returns (false, error) so
// the caller can choose fail-open vs fail-closed semantics.
func Evaluate(ctx context.Context, e Expression, env Env) (bool, error) {
	if e.IsComposition() {
		return evalComposition(ctx, e, env)
	}
	return evalLeaf(ctx, e.Leaf, env)
}

func evalComposition(ctx context.Context, e Expression, env Env) (bool, error) {
	switch {
	case len(e.All) > 0:
		for i := range e.All {
			ok, err := Evaluate(ctx, e.All[i], env)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	case len(e.Any) > 0:
		for i := range e.Any {
			ok, err := Evaluate(ctx, e.Any[i], env)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case e.Not != nil:
		ok, err := Evaluate(ctx, *e.Not, env)
		if err != nil {
			return false, err
		}
		return !ok, nil
	}
	// Should be unreachable when IsComposition() is true.
	return false, fmt.Errorf("conditions: empty composition")
}

func evalLeaf(ctx context.Context, l Leaf, env Env) (bool, error) {
	switch {
	case l.Type == "" && l.Field == "":
		// No-op leaf: "no constraint" → satisfied.
		return true, nil
	case l.Type != "":
		// Daemon-side leaves are addressed by Type. Some daemon leaves
		// (e.g. "acceptance_metric") use Field as a sub-selector.
		return evalDaemonLeaf(ctx, l, env)
	default:
		return evalFieldLeaf(ctx, l, env)
	}
}

// EvaluateCondition runs a single legacy Condition against the bead store.
// Preserved for backward compatibility with scheduler.EvaluateCondition.
func EvaluateCondition(ctx context.Context, client BeadClient, c Condition, defaultProject string) (bool, error) {
	if client == nil {
		return false, fmt.Errorf("nil condition client")
	}
	return Evaluate(ctx, c.ToExpression(), Env{Bead: client, DefaultProject: defaultProject})
}

// EvaluateConditions runs every legacy Condition with AND semantics.
// An empty slice returns (true, nil). Preserved for backward compatibility
// with scheduler.EvaluateConditions.
func EvaluateConditions(ctx context.Context, client BeadClient, cs []Condition, defaultProject string) (bool, error) {
	for _, c := range cs {
		ok, err := EvaluateCondition(ctx, client, c, defaultProject)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// CompareCount applies op to count vs the integer parsed from valueStr.
// Returns false on parse failure or unknown op.
func CompareCount(count int, op, valueStr string) bool {
	threshold, err := strconv.Atoi(strings.TrimSpace(valueStr))
	if err != nil {
		return false
	}
	switch op {
	case ">":
		return count > threshold
	case ">=":
		return count >= threshold
	case "<":
		return count < threshold
	case "<=":
		return count <= threshold
	case "==", "eq":
		return count == threshold
	case "!=", "ne":
		return count != threshold
	default:
		return false
	}
}
