package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack/slackevents"
)

// seedDMPerson puts Ada in the user directory: her email, Slack id and name.
func seedDMPerson(d *mockDaemon) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.beads["gd-user-ada"] = &beadsapi.BeadDetail{
		ID: "gd-user-ada", Type: "user", Kind: "config", Status: "open", Title: "Ada",
		Fields: map[string]string{"email": "ada@example.com", "slack_id": "U0ADA", "display_name": "Ada"},
	}
}

func postSayJSON(t *testing.T, h http.Handler, body map[string]any) (*httptest.ResponseRecorder, sayResponse) {
	t.Helper()
	raw, _ := json.Marshal(body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/conversation/say", bytes.NewReader(raw)))
	var resp sayResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr, resp
}

// beadsOfType returns d's beads of type typ.
func beadsOfType(d *mockDaemon, typ string) []*beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if b.Type == typ {
			out = append(out, b)
		}
	}
	return out
}

// TestSayTo_PersonAskWritesTheCommonsDMAndSendsTheSlackIM: gb say --to
// <person> --ask writes a kind=ask message bead in the DM channel between the
// agent and the person, made on first use, and still sends the Slack IM gb dm
// ask sends (Slack stays the primary delivery), whose ref is stamped on the
// bead so a reply in its thread finds the ask.
func TestSayTo_PersonAskWritesTheCommonsDMAndSendsTheSlackIM(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	seedDMPerson(daemon)

	rr, resp := postSayJSON(t, a.mux(), map[string]any{"agent": "crew-say", "to": "ada@example.com", "ask": true, "text": "ship it?"})
	if rr.Code != http.StatusOK {
		t.Fatalf("say --to: %d %s", rr.Code, rr.Body)
	}
	if resp.ID == "" || resp.DM == "" || resp.Channel == "" || resp.Timestamp == "" {
		t.Fatalf("response = %+v, want the bead, the DM channel and the Slack IM ref", resp)
	}

	channels := beadsOfType(daemon, "channel")
	var dm *beadsapi.BeadDetail
	for _, c := range channels {
		if c.ID == resp.DM {
			dm = c
		}
	}
	if dm == nil || dm.Fields["channel_kind"] != "dm" || !strings.HasPrefix(dm.Fields["name"], "dm-") ||
		dm.Title != "crew-say, Ada" || projectFromLabels(dm.Labels) != "gardenio" {
		t.Fatalf("DM channel = %+v", dm)
	}
	// The mock keeps a non-string field as fmt's %v of it.
	members := dm.Fields["members"]
	if !strings.Contains(members, "agent:crew-say") || !strings.Contains(members, "oidc:ada@example.com") {
		t.Fatalf("DM members = %s", members)
	}

	ask, err := daemon.GetBead(context.Background(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"kind": "ask", "to": "oidc:ada@example.com", "channel_id": resp.DM, "author": "agent:crew-say",
		"origin": "cli", "slack_channel": resp.Channel, "slack_ts": resp.Timestamp,
	} {
		if got := ask.Fields[k]; got != want {
			t.Errorf("ask %s = %q, want %q", k, got, want)
		}
	}

	var posted bool
	for _, c := range twin.Calls() {
		if c.Method == "chat.postMessage" && c.Params["channel"] == resp.Channel {
			posted = strings.Contains(c.Params["text"], "ship it?") && strings.Contains(c.Params["text"], "Reply in this thread")
		}
	}
	if !posted {
		t.Fatalf("no Slack IM with gb dm ask's face: %+v", twin.Calls())
	}

	// A second DM reuses the channel.
	rr, again := postSayJSON(t, a.mux(), map[string]any{"agent": "crew-say", "to": "oidc:ada@example.com", "text": "done"})
	if rr.Code != http.StatusOK || again.DM != resp.DM {
		t.Fatalf("second say --to: %d %+v, want DM %s", rr.Code, again, resp.DM)
	}
	if n := len(beadsOfType(daemon, "channel")); n != len(channels) {
		t.Fatalf("channels = %d, want %d: the DM was made twice", n, len(channels))
	}
}

func TestSayTo_Refusals(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	daemon.beads["crew-far"] = &beadsapi.BeadDetail{ID: "gd-agent-far", Type: "agent", Status: "open",
		Labels: []string{"project:claimlayer"}, Fields: map[string]string{"agent": "crew-far"}}

	for name, tc := range map[string]struct {
		body map[string]any
		code int
	}{
		"ask without to":      {map[string]any{"agent": "crew-say", "ask": true, "text": "q?"}, http.StatusBadRequest},
		"to with a thread":    {map[string]any{"agent": "crew-say", "to": "ada@example.com", "thread": sayHeadID, "text": "x"}, http.StatusBadRequest},
		"agent of other land": {map[string]any{"agent": "crew-say", "to": "agent:crew-far", "text": "x"}, http.StatusForbidden},
		"no such agent":       {map[string]any{"agent": "crew-say", "to": "agent:nobody", "text": "x"}, http.StatusNotFound},
		"oneself":             {map[string]any{"agent": "crew-say", "to": "agent:crew-say", "text": "x"}, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			rr, _ := postSayJSON(t, a.mux(), tc.body)
			if rr.Code != tc.code {
				t.Fatalf("code = %d (%s), want %d", rr.Code, rr.Body, tc.code)
			}
		})
	}
	if n := len(beadsOfType(daemon, "channel")); n != 1 {
		t.Fatalf("a refused DM made a channel (%d channel beads)", n)
	}
}

// TestCaptureAskReply_SlackReplyLandsInTheDM: a reply in the thread of an
// ask's Slack IM is written under the ask in its DM channel and the thread is
// told it was captured.
func TestCaptureAskReply_SlackReplyLandsInTheDM(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	seedDMPerson(daemon)
	_, resp := postSayJSON(t, a.mux(), map[string]any{"agent": "crew-say", "to": "ada@example.com", "ask": true, "text": "ship it?"})
	if resp.ID == "" {
		t.Fatal("no ask written")
	}
	b := a.bot

	ev := &slackevents.MessageEvent{Channel: resp.Channel, ThreadTimeStamp: resp.Timestamp, TimeStamp: "1790000000.000900", User: "U0ADA", Text: "yes, ship it"}
	if !b.captureAskReply(context.Background(), ev) {
		t.Fatal("the reply was not captured")
	}
	var reply *beadsapi.BeadDetail
	for _, m := range beadsOfType(daemon, "message") {
		if m.Fields["thread_id"] == resp.ID {
			reply = m
		}
	}
	if reply == nil {
		t.Fatal("no reply bead under the ask")
	}
	for k, want := range map[string]string{"channel_id": resp.DM, "origin": "slack", "author": "oidc:ada@example.com", "text": "yes, ship it", "slack_ts": ev.TimeStamp} {
		if got := reply.Fields[k]; got != want {
			t.Errorf("reply %s = %q, want %q", k, got, want)
		}
	}
	var told bool
	for _, c := range twin.Calls() {
		if c.Method == "chat.postMessage" && c.Params["thread_ts"] == resp.Timestamp && strings.Contains(c.Params["text"], "Reply captured") {
			told = true
		}
	}
	if !told {
		t.Fatal("the thread was not told the reply was captured")
	}

	// A reply under any other IM message is not an ask's.
	if b.captureAskReply(context.Background(), &slackevents.MessageEvent{Channel: resp.Channel, ThreadTimeStamp: "1.2", TimeStamp: "1.3", User: "U0ADA", Text: "hm"}) {
		t.Fatal("a reply under no ask was captured")
	}
}

// TestCommonsDM_TopLevelMessageIsDispatched: a person's top-level message in
// a DM channel goes to the agents in it (the reply core's DM branch), where a
// top-level message anywhere else has no thread to hand to.
func TestCommonsDM_TopLevelMessageIsDispatched(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	ch, err := conversation.EnsureDM(context.Background(), daemon, "gardenio", "crew-say, Ada", "agent:crew-say", "oidc:ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	b := a.bot
	if _, ok := b.commonsDM(context.Background(), ch.ID); !ok {
		t.Fatalf("%s is not read as a DM", ch.ID)
	}
	if _, ok := b.commonsDM(context.Background(), sayChanBead); ok {
		t.Fatal("#crew read as a DM")
	}
	in := conversation.Inbound{Kind: conversation.InboundReply, Surface: conversation.SurfaceCommons, Land: "gardenio",
		Channel: ch.ID, Message: "gd-m1", Actor: conversation.Principal{ID: "oidc:ada@example.com"}, Text: "hi"}
	if _, err := b.HandleReply(context.Background(), in); err != nil {
		t.Fatalf("a top-level DM message: %v, want it handled", err)
	}
	in.Channel = sayChanBead
	if _, err := b.HandleReply(context.Background(), in); err == nil {
		t.Fatal("a top-level #crew message was handled as a reply")
	}
}

// TestDMCards_LandInTheDMChannel: the DM-first decision card and the
// escalation operator DM are recorded on card beads in the DM channel between
// the requesting agent and the person; an addressee with no email keeps
// today's card bead, keyed by the Slack IM alone.
func TestDMCards_LandInTheDMChannel(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	seedDMPerson(daemon)
	b, ctx := a.bot, context.Background()
	at := MessageRef{ChannelID: "D0ADA", Timestamp: "1790000000.000100"}

	b.recordDecisionDMCard(ctx, "gardenio", "gd-dec1", agentPrincipal("crew-say"), "U0ADA", at, "Decision needed: ship?")
	b.recordEscalationDM(ctx, BeadEvent{ID: "gd-esc1", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"requesting_agent": "crew-say"}}, "Ada@Example.com", "D0ADA", "1790000000.000200", "Escalation: merge")
	b.recordDecisionDMCard(ctx, "gardenio", "gd-dec2", agentPrincipal("crew-say"), "U0NOBODY", at, "Decision needed: other?")

	dm, err := conversation.EnsureDM(ctx, daemon, "gardenio", "", "agent:crew-say", "oidc:ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(beadsOfType(daemon, "channel")); n != 2 {
		t.Fatalf("channels = %d, want #crew and one DM", n)
	}
	cards := map[string]*beadsapi.BeadDetail{}
	for _, m := range beadsOfType(daemon, "message") {
		if m.Fields["kind"] == "card" {
			cards[m.Fields["subject_id"]] = m
		}
	}
	for _, subject := range []string{"gd-dec1", "gd-esc1"} {
		c := cards[subject]
		if c == nil || c.Fields["channel_id"] != dm.ID || c.Fields["slack_channel"] != "D0ADA" || c.Fields["slack_ts"] == "" {
			t.Errorf("card of %s = %+v, want it in DM %s with its Slack IM ref", subject, c, dm.ID)
		}
	}
	if c := cards["gd-dec2"]; c == nil || c.Fields["channel_id"] != "" || c.Fields["slack_channel"] != "D0ADA" {
		t.Errorf("card of gd-dec2 = %+v, want it keyed by the Slack IM alone", c)
	}
}

// TestEscalationDM_ResolvesTheRequestingAgentBeadIDToItsName: gb escalation
// create writes the agent's bead id into requesting_agent, so the operator DM
// is recorded in the DM keyed by the agent's NAME, the one gb say --to and
// decisions use, not a second DM keyed by agent:<bead id> (gd-DyGO2Z7zh9).
func TestEscalationDM_ResolvesTheRequestingAgentBeadIDToItsName(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	seedDMPerson(daemon)
	daemon.mu.Lock()
	daemon.beads["gd-W68dF2GFPi"] = &beadsapi.BeadDetail{
		ID: "gd-W68dF2GFPi", Type: "agent", Kind: "config", Status: "open", Title: "crew-say",
		Fields: map[string]string{"agent": "crew-say"},
	}
	daemon.mu.Unlock()
	b, ctx := a.bot, context.Background()

	b.recordEscalationDM(ctx, BeadEvent{ID: "gd-esc1", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"requesting_agent": "gd-W68dF2GFPi"}}, "ada@example.com", "D0ADA", "1790000000.000200", "Escalation: merge")
	b.recordEscalationDM(ctx, BeadEvent{ID: "gd-esc2", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"requesting_agent": "crew-say"}}, "ada@example.com", "D0ADA", "1790000000.000300", "Escalation: deploy")

	dm, err := conversation.EnsureDM(ctx, daemon, "gardenio", "", "agent:crew-say", "oidc:ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(beadsOfType(daemon, "channel")); n != 2 {
		t.Fatalf("channels = %d, want #crew and one DM keyed by the agent's name", n)
	}
	for _, m := range beadsOfType(daemon, "message") {
		if m.Fields["kind"] != "card" {
			continue
		}
		if m.Fields["channel_id"] != dm.ID {
			t.Errorf("card of %s in channel %q, want DM %s", m.Fields["subject_id"], m.Fields["channel_id"], dm.ID)
		}
		if got := m.Fields["author"]; strings.Contains(got, "gd-W68dF2GFPi") {
			t.Errorf("card of %s author = %q, want the agent's name", m.Fields["subject_id"], got)
		}
	}
}

// TestEscalationRequester: a name is kept; a bead id resolves to its agent
// bead's name; an id that does not resolve, or whose bead has no agent
// field, is kept as it is.
func TestEscalationRequester(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	a, daemon := newSayAPI(t, twin)
	daemon.mu.Lock()
	daemon.beads["gd-W68dF2GFPi"] = &beadsapi.BeadDetail{ID: "gd-W68dF2GFPi", Type: "agent", Fields: map[string]string{"agent": "gardenio/crew/crew-say"}}
	daemon.beads["gd-noname"] = &beadsapi.BeadDetail{ID: "gd-noname", Type: "agent", Fields: map[string]string{}}
	daemon.mu.Unlock()
	for in, want := range map[string]string{
		"":                "",
		"crew-say":        "crew-say",
		"gd-W68dF2GFPi":   "crew-say",
		" gd-W68dF2GFPi ": "crew-say",
		"gd-missing":      "gd-missing",
		"gd-noname":       "gd-noname",
	} {
		if got := a.bot.escalationRequester(context.Background(), BeadEvent{ID: "gd-esc", Fields: map[string]string{"requesting_agent": in}}); got != want {
			t.Errorf("escalationRequester(%q) = %q, want %q", in, got, want)
		}
	}
}
