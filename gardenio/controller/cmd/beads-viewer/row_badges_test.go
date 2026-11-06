package main

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// R5 (gd-VELm4MvAW3) — render 📄 (has-report) and ⏸ (blocked-on-decision)
// badges in the project_row partial, the bead_show header, and (since /active
// + /p/<project> share project_row) the active-list and project-view rows.

func renderRowBadges(t *testing.T, row any, basePath string) string {
	t.Helper()
	funcs := template.FuncMap{
		"dict": func(pairs ...any) (map[string]any, error) {
			out := map[string]any{}
			for i := 0; i+1 < len(pairs); i += 2 {
				k, ok := pairs[i].(string)
				if !ok {
					return nil, errors.New("dict key not string")
				}
				out[k] = pairs[i+1]
			}
			return out, nil
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/row_badges.html",
	)
	if err != nil {
		t.Fatalf("parse row_badges template: %v", err)
	}
	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, "row_badges", map[string]any{
		"Row":      row,
		"BasePath": basePath,
	}); err != nil {
		t.Fatalf("render row_badges: %v", err)
	}
	return buf.String()
}

func TestRowBadges_HasReportShows(t *testing.T) {
	v := projectBeadView{ID: "gd-x", Type: "task", HasReport: true}
	out := renderRowBadges(t, v, "/bv")
	if !strings.Contains(out, "bead-row-badge-report") {
		t.Errorf("expected report badge present: %s", out)
	}
	if !strings.Contains(out, "📄") {
		t.Errorf("expected 📄 glyph: %s", out)
	}
	if !strings.Contains(out, `href="/bv/gd-x"`) {
		t.Errorf("expected report badge to link to bead detail: %s", out)
	}
}

func TestRowBadges_NoReportNoBadge(t *testing.T) {
	v := projectBeadView{ID: "gd-x", Type: "task"}
	out := renderRowBadges(t, v, "/bv")
	if strings.Contains(out, "bead-row-badge-report") {
		t.Errorf("expected no report badge when has_report=false: %s", out)
	}
}

func TestRowBadges_BlockedOnDecisionShowsAndLinks(t *testing.T) {
	v := projectBeadView{
		ID:                      "gd-x",
		Type:                    "task",
		BlockedOnDecision:       true,
		BlockedOnDecisionBeadID: "gd-dec01",
	}
	out := renderRowBadges(t, v, "/bv")
	if !strings.Contains(out, "bead-row-badge-blocked") {
		t.Errorf("expected blocked badge: %s", out)
	}
	if !strings.Contains(out, "⏸") {
		t.Errorf("expected ⏸ glyph: %s", out)
	}
	if !strings.Contains(out, `href="/bv/gd-dec01"`) {
		t.Errorf("expected blocked badge to link to the decision id: %s", out)
	}
	if !strings.Contains(out, `title="Blocked on open decision"`) {
		t.Errorf("expected hover title: %s", out)
	}
}

func TestRowBadges_BlockedNoDecisionIDFallsBackToDecisionsPage(t *testing.T) {
	v := projectBeadView{ID: "gd-x", Type: "task", BlockedOnDecision: true}
	out := renderRowBadges(t, v, "/bv")
	if !strings.Contains(out, `href="/bv/decisions"`) {
		t.Errorf("expected fallback to /decisions: %s", out)
	}
}

func TestRowBadges_DecisionRowsSuppressBlockedBadge(t *testing.T) {
	// /decisions page renders type=decision rows. Per spec, ⏸ is suppressed
	// because every row on that page is itself a decision — adding the badge
	// is noise. 📄 still applies (resolved decisions with reports).
	v := projectBeadView{
		ID:                      "gd-dec01",
		Type:                    "decision",
		BlockedOnDecision:       true,
		BlockedOnDecisionBeadID: "gd-dec01",
		HasReport:               true,
	}
	out := renderRowBadges(t, v, "/bv")
	if strings.Contains(out, "bead-row-badge-blocked") {
		t.Errorf("⏸ should be suppressed on type=decision rows: %s", out)
	}
	if !strings.Contains(out, "bead-row-badge-report") {
		t.Errorf("📄 should still render on a resolved decision with a report: %s", out)
	}
}

func TestRowBadges_NoBadgesEmitsNothingExtra(t *testing.T) {
	// When neither badge fires, the partial must not emit the wrapper span —
	// so rows without badges keep identical layout to the pre-R5 baseline.
	v := projectBeadView{ID: "gd-x", Type: "task"}
	out := renderRowBadges(t, v, "/bv")
	if strings.Contains(out, "bead-row-badges") {
		t.Errorf("expected no wrapper span when no badges fire: %s", out)
	}
}

func TestProjectViewFromBead_BadgeFieldsPlumbed(t *testing.T) {
	b := &beadsapi.BeadDetail{
		ID:                      "gd-x",
		Type:                    "task",
		Status:                  "open",
		HasReport:               true,
		BlockedOnDecision:       true,
		BlockedOnDecisionBeadID: "gd-dec01",
	}
	v := projectViewFromBead(b, "user@example.com", "gardenio", "")
	if !v.HasReport {
		t.Errorf("HasReport not plumbed")
	}
	if !v.BlockedOnDecision {
		t.Errorf("BlockedOnDecision not plumbed")
	}
	if v.BlockedOnDecisionBeadID != "gd-dec01" {
		t.Errorf("BlockedOnDecisionBeadID = %q", v.BlockedOnDecisionBeadID)
	}
}

func TestProjectRow_BadgesRenderInProjectRow(t *testing.T) {
	v := projectBeadView{
		ID:                      "gd-task1",
		Title:                   "Plain task",
		Type:                    "task",
		Status:                  "open",
		HasReport:               true,
		BlockedOnDecision:       true,
		BlockedOnDecisionBeadID: "gd-dec01",
	}
	out := renderProjectRow(t, v)
	if !strings.Contains(out, "bead-row-badge-report") {
		t.Errorf("project_row should include the 📄 badge when HasReport=true: %s", out)
	}
	if !strings.Contains(out, "bead-row-badge-blocked") {
		t.Errorf("project_row should include the ⏸ badge when BlockedOnDecision=true: %s", out)
	}
}

func TestBeadShowHeader_RendersBadges(t *testing.T) {
	fake := newFake()
	fake.beads["gd-with-badges"] = &beadsapi.BeadDetail{
		ID:                      "gd-with-badges",
		Type:                    "task",
		Title:                   "Task with badges",
		Status:                  "open",
		Labels:                  []string{"project:gardenio"},
		HasReport:               true,
		BlockedOnDecision:       true,
		BlockedOnDecisionBeadID: "gd-dec99",
	}
	mux := mkServer(t, fake)
	req := httptest.NewRequest("GET", "/gd-with-badges", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "bead-row-badge-report") {
		t.Errorf("bead_show header missing 📄 badge")
	}
	if !strings.Contains(body, "bead-row-badge-blocked") {
		t.Errorf("bead_show header missing ⏸ badge")
	}
	if !strings.Contains(body, "/gd-dec99") {
		t.Errorf("bead_show ⏸ badge should link to the decision id")
	}
}

func TestBeadShowHeader_NoBadgesWhenFlagsFalse(t *testing.T) {
	fake := newFake()
	fake.beads["gd-plain"] = &beadsapi.BeadDetail{
		ID:     "gd-plain",
		Type:   "task",
		Title:  "Plain task",
		Status: "open",
		Labels: []string{"project:gardenio"},
	}
	mux := mkServer(t, fake)
	req := httptest.NewRequest("GET", "/gd-plain", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "bead-row-badge-report") || strings.Contains(body, "bead-row-badge-blocked") {
		t.Errorf("plain bead_show header should not emit badges")
	}
}

// Compile-time assertion that newFake exposes a beads-by-id map; consumed
// indirectly above. Kept as a no-op test so a future refactor of newFake
// surfaces here too.
func TestNewFakeHasBeads(t *testing.T) {
	f := newFake()
	if f.beads == nil {
		t.Fatal("newFake().beads must be non-nil for these tests")
	}
	_ = context.Background
}
