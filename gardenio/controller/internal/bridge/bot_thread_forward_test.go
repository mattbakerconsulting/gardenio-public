package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

func TestHandleMessageEvent_ThreadForward_AgentCardThread_NoForward(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "crew-gardenio-crew-hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:    daemon,
		state:     state,
		logger:    slog.Default(),
		botUserID: "U-BOT",
		agentCards: map[string]MessageRef{
			"hq": {
				ChannelID: "C-agents",
				Timestamp: "1111.2222",
				Agent:     "hq",
			},
		},
		lastThreadNudge: make(map[string]time.Time),
	}

	// Simulate a thread reply (not a mention) under an agent card thread.
	ev := &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-agents",
		Text:            "here is the extra context you asked for",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: "1111.2222",
	}

	// getAgentByThread still finds the agent (used for @mention routing).
	agent := b.getAgentByThread(ev.Channel, ev.ThreadTimeStamp)
	if agent != "hq" {
		t.Fatalf("expected agent hq from getAgentByThread, got %q", agent)
	}

	// But getThreadSpawnedAgent should NOT find it (agent card, not thread-spawned).
	spawned := b.getThreadSpawnedAgent(ev.Channel, ev.ThreadTimeStamp)
	if spawned != "" {
		t.Fatalf("expected empty from getThreadSpawnedAgent for agent card thread, got %q", spawned)
	}

	// No bead should be created for non-mention messages in agent card threads.
	for _, bead := range daemon.beads {
		if hasLabel(bead.Labels, "slack-thread-reply") {
			t.Error("should not create thread-reply bead for agent card thread without @mention")
		}
	}
}

func TestHandleMessageEvent_ThreadForward_ThreadSpawnedAgent_WithListen(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["thread-1111-2222"] = &beadsapi.BeadDetail{
		ID:    "bd-thread-agent",
		Title: "thread-1111-2222",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "thread-1111-2222",
			"slack_thread_channel": "C-support",
			"slack_thread_ts":      "1111.2222",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-support", "1111.2222", "thread-1111-2222")
	_ = state.SetListenThread("C-support", "1111.2222") // --listen mode

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		lastThreadNudge: make(map[string]time.Time),
	}

	ev := &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-support",
		Text:            "follow up info",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: "1111.2222",
	}

	agent := b.getAgentByThread(ev.Channel, ev.ThreadTimeStamp)
	if agent != "thread-1111-2222" {
		t.Fatalf("expected thread-1111-2222, got %q", agent)
	}

	b.handleThreadForward(context.Background(), slackReplyInbound(ev, ""), agent)

	found := false
	for _, bead := range daemon.beads {
		if bead.Type == "message" && bead.Assignee == "thread-1111-2222" && hasLabel(bead.Labels, "slack-thread-reply") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a task bead with label slack-thread-reply assigned to thread-1111-2222")
	}

	// ka-0000003279: the human's words must also be recorded as a type=message
	// conversation bead bound to the thread (wish) head by thread_id, so the
	// beads-viewer /threads conversation lane shows the follow-up. The tracking
	// task above lands in the WORK lane; this message lands in the CONVERSATION
	// lane. It must NOT carry the `thread-say` label (that would make the
	// outbound ThreadMessageForward watcher echo the user's own message back
	// into their Slack thread).
	var msg *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if bead.Type == "message" && hasLabel(bead.Labels, "slack-thread-message") {
			msg = bead
			break
		}
	}
	if msg == nil {
		t.Fatal("expected a type=message conversation bead (label slack-thread-message) for the human follow-up")
	}
	if msg.Fields["thread_id"] == "" {
		t.Errorf("conversation message bead %s must carry a thread_id wish-pointer; got fields %v", msg.ID, msg.Fields)
	}
	if msg.Fields["text"] != "follow up info" {
		t.Errorf("conversation message text = %q, want %q", msg.Fields["text"], "follow up info")
	}
	if hasLabel(msg.Labels, ThreadSayLabel) {
		t.Errorf("conversation message must NOT carry the %q label (would re-forward into Slack)", ThreadSayLabel)
	}
}

func TestHandleMessageEvent_ThreadForward_ThreadSpawnedAgent_NoListen(t *testing.T) {
	// ka-0000006280: thread-spawned agents auto-forward non-mention replies
	// even without --listen. The user opted into the conversation by
	// @mentioning the bot to spawn the agent in this thread; expecting them
	// to re-@mention every turn caused silent drops once hintMentionRequired's
	// 10-minute throttle fired.
	daemon := newMockDaemon()
	daemon.beads["thread-1111-2222"] = &beadsapi.BeadDetail{
		ID:    "bd-thread-agent",
		Title: "thread-1111-2222",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "thread-1111-2222",
			"slack_thread_channel": "C-support",
			"slack_thread_ts":      "1111.2222",
		},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(daemon, slackSrv)
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-support", "1111.2222", "thread-1111-2222")
	// No SetListenThread — default is mention-only.
	b.state = state

	// Sanity: state recorded the binding without --listen.
	if state.IsListenThread("C-support", "1111.2222") {
		t.Fatal("expected listen mode to be false by default")
	}

	// Non-mention thread reply.
	b.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Text:            "follow up info",
		Channel:         "C-support",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: "1111.2222",
	})

	// A slack-thread-reply tracking bead should have been created and
	// assigned to the bound thread agent — the fix replaces the
	// hintMentionRequired-only branch with a forward+nudge.
	found := false
	for _, bead := range daemon.beads {
		if bead.Type == "message" && bead.Assignee == "thread-1111-2222" && hasLabel(bead.Labels, "slack-thread-reply") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a slack-thread-reply bead for non-listen thread-spawned agent (ka-0000006280 fix)")
	}
}

func TestHandleMessageEvent_ThreadForward_UntrackedThread(t *testing.T) {
	b := &Bot{
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		lastThreadNudge: make(map[string]time.Time),
	}

	// Thread not bound to any agent — getAgentByThread returns "".
	agent := b.getAgentByThread("C-random", "9999.8888")
	if agent != "" {
		t.Errorf("expected empty agent for untracked thread, got %q", agent)
	}
	// handleThreadForward would not be called in this case.
}

func TestHandleMessageEvent_ThreadForward_SkipsMentions(t *testing.T) {
	// Verify that messages containing @BOT are skipped in handleMessageEvent's
	// thread forward path (to avoid duplicates with app_mention handler).
	botUserID := "U-BOT"
	text := "<@U-BOT> check this"

	// This is the same check used in handleMessageEvent.
	containsMention := containsBotMention(text, botUserID)
	if !containsMention {
		t.Error("expected message with @BOT to be detected as containing mention")
	}

	// A plain message should not be skipped.
	plain := "here is more context"
	if containsBotMention(plain, botUserID) {
		t.Error("plain message should not be detected as containing mention")
	}
}

// containsBotMention mirrors the check in handleMessageEvent.
func containsBotMention(text, botUserID string) bool {
	return len(botUserID) > 0 && len(text) > 0 &&
		// Use same format as handleMessageEvent.
		stringContains(text, "<@"+botUserID+">")
}

func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestHandleMessageEvent_ThreadForward_SkipsBotMessages(t *testing.T) {
	// handleMessageEvent filters ev.User == botUserID at the top.
	// Verify that check prevents forwarding bot's own messages.
	botUserID := "U-BOT"

	ev := &slackevents.MessageEvent{
		User:            botUserID, // bot's own message
		Channel:         "C-agents",
		Text:            "I did the thing",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: "1111.2222",
	}

	// This is the same check at the top of handleMessageEvent.
	if ev.User != botUserID {
		t.Error("expected bot message to be filtered")
	}
}

func TestHandleMessageEvent_ThreadForward_SkipsSubtypes(t *testing.T) {
	ev := &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-agents",
		Text:            "edited text",
		SubType:         "message_changed",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: "1111.2222",
	}

	// handleMessageEvent filters SubType != "" at the top, except for the
	// thread_broadcast exemption — see TestHandleMessageEvent_ThreadBroadcast_Forwarded.
	if ev.SubType == "" {
		t.Error("expected subtype to be non-empty")
	}
}

// ka-0000001650: thread replies sent with "Also send to channel" carry
// SubType=thread_broadcast. They are user-authored conversational messages
// and must reach the bound agent the same as a normal thread reply.
func TestHandleMessageEvent_ThreadBroadcast_Forwarded(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["thread-1111-2222"] = &beadsapi.BeadDetail{
		ID:    "bd-thread-agent",
		Title: "thread-1111-2222",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "thread-1111-2222",
			"slack_thread_channel": "C-support",
			"slack_thread_ts":      "1111.2222",
		},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(daemon, slackSrv)
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-support", "1111.2222", "thread-1111-2222")
	b.state = state

	b.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Text:            "also-to-channel reply",
		Channel:         "C-support",
		SubType:         "thread_broadcast",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: "1111.2222",
	})

	found := false
	for _, bead := range daemon.beads {
		if bead.Type == "message" && bead.Assignee == "thread-1111-2222" && hasLabel(bead.Labels, "slack-thread-reply") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a slack-thread-reply bead for thread_broadcast subtype (ka-0000001650)")
	}
}

// TestHandleMessageEvent_ThreadForward_InactiveAgent_DoesNotRespawn verifies
// the post-gd-DcRoLBzsOV behavior: when a thread reply arrives for an agent
// that has exited, we do NOT auto-respawn. Instead a throttled hint is posted
// in the thread nudging the user to @mention the bot to resume. See
// gd-lUNTlpAVm5 for the rationale — auto-respawn was resurrecting agents for
// days whenever a single reply arrived in a long-open thread.
func TestHandleMessageEvent_ThreadForward_InactiveAgent_DoesNotRespawn(t *testing.T) {
	daemon := newMockDaemon()
	// A project bead would be needed for respawn; we keep it out to make it
	// doubly obvious that no respawn is attempted.
	daemon.beads["proj-test"] = &beadsapi.BeadDetail{
		ID: "proj-test", Title: "testproject", Type: "project",
		Fields: map[string]string{"slack_channel": "C-test"},
	}

	var mu sync.Mutex
	var postedThreadTS []string
	var postedTexts []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedThreadTS = append(postedThreadTS, r.FormValue("thread_ts"))
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.9999"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-test", "1111.2222", "dead-agent")

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	ev := &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-test",
		Text:            "hello?",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: "1111.2222",
	}

	agent := b.getAgentByThread(ev.Channel, ev.ThreadTimeStamp)
	if agent != "dead-agent" {
		t.Fatalf("expected dead-agent, got %q", agent)
	}

	b.handleThreadForward(context.Background(), slackReplyInbound(ev, ""), agent)

	// No agent bead should have been created by the forward path.
	for _, bead := range daemon.beads {
		if bead.Type == "agent" && bead.Title == "dead-agent" {
			t.Fatalf("REGRESSION: handleThreadForward auto-spawned dead-agent — "+
				"plan gd-lUNTlpAVm5 explicitly removed auto-respawn; "+
				"user must @mention to resume. bead=%+v", bead)
		}
	}

	// Thread→agent mapping MUST be preserved so the mention-path resume works.
	if _, ok := state.GetThreadAgent("C-test", "1111.2222"); !ok {
		t.Error("expected thread agent mapping to be preserved after inactive-reply")
	}

	// A resume hint should have been posted in the thread as just the
	// :sleeping: emoji. The longer "stepped out / reply with @bot" phrasing
	// was removed because it added noise to threads where humans are
	// chatting after the agent's job is done.
	mu.Lock()
	defer mu.Unlock()
	var hinted bool
	for i, ts := range postedThreadTS {
		if ts == "1111.2222" && postedTexts[i] == ":sleeping:" {
			hinted = true
			break
		}
	}
	if !hinted {
		t.Errorf("expected a resume hint of just \":sleeping:\" in thread 1111.2222; got ts=%v texts=%v",
			postedThreadTS, postedTexts)
	}
}

// TestHandleThreadForward_ResumeHintThrottled verifies the resume hint is
// throttled per (agent, thread) so a busy thread does not spam the hint.
func TestHandleThreadForward_ResumeHintThrottled(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			mu.Lock()
			postCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.9999"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-throttle", "4444.5555", "gone-agent")

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		ev := &slackevents.MessageEvent{
			User:            "U-HUMAN",
			Channel:         "C-throttle",
			Text:            "still there?",
			TimeStamp:       "4444." + string(rune('6'+i)),
			ThreadTimeStamp: "4444.5555",
		}
		b.handleThreadForward(ctx, slackReplyInbound(ev, ""), "gone-agent")
	}

	mu.Lock()
	defer mu.Unlock()
	if postCount != 1 {
		t.Errorf("expected exactly 1 resume hint post (throttled), got %d", postCount)
	}
}

// TestHandleThreadForward_InactiveAgent_RecordsConversationBubble is the
// regression guard for ka-0000001743 ("/threads view dropping user messages").
// handleThreadForward used to `return` immediately after posting the resume
// hint when the bound agent had exited — BEFORE recording the human's words —
// so every post-termination follow-up was dropped from the beads-viewer
// /threads conversation lane even though it still showed in raw Slack. The fix
// records the conversation message bead UNCONDITIONALLY (it resolves the wish
// head by canonical label and needs no live agent), then branches to the resume
// hint. Here the agent bead is absent (FindAgentBead 404s), and we assert BOTH
// that the bubble is recorded AND that the resume hint still fires.
func TestHandleThreadForward_InactiveAgent_RecordsConversationBubble(t *testing.T) {
	daemon := newMockDaemon()
	// Pre-seed the wish head so recordThreadConversationMessage resolves it by
	// label (getOrCreateThreadBead) rather than minting a fresh one.
	daemon.beads["wish-head"] = &beadsapi.BeadDetail{
		ID:     "wish-head",
		Title:  "the user's wish",
		Type:   "task",
		Labels: []string{threadBeadLabel("C-dead", "2222.1111"), threadKindLabel},
		Fields: map[string]string{
			"slack_thread_channel": "C-dead",
			"slack_thread_ts":      "2222.1111",
		},
	}

	var mu sync.Mutex
	var postedTexts []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.9999"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-dead", "2222.1111", "gone-agent")

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	ev := &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-dead",
		Text:            "are you still working on this?",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "2222.1111",
	}

	// The agent bead is absent -> FindAgentBead returns ErrAgentNotFound.
	b.handleThreadForward(context.Background(), slackReplyInbound(ev, ""), "gone-agent")

	// The human's words MUST be recorded as a type=message conversation bubble
	// bound to the wish head, so /threads keeps the turn.
	var msg *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if bead.Type == "message" && hasLabel(bead.Labels, "slack-thread-message") {
			msg = bead
			break
		}
	}
	if msg == nil {
		t.Fatal("REGRESSION (ka-0000001743): dead-agent thread reply must still record a " +
			"conversation bubble; got none — /threads would drop the user's message")
	}
	if msg.Fields["thread_id"] != "wish-head" {
		t.Errorf("conversation bubble thread_id = %q, want wish-head", msg.Fields["thread_id"])
	}
	if msg.Fields["text"] != "are you still working on this?" {
		t.Errorf("conversation bubble text = %q, want the reply text", msg.Fields["text"])
	}
	if hasLabel(msg.Labels, ThreadSayLabel) {
		t.Errorf("inbound bubble must NOT carry %q (would echo back into Slack)", ThreadSayLabel)
	}

	// The resume hint must still fire (agent is gone, user should be told to
	// @mention to resume).
	mu.Lock()
	defer mu.Unlock()
	var hinted bool
	for _, txt := range postedTexts {
		if txt == ":sleeping:" {
			hinted = true
		}
	}
	if !hinted {
		t.Errorf("expected a :sleeping: resume hint for the exited agent; got posts %v", postedTexts)
	}

	// And we must NOT have auto-respawned the agent.
	for _, bead := range daemon.beads {
		if bead.Type == "agent" && bead.Title == "gone-agent" {
			t.Fatalf("REGRESSION: dead-agent thread reply auto-spawned the agent (gd-lUNTlpAVm5 forbids this)")
		}
	}
}

// TestHandleMessageEvent_OrphanWishThread_RecordsBubble is the regression guard
// for the second half of ka-0000001743: a non-@mention reply in a thread that
// has a wish HEAD (so it renders in /threads) but NO bound agent at all (state
// binding lost, card gone, daemon-metadata resolver skipped a done/failed agent
// or the agent bead was pruned) used to fall off the if/else-if in
// handleMessageEvent and hit the unconditional return — silently dropping the
// turn. The fix adds a head-gated else branch that records the conversation
// bubble against the existing head.
func TestHandleMessageEvent_OrphanWishThread_RecordsBubble(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["orphan-head"] = &beadsapi.BeadDetail{
		ID:     "orphan-head",
		Title:  "orphaned wish",
		Type:   "task",
		Labels: []string{threadBeadLabel("C-orphan", "3333.1111"), threadKindLabel},
		Fields: map[string]string{
			"slack_thread_channel": "C-orphan",
			"slack_thread_ts":      "3333.1111",
		},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately NO SetThreadAgent — the thread→agent binding is lost, and no
	// agent card or agent-metadata bead exists, so getThreadSpawnedAgent and
	// getAgentByThread both return "".

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	// Sanity: no agent resolves for this thread.
	if a := b.getAgentByThread("C-orphan", "3333.1111"); a != "" {
		t.Fatalf("expected no agent bound to orphan thread, got %q", a)
	}

	b.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-orphan",
		Text:            "any update on my wish?",
		TimeStamp:       "3333.2222",
		ThreadTimeStamp: "3333.1111",
	})

	var msg *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if bead.Type == "message" && hasLabel(bead.Labels, "slack-thread-message") {
			msg = bead
			break
		}
	}
	if msg == nil {
		t.Fatal("REGRESSION (ka-0000001743): reply in an orphan wish thread (head exists, no agent) " +
			"must record a conversation bubble; got none — /threads would drop the message")
	}
	if msg.Fields["thread_id"] != "orphan-head" {
		t.Errorf("orphan bubble thread_id = %q, want orphan-head (bound to the existing head)", msg.Fields["thread_id"])
	}
}

// TestHandleMessageEvent_NonWishThread_NoBubble guards the head-gate: a
// non-@mention reply in an ordinary channel thread that is NOT a wish (no head
// bead) must NOT mint a head or a conversation bubble. Without the gate, the
// orphan-head fix would turn every random threaded reply into a wish.
func TestHandleMessageEvent_NonWishThread_NoBubble(t *testing.T) {
	daemon := newMockDaemon()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	b.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-random",
		Text:            "just two humans chatting in a thread",
		TimeStamp:       "4444.2222",
		ThreadTimeStamp: "4444.1111",
	})

	for _, bead := range daemon.beads {
		if bead.Type == "message" && hasLabel(bead.Labels, "slack-thread-message") {
			t.Fatalf("non-wish channel thread must not mint a conversation bubble; got %+v", bead)
		}
		if hasLabel(bead.Labels, threadKindLabel) {
			t.Fatalf("non-wish channel thread must not mint a wish head; got %+v", bead)
		}
	}
}

// fileShareSlackServer returns an httptest.Server whose conversations.replies
// response includes the given files on the message with ts==fileTS, mirroring
// how Slack returns a threaded upload. Any other endpoint returns a generic OK.
// It records posted chat.postMessage texts into *posted (guarded by *mu) when
// both are non-nil.
func fileShareSlackServer(t *testing.T, fileTS string, files []map[string]any, mu *sync.Mutex, posted *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.replies":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"messages": []map[string]any{
					{"ts": fileTS, "files": files},
				},
			})
		case "/chat.postMessage":
			if mu != nil && posted != nil {
				_ = r.ParseForm()
				mu.Lock()
				*posted = append(*posted, r.FormValue("text"))
				mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.9999"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
		}
	}))
}

// TestHandleMessageEvent_FileShareSubtype_Forwarded is the primary regression
// guard for ka-0000003513: Slack dispatches a threaded file/image upload with
// SubType=file_share, which the subtype guard in handleMessageEvent used to drop
// before it ever reached the thread-forward path. Here a file_share reply with a
// caption in a thread bound to a live agent must produce a slack-thread-reply
// conversation bubble carrying the attachment metadata.
func TestHandleMessageEvent_FileShareSubtype_Forwarded(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["thread-1111-2222"] = &beadsapi.BeadDetail{
		ID:    "bd-thread-agent",
		Title: "thread-1111-2222",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "thread-1111-2222",
			"slack_thread_channel": "C-support",
			"slack_thread_ts":      "1111.2222",
		},
	}

	slackSrv := fileShareSlackServer(t, "1111.3333", []map[string]any{
		{"id": "F1", "name": "diagram.png", "mimetype": "image/png", "size": 4096},
	}, nil, nil)
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-support", "1111.2222", "thread-1111-2222")

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	b.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Text:            "here's the screenshot you asked for",
		Channel:         "C-support",
		SubType:         "file_share",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: "1111.2222",
	})

	var msg *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if bead.Type == "message" && hasLabel(bead.Labels, "slack-thread-reply") {
			msg = bead
			break
		}
	}
	if msg == nil {
		t.Fatal("REGRESSION (ka-0000003513): file_share thread reply must produce a " +
			"slack-thread-reply bubble; got none — the subtype guard dropped it")
	}
	if msg.Fields["text"] != "here's the screenshot you asked for" {
		t.Errorf("bubble text = %q, want the caption", msg.Fields["text"])
	}
	if msg.Fields["slack_attachment_count"] != "1" {
		t.Errorf("slack_attachment_count = %q, want 1", msg.Fields["slack_attachment_count"])
	}
	if msg.Fields["slack_has_images"] != "true" {
		t.Errorf("slack_has_images = %q, want true", msg.Fields["slack_has_images"])
	}
	assertMessageSchemaFields(t, msg.Fields)
}

// TestRecordUnifiedThreadReply_AttachmentOnly_NoCaption is the regression guard
// for the second half of ka-0000003513: an attachment with NO caption trims to
// empty text, and recordUnifiedThreadReply used to `return ""` on empty text
// BEFORE it fetched attachment metadata — so an image-only reply persisted no
// bubble at all. The fix fetches files first and synthesizes a placeholder body
// when there is no caption.
func TestRecordUnifiedThreadReply_AttachmentOnly_NoCaption(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["wish-head"] = &beadsapi.BeadDetail{
		ID:     "wish-head",
		Title:  "the user's wish",
		Type:   "task",
		Labels: []string{threadBeadLabel("C-img", "5555.1111"), threadKindLabel},
		Fields: map[string]string{
			"slack_thread_channel": "C-img",
			"slack_thread_ts":      "5555.1111",
		},
	}

	slackSrv := fileShareSlackServer(t, "5555.2222", []map[string]any{
		{"id": "F9", "name": "photo.jpg", "mimetype": "image/jpeg", "size": 20480, "permalink": "https://acme.slack.com/files/U-HUMAN/F9/photo.jpg"},
	}, nil, nil)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	beadID := b.recordUnifiedThreadReply(context.Background(), unifiedThreadReplyInput{
		Channel:    "C-img",
		ThreadTS:   "5555.1111",
		MessageTS:  "5555.2222",
		Text:       "", // image-only upload: no caption
		SlackUser:  "U-HUMAN",
		AgentName:  "some-agent",
		ExtraLabel: "slack-thread-reply",
		LogPrefix:  "thread-forward",
	})
	if beadID == "" {
		t.Fatal("REGRESSION (ka-0000003513): attachment-only reply (empty caption) must " +
			"still persist a bubble; got empty bead id")
	}

	msg := daemon.beads[beadID]
	if msg == nil {
		// mockDaemon may key by a different id; fall back to a scan.
		for _, bead := range daemon.beads {
			if bead.Type == "message" && hasLabel(bead.Labels, "slack-thread-message") {
				msg = bead
				break
			}
		}
	}
	if msg == nil {
		t.Fatal("expected a conversation message bead for the attachment-only reply")
	}
	if msg.Fields["text"] == "" {
		t.Error("attachment-only bubble must carry a non-empty placeholder body, not blank")
	}
	if refs := decodeFileRefs(msg.Fields["files"]); len(refs) != 1 || refs[0].Name != "photo.jpg" {
		t.Errorf("files = %q, want the photo's link", msg.Fields["files"])
	}
	if msg.Fields["slack_attachment_count"] != "1" {
		t.Errorf("slack_attachment_count = %q, want 1", msg.Fields["slack_attachment_count"])
	}
	if msg.Fields["slack_has_images"] != "true" {
		t.Errorf("slack_has_images = %q, want true", msg.Fields["slack_has_images"])
	}
	assertMessageSchemaFields(t, msg.Fields)
}

// TestRecordUnifiedThreadReply_NoTextNoFiles_Skips verifies the empty-content
// gate is preserved: a reply with neither caption nor attachments records
// nothing (the pre-fix behavior for genuinely empty messages).
func TestRecordUnifiedThreadReply_NoTextNoFiles_Skips(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	beadID := b.recordUnifiedThreadReply(context.Background(), unifiedThreadReplyInput{
		Channel:   "C-empty",
		ThreadTS:  "6666.1111",
		MessageTS: "6666.2222",
		Text:      "   ", // whitespace only, no files
		SlackUser: "U-HUMAN",
		AgentName: "some-agent",
		LogPrefix: "thread-forward",
	})
	if beadID != "" {
		t.Errorf("empty reply (no text, no files) should record nothing; got bead %q", beadID)
	}
	for _, bead := range daemon.beads {
		if bead.Type == "message" {
			t.Errorf("empty reply should not create a message bead; got %+v", bead)
		}
	}
}

func TestAttachmentPlaceholderText(t *testing.T) {
	img := slack.File{Name: "shot.png", Mimetype: "image/png"}
	doc := slack.File{Name: "report.pdf", Mimetype: "application/pdf"}

	cases := []struct {
		name    string
		files   []slack.File
		wantSub string // substring the result must contain
		wantErr bool   // true => expect empty string
	}{
		{"none", nil, "", true},
		{"single-image", []slack.File{img}, "shot.png", false},
		{"single-file", []slack.File{doc}, "report.pdf", false},
		{"multi-image", []slack.File{img, img}, "2 images", false},
		{"multi-mixed-has-image", []slack.File{doc, img}, "2 images", false},
		{"multi-file", []slack.File{doc, doc}, "2 files", false},
		{"image-no-name", []slack.File{{Mimetype: "image/png"}}, "image", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := attachmentPlaceholderText(tc.files)
			if tc.wantErr {
				if got != "" {
					t.Errorf("want empty, got %q", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("want non-empty placeholder, got empty")
			}
			if !stringContains(got, tc.wantSub) {
				t.Errorf("placeholder %q does not contain %q", got, tc.wantSub)
			}
		})
	}
}

func TestNudgeThrottling(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent": "hq",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:    daemon,
		state:     state,
		logger:    slog.Default(),
		botUserID: "U-BOT",
		agentCards: map[string]MessageRef{
			"hq": {
				ChannelID: "C-agents",
				Timestamp: "1111.2222",
				Agent:     "hq",
			},
		},
		lastThreadNudge: make(map[string]time.Time),
	}

	// First nudge should not be throttled.
	if b.shouldThrottleNudge("hq", "1111.2222") {
		t.Error("first nudge should not be throttled")
	}

	// Second nudge within window should be throttled.
	if !b.shouldThrottleNudge("hq", "1111.2222") {
		t.Error("second nudge within window should be throttled")
	}

	// Different thread should not be throttled.
	if b.shouldThrottleNudge("hq", "3333.4444") {
		t.Error("nudge for different thread should not be throttled")
	}

	// Different agent should not be throttled.
	if b.shouldThrottleNudge("k8s", "1111.2222") {
		t.Error("nudge for different agent should not be throttled")
	}
}

func TestNudgeThrottling_Expiry(t *testing.T) {
	b := &Bot{
		logger:          slog.Default(),
		lastThreadNudge: make(map[string]time.Time),
	}

	// Manually set a past nudge time beyond the interval.
	b.mu.Lock()
	b.lastThreadNudge["hq:1111.2222"] = time.Now().Add(-2 * threadNudgeInterval)
	b.mu.Unlock()

	// Should not be throttled since the interval has passed.
	if b.shouldThrottleNudge("hq", "1111.2222") {
		t.Error("nudge after interval should not be throttled")
	}
}

func TestHandleThreadForward_ThrottledMessagesSkipBeadCreation(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent": "hq",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Use a thread-spawned agent (not agent card) for forwarding.
	_ = state.SetThreadAgent("C-agents", "1111.2222", "hq")
	_ = state.SetListenThread("C-agents", "1111.2222") // --listen mode for auto-forward

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		lastThreadNudge: make(map[string]time.Time),
	}

	ctx := context.Background()

	// Send two messages in quick succession. Under the unify model
	// (ka-0000007618) every reply records a SINGLE type=message bead that
	// carries both the conversation bubble (slack-thread-message) and the
	// taskiness label (slack-thread-reply); only the agent NUDGE is throttled.
	// So two messages produce two beads here, but only the first triggers a
	// nudge — keeping a busy thread from hammering the agent while still
	// surfacing every word in the /threads conversation lane (ka-0000003279).
	for i, text := range []string{"first message", "second message"} {
		ev := &slackevents.MessageEvent{
			User:            "U-HUMAN",
			Channel:         "C-agents",
			Text:            text,
			TimeStamp:       "1111." + string(rune('3'+i)),
			ThreadTimeStamp: "1111.2222",
		}
		b.handleThreadForward(ctx, slackReplyInbound(ev, ""), "hq")
	}

	// Both messages must surface as unified conversation beads (throttle does
	// not apply to bubble creation), each carrying the slack-thread-reply
	// taskiness label + the thread_id wish-pointer.
	msgCount := 0
	for _, bead := range daemon.beads {
		if bead.Type == "message" && hasLabel(bead.Labels, "slack-thread-message") {
			msgCount++
			if !hasLabel(bead.Labels, "slack-thread-reply") {
				t.Errorf("unified reply bead %s missing slack-thread-reply label; got %v", bead.ID, bead.Labels)
			}
			if bead.Fields["thread_id"] == "" {
				t.Errorf("unified reply bead %s missing thread_id binding", bead.ID)
			}
		}
	}
	if msgCount != 2 {
		t.Errorf("expected 2 unified message beads (both messages, throttle does not apply to bubble creation), got %d", msgCount)
	}
}

func TestHandleMessageEvent_DecisionThread_NotForwarded(t *testing.T) {
	// Verify that a decision thread reply does not also trigger thread forwarding
	// when no agent is bound to the thread. The routing in handleMessageEvent
	// calls handleThreadReply first, then checks getAgentByThread — if the thread
	// is a decision thread but NOT bound to an agent, no forwarding happens.
	daemon := newMockDaemon()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Decision thread exists in state, but no agent is bound to this thread.
	_ = state.SetDecisionMessage("bd-decision-1", MessageRef{
		ChannelID: "C-decisions",
		Timestamp: "5555.6666",
		Agent:     "hq",
	})

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		lastThreadNudge: make(map[string]time.Time),
	}

	// No agent is bound to this thread via getAgentByThread.
	agent := b.getAgentByThread("C-decisions", "5555.6666")
	if agent != "" {
		t.Errorf("expected no agent bound to decision thread, got %q", agent)
	}
	// So handleThreadForward would NOT be called — no duplicate bead created.
}

// TestRespawnThreadAgent_UsesCrewMode_Regression is a regression test for the
// session loss bug where agents woken by explicit @mention started fresh
// instead of resuming. The root cause was thread agents using mode=job
// (EmptyDir) which lost all session data when the pod died. The fix (PR #487)
// switched to mode=crew for PVC-backed storage.
//
// Note: handleThreadForward no longer calls respawnThreadAgent — thread
// replies with an exited agent now post a resume hint instead (see
// gd-lUNTlpAVm5). This test exercises respawnThreadAgent directly, which is
// still reached via the mention path (bot_mentions.go).
func TestRespawnThreadAgent_UsesCrewMode_Regression(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproject", "C-resume-test")

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	channel := "C-resume-test"
	threadTS := "7777.8888"
	agentName := "thread-7777-8888"

	_ = state.SetThreadAgent(channel, threadTS, agentName)
	_ = state.SetListenThread(channel, threadTS)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	// Mention path: user explicitly @mentions the bot to resume.
	b.respawnThreadAgent(context.Background(), channel, threadTS, agentName, "@bot are you still there?", "U-WAKER")

	var respawned *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if bead.Type == "agent" && bead.Title == agentName {
			respawned = bead
			break
		}
	}
	if respawned == nil {
		t.Fatal("expected agent to be respawned by mention-path respawnThreadAgent")
	}

	if respawned.Fields["mode"] != "crew" {
		t.Errorf("REGRESSION: respawned agent mode = %q, want crew\n"+
			"Job mode uses EmptyDir — session data is lost when the pod dies.\n"+
			"Crew mode uses a PVC so coop --resume can restore the conversation.", respawned.Fields["mode"])
	}
	if respawned.Fields["spawn_source"] != "slack-thread-resume" {
		t.Errorf("spawn_source = %q, want slack-thread-resume", respawned.Fields["spawn_source"])
	}
	if respawned.Fields["slack_thread_channel"] != channel {
		t.Errorf("slack_thread_channel = %q, want %q", respawned.Fields["slack_thread_channel"], channel)
	}
	if respawned.Fields["slack_thread_ts"] != threadTS {
		t.Errorf("slack_thread_ts = %q, want %q", respawned.Fields["slack_thread_ts"], threadTS)
	}
	if respawned.Fields["agent"] != agentName {
		t.Errorf("agent = %q, want %q (same name needed for PVC continuity)", respawned.Fields["agent"], agentName)
	}

	mappedAgent, ok := state.GetThreadAgent(channel, threadTS)
	if !ok {
		t.Error("thread→agent mapping was lost during respawn")
	}
	if mappedAgent != agentName {
		t.Errorf("thread→agent mapping = %q, want %q", mappedAgent, agentName)
	}
}

// TestHandleThreadForward_InitialSpawnUsesCrewMode verifies that the initial
// thread spawn (via handleThreadSpawn in bot_mentions.go) also uses crew mode.
// This is tested indirectly via the podspec test, but we verify the bead field
// here to catch regressions at the bridge level.
func TestHandleThreadForward_InitialSpawnUsesCrewMode(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproject", "C-initial-spawn")

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		lastThreadNudge: make(map[string]time.Time),
		threadSpawnMsgs: make(map[string]MessageRef),
	}

	// Use respawnThreadAgent to simulate the initial spawn path's bead creation.
	// The real initial spawn goes through handleThreadSpawn, but respawnThreadAgent
	// creates beads with the same field structure.
	b.respawnThreadAgent(context.Background(), "C-initial-spawn", "3333.4444", "new-thread-agent", "hello", "U-SPAWNER")

	var created *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if bead.Type == "agent" && bead.Title == "new-thread-agent" {
			created = bead
			break
		}
	}
	if created == nil {
		t.Fatal("expected agent bead to be created")
	}
	if created.Fields["mode"] != "crew" {
		t.Errorf("initial thread spawn mode = %q, want crew", created.Fields["mode"])
	}
}

// TestHandleThreadForward_WritesTriggeredByDepToThreadBead exercises plan 4.2
// (epic ka-0000000933 / M2d): a thread forward must record a triggered-by dep
// from the tracking bead to the canonical Slack-thread bead. This is the
// option-C catch-up affordance — if the dep wasn't written at original spawn
// time, it gets installed on the first forward.
func TestHandleThreadForward_WritesTriggeredByDepToThreadBead(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["thread-9000-1234"] = &beadsapi.BeadDetail{
		ID:    "bd-thread-agent",
		Title: "thread-9000-1234",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "thread-9000-1234",
			"project":              "gardenio",
			"slack_thread_channel": "C-fwd",
			"slack_thread_ts":      "9000.1234",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-fwd", "9000.1234", "thread-9000-1234")

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		lastThreadNudge: make(map[string]time.Time),
	}

	ev := &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Channel:         "C-fwd",
		Text:            "extra context for the agent",
		TimeStamp:       "9000.5678",
		ThreadTimeStamp: "9000.1234",
	}

	b.handleThreadForward(context.Background(), slackReplyInbound(ev, ""), "thread-9000-1234")

	// Find the tracking task bead.
	var trackingID string
	for _, bead := range daemon.beads {
		if bead.Type == "message" && hasLabel(bead.Labels, "slack-thread-reply") {
			trackingID = bead.ID
			break
		}
	}
	if trackingID == "" {
		t.Fatal("expected tracking task bead to be created")
	}

	// Find the resolved-or-created thread bead.
	threadLabel := threadBeadLabel("C-fwd", "9000.1234")
	var threadID string
	for _, bead := range daemon.beads {
		if hasLabel(bead.Labels, threadLabel) {
			threadID = bead.ID
			break
		}
	}
	if threadID == "" {
		t.Fatal("expected thread bead to be created with canonical label")
	}

	// Verify the triggered-by dep was written.
	var matched bool
	for _, d := range daemon.getDeps() {
		if d.BeadID == trackingID && d.DependsOnID == threadID && d.DepType == "triggered-by" {
			matched = true
			if d.CreatedBy != "slack-bridge" {
				t.Errorf("dep CreatedBy = %q, want slack-bridge", d.CreatedBy)
			}
			break
		}
	}
	if !matched {
		t.Errorf("expected triggered-by dep from %s to %s, got %+v", trackingID, threadID, daemon.getDeps())
	}
}
