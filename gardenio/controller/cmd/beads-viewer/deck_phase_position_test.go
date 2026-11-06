package main

// deck_phase_position_test.go — iter-90 (ka-0000005352): fold the PHASE-POSITION
// milestone ("· phase 2/2") onto the /deck triage card face. The /plans index
// card face carries it (plan_card.html, .plan-progress-phase, iter-58), but the
// /deck card carried only the "N/M steps · X%" progress facet (iter-67) + the
// per-status tally (iter-84) + the NOW/NEXT step chip (iter-83) — the HOW-FAR and
// the WHICH-STEP, but never WHICH PHASE of how many is in flight (the coarse
// lifecycle milestone the mission names).
//
// The facet rides the SAME enriched planRowView fields planIndexEnrich already
// sets (row.PhaseCur / row.PhaseTotal from planPhasePosition, a pure fold over the
// Grid already on the row) and deckCards already builds from — so the fold costs
// the deck ZERO extra daemon call and agrees with the index + cockpit by
// construction (two-derivations-must-agree, advice ka-0000005924). Gated on
// PhaseTotal>=2 (the index's own gate) so a single-phase/flat plan adds no ink.

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// seedDeckMultiPhase extends the deck fixture so its decision-tier plan
// (gd-decision — which the deck renders because it carries an open decision) also
// carries a real phase_order of two phase sub-epics: phase 1 fully closed, phase 2
// in flight. So the enriched row's planPhasePosition reads (2, 2) and the card
// must render "· phase 2/2". The step under phase 2 is in_progress, matching the
// phaseStatuses so phaseSquaresAllClosed advances the current phase exactly as in
// production.
func seedDeckMultiPhase() *fakeDaemon {
	f := seedDeck()
	plan := f.beads["gd-decision"]
	// Two phase sub-epics on the decision-tier plan.
	phaseIDs := []string{"gd-decph1", "gd-decph2"}
	plan.Fields["phase_order"] = `["gd-decph1","gd-decph2"]`
	phaseStatuses := []string{"closed", "in_progress"}
	for i, pid := range phaseIDs {
		phase := &beadsapi.BeadDetail{
			ID: pid, Title: "Phase", Kind: "issue", Type: "epic",
			Status:    "in_progress",
			CreatedAt: deckCrewNow.Add(-72 * time.Hour),
			UpdatedAt: deckCrewNow.Add(-2 * time.Hour),
		}
		f.beads[pid] = phase
		f.revDeps["gd-decision"] = append(f.revDeps["gd-decision"], beadsapi.Dependency{
			BeadID: pid, DependsOnID: "gd-decision", Type: "parent-child",
		})
		stepID := pid + "-step"
		f.beads[stepID] = &beadsapi.BeadDetail{
			ID: stepID, Title: "Step", Kind: "issue", Type: "task",
			Status:    phaseStatuses[i],
			CreatedAt: deckCrewNow.Add(-48 * time.Hour),
			UpdatedAt: deckCrewNow.Add(-2 * time.Hour),
		}
		f.revDeps[pid] = append(f.revDeps[pid], beadsapi.Dependency{
			BeadID: stepID, DependsOnID: pid, Type: "parent-child",
		})
	}
	return f
}

// TestDeck_SurfacesPhasePositionOnCardFace is the load-bearing assertion: the
// /deck card face carries the "· phase 2/2" milestone for a multi-phase plan, so
// an operator triaging on the deck sees WHICH phase is in flight — not only the
// task progress. Fail-without: dropping the field copy or the template gate reds
// this.
func TestDeck_SurfacesPhasePositionOnCardFace(t *testing.T) {
	body := getDeckAtNow(t, seedDeckMultiPhase(), "/deck")

	if !strings.Contains(body, "· phase 2/2") {
		t.Errorf("deck card missing the '· phase 2/2' milestone facet")
	}
	if !strings.Contains(body, `class="deck-card-phase"`) {
		t.Errorf("phase facet missing its .deck-card-phase class (rendered span)")
	}
}

// TestDeck_NoPhaseFacetForSinglePhase proves the facet self-suppresses on a plan
// with fewer than two phases — the base seedDeckWithCrew plan has a single flat
// step (no phase_order), so planPhasePosition returns (0, 0) and no facet renders.
func TestDeck_NoPhaseFacetForSinglePhase(t *testing.T) {
	body := getDeckAtNow(t, seedDeckWithCrew(), "/deck")

	// Sanity: the plan renders (it carries the step-progress facet).
	if !strings.Contains(body, `class="deck-card-steps"`) {
		t.Fatal("expected the step-progress facet to render for a plan with a rendered step")
	}
	// The <style> block defines .deck-card-phase; match the rendered SPAN
	// (class="deck-card-phase") rather than the bare class name in the CSS.
	if strings.Contains(body, `class="deck-card-phase"`) {
		t.Errorf("a single-phase/flat plan must render no phase-position facet")
	}
}

// TestDeck_PhasePositionAgreesWithIndexRollup proves the deck reads the SAME
// enriched planPhasePosition the /plans index card does (no divergent derivation):
// the deckCardItem's PhaseCur/PhaseTotal are exactly the enriched row's, so the two
// surfaces can never disagree.
func TestDeck_PhasePositionAgreesWithIndexRollup(t *testing.T) {
	f := seedDeckMultiPhase()
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
	if got.PhaseCur != want.PhaseCur || got.PhaseTotal != want.PhaseTotal {
		t.Errorf("deck card phase (%d/%d) diverged from enriched row (%d/%d) — the two must be the SAME derivation",
			got.PhaseCur, got.PhaseTotal, want.PhaseCur, want.PhaseTotal)
	}
	if want.PhaseCur != 2 || want.PhaseTotal != 2 {
		t.Errorf("fixture sanity: phase = %d/%d, want 2/2", want.PhaseCur, want.PhaseTotal)
	}
}
