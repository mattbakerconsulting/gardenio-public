package bridge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// These tests lock in the respawn half of the --model fix. respawnThreadAgent
// mints a NEW pod, so it must (1) carry forward the prior agent's --model
// override so a `--model fable` thread keeps its model across restart/resume,
// and (2) honor a freshly-typed --model on the wake-up (which WINS over the
// prior) while stripping the flag token from the resume Description.

// seedPriorAgentBeadWithModel is seedPriorAgentBead plus a stamped model field,
// mirroring what handleThreadSpawn (or a prior respawn) leaves on the bead.
func seedPriorAgentBeadWithModel(daemon *mockDaemon, agentName, project, model string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName+"-prior"] = &beadsapi.BeadDetail{
		ID:     agentName + "-prior",
		Title:  agentName,
		Type:   "agent",
		Status: "closed",
		// Non-zero CreatedAt so priorAgentBead's most-recent selector
		// (bead.CreatedAt.After(zeroTime)) picks this bead.
		CreatedAt: time.Now(),
		Fields: map[string]string{
			"agent":   agentName,
			"project": project,
			"model":   model,
		},
	}
}

// TestRespawnThreadAgent_CarriesForwardPriorModel verifies a thread originally
// spawned with a --model override keeps that model when its pod is respawned
// (no new --model typed). The prior model also SKIPS the warm pool — a pooled
// pod bakes its model at create time — so the cold-resume bead must carry it.
func TestRespawnThreadAgent_CarriesForwardPriorModel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	seedPriorAgentBeadWithModel(daemon, "thread-9999-5555", "gardenio", "claude-fable-5")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var called bool
	// hit=true: if the pool were consulted it would assign — but a prior model
	// must SKIP the pool, so we assert it is never called.
	ctrlSrv := fakeControllerForAssign(t, true, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-GARDENIO", "9999.5555",
		"thread-9999-5555", "keep going", "U-HUMAN")

	mu.Lock()
	wasCalled := called
	mu.Unlock()
	if wasCalled {
		t.Error("expected pool assign to be SKIPPED when a prior --model is set (pooled pod can't honor it)")
	}

	var resumeBeads []*beadsapi.BeadDetail
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.Fields["spawn_source"] == "slack-thread-resume" {
			resumeBeads = append(resumeBeads, b)
		}
	}
	if len(resumeBeads) != 1 {
		t.Fatalf("expected 1 cold-resume agent bead, got %d", len(resumeBeads))
	}
	if got := resumeBeads[0].Fields["model"]; got != "claude-fable-5" {
		t.Errorf("expected carried-forward model=claude-fable-5, got %q", got)
	}
}

// TestRespawnThreadAgent_FreshModelWinsAndIsStripped verifies a --model typed on
// the wake-up (a) overrides the prior model, (b) is resolved to its canonical
// alias-KEY id and stamped on the resume bead, and (c) is stripped from the
// resume Description so the flag token does not leak into the agent's prompt.
func TestRespawnThreadAgent_FreshModelWinsAndIsStripped(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	seedPriorAgentBeadWithModel(daemon, "thread-9999-7777", "gardenio", "claude-fable-5")

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

	bot.respawnThreadAgent(context.Background(), "C-GARDENIO", "9999.7777",
		"thread-9999-7777", "--model opus keep going", "U-HUMAN")

	mu.Lock()
	wasCalled := called
	mu.Unlock()
	if wasCalled {
		t.Error("expected pool assign to be SKIPPED when a --model override is present")
	}

	var resumeBeads []*beadsapi.BeadDetail
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.Fields["spawn_source"] == "slack-thread-resume" {
			resumeBeads = append(resumeBeads, b)
		}
	}
	if len(resumeBeads) != 1 {
		t.Fatalf("expected 1 cold-resume agent bead, got %d", len(resumeBeads))
	}
	bead := resumeBeads[0]
	if got := bead.Fields["model"]; got != "claude-opus-5-5" {
		t.Errorf("expected fresh --model opus to win as claude-opus-5-5, got %q", got)
	}
	// The flag token must not survive into the description the agent reads.
	if strings.Contains(bead.Description, "--model") {
		t.Errorf("expected --model token stripped from resume description, got:\n%s", bead.Description)
	}
	if !strings.Contains(bead.Description, "keep going") {
		t.Errorf("expected remaining prompt text preserved in resume description, got:\n%s", bead.Description)
	}
}
