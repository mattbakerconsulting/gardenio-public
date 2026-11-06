package conversation_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

// fakeReader is an in-memory conversation.ChannelReader over beads by id.
type fakeReader struct {
	beads   map[string]*beadsapi.BeadDetail
	listErr error
}

func newReader(beads ...*beadsapi.BeadDetail) *fakeReader {
	r := &fakeReader{beads: map[string]*beadsapi.BeadDetail{}}
	for _, b := range beads {
		r.beads[b.ID] = b
	}
	return r
}

func (r *fakeReader) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := r.beads[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("bead %s not found", id)
}

func (r *fakeReader) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	ids := make([]string, 0, len(r.beads))
	for id := range r.beads {
		ids = append(ids, id)
	}
	// Map order must not decide placement: hand them back in an order the
	// placement's own sort has to undo.
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	res := &beadsapi.ListBeadsResult{}
	for _, id := range ids {
		if b := r.beads[id]; match(b, q) {
			res.Beads = append(res.Beads, b)
		}
	}
	return res, nil
}

func channelBead(id, land, name, kind string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{"name": name, "channel_kind": kind}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{ID: id, Type: "channel", Status: "open", Labels: []string{"project:" + land}, Fields: f}
}

// operatorLand is a land seeded with the four default channels plus an infra
// room that takes the infra role's cards.
func operatorLand() []*beadsapi.BeadDetail {
	return []*beadsapi.BeadDetail{
		channelBead("gd-ch-general", "gardenio", "general", "general", map[string]string{"slack_channel_id": "C0GENERAL", "mirror": "both"}),
		channelBead("gd-ch-crew", "gardenio", "crew", "crew", map[string]string{"slack_channel_id": "C0CREW", "mirror": "both"}),
		channelBead("gd-ch-decisions", "gardenio", "decisions", "decisions", nil),
		channelBead("gd-ch-escalations", "gardenio", "escalations", "escalations", nil),
		channelBead("gd-ch-infra", "gardenio", "infra-room", "user", map[string]string{"channel_roles": `["infra","ops"]`, "slack_channel_id": "C0INFRA"}),
	}
}

func TestPlaceDefaultsBySubjectType(t *testing.T) {
	rd := newReader(operatorLand()...)
	cases := []struct {
		subject conversation.Subject
		want    string
	}{
		{conversation.Subject{ID: "gd-a1", Type: "agent", Land: "gardenio", Role: "crew"}, "gd-ch-crew"},
		{conversation.Subject{ID: "gd-d1", Type: "decision", Land: "gardenio"}, "gd-ch-decisions"},
		{conversation.Subject{ID: "gd-e1", Type: "escalation", Land: "gardenio"}, "gd-ch-escalations"},
		{conversation.Subject{ID: "gd-p1", Type: "permission", Land: "gardenio"}, "gd-ch-crew"},
	}
	for _, tc := range cases {
		p, err := conversation.Place(context.Background(), rd, tc.subject)
		if err != nil {
			t.Fatalf("Place(%s): %v", tc.subject.Type, err)
		}
		if p.Channel.ID != tc.want || p.Thread != nil || p.Rule != conversation.PlacedByDefault {
			t.Errorf("Place(%s) = %+v, want top-level in %s by default", tc.subject.Type, p, tc.want)
		}
	}
}

// TestPlaceAgentInMachines: in a land with #machines, an agent's card goes
// there by default (single voice); a decision, an escalation and a permission
// prompt do not move, and a channel_roles match still wins.
func TestPlaceAgentInMachines(t *testing.T) {
	beads := append(operatorLand(), channelBead("gd-ch-machines", "gardenio", "machines", "machines", nil))
	rd := newReader(beads...)
	cases := []struct {
		subject conversation.Subject
		want    string
	}{
		{conversation.Subject{ID: "gd-a1", Type: "agent", Land: "gardenio", Role: "crew"}, "gd-ch-machines"},
		{conversation.Subject{ID: "gd-a2", Type: "agent", Land: "gardenio", Role: "infra"}, "gd-ch-infra"},
		{conversation.Subject{ID: "gd-d1", Type: "decision", Land: "gardenio"}, "gd-ch-decisions"},
		{conversation.Subject{ID: "gd-e1", Type: "escalation", Land: "gardenio"}, "gd-ch-escalations"},
		{conversation.Subject{ID: "gd-p1", Type: "permission", Land: "gardenio"}, "gd-ch-crew"},
	}
	for _, tc := range cases {
		p, err := conversation.Place(context.Background(), rd, tc.subject)
		if err != nil {
			t.Fatalf("Place(%s): %v", tc.subject.ID, err)
		}
		if p.Channel.ID != tc.want {
			t.Errorf("Place(%s) = %s, want %s", tc.subject.ID, p.Channel.ID, tc.want)
		}
	}
}

func TestMachinesChannel(t *testing.T) {
	ctx := context.Background()
	if ch, err := conversation.MachinesChannel(ctx, newReader(operatorLand()...), "gardenio"); err != nil || ch != nil {
		t.Fatalf("MachinesChannel without one = %v, %v; want nil, nil", ch, err)
	}
	closed := channelBead("gd-ch-m0", "gardenio", "machines", "machines", nil)
	closed.Status = "closed"
	rd := newReader(append(operatorLand(),
		closed,
		channelBead("gd-ch-other", "other", "machines", "machines", nil),
		channelBead("gd-ch-m2", "gardenio", "backstage", "machines", nil),
		channelBead("gd-ch-m1", "gardenio", "machines", "machines", nil),
	)...)
	ch, err := conversation.MachinesChannel(ctx, rd, "gardenio")
	if err != nil || ch == nil || ch.ID != "gd-ch-m1" {
		t.Fatalf("MachinesChannel = %v, %v; want the open one named machines (gd-ch-m1)", ch, err)
	}
	if _, err := conversation.MachinesChannel(ctx, rd, ""); !errors.Is(err, conversation.ErrNoLand) {
		t.Errorf("MachinesChannel(\"\") error = %v, want ErrNoLand", err)
	}
}

func TestPlaceByChannelRoles(t *testing.T) {
	land := append(operatorLand(),
		// A second channel taking infra: the lower name wins, every time.
		channelBead("gd-ch-zz", "gardenio", "zz-infra", "user", map[string]string{"channel_roles": `["infra"]`}),
		// Archived and dm channels never take a role's cards.
		func() *beadsapi.BeadDetail {
			b := channelBead("gd-ch-old", "gardenio", "aa-old", "user", map[string]string{"channel_roles": `["infra"]`})
			b.Status = "closed"
			return b
		}(),
		channelBead("gd-ch-dm", "gardenio", "aa-dm", "dm", map[string]string{"channel_roles": `["infra"]`}),
	)
	rd := newReader(land...)
	p, err := conversation.Place(context.Background(), rd, conversation.Subject{ID: "gd-a1", Type: "agent", Land: "gardenio", Role: "infra"})
	if err != nil {
		t.Fatal(err)
	}
	want := conversation.ChannelRef{ID: "gd-ch-infra", Land: "gardenio", Slack: "C0INFRA"}
	if p.Channel != want || p.Rule != conversation.PlacedByRole {
		t.Errorf("Place(infra) = %+v, want %+v by channel_roles", p, want)
	}
}

func TestPlaceInBoundThread(t *testing.T) {
	head := &beadsapi.BeadDetail{
		ID: "gd-wish1", Type: "task", Labels: []string{"thread", "project:gardenio", "slack-thread:C0GENERAL:1792393553.000129"},
		Fields: map[string]string{"channel_id": "gd-ch-general", "slack_thread_channel": "C0GENERAL", "slack_thread_ts": "1792393553.000129"},
	}
	rd := newReader(append(operatorLand(), head)...)

	// Bound beats channel_roles: an infra agent spawned from a thread reports
	// in that thread.
	p, err := conversation.Place(context.Background(), rd, conversation.Subject{ID: "gd-a1", Type: "agent", Land: "gardenio", Role: "infra", ThreadID: "gd-wish1"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Rule != conversation.PlacedByThread || p.Channel.ID != "gd-ch-general" || p.Thread == nil || p.Thread.Head != "gd-wish1" {
		t.Fatalf("Place(bound) = %+v, want a reply in gd-wish1 in #general", p)
	}
	want := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: "C0GENERAL", TS: "1792393553.000129"}
	if got, ok := p.Thread.On(conversation.SurfaceSlack, p.Channel); !ok || got != want {
		t.Errorf("Slack root = %+v, want %+v", got, want)
	}
}

func TestPlaceInBoundThreadWithoutChannelID(t *testing.T) {
	// A head minted before channel_id was stamped: only the Slack label.
	head := &beadsapi.BeadDetail{ID: "gd-wish2", Type: "task", Labels: []string{"thread", "slack-thread:C0CREW:1.5"}, Fields: map[string]string{}}
	rd := newReader(append(operatorLand(), head)...)
	p, err := conversation.Place(context.Background(), rd, conversation.Subject{ID: "gd-a1", Type: "agent", Land: "gardenio", ThreadID: "gd-wish2"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Channel.ID != "gd-ch-crew" || p.Thread == nil || p.Thread.Refs[0].TS != "1.5" {
		t.Errorf("Place = %+v, want a reply in gd-wish2 in #crew (mapped by slack_channel_id)", p)
	}

	// A land with no Commons yet: the card still goes in the Slack thread.
	bare := newReader(head)
	p, err = conversation.Place(context.Background(), bare, conversation.Subject{ID: "gd-a1", Type: "agent", Land: "gardenio", ThreadID: "gd-wish2"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Channel != (conversation.ChannelRef{Land: "gardenio", Slack: "C0CREW"}) || p.Thread == nil {
		t.Errorf("Place(no Commons) = %+v, want the Slack thread with no channel bead", p)
	}
}

func TestPlaceIgnoresDanglingOrForeignThread(t *testing.T) {
	foreign := channelBead("gd-ch-other", "acme", "general", "general", nil)
	head := &beadsapi.BeadDetail{ID: "gd-wish3", Type: "task", Fields: map[string]string{"channel_id": "gd-ch-other"}}
	rd := newReader(append(operatorLand(), foreign, head)...)
	for _, thread := range []string{"gd-pruned", "gd-wish3"} {
		p, err := conversation.Place(context.Background(), rd, conversation.Subject{ID: "gd-a1", Type: "agent", Land: "gardenio", ThreadID: thread})
		if err != nil {
			t.Fatal(err)
		}
		if p.Channel.ID != "gd-ch-crew" || p.Thread != nil {
			t.Errorf("thread %s: Place = %+v, want #crew top-level (never another land's thread)", thread, p)
		}
	}
}

func TestPlaceErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := conversation.Place(ctx, newReader(), conversation.Subject{ID: "gd-a1", Type: "agent"}); !errors.Is(err, conversation.ErrNoLand) {
		t.Errorf("no land: err = %v, want ErrNoLand", err)
	}
	if _, err := conversation.Place(ctx, newReader(), conversation.Subject{ID: "gd-a1", Type: "agent", Land: "gardenio"}); !errors.Is(err, conversation.ErrUnplaced) {
		t.Errorf("no channels: err = %v, want ErrUnplaced", err)
	}
	broken := newReader()
	broken.listErr = errors.New("daemon down")
	if _, err := conversation.Place(ctx, broken, conversation.Subject{ID: "gd-a1", Type: "agent", Land: "gardenio"}); err == nil || errors.Is(err, conversation.ErrUnplaced) {
		t.Errorf("list failure: err = %v, want a lookup error, not ErrUnplaced", err)
	}
}

func TestSubjectFromBead(t *testing.T) {
	agent := &beadsapi.BeadDetail{ID: "gd-a1", Type: "agent", Fields: map[string]string{
		"project": "gardenio", "role": "crew", "slack_thread_bead_id": "gd-wish1",
	}}
	if got, want := conversation.SubjectFromBead(agent), (conversation.Subject{ID: "gd-a1", Type: "agent", Land: "gardenio", Role: "crew", ThreadID: "gd-wish1"}); got != want {
		t.Errorf("agent subject = %+v, want %+v", got, want)
	}
	decision := &beadsapi.BeadDetail{ID: "gd-d1", Type: "decision", Labels: []string{"project:gardenio"}, Fields: map[string]string{"role": "ignored"}}
	if got, want := conversation.SubjectFromBead(decision), (conversation.Subject{ID: "gd-d1", Type: "decision", Land: "gardenio"}); got != want {
		t.Errorf("decision subject = %+v, want %+v", got, want)
	}
}

// TestPostCardPlacesAndUpserts: a card placed in a thread is written as a reply
// in it, and posting it again re-renders the one card instead of a second.
func TestPostCardPlacesAndUpserts(t *testing.T) {
	r, store, slack, _ := newRouter(t)
	ctx := context.Background()
	p := conversation.Placement{
		Channel: mirrored,
		Thread:  &conversation.Thread{Head: "gd-wish1", Refs: []conversation.Ref{{Surface: conversation.SurfaceSlack, Channel: "C0CREW", TS: "1.5"}}},
		Rule:    conversation.PlacedByThread,
	}
	m := conversation.Message{SubjectID: "gd-a1", Text: "crew-1 is working", Author: author}
	d1, err := r.PostCard(ctx, p, m)
	if err != nil {
		t.Fatal(err)
	}
	card := store.beads[d1.BeadID]
	if card.Fields["kind"] != "card" || card.Fields["thread_id"] != "gd-wish1" || card.Fields["subject_id"] != "gd-a1" {
		t.Fatalf("card bead fields = %v", card.Fields)
	}
	d2, err := r.PostCard(ctx, p, m)
	if err != nil {
		t.Fatal(err)
	}
	if d2.BeadID != d1.BeadID || len(store.creates) != 1 {
		t.Errorf("second PostCard wrote %d beads (ids %s, %s), want the one card re-rendered", len(store.creates), d1.BeadID, d2.BeadID)
	}
	calls := slack.Calls()
	if len(calls) != 2 || calls[0].Op != "Reply" || calls[1].Op != "Update" {
		t.Errorf("slack calls = %+v, want reply then update", calls)
	}
}
