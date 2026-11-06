package main

import (
	"context"
	"html/template"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// actorInitialsTest mirrors the production helper just so the partial parses
// stand-alone in this test. The template file references `actorInitials`
// which is registered on the production FuncMap.
func actorInitialsTest(_ string) string { return "" }

// TestInboxMRResolver_Prewarm_MRBead asserts that a type=merge-request bead
// populates the cache directly from its own fields with no daemon calls.
func TestInboxMRResolver_Prewarm_MRBead(t *testing.T) {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{},
		deps:  map[string][]beadsapi.Dependency{},
	}
	mrBead := &beadsapi.BeadDetail{
		ID:   "gd-mr1",
		Type: "merge-request",
		Fields: map[string]string{
			"mr_state": "opened",
			"mr_url":   "https://gitlab.com/x/y/-/merge_requests/42",
		},
	}
	r := newInboxMRResolver()
	r.Prewarm(context.Background(), f, []*beadsapi.BeadDetail{mrBead})

	if got := r.Lookup("gd-mr1"); got.State != "opened" || got.URL != "https://gitlab.com/x/y/-/merge_requests/42" {
		t.Fatalf("MR bead cache miss: %+v", got)
	}
	if f.batchGetDepsCalls != 0 {
		t.Fatalf("expected no batched dep calls for MR bead, got %d", f.batchGetDepsCalls)
	}
	if f.batchGetBeadsCalls != 0 {
		t.Fatalf("expected no batched bead calls for MR bead, got %d", f.batchGetBeadsCalls)
	}
}

// TestInboxMRResolver_Prewarm_TaskWithMR asserts a task bead with a
// `produces` edge to an MR bead resolves to that MR's state with exactly one
// batched dep call + one batched bead call (the hot-path perf contract).
func TestInboxMRResolver_Prewarm_TaskWithMR(t *testing.T) {
	mrBead := &beadsapi.BeadDetail{
		ID:   "gd-mr1",
		Type: "merge-request",
		Fields: map[string]string{
			"mr_state": "merged",
			"mr_url":   "https://gitlab.com/x/y/-/merge_requests/100",
		},
	}
	taskA := &beadsapi.BeadDetail{ID: "gd-taskA", Type: "task"}
	taskB := &beadsapi.BeadDetail{ID: "gd-taskB", Type: "task"} // no MR edge
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-mr1": mrBead,
		},
		deps: map[string][]beadsapi.Dependency{
			"gd-taskA": {
				{BeadID: "gd-taskA", DependsOnID: "gd-mr1", Type: "produces"},
			},
		},
	}
	r := newInboxMRResolver()
	r.Prewarm(context.Background(), f, []*beadsapi.BeadDetail{taskA, taskB})

	if got := r.Lookup("gd-taskA"); got.State != "merged" {
		t.Fatalf("taskA expected MRState=merged, got %+v", got)
	}
	if got := r.Lookup("gd-taskB"); got.State != "" {
		t.Fatalf("taskB expected empty MRState, got %+v", got)
	}
	if f.batchGetDepsCalls != 1 {
		t.Fatalf("expected 1 batched dep call, got %d", f.batchGetDepsCalls)
	}
	if f.batchGetBeadsCalls != 1 {
		t.Fatalf("expected 1 batched bead call, got %d", f.batchGetBeadsCalls)
	}
}

// TestInboxMRResolver_Prewarm_NoEdgeNoBeadCall asserts that when none of the
// task beads have a produces edge, we issue zero BatchGetBeads round-trips —
// no daemon traffic past the dep batch (kept cheap for the no-MR steady state).
func TestInboxMRResolver_Prewarm_NoEdgeNoBeadCall(t *testing.T) {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{},
		deps:  map[string][]beadsapi.Dependency{},
	}
	r := newInboxMRResolver()
	r.Prewarm(context.Background(), f,
		[]*beadsapi.BeadDetail{{ID: "gd-t1", Type: "task"}, {ID: "gd-t2", Type: "task"}})
	if f.batchGetDepsCalls != 1 {
		t.Fatalf("expected 1 dep batch call, got %d", f.batchGetDepsCalls)
	}
	if f.batchGetBeadsCalls != 0 {
		t.Fatalf("expected 0 bead batch calls when no edges, got %d", f.batchGetBeadsCalls)
	}
}

// TestInboxMRResolver_NilSafe asserts Lookup on a nil resolver returns the
// zero info and does not panic — covers the inboxRowFromBead legacy callers.
func TestInboxMRResolver_NilSafe(t *testing.T) {
	var r *inboxMRResolver
	got := r.Lookup("gd-anything")
	if got.State != "" || got.URL != "" {
		t.Fatalf("nil resolver should miss: %+v", got)
	}
}

// TestInboxRowFromBead_MRChip asserts the row builder copies MR state from
// the resolver onto inboxRowView for both the MR-bead and task-bead paths,
// and leaves the field empty when the resolver returns nothing.
func TestInboxRowFromBead_MRChip(t *testing.T) {
	mr := newInboxMRResolver()
	mr.cache["gd-mrA"] = inboxMRInfo{State: "opened", URL: "https://example/1"}
	mr.cache["gd-YRgvMHA0v1"] = inboxMRInfo{State: "merged", URL: "https://example/2"}

	mrBead := &beadsapi.BeadDetail{ID: "gd-mrA", Type: "merge-request", Title: "MR row"}
	taskWithMR := &beadsapi.BeadDetail{ID: "gd-YRgvMHA0v1", Type: "task", Title: "Task w/ MR"}
	taskNoMR := &beadsapi.BeadDetail{ID: "gd-taskNone", Type: "task", Title: "Lonely task"}

	if row := inboxRowFromBeadWithMR(mrBead, mr); row.MRState != "opened" || row.MRURL != "https://example/1" {
		t.Fatalf("MR-bead row missing chip data: %+v", row)
	}
	if row := inboxRowFromBeadWithMR(taskWithMR, mr); row.MRState != "merged" {
		t.Fatalf("task-with-MR row missing chip data: %+v", row)
	}
	if row := inboxRowFromBeadWithMR(taskNoMR, mr); row.MRState != "" {
		t.Fatalf("task-without-MR row should not carry chip data: %+v", row)
	}
}

// TestInboxStreamRowFromIntake_MRChip mirrors TestInboxRowFromBead_MRChip
// for the stream-row builder used by the densified inbox template.
func TestInboxStreamRowFromIntake_MRChip(t *testing.T) {
	mr := newInboxMRResolver()
	mr.cache["gd-T"] = inboxMRInfo{State: "opened", URL: "https://example/3"}

	taskWithMR := &beadsapi.BeadDetail{ID: "gd-T", Type: "task", Title: "T"}
	taskNoMR := &beadsapi.BeadDetail{ID: "gd-U", Type: "task", Title: "U"}

	rowA := inboxStreamRowFromIntake(taskWithMR, nil, mr, "", time.Time{})
	if rowA.MRState != "opened" || rowA.MRURL != "https://example/3" {
		t.Fatalf("stream row missing MR chip: %+v", rowA)
	}
	rowB := inboxStreamRowFromIntake(taskNoMR, nil, mr, "", time.Time{})
	if rowB.MRState != "" {
		t.Fatalf("stream row should not carry MR chip: %+v", rowB)
	}
}

// TestInboxRowTemplate_MRChip asserts the inbox_row partial emits the chip
// iff MRState is non-empty, and that the chip carries the state's class
// variant + tooltip text.
func TestInboxRowTemplate_MRChip(t *testing.T) {
	tmpl := template.Must(template.New("").
		Funcs(template.FuncMap{"actorInitials": actorInitialsTest, "displayTitle": displayTitle}).
		ParseFS(templateFS, "templates/partials/inbox_row.html"))

	render := func(row inboxStreamRow) string {
		var buf strings.Builder
		if err := tmpl.ExecuteTemplate(&buf, "inbox_row", row); err != nil {
			t.Fatalf("render: %v", err)
		}
		return buf.String()
	}

	withMR := render(inboxStreamRow{
		Kind:    "intake",
		ID:      "gd-row1",
		Title:   "with MR",
		Type:    "task",
		MRState: "opened",
		MRURL:   "https://gitlab.com/x/y/-/merge_requests/5",
		Href:    "/inbox/gd-row1",
	})
	if !strings.Contains(withMR, `chip-mr chip-mr-opened`) {
		t.Errorf("expected chip-mr-opened class in rendered row: %s", withMR)
	}
	if !strings.Contains(withMR, "mr: opened") {
		t.Errorf("expected chip label 'mr: opened': %s", withMR)
	}
	if !strings.Contains(withMR, "https://gitlab.com/x/y/-/merge_requests/5") {
		t.Errorf("expected MR URL in tooltip: %s", withMR)
	}
	if !strings.Contains(withMR, "· mr: opened") {
		t.Errorf("expected ' · mr: opened' suffix in chip-cluster title=: %s", withMR)
	}

	noMR := render(inboxStreamRow{
		Kind:  "intake",
		ID:    "gd-row2",
		Title: "no MR",
		Type:  "task",
		Href:  "/inbox/gd-row2",
	})
	if strings.Contains(noMR, "chip-mr") {
		t.Errorf("row without MRState should not emit chip-mr: %s", noMR)
	}
}
