package main

// deck_crew_test.go — iter-66 (ka-0000005352): surface the live-crew tally on
// the /deck mobile card face. The mission is "surface live crew on EACH card,
// not just the cockpit" — the /plans index chip (ka-0000004801) and the peek
// drawer (iter-65) both carry the crew tally, but the /deck triage card showed
// ZERO crew, so an operator flipping the deck could answer a plan's question
// without ever seeing that crew are already working (or stuck) on it.
//
// The tally rides the SAME reconciled planCrewRollup the index chip renders,
// carried on the enriched planRowView the deck already builds (planIndexEnrich
// populates row.Crew) — so it costs the inline deck render ZERO extra daemon
// call, and its counts agree with the index chip by construction. Scoped to the
// inline stack card (deck.html); the lean answer-in-place fragment
// (handlePlanCard / renderDeckCard) is deliberately untouched so its
// 2-round-trip budget (deckCardRoundTripBudget) holds.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// deckCrewNow pins the clock so a seeded working crew reads FRESH (live) under
// the agentStaleThreshold freshness gate — the same discipline crewFixtureNow
// uses. A crew stamped now−5m is comfortably alive.
var deckCrewNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

// seedDeckWithCrew extends the deck fixture with a step under the decision-tier
// plan and TWO crew agents dispatched onto it — one working (live), one done —
// so the enriched row carries a real crew rollup the deck card must render.
func seedDeckWithCrew() *fakeDaemon {
	f := seedDeck()

	// A step child of the decision-tier plan, so planIndexEnrich's step walk finds
	// it and the crew-bulk join hangs crew off it.
	step := &beadsapi.BeadDetail{
		ID:        "gd-decstep1",
		Title:     "Wire the cache",
		Kind:      "issue",
		Type:      "task",
		Status:    "in_progress",
		CreatedAt: deckCrewNow.Add(-48 * time.Hour),
		UpdatedAt: deckCrewNow.Add(-2 * time.Hour),
	}
	f.beads[step.ID] = step
	// The plan → step parent-child edge (reverse dep of the epic finds the step).
	f.revDeps["gd-decision"] = append(f.revDeps["gd-decision"], beadsapi.Dependency{
		BeadID: step.ID, DependsOnID: "gd-decision", Type: "parent-child",
	})

	// Two crew agents: one working (live), one finished — a MIXED roster, exactly
	// the case where "N crew · M working · K done" adds signal.
	mkCrew := func(id, name, state, status string) {
		b := &beadsapi.BeadDetail{
			ID:     id,
			Title:  name,
			Kind:   "config",
			Type:   "agent",
			Status: status,
			Fields: map[string]string{
				"agent": name, "role": "crew", "agent_state": state, "task_id": step.ID,
			},
			CreatedAt: deckCrewNow.Add(-time.Hour),
			UpdatedAt: deckCrewNow.Add(-5 * time.Minute),
		}
		f.beads[id] = b
		f.revDeps[step.ID] = append(f.revDeps[step.ID], beadsapi.Dependency{
			BeadID: id, DependsOnID: step.ID, Type: "assigned",
		})
	}
	mkCrew("gd-deckcrewA", "gardenio-wire-cache-abc", "working", "in_progress")
	mkCrew("gd-deckcrewB", "gardenio-wire-cache-xyz", "done", "closed")

	return f
}

func getDeckAtNow(t *testing.T, f *fakeDaemon, target string) string {
	t.Helper()
	mux := mkServerWithNow(t, f, func() time.Time { return deckCrewNow })
	req := httptest.NewRequest("GET", target, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200", target, w.Code)
	}
	return w.Body.String()
}

// TestDeck_SurfacesLiveCrewOnCardFace is the load-bearing assertion: the /deck
// card face carries the crew tally, the live-dot, and the working/done facets,
// so a plan's crew is visible while flipping the triage deck — not only in the
// cockpit.
func TestDeck_SurfacesLiveCrewOnCardFace(t *testing.T) {
	body := getDeckAtNow(t, seedDeckWithCrew(), "/deck")

	for _, want := range []string{
		`class="deck-card-crew deck-card-crew-live`, // rendered anchor, live class set by a working crew
		`deck-card-crew-dot`,                        // the green live-dot
		`2 crew`,                                    // total
		`1 working`,                                 // the live facet
		`1 done`,                                    // the mixed-roster done facet
		`/plans/gd-decision#plan-crew`,              // deep-links to the cockpit roster
	} {
		if !strings.Contains(body, want) {
			t.Errorf("deck crew tally missing %q", want)
		}
	}

	// The crew line must be a non-swipe anchor — a tap that starts on it opens the
	// roster rather than being read as a drag that could commit an answer.
	crewStart := strings.Index(body, `class="deck-card-crew`)
	if crewStart < 0 {
		t.Fatal("deck crew anchor not found")
	}
	tagEnd := strings.Index(body[crewStart:], ">")
	if tagEnd < 0 || !strings.Contains(body[crewStart:crewStart+tagEnd], "data-deck-no-swipe") {
		t.Errorf("deck crew anchor must carry data-deck-no-swipe so a tap never reads as a swipe")
	}
}

// TestDeck_CrewlessPlanRendersNoCrewLine proves the tally is gated on Crew.Any —
// a plan with no dispatched crew adds no ink, keeping the card lean.
func TestDeck_CrewlessPlanRendersNoCrewLine(t *testing.T) {
	// seedDeck's ready-tier plan (gd-ready) has no crew.
	body := getDeckAtNow(t, seedDeck(), "/deck")

	// The ready card renders, but no crew tally class appears anywhere (no plan in
	// the base fixture has crew).
	if !strings.Contains(body, "Plan ready to build") {
		t.Fatal("expected the ready-tier card to render")
	}
	// The <style> block defines .deck-card-crew, so match the rendered ANCHOR
	// (class="deck-card-crew...) rather than the bare class name.
	if strings.Contains(body, `class="deck-card-crew`) {
		t.Errorf("a crew-less deck must render no crew tally — Crew.Any gate leaked ink")
	}
}

// TestDeck_CrewTallyAgreesWithIndexRollup proves the deck reads the SAME
// reconciled rollup the /plans index chip does (no divergent derivation): the
// deckCardItem's Crew is exactly the enriched row's Crew, so the two surfaces
// can never disagree on the counts.
func TestDeck_CrewTallyAgreesWithIndexRollup(t *testing.T) {
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
	got, want := items[0].Crew, rows[0].Crew
	if got.Total != want.Total || got.Working != want.Working || got.Done != want.Done || got.Idle != want.Idle {
		t.Errorf("deck card crew %+v diverged from enriched row crew %+v — the two must be the SAME rollup", got, want)
	}
	if want.Total != 2 || want.Working != 1 || want.Done != 1 {
		t.Errorf("fixture sanity: rollup = %d/%d working/done, want 2 total, 1 working, 1 done", want.Total, want.Working)
	}
}

// TestDeck_WorkingFacetCarriesRuntimeAge proves the /deck card's "· M working"
// facet now folds the longest-running live crew's RUNTIME age ("· 1h") — the
// deck twin of the index card's plan-card-crew-working-age facet and the cockpit
// roster's chip (MR !7217), closing the last deck↔index↔cockpit working-facet
// parity gap. The seeded working crew was CreatedAt deckCrewNow−1h, so its
// runtime reads "1h" (aged on CreatedAt, not the fresh −5m heartbeat, so a
// heartbeating crew does not read ~0s). The age rides the SAME
// deckCardItem.Crew planCrewRollup the tally reads (WorkingOldestAge()), so it
// costs the deck ZERO extra fetch and agrees with the index + cockpit by
// construction.
func TestDeck_WorkingFacetCarriesRuntimeAge(t *testing.T) {
	f := seedDeckWithCrew()

	// Two-derivations invariant: what the rollup reports is what the face must show.
	s := testServerWithDaemon(f)
	s.SetNow(func() time.Time { return deckCrewNow })
	rows := s.planIndexEnrich(context.Background(), []*beadsapi.BeadDetail{f.beads["gd-decision"]})
	if len(rows) != 1 {
		t.Fatalf("expected 1 enriched row, got %d", len(rows))
	}
	wantAge := rows[0].Crew.WorkingOldestAge()
	if wantAge == "" {
		t.Fatal("fixture sanity: expected the working crew to carry a derivable runtime age")
	}

	body := getDeckAtNow(t, f, "/deck")

	// The runtime age must render inside the working-age span on the deck face.
	ageSpan := `<span class="deck-card-crew-working-age">` + wantAge + `</span>`
	if !strings.Contains(body, ageSpan) {
		t.Errorf("deck working facet missing the runtime age span %q — the deck↔index↔cockpit working-facet parity gap is not closed", ageSpan)
	}
}

// seedDeckWithCrewSpend extends the crew fixture with a known dollar cost on each
// crew agent so the rollup carries a derivable cumulative spend — the working
// crew is live so its live_cost_usd counts too (agentSpendUSD live semantics).
func seedDeckWithCrewSpend() *fakeDaemon {
	f := seedDeckWithCrew()
	// Working (live) crew: $2.00 total + $0.50 live = $2.50 counted.
	f.beads["gd-deckcrewA"].Fields["total_cost_usd"] = "2.00"
	f.beads["gd-deckcrewA"].Fields["live_cost_usd"] = "0.50"
	// Done crew: $1.75 total (not live, so no live_cost_usd fold).
	f.beads["gd-deckcrewB"].Fields["total_cost_usd"] = "1.75"
	return f
}

// TestDeck_CrewFaceCarriesSpend proves the /deck card's crew face now folds the
// cumulative crew SPEND ("· $N") facet (ka-0000005352 iter-77) — the deck twin of
// the index card's plan-card-crew-spend facet (plan_card.html, iter-12) and the
// cockpit roster's plan-crew-count-spend chip (plan_crew.html). The deck rendered
// working/queued/done/stuck on the face but HID the cumulative spend in the
// tooltip only, and the /deck is a standalone triage card with NO crew roster
// below it — so an operator triaging the deck could not see how much budget a
// plan had burned without hovering. The spend rides the SAME deckCardItem.Crew
// planCrewRollup (HasSpend()/SpendLabel()) the tally reads, so it costs the deck
// ZERO extra fetch and agrees with the index + cockpit by construction.
func TestDeck_CrewFaceCarriesSpend(t *testing.T) {
	f := seedDeckWithCrewSpend()

	// Two-derivations invariant: what the rollup reports is what the face must show.
	s := testServerWithDaemon(f)
	s.SetNow(func() time.Time { return deckCrewNow })
	rows := s.planIndexEnrich(context.Background(), []*beadsapi.BeadDetail{f.beads["gd-decision"]})
	if len(rows) != 1 {
		t.Fatalf("expected 1 enriched row, got %d", len(rows))
	}
	if !rows[0].Crew.HasSpend() {
		t.Fatal("fixture sanity: expected the crew rollup to carry a derivable spend")
	}
	wantLabel := rows[0].Crew.SpendLabel()
	if wantLabel != "$4.25" {
		t.Fatalf("fixture sanity: rollup spend = %q, want $4.25 (2.00+0.50 live + 1.75)", wantLabel)
	}

	body := getDeckAtNow(t, f, "/deck")

	// The spend must render inside the spend span on the deck face.
	spendSpan := `<span class="deck-card-crew-spend">· ` + wantLabel + `</span>`
	if !strings.Contains(body, spendSpan) {
		t.Errorf("deck crew face missing the spend span %q — the deck↔index↔cockpit crew-spend parity gap is not closed", spendSpan)
	}
}

// TestDeck_CrewFaceNoSpendWhenUnknown is the fail-without floor: a crew roster
// with NO reported cost renders no spend facet at all (HasSpend gate), so a plan
// whose crew have not yet reported cost reads exactly as before.
func TestDeck_CrewFaceNoSpendWhenUnknown(t *testing.T) {
	f := seedDeckWithCrew() // no cost fields stamped

	s := testServerWithDaemon(f)
	s.SetNow(func() time.Time { return deckCrewNow })
	rows := s.planIndexEnrich(context.Background(), []*beadsapi.BeadDetail{f.beads["gd-decision"]})
	if len(rows) != 1 {
		t.Fatalf("expected 1 enriched row, got %d", len(rows))
	}
	if rows[0].Crew.HasSpend() {
		t.Fatal("fixture sanity: expected NO derivable spend for the cost-less crew fixture")
	}

	body := getDeckAtNow(t, f, "/deck")

	if strings.Contains(body, `class="deck-card-crew-spend"`) {
		t.Error("deck crew face rendered a spend facet for a crew roster with no reported cost — the HasSpend gate is not holding")
	}
}

// seedDeckWithQueuedStuckCrew extends the deck fixture with a step under the
// decision-tier plan and TWO OPEN crew dispatched onto it: one QUEUED (idle
// agent_state, waiting on a pod slot) and one STUCK (rate_limited, reachable) —
// both stamped with a last-activity heartbeat OLDER than agentStaleThreshold (1h)
// so each carries a derivable wait-age AND trips its stale bar. This is exactly
// the case where the deck's "· N queued <age>" / "· ⚠ N <age>" wait-age facets
// (ka-0000005352 iter-80) add signal: a pool-starved / long-wedged plan an
// operator triaging the deck could not distinguish from a fresh one before.
func seedDeckWithQueuedStuckCrew() *fakeDaemon {
	f := seedDeck()

	step := &beadsapi.BeadDetail{
		ID:        "gd-decstepQS",
		Title:     "Ship the widget",
		Kind:      "issue",
		Type:      "task",
		Status:    "in_progress",
		CreatedAt: deckCrewNow.Add(-48 * time.Hour),
		UpdatedAt: deckCrewNow.Add(-3 * time.Hour),
	}
	f.beads[step.ID] = step
	f.revDeps["gd-decision"] = append(f.revDeps["gd-decision"], beadsapi.Dependency{
		BeadID: step.ID, DependsOnID: "gd-decision", Type: "parent-child",
	})

	// mkCrewAt lets each crew carry its OWN last-activity (updated_at) so the
	// wait-age reads a controlled, stale value (> the 1h agentStaleThreshold).
	mkCrewAt := func(id, name, state, status string, updated time.Time) {
		b := &beadsapi.BeadDetail{
			ID:     id,
			Title:  name,
			Kind:   "config",
			Type:   "agent",
			Status: status,
			Fields: map[string]string{
				"agent": name, "role": "crew", "agent_state": state, "task_id": step.ID,
			},
			CreatedAt: deckCrewNow.Add(-4 * time.Hour),
			UpdatedAt: updated,
		}
		f.beads[id] = b
		f.revDeps[step.ID] = append(f.revDeps[step.ID], beadsapi.Dependency{
			BeadID: id, DependsOnID: step.ID, Type: "assigned",
		})
	}
	// Queued (idle) crew stalled 2h ago — past the 1h queued-stale bar. An idle
	// agent_state is NOT re-bucketed by a stale heartbeat (only alive states are),
	// so it stays in the queued disposition while its wait-age reads loud.
	mkCrewAt("gd-deckcrewQ", "gardenio-ship-widget-q1", "idle", "in_progress", deckCrewNow.Add(-2*time.Hour))
	// Stuck (rate_limited) crew wedged 6h ago — reachable (open) + past the stuck
	// stale bar, so the loud "· ⚠ 1 <age>" facet fires with the stale tint.
	mkCrewAt("gd-deckcrewS", "gardenio-ship-widget-s1", "rate_limited", "in_progress", deckCrewNow.Add(-6*time.Hour))

	return f
}

// TestDeck_QueuedFacetCarriesWaitAge proves the /deck card's "· N queued" facet
// now folds the oldest queued crew's wait-age + the pool-starved stale tint
// (ka-0000005352 iter-80) — the deck twin of the index card's
// plan-card-crew-queued-age / -queued-stale spans (plan_card.html iter-32) and
// the cockpit roster chip. The facet was COUNT-ONLY on the deck, so a crew queued
// 2m (the pool will slot it) read identically to one queued hours (the pool is
// starved). The age rides the SAME deckCardItem.Crew planCrewRollup
// (QueuedOldestAge/QueuedOldestStale) the tally reads — zero Go, zero extra fetch
// — so it agrees with the index + cockpit by construction.
func TestDeck_QueuedFacetCarriesWaitAge(t *testing.T) {
	f := seedDeckWithQueuedStuckCrew()

	// Two-derivations invariant: what the rollup reports is what the face must show.
	s := testServerWithDaemon(f)
	s.SetNow(func() time.Time { return deckCrewNow })
	rows := s.planIndexEnrich(context.Background(), []*beadsapi.BeadDetail{f.beads["gd-decision"]})
	if len(rows) != 1 {
		t.Fatalf("expected 1 enriched row, got %d", len(rows))
	}
	wantAge := rows[0].Crew.QueuedOldestAge()
	if wantAge == "" {
		t.Fatal("fixture sanity: expected the queued crew to carry a derivable wait-age")
	}
	if !rows[0].Crew.QueuedOldestStale() {
		t.Fatal("fixture sanity: expected the 2h-stalled queued crew to be past the pool-starved stale bar")
	}
	if rows[0].Crew.Idle != 1 {
		t.Fatalf("fixture sanity: expected 1 queued crew, got Idle=%d", rows[0].Crew.Idle)
	}

	body := getDeckAtNow(t, f, "/deck")

	// The queued wait-age must render inside the queued-age span on the deck face.
	ageSpan := `<span class="deck-card-crew-queued-age">` + wantAge + `</span>`
	if !strings.Contains(body, ageSpan) {
		t.Errorf("deck queued facet missing the wait-age span %q — the deck↔index↔cockpit queued-facet parity gap is not closed", ageSpan)
	}
	// A stale (pool-starved) queued crew tints the whole facet loud.
	if !strings.Contains(body, `class="deck-card-crew-queued deck-card-crew-queued-stale"`) {
		t.Error("deck queued facet missing the loud stale tint for a pool-starved (past-threshold) queued crew")
	}
}

// TestDeck_StuckFacetCarriesWaitAge proves the /deck card's "· ⚠ N" stuck facet
// now folds the oldest OPEN stuck crew's wait-age + the loud stale tint
// (ka-0000005352 iter-80) — the deck twin of the index card's
// plan-card-crew-stuck-age / -stuck-stale spans (plan_card.html iter-13/29). The
// facet was COUNT-ONLY on the deck, so a crew wedged 6m read identically to one
// wedged days. The age rides the SAME deckCardItem.Crew planCrewRollup
// (StuckOldestAge/StuckOldestStale) the tally reads — zero Go, zero extra fetch.
func TestDeck_StuckFacetCarriesWaitAge(t *testing.T) {
	f := seedDeckWithQueuedStuckCrew()

	s := testServerWithDaemon(f)
	s.SetNow(func() time.Time { return deckCrewNow })
	rows := s.planIndexEnrich(context.Background(), []*beadsapi.BeadDetail{f.beads["gd-decision"]})
	if len(rows) != 1 {
		t.Fatalf("expected 1 enriched row, got %d", len(rows))
	}
	wantAge := rows[0].Crew.StuckOldestAge()
	if wantAge == "" {
		t.Fatal("fixture sanity: expected the stuck crew to carry a derivable wait-age")
	}
	if !rows[0].Crew.StuckOldestStale() {
		t.Fatal("fixture sanity: expected the 6h-wedged stuck crew to be past the stale bar")
	}
	if rows[0].Crew.OpenStuck() != 1 {
		t.Fatalf("fixture sanity: expected 1 open stuck crew, got OpenStuck=%d", rows[0].Crew.OpenStuck())
	}

	body := getDeckAtNow(t, f, "/deck")

	// The stuck wait-age must render inside the stuck-age span on the deck face.
	ageSpan := `<span class="deck-card-crew-stuck-age">` + wantAge + `</span>`
	if !strings.Contains(body, ageSpan) {
		t.Errorf("deck stuck facet missing the wait-age span %q — the deck↔index↔cockpit stuck-facet parity gap is not closed", ageSpan)
	}
	// A stale (long-wedged) stuck crew tints the whole facet loud.
	if !strings.Contains(body, `deck-card-crew-stuck deck-card-crew-stuck-stale`) {
		t.Error("deck stuck facet missing the loud stale tint for a long-wedged (past-threshold) stuck crew")
	}
}

// TestDeck_QueuedStuckFacetsNoAgeWhenUndatable is the fail-without floor: a queued
// / stuck crew with NO last-activity stamp (zero UpdatedAt) contributes no age, so
// each facet reads count-only exactly as before iter-80 — the age spans and the
// stale tints self-suppress rather than rendering an empty or false-loud facet.
func TestDeck_QueuedStuckFacetsNoAgeWhenUndatable(t *testing.T) {
	f := seedDeckWithQueuedStuckCrew()
	// Wipe the heartbeats so neither open crew is datable.
	f.beads["gd-deckcrewQ"].UpdatedAt = time.Time{}
	f.beads["gd-deckcrewS"].UpdatedAt = time.Time{}

	s := testServerWithDaemon(f)
	s.SetNow(func() time.Time { return deckCrewNow })
	rows := s.planIndexEnrich(context.Background(), []*beadsapi.BeadDetail{f.beads["gd-decision"]})
	if len(rows) != 1 {
		t.Fatalf("expected 1 enriched row, got %d", len(rows))
	}
	// A wiped heartbeat leaves an idle crew queued (idle is not re-bucketed on a
	// stale heartbeat) but with no derivable age; the rate_limited crew stays stuck.
	if rows[0].Crew.QueuedOldestAge() != "" {
		t.Fatal("fixture sanity: expected NO derivable queued wait-age for the undated crew")
	}
	if rows[0].Crew.StuckOldestAge() != "" {
		t.Fatal("fixture sanity: expected NO derivable stuck wait-age for the undated crew")
	}

	body := getDeckAtNow(t, f, "/deck")

	if strings.Contains(body, `class="deck-card-crew-queued-age"`) {
		t.Error("deck rendered a queued wait-age span for an undatable queued crew — the QueuedOldestAge gate leaked")
	}
	if strings.Contains(body, `class="deck-card-crew-stuck-age"`) {
		t.Error("deck rendered a stuck wait-age span for an undatable stuck crew — the StuckOldestAge gate leaked")
	}
	// Match the APPLIED class combos (not the bare stale class, which also appears
	// in the <style> block's CSS rules) so this asserts on the rendered facet.
	if strings.Contains(body, `class="deck-card-crew-queued deck-card-crew-queued-stale"`) ||
		strings.Contains(body, `deck-card-crew-stuck deck-card-crew-stuck-stale`) {
		t.Error("deck rendered a stale tint for an undatable crew — the stale gate must self-suppress with the age")
	}
}

// --- iter-81 (gd-AZe4HlZO80): live-crew click-through FACE strips on the deck ---
//
// The deck carried the crew TALLY (counts) since iter-66 but no click-through
// FACES — the avatar strips the index card renders (plan_card.html working/stuck/
// queued, ka-0000002189/yjwZ60U0fi/iter-53). On the /deck — a standalone mobile
// triage surface with NO crew roster below and NO hover — WHICH crew is stuck (and
// the terminal to unstick it) was unreachable: only the count. These fold the SAME
// .Faces / .StuckFaces / .QueuedFaces already on deckCardItem.Crew (planCrewRollup)
// into the card face, so deck↔index agree by construction — template + CSS only.

// TestDeck_RendersWorkingCrewFace is the load-bearing assertion: a working crew
// gets a green click-through avatar on the deck face, carrying its initials, the
// step "#N" tag, the terminal href, and data-deck-no-swipe (a tap that starts on
// the avatar opens the terminal, never a card swipe).
func TestDeck_RendersWorkingCrewFace(t *testing.T) {
	body := getDeckAtNow(t, seedDeckWithCrew(), "/deck")

	// The working face strip container.
	if !strings.Contains(body, `<span class="deck-card-crew-faces" role="group"`) {
		t.Fatal("deck did not render the working crew face strip")
	}
	// The seeded working crew gd-deckcrewA (GWCA) → a green face linking to its
	// bead page, tagged with its step #1, and non-swipe.
	strip := body[strings.Index(body, `<span class="deck-card-crew-faces" role="group"`):]
	strip = strip[:strings.Index(strip, "</span>")+len("</span>")]
	for _, want := range []string{
		`class="deck-card-crew-face"`, // green working avatar class
		`data-deck-no-swipe`,          // a tap opens the terminal, not a swipe
		`href="/gd-deckcrewA"`,        // click-through to the crew's page
		`>GWCA<`,                      // its initials
		`deck-card-crew-face-step`,    // the "#N" step tag
		`>#1<`,                        // the resolved step number
	} {
		if !strings.Contains(strip, want) {
			t.Errorf("deck working face strip missing %q", want)
		}
	}
}

// TestDeck_RendersStuckCrewFace proves the RED stuck avatar — the crew an operator
// most needs to reach on a mobile triage surface — renders a click-through face,
// distinct from the working strip, so a stuck crew is reachable from the deck.
func TestDeck_RendersStuckCrewFace(t *testing.T) {
	body := getDeckAtNow(t, seedDeckWithQueuedStuckCrew(), "/deck")

	if !strings.Contains(body, `deck-card-crew-faces-stuck`) {
		t.Fatal("deck did not render the stuck crew face strip")
	}
	if !strings.Contains(body, `class="deck-card-crew-face deck-card-crew-face-stuck"`) {
		t.Error("deck stuck face missing the red deck-card-crew-face-stuck class")
	}
	// The stuck crew gd-deckcrewS → its face links to its own page and is non-swipe.
	if !strings.Contains(body, `href="/gd-deckcrewS"`) {
		t.Error("deck stuck face missing the click-through to the stuck crew's page")
	}
	// The stuck-strip tooltip names the unstick action.
	if !strings.Contains(body, `stuck (rate-limited / failing)`) {
		t.Error("deck stuck face missing the stuck-reason tooltip")
	}
}

// TestDeck_RendersQueuedCrewFace proves the CALM slate queued avatar renders a
// click-through face — the last crew disposition the deck face carried a count for
// but no way to reach the specific pod.
func TestDeck_RendersQueuedCrewFace(t *testing.T) {
	body := getDeckAtNow(t, seedDeckWithQueuedStuckCrew(), "/deck")

	if !strings.Contains(body, `deck-card-crew-faces-queued`) {
		t.Fatal("deck did not render the queued crew face strip")
	}
	if !strings.Contains(body, `class="deck-card-crew-face deck-card-crew-face-queued"`) {
		t.Error("deck queued face missing the slate deck-card-crew-face-queued class")
	}
	if !strings.Contains(body, `href="/gd-deckcrewQ"`) {
		t.Error("deck queued face missing the click-through to the queued crew's page")
	}
}

// TestDeck_CrewlessPlanRendersNoFaces is the fail-without floor: a plan with no
// dispatched crew adds no face avatars, keeping the mobile card lean.
func TestDeck_CrewlessPlanRendersNoFaces(t *testing.T) {
	body := getDeckAtNow(t, seedDeck(), "/deck")

	// Match the rendered ANCHOR/SPAN combos, not the bare class (which also appears
	// in the <style> block's CSS rules).
	for _, unwanted := range []string{
		`<span class="deck-card-crew-faces" role="group"`,
		`class="deck-card-crew-face"`,
		`deck-card-crew-face-stuck"`,
		`deck-card-crew-face-queued"`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("crew-less deck card rendered a face strip element %q", unwanted)
		}
	}
}

// TestDeck_FaceStripsAgreeWithIndexRollup pins the two-derivations-must-agree
// invariant (advice ka-0000005924): the deck face strips read the SAME planCrewRollup
// .Faces / .StuckFaces / .QueuedFaces the index card renders, so the count of
// rendered deck avatars matches the rollup the enrich pass produced. A drift here
// means the deck and index disagree about who is on the plan.
func TestDeck_FaceStripsAgreeWithIndexRollup(t *testing.T) {
	f := seedDeckWithQueuedStuckCrew()
	s := testServerWithDaemon(f)
	s.SetNow(func() time.Time { return deckCrewNow })
	rows := s.planIndexEnrich(context.Background(), []*beadsapi.BeadDetail{f.beads["gd-decision"]})
	if len(rows) != 1 {
		t.Fatalf("expected 1 enriched row, got %d", len(rows))
	}
	roll := rows[0].Crew

	body := getDeckAtNow(t, f, "/deck")

	// One stuck avatar in the rollup → exactly one rendered stuck face anchor.
	gotStuck := strings.Count(body, `class="deck-card-crew-face deck-card-crew-face-stuck"`)
	if gotStuck != len(roll.StuckFaces) {
		t.Errorf("deck rendered %d stuck faces, rollup has %d — surfaces disagree", gotStuck, len(roll.StuckFaces))
	}
	// One queued avatar in the rollup → exactly one rendered queued face anchor.
	gotQueued := strings.Count(body, `class="deck-card-crew-face deck-card-crew-face-queued"`)
	if gotQueued != len(roll.QueuedFaces) {
		t.Errorf("deck rendered %d queued faces, rollup has %d — surfaces disagree", gotQueued, len(roll.QueuedFaces))
	}
}
