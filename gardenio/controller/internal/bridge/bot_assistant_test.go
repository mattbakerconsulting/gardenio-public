package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// fakeSlackAssistantServer tracks API calls to assistant.* endpoints.
type fakeSlackAssistantServer struct {
	calls []slackAssistantCall
	srv   *httptest.Server
}

type slackAssistantCall struct {
	Method    string
	ChannelID string
	ThreadTS  string
	// Extra fields captured per method.
	Status       string
	Title        string
	Text         string   // for chat.postMessage
	PromptTitles []string // for assistant.threads.setSuggestedPrompts
}

func newFakeSlackAssistantServer(t *testing.T) *fakeSlackAssistantServer {
	t.Helper()
	f := &fakeSlackAssistantServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		path := strings.TrimPrefix(r.URL.Path, "/")

		call := slackAssistantCall{
			Method:    path,
			ChannelID: r.FormValue("channel") + r.FormValue("channel_id"),
			ThreadTS:  r.FormValue("thread_ts"),
		}

		switch path {
		case "assistant.threads.setStatus":
			call.Status = r.FormValue("status")
		case "assistant.threads.setTitle":
			call.Title = r.FormValue("title")
		case "assistant.threads.setSuggestedPrompts":
			var prompts []struct {
				Title   string `json:"title"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(r.FormValue("prompts")), &prompts); err == nil {
				for _, p := range prompts {
					call.PromptTitles = append(call.PromptTitles, p.Title)
				}
			}
		case "chat.postMessage":
			call.Text = r.FormValue("text")
		case "conversations.info":
			// Return a channel whose name mirrors the requested id so tests can
			// assert conversations.info enrichment ran. Written directly (not via
			// the default ok-only encoder) so slack-go populates channel.Name.
			f.calls = append(f.calls, call)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"channel": map[string]any{
					"id":   call.ChannelID,
					"name": "name-" + call.ChannelID,
				},
			})
			return
		}

		f.calls = append(f.calls, call)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	return f
}

func (f *fakeSlackAssistantServer) Close() { f.srv.Close() }

func (f *fakeSlackAssistantServer) callsByMethod(method string) []slackAssistantCall {
	var result []slackAssistantCall
	for _, c := range f.calls {
		if c.Method == method {
			result = append(result, c)
		}
	}
	return result
}

func TestHandleAppHomeOpened_MessagesTab_SetsPrompts(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	bot.botUserID = "U_BOT"

	before := counterValue(t, assistantEventThreadStarted)

	ev := &appHomeOpenedEvent{
		Type:    "app_home_opened",
		User:    "U_USER",
		Channel: "D_ASSISTANT",
		Tab:     "messages",
		Context: agentContext{Entities: []agentContextEntity{
			{Type: agentContextChannelIDType, Value: "C_VIEWING", TeamID: "T1"},
		}},
	}

	bot.handleAppHomeOpened(context.Background(), ev)

	// Records the "thread_started" smoke-check counter (agent_view successor).
	if after := counterValue(t, assistantEventThreadStarted); after != before+1 {
		t.Errorf("thread_started counter = %v, want %v", after, before+1)
	}

	// Sets suggested prompts, capped at Slack's max, context-aware prompt first.
	promptCalls := srv.callsByMethod("assistant.threads.setSuggestedPrompts")
	if len(promptCalls) != 1 {
		t.Fatalf("expected 1 setSuggestedPrompts call, got %d", len(promptCalls))
	}
	titles := promptCalls[0].PromptTitles
	if len(titles) > maxAssistantSuggestedPrompts {
		t.Errorf("setSuggestedPrompts sent %d prompts, exceeds Slack max %d: %v",
			len(titles), maxAssistantSuggestedPrompts, titles)
	}
	hasContextPrompt := false
	for _, tt := range titles {
		if tt == "Spawn agent for this channel" {
			hasContextPrompt = true
		}
	}
	if !hasContextPrompt {
		t.Errorf("context-aware prompt dropped by the cap; got %v", titles)
	}

	// The viewed-channel context is cached under the DM channel with an empty
	// thread_ts so a later DM "spawn" can resolve the project.
	if got := bot.getAssistantCtxChannel("D_ASSISTANT", ""); got != "C_VIEWING" {
		t.Errorf("expected cached context channel C_VIEWING, got %q", got)
	}

	// agent_view has no per-open thread → do NOT post a per-open greeting
	// (it would spam the DM) and do NOT set a thread status.
	if postCalls := srv.callsByMethod("chat.postMessage"); len(postCalls) != 0 {
		t.Errorf("expected no chat.postMessage (no per-open greeting under agent_view), got %d", len(postCalls))
	}
	if statusCalls := srv.callsByMethod("assistant.threads.setStatus"); len(statusCalls) != 0 {
		t.Errorf("expected no setStatus calls under agent_view app_home_opened, got %d", len(statusCalls))
	}
}

func TestHandleAppHomeOpened_NonMessagesTab_Ignored(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	bot.botUserID = "U_BOT"

	before := counterValue(t, assistantEventThreadStarted)

	// The Home tab open is not the agent Messages surface — ignore it entirely.
	ev := &appHomeOpenedEvent{
		Type:    "app_home_opened",
		User:    "U_USER",
		Channel: "D_ASSISTANT",
		Tab:     "home",
	}
	bot.handleAppHomeOpened(context.Background(), ev)

	if after := counterValue(t, assistantEventThreadStarted); after != before {
		t.Errorf("thread_started counter moved on non-messages tab: %v -> %v", before, after)
	}
	if promptCalls := srv.callsByMethod("assistant.threads.setSuggestedPrompts"); len(promptCalls) != 0 {
		t.Errorf("expected no setSuggestedPrompts on non-messages tab, got %d", len(promptCalls))
	}
}

func TestHandleAppHomeOpened_EmptyContext_PreservesPriorCache(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	bot.botUserID = "U_BOT"

	// Prime a context channel captured on an earlier context-bearing open.
	bot.setAssistantCtxChannel("D_ASSISTANT", "", "C_PRIOR")

	// A later open with NO viewed context must PRESERVE the prior channel, not
	// wipe it (ka-0000004985 review). Slack omits the context field entirely on
	// an app_home_opened that carries no entities — the common case (clicking
	// into the DM, reloading Slack). Treating that as "user cleared context"
	// and clearing the cache would break "spawn for this channel" resolution on
	// the very next generic spawn. Mirror the old assistant_view handlers'
	// purely-additive semantics: a context survives until a NEW one replaces it.
	ev := &appHomeOpenedEvent{
		Type:    "app_home_opened",
		User:    "U_USER",
		Channel: "D_ASSISTANT",
		Tab:     "messages",
		Context: agentContext{},
	}
	bot.handleAppHomeOpened(context.Background(), ev)

	if got := bot.getAssistantCtxChannel("D_ASSISTANT", ""); got != "C_PRIOR" {
		t.Errorf("expected prior context channel C_PRIOR preserved on no-context open, got %q", got)
	}

	// And a subsequent open that DOES carry a new context replaces it.
	ev2 := &appHomeOpenedEvent{
		Type:    "app_home_opened",
		User:    "U_USER",
		Channel: "D_ASSISTANT",
		Tab:     "messages",
		Context: agentContext{Entities: []agentContextEntity{
			{Type: agentContextChannelIDType, Value: "C_NEW", TeamID: "T1"},
		}},
	}
	bot.handleAppHomeOpened(context.Background(), ev2)

	if got := bot.getAssistantCtxChannel("D_ASSISTANT", ""); got != "C_NEW" {
		t.Errorf("expected context channel replaced with C_NEW, got %q", got)
	}
}

func TestHandleAppContextChanged_RecordsMetric(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	bot.botUserID = "U_BOT"

	before := counterValue(t, assistantEventContextChanged)

	ev := &appContextChangedEvent{
		Type: "app_context_changed",
		Context: agentContext{Entities: []agentContextEntity{
			{Type: agentContextChannelIDType, Value: "C_NEW_CHANNEL", TeamID: "T1"},
		}},
	}
	bot.handleAppContextChanged(context.Background(), ev)

	// app_context_changed carries no DM channel/user, so it cannot target a
	// specific DM's prompts — it only records the smoke-check counter.
	if after := counterValue(t, assistantEventContextChanged); after != before+1 {
		t.Errorf("context_changed counter = %v, want %v", after, before+1)
	}
	if promptCalls := srv.callsByMethod("assistant.threads.setSuggestedPrompts"); len(promptCalls) != 0 {
		t.Errorf("expected no setSuggestedPrompts on app_context_changed (no DM to target), got %d", len(promptCalls))
	}
}

func TestAgentContextChannelID(t *testing.T) {
	// Returns the first channel_id entity value; skips non-channel entities.
	c := agentContext{Entities: []agentContextEntity{
		{Type: "slack#/types/unknown", Value: "X"},
		{Type: agentContextChannelIDType, Value: "C_FIRST"},
		{Type: agentContextChannelIDType, Value: "C_SECOND"},
	}}
	if got := c.channelID(); got != "C_FIRST" {
		t.Errorf("channelID() = %q, want C_FIRST", got)
	}
	// Empty context (no entities) → "".
	if got := (agentContext{}).channelID(); got != "" {
		t.Errorf("empty channelID() = %q, want empty", got)
	}
	// A channel entity with an empty value is skipped.
	c2 := agentContext{Entities: []agentContextEntity{
		{Type: agentContextChannelIDType, Value: ""},
		{Type: agentContextChannelIDType, Value: "C_REAL"},
	}}
	if got := c2.channelID(); got != "C_REAL" {
		t.Errorf("channelID() with empty-first = %q, want C_REAL", got)
	}
}

func TestCapSuggestedPrompts(t *testing.T) {
	mk := func(n int) []slack.AssistantThreadsPrompt {
		out := make([]slack.AssistantThreadsPrompt, n)
		for i := range out {
			out[i] = slack.AssistantThreadsPrompt{Title: string(rune('A' + i))}
		}
		return out
	}
	for _, n := range []int{0, 1, 4} {
		if got := capSuggestedPrompts(mk(n)); len(got) != n {
			t.Errorf("capSuggestedPrompts(%d) len = %d, want %d", n, len(got), n)
		}
	}
	// 5 and 6 must be trimmed to exactly 4, keeping the first (highest-priority) entries.
	for _, n := range []int{5, 6} {
		got := capSuggestedPrompts(mk(n))
		if len(got) != maxAssistantSuggestedPrompts {
			t.Errorf("capSuggestedPrompts(%d) len = %d, want %d", n, len(got), maxAssistantSuggestedPrompts)
		}
		if got[0].Title != "A" {
			t.Errorf("capSuggestedPrompts(%d) dropped the first prompt; got[0]=%q", n, got[0].Title)
		}
	}
}

func TestHandleAssistantMessage_ClearsStatusOnNoProject(t *testing.T) {
	// When no project is mapped and the thread spawn fails, the assistant
	// loading status must end CLEARED to avoid "Slack couldn't send this
	// message" errors in the AI container. After the composer-unlock fix
	// (ka-0000000115) the "Thinking..." loading status is NOT set by default
	// (only when HoldAssistantStatus is true), so the invariant this test pins
	// is the one that actually matters: whatever status calls occur, the LAST
	// one is an empty clear and no lingering non-empty status is left behind.
	daemon := newMockDaemon()
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	bot.botUserID = "U_BOT"

	ev := &slackevents.MessageEvent{
		User:            "U_USER",
		Text:            "what agents are running?",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "1111.2222",
		Channel:         "D_ASSISTANT",
		AssistantThread: &slackevents.AssistantThreadActionToken{ActionToken: "tok"},
	}

	bot.handleAssistantMessage(context.Background(), ev)

	// Verify: at least one setStatus call, and the LAST is an empty clear. No
	// "Thinking..." should be set by default (it would linger — that is the bug
	// this fix removes).
	statusCalls := srv.callsByMethod("assistant.threads.setStatus")
	if len(statusCalls) < 1 {
		t.Fatalf("expected at least 1 setStatus call (the clear), got %d", len(statusCalls))
	}
	if statusCalls[len(statusCalls)-1].Status != "" {
		t.Errorf("last setStatus should be empty (clear), got %q", statusCalls[len(statusCalls)-1].Status)
	}
	for _, c := range statusCalls {
		if c.Status == "Thinking..." {
			t.Errorf("did not expect a lingering 'Thinking...' status by default (composer-unlock); got %+v", statusCalls)
		}
	}

	// Verify: an error message was posted (no project mapped).
	postCalls := srv.callsByMethod("chat.postMessage")
	if len(postCalls) < 1 {
		t.Fatalf("expected at least 1 chat.postMessage call, got %d", len(postCalls))
	}
	found := false
	for _, c := range postCalls {
		if strings.Contains(c.Text, "Cannot spawn agent") || strings.Contains(c.Text, "no project") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected error message about missing project, but none found")
	}
}

func TestHandleAssistantMessage_ClearsStatusOnNoProject_HoldFlag(t *testing.T) {
	// With HoldAssistantStatus opted back in, the legacy behavior holds: a
	// "Thinking..." loading status is set, then cleared on the no-project
	// early-return path.
	daemon := newMockDaemon()
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	bot.botUserID = "U_BOT"
	bot.notifConfig.HoldAssistantStatus = true

	ev := &slackevents.MessageEvent{
		User:            "U_USER",
		Text:            "what agents are running?",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "1111.2222",
		Channel:         "D_ASSISTANT",
		AssistantThread: &slackevents.AssistantThreadActionToken{ActionToken: "tok"},
	}

	bot.handleAssistantMessage(context.Background(), ev)

	statusCalls := srv.callsByMethod("assistant.threads.setStatus")
	if len(statusCalls) < 2 {
		t.Fatalf("expected at least 2 setStatus calls (loading + clear) with HoldAssistantStatus=true, got %d", len(statusCalls))
	}
	if statusCalls[0].Status != "Thinking..." {
		t.Errorf("first setStatus should be 'Thinking...', got %q", statusCalls[0].Status)
	}
	if statusCalls[len(statusCalls)-1].Status != "" {
		t.Errorf("last setStatus should be empty (clear), got %q", statusCalls[len(statusCalls)-1].Status)
	}
}

func TestHandleAssistantMessage_ResolvesProjectFromContextChannel(t *testing.T) {
	// When a user opens the assistant pane while viewing a channel mapped to a
	// project, the cached context channel should be used for project resolution
	// even though multiple projects exist (so auto-select doesn't fire).
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("monorepo", "C_MONOREPO_CHAN")
	daemon.seedProjectWithChannel("gardenio", "C_GARDENIO_CHAN")
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	bot.botUserID = "U_BOT"
	bot.state, _ = NewStateManager("")

	// Simulate assistant_thread_started caching the context channel.
	bot.setAssistantCtxChannel("D_ASSISTANT", "1111.2222", "C_MONOREPO_CHAN")

	ev := &slackevents.MessageEvent{
		User:            "U_USER",
		Text:            "spawn an agent",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "1111.2222",
		Channel:         "D_ASSISTANT",
		AssistantThread: &slackevents.AssistantThreadActionToken{ActionToken: "tok"},
	}

	bot.handleAssistantMessage(context.Background(), ev)

	// Verify: an agent bead was created with project=monorepo.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead (resolved from context channel), got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "monorepo" {
			t.Errorf("expected project=monorepo, got %s", b.Fields["project"])
		}
	}
}

func TestHandleAssistantMessage_AutoSelectsSoleProject(t *testing.T) {
	// When an AI container thread has no channel→project mapping but there's
	// exactly one project, the bridge should auto-select it and spawn an agent.
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	bot.botUserID = "U_BOT"
	bot.state, _ = NewStateManager("")

	ev := &slackevents.MessageEvent{
		User:            "U_USER",
		Text:            "spawn an agent",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "1111.2222",
		Channel:         "D_ASSISTANT",
		AssistantThread: &slackevents.AssistantThreadActionToken{ActionToken: "tok"},
	}

	bot.handleAssistantMessage(context.Background(), ev)

	// Verify: status was cleared.
	statusCalls := srv.callsByMethod("assistant.threads.setStatus")
	if len(statusCalls) < 2 {
		t.Fatalf("expected at least 2 setStatus calls, got %d", len(statusCalls))
	}
	if statusCalls[len(statusCalls)-1].Status != "" {
		t.Errorf("last setStatus should be empty (clear), got %q", statusCalls[len(statusCalls)-1].Status)
	}

	// Verify: an agent bead was created (cold-start path).
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created (auto-selected gardenio project), got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio, got %s", b.Fields["project"])
		}
	}
}

func TestHandleEventsAPI_AssistantThread_SkipsDuplicateAppMention(t *testing.T) {
	// When a user @mentions the bot in an assistant thread, Slack sends both
	// a MessageEvent and an AppMentionEvent. The MessageEvent is handled by
	// handleAssistantMessage which synthesizes its own AppMentionEvent.
	// The real AppMentionEvent must be skipped to avoid double processing
	// (duplicate tracking beads + duplicate agent nudges).
	daemon := newMockDaemon()
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "crew-gardenio-crew-hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	bot.botUserID = "U_BOT"
	bot.state, _ = NewStateManager("")

	// Set up an agent card bound to the assistant thread.
	bot.mu.Lock()
	bot.agentCards["hq"] = MessageRef{
		ChannelID: "D_ASSISTANT",
		Timestamp: "1111.2222",
		ThreadTS:  "1111.2222",
		Agent:     "hq",
	}
	bot.mu.Unlock()

	assistantThread := &slackevents.AssistantThreadActionToken{ActionToken: "tok"}

	// Simulate both events as Slack would send them:
	// 1. MessageEvent (handled first by handleAssistantMessage → handleAppMention)
	bot.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				User:            "U_USER",
				Text:            "<@U_BOT> check the logs",
				TimeStamp:       "2222.3333",
				ThreadTimeStamp: "1111.2222",
				Channel:         "D_ASSISTANT",
				AssistantThread: assistantThread,
			},
		},
	})

	// 2. AppMentionEvent (should be SKIPPED — already handled above)
	bot.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.AppMentionEvent{
				User:            "U_USER",
				Text:            "<@U_BOT> check the logs",
				TimeStamp:       "2222.3333",
				ThreadTimeStamp: "1111.2222",
				Channel:         "D_ASSISTANT",
				AssistantThread: assistantThread,
			},
		},
	})

	// Verify: exactly ONE mention tracking bead was created, not two. Filter
	// by slack-mention label to skip the canonical thread bead created as a
	// side effect of the M2d triggered-by dep write.
	var mentionBeads []*beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "message" && hasLabel(b.Labels, "slack-mention") {
			mentionBeads = append(mentionBeads, b)
		}
	}
	if len(mentionBeads) != 1 {
		t.Errorf("expected exactly 1 mention tracking bead, got %d (double nudge bug)", len(mentionBeads))
	}
}

func TestHandleEventsAPI_NonAssistantThread_ProcessesAppMention(t *testing.T) {
	// AppMentionEvent in a non-assistant thread should be processed normally.
	daemon := newMockDaemon()
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "crew-gardenio-crew-hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U_BOT"
	bot.state, _ = NewStateManager("")

	bot.mu.Lock()
	bot.agentCards["hq"] = MessageRef{
		ChannelID: "C_CHANNEL",
		Timestamp: "1111.2222",
		Agent:     "hq",
	}
	bot.mu.Unlock()

	// Regular channel AppMentionEvent (no AssistantThread) — should be processed.
	bot.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.AppMentionEvent{
				User:            "U_USER",
				Text:            "<@U_BOT> check the logs",
				TimeStamp:       "2222.3333",
				ThreadTimeStamp: "1111.2222",
				Channel:         "C_CHANNEL",
			},
		},
	})

	// Verify: one mention tracking bead was created. Filter by slack-mention
	// label to skip the canonical thread bead created as a side effect of the
	// M2d triggered-by dep write.
	var mentionBeads []*beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "message" && hasLabel(b.Labels, "slack-mention") {
			mentionBeads = append(mentionBeads, b)
		}
	}
	if len(mentionBeads) != 1 {
		t.Errorf("expected 1 mention tracking bead for non-assistant mention, got %d", len(mentionBeads))
	}
}

func TestHandleEventsAPI_TopLevelMention_WithAssistantThread_NotSkipped(t *testing.T) {
	// Regression test: when the app has Agents & AI Apps enabled, Slack
	// populates AssistantThread on AppMentionEvent even for top-level
	// channel mentions (ThreadTimeStamp is empty). The dedup filter must
	// NOT skip these — they have no MessageEvent counterpart that would
	// call handleAssistantMessage. Without the ThreadTimeStamp check,
	// these mentions are silently dropped.
	daemon := newMockDaemon()
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "crew-gardenio-crew-hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U_BOT"
	bot.state, _ = NewStateManager("")

	// Map the channel to an agent via router so the top-level mention
	// routes to the agent (Priority 3) instead of triggering auto-spawn.
	bot.router = NewRouter(RouterConfig{
		Overrides: map[string]string{
			"hq": "C_ACMEDEVOPS",
		},
	})

	assistantThread := &slackevents.AssistantThreadActionToken{ActionToken: "tok"}

	// Top-level AppMentionEvent: AssistantThread is set but ThreadTimeStamp
	// is empty (no thread). This MUST be processed, not skipped.
	bot.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.AppMentionEvent{
				User:            "U_USER",
				Text:            "<@U_BOT> check the deploy status",
				TimeStamp:       "3333.4444",
				ThreadTimeStamp: "", // top-level — NOT in a thread
				Channel:         "C_ACMEDEVOPS",
				AssistantThread: assistantThread,
			},
		},
	})

	// Verify: a tracking bead was created (mention was processed, not skipped).
	taskBeads := filterBeadsByType(daemon.beads, "task")
	if len(taskBeads) != 1 {
		t.Errorf("expected 1 mention tracking bead for top-level mention with AssistantThread, got %d (mention was silently dropped)", len(taskBeads))
	}
}

func TestIsAssistantThread(t *testing.T) {
	// With AssistantThread set.
	ev := &slackevents.MessageEvent{
		AssistantThread: &slackevents.AssistantThreadActionToken{ActionToken: "abc"},
	}
	if !isAssistantThread(ev) {
		t.Error("expected isAssistantThread=true when AssistantThread is set")
	}

	// Without AssistantThread.
	ev2 := &slackevents.MessageEvent{}
	if isAssistantThread(ev2) {
		t.Error("expected isAssistantThread=false when AssistantThread is nil")
	}
}

// TestAgentViewEventsParseViaRegistration is the linchpin guard for the
// agent_view migration (ka-0000004985): it proves the init() in
// agent_view_events.go actually makes slack-go's ParseEvent deliver both new
// events through the normal CallbackEvent path.
//
//   - app_context_changed has NO typed struct/const in slack-go — without our
//     EventsAPIInnerEventMapping registration, parseInnerEvent returns an "inner
//     Event does not exist" error and socketmode drops the event as a bad
//     message (it never reaches handleEventsAPI).
//   - slack-go's native AppHomeOpenedEvent lacks the `context` field Slack embeds
//     once app_context_changed is subscribed; our superset override preserves it.
//
// If this fails, the whole surface silently stops working (the exact class of
// silent breakage the smoke-check exists for), so pin the parse contract here.
func TestAgentViewEventsParseViaRegistration(t *testing.T) {
	t.Run("app_context_changed parses via registration", func(t *testing.T) {
		raw := []byte(`{"token":"x","team_id":"T1","api_app_id":"A1","type":"event_callback",` +
			`"event":{"type":"app_context_changed","context":{"entities":[` +
			`{"type":"slack#/types/channel_id","value":"C123","team_id":"T1"}]}}}`)
		ev, err := slackevents.ParseEvent(raw, slackevents.OptionNoVerifyToken())
		if err != nil {
			t.Fatalf("ParseEvent(app_context_changed) errored — registration missing? %v", err)
		}
		acc, ok := ev.InnerEvent.Data.(*appContextChangedEvent)
		if !ok {
			t.Fatalf("inner event type = %T, want *appContextChangedEvent", ev.InnerEvent.Data)
		}
		if got := acc.Context.channelID(); got != "C123" {
			t.Fatalf("context channelID = %q, want C123", got)
		}
	})

	t.Run("app_home_opened superset keeps embedded context", func(t *testing.T) {
		raw := []byte(`{"token":"x","team_id":"T1","api_app_id":"A1","type":"event_callback",` +
			`"event":{"type":"app_home_opened","user":"U1","channel":"D1","tab":"messages",` +
			`"event_ts":"1.2","context":{"entities":[` +
			`{"type":"slack#/types/channel_id","value":"C777","team_id":"T1"}]}}}`)
		ev, err := slackevents.ParseEvent(raw, slackevents.OptionNoVerifyToken())
		if err != nil {
			t.Fatalf("ParseEvent(app_home_opened): %v", err)
		}
		aho, ok := ev.InnerEvent.Data.(*appHomeOpenedEvent)
		if !ok {
			t.Fatalf("inner event type = %T, want *appHomeOpenedEvent (superset override)", ev.InnerEvent.Data)
		}
		if aho.Tab != "messages" || aho.Channel != "D1" || aho.User != "U1" {
			t.Fatalf("native app_home_opened fields lost: %+v", aho)
		}
		if got := aho.Context.channelID(); got != "C777" {
			t.Fatalf("embedded context channelID = %q, want C777 (superset dropped it?)", got)
		}
	})
}

// --- Composer-unlock: assistant status footer is not held during runs ---
// (ka-0000000115) The assistant pane held the setStatus footer populated for
// the entire run, which is the only axis on which it diverged from the main
// threaded interface; a continuously-held status on the DM-style surface is
// community-reported to lock the composer and block mid-inference follow-ups.
// SetThreadStatus therefore suppresses NON-EMPTY status writes by default and
// only lets an EMPTY status (a clear) through, unless HoldAssistantStatus is set.

func TestSetThreadStatus_SuppressesNonEmptyByDefault(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	// Default config: HoldAssistantStatus is false (zero value).

	bot.SetThreadStatus(context.Background(), "D_ASSISTANT", "1111.2222",
		"running commands... (44 tools) [9m57s]")

	statusCalls := srv.callsByMethod("assistant.threads.setStatus")
	if len(statusCalls) != 0 {
		t.Errorf("expected non-empty status to be suppressed by default (composer-unlock), "+
			"got %d setStatus calls: %+v", len(statusCalls), statusCalls)
	}
}

func TestSetThreadStatus_EmptyClearAlwaysPassesThrough(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	// Default config: HoldAssistantStatus is false.

	// An empty status is a CLEAR — it must always reach Slack so terminal paths
	// (StopSession) can remove a stale indicator.
	bot.SetThreadStatus(context.Background(), "D_ASSISTANT", "1111.2222", "")

	statusCalls := srv.callsByMethod("assistant.threads.setStatus")
	if len(statusCalls) != 1 {
		t.Fatalf("expected 1 setStatus call (the clear), got %d", len(statusCalls))
	}
	if statusCalls[0].Status != "" {
		t.Errorf("expected empty clear status, got %q", statusCalls[0].Status)
	}
}

func TestSetThreadStatus_HoldFlagRestoresLegacyBehavior(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	// Opt back in to the legacy always-on footer status.
	bot.notifConfig.HoldAssistantStatus = true

	bot.SetThreadStatus(context.Background(), "D_ASSISTANT", "1111.2222", "Working...")

	statusCalls := srv.callsByMethod("assistant.threads.setStatus")
	if len(statusCalls) != 1 {
		t.Fatalf("expected 1 setStatus call when HoldAssistantStatus=true, got %d", len(statusCalls))
	}
	if statusCalls[0].Status != "Working..." {
		t.Errorf("expected 'Working...' status, got %q", statusCalls[0].Status)
	}
}

func TestHandleAssistantMessage_DoesNotSetThinkingStatusByDefault(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	bot.botUserID = "U_BOT"
	// Default config: HoldAssistantStatus false.

	ev := &slackevents.MessageEvent{
		User:            "U_USER",
		Text:            "hello",
		Channel:         "D_ASSISTANT",
		ThreadTimeStamp: "1111.2222",
		AssistantThread: &slackevents.AssistantThreadActionToken{ActionToken: "tok"},
	}
	bot.handleAssistantMessage(context.Background(), ev)

	// The one-shot "Thinking..." status must NOT be set by default. Any
	// setStatus calls that DO occur must be empty (clears from the deferred
	// clearAssistantStatus / terminal paths) — never a lingering "Thinking...".
	for _, c := range srv.callsByMethod("assistant.threads.setStatus") {
		if c.Status == "Thinking..." {
			t.Errorf("handleAssistantMessage set a lingering 'Thinking...' status " +
				"by default; expected it suppressed (composer-unlock)")
		}
	}
}
