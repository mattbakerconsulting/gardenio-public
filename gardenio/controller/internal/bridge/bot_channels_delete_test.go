package bridge

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"
)

// newDeleteTestAPI creates a SlackChannelAPI with a mock Slack server that
// routes chat.delete calls to the provided handler.
func newDeleteTestAPI(handler http.HandlerFunc) (*SlackChannelAPI, *httptest.Server) {
	slackSrv := httptest.NewServer(handler)
	api := slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/"))
	return NewSlackChannelAPI(api, slog.Default()), slackSrv
}

func TestDeleteMessage_Success(t *testing.T) {
	var mu sync.Mutex
	var deletedChannel, deletedTS string

	channelAPI, slackSrv := newDeleteTestAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.delete" {
			_ = r.ParseForm()
			mu.Lock()
			deletedChannel = r.FormValue("channel")
			deletedTS = r.FormValue("ts")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1234.5678"})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterRoutes(mux)

	body := `{"channel":"C123","timestamp":"1234.5678"}`
	req := httptest.NewRequest(http.MethodPost, "/api/slack/messages/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp deleteMessageResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK {
		t.Error("expected ok=true")
	}
	if resp.Channel != "C123" {
		t.Errorf("channel = %q, want C123", resp.Channel)
	}
	if resp.Timestamp != "1234.5678" {
		t.Errorf("timestamp = %q, want 1234.5678", resp.Timestamp)
	}

	mu.Lock()
	defer mu.Unlock()
	if deletedChannel != "C123" {
		t.Errorf("Slack received channel = %q, want C123", deletedChannel)
	}
	if deletedTS != "1234.5678" {
		t.Errorf("Slack received ts = %q, want 1234.5678", deletedTS)
	}
}

func TestDeleteMessage_SlackReturnsNotFound(t *testing.T) {
	channelAPI, slackSrv := newDeleteTestAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "message_not_found"})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterRoutes(mux)

	body := `{"channel":"C123","timestamp":"0000.0000"}`
	req := httptest.NewRequest(http.MethodPost, "/api/slack/messages/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("got status %d, want %d", w.Code, http.StatusInternalServerError)
	}

	var resp deleteMessageResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OK {
		t.Error("expected ok=false")
	}
	if !strings.Contains(resp.Error, "message_not_found") {
		t.Errorf("error = %q, want it to contain 'message_not_found'", resp.Error)
	}
}

func TestDeleteMessage_SlackReturnsChannelNotFound(t *testing.T) {
	channelAPI, slackSrv := newDeleteTestAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "channel_not_found"})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterRoutes(mux)

	body := `{"channel":"C-NONEXISTENT","timestamp":"1234.5678"}`
	req := httptest.NewRequest(http.MethodPost, "/api/slack/messages/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("got status %d, want %d", w.Code, http.StatusInternalServerError)
	}

	var resp deleteMessageResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp.Error, "channel_not_found") {
		t.Errorf("error = %q, want 'channel_not_found'", resp.Error)
	}
}

func TestDeleteMessage_SlackReturnsCantDeleteMessage(t *testing.T) {
	// Slack returns "cant_delete_message" when the bot doesn't own the message.
	channelAPI, slackSrv := newDeleteTestAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "cant_delete_message"})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterRoutes(mux)

	body := `{"channel":"C123","timestamp":"9999.8888"}`
	req := httptest.NewRequest(http.MethodPost, "/api/slack/messages/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("got status %d, want %d", w.Code, http.StatusInternalServerError)
	}

	var resp deleteMessageResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if !strings.Contains(resp.Error, "cant_delete_message") {
		t.Errorf("error = %q, want 'cant_delete_message'", resp.Error)
	}
}

func TestDeleteMessage_SlackReturnsRateLimited(t *testing.T) {
	channelAPI, slackSrv := newDeleteTestAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "30")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "ratelimited"})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterRoutes(mux)

	body := `{"channel":"C123","timestamp":"1234.5678"}`
	req := httptest.NewRequest(http.MethodPost, "/api/slack/messages/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Rate limited responses still come back as 500 from our handler
	// since the handler treats all Slack API errors the same way.
	if w.Code != http.StatusInternalServerError {
		t.Errorf("got status %d, want %d", w.Code, http.StatusInternalServerError)
	}

	var resp deleteMessageResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK {
		t.Error("expected ok=false for rate limited response")
	}
}

func TestDeleteMessage_SlackServerDown(t *testing.T) {
	// Create a server and immediately close it to simulate connection refused.
	channelAPI, slackSrv := newDeleteTestAPI(func(w http.ResponseWriter, r *http.Request) {})
	slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterRoutes(mux)

	body := `{"channel":"C123","timestamp":"1234.5678"}`
	req := httptest.NewRequest(http.MethodPost, "/api/slack/messages/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("got status %d, want %d", w.Code, http.StatusInternalServerError)
	}

	var resp deleteMessageResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK {
		t.Error("expected ok=false when Slack is unreachable")
	}
	if resp.Error == "" {
		t.Error("expected non-empty error message")
	}
}

func TestDeleteMessage_EmptyBody(t *testing.T) {
	channelAPI, slackSrv := newDeleteTestAPI(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Slack API should not be called for empty body")
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/slack/messages/delete", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestDeleteMessage_ResponseFormat(t *testing.T) {
	channelAPI, slackSrv := newDeleteTestAPI(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-RESP", "ts": "5555.6666"})
	})
	defer slackSrv.Close()

	mux := http.NewServeMux()
	channelAPI.RegisterRoutes(mux)

	body := `{"channel":"C-RESP","timestamp":"5555.6666"}`
	req := httptest.NewRequest(http.MethodPost, "/api/slack/messages/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp deleteMessageResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	// Verify the response matches the deleteMessageResponse struct shape.
	if !resp.OK {
		t.Error("expected ok=true")
	}
	if resp.Error != "" {
		t.Errorf("expected empty error, got %q", resp.Error)
	}
}
