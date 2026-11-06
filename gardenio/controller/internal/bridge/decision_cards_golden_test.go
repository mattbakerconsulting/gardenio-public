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
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack"
)

var updateDecisionCardsGolden = flag.Bool("update-decision-cards", false,
	"rewrite testdata/decision_cards_calls.golden.json from the decision and permission card handlers' recorded calls")

// newDecisionCardsBot returns a test Bot whose Slack client talks to twin,
// with three agents in the gardenio land: crew-x (cards in #crew), crew-t
// (bound to a Slack thread) and crew-o (acting for a person, so its decisions
// go DM-first).
func newDecisionCardsBot(t *testing.T, twin *surfacetest.SlackTwin) (*Bot, *mockDaemon) {
	t.Helper()
	srv := newFakeSlackServer(t)
	t.Cleanup(srv.Close)
	daemon := newMockDaemon()
	b := newTestBot(daemon, srv)
	b.api = slack.New("xoxb-test", slack.OptionAPIURL(twin.Serve(t)))
	b.channel = "C0OPS"
	b.beadsViewerURL = "https://beads.test"
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.state = state

	agent := func(name, id string, extra map[string]string) {
		f := map[string]string{"agent": name, "project": "gardenio", "role": "crew", "agent_state": "working"}
		for k, v := range extra {
			f[k] = v
		}
		daemon.beads[name] = &beadsapi.BeadDetail{ID: id, Type: "agent", Status: "open", Title: name, Labels: []string{"project:gardenio"}, Fields: f}
		daemon.beads[id] = daemon.beads[name]
		b.agentProject[name] = "gardenio"
		b.agentRole[name] = "crew"
		b.agentSpawnChannel[name] = "C0CREW"
	}
	// The person who answers the permission prompt (permissionAnswerer).
	daemon.seedUser("Matt", map[string]string{"slack_id": "UMATT", "email": "matt@example.com"})
	agent("crew-x", "gd-agent-x", nil)
	agent("crew-t", "gd-agent-t", map[string]string{
		"slack_thread_channel": "C0THREAD", "slack_thread_ts": "1690000000.000100", "slack_thread_bead_id": "gd-head-t",
	})
	agent("crew-o", "gd-agent-o", map[string]string{"slack_user_id": "UOWNER"})
	daemon.beads["gd-head-t"] = &beadsapi.BeadDetail{
		ID: "gd-head-t", Type: "task", Status: "open", Title: "fix the chart",
		Labels: []string{"thread", "project:gardenio", "slack-thread:C0THREAD:1690000000.000100"},
		Fields: map[string]string{"slack_thread_channel": "C0THREAD", "slack_thread_ts": "1690000000.000100"},
	}
	return b, daemon
}

// decisionCardEvent is a decision of the gardenio land asked by agent.
func decisionCardEvent(daemon *mockDaemon, id, agent, agentBead string, priority int) BeadEvent {
	ev := BeadEvent{
		ID: id, Type: "decision", Title: "Ship the chart fix?", Status: "open", Priority: priority,
		Assignee: agent, Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt":                   "Ship the chart fix?",
			"context":                  "The values schema is fixed; CI is green.",
			"options":                  `[{"id":"ship","short":"Ship it","label":"Merge and release","description":"Merge PR #42 and cut a release"},{"id":"wait","short":"Wait","artifact_type":"report"}]`,
			"requesting_agent_bead_id": agentBead,
		},
	}
	daemon.beads[id] = &beadsapi.BeadDetail{
		ID: id, Type: "decision", Status: "open", Title: ev.Title, Priority: priority,
		Assignee: agent, Labels: ev.Labels, Fields: ev.Fields,
	}
	return ev
}

// decisionCardsScenario drives every decision and permission card handler
// through b once: a decision posted to the agent's channel, one in the agent's
// Slack thread, one delivered DM-first (the card in the DM, the pointer in the
// channel), the resolved edit of the first (primary and mirrors), the dismiss
// of the second, and a permission prompt and its verdict.
func decisionCardsScenario(t *testing.T, b *Bot, daemon *mockDaemon, twin *surfacetest.SlackTwin) {
	t.Helper()
	ctx := context.Background()

	for _, ev := range []BeadEvent{
		decisionCardEvent(daemon, "gd-dec-x", "crew-x", "gd-agent-x", 1),
		decisionCardEvent(daemon, "gd-dec-t", "crew-t", "gd-agent-t", 2),
		decisionCardEvent(daemon, "gd-dec-o", "crew-o", "gd-agent-o", 0),
	} {
		if err := b.NotifyDecision(ctx, ev); err != nil {
			t.Fatalf("NotifyDecision %s: %v", ev.ID, err)
		}
	}
	if err := b.UpdateDecision(ctx, "gd-dec-x", "ship", "CI is green"); err != nil {
		t.Fatal(err)
	}
	if err := b.UpdateDecision(ctx, "gd-dec-o", "wait", "after the release"); err != nil {
		t.Fatal(err)
	}
	if err := b.DismissDecision(ctx, "gd-dec-t"); err != nil {
		t.Fatal(err)
	}

	h := NewPermissionCallbackHandler(b, b.logger)
	h.postPermissionPrompt(ctx, channelPermissionCallback{
		Type: "permission_request", RequestID: "abcde", ToolName: "Bash",
		Description: "Run the chart tests", InputPreview: "make test-helm", Agent: "crew-x",
	})
	// The verdict edits the prompt the click came from: crew-x's prompt is the
	// newest message in #crew.
	var at slack.InteractionCallback
	msgs := twin.Messages("C0CREW")
	if len(msgs) == 0 {
		t.Fatal("no permission prompt posted to C0CREW")
	}
	at.Channel.ID, at.Message.Timestamp, at.User.ID, at.User.Name = "C0CREW", msgs[len(msgs)-1][0], "UMATT", "matt"
	b.resolvePermission(ctx, permissionButtonValue(t, twin, "C0CREW"), "allow", at)
}

// TestDecisionCardCallsGolden pins the Slack Web API calls of the slice-3
// decision and permission handlers (design spec
// 2026-10-19-lands-commons-design.md, section 5.2): the decision card in a
// channel and in an agent's thread, DM-first delivery (the DM card and the
// channel pointer), the resolved edit of the card and its mirrors, the
// dismiss, and the permission prompt and verdict. The golden was recorded from
// the direct-call code before those handlers moved behind the conversation
// Router; the routed code must send identical calls (no Slack-visible change).
// Regenerate with: go test ./internal/bridge -run TestDecisionCardCallsGolden -update-decision-cards
func TestDecisionCardCallsGolden(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newDecisionCardsBot(t, twin)
	decisionCardsScenario(t, b, daemon, twin)

	got, err := json.MarshalIndent(twin.Calls(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "decision_cards_calls.golden.json")
	if *updateDecisionCardsGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("DECISION_CARDS_GOLDEN_B64:%s", base64.StdEncoding.EncodeToString(got))
		t.Fatalf("%s missing (rerun with -update-decision-cards)", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("decision card Slack calls drifted from %s (rerun with -update-decision-cards only if the change is intended):\n%s", path, got)
	}
}

// TestDecisionCardBeads checks the dual-write behind the golden: every
// decision and permission card is a card bead of its subject in the gardenio
// land, carrying the Slack copy's ref, and the verdict closes the permission
// bead.
func TestDecisionCardBeads(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newDecisionCardsBot(t, twin)
	decisionCardsScenario(t, b, daemon, twin)

	type card struct{ channel, ts, thread string }
	cards := map[string][]card{}
	var perm *beadsapi.BeadDetail
	for _, bd := range daemon.beads {
		switch {
		case bd.Type == "message" && bd.Fields["kind"] == "card":
			if got := projectFromLabels(bd.Labels); got != "gardenio" {
				t.Errorf("card %s has land %q, want gardenio", bd.ID, got)
			}
			cards[bd.Fields["subject_id"]] = append(cards[bd.Fields["subject_id"]], card{bd.Fields["slack_channel"], bd.Fields["slack_ts"], bd.Fields["thread_id"]})
		case bd.Type == "permission":
			perm = bd
		}
	}
	for _, c := range append(append([]card{}, cards["gd-dec-x"]...), cards["gd-dec-t"]...) {
		if c.ts == "" {
			t.Errorf("decision card without a Slack ref: %+v", c)
		}
	}
	if got := cards["gd-dec-x"]; len(got) != 1 || got[0].channel != "C0CREW" {
		t.Errorf("gd-dec-x cards = %+v, want one in C0CREW", got)
	}
	if got := cards["gd-dec-t"]; len(got) != 1 || got[0].channel != "C0THREAD" || got[0].thread != "gd-head-t" {
		t.Errorf("gd-dec-t cards = %+v, want one in C0THREAD under the wish head gd-head-t", got)
	}
	// DM-first: the DM copy and the channel pointer are two cards of one
	// decision.
	chans := map[string]bool{}
	for _, c := range cards["gd-dec-o"] {
		chans[c.channel] = c.ts != ""
	}
	if len(cards["gd-dec-o"]) != 2 || !chans["DUOWNER"] || !chans["C0CREW"] {
		t.Errorf("gd-dec-o cards = %+v, want the DM copy (DUOWNER) and the pointer (C0CREW)", cards["gd-dec-o"])
	}

	if perm == nil {
		t.Fatal("no permission bead written")
	}
	if perm.Fields["tool_name"] != "Bash" || perm.Fields["request_id"] != "abcde" || perm.Fields["agent_bead_id"] != "gd-agent-x" {
		t.Errorf("permission bead fields = %v", perm.Fields)
	}
	if got := cards[perm.ID]; len(got) != 1 || got[0].channel != "C0CREW" {
		t.Errorf("permission cards = %+v, want one in C0CREW", got)
	}
	closed := false
	for _, c := range daemon.getClosed() {
		if c.BeadID == perm.ID {
			closed = c.Fields["behavior"] == "allow" && c.Fields["answered_by"] == "oidc:matt@example.com" && c.Fields["answered_by_slack_id"] == "UMATT"
		}
	}
	if !closed {
		t.Errorf("permission bead %s not closed with the verdict: %+v", perm.ID, daemon.getClosed())
	}
}
