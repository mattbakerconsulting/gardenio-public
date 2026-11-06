package commons

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

var t0 = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

var (
	adminScope      = Scope{Admin: true}
	claimlayerScope = Scope{Lands: []string{"claimlayer"}}
)

// commonsServer mounts the routes over the fixture, answering each request with
// the scope and bearer the test names in the X-Test-As header. opts adjust the
// handler first.
func commonsServer(t *testing.T, d *commonstest.Daemon, opts ...func(*Handler)) http.Handler {
	t.Helper()
	h := &Handler{
		Beads: d, BasePath: "/base",
		Scope: func(r *http.Request) Scope {
			if r.Header.Get("X-Test-As") == "claimlayer" {
				return claimlayerScope
			}
			return adminScope
		},
		Now:         func() time.Time { return t0.Add(30 * time.Minute) },
		Writes:      d,
		Gate:        testGate,
		EmptyPrompt: testPrompt,
	}
	for _, o := range opts {
		o(h)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	// One message as the event stream carries it (messageRow): the cards' tests read it here, since
	// GET /garden/c/m/{id} answers with a redirect to its node (gd-ZeiSZV7j2Y).
	mux.HandleFunc("GET /test/m/{id}", func(w http.ResponseWriter, r *http.Request) {
		rw, err := h.messageRow(r.Context(), r, h.scope(r), r.PathValue("id"))
		if err != nil {
			h.fail(w, err)
			return
		}
		h.write(w, "message", rw)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := commonstest.AdminBearer
		if r.Header.Get("X-Test-As") == "claimlayer" {
			bearer = commonstest.ClaimlayerBearer
		}
		mux.ServeHTTP(w, r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer)))
	})
}

// fakeApprovals is the daemon's can-approve: ans per escalation, err to fail
// one, and "not an approver" for the rest.
type fakeApprovals struct {
	ans map[string]beadsapi.CanApproveResult
	err map[string]error
}

func (f fakeApprovals) CanApproveEscalation(_ context.Context, id string) (beadsapi.CanApproveResult, error) {
	if err := f.err[id]; err != nil {
		return beadsapi.CanApproveResult{}, err
	}
	if r, ok := f.ans[id]; ok {
		return r, nil
	}
	return beadsapi.CanApproveResult{Reason: "not an approver"}, nil
}

// testPrompt stands in for gardenhub.EmptyPrompt, which beads-viewer passes.
const testPrompt = "(the house voice)"

// testSelf is who the test request is: X-Test-Self, else the operator for an
// admin request and bo, a claimlayer member in no user channel, for claimlayer.
func testSelf(r *http.Request) string {
	if s := r.Header.Get("X-Test-Self"); s != "" {
		return s
	}
	if r.Header.Get("X-Test-As") == "claimlayer" {
		return "oidc:bo@claimlayer.example"
	}
	return "oidc:op@gardenio.example"
}

// testGate is the action table as beads-viewer applies it, cut down: the
// operator may do anything; a claimlayer member posts where the channel is
// open to the land or lists it, and joins or leaves only itself, unless the
// request says X-Test-Guest (a guest may leave, not join).
var testGate = Gate{
	Self: func(r *http.Request) string {
		if r.Header.Get("X-Test-Anon") != "" {
			return ""
		}
		return testSelf(r)
	},
	May: func(r *http.Request, ch Channel, action string) bool {
		if r.Header.Get("X-Test-As") != "claimlayer" {
			return true
		}
		return !ch.MembersOnly() || ch.HasMember(testSelf(r))
	},
	MayMembers: func(r *http.Request, ch Channel, next []string) bool {
		if r.Header.Get("X-Test-As") != "claimlayer" {
			return true
		}
		self := testSelf(r)
		joining := !ch.HasMember(self) && slices.Contains(next, self)
		return !(joining && r.Header.Get("X-Test-Guest") != "")
	},
	// A guest may answer no card.
	MayAct: func(r *http.Request, land, action string) bool {
		return r.Header.Get("X-Test-Guest") == ""
	},
}

func get(t *testing.T, h http.Handler, path, as string) (int, string) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if as != "" {
		r.Header.Set("X-Test-As", as)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// cards renders each of ids as the event stream carries it (GET /test/m) and
// joins them in order: the code is the first that is not 200.
func cards(t *testing.T, h http.Handler, as string, ids ...string) (int, string) {
	t.Helper()
	code, all := http.StatusOK, ""
	for _, id := range ids {
		c, body := get(t, h, "/test/m/"+id, as)
		if c != http.StatusOK && code == http.StatusOK {
			code = c
		}
		all += body
	}
	return code, all
}

func mustContain(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("body lacks %q:\n%s", w, body)
		}
	}
}

func mustNotContain(t *testing.T, body string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(body, b) {
			t.Errorf("body carries %q:\n%s", b, body)
		}
	}
}

// A channel's address answers only a redirect to its land's steward
// (gd-1keb7z0nBn); its messages render one by one as the stream carries them.
func TestChannelAddress(t *testing.T) {
	h := commonsServer(t, commonstest.TwoLands(t0))
	for _, path := range []string{"/garden/c/claimlayer/general", "/garden/c/claimlayer/general?before=2026-10-19T09:00:00Z", "/garden/c/claimlayer/nope"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Commons", "fragment")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/base/garden/c/s/claimlayer" {
			t.Errorf("GET %s = %d to %q, want 302 to the steward", path, rec.Code, rec.Header().Get("Location"))
		}
	}

	bodies := ""
	for _, id := range []string{"cl-wish1", "cl-msg1", "cl-del1", "cl-card1"} {
		code, body := get(t, h, "/test/m/"+id, "claimlayer")
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", id, code, body)
		}
		bodies += body
	}
	mustContain(t, bodies,
		`data-channel="cl-chgen"`, `id="m-cl-wish1"`, "wish · open",
		// Text is escaped, and URLs and bead ids are linked.
		"Claimlayer hello &lt;b&gt;there&lt;/b&gt;",
		`href="https://github.com/acme/app/pull/42"`, ">github.com/…/pull/42</a>",
		`href="/base/garden?bead=cl-AbCdEfGhIj"`,
		`href="/base/garden/c/claimlayer/cl-chgen/t/cl-wish1" class="cm-replies">1 reply</a>`,
		"Message deleted", `class="cm-author cm-author--human" title="oidc:ann@claimlayer.example">ann<`,
		`href="/base/garden?bead=cl-agent1"`,
	)
	mustNotContain(t, bodies, "deleted words")
	mustNotContain(t, bodies, commonstest.Leaks...)
}

// readChannel finds a channel of the viewer's land by name or bead id, an
// archived one too, and no other land's.
func TestReadChannel(t *testing.T) {
	d := commonstest.TwoLands(t0)
	ctx := context.Background()
	for ref, want := range map[string]string{"general": "cl-chgen", "cl-chcrew": "cl-chcrew", "old": "cl-charch"} {
		if ch, err := readChannel(ctx, d, claimlayerScope, "claimlayer", ref); err != nil || ch.ID != want {
			t.Errorf("readChannel(claimlayer, %s) = %s, %v, want %s", ref, ch.ID, err, want)
		}
	}
	for _, c := range [][2]string{
		{"gardenio", "general"},    // another land
		{"claimlayer", "gd-chgen"}, // another land's channel under this land
		{"claimlayer", "nl-chan"},  // a land-less channel
		{"claimlayer", "nope"},
	} {
		if _, err := readChannel(ctx, d, claimlayerScope, c[0], c[1]); !errors.Is(err, errNotFound) {
			t.Errorf("scoped readChannel(%s, %s) = %v, want not found", c[0], c[1], err)
		}
	}
}

func TestThread(t *testing.T) {
	h := commonsServer(t, commonstest.TwoLands(t0))
	code, body := get(t, h, "/garden/c/claimlayer/general/t/cl-wish1", "claimlayer")
	if code != http.StatusOK {
		t.Fatalf("GET thread = %d: %s", code, body)
	}
	mustContain(t, body, `data-thread="cl-wish1"`, `id="m-cl-wish1"`, `id="m-cl-rep1"`, `id="m-cl-rep2"`, "legacy reply",
		`class="cm-author cm-author--agent" title="agent:claimlayer-crew-1">claimlayer-crew-1<`, `href="/base/garden/c/claimlayer/general"`)
	mustNotContain(t, body, "cl-task1", "Claimlayer member task")
	if a, b := strings.Index(body, "m-cl-rep1"), strings.Index(body, "m-cl-rep2"); a < 0 || b < a {
		t.Errorf("replies not oldest first")
	}

	for _, path := range []string{
		"/garden/c/claimlayer/crew/t/cl-wish1",    // the root is in another channel
		"/garden/c/claimlayer/general/t/cl-rep1",  // a reply is not a root
		"/garden/c/claimlayer/general/t/gd-wish1", // another land's thread
		"/garden/c/claimlayer/general/t/cl-task1", // not a conversation bead
	} {
		if code, _ := get(t, h, path, "claimlayer"); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}

// A thread past threadMax replies shows its newest threadMax, oldest first: a
// reply to a long thread is never cut off the end (gd-NRi4Fdu79J).
func TestThread_PastThreadMaxShowsTheNewest(t *testing.T) {
	d := commonstest.TwoLands(t0)
	cl := []string{"project:claimlayer"}
	for i := range threadMax + 10 {
		d.Put(&beadsapi.BeadDetail{ID: fmt.Sprintf("cl-long%04d", i), Type: "message", Status: "closed", Labels: cl,
			CreatedAt: t0.Add(time.Hour + time.Duration(i)*time.Second),
			Fields:    map[string]string{"channel_id": "cl-chgen", "thread_id": "cl-wish1", "author": "oidc:ann@claimlayer.example", "text": "more"}})
	}
	_, body := get(t, commonsServer(t, d), "/garden/c/claimlayer/general/t/cl-wish1", "claimlayer")
	mustContain(t, body, `id="m-cl-long0010"`, fmt.Sprintf(`id="m-cl-long%04d"`, threadMax+9))
	mustNotContain(t, body, `id="m-cl-long0009"`, `id="m-cl-rep1"`)
	if a, b := strings.Index(body, `id="m-cl-long0010"`), strings.Index(body, fmt.Sprintf(`id="m-cl-long%04d"`, threadMax+9)); b < a {
		t.Errorf("replies not oldest first")
	}
}

func TestMessageFragment(t *testing.T) {
	h := commonsServer(t, commonstest.TwoLands(t0))
	code, body := get(t, h, "/test/m/cl-rep1", "claimlayer")
	if code != http.StatusOK {
		t.Fatalf("GET message = %d", code)
	}
	mustContain(t, body, `<li class="cm-msg cm-msg--text cm-msg--reply" id="m-cl-rep1"`, `data-thread="cl-wish1"`, "on it")
	mustNotContain(t, body, "cm-thread-link") // a reply opens no thread of its own

	_, body = get(t, h, "/test/m/cl-msg1", "claimlayer")
	mustContain(t, body, `href="/base/garden/c/claimlayer/cl-chgen/t/cl-msg1">Reply</a>`)

	// A reply with no channel_id refetches in its root's channel, as the thread
	// shows it, so the event that names it does not 404.
	code, body = get(t, h, "/test/m/cl-rep2", "claimlayer")
	if code != http.StatusOK {
		t.Fatalf("GET legacy reply = %d: %s", code, body)
	}
	mustContain(t, body, `id="m-cl-rep2"`, `data-channel="cl-chgen"`, `data-thread="cl-wish1"`, "legacy reply")

	for _, id := range []string{"gd-msg1", "nl-msg1", "xx-msg1", "cl-task1", "cl-chgen", "zz-none"} {
		if code, body := get(t, h, "/test/m/"+id, "claimlayer"); code != http.StatusNotFound {
			t.Errorf("scoped GET /garden/c/m/%s = %d, want 404: %s", id, code, body)
		}
	}
	// The unrestricted reader still sees the other land, but never a bead with
	// no single land.
	if code, _ := get(t, h, "/test/m/gd-msg1", ""); code != http.StatusOK {
		t.Errorf("admin GET gd-msg1 = %d", code)
	}
	for _, id := range []string{"nl-msg1", "xx-msg1"} {
		if code, _ := get(t, h, "/test/m/"+id, ""); code != http.StatusNotFound {
			t.Errorf("admin GET %s = %d, want 404", id, code)
		}
	}
}

func TestNoScopeAdmitsNothing(t *testing.T) {
	h := &Handler{Beads: commonstest.TwoLands(t0)}
	mux := http.NewServeMux()
	h.Register(mux)
	if code, _ := get(t, mux, "/garden/c/claimlayer/general/t/cl-wish1", ""); code != http.StatusNotFound {
		t.Errorf("nil Scope GET thread = %d, want 404", code)
	}
}

func TestLinkify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain <i>", "plain &lt;i&gt;"},
		{"see https://x.example/a/b/c.", `see <a class="cm-link" href="https://x.example/a/b/c" title="https://x.example/a/b/c" target="_blank" rel="noopener noreferrer">x.example/…/b/c</a>.`},
		{"(https://x.example)", `(<a class="cm-link" href="https://x.example" title="https://x.example" target="_blank" rel="noopener noreferrer">x.example</a>)`},
		{"bead gd-AbCdEfGhIj ok", `bead <a class="cm-bead" href="/b/garden?bead=gd-AbCdEfGhIj" data-bead="gd-AbCdEfGhIj" title="Staging IdP &lt;edge&gt; (gd-AbCdEfGhIj)">Staging IdP &lt;edge&gt;</a> ok`},
		{"unread gd-ZyXwVuTsRq ok", `unread <a class="cm-bead" href="/b/garden?bead=gd-ZyXwVuTsRq" data-bead="gd-ZyXwVuTsRq" title="gd-ZyXwVuTsRq">gd-ZyXwVuTsRq</a> ok`},
		{"gone gd-GoneGoneGo ok", `gone <a class="cm-bead cm-bead--missing" href="/b/garden?bead=gd-GoneGoneGo" data-bead="gd-GoneGoneGo" title="gd-GoneGoneGo: not found" data-titles="off">Bead not found <span class="cm-bead-id">gd-GoneGoneGo</span></a> ok`},
		{"words aa-abcdefghij stay", "words aa-abcdefghij stay"},
		{`x https://a.example/"><script>`, `x <a class="cm-link" href="https://a.example/" title="https://a.example/" target="_blank" rel="noopener noreferrer">a.example</a>&#34;&gt;&lt;script&gt;`},
	}
	names := beadNames{"gd-AbCdEfGhIj": "Staging IdP <edge>", "gd-GoneGoneGo": ""}
	for _, c := range cases {
		if got := string(linkify(c.in, "/b", names)); got != c.want {
			t.Errorf("linkify(%q) =\n %s\nwant\n %s", c.in, got, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	d := commonstest.TwoLands(t0)
	cases := map[string]struct {
		kind, channel, thread string
		ok                    bool
	}{
		"cl-chgen": {kind: "channel", channel: "cl-chgen", ok: true},
		"cl-wish1": {kind: "message", channel: "cl-chgen", ok: true},
		"cl-msg1":  {kind: "message", channel: "cl-chgen", ok: true},
		"cl-rep1":  {kind: "message", channel: "cl-chgen", thread: "cl-wish1", ok: true},
		"cl-rep2":  {kind: "message", thread: "cl-wish1", ok: true},
		"cl-card1": {kind: "card", channel: "cl-chgen", ok: true},
		"cl-task1": {ok: false}, // a thread's work is not conversation
	}
	for id, want := range cases {
		inv, ok := classify(commonstest.Payload(d.Beads[id]))
		if ok != want.ok || inv.Kind != want.kind || inv.Channel != want.channel || inv.Thread != want.thread {
			t.Errorf("classify(%s) = %+v %v, want %+v", id, inv, ok, want)
		}
	}
	if _, ok := classify([]byte(`{"bead_id":"x"}`)); ok {
		t.Error("a payload with no bead classified")
	}
}

// liveSource is a Source the test feeds by hand.
type liveSource chan beadsapi.SSEEvent

func (s liveSource) open(context.Context) (<-chan beadsapi.SSEEvent, func()) { return s, func() {} }

func TestHubCutsPerSubscriber(t *testing.T) {
	d := commonstest.TwoLands(t0)
	// The payload claims claimlayer; the daemon says gardenio. The hub must
	// believe the daemon.
	liar := *d.Beads["gd-msg1"]
	liar.Labels = []string{"project:claimlayer"}

	src := make(liveSource, 16)
	hub := NewHub(src.open, d, nil)
	hub.Start()
	defer hub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin, _ := hub.Subscribe(ctx, adminScope)
	scoped, _ := hub.Subscribe(ctx, claimlayerScope)
	none, _ := hub.Subscribe(ctx, Scope{})

	for _, id := range []string{"gd-msg1", "nl-msg1", "xx-msg1", "cl-task1", "cl-rep1"} {
		src <- beadsapi.SSEEvent{Event: "beads.bead.created", Data: commonstest.Payload(d.Beads[id])}
	}
	src <- beadsapi.SSEEvent{Event: "beads.bead.updated", Data: commonstest.Payload(&liar)}
	src <- beadsapi.SSEEvent{Event: "beads.bead.created", Data: commonstest.Payload(d.Beads["cl-msg1"])}

	recv := func(ch <-chan Invalidation, n int) []string {
		var ids []string
		for len(ids) < n {
			select {
			case inv := <-ch:
				ids = append(ids, inv.ID+"@"+inv.Land)
			case <-time.After(2 * time.Second):
				t.Fatalf("got %v, waiting for %d", ids, n)
			}
		}
		return ids
	}
	if got := strings.Join(recv(admin, 4), " "); got != "gd-msg1@gardenio cl-rep1@claimlayer gd-msg1@gardenio cl-msg1@claimlayer" {
		t.Errorf("admin got %s", got)
	}
	if got := strings.Join(recv(scoped, 2), " "); got != "cl-rep1@claimlayer cl-msg1@claimlayer" {
		t.Errorf("scoped got %s", got)
	}
	select {
	case inv := <-none:
		t.Errorf("a scope with no land got %+v", inv)
	case <-time.After(50 * time.Millisecond):
	}
	// Stop the worker before reading the daemon's counters: it may still be
	// reading for the steward response SLI after the last broadcast.
	hub.Close()
	if d.Reads["system"] == 0 {
		t.Error("the hub did not attribute through system reads")
	}
}

func TestNilHub(t *testing.T) {
	var h *Hub
	h.Start()
	ch, cancel := h.Subscribe(context.Background(), adminScope)
	if _, ok := <-ch; ok {
		t.Error("nil hub channel is open")
	}
	cancel()
	h.Close()
	if NewHub(nil, nil, nil) != nil {
		t.Error("NewHub without a source is not nil")
	}
}

// stream runs the event stream until it has written until (or a second
// passes) and returns what it wrote.
func stream(t *testing.T, h *Handler, path, bearer, lastEventID string, until string) string {
	t.Helper()
	return streamOpened(t, h, path, bearer, lastEventID, until, "", nil)
}

// streamOpened is stream that runs opened, once and on its own goroutine, as
// soon as the body carries open.
func streamOpened(t *testing.T, h *Handler, path, bearer, lastEventID, until, open string, opened func()) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(beadsapi.WithUserBearer(context.Background(), bearer), time.Second)
	defer cancel()
	r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	if lastEventID != "" {
		r.Header.Set("Last-Event-ID", lastEventID)
	}
	rec := &syncRecorder{ResponseRecorder: httptest.NewRecorder(), until: until, done: cancel, open: open, opened: opened}
	h.events(rec, r)
	return rec.Body.String()
}

// syncRecorder ends the stream once its body carries until, and calls opened
// the first time it carries open.
type syncRecorder struct {
	*httptest.ResponseRecorder
	until  string
	done   func()
	open   string
	opened func()
}

func (s *syncRecorder) Flush() {
	s.ResponseRecorder.Flush()
	body := s.Body.String()
	if s.opened != nil && strings.Contains(body, s.open) {
		go s.opened()
		s.opened = nil
	}
	if s.until != "" && strings.Contains(body, s.until) {
		s.done()
	}
}

func TestEventsReplay(t *testing.T) {
	d := commonstest.TwoLands(t0)
	scopeOf := func(r *http.Request) Scope {
		if beadsapi.UserBearerFromContext(r.Context()) == commonstest.ClaimlayerBearer {
			return claimlayerScope
		}
		return adminScope
	}
	h := &Handler{Beads: d, Scope: scopeOf, Now: func() time.Time { return t0.Add(30 * time.Minute) }}

	since := t0.Add(150 * time.Second).Format(time.RFC3339) // after cl-msg1 (2m), before cl-rep1 (3m) once the slack is taken
	body := stream(t, h, "/garden/c/events?since="+since, commonstest.ClaimlayerBearer, "", "event: replayed")
	mustContain(t, body, "event: message\ndata: {\"id\":\"cl-rep1\",\"channel\":\"cl-chgen\",\"thread\":\"cl-wish1\",\"land\":\"claimlayer\"}",
		"event: card\ndata: {\"id\":\"cl-card1\"", `"id":"cl-crew1"`, `"id":"cl-rep2"`, "event: replayed\ndata: {\"count\":5}")
	mustNotContain(t, body, `"id":"cl-msg1"`, "cl-task1", "reset")
	mustNotContain(t, body, commonstest.Leaks...)
	if a, b := strings.Index(body, "cl-rep1"), strings.Index(body, "cl-card1"); b < a {
		t.Errorf("replay not oldest first:\n%s", body)
	}
	if !strings.Contains(body, "id: "+t0.Add(3*time.Minute).Format(time.RFC3339Nano)+"\nevent: message") {
		t.Errorf("replayed event not stamped with its log time:\n%s", body)
	}

	// The browser's Last-Event-ID works as since, and the admin sees both lands
	// but never a land-less bead.
	body = stream(t, h, "/garden/c/events", commonstest.AdminBearer, t0.Add(150*time.Second).Format(time.RFC3339Nano), "event: replayed")
	mustContain(t, body, `"id":"gd-rep1"`, `"id":"cl-rep1"`)
	mustNotContain(t, body, "nl-msg1", "xx-msg1")

	// Too far back: a reset, then what the window holds.
	body = stream(t, h, "/garden/c/events?since="+t0.Add(-2*time.Hour).Format(time.RFC3339), commonstest.ClaimlayerBearer, "", "event: replayed")
	mustContain(t, body, "event: reset")

	// The browser's own reconnect of a stream opened with ?since= (the pane
	// moving between channels) sends a newer Last-Event-ID, and that wins.
	body = stream(t, h, "/garden/c/events?since="+t0.Add(-2*time.Hour).Format(time.RFC3339), commonstest.ClaimlayerBearer, since, "event: replayed")
	mustNotContain(t, body, "event: reset", `"id":"cl-msg1"`)
	mustContain(t, body, `"id":"cl-rep1"`)

	// No since: no replay, straight to live, stamped at once so a page asleep before the first heartbeat resumes from here.
	body = stream(t, h, "/garden/c/events", commonstest.ClaimlayerBearer, "", "event: ping")
	mustNotContain(t, body, "replayed", "event: message")
	mustContain(t, body, "id: "+t0.Add(30*time.Minute).Format(time.RFC3339Nano)+"\nevent: ping")
}

func TestEventsLive(t *testing.T) {
	d := commonstest.TwoLands(t0)
	src := make(liveSource, 4)
	hub := NewHub(src.open, d, nil)
	hub.Start()
	defer hub.Close()
	h := &Handler{Beads: d, Hub: hub, Scope: func(*http.Request) Scope { return claimlayerScope }}

	go func() {
		time.Sleep(50 * time.Millisecond)
		src <- beadsapi.SSEEvent{Data: commonstest.Payload(d.Beads["gd-msg1"])}
		src <- beadsapi.SSEEvent{Data: commonstest.Payload(d.Beads["cl-msg1"])}
	}()
	body := stream(t, h, "/garden/c/events", commonstest.ClaimlayerBearer, "", `"id":"cl-msg1"`)
	mustContain(t, body, "event: message\ndata: {\"id\":\"cl-msg1\",\"channel\":\"cl-chgen\",\"land\":\"claimlayer\"}")
	mustNotContain(t, body, "gd-msg1")
}

// TestMessageFiles pins a message's attachments (gd-2Ie1cDAm4K): each opens
// from a link, a file-only message shows no empty text, and a URL that is not
// absolute http(s) is never linked.
func TestMessageFiles(t *testing.T) {
	d := commonstest.TwoLands(t0)
	cl := []string{"project:claimlayer"}
	d.Put(&beadsapi.BeadDetail{ID: "cl-files1", Type: "message", Title: "Shared plot.png", Status: "closed", Labels: cl, CreatedAt: t0,
		Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:ada@claimlayer.example", "origin": "slack",
			"files": `[{"name":"plot.png","mimetype":"image/png","size":2048,"url":"https://acme.slack.com/files/U1/F1/plot.png"},` +
				`{"name":"evil","url":"javascript:alert(1)"}]`}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-files2", Type: "message", Title: "the spec", Status: "closed", Labels: cl, CreatedAt: t0,
		Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:ada@claimlayer.example", "text": "the spec",
			"files": `[{"name":"spec.pdf","url":"https://acme.slack.com/files/U1/F2/spec.pdf"}]`}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-files3", Type: "message", Title: "Shared x", Status: "closed", Labels: cl, CreatedAt: t0,
		Fields: map[string]string{"channel_id": "cl-chgen", "author": "oidc:ada@claimlayer.example", "files": `not json`}})
	h := commonsServer(t, d)

	_, body := get(t, h, "/test/m/cl-files1", "claimlayer")
	mustContain(t, body, `<ul class="cm-files" aria-label="Attachments">`,
		`<a class="cm-link" href="https://acme.slack.com/files/U1/F1/plot.png" target="_blank" rel="noopener noreferrer">plot.png</a>`,
		`<span class="cm-file-meta">image · 2.0 KB</span>`)
	mustNotContain(t, body, `class="cm-text"`, "Shared plot.png", "javascript:", "evil")

	_, body = get(t, h, "/test/m/cl-files2", "claimlayer")
	mustContain(t, body, `<p class="cm-text">the spec</p>`, `>spec.pdf</a> <span class="cm-file-meta">file</span>`)

	// A malformed field reads as no files: the title is the message.
	_, body = get(t, h, "/test/m/cl-files3", "claimlayer")
	mustContain(t, body, `<p class="cm-text">Shared x</p>`)
	mustNotContain(t, body, "cm-files")
}

// TestNoticeFiles pins the notice face beside attachments (gd-zOFaT9VFUc): a
// notice shows its face, never its fallback text, and still lists any files.
func TestNoticeFiles(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Put(&beadsapi.BeadDetail{ID: "cl-nfiles", Type: "message", Title: "Agent died: crew-9", Status: "closed",
		Labels: []string{"project:claimlayer"}, CreatedAt: t0,
		Fields: map[string]string{"channel_id": "cl-chgen", "kind": "notice", "author": "agent:gardenio-forensics",
			"text": "Agent died: crew-9", "notice": "death", "notice_agent": "crew-9", "notice_cause": "pod deleted",
			"files": `[{"name":"last.log","url":"https://acme.slack.com/files/U1/F3/last.log"}]`}})
	h := commonsServer(t, d)

	_, body := get(t, h, "/test/m/cl-nfiles", "claimlayer")
	mustContain(t, body, `data-notice="death"`, `<span class="cm-notice-what">Agent died</span>`,
		`<ul class="cm-files" aria-label="Attachments">`, `>last.log</a>`)
	mustNotContain(t, body, `class="cm-text"`)
}

// A fragment route's Server-Timing splits its daemon reads from its template (gd-7jlAbyk8Fy); the unread count, a
// JSON route, splits its reads from its encode.
func TestServerTiming(t *testing.T) {
	h := commonsServer(t, commonstest.TwoLands(t0))
	for path, want := range map[string]string{
		"/garden/c/claimlayer/general/t/cl-wish1": `^kbeads;dur=[\d.]+, template;dur=[\d.]+, total;dur=[\d.]+$`,
		"/garden/c/unread":                        `^kbeads;dur=[\d.]+, encode;dur=[\d.]+, total;dur=[\d.]+$`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		if st := rec.Header().Get("Server-Timing"); !regexp.MustCompile(want).MatchString(st) {
			t.Errorf("GET %s: Server-Timing = %q, want %s", path, st, want)
		}
	}
}
