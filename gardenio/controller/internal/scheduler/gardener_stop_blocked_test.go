package scheduler

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeBlockedStopDaemon implements GardenerStopDaemon AND the optional
// BatchGetDependencies/BatchGetBeads capability, so it exercises the full
// exclude_blocked_by_open_types path threaded from the StopEntry down into
// evalBeadQuery (ka-0000002198). It returns every configured row from
// ListBeadsFiltered and resolves each child's forward deps + blocker beads.
type fakeBlockedStopDaemon struct {
	rows        []*beadsapi.BeadDetail
	depsByChild map[string][]beadsapi.Dependency
	beadsByID   map[string]*beadsapi.BeadDetail
}

func (f *fakeBlockedStopDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{Beads: f.rows, Total: len(f.rows)}, nil
}

func (f *fakeBlockedStopDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	return f.beadsByID[id], nil
}

func (f *fakeBlockedStopDaemon) IsAncestor(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (f *fakeBlockedStopDaemon) BatchGetDependencies(_ context.Context, ids []string, _ string) (map[string][]beadsapi.Dependency, error) {
	out := make(map[string][]beadsapi.Dependency, len(ids))
	for _, id := range ids {
		out[id] = f.depsByChild[id]
	}
	return out, nil
}

func (f *fakeBlockedStopDaemon) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	for _, id := range ids {
		if b, ok := f.beadsByID[id]; ok {
			out[id] = b
		}
	}
	return out, nil
}

// TestGardenerStop_BeadQueryExcludeBlockedByOpenTypes is the end-to-end
// regression lock for ka-0000002198: a gardener whose epic's only open child is
// blocked on an open operator escalation should reach loop_end (==0) once its
// StopEntry opts into exclude_blocked_by_open_types, instead of looping forever
// and scoring STALE-STOP.
func TestGardenerStop_BeadQueryExcludeBlockedByOpenTypes(t *testing.T) {
	const (
		epic  = "gd-sd7Hxka19a"
		child = "gd-H6Jodnv2Ar"
		esc   = "ka-0000001499"
	)

	spec := StopSpec{LoopEnd: StopSlot{Any: []StopEntry{{
		Kind:                      StopKindBeadQuery,
		Op:                        "==",
		Value:                     "0",
		Parent:                    epic,
		Types:                     []string{"task", "bug", "epic"},
		Statuses:                  []string{"open", "in_progress", "blocked"},
		ExcludeBlockedByOpenTypes: []string{"escalation", "decision"},
	}}}}

	// RunsSpawned>0 so the never-started guard does not suppress the close.
	sctx := GardenerStopContext{BeadID: "gd-cap", Project: "gardenio", RunsSpawned: 3}

	t.Run("open child blocked by open escalation → loop_end fires (premature=false)", func(t *testing.T) {
		d := &fakeBlockedStopDaemon{
			rows:        []*beadsapi.BeadDetail{{ID: child}},
			depsByChild: map[string][]beadsapi.Dependency{child: {{BeadID: child, DependsOnID: esc, Type: "blocks"}}},
			beadsByID:   map[string]*beadsapi.BeadDetail{esc: {ID: esc, Type: "escalation", Status: "open"}},
		}
		match, fired := EvaluateGardenerStopSpec(context.Background(), d, nil, sctx, spec)
		if !fired {
			t.Fatal("expected loop_end to fire: the only open child is externally blocked, so effective count is 0")
		}
		if match.Slot != StopSlotLoopEnd || match.Kind != StopKindBeadQuery {
			t.Fatalf("unexpected match: %+v", match)
		}
		// bead_query is a terminal kind, so a loop_end match is not premature.
		if match.Premature {
			t.Error("bead_query loop_end match should be terminal (Premature=false)")
		}
	})

	t.Run("open child with live work → loop_end does NOT fire", func(t *testing.T) {
		d := &fakeBlockedStopDaemon{
			rows:        []*beadsapi.BeadDetail{{ID: child}},
			depsByChild: map[string][]beadsapi.Dependency{child: nil}, // unblocked, real work
		}
		if _, fired := EvaluateGardenerStopSpec(context.Background(), d, nil, sctx, spec); fired {
			t.Fatal("expected loop_end NOT to fire while an unblocked open child remains")
		}
	})
}
