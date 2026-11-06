package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestCreateBead_InheritsCallerAgentTaints pins ADR §4.2 row 1: a bead
// created by a tainted agent carries that agent's taints immediately on
// creation, attributed source=inherit:create.
func TestCreateBead_InheritsCallerAgentTaints(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.taints = map[string][]int64{"gd-agent1": {1, 2}}

	ctx := setBoth(context.Background(), "slack:U_TEST", "")
	ctx = withTestPrivilegedAgent(ctx, "gd-agent1")
	bead, err := s.createBead(ctx, createBeadInput{Title: "tainted-agent create", Type: "task"})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}

	rows := ms.beadTaints[bead.ID]
	if len(rows) != 2 {
		t.Fatalf("expected 2 bead_taints rows, got %d (%v)", len(rows), rows)
	}
	for _, wantTaintID := range []int64{1, 2} {
		row, ok := beadTaintByID(rows, wantTaintID)
		if !ok {
			t.Fatalf("missing bead_taints row for taint %d: %v", wantTaintID, rows)
		}
		if row.Source != model.TaintSourceInheritCreate {
			t.Errorf("taint %d: Source = %q; want %q", wantTaintID, row.Source, model.TaintSourceInheritCreate)
		}
		if row.SourceBeadID != "gd-agent1" {
			t.Errorf("taint %d: SourceBeadID = %q; want %q", wantTaintID, row.SourceBeadID, "gd-agent1")
		}
	}
}

// TestCreateBead_InheritsParentTaints pins ADR §4.2 row 1's other half: a
// bead created with a parent (the atomic ParentBeadID create-time wiring —
// the server-level equivalent of `kd create --parent-task`) inherits the
// parent's taints even when the creating agent is untainted, attributed
// source=inherit:parent.
func TestCreateBead_InheritsParentTaints(t *testing.T) {
	s, ms, _ := newTestServer()

	// Untainted caller: an OIDC human carries no taints of its own.
	ctx := ctxAsTestSubject()

	parent, err := s.createBead(ctx, createBeadInput{Title: "parent", Type: "task"})
	if err != nil {
		t.Fatalf("creating parent bead: %v", err)
	}
	ms.taints = map[string][]int64{parent.ID: {5}}

	child, err := s.createBead(ctx, createBeadInput{
		Title:        "child",
		Type:         "task",
		ParentBeadID: parent.ID,
	})
	if err != nil {
		t.Fatalf("creating child bead: %v", err)
	}

	rows := ms.beadTaints[child.ID]
	if len(rows) != 1 {
		t.Fatalf("expected 1 bead_taints row on child, got %d (%v)", len(rows), rows)
	}
	row, ok := beadTaintByID(rows, 5)
	if !ok {
		t.Fatalf("missing bead_taints row for taint 5: %v", rows)
	}
	if row.Source != model.TaintSourceInheritParent {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritParent)
	}
	if row.SourceBeadID != parent.ID {
		t.Errorf("SourceBeadID = %q; want %q", row.SourceBeadID, parent.ID)
	}
}

// TestCreateBead_NoAutoInheritDoesNotSuppressTaintPropagation is the
// required regression test: X-No-Auto-Inherit (WithNoAutoInherit) controls
// Fields[] defaulting only — per ADR §4.2 there is no opt-out for taint
// propagation, so a tainted caller's new bead must still be tainted.
func TestCreateBead_NoAutoInheritDoesNotSuppressTaintPropagation(t *testing.T) {
	s, ms, _ := newTestServer()
	ms.taints = map[string][]int64{"gd-agent1": {9}}

	ctx := setBoth(context.Background(), "slack:U_TEST", "")
	ctx = withTestPrivilegedAgent(ctx, "gd-agent1")
	ctx = WithNoAutoInherit(ctx, true)

	bead, err := s.createBead(ctx, createBeadInput{Title: "opt-out header set", Type: "task"})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}

	rows := ms.beadTaints[bead.ID]
	row, ok := beadTaintByID(rows, 9)
	if !ok {
		t.Fatalf("expected taint 9 to propagate despite X-No-Auto-Inherit, got rows %v", rows)
	}
	if row.Source != model.TaintSourceInheritCreate {
		t.Errorf("Source = %q; want %q", row.Source, model.TaintSourceInheritCreate)
	}
}

// beadTaintByID finds the bead_taints row for one taint id. The mock store
// keys bead_taints by bead id and holds a SLICE (mirroring the real table,
// where (bead_id, taint_id) is unique but ordering is by taint name), so a
// lookup by taint id is a scan rather than a map index.
func beadTaintByID(rows []*model.BeadTaint, taintID int64) (*model.BeadTaint, bool) {
	for _, r := range rows {
		if r.TaintID == taintID {
			return r, true
		}
	}
	return nil, false
}
