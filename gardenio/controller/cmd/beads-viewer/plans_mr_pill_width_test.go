package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPlansIndex_MRPillClampedToCard guards the iter-89 /plans card MR-pill
// width fix (ka-0000006484, plan ka-0000005352). The open-MR pill (.plan-card-mr)
// was capped at a bare 15rem (240px), which EXCEEDS the ~210px content box of a
// 239px board-lane card — so on the board the pill and its ellipsis-guarded title
// overran the card's right edge instead of ellipsizing INSIDE it, defeating the
// point of .plan-card-mr-title's overflow:hidden. Measured live (2026-08-15):
// 30/33 rendered pills spilled exactly 32px past the card border.
//
// The fix is one declaration: max-width goes from `15rem` to `min(100%, 15rem)`,
// which clamps the pill to the card's content box on the narrow board (title
// ellipsizes, 0 overflow) while keeping the 15rem ceiling on the wide grid card.
// This is a served-HTML string guard (the CSS is inlined in the plans.html
// <style> block), so it reds if a future refactor drops the min() clamp back to a
// bare fixed cap — the exact regression that reintroduces the overflow.
func TestPlansIndex_MRPillClampedToCard(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The clamp must ride the .plan-card-mr rule. min(100%, 15rem) collapses to
	// the card's content box on a board lane and the 15rem ceiling on grid — the
	// two together being what keeps the pill inside the card at BOTH widths.
	if !strings.Contains(body, "min(100%, 15rem)") {
		t.Errorf(".plan-card-mr is missing the min(100%%, 15rem) width clamp — the pill will overflow the narrow board lane")
	}

	// Fail-loud on the pre-fix shape: a bare `max-width: 15rem` (no min()) is the
	// exact declaration that let the pill spill 32px past the 239px card. Keyed
	// with the space after the colon so it doesn't false-match `min(100%, 15rem)`.
	if strings.Contains(body, "max-width: 15rem;") {
		t.Errorf(".plan-card-mr still carries the bare `max-width: 15rem` cap that overflows the board lane; expected min(100%%, 15rem)")
	}
}
