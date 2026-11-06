package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestSquawkPreview is a table test for the squawk-preview truncation helper:
// whitespace is collapsed, the head is clipped at squawkPreviewMaxLen on a word
// boundary with an ellipsis, and multi-byte runes are never split. Mirrors the
// table style of agents_uptime_test.go (ka-0000003997).
func TestSquawkPreview(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \t\n ", ""},
		{"short passthrough", "merged MR !42", "merged MR !42"},
		{
			"collapses internal whitespace",
			"merged   MR\n!42   on  auth",
			"merged MR !42 on auth",
		},
		{
			// 90 chars → clipped at the last space at or before 80, ellipsis appended.
			"truncates long text at word boundary",
			"claimed ka-0000003997 and started building slice three of the home live agents rail now",
			"claimed ka-0000003997 and started building slice three of the home live agents…",
		},
		{
			// A 90-char run with no spaces in the head clips hard at the rune cap.
			"hard clip when no word boundary",
			strings.Repeat("x", 90),
			strings.Repeat("x", squawkPreviewMaxLen) + "…",
		},
		{
			// Multi-byte em-dash near the cap must not be split mid-rune; result
			// stays valid UTF-8 and ends in the ellipsis.
			"rune-safe truncation",
			strings.Repeat("é", 90),
			strings.Repeat("é", squawkPreviewMaxLen) + "…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := squawkPreview(tc.in)
			if got != tc.want {
				t.Errorf("squawkPreview(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8ValidString(got) {
				t.Errorf("squawkPreview(%q) returned invalid UTF-8: %q", tc.in, got)
			}
		})
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// TestRecentSquawksByAgent verifies the batched squawk join keeps the newest
// few squawks per source_agent (newest-first), falls back to the title when
// fields.text is empty, and skips beads with no source_agent. The fake routes
// the Types=[message]/Labels=[squawk] query to squawkResp, asserting the join
// uses a single batched call (ka-0000003997, ka-0000003393).
func TestRecentSquawksByAgent(t *testing.T) {
	f := newFake()
	now := time.Now()
	// Beads arrive newest-first (Sort=-created_at). agent-a has two squawks;
	// both are kept in newest-first order (cap is eventStreamRowsPerAgent).
	f.squawkResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-sq-1", Type: "message", Fields: map[string]string{"source_agent": "agent-a", "text": "newest from a"}, CreatedAt: now},
			{ID: "gd-sq-2", Type: "message", Fields: map[string]string{"source_agent": "agent-a", "text": "older from a"}, CreatedAt: now.Add(-time.Minute)},
			{ID: "gd-sq-3", Type: "message", Title: "title fallback", Fields: map[string]string{"source_agent": "agent-b"}, CreatedAt: now.Add(-2 * time.Minute)},
			{ID: "gd-sq-4", Type: "message", Fields: map[string]string{"text": "orphan no agent"}, CreatedAt: now.Add(-3 * time.Minute)},
		},
		Total: 4,
	}
	srv := NewServer(f, testLogger(), "", "", "", nil)

	got := srv.recentSquawksByAgent(t.Context())
	if len(got["agent-a"]) != 2 {
		t.Fatalf("agent-a = %d squawks, want 2: %+v", len(got["agent-a"]), got["agent-a"])
	}
	if got["agent-a"][0].Text != "newest from a" {
		t.Errorf("agent-a[0] = %q, want newest squawk first", got["agent-a"][0].Text)
	}
	if got["agent-a"][1].Text != "older from a" {
		t.Errorf("agent-a[1] = %q, want older squawk second", got["agent-a"][1].Text)
	}
	if len(got["agent-b"]) != 1 || got["agent-b"][0].Text != "title fallback" {
		t.Errorf("agent-b = %+v, want single title fallback when text empty", got["agent-b"])
	}
	if _, ok := got[""]; ok {
		t.Errorf("orphan squawk with no source_agent should be skipped, got %+v", got)
	}
	if len(got) != 2 {
		t.Errorf("expected exactly 2 agents in join, got %d: %+v", len(got), got)
	}
}

// TestHandleIndex_LiveAgents_SquawkAndOperatorChip verifies the third
// ka-0000007459 slice (ka-0000003997): each Live agents row renders the agent's
// latest squawk preview (truncated) joined from the single batched squawk
// fetch, and renders OperatedBy as a compact initials chip instead of the raw
// "slack:U…" handle — all with no per-agent N+1.
func TestHandleIndex_LiveAgents_SquawkAndOperatorChip(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	now := time.Now()

	// A crew agent with a resolved human operator (display "Matt Baker") whose
	// fields.agent names the squawk source.
	crew := &beadsapi.BeadDetail{
		ID:                "gd-ag-sq",
		Title:             "crew-builder",
		Type:              "agent",
		Status:            "in_progress",
		Labels:            []string{"project:gardenio"},
		OperatedBy:        "slack:U0OPERATOR1",
		OperatedByDisplay: "Matt Baker",
		Fields: map[string]string{
			"agent":       "crew-builder",
			"agent_state": "working",
			"role":        "crew",
			"task_id":     "gd-task-sq",
		},
		CreatedAt: now.Add(-30 * time.Minute),
		UpdatedAt: now.Add(-1 * time.Minute),
	}
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{crew}, Total: 1}
	f.squawkResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-sq-live",
				Type:      "message",
				Labels:    []string{"squawk", "from:crew-builder"},
				Fields:    map[string]string{"source_agent": "crew-builder", "text": "pushed MR and kicked off CI"},
				CreatedAt: now.Add(-90 * time.Second),
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

	// The squawk renders as a row in the recent-event stream (ka-0000003393);
	// the legacy single-line index-rail-agent-squawk fallback was removed in
	// ka-0000000215, so assert on the event-stream markup the row actually emits.
	if !strings.Contains(body, `index-active-card-event-squawk`) {
		t.Errorf("missing squawk event row\nbody=%s", snippetN(body, 1500))
	}
	if !strings.Contains(body, "pushed MR and kicked off CI") {
		t.Errorf("missing squawk text\nbody=%s", snippetN(body, 1500))
	}
	// The dead single-line fallback markup must never render.
	if strings.Contains(body, `index-rail-agent-squawk`) {
		t.Errorf("dead index-rail-agent-squawk fallback should not render\nbody=%s", snippetN(body, 1500))
	}

	// Operated-by renders as a compact initials chip ("MB"), not the raw
	// slack handle.
	if !strings.Contains(body, `operator-chip-compact`) {
		t.Errorf("missing compact operator chip\nbody=%s", snippetN(body, 1500))
	}
	if !strings.Contains(body, `>MB<`) {
		t.Errorf("operator chip should render initials MB\nbody=%s", snippetN(body, 1500))
	}
	if strings.Contains(body, `@slack:U0OPERATOR1`) {
		t.Errorf("row leaked raw slack: handle instead of chip\nbody=%s", snippetN(body, 1500))
	}
}
