package commons

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// typingPresence is presence over a started hub with typing times short
// enough to watch run out. TypingFor is several gaps long: the tests wait out
// a gap (and a quiet window) before a signal they expect to find the line
// still up, and with TypingFor only 150ms a loaded runner ran the line out
// first, so the extension published as a fresh start (gd-d11UAZW1Qm).
func typingPresence(t *testing.T, d *commonstest.Daemon) (*Hub, *Presence) {
	t.Helper()
	hub := presenceHub(t, d)
	p := NewPresence(hub, nil, nil)
	p.Linger, p.TypingFor, p.TypingGap = 0, 400*time.Millisecond, 60*time.Millisecond
	return hub, p
}

func TestTypingStartsOnceThenRunsOut(t *testing.T) {
	hub, p := typingPresence(t, commonstest.TwoLands(t0))
	cl, stopCL := hub.Subscribe(context.Background(), claimlayerScope)
	defer stopCL()
	gd, stopGD := hub.Subscribe(context.Background(), Scope{Lands: []string{"gardenio"}})
	defer stopGD()
	ann, general := "oidc:ann@example.com", Spot{Land: "claimlayer", Channel: "cl-chgen"}

	first := time.Now()
	if ok, _ := p.Type(ann, general, ""); !ok {
		t.Fatal("the first signal was refused")
	}
	inv := next(t, cl)
	if inv.Kind != "typing" || inv.ID != "cl-chgen" || inv.Channel != "cl-chgen" || inv.Thread != "" || inv.Land != "claimlayer" {
		t.Errorf("typing published %+v", inv)
	}
	quiet(t, gd) // another land's subscriber never hears of it
	if got := p.Typing(general, ""); len(got) != 1 || got[0] != ann {
		t.Errorf("Typing(general) = %v", got)
	}

	// Faster than the gap is refused with how long to wait; after it, the
	// signal only extends the line, so nobody hears of it. A runner that
	// stalled past the gap since the first signal proves nothing about it
	// (gd-bzIwv4oNJg); a signal let through then only extends the line too.
	ok, retry := p.Type(ann, general, "")
	if inside := time.Since(first); inside < p.TypingGap && (ok || retry <= 0 || retry > p.TypingGap) {
		t.Errorf("a signal inside the gap = %v, %v", ok, retry)
	} else if inside >= p.TypingGap {
		t.Logf("the runner stalled %v past the first signal, past the gap; not checking the refusal", inside)
	}
	time.Sleep(p.TypingGap + 10*time.Millisecond)
	// start is taken before the signal, so the wait below covers the whole
	// line however long quiet and the scheduler take.
	start := time.Now()
	if ok, _ := p.Type(ann, general, ""); !ok {
		t.Error("a signal after the gap was refused")
	}
	if late := start.Sub(first); late >= p.TypingFor {
		t.Fatalf("the runner stalled: the extending signal came %v after the first, past TypingFor, so the line had run out", late)
	}
	quiet(t, cl)

	// The line runs out TypingFor after the last signal, and that is news.
	inv = next(t, cl)
	if inv.Kind != "typing" || inv.Channel != "cl-chgen" {
		t.Errorf("running out published %+v", inv)
	}
	if waited := time.Since(start); waited < p.TypingFor {
		t.Errorf("the line ran out after %v, before TypingFor since the last signal", waited)
	}
	if got := p.Typing(general, ""); len(got) != 0 {
		t.Errorf("Typing after running out = %v", got)
	}
	quiet(t, cl)
}

// The rate limit reads p.now, so this test drives it with a clock of its own:
// on the wall clock a runner that stalled ~30ms between the stop and the next
// signal let the gap elapse, and the signal through (gd-bzIwv4oNJg). The
// timers stay real; expire checks p.now before it forgets a typist.
func TestTypingStopAndThreads(t *testing.T) {
	hub, p := typingPresence(t, commonstest.TwoLands(t0))
	var clockMu sync.Mutex
	clock := t0
	p.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	advance := func(d time.Duration) { clockMu.Lock(); clock = clock.Add(d); clockMu.Unlock() }
	cl, stop := hub.Subscribe(context.Background(), claimlayerScope)
	defer stop()
	ann, general := "oidc:ann@example.com", Spot{Land: "claimlayer", Channel: "cl-chgen"}

	// A thread is its own line, apart from the channel's.
	typed := time.Now()
	p.Type(ann, general, "cl-msg1")
	if inv := next(t, cl); inv.Kind != "typing" || inv.Thread != "cl-msg1" {
		t.Errorf("thread typing published %+v", inv)
	}
	if got := p.Typing(general, ""); len(got) != 0 {
		t.Errorf("typing in a thread shows in the channel: %v", got)
	}
	if got := p.Typing(general, "cl-msg1"); len(got) != 1 {
		t.Errorf("Typing(thread) = %v", got)
	}

	// Stopping is news once; stopping again, or someone who never typed, is not.
	p.StopTyping(ann, general, "cl-msg1")
	if inv := next(t, cl); inv.Kind != "typing" || inv.Thread != "cl-msg1" {
		t.Errorf("stop published %+v", inv)
	}
	p.StopTyping(ann, general, "cl-msg1")
	p.StopTyping("oidc:bo@example.com", general, "")
	quiet(t, cl)
	if got := p.Typing(general, "cl-msg1"); len(got) != 0 {
		t.Errorf("Typing after stop = %v", got)
	}

	// Stopping does not reset the rate limit, and the stopped line never
	// "runs out" a second time. A loaded runner can spend the whole gap getting
	// here (CI on PR #1906), and then the start is rightly let through.
	if ok, _ := p.Type(ann, general, "cl-msg1"); ok && time.Since(typed) < p.TypingGap {
		t.Error("stop then start inside the gap was let through")
	}
	// The stop's timer, real, fires a gap after it and forgets the typist only
	// once the clock is past the gap too.
	advance(p.TypingFor + 50*time.Millisecond)
	time.Sleep(p.TypingGap + 50*time.Millisecond)
	quiet(t, cl)
	if ok, _ := p.Type(ann, general, "cl-msg1"); !ok {
		t.Error("a signal long after the stop was refused")
	}
	next(t, cl)
}

// A principal has two composers on a spot, the channel's and one thread's,
// as the pane does. The thread composer's rate limit is the same whatever
// thread a signal names, so a new thread id per signal is no faster than
// repeating one; a signal in another thread moves the thread line there.
func TestTypingRateLimitsAcrossThreads(t *testing.T) {
	hub, p := typingPresence(t, commonstest.TwoLands(t0))
	cl, stop := hub.Subscribe(context.Background(), claimlayerScope)
	defer stop()
	ann, general := "oidc:ann@example.com", Spot{Land: "claimlayer", Channel: "cl-chgen"}

	// The channel's composer and a thread's are apart: both get through.
	p.Type(ann, general, "")
	if inv := next(t, cl); inv.Kind != "typing" || inv.Thread != "" {
		t.Errorf("channel typing published %+v", inv)
	}
	if ok, _ := p.Type(ann, general, "cl-msg1"); !ok {
		t.Fatal("typing in a thread just after the channel was refused")
	}
	if inv := next(t, cl); inv.Kind != "typing" || inv.Thread != "cl-msg1" {
		t.Errorf("thread typing published %+v", inv)
	}

	// Rotating thread ids is refused, publishes nothing and keeps nothing.
	for i := range 50 {
		thread := fmt.Sprintf("x%d", i)
		if ok, retry := p.Type(ann, general, thread); ok || retry <= 0 {
			t.Fatalf("Type in thread %q inside the gap = %v, %v; want refused", thread, ok, retry)
		}
	}
	quiet(t, cl)
	if got := p.Typing(general, "x1"); len(got) != 0 {
		t.Errorf("a refused thread shows typing: %v", got)
	}
	p.mu.Lock()
	kept := len(p.typing)
	p.mu.Unlock()
	if kept != 2 {
		t.Errorf("rotating threads kept %d typists, want ann's two composers", kept)
	}

	// After the gap, another thread moves the line: a stop where it was,
	// then a start where it is.
	time.Sleep(p.TypingGap + 10*time.Millisecond)
	if ok, _ := p.Type(ann, general, "cl-msg2"); !ok {
		t.Fatal("a signal after the gap was refused")
	}
	if inv := next(t, cl); inv.Thread != "cl-msg1" {
		t.Errorf("moving published %+v first, want cl-msg1's stop", inv)
	}
	if inv := next(t, cl); inv.Thread != "cl-msg2" {
		t.Errorf("moving published %+v second, want cl-msg2's start", inv)
	}
	if got := p.Typing(general, "cl-msg1"); len(got) != 0 {
		t.Errorf("the old thread still shows the moved typist: %v", got)
	}
	if got := p.Typing(general, "cl-msg2"); len(got) != 1 {
		t.Errorf("Typing(new thread) = %v", got)
	}
	if got := p.Typing(general, ""); len(got) != 1 {
		t.Errorf("moving the thread line ended the channel's: %v", got)
	}
	// A stop for a thread ann no longer types in changes nothing.
	p.StopTyping(ann, general, "cl-msg1")
	quiet(t, cl)
	if got := p.Typing(general, "cl-msg2"); len(got) != 1 {
		t.Errorf("a stop for the old thread ended the new one's line: %v", got)
	}
}

func TestNilPresenceTyping(t *testing.T) {
	var p *Presence
	if ok, _ := p.Type("oidc:ann@example.com", Spot{Land: "claimlayer", Channel: "cl-chgen"}, ""); !ok {
		t.Error("nil presence refused a signal")
	}
	p.StopTyping("oidc:ann@example.com", Spot{Land: "claimlayer", Channel: "cl-chgen"}, "")
	if got := p.Typing(Spot{Land: "claimlayer", Channel: "cl-chgen"}, ""); got != nil {
		t.Errorf("nil Typing = %v", got)
	}
}

// A typing line cuts each name (typingNameMax), not only the line.
func TestTypingNameCut(t *testing.T) {
	if got := clipRunes("bartholomew.bartholomew.smith", typingNameMax); got != "bartholomew.barthol…" || len([]rune(got)) != 20 {
		t.Errorf("clipRunes(long) = %q", got)
	}
	if got := clipRunes("田中花子", typingNameMax); got != "田中花子" {
		t.Errorf("clipRunes(short) = %q", got)
	}
}

func TestTypingWords(t *testing.T) {
	for _, c := range []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"ann"}, "ann is typing…"},
		{[]string{"ann", "bo"}, "ann and bo are typing…"},
		{[]string{"ann", "bo", "cy"}, "ann, bo and cy are typing…"},
		{[]string{"ann", "bo", "cy", "di"}, "Several people are typing…"},
	} {
		if got := typingWords(c.names); got != c.want {
			t.Errorf("typingWords(%v) = %q, want %q", c.names, got, c.want)
		}
	}
}

// typingServer is commonsServer with a hub, Presence and a Viewer wired; the
// viewer is the Gate's self (testSelf), as beads-viewer wires both from the
// signed-in email.
func typingServer(t *testing.T, d *commonstest.Daemon, hub *Hub, p *Presence) http.Handler {
	t.Helper()
	h := &Handler{
		Beads: d, Writes: d, Hub: hub, Presence: p, BasePath: "/base", Gate: testGate, EmptyPrompt: testPrompt,
		Scope: func(r *http.Request) Scope {
			if r.Header.Get("X-Test-As") == "claimlayer" {
				return claimlayerScope
			}
			return adminScope
		},
		Viewer: testGate.Self,
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := commonstest.AdminBearer
		if r.Header.Get("X-Test-As") == "claimlayer" {
			bearer = commonstest.ClaimlayerBearer
		}
		mux.ServeHTTP(w, r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer)))
	})
}

// signal posts a typing form as the claimlayer viewer, with X-Test-* header
// pairs in hdr.
func signal(t *testing.T, h http.Handler, path string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Test-As", "claimlayer")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestTypingRoutes(t *testing.T) {
	d := commonstest.TwoLands(t0)
	hub, p := typingPresence(t, d)
	p.TypingFor, p.TypingGap = time.Minute, time.Minute // nothing runs out mid-test
	h := typingServer(t, d, hub, p)
	cl, stop := hub.Subscribe(context.Background(), claimlayerScope)
	defer stop()
	bo, general := "oidc:bo@claimlayer.example", Spot{Land: "claimlayer", Channel: "cl-chgen"}
	path := "/garden/c/claimlayer/general/typing"

	// Without a stream on the channel there is no typing there.
	if rec := signal(t, h, path, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Open #general") {
		t.Errorf("typing with no stream = %d %q", rec.Code, rec.Body.String())
	}
	quiet(t, cl)

	leave := p.Join(bo, general)
	defer leave()
	next(t, cl) // the join's presence

	// With one, the first signal is news and the next one inside the gap is
	// refused with Retry-After. Typing never writes a bead.
	before := len(d.Created)
	if rec := signal(t, h, path, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("typing = %d %q", rec.Code, rec.Body.String())
	}
	inv := next(t, cl)
	data, _ := json.Marshal(inv)
	if inv.Kind != "typing" || string(data) != `{"id":"cl-chgen","channel":"cl-chgen","land":"claimlayer"}` {
		t.Errorf("typing published %s %s", inv.Kind, data)
	}
	rec := signal(t, h, path, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
		t.Errorf("a second signal = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	quiet(t, cl)
	if len(d.Created) != before {
		t.Errorf("typing created beads: %+v", d.Created[before:])
	}

	// Another viewer reads the line; the typist does not see itself.
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("X-Test-Self", "oidc:ann@claimlayer.example")
	r.Header.Set("X-Test-As", "claimlayer")
	got := httptest.NewRecorder()
	h.ServeHTTP(got, r)
	if got.Code != http.StatusOK {
		t.Fatalf("GET typing = %d", got.Code)
	}
	mustContain(t, got.Body.String(), `data-commons="typing"`, `data-channel="cl-chgen"`, `>bo is typing…</p>`)
	golden(t, "typing", got.Body.String())
	if _, own := get(t, h, path, "claimlayer"); !strings.Contains(own, `data-channel="cl-chgen"></p>`) {
		t.Errorf("the typist sees its own line:\n%s", own)
	}

	// Saying the message ends the line, and so does stop=1 in a thread.
	if code, body := post(t, h, "/garden/c/claimlayer/general/say", "claimlayer", "hi"); code != http.StatusCreated {
		t.Fatalf("say = %d %s", code, body)
	}
	for {
		if inv := next(t, cl); inv.Kind == "typing" {
			if inv.Thread != "" || inv.Channel != "cl-chgen" {
				t.Errorf("say's stop published %+v", inv)
			}
			break
		}
	}
	if got := p.Typing(general, ""); len(got) != 0 {
		t.Errorf("still typing after say: %v", got)
	}

	// bo's thread composer has one limit whatever thread a signal names: the
	// first gets through, and a fresh thread id per request after it is
	// refused like any other signal inside the gap, with no event, no line and
	// nothing kept.
	if rec := signal(t, h, path, url.Values{"thread": {"x0"}}); rec.Code != http.StatusNoContent {
		t.Fatalf("thread typing = %d %q", rec.Code, rec.Body.String())
	}
	if inv := next(t, cl); inv.Thread != "x0" {
		t.Errorf("thread typing published %+v", inv)
	}
	for i := 1; i < 20; i++ {
		rec := signal(t, h, path, url.Values{"thread": {fmt.Sprintf("x%d", i)}})
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("rotated thread x%d = %d, Retry-After %q", i, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	if rec := signal(t, h, path, url.Values{"thread": {"x1"}, "stop": {"1"}}); rec.Code != http.StatusNoContent {
		t.Errorf("stop in a thread never typed in = %d %q", rec.Code, rec.Body.String())
	}
	quiet(t, cl)
	if got := p.Typing(general, "x1"); len(got) != 0 {
		t.Errorf("a refused thread shows typing: %v", got)
	}
	p.mu.Lock()
	kept := len(p.typing)
	p.mu.Unlock()
	if kept != 2 {
		t.Errorf("rotating threads kept %d typists, want bo's two composers", kept)
	}

	// Typing in a thread is its own line, and stop=1 there ends it.
	ann := "oidc:ann@claimlayer.example"
	leaveAnn := p.Join(ann, general)
	defer leaveAnn()
	next(t, cl)
	if rec := signal(t, h, path, url.Values{"thread": {"cl-msg1"}}, "X-Test-Self", ann); rec.Code != http.StatusNoContent {
		t.Fatalf("thread typing = %d %q", rec.Code, rec.Body.String())
	}
	if inv := next(t, cl); inv.Thread != "cl-msg1" {
		t.Errorf("thread typing published %+v", inv)
	}
	_, line := get(t, h, path+"?thread=cl-msg1", "")
	mustContain(t, line, `data-thread="cl-msg1">ann is typing…</p>`)
	if rec := signal(t, h, path, url.Values{"thread": {"cl-msg1"}, "stop": {"1"}}, "X-Test-Self", ann); rec.Code != http.StatusNoContent {
		t.Errorf("stop = %d %q", rec.Code, rec.Body.String())
	}
	if inv := next(t, cl); inv.Thread != "cl-msg1" {
		t.Errorf("stop published %+v", inv)
	}

	// Refusals, none of them news.
	for _, c := range []struct {
		name, path string
		form       url.Values
		hdr        []string
		want       int
	}{
		{"anonymous", path, nil, []string{"X-Test-Anon", "1"}, http.StatusUnauthorized},
		{"another land", "/garden/c/gardenio/general/typing", nil, nil, http.StatusNotFound},
		{"no such channel", "/garden/c/claimlayer/nope/typing", nil, nil, http.StatusNotFound},
		{"not a member", "/garden/c/claimlayer/build/typing", nil, nil, http.StatusForbidden},
		{"bad thread", path, url.Values{"thread": {"a/b"}}, nil, http.StatusBadRequest},
		{"another channel", "/garden/c/claimlayer/crew/typing", nil, nil, http.StatusConflict},
	} {
		if rec := signal(t, h, c.path, c.form, c.hdr...); rec.Code != c.want {
			t.Errorf("%s = %d %q, want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
	quiet(t, cl)
	if code, _ := get(t, h, "/garden/c/gardenio/general/typing", "claimlayer"); code != http.StatusNotFound {
		t.Errorf("scoped GET another land's typing = %d, want 404", code)
	}
}
