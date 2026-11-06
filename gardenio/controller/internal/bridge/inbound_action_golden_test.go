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
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack"
)

var updateInboundActionGolden = flag.Bool("update-inbound-action", false,
	"rewrite testdata/inbound_action_calls.golden.json from the action handlers' recorded calls")

// inboundActionOutcome is what the slice-4a action and submission handlers
// did: the Slack Web API calls, the coop calls (verdicts and nudges) and the
// beads closed.
type inboundActionOutcome struct {
	Slack  []surfacetest.TwinCall `json:"slack"`
	Coop   []string               `json:"coop"`
	Closed []goldenClose          `json:"closed"`
}

// newInboundActionBot returns a test Bot whose Slack client talks to twin,
// with a carded agent (crew-card), a live agent reachable through coop
// (crew-live), an open permission request from it, and four open decisions.
func newInboundActionBot(t *testing.T, twin *surfacetest.SlackTwin, coop string) (*Bot, *mockDaemon) {
	t.Helper()
	srv := newFakeSlackServer(t)
	t.Cleanup(srv.Close)
	daemon := newMockDaemon()
	daemon.seedUser("Ada", map[string]string{"slack_id": "U0ADA", "email": "ada@example.com"})
	b := newTestBot(daemon, srv)
	b.api = slack.New("xoxb-test", slack.OptionAPIURL(twin.Serve(t)))
	b.botUserID = "U0BOT"
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.state = state

	b.agentCards["crew-card"] = MessageRef{ChannelID: "C0CREW", Timestamp: "1690000000.000300"}
	daemon.beads["crew-live"] = &beadsapi.BeadDetail{
		ID: "gd-agent-live", Type: "agent", Status: "open", Title: "crew-live",
		Labels: []string{"project:gardenio"},
		Notes:  "coop_url: " + coop,
		Fields: map[string]string{"agent": "crew-live", "project": "gardenio", "agent_state": "working"},
	}
	daemon.beads["gd-agent-live"] = daemon.beads["crew-live"]
	daemon.beads["gd-perm1"] = &beadsapi.BeadDetail{
		ID: "gd-perm1", Type: "permission", Status: "open", Title: "Bash",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{"request_id": "req-1", "agent": "crew-live", "agent_bead_id": "gd-agent-live", "tool_name": "Bash"},
	}
	opts := `[{"id":"yes","short":"y","label":"Ship it"},{"id":"no","short":"n","label":"Hold"}]`
	for _, id := range []string{"gd-dec1", "gd-dec2", "gd-dec3", "gd-dec4"} {
		daemon.beads[id] = &beadsapi.BeadDetail{
			ID: id, Type: "decision", Status: "open", Title: "ship it?", Assignee: "crew-live",
			Labels: []string{"project:gardenio"},
			Fields: map[string]string{"options": opts, "question": "Ship it?"},
		}
	}
	return b, daemon
}

// slackClick is a block action from Ada on the message at channel/ts.
func slackClick(channel, ts, threadTS, actionID, value string) slack.InteractionCallback {
	cb := slack.InteractionCallback{
		Type:      slack.InteractionTypeBlockActions,
		User:      slack.User{ID: "U0ADA", Name: "ada"},
		TriggerID: "trig-1",
		Channel:   slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: channel}}},
		Message:   slack.Message{Msg: slack.Msg{Timestamp: ts}},
		Container: slack.Container{ChannelID: channel, MessageTs: ts, ThreadTs: threadTS},
	}
	cb.MessageTs = ts
	cb.ActionCallback.BlockActions = []*slack.BlockAction{{ActionID: actionID, Value: value}}
	return cb
}

// slackSubmit is a modal submission from Ada.
func slackSubmit(callbackID, metadata string, values map[string]map[string]slack.BlockAction) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type: slack.InteractionTypeViewSubmission,
		User: slack.User{ID: "U0ADA", Name: "ada"},
		View: slack.View{CallbackID: callbackID, PrivateMetadata: metadata, State: &slack.ViewState{Values: values}},
	}
}

// TestInboundActionCallsGolden pins what the slice-4a handlers do for Slack
// card buttons and modal submissions (design spec
// 2026-10-19-lands-commons-design.md, section 5.2): Clear on an agent card,
// Allow and Deny on a permission prompt, Dismiss, an option (modal, then its
// submission), Other (modal, then its submission) and Escalate on decision
// cards, Restart outside a thread and a debounced Restart, and Kill. The
// golden was recorded from the code before these handlers moved behind
// conversation.Inbound and HandleAction/HandleSubmit; the moved code must make
// identical Slack calls, coop calls and bead closes.
// Regenerate with: go test ./internal/bridge -run TestInboundActionCallsGolden -update-inbound-action
func TestInboundActionCallsGolden(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, coopCalls := newCoopRecorder(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	ctx := context.Background()

	for _, cb := range []slack.InteractionCallback{
		slackClick("C0CREW", "1690000000.000300", "", "clear_agent", "crew-card"),
		slackClick("C0DEC", "1690000000.000700", "", "permission_allow", "req-1|crew-live|gd-perm1"),
		slackClick("C0DEC", "1690000000.000701", "", "permission_deny", "req-2|crew-live"),
		slackClick("C0DEC", "1690000000.000601", "", "dismiss_decision", "gd-dec1"),
		slackClick("C0DEC", "1690000000.000602", "", "resolve_gd-dec2_1", "gd-dec2:1"),
		slackClick("C0DEC", "1690000000.000603", "", "resolve_other_gd-dec3", "gd-dec3"),
		slackClick("C0DEC", "1690000000.000604", "", "promote_decision", "gd-dec4"),
		slackClick("C0CREW", "1690000000.000300", "", "restart_thread_agent", "crew-live"),
	} {
		b.handleBlockActions(ctx, cb)
	}
	for _, cb := range []slack.InteractionCallback{
		slackSubmit("resolve_decision", "gd-dec2|yes|C0DEC|1690000000.000602|Ship it",
			map[string]map[string]slack.BlockAction{"rationale": {"rationale_input": {Value: "CI is green"}}}),
		slackSubmit("resolve_other", "gd-dec3|C0DEC|1690000000.000603",
			map[string]map[string]slack.BlockAction{"response": {"response_input": {Value: "Ship it on Monday"}}}),
	} {
		b.handleViewSubmission(ctx, cb)
	}

	// A second Restart inside the debounce window is coalesced. Ada owns
	// crew-busy, so the restart passes the owner check (bot_kill_owner.go);
	// the Kill on crew-nobody stops at its lookup. The decision nudges started
	// above are still reading daemon.beads, so the seed takes daemon.mu.
	daemon.mu.Lock()
	daemon.beads["crew-busy"] = &beadsapi.BeadDetail{
		ID: "gd-agent-busy", Type: "agent", Status: "open", Title: "crew-busy",
		OperatedBy: "oidc:ada@example.com",
		Fields:     map[string]string{"agent": "crew-busy"},
	}
	daemon.mu.Unlock()
	b.reopenDebouncer.Allow("crew-busy")
	b.handleBlockActions(ctx, slackClick("C0LIVE", "1690000000.000101", "1690000000.000100", "restart_thread_agent", "crew-busy"))

	// Kill on an agent that is not running is answered at once.
	before := len(twin.Calls())
	b.handleBlockActions(ctx, slackClick("C0CREW", "1690000000.000300", "", "kill_thread_agent", "crew-nobody"))
	deadline := time.Now().Add(10 * time.Second)
	for len(twin.Calls()) < before+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Decision nudges run in goroutines too: wait until coop goes quiet.
	for n, quiet := -1, 0; quiet < 30 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if c := len(coopCalls()); c == n {
			quiet++
		} else {
			n, quiet = c, 0
		}
	}

	out := inboundActionOutcome{Slack: twin.Calls(), Coop: coopCalls()}
	sort.Strings(out.Coop)
	daemon.mu.Lock()
	for _, c := range daemon.closed {
		out.Closed = append(out.Closed, goldenClose{ID: c.BeadID, Fields: c.Fields})
	}
	daemon.mu.Unlock()
	sort.SliceStable(out.Closed, func(i, j int) bool { return out.Closed[i].ID < out.Closed[j].ID })

	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "inbound_action_calls.golden.json")
	if *updateInboundActionGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("INBOUND_ACTION_GOLDEN_B64:%s", base64.StdEncoding.EncodeToString(got))
		t.Fatalf("%s missing (rerun with -update-inbound-action)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("action handlers drifted from %s (rerun with -update-inbound-action only if the change is intended):\n%s", path, got)
	}
}
