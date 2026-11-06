package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
)

// monitorIdleCoopStub is a fake coop /api/v1/agent that always reports the same
// live state (the observed prod loop: coop keeps reporting the SAME live session
// while the controller is suspending/tearing down the pod).
type monitorIdleCoopStub struct {
	state string
}

func (s *monitorIdleCoopStub) server(t *testing.T) *coopapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent" {
			_ = json.NewEncoder(w).Encode(coopapi.AgentState{State: s.state})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return coopapi.New(srv.URL)
}

// monitorIdleDaemonStub serves GetBead (returning a configurable status +
// agent_state + stop_requested) and records every UpdateAgentState write
// monitorIdle attempts, so a test can assert whether the poller un-latched the
// bead.
type monitorIdleDaemonStub struct {
	mu           sync.Mutex
	status       string
	agentState   string
	stopReq      string
	stateWrites  []string // agent_state values monitorIdle wrote via UpdateAgentState
	getBeadCalls int
}

func (d *monitorIdleDaemonStub) client(t *testing.T) *beadsapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v1/beads/"):
			d.getBeadCalls++
			fields := map[string]any{"agent_state": d.agentState}
			if d.stopReq != "" {
				fields["stop_requested"] = d.stopReq
			}
			fieldsRaw, _ := json.Marshal(fields)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         "gd-agent1",
				"title":      "thread-agent",
				"type":       "agent",
				"status":     d.status,
				"fields":     json.RawMessage(fieldsRaw),
				"created_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
				"updated_at": time.Now().Format(time.RFC3339),
			})
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/v1/beads/"):
			// UpdateBeadFields write leg — PATCH /v1/beads/<id> with
			// {"fields": {"agent_state": "..."}}. Capture the agent_state it wrote.
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if s, ok := body.Fields["agent_state"].(string); ok {
				d.stateWrites = append(d.stateWrites, s)
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (d *monitorIdleDaemonStub) writes() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.stateWrites...)
}

// withFastMonitorIdle shrinks the settle/poll sleeps so a test exercises several
// poll ticks in milliseconds.
func withFastMonitorIdle(t *testing.T) {
	t.Helper()
	oldSettle, oldPoll := monitorIdleSettle, monitorIdlePoll
	monitorIdleSettle = 2 * time.Millisecond
	monitorIdlePoll = 2 * time.Millisecond
	t.Cleanup(func() {
		monitorIdleSettle, monitorIdlePoll = oldSettle, oldPoll
	})
}

// TestMonitorIdle_DoesNotUnlatchSuspendedAgent is the regression for
// ka-0000006721: a thread agent that called `gb stop` was SUSPENDED
// (agent_state=stopping, bead left open as the thaw target). coop still reports
// the live session as "idle". monitorIdle MUST NOT write that "idle" back over
// the "stopping" latch — doing so un-latches the stop and the podless
// in_progress+idle bead gets cold-restarted, replaying the original trigger text
// in an endless loop (30+ cycles / 12h observed on agent ka-0000007854).
func TestMonitorIdle_DoesNotUnlatchSuspendedAgent(t *testing.T) {
	withFastMonitorIdle(t)
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	dstub := &monitorIdleDaemonStub{status: "in_progress", agentState: "stopping"}
	oldDaemon := daemon
	daemon = dstub.client(t)
	t.Cleanup(func() { daemon = oldDaemon })

	coop := (&monitorIdleCoopStub{state: "idle"}).server(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	monitorIdle(ctx, coop)

	if got := dstub.writes(); len(got) != 0 {
		t.Fatalf("monitorIdle un-latched a suspended (agent_state=stopping) agent: wrote %v, want no UpdateAgentState writes", got)
	}
}

// TestMonitorIdle_DoesNotUnlatchStopRequested covers the terminal-stop shape: a
// clean `gb stop` set stop_requested=true (the entrypoint will close the bead
// shortly). monitorIdle must not resurrect the agent to "idle" in that window.
func TestMonitorIdle_DoesNotUnlatchStopRequested(t *testing.T) {
	withFastMonitorIdle(t)
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	dstub := &monitorIdleDaemonStub{status: "in_progress", agentState: "working", stopReq: "true"}
	oldDaemon := daemon
	daemon = dstub.client(t)
	t.Cleanup(func() { daemon = oldDaemon })

	coop := (&monitorIdleCoopStub{state: "idle"}).server(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	monitorIdle(ctx, coop)

	if got := dstub.writes(); len(got) != 0 {
		t.Fatalf("monitorIdle un-latched a stop_requested=true agent: wrote %v, want no UpdateAgentState writes", got)
	}
}

// TestMonitorIdle_DoesNotUnlatchClosedBead covers the closed-bead shape: the
// entrypoint already closed the bead to done. monitorIdle must not write live
// state onto a closed bead (which the reconciler treats as gone).
func TestMonitorIdle_DoesNotUnlatchClosedBead(t *testing.T) {
	withFastMonitorIdle(t)
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	dstub := &monitorIdleDaemonStub{status: "closed", agentState: "done"}
	oldDaemon := daemon
	daemon = dstub.client(t)
	t.Cleanup(func() { daemon = oldDaemon })

	coop := (&monitorIdleCoopStub{state: "working"}).server(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	monitorIdle(ctx, coop)

	if got := dstub.writes(); len(got) != 0 {
		t.Fatalf("monitorIdle wrote live state onto a closed bead: wrote %v, want none", got)
	}
}

// TestMonitorIdle_ReportsHealthyWorkingAgent proves the guard does NOT regress
// the normal path: a live, un-latched agent whose coop reports "working" must
// still have that state reported to the bead.
func TestMonitorIdle_ReportsHealthyWorkingAgent(t *testing.T) {
	withFastMonitorIdle(t)
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	dstub := &monitorIdleDaemonStub{status: "in_progress", agentState: "idle"}
	oldDaemon := daemon
	daemon = dstub.client(t)
	t.Cleanup(func() { daemon = oldDaemon })

	coop := (&monitorIdleCoopStub{state: "working"}).server(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	monitorIdle(ctx, coop)

	got := dstub.writes()
	if len(got) == 0 {
		t.Fatal("monitorIdle did not report live 'working' state for a healthy un-latched agent")
	}
	for _, s := range got {
		if s != "working" {
			t.Fatalf("monitorIdle wrote unexpected state %q, want only 'working'", s)
		}
	}
}

// TestAgentStopLatched_Table exercises the pure latch predicate directly.
func TestAgentStopLatched_Table(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agent1")
	cases := []struct {
		name       string
		status     string
		agentState string
		stopReq    string
		want       bool
	}{
		{"suspended-stopping", "in_progress", "stopping", "", true},
		{"terminal-done", "in_progress", "done", "", true},
		{"terminal-failed", "in_progress", "failed", "", true},
		{"closed-bead", "closed", "done", "", true},
		{"stop-requested", "in_progress", "working", "true", true},
		{"healthy-idle", "in_progress", "idle", "", false},
		{"healthy-working", "in_progress", "working", "", false},
		// Recoverable in-place parks are NOT latched — the in-session loop owns
		// them and flips back to idle on recovery.
		{"rate-limited-not-latched", "in_progress", "rate_limited", "", false},
		{"model-rate-limited-not-latched", "in_progress", "model_rate_limited", "", false},
		{"budget-exceeded-not-latched", "in_progress", "budget_exceeded", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dstub := &monitorIdleDaemonStub{status: tc.status, agentState: tc.agentState, stopReq: tc.stopReq}
			oldDaemon := daemon
			daemon = dstub.client(t)
			t.Cleanup(func() { daemon = oldDaemon })

			if got := agentStopLatched(context.Background(), "gd-agent1"); got != tc.want {
				t.Fatalf("agentStopLatched(status=%q,state=%q,stopReq=%q) = %v, want %v",
					tc.status, tc.agentState, tc.stopReq, got, tc.want)
			}
		})
	}
}

// TestAgentStopLatched_FailsOpenOnDaemonError proves a daemon read error is
// treated as NOT latched — a transient blip must never wedge live-state
// reporting for a healthy working agent.
func TestAgentStopLatched_FailsOpenOnDaemonError(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agent1")
	// A daemon whose server is already closed → GetBead errors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	srv.Close() // force a connection error
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	if agentStopLatched(context.Background(), "gd-agent1") {
		t.Fatal("agentStopLatched should fail open (return false) on a daemon read error")
	}
}
