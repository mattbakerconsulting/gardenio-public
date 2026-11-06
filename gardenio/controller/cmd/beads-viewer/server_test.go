package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestHandleIndex(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
		"kbeads":   {Name: "kbeads"},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html content-type, got %q", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Beads Viewer") {
		t.Errorf("expected landing page content, got %q", body)
	}
	for _, want := range []string{
		"<h2>Projects</h2>",
		`href="/p/gardenio"`,
		`href="/p/kbeads"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q\nbody=%s", want, body)
		}
	}
	// Projects are rendered alphabetically: gardenio before kbeads.
	if i, j := strings.Index(body, "/p/gardenio"), strings.Index(body, "/p/kbeads"); i < 0 || j < 0 || i >= j {
		t.Errorf("expected projects rendered alphabetically, got order i=%d j=%d", i, j)
	}
}

func TestHandleIndex_DaemonError(t *testing.T) {
	f := newFake()
	f.projectsErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load project list") {
		t.Errorf("expected error banner, body=%s", w.Body.String())
	}
}

func TestHandleIndex_Empty(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No projects registered") {
		t.Errorf("expected empty-state text, body=%s", w.Body.String())
	}
}

func TestHandleIndex_StatsBanner(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{"gardenio": {Name: "gardenio"}}
	f.statsResp = &beadsapi.GraphStats{
		TotalOpen:       7,
		TotalInProgress: 3,
		TotalBlocked:    1,
		TotalClosed:     11,
		TotalDeferred:   2,
	}
	// Two type configs: "task" with fields {priority, design}, "bug" with
	// fields {priority, severity}. Distinct field names: priority, design,
	// severity → 3 unique fields across 2 types.
	f.configsByNS = map[string][]beadsapi.ConfigEntry{
		"type": {
			{Key: "type:task", Value: []byte(`{"kind":"issue","fields":[{"name":"priority","type":"integer"},{"name":"design","type":"string"}]}`)},
			{Key: "type:bug", Value: []byte(`{"kind":"issue","fields":[{"name":"priority","type":"integer"},{"name":"severity","type":"string"}]}`)},
		},
	}
	// Hot-ready / banner-ready queries share NoOpenDeps+open; the fake routes
	// both to hotReadyResp. Seed a Total so the "ready" banner cell has a
	// distinct digit to assert against.
	f.hotReadyResp = &beadsapi.ListBeadsResult{Total: 9}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`stats-banner-dense`,
		`>24<`, // total beads = 7+3+1+11+2
		`>7<`,  // open
		`>3<`,  // in_progress
		`>1<`,  // blocked
		`>11<`, // closed
		`>2<`,  // deferred OR type configs (2)
		`>9<`,  // ready (seeded via hotReadyResp.Total)
		// 3 unique fields is also rendered as ">3<" — distinct dimensions all
		// share the same digit, so we only assert the headline labels below.
		"total beads",
		"open",
		"in progress",
		"blocked",
		"closed",
		"deferred",
		"types tracked",
		"unique fields",
		"decisions",
		"agents live",
		"ready",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stats banner missing %q\nbody=%s", want, body)
		}
	}
}

func TestHandleIndex_StatsBannerDegradesOnError(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	f.statsErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	f.configsErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (banner failure should not fail the page), got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `stats-banner-dense`) {
		t.Errorf("expected banner to still render with em-dashes, body=%s", body)
	}
	if !strings.Contains(body, "&mdash;") {
		t.Errorf("expected em-dash placeholder when stats unavailable, body=%s", body)
	}
}

// TestHandleIndex_StatsBannerPerGroupDegradation pins the fix for ka-0000002676:
// one failing best-effort sub-call must blank ONLY its own tile, not the whole
// banner. Before the fix a single shared OK bool meant any one of seven
// sub-calls flipping false rendered all twelve numbers as em-dashes. Here only
// the escalations call fails; every other tile must still show its real count.
func TestHandleIndex_StatsBannerPerGroupDegradation(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	f.statsResp = &beadsapi.GraphStats{TotalOpen: 7}
	// Escalations is the single failing sub-call.
	f.escalationsErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	// P0 scan succeeds with two P0s so its tile has a distinctive real number.
	f.p0ScanResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-p0a", Priority: 0, Status: "open"},
			{ID: "gd-p0b", Priority: 0, Status: "open"},
			{ID: "gd-p1a", Priority: 1, Status: "open"},
		},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Tiles whose sub-call succeeded must render their real numbers, NOT dashes.
	for _, want := range []string{
		`stats-banner-num">7</span>`,       // open (GetStats)
		`stats-banner-num">2</span>`,       // P0 open (p0 scan)
		`stats-banner-label">total beads<`, // stats tile present
	} {
		if !strings.Contains(body, want) {
			t.Errorf("banner missing healthy tile %q — a single failing sub-call blanked more than its own tile\nbody=%s", want, body)
		}
	}
	// The escalations tile (the ONLY failing call) must render an em-dash.
	if !strings.Contains(body, `href="/escalations">&mdash;</a>`) {
		t.Errorf("escalations tile should show em-dash when its sub-call fails\nbody=%s", body)
	}
}

// TestHandleIndex_ProjectCounts verifies that per-project rows render the
// open / in_progress / blocked / deferred / decisions counts the daemon
// returns. The blocked / deferred / decisions chips were added in
// ka-0000007960 — the audit found the two-chip row was still leaving the
// lower-left viewport sparse next to the recent-activity column.
func TestHandleIndex_ProjectCounts(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
	}
	// computeIndexProjects issues five ListBeadsFiltered calls per project
	// (statuses=[open|in_progress|blocked|deferred] + types=[decision]).
	// The fake's listResp covers the four status-only queries; decisionResp
	// covers the type=decision query.
	f.listResp = &beadsapi.ListBeadsResult{Total: 7}
	f.decisionResp = &beadsapi.ListBeadsResult{Total: 3}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`index-project-list`,
		`href="/p/gardenio"`,
		`href="/p/gardenio?status=open"`,
		`href="/p/gardenio?status=in_progress"`,
		`href="/p/gardenio?status=blocked"`,
		`href="/p/gardenio?status=deferred"`,
		`href="/p/gardenio?type=decision&amp;status=open"`,
		`index-project-count-num">7<`,
		`index-project-count-num">3<`,
		`index-project-count-label">blocked<`,
		`index-project-count-label">deferred<`,
		`index-project-count-label">decisions<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project panel missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleIndex_ProjectCounts_KindIssue verifies the per-project counter
// queries restrict to Kinds:["issue"] so /index doesn't count type=session /
// type=event / agent / advice / schedule rows alongside real work
// (ka-0000002522). Mirrors the prior open-beads-tile fix (ka-0000006881).
func TestHandleIndex_ProjectCounts_KindIssue(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
	}
	f.listResp = &beadsapi.ListBeadsResult{Total: 0}
	f.decisionResp = &beadsapi.ListBeadsResult{Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Inspect captured ListBeadsFiltered queries: every per-project count
	// (open/in_progress/blocked/deferred/decisions) must carry Kinds:["issue"]
	// and labels:["project:gardenio"]. Without the kind filter the daemon's
	// Total covers all kinds and the tile inflates by ~10x.
	wantStatuses := map[string]bool{"open": false, "in_progress": false, "blocked": false, "deferred": false}
	sawDecisionWithKind := false
	for _, q := range f.listAllQ {
		if !slices.Contains(q.Labels, "project:gardenio") {
			continue
		}
		if !slices.Contains(q.Kinds, "issue") {
			continue
		}
		if slices.Contains(q.Types, "decision") {
			sawDecisionWithKind = true
			continue
		}
		for _, s := range q.Statuses {
			if _, ok := wantStatuses[s]; ok {
				wantStatuses[s] = true
			}
		}
	}
	for s, ok := range wantStatuses {
		if !ok {
			t.Errorf("expected a per-project count query with Kinds=[issue], status=%s, labels=[project:gardenio]; saw queries: %+v", s, f.listAllQ)
		}
	}
	if !sawDecisionWithKind {
		t.Errorf("expected the decisions count query to also carry Kinds=[issue]; saw queries: %+v", f.listAllQ)
	}
}

// TestHandleIndex_ConfigValidityBadge_Valid verifies a project with a
// registered, well-formed config bead renders the green "✓ config" validity
// badge on its index card (design doc §8, ka-0000002729).
func TestHandleIndex_ConfigValidityBadge_Valid(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio", BeadID: "gd-cfg1"},
	}
	// A config bead whose fields all parse cleanly → zero findings → green.
	f.beads["gd-cfg1"] = &beadsapi.BeadDetail{
		ID:     "gd-cfg1",
		Type:   "project",
		Fields: map[string]string{"prefix": "kd", "cpu_limit": "2000m"},
	}
	f.listResp = &beadsapi.ListBeadsResult{Total: 0}
	f.decisionResp = &beadsapi.ListBeadsResult{Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "✓ config") {
		t.Errorf("expected green '✓ config' validity badge for a valid project\nbody=%s", body)
	}
	if strings.Contains(body, "config error") {
		t.Errorf("did not expect a config-error badge for a valid project")
	}
}

// TestHandleIndex_ConfigValidityBadge_Error verifies a project whose config
// bead carries a field the controller silently drops (malformed JSON / bad
// quantity / dangling account) renders the red error badge with a count.
func TestHandleIndex_ConfigValidityBadge_Error(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio", BeadID: "gd-cfg1"},
	}
	// secrets is not a JSON array (object) → silently dropped by the controller
	// → error finding; cpu_limit is not a K8s quantity → second error.
	f.beads["gd-cfg1"] = &beadsapi.BeadDetail{
		ID:     "gd-cfg1",
		Type:   "project",
		Fields: map[string]string{"secrets": `{"not":"an array"}`, "cpu_limit": "notaquantity"},
	}
	f.listResp = &beadsapi.ListBeadsResult{Total: 0}
	f.decisionResp = &beadsapi.ListBeadsResult{Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "2 config errors") {
		t.Errorf("expected '2 config errors' in red badge\nbody=%s", body)
	}
}

// TestHandleIndex_ConfigValidityBadge_DanglingAccount verifies the index runs
// the FULL validity check set (including account-reference resolution), unlike
// the hub's eager validity which skips it. A config referencing an account
// that no live type:account bead resolves to → red error.
func TestHandleIndex_ConfigValidityBadge_DanglingAccount(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio", BeadID: "gd-cfg1"},
	}
	f.beads["gd-cfg1"] = &beadsapi.BeadDetail{
		ID:     "gd-cfg1",
		Type:   "project",
		Fields: map[string]string{"account": "ghost-account"},
	}
	// One live account bead, but not "ghost-account" → dangling reference.
	f.accountResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{ID: "gd-acc1", Type: "account", Title: "real-account"}},
		Total: 1,
	}
	f.listResp = &beadsapi.ListBeadsResult{Total: 0}
	f.decisionResp = &beadsapi.ListBeadsResult{Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "config error") {
		t.Errorf("expected a config-error badge for a dangling account reference\nbody=%s", body)
	}
}

// TestHandleIndex_NoValidityBadgeForLabelOnlyProject verifies a project with
// no registered config bead (label-only) renders NO validity badge — the zero
// report's OK() is true, so the HasConfig gate must suppress a misleading green.
func TestHandleIndex_NoValidityBadgeForLabelOnlyProject(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"labelonly": {Name: "labelonly"}, // no BeadID
	}
	f.listResp = &beadsapi.ListBeadsResult{Total: 0}
	f.decisionResp = &beadsapi.ListBeadsResult{Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "✓ config") || strings.Contains(body, "config error") || strings.Contains(body, "config warning") {
		t.Errorf("label-only project must render no config-validity badge\nbody=%s", body)
	}
}

// TestHandleIndex_StaleBadge verifies the per-project stale-in-progress health
// flag renders when the daemon reports stale beads, and that the stale count
// query carries the in_progress status + a non-zero updated_before cutoff.
func TestHandleIndex_StaleBadge(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
	}
	f.listResp = &beadsapi.ListBeadsResult{Total: 0}
	f.decisionResp = &beadsapi.ListBeadsResult{Total: 0}
	f.staleResp = &beadsapi.ListBeadsResult{Total: 4}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "4 stale") {
		t.Errorf("expected '4 stale' in badge\nbody=%s", body)
	}

	// A stale-count query must carry in_progress + a non-zero UpdatedBefore.
	sawStaleQuery := false
	for _, q := range f.listAllQ {
		if slices.Contains(q.Labels, "project:gardenio") && slices.Contains(q.Statuses, "in_progress") && !q.UpdatedBefore.IsZero() {
			sawStaleQuery = true
		}
	}
	if !sawStaleQuery {
		t.Errorf("expected a stale-count query with in_progress + non-zero UpdatedBefore; saw: %+v", f.listAllQ)
	}
}

// TestHandleIndex_NoStaleBadgeWhenZero verifies the stale badge is suppressed
// when no in-progress bead is stale.
func TestHandleIndex_NoStaleBadgeWhenZero(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
	}
	f.listResp = &beadsapi.ListBeadsResult{Total: 0}
	f.decisionResp = &beadsapi.ListBeadsResult{Total: 0}
	f.staleResp = &beadsapi.ListBeadsResult{Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), " stale</a>") {
		t.Errorf("expected no stale badge when stale count is zero")
	}
}

// TestHandleIndex_RecentActivity verifies the recent-activity feed renders
// the most-recently-updated in-progress work beads with title + project +
// updated-ago. Adds the second below-the-fold panel called out in ka-0000002715.
func TestHandleIndex_RecentActivity(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-r1",
				Title:     "First recent",
				Type:      "task",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: time.Now().Add(-3 * time.Hour),
			},
			{
				ID:        "gd-r2",
				Title:     "Second recent",
				Type:      "bug",
				Status:    "in_progress",
				UpdatedAt: time.Now().Add(-30 * time.Minute),
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		">Recent activity<",
		`index-recent-list`,
		`href="/gd-r1"`,
		"First recent",
		`href="/p/gardenio"`,
		`href="/gd-r2"`,
		"Second recent",
		`>3h<`,  // formatAgo(UpdatedAt - 3h)
		`>30m<`, // formatAgo(UpdatedAt - 30m)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("recent activity panel missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleIndex_RecentActivity_FiltersNonActionable verifies the recent-
// activity feed only queries for in-progress beads and drops non-actionable
// meta types (agent, decision, schedule, advice, rating, generic, mail,
// config) from the response. Per Matt Baker: "we shouldn't show agents,
// tasks, triggers, decisions, etc there; just in progress work."
func TestHandleIndex_RecentActivity_FiltersNonActionable(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-keep1", Title: "Real work in flight", Type: "task", Status: "in_progress", UpdatedAt: time.Now().Add(-5 * time.Minute)},
			{ID: "gd-drop-agent", Title: "Some agent pod", Type: "agent", Status: "in_progress", UpdatedAt: time.Now().Add(-6 * time.Minute)},
			{ID: "gd-drop-decision", Title: "Pending decision", Type: "decision", Status: "in_progress", UpdatedAt: time.Now().Add(-7 * time.Minute)},
			{ID: "gd-drop-schedule", Title: "Gardener trigger", Type: "schedule", Status: "in_progress", UpdatedAt: time.Now().Add(-8 * time.Minute)},
			{ID: "gd-drop-advice", Title: "Advice bead", Type: "advice", Status: "in_progress", UpdatedAt: time.Now().Add(-9 * time.Minute)},
			{ID: "gd-keep2", Title: "Bug in flight", Type: "bug", Status: "in_progress", UpdatedAt: time.Now().Add(-10 * time.Minute)},
		},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Find the ListBeadsFiltered call that drives Recent activity (sorted
	// by -updated_at, status=in_progress, no Types filter — the index's
	// right-rail live-agents / open-decisions panels also sort by
	// -updated_at but restrict by Types, so we disambiguate on that).
	var recentQ *beadsapi.ListBeadsQuery
	for i := range f.listAllQ {
		q := f.listAllQ[i]
		if q.Sort == "-updated_at" && len(q.Types) == 0 {
			qq := q
			recentQ = &qq
			break
		}
	}
	if recentQ == nil {
		t.Fatal("expected a ListBeadsFiltered call with Sort=-updated_at for recent activity")
	}
	if len(recentQ.Statuses) != 1 || recentQ.Statuses[0] != "in_progress" {
		t.Errorf("recent-activity query Statuses = %v, want [in_progress]", recentQ.Statuses)
	}

	body := w.Body.String()
	for _, want := range []string{
		`href="/gd-keep1"`,
		`href="/gd-keep2"`,
		"Real work in flight",
		"Bug in flight",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("recent activity should contain %q\nbody=%s", want, body)
		}
	}
	for _, unwanted := range []string{
		`href="/gd-drop-agent"`,
		`href="/gd-drop-decision"`,
		`href="/gd-drop-schedule"`,
		`href="/gd-drop-advice"`,
		"Some agent pod",
		"Pending decision",
		"Gardener trigger",
		"Advice bead",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("recent activity should NOT contain %q\nbody=%s", unwanted, body)
		}
	}
}

// TestHandleIndex_RightRail verifies the three right-rail panels added in
// ka-0000006596: Hot ready (top P0/P1 ready beads), Live agents
// (in_progress agent beads), and Open decisions. Each panel renders its
// canned data; the daemon queries are wired to dedicated fake response
// fields so legacy listResp doesn't leak.
func TestHandleIndex_RightRail(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	f.hotReadyResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-hot1",
				Title:     "Critical hot bead",
				Type:      "bug",
				Status:    "open",
				Priority:  0,
				Labels:    []string{"project:gardenio"},
				UpdatedAt: time.Now().Add(-12 * time.Minute),
			},
			{
				ID:        "gd-hot2",
				Title:     "High-pri ready task",
				Type:      "task",
				Status:    "open",
				Priority:  1,
				UpdatedAt: time.Now().Add(-2 * time.Hour),
			},
		},
		Total: 2,
	}
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-ag1",
				Title:  "agent-pod-alpha",
				Type:   "agent",
				Status: "in_progress",
				Labels: []string{"project:gardenio"},
				Fields: map[string]string{
					"task_id":     "gd-task1",
					"agent_state": "working",
				},
				UpdatedAt: time.Now().Add(-4 * time.Minute),
			},
		},
		Total: 1,
	}
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-dec1",
				Title:     "Pick auth strategy",
				Type:      "decision",
				Status:    "open",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: time.Now().Add(-25 * time.Minute),
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`index-columns-3`,
		`>Hot ready<`,
		`href="/gd-hot1"`,
		"Critical hot bead",
		`chip-priority-0`,
		`href="/gd-hot2"`,
		"High-pri ready task",
		`>Live agents<`,
		`href="/gd-ag1"`,
		"agent-pod-alpha",
		`href="/gd-task1"`,
		`>Open decisions<`,
		`href="/gd-dec1"`,
		"Pick auth strategy",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("right rail missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleIndex_RightRail_EmptyStates verifies each panel renders an
// empty-state message when its data source returns nothing — the page
// shouldn't go blank or 500 just because there's no hot work to surface.
func TestHandleIndex_RightRail_EmptyStates(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`>Hot ready<`,
		"Ready queue empty.",
		`>Live agents<`,
		"No live agents.",
		`>Open decisions<`,
		"No open decisions.",
		`>Hot epics<`,
		"No open epics.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("right rail empty state missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleIndex_HotEpics verifies the right-rail "Hot epics" panel renders
// open epic beads sorted by most-recently-updated. Adds the panel called out
// in ka-0000006022 as filling the void left of Live agents.
func TestHandleIndex_HotEpics(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-epic1",
				Title:     "Auth refactor umbrella",
				Type:      "epic",
				Status:    "open",
				Priority:  1,
				Labels:    []string{"project:gardenio"},
				UpdatedAt: time.Now().Add(-6 * time.Minute),
			},
			{
				ID:        "gd-epic2",
				Title:     "Beads-viewer density",
				Type:      "epic",
				Status:    "open",
				Priority:  2,
				Labels:    []string{"project:gardenio"},
				UpdatedAt: time.Now().Add(-2 * time.Hour),
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`>Hot epics<`,
		`href="/gd-epic1"`,
		"Auth refactor umbrella",
		`href="/gd-epic2"`,
		"Beads-viewer density",
		`chip-priority-1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hot epics panel missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleIndex_StatsBanner_EscalationsAndP0 verifies the banner promotes
// escalations-pending and P0-open counts from /health (ka-0000006022).
func TestHandleIndex_StatsBanner_EscalationsAndP0(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	f.statsResp = &beadsapi.GraphStats{}
	// Three pending escalations for the owner; the banner reports the count.
	// A fourth, a merge the desk has, is in hand and not counted (gd-bmOiteQi36).
	own := map[string]string{"audience": "owner"}
	f.escalationsResp = []beadsapi.EscalationDetail{
		{Issue: &beadsapi.BeadDetail{ID: "gd-esc1", Title: "escalation one", Fields: own}},
		{Issue: &beadsapi.BeadDetail{ID: "gd-esc2", Title: "escalation two", Fields: own}},
		{Issue: &beadsapi.BeadDetail{ID: "gd-esc3", Title: "escalation three", Fields: own}},
		{Issue: &beadsapi.BeadDetail{ID: "gd-esc4", Title: "a merge", Fields: map[string]string{"operation_type": "gitlab_mr_merge"}}},
	}
	// Four P0 issues followed by a P1 — the scan stops at the P1 boundary, so
	// the banner reports four. The trailing P1 should be ignored.
	f.p0ScanResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-p0a", Priority: 0, Status: "open"},
			{ID: "gd-p0b", Priority: 0, Status: "open"},
			{ID: "gd-p0c", Priority: 0, Status: "open"},
			{ID: "gd-p0d", Priority: 0, Status: "open"},
			{ID: "gd-p1a", Priority: 1, Status: "open"},
		},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`href="/escalations">3<`,
		`stats-banner-label">escalations<`,
		`stats-banner-num">4<`, // P0 open
		`stats-banner-label">P0 open<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stats banner missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleIndex_RightRail_HotReadyQueryShape pins the daemon query the
// Hot ready panel issues. Before ka-0000008099 the query was
// {Statuses:[open], NoOpenDeps, Sort:"priority"} with a client-side
// denylist of meta types — the missing sort tiebreaker meant Postgres
// returned P0 rows in insertion order, surfacing months-old session /
// trigger / event beads. The fix narrows Types to issueMacroTypes
// (task/bug/epic/...) so meta types never enter the response, and adds
// -updated_at as a secondary sort so the freshest in-tier bead wins.
func TestHandleIndex_RightRail_HotReadyQueryShape(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	f.hotReadyResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-keep", Title: "Real ready work", Type: "task", Status: "open", Priority: 1},
		},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/gd-keep"`) {
		t.Errorf("Hot ready should render the seeded bead\nbody=%s", body)
	}

	var hotQ *beadsapi.ListBeadsQuery
	for i := range f.listAllQ {
		q := f.listAllQ[i]
		if q.NoOpenDeps && slices.Contains(q.Statuses, "open") && len(q.Types) > 0 {
			hotQ = &f.listAllQ[i]
			break
		}
	}
	if hotQ == nil {
		t.Fatalf("expected Hot ready query with NoOpenDeps + open + Types narrowing, got none in %d calls", len(f.listAllQ))
	}
	if !slices.Equal(hotQ.Types, issueMacroTypes) {
		t.Errorf("Hot ready Types should be issueMacroTypes %v, got %v", issueMacroTypes, hotQ.Types)
	}
	if hotQ.Sort != "priority,-updated_at" {
		t.Errorf("Hot ready Sort should be %q to break ties by recency, got %q", "priority,-updated_at", hotQ.Sort)
	}
}

// TestHandleIndex_LiveAgents_MatchesAgentsPageSemantics verifies the bug
// from ka-0000000513: the /index "Live agents" rail and banner "agents live"
// stat used to query status=in_progress only, missing the common
// status=open + agent_state=working case. /agents reported six alive while
// /index reported zero on the same page load. The fix queries
// status open|in_progress and filters by agent_state in the alive set
// (working / spawning / yielding) — matching /agents.
func TestHandleIndex_LiveAgents_MatchesAgentsPageSemantics(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	now := time.Now()
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// Newly spawned: open + spawning → alive. Shows up on /agents and
			// must show up here too.
			{
				ID:        "gd-alive-spawning",
				Title:     "agent-pod-spawning",
				Type:      "agent",
				Status:    "open",
				Labels:    []string{"project:gardenio"},
				Fields:    map[string]string{"agent_state": "spawning", "task_id": "gd-t-spawn"},
				UpdatedAt: now.Add(-2 * time.Minute),
			},
			// Running: open + working → alive. The original bug filtered this
			// out because status was "open", not "in_progress".
			{
				ID:        "gd-alive-working-open",
				Title:     "agent-pod-working-open",
				Type:      "agent",
				Status:    "open",
				Labels:    []string{"project:gardenio"},
				Fields:    map[string]string{"agent_state": "working", "task_id": "gd-t-working1"},
				UpdatedAt: now.Add(-3 * time.Minute),
			},
			// Running: in_progress + working → alive. Already covered before.
			{
				ID:        "gd-alive-working-ip",
				Title:     "agent-pod-working-ip",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				Fields:    map[string]string{"agent_state": "working", "task_id": "gd-t-working2"},
				UpdatedAt: now.Add(-4 * time.Minute),
			},
			// Yielding (awaiting decision): in_progress + yielding → alive.
			{
				ID:        "gd-alive-yielding",
				Title:     "agent-pod-yielding",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				Fields:    map[string]string{"agent_state": "yielding", "task_id": "gd-t-yield"},
				UpdatedAt: now.Add(-5 * time.Minute),
			},
			// Idle: in_progress + idle → NOT alive. /agents classifies as Idle.
			{
				ID:        "gd-idle",
				Title:     "agent-pod-idle",
				Type:      "agent",
				Status:    "in_progress",
				Fields:    map[string]string{"agent_state": "idle", "task_id": "gd-t-idle"},
				UpdatedAt: now.Add(-6 * time.Minute),
			},
			// Stuck: in_progress + failed → NOT alive. /agents classifies as Stuck.
			{
				ID:        "gd-stuck",
				Title:     "agent-pod-failed",
				Type:      "agent",
				Status:    "in_progress",
				Fields:    map[string]string{"agent_state": "failed", "task_id": "gd-t-fail"},
				UpdatedAt: now.Add(-7 * time.Minute),
			},
		},
		Total: 6,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// All four alive agents must surface in the Live agents rail, regardless
	// of whether their bead status is open or in_progress.
	for _, id := range []string{
		"gd-alive-spawning",
		"gd-alive-working-open",
		"gd-alive-working-ip",
		"gd-alive-yielding",
	} {
		if !strings.Contains(body, `href="/`+id+`"`) {
			t.Errorf("Live agents rail missing alive agent %s\nbody=%s", id, body)
		}
	}
	// Idle and stuck agents must NOT appear in the rail panel — they show up
	// on /agents under their own buckets, not as "Live".
	for _, id := range []string{"gd-idle", "gd-stuck"} {
		if strings.Contains(body, `href="/`+id+`"`) {
			t.Errorf("Live agents rail must not include non-alive agent %s\nbody=%s", id, body)
		}
	}
	// The "No live agents." empty-state must not render when alive agents
	// exist — that was the visible symptom of the original bug.
	if strings.Contains(body, "No live agents.") {
		t.Errorf("Live agents empty state rendered despite four alive agents\nbody=%s", body)
	}
	// Banner "agents live" count must equal the number of alive agents (4),
	// not the total active set (6). The banner pre-fix reported 0 because
	// agents typically run with status=open.
	if !strings.Contains(body, `>4</span>
    <span class="stats-banner-label">agents live`) {
		t.Errorf("stats banner agents-live count should be 4, body=%s", body)
	}
}

// TestHandleIndex_LiveAgents_DensifiedRows verifies the ka-0000007459 slice:
// each Live agents row surfaces the agent_state chip and a compact operator
// handle (operated_by, provider prefix stripped), and the panel renders well
// past the old 8-row cap so a busy fleet stays visible on Home.
func TestHandleIndex_LiveAgents_DensifiedRows(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	now := time.Now()

	// Seed 14 alive agents — more than the previous 8-row cap — so the test
	// guards against a regression to the old truncation. Vary the operator
	// source: OperatedByDisplay, OperatedBy, and a fields["operated_by"] with a
	// "slack:" prefix that must be stripped to a bare handle.
	var beads []*beadsapi.BeadDetail
	for i := range 14 {
		beads = append(beads, &beadsapi.BeadDetail{
			ID:        fmt.Sprintf("gd-live-%02d", i),
			Title:     fmt.Sprintf("agent-pod-%02d", i),
			Type:      "agent",
			Status:    "open",
			Labels:    []string{"project:gardenio"},
			Fields:    map[string]string{"agent_state": "working", "task_id": fmt.Sprintf("gd-task-%02d", i)},
			UpdatedAt: now.Add(time.Duration(-i) * time.Minute),
		})
	}
	beads[0].OperatedByDisplay = "Matt Baker"
	beads[1].OperatedBy = "alice"
	beads[2].Fields["operated_by"] = "slack:U0OPERATOR1"
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: beads, Total: len(beads)}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// At least 12 alive agents must render — the raised cap is the headline of
	// this slice (target ~12+ visible at 1440x900).
	for _, id := range []string{"gd-live-00", "gd-live-08", "gd-live-11"} {
		if !strings.Contains(body, `href="/`+id+`"`) {
			t.Errorf("Live agents rail missing %s past old 8-row cap\nbody=%s", id, body)
		}
	}

	// Each row carries an agent_state chip.
	if !strings.Contains(body, `agent-state-working index-rail-agent-state`) {
		t.Errorf("Live agents row missing agent_state chip\nbody=%s", body)
	}

	// Operators render as compact initials chips (slice 3, ka-0000003997),
	// superseding the raw "@handle" span. "Matt Baker" → MB monogram; the raw
	// slack: provider id is never leaked into the row.
	if !strings.Contains(body, `operator-chip-compact`) {
		t.Errorf("Live agents row missing compact operator chip\nbody=%s", body)
	}
	if !strings.Contains(body, `>MB<`) {
		t.Errorf("Live agents row missing MB initials chip for OperatedByDisplay\nbody=%s", body)
	}
	if strings.Contains(body, `@slack:U0OPERATOR1`) {
		t.Errorf("Live agents row leaked slack: provider prefix\nbody=%s", body)
	}
}

// TestHandleIndex_LiveAgents_EnrichedRows verifies the second ka-0000007459
// slice: each Live agents row surfaces the audit-ranked richer per-agent
// signals (ka-0000008016) — role chip, linked task title (not a bare id),
// uptime, a stale flag for wedged agents, and compact MR + decision badges —
// all derived from the agent bead already fetched (no extra round-trip).
func TestHandleIndex_LiveAgents_EnrichedRows(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	now := time.Now()

	// A gardener claiming an MR, freshly active.
	cap := &beadsapi.BeadDetail{
		ID:     "gd-cap-01",
		Title:  "gardener-delivery",
		Type:   "agent",
		Status: "in_progress",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"agent_state": "working",
			"role":        "gardener",
			"task_id":     "gd-task-aa",
			"task_title":  "Drive MR throughput",
			"mr_url":      "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/9001",
		},
		CreatedAt: now.Add(-90 * time.Minute),
		UpdatedAt: now.Add(-2 * time.Minute),
	}
	// A crew blocked on a decision.
	crew := &beadsapi.BeadDetail{
		ID:     "gd-crew-01",
		Title:  "crew-fix-ci",
		Type:   "agent",
		Status: "open",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"agent_state":      "yielding",
			"role":             "crew",
			"task_id":          "gd-task-bb",
			"task_title":       "Fix CI on auth flow",
			"decision_bead_id": "gd-dec-77",
		},
		CreatedAt: now.Add(-30 * time.Minute),
		UpdatedAt: now.Add(-1 * time.Minute),
	}
	// A stale agent: alive state pill but no activity for well over an hour.
	stale := &beadsapi.BeadDetail{
		ID:     "gd-stale-01",
		Title:  "crew-wedged",
		Type:   "agent",
		Status: "in_progress",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"agent_state": "working",
			"role":        "crew",
			"task_id":     "gd-task-cc",
		},
		CreatedAt: now.Add(-5 * time.Hour),
		UpdatedAt: now.Add(-3 * time.Hour),
	}
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{cap, crew, stale},
		Total: 3,
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Role chips render color-coded per role.
	if !strings.Contains(body, `index-rail-agent-role-gardener`) {
		t.Errorf("missing gardener role chip\nbody=%s", body)
	}
	if !strings.Contains(body, `index-rail-agent-role-crew`) {
		t.Errorf("missing crew role chip\nbody=%s", body)
	}

	// Task title is rendered (preferred over the bare task id) and links to it.
	if !strings.Contains(body, `Drive MR throughput`) {
		t.Errorf("missing task title text\nbody=%s", body)
	}
	if !strings.Contains(body, `href="/gd-task-aa"`) {
		t.Errorf("task title should link to the task bead\nbody=%s", body)
	}

	// MR badge links out to GitLab; decision badge links to the decision bead.
	if !strings.Contains(body, `merge_requests/9001`) || !strings.Contains(body, `index-rail-agent-mr`) {
		t.Errorf("missing MR badge for MR-claiming agent\nbody=%s", body)
	}
	if !strings.Contains(body, `href="/gd-dec-77"`) || !strings.Contains(body, `index-rail-agent-dec`) {
		t.Errorf("missing decision badge for blocked agent\nbody=%s", body)
	}

	// Uptime renders ("up <dur>"); the stale agent is flagged.
	if !strings.Contains(body, `index-rail-agent-uptime`) || !strings.Contains(body, `up `) {
		t.Errorf("missing uptime label\nbody=%s", body)
	}
	if !strings.Contains(body, `index-rail-agent-stale`) {
		t.Errorf("stale agent (no activity > 1h) should be flagged\nbody=%s", body)
	}
}

// TestHandleIndex_ActiveAgentsPrimary verifies the ka-0000007459 redesign:
// active agents are promoted out of the right rail into a full-width PRIMARY
// "Active agents" card grid at the top of Home, each card carries the new
// pod_phase chip, and a "full view" link points to /active (Home SUPPLEMENTS
// /active rather than replacing it). The relocated secondary "Platform pulse"
// rail is now collapsible.
func TestHandleIndex_ActiveAgentsPrimary(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	now := time.Now()

	withTask := &beadsapi.BeadDetail{
		ID:     "gd-prim-01",
		Title:  "agent-with-task",
		Type:   "agent",
		Status: "in_progress",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"agent_state": "working",
			"role":        "crew",
			"task_id":     "gd-task-zz",
			"task_title":  "Wire the thing",
			"pod_phase":   "Running",
		},
		CreatedAt: now.Add(-20 * time.Minute),
		UpdatedAt: now.Add(-1 * time.Minute),
	}
	// An agent that has not yet claimed a task and whose pod is still Pending.
	noTask := &beadsapi.BeadDetail{
		ID:     "gd-prim-02",
		Title:  "agent-no-task",
		Type:   "agent",
		Status: "open",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"agent_state": "spawning",
			"pod_phase":   "Pending",
		},
		CreatedAt: now.Add(-2 * time.Minute),
		UpdatedAt: now.Add(-30 * time.Second),
	}
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{withTask, noTask},
		Total: 2,
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The primary section + card grid render, with both agents as cards.
	for _, want := range []string{
		`index-active-grid`,
		`index-active-card`,
		`href="/gd-prim-01"`,
		`href="/gd-prim-02"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("active-agents primary section missing %q\nbody=%s", want, body)
		}
	}

	// Pod-phase chip renders for both states.
	if !strings.Contains(body, `index-rail-agent-pod-Running`) {
		t.Errorf("missing Running pod-phase chip\nbody=%s", body)
	}
	if !strings.Contains(body, `pod: Pending`) {
		t.Errorf("missing Pending pod-phase chip\nbody=%s", body)
	}

	// The task-less agent shows the no-task affordance, not a dangling link.
	if !strings.Contains(body, `no task claimed`) {
		t.Errorf("task-less agent should render the no-task affordance\nbody=%s", body)
	}

	// Home supplements /active: a full-view link points there.
	if !strings.Contains(body, `href="/active"`) {
		t.Errorf("missing full-view link to /active\nbody=%s", body)
	}

	// The relocated secondary rail is collapsible.
	if !strings.Contains(body, `index-rail-collapse`) {
		t.Errorf("secondary platform-pulse rail should be collapsible\nbody=%s", body)
	}

	// The active-agents empty-state must NOT render when agents are present.
	if strings.Contains(body, "No live agents.") {
		t.Errorf("empty state rendered despite alive agents\nbody=%s", body)
	}
}

func TestCountUniqueFields(t *testing.T) {
	cases := []struct {
		name string
		in   []beadsapi.ConfigEntry
		want int
	}{
		{"empty", nil, 0},
		{"single type", []beadsapi.ConfigEntry{
			{Key: "type:task", Value: []byte(`{"fields":[{"name":"a"},{"name":"b"}]}`)},
		}, 2},
		{"dedup across types", []beadsapi.ConfigEntry{
			{Key: "type:task", Value: []byte(`{"fields":[{"name":"a"},{"name":"b"}]}`)},
			{Key: "type:bug", Value: []byte(`{"fields":[{"name":"a"},{"name":"c"}]}`)},
		}, 3},
		{"malformed entries skipped", []beadsapi.ConfigEntry{
			{Key: "type:task", Value: []byte(`{"fields":[{"name":"a"}]}`)},
			{Key: "type:broken", Value: []byte(`not json`)},
			{Key: "type:empty", Value: nil},
		}, 1},
		{"empty field names ignored", []beadsapi.ConfigEntry{
			{Key: "type:task", Value: []byte(`{"fields":[{"name":""},{"name":"a"}]}`)},
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countUniqueFields(tc.in); got != tc.want {
				t.Errorf("countUniqueFields = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestHandleProject_ListsOpenBeads(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-p1", Title: "First open", Type: "task", Status: "open"},
			{ID: "gd-p2", Title: "Second open", Type: "bug", Status: "open"},
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

	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	if got := f.listLastQ.Statuses; len(got) != 1 || got[0] != "open" {
		t.Errorf("statuses = %v, want [open]", got)
	}
	if got := f.listLastQ.Labels; len(got) != 1 || got[0] != "project:gardenio" {
		t.Errorf("labels = %v, want [project:gardenio]", got)
	}
	if f.listLastQ.Limit != projectListLimit {
		t.Errorf("limit = %d, want %d", f.listLastQ.Limit, projectListLimit)
	}

	for _, want := range []string{
		"Project: gardenio",
		"gd-p1", "First open", "task",
		"gd-p2", "Second open", "bug",
		`href="/gd-p1"`,
		`href="/gd-p2"`,
		`href="/"`, // back link
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project page missing %q\nbody=%s", want, body)
		}
	}
	// No truncation banner when total == len(results).
	if strings.Contains(body, "showing first") {
		t.Errorf("unexpected truncation banner when total matches rendered count")
	}
}

func TestHandleProject_DefaultTypeFilterIsIssuesMacro(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	got := append([]string{}, f.listLastQ.Types...)
	sort.Strings(got)
	want := []string{"bug", "chore", "epic", "feature", "rich-report", "task"}
	if len(got) != len(want) {
		t.Fatalf("default Types = %v, want %v", got, want)
	}
	for i, v := range got {
		if v != want[i] {
			t.Errorf("default Types[%d] = %q, want %q", i, v, want[i])
		}
	}

	body := w.Body.String()
	// Issues chip is active by default.
	if !strings.Contains(body, `chip chip-active`) {
		t.Errorf("expected an active chip, body=%s", body)
	}
	for _, want := range []string{
		">Issues<", ">All<", ">Tasks<", ">Features<", ">Bugs<",
		">Triggers<", ">Trigger beads<", ">Decisions<", ">Agents<", ">Advice<",
		`href="/p/gardenio?type=all"`,
		`href="/p/gardenio?type=task"`,
		`href="/p/gardenio?type=trigger"`,
		`href="/p/gardenio?type=decision"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project page missing %q\nbody=%s", want, body)
		}
	}
}

func TestHandleProject_ExplicitTypeFilter(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?type=task", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	if got := f.listLastQ.Types; len(got) != 1 || got[0] != "task" {
		t.Errorf("Types = %v, want [task]", got)
	}
}

func TestHandleProject_TypeFilterAll(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?type=all", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	if got := f.listLastQ.Types; len(got) != 0 {
		t.Errorf("Types for all = %v, want empty", got)
	}
}

func TestHandleProject_TypeFilterDecisionDeepLink(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-d1", Title: "A decision", Type: "decision", Status: "open"},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?type=decision", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	if got := f.listLastQ.Types; len(got) != 1 || got[0] != "decision" {
		t.Errorf("Types = %v, want [decision]", got)
	}
	body := w.Body.String()
	// The Decisions chip should be active.
	if !strings.Contains(body, `aria-current="true"`) || !strings.Contains(body, ">Decisions<") {
		t.Errorf("expected Decisions chip marked active, body=%s", body)
	}
}

func TestHandleProject_URLStateRoundTrip(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?type=task&status=open&label=foo&q=bar", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}

	if got := f.listLastQ.Types; len(got) != 1 || got[0] != "task" {
		t.Errorf("Types = %v, want [task]", got)
	}
	if got := f.listLastQ.Statuses; len(got) != 1 || got[0] != "open" {
		t.Errorf("Statuses = %v, want [open]", got)
	}
	wantLabels := []string{"project:gardenio", "foo"}
	if got := f.listLastQ.Labels; len(got) != len(wantLabels) {
		t.Fatalf("Labels = %v, want %v", got, wantLabels)
	} else {
		for i, v := range wantLabels {
			if got[i] != v {
				t.Errorf("Labels[%d] = %q, want %q", i, got[i], v)
			}
		}
	}
	if f.listLastQ.Search != "bar" {
		t.Errorf("Search = %q, want %q", f.listLastQ.Search, "bar")
	}

	body := w.Body.String()
	// Tasks chip should be active.
	if !strings.Contains(body, `aria-current="true"`) || !strings.Contains(body, ">Tasks<") {
		t.Errorf("expected Tasks chip marked active, body=%s", body)
	}
	// Chip URLs must preserve the other filter dimensions when only changing type.
	// url.Values.Encode() sorts keys alphabetically: label, q, status, type.
	for _, want := range []string{
		`label=foo`,
		`q=bar`,
		`status=open`,
		`type=all`,
		`type=decision`,
		`hx-push-url="true"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project page missing %q\nbody=%s", want, body)
		}
	}
}

// A ?status= combination with no dropdown preset reaches the daemon whole and
// shows as the selected option, rather than collapsing to "open" (the
// filter_chips.spec.js multi-select contract, gd-l6pZ3yS023).
func TestHandleProject_StatusCombinationDeepLink(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?type=all&status=open,closed", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	if got := f.listLastQ.Statuses; !slices.Equal(got, []string{"open", "closed"}) {
		t.Errorf("Statuses = %v, want [open closed]", got)
	}
	if body := w.Body.String(); !strings.Contains(body, `<option value="open,closed" selected>Open &#43; Closed</option>`) {
		t.Errorf("status select does not show the combination as selected\nbody=%s", body)
	}
}

func TestHandleProject_UnknownTypeFallsBackToIssues(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?type=garbage", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	got := append([]string{}, f.listLastQ.Types...)
	sort.Strings(got)
	want := []string{"bug", "chore", "epic", "feature", "rich-report", "task"}
	if len(got) != len(want) {
		t.Fatalf("Types for unknown filter = %v, want %v", got, want)
	}
}

func TestHandleProject_SearchInputRendered(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?q=foo&type=task&status=open&label=bar", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`type="search"`,
		`name="q"`,
		`value="foo"`,
		`placeholder="Filter beads..."`,
		`<form class="project-search"`,
		`method="get"`,
		`hx-trigger="input changed delay:300ms, search"`,
		`hx-push-url="true"`,
		// Hidden inputs preserve other filter dimensions across htmx submits.
		`<input type="hidden" name="type" value="task">`,
		`<input type="hidden" name="status" value="open">`,
		`<input type="hidden" name="label" value="bar">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("search input missing %q\nbody=%s", want, body)
		}
	}
}

func TestHandleProject_SearchInputEmptyWhenNoQ(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `name="q"`) {
		t.Errorf("search input missing on /p/gardenio (no query)\nbody=%s", body)
	}
	if !strings.Contains(body, `value=""`) {
		t.Errorf("expected empty value=\"\" when ?q is absent\nbody=%s", body)
	}
}

func TestHandleProject_RichRowContent(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:          "gd-rich1",
				Title:       "A rich row",
				Type:        "task",
				Status:      "open",
				Priority:    0,
				Assignee:    "alice",
				Description: "This is a description preview that should appear in the row.",
				UpdatedAt:   time.Now().Add(-2 * time.Hour),
			},
			{
				ID:       "gd-rich2",
				Title:    "Low priority no desc",
				Type:     "bug",
				Status:   "open",
				Priority: 3, // should NOT show priority chip (only ≤1)
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?type=all", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{
		"gd-rich1", "A rich row", "alice",
		"This is a description preview",
		`class="status-pill`,
		`class="chip-type chip-type-task"`,
		`class="chip-priority chip-priority-0"`, // P0 chip rendered
		`>2h<`,                                  // updated 2h ago
		"gd-rich2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rich row missing %q\nbody=%s", want, body)
		}
	}
	// Priority 3 row must NOT have a priority chip.
	if strings.Contains(body, `chip-priority-3`) {
		t.Errorf("priority 3 should not render a chip-priority badge")
	}
	// Existing prefetch attrs preserved on title link.
	if !strings.Contains(body, `hx-get="/gd-rich1"`) || !strings.Contains(body, `hx-trigger="mouseover once"`) {
		t.Errorf("expected prefetch hx-get/hx-trigger on title link, body=%s", body)
	}
}

func TestFormatAgo(t *testing.T) {
	now := time.Now()
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Time{}, ""},
		{now, "just now"},
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m"},
		{now.Add(-3 * time.Hour), "3h"},
		{now.Add(-2 * 24 * time.Hour), "2d"},
		{now.Add(-3 * 7 * 24 * time.Hour), "3w"},
	}
	for _, c := range cases {
		if got := formatAgo(c.in); got != c.want {
			t.Errorf("formatAgo(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveProjectTypeFilter(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "issues"},
		{"  ", "issues"},
		{"issues", "issues"},
		{"all", "all"},
		{"task", "task"},
		{"decision", "decision"},
		{"agent", "agent"},
		{"advice", "advice"},
		{"garbage", "issues"},
		{"epic", "issues"}, // not in chip set → fallback
	}
	for _, c := range cases {
		if got := resolveProjectTypeFilter(c.in); got != c.want {
			t.Errorf("resolveProjectTypeFilter(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveProjectSort(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "priority"},
		{"  ", "priority"},
		{"priority", "priority"},
		{"-updated_at", "-updated_at"},
		{"-created_at", "-created_at"},
		{"created_at", "created_at"},
		{"title", "title"},
		{"garbage", "priority"},
		{"updated_at", "priority"}, // ascending updated_at not in option set → fallback
	}
	for _, c := range cases {
		if got := resolveProjectSort(c.in); got != c.want {
			t.Errorf("resolveProjectSort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveProjectStatus(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "open"},
		{"  ", "open"},
		{"open", "open"},
		{"in_progress", "in_progress"},
		{"open,in_progress", "open,in_progress"},
		{"closed", "closed"},
		{"all", "all"},
		{"garbage", "open"},
		{"deferred", "open"}, // not in option set → fallback
		// A combination of the dropdown's single statuses is kept, so a
		// ?status=open,closed deep link lists both (filter_chips.spec.js).
		{"open,closed", "open,closed"},
		{" closed , open ,closed", "closed,open"},
		{"open, in_progress", "open,in_progress"},
		{"open,deferred", "open"}, // any unknown member → fallback
		{"open,all", "open"},
		{",", "open"},
	}
	for _, c := range cases {
		if got := resolveProjectStatus(c.in); got != c.want {
			t.Errorf("resolveProjectStatus(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestProjectStatusLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"open,in_progress", "Open + in progress"},
		{"all", "All"},
		{"open,closed", "Open + Closed"},
		{"closed,in_progress", "Closed + In progress"},
	}
	for _, c := range cases {
		if got := projectStatusLabel(c.in); got != c.want {
			t.Errorf("projectStatusLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestProjectFilterStatuses(t *testing.T) {
	if got := projectFilterStatuses("all"); got != nil {
		t.Errorf("projectFilterStatuses(all) = %v, want nil", got)
	}
	if got := projectFilterStatuses("open"); len(got) != 1 || got[0] != "open" {
		t.Errorf("projectFilterStatuses(open) = %v, want [open]", got)
	}
	got := projectFilterStatuses("open,in_progress")
	if len(got) != 2 || got[0] != "open" || got[1] != "in_progress" {
		t.Errorf("projectFilterStatuses(open,in_progress) = %v, want [open in_progress]", got)
	}
}

func TestHandleProject_DefaultSortIsPriority(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	if f.listLastQ.Sort != "priority" {
		t.Errorf("default Sort = %q, want %q", f.listLastQ.Sort, "priority")
	}

	body := w.Body.String()
	for _, want := range []string{
		`class="project-controls"`,
		`name="sort"`,
		`<option value="priority" selected>Priority</option>`,
		`<option value="-updated_at">Recently updated</option>`,
		`<option value="-created_at">Recently created</option>`,
		`name="status"`,
		`<option value="open" selected>Open</option>`,
		`<option value="all">All</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project controls missing %q\nbody=%s", want, body)
		}
	}
}

func TestHandleProject_SortByRecency(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?sort=-updated_at", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	if f.listLastQ.Sort != "-updated_at" {
		t.Errorf("Sort = %q, want %q", f.listLastQ.Sort, "-updated_at")
	}

	body := w.Body.String()
	if !strings.Contains(body, `<option value="-updated_at" selected>Recently updated</option>`) {
		t.Errorf("expected -updated_at option marked selected\nbody=%s", body)
	}
}

func TestHandleProject_StatusAllClearsFilter(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?status=all", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	if got := f.listLastQ.Statuses; len(got) != 0 {
		t.Errorf("Statuses for status=all = %v, want empty", got)
	}
}

func TestHandleProject_StatusInProgress(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?status=in_progress", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if got := f.listLastQ.Statuses; len(got) != 1 || got[0] != "in_progress" {
		t.Errorf("Statuses = %v, want [in_progress]", got)
	}
}

func TestHandleProject_ChipsPreserveSort(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?sort=-updated_at&status=closed", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	// Chip hrefs must carry sort + status forward when the user changes type.
	for _, want := range []string{
		`sort=-updated_at`,
		`status=closed`,
		`type=task`,
		`type=all`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("chip href missing %q\nbody=%s", want, body)
		}
	}
}

func TestProjectFilterTypes(t *testing.T) {
	if got := projectFilterTypes("all"); got != nil {
		t.Errorf("projectFilterTypes(all) = %v, want nil", got)
	}
	got := projectFilterTypes("issues")
	sort.Strings(got)
	want := []string{"bug", "chore", "epic", "feature", "rich-report", "task"}
	if len(got) != len(want) {
		t.Fatalf("projectFilterTypes(issues) = %v, want %v", got, want)
	}
	for i, v := range got {
		if v != want[i] {
			t.Errorf("[%d] = %q, want %q", i, v, want[i])
		}
	}
	if got := projectFilterTypes("task"); len(got) != 1 || got[0] != "task" {
		t.Errorf("projectFilterTypes(task) = %v, want [task]", got)
	}
}

func TestHandleProject_TruncationBanner(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{ID: "gd-only", Title: "One", Type: "task", Status: "open"}},
		Total: 250,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "showing first 100") {
		t.Errorf("expected truncation banner, body=%s", w.Body.String())
	}
}

func TestHandleProject_DaemonError(t *testing.T) {
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load beads") {
		t.Errorf("expected error banner, body=%s", w.Body.String())
	}
}

func TestHandleProject_Empty(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/other?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No open beads") {
		t.Errorf("expected empty-state text, body=%s", w.Body.String())
	}
}

func TestHandleProject_EscapesProjectName(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/p/"+`<script>`+"?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "<script>") {
		t.Errorf("project name rendered without escaping, body=%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected project name HTML-escaped, body=%s", body)
	}
}

func TestHandleActive_Empty(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Nothing Active") {
		t.Errorf("expected empty-state copy, body=%s", body)
	}
	// Summary bar should still render with zero counts.
	for _, want := range []string{">Active<", ">Agents<", ">Decisions<"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary bar missing %q\nbody=%s", want, body)
		}
	}
	// Confirm the in-progress query was made with Statuses=[in_progress] and
	// no project label filter when ?project= is absent.
	if len(f.listAllQ) == 0 {
		t.Fatal("expected at least one ListBeadsFiltered call")
	}
	first := f.listAllQ[0]
	if got := first.Statuses; len(got) != 1 || got[0] != "in_progress" {
		t.Errorf("first-call statuses = %v, want [in_progress]", got)
	}
	if len(first.Labels) != 0 {
		t.Errorf("first-call labels = %v, want none (no project filter)", first.Labels)
	}
}

func TestHandleActive_SingleProject_GroupsAndCounts(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-a1", Title: "Working bead", Type: "task", Status: "in_progress",
				Priority: 2, Assignee: "bot-1", Labels: []string{"project:gardenio"}},
			{ID: "gd-a2", Title: "Active agent", Type: "agent", Status: "in_progress",
				Priority: 2, Assignee: "agent-x", Labels: []string{"project:gardenio"}},
		},
		Total: 2,
	}
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-d1", Title: "Pending decision", Type: "decision", Status: "open"},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"<h2>gardenio",
		"gd-a1", "Working bead",
		"gd-a2", "Active agent",
		`href="/gd-a1"`,
		"pending decision", // banner copy
		`href="/search?q=&amp;type=decision&amp;status=open"`, // banner link
	} {
		if !strings.Contains(body, want) {
			t.Errorf("active page missing %q\nbody=%s", want, body)
		}
	}
	// Empty-state must NOT render when there are beads.
	if strings.Contains(body, "Nothing Active") {
		t.Errorf("unexpected empty-state when beads exist")
	}
}

func TestHandleActive_MultiProject_GroupedAndSorted(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-z1", Title: "Zeta one", Type: "task", Status: "in_progress",
				Priority: 2, Labels: []string{"project:zeta"}},
			{ID: "gd-g1", Title: "Gardenio one", Type: "task", Status: "in_progress",
				Priority: 2, Labels: []string{"project:gardenio"}},
			{ID: "gd-u1", Title: "Untagged one", Type: "task", Status: "in_progress",
				Priority: 2, Labels: nil},
		},
		Total: 3,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	// Each bead renders.
	for _, want := range []string{"gd-g1", "gd-u1", "gd-z1"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body=%s", want, body)
		}
	}
	// Project sections render alphabetically: gardenio → untagged → zeta.
	g := strings.Index(body, "<h2>gardenio")
	u := strings.Index(body, "<h2>untagged")
	z := strings.Index(body, "<h2>zeta")
	if g < 0 || u < 0 || z < 0 {
		t.Fatalf("missing one of the project headers: g=%d u=%d z=%d", g, u, z)
	}
	if !(g < u && u < z) {
		t.Errorf("project sections out of order: g=%d u=%d z=%d", g, u, z)
	}
	// No decision banner when decisions count is zero.
	if strings.Contains(body, "pending decision") {
		t.Errorf("unexpected decision banner when zero open decisions")
	}
}

func TestHandleActive_ProjectFilter_AppliesLabelToDaemonQuery(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/active?project=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(f.listAllQ) == 0 {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	first := f.listAllQ[0]
	if got := first.Labels; len(got) != 1 || got[0] != "project:gardenio" {
		t.Errorf("expected label filter [project:gardenio], got %v", got)
	}
	// "← all projects" back link only appears when a project filter is set.
	if !strings.Contains(w.Body.String(), "all projects") {
		t.Errorf("expected back link when project filter is set, body=%s", w.Body.String())
	}
}

// TestHandleActiveSortsByRecency verifies that recency wins over priority on
// /active. Same-project beads must sort UpdatedAt-desc so fresh activity
// floats up; priority is a tiebreaker only. (Plan §3.1.)
func TestHandleActiveSortsByRecency(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// Stale but high priority — would have led under the old sort.
			{ID: "gd-old", Title: "Old high-priority", Type: "task", Status: "in_progress",
				Priority: 0, UpdatedAt: now.Add(-30 * time.Minute),
				Labels: []string{"project:gardenio"}},
			// Fresh but low priority — should now lead.
			{ID: "gd-new", Title: "Recent low-priority", Type: "task", Status: "in_progress",
				Priority: 2, UpdatedAt: now.Add(-30 * time.Second),
				Labels: []string{"project:gardenio"}},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	newPos := strings.Index(body, "gd-new")
	oldPos := strings.Index(body, "gd-old")
	if newPos < 0 || oldPos < 0 {
		t.Fatalf("missing bead ids: new=%d old=%d", newPos, oldPos)
	}
	if !(newPos < oldPos) {
		t.Errorf("expected recency to lead over priority: gd-new=%d gd-old=%d", newPos, oldPos)
	}
}

// TestHandleActiveSummaryCounts verifies the 4-count summary bar:
// Agents, Active, Decisions, Queued. Active counts non-agent in_progress;
// Agents counts type=agent in_progress; Queued counts non-agent open.
// (Plan §3.1.)
func TestHandleActiveSummaryCounts(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-t1", Title: "Task one", Type: "task", Status: "in_progress",
				Labels: []string{"project:gardenio"}},
			{ID: "gd-t2", Title: "Task two", Type: "task", Status: "in_progress",
				Labels: []string{"project:gardenio"}},
			{ID: "gd-ag", Title: "Agent one", Type: "agent", Status: "in_progress",
				Labels: []string{"project:gardenio"}},
		},
		Total: 3,
	}
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-d1", Type: "decision", Status: "open"},
		},
		Total: 1,
	}
	f.queuedResp = &beadsapi.ListBeadsResult{
		// Server-side ExcludeTypes drops agents before they reach the
		// client, so the response carries only non-agent rows. The handler
		// reads Total instead of post-filtering, eliminating the
		// pull-500-rows-then-client-filter pattern that dominated /v1/beads
		// load on gardenio-e2e.
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-q1", Type: "task", Status: "open"},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Each summary cell must show the expected number followed by its label.
	for _, want := range []string{
		`<span class="stats-banner-num">1</span>
    <span class="stats-banner-label">Agents</span>`,
		`<span class="stats-banner-num">2</span>
    <span class="stats-banner-label">Active</span>`,
		`<span class="stats-banner-num">1</span>
    <span class="stats-banner-label">Decisions</span>`,
		`<span class="stats-banner-num">2</span>
    <span class="stats-banner-label">Queued</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("summary missing %q\nbody=%s", want, body)
		}
	}

	// Verify the third call (queued) uses the cheap shape: Statuses=[open],
	// ExcludeTypes=[agent] for server-side filtering, CountOnly=true so the
	// daemon skips the COUNT(*) OVER() + ORDER BY pattern entirely, and
	// ExcludeScheduled=true by default.
	if len(f.listAllQ) < 3 {
		t.Fatalf("expected at least 3 ListBeadsFiltered calls, got %d", len(f.listAllQ))
	}
	queuedQ := f.listAllQ[2]
	if got := queuedQ.Statuses; len(got) != 1 || got[0] != "open" {
		t.Errorf("queued query statuses = %v, want [open]", got)
	}
	if got := queuedQ.ExcludeTypes; len(got) != 1 || got[0] != "agent" {
		t.Errorf("queued query ExcludeTypes = %v, want [agent]", got)
	}
	if !queuedQ.CountOnly {
		t.Errorf("queued query should set CountOnly=true")
	}
	if !queuedQ.ExcludeScheduled {
		t.Errorf("queued query should set ExcludeScheduled=true by default")
	}
}

// TestHandleActiveDescPreviewTruncated verifies the per-card description
// preview is truncated to ~140 runes (with ellipsis) and that empty
// descriptions render no preview div. (Plan §3.1.)
func TestHandleActiveDescPreviewTruncated(t *testing.T) {
	longDesc := strings.Repeat("a", 200)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-long", Title: "Long desc", Type: "task", Status: "in_progress",
				Description: longDesc, Labels: []string{"project:gardenio"}},
			{ID: "gd-empty", Title: "Empty desc", Type: "task", Status: "in_progress",
				Description: "", Labels: []string{"project:gardenio"}},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Long description renders within a bead-row-desc div, ends with "…",
	// and contains fewer than the original 200 a's (i.e. it was truncated).
	if !strings.Contains(body, `<div class="bead-row-desc">`) {
		t.Errorf("expected at least one bead-row-desc div, body=%s", body)
	}
	// The full 200-char run must not appear verbatim — truncation kicked in.
	if strings.Contains(body, longDesc) {
		t.Errorf("long description was not truncated")
	}
	// The truncated preview must end in an ellipsis.
	if !strings.Contains(body, "a…") {
		t.Errorf("expected ellipsis after truncation, body=%s", body)
	}

	// Empty-description bead must NOT emit a desc div for itself. We can't
	// easily slice by bead, so assert there is exactly one bead-row-desc
	// occurrence in the page (the long bead's).
	if got := strings.Count(body, `<div class="bead-row-desc">`); got != 1 {
		t.Errorf("expected exactly 1 bead-row-desc (only the long bead), got %d", got)
	}
}

func TestHandleActive_DaemonError_RendersBanner(t *testing.T) {
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load active beads") {
		t.Errorf("expected error banner, body=%s", w.Body.String())
	}
}

func TestHandleSearch_EmptyQuery_RendersForm(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/search", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Search beads") {
		t.Errorf("expected form heading, body=%q", body)
	}
	if !strings.Contains(body, `name="q"`) {
		t.Errorf("expected query input, body=%q", body)
	}
	// Per info-density audit (gd-4SbTFHFvXN), the results table scaffold is
	// rendered even on empty query so users see the result shape.
	for _, want := range []string{
		"search-results-heading",
		"<th>Bead</th>",
		"<th>Age</th>",
		"Enter a query above to search beads",
		"search-hints",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q on empty-query landing", want)
		}
	}
	// Save-view form is gated on a non-empty query.
	if strings.Contains(body, "Save this view") {
		t.Errorf("did not expect save-view form for empty query")
	}
}

// The save-view UI lives only on /search now (was on /active and /decisions).
func TestHandleSearch_NonEmptyQuery_RendersSaveViewForm(t *testing.T) {
	f := newFake()
	f.semanticResp = &beadsapi.SemanticSearchResponse{}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=auth&status=open&type=task&labels=project:gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Save this view",
		`action="/views"`,
		`name="search" value="auth"`,
		`name="statuses" value="open"`,
		`name="types" value="task"`,
		`name="labels_filter" value="project:gardenio"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody=%s", want, body)
		}
	}
}

func TestHandleActive_DoesNotRenderSaveViewForm(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "Save this view") {
		t.Errorf("save-view form should not appear on /active (moved to /search)")
	}
}

func TestHandleSearch_SemanticSuccess(t *testing.T) {
	f := newFake()
	f.semanticResp = &beadsapi.SemanticSearchResponse{
		Model: "nomic-embed-text",
		Results: []*beadsapi.SemanticSearchResult{
			{
				Bead: &beadsapi.BeadDetail{
					ID:          "gd-hit1",
					Title:       "Auth middleware rewrite",
					Type:        "task",
					Status:      "open",
					Description: "Refactor session handling to use JWT tokens with refresh rotation.",
				},
				Score: 0.912,
			},
			{
				Bead: &beadsapi.BeadDetail{
					ID:          "gd-hit2",
					Title:       "Login flow regression",
					Type:        "bug",
					Status:      "in_progress",
					Description: "A multi-line\ndescription to test snippet rendering.",
				},
				Score: 0.873,
			},
		},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=auth+bug&status=open&type=task", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if f.semanticLastReq == nil {
		t.Fatal("expected SemanticSearch to be called")
	}
	if f.semanticLastReq.Query != "auth bug" {
		t.Errorf("query = %q, want %q", f.semanticLastReq.Query, "auth bug")
	}
	if got := f.semanticLastReq.Status; len(got) != 1 || got[0] != "open" {
		t.Errorf("status filter = %v, want [open]", got)
	}
	if got := f.semanticLastReq.Type; len(got) != 1 || got[0] != "task" {
		t.Errorf("type filter = %v, want [task]", got)
	}

	// Title rendering bolds query-token matches (ka-0000005947): query "auth bug"
	// against "Auth middleware rewrite" wraps "Auth" but leaves the rest of the
	// title as escaped literal text.
	for _, want := range []string{"gd-hit1", "gd-hit2", `<strong class="search-hl">Auth</strong>`, " middleware rewrite", "0.912", "semantic", "nomic-embed-text"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody=%s", want, body)
		}
	}
	// Lexical should not have been called when semantic succeeds.
	if f.listLastQ != nil {
		t.Errorf("ListBeadsFiltered should not be called when semantic succeeds, got %+v", f.listLastQ)
	}
	// Degraded banner should not be present.
	if strings.Contains(body, "Semantic search unavailable") {
		t.Errorf("unexpected degraded banner on success path")
	}
}

func TestHandleSearch_NoopBackend_FallsBackToLexical(t *testing.T) {
	f := newFake()
	f.semanticErr = &beadsapi.APIError{
		StatusCode: http.StatusServiceUnavailable,
		Message:    "semantic search disabled (BEADS_EMBED_BACKEND=noop)",
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:          "gd-lex1",
				Title:       "Lexical match",
				Type:        "task",
				Status:      "open",
				Description: "Contains the literal search term somewhere in the body.",
			},
		},
		Total: 1,
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=lexical", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called after 503")
	}
	if f.listLastQ.Search != "lexical" {
		t.Errorf("lexical search = %q, want %q", f.listLastQ.Search, "lexical")
	}

	// Title is rendered with query-term highlighting (ka-0000005947), so the
	// matching token is wrapped in <strong> and the literal "Lexical match"
	// no longer appears in the body. Assert on the highlighted token + the
	// trailing literal " match" instead.
	for _, want := range []string{"Semantic search unavailable", "gd-lex1", `<strong class="search-hl">Lexical</strong>`, " match", "lexical"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody=%s", want, body)
		}
	}
	// No score column on lexical results.
	if strings.Contains(body, "<th>Score</th>") {
		t.Errorf("lexical results should not have score column")
	}
}

// TestHandleSearch_SemanticError_DegradesToLexical: a 500 from the semantic
// endpoint (embedder configured but its call failed — e.g. the new-prod
// empty-BEADS_EMBED_API_KEY bug, ka-0000000389) now degrades to the healthy
// lexical path rather than hard-erroring. Semantic is a strict enhancement over
// lexical, so ANY semantic failure falls back. (Was: _ShowsError, which pinned
// the old 503-only fallback and is the exact behavior this fix corrects.)
func TestHandleSearch_SemanticError_DegradesToLexical(t *testing.T) {
	f := newFake()
	f.semanticErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-lex1", Title: "Lexical match", Type: "task", Status: "open"},
		},
		Total: 1,
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=anything", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Degraded banner shown, lexical fallback ran, no hard error.
	if !strings.Contains(body, "Semantic search unavailable") {
		t.Errorf("expected degraded banner on 500, body=%q", body)
	}
	if f.listLastQ == nil {
		t.Fatal("expected ListBeadsFiltered to be called after 500")
	}
	if !strings.Contains(body, "gd-lex1") {
		t.Errorf("expected lexical fallback results on 500, body=%q", body)
	}
	if strings.Contains(body, `<div class="banner banner-error">`) {
		t.Errorf("500 must degrade, not hard-error; body=%q", body)
	}
}

// TestHandleSearch_LexicalFallbackAlsoFails_ShowsError: if the semantic call
// fails AND the lexical fallback itself errors, the page surfaces a hard error
// (there is no healthy path left). This is the only remaining hard-error branch.
func TestHandleSearch_LexicalFallbackAlsoFails_ShowsError(t *testing.T) {
	f := newFake()
	f.semanticErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "lexical down too"}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=anything", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Lexical search fallback failed") {
		t.Errorf("expected lexical-fallback error banner, body=%q", body)
	}
}

// gd-4SbTFHFvXN: /search must always render the results-table scaffold (column
// headers + Age column) and the empty-state hint panel so the page carries
// real signal even when zero beads match — not just a sparse form.
func TestHandleSearch_AlwaysRendersScaffoldAndHints(t *testing.T) {
	f := newFake()
	f.semanticResp = &beadsapi.SemanticSearchResponse{} // zero results

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/search?q=screenshot", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"search-results-heading",
		"<th>Bead</th>",
		"<th>Title</th>",
		"<th>Type</th>",
		"<th>Status</th>",
		"<th>Operator</th>",
		"<th>Age</th>",
		"<th>Score</th>",
		"search-empty-row",
		`No results for "screenshot"`,
		"search-hints",
		"Try these",
		"Open bugs",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q (no-results scaffold/hint regression)", want)
		}
	}
}

func TestSnippet(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 10, ""},
		{"short", 10, "short"},
		{"a multi\nline value", 100, "a multi line value"},
		{"this is a long description that should be truncated", 10, "this is a…"},
	}
	for _, c := range cases {
		if got := snippet(c.in, c.n); got != c.want {
			t.Errorf("snippet(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestParseCommaSeparated(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b , c ", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}},
	}
	for _, c := range cases {
		got := parseCommaSeparated(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseCommaSeparated(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i, v := range got {
			if v != c.want[i] {
				t.Errorf("parseCommaSeparated(%q)[%d] = %q, want %q", c.in, i, v, c.want[i])
			}
		}
	}
}

func TestParseConfig_RequiresOIDC(t *testing.T) {
	t.Setenv("OIDC_ISSUER_URL", "")
	t.Setenv("OIDC_CLIENT_ID", "")
	if _, err := parseConfig(); err == nil {
		t.Fatal("expected error when OIDC env missing, got nil")
	}

	t.Setenv("OIDC_ISSUER_URL", "https://keycloak.example/realms/test")
	t.Setenv("OIDC_CLIENT_ID", "")
	if _, err := parseConfig(); err == nil {
		t.Fatal("expected error when OIDC_CLIENT_ID missing, got nil")
	}

	t.Setenv("OIDC_ISSUER_URL", "https://keycloak.example/realms/test")
	t.Setenv("OIDC_CLIENT_ID", "beads-viewer")
	cfg, err := parseConfig()
	if err != nil {
		t.Fatalf("expected success with OIDC set, got %v", err)
	}
	if cfg.listenAddr != ":8095" {
		t.Errorf("expected default listen :8095, got %q", cfg.listenAddr)
	}
}
