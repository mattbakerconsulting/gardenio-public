package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// slackCall records a single Slack API call made to the fake server.
type slackCall struct {
	Method   string
	Channel  string
	Text     string
	ThreadTS string
	Blocks   string // raw "blocks" form value ("" when the message is text-only)
}

// capturingSlackServer returns an httptest.Server that records all Slack API
// calls (chat.postMessage, chat.update) for assertion.
func capturingSlackServer(t *testing.T) (*httptest.Server, *[]slackCall, *sync.Mutex) {
	t.Helper()
	var calls []slackCall
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		calls = append(calls, slackCall{
			Method:   strings.TrimPrefix(r.URL.Path, "/"),
			Channel:  r.FormValue("channel"),
			Text:     r.FormValue("text"),
			ThreadTS: r.FormValue("thread_ts"),
			Blocks:   r.FormValue("blocks"),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1111.2222", "message_ts": "1111.2222"})
	}))
	return srv, &calls, &mu
}

func getCalls(calls *[]slackCall, mu *sync.Mutex) []slackCall {
	mu.Lock()
	defer mu.Unlock()
	return append([]slackCall{}, *calls...)
}

// --- NotifyAgentCrash tests ---

func TestNotifyAgentCrash_Basic(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	bead := BeadEvent{
		ID:       "bd-crash-1",
		Assignee: "my-agent",
		Fields: map[string]string{
			"agent_state": "failed",
			"pod_phase":   "failed",
			"pod_name":    "agent-pod-xyz",
		},
	}

	err := bot.NotifyAgentCrash(context.Background(), bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(got))
	}
	if got[0].Method != "chat.postMessage" {
		t.Errorf("expected chat.postMessage, got %s", got[0].Method)
	}
	if got[0].Channel != "C-test" {
		t.Errorf("expected channel C-test, got %s", got[0].Channel)
	}
	if !strings.Contains(got[0].Text, "Agent crashed") {
		t.Errorf("expected text to contain 'Agent crashed', got %s", got[0].Text)
	}
}

func TestNotifyAgentCrash_FallsBackToTitle(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	bead := BeadEvent{
		ID:    "bd-crash-2",
		Title: "fallback-agent",
		Fields: map[string]string{
			"agent_state": "failed",
		},
	}

	err := bot.NotifyAgentCrash(context.Background(), bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "fallback-agent") {
		t.Errorf("expected text to contain 'fallback-agent', got %s", got[0].Text)
	}
}

func TestNotifyAgentCrash_FallsBackToID(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	bead := BeadEvent{
		ID:     "bd-crash-3",
		Fields: map[string]string{},
	}

	err := bot.NotifyAgentCrash(context.Background(), bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "bd-crash-3") {
		t.Errorf("expected text to contain bead ID, got %s", got[0].Text)
	}
}

func TestNotifyAgentCrash_ThreadBoundAgent(t *testing.T) {
	daemon := newMockDaemon()
	// Seed an agent bead with thread binding fields.
	daemon.mu.Lock()
	daemon.beads["my-thread-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-thread",
		Title: "my-thread-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "my-thread-agent",
			"slack_thread_channel": "C-thread-chan",
			"slack_thread_ts":      "9999.0001",
		},
	}
	daemon.mu.Unlock()

	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-default"

	bead := BeadEvent{
		ID:       "bd-crash-thread",
		Assignee: "my-thread-agent",
		Fields:   map[string]string{},
	}

	err := bot.NotifyAgentCrash(context.Background(), bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	if got[0].Channel != "C-thread-chan" {
		t.Errorf("expected crash posted to thread-bound channel C-thread-chan, got %s", got[0].Channel)
	}
	if got[0].ThreadTS != "9999.0001" {
		t.Errorf("expected thread_ts 9999.0001, got %s", got[0].ThreadTS)
	}
}

// TestNotifyAgentCrash_ThreadNamedAgent_DerivesThreadTS verifies the
// name-encoded fallback: when an agent is named `thread-<seconds>-<micros>`
// (spawned from a Slack thread) but the thread binding on the agent bead is
// unavailable (bead pruned, bridge restart with cold cache), the crash
// notification must still land inside the originating thread — not spam the
// top level of the resolved channel.
func TestNotifyAgentCrash_ThreadNamedAgent_DerivesThreadTS(t *testing.T) {
	daemon := newMockDaemon()
	// Intentionally do NOT seed a thread binding — simulate the cold-cache /
	// closed-bead case where resolveAgentThread returns empty.
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-default"

	bead := BeadEvent{
		ID:       "bd-crash-thread-name",
		Assignee: "thread-1776549778-624729",
		Fields: map[string]string{
			"agent_state": "failed",
			"pod_phase":   "failed",
		},
	}

	if err := bot.NotifyAgentCrash(context.Background(), bead); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(got))
	}
	if got[0].Channel != "C-default" {
		t.Errorf("expected channel C-default, got %s", got[0].Channel)
	}
	if got[0].ThreadTS != "1776549778.624729" {
		t.Errorf("expected thread_ts derived from agent name (1776549778.624729), got %q", got[0].ThreadTS)
	}
}

func TestDeriveThreadTSFromAgentName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"thread-1776549778-624729", "1776549778.624729"},
		{"thread-1234567890-111111", "1234567890.111111"},
		{"gardenio/crew/thread-1776549778-624729", "1776549778.624729"},
		{"thread-abc-def", ""},
		{"thread-1234-", ""},
		{"regular-agent-name", ""},
		{"", ""},
		{"thread-1-2-3", ""},
	}
	for _, tc := range cases {
		if got := deriveThreadTSFromAgentName(tc.in); got != tc.want {
			t.Errorf("deriveThreadTSFromAgentName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- NotifyJackOn tests ---

func TestNotifyJackOn_Basic(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAgent(daemon, "deploy-agent") // full-verbosity path

	bead := BeadEvent{
		ID:       "bd-jack-1",
		Title:    "Jack for deploy",
		Assignee: "deploy-agent",
		Fields: map[string]string{
			"target": "production-db",
			"ttl":    "30m",
			"reason": "Schema migration",
		},
	}

	err := bot.NotifyJackOn(context.Background(), bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	if got[0].Method != "chat.postMessage" {
		t.Errorf("expected chat.postMessage, got %s", got[0].Method)
	}
	if !strings.Contains(got[0].Text, "Jack raised") {
		t.Errorf("expected text to contain 'Jack raised', got %s", got[0].Text)
	}
	if !strings.Contains(got[0].Text, "production-db") {
		t.Errorf("expected text to contain target, got %s", got[0].Text)
	}
}

func TestNotifyJackOn_NoOptionalFields(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAgent(daemon, "jack-agent") // full-verbosity path

	bead := BeadEvent{
		ID:       "bd-jack-2",
		Assignee: "jack-agent",
		Fields:   map[string]string{"target": "staging-api"},
	}

	err := bot.NotifyJackOn(context.Background(), bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
}

// --- NotifyJackOnBatch tests ---

func TestNotifyJackOnBatch_Basic(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAgent(daemon, "agent-1") // one vibe owner surfaces the batch

	beads := []BeadEvent{
		{ID: "jack-1", Title: "J1", Fields: map[string]string{"target": "svc-a"}, Assignee: "agent-1"},
		{ID: "jack-2", Title: "J2", Fields: map[string]string{"target": "svc-b"}},
		{ID: "jack-3", Title: "J3", Fields: map[string]string{"target": "svc-c"}},
	}

	err := bot.NotifyJackOnBatch(context.Background(), beads)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	if got[0].Channel != "C-test" {
		t.Errorf("expected channel C-test, got %s", got[0].Channel)
	}
	if !strings.Contains(got[0].Text, "3 additional jacks raised") {
		t.Errorf("expected batch count in text, got %s", got[0].Text)
	}
}

func TestNotifyJackOnBatch_MoreThanFive(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAgent(daemon, "batch-owner") // one vibe owner surfaces the batch

	beads := make([]BeadEvent, 8)
	for i := range beads {
		beads[i] = BeadEvent{
			ID:       "jack-batch-" + string(rune('a'+i)),
			Assignee: "batch-owner",
			Fields:   map[string]string{"target": "svc"},
		}
	}

	err := bot.NotifyJackOnBatch(context.Background(), beads)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "8 additional jacks raised") {
		t.Errorf("expected 8 in text, got %s", got[0].Text)
	}
}

func TestNotifyJackOnBatch_FewerThanFive(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAgent(daemon, "batch-owner") // one vibe owner surfaces the batch

	beads := []BeadEvent{
		{ID: "j1", Assignee: "batch-owner", Fields: map[string]string{"target": "a"}},
		{ID: "j2", Fields: map[string]string{"target": "b"}},
	}

	err := bot.NotifyJackOnBatch(context.Background(), beads)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "2 additional jacks raised") {
		t.Errorf("expected 2 in text, got %s", got[0].Text)
	}
}

// --- NotifyJackOff tests ---

func TestNotifyJackOff_Basic(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAgent(daemon, "ops-agent") // full-verbosity path

	bead := BeadEvent{
		ID:       "bd-jack-off-1",
		Title:    "Jack lowered",
		Assignee: "ops-agent",
		Fields: map[string]string{
			"target": "production-db",
			"reason": "Migration complete",
		},
	}

	err := bot.NotifyJackOff(context.Background(), bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "Jack lowered") {
		t.Errorf("expected text to contain 'Jack lowered', got %s", got[0].Text)
	}
}

func TestNotifyJackOff_NoOptionalFields(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAgent(daemon, "jackoff-agent") // full-verbosity path

	bead := BeadEvent{
		ID:       "bd-jack-off-2",
		Assignee: "jackoff-agent",
		Fields:   map[string]string{"target": "staging"},
	}

	err := bot.NotifyJackOff(context.Background(), bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
}

// --- NotifyJackExpired tests ---

func TestNotifyJackExpired_Basic(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAgent(daemon, "ops-agent") // full-verbosity path

	bead := BeadEvent{
		ID:       "bd-jack-exp-1",
		Title:    "Expired jack",
		Assignee: "ops-agent",
		Fields: map[string]string{
			"target": "production-db",
			"reason": "TTL exceeded",
		},
	}

	err := bot.NotifyJackExpired(context.Background(), bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "Jack expired") {
		t.Errorf("expected text to contain 'Jack expired', got %s", got[0].Text)
	}
	if !strings.Contains(got[0].Text, "bd-jack-exp-1") {
		t.Errorf("expected text to contain bead ID, got %s", got[0].Text)
	}
}

func TestNotifyJackExpired_NoOptionalFields(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAgent(daemon, "jackexp-agent") // full-verbosity path

	bead := BeadEvent{
		ID:       "bd-jack-exp-2",
		Assignee: "jackexp-agent",
		Fields:   map[string]string{"target": "staging"},
	}

	err := bot.NotifyJackExpired(context.Background(), bead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
}

// --- Cred-exhaustion alert-bead tests (epic ka-0000003084, task ka-0000005341) ---
//
// no_fallback no longer posts to Slack; it files a firing-alert
// bead (source:alert, severity:warn) that dedups while open and auto-closes on
// account_recovered. These tests assert the bead shape, dedup, and auto-close
// contract — and that NO Slack call is made.

// openCredExhaustionAlerts returns every open cred-exhaustion alert bead the
// mock daemon holds, using the same label/field query the emitter uses.
func openCredExhaustionAlerts(t *testing.T, daemon *mockDaemon) []*beadsapi.BeadDetail {
	t.Helper()
	res, err := daemon.ListBeadsFiltered(context.Background(), beadsapi.ListBeadsQuery{
		Types:    []string{"bug"},
		Statuses: []string{"open", "in_progress"},
		Labels:   []string{"source:alert", credExhaustionAlertLabel},
	})
	if err != nil {
		t.Fatalf("list cred-exhaustion alerts: %v", err)
	}
	return res.Beads
}

func TestNotifyNoFallback_FilesAlertBead(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	evt := RotationEvent{
		Event:       "no_fallback",
		Project:     "monorepo",
		FromAccount: "acme-team",
		ResetAt:     "2099-06-15T08:00:00Z",
	}
	if err := bot.NotifyNoFallback(context.Background(), evt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No bespoke Slack post — the whole point of ka-0000005341.
	if got := getCalls(calls, mu); len(got) != 0 {
		t.Fatalf("expected 0 Slack calls (routed to gardenio alerts), got %d", len(got))
	}

	beads := openCredExhaustionAlerts(t, daemon)
	if len(beads) != 1 {
		t.Fatalf("expected 1 cred-exhaustion alert bead, got %d", len(beads))
	}
	b := beads[0]
	if b.Type != "bug" {
		t.Errorf("alert bead type = %q, want bug", b.Type)
	}
	for _, want := range []string{"source:alert", "severity:warn", credExhaustionAlertLabel, "project:monorepo"} {
		if !hasLabel(b.Labels, want) {
			t.Errorf("alert bead missing label %q (have %v)", want, b.Labels)
		}
	}
	if b.Fields["alert_severity"] != "warn" {
		t.Errorf("alert_severity = %q, want warn", b.Fields["alert_severity"])
	}
	if b.Fields["cred_exhaustion_kind"] != credExhaustionKindNoFallback {
		t.Errorf("cred_exhaustion_kind = %q, want %q", b.Fields["cred_exhaustion_kind"], credExhaustionKindNoFallback)
	}
	if b.Fields["cred_exhaustion_account"] != "acme-team" {
		t.Errorf("cred_exhaustion_account = %q, want acme-team", b.Fields["cred_exhaustion_account"])
	}
	wantKey := credExhaustionEventKey("monorepo", "acme-team")
	if b.Fields["webhook_event_key"] != wantKey {
		t.Errorf("webhook_event_key = %q, want %q", b.Fields["webhook_event_key"], wantKey)
	}
	// Recovery ETA is folded into the bead body (no longer a Slack line).
	if !strings.Contains(b.Description, "2099-06-15T08:00:00Z") {
		t.Errorf("expected recovery ETA in bead description, got %q", b.Description)
	}
}

func TestNotifyNoFallback_DedupsWhileOpen(t *testing.T) {
	daemon := newMockDaemon()
	srv, _, _ := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	evt := RotationEvent{Event: "no_fallback", Project: "monorepo", FromAccount: "acme-team"}

	if err := bot.NotifyNoFallback(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if err := bot.NotifyNoFallback(context.Background(), evt); err != nil {
		t.Fatal(err)
	}

	if beads := openCredExhaustionAlerts(t, daemon); len(beads) != 1 {
		t.Fatalf("expected 1 alert bead (second deduped by open state), got %d", len(beads))
	}
}

// --- Recovery auto-close tests ---

func TestNotifyAccountRecovered_ClosesOpenAlert(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	account := "acme-team"
	noFallback := RotationEvent{
		Event: "no_fallback", Project: "monorepo", FromAccount: account,
	}
	if err := bot.NotifyNoFallback(context.Background(), noFallback); err != nil {
		t.Fatal(err)
	}
	if beads := openCredExhaustionAlerts(t, daemon); len(beads) != 1 {
		t.Fatalf("expected 1 open alert before recovery, got %d", len(beads))
	}

	recovery := RotationEvent{Event: "account_recovered", FromAccount: account}
	if err := bot.NotifyAccountRecovered(context.Background(), recovery); err != nil {
		t.Fatal(err)
	}

	// The alert bead is auto-closed on recovery.
	if beads := openCredExhaustionAlerts(t, daemon); len(beads) != 0 {
		t.Fatalf("expected 0 open alerts after recovery, got %d", len(beads))
	}
	if closed := daemon.getClosed(); len(closed) != 1 {
		t.Fatalf("expected 1 CloseBead call on recovery, got %d", len(closed))
	}
	// An auto-resolve provenance comment is left before the close.
	foundComment := false
	for _, c := range daemon.getComments() {
		if c.Author == credExhaustionActor && strings.Contains(c.Text, "recovered") {
			foundComment = true
		}
	}
	if !foundComment {
		t.Error("expected an auto-resolve provenance comment from the cred-exhaustion actor")
	}

	// A fresh exhaustion for the same account files a new alert (open-state
	// dedup was released by the close).
	if err := bot.NotifyNoFallback(context.Background(), noFallback); err != nil {
		t.Fatal(err)
	}
	if beads := openCredExhaustionAlerts(t, daemon); len(beads) != 1 {
		t.Fatalf("expected 1 open alert after re-exhaustion, got %d", len(beads))
	}

	// Recovery never posts a bespoke exhaustion Slack line here (default mode).
	if got := getCalls(calls, mu); len(got) != 0 {
		t.Fatalf("expected 0 Slack calls across the exhaustion→recovery flow, got %d", len(got))
	}
}

// --- formatRecoveryETA tests ---

func TestFormatRecoveryETA_Empty(t *testing.T) {
	if got := formatRecoveryETA(""); got != "" {
		t.Errorf("expected empty string for empty input, got %q", got)
	}
}

// TestResolveThreadedChannel_ResurrectsCardFromState verifies that
// resolveThreadedChannel ensures the agent card exists (via state resurrection)
// instead of falling back to unthreaded posting when the hot cache is empty.
func TestResolveThreadedChannel_ResurrectsCardFromState(t *testing.T) {
	daemon := newMockDaemon()
	srv, _, _ := capturingSlackServer(t)
	defer srv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	cardRef := MessageRef{ChannelID: "C-test", Timestamp: "1700000000.000001"}
	_ = state.SetAgentCard("crash-agent", cardRef)

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.state = state
	// Do NOT populate bot.agentCards — simulates hot cache pruned.

	ch, threadTS := bot.resolveThreadedChannel(context.Background(), "crash-agent")
	if ch != "C-test" {
		t.Errorf("expected channel C-test, got %q", ch)
	}
	if threadTS == "" {
		t.Errorf("resolveThreadedChannel returned empty threadTS; expected card to be resurrected from state")
	}
}

func TestFormatRecoveryETA_InvalidTimestamp(t *testing.T) {
	if got := formatRecoveryETA("not-a-date"); got != "" {
		t.Errorf("expected empty string for invalid timestamp, got %q", got)
	}
}

func TestFormatRecoveryETA_FutureTime(t *testing.T) {
	// Far future — should contain "from now".
	got := formatRecoveryETA("2099-01-01T00:00:00Z")
	if !strings.Contains(got, "from now") {
		t.Errorf("expected 'from now' for future timestamp, got %q", got)
	}
	if !strings.Contains(got, "2099-01-01T00:00:00Z") {
		t.Errorf("expected timestamp in output, got %q", got)
	}
}

func TestFormatRecoveryETA_PastTime(t *testing.T) {
	// Past — should say "imminent".
	got := formatRecoveryETA("2020-01-01T00:00:00Z")
	if !strings.Contains(got, "imminent") {
		t.Errorf("expected 'imminent' for past timestamp, got %q", got)
	}
}

// --- NotifySwitchback tests ---

func TestNotifySwitchback_Basic(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAccountEvent(daemon, "agent-switchback-basic") // full-verbosity path

	evt := RotationEvent{
		Event:       "switchback",
		Project:     "monorepo",
		FromAccount: "matthewbaker-acme",
		ToAccount:   "matthewcbaker0122",
		AgentBead:   "agent-switchback-basic",
	}
	if err := bot.NotifySwitchback(context.Background(), evt); err != nil {
		t.Fatalf("NotifySwitchback error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(got))
	}
}

func TestNotifySwitchback_SuppressesDuplicates(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAccountEvent(daemon, "agent-switchback-dup") // full-verbosity path

	evt := RotationEvent{
		Event:       "switchback",
		Project:     "monorepo",
		FromAccount: "matthewbaker-acme",
		ToAccount:   "matthewcbaker0122",
		AgentBead:   "agent-switchback-dup",
	}

	// A flapping primary fires switchback repeatedly; only the first posts.
	for i := 0; i < 5; i++ {
		if err := bot.NotifySwitchback(context.Background(), evt); err != nil {
			t.Fatalf("call %d error: %v", i, err)
		}
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call (flaps suppressed), got %d", len(got))
	}
}

func TestNotifySwitchback_DifferentProjectsNotSuppressed(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAccountEvent(daemon, "agent-switchback-proj") // full-verbosity path

	evt1 := RotationEvent{
		Event:       "switchback",
		Project:     "monorepo",
		FromAccount: "matthewbaker-acme",
		ToAccount:   "matthewcbaker0122",
		AgentBead:   "agent-switchback-proj",
	}
	evt2 := RotationEvent{
		Event:       "switchback",
		Project:     "gardenio",
		FromAccount: "matthewbaker-acme",
		ToAccount:   "matthewcbaker0122",
		AgentBead:   "agent-switchback-proj",
	}

	if err := bot.NotifySwitchback(context.Background(), evt1); err != nil {
		t.Fatal(err)
	}
	if err := bot.NotifySwitchback(context.Background(), evt2); err != nil {
		t.Fatal(err)
	}

	got := getCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 Slack calls (different projects), got %d", len(got))
	}
}

func TestNotifySwitchback_DifferentAccountsNotSuppressed(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAccountEvent(daemon, "agent-switchback-acct") // full-verbosity path

	evt1 := RotationEvent{
		Event:       "switchback",
		Project:     "monorepo",
		FromAccount: "fallback-a",
		ToAccount:   "primary-one",
		AgentBead:   "agent-switchback-acct",
	}
	evt2 := RotationEvent{
		Event:       "switchback",
		Project:     "monorepo",
		FromAccount: "fallback-b",
		ToAccount:   "primary-two",
		AgentBead:   "agent-switchback-acct",
	}

	if err := bot.NotifySwitchback(context.Background(), evt1); err != nil {
		t.Fatal(err)
	}
	if err := bot.NotifySwitchback(context.Background(), evt2); err != nil {
		t.Fatal(err)
	}

	got := getCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 Slack calls (different recovered accounts), got %d", len(got))
	}
}

func TestNotifySwitchback_RepostsAfterCooldown(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	enableVibeForAccountEvent(daemon, "agent-switchback-cooldown") // full-verbosity path

	evt := RotationEvent{
		Event:       "switchback",
		Project:     "monorepo",
		FromAccount: "matthewbaker-acme",
		ToAccount:   "matthewcbaker0122",
		AgentBead:   "agent-switchback-cooldown",
	}
	if err := bot.NotifySwitchback(context.Background(), evt); err != nil {
		t.Fatal(err)
	}

	// Simulate the cooldown having elapsed by backdating the recorded send.
	bot.mu.Lock()
	bot.switchbackAlertSent[switchbackAlertKey(evt.Project, evt.ToAccount)] = time.Now().Add(-switchbackAlertCooldown - time.Minute)
	bot.mu.Unlock()

	if err := bot.NotifySwitchback(context.Background(), evt); err != nil {
		t.Fatal(err)
	}

	got := getCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 Slack calls (cooldown elapsed), got %d", len(got))
	}
}
