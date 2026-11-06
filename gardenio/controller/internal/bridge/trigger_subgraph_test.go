package bridge

import (
	"context"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestBeadStateTriggerSource_InSubgraphOf verifies the new daemon-backed
// `in_subgraph_of` condition operator: a gardener that watches an epic root
// fires when ANY bead in the connected subgraph of that root mutates, but
// not for unrelated beads. This is the acceptance criterion in ka-0000007345
// — "wake when ANY bead in the connected subgraph of my demo bead mutates".
func TestBeadStateTriggerSource_InSubgraphOf(t *testing.T) {
	// Graph:
	//   gd-root          (the gardener's demo bead)
	//     parent-child → gd-child
	//                      blocks → gd-lq6hamdgda
	//   gd-mentions-root (mentions gd-root in description, reverse-mention edge)
	//   gd-unrelated     (no edge to gd-root)
	client := &fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"gd-root":  {dep("gd-root", "gd-child", "parent-child")},
			"gd-child": {dep("gd-child", "gd-lq6hamdgda", "blocks")},
		},
		mentionedBy: map[string][]beadsapi.BeadReferenceSummary{
			"gd-root": {ref("gd-mentions-root")},
		},
	}
	resolver := NewSubgraphResolver(client, SubgraphResolverConfig{MaxDepth: 4})

	src := NewBeadStateTriggerSource(slog.Default())
	src.SetSubgraphResolver(resolver)

	// Push events for every bead — only the ones inside the subgraph should match.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-root", Type: "task", Status: "in_progress"}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-child", Type: "task", Status: "in_progress"}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-lq6hamdgda", Type: "task", Status: "in_progress"}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-mentions-root", Type: "task", Status: "in_progress"}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-unrelated", Type: "task", Status: "in_progress"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID:            "gardener-on-root",
		Source:        "bead",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: beadTriggerData{
			Conditions: []beadCondition{
				// Default Field=="" resolves to the bead's id.
				{Op: "in_subgraph_of", Value: "gd-root"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	matched := map[string]bool{}
	for _, m := range matches {
		matched[m.Fields["source_bead_id"]] = true
	}

	wantMatch := []string{"gd-root", "gd-child", "gd-lq6hamdgda", "gd-mentions-root"}
	for _, id := range wantMatch {
		if !matched[id] {
			t.Errorf("expected %s to match in_subgraph_of:gd-root; matched=%v", id, matched)
		}
	}
	if matched["gd-unrelated"] {
		t.Errorf("gd-unrelated must NOT match in_subgraph_of:gd-root; matched=%v", matched)
	}
	if len(matches) != len(wantMatch) {
		t.Errorf("expected %d matches, got %d (%v)", len(wantMatch), len(matches), matched)
	}
}

// TestBeadStateTriggerSource_NotInSubgraphOf verifies the inverse operator.
func TestBeadStateTriggerSource_NotInSubgraphOf(t *testing.T) {
	client := &fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"gd-root": {dep("gd-root", "gd-inside", "parent-child")},
		},
	}
	resolver := NewSubgraphResolver(client, SubgraphResolverConfig{MaxDepth: 4})

	src := NewBeadStateTriggerSource(slog.Default())
	src.SetSubgraphResolver(resolver)

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-inside", Type: "task", Status: "in_progress"}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-outside", Type: "task", Status: "in_progress"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID:            "gardener-not-on-root",
		Source:        "bead",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: beadTriggerData{
			Conditions: []beadCondition{
				{Op: "not_in_subgraph_of", Value: "gd-root"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].Fields["source_bead_id"] != "gd-outside" {
		t.Errorf("not_in_subgraph_of expected only gd-outside to match; got %v", matches)
	}
}

// TestBeadStateTriggerSource_InSubgraphOfNoResolver verifies the trigger
// fails closed (does not fire) when no resolver is wired — operator
// misconfiguration should not silently turn the trigger into a fire-on-every-
// event hook.
func TestBeadStateTriggerSource_InSubgraphOfNoResolver(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	// Deliberately no SetSubgraphResolver call.

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-any", Type: "task", Status: "in_progress"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID:            "gardener-misconfigured",
		Source:        "bead",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: beadTriggerData{
			Conditions: []beadCondition{
				{Op: "in_subgraph_of", Value: "gd-root"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("missing resolver must fail closed (no matches); got %d", len(matches))
	}
}

// TestBeadStateTriggerSource_InSubgraphOfComposedWithOtherOps verifies the
// new operator composes with existing label / field checks via AND semantics
// (all conditions must hold). This is the realistic gardener wiring: "wake
// when something in my subgraph mutates AND it carries project:X".
func TestBeadStateTriggerSource_InSubgraphOfComposedWithOtherOps(t *testing.T) {
	client := &fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"gd-root": {
				dep("gd-root", "gd-a", "parent-child"),
				dep("gd-root", "gd-b", "parent-child"),
			},
		},
	}
	resolver := NewSubgraphResolver(client, SubgraphResolverConfig{MaxDepth: 4})

	src := NewBeadStateTriggerSource(slog.Default())
	src.SetSubgraphResolver(resolver)

	src.mu.Lock()
	src.pending = []beadStateEvent{
		// gd-a is in subgraph AND has project:gardenio → match
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-a", Type: "task", Labels: []string{"project:gardenio"}, Status: "in_progress"}},
		// gd-b is in subgraph but NOT project:gardenio → no match
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-b", Type: "task", Labels: []string{"project:other"}, Status: "in_progress"}},
		// gd-c has project:gardenio but is OUTSIDE the subgraph → no match
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-c", Type: "task", Labels: []string{"project:gardenio"}, Status: "in_progress"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID:            "gardener-scoped",
		Source:        "bead",
		Action:        TriggerActionNotify,
		MaxConcurrent: 10,
		SourceData: beadTriggerData{
			Conditions: []beadCondition{
				{Op: "in_subgraph_of", Value: "gd-root"},
				{Field: "label:project:gardenio", Op: "present"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].Fields["source_bead_id"] != "gd-a" {
		t.Errorf("expected only gd-a (in subgraph AND project:gardenio); got %v", matches)
	}
}
