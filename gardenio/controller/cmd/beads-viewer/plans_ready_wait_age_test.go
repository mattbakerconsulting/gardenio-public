package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// plans_ready_wait_age_test.go — iter-47 (ka-0000005352): the READY authoring-
// lifecycle chip folds the awaiting-approval wait-age onto its face ("ready · 4d")
// and tints loud past planReadyStaleThreshold (7d).
//
// The READY chip is the #3 attention tier — planAttention ranks a `ready` plan at
// planAttentionReady (ABOVE planAttentionStalled, plans_attention.go): a finalized
// plan sitting behind a single deliberate operator tap ("Approve & build"), where
// one tap unblocks the whole plan. Yet it was the LAST attention-relevant card
// signal carrying no wait-age, while every sibling already surfaces HOW LONG
// (decision "· 32d" iter-4, escalation "· 40d" iter-8, stall "· 3d" iter-11,
// queued/stuck crew "· 12m"/"· 6h" iter-32/13, open-MR "· 8d" iter-40, now/next
// step "· 5d" iter-43). Live proof (2026-08-09): P0 ka-0000000093 awaited approval
// ~4d, ka-0000003709 3+ weeks.
//
// The age is derived off the epic's OWN updated_at — a DIFFERENT primitive from the
// folded lastActivityAt the freshness stamp (iter-38) ages on — so it is a CTA age
// ("awaiting your approval since"), never redundant with the freshness tint.

// --- unit truth-table: planReadyWaitAge --------------------------------------

// TestPlanReadyWaitAge is the pure crux: the wait-age + stale flag render iff the
// reconciled chip is READY and the stamp is datable. A draft/building/complete
// chip carries no approval-wait; an undatable ready plan reads chip-word-only
// (exactly as before); a >7d wait flips stale.
func TestPlanReadyWaitAge(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	fourDaysAgo := now.Add(-4 * 24 * time.Hour)
	eightDaysAgo := now.Add(-8 * 24 * time.Hour)

	cases := []struct {
		name      string
		chip      planState
		updatedAt time.Time
		wantAge   bool // true = a non-empty humanized age is returned
		wantStale bool
	}{
		// READY + datable, below the 7d bar → age, calm.
		{"ready 4d calm", planStateReady, fourDaysAgo, true, false},
		// READY + datable, past the 7d bar → age, loud.
		{"ready 8d stale", planStateReady, eightDaysAgo, true, true},
		// READY at exactly the 7d boundary → stale (>=, mirrors the sibling bars).
		{"ready 7d boundary stale", planStateReady, now.Add(-planReadyStaleThreshold), true, true},
		// READY but undatable → chip word only, no age, never stale.
		{"ready undatable", planStateReady, time.Time{}, false, false},
		// Non-READY chips carry no approval-wait, whatever their age.
		{"draft has no wait", planStateDraft, eightDaysAgo, false, false},
		{"building has no wait", planStateBuilding, eightDaysAgo, false, false},
		{"complete has no wait", planStateComplete, eightDaysAgo, false, false},
		{"unset has no wait", planStateUnset, eightDaysAgo, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			age, stale := planReadyWaitAge(c.chip, c.updatedAt, now)
			if (age != "") != c.wantAge {
				t.Errorf("planReadyWaitAge age = %q, wantNonEmpty=%v", age, c.wantAge)
			}
			if stale != c.wantStale {
				t.Errorf("planReadyWaitAge stale = %v, want %v (age=%q)", stale, c.wantStale, age)
			}
			// An empty age must never be stale — the two facets can't disagree.
			if age == "" && stale {
				t.Errorf("planReadyWaitAge returned stale=true with an empty age")
			}
		})
	}
}

// TestPlanReadyWaitAge_ReconciledChipGatesIt proves the derivation keys off the
// RECONCILED LifecycleChip, not the raw plan_state: a terminal plan whose stale
// plan_state still reads "ready" carries NO wait-age, because planLifecycleChipState
// already suppressed the chip to unset (iter-27) — so the row-build passes
// planState(row.LifecycleChip)=="" here, not planStateReady. This is why the
// row-build derives off row.LifecycleChip (the reconciled value) rather than
// planStateOf(b): the age can never ride a chip that isn't rendered.
func TestPlanReadyWaitAge_ReconciledChipGatesIt(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	// A closed plan with a stale plan_state=ready reconciles to an EMPTY chip.
	reconciled := planLifecycleChipState(planStateReady, "closed")
	if reconciled != planStateUnset {
		t.Fatalf("precondition: closed+ready must reconcile to unset, got %q", reconciled)
	}
	age, stale := planReadyWaitAge(reconciled, now.Add(-30*24*time.Hour), now)
	if age != "" || stale {
		t.Errorf("a terminal plan (reconciled chip unset) must carry no ready wait-age; got age=%q stale=%v", age, stale)
	}
}

// --- HTTP render: fail-without / pass-with -----------------------------------

// seedReadyPlan builds a single READY plan whose epic updated_at is `age` before
// `now` — the ready-transition write the wait-age measures. No steps/decisions, so
// the card is the bare lifecycle chip + status. Returns the fake + the pinned now.
func seedReadyPlan(age time.Duration, now time.Time) *fakeDaemon {
	f := newFake()
	plan := &beadsapi.BeadDetail{
		ID:        "gd-ready1",
		Title:     "Plan: drain the FSM backlog",
		Kind:      "issue",
		Type:      "epic",
		Status:    "open",
		Labels:    []string{"project:gardenio", "plan"},
		Fields:    map[string]string{"plan_state": "ready"},
		UpdatedAt: now.Add(-age),
	}
	f.beads["gd-ready1"] = plan
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	f.epicSubtreeCount = map[string]int{"gd-ready1": 0, "gd-ready1|closed": 0}
	return f
}

// TestPlansIndex_ReadyChipShowsWaitAge is the render regression: a READY plan whose
// epic last moved 4 days ago renders the "· 4d" wait-age on its lifecycle chip (calm,
// below the 7d bar). Reverting the template seam (dropping the .ReadyWaitAge span)
// reds this.
func TestPlansIndex_ReadyChipShowsWaitAge(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	f := seedReadyPlan(4*24*time.Hour, now)
	mux := mkServerWithNow(t, f, func() time.Time { return now })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The ready chip renders.
	if !strings.Contains(body, "plan-lifecycle-chip-ready") {
		t.Fatalf("ready plan must render the ready lifecycle chip")
	}
	// The wait-age span rides the chip face.
	if !strings.Contains(body, `class="plan-lifecycle-chip-age"`) {
		t.Errorf("ready chip must carry the .plan-lifecycle-chip-age wait-age span")
	}
	if !strings.Contains(body, "· 4d") {
		t.Errorf("ready chip must render the '· 4d' wait-age face; body missing it")
	}
	// Calm (below the 7d bar) → NOT the loud stale tint ON THE ELEMENT. Key on the
	// quote-terminated class (as rendered in the class attr) — the bare class also
	// appears in the always-shipped <style> block, so a bare-substring check would
	// false-match the CSS rule.
	if strings.Contains(body, `plan-lifecycle-chip-ready-stale"`) {
		t.Errorf("a 4d ready wait is below the 7d bar and must NOT tint the chip stale")
	}
}

// TestPlansIndex_ReadyChipStaleTint is the loud-tier regression: a READY plan awaiting
// approval 8 days (past the 7d bar) gains .plan-lifecycle-chip-ready-stale so it pops.
func TestPlansIndex_ReadyChipStaleTint(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	f := seedReadyPlan(8*24*time.Hour, now)
	mux := mkServerWithNow(t, f, func() time.Time { return now })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Key on the quote-terminated class so this asserts the tint is on the ELEMENT,
	// not merely present as the always-shipped <style> rule.
	if !strings.Contains(body, `plan-lifecycle-chip-ready-stale"`) {
		t.Errorf("an 8d ready wait is past the 7d bar and MUST tint the chip stale")
	}
	if !strings.Contains(body, "· 8d") {
		t.Errorf("stale ready chip must still render its '· 8d' wait-age face")
	}
}

// TestPlansIndex_ReadyChipStaleCSS proves both skins carry the loud stale rule — the
// iter-16 dark-legibility invariant: a NEW light-only card chip would render
// near-invisible on the dark skin (which no PACES capture catches). Keyed on the
// selector so a refactor dropping either skin reds.
func TestPlansIndex_ReadyChipStaleCSS(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, ".plan-lifecycle-chip-ready-stale {") {
		t.Errorf("/plans is missing the light-skin ready-stale tint rule")
	}
	if !strings.Contains(body, `html[data-skin^="dark"] .plan-lifecycle-chip-ready-stale {`) {
		t.Errorf("/plans is missing the DARK-skin ready-stale override — the tint would be illegible on the dark skin")
	}
	// The age span must opt out of the chip's uppercase transform so "· 4d" doesn't
	// render "· 4D".
	if !strings.Contains(body, ".plan-lifecycle-chip-age { text-transform: none;") {
		t.Errorf("/plans is missing the .plan-lifecycle-chip-age lowercase rule")
	}
	// a11y (ka-0000007487): the age span must run at FULL opacity under the dark
	// skin. The light-skin rule dims it to 0.85, which over the dark ready chip
	// composites #58a6ff down to #4f94e3 on the #1f2f41 backdrop = 4.33:1 —
	// under the WCAG AA 4.5:1 floor for this 11.5px text. That reds the
	// dark-/plans axe spec in tests/beads-viewer-e2e/tests/a11y.spec.js, and
	// because it ships on main it blocks EVERY MR branched off it. Full opacity
	// is 5.39:1. Keep this override or restore the contrast another way.
	if !strings.Contains(body, `html[data-skin^="dark"] .plan-lifecycle-chip-age { opacity: 1; }`) {
		t.Errorf("/plans is missing the dark-skin full-opacity override for .plan-lifecycle-chip-age — at 0.85 the age text is 4.33:1 on the dark chip, below the WCAG AA 4.5:1 floor (fails the dark /plans a11y spec)")
	}
}

// --- non-regression: non-ready chips + freshness stamp untouched -------------

// TestPlansIndex_NonReadyChipNoWaitAge is the anti-over-reach guard: a BUILDING plan
// (the default seedPlansFake state is in_progress with no plan_state; seed building)
// carries no wait-age span even though its epic is old — the age rides ONLY the ready
// arm.
func TestPlansIndex_NonReadyChipNoWaitAge(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	f := newFake()
	plan := &beadsapi.BeadDetail{
		ID:        "gd-build1",
		Title:     "Plan: building away",
		Kind:      "issue",
		Type:      "epic",
		Status:    "in_progress",
		Labels:    []string{"project:gardenio", "plan"},
		Fields:    map[string]string{"plan_state": "building"},
		UpdatedAt: now.Add(-30 * 24 * time.Hour), // ancient, but not a ready plan
	}
	f.beads["gd-build1"] = plan
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	f.epicSubtreeCount = map[string]int{"gd-build1": 0, "gd-build1|closed": 0}
	mux := mkServerWithNow(t, f, func() time.Time { return now })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&status=all", "matt@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-lifecycle-chip-building") {
		t.Fatalf("precondition: the building chip must render")
	}
	if strings.Contains(body, `class="plan-lifecycle-chip-age"`) {
		t.Errorf("a building plan must carry NO wait-age span — the age rides only the ready arm")
	}
	// Quote-terminated so this checks the ELEMENT, not the always-shipped CSS rule.
	if strings.Contains(body, `plan-lifecycle-chip-ready-stale"`) {
		t.Errorf("a non-ready plan must never carry the ready-stale tint on the chip")
	}
}
