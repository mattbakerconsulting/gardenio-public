package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// parkedWireBead is the minimal JSON shape the fake daemon returns for the
// escalation-parked subtree regression.
type parkedWireBead struct {
	ID     string            `json:"id"`
	Title  string            `json:"title"`
	Type   string            `json:"type"`
	Status string            `json:"status"`
	Fields map[string]string `json:"fields"`
}

// TestGatherEscalationParked_GateSubtree is the ka-0000003000 regression. A
// molecule parks behind an operator handle whose requesting_task names its GATE
// STEP, not the molecule root. The exact-id match (relation (a)) never sees it,
// so before this fix the root fell through to `claimable (reapable)` — a
// data-loss path (NWS-1 would close/unassign a converged, human-gated
// deliverable). gatherEscalationParked must now resolve the molecule's
// parent-child subtree (relation (b)) and park the ROOT on the handle that
// names the gate step.
//
// It also pins the two properties the fix must preserve:
//   - the single-bead shape (a handle naming the stale bead directly) still parks;
//   - a subtree fetch is only issued for molecule/formula rows.
func TestGatherEscalationParked_GateSubtree(t *testing.T) {
	// Open operator handles. gd-dec-gate names the GATE STEP (gd-gate), which is
	// a child of the stale molecule gd-mol — the multi-step shape. gd-esc-single
	// names the single-bead stale task gd-single directly.
	handles := []parkedWireBead{
		{ID: "gd-dec-gate", Title: "approve cross-post", Type: "decision", Status: "open", Fields: map[string]string{"requesting_task": "gd-gate"}},
		{ID: "gd-esc-single", Title: "merge MR", Type: "escalation", Status: "open", Fields: map[string]string{"requesting_task": "gd-single"}},
		// A handle naming a step in NO stale subtree must not park anything.
		{ID: "gd-esc-noise", Title: "unrelated", Type: "escalation", Status: "open", Fields: map[string]string{"requesting_task": "gd-elsewhere"}},
	}
	// gd-mol's open parent-child subtree: the gate step (+ a still-open writer
	// child, to prove a non-handle descendant does not spuriously park).
	molSubtree := []parkedWireBead{
		{ID: "gd-gate", Title: "GATE — human approves", Type: "task", Status: "open"},
		{ID: "gd-writer", Title: "writer child", Type: "task", Status: "open"},
	}

	var subtreeParents []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var rows []parkedWireBead
		switch {
		case q.Get("parent") != "":
			// Subtree query — only gd-mol should ever be walked.
			subtreeParents = append(subtreeParents, q.Get("parent"))
			if q.Get("parent") == "gd-mol" {
				rows = molSubtree
			}
		case q.Get("type") == "escalation,decision" || q.Get("type") == "decision,escalation":
			rows = handles
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": rows, "total": len(rows)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	staleIP := []*beadsapi.BeadDetail{
		{ID: "gd-mol", Type: "molecule", Title: "standup-convergence", Assignee: "gone-gardener"},
		{ID: "gd-single", Type: "task", Title: "MR merge parked", Assignee: "gone-agent"},
		{ID: "gd-free", Type: "task", Title: "genuinely free", Assignee: "also-gone"},
	}

	parked := gatherEscalationParked(context.Background(), staleIP, []string{"project:gardenio"})

	// Relation (b): the molecule root parks on the handle that names its gate step.
	if got := parked["gd-mol"]; got != "gd-dec-gate" {
		t.Errorf("molecule root should park on gate handle gd-dec-gate, got %q (subtree match failed — the bug)", got)
	}
	// Relation (a): the single-bead shape still parks directly.
	if got := parked["gd-single"]; got != "gd-esc-single" {
		t.Errorf("single-bead task should park on gd-esc-single, got %q", got)
	}
	// A genuinely free stale bead stays unparked → classifyStale keeps it claimable.
	if got, ok := parked["gd-free"]; ok {
		t.Errorf("gd-free must not be parked, got handle %q", got)
	}
	// Only the molecule triggers a subtree walk — the single-bead + free rows do not.
	if len(subtreeParents) != 1 || subtreeParents[0] != "gd-mol" {
		t.Errorf("subtree fetch should be scoped to molecule/formula rows only, got parents=%v", subtreeParents)
	}
}

// TestGatherEscalationParked_OpenDecisionInSubtree pins the inverse safety net:
// a stale molecule whose subtree contains an OPEN decision/escalation is parked
// on that decision even when no handle's requesting_task points into the
// subtree — a converging container whose remaining open child is an un-actioned
// operator gate is never free work.
func TestGatherEscalationParked_OpenDecisionInSubtree(t *testing.T) {
	molSubtree := []parkedWireBead{
		{ID: "gd-gate2", Title: "GATE", Type: "task", Status: "open"},
		{ID: "gd-dec-child", Title: "approve", Type: "decision", Status: "open"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var rows []parkedWireBead
		if q.Get("parent") == "gd-mol2" {
			rows = molSubtree
		}
		// No open handles at all in this scenario.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": rows, "total": len(rows)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	staleIP := []*beadsapi.BeadDetail{
		{ID: "gd-mol2", Type: "molecule", Title: "gated molecule", Assignee: "gone"},
	}
	parked := gatherEscalationParked(context.Background(), staleIP, nil)
	if got := parked["gd-mol2"]; got != "gd-dec-child" {
		t.Errorf("molecule with an open decision child must park on it, got %q", got)
	}
}

// TestGatherEscalationParked_InProgressHandle is the gd-Z2E1CJKZR7 regression.
// The merge desk claims a merge escalation (status in_progress) while it lands
// the PR. The task the escalation names is still parked until the handle
// closes, but the handle query asked only for status=open, so the task fell
// through to `claimable (reapable)` mid-merge and invited a re-dispatch. The
// fake daemon answers per requested status, so the test fails unless the
// handle query asks for in_progress too.
func TestGatherEscalationParked_InProgressHandle(t *testing.T) {
	byStatus := map[string][]parkedWireBead{
		"open":        {{ID: "gd-esc-open", Title: "merge MR (queued)", Type: "escalation", Status: "open", Fields: map[string]string{"requesting_task": "gd-queued"}}},
		"in_progress": {{ID: "gd-esc-claimed", Title: "merge MR (merge desk landing it)", Type: "escalation", Status: "in_progress", Fields: map[string]string{"requesting_task": "gd-merging"}}},
		"closed":      {{ID: "gd-esc-done", Title: "merge MR (merged)", Type: "escalation", Status: "closed", Fields: map[string]string{"requesting_task": "gd-released"}}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		var rows []parkedWireBead
		for _, s := range strings.Split(r.URL.Query().Get("status"), ",") {
			rows = append(rows, byStatus[s]...)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": rows, "total": len(rows)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	staleIP := []*beadsapi.BeadDetail{
		{ID: "gd-queued", Type: "task", Title: "PR queued for merge", Assignee: "gone-a"},
		{ID: "gd-merging", Type: "task", Title: "PR the merge desk is landing", Assignee: "gone-b"},
		{ID: "gd-released", Type: "task", Title: "PR merged, handle closed", Assignee: "gone-c"},
	}
	parked := gatherEscalationParked(context.Background(), staleIP, []string{"project:gardenio"})

	if got := parked["gd-queued"]; got != "gd-esc-open" {
		t.Errorf("task behind an open handle should park on gd-esc-open, got %q", got)
	}
	if got := parked["gd-merging"]; got != "gd-esc-claimed" {
		t.Errorf("task behind an in_progress handle should park on gd-esc-claimed, got %q (the bug: listed as reapable mid-merge)", got)
	}
	if got, ok := parked["gd-released"]; ok {
		t.Errorf("a closed handle releases the park, but gd-released parked on %q", got)
	}
}
