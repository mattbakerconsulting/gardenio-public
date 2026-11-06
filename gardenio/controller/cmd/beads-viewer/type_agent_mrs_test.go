package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestBeadShow_AgentType_ProducedMRs_None asserts that the Produced MRs
// section is omitted entirely when the agent has no produced MR beads.
func TestBeadShow_AgentType_ProducedMRs_None(t *testing.T) {
	f := newFake()
	f.beads["gd-agt-none"] = &beadsapi.BeadDetail{
		ID:     "gd-agt-none",
		Title:  "crew-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent": "crew-1",
			"role":  "crew",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-agt-none", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, banned := range []string{
		`class="agent-mrs"`,
		"Produced MRs",
		"mr-pill",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("empty produced-MRs path unexpectedly rendered %q\nbody: %s", banned, body)
		}
	}
}

// TestBeadShow_AgentType_ProducedMRs_One asserts a single produced MR
// renders one pill inside the agent-mrs section with the full pill chrome.
func TestBeadShow_AgentType_ProducedMRs_One(t *testing.T) {
	f := newFake()
	f.beads["gd-agt-one"] = &beadsapi.BeadDetail{
		ID:     "gd-agt-one",
		Title:  "crew-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent": "crew-1",
			"role":  "crew",
		},
	}
	f.beads["gd-task-1"] = &beadsapi.BeadDetail{
		ID:     "gd-task-1",
		Title:  "feature task",
		Type:   "task",
		Status: "in_progress",
	}
	f.beads["gd-mr-1"] = &beadsapi.BeadDetail{
		ID:     "gd-mr-1",
		Title:  "feature MR",
		Type:   "merge-request",
		Status: "open",
		Fields: map[string]string{
			"mr_url":             "https://gitlab.example/group/project/-/merge_requests/42",
			"mr_iid":             "42",
			"mr_title":           "feat: ship the thing",
			"mr_state":           "opened",
			"mr_pipeline_status": "success",
		},
	}
	f.deps["gd-agt-one"] = []beadsapi.Dependency{
		{BeadID: "gd-agt-one", DependsOnID: "gd-task-1", Type: "produces"},
	}
	f.deps["gd-task-1"] = []beadsapi.Dependency{
		{BeadID: "gd-task-1", DependsOnID: "gd-mr-1", Type: "produces"},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-agt-one", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="agent-mrs"`,
		"Produced MRs",
		`class="mr-pill"`,
		"https://gitlab.example/group/project/-/merge_requests/42",
		"!42",
		"feat: ship the thing",
		"mr-state-opened",
		"pipe-success",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("one-MR render missing %q\nbody: %s", want, body)
		}
	}
	if got := strings.Count(body, `class="mr-pill"`); got != 1 {
		t.Errorf("expected exactly 1 mr-pill, got %d\nbody: %s", got, body)
	}
}

// TestBeadShow_AgentType_ProducedMRs_Many asserts N>1 produced MRs across
// multiple producing tasks all render in the section, in traversal order,
// and that the same MR reached via two tasks is rendered once (dedup).
func TestBeadShow_AgentType_ProducedMRs_Many(t *testing.T) {
	f := newFake()
	f.beads["gd-agt-many"] = &beadsapi.BeadDetail{
		ID:     "gd-agt-many",
		Title:  "crew-multi",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent": "crew-multi",
			"role":  "crew",
		},
	}
	for _, taskID := range []string{"gd-task-a", "gd-task-b", "gd-task-c"} {
		f.beads[taskID] = &beadsapi.BeadDetail{
			ID:     taskID,
			Title:  "task " + taskID,
			Type:   "task",
			Status: "closed",
		}
	}
	mrSpec := []struct {
		id, url, iid, title, state, pipeline string
	}{
		{"gd-mr-a", "https://gl.example/p/-/merge_requests/1", "1", "first MR", "merged", "success"},
		{"gd-mr-b", "https://gl.example/p/-/merge_requests/2", "2", "second MR", "opened", "running"},
		{"gd-mr-c", "https://gl.example/p/-/merge_requests/3", "3", "third MR", "closed", "failed"},
	}
	for _, m := range mrSpec {
		f.beads[m.id] = &beadsapi.BeadDetail{
			ID:     m.id,
			Title:  m.title,
			Type:   "merge-request",
			Status: "open",
			Fields: map[string]string{
				"mr_url":             m.url,
				"mr_iid":             m.iid,
				"mr_title":           m.title,
				"mr_state":           m.state,
				"mr_pipeline_status": m.pipeline,
			},
		}
	}
	f.deps["gd-agt-many"] = []beadsapi.Dependency{
		{BeadID: "gd-agt-many", DependsOnID: "gd-task-a", Type: "produces"},
		{BeadID: "gd-agt-many", DependsOnID: "gd-task-b", Type: "produces"},
		{BeadID: "gd-agt-many", DependsOnID: "gd-task-c", Type: "produces"},
	}
	f.deps["gd-task-a"] = []beadsapi.Dependency{
		{BeadID: "gd-task-a", DependsOnID: "gd-mr-a", Type: "produces"},
	}
	// gd-task-b also references gd-mr-a — dedup should fold the second hit.
	f.deps["gd-task-b"] = []beadsapi.Dependency{
		{BeadID: "gd-task-b", DependsOnID: "gd-mr-a", Type: "produces"},
		{BeadID: "gd-task-b", DependsOnID: "gd-mr-b", Type: "produces"},
	}
	f.deps["gd-task-c"] = []beadsapi.Dependency{
		{BeadID: "gd-task-c", DependsOnID: "gd-mr-c", Type: "produces"},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-agt-many", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if got := strings.Count(body, `class="mr-pill"`); got != 3 {
		t.Errorf("expected exactly 3 mr-pills (dedup), got %d\nbody: %s", got, body)
	}
	for _, want := range []string{
		"https://gl.example/p/-/merge_requests/1",
		"https://gl.example/p/-/merge_requests/2",
		"https://gl.example/p/-/merge_requests/3",
		"first MR",
		"second MR",
		"third MR",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("many-MRs render missing %q\nbody: %s", want, body)
		}
	}
	// Traversal order: agent's produces deps in declared order, then each
	// task's produces deps in declared order, with dedup.
	idxA := strings.Index(body, "first MR")
	idxB := strings.Index(body, "second MR")
	idxC := strings.Index(body, "third MR")
	if !(idxA < idxB && idxB < idxC) {
		t.Errorf("expected MR order first<second<third, got idxA=%d idxB=%d idxC=%d", idxA, idxB, idxC)
	}
}

// TestBeadShow_AgentType_ProducedMRs_DaemonError asserts the section
// degrades to omitted (rather than blanking the whole page) when the
// daemon errors on the produced-MRs walk.
func TestBeadShow_AgentType_ProducedMRs_DaemonError(t *testing.T) {
	f := newFake()
	f.beads["gd-agt-err"] = &beadsapi.BeadDetail{
		ID:     "gd-agt-err",
		Title:  "crew-err",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent": "crew-err",
		},
	}
	f.mrsForAgentErr = errFake("boom")

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-agt-err", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `class="agent-mrs"`) {
		t.Errorf("daemon-error path should omit agent-mrs section\nbody: %s", body)
	}
	// Page chrome still renders.
	if !strings.Contains(body, "<h3>Agent</h3>") {
		t.Errorf("daemon-error path should still render the agent card")
	}
}

// errFake is a one-line error type used to inject hard failures into the
// fake daemon's MR-walk hook without needing a fresh errors-package import.
type errFake string

func (e errFake) Error() string { return string(e) }
