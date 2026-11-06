package scheduler

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// stubConditionClient records the last query it received and returns a
// caller-configured Total. Lets tests verify that Condition fields plumb
// through to the daemon-bound ListBeadsQuery (in particular the §T2
// `parent` filter on bead_query).
type stubConditionClient struct {
	total int
	lastQ beadsapi.ListBeadsQuery
	calls int
}

func (s *stubConditionClient) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	s.calls++
	s.lastQ = q
	return &beadsapi.ListBeadsResult{Total: s.total}, nil
}

// TestEvaluateBeadQuery_ParentPlumbsThrough verifies the bead_query condition
// pipes its `parent` field through to ListBeadsQuery.Parent so the daemon
// receives it as the `parent=<bead-id>` query param. Plan §T2:
// gardenio/plans/epic-mention-looping-gardener.md
func TestEvaluateBeadQuery_ParentPlumbsThrough(t *testing.T) {
	stub := &stubConditionClient{total: 3}
	cond := Condition{
		Type:   "bead_query",
		Op:     ">",
		Value:  "0",
		Parent: "gd-Epic1",
		Types:  []string{"task", "bug"},
	}
	met, err := EvaluateCondition(context.Background(), stub, cond, "")
	if err != nil {
		t.Fatalf("EvaluateCondition err: %v", err)
	}
	if !met {
		t.Fatal("expected condition met (3 > 0)")
	}
	if stub.lastQ.Parent != "gd-Epic1" {
		t.Errorf("Parent not plumbed: got %q want gd-Epic1", stub.lastQ.Parent)
	}
	if got := stub.lastQ.Types; len(got) != 2 || got[0] != "task" || got[1] != "bug" {
		t.Errorf("Types mismatch: %v", got)
	}
	// Default status filter must remain ["open"].
	if got := stub.lastQ.Statuses; len(got) != 1 || got[0] != "open" {
		t.Errorf("Statuses default mismatch: %v", got)
	}
}

// TestEvaluateBeadQuery_ParentExcludesSubtreeWhenZero locks in the
// "loop_end / exit_condition with parent" worked example: when the daemon
// reports zero open tasks under the epic subtree, the bead_query condition
// fires (=== 0) and a gardener loop's exit_condition is met.
func TestEvaluateBeadQuery_ParentExcludesSubtreeWhenZero(t *testing.T) {
	stub := &stubConditionClient{total: 0}
	cond := Condition{
		Type:     "bead_query",
		Op:       "==",
		Value:    "0",
		Parent:   "gd-Epic1",
		Types:    []string{"task"},
		Statuses: []string{"open", "in_progress"},
	}
	met, err := EvaluateCondition(context.Background(), stub, cond, "")
	if err != nil {
		t.Fatalf("EvaluateCondition err: %v", err)
	}
	if !met {
		t.Fatal("expected exit_condition met when subtree count == 0")
	}
	if stub.lastQ.Parent != "gd-Epic1" {
		t.Errorf("Parent not plumbed")
	}
}

// TestEvaluateBeadQuery_ParentIncludesSubtreeWhenNonzero locks in the
// reverse: with open work in the subtree, the gardener's exit_condition is
// NOT met and the loop continues.
func TestEvaluateBeadQuery_ParentIncludesSubtreeWhenNonzero(t *testing.T) {
	stub := &stubConditionClient{total: 5}
	cond := Condition{
		Type:   "bead_query",
		Op:     "==",
		Value:  "0",
		Parent: "gd-Epic1",
		Types:  []string{"task"},
	}
	met, err := EvaluateCondition(context.Background(), stub, cond, "")
	if err != nil {
		t.Fatalf("EvaluateCondition err: %v", err)
	}
	if met {
		t.Fatal("expected condition NOT met (5 != 0)")
	}
}

// TestParseCondition_ParentField round-trips the JSON shape used by
// schedule beads: {"type":"bead_query","parent":"gd-Epic1",...}.
func TestParseCondition_ParentField(t *testing.T) {
	raw := `{"type":"bead_query","op":"==","value":"0","parent":"gd-Epic1","types":["task"]}`
	c, err := ParseCondition(raw)
	if err != nil {
		t.Fatalf("ParseCondition err: %v", err)
	}
	if c.Parent != "gd-Epic1" {
		t.Fatalf("Parent not parsed: got %q", c.Parent)
	}
}
