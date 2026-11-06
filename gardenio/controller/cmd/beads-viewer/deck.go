package main

// deck.go — GET /deck, the mobile card-deck triage surface (plan ka-0000000492
// Step 4, task ka-0000000701). The payoff step: everything Steps 0-3, 5, 5b and
// 6 landed is inert until something mounts it, and this is that something.
//
// WHAT IT IS. One plan at a time, full-bleed on a phone, showing the ONE
// question that plan is waiting on and its answers as thumb-sized buttons.
// Answer it, or skip it and it goes to the back. That is the whole surface.
// The measured problem it exists for (decision ka-0000000932): dozens of open
// decisions, most older than a day, many older than a month. Agents file
// questions faster than a human at a desk answers them, and /plans at 390px
// spent over a thousand px on chrome before the first card. A phone-shaped
// answer queue is the gap.
//
// NEW SURFACE, not a lens (decision ka-0000000932, resolved new-surface:
// "Inbox is not in use and this is a reasonable replacement for it"). So /deck
// owns its own plan-scoped ordering via sortPlanRows (Step 2) rather than
// growing a `plan` kind on inboxStreamKinds. The cost of that call is two
// attention queues; the operator accepted it because the other one is unused.
//
// WHY THIS IS MOSTLY ASSEMBLY. Every load-bearing piece already shipped:
//   - sortPlanRows / planAttention (Step 2, plans_attention.go) — the ordering
//   - deck_card.html + handlePlanCard (Step 3, deck_card.go) — the card face
//   - deck_swipe.js (Step 5 / 5b) — binds to the data-deck* attributes THIS file emits
//   - handleWebManifest (Step 6, pwa.go) — already points start_url at /deck
//   - POST /plans/{id}/decisions/{d}/resolve, POST /plans/{id}/build (plans.go,
//     plans_lifecycle.go) — the answer + approve writes
//
// So this file adds the queue, the cursor, and the shell. It introduces no new
// write verb: every action on a deck card POSTs to a route that already existed
// and is already operator-gated.
//
// THE ANSWER-IN-PLACE SEAM (fixes the bug decision ka-0000001748 flagged on the
// first cut of Step 4, MR !6302). The deck's answer/build forms carry a hidden
// from_card=1 and target `closest .deck-card`. The existing resolve/build
// handlers branch on that flag and re-render the LEAN deck_card fragment
// (renderDeckCard) instead of their cockpit fragment — so a one-tap answer
// swaps the SAME lean card shape back into the slot (its now-resolved next
// question, or a header-only "all clear" face), and the JS stack tracking stays
// intact. Without the flag those handlers still return the cockpit fragments
// for /plans/{id}, unchanged.
//
// THE TWO DESIGN RULES, both deliberate:
//
//  1. SWIPE NAVIGATES, TAP COMMITS. A gesture can only skip or go back; it can
//     never resolve. That is structural in deck_swipe.js (it has no path to a
//     write verb) and mirrored here: Skip/Back are GET links, Answer is a POST
//     form. The retired iOS app bound swipe-left to a destructive close; these
//     actions dispatch crew and merge MRs, so we do not repeat that.
//
//  2. WORKS WITH JAVASCRIPT OFF. Skip/Back are real <a href> links carrying the
//     cursor in the URL, and the answer buttons are real <form method=post>.
//     htmx and deck_swipe.js are enhancement layers over a functioning page —
//     which also means the deck survives the layout.html 401-reload path with
//     its position intact, because the position lives in the URL.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// deckQueueLimit caps the plans pulled into one deck session. Deliberately
// smaller than plansListLimit (200): the deck is a "what needs me right now"
// queue, not an archive, and sortPlanRows has already floated the urgent work
// to the front — a human flipping cards on a phone will never reach card 60,
// let alone 200. Fetching fewer keeps the single list round-trip cheap.
const deckQueueLimit = 60

// deckPrefetchAhead is how many upcoming cards the shell renders (hidden)
// behind the active one. The lean card fragment (Step 3) measured ~1-2 KB, so
// carrying the next couple in the initial HTML costs almost nothing and makes
// a skip instant — no network hop between cards at all. This is exactly the
// budget deck_card.go's TestDeckCard_SizeAndRoundTripBudget protects.
const deckPrefetchAhead = 2

// deckCardItem is one card in the rendered stack. It is a projection of the
// SAME planRowView the /plans index builds — the deck adds no new enrichment,
// so a plan's attention tier, age, and open-question count read identically on
// both surfaces by construction.
type deckCardItem struct {
	PlanID   string
	Title    string
	Heraldry string      // the plan's arms, canonical; "" when none (arms.go)
	Badge    *badgeImage // the card cover; nil when the plan has no badge (badge_image.go)
	Project  string
	Status   string
	// PlanState drives the "Approve & build" affordance: a plan sitting in
	// `ready` is one tap from moving, which is why planAttentionReady is its own
	// tier. Any other state renders no build button.
	PlanState string
	// Priority is the plan epic's 0-4 priority (0 = highest), one of the three
	// fields the card face must show (title, status, priority). Rendered as a
	// color-tiered pill so an urgent plan reads as urgent while flipping.
	Priority int
	// StepsClosed / StepsTotal / PctDone surface the plan's STEP PROGRESS on the
	// deck card face (ka-0000005352 iter-67) — the compact "N/M steps · X%" the
	// /plans index card + the cockpit both render, but the /deck mobile card
	// showed NOTHING. The deck is a decision-triage queue: an operator answering a
	// plan's question here could not see how far along the plan is — a decision on
	// a barely-begun plan (0%) read identically to one on a nearly-finished plan
	// (90%), yet "how much is already built" is exactly the context that colors
	// the answer. These ride the SAME StepsClosed/StepsTotal/PctDone the enriched
	// planRowView already carries (planIndexEnrich's renderedStepRollup, the same
	// numbers the index card's progress bar + label render), so the facet costs
	// the deck ZERO extra daemon call and agrees with the index + cockpit by
	// construction. HasSteps gates the render (StepsTotal>0), so a no-steps plan
	// adds no ink. Calm neutral (context, not an alarm — the deck's loud tiers are
	// the attention Reason + the ⚠ stuck crew facet). Scoped to the inline stack
	// card ONLY (deck.html, built from enriched rows); the lean answer-in-place
	// fragment (handlePlanCard / renderDeckCard) does NOT carry it, preserving that
	// path's 2-round-trip budget — it fetches only the epic + top decision and
	// never the step subtree.
	StepsClosed int
	StepsTotal  int
	PctDone     int
	// StepsInProgress / StepsBlocked / StepsAllDone surface the plan's per-status
	// STEP BREAKDOWN on the deck card face (ka-0000005352 iter-84) — the index card
	// face renders the "▶ N in progress · ⛔ N blocked" step-tally pills
	// (plan_card.html, iter and MR history) plus the calm "✓ steps done"
	// ghost-done marker (StepsAllDone, iter-41), but the /deck card carried only
	// the "N/M steps · X%" progress facet (iter-67) — the HOW-FAR without the
	// WHICH-DISPOSITION. So a 4/10 plan with 3 steps actively running read
	// identically to a 4/10 plan with 3 steps BLOCKED (the second needs help), and
	// a plan whose rendered steps are ALL closed but was never closed out (the
	// ghost-done case) read identically to a genuinely mid-flight plan. These ride
	// the SAME enriched planRowView fields planIndexEnrich already sets
	// (row.StepsInProgress / .StepsBlocked from the tallyPlanGrid pass;
	// row.StepsAllDone from the all-closed-but-non-terminal guard) and deckCards
	// already builds from — so the fold costs the deck ZERO extra daemon call and
	// agrees with the index + cockpit by construction (two-derivations-must-agree,
	// advice ka-0000005924). The tally pills render only when non-zero, so a plan
	// with nothing in flight and nothing blocked adds no ink; StepsAllDone already
	// folds the !planStatusTerminal guard, so a genuinely CLOSED/DEFERRED plan's
	// 100% face stays calm history with no marker. The tally and the ghost marker
	// are mutually exclusive by construction (an all-done plan has zero in-flight
	// and zero blocked steps). Scoped to the inline stack card ONLY (deck.html,
	// built from enriched rows); the lean answer-in-place fragment (handlePlanCard
	// / renderDeckCard) does NOT carry it, preserving that path's round-trip budget.
	StepsInProgress int
	StepsBlocked    int
	StepsAllDone    bool
	// PhaseCur / PhaseTotal surface a MULTI-PHASE plan's phase MILESTONE on the
	// deck card face (ka-0000005352 iter-90) — the "· phase N/M" facet the /plans
	// index card face carries (plan_card.html, .plan-progress-phase, iter-58) but
	// the /deck card DROPPED. The deck already names how FAR through the TASKS a
	// plan is (StepsClosed/StepsTotal/PctDone, iter-67) and WHICH step is in flight
	// (CurrentOrNextStep, iter-83), but never WHICH PHASE of how many is running —
	// the coarse lifecycle milestone the mission explicitly names ("add lifecycle/
	// step-breakdown signals"). PhaseCur/PhaseTotal ride the SAME enriched
	// planRowView fields planIndexEnrich already sets (planPhasePosition, derived
	// off the same Grid the card renders), so the fold costs the deck ZERO extra
	// daemon call and agrees with the index + cockpit by construction (two-
	// derivations-must-agree, advice ka-0000005924). Rendered only when
	// PhaseTotal>=2 (the index's own gate), so a single-phase or pure-flat plan
	// adds no ink — the "signal only where it means something" discipline. Scoped
	// to the inline stack card ONLY (deck.html, built from enriched rows); the lean
	// answer-in-place fragment (handlePlanCard / renderDeckCard) does NOT carry it,
	// preserving that path's round-trip budget.
	PhaseCur   int
	PhaseTotal int
	Ago        string
	AgoRFC     string
	// AgoCompact / FreshnessStale surface the plan's "last change" freshness on
	// the deck card face in the SAME shape the /plans index card head
	// (.plan-card-updated, plan_card.html) and the peek header
	// (.plan-peek-updated, plan_peek.html) render (ka-0000005352 iter-86). The
	// deck already carried the WIDE Ago ("7 hours ago"), but that vocabulary is
	// both wider than the compact "7h"/"2d"/"1w" the index+peek use (the mission's
	// "more info in LESS space") AND carried NO stale-tint — so a plan that has not
	// moved in 3 weeks read calm on the deck while the same plan pops loud-red on
	// the index. AgoCompact is the compact instant (fmtTimeAgo); FreshnessStale
	// marks an ACTIVE plan whose newest activity crossed planFreshnessStaleThreshold
	// (7d, gated on non-terminal status so a genuinely finished plan's honest
	// "3 weeks ago" stays calm history). Both ride the SAME enriched planRowView
	// fields planIndexEnrich already sets (row.AgoCompact / row.FreshnessStale, the
	// exact values the index head + peek stamp render) — zero extra daemon call, and
	// deck↔index↔peek agree by construction (advice ka-0000005924).
	AgoCompact     string
	FreshnessStale bool
	// Reason is the human-readable "why is this card in front of you" line —
	// the rendered form of the planAttention tier. The deck's whole promise is
	// that the top card deserves to be the top card, so it says why.
	Reason string
	// Tier is the raw attention tier, carried for the section styling and so a
	// test can assert the rendered order matches sortPlanRows.
	Tier planAttentionTier
	// Decision is the ONE question this card asks. nil when the plan is on the
	// deck for a non-question reason (ready-to-build, blocked, stalled), in
	// which case the card renders its action affordance instead.
	Decision *deckCardDecision
	// QuestionLead / QuestionRest split a long prompt so the ANSWER BUTTONS stay
	// above the fold on a phone. Measured against live prod: real gardener prompts
	// run several hundred characters and, rendered whole, pushed every option off
	// a 390x844 screen — which defeats the deck (you cannot answer what you
	// cannot see without scrolling first).
	//
	// The split is rendered as a native <details>: the lead is the <summary>, the
	// rest is the body. That is deliberately an HTML element and not a JS toggle,
	// because the deck's no-JS contract has to hold here too — <details> expands
	// with scripting fully disabled.
	//
	// QuestionRest is empty for a short prompt, in which case the card renders it
	// as a plain paragraph with no disclosure at all.
	QuestionLead string
	QuestionRest string
	// Escalations is the count of open escalations blocking this plan. v1 keeps
	// escalations READ-ONLY: they are capability-gated and the write verb lives
	// in the slack-bridge binary (api_escalations.go), not here — so the card
	// renders "blocked, tap to review" and deep-links out rather than offering
	// an action that would 404 or 403.
	Escalations int
	// EscalationID is the first blocking escalation, for that deep link.
	EscalationID string
	// StallReason is the tooltip text when the card is on the deck because the
	// plan is wedged (deriveStall's reason string).
	StallReason string
	// Crew is the compact live-crew tally the deck's inline stack card renders
	// (ka-0000005352 iter-66). The mission is "surface live crew on EACH card, not
	// just the cockpit" — the index card (ka-0000004801) and the peek drawer
	// (iter-65) both carry it, but the /deck mobile card face showed ZERO crew, so
	// an operator flipping the triage deck could answer a plan's question without
	// ever seeing that N crew are already working (or stuck) on it. This is the
	// SAME reconciled planCrewRollup the /plans index chip renders — it rides the
	// enriched planRowView the deck already builds (planIndexEnrich populates
	// row.Crew, reconcileTerminal quiets a terminal plan), so it costs the inline
	// deck render ZERO extra daemon call. Scoped to the inline stack card ONLY
	// (deck.html, built from enriched rows); the lean answer-in-place fragment
	// (handlePlanCard / renderDeckCard, GET /plans/{id}/card) does NOT carry it,
	// preserving that path's 2-round-trip budget (deckCardRoundTripBudget) — the
	// lean fragment fetches only the epic + top decision and never the crew
	// subtree.
	Crew planCrewRollup
	// CurrentStep is the plan's current-or-next step — the compact "NOW/NEXT #N
	// <title>" chip the /plans index card face renders (plan_card.html,
	// CurrentOrNextStep) but the /deck card DROPPED (ka-0000005352 iter-83). The
	// deck names how far along a plan is (N/M steps · X%, iter-67) yet not WHICH
	// step is in flight — the single most actionable "what is happening right now"
	// signal an operator triaging the deck needs: a 40%-done plan whose live step
	// is #13 "e2e bug fix" (or one wedged/blocked) reads identically to any other
	// 40% plan without it. Rides the SAME row.CurrentOrNextStep planIndexEnrich
	// already populates on the enriched planRowView the deck builds from (the
	// index card's exact projection, planStatusTerminal-guarded upstream so a
	// terminal plan carries none) — so it costs the inline deck render ZERO extra
	// daemon call and deck↔index agree by construction. nil on a terminal /
	// step-less plan, in which case the card renders no step chip. Scoped to the
	// inline stack card ONLY (deck.html, built from enriched rows); the lean
	// answer-in-place fragment (handlePlanCard / renderDeckCard) does NOT carry it,
	// preserving that path's 2-round-trip budget — it fetches only the epic + top
	// decision and never the step subtree.
	CurrentStep *planCardStep
}

// deckView is the whole page's data bag.
type deckView struct {
	Cards []deckCardItem
	// Position is the 0-based index of the ACTIVE card within Cards. It is
	// carried in the URL (?at=) rather than in JS state precisely so a reload —
	// including layout.html's 401 re-auth force-reload — lands on the same card.
	Position int
	// Total is len(Cards), for the "3 of 12" progress readout.
	Total int
	// Skipped is the count of cards the operator has already skipped this
	// session, carried in the URL so a skip is remembered across the reload.
	Skipped int
	// IsOperator gates every action affordance, exactly as the /plans index and
	// the card fragment do.
	IsOperator bool
	// ProjectFilter is the active ?project= scope ("" = all projects).
	ProjectFilter string
	// Projects is the project list for the scope selector.
	Projects []string
	// Error is a page-level failure message (a daemon error on the list call).
	Error string
	// Empty is true when the deck has no cards — the "you're all caught up"
	// state, which on this surface is a genuine success, not a degenerate case.
	Empty bool
	// BackURL / SkipURL are the FULL hrefs the Back and Skip links navigate to.
	// Built SERVER-SIDE so the two nav controls are real links that work with JS
	// off and survive a reload — the position and the skip list live in the URL,
	// which is the whole reason a mid-flip re-auth (layout.html's 401
	// force-reload) doesn't lose your place.
	//
	// They are whole URLs rather than bare query strings on purpose: html/template
	// escapes an action interpolated AFTER a literal "?" as a query VALUE, turning
	// "at=1&skip=x" into "at%3d1%26skip%3dx" and breaking the link. Handing the
	// template one complete URL puts it in URL context, where "?", "=" and "&"
	// survive.
	BackURL string
	SkipURL string
}

// deckNavURLs builds the Back/Skip link targets for the current position.
//
// Back simply decrements ?at=. Skip advances ?at= AND appends the current plan
// id to ?skip=, which is what moves that card to the BACK of the queue on the
// next render (applyDeckSkips) rather than dropping it. Both preserve the
// active ?project= scope so navigating never silently widens the deck.
func deckNavURLs(v *deckView, basePath, skipRaw string) {
	build := func(at int, skip string) string {
		q := make(url.Values)
		if v.ProjectFilter != "" {
			q.Set("project", v.ProjectFilter)
		}
		if at > 0 {
			q.Set("at", strconv.Itoa(at))
		}
		if skip != "" {
			q.Set("skip", skip)
		}
		u := basePath + "/deck"
		if enc := q.Encode(); enc != "" {
			u += "?" + enc
		}
		return u
	}
	v.BackURL = build(v.Position-1, skipRaw)
	current := ""
	if v.Position >= 0 && v.Position < len(v.Cards) {
		current = v.Cards[v.Position].PlanID
	}
	// Skip advances to the next slot. The skipped id is remembered in ?skip= so
	// the NEXT full render pushes it to the back instead of losing it.
	v.SkipURL = build(v.Position+1, deckSkipParam(skipRaw, current))
}

// handleDeck renders the mobile card-deck (Step 4, ka-0000000701).
//
// The whole queue is built with ONE plan-list round-trip plus the SAME batched
// enrich pass /plans uses (planIndexEnrich — bounded batch joins, no per-plan
// fan-out), then ordered by sortPlanRows. The deck therefore costs what one
// /plans render costs, not N card renders: the per-card fragment endpoint
// (Step 3) exists for incremental refresh after an answer, not for building the
// stack.
//
// Cursor semantics. ?at= is the active index and ?skip= is a comma-separated
// list of plan ids already skipped this session. Both live in the URL so:
//   - a reload (including the 401 re-auth force-reload) keeps your place;
//   - Back is a real link to at-1, so browser Back works;
//   - a skipped card is NOT lost — skipped ids are moved to the BACK of the
//     queue rather than dropped, so a full pass still shows you everything.
func (s *Server) handleDeck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectFilter := strings.TrimSpace(r.URL.Query().Get("project"))
	isOperator := viewerCanOperate(r)

	view := deckView{
		IsOperator:    isOperator,
		ProjectFilter: projectFilter,
	}

	// The project scope selector reuses the /plans project list helper, so the
	// two surfaces offer the same scopes.
	view.Projects = s.planProjectOptions(ctx, projectFilter)

	// ONE list round-trip. Deliberately UNSCOPED by operator: the deck answers
	// "what needs a human", and a question waiting 30 days needs an answer from
	// whoever picks up the phone, not only from the plan's author. This is the
	// opposite default from /plans (which scopes to your own work by default)
	// and it is intentional — the deck exists to drain a shared backlog.
	q := beadsapi.ListBeadsQuery{
		Types:            []string{"epic"},
		Labels:           []string{planLabel},
		Statuses:         planStatusEnum(planStatusActive),
		Sort:             "-created_at",
		ExcludeScheduled: true,
		Limit:            deckQueueLimit,
	}
	if projectFilter != "" {
		q.Labels = append(q.Labels, "project:"+projectFilter)
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, q)
	if err != nil {
		s.logger.Error("deck: listing plan epics", "error", err)
		view.Error = "Failed to load the deck."
		s.render(w, r, "deck.html", deckTemplateData(view))
		return
	}

	// The SAME batched enrich + ranking the /plans index uses. Reusing it is the
	// point: the deck cannot disagree with /plans about which plan is urgent,
	// because it is literally the same ordering function over the same fields.
	rows := s.planIndexEnrich(ctx, res.Beads)
	sortPlanRows(rows)

	// Only plans that ACTUALLY need a human belong on the deck. sortPlanRows
	// ranks everything; deckCards filters to the four attention tiers, dropping
	// the healthy residual feed. A deck padded with "nothing to do here" cards
	// trains the operator to swipe without reading, which defeats the surface.
	cards := deckCards(rows)

	// Apply the skip list: skipped ids move to the BACK, they are never dropped.
	// "Skip is free and remembered" — the operator can flip past a hard question
	// without that question falling out of the world.
	skipRaw := r.URL.Query().Get("skip")
	skipped := parseDeckSkips(skipRaw)
	cards, skipCount := applyDeckSkips(cards, skipped)

	view.Cards = cards
	view.Total = len(cards)
	view.Skipped = skipCount
	view.Empty = len(cards) == 0
	view.Position = clampDeckPosition(r.URL.Query().Get("at"), len(cards))
	deckNavURLs(&view, s.basePath, skipRaw)

	// Hydrate the ACTIVE card's question (and the prefetched next few) with the
	// same lean top-decision fetch the card fragment uses. Only the visible
	// window pays for it — a 60-card queue does not issue 60 decision lists.
	s.hydrateDeckDecisions(ctx, view.Cards, view.Position)

	s.render(w, r, "deck.html", deckTemplateData(view))
}

// deckTemplateData flattens the view into the map the render seam takes.
func deckTemplateData(v deckView) map[string]any {
	return map[string]any{
		"Cards":         v.Cards,
		"Position":      v.Position,
		"Total":         v.Total,
		"Skipped":       v.Skipped,
		"IsOperator":    v.IsOperator,
		"ProjectFilter": v.ProjectFilter,
		"Projects":      v.Projects,
		"Error":         v.Error,
		"Empty":         v.Empty,
		"BackURL":       v.BackURL,
		"SkipURL":       v.SkipURL,
	}
}

// deckCards projects the ranked plan rows into deck cards, keeping ONLY the
// plans in an attention tier — the ones where a human action actually unblocks
// something. planAttentionNone (healthy, moving, or idle plans) is dropped: the
// deck is an action queue, not a browser.
//
// Order is preserved exactly as sortPlanRows left it, so the deck's sequence IS
// the index's ranking — decision → escalation → ready → stalled, each tier
// draining oldest-waiting-first.
func deckCards(rows []planRowView) []deckCardItem {
	out := make([]deckCardItem, 0, len(rows))
	for _, r := range rows {
		tier := planAttention(r)
		if tier == planAttentionNone {
			continue
		}
		item := deckCardItem{
			PlanID:   r.ID,
			Title:    r.Title,
			Heraldry: r.Heraldry,
			Badge:    r.Badge,
			Project:  r.Project,
			Status:   r.Status,
			// Reconcile the DISPLAY chip against the epic's terminal status
			// (ka-0000005352 iter-28, planLifecycleChipState) — the same iter-27 fix
			// the index card already applies to row.LifecycleChip, extended to the
			// /deck mobile card so a terminal plan (a deferred plan can still enter
			// the deck via a stalled/blocked tier) never shows a stale
			// "building"/"draft"/"ready" chip. Display-only: the deck's "Approve &
			// build" affordance gates on the Go-derived planAttentionReady Tier, not
			// this chip value, so the raw plan_state is not needed on the card face.
			PlanState: string(planLifecycleChipState(planState(r.PlanState), r.Status)),
			Priority:  r.Priority,
			// Step progress off the SAME enriched rollup the index card renders
			// (ka-0000005352 iter-67) — zero extra fetch, agrees with /plans + the
			// cockpit by construction.
			StepsClosed: r.StepsClosed,
			StepsTotal:  r.StepsTotal,
			PctDone:     r.PctDone,
			// Per-status step breakdown + ghost-done marker off the SAME enriched
			// tally the index card renders (ka-0000005352 iter-84) — zero extra
			// fetch, agrees with /plans + the cockpit by construction.
			StepsInProgress: r.StepsInProgress,
			StepsBlocked:    r.StepsBlocked,
			StepsAllDone:    r.StepsAllDone,
			// Phase-position milestone off the SAME enriched planPhasePosition the
			// index card renders (ka-0000005352 iter-90) — zero extra fetch, agrees
			// with /plans + the cockpit by construction.
			PhaseCur:       r.PhaseCur,
			PhaseTotal:     r.PhaseTotal,
			Ago:            r.Ago,
			AgoRFC:         r.AgoRFC,
			AgoCompact:     r.AgoCompact,
			FreshnessStale: r.FreshnessStale,
			Tier:           tier,
			Reason:         deckReason(tier, r),
			Escalations:    r.Escalations.Count,
			StallReason:    r.Stall.Reason,
			// Zero extra fetch: r.Crew is the already-reconciled rollup
			// planIndexEnrich set on the enriched row (reconcileTerminal quiets a
			// terminal plan). The deck adds no new enrichment, so its crew tally
			// agrees with the /plans index chip by construction.
			Crew: r.Crew,
			// The current-or-next step chip (ka-0000005352 iter-83) off the SAME
			// row.CurrentOrNextStep the index card renders. planIndexEnrich already
			// picked + terminal-guarded it (a closed/deferred plan carries nil, so
			// the deck shows no step chip on a terminal plan exactly like the index),
			// so this is a zero-extra-fetch fold that agrees with /plans by
			// construction.
			CurrentStep: r.CurrentOrNextStep,
		}
		if len(r.Escalations.Escalations) > 0 {
			item.EscalationID = r.Escalations.Escalations[0].ID
		}
		out = append(out, item)
	}
	return out
}

// deckReason renders the one-line "why is this in front of you" label. The deck
// promises the top card deserves the top slot; saying why in plain words is how
// that promise is kept legible rather than asserted.
func deckReason(tier planAttentionTier, r planRowView) string {
	switch tier {
	case planAttentionDecision:
		if r.OpenDecisions > 1 {
			return "Waiting on you — " + strconv.Itoa(r.OpenDecisions) + " open questions"
		}
		return "Waiting on you — a question needs an answer"
	case planAttentionEscalation:
		if r.Escalations.Count > 1 {
			return "Blocked — " + strconv.Itoa(r.Escalations.Count) + " open escalations"
		}
		return "Blocked — an escalation needs a privileged action"
	case planAttentionReady:
		return "Ready — one tap approves the plan and starts the build"
	case planAttentionStalled:
		if r.Stall.Reason != "" {
			return "Stalled — " + r.Stall.Reason
		}
		return "Stalled — open work with nothing watching it"
	default:
		return ""
	}
}

// hydrateDeckDecisions fetches the card FACE question for the active card and
// the next deckPrefetchAhead cards — the visible window only.
//
// This is the deliberate bound on the deck's cost. The alternative (hydrating
// every queued card) would issue up to deckQueueLimit decision lists on a page
// the operator may close after two cards. Cards outside the window render their
// header and, when the operator reaches them, the client swaps in the lean
// fragment from GET /plans/{id}/card (Step 3, budgeted at <10 KB / 2 round
// trips) — which is exactly what that endpoint was built for.
func (s *Server) hydrateDeckDecisions(ctx context.Context, cards []deckCardItem, from int) {
	if len(cards) == 0 {
		return
	}
	end := from + 1 + deckPrefetchAhead
	if end > len(cards) {
		end = len(cards)
	}
	for i := from; i < end; i++ {
		if i < 0 || i >= len(cards) {
			continue
		}
		// Only the decision tier has a question to show. A ready/stalled/blocked
		// card's affordance is its action, so it costs no fetch at all.
		if cards[i].Tier != planAttentionDecision {
			continue
		}
		cards[i].Decision = s.deckCardTopDecision(ctx, cards[i].PlanID)
		if cards[i].Decision != nil {
			cards[i].QuestionLead, cards[i].QuestionRest = splitDeckQuestion(cards[i].Decision.Prompt)
		}
	}
}

// deckQuestionLeadLen is the prompt length above which the card collapses the
// tail behind a disclosure. Tuned against real prod prompts at 390x844: about
// this much text plus the card header still leaves the first answer buttons on
// screen, which is the property that matters — the deck's promise is that you
// can answer without scrolling first.
const deckQuestionLeadLen = 180

// splitDeckQuestion splits a prompt into an above-the-fold lead and a collapsed
// remainder. It breaks at a SENTENCE boundary when one falls in a reasonable
// window, and otherwise at a word boundary, so the summary never ends mid-word.
// A short prompt returns (whole, "") — no disclosure, just a paragraph.
func splitDeckQuestion(prompt string) (lead, rest string) {
	p := strings.TrimSpace(prompt)
	if len(p) <= deckQuestionLeadLen {
		return p, ""
	}
	// Prefer the last sentence end inside the budget — a lead that stops on a
	// full stop reads as a complete thought rather than a truncation.
	cut := -1
	for i := range deckQuestionLeadLen {
		if i >= len(p) {
			break
		}
		if p[i] == '.' || p[i] == '?' || p[i] == '!' {
			// Only treat it as a boundary if a space (or the end) follows, so
			// "ka-0000001682." style ids don't split the line.
			if i+1 >= len(p) || p[i+1] == ' ' || p[i+1] == '\n' {
				cut = i + 1
			}
		}
	}
	// A sentence break that lands too early wastes the fold; fall back to words.
	if cut < deckQuestionLeadLen/3 {
		cut = strings.LastIndex(p[:deckQuestionLeadLen], " ")
		if cut < 0 {
			cut = deckQuestionLeadLen
		}
	}
	return strings.TrimSpace(p[:cut]), strings.TrimSpace(p[cut:])
}

// parseDeckSkips reads the ?skip= cursor — the comma-separated plan ids the
// operator has flipped past this session. Bounded so a hand-edited or
// accumulated URL cannot turn into an unbounded map.
func parseDeckSkips(raw string) map[string]struct{} {
	out := make(map[string]struct{})
	if raw == "" {
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		out[id] = struct{}{}
		if len(out) >= deckQueueLimit {
			break
		}
	}
	return out
}

// applyDeckSkips moves already-skipped cards to the BACK of the queue, in place
// of dropping them. This is the "skip is free and remembered" rule: flipping
// past a question you cannot answer right now must not delete it from the
// world, or the deck becomes a way to lose work rather than drain it. Returns
// the reordered deck and how many cards were pushed back.
func applyDeckSkips(cards []deckCardItem, skipped map[string]struct{}) ([]deckCardItem, int) {
	if len(skipped) == 0 || len(cards) == 0 {
		return cards, 0
	}
	front := make([]deckCardItem, 0, len(cards))
	back := make([]deckCardItem, 0, len(skipped))
	for _, c := range cards {
		if _, dup := skipped[c.PlanID]; dup {
			back = append(back, c)
			continue
		}
		front = append(front, c)
	}
	return append(front, back...), len(back)
}

// clampDeckPosition parses the ?at= cursor into a valid index. Out-of-range and
// unparseable values clamp to a valid card rather than erroring: a cursor is a
// convenience, and a stale one (a card answered from another device, shrinking
// the deck under you) must land you on a real card, not a 400.
func clampDeckPosition(raw string, total int) int {
	if total <= 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	if n >= total {
		return total - 1
	}
	return n
}

// deckSkipParam appends id to a ?skip= cursor, preserving the existing entries
// and de-duplicating. It is what the Skip link's href is built from, so a skip
// survives in the URL and therefore survives a reload.
func deckSkipParam(existing, id string) string {
	if id == "" {
		return existing
	}
	for _, part := range strings.Split(existing, ",") {
		if strings.TrimSpace(part) == id {
			return existing
		}
	}
	if strings.TrimSpace(existing) == "" {
		return id
	}
	return existing + "," + id
}
