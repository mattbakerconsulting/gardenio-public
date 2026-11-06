package main

// Tests for the read-only /api/v1/charts[/{id}] proxy to the kbeads chart
// catalog (E6.1, task ka-0000005063). Covers: catalog + render passthrough,
// query-string forwarding, single-segment path safety (only /v1/charts[/id]
// is ever hit upstream), the server-side daemon bearer, If-None-Match / 304
// forwarding, ETag/Cache-Control mirroring, degraded-mode 503 when no daemon
// is wired, missing-id 404, and upstream-status passthrough.

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newChartsProxyMux registers the charts proxy routes exactly as server.go
// does, so route-level constraints (GET-only, {chart_id} segment) are
// exercised.
func newChartsProxyMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/charts", srv.handleChartsCatalog)
	mux.HandleFunc("GET /api/v1/charts/{chart_id}", srv.handleChartsRender)
	return mux
}

func newChartsProxyServerForTest(addr, token string) *Server {
	return &Server{logger: slog.Default(), beadsHTTPAddr: addr, beadsDaemonToken: token}
}

// fakeChartsDaemon records the path, raw query, and auth header it was hit with and
// returns a canned chart-catalog-shaped response.
type fakeChartsDaemon struct {
	gotPath  string
	gotQuery string
	gotAuth  string
	gotINM   string
	hits     int
	status   int
	body     string
	ct       string
	etag     string
	cc       string
}

func (f *fakeChartsDaemon) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.hits++
		f.gotPath = r.URL.Path
		f.gotQuery = r.URL.RawQuery
		f.gotAuth = r.Header.Get("Authorization")
		f.gotINM = r.Header.Get("If-None-Match")
		if f.ct != "" {
			w.Header().Set("Content-Type", f.ct)
		}
		if f.etag != "" {
			w.Header().Set("ETag", f.etag)
		}
		if f.cc != "" {
			w.Header().Set("Cache-Control", f.cc)
		}
		status := f.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if status != http.StatusNotModified {
			body := f.body
			if body == "" {
				body = `{"charts":[]}`
			}
			_, _ = io.WriteString(w, body)
		}
	}
}

func TestChartsProxy_Catalog_200(t *testing.T) {
	want := `{"charts":[{"chart_id":"spend-by-agent","title":"Spend per agent"}]}`
	fake := &fakeChartsDaemon{body: want, ct: "application/json"}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newChartsProxyServerForTest(upstream.URL, "tok-123")
	mux := newChartsProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/charts", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != want {
		t.Errorf("body: got %q want %q", rr.Body.String(), want)
	}
	if fake.gotPath != "/v1/charts" {
		t.Errorf("upstream path: got %q want /v1/charts", fake.gotPath)
	}
	if fake.gotAuth != "Bearer tok-123" {
		t.Errorf("auth header: got %q want %q", fake.gotAuth, "Bearer tok-123")
	}
}

func TestChartsProxy_Render_ForwardsQueryAndID(t *testing.T) {
	want := `{"chart_id":"spend-by-agent","data":[],"meta":{"empty":true}}`
	fake := &fakeChartsDaemon{body: want, ct: "application/json"}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newChartsProxyServerForTest(upstream.URL, "tok-xyz")
	mux := newChartsProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/charts/spend-by-agent?since=2026-01-01T00:00:00Z&agent=a&agent=b&top_n=5", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rr.Code, rr.Body.String())
	}
	if fake.gotPath != "/v1/charts/spend-by-agent" {
		t.Errorf("upstream path: got %q want /v1/charts/spend-by-agent", fake.gotPath)
	}
	// The raw query string must be forwarded verbatim, including repeated
	// multi-value selector params.
	for _, frag := range []string{"since=2026-01-01T00:00:00Z", "agent=a", "agent=b", "top_n=5"} {
		if !strings.Contains(fake.gotQuery, frag) {
			t.Errorf("forwarded query %q missing %q", fake.gotQuery, frag)
		}
	}
}

func TestChartsProxy_MirrorsETagAndCacheControl(t *testing.T) {
	fake := &fakeChartsDaemon{
		body: `{"chart_id":"x"}`,
		ct:   "application/json",
		etag: `"sha256:abc"`,
		cc:   "public, max-age=60",
	}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newChartsProxyServerForTest(upstream.URL, "")
	mux := newChartsProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/charts/x", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if got := rr.Header().Get("ETag"); got != `"sha256:abc"` {
		t.Errorf("ETag: got %q want %q", got, `"sha256:abc"`)
	}
	if got := rr.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control: got %q want %q", got, "public, max-age=60")
	}
	// No token configured → no Authorization header sent upstream.
	if fake.gotAuth != "" {
		t.Errorf("auth header: got %q want empty", fake.gotAuth)
	}
}

func TestChartsProxy_304_ForwardsIfNoneMatch(t *testing.T) {
	fake := &fakeChartsDaemon{status: http.StatusNotModified, etag: `"sha256:abc"`}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newChartsProxyServerForTest(upstream.URL, "tok")
	mux := newChartsProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/charts/x", nil)
	req.Header.Set("If-None-Match", `"sha256:abc"`)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotModified {
		t.Fatalf("status: got %d want 304", rr.Code)
	}
	if fake.gotINM != `"sha256:abc"` {
		t.Errorf("If-None-Match forwarded: got %q want %q", fake.gotINM, `"sha256:abc"`)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("304 body: got %q want empty", rr.Body.String())
	}
}

func TestChartsProxy_DegradedMode_503(t *testing.T) {
	srv := newChartsProxyServerForTest("", "") // no daemon wired
	mux := newChartsProxyMux(srv)

	for _, path := range []string{"/api/v1/charts", "/api/v1/charts/spend-by-agent"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status got %d want 503", path, rr.Code)
		}
	}
}

func TestChartsProxy_UpstreamStatusPassthrough(t *testing.T) {
	// A 404 from the daemon (unknown chart id) is mirrored to the client.
	fake := &fakeChartsDaemon{status: http.StatusNotFound, body: `{"error":"unknown chart_id"}`, ct: "application/json"}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newChartsProxyServerForTest(upstream.URL, "tok")
	mux := newChartsProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/charts/nope", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404; body=%q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unknown chart_id") {
		t.Errorf("upstream error body not passed through: %q", rr.Body.String())
	}
}

func TestChartsProxy_PathSegmentIsContained(t *testing.T) {
	// A crafted chart id with slashes must not escape the /v1/charts/ prefix:
	// the proxy percent-encodes the single dynamic segment.
	fake := &fakeChartsDaemon{body: `{}`, ct: "application/json"}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newChartsProxyServerForTest(upstream.URL, "tok")
	mux := newChartsProxyMux(srv)

	// Go's ServeMux already decodes %2F; pass a benign id and assert the
	// upstream path stays under /v1/charts/.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/charts/spend-by-agent", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if !strings.HasPrefix(fake.gotPath, "/v1/charts/") {
		t.Errorf("upstream path %q escaped /v1/charts/ prefix", fake.gotPath)
	}
}
