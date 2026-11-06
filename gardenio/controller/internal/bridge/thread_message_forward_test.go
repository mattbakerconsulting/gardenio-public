package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"gardenio/controller/internal/conversation/surfacetest"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	slackapi "github.com/slack-go/slack"
	"golang.org/x/time/rate"
)

// capturingThreadSlack is a Slack-named conversation.Surface that records each
// Reply: the thread root it was routed into and the text, so tests can assert
// the message goes INTO the thread rather than to the channel root. Only Name
// and Reply are implemented; the forwarder calls nothing else.
type capturingThreadSlack struct {
	conversation.Surface

	mu       sync.Mutex
	calls    int
	channel  string
	threadTS string
	text     string
	err      error
}

func (c *capturingThreadSlack) Name() string { return conversation.SurfaceSlack }

func (c *capturingThreadSlack) Reply(_ context.Context, thread conversation.Ref, m conversation.Message, _ bool) (conversation.Ref, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.channel = thread.Channel
	c.threadTS = thread.TS
	c.text = m.Text
	if c.err != nil {
		return conversation.Ref{}, c.err
	}
	return conversation.Ref{Surface: conversation.SurfaceSlack, Channel: thread.Channel, TS: "9999.0001"}, nil
}

// threadRouter is a conversation Router whose only surface is s.
func threadRouter(s conversation.Surface) *conversation.Router {
	return conversation.NewRouter(newMockDaemon(), nil, s)
}

// newThreadSayBead is a composer bead as the viewer writes it, on the
// person's own bearer, so created_by is their email.
func newThreadSayBead(t *testing.T, id string, fields map[string]string) []byte {
	t.Helper()
	return newThreadSayBeadBy(t, id, "ada@example.com", fields)
}

func newThreadSayBeadBy(t *testing.T, id, createdBy string, fields map[string]string) []byte {
	t.Helper()
	return newThreadSayBeadLabelled(t, id, createdBy, []string{ThreadSayLabel}, fields)
}

// newThreadSayBeadIn is a composer message of land whose thread is a wish of
// land bound to channel/ts, in a Slack channel a channel bead of land binds:
// a Commons reply follows a Slack thread only there (gd-cVqdya4Zfo).
func newThreadSayBeadIn(t *testing.T, d *mockDaemon, id, land string, fields map[string]string) []byte {
	t.Helper()
	ch, ts, head := fields["slack_thread_channel"], fields["slack_thread_ts"], fields["thread_id"]
	d.mu.Lock()
	d.beads[head] = &beadsapi.BeadDetail{
		ID: head, Type: "task", Status: "open", Title: "a viewer wish",
		Labels: []string{"thread", "project:" + land},
		Fields: map[string]string{"slack_thread_channel": ch, "slack_thread_ts": ts},
	}
	d.mu.Unlock()
	seedSlackChannelBead(d, "gd-ch-"+strings.ToLower(ch), land, ch)
	return newThreadSayBeadLabelled(t, id, "ada@example.com", []string{ThreadSayLabel, "project:" + land}, fields)
}

func newThreadSayBeadLabelled(t *testing.T, id, createdBy string, labels []string, fields map[string]string) []byte {
	t.Helper()
	var ev map[string]map[string]any
	if err := json.Unmarshal(marshalBeadEvent(t, id, "message", labels, fields), &ev); err != nil {
		t.Fatalf("unmarshal bead event: %v", err)
	}
	ev["bead"]["created_by"] = createdBy
	out, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal bead event: %v", err)
	}
	return out
}

func TestThreadMessageForward_PostsIntoThread(t *testing.T) {
	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Logger: slog.Default()})
	if fwd == nil {
		t.Fatal("NewThreadMessageForward returned nil")
	}

	data := newThreadSayBead(t, "gd-msg1", map[string]string{
		"text":                 "hello thread",
		"slack_thread_channel": "C123",
		"slack_thread_ts":      "1700000000.000100",
		"thread_id":            "gd-wish1",
	})
	fwd.handleClosed(context.Background(), data)

	if api.calls != 1 {
		t.Fatalf("PostMessage calls = %d, want 1", api.calls)
	}
	if api.channel != "C123" {
		t.Errorf("channel = %q, want C123", api.channel)
	}
	// The load-bearing assertion: the message is routed INTO the thread via
	// MsgOptionTS, not posted at the channel root.
	if api.threadTS != "1700000000.000100" {
		t.Errorf("thread_ts = %q, want 1700000000.000100 (MsgOptionTS must be applied)", api.threadTS)
	}
	if api.text != "hello thread" {
		t.Errorf("text = %q, want hello thread", api.text)
	}
}

// A real *slack.Client (under the Slack surface) pointed at an httptest server
// proves MsgOptionTS actually serializes thread_ts onto the chat.postMessage
// request — the surface fake alone can't catch a regression where the option
// is dropped on the wire.
func TestThreadMessageForward_RealClientSendsThreadTS(t *testing.T) {
	var mu sync.Mutex
	var gotChannel, gotThreadTS, gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			gotChannel = r.FormValue("channel")
			gotThreadTS = r.FormValue("thread_ts")
			gotText = r.FormValue("text")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C123","ts":"9999.0002"}`))
	}))
	defer srv.Close()

	client := slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(newSlackSurface(client)), Logger: slog.Default()})

	data := newThreadSayBead(t, "gd-msg2", map[string]string{
		"text":                 "real client thread post",
		"slack_thread_channel": "C123",
		"slack_thread_ts":      "1700000000.000999",
	})
	fwd.handleClosed(context.Background(), data)

	mu.Lock()
	defer mu.Unlock()
	if gotChannel != "C123" {
		t.Errorf("channel = %q, want C123", gotChannel)
	}
	if gotThreadTS != "1700000000.000999" {
		t.Errorf("thread_ts = %q, want 1700000000.000999", gotThreadTS)
	}
	if gotText != "real client thread post" {
		t.Errorf("text = %q", gotText)
	}
}

func TestThreadMessageForward_TitleFallbackWhenNoTextField(t *testing.T) {
	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Logger: slog.Default()})
	// No "text" field — the watcher falls back to the bead Title.
	data := marshalBeadEventTitled(t, "gd-msg3", "message", "fallback title",
		[]string{ThreadSayLabel}, map[string]string{
			"slack_thread_channel": "C123",
			"slack_thread_ts":      "1.2",
		})
	fwd.handleClosed(context.Background(), data)
	if api.calls != 1 || api.text != "fallback title" {
		t.Errorf("calls=%d text=%q, want 1 / fallback title", api.calls, api.text)
	}
}

// marshalBeadEventTitled is marshalBeadEvent with an explicit title (the shared
// helper omits the title field). Used to exercise the text→title fallback.
func marshalBeadEventTitled(t *testing.T, id, btype, title string, labels []string, fields map[string]string) []byte {
	t.Helper()
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	inner := map[string]any{
		"id":     id,
		"type":   btype,
		"title":  title,
		"labels": labels,
		"fields": json.RawMessage(fieldsJSON),
	}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal bead: %v", err)
	}
	out, err := json.Marshal(map[string]json.RawMessage{"bead": innerJSON})
	if err != nil {
		t.Fatalf("marshal wrapper: %v", err)
	}
	return out
}

func TestThreadMessageForward_IgnoresNonThreadSayBeads(t *testing.T) {
	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Logger: slog.Default()})

	// Wrong type (task with the label).
	fwd.handleClosed(context.Background(), marshalBeadEvent(t, "k1", "task",
		[]string{ThreadSayLabel}, map[string]string{"text": "x", "slack_thread_channel": "C1", "slack_thread_ts": "1.2"}))
	// Right type, missing the thread-say label (e.g. a squawk or notice bead).
	fwd.handleClosed(context.Background(), marshalBeadEvent(t, "k2", "message",
		[]string{"squawk"}, map[string]string{"text": "x", "slack_thread_channel": "C1", "slack_thread_ts": "1.2"}))

	if api.calls != 0 {
		t.Errorf("PostMessage called %d times for non-thread-say beads, want 0", api.calls)
	}
}

func TestThreadMessageForward_DropsInvalidBinding(t *testing.T) {
	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Logger: slog.Default()})

	cases := []map[string]string{
		{"text": "x"}, // no binding at all
		{"text": "x", "slack_thread_channel": "C1"},                             // no ts
		{"text": "x", "slack_thread_ts": "1.2"},                                 // no channel
		{"text": "x", "slack_thread_channel": "C1", "slack_thread_ts": "nodot"}, // malformed ts
	}
	for i, fields := range cases {
		fwd.handleClosed(context.Background(), newThreadSayBead(t, "gd-bad", fields))
		if api.calls != 0 {
			t.Errorf("case %d: PostMessage called for invalid binding %v", i, fields)
		}
	}
}

func TestThreadMessageForward_DropsEmptyText(t *testing.T) {
	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Logger: slog.Default()})
	// No text and no title → nothing to post.
	fwd.handleClosed(context.Background(), newThreadSayBead(t, "gd-empty", map[string]string{
		"slack_thread_channel": "C123",
		"slack_thread_ts":      "1.2",
	}))
	if api.calls != 0 {
		t.Errorf("PostMessage called %d times for empty text, want 0", api.calls)
	}
}

func TestThreadMessageForward_DedupesByBeadID(t *testing.T) {
	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Logger: slog.Default()})
	data := newThreadSayBead(t, "gd-dup", map[string]string{
		"text":                 "once",
		"slack_thread_channel": "C123",
		"slack_thread_ts":      "1.2",
	})
	fwd.handleClosed(context.Background(), data)
	fwd.handleClosed(context.Background(), data) // SSE reconnect replay
	if api.calls != 1 {
		t.Errorf("PostMessage calls = %d, want 1 (dedup on bead id)", api.calls)
	}
}

func TestThreadMessageForward_PostErrorDoesNotPanic(t *testing.T) {
	api := &capturingThreadSlack{err: errors.New("slack down")}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Logger: slog.Default()})
	data := newThreadSayBead(t, "gd-err", map[string]string{
		"text":                 "x",
		"slack_thread_channel": "C123",
		"slack_thread_ts":      "1.2",
	})
	fwd.handleClosed(context.Background(), data) // must not panic
	if api.calls != 1 {
		t.Errorf("PostMessage calls = %d, want 1", api.calls)
	}
}

func TestNewThreadMessageForward_NilRouterReturnsNil(t *testing.T) {
	if got := NewThreadMessageForward(ThreadMessageForwardConfig{Router: nil}); got != nil {
		t.Error("expected nil watcher when the router is nil")
	}
}

func TestThreadMessageForward_RegisterHandlers(t *testing.T) {
	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Logger: slog.Default()})
	stream := NewSSEStream(SSEStreamConfig{BeadsHTTPAddr: "http://x", Logger: slog.Default()})
	fwd.RegisterHandlers(stream) // must not panic; registers the closed handler
}

// A drained limiter (zero rate, depleted burst) makes Wait fail under a
// cancelled context, so the forwarder drops the message rather than posting —
// proving composer traffic is actually gated by the shared slackCardLimiter.
func TestThreadMessageForward_RateLimiterGatesPost(t *testing.T) {
	api := &capturingThreadSlack{}
	limiter := rate.NewLimiter(0, 0) // never admits
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Limiter: limiter, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Wait returns immediately with the context error
	fwd.handleClosed(ctx, newThreadSayBead(t, "gd-rl", map[string]string{
		"text":                 "blocked by limiter",
		"slack_thread_channel": "C123",
		"slack_thread_ts":      "1.2",
	}))
	if api.calls != 0 {
		t.Errorf("PostMessage calls = %d, want 0 (limiter must gate the post)", api.calls)
	}
}

// A limiter with headroom admits the post — the gate is a budget, not a block.
func TestThreadMessageForward_RateLimiterAdmitsWhenAvailable(t *testing.T) {
	api := &capturingThreadSlack{}
	limiter := rate.NewLimiter(rate.Inf, 1)
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Limiter: limiter, Logger: slog.Default()})
	fwd.handleClosed(context.Background(), newThreadSayBead(t, "gd-ok", map[string]string{
		"text":                 "admitted",
		"slack_thread_channel": "C123",
		"slack_thread_ts":      "1.2",
	}))
	if api.calls != 1 {
		t.Errorf("PostMessage calls = %d, want 1 (limiter with headroom admits)", api.calls)
	}
}

// A nil watcher's methods must be safe no-ops (parity with NewNotice→nil).
func TestThreadMessageForward_NilReceiverSafe(t *testing.T) {
	var fwd *ThreadMessageForward
	fwd.RegisterHandlers(nil)                            // no panic
	fwd.handleClosed(context.Background(), []byte(`{}`)) // no panic
}

// SlackCardLimiter exposes the bot's shared limiter so forwarders reuse it
// rather than standing up a second, uncoordinated bucket.
func TestBot_SlackCardLimiter_ReturnsSharedLimiter(t *testing.T) {
	lim := rate.NewLimiter(rate.Limit(1), 1)
	b := &Bot{slackCardLimiter: lim}
	if b.SlackCardLimiter() != lim {
		t.Error("SlackCardLimiter did not return the bot's shared limiter")
	}
}

// --- Viewer-reply spawn dispatch (ka-0000005407) ---
//
// After the forwarder relays a viewer `thread-say` message into Slack it must
// give that reply the SAME agent outcome an @gardenio mention would: spawn a
// fresh thread agent when none is bound, respawn a dead one with session resume,
// or nudge a live one. These tests drive handleClosed end-to-end with a real
// *Bot wired to the shared mock daemon + fake Slack/controller servers, so they
// exercise the actual spawn/respawn/nudge paths, not a stub.

// TestViewerReply_NoBoundAgent_Spawns: a viewer reply to a thread with no bound
// agent cold-spawns a fresh thread-<ts> agent, exactly as a first @gardenio in a
// new thread does.
func TestViewerReply_NoBoundAgent_Spawns(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-VIEW", "thread")

	dir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = sm
	b.botUserID = "U-BOT"

	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Bot: b, Logger: slog.Default()})

	fwd.handleClosed(context.Background(), newThreadSayBeadIn(t, daemon, "gd-vr1", "monorepo", map[string]string{
		"text":                 "hey can you look at this",
		"slack_thread_channel": "C-VIEW",
		"slack_thread_ts":      "1700000000.000100",
		"thread_id":            "gd-wish-vr1",
	}))

	if api.calls != 1 {
		t.Fatalf("Slack forward calls = %d, want 1 (message must still relay)", api.calls)
	}
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 spawned agent bead, got %d", len(agentBeads))
	}
	got := agentBeads[0]
	wantName := "thread-" + sanitizeTS("1700000000.000100")
	if got.Fields["agent"] != wantName {
		t.Errorf("spawned agent name = %q, want %q (thread-<ts>)", got.Fields["agent"], wantName)
	}
	if got.Fields["project"] != "monorepo" {
		t.Errorf("spawned agent project = %q, want monorepo", got.Fields["project"])
	}
	if got.Fields["slack_thread_channel"] != "C-VIEW" {
		t.Errorf("spawned agent slack_thread_channel = %q, want C-VIEW", got.Fields["slack_thread_channel"])
	}
}

// TestViewerReply_AgentWritten_NoSpawn pins gd-vnVsKJo4zw on the composer
// path: a thread-say bead an agent wrote, with an author field naming a human,
// spawns no agent in that human's name.
func TestViewerReply_AgentWritten_NoSpawn(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-VIEW", "thread")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(daemon, slackSrv)
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.state = sm
	b.botUserID = "U-BOT"
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(&capturingThreadSlack{}), Bot: b, Logger: slog.Default()})

	fwd.handleClosed(context.Background(), newThreadSayBeadBy(t, "gd-vr-forged", "gardenio-crew-x", map[string]string{
		"text":                 "do the thing",
		"author":               "oidc:ada@example.com",
		"slack_thread_channel": "C-VIEW",
		"slack_thread_ts":      "1700000000.000150",
		"thread_id":            "gd-wish-forged",
	}))

	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Fatalf("%d agent beads spawned off an agent-written thread-say message, want 0", n)
	}
}

// TestViewerReply_TerminalAgent_Respawns: a viewer reply to a thread whose bound
// agent is in a terminal state respawns the same-named agent with session
// resume, mirroring handleAppMention's terminal-state branch.
func TestViewerReply_TerminalAgent_Respawns(t *testing.T) {
	for _, state := range []string{"done", "failed"} {
		t.Run(state, func(t *testing.T) {
			daemon := newMockDaemon()
			daemon.seedProjectWithChannelRole("monorepo", "C-VIEW2", "thread")

			const agentName = "thread-1700000000-000200"
			daemon.mu.Lock()
			daemon.beads[agentName] = &beadsapi.BeadDetail{
				ID:     "bd-agent-term-vr",
				Title:  agentName,
				Type:   "agent",
				Status: "open",
				Fields: map[string]string{
					"agent":       agentName,
					"project":     "monorepo",
					"agent_state": state,
				},
				Notes: "backend: coop\ncoop_url: http://127.0.0.1:9999",
			}
			daemon.mu.Unlock()

			dir := t.TempDir()
			sm, err := NewStateManager(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := sm.SetThreadAgent("C-VIEW2", "1700000000.000200", agentName); err != nil {
				t.Fatal(err)
			}

			slackSrv := newFakeSlackServer(t)
			defer slackSrv.Close()

			var gotRole string
			var mu sync.Mutex
			var called bool
			ctrlSrv := fakeControllerForAssign(t, false, &gotRole, &mu, &called)
			defer ctrlSrv.Close()

			b := newTestBot(daemon, slackSrv)
			b.state = sm
			b.botUserID = "U-BOT"
			b.controllerURL = ctrlSrv.URL

			api := &capturingThreadSlack{}
			fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Bot: b, Logger: slog.Default()})

			fwd.handleClosed(context.Background(), newThreadSayBeadIn(t, daemon, "gd-vr2", "monorepo", map[string]string{
				"text":                 "are you still on this?",
				"slack_thread_channel": "C-VIEW2",
				"slack_thread_ts":      "1700000000.000200",
				"thread_id":            "gd-wish-vr2",
			}))

			// A cold-resume respawn mints exactly one fresh agent bead carrying the
			// resume spawn_source under the SAME name — proof we respawned rather
			// than just forwarding.
			var resumeBeads []*beadsapi.BeadDetail
			for _, bead := range filterAgentBeads(daemon.beads) {
				if bead.Fields["spawn_source"] == "slack-thread-resume" {
					resumeBeads = append(resumeBeads, bead)
				}
			}
			if len(resumeBeads) != 1 {
				t.Fatalf("expected 1 cold-resume agent bead, got %d", len(resumeBeads))
			}
			if resumeBeads[0].Fields["agent"] != agentName {
				t.Errorf("resume bead agent = %q, want %q (same name for session continuity)",
					resumeBeads[0].Fields["agent"], agentName)
			}
		})
	}
}

// TestViewerReply_ContractBeadNudgesSameLandAgent feeds the thread-say bead
// the beads-viewer composer writes (testdata/viewer_thread_say.json, which
// cmd/beads-viewer TestThreadReply_WritesTheBridgeContractBead pins to the
// viewer's real output) to the forwarder: it is relayed, and it nudges the
// live agent of its head's land (gd-oG21yWzoKL). With its project label
// stripped it reaches none.
func TestViewerReply_ContractBeadNudgesSameLandAgent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "viewer_thread_say.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Head struct {
			ID     string            `json:"id"`
			Labels []string          `json:"labels"`
			Fields map[string]string `json:"fields"`
		} `json:"head"`
		Operator string `json:"operator"`
		Message  struct {
			Labels []string          `json:"labels"`
			Fields map[string]string `json:"fields"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	land := ""
	for _, l := range c.Head.Labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok {
			land = v
		}
	}
	channel, ts := c.Head.Fields["slack_thread_channel"], c.Head.Fields["slack_thread_ts"]

	for _, tc := range []struct {
		name      string
		labels    []string
		wantNudge bool
	}{
		{"as the viewer writes it", c.Message.Labels, true},
		{"without its land", slices.DeleteFunc(slices.Clone(c.Message.Labels), func(l string) bool { return strings.HasPrefix(l, "project:") }), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			twin := surfacetest.NewSlackTwin()
			coop, nudges := newCoopRecorder(t)
			b, daemon := newInboundReplyBot(t, twin, coop)
			daemon.beads[c.Head.ID] = &beadsapi.BeadDetail{
				ID: c.Head.ID, Type: "task", Status: "open", Title: "a viewer wish", Labels: c.Head.Labels, Fields: c.Head.Fields,
			}
			seedSlackChannelBead(daemon, "gd-ch-view", land, channel)
			daemon.beads["crew-view"] = &beadsapi.BeadDetail{
				ID: "gd-agent-view", Type: "agent", Status: "open", Title: "crew-view",
				Labels: []string{"project:" + land}, Notes: "coop_url: " + coop,
				Fields: map[string]string{"agent": "crew-view", "project": land, "agent_state": "working"},
			}
			_ = b.state.SetThreadAgent(channel, ts, "crew-view")

			fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Bot: b, Logger: slog.Default()})
			fwd.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
				ID: "gd-vmsg", Type: "message", Status: "closed", CreatedBy: c.Operator,
				Labels: tc.labels, Fields: c.Message.Fields,
			}))

			relayed := false
			for _, call := range twin.Calls() {
				if call.Params["channel"] == channel && strings.Contains(call.Params["text"], c.Message.Fields["text"]) {
					relayed = true
				}
			}
			if !relayed {
				t.Errorf("the composer message was not relayed into %s: %v", channel, twin.Calls())
			}
			got := countContaining(nudges(), c.Message.Fields["text"])
			if tc.wantNudge && got == 0 { // the recorder's coop retries a nudge it did not deliver
				t.Errorf("crew-view was not nudged: %v", nudges())
			}
			if !tc.wantNudge && got != 0 {
				t.Errorf("a composer message of no land nudged crew-view %d times", got)
			}
		})
	}
}

// TestViewerReply_NoLand_RelaysOnly: a viewer reply of no land (a thread-say
// with no project label) is still relayed into Slack, but reaches no agent:
// no land holds no agent (gd-cVqdya4Zfo), so it neither nudges the live one
// nor spawns one.
func TestViewerReply_NoLand_RelaysOnly(t *testing.T) {
	var nudgeCount int
	var nmu sync.Mutex
	coopSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			nmu.Lock()
			nudgeCount++
			nmu.Unlock()
		}
		_, _ = w.Write([]byte(`{"delivered":true}`))
	}))
	defer coopSrv.Close()

	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-VIEW4", "thread")
	seedSlackChannelBead(daemon, "gd-ch-view4", "monorepo", "C-VIEW4")
	const agentName = "thread-1700000000-000400"
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID: "bd-agent-noland", Title: agentName, Type: "agent", Status: "open",
		Fields: map[string]string{"agent": agentName, "project": "monorepo", "agent_state": "working"},
		Notes:  "backend: coop\ncoop_url: " + coopSrv.URL,
	}
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = sm.SetThreadAgent("C-VIEW4", "1700000000.000400", agentName)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(daemon, slackSrv)
	b.state = sm
	b.botUserID = "U-BOT"

	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Bot: b, Logger: slog.Default()})
	agentsBefore := len(filterAgentBeads(daemon.beads))
	fwd.handleClosed(context.Background(), newThreadSayBead(t, "gd-vr4", map[string]string{
		"text":                 "no land here",
		"slack_thread_channel": "C-VIEW4",
		"slack_thread_ts":      "1700000000.000400",
		"thread_id":            "gd-wish-vr4",
	}))

	if api.calls != 1 {
		t.Fatalf("Slack forward calls = %d, want 1 (the message still relays)", api.calls)
	}
	nmu.Lock()
	defer nmu.Unlock()
	if nudgeCount != 0 {
		t.Errorf("a reply of no land nudged the agent %d times", nudgeCount)
	}
	if n := len(filterAgentBeads(daemon.beads)); n != agentsBefore {
		t.Errorf("a reply of no land spawned: %d agent beads, was %d", n, agentsBefore)
	}
}

// TestViewerReply_LiveAgent_NudgesNotDoubleBubble: a viewer reply to a thread
// with a LIVE agent nudges it via HTTP and does NOT mint a second conversation
// bead — the viewer's thread-say bead already renders in the /threads lane, so
// recordUnifiedThreadReply would draw a duplicate bubble.
func TestViewerReply_LiveAgent_NudgesNotDoubleBubble(t *testing.T) {
	var nudgeCount int
	var nmu sync.Mutex
	coopSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			nmu.Lock()
			nudgeCount++
			nmu.Unlock()
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"delivered":true}`))
	}))
	defer coopSrv.Close()

	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-VIEW3", "thread")

	const agentName = "thread-1700000000-000300"
	daemon.mu.Lock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     "bd-agent-live-vr",
		Title:  agentName,
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":       agentName,
			"project":     "monorepo",
			"agent_state": "working",
		},
		Notes: "backend: coop\ncoop_url: " + coopSrv.URL,
	}
	daemon.mu.Unlock()
	payload := newThreadSayBeadIn(t, daemon, "gd-vr3", "monorepo", map[string]string{
		"text":                 "one more thing",
		"slack_thread_channel": "C-VIEW3",
		"slack_thread_ts":      "1700000000.000300",
		"thread_id":            "gd-wish-vr3",
	})
	beadsBefore := len(daemon.beads)

	dir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.SetThreadAgent("C-VIEW3", "1700000000.000300", agentName); err != nil {
		t.Fatal(err)
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = sm
	b.botUserID = "U-BOT"

	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Bot: b, Logger: slog.Default()})

	fwd.handleClosed(context.Background(), payload)

	nmu.Lock()
	gotNudges := nudgeCount
	nmu.Unlock()
	if gotNudges != 1 {
		t.Errorf("nudge deliveries = %d, want 1 (live agent must be nudged)", gotNudges)
	}
	// No new bead: a live-agent viewer reply must NOT record a second
	// conversation bubble on top of the viewer's own thread-say bead.
	if len(daemon.beads) != beadsBefore {
		t.Errorf("bead count changed from %d to %d — live-agent reply must not create a duplicate bubble",
			beadsBefore, len(daemon.beads))
	}
	// No respawn bead either — the agent was live.
	for _, bead := range filterAgentBeads(daemon.beads) {
		if bead.Fields["spawn_source"] == "slack-thread-resume" {
			t.Errorf("unexpected respawn bead %q for a live agent", bead.ID)
		}
	}
}

// TestViewerReply_NilBot_ForwardOnly: with no *Bot the forwarder keeps its
// pre-fix behavior — the message relays into Slack, but no agent is spawned.
func TestViewerReply_NilBot_ForwardOnly(t *testing.T) {
	daemon := newMockDaemon()
	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Logger: slog.Default()}) // Bot nil

	fwd.handleClosed(context.Background(), newThreadSayBead(t, "gd-vr4", map[string]string{
		"text":                 "forward only",
		"slack_thread_channel": "C-VIEW4",
		"slack_thread_ts":      "1700000000.000400",
	}))

	if api.calls != 1 {
		t.Fatalf("Slack forward calls = %d, want 1", api.calls)
	}
	if got := len(filterAgentBeads(daemon.beads)); got != 0 {
		t.Errorf("expected 0 agent beads with nil bot, got %d", got)
	}
}

// TestViewerReply_PostFails_NoSpawn: if the Slack relay fails, the spawn
// dispatch is skipped — we never spawn an agent for a message the operator
// never saw land.
func TestViewerReply_PostFails_NoSpawn(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-VIEW5", "thread")

	dir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = sm
	b.botUserID = "U-BOT"

	api := &capturingThreadSlack{err: errors.New("slack down")}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Bot: b, Logger: slog.Default()})

	fwd.handleClosed(context.Background(), newThreadSayBead(t, "gd-vr5", map[string]string{
		"text":                 "will fail to post",
		"slack_thread_channel": "C-VIEW5",
		"slack_thread_ts":      "1700000000.000500",
	}))

	if got := len(filterAgentBeads(daemon.beads)); got != 0 {
		t.Errorf("expected 0 agent beads when the Slack post failed, got %d", got)
	}
}

// TestSpawnOrRespawnForViewerReply_InvalidBindingNoSpawn: a malformed
// (channel, ts) binding is refused before any spawn — matching handleThreadSpawn.
func TestSpawnOrRespawnForViewerReply_InvalidBindingNoSpawn(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(daemon, slackSrv)
	b.botUserID = "U-BOT"

	// Missing dot in the ts → invalid binding.
	b.SpawnOrRespawnForViewerReply(context.Background(), "C-X", "nodot", "hi", "gd-msg", "")

	if got := len(filterAgentBeads(daemon.beads)); got != 0 {
		t.Errorf("expected 0 agent beads for invalid binding, got %d", got)
	}
}

// TestSpawnOrRespawnForViewerReply_EmptyTextNoSpawn: an empty message is a
// no-op (nothing to act on).
func TestSpawnOrRespawnForViewerReply_EmptyTextNoSpawn(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(daemon, slackSrv)
	b.botUserID = "U-BOT"

	b.SpawnOrRespawnForViewerReply(context.Background(), "C-Y", "1700000000.000600", "   ", "gd-msg", "")

	if got := len(filterAgentBeads(daemon.beads)); got != 0 {
		t.Errorf("expected 0 agent beads for empty text, got %d", got)
	}
}

// TestSpawnOrRespawnForViewerReply_StampsOperatorContext locks in the D1 fix:
// the spawn path's daemon writes must carry an X-Operated-By-shaped operator on
// the context, or the R2-enforcing daemon fail-closes them with HTTP 400. The
// incoming ctx is a bare machine context; the method must stamp the operator
// (oidc:<email> for a human email) before calling handleThreadSpawn's CreateBead.
func TestSpawnOrRespawnForViewerReply_StampsOperatorContext(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-OP", "thread")

	dir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = sm
	b.botUserID = "U-BOT"

	// No bound agent → cold-spawn, which calls daemon.CreateBead on the stamped
	// ctx. Operator is a bare email, as the viewer stamps on CreatedBy.
	b.SpawnOrRespawnForViewerReply(context.Background(), "C-OP", "1700000000.000700",
		"spawn me", "gd-msg-op", "matthew.baker@acme.example")

	if daemon.lastCreateBeadCtx == nil {
		t.Fatal("expected a CreateBead call on the spawn path")
	}
	if got := beadsapi.OperatorFromContext(daemon.lastCreateBeadCtx); got != "oidc:matthew.baker@acme.example" {
		t.Errorf("CreateBead operator = %q, want oidc:matthew.baker@acme.example (R2 fail-close guard)", got)
	}
}

// TestSpawnOrRespawnForViewerReply_EmptyOperatorFallsBackToSystem: with no
// operator email, the writes still carry a system-operator source so they don't
// fail-close.
func TestSpawnOrRespawnForViewerReply_EmptyOperatorFallsBackToSystem(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-OP2", "thread")

	dir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = sm
	b.botUserID = "U-BOT"

	b.SpawnOrRespawnForViewerReply(context.Background(), "C-OP2", "1700000000.000800",
		"spawn me", "gd-msg-op2", "")

	if daemon.lastCreateBeadCtx == nil {
		t.Fatal("expected a CreateBead call on the spawn path")
	}
	if got := beadsapi.SystemOperatorFromContext(daemon.lastCreateBeadCtx); got != "slack-bridge" {
		t.Errorf("CreateBead system operator = %q, want slack-bridge", got)
	}
}

// The Commons pane writes a reply already closed (a person may create a message
// bead, not close one), so its only event is the create: the forwarder posts it
// from there, once, and leaves an open create to its close.
func TestThreadMessageForward_ForwardsAMessageCreatedClosed(t *testing.T) {
	api := &capturingThreadSlack{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: threadRouter(api), Logger: slog.Default()})
	event := func(id, status string) []byte {
		out, err := json.Marshal(map[string]any{"bead": map[string]any{
			"id": id, "type": "message", "status": status, "labels": []string{ThreadSayLabel},
			"fields": map[string]string{"text": "from the commons", "slack_thread_channel": "C123", "slack_thread_ts": "1700000000.000100", "thread_id": "gd-wish1"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	fwd.handleCreated(context.Background(), event("gd-open", "open"))
	if api.calls != 0 {
		t.Fatalf("an open create was forwarded (%d posts); its close forwards it", api.calls)
	}
	fwd.handleCreated(context.Background(), event("gd-cm1", "closed"))
	if api.calls != 1 || api.threadTS != "1700000000.000100" || api.text != "from the commons" {
		t.Fatalf("closed create: calls=%d thread_ts=%q text=%q", api.calls, api.threadTS, api.text)
	}
	fwd.handleClosed(context.Background(), event("gd-cm1", "closed"))
	if api.calls != 1 {
		t.Errorf("a later close of the same bead re-posted it (%d posts)", api.calls)
	}
}
