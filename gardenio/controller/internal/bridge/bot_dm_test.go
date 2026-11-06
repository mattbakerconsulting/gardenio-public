package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// dmSlackServer returns an httptest.Server that handles conversations.open
// and chat.postMessage for DM tests, recording all calls.
func dmSlackServer(t *testing.T) (*httptest.Server, *[]slackCall) {
	t.Helper()
	var calls []slackCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		method := r.URL.Path
		w.Header().Set("Content-Type", "application/json")

		switch method {
		case "/conversations.open":
			calls = append(calls, slackCall{
				Method: "conversations.open",
				Text:   r.FormValue("users"),
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D-dm-chan"},
			})
		case "/chat.postMessage":
			calls = append(calls, slackCall{
				Method:  "chat.postMessage",
				Channel: r.FormValue("channel"),
				Text:    r.FormValue("text"),
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": r.FormValue("channel"),
				"ts":      "1111.2222",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	return srv, &calls
}

func TestSendDM_Success(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls := dmSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	ch, ts, err := bot.SendDM(context.Background(), "U-target", "Hello from gardenio")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ch != "D-dm-chan" {
		t.Errorf("expected channel D-dm-chan, got %s", ch)
	}
	if ts != "1111.2222" {
		t.Errorf("expected ts 1111.2222, got %s", ts)
	}

	if len(*calls) != 2 {
		t.Fatalf("expected 2 Slack calls, got %d", len(*calls))
	}
	if (*calls)[0].Method != "conversations.open" {
		t.Errorf("expected first call to be conversations.open, got %s", (*calls)[0].Method)
	}
	if (*calls)[0].Text != "U-target" {
		t.Errorf("expected conversations.open users=U-target, got %s", (*calls)[0].Text)
	}
	if (*calls)[1].Method != "chat.postMessage" {
		t.Errorf("expected second call to be chat.postMessage, got %s", (*calls)[1].Method)
	}
	if (*calls)[1].Channel != "D-dm-chan" {
		t.Errorf("expected postMessage to D-dm-chan, got %s", (*calls)[1].Channel)
	}
	if (*calls)[1].Text != "Hello from gardenio" {
		t.Errorf("expected message text 'Hello from gardenio', got %s", (*calls)[1].Text)
	}
}

func TestSendDM_EmptyUserID(t *testing.T) {
	daemon := newMockDaemon()
	srv, _ := dmSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	_, _, err := bot.SendDM(context.Background(), "", "Hello")
	if err == nil {
		t.Fatal("expected error for empty user ID")
	}
}

func TestSendDM_EmptyText(t *testing.T) {
	daemon := newMockDaemon()
	srv, _ := dmSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	_, _, err := bot.SendDM(context.Background(), "U-target", "")
	if err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestSendDMBlocks_Success(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls := dmSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	ch, ts, err := bot.SendDMBlocks(context.Background(), "U-target", "fallback text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ch != "D-dm-chan" {
		t.Errorf("expected channel D-dm-chan, got %s", ch)
	}
	if ts != "1111.2222" {
		t.Errorf("expected ts 1111.2222, got %s", ts)
	}

	if len(*calls) != 2 {
		t.Fatalf("expected 2 Slack calls, got %d", len(*calls))
	}
	if (*calls)[0].Method != "conversations.open" {
		t.Errorf("expected first call to be conversations.open, got %s", (*calls)[0].Method)
	}
	if (*calls)[1].Method != "chat.postMessage" {
		t.Errorf("expected second call to be chat.postMessage, got %s", (*calls)[1].Method)
	}
}

func TestSendDMBlocks_EmptyUserID(t *testing.T) {
	daemon := newMockDaemon()
	srv, _ := dmSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	_, _, err := bot.SendDMBlocks(context.Background(), "", "fallback")
	if err == nil {
		t.Fatal("expected error for empty user ID")
	}
}

func TestSendDM_CachesConversationsOpen(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls := dmSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	// First DM — conversations.open is called.
	_, _, err := bot.SendDM(context.Background(), "U-target", "msg 1")
	if err != nil {
		t.Fatalf("first SendDM: %v", err)
	}

	// Second DM to same user — should use cached channel, no conversations.open.
	_, _, err = bot.SendDM(context.Background(), "U-target", "msg 2")
	if err != nil {
		t.Fatalf("second SendDM: %v", err)
	}

	// Count conversations.open calls — should be exactly 1 (cached for second).
	openCalls := 0
	for _, c := range *calls {
		if c.Method == "conversations.open" {
			openCalls++
		}
	}
	if openCalls != 1 {
		t.Errorf("expected 1 conversations.open call (cached), got %d", openCalls)
	}

	// But both messages should have been posted.
	postCalls := 0
	for _, c := range *calls {
		if c.Method == "chat.postMessage" {
			postCalls++
		}
	}
	if postCalls != 2 {
		t.Errorf("expected 2 chat.postMessage calls, got %d", postCalls)
	}
}

// ── SendGroupDM tests ─────────────────────────────────────────────

func TestSendGroupDM_Success(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls := dmSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	ch, ts, err := bot.SendGroupDM(context.Background(), []string{"U-alice", "U-bob"}, "Hello group")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ch != "D-dm-chan" {
		t.Errorf("expected channel D-dm-chan, got %s", ch)
	}
	if ts != "1111.2222" {
		t.Errorf("expected ts 1111.2222, got %s", ts)
	}

	if len(*calls) != 2 {
		t.Fatalf("expected 2 Slack calls, got %d", len(*calls))
	}
	if (*calls)[0].Method != "conversations.open" {
		t.Errorf("expected first call to be conversations.open, got %s", (*calls)[0].Method)
	}
	// Multiple users should be comma-separated.
	if (*calls)[0].Text != "U-alice,U-bob" {
		t.Errorf("expected conversations.open users=U-alice,U-bob, got %s", (*calls)[0].Text)
	}
	if (*calls)[1].Method != "chat.postMessage" {
		t.Errorf("expected second call to be chat.postMessage, got %s", (*calls)[1].Method)
	}
	if (*calls)[1].Text != "Hello group" {
		t.Errorf("expected message text 'Hello group', got %s", (*calls)[1].Text)
	}
}

func TestSendGroupDM_SingleUserDelegatesToSendDM(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls := dmSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	ch, ts, err := bot.SendGroupDM(context.Background(), []string{"U-solo"}, "Solo message")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ch != "D-dm-chan" {
		t.Errorf("expected channel D-dm-chan, got %s", ch)
	}
	if ts != "1111.2222" {
		t.Errorf("expected ts 1111.2222, got %s", ts)
	}

	// Should call conversations.open with single user (delegated to SendDM).
	if len(*calls) != 2 {
		t.Fatalf("expected 2 Slack calls, got %d", len(*calls))
	}
	if (*calls)[0].Text != "U-solo" {
		t.Errorf("expected conversations.open users=U-solo, got %s", (*calls)[0].Text)
	}
}

func TestSendGroupDM_EmptyUserIDs(t *testing.T) {
	daemon := newMockDaemon()
	srv, _ := dmSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	_, _, err := bot.SendGroupDM(context.Background(), []string{}, "Hello")
	if err == nil {
		t.Fatal("expected error for empty user IDs")
	}
}

func TestSendGroupDM_EmptyText(t *testing.T) {
	daemon := newMockDaemon()
	srv, _ := dmSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	_, _, err := bot.SendGroupDM(context.Background(), []string{"U-alice", "U-bob"}, "")
	if err == nil {
		t.Fatal("expected error for empty text")
	}
}

func TestSendDM_ConversationsOpenError(t *testing.T) {
	daemon := newMockDaemon()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.open" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":    false,
				"error": "user_not_found",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	_, _, err := bot.SendDM(context.Background(), "U-nonexistent", "Hello")
	if err == nil {
		t.Fatal("expected error when conversations.open fails")
	}
}
