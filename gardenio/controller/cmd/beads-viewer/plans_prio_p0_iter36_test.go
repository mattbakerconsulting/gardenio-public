package main

// plans_prio_p0_iter36_test.go — iter-36 (ka-0000005352 / gd-xU48sJR6HC): a P0
// (critical / incident-class) plan must be visually distinguishable from a P1
// (high) plan on the /plans index card AND the /deck card.
//
// THE GAP (measured on the live board, 9 P0 + 24 P1 open plans): the priority
// pill bucketed P0 and P1 into ONE shared CSS rule
// (`.plan-prio-chip-0, .plan-prio-chip-1 { color:#a3282a; background:… }`), so a
// P0 and a P1 rendered an identical tinted-red pill — the only differentiator
// was the 0.7rem digit 0 vs 1. An operator scanning the board for the
// drop-everything plans could not spot them among the high ones. iter-36 splits
// the loud tier's top rung: P0 gets a SOLID filled red (white text) so critical
// pops, P1 keeps the tinted outline. The 3-tier scheme (P0/P1 loud, P2 neutral,
// P3/P4 quiet) is preserved.
//
// The pill MARKUP (`plan-prio-chip-{Priority}`) was already correct — the fix is
// in the page <style>. So the crux asserts the rendered CSS now carries a
// DISTINCT P0 solid-fill rule and that P0/P1 no longer share one selector, on
// both surfaces + both skins.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plansIndexCSS renders the /plans index and returns its full body (the page
// <style> is inlined, so the CSS is in the body).
func renderBody(t *testing.T, target string) string {
	t.Helper()
	f := seedPlansFake()
	f.beads["gd-plan1"].Priority = 0
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: expected 200, got %d", target, w.Code)
	}
	return w.Body.String()
}

// TestPlansIndex_P0SolidFillDistinctFromP1 proves the /plans index page CSS gives
// P0 its own solid-fill rule, distinct from P1 — no longer a shared selector.
func TestPlansIndex_P0SolidFillDistinctFromP1(t *testing.T) {
	body := renderBody(t, "/plans")

	// P0 has its OWN rule with a solid red fill + white text.
	if !strings.Contains(body, ".plan-prio-chip-0 { color: #fff; background: #c62828;") {
		t.Errorf("index CSS missing the P0 solid-fill rule (.plan-prio-chip-0 white on #c62828)")
	}
	// P1 keeps the tinted-red outline it had.
	if !strings.Contains(body, ".plan-prio-chip-1 { color: #a3282a; background: rgba(229,57,53,0.10); }") {
		t.Errorf("index CSS: P1 should keep its tinted-red rule")
	}
	// The OLD shared selector must be gone — P0 and P1 no longer render identically.
	if strings.Contains(body, ".plan-prio-chip-0, .plan-prio-chip-1 {") {
		t.Errorf("index CSS still shares ONE rule for P0+P1 — a P0 is indistinguishable from a P1")
	}
	// Dark skin: P0 gets its own solid-fill override too.
	if !strings.Contains(body, `html[data-skin^="dark"] .plan-prio-chip-0 { color: #fff; background: #d32f2f;`) {
		t.Errorf("index dark CSS missing the P0 solid-fill override")
	}
	if strings.Contains(body, `html[data-skin^="dark"] .plan-prio-chip-0, html[data-skin^="dark"] .plan-prio-chip-1 {`) {
		t.Errorf("index dark CSS still shares ONE rule for P0+P1")
	}
	// The 3-tier scheme is preserved: P2 neutral, P3/P4 quiet still present.
	if !strings.Contains(body, ".plan-prio-chip-2 { color: #8a5a00;") {
		t.Errorf("index CSS: P2 neutral tier must be preserved")
	}
	if !strings.Contains(body, ".plan-prio-chip-3, .plan-prio-chip-4 { color: #4a5568;") {
		t.Errorf("index CSS: P3/P4 quiet tier must be preserved")
	}
}

// TestDeck_P0SolidFillDistinctFromP1 proves the same split on the /deck surface,
// so a critical plan pops identically on both.
func TestDeck_P0SolidFillDistinctFromP1(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Priority = 0
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/deck", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /deck: expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, ".deck-prio-0 { color: #fff; background: #c62828;") {
		t.Errorf("deck CSS missing the P0 solid-fill rule")
	}
	if !strings.Contains(body, ".deck-prio-1 { color: #a3282a; background: rgba(229,57,53,0.10); }") {
		t.Errorf("deck CSS: P1 should keep its tinted-red rule")
	}
	if strings.Contains(body, ".deck-prio-0, .deck-prio-1 {") {
		t.Errorf("deck CSS still shares ONE rule for P0+P1")
	}
	// Dark twin present.
	if !strings.Contains(body, `html[data-skin^="dark"] .deck-prio-0 { color: #fff; background: #d32f2f;`) {
		t.Errorf("deck dark CSS missing the P0 solid-fill override")
	}
}

// TestPlansIndex_P0PillMarkupUnchanged is the belt-and-suspenders check that the
// PILL itself still renders for a P0 (the markup was always correct; the fix is
// CSS-only) — so the split didn't accidentally drop the pill.
func TestPlansIndex_P0PillMarkupUnchanged(t *testing.T) {
	body := renderBody(t, "/plans")
	if !strings.Contains(body, `class="plan-prio-chip plan-prio-chip-0"`) {
		t.Errorf("P0 pill markup missing after the CSS split")
	}
	if !strings.Contains(body, ">P0</span>") {
		t.Errorf("P0 pill should still render the P0 label")
	}
}
