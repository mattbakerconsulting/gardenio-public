package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// lineageAgentBead builds a type=agent bead carrying task_id plus optional
// existing lineage fields, created at the given offset (seconds) from a fixed
// base so ordering within a task is deterministic.
func lineageAgentBead(id, taskID string, status model.Status, offsetSec int, prior, root string) *model.Bead {
	fields := map[string]any{"task_id": taskID}
	if prior != "" {
		fields["prior_agent_id"] = prior
	}
	if root != "" {
		fields["lineage_root_id"] = root
	}
	raw, _ := json.Marshal(fields)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	return &model.Bead{
		ID:        id,
		Type:      "agent",
		Kind:      model.KindConfig,
		Status:    status,
		CreatedAt: base.Add(time.Duration(offsetSec) * time.Second),
		Fields:    raw,
	}
}

// patchFields decodes the prior_agent_id / lineage_root_id from a row's patch.
func patchFields(t *testing.T, patch json.RawMessage) (string, string) {
	t.Helper()
	var p map[string]string
	if err := json.Unmarshal(patch, &p); err != nil {
		t.Fatalf("patch unmarshal: %v", err)
	}
	return p["prior_agent_id"], p["lineage_root_id"]
}

func rowByID(rows []lineageRow, id string) *lineageRow {
	for i := range rows {
		if rows[i].BeadID == id {
			return &rows[i]
		}
	}
	return nil
}

func TestPlanLineageForTask_ChainRootsAtFirstClosed(t *testing.T) {
	// Three occupants created in order, all closed. b1 is the root; b2 points at
	// b1; b3 points at b2 but inherits b1 as the root (chain roots at first).
	beads := []*model.Bead{
		lineageAgentBead("gd-b1", "gd-task1", model.StatusClosed, 0, "", ""),
		lineageAgentBead("gd-b2", "gd-task1", model.StatusClosed, 10, "", ""),
		lineageAgentBead("gd-b3", "gd-task1", model.StatusClosed, 20, "", ""),
	}
	rows := planLineageForTask("gd-task1", beads)
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}

	// b1: no predecessor.
	if r := rowByID(rows, "gd-b1"); r == nil || r.Status != lineageSkipNoPredecessor {
		t.Fatalf("b1 row = %+v", r)
	}
	// b2: prior=b1, root=b1 (b1 has no root of its own → becomes root).
	r2 := rowByID(rows, "gd-b2")
	if r2 == nil || r2.Status != lineageWouldUpdate {
		t.Fatalf("b2 row = %+v", r2)
	}
	prior, root := patchFields(t, r2.patch)
	if prior != "gd-b1" || root != "gd-b1" {
		t.Errorf("b2 prior=%q root=%q, want gd-b1/gd-b1", prior, root)
	}
	// b3: prior=b2, root inherited = b1.
	r3 := rowByID(rows, "gd-b3")
	if r3 == nil || r3.Status != lineageWouldUpdate {
		t.Fatalf("b3 row = %+v", r3)
	}
	prior, root = patchFields(t, r3.patch)
	if prior != "gd-b2" || root != "gd-b1" {
		t.Errorf("b3 prior=%q root=%q, want gd-b2/gd-b1", prior, root)
	}
}

func TestPlanLineageForTask_SkipsOpenPredecessors(t *testing.T) {
	// The middle occupant is still OPEN, so b3's predecessor is the closed b1,
	// not the open b2 — an unfinished sibling is not a predecessor.
	beads := []*model.Bead{
		lineageAgentBead("gd-b1", "gd-task2", model.StatusClosed, 0, "", ""),
		lineageAgentBead("gd-b2", "gd-task2", model.StatusInProgress, 10, "", ""),
		lineageAgentBead("gd-b3", "gd-task2", model.StatusClosed, 20, "", ""),
	}
	rows := planLineageForTask("gd-task2", beads)

	// b2 is open → it has a closed predecessor (b1), so it still gets stamped;
	// only its being-open matters for whether OTHERS point at it.
	r2 := rowByID(rows, "gd-b2")
	if r2 == nil || r2.Status != lineageWouldUpdate {
		t.Fatalf("b2 row = %+v", r2)
	}
	if prior, _ := patchFields(t, r2.patch); prior != "gd-b1" {
		t.Errorf("b2 prior=%q, want gd-b1", prior)
	}
	// b3 skips the open b2 and points at the closed b1.
	r3 := rowByID(rows, "gd-b3")
	if r3 == nil || r3.Status != lineageWouldUpdate {
		t.Fatalf("b3 row = %+v", r3)
	}
	if prior, root := patchFields(t, r3.patch); prior != "gd-b1" || root != "gd-b1" {
		t.Errorf("b3 prior=%q root=%q, want gd-b1/gd-b1", prior, root)
	}
}

func TestPlanLineageForTask_Idempotent(t *testing.T) {
	// A bead already carrying prior_agent_id is skipped; a later bead still
	// inherits the existing chain's root through it.
	beads := []*model.Bead{
		lineageAgentBead("gd-b1", "gd-task3", model.StatusClosed, 0, "", ""),
		lineageAgentBead("gd-b2", "gd-task3", model.StatusClosed, 10, "gd-b1", "gd-b1"),
		lineageAgentBead("gd-b3", "gd-task3", model.StatusClosed, 20, "", ""),
	}
	rows := planLineageForTask("gd-task3", beads)

	if r := rowByID(rows, "gd-b2"); r == nil || r.Status != lineageSkipIdempotent {
		t.Fatalf("b2 row = %+v", r)
	}
	// b3 points at the already-stamped b2 and inherits its explicit root b1.
	r3 := rowByID(rows, "gd-b3")
	if r3 == nil || r3.Status != lineageWouldUpdate {
		t.Fatalf("b3 row = %+v", r3)
	}
	if prior, root := patchFields(t, r3.patch); prior != "gd-b2" || root != "gd-b1" {
		t.Errorf("b3 prior=%q root=%q, want gd-b2/gd-b1", prior, root)
	}
}

func TestPlanLineageForTask_TieBreakByID(t *testing.T) {
	// Two beads share a created_at; the id tie-break makes the ordering (and
	// thus the predecessor pick) deterministic: gd-a sorts before gd-b.
	beads := []*model.Bead{
		lineageAgentBead("gd-b", "gd-task4", model.StatusClosed, 0, "", ""),
		lineageAgentBead("gd-a", "gd-task4", model.StatusClosed, 0, "", ""),
	}
	rows := planLineageForTask("gd-task4", beads)
	if r := rowByID(rows, "gd-a"); r == nil || r.Status != lineageSkipNoPredecessor {
		t.Fatalf("gd-a should be first (no predecessor): %+v", r)
	}
	rb := rowByID(rows, "gd-b")
	if rb == nil || rb.Status != lineageWouldUpdate {
		t.Fatalf("gd-b row = %+v", rb)
	}
	if prior, _ := patchFields(t, rb.patch); prior != "gd-a" {
		t.Errorf("gd-b prior=%q, want gd-a", prior)
	}
}

func TestGroupAgentsByTask(t *testing.T) {
	beads := []*model.Bead{
		lineageAgentBead("gd-1", "gd-taskA", model.StatusClosed, 0, "", ""),
		lineageAgentBead("gd-2", "gd-taskB", model.StatusClosed, 0, "", ""),
		lineageAgentBead("gd-3", "gd-taskA", model.StatusClosed, 5, "", ""),
		{ID: "gd-notask", Type: "agent", Fields: mustFields(map[string]any{})}, // no task_id → dropped
		nil, // defensive
	}
	groups := groupAgentsByTask(beads)
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d: %v", len(groups), groups)
	}
	if len(groups["gd-taskA"]) != 2 {
		t.Errorf("taskA group = %d, want 2", len(groups["gd-taskA"]))
	}
	if len(groups["gd-taskB"]) != 1 {
		t.Errorf("taskB group = %d, want 1", len(groups["gd-taskB"]))
	}
}

// TestAdminBackfillLineage_Apply drives the full RunE end to end: a single task
// with a two-bead chain. The apply issues exactly one UpdateBead (for the
// successor) carrying the merged prior_agent_id + lineage_root_id, and the
// table reports UPDATED.
func TestAdminBackfillLineage_Apply(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{{
		lineageAgentBead("gd-old", "gd-taskX", model.StatusClosed, 0, "", ""),
		lineageAgentBead("gd-new", "gd-taskX", model.StatusClosed, 10, "", ""),
	}}
	withMockClient(t, mc)
	t.Cleanup(func() { _ = adminBackfillLineageCmd.Flags().Set("dry-run", "false") })
	_ = adminBackfillLineageCmd.Flags().Set("dry-run", "false")

	out := captureStdout(t, func() {
		if err := adminBackfillLineageCmd.RunE(adminBackfillLineageCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("want 1 UpdateBead call, got %d", len(mc.UpdateBeadCalls))
	}
	call := mc.UpdateBeadCalls[0]
	if call.ID != "gd-new" {
		t.Errorf("updated %q, want gd-new", call.ID)
	}
	prior, root := patchFields(t, call.Req.Fields)
	if prior != "gd-old" || root != "gd-old" {
		t.Errorf("patch prior=%q root=%q, want gd-old/gd-old", prior, root)
	}
	if !strings.Contains(out, string(lineageUpdated)) {
		t.Errorf("missing UPDATED in output:\n%s", out)
	}
	// tabwriter expands the header tabs to aligned spaces, so assert on the
	// column labels rather than the tab-joined literal.
	if !strings.Contains(out, "BEAD_ID") || !strings.Contains(out, "LINEAGE_ROOT") || !strings.Contains(out, "STATUS") {
		t.Errorf("missing table header columns:\n%s", out)
	}
}

// TestAdminBackfillLineage_DryRunIdempotent proves a re-run after an apply is a
// no-op: both beads already carry prior_agent_id, so --dry-run issues no writes
// and every row is SKIP_IDEMPOTENT (the first occupant is SKIP_NO_PREDECESSOR).
func TestAdminBackfillLineage_DryRunIdempotent(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{{
		lineageAgentBead("gd-old", "gd-taskY", model.StatusClosed, 0, "", ""),
		lineageAgentBead("gd-new", "gd-taskY", model.StatusClosed, 10, "gd-old", "gd-old"),
	}}
	withMockClient(t, mc)
	t.Cleanup(func() { _ = adminBackfillLineageCmd.Flags().Set("dry-run", "false") })
	_ = adminBackfillLineageCmd.Flags().Set("dry-run", "true")

	out := captureStdout(t, func() {
		if err := adminBackfillLineageCmd.RunE(adminBackfillLineageCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("dry-run issued %d UpdateBead calls", len(mc.UpdateBeadCalls))
	}
	if strings.Contains(out, string(lineageWouldUpdate)) {
		t.Errorf("unexpected WOULD_UPDATE in idempotent re-run:\n%s", out)
	}
	if !strings.Contains(out, string(lineageSkipIdempotent)) {
		t.Errorf("expected SKIP_IDEMPOTENT rows:\n%s", out)
	}
}
