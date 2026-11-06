package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// dmSlackFixture is a fake Slack that supports the DM path: conversations.open
// returns a DM channel id, and every chat.* call is recorded with its channel so
// a test can tell a DM post from a channel post.
type dmSlackFixture struct {
	srv *httptest.Server

	mu      sync.Mutex
	posts   []dmSlackCall
	updates []dmSlackCall
	deletes []dmSlackCall
	opens   int
}

type dmSlackCall struct {
	channel string
	ts      string
	text    string
	blocks  string
	thread  string
}

const fixtureDMChannel = "D-OWNER"

func newDMSlackFixture(t *testing.T) *dmSlackFixture {
	t.Helper()
	f := &dmSlackFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		call := dmSlackCall{
			channel: r.FormValue("channel"),
			ts:      r.FormValue("ts"),
			text:    r.FormValue("text"),
			blocks:  r.FormValue("blocks"),
			thread:  r.FormValue("thread_ts"),
		}
		f.mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "conversations.open"):
			f.opens++
		case strings.HasSuffix(r.URL.Path, "chat.postMessage"):
			f.posts = append(f.posts, call)
		case strings.HasSuffix(r.URL.Path, "chat.update"):
			f.updates = append(f.updates, call)
		case strings.HasSuffix(r.URL.Path, "chat.delete"):
			f.deletes = append(f.deletes, call)
		}
		f.mu.Unlock()

		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "conversations.open") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": fixtureDMChannel},
			})
			return
		}
		// Echo a per-channel ts so the caller's tracked ref is distinguishable.
		ts := "1700.0001"
		if call.channel == fixtureDMChannel {
			ts = "1700.DM01"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": call.channel, "ts": ts, "message_ts": ts,
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *dmSlackFixture) snapshot() ([]dmSlackCall, []dmSlackCall, []dmSlackCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dmSlackCall(nil), f.posts...),
		append([]dmSlackCall(nil), f.updates...),
		append([]dmSlackCall(nil), f.deletes...)
}

// seedDecisionAgent registers an agent bead carrying the addressee's slack id,
// which is what resolveDecisionMentionUser reads.
func seedDecisionAgent(daemon *mockDaemon, agentName, slackUserID string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     agentName,
		Type:   "agent",
		Fields: map[string]string{"slack_user_id": slackUserID},
	}
}

// bindDecisionChannel makes resolveChannel return ch for this agent, standing in
// for the spawn channel a real decision would be posted to.
func bindDecisionChannel(bot *Bot, agent, ch string) {
	bot.mu.Lock()
	defer bot.mu.Unlock()
	bot.agentSpawnChannel[agent] = ch
}

func decisionEvent(id, agent string) BeadEvent {
	return BeadEvent{
		ID:       id,
		Title:    "Push the fix to your MR?",
		Type:     "decision",
		Assignee: agent,
		Fields: map[string]string{
			"prompt":  "Push the fix to your MR?",
			"options": `[{"id":"push","short":"Push it"},{"id":"no","short":"No thanks"}]`,
		},
	}
}

// TestNotifyDecision_DMFirst is the core assertion of the operator ask: the
// ANSWERABLE decision card goes to the addressed human's DM, and the channel
// gets a pointer rather than a second set of buttons.
func TestNotifyDecision_DMFirst(t *testing.T) {
	daemon := newMockDaemon()
	seedDecisionAgent(daemon, "agent-x", "UOWNER")
	f := newDMSlackFixture(t)
	bot := newTestBot(daemon, f.srv)
	bindDecisionChannel(bot, "agent-x", "C123")

	if err := bot.NotifyDecision(context.Background(), decisionEvent("dec-1", "agent-x")); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	posts, _, _ := f.snapshot()

	var dmPost, channelPost *dmSlackCall
	for i := range posts {
		if posts[i].channel == fixtureDMChannel {
			dmPost = &posts[i]
		} else if strings.Contains(posts[i].text, "Decision sent to") {
			channelPost = &posts[i]
		}
	}

	if dmPost == nil {
		t.Fatalf("expected the decision card to be posted to the DM channel; posts=%+v", posts)
	}
	// The DM must carry the ANSWERABLE card — the option buttons.
	if !strings.Contains(dmPost.blocks, "resolve_dec-1_1") {
		t.Errorf("DM card must carry the option buttons, got blocks=%s", dmPost.blocks)
	}

	if channelPost == nil {
		t.Fatalf("expected a channel pointer post; posts=%+v", posts)
	}
	if strings.Contains(channelPost.blocks, "resolve_dec-1_") {
		t.Errorf("channel pointer must NOT carry buttons (exactly one answerable copy), got %s", channelPost.blocks)
	}
	if !strings.Contains(channelPost.text, "<@UOWNER>") {
		t.Errorf("channel pointer should name the addressee, got %q", channelPost.text)
	}

	// The PRIMARY tracked ref must be the DM — that is what the resolve modal,
	// reports and tldr updates target.
	ref, ok := bot.lookupMessage("dec-1")
	if !ok {
		t.Fatal("expected a tracked primary message ref for dec-1")
	}
	if ref.ChannelID != fixtureDMChannel {
		t.Errorf("primary ref should be the DM copy, got channel %q", ref.ChannelID)
	}
	// And the channel pointer must be tracked as a MIRROR so it stays truthful.
	mirrors := bot.lookupMessageMirrors("dec-1")
	if len(mirrors) != 1 {
		t.Fatalf("expected 1 tracked mirror (the channel pointer), got %d: %+v", len(mirrors), mirrors)
	}
	if mirrors[0].ChannelID == fixtureDMChannel {
		t.Errorf("mirror should be the CHANNEL pointer, not the DM: %+v", mirrors[0])
	}
}

// TestNotifyDecision_DMDisabledFallsBackToChannel pins the kill switch:
// SLACK_DECISION_DM=off restores channel-only posting.
func TestNotifyDecision_DMDisabledFallsBackToChannel(t *testing.T) {
	daemon := newMockDaemon()
	seedDecisionAgent(daemon, "agent-x", "UOWNER")
	f := newDMSlackFixture(t)
	bot := newTestBot(daemon, f.srv)
	bindDecisionChannel(bot, "agent-x", "C123")
	bot.decisionDMMode = "off"

	if err := bot.NotifyDecision(context.Background(), decisionEvent("dec-1", "agent-x")); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	posts, _, _ := f.snapshot()
	for _, p := range posts {
		if p.channel == fixtureDMChannel {
			t.Errorf("no DM should be sent with the kill switch on, got %+v", p)
		}
	}
	if len(bot.lookupMessageMirrors("dec-1")) != 0 {
		t.Errorf("kill switch should produce no mirrors")
	}
	// The channel copy is the answerable one again.
	var found bool
	for _, p := range posts {
		if strings.Contains(p.blocks, "resolve_dec-1_1") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the answerable card in the channel; posts=%+v", posts)
	}
}

// TestNotifyDecision_NoAddresseeKeepsChannelCard asserts the unaddressed case is
// untouched: a decision with no resolvable human still posts its card to the
// channel and creates no mirror.
func TestNotifyDecision_NoAddresseeKeepsChannelCard(t *testing.T) {
	daemon := newMockDaemon()
	seedDecisionAgent(daemon, "agent-x", "") // no slack_user_id
	f := newDMSlackFixture(t)
	bot := newTestBot(daemon, f.srv)
	bindDecisionChannel(bot, "agent-x", "C123")

	if err := bot.NotifyDecision(context.Background(), decisionEvent("dec-1", "agent-x")); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	posts, _, _ := f.snapshot()
	for _, p := range posts {
		if p.channel == fixtureDMChannel {
			t.Errorf("ownerless decision must not be DMed, got %+v", p)
		}
	}
	if got := len(bot.lookupMessageMirrors("dec-1")); got != 0 {
		t.Errorf("ownerless decision should have no mirrors, got %d", got)
	}
}

// TestDecisionMirrorsUpdatedOnResolve is the reason C1 exists: a second copy of
// a decision must not keep advertising an open ask after it is answered.
func TestDecisionMirrorsUpdatedOnResolve(t *testing.T) {
	daemon := newMockDaemon()
	seedDecisionAgent(daemon, "agent-x", "UOWNER")
	f := newDMSlackFixture(t)
	bot := newTestBot(daemon, f.srv)
	bindDecisionChannel(bot, "agent-x", "C123")

	if err := bot.NotifyDecision(context.Background(), decisionEvent("dec-1", "agent-x")); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}
	mirrors := bot.lookupMessageMirrors("dec-1")
	if len(mirrors) != 1 {
		t.Fatalf("setup: expected 1 mirror, got %d", len(mirrors))
	}

	if err := bot.UpdateDecision(context.Background(), "dec-1", "push", "ship it"); err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}

	_, updates, _ := f.snapshot()
	var primaryUpdated, mirrorUpdated bool
	for _, u := range updates {
		if !strings.Contains(u.blocks, "Resolved") {
			continue
		}
		if u.channel == fixtureDMChannel {
			primaryUpdated = true
		}
		if u.channel == mirrors[0].ChannelID && u.ts == mirrors[0].Timestamp {
			mirrorUpdated = true
		}
	}
	if !primaryUpdated {
		t.Errorf("primary (DM) copy was not updated to resolved; updates=%+v", updates)
	}
	if !mirrorUpdated {
		t.Errorf("MIRROR was not updated to resolved — it would keep showing an open decision forever; updates=%+v", updates)
	}
}

// TestDecisionMirrorsDeletedOnDismiss asserts a dismissed decision leaves no
// live-looking copy behind in the channel.
func TestDecisionMirrorsDeletedOnDismiss(t *testing.T) {
	daemon := newMockDaemon()
	seedDecisionAgent(daemon, "agent-x", "UOWNER")
	f := newDMSlackFixture(t)
	bot := newTestBot(daemon, f.srv)
	bindDecisionChannel(bot, "agent-x", "C123")

	if err := bot.NotifyDecision(context.Background(), decisionEvent("dec-1", "agent-x")); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}
	mirrors := bot.lookupMessageMirrors("dec-1")
	if len(mirrors) != 1 {
		t.Fatalf("setup: expected 1 mirror, got %d", len(mirrors))
	}

	if err := bot.DismissDecision(context.Background(), "dec-1"); err != nil {
		t.Fatalf("DismissDecision: %v", err)
	}

	_, _, deletes := f.snapshot()
	var mirrorDeleted bool
	for _, d := range deletes {
		if d.channel == mirrors[0].ChannelID && d.ts == mirrors[0].Timestamp {
			mirrorDeleted = true
		}
	}
	if !mirrorDeleted {
		t.Errorf("mirror was not deleted on dismiss; deletes=%+v", deletes)
	}
	if got := len(bot.lookupMessageMirrors("dec-1")); got != 0 {
		t.Errorf("mirror tracking should be cleared after dismiss, got %d", got)
	}
}
