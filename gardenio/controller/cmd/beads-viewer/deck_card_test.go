package main

// deck_card_test.go — the LEAN deck card fragment (plan ka-0000000492 step 3,
// task ka-0000004313). Beyond proving the fragment renders the card face, the
// budget test here is the load-bearing artifact: it fails the build if a card
// regresses past <10 KB or past its 2-round-trip (proxy for <150 ms) budget,
// so the deck's prefetch premise (Step 4) can never silently rot.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// deckCardByteBudget caps the served /plans/{id}/card fragment at 10 KB — the
// budget the task pins (peek is 146 KB; the deck cannot flip through that).
// The card is measured on a MAXIMAL-face plan (below): a long title, a long
// question, and a full set of long-labeled options — the heaviest a real card
// gets — so the budget has real headroom on a typical card, not a fixture that
// happens to squeak under. Bumping this requires updating the plan-of-record.
const deckCardByteBudget = 10 * 1024

// deckCardRoundTripBudget caps the DAEMON round-trips one card render issues.
// It is the deterministic, CI-stable proxy for the <150 ms wall-clock target:
// the peek's weight was rendering, not fetching, and the enrich work proved a
// card adds NO per-card daemon cost — so pinning the fetch count at a small
// constant (GetBead + one bounded decision list) is what actually forbids the
// over-fetching that would blow the latency budget. A wall-clock assertion
// would be flaky under CI load; this is not.
const deckCardRoundTripBudget = 2

// seedDeckCardFatFace builds the heaviest realistic card: a plan with a long
// title, a fresh open decision whose prompt AND every option label is long,
// and (to prove they cost the card NOTHING) a step child, a crew agent, and an
// MR bead in the subtree — none of which a lean card fetches or renders. This
// is the fixture the byte + round-trip budgets are measured against, so they
// gate the worst real case, not a trivially-small one.
func seedDeckCardFatFace() *fakeDaemon {
	f := newFake()
	plan := &beadsapi.BeadDetail{
		ID:          "gd-plan1",
		Title:       strings.Repeat("Ship the mobile card-deck triage surface ", 4),
		Kind:        "issue",
		Type:        "epic",
		Status:      "in_progress",
		Labels:      []string{"project:gardenio", "plan"},
		Description: strings.Repeat("Lots of narrative that the card must NOT carry. ", 40),
		Fields:      map[string]string{"plan_state": "building"},
	}
	// A fat decision: long prompt + eight long-labeled options — the heaviest
	// question a card face renders.
	var opts []string
	for i := range 8 {
		opts = append(opts, `{"id":"opt`+string(rune('a'+i))+`",`+
			`"short":"`+strings.Repeat("Option ", 3)+string(rune('A'+i))+`",`+
			`"label":"`+strings.Repeat("A much longer descriptive label for the option ", 2)+`",`+
			`"artifact_type":"report"}`)
	}
	dec := &beadsapi.BeadDetail{
		ID:     "gd-dec1",
		Title:  "Which approach?",
		Type:   "decision",
		Status: "open",
		Fields: map[string]string{
			"prompt":  strings.Repeat("This is a fairly long clarifying question the gardener is asking. ", 6),
			"options": "[" + strings.Join(opts, ",") + "]",
		},
	}
	// Subtree ballast the card must ignore: a step, a crew agent, an MR bead.
	step := &beadsapi.BeadDetail{ID: "gd-step1", Title: "Wire the route", Type: "task", Status: "in_progress", Labels: []string{"project:gardenio"}}
	crew := &beadsapi.BeadDetail{ID: "gd-crew1", Title: "gardenio-wire-route-abc", Type: "agent", Status: "in_progress"}
	mr := &beadsapi.BeadDetail{ID: "gd-mr1", Title: "Draft: wire the route", Type: "merge-request", Status: "open"}

	f.beads["gd-plan1"] = plan
	f.beads["gd-dec1"] = dec
	f.beads["gd-step1"] = step
	f.beads["gd-crew1"] = crew
	f.beads["gd-mr1"] = mr
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-plan1": {
			{BeadID: "gd-step1", DependsOnID: "gd-plan1", Type: "parent-child"},
			{BeadID: "gd-dec1", DependsOnID: "gd-plan1", Type: "parent-child"},
		},
	}
	// The lean top-decision query is Types=[decision] → decisionResp.
	f.decisionResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{dec}, Total: 1}
	return f
}

// TestDeckCard_RendersFace proves GET /plans/{id}/card returns the lean card:
// the title linked to the cockpit, the status/project/age meta, and the plan's
// one open question with its tappable option buttons.
func TestDeckCard_RendersFace(t *testing.T) {
	mux := mkServer(t, seedDeckCardFatFace())
	req := httptest.NewRequest("GET", "/plans/gd-plan1/card", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="deck-card"`,
		`data-plan-id="gd-plan1"`,
		`href="/plans/gd-plan1"`,                    // full-cockpit link
		`data-deck-up`,                              // the "swipe up to action" target (ka-0000005224)
		"chip-status-in_progress",                   // status chip
		`href="/plans?project=gardenio"`,            // project chip
		`class="deck-card-decision"`,                // the question section
		`data-decision-id="gd-dec1"`,                // the surfaced decision
		`name="option_id"`,                          // a tappable resolve form
		`/plans/gd-plan1/decisions/gd-dec1/resolve`, // same route as cockpit/peek
	} {
		if !strings.Contains(body, want) {
			t.Errorf("deck card missing %q", want)
		}
	}
	// A bare fragment, not a page: no layout chrome. And NONE of the heavy peek
	// sections leak in — that is the whole point of the lean card.
	for _, forbidden := range []string{
		"<nav",             // no site nav (fragment)
		`id="plan-steps"`,  // no step timeline
		`id="plan-crew"`,   // no crew roster
		`class="bead-chat`, // no chat transcript
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("lean card must not contain %q (that weight belongs to the peek drawer)", forbidden)
		}
	}
}

// TestDeckCard_ReadOnlyViewerGetsStaticOptions proves a non-operator viewer
// sees the options as static chips, not tappable resolve forms — the same
// operator gating the peek/cockpit decision chips apply.
func TestDeckCard_ReadOnlyViewerGetsStaticOptions(t *testing.T) {
	mux := mkServer(t, seedDeckCardFatFace())
	// A request carrying the read-only Viewer role (not Operator): viewerCanOperate
	// enforces the role floor when roles are present, so this viewer cannot resolve.
	// (With NO roles, RBAC is not enforced and viewerCanOperate returns true — the
	// dev-bypass path the other tests exercise; here we pin the gated-viewer case.)
	req := httptest.NewRequest("GET", "/plans/gd-plan1/card", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if strings.Contains(body, `name="option_id"`) {
		t.Errorf("read-only viewer must not get tappable resolve forms")
	}
	if !strings.Contains(body, "deck-card-opt-static") {
		t.Errorf("read-only viewer should see static option chips")
	}
}

// TestDeckCard_NoDecisionRendersHeaderOnly proves a plan with no open decision
// still renders a valid card (header + cockpit link), never an error.
func TestDeckCard_NoDecisionRendersHeaderOnly(t *testing.T) {
	f := seedDeckCardFatFace()
	f.decisionResp = &beadsapi.ListBeadsResult{} // no open decision
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1/card", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `class="deck-card"`) {
		t.Errorf("card should still render its header")
	}
	if strings.Contains(body, `class="deck-card-decision"`) {
		t.Errorf("a plan with no open decision should render no question section")
	}
}

// TestDeckCard_NotFound proves a missing or non-epic id 404s, mirroring peek.
func TestDeckCard_NotFound(t *testing.T) {
	f := seedDeckCardFatFace()
	f.beads["gd-task9"] = &beadsapi.BeadDetail{ID: "gd-task9", Type: "task", Status: "open"}
	mux := mkServer(t, f)
	for _, id := range []string{"gd-nope", "gd-task9"} {
		req := httptest.NewRequest("GET", "/plans/"+id+"/card", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("card %s: got %d, want 404", id, w.Code)
		}
	}
}

// TestDeckCard_SizeAndRoundTripBudget is the load-bearing regression gate. It
// drives the heaviest realistic card and asserts BOTH budgets:
//
//   - payload < 10 KB  — the served fragment size (the peek's 146 KB is what
//     made a deck impossible; a card must stay flip-through-cheap).
//   - <= 2 daemon round-trips — the CI-stable proxy for the <150 ms target:
//     GetBead (the epic) + one bounded decision list, O(1) per card, no
//     over-fetch. This is what forbids a future edit from quietly folding the
//     step/crew/MR walks back in.
//
// Both together are what let Step 4 prefetch the next card speculatively.
func TestDeckCard_SizeAndRoundTripBudget(t *testing.T) {
	f := seedDeckCardFatFace()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1/card", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// --- payload budget ---
	body := w.Body.Bytes()
	if len(body) == 0 {
		t.Fatal("card fragment is empty — render regression?")
	}
	if len(body) > deckCardByteBudget {
		t.Errorf("deck card = %d bytes, budget = %d bytes (%.1f KB over) — the deck cannot prefetch a card this heavy",
			len(body), deckCardByteBudget, float64(len(body)-deckCardByteBudget)/1024)
	}
	// Sanity floor: a real card carries its face. A near-empty body would pass
	// the ceiling while proving nothing.
	if !bytes.Contains(body, []byte(`class="deck-card"`)) {
		t.Fatal("card fragment did not render the card face")
	}
	t.Logf("deck card fat-face payload = %d bytes (%.1f KB), budget = %d bytes",
		len(body), float64(len(body))/1024, deckCardByteBudget)

	// --- round-trip budget ---
	// GetBead is counted by getBeadCalls; the decision list is one entry in
	// listAllQ. Together they are the whole fetch cost of a card.
	f.getBeadMu.Lock()
	gets := f.getBeadCalls
	f.getBeadMu.Unlock()
	f.listMu.Lock()
	lists := len(f.listAllQ)
	f.listMu.Unlock()
	total := gets + lists
	if total > deckCardRoundTripBudget {
		t.Errorf("deck card issued %d daemon round-trips (%d GetBead + %d list), budget = %d — an over-fetch this size blows the <150ms card budget",
			total, gets, lists, deckCardRoundTripBudget)
	}
	// Also assert the batch walks the heavy peek pays are NOT touched — the
	// clearest signal that no step/crew/MR enrichment crept into the card.
	f.getBeadMu.Lock()
	batchBeads := f.batchGetBeadsCalls
	batchDeps := f.batchGetDepsCalls
	f.getBeadMu.Unlock()
	if batchBeads != 0 || batchDeps != 0 {
		t.Errorf("lean card must issue no batch walks; got BatchGetBeads=%d BatchGetDependencies=%d", batchBeads, batchDeps)
	}
	t.Logf("deck card round-trips = %d (GetBead=%d list=%d), budget = %d", total, gets, lists, deckCardRoundTripBudget)
}
