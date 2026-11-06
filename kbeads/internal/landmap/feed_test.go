package landmap

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"gardenio/lands/contract/landsv1"
)

// fakeChanges is the daemon's event stream: publish runs every tap, as the
// SSE hub's broadcast does.
type fakeChanges struct {
	mu   sync.Mutex
	taps []func(Change)
}

func (c *fakeChanges) Tap(fn func(Change)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.taps = append(c.taps, fn)
}

func (c *fakeChanges) publish(kind ChangeKind, id string, labels ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, fn := range c.taps {
		fn(Change{Kind: kind, BeadID: id, Labels: labels})
	}
}

func newFeedFixture(t *testing.T) (*fakeDaemon, *fakeChanges, *Map, http.Handler) {
	t.Helper()
	return newFeedFixtureWith(t, fakeLands{})
}

func newFeedFixtureWith(t *testing.T, lands fakeLands) (*fakeDaemon, *fakeChanges, *Map, http.Handler) {
	t.Helper()
	d, _ := newMapFixture(t, lands)
	c := &fakeChanges{}
	m := New(d.mux, lands, c)
	mux := http.NewServeMux()
	Register(mux, m)
	return d, c, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kbeads-Version", "v-test")
		mux.ServeHTTP(w, r)
	})
}

// streamWriter is a flushable ResponseWriter a test can read while the
// handler is still writing.
type streamWriter struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    bytes.Buffer
	started chan struct{}
}

func (s *streamWriter) Header() http.Header { return s.header }

func (s *streamWriter) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == 0 {
		s.status = code
		close(s.started)
	}
}

func (s *streamWriter) Write(p []byte) (int, error) {
	s.WriteHeader(http.StatusOK)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(p)
}

func (s *streamWriter) Flush() {}

func (s *streamWriter) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String()
}

type stream struct {
	w      *streamWriter
	cancel context.CancelFunc
	done   chan struct{}
	read   int
}

type frame struct{ id, event, data string }

// openStream starts GET /<land>/events and returns once the stream is
// subscribed (its status is written).
func openStream(t *testing.T, h http.Handler, path string, hdr ...string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequestWithContext(ctx, "GET", "/v1/lands/v1/lands"+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	s := &stream{w: &streamWriter{header: http.Header{}, started: make(chan struct{})}, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		h.ServeHTTP(s.w, r)
	}()
	select {
	case <-s.w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never started")
	}
	t.Cleanup(s.close)
	return s
}

func (s *stream) close() {
	s.cancel()
	<-s.done
}

// next waits for the next frame.
func (s *stream) next(t *testing.T) frame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body := s.w.text()[s.read:]
		if i := strings.Index(body, "\n\n"); i >= 0 {
			s.read += i + 2
			var f frame
			for _, line := range strings.Split(body[:i], "\n") {
				k, v, _ := strings.Cut(line, ": ")
				switch k {
				case "id":
					f.id = v
				case "event":
					f.event = v
				case "data":
					f.data = v
				}
			}
			return f
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no frame; stream so far:\n%s", s.w.text())
	return frame{}
}

// quiet checks that no further frame arrives for a moment.
func (s *stream) quiet(t *testing.T) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if rest := s.w.text()[s.read:]; rest != "" {
		t.Errorf("unexpected frames:\n%s", rest)
	}
}

func (s *stream) hygiene(t *testing.T) {
	t.Helper()
	s.w.mu.Lock()
	status := s.w.status
	s.w.mu.Unlock()
	assertHygiene(t, "stream", resp{status: status, header: s.w.header, body: s.w.text()})
	if ct := s.w.header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func decodeObject[T any](t *testing.T, f frame) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(strings.NewReader(f.data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %T: %v\n%s", v, err, f.data)
	}
	return v
}

func TestFeed_StreamsTheAllowlistOnly(t *testing.T) {
	_, c, _, h := newFeedFixture(t)
	s := openStream(t, h, "/acme/events")

	c.publish(ChangeUpdated, "ac-c1", "project:acme")         // a chore: no noun streams it
	c.publish(ChangeOther, "ac-t1", "project:acme")           // a topic no event comes from
	c.publish(ChangeUpdated, "zz-t1", "project:zeta")         // another land
	c.publish(ChangeUpdated, "ac-t1", "project:acme", "x")    // ticket.updated
	c.publish(ChangeCreated, "ac-d1", "project:acme")         // decision.opened
	c.publish(ChangeUpdated, "ac-d1", "project:acme")         // a decision edit: nothing
	c.publish(ChangeCreated, "ac-e1", "project:acme")         // escalation.opened
	c.publish(ChangeUpdated, "ac-p1", "project:acme", "plan") // plan.updated
	c.publish(ChangeUpdated, "ac-gone", "project:acme")       // no such bead
	c.publish(ChangeUpdated, "ac-t1", "project:acme-annex")   // a land named like this one

	f := s.next(t)
	if f.event != string(landsv1.TicketUpdated) || f.id == "" {
		t.Fatalf("frame 1 = %+v", f)
	}
	if tk := decodeObject[landsv1.Ticket](t, f); tk.Id != "ac-t1" || tk.Status != landsv1.TicketStatusWaiting {
		t.Errorf("ticket = %+v", tk)
	}
	f = s.next(t)
	if d := decodeObject[landsv1.Decision](t, f); f.event != string(landsv1.DecisionOpened) || d.Id != "ac-d1" {
		t.Errorf("frame 2 = %+v", f)
	}
	f = s.next(t)
	if e := decodeObject[landsv1.Escalation](t, f); f.event != string(landsv1.EscalationOpened) || e.Id != "ac-e1" {
		t.Errorf("frame 3 = %+v", f)
	}
	f = s.next(t)
	p := decodeObject[landsv1.Plan](t, f)
	if f.event != string(landsv1.PlanUpdated) || p.Id != "ac-p1" || len(p.Steps) != 1 {
		t.Errorf("frame 4 = %+v", f)
	}
	s.quiet(t)
	s.hygiene(t)
}

func TestFeed_DecisionResolved(t *testing.T) {
	d, c, _, h := newFeedFixture(t)
	s := openStream(t, h, "/acme/events")
	d.beads["ac-d1"]["status"] = "closed"
	d.beads["ac-d1"]["fields"].(map[string]any)["chosen"] = "red"
	c.publish(ChangeClosed, "ac-d1", "project:acme")
	f := s.next(t)
	if dec := decodeObject[landsv1.Decision](t, f); f.event != string(landsv1.DecisionResolved) || dec.Status != landsv1.DecisionStatusResolved {
		t.Errorf("frame = %+v", f)
	}
	s.hygiene(t)
}

// A purse write sends budget.updated with the snapshot as it reads at
// delivery, though the reader cannot read the account bead; a rewrite that
// changes nothing the contract shows (the watcher's heartbeat) sends nothing,
// and a snapshot with no used figure is not sent.
func TestFeed_BudgetUpdated(t *testing.T) {
	fig := func(v float64) *float64 { return &v }
	var mu sync.Mutex
	cur := BudgetRecord{Granted: fig(500), Used: fig(100), State: "ok", ReadAt: time.Now()}
	set := func(r BudgetRecord) { mu.Lock(); cur = r; mu.Unlock() }
	d, c, _, h := newFeedFixtureWith(t, fakeLands{livePurse: func() BudgetRecord { mu.Lock(); defer mu.Unlock(); return cur }})
	d.mu.Lock()
	d.forced[rGetBead] = http.StatusForbidden // the account bead, and every other
	d.mu.Unlock()
	s := openStream(t, h, "/acme/events")

	c.publish(ChangePurse, "ac-acct", "project:acme")
	f := s.next(t)
	if b := decodeObject[landsv1.Budget](t, f); f.event != string(landsv1.BudgetUpdated) || b != (landsv1.Budget{Limit: 500, Spent: 100, Unit: landsv1.Credits, Status: landsv1.Ok}) {
		t.Fatalf("frame 1 = %+v", f)
	}
	c.publish(ChangePurse, "ac-acct", "project:acme") // heartbeat: the same purse
	set(BudgetRecord{Granted: fig(500), State: "unknown", ReadAt: time.Now()})
	c.publish(ChangePurse, "ac-acct", "project:acme") // no used figure
	set(BudgetRecord{Granted: fig(500), Used: fig(480), State: "wall", ReadAt: time.Now()})
	c.publish(ChangePurse, "ac-acct", "project:acme")
	f = s.next(t)
	if b := decodeObject[landsv1.Budget](t, f); b.Status != landsv1.Exhausted || b.Spent != 480 {
		t.Errorf("frame 2 = %+v", f)
	}
	c.publish(ChangePurse, "zz-acct", "project:zeta") // another land's purse
	s.quiet(t)
	s.hygiene(t)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.calls {
		if c.path == "/v1/beads/ac-acct" {
			t.Error("the feed read the account bead; a purse change reads the snapshot")
		}
	}
}

// The bead is read as the reader when the frame is written: a read the
// daemon refuses then is dropped, whatever it was when the change happened.
func TestFeed_ScopeResolvedAtDelivery(t *testing.T) {
	d, c, _, h := newFeedFixture(t)
	s := openStream(t, h, "/acme/events")

	d.mu.Lock()
	d.forced[rGetBead] = http.StatusForbidden
	d.mu.Unlock()
	c.publish(ChangeUpdated, "ac-t1", "project:acme")
	waitForCall(t, d, "/v1/beads/ac-t1")
	d.mu.Lock()
	delete(d.forced, rGetBead)
	d.mu.Unlock()
	c.publish(ChangeUpdated, "ac-b1", "project:acme")

	if f := s.next(t); !strings.Contains(f.data, `"ac-b1"`) {
		t.Errorf("first frame = %+v, want ac-b1 (ac-t1 was unreadable at delivery)", f)
	}
	s.quiet(t)
	s.hygiene(t)
}

// waitForCall waits until the daemon has answered a call on path.
func waitForCall(t *testing.T, d *fakeDaemon, path string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		d.mu.Lock()
		for _, c := range d.calls {
			if c.path == path {
				d.mu.Unlock()
				return
			}
		}
		d.mu.Unlock()
	}
	t.Fatalf("no call on %s", path)
}

func TestFeed_ResumesFromItsCursor(t *testing.T) {
	_, c, _, h := newFeedFixture(t)
	s := openStream(t, h, "/acme/events")
	c.publish(ChangeUpdated, "ac-t1", "project:acme")
	first := s.next(t)
	s.close()

	c.publish(ChangeUpdated, "ac-b1", "project:acme")
	c.publish(ChangeCreated, "ac-d1", "project:acme")

	for _, open := range []func() *stream{
		func() *stream { return openStream(t, h, "/acme/events", "Last-Event-ID", first.id) },
		func() *stream { return openStream(t, h, "/acme/events?since="+first.id) },
	} {
		r := open()
		if f := r.next(t); !strings.Contains(f.data, `"ac-b1"`) {
			t.Errorf("replay 1 = %+v", f)
		}
		if f := r.next(t); f.event != string(landsv1.DecisionOpened) {
			t.Errorf("replay 2 = %+v", f)
		}
		r.quiet(t)
		r.close()
	}

	// A cursor this process did not mint, or garbage, resumes from now.
	for _, cur := range []string{"bm90LWEtY3Vyc29y", "%%%"} {
		r := openStream(t, h, "/acme/events", "Last-Event-ID", cur)
		r.quiet(t)
		r.close()
	}
}

func TestFeed_ExpiredCursorResumesFromNow(t *testing.T) {
	_, c, _, h := newFeedFixture(t)
	s := openStream(t, h, "/acme/events")
	c.publish(ChangeUpdated, "ac-t1", "project:acme")
	first := s.next(t)
	s.close()
	// Enough streamable changes to push the cursor out of the land's log.
	for range feedRingSize + 1 {
		c.publish(ChangeUpdated, "ac-b1", "project:acme")
	}
	r := openStream(t, h, "/acme/events", "Last-Event-ID", first.id)
	r.quiet(t)
}

func TestFeed_OtherLandIsNotFound(t *testing.T) {
	_, _, _, h := newFeedFixture(t)
	problemOf(t, do(t, h, "GET", "/zeta/events", ""), landsv1.NotFound)
}

// A reader that falls feedSubscriberBuffer behind is cut, not left with a
// silent gap; its cursor then replays what it missed.
func TestFeed_SlowReaderIsCut(t *testing.T) {
	f := newFeed(nil)
	sub, _, err := f.subscribe("acme", nil)
	if err != nil {
		t.Fatal(err)
	}
	for range feedSubscriberBuffer {
		f.publish(Change{Kind: ChangeUpdated, BeadID: "ac-t1", Labels: []string{"project:acme"}})
	}
	select {
	case <-sub.cut:
		t.Fatal("cut while within its buffer")
	default:
	}
	f.publish(Change{Kind: ChangeUpdated, BeadID: "ac-t1", Labels: []string{"project:acme"}})
	select {
	case <-sub.cut:
	default:
		t.Fatal("not cut after overflowing its buffer")
	}
	cur := f.encodeCursor(1)
	_, replay, err := f.subscribe("acme", &cur)
	if err != nil || len(replay) != feedSubscriberBuffer {
		t.Errorf("replay after cut = %d entries, %v; want %d", len(replay), err, feedSubscriberBuffer)
	}
}

func TestFeed_PerLandReaderCap(t *testing.T) {
	_, _, m, h := newFeedFixture(t)
	m.feed.lands["acme"] = &landLog{subs: map[*feedSub]struct{}{}, opening: rate.NewLimiter(rate.Inf, 0)}
	for range maxLandFeedSubscribers {
		if _, _, err := m.feed.subscribe("acme", nil); err != nil {
			t.Fatal(err)
		}
	}
	problemOf(t, do(t, h, "GET", "/acme/events", ""), landsv1.RateLimited)
	// Another land is not affected.
	if _, _, err := m.feed.subscribe("other", nil); err != nil {
		t.Errorf("other land: %v", err)
	}
}

// New streams per land: a burst, then landFeedRate a second. A reader that
// leaves gives back no new-stream token, only its seat.
func TestFeed_PerLandNewStreamRate(t *testing.T) {
	f := newFeed(nil)
	for i := range landFeedBurst {
		s, _, err := f.subscribe("acme", nil)
		if err != nil {
			t.Fatalf("stream %d within the burst: %v", i, err)
		}
		f.unsubscribe("acme", s)
	}
	if _, _, err := f.subscribe("acme", nil); err == nil {
		t.Error("a stream past the burst was let in")
	}
	if _, _, err := f.subscribe("other", nil); err != nil {
		t.Errorf("other land: %v", err)
	}
}

func TestFeed_HeadersAreInTheTable(t *testing.T) {
	for k := range feedHeaders {
		if !strings.Contains(strings.Join(ResponseHeaders, ","), k) {
			t.Errorf("feed header %s is not in ResponseHeaders", k)
		}
	}
}
