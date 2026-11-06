package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plans_peek_crew_working_age_test.go — iter-76 (ka-0000005352): the /plans peek
// drawer HEADER crew chip now folds the longest-running live crew's elapsed
// RUNTIME ("· 4h") plus the "(K starting)" booting sub-qualifier onto its
// "· M working" facet.
//
// THE GAP (measure-first): iter-65 folded the healthy "● N crew · M working"
// chip into the peek header, but its "· M working" facet stayed COUNT-ONLY while
// the index card face (plan_card.html:657), the /deck card (deck.html:371), and
// the cockpit roster header (plan_crew.html, MR !7217) all carry BOTH the
// working RUNTIME age and the "(K starting)" sub-qualifier. So the peek header
// was the LAST plan-card surface whose working facet read count-only — a crew
// grinding for an hour read identically to one just dispatched, and a booting
// pod read identical to one actually working.
//
// The fold is template-only: .Crew (planCrewInfo) already exposes
// WorkingOldestAge() (runtime of the longest-running live crew, aged on
// CreatedAt) and HasStarting()/Starting (the 0<K<M booting subset), the
// cockpit-side twins of the index rollup's fields — so the peek chip and the
// index/deck/cockpit facets agree by construction, zero extra daemon call.
//
// seedCrew stamps CreatedAt = crewFixtureNow − 1h, so a working crew's runtime
// age renders "1h"; UpdatedAt = crewFixtureNow − 5m keeps it FRESH (alive).

// TestPlanPeekHeader_WorkingRuntimeAge: a single live working crew shows the
// runtime age ("1h") folded onto the "· 1 working" facet in the header chip.
// FAILS pre-fix (the count-only facet renders no age span).
func TestPlanPeekHeader_WorkingRuntimeAge(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")

	header := peekHeader(t, peekBody(t, f))
	for _, want := range []string{
		"· 1 working", // the count facet still renders
		`<span class="plan-peek-crew-working-age">1h</span>`, // the runtime-age span (crewFixtureNow − 1h CreatedAt)
	} {
		if !strings.Contains(header, want) {
			t.Errorf("peek header missing working-runtime-age piece %q; header=%s", want, header)
		}
	}
	// The runtime span must NOT carry the "(K starting)" qualifier when no crew
	// is booting.
	if strings.Contains(header, "plan-peek-crew-starting") {
		t.Errorf("a plan with no booting crew must render NO starting sub-qualifier; header=%s", header)
	}
}

// TestPlanPeekHeader_StartingSubQualifier: a mix of one grinding crew + one still
// BOOTING (spawning agent_state) shows the "(1 starting)" sub-qualifier — the
// 0<K<M gate (HasStarting). Asserts the qualifier renders BEFORE the runtime age,
// matching the index card's order. FAILS pre-fix.
func TestPlanPeekHeader_StartingSubQualifier(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	// Two LIVE crew: one working (grinding), one spawning (booting) — so
	// Working==2, Starting==1, and 0<Starting<Working fires HasStarting.
	seedCrew(f, "gd-crewGrind", "gardenio-wire-route-grind", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-crewBoot", "gardenio-wire-route-boot", "gd-step1", "spawning", "in_progress")

	header := peekHeader(t, peekBody(t, f))
	for _, want := range []string{
		"· 2 working",              // both live crew count as working
		"plan-peek-crew-starting",  // the booting sub-qualifier span
		"(1 starting)",             // one of the two is still booting
	} {
		if !strings.Contains(header, want) {
			t.Errorf("peek header missing starting sub-qualifier piece %q; header=%s", want, header)
		}
	}
	// Order: "(K starting)" must precede the runtime age, matching plan_card.html.
	iStart := strings.Index(header, "plan-peek-crew-starting")
	iAge := strings.Index(header, "plan-peek-crew-working-age")
	if iStart < 0 || iAge < 0 || iStart > iAge {
		t.Errorf("(K starting) must render BEFORE the runtime age; startIdx=%d ageIdx=%d header=%s", iStart, iAge, header)
	}
}

// TestPlanPeekHeader_AllSpawningNoQualifier: when EVERY live crew is still
// booting (Starting == Working) the bare "· N working" already reads "just
// started", so the "(K starting)" qualifier self-suppresses (the HasStarting
// 0<K<M gate). Guards the fix against over-firing.
func TestPlanPeekHeader_AllSpawningNoQualifier(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewBoot", "gardenio-wire-route-boot", "gd-step1", "spawning", "in_progress")

	header := peekHeader(t, peekBody(t, f))
	if !strings.Contains(header, "· 1 working") {
		t.Fatalf("an all-spawning set still counts as working; header=%s", header)
	}
	if strings.Contains(header, "plan-peek-crew-starting") {
		t.Errorf("an all-spawning set (Starting==Working) must render NO (K starting) qualifier; header=%s", header)
	}
}

// TestPlanPeekHeader_TerminalNoWorkingAge: a TERMINAL (closed) plan folds
// Working→Done (reconcileTerminal), so its header chip carries neither a working
// facet nor a runtime age — a finished plan reads calm history, matching the
// index chip's terminal quiet.
func TestPlanPeekHeader_TerminalNoWorkingAge(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "closed"
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")

	header := peekHeader(t, peekBody(t, f))
	if strings.Contains(header, "plan-peek-crew-working-age") {
		t.Errorf("a terminal plan must render NO working runtime age (Working folds to Done); header=%s", header)
	}
	if strings.Contains(header, "plan-peek-crew-starting") {
		t.Errorf("a terminal plan must render NO starting sub-qualifier; header=%s", header)
	}
}

// TestPlanPeekHeader_WorkingAgeAgreesWithIndex: the two-derivations-must-agree
// keystone (advice ka-0000005924). The peek header's working runtime age reads
// the SAME planCrewInfo the roster derives, and the index card face reads the
// SAME planCrewRollup — both project the same crew beads over the same clock, so
// the "1h" runtime the peek header shows must be the one the index card would.
// This test proves the peek surfaces the age at all; the fail-without half is the
// primary TestPlanPeekHeader_WorkingRuntimeAge above (neutering the template gate
// drops the age span).
func TestPlanPeekHeader_WorkingAgeAgreesWithIndex(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")

	// The index card renders the SAME crew via the /plans board (control): with
	// seedCrew's CreatedAt = crewFixtureNow − 1h, both surfaces derive "1h".
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("index render: expected 200, got %d", w.Code)
	}
	rows := w.Body.String()
	if !strings.Contains(rows, `<span class="plan-card-crew-working-age">1h</span>`) {
		t.Fatalf("index card should carry the working runtime age '1h' (control); rows-has-age=%v", strings.Contains(rows, "plan-card-crew-working-age"))
	}

	header := peekHeader(t, peekBody(t, f))
	if !strings.Contains(header, `<span class="plan-peek-crew-working-age">1h</span>`) {
		t.Errorf("peek header runtime age must AGREE with the index card's (both derive 1h); header=%s", header)
	}
}
