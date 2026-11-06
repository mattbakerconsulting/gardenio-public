package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/coopapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// livelockCoop models a rate-limit LIVELOCK the way it actually presents in
// production (ka-0000001209): after startup the agent 429s on an account
// monthly-spend cap and every recovery re-inject briefly flips coop to
// "working" (the doomed turn's round-trip) before landing straight back on the
// error screen. request_count NEVER advances because a 429 surfaces an
// isApiErrorMessage that carries no usage — which is exactly why the bound must
// key on request_count and not on the transient "working" state.
type livelockCoop struct {
	mu             sync.Mutex
	startupPolls   int    // number of leading /api/v1/agent polls that report "working"
	polls          int    // total /api/v1/agent polls seen
	inputs         int    // POST /api/v1/input count (recovery re-injects)
	flipPending    bool   // emit one transient "working" on the next agent poll
	category       string // error_category to report while wedged (rate_limited / model_rate_limited)
	startCount     uint64 // request_count coop reports before any genuine turn (a resumed coop may start above 0)
	requestCount   uint64 // advanced only when advanceOnInput fires; tests set it to startCount
	advanceOnInput int    // advance requestCount on the Nth input (0 = never; models a genuine turn)
	shutdownCalled bool
}

func (c *livelockCoop) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/agent":
			c.polls++
			if c.polls <= c.startupPolls {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working", RequestCount: c.requestCount})
				return
			}
			if c.requestCount > c.startCount {
				// A genuine turn has landed — the agent recovered and keeps
				// working (request_count already advanced past the park).
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working", RequestCount: c.requestCount})
				return
			}
			if c.flipPending {
				// The transient recovery flip: coop reports a brief "working"
				// while the doomed turn is in flight. confirmNudgeTurn catches
				// this; the bound must survive it.
				c.flipPending = false
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working", RequestCount: c.requestCount})
				return
			}
			json.NewEncoder(w).Encode(coopapi.AgentState{
				State:         "error",
				ErrorCategory: c.category,
				LastMessage:   "This request would exceed your account's monthly spend limit. Please try again later.",
				RequestCount:  c.requestCount,
			})
		case "/api/v1/input":
			c.inputs++
			if c.advanceOnInput > 0 && c.inputs == c.advanceOnInput {
				// A genuine successful turn landed: request_count advances. This
				// is the ONLY thing that must reset the livelock streak.
				c.requestCount++
			}
			c.flipPending = true
			json.NewEncoder(w).Encode(map[string]int{"bytes_written": 12})
		case "/api/v1/shutdown":
			c.shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			// /api/v1/input/keys, /api/v1/agent/nudge, etc.
			w.WriteHeader(http.StatusOK)
		}
	}
}

func (c *livelockCoop) shutdown() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shutdownCalled
}

// livelockDaemon captures the reopen (PATCH), the requeue audit comment, and the
// agent-bead close that failAgentRateLimitLivelock performs on give-up.
type livelockDaemon struct {
	mu             sync.Mutex
	agentBeadID    string
	taskID         string
	actor          string
	taskReopened   bool
	taskComment    string
	closeReason    string
	closedState    string
	agentClosed    bool
	progressStamps int // count of last_progress_at writes on the agent bead
}

func (d *livelockDaemon) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads") && r.URL.Query().Get("assignee") != "":
			// ListAssignedTask: the agent's claimed in_progress issue task.
			json.NewEncoder(w).Encode(map[string]any{
				"beads": []map[string]any{{
					"id":       d.taskID,
					"title":    "some claimed work",
					"kind":     "issue",
					"type":     "task",
					"status":   "in_progress",
					"assignee": d.actor,
				}},
				"total": 1,
			})
		case r.Method == "PATCH" && r.URL.Path == "/v1/beads/"+d.taskID:
			body, _ := io.ReadAll(r.Body)
			var patch map[string]any
			_ = json.Unmarshal(body, &patch)
			if patch["status"] == "open" {
				d.taskReopened = true
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == "PATCH" && r.URL.Path == "/v1/beads/"+d.agentBeadID:
			// UpdateAgentState(rate_limited / idle) on each park cycle — benign
			// agent_state pill updates the bound does not depend on. Also the
			// progress heartbeat (last_progress_at) on a genuine turn — count it
			// so the stamp test can assert it fired (ka-0000000286).
			body, _ := io.ReadAll(r.Body)
			var patch map[string]json.RawMessage
			_ = json.Unmarshal(body, &patch)
			var fields map[string]any
			_ = json.Unmarshal(patch["fields"], &fields)
			if _, ok := fields["last_progress_at"]; ok {
				d.progressStamps++
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+d.taskID+"/comments":
			body, _ := io.ReadAll(r.Body)
			var c map[string]string
			_ = json.Unmarshal(body, &c)
			d.taskComment = c["text"]
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+d.agentBeadID+"/close":
			body, _ := io.ReadAll(r.Body)
			var c map[string]any
			_ = json.Unmarshal(body, &c)
			if cr, ok := c["close_reason"].(string); ok {
				d.closeReason = cr
			}
			if s, ok := c["agent_state"].(string); ok {
				d.closedState = s
			}
			d.agentClosed = true
			json.NewEncoder(w).Encode(map[string]any{"id": d.agentBeadID})
		case r.Method == http.MethodGet:
			// Incidental reads the nudge-prompt resolver makes each recovery
			// (agent-bead GET, config-bead list) are benign here — the exact
			// prompt text does not affect the bound. Return empty so they no-op.
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		default:
			t.Errorf("unexpected daemon request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}
}

// withFastLivelockBound shrinks the give-up bound and the per-cycle waits so the
// livelock tests converge in well under a second, restoring them after.
func withFastLivelockBound(t *testing.T, bound int) {
	t.Helper()
	oldBound := rateLimitLivelockBound
	oldStreak := rateLimitNoHintStreak
	oldModelStreak := modelRateLimitStreak
	oldLivelockStreak := rateLimitLivelockStreak
	rateLimitLivelockBound = bound
	rateLimitLivelockStreak = 0
	rateLimitBackoffOverride = 1 * time.Millisecond
	modelRateLimitBackoffOverride = 1 * time.Millisecond
	monitorSleepOverride = 1 * time.Millisecond
	rateLimitNoHintStreak = 0
	modelRateLimitStreak = 0
	withFastRecovery(t)
	t.Cleanup(func() {
		rateLimitLivelockBound = oldBound
		rateLimitBackoffOverride = 0
		modelRateLimitBackoffOverride = 0
		monitorSleepOverride = 0
		rateLimitNoHintStreak = oldStreak
		modelRateLimitStreak = oldModelStreak
		rateLimitLivelockStreak = oldLivelockStreak
	})
}

// TestMonitorExit_RateLimitLivelock_GivesUpAndRequeues is the core regression
// for ka-0000001209: after rateLimitLivelockBound consecutive account
// rate-limit parks with no successful turn (request_count never advances), the
// harness must GIVE UP LOUDLY — requeue the claimed task, close the agent bead
// failed with a counted rate_limit_livelock reason, and shut coop down — rather
// than spin the probe-green livelock forever.
func TestMonitorExit_RateLimitLivelock_GivesUpAndRequeues(t *testing.T) {
	withFastLivelockBound(t, 4)

	cs := &livelockCoop{startupPolls: 1, category: "rate_limited"}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()

	dm := &livelockDaemon{agentBeadID: "gd-agent-livelock", taskID: "gd-TASK-livelock", actor: "gardenio-crew-livelock"}
	dsrv := httptest.NewServer(dm.handler(t))
	defer dsrv.Close()
	swapDaemon(t, dsrv.URL)
	withActor(t, "gardenio-crew-livelock")
	t.Setenv("KD_AGENT_ID", "gd-agent-livelock")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	coop := coopapi.New(csrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}

	if !cs.shutdown() {
		t.Error("coop.Shutdown was not called — the livelock bound did not give up")
	}
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if !dm.taskReopened {
		t.Error("claimed task was not requeued (status=open) on give-up")
	}
	if !dm.agentClosed {
		t.Error("agent bead was not closed on give-up")
	}
	if dm.closedState != "failed" {
		t.Errorf("agent bead closed with agent_state=%q, want failed", dm.closedState)
	}
	if !strings.HasPrefix(dm.closeReason, "rate_limit_livelock_involuntary") {
		t.Errorf("close_reason %q lacks the rate_limit_livelock_involuntary prefix", dm.closeReason)
	}
	// The close_reason names the last error instead of asserting a spend cap,
	// because a permanent error coop classes as a rate limit trips the same
	// bound (gd-a0jdFgwngn).
	if !strings.Contains(dm.closeReason, "last error: This request would exceed your account's monthly spend limit") {
		t.Errorf("close_reason %q does not name the last error", dm.closeReason)
	}
	if strings.Contains(dm.closeReason, "(account monthly-spend cap)") {
		t.Errorf("close_reason %q still asserts a spend cap", dm.closeReason)
	}
	if !strings.Contains(dm.closeReason, "reopened gd-TASK-livelock") {
		t.Errorf("close_reason %q does not record the reopened task", dm.closeReason)
	}
	// The give-up must be counted as an involuntary death in its own family.
	fam, counted := gardeniometrics.InvoluntaryDeathFamily(dm.closeReason)
	if !counted || fam != gardeniometrics.FamilyRateLimitLivelock {
		t.Errorf("close_reason %q classified as (%q,%v), want family=%q counted", dm.closeReason, fam, counted, gardeniometrics.FamilyRateLimitLivelock)
	}
}

// TestMonitorExit_ModelRateLimitLivelock_GivesUp proves the bound is shared with
// the model_rate_limited branch (a no-fallback pool throttle is just as
// unrecoverable by re-injection).
func TestMonitorExit_ModelRateLimitLivelock_GivesUp(t *testing.T) {
	withFastLivelockBound(t, 4)

	cs := &livelockCoop{startupPolls: 1, category: "model_rate_limited"}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()

	dm := &livelockDaemon{agentBeadID: "gd-agent-mll", taskID: "gd-TASK-mll", actor: "gardenio-crew-mll"}
	dsrv := httptest.NewServer(dm.handler(t))
	defer dsrv.Close()
	swapDaemon(t, dsrv.URL)
	withActor(t, "gardenio-crew-mll")
	t.Setenv("KD_AGENT_ID", "gd-agent-mll")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	coop := coopapi.New(csrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}
	if !cs.shutdown() {
		t.Error("coop.Shutdown was not called — the model-rate-limit livelock did not give up")
	}
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if !dm.agentClosed || dm.closedState != "failed" {
		t.Errorf("agent bead not failed-closed on model-rate-limit livelock (closed=%v state=%q)", dm.agentClosed, dm.closedState)
	}
	if !strings.Contains(dm.closeReason, "model_rate_limited") {
		t.Errorf("close_reason %q should name the model_rate_limited category", dm.closeReason)
	}
}

// TestMonitorExit_RateLimit_SuccessfulTurnResetsStreak proves the bound does NOT
// fire when a genuine turn lands in between parks: request_count advancing on
// the first recovery resets the livelock streak, so a pod that recovers is never
// falsely failed even after intermittent 429s.
func TestMonitorExit_RateLimit_SuccessfulTurnResetsStreak(t *testing.T) {
	withFastLivelockBound(t, 4)

	// One park, then the recovery's turn genuinely succeeds (request_count
	// advances on the 1st input) and coop stays working thereafter.
	cs := &livelockCoop{startupPolls: 1, category: "rate_limited", advanceOnInput: 1}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()

	dm := &livelockDaemon{agentBeadID: "gd-agent-ok", taskID: "gd-TASK-ok", actor: "gardenio-crew-ok"}
	dsrv := httptest.NewServer(dm.handler(t))
	defer dsrv.Close()
	swapDaemon(t, dsrv.URL)
	withActor(t, "gardenio-crew-ok")
	t.Setenv("KD_AGENT_ID", "gd-agent-ok")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	coop := coopapi.New(csrv.URL)
	// Let it run through the park+recovery, then cancel. If the bound were
	// keyed on state instead of request_count it would (wrongly) trip; it must
	// NOT here.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if dm.agentClosed {
		t.Errorf("agent bead was failed-closed despite a successful turn resetting the streak (close_reason=%q)", dm.closeReason)
	}
	if dm.taskReopened {
		t.Error("task was requeued despite the agent recovering — the streak reset did not hold")
	}
}

// TestMonitorExit_StampsProgressOnGenuineTurn is the heartbeat half of
// ka-0000000286: when a genuine, usage-bearing turn lands (coop's request_count
// advances), monitorExit must stamp last_progress_at on the agent bead so the
// single-claim guard and gb news can distinguish a working crew from a
// livelocked one. Reuses the "successful turn resets the streak" scenario (one
// park, then request_count advances on recovery) and asserts the stamp fired.
func TestMonitorExit_StampsProgressOnGenuineTurn(t *testing.T) {
	withFastLivelockBound(t, 4)

	cs := &livelockCoop{startupPolls: 1, category: "rate_limited", advanceOnInput: 1}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()

	dm := &livelockDaemon{agentBeadID: "gd-agent-stamp", taskID: "gd-TASK-stamp", actor: "gardenio-crew-stamp"}
	dsrv := httptest.NewServer(dm.handler(t))
	defer dsrv.Close()
	swapDaemon(t, dsrv.URL)
	withActor(t, "gardenio-crew-stamp")
	t.Setenv("KD_AGENT_ID", "gd-agent-stamp")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	coop := coopapi.New(csrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if dm.progressStamps < 1 {
		t.Errorf("expected at least one last_progress_at stamp after a genuine turn landed, got %d", dm.progressStamps)
	}
}

// TestMonitorExit_NoProgressStampDuringLivelock proves the stamp is NOT written
// while the agent is livelocked (request_count never advances): the wedged
// agent's last_progress_at stays frozen, which is exactly what lets the guards
// detect the stall. This is the negative control for the stamp.
func TestMonitorExit_NoProgressStampDuringLivelock(t *testing.T) {
	withFastLivelockBound(t, 4)

	// advanceOnInput default 0: no genuine turn ever lands.
	cs := &livelockCoop{startupPolls: 1, category: "rate_limited"}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()

	dm := &livelockDaemon{agentBeadID: "gd-agent-nostamp", taskID: "gd-TASK-nostamp", actor: "gardenio-crew-nostamp"}
	dsrv := httptest.NewServer(dm.handler(t))
	defer dsrv.Close()
	swapDaemon(t, dsrv.URL)
	withActor(t, "gardenio-crew-nostamp")
	t.Setenv("KD_AGENT_ID", "gd-agent-nostamp")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	coop := coopapi.New(csrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if dm.progressStamps != 0 {
		t.Errorf("expected NO progress stamp during a livelock (request_count never advances), got %d", dm.progressStamps)
	}
}

// TestTurnRecovered_ClassifiesHonestly is the honest-logging half of the fix
// (ka-0000001209): a re-inject that lands back in a rate-limit state must NOT be
// reported as recovered — that "agent working again" line is what kept the probe
// green through the livelock. "working" alone is not a recovery either: only a
// completed request (request_count past the baseline) is (gd-a0jdFgwngn).
func TestTurnRecovered_ClassifiesHonestly(t *testing.T) {
	withFastConfirmTurn(t)
	cases := []struct {
		name      string
		state     coopapi.AgentState
		recovered bool
		category  string
	}{
		{"working with a completed request", coopapi.AgentState{State: "working", RequestCount: 1}, true, ""},
		{"working, no completed request", coopapi.AgentState{State: "working"}, false, turnOutcomeInFlight},
		{"back to idle", coopapi.AgentState{State: "idle"}, true, ""},
		{"request landed, then re-429", coopapi.AgentState{State: "error", ErrorCategory: "rate_limited", RequestCount: 1}, true, ""},
		{"re-429 into error", coopapi.AgentState{State: "error", ErrorCategory: "rate_limited"}, false, "rate_limited"},
		{"model throttle", coopapi.AgentState{State: "error", ErrorCategory: "model_rate_limited"}, false, "model_rate_limited"},
		{"budget exceeded", coopapi.AgentState{State: "error", ErrorCategory: "budget_exceeded"}, false, "budget_exceeded"},
		{"bare error", coopapi.AgentState{State: "error"}, false, "error"},
		{"parked", coopapi.AgentState{State: "parked"}, false, "parked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(tc.state)
			}))
			defer srv.Close()
			coop := coopapi.New(srv.URL)
			got, cat := turnRecovered(context.Background(), coop, 0)
			if got != tc.recovered {
				t.Errorf("turnRecovered recovered=%v, want %v", got, tc.recovered)
			}
			if !tc.recovered && cat != tc.category {
				t.Errorf("turnRecovered category=%q, want %q", cat, tc.category)
			}
		})
	}
}

// TestTurnRecovered_WaitsOutTheDoomedFlip is the gd-a0jdFgwngn regression: the
// doomed retry reads "working" for its round-trip, then lands back on the rate
// limit. A single sample taken during the flip logged "agent working again";
// turnRecovered must wait for the outcome and report the rate limit.
func TestTurnRecovered_WaitsOutTheDoomedFlip(t *testing.T) {
	withFastConfirmTurn(t)
	var mu sync.Mutex
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		if n <= 2 {
			json.NewEncoder(w).Encode(coopapi.AgentState{State: "working", RequestCount: 7})
			return
		}
		json.NewEncoder(w).Encode(coopapi.AgentState{State: "error", ErrorCategory: "model_rate_limited", RequestCount: 7})
	}))
	defer srv.Close()

	got, cat := turnRecovered(context.Background(), coopapi.New(srv.URL), 7)
	if got {
		t.Fatal("turnRecovered reported a recovery for a retry that landed back on the rate limit")
	}
	if cat != "model_rate_limited" {
		t.Errorf("category=%q, want model_rate_limited", cat)
	}
}

// TestMonitorExit_RateLimitPark_DoesNotCountTowardNeverWorkedWatchdog is the
// gd-a0jdFgwngn regression for the watchdog: a park longer than the
// never-worked watchdog used to end in "FATAL: agent never started working"
// and a coop restart after one park, so the livelock bound never tripped. The
// park time is gb's own backoff, so the agent must keep parking until the
// livelock bound gives up and closes the bead.
func TestMonitorExit_RateLimitPark_DoesNotCountTowardNeverWorkedWatchdog(t *testing.T) {
	withFastLivelockBound(t, 3)
	oldWatchdog := zombieWatchdogTimeout
	zombieWatchdogTimeout = 200 * time.Millisecond
	modelRateLimitBackoffOverride = 300 * time.Millisecond
	t.Cleanup(func() { zombieWatchdogTimeout = oldWatchdog })

	// No startup "working" poll: the agent never works, as in the incident.
	cs := &livelockCoop{category: "model_rate_limited"}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()

	dm := &livelockDaemon{agentBeadID: "gd-agent-wd", taskID: "gd-TASK-wd", actor: "gardenio-crew-wd"}
	dsrv := httptest.NewServer(dm.handler(t))
	defer dsrv.Close()
	swapDaemon(t, dsrv.URL)
	withActor(t, "gardenio-crew-wd")
	t.Setenv("KD_AGENT_ID", "gd-agent-wd")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := monitorExit(ctx, coopapi.New(csrv.URL), nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}

	dm.mu.Lock()
	defer dm.mu.Unlock()
	if !dm.agentClosed || !strings.HasPrefix(dm.closeReason, "rate_limit_livelock_involuntary") {
		t.Fatalf("monitorExit ended without the livelock give-up (closed=%v close_reason=%q): the never-worked watchdog fired during a park", dm.agentClosed, dm.closeReason)
	}
	if !strings.Contains(dm.closeReason, "3 consecutive model_rate_limited parks") {
		t.Errorf("close_reason %q, want 3 parks counted in one life", dm.closeReason)
	}
}

// TestMonitorExit_RateLimitLivelockStreak_SurvivesRestart proves the streak is
// carried across coop restarts in the same pod (gd-a0jdFgwngn). A restarted
// coop may report a non-zero request_count from the start; that is a baseline,
// not a genuine turn, so it must not reset the carried streak.
func TestMonitorExit_RateLimitLivelockStreak_SurvivesRestart(t *testing.T) {
	withFastLivelockBound(t, 4)
	rateLimitLivelockStreak = 3 // three parks carried from the previous life

	cs := &livelockCoop{category: "model_rate_limited", startCount: 5, requestCount: 5}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()

	dm := &livelockDaemon{agentBeadID: "gd-agent-carry", taskID: "gd-TASK-carry", actor: "gardenio-crew-carry"}
	dsrv := httptest.NewServer(dm.handler(t))
	defer dsrv.Close()
	swapDaemon(t, dsrv.URL)
	withActor(t, "gardenio-crew-carry")
	t.Setenv("KD_AGENT_ID", "gd-agent-carry")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := monitorExit(ctx, coopapi.New(csrv.URL), nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}

	dm.mu.Lock()
	defer dm.mu.Unlock()
	if !strings.Contains(dm.closeReason, "4 consecutive model_rate_limited parks") {
		t.Errorf("close_reason %q, want the carried streak to trip the bound on this life's first park", dm.closeReason)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.inputs != 0 {
		t.Errorf("recovery re-injected %d times, want 0: the first park should have tripped the bound", cs.inputs)
	}
}

// TestMonitorExit_GenuineTurnResetsCarriedStreak is the negative control: a
// genuine turn in the new life still clears a carried streak.
func TestMonitorExit_GenuineTurnResetsCarriedStreak(t *testing.T) {
	withFastLivelockBound(t, 8)
	rateLimitLivelockStreak = 3

	cs := &livelockCoop{category: "rate_limited", startCount: 5, requestCount: 5, advanceOnInput: 1}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()

	dm := &livelockDaemon{agentBeadID: "gd-agent-reset", taskID: "gd-TASK-reset", actor: "gardenio-crew-reset"}
	dsrv := httptest.NewServer(dm.handler(t))
	defer dsrv.Close()
	swapDaemon(t, dsrv.URL)
	withActor(t, "gardenio-crew-reset")
	t.Setenv("KD_AGENT_ID", "gd-agent-reset")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := monitorExit(ctx, coopapi.New(csrv.URL), nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}
	if rateLimitLivelockStreak != 0 {
		t.Errorf("rateLimitLivelockStreak=%d after a genuine turn, want 0", rateLimitLivelockStreak)
	}
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if dm.agentClosed {
		t.Errorf("agent bead closed despite a genuine turn (close_reason=%q)", dm.closeReason)
	}
}

// TestMonitorExit_WorkingFlipDoesNotResetRateLimitBackoff proves a "working"
// state with no completed request leaves the rate-limit backoff streaks alone.
// A doomed retry's flip used to drop the backoff to base mid-livelock
// (gd-a0jdFgwngn); only request_count advancing resets them.
func TestMonitorExit_WorkingFlipDoesNotResetRateLimitBackoff(t *testing.T) {
	for _, tc := range []struct {
		name         string
		requestCount uint64
		wantStreak   int
	}{
		{"flip with no completed request", 0, 5},
		{"genuine turn", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFastLivelockBound(t, 8)
			rateLimitNoHintStreak = 5
			modelRateLimitStreak = 5
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/agent" {
					json.NewEncoder(w).Encode(coopapi.AgentState{State: "working", RequestCount: tc.requestCount})
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			t.Setenv("KD_AGENT_ID", "")

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := monitorExit(ctx, coopapi.New(srv.URL), nil, noRequestBaseline); err != nil {
				t.Fatalf("monitorExit returned error: %v", err)
			}
			if rateLimitNoHintStreak != tc.wantStreak || modelRateLimitStreak != tc.wantStreak {
				t.Errorf("streaks no-hint=%d model=%d, want %d", rateLimitNoHintStreak, modelRateLimitStreak, tc.wantStreak)
			}
		})
	}
}
