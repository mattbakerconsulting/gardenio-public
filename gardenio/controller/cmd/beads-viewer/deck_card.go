package main

// deck_card.go — the LEAN deck card fragment (plan ka-0000000492 step 3,
// task ka-0000004313). GET /plans/{id}/card renders a plan as a single
// swipe-through triage card: its title + link, project, age, and the ONE
// clarifying-question the plan is waiting on (prompt + tappable option
// buttons). Nothing else.
//
// Why a NEW fragment instead of reusing the peek drawer:
//
//	The peek (GET /plans/{id}/peek, plans_board.go) is a deliberately RICH
//	cut of the cockpit — it renders the full step timeline, the crew roster,
//	the plan-agent chat transcript, the pour picker, and the gardener chip,
//	because Matt asked the drawer to answer "who's on it / is it live"
//	(thread ka-0000002125). That richness measured 146 KB / 1.7 s per open —
//	fine for a click-to-inspect drawer, far too heavy to FLIP THROUGH one
//	card at a time on a phone. Prefetching the next card (the deck's whole
//	premise) is impossible at that weight.
//
//	This fragment is the opposite bias: carry ONLY what the card face shows.
//	It reuses none of the heavy builders (no planTimeline, no planCrew, no
//	planTranscriptView, no planGardenerBarData, no grid) — the weight the peek
//	pays is exactly the weight a deck cannot afford. The result is a ~1-2 KB
//	fragment served in 2 bounded daemon round-trips, O(1) per card, enforced
//	by TestDeckCard_SizeAndRoundTripBudget so a regression reds the build.
//
// The budget is load-bearing, not cosmetic: Step 4 (GET /deck) prefetches the
// NEXT card while the operator reads the current one, and Step 5 swipes
// between them — both assume a card is cheap enough to fetch speculatively.

import (
	"context"
	"html/template"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// deckCardOption is one answer option on the card face's decision — the
// minimal (id/short/label) projection the tappable buttons render. It is a
// deliberately narrower cut of decisionOption (no ArtifactType): the card
// shows the choice, not its provenance.
type deckCardOption struct {
	ID          string
	Short       string
	Label       string
	Description string
}

// deckCardDecision is the ONE clarifying-question the card surfaces — the
// plan's freshest open decision (the gardener's latest ask). Prompt is carried
// as PLAIN TEXT, not markdown-rendered HTML: the card face shows a short
// question, and skipping the goldmark render keeps both the CPU cost and the
// payload down (a peek renders full markdown; a deck card must not need to).
// Context is the one Markdown render the card pays for: the agent's context
// is what makes the question answerable, so it shows above the answers
// (gd-PNtilz1bgs), clamped past ~600 chars.
type deckCardDecision struct {
	ID      string
	Prompt  string
	Context decisionContextView
	// Evidence is the decision's screenshot strip under the context
	// (gd-Pi7C83262N): thumbnails that load lazily, read through the short
	// evidence cache, so a card the deck prefetches again costs no reads.
	Evidence template.HTML
	Options  []deckCardOption
}

// deckCardView is the whole card fragment's data bag — the lean parallel to
// the peek's ~20-field bag. Everything here is either already on the plan
// epic (Title/Project/Status/PlanState/Ago, zero extra fetch) or the single
// top open decision. No step, crew, MR, chat, or grid data is present by
// design — that is what makes the card lean.
type deckCardView struct {
	PlanID     string
	Title      string
	ShortTitle string      // shown in place of Title when set (gd-V5nGseRS5f)
	Heraldry   string      // the plan's arms, canonical; "" when none (arms.go)
	Badge      *badgeImage // the card cover; nil when the plan has no badge (badge_image.go)
	Project    string
	Status     string
	PlanState  string
	// Ago / AgoRFC are the plan epic's own last-touched stamp — the cheap,
	// always-present "how fresh is this" signal (fmtTimeAgo's compact form for
	// the face, RFC3339 for the tooltip). The card deliberately does NOT walk
	// the step subtree for planLastActivity (a daemon round-trip); the deck's
	// ranker (Step 2) owns sophisticated ordering, the card face just needs a
	// cheap freshness cue.
	Ago    string
	AgoRFC string
	// Decision is the plan's top open clarifying-question — the card FACE's
	// question + option buttons. nil when the plan has no open decision, in
	// which case the card renders just its header + full-cockpit link.
	Decision *deckCardDecision
	// IsOperator gates the tappable one-tap resolve forms exactly as the peek /
	// cockpit decision chips do (the SAME POST /plans/{id}/decisions/{d}/resolve
	// route). A read-only viewer sees the options as static chips.
	IsOperator bool
}

// handlePlanCard renders the lean deck card fragment for one plan epic. It is
// the deck's per-card endpoint — Step 4's /deck page mounts these, and its
// prefetch logic fetches the NEXT one speculatively, so the whole surface
// leans on this staying cheap (TestDeckCard_SizeAndRoundTripBudget).
//
// Viewer-scoped like the peek it slims: the option buttons inside gate on the
// viewer's operate role exactly as the cockpit does.
func (s *Server) handlePlanCard(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "missing plan id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	// Round-trip 1: the plan epic itself. Mirrors handlePlanPeek's not-found /
	// non-epic handling so /card and /peek agree on what "a plan" is.
	bead, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		if beadMissing(err) {
			http.Error(w, "Plan not found", http.StatusNotFound)
			return
		}
		s.logger.Error("plan card: get bead", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if bead == nil || bead.Type != "epic" {
		http.Error(w, "Plan not found", http.StatusNotFound)
		return
	}

	view := deckCardView{
		PlanID:     bead.ID,
		Title:      bead.Title,
		ShortTitle: bead.ShortTitle,
		Heraldry:   bearerArms(bead),
		Badge:      beadBadge(bead),
		Project:    projectFromLabels(bead.Labels),
		Status:     bead.Status,
		// Reconcile the DISPLAY chip against the epic's terminal status
		// (ka-0000005352 iter-28, planLifecycleChipState) — the same iter-27 fix
		// the index card + cockpit header carry, extended to the deck card so a
		// closed/deferred plan never shows a stale "building"/"draft"/"ready" chip
		// here while the index correctly suppresses it. Display-only: this card
		// renders no lifecycle GATE off PlanState (the deck's "Approve & build"
		// gates on the Go-derived planAttentionReady tier, not this chip), so the
		// raw plan_state is not needed anywhere on the fragment.
		PlanState:  string(planLifecycleChipState(planStateOf(bead), bead.Status)),
		Ago:        fmtTimeAgo(bead.UpdatedAt, s.now()),
		AgoRFC:     bead.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Decision:   s.deckCardTopDecision(ctx, id),
		IsOperator: viewerCanOperate(r),
	}

	s.renderPartial(w, r, "templates/partials/deck_card.html", "deck_card", deckCardBag(view))
}

// deckCardBag flattens a deckCardView into the map the deck_card partial takes.
// Extracted so the two renderers that emit this fragment — GET /plans/{id}/card
// (handlePlanCard) and the answer-in-place seam (renderDeckCard) — build the
// SAME bag rather than duplicating the key list and risking divergence.
func deckCardBag(v deckCardView) map[string]any {
	return map[string]any{
		"PlanID":     v.PlanID,
		"Title":      v.Title,
		"ShortTitle": v.ShortTitle,
		"Heraldry":   v.Heraldry,
		"Badge":      v.Badge,
		"Project":    v.Project,
		"Status":     v.Status,
		"PlanState":  v.PlanState,
		"Ago":        v.Ago,
		"AgoRFC":     v.AgoRFC,
		"Decision":   v.Decision,
		"IsOperator": v.IsOperator,
	}
}

// renderDeckCard re-renders the LEAN deck_card fragment for one plan epic in
// place — the answer-in-place seam for the deck (Step 4, task ka-0000000701).
//
// This is the fix for the bug decision ka-0000001748 flagged on the first cut
// of Step 4 (MR !6302): the deck's answer/build forms POST to the SAME
// operator-gated /plans/{id}/decisions/{d}/resolve and /plans/{id}/build routes
// the cockpit uses, but those handlers return the COCKPIT fragments
// (#plan-decisions, the gardener bar) — full panels with no .deck-card. htmx
// swapping one of those into the deck's slot corrupted the JS stack tracking
// (JS-on) or navigated to a bare layout-less fragment (JS-off), breaking the
// deck's headline interaction and its own no-JS claim.
//
// The handlers now branch on a from_card=1 hidden input the deck forms carry
// (respondPlanDecisionResult / respondPlanBuild) and route through here, so a
// one-tap answer swaps the SAME lean-card shape back into the slot: the plan's
// now-resolved next question, a header-only "all clear" face when nothing else
// is open, or — on a failed write — an error banner ON the card (errMsg), which
// the deck's JS deliberately does NOT auto-advance past.
//
// It re-fetches the epic (one GetBead) + the fresh top decision (one bounded
// list) — the same two round-trips handlePlanCard pays, so the in-place refresh
// costs exactly what a card render costs and no more. A missing/non-epic id
// renders an error card rather than an http.Error, because the swap target is
// inside the deck slot: an http.Error body would be swapped in raw.
func (s *Server) renderDeckCard(w http.ResponseWriter, r *http.Request, epicID, errMsg string) {
	ctx := r.Context()
	bead, err := s.daemon.GetBead(ctx, epicID)
	if err != nil || bead == nil || bead.Type != "epic" {
		if err != nil {
			s.logger.Error("deck card refresh: get bead", "id", epicID, "error", err)
		}
		if errMsg == "" {
			errMsg = "Could not reload this card."
		}
		s.renderPartial(w, r, "templates/partials/deck_card.html", "deck_card", map[string]any{
			"PlanID":     epicID,
			"Title":      epicID,
			"ShortTitle": "",
			"Status":     "",
			"IsOperator": viewerCanOperate(r),
			"Error":      errMsg,
		})
		return
	}
	view := deckCardView{
		PlanID:     bead.ID,
		Title:      bead.Title,
		ShortTitle: bead.ShortTitle,
		Heraldry:   bearerArms(bead),
		Badge:      beadBadge(bead),
		Project:    projectFromLabels(bead.Labels),
		Status:     bead.Status,
		// Same terminal-status reconcile as handlePlanCard above (iter-28): the
		// answer-in-place re-render must render the identical chip the initial GET
		// did, or a one-tap answer on a terminal plan would flip its stale
		// lifecycle chip back on.
		PlanState:  string(planLifecycleChipState(planStateOf(bead), bead.Status)),
		Ago:        fmtTimeAgo(bead.UpdatedAt, s.now()),
		AgoRFC:     bead.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Decision:   s.deckCardTopDecision(ctx, epicID),
		IsOperator: viewerCanOperate(r),
	}
	bag := deckCardBag(view)
	bag["Error"] = errMsg
	s.renderPartial(w, r, "templates/partials/deck_card.html", "deck_card", bag)
}

// deckCardTopDecision issues round-trip 2: the plan's single freshest open
// clarifying-question. It fetches exactly ONE decision (Limit=1) and projects
// only id/short/label/description per option, the plain prompt and the
// context block — never the full
// planDecisions view (markdown render, resolution attribution, full option
// set). Returns nil on any miss so the card degrades to a header-only face.
//
// Sort=-created_at surfaces the gardener's LATEST ask ("what does this plan
// need from me right now"); the deck's cross-plan ORDER (oldest-waiting-first,
// Step 2's ranker) is a separate concern from which question one card shows.
func (s *Server) deckCardTopDecision(ctx context.Context, epicID string) *deckCardDecision {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Parent:   epicID,
		Sort:     "-created_at",
		Limit:    1,
	})
	if err != nil || res == nil || len(res.Beads) == 0 {
		if err != nil {
			s.logger.Debug("plan card: top decision", "epic", epicID, "error", err)
		}
		return nil
	}
	b := res.Beads[0]
	if b == nil {
		return nil
	}
	prompt := b.Fields["prompt"]
	if prompt == "" {
		prompt = b.Title
	}
	dec := &deckCardDecision{ID: b.ID, Prompt: prompt, Context: s.decisionContextOf(b), Evidence: s.decisionEvidenceOf(ctx, b)}
	for _, o := range parseDecisionOptions(b.Fields["options"]) {
		dec.Options = append(dec.Options, deckCardOption{
			ID:          o.ID,
			Short:       o.Short,
			Label:       o.Label,
			Description: o.Description,
		})
	}
	return dec
}
