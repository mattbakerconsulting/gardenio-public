package main

// plans_esc_badge_wrap_test.go — iter-82 (ka-0000005352 / gd-hcAkNJMmJq): a
// pure DENSITY (space-saving) pass on the ⛔ "blocked by escalation" badge, the
// mission's "more info in LESS space" half. Measure-first (live prod board,
// 239px board lane): this badge was the single worst vertical sink among the
// attention signals — it ballooned to 55px / 3 lines on ALL 19 cards that
// carried it (100%), while the ⚠ stall badge, MR pills, decision chip, and
// lifecycle chip all stayed one line (21-22px).
//
// Root cause: the badge was a display:flex; flex-wrap:nowrap pill whose HEADLINE
// was a bare text node ("⛔ blocked by escalation ×6"), so the headline wrapped
// mid-phrase AND the "· merge MR" op + "· 1d" age facet spans could not drop as
// whole units. The fix wraps the headline in a .plan-esc-badge-label nowrap span
// and switches the badge to flex-wrap:wrap with a tight row-gap, so the label
// stays intact and each facet drops as a whole unit only when the lane genuinely
// can't fit them inline. Verified live in-browser: 55px -> 40px per affected
// card, ZERO wording change, zero new signal.
//
// These tests guard the win two ways: the rendered badge carries the nowrap
// label span wrapping the WHOLE headline (a revert to the bare text node reds
// here), and the CSS carries the flex-wrap + the label's white-space:nowrap
// rule (a revert to nowrap-on-the-pill reds here). Both are keyed tightly enough
// that an unrelated wrap rule elsewhere in the sheet can't satisfy them.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPlansIndex_EscalationBadgeLabelIsNowrapSpan proves the badge headline
// ("⛔ blocked by escalation[ ×N]") renders inside its own
// .plan-esc-badge-label span — the seam that lets the headline stay intact
// (white-space:nowrap) while the badge wraps its facets as whole units. This is
// the fail-without/pass-with anchor: reverting the template to a bare-text-node
// headline removes this span and reds this test.
func TestPlansIndex_EscalationBadgeLabelIsNowrapSpan(t *testing.T) {
	mux := mkEscOpTestServer(t, "gitlab_mr_merge")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The headline lives in a dedicated label span, with the full "blocked by
	// escalation" phrase inside it (so it can never wrap mid-phrase).
	if !strings.Contains(body, `<span class="plan-esc-badge-label">⛔ blocked by escalation`) {
		t.Errorf("expected the badge headline in a .plan-esc-badge-label nowrap span; body missing it (reverted to a bare text node?)")
	}

	// The label span must CLOSE before the op facet span opens — i.e. the op
	// facet is a sibling of the label, not nested inside it — so the two drop
	// independently when the lane is narrow.
	badgeStart := strings.Index(body, `<a class="plan-esc-badge`)
	if badgeStart < 0 {
		t.Fatalf("could not locate the rendered escalation badge anchor")
	}
	badge := body[badgeStart:]
	if end := strings.Index(badge, "</a>"); end >= 0 {
		badge = badge[:end]
	}
	labelClose := strings.Index(badge, `</span>`)
	opOpen := strings.Index(badge, `plan-esc-badge-op`)
	if labelClose < 0 || opOpen < 0 || labelClose > opOpen {
		t.Errorf("the label span must close before the op facet opens (labelClose=%d, opOpen=%d) — the op facet must be a sibling, not nested in the label", labelClose, opOpen)
	}
}

// TestPlansIndex_EscalationBadgeCSSWrapsByFacet proves the stylesheet carries
// the two rules that make the badge wrap by whole facet instead of ballooning
// mid-phrase: the pill is flex-wrap:wrap, and the headline label is
// white-space:nowrap. A revert to the old nowrap-on-the-pill layout reds here.
func TestPlansIndex_EscalationBadgeCSSWrapsByFacet(t *testing.T) {
	mux := mkEscOpTestServer(t, "gitlab_mr_merge")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The badge pill wraps its facets (flex-wrap:wrap), replacing the old
	// flex-wrap:nowrap that forced the whole run onto ballooning wrapped lines.
	if !strings.Contains(body, ".plan-esc-badge { display: inline-flex; align-items: center; flex-wrap: wrap;") {
		t.Errorf(".plan-esc-badge must be flex-wrap:wrap so facets drop as whole units; CSS rule missing/changed")
	}
	// The badge must NOT re-introduce a nowrap on the whole pill (which would
	// defeat the per-facet wrap and re-balloon the headline).
	if strings.Contains(body, ".plan-esc-badge { display: inline-flex; align-items: center; flex-wrap: nowrap;") {
		t.Errorf(".plan-esc-badge must not be flex-wrap:nowrap — that re-balloons the headline")
	}
	// The headline label keeps its own nowrap so the "blocked by escalation"
	// phrase never breaks mid-word.
	if !strings.Contains(body, ".plan-esc-badge-label { white-space: nowrap; }") {
		t.Errorf(".plan-esc-badge-label must be white-space:nowrap so the headline stays intact; CSS rule missing")
	}
}
