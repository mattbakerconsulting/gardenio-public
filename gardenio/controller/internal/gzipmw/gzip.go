// Package gzipmw is beads-viewer's response compression (gd-oRf2tdEqsQ), shared with the Lands perf rig
// (gardenhub TestLandsPerfBaseline) so its byte budgets grade what a browser is sent (gd-GO1ba3rosB).
package gzipmw

import (
	"bufio"
	"compress/gzip"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMinSize is the smallest body worth compressing: below about one packet
// the gzip header and the CPU cost more than the bytes saved.
const gzipMinSize = 1024

var gzipWriters = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
	return zw
}}

// Middleware compresses text responses (HTML, CSS, JS, JSON, SVG) for a
// client that accepts gzip (gd-oRf2tdEqsQ). Nothing on the way out does it:
// the public edge and viewer-authproxy run nginx with gzip off, so every page,
// /assets/static/ file (xterm.bundle.js, 285 KB) and JSON API went out raw,
// and the splash's three.module.min.js was 742 KB on a phone's first visit.
//
// beads-viewer puts it outermost on the listener, so it sees each route's final headers,
// and it never touches:
//   - text/event-stream: an SSE stream goes out uncompressed and every Flush
//     reaches the client straight away, so no event waits in a gzip buffer;
//   - a response that already has a Content-Encoding (/garden and the farm
//     snapshot gzip themselves, cached per version) or a Content-Range (a 206
//     from http.FileServer: its byte ranges count identity bytes);
//   - HEAD, an Upgrade (the Glass and Grafana Live websockets), 1xx, 204 and
//     304, and a body under gzipMinSize.
//
// ETags are left as the route set them. The routes compare If-None-Match by
// exact string (gardenhub's page, farm, splash and arms), and /garden already
// sends one ETag for both encodings, so weakening it here would cost them
// their 304s.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || r.Header.Get("Upgrade") != "" || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// acceptsGzip reports whether an Accept-Encoding header admits gzip, honouring
// q=0 ("gzip;q=0" refuses it; "*" admits it unless gzip is refused by name).
func acceptsGzip(header string) bool {
	star := false
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		if coding != "gzip" && coding != "x-gzip" && coding != "*" {
			continue
		}
		ok := true
		for _, p := range strings.Split(params, ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			if strings.EqualFold(strings.TrimSpace(k), "q") {
				if q, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && q == 0 {
					ok = false
				}
			}
		}
		if coding == "*" {
			star = ok
			continue
		}
		return ok
	}
	return star
}

// compressibleType reports whether a Content-Type is text worth gzipping.
// text/event-stream is not: compressing it would hold events in the buffer.
func compressibleType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	switch {
	case mt == "text/event-stream":
		return false
	case strings.HasPrefix(mt, "text/"):
		return true
	case strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true
	}
	switch mt {
	case "application/json", "application/javascript", "application/x-javascript", "application/ecmascript",
		"application/xml", "application/x-ndjson", "image/svg+xml":
		return true
	}
	return false
}

// gzipResponseWriter decides at the first moment it must (the body passing
// gzipMinSize, a Flush, or the handler returning) whether this response is
// compressed or passed through untouched. Until then it holds the status and
// up to gzipMinSize bytes.
//
// Like beads-viewer's statusRecorder it re-exposes http.Flusher and
// http.Hijacker by hand, because the SSE routes and httputil.ReverseProxy
// type-assert for them.
type gzipResponseWriter struct {
	http.ResponseWriter
	status  int // held status; 0 until WriteHeader or the first Write
	decided bool
	zw      *gzip.Writer // non-nil once compressing
	buf     []byte       // body held while undecided
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.decided || g.status != 0 {
		if g.decided && g.zw == nil {
			g.ResponseWriter.WriteHeader(code) // net/http logs the superfluous call, as it would without us
		}
		return
	}
	if code < 200 {
		g.ResponseWriter.WriteHeader(code) // informational (103 Early Hints): send now, the real status follows
		return
	}
	g.status = code
	// Decide now when the headers already say no, so a stream, a pre-gzipped
	// page or a file range goes straight through without being held.
	if !g.eligible(false) {
		g.passThrough()
	}
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if g.status == 0 {
		g.sniff(p)
		g.WriteHeader(http.StatusOK)
	}
	switch {
	case g.zw != nil:
		return g.zw.Write(p)
	case g.decided:
		return g.ResponseWriter.Write(p)
	}
	g.buf = append(g.buf, p...)
	if len(g.buf) >= gzipMinSize {
		if err := g.startGzip(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush commits to a decision on what is held so far, then flushes through.
func (g *gzipResponseWriter) Flush() {
	if !g.decided {
		if g.status == 0 {
			g.WriteHeader(http.StatusOK)
		}
		if !g.decided {
			if g.eligible(true) && len(g.buf) > 0 {
				_ = g.startGzip()
			} else {
				g.passThrough()
			}
		}
	}
	if g.zw != nil {
		_ = g.zw.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack hands over the connection only while nothing has been written.
func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := g.ResponseWriter.(http.Hijacker); ok && !g.decided && g.status == 0 {
		g.decided = true
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// close finishes the response once the handler returns: a body still held
// under gzipMinSize goes out as it is, and a gzip stream gets its trailer.
func (g *gzipResponseWriter) close() {
	if g.zw != nil {
		_ = g.zw.Close()
		g.zw.Reset(io.Discard)
		gzipWriters.Put(g.zw)
		g.zw = nil
		return
	}
	if !g.decided && (g.status != 0 || len(g.buf) > 0) {
		if g.status == 0 {
			g.status = http.StatusOK
		}
		g.passThrough()
	}
}

// eligible reports whether the response as it stands may be compressed. With
// final set, a missing Content-Length no longer means "maybe big enough".
func (g *gzipResponseWriter) eligible(final bool) bool {
	h := g.Header()
	if g.status == http.StatusNoContent || g.status == http.StatusNotModified || g.status == http.StatusPartialContent {
		return false
	}
	if h.Get("Content-Encoding") != "" || h.Get("Content-Range") != "" {
		return false
	}
	if ct := h.Get("Content-Type"); ct != "" && !compressibleType(ct) {
		return false
	} else if ct == "" && final && !compressibleType(http.DetectContentType(g.buf)) {
		return false
	}
	if cl := h.Get("Content-Length"); cl != "" {
		if n, err := strconv.Atoi(cl); err == nil && n < gzipMinSize {
			return false
		}
	}
	return true
}

// sniff sets the Content-Type net/http would have sniffed from the first
// write, so the decision sees it. A handler that set the key, even empty, has
// opted out of sniffing and is left alone.
func (g *gzipResponseWriter) sniff(p []byte) {
	if _, set := g.Header()["Content-Type"]; !set && len(p) > 0 {
		g.Header().Set("Content-Type", http.DetectContentType(p))
	}
}

func (g *gzipResponseWriter) passThrough() {
	g.decided = true
	g.ResponseWriter.WriteHeader(g.status)
	if len(g.buf) > 0 {
		_, _ = g.ResponseWriter.Write(g.buf)
		g.buf = nil
	}
}

func (g *gzipResponseWriter) startGzip() error {
	if _, set := g.Header()["Content-Type"]; !set {
		g.sniff(g.buf)
	}
	if !g.eligible(true) {
		g.passThrough()
		return nil
	}
	g.decided = true
	h := g.Header()
	h.Set("Content-Encoding", "gzip")
	h.Del("Content-Length")
	h.Del("Accept-Ranges")
	if !varyHas(h, "Accept-Encoding") {
		h.Add("Vary", "Accept-Encoding")
	}
	g.ResponseWriter.WriteHeader(g.status)
	g.zw = gzipWriters.Get().(*gzip.Writer)
	g.zw.Reset(g.ResponseWriter)
	buf := g.buf
	g.buf = nil
	_, err := g.zw.Write(buf)
	return err
}

func varyHas(h http.Header, field string) bool {
	for _, v := range h.Values("Vary") {
		for _, f := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), field) {
				return true
			}
		}
	}
	return false
}
