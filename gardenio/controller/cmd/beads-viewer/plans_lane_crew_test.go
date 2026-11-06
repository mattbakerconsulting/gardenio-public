package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// plans_lane_crew_test.go — the aggregate live-crew tally on the /plans board
// LANE and ?group= SECTION headers (ka-0000005352).
//
// The mission's 86 card-facet iterations folded crew signals onto every plan
// CARD surface, but the board's lane section headers and the ?group= section
// headers still rendered only "Label (N)". planLaneCrew sums working +
// open-stuck across a section's rows so the header carries the column-level
// "N working · ⚠ M stuck" scan signal, reading the SAME per-row .Crew rollup
// the card facets render (zero extra daemon call; header ⇄ sum-of-cards agree
// by construction, advice ka-0000005924).

// --- unit: planLaneCrew arithmetic + gates ----------------------------------

func TestPlanLaneCrew_Aggregate(t *testing.T) {
	rows := []planRowView{
		{Crew: planCrewRollup{Total: 2, Working: 2}},
		{Crew: planCrewRollup{Total: 3, Working: 1, stuckOpen: 1, Failed: 1}},
		{Crew: planCrewRollup{Total: 1, Done: 1}}, // finished → contributes nothing
		{Crew: planCrewRollup{Total: 2, Idle: 2}}, // queued → contributes nothing
	}
	agg := planLaneCrew(rows)
	if agg.Working != 3 {
		t.Errorf("Working = %d, want 3 (2+1 across the two live plans)", agg.Working)
	}
	if agg.OpenStuck != 1 {
		t.Errorf("OpenStuck = %d, want 1 (only the reachable open-stuck crew)", agg.OpenStuck)
	}
	if !agg.Any() {
		t.Errorf("Any() = false, want true")
	}
	if !agg.HasWorking() || !agg.HasOpenStuck() {
		t.Errorf("HasWorking=%v HasOpenStuck=%v, want both true", agg.HasWorking(), agg.HasOpenStuck())
	}
}

// A section whose crew are all queued or done — no working, no open-stuck —
// surfaces NO aggregate facet (Any false). The header adds no ink where the
// value does not discriminate, the density discipline the card facets use.
func TestPlanLaneCrew_NoLiveNoStuckSuppresses(t *testing.T) {
	rows := []planRowView{
		{Crew: planCrewRollup{Total: 2, Idle: 2}},   // all queued
		{Crew: planCrewRollup{Total: 1, Done: 1}},   // finished
		{Crew: planCrewRollup{Total: 3, Failed: 2}}, // failed-but-CLOSED (stuckOpen=0)
		{Crew: planCrewRollup{}},                    // no crew
	}
	agg := planLaneCrew(rows)
	if agg.Working != 0 || agg.OpenStuck != 0 {
		t.Errorf("expected no working/open-stuck, got %d/%d", agg.Working, agg.OpenStuck)
	}
	if agg.Any() {
		t.Errorf("Any() = true, want false — a section with no live/reachable-stuck crew adds no header facet")
	}
}

// An empty section (no rows at all) aggregates to nothing.
func TestPlanLaneCrew_EmptySection(t *testing.T) {
	if agg := planLaneCrew(nil); agg.Any() {
		t.Errorf("planLaneCrew(nil).Any() = true, want false")
	}
}

// --- render: the board LANE header carries the aggregate --------------------

// seedLaneCrewFake returns a fake whose single ACTIVE plan carries one working
// crew AND one OPEN-stuck (stale-heartbeat) crew, so its lane header must render
// both aggregate facets. The plan lands in the In progress lane: a working crew
// suppresses the ⚠ stall (reconcileStallWithCrew — the gardener plainly did its
// job), so the plan reads as genuinely in-flight, exactly the column where "this
// column has N working + M stuck crew" is the useful scan signal.
func seedLaneCrewFake() *fakeDaemon {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	f.beads["gd-step1"].Status = "open"
	seedCrew(f, "gd-working", "gardenio-lane-crew-www", "gd-step1", "working", "in_progress")
	// A stale-heartbeat alive crew is demoted to reachable OPEN-stuck (iter-42).
	seedStaleCrew(f, "gd-stuck", "gardenio-lane-crew-sss", "gd-step1", "working", 3*time.Hour)
	return f
}

func TestPlansBoard_LaneHeaderRendersCrewAggregate(t *testing.T) {
	f := seedLaneCrewFake()
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	// Default board view (no ?view=), all statuses so nothing is scoped out.
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	head := isolateLaneHeader(t, w.Body.String(), "in_progress")
	if !strings.Contains(head, "plans-lane-crew-working") {
		t.Errorf("in_progress lane header must carry the working aggregate facet; head:\n%s", head)
	}
	if !strings.Contains(head, "1 working") {
		t.Errorf("working aggregate must read '1 working'; head:\n%s", head)
	}
	if !strings.Contains(head, "plans-lane-crew-stuck") {
		t.Errorf("in_progress lane header must carry the open-stuck aggregate facet; head:\n%s", head)
	}
	if !strings.Contains(head, "plans-lane-crew-dot") {
		t.Errorf("working aggregate must carry the pulsing live-dot; head:\n%s", head)
	}
}

// A lane with NO working and NO open-stuck crew renders no aggregate facet — the
// Any() suppression proven end-to-end through the board render. The Closed lane
// (a finished plan) is the natural such column.
func TestPlansBoard_QuietLaneNoCrewAggregate(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "closed"
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// The open lane is a core lane rendered even when empty — it must carry no
	// aggregate facet with no live crew anywhere.
	head := isolateLaneHeader(t, w.Body.String(), "open")
	if strings.Contains(head, "plans-lane-crew") {
		t.Errorf("a lane with no working/stuck crew must render no aggregate facet; head:\n%s", head)
	}
}

// --- render: the ?group= SECTION header folds the SAME helper ---------------

// The group header uses the identical planLaneCrew helper, so a ?group= board's
// section header carries the same aggregate — lane ⇄ group parity by
// construction.
func TestPlansGroup_HeaderRendersCrewAggregate(t *testing.T) {
	f := seedLaneCrewFake()
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	// ?view=grid + ?group=status renders the sectioned grid with group headers.
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all&view=grid&group=status", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	start := strings.Index(body, "plan-group-head")
	if start < 0 {
		t.Fatalf("no group header rendered; body:\n%s", body[:min(len(body), 2000)])
	}
	// Isolate the first group header element.
	seg := body[start:]
	if end := strings.Index(seg, "</h2>"); end >= 0 {
		seg = seg[:end]
	}
	if !strings.Contains(seg, "plans-lane-crew-working") {
		t.Errorf("group header must carry the working aggregate facet via planLaneCrew; head:\n%s", seg)
	}
	if !strings.Contains(seg, "plans-lane-crew-stuck") {
		t.Errorf("group header must carry the open-stuck aggregate facet; head:\n%s", seg)
	}
}

// isolateLaneHeader extracts the <header class="plans-lane-head">…</header> of
// the lane whose key matches, so a test can assert on the header alone without
// matching the cards below it.
func isolateLaneHeader(t *testing.T, body, laneKey string) string {
	t.Helper()
	anchor := `class="plans-lane plans-lane-` + laneKey + `"`
	start := strings.Index(body, anchor)
	if start < 0 {
		t.Fatalf("lane %s not found in body", laneKey)
	}
	seg := body[start:]
	hStart := strings.Index(seg, `class="plans-lane-head"`)
	if hStart < 0 {
		t.Fatalf("lane %s header not found", laneKey)
	}
	seg = seg[hStart:]
	end := strings.Index(seg, "</header>")
	if end < 0 {
		t.Fatalf("lane %s header close not found", laneKey)
	}
	return seg[:end]
}
