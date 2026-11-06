package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestRecentCommentsByAgent verifies the durable-events comment join keeps the
// newest few comments per author (newest-first), drops squawk-mirror copies (the
// "_squawk gd-… from …_" footer), skips authorless / empty comments, and uses a
// SINGLE ListGlobalEvents call (ka-0000003393).
func TestRecentCommentsByAgent(t *testing.T) {
	f := newFake()
	now := time.Now()
	// Authored oldest-first; the fake reverses for a Desc request so the join
	// sees them newest-first (matching the live daemon).
	f.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			// agent-a: a real comment plus a squawk-mirror that must be dropped.
			{
				ID: 1, Topic: "beads.comment.added", BeadID: "gd-x", CreatedAt: now.Add(-5 * time.Minute),
				Payload: mustJSON(t, map[string]any{"comment": map[string]any{"author": "agent-a", "text": "mirror body\n\n_squawk gd-abc from agent-a_"}}),
			},
			{
				ID: 2, Topic: "beads.comment.added", BeadID: "gd-x", CreatedAt: now.Add(-2 * time.Minute),
				Payload: mustJSON(t, map[string]any{"comment": map[string]any{"author": "agent-a", "text": "a real review comment"}}),
			},
			// A non-comment event on the same page must be ignored.
			{
				ID: 3, Topic: "beads.bead.closed", BeadID: "gd-x", CreatedAt: now.Add(-1 * time.Minute),
				Payload: mustJSON(t, map[string]any{"closed_by": "agent-a"}),
			},
			// authorless comment — skipped.
			{
				ID: 4, Topic: "beads.comment.added", BeadID: "gd-y", CreatedAt: now.Add(-3 * time.Minute),
				Payload: mustJSON(t, map[string]any{"comment": map[string]any{"text": "no author"}}),
			},
			// agent-b: a normal comment.
			{
				ID: 5, Topic: "beads.comment.added", BeadID: "gd-z", CreatedAt: now.Add(-4 * time.Minute),
				Payload: mustJSON(t, map[string]any{"comment": map[string]any{"author": "agent-b", "text": "b commented"}}),
			},
			// agent-a: an HTML-comment-marker bookkeeping sentinel — dropped.
			{
				ID: 6, Topic: "beads.comment.added", BeadID: "gd-x", CreatedAt: now.Add(-30 * time.Second),
				Payload: mustJSON(t, map[string]any{"comment": map[string]any{"author": "agent-a", "text": "<!-- gardener-delivery-cooldown --> dispatch cooldown until 13:00"}}),
			},
		},
	}
	srv := NewServer(f, testLogger(), "", "", "", nil)

	got := srv.recentCommentsByAgent(t.Context())
	if f.listGlobalEventsCalls != 1 {
		t.Errorf("expected exactly 1 ListGlobalEvents call, got %d", f.listGlobalEventsCalls)
	}
	if !f.listGlobalEventsLastReq.Desc {
		t.Errorf("comment join must request newest-first (Desc), got %+v", f.listGlobalEventsLastReq)
	}
	if len(got["agent-a"]) != 1 {
		t.Fatalf("agent-a = %d comments, want 1 (mirror + HTML-marker sentinel dropped): %+v", len(got["agent-a"]), got["agent-a"])
	}
	if got["agent-a"][0].Text != "a real review comment" {
		t.Errorf("agent-a[0] = %q, want the real (non-mirror) comment", got["agent-a"][0].Text)
	}
	if len(got["agent-b"]) != 1 || got["agent-b"][0].Text != "b commented" {
		t.Errorf("agent-b = %+v, want single comment", got["agent-b"])
	}
	if _, ok := got[""]; ok {
		t.Errorf("authorless comment should be skipped, got %+v", got)
	}
}

// TestBuildAgentEventStream verifies squawks + comments merge newest-first, are
// previewed to a single line, and are capped at eventStreamRowsPerAgent
// (ka-0000003393).
func TestBuildAgentEventStream(t *testing.T) {
	now := time.Now()
	squawks := []stampedSquawk{
		{Text: "squawk newest", At: now.Add(-1 * time.Minute)},
		{Text: "squawk oldest", At: now.Add(-9 * time.Minute)},
	}
	comments := []stampedSquawk{
		{Text: "comment middle", At: now.Add(-4 * time.Minute)},
	}
	got := buildAgentEventStream(squawks, comments, now)
	if len(got) != eventStreamRowsPerAgent {
		t.Fatalf("expected %d rows (cap), got %d: %+v", eventStreamRowsPerAgent, len(got), got)
	}
	// Newest-first: squawk newest → comment middle → squawk oldest.
	wantOrder := []struct{ kind, text string }{
		{"squawk", "squawk newest"},
		{"comment", "comment middle"},
		{"squawk", "squawk oldest"},
	}
	for i, w := range wantOrder {
		if got[i].Kind != w.kind || got[i].Text != w.text {
			t.Errorf("row %d = {%s %q}, want {%s %q}", i, got[i].Kind, got[i].Text, w.kind, w.text)
		}
		if got[i].Ago == "" {
			t.Errorf("row %d missing relative time", i)
		}
	}

	// Empty in → nil out.
	if got := buildAgentEventStream(nil, nil, now); got != nil {
		t.Errorf("empty inputs should yield nil, got %+v", got)
	}

	// Duplicate previews collapse to one row (e.g. a bot re-posting the same
	// automated line, or a mirror-comment that slipped the footer filter).
	dupSquawks := []stampedSquawk{{Text: "same line", At: now.Add(-1 * time.Minute)}}
	dupComments := []stampedSquawk{
		{Text: "same line", At: now.Add(-2 * time.Minute)},
		{Text: "same line", At: now.Add(-3 * time.Minute)},
	}
	deduped := buildAgentEventStream(dupSquawks, dupComments, now)
	if len(deduped) != 1 {
		t.Errorf("duplicate previews should collapse to 1 row, got %d: %+v", len(deduped), deduped)
	}
}

// TestHandleIndex_LiveAgents_EventStream is the end-to-end render check: a live
// agent's card renders a stacked event stream interleaving its squawk and its
// (non-mirror) comment, newest-first, with no regression to the existing card
// chrome (task title link, operator chip) (ka-0000003393).
func TestHandleIndex_LiveAgents_EventStream(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	now := time.Now()

	crew := &beadsapi.BeadDetail{
		ID:                "gd-ag-es",
		Title:             "crew-streamer",
		Type:              "agent",
		Status:            "in_progress",
		Labels:            []string{"project:gardenio"},
		OperatedBy:        "slack:U0OPERATOR1",
		OperatedByDisplay: "Matt Baker",
		Fields: map[string]string{
			"agent":       "crew-streamer",
			"agent_state": "working",
			"role":        "crew",
			"task_id":     "gd-task-es",
		},
		CreatedAt: now.Add(-30 * time.Minute),
		UpdatedAt: now.Add(-1 * time.Minute),
	}
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{crew}, Total: 1}
	// One squawk (newest) …
	f.squawkResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-sq-es", Type: "message", Labels: []string{"squawk"}, Fields: map[string]string{"source_agent": "crew-streamer", "text": "pushed MR and kicked off CI"}, CreatedAt: now.Add(-30 * time.Second)},
		},
		Total: 1,
	}
	// … and one older real comment plus a mirror that must NOT render twice.
	f.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			{
				ID: 1, Topic: "beads.comment.added", BeadID: "gd-task-es", CreatedAt: now.Add(-10 * time.Minute),
				Payload: mustJSON(t, map[string]any{"comment": map[string]any{"author": "crew-streamer", "text": "investigated the flaky test root cause"}}),
			},
			{
				ID: 2, Topic: "beads.comment.added", BeadID: "gd-task-es", CreatedAt: now.Add(-31 * time.Second),
				Payload: mustJSON(t, map[string]any{"comment": map[string]any{"author": "crew-streamer", "text": "pushed MR and kicked off CI\n\n_squawk gd-sq-es from crew-streamer_"}}),
			},
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

	// The stacked event list renders.
	if !strings.Contains(body, "index-active-card-events") {
		t.Errorf("missing event-stream list\nbody=%s", snippetN(body, 2000))
	}
	// Both the squawk and the real comment appear …
	if !strings.Contains(body, "pushed MR and kicked off CI") {
		t.Errorf("missing squawk row\nbody=%s", snippetN(body, 2000))
	}
	if !strings.Contains(body, "investigated the flaky test root cause") {
		t.Errorf("missing comment row\nbody=%s", snippetN(body, 2000))
	}
	// … and the squawk text renders exactly once (the mirror comment is deduped).
	if n := strings.Count(body, "pushed MR and kicked off CI"); n != 1 {
		t.Errorf("squawk text rendered %d times, want 1 (mirror comment must be deduped)\nbody=%s", n, snippetN(body, 2000))
	}
	// Newest-first: the squawk (30s) precedes the comment (10m).
	if si, ci := strings.Index(body, "pushed MR and kicked off CI"), strings.Index(body, "investigated the flaky test root cause"); si > ci {
		t.Errorf("events not newest-first: squawk at %d should precede comment at %d", si, ci)
	}
	// No regression: task title link + operator chip still render.
	if !strings.Contains(body, "operator-chip-compact") {
		t.Errorf("regression: missing compact operator chip\nbody=%s", snippetN(body, 2000))
	}
}
