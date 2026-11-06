package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// parkedAgentBeadJSON renders a closed, parked-persistent-identity agent bead
// as the daemon's GET /v1/beads/<id> JSON. fields is marshaled verbatim.
func parkedAgentBeadJSON(id string, fields map[string]any) map[string]any {
	return map[string]any{
		"id":     id,
		"title":  "parked-agent",
		"type":   "agent",
		"status": "closed",
		"labels": []string{"project:gardenio"},
		"fields": fields,
	}
}

// wakeTestServer stands up a daemon stub that serves one parked agent bead and
// records the PATCH body + whether a comment was posted. patchBody is nil until
// a PATCH lands.
type wakeTestServer struct {
	patchBody map[string]any
	commented bool
}

func newWakeServer(t *testing.T, id string, fields map[string]any) (*wakeTestServer, *httptest.Server) {
	t.Helper()
	st := &wakeTestServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+id:
			_ = json.NewEncoder(w).Encode(parkedAgentBeadJSON(id, fields))
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/beads/"+id:
			_ = json.NewDecoder(r.Body).Decode(&st.patchBody)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+id+"/comments":
			st.commented = true
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected daemon call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return st, srv
}

func runWakeCmd(t *testing.T, args ...string) error {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.Flags().Bool("force", false, "")
	// Mirror wakeCmd's flag defaults; individual tests flip --force via args.
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	return runWake(cmd, cmd.Flags().Args())
}

// TestRunWake_RevivesParkedIdentity is the happy path: a closed agent carrying
// successor=same + a future next_respawn_at + agent_state=dormant is reopened,
// its wake metadata cleared, and its respawn_count bumped — all in ONE PATCH.
func TestRunWake_RevivesParkedIdentity(t *testing.T) {
	oldActor := actor
	actor = "tester"
	t.Cleanup(func() { actor = oldActor })

	st, srv := newWakeServer(t, "gd-parked", map[string]any{
		"agent":           "gardenio-thread-abc",
		"successor":       "same",
		"agent_state":     "dormant",
		"next_respawn_at": "2099-01-01T00:00:00Z",
		"respawn_count":   "1",
	})
	swapDaemon(t, srv.URL)

	if err := runWakeCmd(t, "gd-parked"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.patchBody == nil {
		t.Fatal("expected a PATCH to reopen the bead")
	}
	if got := st.patchBody["status"]; got != "open" {
		t.Errorf("status = %v; want open", got)
	}
	fields, ok := st.patchBody["fields"].(map[string]any)
	if !ok {
		t.Fatalf("expected fields patch, got %T", st.patchBody["fields"])
	}
	// next_respawn_at and agent_state must be DELETED (JSON null), not blanked —
	// the anti-flap contract: the dormant bucket empties the instant we reopen.
	if v, present := fields["next_respawn_at"]; !present || v != nil {
		t.Errorf("next_respawn_at = %v (present=%v); want explicit null", v, present)
	}
	if v, present := fields["agent_state"]; !present || v != nil {
		t.Errorf("agent_state = %v (present=%v); want explicit null (dormant cleared)", v, present)
	}
	// respawn_count bumps 1 → 2.
	if got := fields["respawn_count"]; got != "2" {
		t.Errorf("respawn_count = %v; want \"2\"", got)
	}
	// wake_reason=manual is stamped so the controller surfaces BOAT_WAKE_REASON
	// to the revived pod (ka-0000000027).
	if got := fields["wake_reason"]; got != "manual" {
		t.Errorf("wake_reason = %v; want \"manual\"", got)
	}
	if !st.commented {
		t.Error("expected an audit comment on the woken bead")
	}
}

// TestRunWake_RejectsOpenBead refuses to wake a live (non-closed) agent — a
// wake only makes sense for a parked/dormant identity.
func TestRunWake_RejectsOpenBead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			t.Errorf("must NOT patch a live agent")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-live", "title": "live", "type": "agent", "status": "open",
			"labels": []string{"project:gardenio"},
			"fields": map[string]any{"agent": "x", "successor": "same", "agent_state": "working"},
		})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)

	err := runWakeCmd(t, "gd-live")
	if err == nil || !strings.Contains(err.Error(), "not parked") {
		t.Fatalf("expected 'not parked' rejection, got %v", err)
	}
}

// TestRunWake_RejectsNoWakeIntent refuses a closed agent that carries no
// successor=same wake intent — it was terminally closed, not parked.
func TestRunWake_RejectsNoWakeIntent(t *testing.T) {
	_, srv := newWakeServer(t, "gd-dead", map[string]any{
		"agent":       "gardenio-oneshot",
		"agent_state": "done",
		// no successor field
	})
	swapDaemon(t, srv.URL)

	err := runWakeCmd(t, "gd-dead")
	if err == nil || !strings.Contains(err.Error(), "no wake intent") {
		t.Fatalf("expected 'no wake intent' rejection, got %v", err)
	}
}

// TestRunWake_BudgetGate refuses to wake past the respawn budget without --force
// and allows it with --force.
func TestRunWake_BudgetGate(t *testing.T) {
	oldActor := actor
	actor = "tester"
	t.Cleanup(func() { actor = oldActor })

	fields := map[string]any{
		"agent":         "gardenio-spent",
		"successor":     "same",
		"agent_state":   "dormant",
		"respawn_count": "5", // == DefaultMaxRespawns
	}

	// Without --force → refused, no PATCH.
	st, srv := newWakeServer(t, "gd-spent", fields)
	swapDaemon(t, srv.URL)
	if err := runWakeCmd(t, "gd-spent"); err == nil || !strings.Contains(err.Error(), "respawn budget") {
		t.Fatalf("expected budget rejection, got %v", err)
	}
	if st.patchBody != nil {
		t.Error("must not PATCH when budget-gated without --force")
	}

	// With --force → revived.
	st2, srv2 := newWakeServer(t, "gd-spent", fields)
	swapDaemon(t, srv2.URL)
	if err := runWakeCmd(t, "gd-spent", "--force"); err != nil {
		t.Fatalf("--force should override the budget gate, got %v", err)
	}
	if st2.patchBody == nil {
		t.Fatal("expected PATCH under --force")
	}
}

// TestRunWake_RejectsNonAgentBead refuses a bead that is not an agent.
func TestRunWake_RejectsNonAgentBead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-task", "title": "a task", "type": "task", "status": "closed",
			"fields": map[string]any{},
		})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)

	err := runWakeCmd(t, "gd-task")
	if err == nil || !strings.Contains(err.Error(), "not an agent bead") {
		t.Fatalf("expected non-agent rejection, got %v", err)
	}
}
