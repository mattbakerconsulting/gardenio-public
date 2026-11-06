package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPlansIndex_TitleDensity proves the ka-0000002389 plan-card title
// treatment — "smaller text but way more of it" — is present in the served
// /plans HTML.
//
// THE BUG IT GUARDS. Measured on the live prod board (76 cards, chromium,
// 1280x900), the pre-change card showed only 26% of its title text: a median
// of 23 VISIBLE characters against a median title length of 92, and NOT ONE of
// the 76 cards showed a whole title. Shrinking the face alone does not fix
// that, because the binding constraint was never the face — it was WIDTH. The
// title, the gd- id chip, and the relocated freshness stamp (iter-52) all
// shared ONE non-wrapping flex row, so on a 232px board-lane card head the
// chips took ~140px and left the title 98px. At 98px a 2-line clamp can only
// ever hold ~23 characters no matter how small the type gets — the font-only
// variant was measured at 0.75rem/4-line and still showed 26%, unchanged.
//
// THE FIX, and why each half is load-bearing:
//
//  1. The id + freshness tokens are grouped into ONE .plan-card-head-tail box
//     and the head is allowed to flex-wrap, so the tail DROPS to its own
//     sub-row when the title's 11rem flex-basis can't be met (board lanes) and
//     stays inline when it can (grid view, wide cards). This is what actually
//     buys the width: it alone took the board from 26% → 61%.
//  2. The face drops 0.85rem → 0.75rem (line-height 1.3 → 1.22) and the clamp
//     opens 2 → 5 lines, converting that width into text: 61% → 100%.
//
// Together: 100% of title text visible, median 23 → 85 visible chars, 76/76
// whole titles, for +34px of median card height (264 → 298px). Verified in
// board AND grid, at 1280/820/375, on both skins; the narrow-viewport
// horizontal overflow and the per-card scrollWidth spill are byte-identical to
// main (pre-existing, untouched by this change).
//
// The rules are inlined in the plans.html <style> block and the tail markup
// lives in the plan_card.html partial, so the test renders the page and
// asserts both. Each assertion keys on a string UNIQUE to these rules —
// layout.html is served on every page and carries its own unrelated font-size
// / flex-wrap declarations, so a bare value check would pass spuriously.
func TestPlansIndex_TitleDensity(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// 1. The smaller face + opened clamp. Keyed on the full declaration head so
	//    it cannot collide with an unrelated 0.75rem elsewhere in the shell.
	if !strings.Contains(body, ".plan-card-title { font-weight: 600; font-size: 0.75rem; line-height: 1.22;") {
		t.Errorf("/plans .plan-card-title should use the denser 0.75rem/1.22 face (ka-0000002389)")
	}
	if !strings.Contains(body, "-webkit-line-clamp: 5; line-clamp: 5;") {
		t.Errorf("/plans .plan-card-title should clamp to 5 lines, not 2 (ka-0000002389)")
	}

	// 2. The flex-basis is what lets the tail drop on a narrow lane. Without it
	//    the tail never wraps and the clamp has no width to fill — the measured
	//    font-only variant stayed at 26%.
	if !strings.Contains(body, "flex: 1 1 11rem; min-width: 0;") {
		t.Errorf("/plans .plan-card-title should carry the 11rem flex-basis that lets the head tail wrap (ka-0000002389)")
	}
	if !strings.Contains(body, ".plan-card-head { display: flex; flex-wrap: wrap;") {
		t.Errorf("/plans .plan-card-head must be flex-wrap: wrap so the id/freshness tail can drop below the title")
	}
	if !strings.Contains(body, ".plan-card-head-tail { display: flex;") {
		t.Errorf("/plans should define .plan-card-head-tail, the grouped id + freshness box")
	}

	// 3. The tail markup must actually be rendered by the card partial — the CSS
	//    is inert without it, and a template refactor that drops the wrapper
	//    would silently restore the single-row head and the 26% regression.
	if !strings.Contains(body, `<span class="plan-card-head-tail">`) {
		t.Errorf("/plans cards should wrap the id + freshness tokens in .plan-card-head-tail")
	}

	// 4. And the pre-change title face must be gone, catching a revert. Keyed on
	//    the .plan-card-title declaration head so it can never match another rule.
	if strings.Contains(body, ".plan-card-title { font-weight: 600; font-size: 0.85rem; line-height: 1.3;") {
		t.Errorf("/plans still emits the pre-ka-0000002389 0.85rem/2-line title face")
	}
}
