package main

// plans_queued_age_test.go — iter-32 (ka-0000005352 / gd-y9zzLeukQl): the queued
// crew facet's WAIT-AGE. iter-31 shipped the "· N queued" facet (a crew dispatched
// onto a step but not yet working — an open idle/queued agent_state whose pod is
// capacity-deferred in the pool), but it was the LAST card signal that carried a
// count with no age — its siblings the decision chip ("· 32d"), escalation badge
// ("· 40d"), stall badge ("· 3d"), and stuck facet ("· 6h") all surface HOW LONG.
// A crew queued 2 minutes ago (the pool will slot it) read identically to one
// queued 3 hours ago (the pool is genuinely starved). This folds the oldest queued
// crew's wait onto the facet + cockpit chip, loud past planCrewQueuedStaleThreshold.
//
// The wait-age basis is honest: a queued crew's updated_at freezes at queue-entry
// (the reconciler re-marks "queued" every pass, but UpdateBeadFields short-circuits
// an unchanged value → no PATCH → no updated_at bump), exactly like the open-stuck
// crew's updated_at iter-13 already ages on.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- unit: the index rollup carries the oldest queued crew's wait-age ----------

// TestPlanCrewBulk_QueuedOldestAge proves the rollup reports the wait-age of the
// OLDEST OPEN queued crew (the earliest last-activity stamp), NOT the newest. With
// the clock at 12:00, a queued crew last active at 11:40 has waited 20m; one at
// 11:50 has waited 10m. The oldest wait (20m) is reported and is under the 1h
// stale bar so QueuedOldestStale is false.
func TestPlanCrewBulk_QueuedOldestAge(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	newer := seedCrew(f, "gd-qN", "gardenio-newer-queued", "gd-step1", "queued", "open")
	newer.UpdatedAt = time.Date(2026, 7, 5, 11, 50, 0, 0, time.UTC) // 10m ago
	older := seedCrew(f, "gd-qO", "gardenio-older-queued", "gd-step1", "queued", "open")
	older.UpdatedAt = time.Date(2026, 7, 5, 11, 40, 0, 0, time.UTC) // 20m ago — the oldest wait
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Idle != 2 {
		t.Fatalf("Idle = %d, want 2 (two queued crew)", roll.Idle)
	}
	if got := roll.QueuedOldestAge(); got != "20m" {
		t.Errorf("QueuedOldestAge = %q, want %q (the OLDER queued crew's wait, not the newer)", got, "20m")
	}
	if roll.QueuedOldestStale() {
		t.Errorf("QueuedOldestStale = true, want false (20m is under the 1h stale bar)")
	}
	// No-new-fan-out budget: the wait-age is a fold over beads already in hand, so
	// the rollup still pays exactly ONE reverse assigned-edge batch + ONE agent
	// hydration batch — no per-crew round-trip for the age.
	if f.batchGetDepsCalls != 1 || f.batchGetBeadsCalls != 1 {
		t.Errorf("fan-out = %d deps + %d beads, want 1 + 1 (the queued age adds no daemon call)", f.batchGetDepsCalls, f.batchGetBeadsCalls)
	}
}

// TestPlanCrewBulk_QueuedOldestStale proves a queued crew waiting past the
// staleness threshold (1h) flips QueuedOldestStale — the mark that tints the facet
// loud so a pool-starved plan pops out of the board.
func TestPlanCrewBulk_QueuedOldestStale(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	// Queued since 09:30 — 2.5h before the 12:00 clock, well past the 1h bar.
	old := seedCrew(f, "gd-qX", "gardenio-long-queued", "gd-step1", "queued", "open")
	old.UpdatedAt = time.Date(2026, 7, 5, 9, 30, 0, 0, time.UTC)
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if got := roll.QueuedOldestAge(); got == "" {
		t.Fatalf("QueuedOldestAge is empty, want a datable age")
	}
	if !roll.QueuedOldestStale() {
		t.Errorf("QueuedOldestStale = false, want true (queued 2.5h, past the 1h bar)")
	}
}

// TestPlanCrewBulk_UndatableQueuedNoAge proves a queued crew with no timestamp
// contributes no wait-age — the facet then reads count-only (its bare "· N
// queued"), exactly as iter-31, never a bogus "just now".
func TestPlanCrewBulk_UndatableQueuedNoAge(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	q := seedCrew(f, "gd-qU", "gardenio-undatable-queued", "gd-step1", "queued", "open")
	q.UpdatedAt = time.Time{} // zero stamp
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]
	if roll.Idle != 1 {
		t.Fatalf("Idle = %d, want 1", roll.Idle)
	}
	if got := roll.QueuedOldestAge(); got != "" {
		t.Errorf("QueuedOldestAge = %q, want empty (an undatable queued crew contributes no age)", got)
	}
	if roll.QueuedOldestStale() {
		t.Errorf("QueuedOldestStale = true, want false (no datable queued crew)")
	}
}

// --- unit: the cockpit roster carries the same age (index↔cockpit parity) ------

func TestPlanCrew_QueuedOldestAgeParity(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	newer := seedCrew(f, "gd-qN", "gardenio-newer-queued", "gd-step1", "queued", "open")
	newer.UpdatedAt = time.Date(2026, 7, 5, 11, 50, 0, 0, time.UTC)
	older := seedCrew(f, "gd-qO", "gardenio-older-queued", "gd-step1", "queued", "open")
	older.UpdatedAt = time.Date(2026, 7, 5, 11, 40, 0, 0, time.UTC)
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	cockpit := s.planCrew(context.Background(), "", steps, nil)
	index := s.planCrewBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}}, nil)["gd-plan1"]

	if cockpit.QueuedOldestAge() != index.QueuedOldestAge() {
		t.Errorf("parity: cockpit age %q != index age %q", cockpit.QueuedOldestAge(), index.QueuedOldestAge())
	}
	if cockpit.QueuedOldestStale() != index.QueuedOldestStale() {
		t.Errorf("parity: cockpit stale %v != index stale %v", cockpit.QueuedOldestStale(), index.QueuedOldestStale())
	}
	if cockpit.QueuedOldestAge() != "20m" {
		t.Errorf("cockpit QueuedOldestAge = %q, want 20m", cockpit.QueuedOldestAge())
	}
}

// --- unit: helper generalization is threshold-parameterized -------------------

// TestOldestOpenCrewAge_RespectsThreshold proves the shared helper flips stale
// against the threshold PASSED to it, so the stuck facet (1h) and the queued facet
// (1h today, but independent) each apply their own bar. A crew waiting 40m is
// stale under a 30m bar but not under a 1h bar.
func TestOldestOpenCrewAge_RespectsThreshold(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	waited40m := []*beadsapi.BeadDetail{{ID: "gd-q", Type: "agent", UpdatedAt: time.Date(2026, 7, 5, 11, 20, 0, 0, time.UTC)}}
	if _, stale := oldestOpenCrewAge(waited40m, now, time.Hour); stale {
		t.Errorf("40m wait under a 1h bar: stale=true, want false")
	}
	if _, stale := oldestOpenCrewAge(waited40m, now, 30*time.Minute); !stale {
		t.Errorf("40m wait under a 30m bar: stale=false, want true")
	}
}

// --- render regression: the index card renders the queued age ----------------

func TestPlansIndex_QueuedFacetRendersAge(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	q := seedCrew(f, "gd-queued", "gardenio-prefix-cache-qqq", "gd-step1", "queued", "open")
	q.UpdatedAt = time.Date(2026, 7, 5, 11, 40, 0, 0, time.UTC) // 20m before the test clock
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if !strings.Contains(card, "plan-card-crew-queued-age") {
		t.Errorf("a datable queued crew must render the wait-age span; card:\n%s", card)
	}
	if !strings.Contains(card, "1 queued") || !strings.Contains(card, "20m") {
		t.Errorf("the queued facet must read '· 1 queued 20m'; card:\n%s", card)
	}
	// Not stale (20m < 1h): the loud stale class must be absent.
	if strings.Contains(card, "plan-card-crew-queued-stale") {
		t.Errorf("a 20m queued wait is under the stale bar — the loud stale class must be absent")
	}
}

// A queued crew wedged past the stale bar tints the facet loud.
func TestPlansIndex_QueuedFacetStaleTintsLoud(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	q := seedCrew(f, "gd-queued", "gardenio-prefix-cache-qqq", "gd-step1", "queued", "open")
	q.UpdatedAt = time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC) // 3h before the clock
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if !strings.Contains(card, "plan-card-crew-queued-stale") {
		t.Errorf("a 3h queued wait is past the stale bar — the facet must tint loud; card:\n%s", card)
	}
	if !strings.Contains(card, "3h") {
		t.Errorf("the queued facet must read the 3h wait; card:\n%s", card)
	}
}

// A queued crew with no timestamp renders the count only — no age span, exactly
// as iter-31 (must-not-regress).
func TestPlansIndex_UndatableQueuedNoAgeSpan(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	q := seedCrew(f, "gd-queued", "gardenio-prefix-cache-qqq", "gd-step1", "queued", "open")
	q.UpdatedAt = time.Time{}
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if !strings.Contains(card, "1 queued") {
		t.Errorf("the queued facet must still render its count; card:\n%s", card)
	}
	if strings.Contains(card, "plan-card-crew-queued-age") {
		t.Errorf("an undatable queued crew must render no age span (must-not-regress iter-31); card:\n%s", card)
	}
}

// A TERMINAL plan folds idle→done (iter-30/31 reconcileTerminal), so a leftover
// queued crew renders NEITHER the facet NOR its age (must-not-regress).
func TestPlansIndex_TerminalPlanNoQueuedAge(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "closed"
	q := seedCrew(f, "gd-queued", "gardenio-prefix-cache-qqq", "gd-step1", "queued", "open")
	q.UpdatedAt = time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC)
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if strings.Contains(card, "plan-card-crew-idle") || strings.Contains(card, "plan-card-crew-queued-age") {
		t.Errorf("a closed plan folds idle→done — no queued facet or age may render; card:\n%s", card)
	}
}

// --- render regression: the cockpit chip renders the age + stale tint ---------

func TestPlanDetail_QueuedChipRendersAge(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	q := seedCrew(f, "gd-queued", "gardenio-prefix-cache-qqq", "gd-step1", "queued", "open")
	q.UpdatedAt = time.Date(2026, 7, 5, 11, 40, 0, 0, time.UTC)
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "1 queued · 20m") {
		t.Errorf("the cockpit queued chip must read '1 queued · 20m'")
	}
	// Not stale at 20m — the chip renders the calm (non-stale) element. Assert on the
	// rendered ELEMENT markup, not the bare class: .plan-crew-count-idle-stale also
	// appears in the page's <style> block, so a bare-substring check false-matches
	// (the same gotcha the iter-31 TerminalPlanNoQueuedChip test documents).
	if !strings.Contains(body, `class="chip plan-crew-count plan-crew-count-idle"`) {
		t.Errorf("a 20m queued wait is under the stale bar — the chip must render the calm element (no stale modifier)")
	}
}

func TestPlanDetail_QueuedChipStaleTintsLoud(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	q := seedCrew(f, "gd-queued", "gardenio-prefix-cache-qqq", "gd-step1", "queued", "open")
	q.UpdatedAt = time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC) // 3h
	mux := mkServerWithNow(t, f, func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `class="chip plan-crew-count plan-crew-count-idle plan-crew-count-idle-stale"`) {
		t.Errorf("a 3h queued wait is past the stale bar — the cockpit chip must render the loud stale element")
	}
}
