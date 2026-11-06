package main

// deck_steps_test.go — iter-67 (ka-0000005352): surface the plan's STEP PROGRESS
// ("N/M steps · X%") on the /deck mobile card face. The /plans index card + the
// cockpit both render the step-progress, but the /deck triage card face showed
// NOTHING about how far along the plan is — so a decision on a barely-begun plan
// (0%) read identically to one on a nearly-finished plan (90%), even though "how
// much is already built" is exactly the context that colors the answer.
//
// The facet rides the SAME StepsClosed/StepsTotal/PctDone the enriched
// planRowView already carries (planIndexEnrich's renderedStepRollup — the same
// numbers the index card renders), so it costs the inline deck render ZERO extra
// daemon call and agrees with the index + cockpit by construction. Scoped to the
// inline stack card (deck.html); the lean answer-in-place fragment
// (handlePlanCard / renderDeckCard) is deliberately untouched.

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestDeck_SurfacesStepProgressOnCardFace is the load-bearing assertion: the
// /deck card face carries the "N/M steps · X%" facet, so a plan's progress is
// visible while flipping the triage deck — not only in the index + cockpit.
// seedDeckWithCrew hangs one in_progress step under the decision-tier plan, so
// the enriched row reads StepsTotal=1, StepsClosed=0, PctDone=0.
func TestDeck_SurfacesStepProgressOnCardFace(t *testing.T) {
	body := getDeckAtNow(t, seedDeckWithCrew(), "/deck")

	for _, want := range []string{
		`class="deck-card-steps"`, // the rendered facet span
		`0/1 steps · 0%`,          // one open step, none closed
	} {
		if !strings.Contains(body, want) {
			t.Errorf("deck step-progress facet missing %q", want)
		}
	}
}

// TestDeck_NoStepsPlanRendersNoProgress proves the facet is gated on
// StepsTotal>0 — a plan with no rendered steps adds no ink, keeping the card
// lean. seedDeck's decision-tier plan has only a decision child (no step), so
// its rollup is StepsTotal=0.
func TestDeck_NoStepsPlanRendersNoProgress(t *testing.T) {
	body := getDeckAtNow(t, seedDeck(), "/deck")

	// The deck renders (the ready-tier card is present)...
	if !strings.Contains(body, "Plan ready to build") {
		t.Fatal("expected the ready-tier card to render")
	}
	// ...but no step-progress span appears anywhere, because no plan in the base
	// fixture has a rendered step. Match the rendered span (class="deck-card-steps")
	// rather than the CSS rule in the <style> block.
	if strings.Contains(body, `class="deck-card-steps"`) {
		t.Errorf("a plan with no steps must render no progress facet — StepsTotal>0 gate leaked ink")
	}
}

// TestDeck_StepProgressAgreesWithIndexRollup proves the deck reads the SAME
// enriched rollup the /plans index card does (no divergent derivation): the
// deckCardItem's step counts are exactly the enriched row's, so the two
// surfaces can never disagree on the progress.
func TestDeck_StepProgressAgreesWithIndexRollup(t *testing.T) {
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
	if got.StepsClosed != want.StepsClosed || got.StepsTotal != want.StepsTotal || got.PctDone != want.PctDone {
		t.Errorf("deck card progress %d/%d %d%% diverged from enriched row %d/%d %d%% — the two must be the SAME rollup",
			got.StepsClosed, got.StepsTotal, got.PctDone, want.StepsClosed, want.StepsTotal, want.PctDone)
	}
	if want.StepsTotal != 1 || want.StepsClosed != 0 {
		t.Errorf("fixture sanity: rollup = %d/%d closed/total, want 1 total, 0 closed", want.StepsClosed, want.StepsTotal)
	}
}
