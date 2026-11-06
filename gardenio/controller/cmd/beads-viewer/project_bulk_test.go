package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// bulkFake seeds two open task beads under project:gardenio so the bulk
// handlers have something concrete to mutate. ListBeadsFiltered's response
// drives the GET render; the per-bead store backs the per-id GetBead lookups
// the OOB row re-renders use.
func bulkFake() *fakeDaemon {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-bk1": {
				ID:     "gd-bk1",
				Title:  "First bulk target",
				Type:   "task",
				Status: "open",
				Labels: []string{"project:gardenio"},
			},
			"gd-bk2": {
				ID:     "gd-bk2",
				Title:  "Second bulk target",
				Type:   "task",
				Status: "open",
				Labels: []string{"project:gardenio"},
			},
		},
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-bk1"], f.beads["gd-bk2"]},
		Total: 2,
	}
	return f
}

// TestProjectPage_RendersCheckboxAndBulkBar verifies the GET render carries
// the multi-select affordances: a checkbox per row (with the bead id as the
// form value), the row-id wrapper used by OOB swaps, and the bulk action bar
// with both POST endpoints wired up.
func TestProjectPage_RendersCheckboxAndBulkBar(t *testing.T) {
	mux := mkServer(t, bulkFake())
	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="project-row-gd-bk1"`,
		`id="project-row-gd-bk2"`,
		`class="project-row-select"`,
		`name="bead_id" value="gd-bk1"`,
		`name="bead_id" value="gd-bk2"`,
		`id="project-bulk-bar"`,
		`hx-post="/p/gardenio/bulk/status"`,
		`hx-post="/p/gardenio/bulk/labels"`,
		`data-bulk-clear`,
		`data-bulk-count`,
		`data-bulk-select-all`,
		`hx-include=".project-row-select:checked"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project page missing %q\nbody=%s", want, body)
		}
	}
}

// TestBulkStatus_HappyPath posts two bead_ids + status=closed, verifies the
// daemon receives one UpdateBead call per id with the new status, and that
// the response body contains the cleared bar plus an OOB row partial for
// each updated bead.
func TestBulkStatus_HappyPath(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id": []string{"gd-bk1", "gd-bk2"},
		"status":  []string{"closed"},
	}
	req := authedPost("/p/gardenio/bulk/status", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 2 {
		t.Fatalf("expected 2 UpdateBead calls, got %d", f.updateCalls)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "closed" {
		t.Errorf("last update Status = %v, want pointer to \"closed\"", f.updateLastReq.Status)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="project-bulk-bar"`,
		`hidden`,
		`id="project-row-gd-bk1"`,
		`id="project-row-gd-bk2"`,
		`hx-swap-oob="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bulk status response missing %q\nbody=%s", want, body)
		}
	}
}

// TestBulkStatus_RejectsBadStatus verifies an invalid status value yields a
// 4xx and the cleared bar with an inline error — daemon must not be called.
func TestBulkStatus_RejectsBadStatus(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id": []string{"gd-bk1"},
		"status":  []string{"garbage"},
	}
	req := authedPost("/p/gardenio/bulk/status", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 0 {
		t.Errorf("expected no UpdateBead calls on validation failure, got %d", f.updateCalls)
	}
	if !strings.Contains(w.Body.String(), "Status must be one of") {
		t.Errorf("expected inline status error, body=%s", w.Body.String())
	}
}

// TestBulkStatus_RejectsEmptySelection verifies that missing bead_id yields
// a 4xx with a "Select at least one bead" message — daemon must not be called.
func TestBulkStatus_RejectsEmptySelection(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{"status": []string{"closed"}}
	req := authedPost("/p/gardenio/bulk/status", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if f.updateCalls != 0 {
		t.Errorf("expected no UpdateBead calls on empty selection, got %d", f.updateCalls)
	}
	if !strings.Contains(w.Body.String(), "Select at least one bead") {
		t.Errorf("expected inline selection error, body=%s", w.Body.String())
	}
}

// TestBulkStatus_AnonymousReturns401 verifies the OIDC gate fires before any
// daemon work happens.
func TestBulkStatus_AnonymousReturns401(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id": []string{"gd-bk1"},
		"status":  []string{"closed"},
	}
	req := httptest.NewRequest("POST", "/p/gardenio/bulk/status", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if f.updateCalls != 0 {
		t.Errorf("expected no UpdateBead calls when unauthenticated, got %d", f.updateCalls)
	}
}

// TestBulkLabels_AddPath posts add_label across two ids and verifies AddLabel
// is called once per id with the trimmed value, and OOB row partials come back.
func TestBulkLabels_AddPath(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id":   []string{"gd-bk1", "gd-bk2"},
		"add_label": []string{"  area:viewer  "},
	}
	req := authedPost("/p/gardenio/bulk/labels", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.addLabelCalls != 2 {
		t.Errorf("expected 2 AddLabel calls, got %d", f.addLabelCalls)
	}
	if f.removeLabelCalls != 0 {
		t.Errorf("expected 0 RemoveLabel calls, got %d", f.removeLabelCalls)
	}
	if f.addLabelLastVal != "area:viewer" {
		t.Errorf("AddLabel last val = %q, want %q (trim)", f.addLabelLastVal, "area:viewer")
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="project-row-gd-bk1"`,
		`id="project-row-gd-bk2"`,
		`hx-swap-oob="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bulk labels response missing %q\nbody=%s", want, body)
		}
	}
}

// TestBulkLabels_RemovePath posts remove_label and verifies RemoveLabel is
// called per id (no AddLabel calls).
func TestBulkLabels_RemovePath(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id":      []string{"gd-bk1", "gd-bk2"},
		"remove_label": []string{"project:gardenio"},
	}
	req := authedPost("/p/gardenio/bulk/labels", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.removeLabelCalls != 2 {
		t.Errorf("expected 2 RemoveLabel calls, got %d", f.removeLabelCalls)
	}
	if f.addLabelCalls != 0 {
		t.Errorf("expected 0 AddLabel calls, got %d", f.addLabelCalls)
	}
	if f.removeLabelLastVal != "project:gardenio" {
		t.Errorf("RemoveLabel last val = %q", f.removeLabelLastVal)
	}
}

// TestBulkLabels_AddAndRemove posts both add_label and remove_label — both
// RPCs must run per id.
func TestBulkLabels_AddAndRemove(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id":      []string{"gd-bk1"},
		"add_label":    []string{"area:viewer"},
		"remove_label": []string{"project:gardenio"},
	}
	req := authedPost("/p/gardenio/bulk/labels", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.addLabelCalls != 1 || f.removeLabelCalls != 1 {
		t.Errorf("calls: add=%d remove=%d, want 1/1", f.addLabelCalls, f.removeLabelCalls)
	}
}

// TestBulkLabels_RejectsEmptyLabel verifies that posting bead_ids without any
// add_label or remove_label is a 4xx and no daemon calls happen.
func TestBulkLabels_RejectsEmptyLabel(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{"bead_id": []string{"gd-bk1"}}
	req := authedPost("/p/gardenio/bulk/labels", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if f.addLabelCalls+f.removeLabelCalls != 0 {
		t.Errorf("expected zero label calls on empty submit, got add=%d remove=%d",
			f.addLabelCalls, f.removeLabelCalls)
	}
	if !strings.Contains(w.Body.String(), "Provide a label") {
		t.Errorf("expected inline label error, body=%s", w.Body.String())
	}
}

// TestBulkStatus_DaemonErrorReturns502 wires updateErr on the fake and posts a
// status update — every per-id call fails so the response should be 502 with
// an inline error and no OOB row partials.
func TestBulkStatus_DaemonErrorReturns502(t *testing.T) {
	f := bulkFake()
	f.updateErr = errors.New("kaboom")
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id": []string{"gd-bk1"},
		"status":  []string{"closed"},
	}
	req := authedPost("/p/gardenio/bulk/status", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Failed to update gd-bk1") {
		t.Errorf("expected per-id error, body=%s", w.Body.String())
	}
	// No OOB row should be emitted when no bead succeeded.
	if strings.Contains(w.Body.String(), `hx-swap-oob="outerHTML"`) {
		t.Errorf("no OOB rows expected on total failure, body=%s", w.Body.String())
	}
}

// TestBulkAssignee_HappyPath posts two bead_ids + a non-empty assignee and
// verifies the daemon receives one UpdateBead call per id with the new
// assignee, that other UpdateBead fields are left untouched, and that the
// response body contains the cleared bar plus an OOB row partial per id with
// the assignee column re-rendered.
func TestBulkAssignee_HappyPath(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id":  []string{"gd-bk1", "gd-bk2"},
		"assignee": []string{"  alice@example.com  "},
	}
	req := authedPost("/p/gardenio/bulk/assignee", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 2 {
		t.Fatalf("expected 2 UpdateBead calls, got %d", f.updateCalls)
	}
	if f.updateLastReq.Assignee == nil {
		t.Fatalf("expected Assignee pointer set")
	}
	if got := *f.updateLastReq.Assignee; got != "alice@example.com" {
		t.Errorf("Assignee = %q, want %q (trim)", got, "alice@example.com")
	}
	if f.updateLastReq.Status != nil || f.updateLastReq.Priority != nil ||
		f.updateLastReq.Description != nil || f.updateLastReq.Labels != nil ||
		f.updateLastReq.Owner != nil {
		t.Errorf("bulk assignee should not touch other fields, got %+v", f.updateLastReq)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="project-bulk-bar"`,
		`hidden`,
		`id="project-row-gd-bk1"`,
		`id="project-row-gd-bk2"`,
		`hx-swap-oob="outerHTML"`,
		`hx-post="/p/gardenio/bulk/assignee"`,
		// Row re-render reflects the new assignee on the assignee column.
		`bead-row-assignee">alice@example.com<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bulk assignee response missing %q\nbody=%s", want, body)
		}
	}
}

// TestBulkAssignee_EmptyClears posts bead_ids with an empty assignee value
// and verifies UpdateBead is called per id with an Assignee pointer to "" —
// matching the per-row popover semantics (empty submission clears the field).
func TestBulkAssignee_EmptyClears(t *testing.T) {
	f := bulkFake()
	// Seed an existing assignee so the clear is observable on the row render.
	f.beads["gd-bk1"].Assignee = "old@example.com"
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id":  []string{"gd-bk1"},
		"assignee": []string{"   "},
	}
	req := authedPost("/p/gardenio/bulk/assignee", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 UpdateBead call, got %d", f.updateCalls)
	}
	if f.updateLastReq.Assignee == nil {
		t.Fatalf("expected Assignee pointer set (clear-by-empty)")
	}
	if got := *f.updateLastReq.Assignee; got != "" {
		t.Errorf("Assignee = %q, want empty string", got)
	}
}

// TestBulkAssignee_RejectsEmptySelection verifies that missing bead_id yields
// a 4xx — daemon must not be called.
func TestBulkAssignee_RejectsEmptySelection(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{"assignee": []string{"alice@example.com"}}
	req := authedPost("/p/gardenio/bulk/assignee", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if f.updateCalls != 0 {
		t.Errorf("expected no UpdateBead calls on empty selection, got %d", f.updateCalls)
	}
}

// TestBulkAssignee_AnonymousReturns401 verifies the OIDC gate fires before any
// daemon work.
func TestBulkAssignee_AnonymousReturns401(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id":  []string{"gd-bk1"},
		"assignee": []string{"alice@example.com"},
	}
	req := httptest.NewRequest("POST", "/p/gardenio/bulk/assignee", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if f.updateCalls != 0 {
		t.Errorf("expected no UpdateBead calls when unauthenticated, got %d", f.updateCalls)
	}
}

// TestBulkAssignee_DaemonErrorReturns502 — every per-id call fails so the
// response should be 502 with an inline error and no OOB row partials.
func TestBulkAssignee_DaemonErrorReturns502(t *testing.T) {
	f := bulkFake()
	f.updateErr = errors.New("kaboom")
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id":  []string{"gd-bk1"},
		"assignee": []string{"alice@example.com"},
	}
	req := authedPost("/p/gardenio/bulk/assignee", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Failed to update gd-bk1") {
		t.Errorf("expected per-id error, body=%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `hx-swap-oob="outerHTML"`) {
		t.Errorf("no OOB rows expected on total failure, body=%s", w.Body.String())
	}
}

// TestProjectPage_RendersBulkAssigneeForm verifies the bulk action bar on the
// initial GET render carries the assignee form pointing at the new endpoint.
func TestProjectPage_RendersBulkAssigneeForm(t *testing.T) {
	mux := mkServer(t, bulkFake())
	req := httptest.NewRequest("GET", "/p/gardenio?view=list", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-post="/p/gardenio/bulk/assignee"`,
		`name="assignee"`,
		`aria-label="Bulk assignee"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("project page missing %q\nbody=%s", want, body)
		}
	}
}

// TestBulkStatus_DedupesBeadIDs verifies the same bead id passed twice in a
// form is collapsed to one daemon call (defensive against a malicious or
// confused client).
func TestBulkStatus_DedupesBeadIDs(t *testing.T) {
	f := bulkFake()
	mux := mkServer(t, f)
	form := url.Values{
		"bead_id": []string{"gd-bk1", "gd-bk1", " gd-bk1 "},
		"status":  []string{"closed"},
	}
	req := authedPost("/p/gardenio/bulk/status", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.updateCalls != 1 {
		t.Errorf("expected 1 UpdateBead call after dedupe, got %d", f.updateCalls)
	}
}
