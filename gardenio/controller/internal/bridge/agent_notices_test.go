package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newNoticeBot is newLeafBot in a land with a Commons: channel beads bound to
// the thread agent's and the card agent's Slack channels, the thread agent
// bound to a wish head, and the card agent's card bead.
func newNoticeBot(t *testing.T, twin *surfacetest.SlackTwin) (*Bot, *mockDaemon) {
	t.Helper()
	b, daemon := newLeafBot(t, twin)
	land := []string{"project:gardenio"}
	daemon.beads["gd-chThread"] = &beadsapi.BeadDetail{ID: "gd-chThread", Type: "channel", Status: "open", Labels: land,
		Fields: map[string]string{"name": "general", "channel_kind": "general", "slack_channel_id": "C0THREAD", "mirror": "both"}}
	daemon.beads["gd-chCrew"] = &beadsapi.BeadDetail{ID: "gd-chCrew", Type: "channel", Status: "open", Labels: land,
		Fields: map[string]string{"name": "crew", "channel_kind": "crew", "slack_channel_id": "C0CREW"}}
	daemon.beads["crew-thread"].Fields["thread_id"] = "gd-wishT"
	daemon.beads["crew-card"] = &beadsapi.BeadDetail{ID: "gd-agent-card", Type: "agent", Status: "closed", Title: "crew-card", Labels: land,
		Fields: map[string]string{"agent": "crew-card", "project": "gardenio"}}
	daemon.beads["gd-card-card"] = &beadsapi.BeadDetail{ID: "gd-card-card", Type: "message", Status: "closed", Labels: land,
		Fields: map[string]string{"kind": "card", "subject_id": "gd-agent-card", "channel_id": "gd-chCrew",
			"slack_channel": "C0CREW", "slack_ts": "1690000000.000300"}}
	return b, daemon
}

// noticeBead returns the one message bead whose notice field is kind.
func noticeBead(t *testing.T, d *mockDaemon, kind string) *beadsapi.BeadDetail {
	t.Helper()
	var found *beadsapi.BeadDetail
	for _, b := range messageBeads(d) {
		if b.Fields[noticeField] != kind {
			continue
		}
		if found != nil {
			t.Fatalf("two %s notices: %s and %s", kind, found.ID, b.ID)
		}
		found = b
	}
	if found == nil {
		t.Fatalf("no %s notice bead", kind)
	}
	return found
}

// A death report reaches the Commons: the bead carries the channel bead bound
// to its Slack channel, the wish head of the thread it replies in, the dead
// agent as its notice_subject (never subject_id, which the daemon refuses on a
// notice: gd-JGmXQuSndB) and the face's fields, all declared on type:message.
func TestDeathReportLandsInCommonsThread(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newNoticeBot(t, twin)
	if err := b.NotifyAgentDeath(context.Background(), BeadEvent{
		ID: "gd-agent-thread", Type: "agent", Assignee: "crew-thread", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"close_reason": "context_cancelled_involuntary"},
	}); err != nil {
		t.Fatal(err)
	}
	f := noticeBead(t, daemon, noticeDeath).Fields
	want := map[string]string{
		"channel_id": "gd-chThread", "thread_id": "gd-wishT", noticeSubjectField: "gd-agent-thread",
		"kind": "notice", noticeAgentField: "crew-thread", "slack_channel": "C0THREAD",
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("%s = %q, want %q (fields %v)", k, f[k], v, f)
		}
	}
	if f[noticeCauseField] == "" {
		t.Errorf("no cause on the death notice: %v", f)
	}
	assertMessageSchemaFields(t, f)
	if msgs := twin.Messages("C0THREAD"); len(msgs) != 1 {
		t.Fatalf("thread shows %v, want the one death report", msgs)
	}
}

// A crash notice threaded under an agent's card is a reply to the card bead in
// the Commons.
func TestCrashNoticeRepliesToAgentCard(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newNoticeBot(t, twin)
	if err := b.NotifyAgentCrash(context.Background(), BeadEvent{
		ID: "gd-agent-card", Type: "agent", Assignee: "crew-card", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent_state": "failed", "pod_phase": "failed", "pod_name": "crew-card-pod"},
	}); err != nil {
		t.Fatal(err)
	}
	f := noticeBead(t, daemon, noticeCrash).Fields
	want := map[string]string{
		"channel_id": "gd-chCrew", "thread_id": "gd-card-card", noticeSubjectField: "gd-agent-card",
		noticeAgentField: "crew-card", noticePodField: "crew-card-pod", noticePodPhaseField: "failed",
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("%s = %q, want %q (fields %v)", k, f[k], v, f)
		}
	}
	assertMessageSchemaFields(t, f)
	if msgs := twin.Messages("C0CREW"); len(msgs) != 1 {
		t.Fatalf("crew channel shows %v, want the crash notice", msgs)
	}
}

// When the daemon refuses a notice's bead the report still reaches Slack, but
// the miss is counted (gardenio_conversation_bead_write_failed_total), not
// left to a log line (gd-JGmXQuSndB).
func TestNoticeBeadWriteFailureIsCounted(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newNoticeBot(t, twin)
	daemon.createBeadHook = func(_ int, req beadsapi.CreateBeadRequest) error {
		if req.Type == "message" {
			return errors.New("refused")
		}
		return nil
	}
	failed := conversationBeadWriteFailedTotal.WithLabelValues("notice")
	before := testutil.ToFloat64(failed)
	if err := b.NotifyAgentCrash(context.Background(), BeadEvent{
		ID: "gd-agent-card", Type: "agent", Assignee: "crew-card", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent_state": "failed", "pod_phase": "failed"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(failed) - before; got != 1 {
		t.Errorf("bead write failures counted = %v, want 1", got)
	}
	if msgs := twin.Messages("C0CREW"); len(msgs) != 1 {
		t.Fatalf("crew channel shows %v, want the crash notice posted to Slack anyway", msgs)
	}
}

// A storm's held-back deaths roll up in the Commons once per land, in the
// land's #crew, counting only that land's deaths and never posting to Slack
// (Slack has its own fleet-wide roll-up). A land with no Commons gets none.
func TestDeathStormRollsUpPerLandInCommons(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newNoticeBot(t, twin)
	b.mu.Lock()
	b.deathReportWindowStart = time.Now().Add(-2 * deathStormWindow)
	b.deathReportSuppressed = 3
	b.deathReportHeld = []heldDeath{
		{land: "gardenio", subject: "gd-dead-a", name: "crew-a"},
		{land: "otherland", subject: "ot-dead-b", name: "other-b"},
		{land: "gardenio", subject: "gd-dead-c", name: "crew-c"},
	}
	b.mu.Unlock()
	if err := b.NotifyAgentDeath(context.Background(), BeadEvent{
		ID: "gd-agent-thread", Type: "agent", Assignee: "crew-thread", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"close_reason": "context_cancelled_involuntary"},
	}); err != nil {
		t.Fatal(err)
	}
	storm := noticeBead(t, daemon, noticeDeathStorm)
	f := storm.Fields
	if f["channel_id"] != "gd-chCrew" || f[noticeCountField] != "2" || f[noticeWindowField] != deathStormWindow.String() {
		t.Fatalf("storm roll-up fields = %v, want 2 deaths in #crew", f)
	}
	if f["slack_ts"] != "" || len(twin.Messages("C0CREW")) != 0 {
		t.Fatalf("the Commons roll-up reached Slack: fields %v, #crew shows %v", f, twin.Messages("C0CREW"))
	}
	if len(storm.Labels) != 1 || storm.Labels[0] != "project:gardenio" {
		t.Fatalf("labels = %v, want exactly project:gardenio", storm.Labels)
	}
	var dead []struct{ ID, Name string }
	if err := json.Unmarshal([]byte(f[noticeDeadField]), &dead); err != nil || len(dead) != 2 ||
		dead[0].ID != "gd-dead-a" || dead[1].Name != "crew-c" {
		t.Fatalf("notice_dead = %q (%v), want gardenio's two deaths", f[noticeDeadField], err)
	}
	assertMessageSchemaFields(t, f)
	if msgs := twin.Messages("C0OPS"); len(msgs) != 1 {
		t.Fatalf("ops channel shows %v, want the fleet-wide Slack roll-up", msgs)
	}
}

// The gate names the deaths it holds back, and hands them over on rollover.
func TestAdmitDeathReport_HandsOverHeldDeaths(t *testing.T) {
	bot := &Bot{}
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < deathStormBurst+2; i++ {
		bot.admitDeathReport(base, heldDeath{land: "gardenio", name: "crew-" + string(rune('a'+i))})
	}
	_, rollup, held := bot.admitDeathReport(base.Add(deathStormWindow), heldDeath{})
	if rollup != 2 || len(held) != 2 || held[0].name != "crew-f" || held[1].name != "crew-g" {
		t.Fatalf("rollup %d, held %v; want the two held-back deaths", rollup, held)
	}
}

// commonsChannel always projects to the Slack channel the bridge routed to,
// whatever (or however unset) the channel bead's mirror is: an unset mirror
// reads as off in the router, which would leave the Slack copy unposted.
func TestCommonsChannelAlwaysProjectsToSlack(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, _ := newNoticeBot(t, twin)
	ch := b.commonsChannel(context.Background(), "gardenio", "C0CREW")
	if ch.ID != "gd-chCrew" || ch.Slack != "C0CREW" || !conversation.MirrorsOut(ch.Mirror) {
		t.Fatalf("channel = %+v, want gd-chCrew projecting to C0CREW", ch)
	}
	if ch := b.commonsChannel(context.Background(), "gardenio", "C0NONE"); ch.ID != "" || ch.Slack != "C0NONE" {
		t.Fatalf("unbound channel = %+v, want Slack only", ch)
	}
}
