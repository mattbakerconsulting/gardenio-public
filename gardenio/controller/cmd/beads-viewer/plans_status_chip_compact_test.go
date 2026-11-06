package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestPlansIndex_StatusChipCompact guards the iter-92 /plans card status-chip
// compaction (gd-2EEHpBmEuX, plan ka-0000005352). Every other chip in the first
// .plan-card-meta row carries a compact card-context size — .plan-lifecycle-chip
// and .chip-type are 0.72rem / 0.1rem-0.5rem padding, .plan-prio-chip smaller
// still — but .chip-status alone still wore the fat base .chip filter-pill
// padding (layout.html: `.chip { padding: 0.2rem 0.6rem; font-size: 0.9em; }`),
// making it the TALLEST chip in that row (30px vs the peers' 22px) on all 80
// board cards. Because .plan-card-meta is align-items:center the status chip set
// the whole row's line height, so it cost ~8px on every card (671px board-wide)
// and forced a wrapped meta line on the wide "blocked · ✓ cleared" cards.
//
// The fix is one card-scoped rule: `.plan-card .chip-status { padding: 0.1rem
// 0.5rem; font-size: 0.72rem; }`, matching the .plan-lifecycle-chip idiom. This
// is a served-HTML string guard (the CSS is inlined in the plans.html <style>
// block) — it reds if a refactor drops the compact override and the status chip
// silently balloons back to the fat base pill.
func TestPlansIndex_StatusChipCompact(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The card-scoped compact override must be present. Matched with a tolerant
	// whitespace regex so reordering the two declarations or reflowing the rule
	// does not false-red, while still requiring both the compact padding and the
	// 0.72rem font that bring the chip down to its meta-row peers' height.
	rule := regexp.MustCompile(`\.plan-card\s+\.chip-status\s*\{[^}]*\}`)
	m := rule.FindString(body)
	if m == "" {
		t.Fatalf(".plan-card .chip-status compact rule missing — the status chip will balloon back to the fat base .chip pill and set the meta-row line height on every board card")
	}
	if !strings.Contains(m, "padding: 0.1rem 0.5rem") {
		t.Errorf(".plan-card .chip-status must carry the compact `padding: 0.1rem 0.5rem` (matching .plan-lifecycle-chip); got %q", m)
	}
	if !strings.Contains(m, "font-size: 0.72rem") {
		t.Errorf(".plan-card .chip-status must carry the compact `font-size: 0.72rem` (matching .plan-lifecycle-chip); got %q", m)
	}
}
