package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func setupMock(t *testing.T) (*MockSlack, *httptest.Server, *http.ServeMux) {
	t.Helper()
	mock := NewMockSlack(nil)
	// Use nil logger — calls still work but skip logging.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth.test", mock.handleAuthTest)
	mux.HandleFunc("/api/chat.postMessage", mock.handlePostMessage)
	mux.HandleFunc("/api/chat.update", mock.handleChatUpdate)
	mux.HandleFunc("/api/chat.delete", mock.handleChatDelete)
	mux.HandleFunc("/api/chat.postEphemeral", mock.handlePostEphemeral)
	mux.HandleFunc("/api/conversations.open", mock.handleConversationsOpen)
	mux.HandleFunc("/api/conversations.history", mock.handleConversationsHistory)
	mux.HandleFunc("/api/conversations.replies", mock.handleConversationsReplies)
	mux.HandleFunc("/mock/calls", mock.handleQueryCalls)
	mux.HandleFunc("/mock/messages", mock.handleQueryMessages)
	mux.HandleFunc("/mock/stats", mock.handleQueryStats)
	mux.HandleFunc("/mock/reset", mock.handleReset)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return mock, srv, mux
}

func postForm(t *testing.T, url string, vals url.Values) map[string]any {
	t.Helper()
	resp, err := http.Post(url, "application/x-www-form-urlencoded", strings.NewReader(vals.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("bad JSON: %s", body)
	}
	return result
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("bad JSON: %s", body)
	}
	return result
}

func TestAuthTest(t *testing.T) {
	_, srv, _ := setupMock(t)
	result := postForm(t, srv.URL+"/api/auth.test", url.Values{
		"token": {"xoxb-test"},
	})
	if result["ok"] != true {
		t.Errorf("expected ok=true, got %v", result["ok"])
	}
	if result["user_id"] != "U00000000" {
		t.Errorf("expected user_id U00000000, got %v", result["user_id"])
	}
}

func TestPostAndRetrieveMessage(t *testing.T) {
	_, srv, _ := setupMock(t)

	// Post a message.
	result := postForm(t, srv.URL+"/api/chat.postMessage", url.Values{
		"channel": {"C-test"},
		"text":    {"hello world"},
	})
	if result["ok"] != true {
		t.Fatal("postMessage failed")
	}
	ts, ok := result["ts"].(string)
	if !ok || ts == "" {
		t.Fatal("no ts returned")
	}

	// Query via mock API.
	msgs := getJSON(t, srv.URL+"/mock/messages?channel=C-test")
	total := msgs["total"].(float64)
	if total != 1 {
		t.Errorf("expected 1 message, got %v", total)
	}

	// Query via conversations.history.
	hist := postForm(t, srv.URL+"/api/conversations.history", url.Values{
		"channel": {"C-test"},
	})
	histMsgs := hist["messages"].([]any)
	if len(histMsgs) != 1 {
		t.Errorf("expected 1 message in history, got %d", len(histMsgs))
	}
}

func TestUpdateMessage(t *testing.T) {
	_, srv, _ := setupMock(t)

	// Post.
	result := postForm(t, srv.URL+"/api/chat.postMessage", url.Values{
		"channel": {"C-test"},
		"text":    {"original"},
	})
	ts := result["ts"].(string)

	// Update.
	postForm(t, srv.URL+"/api/chat.update", url.Values{
		"channel": {"C-test"},
		"ts":      {ts},
		"text":    {"updated"},
	})

	// Verify.
	msgs := getJSON(t, srv.URL+"/mock/messages?channel=C-test&search=updated")
	if msgs["total"].(float64) != 1 {
		t.Error("expected updated message")
	}
}

func TestDeleteMessage(t *testing.T) {
	_, srv, _ := setupMock(t)

	result := postForm(t, srv.URL+"/api/chat.postMessage", url.Values{
		"channel": {"C-test"},
		"text":    {"to-delete"},
	})
	ts := result["ts"].(string)

	postForm(t, srv.URL+"/api/chat.delete", url.Values{
		"channel": {"C-test"},
		"ts":      {ts},
	})

	msgs := getJSON(t, srv.URL+"/mock/messages?channel=C-test")
	if msgs["total"].(float64) != 0 {
		t.Error("expected 0 messages after delete")
	}
}

func TestThreadReplies(t *testing.T) {
	_, srv, _ := setupMock(t)

	// Post parent.
	parent := postForm(t, srv.URL+"/api/chat.postMessage", url.Values{
		"channel": {"C-test"},
		"text":    {"parent"},
	})
	parentTS := parent["ts"].(string)

	// Post reply in thread.
	postForm(t, srv.URL+"/api/chat.postMessage", url.Values{
		"channel":   {"C-test"},
		"text":      {"reply"},
		"thread_ts": {parentTS},
	})

	// Query replies.
	replies := postForm(t, srv.URL+"/api/conversations.replies", url.Values{
		"channel": {"C-test"},
		"ts":      {parentTS},
	})
	repMsgs := replies["messages"].([]any)
	if len(repMsgs) != 2 { // parent + reply
		t.Errorf("expected 2 messages (parent + reply), got %d", len(repMsgs))
	}
}

func TestCallStats(t *testing.T) {
	_, srv, _ := setupMock(t)

	postForm(t, srv.URL+"/api/chat.postMessage", url.Values{
		"channel": {"C-test"},
		"text":    {"msg1"},
	})
	postForm(t, srv.URL+"/api/chat.postMessage", url.Values{
		"channel": {"C-test"},
		"text":    {"msg2"},
	})
	postForm(t, srv.URL+"/api/auth.test", url.Values{})

	stats := getJSON(t, srv.URL+"/mock/stats")
	counts := stats["call_counts"].(map[string]any)
	if counts["chat.postMessage"].(float64) != 2 {
		t.Errorf("expected 2 postMessage calls, got %v", counts["chat.postMessage"])
	}
	if counts["auth.test"].(float64) != 1 {
		t.Errorf("expected 1 auth.test call, got %v", counts["auth.test"])
	}
}

func TestReset(t *testing.T) {
	_, srv, _ := setupMock(t)

	postForm(t, srv.URL+"/api/chat.postMessage", url.Values{
		"channel": {"C-test"},
		"text":    {"msg"},
	})

	// Reset.
	http.Post(srv.URL+"/mock/reset", "", nil)

	stats := getJSON(t, srv.URL+"/mock/stats")
	if stats["total_calls"].(float64) != 0 {
		t.Error("expected 0 calls after reset")
	}
	if stats["total_messages"].(float64) != 0 {
		t.Error("expected 0 messages after reset")
	}
}

func TestSearchMessages(t *testing.T) {
	_, srv, _ := setupMock(t)

	postForm(t, srv.URL+"/api/chat.postMessage", url.Values{
		"channel": {"C-test"},
		"text":    {"hello from agent-1"},
	})
	postForm(t, srv.URL+"/api/chat.postMessage", url.Values{
		"channel": {"C-test"},
		"text":    {"goodbye from agent-2"},
	})

	msgs := getJSON(t, srv.URL+"/mock/messages?search=agent-1")
	if msgs["total"].(float64) != 1 {
		t.Errorf("expected 1 match for agent-1, got %v", msgs["total"])
	}
}

// The conversation package's SlackTwin
// (gardenio/controller/internal/conversation/surfacetest/slacktwin.go) mirrors
// these two shapes; keep them in step.
func TestConversationsOpen(t *testing.T) {
	mock, srv, _ := setupMock(t)
	result := postForm(t, srv.URL+"/api/conversations.open", url.Values{
		"users":     {"U0OP"},
		"return_im": {"true"},
	})
	ch, _ := result["channel"].(map[string]any)
	if result["ok"] != true || ch["id"] != "DU0OP" {
		t.Fatalf("conversations.open = %v, want ok and channel DU0OP", result)
	}
	if len(mock.calls) != 1 || mock.calls[0].Method != "conversations.open" {
		t.Fatalf("calls = %+v", mock.calls)
	}
}

func TestPostEphemeralIsNotInHistory(t *testing.T) {
	mock, srv, _ := setupMock(t)
	result := postForm(t, srv.URL+"/api/chat.postEphemeral", url.Values{
		"channel": {"C-test"},
		"user":    {"U0OP"},
		"text":    {"only you"},
	})
	if result["ok"] != true || result["message_ts"] == "" {
		t.Fatalf("chat.postEphemeral = %v", result)
	}
	if len(mock.messages["C-test"]) != 0 {
		t.Fatal("ephemeral was stored as a channel message")
	}
	if p := mock.calls[0].Params; mock.calls[0].Method != "chat.postEphemeral" || p["user"] != "U0OP" {
		t.Fatalf("calls = %+v", mock.calls)
	}
}
