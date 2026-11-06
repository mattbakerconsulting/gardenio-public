package main

// plans_mr_age_test.go — iter-40 (ka-0000005352): the open-MR wait-age on the
// plan-card MR pill face. The MR pill was the LAST card presence-signal carrying
// no temporal urgency — every sibling wait-context signal already surfaces "how
// long" (decision chip "· 32d" iter-4, escalation badge "· 40d" iter-8, stall
// badge "· 3d" iter-11, stuck/queued crew facets "· 6h"/"· 12m" iter-13/32). A
// green MR opened 40 minutes ago read identically to one open 3 weeks awaiting a
// human merge. planOpenMRsBulk now folds the MR's since-opened age onto the pill
// (OpenAge), tinting it loud (OpenStale) past planMROpenStaleThreshold (7d).
//
// Covers: the age/stale derivation truth-table (fresh / stale / boundary /
// undatable), that the age keys on CreatedAt not UpdatedAt, the render
// fail-without/pass-with, the must-not-regress (undatable MR reads exactly as
// before — no age), and that a CI-failed pill keeps its louder red chrome when
// also stale.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The mrsTestServer clock is pinned at 2026-07-05 12:00 UTC (see plans_mrs_test.go).
// seedProducedMR sets only UpdatedAt, so a fixture that does not stamp CreatedAt
// carries a zero CreatedAt and reads age-less, exactly as before iter-40 (the
// must-not-regress default). The tests below set CreatedAt directly on the
// returned bead to exercise the age/stale derivation.
var mrAgeTestNow = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

// --- unit truth-table: OpenAge / OpenStale in planOpenMRsBulk ----------------

func TestPlanOpenMRsBulk_OpenAgeTruthTable(t *testing.T) {
	cases := []struct {
		name      string
		created   time.Time
		wantAge   string
		wantStale bool
	}{
		// Fresh: opened 3h ago → calm "3h", not stale.
		{"fresh_3h", mrAgeTestNow.Add(-3 * time.Hour), "3h", false},
		// A day-old MR → "1d", still calm (below the 7d bar).
		{"one_day", mrAgeTestNow.Add(-25 * time.Hour), "1d", false},
		// Exactly at the 7d boundary → stale (>= threshold).
		{"boundary_7d", mrAgeTestNow.Add(-7 * 24 * time.Hour), "7d", true},
		// Just under 7d → NOT stale.
		{"just_under_7d", mrAgeTestNow.Add(-7*24*time.Hour + time.Minute), "6d", false},
		// Egregiously long → "21d", loud stale.
		{"three_weeks", mrAgeTestNow.Add(-21 * 24 * time.Hour), "21d", true},
		// Undatable (zero CreatedAt) → no age, not stale (reads as before iter-40).
		{"undatable", time.Time{}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := seedPlansFake()
			mr := seedProducedMR(f, "gd-mrAge", "gd-step1", "410", "feat: age", "opened", "open",
				mrAgeTestNow.Add(-time.Hour)) // UpdatedAt distinct from CreatedAt
			mr.CreatedAt = c.created
			s := mrsTestServer(f)

			info := s.planOpenMRsBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}})["gd-plan1"]
			if !info.Any() || info.Count() != 1 {
				t.Fatalf("Any=%v Count=%d, want true/1", info.Any(), info.Count())
			}
			got := info.MRs[0]
			if got.OpenAge != c.wantAge {
				t.Errorf("OpenAge = %q, want %q", got.OpenAge, c.wantAge)
			}
			if got.OpenStale != c.wantStale {
				t.Errorf("OpenStale = %v, want %v", got.OpenStale, c.wantStale)
			}
		})
	}
}

// TestPlanOpenMRsBulk_AgeKeysOnCreatedNotUpdated proves the wait-age answers "how
// long has this MR sat OPEN" (CreatedAt) and not "how long since last touched"
// (UpdatedAt) — a CI webhook or comment bumps UpdatedAt continuously, so keying
// on it would reset the age on every pipeline event and never read stale. The MR
// was created 10d ago (stale) but touched 5m ago; the age must read the 10d
// creation age, not 5m.
func TestPlanOpenMRsBulk_AgeKeysOnCreatedNotUpdated(t *testing.T) {
	f := seedPlansFake()
	mr := seedProducedMR(f, "gd-mrOld", "gd-step1", "411", "feat: long open", "opened", "open",
		mrAgeTestNow.Add(-5*time.Minute)) // UpdatedAt: touched 5m ago
	mr.CreatedAt = mrAgeTestNow.Add(-10 * 24 * time.Hour) // opened 10d ago
	s := mrsTestServer(f)

	got := s.planOpenMRsBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}})["gd-plan1"].MRs[0]
	if got.OpenAge != "10d" {
		t.Errorf("OpenAge = %q, want 10d (keyed on CreatedAt, not the 5m-ago UpdatedAt)", got.OpenAge)
	}
	if !got.OpenStale {
		t.Errorf("a 10d-open MR must read stale even though it was touched 5m ago")
	}
}

// --- render regression: the index card MR pill -------------------------------

// TestHandlePlans_MRPillRendersOpenAge is the fail-without/pass-with render
// guard: a datable open MR renders the "· Nd" age facet + (past 7d) the loud
// plan-card-mr-stale tint on the pill. Reverting the template age spans reds this.
func TestHandlePlans_MRPillRendersOpenAge(t *testing.T) {
	f := seedPlansFake()
	// Opened 9 days ago → stale.
	mr := seedProducedMR(f, "gd-mrStale", "gd-step1", "412", "feat: stale", "opened", "open",
		mrAgeTestNow.Add(-time.Hour))
	mr.CreatedAt = mrAgeTestNow.Add(-9 * 24 * time.Hour)
	mux := mkServerWithNow(t, f, func() time.Time { return mrAgeTestNow })

	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if !strings.Contains(card, "plan-card-mr-age") {
		t.Errorf("a datable open MR must render the plan-card-mr-age facet; card:\n%s", card)
	}
	if !strings.Contains(card, "· 9d") {
		t.Errorf("the pill must render the '· 9d' since-opened age; card:\n%s", card)
	}
	if !strings.Contains(card, "plan-card-mr-stale") {
		t.Errorf("a 9d-open MR must tint the pill loud (plan-card-mr-stale); card:\n%s", card)
	}
}

// TestHandlePlans_FreshMRPillCalm proves a freshly-opened MR renders the age but
// NOT the stale tint — the calm two-tier default (context, not an alarm).
func TestHandlePlans_FreshMRPillCalm(t *testing.T) {
	f := seedPlansFake()
	mr := seedProducedMR(f, "gd-mrFresh", "gd-step1", "413", "feat: fresh", "opened", "open",
		mrAgeTestNow.Add(-time.Hour))
	mr.CreatedAt = mrAgeTestNow.Add(-2 * time.Hour) // opened 2h ago
	mux := mkServerWithNow(t, f, func() time.Time { return mrAgeTestNow })

	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	if !strings.Contains(card, "· 2h") {
		t.Errorf("a fresh open MR must still render its age (· 2h); card:\n%s", card)
	}
	if strings.Contains(card, "plan-card-mr-stale") {
		t.Errorf("a 2h-open MR must NOT tint the pill stale; card:\n%s", card)
	}
}

// TestHandlePlans_UndatableMRPillNoAge is the must-not-regress guard: an MR with
// no CreatedAt (a legacy/fixture bead) renders the pill EXACTLY as before iter-40
// — no age facet, no stale tint — so the increment is purely additive.
func TestHandlePlans_UndatableMRPillNoAge(t *testing.T) {
	f := seedPlansFake()
	// seedProducedMR leaves CreatedAt zero — the undatable case.
	seedProducedMR(f, "gd-mrNoDate", "gd-step1", "414", "feat: legacy", "opened", "open",
		mrAgeTestNow.Add(-time.Hour))
	mux := mkServerWithNow(t, f, func() time.Time { return mrAgeTestNow })

	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	// The pill still renders (the MR is open) …
	if !strings.Contains(card, "!414") {
		t.Fatalf("the open MR pill must still render; card:\n%s", card)
	}
	// … but with no age facet and no stale tint.
	if strings.Contains(card, "plan-card-mr-age") {
		t.Errorf("an undatable MR must render NO age facet; card:\n%s", card)
	}
	if strings.Contains(card, "plan-card-mr-stale") {
		t.Errorf("an undatable MR must NOT tint the pill stale; card:\n%s", card)
	}
}

// TestHandlePlans_StaleAndFailedMRKeepsCIFailedChrome proves the CSS ordering
// contract: a pill that is BOTH stale (>7d open) AND CI-failed carries both
// classes, and because .plan-card-mr-ci-failed is declared AFTER
// .plan-card-mr-stale in the stylesheet, the louder CI-failed red chrome wins
// (a broken pipeline outranks mere age). We assert both classes are present on
// the SAME pill element (the render guarantees the ordering; the CSS cascade
// resolves the chrome).
func TestHandlePlans_StaleAndFailedMRKeepsCIFailedChrome(t *testing.T) {
	f := seedPlansFake()
	mr := withPipelineStatus(
		seedProducedMR(f, "gd-mrBoth", "gd-step1", "415", "feat: red+old", "opened", "open",
			mrAgeTestNow.Add(-time.Hour)),
		"failed")
	mr.CreatedAt = mrAgeTestNow.Add(-12 * 24 * time.Hour) // 12d open AND failed
	mux := mkServerWithNow(t, f, func() time.Time { return mrAgeTestNow })

	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	card := isolatePlanCard(t, w.Body.String(), "gd-plan1")
	// Find the pill element and assert both classes ride it.
	pill := isolateMRPill(t, card, "415")
	if !strings.Contains(pill, "plan-card-mr-ci-failed") {
		t.Errorf("a failed pill must carry plan-card-mr-ci-failed; pill: %q", pill)
	}
	if !strings.Contains(pill, "plan-card-mr-stale") {
		t.Errorf("a 12d-open pill must carry plan-card-mr-stale; pill: %q", pill)
	}
	if !strings.Contains(pill, "· 12d") {
		t.Errorf("the pill must render the 12d age; pill: %q", pill)
	}
}

// isolateMRPill extracts the single <a class="plan-card-mr …">…</a> element whose
// iid label is "!iid" from a card fragment, so a class assertion checks THAT pill
// and not the card's other pills or the shared <style> block.
func isolateMRPill(t *testing.T, card, iid string) string {
	t.Helper()
	needle := "!" + iid
	idx := strings.Index(card, needle)
	if idx < 0 {
		t.Fatalf("MR pill !%s not found in card:\n%s", iid, card)
	}
	start := strings.LastIndex(card[:idx], "<a ")
	if start < 0 {
		t.Fatalf("opening <a for MR pill !%s not found", iid)
	}
	end := strings.Index(card[idx:], "</a>")
	if end < 0 {
		t.Fatalf("closing </a> for MR pill !%s not found", iid)
	}
	return card[start : idx+end+len("</a>")]
}
