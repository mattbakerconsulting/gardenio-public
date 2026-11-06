package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

// These tests close the C4 confidence gap (gd-aNKxcKp0wW): the role plumbing
// landed in d9d59265f (gd-NIpnhNRe4F), but no test exercised the FAST
// pool-assign path end-to-end — the existing router tests leave controllerURL
// empty, so tryPoolAssign short-circuits and only cold-start is covered. Here
// we stand up a fake controller for /api/v1/pool/assign and assert:
//
//  1. the channel_roles-resolved role is actually transmitted in the assign
//     request body (so a gvisor-poc/runsc channel gets a role-matched warm pod,
//     not a generic one — the Phase 5 headline gap, plan doc C4); and
//  2. a pool miss (404) falls through to cold start with the same role applied
//     to the freshly-created agent bead.

// fakeControllerForAssign returns an httptest server emulating the controller's
// POST /api/v1/pool/assign. It records the decoded request body for assertions.
// When assignOK is true it returns a 200 with a synthetic assignment; otherwise
// it returns 404 (ErrPoolEmpty shape) so the caller falls through to cold start.
func fakeControllerForAssign(t *testing.T, assignOK bool, gotRole *string, mu *sync.Mutex, called *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pool/assign" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Channel     string `json:"channel"`
			ThreadTS    string `json:"thread_ts"`
			Description string `json:"description"`
			Project     string `json:"project"`
			Role        string `json:"role"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		*gotRole = req.Role
		*called = true
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if !assignOK {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no prewarmed agents available"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"bead_id":    "gd-pooled-1",
			"agent_name": "monorepo-k6-perf-warm-1",
		})
	}))
}

// TestTryPoolAssign_FastPath_SendsChannelRole verifies the fast pool-assign
// path transmits the channel_roles-resolved role over the wire, so the pool can
// hand back a role/runtime-matched warm pod instead of a generic one.
func TestTryPoolAssign_FastPath_SendsChannelRole(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-K6", "k6-perf")

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

	ev := &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Channel:         "C-K6",
		ThreadTimeStamp: "9999.1111",
		TimeStamp:       "9999.2222",
		Text:            "<@U-BOT> run perf tests",
	}

	bot.handleThreadSpawn(context.Background(), ev, "run perf tests")

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatal("expected fast pool-assign path to call /api/v1/pool/assign")
	}
	if gotRole != "k6-perf" {
		t.Errorf("expected role=k6-perf transmitted to pool assign, got %q", gotRole)
	}
	// On a pool HIT, no cold-start agent bead should be created.
	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Errorf("expected 0 cold-start agent beads on pool hit, got %d", n)
	}
}

// TestTryPoolAssign_PoolMiss_FallsThroughToColdStart verifies a 404 pool miss
// degrades gracefully: the same channel_roles-resolved role is still applied to
// the cold-started agent bead.
func TestTryPoolAssign_PoolMiss_FallsThroughToColdStart(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-K6", "k6-perf")

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

	ev := &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Channel:         "C-K6",
		ThreadTimeStamp: "9999.3333",
		TimeStamp:       "9999.4444",
		Text:            "<@U-BOT> run perf tests",
	}

	bot.handleThreadSpawn(context.Background(), ev, "run perf tests")

	mu.Lock()
	transmitted := gotRole
	wasCalled := called
	mu.Unlock()

	if !wasCalled {
		t.Fatal("expected the fast path to attempt pool assign before falling through")
	}
	if transmitted != "k6-perf" {
		t.Errorf("expected role=k6-perf transmitted on the attempted assign, got %q", transmitted)
	}

	// On a pool MISS, cold start must create exactly one agent bead carrying the
	// resolved role.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 cold-start agent bead on pool miss, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["role"] != "k6-perf" {
			t.Errorf("expected cold-start role=k6-perf, got %q", b.Fields["role"])
		}
		if b.Fields["spawn_source"] != "slack-thread" {
			t.Errorf("expected spawn_source=slack-thread on cold start, got %q", b.Fields["spawn_source"])
		}
	}
}

// TestTryPoolAssign_HitHydratesWarmFields closes gap G4 (ka-0000001120): the
// warm pool-assign path must backfill slack_spawn_channel, role, and
// slack_opener_user_id onto the assigned agent bead so it matches a cold-minted
// one. Without this hydration, thread-card ownership failed closed and
// role/channel-dependent behavior degraded on every warm thread.
func TestTryPoolAssign_HitHydratesWarmFields(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-K6", "k6-perf")

	// Seed the bead the fake controller hands back on a HIT so the bridge's
	// post-assign UpdateBeadFields lands somewhere assertable.
	daemon.mu.Lock()
	daemon.beads["gd-pooled-1"] = &beadsapi.BeadDetail{
		ID:     "gd-pooled-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{"agent_state": "assigning"},
	}
	daemon.mu.Unlock()

	// A Slack server whose conversations.replies returns a thread root authored
	// by U-ROOT, so fetchThreadContext resolves a non-empty rootAuthor and the
	// bridge can stamp slack_opener_user_id.
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"messages": []map[string]any{
					{"user": "U-ROOT", "text": "please run perf tests", "ts": "9999.5555"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	var gotRole string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerForAssign(t, true, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	ev := &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Channel:         "C-K6",
		ThreadTimeStamp: "9999.5555",
		TimeStamp:       "9999.6666",
		Text:            "<@U-BOT> run perf tests",
	}

	bot.handleThreadSpawn(context.Background(), ev, "run perf tests")

	mu.Lock()
	wasCalled := called
	mu.Unlock()
	if !wasCalled {
		t.Fatal("expected fast pool-assign path to call /api/v1/pool/assign")
	}

	daemon.mu.Lock()
	pooled := daemon.beads["gd-pooled-1"]
	daemon.mu.Unlock()
	if pooled == nil {
		t.Fatal("pooled agent bead vanished")
	}
	if got := pooled.Fields["slack_spawn_channel"]; got != "C-K6" {
		t.Errorf("expected warm-assign slack_spawn_channel=C-K6, got %q", got)
	}
	if got := pooled.Fields["role"]; got != "k6-perf" {
		t.Errorf("expected warm-assign role=k6-perf, got %q", got)
	}
	if got := pooled.Fields["slack_opener_user_id"]; got != "U-ROOT" {
		t.Errorf("expected warm-assign slack_opener_user_id=U-ROOT, got %q", got)
	}
	// The pre-existing post-assign fields must still be stamped (no regression).
	if got := pooled.Fields["slack_user_id"]; got != "U-HUMAN" {
		t.Errorf("expected slack_user_id=U-HUMAN preserved, got %q", got)
	}
}
