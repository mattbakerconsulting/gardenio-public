package main

// plans_decisions.go — the plan's clarifying-question decisions: discovery across the subtree, render, resolve and dismiss.
// Split out of plans.go (gd-Wl6LuCnvlH); see plans.go for the Plans surface overview.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// planDecisionBeads returns the deduped union of the plan's clarifying-question
// decisions from BOTH surfacing paths, so the cockpit and its SSE interest set
// see the same set:
//
//   - PATH 1 — the transitive parent-child subtree (Types=[decision],
//     Parent=epicID): the daemon maps Parent to ParentSubtreeID, so a decision
//     at any parent-child depth under the plan epic is collected. This carries
//     the full open+resolved Q&A history.
//   - PATH 2 — the requesting_plan pin (Layer 2 of the orphan fix,
//     ka-0000007801 / design ka-0000006833 §3): OPEN decisions whose
//     fields.requesting_plan == epicID. gb decision create stamps that field to
//     the nearest plan-labeled epic ancestor at CREATE time (decision.go), so a
//     decision born inside the plan stays homed to it even after its origin step
//     closes or is reparented — the exact case where PATH 1 loses the edge. The
//     field JSONB equality filter is plumbed end-to-end (beadsapi
//     client_query.go → ?field=k=v → kbeads), so this is a pure read change.
//
// Deduped by bead id (a decision reached by both paths is collected once) with
// PATH 1 winning so the history's open+resolved ordering is preserved; the pin
// only ADDS the open forks PATH 1 misses. Reading a previously-unread field can
// only add rows, and the dedup prevents double-render. Shared by planDecisions
// (view models) and planDecisionIDs (SSE interest) so the rendered set and the
// live-update set can never drift.
func (s *Server) planDecisionBeads(ctx context.Context, epicID string) []*beadsapi.BeadDetail {
	seen := make(map[string]struct{})
	out := make([]*beadsapi.BeadDetail, 0, 8)
	add := func(res *beadsapi.ListBeadsResult) {
		if res == nil {
			return
		}
		for _, b := range res.Beads {
			if b == nil || b.ID == "" {
				continue
			}
			if _, dup := seen[b.ID]; dup {
				continue
			}
			seen[b.ID] = struct{}{}
			out = append(out, b)
		}
	}
	// PATH 1 — the transitive parent-child subtree (today's query).
	sub, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"decision"},
		Parent: epicID,
		Sort:   "-created_at",
		Limit:  100,
	})
	if err != nil {
		s.logger.Debug("plan decisions: subtree list", "epic", epicID, "error", err)
	}
	add(sub)
	// PATH 2 — the requesting_plan pin (durable homing; open forks only, since a
	// resolved decision needs no re-homing and the subtree already carries the
	// history).
	pin, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Fields:   map[string]string{"requesting_plan": epicID},
		Sort:     "-created_at",
		Limit:    100,
	})
	if err != nil {
		s.logger.Debug("plan decisions: requesting_plan pin", "epic", epicID, "error", err)
	}
	add(pin)
	return out
}

// planDecisionHomedTo reports whether dec is homed to plan epicID under the
// SAME two-path rule planDecisionBeads renders by. It is the write-side twin of
// that read: the resolve/dismiss membership guard calls it so the set of
// decisions the cockpit offers an Answer chip for is exactly the set those
// routes accept.
//
// The bug it fixes (ka-0000001446): the guard used to require a DIRECT
// parent-child edge from the decision to the epic, while the render collects
// the TRANSITIVE subtree ∪ the requesting_plan pin. Every decision homed by the
// other two routes — nested under a step/phase sub-epic, or pin-homed after its
// origin step closed — therefore rendered with a live Answer button that could
// only ever fail with "That decision is not part of this plan." Measured on the
// live board at the time of the fix: 16 of 17 open pin-homed decisions were
// unanswerable this way.
//
// The paths, in cost order (cheapest first, so the common cases never pay for a
// dependency walk):
//
//   - PATH 2 — the requesting_plan pin: a plain field read off the bead we
//     already hold. No round-trip.
//   - PATH 1 — the transitive parent-child subtree: walk the decision's forward
//     parent-child edges upward looking for epicID. This mirrors the daemon's
//     ParentSubtreeID recursion from the child side, which is the cheap
//     direction here — one decision has one short parent chain, whereas
//     re-issuing the subtree LIST would pull up to 100 sibling rows to answer a
//     yes/no about a single bead. Alias-tolerant via isPlanParentEdge, matching
//     the daemon's ParentChildDepTypes read set (legacy `parent` edges count).
//
// The walk is visit-capped and cycle-guarded: a parent-child cycle in corrupted
// data must not spin the request (the daemon carries the same concern, see the
// cycle-safety note on its subtree CTE). Exhausting the cap reads as "not
// homed" — the safe answer, since the guard is scoping, not privilege control.
func (s *Server) planDecisionHomedTo(ctx context.Context, epicID string, dec *beadsapi.BeadDetail) (bool, error) {
	if dec == nil || epicID == "" {
		return false, nil
	}
	// PATH 2 — the pin, free.
	if dec.Fields["requesting_plan"] == epicID {
		return true, nil
	}
	// PATH 1 — walk up the parent-child DAG, breadth-first.
	//
	// Breadth-first over ALL parents rather than following a single chain: a
	// bead may carry more than one parent-child edge, and descending only the
	// first would re-introduce a narrower version of the very bug this function
	// fixes (a plan reachable only through the second branch would read as "not
	// part of this plan"). In the single-parent shape that is essentially every
	// real plan, the frontier never exceeds one and this costs exactly what a
	// linear walk costs.
	//
	// The seen-set makes a cycle or a diamond terminate; visits is the hard
	// backstop that bounds a pathologically wide graph.
	seen := map[string]struct{}{dec.ID: {}}
	frontier := []string{dec.ID}
	for visits := 0; len(frontier) > 0 && visits < maxPlanDecisionParentWalk; {
		var next []string
		for _, cur := range frontier {
			if visits >= maxPlanDecisionParentWalk {
				break
			}
			visits++
			deps, err := s.daemon.GetDependencies(ctx, cur)
			if err != nil {
				return false, err
			}
			for _, d := range deps {
				if !isPlanParentEdge(d.Type) || d.DependsOnID == "" {
					continue
				}
				if d.DependsOnID == epicID {
					return true, nil
				}
				if _, dup := seen[d.DependsOnID]; dup {
					continue
				}
				seen[d.DependsOnID] = struct{}{}
				next = append(next, d.DependsOnID)
			}
		}
		frontier = next
	}
	return false, nil
}

// isPlanParentEdge reports whether a dep type stored on the CHILD names its
// parent. It is the viewer-side mirror of the daemon's ParentChildDepTypes read
// set (canonical `parent-child` + the legacy `parent` alias), widened with the
// `child-of` synonym the viewer's other structural walks already accept
// (isParentChildAlias). Read surfaces must match all of them or a legacy edge
// silently reads as "no parent" — the class of bug ka-0000008107 filed.
func isPlanParentEdge(t string) bool {
	return isParentChildAlias(t) || t == "parent"
}

// maxPlanDecisionParentWalk caps the number of beads planDecisionHomedTo's
// upward walk will VISIT (not its depth — the walk is breadth-first over a DAG
// that may fan out). Real plan hierarchies are epic → phase sub-epic → step →
// decision (depth 3) and single-parent throughout, so 16 visits is far above
// any legitimate shape while keeping a corrupted graph's cost bounded.
const maxPlanDecisionParentWalk = 16

// planDecisions returns the clarifying-question decisions homed to the plan,
// open first (newest), then resolved. These ARE the Cursor-style "one question
// at a time" exchanges — they already render at /decisions; here they are
// scoped to the plan so the cockpit shows the full Q&A history in place, and
// (task ka-0000003157) can be ANSWERED in place: each view carries the parsed
// options + markdown prompt + resolution attribution built through the same
// buildDecisionView the canonical decision card uses, so the inline render
// never diverges. isOperator gates the inline resolve/dismiss chips. The homed
// set is the parent-child subtree UNIONED with the requesting_plan pin
// (planDecisionBeads).
func (s *Server) planDecisions(ctx context.Context, epicID string, isOperator bool) []planDecisionView {
	beads := s.planDecisionBeads(ctx, epicID)
	out := make([]planDecisionView, 0, len(beads))
	for _, b := range beads {
		if b == nil {
			continue
		}
		v := planDecisionView{
			ID:       b.ID,
			Title:    b.Title,
			Prompt:   b.Fields["prompt"],
			Status:   b.Status,
			Ago:      humanizeTime(b.UpdatedAt),
			Resolved: b.Status == "closed",
		}
		// Reuse the canonical decision view builder for options, the
		// markdown-rendered prompt, and the resolution attribution so the
		// inline cockpit render matches /{id} exactly (no re-implementation).
		if dv := buildDecisionView(b, s.decisionMarkdown); dv != nil {
			v.PromptHTML = dv.PromptHTML
			v.RequestedBy = dv.RequestedBy
			v.Context = dv.Context
			v.Evidence = s.decisionEvidenceOf(ctx, b)
			v.Options = dv.Options
			v.Resolution = dv.Resolution
			// buildDecisionView derives Resolved from the resolution fields as
			// well as status; honour whichever marks it resolved so a decision
			// resolved via flat fields (but still status=open in a race) renders
			// as resolved and never offers stale chips.
			if dv.Resolved {
				v.Resolved = true
			}
		}
		v.CanResolve = isOperator && !v.Resolved
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return !out[i].Resolved && out[j].Resolved // open decisions first
	})
	return out
}

// planDecisionIDs returns just the ids of the plan's clarifying-question
// decisions — the SAME homed set planDecisions renders (parent-child subtree ∪
// requesting_plan pin, planDecisionBeads) — for the SSE interest set (task
// ka-0000003157). A lightweight id-only sweep so buildPlanEventFilters can track
// which decision beads should flash the #plan-decisions section without building
// the full view models. Routing through the shared collector keeps the interest
// set == the rendered set, so a pinned decision's answer/close streams live too.
func (s *Server) planDecisionIDs(ctx context.Context, epicID string) []string {
	beads := s.planDecisionBeads(ctx, epicID)
	out := make([]string, 0, len(beads))
	for _, b := range beads {
		if b != nil && b.ID != "" {
			out = append(out, b.ID)
		}
	}
	return out
}

// planDecisionsData builds the data bag the plan_decisions partial expects,
// reading the plan's clarifying-question decisions live and deriving the open
// count. Shared by the detail page, the HTMX re-render after resolve/dismiss,
// and the live-progress SSE stream so all three render the section identically
// (mirrors planStepsData). isOperator is passed in (not derived) because the
// SSE path has no per-event *http.Request to read roles off. errMsg surfaces an
// inline banner on a failed resolve/dismiss.
func (s *Server) planDecisionsData(ctx context.Context, epicID string, isOperator bool, errMsg string) map[string]any {
	decisions := s.planDecisions(ctx, epicID, isOperator)
	open := 0
	for _, d := range decisions {
		if !d.Resolved {
			open++
		}
	}
	return map[string]any{
		"PlanID":        epicID,
		"Decisions":     decisions,
		"OpenDecisions": open,
		"IsOperator":    isOperator,
		"Error":         errMsg,
		"BasePath":      s.basePath,
	}
}

// renderPlanDecisions re-renders the #plan-decisions fragment (the HTMX swap
// target after an inline resolve/dismiss). Re-reads live so the section is
// authoritative — the answered question flips to its resolved skin and the
// gardener's next question (a fresh decision bead) appears in place.
//
// ALWAYS 200: the body is a swappable fragment that carries any error in-band
// via the {{if .Error}} banner. htmx's default responseHandling does NOT swap
// 4xx/5xx bodies (it fires htmx:responseError and discards them), so returning
// a non-2xx status here would silently swallow the error banner and the
// answer-in-place surface would appear to do nothing. Genuine auth failures
// (unauthenticated / non-operator) never reach this path — they're rejected at
// the route layer / with http.Error before any render.
func (s *Server) renderPlanDecisions(w http.ResponseWriter, r *http.Request, epicID, errMsg string) {
	data := s.planDecisionsData(r.Context(), epicID, viewerCanOperate(r), errMsg)
	s.renderPartial(w, r, "templates/partials/plan_decisions.html", "plan_decisions", data)
}

// renderPlanDecisionsFragment renders plan_decisions to a string for the SSE
// plan-decisions-updated payload. Mirrors renderPlanStepsFragment (buffer
// render, "" on any error so the caller skips the frame rather than emitting a
// broken one). isOperator is the connected viewer's real operate verdict
// (handlePlanEvents derives it from the SSE request), so the live swap gates the
// inline resolve/dismiss chips exactly as the full-page render did — a read-only
// viewer's stream never grows operator controls it lacked.
func (s *Server) renderPlanDecisionsFragment(ctx context.Context, r *http.Request, epicID string, isOperator bool) string {
	tmpl, ok := s.partials["templates/partials/plan_decisions.html"]
	if !ok {
		s.logger.Error("plan events: plan_decisions partial not found")
		return ""
	}
	data := s.planDecisionsData(ctx, epicID, isOperator, "")
	bound, err := s.bindAgentLink(tmpl, r)
	if err != nil {
		s.logger.Error("plan events: bind plan_decisions template", "error", err)
		return ""
	}
	var buf bytes.Buffer
	if err := bound.ExecuteTemplate(&buf, "plan_decisions", data); err != nil {
		s.logger.Error("plan events: render plan_decisions", "epic", epicID, "error", err)
		return ""
	}
	return buf.String()
}

// handlePlanResolveDecision resolves one clarifying-question decision from the
// plan cockpit IN PLACE (task ka-0000003157) and re-renders #plan-decisions so
// the answered question flips to resolved and the gardener's next question
// appears without a reload. Delegates to the SAME daemon.ResolveDecision the
// canonical /{id} card uses (no new resolution logic); the only difference is
// the swap target is the whole plan-decisions section, not one decision card.
// Operator-gated at the route layer.
//
// Browser write-proxy (carve-out ka-0000004319; canonical banner in
// plans_lifecycle.go). RETAINED, not deleted — the browser cannot reach the
// daemon directly (unifiedHost off in every committed env, no browser
// daemon-bearer), so a browser Answer MUST forward through this same-origin POST
// to the daemon on the viewer's own bearer. Owns no plan-domain write logic: the
// resolve WRITE (state transition, option/response validation, DecisionAnswer
// rebind) is enforced daemon-side at POST /v1/decisions/{id}/resolve (kbeads
// http_decisions.go) regardless of caller — the same daemon endpoint `gb decision
// respond` / `gb plan answer` hit daemon-direct under BEADS_AUTH_TOKEN. This
// handler only forwards the form + RE-RENDERS the #plan-decisions fragment; the
// in-place cockpit update is ALSO delivered for free by the write-origin-agnostic
// SSE beads.closed re-render, so it loses nothing when a gardener answers daemon-
// direct instead. loadPlanDecision's checks are URL-scoping/UX guards, not plan
// invariants.
func (s *Server) handlePlanResolveDecision(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	decisionID := strings.TrimSpace(r.PathValue("decision"))
	if id == "" || decisionID == "" {
		http.Error(w, "Missing plan or decision id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	// from_card=1 marks a POST that came from a lean deck card (Step 4's /deck).
	// It swaps this handler's re-render from the cockpit #plan-decisions panel to
	// the lean deck_card fragment, so a one-tap deck answer refreshes the SAME
	// card shape in place instead of swapping a whole cockpit panel into the deck
	// slot (decision ka-0000001748's bug). The write itself is identical either
	// way — only the fragment the operator gets back differs by surface.
	fromCard := r.PostForm.Get("from_card") == "1"
	optionID := strings.TrimSpace(r.PostForm.Get("option_id"))
	responseText := strings.TrimSpace(r.PostForm.Get("response_text"))
	if optionID == "" && responseText == "" {
		s.respondPlanDecisionResult(w, r, id, fromCard, "Pick an option or write a response.")
		return
	}
	if len(responseText) > maxDecisionResponseLen {
		s.respondPlanDecisionResult(w, r, id, fromCard, "Response is too long (max 10000 characters).")
		return
	}

	if _, err := s.loadPlanDecision(r.Context(), id, decisionID); err != nil {
		s.respondPlanDecisionResult(w, r, id, fromCard, err.Error())
		return
	}

	if err := s.daemon.ResolveDecision(r.Context(), decisionID, beadsapi.ResolveDecisionRequest{
		SelectedOption: optionID,
		ResponseText:   responseText,
		RespondedBy:    email,
	}); err != nil {
		s.logger.Error("plan resolve decision", "plan", id, "decision", decisionID, "actor", email, "error", err)
		s.respondPlanDecisionResult(w, r, id, fromCard, "Failed to resolve decision.")
		return
	}
	s.respondPlanDecisionResult(w, r, id, fromCard, "")
}

// respondPlanDecisionResult writes the resolve/dismiss result on WHICHEVER
// surface issued the POST (task ka-0000000701). The cockpit (#plan-decisions,
// the default) and the deck's lean card answer to the SAME operator-gated
// resolve route; the only difference is the fragment swapped back — a cockpit
// carries the whole decisions panel, a deck card must get its own lean shape
// back or the swap corrupts the stack. from_card (the deck form's hidden input)
// is what selects renderDeckCard over renderPlanDecisions. errMsg threads
// through to whichever fragment renders — an inline banner on the panel, or the
// deck card's error face (which the deck's JS deliberately does not auto-drop).
func (s *Server) respondPlanDecisionResult(w http.ResponseWriter, r *http.Request, epicID string, fromCard bool, errMsg string) {
	if fromCard {
		s.respondDeckWrite(w, r, epicID, errMsg)
		return
	}
	s.renderPlanDecisions(w, r, epicID, errMsg)
}

// loadPlanDecision fetches decisionID and validates it is a resolvable
// clarifying-question decision that belongs to plan epicID (task ka-0000003157).
// It returns a user-facing error (safe to render inline) when the bead is
// missing, is not a decision, is already resolved, or is a decision under a
// DIFFERENT plan — so the plan-scoped resolve/dismiss routes never act on a
// decision outside their scope. Membership is decided by planDecisionHomedTo,
// which reuses the SAME homing the render path uses, so every decision the
// cockpit offers an Answer chip for can actually be answered.
func (s *Server) loadPlanDecision(ctx context.Context, epicID, decisionID string) (*beadsapi.BeadDetail, error) {
	dec, err := s.daemon.GetBead(ctx, decisionID)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("Decision not found.")
		}
		s.logger.Error("plan decision: get bead", "plan", epicID, "decision", decisionID, "error", err)
		return nil, fmt.Errorf("Failed to load decision.")
	}
	if dec == nil || dec.Type != "decision" {
		return nil, fmt.Errorf("That bead is not a decision.")
	}
	// Membership: the decision must belong to THIS plan, decided by the same
	// homing the render path uses (see planDecisionHomedTo). Guards against
	// POST /plans/{planA}/decisions/{decisionUnderPlanB}/resolve acting on a
	// question outside the addressed plan (both routes are operator-gated, so
	// this is a scoping/attribution correctness guard, not privilege control).
	member, merr := s.planDecisionHomedTo(ctx, epicID, dec)
	if merr != nil {
		s.logger.Error("plan decision: membership deps", "plan", epicID, "decision", decisionID, "error", merr)
		return nil, fmt.Errorf("Failed to verify the decision belongs to this plan.")
	}
	if !member {
		return nil, fmt.Errorf("That decision is not part of this plan.")
	}
	if dec.Status != "open" {
		return nil, fmt.Errorf("Decision is already resolved.")
	}
	return dec, nil
}

// handlePlanDismissDecision cancels one clarifying-question decision from the
// plan cockpit IN PLACE (task ka-0000003157) and re-renders #plan-decisions.
// Delegates to daemon.CancelDecision (the same path the /{id} card's Dismiss
// uses). Operator-gated at the route layer.
//
// Browser write-proxy (carve-out ka-0000004319; canonical banner in
// plans_lifecycle.go). RETAINED, not deleted — the browser cannot reach the
// daemon directly (see handlePlanResolveDecision). Owns no plan-domain write
// logic: the cancel WRITE is enforced daemon-side at POST
// /v1/decisions/{id}/cancel (kbeads http_decisions.go) regardless of caller —
// the same endpoint `gb decision cancel` / `gb plan dismiss` hit daemon-direct.
// This handler only forwards the reason + RE-RENDERS the fragment (the SSE
// beads.closed re-render also delivers the in-place update); loadPlanDecision's
// checks are URL-scoping/UX guards, not plan invariants.
func (s *Server) handlePlanDismissDecision(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	decisionID := strings.TrimSpace(r.PathValue("decision"))
	if id == "" || decisionID == "" {
		http.Error(w, "Missing plan or decision id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if len(reason) > maxDecisionDismissReasonLen {
		s.renderPlanDecisions(w, r, id, "Reason is too long (max 10000 characters).")
		return
	}

	if _, err := s.loadPlanDecision(r.Context(), id, decisionID); err != nil {
		s.renderPlanDecisions(w, r, id, err.Error())
		return
	}

	if err := s.daemon.CancelDecision(r.Context(), decisionID, reason, email); err != nil {
		s.logger.Error("plan dismiss decision", "plan", id, "decision", decisionID, "actor", email, "error", err)
		s.renderPlanDecisions(w, r, id, "Failed to dismiss decision.")
		return
	}
	s.renderPlanDecisions(w, r, id, "")
}

// planOpenDecisionCount is the single-plan open-decision count: one CountOnly
// query over the plan subtree's open decisions. The /plans INDEX no longer uses
// it — that count is folded into the batched child walk (planIndexStepData,
// Phase 2 ka-0000004522) to kill the per-row N+1. This helper survives for the
// single-plan surfaces that have no batched walk in hand: the board peek drawer
// (plans_board.go). It intentionally keeps the transitive-subtree Parent filter
// so a decision nested anywhere under the plan is counted on those surfaces.
func (s *Server) planOpenDecisionCount(ctx context.Context, epicID string) int {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:     []string{"decision"},
		Statuses:  []string{"open"},
		Parent:    epicID,
		CountOnly: true,
	})
	if err != nil {
		return 0
	}
	return res.Total
}
