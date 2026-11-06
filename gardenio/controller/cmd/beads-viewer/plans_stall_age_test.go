package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// plans_stall_age_test.go — coverage for the dead-agent orphan wait-age folded
// onto the ⚠ stalled badge FACE (ka-0000005352 iteration 11). The decision chip
// (iter-4, "· 32d") and the escalation badge (iter-8, "· 40d") already surface
// their temporal urgency; this completes the attention-badge trio so the stall
// badge — the last one whose face carried no age — reads how long the orphaned
// step has sat frozen. Three levels: the oldestDeadStepUpdatedAt primitive (same
// dead-step predicate as deadStepCount), the withDeadAgentAge stamping (only a
// dead-agent stall with a datable orphan gets an age), and the HTTP render of the
// "· Nd" suffix + stale tint on both the index card and the cockpit header.

// --- oldestDeadStepUpdatedAt: the derivation primitive ----------------------

// TestOldestDeadStepUpdatedAt proves the age scans the SAME set deadStepCount
// counts (grace + liveness predicate), returns the OLDEST datable orphan, and
// skips a within-grace / live / zero-stamp step so it can never describe a step
// the count did not include.
func TestOldestDeadStepUpdatedAt(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	// "alive" reports only gardenio-live-1 as live.
	alive := func(name string) bool { return name == "gardenio-live-1" }

	oldFrozen := now.Add(-3 * 24 * time.Hour) // 3d ago (dead, oldest)
	midFrozen := now.Add(-5 * time.Hour)      // 5h ago (dead, newer)
	withinGrace := now.Add(-30 * time.Minute) // inside the 1h grace → not dead

	cases := []struct {
		name string
		refs []stallStepRef
		want time.Time
	}{
		{"no refs → zero", nil, time.Time{}},
		{
			"single dead orphan → its stamp",
			[]stallStepRef{{Assignee: "gardenio-vanished-1", UpdatedAt: midFrozen}},
			midFrozen,
		},
		{
			"two dead orphans → the older",
			[]stallStepRef{
				{Assignee: "gardenio-vanished-2", UpdatedAt: midFrozen},
				{Assignee: "gardenio-vanished-1", UpdatedAt: oldFrozen},
			},
			oldFrozen,
		},
		{
			"a live agent's step is not dead → skipped",
			[]stallStepRef{
				{Assignee: "gardenio-live-1", UpdatedAt: oldFrozen}, // live → not dead
				{Assignee: "gardenio-vanished-1", UpdatedAt: midFrozen},
			},
			midFrozen,
		},
		{
			"within-grace step is not dead → skipped even with an absent agent",
			[]stallStepRef{
				{Assignee: "gardenio-vanished-1", UpdatedAt: withinGrace},
			},
			time.Time{},
		},
		{
			"dead step with a zero stamp is undatable → skipped",
			[]stallStepRef{
				{Assignee: "gardenio-vanished-1", UpdatedAt: time.Time{}}, // dead but undatable
			},
			time.Time{},
		},
		{
			"a datable orphan wins over an undatable one",
			[]stallStepRef{
				{Assignee: "gardenio-vanished-1", UpdatedAt: time.Time{}},
				{Assignee: "gardenio-vanished-2", UpdatedAt: oldFrozen},
			},
			oldFrozen,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := oldestDeadStepUpdatedAt(tc.refs, now, alive); !got.Equal(tc.want) {
				t.Errorf("oldestDeadStepUpdatedAt = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOldestDeadStepUpdatedAt_AgreesWithDeadStepCount is the anti-drift guard:
// whenever deadStepCount counts ≥1 dead step AND at least one is datable,
// oldestDeadStepUpdatedAt returns a non-zero instant; whenever the count is 0,
// it returns zero. The age can never describe a set the count disagrees with.
func TestOldestDeadStepUpdatedAt_AgreesWithDeadStepCount(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	alive := func(name string) bool { return name == "gardenio-live-1" }
	frozen := now.Add(-2 * time.Hour)

	cases := []struct {
		name         string
		refs         []stallStepRef
		wantCount    int
		wantDatedAge bool
	}{
		{"no dead steps", []stallStepRef{{Assignee: "gardenio-live-1", UpdatedAt: frozen}}, 0, false},
		{"one dead datable step", []stallStepRef{{Assignee: "gardenio-gone-1", UpdatedAt: frozen}}, 1, true},
		{"one dead undatable step", []stallStepRef{{Assignee: "gardenio-gone-1", UpdatedAt: time.Time{}}}, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCount := deadStepCount(tc.refs, now, alive)
			if gotCount != tc.wantCount {
				t.Fatalf("deadStepCount = %d, want %d", gotCount, tc.wantCount)
			}
			gotAge := oldestDeadStepUpdatedAt(tc.refs, now, alive)
			if tc.wantDatedAge && gotAge.IsZero() {
				t.Errorf("expected a non-zero age when a datable dead step exists")
			}
			if !tc.wantDatedAge && !gotAge.IsZero() {
				t.Errorf("expected a zero age (no datable dead step), got %v", gotAge)
			}
			if gotCount == 0 && !gotAge.IsZero() {
				t.Errorf("count is 0 but age is non-zero (%v) — the age described a step the count did not", gotAge)
			}
		})
	}
}

// --- withDeadAgentAge: the stamping gate ------------------------------------

// TestWithDeadAgentAge proves the age is stamped ONLY for a dead-agent stall
// (Stalled AND DeadSteps ≥ 1) with a datable orphan, and left empty for the
// gardener-paused/no-gardener conditions, a healthy plan, and an undatable orphan.
func TestWithDeadAgentAge(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	alive := func(string) bool { return false }
	frozen2d := now.Add(-2 * 24 * time.Hour) // stale (> 1d)
	frozen5h := now.Add(-5 * time.Hour)      // fresh (< 1d)
	deadRefs2d := []stallStepRef{{Assignee: "gardenio-gone-1", UpdatedAt: frozen2d}}
	deadRefs5h := []stallStepRef{{Assignee: "gardenio-gone-1", UpdatedAt: frozen5h}}

	t.Run("dead-agent stall, stale orphan → age + stale", func(t *testing.T) {
		info := withDeadAgentAge(planStallInfo{Stalled: true, DeadSteps: 1}, deadRefs2d, now, alive)
		if info.AgeLabel != "2d" {
			t.Errorf("AgeLabel = %q, want %q", info.AgeLabel, "2d")
		}
		if !info.AgeStale {
			t.Errorf("a 2-day-frozen orphan must be stale (threshold 1d)")
		}
	})
	t.Run("dead-agent stall, fresh orphan → age, no stale", func(t *testing.T) {
		info := withDeadAgentAge(planStallInfo{Stalled: true, DeadSteps: 1}, deadRefs5h, now, alive)
		if info.AgeLabel != "5h" {
			t.Errorf("AgeLabel = %q, want %q", info.AgeLabel, "5h")
		}
		if info.AgeStale {
			t.Errorf("a 5-hour-frozen orphan must NOT be stale (threshold 1d)")
		}
	})
	t.Run("gardener-paused stall (no dead steps) → no age", func(t *testing.T) {
		info := withDeadAgentAge(planStallInfo{Stalled: true, DeadSteps: 0, GardenerState: "paused"}, nil, now, alive)
		if info.AgeLabel != "" || info.AgeStale {
			t.Errorf("a gardener-paused stall must carry no age; got %q stale=%v", info.AgeLabel, info.AgeStale)
		}
	})
	t.Run("healthy plan → no age", func(t *testing.T) {
		info := withDeadAgentAge(planStallInfo{Stalled: false}, deadRefs2d, now, alive)
		if info.AgeLabel != "" || info.AgeStale {
			t.Errorf("a non-stalled info must carry no age; got %q stale=%v", info.AgeLabel, info.AgeStale)
		}
	})
	t.Run("dead-agent stall, undatable orphan → no age", func(t *testing.T) {
		refs := []stallStepRef{{Assignee: "gardenio-gone-1", UpdatedAt: time.Time{}}}
		info := withDeadAgentAge(planStallInfo{Stalled: true, DeadSteps: 1}, refs, now, alive)
		if info.AgeLabel != "" || info.AgeStale {
			t.Errorf("an undatable orphan must render count-only (no age); got %q stale=%v", info.AgeLabel, info.AgeStale)
		}
	})
}

// --- HTTP render: index card + cockpit header -------------------------------

// seedDeadAgentStallPlan flips the seeded plan's step to an in_progress orphan
// under a vanished agent that last moved `frozenAgo` before planAssistantTestNow,
// with a RUNNING gardener + a live assistant so ONLY the dead-agent condition
// fires (isolating the age). Returns a mux clocked to planAssistantTestNow.
func seedDeadAgentStallPlan(t *testing.T, frozenAgo time.Duration) http.Handler {
	t.Helper()
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)
	f.beads["gd-step1"].Status = "in_progress"
	f.beads["gd-step1"].Assignee = "gardenio-vanished-agent-1"
	f.beads["gd-step1"].UpdatedAt = planAssistantTestNow.Add(-frozenAgo)
	return mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })
}

// TestPlansIndex_StallBadgeShowsFreshDeadAgentAge proves a dead-agent orphan
// under the 1-day bar renders the "· Nh" age suffix WITHOUT the stale tint.
func TestPlansIndex_StallBadgeShowsFreshDeadAgentAge(t *testing.T) {
	mux := seedDeadAgentStallPlan(t, 5*time.Hour)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "⚠ stalled") {
		t.Fatalf("expected the ⚠ stall badge; body missing it")
	}
	if !strings.Contains(body, `<span class="plan-stall-badge-age">· 5h`) {
		t.Errorf("expected the '· 5h' dead-agent age suffix; body missing it")
	}
	// 5h is under the 1-day threshold — no stale tint on the rendered badge.
	if strings.Contains(body, `class="plan-stall-badge plan-stall-badge-stale"`) {
		t.Errorf("a 5-hour-frozen orphan must NOT carry the stale tint (threshold 1d)")
	}
}

// TestPlansIndex_StallBadgeShowsStaleDeadAgentAge proves an orphan frozen past
// the 1-day bar gains the .plan-stall-badge-stale class + the stale tooltip so it
// pops out of the board.
func TestPlansIndex_StallBadgeShowsStaleDeadAgentAge(t *testing.T) {
	mux := seedDeadAgentStallPlan(t, 3*24*time.Hour)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="plan-stall-badge plan-stall-badge-stale"`) {
		t.Errorf("a 3-day-frozen orphan must carry the .plan-stall-badge-stale tint; body missing it")
	}
	if !strings.Contains(body, `<span class="plan-stall-badge-age">· 3d`) {
		t.Errorf("expected the '· 3d' age suffix; body missing it")
	}
	if !strings.Contains(body, "oldest dead-agent step frozen 3d (stale)") {
		t.Errorf("expected the stale tooltip naming the frozen duration; body missing it")
	}
	// The reason discriminator is still present — the age adds to it, not replaces.
	if !strings.Contains(body, `<span class="plan-stall-badge-reason">· 1 dead step`) {
		t.Errorf("the '1 dead step' reason must still render alongside the age")
	}
}

// TestPlansIndex_StallBadgeNoAgeForGardenerCondition proves the gardener-paused
// stall (no dead steps) renders reason-only — no age suffix, no stale tint —
// because the "updated Xh ago" meta beside the badge already IS its "stalled
// since". This is the deliberate density decision, not an omission.
func TestPlansIndex_StallBadgeNoAgeForGardenerCondition(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false") // paused → gardener-paused stall
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "⚠ stalled") {
		t.Fatalf("expected the ⚠ stall badge (paused gardener, open work); body missing it")
	}
	if !strings.Contains(body, "gardener paused") {
		t.Errorf("expected the 'gardener paused' reason discriminator")
	}
	// Match the RENDERED age span, not the .plan-stall-badge-age CSS rule that
	// always lives in the <style> block.
	if strings.Contains(body, `<span class="plan-stall-badge-age">`) {
		t.Errorf("a gardener-paused stall must render NO age suffix (the 'updated' meta is its 'stalled since')")
	}
	if strings.Contains(body, `class="plan-stall-badge plan-stall-badge-stale"`) {
		t.Errorf("a gardener-paused stall must never carry the dead-agent stale tint")
	}
}

// TestPlanDetail_StallBadgeShowsDeadAgentAge proves the cockpit header renders
// the SAME dead-agent age the index card does, so the two surfaces agree.
func TestPlanDetail_StallBadgeShowsDeadAgentAge(t *testing.T) {
	mux := seedDeadAgentStallPlan(t, 2*24*time.Hour)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans/gd-plan1", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<span class="plan-stall-badge-age">· 2d`) {
		t.Errorf("cockpit header should carry the '· 2d' dead-agent age; body missing it")
	}
	if !strings.Contains(body, `class="plan-stall-badge plan-stall-badge-stale"`) {
		t.Errorf("a 2-day-frozen orphan must carry the stale tint on the cockpit header too")
	}
}
