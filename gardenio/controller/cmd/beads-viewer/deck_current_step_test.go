package main

// deck_current_step_test.go — iter-83 (ka-0000005352): surface the current-or-
// next STEP chip ("NOW/NEXT #N <title>") on the /deck mobile card face. The
// /plans index card face carries it (plan_card.html, CurrentOrNextStep) but the
// /deck triage card DROPPED it — the deck named how far along a plan is
// ("N/M steps · X%", iter-67) and WHO is on it (crew tally, iter-66/80/81) but
// not WHICH step is in flight, the single most actionable "what is happening
// right now" signal an operator triaging the deck needs.
//
// The chip rides the SAME row.CurrentOrNextStep the enriched planRowView already
// carries (planIndexEnrich picked + planStatusTerminal-guarded it — the same
// projection the index card renders), so it costs the inline deck render ZERO
// extra daemon call and agrees with the index by construction (advice
// ka-0000005924). Scoped to the inline stack card (deck.html); the lean answer-
// in-place fragment (handlePlanCard / renderDeckCard) is deliberately untouched.

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestDeck_SurfacesCurrentStepOnCardFace is the load-bearing assertion: the
// /deck card face carries the "NOW/NEXT #N <title>" chip, so WHICH step is in
// flight is visible while flipping the triage deck — not only in the index +
// cockpit. seedDeckWithCrew hangs one in_progress step ("Wire the cache") under
// the decision-tier plan, so the pick reads Kind "now" (in_progress), #1.
func TestDeck_SurfacesCurrentStepOnCardFace(t *testing.T) {
	body := getDeckAtNow(t, seedDeckWithCrew(), "/deck")

	for _, want := range []string{
		`class="deck-card-step deck-card-step-now`,    // rendered anchor, "now" kind (in_progress step)
		`deck-card-step-kind deck-card-step-kind-now`, // the NOW kind pill
		`>#1<`,                                // the step number
		`Wire the cache`,                      // the step title
		`/plans/gd-decision#step-gd-decstep1`, // deep-links into the cockpit, anchored at the step
	} {
		if !strings.Contains(body, want) {
			t.Errorf("deck current-step chip missing %q", want)
		}
	}

	// The chip must be a non-swipe anchor — a tap that starts on it opens the step
	// rather than being read as a drag that could commit an answer (the same guard
	// the crew tally + every deck link carries).
	stepStart := strings.Index(body, `class="deck-card-step deck-card-step-`)
	if stepStart < 0 {
		t.Fatal("deck step anchor not found")
	}
	tagEnd := strings.Index(body[stepStart:], ">")
	if tagEnd < 0 || !strings.Contains(body[stepStart:stepStart+tagEnd], "data-deck-no-swipe") {
		t.Errorf("deck step anchor must carry data-deck-no-swipe so a tap never reads as a swipe")
	}
}

// TestDeck_SteplessPlanRendersNoStepChip proves the chip is gated on a non-nil
// CurrentStep with an ID — a plan with no rendered step adds no ink, keeping the
// card lean. seedDeck's tiers have only a decision child (no step), so the pick
// returns nil.
func TestDeck_SteplessPlanRendersNoStepChip(t *testing.T) {
	body := getDeckAtNow(t, seedDeck(), "/deck")

	// The deck renders (the ready-tier card is present)...
	if !strings.Contains(body, "Plan ready to build") {
		t.Fatal("expected the ready-tier card to render")
	}
	// ...but no step chip anchor appears anywhere, because no plan in the base
	// fixture has a rendered step. Match the rendered ANCHOR (class="deck-card-step
	// deck-card-step-) rather than the CSS rules in the <style> block.
	if strings.Contains(body, `class="deck-card-step deck-card-step-`) {
		t.Errorf("a plan with no step must render no step chip — the .ID gate leaked ink")
	}
}

// TestDeck_CurrentStepAgreesWithIndexRollup proves the deck reads the SAME
// enriched row.CurrentOrNextStep the /plans index card does (no divergent
// derivation): the deckCardItem's CurrentStep is exactly the enriched row's, so
// the two surfaces can never disagree on which step is in flight.
func TestDeck_CurrentStepAgreesWithIndexRollup(t *testing.T) {
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
	got, want := items[0].CurrentStep, rows[0].CurrentOrNextStep
	if want == nil {
		t.Fatal("fixture sanity: the in_progress step must be picked as CurrentOrNextStep, got nil")
	}
	if got == nil {
		t.Fatal("deck card dropped the step the enriched row carries — the two must be the SAME pointer")
	}
	if got != want {
		t.Errorf("deck card CurrentStep (%p) diverged from enriched row CurrentOrNextStep (%p) — the two must be the SAME projection", got, want)
	}
	if got.ID != "gd-decstep1" || got.Kind() != "now" || got.Number != 1 {
		t.Errorf("fixture sanity: picked step = id=%q kind=%q #%d, want id=gd-decstep1 kind=now #1", got.ID, got.Kind(), got.Number)
	}
}
