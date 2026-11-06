package main

// plans_blocked_step_age_test.go — iter-48 (ka-0000005352): the BLOCKED
// current/next step's wedge-age folded onto the /plans now/next chip face — the
// twin of iter-43's in_progress running-age (plans_now_step_age_test.go) for the
// arm iter-43 EXPLICITLY deferred.
//
// The now/next chip's blocked arm was the LAST arm carrying no temporal facet. A
// status=blocked step's wedge-age is surfaced by NO other card signal:
//
//   - deriveStall (plans_stall.go) counts ONLY in_progress steps under a dead
//     agent, so a status=blocked step NEVER lights the ⚠ stall badge;
//   - a plain status=blocked step with no blocks-edge never lights the ⛔
//     escalation badge either;
//   - the freshness stamp (iter-38) shows the plan's NEWEST activity, so a plan
//     with one live step + one long-wedged blocked step reads misleadingly fresh
//     (the exact misdirection iter-11 named for the dead-agent orphan).
//
// So a step blocked 6 days read identically to one blocked 6 minutes. A blocked
// step's updatedAt is its "blocked since" instant, so setStepAge folds
// BlockedAge / BlockedStale onto the chip, tinting loud past the SAME 24h
// planStallStaleThreshold NowAge + the ⚠ stall badge use. Covers: the
// blocked-arm age/stale derivation (via the shared setStepAge truth-table below,
// paired with Kind()), the render fail-without/pass-with (calm + loud stale) on
// the index card, and the anti-over-reach (a "now" step still reads its running-
// age, not a wedge-age; a "next" step reads neither).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The blocked-arm tests reuse the iter-43 pinned clock (2026-07-10 12:00 UTC) so
// a step's UpdatedAt offset maps deterministically onto the humanized age.

// --- unit truth-table: setStepAge on the blocked arm -------------------------

// TestSetStepAge_BlockedArm proves setStepAge sets the BlockedAge / BlockedStale
// pair (and leaves NowAge empty) for a status=blocked step, keyed off Kind() ==
// "blocked" — the twin of TestSetNowStepAge_TruthTable's "now" cases. It also
// pins the cross-arm invariant: a blocked step never sets NowAge, and a "now"
// step never sets BlockedAge, so the two arms can never bleed into each other.
func TestSetStepAge_BlockedArm(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		updated   time.Time
		wantAge   string
		wantStale bool
	}{
		// Fresh blocked: wedged 20m → calm "20m", not stale.
		{"blocked_fresh_20m", "blocked", nowStepAgeTestNow.Add(-20 * time.Minute), "20m", false},
		// Wedged a few hours, still under the 24h bar → calm "5h".
		{"blocked_5h", "blocked", nowStepAgeTestNow.Add(-5 * time.Hour), "5h", false},
		// Exactly at the 24h boundary → stale (>= threshold), reads "1d".
		{"blocked_boundary_24h", "blocked", nowStepAgeTestNow.Add(-24 * time.Hour), "1d", true},
		// Just under 24h → NOT stale, reads "23h".
		{"blocked_just_under_24h", "blocked", nowStepAgeTestNow.Add(-24*time.Hour + time.Minute), "23h", false},
		// Egregiously long → loud stale "14d" (a plan wedged two weeks).
		{"blocked_two_weeks", "blocked", nowStepAgeTestNow.Add(-14 * 24 * time.Hour), "14d", true},
		// An undatable blocked step (zero updatedAt) reads exactly as before iter-48
		// — no age (matching fmtTimeAgo's own zero-time contract).
		{"blocked_undatable", "blocked", time.Time{}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &planCardStep{Status: c.status, updatedAt: c.updated}
			s.setStepAge(nowStepAgeTestNow)
			if s.BlockedAge != c.wantAge {
				t.Errorf("BlockedAge = %q, want %q", s.BlockedAge, c.wantAge)
			}
			if s.BlockedStale != c.wantStale {
				t.Errorf("BlockedStale = %v, want %v", s.BlockedStale, c.wantStale)
			}
			// Cross-arm invariant: the blocked arm never sets the "now" running-age.
			if s.NowAge != "" || s.NowStale {
				t.Errorf("a blocked step must not set NowAge/NowStale; got NowAge=%q NowStale=%v", s.NowAge, s.NowStale)
			}
		})
	}
}

// TestSetStepAge_NowArmNeverSetsBlockedAge is the mirror of the cross-arm
// invariant: an in_progress ("now") step sets NowAge but NEVER BlockedAge, so
// the running-age can never leak onto the blocked facet.
func TestSetStepAge_NowArmNeverSetsBlockedAge(t *testing.T) {
	s := &planCardStep{Status: "in_progress", updatedAt: nowStepAgeTestNow.Add(-3 * 24 * time.Hour)}
	s.setStepAge(nowStepAgeTestNow)
	if s.NowAge != "3d" || !s.NowStale {
		t.Errorf("expected NowAge=3d NowStale=true, got NowAge=%q NowStale=%v", s.NowAge, s.NowStale)
	}
	if s.BlockedAge != "" || s.BlockedStale {
		t.Errorf("a 'now' step must not set BlockedAge/BlockedStale; got BlockedAge=%q BlockedStale=%v", s.BlockedAge, s.BlockedStale)
	}
}

// TestSetStepAge_NextArmAgeless is the anti-over-reach at the unit level: a ready
// "next" (open) step sets NEITHER age pair even with a datable, would-be-stale
// updatedAt — a ready step is not wedged and has not started, so it carries no
// per-step wait-age (its wait is the plan freshness stamp + READY chip's job).
func TestSetStepAge_NextArmAgeless(t *testing.T) {
	s := &planCardStep{Status: "open", updatedAt: nowStepAgeTestNow.Add(-10 * 24 * time.Hour)}
	s.setStepAge(nowStepAgeTestNow)
	if s.NowAge != "" || s.NowStale || s.BlockedAge != "" || s.BlockedStale {
		t.Errorf("a 'next' step must carry no age; got NowAge=%q NowStale=%v BlockedAge=%q BlockedStale=%v",
			s.NowAge, s.NowStale, s.BlockedAge, s.BlockedStale)
	}
}

// --- render regression: fail-without / pass-with -----------------------------

// blockedStepAgeMux seeds the shared blocked-next-step fake (step #2 blocked)
// with the given wedge-age (its UpdatedAt) and returns a server pinned to the
// iter-43 clock so the age humanizes deterministically.
func blockedStepAgeMux(t *testing.T, wedged time.Duration) http.Handler {
	t.Helper()
	f := seedPlanWithBlockedNextStep()
	f.beads["gd-step2"].UpdatedAt = nowStepAgeTestNow.Add(-wedged)
	return mkServerWithNow(t, f, func() time.Time { return nowStepAgeTestNow })
}

// TestPlansIndex_BlockedStepShowsWedgeAge proves the blocked chip renders the
// step's wedge-age (fresh, calm) — the fail-without half is that removing the
// BlockedAge span (or reverting setNowStepAge's Kind gate to "now" only) drops
// this facet.
func TestPlansIndex_BlockedStepShowsWedgeAge(t *testing.T) {
	mux := blockedStepAgeMux(t, 5*time.Hour) // wedged 5h, below the 24h bar → calm
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `plan-step-kind-blocked">blocked</span>`) {
		t.Fatalf("expected the red 'blocked' pill on the card (iter-14)")
	}
	// The wedge-age facet renders with its "· 5h" face + "blocked 5h" aria-label.
	if !strings.Contains(body, `class="plan-step-age" aria-label="blocked 5h">· 5h</span>`) {
		t.Errorf("expected the '· 5h' wedge-age facet on the blocked chip; body:\n%s", body)
	}
	// The tooltip names the blocked duration.
	if !strings.Contains(body, "blocked 5h") {
		t.Errorf("expected the 'blocked 5h' tooltip qualifier on the chip")
	}
	// A calm (fresh) wedge must NOT tint the whole chip loud. Assert on the applied
	// class in the rendered chip markup, not the bare class name (which also
	// appears in the page's <style> block).
	if strings.Contains(body, "plan-card-step-blocked-stale") &&
		strings.Contains(body, `plan-card-step-link plan-card-step-blocked-stale`) {
		t.Errorf("a fresh blocked step must not render the loud stale chip")
	}
}

// TestPlansIndex_BlockedStepStaleReadsLoud proves a blocked step wedged past the
// 24h staleness bar tints the whole chip loud (plan-card-step-blocked-stale) and
// marks the tooltip "(stale)" — the same loud-when-egregious treatment the
// now-arm running-age (iter-43) and the stuck-crew / MR-open wait-ages use. This
// is the mission's live case: two of the three measured blocked-next cards were
// wedged past 24h (5.7d, 3.7d).
func TestPlansIndex_BlockedStepStaleReadsLoud(t *testing.T) {
	mux := blockedStepAgeMux(t, 3*24*time.Hour) // wedged 3d → loud stale
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Assert on the applied class in the rendered chip markup, not the bare class
	// name (which also appears in the page's <style> block).
	if !strings.Contains(body, "plan-card-step-link plan-card-step-blocked-stale") {
		t.Errorf("expected the loud stale chip class applied on the chip for a 3d-old blocked step")
	}
	if !strings.Contains(body, `class="plan-step-age" aria-label="blocked 3d">· 3d</span>`) {
		t.Errorf("expected the '· 3d' wedge-age facet on the stale blocked chip")
	}
	if !strings.Contains(body, "blocked 3d (stale)") {
		t.Errorf("expected the '(stale)' qualifier in the chip tooltip")
	}
}

// TestPlansIndex_BlockedStepUndatableNoAge is the graceful-degradation guard: a
// blocked step with an unset updated_at renders the red 'blocked' pill exactly as
// before iter-48 — no age facet, no stale tint. Reads identically to the pre-iter
// card, matching the "undatable reads exactly as before" idiom every sibling
// wait-age uses.
func TestPlansIndex_BlockedStepUndatableNoAge(t *testing.T) {
	f := seedPlanWithBlockedNextStep()
	f.beads["gd-step2"].UpdatedAt = time.Time{} // undatable
	mux := mkServerWithNow(t, f, func() time.Time { return nowStepAgeTestNow })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `plan-step-kind-blocked">blocked</span>`) {
		t.Fatalf("expected the red 'blocked' pill even for an undatable blocked step")
	}
	if strings.Contains(body, `aria-label="blocked `) {
		t.Errorf("an undatable blocked step must render no wedge-age facet")
	}
	if strings.Contains(body, "plan-card-step-link plan-card-step-blocked-stale") {
		t.Errorf("an undatable blocked step must never tint the loud stale chip")
	}
}
