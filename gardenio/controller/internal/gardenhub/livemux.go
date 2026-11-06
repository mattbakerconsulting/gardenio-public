package gardenhub

// livemux.go — one EventSource per page (P2.4, gd-ymB1Pgg0fQ; inputs gardenio/docs/perf/netinv-2026-10-28.md). A page used
// to hold two or three event streams (the lists, the farm, and a panel's: the bead view, the cockpit, the Commons, the
// Glass or the Wall), and over HTTP/1.1 two tabs of three fill a browser's six connections per host. Now it holds one,
// GET /garden/live/events, which carries every stream the page wants as a channel of its own (web/live.js keeps them):
//
//   - The connection opens with its first streams (?s=, repeatable) and says hello with its sid. A stream added or let go
//     later is a POST /garden/live/sub {sid, add, rm} on that connection, as viewcut steers the farm stream, so opening a
//     panel or a gardener's card never reconnects the others.
//   - Each stream is the existing route itself, served through Live (the viewer's own auth, scope and machine chain, as
//     if the page had asked for it) with the connection's request: the same reads, the same cut, the same refusals. Only
//     the event-stream routes in liveRoutes may be named.
//   - Every event a stream writes goes out as one unnamed message whose first data line is "<key> <event>[ <id>]" and
//     whose other lines are the event's own data, so EventSource needs one listener and nothing is re-encoded. A stream's
//     own lifecycle is sent the same way under reserved names: ":open" once its route answered 200, ":err <status>" (data:
//     the route's words) when it refused, ":end" when it closed. The connection's own hello is key "*".
//   - Resume is the stream's own: each one is opened with the last id the page saw on it as its Last-Event-ID, which every
//     stream route already honours. Reconnecting, with full-jitter backoff, is the page's (live.js), never the browser's.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	// liveMaxStreams caps the streams one connection carries: a page holds two, a panel's, the Commons' and a card's.
	liveMaxStreams = 12
	// liveSpecMax caps one stream's spec (key, last id and path).
	liveSpecMax = 2 << 10
	// liveErrWords caps the words a refusing route's body sends with its :err.
	liveErrWords = 200
	// liveDrain is how long a closing connection waits for its streams' handlers to return.
	liveDrain = 2 * time.Second
)

// liveRoutes is every route a shared connection may carry: the page's event streams, nothing else.
var liveRoutes = func() *http.ServeMux {
	m := http.NewServeMux()
	for _, p := range []string{
		"GET /garden/events",
		"GET /garden/farm/events",
		"GET /garden/bead/{id}/events",
		"GET /garden/plan/{id}/events",
		"GET /garden/glass/wall",
		"GET /garden/glass/{agent}/stream",
		"GET /garden/c/events",
	} {
		m.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	return m
}()

// liveSpec is one stream a page asks for: its key (the page's, never reused on a connection), the last id it saw on it,
// and the route, below the viewer root.
type liveSpec struct {
	Key  string `json:"k"`
	Last string `json:"l,omitempty"`
	Path string `json:"u"`
}

// parseLiveSpec reads the ?s= form "<key>,<url-escaped last id>,<path>".
func parseLiveSpec(s string) (liveSpec, bool) {
	key, rest, ok := strings.Cut(s, ",")
	if !ok {
		return liveSpec{}, false
	}
	last, p, ok := strings.Cut(rest, ",")
	if !ok {
		return liveSpec{}, false
	}
	last, err := url.QueryUnescape(last)
	if err != nil {
		return liveSpec{}, false
	}
	return liveSpec{Key: key, Last: last, Path: p}, true
}

func validLiveKey(k string) bool {
	if k == "" || len(k) > 16 {
		return false
	}
	for _, c := range k {
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// liveTarget is the stream's route as a URL below the viewer root, or false when it is not one liveRoutes names.
func (h *Handler) liveTarget(sp liveSpec) (*url.URL, bool) {
	if !validLiveKey(sp.Key) || len(sp.Key)+len(sp.Last)+len(sp.Path) > liveSpecMax || strings.ContainsAny(sp.Last, "\r\n") {
		return nil, false
	}
	p := sp.Path
	if h.BasePath != "" && strings.HasPrefix(p, h.BasePath+"/garden/") {
		p = strings.TrimPrefix(p, h.BasePath)
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Fragment != "" || !strings.HasPrefix(u.Path, "/garden/") ||
		path.Clean(u.Path) != u.Path {
		return nil, false
	}
	probe := &http.Request{Method: http.MethodGet, URL: u, Host: "garden"}
	if _, pattern := liveRoutes.Handler(probe); pattern == "" {
		return nil, false
	}
	return u, true
}

// liveConns is the open shared connections by sid, for POST /garden/live/sub.
type liveConns struct {
	mu sync.Mutex
	by map[string]*liveConn
}

func (l *liveConns) put(c *liveConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.by == nil {
		l.by = map[string]*liveConn{}
	}
	l.by[c.sid] = c
}

func (l *liveConns) get(sid string) *liveConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.by[sid]
}

func (l *liveConns) drop(sid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.by, sid)
}

// liveConn is one page's connection: the response every stream writes into, one frame at a time.
type liveConn struct {
	sid   string
	r     *http.Request // the connection's request: whose auth, scope and context every stream runs on
	serve http.Handler

	mu     sync.Mutex // guards w, closed and subs
	w      http.ResponseWriter
	fl     http.Flusher
	closed bool
	subs   map[string]*liveSub
	wg     sync.WaitGroup
}

// liveSub is one stream on a connection.
type liveSub struct {
	key    string
	cancel context.CancelFunc
}

// emit writes one frame for key and flushes it, unless the connection closed or (from) the stream was let go.
func (c *liveConn) emit(from *liveSub, key, event, id string, data []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || (from != nil && c.subs[from.key] != from) {
		return false
	}
	var b bytes.Buffer
	b.WriteString("data: ")
	b.WriteString(key)
	b.WriteByte(' ')
	b.WriteString(event)
	if id != "" {
		b.WriteByte(' ')
		b.WriteString(id)
	}
	b.WriteByte('\n')
	for _, d := range data {
		b.WriteString("data: ")
		b.WriteString(d)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	_, _ = c.w.Write(b.Bytes())
	c.fl.Flush()
	return true
}

// add starts sp on the connection, replacing a stream under the same key. false: the connection is closed or full.
func (c *liveConn) add(u *url.URL, sp liveSpec) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	if old := c.subs[sp.Key]; old != nil {
		old.cancel()
		delete(c.subs, sp.Key)
	}
	if len(c.subs) >= liveMaxStreams {
		c.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(c.r.Context())
	s := &liveSub{key: sp.Key, cancel: cancel}
	c.subs[sp.Key] = s
	c.wg.Add(1)
	c.mu.Unlock()

	sub := c.r.Clone(ctx)
	sub.Method = http.MethodGet
	sub.URL = &url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}
	sub.RequestURI = u.RequestURI()
	sub.Body, sub.ContentLength = http.NoBody, 0
	sub.Header.Del("Last-Event-ID")
	sub.Header.Del("Accept-Encoding")
	if sp.Last != "" {
		sub.Header.Set("Last-Event-ID", sp.Last)
	}
	go func() {
		defer c.wg.Done()
		defer cancel()
		sw := &liveSubWriter{c: c, s: s, hdr: http.Header{}}
		c.serve.ServeHTTP(sw, sub)
		sw.finish()
	}()
	return true
}

// rm lets the stream under key go; its handler returns once its context ends, and nothing it writes after is sent.
func (c *liveConn) rm(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.subs[key]; s != nil {
		s.cancel()
		delete(c.subs, key)
	}
}

// release forgets s if it is still the stream under its key: false when it was let go, replaced or closed with the
// connection, so nothing more is said of it.
func (c *liveConn) release(s *liveSub) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.subs[s.key] != s {
		return false
	}
	delete(c.subs, s.key)
	return true
}

func (c *liveConn) close() {
	c.mu.Lock()
	c.closed = true
	for k, s := range c.subs {
		s.cancel()
		delete(c.subs, k)
	}
	c.mu.Unlock()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(liveDrain): // a handler stuck in a read: its writes are dropped (emit checks closed)
	}
}

var errLiveGone = errors.New("live stream let go")

// liveSubWriter is the ResponseWriter one stream's route writes to: it cuts what the route writes into SSE frames and
// sends each as a frame of the connection's, and says how the route answered.
type liveSubWriter struct {
	c      *liveConn
	s      *liveSub
	hdr    http.Header
	code   int
	opened bool
	buf    []byte
	words  []byte
}

func (w *liveSubWriter) Header() http.Header { return w.hdr }

func (w *liveSubWriter) WriteHeader(code int) {
	if w.code != 0 || code < 200 {
		return
	}
	w.code = code
	if code == http.StatusOK && strings.HasPrefix(w.hdr.Get("Content-Type"), "text/event-stream") {
		w.opened = w.c.emit(w.s, w.s.key, ":open", "", nil)
	}
}

func (w *liveSubWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if !w.opened {
		if room := liveErrWords - len(w.words); room > 0 {
			w.words = append(w.words, p[:min(room, len(p))]...)
		}
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	for {
		i := bytes.Index(w.buf, []byte("\n\n"))
		if i < 0 {
			break
		}
		frame := w.buf[:i]
		w.buf = w.buf[i+2:]
		if !w.frame(frame) {
			return 0, errLiveGone
		}
	}
	return len(p), nil
}

// Flush is a no-op past the header: emit flushes every frame it sends.
func (w *liveSubWriter) Flush() { w.WriteHeader(http.StatusOK) }

// frame sends one SSE frame the route wrote: its event, id and data lines. A frame with no data (a comment, a retry) is
// one EventSource would not dispatch, so it is dropped. false: the stream is gone.
func (w *liveSubWriter) frame(raw []byte) bool {
	event, id := "message", ""
	var data []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || line[0] == ':' {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "id":
			id = value
		case "data":
			data = append(data, value)
		}
	}
	if len(data) == 0 {
		return true
	}
	if event == "" || strings.ContainsAny(event, " ") {
		event = "message"
	}
	return w.c.emit(w.s, w.s.key, event, id, data)
}

// finish says how the stream ended: :end after an open stream, :err with the route's status and words for a refusal.
func (w *liveSubWriter) finish() {
	if !w.c.release(w.s) {
		return
	}
	if w.opened {
		w.c.emit(nil, w.s.key, ":end", "", nil)
		return
	}
	code := w.code
	if code == 0 || code == http.StatusOK {
		code = http.StatusBadGateway // answered nothing, or not a stream
	}
	words := strings.TrimSpace(string(w.words))
	words = strings.Join(strings.Fields(words), " ")
	w.c.emit(nil, w.s.key, ":err", fmt.Sprint(code), []string{words})
}

func newLiveSID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// liveEvents serves GET /garden/live/events: the page's one connection, carrying the streams its ?s= names to begin with.
func (h *Handler) liveEvents(w http.ResponseWriter, r *http.Request) {
	if h.Live == nil {
		http.Error(w, "shared stream not enabled", http.StatusServiceUnavailable)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	type first struct {
		u  *url.URL
		sp liveSpec
	}
	var streams []first
	for _, raw := range r.URL.Query()["s"] {
		sp, ok := parseLiveSpec(raw)
		u, okT := h.liveTarget(sp)
		if !ok || !okT {
			http.Error(w, "bad stream", http.StatusBadRequest)
			return
		}
		streams = append(streams, first{u, sp})
	}
	if len(streams) > liveMaxStreams {
		http.Error(w, "too many streams", http.StatusBadRequest)
		return
	}

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	c := &liveConn{sid: newLiveSID(), r: r, serve: h.Live, w: w, fl: fl, subs: map[string]*liveSub{}}
	h.live.put(c)
	defer h.live.drop(c.sid)
	defer c.close()
	hello, _ := json.Marshal(map[string]string{"sid": c.sid})
	c.emit(nil, "*", ":hello", "", []string{string(hello)})
	for _, s := range streams {
		c.add(s.u, s.sp)
	}
	<-r.Context().Done()
}

// liveSubscribe serves POST /garden/live/sub: {sid, add: [{k, l, u}], rm: [k]} adds and lets go streams on a connection.
// It only steers what that connection is sent, every stream still read as the connection's own viewer, so the sid is
// the whole credential (as for the farm's view, farm_http.go); a stale or unknown one is a 404, and the page reconnects.
func (h *Handler) liveSubscribe(w http.ResponseWriter, r *http.Request) {
	if h.Live == nil {
		http.Error(w, "shared stream not enabled", http.StatusServiceUnavailable)
		return
	}
	if crossOrigin(r) {
		http.Error(w, "cross-origin", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var in struct {
		SID string     `json:"sid"`
		Add []liveSpec `json:"add"`
		Rm  []string   `json:"rm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	us := make([]*url.URL, len(in.Add))
	for i, sp := range in.Add {
		u, ok := h.liveTarget(sp)
		if !ok {
			http.Error(w, "bad stream", http.StatusBadRequest)
			return
		}
		us[i] = u
	}
	c := h.live.get(in.SID)
	if c == nil {
		http.Error(w, "no such stream", http.StatusNotFound)
		return
	}
	for _, k := range in.Rm {
		c.rm(k)
	}
	for i, sp := range in.Add {
		if !c.add(us[i], sp) {
			http.Error(w, "too many streams", http.StatusConflict)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
