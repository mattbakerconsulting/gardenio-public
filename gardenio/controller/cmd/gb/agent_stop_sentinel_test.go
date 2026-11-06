package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// stopTestDaemon points the package daemon at a server that answers every
// request with status (and a JSON error body), counting the requests. It also
// points the stop sentinel at a temp dir, zeroes the retry delay and replaces
// the park with a recorder, restoring all of it on cleanup.
func stopTestDaemon(t *testing.T, handler http.HandlerFunc) (requests *atomic.Int32, parked *atomic.Int32) {
	t.Helper()
	requests = &atomic.Int32{}
	parked = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	oldDaemon, oldPath, oldDelay, oldPark := daemon, stopSentinelPath, stopCheckRetryDelay, parkContainer
	t.Cleanup(func() {
		daemon, stopSentinelPath, stopCheckRetryDelay, parkContainer = oldDaemon, oldPath, oldDelay, oldPark
	})
	d, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	daemon = d
	stopSentinelPath = filepath.Join(t.TempDir(), "gb-stop-requested")
	stopCheckRetryDelay = 0
	parkContainer = func(context.Context) { parked.Add(1) }
	return requests, parked
}

func statusHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(status)})
	}
}

// The incident: the daemon answers 403 for the agent's own bead. The loop's
// stop check must park (no respawn) after a single read, not retry and respawn.
func TestHandleStopAfterExit_ForbiddenOwnBeadParksWithoutRespawn(t *testing.T) {
	requests, parked := stopTestDaemon(t, statusHandler(http.StatusForbidden))

	if respawn := !handleStopAfterExit(context.Background(), "gd-agent1"); respawn {
		t.Fatal("handleStopAfterExit returned false on a 403: the loop would respawn coop")
	}
	if got := parked.Load(); got != 1 {
		t.Errorf("parked %d times, want 1", got)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("daemon saw %d requests, want 1 (a denial is not retried)", got)
	}
}

func TestHandleStopAfterExit_UnauthorizedParks(t *testing.T) {
	_, parked := stopTestDaemon(t, statusHandler(http.StatusUnauthorized))

	if !handleStopAfterExit(context.Background(), "gd-agent1") {
		t.Fatal("handleStopAfterExit returned false on a 401: the loop would respawn coop")
	}
	if parked.Load() != 1 {
		t.Error("a 401 on the agent's own bead should park the loop")
	}
}

// A transport-ish failure (5xx) keeps the old behavior: retry, then respawn.
func TestHandleStopAfterExit_ServerErrorStillRespawns(t *testing.T) {
	requests, parked := stopTestDaemon(t, statusHandler(http.StatusInternalServerError))

	if handleStopAfterExit(context.Background(), "gd-agent1") {
		t.Fatal("a 500 should not stop the loop")
	}
	if parked.Load() != 0 {
		t.Error("a 500 should not park the loop")
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("daemon saw %d requests, want 3 retries", got)
	}
}

// stop_requested readable and the close succeeds: exit, no park.
func TestHandleStopAfterExit_StopRequestedClosesAndExits(t *testing.T) {
	var closed atomic.Bool
	_, parked := stopTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-agent1", "status": "open", "fields": map[string]string{"stop_requested": "true"},
			})
			return
		}
		closed.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-agent1", "status": "closed"})
	})

	if !handleStopAfterExit(context.Background(), "gd-agent1") {
		t.Fatal("stop_requested=true should stop the loop")
	}
	if !closed.Load() {
		t.Error("the agent bead was not closed")
	}
	if parked.Load() != 0 {
		t.Error("a successful close should exit, not park")
	}
}

// gb stop against a daemon that denies the stop_requested write: the sentinel
// is written, isStopRequested honors it, the loop stops (and parks, since the
// close is denied too), and a container restart's boot check honors it as well.
func TestMarkStopRequested_ForbiddenWritesSentinelThatLoopHonors(t *testing.T) {
	_, parked := stopTestDaemon(t, statusHandler(http.StatusForbidden))

	if err := markStopRequested(context.Background(), "gd-agent1"); err != nil {
		t.Fatalf("markStopRequested: %v (the sentinel should have absorbed the 403)", err)
	}
	data, err := os.ReadFile(stopSentinelPath)
	if err != nil {
		t.Fatalf("sentinel not written: %v", err)
	}
	if strings.TrimSpace(string(data)) != "gd-agent1" {
		t.Errorf("sentinel = %q, want the agent bead ID", data)
	}

	if !isStopRequested(context.Background(), "gd-agent1") {
		t.Error("isStopRequested ignored the sentinel")
	}
	if !handleStopAfterExit(context.Background(), "gd-agent1") {
		t.Fatal("the loop respawned coop despite the sentinel")
	}
	if parked.Load() != 1 {
		t.Errorf("parked %d times, want 1 (the close is denied, so exiting would restart the container)", parked.Load())
	}

	t.Setenv("KD_AGENT_ID", "gd-agent1")
	if !shouldExitBeforeStart(context.Background()) {
		t.Error("shouldExitBeforeStart ignored the sentinel after a container restart")
	}
}

func TestMarkStopRequested_WriteOKNoSentinel(t *testing.T) {
	stopTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-agent1"})
	})

	if err := markStopRequested(context.Background(), "gd-agent1"); err != nil {
		t.Fatalf("markStopRequested: %v", err)
	}
	if _, err := os.Stat(stopSentinelPath); !os.IsNotExist(err) {
		t.Errorf("sentinel written although the bead write succeeded (stat err %v)", err)
	}
}

func TestMarkStopRequested_BothWritesFailReturnsError(t *testing.T) {
	stopTestDaemon(t, statusHandler(http.StatusForbidden))
	stopSentinelPath = filepath.Join(t.TempDir(), "missing-dir", "gb-stop-requested")

	err := markStopRequested(context.Background(), "gd-agent1")
	if err == nil || !strings.Contains(err.Error(), "stop_requested") {
		t.Fatalf("err = %v, want the stop_requested write error", err)
	}
}

// A sentinel left for another agent must not stop this one.
func TestStopSentinel_OtherAgentIgnored(t *testing.T) {
	stopTestDaemon(t, statusHandler(http.StatusInternalServerError))
	if err := writeStopSentinel("gd-other"); err != nil {
		t.Fatal(err)
	}
	if stopSentinelSet("gd-agent1") {
		t.Error("a sentinel for gd-other stopped gd-agent1")
	}
	if !stopSentinelSet("gd-other") {
		t.Error("the sentinel for gd-other was not read back")
	}
}

// The gb agent start loop (gd-WKjtvzPl6g): a 403 on the agent's own bead parks
// after a single read instead of respawning coop.
func TestHandleAgentStartStop_ForbiddenOwnBeadParks(t *testing.T) {
	requests, parked := stopTestDaemon(t, statusHandler(http.StatusForbidden))

	if !handleAgentStartStop(context.Background(), "gd-agent1") {
		t.Fatal("handleAgentStartStop returned false on a 403: gb agent start would respawn coop")
	}
	if got := parked.Load(); got != 1 {
		t.Errorf("parked %d times, want 1", got)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("daemon saw %d requests, want 1 (a denial is not retried)", got)
	}
}

func TestHandleAgentStartStop_ServerErrorRespawns(t *testing.T) {
	_, parked := stopTestDaemon(t, statusHandler(http.StatusInternalServerError))

	if handleAgentStartStop(context.Background(), "gd-agent1") {
		t.Fatal("a 500 should not stop the loop")
	}
	if parked.Load() != 0 {
		t.Error("a 500 should not park the loop")
	}
}

func TestHandleAgentStartStop_NoAgentBeadRespawns(t *testing.T) {
	requests, parked := stopTestDaemon(t, statusHandler(http.StatusForbidden))

	if handleAgentStartStop(context.Background(), "") {
		t.Fatal("no agent bead should not stop the loop")
	}
	if requests.Load() != 0 || parked.Load() != 0 {
		t.Errorf("requests=%d parked=%d, want neither without an agent bead", requests.Load(), parked.Load())
	}
}

// stop_requested readable and the close succeeds: the close carries the
// stop_requested close fields and the loop exits without parking.
func TestHandleAgentStartStop_StopRequestedClosesWithReason(t *testing.T) {
	var closeBody atomic.Value
	_, parked := stopTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-agent1", "status": "open", "fields": map[string]string{"stop_requested": "true"},
			})
			return
		}
		body, _ := io.ReadAll(r.Body)
		closeBody.Store(string(body))
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-agent1", "status": "closed"})
	})

	if !handleAgentStartStop(context.Background(), "gd-agent1") {
		t.Fatal("stop_requested=true should stop the loop")
	}
	body, _ := closeBody.Load().(string)
	if !strings.Contains(body, `"close_reason":"stop_requested"`) {
		t.Errorf("close request %q does not carry close_reason stop_requested", body)
	}
	if parked.Load() != 0 {
		t.Error("a successful close should exit, not park")
	}
}

// The sentinel says stop but the daemon denies the close: park, since exiting
// would only restart the container with the bead still open.
func TestHandleAgentStartStop_SentinelDeniedCloseParks(t *testing.T) {
	_, parked := stopTestDaemon(t, statusHandler(http.StatusForbidden))
	if err := writeStopSentinel("gd-agent1"); err != nil {
		t.Fatal(err)
	}

	if !handleAgentStartStop(context.Background(), "gd-agent1") {
		t.Fatal("the loop respawned coop despite the sentinel")
	}
	if got := parked.Load(); got != 1 {
		t.Errorf("parked %d times, want 1 (the close is denied)", got)
	}
}

// A close that fails for a non-denial reason exits as before, without parking.
func TestHandleAgentStartStop_SentinelCloseServerErrorExits(t *testing.T) {
	_, parked := stopTestDaemon(t, statusHandler(http.StatusInternalServerError))
	if err := writeStopSentinel("gd-agent1"); err != nil {
		t.Fatal(err)
	}

	if !handleAgentStartStop(context.Background(), "gd-agent1") {
		t.Fatal("the loop respawned coop despite the sentinel")
	}
	if parked.Load() != 0 {
		t.Error("a 500 on the close should exit, not park")
	}
}
