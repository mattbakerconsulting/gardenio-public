package bridge

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// These tests close the ka-0000004735 gap: a thread agent's session-resume
// respawn used to one-shot-fail its agent-bead write to a Slack dead-end
// (":x: Failed to resume agent"). A single transient daemon blip — a 5xx /
// connection-refused during a daemon/bridge roll (release rollout, node churn)
// — dropped the operator's wake-up on the floor. That is exactly how
// thread-1784843199-442769 failed to resume during the 2026.204.17 deploy. The
// fix wraps the write in retryTransientDaemon (the same helper the
// conversation-bubble write uses).
//
// Post-ka-0000006807 the resume write REVIVES the prior thread agent bead in
// place (UpdateBead) whenever a prior bead exists — keeping KD_AGENT_ID (=
// coop's S3 resume prefix) stable — and only falls back to CreateBead (with a
// per-wake-up idempotency key) when no prior bead is found. So the retry
// coverage is split across both writes:
//   - TestRespawnThreadAgent_RetriesTransientRevive: the revive UpdateBead.
//   - TestRespawnThreadAgent_TransientRetryDoesNotDoubleMint: the create
//     fallback (no prior bead) still retries + dedups on a lost response.

// TestRespawnThreadAgent_RetriesTransientRevive verifies that a transient 5xx
// on the FIRST revive-in-place UpdateBead is retried, so the operator's wake-up
// resumes the SAME prior agent bead (stable S3 key) instead of dead-ending on
// the failure message.
func TestRespawnThreadAgent_RetriesTransientRevive(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-RETRY", "thread")

	const agentName = "thread-8888-1111"
	const priorBeadID = "bd-agent-retry"
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
		Notes: "backend: coop\ncoop_url: http://127.0.0.1:9999",
	}
	daemon.mu.Unlock()

	// Fail only the FIRST revive attempt with a transient 5xx.
	failedOnce := false
	daemon.updateBeadHook = func(_ int, beadID string, req beadsapi.UpdateBeadRequest) error {
		if beadID == priorBeadID && req.Status != nil && *req.Status == "in_progress" && !failedOnce {
			failedOnce = true
			return &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
		}
		return nil
	}

	dir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.SetThreadAgent("C-RETRY", "2222.0000", agentName); err != nil {
		t.Fatal(err)
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	// Pool assign misses (404) so the respawn falls through to the cold
	// session-resume path — the revive UpdateBead under test.
	var gotRole string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerForAssign(t, false, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = sm
	b.botUserID = "U-BOT"
	b.controllerURL = ctrlSrv.URL

	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Text:            "<@U-BOT> make it so",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "2222.0000",
		Channel:         "C-RETRY",
	})

	if !failedOnce {
		t.Fatal("expected the injected transient failure to fire on the first revive attempt")
	}

	// The retry must have revived the PRIOR bead in place — same id (stable S3
	// resume key), reopened to in_progress, marked as a resume, and NO second
	// agent bead minted.
	daemon.mu.Lock()
	prior := daemon.beads[agentName]
	daemon.mu.Unlock()
	if prior.ID != priorBeadID {
		t.Fatalf("prior bead id changed: got %q, want %q (revive must keep the id stable)", prior.ID, priorBeadID)
	}
	if prior.Status != "in_progress" {
		t.Errorf("prior bead status = %q, want in_progress (revive reopens in place)", prior.Status)
	}
	if prior.Fields["spawn_source"] != "slack-thread-resume" {
		t.Errorf("prior bead spawn_source = %q, want slack-thread-resume", prior.Fields["spawn_source"])
	}

	var resumeBeads []*beadsapi.BeadDetail
	for _, bead := range filterAgentBeads(daemon.beads) {
		if bead.Fields["spawn_source"] == "slack-thread-resume" {
			resumeBeads = append(resumeBeads, bead)
		}
	}
	if len(resumeBeads) != 1 {
		t.Fatalf("expected exactly 1 resume agent bead after transient retry (revive in place), got %d", len(resumeBeads))
	}
	if resumeBeads[0].ID != priorBeadID {
		t.Errorf("resume bead id = %q, want %q (same bead revived, not a fresh mint)", resumeBeads[0].ID, priorBeadID)
	}
}

// TestRespawnThreadAgent_TransientRetryDoesNotDoubleMint proves the create
// FALLBACK (taken when no prior bead exists to revive) still retries on a lost
// response and its per-wake-up idempotency key collapses the retry onto the one
// committed bead rather than minting a duplicate agent. We model the fallback by
// seeding NO prior agent bead for the thread's agent name, so FindAgentBeadAnyStatus
// misses and respawnThreadAgent falls through to CreateBead.
func TestRespawnThreadAgent_TransientRetryDoesNotDoubleMint(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-DEDUP", "thread")

	const agentName = "thread-9999-2222"
	// NOTE: intentionally NO prior agent bead seeded under agentName — this is
	// what routes respawn to the CreateBead fallback (no bead to revive). The
	// thread mapping alone drives the respawn.

	// Persist-then-fail on the first respawn mint: the mock records the bead
	// (as the real daemon would on a committed-but-lost-response write) and
	// THEN returns a transient error, so the bridge retries. The retry must
	// hit the idempotency lookup-or-return and NOT insert a second agent bead.
	var respawnKey string
	firstDone := false
	daemon.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if !strings.HasPrefix(req.IdempotencyKey, "thread-respawn:") {
			return nil
		}
		if !firstDone {
			firstDone = true
			respawnKey = req.IdempotencyKey
			// Simulate the write landing at the daemon (idempotency-keyed) even
			// though the client sees a transport failure. Insert under the key
			// so the retry dedups against it.
			daemon.beads["bd-lost-response"] = &beadsapi.BeadDetail{
				ID:     "bd-lost-response",
				Title:  req.Title,
				Type:   req.Type,
				Status: "open",
				Labels: req.Labels,
				Fields: map[string]string{
					"idempotency_key": req.IdempotencyKey,
					"agent":           agentName,
					"spawn_source":    "slack-thread-resume",
				},
				Description: req.Description,
			}
			return &beadsapi.APIError{StatusCode: http.StatusBadGateway, Message: "lost response"}
		}
		return nil
	}

	dir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.SetThreadAgent("C-DEDUP", "3333.0000", agentName); err != nil {
		t.Fatal(err)
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerForAssign(t, false, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = sm
	b.botUserID = "U-BOT"
	b.controllerURL = ctrlSrv.URL

	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Text:            "<@U-BOT> make it so",
		TimeStamp:       "3333.4444",
		ThreadTimeStamp: "3333.0000",
		Channel:         "C-DEDUP",
	})

	if !firstDone {
		t.Fatal("expected the first respawn mint attempt to fire (create fallback)")
	}
	if respawnKey == "" {
		t.Fatal("expected a non-empty per-wake-up respawn idempotency key")
	}

	// Exactly ONE agent bead carrying the resume spawn_source must exist — the
	// lost-response bead, reused by the retry, not a duplicate.
	var resumeBeads []*beadsapi.BeadDetail
	for _, bead := range filterAgentBeads(daemon.beads) {
		if bead.Fields["spawn_source"] == "slack-thread-resume" {
			resumeBeads = append(resumeBeads, bead)
		}
	}
	if len(resumeBeads) != 1 {
		t.Fatalf("expected exactly 1 resume agent bead (idempotency dedup), got %d", len(resumeBeads))
	}
}
