package main

// plans_now_step_age_test.go — iter-43 (ka-0000005352): the in-progress "now"
// step's running-age folded onto the /plans now/next chip face. The now/next
// chip was the LAST card signal that named a specific step but carried no
// temporal facet — every sibling wait-context signal already surfaces "how
// long": the decision chip ("· 32d", iter-4), the escalation badge ("· 40d",
// iter-8), the stall badge ("· 3d", iter-11), the stuck/queued crew facets
// ("· 6h"/"· 12m", iter-13/32), and the open-MR pill ("· 8d", iter-40). A step
// in_progress for 3 minutes read identically to one grinding for 2 weeks.
//
// setStepAge (formerly setNowStepAge; generalized to the blocked arm in iter-48)
// folds the in_progress step's age (NowAge) onto the chip, tinting it loud
// (NowStale) past planStallStaleThreshold (24h — the SAME bar the ⚠ stall badge
// uses for a frozen step). Covers: the age/stale derivation truth-table (fresh /
// boundary / stale / next-arm / blocked-arm-sets-no-NowAge / undatable / nil
// receiver), the render fail-without/pass-with on the index card, and the
// must-not-regress (a next step reads no age, exactly as before). The blocked
// arm's OWN wedge-age (BlockedAge) is covered by plans_blocked_step_age_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The now/next age tests pin the clock at 2026-07-10 12:00 UTC so a step's
// UpdatedAt offset maps deterministically onto the humanized age.
var nowStepAgeTestNow = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

// --- unit truth-table: setStepAge (the "now" arm) ----------------------------

func TestSetNowStepAge_TruthTable(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		updated   time.Time
		wantAge   string
		wantStale bool
	}{
		// Fresh in_progress: running 20m → calm "20m", not stale.
		{"now_fresh_20m", "in_progress", nowStepAgeTestNow.Add(-20 * time.Minute), "20m", false},
		// Running a few hours, still under the 24h bar → calm "5h".
		{"now_5h", "in_progress", nowStepAgeTestNow.Add(-5 * time.Hour), "5h", false},
		// Exactly at the 24h boundary → stale (>= threshold), reads "1d".
		{"now_boundary_24h", "in_progress", nowStepAgeTestNow.Add(-24 * time.Hour), "1d", true},
		// Just under 24h → NOT stale, reads "23h".
		{"now_just_under_24h", "in_progress", nowStepAgeTestNow.Add(-24*time.Hour + time.Minute), "23h", false},
		// Egregiously long → loud stale "14d".
		{"now_two_weeks", "in_progress", nowStepAgeTestNow.Add(-14 * 24 * time.Hour), "14d", true},
		// A "next" (open) step has NOT started, so it carries no running-age even
		// with a datable updated_at — the in_progress arm only.
		{"next_no_age", "open", nowStepAgeTestNow.Add(-5 * time.Hour), "", false},
		// A "blocked" step carries no NOW-age (it sets its own BlockedAge instead,
		// iter-48 — covered by plans_blocked_step_age_test.go); NowAge stays empty.
		{"blocked_no_now_age", "blocked", nowStepAgeTestNow.Add(-5 * 24 * time.Hour), "", false},
		// An undatable in_progress step (zero updatedAt) reads exactly as before
		// iter-43 — no age (matching fmtTimeAgo's own zero-time contract).
		{"now_undatable", "in_progress", time.Time{}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &planCardStep{Status: c.status, updatedAt: c.updated}
			s.setStepAge(nowStepAgeTestNow)
			if s.NowAge != c.wantAge {
				t.Errorf("NowAge = %q, want %q", s.NowAge, c.wantAge)
			}
			if s.NowStale != c.wantStale {
				t.Errorf("NowStale = %v, want %v", s.NowStale, c.wantStale)
			}
		})
	}
}

// TestSetNowStepAge_NilReceiver proves the method is a no-op on a nil receiver,
// so the enrich seam can call row.CurrentOrNextStep.setStepAge(now)
// unconditionally on a plan with no open step (CurrentOrNextStep == nil) without
// a guard and without panicking.
func TestSetNowStepAge_NilReceiver(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("setStepAge on a nil receiver panicked: %v", r)
		}
	}()
	var s *planCardStep
	s.setStepAge(nowStepAgeTestNow) // must not panic
}

// --- render regression: fail-without / pass-with -----------------------------

// TestPlansIndex_NowStepShowsRunningAge proves the index card's now/next chip
// renders the in-progress step's running-age (fresh, calm) — the fail-without
// half is that removing the setStepAge call drops this facet.
func TestPlansIndex_NowStepShowsRunningAge(t *testing.T) {
	f := seedPlansFake()
	// Make step1 the in_progress "now" step, running 5h (calm, below the 24h bar).
	f.beads["gd-step1"].Status = "in_progress"
	f.beads["gd-step1"].UpdatedAt = nowStepAgeTestNow.Add(-5 * time.Hour)
	mux := mkServerWithNow(t, f, func() time.Time { return nowStepAgeTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-step-kind-now") {
		t.Fatalf("expected the 'now' step kind on the card")
	}
	// The running-age facet renders with its "· 5h" face + aria-label.
	if !strings.Contains(body, `class="plan-step-age" aria-label="running 5h">· 5h</span>`) {
		t.Errorf("expected the '· 5h' running-age facet on the now chip; body:\n%s", body)
	}
	// A calm (fresh) step must NOT tint the whole chip loud. Assert on the
	// applied class in the rendered chip markup, not the bare class name — the
	// name also appears in the page's <style> block (same trap as the blocked
	// step test).
	if strings.Contains(body, "plan-card-step-link plan-card-step-now-stale") {
		t.Errorf("a fresh in_progress step must not render the loud stale chip")
	}
	// The tooltip names the running duration.
	if !strings.Contains(body, "running 5h") {
		t.Errorf("expected the 'running 5h' tooltip qualifier on the chip")
	}
}

// TestPlansIndex_NowStepStaleReadsLoud proves an in_progress step grinding past
// the 24h staleness bar tints the whole chip loud (plan-card-step-now-stale) and
// marks the tooltip "(stale)" — the same loud-when-egregious treatment the
// stuck-crew / MR-open wait-ages use.
func TestPlansIndex_NowStepStaleReadsLoud(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	f.beads["gd-step1"].UpdatedAt = nowStepAgeTestNow.Add(-3 * 24 * time.Hour) // running 3d
	mux := mkServerWithNow(t, f, func() time.Time { return nowStepAgeTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Assert on the applied class in the rendered chip markup, not the bare class
	// name (which also appears in the page's <style> block).
	if !strings.Contains(body, "plan-card-step-link plan-card-step-now-stale") {
		t.Errorf("expected the loud stale chip class applied on the chip for a 3d-old in_progress step")
	}
	if !strings.Contains(body, `class="plan-step-age" aria-label="running 3d">· 3d</span>`) {
		t.Errorf("expected the '· 3d' running-age facet on the stale now chip")
	}
	if !strings.Contains(body, "running 3d (stale)") {
		t.Errorf("expected the '(stale)' qualifier in the chip tooltip")
	}
}

// TestPlansIndex_NextStepNoRunningAge is the must-not-regress: a "next" (open)
// step carries NO running-age — it has not started, so the age facet + stale
// tint must be absent, exactly as the card read before iter-43. Guards against
// the age leaking onto the next/blocked arms.
func TestPlansIndex_NextStepNoRunningAge(t *testing.T) {
	f := seedPlansFake()
	// step1 stays open ("next"); give it a datable updated_at that WOULD read
	// stale IF the age wrongly applied to a non-in_progress step.
	f.beads["gd-step1"].Status = "open"
	f.beads["gd-step1"].UpdatedAt = nowStepAgeTestNow.Add(-10 * 24 * time.Hour)
	mux := mkServerWithNow(t, f, func() time.Time { return nowStepAgeTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-step-kind-next") {
		t.Fatalf("expected the 'next' step kind on the card")
	}
	if strings.Contains(body, `class="plan-step-age"`) {
		t.Errorf("a 'next' step must render no running-age facet")
	}
	// Assert on the applied class in the rendered chip markup, not the bare class
	// name (which also appears in the page's <style> block).
	if strings.Contains(body, "plan-card-step-link plan-card-step-now-stale") {
		t.Errorf("a 'next' step must never tint the loud stale chip")
	}
}
