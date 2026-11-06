package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func tlTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestIsJiraMirrorBead: only a bead carrying the source:jira label is a mirror.
func TestIsJiraMirrorBead(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"nil", nil, false},
		{"mirror", &beadsapi.BeadDetail{ID: "gd-m", Labels: []string{"project:gardenio", "source:jira", "jira:PE-1"}}, true},
		{"plain task", &beadsapi.BeadDetail{ID: "gd-t", Labels: []string{"project:gardenio"}}, false},
		{"no labels", &beadsapi.BeadDetail{ID: "gd-n"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isJiraMirrorBead(tc.bead); got != tc.want {
				t.Errorf("isJiraMirrorBead(%s)=%v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestCollectJiraTimelineIDs: only triggered-by reverse edges are collected;
// self and non-triggered-by edges are excluded, and duplicates de-duplicated.
func TestCollectJiraTimelineIDs(t *testing.T) {
	mirror := "gd-mirror"
	revDeps := []beadsapi.Dependency{
		{BeadID: "gd-mention", DependsOnID: mirror, Type: "triggered-by"},
		{BeadID: "gd-reply", DependsOnID: mirror, Type: "triggered-by"},
		{BeadID: "gd-reply", DependsOnID: mirror, Type: "triggered-by"}, // dup
		{BeadID: "gd-child", DependsOnID: mirror, Type: "parent-child"}, // not triggered-by
		{BeadID: mirror, DependsOnID: mirror, Type: "triggered-by"},     // self
		{BeadID: "", DependsOnID: mirror, Type: "triggered-by"},         // empty
	}
	got := collectJiraTimelineIDs(mirror, revDeps)
	want := []string{"gd-mention", "gd-reply"}
	if len(got) != len(want) {
		t.Fatalf("collectJiraTimelineIDs got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("collectJiraTimelineIDs[%d]=%q, want %q (order preserved)", i, got[i], want[i])
		}
	}
}

// TestClassifyJiraTimelineBead: reply label > task type > generic.
func TestClassifyJiraTimelineBead(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want jiraTimelineKind
	}{
		{"nil", nil, jiraKindLinked},
		{"reply message", &beadsapi.BeadDetail{Type: "message", Labels: []string{"jira-reply", "from:agent-x"}}, jiraKindReply},
		{"mention task", &beadsapi.BeadDetail{Type: "task", Labels: []string{"source:jira"}}, jiraKindMention},
		{"other", &beadsapi.BeadDetail{Type: "agent"}, jiraKindLinked},
		// A reply that is (unexpectedly) a task type still classifies as reply —
		// the label is authoritative.
		{"reply wins over task type", &beadsapi.BeadDetail{Type: "task", Labels: []string{"jira-reply"}}, jiraKindReply},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyJiraTimelineBead(tc.bead); got != tc.want {
				t.Errorf("classifyJiraTimelineBead(%s)=%q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestBuildJiraTimeline_OrdersAndProjects: a mention + a reply project onto
// ordered rows (oldest first), carry their kind/badge/visibility, and summary.
func TestBuildJiraTimeline_OrdersAndProjects(t *testing.T) {
	now := tlTime("2026-08-11T15:00:00Z")
	ids := []string{"gd-reply", "gd-mention"} // deliberately reversed input order
	beads := map[string]*beadsapi.BeadDetail{
		"gd-mention": {
			ID:        "gd-mention",
			Type:      "task",
			Status:    "in_progress",
			Title:     "[PE-42] @gardenio please look",
			Labels:    []string{"source:jira", "jira:PE-42"},
			CreatedBy: "jira-bridge",
			CreatedAt: tlTime("2026-08-11T14:00:00Z"),
		},
		"gd-reply": {
			ID:     "gd-reply",
			Type:   "message",
			Status: "closed",
			Title:  "On it — here is the fix",
			Labels: []string{"jira-reply", "from:agent-x"},
			Fields: map[string]string{
				"source_agent": "agent-x",
				"text":         "On it — here is the fix",
				"visibility":   "internal",
			},
			CreatedAt: tlTime("2026-08-11T14:30:00Z"),
		},
	}
	v := buildJiraTimeline(ids, beads, "", now)
	if !v.HasEntries {
		t.Fatal("expected HasEntries=true")
	}
	if v.Total != 2 || v.Shown != 2 || v.OlderHidden {
		t.Fatalf("counts wrong: total=%d shown=%d olderHidden=%v", v.Total, v.Shown, v.OlderHidden)
	}
	// Oldest first: mention (14:00) then reply (14:30).
	if v.Entries[0].ID != "gd-mention" || v.Entries[1].ID != "gd-reply" {
		t.Fatalf("order wrong: %q, %q", v.Entries[0].ID, v.Entries[1].ID)
	}
	m := v.Entries[0]
	if m.Kind != jiraKindMention || m.Badge != "mention" {
		t.Errorf("mention kind/badge wrong: %q / %q", m.Kind, m.Badge)
	}
	if m.Status != "in_progress" {
		t.Errorf("mention status=%q, want in_progress", m.Status)
	}
	if m.Author != "jira-bridge" {
		t.Errorf("mention author=%q, want jira-bridge", m.Author)
	}
	r := v.Entries[1]
	if r.Kind != jiraKindReply || r.Badge != "reply" {
		t.Errorf("reply kind/badge wrong: %q / %q", r.Kind, r.Badge)
	}
	if r.Visibility != "internal" {
		t.Errorf("reply visibility=%q, want internal", r.Visibility)
	}
	if r.Author != "agent-x" {
		t.Errorf("reply author=%q, want agent-x", r.Author)
	}
	if !strings.Contains(r.Summary, "On it") {
		t.Errorf("reply summary missing body: %q", r.Summary)
	}
}

// TestBuildJiraTimeline_Empty: no ids, all-unhydrated, and nil map degrade to
// an empty (no-panel) view rather than panicking.
func TestBuildJiraTimeline_Empty(t *testing.T) {
	now := tlTime("2026-08-11T15:00:00Z")
	if v := buildJiraTimeline(nil, nil, "", now); v.HasEntries {
		t.Error("nil ids should give empty view")
	}
	if v := buildJiraTimeline([]string{"gd-x"}, map[string]*beadsapi.BeadDetail{}, "", now); v.HasEntries {
		t.Error("unhydrated ids should give empty view")
	}
}

// TestBuildJiraTimeline_CapKeepsNewest: past the cap, the newest window is kept
// and the dropped older count is reported.
func TestBuildJiraTimeline_CapKeepsNewest(t *testing.T) {
	now := tlTime("2026-08-11T15:00:00Z")
	total := jiraTimelineMaxEntries + 5
	ids := make([]string, 0, total)
	beads := make(map[string]*beadsapi.BeadDetail, total)
	base := tlTime("2026-08-01T00:00:00Z")
	for i := 0; i < total; i++ {
		id := "gd-" + string(rune('a'+i%26)) + time.Duration(i).String()
		ids = append(ids, id)
		beads[id] = &beadsapi.BeadDetail{
			ID:        id,
			Type:      "message",
			Labels:    []string{"jira-reply"},
			Title:     "msg",
			CreatedAt: base.Add(time.Duration(i) * time.Hour),
		}
	}
	v := buildJiraTimeline(ids, beads, "", now)
	if v.Total != total {
		t.Errorf("Total=%d, want %d", v.Total, total)
	}
	if v.Shown != jiraTimelineMaxEntries {
		t.Errorf("Shown=%d, want %d", v.Shown, jiraTimelineMaxEntries)
	}
	if !v.OlderHidden || v.HiddenCount != 5 {
		t.Errorf("OlderHidden=%v HiddenCount=%d, want true/5", v.OlderHidden, v.HiddenCount)
	}
	// The kept window must be the NEWEST — the last-created bead survives.
	lastID := ids[total-1]
	found := false
	for _, e := range v.Entries {
		if e.ID == lastID {
			found = true
		}
	}
	if !found {
		t.Errorf("newest entry %q dropped by cap", lastID)
	}
}

// TestBeadShow_JiraTimeline_Renders drives the full bead-show handler for a
// source:jira mirror bead whose reverse triggered-by edges point at an inbound
// mention task and an outbound reply message, and asserts the conversation
// panel renders both, ordered, with kind badges — end-to-end through the real
// template set (also proving jira_timeline.html parses into bead_show.html).
func TestBeadShow_JiraTimeline_Renders(t *testing.T) {
	f := newFake()
	f.beads["gd-mirror"] = &beadsapi.BeadDetail{
		ID:        "gd-mirror",
		Type:      "task",
		Status:    "open",
		Title:     "[PE-77] Investigate flaky deploy",
		Labels:    []string{"project:gardenio", "source:jira", "jira:PE-77"},
		Fields:    map[string]string{"jira_key": "PE-77"},
		CreatedBy: "jira-bridge",
		CreatedAt: time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 11, 14, 40, 0, 0, time.UTC),
	}
	f.beads["gd-mention77"] = &beadsapi.BeadDetail{
		ID:        "gd-mention77",
		Type:      "task",
		Status:    "in_progress",
		Title:     "[PE-77] @gardenio what is going on",
		Labels:    []string{"source:jira", "jira:PE-77"},
		CreatedBy: "jira-bridge",
		CreatedAt: time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC),
	}
	f.beads["gd-reply77"] = &beadsapi.BeadDetail{
		ID:     "gd-reply77",
		Type:   "message",
		Status: "closed",
		Title:  "Root cause found: stale cache",
		Labels: []string{"jira-reply", "from:agent-z"},
		Fields: map[string]string{
			"source_agent": "agent-z",
			"text":         "Root cause found: stale cache",
			"visibility":   "internal",
		},
		CreatedAt: time.Date(2026, 8, 11, 14, 30, 0, 0, time.UTC),
	}
	// The reverse triggered-by edges the bridge / gb jira reply stamp back to
	// the mirror (BeadID depends on the mirror).
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-mirror": {
			{BeadID: "gd-mention77", DependsOnID: "gd-mirror", Type: "triggered-by"},
			{BeadID: "gd-reply77", DependsOnID: "gd-mirror", Type: "triggered-by"},
		},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-mirror", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Jira conversation",
		"gd-mention77",
		"gd-reply77",
		"Root cause found: stale cache",
		`class="jira-badge badge-mention"`,
		`class="jira-badge badge-reply"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("jira timeline missing %q", want)
		}
	}
	// Ordered oldest→newest: the mention row must precede the reply row.
	if mi, ri := strings.Index(body, "gd-mention77"), strings.Index(body, "gd-reply77"); mi < 0 || ri < 0 || mi > ri {
		t.Errorf("timeline order wrong: mention idx=%d reply idx=%d", mi, ri)
	}
}

// TestBeadShow_JiraTimeline_AbsentForPlainBead: a non-mirror bead never renders
// the Jira conversation panel, even if it happens to have triggered-by edges.
func TestBeadShow_JiraTimeline_AbsentForPlainBead(t *testing.T) {
	f := newFake()
	// gd-aaa is a plain project:gardenio task (no source:jira).
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-aaa": {{BeadID: "gd-child", DependsOnID: "gd-aaa", Type: "triggered-by"}},
	}
	f.beads["gd-child"] = &beadsapi.BeadDetail{ID: "gd-child", Type: "message", Labels: []string{"jira-reply"}, Title: "x"}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "Jira conversation") {
		t.Error("plain bead must not render the Jira conversation panel")
	}
}

// TestJiraTimelineSummary_CollapsesAndBounds: whitespace collapsed, length
// bounded.
func TestJiraTimelineSummary_CollapsesAndBounds(t *testing.T) {
	b := &beadsapi.BeadDetail{Fields: map[string]string{"text": "line one\n\n   line   two\t\tthree"}}
	got := jiraTimelineSummary(b)
	if strings.ContainsAny(got, "\n\t") || strings.Contains(got, "  ") {
		t.Errorf("summary not collapsed: %q", got)
	}
	long := strings.Repeat("x", 500)
	b2 := &beadsapi.BeadDetail{Title: long}
	got2 := jiraTimelineSummary(b2)
	if len([]rune(got2)) > 241 { // 240 cap + ellipsis rune slack
		t.Errorf("summary not bounded: len=%d", len([]rune(got2)))
	}
	if !strings.HasSuffix(got2, "…") {
		t.Errorf("bounded summary should end with ellipsis: %q", got2[len(got2)-5:])
	}
}
