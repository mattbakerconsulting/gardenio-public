package main

// Tests for the read-only /api/v1/query_range vmsingle proxy (ka-0000002747).
// Covers: happy-path range data passthrough, query-string forwarding,
// allowlist-by-construction (only /api/v1/query_range is ever hit upstream),
// missing-query 400, degraded-mode 503 when no upstream is wired, upstream
// error 502, and upstream-status/body passthrough for a bad query.

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newMetricsProxyMux registers the metrics proxy route exactly as server.go
// does, so route-level constraints (GET-only, fixed path) are exercised.
func newMetricsProxyMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/query_range", srv.handleMetricsQueryRange)
	return mux
}

func newMetricsProxyServerForTest(upstream string) *Server {
	srv := &Server{logger: slog.Default()}
	if upstream != "" {
		srv.SetMetricsProxy(upstream)
	}
	return srv
}

// fakeVMSingle records the path + raw query it was hit with and returns a
// canned vmsingle-shaped range response.
type fakeVMSingle struct {
	gotPath  string
	gotQuery string
	hits     int
	status   int
	body     string
	ct       string
}

func (f *fakeVMSingle) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.hits++
		f.gotPath = r.URL.Path
		f.gotQuery = r.URL.RawQuery
		if f.ct != "" {
			w.Header().Set("Content-Type", f.ct)
		}
		status := f.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		body := f.body
		if body == "" {
			body = `{"status":"success","data":{"resultType":"matrix","result":[]}}`
		}
		_, _ = io.WriteString(w, body)
	}
}

func TestMetricsProxy_200_RangeData(t *testing.T) {
	want := `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"__name__":"up"},"values":[[1700000000,"1"]]}]}}`
	fake := &fakeVMSingle{body: want, ct: "application/json"}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newMetricsProxyServerForTest(upstream.URL)
	mux := newMetricsProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?query=up&start=1700000000&end=1700003600&step=60", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != want {
		t.Errorf("body passthrough: got %q want %q", got, want)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: got %q want application/json", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != metricsProxyCacheControl {
		t.Errorf("Cache-Control: got %q want %q", got, metricsProxyCacheControl)
	}
}

// The proxy must only ever hit /api/v1/query_range upstream, and must forward
// the client's query string verbatim — that is the allowlist by construction.
func TestMetricsProxy_AllowlistPathAndQueryForwarding(t *testing.T) {
	fake := &fakeVMSingle{ct: "application/json"}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newMetricsProxyServerForTest(upstream.URL)
	mux := newMetricsProxyMux(srv)

	rawQuery := "query=rate%28beads_events_total%5B5m%5D%29&start=1700000000&end=1700003600&step=60"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?"+rawQuery, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rr.Code)
	}
	if fake.hits != 1 {
		t.Fatalf("upstream hits: got %d want 1", fake.hits)
	}
	if fake.gotPath != "/api/v1/query_range" {
		t.Errorf("upstream path: got %q want /api/v1/query_range", fake.gotPath)
	}
	if fake.gotQuery != rawQuery {
		t.Errorf("forwarded query: got %q want %q", fake.gotQuery, rawQuery)
	}
}

// A non-query_range path is not registered, so the mux 404s without ever
// reaching the handler — proving the proxy can't be pivoted at another
// upstream endpoint (e.g. the write or delete API).
func TestMetricsProxy_OtherPathsNotRouted(t *testing.T) {
	fake := &fakeVMSingle{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newMetricsProxyServerForTest(upstream.URL)
	mux := newMetricsProxyMux(srv)

	for _, path := range []string{
		"/api/v1/write",
		"/api/v1/query",
		"/api/v1/admin/tsdb/delete_series",
		"/api/v1/labels",
	} {
		req := httptest.NewRequest(http.MethodGet, path+"?query=up", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: got %d want 404 (not routed)", path, rr.Code)
		}
	}
	if fake.hits != 0 {
		t.Errorf("upstream should never be hit for non-allowlisted paths; got %d hits", fake.hits)
	}
}

// POST is not registered either — read-only by route construction.
func TestMetricsProxy_POSTNotRouted(t *testing.T) {
	fake := &fakeVMSingle{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newMetricsProxyServerForTest(upstream.URL)
	mux := newMetricsProxyMux(srv)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/query_range?query=up", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed && rr.Code != http.StatusNotFound {
		t.Errorf("POST: got %d want 405/404 (read-only route)", rr.Code)
	}
	if fake.hits != 0 {
		t.Errorf("upstream hit on POST; got %d", fake.hits)
	}
}

func TestMetricsProxy_MissingQuery400(t *testing.T) {
	fake := &fakeVMSingle{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newMetricsProxyServerForTest(upstream.URL)
	mux := newMetricsProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?start=1&end=2", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing query: got %d want 400", rr.Code)
	}
	if fake.hits != 0 {
		t.Errorf("upstream hit despite missing query; got %d", fake.hits)
	}
}

func TestMetricsProxy_DegradedNoUpstream503(t *testing.T) {
	srv := newMetricsProxyServerForTest("") // no upstream wired
	mux := newMetricsProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?query=up", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("no upstream: got %d want 503", rr.Code)
	}
}

func TestMetricsProxy_UpstreamUnreachable502(t *testing.T) {
	// Point at a closed server so Do() fails at the transport layer.
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := upstream.URL
	upstream.Close()

	srv := newMetricsProxyServerForTest(closed)
	mux := newMetricsProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?query=up", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("unreachable upstream: got %d want 502", rr.Code)
	}
}

// vmsingle returns its own 4xx + JSON error body for a bad query; the proxy
// passes the status and body straight through so the panel sees the upstream
// message.
func TestMetricsProxy_BadQueryStatusPassthrough(t *testing.T) {
	errBody := `{"status":"error","errorType":"422","error":"unparsed tail"}`
	fake := &fakeVMSingle{status: http.StatusUnprocessableEntity, body: errBody, ct: "application/json"}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newMetricsProxyServerForTest(upstream.URL)
	mux := newMetricsProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?query=%28%28", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status passthrough: got %d want 422", rr.Code)
	}
	if got := rr.Body.String(); got != errBody {
		t.Errorf("error body passthrough: got %q want %q", got, errBody)
	}
	// A non-200 upstream must not be cached.
	if got := rr.Header().Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control on error: got %q want empty", got)
	}
}

func TestMetricsQueryURLFromEnv(t *testing.T) {
	// Default when unset.
	got := metricsQueryURLFromEnv(func(string) string { return "" })
	if got != defaultVMSingleQueryURL {
		t.Errorf("default: got %q want %q", got, defaultVMSingleQueryURL)
	}
	// Override.
	want := "http://vmsingle.example:9090"
	got = metricsQueryURLFromEnv(func(k string) string {
		if k == "VMSINGLE_QUERY_URL" {
			return want
		}
		return ""
	})
	if got != want {
		t.Errorf("override: got %q want %q", got, want)
	}
	// Trailing slash is trimmed by SetMetricsProxy, not the env helper;
	// verify SetMetricsProxy normalizes it.
	srv := &Server{logger: slog.Default()}
	srv.SetMetricsProxy("http://vmsingle:8428/")
	if strings.HasSuffix(srv.metricsUpstream, "/") {
		t.Errorf("SetMetricsProxy left trailing slash: %q", srv.metricsUpstream)
	}
}
