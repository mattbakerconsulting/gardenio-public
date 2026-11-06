package server

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/landmap"
	"gardenio/kbeads/internal/model"
)

// D.9 gd-Z9VVGjvzBk: a client that falls sseClientBuffer behind is cut, so it
// reconnects with Last-Event-ID and replays from the ring instead of carrying
// on past a gap it cannot see.
func TestSSEHub_SlowClientIsCut(t *testing.T) {
	hub := newSSEHub()
	c := hub.subscribe(nil, "", sseClientScope{})
	defer hub.unsubscribe(c)
	for range sseClientBuffer {
		hub.broadcast("beads.bead.updated", []byte(`{}`), "", "", "", nil, nil)
	}
	select {
	case <-c.cut:
		t.Fatal("cut while within its buffer")
	default:
	}
	hub.broadcast("beads.bead.updated", []byte(`{}`), "", "", "", nil, nil)
	select {
	case <-c.cut:
	default:
		t.Fatal("not cut after overflowing its buffer")
	}
	if got := len(hub.eventsSince(0)); got != sseClientBuffer+1 {
		t.Errorf("ring holds %d events, want %d to replay", got, sseClientBuffer+1)
	}
}

// D.9 gd-Z9VVGjvzBk: the scope an event is delivered under is the client's
// scope at delivery. An event queued under the old scope fails the check
// handleEventStream makes before writing it, once a refresh narrowed it.
func TestSSEClient_ScopeAtDelivery(t *testing.T) {
	hub := newSSEHub()
	c := hub.subscribe(nil, "", sseClientScope{oidcAllowedProjects: []string{"acme"}, taintGate: beadTaintGateState{exempt: true}})
	defer hub.unsubscribe(c)
	hub.broadcast("beads.bead.updated", []byte(`{}`), "gd-1", "", "", []string{"project:acme"}, nil)
	evt := <-c.ch
	if !c.matchesScope(evt.BeadID, evt.BeadType, evt.Assignee, evt.Labels, evt.TaintIDs) {
		t.Fatal("in scope when queued, out of scope now")
	}
	c.setScope([]string{"zeta"}, beadTaintGateState{exempt: true})
	if c.matchesScope(evt.BeadID, evt.BeadType, evt.Assignee, evt.Labels, evt.TaintIDs) {
		t.Error("a queued event is still delivered after the scope narrowed")
	}
}

// userLookupFailingStore fails the user-bead lookup behind a caller's
// allowed projects (lookupSpawnPermissions) once fail is set.
type userLookupFailingStore struct {
	*mockStore
	fail atomic.Bool
}

func (s *userLookupFailingStore) ListBeads(ctx context.Context, f model.BeadFilter) ([]*model.Bead, int, error) {
	if s.fail.Load() && slices.Contains(f.Type, model.BeadType("user")) {
		return nil, 0, errors.New("store unavailable")
	}
	return s.mockStore.ListBeads(ctx, f)
}

// D.9 gd-Z9VVGjvzBk: a refresh tick whose permission lookup errors keeps the
// stream's previous scope. Read as oidcAllowedProjects reads it (fail open,
// nil), one transient error would make a scoped stream unrestricted.
func TestHandleEventStream_ScopeKeptWhenRefreshLookupErrors(t *testing.T) {
	prevInterval := sseTaintGateRefreshInterval
	sseTaintGateRefreshInterval = 20 * time.Millisecond
	defer func() { sseTaintGateRefreshInterval = prevInterval }()

	srv, ms, _ := newTestServer()
	seedUserBead(t, ms, "gd-user-scoped", map[string]any{"email": "scoped@example.com", "allowed_projects": []string{"acme"}})
	ms.beads["gd-user-scoped"].Kind = model.KindConfig
	fs := &userLookupFailingStore{mockStore: ms}
	srv.store = fs

	ctx, cancel := context.WithCancel(asOIDC(context.Background(), "scoped@example.com"))
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/events/stream", nil).WithContext(ctx)
	rec, done := startSSEStream(srv, req)
	time.Sleep(50 * time.Millisecond)

	// The first lookup (at subscribe) succeeded; every one after it errors.
	fs.fail.Store(true)
	time.Sleep(80 * time.Millisecond)
	srv.sseHub.broadcast("beads.bead.updated", []byte(`{"id":"out-of-scope"}`), "gd-zeta", "", "", []string{"project:zeta"}, nil)
	srv.sseHub.broadcast("beads.bead.updated", []byte(`{"id":"in-scope"}`), "gd-acme", "", "", []string{"project:acme"}, nil)
	time.Sleep(50 * time.Millisecond)

	cancel()
	<-done
	body := rec.Body.String()
	if strings.Contains(body, "out-of-scope") {
		t.Errorf("a failed refresh lookup widened the stream; got:\n%s", body)
	}
	if !strings.Contains(body, "in-scope") {
		t.Errorf("the stream lost its own project after a failed refresh lookup; got:\n%s", body)
	}
}

// The land feed's tap sees every broadcast as a landmap.Change: the topic
// mapped to its kind, the bead and its labels, never the payload.
func TestChangesForMap_TapsTheHub(t *testing.T) {
	hub := newSSEHub()
	var got []landmap.Change
	changesForMap{hub}.Tap(func(c landmap.Change) { got = append(got, c) })
	labels := []string{"project:acme"}
	hub.broadcast("beads.bead.created", []byte(`{}`), "gd-1", "", "", labels, nil)
	hub.broadcast("beads.comment.added", []byte(`{}`), "gd-1", "", "", labels, nil)
	hub.broadcast("beads.bead.closed", []byte(`{}`), "gd-1", "", "", labels, nil)
	hub.broadcast("beads.dependency.added", []byte(`{}`), "gd-1", "", "", labels, nil)
	want := []landmap.ChangeKind{landmap.ChangeCreated, landmap.ChangeUpdated, landmap.ChangeClosed, landmap.ChangeOther}
	if len(got) != len(want) {
		t.Fatalf("tap saw %d changes, want %d", len(got), len(want))
	}
	for i, c := range got {
		if c.Kind != want[i] || c.BeadID != "gd-1" || !slices.Equal(c.Labels, labels) {
			t.Errorf("change %d = %+v, want kind %d", i, c, want[i])
		}
	}
	changesForMap{}.Tap(func(landmap.Change) {}) // no hub: no tap, no panic
}

// TestLandsAPI_FeedThroughTheDaemon drives the land feed end to end: a land
// member's bearer through the full HTTP chain. A change filed under the
// member's land whose bead the member cannot read at delivery (it now sits
// in another land) is dropped; the land's own ticket streams as a contract
// Ticket.
func TestLandsAPI_FeedThroughTheDaemon(t *testing.T) {
	f := newScopedFixture(t)
	f.ms.beads[beadClaimlayer].Labels = []string{"project:claimlayer"}
	f.ms.beads[beadGardenio].Labels = []string{"project:gardenio"}
	f.ms.labels[beadClaimlayer] = []string{"project:claimlayer"}

	resp := oidcE2ERequest(t, f.ts, http.MethodGet, "/v1/lands/v1/lands/claimlayer/events", tokScopedWriter, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %v", resp.StatusCode, resp.Header)
	}
	if resp.Header.Get("X-Lands-Contract") == "" || resp.Header.Get("X-Kbeads-Version") != "" {
		t.Errorf("headers: %v", resp.Header)
	}

	// Labelled claimlayer when it was published (it has since moved): it is
	// read, as the member, in gardenio.
	f.ms.labels[beadGardenio] = []string{"project:gardenio"}
	f.srv.sseHub.broadcast(events.TopicBeadUpdated, []byte(`{}`), beadGardenio, "", "", []string{"project:claimlayer"}, nil)
	f.srv.broadcastEvent(context.Background(), events.TopicBeadUpdated, beadClaimlayer, map[string]string{"id": beadClaimlayer})

	frame := make(chan string, 1)
	go func() {
		var b strings.Builder
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() == "" && b.Len() > 0 {
				frame <- b.String()
				return
			}
			if !strings.HasPrefix(sc.Text(), ":") {
				b.WriteString(sc.Text() + "\n")
			}
		}
	}()
	select {
	case got := <-frame:
		if !strings.Contains(got, "event: ticket.updated\n") || !strings.Contains(got, `"id":"`+beadClaimlayer+`"`) {
			t.Errorf("first frame:\n%s\nwant claimlayer's ticket", got)
		}
		for _, leak := range []string{beadGardenio, "project:", `"labels"`, `"fields"`} {
			if strings.Contains(got, leak) {
				t.Errorf("frame carries %q:\n%s", leak, got)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
}
