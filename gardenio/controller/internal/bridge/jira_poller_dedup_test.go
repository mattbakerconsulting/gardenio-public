package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// flakyDaemon wraps mockJiraDaemon and fails every call while down is set —
// simulating the beads daemon being unreachable during a deploy window.
type flakyDaemon struct {
	*mockJiraDaemon
	down atomic.Bool
}

var errDaemonDown = errors.New("connection refused")

func (f *flakyDaemon) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if f.down.Load() {
		return nil, errDaemonDown
	}
	return f.mockJiraDaemon.ListBeadsFiltered(ctx, q)
}

func (f *flakyDaemon) ListTaskBeads(ctx context.Context) ([]*beadsapi.BeadDetail, error) {
	if f.down.Load() {
		return nil, errDaemonDown
	}
	return f.mockJiraDaemon.ListTaskBeads(ctx)
}

func (f *flakyDaemon) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if f.down.Load() {
		return "", errDaemonDown
	}
	return f.mockJiraDaemon.CreateBead(ctx, req)
}

// newDedupTestJiraServer returns a JIRA test server that always serves one
// DEVOPS-461 issue — the key that was duplicated on every deploy restart.
func newDedupTestJiraServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/search/jql" {
			http.NotFound(w, r)
			return
		}
		resp := map[string]any{
			"issues": []map[string]any{{
				"key": "DEVOPS-461", "id": "461",
				"fields": map[string]any{
					"summary":   "Set feature flag in all environments",
					"status":    map[string]string{"name": "To Do"},
					"issuetype": map[string]string{"name": "Task"},
					"priority":  map[string]string{"name": "Medium"},
				},
			}},
			"total": 1,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedExistingBead installs a pre-existing tracked bead for DEVOPS-461, as
// created by a prior scan before the restart.
func seedExistingBead(d *mockJiraDaemon) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.beads["bd-existing"] = &beadsapi.BeadDetail{
		ID: "bd-existing", Title: "[DEVOPS-461] Set feature flag in all environments",
		Type: "task", Kind: "issue", Status: "open",
		Labels: []string{"source:jira", "jira:DEVOPS-461"},
		Fields: map[string]string{"jira_key": "DEVOPS-461"},
	}
}

// TestJiraPoller_CatchUpFail_FullScanCreatesNoDuplicates reproduces the
// ka-0000001931 deploy-window sequence: catch-up fails (daemon down), the
// daemon comes back, and the startup full scan runs. Fail-closed means ZERO
// duplicate beads for the already-imported issue.
func TestJiraPoller_CatchUpFail_FullScanCreatesNoDuplicates(t *testing.T) {
	daemon := &flakyDaemon{mockJiraDaemon: newMockJiraDaemon()}
	seedExistingBead(daemon.mockJiraDaemon)
	daemon.down.Store(true) // daemon unreachable at bridge startup

	poller := NewJiraPoller(newTestJiraClient(newDedupTestJiraServer(t).URL), daemon, JiraPollerConfig{
		Projects: []string{"DEVOPS"}, Logger: discardLogger(),
	})

	// 1. Catch-up fails: tracked map stays empty, poller NOT caught up.
	if poller.CatchUp(context.Background()) {
		t.Fatal("expected CatchUp to fail while daemon is down")
	}
	if poller.isCaughtUp() {
		t.Fatal("poller must not be caught-up after failed CatchUp")
	}

	// 2. Daemon comes back up (deploy window ends).
	daemon.down.Store(false)

	// 3. Startup full scan fires with the empty tracked map — the old code
	// created gd-ujxc9fyvon here. Fail-closed: it must defer instead.
	poller.fullScan(context.Background())

	if got := len(daemon.getBeads()); got != 1 {
		t.Fatalf("expected 1 bead (no duplicates) after catch-up-fail full scan, got %d", got)
	}

	// 4. Next poll cycle: catch-up retries, succeeds, runs the deferred
	// full scan, and still creates no duplicate.
	poller.poll(context.Background())

	if !poller.isCaughtUp() {
		t.Error("expected poll to re-run catch-up and succeed once daemon is up")
	}
	if !poller.IsTracked("DEVOPS-461") {
		t.Error("expected DEVOPS-461 tracked after recovery")
	}
	if got := len(daemon.getBeads()); got != 1 {
		t.Fatalf("expected 1 bead (no duplicates) after recovery poll, got %d", got)
	}
}

// TestJiraPoller_PollSkippedUntilCaughtUp pins that poll creates nothing at
// all while the daemon is down — the poll path also raced bead creation
// during the deploy window (DEVOPS-548/549 in the incident log).
func TestJiraPoller_PollSkippedUntilCaughtUp(t *testing.T) {
	daemon := &flakyDaemon{mockJiraDaemon: newMockJiraDaemon()}
	seedExistingBead(daemon.mockJiraDaemon)
	daemon.down.Store(true)

	poller := NewJiraPoller(newTestJiraClient(newDedupTestJiraServer(t).URL), daemon, JiraPollerConfig{
		Projects: []string{"DEVOPS"}, Logger: discardLogger(),
	})

	if poller.CatchUp(context.Background()) {
		t.Fatal("expected CatchUp to fail while daemon is down")
	}

	// Poll while still down: fail-closed, no creates attempted.
	poller.poll(context.Background())
	if got := len(daemon.getBeads()); got != 1 {
		t.Fatalf("expected no new beads while daemon down, got %d total", got)
	}

	// Daemon recovers; poll converges without duplicates.
	daemon.down.Store(false)
	poller.poll(context.Background())
	if got := len(daemon.getBeads()); got != 1 {
		t.Fatalf("expected 1 bead (no duplicates) after recovery, got %d", got)
	}
}

// TestJiraPoller_CatchUpWithRetry_RecoversWithinBudget pins the startup retry
// loop: the daemon is down for the first attempts and comes up before the
// budget elapses; Run's catch-up must succeed without duplicates.
func TestJiraPoller_CatchUpWithRetry_RecoversWithinBudget(t *testing.T) {
	daemon := &flakyDaemon{mockJiraDaemon: newMockJiraDaemon()}
	seedExistingBead(daemon.mockJiraDaemon)
	daemon.down.Store(true)

	poller := NewJiraPoller(newTestJiraClient(newDedupTestJiraServer(t).URL), daemon, JiraPollerConfig{
		Projects: []string{"DEVOPS"}, Logger: discardLogger(),
	})
	poller.catchUpRetryInterval = 10 * time.Millisecond
	poller.catchUpRetryBudget = 5 * time.Second

	// Bring the daemon up shortly after startup, like a deploy window.
	go func() {
		time.Sleep(30 * time.Millisecond)
		daemon.down.Store(false)
	}()

	poller.catchUpWithRetry(context.Background())

	if !poller.isCaughtUp() {
		t.Fatal("expected catch-up to succeed within retry budget")
	}
	if !poller.IsTracked("DEVOPS-461") {
		t.Error("expected DEVOPS-461 tracked after retry catch-up")
	}
}

// TestJiraPoller_CreateBead_DupCheckErrorFailsClosed pins that a failed
// server-side duplicate check aborts creation instead of proceeding —
// proceeding was the second half of the incident (create raced daemon
// startup between the dup-check error and the create call).
func TestJiraPoller_CreateBead_DupCheckErrorFailsClosed(t *testing.T) {
	daemon := &flakyDaemon{mockJiraDaemon: newMockJiraDaemon()}
	seedExistingBead(daemon.mockJiraDaemon)

	poller := NewJiraPoller(newTestJiraClient(newDedupTestJiraServer(t).URL), daemon, JiraPollerConfig{
		Projects: []string{"DEVOPS"}, Logger: discardLogger(),
	})
	if !poller.CatchUp(context.Background()) {
		t.Fatal("catch-up should succeed while daemon is up")
	}

	// Simulate the daemon dropping between poll start and create: the dup
	// check errors, so createBeadFromIssue must return an error, not create.
	daemon.down.Store(true)
	_, err := poller.createBeadFromIssue(context.Background(), JiraIssue{
		Key:    "DEVOPS-999",
		Fields: JiraIssueFields{Summary: "new issue"},
	})
	if err == nil {
		t.Fatal("expected createBeadFromIssue to fail closed when dup check errors")
	}
	if got := len(daemon.getBeads()); got != 1 {
		t.Fatalf("expected no bead created on dup-check error, got %d total", got)
	}
}

// TestJiraPoller_RunDefersEverythingWhileDaemonDown drives Run end-to-end
// with the daemon down past the (tiny) retry budget: the startup full scan
// and trigger catch-up must both defer, and the first poll after recovery
// must converge without duplicates.
func TestJiraPoller_RunDefersEverythingWhileDaemonDown(t *testing.T) {
	daemon := &flakyDaemon{mockJiraDaemon: newMockJiraDaemon()}
	seedExistingBead(daemon.mockJiraDaemon)
	daemon.down.Store(true)

	poller := NewJiraPoller(newTestJiraClient(newDedupTestJiraServer(t).URL), daemon, JiraPollerConfig{
		Projects:     []string{"DEVOPS"},
		PollInterval: 20 * time.Millisecond,
		Logger:       discardLogger(),
	})
	poller.catchUpRetryInterval = 5 * time.Millisecond
	poller.catchUpRetryBudget = 15 * time.Millisecond

	// Recover the daemon after the retry budget has elapsed, so Run enters
	// its poll loop NOT caught up (the worst case from the incident).
	go func() {
		time.Sleep(60 * time.Millisecond)
		daemon.down.Store(false)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = poller.Run(ctx)

	if !poller.isCaughtUp() {
		t.Fatal("expected poller to catch up once daemon recovered")
	}
	if got := len(daemon.getBeads()); got != 1 {
		t.Fatalf("expected 1 bead (no duplicates) after full Run cycle, got %d", got)
	}
	if !poller.IsTracked("DEVOPS-461") {
		t.Error("expected DEVOPS-461 tracked after recovery")
	}
}
