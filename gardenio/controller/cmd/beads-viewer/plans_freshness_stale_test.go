package main

// plans_freshness_stale_test.go — coverage for the freshness-staleness tint on
// the /plans index card's "updated Xh ago" stamp (standing plan ka-0000005352
// iteration 38). The stamp was the LAST card age-signal with no stale escalation:
// every other wait-age (the decision chip .chip-attention-stale/14d, the
// escalation badge .plan-esc-badge-stale/7d, the stall badge
// .plan-stall-badge-stale/24h, the stuck/queued crew facets/1h) already tints
// loud past its bar, but the plan's own freshness stamp rendered the same muted
// gray whether it moved 6 minutes or 3 weeks ago — so an operator scanning the
// board could not spot a rotting/abandoned ACTIVE plan at a glance.
//
// Three levels, mirroring the escalation/stall wait-age suites
// (plans_escalation_age_test.go, plans_stall_age_test.go): the pure
// planFreshnessStale projection truth-table (incl. the TERMINAL-never-tints and
// undatable guards), the HTTP render of the .plan-card-updated-stale class across
// fresh/stale/terminal, and a no-new-fan-out assertion (the tint is a pure
// projection off the lastActivityAt the row already carries).

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPlanFreshnessStale pins the pure derivation: an ACTIVE plan tints once its
// newest activity crosses the 7-day bar; a TERMINAL (closed/deferred) plan never
// tints (its age is honest history); an undatable/zero stamp never tints.
func TestPlanFreshnessStale(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		status  string
		lastAct time.Time
		want    bool
	}{
		{"fresh active (2d)", "in_progress", now.Add(-2 * 24 * time.Hour), false},
		{"just under the bar (6d)", "open", now.Add(-6 * 24 * time.Hour), false},
		{"exactly at the bar (7d)", "open", now.Add(-7 * 24 * time.Hour), true},
		{"stale active (21d)", "in_progress", now.Add(-21 * 24 * time.Hour), true},
		{"blocked active is still a plan (14d)", "blocked", now.Add(-14 * 24 * time.Hour), true},
		// The load-bearing trap: a terminal plan is SUPPOSED to look old. A closed
		// or deferred plan sitting weeks in the past is honest history, not a stall.
		{"closed 21d NEVER tints", "closed", now.Add(-21 * 24 * time.Hour), false},
		{"deferred 21d NEVER tints", "deferred", now.Add(-21 * 24 * time.Hour), false},
		{"CLOSED case-insensitive NEVER tints", "CLOSED", now.Add(-21 * 24 * time.Hour), false},
		// An undatable (zero) lastActivity reads exactly as before — plain stamp.
		{"undatable active never tints", "open", time.Time{}, false},
		{"undatable terminal never tints", "closed", time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planFreshnessStale(tc.status, tc.lastAct, now); got != tc.want {
				t.Errorf("planFreshnessStale(%q, -%s) = %v, want %v",
					tc.status, now.Sub(tc.lastAct), got, tc.want)
			}
		})
	}
}

// mkFreshnessTestServer seeds the standard one-plan fake, then stamps the plan
// epic's status + UpdatedAt so its rendered freshness (planLastActivity folds the
// epic stamp with its stampless step, so the epic stamp wins) is exactly `ago`
// old against a fixed clock. Returns the mux + the fixed now.
func mkFreshnessTestServer(t *testing.T, status string, ago time.Duration) (http.Handler, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	plan := f.beads["gd-plan1"]
	plan.Status = status
	plan.UpdatedAt = now.Add(-ago)
	// hotEpicsResp carries the SAME pointer, so the status/stamp edit is seen by
	// the index route without re-seeding.
	return mkServerWithNow(t, f, func() time.Time { return now }), now
}

// TestPlansIndex_FreshnessStampFreshNoTint proves a recently-active plan renders
// the plain muted stamp — no .plan-card-updated-stale class, no stale tooltip.
func TestPlansIndex_FreshnessStampFreshNoTint(t *testing.T) {
	mux, _ := mkFreshnessTestServer(t, "in_progress", 2*24*time.Hour)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-card-updated") {
		t.Fatalf("expected the freshness stamp; body missing .plan-card-updated")
	}
	// Match the rendered class ATTRIBUTE (both classes) so we don't false-match the
	// .plan-card-updated-stale CSS rule in the <style> block.
	if strings.Contains(body, `class="meta plan-card-updated plan-card-updated-stale"`) {
		t.Errorf("a 2-day-old active plan must NOT carry the stale tint (threshold is 7d)")
	}
	if strings.Contains(body, "has not moved in over a week") {
		t.Errorf("a fresh plan must NOT render the stale tooltip")
	}
}

// TestPlansIndex_FreshnessStampStaleTints proves an aged (>7d) ACTIVE plan gains
// the .plan-card-updated-stale class + the stale tooltip so it pops out of the
// board. This is the fail-without/pass-with seam — reverting the plan_card.html
// class seam reds this test.
func TestPlansIndex_FreshnessStampStaleTints(t *testing.T) {
	mux, _ := mkFreshnessTestServer(t, "in_progress", 21*24*time.Hour)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Match the rendered class attribute on the stamp (both classes present),
	// not the CSS rule of the same name in the <style> block.
	if !strings.Contains(body, `class="meta plan-card-updated plan-card-updated-stale"`) {
		t.Errorf("a 21-day-old active plan must carry .plan-card-updated-stale on the rendered stamp; body missing it")
	}
	// The tooltip must name the staleness so the signal is explained, not just colored.
	if !strings.Contains(body, "has not moved in over a week (stale)") {
		t.Errorf("expected the stale tooltip naming the staleness; body missing it")
	}
}

// TestPlansIndex_FreshnessStampTerminalNeverTints is the must-not-regress bar: a
// CLOSED plan sitting weeks in the past keeps the plain muted stamp — its age is
// honest history, not a stall (the iter-24/25/27/30 terminal-reconcile family). A
// finished plan is SUPPOSED to look old.
func TestPlansIndex_FreshnessStampTerminalNeverTints(t *testing.T) {
	for _, status := range []string{"closed", "deferred"} {
		t.Run(status, func(t *testing.T) {
			mux, _ := mkFreshnessTestServer(t, status, 21*24*time.Hour)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans?status=all", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			body := w.Body.String()
			if !strings.Contains(body, "plan-card-updated") {
				t.Fatalf("expected the freshness stamp on the %s plan; body missing it", status)
			}
			// Match the rendered class ATTRIBUTE (both classes), not the CSS rule name.
			if strings.Contains(body, `class="meta plan-card-updated plan-card-updated-stale"`) {
				t.Errorf("a 21-day-old %s (terminal) plan must NOT be tinted stale — its age is honest history", status)
			}
		})
	}
}

// TestPlansIndex_FreshnessStaleAddsNoFanOut proves the density contract: the tint
// is a pure projection off the lastActivityAt the enrich pass already computes
// (epic stamp + step/phase + crew heartbeats) and the plan's own status — so it
// adds ZERO daemon round-trips. Compares the enrich call budget of a stale-active
// board against a fresh one; they must be identical.
func TestPlansIndex_FreshnessStaleAddsNoFanOut(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	runIndex := func(ago time.Duration) (deps, beads, list int) {
		f := seedPlansFake()
		f.beads["gd-plan1"].UpdatedAt = now.Add(-ago)
		srv := NewServer(f, slog.Default(), "", "", "", nil)
		srv.SetNow(func() time.Time { return now })
		mux := http.NewServeMux()
		srv.RegisterRoutes(mux)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		return f.batchGetDepsCalls, f.batchGetBeadsCalls, len(f.listAllQ)
	}

	fDeps, fBeads, fList := runIndex(2 * 24 * time.Hour)     // fresh
	stDeps, stBeads, stList := runIndex(21 * 24 * time.Hour) // stale

	if fDeps != stDeps || fBeads != stBeads || fList != stList {
		t.Errorf("freshness-stale tint changed the enrich budget: fresh=(deps=%d beads=%d list=%d) stale=(deps=%d beads=%d list=%d) — the tint must add zero fan-out",
			fDeps, fBeads, fList, stDeps, stBeads, stList)
	}
}
