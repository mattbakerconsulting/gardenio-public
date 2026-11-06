package main

// deck_step_tally_test.go — iter-84 (ka-0000005352): fold the per-status STEP
// BREAKDOWN tally ("▶ N in progress · ⛔ N blocked") + the calm "✓ steps done"
// ghost-done marker onto the /deck mobile card face. The /plans index card face
// renders both (plan_card.html: the .plan-step-tally pills, and the iter-41
// .plan-steps-done marker), but the /deck triage card carried only the "N/M
// steps · X%" progress facet (iter-67) — the HOW-FAR without the WHICH-
// DISPOSITION. So a 4/10 plan with 3 steps actively running read identically to
// one with 3 steps BLOCKED (the second needs a human), and a plan whose rendered
// steps are ALL closed but was never closed out read identically to a genuinely
// mid-flight plan.
//
// The facets ride the SAME enriched planRowView fields planIndexEnrich already
// sets (row.StepsInProgress / .StepsBlocked from tallyPlanGrid; row.StepsAllDone
// from the all-closed-but-non-terminal guard) and deckCards already builds from,
// so the fold costs the deck ZERO extra daemon call and agrees with the index +
// cockpit by construction (two-derivations-must-agree, advice ka-0000005924).

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestDeck_SurfacesStepTallyOnCardFace is the load-bearing assertion: the /deck
// card face carries the "▶ N" in-progress pill for a plan with a live step, so
// an operator flipping the triage deck sees WHICH disposition the open steps are
// in — not only HOW-FAR. seedDeckWithCrew hangs one in_progress step under the
// decision-tier plan, so the enriched row reads StepsInProgress=1, StepsBlocked=0.
func TestDeck_SurfacesStepTallyOnCardFace(t *testing.T) {
	body := getDeckAtNow(t, seedDeckWithCrew(), "/deck")

	for _, want := range []string{
		`class="deck-card-step-tally"`,                              // the rendered tally strip
		`deck-card-step-tally-item deck-card-step-tally-inprogress`, // the ▶ pill
		`&#x25b6; 1`, // one in-progress step
	} {
		if !strings.Contains(body, want) {
			t.Errorf("deck step-tally missing %q", want)
		}
	}
	// The single in_progress step means NO blocked pill and NO ghost-done marker.
	// Match the combined rendered class attribute (the ▶/⛔ pills share the
	// -item class), NOT the bare "-blocked" token — that also appears in the CSS
	// rule inside the <style> block.
	if strings.Contains(body, "deck-card-step-tally-item deck-card-step-tally-blocked") {
		t.Errorf("a plan with no blocked steps must render no ⛔ pill")
	}
	if strings.Contains(body, `class="deck-card-steps-done"`) {
		t.Errorf("a mid-flight plan (a live step) must render no ✓ steps-done marker")
	}
}

// TestDeck_RendersBlockedStepPill proves the ⛔ blocked pill renders when a step
// is blocked — the highest-value tally facet (a blocked step needs a human). A
// step child with Status="blocked" makes tallyPlanGrid count StepsBlocked=1.
func TestDeck_RendersBlockedStepPill(t *testing.T) {
	f := seedDeckBlockedStep()
	body := getDeckAtNow(t, f, "/deck")

	for _, want := range []string{
		`deck-card-step-tally-item deck-card-step-tally-blocked`,
		`&#x26d4; 1`, // one blocked step
	} {
		if !strings.Contains(body, want) {
			t.Errorf("deck blocked-step pill missing %q", want)
		}
	}
}

// TestDeck_RendersGhostDoneMarker proves the calm "✓ steps done" marker renders
// on a plan whose every rendered step is closed but whose epic is still non-
// terminal — and that the loud in-flight tally is mutually exclusive with it.
func TestDeck_RendersGhostDoneMarker(t *testing.T) {
	f := seedDeckAllStepsDone()
	body := getDeckAtNow(t, f, "/deck")

	if !strings.Contains(body, `class="deck-card-steps-done"`) {
		t.Errorf("an all-steps-closed non-terminal plan must render the ✓ steps-done marker")
	}
	if !strings.Contains(body, "&#x2713; steps done") {
		t.Errorf("ghost-done marker label missing")
	}
	// The ghost marker and the in-flight tally never both render.
	if strings.Contains(body, `class="deck-card-step-tally"`) {
		t.Errorf("an all-done plan has no in-flight/blocked steps — the ▶/⛔ tally must not render beside the ✓ marker")
	}
}

// TestDeck_NoTallyWhenNothingInFlight proves the tally is gated on
// StepsInProgress or StepsBlocked — a plan whose only rendered step is
// not-started (open, not in_progress/blocked) adds no ▶/⛔ ink.
func TestDeck_NoTallyWhenNothingInFlight(t *testing.T) {
	f := seedDeckOpenStep()
	body := getDeckAtNow(t, f, "/deck")

	// Sanity: the plan renders (it has a rendered step, so the progress facet is present).
	if !strings.Contains(body, `class="deck-card-steps"`) {
		t.Fatal("expected the step-progress facet to render for a plan with a rendered step")
	}
	if strings.Contains(body, `class="deck-card-step-tally"`) {
		t.Errorf("a plan with a single not-started step must render no ▶/⛔ tally")
	}
	if strings.Contains(body, `class="deck-card-steps-done"`) {
		t.Errorf("a plan with an open (not-closed) step is not all-done — no ✓ marker")
	}
}

// TestDeck_StepTallyAgreesWithIndexRollup proves the deck reads the SAME enriched
// tally the /plans index card does (no divergent derivation): the deckCardItem's
// StepsInProgress/StepsBlocked/StepsAllDone are exactly the enriched row's, so the
// two surfaces can never disagree.
func TestDeck_StepTallyAgreesWithIndexRollup(t *testing.T) {
	f := seedDeckWithCrew()
	s := testServerWithDaemon(f)
	s.SetNow(func() time.Time { return deckCrewNow })

	rows := s.planIndexEnrich(context.Background(), []*beadsapi.BeadDetail{
		f.beads["gd-decision"],
	})
	if len(rows) != 1 {
		t.Fatalf("expected 1 enriched row, got %d", len(rows))
	}
	items := deckCards(rows)
	if len(items) != 1 {
		t.Fatalf("expected 1 deck card, got %d", len(items))
	}
	got, want := items[0], rows[0]
	if got.StepsInProgress != want.StepsInProgress ||
		got.StepsBlocked != want.StepsBlocked ||
		got.StepsAllDone != want.StepsAllDone {
		t.Errorf("deck card tally (▶%d ⛔%d done=%v) diverged from enriched row (▶%d ⛔%d done=%v) — the two must be the SAME rollup",
			got.StepsInProgress, got.StepsBlocked, got.StepsAllDone,
			want.StepsInProgress, want.StepsBlocked, want.StepsAllDone)
	}
	if want.StepsInProgress != 1 || want.StepsBlocked != 0 {
		t.Errorf("fixture sanity: tally = ▶%d ⛔%d, want ▶1 ⛔0", want.StepsInProgress, want.StepsBlocked)
	}
}

// seedDeckBlockedStep hangs a single BLOCKED step under the decision-tier plan.
func seedDeckBlockedStep() *fakeDaemon {
	f := seedDeck()
	step := &beadsapi.BeadDetail{
		ID:        "gd-decstep-blk",
		Title:     "Awaiting a secret",
		Kind:      "issue",
		Type:      "task",
		Status:    "blocked",
		CreatedAt: deckCrewNow.Add(-48 * time.Hour),
		UpdatedAt: deckCrewNow.Add(-2 * time.Hour),
	}
	f.beads[step.ID] = step
	f.revDeps["gd-decision"] = append(f.revDeps["gd-decision"], beadsapi.Dependency{
		BeadID: step.ID, DependsOnID: "gd-decision", Type: "parent-child",
	})
	return f
}

// seedDeckAllStepsDone hangs a single CLOSED step under the (non-terminal)
// decision-tier plan, so the enriched row reads StepsAllDone=true.
func seedDeckAllStepsDone() *fakeDaemon {
	f := seedDeck()
	step := &beadsapi.BeadDetail{
		ID:        "gd-decstep-done",
		Title:     "Wired the cache",
		Kind:      "issue",
		Type:      "task",
		Status:    "closed",
		CreatedAt: deckCrewNow.Add(-48 * time.Hour),
		UpdatedAt: deckCrewNow.Add(-2 * time.Hour),
	}
	f.beads[step.ID] = step
	f.revDeps["gd-decision"] = append(f.revDeps["gd-decision"], beadsapi.Dependency{
		BeadID: step.ID, DependsOnID: "gd-decision", Type: "parent-child",
	})
	return f
}

// seedDeckOpenStep hangs a single OPEN (not-started) step — neither closed nor
// in_progress nor blocked, so no ▶/⛔ tally and no ✓ marker.
func seedDeckOpenStep() *fakeDaemon {
	f := seedDeck()
	step := &beadsapi.BeadDetail{
		ID:        "gd-decstep-open",
		Title:     "Not yet started",
		Kind:      "issue",
		Type:      "task",
		Status:    "open",
		CreatedAt: deckCrewNow.Add(-48 * time.Hour),
		UpdatedAt: deckCrewNow.Add(-2 * time.Hour),
	}
	f.beads[step.ID] = step
	f.revDeps["gd-decision"] = append(f.revDeps["gd-decision"], beadsapi.Dependency{
		BeadID: step.ID, DependsOnID: "gd-decision", Type: "parent-child",
	})
	return f
}
