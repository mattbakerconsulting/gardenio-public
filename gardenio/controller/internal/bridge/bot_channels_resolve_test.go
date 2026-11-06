package bridge

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/slack-go/slack"
)

// fakeSlackChannelServer returns a handler that:
//   - Responds to conversations.history / conversations.replies with the given
//     messages (each carrying a user ID).
//   - Responds to users.info with display names from the userMap.
//   - Counts users.info calls in the provided counter.
func fakeSlackChannelServer(t *testing.T, msgs []map[string]any, userMap map[string]string, infoCalls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		switch {
		case strings.Contains(r.URL.Path, "users.info"):
			atomic.AddInt32(infoCalls, 1)
			uid := r.FormValue("user")
			name, ok := userMap[uid]
			if !ok {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "user_not_found"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"user": map[string]any{"id": uid, "real_name": name, "name": strings.ToLower(name)},
			})
		case strings.Contains(r.URL.Path, "conversations.history"),
			strings.Contains(r.URL.Path, "conversations.replies"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": msgs,
				"has_more": false,
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
}

func newChannelTestAPI(srv *httptest.Server) *SlackChannelAPI {
	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	return NewSlackChannelAPI(api, slog.Default())
}

// TestChannelHistory_ResolvesUserIDsToNames verifies that the channel-history
// handler renders display names instead of raw Slack user IDs (ka-0000001421).
func TestChannelHistory_ResolvesUserIDsToNames(t *testing.T) {
	msgs := []map[string]any{
		{"user": "U123ABC", "text": "hello", "ts": "1.1"},
		{"user": "U456DEF", "text": "world", "ts": "1.2"},
		{"user": "U123ABC", "text": "again", "ts": "1.3"},
	}
	users := map[string]string{"U123ABC": "Alice", "U456DEF": "Bob"}

	var infoCalls int32
	srv := fakeSlackChannelServer(t, msgs, users, &infoCalls)
	defer srv.Close()

	mux := http.NewServeMux()
	newChannelTestAPI(srv).RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/slack/channels/history?channel=C-test", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got []ChannelMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	wantAuthors := []string{"Alice", "Bob", "Alice"}
	for i, m := range got {
		if m.Author != wantAuthors[i] {
			t.Errorf("msg[%d].Author=%q want %q", i, m.Author, wantAuthors[i])
		}
	}
	// Per-request cache: 2 unique users -> 2 users.info calls.
	if got := atomic.LoadInt32(&infoCalls); got != 2 {
		t.Errorf("users.info calls=%d want 2 (per-request cache should dedupe)", got)
	}
}

// TestChannelThread_ResolvesUserIDsToNames covers the gb slack read-thread path.
func TestChannelThread_ResolvesUserIDsToNames(t *testing.T) {
	msgs := []map[string]any{
		{"user": "U0OPERATOR1", "text": "first post", "ts": "1.1"},
		{"user": "U0OPERATOR1", "text": "follow up", "ts": "1.2"},
		{"user": "UABCDE", "text": "reply", "ts": "1.3"},
	}
	users := map[string]string{"U0OPERATOR1": "Carol", "UABCDE": "Dave"}

	var infoCalls int32
	srv := fakeSlackChannelServer(t, msgs, users, &infoCalls)
	defer srv.Close()

	mux := http.NewServeMux()
	newChannelTestAPI(srv).RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/slack/channels/thread?channel=C-test&ts=1.1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got []ChannelMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	if got[0].Author != "Carol" || got[2].Author != "Dave" {
		t.Errorf("authors=%q,%q,%q; want Carol, Carol, Dave",
			got[0].Author, got[1].Author, got[2].Author)
	}
	// Should never leak raw IDs.
	for _, m := range got {
		if strings.HasPrefix(m.Author, "U07") || strings.HasPrefix(m.Author, "UABC") {
			t.Errorf("raw Slack ID leaked as author: %q", m.Author)
		}
	}
}

// TestChannelSearch_ResolvesUserIDsToNames covers the gb slack search path,
// including matches found via author name (post-resolution).
func TestChannelSearch_ResolvesUserIDsToNames(t *testing.T) {
	msgs := []map[string]any{
		{"user": "U111", "text": "deploy started", "ts": "1.1"},
		{"user": "U222", "text": "ack", "ts": "1.2"},
	}
	users := map[string]string{"U111": "Erin", "U222": "Frank"}

	var infoCalls int32
	srv := fakeSlackChannelServer(t, msgs, users, &infoCalls)
	defer srv.Close()

	mux := http.NewServeMux()
	newChannelTestAPI(srv).RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/slack/channels/search?channel=C-test&q=deploy", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got []SearchResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].Message.Author != "Erin" {
		t.Errorf("match author=%q want Erin", got[0].Message.Author)
	}
}

// TestSlackUserResolver_FailureFallsBackToID verifies that when users.info
// fails the resolver caches and returns the raw ID without retrying.
func TestSlackUserResolver_FailureFallsBackToID(t *testing.T) {
	var infoCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users.info") {
			atomic.AddInt32(&infoCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "user_not_found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	r := newSlackUserResolver(api, slog.Default())

	if got := r.resolve(""); got != "" {
		t.Errorf("empty input: got %q want \"\"", got)
	}
	if got := r.resolve("UMISS"); got != "UMISS" {
		t.Errorf("miss: got %q want UMISS", got)
	}
	// Repeat call must not retry users.info.
	if got := r.resolve("UMISS"); got != "UMISS" {
		t.Errorf("repeat miss: got %q want UMISS", got)
	}
	if got := atomic.LoadInt32(&infoCalls); got != 1 {
		t.Errorf("users.info calls=%d want 1 (negative cache must hold)", got)
	}
}
