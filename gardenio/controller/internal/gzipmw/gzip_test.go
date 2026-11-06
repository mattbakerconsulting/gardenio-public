package gzipmw

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// bigHTML is comfortably over gzipMinSize and compresses well, like a page.
var bigHTML = "<!doctype html><title>t</title>" + strings.Repeat("<p>the garden grows</p>\n", 400)

func gzipServe(t *testing.T, h http.Handler, method, acceptEncoding string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/x", nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	Middleware(h).ServeHTTP(rec, req)
	return rec
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(out)
}

func TestGzipCompressesTextAndKeepsHeaders(t *testing.T) {
	cases := map[string]string{
		"html": "text/html; charset=utf-8",
		"css":  "text/css; charset=utf-8",
		"js":   "text/javascript; charset=utf-8",
		"json": "application/json",
		"svg":  "image/svg+xml",
	}
	for name, ct := range cases {
		t.Run(name, func(t *testing.T) {
			rec := gzipServe(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", ct)
				w.Header().Set("Content-Length", "99999") // a stale length must not survive compression
				w.Header().Set("ETag", `"v1"`)
				w.WriteHeader(http.StatusCreated)
				// Many small writes, as html/template does.
				for i := 0; i < len(bigHTML); i += 100 {
					_, _ = io.WriteString(w, bigHTML[i:min(i+100, len(bigHTML))])
				}
			}), http.MethodGet, "gzip, deflate, br")
			if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", got)
			}
			if rec.Code != http.StatusCreated {
				t.Errorf("status = %d, want 201", rec.Code)
			}
			if got := rec.Header().Get("Content-Length"); got != "" {
				t.Errorf("Content-Length = %q survived compression", got)
			}
			if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
				t.Errorf("Vary = %q", got)
			}
			if got := rec.Header().Get("ETag"); got != `"v1"` {
				t.Errorf("ETag = %q, want it untouched", got)
			}
			if got := gunzip(t, rec.Body.Bytes()); got != bigHTML {
				t.Errorf("body round-trip differs: %d bytes, want %d", len(got), len(bigHTML))
			}
			if rec.Body.Len()*4 > len(bigHTML) {
				t.Errorf("wire %d bytes for %d raw: barely compressed", rec.Body.Len(), len(bigHTML))
			}
		})
	}
}

func TestGzipSniffsAnUntypedBody(t *testing.T) {
	rec := gzipServe(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, bigHTML)
	}), http.MethodGet, "gzip")
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("untyped HTML not compressed")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want the sniffed text/html", ct)
	}
}

func TestGzipVaryAppendsOnce(t *testing.T) {
	rec := gzipServe(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Vary", "Cookie, accept-encoding")
		_, _ = io.WriteString(w, bigHTML)
	}), http.MethodGet, "gzip")
	if got := rec.Header().Values("Vary"); len(got) != 1 || got[0] != "Cookie, accept-encoding" {
		t.Errorf("Vary = %q, want the route's value alone", got)
	}
}

func TestGzipLeavesAlone(t *testing.T) {
	html := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, bigHTML)
	}
	cases := []struct {
		name   string
		method string
		ae     string
		h      http.HandlerFunc
	}{
		{"no Accept-Encoding", http.MethodGet, "", html},
		{"gzip refused by q=0", http.MethodGet, "br, gzip;q=0, *;q=1", html},
		{"HEAD", http.MethodHead, "gzip", html},
		{"small body", http.MethodGet, "gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		}},
		{"small Content-Length", http.MethodGet, "gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", "10")
			_, _ = io.WriteString(w, "Forbidden\n")
		}},
		{"image", http.MethodGet, "gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, bigHTML)
		}},
		{"already encoded", http.MethodGet, "gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Encoding", "identity-ish")
			_, _ = io.WriteString(w, bigHTML)
		}},
		{"partial content", http.MethodGet, "gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/javascript")
			w.Header().Set("Content-Range", "bytes 0-9999/20000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, bigHTML)
		}},
		{"not modified", http.MethodGet, "gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNotModified)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := httptest.NewRecorder()
			tc.h(want, httptest.NewRequest(tc.method, "/x", nil))
			rec := gzipServe(t, tc.h, tc.method, tc.ae)
			if rec.Code != want.Code {
				t.Errorf("status = %d, want %d", rec.Code, want.Code)
			}
			if got, w := rec.Header().Get("Content-Encoding"), want.Header().Get("Content-Encoding"); got != w {
				t.Errorf("Content-Encoding = %q, want %q", got, w)
			}
			if rec.Body.String() != want.Body.String() {
				t.Errorf("body changed: %d bytes, want %d", rec.Body.Len(), want.Body.Len())
			}
			if rec.Header().Get("Vary") != "" {
				t.Errorf("Vary = %q on an uncompressed response", rec.Header().Get("Vary"))
			}
		})
	}
}

// TestGzipPreGzippedPage covers /garden and the farm snapshot, which gzip
// themselves: the middleware must not compress them twice.
func TestGzipPreGzippedPage(t *testing.T) {
	var pre bytes.Buffer
	zw := gzip.NewWriter(&pre)
	_, _ = io.WriteString(zw, bigHTML)
	_ = zw.Close()
	rec := gzipServe(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Vary", "Cookie, Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(pre.Bytes())
	}), http.MethodGet, "gzip")
	if !bytes.Equal(rec.Body.Bytes(), pre.Bytes()) {
		t.Fatal("a pre-gzipped body was re-encoded")
	}
	if got := gunzip(t, rec.Body.Bytes()); got != bigHTML {
		t.Fatal("pre-gzipped page does not decode")
	}
}

// TestGzipStaticFiles covers /assets/static/ (xterm.bundle.js) through
// http.FileServer: a whole file is compressed, a Range request is not.
func TestGzipStaticFiles(t *testing.T) {
	js := strings.Repeat("function f(){return 1}\n", 500)
	fs := http.FileServer(http.FS(fstest.MapFS{"xterm.bundle.js": {Data: []byte(js)}}))

	req := httptest.NewRequest(http.MethodGet, "/xterm.bundle.js", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	full := httptest.NewRecorder()
	Middleware(fs).ServeHTTP(full, req)
	if full.Code != http.StatusOK || full.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("whole file: status %d, Content-Encoding %q", full.Code, full.Header().Get("Content-Encoding"))
	}
	if full.Header().Get("Accept-Ranges") != "" || full.Header().Get("Content-Length") != "" {
		t.Errorf("compressed file kept Accept-Ranges %q / Content-Length %q", full.Header().Get("Accept-Ranges"), full.Header().Get("Content-Length"))
	}
	if gunzip(t, full.Body.Bytes()) != js {
		t.Fatal("file round-trip differs")
	}

	req = httptest.NewRequest(http.MethodGet, "/xterm.bundle.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Range", "bytes=0-1999")
	part := httptest.NewRecorder()
	Middleware(fs).ServeHTTP(part, req)
	if part.Code != http.StatusPartialContent || part.Header().Get("Content-Encoding") != "" {
		t.Fatalf("range: status %d, Content-Encoding %q", part.Code, part.Header().Get("Content-Encoding"))
	}
	if part.Body.String() != js[:2000] {
		t.Fatal("range body is not the identity bytes")
	}
}

func TestAcceptsGzip(t *testing.T) {
	for h, want := range map[string]bool{
		"":                      false,
		"gzip":                  true,
		"gzip, deflate, br":     true,
		"br":                    false,
		"GZIP;q=0.5":            true,
		"gzip;q=0":              false,
		"gzip; q=0.0":           false,
		"*":                     true,
		"br, *;q=0":             false,
		"gzip;q=0, *":           false,
		"identity, x-gzip;q=.1": true,
	} {
		if got := acceptsGzip(h); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", h, got, want)
		}
	}
}

// TestGzipSSEStreamsUnbuffered is the proof the plan step asks for: through
// the middleware, over a real connection, an SSE route's first event reaches
// the client before the handler writes its second, with no Content-Encoding.
// The handler blocks until the client has read event one, so any buffering in
// the wrapper (gzip's or the min-size hold) fails the read's deadline.
func TestGzipSSEStreamsUnbuffered(t *testing.T) {
	gotFirst := make(chan struct{})
	done := make(chan struct{})
	srv := httptest.NewServer(Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("the middleware's writer is not an http.Flusher")
			return
		}
		_, _ = io.WriteString(w, "event: hello\ndata: 1\n\n")
		fl.Flush()
		select {
		case <-gotFirst:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			t.Error("client never saw event one: the stream is buffered")
			return
		}
		_, _ = io.WriteString(w, "event: hello\ndata: 2\n\n")
		fl.Flush()
	})))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	// Set by hand, so the transport neither adds its own nor decodes for us.
	req.Header.Set("Accept-Encoding", "gzip, br")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("SSE Content-Encoding = %q, want none", ce)
	}
	if v := resp.Header.Get("Vary"); v != "" {
		t.Errorf("SSE Vary = %q", v)
	}

	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	read := func(want string) {
		t.Helper()
		select {
		case l := <-lines:
			if l != want {
				t.Fatalf("line = %q, want %q", l, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("waiting for %q: the stream is buffered", want)
		}
	}
	read("event: hello")
	read("data: 1")
	close(gotFirst)
	read("")
	read("event: hello")
	read("data: 2")
	<-done
}

// TestGzipFlushStreamsCompressed: a compressible route that flushes mid-body
// (a long HTML render) still gets its first part to the client while the
// handler is running, as a decodable gzip prefix.
func TestGzipFlushStreamsCompressed(t *testing.T) {
	first := bigHTML
	gotFirst := make(chan struct{})
	srv := httptest.NewServer(Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-gotFirst:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			return
		}
		_, _ = io.WriteString(w, "<p>tail</p>")
	})))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", resp.Header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, len(first))
	readDone := make(chan error, 1)
	go func() { _, err := io.ReadFull(zr, head); readDone <- err }()
	select {
	case err := <-readDone:
		if err != nil || string(head) != first {
			t.Fatalf("flushed prefix: err %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the flushed part never arrived")
	}
	close(gotFirst)
	rest, err := io.ReadAll(zr)
	if err != nil || string(rest) != "<p>tail</p>" {
		t.Fatalf("tail = %q, err %v", rest, err)
	}
}

// TestGzipUpgradeHijacks: the Glass and Grafana Live websockets hijack the
// connection; an Upgrade request goes through unwrapped.
func TestGzipUpgradeHijacks(t *testing.T) {
	srv := httptest.NewServer(Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
}
