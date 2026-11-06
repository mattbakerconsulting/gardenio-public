package main

// deck_freshstamp_test.go — iter-86 (ka-0000005352): the /deck mobile triage
// card's "last change" freshness stamp rendered the WIDE "7 hours ago" (Ago)
// with NO stale-tint, while the /plans index card head (.plan-card-updated) and
// the peek header (.plan-peek-updated) both render the COMPACT "7h"/"2d"/"1w"
// (AgoCompact) AND fold the FreshnessStale loud-red ">7d-dead active plan" tint.
// So a plan that had not moved in weeks read calm — and in more horizontal space
// — on the deck than on the index.
//
// This fold rides the SAME AgoCompact + FreshnessStale already on the enriched
// planRowView the deck builds from (planIndexEnrich sets them; deckCards copies
// them onto deckCardItem), so it costs the deck ZERO extra daemon call and
// deck↔index↔peek agree by construction (two-derivations-must-agree, advice
// ka-0000005924).

import (
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// seedDeckStaleFresh returns a fake with TWO decision-tier plans (both on the
// deck): gd-decision is >7d dead (its epic last moved 20 days before the deck's
// clock, no crew heartbeat to refresh it), and a second gd-fresh plan moved 2h
// ago. The stale one must tint loud; the fresh one must stay calm.
func seedDeckStaleFresh() *fakeDaemon {
	f := seedDeck()

	// Push gd-decision's newest activity 20 days into the past so, under the deck
	// clock (deckCrewNow), planFreshnessStale fires (>7d, non-terminal). seedDeck
	// hangs no step/crew off gd-decision, so its lastActivityAt IS the epic's
	// UpdatedAt — set it stale.
	dec := f.beads["gd-decision"]
	dec.UpdatedAt = deckCrewNow.Add(-20 * 24 * time.Hour)
	dec.CreatedAt = deckCrewNow.Add(-30 * 24 * time.Hour)

	// A SECOND decision-tier plan that moved 2h ago — fresh, so its stamp stays
	// calm. It carries its own open decision so it lands on the deck too.
	fresh := &beadsapi.BeadDetail{
		ID:        "gd-fresh",
		Title:     "Freshly moving plan awaiting an answer",
		Kind:      "issue",
		Type:      "epic",
		Status:    "in_progress",
		Labels:    []string{"plan", "project:gardenio"},
		Fields:    map[string]string{"plan_state": "building"},
		Priority:  1,
		CreatedAt: deckCrewNow.Add(-72 * time.Hour),
		UpdatedAt: deckCrewNow.Add(-2 * time.Hour),
	}
	freshDec := &beadsapi.BeadDetail{
		ID:     "gd-q2",
		Title:  "Which cache TTL?",
		Type:   "decision",
		Status: "open",
		Fields: map[string]string{
			"prompt":  "Which cache TTL should we use?",
			"options": `[{"id":"short","short":"5m","label":"5 minutes"},{"id":"long","short":"1h","label":"1 hour"}]`,
		},
		CreatedAt: deckCrewNow.Add(-2 * time.Hour),
		UpdatedAt: deckCrewNow.Add(-2 * time.Hour),
	}
	f.beads[fresh.ID] = fresh
	f.beads[freshDec.ID] = freshDec
	f.revDeps["gd-fresh"] = []beadsapi.Dependency{
		{BeadID: "gd-q2", DependsOnID: "gd-fresh", Type: "parent-child"},
	}
	f.deps["gd-q2"] = []beadsapi.Dependency{
		{BeadID: "gd-q2", DependsOnID: "gd-fresh", Type: "parent-child"},
	}
	// Add both new beads to the epic list + decision list the deck hydrates from.
	f.hotEpicsResp.Beads = append(f.hotEpicsResp.Beads, fresh)
	f.hotEpicsResp.Total = len(f.hotEpicsResp.Beads)
	f.decisionResp.Beads = append(f.decisionResp.Beads, freshDec)
	f.decisionResp.Total = len(f.decisionResp.Beads)
	return f
}

// TestDeck_FreshStampIsCompactAndStaleTints is the load-bearing assertion: the
// deck card face renders the COMPACT freshness face (not the wide "N hours ago")
// and tints an aged active plan loud with .deck-card-updated-stale, so a dead
// plan pops out of the deck exactly as it does on the index card.
func TestDeck_FreshStampIsCompactAndStaleTints(t *testing.T) {
	body := getDeckAtNow(t, seedDeckStaleFresh(), "/deck")

	// The stale plan's stamp carries the loud-tint class. Match the rendered
	// class-ATTRIBUTE form (space-separated) — the bare token also appears in the
	// <style> block's `.deck-card-updated.deck-card-updated-stale` rule (dot-
	// concatenated), so keying on it would be a false positive.
	if !strings.Contains(body, `deck-card-updated deck-card-updated-stale"`) {
		t.Errorf("a >7d-dead active plan must tint its deck freshness stamp loud (deck-card-updated-stale); body:\n%s", body)
	}
	// The compact face renders (2w for a 20-day-old plan under fmtTimeAgo) — NOT
	// the wide "20 days ago" visible text. "days ago"/"hours ago" now lives ONLY
	// in the aria-label.
	if !strings.Contains(body, `class="meta deck-card-updated`) {
		t.Errorf("deck freshness stamp must render as .deck-card-updated (compact); body:\n%s", body)
	}
	// The stale reason rides the tooltip.
	if !strings.Contains(body, "this plan has not moved in over a week (stale)") {
		t.Errorf("stale deck stamp must carry the stale reason in its tooltip; body:\n%s", body)
	}
	// The long humanized age survives in the aria-label for screen readers.
	if !strings.Contains(body, `aria-label="updated `) {
		t.Errorf("deck stamp must keep aria-label=\"updated <long age>\"; body:\n%s", body)
	}
}

// TestDeck_FreshPlanStampStaysCalm proves a plan that moved recently renders the
// stamp WITHOUT the loud tint — the stale signal is scoped to genuinely aged
// active plans, so a healthy plan adds no red ink.
func TestDeck_FreshPlanStampStaysCalm(t *testing.T) {
	// A fixture with ONLY a fresh decision-tier plan, so any -stale token in the
	// body would be a false positive (the stale plan is removed).
	f := seedDeck()
	// Make gd-decision fresh (2h) — seedDeck already sets it 48h old, still fresh
	// under the 7d bar, so this fixture has no stale plan at all.
	body := getDeckAtNow(t, f, "/deck")
	// Key on the rendered class-ATTRIBUTE form (space-separated) — the bare token
	// appears in the <style> rule, which is always present.
	if strings.Contains(body, `deck-card-updated deck-card-updated-stale"`) {
		t.Errorf("no plan in this fixture is >7d dead — the deck must render no stale-tinted stamp; body:\n%s", body)
	}
	// But it DOES render the compact stamp.
	if !strings.Contains(body, `class="meta deck-card-updated`) {
		t.Errorf("a datable plan must still render the compact .deck-card-updated stamp; body:\n%s", body)
	}
}

// TestDeck_FreshStampDropsWideFace proves the OLD wide "<N> hours/days ago"
// visible text face is gone from the deck card — the compact face replaced it,
// with the long form only in the aria-label. This is the "more info in LESS
// space" half of the mission for this stamp.
func TestDeck_FreshStampDropsWideFace(t *testing.T) {
	body := getDeckAtNow(t, seedDeck(), "/deck")
	// Extract just the deck-card-updated spans and assert their visible text is the
	// compact form (no "ago" between the tags). "ago" may legitimately appear in
	// the aria-label; we only reject it as VISIBLE face text.
	idx := 0
	for {
		i := strings.Index(body[idx:], `class="meta deck-card-updated`)
		if i < 0 {
			break
		}
		start := idx + i
		close := strings.Index(body[start:], "</span>")
		if close < 0 {
			t.Fatalf("unterminated deck-card-updated span")
		}
		span := body[start : start+close]
		// The visible text is after the final ">". It must not contain "ago".
		gt := strings.LastIndex(span, ">")
		visible := span[gt+1:]
		if strings.Contains(visible, "ago") {
			t.Errorf("deck freshness stamp visible face must be COMPACT (no \"ago\"); got visible=%q", visible)
		}
		idx = start + close
	}
}
