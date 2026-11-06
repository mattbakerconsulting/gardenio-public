package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestComputeIndexLiveAgents_ResolvesTaskTitle verifies the ka-0000004222
// batch title-resolution: a live agent whose fields.task_title is empty gets
// its row.TaskTitle hydrated from the claimed task bead via a SINGLE batched
// BatchGetBeads call (no per-agent N+1), while an unresolved TaskID leaves
// TaskTitle empty so the template falls back to the bare id.
func TestComputeIndexLiveAgents_ResolvesTaskTitle(t *testing.T) {
	f := newFake()
	now := time.Now()

	// Two agents claim the SAME task (dedup path) plus one claiming an
	// unresolvable task. None carry fields.task_title.
	mkAgent := func(id, name, taskID string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:     id,
			Title:  name,
			Type:   "agent",
			Status: "in_progress",
			Labels: []string{"project:gardenio"},
			Fields: map[string]string{
				"agent":       name,
				"agent_state": "working",
				"task_id":     taskID,
			},
			CreatedAt: now.Add(-20 * time.Minute),
			UpdatedAt: now.Add(-1 * time.Minute),
		}
	}
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mkAgent("gd-ag-1", "crew-one", "gd-task-real"),
			mkAgent("gd-ag-2", "crew-two", "gd-task-real"),
			mkAgent("gd-ag-3", "crew-three", "gd-task-missing"),
		},
		Total: 3,
	}
	// The claimed task is resolvable via BatchGetBeads (f.beads).
	f.beads["gd-task-real"] = &beadsapi.BeadDetail{
		ID:    "gd-task-real",
		Title: "Wire up the Home live-agents card",
		Type:  "task",
	}

	srv := NewServer(f, testLogger(), "", "", "", nil)
	f.batchGetBeadsCalls = 0
	rows := srv.computeIndexLiveAgents(t.Context(), 10, "")

	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	byID := map[string]indexLiveAgent{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if got := byID["gd-ag-1"].TaskTitle; got != "Wire up the Home live-agents card" {
		t.Errorf("gd-ag-1 TaskTitle = %q, want resolved title", got)
	}
	if got := byID["gd-ag-2"].TaskTitle; got != "Wire up the Home live-agents card" {
		t.Errorf("gd-ag-2 (same task) TaskTitle = %q, want resolved title", got)
	}
	if got := byID["gd-ag-3"].TaskTitle; got != "" {
		t.Errorf("gd-ag-3 unresolved task should leave TaskTitle empty; got %q", got)
	}
	// One batched fetch covers all distinct ids — never per-agent.
	if f.batchGetBeadsCalls != 1 {
		t.Errorf("BatchGetBeads calls = %d, want exactly 1 (no per-agent N+1)", f.batchGetBeadsCalls)
	}
	// Distinct ids only: {gd-task-real, gd-task-missing} = 2, not 3.
	if f.batchGetBeadsLastN != 2 {
		t.Errorf("BatchGetBeads ids = %d, want 2 distinct task ids", f.batchGetBeadsLastN)
	}
}

// TestComputeIndexLiveAgents_PrefersExistingTaskTitle verifies that an agent
// already carrying fields.task_title is NOT re-fetched — the pre-populated
// title wins and contributes no id to the batch.
func TestComputeIndexLiveAgents_PrefersExistingTaskTitle(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:     "gd-ag-pref",
			Title:  "crew-pref",
			Type:   "agent",
			Status: "in_progress",
			Labels: []string{"project:gardenio"},
			Fields: map[string]string{
				"agent":       "crew-pref",
				"agent_state": "working",
				"task_id":     "gd-task-pref",
				"task_title":  "Pre-set title",
			},
			CreatedAt: now.Add(-10 * time.Minute),
			UpdatedAt: now.Add(-1 * time.Minute),
		}},
		Total: 1,
	}
	srv := NewServer(f, testLogger(), "", "", "", nil)
	f.batchGetBeadsCalls = 0
	rows := srv.computeIndexLiveAgents(t.Context(), 10, "")
	if len(rows) != 1 || rows[0].TaskTitle != "Pre-set title" {
		t.Fatalf("expected pre-set TaskTitle preserved, got %+v", rows)
	}
	if f.batchGetBeadsCalls != 0 {
		t.Errorf("BatchGetBeads should not be called when all titles present; calls=%d", f.batchGetBeadsCalls)
	}
}

// TestHandleIndex_LiveAgentsCard_TitleHeadlineAgentFooter verifies the
// ka-0000004222 template restructure end-to-end: the TASK title is rendered as
// the card headline link (to the task bead, showing the resolved title not the
// gd-id), and the agent-name link is rendered small in a de-emphasized footer
// line at the bottom of the card.
func TestHandleIndex_LiveAgentsCard_TitleHeadlineAgentFooter(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{}
	now := time.Now()

	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:     "gd-ag-card",
			Title:  "gardenio-home-liveagents-card-l44",
			Type:   "agent",
			Status: "in_progress",
			Labels: []string{"project:gardenio"},
			Fields: map[string]string{
				"agent":       "gardenio-home-liveagents-card-l44",
				"agent_state": "working",
				"role":        "crew",
				"task_id":     "gd-task-card",
			},
			CreatedAt: now.Add(-15 * time.Minute),
			UpdatedAt: now.Add(-1 * time.Minute),
		}},
		Total: 1,
	}
	f.beads["gd-task-card"] = &beadsapi.BeadDetail{
		ID:    "gd-task-card",
		Title: "Home Live-agents card: link task by title",
		Type:  "task",
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/index", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Headline link is the task: shows the resolved TITLE, links to the task bead.
	if !strings.Contains(body, "Home Live-agents card: link task by title") {
		t.Errorf("card headline should show resolved task title\nbody=%s", snippetN(body, 2000))
	}
	if !strings.Contains(body, `class="index-active-card-name index-active-card-title"`) {
		t.Errorf("task title should use the headline class\nbody=%s", snippetN(body, 2000))
	}
	if !strings.Contains(body, `href="/gd-task-card"`) {
		t.Errorf("headline should link to the task bead\nbody=%s", snippetN(body, 2000))
	}

	// Agent-name link is demoted to the small footer line, linking to the agent bead.
	footIdx := strings.Index(body, "index-active-card-foot")
	if footIdx < 0 {
		t.Fatalf("missing agent footer line\nbody=%s", snippetN(body, 2000))
	}
	if !strings.Contains(body, `class="index-active-card-agentlink"`) {
		t.Errorf("agent link should use the de-emphasized footer class\nbody=%s", snippetN(body, 2000))
	}
	// The agent footer must come AFTER the task headline in the document (i.e.
	// not at the top of the card).
	headIdx := strings.Index(body, "index-active-card-title")
	if headIdx < 0 || footIdx < headIdx {
		t.Errorf("agent footer (%d) should render below the task headline (%d)", footIdx, headIdx)
	}
}
