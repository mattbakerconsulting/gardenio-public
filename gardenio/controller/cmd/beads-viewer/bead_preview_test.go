package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// newTestServer builds a Server suitable for unit-testing the excerpt helper.
// The daemon is unused for excerpt-only tests but must be non-nil to construct
// the goldmark instances.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(newFake(), slog.Default(), "", "", "", nil)
}

func TestExcerpt_NoEllipsisWhenShort(t *testing.T) {
	s := newTestServer(t)
	got := s.excerpt("Hello world.", 280)
	if got != "Hello world." {
		t.Errorf("excerpt(short) = %q, want %q", got, "Hello world.")
	}
	if strings.HasSuffix(got, "…") {
		t.Errorf("expected no ellipsis on short input, got %q", got)
	}
}

func TestExcerpt_StripsMarkdown(t *testing.T) {
	s := newTestServer(t)
	got := s.excerpt("**bold** and _italic_ with `code`", 280)
	for _, want := range []string{"bold", "italic", "code"} {
		if !strings.Contains(got, want) {
			t.Errorf("excerpt missing %q in %q", want, got)
		}
	}
	for _, banned := range []string{"**", "__", "<strong>", "<em>", "<code>"} {
		if strings.Contains(got, banned) {
			t.Errorf("excerpt should have stripped %q from %q", banned, got)
		}
	}
}

func TestExcerpt_AppendsEllipsisOnTruncate(t *testing.T) {
	s := newTestServer(t)
	long := strings.Repeat("a", 400)
	got := s.excerpt(long, 100)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected trailing ellipsis on truncated excerpt, got %q (len=%d)", got, len([]rune(got)))
	}
	// 100 truncated runes + the ellipsis = 101 runes total.
	if n := len([]rune(got)); n != 101 {
		t.Errorf("expected 101 runes (100 + ellipsis), got %d (%q)", n, got)
	}
}

func TestExcerpt_TruncatesAtRuneBoundary(t *testing.T) {
	s := newTestServer(t)
	// "héllo" has multi-byte runes — truncate at 3 runes should yield "hél…".
	got := s.excerpt("héllo world", 3)
	if got != "hél…" {
		t.Errorf("excerpt = %q, want %q", got, "hél…")
	}
	// All-emoji input: truncating 5 emoji to 3 runes should leave 3 emoji + ellipsis.
	emoji := s.excerpt("🌊🌊🌊🌊🌊", 3)
	if got, want := len([]rune(emoji)), 4; got != want {
		t.Errorf("emoji excerpt rune count = %d, want %d (%q)", got, want, emoji)
	}
	if !strings.HasSuffix(emoji, "…") {
		t.Errorf("emoji excerpt missing trailing ellipsis: %q", emoji)
	}
}

func TestExcerpt_EmptyAndZero(t *testing.T) {
	s := newTestServer(t)
	if got := s.excerpt("", 280); got != "" {
		t.Errorf("excerpt(\"\", 280) = %q, want empty", got)
	}
	if got := s.excerpt("hello", 0); got != "" {
		t.Errorf("excerpt(\"hello\", 0) = %q, want empty", got)
	}
}

func TestHandleBeadPreview_Renders(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/b/gd-aaa/preview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "private, max-age=30" {
		t.Errorf("Cache-Control = %q, want private, max-age=30", cc)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="bead-preview"`,
		`data-bead-id="gd-aaa"`,
		`tag-type`,
		`task`,            // type
		`tag-open`,        // status tag class
		`>open<`,          // status tag text
		`bead-preview-id`, // id chip class
		`>gd-aaa<`,        // id literal
		`Example bead`,    // title
		`First paragraph`, // excerpt
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered preview missing %q\nbody=%s", want, body)
		}
	}
	// Fragment, not a page.
	if strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "<html") {
		t.Errorf("preview must be a fragment, not a page\nbody=%s", body)
	}
}

func TestHandleBeadPreview_NotFound(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/b/gd-missing/preview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (notfound fragment), got %d body=%s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := w.Body.String()
	for _, want := range []string{
		`bead-preview-missing`,
		`data-bead-id="gd-missing"`,
		`>gd-missing<`,
		`not found`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("notfound fragment missing %q\nbody=%s", want, body)
		}
	}
}

// previewErrDaemon wraps fakeDaemon to force GetBead into a non-404 error
// path so the preview handler exercises the 500 branch.
type previewErrDaemon struct {
	*fakeDaemon
	getBeadErr error
}

func (p *previewErrDaemon) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	if p.getBeadErr != nil {
		return nil, p.getBeadErr
	}
	return p.fakeDaemon.GetBead(ctx, id)
}

func TestHandleBeadPreview_DaemonError(t *testing.T) {
	d := &previewErrDaemon{fakeDaemon: newFake(), getBeadErr: errors.New("upstream broke")}
	mux := mkServer(t, d)
	req := httptest.NewRequest("GET", "/b/gd-aaa/preview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleBeadPreview_RouteMounted(t *testing.T) {
	// Sanity that the new route is registered through RegisterRoutes — a
	// regression where the wiring is forgotten would surface as 404 here.
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/b/gd-aaa/preview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Fatalf("/b/{id}/preview not mounted — got 404 from registered mux")
	}
}

// TestHandleBeadPreview_AuthRequired verifies the preview route is registered
// on the protected mux (i.e. wrapped by RegisterRoutes — the same mux every
// other /b/{id}/* route is wired through). main.go composes the OIDC chain
// around that mux, so any route registered here inherits the same auth gate.
//
// The test simulates the production composition by wrapping the mux in a
// gate middleware that requires X-Auth-Request-Email — the same header the
// OIDC middleware injects in prod — and asserts the preview route is blocked
// on missing creds and reachable when the header is present.
func TestHandleBeadPreview_AuthRequired(t *testing.T) {
	mux := mkServer(t, newFake())
	gated := requireEmailMiddleware(mux)

	// Anonymous: middleware refuses.
	reqAnon := httptest.NewRequest("GET", "/b/gd-aaa/preview", nil)
	wAnon := httptest.NewRecorder()
	gated.ServeHTTP(wAnon, reqAnon)
	if wAnon.Code != http.StatusUnauthorized {
		t.Errorf("anonymous preview = %d, want 401", wAnon.Code)
	}

	// Authenticated: middleware passes through, handler returns 200.
	reqAuth := httptest.NewRequest("GET", "/b/gd-aaa/preview", nil)
	reqAuth.Header.Set("X-Auth-Request-Email", authorEmail)
	wAuth := httptest.NewRecorder()
	gated.ServeHTTP(wAuth, reqAuth)
	if wAuth.Code != http.StatusOK {
		t.Errorf("authenticated preview = %d, want 200", wAuth.Code)
	}
}

// TestHandleBeadPreview_EscapesHTMLInTitle guards ka-0000005464: the hover
// preview fragment renders a bead title HTML-escaped, never as markup.
func TestHandleBeadPreview_EscapesHTMLInTitle(t *testing.T) {
	f := newFake()
	f.beads["gd-xss"] = &beadsapi.BeadDetail{
		ID:          "gd-xss",
		Title:       "<script>alert(1)</script>",
		Type:        "task",
		Status:      "open",
		Description: "Plain description.",
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-xss/preview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("preview rendered raw <script> tag — XSS leak\nbody=%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("expected HTML-escaped title in body=%s", body)
	}
}

// requireEmailMiddleware mirrors the shape of the OIDC middleware applied in
// main.go: requests missing the X-Auth-Request-Email header are rejected
// before the handler runs.
func requireEmailMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Request-Email") == "" {
			http.Error(w, "Unauthenticated", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Anchored hover preview (A5, ka-0000003033) ---

func TestHandleBeadPreview_AnchorRendersSection(t *testing.T) {
	fake := newFake()
	fake.beads["gd-anchored1"] = &beadsapi.BeadDetail{
		ID:    "gd-anchored1",
		Type:  "task",
		Title: "Anchored bead",
		Description: "# Plan\n\nintro\n\n" +
			"### C1.1 — TTL field\n\nThe TTL field carries expiry.\n\n" +
			"### C1.2 — Refresh\n\nrefresh body",
	}
	s := NewServer(fake, slog.Default(), "", "", "", nil)

	req := httptest.NewRequest(http.MethodGet, "/b/gd-anchored1/preview?anchor=c1-1", nil)
	req.SetPathValue("id", "gd-anchored1")
	rec := httptest.NewRecorder()
	s.handleBeadPreview(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "C1.1 — TTL field") {
		t.Errorf("preview missing anchored heading: %s", body)
	}
	if !strings.Contains(body, "The TTL field carries expiry.") {
		t.Errorf("preview missing anchored section body: %s", body)
	}
	if strings.Contains(body, "refresh body") {
		t.Errorf("preview leaked into next section: %s", body)
	}
}

func TestHandleBeadPreview_UnknownAnchorFallsBackToWholeBead(t *testing.T) {
	fake := newFake()
	fake.beads["gd-anchored2"] = &beadsapi.BeadDetail{
		ID:          "gd-anchored2",
		Type:        "task",
		Title:       "Fallback bead",
		Description: "## Real\n\nwhole bead body here",
	}
	s := NewServer(fake, slog.Default(), "", "", "", nil)

	req := httptest.NewRequest(http.MethodGet, "/b/gd-anchored2/preview?anchor=does-not-exist", nil)
	req.SetPathValue("id", "gd-anchored2")
	rec := httptest.NewRecorder()
	s.handleBeadPreview(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "whole bead body here") {
		t.Errorf("expected whole-bead fallback, got: %s", body)
	}
}
