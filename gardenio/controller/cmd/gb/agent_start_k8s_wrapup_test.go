package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// midSessionServer is a tiny fake beads daemon for the terminated-mid-session
// wrap-up tests. It serves a single agent bead (GET) and records the wrapup
// value the client PATCHes (Fix C, ka-0000007005).
type midSessionServer struct {
	beadID     string
	status     string
	fields     map[string]string
	patchCount atomic.Int64
	closeCount atomic.Int64
	lastWrapup atomic.Value // string
}

func newMidSessionDaemon(t *testing.T, s *midSessionServer) *beadsapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+s.beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     s.beadID,
				"type":   "agent",
				"status": s.status,
				"fields": s.fields,
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/beads/"+s.beadID:
			s.patchCount.Add(1)
			var body struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var patch map[string]any
			_ = json.Unmarshal(body.Fields, &patch)
			if wu, ok := patch[WrapUpFieldName].(string); ok {
				s.lastWrapup.Store(wu)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+s.beadID+"/close":
			s.closeCount.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": s.beadID})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
	return c
}

// cancelledCtx returns a context that is already cancelled, mimicking the
// SIGTERM/SIGINT/SIGHUP graceful-shutdown path in runAgentStartK8s.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestWriteTerminatedMidSessionWrapUp_StampsOpenBead is the core case: on a
// graceful (ctx-cancelled) teardown of an OPEN agent bead with no wrap-up, a
// synthesized "terminated mid-session" wrap-up is PATCHed onto the bead and the
// bead is NOT closed — so the resumed pod reads the last progress.
func TestWriteTerminatedMidSessionWrapUp_StampsOpenBead(t *testing.T) {
	const beadID = "gd-tkvbpqykyv-open"
	s := &midSessionServer{
		beadID: beadID,
		status: "open",
		fields: map[string]string{"agent_state": "working", "agent": "test-crew-1"},
	}
	newMidSessionDaemon(t, s)

	writeTerminatedMidSessionWrapUp(cancelledCtx(), beadID)

	if got := s.patchCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 field PATCH, got %d", got)
	}
	if got := s.closeCount.Load(); got != 0 {
		t.Fatalf("expected the bead to stay OPEN (0 closes), got %d closes", got)
	}

	raw, _ := s.lastWrapup.Load().(string)
	if raw == "" {
		t.Fatal("no wrapup was written")
	}
	// The written wrap-up must be readable back as a valid WrapUp — this is the
	// "verify the wrap-up is readable on restart" contract.
	var wu WrapUp
	if err := json.Unmarshal([]byte(raw), &wu); err != nil {
		t.Fatalf("synthesized wrap-up is not valid JSON: %v\n%s", err, raw)
	}
	if !strings.Contains(wu.TLDR, "terminated mid-session") {
		t.Errorf("tldr should name the mid-session termination, got: %q", wu.TLDR)
	}
	if wu.Accomplishments == "" {
		t.Error("accomplishments must be populated (required wrap-up field)")
	}
	if wu.Custom["synthesized"] != "true" {
		t.Errorf("wrap-up should be marked synthesized=true, got: %v", wu.Custom)
	}
	if wu.Custom["terminated_mid_session"] != "true" {
		t.Errorf("wrap-up should be marked terminated_mid_session=true, got: %v", wu.Custom)
	}
	if wu.Custom["close_reason"] != terminatedMidSessionCloseReason {
		t.Errorf("close_reason custom = %q, want %q", wu.Custom["close_reason"], terminatedMidSessionCloseReason)
	}
	// The agent identity should give the tldr some specificity.
	if !strings.Contains(wu.TLDR, "test-crew-1") {
		t.Errorf("tldr should append the agent identity for context, got: %q", wu.TLDR)
	}
}

// TestWriteTerminatedMidSessionWrapUp_FoldsInToolCalls verifies partial
// progress (recorded tool calls) is surfaced in the synthesized headline so a
// mid-work teardown doesn't read as a no-op.
func TestWriteTerminatedMidSessionWrapUp_FoldsInToolCalls(t *testing.T) {
	const beadID = "gd-tkvbpqykyv-work"
	s := &midSessionServer{
		beadID: beadID,
		status: "open",
		fields: map[string]string{"agent_state": "working", "total_tool_calls": "42"},
	}
	newMidSessionDaemon(t, s)

	writeTerminatedMidSessionWrapUp(cancelledCtx(), beadID)

	raw, _ := s.lastWrapup.Load().(string)
	var wu WrapUp
	if err := json.Unmarshal([]byte(raw), &wu); err != nil {
		t.Fatalf("invalid wrap-up JSON: %v", err)
	}
	if !strings.Contains(wu.TLDR, "42 tool calls") {
		t.Errorf("tldr should fold in the 42 recorded tool calls, got: %q", wu.TLDR)
	}
	if !strings.Contains(wu.Accomplishments, "42 tool calls") {
		t.Errorf("accomplishments should reflect the partial progress, got: %q", wu.Accomplishments)
	}
}

// TestWriteTerminatedMidSessionWrapUp_PreservesExistingWrapUp pins the
// invariant that a real (agent-authored or already-synthesized) wrap-up always
// wins — the teardown write must never clobber it.
func TestWriteTerminatedMidSessionWrapUp_PreservesExistingWrapUp(t *testing.T) {
	const beadID = "gd-tkvbpqykyv-haswrap"
	s := &midSessionServer{
		beadID: beadID,
		status: "open",
		fields: map[string]string{
			WrapUpFieldName: `{"tldr":"real work done","accomplishments":"merged MR !123"}`,
		},
	}
	newMidSessionDaemon(t, s)

	writeTerminatedMidSessionWrapUp(cancelledCtx(), beadID)

	if got := s.patchCount.Load(); got != 0 {
		t.Fatalf("expected NO write when a wrap-up already exists, got %d PATCHes", got)
	}
}

// TestWriteTerminatedMidSessionWrapUp_SkipsClosedBead verifies a bead that
// already ran its terminal close path (status=closed, which synthesizes its own
// fallback) is left untouched.
func TestWriteTerminatedMidSessionWrapUp_SkipsClosedBead(t *testing.T) {
	const beadID = "gd-tkvbpqykyv-closed"
	s := &midSessionServer{
		beadID: beadID,
		status: "closed",
		fields: map[string]string{"agent_state": "done"},
	}
	newMidSessionDaemon(t, s)

	writeTerminatedMidSessionWrapUp(cancelledCtx(), beadID)

	if got := s.patchCount.Load(); got != 0 {
		t.Fatalf("expected NO write on a closed bead, got %d PATCHes", got)
	}
}

// TestWriteTerminatedMidSessionWrapUp_SkipsUncancelledContext verifies the
// deferred write is a no-op on a normal (non-graceful) return — it must fire
// ONLY on the signal-cancelled shutdown path.
func TestWriteTerminatedMidSessionWrapUp_SkipsUncancelledContext(t *testing.T) {
	const beadID = "gd-tkvbpqykyv-live"
	s := &midSessionServer{
		beadID: beadID,
		status: "open",
		fields: map[string]string{"agent_state": "working"},
	}
	newMidSessionDaemon(t, s)

	// A live (uncancelled) context — the max-restarts / setup-error return path.
	writeTerminatedMidSessionWrapUp(context.Background(), beadID)

	if got := s.patchCount.Load(); got != 0 {
		t.Fatalf("expected NO write on an uncancelled context, got %d PATCHes", got)
	}
}

// TestWriteTerminatedMidSessionWrapUp_NoBeadID is a defensive no-op check: an
// empty agent bead ID (unresolved identity) must never panic or write.
func TestWriteTerminatedMidSessionWrapUp_NoBeadID(t *testing.T) {
	s := &midSessionServer{beadID: "unused", status: "open", fields: map[string]string{}}
	newMidSessionDaemon(t, s)

	writeTerminatedMidSessionWrapUp(cancelledCtx(), "")

	if got := s.patchCount.Load(); got != 0 {
		t.Fatalf("expected NO write with an empty bead ID, got %d PATCHes", got)
	}
}

// TestSynthesizeTerminatedMidSessionWrapUp_Shape unit-tests the synthesizer in
// isolation: it always produces valid JSON with the required fields, whether or
// not tool calls were recorded.
func TestSynthesizeTerminatedMidSessionWrapUp_Shape(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bead      *beadsapi.BeadDetail
		wantTool  bool
		wantTitle string
	}{
		{
			name: "no tool calls",
			bead: &beadsapi.BeadDetail{ID: "b1", Title: "some task", Fields: map[string]string{}},
		},
		{
			name:     "with tool calls",
			bead:     &beadsapi.BeadDetail{ID: "b2", Fields: map[string]string{"total_tool_calls": "7", "agent": "crew-x"}},
			wantTool: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := synthesizeTerminatedMidSessionWrapUp(tc.bead)
			if out == "" {
				t.Fatal("synthesizer returned empty")
			}
			var wu WrapUp
			if err := json.Unmarshal([]byte(out), &wu); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, out)
			}
			if wu.TLDR == "" || wu.Accomplishments == "" {
				t.Errorf("tldr/accomplishments must be non-empty, got %+v", wu)
			}
			if wu.Custom["terminated_mid_session"] != "true" {
				t.Errorf("missing terminated_mid_session marker: %v", wu.Custom)
			}
			if tc.wantTool && !strings.Contains(wu.TLDR, "tool call") {
				t.Errorf("expected tool-call count in tldr, got %q", wu.TLDR)
			}
		})
	}
}
