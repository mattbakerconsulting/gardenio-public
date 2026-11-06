package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

var updateInboundReplyGolden = flag.Bool("update-inbound-reply", false,
	"rewrite testdata/inbound_reply_calls.golden.json from the reply handlers' recorded calls")

// inboundReplyOutcome is what the slice-4c reply handlers did: the Slack Web
// API calls, the agent nudges, the message beads written and the decisions
// closed.
type inboundReplyOutcome struct {
	Slack     []surfacetest.TwinCall `json:"slack"`
	Nudges    []string               `json:"nudges"`
	Messages  []goldenMessageBead    `json:"messages"`
	Decisions []goldenClose          `json:"decisions"`
}

type goldenMessageBead struct {
	ID       string            `json:"id"`
	Assignee string            `json:"assignee,omitempty"`
	Labels   []string          `json:"labels"`
	Fields   map[string]string `json:"fields"`
}

type goldenClose struct {
	ID     string            `json:"id"`
	Fields map[string]string `json:"fields"`
}

// newInboundReplyBot returns a test Bot whose Slack client talks to twin, with
// a live thread-spawned agent (crew-live, nudged through coop), a gone one
// (crew-gone), a card-threaded agent (crew-card), an orphan wish head and an
// open decision.
func newInboundReplyBot(t *testing.T, twin *surfacetest.SlackTwin, coop string) (*Bot, *mockDaemon) {
	t.Helper()
	srv := newFakeSlackServer(t)
	t.Cleanup(srv.Close)
	daemon := newMockDaemon()
	b := newTestBot(daemon, srv)
	b.api = slack.New("xoxb-test", slack.OptionAPIURL(twin.Serve(t)))
	b.botUserID = "U0BOT"
	b.lastThreadNudge = map[string]time.Time{}
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.state = state

	daemon.beads["crew-live"] = &beadsapi.BeadDetail{
		ID: "gd-agent-live", Type: "agent", Status: "open", Title: "crew-live",
		Labels: []string{"project:gardenio"},
		Notes:  "coop_url: " + coop,
		Fields: map[string]string{
			"agent": "crew-live", "project": "gardenio", "agent_state": "working",
			"slack_thread_channel": "C0LIVE", "slack_thread_ts": "1690000000.000100",
		},
	}
	_ = state.SetThreadAgent("C0LIVE", "1690000000.000100", "crew-live")
	_ = state.SetThreadAgent("C0GONE", "1690000000.000200", "crew-gone")
	b.agentCards["crew-card"] = MessageRef{ChannelID: "C0CREW", Timestamp: "1690000000.000300"}

	daemon.beads["gd-wish"] = &beadsapi.BeadDetail{
		ID: "gd-wish", Type: "task", Status: "open", Title: "an old wish",
		Labels: []string{"thread", threadBeadLabel("C0WISH", "1690000000.000500"), "project:gardenio"},
		Fields: map[string]string{"slack_thread_channel": "C0WISH", "slack_thread_ts": "1690000000.000500"},
	}
	daemon.beads["gd-dec1"] = &beadsapi.BeadDetail{
		ID: "gd-dec1", Type: "decision", Status: "open", Title: "ship it?",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{"options": `[{"id":"yes","short":"y","label":"Ship it"},{"id":"no","short":"n","label":"Hold"}]`},
	}
	_ = state.SetDecisionMessage("gd-dec1", MessageRef{ChannelID: "C0DEC", Timestamp: "1690000000.000600"})
	return b, daemon
}

// newCoopRecorder is a coop stand-in that records every nudge body.
func newCoopRecorder(t *testing.T) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.URL.Path+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// TestInboundReplyCallsGolden pins what the slice-4c reply handlers do for a
// Slack thread reply (design spec 2026-10-19-lands-commons-design.md, section
// 5.2): the thread forward to a live agent (bead + nudge, then a throttled
// second reply), a reply to a gone agent (bead + resume hint), a reply in an
// agent card thread (mention hint), a reply in an orphan wish thread (bead
// only), a reply that resolves a decision, and a /threads composer reply
// (projection + nudge). The golden was recorded from the code before these
// handlers moved behind conversation.Inbound and HandleReply; the moved code
// must make identical Slack calls, nudges and bead writes.
// Regenerate with: go test ./internal/bridge -run TestInboundReplyCallsGolden -update-inbound-reply
func TestInboundReplyCallsGolden(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, nudges := newCoopRecorder(t)
	b, daemon := newInboundReplyBot(t, twin, coop)
	ctx := context.Background()

	for _, ev := range []*slackevents.MessageEvent{
		{User: "U0ADA", Channel: "C0LIVE", ThreadTimeStamp: "1690000000.000100", TimeStamp: "1690000000.000101", Text: "please add a test"},
		{User: "U0ADA", Channel: "C0LIVE", ThreadTimeStamp: "1690000000.000100", TimeStamp: "1690000000.000102", Text: "and the docs", SubType: "thread_broadcast"},
		{User: "U0ADA", Channel: "C0GONE", ThreadTimeStamp: "1690000000.000200", TimeStamp: "1690000000.000201", Text: "still there?"},
		{User: "U0ADA", Channel: "C0CREW", ThreadTimeStamp: "1690000000.000300", TimeStamp: "1690000000.000301", Text: "nice card"},
		{User: "U0ADA", Channel: "C0WISH", ThreadTimeStamp: "1690000000.000500", TimeStamp: "1690000000.000501", Text: "reopening this"},
		{User: "U0ADA", Channel: "C0DEC", ThreadTimeStamp: "1690000000.000600", TimeStamp: "1690000000.000601", Text: "Ship it"},
		// Not replies to the reply handlers: an edit, the bot's own post.
		{User: "U0ADA", Channel: "C0LIVE", ThreadTimeStamp: "1690000000.000100", TimeStamp: "1690000000.000103", Text: "edited", SubType: "message_changed"},
		{User: "U0BOT", Channel: "C0LIVE", ThreadTimeStamp: "1690000000.000100", TimeStamp: "1690000000.000104", Text: "bot echo"},
	} {
		b.handleMessageEvent(ctx, ev)
	}

	// A /threads composer reply into the live agent's thread: projected into
	// Slack, then the agent is nudged off the composer's own bead. The Slack
	// thread is in a channel of the message's land (slackChannelOfLand).
	seedSlackChannelBead(daemon, "gd-ch-live", "gardenio", "C0LIVE")
	b.channelBeads = channelBeadCache{}
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Bot: b, Logger: slog.Default()})
	fwd.handleClosed(ctx, marshalSSEBeadPayload(BeadEvent{
		ID: "gd-say1", Type: "message", CreatedBy: "ada@example.com",
		Labels: []string{ThreadSayLabel, "project:gardenio"},
		Fields: map[string]string{
			"text": "from the viewer composer", "thread_id": "gd-head-live",
			"slack_thread_channel": "C0LIVE", "slack_thread_ts": "1690000000.000100",
		},
	}))

	out := inboundReplyOutcome{Slack: twin.Calls(), Nudges: nudges()}
	daemon.mu.Lock()
	for _, bead := range daemon.beads {
		if bead.Type != "message" {
			continue
		}
		fields := map[string]string{}
		for _, k := range []string{"text", "thread_id", "slack_thread_channel", "slack_thread_ts", "slack_user_id", "operated_by", "idempotency_key"} {
			if v := bead.Fields[k]; v != "" {
				fields[k] = v
			}
		}
		labels := append([]string(nil), bead.Labels...)
		sort.Strings(labels)
		out.Messages = append(out.Messages, goldenMessageBead{ID: bead.ID, Assignee: bead.Assignee, Labels: labels, Fields: fields})
	}
	for _, c := range daemon.closed {
		if c.BeadID == "gd-dec1" {
			out.Decisions = append(out.Decisions, goldenClose{ID: c.BeadID, Fields: c.Fields})
		}
	}
	daemon.mu.Unlock()
	sort.Slice(out.Messages, func(i, j int) bool { return out.Messages[i].ID < out.Messages[j].ID })

	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "inbound_reply_calls.golden.json")
	if *updateInboundReplyGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("INBOUND_REPLY_GOLDEN_B64:%s", base64.StdEncoding.EncodeToString(got))
		t.Fatalf("%s missing (rerun with -update-inbound-reply)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("reply handlers drifted from %s (rerun with -update-inbound-reply only if the change is intended):\n%s", path, got)
	}
}
