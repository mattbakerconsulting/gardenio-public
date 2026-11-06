package gardenhub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/glass"
)

// The Wall (Lands workbench-glass spec GL-5; step 6.7, gd-Pjha29mbgN): GET /garden/glass/wall and web/wall.js.

// wallFeed is coopmux's /ws/mux behind the Wall: each dial's connection answers with the sessions list and records what
// the Wall subscribes to.
type wallFeed struct {
	mu   sync.Mutex
	ups  []*wallUp
	dial chan *wallUp
}

type wallUp struct {
	ctx  context.Context
	in   chan coopmuxapi.Frame
	mu   sync.Mutex
	subs map[string]bool // ever subscribed on this connection
	once sync.Once
	done chan struct{}
}

func newWallFeed() *wallFeed { return &wallFeed{dial: make(chan *wallUp, 8)} }

func (f *wallFeed) open(ctx context.Context) (glass.MuxUpstream, error) {
	u := &wallUp{ctx: ctx, in: make(chan coopmuxapi.Frame, 64), subs: map[string]bool{}, done: make(chan struct{})}
	f.mu.Lock()
	f.ups = append(f.ups, u)
	f.mu.Unlock()
	f.dial <- u
	return u, nil
}

func (u *wallUp) Recv() (coopmuxapi.Frame, error) {
	select {
	case fr := <-u.in:
		return fr, nil
	case <-u.ctx.Done():
		return coopmuxapi.Frame{}, u.ctx.Err()
	case <-u.done:
		return coopmuxapi.Frame{}, io.EOF
	}
}

func (u *wallUp) SubscribeSessions(s ...string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, x := range s {
		u.subs[x] = true
	}
	return nil
}

func (u *wallUp) UnsubscribeSessions(s ...string) error { return nil }

func (u *wallUp) Close() error { u.once.Do(func() { close(u.done) }); return nil }

func (u *wallUp) emit(raw string) {
	var tag struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal([]byte(raw), &tag)
	u.in <- coopmuxapi.Frame{Event: tag.Event, Data: json.RawMessage(raw)}
}

func (u *wallUp) subscribed() map[string]bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string]bool{}
	for k := range u.subs {
		out[k] = true
	}
	return out
}

// wallHandler is the Glass's fixture (glassHandler) with its agents open, as the daemon lists them, and a Wall.
func wallHandler(feed *wallFeed) *Handler {
	h := glassHandler(&glassMux{}, glassScreens{})
	for _, b := range h.Beads.(*fakeBeads).beads {
		if b.Type == "agent" {
			b.Status = "open"
		}
	}
	if feed != nil {
		h.Glass.Wall = glass.NewWall(feed.open, nil)
	}
	return h
}

func wallDoc(t *testing.T, h *Handler, path string, hdr ...string) GlassWallDoc {
	t.Helper()
	rec := glassGet(t, h, path, "gardenio-operator", hdr...)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
	}
	var d GlassWallDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func wallIDs(d GlassWallDoc) []string {
	var ids []string
	for _, a := range d.Agents {
		ids = append(ids, a.ID)
	}
	return ids
}

// The JSON first paint lists the viewer's agents that have a session, in their land scope, with the task each tends; an
// unrestricted viewer sees every land's; the stream is offered only when the Wall is wired.
func TestGlassWallDoc(t *testing.T) {
	h := wallHandler(newWallFeed())
	d := wallDoc(t, h, "/garden/glass/wall")
	if got := strings.Join(wallIDs(d), ","); got != "gd-agent1,gd-gone" {
		t.Errorf("scoped agents = %s, want the in-scope ones with a session", got)
	}
	a := d.Agents[0]
	if a.Agent != "crew-one" || a.Role != "crew" || a.Project != "gardenio" || a.TaskTitle != "6.3 Glass relay" || a.State != "working" {
		t.Errorf("tile header = %+v", a)
	}
	if !d.Live || d.Stream != "/v/garden/glass/wall" || d.Watch != "operators" {
		t.Errorf("doc = %+v", d)
	}
	if raw := glassGet(t, h, "/garden/glass/wall", "gardenio-operator").Body.String(); strings.Contains(raw, "sess-1") {
		t.Errorf("the doc names a session: %s", raw)
	}

	all := wallDoc(t, h, "/garden/glass/wall", "X-Test-Admin", "1")
	if got := strings.Join(wallIDs(all), ","); !strings.Contains(got, "gd-other") || len(all.Agents) != 3 {
		t.Errorf("unrestricted agents = %s", got)
	}

	one := wallDoc(t, h, "/garden/glass/wall?agent=gd-agent1")
	if got := strings.Join(wallIDs(one), ","); got != "gd-agent1" || one.Stream != "/v/garden/glass/wall?agent=gd-agent1" {
		t.Errorf("?agent= doc = %+v", one)
	}

	off := wallHandler(nil)
	if d := wallDoc(t, off, "/garden/glass/wall"); d.Live || d.Stream != "" || len(d.Agents) != 2 {
		t.Errorf("without a Wall the doc still lists the agents, not live: %+v", d)
	}
}

// Watch is operator-tier, the Wall included; ?agent= outside the viewer's lands is a 404, like every Glass route; a page
// without the Glass answers 503, and so does the stream without a Wall.
func TestGlassWallScopeAndTier(t *testing.T) {
	h := wallHandler(newWallFeed())
	if rec := glassGet(t, h, "/garden/glass/wall", "gardenio-viewer"); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer-tier principal got %d", rec.Code)
	}
	for _, id := range []string{"gd-other", "gd-plain", "gd-nope"} {
		if rec := glassGet(t, h, "/garden/glass/wall?agent="+id, "gardenio-operator"); rec.Code != http.StatusNotFound {
			t.Errorf("?agent=%s = %d, want 404", id, rec.Code)
		}
	}
	if rec := glassGet(t, &Handler{}, "/garden/glass/wall", "gardenio-operator"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no glass = %d", rec.Code)
	}
	if rec := glassGet(t, wallHandler(nil), "/garden/glass/wall", "gardenio-operator", "Accept", "text/event-stream"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a stream with no Wall = %d", rec.Code)
	}
}

// The stream names its tiles first (agents), then carries the Wall's events for exactly those agents: the upstream is
// subscribed only to the viewer's sessions, a session outside their lands never reaches them, and no session id is sent.
func TestGlassWallStreamIsFiltered(t *testing.T) {
	feed := newWallFeed()
	h := wallHandler(feed)
	go func() {
		select {
		case u := <-feed.dial:
			u.emit(`{"event":"sessions","sessions":[{"id":"gardenio-crew-one-abc","state":"working"},{"id":"sess-2","state":"prompt"},{"id":"gone-session","state":"idle"}]}`)
			u.emit(`{"event":"screen_batch","screens":[{"session":"gardenio-crew-one-abc","lines":["$ make test"],"ansi":["$ make test"],"cols":80,"rows":24,"seq":3},` +
				`{"session":"sess-2","lines":["tenant secret"],"ansi":[],"cols":80,"rows":24,"seq":4}]}`)
		case <-time.After(2 * time.Second):
		}
	}()
	rec := glassGet(t, h, "/garden/glass/wall", "gardenio-operator", "Accept", "text/event-stream")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	ia, it, is := strings.Index(body, "event: agents"), strings.Index(body, "event: tiles"), strings.Index(body, "event: screens")
	if ia < 0 || it < ia || is < it {
		t.Fatalf("want agents, tiles, screens in order:\n%s", body)
	}
	for _, banned := range []string{"gd-other", "tenant secret", "sess-1", "sess-2"} {
		if strings.Contains(body, banned) {
			t.Errorf("the scoped viewer's stream carries %q:\n%s", banned, body)
		}
	}
	if !strings.Contains(body, `"agent":"gd-agent1"`) || !strings.Contains(body, "$ make test") {
		t.Errorf("the viewer's own tile is missing:\n%s", body)
	}
	feed.mu.Lock()
	up := feed.ups[0]
	feed.mu.Unlock()
	if subs := up.subscribed(); subs["sess-2"] || !subs["gardenio-crew-one-abc"] || !subs["gone-session"] {
		t.Errorf("subscribed to %v, want the viewer's sessions only", subs)
	}
}

// wall.js's pure half runs under node: the attention order (GL-5: prompt, error, rate limited, working, idle, then the
// rest), the status line, coop's ANSI colours as spans, a thumbnail's last lines; and the Workbench's ?glass= with no
// agent is the Wall.
func TestWallLogicUnderNode(t *testing.T) {
	wb, err := filepath.Abs(filepath.Join("web", "workbench.js"))
	if err != nil {
		t.Fatal(err)
	}
	script := `
var L = require(process.argv[1]), assert = require("assert"), W = require(` + jsString(wb) + `).host;
assert.strictEqual(L.url(), "/garden/glass/wall");
assert.strictEqual(L.url("gd-a b"), "/garden/glass/wall?agent=gd-a%20b");

var o = L.order([{ id: "i", name: "idle-one", key: "idle" }, { id: "w", name: "work", key: "working" }, { id: "x", name: "exited", key: "exited" },
  { id: "r", name: "rl", key: "rate_limited" }, { id: "e", name: "err", key: "error" }, { id: "p", name: "ask", key: "prompt" }, { id: "s", name: "sprout", key: "starting" }]);
assert.deepStrictEqual(o.map(function (t) { return t.id; }), ["p", "e", "r", "w", "i", "s", "x"], "the attention order");
assert.deepStrictEqual(L.order([{ id: "b", name: "b", key: "idle" }, { id: "a", name: "a", key: "idle" }]).map(function (t) { return t.id; }), ["a", "b"], "then by name");

assert.strictEqual(L.summary([], 0), "No live sessions in your lands right now.");
assert.strictEqual(L.summary([{ key: "prompt" }, { key: "working" }, { key: "error" }], 2), "3 live sessions: 1 at a prompt, 1 in error or rate limited; 2 more agents have no live session.");
assert.strictEqual(L.summary([{ key: "idle" }], 0), "1 live session.");

assert.deepStrictEqual(L.spans("plain"), [["plain", null]]);
var sp = L.spans("\x1b[1;31mred\x1b[0m ok");
assert.deepStrictEqual(sp[0], ["red", { color: "#f14c4c", fontWeight: "700" }], "bold brightens a named colour");
assert.deepStrictEqual(sp[1], [" ok", null]);
assert.strictEqual(L.spans("\x1b[38;5;196mx")[0][1].color, "rgb(255,0,0)");
assert.strictEqual(L.spans("\x1b[38;2;1;2;3mx")[0][1].color, "rgb(1,2,3)");
assert.strictEqual(L.spans("\x1b[48;5;240mx")[0][1].backgroundColor, "rgb(88,88,88)");
assert.deepStrictEqual(L.spans("\x1b[7mi")[0][1], { color: "var(--wl-bg)", backgroundColor: "var(--wl-fg)" }, "inverse with no colours swaps the terminal's own");
assert.strictEqual(L.plain("\x1b]0;title\x07a\x1b[2Kb\x1b(B"), "ab", "every other escape is dropped");
assert.strictEqual(L.color256(232), "rgb(8,8,8)");
function lum(h) { return [1, 3, 5].reduce(function (a, i, k) { var c = parseInt(h.substr(i, 2), 16) / 255; return a + [0.2126, 0.7152, 0.0722][k] * (c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4)); }, 0); }
L.COLORS.slice(1).forEach(function (c) { assert.ok((lum(c) + 0.05) / (lum("#0b1220") + 0.05) >= 4.5, c + " reads at 4.5:1 on --wl-bg (gd-pHyKC0Uef1)"); });

assert.deepStrictEqual(L.tail(["1", "2", "3", "4", "5", "6", "7", "8", "", "\x1b[0m  "], 6), ["3", "4", "5", "6", "7", "8"], "the last lines that say something");
assert.deepStrictEqual(L.tail([], 6), []);
assert.deepStrictEqual(L.tail(["a", "", "b", "c", "", "d", "e", "", "f", ""], 6, true), ["a", "b", "c", "d", "e", "f"], "a card's six rows each say something");
assert.ok(L.MIN_SCALE * 12 >= 10, "a Wall tile's glyphs are never below 10 px");
assert.strictEqual(L.THUMB_ROWS, 6, "a figure's card shows six lines");

assert.deepStrictEqual(W.panelOf("?glass="), { kind: "glass", id: "wall" }, "?glass= with no agent is the Wall");
assert.deepStrictEqual(W.panelOf("?glass"), { kind: "glass", id: "wall" });
assert.deepStrictEqual(W.panelOf("?glass=gd-a"), { kind: "glass", id: "gd-a" });
assert.strictEqual(W.panelOf("?bead="), null, "other kinds still need an id");
assert.strictEqual(W.withPanel("", "glass", "wall"), "?glass=wall");
`
	runNodeLogic(t, "wall.js", script)
}

func jsString(s string) string { b, _ := json.Marshal(s); return string(b) }

// The Wall never writes to a session either: wall.js posts nothing and opens no WebSocket; and neither bundle needs the
// other's code, so the Wall loads no Glass (gd-BC3yZxTQ65) and a Glass watch no Wall (gd-gjqJXrcXsJ).
func TestWallNeverWrites(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("web", "wall.js"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, banned := range []string{"method:", "'POST'", `"POST"`, "WebSocket", "sendBeacon", "/send", "innerHTML"} {
		if strings.Contains(s, banned) {
			t.Errorf("wall.js contains %q: the Wall is look only, and its text goes in through textContent", banned)
		}
	}
	if g, err := os.ReadFile(filepath.Join("web", "glass.js")); err != nil || strings.Contains(string(g), "WALL") {
		t.Errorf("glass.js uses wall.js, which a Glass watch does not load (%v)", err)
	}
	if strings.Contains(s, "GLASS") {
		t.Error("wall.js uses glass.js, which the Wall does not load")
	}
}

// wallReplayUp is the card world's /ws/mux (fixtureWall): it answers a connection with the sessions list and each
// subscription with that session's screen, as coopmux sends a new subscriber its cached thumbnail.
type wallReplayUp struct {
	screens map[string]string // session -> its screen_batch entry
	ctx     context.Context
	in      chan coopmuxapi.Frame
	once    sync.Once
	done    chan struct{}
}

func (u *wallReplayUp) Recv() (coopmuxapi.Frame, error) {
	select {
	case f := <-u.in:
		return f, nil
	case <-u.ctx.Done():
		return coopmuxapi.Frame{}, u.ctx.Err()
	case <-u.done:
		return coopmuxapi.Frame{}, io.EOF
	}
}

func (u *wallReplayUp) SubscribeSessions(s ...string) error {
	var parts []string
	for _, x := range s {
		if e := u.screens[x]; e != "" {
			parts = append(parts, e)
		}
	}
	if len(parts) > 0 {
		raw := `{"event":"screen_batch","screens":[` + strings.Join(parts, ",") + `]}`
		select {
		case u.in <- coopmuxapi.Frame{Event: "screen_batch", Data: json.RawMessage(raw)}:
		case <-u.done:
		}
	}
	return nil
}

func (u *wallReplayUp) UnsubscribeSessions(...string) error { return nil }

func (u *wallReplayUp) Close() error { u.once.Do(func() { close(u.done) }); return nil }

// fixtureWall is the card world's Wall: the Glass's two recorded agents (fixtureGlass), the asking gardener on plot B,
// gd-gAskB (fixtureGardeners), and g-work-1 on plot A, gd-gWork1 (addFixtureGardeners), the last two put in beads with a
// session, so the Wall shows four tiles in attention order (g-ask-b at a permission prompt, the glass panel's crew and
// g-work-1 working, the relay's crew idle) and g-work-1's figure on the map shows its last six lines on its card. Screens
// carry SGR colours, as coopmux's ansi lines do.
func fixtureWall(rec *glassRecording, beads *fakeBeads) *glass.Wall {
	beads.beads["gd-gAskB"] = &beadsapi.BeadDetail{ID: "gd-gAskB", Type: "agent", Status: "in_progress", Title: "g-ask-b", Labels: []string{"project:gardenio", "role:crew"},
		Fields: map[string]string{"agent": "g-ask-b", "project": "gardenio", "role": "crew", "task_id": "gd-A1a", "agent_state": "yielding", "last_session_id": "sess-ask"}}
	beads.beads["gd-gWork1"] = &beadsapi.BeadDetail{ID: "gd-gWork1", Type: "agent", Status: "in_progress", Title: "g-work-1", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent": "g-work-1", "role": "crew", "task_id": "gd-A1a", "agent_state": "working", "last_session_id": "sess-work"}}
	entry := func(session, state string, cols, rows int, plain, ansi []string) string {
		b, _ := json.Marshal(map[string]any{"session": session, "lines": plain, "ansi": ansi, "cols": cols, "rows": rows, "seq": 7, "state": state})
		return string(b)
	}
	colour := func(lines []string) []string {
		out := make([]string, len(lines))
		for i, l := range lines {
			switch {
			case strings.HasPrefix(l, "* "):
				out[i] = "\x1b[1;32m*\x1b[0m " + l[2:]
			case strings.HasPrefix(l, "     +"):
				out[i] = "\x1b[32m" + l + "\x1b[0m"
			case strings.HasPrefix(l, "> "):
				out[i] = "\x1b[1m" + l + "\x1b[0m"
			default:
				out[i] = l
			}
		}
		return out
	}
	ask := []string{"* Bash(kubectl rollout restart deploy/coopmux -n gardenio)", "", "Do you want to proceed?", "> 1. Yes",
		"  2. Yes, and don't ask again for kubectl commands", "  3. No, and tell Claude what to do differently (esc)", "", "", ""}
	work := []string{"* Bash(go test ./internal/edgeauth/ -count=1)", "  L  ok   gardenio/controller/internal/edgeauth  0.52s", "",
		"* Update(gardenio/controller/internal/edgeauth/exchange.go)", "     +  // the token is cached per pod", "     +  c.cache.Put(pod, tok)", "",
		"* Running the edgeauth tests again...", "", ""}
	idle := []string{"* Claude Code  gardenio-63-glass-relay  ~/bot/gardenio/work", "", "> Waiting for the next task", "", ""}
	screens := map[string]string{
		"gardenio-64-glass-panel-7f9c4": entry("gardenio-64-glass-panel-7f9c4", "working", int(rec.screen.Cols), int(rec.screen.Rows), rec.screen.Lines, colour(rec.screen.Lines)),
		"gardenio-63-glass-relay-7f9c4": entry("gardenio-63-glass-relay-7f9c4", "idle", 100, 26, idle, colour(idle)),
		"sess-ask":                      entry("sess-ask", "prompt", 100, 26, ask, colour(ask)),
		"sess-work":                     entry("sess-work", "working", 100, 26, work, colour(work)),
	}
	dial := func(ctx context.Context) (glass.MuxUpstream, error) {
		u := &wallReplayUp{screens: screens, ctx: ctx, in: make(chan coopmuxapi.Frame, 16), done: make(chan struct{})}
		u.in <- coopmuxapi.Frame{Event: "sessions", Data: json.RawMessage(`{"event":"sessions","sessions":[{"id":"gardenio-64-glass-panel-7f9c4","state":"working"},{"id":"gardenio-63-glass-relay-7f9c4","state":"idle"},{"id":"sess-ask","state":"prompt"},{"id":"sess-work","state":"working"}]}`)}
		return u, nil
	}
	return glass.NewWall(dial, nil)
}
