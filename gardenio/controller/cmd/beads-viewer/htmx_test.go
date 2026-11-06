package main

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// authorEmail is the email a real OIDC middleware would inject into the
// X-Auth-Request-Email header. Tests use it to simulate an authenticated user
// without standing up a Keycloak provider.
const authorEmail = "operator@example.com"

// authedPost builds a POST httptest request with form values and the
// authentication header the OIDC middleware would normally inject.
func authedPost(path string, form url.Values) *http.Request {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	return req
}

// TestStaticAsset_ServesHtmx verifies the vendored htmx.min.js is served
// from /assets/static/htmx.min.js — the script tag in layout.html points
// there, and a missing or mis-served asset would break every page that
// relies on progressive enhancement.
func TestStaticAsset_ServesHtmx(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/htmx.min.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for htmx.min.js, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "htmx") {
		t.Errorf("expected htmx library content in asset body, first 200 bytes: %q", body[:min(200, len(body))])
	}
	// Sanity — should look like JS, not HTML.
	if strings.HasPrefix(body, "<!DOCTYPE") {
		t.Errorf("htmx asset served as HTML page, got: %q", body[:min(200, len(body))])
	}
}

// TestStaticAsset_ServesKeys verifies templates/static/keys.js (the
// keyboard-shortcut script) is reachable at /assets/static/keys.js.
func TestStaticAsset_ServesKeys(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/keys.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for keys.js, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "keydown") {
		t.Errorf("keys.js missing expected keydown handler, first 200 bytes: %q", body[:min(200, len(body))])
	}
	if strings.HasPrefix(body, "<!DOCTYPE") {
		t.Errorf("keys.js served as HTML page, got: %q", body[:min(200, len(body))])
	}
	// g-prefix nav (gd-Kwfe762qkZ): g h → / (home), g i → /active, / → focus search.
	// /inbox was folded into /u/me (5af097aee) and the home route became the
	// canonical landing, so keys.js GO_TARGETS maps h→'/' and i→'/active';
	// keyboard.spec.js asserts the same. Keep all three layers aligned.
	if !strings.Contains(body, "h: '/'") {
		t.Errorf("keys.js: expected g h to map to / (home)")
	}
	if !strings.Contains(body, "i: '/active'") {
		t.Errorf("keys.js: expected g i to map to /active")
	}
	if !strings.Contains(body, "focusSearch") {
		t.Errorf("keys.js: expected '/' shortcut to focus search input (M2 §7)")
	}
	// /up two-driver keydown gate (ka-0000003390): keys.js must bail on the /up
	// surface so up.js is the sole keyboard owner there — otherwise BOTH
	// document keydown listeners fire on one press (e.g. `?` opened two help
	// <dialog>s). The gate is the presence check for up.js's own self-gate
	// marker; up_keymap.spec.js asserts the resulting single-dialog behavior in
	// a real browser. Keep this string in sync with the guard in keys.js.
	if !strings.Contains(body, `document.querySelector('[data-surface="up"]')`) {
		t.Errorf("keys.js: expected the /up keydown gate ([data-surface=\"up\"] bail) — the two-driver collision guard (ka-0000003390)")
	}
}

// TestLayout_IncludesKeysScript ensures every rendered page loads the
// keyboard-shortcut script alongside htmx.
func TestLayout_IncludesKeysScript(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)
	for _, path := range []string{"/", "/gd-aaa", "/p/gardenio", "/search"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		body := w.Body.String()
		if !strings.Contains(body, `src="/assets/static/keys.js"`) {
			t.Errorf("%s: layout missing keys.js script tag", path)
		}
		if !strings.Contains(body, `class="kbd-help-button"`) {
			t.Errorf("%s: nav missing keyboard-shortcut help button", path)
		}
	}
}

// TestStaticAsset_ServesDrafts verifies templates/static/drafts.js (the
// comment-draft autosave script) is reachable at /assets/static/drafts.js.
func TestStaticAsset_ServesDrafts(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/drafts.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for drafts.js, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "bv:draft:") {
		t.Errorf("drafts.js missing expected localStorage key prefix, first 200 bytes: %q", body[:min(200, len(body))])
	}
	if strings.HasPrefix(body, "<!DOCTYPE") {
		t.Errorf("drafts.js served as HTML page, got: %q", body[:min(200, len(body))])
	}
}

// TestLayout_IncludesDraftsScript ensures every rendered page loads the
// comment-draft autosave script alongside keys.js.
func TestLayout_IncludesDraftsScript(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)
	for _, path := range []string{"/", "/gd-aaa", "/p/gardenio", "/search"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		body := w.Body.String()
		if !strings.Contains(body, `src="/assets/static/drafts.js"`) {
			t.Errorf("%s: layout missing drafts.js script tag", path)
		}
	}
}

// TestStaticAsset_ServesCommentBadge verifies templates/static/comment_badge.js
// (the tab-title badge for new comments) is reachable at
// /assets/static/comment_badge.js.
func TestStaticAsset_ServesCommentBadge(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/comment_badge.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for comment_badge.js, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "comment-new") {
		t.Errorf("comment_badge.js missing expected .comment-new selector, first 200 bytes: %q", body[:min(200, len(body))])
	}
	if !strings.Contains(body, "visibilitychange") {
		t.Errorf("comment_badge.js missing visibilitychange listener, first 200 bytes: %q", body[:min(200, len(body))])
	}
	if strings.HasPrefix(body, "<!DOCTYPE") {
		t.Errorf("comment_badge.js served as HTML page, got: %q", body[:min(200, len(body))])
	}
}

// TestBeadShow_IncludesCommentBadgeScript ensures the bead-detail page
// loads the tab-title badge script. Other pages must NOT load it (it is
// only relevant where the comments-section live-poll runs).
func TestBeadShow_IncludesCommentBadgeScript(t *testing.T) {
	mux := mkServer(t, newFake())

	// Bead detail page: must load the badge script.
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `src="/assets/static/comment_badge.js"`) {
		t.Errorf("bead-detail page missing comment_badge.js script tag\nbody=%s", body)
	}

	// Non-detail pages: must NOT load it.
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux2 := mkServer(t, f)
	for _, path := range []string{"/", "/p/gardenio", "/search"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux2.ServeHTTP(w, req)
		if strings.Contains(w.Body.String(), `src="/assets/static/comment_badge.js"`) {
			t.Errorf("%s: should not load comment_badge.js", path)
		}
	}
}

// TestStaticAsset_ServesUnreadListing verifies templates/static/unread_listing.js
// (the per-bead unread-comment indicator) is reachable at
// /assets/static/unread_listing.js. The decorate function is the load-bearing
// part of the script — failing to ship that selector means listings can never
// surface unread dots.
func TestStaticAsset_ServesUnreadListing(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/unread_listing.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for unread_listing.js, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "bv:lastSeen:v1") {
		t.Errorf("unread_listing.js missing localStorage key, first 200 bytes: %q", body[:min(200, len(body))])
	}
	if !strings.Contains(body, "data-bead-id") || !strings.Contains(body, "data-last-activity-ts") {
		t.Errorf("unread_listing.js missing row selectors, first 200 bytes: %q", body[:min(200, len(body))])
	}
	if !strings.Contains(body, "htmx:afterSwap") {
		t.Errorf("unread_listing.js missing htmx:afterSwap listener, first 200 bytes: %q", body[:min(200, len(body))])
	}
	if strings.HasPrefix(body, "<!DOCTYPE") {
		t.Errorf("unread_listing.js served as HTML page, got: %q", body[:min(200, len(body))])
	}
}

// TestLayout_IncludesUnreadListingScript ensures every page (listing + detail)
// loads the unread-listing decorator. The script handles both writer-side
// (bead-show) and listing-decorator duties, so it lives in layout.html and
// runs on all pages.
func TestLayout_IncludesUnreadListingScript(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)
	for _, path := range []string{"/", "/gd-aaa", "/p/gardenio", "/active", "/decisions"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		body := w.Body.String()
		if !strings.Contains(body, `src="/assets/static/unread_listing.js"`) {
			t.Errorf("%s: layout missing unread_listing.js script tag", path)
		}
	}
}

// TestLayout_IncludesHtmxScript ensures every rendered page loads htmx.
func TestLayout_IncludesHtmxScript(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)
	for _, path := range []string{"/", "/gd-aaa", "/p/gardenio", "/search"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		body := w.Body.String()
		if !strings.Contains(body, `src="/assets/static/htmx.min.js"`) {
			t.Errorf("%s: layout missing htmx script tag\nbody=%s", path, body)
		}
	}
}

// TestBeadShow_FullServerSideRender_NoJS verifies the progressive-enhancement
// contract: a JS-off client must still see every section (deps, comments,
// description). htmx must augment, not replace, the server-side render.
func TestBeadShow_FullServerSideRender_NoJS(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// All of these are content a JS-off client must see inline.
	for _, want := range []string{
		"<h1", "Example bead",
		"<h2>Dependencies</h2>",
		`<a href="/gd-parent"`,
		"parent-child",
		"<h2>Comments (1)</h2>",
		"reviewer",
		"Looks good to me.",
		`<h3 aria-level="2">Description</h3>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("JS-off render missing %q\nbody=%s", want, body)
		}
	}
}

// TestBeadLinks_HavePrefetchAttrs verifies anchor tags to bead pages include
// hx-trigger=mouseover prefetch attributes so htmx warms the browser cache
// before the user clicks.
func TestBeadLinks_HavePrefetchAttrs(t *testing.T) {
	// Dependency link on bead show page.
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{
		`hx-get="/gd-parent"`,
		`hx-trigger="mouseover once"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bead show: dep link missing %q\nbody=%s", want, body)
		}
	}
}

// TestProjectListing_EmitsUnreadDataAttrs verifies the per-project listing
// rows carry the `data-bead-id` and `data-last-activity-ts` attributes that
// unread_listing.js reads. Without these, the JS decorator has no rows to
// match and the unread dot never renders, regardless of localStorage state.
func TestProjectListing_EmitsUnreadDataAttrs(t *testing.T) {
	updated := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-rowx", Title: "Row X", Type: "task", Status: "open", UpdatedAt: updated},
		},
		Total: 1,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `data-bead-id="gd-rowx"`) {
		t.Errorf("project row missing data-bead-id\nbody=%s", body)
	}
	if !strings.Contains(body, `data-last-activity-ts="2026-04-20T12:00:00Z"`) {
		t.Errorf("project row missing data-last-activity-ts (RFC3339Nano UTC)\nbody=%s", body)
	}
}

// TestBeadShow_CommentsSectionExposesUnreadAttrs verifies the bead-show
// `#comments-section` exposes the bead-id + bead-updated-at the
// unread_listing.js writer reads to advance lastSeen[beadId].
func TestBeadShow_CommentsSectionExposesUnreadAttrs(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `data-bead-id="gd-aaa"`) {
		t.Errorf("comments-section missing data-bead-id\nbody=%s", body)
	}
	if !strings.Contains(body, `data-bead-updated-at="`) {
		t.Errorf("comments-section missing data-bead-updated-at\nbody=%s", body)
	}
}

// TestProjectLinks_HavePrefetchAttrs exercises the same contract on the
// per-project listing.
func TestProjectLinks_HavePrefetchAttrs(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-p1", Title: "One", Type: "task", Status: "open"},
		},
		Total: 1,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `hx-get="/gd-p1"`) {
		t.Errorf("project link missing hx-get prefetch\nbody=%s", body)
	}
	if !strings.Contains(body, `hx-trigger="mouseover once"`) {
		t.Errorf("project link missing hx-trigger mouseover\nbody=%s", body)
	}
}

// TestCommentsPartial_RendersFragment verifies /b/{id}/comments returns
// just the comments <section>, not a full HTML page.
func TestCommentsPartial_RendersFragment(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/b/gd-aaa/comments", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, "<html") {
		t.Errorf("partial must not wrap layout, got full page\nbody=%s", body)
	}
	for _, want := range []string{
		`id="comments-section"`,
		"<h2>Comments (1)</h2>",
		"reviewer",
		"Looks good to me.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comments partial missing %q\nbody=%s", want, body)
		}
	}
}

// TestCommentsPartial_PollsRegardlessOfStatus ensures the partial emits
// htmx polling attributes for any bead status, not just in_progress —
// open and closed beads can still receive new comments while a user is
// viewing them.
func TestCommentsPartial_PollsRegardlessOfStatus(t *testing.T) {
	f := newFake()
	f.beads["gd-ip"] = &beadsapi.BeadDetail{
		ID: "gd-ip", Title: "In progress", Type: "task", Status: "in_progress",
		CreatedAt: time.Now(),
	}
	f.beads["gd-cc"] = &beadsapi.BeadDetail{
		ID: "gd-cc", Title: "Closed", Type: "task", Status: "closed",
		CreatedAt: time.Now(),
	}
	mux := mkServer(t, f)

	for _, id := range []string{"gd-ip", "gd-cc", "gd-aaa"} {
		req := httptest.NewRequest("GET", "/b/"+id+"/comments", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		body := w.Body.String()
		if !strings.Contains(body, `hx-get="/b/`+id+`/comments"`) {
			t.Errorf("%s: missing hx-get poll target\nbody=%s", id, body)
		}
		if !strings.Contains(body, "every 30s") {
			t.Errorf("%s: missing 30s poll trigger\nbody=%s", id, body)
		}
		if !strings.Contains(body, "document.visibilityState") {
			t.Errorf("%s: poll trigger should pause when tab is hidden\nbody=%s", id, body)
		}
	}
}

// TestCommentsPartial_SinceMatchesLatest_Returns204 verifies the polling
// no-op path: when the client sends `?since=<latest>` matching the most
// recent comment, the server returns 204 No Content with no body so htmx
// leaves the existing DOM alone.
//
// MUST be 204, NOT 304 (regression ka-0000006459): htmx 2.0.4 classifies 304
// under its default [23].. "swap:true" rule, so a 304 with an empty body would
// swap NOTHING into the hx-swap="outerHTML" target and ERASE the whole
// #comments-section — which carries the poll's own hx-trigger, so the comments
// vanish on the first 30s poll and never come back. 204 is the htmx-blessed
// "leave the DOM alone" status (it maps 204 to swap:false).
func TestCommentsPartial_SinceMatchesLatest_Returns204(t *testing.T) {
	mux := mkServer(t, newFake())
	// gd-aaa's only comment was created at 2026-04-20T12:00:00Z.
	latest := "2026-04-20T12:00:00Z"
	req := httptest.NewRequest("GET", "/b/gd-aaa/comments?since="+latest, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 when since matches latest, got %d body=%s", w.Code, w.Body.String())
	}
	if w.Code == http.StatusNotModified {
		t.Fatal("304 would make htmx swap an empty body and erase the comments section (ka-0000006459)")
	}
	if got := strings.TrimSpace(w.Body.String()); got != "" {
		t.Errorf("204 must have empty body, got %q", got)
	}
	if got := w.Header().Get("X-Comments-Latest"); got != latest {
		t.Errorf("X-Comments-Latest=%q want %q", got, latest)
	}
}

// TestCommentsPartial_SinceOlder_RendersWithComment_New verifies the
// happy path: a `?since=<older>` cursor returns 200 with the new comment
// flagged via the `comment-new` class so the client can animate it.
func TestCommentsPartial_SinceOlder_RendersWithComment_New(t *testing.T) {
	mux := mkServer(t, newFake())
	// since=earlier than gd-aaa's comment timestamp.
	req := httptest.NewRequest("GET", "/b/gd-aaa/comments?since=2026-04-19T00:00:00Z", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="comment comment-new"`) {
		t.Errorf("expected comment-new class on new comment\nbody=%s", body)
	}
	if !strings.Contains(body, `data-created-at="2026-04-20T12:00:00Z"`) {
		t.Errorf("expected data-created-at on comment for client-side cursor\nbody=%s", body)
	}
	if got := w.Header().Get("X-Comments-Latest"); got != "2026-04-20T12:00:00Z" {
		t.Errorf("X-Comments-Latest=%q want 2026-04-20T12:00:00Z", got)
	}
}

// TestCommentsPartial_NoSince_NoNewClass verifies the initial render
// path (and any malformed `since`) does not flag every comment as new.
// Without this guard, every initial poll response would re-trigger the
// "new comment" animation across the whole list.
func TestCommentsPartial_NoSince_NoNewClass(t *testing.T) {
	mux := mkServer(t, newFake())
	for _, q := range []string{"", "?since=", "?since=garbage"} {
		req := httptest.NewRequest("GET", "/b/gd-aaa/comments"+q, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		body := w.Body.String()
		if w.Code != http.StatusOK {
			t.Errorf("%q: expected 200, got %d", q, w.Code)
		}
		if strings.Contains(body, "comment-new") {
			t.Errorf("%q: must not flag comments as new without a valid since\nbody=%s", q, body)
		}
		if !strings.Contains(body, `data-comments-latest="2026-04-20T12:00:00Z"`) {
			t.Errorf("%q: section must carry data-comments-latest for the next poll's cursor\nbody=%s", q, body)
		}
	}
}

// TestDepsPartial_RendersFragment verifies /b/{id}/deps returns just the
// dependencies <section>.
func TestDepsPartial_RendersFragment(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/b/gd-aaa/deps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, "<html") {
		t.Errorf("partial must not wrap layout\nbody=%s", body)
	}
	for _, want := range []string{
		`id="deps-section"`,
		"<h2>Dependencies</h2>",
		`href="/gd-parent"`,
		"parent-child",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("deps partial missing %q\nbody=%s", want, body)
		}
	}
}

// TestFragments_NotFound verifies missing beads return 404 on partial routes.
func TestFragments_NotFound(t *testing.T) {
	mux := mkServer(t, newFake())
	for _, path := range []string{"/b/gd-missing/comments", "/b/gd-missing/deps"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404, got %d", path, w.Code)
		}
	}
}

// TestFragments_RespectBasePath verifies partials use basePath for any
// internal links they emit (deps have bead-link anchors).
func TestFragments_RespectBasePath(t *testing.T) {
	f := newFake()
	srv := NewServer(f, slog.Default(), "/beads", "", "", nil)
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/b/gd-aaa/deps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `href="/beads/gd-parent"`) {
		t.Errorf("deps partial must honor basePath, body=%s", body)
	}
}

// TestCommentForm_RendersWhenAuthenticated verifies the GET path emits the
// htmx comment form when X-Auth-Request-Email is present (i.e. an OIDC user
// is logged in) and hides it otherwise.
func TestCommentForm_RendersWhenAuthenticated(t *testing.T) {
	mux := mkServer(t, newFake())

	// Anonymous: no header → no form.
	reqAnon := httptest.NewRequest("GET", "/b/gd-aaa/comments", nil)
	wAnon := httptest.NewRecorder()
	mux.ServeHTTP(wAnon, reqAnon)
	if strings.Contains(wAnon.Body.String(), "comment-form") {
		t.Errorf("anonymous request must not see the comment form\nbody=%s", wAnon.Body.String())
	}

	// Authenticated: form present, hx-post + hx-target wired.
	reqAuth := httptest.NewRequest("GET", "/b/gd-aaa/comments", nil)
	reqAuth.Header.Set("X-Auth-Request-Email", authorEmail)
	wAuth := httptest.NewRecorder()
	mux.ServeHTTP(wAuth, reqAuth)
	body := wAuth.Body.String()
	for _, want := range []string{
		`class="comment-form"`,
		`data-bead-id="gd-aaa"`,
		`hx-post="/b/gd-aaa/comments"`,
		`hx-target="#comments-section"`,
		`hx-swap="outerHTML"`,
		`hx-disabled-elt="find textarea, find button"`,
		authorEmail,
		`name="text"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("authenticated comments fragment missing %q\nbody=%s", want, body)
		}
	}
}

// TestAddComment_RequiresAuth verifies the POST handler rejects unauthenticated
// requests with 401, even though the OIDC middleware would normally block them
// upstream — defense-in-depth.
func TestAddComment_RequiresAuth(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{"text": {"hello"}}
	req := httptest.NewRequest("POST", "/b/gd-aaa/comments", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated POST, got %d", w.Code)
	}
	if f.addCommentCalls != 0 {
		t.Errorf("daemon.AddComment must not be called when unauthenticated, calls=%d", f.addCommentCalls)
	}
}

// TestAddComment_HappyPath posts a comment, verifies the daemon was called
// with the authenticated email, and that the response body is a fragment
// (not a full page) containing the new comment.
func TestAddComment_HappyPath(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{"text": {"Looks great, shipping it."}}
	req := authedPost("/b/gd-aaa/comments", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.addCommentCalls != 1 {
		t.Errorf("expected 1 AddComment call, got %d", f.addCommentCalls)
	}
	if f.addCommentLastID != "gd-aaa" {
		t.Errorf("expected bead id gd-aaa, got %q", f.addCommentLastID)
	}
	if f.addCommentLastAuth != authorEmail {
		t.Errorf("author = %q, want %q", f.addCommentLastAuth, authorEmail)
	}
	if f.addCommentLastText != "Looks great, shipping it." {
		t.Errorf("text = %q", f.addCommentLastText)
	}

	body := w.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, "<html") {
		t.Errorf("response must be a partial, not a full page\nbody=%s", body)
	}
	for _, want := range []string{
		`id="comments-section"`,
		// Re-rendered list now contains the seed comment (1) plus the new one (2).
		"<h2>Comments (2)</h2>",
		"Looks great, shipping it.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("post-write fragment missing %q\nbody=%s", want, body)
		}
	}
}

// TestAddComment_StampsAuthorAsXActor is the viewer half of the
// comment-authorship attribution chain (ka-0000006262). The daemon ignores the
// request-body Author and derives comment.Author from the auth identity,
// honoring the X-Actor override only for primary-static-token callers (proven
// daemon-side by kbeads server.TestAddComment_XActorOverridesStaticSubject).
// This test proves the viewer supplies that override: handleAddComment must
// attach the logged-in email via beadsapi.WithActor so the daemon client sends
// X-Actor: <email>. Without the WithActor stamp the daemon would fall back to
// "static" and the comment_write.spec.js author assertion would go false-green.
func TestAddComment_StampsAuthorAsXActor(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{"text": {"attribute me"}}
	req := authedPost("/b/gd-aaa/comments", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.addCommentLastCtx == nil {
		t.Fatal("AddComment was not called; no context captured")
	}
	// The context handed to the daemon client must carry the authenticated
	// email as the per-request actor — that is what becomes X-Actor on the
	// wire and, for a primary-static-token caller, comment.Author.
	if got := beadsapi.ActorFromContext(f.addCommentLastCtx); got != authorEmail {
		t.Errorf("WithActor(ctx) = %q, want %q — viewer must forward the logged-in email as X-Actor so the daemon attributes the comment to the human, not \"static\"", got, authorEmail)
	}
}

// TestAddComment_TrimsAndRejectsEmpty verifies whitespace-only payloads return
// 400 with an inline error and never call the daemon.
func TestAddComment_TrimsAndRejectsEmpty(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	form := url.Values{"text": {"   \t\n  "}}
	req := authedPost("/b/gd-aaa/comments", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty comment, got %d", w.Code)
	}
	if f.addCommentCalls != 0 {
		t.Errorf("daemon.AddComment must not be called for empty text, calls=%d", f.addCommentCalls)
	}
	if !strings.Contains(w.Body.String(), "Comment cannot be empty") {
		t.Errorf("expected inline empty-comment error, body=%s", w.Body.String())
	}
}

// TestAddComment_OversizedReturns413 verifies oversized payloads are rejected
// at the handler before reaching the daemon, with the user's draft preserved
// in the re-rendered form.
func TestAddComment_OversizedReturns413(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	big := strings.Repeat("x", maxCommentLen+1)
	form := url.Values{"text": {big}}
	req := authedPost("/b/gd-aaa/comments", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized comment, got %d", w.Code)
	}
	if f.addCommentCalls != 0 {
		t.Errorf("daemon.AddComment must not be called for oversized text, calls=%d", f.addCommentCalls)
	}
	if !strings.Contains(w.Body.String(), "too long") {
		t.Errorf("expected oversized error in body=%s", w.Body.String())
	}
}

// TestAddComment_DaemonErrorReturns502 verifies a daemon-side failure surfaces
// as 502 and re-renders the form with an inline error and preserved draft.
func TestAddComment_DaemonErrorReturns502(t *testing.T) {
	f := newFake()
	f.addCommentErr = errors.New("daemon unavailable")
	mux := mkServer(t, f)

	form := url.Values{"text": {"draft I want preserved"}}
	req := authedPost("/b/gd-aaa/comments", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 on daemon error, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to post comment") {
		t.Errorf("expected inline daemon-error message, body=%s", body)
	}
	if !strings.Contains(body, "draft I want preserved") {
		t.Errorf("draft text must be preserved in form on error, body=%s", body)
	}
}

// TestStaticAsset_ServesPreviewPopover verifies templates/static/preview_popover.js
// (the bead-link hover preview controller) is reachable at
// /assets/static/preview_popover.js. See bead ka-0000005464.
func TestStaticAsset_ServesPreviewPopover(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/preview_popover.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for preview_popover.js, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"bead-preview-popover",
		"BEADS_VIEWER_BASE_PATH",
		"/preview",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview_popover.js missing %q, first 200 bytes: %q", want, body[:min(200, len(body))])
		}
	}
	if strings.HasPrefix(body, "<!DOCTYPE") {
		t.Errorf("preview_popover.js served as HTML page, got: %q", body[:min(200, len(body))])
	}
}

// TestLayout_IncludesPreviewPopoverScript ensures every rendered page loads
// the bead-link hover preview controller. The script is delegated on
// document so it must be live on every page that can render bead links.
func TestLayout_IncludesPreviewPopoverScript(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)
	for _, path := range []string{"/", "/gd-aaa", "/p/gardenio", "/active", "/search"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		body := w.Body.String()
		if !strings.Contains(body, `src="/assets/static/preview_popover.js"`) {
			t.Errorf("%s: layout missing preview_popover.js script tag", path)
		}
	}
}

// TestLayout_EdgeReauthGuard ensures every rendered page carries the edge-auth
// recovery guard. When the AWS Verified Access session lapses, the edge answers
// with a 303 to GitLab OAuth: a GET navigation re-auths transparently, but an
// htmx XHR cannot read the cross-origin OAuth response and fails with status 0
// (htmx:sendError), surfacing as a raw "NetworkError" on writes such as
// resolving a decision. The guard reloads to re-auth. The original guard only
// caught status 401 (Keycloak/Traefik ForwardAuth); this asserts the broader
// AVA coverage so it can't silently regress (ka-0000006907, sibling ka-0000005477).
func TestLayout_EdgeReauthGuard(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	mux := mkServer(t, f)
	// The guard is delegated on document, so it must be present on every page.
	for _, path := range []string{"/", "/gd-aaa", "/active"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		body := w.Body.String()
		// Must listen for the network-level failure (AVA cross-origin redirect)
		// via sendError, not just the 401 case the original guard handled.
		for _, want := range []string{"htmx:sendError", "htmx:beforeSwap", "reloadForEdgeReauth"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: layout missing edge-reauth guard token %q", path, want)
			}
		}
		// Must reload on status 0 (the AVA failure shape) in addition to 401.
		if !strings.Contains(body, "xhr.status === 0") {
			t.Errorf("%s: edge-reauth guard does not handle status 0 (AVA network failure)", path)
		}
		// The shared SSE self-heal entry point + its mid-edit guard must be defined
		// so the long-lived stream clients can recover an expired edge session.
		for _, want := range []string{"window.bvReauthOnEdgeExpiry", "bvEditingInProgress"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: layout missing SSE edge-reauth helper %q", path, want)
			}
		}
		// The reload must be gated so passive background polls never trigger it —
		// only a real user action (write or click/submit navigation) re-auths
		// (ka-0000004168). The classifier is what enforces that; assert it exists
		// so the gate can't be silently removed.
		if !strings.Contains(body, "bvIsUserActionRequest") {
			t.Errorf("%s: edge-reauth guard missing the user-action gate (bvIsUserActionRequest)", path)
		}
	}
}

// TestSSEClients_SelfHealOnEdgeExpiry pins that the long-lived stream clients
// (SSE and the PTY WebSocket) route their detected auth-expired verdict into
// the shared edge-reauth helper (ka-0000005477). Note the helper now DEFERS the
// re-auth for a passive stream expiry instead of force-reloading mid-read
// (ka-0000004168) — the client's own "session expired — reload" hint/banner is
// the manual fallback and the user's next real action self-heals. This test
// asserts the wiring (detection + the call into the shared policy) is present;
// the defer-vs-reload behavior of the policy itself is covered by
// TestEdgeReauth_GatesPassivePolls. Every file that opens an EventSource or
// WebSocket must be listed here — an unlisted client reconnect-storms against
// the OAuth redirect when the edge session lapses.
func TestSSEClients_SelfHealOnEdgeExpiry(t *testing.T) {
	mux := mkServer(t, newFake())
	for _, asset := range []string{
		"agent_events.js", "agents_stream.js", "telepathy.js",
		"plan_events.js", "molecule_events.js", "feed.js", "agent_terminal.js",
	} {
		req := httptest.NewRequest("GET", "/assets/static/"+asset, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", asset, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "bvReauthOnEdgeExpiry") {
			t.Errorf("%s: SSE client does not call bvReauthOnEdgeExpiry on auth-expired", asset)
		}
		// Sanity: it should still detect auth-expiry via the same-origin probe.
		if !strings.Contains(body, "auth-expired") {
			t.Errorf("%s: SSE client missing auth-expired detection", asset)
		}
	}
}
