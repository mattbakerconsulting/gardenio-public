package conversation_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"
)

// fakeStore is an in-memory conversation.Store.
type fakeStore struct {
	mu        sync.Mutex
	beads     map[string]*beadsapi.BeadDetail
	creates   []beadsapi.CreateBeadRequest
	seq       int
	createErr error
	updateErr error
}

func newFakeStore() *fakeStore { return &fakeStore{beads: map[string]*beadsapi.BeadDetail{}} }

func (s *fakeStore) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return "", s.createErr
	}
	s.creates = append(s.creates, req)
	// A non-string field (a channel's members) comes back as its JSON text,
	// as the daemon client hands it back.
	fields := map[string]string{}
	if len(req.Fields) > 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(req.Fields, &raw); err != nil {
			return "", err
		}
		for k, v := range raw {
			var str string
			if json.Unmarshal(v, &str) == nil {
				fields[k] = str
			} else {
				fields[k] = string(v)
			}
		}
	}
	s.seq++
	id := fmt.Sprintf("gd-msg%d", s.seq)
	s.beads[id] = &beadsapi.BeadDetail{ID: id, Type: req.Type, Status: req.Status, Title: req.Title, Labels: req.Labels, Fields: fields}
	return id, nil
}

func (s *fakeStore) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateErr != nil {
		return s.updateErr
	}
	b, ok := s.beads[id]
	if !ok {
		return fmt.Errorf("no bead %s", id)
	}
	for k, v := range fields {
		b.Fields[k] = v
	}
	return nil
}

func (s *fakeStore) UpdateBead(_ context.Context, id string, req beadsapi.UpdateBeadRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.beads[id]
	if !ok {
		return fmt.Errorf("no bead %s", id)
	}
	if req.Status != nil {
		b.Status = *req.Status
	}
	return nil
}

func (s *fakeStore) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.beads[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("no bead %s", id)
}

func (s *fakeStore) CloseBead(_ context.Context, id string, _ map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.beads[id]
	if !ok {
		return fmt.Errorf("no bead %s", id)
	}
	b.Status = "closed"
	return nil
}

func (s *fakeStore) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &beadsapi.ListBeadsResult{}
	for i := 1; i <= s.seq; i++ {
		b, ok := s.beads[fmt.Sprintf("gd-msg%d", i)]
		if !ok || !match(b, q) {
			continue
		}
		res.Beads = append(res.Beads, b)
		if q.Limit > 0 && len(res.Beads) == q.Limit {
			break
		}
	}
	res.Total = len(res.Beads)
	return res, nil
}

func match(b *beadsapi.BeadDetail, q beadsapi.ListBeadsQuery) bool {
	if len(q.Types) > 0 && b.Type != q.Types[0] {
		return false
	}
	// A bead created with no status is open, as the daemon makes it.
	if status := cmp.Or(b.Status, "open"); len(q.Statuses) > 0 && !slices.Contains(q.Statuses, status) {
		return false
	}
	for k, v := range q.Fields {
		if b.Fields[k] != v {
			return false
		}
	}
	for _, l := range q.Labels {
		found := false
		for _, have := range b.Labels {
			found = found || have == l
		}
		if !found {
			return false
		}
	}
	return true
}

var (
	mirrored = conversation.ChannelRef{ID: "gd-chan-crew", Land: "gardenio", Slack: "C0CREW", Mirror: conversation.MirrorBoth}
	author   = conversation.Principal{ID: "agent:crew-1"}
)

func newRouter(t *testing.T) (*conversation.Router, *fakeStore, *surfacetest.Recorder, *surfacetest.Recorder) {
	t.Helper()
	store := newFakeStore()
	slack := surfacetest.NewRecorder(conversation.SurfaceSlack)
	commons := surfacetest.NewRecorder(conversation.SurfaceCommons)
	return conversation.NewRouter(store, nil, slack, commons), store, slack, commons
}

func TestRouterPostWritesBeadThenProjects(t *testing.T) {
	r, store, slack, commons := newRouter(t)
	d, err := r.Post(context.Background(), mirrored, conversation.Message{Text: "hello land", Author: author})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(store.creates) != 1 {
		t.Fatalf("bead creates = %d, want 1", len(store.creates))
	}
	req := store.creates[0]
	if req.Type != "message" || req.Status != "closed" || req.CreatedBy != "agent:crew-1" {
		t.Fatalf("bead = type %q status %q by %q, want a closed message by the author", req.Type, req.Status, req.CreatedBy)
	}
	if len(req.Labels) != 1 || req.Labels[0] != "project:gardenio" {
		t.Fatalf("labels = %v, want exactly one project label", req.Labels)
	}
	bead := store.beads[d.BeadID]
	if bead.Fields["channel_id"] != "gd-chan-crew" || bead.Fields["kind"] != "text" || bead.Fields["author"] != "agent:crew-1" {
		t.Fatalf("fields = %v", bead.Fields)
	}
	if len(slack.CallsOf("Post")) != 1 || len(commons.CallsOf("Post")) != 1 {
		t.Fatalf("posts: slack %d commons %d, want 1 each", len(slack.CallsOf("Post")), len(commons.CallsOf("Post")))
	}
	ref, ok := d.Ref(conversation.SurfaceSlack)
	if !ok || ref.Channel != "C0CREW" {
		t.Fatalf("slack ref = %+v (%v)", ref, ok)
	}
	if bead.Fields["slack_channel"] != "C0CREW" || bead.Fields["slack_ts"] != ref.TS {
		t.Fatalf("slack ref not stamped on bead: %v", bead.Fields)
	}
	if _, ok := bead.Fields["commons_ts"]; ok {
		t.Fatal("commons ref stamped; the bead id is the commons ref")
	}
	if slack.CallsOf("Post")[0].Message.BeadID != d.BeadID {
		t.Fatal("surface was not told the bead id it projects")
	}
}

// A notice's face is part of the record: Detail and Footnote go on the bead
// beside the fallback Text, and the surface gets the whole face.
func TestRouterNoticeKeepsItsFace(t *testing.T) {
	r, store, slack, _ := newRouter(t)
	m := conversation.Message{
		Kind: conversation.KindNotice, Text: "Agent died: crew-1", Detail: "*Agent died: crew-1*\n> Cause: spot",
		Footnote: "Forensics on bead `gd-1`", Rich: true,
	}
	d, err := r.Post(context.Background(), mirrored, m)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	f := store.beads[d.BeadID].Fields
	if f["kind"] != "notice" || f["text"] != m.Text || f["detail"] != m.Detail || f["footnote"] != m.Footnote {
		t.Fatalf("fields = %v, want the notice face", f)
	}
	got := slack.CallsOf("Post")[0].Message
	if got.Detail != m.Detail || got.Footnote != m.Footnote || !got.Rich {
		t.Fatalf("surface got %+v, want the notice face", got)
	}
}

func TestRouterBinding(t *testing.T) {
	cases := []struct {
		name          string
		ch            conversation.ChannelRef
		origin        string
		slack, common int
	}{
		{"mirrored", mirrored, "", 1, 1},
		{"mirror off", conversation.ChannelRef{ID: "c", Land: "l", Slack: "C1", Mirror: conversation.MirrorOff}, "", 0, 1},
		{"mirror out", conversation.ChannelRef{ID: "c", Land: "l", Slack: "C1", Mirror: conversation.MirrorOut}, "", 1, 1},
		{"mirror unset is off", conversation.ChannelRef{ID: "c", Land: "l", Slack: "C1"}, "", 0, 1},
		{"commons only", conversation.ChannelRef{ID: "c", Land: "l"}, "", 0, 1},
		{"no commons yet", conversation.ChannelRef{Land: "l", Slack: "C1", Mirror: conversation.MirrorOff}, "", 1, 0},
		{"from slack", mirrored, conversation.SurfaceSlack, 0, 1},
		{"from commons", mirrored, conversation.SurfaceCommons, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _, slack, commons := newRouter(t)
			if _, err := r.Post(context.Background(), tc.ch, conversation.Message{Text: "x", Origin: tc.origin}); err != nil {
				t.Fatalf("Post: %v", err)
			}
			if got := len(slack.Calls()); got != tc.slack {
				t.Errorf("slack calls = %d, want %d", got, tc.slack)
			}
			if got := len(commons.Calls()); got != tc.common {
				t.Errorf("commons calls = %d, want %d", got, tc.common)
			}
		})
	}
}

func TestRouterRefusesLandlessAndEmpty(t *testing.T) {
	r, store, _, _ := newRouter(t)
	ctx := context.Background()
	if _, err := r.Post(ctx, conversation.ChannelRef{ID: "c", Slack: "C1"}, conversation.Message{Text: "x"}); !errors.Is(err, conversation.ErrNoLand) {
		t.Fatalf("landless: err = %v, want ErrNoLand", err)
	}
	if _, err := r.Post(ctx, mirrored, conversation.Message{Text: " "}); !errors.Is(err, conversation.ErrEmptyMessage) {
		t.Fatalf("empty: err = %v, want ErrEmptyMessage", err)
	}
	if _, err := r.Post(ctx, mirrored, conversation.Message{Kind: conversation.KindCard, Text: "x"}); !errors.Is(err, conversation.ErrEmptyMessage) {
		t.Fatalf("subjectless card: err = %v, want ErrEmptyMessage", err)
	}
	if len(store.creates) != 0 {
		t.Fatalf("refused messages wrote %d beads", len(store.creates))
	}
}

func TestRouterExistingBeadOnlyProjects(t *testing.T) {
	r, store, slack, _ := newRouter(t)
	store.beads["gd-squawk"] = &beadsapi.BeadDetail{ID: "gd-squawk", Fields: map[string]string{}}
	d, err := r.Post(context.Background(), mirrored, conversation.Message{BeadID: "gd-squawk", Text: "from gb say"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(store.creates) != 0 || d.BeadID != "gd-squawk" {
		t.Fatalf("creates = %d, bead = %s; want the existing bead projected, none written", len(store.creates), d.BeadID)
	}
	if len(slack.CallsOf("Post")) != 1 || store.beads["gd-squawk"].Fields["slack_ts"] == "" {
		t.Fatal("existing bead was not projected and stamped")
	}
}

func TestRouterSurfaceFailureNeverFailsTheWrite(t *testing.T) {
	r, store, slack, commons := newRouter(t)
	boom := errors.New("slack down")
	slack.Fail("Post", boom)
	d, err := r.Post(context.Background(), mirrored, conversation.Message{Text: "still recorded"})
	if err != nil {
		t.Fatalf("Post returned %v; a surface failure must not fail the bead write", err)
	}
	if d.BeadID == "" || len(store.creates) != 1 {
		t.Fatal("bead not written")
	}
	if !errors.Is(d.Failed[conversation.SurfaceSlack], boom) {
		t.Fatalf("Failed = %v, want slack: %v", d.Failed, boom)
	}
	if len(commons.CallsOf("Post")) != 1 {
		t.Fatal("one surface failing stopped the other")
	}
	if _, ok := store.beads[d.BeadID].Fields["slack_ts"]; ok {
		t.Fatal("failed projection stamped a ref")
	}
}

func TestRouterBeadWriteFailureProjectsNothing(t *testing.T) {
	r, store, slack, commons := newRouter(t)
	store.createErr = errors.New("daemon down")
	if _, err := r.Post(context.Background(), mirrored, conversation.Message{Text: "x"}); err == nil {
		t.Fatal("Post succeeded with the daemon down")
	}
	if len(slack.Calls())+len(commons.Calls()) != 0 {
		t.Fatal("projected a message that has no bead")
	}
}

func TestRouterRefStampFailureIsReported(t *testing.T) {
	r, store, _, _ := newRouter(t)
	store.updateErr = errors.New("patch failed")
	d, err := r.Post(context.Background(), mirrored, conversation.Message{Text: "x"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if d.Failed[conversation.SurfaceSlack] == nil {
		t.Fatalf("lost slack ref not reported: %v", d.Failed)
	}
}

func TestRouterReply(t *testing.T) {
	r, store, slack, commons := newRouter(t)
	th := conversation.Thread{Head: "gd-head", Refs: []conversation.Ref{{Surface: conversation.SurfaceSlack, Channel: "C0CREW", TS: "1.000001"}}}
	d, err := r.Reply(context.Background(), mirrored, th, conversation.Message{Text: "in thread"}, true)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if store.beads[d.BeadID].Fields["thread_id"] != "gd-head" {
		t.Fatalf("thread_id not stamped: %v", store.beads[d.BeadID].Fields)
	}
	sc := slack.CallsOf("Reply")
	if len(sc) != 1 || sc[0].At.TS != "1.000001" || !sc[0].Broadcast {
		t.Fatalf("slack replies = %+v", sc)
	}
	cc := commons.CallsOf("Reply")
	if len(cc) != 1 || cc[0].At != (conversation.Ref{Surface: conversation.SurfaceCommons, Channel: "gd-chan-crew", TS: "gd-head"}) {
		t.Fatalf("commons replies = %+v, want the head bead as root", cc)
	}
}

func TestRouterReplyWithoutSlackRootFailsOnlySlack(t *testing.T) {
	r, _, slack, commons := newRouter(t)
	d, err := r.Reply(context.Background(), mirrored, conversation.Thread{Head: "gd-head"}, conversation.Message{Text: "x"}, false)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if !errors.Is(d.Failed[conversation.SurfaceSlack], conversation.ErrNoRef) || len(slack.Calls()) != 0 {
		t.Fatalf("slack: failed=%v calls=%d, want ErrNoRef and no call", d.Failed, len(slack.Calls()))
	}
	if len(commons.CallsOf("Reply")) != 1 {
		t.Fatal("commons reply missing")
	}
	if _, err := r.Reply(context.Background(), mirrored, conversation.Thread{}, conversation.Message{Text: "x"}, false); !errors.Is(err, conversation.ErrNoRef) {
		t.Fatalf("Reply with no thread: err = %v, want ErrNoRef", err)
	}
}

func TestRouterCardIsUpsertedNeverReposted(t *testing.T) {
	r, store, slack, _ := newRouter(t)
	ctx := context.Background()
	card := conversation.Message{Kind: conversation.KindCard, SubjectID: "gd-agent1", Text: "crew-1 working"}

	first, err := r.Post(ctx, mirrored, card)
	if err != nil {
		t.Fatalf("first Post: %v", err)
	}
	if store.creates[0].IdempotencyKey != "card:gd-agent1:gd-chan-crew" {
		t.Fatalf("idempotency key = %q", store.creates[0].IdempotencyKey)
	}
	card.Text = "crew-1 done"
	second, err := r.Post(ctx, mirrored, card)
	if err != nil {
		t.Fatalf("second Post: %v", err)
	}
	if len(store.creates) != 1 || second.BeadID != first.BeadID {
		t.Fatalf("second post wrote a new card bead (%d creates)", len(store.creates))
	}
	if len(slack.CallsOf("Post")) != 1 || len(slack.CallsOf("Update")) != 1 {
		t.Fatalf("slack posts %d updates %d, want 1 and 1 (D1: one card, rewritten in place)",
			len(slack.CallsOf("Post")), len(slack.CallsOf("Update")))
	}
	ref, _ := first.Ref(conversation.SurfaceSlack)
	if got, _ := slack.Lookup(ref); got.Text != "crew-1 done" {
		t.Fatalf("card shows %q after update", got.Text)
	}

	// The same subject in another channel is a second card.
	other := conversation.ChannelRef{ID: "gd-chan-dm", Land: "gardenio", Slack: "D0OP", Mirror: conversation.MirrorBoth}
	if _, err := r.Post(ctx, other, card); err != nil {
		t.Fatalf("Post in other channel: %v", err)
	}
	if len(store.creates) != 2 {
		t.Fatalf("creates = %d, want a second card bead for the second channel", len(store.creates))
	}
}

func TestRouterUpdateCardsRewritesEveryCopy(t *testing.T) {
	r, _, slack, commons := newRouter(t)
	ctx := context.Background()
	card := conversation.Message{Kind: conversation.KindCard, SubjectID: "gd-dec1", Text: "decide"}
	if _, err := r.Post(ctx, mirrored, card); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Post(ctx, conversation.ChannelRef{ID: "gd-chan-dm", Land: "gardenio", Slack: "D0OP", Mirror: conversation.MirrorBoth}, card); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Post(ctx, mirrored, conversation.Message{Kind: conversation.KindCard, SubjectID: "gd-other", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	d, err := r.UpdateCards(ctx, "gd-dec1")
	if err != nil {
		t.Fatalf("UpdateCards: %v", err)
	}
	if len(d.Failed) != 0 {
		t.Fatalf("UpdateCards failed: %v", d.Failed)
	}
	su, cu := slack.CallsOf("Update"), commons.CallsOf("Update")
	if len(su) != 2 || len(cu) != 2 {
		t.Fatalf("updates: slack %d commons %d, want 2 each (both cards of the subject, nothing else)", len(su), len(cu))
	}
	for _, c := range append(su, cu...) {
		if c.Message.SubjectID != "gd-dec1" || c.Message.Kind != conversation.KindCard {
			t.Fatalf("update carried %+v", c.Message)
		}
	}
	if _, err := r.UpdateCards(ctx, ""); err == nil {
		t.Fatal("UpdateCards with no subject succeeded")
	}
}

func TestRouterUpdateCardUpsertsTheBeadAtTheCopy(t *testing.T) {
	r, store, slack, commons := newRouter(t)
	ctx := context.Background()
	// A copy the handler posted before card beads existed.
	at, err := slack.Post(ctx, mirrored, conversation.Message{Kind: conversation.KindCard, SubjectID: "gd-agent1", Text: "crew-1"})
	if err != nil {
		t.Fatal(err)
	}
	card := conversation.Message{SubjectID: "gd-agent1", Text: "crew-1 working", Author: author}
	th := &conversation.Thread{Head: "gd-head1"}

	d, err := r.UpdateCard(ctx, mirrored, th, card, at)
	if err != nil || len(d.Failed) != 0 {
		t.Fatalf("UpdateCard: %v %v", err, d.Failed)
	}
	if len(store.creates) != 1 || d.BeadID == "" {
		t.Fatalf("creates = %d, bead %q: want the card bead written once", len(store.creates), d.BeadID)
	}
	b := store.beads[d.BeadID]
	if b.Fields["kind"] != "card" || b.Fields["subject_id"] != "gd-agent1" || b.Fields["thread_id"] != "gd-head1" ||
		b.Fields["slack_channel"] != at.Channel || b.Fields["slack_ts"] != at.TS || b.Fields["channel_id"] != mirrored.ID {
		t.Fatalf("card bead fields = %v", b.Fields)
	}
	if len(slack.CallsOf("Post")) != 1 || len(slack.CallsOf("Update")) != 1 || len(commons.Calls()) != 0 {
		t.Fatalf("slack posts %d updates %d, commons %d: want the copy edited in place and nothing else",
			len(slack.CallsOf("Post")), len(slack.CallsOf("Update")), len(commons.Calls()))
	}
	if got, _ := slack.Lookup(at); got.Text != "crew-1 working" {
		t.Fatalf("card shows %q", got.Text)
	}

	// Again: the bead is found, not re-written; a moved copy is re-stamped.
	if d2, _ := r.UpdateCard(ctx, mirrored, th, card, at); d2.BeadID != d.BeadID || len(store.creates) != 1 {
		t.Fatalf("second UpdateCard wrote a bead (%d creates, bead %q)", len(store.creates), d2.BeadID)
	}
	moved, _ := slack.Post(ctx, mirrored, conversation.Message{Text: "x"})
	if _, err := r.UpdateCard(ctx, mirrored, th, card, moved); err != nil {
		t.Fatal(err)
	}
	if b.Fields["slack_ts"] != moved.TS {
		t.Fatalf("slack_ts = %q, want the moved copy %q", b.Fields["slack_ts"], moved.TS)
	}

	// A known bead (BeadID set) only re-renders.
	card.BeadID = d.BeadID
	before := len(slack.CallsOf("Update"))
	if _, err := r.UpdateCard(ctx, mirrored, th, card, moved); err != nil {
		t.Fatal(err)
	}
	if len(store.creates) != 1 || len(slack.CallsOf("Update")) != before+1 {
		t.Fatal("UpdateCard with a known bead did more than re-render")
	}
}

func TestRouterUpdateCardDegrades(t *testing.T) {
	r, store, slack, _ := newRouter(t)
	ctx := context.Background()
	at, _ := slack.Post(ctx, mirrored, conversation.Message{Text: "crew-1"})
	card := conversation.Message{SubjectID: "gd-agent1", Text: "crew-1 done"}

	store.createErr = errors.New("daemon down")
	d, err := r.UpdateCard(ctx, mirrored, nil, card, at)
	if err != nil || d.BeadID != "" {
		t.Fatalf("UpdateCard with the daemon down: %v, bead %q", err, d.BeadID)
	}
	if got, _ := slack.Lookup(at); got.Text != "crew-1 done" {
		t.Fatal("a failed bead write stopped the re-render")
	}

	slack.Fail("Update", errors.New("rate limited"))
	store.createErr = nil
	d, err = r.UpdateCard(ctx, mirrored, nil, card, at)
	if err != nil || d.Failed[conversation.SurfaceSlack] == nil {
		t.Fatalf("surface failure: err %v, failed %v", err, d.Failed)
	}

	for name, tc := range map[string]struct {
		ch conversation.ChannelRef
		m  conversation.Message
		at conversation.Ref
	}{
		"no land":    {conversation.ChannelRef{Slack: "C0CREW"}, card, at},
		"no subject": {mirrored, conversation.Message{Text: "x"}, at},
		"no ref":     {mirrored, card, conversation.Ref{Surface: conversation.SurfaceSlack}},
		"no surface": {mirrored, card, conversation.Ref{Surface: "irc", Channel: "#x", TS: "1"}},
	} {
		if _, err := r.UpdateCard(ctx, tc.ch, nil, tc.m, tc.at); err == nil {
			t.Errorf("%s: UpdateCard succeeded", name)
		}
	}
}

func TestRouterSurfaceLookup(t *testing.T) {
	r, _, slack, _ := newRouter(t)
	s, ok := r.Surface(conversation.SurfaceSlack)
	if !ok || s != conversation.Surface(slack) {
		t.Fatal("Surface(slack) did not return the registered surface")
	}
	if _, ok := r.Surface("nope"); ok {
		t.Fatal("Surface found an unregistered name")
	}
}

func TestModalValidate(t *testing.T) {
	if err := surfacetest.ContractModal().Validate(); err != nil {
		t.Fatalf("contract modal invalid: %v", err)
	}
	bad := []func(*conversation.Modal){
		func(m *conversation.Modal) { m.Title = "" },
		func(m *conversation.Modal) { m.Fields[2].ID = "" },
		func(m *conversation.Modal) { m.Fields[3].Options = nil },
		func(m *conversation.Modal) { m.Fields[3].Initial = "z" },
		func(m *conversation.Modal) { m.Fields[0].Kind = "slider" },
	}
	for i, mutate := range bad {
		m := surfacetest.ContractModal()
		mutate(&m)
		if err := m.Validate(); !errors.Is(err, conversation.ErrBadModal) {
			t.Errorf("case %d: err = %v, want ErrBadModal", i, err)
		}
	}
}

// shareSurface answers a post like a file share: a copy with no message ts.
type shareSurface struct{ *surfacetest.Recorder }

func (s shareSurface) Post(ctx context.Context, ch conversation.ChannelRef, m conversation.Message) (conversation.Ref, error) {
	if _, err := s.Recorder.Post(ctx, ch, m); err != nil {
		return conversation.Ref{}, err
	}
	return conversation.Ref{Surface: s.Name(), Channel: ch.Slack}, nil
}

// A files-only message is titled by its files, and a copy with no ts (a Slack
// file share) is delivered but leaves no ref to edit on the bead.
func TestRouterFileShare(t *testing.T) {
	store := newFakeStore()
	r := conversation.NewRouter(store, nil, shareSurface{surfacetest.NewRecorder(conversation.SurfaceSlack)})
	d, err := r.Post(context.Background(), mirrored, conversation.Message{Author: author, Files: []conversation.File{
		{Name: "a.png", Data: []byte("A")}, {Name: "b.png", Data: []byte("B")},
	}})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if got := store.creates[0].Title; got != "Uploaded a.png, b.png" {
		t.Fatalf("title = %q, want the file names", got)
	}
	if _, ok := d.Ref(conversation.SurfaceSlack); !ok || len(d.Failed) != 0 {
		t.Fatalf("delivery = %+v, want the Slack copy", d)
	}
	if f := store.beads[d.BeadID].Fields; f["slack_ts"] != "" || f["slack_channel"] != "" {
		t.Fatalf("fields = %v, want no Slack ref stamped", f)
	}
}
