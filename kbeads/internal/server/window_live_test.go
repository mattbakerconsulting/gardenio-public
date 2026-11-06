package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// fastWindowLive shrinks a feed's timing, before any stream opens on it.
func fastWindowLive(f *windowLiveFeed) *windowLiveFeed {
	f.timing = windowLiveTiming{settle: 5 * time.Millisecond, minGap: 5 * time.Millisecond, heartbeat: time.Hour, maxLifetime: time.Hour, keepalive: time.Hour}
	return f
}

// liveFrame is one SSE frame read off a stream.
type liveFrame struct {
	event, data string
}

// openWindowLive opens GET /v1/windows/self/live as tok and returns the status
// and, on 200, a channel of its frames, closed when the stream ends.
func openWindowLive(t *testing.T, f *scopedFixture, tok string) (int, <-chan liveFrame) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.ts.URL+windowLivePath, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET live: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type %q", ct)
	}
	ch := make(chan liveFrame, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		var fr liveFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if fr.event != "" {
					ch <- fr
				}
				fr = liveFrame{}
			case strings.HasPrefix(line, "event: "):
				fr.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				fr.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return resp.StatusCode, ch
}

func nextFrame(t *testing.T, ch <-chan liveFrame) liveFrame {
	t.Helper()
	select {
	case fr, ok := <-ch:
		if !ok {
			t.Fatal("stream ended")
		}
		return fr
	case <-time.After(5 * time.Second):
		t.Fatal("no frame in 5s")
	}
	return liveFrame{}
}

// TestWindowLive_StreamsTheView: the first frame is the view route's body, a
// change to a closure bead sends the view again, and no frame carries what
// the projection withholds.
func TestWindowLive_StreamsTheView(t *testing.T) {
	f := newScopedFixture(t)
	fastWindowLive(f.srv.windowLive)
	seedWindowPlan(t, f)
	_, minted := mintWindow(t, f, tokScopedOwner, winPlan, 0)

	code, frames := openWindowLive(t, f, minted.Token)
	if code != http.StatusOK {
		t.Fatalf("live as the window: %d", code)
	}
	first := nextFrame(t, frames)
	_, viewBody := windowDo(t, f, http.MethodGet, windowViewPath, minted.Token, nil)
	var got, want any
	if first.event != "view" || json.Unmarshal([]byte(first.data), &got) != nil || json.Unmarshal([]byte(viewBody), &want) != nil {
		t.Fatalf("first frame %+v", first)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("first frame is not the view:\n got %s\nwant %s", first.data, viewBody)
	}

	f.ms.beadsMu.Lock()
	f.ms.beads[winStepOpen].Status = model.StatusClosed
	f.ms.beads[winStepOpen].Title = "Open step, now done"
	f.ms.beadsMu.Unlock()
	f.srv.recordAndPublish(context.Background(), events.TopicBeadClosed, winStepOpen, "x", events.BeadClosed{Bead: f.ms.beads[winStepOpen]})

	second := nextFrame(t, frames)
	if second.event != "view" || !strings.Contains(second.data, "Open step, now done") {
		t.Errorf("second frame %+v, want the changed view", second)
	}
	for _, fr := range []liveFrame{first, second} {
		if strings.Contains(fr.data, "SECRET") || strings.Contains(fr.data, winStepOpen) {
			t.Errorf("a frame leaks: %s", fr.data)
		}
	}
}

// TestWindowLive_OutlivesTheRequestTimeout: the live route is in
// streamingPaths, so requestTimeoutMiddleware's deadline does not end the
// stream; it still sends a change made after that deadline has passed.
func TestWindowLive_OutlivesTheRequestTimeout(t *testing.T) {
	old := requestTimeout
	requestTimeout = 100 * time.Millisecond
	t.Cleanup(func() { requestTimeout = old })
	f := newScopedFixture(t)
	fastWindowLive(f.srv.windowLive)
	seedWindowPlan(t, f)
	_, minted := mintWindow(t, f, tokScopedOwner, winPlan, 0)

	code, frames := openWindowLive(t, f, minted.Token)
	if code != http.StatusOK {
		t.Fatalf("live as the window: %d", code)
	}
	nextFrame(t, frames)

	select {
	case fr, ok := <-frames:
		if !ok {
			t.Fatal("the stream ended at the request timeout")
		}
		t.Fatalf("unexpected frame %+v", fr)
	case <-time.After(5 * requestTimeout):
	}

	f.ms.beadsMu.Lock()
	f.ms.beads[winStepOpen].Title = "Renamed past the timeout"
	f.ms.beadsMu.Unlock()
	f.srv.recordAndPublish(context.Background(), events.TopicBeadUpdated, winStepOpen, "x", events.BeadUpdated{Bead: f.ms.beads[winStepOpen]})
	if fr := nextFrame(t, frames); fr.event != "view" || !strings.Contains(fr.data, "Renamed past the timeout") {
		t.Errorf("frame after the timeout %+v, want the changed view", fr)
	}
}

// TestWindowLive_RevokeEndsTheStream: a revoke writes the window bead, which
// is in the watch set, so the stream says gone and ends without waiting for
// the heartbeat.
func TestWindowLive_RevokeEndsTheStream(t *testing.T) {
	f := newScopedFixture(t)
	fastWindowLive(f.srv.windowLive)
	seedWindowPlan(t, f)
	_, minted := mintWindow(t, f, tokScopedOwner, winPlan, 0)
	_, frames := openWindowLive(t, f, minted.Token)
	nextFrame(t, frames)

	if code, body := windowDo(t, f, http.MethodDelete, "/v1/windows/"+minted.Window.ID, tokScopedOwner, nil); code != http.StatusOK {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if fr := nextFrame(t, frames); fr.event != "gone" {
		t.Fatalf("frame after revoke %+v, want gone", fr)
	}
	select {
	case fr, ok := <-frames:
		if ok {
			t.Fatalf("frame after revoke: %+v", fr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its window's revoke")
	}
	if code, _ := openWindowLive(t, f, minted.Token); code != http.StatusUnauthorized {
		t.Errorf("live after revoke: %d, want 401", code)
	}
}

// TestWindowLive_GoneWindowIsRefused: a window whose plan was retyped or
// deleted is refused with 410 before any 200, so the browser's EventSource
// closes rather than reconnecting into another build.
func TestWindowLive_GoneWindowIsRefused(t *testing.T) {
	for name, gone := range map[string]func(f *scopedFixture){
		"retyped": func(f *scopedFixture) { f.ms.beads[winPlan].Type = model.TypeTask },
		"deleted": func(f *scopedFixture) { delete(f.ms.beads, winPlan) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newScopedFixture(t)
			fastWindowLive(f.srv.windowLive)
			seedWindowPlan(t, f)
			_, minted := mintWindow(t, f, tokScopedOwner, winPlan, 0)
			f.ms.beadsMu.Lock()
			gone(f)
			f.ms.beadsMu.Unlock()
			if code, _ := openWindowLive(t, f, minted.Token); code != http.StatusGone {
				t.Errorf("live on a gone window: %d, want 410", code)
			}
		})
	}
}

// TestWindowLive_GoneMidStreamSaysGone: a window that ends while streaming
// sends a "gone" frame and ends the stream; the reconnect is the 410.
func TestWindowLive_GoneMidStreamSaysGone(t *testing.T) {
	f := newScopedFixture(t)
	fastWindowLive(f.srv.windowLive)
	seedWindowPlan(t, f)
	_, minted := mintWindow(t, f, tokScopedOwner, winPlan, 0)
	_, frames := openWindowLive(t, f, minted.Token)
	if fr := nextFrame(t, frames); fr.event != "view" {
		t.Fatalf("first frame %+v", fr)
	}

	f.ms.beadsMu.Lock()
	f.ms.beads[winPlan].Type = model.TypeTask
	f.ms.beadsMu.Unlock()
	f.srv.recordAndPublish(context.Background(), events.TopicBeadUpdated, winPlan, "x", events.BeadUpdated{Bead: f.ms.beads[winPlan]})
	if fr := nextFrame(t, frames); fr.event != "gone" {
		t.Fatalf("frame after the plan was retyped %+v, want gone", fr)
	}
	select {
	case fr, ok := <-frames:
		if ok {
			t.Fatalf("frame after gone: %+v", fr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its gone frame")
	}
	if code, _ := openWindowLive(t, f, minted.Token); code != http.StatusGone {
		t.Errorf("reconnect after gone: %d, want 410", code)
	}
}

// TestWindowLive_LandWindowHasNoStream: only a plan window streams; a land
// window's page polls.
func TestWindowLive_LandWindowHasNoStream(t *testing.T) {
	f := newScopedFixture(t)
	seedWindowPlan(t, f)
	seedWindowLand(t, f)
	_, onLand := mintLandWindow(t, f, tokScopedOwner, "claimlayer", 0)
	if code, _ := openWindowLive(t, f, onLand.Token); code != http.StatusNotFound {
		t.Errorf("live on a land window: %d, want 404", code)
	}
}

// countingBuild is a fake windowLiveBuild: it counts builds per window and
// answers frame, watch and err as set.
type countingBuild struct {
	mu     sync.Mutex
	frame  string
	watch  []string
	err    error
	builds atomic.Int32
}

func (c *countingBuild) build(_ context.Context, _ string) ([]byte, []string, error) {
	c.builds.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	return []byte(c.frame), c.watch, c.err
}

func (c *countingBuild) set(frame string, err error) {
	c.mu.Lock()
	c.frame, c.err = frame, err
	c.mu.Unlock()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func recv(t *testing.T, s *windowLiveSub) string {
	t.Helper()
	select {
	case b := <-s.ch:
		return string(b)
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	return ""
}

// TestWindowLiveFeed_WakesOnlyForItsClosure: events on beads outside the
// closure build nothing; an event on a closure bead, or an edge onto one,
// builds once; an unchanged view is not sent again.
func TestWindowLiveFeed_WakesOnlyForItsClosure(t *testing.T) {
	cb := &countingBuild{frame: "v1", watch: []string{"plan", "step"}}
	f := fastWindowLive(newWindowLiveFeed(cb.build))
	s, err := f.subscribe("win")
	if err != nil {
		t.Fatal(err)
	}
	defer f.unsubscribe("win", s)
	if got := recv(t, s); got != "v1" {
		t.Fatalf("first frame %q", got)
	}
	waitFor(t, "the first build", func() bool { return cb.builds.Load() == 1 })

	f.publish(&sseEvent{Topic: events.TopicBeadUpdated, BeadID: "elsewhere"})
	f.publish(&sseEvent{Topic: events.TopicDependencyAdded, BeadID: "x", Data: []byte(`{"dependency":{"bead_id":"x","depends_on_id":"elsewhere"}}`)})
	time.Sleep(50 * time.Millisecond)
	if n := cb.builds.Load(); n != 1 {
		t.Fatalf("%d builds after events outside the closure, want 1", n)
	}

	f.publish(&sseEvent{Topic: events.TopicBeadUpdated, BeadID: "step"})
	waitFor(t, "a build for a closure bead", func() bool { return cb.builds.Load() == 2 })
	select {
	case b := <-s.ch:
		t.Fatalf("an unchanged view was sent again: %q", b)
	case <-time.After(30 * time.Millisecond):
	}

	cb.set("v2", nil)
	f.publish(&sseEvent{Topic: events.TopicDependencyAdded, BeadID: "new-step", Data: []byte(`{"dependency":{"bead_id":"new-step","depends_on_id":"plan"}}`)})
	if got := recv(t, s); got != "v2" {
		t.Fatalf("frame after an edge onto the plan: %q", got)
	}
	cb.set("v3", nil)
	f.publish(&sseEvent{Topic: events.TopicDependencyRemoved, BeadID: "gone-step", Data: []byte(`{"bead_id":"gone-step","depends_on_id":"plan"}`)})
	if got := recv(t, s); got != "v3" {
		t.Fatalf("frame after an edge removed from the plan: %q", got)
	}
	cb.set("v4", nil)
	f.publish(&sseEvent{Topic: events.TopicBeadUpdated, BeadID: "win"})
	if got := recv(t, s); got != "v4" {
		t.Fatalf("frame after a write to the window bead: %q", got)
	}
}

// TestWindowLiveFeed_CapPerWindow: a window takes windowLiveMaxPerWindow
// streams and refuses the next; another window is not affected; a slot frees
// when a stream ends.
func TestWindowLiveFeed_CapPerWindow(t *testing.T) {
	cb := &countingBuild{frame: "v"}
	f := fastWindowLive(newWindowLiveFeed(cb.build))
	var subs []*windowLiveSub
	for range windowLiveMaxPerWindow {
		s, err := f.subscribe("a")
		if err != nil {
			t.Fatalf("stream %d: %v", len(subs)+1, err)
		}
		subs = append(subs, s)
	}
	if _, err := f.subscribe("a"); !errors.Is(err, errWindowLiveFull) {
		t.Fatalf("stream past the cap: %v, want errWindowLiveFull", err)
	}
	other, err := f.subscribe("b")
	if err != nil {
		t.Fatalf("another window: %v", err)
	}
	f.unsubscribe("a", subs[0])
	if _, err := f.subscribe("a"); err != nil {
		t.Fatalf("after a stream ended: %v", err)
	}
	f.unsubscribe("b", other)
	f.mu.Lock()
	_, watched := f.windows["b"]
	f.mu.Unlock()
	if watched {
		t.Error("a window with no stream is still watched")
	}
}

// TestWindowLiveFeed_GoneEndsStreams: a build that finds the window gone
// ends every stream on it and stops watching; the next stream starts afresh.
func TestWindowLiveFeed_GoneEndsStreams(t *testing.T) {
	cb := &countingBuild{frame: "v", watch: []string{"plan"}}
	f := fastWindowLive(newWindowLiveFeed(cb.build))
	a, _ := f.subscribe("win")
	b, _ := f.subscribe("win")
	recv(t, a)
	recv(t, b)

	cb.set("", errWindowLiveGone)
	f.publish(&sseEvent{Topic: events.TopicBeadUpdated, BeadID: "win"})
	for _, s := range []*windowLiveSub{a, b} {
		select {
		case <-s.gone:
		case <-time.After(5 * time.Second):
			t.Fatal("a stream outlived its window")
		}
	}
	f.unsubscribe("win", a)
	f.unsubscribe("win", b)

	cb.set("fresh", nil)
	c, err := f.subscribe("win")
	if err != nil {
		t.Fatal(err)
	}
	defer f.unsubscribe("win", c)
	if got := recv(t, c); got != "fresh" {
		t.Errorf("a new stream after the window ended got %q", got)
	}
}
