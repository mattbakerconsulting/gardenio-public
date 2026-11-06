package glass

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/coopmuxapi"
)

// fakeMuxFeed is coopmux's /ws/mux: one fakeMuxUp per dial.
type fakeMuxFeed struct {
	mu    sync.Mutex
	ups   []*fakeMuxUp
	dials atomic.Int32
	err   error
	ids   []coopmuxapi.Identity
}

type fakeMuxUp struct {
	ctx       context.Context
	in        chan coopmuxapi.Frame
	mu        sync.Mutex
	subs      map[string]bool // what the Wall is subscribed to on this connection
	closeOnce sync.Once
	closed    chan struct{}
}

func (m *fakeMuxFeed) dial(ctx context.Context) (MuxUpstream, error) {
	m.dials.Add(1)
	id, _ := coopmuxapi.IdentityFromContext(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ids = append(m.ids, id)
	if m.err != nil {
		return nil, m.err
	}
	u := &fakeMuxUp{ctx: ctx, in: make(chan coopmuxapi.Frame, 256), subs: map[string]bool{}, closed: make(chan struct{})}
	m.ups = append(m.ups, u)
	return u, nil
}

func (m *fakeMuxFeed) up(t *testing.T, n int) *fakeMuxUp {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		if len(m.ups) > n {
			u := m.ups[n]
			m.mu.Unlock()
			return u
		}
		m.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no dial #%d", n)
	return nil
}

func (u *fakeMuxUp) Recv() (coopmuxapi.Frame, error) {
	select {
	case f := <-u.in:
		return f, nil
	case <-u.ctx.Done():
		return coopmuxapi.Frame{}, u.ctx.Err()
	case <-u.closed:
		return coopmuxapi.Frame{}, io.EOF
	}
}

func (u *fakeMuxUp) SubscribeSessions(s ...string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, x := range s {
		u.subs[x] = true
	}
	return nil
}

func (u *fakeMuxUp) UnsubscribeSessions(s ...string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, x := range s {
		delete(u.subs, x)
	}
	return nil
}

func (u *fakeMuxUp) Close() error { u.closeOnce.Do(func() { close(u.closed) }); return nil }

func (u *fakeMuxUp) isClosed() bool {
	select {
	case <-u.closed:
		return true
	default:
		return false
	}
}

func (u *fakeMuxUp) emit(raw string) {
	var tag struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal([]byte(raw), &tag)
	u.in <- coopmuxapi.Frame{Event: tag.Event, Data: json.RawMessage(raw)}
}

// subscribed waits until the connection is subscribed to exactly want.
func (u *fakeMuxUp) subscribed(t *testing.T, want ...string) {
	t.Helper()
	slices.Sort(want)
	var got []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		got = got[:0]
		for s := range u.subs {
			got = append(got, s)
		}
		u.mu.Unlock()
		slices.Sort(got)
		if slices.Equal(got, want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscribed to %v, want %v", got, want)
}

func screenBatch(sessions ...string) string {
	var parts []string
	for i, s := range sessions {
		parts = append(parts, fmt.Sprintf(`{"session":%q,"lines":["$ %s"],"ansi":["\u001b[32m$\u001b[0m %s"],"cols":80,"rows":24,"seq":%d}`, s, s, s, i+1))
	}
	return `{"event":"screen_batch","screens":[` + strings.Join(parts, ",") + `]}`
}

// next waits for v's next events.
func next(t *testing.T, v *WallViewer) ([]Event, bool, bool) {
	t.Helper()
	select {
	case <-v.Ready():
		return v.Take()
	case <-time.After(2 * time.Second):
		t.Fatal("no wall events")
		return nil, false, false
	}
}

// until reads v's events until one of kind arrives, and returns it.
func until(t *testing.T, v *WallViewer, kind string) Event {
	t.Helper()
	for range 20 {
		evs, _, _ := next(t, v)
		for _, ev := range evs {
			if ev.Kind == kind {
				return ev
			}
		}
	}
	t.Fatalf("no %s event", kind)
	return Event{}
}

var (
	opAlice = coopmuxapi.Identity{Operator: "alice@example.com", AuthMode: "cookie", Roles: []string{"gardenio-operator"}}
	opBob   = coopmuxapi.Identity{Operator: "bob@example.com", AuthMode: "cookie", Roles: []string{"gardenio-operator"}}
)

// Two viewers share one /ws/mux upstream, subscribed to the union of what they may see; each one's tiles and screens hold
// only its own sessions, named by agent bead, and a session nobody may see is never subscribed.
func TestWallFiltersOneSharedUpstream(t *testing.T) {
	mux := &fakeMuxFeed{}
	w := NewWall(mux.dial, nil)
	a := w.Join(opAlice, map[string]string{"s1": "gd-a1"})
	defer a.Close()
	b := w.Join(opBob, map[string]string{"s2": "gd-b2"})
	defer b.Close()
	up := mux.up(t, 0)
	up.subscribed(t, "s1", "s2")
	up.emit(`{"event":"sessions","sessions":[{"id":"s1","state":"working"},{"id":"s2","state":"prompt"},{"id":"s3","state":"idle"}]}`)

	tiles := until(t, a, WallTiles)
	if s := string(tiles.Data); !strings.Contains(s, `"agent":"gd-a1"`) || strings.Contains(s, "gd-b2") || strings.Contains(s, `"s1"`) {
		t.Errorf("alice's tiles = %s", s)
	}
	if bt := string(until(t, b, WallTiles).Data); !strings.Contains(bt, `"agent":"gd-b2"`) || !strings.Contains(bt, `"attention":true`) {
		t.Errorf("bob's tiles = %s: his prompt tile should want attention", bt)
	}

	up.emit(screenBatch("s1", "s2", "s3"))
	as := string(until(t, a, WallScreens).Data)
	if !strings.Contains(as, `"agent":"gd-a1"`) || strings.Contains(as, "gd-b2") || strings.Contains(as, "$ s3") || !strings.Contains(as, `"ansi":true`) {
		t.Errorf("alice's screens = %s", as)
	}
	if bs := string(until(t, b, WallScreens).Data); !strings.Contains(bs, "gd-b2") || strings.Contains(bs, "gd-a1") {
		t.Errorf("bob's screens = %s", bs)
	}
	if n := mux.dials.Load(); n != 1 {
		t.Errorf("%d dials, want one shared upstream", n)
	}
	if got := mux.ids[0].Operator; got != "alice@example.com" && got != "bob@example.com" {
		t.Errorf("upstream signed for %q, want a human watching", got)
	}

	b.Close()
	up.subscribed(t, "s1")
}

// A transition moves the tile's state (rate_limited lifted out of error_category, Prompt and Error wanting attention); a
// session going offline drops its tile; one the viewer may not see coming online only tells the handler to look again,
// and Allow then answers with a fresh snapshot.
func TestWallStateOnlineOffline(t *testing.T) {
	mux := &fakeMuxFeed{}
	w := NewWall(mux.dial, nil)
	v := w.Join(opAlice, map[string]string{"s1": "gd-a1"})
	defer v.Close()
	up := mux.up(t, 0)
	up.emit(`{"event":"sessions","sessions":[{"id":"s1","state":"working"}]}`)
	until(t, v, WallTiles)

	up.emit(`{"event":"transition","session":"s1","prev":"working","next":"error","seq":4,"error_category":"rate_limited","error_detail":"429"}`)
	st := string(until(t, v, WallState).Data)
	for _, want := range []string{`"agent":"gd-a1"`, `"state":"error"`, `"rate_limited":true`, `"attention":true`} {
		if !strings.Contains(st, want) {
			t.Errorf("state = %s, want %s", st, want)
		}
	}

	up.emit(`{"event":"session:online","session":"s9","url":"http://x","metadata":{}}`)
	for {
		evs, unknown, _ := next(t, v)
		if len(evs) > 0 {
			t.Fatalf("a session the viewer may not see sent it %v", evs)
		}
		if unknown {
			break
		}
	}
	v.Allow(map[string]string{"s1": "gd-a1", "s9": "gd-a9"})
	if tl := string(until(t, v, WallTiles).Data); !strings.Contains(tl, "gd-a9") || !strings.Contains(tl, "gd-a1") {
		t.Errorf("tiles after Allow = %s", tl)
	}
	up.subscribed(t, "s1", "s9")

	up.emit(`{"event":"session:offline","session":"s1"}`)
	if off := string(until(t, v, WallOffline).Data); off != `{"agent":"gd-a1"}` {
		t.Errorf("offline = %s", off)
	}
}

// A viewer that never drains its queue is resynced with a snapshot rather than holding every batch: its queue stays small.
func TestWallSlowViewerGetsASnapshot(t *testing.T) {
	mux := &fakeMuxFeed{}
	w := NewWall(mux.dial, nil)
	slow := w.Join(opAlice, map[string]string{"s1": "gd-a1"})
	defer slow.Close()
	up := mux.up(t, 0)
	up.emit(`{"event":"sessions","sessions":[{"id":"s1","state":"working"}]}`)
	for i := 0; i < wallQueueMaxEvents+10; i++ {
		up.emit(screenBatch("s1"))
	}
	deadline := time.Now().Add(2 * time.Second)
	for slow.Resyncs() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if slow.Resyncs() == 0 {
		t.Fatal("a slow viewer was never resynced")
	}
	evs, _, _ := slow.Take()
	if len(evs) == 0 || len(evs) > wallQueueMaxEvents {
		t.Fatalf("slow viewer holds %d events", len(evs))
	}
	if evs[0].Kind != WallTiles {
		t.Errorf("a resync starts with a snapshot, got %s", evs[0].Kind)
	}
}

// The upstream lingers after the last viewer leaves and closes when nobody comes back; the next viewer dials afresh.
func TestWallLingersThenCloses(t *testing.T) {
	mux := &fakeMuxFeed{}
	w := NewWall(mux.dial, nil)
	w.Linger = 30 * time.Millisecond
	v := w.Join(opAlice, map[string]string{"s1": "gd-a1"})
	up := mux.up(t, 0)
	v.Close()
	deadline := time.Now().Add(2 * time.Second)
	for w.Open() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if w.Open() {
		t.Fatal("the upstream stayed open with nobody watching")
	}
	for !up.isClosed() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !up.isClosed() {
		t.Error("the connection was not closed")
	}
	v2 := w.Join(opAlice, map[string]string{"s1": "gd-a1"})
	defer v2.Close()
	mux.up(t, 1)
	if len(w.Subscribed()) != 1 {
		t.Errorf("subscribed = %v", w.Subscribed())
	}
}

// coopmux refusing the feed ends every viewer with why; nothing is retried in a loop.
func TestWallEndsWhenRefused(t *testing.T) {
	mux := &fakeMuxFeed{err: fmt.Errorf("%w: status 403", coopmuxapi.ErrUnauthorized)}
	w := NewWall(mux.dial, nil)
	v := w.Join(opAlice, map[string]string{"s1": "gd-a1"})
	defer v.Close()
	evs, _, finished := next(t, v)
	if !finished || len(evs) != 1 || evs[0].Kind != WallEnded || string(evs[0].Data) != `{"reason":"refused"}` {
		t.Fatalf("got %v finished=%v", evs, finished)
	}
	if w.Open() {
		t.Error("a refused Wall stays closed")
	}
}
