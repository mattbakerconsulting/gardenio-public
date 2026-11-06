package bridge

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"
)

// messageBeads returns the type=message beads the mock daemon holds.
func messageBeads(d *mockDaemon) []*beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if b.Type == "message" {
			out = append(out, b)
		}
	}
	return out
}

// A squawk bead already exists, so routing it writes no new bead: the router
// projects it into the agent's thread and stamps the Slack ref on it.
func TestRoutedSquawkStampsSlackRefOnItsBead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newLeafBot(t, twin)
	daemon.beads["gd-sq9"] = &beadsapi.BeadDetail{ID: "gd-sq9", Type: "message", Status: "closed", Labels: []string{"squawk", "project:gardenio"}, Fields: map[string]string{}}

	s := NewSquawk(SquawkConfig{Daemon: daemon, Bot: b, Logger: slog.Default()})
	s.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID: "gd-sq9", Type: "message", Title: "tests green", Labels: []string{"squawk", "from:crew-thread", "project:gardenio"},
	}))

	if n := len(messageBeads(daemon)); n != 1 {
		t.Fatalf("message beads = %d, want only the squawk's own", n)
	}
	msgs := twin.Messages("C0THREAD")
	if len(msgs) != 1 {
		t.Fatalf("thread messages = %v, want one reply", msgs)
	}
	f := daemon.beads["gd-sq9"].Fields
	if f["slack_channel"] != "C0THREAD" || f["slack_ts"] != msgs[0][0] {
		t.Fatalf("squawk bead fields = %v, want the slack ref %s", f, msgs[0][0])
	}
}

// A death report is a notice message bead in the agent's land: one project
// label, the notice face, and the Slack ref of the copy it projected.
func TestRoutedDeathReportWritesNoticeBead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newLeafBot(t, twin)

	if err := b.NotifyAgentDeath(context.Background(), BeadEvent{
		ID: "gd-dead1", Type: "agent", Assignee: "crew-vibe", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"close_reason": "context_cancelled_involuntary"},
	}); err != nil {
		t.Fatal(err)
	}
	beads := messageBeads(daemon)
	if len(beads) != 1 {
		t.Fatalf("message beads = %d, want 1", len(beads))
	}
	got := beads[0]
	if len(got.Labels) != 1 || got.Labels[0] != "project:gardenio" {
		t.Fatalf("labels = %v, want exactly project:gardenio", got.Labels)
	}
	f := got.Fields
	if f["kind"] != "notice" || f["detail"] == "" || f["footnote"] != "Forensics on bead `gd-dead1`" {
		t.Fatalf("fields = %v, want the rich notice face", f)
	}
	if f["author"] != "agent:"+deathReportAuthor {
		t.Fatalf("author = %q", f["author"])
	}
	msgs := twin.Messages("C0THREAD")
	if len(msgs) != 1 || f["slack_ts"] != msgs[0][0] || f["slack_channel"] != "C0THREAD" {
		t.Fatalf("slack ref on bead = %v, thread shows %v", f, msgs)
	}
}

// assertMessageSchemaFields fails unless every field on a message bead is
// declared on the bridge's type:message config, with an enum field holding one
// of its values. The mock daemon takes any field; the real one 400s an
// undeclared one, and the router then posts a bead-less Slack copy.
func assertMessageSchemaFields(t *testing.T, fields map[string]string) {
	t.Helper()
	tc, ok := configs()["type:message"].(TypeConfig)
	if !ok {
		t.Fatal(`configs() has no type:message TypeConfig`)
	}
	defs := map[string]FieldDef{}
	for _, fd := range tc.Fields {
		defs[fd.Name] = fd
	}
	for name, v := range fields {
		fd, ok := defs[name]
		if !ok {
			t.Errorf("message field %q is not declared on type:message (init_message.go and kbeads config.go)", name)
			continue
		}
		if fd.Type == "enum" && !slices.Contains(fd.Values, v) {
			t.Errorf("message field %s=%q is not one of %v", name, v, fd.Values)
		}
	}
}

// A routed agent notice (spawn, schedule) writes a bead the daemon's
// type:message schema admits: its detail and footnote are declared fields.
func TestRoutedAgentNoticeFieldsMatchMessageSchema(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newLeafBot(t, twin)

	m := agentNotice("Agent spawned: crew-card", "*Agent spawned:* crew-card")
	m.Footnote = "Forensics on bead `gd-agent-card`"
	if _, err := b.postAgentNotice(context.Background(), "crew-card", "gardenio", "C0CREW", "", m); err != nil {
		t.Fatal(err)
	}
	if err := b.NotifyAgentDeath(context.Background(), BeadEvent{
		ID: "gd-dead1", Type: "agent", Assignee: "crew-vibe", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"close_reason": "context_cancelled_involuntary"},
	}); err != nil {
		t.Fatal(err)
	}
	beads := messageBeads(daemon)
	if len(beads) != 2 {
		t.Fatalf("message beads = %d, want the spawn notice and the death report", len(beads))
	}
	for _, bead := range beads {
		f := bead.Fields
		if f["kind"] != "notice" || f["detail"] == "" || f["footnote"] == "" || f["slack_ts"] == "" {
			t.Fatalf("fields = %v, want a notice face with its Slack ref", f)
		}
		assertMessageSchemaFields(t, f)
	}
}

// Degrade, never drop: when the message bead cannot be written the crash
// notice still posts, straight to the Slack surface.
func TestRoutedNoticePostsWhenBeadWriteFails(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newLeafBot(t, twin)
	daemon.createBeadHook = func(int, beadsapi.CreateBeadRequest) error { return errors.New("daemon down") }

	if err := b.NotifyAgentCrash(context.Background(), BeadEvent{
		ID: "gd-crash1", Type: "agent", Assignee: "crew-thread", Labels: []string{"project:gardenio"},
	}); err != nil {
		t.Fatal(err)
	}
	if msgs := twin.Messages("C0THREAD"); len(msgs) != 1 || !strings.Contains(msgs[0][1], "*Agent crashed: crew-thread*") {
		t.Fatalf("thread shows %v, want the crash notice", msgs)
	}
}

// With no land there is no bead to write: the storm roll-up is fleet-wide and
// posts with no message bead.
func TestDeathStormRollupWritesNoBead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newLeafBot(t, twin)
	b.postDeathStormRollup(context.Background(), 3, nil)
	if n := len(messageBeads(daemon)); n != 0 {
		t.Fatalf("message beads = %d, want 0", n)
	}
	if msgs := twin.Messages("C0OPS"); len(msgs) != 1 || msgs[0][2] == "" {
		t.Fatalf("ops channel shows %v, want the roll-up with its block", msgs)
	}
}

// The viewer stamps only thread_id on a composer message; its land is the
// wish head's project, so the forwarder routes it and stamps the Slack ref.
func TestThreadMessageForwardLandFromWishHead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newLeafBot(t, twin)
	daemon.beads["gd-wish1"] = &beadsapi.BeadDetail{ID: "gd-wish1", Type: "task", Labels: []string{"thread", "project:gardenio"}}
	daemon.beads["gd-say9"] = &beadsapi.BeadDetail{ID: "gd-say9", Type: "message", Status: "closed", Fields: map[string]string{}}

	fwd := NewThreadMessageForward(ThreadMessageForwardConfig{Router: b.Conversation(), Bot: b, Logger: slog.Default()})
	ev := BeadEvent{ID: "gd-say9", Labels: []string{ThreadSayLabel}, Fields: map[string]string{"thread_id": "gd-wish1"}}
	if got := fwd.land(context.Background(), &ev); got != "gardenio" {
		t.Fatalf("land = %q, want gardenio from the wish head", got)
	}

	ref, err := routeSlackMessage(context.Background(), b.Conversation(), slog.Default(), "gardenio", "C0WISH", "1690000000.000500",
		conversation.Message{BeadID: "gd-say9", Text: "from the composer"})
	if err != nil {
		t.Fatal(err)
	}
	if f := daemon.beads["gd-say9"].Fields; f["slack_ts"] != ref.TS || f["slack_channel"] != "C0WISH" {
		t.Fatalf("composer bead fields = %v, want slack ref %+v", f, ref)
	}
	if n := len(messageBeads(daemon)); n != 1 {
		t.Fatalf("message beads = %d, want only the composer's own", n)
	}
}
