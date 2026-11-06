package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack"
)

var updateLeafOutboundGolden = flag.Bool("update-leaf-outbound", false,
	"rewrite testdata/leaf_outbound_calls.golden.json from the leaf handlers' recorded calls")

// newLeafBot returns a test Bot whose Slack client talks to twin, with one
// thread-bound agent (crew-thread), one card-threaded agent (crew-card) and a
// vibe-mode agent (crew-vibe, thread-bound too).
func newLeafBot(t *testing.T, twin *surfacetest.SlackTwin) (*Bot, *mockDaemon) {
	t.Helper()
	srv := newFakeSlackServer(t)
	t.Cleanup(srv.Close)
	daemon := newMockDaemon()
	b := newTestBot(daemon, srv)
	b.api = slack.New("xoxb-test", slack.OptionAPIURL(twin.Serve(t)))
	b.channel = "C0OPS"
	b.beadsViewerURL = "https://beads.test"

	daemon.beads["crew-thread"] = &beadsapi.BeadDetail{
		ID: "gd-agent-thread", Type: "agent", Status: "open", Title: "crew-thread",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"agent": "crew-thread", "project": "gardenio",
			"slack_thread_channel": "C0THREAD", "slack_thread_ts": "1690000000.000100",
		},
	}
	enableVibeForAgent(daemon, "crew-vibe")
	vibe := daemon.beads["crew-vibe"]
	vibe.Labels = []string{"project:gardenio"}
	vibe.Fields["project"] = "gardenio"
	vibe.Fields["slack_thread_channel"] = "C0THREAD"
	vibe.Fields["slack_thread_ts"] = "1690000000.000200"

	b.threadingMode = "agent"
	b.agentCards["crew-card"] = MessageRef{ChannelID: "C0CREW", Timestamp: "1690000000.000300"}
	b.agentSpawnChannel["crew-card"] = "C0CREW"
	b.agentProject["crew-card"] = "gardenio"
	return b, daemon
}

// TestLeafOutboundCallsGolden pins the Slack Web API calls of the slice-1 leaf
// handlers (design spec 2026-10-19-lands-commons-design.md, section 5.2): the
// squawk projector, comment relay, chat-close relay, thread message forward,
// death report and crash notice. The golden was recorded from the direct-call
// code before those handlers moved behind the conversation Router; the routed
// code must send byte-identical calls (no Slack-visible change). One
// deliberate change since: the terse death and crash notices send Detail as
// their only text param instead of the fallback then Detail (gd-aMglRLWRfn).
// Regenerate with: go test ./internal/bridge -run TestLeafOutboundCallsGolden -update-leaf-outbound
func TestLeafOutboundCallsGolden(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newLeafBot(t, twin)
	ctx := context.Background()
	logger := slog.Default()

	// Squawk: the first squawk of a thread agent edits its spawn message in
	// place; later ones reply in the bound thread; a card-threaded agent's
	// squawk replies under its card.
	b.threadSpawnMsgs["crew-thread"] = MessageRef{ChannelID: "C0THREAD", Timestamp: "1690000000.000101", ThreadTS: "1690000000.000100"}
	squawk := NewSquawk(SquawkConfig{Daemon: daemon, Bot: b, Logger: logger})
	for _, ev := range []BeadEvent{
		{ID: "gd-sq1", Type: "message", Title: "spawned and reading", Labels: []string{"squawk", "from:crew-thread", "project:gardenio"}},
		{ID: "gd-sq2", Type: "message", Title: "t", Labels: []string{"say", "project:gardenio"}, Fields: map[string]string{"source_agent": "crew-thread", "text": "second squawk"}},
		{ID: "gd-sq3", Type: "message", Title: "card squawk", Labels: []string{"squawk", "from:crew-card"}},
	} {
		squawk.handleClosed(ctx, marshalSSEBeadPayload(ev))
	}
	if _, ok := b.threadSpawnMsgs["crew-thread"]; ok {
		t.Fatal("first squawk did not consume the spawn message")
	}

	// Comment relay (vibe mode only): an agent comment and a human comment.
	daemon.beads["gd-task1"] = &beadsapi.BeadDetail{ID: "gd-task1", Type: "task", Assignee: "crew-vibe", Labels: []string{"project:gardenio"}}
	cf := NewCommentForward(CommentForwardConfig{Daemon: daemon, Bot: b, Logger: logger})
	for _, c := range []sseComment{
		{ID: 1, BeadID: "gd-task1", Author: "crew-vibe", Text: "audit: all green"},
		{ID: 2, BeadID: "gd-task1", Author: "oidc:ada@example.com", OperatedByDisplay: "Ada", Text: "please also check the chart"},
	} {
		data, err := json.Marshal(sseCommentPayload{Comment: &c})
		if err != nil {
			t.Fatal(err)
		}
		cf.handleCommentAdded(ctx, data)
	}

	// Chat-close relay: the close reason goes back into the chat's thread.
	daemon.beads["gd-chat1"] = &beadsapi.BeadDetail{
		ID: "gd-chat1", Type: "task", Assignee: "crew-thread", Labels: []string{"slack-chat", "project:gardenio"},
		Fields: map[string]string{"description": "asked in [slack:C0CHAT:1690000000.000400]", "close_reason": "done: the chart lints"},
	}
	chat := &Chat{daemon: daemon, bot: b, logger: logger}
	chat.handleClosed(ctx, marshalSSEBeadPayload(BeadEvent{ID: "gd-chat1", Type: "task", Assignee: "crew-thread", Labels: []string{"slack-chat", "project:gardenio"}}))

	// Thread message forward: the /threads composer's thread-say bead.
	leafThreadForward(t, b).handleClosed(ctx, marshalBeadEvent(t, "gd-say1", "message",
		[]string{ThreadSayLabel, "project:gardenio"}, map[string]string{
			"text": "from the viewer composer", "slack_thread_channel": "C0WISH", "slack_thread_ts": "1690000000.000500",
		}))

	// Death report: terse (simple mode), rich (vibe), and the storm roll-up.
	dead := func(agent string) BeadEvent {
		return BeadEvent{ID: "gd-dead-" + agent, Type: "agent", Assignee: agent, Labels: []string{"project:gardenio"},
			Fields: map[string]string{"close_reason": "context_cancelled_involuntary"}}
	}
	if err := b.NotifyAgentDeath(ctx, dead("crew-thread")); err != nil {
		t.Fatal(err)
	}
	if err := b.NotifyAgentDeath(ctx, dead("crew-vibe")); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.deathReportWindowStart = time.Now().Add(-2 * deathStormWindow)
	b.deathReportSuppressed = 7
	b.mu.Unlock()
	if err := b.NotifyAgentDeath(ctx, dead("crew-thread")); err != nil {
		t.Fatal(err)
	}

	// Crash notice: terse and rich.
	crash := func(agent string) BeadEvent {
		return BeadEvent{ID: "gd-crash-" + agent, Type: "agent", Assignee: agent, Labels: []string{"project:gardenio"},
			Fields: map[string]string{"agent_state": "failed", "pod_phase": "failed", "pod_name": agent + "-pod"}}
	}
	if err := b.NotifyAgentCrash(ctx, crash("crew-thread")); err != nil {
		t.Fatal(err)
	}
	if err := b.NotifyAgentCrash(ctx, crash("crew-vibe")); err != nil {
		t.Fatal(err)
	}

	got, err := json.MarshalIndent(twin.Calls(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "leaf_outbound_calls.golden.json")
	if *updateLeafOutboundGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Lets the golden be recorded from a CI run on a pod too small to
		// build the bridge: decode the line below into the golden file.
		t.Logf("LEAF_OUTBOUND_GOLDEN_B64:%s", base64.StdEncoding.EncodeToString(got))
		t.Fatalf("%s missing (rerun with -update-leaf-outbound)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("leaf handler Slack calls drifted from %s (rerun with -update-leaf-outbound only if the change is intended):\n%s", path, got)
	}
}

func leafThreadForward(t *testing.T, b *Bot) *ThreadMessageForward {
	t.Helper()
	return NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Logger: slog.Default()})
}
