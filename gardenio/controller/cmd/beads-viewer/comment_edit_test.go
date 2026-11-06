package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// commentFake builds a fake daemon with one bead and an empty comment list,
// so the comment-preview/post round-trip has a stable input.
func commentFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-cmt1": {
				ID:       "gd-cmt1",
				Title:    "Comment target",
				Type:     "task",
				Status:   "open",
				Priority: 2,
				Labels:   []string{"project:gardenio"},
			},
		},
		comments: map[string][]beadsapi.Comment{
			"gd-cmt1": {},
		},
	}
}

// TestCommentForm_AuthedRendersPreviewPane verifies the new-comment form on
// /gd-cmt1 ships the side-by-side preview pane and the textarea is wired to
// the debounced htmx preview endpoint.
func TestCommentForm_AuthedRendersPreviewPane(t *testing.T) {
	mux := mkServer(t, commentFake())
	req := httptest.NewRequest("GET", "/gd-cmt1", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="comment-preview"`,
		`aria-live="polite"`,
		`aria-label="Comment preview"`,
		`hx-post="/b/gd-cmt1/comments/preview"`,
		`hx-trigger="input changed delay:300ms, load"`,
		`hx-target="#comment-preview"`,
		`bead-description-grid`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment form missing %q\nbody=%s", want, body)
		}
	}
}

// TestCommentForm_HiddenWhenAnonymous verifies anonymous viewers do not see
// the preview pane (since the form itself is gated).
func TestCommentForm_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, commentFake())
	req := httptest.NewRequest("GET", "/gd-cmt1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `hx-post="/b/gd-cmt1/comments/preview"`) {
		t.Errorf("anonymous viewer should not see the comment preview wiring\nbody=%s", body)
	}
}

// TestCommentPreview_HappyPath posts a markdown draft and verifies the preview
// fragment renders the same HTML the saved view would (bold, code, gd-XXX
// linkify all flow through the shared inline pipeline).
func TestCommentPreview_HappyPath(t *testing.T) {
	f := commentFake()
	mux := mkServer(t, f)
	form := url.Values{"text": []string{"Draft with **bold** and `code` and gd-other."}}
	req := authedPost("/b/gd-cmt1/comments/preview", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.addCommentCalls != 0 {
		t.Errorf("preview must not mutate the bead, got %d AddComment calls", f.addCommentCalls)
	}
	body := w.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, "<html") {
		t.Errorf("preview must be a partial, not a full page\nbody=%s", body)
	}
	for _, want := range []string{
		`id="comment-preview"`,
		`aria-live="polite"`,
		`<strong>bold</strong>`,
		`<code>code</code>`,
		// gd-XXX linkify must flow through — same pipeline as saved comments.
		`href="/gd-other"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q\nbody=%s", want, body)
		}
	}
	// The preview swap is read-only — must not echo the textarea.
	if strings.Contains(body, `name="text"`) {
		t.Errorf("preview fragment should not contain the comment textarea\nbody=%s", body)
	}
}

// TestCommentPreview_EmptyDraft renders the empty placeholder so the preview
// pane stays present (and the htmx target keeps resolving) before the operator
// types anything.
func TestCommentPreview_EmptyDraft(t *testing.T) {
	mux := mkServer(t, commentFake())
	form := url.Values{"text": []string{""}}
	req := authedPost("/b/gd-cmt1/comments/preview", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="comment-preview"`) {
		t.Errorf("empty-draft preview should still render the wrapping section\nbody=%s", body)
	}
	if !strings.Contains(body, "&mdash;") {
		t.Errorf("empty-draft preview should render the em-dash placeholder\nbody=%s", body)
	}
}

// TestCommentPreview_Unauthenticated rejects anonymous POSTs with 401 — the
// comment form is gated on the same role and the preview should not leak the
// rendered draft (defense-in-depth aligning the surface with the post gate).
func TestCommentPreview_Unauthenticated(t *testing.T) {
	mux := mkServer(t, commentFake())
	form := url.Values{"text": []string{"hi"}}
	req := httptest.NewRequest("POST", "/b/gd-cmt1/comments/preview", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestCommentPreview_OversizedReturns413 mirrors the save-path cap so the
// preview cannot be used to bypass the size limit on the markdown render.
func TestCommentPreview_OversizedReturns413(t *testing.T) {
	mux := mkServer(t, commentFake())
	big := strings.Repeat("x", maxCommentLen+1)
	form := url.Values{"text": []string{big}}
	req := authedPost("/b/gd-cmt1/comments/preview", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestCommentPreview_MissingBeadID rejects a request with no path id with 400,
// matching the surrounding handlers' pattern.
func TestCommentPreview_MissingBeadID(t *testing.T) {
	srv := NewServer(commentFake(), slog.Default(), "", "", "", nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/b//comments/preview", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	srv.handleCommentPreview(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with empty bead id, got %d", w.Code)
	}
}

// TestCommentPreview_MatchesSavedRender posts a comment with rich markdown,
// then re-runs the same body through the preview endpoint, and checks both
// fragments produce equivalent rendered HTML for the comment body — proving
// the preview matches the saved view (the acceptance-critical parity goal).
//
// Comments use the inline-md instance (`s.mdInline`) so we cannot rely on
// chroma fence-highlighting like the description editor's parity test does;
// instead we exercise the inline features that comments support (bold, code
// span, gd-XXX linkify, lists rendered as inline) and compare the comment
// body fragment from each path.
func TestCommentPreview_MatchesSavedRender(t *testing.T) {
	f := commentFake()
	mux := mkServer(t, f)
	body := "Inline **bold**, `code span`, and gd-other reference."

	saveReq := authedPost("/b/gd-cmt1/comments", url.Values{"text": []string{body}})
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", saveW.Code, saveW.Body.String())
	}

	previewReq := authedPost("/b/gd-cmt1/comments/preview", url.Values{"text": []string{body}})
	previewW := httptest.NewRecorder()
	mux.ServeHTTP(previewW, previewReq)
	if previewW.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d body=%s", previewW.Code, previewW.Body.String())
	}

	// The exact rendered comment-body HTML must match byte-for-byte across
	// the two paths — both go through renderCommentMarkdown(s.mdInline, ...).
	saveInner := extractCommentBody(t, saveW.Body.String())
	previewInner := extractCommentBody(t, previewW.Body.String())
	if saveInner != previewInner {
		t.Errorf("preview render does not match saved render byte-for-byte\nsaved   = %q\npreview = %q", saveInner, previewInner)
	}

	// And sanity-check the rich features are actually rendered (not just both
	// rendering the same empty string).
	for _, want := range []string{
		`<strong>bold</strong>`,
		`<code>code span</code>`,
		`href="/gd-other"`,
	} {
		if !strings.Contains(saveInner, want) {
			t.Errorf("saved comment body missing %q: %s", want, saveInner)
		}
	}
}

// extractCommentBody pulls the contents of the first <div class="comment-body">
// block out of an HTML fragment so the parity test can compare just the
// rendered comment body across the save and preview paths (ignoring outer
// wrappers like aria attrs that differ by design).
func extractCommentBody(t *testing.T, html string) string {
	t.Helper()
	_, after, ok := strings.Cut(html, `<div class="comment-body">`)
	if !ok {
		t.Fatalf("no <div class=\"comment-body\"> in fragment: %s", html)
	}
	inner, _, ok := strings.Cut(after, `</div>`)
	if !ok {
		t.Fatalf("unterminated comment-body div in fragment: %s", html)
	}
	return strings.TrimSpace(inner)
}

// TestAddComment_PreservesDraftWithRenderedPreview verifies that a transient
// daemon failure on the post-comment path re-renders the form with the
// operator's draft AND the matching pre-rendered preview HTML, so the live
// preview pane reflects what they were about to post.
func TestAddComment_PreservesDraftWithRenderedPreview(t *testing.T) {
	f := commentFake()
	f.addCommentErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)
	form := url.Values{"text": []string{"draft with **bold** I want preserved"}}
	req := authedPost("/b/gd-cmt1/comments", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "draft with **bold** I want preserved") {
		t.Errorf("draft should be preserved in re-rendered form, body=%s", body)
	}
	// The preview pane should reflect the draft, not the empty placeholder.
	if !strings.Contains(body, `<strong>bold</strong>`) {
		t.Errorf("preview pane should re-render preserved draft, body=%s", body)
	}
}

// TestCommentsRefresh_RendersEmptyPreview verifies the htmx-driven
// comments-refresh fragment includes an empty preview pane (no draft state on
// the polling path, but the form must still render with a working preview
// target).
func TestCommentsRefresh_RendersEmptyPreview(t *testing.T) {
	mux := mkServer(t, commentFake())
	req := httptest.NewRequest("GET", "/b/gd-cmt1/comments", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="comment-preview"`) {
		t.Errorf("comments-refresh fragment must include the preview target\nbody=%s", body)
	}
	if !strings.Contains(body, "&mdash;") {
		t.Errorf("preview should be empty placeholder on refresh\nbody=%s", body)
	}
}

// Compile-time assertion: keep the time-package import live to satisfy
// linters when other tests in the file shrink — the comment fixture seeds
// timestamps via beadsapi types that import time transitively.
var _ = time.Time{}
