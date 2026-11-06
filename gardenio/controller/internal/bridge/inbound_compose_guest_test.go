package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack/slackevents"
)

// Follow-ups from the #784 review (gd-Vpva1k4aK1): a guest's @-reply is
// delivered, commonsComposes fails closed, the --project check holds in a
// Commons thread with a Slack copy, and @<agent> does not wake an agent into
// a done wish.

// commonsClosedEvent is the SSE close event of a message bead.
func commonsClosedEvent(d *beadsapi.BeadDetail) []byte {
	data, _ := json.Marshal(map[string]any{"bead": map[string]any{
		"id": d.ID, "type": d.Type, "title": d.Title, "status": "closed",
		"created_by": d.CreatedBy, "labels": d.Labels, "fields": d.Fields,
	}})
	return data
}

// A guest may reply but not spawn: their @-mention in a thread reaches the
// thread's agent as a reply through the inbound call, with a note saying so.
// Their top-level mention and their verb are still refused, and nothing
// spawns.
func TestConversationInbound_GuestMentionIsAReply(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, coopCalls := newDeliveringCoop(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	daemon.beads["gd-wishg"] = commonsWishBead("gd-wishg", "project:gardenio")
	seedLiveThreadAgent(b, daemon, "thread-g", "gd-wishg", "gardenio", coop)
	daemon.beads["gd-wishe"] = commonsWishBead("gd-wishe", "project:gardenio") // no agent yet
	for _, m := range []*beadsapi.BeadDetail{
		commonsMentionBead("gd-g1", "gd-wishg", "gu@example.com", "@gardenio also check the logs"),
		commonsMentionBead("gd-g2", "gd-wishg", "gu@example.com", "@thread-g the bucket too"),
		commonsMentionBead("gd-g3", "gd-wishe", "gu@example.com", "@gardenio start one for this"),
		commonsMentionBead("gd-g4", "", "gu@example.com", "@thread-g hi from the top"),
		commonsMentionBead("gd-g5", "gd-wishg", "gu@example.com", "@gardenio kill"),
		commonsMentionBead("gd-g6", "", "gu@example.com", "hello @alice"),
	} {
		daemon.beads[m.ID] = m
	}
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)
	agentsBefore := countAgentBeads(daemon)

	for _, c := range []struct {
		msg  string
		code int
		want string
	}{
		{"gd-g1", http.StatusOK, "went to the thread as a reply"},
		{"gd-g2", http.StatusOK, "went to the thread as a reply"},
		{"gd-g3", http.StatusOK, "went to the thread as a reply"},
		{"gd-g4", http.StatusForbidden, "does not let you"},
		{"gd-g5", http.StatusForbidden, "does not let you"},
		{"gd-g6", http.StatusUnprocessableEntity, "names no agent"},
	} {
		rr := postCard(t, mux, cardGuest, `{"kind":"compose","message":"`+c.msg+`"}`)
		if rr.Code != c.code || !strings.Contains(rr.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want %d containing %q", c.msg, rr.Code, rr.Body.String(), c.code, c.want)
		}
	}
	if n := countAgentBeads(daemon); n != agentsBefore {
		t.Fatalf("a guest's mention spawned: %d agent beads, was %d", n, agentsBefore)
	}
	calls := strings.Join(coopCalls(), "\n")
	for _, w := range []string{"also check the logs", "the bucket too"} {
		if !strings.Contains(calls, w) {
			t.Errorf("the guest's reply %q did not reach the thread's agent: %s", w, calls)
		}
	}
	for _, w := range []string{"hi from the top", "kill"} {
		if strings.Contains(calls, w) {
			t.Errorf("a refused guest mention %q reached an agent: %s", w, calls)
		}
	}
	for _, id := range []string{"gd-g1", "gd-g2", "gd-g3"} {
		if daemon.beads[id].Fields[commonsHandledAt] == "" {
			t.Errorf("%s was delivered but not stamped handled_at", id)
		}
	}
}

// commonsComposes fails closed: a name the daemon could not look up leaves
// the message to the inbound call, and what a watcher or the sweep does
// dispatch runs as a reply, never as a mention, whatever it names.
func TestCommonsComposes_FailsClosed(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	ctx := context.Background()
	m := commonsMessageFromDetail(commonsMentionBead("gd-f1", "gd-wishf", "gu@example.com", "@crew-x wake up"))

	daemon.findAgentAnyErr = errors.New("daemon unavailable")
	if !b.commonsComposes(ctx, m) {
		t.Fatal("a mention lookup that failed let the watchers dispatch the message")
	}
	daemon.findAgentAnyErr = nil
	if b.commonsComposes(ctx, m) {
		t.Fatal("a name that is no agent (a person) composes")
	}

	var mu sync.Mutex
	var kinds []conversation.InboundKind
	record := func(_ context.Context, in conversation.Inbound) (conversation.Answer, error) {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, in.Kind)
		return conversation.Answer{}, nil
	}
	b.inbound().Handle(conversation.InboundCompose, record)
	b.inbound().Handle(conversation.InboundReply, record)

	// A watcher's dispatch, and the sweep's of a mention the inbound call
	// never ran, are replies.
	daemon.beads["gd-f2"] = commonsMentionBead("gd-f2", "gd-wishf", "gu@example.com", "@gardenio spawn one")
	if _, dispatched, err := b.dispatchCommonsMessage(ctx, commonsMessageFromDetail(daemon.beads["gd-f2"])); !dispatched || err != nil {
		t.Fatalf("dispatch: %v %v", dispatched, err)
	}
	old := commonsMentionBead("gd-f3", "gd-wishf", "gu@example.com", "@gardenio spawn another")
	old.CreatedAt = time.Now().Add(-5 * time.Minute)
	daemon.beads["gd-f3"] = old
	if n := b.sweepCommonsInbound(ctx, time.Now()); n != 1 {
		t.Fatalf("sweep dispatched %d, want 1", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(kinds) != 2 || kinds[0] != conversation.InboundReply || kinds[1] != conversation.InboundReply {
		t.Fatalf("dispatched kinds = %v, want two replies", kinds)
	}
}

// The --project check of a Commons spawn holds in a Commons thread that has a
// Slack copy too: it keys on the Commons anchor (its land), not on whether the
// thread posts to Slack.
func TestThreadSpawn_CommonsProjectCheckWithSlackCopy(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	seedLandProject(daemon, "gardenio")
	seedLandProject(daemon, "otherland")
	head := commonsWishBead("gd-wishm", "project:gardenio", threadBeadLabel("C0CREW", "1700000000.000300"))
	head.Fields["slack_thread_channel"], head.Fields["slack_thread_ts"] = "C0CREW", "1700000000.000300"
	daemon.beads["gd-wishm"] = head
	seedSlackChannelBead(daemon, "gd-chcrew", "gardenio", "C0CREW")
	var said []string
	in := conversation.Inbound{Land: "gardenio", Channel: "gd-chgen", Actor: conversation.Principal{ID: "oidc:mo@example.com"}}
	at, ok := b.commonsThreadAt(context.Background(), head, in, func(s string) { said = append(said, s) })
	if !ok || !at.slack() || !at.commons() {
		t.Fatalf("anchor = %+v, want a Commons anchor with a Slack copy", at)
	}

	agentsBefore := countAgentBeads(daemon)
	text := "--project otherland fix the build"
	b.handleThreadSpawnAt(context.Background(), at, &slackevents.AppMentionEvent{
		Channel: at.Channel, ThreadTimeStamp: at.TS, TimeStamp: at.TS, Text: text,
	}, text)
	if n := countAgentBeads(daemon); n != agentsBefore {
		t.Fatalf("--project otherland spawned from a gardenio thread with a Slack copy: %d agent beads, was %d", n, agentsBefore)
	}
	posted := strings.Join(said, "\n")
	for _, c := range twin.Calls() {
		posted += "\n" + c.Params["text"]
	}
	if !strings.Contains(posted, "--project otherland is not allowed here") {
		t.Fatalf("no refusal was said: %s", posted)
	}
}

// @<agent> for a stopped agent whose wish is done does not wake it into the
// done wish, as @gardenio in that thread would not; nor into a head of another
// land.
func TestCommonsCompose_NamedAgentNotWokenIntoDoneWish(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	seedLandProject(daemon, "gardenio")
	done := commonsWishBead("gd-wishd", "project:gardenio")
	done.Status = "closed"
	daemon.beads["gd-wishd"] = done
	daemon.beads["gd-wishx"] = commonsWishBead("gd-wishx", "project:otherland")
	stopped := func(name, head string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID: "gd-agent-" + name, Type: "agent", Status: "closed", Title: name,
			Labels: []string{"project:gardenio", commonsThreadLabel},
			Fields: map[string]string{"agent": name, "project": "gardenio", "agent_state": "done", "thread_id": head},
		}
	}
	daemon.beads["crew-done"] = stopped("crew-done", "gd-wishd")
	daemon.beads["crew-away"] = stopped("crew-away", "gd-wishx")
	daemon.beads["gd-n1"] = commonsMentionBead("gd-n1", "", "mo@example.com", "@crew-done hi")
	daemon.beads["gd-n2"] = commonsMentionBead("gd-n2", "", "mo@example.com", "@crew-away hi")
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)
	agentsBefore := countAgentBeads(daemon)

	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-n1"}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "its wish is done") {
		t.Fatalf("@crew-done: %d %s", rr.Code, rr.Body.String())
	}
	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-n2"}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "not this land's") {
		t.Fatalf("@crew-away: %d %s", rr.Code, rr.Body.String())
	}
	if n := countAgentBeads(daemon); n != agentsBefore {
		t.Fatalf("a stopped agent was woken: %d agent beads, was %d", n, agentsBefore)
	}
	for _, name := range []string{"crew-done", "crew-away"} {
		if a := daemon.beads[name]; a.Status != "closed" || a.Fields["agent_state"] != "done" {
			t.Errorf("%s was woken: status %s, agent_state %s", name, a.Status, a.Fields["agent_state"])
		}
	}
}
