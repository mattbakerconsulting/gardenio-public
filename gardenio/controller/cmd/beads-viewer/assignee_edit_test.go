package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// assigneeFake builds a fake daemon seeded with one open task bead carrying a
// stable assignee, so the edit/cancel/save round-trip has a known input.
func assigneeFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-asg1": {
				ID:       "gd-asg1",
				Title:    "Assignee target",
				Type:     "task",
				Status:   "open",
				Priority: 2,
				Assignee: "alice@example.com",
			},
		},
	}
}

// TestBeadShow_AssigneeEditButton_AuthedRendersButton verifies the
// "Edit assignee" button appears in the bead-actions toolbar for an
// authenticated operator and points at the edit fragment route.
func TestBeadShow_AssigneeEditButton_AuthedRendersButton(t *testing.T) {
	mux := mkServer(t, assigneeFake())
	req := httptest.NewRequest("GET", "/gd-asg1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-get="/b/gd-asg1/assignee/edit"`,
		`hx-target="#assignee-block"`,
		`Edit assignee`,
		`id="assignee-block"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in bead-show render\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_AssigneeEditButton_HiddenWhenAnonymous verifies anonymous
// viewers do not see the edit affordance — the toolbar gate must fall over
// for non-OIDC requests.
func TestBeadShow_AssigneeEditButton_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, assigneeFake())
	req := httptest.NewRequest("GET", "/gd-asg1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, `hx-get="/b/gd-asg1/assignee/edit"`) {
		t.Errorf("anonymous viewer should not see the edit-assignee button\nbody: %s", body)
	}
	if !strings.Contains(body, `id="assignee-block"`) {
		t.Errorf("anonymous viewer should still see the read-only assignee block\nbody: %s", body)
	}
}

// TestAssigneeEdit_AuthedRendersForm verifies GET .../assignee/edit returns
// the editor partial pre-filled with the current assignee.
func TestAssigneeEdit_AuthedRendersForm(t *testing.T) {
	mux := mkServer(t, assigneeFake())
	req := httptest.NewRequest("GET", "/b/gd-asg1/assignee/edit", nil)
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
		`id="assignee-block"`,
		`hx-post="/b/gd-asg1/assignee"`,
		`name="assignee"`,
		`value="alice@example.com"`,
		`Cancel`,
		`Save`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor partial missing %q\nbody=%s", want, body)
		}
	}
}

// TestAssigneeEdit_Unauthenticated rejects anonymous GETs with 401 — defense
// in depth ahead of the OIDC middleware.
func TestAssigneeEdit_Unauthenticated(t *testing.T) {
	mux := mkServer(t, assigneeFake())
	req := httptest.NewRequest("GET", "/b/gd-asg1/assignee/edit", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestAssigneeView_RendersBlock verifies GET .../assignee returns the
// read-only block (used by the editor's Cancel button).
func TestAssigneeView_RendersBlock(t *testing.T) {
	mux := mkServer(t, assigneeFake())
	req := httptest.NewRequest("GET", "/b/gd-asg1/assignee", nil)
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
		`id="assignee-block"`,
		`alice@example.com`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("block partial missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, `name="assignee"`) {
		t.Errorf("read-only block should not contain an assignee input\nbody=%s", body)
	}
}

// TestUpdateAssignee_HappyPath posts a new assignee, verifies the daemon is
// called with the trimmed value, and that the response is the read-only
// block rendered with the new assignee.
func TestUpdateAssignee_HappyPath(t *testing.T) {
	f := assigneeFake()
	mux := mkServer(t, f)
	form := url.Values{"assignee": []string{"  bob@example.com  "}}
	req := authedPost("/b/gd-asg1/assignee", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 update call, got %d", f.updateCalls)
	}
	if f.updateLastID != "gd-asg1" {
		t.Errorf("update id = %q, want gd-asg1", f.updateLastID)
	}
	if f.updateLastReq.Assignee == nil {
		t.Fatalf("expected Assignee on update, got nil")
	}
	if got, want := *f.updateLastReq.Assignee, "bob@example.com"; got != want {
		t.Errorf("Assignee = %q, want %q", got, want)
	}
	if f.updateLastReq.Status != nil || f.updateLastReq.Priority != nil || f.updateLastReq.Description != nil || f.updateLastReq.Labels != nil || f.updateLastReq.Owner != nil {
		t.Errorf("assignee update should not touch other fields, got %+v", f.updateLastReq)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="assignee-block"`,
		`bob@example.com`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("post-update fragment missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, `name="assignee"`) {
		t.Errorf("happy-path response should be the read-only block, got editor\nbody=%s", body)
	}
}

// TestUpdateAssignee_EmptyClears verifies an empty submission clears the
// assignee — the form contract is "submit empty to clear".
func TestUpdateAssignee_EmptyClears(t *testing.T) {
	f := assigneeFake()
	mux := mkServer(t, f)
	form := url.Values{"assignee": []string{"   "}}
	req := authedPost("/b/gd-asg1/assignee", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateLastReq.Assignee == nil {
		t.Fatalf("expected non-nil Assignee on clear, got nil (must distinguish clear from leave-alone)")
	}
	if got := *f.updateLastReq.Assignee; got != "" {
		t.Errorf("expected empty assignee on clear, got %q", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="empty"`) {
		t.Errorf("cleared assignee should render empty placeholder\nbody=%s", body)
	}
}

// TestUpdateAssignee_Unauthenticated rejects POSTs without an auth header
// with 401, defense-in-depth ahead of the OIDC middleware.
func TestUpdateAssignee_Unauthenticated(t *testing.T) {
	f := assigneeFake()
	mux := mkServer(t, f)
	form := url.Values{"assignee": []string{"bob@example.com"}}
	req := httptest.NewRequest("POST", "/b/gd-asg1/assignee", strings.NewReader(form.Encode()))
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

// TestUpdateAssignee_NotFound surfaces a missing-bead error from the daemon
// as 404 rather than rendering the editor.
func TestUpdateAssignee_NotFound(t *testing.T) {
	mux := mkServer(t, assigneeFake())
	form := url.Values{"assignee": []string{"bob@example.com"}}
	req := authedPost("/b/gd-missing/assignee", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestUpdateAssignee_DaemonError_RendersInline verifies a daemon-side failure
// surfaces as 502 with the editor re-rendered, the inline error, and the
// operator's draft preserved so they can retry.
func TestUpdateAssignee_DaemonError_RendersInline(t *testing.T) {
	f := assigneeFake()
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)
	form := url.Values{"assignee": []string{"draft-preserved@example.com"}}
	req := authedPost("/b/gd-asg1/assignee", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to update assignee") {
		t.Errorf("expected inline error, body=%s", body)
	}
	if !strings.Contains(body, "draft-preserved@example.com") {
		t.Errorf("draft should be preserved in re-rendered editor, body=%s", body)
	}
	if !strings.Contains(body, `hx-post="/b/gd-asg1/assignee"`) {
		t.Errorf("error fragment should re-render editor for retry\nbody=%s", body)
	}
}

// TestUpdateAssignee_OversizedReturns413 rejects payloads larger than the
// inline editor cap with 413 and skips the daemon call.
func TestUpdateAssignee_OversizedReturns413(t *testing.T) {
	f := assigneeFake()
	mux := mkServer(t, f)
	big := strings.Repeat("x", maxAssigneeLen+1)
	form := url.Values{"assignee": []string{big}}
	req := authedPost("/b/gd-asg1/assignee", form)
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

// TestAssigneeEdit_RoundTrip simulates the popover open/save round trip
// against the same server: open the editor, post a new value, fetch the
// bead show — the new content must reflect the saved value end to end.
func TestAssigneeEdit_RoundTrip(t *testing.T) {
	mux := mkServer(t, assigneeFake())

	// 1. Open the editor.
	openReq := httptest.NewRequest("GET", "/b/gd-asg1/assignee/edit", nil)
	openReq.Header.Set("X-Auth-Request-Email", authorEmail)
	openW := httptest.NewRecorder()
	mux.ServeHTTP(openW, openReq)
	if openW.Code != http.StatusOK {
		t.Fatalf("open: expected 200, got %d", openW.Code)
	}
	if !strings.Contains(openW.Body.String(), `value="alice@example.com"`) {
		t.Errorf("open: editor should pre-fill current assignee\nbody=%s", openW.Body.String())
	}

	// 2. Save a new value.
	form := url.Values{"assignee": []string{"carol@example.com"}}
	saveReq := authedPost("/b/gd-asg1/assignee", form)
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d", saveW.Code)
	}
	if !strings.Contains(saveW.Body.String(), "carol@example.com") {
		t.Errorf("save: response should reflect new assignee\nbody=%s", saveW.Body.String())
	}

	// 3. Re-render the bead show — the read-only block should now show the
	//    new assignee and not the original.
	showReq := httptest.NewRequest("GET", "/gd-asg1", nil)
	showReq.Header.Set("X-Auth-Request-Email", authorEmail)
	showW := httptest.NewRecorder()
	mux.ServeHTTP(showW, showReq)
	body := showW.Body.String()
	if !strings.Contains(body, "carol@example.com") {
		t.Errorf("show: persisted assignee missing on full page render\nbody=%s", body)
	}
	if strings.Contains(body, "alice@example.com") {
		t.Errorf("show: previous assignee should be replaced\nbody=%s", body)
	}
}
