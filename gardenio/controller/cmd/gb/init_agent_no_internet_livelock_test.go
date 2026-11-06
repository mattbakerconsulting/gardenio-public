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

	"gardenio/controller/internal/coopapi"
)

// noInternetLoopCoop models gd-fr3xjSxIws: a persistent failure (Codex 401 Missing
// bearer on every turn) surfaced as error_category=no_internet. Every re-inject
// drives a "working" flip and then lands back in the error, and request_count
// never advances because no inference turn ever succeeds.
type noInternetLoopCoop struct {
	mu             sync.Mutex
	requestCount   uint64 // bumped by advanceAfter, to model a genuine turn
	polls          int
	advanceAfter   int // poll number at which a genuine turn lands (0 = never)
	shutdownCalled bool
	inputs         int
}

func (c *noInternetLoopCoop) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/agent":
			c.polls++
			if c.advanceAfter > 0 && c.polls == c.advanceAfter {
				c.requestCount++
			}
			// Alternate working (the doomed re-inject's transient flip) and error.
			if c.polls%2 == 1 {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working", RequestCount: c.requestCount})
				return
			}
			json.NewEncoder(w).Encode(coopapi.AgentState{
				State:         "error",
				ErrorCategory: "no_internet",
				LastMessage:   "[codex:httpConnectionFailed] 401 Unauthorized: Missing bearer",
				RequestCount:  c.requestCount,
			})
		case "/api/v1/input":
			c.inputs++
			json.NewEncoder(w).Encode(map[string]int{"bytes_written": 12})
		case "/api/v1/shutdown":
			c.shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}
}

func withFastNoInternetBound(t *testing.T, bound int) {
	t.Helper()
	oldBound := noInternetLivelockBound
	noInternetLivelockBound = bound
	noInternetBackoffOverride = 1 * time.Millisecond
	monitorSleepOverride = 1 * time.Millisecond
	withFastRecovery(t)
	t.Cleanup(func() {
		noInternetLivelockBound = oldBound
		noInternetBackoffOverride = 0
		monitorSleepOverride = 0
		noInternetStreak = 0
	})
}

// TestMonitorExit_NoInternetLivelock_GivesUpAndRequeues: after
// noInternetLivelockBound no_internet recoveries with no genuine turn, monitorExit
// must requeue the task, close the agent bead failed, and shut coop down — even
// though every re-inject flips coop to "working" in between.
func TestMonitorExit_NoInternetLivelock_GivesUpAndRequeues(t *testing.T) {
	withFastNoInternetBound(t, 3)

	cs := &noInternetLoopCoop{}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()

	dm := &livelockDaemon{agentBeadID: "gd-agent-ni", taskID: "gd-TASK-ni", actor: "gardenio-crew-ni"}
	dsrv := httptest.NewServer(dm.handler(t))
	defer dsrv.Close()
	swapDaemon(t, dsrv.URL)
	withActor(t, "gardenio-crew-ni")
	t.Setenv("KD_AGENT_ID", "gd-agent-ni")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	coop := coopapi.New(csrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}

	cs.mu.Lock()
	shutdown := cs.shutdownCalled
	cs.mu.Unlock()
	if !shutdown {
		t.Error("coop.Shutdown was not called — the no_internet livelock bound did not give up")
	}
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if !dm.taskReopened {
		t.Error("claimed task was not requeued on give-up")
	}
	if !dm.agentClosed || dm.closedState != "failed" {
		t.Errorf("agent bead closed=%v state=%q, want closed failed", dm.agentClosed, dm.closedState)
	}
	if !strings.HasPrefix(dm.closeReason, "no_internet_livelock") {
		t.Errorf("close_reason %q lacks the no_internet_livelock prefix", dm.closeReason)
	}
}

// TestMonitorExit_NoInternetLivelock_GenuineTurnResetsStreak: a genuine turn
// (request_count advancing) zeroes the streak, so a real blip that recovers never
// trips the bound.
func TestMonitorExit_NoInternetLivelock_GenuineTurnResetsStreak(t *testing.T) {
	withFastNoInternetBound(t, 3)

	// Polls 2 and 4 are errors; a genuine turn lands at poll 5, so the streak
	// never reaches 3 before it is reset. Cancel soon after, before another
	// run of 3 errors could accumulate.
	cs := &noInternetLoopCoop{advanceAfter: 5}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	coop := coopapi.New(csrv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			cs.mu.Lock()
			p := cs.polls
			cs.mu.Unlock()
			if p >= 6 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.shutdownCalled {
		t.Error("gave up despite a genuine turn resetting the streak")
	}
}
