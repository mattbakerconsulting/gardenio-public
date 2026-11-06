package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestPlansIndex_ProgressFacetsWrap proves the /plans progress block cannot spill
// its text facets off the card and clip them (gd-fQSjt2Xww0, plan ka-0000005352
// iteration 91). THE BUG: .plan-progress was ONE flex row (display:flex;
// align-items:center) holding the segmented bar (flex:1) plus every text facet —
// the "N/M steps · X%" label, the iter-58 "· phase P/T" milestone, the iter-59
// "· last ship Nd" recency facet, the iter-84 "⛔ N" step-tally, and the iter-41
// "✓ steps done" ghost marker — as white-space:nowrap SIBLINGS with NO flex-wrap.
// On a 239px board lane, once a card carried more than the bare label the row
// overflowed the card box and SILENTLY CLIPPED its rightmost facets off the right
// edge (measured live 2026-08-15 against the prod daemon: 53 of 81 cards spilled
// up to 165px — the phase/last-ship/tally/ghost facets were RENDERED BUT
// INVISIBLE on ~2/3 of the board). Worse, the flex:1 bar lost its basis fight
// with the nowrap text and collapsed to ~0px width, so the segmented progress bar
// itself vanished on those same cards (measured: 59 of 81 bars < 50px).
//
// The fix makes .plan-progress a COLUMN: the segmented bar sits on its own
// full-width top line (width:100%, flex:none — it can no longer collapse), and
// the text facets move into a .plan-progress-facets sub-row that flex-wraps
// between whole facets inside the card box (each facet keeps its own per-facet
// nowrap so it never breaks mid-token). Zero signal removed — the
// currently-invisible facets become visible and the bar is restored — the
// mission's "more info in LESS space" in its purest form. This is the row-level
// twin of the iter-22 crew-chip self-cap (ka-0000000417) and the iter-89 MR-pill
// clamp (ka-0000006484): a nowrap facet strip with no width cap that spilled and
// clipped.
//
// A CSS-string + template-structure presence check is the same mechanism the
// CrewChipSelfCaps / dark-skin legibility tests use (the served <style> + HTML
// are the artifact); the measured on-card behavior (0 spill, bar full-width,
// every facet visible) is captured in the MR PROOF via a board-width Playwright
// render. It guards against a future edit reverting .plan-progress to the single
// nowrap row that clips.
func TestPlansIndex_ProgressFacetsWrap(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// (1) .plan-progress must be a COLUMN so the bar and the facet row stack
	//     instead of fighting for one nowrap line. Key on the base rule body.
	progRule := cssRuleBody(t, body, ".plan-progress {")
	if !strings.Contains(progRule, "flex-direction: column") {
		t.Errorf(".plan-progress must be flex-direction:column so the bar gets its own full-width line and the facets wrap beneath it (was a single nowrap row that clipped); rule was %q", progRule)
	}

	// (2) The facet sub-row must flex-wrap so a fully-loaded facet strip flows
	//     onto a second line INSIDE the card instead of spilling off its right
	//     edge and clipping the rightmost facet.
	facetsRule := cssRuleBody(t, body, ".plan-progress-facets {")
	if !strings.Contains(facetsRule, "flex-wrap: wrap") {
		t.Errorf(".plan-progress-facets must flex-wrap so the phase/last-ship/tally/ghost facets wrap inside the card instead of clipping off the edge; rule was %q", facetsRule)
	}
	if !strings.Contains(facetsRule, "max-width: 100%") {
		t.Errorf(".plan-progress-facets must cap to the card width (max-width:100%%) so it cannot grow past the card's right edge; rule was %q", facetsRule)
	}

	// (3) The bar must be full-width and NOT flex:1 — the flex:1 in the old
	//     single-row layout is exactly what let the nowrap text starve the bar to
	//     0px. Assert the collapse-proof geometry is present and the collapse-prone
	//     flex:1 is gone from the base rule.
	barRule := cssRuleBody(t, body, ".plan-progress-bar {")
	if !strings.Contains(barRule, "width: 100%") {
		t.Errorf(".plan-progress-bar must be width:100%% so the segmented bar spans the card's own line and cannot collapse against the facet text; rule was %q", barRule)
	}
	if strings.Contains(barRule, "flex: 1") {
		t.Errorf(".plan-progress-bar must NOT be flex:1 — that is exactly what let the nowrap facet text starve the bar to 0px width in the single-row layout (gd-fQSjt2Xww0); rule was %q", barRule)
	}

	// (4) The template must actually WRAP the text facets in the sub-row: the
	//     .plan-progress-facets div must open before the "N/M steps · X%" label
	//     (the first facet). A render-time check on the served HTML so a future
	//     edit that drops the wrapper (facets back to direct .plan-progress
	//     children) reds here even if the CSS survives.
	facetsOpen := strings.Index(body, `<div class="plan-progress-facets">`)
	if facetsOpen < 0 {
		t.Fatal("/plans is missing the .plan-progress-facets sub-row wrapper — the text facets would render as direct .plan-progress children and clip off the card")
	}
	label := regexp.MustCompile(`<span class="plan-progress-label">`).FindStringIndex(body[facetsOpen:])
	if label == nil {
		t.Error("the .plan-progress-label ('N/M steps · X%') must render INSIDE the .plan-progress-facets sub-row so it wraps with its sibling facets instead of clipping")
	}
}

// cssRuleBody returns the body of the CSS rule whose selector+brace is `open`,
// bounded to the next `}` so an assertion describes THAT rule and cannot be
// satisfied by an unrelated declaration elsewhere in the sheet. Mirrors the
// bounded-search idiom in TestPlansIndex_CrewChipSelfCaps.
func cssRuleBody(t *testing.T, body, open string) string {
	t.Helper()
	idx := strings.Index(body, open)
	if idx < 0 {
		t.Fatalf("/plans is missing the %q rule", open)
	}
	end := strings.Index(body[idx:], "}")
	if end < 0 {
		t.Fatalf("%q rule is unterminated", open)
	}
	return body[idx : idx+end]
}
