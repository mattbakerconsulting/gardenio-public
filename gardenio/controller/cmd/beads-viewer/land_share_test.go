package main

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planwindow"
)

// landShareFake is the viewer's daemon with the window routes for a land: the
// land preview (planwindow's "mixed" land), a land window, a revoked land
// window and a window on one of the land's plans.
type landShareFake struct {
	*fakeDaemon
	view     []byte
	err      error
	minted   []int
	revoked  []string
	previews []int
}

func (f *landShareFake) PreviewLandWindow(ctx context.Context, _ string, days int) (json.RawMessage, error) {
	if beadsapi.UserBearerFromContext(ctx) == "" {
		return nil, beadsapi.ErrUserBearerRequired // as the client fails closed on WithUserBearerOnly, unsent
	}
	f.previews = append(f.previews, days)
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(f.view), nil
}

func (f *landShareFake) CreateLandWindow(_ context.Context, land string, days int) (*beadsapi.MintedWindow, error) {
	f.minted = append(f.minted, days)
	if f.err != nil {
		return nil, f.err
	}
	return &beadsapi.MintedWindow{
		Window: beadsapi.Window{ID: "gd-lwnew", TargetKind: "land", Land: land, State: "live", ExpiresAt: time.Now().AddDate(0, 0, days)},
		Token:  "gw_LAND", Path: "/w/gw_LAND",
	}, nil
}

func (f *landShareFake) ListWindows(_ context.Context, land string) ([]beadsapi.Window, error) {
	if f.err != nil {
		return nil, f.err
	}
	seen := time.Now().Add(-time.Hour)
	return []beadsapi.Window{
		{ID: "gd-lw1", TargetKind: "land", Land: land, State: "live", ViewCount: 42, LastViewedAt: &seen, ExpiresAt: time.Now().AddDate(0, 0, 20), CreatedBy: "owner@orchard.example"},
		{ID: "gd-lw0", TargetKind: "land", Land: land, State: "revoked", ExpiresAt: time.Now().AddDate(0, 0, 5)},
		{ID: "gd-wplan", TargetKind: "plan", PlanID: "gd-plan", Land: land, State: "live", ExpiresAt: time.Now().AddDate(0, 0, 5)},
	}, nil
}

func (f *landShareFake) RevokeWindow(_ context.Context, id string) (*beadsapi.Window, error) {
	f.revoked = append(f.revoked, id)
	if f.err != nil {
		return nil, f.err
	}
	return &beadsapi.Window{ID: id, TargetKind: "land", State: "revoked"}, nil
}

func landShareSetup(t *testing.T) (*landShareFake, *Server) {
	t.Helper()
	f := &landShareFake{fakeDaemon: &fakeDaemon{}, view: windowLandFixture(t, "mixed")}
	return f, NewServer(f, slog.Default(), "", "", "", nil)
}

func landShareRequest(method, target string, form url.Values) *http.Request {
	r := shareRequest(method, target, form)
	r.SetPathValue("project", "orchard")
	return r
}

func decodeLandPanel(t *testing.T, rec *httptest.ResponseRecorder) landSharePanel {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, body:\n%s", ct, rec.Body.String())
	}
	var p landSharePanel
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("panel: %v\n%s", err, rec.Body.String())
	}
	return p
}

// TestLandShare_Panel: the tab's document says what the link grants, links the
// preview at the chosen lifetime, lists what the window would withhold and
// the land's windows on the whole land, and never a token.
func TestLandShare_Panel(t *testing.T) {
	f, s := landShareSetup(t)
	rec := httptest.NewRecorder()
	s.handleLandShare(rec, landShareRequest(http.MethodGet, "/p/orchard/share?days=7", nil))
	p := decodeLandPanel(t, rec)
	if !p.OK || p.Error != "" || p.Land != "orchard" || p.Grants != planwindow.LandGrants || p.Days != 7 {
		t.Errorf("panel = %+v", p)
	}
	if p.Preview != "/p/orchard/share/preview?days=7" || p.Plans == 0 {
		t.Errorf("preview %q over %d plans", p.Preview, p.Plans)
	}
	if len(p.Withheld) == 0 || p.WithheldText != planwindow.WithheldText {
		t.Errorf("withheld = %+v (%q), want the mixed land's hits", p.Withheld, p.WithheldText)
	}
	if len(p.Windows) != 2 || p.Windows[0].ID != "gd-lw1" || p.Windows[0].ViewCount != 42 || p.Windows[1].ID != "gd-lw0" {
		t.Errorf("windows = %+v, want the two land windows", p.Windows)
	}
	if body := rec.Body.String(); strings.Contains(body, "gd-wplan") || strings.Contains(body, "gw_") {
		t.Errorf("panel carries a plan window or a token:\n%s", body)
	}
	if len(f.previews) != 1 || f.previews[0] != 7 {
		t.Errorf("previews = %v, want one of 7 days", f.previews)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q", cc)
	}
}

// TestLandShare_Preview: the preview is the land window page itself
// (window.html's window_land), with land.js and the window's headers, and
// the document a minted window's land.json would serve inline for land.js to
// draw.
func TestLandShare_Preview(t *testing.T) {
	f, s := landShareSetup(t)
	rec := httptest.NewRecorder()
	s.handleLandSharePreview(rec, landShareRequest(http.MethodGet, "/p/orchard/share/preview?days=90", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"A window on a land · read-only", "<h1>Orchard</h1>",
		`<script src="/w/static/land.js"></script>`,
		`<div class="w-world" id="w-world" hidden>`, `<canvas id="w-cv"`,
		`<span class="w-what">Grow the orchard</span>`,
		"<span class=\"w-what\">" + planwindow.WithheldText + "</span>",
		`<script type="application/json" id="w-seed">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	if strings.Contains(body, "gw_") || strings.Contains(body, "ops@example.com") {
		t.Errorf("preview carries a token or a withheld field:\n%s", body)
	}
	if len(f.previews) != 1 || f.previews[0] != 90 {
		t.Errorf("previews = %v, want one of 90 days", f.previews)
	}

	// The seed is exactly what land.json would serve for this land.
	_, rest, _ := strings.Cut(body, `<script type="application/json" id="w-seed">`)
	seed, _, _ := strings.Cut(rest, "</script>")
	snap, err := buildWindowSnapshot(f.view, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if seed != string(snap.doc) {
		t.Errorf("seed is not land.json's document:\n%s\nwant\n%s", seed, snap.doc)
	}

	h := rec.Header()
	if csp := h.Get("Content-Security-Policy"); csp != windowContentSecurityPolicy {
		t.Errorf("Content-Security-Policy = %q, want the window page's", csp)
	}
	if h.Get("Referrer-Policy") != "no-referrer" || !strings.Contains(h.Get("Cache-Control"), "no-store") || h.Get("X-Robots-Tag") == "" {
		t.Errorf("headers = %v", h)
	}

	// A refusal is the daemon's status and the tab's sentence, no page.
	f.err = &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "no"}
	rec = httptest.NewRecorder()
	s.handleLandSharePreview(rec, landShareRequest(http.MethodGet, "/p/orchard/share/preview", nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), landShareDenied) || strings.Contains(rec.Body.String(), "w-world") {
		t.Errorf("refused preview: %d\n%s", rec.Code, rec.Body.String())
	}
}

// TestLandShare_PreviewSeedCannotCloseItsScript: a land title that tries to
// end the seed's script block stays inside it as JSON.
func TestLandShare_PreviewSeedCannotCloseItsScript(t *testing.T) {
	f, s := landShareSetup(t)
	f.view = []byte(`{"land":"orchard","open_plans":1,"plans":[{"title":"</script><script>alert(1)</script>","status":"open","state":"building"}]}`)
	rec := httptest.NewRecorder()
	s.handleLandSharePreview(rec, landShareRequest(http.MethodGet, "/p/orchard/share/preview", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, "<script>alert") || strings.Count(body, "<script") != 2 {
		t.Errorf("preview with a hostile title: %d\n%s", rec.Code, body)
	}
	_, rest, _ := strings.Cut(body, `id="w-seed">`)
	seed, _, _ := strings.Cut(rest, "</script>")
	var doc map[string]any
	if err := json.Unmarshal([]byte(seed), &doc); err != nil || !bytes.Contains([]byte(seed), []byte(`\u003c/script\u003e`)) {
		t.Errorf("seed = %s (%v)", seed, err)
	}
}

func TestLandShare_MintShowsURLOnce(t *testing.T) {
	f, s := landShareSetup(t)
	rec := httptest.NewRecorder()
	s.handleLandShareMint(rec, landShareRequest(http.MethodPost, "/p/orchard/share", url.Values{"days": {"90"}}))
	p := decodeLandPanel(t, rec)
	if rec.Code != http.StatusOK || !p.OK || p.Minted == nil || p.Minted.URL != "https://gardenio.clonk.tv/w/gw_LAND" || p.Minted.ID != "gd-lwnew" {
		t.Errorf("mint answer: %d %+v", rec.Code, p)
	}
	if p.Grants != planwindow.LandGrants || len(p.Windows) != 2 {
		t.Errorf("mint answer's panel = %+v", p)
	}
	if len(f.minted) != 1 || f.minted[0] != 90 {
		t.Errorf("minted = %v, want one of 90 days", f.minted)
	}
	// A lifetime the form does not offer falls back to the default.
	s.handleLandShareMint(httptest.NewRecorder(), landShareRequest(http.MethodPost, "/p/orchard/share", url.Values{"days": {"9999"}}))
	if f.minted[1] != planShareDefaultDays {
		t.Errorf("minted with days=9999: %d, want %d", f.minted[1], planShareDefaultDays)
	}
	// The panel the GET answers never carries the URL again.
	rec = httptest.NewRecorder()
	s.handleLandShare(rec, landShareRequest(http.MethodGet, "/p/orchard/share", nil))
	if strings.Contains(rec.Body.String(), "gw_LAND") {
		t.Error("the panel shows the token again")
	}
}

func TestLandShare_Revoke(t *testing.T) {
	f, s := landShareSetup(t)
	r := landShareRequest(http.MethodPost, "/p/orchard/share/gd-lw1/revoke", url.Values{"days": {"7"}})
	r.SetPathValue("window", "gd-lw1")
	rec := httptest.NewRecorder()
	s.handleLandShareRevoke(rec, r)
	p := decodeLandPanel(t, rec)
	if len(f.revoked) != 1 || f.revoked[0] != "gd-lw1" || !p.OK || p.Revoked != "gd-lw1" || p.Days != 7 {
		t.Errorf("revoked %v, panel %+v", f.revoked, p)
	}
}

// TestLandShare_ScopedMember: kbeads refuses a member, and the tab says who
// may share instead of carrying anything of the land; a refused mint or
// revoke answers the daemon's status.
func TestLandShare_ScopedMember(t *testing.T) {
	f, s := landShareSetup(t)
	f.err = &beadsapi.APIError{StatusCode: http.StatusForbidden, Message: "only the owner of land orchard or an operator, on their own sign-in, may manage its windows"}
	rec := httptest.NewRecorder()
	s.handleLandShare(rec, landShareRequest(http.MethodGet, "/p/orchard/share", nil))
	if p := decodeLandPanel(t, rec); p.Error != landShareDenied || p.Preview != "" || len(p.Windows) != 0 || len(p.Withheld) != 0 {
		t.Errorf("refused panel = %+v", p)
	}
	for name, run := range map[string]func(http.ResponseWriter, *http.Request){"mint": s.handleLandShareMint, "revoke": s.handleLandShareRevoke} {
		rec := httptest.NewRecorder()
		r := landShareRequest(http.MethodPost, "/p/orchard/share", url.Values{})
		r.SetPathValue("window", "gd-lw1")
		run(rec, r)
		p := decodeLandPanel(t, rec)
		if rec.Code != http.StatusForbidden || p.OK || p.ActionError != landShareDenied {
			t.Errorf("refused %s: %d %+v", name, rec.Code, p)
		}
	}
	if got := windowShareRefusal(beadsapi.ErrUserBearerRequired, landShareDenied); !strings.Contains(got, "own sign-in") {
		t.Errorf("refusal for a missing bearer = %q", got)
	}
}

// TestLandShare_Routes: the four routes are mounted, routeScoped and held to
// the caller's own land, and a session with no bearer is refused unsent.
func TestLandShare_Routes(t *testing.T) {
	for _, p := range []string{"GET /p/{project}/share", "GET /p/{project}/share/preview", "POST /p/{project}/share", "POST /p/{project}/share/{window}/revoke"} {
		if viewerRouteClasses[p] != routeScoped || viewerProjectParams[p] != "project" {
			t.Errorf("%s: class %v, project param %q", p, viewerRouteClasses[p], viewerProjectParams[p])
		}
	}
	f, s := landShareSetup(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	r := httptest.NewRequest(http.MethodGet, "/p/orchard/share?days=30", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	p := decodeLandPanel(t, rec)
	if !strings.Contains(p.Error, "own sign-in") || len(f.previews) != 0 {
		t.Errorf("no bearer: panel %+v, previews %v (want refused unsent)", p, f.previews)
	}
	if strings.Contains(html.UnescapeString(rec.Body.String()), "Grow the orchard") {
		t.Error("a refused panel carries the land")
	}
}

// TestLandSharePreviewUnderChromium opens the owner's preview in a real
// Chromium (testdata/land_share_preview_browser.js): land.js draws the World
// from the page's inline document and never asks for land.json.
// LAND_SHARE_SHOTS=<dir> keeps a screenshot per size.
func TestLandSharePreviewUnderChromium(t *testing.T) {
	skip := t.Skipf
	if os.Getenv("GARDENHUB_BROWSER_REQUIRED") == "1" {
		skip = t.Fatalf
	}
	node, err := exec.LookPath("node")
	if err != nil {
		skip("node not installed")
	}
	mods := os.Getenv("GARDENHUB_PW_MODULES")
	if mods == "" {
		mods, _ = filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "node_modules"))
	}
	if _, err := os.Stat(filepath.Join(mods, "playwright-core")); err != nil {
		skip("playwright-core not installed under %s (make test-gardenhub-browser)", mods)
	}
	_, s := landShareSetup(t)
	app := http.NewServeMux()
	s.RegisterRoutes(app)
	// the auth chain's part: the signed-in owner's bearer on the request
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app.ServeHTTP(w, r.WithContext(beadsapi.WithUserBearer(r.Context(), "user-jwt")))
	}))
	t.Cleanup(hs.Close)
	script, _ := filepath.Abs(filepath.Join("testdata", "land_share_preview_browser.js"))
	cmd := exec.Command(node, script, hs.URL+"/p/orchard/share/preview?days=30", mods)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
		skip("no Chromium for playwright-core: %s", strings.TrimSpace(string(out)))
	}
	if err != nil {
		t.Fatalf("land share preview checks failed: %v\n%s", err, out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}
