package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gardeniometrics "gardenio/controller/internal/metrics"
)

// involuntaryFamilyOf asserts the stamped close_reason classifies through the
// SHARED metric predicate as an involuntary death in the upstream_api_error
// family — the same predicate gardenio_agent_involuntary_deaths_total uses, so a
// pass proves the teardown-stamped reason is counted under that family on the
// dashboard.
func involuntaryFamilyOf(t *testing.T, closeReason string) (family string, involuntary bool) {
	t.Helper()
	if !strings.HasPrefix(closeReason, gardeniometrics.FamilyUpstreamAPIError+"_involuntary") {
		t.Errorf("close_reason %q does not carry the upstream_api_error_involuntary prefix", closeReason)
	}
	fam, ok := gardeniometrics.InvoluntaryDeathFamily(closeReason)
	if ok && fam != gardeniometrics.FamilyUpstreamAPIError {
		t.Errorf("close_reason %q classified as family %q, want %q", closeReason, fam, gardeniometrics.FamilyUpstreamAPIError)
	}
	return fam, ok
}

// upstreamDeathMockDaemon is a minimal in-memory agent bead that captures the
// close_reason a teardown stamps. It covers only the endpoints
// closeAgentBeadBestEffort touches on the max_restarts_reached / context_cancelled
// paths with no claimed task and no Slack thread:
//   - GET  /v1/beads/<id>          (the pre-close guards + wrapup lookup)
//   - POST /v1/beads/<id>/close    (CloseBead — the close_reason we assert on)
type upstreamDeathMockDaemon struct {
	mu          sync.Mutex
	agentBeadID string
	fields      map[string]any
	closeReason string
	closed      bool
}

func (m *upstreamDeathMockDaemon) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+m.agentBeadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     m.agentBeadID,
				"type":   "agent",
				"status": "open",
				"fields": m.fields,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+m.agentBeadID+"/close":
			body, _ := io.ReadAll(r.Body)
			var close map[string]any
			_ = json.Unmarshal(body, &close)
			if cr, ok := close["close_reason"].(string); ok {
				m.closeReason = cr
			}
			m.closed = true
			_ = json.NewEncoder(w).Encode(map[string]any{"id": m.agentBeadID})
		default:
			// Any other endpoint (e.g. ListAssignedTask) is not expected in these
			// no-claim / no-thread scenarios; fail loudly if reached.
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}
}

func (m *upstreamDeathMockDaemon) reason() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeReason
}

// newUpstreamDeathHarness wires a mock daemon + empty actor/slack env so the
// teardown takes the no-claim, no-breadcrumb path and only the close_reason
// computation is under test.
func newUpstreamDeathHarness(t *testing.T, beadID string) *upstreamDeathMockDaemon {
	t.Helper()
	mock := &upstreamDeathMockDaemon{agentBeadID: beadID, fields: map[string]any{}}
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	// Empty actor → reopenOrphanedClaimedTask no-ops (didWork=false), isolating
	// the upstream-outage override from the claimed-task salvage path.
	withActor(t, "")
	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	// No Slack thread → postInterruptedBreadcrumbOnce no-ops.
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("SLACK_BRIDGE_URL", "")
	// Start from a clean upstream-observation slate every test.
	clearUpstreamOutageObservation()
	t.Cleanup(clearUpstreamOutageObservation)
	return mock
}

// TestCloseAgentBead_MaxRestarts_UpstreamOutage is the canonical misattribution
// fix (ka-0000000202): when the restart loop gives up (max_restarts_reached)
// while a Claude-upstream outage was still active, the death must be stamped
// upstream_api_error_involuntary — NOT the generic max_restarts_reached that
// used to silently absorb outage-driven deaths.
func TestCloseAgentBead_MaxRestarts_UpstreamOutage(t *testing.T) {
	mock := newUpstreamDeathHarness(t, "gd-agent-maxrestarts-outage")
	recordUpstreamOutageObservation("server_error")

	closeAgentBeadBestEffort("max_restarts_reached")

	got := mock.reason()
	if _, involuntary := involuntaryFamilyOf(t, got); !involuntary {
		t.Fatalf("close_reason %q did not classify as an involuntary upstream death", got)
	}
}

// TestCloseAgentBead_MaxRestarts_NoOutage proves the override is conditional: a
// genuine restart-loop exhaustion with NO upstream outage observed keeps the
// plain max_restarts_reason so the max_restarts family is not polluted.
func TestCloseAgentBead_MaxRestarts_NoOutage(t *testing.T) {
	mock := newUpstreamDeathHarness(t, "gd-agent-maxrestarts-clean")
	// No recordUpstreamOutageObservation call → no outage active.

	closeAgentBeadBestEffort("max_restarts_reached")

	if got := mock.reason(); got != "max_restarts_reached" {
		t.Fatalf("close_reason = %q, want %q (no outage → no override)", got, "max_restarts_reached")
	}
}

// TestCloseAgentBead_ContextCancelled_UpstreamOutage covers the dominant real
// teardown: an agent sitting on an upstream error is SIGTERM'd (rollout / drain
// / Spot reclaim) → closeAgentBeadBestEffort("context_cancelled"). With the
// outage active, the death is attributed to upstream_api_error rather than the
// routine node-churn context_cancelled family.
func TestCloseAgentBead_ContextCancelled_UpstreamOutage(t *testing.T) {
	mock := newUpstreamDeathHarness(t, "gd-agent-ctxcancel-outage")
	recordUpstreamOutageObservation("no_internet")

	closeAgentBeadBestEffort("context_cancelled")

	got := mock.reason()
	if _, involuntary := involuntaryFamilyOf(t, got); !involuntary {
		t.Fatalf("close_reason %q did not classify as an involuntary upstream death", got)
	}
}

// TestCloseAgentBead_ContextCancelled_NoOutage proves a plain SIGTERM with no
// claimed task and no outage keeps the bare context_cancelled reason (the
// pre-existing behavior) — the upstream override must not fire spuriously.
func TestCloseAgentBead_ContextCancelled_NoOutage(t *testing.T) {
	mock := newUpstreamDeathHarness(t, "gd-agent-ctxcancel-clean")

	closeAgentBeadBestEffort("context_cancelled")

	if got := mock.reason(); got != "context_cancelled" {
		t.Fatalf("close_reason = %q, want %q (no task, no outage)", got, "context_cancelled")
	}
}

// TestCloseAgentBead_ContextCancelled_StaleOutageIgnored is the false-positive
// guard: an upstream observation older than the staleness window must NOT
// attribute an unrelated later death to the outage.
func TestCloseAgentBead_ContextCancelled_StaleOutageIgnored(t *testing.T) {
	mock := newUpstreamDeathHarness(t, "gd-agent-ctxcancel-stale")
	// Observe an outage, then simulate it being long past the staleness window.
	recordUpstreamOutageObservation("server_error")
	orig := upstreamOutageStaleWindow
	upstreamOutageStaleWindow = -1 // any prior observation is now "stale"
	t.Cleanup(func() { upstreamOutageStaleWindow = orig })

	closeAgentBeadBestEffort("context_cancelled")

	if got := mock.reason(); got != "context_cancelled" {
		t.Fatalf("close_reason = %q, want %q (stale observation must be ignored)", got, "context_cancelled")
	}
}

// TestUpstreamOutageActive_ClearedByProductiveTurn verifies the reset semantics:
// once cleared (as a productive "working" turn does in monitorExit), the outage
// is no longer active and a subsequent death is not misattributed.
func TestUpstreamOutageActive_ClearedByProductiveTurn(t *testing.T) {
	clearUpstreamOutageObservation()
	t.Cleanup(clearUpstreamOutageObservation)

	recordUpstreamOutageObservation("server_error")
	if cat, active := upstreamOutageActive(); !active || cat != "server_error" {
		t.Fatalf("after observation: active=%v cat=%q, want true/server_error", active, cat)
	}
	clearUpstreamOutageObservation()
	if _, active := upstreamOutageActive(); active {
		t.Fatal("after clear: outage still active, want inactive")
	}
}
