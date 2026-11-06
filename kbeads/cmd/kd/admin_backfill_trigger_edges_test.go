package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

var triggerBase = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// teAgent builds a type=agent bead carrying a name plus optional
// schedule_id/task_id, created at offsetSec from a fixed base.
func teAgent(id, name, schedID, taskID string, offsetSec int) *model.Bead {
	fields := map[string]any{"agent": name}
	if schedID != "" {
		fields["schedule_id"] = schedID
	}
	if taskID != "" {
		fields["task_id"] = taskID
	}
	raw, _ := json.Marshal(fields)
	return &model.Bead{
		ID:        id,
		Type:      "agent",
		Kind:      model.KindConfig,
		CreatedAt: triggerBase.Add(time.Duration(offsetSec) * time.Second),
		Fields:    raw,
	}
}

// teMolecule builds a type=molecule bead with the given created_by, optional
// extra fields, created at offsetSec from the base.
func teMolecule(id, createdBy string, offsetSec int, extra map[string]any) *model.Bead {
	raw, _ := json.Marshal(extra)
	return &model.Bead{
		ID:        id,
		Type:      "molecule",
		Kind:      model.KindIssue,
		CreatedBy: createdBy,
		CreatedAt: triggerBase.Add(time.Duration(offsetSec) * time.Second),
		Fields:    raw,
	}
}

func TestResolveMoleculeTrigger_ScheduleWinsOverTask(t *testing.T) {
	// A scheduled gardener's agent carries BOTH schedule_id and task_id; the
	// schedule is the canonical trigger, so triggered-by → schedule must win.
	index := buildAgentTriggerIndex([]*model.Bead{
		teAgent("gd-a1", "sched-123", "gd-sched", "gd-schedtask", 10),
	})
	mol := teMolecule("gd-m1", "sched-123", 100, nil)
	plan := resolveMoleculeTrigger(mol, index)
	if plan.Status != triggerWouldAdd {
		t.Fatalf("status = %q, want WOULD_ADD", plan.Status)
	}
	if plan.EdgeType != "triggered-by" || plan.TargetID != "gd-sched" {
		t.Fatalf("edge = %s → %s, want triggered-by → gd-sched", plan.EdgeType, plan.TargetID)
	}
	if plan.Class != "sched-via-agent.schedule_id" {
		t.Fatalf("class = %q", plan.Class)
	}
}

func TestResolveMoleculeTrigger_AgentSpawnParentChild(t *testing.T) {
	index := buildAgentTriggerIndex([]*model.Bead{
		teAgent("gd-a1", "gardenio-worker-abc", "", "gd-captask", 10),
	})
	mol := teMolecule("gd-m1", "gardenio-worker-abc", 100, nil)
	plan := resolveMoleculeTrigger(mol, index)
	if plan.Status != triggerWouldAdd {
		t.Fatalf("status = %q, want WOULD_ADD", plan.Status)
	}
	if plan.EdgeType != "parent-child" || plan.TargetID != "gd-captask" {
		t.Fatalf("edge = %s → %s, want parent-child → gd-captask", plan.EdgeType, plan.TargetID)
	}
	if plan.Class != "agent-spawn-via-agent.task_id" {
		t.Fatalf("class = %q", plan.Class)
	}
}

func TestResolveMoleculeTrigger_SkipStaticAndHuman(t *testing.T) {
	index := buildAgentTriggerIndex([]*model.Bead{
		teAgent("gd-a1", "sched-123", "gd-sched", "", 10),
	})
	cases := []struct {
		name string
		mol  *model.Bead
	}{
		{"static", teMolecule("gd-m1", "static", 100, nil)},
		{"empty", teMolecule("gd-m2", "", 100, nil)},
		{"human-direct", teMolecule("gd-m3", "Matthew.Baker", 100, nil)},
		{"unknown-agent", teMolecule("gd-m4", "some-agent-with-no-bead", 100, nil)},
	}
	for _, c := range cases {
		plan := resolveMoleculeTrigger(c.mol, index)
		if plan.Status != triggerSkipUnresolved {
			t.Errorf("%s: status = %q, want SKIP_UNRESOLVED", c.name, plan.Status)
		}
	}

	// A molecule that resolves to a scheduled agent → WOULD_ADD (no field-stamp
	// shadowing exists any more; idempotency is decided by the live edge check).
	resolvable := teMolecule("gd-m5", "sched-123", 100, nil)
	if plan := resolveMoleculeTrigger(resolvable, index); plan.Status != triggerWouldAdd {
		t.Errorf("resolvable: status = %q, want WOULD_ADD", plan.Status)
	}
}

func TestResolveMoleculeTrigger_SelfReferenceSkipped(t *testing.T) {
	// A pathological agent bead whose schedule_id points at the molecule itself
	// must not produce a self-edge.
	index := buildAgentTriggerIndex([]*model.Bead{
		teAgent("gd-a1", "sched-x", "gd-self", "", 10),
	})
	mol := teMolecule("gd-self", "sched-x", 100, nil)
	if plan := resolveMoleculeTrigger(mol, index); plan.Status != triggerSkipUnresolved {
		t.Fatalf("status = %q, want SKIP_UNRESOLVED (self-edge guarded)", plan.Status)
	}
}

func TestPickAgentRef_PicksClosestPrecedingThenEarliest(t *testing.T) {
	refs := []triggerAgentRef{
		{ID: "gd-early", CreatedAt: triggerBase.Add(10 * time.Second), TaskID: "gd-t-early"},
		{ID: "gd-mid", CreatedAt: triggerBase.Add(50 * time.Second), TaskID: "gd-t-mid"},
		{ID: "gd-late", CreatedAt: triggerBase.Add(200 * time.Second), TaskID: "gd-t-late"},
	}
	// Molecule at t=100 → closest preceding is gd-mid (t=50).
	if got := pickAgentRef(refs, triggerBase.Add(100*time.Second)); got.ID != "gd-mid" {
		t.Fatalf("closest-preceding = %s, want gd-mid", got.ID)
	}
	// Molecule at t=5, before every agent → fall back to earliest (gd-early).
	if got := pickAgentRef(refs, triggerBase.Add(5*time.Second)); got.ID != "gd-early" {
		t.Fatalf("none-preceding = %s, want gd-early (earliest)", got.ID)
	}
}

func TestBuildAgentTriggerIndex_DropsAgentsWithoutTriggerFields(t *testing.T) {
	index := buildAgentTriggerIndex([]*model.Bead{
		teAgent("gd-a1", "has-sched", "gd-s", "", 0),
		teAgent("gd-a2", "no-fields", "", "", 0),                    // dropped: no schedule/task
		{ID: "gd-a3", Type: "agent", Fields: json.RawMessage(`{}`)}, // dropped: no name
	})
	if _, ok := index["has-sched"]; !ok {
		t.Errorf("has-sched missing from index")
	}
	if _, ok := index["no-fields"]; ok {
		t.Errorf("no-fields should be dropped (no trigger field)")
	}
	if len(index) != 1 {
		t.Errorf("index size = %d, want 1", len(index))
	}
}

func TestMoleculeHasTriggerEdge(t *testing.T) {
	mc := newMockClientForTrigger(t)
	mc.Deps = map[string][]*model.Dependency{
		"gd-withedge": {{BeadID: "gd-withedge", DependsOnID: "gd-x", Type: "triggered-by"}},
		"gd-onlyprod": {{BeadID: "gd-onlyprod", DependsOnID: "gd-x", Type: "produces"}},
	}
	ctx := context.Background()
	if has, _ := moleculeHasTriggerEdge(ctx, "gd-withedge"); !has {
		t.Errorf("gd-withedge should report an existing trigger edge")
	}
	if has, _ := moleculeHasTriggerEdge(ctx, "gd-onlyprod"); has {
		t.Errorf("gd-onlyprod has only a produces edge — not a trigger edge")
	}
	if has, _ := moleculeHasTriggerEdge(ctx, "gd-none"); has {
		t.Errorf("gd-none has no deps")
	}
}

func TestApplyTriggerEdge_WritesEdgeNoStamp(t *testing.T) {
	mc := newMockClientForTrigger(t)
	plan := triggerEdgePlan{
		MoleculeID: "gd-m1",
		TargetID:   "gd-sched",
		EdgeType:   "triggered-by",
		Class:      "sched-via-agent.schedule_id",
		Status:     triggerWouldAdd,
	}
	if err := applyTriggerEdge(context.Background(), &plan); err != nil {
		t.Fatalf("applyTriggerEdge: %v", err)
	}
	if len(mc.AddDepCalls) != 1 {
		t.Fatalf("AddDepCalls = %d, want 1", len(mc.AddDepCalls))
	}
	dep := mc.AddDepCalls[0]
	if dep.BeadID != "gd-m1" || dep.DependsOnID != "gd-sched" || dep.Type != "triggered-by" {
		t.Fatalf("dep = %+v", dep)
	}
	// The molecule schema rejects extra fields, so apply must NOT UpdateBead —
	// a stamp write is exactly the partial-commit bug this redesign removes.
	if len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("UpdateBeadCalls = %d, want 0 (no field stamp)", len(mc.UpdateBeadCalls))
	}
	// A triggered-by edge must NOT carry the closed-parent force override.
	if mc.AddDepForceParent[0] {
		t.Errorf("triggered-by edge set X-Force-Parent-Edge, want unset")
	}
}

func TestApplyTriggerEdge_ParentChildForcesClosedParent(t *testing.T) {
	mc := newMockClientForTrigger(t)
	plan := triggerEdgePlan{
		MoleculeID: "gd-m1",
		TargetID:   "gd-captask",
		EdgeType:   "parent-child",
		Class:      "agent-spawn-via-agent.task_id",
		Status:     triggerWouldAdd,
	}
	if err := applyTriggerEdge(context.Background(), &plan); err != nil {
		t.Fatalf("applyTriggerEdge: %v", err)
	}
	if len(mc.AddDepCalls) != 1 || mc.AddDepCalls[0].Type != "parent-child" {
		t.Fatalf("AddDepCalls = %+v", mc.AddDepCalls)
	}
	// parent-child edges onto historically-closed gardener tasks REQUIRE the
	// X-Force-Parent-Edge override, else the closed-parent guard 409s.
	if !mc.AddDepForceParent[0] {
		t.Errorf("parent-child edge did not set X-Force-Parent-Edge")
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("UpdateBeadCalls = %d, want 0", len(mc.UpdateBeadCalls))
	}
}

func TestRollback_RemovesEdgesByCreatedBy(t *testing.T) {
	mc := newMockClientForTrigger(t)
	mc.ListBeadsResult = []*model.Bead{
		teMolecule("gd-m1", "sched-123", 100, nil),
		teMolecule("gd-m2", "sched-456", 200, nil),
	}
	mc.Deps = map[string][]*model.Dependency{
		// gd-m1: our backfill triggered-by edge (created_by == marker actor) plus
		// a produces edge from a different author — only the first must be removed.
		"gd-m1": {
			{BeadID: "gd-m1", DependsOnID: "gd-sched", Type: "triggered-by", CreatedBy: "d9-runner"},
			{BeadID: "gd-m1", DependsOnID: "gd-prod", Type: "produces", CreatedBy: "d9-runner"},
		},
		// gd-m2: a parent-child trigger edge from a DIFFERENT actor — must survive.
		"gd-m2": {
			{BeadID: "gd-m2", DependsOnID: "gd-cap", Type: "parent-child", CreatedBy: "someone-else"},
		},
	}
	if err := runTriggerEdgesRollback(context.Background(), "d9-runner", 200, 0, false); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if len(mc.RemoveDepCalls) != 1 {
		t.Fatalf("RemoveDepCalls = %d, want 1", len(mc.RemoveDepCalls))
	}
	rm := mc.RemoveDepCalls[0]
	if rm.BeadID != "gd-m1" || rm.DependsOnID != "gd-sched" || rm.Type != "triggered-by" {
		t.Fatalf("remove = %+v", rm)
	}
	// Rollback must NOT write the molecule bead (no stamp to clear).
	if len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("UpdateBeadCalls = %d, want 0", len(mc.UpdateBeadCalls))
	}
}

func TestRollback_DryRunWritesNothing(t *testing.T) {
	mc := newMockClientForTrigger(t)
	mc.ListBeadsResult = []*model.Bead{
		teMolecule("gd-m1", "sched-123", 100, nil),
	}
	mc.Deps = map[string][]*model.Dependency{
		"gd-m1": {{BeadID: "gd-m1", DependsOnID: "gd-sched", Type: "triggered-by", CreatedBy: "d9-runner"}},
	}
	if err := runTriggerEdgesRollback(context.Background(), "d9-runner", 200, 0, true); err != nil {
		t.Fatalf("rollback dry-run: %v", err)
	}
	if len(mc.RemoveDepCalls) != 0 || len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("dry-run wrote: remove=%d update=%d", len(mc.RemoveDepCalls), len(mc.UpdateBeadCalls))
	}
}

func TestRollback_EmptyMarkerActorErrors(t *testing.T) {
	newMockClientForTrigger(t)
	if err := runTriggerEdgesRollback(context.Background(), "", 200, 0, true); err == nil {
		t.Fatalf("empty marker-actor should error, got nil")
	}
}

// newMockClientForTrigger installs a fresh mock as the package client and
// restores the original after the test.
func newMockClientForTrigger(t *testing.T) *mockBeadsClient {
	t.Helper()
	orig := beadsClient
	mc := &mockBeadsClient{Beads: map[string]*model.Bead{}, Deps: map[string][]*model.Dependency{}}
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })
	return mc
}
