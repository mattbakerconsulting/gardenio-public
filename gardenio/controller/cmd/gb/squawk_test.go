package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestResolveThreadCoords_EnvWins verifies that when SLACK_THREAD_CHANNEL and
// SLACK_THREAD_TS are both set, they are returned verbatim without any daemon
// lookup (cold-spawned thread agents).
func TestResolveThreadCoords_EnvWins(t *testing.T) {
	t.Setenv("SLACK_THREAD_CHANNEL", "C_ENV")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	ch, ts := resolveThreadCoords(t.Context())
	if ch != "C_ENV" || ts != "111.222" {
		t.Fatalf("expected env coords (C_ENV, 111.222), got (%s, %s)", ch, ts)
	}
}

// TestResolveThreadCoords_BeadFallback verifies that when the env vars are
// empty, the coords are recovered from the agent bead's
// slack_thread_channel/slack_thread_ts fields. This is the prewarmed
// (pool-assigned) agent path: the pod's env is baked empty at create time and
// the controller writes the coords onto the bead at assign time. See bug
// ka-0000006843.
func TestResolveThreadCoords_BeadFallback(t *testing.T) {
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-agent1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/beads/gd-agent1" {
			t.Errorf("expected GET /v1/beads/gd-agent1, got %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-agent1",
			"type":   "agent",
			"status": "in_progress",
			"fields": map[string]string{
				"slack_thread_channel": "C_BEAD",
				"slack_thread_ts":      "333.444",
			},
		})
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	ch, ts := resolveThreadCoords(t.Context())
	if ch != "C_BEAD" || ts != "333.444" {
		t.Fatalf("expected bead-fallback coords (C_BEAD, 333.444), got (%s, %s)", ch, ts)
	}
}

// TestResolveThreadCoords_NoneAvailable verifies the empty-everything case
// degrades to ("", "") rather than panicking when there is no env and no
// agent bead id.
func TestResolveThreadCoords_NoneAvailable(t *testing.T) {
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")

	ch, ts := resolveThreadCoords(t.Context())
	if ch != "" || ts != "" {
		t.Fatalf("expected empty coords, got (%s, %s)", ch, ts)
	}
}
