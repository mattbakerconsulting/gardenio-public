package bridge

import (
	"context"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// These tests guard ka-0000006807: role=thread agents cold-started on every
// respawn because respawnThreadAgent CreateBead'd a NEW agent bead id each
// time, and coop's S3 resume prefix is composed from KD_AGENT_ID = the agent
// bead id. The new pod probed an EMPTY <new-id>/ prefix → archive-empty → cold
// start, while the prior transcript sat under the PRIOR bead's id. The fix
// (option B, the thread-path twin of the wakePlanAssistant fix ka-0000002360)
// REVIVES the prior bead in place — reopening the SAME id so the S3 prefix is
// stable and the conversation resumes. These tests are the CI-enforced
// regression guard mirroring the sibling's e2e scenario 2.

// findResumeAgentBeads returns the agent beads carrying the resume spawn_source.
func findResumeAgentBeads(daemon *mockDaemon) []*beadsapi.BeadDetail {
	var out []*beadsapi.BeadDetail
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "slack-thread-resume" {
			out = append(out, b)
		}
	}
	return out
}

// TestRespawnThreadAgent_RevivesClosedPriorBeadInPlace covers Case A: the prior
// thread agent bead is already CLOSED (reaper/self-close). The respawn must
// reopen THAT bead (same id) rather than mint a fresh one, so KD_AGENT_ID —
// coop's S3 resume prefix — stays constant across the respawn.
func TestRespawnThreadAgent_RevivesClosedPriorBeadInPlace(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-REV", "thread")

	const agentName = "thread-1234-5678"
	const priorBeadID = "gd-PRIOR-transcript"
	// A CLOSED prior bead keyed by a distinct id (its own id is the S3 prefix
	// with the transcript). FindAgentBead (active-only) misses this — the revive
	// resolves it via FindAgentBeadAnyStatus.
	daemon.mu.Lock()
	daemon.beads[priorBeadID] = &beadsapi.BeadDetail{
		ID:     priorBeadID,
		Title:  agentName,
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":           agentName,
			"project":         "monorepo",
			"agent_state":     "done",
			"last_session_id": "sess-prior-uuid",
			// A prior life that self-closed via `gb stop` leaves stop_requested
			// set; the revive MUST clear it or shouldExitBeforeStart re-closes the
			// resumed pod at boot (dropping the operator's reply).
			"stop_requested": "true",
			"close_reason":   "context_cancelled",
			"wrapup":         `{"tldr":"stale prior-turn wrapup"}`,
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var called bool
	// Pool miss → cold-resume path (the revive under test).
	ctrlSrv := fakeControllerForAssign(t, false, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	bot.respawnThreadAgent(context.Background(), "C-REV", "1234.5678",
		agentName, "follow-up question", "U-HUMAN")

	// Exactly one resume agent bead, and it is the SAME prior bead reopened —
	// no second bead minted, id unchanged (stable S3 resume key).
	resume := findResumeAgentBeads(daemon)
	if len(resume) != 1 {
		t.Fatalf("expected exactly 1 resume agent bead (revive in place), got %d", len(resume))
	}
	if resume[0].ID != priorBeadID {
		t.Fatalf("resume bead id = %q, want %q — revive must reuse the prior id so coop's S3 resume key is stable",
			resume[0].ID, priorBeadID)
	}
	if resume[0].Status != "in_progress" {
		t.Errorf("revived bead status = %q, want in_progress (closed→in_progress is the SSE re-spawn trigger)", resume[0].Status)
	}
	// agent_state must be reset to spawning (not left terminal), both to re-arm
	// the boot-race guard and to clear any terminal budget/rate state that would
	// otherwise divert the SSE watcher into the auth-failover branch.
	if resume[0].Fields["agent_state"] != "spawning" {
		t.Errorf("revived bead agent_state = %q, want spawning", resume[0].Fields["agent_state"])
	}
	// The thread binding fields must be (re)stamped on the revived bead.
	if resume[0].Fields["slack_thread_ts"] != "1234.5678" {
		t.Errorf("revived bead slack_thread_ts = %q, want 1234.5678", resume[0].Fields["slack_thread_ts"])
	}
	if resume[0].Fields["operated_by"] == "" {
		t.Errorf("revived bead operated_by should be stamped from the waking operator")
	}
	// The prior life's terminal-close markers must be cleared so the resumed
	// pod boots like a fresh spawn. stop_requested is the critical one:
	// shouldExitBeforeStart (cmd/gb/init_agent.go) exits the pod on the literal
	// "true", so a surviving stop_requested silently drops the operator's reply.
	if resume[0].Fields["stop_requested"] == "true" {
		t.Errorf("revived bead still has stop_requested=true — the resumed pod would exit at boot and drop the reply")
	}
	if _, ok := resume[0].Fields["close_reason"]; ok {
		t.Errorf("revived bead should have close_reason cleared, got %q", resume[0].Fields["close_reason"])
	}
	if _, ok := resume[0].Fields["wrapup"]; ok {
		t.Errorf("revived bead should have wrapup cleared, got %q", resume[0].Fields["wrapup"])
	}
	// The prior turn's wrap-up must be ARCHIVED to prior_wrapup, not destroyed
	// (ka-0000003692). The anti-stale intent (wrapup cleared, above) survives,
	// but the card can now fall back to the previous turn's summary instead of
	// rendering blank for the whole resumed turn.
	if resume[0].Fields["prior_wrapup"] != `{"tldr":"stale prior-turn wrapup"}` {
		t.Errorf("revived bead should archive the outgoing wrapup to prior_wrapup, got %q", resume[0].Fields["prior_wrapup"])
	}
	// Revive-in-place reopens the SAME bead id, so the neutral succession
	// pointers must NOT be stamped — a bead pointing at itself is a false
	// one-node lineage (task ka-0000006931). The reopened bead keeps whatever
	// lineage it was first minted with (here, none).
	if _, ok := resume[0].Fields["prior_agent_id"]; ok {
		t.Errorf("revive-in-place must not self-stamp prior_agent_id, got %q", resume[0].Fields["prior_agent_id"])
	}
	if _, ok := resume[0].Fields["lineage_root_id"]; ok {
		t.Errorf("revive-in-place must not self-stamp lineage_root_id, got %q", resume[0].Fields["lineage_root_id"])
	}

	// No CreateBead of a second agent bead should have happened for the resume.
	daemon.mu.Lock()
	agentCount := 0
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["agent"] == agentName {
			agentCount++
		}
	}
	daemon.mu.Unlock()
	if agentCount != 1 {
		t.Errorf("expected exactly 1 agent bead for %s after revive, got %d (a fresh id was minted)", agentName, agentCount)
	}
}

// TestRespawnThreadAgent_RevivesActiveTerminalPriorBeadInPlace covers Case B:
// the prior bead is still ACTIVE but in a terminal agent_state (done/failed).
// respawnThreadAgent closes the stale bead (to reap its dead pod) then must
// revive that SAME id in place on the cold-resume path.
func TestRespawnThreadAgent_RevivesActiveTerminalPriorBeadInPlace(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-REV2", "thread")

	const agentName = "thread-2222-3333"
	const priorBeadID = "gd-ACTIVE-terminal"
	// Keyed by agent NAME so FindAgentBead (active lookup) resolves it and the
	// stale-terminal-close path fires; agent_state=done makes it terminal.
	daemon.mu.Lock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     priorBeadID,
		Title:  agentName,
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":       agentName,
			"project":     "monorepo",
			"agent_state": "done",
		},
	}
	daemon.mu.Unlock()

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

	bot.respawnThreadAgent(context.Background(), "C-REV2", "2222.3333",
		agentName, "another follow-up", "U-HUMAN")

	resume := findResumeAgentBeads(daemon)
	if len(resume) != 1 {
		t.Fatalf("expected exactly 1 resume agent bead (revive in place), got %d", len(resume))
	}
	if resume[0].ID != priorBeadID {
		t.Fatalf("resume bead id = %q, want %q — revive must reuse the prior id", resume[0].ID, priorBeadID)
	}
	if resume[0].Status != "in_progress" {
		t.Errorf("revived bead status = %q, want in_progress", resume[0].Status)
	}
	if resume[0].Fields["agent_state"] != "spawning" {
		t.Errorf("revived bead agent_state = %q, want spawning", resume[0].Fields["agent_state"])
	}
}

// TestRespawnThreadAgent_ColdMintWhenNoPriorBead covers the fallback: with NO
// prior agent bead to revive, respawnThreadAgent must still mint a fresh resume
// bead (never dead-end the wake-up).
func TestRespawnThreadAgent_ColdMintWhenNoPriorBead(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-REV3", "thread")

	// A prior bead is required only to resolve the project; seed it keyed by a
	// DIFFERENT agent name so the respawn's own name has nothing to revive.
	seedPriorAgentBead(daemon, "some-other-agent", "monorepo")

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

	const agentName = "thread-4444-5555"
	bot.respawnThreadAgent(context.Background(), "C-REV3", "4444.5555",
		agentName, "fresh follow-up", "U-HUMAN")

	resume := findResumeAgentBeads(daemon)
	if len(resume) != 1 {
		t.Fatalf("expected exactly 1 resume agent bead (fresh mint fallback), got %d", len(resume))
	}
	if resume[0].Fields["agent"] != agentName {
		t.Errorf("resume bead agent = %q, want %q", resume[0].Fields["agent"], agentName)
	}
	// The fresh mint uses the daemon-generated id (mock: bd-chat-*), not a
	// pre-seeded one — proving CreateBead ran.
	if resume[0].Status != "open" {
		t.Errorf("freshly-minted resume bead status = %q, want open", resume[0].Status)
	}
}

// TestRespawnThreadAgent_PreservesWrapupToPriorWrapup is the focused
// preserve-on-resume guard (ka-0000003692): the outgoing wrapup is copied to
// prior_wrapup AND the live wrapup is still cleared (the anti-stale intent must
// survive the fix).
func TestRespawnThreadAgent_PreservesWrapupToPriorWrapup(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-PW", "thread")

	const agentName = "thread-9100-9200"
	const priorBeadID = "gd-PRIOR-preserve"
	const priorWrapup = `{"tldr":"Merged MR 1536; closed 3 beads","accomplishments":"real work"}`
	seedPriorAgentBeadWithID(daemon, agentName, "monorepo", priorBeadID, map[string]string{
		"agent_state": "done",
		"wrapup":      priorWrapup,
	})

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

	bot.respawnThreadAgent(context.Background(), "C-PW", "9100.9200",
		agentName, "follow-up", "U-HUMAN")

	resume := findResumeAgentBeads(daemon)
	if len(resume) != 1 {
		t.Fatalf("expected exactly 1 resume agent bead, got %d", len(resume))
	}
	if resume[0].Fields["prior_wrapup"] != priorWrapup {
		t.Errorf("prior_wrapup = %q, want the archived outgoing wrapup %q", resume[0].Fields["prior_wrapup"], priorWrapup)
	}
	// The anti-stale invariant: the live wrapup is STILL cleared.
	if _, ok := resume[0].Fields["wrapup"]; ok {
		t.Errorf("wrapup must still be cleared after the archive, got %q", resume[0].Fields["wrapup"])
	}
}

// TestRespawnThreadAgent_ChainedResumeDoesNotClobberPriorWrapup guards the
// chained-resume shape (gd-faDicZTg2m, session_count>2): when THIS turn ended
// without writing a fresh wrap-up, the empty outgoing wrapup must NOT overwrite
// a prior_wrapup preserved from an earlier turn.
func TestRespawnThreadAgent_ChainedResumeDoesNotClobberPriorWrapup(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-CH", "thread")

	const agentName = "thread-7700-7800"
	const priorBeadID = "gd-PRIOR-chained"
	const earlierWrapup = `{"tldr":"Turn 1: shipped the schema","accomplishments":"turn one"}`
	// Turn 2 ended with NO wrapup of its own, but turn 1's wrap-up was already
	// archived to prior_wrapup. The empty outgoing wrapup must be a no-op.
	seedPriorAgentBeadWithID(daemon, agentName, "monorepo", priorBeadID, map[string]string{
		"agent_state":   "done",
		"prior_wrapup":  earlierWrapup,
		"session_count": "3",
		// no "wrapup" field — turn 2 wrote none.
	})

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

	bot.respawnThreadAgent(context.Background(), "C-CH", "7700.7800",
		agentName, "third follow-up", "U-HUMAN")

	resume := findResumeAgentBeads(daemon)
	if len(resume) != 1 {
		t.Fatalf("expected exactly 1 resume agent bead, got %d", len(resume))
	}
	// The earlier preserved wrap-up must survive — NOT clobbered to "".
	if resume[0].Fields["prior_wrapup"] != earlierWrapup {
		t.Errorf("prior_wrapup = %q, want the earlier turn's preserved wrap-up %q (empty outgoing wrapup must not clobber it)",
			resume[0].Fields["prior_wrapup"], earlierWrapup)
	}
}
