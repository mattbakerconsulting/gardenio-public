package main

// plans_last_ship_age_test.go — iter-59 (ka-0000005352): the "last ship · Nd"
// progress-recency facet folded onto the /plans index card's progress row. It is
// the delivery-momentum twin of the head-row freshness stamp, and DELIBERATELY a
// different primitive: the freshness stamp (iter-24/38) ages on the newest ANY
// activity (an epic touch, a step edit, a crew heartbeat, a comment), so a plan
// whose crew heartbeat + comments churn reads FRESH even while it has closed no
// step in weeks; this facet ages on the newest step-CLOSE, answering the
// orthogonal "is the plan actually moving forward". Live proof (2026-08-11):
// ka-0000008223 read a calm freshness stamp (newest activity 3.3d, below the 7d
// bar) yet its newest step closed 18.1d prior — a fortnight-plus delivery drought
// no card signal surfaced.
//
// Coverage: the planLastShipAge derivation truth-table (fresh ship / drought /
// boundary / just-under / terminal / no-step-closed / ghost-done / already-
// freshness-stale / deferred), the countIssueChildren lastShip primitive
// (prefers closed_at, falls back to updated_at, ignores open steps), and the
// render fail-without/pass-with + must-not-regress on the served index card.

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The last-ship tests pin the clock at 2026-08-10 12:00 UTC so a step's close
// offset maps deterministically onto the humanized age.
var lastShipTestNow = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// --- unit truth-table: planLastShipAge ---------------------------------------

func TestPlanLastShipAge_TruthTable(t *testing.T) {
	cases := []struct {
		name           string
		status         string
		lastShip       time.Time
		stepsOpen      int
		freshnessStale bool
		wantAge        string
		wantStale      bool
	}{
		// A plan actively shipping — last close 2h ago, open steps left, reading
		// fresh → calm "2h", not stale. The healthy default: the facet is present
		// (context) but quiet.
		{"fresh_ship_2h", "in_progress", lastShipTestNow.Add(-2 * time.Hour), 3, false, "2h", false},
		// The mission case: a plan reading FRESH (freshnessStale=false) that has
		// shipped no step in 18 days, still has open steps → loud "18d", stale.
		{"drought_18d", "in_progress", lastShipTestNow.Add(-18 * 24 * time.Hour), 2, false, "18d", true},
		// Exactly at the 7d boundary → stale (>= threshold), reads "7d".
		{"boundary_7d", "in_progress", lastShipTestNow.Add(-7 * 24 * time.Hour), 1, false, "7d", true},
		// Just under 7d → NOT stale, reads "6d".
		{"just_under_7d", "in_progress", lastShipTestNow.Add(-7*24*time.Hour + time.Minute), 1, false, "6d", false},
		// A TERMINAL (closed) plan's old last-ship is honest history, not a drought
		// → no facet, exactly like the iter-25/38/41 terminal-reconcile family.
		{"terminal_closed", "closed", lastShipTestNow.Add(-18 * 24 * time.Hour), 1, false, "", false},
		// A DEFERRED plan is intentionally parked → no facet (terminal too).
		{"terminal_deferred", "deferred", lastShipTestNow.Add(-18 * 24 * time.Hour), 1, false, "", false},
		// A plan that has closed NO rendered step has no delivery cadence to age →
		// no facet (reads exactly as before).
		{"no_step_closed", "in_progress", time.Time{}, 3, false, "", false},
		// A ghost-done plan (every step closed, stepsOpen == 0) → no facet: its
		// story is the iter-41 "✓ steps done" marker, not a drought. The facet
		// defers to the completion marker rather than showing two markers.
		{"ghost_done_no_open", "in_progress", lastShipTestNow.Add(-30 * 24 * time.Hour), 0, false, "", false},
		// An ALREADY-freshness-stale plan (its freshness stamp is already loud) is
		// all-round-stale; a second loud drought facet would be redundant ink → no
		// facet. The drought facet earns its place only when the plan reads fresh.
		{"already_freshness_stale", "in_progress", lastShipTestNow.Add(-30 * 24 * time.Hour), 2, true, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			age, stale := planLastShipAge(c.status, c.lastShip, c.stepsOpen, c.freshnessStale, lastShipTestNow)
			if age != c.wantAge {
				t.Errorf("age = %q, want %q", age, c.wantAge)
			}
			if stale != c.wantStale {
				t.Errorf("stale = %v, want %v", stale, c.wantStale)
			}
		})
	}
}

// --- unit: countIssueChildren lastShip primitive ------------------------------

// TestCountIssueChildren_LastShipPrefersClosedAt proves the lastShip return is the
// newest step-CLOSE instant, preferring the immutable closed_at stamp and folding
// only CLOSED steps — an open step (however recently touched) never advances it.
func TestCountIssueChildren_LastShipPrefersClosedAt(t *testing.T) {
	beads := map[string]*beadsapi.BeadDetail{
		// Closed with an explicit closed_at (the newest close) — must win.
		"gd-a": {ID: "gd-a", Type: "task", Status: "closed",
			ClosedAt:  "2026-08-05T10:00:00Z",
			UpdatedAt: time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)},
		// Closed with an OLDER closed_at.
		"gd-b": {ID: "gd-b", Type: "task", Status: "closed",
			ClosedAt:  "2026-07-01T10:00:00Z",
			UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)},
		// An OPEN step touched very recently — must NOT advance lastShip (it never
		// shipped), even though its updated_at is the newest of the set.
		"gd-c": {ID: "gd-c", Type: "task", Status: "open",
			UpdatedAt: time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)},
	}
	total, closed, lastActivity, lastShip := countIssueChildren([]string{"gd-a", "gd-b", "gd-c"}, beads, nil)
	if total != 3 || closed != 2 {
		t.Fatalf("total/closed = %d/%d, want 3/2", total, closed)
	}
	wantShip := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	if !lastShip.Equal(wantShip) {
		t.Errorf("lastShip = %v, want %v (newest closed_at, open step excluded)", lastShip, wantShip)
	}
	// lastActivity still tracks the newest updated_at of ANY step — the open gd-c —
	// the exact primitive the freshness stamp uses, so the two diverge here.
	wantAct := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	if !lastActivity.Equal(wantAct) {
		t.Errorf("lastActivity = %v, want %v (newest updated_at incl. the open step)", lastActivity, wantAct)
	}
}

// TestCountIssueChildren_LastShipFallsBackToUpdatedAt proves a closed step with no
// (or unparseable) closed_at still contributes its close instant via the
// updated_at fallback — a closed step never re-edited has its close IN its
// updated_at.
func TestCountIssueChildren_LastShipFallsBackToUpdatedAt(t *testing.T) {
	beads := map[string]*beadsapi.BeadDetail{
		"gd-a": {ID: "gd-a", Type: "task", Status: "closed",
			UpdatedAt: time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)}, // no ClosedAt
	}
	_, closed, _, lastShip := countIssueChildren([]string{"gd-a"}, beads, nil)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}
	want := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	if !lastShip.Equal(want) {
		t.Errorf("lastShip = %v, want %v (updated_at fallback for a missing closed_at)", lastShip, want)
	}
}

// --- render: the facet on the served /plans index -----------------------------

// seedFreshButDroughtPlan builds a plan that reads FRESH on the head-row freshness
// stamp (its epic + an open step are recently touched, below the 7d bar) yet has
// shipped no step in ~18 days (its one closed step's closed_at is old). This is
// the exact live shape ka-0000008223 had: a delivery drought hidden behind fresh
// activity. It has an open step so stepsOpen > 0 (not ghost-done).
func seedFreshButDroughtPlan() *fakeDaemon {
	f := newFake()
	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "Fresh but not shipping", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		// Recent epic touch → freshness stamp reads calm (well under 7d).
		UpdatedAt: lastShipTestNow.Add(-2 * time.Hour),
	}
	// One CLOSED step shipped 18 days ago — the drought the facet surfaces.
	shipped := &beadsapi.BeadDetail{
		ID: "gd-step1", Title: "Shipped long ago", Kind: "issue", Type: "task",
		Status:    "closed",
		ClosedAt:  lastShipTestNow.Add(-18 * 24 * time.Hour).UTC().Format(time.RFC3339),
		UpdatedAt: lastShipTestNow.Add(-18 * 24 * time.Hour),
	}
	// One OPEN step touched recently — keeps lastActivity fresh (freshness calm)
	// AND keeps stepsOpen > 0 (not ghost-done). Its recent touch does NOT advance
	// lastShip because it is not closed.
	open := &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Still open, recently touched", Kind: "issue", Type: "task",
		Status:    "open",
		UpdatedAt: lastShipTestNow.Add(-1 * time.Hour),
	}
	f.beads["gd-plan1"] = plan
	f.beads["gd-step1"] = shipped
	f.beads["gd-step2"] = open
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-plan1": {
			{BeadID: "gd-step1", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"},
		},
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	f.epicSubtreeCount = map[string]int{"gd-plan1": 2, "gd-plan1|closed": 1}
	return f
}

func lastShipPlanCardHTML(t *testing.T, f *fakeDaemon) string {
	t.Helper()
	mux := mkServerWithNow(t, f, func() time.Time { return lastShipTestNow })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans", "matt@acme.example"))
	if w.Code != 200 {
		t.Fatalf("GET /plans: expected 200, got %d", w.Code)
	}
	re := regexp.MustCompile(`(?s)<li class="plan-card".*?</li>`)
	m := re.FindString(w.Body.String())
	if m == "" {
		t.Fatalf("no plan-card in body:\n%s", w.Body.String())
	}
	return m
}

// TestPlansIndex_LastShipRendersLoudForDrought proves the served card for a
// fresh-but-not-shipping plan renders the loud "· last ship 18d" facet with its
// stale class — the pass-with half. Fail-without: dropping the row wiring or the
// template gate reds this.
func TestPlansIndex_LastShipRendersLoudForDrought(t *testing.T) {
	card := lastShipPlanCardHTML(t, seedFreshButDroughtPlan())
	if !strings.Contains(card, "· last ship 18d") {
		t.Fatalf("drought card missing '· last ship 18d' facet:\n%s", card)
	}
	if !strings.Contains(card, "plan-progress-lastship-stale") {
		t.Errorf("drought facet missing its loud .plan-progress-lastship-stale class:\n%s", card)
	}
	// The head-row freshness stamp must read CALM (not stale) on the SAME card —
	// that is the whole point: the plan looks fresh but the drought facet tells the
	// truth. A loud freshness stamp here would mean the seed is not the mission case.
	if strings.Contains(card, "plan-card-updated-stale") {
		t.Errorf("freshness stamp is stale on the drought card — the seed is not the fresh-but-not-shipping case:\n%s", card)
	}
}

// TestPlansIndex_LastShipCalmWhenShippingRecently proves a plan that shipped a
// step recently renders a CALM facet (present as context, no loud class).
func TestPlansIndex_LastShipCalmWhenShippingRecently(t *testing.T) {
	f := seedFreshButDroughtPlan()
	// Re-ship the closed step 3 hours ago → calm facet.
	f.beads["gd-step1"].ClosedAt = lastShipTestNow.Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	f.beads["gd-step1"].UpdatedAt = lastShipTestNow.Add(-3 * time.Hour)
	card := lastShipPlanCardHTML(t, f)
	if !strings.Contains(card, "· last ship 3h") {
		t.Fatalf("recently-shipping card missing calm '· last ship 3h' facet:\n%s", card)
	}
	if strings.Contains(card, "plan-progress-lastship-stale") {
		t.Errorf("recently-shipping facet must NOT carry the loud stale class:\n%s", card)
	}
}

// TestPlansIndex_LastShipAbsentForGhostDone proves a plan whose every rendered step
// is closed (ghost-done) renders NO last-ship facet — its story is the iter-41
// "✓ steps done" marker, not a drought.
func TestPlansIndex_LastShipAbsentForGhostDone(t *testing.T) {
	f := seedFreshButDroughtPlan()
	// Close the open step too → stepsOpen == 0 (ghost-done). Keep its close old so a
	// bug that ignored the ghost-done gate would render a loud drought facet.
	f.beads["gd-step2"].Status = "closed"
	f.beads["gd-step2"].ClosedAt = lastShipTestNow.Add(-18 * 24 * time.Hour).UTC().Format(time.RFC3339)
	f.beads["gd-step2"].UpdatedAt = lastShipTestNow.Add(-18 * 24 * time.Hour)
	f.epicSubtreeCount = map[string]int{"gd-plan1": 2, "gd-plan1|closed": 2}
	card := lastShipPlanCardHTML(t, f)
	if strings.Contains(card, "last ship") {
		t.Errorf("ghost-done card must render NO last-ship facet (the ✓ steps done marker tells its story):\n%s", card)
	}
	// It SHOULD carry the ghost-done marker instead (belt-and-suspenders that the
	// seed really is ghost-done and the two markers are mutually exclusive).
	if !strings.Contains(card, "steps done") {
		t.Errorf("ghost-done card expected the '✓ steps done' marker:\n%s", card)
	}
}

// TestPlansIndex_LastShipAbsentWhenNoStepClosed proves a plan that has closed NO
// step renders no last-ship facet (undatable reads exactly as before).
func TestPlansIndex_LastShipAbsentWhenNoStepClosed(t *testing.T) {
	f := seedFreshButDroughtPlan()
	// Reopen the shipped step → nothing has ever closed.
	f.beads["gd-step1"].Status = "open"
	f.beads["gd-step1"].ClosedAt = ""
	f.epicSubtreeCount = map[string]int{"gd-plan1": 2, "gd-plan1|closed": 0}
	card := lastShipPlanCardHTML(t, f)
	if strings.Contains(card, "last ship") {
		t.Errorf("no-step-closed card must render NO last-ship facet:\n%s", card)
	}
}
