package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/coopapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// terminalAuthCoop models the way a non-recoverable credential failure actually
// presents (ka-0000001098): after an optional working-startup window, coop leaves
// the session in "error" with error_category=unauthorized (or out_of_credits)
// and marks it zombie — but does NOT park, so there is no ParkedReason to key on.
// request_count never advances because no inference turn ever succeeds. Left
// unhandled the pod waits for the K8s /livez probe to SIGTERM it, which the
// generic teardown then mislabels as a node-drain/Spot-reclaim context_cancelled.
type terminalAuthCoop struct {
	category       string // "unauthorized" or "out_of_credits"
	startupPolls   int    // leading /api/v1/agent polls reported "working"
	polls          int
	shutdownCalled bool
}

func (c *terminalAuthCoop) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent":
			c.polls++
			if c.polls <= c.startupPolls {
				json.NewEncoder(w).Encode(coopapi.AgentState{State: "working"})
				return
			}
			json.NewEncoder(w).Encode(coopapi.AgentState{
				State:         "error",
				ErrorCategory: c.category,
				LastMessage:   "API Error: 401 {\"error\":{\"message\":\"authentication_failed\"}}",
			})
		case "/api/v1/shutdown":
			c.shutdownCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}
}

// withFastTerminalAuthBound shrinks the give-up bound and the poll cadence so the
// terminal-auth tests converge in well under a second, restoring them after.
func withFastTerminalAuthBound(t *testing.T, bound int) {
	t.Helper()
	oldBound := terminalAuthFailureBound
	terminalAuthFailureBound = bound
	monitorSleepOverride = 1 * time.Millisecond
	t.Cleanup(func() {
		terminalAuthFailureBound = oldBound
		monitorSleepOverride = 0
	})
}

// TestMonitorExit_TerminalAuthFailure_GivesUpAndRequeues is the core regression
// for ka-0000001098: after terminalAuthFailureBound consecutive polls at
// error_category=unauthorized with no successful turn, monitorExit must GIVE UP
// LOUDLY — requeue the claimed task, close the agent bead failed carrying the
// honest error_category, and shut coop down — instead of zombying until the
// liveness probe SIGTERMs it into a phantom context_cancelled "Spot reclaim".
func TestMonitorExit_TerminalAuthFailure_GivesUpAndRequeues(t *testing.T) {
	withFastTerminalAuthBound(t, 3)

	cs := &terminalAuthCoop{category: "unauthorized"}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()

	// Reuse the livelock daemon mock: it captures the reopen PATCH, the requeue
	// comment, and the agent-bead close — exactly what this give-up performs.
	dm := &livelockDaemon{agentBeadID: "gd-agent-auth", taskID: "gd-TASK-auth", actor: "gardenio-crew-auth"}
	dsrv := httptest.NewServer(dm.handler(t))
	defer dsrv.Close()
	swapDaemon(t, dsrv.URL)
	withActor(t, "gardenio-crew-auth")
	t.Setenv("KD_AGENT_ID", "gd-agent-auth")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	coop := coopapi.New(csrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}

	if !cs.shutdownCalled {
		t.Error("coop.Shutdown was not called — the terminal-auth bound did not give up")
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
	if !strings.HasPrefix(dm.closeReason, "terminal_auth_failure") {
		t.Errorf("close_reason %q lacks the terminal_auth_failure prefix", dm.closeReason)
	}
	if !strings.Contains(dm.closeReason, "unauthorized") {
		t.Errorf("close_reason %q should name the error_category that tripped the bound", dm.closeReason)
	}
	if !strings.Contains(dm.closeReason, "reopened gd-TASK-auth") {
		t.Errorf("close_reason %q does not record the reopened task", dm.closeReason)
	}
	// The honest cause must NOT be counted in the involuntary-death (node-churn)
	// families — the whole point is to separate a credential failure out of the
	// context_cancelled/Spot-reclaim bucket the operator watches.
	if fam, counted := gardeniometrics.InvoluntaryDeathFamily(dm.closeReason); counted {
		t.Errorf("close_reason %q was classified as involuntary-death family %q; a credential failure must not inflate the node-churn buckets", dm.closeReason, fam)
	}
}

// TestMonitorExit_TerminalAuthFailure_OutOfCredits proves the bound covers the
// out_of_credits category too (a spent account is as unrecoverable as an ACL
// denial — no re-inject or rotation clears it).
func TestMonitorExit_TerminalAuthFailure_OutOfCredits(t *testing.T) {
	withFastTerminalAuthBound(t, 3)

	cs := &terminalAuthCoop{category: "out_of_credits"}
	csrv := httptest.NewServer(cs.handler())
	defer csrv.Close()

	dm := &livelockDaemon{agentBeadID: "gd-agent-ooc", taskID: "gd-TASK-ooc", actor: "gardenio-crew-ooc"}
	dsrv := httptest.NewServer(dm.handler(t))
	defer dsrv.Close()
	swapDaemon(t, dsrv.URL)
	withActor(t, "gardenio-crew-ooc")
	t.Setenv("KD_AGENT_ID", "gd-agent-ooc")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("COOP_S3_BUCKET", "")

	coop := coopapi.New(csrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := monitorExit(ctx, coop, nil, noRequestBaseline); err != nil {
		t.Fatalf("monitorExit returned error: %v", err)
	}
	if !cs.shutdownCalled {
		t.Error("coop.Shutdown was not called — the out_of_credits bound did not give up")
	}
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if !dm.agentClosed || dm.closedState != "failed" {
		t.Errorf("agent bead not failed-closed on out_of_credits (closed=%v state=%q)", dm.agentClosed, dm.closedState)
	}
	if !strings.Contains(dm.closeReason, "out_of_credits") {
		t.Errorf("close_reason %q should name the out_of_credits category", dm.closeReason)
	}
}

// TestIsTerminalAuthFailure_OnlyTerminalCategories proves the predicate fires on
// exactly the two non-recoverable credential categories and stays silent for
// recoverable ones (rate_limited/server_error/no_internet have their own in-place
// recovery) and for budget_exceeded (its own failover + budget-marker path).
func TestIsTerminalAuthFailure_OnlyTerminalCategories(t *testing.T) {
	cases := []struct {
		category string
		want     bool
	}{
		{"unauthorized", true},
		{"out_of_credits", true},
		{"budget_exceeded", false},
		{"rate_limited", false},
		{"model_rate_limited", false},
		{"server_error", false},
		{"no_internet", false},
		{"other", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.category, func(t *testing.T) {
			state := &coopapi.AgentState{State: "error", ErrorCategory: tc.category}
			if got := isTerminalAuthFailure(state); got != tc.want {
				t.Errorf("isTerminalAuthFailure(category=%q) = %v, want %v", tc.category, got, tc.want)
			}
		})
	}
	if isTerminalAuthFailure(nil) {
		t.Error("isTerminalAuthFailure(nil) must be false")
	}
}
