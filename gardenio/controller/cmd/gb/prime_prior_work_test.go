package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// setupPriorWorkTestDaemon installs a test daemon that serves GetBead for the
// supplied beads (keyed by ID) and answers a filtered list query on task_id by
// returning listResult (newest-first, as the daemon would). Any other request
// 400s so the test fails loud on an unexpected call.
func setupPriorWorkTestDaemon(t *testing.T, beads map[string]*mockBead, listResult []*mockBead) func() {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// GetBead: /v1/beads/<id> with no query string.
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.URL.RawQuery == "" {
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			if b, ok := beads[id]; ok {
				_ = json.NewEncoder(w).Encode(b)
				return
			}
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		// ListBeadsFiltered: /v1/beads?...
		if r.Method == "GET" && r.URL.Path == "/v1/beads" {
			items := make([]any, 0, len(listResult))
			for _, b := range listResult {
				items = append(items, b)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": items,
				"total": len(items),
			})
			return
		}

		http.Error(w, "unexpected request: "+r.Method+" "+r.URL.String(), http.StatusBadRequest)
	}))

	oldDaemon := daemon
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}
	daemon = c

	return func() {
		daemon = oldDaemon
		srv.Close()
	}
}

func wrapJSON(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal wrapup: %v", err)
	}
	return string(b)
}

func TestOutputPriorWorkOnBead_EmptyAgentID_Empty(t *testing.T) {
	var buf bytes.Buffer
	outputPriorWorkOnBead(&buf, "")
	if buf.Len() != 0 {
		t.Errorf("expected empty output for empty agentID, got: %q", buf.String())
	}
}

func TestOutputPriorWorkOnBead_NoTaskID_Empty(t *testing.T) {
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{}}
	cleanup := setupPriorWorkTestDaemon(t, map[string]*mockBead{"gd-agent": agent}, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWorkOnBead(&buf, "gd-agent")
	if buf.Len() != 0 {
		t.Errorf("expected empty output when agent carries no task_id, got: %q", buf.String())
	}
}

func TestOutputPriorWorkOnBead_NoPriorOccupant_Empty(t *testing.T) {
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{"task_id": "gd-task"}}
	cleanup := setupPriorWorkTestDaemon(t, map[string]*mockBead{"gd-agent": agent}, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWorkOnBead(&buf, "gd-agent")
	if buf.Len() != 0 {
		t.Errorf("expected empty output when no prior occupant on the task, got: %q", buf.String())
	}
}

func TestOutputPriorWorkOnBead_ViaTaskIDDiscovery(t *testing.T) {
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{"task_id": "gd-task"}}
	prior := &mockBead{
		ID:     "gd-prior",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"task_id": "gd-task",
			"wrapup": wrapJSON(t, map[string]any{
				"tldr":          "Fixed the panel query; one dashboard still red.",
				"handoff_notes": "The remaining failure is a bad time-range var.\nStart at panel 4.",
			}),
		},
	}
	cleanup := setupPriorWorkTestDaemon(t,
		map[string]*mockBead{"gd-agent": agent},
		[]*mockBead{prior},
	)
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWorkOnBead(&buf, "gd-agent")
	out := buf.String()

	checks := []string{
		"## Prior work on this bead",
		"`gd-prior`",
		"**Predecessor tl;dr:**",
		"> Fixed the panel query; one dashboard still red.",
		"**Handoff notes from your predecessor:**",
		"> The remaining failure is a bad time-range var.",
		"> Start at panel 4.",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestOutputPriorWorkOnBead_ViaPriorAgentIDPointer(t *testing.T) {
	// A1 stamped an explicit prior_agent_id — that pointer wins, no list query
	// is needed (list returns empty here to prove the pointer path is used).
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{
		"task_id":        "gd-task",
		"prior_agent_id": "gd-prior",
	}}
	prior := &mockBead{
		ID:     "gd-prior",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"wrapup": wrapJSON(t, map[string]any{"handoff_notes": "Pointer-delivered handoff."}),
		},
	}
	cleanup := setupPriorWorkTestDaemon(t,
		map[string]*mockBead{"gd-agent": agent, "gd-prior": prior},
		nil,
	)
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWorkOnBead(&buf, "gd-agent")
	out := buf.String()
	if !strings.Contains(out, "Pointer-delivered handoff.") {
		t.Errorf("expected pointer-delivered handoff, got: %q", out)
	}
}

func TestOutputPriorWorkOnBead_TLDROnly(t *testing.T) {
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{"task_id": "gd-task"}}
	prior := &mockBead{
		ID: "gd-prior", Type: "agent", Status: "closed",
		Fields: map[string]string{
			"task_id": "gd-task",
			"wrapup":  wrapJSON(t, map[string]any{"tldr": "Shipped it."}),
		},
	}
	cleanup := setupPriorWorkTestDaemon(t, map[string]*mockBead{"gd-agent": agent}, []*mockBead{prior})
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWorkOnBead(&buf, "gd-agent")
	out := buf.String()
	if !strings.Contains(out, "**Predecessor tl;dr:**") || !strings.Contains(out, "> Shipped it.") {
		t.Errorf("expected tldr block, got: %q", out)
	}
	if strings.Contains(out, "Handoff notes from your predecessor") {
		t.Errorf("should not render handoff block when handoff_notes empty, got: %q", out)
	}
}

func TestOutputPriorWorkOnBead_PredecessorEmptyWrapup_SkipsToNext(t *testing.T) {
	// Newest prior occupant left no usable wrap-up; the block must fall through
	// to an earlier occupant that authored a handoff rather than rendering blank.
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{"task_id": "gd-task"}}
	empty := &mockBead{ID: "gd-empty", Type: "agent", Status: "closed", Fields: map[string]string{"task_id": "gd-task"}}
	real := &mockBead{
		ID: "gd-real", Type: "agent", Status: "closed",
		Fields: map[string]string{
			"task_id": "gd-task",
			"wrapup":  wrapJSON(t, map[string]any{"handoff_notes": "Real handoff from two runs ago."}),
		},
	}
	// Newest-first ordering: the empty one is returned first.
	cleanup := setupPriorWorkTestDaemon(t, map[string]*mockBead{"gd-agent": agent}, []*mockBead{empty, real})
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWorkOnBead(&buf, "gd-agent")
	out := buf.String()
	if !strings.Contains(out, "Real handoff from two runs ago.") {
		t.Errorf("expected fall-through to the occupant with a real handoff, got: %q", out)
	}
	if !strings.Contains(out, "`gd-real`") {
		t.Errorf("expected gd-real credited as predecessor, got: %q", out)
	}
}

func TestOutputPriorWorkOnBead_ExcludesSelf(t *testing.T) {
	// A resume of the SAME bead must never render its own handoff back to itself.
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{"task_id": "gd-task"}}
	selfClosed := &mockBead{
		ID: "gd-agent", Type: "agent", Status: "closed",
		Fields: map[string]string{
			"task_id": "gd-task",
			"wrapup":  wrapJSON(t, map[string]any{"handoff_notes": "my own notes"}),
		},
	}
	cleanup := setupPriorWorkTestDaemon(t, map[string]*mockBead{"gd-agent": agent}, []*mockBead{selfClosed})
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWorkOnBead(&buf, "gd-agent")
	if buf.Len() != 0 {
		t.Errorf("expected empty output when the only occupant is self, got: %q", buf.String())
	}
}
