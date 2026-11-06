package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// labelsFake builds a fake daemon seeded with one open task bead carrying a
// stable label set, so the edit/cancel/save round-trip has a known input.
func labelsFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-lab1": {
				ID:       "gd-lab1",
				Title:    "Labels target",
				Type:     "task",
				Status:   "open",
				Priority: 2,
				Labels:   []string{"project:gardenio", "scope:test"},
			},
		},
	}
}

// TestBeadShow_LabelsEditButton_AuthedRendersButton verifies the
// "Edit labels" button appears in the bead-actions toolbar for an
// authenticated operator and points at the edit fragment route.
func TestBeadShow_LabelsEditButton_AuthedRendersButton(t *testing.T) {
	mux := mkServer(t, labelsFake())
	req := httptest.NewRequest("GET", "/gd-lab1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-get="/b/gd-lab1/labels/edit"`,
		`hx-target="#labels-block"`,
		`Edit labels`,
		`id="labels-block"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in bead-show render\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_LabelsEditButton_HiddenWhenAnonymous verifies anonymous
// viewers don't see the edit affordance — the toolbar gate must fall over
// for non-OIDC requests just like priority/assignee/description.
func TestBeadShow_LabelsEditButton_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, labelsFake())
	req := httptest.NewRequest("GET", "/gd-lab1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, `hx-get="/b/gd-lab1/labels/edit"`) {
		t.Errorf("anonymous viewer should not see the edit-labels button\nbody: %s", body)
	}
	// The read-only block is still emitted for everyone.
	if !strings.Contains(body, `id="labels-block"`) {
		t.Errorf("anonymous viewer should still see the read-only labels block\nbody: %s", body)
	}
}

// TestLabelsEdit_AuthedRendersForm verifies GET .../labels/edit returns the
// editor partial pre-filled with the current label set joined by ", ".
func TestLabelsEdit_AuthedRendersForm(t *testing.T) {
	mux := mkServer(t, labelsFake())
	req := httptest.NewRequest("GET", "/b/gd-lab1/labels/edit", nil)
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
		`id="labels-block"`,
		`hx-post="/b/gd-lab1/labels"`,
		`name="labels"`,
		`value="project:gardenio, scope:test"`,
		`Cancel`,
		`Save`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor partial missing %q\nbody=%s", want, body)
		}
	}
}

// TestLabelsEdit_Unauthenticated rejects anonymous GETs with 401 — defense
// in depth ahead of the OIDC middleware.
func TestLabelsEdit_Unauthenticated(t *testing.T) {
	mux := mkServer(t, labelsFake())
	req := httptest.NewRequest("GET", "/b/gd-lab1/labels/edit", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestLabelsView_RendersBlock verifies GET .../labels returns the read-only
// block (used by the editor's Cancel button).
func TestLabelsView_RendersBlock(t *testing.T) {
	mux := mkServer(t, labelsFake())
	req := httptest.NewRequest("GET", "/b/gd-lab1/labels", nil)
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
		`id="labels-block"`,
		`project:gardenio`,
		`scope:test`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("block partial missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, `name="labels"`) {
		t.Errorf("read-only block should not contain a labels input\nbody=%s", body)
	}
}

// TestUpdateLabels_HappyPath posts a new label set, verifies the daemon is
// called with the parsed/trimmed/deduped slice, and that the response is the
// read-only block rendered with the new labels.
func TestUpdateLabels_HappyPath(t *testing.T) {
	f := labelsFake()
	mux := mkServer(t, f)
	form := url.Values{"labels": []string{" project:gardenio ,  area:viewer , project:gardenio "}}
	req := authedPost("/b/gd-lab1/labels", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 update call, got %d", f.updateCalls)
	}
	if f.updateLastID != "gd-lab1" {
		t.Errorf("update id = %q, want gd-lab1", f.updateLastID)
	}
	if f.updateLastReq.Labels == nil {
		t.Fatalf("expected Labels on update, got nil")
	}
	got := *f.updateLastReq.Labels
	want := []string{"project:gardenio", "area:viewer"}
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("labels[%d] = %q, want %q (full=%v)", i, got[i], v, got)
		}
	}
	if f.updateLastReq.Status != nil || f.updateLastReq.Priority != nil || f.updateLastReq.Assignee != nil || f.updateLastReq.Description != nil {
		t.Errorf("labels update should not touch other fields, got %+v", f.updateLastReq)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="labels-block"`,
		`project:gardenio`,
		`area:viewer`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("post-update fragment missing %q\nbody=%s", want, body)
		}
	}
	// No editor on success — the swap returns the read-only block.
	if strings.Contains(body, `name="labels"`) {
		t.Errorf("happy-path response should be the read-only block, got editor\nbody=%s", body)
	}
}

// TestUpdateLabels_AllowsEmptyClears verifies an empty submission clears all
// labels — the form contract is "submit empty to clear".
func TestUpdateLabels_AllowsEmptyClears(t *testing.T) {
	f := labelsFake()
	mux := mkServer(t, f)
	form := url.Values{"labels": []string{""}}
	req := authedPost("/b/gd-lab1/labels", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateLastReq.Labels == nil {
		t.Fatalf("expected non-nil Labels on clear, got nil (must distinguish clear from leave-alone)")
	}
	if len(*f.updateLastReq.Labels) != 0 {
		t.Errorf("expected empty labels slice on clear, got %v", *f.updateLastReq.Labels)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="empty"`) {
		t.Errorf("cleared labels should render empty placeholder\nbody=%s", body)
	}
}

// TestUpdateLabels_Unauthenticated rejects POSTs without an auth header with
// 401, defense-in-depth ahead of the OIDC middleware.
func TestUpdateLabels_Unauthenticated(t *testing.T) {
	f := labelsFake()
	mux := mkServer(t, f)
	form := url.Values{"labels": []string{"foo"}}
	req := httptest.NewRequest("POST", "/b/gd-lab1/labels", strings.NewReader(form.Encode()))
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

// TestUpdateLabels_NotFound surfaces a missing-bead error from the daemon as
// 404 rather than rendering the editor.
func TestUpdateLabels_NotFound(t *testing.T) {
	mux := mkServer(t, labelsFake())
	form := url.Values{"labels": []string{"foo"}}
	req := authedPost("/b/gd-missing/labels", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestUpdateLabels_DaemonError_RendersInline verifies a daemon-side failure
// surfaces as 502 with the editor re-rendered, the inline error message, and
// the operator's draft preserved so they can retry.
func TestUpdateLabels_DaemonError_RendersInline(t *testing.T) {
	f := labelsFake()
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)
	form := url.Values{"labels": []string{"draft:preserved, area:viewer"}}
	req := authedPost("/b/gd-lab1/labels", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to update labels") {
		t.Errorf("expected inline error, body=%s", body)
	}
	if !strings.Contains(body, "draft:preserved, area:viewer") {
		t.Errorf("draft should be preserved in re-rendered editor, body=%s", body)
	}
	if !strings.Contains(body, `hx-post="/b/gd-lab1/labels"`) {
		t.Errorf("error fragment should re-render the editor for retry\nbody=%s", body)
	}
}

// TestUpdateLabels_OversizedReturns413 rejects payloads larger than the
// inline editor cap with 413, and re-renders the editor with the draft so
// the operator can trim it.
func TestUpdateLabels_OversizedReturns413(t *testing.T) {
	f := labelsFake()
	mux := mkServer(t, f)
	big := strings.Repeat("x", maxLabelsLen+1)
	form := url.Values{"labels": []string{big}}
	req := authedPost("/b/gd-lab1/labels", form)
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

// TestLabelsEdit_RoundTrip simulates the popover open/save round trip
// against the same server: open the editor, post a new value, fetch the
// labels block — the new content must reflect the saved value end to end.
func TestLabelsEdit_RoundTrip(t *testing.T) {
	mux := mkServer(t, labelsFake())

	// 1. Open the editor.
	openReq := httptest.NewRequest("GET", "/b/gd-lab1/labels/edit", nil)
	openReq.Header.Set("X-Auth-Request-Email", authorEmail)
	openW := httptest.NewRecorder()
	mux.ServeHTTP(openW, openReq)
	if openW.Code != http.StatusOK {
		t.Fatalf("open: expected 200, got %d", openW.Code)
	}
	if !strings.Contains(openW.Body.String(), "project:gardenio, scope:test") {
		t.Errorf("open: editor should pre-fill current labels\nbody=%s", openW.Body.String())
	}

	// 2. Save a new value.
	form := url.Values{"labels": []string{"area:viewer, kind:ui"}}
	saveReq := authedPost("/b/gd-lab1/labels", form)
	saveW := httptest.NewRecorder()
	mux.ServeHTTP(saveW, saveReq)
	if saveW.Code != http.StatusOK {
		t.Fatalf("save: expected 200, got %d", saveW.Code)
	}
	if !strings.Contains(saveW.Body.String(), "area:viewer") || !strings.Contains(saveW.Body.String(), "kind:ui") {
		t.Errorf("save: response should reflect new labels\nbody=%s", saveW.Body.String())
	}

	// 3. Re-render the bead show — the read-only block should now show the
	//    new labels and not the original ones.
	showReq := httptest.NewRequest("GET", "/gd-lab1", nil)
	showReq.Header.Set("X-Auth-Request-Email", authorEmail)
	showW := httptest.NewRecorder()
	mux.ServeHTTP(showW, showReq)
	body := showW.Body.String()
	if !strings.Contains(body, "area:viewer") || !strings.Contains(body, "kind:ui") {
		t.Errorf("show: persisted labels missing on full page render\nbody=%s", body)
	}
	// The previous label set must be gone (and Project derived from labels
	// will fall back since project: was replaced).
	if strings.Contains(body, "scope:test") {
		t.Errorf("show: previous labels should be replaced\nbody=%s", body)
	}
}

// TestParseLabelsInput_TrimsAndDedupes covers the parse helper directly so
// future regressions in trim / empty-token / dedupe logic surface here
// rather than as flaky end-to-end behavior.
func TestParseLabelsInput_TrimsAndDedupes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", []string{}},
		{"whitespace only", "   ", []string{}},
		{"single", "foo", []string{"foo"}},
		{"trims spaces", "  foo  ,  bar  ", []string{"foo", "bar"}},
		{"drops empty tokens", "foo,,bar,,", []string{"foo", "bar"}},
		{"dedupes preserving order", "foo, bar, foo, baz, bar", []string{"foo", "bar", "baz"}},
		{"prefixed", "project:gardenio, scope:test", []string{"project:gardenio", "scope:test"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLabelsInput(tc.in)
			if got == nil {
				t.Fatalf("parseLabelsInput must return non-nil slice for %q", tc.in)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i, v := range tc.want {
				if got[i] != v {
					t.Errorf("[%d] = %q, want %q (full=%v)", i, got[i], v, got)
				}
			}
		})
	}
}
