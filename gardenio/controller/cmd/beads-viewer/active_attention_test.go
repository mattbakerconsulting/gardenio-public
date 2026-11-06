package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// active_attention_test.go — ka-0000002647.
//
// The /active page was promoted into simple-mode CORE once it became a real
// work surface rather than a passive recency list. The two features that earn
// that promotion are covered here:
//
//  1. A "Needs attention" shortlist at the top of the page — the in-progress
//     work items blocked on an open decision (⏸) or carrying a report awaiting
//     review (📄), ordered by priority then recency.
//  2. Blocked / Reports counts in the top-of-page stats banner.

func activeAttentionBeads() []*beadsapi.BeadDetail {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	return []*beadsapi.BeadDetail{
		{
			ID:                      "gd-blocked",
			Title:                   "Waiting on a decision",
			Type:                    "task",
			Status:                  "in_progress",
			Priority:                1,
			Labels:                  []string{"project:gardenio"},
			UpdatedAt:               now,
			BlockedOnDecision:       true,
			BlockedOnDecisionBeadID: "gd-thedecision",
		},
		{
			ID:        "gd-report",
			Title:     "Has a report to read",
			Type:      "task",
			Status:    "in_progress",
			Priority:  2,
			Labels:    []string{"project:gardenio"},
			UpdatedAt: now.Add(-time.Hour),
			HasReport: true,
		},
		{
			ID:        "gd-plain",
			Title:     "Just chugging along",
			Type:      "task",
			Status:    "in_progress",
			Priority:  3,
			Labels:    []string{"project:gardenio"},
			UpdatedAt: now.Add(-2 * time.Hour),
		},
	}
}

func renderActive(t *testing.T) string {
	t.Helper()
	beads := activeAttentionBeads()
	byID := map[string]*beadsapi.BeadDetail{}
	for _, b := range beads {
		byID[b.ID] = b
	}
	f := &fakeDaemon{
		beads:    byID,
		listResp: &beadsapi.ListBeadsResult{Beads: beads, Total: len(beads)},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest(http.MethodGet, "/active?operated_by=", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /active = %d, want 200\nbody=%s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestActive_NeedsAttentionSection asserts the blocked + report rows surface
// in the "Needs attention" shortlist, the plain in-progress row does not, and
// the blocked row leads (lower priority number wins the ordering).
func TestActive_NeedsAttentionSection(t *testing.T) {
	body := renderActive(t)

	sectionStart := strings.Index(body, `class="active-attention"`)
	if sectionStart < 0 {
		t.Fatalf("no Needs-attention section rendered\nbody=%s", body)
	}
	// Scope assertions to the section markup so a row's presence in its
	// project group below cannot satisfy the check.
	sectionEnd := strings.Index(body[sectionStart:], "</section>")
	if sectionEnd < 0 {
		t.Fatalf("unterminated Needs-attention section")
	}
	section := body[sectionStart : sectionStart+sectionEnd]

	if !strings.Contains(section, `id="project-row-gd-blocked"`) {
		t.Errorf("Needs-attention section missing the blocked row\nsection=%s", section)
	}
	if !strings.Contains(section, `id="project-row-gd-report"`) {
		t.Errorf("Needs-attention section missing the report row\nsection=%s", section)
	}
	if strings.Contains(section, `id="project-row-gd-plain"`) {
		t.Errorf("plain in-progress row should NOT be in Needs-attention\nsection=%s", section)
	}

	// Priority ordering: the blocked row (P1) precedes the report row (P2).
	if i, j := strings.Index(section, "gd-blocked"), strings.Index(section, "gd-report"); i > j {
		t.Errorf("blocked row (P1) should lead report row (P2); got blocked@%d report@%d", i, j)
	}
}

// TestActive_BlockedAndReportCounts asserts the stats banner surfaces the two
// new counts.
func TestActive_BlockedAndReportCounts(t *testing.T) {
	body := renderActive(t)
	for _, want := range []string{
		`<span class="stats-banner-label">Blocked</span>`,
		`<span class="stats-banner-label">Reports</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stats banner missing %q", want)
		}
	}
	// One blocked + one report bead were seeded, so each count reads 1. Assert
	// on the num/label pair so the value is anchored to its own tile.
	for _, want := range []string{
		`<span class="stats-banner-num">1</span>
    <span class="stats-banner-label">Blocked</span>`,
		`<span class="stats-banner-num">1</span>
    <span class="stats-banner-label">Reports</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stats banner missing expected count tile %q", want)
		}
	}
}

// TestActive_NoAttentionWhenNoneFlagged asserts the section is absent when no
// in-progress work item is blocked or carries a report.
func TestActive_NoAttentionWhenNoneFlagged(t *testing.T) {
	plain := &beadsapi.BeadDetail{
		ID:        "gd-only",
		Title:     "Nothing to see",
		Type:      "task",
		Status:    "in_progress",
		Labels:    []string{"project:gardenio"},
		UpdatedAt: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
	}
	f := &fakeDaemon{
		beads:    map[string]*beadsapi.BeadDetail{"gd-only": plain},
		listResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plain}, Total: 1},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest(http.MethodGet, "/active?operated_by=", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /active = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), `class="active-attention"`) {
		t.Errorf("Needs-attention section should be absent when nothing is flagged")
	}
}
