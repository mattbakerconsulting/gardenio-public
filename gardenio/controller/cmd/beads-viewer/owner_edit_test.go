package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// ownerFake builds a fake daemon seeded with one open task bead carrying a
// stable owner string, so the edit/cancel/save round-trip has a known input.
func ownerFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-own1": {
				ID:       "gd-own1",
				Title:    "Owner target",
				Type:     "task",
				Status:   "open",
				Priority: 2,
				Owner:    "alice@example.com",
			},
		},
	}
}

// TestBeadShow_OwnerEditButton_AuthedRendersButton verifies the
// "Edit owner" button appears in the bead-actions toolbar for an
// authenticated operator and points at the edit fragment route.
func TestBeadShow_OwnerEditButton_AuthedRendersButton(t *testing.T) {
	mux := mkServer(t, ownerFake())
	req := httptest.NewRequest("GET", "/gd-own1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-get="/b/gd-own1/owner/edit"`,
		`hx-target="#owner-block"`,
		`Edit owner`,
		`id="owner-block"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in bead-show render\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_OwnerEditButton_HiddenWhenAnonymous verifies anonymous viewers
// do not see the edit affordance — the toolbar gate must fall over for
// non-OIDC requests just like priority/assignee/description/labels.
func TestBeadShow_OwnerEditButton_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, ownerFake())
	req := httptest.NewRequest("GET", "/gd-own1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, `hx-get="/b/gd-own1/owner/edit"`) {
		t.Errorf("anonymous viewer should not see the edit-owner button\nbody: %s", body)
	}
	if !strings.Contains(body, `id="owner-block"`) {
		t.Errorf("anonymous viewer should still see the read-only owner block\nbody: %s", body)
	}
}

// TestOwnerEdit_AuthedRendersForm verifies GET .../owner/edit returns the
// editor partial pre-filled with the current owner.
func TestOwnerEdit_AuthedRendersForm(t *testing.T) {
	mux := mkServer(t, ownerFake())
	req := httptest.NewRequest("GET", "/b/gd-own1/owner/edit", nil)
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
		`id="owner-block"`,
		`hx-post="/b/gd-own1/owner"`,
		`name="owner"`,
		`value="alice@example.com"`,
		`Cancel`,
		`Save`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor partial missing %q\nbody=%s", want, body)
		}
	}
}

// TestOwnerEdit_Unauthenticated rejects anonymous GETs with 401 — defense
// in depth ahead of the OIDC middleware.
func TestOwnerEdit_Unauthenticated(t *testing.T) {
	mux := mkServer(t, ownerFake())
	req := httptest.NewRequest("GET", "/b/gd-own1/owner/edit", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestOwnerView_RendersBlock verifies GET .../owner returns the read-only
// block (used by the editor's Cancel button).
func TestOwnerView_RendersBlock(t *testing.T) {
	mux := mkServer(t, ownerFake())
	req := httptest.NewRequest("GET", "/b/gd-own1/owner", nil)
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
		`id="owner-block"`,
		`alice@example.com`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("block partial missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, `name="owner"`) {
		t.Errorf("read-only block should not contain an owner input\nbody=%s", body)
	}
}

// TestUpdateOwner_HappyPath posts a new owner, verifies the daemon is called
// with the trimmed value, and that the response is the read-only block
// rendered with the new owner.
func TestUpdateOwner_HappyPath(t *testing.T) {
	f := ownerFake()
	mux := mkServer(t, f)
	form := url.Values{"owner": []string{"  bob@example.com  "}}
	req := authedPost("/b/gd-own1/owner", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 update call, got %d", f.updateCalls)
	}
	if f.updateLastID != "gd-own1" {
		t.Errorf("update id = %q, want gd-own1", f.updateLastID)
	}
	if f.updateLastReq.Owner == nil {
		t.Fatalf("expected Owner on update, got nil")
	}
	if got, want := *f.updateLastReq.Owner, "bob@example.com"; got != want {
		t.Errorf("Owner = %q, want %q", got, want)
	}
	if f.updateLastReq.Status != nil || f.updateLastReq.Priority != nil || f.updateLastReq.Assignee != nil || f.updateLastReq.Description != nil || f.updateLastReq.Labels != nil {
		t.Errorf("owner update should not touch other fields, got %+v", f.updateLastReq)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="owner-block"`,
		`bob@example.com`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("post-update fragment missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, `name="owner"`) {
		t.Errorf("happy-path response should be the read-only block, got editor\nbody=%s", body)
	}
}

// TestUpdateOwner_EmptySubmitWritesUnownedSentinel verifies an empty
// submission writes the explicit "system:unowned" sentinel rather than an
// empty string. The sentinel blocks the server-side X-Operated-By default
// (ka-0000000269) from re-populating Owner on the next write, and the
// read-only block re-renders with a "(unowned)" label distinct from the
// em-dash never-set default.
func TestUpdateOwner_EmptySubmitWritesUnownedSentinel(t *testing.T) {
	f := ownerFake()
	mux := mkServer(t, f)
	form := url.Values{"owner": []string{""}}
	req := authedPost("/b/gd-own1/owner", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateLastReq.Owner == nil {
		t.Fatalf("expected non-nil Owner on empty submit, got nil (must distinguish from leave-alone)")
	}
	if got, want := *f.updateLastReq.Owner, unownedSentinel; got != want {
		t.Errorf("expected Owner=%q on empty submit, got %q", want, got)
	}
	body := w.Body.String()
	if !strings.Contains(body, "(unowned)") {
		t.Errorf("empty-submit response should render (unowned) label\nbody=%s", body)
	}
	if !strings.Contains(body, `class="empty owner-unowned"`) {
		t.Errorf("(unowned) label should carry owner-unowned class for distinct styling\nbody=%s", body)
	}
	if strings.Contains(body, `&mdash;`) {
		t.Errorf("explicit-unowned must not render as em-dash (would collide with never-set default)\nbody=%s", body)
	}
}

// TestUpdateOwner_WhitespaceOnlySubmitWritesUnownedSentinel verifies that a
// submission containing only whitespace is treated the same as empty —
// trimmed to "", then written as the sentinel. Without this, a stray space
// would round-trip to the daemon as " " and bypass the sentinel rendering.
func TestUpdateOwner_WhitespaceOnlySubmitWritesUnownedSentinel(t *testing.T) {
	f := ownerFake()
	mux := mkServer(t, f)
	form := url.Values{"owner": []string{"   "}}
	req := authedPost("/b/gd-own1/owner", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateLastReq.Owner == nil {
		t.Fatalf("expected non-nil Owner, got nil")
	}
	if got, want := *f.updateLastReq.Owner, unownedSentinel; got != want {
		t.Errorf("expected Owner=%q on whitespace-only submit, got %q", want, got)
	}
}

// TestOwnerView_RendersUnownedSentinelDistinctly verifies that a bead whose
// Owner is the system:unowned sentinel renders "(unowned)" — distinct from
// the em-dash empty-state used for beads whose Owner has never been set.
func TestOwnerView_RendersUnownedSentinelDistinctly(t *testing.T) {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-unowned": {
				ID:     "gd-unowned",
				Title:  "Explicit unowned",
				Type:   "task",
				Status: "open",
				Owner:  unownedSentinel,
			},
			"gd-never-set": {
				ID:     "gd-never-set",
				Title:  "Never set",
				Type:   "task",
				Status: "open",
				Owner:  "",
			},
		},
	}
	mux := mkServer(t, f)

	reqUnowned := httptest.NewRequest("GET", "/b/gd-unowned/owner", nil)
	wUnowned := httptest.NewRecorder()
	mux.ServeHTTP(wUnowned, reqUnowned)
	if wUnowned.Code != http.StatusOK {
		t.Fatalf("unowned: expected 200, got %d", wUnowned.Code)
	}
	bodyUnowned := wUnowned.Body.String()
	if !strings.Contains(bodyUnowned, "(unowned)") {
		t.Errorf("unowned bead should render (unowned) label\nbody=%s", bodyUnowned)
	}
	if strings.Contains(bodyUnowned, `&mdash;`) {
		t.Errorf("unowned bead must not render em-dash (would collide with never-set)\nbody=%s", bodyUnowned)
	}

	reqNever := httptest.NewRequest("GET", "/b/gd-never-set/owner", nil)
	wNever := httptest.NewRecorder()
	mux.ServeHTTP(wNever, reqNever)
	bodyNever := wNever.Body.String()
	if !strings.Contains(bodyNever, `&mdash;`) {
		t.Errorf("never-set bead should render em-dash default\nbody=%s", bodyNever)
	}
	if strings.Contains(bodyNever, "(unowned)") {
		t.Errorf("never-set bead must not render (unowned) label\nbody=%s", bodyNever)
	}
}

// TestUpdateOwner_Unauthenticated rejects POSTs without an auth header with
// 401, defense-in-depth ahead of the OIDC middleware.
func TestUpdateOwner_Unauthenticated(t *testing.T) {
	f := ownerFake()
	mux := mkServer(t, f)
	form := url.Values{"owner": []string{"bob@example.com"}}
	req := httptest.NewRequest("POST", "/b/gd-own1/owner", strings.NewReader(form.Encode()))
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

// TestUpdateOwner_NotFound surfaces a missing-bead error from the daemon as
// 404 rather than rendering the editor.
func TestUpdateOwner_NotFound(t *testing.T) {
	mux := mkServer(t, ownerFake())
	form := url.Values{"owner": []string{"bob@example.com"}}
	req := authedPost("/b/gd-missing/owner", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestUpdateOwner_DaemonError_RendersInline verifies a daemon-side failure
// surfaces as 502 with the editor re-rendered, the inline error message, and
// the operator's draft preserved so they can retry.
func TestUpdateOwner_DaemonError_RendersInline(t *testing.T) {
	f := ownerFake()
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)
	form := url.Values{"owner": []string{"draft-preserved@example.com"}}
	req := authedPost("/b/gd-own1/owner", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to update owner") {
		t.Errorf("expected inline error, body=%s", body)
	}
	if !strings.Contains(body, "draft-preserved@example.com") {
		t.Errorf("draft should be preserved in re-rendered editor, body=%s", body)
	}
	if !strings.Contains(body, `hx-post="/b/gd-own1/owner"`) {
		t.Errorf("error fragment should re-render the editor for retry\nbody=%s", body)
	}
}

// TestUpdateOwner_OversizedReturns413 rejects payloads larger than the inline
// editor cap with 413 and skips the daemon call.
func TestUpdateOwner_OversizedReturns413(t *testing.T) {
	f := ownerFake()
	mux := mkServer(t, f)
	big := strings.Repeat("x", maxOwnerLen+1)
	form := url.Values{"owner": []string{big}}
	req := authedPost("/b/gd-own1/owner", form)
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

// TestOwnerEdit_RoundTrip simulates the popover open/save round trip against
// the same server: open the editor, post a new value, fetch the bead show —
// the new content must reflect the saved value end to end.
func TestOwnerEdit_RoundTrip(t *testing.T) {
	mux := mkServer(t, ownerFake())

	// 1. Open the editor.
	openReq := httptest.NewRequest("GET", "/b/gd-own1/owner/edit", nil)
	openReq.Header.Set("X-Auth-Request-Email", authorEmail)
	openW := httptest.NewRecorder()
	mux.ServeHTTP(openW, openReq)
	if openW.Code != http.StatusOK {
		t.Fatalf("open: expected 200, got %d", openW.Code)
	}
	if !strings.Contains(openW.Body.String(), `value="alice@example.com"`) {
		t.Errorf("open: editor should pre-fill current owner\nbody=%s", openW.Body.String())
	}

	// 2. Save a new value.
	form := url.Values{"owner": []string{"carol@example.com"}}
	saveReq := authedPost("/b/gd-own1/owner", form)
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d", saveW.Code)
	}
	if !strings.Contains(saveW.Body.String(), "carol@example.com") {
		t.Errorf("save: response should reflect new owner\nbody=%s", saveW.Body.String())
	}

	// 3. Re-render the bead show — the read-only block should now show the
	//    new owner and not the original.
	showReq := httptest.NewRequest("GET", "/gd-own1", nil)
	showReq.Header.Set("X-Auth-Request-Email", authorEmail)
	showW := httptest.NewRecorder()
	mux.ServeHTTP(showW, showReq)
	body := showW.Body.String()
	if !strings.Contains(body, "carol@example.com") {
		t.Errorf("show: persisted owner missing on full page render\nbody=%s", body)
	}
	if strings.Contains(body, "alice@example.com") {
		t.Errorf("show: previous owner should be replaced\nbody=%s", body)
	}
}
