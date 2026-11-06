package plugin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// testHandler builds an App pointed at a fake upstream (fixed managed-SA token +
// in-cluster URL) and returns its route mux, so we can assert the reverse-proxy's
// header injection and routing in isolation — no Grafana, no real sidecar, and
// without the SDK httpadapter (which needs a CallResourceResponseSender).
func testHandler(a *App) http.Handler {
	mux := http.NewServeMux()
	a.registerRoutes(mux)
	return mux
}

func newTestApp(upstreamURL string) *App {
	return &App{
		mcpUpstreamURL:      upstreamURL,
		grafanaInClusterURL: "http://gardenio-grafana:3000/grafana",
		saToken:             "test-managed-sa-token",
	}
}

func TestMCPProxy_InjectsInClusterURLAndToken(t *testing.T) {
	var gotURL, gotKey, gotPath string
	var gotOrg, gotAuth, gotID, gotSAToken, gotAccessToken string
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.Header.Get("X-Grafana-URL")
		gotKey = r.Header.Get("X-Grafana-API-Key")
		gotOrg = r.Header.Get("X-Grafana-Org-Id")
		gotAuth = r.Header.Get("Authorization")
		gotID = r.Header.Get("X-Grafana-Id")
		gotSAToken = r.Header.Get("X-Grafana-Service-Account-Token")
		gotAccessToken = r.Header.Get("X-Access-Token")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer upstream.Close()

	app := newTestApp(upstream.URL)

	// Every client-supplied identity header MUST be stripped/overwritten so the
	// browser can never act as a different identity, switch org, repoint the
	// target, or smuggle a bearer. X-Grafana-Service-Account-Token is the
	// load-bearing case: mcp-grafana 0.17.0 PREFERS it over X-Grafana-API-Key, so
	// a leaked enumeration would let an authenticated chat user override the
	// managed-SA bearer (confused-deputy). The prefix strip must catch it.
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/grafana", strings.NewReader(body))
	req.Header.Set("X-Grafana-API-Key", "attacker-supplied")
	req.Header.Set("X-Grafana-Service-Account-Token", "attacker-sa-token")
	req.Header.Set("X-Grafana-URL", "http://evil.example.com")
	req.Header.Set("X-Grafana-Org-Id", "99")
	req.Header.Set("Authorization", "Basic YWRtaW46YWRtaW4=")
	req.Header.Set("X-Grafana-Id", "forged-onbehalfof")
	req.Header.Set("X-Access-Token", "forged-access-token")
	rw := httptest.NewRecorder()
	testHandler(app).ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 from proxy, got %d (body %q)", rw.Code, rw.Body.String())
	}
	if gotURL != "http://gardenio-grafana:3000/grafana" {
		t.Errorf("upstream should receive the in-cluster X-Grafana-URL, got %q", gotURL)
	}
	if gotKey != "test-managed-sa-token" {
		t.Errorf("upstream should receive the managed-SA token (client copy stripped), got %q", gotKey)
	}
	if gotSAToken != "" {
		t.Errorf("client X-Grafana-Service-Account-Token MUST be stripped (mcp-grafana prefers it over X-Grafana-API-Key — confused-deputy), got %q", gotSAToken)
	}
	if gotOrg != "" {
		t.Errorf("client X-Grafana-Org-Id must be stripped, got %q", gotOrg)
	}
	if gotAuth != "" {
		t.Errorf("client Authorization must be stripped (no basic-auth smuggling), got %q", gotAuth)
	}
	if gotID != "" {
		t.Errorf("client X-Grafana-Id (on-behalf-of) must be stripped, got %q", gotID)
	}
	if gotAccessToken != "" {
		t.Errorf("client X-Access-Token must be stripped, got %q", gotAccessToken)
	}
	if gotPath != "/mcp" {
		t.Errorf("upstream path should be the sidecar /mcp endpoint, got %q", gotPath)
	}
	if gotBody != body {
		t.Errorf("request body should pass through unchanged, got %q", gotBody)
	}
}

func TestMCPProxy_BadGatewayWhenUpstreamUnset(t *testing.T) {
	app := &App{} // no mcpUpstreamURL

	req := httptest.NewRequest(http.MethodPost, "/mcp/grafana", strings.NewReader("{}"))
	rw := httptest.NewRecorder()
	testHandler(app).ServeHTTP(rw, req)

	if rw.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when mcpUpstreamURL unset, got %d", rw.Code)
	}
}

// TestMCPProxy_LiveIntegration drives the REAL reverse-proxy against a live
// mcp-grafana sidecar, end to end: initialize -> tools/list -> update_dashboard
// CREATE. It is gated on the GARDENIO_MCP_INT_UPSTREAM env var so it never runs
// in unit-test CI (which has no sidecar); set UPSTREAM + GRAFANA_URL + TOKEN and
// run with -run TestMCPProxy_LiveIntegration to exercise the full path against a
// port-forwarded write sidecar. Proves the backend's header injection +
// streaming wiring work against a real mcp-grafana that reaches Grafana.
func TestMCPProxy_LiveIntegration(t *testing.T) {
	upstream := os.Getenv("GARDENIO_MCP_INT_UPSTREAM")
	if upstream == "" {
		t.Skip("set GARDENIO_MCP_INT_UPSTREAM (+ GARDENIO_MCP_INT_GRAFANA_URL, GARDENIO_MCP_INT_TOKEN) to run the live integration test")
	}
	app := &App{
		mcpUpstreamURL:      upstream,
		grafanaInClusterURL: os.Getenv("GARDENIO_MCP_INT_GRAFANA_URL"),
		saToken:             os.Getenv("GARDENIO_MCP_INT_TOKEN"),
	}
	srv := httptest.NewServer(testHandler(app))
	defer srv.Close()

	post := func(sid, body string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/grafana", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, string(b)
	}

	resp, body := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"int","version":"1"}}}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	t.Logf("initialize -> HTTP %d session=%q body=%.160s", resp.StatusCode, sid, body)
	if resp.StatusCode != http.StatusOK || sid == "" {
		t.Fatalf("initialize through proxy failed: HTTP %d", resp.StatusCode)
	}
	post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	_, lst := post(sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.Contains(lst, `"update_dashboard"`) {
		t.Errorf("write-capable proxy must advertise update_dashboard; got:\n%.400s", lst)
	}
	if strings.Contains(lst, `"alerting_manage_rules"`) {
		t.Errorf("curated envelope must NOT advertise alerting tools; got:\n%.400s", lst)
	}

	_, cr := post(sid, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"update_dashboard","arguments":{"dashboard":{"title":"backend-int-probe-DELETE","uid":"backend-int-del","panels":[],"schemaVersion":39},"overwrite":false}}}`)
	t.Logf("update_dashboard CREATE via backend proxy -> %.300s", cr)
	// The MCP tool result embeds the Grafana save response as ESCAPED JSON inside
	// the text content (e.g. {\"status\":\"success\"}), so match the success
	// signal robustly without depending on quote-escaping.
	if !strings.Contains(cr, "success") || !strings.Contains(cr, "backend-int-del") {
		t.Errorf("dashboard CREATE through the backend proxy did not succeed; got:\n%.400s", cr)
	}
}

func TestMCPProxy_TrailingSlashRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	app := newTestApp(upstream.URL)

	// The streamable-HTTP client may append a trailing slash; both must route.
	req := httptest.NewRequest(http.MethodPost, "/mcp/grafana/", strings.NewReader("{}"))
	rw := httptest.NewRecorder()
	testHandler(app).ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("trailing-slash route should proxy, got %d", rw.Code)
	}
}
