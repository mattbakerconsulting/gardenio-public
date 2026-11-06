package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// plans_decision_age_test.go — coverage for the oldest open-decision wait-age
// folded into the "N to answer" chip (ka-0000004999, standing plan
// ka-0000005352 iteration 4). The attention ranker already drains decisions
// oldest-waiting-first to surface the measured >30-day tail; this makes that age
// legible on the card face. Three levels: the earlierNonZeroTime primitive, the
// batched OldestDecisionAt derivation (unphased + phased, zero extra daemon
// calls), and the HTTP render of the age suffix + stale tint.

// TestEarlierNonZeroTime pins the min-side mirror of laterTime: a zero stamp is
// treated as "unset" and never wins, so folding a set of created_at stamps
// yields the OLDEST real one (never a spurious epoch-zero).
func TestEarlierNonZeroTime(t *testing.T) {
	early := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	late := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	var zero time.Time

	cases := []struct {
		name string
		a, b time.Time
		want time.Time
	}{
		{"both set, a earlier", early, late, early},
		{"both set, b earlier", late, early, early},
		{"equal", early, early, early},
		{"a zero -> b wins", zero, late, late},
		{"b zero -> a wins", early, zero, early},
		{"both zero -> zero", zero, zero, zero},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := earlierNonZeroTime(tc.a, tc.b); !got.Equal(tc.want) {
				t.Errorf("earlierNonZeroTime(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestPlanIndexStepData_OldestDecisionAtUnphased proves the batched child walk
// records the OLDEST open decision's CreatedAt (not a resolved one, not the
// newest), off the SAME reverse-deps + get batch that counts them — no extra
// daemon round-trip.
func TestPlanIndexStepData_OldestDecisionAtUnphased(t *testing.T) {
	f := &fakeDaemon{
		beads:   map[string]*beadsapi.BeadDetail{},
		revDeps: map[string][]beadsapi.Dependency{},
	}
	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "Plan 1", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
	f.beads[plan.ID] = plan
	addDec := func(id, status string, created time.Time) {
		f.beads[id] = &beadsapi.BeadDetail{
			ID: id, Title: "Q " + id, Kind: "issue", Type: "decision", Status: status,
			CreatedAt: created,
			UpdatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		}
		f.revDeps[plan.ID] = append(f.revDeps[plan.ID],
			beadsapi.Dependency{BeadID: id, DependsOnID: plan.ID, Type: "parent-child"})
	}
	oldest := time.Date(2026, 6, 20, 8, 0, 0, 0, time.UTC)
	// The oldest is an OPEN decision; a resolved decision older still must NOT win
	// (only open decisions block the plan), and a newer open decision must not
	// shadow the oldest open one.
	addDec("gd-dec-old", "open", oldest)
	addDec("gd-dec-new", "open", time.Date(2026, 7, 2, 8, 0, 0, 0, time.UTC))
	addDec("gd-dec-closed-older", "closed", time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC))

	s := &Server{daemon: f, logger: slog.Default()}
	out := s.planIndexStepData(context.Background(), []*beadsapi.BeadDetail{plan})
	info := out[plan.ID]
	if info == nil {
		t.Fatalf("expected a planIndexStepInfo for %s", plan.ID)
	}
	if info.OpenDecisions != 2 {
		t.Fatalf("OpenDecisions = %d, want 2", info.OpenDecisions)
	}
	if !info.OldestDecisionAt.Equal(oldest) {
		t.Errorf("OldestDecisionAt = %v, want %v (the oldest OPEN decision; the older resolved one must not win)",
			info.OldestDecisionAt, oldest)
	}
	// Zero extra round-trips: one reverse-deps batch + one get-beads batch, no
	// per-row query — the age must ride the walk that already counts decisions.
	if f.batchGetDepsCalls != 1 || f.batchGetBeadsCalls != 1 {
		t.Errorf("daemon calls = %d deps / %d beads, want 1/1 (the wait-age must add no round-trip)",
			f.batchGetDepsCalls, f.batchGetBeadsCalls)
	}
	if len(f.listAllQ) != 0 {
		t.Errorf("planIndexStepData issued %d ListBeadsFiltered call(s), want 0", len(f.listAllQ))
	}
}

// TestPlanIndexStepData_OldestDecisionAtSpansBothTiers proves the oldest is
// taken across BOTH decision tiers — a decision parented straight to the plan
// epic (tier-1) and one under a phase sub-epic (tier-2) — off the SAME two-tier
// batched walk.
func TestPlanIndexStepData_OldestDecisionAtSpansBothTiers(t *testing.T) {
	tier2Oldest := time.Date(2026, 6, 15, 8, 0, 0, 0, time.UTC)
	// directOpen=1 (newer), phaseOpen=1 (older, tier-2) — the tier-2 one must win.
	f, plan := seedPhasedPlanWithDecisions("gd-planP", "gd-phaseP", 1, 0, 1, 0)
	// seedPhasedPlanWithDecisions stamps every child CreatedAt zero; set them so
	// the tier-2 decision is the oldest.
	f.beads["gd-planP-decopen0"].CreatedAt = time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)
	f.beads["gd-phaseP-decopen0"].CreatedAt = tier2Oldest

	s := &Server{daemon: f, logger: slog.Default()}
	out := s.planIndexStepData(context.Background(), []*beadsapi.BeadDetail{plan})
	info := out[plan.ID]
	if info == nil {
		t.Fatalf("expected a planIndexStepInfo for %s", plan.ID)
	}
	if info.OpenDecisions != 2 {
		t.Fatalf("OpenDecisions = %d, want 2 (1 tier-1 + 1 tier-2)", info.OpenDecisions)
	}
	if !info.OldestDecisionAt.Equal(tier2Oldest) {
		t.Errorf("OldestDecisionAt = %v, want %v (the tier-2 phase decision is oldest)",
			info.OldestDecisionAt, tier2Oldest)
	}
}

// mkAgeTestServer builds a one-plan fake carrying `open` open decisions, the
// oldest of which was created `oldestAgo` before the pinned clock, plus one
// resolved decision. It returns a handler with the clock pinned so the rendered
// wait-age is deterministic.
func mkAgeTestServer(t *testing.T, oldestAgo time.Duration, open int) http.Handler {
	t.Helper()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeDaemon{
		beads:   map[string]*beadsapi.BeadDetail{},
		revDeps: map[string][]beadsapi.Dependency{},
	}
	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "Age plan", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: now.Add(-time.Hour),
	}
	f.beads[plan.ID] = plan
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	addDec := func(id, status string, created time.Time) {
		f.beads[id] = &beadsapi.BeadDetail{
			ID: id, Title: "Q " + id, Kind: "issue", Type: "decision", Status: status,
			CreatedAt: created, UpdatedAt: now.Add(-time.Hour),
		}
		f.revDeps[plan.ID] = append(f.revDeps[plan.ID],
			beadsapi.Dependency{BeadID: id, DependsOnID: plan.ID, Type: "parent-child"})
	}
	// The oldest open decision waits oldestAgo; any additional open decisions are
	// fresher, so the chip's suffix reflects the oldest wait.
	addDec("gd-dec-oldest", "open", now.Add(-oldestAgo))
	for i := 1; i < open; i++ {
		addDec("gd-dec-fresh", "open", now.Add(-time.Hour))
	}
	addDec("gd-dec-done", "closed", now.Add(-90*24*time.Hour))
	return mkServerWithNow(t, f, func() time.Time { return now })
}

// TestPlansIndex_DecisionChipShowsFreshWaitAge proves a recent open decision
// renders the age suffix but NOT the stale tint — the calm-amber default with a
// dim "· 2d" qualifier.
func TestPlansIndex_DecisionChipShowsFreshWaitAge(t *testing.T) {
	mux := mkAgeTestServer(t, 2*24*time.Hour, 1)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "1 to answer") {
		t.Fatalf("expected the '1 to answer' chip; body missing it")
	}
	// The age suffix rides the chip. (Match the rendered span, not the CSS rule
	// of the same class name in the <style> block.)
	if !strings.Contains(body, `<span class="chip-attention-age">· 2d`) {
		t.Errorf("expected the '· 2d' wait-age suffix span on the decision chip; body missing it")
	}
	// A 2-day-old decision is well under the 14-day staleness threshold — the
	// rendered chip must NOT carry the stale modifier in its class attribute.
	if strings.Contains(body, `class="chip chip-attention chip-attention-stale"`) {
		t.Errorf("a 2-day-old decision must NOT carry the stale tint (threshold is 14d)")
	}
}

// TestPlansIndex_DecisionChipShowsStaleWaitAge proves an aged (>14d) open
// decision gains the .chip-attention-stale class so it pops out of the board.
func TestPlansIndex_DecisionChipShowsStaleWaitAge(t *testing.T) {
	mux := mkAgeTestServer(t, 32*24*time.Hour, 1)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="chip chip-attention chip-attention-stale"`) {
		t.Errorf("a 32-day-old open decision must carry the .chip-attention-stale tint on the rendered chip; body missing it")
	}
	if !strings.Contains(body, `<span class="chip-attention-age">· 32d`) {
		t.Errorf("expected the '· 32d' wait-age suffix; body missing it")
	}
	// The tooltip must name the staleness so the signal is explained, not just colored.
	if !strings.Contains(body, "oldest waiting 32d (stale)") {
		t.Errorf("expected the stale tooltip 'oldest waiting 32d (stale)'; body missing it")
	}
}

// TestPlansIndex_DecisionChipNoAgeWhenStampUnset proves the density-discipline
// fallback: a decision with an unset CreatedAt renders exactly as before (count
// only, no suffix, no tint), so the age never manufactures a phantom stamp.
func TestPlansIndex_DecisionChipNoAgeWhenStampUnset(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeDaemon{
		beads:   map[string]*beadsapi.BeadDetail{},
		revDeps: map[string][]beadsapi.Dependency{},
	}
	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "No-stamp plan", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		UpdatedAt: now.Add(-time.Hour),
	}
	f.beads[plan.ID] = plan
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	// An open decision with a ZERO CreatedAt (legacy row): counted, but no age.
	f.beads["gd-dec0"] = &beadsapi.BeadDetail{
		ID: "gd-dec0", Title: "Q", Kind: "issue", Type: "decision", Status: "open",
		UpdatedAt: now.Add(-time.Hour),
	}
	f.revDeps[plan.ID] = append(f.revDeps[plan.ID],
		beadsapi.Dependency{BeadID: "gd-dec0", DependsOnID: plan.ID, Type: "parent-child"})

	mux := mkServerWithNow(t, f, func() time.Time { return now })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "1 to answer") {
		t.Fatalf("expected the '1 to answer' chip; body missing it")
	}
	if strings.Contains(body, `<span class="chip-attention-age">`) {
		t.Errorf("an unset-CreatedAt decision must render NO age suffix span (count only)")
	}
	if strings.Contains(body, `class="chip chip-attention chip-attention-stale"`) {
		t.Errorf("an unset-CreatedAt decision must NOT be tinted stale")
	}
}
