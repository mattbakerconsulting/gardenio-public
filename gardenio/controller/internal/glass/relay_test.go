package glass

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/coopmuxapi"
)

// fakeMux is a coopmux that hands out one fakeUpstream per dial.
type fakeMux struct {
	mu    sync.Mutex
	ups   []*fakeUpstream
	dials atomic.Int32
	err   error // returned by every dial when set
	ids   []coopmuxapi.Identity
}

func (m *fakeMux) dial(ctx context.Context, session string, topics []string) (Upstream, error) {
	m.dials.Add(1)
	id, _ := coopmuxapi.IdentityFromContext(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ids = append(m.ids, id)
	if m.err != nil {
		return nil, m.err
	}
	u := &fakeUpstream{ctx: ctx, in: make(chan coopmuxapi.Frame, 4096), sent: make(chan map[string]any, 64), closed: make(chan struct{})}
	m.ups = append(m.ups, u)
	return u, nil
}

// upstream waits for the n-th dial (0-based).
func (m *fakeMux) upstream(t *testing.T, n int) *fakeUpstream {
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

type fakeUpstream struct {
	ctx       context.Context
	in        chan coopmuxapi.Frame
	sent      chan map[string]any
	closeOnce sync.Once
	closed    chan struct{}
}

func (u *fakeUpstream) Recv() (coopmuxapi.Frame, error) {
	select {
	case f, ok := <-u.in:
		if !ok {
			return coopmuxapi.Frame{}, io.EOF
		}
		return f, nil
	case <-u.ctx.Done():
		return coopmuxapi.Frame{}, u.ctx.Err()
	case <-u.closed:
		return coopmuxapi.Frame{}, io.EOF
	}
}

func (u *fakeUpstream) Send(frame any) error {
	b, _ := json.Marshal(frame)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	u.sent <- m
	return nil
}

func (u *fakeUpstream) Close() error {
	u.closeOnce.Do(func() { close(u.closed) })
	return nil
}

func (u *fakeUpstream) isClosed() bool {
	select {
	case <-u.closed:
		return true
	default:
		return u.ctx.Err() != nil
	}
}

// emit sends one coop frame.
func (u *fakeUpstream) emit(event string, fields map[string]any) {
	m := map[string]any{"event": event}
	for k, v := range fields {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	u.in <- coopmuxapi.Frame{Event: event, Data: b}
}

func (u *fakeUpstream) replay(data string, next int64) {
	u.emit("replay", map[string]any{"data": b64(data), "offset": next - int64(len(data)), "next_offset": next, "total_written": next})
}

func (u *fakeUpstream) pty(data string, offset int64) {
	u.emit("pty", map[string]any{"data": b64(data), "offset": offset})
}

func (u *fakeUpstream) screen(lines ...string) {
	u.emit("screen", map[string]any{"lines": lines, "cols": 80, "rows": 24, "alt_screen": false, "seq": 7})
}

// waitSent waits for a request the relay sent upstream with the given event.
func (u *fakeUpstream) waitSent(t *testing.T, event string) map[string]any {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case m := <-u.sent:
			if m["event"] == event {
				return m
			}
		case <-timeout:
			t.Fatalf("relay never sent %q upstream", event)
			return nil
		}
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

var alice = coopmuxapi.Identity{Operator: "alice@example.com", AuthMode: "cookie", Roles: []string{"gardenio-operator"}}

// collect drains w until pred holds over everything taken so far.
func collect(t *testing.T, w *Watcher, pred func([]Event) bool) []Event {
	t.Helper()
	var all []Event
	timeout := time.After(2 * time.Second)
	for !pred(all) {
		select {
		case <-w.Ready():
			evs, _ := w.Take()
			all = append(all, evs...)
		case <-timeout:
			t.Fatalf("timed out; got %s", kinds(all))
		}
	}
	return all
}

func kinds(evs []Event) string {
	s := ""
	for _, e := range evs {
		s += fmt.Sprintf("%s@%d ", e.Kind, e.ID)
	}
	return s
}

// bytesOf concatenates what a watcher would write to its terminal: a replay or
// screen resets it, pty appends.
func bytesOf(t *testing.T, evs []Event) string {
	t.Helper()
	out := ""
	for _, e := range evs {
		switch e.Kind {
		case EventReplay, EventPty:
			var m struct {
				Data string `json:"data"`
			}
			if err := json.Unmarshal(e.Data, &m); err != nil {
				t.Fatal(err)
			}
			b, _ := base64.StdEncoding.DecodeString(m.Data)
			if e.Kind == EventReplay {
				out = ""
			}
			out += string(b)
		case EventScreen:
			out = "<screen>"
		}
	}
	return out
}

func hasKind(evs []Event, kind string) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func TestRelayAsksForStateUsageAndReplayOnConnect(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	w := r.Watch("s1", alice, -1)
	defer w.Close()
	u := mux.upstream(t, 0)
	u.waitSent(t, "agent:get")
	u.waitSent(t, "usage:get")
	u.waitSent(t, "screen:get")
	if got := u.waitSent(t, "replay:get"); got["offset"] != float64(0) {
		t.Fatalf("replay:get offset = %v, want 0", got["offset"])
	}
	if id, _ := coopmuxapi.IdentityFromContext(u.ctx); id.Operator != "alice@example.com" {
		t.Fatalf("upstream signed for %q, want the human watching", id.Operator)
	}
}

// Two watchers of one session share one upstream; each sees every byte once.
func TestRelayTwoWatchersShareOneUpstream(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	a := r.Watch("s1", alice, -1)
	defer a.Close()
	u := mux.upstream(t, 0)
	u.replay("hello ", 6)
	collect(t, a, func(e []Event) bool { return bytesOf(t, e) == "hello " })

	bob := coopmuxapi.Identity{Operator: "bob@example.com", AuthMode: "cookie", Roles: []string{"gardenio-admin"}}
	b := r.Watch("s1", bob, -1)
	defer b.Close()
	u.pty("world", 6)
	u.pty("world", 6) // a duplicate is relayed once
	u.pty("!", 11)
	gotA := collect(t, a, func(e []Event) bool { return bytesOf(t, e) == "world!" })
	gotB := collect(t, b, func(e []Event) bool { return bytesOf(t, e) == "hello world!" })
	if n := mux.dials.Load(); n != 1 {
		t.Fatalf("dials = %d, want one upstream shared by both watchers", n)
	}
	if r.Upstreams() != 1 {
		t.Fatalf("upstreams = %d, want 1", r.Upstreams())
	}
	if gotB[0].Kind != EventReplay {
		t.Fatalf("second watcher opened with %s, want the ring as a replay", kinds(gotB))
	}
	if last := gotA[len(gotA)-1]; last.ID != 12 {
		t.Fatalf("last pty id = %d, want offset 12", last.ID)
	}
}

func TestRelayDropsPtyBeforeReplay(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	w := r.Watch("s1", alice, -1)
	defer w.Close()
	u := mux.upstream(t, 0)
	u.pty("early", 0)
	u.replay("early late", 10)
	u.pty("late+", 6)
	got := collect(t, w, func(e []Event) bool { return bytesOf(t, e) == "early late+" })
	if got[0].Kind != EventReplay {
		t.Fatalf("events = %s, want the replay first", kinds(got))
	}
}

// A browser that reconnects with Last-Event-ID inside the ring gets only the
// bytes it missed; one whose id the ring no longer holds gets a screen resync.
func TestRelayResumeFromLastEventID(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	a := r.Watch("s1", alice, -1)
	defer a.Close()
	u := mux.upstream(t, 0)
	u.replay("abc", 3)
	u.screen("abcdef")
	u.pty("def", 3)
	collect(t, a, func(e []Event) bool { return bytesOf(t, e) == "abcdef" })

	resumed := r.Watch("s1", alice, 3)
	defer resumed.Close()
	got := collect(t, resumed, func(e []Event) bool { return len(e) > 0 })
	if bytesOf(t, got) != "def" || got[0].Kind != EventPty {
		t.Fatalf("resume from 3 = %s %q, want pty \"def\"", kinds(got), bytesOf(t, got))
	}

	caughtUp := r.Watch("s1", alice, 6)
	defer caughtUp.Close()
	u.pty("g", 6)
	got = collect(t, caughtUp, func(e []Event) bool { return bytesOf(t, e) == "g" })
	if got[0].Kind != EventPty {
		t.Fatalf("caught-up resume opened with %s, want only new bytes", kinds(got))
	}

	gap := r.Watch("s1", alice, 999)
	defer gap.Close()
	got = collect(t, gap, func(e []Event) bool { return hasKind(e, EventScreen) })
	for _, e := range got {
		if e.Kind == EventPty || e.Kind == EventReplay {
			t.Fatalf("gap resume sent bytes (%s), want a screen resync", kinds(got))
		}
		if e.Kind == EventScreen && e.ID != 7 {
			t.Fatalf("screen resync id = %d, want the ring's end 7", e.ID)
		}
	}
}

// A slow client is resynced with a screen snapshot instead of stalling the
// upstream; a fast one on the same session gets every byte.
func TestRelaySlowClientGetsResync(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	fast := r.Watch("s1", alice, -1)
	defer fast.Close()
	slow := r.Watch("s1", alice, -1)
	defer slow.Close()
	u := mux.upstream(t, 0)
	u.replay("", 0)
	u.screen("the screen now")

	// Send in batches well under the queue bound and drain fast after each, so
	// only slow can fall behind. All at once, the relay could queue a full
	// queue on fast before the test took any of it and resync fast too.
	const n, batch = queueMaxEvents + 50, 64
	var fastGot []Event
	for i := 0; i < n; {
		for end := min(i+batch, n); i < end; i++ {
			u.pty("x", int64(i))
		}
		want := strings.Repeat("x", i)
		seen := fastGot[:len(fastGot):len(fastGot)]
		fastGot = append(fastGot, collect(t, fast, func(e []Event) bool { return bytesOf(t, append(seen, e...)) == want })...)
	}

	got := collect(t, slow, func(e []Event) bool { return len(e) > 0 })
	if slow.Resyncs() == 0 {
		t.Fatal("slow watcher was never resynced")
	}
	if len(got) > queueMaxEvents {
		t.Fatalf("slow watcher queued %d events, want the queue bounded", len(got))
	}
	if !hasKind(got, EventScreen) {
		t.Fatalf("slow watcher got %s, want a screen resync", kinds(got[:min(len(got), 5)]))
	}
	// what follows the resync continues from its id
	var at int64 = -1
	for _, e := range got {
		if e.Kind == EventScreen {
			at = e.ID
			continue
		}
		if at >= 0 && e.Kind == EventPty {
			var m struct {
				Offset int64 `json:"offset"`
			}
			_ = json.Unmarshal(e.Data, &m)
			if m.Offset != at {
				t.Fatalf("pty after resync at %d, want %d", m.Offset, at)
			}
			at = e.ID
		}
	}
}

// The upstream stays open while it lingers, is reused by a watcher who comes
// back, and closes once nobody has watched for the linger.
func TestRelayLingerThenClose(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	r.Linger = 50 * time.Millisecond
	w := r.Watch("s1", alice, -1)
	u := mux.upstream(t, 0)
	w.Close()
	w = r.Watch("s1", alice, -1)
	time.Sleep(80 * time.Millisecond)
	if u.isClosed() || mux.dials.Load() != 1 {
		t.Fatalf("a watcher back within the linger redialled (dials %d)", mux.dials.Load())
	}
	w.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !u.isClosed() || r.Upstreams() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("upstream still open after the linger")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRelayRedialsAndFillsFromRing(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	w := r.Watch("s1", alice, -1)
	defer w.Close()
	u := mux.upstream(t, 0)
	u.replay("abc", 3)
	collect(t, w, func(e []Event) bool { return bytesOf(t, e) == "abc" })
	close(u.in) // coopmux dropped the stream

	u2 := mux.upstream(t, 1)
	if got := u2.waitSent(t, "replay:get"); got["offset"] != float64(3) {
		t.Fatalf("redial replay:get offset = %v, want the ring's end 3", got["offset"])
	}
	u2.replay("abcdef", 6) // overlaps what the ring has
	// only the bytes the ring lacked go out, as pty: the terminal is not reset
	got := collect(t, w, func(e []Event) bool { return bytesOf(t, e) == "def" })
	if hasKind(got, EventReplay) || got[0].ID != 6 {
		t.Fatalf("an overlapping replay sent %s, want pty \"def\" only", kinds(got))
	}

	close(u2.in)
	u3 := mux.upstream(t, 2)
	u3.replay("XYZ", 100) // bytes were lost while away: the terminal resets
	got = collect(t, w, func(e []Event) bool { return hasKind(e, EventReplay) })
	if bytesOf(t, got) != "XYZ" {
		t.Fatalf("after a gap the watcher shows %q, want the new replay", bytesOf(t, got))
	}
}

func TestRelayAsksForMissingBytesOnce(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	w := r.Watch("s1", alice, -1)
	defer w.Close()
	u := mux.upstream(t, 0)
	u.waitSent(t, "replay:get")
	u.replay("ab", 2)
	u.pty("ef", 4) // "cd" never arrived
	u.pty("gh", 6)
	if got := u.waitSent(t, "replay:get"); got["offset"] != float64(2) {
		t.Fatalf("replay:get offset = %v, want 2", got["offset"])
	}
	select {
	case m := <-u.sent:
		t.Fatalf("relay asked again (%v) while a replay was outstanding", m)
	case <-time.After(30 * time.Millisecond):
	}
	u.replay("abcdefgh", 8)
	collect(t, w, func(e []Event) bool { return bytesOf(t, e) == "abcdefgh" })
}

func TestRelaySessionGoneEndsWatchers(t *testing.T) {
	mux := &fakeMux{err: coopmuxapi.ErrSessionNotFound}
	r := NewRelay(mux.dial, nil)
	w := r.Watch("gone", alice, -1)
	defer w.Close()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case <-w.Ready():
			evs, done := w.Take()
			if !done {
				continue
			}
			if len(evs) == 0 || evs[len(evs)-1].Kind != EventEnded || string(evs[len(evs)-1].Data) != `{"reason":"gone"}` {
				t.Fatalf("finished with %s, want ended gone", kinds(evs))
			}
			if r.Upstreams() != 0 {
				t.Fatal("an ended session stayed in the relay")
			}
			return
		case <-timeout:
			t.Fatal("watcher of a gone session never finished")
		}
	}
}

func TestRelayStateMachine(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	w := r.Watch("s1", alice, -1)
	defer w.Close()
	u := mux.upstream(t, 0)
	u.emit("agent", map[string]any{"state": "working", "prompt": nil})
	u.emit("transition", map[string]any{"prev": "working", "next": "error", "seq": 3, "prompt": nil, "error_category": "model_rate_limited", "error_detail": "429"})
	u.emit("transition", map[string]any{"prev": "error", "next": "prompt", "seq": 4, "prompt": map[string]any{"type": "permission", "options": []string{"Yes", "No"}}})
	got := collect(t, w, func(e []Event) bool {
		n := 0
		for _, x := range e {
			if x.Kind == EventState {
				n++
			}
		}
		return n == 3
	})
	var states []State
	for _, e := range got {
		var s State
		_ = json.Unmarshal(e.Data, &s)
		states = append(states, s)
	}
	if states[0].State != "working" || states[0].Attention {
		t.Fatalf("agent:get answer = %+v", states[0])
	}
	if s := states[1]; s.State != "error" || s.Prev != "working" || !s.RateLimited || !s.Attention {
		t.Fatalf("rate-limited error = %+v", s)
	}
	if s := states[2]; s.State != "prompt" || !s.Attention || s.RateLimited || len(s.Prompt) == 0 {
		t.Fatalf("prompt = %+v", s)
	}

	// a late watcher opens with the last state
	late := r.Watch("s1", alice, -1)
	defer late.Close()
	first := collect(t, late, func(e []Event) bool { return len(e) > 0 })
	if first[0].Kind != EventState || string(first[0].Data) != string(got[len(got)-1].Data) {
		t.Fatalf("late watcher opened with %s", kinds(first))
	}
}

func TestRelayRingIsBounded(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	w := r.Watch("s1", alice, -1)
	defer w.Close()
	u := mux.upstream(t, 0)
	big := make([]byte, ringCap+100)
	for i := range big {
		big[i] = 'a'
	}
	u.replay(string(big), int64(len(big)))
	got := collect(t, w, func(e []Event) bool { return hasKind(e, EventReplay) })
	var m struct {
		Data       string `json:"data"`
		Offset     int64  `json:"offset"`
		NextOffset int64  `json:"next_offset"`
	}
	_ = json.Unmarshal(got[len(got)-1].Data, &m)
	b, _ := base64.StdEncoding.DecodeString(m.Data)
	if len(b) != ringCap || m.Offset != 100 || m.NextOffset != int64(len(big)) {
		t.Fatalf("replay kept %d bytes from %d to %d, want the last %d", len(b), m.Offset, m.NextOffset, ringCap)
	}
}

// A resize reaches every watcher: coop says so only in its next screen, which the relay sends on as a resync when its
// geometry differs from the last one; a screen at the same size is kept for resyncs and not sent.
func TestRelayResizeIsAResync(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	w := r.Watch("s1", alice, -1)
	defer w.Close()
	u := mux.upstream(t, 0)
	u.emit("screen", map[string]any{"lines": []string{"a"}, "cols": 80, "rows": 24, "seq": 1})
	u.replay("hello", 5)
	collect(t, w, func(e []Event) bool { return bytesOf(t, e) == "hello" })
	u.emit("screen", map[string]any{"lines": []string{"b"}, "cols": 80, "rows": 24, "seq": 2})
	u.emit("screen", map[string]any{"lines": []string{"c"}, "cols": 132, "rows": 40, "seq": 3})
	evs := collect(t, w, func(e []Event) bool { return hasKind(e, EventScreen) })
	var got []Event
	for _, e := range evs {
		if e.Kind == EventScreen {
			got = append(got, e)
		}
	}
	var m struct {
		Lines []string `json:"lines"`
		Cols  int      `json:"cols"`
	}
	if len(got) != 1 || json.Unmarshal(got[0].Data, &m) != nil || m.Cols != 132 || m.Lines[0] != "c" || got[0].ID != 5 {
		t.Fatalf("screens sent %s (%+v), want the resized one alone, at offset 5", kinds(evs), m)
	}
}

// A replay and a screen carry the app's DEC private modes as the bytes that set them (gd-bftIGCMNVc): the ring is a
// window, so a terminal painting from it would otherwise never learn the app is on the alternate screen taking the
// mouse, and its wheel would scroll a local scrollback instead of reaching the app. No mode set, no field.
func TestRelayCarriesTheAppsModes(t *testing.T) {
	mux := &fakeMux{}
	r := NewRelay(mux.dial, nil)
	w := r.Watch("s1", alice, -1)
	defer w.Close()
	u := mux.upstream(t, 0)
	u.emit("screen", map[string]any{"lines": []string{"a"}, "cols": 80, "rows": 24, "alt_screen": true, "mouse_tracking": 1003, "mouse_sgr": true, "seq": 1})
	u.replay("hello", 5)
	evs := collect(t, w, func(e []Event) bool { return hasKind(e, EventReplay) })
	var m struct {
		Data  string `json:"data"`
		Modes string `json:"modes"`
	}
	want := "\x1b[?1049h\x1b[?1003h\x1b[?1006h"
	for _, e := range evs {
		if e.Kind == EventReplay {
			if json.Unmarshal(e.Data, &m) != nil || m.Modes != want {
				t.Fatalf("replay modes %q, want %q", m.Modes, want)
			}
		}
	}
	// A resize is a screen resync: it names the modes too, and the latest ones.
	u.emit("screen", map[string]any{"lines": []string{"b"}, "cols": 132, "rows": 40, "alt_screen": true, "mouse_tracking": 1000, "seq": 2})
	evs = collect(t, w, func(e []Event) bool { return hasKind(e, EventScreen) })
	for _, e := range evs {
		if e.Kind == EventScreen {
			if json.Unmarshal(e.Data, &m) != nil || m.Modes != "\x1b[?1049h\x1b[?1000h" {
				t.Fatalf("screen modes %q, want alt screen and mode 1000 only", m.Modes)
			}
		}
	}
	if got := modesOf(screenFrame{}); got != "" {
		t.Fatalf("modes with none set: %q, want none", got)
	}
	if got := modesOf(screenFrame{MouseTracking: 1001}); got != "" {
		t.Fatalf("an unknown tracking mode is not set: %q", got)
	}
	b, _ := json.Marshal(screenFrame{Lines: []string{"a"}})
	if strings.Contains(string(b), "modes") || strings.Contains(string(b), "mouse") {
		t.Fatalf("a screen with no modes carries no mode fields: %s", b)
	}
}
