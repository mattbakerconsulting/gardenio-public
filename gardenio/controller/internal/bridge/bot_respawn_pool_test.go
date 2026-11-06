package bridge

import (
	"context"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// These tests close the gd-n3Uln6HiKK gap: respawnThreadAgent (the follow-up
// path, fired when a thread's prior agent has despawned) previously went
// straight to a cold session-resume mint, never touching the warm prewarmed
// pool — so every Slack reply after the agent stopped paid the ~45s cold-mint
// tax even when warm pods were waiting. handleThreadSpawn (the FIRST mention)
// already tried the pool first; these tests assert respawn now does too, and
// that a pool miss still degrades to the cold session-resume mint.

// seedPriorAgentBead pre-populates the mock with a CLOSED prior agent bead for
// the given name + project, so priorAgentBead resolves the project
// the same way the real respawn path does (inheriting the original spawn's
// project instead of re-inferring from the channel).
func seedPriorAgentBead(daemon *mockDaemon, agentName, project string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName+"-prior"] = &beadsapi.BeadDetail{
		ID:     agentName + "-prior",
		Title:  agentName,
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":   agentName,
			"project": project,
		},
	}
}

// TestRespawnThreadAgent_FastPath_AssignsPrewarmed verifies the respawn path
// attempts pool assign first and, on a HIT, creates NO cold-start agent bead.
func TestRespawnThreadAgent_FastPath_AssignsPrewarmed(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-K6", "k6-perf")
	seedPriorAgentBead(daemon, "thread-9999-1111", "monorepo")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerForAssign(t, true, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-K6", "9999.1111",
		"thread-9999-1111", "follow-up question", "U-HUMAN")

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatal("expected respawn fast path to call /api/v1/pool/assign")
	}
	if gotRole != "k6-perf" {
		t.Errorf("expected role=k6-perf transmitted to pool assign, got %q", gotRole)
	}
	// On a pool HIT, no cold-start resume agent bead should be created. The only
	// agent bead present is the seeded prior (closed) one.
	created := 0
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.Fields["spawn_source"] == "slack-thread-resume" {
			created++
		}
	}
	if created != 0 {
		t.Errorf("expected 0 cold-start resume beads on pool hit, got %d", created)
	}
}

// TestRespawnThreadAgent_PoolMiss_FallsThroughToColdResume verifies a 404 pool
// miss degrades to the cold session-resume mint, preserving today's behavior:
// a fresh agent bead with spawn_source=slack-thread-resume and the resolved role.
func TestRespawnThreadAgent_PoolMiss_FallsThroughToColdResume(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-K6", "k6-perf")
	seedPriorAgentBead(daemon, "thread-9999-3333", "monorepo")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerForAssign(t, false, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-K6", "9999.3333",
		"thread-9999-3333", "follow-up question", "U-HUMAN")

	mu.Lock()
	transmitted := gotRole
	wasCalled := called
	mu.Unlock()

	if !wasCalled {
		t.Fatal("expected respawn to attempt pool assign before falling through")
	}
	if transmitted != "k6-perf" {
		t.Errorf("expected role=k6-perf transmitted on the attempted assign, got %q", transmitted)
	}

	// On a pool MISS, cold resume must create exactly one fresh agent bead
	// carrying the resolved role and the resume spawn_source.
	var resumeBeads []*beadsapi.BeadDetail
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.Fields["spawn_source"] == "slack-thread-resume" {
			resumeBeads = append(resumeBeads, b)
		}
	}
	if len(resumeBeads) != 1 {
		t.Fatalf("expected 1 cold-resume agent bead on pool miss, got %d", len(resumeBeads))
	}
	if resumeBeads[0].Fields["role"] != "k6-perf" {
		t.Errorf("expected cold-resume role=k6-perf, got %q", resumeBeads[0].Fields["role"])
	}
	if resumeBeads[0].Fields["mode"] != "crew" {
		t.Errorf("expected cold-resume mode=crew (enables coop --resume), got %q",
			resumeBeads[0].Fields["mode"])
	}
}
