// Command slackmock is a lightweight HTTP server that implements the minimum
// Slack Web API surface needed for E2E testing. It records all API calls in
// memory and exposes query endpoints for test assertions.
//
// This replaces real Slack API calls during E2E tests, preventing message
// flood in production Slack channels while still validating bridge behavior.
//
// Usage:
//
//	slackmock [-addr :8080]
//
// Mock endpoints (Slack API surface):
//
//	POST /api/auth.test
//	POST /api/chat.postMessage
//	POST /api/chat.update
//	POST /api/chat.delete
//	POST /api/chat.postEphemeral
//	POST /api/conversations.history
//	POST /api/conversations.replies
//	POST /api/conversations.info
//	POST /api/conversations.open
//	POST /api/reactions.add
//	POST /api/reactions.remove
//	POST /api/pins.add
//	POST /api/pins.list
//	POST /api/users.info
//	POST /api/files.upload
//	POST /api/views.open
//	POST /api/views.update
//
// Query endpoints (for test assertions):
//
//	GET  /mock/calls                — all recorded API calls
//	GET  /mock/messages?channel=C   — messages in a channel
//	GET  /mock/messages?channel=C&thread_ts=T — thread replies
//	GET  /mock/stats                — call counts by method
//	POST /mock/reset                — clear all recorded state
//	GET  /healthz                   — health check
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mock := NewMockSlack(logger)
	mux := http.NewServeMux()

	// Slack API surface — accepts both GET and POST (Slack uses POST for most).
	// The slack-go library sends form-encoded POST requests.
	mux.HandleFunc("/api/auth.test", mock.handleAuthTest)
	mux.HandleFunc("/api/chat.postMessage", mock.handlePostMessage)
	mux.HandleFunc("/api/chat.update", mock.handleChatUpdate)
	mux.HandleFunc("/api/chat.delete", mock.handleChatDelete)
	mux.HandleFunc("/api/chat.postEphemeral", mock.handlePostEphemeral)
	mux.HandleFunc("/api/conversations.history", mock.handleConversationsHistory)
	mux.HandleFunc("/api/conversations.replies", mock.handleConversationsReplies)
	mux.HandleFunc("/api/conversations.info", mock.handleConversationsInfo)
	mux.HandleFunc("/api/conversations.open", mock.handleConversationsOpen)
	mux.HandleFunc("/api/reactions.add", mock.handleReactionsAdd)
	mux.HandleFunc("/api/reactions.remove", mock.handleReactionsRemove)
	mux.HandleFunc("/api/pins.add", mock.handlePinsAdd)
	mux.HandleFunc("/api/pins.list", mock.handlePinsList)
	mux.HandleFunc("/api/users.info", mock.handleUsersInfo)
	mux.HandleFunc("/api/files.upload", mock.handleFilesUpload)
	mux.HandleFunc("/api/views.open", mock.handleViewsOpen)
	mux.HandleFunc("/api/views.update", mock.handleViewsUpdate)

	// Query endpoints for test assertions.
	mux.HandleFunc("/mock/calls", mock.handleQueryCalls)
	mux.HandleFunc("/mock/messages", mock.handleQueryMessages)
	mux.HandleFunc("/mock/stats", mock.handleQueryStats)
	mux.HandleFunc("/mock/reset", mock.handleReset)

	// Health check.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","service":"slackmock"}`)
	})

	logger.Info("slackmock starting", "addr", *addr)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

// Message represents a stored Slack message.
type Message struct {
	Channel  string `json:"channel"`
	Text     string `json:"text"`
	Blocks   string `json:"blocks,omitempty"` // JSON string of blocks
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
	User     string `json:"user,omitempty"`
	BotID    string `json:"bot_id,omitempty"`
}

// Call records a single Slack API call.
type Call struct {
	Method    string            `json:"method"`
	Params    map[string]string `json:"params"`
	Timestamp time.Time         `json:"timestamp"`
}

// MockSlack is the in-memory Slack mock state.
type MockSlack struct {
	mu       sync.Mutex
	logger   *slog.Logger
	calls    []Call
	messages map[string][]Message // channel → messages
	pins     map[string][]string  // channel → pinned message timestamps
	tsSeq    int64                // monotonic timestamp counter
}

// NewMockSlack creates a new mock Slack instance.
func NewMockSlack(logger *slog.Logger) *MockSlack {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	return &MockSlack{
		logger:   logger,
		messages: make(map[string][]Message),
		pins:     make(map[string][]string),
		tsSeq:    time.Now().Unix(),
	}
}

// nextTS generates a unique Slack-style timestamp (seconds.sequence).
func (m *MockSlack) nextTS() string {
	m.tsSeq++
	return fmt.Sprintf("%d.%06d", m.tsSeq, m.tsSeq%1000000)
}

// recordCall records an API call for later querying.
func (m *MockSlack) recordCall(method string, params map[string]string) {
	m.calls = append(m.calls, Call{
		Method:    method,
		Params:    params,
		Timestamp: time.Now(),
	})
	m.logger.Info("slack API call", "method", method, "params", params)
}

// parseParams extracts form values or JSON body params from the request.
func parseParams(r *http.Request) map[string]string {
	params := make(map[string]string)

	// Try form data first (slack-go default).
	if err := r.ParseForm(); err == nil {
		for k, vs := range r.Form {
			if len(vs) > 0 {
				params[k] = vs[0]
			}
		}
	}

	// If no form params, try JSON body.
	if len(params) == 0 || (len(params) == 1 && params["token"] != "") {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			for k, v := range body {
				switch val := v.(type) {
				case string:
					params[k] = val
				default:
					b, _ := json.Marshal(val)
					params[k] = string(b)
				}
			}
		}
	}

	return params
}

// jsonOK writes a successful Slack API response.
func jsonOK(w http.ResponseWriter, extra map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"ok": true}
	for k, v := range extra {
		resp[k] = v
	}
	json.NewEncoder(w).Encode(resp)
}

// ── Slack API Handlers ─────────────────────────────────────────────

func (m *MockSlack) handleAuthTest(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordCall("auth.test", parseParams(r))
	jsonOK(w, map[string]any{
		"url":     "https://slackmock.test/",
		"team":    "E2E Test Team",
		"user":    "slackmock-bot",
		"team_id": "T00000000",
		"user_id": "U00000000",
		"bot_id":  "B00000000",
	})
}

func (m *MockSlack) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("chat.postMessage", params)

	ts := m.nextTS()
	msg := Message{
		Channel:  params["channel"],
		Text:     params["text"],
		Blocks:   params["blocks"],
		TS:       ts,
		ThreadTS: params["thread_ts"],
		BotID:    "B00000000",
	}
	m.messages[msg.Channel] = append(m.messages[msg.Channel], msg)

	jsonOK(w, map[string]any{
		"channel":    msg.Channel,
		"ts":         ts,
		"message_ts": ts,
		"message": map[string]any{
			"text":      msg.Text,
			"ts":        ts,
			"thread_ts": msg.ThreadTS,
			"bot_id":    msg.BotID,
		},
	})
}

func (m *MockSlack) handleChatUpdate(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("chat.update", params)

	channel := params["channel"]
	ts := params["ts"]

	// Find and update the message in-place.
	for i, msg := range m.messages[channel] {
		if msg.TS == ts {
			if text, ok := params["text"]; ok {
				m.messages[channel][i].Text = text
			}
			if blocks, ok := params["blocks"]; ok {
				m.messages[channel][i].Blocks = blocks
			}
			break
		}
	}

	jsonOK(w, map[string]any{
		"channel": channel,
		"ts":      ts,
	})
}

func (m *MockSlack) handleChatDelete(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("chat.delete", params)

	channel := params["channel"]
	ts := params["ts"]

	// Remove the message.
	msgs := m.messages[channel]
	for i, msg := range msgs {
		if msg.TS == ts {
			m.messages[channel] = append(msgs[:i], msgs[i+1:]...)
			break
		}
	}

	jsonOK(w, map[string]any{
		"channel": channel,
		"ts":      ts,
	})
}

// handlePostEphemeral records an ephemeral. Ephemerals never appear in
// history, so nothing is stored.
func (m *MockSlack) handlePostEphemeral(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("chat.postEphemeral", params)
	jsonOK(w, map[string]any{"message_ts": m.nextTS()})
}

func (m *MockSlack) handleConversationsHistory(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("conversations.history", params)

	channel := params["channel"]
	oldest := params["oldest"]
	latest := params["latest"]
	inclusive := params["inclusive"] == "true"
	limit := 100
	if l, err := strconv.Atoi(params["limit"]); err == nil && l > 0 {
		limit = l
	}

	var filtered []Message
	for _, msg := range m.messages[channel] {
		// Only top-level messages (not thread replies).
		if msg.ThreadTS != "" && msg.ThreadTS != msg.TS {
			continue
		}
		if oldest != "" && msg.TS <= oldest {
			continue
		}
		if latest != "" {
			if inclusive && msg.TS > latest {
				continue
			}
			if !inclusive && msg.TS >= latest {
				continue
			}
		}
		filtered = append(filtered, msg)
	}

	// Slack returns newest first.
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].TS > filtered[j].TS
	})

	if len(filtered) > limit {
		filtered = filtered[:limit]
	}

	// Build response messages with proper Slack shape.
	respMsgs := make([]map[string]any, len(filtered))
	for i, msg := range filtered {
		m := map[string]any{
			"type":   "message",
			"text":   msg.Text,
			"ts":     msg.TS,
			"bot_id": msg.BotID,
		}
		if msg.Blocks != "" {
			var blocks []any
			if err := json.Unmarshal([]byte(msg.Blocks), &blocks); err == nil {
				m["blocks"] = blocks
			}
		}
		if msg.ThreadTS != "" {
			m["thread_ts"] = msg.ThreadTS
		}
		respMsgs[i] = m
	}

	jsonOK(w, map[string]any{
		"messages":          respMsgs,
		"has_more":          false,
		"response_metadata": map[string]any{},
	})
}

func (m *MockSlack) handleConversationsReplies(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("conversations.replies", params)

	channel := params["channel"]
	threadTS := params["ts"]

	var replies []map[string]any
	for _, msg := range m.messages[channel] {
		if msg.ThreadTS == threadTS || msg.TS == threadTS {
			m := map[string]any{
				"type":      "message",
				"text":      msg.Text,
				"ts":        msg.TS,
				"thread_ts": threadTS,
			}
			if msg.Blocks != "" {
				var blocks []any
				if err := json.Unmarshal([]byte(msg.Blocks), &blocks); err == nil {
					m["blocks"] = blocks
				}
			}
			replies = append(replies, m)
		}
	}

	jsonOK(w, map[string]any{
		"messages": replies,
		"has_more": false,
	})
}

func (m *MockSlack) handleConversationsInfo(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("conversations.info", params)

	channel := params["channel"]
	jsonOK(w, map[string]any{
		"channel": map[string]any{
			"id":         channel,
			"name":       "mock-channel",
			"is_channel": true,
			"is_member":  true,
		},
	})
}

// handleConversationsOpen opens an IM (or MPIM): the channel id is "D" plus
// the sorted user ids, so the same users always get the same channel.
func (m *MockSlack) handleConversationsOpen(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("conversations.open", params)
	users := strings.Split(params["users"], ",")
	sort.Strings(users)
	jsonOK(w, map[string]any{"channel": map[string]any{"id": "D" + strings.Join(users, "")}})
}

func (m *MockSlack) handleReactionsAdd(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("reactions.add", params)
	jsonOK(w, nil)
}

func (m *MockSlack) handleReactionsRemove(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("reactions.remove", params)
	jsonOK(w, nil)
}

func (m *MockSlack) handlePinsAdd(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("pins.add", params)

	channel := params["channel"]
	ts := params["timestamp"]
	m.pins[channel] = append(m.pins[channel], ts)
	jsonOK(w, nil)
}

func (m *MockSlack) handlePinsList(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("pins.list", params)

	channel := params["channel"]
	var items []map[string]any
	for _, ts := range m.pins[channel] {
		items = append(items, map[string]any{
			"type": "message",
			"message": map[string]any{
				"ts": ts,
			},
		})
	}

	jsonOK(w, map[string]any{"items": items})
}

func (m *MockSlack) handleUsersInfo(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("users.info", params)

	user := params["user"]
	jsonOK(w, map[string]any{
		"user": map[string]any{
			"id":        user,
			"name":      "mockuser",
			"real_name": "Mock User",
			"profile": map[string]any{
				"display_name": "Mock User",
				"real_name":    "Mock User",
			},
		},
	})
}

func (m *MockSlack) handleFilesUpload(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("files.upload", params)
	jsonOK(w, map[string]any{
		"file": map[string]any{
			"id":   "F00000001",
			"name": params["filename"],
		},
	})
}

func (m *MockSlack) handleViewsOpen(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("views.open", params)
	jsonOK(w, map[string]any{
		"view": map[string]any{
			"id":   "V00000001",
			"type": "modal",
		},
	})
}

func (m *MockSlack) handleViewsUpdate(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	params := parseParams(r)
	m.recordCall("views.update", params)
	jsonOK(w, map[string]any{
		"view": map[string]any{
			"id":   params["view_id"],
			"type": "modal",
		},
	})
}

// ── Query Handlers ─────────────────────────────────────────────────

func (m *MockSlack) handleQueryCalls(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	method := r.URL.Query().Get("method")
	var filtered []Call
	for _, c := range m.calls {
		if method == "" || c.Method == method {
			filtered = append(filtered, c)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"calls": filtered,
		"total": len(filtered),
	})
}

func (m *MockSlack) handleQueryMessages(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	channel := r.URL.Query().Get("channel")
	threadTS := r.URL.Query().Get("thread_ts")
	search := r.URL.Query().Get("search")

	var result []Message
	for ch, msgs := range m.messages {
		if channel != "" && ch != channel {
			continue
		}
		for _, msg := range msgs {
			if threadTS != "" && msg.ThreadTS != threadTS && msg.TS != threadTS {
				continue
			}
			if search != "" && !strings.Contains(msg.Text, search) && !strings.Contains(msg.Blocks, search) {
				continue
			}
			result = append(result, msg)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"messages": result,
		"total":    len(result),
	})
}

func (m *MockSlack) handleQueryStats(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	counts := make(map[string]int)
	for _, c := range m.calls {
		counts[c.Method]++
	}

	totalMessages := 0
	for _, msgs := range m.messages {
		totalMessages += len(msgs)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"call_counts":    counts,
		"total_calls":    len(m.calls),
		"total_messages": totalMessages,
		"channels":       len(m.messages),
	})
}

func (m *MockSlack) handleReset(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = nil
	m.messages = make(map[string][]Message)
	m.pins = make(map[string][]string)
	m.logger.Info("mock state reset")

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"message":"state reset"}`)
}
