package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plans_terminal_crew_test.go — iter-30 (ka-0000005352): a TERMINAL plan
// (closed/deferred) must NOT render a LIVE-NOW crew signal. A finished plan
// cannot have a crew "working now", yet a zombie crew (an OPEN agent bead still
// carrying an alive agent_state whose heartbeat stopped when the plan closed but
// whose bead was never reaped) was tallied Working, pulsed the green live-dot,
// and rendered a live face — so the card read "● N crew · M working" beside
// "updated 3 weeks ago". Same card-vs-truth family as iter-25 (terminal CTA),
// iter-27/28 (terminal lifecycle chip). Found live on the prod board:
// ka-0000001281 (closed 3wk, 13/13 100%, gardener retired) rendered "10 crew · 1
// working · ✕ 1 · $98.20" with the live pulse. planCrewRollup.reconcileTerminal
// (index) and planCrewInfo.reconcileTerminal (cockpit) fold the false-live crew
// into Done and quiet the pulse while preserving the honest record.

// seedTerminalPlanWithZombieCrew returns a fake whose sole plan epic carries the
// given terminal status but still has an OPEN crew agent in an alive state
// assigned to a leftover open step — the exact live shape (a closed plan with a
// zombie "working" crew). A second, cleanly-finished crew anchors the honest
// record (Total/Done) that must survive the reconcile.
func seedTerminalPlanWithZombieCrew(status string) *fakeDaemon {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = status
	f.beads["gd-step1"].Status = "open"
	f.epicSubtreeCount = map[string]int{"gd-plan1": 1, "gd-plan1|closed": 0}
	// The zombie: an OPEN bead in an alive state — classifyCrewBead reads it live.
	seedCrew(f, "gd-zombie", "gardenio-prefix-cache-zzz", "gd-step1", "working", "in_progress")
	// A cleanly-finished crew: the honest record the reconcile must preserve.
	done := seedCrew(f, "gd-doneCrew", "gardenio-prefix-cache-ddd", "gd-step1", "done", "closed")
	done.Fields["wrapup"] = `{"tldr":"Measured the cache-hit rate; MR merged."}`
	return f
}

// --- unit truth-table: planCrewRollup.reconcileTerminal (index) --------------

func TestPlanCrewRollup_ReconcileTerminal(t *testing.T) {
	base := func() planCrewRollup {
		return planCrewRollup{Total: 3, Working: 1, Done: 1, Failed: 1, Faces: []planCrewFace{{Name: "z"}}}
	}
	cases := []struct {
		status      string
		wantWorking int
		wantDone    int
		wantFaces   int
	}{
		// Terminal → the live crew folds into Done, faces cleared.
		{"closed", 0, 2, 0},
		{"deferred", 0, 2, 0},
		// Non-terminal → untouched, the live crew still pulses.
		{"in_progress", 1, 1, 1},
		{"open", 1, 1, 1},
		{"blocked", 1, 1, 1},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			got := base().reconcileTerminal(c.status)
			if got.Working != c.wantWorking {
				t.Errorf("status=%s: Working = %d, want %d", c.status, got.Working, c.wantWorking)
			}
			if got.Done != c.wantDone {
				t.Errorf("status=%s: Done = %d, want %d", c.status, got.Done, c.wantDone)
			}
			if len(got.Faces) != c.wantFaces {
				t.Errorf("status=%s: len(Faces) = %d, want %d", c.status, len(got.Faces), c.wantFaces)
			}
			// The RECORD is always preserved: Total, Failed untouched.
			if got.Total != 3 || got.Failed != 1 {
				t.Errorf("status=%s: record drifted Total/Failed = %d/%d, want 3/1", c.status, got.Total, got.Failed)
			}
			// HasLive() must agree with the reconciled Working count.
			if got.HasLive() != (c.wantWorking > 0) {
				t.Errorf("status=%s: HasLive() = %v, want %v", c.status, got.HasLive(), c.wantWorking > 0)
			}
		})
	}
}

// A terminal plan with NO live crew is untouched (idempotent — no phantom
// double-fold, and the Done tally does not move).
func TestPlanCrewRollup_ReconcileTerminal_NoLiveCrewUnchanged(t *testing.T) {
	r := planCrewRollup{Total: 2, Working: 0, Done: 1, Failed: 1}
	got := r.reconcileTerminal("closed")
	if got.Working != 0 || got.Done != 1 || got.Failed != 1 || got.Total != 2 {
		t.Errorf("no-live-crew terminal reconcile drifted: %+v, want Total2/Working0/Done1/Failed1", got)
	}
}

// --- unit truth-table: planCrewInfo.reconcileTerminal (cockpit) --------------

func TestPlanCrewInfo_ReconcileTerminal(t *testing.T) {
	base := func() planCrewInfo {
		return planCrewInfo{
			Total:   3,
			Working: 1,
			Done:    1,
			Failed:  1,
			Members: []planCrewMember{
				{ID: "gd-z", Live: true, Bucket: "alive", AgentState: "working"},
				{ID: "gd-d", Live: false, Bucket: "done", AgentState: "done"},
				{ID: "gd-f", Live: false, Bucket: "stuck", AgentState: "rate_limited", Failed: true},
			},
		}
	}
	t.Run("closed folds the zombie member to done and drops its pulse", func(t *testing.T) {
		got := base().reconcileTerminal("closed")
		if got.Working != 0 || got.Done != 2 {
			t.Errorf("Working/Done = %d/%d, want 0/2", got.Working, got.Done)
		}
		if got.HasLive() {
			t.Errorf("HasLive() must be false on a reconciled terminal roster")
		}
		z := got.Members[0]
		if z.Live {
			t.Errorf("the zombie member must lose its Live pulse")
		}
		if z.Bucket != "done" {
			t.Errorf("the folded zombie member Bucket = %q, want done", z.Bucket)
		}
		// The RAW agent_state string is a factual record — left as-is.
		if z.AgentState != "working" {
			t.Errorf("the member's raw AgentState must be preserved as a factual record, got %q", z.AgentState)
		}
		// A non-live member is untouched.
		if got.Members[2].Failed != true || got.Members[2].Bucket != "stuck" {
			t.Errorf("a failed member must be untouched by the terminal reconcile")
		}
	})
	t.Run("active roster is untouched", func(t *testing.T) {
		got := base().reconcileTerminal("in_progress")
		if got.Working != 1 || !got.Members[0].Live {
			t.Errorf("an active plan's live crew must still pulse")
		}
	})
}

// --- render regression: the index card (planCrewBulk → row.Crew) -------------

// TestPlansIndex_TerminalPlanNoLiveCrewPulse proves a CLOSED/DEFERRED plan with a
// zombie working crew renders NO live pulse: no plan-card-crew-live class, no
// live-dot, no "· N working" facet, no live face avatar. Without the reconcile
// the card would render all four.
func TestPlansIndex_TerminalPlanNoLiveCrewPulse(t *testing.T) {
	for _, status := range []string{"closed", "deferred"} {
		t.Run(status, func(t *testing.T) {
			f := seedTerminalPlanWithZombieCrew(status)
			mux := mkServerWithNow(t, f, crewFixtureNowFn)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			body := w.Body.String()
			card := isolatePlanCard(t, body, "gd-plan1")
			if strings.Contains(card, "plan-card-crew-live") {
				t.Errorf("%s plan must not render the live crew class (green pulse)", status)
			}
			if strings.Contains(card, `class="plan-card-crew-dot"`) {
				t.Errorf("%s plan must not render the pulsing live-dot", status)
			}
			if strings.Contains(card, "plan-card-crew-working") {
				t.Errorf("%s plan must not render the '· N working' facet", status)
			}
			if strings.Contains(card, "plan-card-crew-face") {
				t.Errorf("%s plan must not render a live crew face avatar", status)
			}
		})
	}
}

// TestPlansIndex_TerminalPlanKeepsCrewRecord proves the reconcile is surgical:
// the closed plan STILL renders the honest crew record — the "N crew" total and
// the spend facet — because that is backward-looking history, not a live signal.
func TestPlansIndex_TerminalPlanKeepsCrewRecord(t *testing.T) {
	f := seedTerminalPlanWithZombieCrew("closed")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if !strings.Contains(card, "plan-card-crew-total") {
		t.Errorf("a closed plan must still render its crew total (honest history)")
	}
	if !strings.Contains(card, "2 crew") {
		t.Errorf("a closed plan must still count all crew (2 crew); card was:\n%s", card)
	}
}

// TestPlansIndex_ActivePlanStillPulses is the anti-over-reach guard: an
// in_progress plan with a working crew MUST still render the live pulse + working
// facet — the reconcile suppresses the live signal only on a TERMINAL plan.
func TestPlansIndex_ActivePlanStillPulses(t *testing.T) {
	f := seedTerminalPlanWithZombieCrew("in_progress") // reuse the shape, but live
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if !strings.Contains(card, "plan-card-crew-live") {
		t.Errorf("a live in_progress plan with a working crew MUST still render the green pulse")
	}
	if !strings.Contains(card, "plan-card-crew-working") {
		t.Errorf("a live in_progress plan MUST still render the '· N working' facet")
	}
}

// --- render regression: the cockpit roster (planCrew) ------------------------

// TestPlanDetail_TerminalPlanCrewNoLivePulse proves the cockpit crew roster on a
// closed plan renders no live signal — no header "working" chip, no is-live
// member, no live-dot — matching the index card by construction.
func TestPlanDetail_TerminalPlanCrewNoLivePulse(t *testing.T) {
	f := seedTerminalPlanWithZombieCrew("closed")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Assert on the rendered ELEMENT markup, not the bare class — the class names
	// also appear in the page's shared <style> block, so a bare-substring check
	// would false-match the CSS rules.
	if strings.Contains(body, `class="chip plan-crew-count plan-crew-count-working"`) {
		t.Errorf("a closed plan's cockpit roster must not render the '(N) working' header chip")
	}
	if strings.Contains(body, `plan-crew-member plan-crew-member-alive`) && strings.Contains(body, ` is-live"`) {
		t.Errorf("a closed plan's cockpit roster must not mark any member is-live")
	}
	if strings.Contains(body, `<span class="plan-crew-live-dot"`) {
		t.Errorf("a closed plan's cockpit roster must not render a per-member live-dot element")
	}
	// The record survives: the roster still renders the done tally chip (the
	// zombie folded into it) — honest history, not a live signal.
	if !strings.Contains(body, `class="chip plan-crew-count plan-crew-count-done"`) {
		t.Errorf("a closed plan's cockpit roster must still render the done tally (honest history)")
	}
	// The folded zombie makes the done count 2 (the clean crew + the ex-working).
	if !strings.Contains(body, "2 done") {
		t.Errorf("the zombie working crew must fold into the done tally (2 done); body lacked it")
	}
}

// TestPlanDetail_ActivePlanCrewStillPulses is the cockpit anti-over-reach twin:
// an in_progress plan's working crew still renders the header working chip + a
// live member.
func TestPlanDetail_ActivePlanCrewStillPulses(t *testing.T) {
	f := seedTerminalPlanWithZombieCrew("in_progress")
	mux := mkServerWithNow(t, f, crewFixtureNowFn)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@acme.example")
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="chip plan-crew-count plan-crew-count-working"`) {
		t.Errorf("an active plan's cockpit roster MUST still render the '(N) working' header chip")
	}
	if !strings.Contains(body, `<span class="plan-crew-live-dot"`) {
		t.Errorf("an active plan's cockpit roster MUST still render a per-member live-dot element")
	}
}

// --- index ↔ cockpit parity --------------------------------------------------

// TestTerminalCrewReconcile_IndexCockpitParity proves the index rollup and the
// cockpit roster agree on the reconciled live-work signal for the SAME terminal
// plan: both report zero working after the fold, and both fold the zombie into
// done. This is the by-construction parity the family always holds.
func TestTerminalCrewReconcile_IndexCockpitParity(t *testing.T) {
	f := seedTerminalPlanWithZombieCrew("closed")
	s := crewTestServer(f)

	steps := []planStepView{{ID: "gd-step1", Title: "Wire the route", Number: 1, Status: "open"}}
	cockpit := s.planCrew(context.Background(), "", steps, nil).reconcileTerminal("closed")

	index := s.planCrewBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-step1"},
	}, nil)["gd-plan1"].reconcileTerminal("closed")

	if cockpit.Working != index.Working {
		t.Errorf("parity: cockpit.Working=%d != index.Working=%d", cockpit.Working, index.Working)
	}
	if cockpit.Working != 0 {
		t.Errorf("both surfaces must report 0 working on a terminal plan, got %d", cockpit.Working)
	}
	if cockpit.Done != index.Done {
		t.Errorf("parity: cockpit.Done=%d != index.Done=%d", cockpit.Done, index.Done)
	}
	if cockpit.Total != index.Total {
		t.Errorf("parity: cockpit.Total=%d != index.Total=%d", cockpit.Total, index.Total)
	}
}

// isolatePlanCard extracts the single <li class="plan-card" ...id="plan-card-ID">
// … </li> block for a given plan id from the rendered index body, so an assertion
// checks THAT card's markup and cannot false-match another card or the page's
// shared <style> block. Fails the test if the card is not found.
func isolatePlanCard(t *testing.T, body, planID string) string {
	t.Helper()
	anchor := `id="plan-card-` + planID + `"`
	start := strings.Index(body, anchor)
	if start < 0 {
		t.Fatalf("plan card %s not found in body", planID)
	}
	// Walk back to the opening <li of this card.
	liStart := strings.LastIndex(body[:start], "<li")
	if liStart < 0 {
		t.Fatalf("opening <li for plan card %s not found", planID)
	}
	// The card ends at the next card's <li or the list close; a simple bound is
	// the next "id=\"plan-card-" occurrence, else the end of body.
	rest := body[start+len(anchor):]
	next := strings.Index(rest, `id="plan-card-`)
	if next < 0 {
		return body[liStart:]
	}
	return body[liStart : start+len(anchor)+next]
}
