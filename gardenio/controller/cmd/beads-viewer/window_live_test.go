package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fakeLiveAPI is fakeWindowAPI with a stream: each WindowLive call gets a
// pipe the test writes kbeads frames into, or err.
type fakeLiveAPI struct {
	*fakeWindowAPI
	mu      sync.Mutex
	err     error
	streams []*io.PipeWriter
	tokens  []string
}

func (f *fakeLiveAPI) WindowLive(_ context.Context, token string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)
	if f.err != nil {
		return nil, f.err
	}
	pr, pw := io.Pipe()
	f.streams = append(f.streams, pw)
	return pr, nil
}

// send writes one kbeads "view" frame to the newest stream.
func (f *fakeLiveAPI) send(t *testing.T, view []byte) {
	t.Helper()
	f.write(t, "event: view\ndata: "+string(compactJSON(t, view))+"\n\n")
}

// write writes raw kbeads stream text to the newest stream.
func (f *fakeLiveAPI) write(t *testing.T, raw string) {
	t.Helper()
	f.mu.Lock()
	pw := f.streams[len(f.streams)-1]
	f.mu.Unlock()
	if _, err := io.WriteString(pw, raw); err != nil {
		t.Fatal(err)
	}
}

func compactJSON(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// liveServer serves the viewer's /w/ routes on a real listener, so a stream
// can be read while it is open.
func liveServer(t *testing.T, api windowAPI) (*httptest.Server, *windowClock) {
	t.Helper()
	h, clock := windowServerFor(t, api, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, clock
}

// openLive GETs path as a visitor at ip (as the edge reports one) and
// returns the response; the body is the caller's to close.
func openLive(t *testing.T, ts *httptest.Server, path, ip string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Forwarded-For", ip+", 10.42.0.7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// changedFrames reads a page stream's "changed" frames onto a channel.
func changedFrames(body io.Reader) <-chan string {
	ch := make(chan string, 8)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(body)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == "changed":
				ch <- strings.TrimPrefix(line, "data: ")
			case line == "":
				event = ""
			}
		}
	}()
	return ch
}

func nextChanged(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case tag, ok := <-ch:
		if !ok {
			t.Fatal("stream ended")
		}
		return tag
	case <-time.After(5 * time.Second):
		t.Fatal("no changed frame in 5s")
	}
	return ""
}

// TestWindowLive_SaysChangedAndFeedsTheCache: each changed view from kbeads
// is one "changed" frame carrying plan.json's new ETag (an unchanged one is
// not sent again), and plan.json then serves that view with no daemon read.
func TestWindowLive_SaysChangedAndFeedsTheCache(t *testing.T) {
	api := &fakeLiveAPI{fakeWindowAPI: &fakeWindowAPI{body: windowFixture(t, "phased")}}
	ts, _ := liveServer(t, api)

	resp := openLive(t, ts, "/w/"+testWindowTok+"/live", "203.0.113.9")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("live: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, h := range []string{"Referrer-Policy", "X-Robots-Tag", "Content-Security-Policy"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("live response has no %s", h)
		}
	}
	frames := changedFrames(resp.Body)

	api.send(t, windowFixture(t, "phased"))
	first := nextChanged(t, frames)
	api.send(t, windowFixture(t, "phased"))
	api.send(t, windowFixture(t, "flat"))
	second := nextChanged(t, frames)
	if first == "" || first == second {
		t.Fatalf("changed frames %q then %q: want two different ETags, the repeat not sent", first, second)
	}

	r, err := http.Get(ts.URL + "/w/" + testWindowTok + "/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK || r.Header.Get("ETag") != second {
		t.Errorf("plan.json after the stream: %d ETag %q, want 200 %q", r.StatusCode, r.Header.Get("ETag"), second)
	}
	if n := api.n(); n != 0 {
		t.Errorf("%d daemon reads; the streamed view should have served plan.json", n)
	}
	if len(api.tokens) != 1 || api.tokens[0] != testWindowTok {
		t.Errorf("stream opened with %q, want the window token once", api.tokens)
	}
}

// pageFrames reads a page stream's named frames as "event data" onto a
// channel, closed when the stream ends.
func pageFrames(body io.Reader) <-chan string {
	ch := make(chan string, 8)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(body)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event != "":
				ch <- event + " " + strings.TrimPrefix(line, "data: ")
			case line == "":
				event = ""
			}
		}
	}()
	return ch
}

// TestWindowLive_RelaysHeartbeatAndGone: each kbeads keepalive reaches the
// page as a "heartbeat" frame, which window.js needs to count the stream
// live; kbeads' "gone" reaches it as "gone", and the stream ends.
func TestWindowLive_RelaysHeartbeatAndGone(t *testing.T) {
	api := &fakeLiveAPI{fakeWindowAPI: &fakeWindowAPI{}}
	ts, _ := liveServer(t, api)
	resp := openLive(t, ts, "/w/"+testWindowTok+"/live", "203.0.113.9")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live: %d", resp.StatusCode)
	}
	frames := pageFrames(resp.Body)
	next := func() (string, bool) {
		t.Helper()
		select {
		case fr, ok := <-frames:
			return fr, ok
		case <-time.After(5 * time.Second):
			t.Fatal("no frame in 5s")
		}
		return "", false
	}

	api.write(t, ":keepalive\n\n")
	if fr, _ := next(); fr != "heartbeat ok" {
		t.Fatalf("frame after a kbeads keepalive %q, want a heartbeat", fr)
	}
	api.write(t, "event: gone\ndata: {}\n\n")
	if fr, _ := next(); fr != "gone gone" {
		t.Fatalf("frame after kbeads said gone %q, want gone", fr)
	}
	if fr, ok := next(); ok {
		t.Fatalf("frame after gone %q; the stream should have ended", fr)
	}
}

// TestWindowLive_Refusals: every refusal is a status the page falls back to
// polling on, and a refusal by the daemon is the uniform 404.
func TestWindowLive_Refusals(t *testing.T) {
	for _, c := range []struct {
		name string
		api  windowAPI
		path string
		want int
	}{
		{"malformed token", &fakeLiveAPI{fakeWindowAPI: &fakeWindowAPI{}}, "/w/gw_short/live", http.StatusNotFound},
		{"no stream API", &fakeWindowAPI{}, "/w/" + testWindowTok + "/live", http.StatusNotFound},
		{"daemon 401", &fakeLiveAPI{fakeWindowAPI: &fakeWindowAPI{}, err: &beadsapi.APIError{StatusCode: 401}}, "/w/" + testWindowTok + "/live", http.StatusNotFound},
		{"window gone", &fakeLiveAPI{fakeWindowAPI: &fakeWindowAPI{}, err: &beadsapi.APIError{StatusCode: 410}}, "/w/" + testWindowTok + "/live", http.StatusNotFound},
		{"land window", &fakeLiveAPI{fakeWindowAPI: &fakeWindowAPI{}, err: &beadsapi.APIError{StatusCode: 404}}, "/w/" + testWindowTok + "/live", http.StatusNotFound},
		{"window at its cap", &fakeLiveAPI{fakeWindowAPI: &fakeWindowAPI{}, err: &beadsapi.APIError{StatusCode: 429}}, "/w/" + testWindowTok + "/live", http.StatusTooManyRequests},
		{"daemon down", &fakeLiveAPI{fakeWindowAPI: &fakeWindowAPI{}, err: errors.New("dial tcp: refused")}, "/w/" + testWindowTok + "/live", http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := windowServerFor(t, c.api, nil)
			w := windowGet(h, c.path)
			if w.Code != c.want {
				t.Errorf("%s: %d, want %d", c.path, w.Code, c.want)
			}
			if strings.Contains(w.Body.String(), testWindowTok) {
				t.Error("the response echoes the token")
			}
		})
	}
}

// TestWindowLive_CapPerVisitor: one visitor address holds windowLiveMaxPerIP
// streams; the next is 429, and another address is not affected.
func TestWindowLive_CapPerVisitor(t *testing.T) {
	api := &fakeLiveAPI{fakeWindowAPI: &fakeWindowAPI{}}
	ts, _ := liveServer(t, api)
	path := "/w/" + testWindowTok + "/live"
	for i := range windowLiveMaxPerIP {
		if resp := openLive(t, ts, path, "203.0.113.9"); resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: %d", i+1, resp.StatusCode)
		}
	}
	if resp := openLive(t, ts, path, "203.0.113.9"); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("stream past the cap: %d, want 429", resp.StatusCode)
	}
	if resp := openLive(t, ts, path, "198.51.100.4"); resp.StatusCode != http.StatusOK {
		t.Errorf("another visitor: %d, want 200", resp.StatusCode)
	}
}

func TestWindowVisitorIP(t *testing.T) {
	for _, c := range []struct{ xff, remote, want string }{
		{"203.0.113.9, 10.42.0.7", "10.42.1.1:5555", "203.0.113.9"},
		// Whatever a client put in front, the edge replaced; past two hops,
		// the entry two from the right is still the one the edge wrote.
		{"6.6.6.6, 203.0.113.9, 10.42.0.7", "10.42.1.1:5555", "203.0.113.9"},
		{"", "10.42.1.1:5555", "10.42.1.1"},
		{"203.0.113.9", "10.42.1.1:5555", "10.42.1.1"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/w/x/live", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := windowVisitorIP(r); got != c.want {
			t.Errorf("XFF %q from %s: %s, want %s", c.xff, c.remote, got, c.want)
		}
	}
}

// TestWindowCache_PutKeepsViewsCounted: a streamed view serves a refresh with
// no read, but a page view still goes to the daemon, which counts it.
func TestWindowCache_PutKeepsViewsCounted(t *testing.T) {
	api := &fakeWindowAPI{body: windowFixture(t, "phased")}
	c := newWindowCache(api)
	snap, err := buildWindowSnapshot(windowFixture(t, "flat"), c.now())
	if err != nil {
		t.Fatal(err)
	}
	c.put(testWindowTok, snap)
	got, err := c.get(context.Background(), testWindowTok, false)
	if err != nil || got != snap || api.n() != 0 {
		t.Fatalf("refresh after put: %v, read %d times; want the streamed view, no read", err, api.n())
	}
	if _, err := c.get(context.Background(), testWindowTok, true); err != nil {
		t.Fatal(err)
	}
	if api.n() != 1 || api.calls[0].views != 1 {
		t.Errorf("page view after put: calls %+v, want one read carrying the view", api.calls)
	}
}
