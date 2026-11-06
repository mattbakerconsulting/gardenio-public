package main

// plans_escalation_age_test.go — coverage for the oldest blocking-escalation
// wait-age folded into the "⛔ blocked by escalation" badge (ka-0000003887,
// standing plan ka-0000005352 iteration 8). The escalation badge is the #2
// attention tier (planAttentionEscalation) whose ranker treats age AS the
// urgency signal and planEscalationsBulk already sorts refs oldest-first; this
// makes that age legible on the card face — the symmetric parallel of iter-4's
// decision-chip wait-age (plans_decision_age_test.go). Three levels: the pure
// planEscalationInfo.OldestAge/OldestStale/oldestCreatedAt projection (incl. the
// unstamped-guard), the HTTP render of the age suffix + stale tint across
// fresh/stale/no-stamp, and a no-new-fan-out assertion.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPlanEscalationInfo_OldestAge pins the pure projection: OldestAge humanizes
// the earliest ref's createdAt against a fixed clock, empty on an empty list.
func TestPlanEscalationInfo_OldestAge(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-40 * 24 * time.Hour)
	newer := now.Add(-2 * 24 * time.Hour)

	// planEscalationsBulk sorts oldest-first, so the head is the oldest.
	info := planEscalationInfo{
		Count: 2,
		Escalations: []planEscalationRef{
			{ID: "gd-old", createdAt: oldest},
			{ID: "gd-new", createdAt: newer},
		},
	}
	if got := info.OldestAge(now); got != "40d" {
		t.Errorf("OldestAge = %q, want %q", got, "40d")
	}
	if got := (planEscalationInfo{}).OldestAge(now); got != "" {
		t.Errorf("empty info OldestAge = %q, want \"\"", got)
	}
}

// TestPlanEscalationInfo_OldestStale pins the staleness gate: false under the
// 7-day threshold, true past it, and false when the list is empty or unstamped.
func TestPlanEscalationInfo_OldestStale(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		info planEscalationInfo
		want bool
	}{
		{"empty", planEscalationInfo{}, false},
		{"fresh (2d)", planEscalationInfo{Count: 1, Escalations: []planEscalationRef{
			{createdAt: now.Add(-2 * 24 * time.Hour)}}}, false},
		{"exactly threshold (7d)", planEscalationInfo{Count: 1, Escalations: []planEscalationRef{
			{createdAt: now.Add(-7 * 24 * time.Hour)}}}, true},
		{"stale (40d)", planEscalationInfo{Count: 1, Escalations: []planEscalationRef{
			{createdAt: now.Add(-40 * 24 * time.Hour)}}}, true},
		{"unstamped never stale", planEscalationInfo{Count: 1, Escalations: []planEscalationRef{
			{ID: "gd-x"}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.OldestStale(now); got != tc.want {
				t.Errorf("OldestStale = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPlanEscalationInfo_OldestCreatedAtSkipsUnstamped proves the guard: an
// unstamped ref sorts LAST in planEscalationsBulk, so if it ever leads the list
// the age must still reflect the oldest REAL (non-zero) stamp, never the zero.
func TestPlanEscalationInfo_OldestCreatedAtSkipsUnstamped(t *testing.T) {
	real := time.Date(2026, 6, 20, 8, 0, 0, 0, time.UTC)
	info := planEscalationInfo{
		Count: 2,
		Escalations: []planEscalationRef{
			{ID: "gd-nostamp"},               // zero createdAt
			{ID: "gd-real", createdAt: real}, // the only datable ref
		},
	}
	if got := info.oldestCreatedAt(); !got.Equal(real) {
		t.Errorf("oldestCreatedAt = %v, want %v (the unstamped ref must not win)", got, real)
	}
	// All-unstamped → zero → no age, never stale.
	allZero := planEscalationInfo{Count: 1, Escalations: []planEscalationRef{{ID: "gd-z"}}}
	if got := allZero.oldestCreatedAt(); !got.IsZero() {
		t.Errorf("all-unstamped oldestCreatedAt = %v, want zero", got)
	}
}

// mkEscAgeTestServer builds a one-plan fake whose plan is blocked by an open
// escalation created escAgo before a pinned clock, so the rendered badge wait-age
// is deterministic. Returns the handler and the fake (for call-count assertions).
func mkEscAgeTestServer(t *testing.T, escAgo time.Duration) (http.Handler, *fakeDaemon) {
	t.Helper()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	esc := seedPlanEscalation(f, "gd-esc1", "gd-step1", "Grant IAM for the prod deploy", "aws_iam", "internal")
	esc.CreatedAt = now.Add(-escAgo)
	return mkServerWithNow(t, f, func() time.Time { return now }), f
}

// TestPlansIndex_EscalationBadgeShowsFreshWaitAge proves a recent blocking
// escalation renders the "· 3d" age suffix on the badge but NOT the stale tint —
// the loud-but-translucent default with a dim age qualifier.
func TestPlansIndex_EscalationBadgeShowsFreshWaitAge(t *testing.T) {
	mux, _ := mkEscAgeTestServer(t, 3*24*time.Hour)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "blocked by escalation") {
		t.Fatalf("expected the ⛔ escalation badge; body missing it")
	}
	if !strings.Contains(body, `<span class="plan-esc-badge-age">· 3d`) {
		t.Errorf("expected the '· 3d' wait-age suffix span on the escalation badge; body missing it")
	}
	// A 3-day-old escalation is under the 7-day threshold — the rendered badge must
	// NOT carry the stale modifier. (Match the rendered class attribute, not the
	// CSS rule of the same class name in the <style> block.)
	if strings.Contains(body, `class="plan-esc-badge plan-esc-badge-stale"`) {
		t.Errorf("a 3-day-old escalation must NOT carry the stale tint (threshold is 7d)")
	}
}

// TestPlansIndex_EscalationBadgeShowsStaleWaitAge proves an aged (>7d) blocking
// escalation gains the .plan-esc-badge-stale class + the stale tooltip so it pops
// out of the board.
func TestPlansIndex_EscalationBadgeShowsStaleWaitAge(t *testing.T) {
	mux, _ := mkEscAgeTestServer(t, 40*24*time.Hour)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="plan-esc-badge plan-esc-badge-stale"`) {
		t.Errorf("a 40-day-old escalation must carry the .plan-esc-badge-stale tint on the rendered badge; body missing it")
	}
	if !strings.Contains(body, `<span class="plan-esc-badge-age">· 40d`) {
		t.Errorf("expected the '· 40d' wait-age suffix; body missing it")
	}
	// The tooltip must name the staleness so the signal is explained, not just colored.
	if !strings.Contains(body, "oldest waiting 40d (stale)") {
		t.Errorf("expected the stale tooltip 'oldest waiting 40d (stale)'; body missing it")
	}
}

// TestPlansIndex_EscalationBadgeNoAgeWhenStampUnset proves the density-discipline
// fallback: an escalation with an unset CreatedAt renders exactly as before
// (badge + count only, no suffix, no tint) — the age never manufactures a stamp.
func TestPlansIndex_EscalationBadgeNoAgeWhenStampUnset(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	esc := seedPlanEscalation(f, "gd-esc1", "gd-step1", "Legacy fork, no stamp", "general", "")
	esc.CreatedAt = time.Time{} // legacy row: no creation stamp

	mux := mkServerWithNow(t, f, func() time.Time { return now })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "blocked by escalation") {
		t.Fatalf("expected the ⛔ escalation badge; body missing it")
	}
	if strings.Contains(body, `<span class="plan-esc-badge-age">`) {
		t.Errorf("an unset-CreatedAt escalation must render NO age suffix span (count only)")
	}
	if strings.Contains(body, `class="plan-esc-badge plan-esc-badge-stale"`) {
		t.Errorf("an unset-CreatedAt escalation must NOT be tinted stale")
	}
}

// TestPlanEscalationsBulk_AgeAddsNoFanOut proves the density contract: the
// wait-age is a pure projection off the refs planEscalationsBulk already
// hydrates + sorts, so OldestAge/OldestStale add ZERO daemon round-trips beyond
// the escalation aggregation's own budget (PATH-1 deps+beads + PATH-2 pin list).
func TestPlanEscalationsBulk_AgeAddsNoFanOut(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	esc := seedPlanEscalation(f, "gd-esc1", "gd-step1", "Grant IAM for the prod deploy", "aws_iam", "internal")
	esc.CreatedAt = now.Add(-40 * 24 * time.Hour)
	s := testServerWithDaemon(f)

	depsBefore, beadsBefore := f.batchGetDepsCalls, f.batchGetBeadsCalls
	listBefore := len(f.listAllQ)

	info := s.planEscalationsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-plan1", "gd-step1"},
	})["gd-plan1"]
	if info.Count != 1 {
		t.Fatalf("Count = %d, want 1", info.Count)
	}
	// The projection reads the ALREADY-hydrated refs — no further daemon calls.
	if got := info.OldestAge(now); got != "40d" {
		t.Errorf("OldestAge = %q, want 40d", got)
	}
	if !info.OldestStale(now) {
		t.Errorf("OldestStale = false, want true (40d > 7d threshold)")
	}
	// PATH-1 fires exactly one forward deps batch + one blocker hydration batch;
	// PATH-2 fires exactly one page-wide open-escalation list. The age methods add
	// NOTHING on top — assert the aggregation's own budget is unchanged.
	if got := f.batchGetDepsCalls - depsBefore; got != 1 {
		t.Errorf("BatchGetDependencies calls = %d, want 1 (PATH-1 forward blocks walk only)", got)
	}
	if got := f.batchGetBeadsCalls - beadsBefore; got != 1 {
		t.Errorf("BatchGetBeads calls = %d, want 1 (PATH-1 blocker hydration only)", got)
	}
	if got := len(f.listAllQ) - listBefore; got != 1 {
		t.Errorf("ListBeadsFiltered calls = %d, want 1 (PATH-2 requesting_plan pin only)", got)
	}
}
