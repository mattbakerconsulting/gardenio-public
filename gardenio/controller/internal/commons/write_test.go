package commons

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
	"gardenio/controller/internal/spawnguard"
)

// post sends a write as the test principal: as names the scope ("claimlayer"
// or admin), and hdr adds X-Test-* headers (Self, Guest, Anon).
func post(t *testing.T, h http.Handler, path, as, text string, hdr ...string) (int, string) {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(url.Values{"text": {text}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if as != "" {
		r.Header.Set("X-Test-As", as)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

func lastCreated(t *testing.T, d *commonstest.Daemon) (beadsapi.CreateBeadRequest, map[string]string) {
	t.Helper()
	if len(d.Created) == 0 {
		t.Fatal("nothing was created")
	}
	req := d.Created[len(d.Created)-1]
	var f map[string]string
	if err := json.Unmarshal(req.Fields, &f); err != nil {
		t.Fatal(err)
	}
	return req, f
}

func TestSayWritesAClosedCommonsMessage(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	code, body := post(t, h, "/garden/c/claimlayer/general/say", "claimlayer", "  hello <there>  ")
	if code != http.StatusCreated {
		t.Fatalf("say = %d %s", code, body)
	}
	req, f := lastCreated(t, d)
	if req.Type != "message" || req.Status != "closed" || !slices.Equal(req.Labels, []string{"project:claimlayer"}) || req.CreatedBy != "oidc:bo@claimlayer.example" {
		t.Errorf("bead = %+v", req)
	}
	want := map[string]string{"channel_id": "cl-chgen", "author": "oidc:bo@claimlayer.example", "kind": "text", "text": "hello <there>", "origin": "commons"}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("fields[%s] = %q, want %q (all: %v)", k, f[k], v, f)
		}
	}
	if _, ok := f["thread_id"]; ok {
		t.Errorf("a top-level message carries thread_id: %v", f)
	}
	// The answer is the message as the stream carries it, with its thread link.
	mustContain(t, body, `<li class="cm-msg cm-msg--text" id="m-cm-new1"`, "hello &lt;there&gt;", `href="/base/garden/c/claimlayer/general/t/cm-new1">Reply<`)
	// And the daemon holds it.
	_, m := get(t, h, "/test/m/cm-new1", "claimlayer")
	mustContain(t, m, `id="m-cm-new1"`, "hello &lt;there&gt;")
}

// A message may name the bead it asks about (the workbench's "Ask the steward
// about this", gd-LisW87zrnf): an id is kept as fields.about, anything else is
// dropped rather than refused.
func TestSayKeepsTheBeadItIsAbout(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	for _, c := range []struct{ about, want string }{{" gd-wbEpic ", "gd-wbEpic"}, {"not an id", ""}, {"", ""}} {
		r := httptest.NewRequest("POST", "/garden/c/claimlayer/general/say", strings.NewReader(url.Values{"text": {"@steward why?"}, "about": {c.about}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Test-As", "claimlayer")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusCreated {
			t.Fatalf("say about %q = %d %s", c.about, rec.Code, rec.Body)
		}
		if _, f := lastCreated(t, d); f["about"] != c.want {
			t.Errorf("about %q: fields[about] = %q, want %q", c.about, f["about"], c.want)
		}
	}
}

func TestReplyInASlackWishRidesTheThreadForward(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	code, body := post(t, h, "/garden/c/t/cl-wish1/reply", "claimlayer", "on my way")
	if code != http.StatusCreated {
		t.Fatalf("reply = %d %s", code, body)
	}
	req, f := lastCreated(t, d)
	if !slices.Equal(req.Labels, []string{"project:claimlayer", LabelThreadSay, "thread:cl-wish1"}) {
		t.Errorf("labels = %v", req.Labels)
	}
	if f["thread_id"] != "cl-wish1" || f["channel_id"] != "cl-chgen" || f["slack_thread_channel"] != "C0CLAIM01" || f["slack_thread_ts"] != "1792400000.000100" || f["origin"] != "commons" {
		t.Errorf("fields = %v", f)
	}
	mustContain(t, body, `class="cm-msg cm-msg--text cm-msg--reply" id="m-cm-new1"`, `data-thread="cl-wish1"`)
	mustNotContain(t, body, "cm-thread-link")

	// A chat root with no Slack thread: a plain Commons reply.
	if code, body := post(t, h, "/garden/c/t/cl-msg1/reply", "claimlayer", "plain"); code != http.StatusCreated {
		t.Fatalf("reply = %d %s", code, body)
	}
	req, f = lastCreated(t, d)
	if !slices.Equal(req.Labels, []string{"project:claimlayer"}) || f["thread_id"] != "cl-msg1" || f["slack_thread_ts"] != "" {
		t.Errorf("plain reply = %v %v", req.Labels, f)
	}
}

func TestWritesRefused(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	long := strings.Repeat("x", textMaxBytes+1)
	for _, c := range []struct {
		name, path, as, text string
		hdr                  []string
		want                 int
		says                 string
	}{
		{"empty", "/garden/c/claimlayer/general/say", "claimlayer", "   ", nil, http.StatusBadRequest, "Write something"},
		{"too long", "/garden/c/claimlayer/general/say", "claimlayer", long, nil, http.StatusRequestEntityTooLarge, "too long"},
		{"anonymous", "/garden/c/claimlayer/general/say", "claimlayer", "hi", []string{"X-Test-Anon", "1"}, http.StatusUnauthorized, "Sign in"},
		{"another land", "/garden/c/gardenio/general/say", "claimlayer", "hi", nil, http.StatusNotFound, ""},
		{"not a member", "/garden/c/claimlayer/build/say", "claimlayer", "hi", nil, http.StatusForbidden, "Join #build"},
		{"reply to a reply", "/garden/c/t/cl-rep1/reply", "claimlayer", "hi", nil, http.StatusNotFound, ""},
		{"reply in another land", "/garden/c/t/gd-wish1/reply", "claimlayer", "hi", nil, http.StatusNotFound, ""},
		{"reply to no bead", "/garden/c/t/cl-nothing/reply", "claimlayer", "hi", nil, http.StatusNotFound, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := len(d.Created)
			code, body := post(t, h, c.path, c.as, c.text, c.hdr...)
			if code != c.want || !strings.Contains(body, c.says) {
				t.Errorf("%s = %d %q, want %d %q", c.path, code, body, c.want, c.says)
			}
			if len(d.Created) != before {
				t.Errorf("a refused write created a bead: %+v", d.Created[before:])
			}
		})
	}

	// A member of #build posts there; kbeads' own refusal passes through with its words.
	d.Selves = map[string]string{commonstest.ClaimlayerBearer: "oidc:ann@claimlayer.example"}
	if code, body := post(t, h, "/garden/c/claimlayer/build/say", "claimlayer", "hi", "X-Test-Self", "oidc:ann@claimlayer.example"); code != http.StatusCreated {
		t.Errorf("member say = %d %s", code, body)
	}
	d.FailWrites = &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "a guest may not do that"}
	if code, body := post(t, h, "/garden/c/claimlayer/general/say", "claimlayer", "hi"); code != http.StatusForbidden || !strings.Contains(body, "a guest may not do that") {
		t.Errorf("daemon refusal = %d %q", code, body)
	}

	// No writer: the routes answer 503 and the fragments offer no composer.
	nw := &Handler{Beads: d, BasePath: "/base", Scope: func(*http.Request) Scope { return adminScope }, Gate: testGate, EmptyPrompt: testPrompt}
	mux := http.NewServeMux()
	nw.Register(mux)
	if code, _ := post(t, mux, "/garden/c/claimlayer/general/say", "", "hi"); code != http.StatusServiceUnavailable {
		t.Errorf("no writer = %d", code)
	}
	_, th := get(t, mux, "/garden/c/claimlayer/general/t/cl-wish1", "")
	mustContain(t, th, `data-thread="cl-wish1"`)
	mustNotContain(t, th, "cm-compose")
}

func TestJoinAndLeave(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	members := func() []string {
		var m []string
		_ = json.Unmarshal([]byte(d.Beads["cl-chbuild"].Fields["members"]), &m)
		return m
	}

	withBuildThread(d)
	const thread = "/garden/c/claimlayer/build/t/cl-bld1"

	// bo joins #build: a thread there now offers the composer.
	code, body := post(t, h, "/garden/c/claimlayer/build/join", "claimlayer", "")
	if code != http.StatusNoContent || body != "" {
		t.Fatalf("join = %d %q, want 204", code, body)
	}
	if got := members(); !slices.Equal(got, []string{"oidc:ann@claimlayer.example", "oidc:bo@claimlayer.example"}) {
		t.Errorf("members after join = %v", got)
	}
	_, th := get(t, h, thread, "claimlayer")
	mustContain(t, th, `data-commons="compose"`, "(the house voice)")
	mustNotContain(t, th, `data-commons="join"`)

	// Joining again changes nothing.
	writes := len(d.Updated)
	if code, _ := post(t, h, "/garden/c/claimlayer/build/join", "claimlayer", ""); code != http.StatusNoContent || len(d.Updated) != writes {
		t.Errorf("second join = %d, %d writes", code, len(d.Updated)-writes)
	}

	// And leaves: Join is offered again.
	code, body = post(t, h, "/garden/c/claimlayer/build/leave", "claimlayer", "")
	if code != http.StatusNoContent || body != "" {
		t.Fatalf("leave = %d %q, want 204", code, body)
	}
	if got := members(); !slices.Equal(got, []string{"oidc:ann@claimlayer.example"}) {
		t.Errorf("members after leave = %v", got)
	}
	_, th = get(t, h, thread, "claimlayer")
	mustContain(t, th, `data-commons="join"`, "Join #build")
	mustNotContain(t, th, `data-commons="compose"`)

	// A guest may not join; a default channel has no members; another land is not there.
	if code, _ := post(t, h, "/garden/c/claimlayer/build/join", "claimlayer", "", "X-Test-Guest", "1"); code != http.StatusForbidden {
		t.Errorf("guest join = %d", code)
	}
	_, th = get(t, h, thread, "claimlayer")
	mustContain(t, th, `data-commons="join"`)
	if code, _ := post(t, h, "/garden/c/claimlayer/general/join", "claimlayer", ""); code != http.StatusConflict {
		t.Errorf("join #general = %d", code)
	}
	if code, _ := post(t, h, "/garden/c/gardenio/general/join", "claimlayer", ""); code != http.StatusNotFound {
		t.Errorf("join in another land = %d", code)
	}
}

func TestComposerStates(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	// The thread composer replies into the root.
	_, th := get(t, h, "/garden/c/claimlayer/general/t/cl-wish1", "claimlayer")
	mustContain(t, th, `action="/base/garden/c/t/cl-wish1/reply"`, `aria-label="Reply in thread"`, `placeholder="(the house voice)"`)
	// The line saying where the words go (gd-EvK4BmuKos), then its slots for a gardener's thread (web/chat.js fills them),
	// ahead of the field so Tab meets them first; empty and hidden as served.
	mustContain(t, th, `data-commons="compose">
<p class="cm-to" data-slot="to" title="Reply in thread" aria-hidden="true">Reply in thread</p>
<div class="cm-about" data-slot="about" hidden></div>
<div class="cm-quick" data-slot="quick" role="group" aria-label="Quick replies" hidden></div>
<textarea`)
	// A thread read signed out: the note, no composer, and no slots, since they live in the composer.
	r := httptest.NewRequest("GET", "/garden/c/claimlayer/general/t/cl-wish1", nil)
	r.Header.Set("X-Test-As", "claimlayer")
	r.Header.Set("X-Test-Anon", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	mustContain(t, rec.Body.String(), "Sign in to post here.")
	mustNotContain(t, rec.Body.String(), "cm-compose")
	mustNotContain(t, rec.Body.String(), `data-slot="about"`)
	mustNotContain(t, rec.Body.String(), `data-slot="quick"`)
}

// Every address the band had goes to the workbench (gd-ZeiSZV7j2Y): a navigation to a channel goes to its land's
// steward, a thread to its node, the channel list to the viewer's steward and a message link to its thread's node,
// so links in Slack and in beads keep working. The workbench's own addresses get the page, and its fragment
// fetches (X-Commons) and plain GETs of a channel, thread or node get the fragment.
func TestBandAddressesGoToTheWorkbench(t *testing.T) {
	d := commonstest.TwoLands(t0)
	served := 0
	h := &Handler{
		Beads: d, Nodes: d, BasePath: "/base", Scope: func(*http.Request) Scope { return adminScope }, Gate: testGate,
		Page: func(w http.ResponseWriter, r *http.Request) bool {
			served++
			_, _ = w.Write([]byte("<!doctype html><title>Workbench</title>"))
			return true
		},
	}
	mux := http.NewServeMux()
	h.Register(mux)
	do := func(path string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}
	nav := "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	for path, want := range map[string]string{
		"/garden/c":                               "/base/garden/c/s",
		"/garden/c/claimlayer/general":            "/base/garden/c/s/claimlayer",
		"/garden/c/claimlayer/cl-chgen":           "/base/garden/c/s/claimlayer",
		"/garden/c/my%20land/general":             "/base/garden/c/s/my%20land",
		"/garden/c/claimlayer/general/t/cl-wish1": "/base/garden/c/n/cl-wish1",
		"/garden/c/m/cl-rep1":                     "/base/garden/c/n/cl-wish1",
		"/garden/c/m/cl-msg1":                     "/base/garden/c/n/cl-msg1",
	} {
		rec := do(path, "Accept", nav)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
			t.Errorf("navigation to %s = %d to %q, want 302 to %q", path, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	// The channel list, a channel and a message link answer with the redirect whoever asks: none has a fragment any more.
	for _, path := range []string{"/garden/c", "/garden/c/claimlayer/general", "/garden/c/m/cl-rep1"} {
		if rec := do(path, "X-Commons", "fragment"); rec.Code != http.StatusFound {
			t.Errorf("fragment fetch of %s = %d, want 302", path, rec.Code)
		}
	}
	if rec := do("/garden/c/m/zz-none", "Accept", nav); rec.Code != http.StatusNotFound {
		t.Errorf("a link to a message that is not there = %d, want 404", rec.Code)
	}
	for _, p := range []string{"/garden/c/claimlayer/general/t/cl-wish1", "/garden/c/n/cl-wish1"} {
		for _, hdr := range [][]string{{"Accept", nav, "X-Commons", "fragment"}, nil} {
			rec := do(p, hdr...)
			if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, `data-commons=`) {
				t.Errorf("GET %s %v = %d %q, want the fragment", p, hdr, rec.Code, body)
			}
			if v := rec.Header().Values("Vary"); !slices.Contains(v, "Accept, X-Commons") {
				t.Errorf("%s: Vary = %v", p, v)
			}
		}
	}
	if body := do("/garden/c/n/cl-wish1", "Accept", nav).Body.String(); !strings.Contains(body, "Workbench") || served != 1 {
		t.Errorf("navigation to a node = %q (page served %d times), want the workbench's page", body, served)
	}
}

// A root refetched on its own carries its reply count: the pane refetches it
// after a reply event to move "N replies".
func TestMessageFragmentCountsReplies(t *testing.T) {
	h := commonsServer(t, commonstest.TwoLands(t0))
	_, body := get(t, h, "/test/m/cl-wish1", "claimlayer")
	mustContain(t, body, `class="cm-replies">1 reply</a>`, `datetime="2026-10-19T09:03:00Z"`)
	if code, _ := post(t, h, "/garden/c/t/cl-wish1/reply", "claimlayer", "another"); code != http.StatusCreated {
		t.Fatalf("reply = %d", code)
	}
	_, body = get(t, h, "/test/m/cl-wish1", "claimlayer")
	mustContain(t, body, `class="cm-replies">2 replies</a>`)
}

func TestSayAndReplyAreRateLimitedPerMember(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d, func(h *Handler) { h.Rate = spawnguard.NewKeyed(2, time.Minute) })
	for i := 0; i < 2; i++ {
		if code, body := post(t, h, "/garden/c/claimlayer/general/say", "claimlayer", "hi"); code != http.StatusCreated {
			t.Fatalf("message %d under the cap = %d %s", i, code, body)
		}
	}
	made := len(d.Created)
	// Says and replies share the member's cap.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/garden/c/t/cl-msg1/reply", strings.NewReader("text=again"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Test-As", "claimlayer")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("third message in a minute = %d (Retry-After %q) %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "2 messages in a minute at most")
	if len(d.Created) != made {
		t.Errorf("a rate-limited message was written: %d beads, want %d", len(d.Created), made)
	}
	// Another member of the land has their own cap.
	if code, body := post(t, h, "/garden/c/claimlayer/general/say", "claimlayer", "me too", "X-Test-Self", "oidc:ann@claimlayer.example"); code != http.StatusCreated {
		t.Errorf("another member = %d %s", code, body)
	}
}

// A farm snapshot (gd-GbF2JS04h5): the picture is a file, and the view's
// context rides beside it as the context field, kept as fields.snap with only
// the keys this package knows, the framed list capped and the view link made
// absolute. A snapshot with no words is titled after the place.
func TestSaySnapKeepsTheViewContext(t *testing.T) {
	d := commonstest.TwoLands(t0)
	m := &memFiles{}
	h := commonsServer(t, d, withFiles(m))
	img := part{name: "file", file: "farm-claimlayer-cl-p1-20261019-0930.png", ct: "image/png", body: pngBytes}
	framed := make([]map[string]string, 0, 30)
	framed = append(framed, map[string]string{"kind": "weed", "id": "w1"}, map[string]string{"kind": "plan"}) // dropped: not a kind, no id
	for i := range 28 {
		framed = append(framed, map[string]string{"kind": []string{"project", "plan", "bead"}[i%3], "id": "cl-" + strconv.Itoa(i), "title": "thing", "secret": "x"})
	}
	ctx := map[string]any{
		"kind": "farm", "url": "/base/garden?at=claimlayer/cl-p1&z=2#frag", "at": "claimlayer/cl-p1", "z": 2, "who": "mine",
		"night": true, "framed": framed, "vp": []int{1280, 800}, "taken_at": "2026-10-19T09:30:00+02:00", "evil": "<script>",
	}
	raw, _ := json.Marshal(ctx)
	code, body := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer", part{name: "text", body: []byte(" ")}, img, part{name: "context", body: raw})
	if code != http.StatusCreated {
		t.Fatalf("say = %d %s", code, body)
	}
	req, f := lastCreated(t, d)
	if req.Title != "Snapshot of the farm at claimlayer/cl-p1" {
		t.Errorf("title = %q", req.Title)
	}
	if _, ok := f["text"]; ok {
		t.Errorf("a snapshot with no words carries text: %v", f)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(f["snap"]), &got); err != nil {
		t.Fatalf("snap field %q: %v", f["snap"], err)
	}
	if _, ok := got["evil"]; ok {
		t.Errorf("an unknown key reached the bead: %v", got)
	}
	if got["url"] != "http://example.com/base/garden?at=claimlayer/cl-p1&z=2" || got["at"] != "claimlayer/cl-p1" || got["z"] != 2.0 ||
		got["who"] != "mine" || got["night"] != true || got["taken_at"] != "2026-10-19T07:30:00Z" || got["kind"] != "farm" {
		t.Errorf("snap = %v", got)
	}
	if fr, _ := got["framed"].([]any); len(fr) != snapFramedMax {
		t.Errorf("framed = %d entries, want %d", len(fr), snapFramedMax)
	} else if first, _ := fr[0].(map[string]any); first["kind"] != "project" || first["id"] != "cl-0" || first["secret"] != nil {
		t.Errorf("framed[0] = %v", first)
	}
	mustContain(t, body, ` · <span class="cm-snap">Farm at claimlayer/cl-p1 (z2, Mine, night) · 8 projects, 8 plans, 8 beads framed · <a class="cm-link cm-snap-view" href="http://example.com/base/garden?at=claimlayer/cl-p1&amp;z=2">Open the view</a></span></figcaption>`)
	mustNotContain(t, body, `class="cm-text"`)

	// With words, the words are the title and the text.
	raw, _ = json.Marshal(map[string]any{"kind": "farm", "at": "yard", "url": "https://evil.example/base/garden"})
	if code, body := postParts(t, h, "/garden/c/t/cl-msg1/reply", "claimlayer", part{name: "text", body: []byte("look at this")}, img, part{name: "context", body: raw}); code != http.StatusCreated {
		t.Fatalf("reply = %d %s", code, body)
	} else {
		mustContain(t, body, ` · <span class="cm-snap">Farm at the Yard</span></figcaption>`)
	}
	req, f = lastCreated(t, d)
	if req.Title != "look at this" || f["text"] != "look at this" || f["thread_id"] != "cl-msg1" || f["snap"] != `{"kind":"farm","at":"yard"}` {
		t.Errorf("reply bead = %+v fields %v (another origin's view link is dropped)", req, f)
	}

	// A context with no picture is not kept.
	raw, _ = json.Marshal(map[string]any{"kind": "farm", "at": "yard"})
	if code, _ := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer", part{name: "text", body: []byte("words")}, part{name: "context", body: raw}); code != http.StatusCreated {
		t.Fatalf("say without a file = %d", code)
	}
	if _, f = lastCreated(t, d); f["snap"] != "" {
		t.Errorf("a context without a file was kept: %v", f)
	}
}

// A crowded view: the farm frames snapFramedMax things with the longest titles
// web/commons.js sends (120 characters, non-ASCII), which is well over the old
// 2 KB cap (gd-eB8PbZaReC); the context must still be kept whole.
func TestSnapContextCrowdedViewFits(t *testing.T) {
	d := commonstest.TwoLands(t0)
	m := &memFiles{}
	h := commonsServer(t, d, withFiles(m))
	img := part{name: "file", file: "farm-claimlayer-yard-20261026-1200.png", ct: "image/png", body: pngBytes}
	title := strings.Repeat("ü", 120)
	framed := make([]map[string]string, 0, snapFramedMax)
	for i := range snapFramedMax {
		framed = append(framed, map[string]string{"kind": []string{"project", "plan", "bead"}[i%3], "id": "cl-" + strings.Repeat("x", 20) + strconv.Itoa(i), "title": title})
	}
	ctx := map[string]any{
		"kind": "farm", "url": "/base/garden?at=claimlayer/cl-p1&z=2", "at": "claimlayer/cl-p1", "z": 2, "who": "all",
		"night": false, "framed": framed, "vp": []int{1920, 1080}, "taken_at": "2026-10-26T12:00:00Z",
	}
	raw, _ := json.Marshal(ctx)
	if len(raw) <= 2<<10 {
		t.Fatalf("context is %d bytes, not over the old 2 KB cap", len(raw))
	}
	code, body := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer", img, part{name: "context", body: raw})
	if code != http.StatusCreated {
		t.Fatalf("say = %d %s", code, body)
	}
	_, f := lastCreated(t, d)
	var got map[string]any
	if err := json.Unmarshal([]byte(f["snap"]), &got); err != nil {
		t.Fatalf("snap field %q: %v", f["snap"], err)
	}
	if fr, _ := got["framed"].([]any); len(fr) != snapFramedMax {
		t.Errorf("framed = %d entries, want %d", len(fr), snapFramedMax)
	}
}

func TestSnapContextRefusalsStoreNothing(t *testing.T) {
	d := commonstest.TwoLands(t0)
	m := &memFiles{}
	h := commonsServer(t, d, withFiles(m))
	img := part{name: "file", file: "a.png", body: pngBytes}
	for name, c := range map[string]struct {
		raw  string
		code int
	}{
		"too large":  {`{"kind":"farm","at":"` + strings.Repeat("a", snapMaxBytes) + `"}`, http.StatusRequestEntityTooLarge},
		"an array":   {`[{"kind":"farm"}]`, http.StatusBadRequest},
		"a string":   {`"farm"`, http.StatusBadRequest},
		"not JSON":   {`{kind: farm}`, http.StatusBadRequest},
		"not a farm": {`{"kind":"plans"}`, http.StatusBadRequest},
	} {
		if code, body := postParts(t, h, "/garden/c/claimlayer/general/say", "claimlayer", img, part{name: "context", body: []byte(c.raw)}); code != c.code {
			t.Errorf("%s: %d %s, want %d", name, code, body, c.code)
		}
	}
	if k := m.keys(); len(k) != 0 || len(d.Created) != 0 {
		t.Fatalf("stored %v, created %d", k, len(d.Created))
	}
}

func TestSnapURL(t *testing.T) {
	h := &Handler{BasePath: "/base"}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "garden.example")
	for in, want := range map[string]string{
		"/base/garden":                     "https://garden.example/base/garden",
		"/base/garden?at=yard&z=1":         "https://garden.example/base/garden?at=yard&z=1",
		"/base/garden/c/cl/general?at=~":   "https://garden.example/base/garden/c/cl/general?at=~",
		"/garden":                          "",
		"/base/gardener":                   "",
		"//evil.example/base/garden":       "",
		"https://evil.example/base/garden": "",
		"javascript:alert(1)":              "",
		`/base/garden\..\x`:                "",
		"":                                 "",
	} {
		if got := h.snapURL(r, in); got != want {
			t.Errorf("snapURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The snapshot caption's fragment (render golden; -update rewrites it).
func TestSnapCaptionGolden(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Put(&beadsapi.BeadDetail{ID: "cl-snap1", Type: "message", Title: "Snapshot of the farm at claimlayer/cl-p1", Status: "closed",
		Labels: []string{"project:claimlayer"}, CreatedAt: t0.Add(5 * time.Minute), Fields: map[string]string{
			"channel_id": "cl-chgen", "author": "oidc:bo@claimlayer.example", "kind": "text", "origin": "commons",
			"files": `[{"name":"farm-claimlayer-cl-p1-20261019-0905.png","mimetype":"image/png","size":48213,"url":"http://example.com/base/garden/c/f/claimlayer/cl-chgen/0123456789abcdef0123456789abcdef/farm-claimlayer-cl-p1-20261019-0905.png"}]`,
			"snap":  `{"kind":"farm","url":"http://example.com/base/garden?at=claimlayer/cl-p1&z=2","at":"claimlayer/cl-p1","z":2,"who":"all","framed":[{"kind":"plan","id":"cl-p1","title":"Plot one"},{"kind":"bead","id":"cl-t1","title":"Dig"},{"kind":"bead","id":"cl-t2","title":"Sow"}],"vp":[1280,800],"taken_at":"2026-10-19T09:05:00Z"}`,
		}})
	h := commonsServer(t, d)
	code, body := get(t, h, "/test/m/cl-snap1", "claimlayer")
	if code != 200 {
		t.Fatalf("GET = %d", code)
	}
	file := filepath.Join("testdata", "snap.golden.html")
	if *update {
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if body != string(want) {
		t.Errorf("drifted from %s (run with -update if meant):\n%s", file, body)
	}
}

// A snapshot whose picture does not show inline (a Slack link here) keeps its
// caption as a line of its own under the files.
func TestSnapCaptionWithoutInlinePicture(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Put(&beadsapi.BeadDetail{ID: "cl-snap2", Type: "message", Title: "Snapshot of the farm at the Yard", Status: "closed",
		Labels: []string{"project:claimlayer"}, CreatedAt: t0.Add(5 * time.Minute), Fields: map[string]string{
			"channel_id": "cl-chgen", "author": "oidc:bo@claimlayer.example", "kind": "text", "origin": "commons",
			"files": `[{"name":"farm.png","mimetype":"image/png","size":48213,"url":"https://files.slack.com/files-pri/T1-F1/farm.png"}]`,
			"snap":  `{"kind":"farm","at":"yard"}`,
		}})
	code, body := get(t, commonsServer(t, d), "/test/m/cl-snap2", "claimlayer")
	if code != 200 {
		t.Fatalf("GET = %d", code)
	}
	mustContain(t, body, "</ul>\n<p class=\"cm-snap\">Farm at the Yard</p>")
	mustNotContain(t, body, "<figcaption")
}

// TestWritesRefuseCrossOrigin: say, reply, join and leave write as the signed-in
// person, so a request another site's page sent is refused before anything is
// written (gd-OPACzpdlZU). Requests with neither header (gb, tests) still pass.
func TestWritesRefuseCrossOrigin(t *testing.T) {
	d := commonstest.TwoLands(t0)
	h := commonsServer(t, d)
	routes := []string{
		"/garden/c/claimlayer/general/say",
		"/garden/c/t/cl-msg1/reply",
		"/garden/c/claimlayer/build/join",
		"/garden/c/claimlayer/build/leave",
	}
	created, updated := len(d.Created), len(d.Updated)
	for name, hdr := range map[string][]string{
		"cross-site":     {"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example"},
		"same-site":      {"Sec-Fetch-Site", "same-site"},
		"foreign origin": {"Origin", "https://evil.example"},
		"opaque origin":  {"Origin", "null"},
	} {
		for _, path := range routes {
			if code, body := post(t, h, path, "claimlayer", "hi", hdr...); code != http.StatusForbidden {
				t.Errorf("%s %s = %d %q, want 403", name, path, code, body)
			}
		}
	}
	if len(d.Created) != created || len(d.Updated) != updated {
		t.Fatalf("a cross-origin write went through: %d created, %d updated", len(d.Created)-created, len(d.Updated)-updated)
	}
	// Control: the page's own fetch still posts.
	if code, body := post(t, h, "/garden/c/claimlayer/general/say", "claimlayer", "hi", "Sec-Fetch-Site", "same-origin"); code != http.StatusCreated {
		t.Errorf("same-origin say = %d %q, want 201", code, body)
	}
}

// A Glass nudge (Lands GL-7) is written into the agent's thread as the
// person's message, marked so the bridge does not deliver it a second time:
// not origin=commons, and without the wish head's Slack binding.
func TestSayInThreadWritesTheGlassNudge(t *testing.T) {
	d := commonstest.TwoLands(t0)
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent1", Type: "agent", Title: "claimlayer-crew-1", Status: "open", Labels: []string{"project:claimlayer"}, CreatedAt: t0,
		Fields: map[string]string{"thread_id": "cl-wish1"}})
	var h *Handler
	commonsServer(t, d, func(x *Handler) { h = x })
	req := func(hdr ...string) *http.Request {
		r := httptest.NewRequest("POST", "/garden/glass/cl-agent1/send", nil)
		r.Header.Set("X-Test-As", "claimlayer")
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return r.WithContext(beadsapi.WithUserBearer(r.Context(), commonstest.ClaimlayerBearer))
	}
	id, err := h.SayInThread(req(), "cl-agent1", "  look at the failing test ", "glass")
	if err != nil || id == "" {
		t.Fatalf("SayInThread = %q, %v", id, err)
	}
	got, f := lastCreated(t, d)
	if got.Type != "message" || got.Status != "closed" || !slices.Equal(got.Labels, []string{"project:claimlayer"}) || got.CreatedBy != "oidc:bo@claimlayer.example" {
		t.Errorf("bead = %+v", got)
	}
	want := map[string]string{"channel_id": "cl-chgen", "thread_id": "cl-wish1", "author": "oidc:bo@claimlayer.example", "kind": "text",
		"text": "look at the failing test", "origin": "glass"}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("fields[%s] = %q, want %q (all: %v)", k, f[k], v, f)
		}
	}
	if f["slack_thread_ts"] != "" || f["slack_thread_channel"] != "" {
		t.Errorf("a Glass nudge carries the Slack binding, so the thread forward would nudge again: %v", f)
	}
	n := len(d.Created)
	if _, err := h.SayInThread(req(), "cl-agent1", "x", "commons"); err == nil {
		t.Error("origin=commons would have the bridge deliver it again")
	}
	if _, err := h.SayInThread(req("X-Test-Anon", "1"), "cl-agent1", "x", "glass"); !errors.Is(err, ErrMayNotReply) {
		t.Errorf("anonymous: %v", err)
	}
	if _, err := h.SayInThread(req(), "cl-agent4-nope", "x", "glass"); !IsNotFound(err) {
		t.Errorf("no such bead: %v", err)
	}
	if len(d.Created) != n {
		t.Errorf("a refused write wrote %d beads", len(d.Created)-n)
	}
}
