package conditions

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// TestParseExpression_LeafShapes covers the three leaf JSON forms from the
// plan-of-record §4.1: bead_query, gate, and field.
func TestParseExpression_LeafShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Expression
	}{
		{
			name: "bead_query leaf",
			raw:  `{"type":"bead_query","op":"==","value":"0","types":["task"],"statuses":["open"]}`,
			want: Expression{Leaf: Leaf{
				Type: "bead_query", Op: "==", Value: "0",
				Types: []string{"task"}, Statuses: []string{"open"},
			}},
		},
		{
			name: "gate leaf",
			raw:  `{"type":"gate","op":"absent","value":"deploy-freeze"}`,
			want: Expression{Leaf: Leaf{Type: "gate", Op: "absent", Value: "deploy-freeze"}},
		},
		{
			name: "field leaf with parent",
			raw:  `{"field":"parent","op":"==","value":"gd-X"}`,
			want: Expression{Leaf: Leaf{Field: "parent", Op: "==", Value: "gd-X"}},
		},
		{
			name: "field leaf with in operator",
			raw:  `{"field":"type","op":"in","values":["task","bug"]}`,
			want: Expression{Leaf: Leaf{Field: "type", Op: "in", Values: []string{"task", "bug"}}},
		},
		{
			name: "field leaf in_subtree_of",
			raw:  `{"field":"raised_by_agent.task_id","op":"in_subtree_of","value":"gd-EPIC"}`,
			want: Expression{Leaf: Leaf{Field: "raised_by_agent.task_id", Op: "in_subtree_of", Value: "gd-EPIC"}},
		},
		{
			name: "bead_query with parent filter (T2 placeholder)",
			raw:  `{"type":"bead_query","op":">","value":"0","parent":"gd-EPIC","types":["task","bug"]}`,
			want: Expression{Leaf: Leaf{
				Type: "bead_query", Op: ">", Value: "0",
				Parent: "gd-EPIC", Types: []string{"task", "bug"},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseExpression(tc.raw)
			if err != nil {
				t.Fatalf("ParseExpression: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// TestParseExpression_Compositions covers all/any/not nested arbitrarily
// deep, matching the §5.3 read-queue-drainer worked example.
func TestParseExpression_Compositions(t *testing.T) {
	raw := `{
		"all": [
			{"type":"bead_query","op":">","value":"0","types":["task"],"statuses":["open"]},
			{"any": [
				{"type":"active_agents","op":"<","value":"3","role":"crew"},
				{"type":"gate","op":"absent","value":"deploy-freeze"}
			]},
			{"not": {"field":"label:hold","op":"present"}}
		]
	}`
	got, err := ParseExpression(raw)
	if err != nil {
		t.Fatalf("ParseExpression: %v", err)
	}
	if !got.IsComposition() || len(got.All) != 3 {
		t.Fatalf("expected top-level all of 3, got %+v", got)
	}
	if got.All[0].Type != "bead_query" {
		t.Errorf("expected first child bead_query, got %+v", got.All[0])
	}
	if !got.All[1].IsComposition() || len(got.All[1].Any) != 2 {
		t.Errorf("expected second child to be any-of-2, got %+v", got.All[1])
	}
	if got.All[2].Not == nil || got.All[2].Not.Field != "label:hold" {
		t.Errorf("expected third child to be not(label:hold present), got %+v", got.All[2])
	}
}

// TestExpression_RoundTrip ensures a parsed expression marshals back to JSON
// that re-parses to the same Expression value (semantic round-trip).
func TestExpression_RoundTrip(t *testing.T) {
	cases := []string{
		`{"type":"bead_query","op":"==","value":"0","types":["task"]}`,
		`{"field":"parent","op":"==","value":"gd-X"}`,
		`{"all":[{"type":"gate","op":"absent","value":"freeze"},{"field":"label:cycle:failed","op":"present"}]}`,
		`{"any":[{"type":"bead_query","op":">","value":"0","types":["bug"]},{"not":{"field":"status","op":"==","value":"closed"}}]}`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			parsed, err := ParseExpression(raw)
			if err != nil {
				t.Fatalf("ParseExpression: %v", err)
			}
			marshalled, err := MarshalExpression(parsed)
			if err != nil {
				t.Fatalf("MarshalExpression: %v", err)
			}
			reparsed, err := ParseExpression(marshalled)
			if err != nil {
				t.Fatalf("ParseExpression(re): %v", err)
			}
			if !reflect.DeepEqual(parsed, reparsed) {
				t.Errorf("round-trip differs:\n  parsed:   %+v\n  reparsed: %+v\n  json:     %s",
					parsed, reparsed, marshalled)
			}
		})
	}
}

// TestParseCondition_LegacyShape pins backward compatibility: the legacy
// scheduler.Condition JSON shape continues to parse.
func TestParseCondition_LegacyShape(t *testing.T) {
	raw := `{"type":"bead_query","op":"==","value":"0","types":["task","bug"],"statuses":["open"],"labels":["project:gardenio"]}`
	c, err := ParseCondition(raw)
	if err != nil {
		t.Fatalf("ParseCondition: %v", err)
	}
	if c.Type != "bead_query" || c.Op != "==" || c.Value != "0" {
		t.Errorf("unexpected: %+v", c)
	}
	if len(c.Types) != 2 || c.Types[0] != "task" {
		t.Errorf("unexpected types: %+v", c.Types)
	}

	// Lower into Expression and verify equivalence to a direct field-by-field
	// Expression construction.
	got := c.ToExpression()
	want := Expression{Leaf: Leaf{
		Type: "bead_query", Op: "==", Value: "0",
		Types: []string{"task", "bug"}, Statuses: []string{"open"},
		Labels: []string{"project:gardenio"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ToExpression mismatch:\n  got  %+v\n  want %+v", got, want)
	}
}

// TestExpression_OmitsZeroFields keeps the marshalled output compact — the
// scheduler stores Expression JSON inside bead fields and the bead char
// budget is finite. Empty all/any/not slices, nil leafs, and zero-value
// strings must not render.
func TestExpression_OmitsZeroFields(t *testing.T) {
	e := Expression{Leaf: Leaf{Type: "gate", Op: "absent", Value: "x"}}
	out, err := MarshalExpression(e)
	if err != nil {
		t.Fatalf("MarshalExpression: %v", err)
	}
	// Decode generically and ensure only the populated keys are present.
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range []string{"all", "any", "not", "field", "labels", "types", "statuses", "values", "project", "role", "parent"} {
		if _, ok := m[forbidden]; ok {
			t.Errorf("expected %q to be omitted, got json=%s", forbidden, out)
		}
	}
	for _, want := range []string{"type", "op", "value"} {
		if _, ok := m[want]; !ok {
			t.Errorf("expected %q to be present, got json=%s", want, out)
		}
	}
}

// TestReferencesEventPayload covers the field-leaf detector that lets a store
// gate probe recognize an event-payload gate it cannot evaluate (ka-0000001670).
func TestReferencesEventPayload(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "field leaf",
			raw:  `{"field":"gate_passed","op":"==","value":"true"}`,
			want: true,
		},
		{
			name: "daemon bead_query leaf",
			raw:  `{"type":"bead_query","op":"==","value":"0"}`,
			want: false,
		},
		{
			name: "no-op leaf",
			raw:  `{"op":"==","value":"x"}`,
			want: false,
		},
		{
			name: "field leaf nested in all-composition",
			raw:  `{"all":[{"type":"bead_query","op":">","value":"0"},{"field":"source_step_id","op":"==","value":"emit"}]}`,
			want: true,
		},
		{
			name: "field leaf nested in any-composition",
			raw:  `{"any":[{"field":"status","op":"==","value":"closed"}]}`,
			want: true,
		},
		{
			name: "field leaf under not",
			raw:  `{"not":{"field":"type","op":"==","value":"task"}}`,
			want: true,
		},
		{
			name: "composition of only daemon leaves",
			raw:  `{"all":[{"type":"bead_query","op":">","value":"0"},{"type":"active_agents","op":"==","value":"0"}]}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := ParseExpression(tc.raw)
			if err != nil {
				t.Fatalf("ParseExpression(%q): %v", tc.raw, err)
			}
			if got := e.ReferencesEventPayload(); got != tc.want {
				t.Fatalf("ReferencesEventPayload()=%v, want %v (raw=%s)", got, tc.want, tc.raw)
			}
		})
	}
}

// TestParseExpressions_PreservesFieldLeaves is the regression guard for the
// root cause of ka-0000001670: the legacy ParseConditions drops the `field`
// key (its Condition struct has no Field member), collapsing every field-leaf
// condition to a no-op "no constraint" leaf. ParseExpressions must preserve it.
func TestParseExpressions_PreservesFieldLeaves(t *testing.T) {
	raw := `[{"field":"type","op":"==","value":"task"},{"field":"gate_passed","op":"==","value":"true"}]`

	exprs, err := ParseExpressions(raw)
	if err != nil {
		t.Fatalf("ParseExpressions: %v", err)
	}
	if len(exprs) != 2 {
		t.Fatalf("expected 2 expressions, got %d", len(exprs))
	}
	if exprs[0].Field != "type" || exprs[1].Field != "gate_passed" {
		t.Fatalf("field leaves not preserved: %+v", exprs)
	}
	for i, e := range exprs {
		if !e.ReferencesEventPayload() {
			t.Fatalf("expr[%d] should reference event payload: %+v", i, e)
		}
	}

	// Demonstrate the lossy legacy path this fix routes around: the legacy
	// Condition shape has no Field member, so both conditions parse to no-op
	// leaves (Type=="" && Field==""), which evalLeaf treats as "no constraint"
	// and evaluates TRUE — the phantom would_fire_now the fix guards against.
	conds, err := ParseConditions(raw)
	if err != nil {
		t.Fatalf("ParseConditions: %v", err)
	}
	for i, c := range conds {
		lf := c.ToExpression().Leaf
		if lf.Type != "" || lf.Field != "" {
			t.Fatalf("legacy cond[%d] unexpectedly retained a leaf discriminator: %+v", i, lf)
		}
	}
	met, err := EvaluateConditions(context.Background(), nil, conds, "monorepo")
	if err != nil {
		// A nil client short-circuits with an error before any leaf evaluates;
		// the point here is only that the field discriminators were dropped.
		return
	}
	if !met {
		t.Fatal("expected the lossy legacy parse to yield met=true (no-op leaves); the guard fix must not rely on the legacy path")
	}
}
