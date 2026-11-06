package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestPlansIndex_MetaRowGapTight guards the iter-93 /plans card meta-row row-gap
// tightening (plan ka-0000005352). The two `.plan-card-meta` chip-strips are the
// #1 vertical sink on the live board — measured ~10,706px total across 160 rows
// (80 cards × 2), the top strip wrapping to a median 4 lines (3–6) on nearly
// every card. The prior single `gap: 0.3rem` set the ROW gap (vertical space
// between wrapped chip lines) equal to the column gap, so every extra wrapped
// line paid the full 0.3rem.
//
// The fix splits the shorthand into `column-gap: 0.3rem` (horizontal breathing
// room between chips unchanged) + a tightened `row-gap: 0.18rem` so the 3–6
// wrapped lines pack closer. Measured live (390px board lane, 80 cards): −476px
// board-wide, ~6px/card, zero chip removed and zero horizontal change. This is a
// served-HTML string guard (the CSS is inlined in the plans.html <style> block)
// — it reds if a refactor collapses the split back to a single `gap` shorthand
// and the wrapped meta lines silently re-inflate to the column gap.
func TestPlansIndex_MetaRowGapTight(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The .plan-card-meta rule must be present. Matched with a tolerant regex so
	// reordering declarations or reflowing the rule does not false-red.
	rule := regexp.MustCompile(`\.plan-card-meta\s*\{[^}]*\}`)
	m := rule.FindString(body)
	if m == "" {
		t.Fatalf(".plan-card-meta rule missing from served /plans HTML")
	}
	// The row-gap must be the tightened value — the whole point of the iter-93 pass.
	if !strings.Contains(m, "row-gap: 0.18rem") {
		t.Errorf(".plan-card-meta must carry the tightened `row-gap: 0.18rem`; got %q", m)
	}
	// The column-gap must stay at the wider value so horizontal breathing room is
	// unchanged — the density win is vertical-only, not a crowding of chips.
	if !strings.Contains(m, "column-gap: 0.3rem") {
		t.Errorf(".plan-card-meta must keep `column-gap: 0.3rem` (horizontal spacing unchanged); got %q", m)
	}
	// Fail-without: a collapsed single `gap:` shorthand would re-inflate the row
	// gap to the column value, undoing the pass. Reject it explicitly.
	if regexp.MustCompile(`[\s;{]gap:\s*0\.3rem`).MatchString(m) {
		t.Errorf(".plan-card-meta must NOT use a single `gap: 0.3rem` shorthand — that sets row-gap == column-gap and re-inflates the wrapped meta lines; use split column-gap/row-gap; got %q", m)
	}
}
