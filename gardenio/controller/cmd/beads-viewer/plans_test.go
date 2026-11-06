package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// seedPlansFake builds a fake serving one plan epic (gd-plan1) on the /plans
// index (via the Types=[epic] hotEpicsResp route), plus a step child, a
// clarifying-question decision, and the subtree count fixtures.
func seedPlansFake() *fakeDaemon {
	f := newFake()
	plan := &beadsapi.BeadDetail{
		ID:          "gd-plan1",
		Title:       "Plan: ship the /plans page",
		Kind:        "issue",
		Type:        "epic",
		Status:      "in_progress",
		Labels:      []string{"project:gardenio", "plan"},
		Description: "Research the viewer, build the cockpit.",
		UpdatedAt:   time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
	step := &beadsapi.BeadDetail{
		ID:     "gd-step1",
		Title:  "Wire the route",
		Kind:   "issue",
		Type:   "task",
		Status: "open",
		Labels: []string{"project:gardenio"},
	}
	// The clarifying-question decision is a parent-child child of the plan epic
	// (the real shape) so the index's batched child walk counts it as one open
	// decision (Phase 2, ka-0000004522) without a per-row query. decisionResp
	// below still serves the detail page's Types=[decision] Parent list.
	dec := &beadsapi.BeadDetail{
		ID:        "gd-dec1",
		Title:     "Which identity marker?",
		Type:      "decision",
		Status:    "open",
		Fields:    map[string]string{"prompt": "Label the epic 'plan' or use a title prefix?"},
		UpdatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.beads["gd-plan1"] = plan
	f.beads["gd-step1"] = step
	f.beads["gd-dec1"] = dec
	// The step AND the decision are parent-child children of the plan (reverse
	// deps point AT the epic). The index counts the open decision off this same
	// batched walk; step collectors filter to issue types, so the decision never
	// leaks into the step rollup or grid.
	if f.revDeps == nil {
		f.revDeps = map[string][]beadsapi.Dependency{}
	}
	f.revDeps["gd-plan1"] = []beadsapi.Dependency{
		{BeadID: "gd-step1", DependsOnID: "gd-plan1", Type: "parent-child"},
		{BeadID: "gd-dec1", DependsOnID: "gd-plan1", Type: "parent-child"},
	}
	// Index route: Types=[epic] → hotEpicsResp.
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	// Subtree rollup counts (deriveOwnedEpicRollup): 1 total, 0 closed.
	f.epicSubtreeCount = map[string]int{"gd-plan1": 1, "gd-plan1|closed": 0}
	// Clarifying-question decision scoped to the plan (Types=[decision] → decisionResp).
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{dec},
		Total: 1,
	}
	return f
}

func TestHandlePlans_IndexRendersPlanAndNav(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Plan: ship the /plans page", // the plan title
		"/plans/gd-plan1",            // link to detail
		"0/1 steps",                  // rollup
		"1 to answer",                // open-decision chip
		`href="/plans"`,              // nav link present in layout
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index body missing %q", want)
		}
	}
}

// TestHandlePlansIndex_RendersTaskGrid proves the /plans index card renders the
// mini task-grid (Matt's ask, ka-0000001226): a per-task square that is an <a>
// to the task bead (so the shared preview_popover.js gives hover-preview +
// click-through, and board-mode click-through opens the task not the peek
// drawer), tagged with data-status for the color mapping. The seed plan has one
// open step (gd-step1).
func TestHandlePlansIndex_RendersTaskGrid(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The grid container renders...
	if !strings.Contains(body, `class="plan-taskgrid"`) {
		t.Errorf("index card missing the task-grid container")
	}
	// ...and the one open step is a square linking to its bead, carrying its
	// status so the CSS can color it. Assert the anchor href + data-status so a
	// regression that drops the click-through (the hover-preview + drill-down
	// contract) trips the test.
	if !strings.Contains(body, `class="plan-taskgrid-sq" data-status="open"`) {
		t.Errorf("index card task square missing data-status color hook, body:\n%s", body)
	}
	if !strings.Contains(body, `href="/gd-step1"`) {
		t.Errorf("index card task square must link to the task bead for hover-preview + click-through")
	}
}

// TestHandlePlansIndex_RendersLifecycleChip proves the /plans index card surfaces
// the plan's authoring-lifecycle state (ka-0000000851) — the SAME draft/ready/
// building chip the /plans/{id} cockpit renders, already computed on the row
// (planStateOf) but previously dropped on the index. The seed epic carries
// plan_state=building.
func TestHandlePlansIndex_RendersLifecycleChip(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Fields = map[string]string{"plan_state": "building"}
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Assert on the rendered chip markup (the shared chip CSS is always in the
	// page <style>, so a bare class-name match would pass even without the chip).
	if !strings.Contains(body, `class="chip plan-lifecycle-chip plan-lifecycle-chip-building"`) {
		t.Errorf("index card missing the building lifecycle chip, body:\n%s", body)
	}
}

// TestHandlePlansIndex_NoLifecycleChipWhenUnset proves a legacy/hand-marked plan
// that never opted into the lifecycle (no plan_state) shows no lifecycle chip on
// the index — matching the detail card's gate. The seed epic has no plan_state.
func TestHandlePlansIndex_NoLifecycleChipWhenUnset(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Assert on the rendered chip markup, not the bare class name — the latter
	// also appears in the page's <style> block (the shared chip rules).
	if strings.Contains(w.Body.String(), `class="chip plan-lifecycle-chip`) {
		t.Errorf("index card must not render a lifecycle chip for an unset plan_state")
	}
}

// TestHandlePlansIndex_RendersStepBreakdown proves the /plans index card surfaces
// the live in_progress/blocked step breakdown beside the progress bar
// (ka-0000000851), derived from the SAME batched grid — more signal, same
// footprint. The seed gets a second step (in_progress) and a third (blocked) so
// the tally reads 1 moving + 1 wedged.
func TestHandlePlansIndex_RendersStepBreakdown(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "In-flight step", Kind: "issue", Type: "task",
		Status: "in_progress", CreatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.beads["gd-step3"] = &beadsapi.BeadDetail{
		ID: "gd-step3", Title: "Wedged step", Kind: "issue", Type: "task",
		Status: "blocked", CreatedAt: time.Date(2026, 7, 1, 9, 30, 0, 0, time.UTC),
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-step3", DependsOnID: "gd-plan1", Type: "parent-child"})
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="plan-step-tally"`) {
		t.Errorf("index card missing the step-tally container, body:\n%s", body)
	}
	// One of each: the rendered tally chips carry the counts.
	if !strings.Contains(body, "▶ 1") {
		t.Errorf("index card in-progress tally should read 1")
	}
	if !strings.Contains(body, "⛔ 1") {
		t.Errorf("index card blocked tally should read 1")
	}
}

// TestHandlePlansIndex_NoStepBreakdownWhenNoneActive proves a plan with nothing
// in flight and nothing wedged renders no breakdown chips (they gate on non-zero)
// — the density adds no ink when there is no signal. The seed's lone step is open.
func TestHandlePlansIndex_NoStepBreakdownWhenNoneActive(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Assert on the rendered tally container markup, not the bare class name —
	// the latter also appears in the page's <style> block.
	if strings.Contains(w.Body.String(), `class="plan-step-tally"`) {
		t.Errorf("index card must not render the step tally when no step is in progress or blocked")
	}
}

// TestHandlePlansIndex_RendersSegmentedProgressBar proves the /plans index card
// paints the four lifecycle segments (ka-0000007226) — done · in-progress ·
// blocked · not-started — in the progress bar's own footprint, each sized by its
// step count (flex-grow), instead of the old single green fill that only
// re-stated PctDone. The seed's lone step (gd-step1) is open (→ not-started); we
// add one closed, one in_progress, and one blocked so all four segments render,
// and assert each carries its count as the flex-grow so the widths are drift-free
// and partition the track.
func TestHandlePlansIndex_RendersSegmentedProgressBar(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Done step", Kind: "issue", Type: "task",
		Status: "closed", CreatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.beads["gd-step3"] = &beadsapi.BeadDetail{
		ID: "gd-step3", Title: "In-flight step", Kind: "issue", Type: "task",
		Status: "in_progress", CreatedAt: time.Date(2026, 7, 1, 9, 15, 0, 0, time.UTC),
	}
	f.beads["gd-step4"] = &beadsapi.BeadDetail{
		ID: "gd-step4", Title: "Wedged step", Kind: "issue", Type: "task",
		Status: "blocked", CreatedAt: time.Date(2026, 7, 1, 9, 30, 0, 0, time.UTC),
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-step3", DependsOnID: "gd-plan1", Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-step4", DependsOnID: "gd-plan1", Type: "parent-child"})
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The old monochrome fill is gone: no bare <span style="width: N%"> inside the
	// bar. The bar is now a flex row of segments.
	if strings.Contains(body, `<div class="plan-progress-bar"><span style="width:`) {
		t.Errorf("index card still renders the old monochrome progress fill, body:\n%s", body)
	}
	// Each of the four lifecycle segments renders, sized by its step count via
	// flex-grow. 4 steps total: 1 done, 1 in_progress, 1 blocked, 1 not-started
	// (the seed's open gd-step1). Assert the segment markup + its count-as-grow so
	// a future change that drops a bucket or reverts to a percent width fails.
	for _, want := range []string{
		`<span class="plan-progress-seg plan-progress-seg-done" style="flex-grow: 1"`,
		`<span class="plan-progress-seg plan-progress-seg-inprogress" style="flex-grow: 1"`,
		`<span class="plan-progress-seg plan-progress-seg-blocked" style="flex-grow: 1"`,
		`<span class="plan-progress-seg plan-progress-seg-todo" style="flex-grow: 1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index card missing segment %q, body:\n%s", want, body)
		}
	}
	// The accessible label carries the full breakdown for assistive tech.
	if !strings.Contains(body, `aria-label="1 done, 1 in progress, 1 blocked, 1 not started of 4 steps"`) {
		t.Errorf("segmented bar missing the breakdown aria-label, body:\n%s", body)
	}
	// The numeric label beside the bar is unchanged (closed/total · pct).
	if !strings.Contains(body, `1/4 steps · 25%`) {
		t.Errorf("progress label should still read 1/4 steps · 25%%, body:\n%s", body)
	}
}

// TestHandlePlansIndex_ProgressBarAllDoneSingleSegment proves a fully-closed
// plan renders exactly one segment — the green done bar filling the track — and
// no not-started/in-progress/blocked segments (they gate on non-zero counts), so
// a finished plan reads as a solid green bar with no gray remainder.
func TestHandlePlansIndex_ProgressBarAllDoneSingleSegment(t *testing.T) {
	f := seedPlansFake()
	// Flip the seed's lone step closed so total=1, closed=1.
	f.beads["gd-step1"].Status = "closed"
	f.epicSubtreeCount = map[string]int{"gd-plan1": 1, "gd-plan1|closed": 1}
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<span class="plan-progress-seg plan-progress-seg-done" style="flex-grow: 1"`) {
		t.Errorf("all-done plan missing the done segment, body:\n%s", body)
	}
	// No other segment renders (each gates on a non-zero count).
	for _, unwanted := range []string{
		`plan-progress-seg-inprogress`, `plan-progress-seg-blocked`, `plan-progress-seg-todo`,
	} {
		// Guard against the CSS block (in <style>) matching: only the rendered
		// <span> carries the flex-grow inline style.
		if strings.Contains(body, unwanted+`" style="flex-grow:`) {
			t.Errorf("all-done plan should not render a %s segment, body:\n%s", unwanted, body)
		}
	}
}

// TestHandlePlansIndex_RendersPriorityPill proves the /plans index card surfaces
// the plan epic's priority (ka-0000007970) — already carried on the row
// (planRowView.Priority, off the batched list query, no extra daemon call) and
// rendered by the /deck face, but previously dropped on the index. A non-baseline
// priority (here P0) renders the color-tiered pill so an operator scanning the
// board sees urgency at a glance.
func TestHandlePlansIndex_RendersPriorityPill(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Priority = 0
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Assert on the rendered pill markup (class + label), not the bare class name —
	// the tier CSS lives in the page <style> so a class-only match would pass even
	// without the pill.
	if !strings.Contains(body, `class="plan-prio-chip plan-prio-chip-0"`) {
		t.Errorf("index card missing the P0 priority pill, body:\n%s", body)
	}
	if !strings.Contains(body, ">P0</span>") {
		t.Errorf("index card priority pill should render the P0 label")
	}
}

// TestHandlePlansIndex_NoPriorityPillAtBaseline proves a plan at the P2 default
// (the kd create baseline) renders NO priority pill — the density adds no ink
// when the priority carries no signal, exactly like the step-tally/crew gates.
func TestHandlePlansIndex_NoPriorityPillAtBaseline(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Priority = 2
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Assert on the rendered pill markup, not the bare class name — the tier CSS
	// rules always appear in the page's <style> block.
	if strings.Contains(w.Body.String(), `class="plan-prio-chip`) {
		t.Errorf("index card must not render a priority pill at the P2 baseline")
	}
}

func TestHandlePlanDetail_RendersCockpit(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Plan: ship the /plans page",                // title
		"Wire the route",                            // the step
		"Clarifying questions",                      // decisions section
		"Label the epic",                            // the decision prompt
		"Plan narrative",                            // prose block
		"Research the viewer",                       // epic description rendered as prose
		"Build steps",                               // the step timeline header
		"plan-timeline",                             // the timeline list (replaces the constellation)
		`data-plan-events="/plans/gd-plan1/events"`, // live-progress SSE anchor
		"plan_events.js",                            // the live-progress client
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q", want)
		}
	}
	// The constellation is gone from the plan cockpit (ask ka-0000001134). The
	// "Plan graph" reveal summary and the rendered constellation reveal wrapper
	// were the plan-specific markers; layout.html still ships constellation CSS
	// globally, so assert on the plan-page markers, not the bare class name.
	if strings.Contains(body, "Plan graph") || strings.Contains(body, `class="constellation-reveal`) {
		t.Error("plan cockpit should no longer render the constellation graph")
	}
}

// TestHandlePlansIndex_ShowsLastChange proves the /plans index card renders a
// labelled "updated <ago>" stamp driven by the plan's LATEST activity — not the
// epic's own rarely-moving updated_at (ka-0000002873). The seed epic's own
// updated_at is Jul 1 10:00; a step touched Jul 8 09:30 is newer, so the card's
// stamp must reflect the step (its RFC instant in the tooltip), proving the fold
// looks past the epic's own timestamp.
func TestHandlePlansIndex_ShowsLastChange(t *testing.T) {
	f := seedPlansFake()
	// The step is the newest thing in the plan — later than the epic's own
	// updated_at (Jul 1). This is the "created stamp is stale" case the fix
	// targets: real activity lives in the child step, not the epic bead.
	f.beads["gd-step1"].UpdatedAt = time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC)

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "updated ") {
		t.Errorf("index card missing the labelled 'updated <ago>' stamp")
	}
	// The precise tooltip must carry the STEP's instant (Jul 8), proving the fold
	// used the newest child activity and not the epic's own updated_at (Jul 1).
	// Match the tooltip PREFIX (no closing quote): an ACTIVE plan a month stale
	// now appends the iter-38 freshness-stale suffix to this same title, so the
	// closing quote no longer follows the instant directly.
	if !strings.Contains(body, `title="Last change 2026-07-08T09:30:00Z`) {
		t.Errorf("index card last-change tooltip should reflect the step's Jul 8 instant, body:\n%s", body)
	}
	if strings.Contains(body, "2026-07-01T10:00:00Z") {
		t.Errorf("index card must not stamp the epic's own (older) updated_at as last-change")
	}
}

// TestHandlePlanDetail_ShowsLastChange proves the /plans/{id} cockpit header now
// renders a "updated <ago>" freshness stamp (ka-0000002873) whose precise
// tooltip reflects the newest activity across the plan — here a step touched
// Jul 8, newer than the epic's own Jul 1 updated_at.
func TestHandlePlanDetail_ShowsLastChange(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].UpdatedAt = time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC)

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-header-updated") {
		t.Errorf("detail header missing the last-change stamp element")
	}
	if !strings.Contains(body, "updated ") {
		t.Errorf("detail header missing the labelled 'updated <ago>' text")
	}
	if !strings.Contains(body, `title="Last change 2026-07-08T09:30:00Z"`) {
		t.Errorf("detail last-change tooltip should reflect the step's Jul 8 instant, body:\n%s", body)
	}
}

// TestHandlePlansIndex_LastChangeFoldsCrewHeartbeat proves the /plans index
// card's "last change" stamp folds the plan's LIVE-CREW heartbeats, not just its
// steps/phases (ka-0000004310, iter-24). A working crew's updated_at moves every
// cycle while the STEP task bead it works sits static until a status change — so
// without this fold a card rendered a pulsing green "● N crew · M working" live
// chip BESIDE "updated 1 month ago" (a card-vs-truth false-freshness, the temporal
// twin of the iter-19/23 false-stall). Here the epic + step both last moved Jul 1;
// a crew heartbeating Jul 8 is the newest activity, so the stamp must reflect the
// CREW's instant. This test FAILS before the fold (the stamp read the Jul 1 step).
func TestHandlePlansIndex_LastChangeFoldsCrewHeartbeat(t *testing.T) {
	f := seedPlansFake()
	// The epic (Jul 1 10:00) and its step (Jul 1) are both stale; only the crew
	// is fresh. The step is in_progress so it has a live crew assigned.
	f.beads["gd-step1"].Status = "in_progress"
	f.beads["gd-step1"].UpdatedAt = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	crew := seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	crew.UpdatedAt = time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC)

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The precise tooltip must carry the CREW's heartbeat instant (Jul 8), proving
	// the fold looked past the stale step/epic (Jul 1) to the live crew. Match the
	// tooltip PREFIX (no closing quote) — an active month-stale plan appends the
	// iter-38 freshness-stale suffix to this same title.
	if !strings.Contains(body, `title="Last change 2026-07-08T09:30:00Z`) {
		t.Errorf("index last-change tooltip should reflect the crew's Jul 8 heartbeat, body:\n%s", body)
	}
	if strings.Contains(body, `title="Last change 2026-07-01T10:00:00Z"`) {
		t.Errorf("index card must not stamp the stale step/epic instant when a live crew is fresher")
	}
}

// TestHandlePlanDetail_LastChangeFoldsCrewHeartbeat is the cockpit-side twin of
// the above (ka-0000004310, iter-24): the /plans/{id} header's freshness stamp
// must fold the crew roster's heartbeats too, so the index card and the cockpit
// header show the SAME instant for one plan. FAILS before the fold.
func TestHandlePlanDetail_LastChangeFoldsCrewHeartbeat(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	f.beads["gd-step1"].UpdatedAt = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	crew := seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	crew.UpdatedAt = time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC)

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `title="Last change 2026-07-08T09:30:00Z"`) {
		t.Errorf("detail last-change tooltip should reflect the crew's Jul 8 heartbeat, body:\n%s", body)
	}
	if strings.Contains(body, `title="Last change 2026-07-01T10:00:00Z"`) {
		t.Errorf("detail header must not stamp the stale step/epic instant when a live crew is fresher")
	}
}

// TestHandlePlansIndex_LastChangeCrewMonotonic proves the crew fold is MONOTONIC
// (ka-0000004310): a STALE crew (older than the step) must never REGRESS the
// stamp backward. Here the step is fresh (Jul 8) and a closed crew last beat
// Jul 1 — the stamp must stay Jul 8 (the step), not slip back to the crew.
func TestHandlePlansIndex_LastChangeCrewMonotonic(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].UpdatedAt = time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC)
	crew := seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "done", "closed")
	crew.UpdatedAt = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Match the tooltip PREFIX (no closing quote) — an active month-stale plan
	// appends the iter-38 freshness-stale suffix to this same title.
	if !strings.Contains(body, `title="Last change 2026-07-08T09:30:00Z`) {
		t.Errorf("a stale crew must not regress the stamp below the fresher step (Jul 8), body:\n%s", body)
	}
	if strings.Contains(body, `title="Last change 2026-07-01T00:00:00Z"`) {
		t.Errorf("index card stamped the stale crew instant — the fold is not monotonic")
	}
}

// TestHandlePlanDetail_TimelineBeforeBundle proves the step timeline (work
// items) sits BEFORE the plan bundle in the rendered page (Matt ka-0000004291:
// "put the work items above the plan in the UX"). This SUPERSEDES the prior
// steps-after-bundle order (ka-0000001134).
func TestHandlePlanDetail_TimelineBeforeBundle(t *testing.T) {
	f := seedPlansFake()
	// Attach a bundle child so the plan-bundle section renders.
	bundle := &beadsapi.BeadDetail{
		ID:     "gd-bundle1",
		Title:  "Plan bundle doc",
		Kind:   "data",
		Type:   "bundle",
		Status: "open",
	}
	f.beads["gd-bundle1"] = bundle
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-bundle1", DependsOnID: "gd-plan1", Type: "parent-child"})

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Compare positions of the rendered MARKUP (not the class names, which also
	// appear earlier in the <style> block). The bundle section opens with
	// <section class="plan-bundle"> and the timeline body with the #plan-steps id.
	bundleIdx := strings.Index(body, `<section class="plan-bundle"`)
	timelineIdx := strings.Index(body, `id="plan-steps"`)
	if bundleIdx < 0 {
		t.Fatalf("plan-bundle section not rendered")
	}
	if timelineIdx < 0 {
		t.Fatalf("plan step timeline not rendered")
	}
	if timelineIdx > bundleIdx {
		t.Errorf("step timeline (%d) should render BEFORE the bundle (%d)", timelineIdx, bundleIdx)
	}
}

// TestHandlePlanDetail_GridLayout proves the "official and best" cockpit layout
// (Matt ka-0000007957): the PLAN on the left, the BUILD STEPS on the right, and
// the ASSISTANT pane full-width along the bottom. The layout is expressed with
// named CSS grid areas so the visual placement is decoupled from DOM order
// (build steps stay DOM-first for work-items-prominence + narrow-screen
// stacking — proven by TestHandlePlanDetail_TimelineBeforeBundle). This test
// locks in (a) the grid-area map, (b) each pane sitting in its area container,
// and (c) the assistant area spanning both columns.
func TestHandlePlanDetail_GridLayout(t *testing.T) {
	f := seedPlansFake()
	// A bound assistant so the assistant lane renders inside its column.
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-assistant-1", "gd-plan1", "working", planAssistantTestNow)

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// (a) The named-area map: plan|steps on the top row, assistant spanning the
	// bottom row. Assert on the exact declaration so a future single-column or
	// reordered rewrite trips this test rather than silently changing the UX.
	if !strings.Contains(body, `grid-template-areas: "plan steps" "assistant assistant"`) {
		t.Error("plan grid should map plan->left, steps->right, assistant->full-width bottom")
	}

	// (a2) The two top-row tracks are BALANCED (equal minmax(0, 1fr)) — steps
	// and narrative get the same width so neither column starves and the long
	// step timeline is not crammed into a narrow track (ka-0000001855, which
	// fixed the earlier 1.5fr/1fr "narrative wider" split). Scoped to the
	// .plan-grid rule so the shared-CSS grids on the full layout don't
	// interfere. minmax(0, …) is load-bearing: a bare 1fr floors at min-content
	// and reintroduces horizontal overflow (cf. .charts-grid, ka-0000004897).
	grid := sliceAfter(body, ".plan-grid {")
	if grid == "" {
		t.Fatal(".plan-grid rule not found in rendered page")
	}
	if !strings.Contains(grid, "grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);") {
		t.Error(".plan-grid top-row tracks are not balanced minmax(0, 1fr) minmax(0, 1fr) " +
			"(ka-0000001855); an unbalanced split cramps the step timeline, and a bare " +
			"1fr floors at min-content and overflows narrow viewports (ka-0000004897)")
	}

	// (b) Each pane lives in its own grid-area container.
	planColIdx := strings.Index(body, `class="plan-col-plan"`)
	stepsColIdx := strings.Index(body, `class="plan-col-steps"`)
	assistantColIdx := strings.Index(body, `class="plan-col-assistant"`)
	if planColIdx < 0 || stepsColIdx < 0 || assistantColIdx < 0 {
		t.Fatalf("expected all three grid-area containers; got plan=%d steps=%d assistant=%d",
			planColIdx, stepsColIdx, assistantColIdx)
	}

	// The narrative + decisions render inside the plan column, the timeline
	// inside the steps column, and the assistant lane inside the bottom column.
	narrativeIdx := strings.Index(body, `id="plan-narrative"`)
	decisionsIdx := strings.Index(body, `id="plan-decisions"`)
	stepsIdx := strings.Index(body, `id="plan-steps"`)
	assistantIdx := strings.Index(body, `id="plan-agent"`)
	for name, idx := range map[string]int{
		"plan-narrative": narrativeIdx, "plan-decisions": decisionsIdx,
		"plan-steps": stepsIdx, "plan-agent": assistantIdx,
	} {
		if idx < 0 {
			t.Fatalf("expected the %q pane to render", name)
		}
	}
	// Steps column comes first in DOM (before the plan column) so it precedes
	// the bundle and stacks first on narrow screens; the grid area floats it
	// right on wide screens.
	if !(stepsColIdx < planColIdx && planColIdx < assistantColIdx) {
		t.Errorf("expected DOM order steps-col < plan-col < assistant-col; got %d, %d, %d",
			stepsColIdx, planColIdx, assistantColIdx)
	}
	// The step timeline sits inside the steps column (steps-col opens before
	// #plan-steps, which opens before the plan column).
	if !(stepsColIdx < stepsIdx && stepsIdx < planColIdx) {
		t.Errorf("#plan-steps (%d) should render inside the steps column (%d..%d)", stepsIdx, stepsColIdx, planColIdx)
	}
	// Narrative + decisions sit inside the plan column (between plan-col and the
	// assistant column).
	if !(planColIdx < narrativeIdx && narrativeIdx < assistantColIdx) {
		t.Errorf("#plan-narrative (%d) should render inside the plan column (%d..%d)", narrativeIdx, planColIdx, assistantColIdx)
	}
	if !(planColIdx < decisionsIdx && decisionsIdx < assistantColIdx) {
		t.Errorf("#plan-decisions (%d) should render inside the plan column (%d..%d)", decisionsIdx, planColIdx, assistantColIdx)
	}
	// The assistant lane sits inside the bottom (last) column.
	if !(assistantColIdx < assistantIdx) {
		t.Errorf("#plan-agent (%d) should render inside the assistant column (opens at %d)", assistantIdx, assistantColIdx)
	}
}

// TestHandlePlanDetail_NarrativeOverflowGuards asserts the cockpit ships all
// three narrative horizontal-overflow guards so wide content stays inside the
// column instead of widening the page at 375px:
//   - Cause B (ka-0000000256): wide <pre>/<table> scroll inside their own box
//     (overflow-x:auto; the <table> is display:block so it can clip).
//   - Cause C (ka-0000002291): a long unbroken inline <code> token in a prose
//     paragraph gets a break opportunity (overflow-wrap:anywhere) scoped to
//     text blocks — NOT to <table>, whose scroll containment above would break
//     if it were forced to wrap. The E2E plans.spec.js test measures the actual
//     no-page-overflow behavior; this is the fast browser-free regression guard.
func TestHandlePlanDetail_NarrativeOverflowGuards(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Cause B: preformatted + tabular content scrolls in its own box.
	for _, want := range []string{
		".plan-narrative pre, .plan-narrative .bundle-doc-content { overflow-x: auto; max-width: 100%; }",
		".plan-narrative table { display: block; overflow-x: auto; max-width: 100%; }",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing Cause-B overflow guard (ka-0000000256): %q", want)
		}
	}

	// Cause C: inline code in prose blocks gets a break opportunity. Scoped to
	// text containers (p/li/headings/blockquote/dd/dt) + their code, so the
	// sibling <table> keeps its own scroll box rather than being force-wrapped.
	if !strings.Contains(body, "overflow-wrap: anywhere; word-break: break-word;") {
		t.Error("missing Cause-C inline-code wrap guard (ka-0000002291): prose blocks " +
			"should carry overflow-wrap:anywhere so a long unbroken inline <code> token " +
			"wraps within the column instead of widening the page at 375px")
	}
	if !strings.Contains(body, ".plan-narrative :is(p, li, h1, h2, h3, h4, h5, h6, blockquote, dd, dt) code") {
		t.Error("Cause-C wrap guard (ka-0000002291) must target .plan-narrative inline " +
			"<code> inside prose blocks (and NOT <table>, whose overflow-x scroll box " +
			"must be preserved)")
	}
}

// TestHandlePlanEvents_ReRendersStepsOnChildChange proves the live-progress SSE
// stream re-renders the step timeline when a child step bead changes, and drops
// events for beads outside the plan's subtree.
func TestHandlePlanEvents_ReRendersStepsOnChildChange(t *testing.T) {
	f := seedPlansFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// A close of the plan's own step child → should re-render the timeline.
	enqueuePlanEvent(t, ch, "beads.bead.closed", "gd-step1")
	// A change to an unrelated bead → must be filtered out (no frame).
	enqueuePlanEvent(t, ch, "beads.bead.updated", "gd-unrelated")
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-updated") {
		t.Errorf("expected a plan-updated frame, got: %s", got)
	}
	if !strings.Contains(got, "plan-timeline") {
		t.Errorf("expected the re-rendered step timeline, got: %s", got)
	}
	if !strings.Contains(got, "Wire the route") {
		t.Errorf("expected the step title in the re-rendered fragment, got: %s", got)
	}
	// S3 (ka-0000002473): the stream also subscribes to dependency add/remove
	// so a doc added to the plan bundle mid-session surfaces immediately.
	if f.eventStreamLastTopics != planEventsTopics {
		t.Errorf("EventStream topics = %q, want %q", f.eventStreamLastTopics, planEventsTopics)
	}
}

// enqueuePlanEvent marshals a minimal bead-lifecycle event for the plan SSE
// stream (only the bead id matters — the handler re-reads live state to render).
func enqueuePlanEvent(t *testing.T, ch chan<- beadsapi.SSEEvent, topic, id string) {
	t.Helper()
	payload := []byte(`{"bead":{"id":"` + id + `"}}`)
	ch <- beadsapi.SSEEvent{Event: topic, Data: payload}
}

// seedPlansFullRowFake extends seedPlansFake with the S4 full-row fixtures
// (ka-0000001060): three steps in creation order — step1 closed (with a
// linked merged MR + assignee agent), step2 open and blocked by an open
// out-of-plan bead AND by a closed bead (which must NOT chip), step3 open.
func seedPlansFullRowFake() *fakeDaemon {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "closed"
	f.beads["gd-step1"].Assignee = "crew-1"
	f.beads["gd-step1"].CreatedAt = time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	f.beads["gd-step1"].ClosedAt = "2026-07-01T12:00:00Z"
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Ship the template", Kind: "issue", Type: "task",
		Status:    "open",
		CreatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.beads["gd-step3"] = &beadsapi.BeadDetail{
		ID: "gd-step3", Title: "Verify in staging", Kind: "issue", Type: "task",
		Status:    "open",
		CreatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-step3", DependsOnID: "gd-plan1", Type: "parent-child"},
	)
	// step1's assignee resolves to a live agent bead (agentLink target).
	f.beads["gd-agent1"] = &beadsapi.BeadDetail{
		ID: "gd-agent1", Title: "crew-1", Kind: "agent", Type: "agent",
		Status: "open", Fields: map[string]string{"agent": "crew-1"},
	}
	// step1 produced a merged MR (forward produces edge → MR bead).
	f.beads["gd-mr1"] = &beadsapi.BeadDetail{
		ID: "gd-mr1", Title: "MR !77", Kind: "data", Type: "merge-request",
		Status: "closed",
		Fields: map[string]string{"mr_state": "merged", "mr_url": "https://gitlab.example/mr/77"},
	}
	// step2 is blocked by an open out-of-plan bead and a closed one; only
	// the open blocker chips.
	f.beads["gd-blk-open"] = &beadsapi.BeadDetail{
		ID: "gd-blk-open", Title: "Provision the cluster", Kind: "issue", Type: "task", Status: "open",
	}
	f.beads["gd-blk-closed"] = &beadsapi.BeadDetail{
		ID: "gd-blk-closed", Title: "Old blocker", Kind: "issue", Type: "task", Status: "closed",
	}
	if f.deps == nil {
		f.deps = map[string][]beadsapi.Dependency{}
	}
	f.deps["gd-step1"] = []beadsapi.Dependency{
		{BeadID: "gd-step1", DependsOnID: "gd-mr1", Type: "produces"},
	}
	f.deps["gd-step2"] = []beadsapi.Dependency{
		{BeadID: "gd-step2", DependsOnID: "gd-blk-open", Type: "blocks"},
		{BeadID: "gd-step2", DependsOnID: "gd-blk-closed", Type: "blocks"},
	}
	// Transitive subtree rollup: 3 total, 1 closed (matches the direct set here).
	f.epicSubtreeCount = map[string]int{"gd-plan1": 3, "gd-plan1|closed": 1}
	return f
}

// TestHandlePlanDetail_FullRowColumns proves the step rows carry the S4
// full-row contract: assignee agent link, MR pill, open-blocker chip (and no
// chip for the closed blocker), and created/closed timestamps.
func TestHandlePlanDetail_FullRowColumns(t *testing.T) {
	mux := mkServer(t, seedPlansFullRowFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-agent="crew-1"`,          // assignee resolved to an agent link…
		`href="/gd-agent1"`,            // …pointing at the agent bead
		`chip-mr chip-mr-merged`,       // MR pill with state class
		`https://gitlab.example/mr/77`, // MR pill links out
		`plan-tl-blocked`,              // blocked-by chip present
		`gd-blk-open`,                  // …for the OPEN blocker
		`created 2026-07-01T08:00:00Z`, // created stamp tooltip (precise)
		`closed 2026-07-01T12:00:00Z`,  // closed stamp tooltip (precise)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q", want)
		}
	}
	// The CLOSED blocker must not chip — it's history, not a waiting-on signal.
	if strings.Contains(body, "gd-blk-closed") {
		t.Error("closed blocker must not render a blocked-by chip")
	}
}

// TestHandlePlanDetail_StepSectionSlugAttr proves a step whose description refs
// a plan-bundle section (`gd-BUNDLE#anchor`) renders `data-section-slug` on its
// row carrying the bare heading-anchor slug. plan_gutter.js reads that attribute
// to map a highlighted narrative passage back to its step so the critique tray
// opens on the right step (ka-0000003210). A step with no section ref stamps no
// attribute, so highlight-mapping falls back rather than mis-binding.
func TestHandlePlanDetail_StepSectionSlugAttr(t *testing.T) {
	f := seedPlansFullRowFake()
	// Attach a narrative bundle so a #anchor ref resolves to a section, and point
	// step2's description at section C1.1 of it (slug c1-1).
	f.beads["gd-bundle1"] = &beadsapi.BeadDetail{
		ID: "gd-bundle1", Title: "Plan bundle", Kind: "data", Type: "bundle", Status: "open",
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-bundle1", DependsOnID: "gd-plan1", Type: "parent-child"})
	f.beads["gd-step2"].Description = "Implement per the plan — see gd-bundle1#C1.1 for the shape."

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// step2's row carries the slug attribute…
	i2 := strings.Index(body, `data-bead-id="gd-step2"`)
	if i2 < 0 {
		t.Fatalf("step2 row not rendered")
	}
	end2 := min(i2+400, len(body))
	if !strings.Contains(body[i2:end2], `data-section-slug="c1-1"`) {
		t.Errorf("step2 row must carry data-section-slug=c1-1, row: %.400s", body[i2:end2])
	}
	// …while step3 (no section ref) stamps no slug attribute.
	i3 := strings.Index(body, `data-bead-id="gd-step3"`)
	if i3 < 0 {
		t.Fatalf("step3 row not rendered")
	}
	end3 := min(i3+400, len(body))
	if strings.Contains(body[i3:end3], "data-section-slug") {
		t.Errorf("step3 has no section ref and must not stamp data-section-slug, row: %.400s", body[i3:end3])
	}
}

// TestHandlePlanDetail_StableStepNumbering proves numbering derives from
// creation order and does NOT reshuffle as steps close: with step1 (oldest)
// closed, step2 and step3 keep positions 2 and 3, and the closed step stays
// first in document order (creation order) rather than sinking to the bottom.
func TestHandlePlanDetail_StableStepNumbering(t *testing.T) {
	mux := mkServer(t, seedPlansFullRowFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	i1 := strings.Index(body, `data-bead-id="gd-step1"`)
	i2 := strings.Index(body, `data-bead-id="gd-step2"`)
	i3 := strings.Index(body, `data-bead-id="gd-step3"`)
	if i1 < 0 || i2 < 0 || i3 < 0 {
		t.Fatalf("missing step rows: step1=%d step2=%d step3=%d", i1, i2, i3)
	}
	if !(i1 < i2 && i2 < i3) {
		t.Errorf("steps must render in creation order (closed step1 first), got positions %d/%d/%d", i1, i2, i3)
	}
	// The open steps keep their creation-order numbers (2 and 3) even though
	// their older sibling closed; under the old open-first sort step2 would
	// have been renumbered to 1.
	row2 := body[i2:i3]
	if !strings.Contains(row2, ">2</span>") {
		t.Errorf("step2 should keep stable number 2, row: %.300s", row2)
	}
	end3 := min(i3+600, len(body))
	if !strings.Contains(body[i3:end3], ">3</span>") {
		t.Errorf("step3 should keep stable number 3")
	}
}

// TestPlanProgress_IndexMatchesDetail proves the /plans index % and the
// /plans/{id} header + fragment % agree — AND that both count exactly the
// RENDERED step rows, not the transitive issue subtree (ka-0000004410). The
// fixture has 2 direct issue children (1 closed → 1/2 · 50%) but a transitive
// subtree of 5/2 (40%): the extra 3 descendants are grandchildren under a step
// (or a freestanding sub-plan) that the timeline never renders. The header/index
// denominator must equal the visible rows (2), never the subtree count (5).
func TestPlanProgress_IndexMatchesDetail(t *testing.T) {
	f := seedPlansFake()
	// Add a second direct issue child (closed) so the rendered set is 2 rows,
	// 1 closed → 1/2 · 50%.
	f.beads["gd-step2"] = &beadsapi.BeadDetail{
		ID: "gd-step2", Title: "Second step", Kind: "issue", Type: "task",
		Status: "closed", CreatedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	// The transitive subtree is BIGGER than the rendered rows — the shape of a
	// plan whose steps have their own sub-tasks. Neither surface may show it.
	f.epicSubtreeCount = map[string]int{"gd-plan1": 5, "gd-plan1|closed": 2}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("index: expected 200, got %d", w.Code)
	}
	indexBody := w.Body.String()
	if !strings.Contains(indexBody, "1/2 steps · 50%") {
		t.Errorf("index should show the rendered rollup 1/2 · 50%%")
	}
	if strings.Contains(indexBody, "2/5") {
		t.Errorf("index must NOT show the transitive subtree count 2/5")
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d", w.Code)
	}
	detailBody := w.Body.String()
	// Header chip and the plan_steps fragment head both render the rendered
	// rows — the same numbers as the index, and the same as the visible ✓ rows.
	if !strings.Contains(detailBody, "1/2 · 50%") {
		t.Errorf("detail header should show the rendered rollup 1/2 · 50%%")
	}
	if !strings.Contains(detailBody, "1/2 done · 50%") {
		t.Errorf("plan_steps fragment should show the rendered rollup 1/2 done · 50%%")
	}
	if strings.Contains(detailBody, "2/5") {
		t.Errorf("detail must NOT show the transitive subtree count 2/5")
	}
}

// TestPlanPercent_FloorAndZeroGuard pins the ONE percent primitive the /plans
// index card, the /plans/{id} header, and the per-phase chips all route through
// (ka-0000007284). Two invariants the surfaces rely on:
//   - it FLOORS (int truncation): 23/25 reads 92, not a rounded-up 92; 2/3 reads
//     66, not 67. Every surface therefore shows the same integer for the same
//     ratio, and none reads "100%" until every step is actually closed.
//   - total<=0 yields 0 (no divide-by-zero, no NaN→garbage-int) — the behavior of
//     the `if total > 0` guards it replaced, so a stepless plan shows no progress.
func TestPlanPercent_FloorAndZeroGuard(t *testing.T) {
	cases := []struct {
		closed, total, want int
	}{
		{0, 0, 0},    // no steps → 0 (the zero-guard; no panic/NaN)
		{1, 0, 0},    // defensive: closed>total==0 still guarded
		{0, 5, 0},    // none closed
		{5, 5, 100},  // all closed
		{2, 3, 66},   // 66.6… floors to 66, never rounds to 67
		{23, 25, 92}, // the acme-scif ratio (ka-0000004410): floors to 92
		{1, 3, 33},   // 33.3… floors to 33
		{99, 100, 99},
	}
	for _, c := range cases {
		if got := planPercent(c.closed, c.total); got != c.want {
			t.Errorf("planPercent(%d, %d) = %d, want %d", c.closed, c.total, got, c.want)
		}
	}
}

// TestPlanPercent_LockstepAcrossSurfaces is the divergence guard for this task
// (ka-0000007284): it drives BOTH the real /plans index HTTP handler (the card,
// via planIndexEnrichTimed → renderedStepRollup → planPercent) and the real
// /plans/{id} detail HTTP handler (the header, via planTimeline → planProgress →
// planPercent) over ONE phased+flat fixture, then asserts the progress string
// each RENDERS is identical. Reading the rendered markup — not re-deriving the
// number in the test — is what makes this catch a drift introduced at the render
// call site itself (e.g. someone hand-inlines a math.Round at the card only).
//
// The fixture is tuned to 23 closed / 25 rendered = 92%. The parent fix
// (ka-0000004410) unified the COUNT the two surfaces show; this pins that the
// PERCENT is unified too, so the index card can never read a different % from
// /plans/{id} for the same plan.
func TestPlanPercent_LockstepAcrossSurfaces(t *testing.T) {
	mux := mkServer(t, seedLockstepPlanFake())

	// Index card: GET /plans renders the plan_card progress label
	// "{closed}/{total} steps · {pct}%".
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("index: expected 200, got %d", w.Code)
	}
	indexBody := w.Body.String()

	// Detail header: GET /plans/{id} renders the plan-chip-progress
	// "{closed}/{total} · {pct}%".
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d", w.Code)
	}
	detailBody := w.Body.String()

	// The fixture is 23/25 · 92% by construction. Pin the exact rendered strings on
	// BOTH surfaces so (a) a fixture/count drift is caught, and (b) the two surfaces
	// are proven to render the SAME numerator/denominator/percent — index % !=
	// detail % is the exact bug this task closes.
	if !strings.Contains(indexBody, "23/25 steps · 92%") {
		t.Errorf("index card should render '23/25 steps · 92%%' (the rendered-step rollup)")
	}
	if !strings.Contains(detailBody, "23/25 · 92%") {
		t.Errorf("detail header should render '23/25 · 92%%' (identical to the index card)")
	}
	// The plan_steps fragment head renders the same rollup a third way; it must
	// agree too (all three chips route through planProgress/planPercent).
	if !strings.Contains(detailBody, "23/25 done · 92%") {
		t.Errorf("plan_steps fragment should render '23/25 done · 92%%' in lockstep with the header")
	}
}

// seedLockstepPlanFake builds a phased+flat plan whose rendered step set is 25
// steps, 23 closed (92%) — a rounding-boundary fixture for
// TestPlanPercent_LockstepAcrossSurfaces. Two phases carry most of the steps and
// a handful of unphased flat steps make up the remainder, so BOTH the phase pass
// and the flat pass of every rollup contribute. A direct bundle + a direct
// decision child sit in the tree too (never counted as steps) so the fixture also
// exercises the non-step exclusion the two paths must apply identically.
func seedLockstepPlanFake() *fakeDaemon {
	f := newFake()
	tm := func(min int) time.Time { return time.Date(2026, 7, 1, 10, min, 0, 0, time.UTC) }

	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "Plan: lockstep", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		Fields:    map[string]string{"phase_order": `["gd-phaseA","gd-phaseB"]`},
		UpdatedAt: tm(0),
	}
	bundle := &beadsapi.BeadDetail{ID: "gd-bundle1", Kind: "issue", Type: "bundle", Status: "open"}
	dec := &beadsapi.BeadDetail{ID: "gd-dec1", Type: "decision", Status: "open"}
	phaseA := &beadsapi.BeadDetail{ID: "gd-phaseA", Title: "Phase A", Kind: "issue", Type: "epic", Status: "in_progress"}
	phaseB := &beadsapi.BeadDetail{ID: "gd-phaseB", Title: "Phase B", Kind: "issue", Type: "epic", Status: "in_progress"}

	f.beads["gd-plan1"] = plan
	f.beads["gd-bundle1"] = bundle
	f.beads["gd-dec1"] = dec
	f.beads["gd-phaseA"] = phaseA
	f.beads["gd-phaseB"] = phaseB

	// Direct children of the plan: the two phases + the non-step bundle & decision
	// + the flat steps (added below).
	planKids := []beadsapi.Dependency{
		{BeadID: "gd-phaseA", DependsOnID: "gd-plan1", Type: "parent-child"},
		{BeadID: "gd-phaseB", DependsOnID: "gd-plan1", Type: "parent-child"},
		{BeadID: "gd-bundle1", DependsOnID: "gd-plan1", Type: "parent-child"},
		{BeadID: "gd-dec1", DependsOnID: "gd-plan1", Type: "parent-child"},
	}

	// mkSteps mints n tasks under parent, the first `closed` of them closed, and
	// returns their reverse-dep edges. Ids are prefixed so they never collide.
	minute := 0
	mkSteps := func(parent, prefix string, n, closed int) []beadsapi.Dependency {
		deps := make([]beadsapi.Dependency, 0, n)
		for i := range n {
			minute++
			id := prefix + "-" + string(rune('a'+i))
			status := "open"
			if i < closed {
				status = "closed"
			}
			f.beads[id] = &beadsapi.BeadDetail{
				ID: id, Title: id, Kind: "issue", Type: "task", Status: status, CreatedAt: tm(minute),
			}
			deps = append(deps, beadsapi.Dependency{BeadID: id, DependsOnID: parent, Type: "parent-child"})
		}
		return deps
	}

	// Phase A: 10 steps, 9 closed. Phase B: 10 steps, 9 closed. Flat: 5 steps, 5
	// closed. Total rendered = 25, closed = 23 → 92%.
	phaseADeps := mkSteps("gd-phaseA", "gd-a", 10, 9)
	phaseBDeps := mkSteps("gd-phaseB", "gd-b", 10, 9)
	flatDeps := mkSteps("gd-plan1", "gd-f", 5, 5)
	planKids = append(planKids, flatDeps...)

	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-plan1":  planKids,
		"gd-phaseA": phaseADeps,
		"gd-phaseB": phaseBDeps,
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	return f
}

// TestHandlePlanEvents_FragmentCarriesFullRow proves the SSE re-render stays
// in lockstep with the page render: the plan-updated fragment carries the
// full-row columns (assignee link, MR pill, blocker chip, stamps) and the
// unified transitive progress.
func TestHandlePlanEvents_FragmentCarriesFullRow(t *testing.T) {
	f := seedPlansFullRowFake()
	ch := make(chan beadsapi.SSEEvent, 2)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	enqueuePlanEvent(t, ch, "beads.bead.closed", "gd-step1")
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-updated") {
		t.Fatalf("expected a plan-updated frame, got: %s", got)
	}
	for _, want := range []string{
		`data-agent="crew-1"`, // assignee agent link
		`chip-mr-merged`,      // MR pill
		`plan-tl-blocked`,     // blocked-by chip
		`gd-blk-open`,         // …the open blocker
		`1/3 done · 33%`,      // unified transitive progress in the fragment
	} {
		if !strings.Contains(got, want) {
			t.Errorf("SSE fragment missing %q", want)
		}
	}
}

func TestHandlePlanDetail_NonEpic404s(t *testing.T) {
	// gd-aaa is a task in newFake — /plans/{id} only renders epics.
	mux := mkServer(t, seedPlansFake())
	req := httptest.NewRequest("GET", "/plans/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-epic, got %d", w.Code)
	}
}

// TestHandlePlanDetail_BareBundleRedirectsToBundleView verifies that a plan
// "published" as a bare type=bundle doc with no epic parent (e.g. via
// `gb bundle create --kind plan`) resolves its /plans/<id> link via a
// redirect to /bundle/<id> instead of 404ing (gd-Ip5pI9fhN8).
func TestHandlePlanDetail_BareBundleRedirectsToBundleView(t *testing.T) {
	f := newFake()
	f.beads["gd-bundle1"] = &beadsapi.BeadDetail{
		ID:     "gd-bundle1",
		Title:  "Design sketch",
		Kind:   "issue",
		Type:   "bundle",
		Status: "open",
		Fields: map[string]string{"kind": "plan"},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-bundle1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect for bare bundle, got %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/bundle/gd-bundle1" {
		t.Fatalf("Location = %q, want /bundle/gd-bundle1", got)
	}
}

func TestHandlePlanDetail_UnlabeledEpicShowsMarkAffordance(t *testing.T) {
	f := seedPlansFake()
	// Strip the plan label so the epic is a bare epic.
	f.beads["gd-plan1"].Labels = []string{"project:gardenio"}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Mark as plan") {
		t.Error("unlabeled epic should offer an operator 'Mark as plan' affordance")
	}
	if !strings.Contains(body, "not yet labelled") {
		t.Error("unlabeled epic should show the not-a-plan banner")
	}
}

func TestHandlePlanAddStep_OperatorCreatesChild(t *testing.T) {
	f := seedPlansFake()
	// A live assistant is bound, but the handler no longer nudges it INLINE
	// (epic ka-0000002170): the browser add-step is now a pure mint+link, and the
	// single PlanStepReactor drives the reactive nudge off the parent-child edge
	// (see plan_step_reactor_test.go). So the handler mints exactly one bead (the
	// step) and calls coopmux zero times.
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/steps",
		strings.NewReader("title=Add+tests"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if f.createCalls != 1 {
		t.Fatalf("expected exactly one CreateBead (the step; the reactor — not the handler — drives collaboration), got %d", f.createCalls)
	}
	if f.createLastReq.Title != "Add tests" {
		t.Errorf("created step title = %q, want %q", f.createLastReq.Title, "Add tests")
	}
	if f.createLastReq.Type != "task" {
		t.Errorf("created step type = %q, want task", f.createLastReq.Type)
	}
	// It must be linked to the plan as a parent-child child — that edge is what
	// the reactor observes to fire the (single) reactive nudge.
	if f.addDepLastType != "parent-child" || f.addDepLastDep != "gd-plan1" {
		t.Errorf("step link = (%s -> %s), want parent-child -> gd-plan1", f.addDepLastType, f.addDepLastDep)
	}
	// The handler fires NO inline reactive collaboration — no double seam.
	if cm.nudgeCalls != 0 {
		t.Errorf("add-step handler must not nudge inline (the reactor owns that now); got %d Nudge calls", cm.nudgeCalls)
	}
}

// TestHandlePlanAddStep_PhaseTargetParentsToPhase: add-step with a `phase` form
// field parents the new step to that phase sub-epic (not the plan), so it renders
// inline under the phase (ka-0000002361). The browser gets the re-rendered
// #plan-steps HTML fragment; the phase parentage is asserted on the recorded edge.
func TestHandlePlanAddStep_PhaseTargetParentsToPhase(t *testing.T) {
	f := seedPhasedPlanFake()
	f.createIDForReq = func(beadsapi.CreateBeadRequest) string { return "gd-newphasestep" }
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := httptest.NewRequest("POST", "/plans/gd-plan1/steps",
		strings.NewReader("title=Add+a+bench&phase=gd-phaseI"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	// The step's parent-child edge must point at the PHASE sub-epic, not the plan.
	// Assert on the step's own recorded deps (robust regardless of any other edge
	// writes on the request).
	var linkedToPhase bool
	for _, d := range f.deps["gd-newphasestep"] {
		if d.Type == "parent-child" && d.DependsOnID == "gd-phaseI" {
			linkedToPhase = true
		}
	}
	if !linkedToPhase {
		t.Errorf("step deps = %+v, want a parent-child edge -> gd-phaseI", f.deps["gd-newphasestep"])
	}
}

// TestHandlePlanAddStep_RejectsForeignPhase: a `phase` id that is NOT a phase of
// this plan (not in phase_order) is rejected with 400 and never mints a bead —
// the fail-closed guard so a stray id can't adopt a step under an unrelated epic.
func TestHandlePlanAddStep_RejectsForeignPhase(t *testing.T) {
	f := seedPhasedPlanFake()
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/steps", "title=Sneaky&phase=gd-bundle1")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-phase id, got %d (body: %s)", w.Code, w.Body.String())
	}
	if f.createCalls != 0 {
		t.Errorf("a rejected phase target must not mint a bead; got %d CreateBead calls", f.createCalls)
	}
}

// TestHandlePlanAddStep_NoInlineReactiveCollaboration asserts the handler's
// post-P3 contract: adding a step is a PURE mint+link and fires NO reactive
// collaboration inline — no coopmux Nudge, no assistant spawn, no parked-wake —
// regardless of the plan's assistant state (live / none / parked). The single
// PlanStepReactor is the sole reactive seam now; it observes the parent-child
// edge this handler writes and drives nudge/wake there (see
// plan_step_reactor_test.go). Killing the inline path is what eliminates the
// double-spawn fork (epic ka-0000002170, decision ka-0000007006): a browser add
// and a daemon-direct CLI add now reach collaboration through the exact same
// reactor, exactly once.
func TestHandlePlanAddStep_NoInlineReactiveCollaboration(t *testing.T) {
	// seedAssistant seeds the plan's bound agent (nil = no agent bound) into a
	// fresh fake; the subtest then asserts the handler mints only the step and
	// touches neither coopmux nor the assistant bead.
	cases := []struct {
		name      string
		seedAgent func(f *fakeDaemon)
	}{
		{
			name: "live assistant is NOT nudged inline",
			seedAgent: func(f *fakeDaemon) {
				seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
			},
		},
		{
			name:      "no assistant is NOT cold-spawned inline",
			seedAgent: func(*fakeDaemon) {},
		},
		{
			name: "parked assistant is NOT woken inline",
			seedAgent: func(f *fakeDaemon) {
				seedAssistantBead(f, "gd-asstpark", "gardenio-plan-asst-park", "gd-plan1", "stopping", planAssistantTestNow.Add(-2*time.Hour))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := seedPlansFake()
			tc.seedAgent(f)
			cm := &fakeScreenClient{}
			mux := plansAssistantMux(t, f, cm)

			req := operatorPost("/plans/gd-plan1/steps", "title=Ship+the+docs")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
			}
			// Exactly one create — the step. Never a second create (a cold spawn).
			if f.createCalls != 1 {
				t.Fatalf("expected exactly 1 CreateBead (the step; no inline spawn), got %d", f.createCalls)
			}
			if f.createLastReq.Type != "task" {
				t.Errorf("created step type = %q, want task", f.createLastReq.Type)
			}
			// The step is linked to the plan — the edge the reactor observes.
			if f.addDepLastType != "parent-child" || f.addDepLastDep != "gd-plan1" {
				t.Errorf("step link = (%s -> %s), want parent-child -> gd-plan1", f.addDepLastType, f.addDepLastDep)
			}
			// No inline reactive collaboration on ANY assistant state.
			if cm.nudgeCalls != 0 {
				t.Errorf("handler must not nudge inline; got %d Nudge calls", cm.nudgeCalls)
			}
			// No inline parked-wake: the handler must not flip the bound agent to
			// in_progress or refresh its wake fields (that is the reactor's job now).
			if f.updateCalls != 0 {
				t.Errorf("handler must not wake (flip status) inline; got %d UpdateBead calls", f.updateCalls)
			}
			if f.updateFieldsCalls != 0 {
				t.Errorf("handler must not refresh assistant wake fields inline; got %d UpdateBeadFields calls", f.updateFieldsCalls)
			}
		})
	}
}

// TestHandlePlanAddStep_MintsStepAndReRendersTimeline: the happy-path handler
// contract — the step is created + linked and the step timeline re-renders
// (not an error fragment). A live assistant is bound but a broken coopmux is
// wired to prove the handler never touches it: post-P3 the add-step path does
// NO inline collaboration, so a coopmux that would error cannot affect the
// outcome (the reactor, not the handler, owns nudge/wake off the edge below).
func TestHandlePlanAddStep_MintsStepAndReRendersTimeline(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	// A coopmux that WOULD error if the handler nudged — it must not be reached.
	cm := &fakeScreenClient{nudgeErr: errors.New("coopmux exploded")}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/steps", "title=Add+tests")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	// The step is still created and linked.
	if f.createCalls != 1 {
		t.Errorf("expected the step to be created (1 CreateBead), got %d", f.createCalls)
	}
	if f.addDepLastType != "parent-child" || f.addDepLastDep != "gd-plan1" {
		t.Errorf("step link = (%s -> %s), want parent-child -> gd-plan1", f.addDepLastType, f.addDepLastDep)
	}
	// The broken coopmux is never reached — the handler does no inline nudge.
	if cm.nudgeCalls != 0 {
		t.Errorf("handler must not reach coopmux inline; got %d Nudge calls", cm.nudgeCalls)
	}
	// The re-rendered step timeline still comes back (not an error fragment).
	if !strings.Contains(w.Body.String(), "plan-timeline") {
		t.Errorf("expected the step timeline to re-render; body=%s", w.Body.String())
	}
}

// TestHandlePlanAddStep_RapidAddsMintEveryStepNoInlineNudge: a burst of adds to
// the same plan mints EVERY step (none dropped) and fires ZERO inline nudges —
// the handler no longer owns the debounce. Coalescing a burst of step-adds into
// a single nudge is now the PlanStepReactor's job (via its shared per-plan
// last_nudge window; see TestReactToStepEdge_DebouncesBurst), keyed off the
// parent-child edges these adds emit — so the handler must never swallow a step
// for debounce reasons.
func TestHandlePlanAddStep_RapidAddsMintEveryStepNoInlineNudge(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-live", "gd-plan1", "working", planAssistantTestNow)
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	for _, title := range []string{"First+step", "Second+step"} {
		req := operatorPost("/plans/gd-plan1/steps", "title="+title)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("add %q: expected 200, got %d", title, w.Code)
		}
	}

	// Both steps created — a rapid burst never drops a step at the handler.
	if f.createCalls != 2 {
		t.Errorf("expected 2 steps created, got %d", f.createCalls)
	}
	// No inline nudge for either add — the reactor owns collaboration + debounce.
	if cm.nudgeCalls != 0 {
		t.Errorf("handler must not nudge inline on a burst; got %d nudge calls", cm.nudgeCalls)
	}
}

// TestHandlePlanAddStep_ClosedPlanStillMintsStepNoGardenerWrite: adding a step to
// a CLOSED plan still creates + links the step and never touches the plan's
// gardener (no coopmux Nudge, no wake flip to in_progress). Post-P3 the handler
// does no reactive work at all, so this holds by construction; the closed-plan
// backstop that stops re-driving a finished plan's gardener (ka-0000000753) now
// lives downstream — the daemon does not emit beads.plan.step.added for a closed
// plan (maybeEmitPlanStepAdded) and the reactor re-checks plan status — so a
// closed-plan add can never wake a gardener regardless of the writer.
func TestHandlePlanAddStep_ClosedPlanStillMintsStepNoGardenerWrite(t *testing.T) {
	f := seedPlansFake()
	// Close the plan epic — the "plan is done" state.
	f.beads["gd-plan1"].Status = "closed"
	// A PARKED gardener is bound to the plan: it must be left alone.
	seedAssistantBead(f, "gd-asstpark", "gardenio-plan-asst-park", "gd-plan1", "stopping", planAssistantTestNow.Add(-2*time.Hour))
	cm := &fakeScreenClient{}
	mux := plansAssistantMux(t, f, cm)

	req := operatorPost("/plans/gd-plan1/steps", "title=Late+addition")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	// The step is still created and linked even on a closed plan.
	if f.createCalls != 1 {
		t.Fatalf("expected exactly one CreateBead (the step), got %d", f.createCalls)
	}
	if f.addDepLastType != "parent-child" || f.addDepLastDep != "gd-plan1" {
		t.Errorf("step link = (%s -> %s), want parent-child -> gd-plan1", f.addDepLastType, f.addDepLastDep)
	}
	// No gardener interaction at all: no live-nudge, no wake flip to in_progress.
	if cm.nudgeCalls != 0 {
		t.Errorf("closed plan must not nudge its gardener; got %d Nudge calls", cm.nudgeCalls)
	}
	if f.updateCalls != 0 {
		t.Errorf("closed plan must not wake (flip to in_progress) its gardener; got %d UpdateBead calls", f.updateCalls)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("closed plan must not refresh the gardener's wake fields; got %d UpdateBeadFields calls", f.updateFieldsCalls)
	}
}

func TestHandlePlanAddStep_NonOperatorForbidden(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/steps",
		strings.NewReader("title=Sneaky"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("viewer must not be able to add a step; got 200")
	}
	if f.createCalls != 0 {
		t.Errorf("viewer add-step created %d beads, want 0", f.createCalls)
	}
}

func TestHandlePlanCloseStep_OperatorClosesChild(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/steps/gd-step1/close", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if f.closeCalls != 1 || f.closeLastID != "gd-step1" {
		t.Errorf("expected CloseBeadAs(gd-step1); got calls=%d last=%s", f.closeCalls, f.closeLastID)
	}
}

func TestHandlePlanMark_TogglesLabel(t *testing.T) {
	f := seedPlansFake()
	// Start unlabeled so mark ADDS the label.
	f.beads["gd-plan1"].Labels = []string{"project:gardenio"}
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/mark", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", w.Code)
	}
	if f.addLabelCalls != 1 || f.addLabelLastVal != "plan" {
		t.Errorf("expected AddLabel(plan); got calls=%d last=%s", f.addLabelCalls, f.addLabelLastVal)
	}
}

// TestPlansRoutePrecedence proves the literal /plans and two-segment
// /plans/{id} patterns don't get shadowed by the catch-all /{id} bead-show
// route, and that a non-plans single segment still falls through to bead-show.
func TestPlansRoutePrecedence(t *testing.T) {
	mux := mkServer(t, seedPlansFake())

	// /plans → index (200, plans page)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Plan: ship the /plans page") {
		t.Errorf("/plans did not resolve to the plans index (code %d)", w.Code)
	}

	// /gd-aaa → bead-show (a single non-plans segment falls through)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/gd-aaa", nil))
	if w.Code != http.StatusOK {
		t.Errorf("/gd-aaa should resolve to bead-show, got %d", w.Code)
	}
}

// --- S3: nudge-to-edit narrative + live prose/bundle streaming ---------------
// (task ka-0000002473; Q2 decision ka-0000005051: bundle-first)

// seedPlanBundleDoc extends seedPlansFake with a narrative bundle child and
// one markdown doc inside it, mirroring what `gb bundle create/add` writes.
func seedPlanBundleDoc(f *fakeDaemon) {
	f.beads["gd-bundle1"] = &beadsapi.BeadDetail{
		ID:     "gd-bundle1",
		Title:  "Plan bundle",
		Kind:   "data",
		Type:   "bundle",
		Status: "open",
	}
	f.beads["gd-doc1"] = &beadsapi.BeadDetail{
		ID:     "gd-doc1",
		Title:  "Narrative",
		Kind:   "data",
		Type:   "doc",
		Status: "open",
		Fields: map[string]string{
			"format":   "md",
			"position": "1",
			"headline": "Plan of record",
			"content":  "# Narrative\nSection 2 body.\n",
		},
	}
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"],
		beadsapi.Dependency{BeadID: "gd-bundle1", DependsOnID: "gd-plan1", Type: "parent-child"})
	f.revDeps["gd-bundle1"] = []beadsapi.Dependency{
		{BeadID: "gd-doc1", DependsOnID: "gd-bundle1", Type: "parent-child"},
	}
}

// TestHandlePlanDetail_TextareaRetired proves the manual prose editor is gone:
// no Edit affordance, no editor routes. Nudge-by-prompt (the assistant lane)
// is the only in-page edit path; kd stays the out-of-page escape hatch.
func TestHandlePlanDetail_TextareaRetired(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, gone := range []string{"plan-prose-edit-btn", "/prose/edit", "plan-prose-textarea"} {
		if strings.Contains(body, gone) {
			t.Errorf("plan cockpit still renders retired editor marker %q", gone)
		}
	}
	// The narrative block itself still renders, inside the SSE-swappable wrapper.
	if !strings.Contains(body, `id="plan-narrative"`) {
		t.Error("expected the #plan-narrative wrapper on the cockpit page")
	}
	if !strings.Contains(body, "Research the viewer") {
		t.Error("expected the epic-description summary in the narrative block")
	}

	// The retired editor routes are gone: GET /prose/edit and POST /prose must
	// not resolve (404 from the mux, or at least not 200/405-with-editor).
	for _, probe := range []struct{ method, path string }{
		{"GET", "/plans/gd-plan1/prose"},
		{"GET", "/plans/gd-plan1/prose/edit"},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(probe.method, probe.path, nil))
		if w.Code == http.StatusOK {
			t.Errorf("%s %s should be retired, got 200", probe.method, probe.path)
		}
	}
}

// TestHandlePlanNarrativeFragment_RendersSummaryAndBundle proves the standalone
// /plans/{id}/narrative fragment carries both the epic-description summary and
// the bundle doc cards — the same composition the detail page embeds and the
// SSE stream re-renders.
func TestHandlePlanNarrativeFragment_RendersSummaryAndBundle(t *testing.T) {
	f := seedPlansFake()
	seedPlanBundleDoc(f)
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans/gd-plan1/narrative", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="plan-narrative"`,
		"Research the viewer", // summary (epic description)
		"Plan of record",      // bundle doc headline
		"Section 2 body",      // bundle doc content
		"/bundle/gd-bundle1",  // open-bundle link
	} {
		if !strings.Contains(body, want) {
			t.Errorf("narrative fragment missing %q", want)
		}
	}
}

// TestHandlePlanEvents_NarrativeFrameOnDocContentChange is the S3 acceptance
// path: the assistant rewrites a bundle doc (kd update -f content=@file) → the
// daemon publishes beads.bead.updated with a per-field "content" change → the
// stream emits a plan-narrative-updated frame carrying the fresh doc content
// and the acting agent's attribution.
func TestHandlePlanEvents_NarrativeFrameOnDocContentChange(t *testing.T) {
	f := seedPlansFake()
	seedPlanBundleDoc(f)
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// Simulate the post-edit state the handler re-reads.
	f.beads["gd-doc1"].Fields["content"] = "# Narrative\nSection 2 tightened.\n"
	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.updated",
		Data:  []byte(`{"bead":{"id":"gd-doc1","type":"doc"},"changes":{"content":"# Narrative\nSection 2 tightened.\n","fields":"{}"},"actor":"gardenio-plan-asst-1"}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-narrative-updated") {
		t.Fatalf("expected a plan-narrative-updated frame, got: %s", got)
	}
	if !strings.Contains(got, "Section 2 tightened") {
		t.Errorf("expected the fresh doc content in the re-rendered fragment, got: %s", got)
	}
	if !strings.Contains(got, "gardenio-plan-asst-1") {
		t.Errorf("expected the acting agent attribution in the fragment, got: %s", got)
	}
	// A doc-content change must not flash the step timeline (the doc is not in
	// the steps interest set).
	if strings.Contains(got, "event: plan-updated") {
		t.Errorf("doc content change should not emit a plan-updated (steps) frame, got: %s", got)
	}
}

// TestHandlePlanEvents_NarrativeFrameOnEpicDescriptionChange: the assistant
// refreshing the epic-level summary (description) re-renders the narrative;
// an epic change also re-renders the steps (the epic is in both sets).
func TestHandlePlanEvents_NarrativeFrameOnEpicDescriptionChange(t *testing.T) {
	f := seedPlansFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	f.beads["gd-plan1"].Description = "Refreshed summary."
	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.updated",
		Data:  []byte(`{"bead":{"id":"gd-plan1","type":"epic"},"changes":{"description":"Refreshed summary."},"actor":"gardenio-plan-asst-1"}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-narrative-updated") {
		t.Fatalf("expected a plan-narrative-updated frame, got: %s", got)
	}
	if !strings.Contains(got, "Refreshed summary.") {
		t.Errorf("expected the refreshed summary in the fragment, got: %s", got)
	}
}

// TestHandlePlanEvents_NoNarrativeFrameOnStatusOnlyChange: a status flip on
// the epic (claim, close) re-renders the steps but must NOT flash the
// narrative — no content-bearing key changed.
func TestHandlePlanEvents_NoNarrativeFrameOnStatusOnlyChange(t *testing.T) {
	f := seedPlansFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.updated",
		Data:  []byte(`{"bead":{"id":"gd-plan1","type":"epic"},"changes":{"status":"in_progress"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if !strings.Contains(got, "event: plan-updated") {
		t.Errorf("expected a plan-updated (steps) frame, got: %s", got)
	}
	if strings.Contains(got, "event: plan-narrative-updated") {
		t.Errorf("status-only change should not emit a narrative frame, got: %s", got)
	}
}

// TestHandlePlanEvents_DependencyAddRebuildsFilter is the "new doc
// mid-session" fix: a doc bead added to the bundle AFTER connect (its
// parent-child edge lands as beads.dependency.added) must rebuild the
// interest set and stream the new doc immediately — not on reconnect.
func TestHandlePlanEvents_DependencyAddRebuildsFilter(t *testing.T) {
	f := seedPlansFake()
	seedPlanBundleDoc(f)
	// Start WITHOUT the doc edge: the connect-time filter sees an empty bundle.
	f.revDeps["gd-bundle1"] = nil
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	// Mid-session: the doc's parent-child edge into the bundle lands...
	f.revDeps["gd-bundle1"] = []beadsapi.Dependency{
		{BeadID: "gd-doc1", DependsOnID: "gd-bundle1", Type: "parent-child"},
	}
	ch <- beadsapi.SSEEvent{
		Event: "beads.dependency.added",
		Data:  []byte(`{"dependency":{"bead_id":"gd-doc1","depends_on_id":"gd-bundle1","type":"parent-child"}}`),
	}
	// ...then a content edit on that NEW doc must pass the rebuilt filter.
	f.beads["gd-doc1"].Fields["content"] = "# Narrative\nFresh doc streamed live.\n"
	ch <- beadsapi.SSEEvent{
		Event: "beads.bead.updated",
		Data:  []byte(`{"bead":{"id":"gd-doc1","type":"doc"},"changes":{"content":"x"},"actor":"gardenio-plan-asst-1"}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	// The dependency event itself re-renders the narrative (frame 1), and the
	// follow-up doc edit passes the rebuilt filter (frame 2).
	if n := strings.Count(got, "event: plan-narrative-updated"); n < 2 {
		t.Fatalf("expected 2 plan-narrative-updated frames (dep rebuild + doc edit), got %d in: %s", n, got)
	}
	if !strings.Contains(got, "Fresh doc streamed live") {
		t.Errorf("expected the mid-session doc's content to stream, got: %s", got)
	}
}

// TestHandlePlanEvents_UnrelatedDependencyIgnored: dependency churn elsewhere
// in the graph must not re-render anything.
func TestHandlePlanEvents_UnrelatedDependencyIgnored(t *testing.T) {
	f := seedPlansFake()
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkServer(t, f)

	ch <- beadsapi.SSEEvent{
		Event: "beads.dependency.added",
		Data:  []byte(`{"dependency":{"bead_id":"gd-x","depends_on_id":"gd-elsewhere","type":"parent-child"}}`),
	}
	close(ch)

	got := drainAgentsEventsStream(t, mux, "/plans/gd-plan1/events")
	if strings.Contains(got, "event: plan-updated") || strings.Contains(got, "event: plan-narrative-updated") {
		t.Errorf("unrelated dependency event should emit no frames, got: %s", got)
	}
}

// TestHandlePlanDetail_CritiqueOverlayRenders proves the inline line-critique
// cockpit (T4, ka-0000000167) ships ON BY DEFAULT: the static overlay shell +
// operator compose form are embedded in the plan detail page and the gutter
// script loads UNCONDITIONALLY (no flag guard, decision ka-0000004596). The
// compose form must carry T3's real anchor field names (block_id/quote/
// prefix/suffix) and post to the /plans/{id}/comments route.
func TestHandlePlanDetail_CritiqueOverlayRenders(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	// Operator context so the operator-only compose form renders.
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="plan-critique-overlay"`,         // the slide-in overlay shell
		`id="plan-critique-mount"`,           // the thread mount point
		`id="plan-critique-form"`,            // the operator compose form
		`hx-post="/plans/gd-plan1/comments"`, // posts to the T3 route (step appended client-side)
		`hx-target="#plan-critique-thread"`,  // swaps the T3 thread fragment
		`name="block_id"`,                    // T3 anchor field
		`name="quote"`,                       // T3 anchor field
		`name="prefix"`,                      // T3 anchor field (W3C surrounding context)
		`name="suffix"`,                      // T3 anchor field
		`name="text"`,                        // the comment body
		`plan_gutter.js`,                     // the gutter client, loaded unconditionally
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan detail (operator) missing critique marker %q", want)
		}
	}
	// The script tag must NOT be behind any flag/conditional gate — assert it
	// sits alongside plan_events.js as a plain deferred <script>.
	if !strings.Contains(body, `src="/assets/static/plan_gutter.js" defer`) {
		t.Error("plan_gutter.js must load unconditionally as a deferred script")
	}
}

// TestHandlePlanDetail_CritiqueComposeHiddenForViewer proves the operator-only
// compose form does NOT render for a read-only viewer (defense-in-depth mirror
// of the operatorOnly POST route), while the overlay shell + gutter script
// still ship so a viewer can read existing critique.
func TestHandlePlanDetail_CritiqueComposeHiddenForViewer(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `id="plan-critique-form"`) {
		t.Error("read-only viewer must not see the operator critique compose form")
	}
	// The read shell still ships so a viewer can open + read a step's critique.
	for _, want := range []string{`id="plan-critique-overlay"`, `plan_gutter.js`} {
		if !strings.Contains(body, want) {
			t.Errorf("viewer plan detail missing read-side critique marker %q", want)
		}
	}
}
