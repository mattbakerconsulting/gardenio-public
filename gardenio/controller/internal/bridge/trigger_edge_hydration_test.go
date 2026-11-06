package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// edgeHydrDaemon is a minimal scheduler.TriggerDaemon (BeadClient +
// AncestorClient) that ALSO implements GetBead, so it satisfies the bridge
// beadHydrator interface used by the edge-event handlers. It serves fixed bead
// details for GetBead and resolves in_subtree_of by walking a parent map.
// Uniquely named to avoid colliding with other fakes in the package.
type edgeHydrDaemon struct {
	beads  map[string]*beadsapi.BeadDetail
	parent map[string]string // child id -> parent id, for IsAncestor
}

func (f *edgeHydrDaemon) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	b := f.beads[id]
	if b == nil {
		return nil, fmt.Errorf("bead %s not found", id)
	}
	return b, nil
}

// IsAncestor reports whether root is an ancestor of child via the parent map.
// Satisfies conditions.AncestorClient (used by the in_subtree_of leaf).
func (f *edgeHydrDaemon) IsAncestor(ctx context.Context, root, child string) (bool, error) {
	cur := child
	for {
		p, ok := f.parent[cur]
		if !ok || p == "" {
			return false, nil
		}
		if p == root {
			return true, nil
		}
		cur = p
	}
}

// ListBeadsFiltered satisfies conditions.BeadClient / TriggerBeadClient. The
// edge tests drive Evaluate with hand-built TriggerDefs, so this returns empty.
func (f *edgeHydrDaemon) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{}, nil
}

func edgeHydrDepFrame(t *testing.T, beadID, dependsOn string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"dependency": map[string]any{
			"bead_id":       beadID,
			"depends_on_id": dependsOn,
		},
	})
	if err != nil {
		t.Fatalf("marshal dep frame: %v", err)
	}
	return string(b)
}

func edgeHydrTrig(id string, entry scheduler.TriggerEntry) TriggerDef {
	return TriggerDef{
		ID:       id,
		Project:  "test",
		Triggers: []scheduler.TriggerEntry{entry},
	}
}

// edgeHydrEmit registers the multi source's SSE handlers on a fresh stream and
// dispatches a single edge-event frame, exercising the hydration path. The
// dispatch signature is dispatch(ctx, id, topic, data); id is unused here.
func edgeHydrEmit(src *MultiTriggerSource, topic, data string) {
	stream := NewSSEStream(SSEStreamConfig{})
	src.RegisterSSE(stream)
	stream.dispatch(context.Background(), "evt-1", topic, data)
}

// TestMultiSourceEdgeEventHydratedInSubtree asserts that a dependency.added
// edge event for a bead whose hydrated id sits in the gardener's watched subtree
// fires a match — and that a label/type leaf (only satisfiable from hydrated
// data) is honored. Pre-fix, the SSE handler fed an id-only payload with nil
// labels, so neither leaf could match.
func TestMultiSourceEdgeEventHydratedInSubtree(t *testing.T) {
	src := NewMultiTriggerSource(nil, nil)
	src.daemon = &edgeHydrDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"bd-child": {ID: "bd-child", Type: "task", Status: "open", Labels: []string{"area:demo"}},
		},
		parent: map[string]string{"bd-child": "bd-root"},
	}

	entry := scheduler.TriggerEntry{
		Kind:   scheduler.TriggerKindBeadEvent,
		Events: []string{"dependency.added"},
		Match: &conditions.Expression{All: []conditions.Expression{
			{Leaf: conditions.Leaf{Field: "id", Op: "in_subtree_of", Value: "bd-root"}},
			{Leaf: conditions.Leaf{Field: "type", Op: "==", Value: "task"}},
			{Leaf: conditions.Leaf{Field: "label:area:demo", Op: "present"}},
		}},
	}
	trig := edgeHydrTrig("trig-in", entry)

	edgeHydrEmit(src, "beads.dependency.added", edgeHydrDepFrame(t, "bd-child", "bd-other"))

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match after hydration, got %d", len(matches))
	}
}

// TestMultiSourceEdgeEventOutOfSubtree asserts that an edge event for a bead
// whose hydrated id is NOT under the watched subtree root does not fire.
func TestMultiSourceEdgeEventOutOfSubtree(t *testing.T) {
	src := NewMultiTriggerSource(nil, nil)
	src.daemon = &edgeHydrDaemon{
		beads:  map[string]*beadsapi.BeadDetail{"bd-orphan": {ID: "bd-orphan", Type: "task", Status: "open"}},
		parent: map[string]string{}, // no ancestry
	}

	entry := scheduler.TriggerEntry{
		Kind:   scheduler.TriggerKindBeadEvent,
		Events: []string{"dependency.added"},
		Match:  &conditions.Expression{Leaf: conditions.Leaf{Field: "id", Op: "in_subtree_of", Value: "bd-root"}},
	}
	trig := edgeHydrTrig("trig-out", entry)

	edgeHydrEmit(src, "beads.dependency.added", edgeHydrDepFrame(t, "bd-orphan", "bd-x"))

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches out of subtree, got %d", len(matches))
	}
}

// TestMultiSourceEdgeEventHydrationFailsOpen asserts that hydration failures
// (GetBead error, and a nil daemon) fail open: no panic, no buffered event, no
// match — rather than firing an unmatchable id-only event.
func TestMultiSourceEdgeEventHydrationFailsOpen(t *testing.T) {
	entry := scheduler.TriggerEntry{
		Kind:   scheduler.TriggerKindBeadEvent,
		Events: []string{"dependency.added"},
		Match:  &conditions.Expression{Leaf: conditions.Leaf{Field: "id", Op: "in_subtree_of", Value: "bd-root"}},
	}
	trig := edgeHydrTrig("trig-fail", entry)

	// (a) GetBead returns an error (bead not in the fake's map).
	srcErr := NewMultiTriggerSource(nil, nil)
	srcErr.daemon = &edgeHydrDaemon{beads: map[string]*beadsapi.BeadDetail{}}
	edgeHydrEmit(srcErr, "beads.dependency.added", edgeHydrDepFrame(t, "bd-missing", "bd-x"))
	if got := srcErr.PendingCount(); got != 0 {
		t.Fatalf("expected 0 buffered events on GetBead error, got %d", got)
	}
	matches, err := srcErr.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("evaluate (err daemon): %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches on GetBead error, got %d", len(matches))
	}

	// (b) nil daemon — handler must skip without panicking and buffer nothing.
	srcNil := NewMultiTriggerSource(nil, nil)
	edgeHydrEmit(srcNil, "beads.dependency.added", edgeHydrDepFrame(t, "bd-child", "bd-x"))
	if got := srcNil.PendingCount(); got != 0 {
		t.Fatalf("expected 0 buffered events with nil daemon, got %d", got)
	}
}

// TestBeadStateEdgeEventHydratedMatch covers the parallel hydration path in the
// BeadStateTriggerSource: a label.added edge event for a bead hydrated with a
// matching type/label fires; an out-of-condition bead does not; and a GetBead
// error fails open (nothing buffered).
func TestBeadStateEdgeEventHydratedMatch(t *testing.T) {
	mkSrc := func(d scheduler.TriggerDaemon) (*BeadStateTriggerSource, *SSEStream) {
		s := NewBeadStateTriggerSource(nil)
		s.daemon = d
		stream := NewSSEStream(SSEStreamConfig{})
		s.RegisterSSE(stream)
		return s, stream
	}
	frame := func(beadID string) string {
		b, _ := json.Marshal(map[string]any{"bead_id": beadID})
		return string(b)
	}

	daemon := &edgeHydrDaemon{beads: map[string]*beadsapi.BeadDetail{
		"bd-1": {ID: "bd-1", Type: "task", Status: "open", Labels: []string{"area:demo"}},
		"bd-2": {ID: "bd-2", Type: "bug", Status: "closed"},
	}}

	// Matching condition: type=task AND label area:demo present.
	conds := []beadCondition{
		{Field: "type", Op: "==", Value: "task"},
		{Field: "label:area:demo", Op: "present"},
	}
	trig := TriggerDef{ID: "bt-1", Project: "test", SourceData: beadTriggerData{
		EventTypes: []string{"label.added"},
		Conditions: conds,
	}}

	// (a) in-condition bead fires.
	src, stream := mkSrc(daemon)
	stream.dispatch(context.Background(), "evt", "beads.label.added", frame("bd-1"))
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match for hydrated in-condition bead, got %d", len(matches))
	}

	// (b) out-of-condition bead (wrong type, missing label) does not fire.
	src2, stream2 := mkSrc(daemon)
	stream2.dispatch(context.Background(), "evt", "beads.label.added", frame("bd-2"))
	matches2, err := src2.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(matches2) != 0 {
		t.Fatalf("expected 0 matches for out-of-condition bead, got %d", len(matches2))
	}

	// (c) GetBead error fails open: nothing buffered, no panic.
	src3, stream3 := mkSrc(&edgeHydrDaemon{beads: map[string]*beadsapi.BeadDetail{}})
	stream3.dispatch(context.Background(), "evt", "beads.label.added", frame("bd-missing"))
	if got := src3.PendingCount(); got != 0 {
		t.Fatalf("expected 0 buffered events on GetBead error, got %d", got)
	}
}
