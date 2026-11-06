package bridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"
	"gardenio/controller/internal/oidcauth"
)

// newCommonsReplyBot is newInboundReplyBot plus the live agent's wish head
// and a Commons reply into it (gd-cm1), which carries only thread_id: its
// Slack thread comes from the head.
func newCommonsReplyBot(t *testing.T) (*Bot, *mockDaemon, *surfacetest.SlackTwin, func() []string) {
	t.Helper()
	twin := surfacetest.NewSlackTwin()
	coop, nudges := newCoopRecorder(t)
	b, daemon := newInboundReplyBot(t, twin, coop)
	// The land's Slack channels: a Commons reply follows a Slack thread only
	// in one of them (slackChannelOfLand).
	seedSlackChannelBead(daemon, "gd-ch-live", "gardenio", "C0LIVE")
	seedSlackChannelBead(daemon, "gd-ch-gone", "gardenio", "C0GONE")
	daemon.beads["gd-head-live"] = &beadsapi.BeadDetail{
		ID: "gd-head-live", Type: "task", Status: "open", Title: "the live wish",
		Labels: []string{"thread", "project:gardenio"},
		Fields: map[string]string{"slack_thread_channel": "C0LIVE", "slack_thread_ts": "1690000000.000100"},
	}
	daemon.beads["gd-cm1"] = commonsReplyBead("gd-cm1", "gd-head-live", "ada@example.com", time.Now().Add(-5*time.Minute))
	return b, daemon, twin, nudges
}

func commonsReplyBead(id, head, author string, created time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Type: "message", Status: "closed", Title: "from the commons",
		Labels: []string{"project:gardenio"}, CreatedBy: author, CreatedAt: created,
		Fields: map[string]string{
			"origin": "commons", "author": "oidc:" + author, "thread_id": head,
			"channel_id": "gd-chan-general", "text": "from the commons",
		},
	}
}

func countContaining(xs []string, sub string) int {
	n := 0
	for _, x := range xs {
		if strings.Contains(x, sub) {
			n++
		}
	}
	return n
}

func TestCommonsMessageInbound(t *testing.T) {
	m := commonsMessageFromDetail(commonsReplyBead("gd-cm1", "gd-head", "ada@example.com", time.Time{}))
	in := m.inbound()
	if in.Kind != conversation.InboundReply || in.Surface != conversation.SurfaceCommons ||
		in.Land != "gardenio" || in.Channel != "gd-chan-general" || in.Thread != "gd-head" ||
		in.Message != "gd-cm1" || in.Actor.ID != "oidc:ada@example.com" || in.Text != "from the commons" {
		t.Fatalf("inbound = %+v", in)
	}
	if _, ok := in.Root(conversation.SurfaceSlack); ok {
		t.Fatal("a message with no Slack binding got a Slack root")
	}

	// A /threads composer bead: bare-email creator, Slack binding on the bead.
	say := commonsMessageFromEvent(&BeadEvent{
		ID: "gd-say1", Type: "message", Title: "t", CreatedBy: "ada@example.com",
		Labels: []string{ThreadSayLabel},
		Fields: map[string]string{"text": "hello", "slack_thread_channel": "C0LIVE", "slack_thread_ts": "1690000000.000100"},
	})
	if !say.fromCommons() || say.author() != "oidc:ada@example.com" {
		t.Fatalf("composer bead: fromCommons=%v author=%q", say.fromCommons(), say.author())
	}
	if root, ok := say.inbound().Root(conversation.SurfaceSlack); !ok || root.Channel != "C0LIVE" {
		t.Fatalf("composer bead root = %+v, %v", root, ok)
	}
	if (commonsMessage{Type: "message", Labels: []string{"slack-thread-reply"}}).fromCommons() {
		t.Fatal("a Slack reply bead read as a Commons message")
	}
}

// TestDispatchCommonsMessage_Once pins the Commons half of HandleReply: the
// reply nudges the thread's live agent off its own bead, handled_at is
// stamped, and a second dispatch of the same bead (the composer forward, the
// inbound call, the sweep) is a no-op.
func TestDispatchCommonsMessage_Once(t *testing.T) {
	b, daemon, _, nudges := newCommonsReplyBot(t)
	ctx := context.Background()

	_, dispatched, err := b.dispatchCommonsMessage(ctx, commonsMessageFromDetail(daemon.beads["gd-cm1"]))
	if err != nil || !dispatched {
		t.Fatalf("dispatch: dispatched=%v err=%v", dispatched, err)
	}
	first := countContaining(nudges(), "(bead gd-cm1)")
	if first == 0 {
		t.Fatalf("live agent not nudged off the commons bead; nudges=%v", nudges())
	}
	if daemon.beads["gd-cm1"].Fields[commonsHandledAt] == "" {
		t.Fatal("handled_at not stamped")
	}
	// Fresh read without handled_at: only the in-process claim stops it.
	again := commonsMessageFromDetail(commonsReplyBead("gd-cm1", "gd-head-live", "ada@example.com", time.Time{}))
	if _, dispatched, _ := b.dispatchCommonsMessage(ctx, again); dispatched {
		t.Fatal("second dispatch of the same bead ran")
	}
	if got := countContaining(nudges(), "(bead gd-cm1)"); got != first {
		t.Fatalf("nudged again: %d nudges, want %d", got, first)
	}
}

// TestDispatchCommonsMessage_NudgesLiveAgent pins half of the Commons I
// check-in (spec 7.1 step 11) against a coop that takes the nudge: a reply
// typed in the Commons reaches the thread's live agent once, anchored on its
// own bead, and respawns nothing. newCoopRecorder's coop answers busy, so the
// tests built on it see the nudge attempted but never delivered.
func TestDispatchCommonsMessage_NudgesLiveAgent(t *testing.T) {
	var mu sync.Mutex
	var got []string
	coop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.URL.Path+" "+string(body))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"delivered":true}`))
	}))
	t.Cleanup(coop.Close)
	b, daemon := newInboundReplyBot(t, surfacetest.NewSlackTwin(), coop.URL)
	seedSlackChannelBead(daemon, "gd-ch-live", "gardenio", "C0LIVE")
	daemon.beads["gd-head-live"] = &beadsapi.BeadDetail{
		ID: "gd-head-live", Type: "task", Status: "open", Title: "the live wish",
		Labels: []string{"thread", "project:gardenio"},
		Fields: map[string]string{"slack_thread_channel": "C0LIVE", "slack_thread_ts": "1690000000.000100"},
	}
	daemon.beads["gd-cm1"] = commonsReplyBead("gd-cm1", "gd-head-live", "ada@example.com", time.Time{})

	if _, dispatched, err := b.dispatchCommonsMessage(context.Background(), commonsMessageFromDetail(daemon.beads["gd-cm1"])); err != nil || !dispatched {
		t.Fatalf("dispatch: dispatched=%v err=%v", dispatched, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !strings.HasPrefix(got[0], "/api/v1/agent/nudge ") || !strings.Contains(got[0], "(bead gd-cm1)") ||
		!strings.Contains(got[0], "from the commons") {
		t.Fatalf("coop calls = %q, want one nudge carrying the reply and its bead", got)
	}
	if n := len(filterAgentBeads(daemon.beads)); n != 1 {
		t.Fatalf("%d agent beads, want only the live one (nothing respawned)", n)
	}
}

// TestDispatchCommonsMessage_RespawnsGoneAgent pins the other half of the
// Commons I check-in (spec 7.1 step 11): a reply typed in the Commons into a
// thread whose agent is gone respawns that agent with session resume, under
// the same name, and nudges nothing, when the inbound call gave it leave to
// spawn (an operator's reply). What a watcher or the sweep dispatches has no
// caller whose land role could give it that, so it leaves the agent be
// (gd-Vc9dC61oKP).
func TestDispatchCommonsMessage_RespawnsGoneAgent(t *testing.T) {
	b, daemon, _, nudges := newCommonsReplyBot(t)
	daemon.seedProjectWithChannel("gardenio", "C0GONE")
	seedPriorAgentBead(daemon, "crew-gone", "gardenio") // its land lets the reply wake it
	daemon.beads["gd-head-gone"] = &beadsapi.BeadDetail{
		ID: "gd-head-gone", Type: "task", Status: "open", Title: "the gone wish",
		Labels: []string{"thread", "project:gardenio"},
		Fields: map[string]string{"slack_thread_channel": "C0GONE", "slack_thread_ts": "1690000000.000200"},
	}
	daemon.beads["gd-cm-stay"] = commonsReplyBead("gd-cm-stay", "gd-head-gone", "ada@example.com", time.Time{})
	agentsBefore := len(filterAgentBeads(daemon.beads))
	if _, dispatched, err := b.dispatchCommonsMessage(context.Background(), commonsMessageFromDetail(daemon.beads["gd-cm-stay"])); err != nil || !dispatched {
		t.Fatalf("dispatch: dispatched=%v err=%v", dispatched, err)
	}
	if n := len(filterAgentBeads(daemon.beads)) - agentsBefore; n != 0 {
		t.Fatalf("a reply with no leave to spawn respawned %d agents", n)
	}

	daemon.beads["gd-cm-gone"] = commonsReplyBead("gd-cm-gone", "gd-head-gone", "ada@example.com", time.Time{})
	_, dispatched, err := b.dispatchCommonsAs(context.Background(), commonsMessageFromDetail(daemon.beads["gd-cm-gone"]), conversation.InboundReply, true)
	if err != nil || !dispatched {
		t.Fatalf("dispatch: dispatched=%v err=%v", dispatched, err)
	}
	bead := singleResumeBead(t, daemon)
	if bead.Fields["agent"] != "crew-gone" {
		t.Fatalf("respawned %q, want the thread's own agent crew-gone", bead.Fields["agent"])
	}
	if n := countContaining(nudges(), "(bead gd-cm-gone)"); n != 0 {
		t.Fatalf("a gone agent was nudged %d times instead of respawned", n)
	}
	if daemon.beads["gd-cm-gone"].Fields[commonsHandledAt] == "" {
		t.Fatal("handled_at not stamped")
	}
}

// A reply in a Commons thread with no Slack copy is the head's (slice 4d): a
// wish no agent works yet just keeps the words, and a reply under a chat root
// that is no wish has no thread to hand to.
func TestDispatchCommonsMessage_NoSlackThread(t *testing.T) {
	b, daemon, _, _ := newCommonsReplyBot(t)
	agentsBefore := len(filterAgentBeads(daemon.beads))
	daemon.beads["gd-head-bare"] = &beadsapi.BeadDetail{ID: "gd-head-bare", Type: "task", Status: "open", Labels: []string{"thread", "project:gardenio"}}
	daemon.beads["gd-cm2"] = commonsReplyBead("gd-cm2", "gd-head-bare", "ada@example.com", time.Time{})
	_, dispatched, err := b.dispatchCommonsMessage(context.Background(), commonsMessageFromDetail(daemon.beads["gd-cm2"]))
	if !dispatched || err != nil {
		t.Fatalf("dispatched=%v err=%v, want a handled reply", dispatched, err)
	}
	if daemon.beads["gd-cm2"].Fields[commonsHandledAt] == "" {
		t.Fatal("handled_at not stamped")
	}
	if n := len(filterAgentBeads(daemon.beads)) - agentsBefore; n != 0 {
		t.Fatalf("a plain reply spawned %d agents; only a mention spawns", n)
	}

	daemon.beads["gd-chat-root"] = &beadsapi.BeadDetail{ID: "gd-chat-root", Type: "message", Status: "closed", Labels: []string{"project:gardenio"}}
	daemon.beads["gd-cm3"] = commonsReplyBead("gd-cm3", "gd-chat-root", "ada@example.com", time.Time{})
	_, dispatched, err = b.dispatchCommonsMessage(context.Background(), commonsMessageFromDetail(daemon.beads["gd-cm3"]))
	if !dispatched || !errors.Is(err, conversation.ErrNoThread) {
		t.Fatalf("dispatched=%v err=%v, want ErrNoThread", dispatched, err)
	}
	if daemon.beads["gd-cm3"].Fields[commonsHandledAt] == "" {
		t.Fatal("a failed dispatch must be stamped so the sweep does not retry it forever")
	}
}

func TestInboundDispatcher_RecoversPanic(t *testing.T) {
	b, _, _, _ := newCommonsReplyBot(t)
	b.inbound().Handle(conversation.InboundReply, func(context.Context, conversation.Inbound) (conversation.Answer, error) {
		panic("boom")
	})
	_, err := b.inbound().Dispatch(context.Background(), conversation.Inbound{
		Kind: conversation.InboundReply, Surface: conversation.SurfaceCommons, Thread: "gd-head-live",
	})
	if !errors.Is(err, conversation.ErrHandlerPanic) {
		t.Fatalf("err = %v, want ErrHandlerPanic", err)
	}
}

func TestSweepCommonsInbound(t *testing.T) {
	b, daemon, _, nudges := newCommonsReplyBot(t)
	now := time.Now()
	daemon.beads["gd-cm-fresh"] = commonsReplyBead("gd-cm-fresh", "gd-head-live", "ada@example.com", now.Add(-10*time.Second))
	done := commonsReplyBead("gd-cm-done", "gd-head-live", "ada@example.com", now.Add(-10*time.Minute))
	done.Fields[commonsHandledAt] = now.Add(-9 * time.Minute).Format(time.RFC3339)
	daemon.beads["gd-cm-done"] = done
	// A top-level Commons message has no thread to hand to an agent.
	top := commonsReplyBead("gd-cm-top", "", "ada@example.com", now.Add(-10*time.Minute))
	delete(top.Fields, conversation.BeadThreadID)
	daemon.beads["gd-cm-top"] = top

	if n := b.sweepCommonsInbound(context.Background(), now); n != 1 {
		t.Fatalf("sweep dispatched %d, want 1 (gd-cm1 only)", n)
	}
	if countContaining(nudges(), "(bead gd-cm1)") == 0 {
		t.Fatal("swept reply did not nudge the agent")
	}
	for _, id := range []string{"gd-cm-fresh", "gd-cm-done", "gd-cm-top"} {
		if countContaining(nudges(), "(bead "+id+")") != 0 {
			t.Errorf("sweep dispatched %s", id)
		}
	}
	if daemon.beads["gd-cm-top"].Fields[commonsHandledAt] != "" {
		t.Error("the sweep stamped a top-level message it has no thread for")
	}
	if n := b.sweepCommonsInbound(context.Background(), now); n != 0 {
		t.Fatalf("second sweep dispatched %d, want 0", n)
	}
}

// TestSweepCommonsInbound_StampRefused pins gd-2IEp8qx7bq: when the daemon
// refuses the handled_at stamp, the sweep must not hand the message to its
// agent again once the hour-long dispatch claim would have lapsed. The claim
// is held for the whole sweep window instead.
func TestSweepCommonsInbound_StampRefused(t *testing.T) {
	b, daemon, _, nudges := newCommonsReplyBot(t)
	daemon.updateFieldsErr = func(_ string, fields map[string]string) error {
		if _, ok := fields[commonsHandledAt]; ok {
			return errors.New("HTTP 403: ownership denied: update on bead gd-cm1")
		}
		return nil
	}
	now := time.Now()
	if n := b.sweepCommonsInbound(context.Background(), now); n != 1 {
		t.Fatalf("sweep dispatched %d, want 1", n)
	}
	if daemon.beads["gd-cm1"].Fields[commonsHandledAt] != "" {
		t.Fatal("the refused stamp landed")
	}
	// One nudge is several coop calls; count them for the one dispatch.
	nudged := countContaining(nudges(), "(bead gd-cm1)")
	if nudged == 0 {
		t.Fatal("swept reply did not nudge the agent")
	}
	// Two hours on: the plain claim has lapsed, the message is still in the
	// sweep's window and still has no handled_at.
	b.conv.mu.Lock()
	for id, until := range b.conv.claims {
		b.conv.claims[id] = until.Add(-2 * time.Hour)
	}
	b.conv.mu.Unlock()
	if n := b.sweepCommonsInbound(context.Background(), now.Add(2*time.Hour)); n != 0 {
		t.Fatalf("sweep re-dispatched %d after the stamp failed, want 0", n)
	}
	if got := countContaining(nudges(), "(bead gd-cm1)"); got != nudged {
		t.Fatalf("agent nudged again with gd-cm1 (%d coop calls, want %d)", got, nudged)
	}
}

func TestCommonsMessageWrittenByAuthor(t *testing.T) {
	for _, c := range []struct {
		name, createdBy, author string
		want                    bool
	}{
		{"commons write", "ada@example.com", "oidc:ada@example.com", true},
		{"author case differs", "Ada@Example.com", "oidc:ada@example.com", true},
		{"composer: no author field", "ada@example.com", "", true},
		{"oidc creator", "oidc:ada@example.com", "oidc:ada@example.com", true},
		{"agent names a human", "gardenio-crew-x", "oidc:ada@example.com", false},
		{"static names a human", "static", "oidc:ada@example.com", false},
		{"agent, no author", "gardenio-crew-x", "", false},
		{"another human named", "bob@example.com", "oidc:ada@example.com", false},
		{"machine creator", "machine:gardenio-beads-viewer", "oidc:ada@example.com", false},
		{"slack creator", "slack:U123", "slack:U123", false},
		{"no creator", "", "oidc:ada@example.com", false},
	} {
		m := commonsMessage{Type: "message", CreatedBy: c.createdBy, Fields: map[string]string{conversation.BeadAuthor: c.author}}
		if got := m.writtenByAuthor(); got != c.want {
			t.Errorf("%s: writtenByAuthor(created_by=%q, author=%q) = %v, want %v", c.name, c.createdBy, c.author, got, c.want)
		}
	}
}

// TestSweepCommonsInbound_RefusesAgentWritten pins gd-vnVsKJo4zw: any writer
// may set origin=commons, a thread_id and an author naming a human, but the
// sweep dispatches only a message that human wrote. An agent-written one, or
// one a human wrote naming another, reaches no agent and spawns nothing, and
// is stamped so it is not retried.
func TestSweepCommonsInbound_RefusesAgentWritten(t *testing.T) {
	b, daemon, _, nudges := newCommonsReplyBot(t)
	now := time.Now()
	daemon.seedProjectWithChannel("gardenio", "C0GONE")
	daemon.beads["gd-head-gone"] = &beadsapi.BeadDetail{
		ID: "gd-head-gone", Type: "task", Status: "open", Title: "the gone wish",
		Labels: []string{"thread", "project:gardenio"},
		Fields: map[string]string{"slack_thread_channel": "C0GONE", "slack_thread_ts": "1690000000.000200"},
	}
	forged := map[string]string{
		"gd-cm-agent":      "gardenio-crew-x",
		"gd-cm-static":     "static",
		"gd-cm-other":      "bob@example.com",
		"gd-cm-agent-gone": "gardenio-crew-x",
	}
	for id, creator := range forged {
		head := "gd-head-live"
		if id == "gd-cm-agent-gone" {
			head = "gd-head-gone"
		}
		m := commonsReplyBead(id, head, "ada@example.com", now.Add(-5*time.Minute))
		m.CreatedBy = creator
		daemon.beads[id] = m
	}
	agentsBefore := len(filterAgentBeads(daemon.beads))

	if n := b.sweepCommonsInbound(context.Background(), now); n != 1 {
		t.Fatalf("sweep dispatched %d, want 1 (the human-written gd-cm1 only)", n)
	}
	if countContaining(nudges(), "(bead gd-cm1)") == 0 {
		t.Fatal("the human-written reply did not reach the agent")
	}
	for id := range forged {
		if countContaining(nudges(), "(bead "+id+")") != 0 {
			t.Errorf("sweep nudged the agent with forged %s", id)
		}
		if daemon.beads[id].Fields[commonsHandledAt] == "" {
			t.Errorf("forged %s not stamped handled_at; the sweep would retry it", id)
		}
	}
	if n := len(filterAgentBeads(daemon.beads)); n != agentsBefore {
		t.Fatalf("%d agent beads after the sweep, want %d (a forged reply respawned an agent)", n, agentsBefore)
	}
}

func TestIsUtteranceSubtype(t *testing.T) {
	for subtype, want := range map[string]bool{
		"": true, "thread_broadcast": true, "file_share": true,
		"message_changed": false, "message_deleted": false, "bot_message": false, "channel_join": false,
	} {
		if got := isUtteranceSubtype(subtype); got != want {
			t.Errorf("isUtteranceSubtype(%q) = %v, want %v", subtype, got, want)
		}
	}
}

func TestConversationInboundAPI(t *testing.T) {
	b, daemon, _, nudges := newCommonsReplyBot(t)
	daemon.beads["gd-cm-bob"] = commonsReplyBead("gd-cm-bob", "gd-head-live", "bob@example.com", time.Time{})
	forged := commonsReplyBead("gd-cm-forged", "gd-head-live", "ada@example.com", time.Time{})
	forged.CreatedBy = "gardenio-crew-x"
	daemon.beads["gd-cm-forged"] = forged
	daemon.beads["gd-slack-reply"] = &beadsapi.BeadDetail{ID: "gd-slack-reply", Type: "message", Labels: []string{"slack-thread-reply"}}
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	post := func(email, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/conversation/inbound", strings.NewReader(body))
		if email != "" {
			ctx := oidcauth.ContextWithEmail(req.Context(), email)
			if email != "member@example.com" {
				ctx = oidcauth.ContextWithRoles(ctx, []string{oidcauth.RoleAdmin})
			}
			req = req.WithContext(ctx)
		}
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr
	}
	for _, c := range []struct {
		name, email, body string
		code              int
		want              string
	}{
		{"unauthenticated", "", `{"kind":"reply","message":"gd-cm1"}`, http.StatusUnauthorized, ""},
		{"bad json", "ada@example.com", `{`, http.StatusBadRequest, ""},
		{"no message", "ada@example.com", `{"kind":"reply"}`, http.StatusBadRequest, ""},
		{"other kind", "ada@example.com", `{"kind":"react","message":"gd-cm1"}`, http.StatusUnprocessableEntity, ""},
		{"reply by a non-admin", "member@example.com", `{"kind":"reply","message":"gd-cm1"}`, http.StatusForbidden, ""},
		{"slack bead", "ada@example.com", `{"kind":"reply","message":"gd-slack-reply"}`, http.StatusUnprocessableEntity, ""},
		{"someone else's", "ada@example.com", `{"kind":"reply","message":"gd-cm-bob"}`, http.StatusForbidden, ""},
		{"agent-written in my name", "ada@example.com", `{"kind":"reply","message":"gd-cm-forged"}`, http.StatusForbidden, ""},
		{"handled", "Ada@Example.com", `{"kind":"reply","message":"gd-cm1"}`, http.StatusOK, `"status":"handled"`},
		{"duplicate", "ada@example.com", `{"kind":"reply","message":"gd-cm1"}`, http.StatusOK, `"status":"duplicate"`},
	} {
		rr := post(c.email, c.body)
		if rr.Code != c.code || !strings.Contains(rr.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, rr.Code, rr.Body.String(), c.code, c.want)
		}
	}
	if countContaining(nudges(), "(bead gd-cm-bob)") != 0 || countContaining(nudges(), "(bead gd-cm-forged)") != 0 {
		t.Fatal("a refused call reached the agent")
	}
	if countContaining(nudges(), "(bead gd-cm1)") == 0 {
		t.Fatal("the handled call did not reach the agent")
	}
}
