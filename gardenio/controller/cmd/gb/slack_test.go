package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestResolveSlackEnv_AllSet(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "http://bridge:8080")
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "1234567890.123456")

	bridgeURL, channel, ts, err := resolveSlackEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bridgeURL != "http://bridge:8080" {
		t.Errorf("bridgeURL = %q, want %q", bridgeURL, "http://bridge:8080")
	}
	if channel != "C123" {
		t.Errorf("channel = %q, want %q", channel, "C123")
	}
	if ts != "1234567890.123456" {
		t.Errorf("ts = %q, want %q", ts, "1234567890.123456")
	}
}

func TestResolveSlackEnv_MissingBridgeURL(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "1234567890.123456")

	_, _, _, err := resolveSlackEnv()
	if err == nil {
		t.Fatal("expected error when SLACK_BRIDGE_URL is empty")
	}
	if !strings.Contains(err.Error(), "SLACK_BRIDGE_URL") {
		t.Errorf("error should mention SLACK_BRIDGE_URL, got: %v", err)
	}
}

func TestResolveSlackEnv_MissingThreadEnvVars(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "http://bridge:8080")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")

	_, _, _, err := resolveSlackEnv()
	if err == nil {
		t.Fatal("expected error when thread env vars are empty")
	}
	if !strings.Contains(err.Error(), "no thread context") {
		t.Errorf("error should mention 'no thread context', got: %v", err)
	}
}

func TestResolveSlackEnv_MissingChannel(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "http://bridge:8080")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "12345.6789")

	_, _, _, err := resolveSlackEnv()
	if err == nil {
		t.Fatal("expected error when SLACK_THREAD_CHANNEL is empty")
	}
}

func TestResolveSlackEnv_MissingTS(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "http://bridge:8080")
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "")

	_, _, _, err := resolveSlackEnv()
	if err == nil {
		t.Fatal("expected error when SLACK_THREAD_TS is empty")
	}
}

func TestPrintThreadMarkdown_Empty(t *testing.T) {
	// Capture stdout.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printThreadMarkdown(nil, 4000, true, nil)

	w.Close()
	os.Stdout = old

	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])
	if !strings.Contains(output, "No thread messages") {
		t.Errorf("expected 'No thread messages' in output, got: %q", output)
	}
}

func TestPrintThreadMarkdown_FormatAndTruncate(t *testing.T) {
	msgs := []slackThreadMessage{
		{Author: "alice", Text: "Hello there"},
		{Author: "bob", Text: "Hi alice!"},
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printThreadMarkdown(msgs, 4000, true, nil)

	w.Close()
	os.Stdout = old

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "**alice**: Hello there") {
		t.Errorf("expected formatted alice message, got: %q", output)
	}
	if !strings.Contains(output, "**bob**: Hi alice!") {
		t.Errorf("expected formatted bob message, got: %q", output)
	}
}

func TestPrintThreadMarkdown_Truncation(t *testing.T) {
	msgs := []slackThreadMessage{
		{Author: "alice", Text: "Short message"},
		{Author: "bob", Text: "Another message that will cause truncation"},
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Very small max-chars to force truncation.
	printThreadMarkdown(msgs, 30, true, nil)

	w.Close()
	os.Stdout = old

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	// New behavior: most-recent message is preserved, older messages are
	// elided with `_(N messages elided)_` (ka-0000004284).
	if !strings.Contains(output, "**bob**: Another message that will cause truncation") {
		t.Errorf("expected most-recent message preserved, got: %q", output)
	}
	if !strings.Contains(output, "messages elided") {
		t.Errorf("expected elision marker, got: %q", output)
	}
}

func TestSlackThread_Integration(t *testing.T) {
	// Mock bridge server.
	server := httptest.NewServer(oldBridge(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/slack/threads" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		channel := r.URL.Query().Get("channel")
		ts := r.URL.Query().Get("ts")
		if channel != "C999" || ts != "111.222" {
			t.Errorf("unexpected params: channel=%s ts=%s", channel, ts)
		}

		msgs := []slackThreadMessage{
			{Author: "user1", Text: "Need help with deployment", Timestamp: "111.223"},
			{Author: "user2", Text: "I can look into it", Timestamp: "111.224"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msgs)
	}))
	defer server.Close()

	t.Setenv("SLACK_BRIDGE_URL", server.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	// Capture stdout.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runSlackThread(slackThreadCmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	w.Close()
	os.Stdout = old

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "**user1**: Need help with deployment") {
		t.Errorf("expected user1 message, got: %q", output)
	}
	if !strings.Contains(output, "**user2**: I can look into it") {
		t.Errorf("expected user2 message, got: %q", output)
	}
}

func TestSlackReply_Integration(t *testing.T) {
	var receivedBody map[string]string
	server := httptest.NewServer(oldBridge(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/slack/threads/reply" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(slackReplyResult{OK: true, Timestamp: "111.225"})
	}))
	defer server.Close()

	t.Setenv("SLACK_BRIDGE_URL", server.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	// daemon is nil in this test → recordSlackReplyBead skips silently.
	// Capture stdout.
	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w

	err := runSlackReply(slackReplyCmd, []string{"Thanks for the help!"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	w.Close()
	os.Stdout = old

	if receivedBody["channel"] != "C999" {
		t.Errorf("channel = %q, want %q", receivedBody["channel"], "C999")
	}
	if receivedBody["thread_ts"] != "111.222" {
		t.Errorf("thread_ts = %q, want %q", receivedBody["thread_ts"], "111.222")
	}
	if receivedBody["text"] != "Thanks for the help!" {
		t.Errorf("text = %q, want %q", receivedBody["text"], "Thanks for the help!")
	}
}

// TestSlackReply_CreatesMessageBead verifies the cheap fix for ka-0000000087:
// after posting a slack reply, gb slack reply records a type=message bead
// (label slack-reply) so the conversational reply is discoverable in
// beads-viewer alongside gb squawk's message beads.
func TestSlackReply_CreatesMessageBead(t *testing.T) {
	// Clear lineage env vars so the bead-create path runs in isolation.
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TRIGGER_BEAD_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	// Bridge server: accepts the reply post.
	bridge := httptest.NewServer(oldBridge(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/slack/threads/reply" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(slackReplyResult{OK: true, Timestamp: "111.225"})
			return
		}
		t.Errorf("unexpected bridge request: %s %s", r.Method, r.URL.Path)
	}))
	defer bridge.Close()

	// Beads daemon stub: captures the create + close calls.
	var createdBody map[string]any
	closed := false
	beads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewDecoder(r.Body).Decode(&createdBody)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-REPLY1"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			closed = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		default:
			t.Errorf("unexpected daemon %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer beads.Close()
	swapDaemon(t, beads.URL)

	t.Setenv("SLACK_BRIDGE_URL", bridge.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	oldActor := actor
	actor = "test-agent"
	t.Cleanup(func() { actor = oldActor })

	// Capture stdout.
	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	defer func() {
		w.Close()
		os.Stdout = old
	}()

	slackReplyCmd.SetContext(context.Background())
	if err := runSlackReply(slackReplyCmd, []string{"Hi there"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if createdBody == nil {
		t.Fatal("expected a message bead to be created, got none")
	}
	if got := createdBody["type"]; got != "message" {
		t.Errorf("bead type = %v, want message", got)
	}
	labels, _ := createdBody["labels"].([]any)
	foundReplyLabel := false
	foundFromLabel := false
	for _, l := range labels {
		switch l {
		case "slack-reply":
			foundReplyLabel = true
		case "from:test-agent":
			foundFromLabel = true
		}
	}
	if !foundReplyLabel {
		t.Errorf("expected `slack-reply` label, got %v", labels)
	}
	if !foundFromLabel {
		t.Errorf("expected `from:test-agent` label, got %v", labels)
	}
	if !closed {
		t.Error("expected bead to be closed after creation")
	}
}

// TestSlackReply_BeadCreateFailureDoesNotFailReply verifies that a beads
// daemon outage cannot make a successful Slack reply look like a failure.
func TestSlackReply_BeadCreateFailureDoesNotFailReply(t *testing.T) {
	bridge := httptest.NewServer(oldBridge(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/slack/threads/reply" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(slackReplyResult{OK: true, Timestamp: "111.225"})
			return
		}
		t.Errorf("unexpected bridge request: %s %s", r.Method, r.URL.Path)
	}))
	defer bridge.Close()

	// Beads daemon: always 500.
	beads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer beads.Close()
	swapDaemon(t, beads.URL)

	t.Setenv("SLACK_BRIDGE_URL", bridge.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w
	defer func() {
		w.Close()
		os.Stdout = old
	}()

	slackReplyCmd.SetContext(context.Background())
	if err := runSlackReply(slackReplyCmd, []string{"hello"}); err != nil {
		t.Fatalf("slack reply must not fail when beads daemon is unreachable: %v", err)
	}
}

func TestSlackThread_BridgeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	t.Setenv("SLACK_BRIDGE_URL", server.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	err := runSlackThread(slackThreadCmd, nil)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention HTTP 500, got: %v", err)
	}
}

func TestSlackReply_BridgeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	t.Setenv("SLACK_BRIDGE_URL", server.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	err := runSlackReply(slackReplyCmd, []string{"test"})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention HTTP 500, got: %v", err)
	}
}

func TestIsThreadSpawnedAgent(t *testing.T) {
	t.Run("both set", func(t *testing.T) {
		t.Setenv("SLACK_THREAD_CHANNEL", "C123")
		t.Setenv("SLACK_THREAD_TS", "111.222")
		if !isThreadSpawnedAgent() {
			t.Error("expected true when both env vars set")
		}
	})
	t.Run("missing channel", func(t *testing.T) {
		t.Setenv("SLACK_THREAD_CHANNEL", "")
		t.Setenv("SLACK_THREAD_TS", "111.222")
		if isThreadSpawnedAgent() {
			t.Error("expected false when channel missing")
		}
	})
	t.Run("missing ts", func(t *testing.T) {
		t.Setenv("SLACK_THREAD_CHANNEL", "C123")
		t.Setenv("SLACK_THREAD_TS", "")
		if isThreadSpawnedAgent() {
			t.Error("expected false when ts missing")
		}
	})
	t.Run("both empty", func(t *testing.T) {
		t.Setenv("SLACK_THREAD_CHANNEL", "")
		t.Setenv("SLACK_THREAD_TS", "")
		if isThreadSpawnedAgent() {
			t.Error("expected false when both empty")
		}
	})
}

func TestFetchThreadMessages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msgs := []slackThreadMessage{
			{Author: "alice", Text: "Hello", Timestamp: "111.222"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msgs)
	}))
	defer server.Close()

	msgs := fetchThreadMessages(server.URL, "C123", "111.222", 50)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Author != "alice" {
		t.Errorf("expected author alice, got %s", msgs[0].Author)
	}
}

// A legacy thread read names the reading agent, for the bridge's land wall
// (gd-3uBuj2h7Ar).
func TestFetchThreadMessages_NamesAgent(t *testing.T) {
	oldActor := actor
	actor = "crew-reader"
	t.Cleanup(func() { actor = oldActor })
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("agent")
		_ = json.NewEncoder(w).Encode([]slackThreadMessage{})
	}))
	defer server.Close()

	fetchThreadMessages(server.URL, "C123", "111.222", 50)
	if got != "crew-reader" {
		t.Errorf("agent = %q, want crew-reader", got)
	}
}

func TestFetchThreadMessages_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "error", http.StatusInternalServerError)
	}))
	defer server.Close()

	msgs := fetchThreadMessages(server.URL, "C123", "111.222", 50)
	if msgs != nil {
		t.Errorf("expected nil on server error, got %v", msgs)
	}
}

func TestFetchThreadMessages_Unreachable(t *testing.T) {
	msgs := fetchThreadMessages("http://127.0.0.1:1", "C123", "111.222", 50)
	if msgs != nil {
		t.Errorf("expected nil for unreachable server, got %v", msgs)
	}
}

func TestFormatThreadMarkdown(t *testing.T) {
	msgs := []slackThreadMessage{
		{Author: "alice", Text: "Hello"},
		{Author: "bob", Text: "Hi"},
	}
	out := formatThreadMarkdown(msgs, 4000, true, nil)
	if !strings.Contains(out, "**alice**: Hello") {
		t.Errorf("expected alice message, got: %q", out)
	}
	if !strings.Contains(out, "**bob**: Hi") {
		t.Errorf("expected bob message, got: %q", out)
	}
}

func TestFormatThreadMarkdown_Empty(t *testing.T) {
	out := formatThreadMarkdown(nil, 4000, true, nil)
	if !strings.Contains(out, "No thread messages") {
		t.Errorf("expected 'No thread messages', got: %q", out)
	}
}

func TestFormatThreadMarkdown_Truncation(t *testing.T) {
	msgs := []slackThreadMessage{
		{Author: "alice", Text: "First message"},
		{Author: "bob", Text: "Second message that will be truncated"},
	}
	out := formatThreadMarkdown(msgs, 30, true, nil)
	// The most-recent message must always be preserved, even when its
	// length alone blows the budget.
	if !strings.Contains(out, "**bob**: Second message that will be truncated") {
		t.Errorf("expected most-recent message to be preserved, got: %q", out)
	}
	if !strings.Contains(out, "1 messages elided") {
		t.Errorf("expected elision marker for the older message, got: %q", out)
	}
}

func TestFormatThreadMarkdown_HeadAndTailPreserved(t *testing.T) {
	msgs := []slackThreadMessage{
		{Author: "alice", Text: "originating problem statement"},
		{Author: "bob", Text: "noise one noise one noise one"},
		{Author: "carol", Text: "noise two noise two noise two"},
		{Author: "dave", Text: "noise three noise three noise three"},
		{Author: "eve", Text: "the latest reply that the agent must answer"},
	}
	// Tight budget that fits head + last but not the middle messages.
	out := formatThreadMarkdown(msgs, 130, true, nil)
	if !strings.Contains(out, "**alice**: originating problem statement") {
		t.Errorf("expected originating message preserved as head, got: %q", out)
	}
	if !strings.Contains(out, "**eve**: the latest reply that the agent must answer") {
		t.Errorf("expected most-recent message preserved as tail, got: %q", out)
	}
	if !strings.Contains(out, "messages elided") {
		t.Errorf("expected elision marker between head and tail, got: %q", out)
	}
	// Old-first truncation (the bug) would have dropped eve.
	if strings.Contains(out, "thread truncated") {
		t.Errorf("legacy truncation marker should not appear, got: %q", out)
	}
}

func TestFormatThreadMarkdown_LatestMessageAlwaysPresent(t *testing.T) {
	// Stress test: many messages, latest is short. Latest must always be in
	// the output regardless of how many messages get elided.
	msgs := make([]slackThreadMessage, 50)
	for i := range msgs {
		msgs[i] = slackThreadMessage{Author: fmt.Sprintf("u%d", i), Text: strings.Repeat("x", 200)}
	}
	msgs[len(msgs)-1] = slackThreadMessage{Author: "latest", Text: "ANSWER_TOKEN"}
	out := formatThreadMarkdown(msgs, 1000, true, nil)
	if !strings.Contains(out, "ANSWER_TOKEN") {
		t.Errorf("latest message must always be preserved, got: %q", out)
	}
}

func TestFormatThreadMarkdown_BotMessagesIncluded(t *testing.T) {
	msgs := []slackThreadMessage{
		{Author: "alice", Text: "Help with deploy"},
		{Author: "gardenio", Text: "Working on it", IsBot: true},
		{Author: "bob", Text: "Thanks"},
	}
	out := formatThreadMarkdown(msgs, 4000, true, nil)
	if !strings.Contains(out, "[bot] **gardenio**: Working on it") {
		t.Errorf("expected labeled bot message, got: %q", out)
	}
	if !strings.Contains(out, "**alice**: Help with deploy") {
		t.Errorf("expected alice message, got: %q", out)
	}
}

func TestFormatThreadMarkdown_BotMessagesExcluded(t *testing.T) {
	msgs := []slackThreadMessage{
		{Author: "alice", Text: "Help with deploy"},
		{Author: "gardenio", Text: "Working on it", IsBot: true},
		{Author: "bob", Text: "Thanks"},
	}
	out := formatThreadMarkdown(msgs, 4000, false, nil)
	if strings.Contains(out, "gardenio") {
		t.Errorf("expected bot message to be excluded, got: %q", out)
	}
	if !strings.Contains(out, "**alice**: Help with deploy") {
		t.Errorf("expected alice message, got: %q", out)
	}
	if !strings.Contains(out, "**bob**: Thanks") {
		t.Errorf("expected bob message, got: %q", out)
	}
}

func TestOutputSlackThreadContext_WithThread(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msgs := []slackThreadMessage{
			{Author: "user1", Text: "Help with deployment", Timestamp: "111.222"},
			{Author: "user2", Text: "I agree we need help", Timestamp: "111.223"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msgs)
	}))
	defer server.Close()

	t.Setenv("SLACK_BRIDGE_URL", server.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	var buf strings.Builder
	outputSlackThreadContext(&buf, 4000)
	out := buf.String()

	if !strings.Contains(out, "## Slack Thread Context") {
		t.Errorf("expected header, got: %q", out)
	}
	if !strings.Contains(out, "You were spawned to help with this Slack thread") {
		t.Errorf("expected intro text, got: %q", out)
	}
	if !strings.Contains(out, "**user1**: Help with deployment") {
		t.Errorf("expected user1 message, got: %q", out)
	}
	if !strings.Contains(out, "**user2**: I agree we need help") {
		t.Errorf("expected user2 message, got: %q", out)
	}
	if !strings.Contains(out, "gb slack reply") {
		t.Errorf("expected reply hint, got: %q", out)
	}
	if !strings.Contains(out, "---") {
		t.Errorf("expected separator, got: %q", out)
	}
}

func TestOutputSlackThreadContext_NotThreadSpawned(t *testing.T) {
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	// No agent bead to fall back to either — a genuine non-thread agent.
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")

	var buf strings.Builder
	outputSlackThreadContext(&buf, 4000)
	if buf.Len() != 0 {
		t.Errorf("expected no output for non-thread agent, got: %q", buf.String())
	}
}

// TestOutputSlackThreadContext_PrewarmedBeadFallback verifies that a PREWARMED
// pod — SLACK_THREAD_* baked empty — recovers its thread context via the agent
// bead's slack_thread_channel/slack_thread_ts fields. Before the fix,
// outputSlackThreadContext read the env vars directly (and gated on the env-only
// isThreadSpawnedAgent), so a pooled thread agent booted with no conversation.
func TestOutputSlackThreadContext_PrewarmedBeadFallback(t *testing.T) {
	// Bridge serves the thread messages, keyed on the bead-recovered coords.
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("channel"); got != "C-POOL" {
			t.Errorf("bridge queried channel=%q, want C-POOL (from bead fallback)", got)
		}
		if got := r.URL.Query().Get("ts"); got != "555.666" {
			t.Errorf("bridge queried ts=%q, want 555.666 (from bead fallback)", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]slackThreadMessage{
			{Author: "operator", Text: "Please look into the pool bug", Timestamp: "555.666"},
		})
	}))
	defer bridge.Close()

	// Daemon serves the agent bead carrying the coords.
	daemonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-pool-agent", "type": "agent", "status": "in_progress",
			"fields": map[string]string{
				"slack_thread_channel": "C-POOL",
				"slack_thread_ts":      "555.666",
			},
		})
	}))
	defer daemonSrv.Close()
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	t.Setenv("SLACK_BRIDGE_URL", bridge.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-pool-agent")

	var buf strings.Builder
	outputSlackThreadContext(&buf, 4000)
	out := buf.String()

	if !strings.Contains(out, "## Slack Thread Context") {
		t.Fatalf("expected thread context header on prewarmed fallback, got: %q", out)
	}
	if !strings.Contains(out, "**operator**: Please look into the pool bug") {
		t.Errorf("expected the operator message recovered via bead fallback, got: %q", out)
	}
}

func TestOutputSlackThreadContext_NoBridgeURL(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "")
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	var buf strings.Builder
	outputSlackThreadContext(&buf, 4000)
	if buf.Len() != 0 {
		t.Errorf("expected no output when bridge URL missing, got: %q", buf.String())
	}
}

func TestOutputSlackThreadContext_BridgeUnreachable(t *testing.T) {
	t.Setenv("SLACK_BRIDGE_URL", "http://127.0.0.1:1")
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	var buf strings.Builder
	outputSlackThreadContext(&buf, 4000)
	// Should gracefully produce no output when bridge is unreachable.
	if buf.Len() != 0 {
		t.Errorf("expected no output when bridge unreachable, got: %q", buf.String())
	}
}

func TestSlackThread_JSONOutput(t *testing.T) {
	server := httptest.NewServer(oldBridge(func(w http.ResponseWriter, r *http.Request) {
		msgs := []slackThreadMessage{
			{Author: "alice", Text: "Hello", Timestamp: "111.222"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msgs)
	}))
	defer server.Close()

	t.Setenv("SLACK_BRIDGE_URL", server.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C999")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	// Set jsonOutput flag.
	origJSON := jsonOutput
	jsonOutput = true
	defer func() { jsonOutput = origJSON }()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runSlackThread(slackThreadCmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	w.Close()
	os.Stdout = old

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	// Verify it's valid JSON.
	var parsed []slackThreadMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &parsed); err != nil {
		t.Fatalf("expected valid JSON, got: %q (error: %v)", output, err)
	}
	if len(parsed) != 1 || parsed[0].Author != "alice" {
		t.Errorf("unexpected parsed result: %+v", parsed)
	}
}

// beadServer returns a test HTTP server that serves a bead with the given fields.
func beadServer(t *testing.T, fields map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":     "gd-test123",
			"title":  "test agent",
			"type":   "agent",
			"status": "open",
			"fields": fields,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestResolveSlackEnv_BeadFallback(t *testing.T) {
	srv := beadServer(t, map[string]string{
		"slack_thread_channel": "C456",
		"slack_thread_ts":      "999.888",
	})
	defer srv.Close()

	c, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	oldDaemon := daemon
	daemon = c
	defer func() { daemon = oldDaemon }()

	t.Setenv("SLACK_BRIDGE_URL", "http://bridge:8080")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-test123")

	bridgeURL, channel, ts, err := resolveSlackEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bridgeURL != "http://bridge:8080" {
		t.Errorf("bridgeURL = %q, want %q", bridgeURL, "http://bridge:8080")
	}
	if channel != "C456" {
		t.Errorf("channel = %q, want %q", channel, "C456")
	}
	if ts != "999.888" {
		t.Errorf("ts = %q, want %q", ts, "999.888")
	}

	// Verify env vars were cached.
	if got := os.Getenv("SLACK_THREAD_CHANNEL"); got != "C456" {
		t.Errorf("cached SLACK_THREAD_CHANNEL = %q, want %q", got, "C456")
	}
	if got := os.Getenv("SLACK_THREAD_TS"); got != "999.888" {
		t.Errorf("cached SLACK_THREAD_TS = %q, want %q", got, "999.888")
	}
}

func TestResolveSlackEnv_BeadFallback_MissingFields(t *testing.T) {
	srv := beadServer(t, map[string]string{})
	defer srv.Close()

	c, _ := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	oldDaemon := daemon
	daemon = c
	defer func() { daemon = oldDaemon }()

	t.Setenv("SLACK_BRIDGE_URL", "http://bridge:8080")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-test123")

	_, _, _, err := resolveSlackEnv()
	if err == nil {
		t.Fatal("expected error when bead has no thread fields")
	}
	if !strings.Contains(err.Error(), "no thread context available") {
		t.Errorf("error should mention 'no thread context available', got: %v", err)
	}
}

func TestResolveSlackEnv_BeadFallback_NoDaemon(t *testing.T) {
	oldDaemon := daemon
	daemon = nil
	defer func() { daemon = oldDaemon }()

	t.Setenv("SLACK_BRIDGE_URL", "http://bridge:8080")
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-test123")

	_, _, _, err := resolveSlackEnv()
	if err == nil {
		t.Fatal("expected error when daemon is nil")
	}
	if !strings.Contains(err.Error(), "no thread context") {
		t.Errorf("error should mention 'no thread context', got: %v", err)
	}
}
