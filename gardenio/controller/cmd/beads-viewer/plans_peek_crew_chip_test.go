package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plans_peek_crew_chip_test.go — iter-65 (ka-0000005352): the /plans peek drawer
// HEADER now carries the compact live-crew chip.
//
// THE GAP (measure-first): Matt's ask (thread ka-0000002125, quoted in
// plan_peek.html) was "the plans preview window should show WORKING CREW and
// whether the gardener is LIVE". The gardener-live half rides the plan_agent_chip
// in the header; the working-crew half lived ONLY in the plan_crew roster BELOW
// the entire steps timeline — out of the header's at-a-glance scan. This iter
// folds the same "● N crew · M working" pill the index card face carries into
// .plan-peek-meta beside the gardener chip, reading the SAME planCrewInfo the
// roster below renders (already in the peek data bag), so the header count and
// the roster header can never disagree — zero extra daemon call.
//
// The tests below guard three things: (1) a plan with a live working crew shows
// the header chip WITH the green live-dot + "· N working" facet and the
// #plan-crew link; (2) a crew-less plan renders NO header chip (the roster's own
// empty state carries "No crew yet"); (3) a plan whose crew are all finished
// shows a plain "N crew" header pill with NO live-dot and NO working facet.

// peekBody drives GET /plans/{id}/peek and returns the rendered drawer HTML.
func peekBody(t *testing.T, f *fakeDaemon) string {
	t.Helper()
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /plans/gd-plan1/peek: expected 200, got %d", w.Code)
	}
	return w.Body.String()
}

// peekHeader returns just the <p class="plan-peek-meta">…</p> header block, so an
// assertion on the header chip cannot be satisfied by the roster below it (which
// legitimately renders "1 working" in its own tally).
func peekHeader(t *testing.T, body string) string {
	t.Helper()
	const open = `<p class="plan-peek-meta">`
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatalf("peek body missing header block %q", open)
	}
	rest := body[i:]
	j := strings.Index(rest, "</p>")
	if j < 0 {
		t.Fatalf("peek header block never closed")
	}
	return rest[:j]
}

// TestPlanPeekHeader_LiveCrewChip: a plan with a live working crew shows the
// header crew chip with the live-dot + "· N working" facet, linking to the
// #plan-crew roster in the same drawer.
func TestPlanPeekHeader_LiveCrewChip(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")

	header := peekHeader(t, peekBody(t, f))
	for _, want := range []string{
		"plan-peek-crew",            // the header chip
		"plan-peek-crew-live",       // the live tint (a crew is working now)
		"plan-peek-crew-dot",        // the pulsing live-dot
		"1 crew",                    // the total
		"· 1 working",               // the working facet
		"/plans/gd-plan1#plan-crew", // links to the roster below in the drawer
	} {
		if !strings.Contains(header, want) {
			t.Errorf("peek header missing live-crew chip piece %q; header=%s", want, header)
		}
	}
}

// TestPlanPeekHeader_NoCrewNoChip: a crew-less plan renders NO header crew chip
// (the roster's own empty state carries "No crew yet"). Guards the Total gate so
// a plan with no crew adds no ink to the header.
func TestPlanPeekHeader_NoCrewNoChip(t *testing.T) {
	header := peekHeader(t, peekBody(t, seedPlansFake()))
	if strings.Contains(header, "plan-peek-crew") {
		t.Errorf("crew-less peek header should render NO crew chip; header=%s", header)
	}
}

// TestPlanPeekHeader_FinishedCrewNoLiveDot: a plan whose crew are ALL finished
// shows a plain "N crew" header pill with NO live-dot and NO working facet — the
// calm, no-alarm reading the index card face uses for a plan with only history.
func TestPlanPeekHeader_FinishedCrewNoLiveDot(t *testing.T) {
	f := seedPlansFake()
	done := seedCrew(f, "gd-crewB", "gardenio-wire-route-xyz", "gd-step1", "done", "closed")
	done.Fields["wrapup"] = `{"tldr":"Wired the route; MR merged."}`

	header := peekHeader(t, peekBody(t, f))
	if !strings.Contains(header, "plan-peek-crew") {
		t.Fatalf("a plan with finished crew should still show the header crew chip; header=%s", header)
	}
	if !strings.Contains(header, "1 crew") {
		t.Errorf("header crew chip should read the total %q; header=%s", "1 crew", header)
	}
	if strings.Contains(header, "plan-peek-crew-live") {
		t.Errorf("a plan with NO working crew must not tint the header chip live; header=%s", header)
	}
	if strings.Contains(header, "plan-peek-crew-dot") {
		t.Errorf("a plan with NO working crew must not render the live-dot; header=%s", header)
	}
	if strings.Contains(header, "working") {
		t.Errorf("a plan with NO working crew must not render the working facet; header=%s", header)
	}
}

// TestPlanPeekHeader_StuckCrewFacet (iter-71, ka-0000005352): an OPEN (reachable)
// stuck crew shows the LOUD "· ⚠ N" facet on the header chip. iter-65 folded only
// the HEALTHY working half; the act-now stuck half lived ONLY in the roster below
// the whole timeline. This is the highest-value triage signal, so it must ride the
// header. Asserts the facet + its red class are INSIDE the header block (not
// satisfied by the roster's own "⚠ N stuck" chip below). FAILS pre-fix.
func TestPlanPeekHeader_StuckCrewFacet(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	// A live working crew AND an OPEN (reachable) stuck one — the mix where the
	// header must read both a healthy green "· 1 working" and a loud red "· ⚠ 1".
	seedCrew(f, "gd-crewOK", "gardenio-wire-route-ok", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-crewStuck", "gardenio-wire-route-stuck", "gd-step1", "rate_limited", "in_progress")

	header := peekHeader(t, peekBody(t, f))
	for _, want := range []string{
		"plan-peek-crew-stuck", // the loud red facet span
		"· &#x26A0; 1",         // ⚠ 1 — the open-stuck count on the face
		"· 1 working",          // the healthy facet still renders beside it
	} {
		if !strings.Contains(header, want) {
			t.Errorf("peek header missing stuck-crew facet piece %q; header=%s", want, header)
		}
	}
}

// TestPlanPeekHeader_NoStuckNoFacet: a healthy plan (working crew, none stuck)
// renders NO stuck facet — the HasOpenStuck gate, so a plan with nothing wedged
// adds no red ink. Guards the fix against over-firing.
func TestPlanPeekHeader_NoStuckNoFacet(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewOK", "gardenio-wire-route-ok", "gd-step1", "working", "in_progress")

	header := peekHeader(t, peekBody(t, f))
	if strings.Contains(header, "plan-peek-crew-stuck") {
		t.Errorf("a plan with no open-stuck crew must render NO stuck facet; header=%s", header)
	}
}

// TestPlanPeekHeader_ClosedFailedNoStuckFacet: a plan whose only failure is a
// CLOSED-failed crew (dead terminal, no reachable pod) renders NO loud stuck facet
// on the header — the reachability split (iter-29/33) reads the same here as on the
// index chip + cockpit roster, so the header never screams a false "act now" for a
// crew with nothing to click through to.
func TestPlanPeekHeader_ClosedFailedNoStuckFacet(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-aSdZpwhQxR", "gardenio-closed-fail", "gd-step1", "failed", "closed")

	header := peekHeader(t, peekBody(t, f))
	if strings.Contains(header, "plan-peek-crew-stuck") {
		t.Errorf("a plan with only closed-failed crew must render NO loud stuck facet on the header; header=%s", header)
	}
}
