package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
)

func TestIdleTTLMinutes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want float64
		ok   bool
	}{
		{"float", float64(90), 90, true},
		{"int", 45, 45, true},
		{"int64", int64(120), 120, true},
		{"string-rejected", "90", 0, false},
		{"nil", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := idleTTLMinutes(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("idleTTLMinutes(%v) = (%v,%v), want (%v,%v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestResolveIdleTTL_ThreadDefaultsOn(t *testing.T) {
	// No config beads: thread agents default ON, others default OFF.
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "1780000000.1")

	oldDaemon := daemon
	daemon = nil // force the default path (no daemon)
	t.Cleanup(func() { daemon = oldDaemon })

	cfg := resolveIdleTTL(context.Background())
	if !cfg.enabled {
		t.Error("expected thread agent to default enabled")
	}
	if cfg.ttl != idleTTLDefault {
		t.Errorf("expected default ttl %s, got %s", idleTTLDefault, cfg.ttl)
	}
}

func TestResolveIdleTTL_NonThreadDefaultsOff(t *testing.T) {
	// Ensure no thread env leaks in from the environment.
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")

	oldDaemon := daemon
	daemon = nil
	t.Cleanup(func() { daemon = oldDaemon })

	cfg := resolveIdleTTL(context.Background())
	if cfg.enabled {
		t.Error("expected non-thread agent to default disabled")
	}
}

func TestResolveIdleTTL_ConfigBeadOverrides(t *testing.T) {
	// A project-scoped idle-ttl config bead opts a non-thread agent in and
	// overrides the TTL.
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "crew")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ResolveConfigBeads lists open config beads.
		resp := map[string]any{
			"beads": []map[string]any{
				{
					"id":          "gd-cfg1",
					"title":       "idle-ttl",
					"labels":      []string{"project:gardenio"},
					"description": `{"enabled":true,"ttl_minutes":30}`,
					"created_at":  time.Now().Format(time.RFC3339),
				},
			},
			"total": 1,
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	cfg := resolveIdleTTL(context.Background())
	if !cfg.enabled {
		t.Error("expected config bead to enable idle-ttl for non-thread agent")
	}
	if cfg.ttl != 30*time.Minute {
		t.Errorf("expected ttl 30m from config, got %s", cfg.ttl)
	}
	// This config bead omits persistent, so the agent keeps today's terminal
	// idle despawn.
	if cfg.persistent {
		t.Error("expected persistent to default false when config omits it")
	}
}

// TestResolveIdleTTL_PersistentConfigOptsIn verifies the persistence opt-in:
// a "persistent": true idle-ttl config bead flips the agent to park-instead-of-
// close and "respawn_after_minutes" tunes the wake delay.
func TestResolveIdleTTL_PersistentConfigOptsIn(t *testing.T) {
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "crew")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"beads": []map[string]any{
				{
					"id":          "gd-cfg2",
					"title":       "idle-ttl",
					"labels":      []string{"project:gardenio"},
					"description": `{"enabled":true,"ttl_minutes":120,"persistent":true,"respawn_after_minutes":45}`,
					"created_at":  time.Now().Format(time.RFC3339),
				},
			},
			"total": 1,
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	cfg := resolveIdleTTL(context.Background())
	if !cfg.enabled {
		t.Error("expected persistent config bead to enable idle-ttl")
	}
	if !cfg.persistent {
		t.Error("expected persistent=true from config bead")
	}
	if cfg.respawnAfter != 45*time.Minute {
		t.Errorf("expected respawnAfter 45m from config, got %s", cfg.respawnAfter)
	}
}

// idleTTLCoopStub records coop calls and serves a scripted sequence of states.
type idleTTLCoopStub struct {
	mu        sync.Mutex
	states    []string // consumed front-to-back; last value repeats
	calls     int
	shutdowns int
}

func (s *idleTTLCoopStub) nextState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i >= len(s.states) {
		i = len(s.states) - 1
	}
	return s.states[i]
}

func (s *idleTTLCoopStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			json.NewEncoder(w).Encode(coopapi.AgentState{State: s.nextState()})
		case "/api/v1/shutdown":
			s.mu.Lock()
			s.shutdowns++
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

// daemonCloseRecorder serves the minimal beads endpoints monitorIdleTTL needs
// and records whether the bead was closed and with what fields.
type daemonCloseRecorder struct {
	mu               sync.Mutex
	status           string // status returned by GetBead
	stopRequested    bool   // stop_requested field returned by GetBead
	closed           bool
	closeReason      string
	successor        string // successor field written on close (park path)
	nextRespawnAt    string // next_respawn_at field written on close (park path)
	closeAgentState  string // agent_state field written on close (dormant on park)
	stopReqWritten   bool
	agentStateWrites []string // agent_state values written via UpdateBeadFields (freeze path)
}

func (d *daemonCloseRecorder) server(t *testing.T) *beadsapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case r.Method == http.MethodGet:
			// GetBead
			fields := map[string]any{}
			if d.stopRequested {
				fields["stop_requested"] = "true"
			}
			fieldsRaw, _ := json.Marshal(fields)
			json.NewEncoder(w).Encode(map[string]any{
				"id":         "gd-agent1",
				"title":      "thread-agent",
				"type":       "agent",
				"status":     d.status,
				"fields":     json.RawMessage(fieldsRaw),
				"created_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
				"updated_at": time.Now().Format(time.RFC3339),
			})
		case r.Method == http.MethodPost && hasSuffix(r.URL.Path, "/close"):
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			d.closed = true
			if cr, ok := body["close_reason"].(string); ok {
				d.closeReason = cr
			}
			if s, ok := body["successor"].(string); ok {
				d.successor = s
			}
			if n, ok := body["next_respawn_at"].(string); ok {
				d.nextRespawnAt = n
			}
			if s, ok := body["agent_state"].(string); ok {
				d.closeAgentState = s
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost || r.Method == http.MethodPatch:
			// UpdateBeadFields (GET happens above; this is the write leg). The
			// client first GETs then writes — record that a write occurred and
			// capture any agent_state written (the freeze path transitions to
			// stopping via UpdateAgentState → UpdateBeadFields).
			d.stopReqWritten = true
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) == nil {
				if f, ok := body["fields"].(map[string]any); ok {
					if s, ok := f["agent_state"].(string); ok {
						d.agentStateWrites = append(d.agentStateWrites, s)
					}
				}
				if s, ok := body["agent_state"].(string); ok {
					d.agentStateWrites = append(d.agentStateWrites, s)
				}
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

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func TestMonitorIdleTTL_DespawnsAfterWindow(t *testing.T) {
	idleTTLPollOverride = 5 * time.Millisecond
	t.Cleanup(func() { idleTTLPollOverride = 0 })
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	rec := &daemonCloseRecorder{status: "open"}
	oldDaemon := daemon
	daemon = rec.server(t)
	t.Cleanup(func() { daemon = oldDaemon })

	stub := &idleTTLCoopStub{states: []string{"idle"}}
	srv := stub.server()
	defer srv.Close()
	coop := coopapi.New(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	// TTL shorter than a couple poll cycles → must despawn.
	monitorIdleTTL(ctx, coop, idleTTLConfig{enabled: true, ttl: 8 * time.Millisecond})

	stub.mu.Lock()
	shutdowns := stub.shutdowns
	stub.mu.Unlock()
	if shutdowns == 0 { // DESPAWN_MARK
		t.Error("expected coop shutdown after idle TTL expired")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.closed {
		t.Error("expected agent bead to be closed on idle-TTL despawn")
	}
	if rec.closeReason != "idle_ttl_expired" {
		t.Errorf("expected close_reason=idle_ttl_expired, got %q", rec.closeReason)
	}
	// Non-persistent (default): a plain terminal close — no successor=same intent
	// and no wake metadata stamped.
	if rec.successor != "" {
		t.Errorf("non-persistent despawn must not stamp successor, got %q", rec.successor)
	}
	if rec.nextRespawnAt != "" {
		t.Errorf("non-persistent despawn must not stamp next_respawn_at, got %q", rec.nextRespawnAt)
	}
	// A terminal despawn closes agent_state=done, never dormant — dormant is the
	// engine-set PARK signal reserved for persistent identities (ka-0000004675).
	if rec.closeAgentState == beadsapi.AgentStateDormant {
		t.Errorf("non-persistent despawn must NOT stamp agent_state=dormant, got %q", rec.closeAgentState)
	}
}

// TestMonitorIdleTTL_FreezesEligibleAgent verifies the (f) idle-freeze path
// (Δ4/A1, ka-0000000860): a freeze-eligible agent (BOAT_FREEZE_ELIGIBLE=true) that
// idles out is SUSPENDED — transitioned to agent_state=stopping without closing
// the bead and without shutting coop down — so the controller can checkpoint the
// live sandbox and a later spawn thaws it. The bead must stay OPEN.
func TestMonitorIdleTTL_FreezesEligibleAgent(t *testing.T) {
	idleTTLPollOverride = 5 * time.Millisecond
	t.Cleanup(func() { idleTTLPollOverride = 0 })
	t.Setenv("KD_AGENT_ID", "gd-agent1")
	t.Setenv("BOAT_FREEZE_ELIGIBLE", "true")

	rec := &daemonCloseRecorder{status: "open"}
	oldDaemon := daemon
	daemon = rec.server(t)
	t.Cleanup(func() { daemon = oldDaemon })

	stub := &idleTTLCoopStub{states: []string{"idle"}}
	srv := stub.server()
	defer srv.Close()
	coop := coopapi.New(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	monitorIdleTTL(ctx, coop, idleTTLConfig{enabled: true, ttl: 8 * time.Millisecond})

	rec.mu.Lock()
	defer rec.mu.Unlock()

	// Freeze path: the bead is NOT closed — it stays open at agent_state=stopping
	// as the name-keyed thaw target.
	if rec.closed {
		t.Error("freeze-eligible idle agent must NOT close its bead (it stays open for thaw)")
	}
	// It transitioned to stopping (the sole AgentStop freeze trigger).
	sawStopping := false
	for _, s := range rec.agentStateWrites {
		if s == "stopping" {
			sawStopping = true
		}
	}
	if !sawStopping {
		t.Errorf("expected agent_state=stopping write (idle-freeze), got writes=%v", rec.agentStateWrites)
	}

	// coop must NOT be shut down: the checkpoint captures the LIVE process; the
	// controller tears down Pod A after `runsc checkpoint --leave-running`.
	stub.mu.Lock()
	shutdowns := stub.shutdowns
	stub.mu.Unlock()
	if shutdowns != 0 {
		t.Errorf("freeze-eligible idle agent must NOT request coop shutdown, got %d", shutdowns)
	}
}

// TestMonitorIdleTTL_ParksPersistentAgent verifies the (e) park-instead-of-close
// path: a persistent agent that idles out has its bead closed WITH successor=same
// + a next_respawn_at wake timestamp so the future wake scanner (ka-0000004902)
// can recreate the identity, and a distinct idle_ttl_parked close_reason.
func TestMonitorIdleTTL_ParksPersistentAgent(t *testing.T) {
	idleTTLPollOverride = 5 * time.Millisecond
	t.Cleanup(func() { idleTTLPollOverride = 0 })
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	rec := &daemonCloseRecorder{status: "open"}
	oldDaemon := daemon
	daemon = rec.server(t)
	t.Cleanup(func() { daemon = oldDaemon })

	stub := &idleTTLCoopStub{states: []string{"idle"}}
	srv := stub.server()
	defer srv.Close()
	coop := coopapi.New(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	before := time.Now()
	monitorIdleTTL(ctx, coop, idleTTLConfig{
		enabled:      true,
		ttl:          8 * time.Millisecond,
		persistent:   true,
		respawnAfter: 20 * time.Minute,
	})

	stub.mu.Lock()
	shutdowns := stub.shutdowns
	stub.mu.Unlock()
	if shutdowns == 0 {
		t.Error("expected coop shutdown after idle TTL expired (park still tears the pod down)")
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.closed {
		t.Fatal("expected agent bead to be closed on idle-TTL park")
	}
	if rec.closeReason != "idle_ttl_parked" {
		t.Errorf("expected close_reason=idle_ttl_parked, got %q", rec.closeReason)
	}
	if rec.successor != SuccessorSame {
		t.Errorf("expected successor=%q on park, got %q", SuccessorSame, rec.successor)
	}
	// The engine-set dormant bucket signal (task ka-0000004675): a persistent
	// park closes the bead as agent_state=dormant (NOT "done"), so the viewer
	// buckets it as parked off this one field rather than deriving dormancy.
	if rec.closeAgentState != beadsapi.AgentStateDormant {
		t.Errorf("expected agent_state=%q on persistent park, got %q", beadsapi.AgentStateDormant, rec.closeAgentState)
	}
	// next_respawn_at must be a parseable RFC3339 timestamp ~20m in the future.
	if rec.nextRespawnAt == "" {
		t.Fatal("expected next_respawn_at wake metadata on park, got empty")
	}
	nextAt, err := time.Parse(time.RFC3339, rec.nextRespawnAt)
	if err != nil {
		t.Fatalf("next_respawn_at %q is not RFC3339: %v", rec.nextRespawnAt, err)
	}
	if delta := nextAt.Sub(before); delta < 19*time.Minute || delta > 21*time.Minute {
		t.Errorf("next_respawn_at should be ~20m out, got %s from start", delta)
	}
}

// TestMonitorIdleTTL_ParkDefaultRespawnDelay verifies a persistent agent that
// omits respawn_after_minutes falls back to idleTTLParkRespawnDefault.
func TestMonitorIdleTTL_ParkDefaultRespawnDelay(t *testing.T) {
	idleTTLPollOverride = 5 * time.Millisecond
	t.Cleanup(func() { idleTTLPollOverride = 0 })
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	rec := &daemonCloseRecorder{status: "open"}
	oldDaemon := daemon
	daemon = rec.server(t)
	t.Cleanup(func() { daemon = oldDaemon })

	stub := &idleTTLCoopStub{states: []string{"idle"}}
	srv := stub.server()
	defer srv.Close()
	coop := coopapi.New(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	before := time.Now()
	monitorIdleTTL(ctx, coop, idleTTLConfig{enabled: true, ttl: 8 * time.Millisecond, persistent: true})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.successor != SuccessorSame {
		t.Errorf("expected successor=%q, got %q", SuccessorSame, rec.successor)
	}
	nextAt, err := time.Parse(time.RFC3339, rec.nextRespawnAt)
	if err != nil {
		t.Fatalf("next_respawn_at %q is not RFC3339: %v", rec.nextRespawnAt, err)
	}
	if delta := nextAt.Sub(before); delta < idleTTLParkRespawnDefault-time.Minute ||
		delta > idleTTLParkRespawnDefault+time.Minute {
		t.Errorf("next_respawn_at should default to ~%s out, got %s", idleTTLParkRespawnDefault, delta)
	}
}

func TestMonitorIdleTTL_WorkingResetsTimer(t *testing.T) {
	// Agent flips working→idle repeatedly; total idle never reaches the TTL in
	// one continuous stretch, so it must NOT despawn before context cancels.
	idleTTLPollOverride = 5 * time.Millisecond
	t.Cleanup(func() { idleTTLPollOverride = 0 })
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	rec := &daemonCloseRecorder{status: "open"}
	oldDaemon := daemon
	daemon = rec.server(t)
	t.Cleanup(func() { daemon = oldDaemon })

	// idle, idle, working (reset!), idle, idle, working (reset! and the trailing
	// "working" repeats — models follow-ups continuing to arrive). Between each
	// pair of "working" turns there are only 2 idle polls (~10ms) < the 14ms TTL,
	// so the timer never trips.
	stub := &idleTTLCoopStub{states: []string{"idle", "idle", "working", "idle", "idle", "working"}}
	srv := stub.server()
	defer srv.Close()
	coop := coopapi.New(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	// TTL spans 3 polls; the working states (every 3rd) reset before it trips.
	monitorIdleTTL(ctx, coop, idleTTLConfig{enabled: true, ttl: 14 * time.Millisecond})

	stub.mu.Lock()
	shutdowns := stub.shutdowns
	stub.mu.Unlock()
	if shutdowns != 0 {
		t.Errorf("expected no despawn while turns keep resetting the timer, got %d shutdowns", shutdowns)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.closed {
		t.Error("agent bead should not be closed while still active")
	}
}

func TestMonitorIdleTTL_ExitedReturnsCleanly(t *testing.T) {
	idleTTLPollOverride = 5 * time.Millisecond
	t.Cleanup(func() { idleTTLPollOverride = 0 })
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	rec := &daemonCloseRecorder{status: "open"}
	oldDaemon := daemon
	daemon = rec.server(t)
	t.Cleanup(func() { daemon = oldDaemon })

	stub := &idleTTLCoopStub{states: []string{"exited"}}
	srv := stub.server()
	defer srv.Close()
	coop := coopapi.New(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { monitorIdleTTL(ctx, coop, idleTTLConfig{enabled: true, ttl: time.Hour}); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("monitorIdleTTL did not return after coop reported exited")
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.closed {
		t.Error("must not close bead when the session exited on its own")
	}
}

func TestMonitorIdleTTL_SkipsWhenBeadAlreadyClosing(t *testing.T) {
	// (d) Coexist with reaper/drain/gb stop: if the bead is already closed, the
	// idle-TTL despawn must NOT re-close it (no double-despawn).
	idleTTLPollOverride = 5 * time.Millisecond
	t.Cleanup(func() { idleTTLPollOverride = 0 })
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	rec := &daemonCloseRecorder{status: "closed"}
	oldDaemon := daemon
	daemon = rec.server(t)
	t.Cleanup(func() { daemon = oldDaemon })

	stub := &idleTTLCoopStub{states: []string{"idle"}}
	srv := stub.server()
	defer srv.Close()
	coop := coopapi.New(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	monitorIdleTTL(ctx, coop, idleTTLConfig{enabled: true, ttl: 8 * time.Millisecond})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.closed {
		t.Error("must not re-close an already-closed bead (double-despawn guard)")
	}
}

func TestMonitorIdleTTL_ZeroTTLNoop(t *testing.T) {
	// A non-positive TTL disables the watcher entirely.
	stub := &idleTTLCoopStub{states: []string{"idle"}}
	srv := stub.server()
	defer srv.Close()
	coop := coopapi.New(srv.URL)

	monitorIdleTTL(context.Background(), coop, idleTTLConfig{enabled: true, ttl: 0})

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.calls != 0 || stub.shutdowns != 0 {
		t.Errorf("zero TTL should be a no-op, got calls=%d shutdowns=%d", stub.calls, stub.shutdowns)
	}
}
