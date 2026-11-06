package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// descFake builds a fake daemon seeded with one open task bead carrying a
// markdown description, so the edit/cancel/save round-trip has a stable input.
func descFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-desc1": {
				ID:          "gd-desc1",
				Title:       "Description target",
				Type:        "task",
				Status:      "open",
				Priority:    2,
				Description: "Original body with `code`.",
				Labels:      []string{"project:gardenio"},
			},
		},
	}
}

// TestBeadShow_DescriptionEditButton_AuthedRendersButton verifies the
// "Edit description" button appears in the bead-actions toolbar for an
// authenticated operator and points at the edit fragment route.
func TestBeadShow_DescriptionEditButton_AuthedRendersButton(t *testing.T) {
	mux := mkServer(t, descFake())
	req := httptest.NewRequest("GET", "/gd-desc1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-get="/b/gd-desc1/description/edit"`,
		`hx-target="#description-block"`,
		`Edit description`,
		`id="description-block"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in bead-show render\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_DescriptionEditButton_HiddenWhenAnonymous verifies anonymous
// viewers don't see the edit affordance — the toolbar gate must fall over for
// non-OIDC requests just like priority/assignee.
func TestBeadShow_DescriptionEditButton_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, descFake())
	req := httptest.NewRequest("GET", "/gd-desc1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, `hx-get="/b/gd-desc1/description/edit"`) {
		t.Errorf("anonymous viewer should not see the edit-description button\nbody: %s", body)
	}
	// The read-only block is still emitted for everyone.
	if !strings.Contains(body, `id="description-block"`) {
		t.Errorf("anonymous viewer should still see the read-only description block\nbody: %s", body)
	}
}

// TestDescriptionEdit_AuthedRendersForm verifies GET .../description/edit
// returns the editor partial pre-filled with the current description.
func TestDescriptionEdit_AuthedRendersForm(t *testing.T) {
	mux := mkServer(t, descFake())
	req := httptest.NewRequest("GET", "/b/gd-desc1/description/edit", nil)
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
		`id="description-block"`,
		`hx-post="/b/gd-desc1/description"`,
		`name="description"`,
		`Original body with ` + "`" + `code` + "`" + `.`,
		`Cancel`,
		`Save`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor partial missing %q\nbody=%s", want, body)
		}
	}
}

// TestDescriptionEdit_Unauthenticated rejects anonymous GETs with 401 — the
// editor would otherwise leak nothing dangerous, but the toolbar is also
// gated on the same role, so defense-in-depth keeps the surfaces aligned.
func TestDescriptionEdit_Unauthenticated(t *testing.T) {
	mux := mkServer(t, descFake())
	req := httptest.NewRequest("GET", "/b/gd-desc1/description/edit", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestDescriptionView_RendersBlock verifies GET .../description returns the
// read-only block (used by the editor's Cancel button).
func TestDescriptionView_RendersBlock(t *testing.T) {
	mux := mkServer(t, descFake())
	req := httptest.NewRequest("GET", "/b/gd-desc1/description", nil)
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
		`id="description-block"`,
		// Markdown was rendered to <code>code</code>.
		`<code>code</code>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("block partial missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, `name="description"`) {
		t.Errorf("read-only block should not contain a textarea\nbody=%s", body)
	}
}

// TestUpdateDescription_HappyPath posts a new description, verifies the daemon
// is called with the new value, and that the response is the read-only block
// rendered with the new markdown.
func TestUpdateDescription_HappyPath(t *testing.T) {
	f := descFake()
	mux := mkServer(t, f)
	form := url.Values{"description": []string{"New body with **bold**."}}
	req := authedPost("/b/gd-desc1/description", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 update call, got %d", f.updateCalls)
	}
	if f.updateLastID != "gd-desc1" {
		t.Errorf("update id = %q, want gd-desc1", f.updateLastID)
	}
	if f.updateLastReq.Description == nil || *f.updateLastReq.Description != "New body with **bold**." {
		t.Errorf("expected new description on update, got %+v", f.updateLastReq.Description)
	}
	if f.updateLastReq.Status != nil || f.updateLastReq.Priority != nil || f.updateLastReq.Assignee != nil {
		t.Errorf("description update should not touch other fields, got %+v", f.updateLastReq)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="description-block"`,
		// Bold markdown was rendered.
		`<strong>bold</strong>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("post-update fragment missing %q\nbody=%s", want, body)
		}
	}
	// No editor on success — the swap returns the read-only block.
	if strings.Contains(body, `name="description"`) {
		t.Errorf("happy-path response should be the read-only block, got editor\nbody=%s", body)
	}
}

// TestUpdateDescription_AllowsEmpty verifies clearing the description is a
// supported mutation (mirrors the empty-string clear path on assignee).
func TestUpdateDescription_AllowsEmpty(t *testing.T) {
	f := descFake()
	mux := mkServer(t, f)
	form := url.Values{"description": []string{""}}
	req := authedPost("/b/gd-desc1/description", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateLastReq.Description == nil || *f.updateLastReq.Description != "" {
		t.Errorf("expected empty description on clear, got %+v", f.updateLastReq.Description)
	}
}

// TestUpdateDescription_Unauthenticated rejects POSTs without an auth header
// with 401, defense-in-depth ahead of the OIDC middleware.
func TestUpdateDescription_Unauthenticated(t *testing.T) {
	f := descFake()
	mux := mkServer(t, f)
	form := url.Values{"description": []string{"hi"}}
	req := httptest.NewRequest("POST", "/b/gd-desc1/description", strings.NewReader(form.Encode()))
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

// TestUpdateDescription_NotFound surfaces a missing-bead error from the
// daemon as 404 rather than rendering the editor.
func TestUpdateDescription_NotFound(t *testing.T) {
	mux := mkServer(t, descFake())
	form := url.Values{"description": []string{"hi"}}
	req := authedPost("/b/gd-missing/description", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestUpdateDescription_DaemonError_RendersInline verifies a daemon-side
// failure surfaces as 502 with the editor re-rendered, the inline error
// message, and the operator's draft preserved so they can retry.
func TestUpdateDescription_DaemonError_RendersInline(t *testing.T) {
	f := descFake()
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)
	form := url.Values{"description": []string{"draft I want preserved"}}
	req := authedPost("/b/gd-desc1/description", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to update description") {
		t.Errorf("expected inline error, body=%s", body)
	}
	if !strings.Contains(body, "draft I want preserved") {
		t.Errorf("draft should be preserved in re-rendered editor, body=%s", body)
	}
	if !strings.Contains(body, `hx-post="/b/gd-desc1/description"`) {
		t.Errorf("error fragment should re-render the editor for retry\nbody=%s", body)
	}
}

// TestUpdateDescription_OversizedReturns413 rejects payloads larger than the
// inline editor cap with 413, and re-renders the editor with the draft so the
// operator can trim it.
func TestUpdateDescription_OversizedReturns413(t *testing.T) {
	f := descFake()
	mux := mkServer(t, f)
	big := strings.Repeat("x", maxDescriptionLen+1)
	form := url.Values{"description": []string{big}}
	req := authedPost("/b/gd-desc1/description", form)
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

// TestDescriptionEdit_RendersInitialPreviewPane verifies the editor partial
// embeds a server-side rendered preview pane so the operator opens the editor
// to a representative render before typing — same goldmark + linkify pipeline
// the read view uses.
func TestDescriptionEdit_RendersInitialPreviewPane(t *testing.T) {
	mux := mkServer(t, descFake())
	req := httptest.NewRequest("GET", "/b/gd-desc1/description/edit", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="description-preview"`,
		`aria-live="polite"`,
		// Initial preview is rendered server-side from the saved markdown,
		// proving parity with the read view (the same `code` span renders).
		`<code>code</code>`,
		// Textarea drives the live preview on input (debounced 300ms).
		`hx-post="/b/gd-desc1/description/preview"`,
		`hx-trigger="input changed delay:300ms, load"`,
		`hx-target="#description-preview"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor partial missing %q\nbody=%s", want, body)
		}
	}
}

// TestDescriptionPreview_HappyPath posts a markdown draft and verifies the
// preview fragment renders the same HTML the saved view would (bold, code,
// gd-XXX linkify all flow through the shared pipeline).
func TestDescriptionPreview_HappyPath(t *testing.T) {
	f := descFake()
	mux := mkServer(t, f)
	form := url.Values{"description": []string{"Draft with **bold** and `code` and gd-other."}}
	req := authedPost("/b/gd-desc1/description/preview", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 0 {
		t.Errorf("preview must not mutate the bead, got %d update calls", f.updateCalls)
	}
	body := w.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, "<html") {
		t.Errorf("preview must be a partial, not a full page\nbody=%s", body)
	}
	for _, want := range []string{
		`id="description-preview"`,
		`aria-live="polite"`,
		`<strong>bold</strong>`,
		`<code>code</code>`,
		// gd-XXX linkify must flow through — same pipeline as read view.
		`href="/gd-other"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q\nbody=%s", want, body)
		}
	}
	// The preview swap is read-only — must not echo the textarea.
	if strings.Contains(body, `name="description"`) {
		t.Errorf("preview fragment should not contain the editor textarea\nbody=%s", body)
	}
}

// TestDescriptionPreview_EmptyDraft renders the empty placeholder so the
// preview pane stays present (and the htmx target keeps resolving) even
// before the operator types anything.
func TestDescriptionPreview_EmptyDraft(t *testing.T) {
	mux := mkServer(t, descFake())
	form := url.Values{"description": []string{""}}
	req := authedPost("/b/gd-desc1/description/preview", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="description-preview"`) {
		t.Errorf("empty-draft preview should still render the wrapping section\nbody=%s", body)
	}
	if !strings.Contains(body, "&mdash;") {
		t.Errorf("empty-draft preview should render the em-dash placeholder\nbody=%s", body)
	}
}

// TestDescriptionPreview_Unauthenticated rejects anonymous POSTs with 401 —
// the editor is gated on the same role and the preview should not leak the
// rendered draft (mainly aligning the surface with the edit gate).
func TestDescriptionPreview_Unauthenticated(t *testing.T) {
	mux := mkServer(t, descFake())
	form := url.Values{"description": []string{"hi"}}
	req := httptest.NewRequest("POST", "/b/gd-desc1/description/preview", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestDescriptionPreview_OversizedReturns413 mirrors the save-path cap so the
// preview cannot be used to bypass the size limit on the markdown render.
func TestDescriptionPreview_OversizedReturns413(t *testing.T) {
	mux := mkServer(t, descFake())
	big := strings.Repeat("x", maxDescriptionLen+1)
	form := url.Values{"description": []string{big}}
	req := authedPost("/b/gd-desc1/description/preview", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestDescriptionPreview_MissingBeadID rejects a request with no path id with
// 400, matching the surrounding handlers' pattern (404s are reserved for
// daemon misses; here the route is simply malformed).
func TestDescriptionPreview_MissingBeadID(t *testing.T) {
	// net/http.ServeMux requires a non-empty path segment for `{id}`; we
	// hit the handler directly to verify its own input validation rather
	// than relying on the mux to enforce non-empty segments.
	srv := NewServer(descFake(), slog.Default(), "", "", "", nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/b//description/preview", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	srv.handleDescriptionPreview(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with empty bead id, got %d", w.Code)
	}
}

// TestDescriptionPreview_MatchesSavedRender saves a description, then re-runs
// the same body through the preview endpoint, and checks both fragments
// produce equivalent rendered HTML for the description body — proving the
// preview matches the saved view (the acceptance-critical parity goal).
func TestDescriptionPreview_MatchesSavedRender(t *testing.T) {
	mux := mkServer(t, descFake())
	body := "Heading\n\n- one\n- two\n\n```go\nfmt.Println(\"hi\")\n```\n\nSee gd-other for context."

	saveReq := authedPost("/b/gd-desc1/description", url.Values{"description": []string{body}})
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d body=%s", saveW.Code, saveW.Body.String())
	}

	previewReq := authedPost("/b/gd-desc1/description/preview", url.Values{"description": []string{body}})
	previewW := httptest.NewRecorder()
	mux.ServeHTTP(previewW, previewReq)
	if previewW.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d body=%s", previewW.Code, previewW.Body.String())
	}

	// Markdown features that must flow through identically on both paths.
	for _, want := range []string{
		">one</li>", // <li> now carries a data-block-id (crit-critique T1)
		">two</li>",
		`href="/gd-other"`,
		// goldmark + chroma highlight wraps the `go` fence in a <pre>
		// carrying inline color styles (github-dark theme) — its presence
		// in both fragments is the signal the same pipeline is wired through.
		`background-color:#0d1117`,
	} {
		if !strings.Contains(saveW.Body.String(), want) {
			t.Errorf("save fragment missing %q\nbody=%s", want, saveW.Body.String())
		}
		if !strings.Contains(previewW.Body.String(), want) {
			t.Errorf("preview fragment missing %q\nbody=%s", want, previewW.Body.String())
		}
	}
}

// TestDescriptionEdit_RoundTrip simulates the popover open/save round trip
// against the same server: open the editor, post a new value, fetch the
// description block — the new content must reflect the saved value end to end.
// Mirrors the inline-popover round-trip pattern used by priority/assignee.
func TestDescriptionEdit_RoundTrip(t *testing.T) {
	mux := mkServer(t, descFake())

	// 1. Open the editor.
	openReq := httptest.NewRequest("GET", "/b/gd-desc1/description/edit", nil)
	openReq.Header.Set("X-Auth-Request-Email", authorEmail)
	openW := httptest.NewRecorder()
	mux.ServeHTTP(openW, openReq)
	if openW.Code != http.StatusOK {
		t.Fatalf("open: expected 200, got %d", openW.Code)
	}
	if !strings.Contains(openW.Body.String(), "Original body") {
		t.Errorf("open: editor should pre-fill current description\nbody=%s", openW.Body.String())
	}

	// 2. Save a new value.
	form := url.Values{"description": []string{"Round-tripped body."}}
	saveReq := authedPost("/b/gd-desc1/description", form)
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d", saveW.Code)
	}
	if !strings.Contains(saveW.Body.String(), "Round-tripped body.") {
		t.Errorf("save: response should reflect new description\nbody=%s", saveW.Body.String())
	}

	// 3. Re-render the bead show — the read-only block should now show the new value.
	showReq := httptest.NewRequest("GET", "/gd-desc1", nil)
	showReq.Header.Set("X-Auth-Request-Email", authorEmail)
	showW := httptest.NewRecorder()
	mux.ServeHTTP(showW, showReq)
	if !strings.Contains(showW.Body.String(), "Round-tripped body.") {
		t.Errorf("show: persisted description missing on full page render\nbody=%s", showW.Body.String())
	}
	if strings.Contains(showW.Body.String(), "Original body") {
		t.Errorf("show: original description should be replaced\nbody=%s", showW.Body.String())
	}
}
