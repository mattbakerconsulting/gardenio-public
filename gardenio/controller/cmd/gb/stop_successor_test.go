package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// --- Pure helper tests -------------------------------------------------------

func TestSuccessorAgentName_StripsSuffixAndReanchors(t *testing.T) {
	// A predecessor with a trailing random suffix: strip it and append a fresh
	// one so successive continuations don't accrete ever-longer names.
	got := successorAgentName("gardenio-fix-thing-ab12", "gardenio")
	if !strings.HasPrefix(got, "gardenio-fix-thing-") {
		t.Errorf("successor name %q must keep the lineage base gardenio-fix-thing-", got)
	}
	if got == "gardenio-fix-thing-ab12" {
		t.Errorf("successor name must differ from the predecessor (fresh suffix), got %q", got)
	}
	// Fallback: an empty predecessor name yields a project-prefixed random name.
	fb := successorAgentName("", "gardenio")
	if !strings.HasPrefix(fb, "gardenio-") {
		t.Errorf("empty predecessor name must fall back to a project-prefixed name, got %q", fb)
	}
}

func TestSuccessorPrompt_PrefersHandoffNotes(t *testing.T) {
	w := &WrapUp{HandoffNotes: "picked bug X, root cause is Y, next is the fix"}
	got := successorPrompt(w, map[string]string{"prompt": "original prompt"})
	if !strings.Contains(got, "root cause is Y") {
		t.Errorf("successor prompt must seed from handoff_notes, got %q", got)
	}
	if strings.Contains(got, "original prompt") {
		t.Errorf("handoff_notes must win over the predecessor prompt, got %q", got)
	}
}

func TestSuccessorPrompt_FallsBackToPredecessorPrompt(t *testing.T) {
	got := successorPrompt(&WrapUp{}, map[string]string{"prompt": "do the thing"})
	if !strings.Contains(got, "do the thing") {
		t.Errorf("with no handoff_notes the predecessor prompt must seed, got %q", got)
	}
}

func TestSuccessorPrompt_MinimalWhenNothingToSeed(t *testing.T) {
	got := successorPrompt(&WrapUp{}, map[string]string{})
	if strings.TrimSpace(got) == "" {
		t.Error("successor prompt must never be empty")
	}
}

func TestIsHumanShapedActor(t *testing.T) {
	cases := map[string]bool{
		"oidc:matt@x.ai":   true,
		"slack:U07K1RLQAK": true,
		"someone@x.ai":     true,
		"":                 false,
		"unknown":          false,
		"system:reaper":    false,
	}
	for in, want := range cases {
		if got := isHumanShapedActor(in); got != want {
			t.Errorf("isHumanShapedActor(%q) = %v, want %v", in, got, want)
		}
	}
}

// --- End-to-end mint test ----------------------------------------------------

// mintTestServer stands up a minimal daemon that serves the exact HTTP calls
// SpawnAgent + GetBead make, capturing the successor create request so the test
// can assert the neutral succession fields land on the minted bead.
type mintTestServer struct {
	pred        *beadsapi.BeadDetail
	createBody  map[string]any
	createdFrom map[string]any // decoded fields of the created successor bead
	// lineageCount is the count LineageContinuationCount's count-only query
	// returns — the existing chain length. Lets a test drive the budget gate.
	lineageCount int
	// createCalled records whether CreateBead(successor) was reached — false
	// means the budget gate short-circuited the mint before any bead was created.
	createCalled bool
}

func (m *mintTestServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		// GetBead(predecessor)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+m.pred.ID:
			writeJSON(w, beadDetailToJSONMap(m.pred))
		// LineageContinuationCount → count-only list of type=agent beads. Must be
		// matched BEFORE the generic agents-by-name GET; it's a GET on /v1/beads
		// with count_only=true.
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads" && r.URL.Query().Get("count_only") == "true":
			writeJSON(w, map[string]any{"beads": nil, "total": m.lineageCount})
		// findActiveAgentByName → 404 (dedup miss, so the mint proceeds)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"error": "not found"})
		// CreateBead(successor)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			m.createCalled = true
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &m.createBody)
			if raw, ok := m.createBody["fields"]; ok {
				// fields arrives as a JSON string (json.RawMessage marshaled).
				switch v := raw.(type) {
				case string:
					_ = json.Unmarshal([]byte(v), &m.createdFrom)
				case map[string]any:
					m.createdFrom = v
				}
			}
			writeJSON(w, map[string]any{"id": "gd-successor-1", "type": "agent"})
		// Best-effort post-create: labels / deps / anything else → 200 ok.
		default:
			w.WriteHeader(http.StatusOK)
			writeJSON(w, map[string]any{})
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// beadDetailToJSONMap renders a BeadDetail into the daemon's wire shape enough
// for GetBead's decoder (id/type/status/fields).
func beadDetailToJSONMap(b *beadsapi.BeadDetail) map[string]any {
	return map[string]any{
		"id":     b.ID,
		"type":   b.Type,
		"status": b.Status,
		"title":  b.Title,
		"fields": b.Fields,
	}
}

func TestMintSuccessorAgent_StampsLineageFields(t *testing.T) {
	mts := &mintTestServer{
		pred: &beadsapi.BeadDetail{
			ID:     "gd-pred-1",
			Type:   "agent",
			Status: "in_progress",
			Title:  "gardenio-worker-ab12",
			Fields: map[string]string{
				"agent":                  "gardenio-worker-ab12",
				"project":                "gardenio",
				"role":                   "crew",
				"mode":                   "crew",
				"task_id":                "gd-task-9",
				"gardener_id":            "gd-gardener-7",
				"schedule_terminal_goal": "ship the feature",
				"lineage_root_id":        "gd-root-orig",
				"operated_by":            "oidc:matt@x.ai",
			},
		},
	}
	srv := httptest.NewServer(mts.handler(t))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	w := &WrapUp{Successor: SuccessorNew, HandoffNotes: "left off at step 3"}
	successorID, err := mintSuccessorAgent(context.Background(), "gd-pred-1", w, "oidc:matt@x.ai")
	if err != nil {
		t.Fatalf("mintSuccessorAgent: %v", err)
	}
	if successorID != "gd-successor-1" {
		t.Errorf("successor id = %q, want gd-successor-1", successorID)
	}
	if mts.createdFrom == nil {
		t.Fatal("no successor bead was created")
	}
	// The two neutral succession back-pointers — the heart of this task.
	if got := mts.createdFrom["prior_agent_id"]; got != "gd-pred-1" {
		t.Errorf("prior_agent_id = %v, want gd-pred-1", got)
	}
	// lineage_root_id is inherited from the predecessor's own root (chain
	// continues), NOT reset to the predecessor id.
	if got := mts.createdFrom["lineage_root_id"]; got != "gd-root-orig" {
		t.Errorf("lineage_root_id = %v, want gd-root-orig (inherited chain root)", got)
	}
	// Inherited ownership + goal so the successor is never an orphan.
	if got := mts.createdFrom["gardener_id"]; got != "gd-gardener-7" {
		t.Errorf("gardener_id = %v, want gd-gardener-7 (inherited)", got)
	}
	if got := mts.createdFrom["spawn_source"]; got != "successor" {
		t.Errorf("spawn_source = %v, want successor", got)
	}
}

func TestMintSuccessorAgent_AnchorsRootWhenPredecessorHasNone(t *testing.T) {
	mts := &mintTestServer{
		pred: &beadsapi.BeadDetail{
			ID:     "gd-pred-2",
			Type:   "agent",
			Status: "in_progress",
			Title:  "gardenio-solo-cd34",
			Fields: map[string]string{
				"agent":   "gardenio-solo-cd34",
				"project": "gardenio",
				"role":    "crew",
				// No lineage_root_id: predecessor is itself a root.
			},
		},
	}
	srv := httptest.NewServer(mts.handler(t))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	_, err = mintSuccessorAgent(context.Background(), "gd-pred-2", &WrapUp{Successor: SuccessorNew}, "")
	if err != nil {
		t.Fatalf("mintSuccessorAgent: %v", err)
	}
	// A predecessor with no recorded root anchors the NEW chain at its own id.
	if got := mts.createdFrom["lineage_root_id"]; got != "gd-pred-2" {
		t.Errorf("lineage_root_id = %v, want gd-pred-2 (predecessor anchors the chain)", got)
	}
	if got := mts.createdFrom["prior_agent_id"]; got != "gd-pred-2" {
		t.Errorf("prior_agent_id = %v, want gd-pred-2", got)
	}
}

// --- Per-lineage-root continuation budget (ka-0000001361) --------------------

// newLineageMintTest wires a mint test server + client for the budget tests and
// returns the server so the test can assert whether CreateBead was reached.
func newLineageMintTest(t *testing.T, pred *beadsapi.BeadDetail, lineageCount int) *mintTestServer {
	t.Helper()
	mts := &mintTestServer{pred: pred, lineageCount: lineageCount}
	srv := httptest.NewServer(mts.handler(t))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
	return mts
}

func lineagePred(root string, extraFields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{
		"agent":           "gardenio-loop-ab12",
		"project":         "gardenio",
		"role":            "crew",
		"lineage_root_id": root,
	}
	for k, v := range extraFields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:     "gd-pred-loop",
		Type:   "agent",
		Status: "in_progress",
		Title:  "gardenio-loop-ab12",
		Fields: f,
	}
}

// TestMintSuccessorAgent_BudgetCeilingBlocksMint is the core enforcement test:
// once the lineage has DefaultMaxLineageContinuations beads, the next
// successor=new mint is refused with errLineageBudgetSpent and NO bead is
// created — the self-spawn loop is broken.
func TestMintSuccessorAgent_BudgetCeilingBlocksMint(t *testing.T) {
	mts := newLineageMintTest(t, lineagePred("gd-root-loop", nil), beadsapi.DefaultMaxLineageContinuations)

	_, err := mintSuccessorAgent(context.Background(), "gd-pred-loop", &WrapUp{Successor: SuccessorNew}, "")
	if err == nil {
		t.Fatal("expected mint to be refused at the ceiling, got nil error")
	}
	if !errors.Is(err, errLineageBudgetSpent) {
		t.Errorf("error = %v, want errLineageBudgetSpent", err)
	}
	if mts.createCalled {
		t.Error("CreateBead must NOT be called once the budget is spent")
	}
}

// TestMintSuccessorAgent_UnderBudgetMintsNormally is the no-regression guard: a
// lineage below the ceiling mints its successor as before.
func TestMintSuccessorAgent_UnderBudgetMintsNormally(t *testing.T) {
	// One shy of the ceiling → still allowed.
	mts := newLineageMintTest(t, lineagePred("gd-root-ok", nil), beadsapi.DefaultMaxLineageContinuations-1)

	id, err := mintSuccessorAgent(context.Background(), "gd-pred-loop", &WrapUp{Successor: SuccessorNew}, "")
	if err != nil {
		t.Fatalf("mint under budget must succeed, got %v", err)
	}
	if id != "gd-successor-1" {
		t.Errorf("successor id = %q, want gd-successor-1", id)
	}
	if !mts.createCalled {
		t.Error("CreateBead must be called when under budget")
	}
}

// TestMintSuccessorAgent_BudgetOverrideRaisesCeiling confirms a per-lineage
// max_lineage_continuations override is honored AND carried forward to the
// successor so the whole chain shares one ceiling.
func TestMintSuccessorAgent_BudgetOverrideRaisesCeiling(t *testing.T) {
	// Chain length equals the DEFAULT ceiling, but an override raises it, so the
	// mint is still allowed.
	pred := lineagePred("gd-root-override", map[string]string{
		beadsapi.FieldMaxLineageContinuations: "50",
	})
	mts := newLineageMintTest(t, pred, beadsapi.DefaultMaxLineageContinuations)

	id, err := mintSuccessorAgent(context.Background(), "gd-pred-loop", &WrapUp{Successor: SuccessorNew}, "")
	if err != nil {
		t.Fatalf("mint under raised ceiling must succeed, got %v", err)
	}
	if id != "gd-successor-1" {
		t.Errorf("successor id = %q, want gd-successor-1", id)
	}
	// The override must be carried forward so the chain keeps one ceiling.
	if got := mts.createdFrom[beadsapi.FieldMaxLineageContinuations]; got != "50" {
		t.Errorf("max_lineage_continuations on successor = %v, want 50 (carried forward)", got)
	}
}

// TestMintSuccessorAgent_CountErrorFailsOpen confirms a transient count failure
// does NOT dead-end a legitimate continuation — the gate fails open.
func TestMintSuccessorAgent_CountErrorFailsOpen(t *testing.T) {
	// A handler that 500s the count query but serves everything else lets us
	// assert the mint proceeds despite the failed budget check.
	mts := &mintTestServer{pred: lineagePred("gd-root-failopen", nil)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/beads" && r.URL.Query().Get("count_only") == "true" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mts.handler(t)(w, r)
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	id, err := mintSuccessorAgent(context.Background(), "gd-pred-loop", &WrapUp{Successor: SuccessorNew}, "")
	if err != nil {
		t.Fatalf("count failure must fail open (mint proceeds), got %v", err)
	}
	if id != "gd-successor-1" {
		t.Errorf("successor id = %q, want gd-successor-1", id)
	}
	if !mts.createCalled {
		t.Error("CreateBead must be called when the budget check fails open")
	}
}

func TestClosePredecessorAsSuperseded_WritesContractCloseReason(t *testing.T) {
	var closeBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close") {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &closeBody)
		}
		writeJSON(w, map[string]any{})
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	if err := closePredecessorAsSuperseded(context.Background(), "gd-pred-1", "gd-successor-1", "oidc:matt@x.ai"); err != nil {
		t.Fatalf("closePredecessorAsSuperseded: %v", err)
	}
	// The exact prefix is a cross-package contract (bridge parses it back out).
	want := "superseded: continued as gd-successor-1"
	if got := closeBody["close_reason"]; got != want {
		t.Errorf("close_reason = %v, want %q", got, want)
	}
	if got := closeBody["agent_state"]; got != "done" {
		t.Errorf("agent_state = %v, want done", got)
	}
}
