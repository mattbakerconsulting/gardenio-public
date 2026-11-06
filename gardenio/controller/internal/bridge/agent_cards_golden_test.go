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
	"regexp"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack"
)

// seenClock matches the "seen" time on an agent card's context line.
var seenClock = regexp.MustCompile(`_\d{1,2}:\d{2} [AP]M UTC_`)

var updateAgentCardsGolden = flag.Bool("update-agent-cards", false,
	"rewrite testdata/agent_cards_calls.golden.json from the agent card handlers' recorded calls")

// newAgentCardsBot returns a test Bot whose Slack client talks to twin, with
// three agents in the gardenio land: crew-a (a top-level card), crew-t (a
// card in its wish head's Slack thread) and crew-gone (a closed agent whose
// stale card the prune compresses).
func newAgentCardsBot(t *testing.T, twin *surfacetest.SlackTwin) (*Bot, *mockDaemon) {
	t.Helper()
	srv := newFakeSlackServer(t)
	t.Cleanup(srv.Close)
	daemon := newMockDaemon()
	b := newTestBot(daemon, srv)
	b.api = slack.New("xoxb-test", slack.OptionAPIURL(twin.Serve(t)))
	b.channel = "C0OPS"
	b.threadingMode = "agent"
	b.beadsViewerURL = "https://beads.test"
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.state = state

	daemon.beads["crew-a"] = &beadsapi.BeadDetail{
		ID: "gd-agent-a", Type: "agent", Status: "open", Title: "crew-a",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent": "crew-a", "project": "gardenio", "role": "crew", "agent_state": "working"},
	}
	daemon.beads["crew-t"] = &beadsapi.BeadDetail{
		ID: "gd-agent-t", Type: "agent", Status: "open", Title: "crew-t",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"agent": "crew-t", "project": "gardenio", "role": "crew", "agent_state": "working",
			"slack_thread_bead_id": "gd-head-t",
		},
	}
	daemon.beads["gd-head-t"] = &beadsapi.BeadDetail{
		ID: "gd-head-t", Type: "task", Status: "open", Title: "fix the chart",
		Labels: []string{"thread", "project:gardenio", "slack-thread:C0THREAD:1690000000.000100"},
		Fields: map[string]string{"slack_thread_channel": "C0THREAD", "slack_thread_ts": "1690000000.000100"},
	}
	daemon.beads["crew-gone"] = &beadsapi.BeadDetail{
		ID: "gd-agent-gone", Type: "agent", Status: "closed", Title: "crew-gone",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent": "crew-gone", "project": "gardenio", "role": "crew", "agent_state": "done"},
	}
	for _, agent := range []string{"crew-a", "crew-t", "crew-gone"} {
		b.agentProject[agent] = "gardenio"
		b.agentRole[agent] = "crew"
		b.agentState[agent] = "working"
	}
	return b, daemon
}

// agentCardsScenario drives every agent card notifier through b once: post a
// top-level and an in-thread card, refresh both (the full and the compact
// working face), move a card to another channel (the old one is compressed),
// wrap one up, prune a stale one, and post the spawn, thread state and
// schedule notices.
func agentCardsScenario(t *testing.T, b *Bot) {
	t.Helper()
	ctx := context.Background()

	if _, err := b.ensureAgentCard(ctx, "crew-a", "C0CREW"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ensureAgentCard(ctx, "crew-t", "C0THREAD", "1690000000.000100"); err != nil {
		t.Fatal(err)
	}
	b.flushAgentCard(ctx, "crew-a")
	b.flushAgentCard(ctx, "crew-t")

	// The card's channel changed: the old card is compressed to "moved" and a
	// new one posted.
	if _, err := b.ensureAgentCard(ctx, "crew-a", "C0OTHER"); err != nil {
		t.Fatal(err)
	}

	wrapup := `{"tldr":"Chart lints","accomplishments":"Fixed the chart lint","handoff_notes":"none left"}`
	b.mu.Lock()
	b.agentState["crew-a"] = "done"
	b.mu.Unlock()
	b.replaceAgentCardWithWrapUp(ctx, "crew-a", "done", wrapup)

	// Thread-card terminal: the anchor card is edited in place; a PR-bearing
	// wrap-up adds the one-line ping.
	b.reconcileThreadCardTerminal(ctx, "crew-t", "done", BeadEvent{
		ID: "gd-agent-t", Type: "agent", Assignee: "crew-t",
		Fields: map[string]string{"wrapup": `{"tldr":"merged","accomplishments":"PR up","pull_requests":["https://example.test/pr/1"]}`},
	}, "C0THREAD", "1690000000.000100")

	// A thread agent's terminal reply: the spawn message is deleted and a
	// state reply posted (the non-streaming path).
	b.threadSpawnMsgs["crew-t"] = MessageRef{ChannelID: "C0THREAD", Timestamp: "1690000000.000101", ThreadTS: "1690000000.000100"}
	b.postThreadStateReply(ctx, "crew-t", "failed", BeadEvent{
		ID: "gd-agent-t", Type: "agent", Assignee: "crew-t",
		Fields: map[string]string{"close_reason": "pod evicted"},
	}, "C0THREAD", "1690000000.000100")

	// The prune compresses a stale card (closed agent, no wrap-up).
	b.agentCards["crew-gone"] = MessageRef{ChannelID: "C0CREW", Timestamp: "1690000000.000900", Agent: "crew-gone"}
	b.pruneStaleAgentCards(ctx)

	// Spawn notices: a non-threaded spawn (flat mode), then a scheduled
	// agent with threading (its card, then the notice under it).
	b.threadingMode = ""
	b.NotifyAgentSpawn(ctx, BeadEvent{
		ID: "gd-agent-s", Type: "agent", Assignee: "crew-s", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent": "crew-s", "project": "gardenio", "role": "crew"},
	})
	b.threadingMode = "agent"
	b.agentSpawnChannel["crew-sched"] = "C0SCHED"
	b.NotifyAgentSpawn(ctx, BeadEvent{
		ID: "gd-agent-sched", Type: "agent", Assignee: "crew-sched", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent": "crew-sched", "project": "gardenio", "role": "crew", "schedule_title": "nightly sweep", "schedule_cron": "0 3 * * *"},
	})
	b.postScheduleStateNotification("crew-sched", "nightly sweep", "done", time.Time{}, BeadEvent{
		ID: "gd-agent-sched", Type: "agent", Assignee: "crew-sched",
		Fields: map[string]string{"wrapup": `{"tldr":"swept"}`},
	})
}

// TestAgentCardCallsGolden pins the Slack Web API calls of the slice-2 agent
// card notifiers (design spec 2026-10-19-lands-commons-design.md, section 5.2):
// the card post (top-level and in-thread), the live refresh (full and compact
// faces), the moved-card compress, the wrap-up edit, the stale prune, the
// thread-card ping, the thread state reply and the spawn and schedule notices.
// The golden was recorded from the direct-call code before those notifiers
// moved behind the conversation Router; the routed code must send identical
// calls (no Slack-visible change).
// Regenerate with: go test ./internal/bridge -run TestAgentCardCallsGolden -update-agent-cards
func TestAgentCardCallsGolden(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, _ := newAgentCardsBot(t, twin)
	agentCardsScenario(t, b)

	got, err := json.MarshalIndent(twin.Calls(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	// A card posted by NotifyAgentSpawn shows when the agent was last seen,
	// which is the wall clock.
	got = seenClock.ReplaceAll(got, []byte("_<seen>_"))
	path := filepath.Join("testdata", "agent_cards_calls.golden.json")
	if *updateAgentCardsGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("AGENT_CARDS_GOLDEN_B64:%s", base64.StdEncoding.EncodeToString(got))
		t.Fatalf("%s missing (rerun with -update-agent-cards)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("agent card Slack calls drifted from %s (rerun with -update-agent-cards only if the change is intended):\n%s", path, got)
	}
}
