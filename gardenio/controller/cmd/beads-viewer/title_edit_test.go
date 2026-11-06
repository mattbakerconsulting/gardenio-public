package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// titleFake builds a fake daemon seeded with one open task bead so the
// title edit/cancel/save round-trip has a stable input.
func titleFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-title1": {
				ID:       "gd-title1",
				Title:    "Original title",
				Type:     "task",
				Status:   "open",
				Priority: 2,
				Labels:   []string{"project:gardenio"},
			},
		},
	}
}

// TestBeadShow_TitleEditTrigger_AuthedRendersTrigger verifies the H1 itself
// becomes the inline editor trigger for an authenticated operator.
func TestBeadShow_TitleEditTrigger_AuthedRendersTrigger(t *testing.T) {
	mux := mkServer(t, titleFake())
	req := httptest.NewRequest("GET", "/gd-title1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-get="/b/gd-title1/title/edit"`,
		`hx-target="#title-block"`,
		`id="title-block"`,
		`Original title`,
		`bead-title-editable`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in bead-show render\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_TitleEditTrigger_HiddenWhenAnonymous verifies anonymous
// viewers don't see the edit affordance — the read-only H1 still renders.
func TestBeadShow_TitleEditTrigger_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, titleFake())
	req := httptest.NewRequest("GET", "/gd-title1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, `hx-get="/b/gd-title1/title/edit"`) {
		t.Errorf("anonymous viewer should not see the title-edit trigger\nbody: %s", body)
	}
	if !strings.Contains(body, `id="title-block"`) {
		t.Errorf("anonymous viewer should still see the read-only title block\nbody: %s", body)
	}
	if !strings.Contains(body, "Original title") {
		t.Errorf("anonymous viewer should still see the title text\nbody: %s", body)
	}
}

// TestTitleEdit_AuthedRendersForm verifies GET .../title/edit returns the
// editor partial pre-filled with the current title.
func TestTitleEdit_AuthedRendersForm(t *testing.T) {
	mux := mkServer(t, titleFake())
	req := httptest.NewRequest("GET", "/b/gd-title1/title/edit", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, "<html") {
		t.Errorf("editor must be a partial, not a full page\nbody=%s", body)
	}
	for _, want := range []string{
		`id="title-block"`,
		`hx-post="/b/gd-title1/title"`,
		`name="title"`,
		`value="Original title"`,
		`Cancel`,
		`Save`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor partial missing %q\nbody=%s", want, body)
		}
	}
}

// TestTitleEdit_Unauthenticated rejects anonymous GETs with 401, matching the
// description-edit defense-in-depth gate.
func TestTitleEdit_Unauthenticated(t *testing.T) {
	mux := mkServer(t, titleFake())
	req := httptest.NewRequest("GET", "/b/gd-title1/title/edit", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestTitleView_RendersBlock verifies GET .../title returns the read-only
// title block (used by the editor's Cancel button).
func TestTitleView_RendersBlock(t *testing.T) {
	mux := mkServer(t, titleFake())
	req := httptest.NewRequest("GET", "/b/gd-title1/title", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, "<html") {
		t.Errorf("block must be a partial\nbody=%s", body)
	}
	for _, want := range []string{
		`id="title-block"`,
		`Original title`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("block partial missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, `name="title"`) {
		t.Errorf("read-only block should not contain a form input\nbody=%s", body)
	}
}

// TestUpdateTitle_HappyPath posts a new title, verifies the daemon is called
// with the new value, and that the response is the read-only block.
func TestUpdateTitle_HappyPath(t *testing.T) {
	f := titleFake()
	mux := mkServer(t, f)
	form := url.Values{"title": []string{"New shiny title"}}
	req := authedPost("/b/gd-title1/title", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 update call, got %d", f.updateCalls)
	}
	if f.updateLastID != "gd-title1" {
		t.Errorf("update id = %q, want gd-title1", f.updateLastID)
	}
	if f.updateLastReq.Title == nil || *f.updateLastReq.Title != "New shiny title" {
		t.Errorf("expected new title on update, got %+v", f.updateLastReq.Title)
	}
	if f.updateLastReq.Status != nil || f.updateLastReq.Priority != nil || f.updateLastReq.Assignee != nil || f.updateLastReq.Description != nil {
		t.Errorf("title update should not touch other fields, got %+v", f.updateLastReq)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="title-block"`,
		"New shiny title",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("post-update fragment missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, `name="title"`) {
		t.Errorf("happy-path response should be the read-only block, got editor\nbody=%s", body)
	}
}

// TestUpdateTitle_TrimsWhitespace verifies leading/trailing whitespace on a
// non-empty title is stripped before the daemon call.
func TestUpdateTitle_TrimsWhitespace(t *testing.T) {
	f := titleFake()
	mux := mkServer(t, f)
	form := url.Values{"title": []string{"   Padded title   "}}
	req := authedPost("/b/gd-title1/title", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateLastReq.Title == nil || *f.updateLastReq.Title != "Padded title" {
		t.Errorf("expected trimmed title, got %+v", f.updateLastReq.Title)
	}
}

// TestUpdateTitle_EmptyRejected verifies an empty title is rejected with 400
// and the editor re-renders with the original title preserved.
func TestUpdateTitle_EmptyRejected(t *testing.T) {
	f := titleFake()
	mux := mkServer(t, f)
	form := url.Values{"title": []string{""}}
	req := authedPost("/b/gd-title1/title", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 0 {
		t.Errorf("daemon UpdateBead should not be called for empty title: %d", f.updateCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, "cannot be empty") {
		t.Errorf("expected empty-title error inline, body=%s", body)
	}
	if !strings.Contains(body, `hx-post="/b/gd-title1/title"`) {
		t.Errorf("error fragment should re-render the editor for retry\nbody=%s", body)
	}
	if !strings.Contains(body, `value="Original title"`) {
		t.Errorf("error fragment should restore the original title\nbody=%s", body)
	}
}

// TestUpdateTitle_WhitespaceOnlyRejected verifies a whitespace-only title is
// rejected the same way as empty input.
func TestUpdateTitle_WhitespaceOnlyRejected(t *testing.T) {
	f := titleFake()
	mux := mkServer(t, f)
	form := url.Values{"title": []string{"   \t  "}}
	req := authedPost("/b/gd-title1/title", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 0 {
		t.Errorf("daemon UpdateBead should not be called for whitespace-only: %d", f.updateCalls)
	}
}

// TestUpdateTitle_Unauthenticated rejects POSTs without an auth header with
// 401, defense-in-depth ahead of the OIDC middleware.
func TestUpdateTitle_Unauthenticated(t *testing.T) {
	f := titleFake()
	mux := mkServer(t, f)
	form := url.Values{"title": []string{"hi"}}
	req := httptest.NewRequest("POST", "/b/gd-title1/title", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 0 {
		t.Errorf("daemon UpdateBead should not be called: %d", f.updateCalls)
	}
}

// TestUpdateTitle_NotFound surfaces a missing-bead error from the daemon as
// 404 rather than rendering the editor.
func TestUpdateTitle_NotFound(t *testing.T) {
	mux := mkServer(t, titleFake())
	form := url.Values{"title": []string{"hi"}}
	req := authedPost("/b/gd-missing/title", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestUpdateTitle_DaemonError_RendersInline verifies a daemon-side failure
// surfaces as 502 with the editor re-rendered, the inline error message,
// and the operator's draft preserved so they can retry.
func TestUpdateTitle_DaemonError_RendersInline(t *testing.T) {
	f := titleFake()
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)
	form := url.Values{"title": []string{"draft I want preserved"}}
	req := authedPost("/b/gd-title1/title", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to update title") {
		t.Errorf("expected inline error, body=%s", body)
	}
	if !strings.Contains(body, "draft I want preserved") {
		t.Errorf("draft should be preserved in re-rendered editor, body=%s", body)
	}
	if !strings.Contains(body, `hx-post="/b/gd-title1/title"`) {
		t.Errorf("error fragment should re-render the editor for retry\nbody=%s", body)
	}
}

// TestUpdateTitle_OversizedReturns413 rejects payloads larger than the inline
// editor cap with 413, and re-renders the editor with the draft so the
// operator can trim it.
func TestUpdateTitle_OversizedReturns413(t *testing.T) {
	f := titleFake()
	mux := mkServer(t, f)
	big := strings.Repeat("x", maxTitleLen+1)
	form := url.Values{"title": []string{big}}
	req := authedPost("/b/gd-title1/title", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
	if f.updateCalls != 0 {
		t.Errorf("daemon UpdateBead should not be called for oversized: %d", f.updateCalls)
	}
	if !strings.Contains(w.Body.String(), "too long") {
		t.Errorf("expected too-long error, body=%s", w.Body.String())
	}
}

// TestTitleEdit_RoundTrip simulates the popover open/save round trip against
// the same server: open the editor, post a new value, fetch the title block
// via the show page — the new content must reflect the saved value end-to-end.
func TestTitleEdit_RoundTrip(t *testing.T) {
	mux := mkServer(t, titleFake())

	// 1. Open the editor.
	openReq := httptest.NewRequest("GET", "/b/gd-title1/title/edit", nil)
	openReq.Header.Set("X-Auth-Request-Email", authorEmail)
	openW := httptest.NewRecorder()
	mux.ServeHTTP(openW, openReq)
	if openW.Code != http.StatusOK {
		t.Fatalf("open: expected 200, got %d", openW.Code)
	}
	if !strings.Contains(openW.Body.String(), `value="Original title"`) {
		t.Errorf("open: editor should pre-fill current title\nbody=%s", openW.Body.String())
	}

	// 2. Save a new value.
	form := url.Values{"title": []string{"Round-tripped title"}}
	saveReq := authedPost("/b/gd-title1/title", form)
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d", saveW.Code)
	}
	if !strings.Contains(saveW.Body.String(), "Round-tripped title") {
		t.Errorf("save: response should reflect new title\nbody=%s", saveW.Body.String())
	}

	// 3. Re-render the bead show — the read-only block should now show the new value.
	showReq := httptest.NewRequest("GET", "/gd-title1", nil)
	showReq.Header.Set("X-Auth-Request-Email", authorEmail)
	showW := httptest.NewRecorder()
	mux.ServeHTTP(showW, showReq)
	if !strings.Contains(showW.Body.String(), "Round-tripped title") {
		t.Errorf("show: persisted title missing on full page render\nbody=%s", showW.Body.String())
	}
	if strings.Contains(showW.Body.String(), "Original title") {
		t.Errorf("show: original title should be replaced\nbody=%s", showW.Body.String())
	}
}
