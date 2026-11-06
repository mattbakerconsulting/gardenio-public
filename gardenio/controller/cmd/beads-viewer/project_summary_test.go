package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestBuildProjectSummary_CountsAndFacets verifies the summary builder slices
// a project's active beads into the right buckets: per-status, per-type,
// per-chip-macro, top-assignees, priority distribution, blocked-on-decision,
// stale-in-progress, and created-in-24h.
func TestBuildProjectSummary_CountsAndFacets(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-48 * time.Hour) // older than 24h => stale
	fresh := now.Add(-1 * time.Hour)  // within 24h
	createdNow := now.Add(-2 * time.Hour)
	createdOld := now.Add(-72 * time.Hour)

	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-1", Type: "task", Status: "open", Priority: 0, Assignee: "alice", CreatedAt: createdNow, UpdatedAt: fresh},
			{ID: "gd-2", Type: "task", Status: "in_progress", Priority: 1, Assignee: "alice", CreatedAt: createdOld, UpdatedAt: fresh},
			{ID: "gd-3", Type: "bug", Status: "in_progress", Priority: 2, Assignee: "bob", CreatedAt: createdOld, UpdatedAt: stale}, // stale in_progress
			{ID: "gd-4", Type: "feature", Status: "open", Priority: 2, Assignee: "bob", CreatedAt: createdNow, UpdatedAt: fresh, BlockedOnDecision: true},
			{ID: "gd-5", Type: "decision", Status: "open", Priority: 1, CreatedAt: createdOld, UpdatedAt: fresh},
		},
		Total: 5,
	}

	sum, err := buildProjectSummary(context.Background(), f, "gardenio", now)
	if err != nil {
		t.Fatalf("buildProjectSummary: %v", err)
	}
	if sum == nil {
		t.Fatal("summary is nil")
	}

	if sum.Open != 3 {
		t.Errorf("Open = %d, want 3", sum.Open)
	}
	if sum.InProgress != 2 {
		t.Errorf("InProgress = %d, want 2", sum.InProgress)
	}
	if sum.TotalOpen != 5 {
		t.Errorf("TotalOpen = %d, want 5", sum.TotalOpen)
	}
	if sum.Blocked != 1 {
		t.Errorf("Blocked = %d, want 1", sum.Blocked)
	}
	if sum.StaleInProgress != 1 {
		t.Errorf("StaleInProgress = %d, want 1 (gd-3)", sum.StaleInProgress)
	}
	if sum.Created24h != 2 {
		t.Errorf("Created24h = %d, want 2 (gd-1, gd-4)", sum.Created24h)
	}

	// Type counts: 2 tasks, 1 bug, 1 feature, 1 decision; macros: all=5, issues=4.
	if got, want := sum.TypeCounts["task"], 2; got != want {
		t.Errorf("TypeCounts[task] = %d, want %d", got, want)
	}
	if got, want := sum.TypeCounts["bug"], 1; got != want {
		t.Errorf("TypeCounts[bug] = %d, want %d", got, want)
	}
	if got, want := sum.TypeCounts["decision"], 1; got != want {
		t.Errorf("TypeCounts[decision] = %d, want %d", got, want)
	}
	if got, want := sum.TypeCounts["all"], 5; got != want {
		t.Errorf("TypeCounts[all] = %d, want %d", got, want)
	}
	if got, want := sum.TypeCounts["issues"], 4; got != want {
		t.Errorf("TypeCounts[issues] = %d, want %d", got, want)
	}

	// Priority distribution.
	if sum.PriorityCounts[0] != 1 || sum.PriorityCounts[1] != 2 || sum.PriorityCounts[2] != 2 {
		t.Errorf("PriorityCounts = %v, want [1 2 2 0 0]", sum.PriorityCounts)
	}

	// Top assignees: alice (2), bob (2). Ties broken by name asc.
	if len(sum.TopAssignees) != 2 {
		t.Fatalf("TopAssignees = %v, want 2 entries", sum.TopAssignees)
	}
	if sum.TopAssignees[0].Assignee != "alice" || sum.TopAssignees[0].Count != 2 {
		t.Errorf("TopAssignees[0] = %+v, want {alice 2}", sum.TopAssignees[0])
	}
	if sum.TopAssignees[1].Assignee != "bob" || sum.TopAssignees[1].Count != 2 {
		t.Errorf("TopAssignees[1] = %+v, want {bob 2}", sum.TopAssignees[1])
	}
}

// TestBuildProjectSummary_KindIssueFilter verifies the summary builder
// restricts its daemon query to Kinds:["issue"] so the active-universe scan
// doesn't fill with type=session / type=event / advice / schedule rows and
// starve the issue-kind facets (ka-0000002522). Mirrors ka-0000006881.
func TestBuildProjectSummary_KindIssueFilter(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}

	if _, err := buildProjectSummary(context.Background(), f, "gardenio", time.Now()); err != nil {
		t.Fatalf("buildProjectSummary: %v", err)
	}

	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	if !slices.Contains(f.listLastQ.Kinds, "issue") {
		t.Errorf("buildProjectSummary must pass Kinds=[issue]; got Kinds=%v", f.listLastQ.Kinds)
	}
}

// TestBuildProjectSummary_BlockedAndStaleLists verifies the right-rail
// attention lists pick the oldest-first beads and cap at projectSummaryListLimit.
func TestBuildProjectSummary_BlockedAndStaleLists(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	older := now.Add(-72 * time.Hour)
	old := now.Add(-48 * time.Hour)
	stale := now.Add(-36 * time.Hour)
	fresh := now.Add(-1 * time.Hour)

	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// Two blocked beads — oldest (gd-block-old) should appear first.
			{ID: "gd-block-new", Type: "task", Status: "open", BlockedOnDecision: true, UpdatedAt: fresh},
			{ID: "gd-block-old", Type: "task", Status: "open", BlockedOnDecision: true, UpdatedAt: older, Title: "older blocked"},
			// Three stale in_progress — oldest first.
			{ID: "gd-stale-a", Type: "bug", Status: "in_progress", UpdatedAt: stale, Title: "stale a"},
			{ID: "gd-stale-b", Type: "bug", Status: "in_progress", UpdatedAt: old, Title: "stale b"},
			{ID: "gd-stale-c", Type: "bug", Status: "in_progress", UpdatedAt: older, Title: "stale c"},
			// Fresh in_progress — not stale.
			{ID: "gd-active", Type: "bug", Status: "in_progress", UpdatedAt: fresh},
		},
		Total: 6,
	}

	sum, err := buildProjectSummary(context.Background(), f, "gardenio", now)
	if err != nil {
		t.Fatalf("buildProjectSummary: %v", err)
	}

	if len(sum.BlockedBeads) != 2 {
		t.Fatalf("BlockedBeads = %+v, want 2 entries", sum.BlockedBeads)
	}
	if sum.BlockedBeads[0].ID != "gd-block-old" {
		t.Errorf("BlockedBeads[0].ID = %q, want gd-block-old (oldest-first)", sum.BlockedBeads[0].ID)
	}
	if sum.BlockedBeads[0].Title != "older blocked" {
		t.Errorf("BlockedBeads[0].Title = %q, want %q", sum.BlockedBeads[0].Title, "older blocked")
	}

	if len(sum.StaleBeads) != 3 {
		t.Fatalf("StaleBeads = %+v, want 3 entries", sum.StaleBeads)
	}
	// Oldest first: c (-72h), b (-48h), a (-36h).
	if sum.StaleBeads[0].ID != "gd-stale-c" || sum.StaleBeads[1].ID != "gd-stale-b" || sum.StaleBeads[2].ID != "gd-stale-a" {
		t.Errorf("StaleBeads ordering wrong: got %v, want [gd-stale-c gd-stale-b gd-stale-a]", []string{sum.StaleBeads[0].ID, sum.StaleBeads[1].ID, sum.StaleBeads[2].ID})
	}
}

// TestBuildProjectSummary_AttentionListsCapAtLimit verifies the right-rail
// lists cap at projectSummaryListLimit even when many beads qualify.
func TestBuildProjectSummary_AttentionListsCapAtLimit(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-48 * time.Hour)

	beads := make([]*beadsapi.BeadDetail, 0, 20)
	for i := range 10 {
		beads = append(beads, &beadsapi.BeadDetail{
			ID:                "gd-block-" + string(rune('a'+i)),
			Type:              "task",
			Status:            "open",
			BlockedOnDecision: true,
			UpdatedAt:         stale.Add(time.Duration(i) * time.Minute),
		})
	}
	for i := range 10 {
		beads = append(beads, &beadsapi.BeadDetail{
			ID:        "gd-stale-" + string(rune('a'+i)),
			Type:      "bug",
			Status:    "in_progress",
			UpdatedAt: stale.Add(time.Duration(i) * time.Minute),
		})
	}

	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: beads, Total: len(beads)}

	sum, err := buildProjectSummary(context.Background(), f, "gardenio", now)
	if err != nil {
		t.Fatalf("buildProjectSummary: %v", err)
	}
	if len(sum.BlockedBeads) != projectSummaryListLimit {
		t.Errorf("BlockedBeads len = %d, want %d", len(sum.BlockedBeads), projectSummaryListLimit)
	}
	if len(sum.StaleBeads) != projectSummaryListLimit {
		t.Errorf("StaleBeads len = %d, want %d", len(sum.StaleBeads), projectSummaryListLimit)
	}
	if sum.Blocked != 10 {
		t.Errorf("Blocked count = %d, want 10 (count is independent of list cap)", sum.Blocked)
	}
}

// TestHandleProject_SidebarRendersBlockedAndStaleLists verifies the right-rail
// surfaces clickable "Blocked" and "Stale in-progress" bead lists — not just
// counts — so operators can jump straight to the work that needs attention.
func TestHandleProject_SidebarRendersBlockedAndStaleLists(t *testing.T) {
	stale := time.Now().Add(-48 * time.Hour)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-blocked-1", Title: "Decide which payment provider", Type: "task", Status: "open", BlockedOnDecision: true, UpdatedAt: stale},
			{ID: "gd-stuck-1", Title: "Refactor auth middleware", Type: "task", Status: "in_progress", UpdatedAt: stale},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`aria-label="Blocked beads"`,
		`href="/gd-blocked-1"`,
		`Decide which payment provider`,
		`aria-label="Stale in-progress beads"`,
		`href="/gd-stuck-1"`,
		`Refactor auth middleware`,
		`class="project-summary-bead-list"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project sidebar missing %q\nbody=%s", want, body)
		}
	}
}

// TestBuildProjectSummary_TruncatedFlag exercises the "more rows than we
// fetched" path so the sidebar can warn that counts are a floor.
func TestBuildProjectSummary_TruncatedFlag(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{ID: "gd-1", Type: "task", Status: "open"}},
		Total: 2000,
	}
	sum, err := buildProjectSummary(context.Background(), f, "gardenio", now)
	if err != nil {
		t.Fatalf("buildProjectSummary: %v", err)
	}
	if !sum.Truncated {
		t.Errorf("Truncated = false, want true (Total > len(Beads))")
	}
}

// TestBuildProjectSummary_DaemonError ensures a daemon failure surfaces as a
// returned error so the caller can suppress the sidebar (rather than
// rendering a half-built panel).
func TestBuildProjectSummary_DaemonError(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: 500, Message: "boom"}
	if _, err := buildProjectSummary(context.Background(), f, "gardenio", now); err == nil {
		t.Errorf("expected error when daemon fails, got nil")
	}
}

// TestHandleProject_SidebarRendersCounts verifies the right-rail summary
// surfaces the key numbers (status + attention + chip counts) on /p/<project>.
func TestHandleProject_SidebarRendersCounts(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-1", Type: "task", Status: "open", Priority: 1, Assignee: "alice"},
			{ID: "gd-2", Type: "bug", Status: "in_progress", Priority: 0, Assignee: "bob", BlockedOnDecision: true},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`class="project-layout"`,
		`class="project-sidebar"`,
		`>Status<`,
		`>Open<`,
		`>In progress<`,
		`>Total active<`,
		`>Top assignees<`,
		`>alice<`,
		`>bob<`,
		`>Blocked on decision<`,
		`class="chip-count"`, // chip badges rendered when counts > 0
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project sidebar missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleProject_HeaderBandRendersCounts verifies the horizontal header
// band above the bead feed (ka-0000002513) surfaces the key project-level
// facets — status counts, blocked/stale/created-24h callouts, and top
// owners — so the page is dense above the fold without scrolling to the
// right-rail sidebar.
func TestHandleProject_HeaderBandRendersCounts(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-1", Type: "task", Status: "open", Priority: 1, Assignee: "alice"},
			{ID: "gd-2", Type: "bug", Status: "in_progress", Priority: 0, Assignee: "bob", BlockedOnDecision: true},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`class="project-header-band"`,
		`>Active<`,
		`>Open<`,
		`>In progress<`,
		`>Blocked<`,
		`>Top owners<`,
		`class="band-owner"`,
		`>alice<`,
		`>bob<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project header band missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleProject_SidebarSuppressedOnSummaryError verifies that a daemon
// error on the summary call hides the sidebar (and chip badges, and the
// header band) without failing the rest of the page.
func TestHandleProject_SidebarSuppressedOnSummaryError(t *testing.T) {
	f := newFake()
	// Both the summary call and the main list call see this error — we
	// expect the page to still render (with an error banner) and to NOT
	// render the sidebar markup.
	f.listErr = &beadsapi.APIError{StatusCode: 500, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `class="project-sidebar"`) {
		t.Errorf("expected sidebar to be suppressed on summary error, got body=%s", body)
	}
	if strings.Contains(body, `class="project-header-band"`) {
		t.Errorf("expected header band to be suppressed on summary error, got body=%s", body)
	}
	if strings.Contains(body, `class="chip-count"`) {
		t.Errorf("expected chip count badges suppressed on summary error, got body=%s", body)
	}
}
