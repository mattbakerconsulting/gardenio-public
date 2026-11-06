package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func reflagFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-dec1": {
				ID:        "gd-dec1",
				Title:     "Pick auth approach",
				Type:      "decision",
				Status:    "open",
				Priority:  2,
				CreatedBy: "gardener-1",
				Labels:    []string{"project:gardenio"},
				CreatedAt: time.Now().Add(-3 * time.Hour),
				Fields: map[string]string{
					"prompt":       "Pick auth approach.",
					"requested_by": "worker-agent",
					"options":      `[{"id":"jwt","short":"JWT","artifact_type":"plan"}]`,
				},
			},
		},
		comments: map[string][]beadsapi.Comment{},
	}
}

func reflagRequest(id, email, reason string) *http.Request {
	body := "reason=" + reason
	req := httptest.NewRequest("POST", "/b/"+id+"/decision/reflag", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if email != "" {
		req.Header.Set("X-Auth-Request-Email", email)
	}
	return req
}

func TestReflagDecision_BumpsPriorityAndRecordsFields(t *testing.T) {
	f := reflagFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, reflagRequest("gd-dec1", "matt@example.com", "user+pinged"))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	// Priority bumped 2 → 1 (reflected onto the seeded bead by the fake).
	if got := f.beads["gd-dec1"].Priority; got != 1 {
		t.Errorf("priority not bumped to 1, got %d", got)
	}
	// Reflag fields written.
	if len(f.updateFieldsHistory) == 0 {
		t.Fatal("expected UpdateBeadFieldsTyped to be called")
	}
	fields := f.updateFieldsHistory[len(f.updateFieldsHistory)-1].Fields
	if fields["reflag_count"] != "1" {
		t.Errorf("reflag_count = %v, want 1", fields["reflag_count"])
	}
	if fields["reflagged_by"] != "matt@example.com" {
		t.Errorf("reflagged_by = %v, want matt@example.com", fields["reflagged_by"])
	}
	if fields["reflag_reason"] != "user pinged" { // '+' decodes to space
		t.Errorf("reflag_reason = %v, want 'user pinged'", fields["reflag_reason"])
	}
	// Audit comment recorded.
	if f.addCommentCalls == 0 {
		t.Error("expected an audit comment")
	}
	// The re-rendered context fragment reflects the new count.
	if body := w.Body.String(); !strings.Contains(body, "Reflagged") {
		t.Errorf("fragment missing reflag history: %s", body)
	}
}

func TestReflagDecision_AddsEscalatedLabelAtP1(t *testing.T) {
	f := reflagFake()
	f.beads["gd-dec1"].Priority = 2 // 2 → 1 triggers the escalated label
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, reflagRequest("gd-dec1", "matt@example.com", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !slices.Contains(f.beads["gd-dec1"].Labels, "escalated") {
		t.Errorf("expected escalated label, got %v", f.beads["gd-dec1"].Labels)
	}
}

func TestReflagDecision_IncrementsExistingCount(t *testing.T) {
	f := reflagFake()
	f.beads["gd-dec1"].Fields["reflag_count"] = "3"
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, reflagRequest("gd-dec1", "matt@example.com", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	fields := f.updateFieldsHistory[len(f.updateFieldsHistory)-1].Fields
	if fields["reflag_count"] != "4" {
		t.Errorf("reflag_count = %v, want 4", fields["reflag_count"])
	}
}

func TestReflagDecision_RejectsResolvedDecision(t *testing.T) {
	f := reflagFake()
	f.beads["gd-dec1"].Status = "closed"
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, reflagRequest("gd-dec1", "matt@example.com", ""))

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for resolved decision, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Error("must not write reflag fields on a resolved decision")
	}
	if body := w.Body.String(); !strings.Contains(body, "already resolved") {
		t.Errorf("expected inline 'already resolved' message: %s", body)
	}
}

func TestReflagDecision_RejectsUnauthenticated(t *testing.T) {
	f := reflagFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, reflagRequest("gd-dec1", "", ""))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Error("must not write on an unauthenticated reflag")
	}
}

func TestReflagDecision_RejectsNonDecision(t *testing.T) {
	f := newFake() // gd-aaa is a task
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, reflagRequest("gd-aaa", "matt@example.com", ""))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-decision, got %d", w.Code)
	}
}

func TestReflagDecision_IdempotentAtP0(t *testing.T) {
	f := reflagFake()
	f.beads["gd-dec1"].Priority = 0 // already critical
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, reflagRequest("gd-dec1", "matt@example.com", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Priority stays at 0 — reflag is idempotent at critical.
	if got := f.beads["gd-dec1"].Priority; got != 0 {
		t.Errorf("priority should stay 0, got %d", got)
	}
	// But reflag_count still increments.
	fields := f.updateFieldsHistory[len(f.updateFieldsHistory)-1].Fields
	if fields["reflag_count"] != "1" {
		t.Errorf("reflag_count = %v, want 1", fields["reflag_count"])
	}
}
