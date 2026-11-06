package conditions

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// evalFieldLeaf evaluates a field leaf against env.Payload.
//
// Operators:
//
//	==              equal (string compare on field value)
//	!=              not equal
//	in              field value matches one of Values
//	not_in          field value matches none of Values
//	contains        Value is a substring of field value (string-typed only)
//	present         field path resolves
//	absent          field path does NOT resolve
//	in_subtree_of   IsAncestor(Value, fieldValue) — field value is a
//	                descendant of the bead id Value via parent-child deps
func evalFieldLeaf(ctx context.Context, l Leaf, env Env) (bool, error) {
	value, present := lookupField(l.Field, env.Payload)
	switch l.Op {
	case "present":
		return present, nil
	case "absent":
		return !present, nil
	}
	if !present {
		// All non-present-style ops over a missing field are false. This
		// matches the "match against event payload" semantics from the
		// plan-of-record §4.1: if the trigger is gated on a field the
		// payload doesn't carry, the trigger does not fire.
		return false, nil
	}
	switch l.Op {
	case "==":
		return fieldString(value) == l.Value, nil
	case "!=":
		return fieldString(value) != l.Value, nil
	case "in":
		return inSet(fieldString(value), l.Values), nil
	case "not_in":
		return !inSet(fieldString(value), l.Values), nil
	case "contains":
		return strings.Contains(fieldString(value), l.Value), nil
	case "in_subtree_of":
		if env.Ancestor == nil {
			return false, fmt.Errorf("conditions: in_subtree_of requires an AncestorClient in env")
		}
		child := fieldString(value)
		if child == "" || l.Value == "" {
			return false, nil
		}
		return env.Ancestor.IsAncestor(ctx, l.Value, child)
	default:
		return false, fmt.Errorf("conditions: unknown field op %q", l.Op)
	}
}

// lookupField resolves a field path against the payload.
//
//   - "label:<name>" looks up membership of <name> in payload["labels"]
//     (as []string or []interface{} of strings). Returns (<name>, true) when
//     present so the value compare degenerates correctly for present/absent.
//   - "a.b.c" walks nested map[string]any.
//   - "a" reads payload["a"].
//
// Returns (value, true) when the path resolves; (nil, false) otherwise.
func lookupField(path string, payload map[string]any) (any, bool) {
	if payload == nil {
		return nil, false
	}
	if name, ok := strings.CutPrefix(path, "label:"); ok {
		raw, exists := payload["labels"]
		if !exists {
			return nil, false
		}
		switch labels := raw.(type) {
		case []string:
			if slices.Contains(labels, name) {
				return name, true
			}
		case []any:
			for _, l := range labels {
				if s, ok := l.(string); ok && s == name {
					return name, true
				}
			}
		}
		return nil, false
	}
	parts := strings.Split(path, ".")
	var cur any = payload
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, exists := m[p]
		if !exists {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// fieldString coerces a payload value to a string for comparison.
// Numeric and bool values are formatted; nested types stringify via fmt.
func fieldString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}

func inSet(needle string, set []string) bool {
	return slices.Contains(set, needle)
}
