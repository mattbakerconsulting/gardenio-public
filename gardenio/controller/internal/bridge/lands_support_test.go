package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeSupportDaemon is an in-memory daemon for the #support watcher: it
// honours the create idempotency key the way kbeads does (lookup-or-return).
type fakeSupportDaemon struct {
	mu       sync.Mutex
	beads    map[string]*beadsapi.BeadDetail
	byKey    map[string]string
	comments map[string][]string
	creates  int
	projects map[string]beadsapi.ProjectInfo
	messages []*beadsapi.BeadDetail
	next     int
}

func newFakeSupportDaemon() *fakeSupportDaemon {
	d := &fakeSupportDaemon{
		beads: map[string]*beadsapi.BeadDetail{}, byKey: map[string]string{}, comments: map[string][]string{},
		projects: map[string]beadsapi.ProjectInfo{
			"tomato-patch": {Name: "tomato-patch", LandKind: "tenant"},
			"gardenio":     {Name: "gardenio"},
		},
	}
	for _, ch := range []struct{ id, land, name string }{
		{"ch-support", "tomato-patch", "support"},
		{"ch-general", "tomato-patch", "general"},
		{"ch-op-support", "gardenio", "support"},
	} {
		d.beads[ch.id] = &beadsapi.BeadDetail{ID: ch.id, Type: "channel", Labels: []string{"project:" + ch.land},
			Fields: map[string]string{"name": ch.name}}
	}
	return d
}

func (d *fakeSupportDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.beads[id]
	if !ok {
		return nil, errors.New("not found: " + id)
	}
	cp := *b
	cp.Fields = map[string]string{}
	for k, v := range b.Fields {
		cp.Fields[k] = v
	}
	return &cp, nil
}

func (d *fakeSupportDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.byKey[req.IdempotencyKey]; ok && req.IdempotencyKey != "" {
		return id, nil
	}
	d.next++
	d.creates++
	id := "gd-sup" + string(rune('0'+d.next))
	fields := map[string]string{}
	_ = json.Unmarshal(req.Fields, &fields)
	d.beads[id] = &beadsapi.BeadDetail{ID: id, Type: req.Type, Kind: req.Kind, Title: req.Title,
		Assignee: req.Assignee, Labels: req.Labels, Description: req.Description, Fields: fields, CreatedBy: req.CreatedBy}
	d.byKey[req.IdempotencyKey] = id
	return id, nil
}

func (d *fakeSupportDaemon) AddComment(_ context.Context, id, _, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.comments[id] = append(d.comments[id], text)
	return nil
}

func (d *fakeSupportDaemon) UpdateBeadFields(_ context.Context, id string, f map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range f {
		d.beads[id].Fields[k] = v
	}
	return nil
}

func (d *fakeSupportDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	var out []*beadsapi.BeadDetail
	for _, m := range d.messages {
		if hasLabel(m.Labels, q.Labels[0]) {
			out = append(out, m)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (d *fakeSupportDaemon) ListProjectBeads(context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return d.projects, nil
}

func (d *fakeSupportDaemon) supportBeads() []*beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if hasLabel(b.Labels, SupportLabel) {
			out = append(out, b)
		}
	}
	return out
}

type fakeDMs struct {
	mu   sync.Mutex
	to   []string
	text []string
	fail bool
}

func (f *fakeDMs) SendDM(_ context.Context, userID, text string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return "", "", errors.New("slack down")
	}
	f.to = append(f.to, userID)
	f.text = append(f.text, text)
	return "D1", "1.1", nil
}

func supportEvent(t *testing.T, id, land, channel, author, text, thread string) []byte {
	t.Helper()
	fields := map[string]string{"channel_id": channel, "author": author, "text": text, "kind": "text"}
	if thread != "" {
		fields["thread_id"] = thread
	}
	raw, _ := json.Marshal(fields)
	b, err := json.Marshal(map[string]any{"bead": map[string]any{
		"id": id, "type": "message", "status": "closed", "created_by": author,
		"labels": []string{"project:" + land}, "fields": json.RawMessage(raw),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newTestSupport(d *fakeSupportDaemon, dms *fakeDMs, operator string, pointers *[]string) *LandsSupport {
	return NewLandsSupport(LandsSupportConfig{
		Daemon: d, Sender: dms, Operator: operator,
		ResolveSlackID: func(_ context.Context, op string) string {
			if strings.Contains(op, "owner@example.com") {
				return "U0OWNER"
			}
			return ""
		},
		PostPointer: func(_ context.Context, text string) error {
			*pointers = append(*pointers, text)
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 10, 21, 23, 0, 0, 0, time.UTC) },
	})
}

func TestLandsSupportTopLevelPostFilesBeadAndDMs(t *testing.T) {
	d, dms, pointers := newFakeSupportDaemon(), &fakeDMs{}, []string{}
	s := newTestSupport(d, dms, "oidc:owner@example.com", &pointers)
	ctx := context.Background()

	s.handleEvent(ctx, supportEvent(t, "m1", "tomato-patch", "ch-support", "ann@tenant.example",
		"The purse is empty <again> & work parked\nsecond line", ""))
	// A redelivery (closed after created, or an SSE reconnect) changes nothing.
	s.handleEvent(ctx, supportEvent(t, "m1", "tomato-patch", "ch-support", "ann@tenant.example", "x", ""))

	beads := d.supportBeads()
	if len(beads) != 1 {
		t.Fatalf("support beads = %d, want 1", len(beads))
	}
	b := beads[0]
	if b.Type != "task" || b.Assignee != "owner@example.com" || !hasLabel(b.Labels, "project:tomato-patch") {
		t.Errorf("bead = type %q assignee %q labels %v", b.Type, b.Assignee, b.Labels)
	}
	if b.Title != "Support (tomato-patch): The purse is empty <again> & work parked" {
		t.Errorf("title = %q", b.Title)
	}
	if b.Fields[SupportMessageField] != "m1" || b.Fields[SupportChannelField] != "ch-support" {
		t.Errorf("fields = %v", b.Fields)
	}
	if b.Fields[SupportNotifiedAtField] != "2026-10-21T23:00:00Z" || b.Fields[SupportNotifiedVia] != "dm" {
		t.Errorf("stamp = %v", b.Fields)
	}
	if len(dms.to) != 1 || dms.to[0] != "U0OWNER" {
		t.Fatalf("DMs = %v", dms.to)
	}
	if !strings.Contains(dms.text[0], "land `tomato-patch` from ann@tenant.example") ||
		!strings.Contains(dms.text[0], ">The purse is empty &lt;again&gt; &amp; work parked\n>second line") {
		t.Errorf("DM text = %q", dms.text[0])
	}
	if len(pointers) != 0 {
		t.Errorf("pointers = %v, want none when the DM lands", pointers)
	}
}

func TestLandsSupportReplyCommentsOnTheThreadsBead(t *testing.T) {
	d, dms, pointers := newFakeSupportDaemon(), &fakeDMs{}, []string{}
	s := newTestSupport(d, dms, "owner@example.com", &pointers)
	ctx := context.Background()

	s.handleEvent(ctx, supportEvent(t, "m1", "tomato-patch", "ch-support", "ann@tenant.example", "help", ""))
	s.handleEvent(ctx, supportEvent(t, "m2", "tomato-patch", "ch-support", "owner@example.com", "on it", "m1"))
	s.handleEvent(ctx, supportEvent(t, "m3", "tomato-patch", "ch-support", "bob@tenant.example", "me too", "m1"))

	beads := d.supportBeads()
	if len(beads) != 1 {
		t.Fatalf("support beads = %d, want 1 per thread", len(beads))
	}
	got := d.comments[beads[0].ID]
	if len(got) != 1 || !strings.Contains(got[0], "bob@tenant.example replied") || !strings.Contains(got[0], "me too") {
		t.Errorf("comments = %q (the operator's own reply is not one)", got)
	}
	if len(dms.text) != 2 || !strings.Contains(dms.text[1], "*Reply* in land `tomato-patch` from bob@tenant.example") {
		t.Errorf("DMs = %q", dms.text)
	}
}

func TestLandsSupportIgnoresWhatIsNotARequest(t *testing.T) {
	d, dms, pointers := newFakeSupportDaemon(), &fakeDMs{}, []string{}
	s := newTestSupport(d, dms, "owner@example.com", &pointers)
	ctx := context.Background()

	s.handleEvent(ctx, supportEvent(t, "a", "tomato-patch", "ch-general", "ann@tenant.example", "hi", ""))    // not #support
	s.handleEvent(ctx, supportEvent(t, "b", "gardenio", "ch-op-support", "ann@tenant.example", "hi", ""))     // operator land
	s.handleEvent(ctx, supportEvent(t, "c", "tomato-patch", "ch-support", "oidc:Owner@Example.com", "x", "")) // the operator
	s.handleEvent(ctx, supportEvent(t, "e", "tomato-patch", "ch-op-support", "ann@tenant.example", "hi", "")) // a channel of another land

	if n := len(d.supportBeads()); n != 0 {
		t.Errorf("support beads = %d, want 0", n)
	}
	if len(dms.text)+len(pointers) != 0 {
		t.Errorf("told the operator %d times, want 0", len(dms.text)+len(pointers))
	}
}

func TestLandsSupportFallsBackToAPointerWithoutTenantText(t *testing.T) {
	d, dms, pointers := newFakeSupportDaemon(), &fakeDMs{fail: true}, []string{}
	s := newTestSupport(d, dms, "owner@example.com", &pointers)
	s.handleEvent(context.Background(), supportEvent(t, "m1", "tomato-patch", "ch-support", "ann@tenant.example", "secret repo detail", ""))

	if len(pointers) != 1 || strings.Contains(pointers[0], "secret repo detail") || !strings.Contains(pointers[0], "tomato-patch") {
		t.Fatalf("pointers = %q", pointers)
	}
	if v := d.supportBeads()[0].Fields[SupportNotifiedVia]; v != "channel" {
		t.Errorf("notified via %q, want channel", v)
	}
}

func TestLandsSupportSweepTellsOnlyUnnotifiedRequests(t *testing.T) {
	d, dms := newFakeSupportDaemon(), &fakeDMs{fail: true}
	s := NewLandsSupport(LandsSupportConfig{Daemon: d, Sender: dms, Operator: "owner@example.com",
		ResolveSlackID: func(context.Context, string) string { return "U0OWNER" }})
	ctx := context.Background()

	// Missed by SSE (the bridge was down), and the first DM fails: filed, not told.
	d.messages = []*beadsapi.BeadDetail{
		{ID: "m1", Type: "message", Labels: []string{"project:tomato-patch"},
			Fields: map[string]string{"channel_id": "ch-support", "author": "ann@tenant.example", "text": "help"}},
		{ID: "m2", Type: "message", Labels: []string{"project:tomato-patch"},
			Fields: map[string]string{"channel_id": "ch-support", "author": "bob@tenant.example", "text": "reply", "thread_id": "m1"}},
	}
	s.Sweep(ctx)
	if n := len(d.supportBeads()); n != 1 {
		t.Fatalf("support beads = %d, want 1", n)
	}
	if got := d.supportBeads()[0].Fields[SupportNotifiedAtField]; got != "" {
		t.Fatalf("stamped %q after a failed DM", got)
	}

	dms.fail = false
	s.Sweep(ctx)
	s.Sweep(ctx)
	if len(dms.text) != 1 {
		t.Errorf("DMs = %d, want exactly 1 across sweeps", len(dms.text))
	}
	if d.creates != 1 || len(d.comments) != 0 {
		t.Errorf("creates = %d comments = %v, want 1 and none (the sweep does not replay replies)", d.creates, d.comments)
	}
}

func TestLandsSupportWithoutOperatorStillFilesTheBead(t *testing.T) {
	d, pointers := newFakeSupportDaemon(), []string{}
	s := newTestSupport(d, nil, "", &pointers)
	s.sender = nil
	s.handleEvent(context.Background(), supportEvent(t, "m1", "tomato-patch", "ch-support", "ann@tenant.example", "help", ""))

	b := d.supportBeads()
	if len(b) != 1 || b[0].Assignee != "" {
		t.Fatalf("beads = %v", b)
	}
	if len(pointers) != 1 || !strings.Contains(pointers[0], "LANDS_SUPPORT_OPERATOR") {
		t.Errorf("pointers = %q", pointers)
	}
}
