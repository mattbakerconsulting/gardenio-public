package main

// deck_test.go — GET /deck, the mobile card-deck (plan ka-0000000492 Step 4,
// task ka-0000000701).
//
// The tests here are weighted toward the deck's two SAFETY properties, because
// those are the ones a future edit could silently break:
//
//   - swipe/nav can never COMMIT (only tap can), and
//   - the page works with JavaScript OFF.
//
// A deck that quietly loses either property still looks fine in a screenshot,
// which is exactly why they are pinned by assertion rather than by comment.
//
// The answer-in-place tests (TestDeck_Answer* below) are the ones that guard
// the fix for the bug decision ka-0000001748 flagged on the first cut of Step 4
// (MR !6302): they actually POST the answer form and inspect the swap payload,
// proving the deck gets its LEAN card back rather than a cockpit panel — the
// exact assertion the original MR's tests were missing.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// seedDeck builds a fixture spanning every attention tier the deck renders, so
// one fake exercises the ordering, the per-tier affordances, and the filtering
// of healthy plans. The plans are seeded in DELIBERATELY WRONG order (healthy
// first, decision last) so a passing order assertion proves sortPlanRows ran
// rather than that the fixture happened to be pre-sorted.
func seedDeck() *fakeDaemon {
	f := newFake()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	mkPlan := func(id, title, state string, updated time.Time) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:        id,
			Title:     title,
			Kind:      "issue",
			Type:      "epic",
			Status:    "in_progress",
			Labels:    []string{"plan", "project:gardenio"},
			Fields:    map[string]string{"plan_state": state},
			CreatedAt: updated.Add(-72 * time.Hour),
			UpdatedAt: updated,
		}
	}
	// A healthy plan (no decision, no escalation, not ready, not stalled) — must
	// be FILTERED OUT of the deck entirely.
	healthy := mkPlan("gd-healthy", "Healthy moving plan", "building", now.Add(-1*time.Hour))
	// A plan with an open decision — the TOP tier.
	withDec := mkPlan("gd-decision", "Plan awaiting an answer", "building", now.Add(-48*time.Hour))
	withDec.Priority = 1
	// A plan sitting in `ready` — the Approve & build tier.
	ready := mkPlan("gd-ready", "Plan ready to build", "ready", now.Add(-24*time.Hour))
	ready.Priority = 2

	dec := &beadsapi.BeadDetail{
		ID:     "gd-q1",
		Title:  "Which storage backend?",
		Type:   "decision",
		Status: "open",
		Fields: map[string]string{
			"prompt":  "Which storage backend should the cache use?",
			"options": `[{"id":"redis","short":"Redis","label":"Use Redis"},{"id":"pg","short":"Postgres","label":"Use Postgres"}]`,
		},
		CreatedAt: now.Add(-48 * time.Hour),
		UpdatedAt: now.Add(-48 * time.Hour),
	}

	f.beads[healthy.ID] = healthy
	f.beads[withDec.ID] = withDec
	f.beads[ready.ID] = ready
	f.beads[dec.ID] = dec

	// The decision is a parent-child child of gd-decision, which is how the
	// batched enrich walk finds it and sets OpenDecisions=1 (the decision tier).
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-decision": {{BeadID: "gd-q1", DependsOnID: "gd-decision", Type: "parent-child"}},
	}
	// The FORWARD parent-child edge loadPlanDecision reads to prove the decision
	// belongs to the plan (the resolve membership guard).
	f.deps = map[string][]beadsapi.Dependency{
		"gd-q1": {{BeadID: "gd-q1", DependsOnID: "gd-decision", Type: "parent-child"}},
	}
	// The plan-epic list query (Types=[epic] → hotEpicsResp in the shared fake).
	// Seeded healthy-first so the deck's own ordering has to do real work.
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{healthy, ready, withDec},
		Total: 3,
	}
	// The lean top-decision fetch (Types=[decision]) the active card hydrates with.
	f.decisionResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{dec}, Total: 1}
	return f
}

// deckNavMarkup extracts just the rendered <nav class="deck-nav"> element. The
// swipe-safety assertions are about MARKUP shape, and the inline driver script
// further down the page names the same data-deck-* attributes in its
// querySelector calls — scanning the whole body would match those strings and
// assert nothing useful.
func deckNavMarkup(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<nav class="deck-nav"`)
	if start < 0 {
		t.Fatal("deck did not render its <nav class=\"deck-nav\">")
	}
	end := strings.Index(body[start:], "</nav>")
	if end < 0 {
		t.Fatal("deck nav was not closed")
	}
	return body[start : start+end]
}

func getDeck(t *testing.T, f *fakeDaemon, target string) *httptest.ResponseRecorder {
	t.Helper()
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", target, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200", target, w.Code)
	}
	return w
}

// TestDeck_RendersRankedCards proves the deck renders, ranks attention-first via
// sortPlanRows, drops the healthy tier, and shows the three card-face fields the
// acceptance criteria pin (title, status, priority).
func TestDeck_RendersRankedCards(t *testing.T) {
	body := getDeck(t, seedDeck(), "/deck").Body.String()

	for _, want := range []string{
		`data-deck`,                                  // the swipe module's DOM gate
		`Plan awaiting an answer`,                    // the decision-tier card
		`Plan ready to build`,                        // the ready-tier card
		`Which storage backend should`,               // the hydrated question
		`name="option_id"`,                           // a tappable answer
		`/plans/gd-decision/decisions/gd-q1/resolve`, // the EXISTING resolve route
		`chip-status-in_progress`,                    // status on the card face
		`deck-prio-1`,                                // priority on the card face
	} {
		if !strings.Contains(body, want) {
			t.Errorf("deck missing %q", want)
		}
	}

	// A healthy plan is not an action, so it must not consume a card.
	if strings.Contains(body, "Healthy moving plan") {
		t.Errorf("deck must not render plans in the planAttentionNone tier — it is an action queue, not a browser")
	}

	// Attention-first: the decision card must precede the ready card.
	iDec := strings.Index(body, "Plan awaiting an answer")
	iReady := strings.Index(body, "Plan ready to build")
	if iDec < 0 || iReady < 0 {
		t.Fatalf("both cards should render (dec=%d ready=%d)", iDec, iReady)
	}
	if iDec > iReady {
		t.Errorf("decision-tier card must sort before ready-tier card (sortPlanRows order); got dec@%d after ready@%d", iDec, iReady)
	}
}

// TestDeck_SwipeNeverCommits is the load-bearing SAFETY test. The deck's one
// non-negotiable rule is that a gesture navigates and only a tap commits. The
// structural guarantee is that the swipe module (deck_swipe.js) can only click
// [data-deck-skip] / [data-deck-back] — so this test pins that those two
// controls are plain navigation links (GET, no form, no write route) and that
// every write is a POST form the module is explicitly excluded from.
//
// If someone ever "simplifies" Skip into a form that posts a dismissal, this
// test is what catches it before a mis-swipe starts dispatching crew.
func TestDeck_SwipeNeverCommits(t *testing.T) {
	body := getDeck(t, seedDeck(), "/deck").Body.String()
	// Scope to the rendered <nav> — the inline driver script below it mentions
	// the same attribute names in selectors, and we are asserting about MARKUP.
	nav := deckNavMarkup(t, body)

	for _, ctrl := range []string{"data-deck-skip", "data-deck-back"} {
		i := strings.Index(nav, ctrl)
		if i < 0 {
			// Back is legitimately absent at position 0 (rendered as a disabled
			// span), so only Skip must be present on a multi-card deck.
			if ctrl == "data-deck-skip" {
				t.Fatalf("deck must expose a %s control for the swipe module to click", ctrl)
			}
			continue
		}
		// Walk back to the start of the element and assert it is an anchor, not
		// a form/button — i.e. navigation, never a write.
		start := strings.LastIndex(nav[:i], "<")
		if start < 0 {
			t.Fatalf("%s: could not locate its element", ctrl)
		}
		el := nav[start:i]
		if !strings.HasPrefix(el, "<a ") {
			t.Errorf("%s must be an <a> navigation link (swipe navigates, never commits); got element starting %q", ctrl, el)
		}
	}

	// Every write on the page is a POST form, and each is marked
	// data-deck-no-swipe so the gesture layer refuses to start a drag on it.
	if !strings.Contains(body, `method="post"`) {
		t.Errorf("answer affordances must be real POST forms (JS-off requirement)")
	}
	// Nothing inside the nav may be a form or a button — those are the shapes
	// that write. The nav's whole job is navigation.
	for _, forbidden := range []string{"<form", "<button", "method=\"post\"", "hx-post"} {
		if strings.Contains(nav, forbidden) {
			t.Errorf("the deck nav must contain no %q — swipe clicks these controls, and swipe must never commit", forbidden)
		}
	}
	// The resolve form must carry the no-swipe opt-out so tapping an answer is
	// never reinterpreted as a drag.
	resolveAt := strings.Index(body, "/decisions/gd-q1/resolve")
	if resolveAt < 0 {
		t.Fatal("expected a resolve form")
	}
	formStart := strings.LastIndex(body[:resolveAt], "<form")
	if formStart < 0 {
		t.Fatal("resolve action should live on a <form>")
	}
	formEnd := strings.Index(body[formStart:], ">")
	if !strings.Contains(body[formStart:formStart+formEnd], "data-deck-no-swipe") {
		t.Errorf("the resolve form must carry data-deck-no-swipe so a tap can never be read as a swipe")
	}
}

// TestDeck_WorksWithJavaScriptOff proves the no-JS contract: the nav controls
// are real hrefs carrying the cursor, and the answer buttons are real form
// submits. Without this, a phone with a failed script load shows a dead deck.
func TestDeck_WorksWithJavaScriptOff(t *testing.T) {
	body := getDeck(t, seedDeck(), "/deck").Body.String()

	// Skip must be a real link that carries the cursor forward AND records the
	// skipped id, so the position survives a reload with no JS at all.
	if !strings.Contains(body, `href="/deck?`) {
		t.Errorf("nav controls must be real hrefs so the deck works with JS off")
	}
	if !strings.Contains(body, "at=1") {
		t.Errorf("the Skip link must advance the ?at= cursor in the URL")
	}
	if !strings.Contains(body, "skip=gd-decision") {
		t.Errorf("the Skip link must record the skipped plan id in ?skip= so the skip is remembered")
	}
	// Answer buttons must be submits inside a form with a plain action, not
	// hx-post only — htmx is enhancement, the form is the contract.
	if !strings.Contains(body, `action="/plans/gd-decision/decisions/gd-q1/resolve"`) {
		t.Errorf("the answer form needs a plain action= for the JS-off path, not just hx-post")
	}
	// The from_card seam must ride on the form so a JS-off full POST still
	// lands on the deck-return path rather than a bare cockpit fragment.
	if !strings.Contains(body, `name="from_card" value="1"`) {
		t.Errorf("the answer form must carry from_card=1 so the resolve handler re-renders the deck card, not the cockpit panel")
	}
	// A <noscript> submit for the scope selector, since its change-handler is JS.
	if !strings.Contains(body, "<noscript>") {
		t.Errorf("the project scope selector needs a <noscript> submit button")
	}
}

// TestDeck_CursorSurvivesAndClamps proves the ?at= cursor round-trips and that
// a stale/garbage cursor lands on a real card instead of erroring. The clamp
// matters in practice: another device answering a card shrinks the deck under
// you, and a mid-flip re-auth reload must not 400.
func TestDeck_CursorSurvivesAndClamps(t *testing.T) {
	// A valid cursor selects the second card.
	body := getDeck(t, seedDeck(), "/deck?at=1").Body.String()
	if !strings.Contains(body, `data-deck-position="1"`) {
		t.Errorf("?at=1 should select card index 1")
	}

	// Out-of-range and garbage cursors clamp rather than fail.
	for _, raw := range []string{"?at=999", "?at=-4", "?at=banana"} {
		w := getDeck(t, seedDeck(), "/deck"+raw)
		if w.Code != http.StatusOK {
			t.Errorf("deck%s: got %d, want a clamped 200", raw, w.Code)
		}
	}
}

// TestDeck_SkipMovesToBackNeverDrops pins the "skip is free and remembered"
// rule: a skipped card goes to the BACK of the deck, not the void. If a skip
// dropped the card, the deck would be a way to LOSE a question rather than
// defer it — the opposite of draining a backlog.
func TestDeck_SkipMovesToBackNeverDrops(t *testing.T) {
	body := getDeck(t, seedDeck(), "/deck?skip=gd-decision").Body.String()

	if !strings.Contains(body, "Plan awaiting an answer") {
		t.Fatalf("a skipped card must still be in the deck — skip defers, it never drops")
	}
	// It must now sort AFTER the card that was behind it.
	iDec := strings.Index(body, "Plan awaiting an answer")
	iReady := strings.Index(body, "Plan ready to build")
	if iDec < iReady {
		t.Errorf("a skipped card must move to the BACK; got skipped@%d still ahead of ready@%d", iDec, iReady)
	}
	if !strings.Contains(body, "1 skipped") {
		t.Errorf("the deck should report how many cards were skipped this session")
	}
}

// TestDeck_ReadOnlyViewerGetsNoActions mirrors the card fragment's gating: a
// non-operator sees the questions but no tappable writes. The deck introduces
// no new write verb, so this is defense-in-depth over the already-gated POST
// routes — but a deck of buttons that all 403 is worse than no buttons.
func TestDeck_ReadOnlyViewerGetsNoActions(t *testing.T) {
	mux := mkServer(t, seedDeck())
	req := httptest.NewRequest("GET", "/deck", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	body := w.Body.String()

	if strings.Contains(body, `name="option_id"`) {
		t.Errorf("a read-only viewer must not get tappable answer forms")
	}
	if strings.Contains(body, "Approve &amp; build") || strings.Contains(body, "Approve & build") {
		t.Errorf("a read-only viewer must not get the Approve & build action")
	}
	// They still see the question — read access is not the thing being gated.
	if !strings.Contains(body, "Which storage backend should") {
		t.Errorf("a read-only viewer should still SEE the question")
	}
	if !strings.Contains(body, "deck-opt-static") {
		t.Errorf("a read-only viewer should see the options as static chips")
	}
}

// TestDeck_EmptyStateIsSuccess proves an empty deck reads as "caught up", not
// as an error or a blank page. On this surface empty is the goal state.
func TestDeck_EmptyStateIsSuccess(t *testing.T) {
	f := seedDeck()
	// Only the healthy plan remains → nothing needs a human.
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{f.beads["gd-healthy"]},
		Total: 1,
	}
	body := getDeck(t, f, "/deck").Body.String()

	if !strings.Contains(body, "Nothing is waiting on a human") {
		t.Errorf("an empty deck should read as caught-up, not as an error")
	}
	// No nav element at all (the inline driver script names the same attributes
	// in its selectors, so assert on the ELEMENT, not the attribute string).
	if strings.Contains(body, `<nav class="deck-nav"`) {
		t.Errorf("an empty deck should render no nav controls")
	}
}

// TestDeck_ReadyPlanOffersBuildNotAnAnswer proves the per-tier affordance: a
// `ready` plan's action is Approve & build (a POST to the EXISTING operator-
// gated /build route), and that action carries a confirm — it starts a gardener
// and dispatches crew, so it must never be one stray tap away.
func TestDeck_ReadyPlanOffersBuildNotAnAnswer(t *testing.T) {
	body := getDeck(t, seedDeck(), "/deck?at=1").Body.String()

	if !strings.Contains(body, `action="/plans/gd-ready/build"`) {
		t.Errorf("a ready plan's card should offer the existing POST /plans/{id}/build action")
	}
	if !strings.Contains(body, "hx-confirm") {
		t.Errorf("Approve & build must carry a confirm — it starts a gardener and dispatches crew")
	}
	// The build form must carry from_card=1 so the handler re-renders the deck
	// card in place rather than swapping the cockpit gardener bar into the slot.
	if !strings.Contains(body, `name="from_card" value="1"`) {
		t.Errorf("the build form must carry from_card=1 so respondPlanBuild re-renders the deck card")
	}
}

// TestDeck_HydratesOnlyTheVisibleWindow pins the deck's cost bound: it issues
// ONE plan-list query plus the batched enrich, and hydrates decisions only for
// the active card and its prefetch window — never one per queued plan. A
// regression here turns a 60-card deck into 60 serial daemon calls.
func TestDeck_HydratesOnlyTheVisibleWindow(t *testing.T) {
	f := seedDeck()
	getDeck(t, f, "/deck")

	f.listMu.Lock()
	var decisionLists int
	for _, q := range f.listAllQ {
		if len(q.Types) == 1 && q.Types[0] == "decision" {
			decisionLists++
		}
	}
	f.listMu.Unlock()

	// Two attention-tier cards render, but only ONE is a decision-tier card, so
	// exactly one top-decision fetch should be issued regardless of queue depth.
	if decisionLists > deckPrefetchAhead+1 {
		t.Errorf("deck issued %d decision lists; the visible window bounds it at %d — a per-card fan-out blows the page budget",
			decisionLists, deckPrefetchAhead+1)
	}
	t.Logf("deck decision hydrations = %d (window bound %d)", decisionLists, deckPrefetchAhead+1)
}

// TestDeck_LongQuestionCollapsesSoAnswersStayAboveTheFold pins the fix for a
// problem found by driving the real page at 390x844 against live prod data: a
// realistic gardener prompt (several hundred chars) rendered whole pushed EVERY
// answer button off screen. A deck you must scroll before you can answer is not
// a deck. The tail collapses behind a native <details> — chosen over a JS
// toggle because the no-JS contract has to hold here too.
func TestDeck_LongQuestionCollapsesSoAnswersStayAboveTheFold(t *testing.T) {
	f := seedDeck()
	long := "Should the loop keep firing every thirty minutes while the plan is parked? " +
		strings.Repeat("Here is a great deal more explanatory context that a gardener wrote. ", 6)
	f.beads["gd-q1"].Fields["prompt"] = long
	f.decisionResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{f.beads["gd-q1"]}, Total: 1}

	body := getDeck(t, f, "/deck").Body.String()

	if !strings.Contains(body, "<details") || !strings.Contains(body, "<summary") {
		t.Fatalf("a long prompt must collapse behind a native <details> so the answers stay above the fold")
	}
	// The disclosure must be NATIVE — a JS-driven toggle would break the no-JS
	// contract exactly where the operator needs to read the question.
	detailsAt := strings.Index(body, "<details")
	summaryEnd := strings.Index(body[detailsAt:], "</summary>")
	summary := body[detailsAt : detailsAt+summaryEnd]
	if strings.Contains(summary, "onclick") || strings.Contains(summary, "hx-") {
		t.Errorf("the question disclosure must be plain <details>/<summary>, not a scripted toggle")
	}
	// The answer buttons must come AFTER the disclosure in source order, i.e.
	// outside it — collapsing the question must never collapse the answers.
	optAt := strings.Index(body, `name="option_id"`)
	closeAt := strings.Index(body, "</details>")
	if optAt < 0 || closeAt < 0 || optAt < closeAt {
		t.Errorf("answer buttons must live OUTSIDE the collapsed question (opt@%d, </details>@%d)", optAt, closeAt)
	}
}

// TestSplitDeckQuestion covers the prompt-splitting rules directly.
func TestSplitDeckQuestion(t *testing.T) {
	t.Run("short prompt is not split", func(t *testing.T) {
		lead, rest := splitDeckQuestion("Which backend?")
		if lead != "Which backend?" || rest != "" {
			t.Errorf("got (%q, %q), want the whole prompt as the lead with no remainder", lead, rest)
		}
	})

	t.Run("long prompt splits at a sentence boundary", func(t *testing.T) {
		p := "Should we keep the loop running? " + strings.Repeat("Extra context here. ", 30)
		lead, rest := splitDeckQuestion(p)
		if rest == "" {
			t.Fatal("a long prompt must produce a collapsed remainder")
		}
		if len(lead) > deckQuestionLeadLen {
			t.Errorf("lead is %d chars, budget is %d", len(lead), deckQuestionLeadLen)
		}
		if !strings.HasSuffix(lead, ".") && !strings.HasSuffix(lead, "?") && !strings.HasSuffix(lead, "!") {
			t.Errorf("lead should end on a sentence boundary, got %q", lead)
		}
	})

	t.Run("no sentence boundary falls back to a word boundary", func(t *testing.T) {
		p := strings.Repeat("word ", 100)
		lead, rest := splitDeckQuestion(p)
		if rest == "" {
			t.Fatal("expected a split")
		}
		if strings.HasSuffix(lead, "wor") || strings.HasSuffix(lead, "wo") {
			t.Errorf("lead must not end mid-word, got %q", lead[max(0, len(lead)-12):])
		}
	})

	t.Run("does not split on an id full-stop", func(t *testing.T) {
		// A trailing "." inside a token (gd-Abc.) must not be read as a sentence
		// end, or the lead truncates mid-identifier.
		p := "The trigger ka-0000001682.is deep " + strings.Repeat("and more context follows here ", 12)
		lead, _ := splitDeckQuestion(p)
		if strings.HasSuffix(lead, "ka-0000001682.") {
			t.Errorf("must not treat an intra-token '.' as a sentence boundary; got %q", lead)
		}
	})
}

// TestDeckHelpers covers the pure cursor/skip logic directly — the branches the
// HTTP tests above only reach incidentally.
func TestDeckHelpers(t *testing.T) {
	t.Run("clampDeckPosition", func(t *testing.T) {
		cases := []struct {
			raw   string
			total int
			want  int
		}{
			{"", 5, 0},
			{"2", 5, 2},
			{"9", 5, 4},  // past the end clamps to the last card
			{"-1", 5, 0}, // negative clamps to the first
			{"x", 5, 0},  // unparseable clamps to the first
			{"3", 0, 0},  // an empty deck is always position 0
		}
		for _, c := range cases {
			if got := clampDeckPosition(c.raw, c.total); got != c.want {
				t.Errorf("clampDeckPosition(%q, %d) = %d, want %d", c.raw, c.total, got, c.want)
			}
		}
	})

	t.Run("applyDeckSkips moves to back preserving order", func(t *testing.T) {
		cards := []deckCardItem{{PlanID: "a"}, {PlanID: "b"}, {PlanID: "c"}}
		got, n := applyDeckSkips(cards, map[string]struct{}{"a": {}})
		if n != 1 {
			t.Errorf("skip count = %d, want 1", n)
		}
		want := []string{"b", "c", "a"}
		for i, id := range want {
			if got[i].PlanID != id {
				t.Errorf("position %d = %q, want %q (skipped cards go to the BACK, never dropped)", i, got[i].PlanID, id)
			}
		}
		if len(got) != len(cards) {
			t.Errorf("skipping must never drop a card: got %d, want %d", len(got), len(cards))
		}
	})

	t.Run("deckSkipParam dedupes", func(t *testing.T) {
		if got := deckSkipParam("", "a"); got != "a" {
			t.Errorf("got %q, want %q", got, "a")
		}
		if got := deckSkipParam("a", "b"); got != "a,b" {
			t.Errorf("got %q, want %q", got, "a,b")
		}
		if got := deckSkipParam("a,b", "a"); got != "a,b" {
			t.Errorf("re-skipping an id must not duplicate it; got %q", got)
		}
	})

	t.Run("deckCards drops the healthy tier", func(t *testing.T) {
		rows := []planRowView{
			{ID: "gd-1", OpenDecisions: 1},
			{ID: "gd-2"}, // healthy → planAttentionNone
			{ID: "gd-3", PlanState: string(planStateReady)},
		}
		got := deckCards(rows)
		if len(got) != 2 {
			t.Fatalf("deckCards kept %d cards, want 2 (the healthy plan must be filtered out)", len(got))
		}
		for _, c := range got {
			if c.Reason == "" {
				t.Errorf("card %s has no reason line — the deck must say why a card is in front of you", c.PlanID)
			}
		}
	})
}

// --- Answer-in-place tests: the fix for decision ka-0000001748's bug. These
//     actually POST the answer form the deck emits and inspect the swap
//     payload, which is exactly the assertion MR !6302 was missing. ---

// postDeckAnswer drives the real POST a deck answer button issues: an htmx
// request (HX-Request: true) carrying from_card=1, as an operator.
func postDeckAnswer(t *testing.T, f *fakeDaemon, path, form string) *httptest.ResponseRecorder {
	t.Helper()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req.Header.Set("HX-Request", "true")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestDeck_AnswerSwapsLeanCardNotCockpitPanel is the regression test for the
// bug decision ka-0000001748 flagged: a deck answer POSTs to the plan-scoped
// resolve route, and WITHOUT the from_card seam that route returns the COCKPIT
// #plan-decisions panel — which htmx would swap into the deck slot, corrupting
// the stack. With from_card=1 the handler must return the LEAN deck_card
// fragment instead. This is the assertion the original MR's tests never made.
func TestDeck_AnswerSwapsLeanCardNotCockpitPanel(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	w := postDeckAnswer(t, f, "/plans/gd-plan1/decisions/gd-dec1/resolve", "from_card=1&option_id=label")

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if f.resolveCalls != 1 || f.resolveLastReq.SelectedOption != "label" {
		t.Errorf("the write must still happen: got resolveCalls=%d option=%q", f.resolveCalls, f.resolveLastReq.SelectedOption)
	}
	body := w.Body.String()

	// The bug: the response contained the cockpit #plan-decisions panel and no
	// .deck-card. The fix inverts both.
	if strings.Contains(body, `id="plan-decisions"`) {
		t.Errorf("a from_card answer must NOT return the cockpit #plan-decisions panel — that is the bug this fixes")
	}
	if !strings.Contains(body, "deck-card") {
		t.Errorf("a from_card answer must return the lean deck_card fragment so the swap keeps the deck's card shape")
	}
}

// TestDeck_AnswerWithoutFromCardStillReturnsCockpit proves the seam is scoped:
// a plain cockpit answer (no from_card) is UNCHANGED — it still returns the
// #plan-decisions panel. The fix must not regress the cockpit's own answer flow.
func TestDeck_AnswerWithoutFromCardStillReturnsCockpit(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	w := postDeckAnswer(t, f, "/plans/gd-plan1/decisions/gd-dec1/resolve", "option_id=label")

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `id="plan-decisions"`) {
		t.Errorf("a cockpit answer (no from_card) must still re-render #plan-decisions — the seam must be scoped to the deck")
	}
}

// TestDeck_JSOffAnswerRedirectsBackToDeck proves the no-JS half of the fix: a
// full POST (no HX-Request header) from a deck card must NOT strand the operator
// on a bare layout-less fragment. It redirects (303) back to the deck, carrying
// the cursor from the Referer so the operator keeps their place.
func TestDeck_JSOffAnswerRedirectsBackToDeck(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("POST", "/plans/gd-plan1/decisions/gd-dec1/resolve",
		strings.NewReader("from_card=1&option_id=label"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req.Header.Set("Referer", "http://example.test/deck?at=2&skip=gd-other")
	// No HX-Request header → JS-off full navigation.
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("a JS-off deck answer should redirect (303), got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/deck") {
		t.Errorf("a JS-off deck answer must redirect back to the deck, got %q", loc)
	}
	if !strings.Contains(loc, "at=2") || !strings.Contains(loc, "skip=gd-other") {
		t.Errorf("the redirect must carry the deck cursor from the Referer so the operator keeps their place, got %q", loc)
	}
}

// TestDeck_FailedAnswerRendersErrorCardNotBlank proves a failed write comes back
// as an error ON the card (the marker the deck's JS advance-guard looks for), so
// the operator sees what went wrong rather than the card silently vanishing.
func TestDeck_FailedAnswerRendersErrorCard(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	f.resolveErr = &beadsapi.APIError{StatusCode: http.StatusBadGateway, Message: "daemon down"}
	w := postDeckAnswer(t, f, "/plans/gd-plan1/decisions/gd-dec1/resolve", "from_card=1&option_id=label")

	body := w.Body.String()
	if !strings.Contains(body, "deck-card-error") {
		t.Errorf("a failed deck answer must render the deck-card-error marker so the JS advance-guard holds the card in place; got: %s", body)
	}
	if strings.Contains(body, `id="plan-decisions"`) {
		t.Errorf("even a failed answer must stay on the deck card, not the cockpit panel")
	}
}

// TestRenderDeckCard_AllClearWhenNoOpenQuestion proves the "answered, nothing
// left" face: when a plan has no further open decision, the re-rendered card
// says so rather than showing a blank body — so the swap reads as progress.
func TestRenderDeckCard_AllClearWhenNoOpenQuestion(t *testing.T) {
	f := seedPlanInlineDecisionFake()
	// After the answer, the plan has no more open decisions.
	f.decisionResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	f.resolveOnSuccess = func(fd *fakeDaemon, id string, _ beadsapi.ResolveDecisionRequest) {
		if b, ok := fd.beads[id]; ok {
			b.Status = "closed"
		}
	}
	w := postDeckAnswer(t, f, "/plans/gd-plan1/decisions/gd-dec1/resolve", "from_card=1&option_id=label")

	body := w.Body.String()
	if !strings.Contains(body, "deck-card-allclear") {
		t.Errorf("an answered plan with nothing left should render the all-clear face; got: %s", body)
	}
}
