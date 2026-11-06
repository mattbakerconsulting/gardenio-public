package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// priorityFake builds a fake daemon seeded with one open task bead carrying a
// stable priority value, so the edit/cancel/save round-trip has a known input.
func priorityFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-prio1": {
				ID:       "gd-prio1",
				Title:    "Priority target",
				Type:     "task",
				Status:   "open",
				Priority: 2,
			},
		},
	}
}

// TestBeadShow_PriorityEditButton_AuthedRendersButton verifies the
// "Edit priority" button appears in the bead-actions toolbar for an
// authenticated operator and points at the edit fragment route.
func TestBeadShow_PriorityEditButton_AuthedRendersButton(t *testing.T) {
	mux := mkServer(t, priorityFake())
	req := httptest.NewRequest("GET", "/gd-prio1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-get="/b/gd-prio1/priority/edit"`,
		`hx-target="#priority-block"`,
		`Edit priority`,
		`id="priority-block"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in bead-show render\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_PriorityEditButton_HiddenWhenAnonymous verifies anonymous
// viewers do not see the edit affordance — the toolbar gate must fall over
// for non-OIDC requests.
func TestBeadShow_PriorityEditButton_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, priorityFake())
	req := httptest.NewRequest("GET", "/gd-prio1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, `hx-get="/b/gd-prio1/priority/edit"`) {
		t.Errorf("anonymous viewer should not see the edit-priority button\nbody: %s", body)
	}
	if !strings.Contains(body, `id="priority-block"`) {
		t.Errorf("anonymous viewer should still see the read-only priority block\nbody: %s", body)
	}
}

// TestPriorityEdit_AuthedRendersForm verifies GET .../priority/edit returns the
// editor partial pre-filled with the current priority.
func TestPriorityEdit_AuthedRendersForm(t *testing.T) {
	mux := mkServer(t, priorityFake())
	req := httptest.NewRequest("GET", "/b/gd-prio1/priority/edit", nil)
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
		`id="priority-block"`,
		`hx-post="/b/gd-prio1/priority"`,
		`name="priority"`,
		`<option value="2" selected>P2</option>`,
		`<option value="0">P0</option>`,
		`<option value="5">P5</option>`,
		`Cancel`,
		`Save`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor partial missing %q\nbody=%s", want, body)
		}
	}
}

// TestPriorityEdit_Unauthenticated rejects anonymous GETs with 401 — defense
// in depth ahead of the OIDC middleware.
func TestPriorityEdit_Unauthenticated(t *testing.T) {
	mux := mkServer(t, priorityFake())
	req := httptest.NewRequest("GET", "/b/gd-prio1/priority/edit", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestPriorityView_RendersBlock verifies GET .../priority returns the
// read-only block (used by the editor's Cancel button).
func TestPriorityView_RendersBlock(t *testing.T) {
	mux := mkServer(t, priorityFake())
	req := httptest.NewRequest("GET", "/b/gd-prio1/priority", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, "<html") {
		t.Errorf("block must be a partial\nbody=%s", body)
	}
	if !strings.Contains(body, `id="priority-block"`) {
		t.Errorf("block partial missing id=\"priority-block\"\nbody=%s", body)
	}
	if strings.Contains(body, `name="priority"`) {
		t.Errorf("read-only block should not contain a priority select\nbody=%s", body)
	}
}

// TestUpdatePriority_HappyPath posts a new priority, verifies the daemon is
// called with the parsed value, and that the response is the read-only block
// rendered with the new priority.
func TestUpdatePriority_HappyPath(t *testing.T) {
	f := priorityFake()
	mux := mkServer(t, f)
	form := url.Values{"priority": []string{"0"}}
	req := authedPost("/b/gd-prio1/priority", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 update call, got %d", f.updateCalls)
	}
	if f.updateLastID != "gd-prio1" {
		t.Errorf("update id = %q, want gd-prio1", f.updateLastID)
	}
	if f.updateLastReq.Priority == nil {
		t.Fatalf("expected Priority on update, got nil")
	}
	if got, want := *f.updateLastReq.Priority, 0; got != want {
		t.Errorf("Priority = %d, want %d", got, want)
	}
	if f.updateLastReq.Status != nil || f.updateLastReq.Assignee != nil || f.updateLastReq.Description != nil || f.updateLastReq.Labels != nil || f.updateLastReq.Owner != nil {
		t.Errorf("priority update should not touch other fields, got %+v", f.updateLastReq)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="priority-block"`) {
		t.Errorf("post-update fragment missing #priority-block\nbody=%s", body)
	}
	if strings.Contains(body, `name="priority"`) {
		t.Errorf("happy-path response should be the read-only block, got editor\nbody=%s", body)
	}
}

// TestUpdatePriority_InvalidValue_RendersInline rejects out-of-range or
// non-numeric inputs with 400 + an inline error, and preserves the bead's
// original priority in the re-rendered editor.
func TestUpdatePriority_InvalidValue_RendersInline(t *testing.T) {
	cases := []string{"6", "-1", "high", "", "1.5"}
	for _, raw := range cases {
		t.Run("priority="+raw, func(t *testing.T) {
			f := priorityFake()
			mux := mkServer(t, f)
			form := url.Values{"priority": []string{raw}}
			req := authedPost("/b/gd-prio1/priority", form)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			if f.updateCalls != 0 {
				t.Errorf("daemon UpdateBead should not be called for invalid value: calls=%d", f.updateCalls)
			}
			body := w.Body.String()
			if !strings.Contains(body, "Priority must be an integer") {
				t.Errorf("expected inline error, body=%s", body)
			}
			if !strings.Contains(body, `<option value="2" selected>P2</option>`) {
				t.Errorf("error fragment should preserve original priority\nbody=%s", body)
			}
			if !strings.Contains(body, `hx-post="/b/gd-prio1/priority"`) {
				t.Errorf("error fragment should re-render editor for retry\nbody=%s", body)
			}
		})
	}
}

// TestUpdatePriority_Unauthenticated rejects POSTs without an auth header
// with 401, defense-in-depth ahead of the OIDC middleware.
func TestUpdatePriority_Unauthenticated(t *testing.T) {
	f := priorityFake()
	mux := mkServer(t, f)
	form := url.Values{"priority": []string{"1"}}
	req := httptest.NewRequest("POST", "/b/gd-prio1/priority", strings.NewReader(form.Encode()))
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

// TestUpdatePriority_NotFound surfaces a missing-bead error from the daemon
// as 404 rather than rendering the editor.
func TestUpdatePriority_NotFound(t *testing.T) {
	mux := mkServer(t, priorityFake())
	form := url.Values{"priority": []string{"1"}}
	req := authedPost("/b/gd-missing/priority", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestUpdatePriority_DaemonError_RendersInline verifies a daemon-side failure
// surfaces as 502 with the editor re-rendered and the operator's draft
// (the submitted priority) preserved so they can retry.
func TestUpdatePriority_DaemonError_RendersInline(t *testing.T) {
	f := priorityFake()
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)
	form := url.Values{"priority": []string{"4"}}
	req := authedPost("/b/gd-prio1/priority", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to update priority") {
		t.Errorf("expected inline error, body=%s", body)
	}
	if !strings.Contains(body, `<option value="4" selected>P4</option>`) {
		t.Errorf("draft priority should be preserved in re-rendered editor, body=%s", body)
	}
	if !strings.Contains(body, `hx-post="/b/gd-prio1/priority"`) {
		t.Errorf("error fragment should re-render editor for retry\nbody=%s", body)
	}
}

// TestPriorityEdit_RoundTrip simulates the popover open/save round trip
// against the same server: open the editor, post a new value, fetch the
// bead show — the new content must reflect the saved value end to end.
func TestPriorityEdit_RoundTrip(t *testing.T) {
	mux := mkServer(t, priorityFake())

	// 1. Open the editor.
	openReq := httptest.NewRequest("GET", "/b/gd-prio1/priority/edit", nil)
	openReq.Header.Set("X-Auth-Request-Email", authorEmail)
	openW := httptest.NewRecorder()
	mux.ServeHTTP(openW, openReq)
	if openW.Code != http.StatusOK {
		t.Fatalf("open: expected 200, got %d", openW.Code)
	}
	if !strings.Contains(openW.Body.String(), `<option value="2" selected>P2</option>`) {
		t.Errorf("open: editor should pre-fill current priority\nbody=%s", openW.Body.String())
	}

	// 2. Save a new value.
	form := url.Values{"priority": []string{"0"}}
	saveReq := authedPost("/b/gd-prio1/priority", form)
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d", saveW.Code)
	}

	// 3. Re-render the bead show — the read-only block should now show the
	//    new priority and not the original.
	showReq := httptest.NewRequest("GET", "/gd-prio1", nil)
	showReq.Header.Set("X-Auth-Request-Email", authorEmail)
	showW := httptest.NewRecorder()
	mux.ServeHTTP(showW, showReq)
	body := showW.Body.String()
	if !strings.Contains(body, `<span id="priority-block" class="priority-block">0</span>`) {
		t.Errorf("show: persisted priority missing on full page render\nbody=%s", body)
	}
}
