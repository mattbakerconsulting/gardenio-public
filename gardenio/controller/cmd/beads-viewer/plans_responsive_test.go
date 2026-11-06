package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPlansIndex_ResponsiveMobileCSS proves the Step 1 (ka-0000000866) mobile
// treatment is present in the served /plans HTML:
//
//  1. the Jira-style board collapses from horizontally-scrolling lanes to ONE
//     stacked column under ~700px (no clipped-right-column, no horizontal
//     overflow at 390px);
//  2. plan-card controls reach a 44px tap target under (pointer: coarse), with
//     the task-grid squares and bulk checkbox given a larger transparent hit
//     area (::after overlay) rather than growing visually.
//
// These are template-level CSS rules inlined in the <style> block, so the test
// renders the page and asserts the rules are emitted. It guards against the
// breakpoints being dropped in a future refactor (the page shipped with ZERO
// width breakpoints before this step). Assertions key on strings UNIQUE to the
// plans.html additions — layout.html also carries a @media (max-width: 700px),
// a @media (pointer: coarse), and a min-height: 44px in its <head> style, so a
// bare presence check on those would pass without this change.
func TestPlansIndex_ResponsiveMobileCSS(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// 1. Board single-column collapse breakpoint. The board's default geometry
	//    is grid-auto-flow: column with overflow-x: auto; the ≤700px rule flips
	//    it to row flow (grid-auto-flow: row — a string absent from layout.html)
	//    with the horizontal scroll dropped, so lanes stack vertically instead
	//    of scrolling off-screen at 390px.
	if !strings.Contains(body, "@media (max-width: 700px)") {
		t.Errorf("/plans is missing the ≤700px board-collapse breakpoint")
	}
	if !strings.Contains(body, "grid-auto-flow: row") {
		t.Errorf("≤700px board rule should switch .plans-board to row flow (grid-auto-flow: row)")
	}
	if !strings.Contains(body, "overflow-x: visible") {
		t.Errorf("≤700px board rule should drop the horizontal scroll (overflow-x: visible)")
	}

	// 2. Coarse-pointer tap targets. The dispatch/approve buttons hit the 44px
	//    comfortable-target floor, and the tiny task-grid squares + bulk
	//    checkbox get a larger hit area via a ::after overlay (they stay small
	//    visually). The ::after selectors are unique to this change.
	if !strings.Contains(body, "@media (pointer: coarse)") {
		t.Errorf("/plans is missing the (pointer: coarse) tap-target treatment")
	}
	if !strings.Contains(body, ".plan-card-dispatch-btn,") && !strings.Contains(body, ".plan-card-dispatch-btn ") {
		t.Errorf("coarse-pointer rules should raise the dispatch/approve buttons to a 44px tap target")
	}
	if !strings.Contains(body, ".plan-taskgrid-sq::after") {
		t.Errorf("task-grid squares should get a transparent ::after hit-area overlay, not grow visually")
	}
	if !strings.Contains(body, ".plans-row-select::after") {
		t.Errorf("bulk-select checkbox should get a 44px ::after hit-area overlay")
	}
}

// TestPlansIndex_TightVerticalRhythm proves the iter-34 (ka-0000005352) density
// pass — tightening the plan-card block padding + inter-row gap to reduce card
// HEIGHT with zero signal loss — is present in the served /plans HTML.
//
// This is the "show more info in less space" half of the mission that every prior
// iteration (1-33) paid for by ADDING a signal; iter-34 instead removes structural
// whitespace. The card padding (0.85rem 1rem) and inter-row gap (0.55rem) had been
// UNTOUCHED since the page's first commit. Measured on the live prod board (123
// cards), the tightened rhythm shrinks every card ~6% and grows none.
//
// The rules are inlined in the plans.html <style> block, so the test renders the
// page and asserts the tightened values are emitted (and the old loose padding is
// gone) — guarding against a future refactor re-loosening the rhythm.
func TestPlansIndex_TightVerticalRhythm(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The tightened .plan-card block rhythm must be present. Each assertion keys on
	// a substring UNIQUE to the .plan-card / .plan-card-meta rules — layout.html (the
	// shared shell served on every page, /plans included) carries its OWN unrelated
	// padding/gap declarations (.quick-spawn-form-inline, .cmdk-empty, …), so a bare
	// value check like "gap: 0.55rem" would pass/fail spuriously. The card-block
	// signature below (padding + fill + gap in ONE substring) appears only here.
	const cardRhythm = "padding: 0.7rem 0.9rem; background: var(--card-bg, #fff);\n" +
		"               display: flex; flex-direction: column; gap: 0.45rem;"
	if !strings.Contains(body, cardRhythm) {
		t.Errorf("/plans .plan-card should use the tightened iter-34 rhythm (padding 0.7rem 0.9rem, gap 0.45rem)")
	}
	// iter-93 (ka-0000005352) split the single iter-34 `gap: 0.3rem` shorthand into
	// `column-gap: 0.3rem` (horizontal breathing room unchanged) + a tightened
	// `row-gap: 0.18rem`, packing the 3–6 wrapped chip lines closer (−476px board-wide).
	// The column gap stays at the iter-34 value; only the vertical rhythm tightened.
	if !strings.Contains(body, ".plan-card-meta { display: flex; flex-wrap: wrap; align-items: center; column-gap: 0.3rem; row-gap: 0.18rem;") {
		t.Errorf("/plans .plan-card-meta should keep the iter-34 inter-chip column-gap (0.3rem) with the iter-93 tightened row-gap (0.18rem)")
	}

	// And the pre-iter-34 loose card-block padding must be gone, catching a revert.
	// Keyed on the card-block signature (padding + --card-bg fill) so it can never
	// collide with layout.html's unrelated 0.85rem declarations.
	if strings.Contains(body, "padding: 0.85rem 1rem; background: var(--card-bg, #fff)") {
		t.Errorf("/plans still emits the pre-iter-34 loose .plan-card padding (0.85rem 1rem)")
	}
}

// TestPlansIndex_DarkSkinChipLegibility proves the plan card and the dense
// signals the 15 density iterations built (ka-0000005352) are themed for the
// dark skin, closing the iteration-16 defect (ka-0000007937). THE ROOT BUG: the
// card is `background: var(--card-bg, #fff)`, but --card-bg is defined by no
// skin — so on the dark skin (--bg #0b0f14) every card rendered a pure-#fff
// rectangle on the black page, and the dense signals' light-mode text colors
// only ever rode that white fill. The fix (a) gives the card an actual dark
// surface (--card-bg) on the dark skin and (b) lifts each signal's text to a
// bright dark-palette tone (all ≥ 4.5× WCAG AA over the dark card, versus the
// 1.2-2.4× the light colors would score on it). The rules are inlined in the
// plans.html <style> block, so the test renders the page and asserts them. It
// guards against a future refactor dropping the dark treatment (or adding a NEW
// light-only card chip) and re-breaking the board on the dark skin — which no
// PACES audit catches, the harness only ever renders the light skin.
func TestPlansIndex_DarkSkinChipLegibility(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// (a) The ROOT-CAUSE fix: the card itself gets a dark --card-bg surface on
	//     the dark skin, so it is no longer a white rectangle on the black page
	//     (and the bright chip text below has a dark surface to read against).
	if !strings.Contains(body, `html[data-skin^="dark"] .plan-card { --card-bg:`) {
		t.Errorf("/plans is missing the dark-skin card surface (--card-bg) — the card would render #fff on the dark page and the bright chip text would be illegible on white")
	}

	// (b) Each dense signal class must have a dark-skin override. These are the
	//     exact selector prefixes emitted by the iteration-16 block; a bare
	//     presence check on "data-skin" alone would pass on the pre-existing
	//     .plan-new-btn / .plans-search dark rules, so key on the CARD-CHIP
	//     classes specifically.
	wantDarkRules := []string{
		`html[data-skin^="dark"] .plan-lifecycle-chip-building`,
		`html[data-skin^="dark"] .plan-card-crew`,
		`html[data-skin^="dark"] .plan-card-crew-live`,
		`html[data-skin^="dark"] .plan-card-crew-failed`,
		`html[data-skin^="dark"] .plan-card-crew-face`,
		`html[data-skin^="dark"] .plan-card-mr`,
		`html[data-skin^="dark"] .plan-card-mr-ci-failed`, // iter-20: failed-CI pill + "+N MR ✕ K" overflow
		`html[data-skin^="dark"] .plan-prio-chip-2`,
		`html[data-skin^="dark"] .plan-step-tally-inprogress`,
		`html[data-skin^="dark"] .plan-step-kind-now`,
		`html[data-skin^="dark"] .plan-step-kind-next`,
		`html[data-skin^="dark"] .plan-step-kind-blocked`,
		`html[data-skin^="dark"] .chip-attention`,
		`html[data-skin^="dark"] .plan-stall-badge`,
		`html[data-skin^="dark"] .plan-esc-badge`,
		`html[data-skin^="dark"] .plan-card-dispatch-note`,
	}
	for _, rule := range wantDarkRules {
		if !strings.Contains(body, rule) {
			t.Errorf("/plans is missing the dark-skin legibility override for %q — the signal would be near-invisible on the dark skin", rule)
		}
	}

	// The bright dark-palette text tones the overrides lift to (all measured
	// ≥ 6.2× WCAG AA over their real effective dark background) — assert the
	// characteristic ones are present so a future edit can't quietly swap them
	// back for a light-mode color while keeping the selector.
	for _, tone := range []string{"#6ee7b7", "#ff8a8a", "#f5d578", "#58a6ff", "#b8c2d0"} {
		if !strings.Contains(body, tone) {
			t.Errorf("/plans dark overrides should use the AA-passing palette tone %s", tone)
		}
	}
}

// TestPlansIndex_CrewChipSelfCaps proves the /plans crew chip cannot spill off
// the card and clip its rightmost facet (ka-0000000417, plan ka-0000005352
// iteration 22). THE BUG: the crew chip carries up to four facets (N crew · M
// working · ⚠ K age · $spend); a fully-loaded chip measures ~283px, wider than
// a desktop board-lane card (~263px). The chip was `white-space: nowrap` with no
// width cap, so it grew straight off the card's right edge and the card/lane's
// overflow SILENTLY CLIPPED the rightmost facet — a plan with real spend
// rendered "· $12" for a true "$128.44" (verified in a rich-card render: the
// chip spilled 36px past the card). That is BOTH a PACES-S overflow (a chip
// exceeding the card box breaks the 0-overflow discipline every prior iteration
// held) AND a PACES-P truth-at-a-glance lie (the card understated spend by 10×).
//
// The fix caps the chip to the card width and lets its facets WRAP in place
// (flex-wrap + max-width:100%), each facet an intact flex item (per-facet
// nowrap) — the SAME self-cap discipline .plan-card-mr (max-width:240px) already
// uses so a long MR pill never spills. A CSS-string presence check is the same
// mechanism the dark-skin legibility tests above use (the served <style> block
// is the artifact); it guards against a future edit reintroducing the uncapped
// nowrap chip. The measured on-card behavior (0 spill, full "$128.44" visible)
// is captured in the MR PROOF via a rich-card Playwright render.
func TestPlansIndex_CrewChipSelfCaps(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The chip must cap to the card and wrap between facets, not overflow. Assert
	// the two properties that make it self-cap: max-width bound + flex-wrap. Keyed
	// on the .plan-card-crew rule specifically (not the dark override) so this
	// can't be satisfied by an unrelated wrap rule elsewhere in the sheet.
	crewRuleIdx := strings.Index(body, ".plan-card-crew {")
	if crewRuleIdx < 0 {
		t.Fatal("/plans is missing the .plan-card-crew rule")
	}
	// Bound the search to the base rule body (up to the next selector) so the
	// assertions describe THIS chip, not a later one.
	ruleEnd := strings.Index(body[crewRuleIdx:], "}")
	if ruleEnd < 0 {
		t.Fatal(".plan-card-crew rule is unterminated")
	}
	rule := body[crewRuleIdx : crewRuleIdx+ruleEnd]
	if !strings.Contains(rule, "flex-wrap: wrap") {
		t.Errorf(".plan-card-crew must flex-wrap so a fully-loaded chip flows onto a second line inside the card instead of spilling off it; rule was %q", rule)
	}
	if !strings.Contains(rule, "max-width: 100%") {
		t.Errorf(".plan-card-crew must cap to the card width (max-width: 100%%) so it cannot grow past the card's right edge and clip the spend facet; rule was %q", rule)
	}
	// The old uncapped nowrap was the defect — assert the whole-chip nowrap is
	// gone from the base rule (the per-facet nowrap lives on the child selector).
	if strings.Contains(rule, "white-space: nowrap") {
		t.Errorf(".plan-card-crew must NOT be whole-chip white-space:nowrap — that is exactly the uncapped-spill bug (ka-0000000417); nowrap belongs on the per-facet child rule, not the chip; rule was %q", rule)
	}
	// The per-facet nowrap that keeps each facet ("⚠ 1 6h" / "$128.44") whole.
	if !strings.Contains(body, ".plan-card-crew > * { white-space: nowrap") {
		t.Errorf("/plans must keep each crew facet intact via a per-facet nowrap rule (.plan-card-crew > *) so wrapping never breaks a facet mid-token")
	}
}

// TestPlanCockpit_DarkSkinSurfaces proves the /plans/{id} COCKPIT — the crew
// roster cards and the full-width chat-thread (plan-agent) lane — is themed for
// the dark skin, closing ka-0000005740 (iteration-18 of plan ka-0000005352).
//
// THE ROOT BUG is the same shape iteration-16 (ka-0000007937) fixed on the
// /plans INDEX, one surface over: every cockpit card is
// `background: var(--card-bg, #fff)`, but NO skin defines --card-bg (the dark
// skin defines --bg/--bg-alt/--text, never --card-bg or --fg). So on the dark
// skin (--bg #0b0f14) the .plan-crew-member cards and the .plan-agent chat lane
// rendered as pure-#fff rectangles on the near-black page — the dark PACES pass
// (MR !6604) measured the desktop cockpit at near_white_ratio 0.207, tripping
// dark_white_island_flag=true. The index was themed in iteration-16; this
// cockpit never was (the harness only ever rendered the LIGHT skin until !6604).
//
// The fix (a) defines --card-bg on the .plan-detail container so every cockpit
// card gets an actual dark surface in one place, (b) gives the always-visible
// compose textarea + step inputs an explicit dark field surface (they have NO
// --card-bg fallback and stayed UA-white), and (c) lifts the riding light-mode
// chip/state/lane text to the bright dark palette iteration-16 validated ≥4.5×
// WCAG AA. The rules are inlined in plan_detail.html's <style> block, so the
// test renders the cockpit and asserts them — guarding against a future refactor
// dropping the dark treatment (or a new light-only cockpit chip) and re-leaking
// white islands onto the dark skin, which no LIGHT-only PACES audit catches.
func TestPlanCockpit_DarkSkinSurfaces(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	// Seed a live crew member + the bound plan-agent so the crew roster and the
	// chat-thread lane both actually render in the cockpit under test.
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// (a) THE ROOT-CAUSE fix: the cockpit container gets a dark --card-bg
	//     surface on the dark skin, so every var(--card-bg,#fff) card on the
	//     page (crew rows, the plan-agent chat lane, phase cards, timeline
	//     markers, action buttons) stops rendering #fff on the black page.
	if !strings.Contains(body, `html[data-skin^="dark"] .plan-detail { --card-bg:`) {
		t.Errorf("cockpit is missing the dark-skin card surface (--card-bg on .plan-detail) — the crew cards + chat lane would render #fff on the dark page")
	}

	// (b) The always-visible compose textarea + step inputs have NO --card-bg
	//     fallback, so they need an EXPLICIT dark field surface or they stay
	//     UA-white — a second white-island class on the dark cockpit.
	if !strings.Contains(body, `html[data-skin^="dark"] .plan-assistant-input`) {
		t.Errorf("cockpit compose textarea is missing its dark-skin field surface — it would render UA-white on the dark page")
	}

	// (c) The two white-island surfaces the bug named — the crew roster cards and
	//     the chat-thread (plan-agent) lane — plus the timeline/phase card text
	//     that falls through to the near-black --fg default once the card goes
	//     dark, must each carry a dark-skin override. These selectors are unique
	//     to the cockpit block; a bare "data-skin" check would pass on the
	//     pre-existing layout.html dark rules, so key on the COCKPIT classes.
	wantDarkRules := []string{
		`html[data-skin^="dark"] .plan-crew-member`,             // crew roster card border
		`html[data-skin^="dark"] .plan-crew-name`,               // crew name text lift
		`html[data-skin^="dark"] .plan-crew-count-working`,      // roster tally chip
		`html[data-skin^="dark"] .plan-assistant-delivery-sent`, // chat lane delivery ack
		`html[data-skin^="dark"] .plan-assistant-event-kind`,    // chat lane activity chip
		`html[data-skin^="dark"] .plan-lifecycle-chip-building`, // chat lane lifecycle chip
		`html[data-skin^="dark"] .plan-tl-title`,                // timeline title text lift
		`html[data-skin^="dark"] .plan-tl-marker`,               // timeline marker text lift
		`html[data-skin^="dark"] .plan-phase-title`,             // phase card title text lift
		`html[data-skin^="dark"] .plan-stall-badge`,             // header stall badge
		`html[data-skin^="dark"] .chip-attention`,               // header "N to answer" chip
	}
	for _, rule := range wantDarkRules {
		if !strings.Contains(body, rule) {
			t.Errorf("cockpit is missing the dark-skin override for %q — the surface/text would be a white island or near-invisible on the dark skin", rule)
		}
	}

	// The bright dark-palette tones the overrides lift to (the SAME palette
	// iteration-16 validated ≥ 6.2× WCAG AA over the dark card) — assert the
	// characteristic ones are present so a future edit can't quietly swap them
	// back for a light-mode color while keeping the selector.
	for _, tone := range []string{"#14181d", "#6ee7b7", "#ff8a8a", "#f5d578", "#58a6ff", "#b8c2d0", "#d6e0ec"} {
		if !strings.Contains(body, tone) {
			t.Errorf("cockpit dark overrides should use the AA-passing palette tone %s", tone)
		}
	}
}
