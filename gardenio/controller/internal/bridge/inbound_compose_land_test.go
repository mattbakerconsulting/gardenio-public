package bridge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"
)

// seedLandProject adds land's project bead, sending the Slack channel C0CREW
// to the crew role.
func seedLandProject(d *mockDaemon, land string) {
	d.beads["gd-proj-"+land] = &beadsapi.BeadDetail{
		ID: "gd-proj-" + land, Type: "project", Status: "open", Title: land,
		Fields: map[string]string{"channel_roles": `{"C0CREW":"crew"}`},
	}
}

// seedSlackChannelBead adds a channel bead of land bound to slackChannel: the
// only kind of Slack channel a Commons thread's Slack copy may be in.
func seedSlackChannelBead(d *mockDaemon, id, land, slackChannel string) {
	d.beads[id] = &beadsapi.BeadDetail{
		ID: id, Type: "channel", Status: "open", Title: id, Labels: []string{"project:" + land},
		Fields: map[string]string{"name": id, "slack_channel_id": slackChannel},
	}
}

// seedLiveThreadAgent adds a working agent of land bound to the wish head.
func seedLiveThreadAgent(b *Bot, d *mockDaemon, name, head, land, coop string) {
	d.beads[name] = &beadsapi.BeadDetail{
		ID: "gd-agent-" + name, Type: "agent", Status: "open", Title: name,
		Labels: []string{"project:" + land, commonsThreadLabel}, Notes: "coop_url: " + coop,
		Fields: map[string]string{"agent": name, "project": land, "agent_state": "working", "thread_id": head},
	}
	_ = b.state.SetThreadAgentByHead(head, name)
}

func countAgentBeads(d *mockDaemon) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, b := range d.beads {
		if b.Type == "agent" {
			n++
		}
	}
	return n
}

// TestCommonsCompose_StaysInItsLand is the #784 review's HOLD: a message of
// land A may not reach land B's wish through a root's promoted_to, a root's
// Slack copy, or a reply's thread_id. Each case has a same-land control.
func TestCommonsCompose_StaysInItsLand(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, coopCalls := newDeliveringCoop(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	seedLandProject(daemon, "gardenio")
	daemon.beads["gd-chgen"] = &beadsapi.BeadDetail{
		ID: "gd-chgen", Type: "channel", Status: "open", Title: "general", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "general", "channel_kind": "general"},
	}
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	// Land B's wish, with a live agent.
	daemon.beads["gd-wishb"] = commonsWishBead("gd-wishb", "project:otherland")
	seedLiveThreadAgent(b, daemon, "thread-b", "gd-wishb", "otherland", coop)
	// Land A's wish, promoted from gd-ra, with a live agent.
	daemon.beads["gd-wisha"] = commonsWishBead("gd-wisha", "project:gardenio")
	daemon.beads["gd-wisha"].Fields[fieldPromotedFrom] = "gd-ra"
	seedLiveThreadAgent(b, daemon, "thread-a", "gd-wisha", "gardenio", coop)

	// 1. promoted_to: a land-A root pointing at land B's head.
	daemon.beads["gd-rb"] = commonsMentionBead("gd-rb", "", "mo@example.com", "a plain root")
	daemon.beads["gd-rb"].Fields[fieldPromotedTo] = "gd-wishb"
	daemon.beads["gd-mb"] = commonsMentionBead("gd-mb", "gd-rb", "mo@example.com", "@gardenio cross-land promoted")
	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-mb"}`); rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "thread-b") {
		t.Fatalf("cross-land promoted_to: %d %s", rr.Code, rr.Body.String())
	}
	if got := daemon.beads["gd-rb"].Fields[fieldPromotedTo]; got != "gd-wishb" {
		t.Fatalf("the root's promoted_to was rewritten to %q", got)
	}
	// Control: a root promoted to its own land's head, which names it back.
	daemon.beads["gd-ra"] = commonsMentionBead("gd-ra", "", "mo@example.com", "a plain root")
	daemon.beads["gd-ra"].Fields[fieldPromotedTo] = "gd-wisha"
	daemon.beads["gd-ma"] = commonsMentionBead("gd-ma", "gd-ra", "mo@example.com", "@gardenio same-land promoted")
	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-ma"}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Told thread-a") {
		t.Fatalf("same-land promoted_to: %d %s", rr.Code, rr.Body.String())
	}

	// 2. promoteChatRoot's Slack branch: a root whose Slack copy is a thread
	// whose wish is land B's.
	daemon.beads["gd-wishs"] = commonsWishBead("gd-wishs", "project:otherland", threadBeadLabel("C0B", "1700000000.000100"))
	daemon.beads["gd-rs"] = commonsMentionBead("gd-rs", "", "mo@example.com", "a mirrored root")
	daemon.beads["gd-rs"].Fields["slack_channel"], daemon.beads["gd-rs"].Fields["slack_ts"] = "C0B", "1700000000.000100"
	daemon.beads["gd-ms"] = commonsMentionBead("gd-ms", "gd-rs", "mo@example.com", "@gardenio slack cross-land")
	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-ms"}`); rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "another land") {
		t.Fatalf("cross-land Slack head: %d %s", rr.Code, rr.Body.String())
	}
	if got := daemon.beads["gd-rs"].Fields[fieldPromotedTo]; got != "" {
		t.Fatalf("a root was promoted to another land's head %q", got)
	}
	// Control: the Slack thread's wish is land A's, so the root joins it.
	daemon.beads["gd-wishs2"] = commonsWishBead("gd-wishs2", "project:gardenio", threadBeadLabel("C0A", "1700000000.000200"))
	daemon.beads["gd-wishs2"].Fields["slack_thread_channel"], daemon.beads["gd-wishs2"].Fields["slack_thread_ts"] = "C0A", "1700000000.000200"
	seedLiveThreadAgent(b, daemon, "thread-s2", "gd-wishs2", "gardenio", coop)
	_ = b.state.SetThreadAgent("C0A", "1700000000.000200", "thread-s2")
	seedSlackChannelBead(daemon, "gd-cha", "gardenio", "C0A")
	daemon.beads["gd-rs2"] = commonsMentionBead("gd-rs2", "", "mo@example.com", "a mirrored root")
	daemon.beads["gd-rs2"].Fields["slack_channel"], daemon.beads["gd-rs2"].Fields["slack_ts"] = "C0A", "1700000000.000200"
	daemon.beads["gd-ms2"] = commonsMentionBead("gd-ms2", "gd-rs2", "mo@example.com", "@gardenio slack same-land")
	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-ms2"}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Told thread-s2") {
		t.Fatalf("same-land Slack head: %d %s", rr.Code, rr.Body.String())
	}
	if got := daemon.beads["gd-rs2"].Fields[fieldPromotedTo]; got != "gd-wishs2" {
		t.Fatalf("same-land root promoted_to = %q, want gd-wishs2", got)
	}

	// 3. A reply (no role check) whose thread_id is land B's head, or a root
	// promoted to it, or whose own Slack thread is land B's.
	ctx := context.Background()
	agentsBefore := countAgentBeads(daemon)
	for id, f := range map[string]map[string]string{
		"gd-p1": {"thread_id": "gd-wishb"},
		"gd-p2": {"thread_id": "gd-rb"},
		"gd-p3": {"thread_id": "gd-rb", "slack_thread_channel": "C0B", "slack_thread_ts": "1700000000.000100"},
	} {
		m := commonsMentionBead(id, "", "mo@example.com", "reply cross-land "+id)
		for k, v := range f {
			m.Fields[k] = v
		}
		daemon.beads[id] = m
		_, _, _ = b.dispatchCommonsMessage(ctx, commonsMessageFromDetail(m))
	}
	if n := countAgentBeads(daemon); n != agentsBefore {
		t.Fatalf("a cross-land reply spawned: %d agent beads, was %d", n, agentsBefore)
	}
	// Control: a reply in land A's own wish reaches its agent.
	same := commonsMentionBead("gd-p4", "gd-wisha", "mo@example.com", "reply same-land")
	daemon.beads["gd-p4"] = same
	_, _, _ = b.dispatchCommonsMessage(ctx, commonsMessageFromDetail(same))

	calls := strings.Join(coopCalls(), "\n")
	for _, w := range []string{"same-land promoted", "slack same-land", "reply same-land"} {
		if !strings.Contains(calls, w) {
			t.Errorf("same-land words %q did not reach their agent: %s", w, calls)
		}
	}
	for _, w := range []string{"cross-land promoted", "slack cross-land", "reply cross-land"} {
		if strings.Contains(calls, w) {
			t.Errorf("cross-land words %q reached an agent: %s", w, calls)
		}
	}
}

// TestCommonsCompose_AgentHeldToThreadLand is gd-cVqdya4Zfo. A land-A head's
// slack_thread_channel/ts are its writer's, so they lead a land-A member
// nowhere outside land A:
//
//   - a head naming a Slack thread outside land A's channels (land B's, an
//     agent card's, or one with no agent or a done one) is refused for every
//     mention and verb, and a reply follows it nowhere: no kill, nudge or
//     spawn, and no Slack call in that channel at all (no history read, no
//     post);
//   - a head naming a thread in land A's own channel whose agent is land B's
//     finds no agent: kill and restart have nothing to act on, and a mention
//     or a reply nudges none.
//
// The control is the same, in a Slack thread of land A's agent.
func TestCommonsCompose_AgentHeldToThreadLand(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, coopCalls := newDeliveringCoop(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	seedLandProject(daemon, "gardenio")
	seedLandProject(daemon, "otherland")
	daemon.beads["gd-chgen"] = &beadsapi.BeadDetail{
		ID: "gd-chgen", Type: "channel", Status: "open", Title: "general", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "general", "channel_kind": "general"},
	}
	seedSlackChannelBead(daemon, "gd-cha", "gardenio", "C0A")
	seedSlackChannelBead(daemon, "gd-chb", "otherland", "C0B")
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)
	slackAgent := func(name, land, state string) {
		status := "open"
		if isAgentTerminal(state) {
			status = "closed"
		}
		daemon.beads[name] = &beadsapi.BeadDetail{
			ID: "gd-agent-" + name, Type: "agent", Status: status, Title: name,
			Labels: []string{"project:" + land}, Notes: "coop_url: " + coop,
			Fields: map[string]string{"agent": name, "project": land, "agent_state": state},
		}
	}
	slackHead := func(id, channel, ts string) {
		daemon.beads[id] = commonsWishBead(id, "project:gardenio")
		daemon.beads[id].Fields["slack_thread_channel"], daemon.beads[id].Fields["slack_thread_ts"] = channel, ts
	}

	// Land B's Slack threads: a bound agent, an agent card, none, a done one.
	slackAgent("crew-b", "otherland", "working")
	_ = b.state.SetThreadAgent("C0B", "1700000000.000300", "crew-b")
	slackAgent("crew-bcard", "otherland", "working")
	b.agentCards["crew-bcard"] = MessageRef{ChannelID: "C0BC", Timestamp: "1700000000.000400", Agent: "crew-bcard"}
	slackAgent("crew-bdone", "otherland", "done")
	daemon.beads["crew-bdone"].Fields["slack_thread_channel"], daemon.beads["crew-bdone"].Fields["slack_thread_ts"] = "C0B", "1700000000.000800"
	// Land A's heads, written by a member, pointing at them.
	slackHead("gd-wx1", "C0B", "1700000000.000300")
	slackHead("gd-wx2", "C0BC", "1700000000.000400")
	slackHead("gd-wx3", "C0B", "1700000000.000700")
	slackHead("gd-wx4", "C0B", "1700000000.000800")
	// Land B's agent bound to a thread of land A's own channel.
	slackAgent("crew-b2", "otherland", "working")
	_ = b.state.SetThreadAgent("C0A", "1700000000.000600", "crew-b2")
	slackHead("gd-wz", "C0A", "1700000000.000600")
	// The control: land A's agent in land A's Slack thread.
	slackAgent("crew-a", "gardenio", "working")
	_ = b.state.SetThreadAgent("C0A", "1700000000.000500", "crew-a")
	slackHead("gd-wy", "C0A", "1700000000.000500")

	ctx := context.Background()
	agentsBefore := countAgentBeads(daemon)
	post := func(id, head, text string) (int, string) {
		daemon.beads[id] = commonsMentionBead(id, head, "mo@example.com", text)
		rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"`+id+`"}`)
		return rr.Code, rr.Body.String()
	}
	replies := func(head string) {
		// A reply that may spawn (a member's, through the inbound call) and
		// one that may not (the watchers').
		if code, body := post(head+"-r1", head, "@alice cross-land reply "+head); code != http.StatusOK {
			t.Fatalf("%s member reply: %d %s", head, code, body)
		}
		daemon.beads[head+"-r2"] = commonsMentionBead(head+"-r2", head, "mo@example.com", "cross-land watched reply "+head)
		_, _, _ = b.dispatchCommonsMessage(ctx, commonsMessageFromDetail(daemon.beads[head+"-r2"]))
	}
	verbs := []string{"@gardenio kill", "@gardenio restart", "@gardenio stop --force"}

	// Outside land A's channels: refused outright.
	for _, head := range []string{"gd-wx1", "gd-wx2", "gd-wx3", "gd-wx4"} {
		for i, text := range append(verbs, "@gardenio cross-land nudge "+head) {
			code, body := post(fmt.Sprintf("%s-v%d", head, i), head, text)
			if code != http.StatusUnprocessableEntity || !strings.Contains(body, "Slack copy belongs to another land") {
				t.Fatalf("%s %q: %d %s", head, text, code, body)
			}
		}
		replies(head)
	}
	// In land A's channel, behind land B's agent: no agent works there.
	for i, text := range verbs {
		code, body := post(fmt.Sprintf("gd-wz-v%d", i), "gd-wz", text)
		if code != http.StatusOK || !strings.Contains(body, "No agent works in this thread") || strings.Contains(body, "crew-b2") {
			t.Fatalf("gd-wz %q: %d %s", text, code, body)
		}
	}
	if code, body := post("gd-wz-m", "gd-wz", "@gardenio cross-land nudge gd-wz"); code != http.StatusOK || strings.Contains(body, "Told crew-b2") {
		t.Fatalf("gd-wz mention: %d %s", code, body)
	}
	replies("gd-wz")
	if n := countAgentBeads(daemon); n != agentsBefore {
		t.Fatalf("a cross-land thread spawned: %d agent beads, was %d", n, agentsBefore)
	}
	inLandA := 0
	for _, c := range twin.Calls() {
		switch c.Params["channel"] {
		case "C0B", "C0BC":
			t.Errorf("a land-A thread reached land B's Slack channel: %s %v", c.Method, c.Params)
		case "C0A":
			inLandA++
		}
	}
	// The positive control for the loop above: the gd-wz mention, in land A's
	// own channel, is answered in its Slack thread (crew-b2 holds it), so the
	// twin records calls by the "channel" param this loop reads.
	if inLandA == 0 {
		t.Errorf("no Slack call named land A's channel C0A, so the land-B check proves nothing: %v", twin.Calls())
	}

	// The control: a mention, a reply and a kill reach land A's agent.
	if code, body := post("gd-wy-m", "gd-wy", "@gardenio same-land nudge"); code != http.StatusOK || !strings.Contains(body, "Told crew-a") {
		t.Fatalf("same-land mention: %d %s", code, body)
	}
	daemon.beads["gd-wy-r"] = commonsMentionBead("gd-wy-r", "gd-wy", "mo@example.com", "same-land watched reply")
	_, _, _ = b.dispatchCommonsMessage(ctx, commonsMessageFromDetail(daemon.beads["gd-wy-r"]))
	// --force closes the bead without waiting on coop's graceful shutdown.
	if code, body := post("gd-wy-k", "gd-wy", "@gardenio kill --force"); code != http.StatusOK || !strings.Contains(body, "Killing crew-a now") {
		t.Fatalf("same-land kill: %d %s", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for beadStatus(daemon, "crew-a") != "closed" {
		if time.Now().After(deadline) {
			t.Fatal("the same-land kill did not close crew-a")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, name := range []string{"crew-b", "crew-bcard", "crew-b2"} {
		if got := beadStatus(daemon, name); got != "open" {
			t.Errorf("land B's %s is %q, want open", name, got)
		}
	}

	calls := strings.Join(coopCalls(), "\n")
	if strings.Contains(calls, "shutdown") {
		t.Errorf("an agent was asked to shut down: %s", calls)
	}
	for _, w := range []string{"same-land nudge", "same-land watched reply"} {
		if !strings.Contains(calls, w) {
			t.Errorf("same-land words %q did not reach crew-a: %s", w, calls)
		}
	}
	if strings.Contains(calls, "cross-land") {
		t.Errorf("cross-land words reached an agent: %s", calls)
	}
}

// agentInLand holds an agent to its land's beads, and no land holds no agent;
// agentLive holds the live bead a nudge reaches to the land.
func TestAgentInLand(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	ctx := context.Background()
	daemon.beads["crew-x-prior"] = &beadsapi.BeadDetail{
		ID: "crew-x-prior", Type: "agent", Status: "closed", Title: "crew-x", CreatedAt: time.Now(),
		Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent": "crew-x", "project": "gardenio"},
	}
	if !b.agentInLand(ctx, "crew-x", "gardenio") {
		t.Fatal("a stopped agent of the land is not of it")
	}
	if b.agentInLand(ctx, "crew-x", "") || b.agentInLand(ctx, "crew-x", "otherland") {
		t.Fatal("an agent was held to no land, or to another")
	}
	// A live agent of that name runs in another land: it is not this land's,
	// and agentLive does not count it live here.
	daemon.beads["crew-x"] = &beadsapi.BeadDetail{
		ID: "gd-agent-x-other", Type: "agent", Status: "open", Title: "crew-x",
		Labels: []string{"project:otherland"}, Fields: map[string]string{"agent": "crew-x", "project": "otherland", "agent_state": "working"},
	}
	if b.agentInLand(ctx, "crew-x", "gardenio") {
		t.Fatal("an agent whose live bead is another land's counted as the land's")
	}
	if b.agentLive(ctx, "crew-x", "gardenio") || !b.agentLive(ctx, "crew-x", "otherland") {
		t.Fatal("agentLive did not hold the live bead to the land")
	}
}

func beadStatus(d *mockDaemon, id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.beads[id]; b != nil {
		return b.Status
	}
	return ""
}

// slackThreadInLand trusts a root's own slack_channel/slack_ts (its writer's)
// only in a Slack channel a channel bead of the land binds.
func TestSlackThreadInLand_RootCopyOnlyInItsLandsChannel(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	daemon.beads["gd-chbound"] = &beadsapi.BeadDetail{
		ID: "gd-chbound", Type: "channel", Status: "open", Title: "bound", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "bound", "slack_channel_id": "C0MINE"},
	}
	daemon.beads["gd-chother"] = &beadsapi.BeadDetail{
		ID: "gd-chother", Type: "channel", Status: "open", Title: "theirs", Labels: []string{"project:otherland"},
		Fields: map[string]string{"name": "theirs", "slack_channel_id": "C0THEIRS"},
	}
	ctx := context.Background()
	for _, c := range []struct {
		channel string
		want    bool
	}{
		{"C0MINE", true},    // a real copy: the mirror's channel of the land
		{"C0THEIRS", false}, // another land's channel
		{"C0NONE", false},   // a channel no land binds
	} {
		id := "gd-root-" + c.channel
		root := commonsMentionBead(id, "", "mo@example.com", "a root")
		root.Fields["slack_channel"], root.Fields["slack_ts"] = c.channel, "1700000000.000700"
		daemon.beads[id] = root
		ref := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: c.channel, TS: "1700000000.000700"}
		if got := b.slackThreadInLand(ctx, ref, id, "gardenio"); got != c.want {
			t.Errorf("root copy in %s: slackThreadInLand = %v, want %v", c.channel, got, c.want)
		}
	}
}

// commonsThreadContext reads only the head's land's messages.
func TestCommonsThreadContext_OnlyItsLand(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	daemon.beads["gd-wishc"] = commonsWishBead("gd-wishc", "project:gardenio")
	daemon.beads["gd-wishc"].Fields[conversation.BeadText] = "the wish"
	daemon.beads["gd-c1"] = commonsMentionBead("gd-c1", "gd-wishc", "mo@example.com", "ours")
	theirs := commonsMentionBead("gd-c2", "gd-wishc", "eve@example.com", "theirs")
	theirs.Labels = []string{"project:otherland"}
	daemon.beads["gd-c2"] = theirs

	got := b.commonsThreadContext(context.Background(), "gd-wishc", "gardenio")
	if !strings.Contains(got, "mo@example.com: ours") || strings.Contains(got, "theirs") {
		t.Fatalf("context = %q", got)
	}
	if got := b.commonsThreadContext(context.Background(), "gd-wishc", "otherland"); got != "(no thread context)" {
		t.Fatalf("another land's head gave context %q", got)
	}
}

// The Commons offers --model and a --role of the land; --harness,
// --ultracode, --backend=microvm, another land's --project and a role the
// land does not run are refused.
func TestCommonsSpawnRefusal(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	seedLandProject(daemon, "gardenio")
	ctx := context.Background()
	for _, c := range []struct {
		text string
		want string // "" = allowed
	}{
		{"fix the build", ""},
		{"--model fable fix the build", ""},
		{"--role crew fix the build", ""},
		{"role:thread fix the build", ""},
		{"--backend=kubernetes fix the build", ""},
		{"--project gardenio --role crew fix it", ""},
		{"--role infra fix the build", "does not run the infra role"},
		{"--role=crew-opus fix the build", "does not run the crew-opus role"},
		{"--harness codex fix the build", "--harness"},
		{"--model fable --harness codex fix it", "--harness"},
		{"fix the build --ultracode", "--ultracode"},
		{"--ultracode=off fix the build", "--ultracode"},
		{"fix the build --backend=microvm", "--backend=microvm"},
		{"--project otherland fix the build", "--project otherland"},
	} {
		got := b.commonsSpawnRefusal(ctx, "gardenio", c.text)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("commonsSpawnRefusal(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

// A Commons mention refuses a role its land does not run before anything
// spawns, from --role or from the channel bead's default_role (which a land
// member may write), so the pool path never assigns it either.
func TestConversationInbound_ComposeRoleLimitedToLand(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	seedLandProject(daemon, "gardenio")
	daemon.beads["gd-chgen"] = &beadsapi.BeadDetail{
		ID: "gd-chgen", Type: "channel", Status: "open", Title: "general", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "general", "channel_kind": "general", "default_role": "infra"},
	}
	daemon.beads["gd-x1"] = commonsMentionBead("gd-x1", "", "mo@example.com", "@gardenio --role infra read the nodes")
	daemon.beads["gd-x2"] = commonsMentionBead("gd-x2", "", "mo@example.com", "@gardenio read the nodes")
	daemon.beads["gd-x3"] = commonsMentionBead("gd-x3", "", "mo@example.com", "@gardenio --backend=microvm go")
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-x1"}`); rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "does not run the infra role") {
		t.Fatalf("--role infra: %d %s", rr.Code, rr.Body.String())
	}
	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-x3"}`); rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "--backend=microvm") {
		t.Fatalf("--backend=microvm: %d %s", rr.Code, rr.Body.String())
	}
	agentsBefore := countAgentBeads(daemon)
	rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-x2"}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "does not run the infra role") || strings.Contains(rr.Body.String(), "Starting an agent") {
		t.Fatalf("default_role infra: %d %s", rr.Code, rr.Body.String())
	}
	if n := countAgentBeads(daemon); n != agentsBefore {
		t.Fatalf("a refused role spawned: %d agent beads, was %d", n, agentsBefore)
	}
}

// TestCommonsCompose_RefusesAKeyHeldByANonHead is gd-3HAfTiByJK: a bead that
// already holds a root's head key commons-promote:<root id> but is no wish
// head (a member wrote it ahead of the bridge) is refused, not adopted, and
// neither it nor the root is stamped. The control promotes a fresh root.
func TestCommonsCompose_RefusesAKeyHeldByANonHead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	coop, _ := newDeliveringCoop(t)
	b, daemon := newInboundActionBot(t, twin, coop)
	seedLandProject(daemon, "gardenio")
	daemon.beads["gd-chgen"] = &beadsapi.BeadDetail{
		ID: "gd-chgen", Type: "channel", Status: "open", Title: "general", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "general", "channel_kind": "general"},
	}
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)

	daemon.beads["gd-forged"] = &beadsapi.BeadDetail{
		ID: "gd-forged", Type: "task", Status: "open", Title: "not a wish", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"idempotency_key": "commons-promote:gd-rk"},
	}
	daemon.beads["gd-rk"] = commonsMentionBead("gd-rk", "", "mo@example.com", "a plain root")
	daemon.beads["gd-mk"] = commonsMentionBead("gd-mk", "gd-rk", "mo@example.com", "@gardenio under a forged head")
	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-mk"}`); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "not a wish") {
		t.Fatalf("forged head key: %d %s", rr.Code, rr.Body.String())
	}
	if got := daemon.beads["gd-forged"].Fields[fieldPromotedFrom]; got != "" {
		t.Fatalf("the forged bead was stamped promoted_from=%q", got)
	}
	if got := daemon.beads["gd-rk"].Fields[fieldPromotedTo]; got != "" {
		t.Fatalf("the root was promoted to the forged bead %q", got)
	}

	// Control: a root no bead holds the key of is promoted to a fresh head.
	daemon.beads["gd-rf"] = commonsMentionBead("gd-rf", "", "mo@example.com", "a plain root")
	daemon.beads["gd-mf"] = commonsMentionBead("gd-mf", "gd-rf", "mo@example.com", "@gardenio a fresh head")
	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-mf"}`); rr.Code != http.StatusOK {
		t.Fatalf("fresh head: %d %s", rr.Code, rr.Body.String())
	}
	head := daemon.beads["gd-rf"].Fields[fieldPromotedTo]
	if head == "" || head == "gd-forged" || daemon.beads[head] == nil || daemon.beads[head].Fields[fieldPromotedFrom] != "gd-rf" {
		t.Fatalf("fresh root promoted_to = %q, want a new head naming it back", head)
	}
}

// A Commons mention in a tenant land runs lands-gardener, the role that holds
// the lands-tenant tier, not thread, which holds none (gd-PtbbUxKWds); --role
// thread is not offered there. An operator land keeps thread.
func TestCommonsCompose_TenantLandRunsLandsGardener(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newInboundActionBot(t, twin, "http://127.0.0.1:1")
	seedLandProject(daemon, "gardenio")
	daemon.beads["gd-proj-gardenio"].Fields["land_kind"] = "tenant"
	ctx := context.Background()

	if got := b.commonsThreadRole(ctx, "gardenio"); got != landsGardenerRole {
		t.Fatalf("commonsThreadRole(tenant land) = %q, want %q", got, landsGardenerRole)
	}
	if got := b.commonsSpawnRefusal(ctx, "gardenio", "--role thread fix it"); !strings.Contains(got, "does not run the thread role") {
		t.Fatalf("--role thread in a tenant land: refusal %q", got)
	}
	if got := b.commonsSpawnRefusal(ctx, "gardenio", "--role crew fix it"); got != "" {
		t.Fatalf("--role crew (channel_roles) in a tenant land: refusal %q", got)
	}

	daemon.beads["gd-m1"] = commonsMentionBead("gd-m1", "", "mo@example.com", "@gardenio look into the flaky test")
	mux := http.NewServeMux()
	NewConversationInboundAPI(b, nil).RegisterRoutes(mux)
	if rr := postCard(t, mux, cardMember, `{"kind":"compose","message":"gd-m1"}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Starting an agent") {
		t.Fatalf("compose: %d %s", rr.Code, rr.Body.String())
	}
	role := "(no agent)"
	daemon.mu.Lock()
	for _, d := range daemon.beads {
		if d.Type == "agent" && hasLabel(d.Labels, commonsThreadLabel) {
			role = d.Fields["role"]
		}
	}
	daemon.mu.Unlock()
	if role != landsGardenerRole {
		t.Fatalf("spawned role = %q, want %q", role, landsGardenerRole)
	}

	daemon.beads["gd-proj-gardenio"].Fields["land_kind"] = "operator"
	if got := b.commonsThreadRole(ctx, "gardenio"); got != "thread" {
		t.Fatalf("commonsThreadRole(operator land) = %q, want thread", got)
	}
}
