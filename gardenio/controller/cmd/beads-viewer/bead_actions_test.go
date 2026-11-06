package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestBuildBeadActions_EscalationSuppressesClose verifies the resolve-surface
// decision (ka-0000005516): the generic Close button is suppressed for an open
// escalation (resolved via its dedicated approve/reject verbs), while an
// ordinary open task still gets Close.
func TestBuildBeadActions_EscalationSuppressesClose(t *testing.T) {
	esc := &beadsapi.BeadDetail{ID: "gd-e1", Type: "escalation", Status: "open"}
	if v := buildBeadActions(esc, "matt@example.com", nil); v.CanClose {
		t.Error("an open escalation must NOT offer the generic Close button")
	}

	task := &beadsapi.BeadDetail{ID: "gd-t1", Type: "task", Status: "open"}
	if v := buildBeadActions(task, "matt@example.com", nil); !v.CanClose {
		t.Error("an open task must still offer the Close button")
	}
}

// actionsFake builds a fake daemon seeded with one open task bead so the
// state-aware action builder has a stable input across tests.
func actionsFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-act1": {
				ID:       "gd-act1",
				Title:    "Action target",
				Type:     "task",
				Status:   "open",
				Priority: 2,
				Assignee: "",
				Labels:   []string{"project:gardenio"},
			},
		},
	}
}

func TestBeadShow_ActionsToolbar_AuthedClaim(t *testing.T) {
	mux := mkServer(t, actionsFake())
	req := httptest.NewRequest("GET", "/gd-act1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="bead-actions"`,
		`hx-post="/b/gd-act1/close"`,
		`hx-post="/b/gd-act1/claim"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in toolbar render\nbody: %s", want, body)
		}
	}
	for _, forbidden := range []string{
		`hx-post="/b/gd-act1/release"`,
		`hx-post="/b/gd-act1/reopen"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("toolbar should not show %q for unassigned open bead", forbidden)
		}
	}
}

func TestBeadShow_ActionsToolbar_AuthedRelease(t *testing.T) {
	f := actionsFake()
	f.beads["gd-act1"].Assignee = "matt@example.com"

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-act1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `hx-post="/b/gd-act1/release"`) {
		t.Errorf("expected Release button when operator owns the bead\nbody: %s", body)
	}
	if strings.Contains(body, `hx-post="/b/gd-act1/claim"`) {
		t.Errorf("Claim should be hidden when operator already owns the bead")
	}
}

func TestBeadShow_ActionsToolbar_ClosedShowsReopenOnly(t *testing.T) {
	f := actionsFake()
	f.beads["gd-act1"].Status = "closed"

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-act1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `hx-post="/b/gd-act1/reopen"`) {
		t.Errorf("expected Reopen button on closed bead\nbody: %s", body)
	}
	for _, forbidden := range []string{
		`hx-post="/b/gd-act1/close"`,
		`hx-post="/b/gd-act1/claim"`,
		`hx-post="/b/gd-act1/release"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("closed bead toolbar should not include %q", forbidden)
		}
	}
}

func TestBeadShow_ActionsToolbar_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, actionsFake())
	req := httptest.NewRequest("GET", "/gd-act1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, forbidden := range []string{
		`hx-post="/b/gd-act1/close"`,
		`hx-post="/b/gd-act1/claim"`,
		`hx-post="/b/gd-act1/release"`,
		`hx-post="/b/gd-act1/reopen"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("anonymous viewer should not see action %q\nbody: %s", forbidden, body)
		}
	}
}

func TestCloseBeadAction_HappyPath(t *testing.T) {
	f := actionsFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/b/gd-act1/close", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.closeCalls != 1 {
		t.Fatalf("expected 1 close call, got %d", f.closeCalls)
	}
	if f.closeLastID != "gd-act1" {
		t.Errorf("close id = %q, want gd-act1", f.closeLastID)
	}
	if f.closeLastBy != "matt@example.com" {
		t.Errorf("close actor = %q, want matt@example.com", f.closeLastBy)
	}
	body := w.Body.String()
	if !strings.Contains(body, `hx-post="/b/gd-act1/reopen"`) {
		t.Errorf("post-close render should show Reopen button\nbody: %s", body)
	}
	if strings.Contains(body, `hx-post="/b/gd-act1/close"`) {
		t.Errorf("post-close render should not show Close button")
	}
}

func TestReopenBead_HappyPath(t *testing.T) {
	f := actionsFake()
	f.beads["gd-act1"].Status = "closed"

	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/b/gd-act1/reopen", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 update call, got %d", f.updateCalls)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "open" {
		t.Errorf("expected status=open on reopen, got %+v", f.updateLastReq.Status)
	}
}

func TestClaimBead_HappyPath(t *testing.T) {
	f := actionsFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/b/gd-act1/claim", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 update call, got %d", f.updateCalls)
	}
	if f.updateLastReq.Assignee == nil || *f.updateLastReq.Assignee != "matt@example.com" {
		t.Errorf("expected assignee=matt@example.com on claim, got %+v", f.updateLastReq.Assignee)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "in_progress" {
		t.Errorf("expected status=in_progress on claim, got %+v", f.updateLastReq.Status)
	}
	body := w.Body.String()
	if !strings.Contains(body, `hx-post="/b/gd-act1/release"`) {
		t.Errorf("post-claim render should show Release button\nbody: %s", body)
	}
}

func TestReleaseBead_HappyPath(t *testing.T) {
	f := actionsFake()
	f.beads["gd-act1"].Assignee = "matt@example.com"
	f.beads["gd-act1"].Status = "in_progress"

	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/b/gd-act1/release", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 update call, got %d", f.updateCalls)
	}
	if f.updateLastReq.Assignee == nil || *f.updateLastReq.Assignee != "" {
		t.Errorf("expected empty assignee on release, got %+v", f.updateLastReq.Assignee)
	}
	if f.updateLastReq.Status != nil {
		t.Errorf("release should not change status, got %+v", *f.updateLastReq.Status)
	}
}

// The /garden chore tour's Defer (gd-S2Cn2dTsP0): status=deferred and nothing else, like `kd defer` without --until.
func TestDeferBead_HappyPath(t *testing.T) {
	f := actionsFake()
	f.beads["gd-act1"].Status = "in_progress"
	f.beads["gd-act1"].Assignee = "flo"

	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/b/gd-act1/defer", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 update call, got %d", f.updateCalls)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "deferred" {
		t.Errorf("expected status=deferred on defer, got %+v", f.updateLastReq.Status)
	}
	if f.updateLastReq.Assignee != nil {
		t.Errorf("defer should not change the assignee, got %+v", *f.updateLastReq.Assignee)
	}
}

func TestBeadAction_Unauthenticated(t *testing.T) {
	for _, path := range []string{"close", "reopen", "claim", "release", "defer"} {
		t.Run(path, func(t *testing.T) {
			f := actionsFake()
			if path == "reopen" {
				f.beads["gd-act1"].Status = "closed"
			}
			mux := mkServer(t, f)
			req := httptest.NewRequest("POST", "/b/gd-act1/"+path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d\nbody: %s", w.Code, w.Body.String())
			}
			if f.closeCalls != 0 || f.updateCalls != 0 {
				t.Errorf("daemon should not be called: close=%d update=%d", f.closeCalls, f.updateCalls)
			}
		})
	}
}

func TestBeadAction_NotFound(t *testing.T) {
	mux := mkServer(t, actionsFake())
	req := httptest.NewRequest("POST", "/b/gd-missing/close", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestBeadAction_DaemonError_RendersInline(t *testing.T) {
	f := actionsFake()
	f.closeErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}

	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/b/gd-act1/close", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to close bead") {
		t.Errorf("expected inline error, body: %s", body)
	}
	// Buttons should still render so the user can retry.
	if !strings.Contains(body, `hx-post="/b/gd-act1/close"`) {
		t.Errorf("error fragment should re-render Close button for retry\nbody: %s", body)
	}
}

func TestBeadAction_RowReturn_RendersRowPartial(t *testing.T) {
	f := actionsFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/b/gd-act1/claim?return=row", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="bead-row-gd-act1"`) {
		t.Errorf("expected row partial output, body: %s", body)
	}
	// Row partial uses ?return=row on its own action buttons.
	if !strings.Contains(body, `hx-post="/b/gd-act1/release?return=row"`) {
		t.Errorf("row partial should keep return=row on action buttons\nbody: %s", body)
	}
}

// TestCloseBeadAction_ProjectRowReturn_RendersProjectRowPartial exercises the
// row-level Close button on /p/<project>: POST /b/{id}/close?return=project_row
// must close the bead and return the project_row partial with status=closed,
// the Close button hidden (layout-stable), and the Spawn button still present
// for the operator to use even on closed beads.
func TestCloseBeadAction_ProjectRowReturn_RendersProjectRowPartial(t *testing.T) {
	f := actionsFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/b/gd-act1/close?return=project_row", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.closeCalls != 1 {
		t.Fatalf("expected 1 close call, got %d", f.closeCalls)
	}
	if f.closeLastID != "gd-act1" {
		t.Errorf("close id = %q, want gd-act1", f.closeLastID)
	}
	if f.closeLastBy != "matt@example.com" {
		t.Errorf("close actor = %q, want matt@example.com", f.closeLastBy)
	}

	body := w.Body.String()
	for _, want := range []string{
		`id="project-row-gd-act1"`,
		`data-status="closed"`,
		// Spawn button must still render on closed beads (hx-get target).
		`/quick-spawn?task=gd-act1`,
		// Close button is rendered but hidden for layout stability.
		`project-row-action-hidden`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in project_row partial\nbody: %s", want, body)
		}
	}
}

func TestBeadShow_StatusEditor_AuthedRendersSelect(t *testing.T) {
	mux := mkServer(t, actionsFake())
	req := httptest.NewRequest("GET", "/gd-act1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-post="/b/gd-act1/status"`,
		`name="status"`,
		`<option value="open" selected>open</option>`,
		`<option value="in_progress">in_progress</option>`,
		`<option value="closed">closed</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in status editor render\nbody: %s", want, body)
		}
	}
}

func TestBeadShow_StatusEditor_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, actionsFake())
	req := httptest.NewRequest("GET", "/gd-act1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, `hx-post="/b/gd-act1/status"`) {
		t.Errorf("anonymous viewer should not see status editor\nbody: %s", body)
	}
}

func TestUpdateStatus_HappyPath(t *testing.T) {
	f := actionsFake()
	mux := mkServer(t, f)
	form := url.Values{"status": []string{"in_progress"}}
	req := authedPost("/b/gd-act1/status", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.updateCalls != 1 {
		t.Fatalf("expected 1 update call, got %d", f.updateCalls)
	}
	if f.updateLastID != "gd-act1" {
		t.Errorf("update id = %q, want gd-act1", f.updateLastID)
	}
	if f.updateLastReq.Status == nil || *f.updateLastReq.Status != "in_progress" {
		t.Errorf("expected status=in_progress on update, got %+v", f.updateLastReq.Status)
	}
	if f.updateLastReq.Assignee != nil {
		t.Errorf("status update should not change assignee, got %+v", *f.updateLastReq.Assignee)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<option value="in_progress" selected>in_progress</option>`) {
		t.Errorf("post-update render should reflect new status\nbody: %s", body)
	}
}

func TestUpdateStatus_Unauthenticated(t *testing.T) {
	f := actionsFake()
	mux := mkServer(t, f)
	form := url.Values{"status": []string{"closed"}}
	req := httptest.NewRequest("POST", "/b/gd-act1/status", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.updateCalls != 0 {
		t.Errorf("daemon UpdateBead should not be called: %d", f.updateCalls)
	}
}

func TestUpdateStatus_InvalidValue_RendersInline(t *testing.T) {
	cases := []string{"", "draft", "OPEN", "in-progress", "deleted"}
	for _, raw := range cases {
		t.Run("status="+raw, func(t *testing.T) {
			f := actionsFake()
			mux := mkServer(t, f)
			form := url.Values{"status": []string{raw}}
			req := authedPost("/b/gd-act1/status", form)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d\nbody: %s", w.Code, w.Body.String())
			}
			if f.updateCalls != 0 {
				t.Errorf("daemon UpdateBead should not be called for invalid value: calls=%d", f.updateCalls)
			}
			body := w.Body.String()
			if !strings.Contains(body, "Status must be one of") {
				t.Errorf("expected inline error, body: %s", body)
			}
			if !strings.Contains(body, `<option value="open" selected>open</option>`) {
				t.Errorf("error fragment should preserve original status\nbody: %s", body)
			}
		})
	}
}

func TestUpdateStatus_NotFound(t *testing.T) {
	mux := mkServer(t, actionsFake())
	form := url.Values{"status": []string{"closed"}}
	req := authedPost("/b/gd-missing/status", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestUpdateStatus_DaemonError_RendersInline(t *testing.T) {
	f := actionsFake()
	f.updateErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}

	mux := mkServer(t, f)
	form := url.Values{"status": []string{"closed"}}
	req := authedPost("/b/gd-act1/status", form)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to update status") {
		t.Errorf("expected inline error, body: %s", body)
	}
	if !strings.Contains(body, `hx-post="/b/gd-act1/status"`) {
		t.Errorf("error fragment should re-render status form for retry\nbody: %s", body)
	}
}

func TestActivePage_RendersRowActions(t *testing.T) {
	row := &beadsapi.BeadDetail{
		ID:       "gd-row1",
		Title:    "In flight",
		Type:     "task",
		Status:   "in_progress",
		Priority: 1,
		Assignee: "matt@example.com",
		Labels:   []string{"project:gardenio"},
	}
	f := &fakeDaemon{
		beads:    map[string]*beadsapi.BeadDetail{"gd-row1": row},
		listResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{row}, Total: 1},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/active", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// /active now renders project_row cards (ka-0000007299 card redesign):
	// row id is project-row-{id}, Close uses ?return=project_row, and the
	// Spawn button replaces Release/Claim.
	for _, want := range []string{
		`id="project-row-gd-row1"`,
		`hx-post="/b/gd-row1/close?return=project_row"`,
		`hx-get="/quick-spawn?task=gd-row1&project=gardenio"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("active page missing %q\nbody: %s", want, body)
		}
	}
}
