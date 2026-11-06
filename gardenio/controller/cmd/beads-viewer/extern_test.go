package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

func newTestServerForExtern(t *testing.T, daemon beadAPI) *Server {
	t.Helper()
	return NewServer(daemon, slog.Default(), "", "http://test", "", nil)
}

func mkMRBead(id, iid, title, state, pipeline string, updated time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  title,
		Type:   "merge-request",
		Status: "open",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"mr_iid":             iid,
			"mr_url":             "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/" + iid,
			"mr_state":           state,
			"mr_title":           title,
			"mr_pipeline_status": pipeline,
			"mr_pipeline_url":    "https://gitlab.com/Acme/Platform/gardenio/-/pipelines/123",
			"target_branch":      "main",
			"author":             "agent-bot",
		},
		UpdatedAt: updated,
	}
}

func mkJiraBead(id, key, status, issueType string, updated time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  key + " summary",
		Type:   "task",
		Status: "open",
		Labels: []string{"project:gardenio", "source:jira", "jira:" + key},
		Fields: map[string]string{
			"jira_key":      key,
			"jira_url":      "https://acme.atlassian.net/browse/" + key,
			"jira_status":   status,
			"jira_type":     issueType,
			"jira_assignee": "Jane Dev",
		},
		UpdatedAt: updated,
	}
}

func TestHandleExtern_HappyPath(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	daemon := &fakeDaemon{
		mrResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			mkMRBead("gd-mr1", "4459", "feat(grafana): default prod org role", "opened", "running", now.Add(-1*time.Hour)),
		}},
		jiraResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			mkJiraBead("gd-j1", "PE-7001", "In Progress", "Story", now.Add(-2*time.Hour)),
		}},
	}
	srv := newTestServerForExtern(t, daemon)
	srv.SetNow(func() time.Time { return now })

	req := httptest.NewRequest(http.MethodGet, "/extern", nil)
	rec := httptest.NewRecorder()
	srv.handleExtern(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		// MR row
		"feat(grafana): default prod org role",
		"gd-mr1",
		"!4459",
		"https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/4459",
		"mr-state-opened",
		"opened",
		"pipe-running",
		// JIRA row
		"PE-7001 summary",
		"gd-j1",
		"PE-7001",
		"https://acme.atlassian.net/browse/PE-7001",
		"In Progress",
		"Story",
		// nav/labels
		"gardenio",
		// client-side search box + per-row haystack (ka-0000008251)
		"data-extern-search",
		"extern-search-input",
		"data-search=",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestHandleExtern_SourceFilterMR(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	daemon := &fakeDaemon{
		mrResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			mkMRBead("gd-mr1", "4459", "an MR title", "merged", "success", now),
		}},
		jiraResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			mkJiraBead("gd-j1", "PE-7001", "Done", "Bug", now),
		}},
	}
	srv := newTestServerForExtern(t, daemon)
	srv.SetNow(func() time.Time { return now })

	req := httptest.NewRequest(http.MethodGet, "/extern?source=mr", nil)
	rec := httptest.NewRecorder()
	srv.handleExtern(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "an MR title") {
		t.Errorf("MR row should render under source=mr")
	}
	if strings.Contains(body, "PE-7001 summary") {
		t.Errorf("JIRA row should NOT render under source=mr")
	}
	if !strings.Contains(body, "mr-state-merged") {
		t.Errorf("expected merged state pill")
	}
	// The active source-filter toggle carries the standardized
	// aria-current="true" cue for in-page filter toggles (ka-0000002684 /
	// audit F3).
	if !strings.Contains(body, `class="chip chip-active" href="`) || !strings.Contains(body, `aria-current="true">`) {
		t.Errorf("expected active source-filter chip with aria-current=\"true\", body=%s", body)
	}
}

func TestHandleExtern_SourceFilterJira(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	daemon := &fakeDaemon{
		mrResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			mkMRBead("gd-mr1", "4459", "an MR title", "opened", "success", now),
		}},
		jiraResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			mkJiraBead("gd-j1", "PE-7001", "Done", "Bug", now),
		}},
	}
	srv := newTestServerForExtern(t, daemon)
	srv.SetNow(func() time.Time { return now })

	req := httptest.NewRequest(http.MethodGet, "/extern?source=jira", nil)
	rec := httptest.NewRecorder()
	srv.handleExtern(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "PE-7001 summary") {
		t.Errorf("JIRA row should render under source=jira")
	}
	if strings.Contains(body, "an MR title") {
		t.Errorf("MR row should NOT render under source=jira")
	}
}

func TestHandleExtern_EmptyState(t *testing.T) {
	daemon := &fakeDaemon{}
	srv := newTestServerForExtern(t, daemon)
	srv.SetNow(func() time.Time { return time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC) })

	req := httptest.NewRequest(http.MethodGet, "/extern", nil)
	rec := httptest.NewRecorder()
	srv.handleExtern(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No external objects") {
		t.Errorf("expected empty state copy; body=\n%s", rec.Body.String())
	}
}

func TestExternRowFromMR_Fallbacks(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	// Bead with no mr_title (falls back to bead Title) and no mr_iid → no label;
	// no mr_state → "unknown"; pipeline_status fallback.
	b := &beadsapi.BeadDetail{
		ID:     "gd-mr9",
		Title:  "bead title fallback",
		Type:   "merge-request",
		Status: "open",
		Fields: map[string]string{
			"mr_url":          "https://gitlab.com/x/y/-/merge_requests/9",
			"pipeline_status": "failed",
		},
		UpdatedAt: now,
	}
	row := externRowFromMR(b, now)
	if row.Title != "bead title fallback" {
		t.Errorf("Title fallback = %q", row.Title)
	}
	if row.MRState != "unknown" {
		t.Errorf("MRState fallback = %q, want unknown", row.MRState)
	}
	if row.PipelineStatus != "failed" {
		t.Errorf("PipelineStatus fallback = %q, want failed", row.PipelineStatus)
	}
	if row.ExternalLabel != "" {
		t.Errorf("ExternalLabel = %q, want empty when no iid", row.ExternalLabel)
	}
}

func TestExternRowFromJira_URLFallback(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	// Legacy bead with jira_key but no jira_url → constructed browse URL.
	b := &beadsapi.BeadDetail{
		ID:        "gd-j9",
		Title:     "legacy jira bead",
		Type:      "task",
		Status:    "closed",
		Labels:    []string{"source:jira"},
		Fields:    map[string]string{"jira_key": "DEVOPS-42"},
		UpdatedAt: now,
	}
	row := externRowFromJira(b, now)
	if !strings.Contains(row.ExternalURL, "/browse/DEVOPS-42") {
		t.Errorf("ExternalURL fallback = %q", row.ExternalURL)
	}
	if row.ExternalLabel != "DEVOPS-42" {
		t.Errorf("ExternalLabel = %q", row.ExternalLabel)
	}
}

// --- search haystack ---

func TestExternSearchText_MR(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	b := &beadsapi.BeadDetail{
		ID:     "gd-MR123",
		Title:  "Add Snappy Search",
		Type:   "merge-request",
		Status: "open",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"mr_url":        "https://gitlab.com/x/y/-/merge_requests/42",
			"mr_iid":        "42",
			"mr_state":      "opened",
			"target_branch": "main",
			"author":        "ada",
		},
		UpdatedAt: now,
	}
	got := externRowFromMR(b, now).SearchText
	// Lowercased, and every operator-meaningful field is present so a
	// per-keystroke indexOf hits on any of them.
	if got != strings.ToLower(got) {
		t.Errorf("SearchText not lowercased: %q", got)
	}
	for _, want := range []string{"add snappy search", "!42", "gd-mr123", "gardenio", "opened", "main", "ada"} {
		if !strings.Contains(got, want) {
			t.Errorf("SearchText %q missing %q", got, want)
		}
	}
}

func TestExternSearchText_Jira(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	b := &beadsapi.BeadDetail{
		ID:     "gd-J7",
		Title:  "Investigate Flaky Pipeline",
		Type:   "task",
		Status: "in_progress",
		Labels: []string{"source:jira", "project:platform"},
		Fields: map[string]string{
			"jira_key":      "PE-7001",
			"jira_url":      "https://x.atlassian.net/browse/PE-7001",
			"jira_status":   "In Progress",
			"jira_type":     "Bug",
			"jira_assignee": "grace",
		},
		UpdatedAt: now,
	}
	got := externRowFromJira(b, now).SearchText
	for _, want := range []string{"investigate flaky pipeline", "pe-7001", "gd-j7", "platform", "in progress", "bug", "grace"} {
		if !strings.Contains(got, want) {
			t.Errorf("SearchText %q missing %q", got, want)
		}
	}
}

// --- sort projection ---

func TestSortExternRows(t *testing.T) {
	mk := func(id, title, ts string) externRowView {
		return externRowView{BeadID: id, Title: title, UpdatedAtRFC: ts}
	}
	// Three rows, deliberately out of every order.
	base := []externRowView{
		mk("b", "Charlie", "2026-06-16T10:00:00Z"),
		mk("a", "alpha", "2026-06-16T12:00:00Z"),
		mk("c", "Bravo", "2026-06-16T11:00:00Z"),
	}
	clone := func() []externRowView { return append([]externRowView(nil), base...) }
	ids := func(rows []externRowView) string {
		var b strings.Builder
		for _, r := range rows {
			b.WriteString(r.BeadID)
		}
		return b.String()
	}

	cases := []struct {
		sortKey string
		want    string
	}{
		{externSortUpdated, "acb"}, // newest first: 12:00, 11:00, 10:00
		{externSortOldest, "bca"},  // oldest first: 10:00, 11:00, 12:00
		{externSortTitle, "acb"},   // case-insensitive: alpha(a), Bravo(c), Charlie(b)
		{"garbage", "acb"},         // unrecognized → default (newest)
	}
	for _, tc := range cases {
		rows := clone()
		sortExternRows(rows, tc.sortKey)
		if got := ids(rows); got != tc.want {
			t.Errorf("sort %q: got %q, want %q", tc.sortKey, got, tc.want)
		}
	}
}

// --- group projection ---

func TestGroupExternRows_ByProject(t *testing.T) {
	rows := []externRowView{
		{BeadID: "a", Source: externSourceMR, Project: "gardenio"},
		{BeadID: "b", Source: externSourceJIRA, Project: "kbeads"},
		{BeadID: "c", Source: externSourceMR, Project: "gardenio"},
		{BeadID: "d", Source: externSourceMR}, // no project
	}
	groups := groupExternRows(rows, externGroupProject)
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}
	// Largest first: gardenio (2), then kbeads (1) vs (no project) (1) → alpha by label.
	if groups[0].Key != "gardenio" || len(groups[0].Rows) != 2 {
		t.Errorf("group[0] = %+v, want gardenio x2", groups[0])
	}
	if groups[1].Label != "(no project)" {
		t.Errorf("group[1].Label = %q, want (no project) (alpha-before kbeads)", groups[1].Label)
	}
	if groups[2].Key != "kbeads" {
		t.Errorf("group[2].Key = %q, want kbeads", groups[2].Key)
	}
}

func TestGroupExternRows_BySource(t *testing.T) {
	rows := []externRowView{
		{BeadID: "a", Source: externSourceMR},
		{BeadID: "b", Source: externSourceJIRA},
		{BeadID: "c", Source: externSourceMR},
	}
	groups := groupExternRows(rows, externGroupSource)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	if groups[0].Key != externSourceMR || len(groups[0].Rows) != 2 {
		t.Errorf("group[0] = %+v, want mr x2", groups[0])
	}
}

func TestGroupExternRows_ByState(t *testing.T) {
	rows := []externRowView{
		{BeadID: "a", Source: externSourceMR, MRState: "opened"},
		{BeadID: "b", Source: externSourceMR, MRState: "merged"},
		{BeadID: "c", Source: externSourceMR, MRState: "opened"},
		{BeadID: "d", Source: externSourceJIRA, JiraStatus: "In Progress"},
	}
	groups := groupExternRows(rows, externGroupState)
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3 (opened/merged/In Progress)", len(groups))
	}
	if groups[0].Label != "opened" || len(groups[0].Rows) != 2 {
		t.Errorf("group[0] = %+v, want opened x2", groups[0])
	}
}

func TestGroupExternRows_NoneReturnsNil(t *testing.T) {
	rows := []externRowView{{BeadID: "a", Source: externSourceMR}}
	if g := groupExternRows(rows, externGroupNone); g != nil {
		t.Errorf("group none = %+v, want nil", g)
	}
}

// --- externState URL building ---

func TestExternState_ChipURLPreservesDimensions(t *testing.T) {
	st := externState{
		source:  externSourceMR,
		project: "gardenio",
		status:  "open", // default — omitted from URLs
		sortKey: externSortTitle,
		group:   externGroupProject,
		base:    "/extern",
	}
	// Toggling group off keeps source/project/sort.
	got := st.chipURL(map[string]string{"group": ""})
	for _, want := range []string{"source=mr", "project=gardenio", "sort=title"} {
		if !strings.Contains(got, want) {
			t.Errorf("chipURL %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "group=") {
		t.Errorf("chipURL %q should drop group", got)
	}
	if strings.Contains(got, "status=") {
		t.Errorf("chipURL %q should omit default status=open", got)
	}
}

func TestExternState_ChipURLBareBase(t *testing.T) {
	st := externState{status: "open", sortKey: externSortUpdated, base: "/extern"}
	if got := st.chipURL(nil); got != "/extern" {
		t.Errorf("all-default chipURL = %q, want /extern", got)
	}
}

// --- handler render: sort / group / empty-state / MR-live ---

func TestHandleExtern_GroupBySource(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	daemon := &fakeDaemon{
		mrResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			mkMRBead("gd-mr1", "4459", "an MR title", "opened", "running", now),
		}},
		jiraResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			mkJiraBead("gd-j1", "PE-7001", "In Progress", "Story", now.Add(-time.Hour)),
		}},
	}
	srv := newTestServerForExtern(t, daemon)
	srv.SetNow(func() time.Time { return now })

	req := httptest.NewRequest(http.MethodGet, "/extern?group=source", nil)
	rec := httptest.NewRecorder()
	srv.handleExtern(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"extern-group-mr",
		"extern-group-jira",
		"an MR title",
		"PE-7001 summary",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("grouped body missing %q", want)
		}
	}
}

func TestHandleExtern_SortTitle(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	daemon := &fakeDaemon{
		mrResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			// Zebra updated most recently, Apple least — title sort must invert
			// the default newest-first order.
			mkMRBead("gd-mrZ", "1", "Zebra", "opened", "success", now),
			mkMRBead("gd-mrA", "2", "Apple", "opened", "success", now.Add(-2*time.Hour)),
		}},
	}
	srv := newTestServerForExtern(t, daemon)
	srv.SetNow(func() time.Time { return now })

	req := httptest.NewRequest(http.MethodGet, "/extern?source=mr&sort=title", nil)
	rec := httptest.NewRecorder()
	srv.handleExtern(rec, req)

	body := rec.Body.String()
	apple := strings.Index(body, "Apple")
	zebra := strings.Index(body, "Zebra")
	if apple < 0 || zebra < 0 || apple > zebra {
		t.Errorf("sort=title should render Apple before Zebra (apple=%d zebra=%d)", apple, zebra)
	}
}

func TestHandleExtern_EmptyStateActions(t *testing.T) {
	daemon := &fakeDaemon{}
	srv := newTestServerForExtern(t, daemon)
	srv.SetNow(func() time.Time { return time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC) })

	// Filtered (status=closed) empty render should offer the widen-to-all action.
	req := httptest.NewRequest(http.MethodGet, "/extern?status=closed", nil)
	rec := httptest.NewRecorder()
	srv.handleExtern(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Show all statuses") {
		t.Errorf("expected 'Show all statuses' action in filtered empty state; body=\n%s", body)
	}
	if !strings.Contains(body, "status=all") {
		t.Errorf("expected status=all link in empty state")
	}
}

func TestHandleExtern_MRLiveStrip(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	lister := &fakeMRLister{
		fixedOpened: []bridge.GitLabMR{
			{IID: 1, State: "opened", MergeStatus: "can_be_merged"},
			{IID: 2, State: "opened", Draft: true},
		},
		fixedMerged: []bridge.GitLabMR{{IID: 3, State: "merged"}},
	}
	srv := newTestServerForExtern(t, &fakeDaemon{})
	srv.SetNow(func() time.Time { return now })
	srv.SetMRLister(lister, "Acme/Platform/gardenio", "https://gitlab.com/x/-/merge_requests")

	req := httptest.NewRequest(http.MethodGet, "/extern", nil)
	rec := httptest.NewRecorder()
	srv.handleExtern(rec, req)

	body := rec.Body.String()
	for _, want := range []string{"extern-mrlive", "GitLab MRs", "mergeable", "merged today"} {
		if !strings.Contains(body, want) {
			t.Errorf("MR-live strip missing %q", want)
		}
	}
}

func TestHandleExtern_MRLiveSuppressedForJiraOrProject(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	lister := &fakeMRLister{fixedOpened: []bridge.GitLabMR{{IID: 1, State: "opened"}}}
	srv := newTestServerForExtern(t, &fakeDaemon{})
	srv.SetNow(func() time.Time { return now })
	srv.SetMRLister(lister, "Acme/Platform/gardenio", "https://gitlab.com/x/-/merge_requests")

	// source=jira → MR-live strip suppressed (counts speak for MRs only).
	req := httptest.NewRequest(http.MethodGet, "/extern?source=jira", nil)
	rec := httptest.NewRecorder()
	srv.handleExtern(rec, req)
	if strings.Contains(rec.Body.String(), "extern-mrlive") {
		t.Errorf("MR-live strip should be suppressed under source=jira")
	}
	if lister.openedCalls() != 0 {
		t.Errorf("GitLab should not be queried under source=jira; openedCalls=%d", lister.openedCalls())
	}
}
