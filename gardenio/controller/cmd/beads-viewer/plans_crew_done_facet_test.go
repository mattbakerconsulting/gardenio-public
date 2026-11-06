package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plans_crew_done_facet_test.go — iter-64 (ka-0000005352): "done" was the LAST
// crew disposition the cockpit roster header rendered (plan_crew.html's "N done"
// tally chip) that the index chip FACE never surfaced — working/queued/stuck/
// failed/spend all render on BOTH surfaces, but a finished crew was legible on the
// index only in this chip's hover tooltip. iter-64 folds a calm "· N done" facet
// onto the index crew chip to close that index↔cockpit parity gap.
//
// The facet is GATED ON 0 < Done < Total (the discriminating case), NOT the
// cockpit's bare Done > 0 — the priority-pill / redundant-ink (iter-60..63)
// "render only where the value discriminates" discipline. When every crew is done
// (Done == Total) the bare "N crew" total beside the ABSENCE of any working/
// queued/stuck facet already reads "all N finished", so "N crew · N done" would be
// redundant ink; and reconcileTerminal folds Working+Idle into Done on a terminal
// plan, driving Done == Total, so a closed roster self-suppresses the facet.

// --- unit: HasDoneFacet gate --------------------------------------------------

func TestPlanCrewRollup_HasDoneFacet(t *testing.T) {
	cases := []struct {
		name string
		roll planCrewRollup
		want bool
	}{
		// Mixed: some done, some still working → discriminating, facet shows.
		{"mixed working+done", planCrewRollup{Total: 3, Working: 1, Done: 2}, true},
		// Mixed: some done, some queued.
		{"mixed queued+done", planCrewRollup{Total: 2, Idle: 1, Done: 1}, true},
		// Mixed: some done, some stuck/failed.
		{"mixed failed+done", planCrewRollup{Total: 3, Failed: 1, Done: 2}, true},
		// All crew done (Done == Total) → redundant with the bare "N crew" total.
		{"all done", planCrewRollup{Total: 3, Done: 3}, false},
		// No done crew at all.
		{"none done", planCrewRollup{Total: 2, Working: 2, Done: 0}, false},
		// No crew.
		{"empty", planCrewRollup{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.roll.HasDoneFacet(); got != c.want {
				t.Errorf("HasDoneFacet() = %v, want %v (roll=%+v)", got, c.want, c.roll)
			}
		})
	}
}

// --- render: the MIXED case surfaces the "· N done" facet on the index --------

// seedMixedDonePlanCrew returns a fake whose sole ACTIVE plan has one working crew
// AND one cleanly-finished (closed) crew — so Done=1 < Total=2, the discriminating
// case where "· 1 done" adds signal beside "· 1 working".
func seedMixedDonePlanCrew(planStatus string) *fakeDaemon {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = planStatus
	f.beads["gd-step1"].Status = "open"
	seedCrew(f, "gd-working", "gardenio-prefix-cache-www", "gd-step1", "working", "in_progress")
	done := seedCrew(f, "gd-done", "gardenio-prefix-cache-ddd", "gd-step1", "done", "closed")
	done.Fields["wrapup"] = `{"tldr":"finished the wiring; MR merged."}`
	return f
}

func TestPlansIndex_MixedDoneRendersDoneFacet(t *testing.T) {
	f := seedMixedDonePlanCrew("in_progress")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if !strings.Contains(card, "plan-card-crew-done") {
		t.Errorf("a mixed plan (some crew done, some working) must render the done facet class; card:\n%s", card)
	}
	if !strings.Contains(card, "· 1 done") {
		t.Errorf("the done facet must read '· 1 done'; card:\n%s", card)
	}
	// Anti-over-reach: the working crew's live facet still renders alongside.
	if !strings.Contains(card, "plan-card-crew-working") {
		t.Errorf("the working crew's live facet must still render alongside the done facet")
	}
}

// --- render: the ALL-DONE case suppresses the facet (redundant-ink discipline) -

// A plan whose crew are ALL done (Done == Total) must NOT render "· N done" — the
// bare "N crew" total beside no active facet already says "all finished", so the
// facet would be pure redundant ink (the iter-60..63 discipline).
func TestPlansIndex_AllDoneNoDoneFacet(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	f.beads["gd-step1"].Status = "open"
	d1 := seedCrew(f, "gd-done1", "gardenio-prefix-cache-dd1", "gd-step1", "done", "closed")
	d1.Fields["wrapup"] = `{"tldr":"one."}`
	d2 := seedCrew(f, "gd-done2", "gardenio-prefix-cache-dd2", "gd-step1", "done", "closed")
	d2.Fields["wrapup"] = `{"tldr":"two."}`
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if strings.Contains(card, "plan-card-crew-done") {
		t.Errorf("an all-done plan must NOT render the done facet (Done==Total is redundant ink); card:\n%s", card)
	}
	// The bare crew total still renders (2 crew, all finished).
	if !strings.Contains(card, "2 crew") {
		t.Errorf("the crew chip total must still render '2 crew'; card:\n%s", card)
	}
}

// A plan with NO done crew renders no done facet — add signal, not ink.
func TestPlansIndex_NoDoneCrewNoDoneFacet(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "in_progress"
	f.beads["gd-step1"].Status = "open"
	seedCrew(f, "gd-working", "gardenio-prefix-cache-www", "gd-step1", "working", "in_progress")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if strings.Contains(card, "plan-card-crew-done") {
		t.Errorf("a plan with no done crew must not render the done facet; card:\n%s", card)
	}
}

// --- render: a TERMINAL plan self-suppresses the facet ------------------------

// A closed plan folds Working+Idle into Done (reconcileTerminal), driving
// Done==Total, so the discriminating gate self-suppresses the facet — the crew
// twin of iter-30's terminal live-signal quiet. A closed roster shows the bare
// "N crew" history, never "· N done" beside it.
func TestPlansIndex_TerminalPlanNoDoneFacet(t *testing.T) {
	f := seedMixedDonePlanCrew("closed")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if strings.Contains(card, "plan-card-crew-done") {
		t.Errorf("a closed plan folds Working→Done (Done==Total) and must render no done facet; card:\n%s", card)
	}
}

// --- index ↔ cockpit parity ---------------------------------------------------

// The index rollup and the cockpit roster classify the same crew set identically
// for the done disposition, so the index face facet and the cockpit "N done" chip
// read the SAME count by construction.
func TestDoneFacet_IndexCockpitParity(t *testing.T) {
	f := seedMixedDonePlanCrew("in_progress")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	cockpit := s.planCrew(context.Background(), "", steps, nil)
	index := s.planCrewBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-step1"},
	}, nil)["gd-plan1"]

	if cockpit.Done != index.Done {
		t.Errorf("parity: cockpit.Done=%d != index.Done=%d", cockpit.Done, index.Done)
	}
	if cockpit.Total != index.Total || cockpit.Working != index.Working {
		t.Errorf("parity drift working/total: cockpit %d/%d vs index %d/%d",
			cockpit.Working, cockpit.Total, index.Working, index.Total)
	}
	// The gate fires on the shared tally: Done=1 < Total=2.
	if !index.HasDoneFacet() {
		t.Errorf("index rollup HasDoneFacet()=false, want true for Done=%d Total=%d", index.Done, index.Total)
	}
}
