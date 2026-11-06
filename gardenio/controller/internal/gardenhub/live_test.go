package gardenhub

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Every upstream event that names a bead moves it: a bead event its bead, a comment or a label the bead it is on, a
// dependency both ends (each end lists the other). An event naming none moves nothing.
func TestMovedIDs(t *testing.T) {
	cases := []struct {
		name string
		in   beadsapi.SSEEvent
		want []string
	}{
		{"bead updated", ev("beads.bead.updated", `{"bead":{"id":"gd-a","type":"task"},"changes":{"title":"x"}}`), []string{"gd-a"}},
		{"bead deleted", ev("beads.bead.deleted", `{"bead_id":"gd-a"}`), []string{"gd-a"}},
		{"comment added", ev("beads.comment.added", `{"comment":{"id":7,"bead_id":"gd-a","text":"hi"}}`), []string{"gd-a"}},
		{"comment deleted", ev("beads.comment.deleted", `{"bead_id":"gd-a","comment_id":7}`), []string{"gd-a"}},
		{"label added", ev("beads.label.added", `{"bead_id":"gd-a","label":"ux"}`), []string{"gd-a"}},
		{"dependency added: both ends", ev("beads.dependency.added", `{"dependency":{"bead_id":"gd-a","depends_on_id":"gd-b","type":"blocks"}}`), []string{"gd-a", "gd-b"}},
		{"dependency removed: both ends", ev("beads.dependency.removed", `{"bead_id":"gd-a","depends_on_id":"gd-b","type":"blocks"}`), []string{"gd-a", "gd-b"}},
		{"named twice, listed once", ev("beads.bead.closed", `{"bead":{"id":"gd-a"},"bead_id":"gd-a"}`), []string{"gd-a"}},
		{"no bead", ev("beads.taint.granted", `{"taint_id":3}`), nil},
		{"not json", ev("beads.bead.updated", `nope`), nil},
		{"empty", ev("beads.bead.updated", ``), nil},
	}
	for _, c := range cases {
		if got := movedIDs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: movedIDs = %v, want %v", c.name, got, c.want)
		}
	}
}

// The fan-out signals only the panels showing a moved bead, folds repeats into one pending signal, moves every panel on
// touchAll, and a cancel (twice is safe) takes the panel off.
func TestMovesFanOut(t *testing.T) {
	var m moves
	a1, stopA1 := m.subscribe("gd-a")
	a2, stopA2 := m.subscribe("gd-a")
	b, stopB := m.subscribe("gd-b")
	defer stopA2()
	defer stopB()
	if n := m.count(); n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}
	pending := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	m.touch("gd-a", "gd-a", "gd-z")
	if !pending(a1) || !pending(a2) {
		t.Error("both panels on gd-a must be told")
	}
	if pending(a1) {
		t.Error("repeats fold into one signal")
	}
	if pending(b) {
		t.Error("gd-b did not move")
	}
	if !m.take(a1) || m.take(a1) {
		t.Error("a touch is a whole move, taken once")
	}
	m.touchComment("gd-b")
	if !pending(b) || m.take(b) {
		t.Error("a comment added signals the panel, and is not a whole move")
	}
	m.touchComment("gd-b")
	m.touch("gd-b")
	if !pending(b) || !m.take(b) {
		t.Error("a comment and a write in one burst are a whole move")
	}
	m.touchAll()
	if !pending(a1) || !pending(a2) || !pending(b) {
		t.Error("touchAll moves every panel")
	}
	if !m.take(b) {
		t.Error("touchAll is a whole move: what it missed is unknown")
	}
	stopA1()
	stopA1()
	if n := m.count(); n != 2 {
		t.Errorf("count after a cancel = %d, want 2", n)
	}
	if _, ok := <-a1; ok {
		t.Error("a cancelled panel's channel is closed")
	}
	m.touch("gd-a") // a closed channel is never sent on
}

type failingBeads struct{ fakeBeads }

func (f *failingBeads) GetBead(context.Context, string) (*beadsapi.BeadDetail, error) {
	return nil, errors.New("daemon down")
}

// GET /garden/bead/{id}/events: a bead the viewer can read gets a stream that says "moved" with only its id, once per
// burst, when an upstream event names it (a comment here, which no snapshot reads); a bead it cannot read is a 404, so
// the stream is never an oracle for when a hidden bead changes.
func TestBeadEventsStream(t *testing.T) {
	src := newFake()
	hub := New(src, "/v", nil)
	hub.Start()
	defer hub.Close()
	for start := time.Now(); !hub.Stats().Upstream && time.Since(start) < 5*time.Second; {
		time.Sleep(5 * time.Millisecond) // the connect's touchAll is behind us, so every move below is an event's
	}
	mux := http.NewServeMux()
	(&Handler{Hub: hub, BasePath: "/v", Beads: beadFixture()}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, c := range []struct {
		h    *Handler
		id   string
		want int
	}{
		{&Handler{Hub: hub, Beads: beadFixture()}, "gd-nope", 404},
		{&Handler{Hub: hub, Beads: &failingBeads{}}, "gd-task", 502},
		{&Handler{Hub: hub}, "gd-task", 503},
		{&Handler{Beads: beadFixture()}, "gd-task", 503},
		{&Handler{Hub: hub, Beads: beadFixture()}, strings.Repeat("x", 201), 400},
	} {
		m := http.NewServeMux()
		c.h.Register(m)
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest("GET", "/garden/bead/"+c.id+"/events", nil))
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.id, rec.Code, c.want)
		}
	}
	if n := hub.moves.count(); n != 0 {
		t.Errorf("a refused stream must not stay subscribed: %d panels", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/garden/bead/gd-task/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for hub.moves.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	src.events <- ev("beads.comment.added", `{"comment":{"bead_id":"gd-other"}}`)
	src.events <- ev("beads.comment.added", `{"comment":{"bead_id":"gd-task","text":"hi"}}`)
	src.events <- ev("beads.dependency.added", `{"dependency":{"bead_id":"gd-next","depends_on_id":"gd-task","type":"blocks"}}`)
	var moved []string
	timeout := time.After(5 * time.Second)
	for len(moved) == 0 {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream ended")
			}
			if l == "event: moved" {
				moved = append(moved, <-lines)
			}
		case <-timeout:
			t.Fatal("no moved event within 5s")
		}
	}
	if moved[0] != `data: {"id":"gd-task"}` {
		t.Errorf("moved data = %q, want only the id", moved[0])
	}
	// the burst was one move: nothing more comes before another event
	select {
	case l := <-lines:
		if l == "event: moved" {
			t.Error("a burst must fold into one moved event")
		}
	case <-time.After(2 * moveSettle):
	}
	// a burst that only added comments says so, and the panel fetches only those (bead.go ?after=)
	src.events <- ev("beads.comment.added", `{"comment":{"bead_id":"gd-task","text":"again"}}`)
	if got := nextMoved(t, lines); got != `data: {"id":"gd-task","only":"comments"}` {
		t.Errorf("comments-only moved data = %q", got)
	}
	if st := hub.Stats(); st.LivePanels != 1 {
		t.Errorf("stats live_panels = %d, want 1", st.LivePanels)
	}
	cancel()
	deadline = time.Now().Add(5 * time.Second)
	for hub.moves.count() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := hub.moves.count(); n != 0 {
		t.Errorf("a closed stream must unsubscribe: %d panels", n)
	}
}

// nextMoved reads the stream up to its next moved event and returns that event's data line.
func nextMoved(t *testing.T, lines <-chan string) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream ended")
			}
			if l == "event: moved" {
				return <-lines
			}
		case <-timeout:
			t.Fatal("no moved event within 5s")
		}
	}
}

// workbench.js's live and degraded logic under node (WB-14, WB-15): a failed load says what failed in words and whether a
// second try may pass; a repaint finds the focused control again by its tag, label and href; a failed bead load says what
// the map knows; a refresh waits while the panel holds the viewer's work.
func TestLivePanelLogicUnderNode(t *testing.T) {
	script := `
var V = require(process.argv[1]), W = V.host, assert = require("assert");
// failure: words, never the browser's own
assert.deepStrictEqual(W.failure(0, "bead", "gd-a"), { text: "Could not load this bead: the garden did not answer.", retry: true }, "no answer");
assert.deepStrictEqual(W.failure(502, "bead", "gd-a"), { text: "Could not load this bead: the beads daemon did not answer (502).", retry: true });
assert.deepStrictEqual(W.failure(503, "bead", "gd-a"), { text: "Could not load this bead: the beads daemon did not answer (503).", retry: true });
assert.deepStrictEqual(W.failure(500, "bead", "gd-a"), { text: "Could not load this bead (500).", retry: true });
assert.deepStrictEqual(W.failure(404, "plan", "gd-p"), { text: "There is no plan gd-p, or you cannot see it.", retry: false }, "not there: no Try again");
assert.strictEqual(W.failure(502, "bundle", "gd-b").text.indexOf("gd-b"), -1, "the line names the noun, not the id the head shows");
assert.strictEqual(W.failure(0, "settings", "").text, "Could not load the settings: the garden did not answer.");
assert.strictEqual(W.refreshFailure(W.failure(502, "plan", "gd-p").text, "plan"), "Could not refresh this plan: the beads daemon did not answer (502).");
assert.strictEqual(W.refreshFailure("Could not load gd-p (500).", "plan"), "Could not refresh this plan (500).", "an older id-worded line too");
assert.strictEqual(W.failure(401, "bead", "gd-a").retry, false, "a session that ended: reload, not retry");
assert.strictEqual(W.failure("redirect", "bead", "gd-a").text, W.failure(401, "bead", "gd-a").text, "the sign-in redirect is the same");
// placeKey: tag, label, href
assert.strictEqual(W.placeKey("BUTTON", " Edit ", null), "button|Edit|");
assert.strictEqual(W.placeKey("A", "In-garden bead view", "/garden?bead=gd-a"), "a|In-garden bead view|/garden?bead=gd-a");
assert.strictEqual(W.placeKey("", "x", ""), "", "no focus: no place");
// degraded: what the map knows, or that it does not
assert.strictEqual(V.degraded({ id: "gd-a", title: "Fix the gate" }), "This is what the map knows about it.");
assert.strictEqual(V.degraded({ id: "gd-a" }), "The map does not show it either.");
assert.strictEqual(V.degraded(null), "The map does not show it either.");
// holding: a stub root answering the selectors holding asks
function root(hit, boxes) { return { querySelector: function (sel) { return hit ? {} : null; }, querySelectorAll: function () { return boxes || []; } }; }
assert.strictEqual(V.holding(root(true)), true, "a form open, an answer armed or a write in flight");
assert.strictEqual(V.holding(root(false, [{ value: "", defaultValue: "" }])), false, "an empty composer");
assert.strictEqual(V.holding(root(false, [{ value: "half a comment", defaultValue: "" }])), true, "a typed draft");
assert.strictEqual(V.holding(root(false, [{ value: "same", defaultValue: "same" }])), false, "untouched");
assert.strictEqual(V.holding(null), false);
var sel = "";
V.holding({ querySelector: function (s) { sel = s; return null; }, querySelectorAll: function () { return []; } });
["form:not(.bw-comment):not(.bw-links)", ".armed", "[data-armed]", "[disabled]:not(.bv-retry)"].forEach(function (s) {
  assert.ok(sel.indexOf(s) >= 0, "holding asks for " + s);
});
`
	runNodeLogic(t, "workbench.js", script)
}

// flakyBeads is the bead reader with a switch: while down, every read fails as a daemon that does not answer.
type flakyBeads struct {
	*fakeBeads
	down atomic.Bool
}

func (f *flakyBeads) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	if f.down.Load() {
		return nil, errors.New("daemon down")
	}
	return f.fakeBeads.GetBead(ctx, id)
}

// GetComments under the lock, since POST /test/comment writes the map while the panel reads it.
func (f *flakyBeads) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]beadsapi.Comment(nil), f.comments[id]...), nil
}

// liveWorld serves /garden on the fixture farm with a bead reader the browser can steer: POST /test/comment?id= adds a
// comment the way the daemon does (the row, then a beads.comment.added event on the Hub's upstream),
// and POST /test/daemon?down=1|0 takes the reader down and back.
func liveWorld(t *testing.T) *httptest.Server {
	t.Helper()
	src := fixtureWorld()
	hubSrc := newFake()
	hubSrc.events = src.events
	f := NewFarm(src, nil)
	h := New(hubSrc, "", nil)
	h.OnEvent(f.InvalidateFor)
	h.Start()
	t.Cleanup(h.Close)
	f.Start()
	t.Cleanup(f.Close)
	fb := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{}, comments: map[string][]beadsapi.Comment{}}
	for _, b := range src.beads {
		fb.beads[b.ID] = b
		if b.ParentID != "" {
			fb.deps = append(fb.deps, beadsapi.Dependency{BeadID: b.ID, DependsOnID: b.ParentID, Type: "parent-child"})
		}
	}
	fb.beads["gd-planA"].Description = strings.Repeat("A long description, so the panel scrolls.\n\n", 40)
	fb.comments["gd-planA"] = []beadsapi.Comment{{Author: "mo", Text: "The first comment.", CreatedAt: time.Now().Add(-time.Hour)}}
	beads := &flakyBeads{fakeBeads: fb}

	mux := http.NewServeMux()
	page := &Handler{Hub: h, Farm: f, Beads: beads, CanEdit: func(*http.Request) bool { return true }, Live: mux} // the panel's stream rides the page's one connection (livemux.go)
	page.Register(mux)
	n := 0
	mux.HandleFunc("POST /test/comment", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		fb.mu.Lock()
		n++
		fb.comments[id] = append(fb.comments[id], beadsapi.Comment{Author: "flo", Text: "Live comment " + strconv.Itoa(n) + ".", CreatedAt: time.Now()})
		fb.mu.Unlock()
		hubSrc.events <- ev("beads.comment.added", `{"comment":{"bead_id":"`+id+`"}}`)
		w.WriteHeader(http.StatusNoContent)
	})
	// no Commons here: the bead panel's node read (web/beadwork.js) is answered as the Commons answers a bead it holds no
	// node for, and the panel keeps its own lists
	mux.HandleFunc("GET /garden/c/n/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /test/daemon", func(w http.ResponseWriter, r *http.Request) {
		beads.down.Store(r.URL.Query().Get("down") == "1")
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Live panels in a real Chromium (WB-14, WB-15): what is asserted lives in testdata/live_browser.js.
func TestLivePanelsUnderChromium(t *testing.T) {
	underChromium(t, "live_browser.js", func() *httptest.Server { return liveWorld(t) })
}
