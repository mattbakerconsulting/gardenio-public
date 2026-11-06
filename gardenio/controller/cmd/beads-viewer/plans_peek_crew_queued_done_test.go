package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plans_peek_crew_queued_done_test.go — iter-79 (ka-0000005352): the /plans peek
// drawer HEADER crew chip now folds the QUEUED ("· N queued") and DONE ("· N
// done") crew facets.
//
// THE GAP (measure-first): the peek header crew chip (iter-65 + iter-71 + iter-76
// + spend) carried total / working / (K starting) / runtime-age / stuck / spend,
// but DROPPED the queued (dispatched-but-pod-starved) and done (finished)
// dispositions on-face. Both the index card face (plan_card.html, "· N queued"
// iter-31/32 + "· N done" iter-64) AND the cockpit roster header (plan_crew.html)
// render them — so an operator opening a peek to triage saw who is working / stuck
// / how much was spent, but not how many crew are waiting on a pod slot or already
// finished. Queued in particular is a pool-starvation signal an operator acts on.
//
// The fold is template-only + CSS: .Crew (planCrewInfo) already carries .Idle /
// .QueuedOldestAge / .QueuedOldestStale (queued), and .HasDoneFacet() — the
// cockpit-side twin added mirroring planCrewRollup.HasDoneFacet (0 < Done < Total)
// — so the peek chip and the index/cockpit facets agree by construction (advice
// ka-0000005924), zero extra daemon call. Order matches the index face exactly:
// working → queued → done → stuck → spend.

// seedPeekCrewQueuedAndDone seeds a MIXED roster: one working (live), one queued
// (open idle, not yet slotted), and one done (closed) crew — so the peek's
// planCrewInfo carries both a derivable queued count AND a 0<Done<Total done
// facet. Total = 3, Working = 1, Idle = 1, Done = 1.
func seedPeekCrewQueuedAndDone(t *testing.T) *fakeDaemon {
	t.Helper()
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewWork", "gardenio-wire-route-work", "gd-step1", "working", "in_progress")
	seedCrew(f, "gd-crewQueue", "gardenio-wire-route-queue", "gd-step1", "queued", "open")
	seedCrew(f, "gd-crewDone", "gardenio-wire-route-done", "gd-step1", "idle", "closed")
	return f
}

// TestPlanPeekHeader_CrewQueuedAndDoneFacets: a MIXED roster renders BOTH the
// "· 1 queued" and "· 1 done" facets in the header chip, in the index face order
// (working → queued → done → stuck). FAILS pre-fix (the chip drops both facets).
func TestPlanPeekHeader_CrewQueuedAndDoneFacets(t *testing.T) {
	f := seedPeekCrewQueuedAndDone(t)

	header := peekHeader(t, peekBody(t, f))

	if !strings.Contains(header, `<span class="plan-peek-crew-queued">· 1 queued`) {
		t.Errorf("peek header missing crew-queued facet — the peek↔index↔cockpit queued parity gap is not closed; header=%s", header)
	}
	if !strings.Contains(header, `<span class="plan-peek-crew-done">· 1 done</span>`) {
		t.Errorf("peek header missing crew-done facet — the peek↔index↔cockpit done parity gap is not closed; header=%s", header)
	}

	// Facet order must match the index face: working → queued → done → stuck.
	iWork := strings.Index(header, "plan-peek-crew-working")
	iQueued := strings.Index(header, "plan-peek-crew-queued")
	iDone := strings.Index(header, "plan-peek-crew-done")
	if iWork < 0 || iQueued < 0 || iDone < 0 || !(iWork < iQueued && iQueued < iDone) {
		t.Errorf("facet order must be working < queued < done; workIdx=%d queuedIdx=%d doneIdx=%d header=%s", iWork, iQueued, iDone, header)
	}
}

// TestPlanPeekHeader_CrewQueuedWaitAge: the queued facet folds the OLDEST queued
// crew's wait-age (matching the index's "· N queued <age>"). seedCrew stamps
// UpdatedAt at now-5m, so the queued crew reads a "5m" wait-age, calm (under the
// stale bar).
func TestPlanPeekHeader_CrewQueuedWaitAge(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewQ", "gardenio-wire-route-q", "gd-step1", "queued", "open")

	header := peekHeader(t, peekBody(t, f))
	if !strings.Contains(header, `<span class="plan-peek-crew-queued-age">5m</span>`) {
		t.Errorf("peek header queued facet missing the oldest queued crew's wait-age '5m'; header=%s", header)
	}
	// A 5m wait is under the stale threshold, so no pool-starvation loud-tint.
	if strings.Contains(header, "plan-peek-crew-queued-stale") {
		t.Errorf("peek header rendered a queued-stale loud-tint for a fresh 5m queue — the stale gate is not holding; header=%s", header)
	}
}

// TestPlanPeekHeader_CrewDoneSuppressedWhenAllDone is a redundant-ink floor: when
// EVERY crew is done (Done == Total), HasDoneFacet() self-suppresses so the chip
// reads its bare "N crew" total — "N crew · N done" would be pure redundant ink,
// and the ABSENCE of any working/queued/stuck facet already conveys "all done".
func TestPlanPeekHeader_CrewDoneSuppressedWhenAllDone(t *testing.T) {
	f := seedPlansFake()
	// All crew closed/done, no live/queued.
	seedCrew(f, "gd-crewD1", "gardenio-done-1", "gd-step1", "idle", "closed")
	seedCrew(f, "gd-crewD2", "gardenio-done-2", "gd-step1", "idle", "closed")

	header := peekHeader(t, peekBody(t, f))
	if strings.Contains(header, "plan-peek-crew-done") {
		t.Errorf("peek header rendered a done facet when ALL crew are done (Done==Total) — the redundant-ink gate (0<Done<Total) is not holding; header=%s", header)
	}
	if !strings.Contains(header, `<span class="plan-peek-crew-total">2 crew</span>`) {
		t.Errorf("peek header should still carry the bare '2 crew' total; header=%s", header)
	}
}

// TestPlanPeekHeader_CrewQueuedDoneAgreeWithIndex is the two-derivations-must-agree
// keystone (advice ka-0000005924): the peek header's queued+done facets read the
// SAME planCrewInfo the roster derives, and the index card face reads the SAME
// planCrewRollup — both project the same crew beads over the same classifier, so
// the "1 queued" / "1 done" the peek shows must be what the index card renders.
func TestPlanPeekHeader_CrewQueuedDoneAgreeWithIndex(t *testing.T) {
	f := seedPeekCrewQueuedAndDone(t)

	// The index card renders the SAME crew via the /plans board (control).
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("index render: expected 200, got %d", w.Code)
	}
	rows := w.Body.String()
	if !strings.Contains(rows, `class="plan-card-crew-idle`) || !strings.Contains(rows, "· 1 queued") {
		t.Fatalf("index card should carry the '1 queued' facet (control); has-class=%v has-value=%v", strings.Contains(rows, `class="plan-card-crew-idle`), strings.Contains(rows, "· 1 queued"))
	}
	if !strings.Contains(rows, `class="plan-card-crew-done"`) || !strings.Contains(rows, "· 1 done") {
		t.Fatalf("index card should carry the '1 done' facet (control); has-class=%v has-value=%v", strings.Contains(rows, `class="plan-card-crew-done"`), strings.Contains(rows, "· 1 done"))
	}

	header := peekHeader(t, peekBody(t, f))
	if !strings.Contains(header, `<span class="plan-peek-crew-queued">· 1 queued`) {
		t.Errorf("peek header queued facet must AGREE with the index card's '1 queued'; header=%s", header)
	}
	if !strings.Contains(header, `<span class="plan-peek-crew-done">· 1 done</span>`) {
		t.Errorf("peek header done facet must AGREE with the index card's '1 done'; header=%s", header)
	}
}

// TestPlanPeekHeader_CrewNoQueuedDoneWhenAbsent is the fail-without floor: a roster
// with only a working crew (no queued, no done) renders neither facet, so a plain
// grinding plan reads exactly as before iter-79.
func TestPlanPeekHeader_CrewNoQueuedDoneWhenAbsent(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")

	header := peekHeader(t, peekBody(t, f))
	if strings.Contains(header, "plan-peek-crew-queued") {
		t.Errorf("peek header rendered a queued facet for a roster with no queued crew; header=%s", header)
	}
	if strings.Contains(header, "plan-peek-crew-done") {
		t.Errorf("peek header rendered a done facet for a roster with no finished crew; header=%s", header)
	}
	// The working facet floor still holds.
	if !strings.Contains(header, `<span class="plan-peek-crew-working">· 1 working`) {
		t.Errorf("peek header lost its working facet; header=%s", header)
	}
}
