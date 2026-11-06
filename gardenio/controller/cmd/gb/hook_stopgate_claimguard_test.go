package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// Layer-1 coverage for the stop-gate CLAIM GUARD (ka-0000006454, decision
// ka-0000005194 = "claim-guard"). The cooldown fast-path used to close the
// agent bead unconditionally — the lethal act, since the reconciler then stops
// keeping the pod alive — on the single most ordinary shape in the fleet: two
// quiet turn-ends inside one cooldown window. That made it the #2
// involuntary-death vector (34 deaths/7d). The guard spares an agent that still
// holds a claimed in-progress task, because `gb stop` REFUSES to despawn while
// one is held (checkClaimedWork, stop.go) — so reaching this branch WITH a claim
// proves the agent did not finish.
//
// These tests pin stopGateClaimedTask, the predicate the guard turns on. Its
// two directions matter for different reasons: a false negative resurrects the
// deaths, a false positive keeps a pod alive that should have been torn down.

// setupClaimedTaskDaemon installs a stub daemon whose ListAssignedTask lookup
// (GET /v1/beads?status=in_progress&assignee=…&kind=issue) returns one task with
// the given id and status. Returns a pointer to the last query values seen so a
// test can lock the query shape. An empty id serves an empty page.
func setupClaimedTaskDaemon(t *testing.T, taskID, status string) *url.Values {
	t.Helper()
	seen := &url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.URL.Query()
		beads := []map[string]any{}
		if taskID != "" {
			beads = append(beads, map[string]any{
				"id":       taskID,
				"type":     "task",
				"status":   status,
				"assignee": r.URL.Query().Get("assignee"),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
	return seen
}

// withActor (hook_reopen_test.go) sets the package-global actor the claim
// lookup keys on.

// TestStopGateClaimedTask_HoldsClaim is the core assertion and the exact
// regression that killed monorepo-fsmtriageclassify-reflow-mode-iqr: an agent
// holding an in-progress claim must be reported as working, so the cooldown
// branch spares its bead instead of closing it and reopening the task.
func TestStopGateClaimedTask_HoldsClaim(t *testing.T) {
	withActor(t, "monorepo-crew-abc")
	seen := setupClaimedTaskDaemon(t, "gd-task1", "in_progress")

	if got := stopGateClaimedTask(context.Background()); got != "gd-task1" {
		t.Fatalf("stopGateClaimedTask = %q, want gd-task1 — a claim-holding agent must not be closed on the cooldown path", got)
	}
	// Lock the lookup shape: it must be the same in_progress/issue/assignee
	// query `gb stop` and reopenOrphanedClaimedTask use, so the predicate that
	// spares the agent cannot disagree with the one that would reopen its task.
	if got := seen.Get("status"); got != "in_progress" {
		t.Errorf("claim lookup status = %q, want in_progress", got)
	}
	if got := seen.Get("kind"); got != "issue" {
		t.Errorf("claim lookup kind = %q, want issue", got)
	}
	if got := seen.Get("assignee"); got != "monorepo-crew-abc" {
		t.Errorf("claim lookup assignee = %q, want monorepo-crew-abc", got)
	}
}

// TestStopGateClaimedTask_NoClaim pins the shutdown-safe direction: an agent
// holding nothing falls through to the normal close, so the guard can never
// wedge a genuinely-finished agent's pod alive forever.
func TestStopGateClaimedTask_NoClaim(t *testing.T) {
	withActor(t, "monorepo-crew-abc")
	setupClaimedTaskDaemon(t, "", "")

	if got := stopGateClaimedTask(context.Background()); got != "" {
		t.Errorf("stopGateClaimedTask = %q, want empty when the agent holds no claim", got)
	}
}

// TestStopGateClaimedTask_NonInProgressStatus covers the defensive re-check: a
// task that came back at some other status is not work in flight and must not
// spare the agent, even though the server-side filter should never return one.
func TestStopGateClaimedTask_NonInProgressStatus(t *testing.T) {
	withActor(t, "monorepo-crew-abc")
	setupClaimedTaskDaemon(t, "gd-task1", "closed")

	if got := stopGateClaimedTask(context.Background()); got != "" {
		t.Errorf("stopGateClaimedTask = %q, want empty for a non-in_progress task", got)
	}
}

// TestStopGateClaimedTask_DaemonError is the fail-CLOSED case. Unlike a
// fail-open guard, a daemon blip must revert to the pre-fix close rather than
// spare a pod on an unverified claim — a false positive here keeps a pod alive
// burning spend, which is worse than the death we are fixing.
func TestStopGateClaimedTask_DaemonError(t *testing.T) {
	withActor(t, "monorepo-crew-abc")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	if got := stopGateClaimedTask(context.Background()); got != "" {
		t.Errorf("stopGateClaimedTask = %q, want empty on a daemon error (fail closed)", got)
	}
}

// TestStopGateClaimedTask_NoActor is the other fail-closed case: with no
// resolvable actor there is no claim to verify, so the guard must not guess.
// A daemon that would answer "claimed" if asked proves the guard is the missing
// identity, not an empty page.
func TestStopGateClaimedTask_NoActor(t *testing.T) {
	setupClaimedTaskDaemon(t, "gd-task1", "in_progress")

	for _, name := range []string{"", "unknown"} {
		withActor(t, name)
		if got := stopGateClaimedTask(context.Background()); got != "" {
			t.Errorf("stopGateClaimedTask = %q with actor=%q, want empty", got, name)
		}
	}
}

// TestStopGateEscapeHatchSurvivesClaimGuard is the COST-SINK assertion — the
// reason the claim-guard was safe to choose over "never close" (option A).
//
// The cooldown branch must not become an unbounded shield for a
// decision->yield->block runaway. It cannot, because the consecutive-block
// counter that arms the escape hatch is advanced ONLY by the block path
// (recordStopGateBlock), never by the cooldown branch the guard sits on. So a
// genuine loop still walks the counter to stopGateMaxBlocks and gets closed by
// the escape hatch regardless of any claim it holds. This test pins that
// invariant directly: the claim-guard has no input to stopGateEscapeHatch.
func TestStopGateEscapeHatchSurvivesClaimGuard(t *testing.T) {
	dir := t.TempDir()
	oldCount := stopGateBlockCountFile
	stopGateBlockCountFile = dir + "/block-count"
	t.Cleanup(func() { stopGateBlockCountFile = oldCount })
	oldCooldown := stopGateCooldownFile
	stopGateCooldownFile = dir + "/last-block"
	t.Cleanup(func() { stopGateCooldownFile = oldCooldown })

	// An agent holding a claim — the state the guard spares on the cooldown path.
	withActor(t, "loopy-agent")
	setupClaimedTaskDaemon(t, "gd-task1", "in_progress")
	if stopGateClaimedTask(context.Background()) == "" {
		t.Fatal("precondition: expected the agent to hold a claim")
	}

	// Walk the block path the way a real runaway does. The hatch must stay
	// disarmed for every block before the last one.
	for i := 1; i <= stopGateMaxBlocks; i++ {
		if stopGateEscapeHatch() {
			t.Fatalf("escape hatch fired early, before block %d of %d", i, stopGateMaxBlocks)
		}
		recordStopGateBlock()
	}

	// After stopGateMaxBlocks blocks the hatch MUST be armed — the claim does
	// not and must not suppress it, or the cost-sink guard would be gone.
	if !stopGateEscapeHatch() {
		t.Fatalf("escape hatch did NOT fire after %d blocks while a claim was held — the claim-guard must not disarm the cost-sink guard", stopGateMaxBlocks)
	}
}
