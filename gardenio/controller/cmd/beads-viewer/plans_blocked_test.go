package main

// plans_blocked_test.go — coverage for the plan-level blocker-state signal
// (ka-0000005352 iter-55): a raw status=blocked plan card folds a compact blocker
// facet onto its status chip — LOUD "· N blocked · Xage" when open blockers
// remain, CALM "· ✓ cleared" when every blocks-edge blocker is closed (so the
// operator sees the plan can be unblocked). Measured live, 4 of 5 blocked plans
// had EVERY blocker already closed, so the bare "blocked" chip was a card-vs-truth
// false-stall. Four levels: the pure planBlockerInfo projection truth-table, the
// planBlockedBulk join (open / cleared / zero-fan-out-when-none), the HTTP render
// of the facet across open/cleared/stale, and an anti-over-reach guard that a
// non-blocked plan renders no facet.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestPlanBlockerInfo_Projections pins the pure gates + wait-age against a fixed
// clock: Any/HasOpen/Cleared/OldestAge/OldestStale across the states the join
// produces.
func TestPlanBlockerInfo_Projections(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		info      planBlockerInfo
		wantAny   bool
		wantOpen  bool
		wantAge   string
		wantStale bool
	}{
		{
			name: "no blockers at all (zero value)",
			info: planBlockerInfo{},
		},
		{
			name:     "one fresh open blocker (2d)",
			info:     planBlockerInfo{Total: 1, Open: 1, oldestOpenAt: now.Add(-2 * 24 * time.Hour)},
			wantAny:  true,
			wantOpen: true,
			wantAge:  "2d",
		},
		{
			name:      "one stale open blocker (10d)",
			info:      planBlockerInfo{Total: 3, Open: 2, oldestOpenAt: now.Add(-10 * 24 * time.Hour)},
			wantAny:   true,
			wantOpen:  true,
			wantAge:   "10d",
			wantStale: true,
		},
		{
			name:      "exactly threshold (7d) is stale",
			info:      planBlockerInfo{Total: 1, Open: 1, oldestOpenAt: now.Add(-7 * 24 * time.Hour)},
			wantAny:   true,
			wantOpen:  true,
			wantAge:   "7d",
			wantStale: true,
		},
		{
			name:    "all blockers closed → cleared, no age",
			info:    planBlockerInfo{Total: 2, Open: 0},
			wantAny: true,
		},
		{
			name:     "open blocker with unset stamp → count only, never stale",
			info:     planBlockerInfo{Total: 1, Open: 1},
			wantAny:  true,
			wantOpen: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.Any(); got != tc.wantAny {
				t.Errorf("Any() = %v, want %v", got, tc.wantAny)
			}
			if got := tc.info.HasOpen(); got != tc.wantOpen {
				t.Errorf("HasOpen() = %v, want %v", got, tc.wantOpen)
			}
			if got := tc.info.OldestAge(now); got != tc.wantAge {
				t.Errorf("OldestAge() = %q, want %q", got, tc.wantAge)
			}
			if got := tc.info.OldestStale(now); got != tc.wantStale {
				t.Errorf("OldestStale() = %v, want %v", got, tc.wantStale)
			}
		})
	}
}

// seedBlockedPlan turns the seeded plan (gd-plan1) into a raw status=blocked plan
// with two forward blocks-edge blockers: one open (created openAgo before now) +
// (when closedToo) one closed. Returns nothing — mutates the fake in place.
func seedBlockedPlan(f *fakeDaemon, now time.Time, openAgo time.Duration, openBlocker bool, closedBlocker bool) {
	f.beads["gd-plan1"].Status = "blocked"
	f.hotEpicsResp.Beads[0].Status = "blocked"
	if f.deps == nil {
		f.deps = map[string][]beadsapi.Dependency{}
	}
	if openBlocker {
		f.beads["gd-blk-open"] = &beadsapi.BeadDetail{
			ID: "gd-blk-open", Title: "Resolve the SPEC_LIST bypass", Type: "task",
			Status: "open", CreatedAt: now.Add(-openAgo),
		}
		f.deps["gd-plan1"] = append(f.deps["gd-plan1"], beadsapi.Dependency{
			BeadID: "gd-plan1", DependsOnID: "gd-blk-open", Type: "blocks",
		})
	}
	if closedBlocker {
		f.beads["gd-blk-closed"] = &beadsapi.BeadDetail{
			ID: "gd-blk-closed", Title: "Bank the ready MR", Type: "decision",
			Status: "closed", CreatedAt: now.Add(-30 * 24 * time.Hour),
		}
		f.deps["gd-plan1"] = append(f.deps["gd-plan1"], beadsapi.Dependency{
			BeadID: "gd-plan1", DependsOnID: "gd-blk-closed", Type: "blocks",
		})
	}
}

// TestPlanBlockedBulk_OpenAndClosed proves the join counts total vs open blockers
// and tracks the oldest OPEN blocker's stamp (ignoring the closed one).
func TestPlanBlockedBulk_OpenAndClosed(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	seedBlockedPlan(f, now, 5*24*time.Hour, true, true)
	s := testServerWithDaemon(f)

	got := s.planBlockedBulk(context.Background(), []string{"gd-plan1"})
	info := got["gd-plan1"]
	if info.Total != 2 {
		t.Errorf("Total = %d, want 2 (one open + one closed blocker)", info.Total)
	}
	if info.Open != 1 {
		t.Errorf("Open = %d, want 1", info.Open)
	}
	if !info.HasOpen() {
		t.Error("HasOpen = false, want true")
	}
	if got := info.OldestAge(now); got != "5d" {
		t.Errorf("OldestAge = %q, want 5d (off the OPEN blocker, not the 30d closed one)", got)
	}
}

// TestPlanBlockedBulk_AllCleared proves a blocked plan whose every blocker is
// closed reports its blockers with no open count.
func TestPlanBlockedBulk_AllCleared(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	seedBlockedPlan(f, now, 0, false, true)
	s := testServerWithDaemon(f)

	info := s.planBlockedBulk(context.Background(), []string{"gd-plan1"})["gd-plan1"]
	if info.Total != 1 || info.Open != 0 {
		t.Errorf("Total=%d Open=%d, want 1/0", info.Total, info.Open)
	}
	if info.HasOpen() {
		t.Error("HasOpen = true, want false")
	}
}

// TestPlanBlockedBulk_ZeroFanoutWhenNoneBlocked is the load-bearing efficiency
// guard: with an empty blocked-id set the join issues ZERO daemon calls (the
// common case — most pages carry no blocked plan).
func TestPlanBlockedBulk_ZeroFanoutWhenNoneBlocked(t *testing.T) {
	f := seedPlansFake()
	s := testServerWithDaemon(f)
	before := f.batchGetDepsCalls
	got := s.planBlockedBulk(context.Background(), nil)
	if len(got) != 0 {
		t.Errorf("expected empty result for nil blocked-id set, got %d entries", len(got))
	}
	if f.batchGetDepsCalls != before {
		t.Errorf("planBlockedBulk issued %d BatchGetDependencies calls for an empty set, want 0",
			f.batchGetDepsCalls-before)
	}
}

// TestPlansIndex_BlockedChipShowsOpenBlockerFacet proves an open blocker renders
// the loud "· N blocked · Xage" facet on the rendered status chip.
func TestPlansIndex_BlockedChipShowsOpenBlockerFacet(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	seedBlockedPlan(f, now, 3*24*time.Hour, true, true)
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Match the RENDERED chip class attribute (the loud open modifier lands in the
	// chip's class list), not the CSS rule of the same name in the <style> block.
	if !strings.Contains(body, `class="chip chip-status chip-status-blocked chip-status-blocked-open`) {
		t.Errorf("expected the rendered chip to carry the loud chip-status-blocked-open modifier; body missing it")
	}
	if !strings.Contains(body, "· 1 blocked") {
		t.Errorf("expected the '· 1 blocked' facet; body missing it")
	}
	if !strings.Contains(body, `<span class="chip-status-blocked-age">· 3d`) {
		t.Errorf("expected the '· 3d' oldest-open wait-age; body missing it")
	}
	if !strings.Contains(body, "1 of 2 blockers still open") {
		t.Errorf("expected the '1 of 2 blockers still open' tooltip; body missing it")
	}
	// A 3-day blocker is under the 7d bar — no stale tint on the rendered chip
	// (the stale modifier only ever follows the open modifier in the class list).
	if strings.Contains(body, `chip-status-blocked-open chip-status-blocked-stale`) {
		t.Errorf("a 3-day-old blocker must NOT carry the stale tint (threshold is 7d)")
	}
	// The calm cleared marker must NOT render when an open blocker exists.
	if strings.Contains(body, "can be unblocked") {
		t.Errorf("the '✓ cleared' marker must not render while an open blocker remains")
	}
}

// TestPlansIndex_BlockedChipStaleTint proves an aged (>7d) open blocker gains the
// loud stale tint + names the staleness in the tooltip.
func TestPlansIndex_BlockedChipStaleTint(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	seedBlockedPlan(f, now, 10*24*time.Hour, true, false)
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	body := w.Body.String()
	if !strings.Contains(body, `chip-status-blocked-open chip-status-blocked-stale`) {
		t.Errorf("a 10-day-old blocker must carry the rendered stale tint modifier; body missing it")
	}
	if !strings.Contains(body, "oldest open 10d (stale)") {
		t.Errorf("expected the stale tooltip 'oldest open 10d (stale)'; body missing it")
	}
}

// TestPlansIndex_BlockedChipClearedMarker proves a blocked plan whose blockers are
// ALL closed renders the calm "✓ cleared" marker (the false-stall reconcile: the
// plan can be unblocked) and NOT the loud open facet.
func TestPlansIndex_BlockedChipClearedMarker(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	seedBlockedPlan(f, now, 0, false, true)
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	body := w.Body.String()
	if !strings.Contains(body, `class="chip-status-blocked-facet chip-status-blocked-cleared-facet">`) {
		t.Errorf("expected the rendered calm '✓ cleared' facet span; body missing it")
	}
	if !strings.Contains(body, "cleared") {
		t.Errorf("expected the 'cleared' text; body missing it")
	}
	if !strings.Contains(body, "this plan can be unblocked") {
		t.Errorf("expected the cleared tooltip 'this plan can be unblocked'; body missing it")
	}
	// The rendered chip must carry the cleared modifier, not the loud open one.
	if strings.Contains(body, `chip-status-blocked chip-status-blocked-open`) {
		t.Errorf("the loud open modifier must not render when every blocker is closed")
	}
	if !strings.Contains(body, `chip-status-blocked chip-status-blocked-cleared"`) {
		t.Errorf("expected the rendered chip to carry the chip-status-blocked-cleared modifier; body missing it")
	}
}

// TestPlansIndex_NonBlockedPlanNoFacet is the anti-over-reach guard: the default
// seeded plan is in_progress, so NO blocker facet renders — the fix touches only
// raw status=blocked cards.
func TestPlansIndex_NonBlockedPlanNoFacet(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	body := w.Body.String()
	// Match rendered facet markers (never the CSS rule names in the <style> block).
	if strings.Contains(body, `chip-status-blocked chip-status-blocked-open`) ||
		strings.Contains(body, `class="chip-status-blocked-facet`) ||
		strings.Contains(body, "can be unblocked") {
		t.Errorf("a non-blocked plan must render no blocker facet")
	}
}
