package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// plans_detail_header_test.go — Phase 4 (ka-0000006040): the /plans/{id} detail
// header now wires the plan epic's own fields into the layout — the badge
// (fields.icon_svg_url, badge_image.go) LEFT of the title, the one-line goal (fields.goal)
// as a subtitle, and the epic's created stamp beside the existing updated stamp.
// Everything renders off the already-fetched .Bead (zero extra daemon call) and
// self-suppresses when its field is absent, so a plain plan renders as before.

// TestHandlePlanDetail_RendersIconAndGoal proves a plan carrying
// fields.icon_svg_url + fields.goal surfaces both on the detail header: the icon
// as an <img> LEFT of the title, and the goal as the subtitle.
func TestHandlePlanDetail_RendersIconAndGoal(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{
		"icon_svg_url": "/image/plans/badge-abc123.svg",
		"goal":         "Give every gardenio plan its own generated badge.",
	}
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The icon rides in the title row as an <img> pointing at its stored URL.
	if !strings.Contains(body, `class="badge-64"`) {
		t.Errorf("detail header missing the plan icon element, body:\n%s", body)
	}
	if !strings.Contains(body, `src="/image/plans/badge-abc123.svg"`) {
		t.Errorf("detail header icon should carry the stored icon_svg_url")
	}
	// The icon sits inside the title row so it renders LEFT of the title.
	if !strings.Contains(body, `class="plan-title-row"`) {
		t.Errorf("detail header missing the icon+title flex row")
	}
	// The goal renders as the header subtitle.
	if !strings.Contains(body, `class="plan-header-goal"`) {
		t.Errorf("detail header missing the goal subtitle element")
	}
	if !strings.Contains(body, "Give every gardenio plan its own generated badge.") {
		t.Errorf("detail header should render the plan goal text")
	}
}

// TestHandlePlanDetail_NoIconOrGoalWhenAbsent proves the icon + goal seam is
// self-suppressing: a plan with no icon_svg_url / goal fields emits neither
// element, so the header renders exactly as it did before this wiring.
func TestHandlePlanDetail_NoIconOrGoalWhenAbsent(t *testing.T) {
	// The default seed carries neither field.
	mux := mkServer(t, seedPlansFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `class="badge-64"`) {
		t.Errorf("detail header must not render an icon when icon_svg_url is unset")
	}
	if strings.Contains(body, `class="plan-header-goal"`) {
		t.Errorf("detail header must not render a goal subtitle when goal is unset")
	}
}

// TestHandlePlanDetail_RendersCreatedStamp proves the header carries a "created
// <ago>" origin stamp off the epic's own CreatedAt, with the precise instant in
// the tooltip, beside the existing "updated <ago>" stamp.
func TestHandlePlanDetail_RendersCreatedStamp(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].CreatedAt = time.Date(2026, 6, 20, 8, 15, 0, 0, time.UTC)
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="meta plan-header-created"`) {
		t.Errorf("detail header missing the created stamp element, body:\n%s", body)
	}
	if !strings.Contains(body, "created ") {
		t.Errorf("detail header missing the labelled 'created <ago>' text")
	}
	if !strings.Contains(body, `title="Created 2026-06-20 08:15:00 UTC"`) {
		t.Errorf("detail header created stamp should carry the epic's create instant in its tooltip")
	}
}

// TestHandlePlanDetail_IconURLIsSanitized proves the icon src passes through
// html/template's URL filter — a javascript: URL is neutralized, not emitted
// verbatim, so a hostile field value cannot inject script.
func TestHandlePlanDetail_IconURLIsSanitized(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{
		"icon_svg_url": "javascript:alert(1)",
	}
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "javascript:alert(1)") {
		t.Errorf("hostile icon_svg_url must be filtered by html/template, not emitted verbatim")
	}
}
