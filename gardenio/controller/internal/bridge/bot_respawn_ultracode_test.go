package bridge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// These tests lock in the respawn half of the --ultracode fix. respawnThreadAgent
// mints a NEW pod, so it must carry forward the prior agent's ultracode field
// (so a `--ultracode=off` thread stays disabled across restart/resume) and honor
// a freshly-typed --ultracode on the wake-up (which WINS over the prior).

func seedPriorAgentBeadWithUltracode(daemon *mockDaemon, agentName, project, ultracode string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName+"-prior"] = &beadsapi.BeadDetail{
		ID:        agentName + "-prior",
		Title:     agentName,
		Type:      "agent",
		Status:    "closed",
		CreatedAt: time.Now(),
		Fields: map[string]string{
			"agent":     agentName,
			"project":   project,
			"ultracode": ultracode,
		},
	}
}

// TestRespawnThreadAgent_CarriesForwardPriorUltracodeOff is the key regression:
// a thread spawned with --ultracode=off must STAY off when its pod respawns with
// no fresh flag. If parseUltracodeFlag ever returned "off" (not "") for an absent
// flag, or the carry-forward were dropped, the pod would silently re-enable
// ultracode on resume. The prior "off" also SKIPS the warm pool (a pooled pod
// baked its settings.json at create time), so we assert the pool is never hit.
func TestRespawnThreadAgent_CarriesForwardPriorUltracodeOff(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	seedPriorAgentBeadWithUltracode(daemon, "thread-8888-1111", "gardenio", "off")

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

	bot.respawnThreadAgent(context.Background(), "C-GARDENIO", "8888.1111",
		"thread-8888-1111", "keep going", "U-HUMAN")

	mu.Lock()
	wasCalled := called
	mu.Unlock()
	if wasCalled {
		t.Error("expected pool assign to be SKIPPED when a prior --ultracode is set (pooled pod can't honor it)")
	}

	bead := singleResumeBead(t, daemon)
	if got := bead.Fields["ultracode"]; got != "off" {
		t.Errorf("expected carried-forward ultracode=off, got %q", got)
	}
}

// TestRespawnThreadAgent_FreshUltracodeWinsAndIsStripped verifies a --ultracode
// typed on the wake-up (a) overrides the prior value and (b) is stripped from the
// resume Description so the flag token does not leak into the agent's prompt.
func TestRespawnThreadAgent_FreshUltracodeWinsAndIsStripped(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	seedPriorAgentBeadWithUltracode(daemon, "thread-8888-2222", "gardenio", "off")

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

	bot.respawnThreadAgent(context.Background(), "C-GARDENIO", "8888.2222",
		"thread-8888-2222", "--ultracode=on keep going", "U-HUMAN")

	mu.Lock()
	wasCalled := called
	mu.Unlock()
	if wasCalled {
		t.Error("expected pool assign to be SKIPPED when a --ultracode override is present")
	}

	bead := singleResumeBead(t, daemon)
	if got := bead.Fields["ultracode"]; got != "on" {
		t.Errorf("expected fresh --ultracode=on to win over prior off, got %q", got)
	}
	if strings.Contains(bead.Description, "--ultracode") {
		t.Errorf("expected --ultracode token stripped from resume description, got:\n%s", bead.Description)
	}
	if !strings.Contains(bead.Description, "keep going") {
		t.Errorf("expected remaining prompt text preserved in resume description, got:\n%s", bead.Description)
	}
}

// singleResumeBead returns the sole cold-resume agent bead, failing if there is
// not exactly one.
func singleResumeBead(t *testing.T, daemon *mockDaemon) *beadsapi.BeadDetail {
	t.Helper()
	var resumeBeads []*beadsapi.BeadDetail
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.Fields["spawn_source"] == "slack-thread-resume" {
			resumeBeads = append(resumeBeads, b)
		}
	}
	if len(resumeBeads) != 1 {
		t.Fatalf("expected 1 cold-resume agent bead, got %d", len(resumeBeads))
	}
	return resumeBeads[0]
}
