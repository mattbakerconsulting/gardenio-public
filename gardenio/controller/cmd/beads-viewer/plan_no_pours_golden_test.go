package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Regression guard for the pour-into-plan feature (epic ka-0000000759): a plan
// with NO poured phases must render byte-for-byte as it did BEFORE pours
// existed. The feature's entire render-time contribution to the /plans cockpit
// is the poured-phase provenance chip (templates/partials/plan_steps.html,
// gated on `$phase.FormulaID`) plus isPhaseContainer widening to accept
// type=molecule. For a plan that was never poured into — no molecule child, no
// formula_id anywhere — both are inert, so the emitted HTML must be unchanged.
//
// The #plan-steps fragment (renderPlanStepsFragment) is the isolated, exact
// render surface where the chip lives: it is the payload every SSE live-progress
// frame and HTMX add/close/reorder swap emits, and the full plan_detail page
// embeds only this partial for its step timeline. Pinning its bytes pins the
// operator-visible plan render.
//
// The golden is deterministic by construction: the fixture steps carry ZERO
// timestamps, so the template's `{{if $step.CreatedAgo}}` / `{{if
// $step.ClosedAgo}}` guards suppress the only `time.Since`-derived (wall-clock)
// spans — nothing else in the fragment depends on the clock, the daemon's
// return order, or map iteration (verified: repeated renders are byte-identical).

// goldenNoPoursPath is the checked-in baseline for the no-pours #plan-steps
// render. Regenerate after an INTENTIONAL, pour-independent template change with
// `GB_UPDATE_GOLDEN=1 go test ./cmd/beads-viewer/ -run TestPlanNoPours_Golden`
// and eyeball the diff — a change here that a pour-feature commit did NOT intend
// is exactly the regression this test exists to catch.
const goldenNoPoursPath = "testdata/plan_no_pours_steps.golden.html"

// seedNoPoursPlanFake builds a plan epic with three flat (unphased) step
// children — one closed, one in_progress, one open — and NOTHING pour-related:
// no phase_order, no type=molecule child, no formula_id / applied_vars fields.
// This is a plan exactly as it existed before the pour feature. Steps carry no
// timestamps so the render is clock-independent (see the file comment).
func seedNoPoursPlanFake() *fakeDaemon {
	f := newFake()
	plan := &beadsapi.BeadDetail{
		ID: "gd-plan1", Title: "Plan: ship the widget", Kind: "issue", Type: "epic",
		Status: "in_progress", Labels: []string{"project:gardenio", "plan"},
		Description: "A plan with no poured phases.",
		UpdatedAt:   time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}
	s1 := &beadsapi.BeadDetail{ID: "gd-s1", Title: "Design the widget", Kind: "issue", Type: "task", Status: "closed"}
	s2 := &beadsapi.BeadDetail{ID: "gd-s2", Title: "Build the widget", Kind: "issue", Type: "task", Status: "in_progress"}
	s3 := &beadsapi.BeadDetail{ID: "gd-s3", Title: "Test the widget", Kind: "issue", Type: "task", Status: "open"}
	for _, b := range []*beadsapi.BeadDetail{plan, s1, s2, s3} {
		f.beads[b.ID] = b
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-plan1": {
			{BeadID: "gd-s1", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-s2", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-s3", DependsOnID: "gd-plan1", Type: "parent-child"},
		},
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{plan}, Total: 1}
	f.epicSubtreeCount = map[string]int{"gd-plan1": 3, "gd-plan1|closed": 1}
	return f
}

// TestPlanNoPours_Golden pins the no-pours #plan-steps fragment to a checked-in
// byte-for-byte baseline. If a pour-feature change (or any other) alters how a
// never-poured plan renders, this diff fails — the regression guard the task
// (gd-Swxuu219aT) asks for.
func TestPlanNoPours_Golden(t *testing.T) {
	srv := NewServer(seedNoPoursPlanFake(), slog.Default(), "", "", "", nil)
	// renderPlanStepsFragment is the real SSE/HTMX swap payload. A nil *http.Request
	// is the SSE path's own call shape (bindAgentLink falls back to a background
	// ctx), so this exercises the fragment exactly as the live-progress stream does.
	got := srv.renderPlanStepsFragment(t.Context(), nil, "gd-plan1", false)

	if os.Getenv("GB_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(goldenNoPoursPath, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		t.Logf("updated golden %s (%d bytes)", goldenNoPoursPath, len(got))
		return
	}

	wantBytes, err := os.ReadFile(goldenNoPoursPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with GB_UPDATE_GOLDEN=1)", goldenNoPoursPath, err)
	}
	want := string(wantBytes)
	if got != want {
		t.Errorf("no-pours #plan-steps render drifted from the baseline in %s.\n"+
			"If this change is INTENTIONAL and pour-independent, regenerate with "+
			"GB_UPDATE_GOLDEN=1 and review the diff.\n%s",
			goldenNoPoursPath, firstDiff(want, got))
	}
}

// TestPlanNoPours_NoPourMarkup is the end-to-end companion: it drives the whole
// GET /plans/{id} page (not just the fragment) and proves the pour feature emits
// ZERO markup for a never-poured plan. These substrings are the feature's only
// render-surface fingerprints — the provenance chip class, its ⚗ glyph, and the
// per-phase grouping chrome. Their absence is what "identical to today" means at
// the page level, and it is robust to whitespace-only template churn that would
// (intentionally) force a golden regen above.
func TestPlanNoPours_NoPourMarkup(t *testing.T) {
	mux := mkServer(t, seedNoPoursPlanFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The flat timeline still renders (this is a real, populated plan).
	for _, want := range []string{
		`<ol class="plan-timeline">`, // the flat, unphased step list
		"Design the widget",          // a step title
		"Build the widget",
		"Test the widget",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no-pours plan render missing expected flat-timeline content %q", want)
		}
	}

	// None of the pour feature's RENDERED markup may appear. Match the class
	// ATTRIBUTE (`class="chip plan-phase-provenance"`), not the bare class name
	// — the latter also appears in the page's static <style> block (as do a stray
	// ⚗ in a CSS comment and the .plan-phase-* rules), so those are page chrome
	// present with or without pours. This mirrors how
	// TestHandlePlanDetail_MoleculePhaseProvenanceChip disambiguates the chip.
	for _, forbidden := range []string{
		`class="chip plan-phase-provenance"`, // the rendered poured-phase provenance chip
		"Poured from formula",                // the chip's hover title (rendered only on a poured phase)
		`class="plan-phase`,                  // per-phase grouping chrome (a phase is a molecule/epic container)
		`data-phase-id=`,                     // a rendered phase section's id attribute
		"Other steps",                        // the "unphased remainder" heading (only appears WITH phases)
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("no-pours plan must not emit pour/phase markup, but found %q", forbidden)
		}
	}
}

// firstDiff returns a short human-readable pointer at the first byte where two
// strings diverge, with a little context on each side — enough to eyeball a
// golden drift without dumping two multi-KB blobs into the test log.
func firstDiff(want, got string) string {
	n := min(len(want), len(got))
	i := 0
	for i < n && want[i] == got[i] {
		i++
	}
	lo := max(i-40, 0)
	wHi := min(i+40, len(want))
	gHi := min(i+40, len(got))
	return "first divergence at byte " + strconv.Itoa(i) +
		"\n  want …" + strings.ReplaceAll(want[lo:wHi], "\n", "⏎") + "…" +
		"\n   got …" + strings.ReplaceAll(got[lo:gHi], "\n", "⏎") + "…"
}
