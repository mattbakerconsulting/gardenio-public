package statusreporter

import (
	"context"
	"sync"
	"testing"
	"time"
)

// gd-ozX2LfXsrv: on the controller machine principal every claim-release write
// to a bead outside its kbeads ownership grant, or to a human's claim, is an
// ownership 403 that repeats every reap pass. These pin the two guards that
// keep the sweeps from attempting them. The grant's types are pinned in
// cmd/controller (TestControllerMachineReclaimTypes_MatchGrant).

func TestStaleReaper_SkipsReclaim_HumanAssigneeWithClosedAgentBead(t *testing.T) {
	// A decision-resume respawn filed a type=agent bead titled with the
	// operator's email (gd-9H3WCBTnmq); it is closed, so agents-by-name
	// resolves the operator to a gone agent. Neither the operator's
	// in_progress epic nor their open bug may be reopened or cleared.
	stale := time.Now().Add(-30 * time.Minute)
	const human = "operator@example.com"

	epic := claimedIssueTask("gd-EPIC1", human, stale)
	epic.Type = "epic"
	issueBeads := []beadEntry{epic, openIssueBead("gd-BUG1", human, stale)}
	agentByName := map[string]beadEntry{human: closedAgentBead("bd-human", human, stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, nil, issueBeads, agentByName, results, &mu))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reopened != 0 || stats.OpenAssigneesCleared != 0 || stats.FlaggedOrphanedWithPR != 0 {
		t.Errorf("human claims touched: reopened=%d cleared=%d flagged=%d, want 0/0/0",
			stats.Reopened, stats.OpenAssigneesCleared, stats.FlaggedOrphanedWithPR)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"gd-EPIC1", "gd-BUG1"} {
		if res := results[id]; res != nil && res.patched {
			t.Errorf("%s (human-assigned) was patched; res=%+v", id, res)
		}
	}
}

func TestStaleReaper_ReclaimTypes_SkipsUngrantedTypes(t *testing.T) {
	// SetReclaimTypes("task"): a gone agent's task is still released, but its
	// in_progress bug and open epic are left alone, since the daemon would
	// refuse those writes.
	stale := time.Now().Add(-30 * time.Minute)

	bug := claimedIssueTask("gd-BUG2", "ghost-agent", stale)
	bug.Type = "bug"
	epic := openIssueBead("gd-EPIC2", "ghost-agent", stale)
	epic.Type = "epic"
	issueBeads := []beadEntry{claimedIssueTask("gd-TASK2", "ghost-agent", stale), bug, epic}
	agentByName := map[string]beadEntry{"ghost-agent": closedAgentBead("bd-ghost", "ghost-agent", stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, nil, issueBeads, agentByName, results, &mu))
	defer cleanup()
	reaper.SetReclaimTypes([]string{"task"})

	stats := reaper.Reap(context.Background())

	if stats.Reopened != 1 {
		t.Errorf("reopened: got %d, want 1 (the task only)", stats.Reopened)
	}
	if stats.OpenAssigneesCleared != 0 {
		t.Errorf("open_assignees_cleared: got %d, want 0 (epic is out of scope)", stats.OpenAssigneesCleared)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-TASK2"]; res == nil || res.status != "open" {
		t.Errorf("gd-TASK2 not reopened; res=%+v", res)
	}
	for _, id := range []string{"gd-BUG2", "gd-EPIC2"} {
		if res := results[id]; res != nil && res.patched {
			t.Errorf("%s is outside the reclaim types but was patched; res=%+v", id, res)
		}
	}
}

func TestStaleReaper_ReclaimTypes_UnsetKeepsEveryIssueType(t *testing.T) {
	// Without SetReclaimTypes (static-token deploys) a gone agent's bug is
	// still reopened.
	stale := time.Now().Add(-30 * time.Minute)

	bug := claimedIssueTask("gd-BUG3", "ghost-agent", stale)
	bug.Type = "bug"
	agentByName := map[string]beadEntry{"ghost-agent": closedAgentBead("bd-ghost", "ghost-agent", stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, nil, []beadEntry{bug}, agentByName, results, &mu))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reopened != 1 {
		t.Errorf("reopened: got %d, want 1", stats.Reopened)
	}
}
