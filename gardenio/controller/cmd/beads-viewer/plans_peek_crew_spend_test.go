package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plans_peek_crew_spend_test.go — iter (ka-0000005352): the /plans peek drawer
// HEADER crew chip now folds the cumulative crew SPEND ("· $N") facet.
//
// THE GAP (measure-first): the peek header crew chip (iter-65 + iter-71 + iter-76)
// carries total / working / (K starting) / runtime-age / stuck, but DROPPED the
// cumulative crew SPEND on-face — the LAST plan-card surface still hiding it. The
// index card face (plan_card.html, iter-12), the /deck card face (deck.html,
// iter-77), AND the cockpit roster header (plan_crew.html) all render "· $N", and
// spend is the mission's #1 first-class operator concern: a plan accretes crew
// that each cost real budget. So an operator opening a peek to triage saw
// who/how-many/working/stuck but had to open the whole cockpit to learn how much
// the plan had burned.
//
// The fold is template-only: .Crew (planCrewInfo) already exposes HasSpend() /
// SpendLabel(), the cockpit-side twins of the index rollup's fields — both sum
// over the SAME crew agent beads via agentSpendUSD, so the peek chip and the
// index/deck/cockpit facets agree by construction (advice ka-0000005924), zero
// extra daemon call.
//
// agentSpendUSD reads total_cost_usd always + live_cost_usd only while the agent
// is live; seedCrew's "working" state is live, so its live_cost_usd counts.

// seedPeekCrewWithSpend seeds a working (live) crew + a done crew, each with a
// known dollar cost, so the peek's planCrewInfo carries a derivable cumulative
// spend. Working (live): $2.00 total + $0.50 live = $2.50 counted. Done (not
// live): $1.75 total, no live fold. Cumulative = $4.25.
func seedPeekCrewWithSpend(t *testing.T) *fakeDaemon {
	t.Helper()
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	work := seedCrew(f, "gd-crewWork", "gardenio-wire-route-work", "gd-step1", "working", "in_progress")
	work.Fields["total_cost_usd"] = "2.00"
	work.Fields["live_cost_usd"] = "0.50"
	done := seedCrew(f, "gd-crewDone", "gardenio-wire-route-done", "gd-step1", "idle", "closed")
	done.Fields["total_cost_usd"] = "1.75"
	return f
}

// TestPlanPeekHeader_CrewSpendFacet: a plan whose crew carry a known cost shows
// the "· $4.25" spend facet in the header chip, ordered AFTER the working facet.
// FAILS pre-fix (the count-only chip renders no spend span).
func TestPlanPeekHeader_CrewSpendFacet(t *testing.T) {
	f := seedPeekCrewWithSpend(t)

	header := peekHeader(t, peekBody(t, f))
	spendSpan := `<span class="plan-peek-crew-spend">· $4.25</span>`
	if !strings.Contains(header, spendSpan) {
		t.Errorf("peek header missing crew-spend facet %q — the peek↔index↔deck↔cockpit crew-spend parity gap is not closed; header=%s", spendSpan, header)
	}
	// The spend facet must render AFTER the working facet (matching the index +
	// deck face ordering: working → stuck → spend).
	iWork := strings.Index(header, "plan-peek-crew-working")
	iSpend := strings.Index(header, "plan-peek-crew-spend")
	if iWork < 0 || iSpend < 0 || iWork > iSpend {
		t.Errorf("crew spend must render AFTER the working facet; workIdx=%d spendIdx=%d header=%s", iWork, iSpend, header)
	}
}

// TestPlanPeekHeader_CrewSpendNoFacetWhenUnknown is the fail-without floor: a crew
// roster with NO reported cost renders no spend facet at all (HasSpend gate), so a
// plan whose crew have not yet reported cost reads exactly as before.
func TestPlanPeekHeader_CrewSpendNoFacetWhenUnknown(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress") // no cost fields

	header := peekHeader(t, peekBody(t, f))
	if strings.Contains(header, "plan-peek-crew-spend") {
		t.Errorf("peek header rendered a spend facet for a crew roster with no reported cost — the HasSpend gate is not holding; header=%s", header)
	}
}

// TestPlanPeekHeader_CrewSpendAgreesWithIndex is the two-derivations-must-agree
// keystone (advice ka-0000005924): the peek header's spend reads the SAME
// planCrewInfo the roster derives, and the index card face reads the SAME
// planCrewRollup — both project the same crew beads over the same cost helper, so
// the "$4.25" the peek header shows must be the one the index card renders.
func TestPlanPeekHeader_CrewSpendAgreesWithIndex(t *testing.T) {
	f := seedPeekCrewWithSpend(t)

	// The index card renders the SAME crew via the /plans board (control).
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("index render: expected 200, got %d", w.Code)
	}
	rows := w.Body.String()
	// The index card's spend span carries a title attribute, so match on the
	// class-open + the dollar value rather than an exact byte string.
	if !strings.Contains(rows, `class="plan-card-crew-spend"`) || !strings.Contains(rows, "· $4.25") {
		t.Fatalf("index card should carry the crew spend '$4.25' (control); has-class=%v has-value=%v", strings.Contains(rows, `class="plan-card-crew-spend"`), strings.Contains(rows, "· $4.25"))
	}

	header := peekHeader(t, peekBody(t, f))
	if !strings.Contains(header, `<span class="plan-peek-crew-spend">· $4.25</span>`) {
		t.Errorf("peek header crew spend must AGREE with the index card's (both derive $4.25); header=%s", header)
	}
}
