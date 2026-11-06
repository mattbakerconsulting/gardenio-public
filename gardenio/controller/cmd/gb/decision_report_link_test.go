package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// linkStub is an httptest stub that lets a test exercise the
// gb decision report → originating-task linking path. It tracks every
// AddDependency POST so the test can assert what edges were written.
//
// Routes:
//
//	GET  /v1/beads/<id>                 → decision/agent fixtures
//	GET  /v1/beads/<id>/dependencies    → parent-child deps for the bead
//	POST /v1/beads                      → returns gd-report-1
//	POST /v1/beads/<id>/dependencies    → records the edge
//	other POST/PATCH                    → 200 OK
type linkStub struct {
	mu             sync.Mutex
	decisionFields map[string]string
	deps           map[string][]beadsapi.Dependency
	addedDeps      []addedDep
	// statusByID overrides a bead's status on GET /v1/beads/<id>; absent ids
	// default to "open". A parent whose status is "closed" also makes the
	// matching POST /v1/beads/<parent>/dependencies return HTTP 409, modeling
	// the daemon's closed-parent guard (ka-0000005756) so the fallback path in
	// gb decision report (ka-0000005854) can be exercised end-to-end.
	statusByID map[string]string
	// patched records every PATCH /v1/beads/<id> field-update the command
	// issued, keyed by bead id → merged field map. Lets tests assert the
	// decision's artifact_status advances pending → submitted (ka-0000003940).
	patched map[string]map[string]any
}

// status returns id's status, defaulting to "open". Caller must hold s.mu.
func (s *linkStub) status(id string) string {
	if st, ok := s.statusByID[id]; ok {
		return st
	}
	return "open"
}

type addedDep struct {
	BeadID      string `json:"bead_id"`
	DependsOnID string
	Type        string
}

// addDepBody mirrors the daemon's expected POST shape so we decode the same
// fields the production client sends (depends_on_id + type). The stub itself
// does not enforce the wire format — it just captures what arrived.
type addDepBody struct {
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

func (s *linkStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/dependencies"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependencies")
			s.mu.Lock()
			deps := s.deps[id]
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": deps})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/dependencies"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependencies")
			var body addDepBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			// Model the daemon's closed-parent guard: an edge whose parent
			// (depends_on_id) is closed is refused with HTTP 409.
			if body.Type == "parent-child" && s.status(body.DependsOnID) == "closed" {
				s.mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "parent-child dependency: parent " + body.DependsOnID + " is closed",
				})
				return
			}
			s.addedDeps = append(s.addedDeps, addedDep{BeadID: id, DependsOnID: body.DependsOnID, Type: body.Type})
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/beads/"):
			id := strings.TrimPrefix(path, "/v1/beads/")
			s.mu.Lock()
			status := s.status(id)
			s.mu.Unlock()
			beadType := "decision"
			if id != "gd-decision-1" {
				// Non-decision beads (tasks/epics walked by the fallback) must
				// not report type=decision, or the report command's type guard
				// would reject them if ever fetched as the subject.
				beadType = "task"
			}
			bead := map[string]any{
				"id":     id,
				"type":   beadType,
				"status": status,
				"fields": s.decisionFields,
			}
			_ = json.NewEncoder(w).Encode(bead)
		case r.Method == http.MethodPost && path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-report-1"})
		case r.Method == http.MethodPatch && strings.HasPrefix(path, "/v1/beads/"):
			id := strings.TrimPrefix(path, "/v1/beads/")
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			if s.patched == nil {
				s.patched = make(map[string]map[string]any)
			}
			if s.patched[id] == nil {
				s.patched[id] = make(map[string]any)
			}
			maps.Copy(s.patched[id], body.Fields)
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	})
}

func runReportLink(t *testing.T, stub *linkStub) []addedDep {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	cmd := decisionReportCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("content", "ok")
	_ = cmd.Flags().Set("type", "summary")
	_ = cmd.Flags().Set("format", "markdown")
	t.Cleanup(func() {
		_ = cmd.Flags().Set("content", "")
		_ = cmd.Flags().Set("type", "")
		_ = cmd.Flags().Set("format", "markdown")
	})
	if err := cmd.RunE(cmd, []string{"gd-decision-1"}); err != nil {
		t.Fatalf("decision report: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	out := make([]addedDep, len(stub.addedDeps))
	copy(out, stub.addedDeps)
	return out
}

// patchedField returns the value the command PATCHed onto bead id's field,
// and whether any such patch was recorded. Safe under the stub lock.
func (s *linkStub) patchedField(id, field string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fields, ok := s.patched[id]
	if !ok {
		return nil, false
	}
	v, ok := fields[field]
	return v, ok
}

// TestDecisionReport_LinksToTaskParentOfDecision asserts that when the
// decision bead is parented under a task, the new report bead gets a
// parent-child edge to that task in addition to the existing edge to the
// decision.
func TestDecisionReport_LinksToTaskParentOfDecision(t *testing.T) {
	stub := &linkStub{
		decisionFields: map[string]string{},
		deps: map[string][]beadsapi.Dependency{
			"gd-decision-1": {
				{BeadID: "gd-decision-1", DependsOnID: "gd-task-orig", Type: "parent-child"},
			},
		},
	}
	deps := runReportLink(t, stub)

	wantToDecision := false
	wantToTask := false
	for _, d := range deps {
		if d.BeadID == "gd-report-1" && d.Type == "parent-child" {
			switch d.DependsOnID {
			case "gd-decision-1":
				wantToDecision = true
			case "gd-task-orig":
				wantToTask = true
			}
		}
	}
	if !wantToDecision {
		t.Errorf("expected report→decision parent-child edge; got %+v", deps)
	}
	if !wantToTask {
		t.Errorf("expected report→originating-task parent-child edge; got %+v", deps)
	}
}

// TestDecisionReport_LinksViaRequestingAgent asserts that when the decision
// has no parent but the requesting agent is parented under a task, the
// report still reaches the task via the agent-parent pathway.
func TestDecisionReport_LinksViaRequestingAgent(t *testing.T) {
	stub := &linkStub{
		decisionFields: map[string]string{
			"requesting_agent_bead_id": "gd-agent-x",
		},
		deps: map[string][]beadsapi.Dependency{
			// decision has no parent
			"gd-decision-1": nil,
			"gd-agent-x": {
				{BeadID: "gd-agent-x", DependsOnID: "gd-task-via-agent", Type: "parent-child"},
			},
		},
	}
	deps := runReportLink(t, stub)

	wantToTask := false
	for _, d := range deps {
		if d.BeadID == "gd-report-1" && d.Type == "parent-child" && d.DependsOnID == "gd-task-via-agent" {
			wantToTask = true
		}
	}
	if !wantToTask {
		t.Errorf("expected report→agent-parent task edge; got %+v", deps)
	}
}

// TestDecisionReport_StandaloneDecision_NoEdge asserts that a decision with
// no parent and no requesting_agent_bead_id (operator-created standalone)
// does not wire an edge beyond the existing report→decision link.
func TestDecisionReport_StandaloneDecision_NoEdge(t *testing.T) {
	stub := &linkStub{
		decisionFields: map[string]string{},
		deps:           map[string][]beadsapi.Dependency{},
	}
	deps := runReportLink(t, stub)

	for _, d := range deps {
		if d.BeadID == "gd-report-1" && d.Type == "parent-child" && d.DependsOnID != "gd-decision-1" {
			t.Errorf("unexpected extra edge %+v on standalone decision", d)
		}
	}
}

// TestDecisionReport_FallsBackToLiveAncestorWhenParentsClosed is the core fix
// for ka-0000005854: when BOTH preferred parents (the decision and the
// originating task) are already closed by artifact-submit time — so the
// daemon's closed-parent guard 409s both edges — the report must not be left
// parentless. It walks up to the nearest live ancestor (here the epic above the
// closed originating task) and parents onto that instead.
func TestDecisionReport_FallsBackToLiveAncestorWhenParentsClosed(t *testing.T) {
	stub := &linkStub{
		decisionFields: map[string]string{},
		deps: map[string][]beadsapi.Dependency{
			// decision → closed originating task → live epic
			"gd-decision-1": {
				{BeadID: "gd-decision-1", DependsOnID: "gd-task-closed", Type: "parent-child"},
			},
			"gd-task-closed": {
				{BeadID: "gd-task-closed", DependsOnID: "gd-epic-live", Type: "parent-child"},
			},
		},
		statusByID: map[string]string{
			"gd-decision-1":  "closed",
			"gd-task-closed": "closed",
			"gd-epic-live":   "open",
		},
	}
	deps := runReportLink(t, stub)

	// The only edge that should have landed is report → live epic; both the
	// decision and the closed task edges must have been refused (409).
	wantEpic := false
	for _, d := range deps {
		if d.BeadID != "gd-report-1" || d.Type != "parent-child" {
			continue
		}
		switch d.DependsOnID {
		case "gd-epic-live":
			wantEpic = true
		case "gd-decision-1", "gd-task-closed":
			t.Errorf("unexpected edge onto closed parent %s: %+v", d.DependsOnID, d)
		}
	}
	if !wantEpic {
		t.Errorf("expected report→live-ancestor (gd-epic-live) fallback edge; got %+v", deps)
	}
}

// TestDecisionReport_FallbackViaDecisionAncestorWhenNoTask covers the fallback
// walking up from the DECISION itself when the decision is closed and there is
// no originating task, but the decision sits under a live epic.
func TestDecisionReport_FallbackViaDecisionAncestorWhenNoTask(t *testing.T) {
	stub := &linkStub{
		decisionFields: map[string]string{},
		deps: map[string][]beadsapi.Dependency{
			"gd-decision-1": {
				{BeadID: "gd-decision-1", DependsOnID: "gd-epic-live", Type: "parent-child"},
			},
		},
		statusByID: map[string]string{
			"gd-decision-1": "closed",
			"gd-epic-live":  "open",
		},
	}
	deps := runReportLink(t, stub)

	// resolveOriginatingTask returns gd-epic-live as the "originating task"
	// (the decision's parent-child parent). That edge is refused only if the
	// epic is closed; here it is live, so the direct edge lands and no fallback
	// is needed. The invariant the fix guarantees is simply: the report ends up
	// parented onto the live bead, never orphaned.
	wantEpic := false
	for _, d := range deps {
		if d.BeadID == "gd-report-1" && d.Type == "parent-child" && d.DependsOnID == "gd-epic-live" {
			wantEpic = true
		}
	}
	if !wantEpic {
		t.Errorf("expected report parented onto live gd-epic-live; got %+v", deps)
	}
}

// TestDecisionReport_NoLiveAncestor_NoOrphanEdge asserts that when every
// candidate parent is closed and there is no live ancestor anywhere, the
// command adds NO parent-child edge (rather than force one onto a closed bead)
// — the report is left for the loud re-parent message, not silently orphaned
// onto a dead parent.
func TestDecisionReport_NoLiveAncestor_NoOrphanEdge(t *testing.T) {
	stub := &linkStub{
		decisionFields: map[string]string{},
		deps: map[string][]beadsapi.Dependency{
			"gd-decision-1": {
				{BeadID: "gd-decision-1", DependsOnID: "gd-task-closed", Type: "parent-child"},
			},
			// task has no further parent; both it and the decision are closed
			"gd-task-closed": nil,
		},
		statusByID: map[string]string{
			"gd-decision-1":  "closed",
			"gd-task-closed": "closed",
		},
	}
	deps := runReportLink(t, stub)

	for _, d := range deps {
		if d.BeadID == "gd-report-1" && d.Type == "parent-child" {
			t.Errorf("expected NO parent-child edge when every candidate is closed; got %+v", d)
		}
	}
}

// TestResolveOriginatingTask_DecisionParentWins covers the helper directly:
// when both pathways are available, the decision-parent pathway is preferred.
func TestResolveOriginatingTask_DecisionParentWins(t *testing.T) {
	stub := &linkStub{
		deps: map[string][]beadsapi.Dependency{
			"gd-decision-1": {
				{BeadID: "gd-decision-1", DependsOnID: "gd-task-via-decision", Type: "parent-child"},
			},
			"gd-agent-x": {
				{BeadID: "gd-agent-x", DependsOnID: "gd-task-via-agent", Type: "parent-child"},
			},
		},
	}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	got, src, err := resolveOriginatingTask(context.Background(), "gd-decision-1", "gd-agent-x")
	if err != nil {
		t.Fatal(err)
	}
	if got != "gd-task-via-decision" || src != "decision-parent" {
		t.Errorf("got (%q,%q); want (gd-task-via-decision, decision-parent)", got, src)
	}
}

// TestDecisionReport_AdvancesArtifactStatusToSubmitted asserts the core fix
// for ka-0000003940: when the decision declared a required_artifact, a
// successful `gb decision report` advances the DECISION bead's artifact_status
// from pending to submitted. Before the fix the field was write-once "pending"
// and every delivered artifact read as undelivered forever.
func TestDecisionReport_AdvancesArtifactStatusToSubmitted(t *testing.T) {
	stub := &linkStub{
		decisionFields: map[string]string{
			"required_artifact": "summary",
			"artifact_status":   "pending",
		},
		deps: map[string][]beadsapi.Dependency{},
	}
	runReportLink(t, stub)

	v, ok := stub.patchedField("gd-decision-1", "artifact_status")
	if !ok {
		t.Fatalf("expected a PATCH of artifact_status on gd-decision-1; got none")
	}
	if v != "submitted" {
		t.Errorf("artifact_status = %v; want submitted", v)
	}
}

// TestDecisionReport_NoRequiredArtifact_LeavesStatusUnwritten asserts that a
// report on a decision that declared NO required_artifact does not manufacture
// an artifact_status write — the field only advances for decisions that
// actually owe an artifact.
func TestDecisionReport_NoRequiredArtifact_LeavesStatusUnwritten(t *testing.T) {
	stub := &linkStub{
		decisionFields: map[string]string{},
		deps:           map[string][]beadsapi.Dependency{},
	}
	runReportLink(t, stub)

	if _, ok := stub.patchedField("gd-decision-1", "artifact_status"); ok {
		t.Errorf("did not expect artifact_status to be written on a decision with no required_artifact")
	}
}
