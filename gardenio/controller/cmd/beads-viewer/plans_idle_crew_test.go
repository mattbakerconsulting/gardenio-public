package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plans_idle_crew_test.go — iter-31 (ka-0000005352 / ka-0000000177): an OPEN crew
// DISPATCHED onto a step but not yet working — an idle/queued agent_state whose
// pod is capacity-deferred in the pool (ka-0000003525) — must read as its own
// "queued" disposition, NOT fold silently into "done". Before this, planCrewBulk
// (index) and planCrew (cockpit) bucketed crew as Working/Done/Failed and the
// default arm swallowed an idle crew into Done, so:
//   - the index chip on a plan whose crew are all queued for pod slots read
//     "N crew" with no hint nothing had started (and its hidden "done" tally was
//     wrong); and
//   - the cockpit header counted that crew "done" while each member row rendered
//     its raw "queued"/"idle" state chip — a self-contradiction on one page.
// This surfaces the idle disposition the mission's working/blocked/idle asks name.

// seedIdlePlanCrew returns a fake whose sole plan is ACTIVE (in_progress) with a
// queued crew (open bead, idle/queued agent_state — dispatched, not yet working),
// a genuinely working crew, and a cleanly-finished (closed) crew — so a test can
// assert the three dispositions tally distinctly.
func seedIdlePlanCrew(planStatus string) *fakeDaemon {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = planStatus
	f.beads["gd-step1"].Status = "open"
	// Queued: an OPEN bead in the idle "queued" bucket — dispatched, pod deferred.
	seedCrew(f, "gd-queued", "gardenio-prefix-cache-qqq", "gd-step1", "queued", "in_progress")
	// Working: an OPEN bead in an alive state — the healthy live crew.
	seedCrew(f, "gd-working", "gardenio-prefix-cache-www", "gd-step1", "working", "in_progress")
	// Done: a cleanly-finished CLOSED crew — the real "done".
	done := seedCrew(f, "gd-done", "gardenio-prefix-cache-ddd", "gd-step1", "done", "closed")
	done.Fields["wrapup"] = `{"tldr":"finished the wiring; MR merged."}`
	return f
}

// --- unit: the cockpit tally (planCrew) counts idle distinctly ---------------

func TestPlanCrew_IdleCrewNotCountedDone(t *testing.T) {
	f := seedIdlePlanCrew("in_progress")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)

	if info.Total != 3 {
		t.Fatalf("Total = %d, want 3", info.Total)
	}
	// The queued crew is Idle, the alive one Working, the closed one Done — all
	// distinct. Before iter-31 the queued crew inflated Done to 2.
	if info.Working != 1 || info.Idle != 1 || info.Done != 1 || info.Failed != 0 {
		t.Errorf("tally working/idle/done/failed = %d/%d/%d/%d, want 1/1/1/0",
			info.Working, info.Idle, info.Done, info.Failed)
	}
}

// An "idle" (empty agent_state) OPEN crew also reads idle, not done — the
// agentStateBucket default folds empty state to idle, so a dispatched-but-blank
// crew is a queued crew, not a finished one.
func TestPlanCrew_EmptyStateOpenCrewIsIdle(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-blank", "gardenio-prefix-cache-bbb", "gd-step1", "", "open")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)
	if info.Idle != 1 || info.Done != 0 {
		t.Errorf("empty-state open crew tally idle/done = %d/%d, want 1/0", info.Idle, info.Done)
	}
}

// A CLOSED crew with an idle/blank state is NOT queued — it finished (its pod is
// gone). It must still read Done, not Idle, so a closed clean crew never leaks
// into the "queued" tally.
func TestPlanCrew_ClosedIdleCrewStillDone(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-wjp4nuf4id", "gardenio-prefix-cache-cci", "gd-step1", "idle", "closed")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	info := s.planCrew(context.Background(), "", steps, nil)
	if info.Idle != 0 || info.Done != 1 {
		t.Errorf("closed idle-state crew tally idle/done = %d/%d, want 0/1 (a closed crew finished, it is not queued)", info.Idle, info.Done)
	}
}

// --- unit: the index rollup (planCrewBulk) counts idle distinctly ------------

func TestPlanCrewRollup_IdleCrewNotCountedDone(t *testing.T) {
	f := seedIdlePlanCrew("in_progress")
	s := crewTestServer(f)

	roll := s.planCrewBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-step1"},
	}, nil)["gd-plan1"]

	if roll.Total != 3 {
		t.Fatalf("Total = %d, want 3", roll.Total)
	}
	if roll.Working != 1 || roll.Idle != 1 || roll.Done != 1 || roll.Failed != 0 {
		t.Errorf("rollup working/idle/done/failed = %d/%d/%d/%d, want 1/1/1/0",
			roll.Working, roll.Idle, roll.Done, roll.Failed)
	}
}

// --- index ↔ cockpit parity ---------------------------------------------------

// TestIdleCrew_IndexCockpitParity proves the index rollup and the cockpit roster
// classify the same crew set identically for the idle disposition — the same
// by-construction parity the working/done/failed tallies already hold.
func TestIdleCrew_IndexCockpitParity(t *testing.T) {
	f := seedIdlePlanCrew("in_progress")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	cockpit := s.planCrew(context.Background(), "", steps, nil)
	index := s.planCrewBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-step1"},
	}, nil)["gd-plan1"]

	if cockpit.Idle != index.Idle {
		t.Errorf("parity: cockpit.Idle=%d != index.Idle=%d", cockpit.Idle, index.Idle)
	}
	if cockpit.Working != index.Working || cockpit.Done != index.Done || cockpit.Total != index.Total {
		t.Errorf("parity drift working/done/total: cockpit %d/%d/%d vs index %d/%d/%d",
			cockpit.Working, cockpit.Done, cockpit.Total, index.Working, index.Done, index.Total)
	}
}

// --- terminal reconcile: idle folds into done on a closed plan ---------------

func TestPlanCrewRollup_ReconcileTerminal_FoldsIdle(t *testing.T) {
	base := func() planCrewRollup {
		return planCrewRollup{Total: 4, Working: 1, Idle: 1, Done: 1, Failed: 1, Faces: []planCrewFace{{Name: "z"}}}
	}
	cases := []struct {
		status                          string
		wantWorking, wantIdle, wantDone int
	}{
		// Terminal → both the live-working AND the queued crew fold into Done.
		{"closed", 0, 0, 3},
		{"deferred", 0, 0, 3},
		// Non-terminal → untouched.
		{"in_progress", 1, 1, 1},
		{"open", 1, 1, 1},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			got := base().reconcileTerminal(c.status)
			if got.Working != c.wantWorking || got.Idle != c.wantIdle || got.Done != c.wantDone {
				t.Errorf("status=%s: working/idle/done = %d/%d/%d, want %d/%d/%d",
					c.status, got.Working, got.Idle, got.Done, c.wantWorking, c.wantIdle, c.wantDone)
			}
			// The record is preserved: Total, Failed untouched.
			if got.Total != 4 || got.Failed != 1 {
				t.Errorf("status=%s: record drifted Total/Failed = %d/%d, want 4/1", c.status, got.Total, got.Failed)
			}
		})
	}
}

// A terminal plan with ONLY an idle crew (no live-working) still folds — the
// guard must trip on Idle alone, not just Working, or a closed plan whose crew
// are all queued would keep rendering "queued".
func TestPlanCrewRollup_ReconcileTerminal_IdleOnlyFolds(t *testing.T) {
	r := planCrewRollup{Total: 2, Working: 0, Idle: 1, Done: 1}
	got := r.reconcileTerminal("closed")
	if got.Idle != 0 || got.Done != 2 || got.Working != 0 {
		t.Errorf("idle-only terminal fold: working/idle/done = %d/%d/%d, want 0/0/2", got.Working, got.Idle, got.Done)
	}
}

func TestPlanCrewInfo_ReconcileTerminal_FoldsIdle(t *testing.T) {
	base := planCrewInfo{
		Total:   3,
		Working: 1,
		Idle:    1,
		Done:    1,
		Members: []planCrewMember{
			{ID: "gd-w", Live: true, Bucket: "alive", AgentState: "working"},
			{ID: "gd-q", Live: false, Bucket: "idle", AgentState: "queued"},
			{ID: "gd-d", Live: false, Bucket: "done", AgentState: "done"},
		},
	}
	got := base.reconcileTerminal("closed")
	if got.Working != 0 || got.Idle != 0 || got.Done != 3 {
		t.Errorf("cockpit terminal fold working/idle/done = %d/%d/%d, want 0/0/3", got.Working, got.Idle, got.Done)
	}
	// The queued member re-buckets to done so its tint matches the header tally...
	if got.Members[1].Bucket != "done" {
		t.Errorf("folded queued member Bucket = %q, want done", got.Members[1].Bucket)
	}
	// ...but its raw AgentState string is preserved as a factual record.
	if got.Members[1].AgentState != "queued" {
		t.Errorf("folded member raw AgentState = %q, want it preserved as queued", got.Members[1].AgentState)
	}
}

// --- render regression: the index card renders a "queued" facet --------------

func TestPlansIndex_IdleCrewRendersQueuedFacet(t *testing.T) {
	f := seedIdlePlanCrew("in_progress")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if !strings.Contains(card, "plan-card-crew-idle") {
		t.Errorf("an active plan with a queued crew must render the idle facet class; card:\n%s", card)
	}
	if !strings.Contains(card, "1 queued") {
		t.Errorf("the idle facet must read '1 queued'; card:\n%s", card)
	}
	// The live signal for the WORKING crew still renders (anti-over-reach).
	if !strings.Contains(card, "plan-card-crew-working") {
		t.Errorf("the working crew's live facet must still render alongside the queued facet")
	}
}

// A plan with NO idle crew renders no queued facet — add signal, not ink.
func TestPlansIndex_NoIdleCrewNoQueuedFacet(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	seedCrew(f, "gd-working", "gardenio-prefix-cache-www", "gd-step1", "working", "in_progress")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if strings.Contains(card, "plan-card-crew-idle") {
		t.Errorf("a plan with no queued crew must not render the idle facet")
	}
}

// A TERMINAL plan folds idle→done, so its card renders NO "queued" facet even
// with a leftover queued crew bead (the crew twin of iter-30's zombie fold).
func TestPlansIndex_TerminalPlanNoQueuedFacet(t *testing.T) {
	f := seedIdlePlanCrew("closed")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if strings.Contains(card, "plan-card-crew-idle") {
		t.Errorf("a closed plan must fold idle→done and render no queued facet; card:\n%s", card)
	}
}

// --- render regression: the cockpit roster renders a "queued" tally chip -----

func TestPlanDetail_IdleCrewRendersQueuedChip(t *testing.T) {
	f := seedIdlePlanCrew("in_progress")
	// Pin the clock so the seeded queued crew (last active 2026-07-05 11:55) reads a
	// fresh 5m wait, under the iter-32 stale bar — this test asserts the queued chip
	// RENDERS, not its staleness, so it must exercise the calm (non-stale) element.
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="chip plan-crew-count plan-crew-count-idle"`) {
		t.Errorf("an active plan's cockpit roster must render the queued tally chip")
	}
	if !strings.Contains(body, "1 queued") {
		t.Errorf("the cockpit queued chip must read '1 queued'")
	}
}

// A closed plan's cockpit roster folds idle→done — no queued chip.
func TestPlanDetail_TerminalPlanNoQueuedChip(t *testing.T) {
	f := seedIdlePlanCrew("closed")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Assert on the rendered ELEMENT markup, not the bare class — the class name
	// also appears in the page's shared <style> block, so a bare-substring check
	// would false-match the CSS rule.
	if strings.Contains(w.Body.String(), `class="chip plan-crew-count plan-crew-count-idle"`) {
		t.Errorf("a closed plan's cockpit roster must fold idle→done and render no queued chip")
	}
}
