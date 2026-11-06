package bridge

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// readerOnly is a DormancyReader over the trigger mock: its reads, plus a fixed
// forward-dependency table for MRBeadForTask.
type readerOnly struct {
	*mockTriggerDaemon
	deps map[string][]beadsapi.Dependency
}

func (r readerOnly) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	return r.deps[id], nil
}

// TestReconcileDormancyProbeReadOnly_SameVerdict: over a reader, the probe reaches
// the verdict the full daemon does, the live blocker rather than the stamp.
func TestReconcileDormancyProbeReadOnly_SameVerdict(t *testing.T) {
	m := wedgedFrontierDaemon("gd-esc-2")
	bead := dormancyProbeTrigger(t, GardenerSpec{AssignBead: "gd-epic-1", DormantOnEscalation: true, DormantBlocker: "gd-esc-old"})

	d := ReconcileDormancyProbeReadOnly(context.Background(), readerOnly{mockTriggerDaemon: m}, nil, bead)
	if d == nil || !d.Parked || d.LiveBlocker != "gd-esc-2" || !d.StampedBlockerStale {
		t.Fatalf("verdict = %+v, want parked on gd-esc-2 with the stamp gd-esc-old stale", d)
	}
	if len(m.updatedFields) != 0 || len(m.updatedTyped) != 0 || len(m.commentedIDs) != 0 {
		t.Errorf("the read-only probe wrote: fields %v, typed %v, comments %v", m.updatedFields, m.updatedTyped, m.commentedIDs)
	}
	if d := ReconcileDormancyProbeReadOnly(context.Background(), nil, nil, bead); d == nil || d.Evaluated || d.Parked {
		t.Errorf("nil reader: verdict = %+v, want not evaluated, not parked", d)
	}
}

// TestReadOnlyGardenerDaemon_RefusesWrites: every write the probe could reach
// returns ErrDormancyProbeReadOnly, and MRBeadForTask resolves a task's
// merge-request bead over the reader's reads.
func TestReadOnlyGardenerDaemon_RefusesWrites(t *testing.T) {
	m := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{
		{ID: "gd-doc", Type: "doc"},
		{ID: "gd-mr", Type: "merge-request"},
	}}
	d := readOnlyGardenerDaemon{readerOnly{mockTriggerDaemon: m, deps: map[string][]beadsapi.Dependency{
		"gd-task": {{DependsOnID: "gd-doc", Type: "produces"}, {DependsOnID: "gd-x", Type: "blocks"}, {DependsOnID: "gd-mr", Type: "produces"}},
	}}}
	ctx := context.Background()
	for name, err := range map[string]error{
		"UpdateBeadFields":      d.UpdateBeadFields(ctx, "gd-x", map[string]string{"a": "b"}),
		"UpdateBeadFieldsTyped": d.UpdateBeadFieldsTyped(ctx, "gd-x", map[string]any{"a": 1}),
		"AddComment":            d.AddComment(ctx, "gd-x", "me", "hi"),
		"AddLabel":              d.AddLabel(ctx, "gd-x", "l"),
		"CloseBead":             d.CloseBead(ctx, "gd-x", nil),
		"UpdateBead":            d.UpdateBead(ctx, "gd-x", beadsapi.UpdateBeadRequest{}),
	} {
		if !errors.Is(err, ErrDormancyProbeReadOnly) {
			t.Errorf("%s = %v, want ErrDormancyProbeReadOnly", name, err)
		}
	}
	if mr, err := d.MRBeadForTask(ctx, ""); mr != nil || err != nil {
		t.Errorf("MRBeadForTask(\"\") = %v, %v; want nil, nil", mr, err)
	}
	if mr, err := d.MRBeadForTask(ctx, "gd-task"); err != nil || mr == nil || mr.ID != "gd-mr" {
		t.Errorf("MRBeadForTask(gd-task) = %v, %v; want gd-mr", mr, err)
	}
}
