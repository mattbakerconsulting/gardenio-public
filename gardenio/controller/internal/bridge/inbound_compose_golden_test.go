package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

var updateInboundComposeGolden = flag.Bool("update-inbound-compose", false,
	"rewrite testdata/inbound_compose_calls.golden.json from the mention core's recorded calls")

// goldenBead is a bead the mention core minted, without what differs per run.
type goldenBead struct {
	Type   string            `json:"type"`
	Title  string            `json:"title"`
	Labels []string          `json:"labels"`
	Fields map[string]string `json:"fields"`
}

// inboundComposeOutcome is what the slice-4d mention core did: the Slack Web
// API calls, the beads it minted and the thread bindings it left.
type inboundComposeOutcome struct {
	Slack    []surfacetest.TwinCall `json:"slack"`
	Beads    []goldenBead           `json:"beads"`
	Bindings map[string]string      `json:"bindings"`
}

// slackMention is an app_mention of the bot from Ada in channel, in thread
// threadTS when set.
func slackMention(channel, ts, threadTS, words string) *slackevents.AppMentionEvent {
	return &slackevents.AppMentionEvent{
		Type: "app_mention", User: "U0ADA", Channel: channel,
		TimeStamp: ts, ThreadTimeStamp: threadTS, Text: "<@U0BOT> " + words,
	}
}

// TestInboundComposeCallsGolden pins what the Slack mention core does (design
// spec 2026-10-19-lands-commons-design.md, section 5.2, slice 4d): a top-level
// mention that spawns, a thread mention with an unknown --model (warned, then
// spawned), one with a --harness that is refused, one with an unknown
// --backend (warned, then spawned), and a mention in a thread whose agent has
// stopped (respawned with resume). The golden was recorded from the code
// before mentions moved behind conversation.Inbound{Compose}, HandleCompose and
// the head-keyed thread anchor; the moved code must make identical Slack calls,
// mint identical beads and leave identical bindings.
// Regenerate with: go test ./internal/bridge -run TestInboundComposeCallsGolden -update-inbound-compose
func TestInboundComposeCallsGolden(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	srv := newFakeSlackServer(t)
	t.Cleanup(srv.Close)
	daemon := newMockDaemon()
	daemon.seedUser("Ada", map[string]string{"slack_id": "U0ADA", "email": "ada@example.com"})
	daemon.seedProjectWithChannel("gardenio", "C0PROJ")
	b := newTestBot(daemon, srv)
	b.api = slack.New("xoxb-test", slack.OptionAPIURL(twin.Serve(t)))
	b.botUserID = "U0BOT"
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.state = state

	// A thread whose agent has stopped: bound in state, its bead terminal.
	_ = b.state.SetThreadAgent("C0PROJ", "1690000000.000500", "thread-1690000000-000500")
	daemon.beads["gd-old-agent"] = &beadsapi.BeadDetail{
		ID: "gd-old-agent", Type: "agent", Status: "open", Title: "thread-1690000000-000500",
		Labels: []string{"project:gardenio", "slack-thread"},
		Fields: map[string]string{
			"agent": "thread-1690000000-000500", "project": "gardenio", "agent_state": "done",
			"role": "thread", "slack_thread_channel": "C0PROJ", "slack_thread_ts": "1690000000.000500",
		},
	}
	before := map[string]bool{}
	for id := range daemon.beads {
		before[id] = id != "gd-old-agent" // the respawn revives it in place: keep its outcome
	}

	ctx := context.Background()
	for _, ev := range []*slackevents.AppMentionEvent{
		slackMention("C0PROJ", "1690000000.000100", "", "fix the build"),
		slackMention("C0PROJ", "1690000000.000201", "1690000000.000200", "--model bogus-model look at the logs"),
		slackMention("C0PROJ", "1690000000.000301", "1690000000.000300", "--harness nope look at the logs"),
		slackMention("C0PROJ", "1690000000.000401", "1690000000.000400", "--backend=weird hi"),
		slackMention("C0PROJ", "1690000000.000501", "1690000000.000500", "are you still there?"),
	} {
		b.handleAppMention(ctx, ev)
	}

	out := inboundComposeOutcome{Slack: twin.Calls(), Bindings: b.state.AllThreadAgents()}
	daemon.mu.Lock()
	ids := make([]string, 0, len(daemon.beads))
	for id := range daemon.beads {
		if !before[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := daemon.beads[id]
		g := goldenBead{Type: d.Type, Title: d.Title, Labels: append([]string(nil), d.Labels...), Fields: map[string]string{}}
		sort.Strings(g.Labels)
		for k, v := range d.Fields {
			if k == "idempotency_key" && strings.HasPrefix(v, "thread-respawn:") {
				v = "thread-respawn:<per wake>"
			}
			g.Fields[k] = v
		}
		out.Beads = append(out.Beads, g)
	}
	daemon.mu.Unlock()
	sort.SliceStable(out.Beads, func(i, j int) bool {
		if out.Beads[i].Type != out.Beads[j].Type {
			return out.Beads[i].Type < out.Beads[j].Type
		}
		return out.Beads[i].Title < out.Beads[j].Title
	})

	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "inbound_compose_calls.golden.json")
	if *updateInboundComposeGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("INBOUND_COMPOSE_GOLDEN_B64:%s", base64.StdEncoding.EncodeToString(got))
		t.Fatalf("%s missing (rerun with -update-inbound-compose)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("mention core drifted from %s (rerun with -update-inbound-compose only if the change is intended):\n%s", path, got)
	}
}
