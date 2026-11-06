package bridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"
)

// newDeliveringCoop is a coop that records every call and takes every nudge.
func newDeliveringCoop(t *testing.T) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.URL.Path+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"delivered":true,"state":"idle"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestCommonsMentionParsing(t *testing.T) {
	if got := commonsMentionTokens("hi @Gardenio and @crew-ann, cc @gardenio. mail a@b.c"); strings.Join(got, ",") != "gardenio,crew-ann" {
		t.Fatalf("tokens = %v", got)
	}
	if got := stripMentions("@gardenio  --role crew fix @alice's build", map[string]bool{"gardenio": true}); got != "--role crew fix @alice's build" {
		t.Fatalf("strip = %q", got)
	}
	for _, c := range []struct {
		text  string
		verb  string
		force bool
	}{
		{"@gardenio kill", "kill", false},
		{"@gardenio stop --force", "stop", true},
		{"@gardenio restart", "restart", false},
		{"@gardenio clear", "clear", false},
		{"@gardenio kill the flaky job", "", false},
		{"kill", "", false},
		{"@crew-ann restart", "", false},
	} {
		if v, f := commonsVerb(c.text); v != c.verb || f != c.force {
			t.Errorf("commonsVerb(%q) = %q %v, want %q %v", c.text, v, f, c.verb, c.force)
		}
	}
	if got := plainNote(":warning: :x: Unrecognized model"); got != "Unrecognized model" {
		t.Fatalf("plainNote = %q", got)
	}
	name := commonsThreadAgentName("gd-AbC123")
	if name != commonsThreadAgentName("gd-AbC123") || name == commonsThreadAgentName("gd-abc123") ||
		!regexp.MustCompile(`^thread-c[0-9a-f]{10}$`).MatchString(name) {
		t.Fatalf("commonsThreadAgentName = %q", name)
	}
}

// commonsMention is a closed origin=commons message by who in the Commons-only
// channel gd-chgen, a reply under thread when set.
func commonsMentionBead(id, thread, who, text string) *beadsapi.BeadDetail {
	f := map[string]string{
		"origin": "commons", "author": "oidc:" + who, "channel_id": "gd-chgen", "text": text, "kind": "text",
	}
	if thread != "" {
		f["thread_id"] = thread
	}
	return &beadsapi.BeadDetail{
		ID: id, Type: "message", Status: "closed", Title: text,
		Labels: []string{"project:gardenio"}, CreatedBy: who, CreatedAt: time.Now(), Fields: f,
	}
}

// TestConversationInbound_ComposeSpawnsInCommonsThread drives a Commons
// mention through POST /api/conversation/inbound: the land-role gate, the chat
// root promoted to a wish head, and an agent spawned and bound by the head
// alone, with nothing posted to Slack.
func TestConversationInbound_ComposeSpawnsInCommonsThread(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	seedLandProject(daemon, "gardenio") // the land runs crew
	daemon.beads["gd-chgen"] = &beadsapi.BeadDetail{
		ID: "gd-chgen", Type: "channel", Status: "open", Title: "general", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "general", "channel_kind": "general", "default_role": "crew"},
	}
	daemon.beads["gd-m1"] = commonsMentionBead("gd-m1", "", "mo@example.com", "@gardenio --model fable look into the flaky test")
	daemon.beads["gd-m2"] = commonsMentionBead("gd-m2", "", "gu@example.com", "@gardenio do it")
	daemon.beads["gd-m3"] = commonsMentionBead("gd-m3", "", "ada@example.com", "@gardenio do it")
	daemon.beads["gd-m4"] = commonsMentionBead("gd-m4", "", "mo@example.com", "hello @alice")
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	for _, c := range []struct {
		name   string
		caller cardCaller
		body   string
		code   int
		want   string
	}{
		{"guest", cardGuest, `{"kind":"compose","message":"gd-m2"}`, http.StatusForbidden, "does not let you"},
		{"someone else's", cardMember, `{"kind":"compose","message":"gd-m3"}`, http.StatusForbidden, "someone else"},
		{"names no agent", cardMember, `{"kind":"compose","message":"gd-m4"}`, http.StatusUnprocessableEntity, "names no agent"},
		{"no message", cardMember, `{"kind":"compose"}`, http.StatusBadRequest, "message"},
		{"member", cardMember, `{"kind":"compose","message":"gd-m1"}`, http.StatusOK, "Starting an agent"},
	} {
		rr := postCard(t, mux, c.caller, c.body)
		if rr.Code != c.code || !strings.Contains(rr.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, rr.Code, rr.Body.String(), c.code, c.want)
		}
	}

	var head, agent *beadsapi.BeadDetail
	daemon.mu.Lock()
	for _, d := range daemon.beads {
		switch {
		case d.Type == "task" && d.Fields[fieldPromotedFrom] == "gd-m1":
			head = d
		case d.Type == "agent" && hasLabel(d.Labels, commonsThreadLabel):
			agent = d
		}
	}
	daemon.mu.Unlock()
	if head == nil {
		t.Fatal("the mentioned chat root was not promoted to a wish head")
	}
	if !hasLabel(head.Labels, threadKindLabel) || !hasLabel(head.Labels, "project:gardenio") ||
		head.Fields[conversation.BeadChannelID] != "gd-chgen" || head.Fields["slack_thread_channel"] != "" {
		t.Fatalf("head = %+v", head)
	}
	if got := daemon.beads["gd-m1"].Fields[fieldPromotedTo]; got != head.ID {
		t.Fatalf("root promoted_to = %q, want %q", got, head.ID)
	}
	if agent == nil {
		t.Fatal("no agent was spawned for the Commons thread")
	}
	sel, _, _ := resolveHarnessModel("", "fable")
	want := map[string]string{
		"agent": commonsThreadAgentName(head.ID), "project": "gardenio", "role": "crew",
		"thread_id": head.ID, "spawn_source": "commons-thread", "operated_by": "oidc:mo@example.com",
		"model": sel.Model,
	}
	for k, v := range want {
		if agent.Fields[k] != v {
			t.Errorf("agent %s = %q, want %q", k, agent.Fields[k], v)
		}
	}
	for _, k := range []string{"slack_thread_channel", "slack_thread_ts", "slack_spawn_channel"} {
		if agent.Fields[k] != "" {
			t.Errorf("a Commons-only agent carries %s=%q", k, agent.Fields[k])
		}
	}
	if !strings.Contains(agent.Description, "Triggered by: look into the flaky test\n") {
		t.Errorf("description = %q", agent.Description)
	}
	if got, ok := b.state.GetThreadAgentByHead(head.ID); !ok || got != agent.Fields["agent"] {
		t.Fatalf("binding by head = %q %v", got, ok)
	}
	for _, c := range twin.Calls() {
		if c.Method == "chat.postMessage" || c.Method == "assistant.threads.setStatus" {
			t.Fatalf("a Commons-only spawn called Slack: %+v", c)
		}
	}

	// A second mention in the same thread finds the bound agent, so it does
	// not spawn a second one (it wakes the first, here, as its pod is gone).
	daemon.beads["gd-m5"] = commonsMentionBead("gd-m5", head.ID, "mo@example.com", "@gardenio and the other test?")
	rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-m5"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("second mention: %d %s", rr.Code, rr.Body.String())
	}
	n := 0
	daemon.mu.Lock()
	for _, d := range daemon.beads {
		if d.Type == "agent" && hasLabel(d.Labels, commonsThreadLabel) {
			n++
		}
	}
	daemon.mu.Unlock()
	if n != 1 {
		t.Fatalf("a mention in a bound thread made %d agent beads, want 1", n)
	}
}

// A mention in a Commons thread whose agent runs nudges it; @gardenio kill
// needs agent_control and kills it; @<agent> nudges a named agent of the land.
func TestConversationInbound_ComposeLiveAgent(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, coopCalls := newDeliveringCoop(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	daemon.beads["gd-wishc"] = commonsWishBead("gd-wishc", "project:gardenio")
	daemon.beads["thread-cwish"] = &beadsapi.BeadDetail{
		ID: "gd-agent-cwish", Type: "agent", Status: "open", Title: "thread-cwish",
		Labels: []string{"project:gardenio", commonsThreadLabel}, Notes: "coop_url: " + coop,
		Fields: map[string]string{"agent": "thread-cwish", "project": "gardenio", "agent_state": "working", "thread_id": "gd-wishc"},
	}
	_ = b.state.SetThreadAgentByHead("gd-wishc", "thread-cwish")
	daemon.beads["gd-r1"] = commonsMentionBead("gd-r1", "gd-wishc", "mo@example.com", "@gardenio what's left?")
	daemon.beads["gd-r2"] = commonsMentionBead("gd-r2", "", "mo@example.com", "@crew-live please look at gd-123")
	daemon.beads["gd-r3"] = commonsMentionBead("gd-r3", "gd-wishc", "gu@example.com", "@gardenio kill")
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-r1"}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Told thread-cwish") {
		t.Fatalf("mention to the thread's agent: %d %s", rr.Code, rr.Body.String())
	}
	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-r2"}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Told crew-live") {
		t.Fatalf("mention of a named agent: %d %s", rr.Code, rr.Body.String())
	}
	if rr := postCard(t, mux, cardGuest, `{"kind":"compose","message":"gd-r3"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("a guest's kill: %d %s", rr.Code, rr.Body.String())
	}
	calls := strings.Join(coopCalls(), "\n")
	if !strings.Contains(calls, "what's left?") || !strings.Contains(calls, "please look at gd-123") {
		t.Fatalf("coop calls = %s", calls)
	}
	if strings.Contains(calls, "@gardenio") || strings.Contains(calls, "@crew-live") {
		t.Fatalf("the mention itself reached the agent: %s", calls)
	}
}

// The SSE watchers leave a mention to the inbound call, which alone carries
// the person's land role; a plain Commons reply in a Commons-only thread
// reaches its live agent through the watcher. The sweep, which has no caller,
// delivers a mention the inbound call never ran as a reply (gd-Vpva1k4aK1):
// before slice 4d a guest's "@gardenio ..." in a thread arrived that way.
func TestCommonsMention_WatchersLeaveItToTheInboundCall(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, coopCalls := newDeliveringCoop(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	daemon.beads["gd-wishw"] = commonsWishBead("gd-wishw", "project:gardenio")
	daemon.beads["thread-cw"] = &beadsapi.BeadDetail{
		ID: "gd-agent-cw", Type: "agent", Status: "open", Title: "thread-cw",
		Labels: []string{"project:gardenio", commonsThreadLabel}, Notes: "coop_url: " + coop,
		Fields: map[string]string{"agent": "thread-cw", "project": "gardenio", "agent_state": "working", "thread_id": "gd-wishw"},
	}
	_ = b.state.SetThreadAgentByHead("gd-wishw", "thread-cw")
	old := time.Now().Add(-5 * time.Minute)
	mention := commonsMentionBead("gd-w1", "gd-wishw", "gu@example.com", "@gardenio spawn me another")
	mention.CreatedAt = old
	plain := commonsMentionBead("gd-w2", "gd-wishw", "gu@example.com", "the logs are in the bucket")
	plain.CreatedAt = old
	daemon.beads["gd-w1"], daemon.beads["gd-w2"] = mention, plain

	if !b.commonsComposes(context.Background(), commonsMessageFromDetail(mention)) {
		t.Fatal("a mention of @gardenio composes")
	}
	if b.commonsComposes(context.Background(), commonsMessageFromDetail(plain)) {
		t.Fatal("a plain reply does not compose")
	}
	ctx := context.Background()
	b.handleCommonsDMEvent(ctx, commonsClosedEvent(mention))
	b.handleCommonsDMEvent(ctx, commonsClosedEvent(plain))
	if daemon.beads["gd-w1"].Fields[commonsHandledAt] != "" {
		t.Fatal("a watcher dispatched a mention")
	}
	if daemon.beads["gd-w2"].Fields[commonsHandledAt] == "" {
		t.Fatal("the watcher left a plain reply undelivered")
	}
	if calls := strings.Join(coopCalls(), "\n"); !strings.Contains(calls, "the logs are in the bucket") || strings.Contains(calls, "spawn me another") {
		t.Fatalf("coop calls after the watcher = %s", calls)
	}

	// A guest's mention in a thread with no agent yet: the sweep delivers it
	// as a reply, which spawns nothing.
	daemon.beads["gd-wishe"] = commonsWishBead("gd-wishe", "project:gardenio")
	idle := commonsMentionBead("gd-w3", "gd-wishe", "gu@example.com", "@gardenio start one")
	idle.CreatedAt = old
	daemon.beads["gd-w3"] = idle
	agentsBefore := countAgentBeads(daemon)
	b.sweepCommonsInbound(ctx, time.Now())
	for _, id := range []string{"gd-w1", "gd-w3"} {
		if daemon.beads[id].Fields[commonsHandledAt] == "" {
			t.Fatalf("the sweep left %s, a mention the inbound call never ran, undelivered", id)
		}
	}
	if calls := strings.Join(coopCalls(), "\n"); !strings.Contains(calls, "spawn me another") {
		t.Fatalf("the swept mention did not reach the thread's agent as a reply: %s", calls)
	}
	if n := countAgentBeads(daemon); n != agentsBefore {
		t.Fatalf("a swept mention spawned: %d agent beads, was %d", n, agentsBefore)
	}
}
