package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// dismissFake builds a fakeDaemon seeded with one open decision bead so the
// dismiss handler tests don't depend on the resolve fixture.
func dismissFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-dec1": {
				ID:        "gd-dec1",
				Title:     "JWT vs sessions",
				Type:      "decision",
				Status:    "open",
				CreatedBy: "gardener-1",
				Fields: map[string]string{
					"prompt":       "Pick auth approach.",
					"requested_by": "gardener-1",
					"options": `[
						{"id":"jwt","short":"Use JWT","label":"Stateless JWT","artifact_type":"plan"}
					]`,
				},
			},
		},
	}
}

// closeAsCanceled mirrors what kbeads' cancel handler does to bead state, so
// the post-dismiss re-render reflects daemon-shaped fields.
func closeAsCanceled(reason, by string) func(*fakeDaemon, string, string, string) {
	return func(fd *fakeDaemon, id, gotReason, gotBy string) {
		b := fd.beads[id]
		b.Status = "closed"
		b.Fields["canceled"] = "true"
		b.Fields["chosen"] = "dismissed"
		if reason != "" {
			b.Fields["cancel_reason"] = reason
		}
		if by != "" {
			b.Fields["canceled_by"] = by
		}
	}
}

func TestBeadShow_DecisionDismissButton_RendersForAuthedUser(t *testing.T) {
	mux := mkServer(t, dismissFake())
	req := httptest.NewRequest("GET", "/gd-dec1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="chip chip-type decision-dismiss-chip decision-dismiss-btn"`,
		`data-bead-id="gd-dec1"`,
		"Dismiss",
		// Dismiss reuses the resolve dialog with a Reason textarea.
		`id="decision-resolve-dialog"`,
		`name="response_text"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in dismiss render\nbody: %s", want, body)
		}
	}
}

func TestBeadShow_DecisionDismissButton_HiddenWhenAnonymous(t *testing.T) {
	mux := mkServer(t, dismissFake())
	req := httptest.NewRequest("GET", "/gd-dec1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `/b/gd-dec1/decision/dismiss`) {
		t.Errorf("anonymous render should not expose dismiss form")
	}
}

func TestBeadShow_DecisionDismissButton_HiddenWhenResolved(t *testing.T) {
	f := dismissFake()
	b := f.beads["gd-dec1"]
	b.Status = "closed"
	b.Fields["chosen"] = "jwt"

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-dec1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `/b/gd-dec1/decision/dismiss`) {
		t.Errorf("resolved decision should not render dismiss form")
	}
}

func TestDismissDecision_HappyPath(t *testing.T) {
	f := dismissFake()
	f.cancelOnSuccess = closeAsCanceled("changed our minds", "matt@example.com")

	mux := mkServer(t, f)
	form := strings.NewReader("reason=changed+our+minds")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/dismiss", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.cancelCalls != 1 {
		t.Fatalf("expected 1 cancel call, got %d", f.cancelCalls)
	}
	if got, want := f.cancelLastID, "gd-dec1"; got != want {
		t.Errorf("cancel id = %q, want %q", got, want)
	}
	if got, want := f.cancelLastReason, "changed our minds"; got != want {
		t.Errorf("cancel reason = %q, want %q", got, want)
	}
	if got, want := f.cancelLastBy, "matt@example.com"; got != want {
		t.Errorf("canceled_by = %q, want %q", got, want)
	}

	body := w.Body.String()
	for _, want := range []string{
		`<section class="decision-card"`,
		"banner-resolved",
		"Dismissed",
		"matt@example.com",
		"changed our minds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("post-dismiss fragment missing %q\nbody: %s", want, body)
		}
	}
	// Must NOT include resolve forms or dismiss form anymore.
	if strings.Contains(body, `/b/gd-dec1/decision/resolve`) {
		t.Errorf("dismissed fragment should not include resolve forms\nbody: %s", body)
	}
	if strings.Contains(body, `/b/gd-dec1/decision/dismiss`) {
		t.Errorf("dismissed fragment should not include dismiss form\nbody: %s", body)
	}
}

func TestDismissDecision_NoReason_StillSucceeds(t *testing.T) {
	f := dismissFake()
	f.cancelOnSuccess = closeAsCanceled("", "matt@example.com")

	mux := mkServer(t, f)
	form := strings.NewReader("reason=")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/dismiss", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if f.cancelCalls != 1 {
		t.Fatalf("expected 1 cancel call, got %d", f.cancelCalls)
	}
	if f.cancelLastReason != "" {
		t.Errorf("expected empty reason, got %q", f.cancelLastReason)
	}
}

func TestDismissDecision_Unauthenticated(t *testing.T) {
	f := dismissFake()
	mux := mkServer(t, f)
	form := strings.NewReader("reason=nope")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/dismiss", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if f.cancelCalls != 0 {
		t.Fatalf("expected daemon to NOT be called, got %d", f.cancelCalls)
	}
}

func TestDismissDecision_AlreadyResolved(t *testing.T) {
	f := dismissFake()
	f.beads["gd-dec1"].Status = "closed"

	mux := mkServer(t, f)
	form := strings.NewReader("reason=")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/dismiss", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
	if f.cancelCalls != 0 {
		t.Fatalf("expected daemon NOT to be called for already-resolved, got %d", f.cancelCalls)
	}
	if !strings.Contains(w.Body.String(), "already resolved") {
		t.Errorf("expected already-resolved message, body=%s", w.Body.String())
	}
}

func TestDismissDecision_DaemonError(t *testing.T) {
	f := dismissFake()
	f.cancelErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}

	mux := mkServer(t, f)
	form := strings.NewReader("reason=oops")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/dismiss", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d\nbody: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Failed to dismiss decision") {
		t.Errorf("expected error message, body=%s", w.Body.String())
	}
}

func TestDismissDecision_NotFound(t *testing.T) {
	f := dismissFake()
	mux := mkServer(t, f)
	form := strings.NewReader("reason=")
	req := httptest.NewRequest("POST", "/b/gd-missing/decision/dismiss", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
	if f.cancelCalls != 0 {
		t.Fatalf("expected daemon NOT to be called, got %d", f.cancelCalls)
	}
}

func TestDismissDecision_NotADecision(t *testing.T) {
	f := dismissFake()
	f.beads["gd-dec1"].Type = "task"

	mux := mkServer(t, f)
	form := strings.NewReader("reason=")
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/dismiss", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestDismissDecision_ReasonTooLong(t *testing.T) {
	f := dismissFake()
	mux := mkServer(t, f)

	long := strings.Repeat("a", 10_001)
	form := strings.NewReader("reason=" + long)
	req := httptest.NewRequest("POST", "/b/gd-dec1/decision/dismiss", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
	if f.cancelCalls != 0 {
		t.Fatalf("expected daemon NOT to be called for too-long reason, got %d", f.cancelCalls)
	}
}

func TestHandleDecisions_RendersDismissChip(t *testing.T) {
	f := newFake()
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-dec1",
				Title:  "Pick a backend",
				Type:   "decision",
				Status: "open",
				Fields: map[string]string{
					"options": `[{"id":"a","short":"A","label":"Option A"}]`,
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/decisions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="chip chip-type decision-dismiss-chip"`,
		`data-bead-id="gd-dec1"`,
		`>Dismiss<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("decisions list missing %q\nbody=%s", want, body)
		}
	}
}
