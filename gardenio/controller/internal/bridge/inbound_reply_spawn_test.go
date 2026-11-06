package bridge

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// gd-Vc9dC61oKP: a plain Commons reply in a thread with a Slack copy may not
// start or wake an agent unless the inbound call checked the sender's land
// role. The watchers and the sweep have no caller, and landrole.go denies a
// guest "spawn", so their reply only reaches a live agent, as in a Commons
// thread with no Slack copy.

// newSlackCopyReplyBot is newCommonsReplyBot with the inbound API, and a
// wish head whose Slack thread is bound to the gone agent crew-gone and one
// whose Slack thread no agent has worked yet.
func newSlackCopyReplyBot(t *testing.T) (*Bot, *mockDaemon, *http.ServeMux, func() []string) {
	t.Helper()
	b, daemon, _, nudges := newCommonsReplyBot(t)
	daemon.seedProjectWithChannel("gardenio", "C0GONE")
	// crew-gone's closed bead: its land is what lets a Commons reply wake it.
	seedPriorAgentBead(daemon, "crew-gone", "gardenio")
	daemon.beads["gd-head-gone"] = &beadsapi.BeadDetail{
		ID: "gd-head-gone", Type: "task", Status: "open", Title: "the gone wish",
		Labels: []string{"thread", "project:gardenio"},
		Fields: map[string]string{"slack_thread_channel": "C0GONE", "slack_thread_ts": "1690000000.000200"},
	}
	daemon.beads["gd-head-new"] = &beadsapi.BeadDetail{
		ID: "gd-head-new", Type: "task", Status: "open", Title: "the new wish",
		Labels: []string{"thread", "project:gardenio"},
		Fields: map[string]string{"slack_thread_channel": "C0GONE", "slack_thread_ts": "1690000000.000900"},
	}
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)
	return b, daemon, mux, nudges
}

// A guest's message in a thread with a Slack copy goes to the thread as a
// reply through the inbound call, and wakes no stopped agent and starts none,
// whether it @-mentions @gardenio or only a person.
func TestCommonsReply_GuestInSlackCopiedThreadSpawnsNothing(t *testing.T) {
	_, daemon, mux, nudges := newSlackCopyReplyBot(t)
	for _, m := range []*beadsapi.BeadDetail{
		commonsMentionBead("gd-gs1", "gd-head-gone", "gu@example.com", "@gardenio wake up"),
		commonsMentionBead("gd-gs2", "gd-head-gone", "gu@example.com", "@alice look at this"),
		commonsMentionBead("gd-gs3", "gd-head-new", "gu@example.com", "@gardenio start one"),
		commonsMentionBead("gd-gs4", "gd-head-new", "gu@example.com", "@alice see here"),
	} {
		daemon.beads[m.ID] = m
	}
	agentsBefore := countAgentBeads(daemon)
	for _, id := range []string{"gd-gs1", "gd-gs2", "gd-gs3", "gd-gs4"} {
		if rr := postCard(t, mux, cardGuest, `{"kind":"compose","message":"`+id+`"}`); rr.Code != http.StatusOK {
			t.Errorf("%s: %d %s, want the reply delivered", id, rr.Code, rr.Body.String())
		}
		if daemon.beads[id].Fields[commonsHandledAt] == "" {
			t.Errorf("%s was delivered but not stamped handled_at", id)
		}
	}
	if n := countAgentBeads(daemon); n != agentsBefore {
		t.Fatalf("a guest's reply in a Slack-copied thread spawned: %d agent beads, was %d", n, agentsBefore)
	}
	if n := countContaining(nudges(), "(bead gd-gs"); n != 0 {
		t.Fatalf("a guest's reply nudged a thread with no live agent %d times", n)
	}
}

// The member control: the same reply through the inbound call, from a member
// whose land role may spawn, wakes the gone agent with session resume, and an
// operator's reply call does too.
func TestCommonsReply_MemberInSlackCopiedThreadRespawns(t *testing.T) {
	for _, c := range []struct {
		name   string
		caller cardCaller
		kind   string
	}{
		{"member", cardMember, "compose"},
		{"operator", cardOperatorOf("mo@example.com"), "reply"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, daemon, mux, _ := newSlackCopyReplyBot(t)
			daemon.beads["gd-ms1"] = commonsMentionBead("gd-ms1", "gd-head-gone", "mo@example.com", "@alice look at this")
			if rr := postCard(t, mux, c.caller, `{"kind":"`+c.kind+`","message":"gd-ms1"}`); rr.Code != http.StatusOK {
				t.Fatalf("%d %s, want the reply handled", rr.Code, rr.Body.String())
			}
			if bead := singleResumeBead(t, daemon); bead.Fields["agent"] != "crew-gone" {
				t.Fatalf("respawned %q, want the thread's own agent crew-gone", bead.Fields["agent"])
			}
		})
	}
}

// cardOperatorOf is an operator (admin) caller with email.
func cardOperatorOf(email string) cardCaller {
	return cardCaller{email, []string{oidcauth.RoleAdmin}, nil}
}

// What the watchers and the sweep dispatch has no caller: a gone agent stays
// gone and an unworked thread gets no agent, while a live one is nudged.
func TestCommonsReply_WatchersAndSweepOnlyNudge(t *testing.T) {
	b, daemon, _, nudges := newSlackCopyReplyBot(t)
	ctx := context.Background()
	agentsBefore := countAgentBeads(daemon)

	// The Commons thread watcher (dm_replies.go).
	daemon.beads["gd-w1"] = commonsReplyBead("gd-w1", "gd-head-gone", "ada@example.com", time.Now())
	b.handleCommonsDMEvent(ctx, commonsClosedEvent(daemon.beads["gd-w1"]))
	// The thread forward of a Commons pane reply that names its Slack thread.
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Bot: b, Logger: slog.Default()})
	pane := commonsReplyBead("gd-w2", "gd-head-new", "ada@example.com", time.Now())
	pane.Labels = append(pane.Labels, ThreadSayLabel)
	pane.Fields["slack_thread_channel"], pane.Fields["slack_thread_ts"] = "C0GONE", "1690000000.000900"
	daemon.beads["gd-w2"] = pane
	fwd.handleClosed(ctx, commonsClosedEvent(pane))
	// The sweep, of a reply the inbound call never delivered.
	daemon.beads["gd-w3"] = commonsReplyBead("gd-w3", "gd-head-gone", "ada@example.com", time.Now().Add(-5*time.Minute))
	b.sweepCommonsInbound(ctx, time.Now())

	for _, id := range []string{"gd-w1", "gd-w2", "gd-w3"} {
		if daemon.beads[id].Fields[commonsHandledAt] == "" {
			t.Errorf("%s was not dispatched", id)
		}
	}
	if n := countAgentBeads(daemon); n != agentsBefore {
		t.Fatalf("a watcher's or the sweep's reply spawned: %d agent beads, was %d", n, agentsBefore)
	}

	// gd-cm1 is newCommonsReplyBot's reply into crew-live's thread.
	b.sweepCommonsInbound(ctx, time.Now())
	if countContaining(nudges(), "(bead gd-cm1)") == 0 {
		t.Fatalf("the live agent was not nudged with the swept reply; nudges=%v", nudges())
	}
	if n := countAgentBeads(daemon); n != agentsBefore {
		t.Fatalf("nudging the live agent spawned: %d agent beads, was %d", n, agentsBefore)
	}
}

// The /threads composer's thread-say, which has no origin, keeps its spawn
// (ka-0000005407) when it carries its land: it wakes the gone agent. One of
// no land is relayed to Slack and wakes nothing (gd-cVqdya4Zfo: no land holds
// no agent).
func TestCommonsReply_ThreadsComposerStillRespawns(t *testing.T) {
	say := func(id string, labels ...string) []byte {
		return marshalSSEBeadPayload(BeadEvent{
			ID: id, Type: "message", Status: "closed", CreatedBy: "ada@example.com",
			Labels: append([]string{ThreadSayLabel}, labels...),
			Fields: map[string]string{
				"text": "are you there", "thread_id": "gd-head-gone",
				"slack_thread_channel": "C0GONE", "slack_thread_ts": "1690000000.000200",
			},
		})
	}

	b, daemon, _, _ := newSlackCopyReplyBot(t)
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Bot: b, Logger: slog.Default()})
	agentsBefore := countAgentBeads(daemon)
	fwd.handleClosed(context.Background(), say("gd-say-noland"))
	if n := countAgentBeads(daemon); n != agentsBefore {
		t.Fatalf("a composer message of no land spawned: %d agent beads, was %d", n, agentsBefore)
	}

	fwd.handleClosed(context.Background(), say("gd-say-gone", "project:gardenio"))
	if bead := singleResumeBead(t, daemon); bead.Fields["agent"] != "crew-gone" {
		t.Fatalf("respawned %q, want crew-gone", bead.Fields["agent"])
	}
}
