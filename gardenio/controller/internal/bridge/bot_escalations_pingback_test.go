package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// capturedSlackCall records one chat.postMessage form payload.
type capturedSlackCall struct {
	Path   string
	Params map[string]string
}

// newPingbackSlackServer returns a fake Slack API server that records every
// call's form parameters, for asserting channel / thread_ts / reply_broadcast.
// Distinct from bot_mentions_test's newCapturingSlackServer: this one parses
// individual form params and returns a channel id in the response so message
// tracking is exercised.
func newPingbackSlackServer(t *testing.T) (*httptest.Server, *[]capturedSlackCall, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var calls []capturedSlackCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		params := make(map[string]string, len(r.Form))
		for k := range r.Form {
			params[k] = r.Form.Get(k)
		}
		mu.Lock()
		calls = append(calls, capturedSlackCall{Path: r.URL.Path, Params: params})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": "C999", "ts": "1234.5678", "message_ts": "1234.5678",
		})
	}))
	return srv, &calls, &mu
}

func TestNotifyEscalationPingback_ThreadBroadcastOnTrackedCard(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := newPingbackSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	// Original card is tracked → pingback must thread onto it and broadcast.
	bot.messages["gd-pb1"] = MessageRef{ChannelID: "C111", Timestamp: "111.222"}

	bead := BeadEvent{
		ID:       "gd-pb1",
		Type:     "escalation",
		Title:    "Rotate prod DB credentials",
		Priority: 1,
		Fields: map[string]string{
			"pingback_count":  "2",
			"pingback_source": "auto",
		},
	}
	if err := bot.NotifyEscalationPingback(context.Background(), bead); err != nil {
		t.Fatalf("NotifyEscalationPingback: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("got %d Slack calls, want 1", len(*calls))
	}
	call := (*calls)[0]
	if call.Params["channel"] != "C111" {
		t.Errorf("channel = %q, want C111 (original card channel)", call.Params["channel"])
	}
	if call.Params["thread_ts"] != "111.222" {
		t.Errorf("thread_ts = %q, want 111.222", call.Params["thread_ts"])
	}
	if call.Params["reply_broadcast"] != "true" {
		t.Errorf("reply_broadcast = %q, want true — pingback must re-surface in channel", call.Params["reply_broadcast"])
	}
}

func TestNotifyEscalationPingback_FreshCardWhenUntracked(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := newPingbackSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.channel = "CDEFAULT"

	bead := BeadEvent{
		ID:    "gd-pb2",
		Type:  "escalation",
		Title: "Grant IAM role",
		Fields: map[string]string{
			"pingback_count": "1",
		},
	}
	if err := bot.NotifyEscalationPingback(context.Background(), bead); err != nil {
		t.Fatalf("NotifyEscalationPingback: %v", err)
	}

	mu.Lock()
	if len(*calls) != 1 {
		mu.Unlock()
		t.Fatalf("got %d Slack calls, want 1", len(*calls))
	}
	call := (*calls)[0]
	mu.Unlock()
	if call.Params["channel"] != "CDEFAULT" {
		t.Errorf("channel = %q, want CDEFAULT", call.Params["channel"])
	}
	if call.Params["thread_ts"] != "" {
		t.Errorf("untracked pingback must post top-level, got thread_ts=%q", call.Params["thread_ts"])
	}

	// The fresh card must now be tracked so the NEXT pingback threads on it.
	if ref, ok := bot.lookupMessage("gd-pb2"); !ok || ref.ChannelID != "C999" {
		t.Errorf("fresh pingback card not tracked: ok=%v ref=%+v", ok, ref)
	}
}

func TestNotifyEscalationQueueAlert_UsesQueueChannelAndMention(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := newPingbackSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.channel = "CDEFAULT"
	bot.escalationQueueChannel = "CQUEUE"
	bot.escalationQueueMention = "!subteam^S123"

	bead := BeadEvent{
		ID:       "gd-qa1",
		Type:     "escalation",
		Title:    "Kafka helm-apply",
		Priority: 0,
		Fields: map[string]string{
			"operation_type":  "infrastructure",
			"requesting_task": "gd-task1",
		},
	}
	if err := bot.NotifyEscalationQueueAlert(context.Background(), bead); err != nil {
		t.Fatalf("NotifyEscalationQueueAlert: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("got %d Slack calls, want 1", len(*calls))
	}
	call := (*calls)[0]
	if call.Params["channel"] != "CQUEUE" {
		t.Errorf("channel = %q, want CQUEUE", call.Params["channel"])
	}
	blocks := call.Params["blocks"]
	if blocks == "" {
		t.Fatal("queue alert posted without blocks")
	}
	if !strings.Contains(blocks, "!subteam^S123") {
		t.Errorf("queue alert blocks missing mention token: %s", blocks)
	}
}

func TestNotifyEscalationQueueAlert_FallsBackToDefaultChannel(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := newPingbackSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.channel = "CDEFAULT"

	bead := BeadEvent{ID: "gd-qa2", Type: "escalation", Title: "stalled"}
	if err := bot.NotifyEscalationQueueAlert(context.Background(), bead); err != nil {
		t.Fatalf("NotifyEscalationQueueAlert: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("got %d Slack calls, want 1", len(*calls))
	}
	if got := (*calls)[0].Params["channel"]; got != "CDEFAULT" {
		t.Errorf("channel = %q, want CDEFAULT fallback", got)
	}
}
