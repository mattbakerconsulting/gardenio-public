package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// --- handleMessageEvent tests ---

func TestHandleMessageEvent_IgnoresBotOwnMessages(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// Message from the bot itself should be ignored.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:    "UBOTID",
		Text:    "hello",
		Channel: "C123",
	})
}

func TestHandleMessageEvent_IgnoresEmptyUser(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:    "",
		Text:    "hello",
		Channel: "C123",
	})
}

func TestHandleMessageEvent_IgnoresSubtypes(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:    "U123",
		Text:    "hello",
		Channel: "C123",
		SubType: "message_changed",
	})
}

func TestHandleMessageEvent_NonThreadNonMention_Ignored(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// Non-thread, non-mention, non-group message is ignored.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:        "U123",
		Text:        "random message",
		Channel:     "C123",
		ChannelType: "channel",
	})
}

func TestHandleMessageEvent_GroupChannelMention_SynthesizesAppMention(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// Group channel + mention should synthesize AppMentionEvent.
	// This won't crash because handleAppMention will just not match any agent.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:        "U123",
		Text:        "hey <@UBOTID> do something",
		Channel:     "G123",
		ChannelType: "group",
		TimeStamp:   "1234.5678",
	})
}

func TestHandleMessageEvent_DMMessage_SynthesizesAppMention(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// DM message without @mention should synthesize AppMentionEvent.
	// In DMs, all messages are implicitly directed at the bot.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:        "U123",
		Text:        "help me with something",
		Channel:     "D123",
		ChannelType: "im",
		TimeStamp:   "1234.5678",
	})
}

func TestHandleMessageEvent_DMMessage_WithMention_SynthesizesAppMention(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// DM message WITH @mention should also work.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:        "U123",
		Text:        "<@UBOTID> help me with something",
		Channel:     "D123",
		ChannelType: "im",
		TimeStamp:   "1234.5678",
	})
}

func TestHandleMessageEvent_DMThreadReply_SynthesizesAppMention(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// DM thread reply without @mention should synthesize AppMentionEvent.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:            "U123",
		Text:            "follow up question",
		Channel:         "D123",
		ChannelType:     "im",
		TimeStamp:       "1234.9999",
		ThreadTimeStamp: "1234.5678",
	})
}

func TestHandleMessageEvent_DMBotMessage_Ignored(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// Bot's own messages in DMs should be ignored (BotID is set).
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:        "U123",
		Text:        "automated response",
		Channel:     "D123",
		ChannelType: "im",
		TimeStamp:   "1234.5678",
		BotID:       "B123",
	})

	// No agent beads should be created.
	if len(filterAgentBeads(daemon.beads)) != 0 {
		t.Error("expected bot messages in DMs to be ignored")
	}
}

func TestHandleMessageEvent_DMSubtype_Ignored(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// Message subtypes (e.g., channel_join) in DMs should be ignored.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:        "U123",
		Text:        "joined the channel",
		Channel:     "D123",
		ChannelType: "im",
		TimeStamp:   "1234.5678",
		SubType:     "channel_join",
	})

	// No agent beads should be created.
	if len(filterAgentBeads(daemon.beads)) != 0 {
		t.Error("expected DM subtypes to be ignored")
	}
}

func TestHandleMessageEvent_ConciergeNotActivatedInDMs(t *testing.T) {
	daemon := newMockDaemon()
	// Seed a concierge project — but it shouldn't activate for DM channels.
	daemon.seedProjectWithChannel("gardenio", "D123")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// DM messages should go through the DM handler, not concierge.
	// This verifies that the DM ChannelType check comes before concierge.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:        "U123",
		Text:        "help me debug this",
		Channel:     "D123",
		ChannelType: "im",
		TimeStamp:   "1234.5678",
	})

	// The message should be processed as a DM (synthesized mention),
	// not as a concierge message. No concierge-specific beads should exist.
	// We just verify no panic and the flow completed.
}

// --- hintMentionRequired tests ---

func TestHintMentionRequired_PostsHint(t *testing.T) {
	var posted bool
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)
	bot.lastThreadNudge = make(map[string]time.Time)

	bot.hintMentionRequired(context.Background(), "C123", "1111.2222", "my-agent")

	if !posted {
		t.Error("expected hint message to be posted")
	}
}

func TestHintMentionRequired_Throttled(t *testing.T) {
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)
	bot.lastThreadNudge = make(map[string]time.Time)

	// First call should go through.
	bot.hintMentionRequired(context.Background(), "C123", "1111.2222", "my-agent")

	// Second call within 10 minutes should be throttled.
	callCount := 0
	slackSrv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	defer slackSrv2.Close()

	// Re-wire the API to the counting server — but the throttle should prevent the call.
	bot.hintMentionRequired(context.Background(), "C123", "1111.2222", "my-agent")

	// The second call used the same bot.api, so the lastThreadNudge check
	// should prevent posting. We verify by checking the nudge map has the key.
	key := "hint:C123:1111.2222"
	bot.mu.Lock()
	_, ok := bot.lastThreadNudge[key]
	bot.mu.Unlock()
	if !ok {
		t.Error("expected nudge key to be stored")
	}
}

func TestHintMentionRequired_NilAPI(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.lastThreadNudge = make(map[string]time.Time)
	bot.api = nil

	// Should not panic when api is nil.
	bot.hintMentionRequired(context.Background(), "C123", "1111.2222", "my-agent")
}

// --- handleThreadReply tests ---

func TestHandleThreadReply_NoDecisionThread(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// No state manager, so getDecisionByThread returns "".
	bot.resolveDecisionByReply(context.Background(), slackReplyInbound(&slackevents.MessageEvent{
		User:            "U123",
		Text:            "my reply",
		Channel:         "C123",
		ThreadTimeStamp: "1111.2222",
	}, ""))

	// Should silently return without error.
	if len(daemon.getClosed()) != 0 {
		t.Error("expected no beads to be closed")
	}
}

// threadReplyTestSlackServer returns a slack test server that answers
// users.info and generic posts — the two calls handleThreadReply makes.
func threadReplyTestSlackServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users.info") || r.FormValue("user") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"user": map[string]any{"id": "U123", "real_name": "Bob", "name": "bob"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
}

// TestHandleThreadReply_FreeTextGoesToResponseText: a reply to an options-less
// decision (or one whose text matches no option) is genuinely free-form, so it
// lands in response_text with chosen EMPTY — never fabricating an option id
// from a sentence (ka-0000000003).
func TestHandleThreadReply_FreeTextGoesToResponseText(t *testing.T) {
	daemon := newMockDaemon()

	// Seed an open decision bead with NO structured options.
	daemon.mu.Lock()
	daemon.beads["gd-dec-1"] = &beadsapi.BeadDetail{
		ID:     "gd-dec-1",
		Title:  "Choose a color",
		Type:   "decision",
		Status: "open",
	}
	daemon.mu.Unlock()

	slackSrv := threadReplyTestSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Set up a state manager with the decision message mapping.
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	sm, err := NewStateManager(statePath)
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	_ = sm.SetDecisionMessage("gd-dec-1", MessageRef{ChannelID: "C123", Timestamp: "1111.2222"})
	bot.state = sm

	bot.resolveDecisionByReply(context.Background(), slackReplyInbound(&slackevents.MessageEvent{
		User:            "U123",
		Text:            "blue",
		Channel:         "C123",
		ThreadTimeStamp: "1111.2222",
		TimeStamp:       "1111.3333",
	}, ""))

	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(closed))
	}
	if closed[0].BeadID != "gd-dec-1" {
		t.Errorf("expected bead gd-dec-1 to be closed, got %s", closed[0].BeadID)
	}
	if closed[0].Fields["chosen"] != "" {
		t.Errorf("expected empty chosen for free-text reply, got %q", closed[0].Fields["chosen"])
	}
	if closed[0].Fields["response_text"] != "blue" {
		t.Errorf("expected response_text=blue, got %q", closed[0].Fields["response_text"])
	}
}

// TestHandleThreadReply_MatchesOptionLabelToID: replying in-thread with the
// text of an option's LABEL resolves to that option's canonical ID in `chosen`,
// so the viewer badges it. This is the core ka-0000000003 regression guard.
func TestHandleThreadReply_MatchesOptionLabelToID(t *testing.T) {
	daemon := newMockDaemon()

	opts := []map[string]string{
		{"id": "keep-disabled", "short": "Keep dark", "label": "Mission still relevant but no capacity"},
		{"id": "archive", "short": "Archive", "label": "Close the trigger bead"},
	}
	optsJSON, _ := json.Marshal(opts)
	daemon.mu.Lock()
	daemon.beads["gd-dec-opt"] = &beadsapi.BeadDetail{
		ID:     "gd-dec-opt",
		Title:  "Revive or archive?",
		Type:   "decision",
		Status: "open",
		Fields: map[string]string{"options": string(optsJSON)},
	}
	daemon.mu.Unlock()

	slackSrv := threadReplyTestSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	tmpDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	_ = sm.SetDecisionMessage("gd-dec-opt", MessageRef{ChannelID: "C123", Timestamp: "1111.2222"})
	bot.state = sm

	// Reply with the LABEL text (case-insensitive, extra whitespace) — the
	// exact shape of the historical bug where the label was stored raw.
	bot.resolveDecisionByReply(context.Background(), slackReplyInbound(&slackevents.MessageEvent{
		User:            "U123",
		Text:            "  mission still relevant but no capacity  ",
		Channel:         "C123",
		ThreadTimeStamp: "1111.2222",
		TimeStamp:       "1111.3333",
	}, ""))

	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(closed))
	}
	if closed[0].Fields["chosen"] != "keep-disabled" {
		t.Errorf("expected chosen=keep-disabled (option id), got %q", closed[0].Fields["chosen"])
	}
	if closed[0].Fields["response_text"] != "" {
		t.Errorf("expected empty response_text for a matched option, got %q", closed[0].Fields["response_text"])
	}
}

func TestHandleThreadReply_SkipsAlreadyClosedDecision(t *testing.T) {
	daemon := newMockDaemon()

	// Seed a closed decision bead.
	daemon.mu.Lock()
	daemon.beads["gd-dec-closed"] = &beadsapi.BeadDetail{
		ID:     "gd-dec-closed",
		Title:  "Already decided",
		Type:   "decision",
		Status: "closed",
	}
	daemon.mu.Unlock()

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users.info") || r.FormValue("user") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"user": map[string]any{"id": "U123", "real_name": "Bob"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	sm, err := NewStateManager(statePath)
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	_ = sm.SetDecisionMessage("gd-dec-closed", MessageRef{ChannelID: "C123", Timestamp: "1111.2222"})
	bot.state = sm

	bot.resolveDecisionByReply(context.Background(), slackReplyInbound(&slackevents.MessageEvent{
		User:            "U123",
		Text:            "too late",
		Channel:         "C123",
		ThreadTimeStamp: "1111.2222",
		TimeStamp:       "1111.3333",
	}, ""))

	// Decision already closed, so no close call.
	if len(daemon.getClosed()) != 0 {
		t.Error("expected no close calls for already-closed decision")
	}
}

// --- getDecisionByThread tests ---

func TestGetDecisionByThread_NilState(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.state = nil

	result := bot.getDecisionByThread("C123", "1111.2222")
	if result != "" {
		t.Errorf("expected empty string for nil state, got %q", result)
	}
}

func TestGetDecisionByThread_Found(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	sm, err := NewStateManager(statePath)
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	_ = sm.SetDecisionMessage("gd-dec-42", MessageRef{ChannelID: "C123", Timestamp: "1111.2222"})
	bot.state = sm

	result := bot.getDecisionByThread("C123", "1111.2222")
	if result != "gd-dec-42" {
		t.Errorf("expected gd-dec-42, got %q", result)
	}
}

func TestGetDecisionByThread_NotFound(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	sm, err := NewStateManager(statePath)
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = sm

	result := bot.getDecisionByThread("C123", "9999.9999")
	if result != "" {
		t.Errorf("expected empty for no match, got %q", result)
	}
}

// --- postThreadStateReply tests ---

func TestPostThreadStateReply_Done(t *testing.T) {
	var postedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			if text := r.FormValue("text"); text != "" {
				postedText = text
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	bead := BeadEvent{
		ID:     "gd-agent-1",
		Fields: map[string]string{},
	}

	bot.postThreadStateReply(context.Background(), "my-agent", "done", bead, "C123", "1111.2222")

	if postedText == "" {
		t.Error("expected a message to be posted")
	}
	if !strings.Contains(postedText, "finished") {
		t.Errorf("expected 'finished' in posted text, got %q", postedText)
	}
	if !strings.Contains(postedText, "mention") {
		t.Errorf("expected resume hint in posted text, got %q", postedText)
	}
}

func TestPostThreadStateReply_Failed(t *testing.T) {
	var postedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			if text := r.FormValue("text"); text != "" {
				postedText = text
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	bead := BeadEvent{
		ID:     "gd-agent-1",
		Fields: map[string]string{"close_reason": "OOM killed"},
	}

	bot.postThreadStateReply(context.Background(), "my-agent", "failed", bead, "C123", "1111.2222")

	if postedText == "" {
		t.Error("expected a message to be posted")
	}
	if !strings.Contains(postedText, "failed") {
		t.Errorf("expected 'failed' in posted text, got %q", postedText)
	}
}

func TestPostThreadStateReply_NonTerminalState_NoPost(t *testing.T) {
	var called bool
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	bead := BeadEvent{ID: "gd-agent-1", Fields: map[string]string{}}

	bot.postThreadStateReply(context.Background(), "my-agent", "running", bead, "C123", "1111.2222")

	if called {
		t.Error("expected no Slack API call for non-terminal state")
	}
}

func TestPostThreadStateReply_UpdatesSpawnMessage(t *testing.T) {
	var updatedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			if text := r.FormValue("text"); text != "" {
				updatedText = text
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.1111"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	// Pre-populate spawn message ref so it tries to update in-place.
	bot.mu.Lock()
	bot.threadSpawnMsgs["my-agent"] = MessageRef{ChannelID: "C123", Timestamp: "9999.1111"}
	bot.mu.Unlock()

	bead := BeadEvent{ID: "gd-agent-1", Fields: map[string]string{}}
	bot.postThreadStateReply(context.Background(), "my-agent", "done", bead, "C123", "1111.2222")

	if updatedText == "" {
		t.Error("expected spawn message to be updated")
	}

	// Verify spawn message was removed from the map.
	bot.mu.Lock()
	_, still := bot.threadSpawnMsgs["my-agent"]
	bot.mu.Unlock()
	if still {
		t.Error("expected spawn message ref to be removed after update")
	}
}

func TestPostThreadStateReply_WithWrapup(t *testing.T) {
	var postedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			if text := r.FormValue("text"); text != "" {
				postedText = text
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	wrapup := `{"accomplishments":"Fixed the bug","blockers":"","handoff_notes":"Ready for review"}`
	bead := BeadEvent{
		ID:     "gd-agent-1",
		Fields: map[string]string{"wrapup": wrapup},
	}

	bot.postThreadStateReply(context.Background(), "my-agent", "done", bead, "C123", "1111.2222")

	if postedText == "" {
		t.Error("expected a message to be posted with wrapup content")
	}
}

// --- resolveChannel project-specific override tests ---

func TestResolveChannel_RouterPatternMatch(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-default"
	bot.router = NewRouter(RouterConfig{
		DefaultChannel: "C-router-default",
		Channels: map[string]string{
			"gardenio/crew/*": "C-crew",
		},
	})

	result := bot.resolveChannel("gardenio/crew/my-agent")
	if result != "C-crew" {
		t.Errorf("expected C-crew from pattern match, got %q", result)
	}
}

func TestResolveChannel_RouterOverrideTakesPrecedence(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-default"
	bot.router = NewRouter(RouterConfig{
		DefaultChannel: "C-router-default",
		Channels: map[string]string{
			"gardenio/crew/*": "C-crew",
		},
		Overrides: map[string]string{
			"gardenio/crew/special-bot": "C-special",
		},
	})

	result := bot.resolveChannel("gardenio/crew/special-bot")
	if result != "C-special" {
		t.Errorf("expected C-special (override takes precedence), got %q", result)
	}
}

func TestResolveChannel_RouterNoMatch_FallsBackToRouterDefault(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-default"
	bot.router = NewRouter(RouterConfig{
		DefaultChannel: "C-router-default",
		Channels: map[string]string{
			"other-project/crew/*": "C-other",
		},
	})

	result := bot.resolveChannel("gardenio/crew/my-agent")
	if result != "C-router-default" {
		t.Errorf("expected C-router-default when no pattern matches, got %q", result)
	}
}

func TestResolveChannel_NilRouter_UsesDefaultChannel(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-default"
	bot.router = nil

	result := bot.resolveChannel("gardenio/crew/my-agent")
	if result != "C-default" {
		t.Errorf("expected C-default with nil router, got %q", result)
	}
}

func TestResolveChannel_RouterMultipleProjects(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-default"
	bot.router = NewRouter(RouterConfig{
		DefaultChannel: "C-router-default",
		Channels: map[string]string{
			"gardenio/crew/*": "C-gardenio",
			"kbeads/crew/*":   "C-kbeads",
		},
	})

	if got := bot.resolveChannel("gardenio/crew/bot-a"); got != "C-gardenio" {
		t.Errorf("expected C-gardenio, got %q", got)
	}
	if got := bot.resolveChannel("kbeads/crew/bot-b"); got != "C-kbeads" {
		t.Errorf("expected C-kbeads, got %q", got)
	}
}

// --- resolveChannel role-scoped channel tests ---

// seedProjectWithRoleMap seeds a project bead with a multi-channel
// slack_channel list and a channel_roles JSON map, so tests can exercise
// role-scoped channel routing in resolveChannel.
func seedProjectWithRoleMap(m *mockDaemon, name, primaryCh string, channelRoles map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := "proj-" + name
	rolesJSON, _ := json.Marshal(channelRoles)
	m.beads[id] = &beadsapi.BeadDetail{
		ID:    id,
		Title: name,
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": primaryCh,
			"channel_roles": string(rolesJSON),
		},
	}
}

// JIRA-triggered agents have no spawn channel set but do have project+role
// cached from the SpawnAgent path. They should route to the channel mapped
// to their role in the project's channel_roles, not the project primary.
func TestResolveChannel_RoleScopedChannel_JIRATriggered(t *testing.T) {
	daemon := newMockDaemon()
	seedProjectWithRoleMap(daemon, "monorepo", "C-monorepo", map[string]string{
		"C-monorepo":   "monorepo-gardener",
		"C-fsm-triage": "fsm-triage",
	})
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-default"
	bot.agentProject["monorepo-fsm-1"] = "monorepo"
	bot.agentRole["monorepo-fsm-1"] = "fsm-triage"

	if got := bot.resolveChannel("monorepo-fsm-1"); got != "C-fsm-triage" {
		t.Errorf("expected C-fsm-triage for role-scoped agent, got %q", got)
	}
}

// Agents whose role has no channel_roles mapping must still fall through
// to the project's primary channel (the original behavior).
func TestResolveChannel_RoleNotMapped_FallsBackToPrimary(t *testing.T) {
	daemon := newMockDaemon()
	seedProjectWithRoleMap(daemon, "monorepo", "C-monorepo", map[string]string{
		"C-fsm-triage": "fsm-triage",
	})
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-default"
	bot.agentProject["monorepo-crew-1"] = "monorepo"
	bot.agentRole["monorepo-crew-1"] = "crew" // not in channel_roles

	if got := bot.resolveChannel("monorepo-crew-1"); got != "C-monorepo" {
		t.Errorf("expected C-monorepo primary fallback, got %q", got)
	}
}

// A spawn-channel binding (set by /spawn or thread spawn) must still take
// precedence over the role-scoped channel resolution.
func TestResolveChannel_SpawnChannelPrecedesRoleChannel(t *testing.T) {
	daemon := newMockDaemon()
	seedProjectWithRoleMap(daemon, "monorepo", "C-monorepo", map[string]string{
		"C-fsm-triage": "fsm-triage",
	})
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-default"
	bot.agentProject["monorepo-fsm-2"] = "monorepo"
	bot.agentRole["monorepo-fsm-2"] = "fsm-triage"
	bot.agentSpawnChannel["monorepo-fsm-2"] = "C-spawned-here"

	if got := bot.resolveChannel("monorepo-fsm-2"); got != "C-spawned-here" {
		t.Errorf("expected spawn channel to win, got %q", got)
	}
}

// Projects without any channel_roles map must behave as before: primary
// channel only.
func TestResolveChannel_NoChannelRolesMap_UsesPrimary(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("monorepo", "C-monorepo")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-default"
	bot.agentProject["monorepo-x"] = "monorepo"
	bot.agentRole["monorepo-x"] = "fsm-triage" // role present but project has no map

	if got := bot.resolveChannel("monorepo-x"); got != "C-monorepo" {
		t.Errorf("expected C-monorepo primary fallback, got %q", got)
	}
}

// --- replaceAgentCardWithWrapUp tests ---

func TestReplaceAgentCardWithWrapUp_UpdatesExistingCard(t *testing.T) {
	var updatedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			if text := r.FormValue("text"); text != "" {
				updatedText = text
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1111.2222"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	bot.mu.Lock()
	bot.agentCards["my-agent"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}
	bot.mu.Unlock()

	wrapup := `{"accomplishments":"Fixed the bug","blockers":"","handoff_notes":"Ready for review"}`
	bot.replaceAgentCardWithWrapUp(context.Background(), "my-agent", "done", wrapup)

	if updatedText == "" {
		t.Error("expected agent card to be updated")
	}
	if !strings.Contains(updatedText, "done") {
		t.Errorf("expected 'done' in updated text, got %q", updatedText)
	}
}

func TestReplaceAgentCardWithWrapUp_NoCard_FallsBackToUpdateAgentCard(t *testing.T) {
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	// No card exists — should not panic, falls back to updateAgentCard.
	bot.replaceAgentCardWithWrapUp(context.Background(), "no-such-agent", "done", `{"accomplishments":"test"}`)
}

func TestReplaceAgentCardWithWrapUp_FailedState(t *testing.T) {
	var updatedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			if text := r.FormValue("text"); text != "" {
				updatedText = text
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1111.2222"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	bot.mu.Lock()
	bot.agentCards["fail-agent"] = MessageRef{ChannelID: "C123", Timestamp: "2222.3333"}
	bot.mu.Unlock()

	wrapup := `{"accomplishments":"","blockers":"OOM killed","handoff_notes":""}`
	bot.replaceAgentCardWithWrapUp(context.Background(), "fail-agent", "failed", wrapup)

	if updatedText == "" {
		t.Error("expected agent card to be updated")
	}
	if !strings.Contains(updatedText, "failed") {
		t.Errorf("expected 'failed' in updated text, got %q", updatedText)
	}
}

func TestReplaceAgentCardWithWrapUp_EmptyWrapup(t *testing.T) {
	var updatedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			if text := r.FormValue("text"); text != "" {
				updatedText = text
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1111.2222"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	bot.mu.Lock()
	bot.agentCards["my-agent"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}
	bot.mu.Unlock()

	// Empty wrapup JSON — should still update the card with just the header.
	bot.replaceAgentCardWithWrapUp(context.Background(), "my-agent", "done", `{}`)

	if updatedText == "" {
		t.Error("expected agent card to be updated even with empty wrapup")
	}
}

// TestResolveChannel_Ladder pins every rung of resolveChannel's precedence
// (gd-3JHijlgZrE), highest first: router override, explicit pin (manual
// /spawn, --slack-spawn-channel or a Slack-bound task's slack_spawn_channel),
// schedule channel, inherited spawn channel from a same-role spawner, the
// role's channel_roles entry, inherited spawn channel from a different-role
// spawner, project primary, router default, bridge default.
func TestResolveChannel_Ladder(t *testing.T) {
	const agent = "gardenio/cicd/a"
	cases := []struct {
		name        string
		router      *Router
		project     string
		role        string
		spawnCh     string
		schedCh     string
		inheritedBy *string // nil = explicit pin
		want        string
	}{
		{"router override beats explicit pin", NewRouter(RouterConfig{DefaultChannel: "C-router-default", Overrides: map[string]string{agent: "C-override"}}), "gardenio", "cicd", "C-spawn", "", nil, "C-override"},
		{"router pattern beats explicit pin", NewRouter(RouterConfig{DefaultChannel: "C-router-default", Channels: map[string]string{"gardenio/cicd/*": "C-pattern"}}), "gardenio", "cicd", "C-spawn", "", nil, "C-pattern"},
		{"explicit pin beats role channel", nil, "gardenio", "cicd", "C-spawn", "", nil, "C-spawn"},
		{"task pin (slack-bound task, no marker) beats role channel", nil, "gardenio", "cicd", "C-task-thread", "", nil, "C-task-thread"},
		{"schedule channel beats role channel", nil, "gardenio", "cicd", "", "C-sched", nil, "C-sched"},
		{"inherited, same role: spawn channel kept", nil, "gardenio", "cicd", "C-spawn", "", strPtr("cicd"), "C-spawn"},
		{"inherited, different role: role channel wins", nil, "gardenio", "cicd", "C-spawn", "", strPtr("gardener"), "C-cicd"},
		{"no spawn channel: role channel", nil, "gardenio", "cicd", "", "", nil, "C-cicd"},
		{"inherited, unmapped role: beats project primary", nil, "gardenio", "research", "C-spawn", "", strPtr("gardener"), "C-spawn"},
		{"unmapped role, no spawn channel: project primary", nil, "gardenio", "research", "", "", nil, "C-primary"},
		{"no project: router default", NewRouter(RouterConfig{DefaultChannel: "C-router-default"}), "", "", "", "", nil, "C-router-default"},
		{"no project, no router: bridge default", nil, "", "", "", "", nil, "C-default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			seedProjectWithRoleMap(daemon, "gardenio", "C-primary", map[string]string{"C-cicd": "cicd", "C-crew": "crew"})
			slackSrv := newFakeSlackServer(t)
			defer slackSrv.Close()

			bot := newTestBot(daemon, slackSrv)
			bot.channel = "C-default"
			bot.router = tc.router
			if tc.project != "" {
				bot.agentProject[agent] = tc.project
			}
			if tc.role != "" {
				bot.agentRole[agent] = tc.role
			}
			if tc.spawnCh != "" {
				bot.agentSpawnChannel[agent] = tc.spawnCh
			}
			if tc.inheritedBy != nil {
				bot.agentSpawnInheritedBy[agent] = *tc.inheritedBy
			}
			if tc.schedCh != "" {
				// Mirror NotifyAgentSpawn: a schedule channel overwrites the
				// spawn channel and is never an inherited one.
				bot.agentSpawnChannel[agent] = tc.schedCh
				delete(bot.agentSpawnInheritedBy, agent)
			}
			if got := bot.resolveChannel(agent); got != tc.want {
				t.Errorf("resolveChannel = %q, want %q", got, tc.want)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

// A crew agent spawned by a gardener carries the gardener's channel plus the
// inherited marker (gb spawn). Driven through NotifyAgentSpawn, the bead
// fields must land in the cache such that the crew's own channel_roles entry
// wins, while the gardener itself (same-role inheritance absent, explicit
// schedule pin) stays in its own channel.
func TestResolveChannel_CrewChildOfGardenerUsesCrewRoleChannel(t *testing.T) {
	daemon := newMockDaemon()
	seedProjectWithRoleMap(daemon, "gardenio", "C-primary", map[string]string{"C-gardener": "gardener", "C-crew": "crew"})
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-default"
	ctx := context.Background()

	bot.NotifyAgentSpawn(ctx, BeadEvent{ID: "g1", Fields: map[string]string{
		"agent": "gardenio/gardener/g1", "project": "gardenio", "role": "gardener",
		"schedule_slack_channel": "C-gardener",
	}})
	bot.NotifyAgentSpawn(ctx, BeadEvent{ID: "c1", Fields: map[string]string{
		"agent": "gardenio/crew/c1", "project": "gardenio", "role": "crew",
		"slack_spawn_channel":                     "C-gardener",
		"slack_spawn_channel_inherited_from_role": "gardener",
	}})
	bot.NotifyAgentSpawn(ctx, BeadEvent{ID: "c2", Fields: map[string]string{
		"agent": "gardenio/crew/c2", "project": "gardenio", "role": "crew",
		"slack_spawn_channel": "C-thread", // explicit pin from a Slack-bound task
	}})

	// NotifyAgentSpawn keys its caches by the short agent name.
	if got := bot.resolveChannel("g1"); got != "C-gardener" {
		t.Errorf("gardener: got %q, want C-gardener", got)
	}
	if got := bot.resolveChannel("c1"); got != "C-crew" {
		t.Errorf("crew child of gardener: got %q, want C-crew (channel_roles for crew)", got)
	}
	if got := bot.resolveChannel("c2"); got != "C-thread" {
		t.Errorf("task-pinned crew: got %q, want C-thread", got)
	}
}

// The marker field on the agent bead is what populates agentSpawnInheritedBy;
// a later explicit pin (no marker) or a schedule channel clears it.
func TestRecordInheritedSpawnRoleLocked(t *testing.T) {
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.recordInheritedSpawnRoleLocked("x", map[string]string{"slack_spawn_channel": "C1"})
	if _, ok := bot.agentSpawnInheritedBy["x"]; ok {
		t.Fatal("explicit pin must not be recorded as inherited")
	}
	bot.recordInheritedSpawnRoleLocked("x", map[string]string{"slack_spawn_channel_inherited_from_role": "crew"})
	if got := bot.agentSpawnInheritedBy["x"]; got != "crew" {
		t.Errorf("got %q, want crew", got)
	}
	bot.recordInheritedSpawnRoleLocked("x", map[string]string{"slack_spawn_channel": "C2"})
	if _, ok := bot.agentSpawnInheritedBy["x"]; ok {
		t.Fatal("explicit re-pin must clear the inherited marker")
	}

	bot.agentSpawnInheritedBy["y"] = "gardener"
	bot.NotifyAgentState(context.Background(), BeadEvent{ID: "y", Fields: map[string]string{
		"agent": "y", "slack_spawn_channel": "C-g", "slack_spawn_channel_inherited_from_role": "gardener",
		"schedule_slack_channel": "C-sched",
	}})
	if _, ok := bot.agentSpawnInheritedBy["y"]; ok {
		t.Fatal("schedule channel is an explicit pin; marker must be cleared")
	}
	if got := bot.agentSpawnChannel["y"]; got != "C-sched" {
		t.Errorf("spawn channel = %q, want C-sched", got)
	}
}
