package gardenhub

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// liveFrame is one message of the shared connection: its head's key, event and id, and its own data.
type liveFrame struct {
	key, event, id, data string
}

// liveReader reads the shared connection's messages as EventSource would: data lines joined, one message per blank line.
type liveReader struct {
	t  *testing.T
	ch chan liveFrame
}

func readLive(t *testing.T, resp *http.Response) *liveReader {
	lr := &liveReader{t: t, ch: make(chan liveFrame, 64)}
	go func() {
		defer close(lr.ch)
		sc := bufio.NewScanner(resp.Body)
		var data []string
		for sc.Scan() {
			line := sc.Text()
			if line != "" {
				if v, ok := strings.CutPrefix(line, "data: "); ok {
					data = append(data, v)
				} else if !strings.HasPrefix(line, ":") {
					lr.ch <- liveFrame{event: "?" + line} // the connection itself names no field but data
				}
				continue
			}
			if len(data) == 0 {
				continue
			}
			head := strings.SplitN(data[0], " ", 3)
			f := liveFrame{key: head[0], data: strings.Join(data[1:], "\n")}
			if len(head) > 1 {
				f.event = head[1]
			}
			if len(head) > 2 {
				f.id = head[2]
			}
			lr.ch <- f
			data = nil
		}
	}()
	return lr
}

// until reads frames until want returns true for one, and returns every frame read.
func (lr *liveReader) until(want func(liveFrame) bool) []liveFrame {
	lr.t.Helper()
	var seen []liveFrame
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-lr.ch:
			if !ok {
				lr.t.Fatalf("connection closed; read %+v", seen)
			}
			seen = append(seen, f)
			if want(f) {
				return seen
			}
		case <-timeout:
			lr.t.Fatalf("timed out; read %+v", seen)
		}
	}
}

func liveIs(key, event string) func(liveFrame) bool {
	return func(f liveFrame) bool { return f.key == key && f.event == event }
}

// liveStreams stands in for the viewer's route chain: a stream that says what it was asked with and ticks until it is
// let go, a refused one, one that ends, and one with a multi-line event.
func liveStreams(lets *sync.Map) http.Handler {
	mux := http.NewServeMux()
	sse := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}
	mux.HandleFunc("GET /garden/events", func(w http.ResponseWriter, r *http.Request) {
		sse(w)
		fmt.Fprint(w, "retry: 2000\n: connected\n\n")
		fmt.Fprintf(w, "id: 5\nevent: asked\ndata: {\"last\":%q,\"since\":%q,\"who\":%q}\n\n",
			r.Header.Get("Last-Event-ID"), r.URL.Query().Get("since"), r.Header.Get("X-Who"))
		w.(http.Flusher).Flush()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for n := 0; ; n++ {
			select {
			case <-r.Context().Done():
				lets.Store(r.URL.Path, true)
				return
			case <-tick.C:
				fmt.Fprintf(w, "event: hb\ndata: {\"n\":%d}\n\n", n)
				w.(http.Flusher).Flush()
			}
		}
	})
	mux.HandleFunc("GET /garden/bead/{id}/events", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such bead", http.StatusNotFound)
	})
	mux.HandleFunc("GET /garden/plan/{id}/events", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
	})
	mux.HandleFunc("GET /garden/glass/wall", func(w http.ResponseWriter, _ *http.Request) {
		sse(w)
		fmt.Fprint(w, "event: tiles\ndata: {\"a\":1}\ndata: {\"b\":2}\n\n") // ends at once
	})
	mux.HandleFunc("GET /garden/c/events", func(w http.ResponseWriter, r *http.Request) {
		sse(w)
		fmt.Fprint(w, ": commons\n\n")
		fmt.Fprint(w, "id: 2026-10-29T03:00:00Z\nevent: ping\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	return mux
}

func liveServer(t *testing.T, base string) (*httptest.Server, *sync.Map) {
	t.Helper()
	lets := &sync.Map{}
	h := &Handler{BasePath: base, Live: liveStreams(lets)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /garden/live/events", h.liveEvents)
	mux.HandleFunc("POST /garden/live/sub", h.liveSubscribe)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, lets
}

func liveOpen(t *testing.T, srv *httptest.Server, specs ...string) (*http.Response, *liveReader, string) {
	t.Helper()
	q := url.Values{"s": specs}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/garden/live/events?"+q.Encode(), nil)
	req.Header.Set("X-Who", "oidc:me")
	req.Header.Set("Last-Event-ID", "999") // the connection's own: never handed to a stream
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect: %d", resp.StatusCode)
	}
	lr := readLive(t, resp)
	hello := lr.until(liveIs("*", ":hello"))
	sid := strings.TrimSuffix(strings.TrimPrefix(hello[len(hello)-1].data, `{"sid":"`), `"}`)
	if len(sid) != 32 {
		t.Fatalf("hello %q", hello[len(hello)-1].data)
	}
	return resp, lr, sid
}

func livePost(t *testing.T, srv *httptest.Server, body string, hdr ...string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/garden/live/sub", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// One connection carries every stream the page asks for (P2.4, gd-ymB1Pgg0fQ): each is the route itself, run on the
// connection's own request (its auth headers; never the connection's Last-Event-ID), opened with the last id the page saw
// on it; its events come through as "<key> <event> <id>" heads over its own data lines, comments and retry lines
// dropped; a refusal is :err with the route's status and words, a stream that closes is :end. Later streams are added and
// let go by POST on the connection's sid, and a let-go stream's handler is cancelled and says nothing more.
func TestLiveMuxCarriesStreams(t *testing.T) {
	srv, lets := liveServer(t, "/v")
	_, lr, sid := liveOpen(t, srv, "1,5,/garden/events?since=4", "2,,/v/garden/bead/gd-x/events", "c,"+url.QueryEscape("2026-10-29T02:00:00Z")+",/garden/c/events")

	var asked, refused, ping liveFrame
	opened := map[string]bool{}
	for asked.key == "" || refused.key == "" || ping.key == "" {
		f := lr.until(func(liveFrame) bool { return true })[0]
		switch {
		case f.event == ":open":
			opened[f.key] = true
		case f.key == "1" && f.event == "asked":
			asked = f
		case f.key == "2":
			refused = f
		case f.key == "c" && f.event == "ping":
			ping = f
		case f.event == "hb" || f.event == "ping":
		default:
			t.Fatalf("unexpected frame %+v", f)
		}
	}
	if !opened["1"] || !opened["c"] || opened["2"] {
		t.Errorf("opened %v, want 1 and c, never 2", opened)
	}
	if want := `{"last":"5","since":"4","who":"oidc:me"}`; asked.id != "5" || asked.data != want {
		t.Errorf("asked = %+v, want id 5 and %s", asked, want)
	}
	if refused.event != ":err" || refused.id != "404" || refused.data != "no such bead" {
		t.Errorf("refused = %+v, want :err 404 with the route's words", refused)
	}
	if ping.id != "2026-10-29T03:00:00Z" || ping.data != "{}" {
		t.Errorf("ping = %+v", ping)
	}

	// later: a stream that ends, one refused for now, and the first let go
	if code := livePost(t, srv, `{"sid":"`+sid+`","add":[{"k":"3","u":"/garden/glass/wall"},{"k":"4","u":"/garden/plan/gd-p/events"}],"rm":["1"]}`); code != http.StatusNoContent {
		t.Fatalf("sub: %d", code)
	}
	var tiles, end, gw liveFrame
	for end.key == "" || gw.key == "" {
		f := lr.until(func(liveFrame) bool { return true })[0]
		switch {
		case f.key == "1" && f.event == "hb":
			if tiles.key != "" || end.key != "" || gw.key != "" {
				t.Fatalf("stream 1 spoke after it was let go and a later stream had: %+v", f)
			}
		case f.key == "3" && f.event == "tiles":
			tiles = f
		case f.key == "3" && f.event == ":end":
			end = f
		case f.key == "4" && f.event == ":err":
			gw = f
		case f.event == ":open" && f.key == "3", f.event == "ping":
		default:
			t.Fatalf("unexpected frame %+v", f)
		}
	}
	if tiles.data != "{\"a\":1}\n{\"b\":2}" {
		t.Errorf("a multi-line event: %q", tiles.data)
	}
	if gw.id != "502" || gw.data != "the beads daemon did not answer" {
		t.Errorf("refused for now = %+v", gw)
	}
	deadline := time.Now().Add(2 * time.Second)
	for _, ok := lets.Load("/garden/events"); !ok; _, ok = lets.Load("/garden/events") {
		if time.Now().After(deadline) {
			t.Fatal("the let-go stream's handler never saw its context end")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// What a connection may name, and who may steer it: only the page's event-stream routes, below the root; no scheme,
// host or dot segments; a POST needs the live sid and the same origin. Off (no Live), both routes say so.
func TestLiveMuxRefuses(t *testing.T) {
	srv, _ := liveServer(t, "")
	for _, s := range []string{
		"1,,/garden/settings",                  // a route, but not a stream
		"1,,/garden/../garden/events",          // not clean
		"1,,http://evil.example/garden/events", // not below the root
		"1,,//evil.example/garden/events",
		"bad key,,/garden/events",
		"1,/garden/events", // no last-id field
		"1,a%0Ab,/garden/events",
		"1,,/b/gd-x",
	} {
		resp, err := http.Get(srv.URL + "/garden/live/events?" + url.Values{"s": {s}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", s, resp.StatusCode)
		}
	}
	many := url.Values{}
	for i := 0; i <= liveMaxStreams; i++ {
		many.Add("s", fmt.Sprintf("k%d,,/garden/events", i))
	}
	if resp, err := http.Get(srv.URL + "/garden/live/events?" + many.Encode()); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("too many streams: %v %v", resp.StatusCode, err)
	}

	_, _, sid := liveOpen(t, srv)
	if code := livePost(t, srv, `{"sid":"nope","add":[{"k":"1","u":"/garden/events"}]}`); code != http.StatusNotFound {
		t.Errorf("unknown sid: %d", code)
	}
	if code := livePost(t, srv, `{"sid":"`+sid+`","add":[{"k":"1","u":"/garden/settings"}]}`); code != http.StatusBadRequest {
		t.Errorf("not a stream: %d", code)
	}
	if code := livePost(t, srv, `{"sid":"`+sid+`","rm":["1"]}`, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-site: %d", code)
	}

	off := &Handler{}
	for _, h := range []http.HandlerFunc{off.liveEvents, off.liveSubscribe} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/garden/live/sub", strings.NewReader(`{}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("no Live: %d, want 503", rec.Code)
		}
	}
}

// The page's half (web/live.js) runs under node (testdata/live_test.js): a turn's streams are one connection, later ones a
// POST on it; events reach their stream with their lastEventId; a 404 closes a stream for good, a 502 or an end asks for it
// again with its last id; a dropped connection or a refused POST reconnects with every stream's last id; full-jitter
// backoff below 1 s doubling to 30 s; and without data-live a stream is the browser's own EventSource.
func TestLiveJSUnderNode(t *testing.T) {
	r := nodeScript(t, "live_test.js")
	t.Logf("live.js: %v connections, %v POSTs", r["connections"], r["posts"])
}
