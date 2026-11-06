package bridge

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack/slackevents"
)

// A Glass nudge's copy in its agent's thread is mirrored into the agent's
// Slack thread, and never delivered to the agent again (gd-Ik3rTCqfxp).

// newGlassMirrorBot is newInboundReplyBot with a mirror=off channel, crew-live's
// wish head bound to its Slack thread, and crew-card's card bead with its
// Slack copy. It returns the coop recorder's nudges.
func newGlassMirrorBot(t *testing.T) (*Bot, *mockDaemon, *surfacetest.SlackTwin, func() []string) {
	t.Helper()
	twin := surfacetest.NewSlackTwin()
	coop, nudges := newCoopRecorder(t)
	b, daemon := newInboundReplyBot(t, twin, coop)
	daemon.beads["gd-chan-off"] = &beadsapi.BeadDetail{
		ID: "gd-chan-off", Type: "channel", Status: "open", Title: "#machines",
		Labels: []string{"project:gardenio"}, UpdatedAt: twinEpoch.Add(time.Hour), // changed after every nudge below
		Fields: map[string]string{"name": "machines", "slack_channel_id": "C0OFF", "mirror": conversation.MirrorOff},
	}
	daemon.beads["gd-wish-live"] = &beadsapi.BeadDetail{
		ID: "gd-wish-live", Type: "task", Status: "open", Title: "fix the build",
		Labels: []string{"thread", threadBeadLabel("C0LIVE", "1690000000.000100"), "project:gardenio"},
		Fields: map[string]string{"slack_thread_channel": "C0LIVE", "slack_thread_ts": "1690000000.000100", "channel_id": "gd-chan-off"},
	}
	daemon.beads["gd-card-crew"] = &beadsapi.BeadDetail{
		ID: "gd-card-crew", Type: "message", Status: "closed", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"kind": "card", "subject_id": "gd-agent-card", "channel_id": "gd-chan-off",
			"slack_channel": "C0CREW", "slack_ts": "1690000000.000300"},
	}
	return b, daemon, twin, nudges
}

// glassBead is the message a Glass nudge writes with SayInThread: closed,
// origin=glass, by the person on their own bearer, in thread.
func glassBead(id, thread, text string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Type: "message", Status: "closed", Title: text, CreatedBy: "ada@example.com", CreatedAt: twinEpoch,
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"channel_id": "gd-chan-off", "thread_id": thread, "author": "oidc:ada@example.com",
			"kind": "text", "origin": conversation.OriginGlass, "text": text,
		},
	}
}

// TestCommonsMirror_GlassNudge is the bead's test: one Glass nudge shows once
// in the agent's Slack thread, and no path nudges the agent with it again.
func TestCommonsMirror_GlassNudge(t *testing.T) {
	b, daemon, twin, nudges := newGlassMirrorBot(t)
	ctx := context.Background()
	mirror := newMirror(b)
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Bot: b, Logger: slog.Default()})

	// Into a thread-spawned agent's Slack thread, though its channel's mirror
	// is off, and under an agent card's Slack copy.
	wish := glassBead("gd-glass-wish", "gd-wish-live", "look at the <!channel> test first")
	card := glassBead("gd-glass-card", "gd-card-crew", "and then stop")
	for _, d := range []*beadsapi.BeadDetail{wish, card} {
		daemon.beads[d.ID] = d
		mirror.handleEvent(ctx, beadEvent(d))
	}
	ps := posts(twin)
	if len(ps) != 2 {
		t.Fatalf("posts = %+v, want 2", ps)
	}
	for i, want := range []struct{ channel, thread, text string }{
		{"C0LIVE", "1690000000.000100", "*ada* via the Glass: look at the &lt;!channel&gt; test first"},
		{"C0CREW", "1690000000.000300", "*ada* via the Glass: and then stop"},
	} {
		if p := ps[i].Params; p["channel"] != want.channel || p["thread_ts"] != want.thread || p["text"] != want.text {
			t.Errorf("post %d = %v, want %+v", i, p, want)
		}
	}
	if wish.Fields["slack_channel"] != "C0LIVE" || wish.Fields["slack_ts"] == "" {
		t.Errorf("Slack ref not stored on the nudge: %+v", wish.Fields)
	}
	assertMessageSchemaFields(t, wish.Fields)

	// Every way it could come round again: the bot's Slack copy as a thread
	// reply event, the thread forward and the inbound sweep on the bead, an SSE
	// replay, a restarted bridge's mirror and its sweep.
	b.handleMessageEvent(ctx, &slackevents.MessageEvent{
		User: "U0BOT", BotID: "B0BOT", Channel: "C0LIVE", ChannelType: "channel",
		ThreadTimeStamp: "1690000000.000100", TimeStamp: wish.Fields["slack_ts"], Text: ps[0].Params["text"],
	})
	now := twinEpoch.Add(10 * time.Minute)
	for _, d := range []*beadsapi.BeadDetail{wish, card} {
		fwd.handleCreated(ctx, beadEvent(d))
		fwd.handleClosed(ctx, beadEvent(d))
		mirror.handleEvent(ctx, beadEvent(d))
		newMirror(b).handleEvent(ctx, beadEvent(d))
	}
	if n := b.sweepCommonsInbound(ctx, now); n != 0 {
		t.Errorf("the inbound sweep dispatched %d Glass nudges", n)
	}
	if n := newMirror(b).Sweep(ctx, now); n != 0 {
		t.Errorf("the mirror sweep posted %d more", n)
	}
	if got := posts(twin); len(got) != 2 {
		t.Fatalf("posts after the round trip = %+v, want still 2", got)
	}
	if got := nudges(); len(got) != 0 {
		t.Fatalf("the agent was nudged again with the Glass nudge: %v", got)
	}
	if n := len(mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["thread_id"] == "gd-wish-live" })); n != 1 {
		t.Fatalf("the agent's thread has %d messages, want the nudge alone", n)
	}

	// Not posted: one no person is shown to have written, one in a thread with
	// no Slack copy, one with no thread.
	forged := glassBead("gd-glass-forged", "gd-wish-live", "forged")
	forged.CreatedBy = "gardenio-crew-x"
	daemon.beads["gd-commons-only"] = &beadsapi.BeadDetail{ID: "gd-commons-only", Type: "message", Status: "closed",
		Labels: []string{"project:gardenio"}, Fields: map[string]string{"channel_id": "gd-chan-off", "kind": "text", "text": "root"}}
	lonely := glassBead("gd-glass-lonely", "gd-commons-only", "lonely")
	top := glassBead("gd-glass-top", "", "top")
	delete(top.Fields, "thread_id")
	for _, d := range []*beadsapi.BeadDetail{forged, lonely, top} {
		daemon.beads[d.ID] = d
		mirror.handleEvent(ctx, beadEvent(d))
	}
	if n := newMirror(b).Sweep(ctx, now); n != 0 {
		t.Errorf("the sweep posted %d of the unpostable nudges", n)
	}
	if got := posts(twin); len(got) != 2 {
		t.Fatalf("posts = %+v, want still 2", got)
	}
}

// TestCommonsMirror_SweepGlass pins the safety net for a Glass nudge written
// while the bridge was down: posted once, though its channel changed after it,
// and one whose ref stamp was lost is repaired from Slack, never posted twice.
func TestCommonsMirror_SweepGlass(t *testing.T) {
	b, daemon, twin, nudges := newGlassMirrorBot(t)
	ctx := context.Background()
	now := twinEpoch.Add(10 * time.Minute)

	stamped := glassBead("gd-glass-stamped", "gd-wish-live", "posted once")
	daemon.beads[stamped.ID] = stamped
	newMirror(b).handleEvent(ctx, beadEvent(stamped))
	wantTS := stamped.Fields["slack_ts"]
	if wantTS == "" {
		t.Fatalf("not posted live: %+v", stamped.Fields)
	}
	delete(stamped.Fields, "slack_ts")
	delete(stamped.Fields, "slack_channel")

	lost := glassBead("gd-glass-lost", "gd-wish-live", "said while the bridge was down")
	fresh := glassBead("gd-glass-fresh", "gd-wish-live", "just now")
	fresh.CreatedAt = now.Add(-10 * time.Second)
	daemon.beads[lost.ID], daemon.beads[fresh.ID] = lost, fresh

	mirror := newMirror(b) // a restarted bridge
	if n := mirror.Sweep(ctx, now); n != 1 {
		t.Fatalf("sweep posted %d, want 1 (the lost nudge)", n)
	}
	ps := posts(twin)
	if len(ps) != 2 || ps[1].Params["thread_ts"] != "1690000000.000100" || ps[1].Params["text"] != "*ada* via the Glass: said while the bridge was down" {
		t.Fatalf("posts = %+v", ps)
	}
	if stamped.Fields["slack_ts"] != wantTS || stamped.Fields["slack_channel"] != "C0LIVE" {
		t.Errorf("lost ref = %q/%q, want it repaired to C0LIVE/%s", stamped.Fields["slack_channel"], stamped.Fields["slack_ts"], wantTS)
	}
	if lost.Fields["slack_ts"] == "" || fresh.Fields["slack_ts"] != "" {
		t.Errorf("lost = %+v, fresh = %+v", lost.Fields, fresh.Fields)
	}
	if n := mirror.Sweep(ctx, now); n != 0 {
		t.Fatalf("second sweep posted %d", n)
	}
	mirror.handleEvent(ctx, beadEvent(lost))
	if got := len(posts(twin)); got != 2 {
		t.Fatalf("posts = %d, want 2", got)
	}
	if got := nudges(); len(got) != 0 {
		t.Fatalf("the sweep nudged the agent: %v", got)
	}
}

func TestViaGlass(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"Ada Lovelace", "check <https://x|this> & stop", "*Ada Lovelace* via the Glass: check &lt;https://x|this&gt; &amp; stop"},
		{"*bold*", "hi", "*bold* via the Glass: hi"},
		{"", "hi", "*Someone* via the Glass: hi"},
	} {
		if got := viaGlass(c.name, c.text); got != c.want {
			t.Errorf("viaGlass(%q, %q) = %q, want %q", c.name, c.text, got, c.want)
		}
	}
}
