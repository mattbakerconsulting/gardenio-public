//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"gardenio/kbeads/internal/idgen"
	"gardenio/kbeads/internal/model"
)

// TestParentSubtreeReverseChildCount_RealPostgres reproduces the false
// STALE-STOP bug (ka-0000000208) end-to-end against a real Postgres backend
// and proves the fix: counting an epic's children via the REVERSE
// parent-child query (BeadFilter.ParentSubtreeID, surfaced as the HTTP
// ?parent= param and `kd list --parent <epic>`) returns the epic's actual
// children — NOT its own forward depends_on edges.
//
// The bug: the meta-gardener scorer read an epic's OWN forward dependencies
// (`kd dep list <epic>` → GetDependencies → the edges the epic itself
// declares) to count children. For an epic that carries an UPWARD
// parent-child edge to a grandparent, that forward read returns the
// grandparent and ZERO children, so a healthy gardener was scored
// STALE-STOP ("0 open + 0 closed children — no work").
//
// The fixture mirrors ka-0000000867 exactly:
//
//	grandparent (ka-0000000867 → ka-0000004667)
//	  ▲ parent-child   (epic's OWN forward edge points UP — the trap)
//	  │
//	epic  ◄── parent-child ── childOpen        (children point AT the epic;
//	      ◄── parent-child ── childInProgress    REVERSE edges, surfaced
//	      ◄── parent-child ── childBlocked       under "Mentioned by")
//	      ◄── parent-child ── childClosed
//	            │
//	            ▼ parent-child
//	          grandchild (open)  ── transitive descendant, also counted
//
// Gated by `//go:build integration` + BEADS_TEST_DATABASE_URL so the default
// sqlmock-only `go test ./...` workflow is unaffected (per CLAUDE.md). Run:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestParentSubtreeReverseChildCount_RealPostgres
func TestParentSubtreeReverseChildCount_RealPostgres(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}

	store, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	suffix, err := idgen.Generate()
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	fleet := "test-parent-subtree-" + suffix
	now := time.Now().UTC()

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	genID := func() string {
		id, err := idgen.Generate()
		if err != nil {
			t.Fatalf("idgen: %v", err)
		}
		return id
	}

	mkBead := func(title string, typ model.BeadType, status model.Status) string {
		t.Helper()
		b := &model.Bead{
			ID: genID(), Kind: model.KindIssue, Type: typ,
			Title: title, Status: status,
			CreatedAt: now, UpdatedAt: now, Fleet: fleet, OperatedByID: &operatorID,
		}
		if status == model.StatusClosed {
			b.ClosedAt = &now
		}
		if err := store.CreateBead(ctx, b); err != nil {
			t.Fatalf("create bead %q: %v", title, err)
		}
		return b.ID
	}

	// link wires `child` --parent-child--> `parent` (the edge row that
	// `kd children <parent>` / ParentSubtreeID walks in reverse).
	link := func(child, parent string) {
		t.Helper()
		dep := &model.Dependency{
			BeadID: child, DependsOnID: parent,
			Type: model.DepParentChild, CreatedAt: now, OperatedByID: &operatorID,
		}
		if err := store.AddDependency(ctx, dep); err != nil {
			t.Fatalf("add parent-child dep %s->%s: %v", child, parent, err)
		}
	}

	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			`UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE fleet = $1`, fleet)
	})

	// Build the topology.
	grandparent := mkBead("grandparent epic", model.TypeEpic, model.StatusClosed)
	epic := mkBead("phase-4 epic (upward edge to grandparent)", model.TypeEpic, model.StatusOpen)
	// The TRAP: the epic's OWN forward edge points UP at the grandparent.
	link(epic, grandparent)

	childOpen := mkBead("child open", model.TypeTask, model.StatusOpen)
	childInProgress := mkBead("child in_progress", model.TypeTask, model.StatusInProgress)
	childBlocked := mkBead("child blocked", model.TypeBug, model.StatusBlocked)
	childClosed := mkBead("child closed", model.TypeTask, model.StatusClosed)
	link(childOpen, epic)
	link(childInProgress, epic)
	link(childBlocked, epic)
	link(childClosed, epic)

	// A transitive grandchild under childOpen — the recursive CTE must reach it.
	grandchild := mkBead("grandchild open", model.TypeTask, model.StatusOpen)
	link(grandchild, childOpen)

	// --- Proof 1: the BROKEN forward read returns the grandparent, not children.
	// This is exactly what `kd dep list <epic>` did → 0 children → false STALE-STOP.
	fwd, err := store.GetDependencies(ctx, epic)
	if err != nil {
		t.Fatalf("GetDependencies(epic): %v", err)
	}
	if len(fwd) != 1 || fwd[0].DependsOnID != grandparent {
		t.Fatalf("forward deps of epic = %+v; want exactly the upward edge to grandparent %s "+
			"(this is the trap the scorer fell into)", fwd, grandparent)
	}

	// --- Proof 2: the FIXED reverse query (ParentSubtreeID) returns the real
	// children + transitive grandchild, and NEVER the epic itself or the
	// grandparent above it.
	listSubtree := func(t *testing.T, statuses []model.Status) map[string]model.Status {
		t.Helper()
		beads, _, err := store.ListBeads(ctx, model.BeadFilter{
			Fleet:           fleet,
			ParentSubtreeID: epic,
			Status:          statuses,
			Limit:           100,
		})
		if err != nil {
			t.Fatalf("ListBeads(ParentSubtreeID=epic): %v", err)
		}
		got := make(map[string]model.Status, len(beads))
		for _, b := range beads {
			got[b.ID] = b.Status
		}
		return got
	}

	t.Run("all_descendants_counted_not_self_not_grandparent", func(t *testing.T) {
		got := listSubtree(t, nil)
		// 4 direct children + 1 grandchild = 5 descendants.
		mustContain(t, mapToBool(got), childOpen, childInProgress, childBlocked, childClosed, grandchild)
		// The epic must NOT count itself (CTE `id <> root`)...
		mustNotContain(t, mapToBool(got), epic)
		// ...and must NEVER surface the grandparent it points UP at — the
		// exact false-0 the bug produced.
		mustNotContain(t, mapToBool(got), grandparent)
		if len(got) != 5 {
			t.Errorf("descendant count = %d, want 5 (4 children + 1 grandchild); got %v", len(got), got)
		}
	})

	t.Run("open_inprogress_blocked_children_nonzero", func(t *testing.T) {
		// The gardener-dormancy gate counts open|in_progress|blocked. With the
		// broken forward read this was 0 (false STALE-STOP). It must now be 4
		// (childOpen, childInProgress, childBlocked, grandchild).
		got := listSubtree(t, []model.Status{
			model.StatusOpen, model.StatusInProgress, model.StatusBlocked,
		})
		mustContain(t, mapToBool(got), childOpen, childInProgress, childBlocked, grandchild)
		mustNotContain(t, mapToBool(got), childClosed) // closed excluded by status filter
		if len(got) != 4 {
			t.Errorf("open|in_progress|blocked descendant count = %d, want 4 (NOT 0 — "+
				"the false STALE-STOP premise); got %v", len(got), got)
		}
	})

	t.Run("closed_children_counted", func(t *testing.T) {
		got := listSubtree(t, []model.Status{model.StatusClosed})
		mustContain(t, mapToBool(got), childClosed)
		if len(got) != 1 {
			t.Errorf("closed descendant count = %d, want 1; got %v", len(got), got)
		}
	})
}

// mapToBool adapts a status map to the bool-valued shape mustContain/
// mustNotContain (defined in queries_has_report_integration_test.go) expect.
func mapToBool(m map[string]model.Status) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
