package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

func gardenerBead(id, typ string, labels []string, fields map[string]any) *model.Bead {
	raw, _ := json.Marshal(fields)
	return &model.Bead{ID: id, Type: model.BeadType(typ), Kind: model.KindIssue, Status: model.StatusOpen, Labels: labels, Fields: raw}
}

// gardenerCmd builds a throwaway cobra command carrying the same flags as
// adminBackfillGardenerOwnerCmd so each test gets isolated flag state.
func gardenerCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "gardener-owner", RunE: runAdminBackfillGardenerOwner, SilenceUsage: true, SilenceErrors: true}
	c.Flags().Bool("apply", false, "")
	c.Flags().Bool("dry-run", false, "")
	c.Flags().Bool("resync-conflicts", false, "")
	c.Flags().Int("limit", 0, "")
	c.Flags().Int("page-size", 200, "")
	c.Flags().Bool("require-saturated", false, "")
	if err := c.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return c
}

func rowsFor(rows []gardenerRow, phase string) []gardenerRow {
	var out []gardenerRow
	for _, r := range rows {
		if r.Phase == phase {
			out = append(out, r)
		}
	}
	return out
}

func TestPlanGardenerMarkers_OldOnlyAllThree(t *testing.T) {
	b := gardenerBead("gd-t1", "trigger",
		[]string{"role:captain", "captain-epic:gd-plan1", "trigger:schedule"},
		map[string]any{"captain": `{"name":"x"}`})
	rows := planGardenerMarkers(b, false)
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %+v", rows)
	}
	for _, r := range rows {
		if r.Status != gardenerWouldUpdate {
			t.Errorf("row %+v not WOULD_UPDATE", r)
		}
	}
	if r := rowsFor(rows, "label")[0]; r.label != "role:gardener" || r.Old != "role:captain" {
		t.Errorf("label row = %+v", r)
	}
	if r := rowsFor(rows, "epic")[0]; r.label != "gardener-epic:gd-plan1" || r.Old != "captain-epic:gd-plan1" {
		t.Errorf("epic row = %+v", r)
	}
	fr := rowsFor(rows, "field")[0]
	var patch map[string]any
	if err := json.Unmarshal(fr.fieldPatch, &patch); err != nil || patch["gardener"] != `{"name":"x"}` {
		t.Errorf("field patch = %s (%v)", fr.fieldPatch, err)
	}
}

func TestPlanGardenerMarkers_ObjectFieldCopiedVerbatim(t *testing.T) {
	b := gardenerBead("gd-t2", "trigger", []string{"role:captain", "role:gardener"},
		map[string]any{"captain": map[string]any{"name": "x", "standing": true}})
	fr := rowsFor(planGardenerMarkers(b, false), "field")[0]
	if fr.Status != gardenerWouldUpdate {
		t.Fatalf("field row = %+v", fr)
	}
	if got := string(fr.fieldPatch); !strings.Contains(got, `"gardener":{"name":"x","standing":true}`) {
		t.Errorf("patch = %s", got)
	}
}

func TestPlanGardenerMarkers_IdempotentWhenBothPresent(t *testing.T) {
	b := gardenerBead("gd-t3", "trigger",
		[]string{"role:captain", "role:gardener", "captain-epic:p", "gardener-epic:p"},
		map[string]any{"captain": `{"a":1}`, "gardener": `{"a":1}`})
	rows := planGardenerMarkers(b, false)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.Status != gardenerSkipIdempotent {
			t.Errorf("row %+v not SKIP_IDEMPOTENT", r)
		}
	}
	if !gardenerSaturated(rows) {
		t.Error("fully dual-marked bead should be saturated")
	}
}

func TestPlanGardenerMarkers_NewOnlyIsReverseGap(t *testing.T) {
	b := gardenerBead("gd-t4", "trigger",
		[]string{"role:gardener", "gardener-epic:p"}, map[string]any{"gardener": `{"a":1}`})
	rows := planGardenerMarkers(b, false)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.Status != gardenerNewOnly {
			t.Errorf("row %+v not NEW_ONLY", r)
		}
	}
	if gardenerSaturated(rows) {
		t.Error("NEW_ONLY must fail the saturation gate")
	}
}

func TestPlanGardenerMarkers_MultipleEpicLabelsPairedPerID(t *testing.T) {
	b := gardenerBead("gd-t5", "task",
		[]string{"captain-epic:a", "captain-epic:b", "gardener-epic:a"}, nil)
	epics := rowsFor(planGardenerMarkers(b, false), "epic")
	if len(epics) != 2 {
		t.Fatalf("epics = %+v", epics)
	}
	if epics[0].Status != gardenerSkipIdempotent || epics[1].Status != gardenerWouldUpdate || epics[1].label != "gardener-epic:b" {
		t.Errorf("epics = %+v", epics)
	}
}

func TestPlanGardenerMarkers_UnrelatedAndNonTriggerFieldIgnored(t *testing.T) {
	if rows := planGardenerMarkers(gardenerBead("gd-t6", "task", []string{"project:gardenio"}, nil), false); len(rows) != 0 {
		t.Errorf("unrelated bead rows = %+v", rows)
	}
	// `captain` on a non-trigger type is not a registered field → no field row.
	b := gardenerBead("gd-t7", "agent", nil, map[string]any{"captain": "x"})
	if rows := planGardenerMarkers(b, false); len(rows) != 0 {
		t.Errorf("non-trigger rows = %+v", rows)
	}
	// Empty blob counts as absent.
	b = gardenerBead("gd-t8", "trigger", nil, map[string]any{"captain": "  "})
	if rows := planGardenerMarkers(b, false); len(rows) != 0 {
		t.Errorf("empty blob rows = %+v", rows)
	}
}

func TestRunGardenerOwner_DryRunWritesNothing(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		gardenerBead("gd-r1", "trigger", []string{"role:captain", "captain-epic:p"}, map[string]any{"captain": `{"a":1}`}),
	}
	withMockClient(t, mc)

	err := runAdminBackfillGardenerOwner(gardenerCmd(t, "--dry-run"), nil)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(mc.AddLabelCalls) != 0 || len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("dry-run wrote: labels=%v updates=%d", mc.AddLabelCalls, len(mc.UpdateBeadCalls))
	}
}

func TestRunGardenerOwner_RequireSaturatedFailsOnGap(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{gardenerBead("gd-r2", "task", []string{"role:captain"}, nil)}
	withMockClient(t, mc)

	err := runAdminBackfillGardenerOwner(gardenerCmd(t, "--dry-run", "--require-saturated"), nil)
	if err == nil || !strings.Contains(err.Error(), "saturation gate FAILED") {
		t.Fatalf("err = %v", err)
	}

	// Saturated tree passes.
	mc2 := newMockClient()
	mc2.ListBeadsResult = []*model.Bead{gardenerBead("gd-r3", "task", []string{"role:captain", "role:gardener"}, nil)}
	withMockClient(t, mc2)
	if err := runAdminBackfillGardenerOwner(gardenerCmd(t, "--dry-run", "--require-saturated"), nil); err != nil {
		t.Fatalf("saturated tree: %v", err)
	}
}

func TestRunGardenerOwner_ApplyStampsAndIsIdempotent(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		gardenerBead("gd-r4", "trigger", []string{"role:captain", "captain-epic:p"}, map[string]any{"captain": `{"a":1}`}),
	}
	withMockClient(t, mc)

	if err := runAdminBackfillGardenerOwner(gardenerCmd(t, "--apply"), nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := map[string]bool{}
	for _, c := range mc.AddLabelCalls {
		if c.BeadID != "gd-r4" {
			t.Errorf("label call on %s", c.BeadID)
		}
		got[c.Label] = true
	}
	if !got["role:gardener"] || !got["gardener-epic:p"] || len(got) != 2 {
		t.Errorf("labels stamped = %v", got)
	}
	if len(mc.UpdateBeadCalls) != 1 || !strings.Contains(string(mc.UpdateBeadCalls[0].Req.Fields), `"gardener"`) {
		t.Fatalf("field update = %+v", mc.UpdateBeadCalls)
	}

	// Re-run against the post-apply state: no writes.
	mc2 := newMockClient()
	mc2.ListBeadsResult = []*model.Bead{
		gardenerBead("gd-r4", "trigger",
			[]string{"role:captain", "role:gardener", "captain-epic:p", "gardener-epic:p"},
			map[string]any{"captain": `{"a":1}`, "gardener": `{"a":1}`}),
	}
	withMockClient(t, mc2)
	if err := runAdminBackfillGardenerOwner(gardenerCmd(t, "--apply"), nil); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if len(mc2.AddLabelCalls) != 0 || len(mc2.UpdateBeadCalls) != 0 {
		t.Fatalf("re-run wrote: %v / %d", mc2.AddLabelCalls, len(mc2.UpdateBeadCalls))
	}
}

func TestRunGardenerOwner_ApplyFailureExitsNonZero(t *testing.T) {
	mc := newMockClient()
	mc.UpdateErr = errTest("boom")
	mc.ListBeadsResult = []*model.Bead{
		gardenerBead("gd-r5", "trigger", nil, map[string]any{"captain": `{"a":1}`}),
	}
	withMockClient(t, mc)
	if err := runAdminBackfillGardenerOwner(gardenerCmd(t, "--apply"), nil); err == nil {
		t.Fatal("expected non-zero exit on failed update")
	}
}

func TestRunGardenerOwner_NoFlagsWritesNothing(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		gardenerBead("gd-n1", "trigger", []string{"role:captain", "captain-epic:p"}, map[string]any{"captain": `{"a":1}`}),
	}
	withMockClient(t, mc)

	if err := runAdminBackfillGardenerOwner(gardenerCmd(t), nil); err != nil {
		t.Fatalf("no-flag run: %v", err)
	}
	if len(mc.AddLabelCalls) != 0 || len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("bare invocation wrote: labels=%v updates=%d", mc.AddLabelCalls, len(mc.UpdateBeadCalls))
	}
}

func TestRunGardenerOwner_FlagValidation(t *testing.T) {
	withMockClient(t, newMockClient())
	if err := runAdminBackfillGardenerOwner(gardenerCmd(t, "--apply", "--dry-run"), nil); err == nil {
		t.Error("--apply with --dry-run should error")
	}
	if err := runAdminBackfillGardenerOwner(gardenerCmd(t, "--resync-conflicts"), nil); err == nil {
		t.Error("--resync-conflicts without --apply should error")
	}
}

func TestPlanGardenerMarkers_DifferingFieldsAreConflict(t *testing.T) {
	b := gardenerBead("gd-c1", "trigger", []string{"role:captain", "role:gardener"},
		map[string]any{"captain": `{"a":1}`, "gardener": `{"a":2}`})
	fr := rowsFor(planGardenerMarkers(b, false), "field")[0]
	if fr.Status != gardenerConflict || fr.fieldPatch != nil {
		t.Fatalf("field row = %+v", fr)
	}
	if gardenerSaturated([]gardenerRow{fr}) {
		t.Error("CONFLICT must fail the saturation gate")
	}
	// Object vs string-encoded value that decode equal are not a conflict.
	b = gardenerBead("gd-c2", "trigger", nil,
		map[string]any{"captain": map[string]any{"a": 1.0}, "gardener": `{"a":1}`})
	if fr := rowsFor(planGardenerMarkers(b, false), "field")[0]; fr.Status != gardenerSkipIdempotent {
		t.Errorf("object vs encoded string = %+v", fr)
	}
}

func TestPlanGardenerMarkers_KeyOrderAndWhitespaceStayIdempotent(t *testing.T) {
	b := gardenerBead("gd-c3", "trigger", nil,
		map[string]any{"captain": `{"a":1,"b":[1,2]}`, "gardener": "{ \"b\": [1, 2],\n \"a\": 1 }"})
	if fr := rowsFor(planGardenerMarkers(b, false), "field")[0]; fr.Status != gardenerSkipIdempotent {
		t.Errorf("reordered blob = %+v", fr)
	}
}

func TestRunGardenerOwner_ConflictFailsGateAndIsNotWritten(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		gardenerBead("gd-c4", "trigger", nil, map[string]any{"captain": `{"a":1}`, "gardener": `{"a":2}`}),
	}
	withMockClient(t, mc)

	err := runAdminBackfillGardenerOwner(gardenerCmd(t, "--require-saturated"), nil)
	if err == nil || !strings.Contains(err.Error(), "saturation gate FAILED") {
		t.Fatalf("err = %v", err)
	}
	// --apply alone must not overwrite the conflicting gardener value.
	if err := runAdminBackfillGardenerOwner(gardenerCmd(t, "--apply"), nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("conflict overwritten without --resync-conflicts: %+v", mc.UpdateBeadCalls)
	}
}

func TestRunGardenerOwner_ResyncConflictsOverwrites(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		gardenerBead("gd-c5", "trigger", nil, map[string]any{"captain": `{"a":1}`, "gardener": `{"a":2}`}),
	}
	withMockClient(t, mc)
	if err := runAdminBackfillGardenerOwner(gardenerCmd(t, "--apply", "--resync-conflicts"), nil); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 || !strings.Contains(string(mc.UpdateBeadCalls[0].Req.Fields), `{\"a\":1}`) {
		t.Fatalf("resync update = %+v", mc.UpdateBeadCalls)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
