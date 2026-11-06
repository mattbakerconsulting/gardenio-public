package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// decisionFake builds a fakeDaemon seeded with one open decision bead
// matching the ka-0000003823 acceptance scenario.
func decisionFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-dec1": {
				ID:        "gd-dec1",
				Title:     "JWT vs sessions",
				Type:      "decision",
				Status:    "open",
				CreatedBy: "gardener-1",
				Fields: map[string]string{
					"prompt":       "Pick auth approach for the new service.",
					"requested_by": "gardener-1",
					"options": `[
						{"id":"jwt","short":"Use JWT","label":"Stateless JWT tokens","artifact_type":"plan"},
						{"id":"session","short":"Use sessions","label":"Server-side sessions","artifact_type":"plan"}
					]`,
				},
			},
		},
	}
}

// TestBeadShow_DecisionResolveForms_RenderForAuthedUser verifies that the
// detail page renders one resolve chip per option (and a dialog with a
// response_text textarea) when the viewer is authenticated and the decision
// is still open. The chip + modal pattern mirrors the /decisions list view.
func TestBeadShow_DecisionResolveForms_RenderForAuthedUser(t *testing.T) {
	mux := mkServer(t, decisionFake())
	req := httptest.NewRequest("GET", "/gd-dec1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="chip chip-type decision-resolve-chip"`,
		`data-bead-id="gd-dec1"`,
		`data-option-id="jwt"`,
		`data-option-id="session"`,
		`name="response_text"`,
		`id="decision-resolve-dialog"`,
		// "Other" chip lets the operator resolve with a free-text response that
		// doesn't fit any of the listed options. Backend already supports this
		// (resolve with empty option_id + non-empty response_text).
		`class="chip chip-type decision-other-chip"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in resolve render\nbody: %s", want, body)
		}
	}
}

// TestBeadShow_DecisionOtherChip_HiddenWhenAnonymous verifies the "Other"
// chip is gated behind authentication just like the other resolve actions.
func TestBeadShow_DecisionOtherChip_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, decisionFake())
	req := httptest.NewRequest("GET", "/gd-dec1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "decision-other-chip") {
		t.Errorf("anonymous render should not expose the Other chip")
	}
}

// TestResolveDecision_OtherFreeText verifies the backend accepts a resolve
// with empty option_id when the operator provided free-text via the "Other"
// chip. The daemon receives an empty SelectedOption and the typed response.
func TestResolveDecision_OtherFreeText(t *testing.T) {
	f := decisionFake()
	f.resolveOnSuccess = func(fd *fakeDaemon, id string, req beadsapi.ResolveDecisionRequest) {
		b := fd.beads[id]
		b.Status = "closed"
		b.Fields["chosen"] = req.SelectedOption
		b.Fields["response_text"] = req.ResponseText
		b.Fields["responded_by"] = req.RespondedBy
		b.Fields["responded_at"] = "2026-05-04T12:00:00Z"
	}

	mux := mkServer(t, f)
	form := strings.NewReader("option_id=&response_text=Neither+of+the+above")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/resolve", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.resolveCalls != 1 {
		t.Fatalf("expected 1 resolve call, got %d", f.resolveCalls)
	}
	if got := f.resolveLastReq.SelectedOption; got != "" {
		t.Errorf("selected option = %q, want empty (Other)", got)
	}
	if got, want := f.resolveLastReq.ResponseText, "Neither of the above"; got != want {
		t.Errorf("response text = %q, want %q", got, want)
	}
}

// TestBeadShow_DecisionResolveForms_HiddenWhenAnonymous verifies that
// unauthenticated viewers see options read-only (no forms, no buttons).
func TestBeadShow_DecisionResolveForms_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, decisionFake())
	req := httptest.NewRequest("GET", "/gd-dec1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, forbidden := range []string{
		`/b/gd-dec1/decision/resolve`,
		`name="option_id"`,
		`name="response_text"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("anonymous render should not expose %q\nbody: %s", forbidden, body)
		}
	}
	// Options themselves still render.
	if !strings.Contains(body, "opt-jwt") {
		t.Errorf("options should still render for anonymous viewers\nbody: %s", body)
	}
}

// TestBeadShow_DecisionResolveForms_HiddenWhenResolved verifies the resolved
// decision is read-only even for authenticated viewers.
func TestBeadShow_DecisionResolveForms_HiddenWhenResolved(t *testing.T) {
	f := decisionFake()
	b := f.beads["gd-dec1"]
	b.Status = "closed"
	b.Fields["chosen"] = "jwt"
	b.Fields["responded_by"] = "matt@example.com"
	b.Fields["responded_at"] = "2026-04-25T12:00:00Z"

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-dec1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `/b/gd-dec1/decision/resolve`) {
		t.Errorf("resolved decision should not render resolve forms\nbody: %s", body)
	}
	for _, want := range []string{
		"banner-resolved",
		"<code>jwt</code>",
		"matt@example.com",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("resolved render missing %q\nbody: %s", want, body)
		}
	}
}

func TestResolveDecision_HappyPath(t *testing.T) {
	f := decisionFake()
	f.resolveOnSuccess = func(fd *fakeDaemon, id string, req beadsapi.ResolveDecisionRequest) {
		// Mirror the kbeads daemon: merge resolution fields and close the bead.
		b := fd.beads[id]
		b.Status = "closed"
		b.Fields["chosen"] = req.SelectedOption
		b.Fields["response_text"] = req.ResponseText
		b.Fields["responded_by"] = req.RespondedBy
		b.Fields["responded_at"] = "2026-04-25T12:00:00Z"
	}

	mux := mkServer(t, f)
	form := strings.NewReader("option_id=jwt&response_text=Go+with+JWT")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/resolve", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.resolveCalls != 1 {
		t.Fatalf("expected 1 resolve call, got %d", f.resolveCalls)
	}
	if got, want := f.resolveLastID, "gd-dec1"; got != want {
		t.Errorf("resolve id = %q, want %q", got, want)
	}
	if got, want := f.resolveLastReq.SelectedOption, "jwt"; got != want {
		t.Errorf("selected option = %q, want %q", got, want)
	}
	if got, want := f.resolveLastReq.ResponseText, "Go with JWT"; got != want {
		t.Errorf("response text = %q, want %q", got, want)
	}
	// Identity must come from the auth header, not the form.
	if got, want := f.resolveLastReq.RespondedBy, "matt@example.com"; got != want {
		t.Errorf("responded_by = %q, want %q", got, want)
	}

	body := w.Body.String()
	for _, want := range []string{
		`<section class="decision-card"`,
		"banner-resolved",
		"<code>jwt</code>",
		"matt@example.com",
		"Go with JWT",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("post-resolve fragment missing %q\nbody: %s", want, body)
		}
	}
	// The fragment must NOT include resolve forms anymore.
	if strings.Contains(body, `/b/gd-dec1/decision/resolve`) {
		t.Errorf("resolved fragment should not include the resolve form\nbody: %s", body)
	}
}

func TestResolveDecision_Unauthenticated(t *testing.T) {
	f := decisionFake()
	mux := mkServer(t, f)
	form := strings.NewReader("option_id=jwt")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/resolve", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.resolveCalls != 0 {
		t.Fatalf("expected daemon to NOT be called, got %d", f.resolveCalls)
	}
}

func TestResolveDecision_AlreadyResolved(t *testing.T) {
	f := decisionFake()
	b := f.beads["gd-dec1"]
	b.Status = "closed"
	b.Fields["chosen"] = "jwt"
	b.Fields["responded_by"] = "earlier@example.com"

	mux := mkServer(t, f)
	form := strings.NewReader("option_id=session&response_text=race")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/resolve", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.resolveCalls != 0 {
		t.Fatalf("expected daemon NOT called for resolved decision, got %d", f.resolveCalls)
	}
	body := w.Body.String()
	for _, want := range []string{
		"already resolved",
		"banner-resolved", // re-rendered partial reflects resolved state
		"<code>jwt</code>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("already-resolved fragment missing %q\nbody: %s", want, body)
		}
	}
}

func TestResolveDecision_EmptyForm(t *testing.T) {
	f := decisionFake()
	mux := mkServer(t, f)
	form := strings.NewReader("")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/resolve", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if f.resolveCalls != 0 {
		t.Fatalf("expected daemon NOT called on empty form, got %d", f.resolveCalls)
	}
}

func TestResolveDecision_DaemonError(t *testing.T) {
	f := decisionFake()
	f.resolveErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)
	form := strings.NewReader("option_id=jwt")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/resolve", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to resolve decision") {
		t.Errorf("expected inline error, got: %s", w.Body.String())
	}
	// Form preserves draft so user doesn't lose their response on retry.
	if !strings.Contains(w.Body.String(), `name="response_text"`) {
		t.Errorf("error fragment should re-render form with response textarea")
	}
}

func TestResolveDecision_NotFound(t *testing.T) {
	f := decisionFake()
	mux := mkServer(t, f)
	form := strings.NewReader("option_id=jwt")
	req := httptest.NewRequest("POST", "/b/gd-missing/decision/resolve", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d\nbody: %s", w.Code, w.Body.String())
	}
}
