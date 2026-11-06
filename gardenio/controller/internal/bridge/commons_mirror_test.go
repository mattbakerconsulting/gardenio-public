package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// newMirrorBot is newInboundReplyBot plus three #general-style channel beads
// in land gardenio, bound to Slack channels with each mirror mode.
func newMirrorBot(t *testing.T) (*Bot, *mockDaemon, *surfacetest.SlackTwin) {
	t.Helper()
	twin := surfacetest.NewSlackTwin()
	coop, _ := newCoopRecorder(t)
	b, daemon := newInboundReplyBot(t, twin, coop)
	for id, ch := range map[string][2]string{
		"gd-chan-both": {"C0BOTH", conversation.MirrorBoth},
		"gd-chan-out":  {"C0OUT", conversation.MirrorOut},
		"gd-chan-off":  {"C0OFF", conversation.MirrorOff},
	} {
		daemon.beads[id] = &beadsapi.BeadDetail{
			ID: id, Type: "channel", Status: "open", Title: "#" + id,
			Labels: []string{"project:gardenio"}, UpdatedAt: twinEpoch.Add(-time.Hour),
			Fields: map[string]string{"name": id, "channel_kind": "general", "slack_channel_id": ch[0], "mirror": ch[1]},
		}
	}
	return b, daemon, twin
}

func newMirror(b *Bot) *CommonsMirror {
	return NewCommonsMirror(CommonsMirrorConfig{Router: b.Conversation(), Bot: b, Logger: slog.Default()})
}

// mirroredBeads returns the message beads matching keep.
func mirroredBeads(d *mockDaemon, keep func(*beadsapi.BeadDetail) bool) []*beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if b.Type == "message" && keep(b) {
			out = append(out, b)
		}
	}
	return out
}

func inChannel(chID string) func(*beadsapi.BeadDetail) bool {
	return func(b *beadsapi.BeadDetail) bool { return b.Fields["channel_id"] == chID }
}

func posts(twin *surfacetest.SlackTwin) []surfacetest.TwinCall {
	var out []surfacetest.TwinCall
	for _, c := range twin.Calls() {
		if c.Method == "chat.postMessage" {
			out = append(out, c)
		}
	}
	return out
}

func commonsBead(id, chID, thread, text string) *beadsapi.BeadDetail {
	fields := map[string]string{
		"origin": "commons", "author": "oidc:ada@example.com", "kind": "text",
		"channel_id": chID, "text": text,
	}
	if thread != "" {
		fields["thread_id"] = thread
	}
	return &beadsapi.BeadDetail{ID: id, Type: "message", Status: "closed", Title: text, CreatedBy: "ada@example.com", Labels: []string{"project:gardenio"}, Fields: fields}
}

func beadEvent(d *beadsapi.BeadDetail) []byte {
	return marshalSSEBeadPayload(BeadEvent{
		ID: d.ID, Type: d.Type, Title: d.Title, Status: d.Status, CreatedBy: d.CreatedBy,
		Labels: d.Labels, Fields: d.Fields,
	})
}

func TestMirrorSlackMessage_TopLevel(t *testing.T) {
	b, daemon, twin := newMirrorBot(t)
	ctx := context.Background()

	for _, ev := range []*slackevents.MessageEvent{
		{User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", TimeStamp: "1690000001.000100", Text: "morning all"},
		// Slack redelivers: the same message lands on the same bead.
		{User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", TimeStamp: "1690000001.000100", Text: "morning all"},
		// Not mirrored in: out-only and off channels, a mention (a compose), a
		// bot's post, an edit, an unbound channel.
		{User: "U0ADA", Channel: "C0OUT", ChannelType: "channel", TimeStamp: "1690000001.000200", Text: "out only"},
		{User: "U0ADA", Channel: "C0OFF", ChannelType: "channel", TimeStamp: "1690000001.000300", Text: "off"},
		{User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", TimeStamp: "1690000001.000400", Text: "<@U0BOT> fix it"},
		{User: "U0OTHER", BotID: "B0OTHER", Channel: "C0BOTH", ChannelType: "channel", TimeStamp: "1690000001.000500", Text: "a bot"},
		{User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", TimeStamp: "1690000001.000600", Text: "edited", SubType: "message_changed"},
		{User: "U0ADA", Channel: "C0NONE", ChannelType: "channel", TimeStamp: "1690000001.000700", Text: "unbound"},
	} {
		b.handleMessageEvent(ctx, ev)
	}

	got := mirroredBeads(daemon, func(*beadsapi.BeadDetail) bool { return true })
	if len(got) != 1 {
		t.Fatalf("mirrored %d message beads, want 1: %+v", len(got), got)
	}
	m := got[0]
	want := map[string]string{
		"channel_id": "gd-chan-both", "text": "morning all", "kind": "text", "origin": "slack",
		"slack_channel": "C0BOTH", "slack_ts": "1690000001.000100", "slack_user_id": "U0ADA",
	}
	for k, v := range want {
		if m.Fields[k] != v {
			t.Errorf("fields[%s] = %q, want %q", k, m.Fields[k], v)
		}
	}
	if m.Fields["thread_id"] != "" || !hasLabel(m.Labels, "project:gardenio") || !hasLabel(m.Labels, slackMirrorLabel) {
		t.Errorf("bead = %+v", m)
	}
	if n := len(posts(twin)); n != 0 {
		t.Errorf("mirroring a Slack message posted %d Slack messages", n)
	}
}

func TestMirrorSlackMessage_Reply(t *testing.T) {
	b, daemon, _ := newMirrorBot(t)
	ctx := context.Background()
	// A mirrored top-level message is the root a Slack reply hangs under.
	daemon.beads["gd-root"] = &beadsapi.BeadDetail{
		ID: "gd-root", Type: "message", Status: "closed", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"channel_id": "gd-chan-both", "origin": "slack", "slack_channel": "C0BOTH", "slack_ts": "1690000002.000100"},
	}
	// A wish head's thread: its replies are the thread forward's to record.
	daemon.beads["gd-wish-both"] = &beadsapi.BeadDetail{
		ID: "gd-wish-both", Type: "task", Status: "open",
		Labels: []string{"thread", threadBeadLabel("C0BOTH", "1690000002.000200"), "project:gardenio"},
		Fields: map[string]string{"slack_thread_channel": "C0BOTH", "slack_thread_ts": "1690000002.000200", "channel_id": "gd-chan-both"},
	}

	for _, ev := range []*slackevents.MessageEvent{
		{User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", ThreadTimeStamp: "1690000002.000100", TimeStamp: "1690000002.000101", Text: "agreed"},
		{User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", ThreadTimeStamp: "1690000002.000200", TimeStamp: "1690000002.000201", Text: "on the wish"},
		// A thread whose root predates the mirror: nothing to hang it under.
		{User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", ThreadTimeStamp: "1690000002.000300", TimeStamp: "1690000002.000301", Text: "old thread"},
	} {
		b.handleMessageEvent(ctx, ev)
	}

	under := func(root string) []*beadsapi.BeadDetail {
		return mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["thread_id"] == root })
	}
	if got := under("gd-root"); len(got) != 1 || got[0].Fields["text"] != "agreed" ||
		got[0].Fields["origin"] != "slack" || got[0].Fields["slack_ts"] != "1690000002.000101" {
		t.Fatalf("reply under the mirrored root = %+v", got)
	}
	// The wish thread's reply is recorded once, by the thread forward, and
	// carries origin=slack too.
	if got := under("gd-wish-both"); len(got) != 1 || got[0].Fields["origin"] != "slack" ||
		!hasLabel(got[0].Labels, "slack-thread-message") || hasLabel(got[0].Labels, slackMirrorLabel) {
		t.Fatalf("reply under the wish head = %+v", got)
	}
	if got := mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["text"] == "old thread" }); len(got) != 0 {
		t.Fatalf("a reply with no root bead was mirrored: %+v", got)
	}
}

func TestCommonsMirror_Projects(t *testing.T) {
	b, daemon, twin := newMirrorBot(t)
	ctx := context.Background()
	mirror := newMirror(b)

	top := commonsBead("gd-cm-top", "gd-chan-out", "", "hello <!channel> & all")
	daemon.beads[top.ID] = top
	mirror.handleEvent(ctx, beadEvent(top))

	msgs := twin.Messages("C0OUT")
	if len(msgs) != 1 {
		t.Fatalf("C0OUT messages = %v, want one", msgs)
	}
	if want := "*ada* (via Commons)\nhello &lt;!channel&gt; &amp; all"; msgs[0][1] != want {
		t.Fatalf("text = %q, want %q", msgs[0][1], want)
	}
	if top.Fields["slack_channel"] != "C0OUT" || top.Fields["slack_ts"] != msgs[0][0] {
		t.Fatalf("Slack ref not stored on the bead: %+v", top.Fields)
	}

	// A reply under it goes into its Slack thread.
	reply := commonsBead("gd-cm-reply", "gd-chan-out", top.ID, "and a reply")
	daemon.beads[reply.ID] = reply
	mirror.handleEvent(ctx, beadEvent(reply))
	ps := posts(twin)
	if len(ps) != 2 || ps[1].Params["thread_ts"] != msgs[0][0] || !strings.Contains(ps[1].Params["text"], "and a reply") {
		t.Fatalf("posts = %+v", ps)
	}

	// Not projected: a mirror=off channel, a Commons-only thread, a
	// thread-say reply (ThreadMessageForward's), an origin=slack message.
	off := commonsBead("gd-cm-off", "gd-chan-off", "", "off")
	lonely := commonsBead("gd-cm-lonely", "gd-chan-out", "gd-commons-only-root", "lonely")
	daemon.beads["gd-commons-only-root"] = commonsBead("gd-commons-only-root", "gd-chan-out", "", "root")
	say := commonsBead("gd-cm-say", "gd-chan-out", "gd-head-live", "say")
	say.Labels = append(say.Labels, ThreadSayLabel)
	fromSlack := commonsBead("gd-slack", "gd-chan-both", "", "from slack")
	fromSlack.Fields["origin"] = "slack"
	for _, d := range []*beadsapi.BeadDetail{off, lonely, say, fromSlack} {
		mirror.handleEvent(ctx, beadEvent(d))
	}
	if n := len(posts(twin)); n != 2 {
		t.Fatalf("%d posts, want still 2: %+v", n, posts(twin))
	}
}

// TestThreadMessageForward_AttributesCommons pins that a Commons pane reply
// into a Slack-bound wish thread is attributed as every mirrored Commons
// message is, and the /threads composer's is not.
func TestThreadMessageForward_AttributesCommons(t *testing.T) {
	b, _, twin := newMirrorBot(t)
	ctx := context.Background()
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Logger: slog.Default()})
	for _, ev := range []BeadEvent{
		{ID: "gd-pane", Type: "message", Status: "closed", CreatedBy: "ada@example.com", Labels: []string{ThreadSayLabel, "project:gardenio"},
			Fields: map[string]string{"text": "from the pane", "origin": "commons", "author": "oidc:ada@example.com",
				"slack_thread_channel": "C0LIVE", "slack_thread_ts": "1690000000.000100"}},
		{ID: "gd-composer", Type: "message", Status: "closed", CreatedBy: "ada@example.com", Labels: []string{ThreadSayLabel, "project:gardenio"},
			Fields: map[string]string{"text": "from the composer",
				"slack_thread_channel": "C0LIVE", "slack_thread_ts": "1690000000.000100"}},
	} {
		fwd.handleClosed(ctx, marshalSSEBeadPayload(ev))
	}
	ps := posts(twin)
	if len(ps) != 2 {
		t.Fatalf("posts = %+v", ps)
	}
	if got := ps[0].Params["text"]; got != "*ada* (via Commons)\nfrom the pane" {
		t.Errorf("pane reply text = %q", got)
	}
	if got := ps[1].Params["text"]; got != "from the composer" {
		t.Errorf("composer reply text = %q", got)
	}
}

// TestMirror_NeverEchoes is the loop test (spec section 10, "a message posted
// on either side appears exactly once on each"). Each message is carried the
// whole way round: its copy on the other side is fed back in as that side's
// own event, and nothing new may appear.
func TestMirror_NeverEchoes(t *testing.T) {
	b, daemon, twin := newMirrorBot(t)
	ctx := context.Background()
	mirror := newMirror(b)
	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Bot: b, Logger: slog.Default()})

	// Said in the Commons: one Slack copy, and the bot's post coming back as a
	// Slack event writes no bead.
	said := commonsBead("gd-cm-1", "gd-chan-both", "", "said in the commons")
	daemon.beads[said.ID] = said
	mirror.handleEvent(ctx, beadEvent(said))
	msgs := twin.Messages("C0BOTH")
	if len(msgs) != 1 {
		t.Fatalf("Slack copies = %v, want 1", msgs)
	}
	before := len(mirroredBeads(daemon, inChannel("gd-chan-both")))
	for _, echo := range []*slackevents.MessageEvent{
		{User: "U0BOT", BotID: "B0BOT", Channel: "C0BOTH", ChannelType: "channel", TimeStamp: msgs[0][0], Text: msgs[0][1]},
		// Even if the bot's user id were unknown, its bot_id stops it.
		{User: "U0BOTUSER", BotID: "B0BOT", Channel: "C0BOTH", ChannelType: "channel", TimeStamp: msgs[0][0], Text: msgs[0][1]},
	} {
		b.handleMessageEvent(ctx, echo)
	}
	// The bead's update (Slack ref stamped) and an SSE replay of the create
	// post nothing more.
	mirror.handleEvent(ctx, beadEvent(said))
	newMirror(b).handleEvent(ctx, beadEvent(said))
	if got := len(mirroredBeads(daemon, inChannel("gd-chan-both"))); got != before {
		t.Fatalf("the Slack echo wrote %d new beads", got-before)
	}
	if got := twin.Messages("C0BOTH"); len(got) != 1 {
		t.Fatalf("Slack copies after the round trip = %v, want 1", got)
	}

	// Said in Slack: one bead, and that bead's events post nothing to Slack.
	b.handleMessageEvent(ctx, &slackevents.MessageEvent{
		User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", TimeStamp: "1690000003.000100", Text: "said in slack",
	})
	got := mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["text"] == "said in slack" })
	if len(got) != 1 {
		t.Fatalf("beads for the Slack message = %d, want 1", len(got))
	}
	got[0].Status = "closed"
	mirror.handleEvent(ctx, beadEvent(got[0]))
	fwd.handleClosed(ctx, beadEvent(got[0]))
	fwd.handleCreated(ctx, beadEvent(got[0]))
	if got := twin.Messages("C0BOTH"); len(got) != 1 {
		t.Fatalf("the Slack message echoed back into Slack: %v", got)
	}

	// The same both ways for a reply in a thread.
	reply := commonsBead("gd-cm-2", "gd-chan-both", said.ID, "a commons reply")
	daemon.beads[reply.ID] = reply
	mirror.handleEvent(ctx, beadEvent(reply))
	if n := len(posts(twin)); n != 2 {
		t.Fatalf("posts = %d, want 2", n)
	}
	b.handleMessageEvent(ctx, &slackevents.MessageEvent{
		User: "U0BOT", BotID: "B0BOT", Channel: "C0BOTH", ChannelType: "channel",
		ThreadTimeStamp: msgs[0][0], TimeStamp: "1700000002.000002", Text: "*ada* (via Commons)\na commons reply",
	})
	b.handleMessageEvent(ctx, &slackevents.MessageEvent{
		User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel",
		ThreadTimeStamp: msgs[0][0], TimeStamp: "1690000003.000200", Text: "a slack reply",
	})
	slackReply := mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["text"] == "a slack reply" })
	if len(slackReply) != 1 || slackReply[0].Fields["thread_id"] != said.ID {
		t.Fatalf("Slack reply beads = %+v", slackReply)
	}
	slackReply[0].Status = "closed"
	mirror.handleEvent(ctx, beadEvent(slackReply[0]))
	if n := len(posts(twin)); n != 2 {
		t.Fatalf("posts after the reply round trip = %d, want 2", n)
	}
	if n := len(mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["thread_id"] == said.ID })); n != 2 {
		t.Fatalf("thread has %d beads, want 2 (one reply from each side)", n)
	}
}

// TestMirror_UnsetOrUnboundCopiesNothing is the negative test for the mirror
// default (gd-t3s76tjnz9): a channel bead bound to a Slack channel with mirror
// unset, and one with mirror=both but no Slack binding, copy nothing in either
// direction, through the mirror or through the Router (an agent's say).
func TestMirror_UnsetOrUnboundCopiesNothing(t *testing.T) {
	b, daemon, twin := newMirrorBot(t)
	ctx := context.Background()
	mirror := newMirror(b)
	daemon.beads["gd-chan-unset"] = &beadsapi.BeadDetail{
		ID: "gd-chan-unset", Type: "channel", Status: "open", Title: "#unset",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "unset", "channel_kind": "user", "slack_channel_id": "C0UNSET"},
	}
	daemon.beads["gd-chan-unbound"] = &beadsapi.BeadDetail{
		ID: "gd-chan-unbound", Type: "channel", Status: "open", Title: "#unbound",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "unbound", "channel_kind": "user", "mirror": conversation.MirrorBoth},
	}
	// A root with a Slack copy in the unset channel, so a reply has somewhere
	// to hang if the guard were missing.
	daemon.beads["gd-unset-root"] = &beadsapi.BeadDetail{
		ID: "gd-unset-root", Type: "message", Status: "closed", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"channel_id": "gd-chan-unset", "origin": "commons", "slack_channel": "C0UNSET", "slack_ts": "1690000004.000100"},
	}
	before := len(mirroredBeads(daemon, func(*beadsapi.BeadDetail) bool { return true }))

	// Slack → Commons: nothing is written.
	for _, ev := range []*slackevents.MessageEvent{
		{User: "U0ADA", Channel: "C0UNSET", ChannelType: "channel", TimeStamp: "1690000004.000200", Text: "top level"},
		{User: "U0ADA", Channel: "C0UNSET", ChannelType: "channel", ThreadTimeStamp: "1690000004.000100", TimeStamp: "1690000004.000300", Text: "a reply"},
		{User: "U0ADA", Channel: "C0NONE", ChannelType: "channel", TimeStamp: "1690000004.000400", Text: "no channel bead"},
	} {
		b.handleMessageEvent(ctx, ev)
	}
	if got := mirroredBeads(daemon, func(*beadsapi.BeadDetail) bool { return true }); len(got) != before {
		t.Fatalf("Slack messages in an unset or unbound channel wrote %d beads: %+v", len(got)-before, got)
	}

	// Commons → Slack: nothing is posted, top level or reply.
	for _, d := range []*beadsapi.BeadDetail{
		commonsBead("gd-cm-unset", "gd-chan-unset", "", "unset top"),
		commonsBead("gd-cm-unset-reply", "gd-chan-unset", "gd-unset-root", "unset reply"),
		commonsBead("gd-cm-unbound", "gd-chan-unbound", "", "unbound top"),
	} {
		daemon.beads[d.ID] = d
		mirror.handleEvent(ctx, beadEvent(d))
		if d.Fields["slack_ts"] != "" {
			t.Errorf("%s got a Slack ref: %+v", d.ID, d.Fields)
		}
	}
	// The Router itself treats unset as off: an agent's say in the channel
	// reaches only the Commons.
	ch := conversation.ChannelRef{ID: "gd-chan-unset", Land: "gardenio", Slack: "C0UNSET"}
	if d, err := b.Conversation().Post(ctx, ch, conversation.Message{Text: "an agent says", Author: conversation.Principal{ID: "agent:crew-1"}}); err != nil {
		t.Fatalf("Router.Post: %v", err)
	} else if _, ok := d.Ref(conversation.SurfaceSlack); ok {
		t.Fatalf("Router posted to Slack in an unset channel: %+v", d)
	}
	if ps := posts(twin); len(ps) != 0 {
		t.Fatalf("an unset or unbound channel posted to Slack: %+v", ps)
	}
}

func TestViaCommons(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"Ada Lovelace", "hi", "*Ada Lovelace* (via Commons)\nhi"},
		{"*bold*", "<@U1> <https://x|y>", "*bold* (via Commons)\n&lt;@U1&gt; &lt;https://x|y&gt;"},
		{"", "hi", "*Someone* (via Commons)\nhi"},
	} {
		if got := viaCommons(tc.name, tc.text); got != tc.want {
			t.Errorf("viaCommons(%q, %q) = %q, want %q", tc.name, tc.text, got, tc.want)
		}
	}
	b := &Bot{}
	if got, _ := b.commonsAuthor(context.Background(), "oidc:ada@example.com"); got != "ada" {
		t.Errorf("name without a directory = %q", got)
	}
}

// TestMirrorSlackMessage_Files pins attachments coming in (gd-2Ie1cDAm4K): a
// Slack message's files are recorded as links, and a file-only post is
// mirrored rather than skipped.
func TestMirrorSlackMessage_Files(t *testing.T) {
	b, daemon, _ := newMirrorBot(t)
	ctx := context.Background()
	png := slack.File{ID: "F1", Name: "plot.png", Mimetype: "image/png", Size: 2048, Permalink: "https://acme.slack.com/files/U0ADA/F1/plot.png"}
	pdf := slack.File{ID: "F2", Name: "spec.pdf", Mimetype: "application/pdf", Permalink: "https://acme.slack.com/files/U0ADA/F2/spec.pdf"}
	gone := slack.File{ID: "F3", Name: "gone.txt"} // no permalink: nothing to link
	for _, ev := range []*slackevents.MessageEvent{
		{User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", TimeStamp: "1690000005.000100", SubType: "file_share",
			Message: &slack.Msg{Files: []slack.File{png}}},
		{User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", TimeStamp: "1690000005.000200", Text: "the spec", SubType: "file_share",
			Message: &slack.Msg{Files: []slack.File{pdf, gone}}},
		// A file_share whose files Slack no longer returns has nothing to show.
		{User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", TimeStamp: "1690000005.000300", SubType: "file_share"},
	} {
		b.handleMessageEvent(ctx, ev)
	}

	byTS := func(ts string) *beadsapi.BeadDetail {
		got := mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["slack_ts"] == ts })
		if len(got) > 1 {
			t.Fatalf("%d beads for %s", len(got), ts)
		}
		if len(got) == 0 {
			return nil
		}
		return got[0]
	}
	only := byTS("1690000005.000100")
	if only == nil {
		t.Fatal("a file-only Slack post was not mirrored")
	}
	if _, ok := only.Fields["text"]; ok || only.Title != "Shared plot.png" {
		t.Errorf("file-only bead text=%q title=%q, want no text and a title naming the file", only.Fields["text"], only.Title)
	}
	refs := decodeFileRefs(only.Fields["files"])
	if len(refs) != 1 || refs[0] != (fileRef{Name: "plot.png", MimeType: "image/png", Size: 2048, URL: png.Permalink}) {
		t.Errorf("files = %+v", refs)
	}
	if only.Fields["slack_attachment_count"] != "1" || only.Fields["slack_has_images"] != "true" {
		t.Errorf("attachment fields = %+v", only.Fields)
	}
	captioned := byTS("1690000005.000200")
	if captioned == nil || captioned.Fields["text"] != "the spec" {
		t.Fatalf("captioned bead = %+v", captioned)
	}
	if refs := decodeFileRefs(captioned.Fields["files"]); len(refs) != 1 || refs[0].Name != "spec.pdf" {
		t.Errorf("captioned files = %+v (a file with no permalink is left out)", refs)
	}
	if got := byTS("1690000005.000300"); got != nil {
		t.Errorf("an empty file_share was mirrored: %+v", got)
	}
	// The mock daemon takes any field; the real one 400s an undeclared one,
	// and then the Slack post is not mirrored at all.
	for _, d := range []*beadsapi.BeadDetail{only, captioned} {
		assertMessageSchemaFields(t, d.Fields)
	}
}

// TestCommonsMirror_ProjectsFiles pins attachments going out: a Commons
// message's files are posted as links under its text, and an unsafe URL is
// never put in a Slack link.
func TestCommonsMirror_ProjectsFiles(t *testing.T) {
	b, daemon, twin := newMirrorBot(t)
	ctx := context.Background()
	mirror := newMirror(b)

	withText := commonsBead("gd-cm-files", "gd-chan-out", "", "see attached")
	withText.Fields["files"] = `[{"name":"a|b.png","mimetype":"image/png","url":"https://files.example/a.png"},` +
		`{"name":"evil","url":"javascript:alert(1)"},{"name":"x","url":"https://x.example/<y>"}]`
	fileOnly := commonsBead("gd-cm-fileonly", "gd-chan-out", "", "")
	fileOnly.Title = "Shared notes.txt"
	fileOnly.Fields["files"] = `[{"name":"notes.txt","url":"https://files.example/notes.txt"}]`
	for _, d := range []*beadsapi.BeadDetail{withText, fileOnly} {
		daemon.beads[d.ID] = d
		mirror.handleEvent(ctx, beadEvent(d))
	}
	msgs := twin.Messages("C0OUT")
	if len(msgs) != 2 {
		t.Fatalf("C0OUT messages = %v", msgs)
	}
	if want := "*ada* (via Commons)\nsee attached\n<https://files.example/a.png|a/b.png>"; msgs[0][1] != want {
		t.Errorf("text = %q, want %q", msgs[0][1], want)
	}
	if want := "*ada* (via Commons)\n<https://files.example/notes.txt|notes.txt>"; msgs[1][1] != want {
		t.Errorf("file-only text = %q, want %q", msgs[1][1], want)
	}
}

// twinEpoch is the Slack twin's clock: its first ts is twinEpoch+1s. The sweep
// reads Slack history from a bead's creation time, so sweep tests write beads
// on this clock.
var twinEpoch = time.Unix(1_700_000_000, 0)

func sweptBead(id, chID, thread, text string) *beadsapi.BeadDetail {
	d := commonsBead(id, chID, thread, text)
	d.CreatedAt = twinEpoch
	return d
}

// TestCommonsMirror_Sweep pins the safety net for Commons messages written
// while the bridge was down (gd-2Ie1cDAm4K): one with no Slack ref is posted
// once, and one whose post landed but whose ref was lost is repaired from
// Slack, never posted twice.
func TestCommonsMirror_Sweep(t *testing.T) {
	b, daemon, twin := newMirrorBot(t)
	ctx := context.Background()
	now := twinEpoch.Add(10 * time.Minute)

	// Posted live, then the ref stamp is lost (the bridge stopped in between).
	stamped := sweptBead("gd-sw-stamped", "gd-chan-out", "", "posted once")
	daemon.beads[stamped.ID] = stamped
	newMirror(b).handleEvent(ctx, beadEvent(stamped))
	// A root whose ref is kept, so the replies have a Slack thread.
	root := sweptBead("gd-sw-root", "gd-chan-out", "", "a root")
	daemon.beads[root.ID] = root
	newMirror(b).handleEvent(ctx, beadEvent(root))
	stampedReply := sweptBead("gd-sw-stamped-reply", "gd-chan-out", root.ID, "a reply posted once")
	daemon.beads[stampedReply.ID] = stampedReply
	newMirror(b).handleEvent(ctx, beadEvent(stampedReply))
	wantTS := map[string]string{stamped.ID: stamped.Fields["slack_ts"], stampedReply.ID: stampedReply.Fields["slack_ts"]}
	for _, d := range []*beadsapi.BeadDetail{stamped, stampedReply} {
		if d.Fields["slack_ts"] == "" {
			t.Fatalf("%s was not posted live: %+v", d.ID, d.Fields)
		}
		delete(d.Fields, "slack_ts")
		delete(d.Fields, "slack_channel")
	}
	before := len(posts(twin))

	// Written while the bridge was down: a top-level message and a reply.
	lost := sweptBead("gd-sw-lost", "gd-chan-out", "", "said while the bridge was down")
	lostReply := sweptBead("gd-sw-lost-reply", "gd-chan-out", root.ID, "replied while it was down")
	// Not swept: too fresh, a mirror=off channel, a thread-say reply, one
	// already in Slack, an origin=slack message.
	fresh := sweptBead("gd-sw-fresh", "gd-chan-out", "", "just now")
	fresh.CreatedAt = now.Add(-10 * time.Second)
	off := sweptBead("gd-sw-off", "gd-chan-off", "", "off")
	say := sweptBead("gd-sw-say", "gd-chan-out", "gd-head-live", "say")
	say.Labels = append(say.Labels, ThreadSayLabel)
	done := sweptBead("gd-sw-done", "gd-chan-out", "", "done")
	done.Fields["slack_channel"], done.Fields["slack_ts"] = "C0OUT", "1600000000.000001"
	fromSlack := sweptBead("gd-sw-slack", "gd-chan-both", "", "from slack")
	fromSlack.Fields["origin"] = "slack"
	for _, d := range []*beadsapi.BeadDetail{lost, lostReply, fresh, off, say, done, fromSlack} {
		daemon.beads[d.ID] = d
	}

	mirror := newMirror(b) // a restarted bridge
	if n := mirror.Sweep(ctx, now); n != 2 {
		t.Fatalf("sweep posted %d, want 2 (the two lost messages)", n)
	}
	ps := posts(twin)[before:]
	if len(ps) != 2 {
		t.Fatalf("new posts = %+v", ps)
	}
	var top, reply surfacetest.TwinCall
	for _, p := range ps {
		if p.Params["thread_ts"] != "" {
			reply = p
		} else {
			top = p
		}
	}
	if !strings.Contains(top.Params["text"], "said while the bridge was down") ||
		!strings.Contains(reply.Params["text"], "replied while it was down") || reply.Params["thread_ts"] != root.Fields["slack_ts"] {
		t.Fatalf("swept posts = %+v", ps)
	}
	for id, ts := range wantTS {
		if got := daemon.beads[id].Fields; got["slack_ts"] != ts || got["slack_channel"] != "C0OUT" {
			t.Errorf("%s ref = %q/%q, want it repaired to C0OUT/%s", id, got["slack_channel"], got["slack_ts"], ts)
		}
	}
	for _, d := range []*beadsapi.BeadDetail{lost, lostReply} {
		if d.Fields["slack_ts"] == "" {
			t.Errorf("%s has no Slack ref after the sweep posted it", d.ID)
		}
	}
	if fresh.Fields["slack_ts"] != "" || off.Fields["slack_ts"] != "" || say.Fields["slack_ts"] != "" {
		t.Error("the sweep posted a fresh, mirror=off or thread-say message")
	}

	// Nothing is left: a second sweep, and a late SSE replay, post nothing.
	if n := mirror.Sweep(ctx, now); n != 0 {
		t.Fatalf("second sweep posted %d", n)
	}
	mirror.handleEvent(ctx, beadEvent(lost))
	if got := len(posts(twin)); got != before+2 {
		t.Fatalf("posts = %d, want %d", got, before+2)
	}
	for _, d := range []*beadsapi.BeadDetail{lost, lostReply, stamped, stampedReply} {
		assertMessageSchemaFields(t, d.Fields)
	}
}

// TestCommonsMirror_SweepKeepsDarkMessagesDark pins that turning a channel's
// mirror on does not post what was said while it was off: the sweep posts
// only a message written after its channel bead last changed.
func TestCommonsMirror_SweepKeepsDarkMessagesDark(t *testing.T) {
	b, daemon, twin := newMirrorBot(t)
	ctx := context.Background()
	now := twinEpoch.Add(time.Hour)

	// The admin set mirror=out at +30m; the channel was off before.
	daemon.beads["gd-chan-out"].UpdatedAt = twinEpoch.Add(30 * time.Minute)
	dark := sweptBead("gd-sw-dark", "gd-chan-out", "", "said while the channel was dark")
	atChange := sweptBead("gd-sw-at", "gd-chan-out", "", "said as it changed")
	atChange.CreatedAt = twinEpoch.Add(30 * time.Minute)
	lit := sweptBead("gd-sw-lit", "gd-chan-out", "", "said once it was mirrored")
	lit.CreatedAt = twinEpoch.Add(40 * time.Minute)
	// A channel bead with no updated_at cannot place a message after it.
	unknown := sweptBead("gd-sw-unknown", "gd-chan-both", "", "channel change unknown")
	unknown.CreatedAt = twinEpoch.Add(40 * time.Minute)
	daemon.beads["gd-chan-both"].UpdatedAt = time.Time{}
	for _, d := range []*beadsapi.BeadDetail{dark, atChange, lit, unknown} {
		daemon.beads[d.ID] = d
	}

	if n := newMirror(b).Sweep(ctx, now); n != 1 {
		t.Fatalf("sweep posted %d, want 1 (only the message said after the change)", n)
	}
	ps := posts(twin)
	if len(ps) != 1 || !strings.Contains(ps[0].Params["text"], "said once it was mirrored") {
		t.Fatalf("posts = %+v", ps)
	}
	for _, d := range []*beadsapi.BeadDetail{dark, atChange, unknown} {
		if d.Fields["slack_ts"] != "" {
			t.Errorf("%s reached Slack: %+v", d.ID, d.Fields)
		}
	}
}

// TestCommonsMirror_SweepMatchesCopyByWords pins the double-post guard's
// match: a lost ref is repaired even when the author's name reads differently
// now (the directory missed when the message went out live, so it carried the
// email fallback), and a copy that another message bead holds is never
// stamped on a second message with the same words.
func TestCommonsMirror_SweepMatchesCopyByWords(t *testing.T) {
	b, daemon, twin := newMirrorBot(t)
	ctx := context.Background()
	now := twinEpoch.Add(10 * time.Minute)

	// Posted live as "ada" (no directory entry yet), then the ref is lost;
	// by the sweep the directory names her "Ada Lovelace".
	renamed := sweptBead("gd-sw-renamed", "gd-chan-out", "", "posted under another name")
	daemon.beads[renamed.ID] = renamed
	newMirror(b).handleEvent(ctx, beadEvent(renamed))
	wantTS := renamed.Fields["slack_ts"]
	if wantTS == "" {
		t.Fatalf("not posted live: %+v", renamed.Fields)
	}
	delete(renamed.Fields, "slack_ts")
	delete(renamed.Fields, "slack_channel")
	daemon.beads["gd-user-ada"] = &beadsapi.BeadDetail{
		ID: "gd-user-ada", Type: "user", Kind: "config", Status: "open", Title: "Ada",
		Fields: map[string]string{"email": "ada@example.com", "display_name": "Ada Lovelace"},
	}
	b.userDir = NewUserDirectory(daemon, slog.Default())

	// Two messages with the same words: the first reached Slack and kept its
	// ref, the second never went out. (Both by Ada: another author's "ok"
	// never matches, isCopyOf.)
	first := sweptBead("gd-sw-ok-1", "gd-chan-out", "", "ok")
	daemon.beads[first.ID] = first
	newMirror(b).handleEvent(ctx, beadEvent(first))
	if first.Fields["slack_ts"] == "" {
		t.Fatalf("first not posted live: %+v", first.Fields)
	}
	second := sweptBead("gd-sw-ok-2", "gd-chan-out", "", "ok")
	daemon.beads[second.ID] = second
	before := len(posts(twin))

	if n := newMirror(b).Sweep(ctx, now); n != 1 {
		t.Fatalf("sweep posted %d, want 1 (the second ok)", n)
	}
	if got := renamed.Fields["slack_ts"]; got != wantTS {
		t.Errorf("renamed ref = %q, want it repaired to %s", got, wantTS)
	}
	ps := posts(twin)[before:]
	if len(ps) != 1 || !strings.HasSuffix(ps[0].Params["text"], "\nok") {
		t.Fatalf("new posts = %+v, want the second ok only", ps)
	}
	if second.Fields["slack_ts"] == "" || second.Fields["slack_ts"] == first.Fields["slack_ts"] {
		t.Errorf("second ref = %q, first = %q: want its own copy", second.Fields["slack_ts"], first.Fields["slack_ts"])
	}
}

func TestNormalizeSlackText(t *testing.T) {
	for in, want := range map[string]string{
		"*ada* (via Commons)\nsee https://x.example/a":   "*ada* (via Commons) see https://x.example/a",
		"*ada* (via Commons)\nsee <https://x.example/a>": "*ada* (via Commons) see https://x.example/a",
		"see <https://f.example/n.txt|n.txt>":            "see n.txt",
		"a &lt;b&gt;":                                    "a &lt;b&gt;",
	} {
		if got := normalizeSlackText(in); got != want {
			t.Errorf("normalizeSlackText(%q) = %q, want %q", in, got, want)
		}
	}
}

// asAuthorBot is newMirrorBot with the scope recorder wired as NewBot wires
// it, the twin reporting scopes, and Ada in the user directory with a Slack
// account.
func asAuthorBot(t *testing.T, scopes string) (*Bot, *mockDaemon, *surfacetest.SlackTwin) {
	t.Helper()
	b, daemon, twin := newMirrorBot(t)
	b.slackScopes = &slackScopes{}
	b.api = slack.New("xoxb-test", slack.OptionAPIURL(twin.Serve(t)),
		slack.OptionHTTPClient(b.slackScopes.recorder(http.DefaultClient)))
	daemon.beads["gd-user-ada"] = &beadsapi.BeadDetail{
		ID: "gd-user-ada", Type: "user", Kind: "config", Status: "open", Title: "Ada",
		Fields: map[string]string{"email": "ada@example.com", "slack_id": "U0ADA", "display_name": "Ada Lovelace"},
	}
	b.userDir = NewUserDirectory(daemon, slog.Default())
	twin.SetScopes(scopes)
	// Slack reports the scopes on every response; the bridge's first call is
	// auth.test.
	if _, err := b.api.AuthTestContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	return b, daemon, twin
}

// TestCommonsMirror_PostsAsAuthor pins gd-kktsmUog17: with chat:write.customize
// a Commons message posts under its author's name and Slack avatar, words
// alone; without it the post is the bot's, attributed, exactly as before.
func TestCommonsMirror_PostsAsAuthor(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, scopes           string
		text, username, avatar string
	}{
		{"without the scope", "chat:write,users:read", "*Ada Lovelace* (via Commons)\nhi &amp; bye", "", ""},
		{"no scopes heard", "", "*Ada Lovelace* (via Commons)\nhi &amp; bye", "", ""},
		{"with the scope", "chat:write,chat:write.customize,users:read",
			"hi &amp; bye", "Ada Lovelace", "https://slackmock.test/avatar/U0ADA_72.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, daemon, twin := asAuthorBot(t, tc.scopes)
			top := commonsBead("gd-cm-as", "gd-chan-out", "", "hi & bye")
			daemon.beads[top.ID] = top
			newMirror(b).handleEvent(ctx, beadEvent(top))

			ps := posts(twin)
			if len(ps) != 1 {
				t.Fatalf("posts = %+v", ps)
			}
			p := ps[0].Params
			if p["text"] != tc.text || p["username"] != tc.username || p["icon_url"] != tc.avatar {
				t.Fatalf("post text/username/icon_url = %q/%q/%q, want %q/%q/%q",
					p["text"], p["username"], p["icon_url"], tc.text, tc.username, tc.avatar)
			}
			if top.Fields["slack_ts"] == "" {
				t.Fatal("Slack ref not stored on the bead")
			}

			// A pane reply into a wish thread posts the same way.
			fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Bot: b, Logger: slog.Default()})
			fwd.handleClosed(ctx, marshalSSEBeadPayload(BeadEvent{
				ID: "gd-pane-as", Type: "message", Status: "closed", CreatedBy: "ada@example.com", Labels: []string{ThreadSayLabel, "project:gardenio"},
				Fields: map[string]string{"text": "hi & bye", "origin": "commons", "author": "oidc:ada@example.com",
					"slack_thread_channel": "C0QUIET", "slack_thread_ts": "1690000000.000900"},
			}))
			// (After it, the dispatch may post its own notice.)
			ps = posts(twin)
			if len(ps) < 2 || ps[1].Params["text"] != tc.text || ps[1].Params["username"] != tc.username {
				t.Fatalf("pane reply = %+v, want text %q as %q", ps[1:], tc.text, tc.username)
			}

			// The customized post is still the bot's: coming back as a Slack
			// event it writes no bead (the loop guard).
			before := len(mirroredBeads(daemon, inChannel("gd-chan-both")))
			b.handleMessageEvent(ctx, &slackevents.MessageEvent{
				User: "U0BOT", BotID: "B00000000", Username: tc.username, Channel: "C0BOTH",
				ChannelType: "channel", TimeStamp: "1700000099.000001", Text: tc.text,
			})
			if got := len(mirroredBeads(daemon, inChannel("gd-chan-both"))); got != before {
				t.Fatalf("the bot's own post was mirrored in: %d beads, want %d", got, before)
			}
		})
	}
}

// TestCommonsMirror_ForgedAuthorPostsAsBot pins gd-Wu4U4hZbY0: a Commons
// message whose author field names a person who did not write it (one an
// agent wrote before kbeads refused it, or one person naming another) never
// posts under that person's name and avatar. The bot posts it, attributed to
// its writer, from the mirror and from the thread forward alike, even with
// chat:write.customize.
func TestCommonsMirror_ForgedAuthorPostsAsBot(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, createdBy, text string }{
		{"agent-written", "gardenio-crew-x", "*gardenio-crew-x* (via Commons)\nhi"},
		{"static-written", "static", "*static* (via Commons)\nhi"},
		{"no writer", "", "*Someone* (via Commons)\nhi"},
		{"another person", "bob@example.com", "*bob* (via Commons)\nhi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, daemon, twin := asAuthorBot(t, "chat:write,chat:write.customize,users:read")
			top := commonsBead("gd-cm-forged", "gd-chan-out", "", "hi")
			top.CreatedBy = tc.createdBy
			daemon.beads[top.ID] = top
			newMirror(b).handleEvent(ctx, beadEvent(top))

			fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Bot: b, Logger: slog.Default()})
			fwd.handleClosed(ctx, marshalSSEBeadPayload(BeadEvent{
				ID: "gd-pane-forged", Type: "message", Status: "closed", CreatedBy: tc.createdBy,
				Labels: []string{ThreadSayLabel, "project:gardenio"},
				Fields: map[string]string{"text": "hi", "origin": "commons", "author": "oidc:ada@example.com",
					"slack_thread_channel": "C0QUIET", "slack_thread_ts": "1690000000.000900"},
			}))

			ps := posts(twin)
			if len(ps) != 2 {
				t.Fatalf("posts = %+v, want the mirror's and the forward's", ps)
			}
			for i, from := range []string{"mirror", "thread forward"} {
				p := ps[i].Params
				if p["text"] != tc.text || p["username"] != "" || p["icon_url"] != "" {
					t.Errorf("%s post text/username/icon_url = %q/%q/%q, want %q as the bot",
						from, p["text"], p["username"], p["icon_url"], tc.text)
				}
			}
		})
	}
}

func TestCommonsMessageSlackAuthor(t *testing.T) {
	for _, c := range []struct {
		createdBy, author, who string
		persona                bool
	}{
		{"ada@example.com", "oidc:ada@example.com", "oidc:ada@example.com", true},
		{"ada@example.com", "", "oidc:ada@example.com", true},
		{"gardenio-crew-x", "oidc:ada@example.com", "gardenio-crew-x", false},
		{"bob@example.com", "oidc:ada@example.com", "oidc:bob@example.com", false},
		{"", "oidc:ada@example.com", "", false},
	} {
		m := commonsMessage{Type: "message", CreatedBy: c.createdBy, Fields: map[string]string{conversation.BeadAuthor: c.author}}
		if who, persona := m.slackAuthor(); who != c.who || persona != c.persona {
			t.Errorf("slackAuthor(created_by=%q, author=%q) = %q, %v; want %q, %v", c.createdBy, c.author, who, persona, c.who, c.persona)
		}
	}
}

// TestCommonsMirror_SkipsAnotherWritersChange pins the defence in depth for
// gd-74wnmpQgyT (gd-oWVBI5NFvK): a Commons message whose event names a writer
// other than its creator is not posted, nor is one whose Slack ts was cleared
// after a post. Its creator's own events, and a create, still are.
func TestCommonsMirror_SkipsAnotherWritersChange(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, createdBy, actor, closedBy, slackChannel string
		posted                                         bool
	}{
		{name: "create", createdBy: "ada@example.com", posted: true},
		{name: "creator's update", createdBy: "ada@example.com", actor: "ada@example.com", posted: true},
		{name: "creator's update as principal", createdBy: "ada@example.com", actor: "oidc:ada@example.com", posted: true},
		{name: "creator's close", createdBy: "ada@example.com", closedBy: "ada@example.com", posted: true},
		{name: "agent's own message", createdBy: "gardenio-crew-x", actor: "gardenio-crew-x", posted: true},
		{name: "agent's update", createdBy: "ada@example.com", actor: "gardenio-crew-x"},
		{name: "another person's update", createdBy: "ada@example.com", actor: "bob@example.com"},
		{name: "another person's close", createdBy: "ada@example.com", closedBy: "oidc:bob@example.com"},
		{name: "ts cleared after a post", createdBy: "ada@example.com", slackChannel: "C0OUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, daemon, twin := newMirrorBot(t)
			m := sweptBead("gd-cm-writer", "gd-chan-out", "", "my words")
			m.CreatedBy = tc.createdBy
			if tc.slackChannel != "" {
				m.Fields["slack_channel"] = tc.slackChannel
			}
			daemon.beads[m.ID] = m
			// The writer rides beside the bead, as kbeads publishes it.
			ev, _ := json.Marshal(map[string]any{"actor": tc.actor, "closed_by": tc.closedBy, "bead": BeadEvent{
				ID: m.ID, Type: m.Type, Title: m.Title, Status: m.Status, CreatedBy: m.CreatedBy,
				Labels: m.Labels, Fields: m.Fields,
			}})
			newMirror(b).handleEvent(ctx, ev)
			if got := len(posts(twin)) == 1; got != tc.posted {
				t.Fatalf("posted = %v, want %v: %+v", got, tc.posted, posts(twin))
			}
			if tc.slackChannel != "" {
				// The sweep has no writer to check, but leaves it too.
				if n := newMirror(b).Sweep(ctx, twinEpoch.Add(10*time.Minute)); n != 0 || len(posts(twin)) != 0 {
					t.Fatalf("sweep posted %d: %+v", n, posts(twin))
				}
			}
		})
	}
}

// TestCommonsMirror_SweepKnowsBothForms pins the double-post guard across a
// scope change: a copy posted as the author, or as the bot, is found by the
// sweep whichever way the bridge would post now, so the ref is repaired and
// nothing is posted twice. A post as someone else with the same words is not
// the copy.
func TestCommonsMirror_SweepKnowsBothForms(t *testing.T) {
	const (
		without = "chat:write,users:read"
		with    = "chat:write,chat:write.customize,users:read"
	)
	ctx := context.Background()
	now := twinEpoch.Add(10 * time.Minute)
	for _, tc := range []struct{ name, postedWith, sweptWith string }{
		{"posted as the author, swept without the scope", with, without},
		{"posted as the bot, swept with the scope", without, with},
		{"posted as the author, swept as the author", with, with},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, daemon, twin := asAuthorBot(t, tc.postedWith)
			root := sweptBead("gd-sw-root", "gd-chan-out", "", "a root")
			daemon.beads[root.ID] = root
			newMirror(b).handleEvent(ctx, beadEvent(root))
			lostTop := sweptBead("gd-sw-top", "gd-chan-out", "", "ok")
			lostReply := sweptBead("gd-sw-reply", "gd-chan-out", root.ID, "ok in a thread")
			for _, d := range []*beadsapi.BeadDetail{lostTop, lostReply} {
				daemon.beads[d.ID] = d
				newMirror(b).handleEvent(ctx, beadEvent(d))
			}
			wantTS := map[string]string{}
			for _, d := range []*beadsapi.BeadDetail{lostTop, lostReply} {
				if d.Fields["slack_ts"] == "" {
					t.Fatalf("%s was not posted live", d.ID)
				}
				wantTS[d.ID] = d.Fields["slack_ts"]
				delete(d.Fields, "slack_ts")
				delete(d.Fields, "slack_channel")
			}
			before := len(posts(twin))

			twin.SetScopes(tc.sweptWith)
			if _, err := b.api.AuthTestContext(ctx); err != nil {
				t.Fatal(err)
			}
			if n := newMirror(b).Sweep(ctx, now); n != 0 {
				t.Fatalf("sweep posted %d, want 0: the copies are in Slack (%+v)", n, posts(twin)[before:])
			}
			for id, ts := range wantTS {
				if got := daemon.beads[id].Fields["slack_ts"]; got != ts {
					t.Errorf("%s ref = %q, want it repaired to %s", id, got, ts)
				}
			}
		})
	}

	// Same words, but the copy in Slack is under another name: not this
	// message's copy, so the sweep posts it.
	b, daemon, twin := asAuthorBot(t, with)
	if _, _, err := b.api.PostMessageContext(ctx, "C0OUT", slack.MsgOptionText("ok", false), slack.MsgOptionUsername("Grace Hopper")); err != nil {
		t.Fatal(err)
	}
	lost := sweptBead("gd-sw-lost", "gd-chan-out", "", "ok")
	daemon.beads[lost.ID] = lost
	if n := newMirror(b).Sweep(ctx, now); n != 1 {
		t.Fatalf("sweep posted %d, want 1", n)
	}
	if got := twin.Usernames("C0OUT"); len(got) != 2 || got[1] != "Ada Lovelace" {
		t.Fatalf("C0OUT usernames = %q", got)
	}
}

func TestSlackScopes(t *testing.T) {
	var nilScopes *slackScopes
	if nilScopes.Has(scopeChatWriteCustomize) {
		t.Error("nil scopes has a scope")
	}
	s := &slackScopes{}
	if s.Has("chat:write") {
		t.Error("nothing heard yet, but has chat:write")
	}
	resp := func(scopes string) slackHTTPClient {
		return doFunc(func(*http.Request) (*http.Response, error) {
			h := http.Header{}
			if scopes != "-" {
				h.Set("X-OAuth-Scopes", scopes)
			}
			return &http.Response{StatusCode: 200, Header: h, Body: http.NoBody}, nil
		})
	}
	call := func(c slackHTTPClient, token string) {
		req, _ := http.NewRequest(http.MethodPost, "https://slack.test/api/auth.test", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if _, err := c.Do(req); err != nil {
			t.Fatal(err)
		}
	}
	call(s.recorder(resp("chat:write, chat:write.customize")), "xoxb-1")
	if !s.Has("chat:write") || !s.Has(scopeChatWriteCustomize) {
		t.Error("scopes not recorded")
	}
	// The app-level token's scopes, and a response with no header, say
	// nothing about the bot token.
	call(s.recorder(resp("connections:write")), "xapp-1")
	call(s.recorder(resp("-")), "xoxb-1")
	if !s.Has(scopeChatWriteCustomize) {
		t.Error("an app-token or headerless response changed the bot's scopes")
	}
	// A scope removed is seen on the next response.
	call(s.recorder(resp("chat:write")), "xoxb-1")
	if s.Has(scopeChatWriteCustomize) {
		t.Error("a removed scope is still held")
	}
}

type doFunc func(*http.Request) (*http.Response, error)

func (f doFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// TestCommonsMirror_SnapViewLink: a farm snapshot from the Commons (gd-GbF2JS04h5)
// goes out with its picture's link and then its view's, a snapshot with no
// words under the name alone, as an agent nudged by it reads it too.
func TestCommonsMirror_SnapViewLink(t *testing.T) {
	b, daemon, twin := newMirrorBot(t)
	ctx := context.Background()
	mirror := newMirror(b)

	snap := commonsBead("gd-cm-snap", "gd-chan-out", "", "")
	snap.Title = "Snapshot of the farm at gardenio/gd-p1"
	snap.Fields["files"] = `[{"name":"farm-gardenio-gardenio-gd-p1-20261019-2340.png","mimetype":"image/png","url":"https://garden.example/garden/c/f/gardenio/gd-chan-out/0123456789abcdef0123456789abcdef/farm.png"}]`
	snap.Fields["snap"] = `{"kind":"farm","url":"https://garden.example/garden?at=gardenio/gd-p1&z=2","at":"gardenio/gd-p1","z":2}`
	daemon.beads[snap.ID] = snap
	mirror.handleEvent(ctx, beadEvent(snap))
	msgs := twin.Messages("C0OUT")
	if len(msgs) != 1 {
		t.Fatalf("C0OUT messages = %v", msgs)
	}
	want := "*ada* (via Commons)\n<https://garden.example/garden/c/f/gardenio/gd-chan-out/0123456789abcdef0123456789abcdef/farm.png|farm-gardenio-gardenio-gd-p1-20261019-2340.png>\n" +
		"<https://garden.example/garden?at=gardenio/gd-p1&z=2|Open the farm view at gardenio/gd-p1>"
	if msgs[0][1] != want {
		t.Errorf("text = %q, want %q", msgs[0][1], want)
	}

	in := commonsMessageFromDetail(snap).inbound()
	if want := "Snapshot of the farm at gardenio/gd-p1\n" +
		"farm-gardenio-gardenio-gd-p1-20261019-2340.png (image/png): https://garden.example/garden/c/f/gardenio/gd-chan-out/0123456789abcdef0123456789abcdef/farm.png\n" +
		"Open the farm view at gardenio/gd-p1: https://garden.example/garden?at=gardenio/gd-p1&z=2\n" +
		"fetch with: gb commons file gd-cm-snap"; in.Text != want {
		t.Errorf("inbound text = %q, want %q", in.Text, want)
	}
}

func TestWithSnapLink(t *testing.T) {
	pic := []fileRef{{Name: "a.png", URL: "https://f.example/a.png"}}
	for raw, want := range map[string]string{
		`{"url":"https://g.example/garden?at=yard","at":"yard"}`:       "Open the farm view at the Yard",
		`{"url":"https://g.example/garden"}`:                           "Open the farm view at the wild",
		`{"url":"https://g.example/garden?at=~/gd-m1","at":"~/gd-m1"}`: "Open the farm view at gd-m1",
		`{"url":"javascript:alert(1)","at":"yard"}`:                    "",
		`{"url":"https://g.example/<x>","at":"yard"}`:                  "",
		`not json`: "",
		``:         "",
	} {
		got := withSnapLink(append([]fileRef(nil), pic...), raw)
		switch {
		case want == "" && len(got) != 1:
			t.Errorf("%s: added %+v", raw, got)
		case want != "" && (len(got) != 2 || got[1].Name != want):
			t.Errorf("%s: %+v, want a link named %q", raw, got, want)
		}
	}
	if got := withSnapLink(nil, `{"url":"https://g.example/garden"}`); len(got) != 0 {
		t.Errorf("a snap with no picture added %+v", got)
	}
}
