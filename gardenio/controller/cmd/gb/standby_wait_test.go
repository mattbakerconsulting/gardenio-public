package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
)

// In-VM standby parking over Ziti (plan ka-0000004145, task gd-PBHP0WDLyW).
//
// standbyWait is the loop a prewarmed pool agent runs (via `gb init --standby`)
// while parked: it polls its OWN agent bead over the daemon client — which, in a
// MicroVM, resolves to the local Ziti proxy listener (BEADS_HTTP_ADDR
// 127.0.0.1:18080) — until the pool manager flips agent_state from "prewarmed"
// to "assigning", then hydrates the assignment env and returns so Claude starts.
// These tests pin that GetBead-driven behavior against an httptest daemon
// standing in for the in-VM proxy target.

// standbyBeadStub serves GET /v1/beads/<id> with a scripted agent_state
// sequence (last value repeats) plus assignment fields, recording every
// GetBead call so a test can assert the loop actually polled.
type standbyBeadStub struct {
	mu     sync.Mutex
	states []string          // consumed front-to-back; last value repeats
	fields map[string]string // extra fields returned once state != prewarmed
	calls  atomic.Int32
}

func (s *standbyBeadStub) handler(beadID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/v1/beads/"+beadID) {
			http.NotFound(w, r)
			return
		}
		n := s.calls.Add(1)

		s.mu.Lock()
		idx := int(n) - 1
		if idx >= len(s.states) {
			idx = len(s.states) - 1
		}
		state := s.states[idx]
		fields := map[string]string{"agent_state": state}
		if state != "prewarmed" {
			maps.Copy(fields, s.fields)
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     beadID,
			"title":  beadID,
			"type":   "agent",
			"status": "open",
			"fields": fields,
		})
	})
}

// setStandbyDaemon points the package-global daemon at a stub server standing in
// for the in-VM local Ziti proxy target, and restores it on cleanup.
func setStandbyDaemon(t *testing.T, stub *standbyBeadStub, beadID string) {
	t.Helper()
	srv := httptest.NewServer(stub.handler(beadID))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

// redirectStandbySentinels points the assignment sentinel at a temp path and
// chdir's into a temp dir so the loop's fixed "/tmp/standby_active" writes and
// removes land somewhere disposable (best-effort — the loop uses absolute /tmp
// for the active sentinel, so this at least isolates the assigned sentinel).
func redirectStandbySentinels(t *testing.T) {
	t.Helper()
	oldSentinel := standbyAssignedSentinelPath
	standbyAssignedSentinelPath = filepath.Join(t.TempDir(), "standby_assigned")
	t.Cleanup(func() { standbyAssignedSentinelPath = oldSentinel })
}

// TestStandbyWait_ExitsOnAssignment is the core of the in-VM standby contract:
// the loop polls its own bead over the daemon (the local Ziti proxy in a
// MicroVM) and returns as soon as agent_state flips off "prewarmed", writing the
// assigned sentinel so gb prime stops handing the agent the standby card.
func TestStandbyWait_ExitsOnAssignment(t *testing.T) {
	redirectStandbySentinels(t)

	const beadID = "gd-STBY1"
	stub := &standbyBeadStub{
		// prewarmed for the first two polls, then assigning — proves the loop
		// keeps polling and only exits on the flip.
		states: []string{"prewarmed", "prewarmed", "assigning"},
		fields: map[string]string{
			"slack_thread_channel": "C0THREAD",
			"slack_thread_ts":      "1784500000.111111",
			"task_id":              "gd-TASK9",
		},
	}
	setStandbyDaemon(t, stub, beadID)

	t.Setenv("BOAT_AGENT_BEAD_ID", beadID)
	// Sub-second poll expressed as a Go duration string — the exact shape the
	// controller writes (cfg.StandbyPoll.String()). Proves envDuration reads it
	// as 20ms, not 20s (regression guard for ka-0000000001).
	t.Setenv("BOAT_STANDBY_POLL", "20ms")
	t.Setenv("BOAT_STANDBY_MAX_TTL", "60s")
	// Clear anything the hydrate step will set so we can assert it populated them.
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("BOAT_TASK_ID", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := standbyWait(ctx, coopapi.New("http://127.0.0.1:0"))
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("standbyWait returned error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("standbyWait did not return after assignment within timeout")
	}

	// It polled its own bead more than once (kept polling while prewarmed).
	if got := stub.calls.Load(); got < 3 {
		t.Errorf("GetBead call count = %d, want >= 3 (polled while prewarmed, then saw assigning)", got)
	}

	// It hydrated the assignment env from the bead fields.
	for k, want := range map[string]string{
		"SLACK_THREAD_CHANNEL": "C0THREAD",
		"SLACK_THREAD_TS":      "1784500000.111111",
		"BOAT_TASK_ID":         "gd-TASK9",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("after assignment, env %s = %q, want %q (hydrateAssignmentEnv)", k, got, want)
		}
	}

	// It wrote the assigned sentinel so gb prime serves the real workflow card.
	if _, err := os.Stat(standbyAssignedSentinelPath); err != nil {
		t.Errorf("assigned sentinel not written: %v", err)
	}
}

// TestStandbyWait_TTLExpiry proves the safety-net TTL fires: with the bead stuck
// at "prewarmed" and a tiny TTL, the loop shuts the session down and returns an
// error rather than parking forever.
func TestStandbyWait_TTLExpiry(t *testing.T) {
	redirectStandbySentinels(t)

	const beadID = "gd-STBY2"
	stub := &standbyBeadStub{states: []string{"prewarmed"}} // never assigned
	setStandbyDaemon(t, stub, beadID)

	t.Setenv("BOAT_AGENT_BEAD_ID", beadID)
	t.Setenv("BOAT_STANDBY_POLL", "10ms")
	t.Setenv("BOAT_STANDBY_MAX_TTL", "30ms") // expires almost immediately

	// Record whether coop.Shutdown was invoked on TTL expiry.
	var shutdownCalled atomic.Int32
	shutSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "shutdown") {
			shutdownCalled.Store(1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer shutSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := standbyWait(ctx, coopapi.New(shutSrv.URL))
	if err == nil {
		t.Fatal("standbyWait returned nil on TTL expiry, want an error")
	}
	if !strings.Contains(err.Error(), "TTL") {
		t.Errorf("error = %v, want a TTL-exceeded error", err)
	}
	if shutdownCalled.Load() != 1 {
		t.Error("coop.Shutdown was not called on TTL expiry")
	}
}

// TestEnvDuration_GoDurationString is the regression guard for ka-0000000001:
// envDuration must read a Go duration string (the shape the controller writes
// for BOAT_STANDBY_POLL via cfg.StandbyPoll.String()) as that duration — NOT as
// integer seconds. The old fmt.Sscanf("%d") parser read "500ms" as 500s,
// silently defeating the sub-second warm-pool poll (ka-0000003812).
func TestEnvDuration_GoDurationString(t *testing.T) {
	cases := map[string]time.Duration{
		"500ms":  500 * time.Millisecond,
		"250ms":  250 * time.Millisecond,
		"5s":     5 * time.Second,
		"1m30s":  90 * time.Second,
		"24h":    24 * time.Hour,
		"1500ms": 1500 * time.Millisecond,
	}
	for in, want := range cases {
		t.Setenv("TEST_STANDBY_DUR", in)
		if got := envDuration("TEST_STANDBY_DUR", 5*time.Second); got != want {
			t.Errorf("envDuration(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestEnvDuration_BareIntStillSeconds keeps the historical contract: a bare
// integer with no unit is interpreted as seconds (back-compat for any operator
// override or caller that still sets a plain count).
func TestEnvDuration_BareIntStillSeconds(t *testing.T) {
	t.Setenv("TEST_STANDBY_DUR", "30")
	if got := envDuration("TEST_STANDBY_DUR", 5*time.Second); got != 30*time.Second {
		t.Errorf("envDuration(\"30\") = %v, want 30s (bare int = seconds)", got)
	}
}
