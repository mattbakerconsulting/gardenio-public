package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// withGrafanaTestDaemon points the global `daemon` at an httptest server whose
// GET /v1/beads/<id> returns the supplied bead JSON (or 404 for unknown ids).
func withGrafanaTestDaemon(t *testing.T, beads map[string]map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
		b, ok := beads[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(b)
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

func TestResolveDashboardUID_BeadPaths(t *testing.T) {
	withGrafanaTestDaemon(t, map[string]map[string]any{
		"gd-Dash1": {"id": "gd-Dash1", "type": "dashboard", "fields": map[string]string{"dashboard_uid": "abc-uid"}},
		"gd-NoUID": {"id": "gd-NoUID", "type": "dashboard", "fields": map[string]string{}},
		"gd-Task1": {"id": "gd-Task1", "type": "task", "fields": map[string]string{}},
	})

	// Happy path: bead → dashboard_uid, and the bead id is returned for --bead.
	uid, bead, err := resolveDashboardUID(context.Background(), "gd-Dash1", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uid != "abc-uid" || bead != "gd-Dash1" {
		t.Fatalf("got uid=%q bead=%q", uid, bead)
	}

	// Dashboard bead with no uid yet → error.
	if _, _, err := resolveDashboardUID(context.Background(), "gd-NoUID", false); err == nil {
		t.Error("dashboard with no dashboard_uid should error")
	}

	// Non-dashboard bead → hard error (don't silently treat as a uid).
	if _, _, err := resolveDashboardUID(context.Background(), "gd-Task1", false); err == nil {
		t.Error("non-dashboard bead should error")
	}

	// Unknown id (404) → treated as a raw uid.
	uid, bead, err = resolveDashboardUID(context.Background(), "not-a-bead", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uid != "not-a-bead" || bead != "" {
		t.Fatalf("unknown id should pass through as uid: got uid=%q bead=%q", uid, bead)
	}
}

func TestGrafanaRenderBaseURL(t *testing.T) {
	// Save & clear the env so the test controls precedence.
	t.Setenv("GRAFANA_URL", "")
	t.Setenv("GRAFANA_BASE_URL", "")

	if got := grafanaRenderBaseURL(""); got != "" {
		t.Fatalf("no source set: want empty, got %q", got)
	}

	// Flag wins over everything, and trailing slash is trimmed.
	t.Setenv("GRAFANA_URL", "http://in-cluster:3000")
	t.Setenv("GRAFANA_BASE_URL", "https://public/grafana")
	if got := grafanaRenderBaseURL("https://flag/grafana/"); got != "https://flag/grafana" {
		t.Fatalf("flag should win+trim: got %q", got)
	}

	// GRAFANA_URL wins over GRAFANA_BASE_URL when no flag.
	if got := grafanaRenderBaseURL(""); got != "http://in-cluster:3000" {
		t.Fatalf("GRAFANA_URL should win: got %q", got)
	}

	// Falls back to GRAFANA_BASE_URL.
	t.Setenv("GRAFANA_URL", "")
	if got := grafanaRenderBaseURL(""); got != "https://public/grafana" {
		t.Fatalf("GRAFANA_BASE_URL fallback: got %q", got)
	}
}

func TestBuildRenderURL_WholeDashboard(t *testing.T) {
	got, err := buildRenderURL("http://g:3000", "abc123", renderOptions{
		from: "now-6h", to: "now", width: 1200, height: 600, theme: "dark", tz: "UTC",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "http://g:3000/render/d/abc123?") {
		t.Fatalf("wrong base/path: %q", got)
	}
	for _, want := range []string{"from=now-6h", "to=now", "width=1200", "height=600", "theme=dark", "tz=UTC"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "panelId") {
		t.Errorf("whole-dashboard render must not set panelId: %q", got)
	}
}

func TestBuildRenderURL_SinglePanelAndVars(t *testing.T) {
	got, err := buildRenderURL("http://g:3000/", "uid9", renderOptions{
		panel: "7",
		vars:  []string{"datasource=vmsingle", "ns=gardenio"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "http://g:3000/render/d-solo/uid9?") {
		t.Fatalf("panel render should use d-solo: %q", got)
	}
	for _, want := range []string{"panelId=7", "var-datasource=vmsingle", "var-ns=gardenio"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestBuildRenderURL_Rejects(t *testing.T) {
	if _, err := buildRenderURL("http://g", "", renderOptions{}); err == nil {
		t.Error("empty uid should error")
	}
	if _, err := buildRenderURL("http://g", "a/b", renderOptions{}); err == nil {
		t.Error("uid with path separator should error")
	}
	if _, err := buildRenderURL("http://g", "uid", renderOptions{vars: []string{"novalue"}}); err == nil {
		t.Error("var without = should error")
	}
	if _, err := buildRenderURL("http://g", "uid", renderOptions{vars: []string{"=v"}}); err == nil {
		t.Error("var with empty key should error")
	}
}

func TestResolveDashboardUID_ForceUID(t *testing.T) {
	// forceUID short-circuits the bead lookup entirely.
	uid, bead, err := resolveDashboardUID(context.Background(), "  rawuid  ", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uid != "rawuid" || bead != "" {
		t.Fatalf("force-uid: got uid=%q bead=%q", uid, bead)
	}
}

func TestResolveDashboardUID_Empty(t *testing.T) {
	if _, _, err := resolveDashboardUID(context.Background(), "   ", false); err == nil {
		t.Error("empty arg should error")
	}
}

func TestDefaultRenderOutPath(t *testing.T) {
	whole := defaultRenderOutPath("my uid", "")
	if filepath.Dir(whole) != os.TempDir() {
		t.Errorf("should live in TempDir: %q", whole)
	}
	if !strings.HasSuffix(whole, ".png") {
		t.Errorf("should be a .png: %q", whole)
	}
	if strings.Contains(filepath.Base(whole), " ") {
		t.Errorf("basename should be sanitized (no spaces): %q", whole)
	}
	panel := defaultRenderOutPath("uid", "4")
	if !strings.Contains(panel, "panel4") {
		t.Errorf("panel render path should encode panel id: %q", panel)
	}
	if panel == whole {
		t.Error("panel and whole-dashboard paths must differ")
	}
}

func TestApplyGrafanaAuth_Token(t *testing.T) {
	t.Setenv("GRAFANA_TOKEN", "svc-token-xyz")
	req, _ := http.NewRequest(http.MethodGet, "http://g/render/d/u", nil)
	applyGrafanaAuth(req)
	if got := req.Header.Get("Authorization"); got != "Bearer svc-token-xyz" {
		t.Fatalf("token auth: got %q", got)
	}
	if req.Header.Get("Cookie") != "" {
		t.Error("token path must not set Cookie")
	}
}

// TestApplyGrafanaAuth_AuthProxyHeader covers the in-cluster path (bug
// ka-0000005842 §2): with no GRAFANA_TOKEN but GRAFANA_AUTH_PROXY_HEADER +
// an operator identity set, the render request must carry the auth-proxy
// identity header so Grafana (anonymous disabled) authenticates it instead of
// 302-looping to /login.
func TestApplyGrafanaAuth_AuthProxyHeader(t *testing.T) {
	t.Setenv("GRAFANA_TOKEN", "")
	t.Setenv("GRAFANA_AUTH_PROXY_HEADER", "X-Auth-Request-Email")
	t.Setenv("BEADS_OPERATED_BY", "oidc:matthew.baker@acme.example")

	req, _ := http.NewRequest(http.MethodGet, "http://g/render/d/u", nil)
	applyGrafanaAuth(req)

	if got := req.Header.Get("X-Auth-Request-Email"); got != "matthew.baker@acme.example" {
		t.Fatalf("auth-proxy header: got %q, want bare email", got)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("auth-proxy path must not set Authorization")
	}
}

// TestApplyGrafanaAuth_TokenBeatsHeader confirms GRAFANA_TOKEN still wins over
// the auth-proxy header (public AVA path uses the SA token).
func TestApplyGrafanaAuth_TokenBeatsHeader(t *testing.T) {
	t.Setenv("GRAFANA_TOKEN", "svc-token")
	t.Setenv("GRAFANA_AUTH_PROXY_HEADER", "X-Auth-Request-Email")
	t.Setenv("BEADS_OPERATED_BY", "oidc:matthew.baker@acme.example")

	req, _ := http.NewRequest(http.MethodGet, "http://g/render/d/u", nil)
	applyGrafanaAuth(req)

	if got := req.Header.Get("Authorization"); got != "Bearer svc-token" {
		t.Fatalf("token should win: got %q", got)
	}
	if req.Header.Get("X-Auth-Request-Email") != "" {
		t.Error("token path must not set the auth-proxy header")
	}
}

// TestGrafanaAuthProxyIdentity_UserOverride confirms GRAFANA_USER wins and is
// used verbatim (no scheme stripping).
func TestGrafanaAuthProxyIdentity_UserOverride(t *testing.T) {
	t.Setenv("GRAFANA_USER", "svc-bot@acme.example")
	t.Setenv("BEADS_OPERATED_BY", "oidc:someone.else@acme.example")
	if got := grafanaAuthProxyIdentity(); got != "svc-bot@acme.example" {
		t.Fatalf("GRAFANA_USER override: got %q", got)
	}
}

func TestFetchRender_OK(t *testing.T) {
	t.Setenv("GRAFANA_TOKEN", "")
	png := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "image/png" {
			t.Errorf("missing Accept header: %v", r.Header)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer srv.Close()

	got, err := fetchRender(context.Background(), srv.URL+"/render/d/u", 1000, 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(png) {
		t.Fatalf("body mismatch: got %d bytes", len(got))
	}
}

func TestFetchRender_404IsActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("plugin not found"))
	}))
	defer srv.Close()

	_, err := fetchRender(context.Background(), srv.URL+"/render/d/u", 1000, 500)
	if err == nil {
		t.Fatal("404 should error")
	}
	if !strings.Contains(err.Error(), "image-renderer") {
		t.Errorf("404 error should hint at the renderer sidecar: %v", err)
	}
}

func TestFetchRender_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	if _, err := fetchRender(context.Background(), srv.URL+"/render/d/u", 1000, 500); err == nil {
		t.Fatal("5xx should error")
	}
}

// realPNG builds a minimal but structurally valid PNG header (signature + IHDR
// with the given dimensions) padded out to size bytes — enough for the
// placeholder detector, which only reads the IHDR and the body length.
func realPNG(w, h, size int) []byte {
	b := make([]byte, 24)
	copy(b, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	copy(b[12:16], []byte("IHDR"))
	binary.BigEndian.PutUint32(b[16:20], uint32(w))
	binary.BigEndian.PutUint32(b[20:24], uint32(h))
	if size > len(b) {
		b = append(b, make([]byte, size-len(b))...)
	}
	return b
}

// TestFetchRender_MissingRendererPlaceholder covers the headline bug: Grafana
// returns HTTP 200 with a tiny fixed-size "No image renderer available"
// placeholder PNG instead of a 404. fetchRender must fail loudly rather than
// write the broken image and report success.
func TestFetchRender_MissingRendererPlaceholder(t *testing.T) {
	t.Setenv("GRAFANA_TOKEN", "")
	// ~9KB 360x180 card, requested 1400x1100 — ignores the requested geometry.
	placeholder := realPNG(360, 180, 9095)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(placeholder)
	}))
	defer srv.Close()

	_, err := fetchRender(context.Background(), srv.URL+"/render/d/u", 1400, 1100)
	if err == nil {
		t.Fatal("placeholder render should error, not silently succeed")
	}
	if !strings.Contains(err.Error(), "placeholder") || !strings.Contains(err.Error(), "image-renderer") {
		t.Errorf("error should name the placeholder and the missing renderer: %v", err)
	}
}

// TestFetchRender_RealRenderHonoursGeometry confirms a genuine full-size render
// (dimensions matching the request, large body) is NOT flagged.
func TestFetchRender_RealRenderHonoursGeometry(t *testing.T) {
	t.Setenv("GRAFANA_TOKEN", "")
	real := realPNG(1400, 1100, 250_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(real)
	}))
	defer srv.Close()

	got, err := fetchRender(context.Background(), srv.URL+"/render/d/u", 1400, 1100)
	if err != nil {
		t.Fatalf("real render should succeed: %v", err)
	}
	if len(got) != len(real) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(got), len(real))
	}
}

// TestFetchRender_NonPNGBody covers the renderer-missing path on builds that
// return HTML/JSON with a 200 instead of an image.
func TestFetchRender_NonPNGBody(t *testing.T) {
	t.Setenv("GRAFANA_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>renderer unavailable</body></html>"))
	}))
	defer srv.Close()

	_, err := fetchRender(context.Background(), srv.URL+"/render/d/u", 1400, 1100)
	if err == nil {
		t.Fatal("non-PNG 200 body should error")
	}
	if !strings.Contains(err.Error(), "not a PNG") {
		t.Errorf("error should flag non-PNG body: %v", err)
	}
}

// TestMissingRendererReason_SmallIntentionalRender guards against false
// positives: a small PNG that actually honours a small request must pass
// (ka-0000006776).
func TestMissingRendererReason_SmallIntentionalRender(t *testing.T) {
	// 300x200 PNG for a 300x200 request — dimensions match, not a placeholder.
	if reason := missingRendererReason(realPNG(300, 200, 5000), 300, 200); reason != "" {
		t.Errorf("small honoured render falsely flagged: %s", reason)
	}
}

func TestUploadRenderedPNG(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "viewer-bucket")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	t.Setenv("COOP_S3_REGION", "us-west-2")

	fu := &fakeUploader{}
	withFakeUploader(t, fu)

	data := []byte("\x89PNGfake")
	url, err := uploadRenderedPNG(context.Background(), "/tmp/grafana-uid.png", data, "gd-Dash1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fu.puts) != 1 {
		t.Fatalf("want 1 PutObject, got %d", len(fu.puts))
	}
	put := fu.puts[0]
	if put.Bucket != "viewer-bucket" {
		t.Errorf("bucket: got %q", put.Bucket)
	}
	if put.ContentType != "image/png" {
		t.Errorf("content-type: got %q", put.ContentType)
	}
	// The bead id is the scope segment, so the key is bucketed under it.
	if !strings.Contains(put.Key, "/images/gd-Dash1/") {
		t.Errorf("key should be scoped under the bead: %q", put.Key)
	}
	if !strings.Contains(url, "viewer-bucket") {
		t.Errorf("url should reference the bucket: %q", url)
	}
}

func TestUploadRenderedPNG_NoBucket(t *testing.T) {
	t.Setenv("COOP_S3_BUCKET", "")
	_, err := uploadRenderedPNG(context.Background(), "/tmp/x.png", []byte("x"), "")
	if err == nil || !strings.Contains(err.Error(), "COOP_S3_BUCKET") {
		t.Fatalf("want COOP_S3_BUCKET error, got %v", err)
	}
}

func resetGrafanaRenderFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		for k, v := range map[string]string{
			"uid": "false", "panel": "", "from": "", "to": "",
			"width": "1000", "height": "500", "theme": "", "tz": "",
			"out": "", "grafana-url": "", "upload": "false", "bead": "",
		} {
			_ = grafanaRenderCmd.Flags().Set(k, v)
		}
	})
}

// TestGrafanaRenderCmd_EndToEnd drives the whole RunE: a stub renderer serves a
// PNG, the command resolves a raw uid, GETs /render/d/<uid>, and writes the
// bytes to --out so an agent could Read them.
func TestGrafanaRenderCmd_EndToEnd(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nend-to-end")
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer srv.Close()

	resetGrafanaRenderFlags(t)
	out := filepath.Join(t.TempDir(), "dash.png")
	cmd := grafanaRenderCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("uid", "true")
	_ = cmd.Flags().Set("grafana-url", srv.URL)
	_ = cmd.Flags().Set("out", out)

	if err := cmd.RunE(cmd, []string{"my-uid"}); err != nil {
		t.Fatalf("RunE failed: %v", err)
	}
	if gotPath != "/render/d/my-uid" {
		t.Errorf("renderer got path %q", gotPath)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output PNG: %v", err)
	}
	if string(data) != string(png) {
		t.Errorf("output PNG mismatch: got %d bytes", len(data))
	}
}

// ── gb grafana put ─────────────────────────────────────────────────────────

// grafanaPutStub is a small stateful beads-daemon fake covering exactly the API
// surface `gb grafana put` drives via the DashboardStore: list-by-field (uid),
// create-bead, get-bead, add-dependency, add-label, and patch-fields. It records
// the interactions so tests can assert the dashboard bead + model doc + linkage
// + label are all written the way render --bead / PutModel expect.
type grafanaPutStub struct {
	mu      sync.Mutex
	beads   map[string]map[string]any // id → {type,title,kind,status,fields,labels}
	deps    []map[string]string       // {bead_id, depends_on_id, type}
	creates []map[string]any
	nextID  int
}

func newGrafanaPutStub() *grafanaPutStub {
	return &grafanaPutStub{beads: map[string]map[string]any{}}
}

func (s *grafanaPutStub) bead(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beads[id]
}

func (s *grafanaPutStub) labels(id string) []string {
	b := s.bead(id)
	if b == nil {
		return nil
	}
	out, _ := b["labels"].([]string)
	return out
}

func (s *grafanaPutStub) fields(id string) map[string]string {
	b := s.bead(id)
	if b == nil {
		return nil
	}
	f, _ := b["fields"].(map[string]string)
	return f
}

func (s *grafanaPutStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		// FindByUID: GET /v1/beads?type=dashboard&...&field=dashboard_uid=<uid>
		case r.Method == http.MethodGet && path == "/v1/beads":
			wantUID := ""
			for _, f := range r.URL.Query()["field"] {
				if k, v, ok := strings.Cut(f, "="); ok && k == "dashboard_uid" {
					wantUID = v
				}
			}
			s.mu.Lock()
			var matches []map[string]any
			for id, b := range s.beads {
				if b["type"] != "dashboard" {
					continue
				}
				f, _ := b["fields"].(map[string]string)
				if wantUID == "" || f["dashboard_uid"] == wantUID {
					matches = append(matches, map[string]any{
						"id": id, "type": b["type"], "title": b["title"],
						"status": b["status"], "fields": f,
					})
				}
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": matches, "total": len(matches)})

		case r.Method == http.MethodPost && path == "/v1/beads":
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			fields := map[string]string{}
			switch f := parsed["fields"].(type) {
			case string:
				if f != "" {
					_ = json.Unmarshal([]byte(f), &fields)
				}
			case map[string]any:
				for k, v := range f {
					if s, ok := v.(string); ok {
						fields[k] = s
					}
				}
			}
			s.mu.Lock()
			s.nextID++
			id := "gd-NEW" + itoa(s.nextID)
			parsed["id"] = id
			s.creates = append(s.creates, parsed)
			labels := []string{}
			if ls, ok := parsed["labels"].([]any); ok {
				for _, l := range ls {
					if str, ok := l.(string); ok {
						labels = append(labels, str)
					}
				}
			}
			s.beads[id] = map[string]any{
				"id": id, "title": parsed["title"], "type": parsed["type"],
				"kind": parsed["kind"], "status": "open", "fields": fields, "labels": labels,
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})

		case r.Method == http.MethodGet && strings.HasSuffix(path, "/dependents"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependents")
			s.mu.Lock()
			var deps []map[string]string
			for _, d := range s.deps {
				if d["depends_on_id"] == id {
					deps = append(deps, d)
				}
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": deps})

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/dependencies"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependencies")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]string
			_ = json.Unmarshal(body, &parsed)
			parsed["bead_id"] = id
			s.mu.Lock()
			s.deps = append(s.deps, parsed)
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/labels"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/labels")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]string
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			if b, ok := s.beads[id]; ok {
				ls, _ := b["labels"].([]string)
				b["labels"] = append(ls, parsed["label"])
			}
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPatch && strings.HasPrefix(path, "/v1/beads/"):
			id := strings.TrimPrefix(path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			if b, ok := s.beads[id]; ok {
				existing, _ := b["fields"].(map[string]string)
				if existing == nil {
					existing = map[string]string{}
				}
				if f, ok := parsed["fields"].(map[string]any); ok {
					for k, v := range f {
						if str, ok := v.(string); ok {
							existing[k] = str
						}
					}
				}
				b["fields"] = existing
			}
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/beads/"):
			id := strings.TrimPrefix(path, "/v1/beads/")
			s.mu.Lock()
			b, ok := s.beads[id]
			s.mu.Unlock()
			if !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": b["id"], "title": b["title"], "type": b["type"],
				"kind": b["kind"], "status": b["status"], "fields": b["fields"],
				"labels": b["labels"],
			})

		default:
			http.NotFound(w, r)
		}
	})
}

func setupGrafanaPutDaemon(t *testing.T, stub *grafanaPutStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

func writeTempJSON(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dash.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func resetGrafanaPutFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = grafanaPutCmd.Flags().Set("project", "")
		_ = grafanaPutCmd.Flags().Set("title", "")
	})
}

func runGrafanaPut(t *testing.T, project, title string, args ...string) error {
	t.Helper()
	resetGrafanaPutFlags(t)
	cmd := grafanaPutCmd
	cmd.SetContext(context.Background())
	if project != "" {
		_ = cmd.Flags().Set("project", project)
	}
	if title != "" {
		_ = cmd.Flags().Set("title", title)
	}
	return cmd.RunE(cmd, args)
}

// TestGrafanaPutCmd_CreatesDashboardAndModel covers the headline behavior: from
// a JSON file, put creates a type=dashboard bead (with grafana:dashboard +
// project label and dashboard_uid field), a type=doc model bead carrying the
// JSON in content, links the doc parent-child under the dashboard, and labels it
// grafana:dashboard-model — exactly what render --bead / PutModel consume.
func TestGrafanaPutCmd_CreatesDashboardAndModel(t *testing.T) {
	stub := newGrafanaPutStub()
	setupGrafanaPutDaemon(t, stub)

	model := `{"uid":"cost-overview","title":"Cost Overview","version":3,"panels":[]}`
	file := writeTempJSON(t, model)

	if err := runGrafanaPut(t, "gardenio", "", file); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	// Two beads created: the dashboard and the model doc.
	if len(stub.creates) != 2 {
		t.Fatalf("expected 2 creates (dashboard + model doc), got %d", len(stub.creates))
	}

	// Locate the dashboard bead and the doc bead by type.
	var dashID, docID string
	for id, b := range stub.beads {
		switch b["type"] {
		case "dashboard":
			dashID = id
		case "doc":
			docID = id
		}
	}
	if dashID == "" || docID == "" {
		t.Fatalf("missing dashboard (%q) or doc (%q) bead", dashID, docID)
	}

	// Dashboard bead: uid field, dashboard_source=beads, version, and labels.
	df := stub.fields(dashID)
	if df["dashboard_uid"] != "cost-overview" {
		t.Errorf("dashboard_uid = %q", df["dashboard_uid"])
	}
	if df["dashboard_source"] != "beads" {
		t.Errorf("dashboard_source = %q, want beads", df["dashboard_source"])
	}
	if df["dashboard_version"] != "3" {
		t.Errorf("dashboard_version = %q, want 3", df["dashboard_version"])
	}
	if !slices.Contains(stub.labels(dashID), "grafana:dashboard") {
		t.Errorf("dashboard missing grafana:dashboard label: %v", stub.labels(dashID))
	}
	if !slices.Contains(stub.labels(dashID), "project:gardenio") {
		t.Errorf("dashboard missing project label: %v", stub.labels(dashID))
	}
	if got := stub.bead(dashID)["title"]; got != "Cost Overview" {
		t.Errorf("dashboard title = %v, want Cost Overview", got)
	}

	// Model doc: carries the JSON in content and the model label.
	if got := stub.fields(docID)["content"]; got != model {
		t.Errorf("model doc content mismatch:\n got %q\nwant %q", got, model)
	}
	if !slices.Contains(stub.labels(docID), "grafana:dashboard-model") {
		t.Errorf("model doc missing grafana:dashboard-model label: %v", stub.labels(docID))
	}

	// Linkage: doc → dashboard parent-child (so render --bead's reverse-dep
	// walk finds the model under the dashboard).
	linked := false
	for _, d := range stub.deps {
		if d["bead_id"] == docID && d["depends_on_id"] == dashID && d["type"] == "parent-child" {
			linked = true
		}
	}
	if !linked {
		t.Errorf("model doc not linked parent-child under dashboard: %v", stub.deps)
	}
}

// TestGrafanaPutCmd_Idempotent confirms a second put for the same uid reuses the
// dashboard bead and replaces the model content in place — no duplicate beads.
func TestGrafanaPutCmd_Idempotent(t *testing.T) {
	stub := newGrafanaPutStub()
	setupGrafanaPutDaemon(t, stub)

	v1 := `{"uid":"d","title":"D","version":1}`
	v2 := `{"uid":"d","title":"D2","version":2}`

	if err := runGrafanaPut(t, "gardenio", "", writeTempJSON(t, v1)); err != nil {
		t.Fatalf("first put: %v", err)
	}
	createsAfterFirst := len(stub.creates)
	if createsAfterFirst != 2 {
		t.Fatalf("first put should create 2 beads, got %d", createsAfterFirst)
	}

	if err := runGrafanaPut(t, "gardenio", "", writeTempJSON(t, v2)); err != nil {
		t.Fatalf("second put: %v", err)
	}
	// No new beads on re-put: same dashboard, same model doc, content replaced.
	if len(stub.creates) != createsAfterFirst {
		t.Fatalf("re-put created %d new beads, want 0", len(stub.creates)-createsAfterFirst)
	}

	var docID string
	for id, b := range stub.beads {
		if b["type"] == "doc" {
			docID = id
		}
	}
	if got := stub.fields(docID)["content"]; got != v2 {
		t.Errorf("model not updated on re-put: got %q, want %q", got, v2)
	}
}

func TestGrafanaPutCmd_RejectsMissingUID(t *testing.T) {
	stub := newGrafanaPutStub()
	setupGrafanaPutDaemon(t, stub)
	file := writeTempJSON(t, `{"title":"No UID"}`)
	err := runGrafanaPut(t, "gardenio", "", file)
	if err == nil || !strings.Contains(err.Error(), "uid") {
		t.Fatalf("want uid error, got %v", err)
	}
	if len(stub.creates) != 0 {
		t.Errorf("no beads should be created without a uid, got %d", len(stub.creates))
	}
}

func TestGrafanaPutCmd_RejectsInvalidJSON(t *testing.T) {
	stub := newGrafanaPutStub()
	setupGrafanaPutDaemon(t, stub)
	file := writeTempJSON(t, `not json at all`)
	if err := runGrafanaPut(t, "gardenio", "", file); err == nil {
		t.Fatal("invalid JSON should error")
	}
}

func TestGrafanaPutCmd_MissingFile(t *testing.T) {
	stub := newGrafanaPutStub()
	setupGrafanaPutDaemon(t, stub)
	if err := runGrafanaPut(t, "gardenio", "", filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("missing file should error")
	}
}

// TestGrafanaPutCmd_TitleOverride confirms --title wins over the model's title.
func TestGrafanaPutCmd_TitleOverride(t *testing.T) {
	stub := newGrafanaPutStub()
	setupGrafanaPutDaemon(t, stub)
	file := writeTempJSON(t, `{"uid":"d","title":"Model Title"}`)
	if err := runGrafanaPut(t, "gardenio", "Override Title", file); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	for _, b := range stub.beads {
		if b["type"] == "dashboard" {
			if got := b["title"]; got != "Override Title" {
				t.Errorf("title = %v, want Override Title", got)
			}
		}
	}
}
