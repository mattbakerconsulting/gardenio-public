package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ── pure-helper tests (no daemon) ────────────────────────────────────────────

func TestPlanPhaseOrder_Decode(t *testing.T) {
	cases := []struct {
		name  string
		field map[string]string
		want  []string
	}{
		{"absent", nil, nil},
		{"empty-string", map[string]string{"phase_order": ""}, nil},
		{"unparseable", map[string]string{"phase_order": "not-json"}, nil},
		{"valid", map[string]string{"phase_order": `["gd-a","gd-b"]`}, []string{"gd-a", "gd-b"}},
		{"drops-blanks", map[string]string{"phase_order": `["gd-a","","gd-b"]`}, []string{"gd-a", "gd-b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planPhaseOrder(&beadsapi.BeadDetail{Fields: c.field})
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("planPhaseOrder(%v) = %v, want %v", c.field, got, c.want)
			}
		})
	}
	// planStepOrder shares the decode; confirm it reads its own field key.
	if got := planStepOrder(&beadsapi.BeadDetail{Fields: map[string]string{"step_order": `["gd-x"]`}}); !reflect.DeepEqual(got, []string{"gd-x"}) {
		t.Fatalf("planStepOrder = %v, want [gd-x]", got)
	}
	// A nil bead must not panic.
	if got := planPhaseOrder(nil); got != nil {
		t.Fatalf("planPhaseOrder(nil) = %v, want nil", got)
	}
}

func TestResolvePlanPhaseIDs_DropsStaleAndNonContainer(t *testing.T) {
	children := map[string]*beadsapi.BeadDetail{
		"gd-p1":  {ID: "gd-p1", Type: "epic"},
		"gd-mol": {ID: "gd-mol", Type: "molecule"}, // poured phase — a container, kept (ka-0000004781)
		"gd-p2":  {ID: "gd-p2", Type: "epic"},
		"gd-t1":  {ID: "gd-t1", Type: "task"}, // listed but not a container → dropped
		// gd-gone is in the order but absent from children (deleted) → dropped
	}
	order := []string{"gd-p1", "gd-gone", "gd-t1", "gd-mol", "gd-p2", "gd-p1"} // trailing dup
	got := resolvePlanPhaseIDs(order, children)
	want := []string{"gd-p1", "gd-mol", "gd-p2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolvePlanPhaseIDs = %v, want %v (poured molecule phase must be kept, matching the cockpit)", got, want)
	}
}

func TestSortPlanSteps_OrderOverlayThenCreation(t *testing.T) {
	t0 := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	// Three steps; step_order lists c then a. b is unlisted → sorts last by
	// creation order.
	a := &beadsapi.BeadDetail{ID: "gd-a", CreatedAt: t0.Add(1 * time.Minute)}
	b := &beadsapi.BeadDetail{ID: "gd-b", CreatedAt: t0.Add(2 * time.Minute)}
	c := &beadsapi.BeadDetail{ID: "gd-c", CreatedAt: t0.Add(3 * time.Minute)}
	steps := []*beadsapi.BeadDetail{b, a, c}
	sortPlanSteps(steps, []string{"gd-c", "gd-a"})
	got := []string{steps[0].ID, steps[1].ID, steps[2].ID}
	want := []string{"gd-c", "gd-a", "gd-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sortPlanSteps(with overlay) = %v, want %v", got, want)
	}

	// Empty overlay reproduces pure creation order (pre-reorder behavior).
	steps2 := []*beadsapi.BeadDetail{c, a, b}
	sortPlanSteps(steps2, nil)
	got2 := []string{steps2[0].ID, steps2[1].ID, steps2[2].ID}
	want2 := []string{"gd-a", "gd-b", "gd-c"}
	if !reflect.DeepEqual(got2, want2) {
		t.Fatalf("sortPlanSteps(no overlay) = %v, want %v", got2, want2)
	}
}

func TestPlanParentChildIDs_FiltersAndDedupes(t *testing.T) {
	deps := []beadsapi.Dependency{
		{BeadID: "gd-a", Type: "parent-child"},
		{BeadID: "gd-x", Type: "assigned"}, // not parent-child → dropped
		{BeadID: "gd-b", Type: "parent-child"},
		{BeadID: "gd-a", Type: "parent-child"}, // dup → dropped
	}
	got := planParentChildIDs(deps)
	want := []string{"gd-a", "gd-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planParentChildIDs = %v, want %v", got, want)
	}
}

func TestIsPlanStepType(t *testing.T) {
	for _, ty := range []string{"task", "bug", "feature", "chore"} {
		if !isPlanStepType(ty) {
			t.Errorf("isPlanStepType(%q) = false, want true", ty)
		}
	}
	for _, ty := range []string{"epic", "bundle", "doc", "decision", ""} {
		if isPlanStepType(ty) {
			t.Errorf("isPlanStepType(%q) = true, want false", ty)
		}
	}
}

// ── resolver test with a fake daemon reader ──────────────────────────────────

// fakePlanReader is an in-memory planDaemonReader: a bead store plus a
// parent→children parent-child edge map. It exercises resolvePlanRead with NO
// HTTP round-trip, standing in for the daemon the contract test (ka-0000004277)
// drives against a real server with only BEADS_AUTH_TOKEN.
type fakePlanReader struct {
	beads    map[string]*beadsapi.BeadDetail
	children map[string][]string // parent id → ordered child ids (parent-child)
}

func (f *fakePlanReader) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	return f.beads[id], nil
}

func (f *fakePlanReader) GetReverseDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	var deps []beadsapi.Dependency
	for _, cid := range f.children[id] {
		deps = append(deps, beadsapi.Dependency{BeadID: cid, DependsOnID: id, Type: "parent-child"})
	}
	return deps, nil
}

func (f *fakePlanReader) BatchGetBeads(_ context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	for _, id := range ids {
		if b := f.beads[id]; b != nil {
			out[id] = b
		}
	}
	return out, nil
}

func (f *fakePlanReader) BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error) {
	out := make(map[string][]beadsapi.Dependency, len(ids))
	for _, id := range ids {
		deps, _ := f.GetReverseDependencies(ctx, id)
		out[id] = deps
	}
	return out, nil
}

// TestResolvePlanRead_PhasedPlan is the end-to-end read contract: a plan epic
// with two phases (each holding steps) plus a flat/unphased step and a narrative
// bundle child. It proves resolvePlanRead groups steps under the right phase in
// phase_order, honors each group's step_order, numbers steps 1..N per group,
// excludes phase steps from the flat list, and drops the bundle (a non-step
// type). This is the shape the daemon-alone contract test asserts.
func TestResolvePlanRead_PhasedPlan(t *testing.T) {
	f := &fakePlanReader{
		beads:    map[string]*beadsapi.BeadDetail{},
		children: map[string][]string{},
	}
	add := func(b *beadsapi.BeadDetail) { f.beads[b.ID] = b }

	add(&beadsapi.BeadDetail{
		ID: "gd-PLAN", Type: "epic", Title: "Ship it", Status: "open",
		Labels: []string{"plan", "project:gardenio"},
		Fields: map[string]string{
			"plan_state":  "building",
			"phase_order": `["gd-PH1","gd-PH2"]`,
			"step_order":  `["gd-FLAT"]`,
		},
	})
	add(&beadsapi.BeadDetail{ID: "gd-PH1", Type: "epic", Title: "Research", Status: "closed",
		Fields: map[string]string{"step_order": `["gd-S2","gd-S1"]`}})
	add(&beadsapi.BeadDetail{ID: "gd-PH2", Type: "epic", Title: "Build", Status: "open"})
	add(&beadsapi.BeadDetail{ID: "gd-S1", Type: "task", Title: "S1", Status: "closed"})
	add(&beadsapi.BeadDetail{ID: "gd-S2", Type: "task", Title: "S2", Status: "open"})
	add(&beadsapi.BeadDetail{ID: "gd-S3", Type: "bug", Title: "S3", Status: "open"})
	add(&beadsapi.BeadDetail{ID: "gd-FLAT", Type: "task", Title: "Flat step", Status: "open"})
	add(&beadsapi.BeadDetail{ID: "gd-BUNDLE", Type: "bundle", Title: "Narrative", Status: "open"})

	// Plan's direct children: both phases, the flat step, and the bundle.
	f.children["gd-PLAN"] = []string{"gd-PH1", "gd-PH2", "gd-FLAT", "gd-BUNDLE"}
	// Phase children.
	f.children["gd-PH1"] = []string{"gd-S1", "gd-S2"} // step_order flips to S2,S1
	f.children["gd-PH2"] = []string{"gd-S3"}

	view, err := resolvePlanRead(context.Background(), f, "gd-PLAN")
	if err != nil {
		t.Fatalf("resolvePlanRead: %v", err)
	}

	if view.PlanState != "building" || view.Project != "gardenio" || view.Status != "open" {
		t.Fatalf("header wrong: %+v", view)
	}
	if len(view.Phases) != 2 {
		t.Fatalf("want 2 phases, got %d", len(view.Phases))
	}

	// Phase 1: numbered 1, closed, steps ordered S2 then S1 by its step_order.
	ph1 := view.Phases[0]
	if ph1.ID != "gd-PH1" || ph1.Number != 1 || !ph1.Closed {
		t.Fatalf("phase1 header wrong: %+v", ph1)
	}
	gotP1 := []string{ph1.Steps[0].ID, ph1.Steps[1].ID}
	if !reflect.DeepEqual(gotP1, []string{"gd-S2", "gd-S1"}) {
		t.Fatalf("phase1 step order = %v, want [gd-S2 gd-S1]", gotP1)
	}
	if ph1.Steps[0].Number != 1 || ph1.Steps[1].Number != 2 {
		t.Fatalf("phase1 step numbering wrong: %+v", ph1.Steps)
	}

	// Phase 2: numbered 2, one step.
	ph2 := view.Phases[1]
	if ph2.ID != "gd-PH2" || ph2.Number != 2 || len(ph2.Steps) != 1 || ph2.Steps[0].ID != "gd-S3" {
		t.Fatalf("phase2 wrong: %+v", ph2)
	}

	// Flat steps: only gd-FLAT (phase steps excluded; bundle dropped as non-step).
	if len(view.Steps) != 1 || view.Steps[0].ID != "gd-FLAT" || view.Steps[0].Number != 1 {
		t.Fatalf("flat steps = %+v, want just gd-FLAT", view.Steps)
	}
}

// TestResolvePlanRead_UnphasedPlan covers the no-phase-order path: every direct
// issue child is a flat step in step_order/creation order, and a non-epic target
// or missing plan is a clear error.
func TestResolvePlanRead_UnphasedPlan(t *testing.T) {
	f := &fakePlanReader{
		beads:    map[string]*beadsapi.BeadDetail{},
		children: map[string][]string{},
	}
	f.beads["gd-PLAN"] = &beadsapi.BeadDetail{
		ID: "gd-PLAN", Type: "epic", Title: "Flat plan", Status: "open",
		Fields: map[string]string{"step_order": `["gd-B","gd-A"]`},
	}
	f.beads["gd-A"] = &beadsapi.BeadDetail{ID: "gd-A", Type: "task", Title: "A", Status: "open"}
	f.beads["gd-B"] = &beadsapi.BeadDetail{ID: "gd-B", Type: "feature", Title: "B", Status: "closed"}
	f.children["gd-PLAN"] = []string{"gd-A", "gd-B"}

	view, err := resolvePlanRead(context.Background(), f, "gd-PLAN")
	if err != nil {
		t.Fatalf("resolvePlanRead: %v", err)
	}
	if len(view.Phases) != 0 {
		t.Fatalf("want no phases, got %d", len(view.Phases))
	}
	got := []string{view.Steps[0].ID, view.Steps[1].ID}
	if !reflect.DeepEqual(got, []string{"gd-B", "gd-A"}) {
		t.Fatalf("flat step order = %v, want [gd-B gd-A]", got)
	}
}

func TestResolvePlanRead_Errors(t *testing.T) {
	f := &fakePlanReader{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-TASK": {ID: "gd-TASK", Type: "task"},
		},
		children: map[string][]string{},
	}
	if _, err := resolvePlanRead(context.Background(), f, "gd-MISSING"); err == nil {
		t.Fatal("want error for missing plan, got nil")
	}
	if _, err := resolvePlanRead(context.Background(), f, "gd-TASK"); err == nil {
		t.Fatal("want error for non-epic target, got nil")
	}
}

// TestResolvePlanRead_UnreadableStepShown: a plan child the batch read does
// not return (outside the caller's project scope) is listed as unreadable, in
// the flat list and under its phase, and `gb plan show` prints it rather than
// dropping it (gd-BYCG4IGJB2).
func TestResolvePlanRead_UnreadableStepShown(t *testing.T) {
	f := &fakePlanReader{beads: map[string]*beadsapi.BeadDetail{}, children: map[string][]string{}}
	f.beads["gd-PLAN"] = &beadsapi.BeadDetail{ID: "gd-PLAN", Type: "epic", Title: "P", Status: "open",
		Labels: []string{"plan", "project:gardenio"}, Fields: map[string]string{"phase_order": `["gd-PH1"]`}}
	f.beads["gd-PH1"] = &beadsapi.BeadDetail{ID: "gd-PH1", Type: "epic", Title: "Phase", Status: "open"}
	f.beads["gd-A"] = &beadsapi.BeadDetail{ID: "gd-A", Type: "task", Title: "A", Status: "open"}
	f.children["gd-PLAN"] = []string{"gd-PH1", "gd-A", "gd-HIDDEN"}
	f.children["gd-PH1"] = []string{"gd-PHHIDDEN"}

	view, err := resolvePlanRead(context.Background(), f, "gd-PLAN")
	if err != nil {
		t.Fatalf("resolvePlanRead: %v", err)
	}
	if !reflect.DeepEqual(view.Unreadable, []string{"gd-HIDDEN"}) {
		t.Fatalf("plan unreadable = %v, want [gd-HIDDEN]", view.Unreadable)
	}
	if len(view.Phases) != 1 || !reflect.DeepEqual(view.Phases[0].Unreadable, []string{"gd-PHHIDDEN"}) {
		t.Fatalf("phase unreadable = %+v, want [gd-PHHIDDEN]", view.Phases)
	}

	out := string(captureStdout(t, func() error { printPlanReadView(view); return nil }))
	for _, want := range []string{
		"    gd-PHHIDDEN (unreadable: outside your project scope)",
		"  gd-HIDDEN (unreadable: outside your project scope)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan show output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(no steps)") {
		t.Fatalf("a phase holding only an unreadable step printed (no steps):\n%s", out)
	}
}
