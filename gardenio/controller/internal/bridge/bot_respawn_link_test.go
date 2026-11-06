package bridge

import (
	"context"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// These tests close the linking gap (ka-0000001874 Part B): respawnThreadAgent
// (the follow-up path) previously never stamped slack_thread_bead_id nor wrote
// the triggered-by lineage edge, so a thread replied-to a second time lost its
// parent link and the event-driven autocloser could never close it. Both the
// warm-pool and cold-resume branches must now resolve-or-create the thread bead
// and stamp the link + dep.

// hasTriggeredByDep reports whether the mock recorded a triggered-by dep from
// beadID → threadBeadID.
func hasTriggeredByDep(daemon *mockDaemon, beadID, threadBeadID string) bool {
	for _, d := range daemon.getDeps() {
		if d.BeadID == beadID && d.DependsOnID == threadBeadID && d.DepType == "triggered-by" {
			return true
		}
	}
	return false
}

// TestRespawnThreadAgent_ColdResume_StampsThreadLink verifies the cold-resume
// mint stamps slack_thread_bead_id and the triggered-by dep on the new bead.
func TestRespawnThreadAgent_ColdResume_StampsThreadLink(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-LINK", "thread")
	seedPriorAgentBead(daemon, "thread-link-1", "monorepo")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var called bool
	// false → pool miss, so we exercise the cold-resume branch.
	ctrlSrv := fakeControllerForAssign(t, false, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-LINK", "5555.6666",
		"thread-link-1", "follow-up", "U-HUMAN")

	// The thread bead must have been resolved-or-created under the canonical
	// label, and the cold-resume agent bead must carry slack_thread_bead_id.
	wantLabel := threadBeadLabel("C-LINK", "5555.6666")
	var threadBeadID string
	var resume *beadsapi.BeadDetail
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		for _, l := range b.Labels {
			if l == wantLabel {
				threadBeadID = b.ID
			}
		}
		if b.Type == "agent" && b.Fields["spawn_source"] == "slack-thread-resume" {
			resume = b
		}
	}
	daemon.mu.Unlock()

	if threadBeadID == "" {
		t.Fatal("expected respawn to resolve-or-create the thread bead")
	}
	if resume == nil {
		t.Fatal("expected a cold-resume agent bead")
	}
	if got := resume.Fields["slack_thread_bead_id"]; got != threadBeadID {
		t.Errorf("expected slack_thread_bead_id=%s on cold-resume bead, got %q", threadBeadID, got)
	}
	if !hasTriggeredByDep(daemon, resume.ID, threadBeadID) {
		t.Errorf("expected triggered-by dep %s → %s", resume.ID, threadBeadID)
	}
}

// TestRespawnThreadAgent_WarmPool_StampsThreadLink verifies the warm-pool hit
// path stamps slack_thread_bead_id (via UpdateBeadFields) and the triggered-by
// dep on the assigned pool bead.
func TestRespawnThreadAgent_WarmPool_StampsThreadLink(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-LINK2", "thread")
	seedPriorAgentBead(daemon, "thread-link-2", "monorepo")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var called bool
	// true → pool hit, so we exercise the warm-pool branch.
	ctrlSrv := fakeControllerForAssign(t, true, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-LINK2", "7777.8888",
		"thread-link-2", "follow-up", "U-HUMAN")

	wantLabel := threadBeadLabel("C-LINK2", "7777.8888")
	var threadBeadID string
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		for _, l := range b.Labels {
			if l == wantLabel {
				threadBeadID = b.ID
			}
		}
	}
	daemon.mu.Unlock()
	if threadBeadID == "" {
		t.Fatal("expected respawn to resolve-or-create the thread bead")
	}

	// fakeControllerForAssign returns a fixed pool bead id; assert the dep was
	// written from that assigned bead to the thread bead.
	deps := daemon.getDeps()
	found := false
	for _, d := range deps {
		if d.DependsOnID == threadBeadID && d.DepType == "triggered-by" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a triggered-by dep to thread bead %s on warm-pool assign, deps=%+v",
			threadBeadID, deps)
	}
}
