package bridge

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// ── test seeding helpers ─────────────────────────────────────────────────
//
// The Slack verbosity seam (gb-ux epic ka-0000005059) defaults every recipient
// to the SIMPLE surface: lifecycle chatter is suppressed or collapsed to a
// terse line unless the owning operator has opted into vibe mode. These helpers
// seed the mock daemon so a given agent / Slack user resolves to vibe mode,
// letting the full-verbosity posting-mechanics tests keep covering the vibe
// path.

// enableVibeForAgent seeds the mock so vibeModeForAgent(agentName) resolves
// true: a vibe-mode user bead plus an agent bead (keyed by the plain agent name
// FindAgentBead receives) stamped with operated_by pointing at that user. If an
// agent bead is already seeded (e.g. a thread-bound agent), its operated_by is
// stamped in place so existing thread bindings are preserved.
func enableVibeForAgent(daemon *mockDaemon, agentName string) {
	email := agentName + "@vibe.example"
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads["user-vibe-"+agentName] = &beadsapi.BeadDetail{
		ID:     "user-vibe-" + agentName,
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"email":       email,
			"preferences": `{"vibe_mode":"true"}`,
		},
	}
	if existing, ok := daemon.beads[agentName]; ok {
		if existing.Fields == nil {
			existing.Fields = map[string]string{}
		}
		existing.Fields["operated_by"] = "oidc:" + email
		return
	}
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     "agent-" + agentName,
		Type:   "agent",
		Status: "open",
		Title:  agentName,
		Fields: map[string]string{
			"agent":       agentName,
			"operated_by": "oidc:" + email,
		},
	}
}

// enableVibeForAccountEvent seeds an agent bead (by the id an account rotation
// event carries in evt.AgentBead) stamped with a vibe-mode operator, so
// vibeModeForAccountEvent resolves true. Callers must also set evt.AgentBead to
// agentBeadID.
func enableVibeForAccountEvent(daemon *mockDaemon, agentBeadID string) {
	email := agentBeadID + "@vibe.example"
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentBeadID] = &beadsapi.BeadDetail{
		ID: agentBeadID, Type: "agent", Status: "open",
		Fields: map[string]string{"operated_by": "oidc:" + email},
	}
	daemon.beads["user-acct-"+agentBeadID] = &beadsapi.BeadDetail{
		ID: "user-acct-" + agentBeadID, Type: "user", Kind: "config", Status: "open",
		Fields: map[string]string{"email": email, "preferences": `{"vibe_mode":"true"}`},
	}
}

// enableVibeForSlackUser seeds a vibe-mode user bead resolvable by Slack id, so
// vibeModeForSlackUser(slackID) resolves true (used on the DM path).
func enableVibeForSlackUser(daemon *mockDaemon, slackID string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads["user-vibe-slack-"+slackID] = &beadsapi.BeadDetail{
		ID:     "user-vibe-slack-" + slackID,
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":    slackID,
			"email":       slackID + "@vibe.example",
			"preferences": `{"vibe_mode":"true"}`,
		},
	}
}

// ── parse-layer unit tests ───────────────────────────────────────────────

func TestParseVibeModeFromPreferences(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty", "", false},
		{"missing-key", `{"spawn_per_selection":"single"}`, false},
		{"native-true", `{"vibe_mode":true}`, true},
		{"native-false", `{"vibe_mode":false}`, false},
		{"string-true", `{"vibe_mode":"true"}`, true}, // gb prefs set … true
		{"string-false", `{"vibe_mode":"false"}`, false},
		{"string-one", `{"vibe_mode":"1"}`, true},
		{"string-zero", `{"vibe_mode":"0"}`, false},
		{"empty-string", `{"vibe_mode":""}`, false},
		{"null", `{"vibe_mode":null}`, false},
		{"garbage-string", `{"vibe_mode":"yes"}`, false}, // unparseable → simple
		{"malformed-json", `{not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVibeModeFromPreferences(tc.raw); got != tc.want {
				t.Errorf("parseVibeModeFromPreferences(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestOIDCEmailFromOperator(t *testing.T) {
	cases := []struct {
		in        string
		wantEmail string
		wantOK    bool
	}{
		{"oidc:matt@acme.example", "matt@acme.example", true},
		{"oidc:", "", false},
		{"slack:U123", "", false},
		{"", "", false},
		{"matt@acme.example", "", false},
	}
	for _, tc := range cases {
		email, ok := oidcEmailFromOperator(tc.in)
		if email != tc.wantEmail || ok != tc.wantOK {
			t.Errorf("oidcEmailFromOperator(%q) = (%q,%v), want (%q,%v)",
				tc.in, email, ok, tc.wantEmail, tc.wantOK)
		}
	}
}

// ── resolver tests ───────────────────────────────────────────────────────

func TestVibeModeForAgent_ResolvesViaOperatedBy(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	// Unknown agent → simple (default).
	if bot.vibeModeForAgent(context.Background(), "ghost-agent") {
		t.Error("unknown agent must resolve simple (false)")
	}

	// Agent with operated_by → vibe user bead → true.
	enableVibeForAgent(daemon, "vibe-agent")
	if !bot.vibeModeForAgent(context.Background(), "vibe-agent") {
		t.Error("agent owned by a vibe operator must resolve true")
	}

	// The "gardenio/crew/<name>" identity form must resolve the same agent.
	if !bot.vibeModeForAgent(context.Background(), "gardenio/crew/vibe-agent") {
		t.Error("prefixed agent identity must resolve via extractAgentName")
	}
}

func TestVibeModeForAgent_SimpleWhenOperatorNotVibe(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	// Agent stamped with an operator whose user bead has no vibe_mode.
	daemon.beads["plain-agent"] = &beadsapi.BeadDetail{
		ID: "agent-plain", Type: "agent", Status: "open", Title: "plain-agent",
		Fields: map[string]string{"operated_by": "oidc:plain@example.com"},
	}
	daemon.beads["user-plain"] = &beadsapi.BeadDetail{
		ID: "user-plain", Type: "user", Kind: "config", Status: "open",
		Fields: map[string]string{"email": "plain@example.com", "preferences": `{}`},
	}
	if bot.vibeModeForAgent(context.Background(), "plain-agent") {
		t.Error("operator without vibe_mode must resolve simple")
	}
}

func TestVibeModeForSlackUser(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	// Seed before the first lookup: the directory caches on first access and
	// only auto-refreshes when stale (production stays fresh via SSE upserts).
	enableVibeForSlackUser(daemon, "U-VIBE")

	if bot.vibeModeForSlackUser(context.Background(), "") {
		t.Error("empty slack id must resolve simple")
	}
	if bot.vibeModeForSlackUser(context.Background(), "U-UNKNOWN") {
		t.Error("unknown slack user must resolve simple")
	}
	if !bot.vibeModeForSlackUser(context.Background(), "U-VIBE") {
		t.Error("vibe-mode slack user must resolve true")
	}
}

// ── simple-default (suppression / terse) behaviour ───────────────────────

func TestNotifyJackOn_SimpleModeSuppressed(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	// No vibe seeding → simple default → suppressed.
	if err := bot.NotifyJackOn(context.Background(), BeadEvent{
		ID: "bd-jack-simple", Assignee: "deploy-agent",
		Fields: map[string]string{"target": "prod-db"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getCalls(calls, mu); len(got) != 0 {
		t.Fatalf("expected jack-on suppressed in simple mode, got %d calls", len(got))
	}
}

func TestNotifyAgentCrash_SimpleModeTerse(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	// Crash ALWAYS posts (needs a human) — but terse in simple mode: a
	// text-only message with no rich blocks.
	if err := bot.NotifyAgentCrash(context.Background(), BeadEvent{
		ID: "bd-crash-simple", Assignee: "doomed-agent",
		Fields: map[string]string{"agent_state": "failed", "pod_phase": "failed"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("crash must always post, got %d calls", len(got))
	}
	if got[0].Blocks != "" {
		t.Errorf("simple-mode crash must be text-only (no blocks), got blocks=%q", got[0].Blocks)
	}
	if !strings.Contains(got[0].Text, "Agent crashed") {
		t.Errorf("expected crash text, got %q", got[0].Text)
	}
}

// TestNotifyAgentCrash_VibeModeFullBlocks is the vibe-mode sibling of
// TestNotifyAgentCrash_SimpleModeTerse: with a vibe-mode owning operator the
// crash restores today's rich section + context blocks (the full experience),
// rather than collapsing to the terse text-only line. Together the two arms pin
// both sides of the crash verbosity seam (bot_notifications.go:126).
func TestNotifyAgentCrash_VibeModeFullBlocks(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAgent(daemon, "vibe-doomed-agent") // full-verbosity path

	if err := bot.NotifyAgentCrash(context.Background(), BeadEvent{
		ID: "bd-crash-vibe", Assignee: "vibe-doomed-agent",
		Fields: map[string]string{"agent_state": "failed", "pod_phase": "failed"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("crash must always post, got %d calls", len(got))
	}
	if got[0].Blocks == "" {
		t.Error("vibe-mode crash must carry the full section + context blocks")
	}
	// The rich block echoes the bold crash header ("Agent crashed: <name>").
	if !strings.Contains(got[0].Blocks, "Agent crashed") {
		t.Errorf("vibe-mode crash blocks must contain the crash header, got blocks=%q", got[0].Blocks)
	}
}

func TestNotifyAccountRotating_SimpleModeSuppressed(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	if err := bot.NotifyAccountRotating(context.Background(), RotationEvent{
		Event: "rotating", Project: "monorepo", FromAccount: "a", ToAccount: "b",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getCalls(calls, mu); len(got) != 0 {
		t.Fatalf("expected account-rotating suppressed in simple mode, got %d", len(got))
	}
}

func TestNotifyNoFallback_NeverPostsToSlack(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	// Cred-exhaustion alerts are fully retired from bespoke Slack posts
	// (ka-0000003084 / ka-0000005341): the whole category is routed through
	// gardenio alerts (a firing-alert bead), so NO Slack call is ever made — not
	// even for a vibe-mode operator. Bead shape/dedup/auto-close are covered by
	// the cred-exhaustion alert-bead tests in bot_notifications_test.go.
	if err := bot.NotifyNoFallback(context.Background(), RotationEvent{
		Event: "no_fallback", Project: "monorepo", FromAccount: "acct-a",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getCalls(calls, mu); len(got) != 0 {
		t.Fatalf("no-fallback must never post to Slack (routed to gardenio alerts), got %d", len(got))
	}
}

func TestNotifyBeadCreated_SimpleModeSuppressed(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)
	bot.agentCards["quiet-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "1.1"}

	// No vibe seeding → per-recipient simple → bead activity suppressed even
	// though a card thread exists.
	bot.NotifyBeadCreated(context.Background(), BeadEvent{
		ID: "bd-quiet", Type: "task", Title: "Quiet task", CreatedBy: "quiet-agent",
	})
	if got := getCalls(calls, mu); len(got) != 0 {
		t.Fatalf("expected bead-created suppressed in simple mode, got %d", len(got))
	}
}

func TestNotifyBeadClaimed_SimpleModeSuppressesPostButStreamsAlways(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()
	mockStream := &mockStreamClient{}
	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)
	bot.streamMgr = NewAgentStreamManager(mockStream, bot.logger)
	bot.agentCards["stream-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "1.1"}
	// An active streaming session is required for HandleBeadClaimed to emit a
	// task_update chunk.
	bot.streamMgr.StartSession(context.Background(), "stream-agent", "C-test", "1.1", "U-op")

	bot.NotifyBeadClaimed(context.Background(), BeadEvent{
		ID: "bd-stream", Type: "task", Title: "T", Assignee: "stream-agent",
	})

	// Slack post suppressed in simple mode …
	if got := getCalls(calls, mu); len(got) != 0 {
		t.Errorf("expected claimed Slack post suppressed in simple mode, got %d", len(got))
	}
	// … but the task_update stream chunk still fires (never gated).
	chunks := 0
	for _, c := range mockStream.getCalls() {
		if c.method == "appendChunks" {
			chunks++
		}
	}
	if chunks != 1 {
		t.Errorf("expected 1 task_update stream chunk regardless of mode, got %d", chunks)
	}
}
