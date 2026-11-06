package main

// plans_focus_step_test.go — the deep-link focus feature (ka-0000000150):
// clicking the "now / next" chip on a /plans index card jumps to the plan and
// highlight+scrolls the clicked step. This covers the two server halves: the
// index card renders a #step-{ID} deep-link, and the detail timeline row
// carries the matching id="step-{ID}" anchor. (The 10s highlight+scroll itself
// is client-side, in plan_focus_step.js.)

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestPickCurrentOrNextStep_CarriesID proves the index card's current/next step
// projection carries the step bead id — the anchor the deep-link is built from.
func TestPickCurrentOrNextStep_CarriesID(t *testing.T) {
	children := map[string]*beadsapi.BeadDetail{
		"gd-a": {ID: "gd-a", Title: "A", Type: "task", Status: "closed"},
		"gd-b": {ID: "gd-b", Title: "B", Type: "task", Status: "in_progress"},
	}
	step := pickCurrentOrNextStep([]string{"gd-a", "gd-b"}, children, nil)
	if step == nil || step.ID != "gd-b" {
		t.Fatalf("current step = %+v, want the in_progress gd-b with its ID", step)
	}
	if step.Status != "in_progress" {
		t.Errorf("status = %q, want in_progress (the 'now' chip)", step.Status)
	}
}

// seedInProgressStepFake gives the plan a single in_progress step so the index
// card renders a "now" chip that deep-links to it.
func seedInProgressStepFake() *fakeDaemon {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	f.beads["gd-step1"].Title = "Wire the route"
	return f
}

// TestPlansIndex_NowChipDeepLinksToStep proves the /plans index card's "now"
// chip is an <a> pointing at /plans/{plan}#step-{stepID} — the deep link
// plan_focus_step.js reads on arrival.
func TestPlansIndex_NowChipDeepLinksToStep(t *testing.T) {
	mux := mkServer(t, seedInProgressStepFake())
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/plans/gd-plan1#step-gd-step1"`) {
		t.Errorf("index card should deep-link the now chip to /plans/gd-plan1#step-gd-step1; body:\n%s", body)
	}
	if !strings.Contains(body, "plan-card-step-link") {
		t.Errorf("now chip should render as the affordant deep-link class plan-card-step-link")
	}
}

// TestPlanDetail_StepRowCarriesAnchor proves the detail timeline row carries the
// id="step-{ID}" anchor the deep link lands on, and that plan_focus_step.js is
// wired into the page.
func TestPlanDetail_StepRowCarriesAnchor(t *testing.T) {
	mux := mkServer(t, seedInProgressStepFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="step-gd-step1"`) {
		t.Errorf("timeline row should carry id=step-gd-step1 as the deep-link anchor")
	}
	if !strings.Contains(body, "plan_focus_step.js") {
		t.Errorf("plan_detail should load plan_focus_step.js")
	}
}

// TestPlansIndex_NextChipAlsoDeepLinks proves a plan whose only open step is
// queued (not in_progress) still deep-links its "next" chip — the feature is
// not limited to in_progress rows.
func TestPlansIndex_NextChipAlsoDeepLinks(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "open" // queued → "next"
	f.beads["gd-step1"].CreatedAt = time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `href="/plans/gd-plan1#step-gd-step1"`) {
		t.Errorf("next chip should also deep-link to the step; body:\n%s", body)
	}
}
