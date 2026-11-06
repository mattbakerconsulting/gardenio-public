package main

// plans_mutations.go — deck-write responses and the operator-only plan mutations (add/close/reopen/dispatch step, mark, narrative).
// Split out of plans.go (gd-Wl6LuCnvlH); see plans.go for the Plans surface overview.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/plans"
)

// respondDeckWrite writes the result of a deck-card write (answer or Approve &
// build) on the right surface for how the request arrived (task ka-0000000701).
//
//   - htmx (JS on): re-render the lean deck_card fragment for an in-place swap
//     into `closest .deck-card`. The answered card becomes its now-resolved next
//     question, a header-only "all clear" face, or (on a failed write) an error
//     card the deck's JS deliberately does not auto-advance past.
//   - full POST (JS off): the browser navigated to this route, so a bare fragment
//     would strand the operator on a layout-less page (the JS-off half of the bug
//     decision ka-0000001748 flagged). Redirect (303) back to the deck instead —
//     on success the answered card has dropped out of the freshly-ranked queue,
//     so the operator lands on the next card with their place kept via the deck
//     URL in the Referer.
func (s *Server) respondDeckWrite(w http.ResponseWriter, r *http.Request, epicID, errMsg string) {
	if r.Header.Get("HX-Request") == "true" {
		s.renderDeckCard(w, r, epicID, errMsg)
		return
	}
	http.Redirect(w, r, s.deckReturnURL(r), http.StatusSeeOther)
}

// deckReturnURL is where a JS-off deck write redirects back to. It honours the
// deck URL in the Referer (path + query only, never the host — so a skip/at
// cursor survives the answer without opening a cross-origin redirect), falling
// back to the bare deck when there is no usable Referer.
func (s *Server) deckReturnURL(r *http.Request) string {
	deckPath := s.basePath + "/deck"
	ref := strings.TrimSpace(r.Header.Get("Referer"))
	if ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Path == deckPath {
			if u.RawQuery != "" {
				return deckPath + "?" + u.RawQuery
			}
			return deckPath
		}
	}
	return deckPath
}

// refererPlansProject extracts the active ?project= filter from the Referer of a
// /plans single-row or bulk-OOB re-render POST (ka-0000005352 iter-60). A row
// re-render (a gardener toggle, a Dispatch, a bulk disable) is issued by htmx from
// the /plans page the operator is looking at, so its Referer carries that page's
// URL — including the ?project=X filter when the board is scoped to one project.
// Threading that value into the re-rendered card's Root lets the project-chip
// suppression (plan_card.html) key off the SAME filter the page render reads off
// the page dot ($root.ProjectFilter), so a swapped-in row stays byte-consistent
// with its unswapped neighbours instead of re-growing a project chip they dropped.
//
// It matches ONLY a Referer whose path is exactly the /plans index (never the host,
// never another page), so a re-render triggered from anywhere else falls back to the
// empty (unfiltered) filter — the chip then renders exactly as on an unfiltered
// board, the safe default. Returns "" when there is no usable Referer.
func (s *Server) refererPlansProject(r *http.Request) string {
	plansPath := s.basePath + "/plans"
	ref := strings.TrimSpace(r.Header.Get("Referer"))
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.Path != plansPath {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("project"))
}

// refererPlansGroup extracts the active ?group= key from the Referer of a /plans
// single-row or bulk-OOB re-render POST (ka-0000005352 iter-62) — the group twin
// of refererPlansProject. A ?group=project board sections its cards into one
// heading-per-project, so the per-card project chip is redundant ink there (the
// project is already named on the section heading) exactly as on a ?project=X
// filtered board (iter-60). Threading the key into the re-rendered card's Root
// lets the project-chip suppression (plan_card.html) key off the SAME group the
// page render reads off the page dot ($root.GroupKey), so a swapped-in card stays
// byte-consistent with its unswapped neighbours instead of re-growing a project
// chip they dropped.
//
// It matches ONLY a Referer whose path is exactly the /plans index, and passes the
// raw value through normalizePlanGroup so an unrecognized ?group= reads as the
// no-group default — the chip then renders exactly as on an ungrouped board, the
// safe default. Returns "" (planGroupNone) when there is no usable Referer.
func (s *Server) refererPlansGroup(r *http.Request) string {
	plansPath := s.basePath + "/plans"
	ref := strings.TrimSpace(r.Header.Get("Referer"))
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.Path != plansPath {
		return ""
	}
	return normalizePlanGroup(u.Query().Get("group"))
}

// refererPlansOperatedBy recovers the RESOLVED operator scope of the /plans page
// a single-row or bulk-OOB re-render POST was issued from (ka-0000005352 iter-61),
// so a swapped-in card's creator-chip suppression keys off the SAME scope the page
// render read off the page dot (.EffectiveOperatedBy). It is the operated_by twin
// of refererPlansProject, but with one extra subtlety the project filter does not
// have: the /plans board is DEFAULT-scoped to the viewer's own operator id when the
// URL carries NO operated_by key (S1 "filter-by-me by default"), so recovering the
// scope means REDOING handlePlans' resolveOperatedByParam + resolveDefaultOperatedBy
// precedence against the Referer's query — not just reading a raw param.
//
// The precedence mirrors handlePlans exactly:
//   - No Referer, or a Referer whose path is not the /plans index → "" (the safe
//     unscoped default; the chip then renders as on an "all" board).
//   - Referer has an operated_by key → resolve it (me-expansion; null/(none) → "").
//     A URL-explicit value (incl. the empty "?operated_by=" = "show all") wins,
//     exactly as resolveDefaultOperatedBy's Has()-guarded skip does on the page.
//   - Referer has NO operated_by key → apply the cold-load default: the viewer's
//     canonical oidc id (canonicalOIDC(email)), or "" when anonymous.
//
// Because it reuses the SAME resolveOperatedByParam + resolveDefaultOperatedBy the
// page render uses, the re-rendered row's scope can never drift from its unswapped
// neighbours' — the load-bearing byte-consistency guarantee iter-60 established for
// the project chip.
func (s *Server) refererPlansOperatedBy(r *http.Request) string {
	plansPath := s.basePath + "/plans"
	email := oidcauth.EmailFromContext(r.Context())
	ref := strings.TrimSpace(r.Header.Get("Referer"))
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.Path != plansPath {
		return ""
	}
	q := u.Query()
	operatedBy, _ := resolveOperatedByParam(q.Get("operated_by"), email)
	// Redo resolveDefaultOperatedBy's precedence against the Referer's query
	// (NOT the POST request's own URL, which has no operated_by): the URL-key
	// presence is the load-bearing trigger — a present key (any value, incl. the
	// empty "show all") wins and skips the default; an absent key applies the
	// viewer default.
	if q.Has("operated_by") {
		return operatedBy
	}
	if e := strings.TrimSpace(email); e != "" {
		return canonicalOIDC(e)
	}
	return ""
}

// --- operator-only mutations -------------------------------------------------

// handlePlanAddStep creates a new TODO step (an issue child) under the plan
// epic and re-renders the step list. Mirrors create_gardener.go's
// CreateBead+AddDependency idiom and description_edit.go's HTMX re-render.
// Operator-gated at the route layer; re-checks EmailFromContext defense-in-depth.
//
// Browser write-proxy (carve-out ka-0000004319; canonical banner in
// plans_lifecycle.go). RETAINED, not deleted — the browser cannot reach the
// daemon directly. Owns no plan invariant: the CreateBead+AddDependency compose
// is the SAME step-mint `gb plan add-step` performs daemon-direct
// (cmd/gb/plan_write.go addPlanStep), the daemon emits the first-class
// beads.plan.step.added signal off the edge, and the single PlanStepReactor (not
// this handler) owns the reactive nudge. The phase-membership check is a
// URL-scoping guard, not a plan-domain invariant.
func (s *Server) handlePlanAddStep(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "Missing plan id", http.StatusBadRequest)
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
	// The service stamps the operator on the daemon ctx so the CreateBead +
	// AddDependency carry X-Operated-By (R2, ka-0000005772). The daemon stamps
	// the same identity as operated_by on the beads.dependency.added edge, which
	// the PlanStepReactor reads to attribute its nudge/wake writes.
	stepID, err := s.plansService(r).AddStep(r.Context(), id, r.PostForm.Get("title"), r.PostForm.Get("phase"), planActor(email))
	if respondPlanAction(w, r, firstNonBlank(stepID, id), err) {
		return
	}
	if err != nil {
		if planWriteHTTPError(w, err) {
			return
		}
		s.renderPlanSteps(w, r, id, plans.Message(err), planWriteStatus(err))
		return
	}
	s.renderPlanSteps(w, r, id, "", http.StatusOK)
}

// planAddStepNudgeWindow is the per-plan debounce window for the reactive
// add-step -> assistant nudge/spawn. A burst of "add step" clicks on one plan
// within this window drives exactly one collaboration nudge; later adds still
// land as steps and the assistant, once live, sees them on its next turn.
const planAddStepNudgeWindow = 5 * time.Second

// planLastNudgeField is the plan-epic field holding the RFC3339 timestamp of the
// last reactive add-step nudge — the DURABLE backing for the per-plan debounce
// (ka-0000002855). It is declared on type:epic in the kbeads builtin schema
// (epics don't allowAdditionalFields, so an undeclared key 400s).
const planLastNudgeField = "last_nudge_at"

// shouldNudgePlanAssistant reports whether the reactive add-step handler should
// drive a collaboration nudge for epicID right now, and durably records the
// decision. It returns true at most once per planAddStepNudgeWindow per plan so
// a rapid bulk-add coalesces into a single nudge/spawn.
//
// DURABLE, not in-memory (ka-0000002855). The window used to live in a volatile
// per-server map (planNudgeLast) that was lost on restart and shared by no other
// writer, so a viewer restart — or a concurrent add from the CLI, the browser,
// or the controller reactor, each its own process with its own map — could
// double-fire the nudge. The last-nudge timestamp now lives on the plan epic
// itself (fields.last_nudge_at), so the window survives restarts and coordinates
// every writer through the daemon.
//
// Concurrency: the per-server mutex serialises the read-modify-write, and the
// GetBead is issued INSIDE the lock so a second concurrent add on this server
// observes the first add's just-persisted stamp (read-after-write) rather than a
// stale snapshot the caller fetched before the stamp landed. The daemon's
// additive field-merge means the stamp never clobbers a concurrent writer's
// other fields. add-step nudges are low-frequency, so holding the lock across the
// daemon round-trip is not a hot path.
//
// Best-effort: a daemon read/write error never suppresses the nudge — the
// reactive collaboration is itself best-effort (a missed nudge is seen by the
// agent on its next turn), so a transient failure fires the nudge and degrades to
// the old volatile behaviour for that one add rather than dropping it.
func (s *Server) shouldNudgePlanAssistant(ctx context.Context, epicID string) bool {
	now := s.now()
	s.planNudgeMu.Lock()
	defer s.planNudgeMu.Unlock()

	// Re-read the epic inside the lock so a concurrent add on this server sees the
	// freshest persisted last_nudge_at (read-after-write), not a snapshot the
	// caller fetched before an earlier add stamped it.
	epic, err := s.daemon.GetBead(ctx, epicID)
	if err != nil || epic == nil {
		s.logger.Debug("plan add step: reading durable debounce state — firing anyway",
			"plan", epicID, "error", err)
		return true
	}
	if raw := epic.Fields[planLastNudgeField]; raw != "" {
		if last, perr := time.Parse(time.RFC3339, raw); perr == nil && now.Sub(last) < planAddStepNudgeWindow {
			return false
		}
	}

	// Outside the window (or never nudged) — durably stamp now so a restart or a
	// concurrent writer respects the same window.
	if err := s.daemon.UpdateBeadFieldsTyped(ctx, epicID, map[string]any{
		planLastNudgeField: now.UTC().Format(time.RFC3339),
	}); err != nil {
		s.logger.Warn("plan add step: persisting durable debounce stamp",
			"plan", epicID, "error", err)
	}
	return true
}

// handlePlanCloseStep closes one TODO step and re-renders the step list.
// Mirrors handleCloseBeadAction. Operator-gated at the route layer.
//
// Browser write-proxy (carve-out ka-0000004319; canonical banner in
// plans_lifecycle.go). Owns no plan invariant — a plain CloseBeadAs on the step
// bead; the daemon's auto-close cascade (which skips plan epics, flagging them
// autoclose-ready instead) is the invariant, and it lives in the daemon.
func (s *Server) handlePlanCloseStep(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	stepID := strings.TrimSpace(r.PathValue("step"))
	if id == "" || stepID == "" {
		http.Error(w, "Missing plan or step id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	err := s.plansService(r).CloseStep(r.Context(), id, stepID, planActor(email))
	if respondPlanAction(w, r, stepID, err) {
		return
	}
	if err != nil {
		s.renderPlanSteps(w, r, id, plans.Message(err), planWriteStatus(err))
		return
	}
	s.renderPlanSteps(w, r, id, "", http.StatusOK)
}

// handlePlanReopenStep flips a closed step back to open and re-renders the list.
//
// Browser write-proxy (carve-out ka-0000004319; canonical banner in
// plans_lifecycle.go). Owns no plan invariant — a plain status→open UpdateBead
// on the step bead.
func (s *Server) handlePlanReopenStep(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	stepID := strings.TrimSpace(r.PathValue("step"))
	if id == "" || stepID == "" {
		http.Error(w, "Missing plan or step id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	err := s.plansService(r).ReopenStep(r.Context(), id, stepID, planActor(email))
	if respondPlanAction(w, r, stepID, err) {
		return
	}
	if err != nil {
		s.renderPlanSteps(w, r, id, plans.Message(err), planWriteStatus(err))
		return
	}
	s.renderPlanSteps(w, r, id, "", http.StatusOK)
}

// handlePlanDispatchStep hands ONE open step to the plan's gardener (Matt's ask,
// thread ka-0000002484): it delivers a targeted instruction to the plan's bound
// gardener to dispatch a crew agent onto that specific step — and spawns the
// gardener first if none is bound yet. It reuses promptPlanAssistant, the same
// nudge-live / wake-parked / spawn-if-none primitive the plan-agent message box
// and the reactive add-step path (the PlanStepReactor) already use, so a dispatch
// never mints a second, unwatched agent: the plan gardener == the plan agent, so
// the message reaches (or spawns) the ONE lineage that coordinates the plan.
//
// Operator-gated at the route layer; re-checks EmailFromContext defense-in-depth.
// Re-renders #plan-steps with a "handed to the gardener" confirmation notice (or
// an inline error on failure) so the operator sees the outcome in place.
func (s *Server) handlePlanDispatchStep(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	stepID := strings.TrimSpace(r.PathValue("step"))
	if id == "" || stepID == "" {
		http.Error(w, "Missing plan or step id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	// The service stamps the operator so promptPlanAssistant's spawn writes carry
	// X-Operated-By (R2, ka-0000005772).
	d, err := s.plansService(r).DispatchStep(r.Context(), id, stepID, planActor(email))
	if wantsActionJSON(r) {
		if err != nil {
			respondPlanAction(w, r, stepID, err)
		} else {
			// Agent names the gardener the step went to, for the panel's notice.
			writeActionJSON(w, actionResponse{OK: true, Bead: stepID, Agent: d.Gardener})
		}
		return
	}
	if err != nil {
		if planWriteHTTPError(w, err) {
			return
		}
		s.renderPlanStepsNotice(w, r, id, plans.Message(err), "", planWriteStatus(err))
		return
	}
	s.renderPlanStepsNotice(w, r, id, "", dispatchNoticeLabel(d.Outcome, d.Gardener), http.StatusOK)
}

// handlePlanRowDispatch is the /plans INDEX-row Dispatch button (Matt's ask,
// thread ka-0000001290): "instead of sending a line and there's a wake button
// there should just be a dispatch button in the index card for the agent for the
// next task to take on." It resolves the plan's CURRENT/NEXT open step — the same
// step the card already surfaces as its "now / next" chip — and hands THAT step
// to the plan's gardener via the SAME promptPlanAssistant primitive the detail
// timeline's per-step Dispatch uses (handlePlanDispatchStep), then re-renders the
// row in place with a confirmation notice.
//
// This replaces the old free-text "spawn/wake with a message" box on the index
// card: one deliberate click hands the next task to the gardener (spawning one if
// none is bound) instead of asking the operator to compose a nudge. Operator-
// gated at the route layer; re-checks EmailFromContext defense-in-depth.
func (s *Server) handlePlanRowDispatch(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "Missing plan id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	// Stamp the operator on the daemon ctx so promptPlanAssistant's spawn writes
	// carry X-Operated-By (mirrors handlePlanDispatchStep / handlePlanAddStep),
	// without which the kbeads audit resolver fail-closes (R2, ka-0000005772).
	ctx := beadsapi.WithOperator(r.Context(), canonicalOIDC(email))

	epic, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Plan not found", http.StatusNotFound)
			return
		}
		s.logger.Error("plan row dispatch: get plan", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if epic == nil || epic.Type != "epic" {
		http.Error(w, "Plan not found", http.StatusNotFound)
		return
	}

	// Defense-in-depth: a TERMINAL plan (closed/deferred) has no live work to
	// dispatch (ka-0000000857, iteration 25). The index no longer renders the
	// Dispatch button on a terminal plan (planIndexEnrich nils CurrentOrNextStep
	// for planStatusTerminal), so a live click can't reach here — but a stale
	// page loaded before the plan closed could still POST. Refuse it with an
	// honest note instead of cold-spawning a gardener onto a finished plan.
	if planStatusTerminal(epic.Status) {
		s.renderPlanRowWith(w, r, epic, func(row *planRowView) {
			row.GardenerDispatchNotice = "This plan is " + strings.ToLower(strings.TrimSpace(epic.Status)) + " — nothing to dispatch."
			row.GardenerDispatchError = true
		})
		return
	}

	// Resolve the plan's current/next open step off the SAME batched walk the
	// index page uses, so the button dispatches exactly the step the card's
	// "now / next" chip shows. No open step → nothing to dispatch: re-render the
	// row with an honest note rather than cold-spawning the gardener for no work.
	next := s.planRowNextStep(ctx, epic)
	if next == nil || strings.TrimSpace(next.ID) == "" {
		s.renderPlanRowWith(w, r, epic, func(row *planRowView) {
			row.GardenerDispatchNotice = "No open step to dispatch — every step is done or blocked."
			row.GardenerDispatchError = true
		})
		return
	}

	// Defense-in-depth: a BLOCKED next step cannot start (ka-0000005352 iteration
	// 44). pickCurrentOrNextStep returns the first non-closed step, which includes
	// a status=blocked one; the index card now suppresses the Dispatch button on a
	// non-Dispatchable step (plan_card.html gates on .CurrentOrNextStep.Dispatchable),
	// so a live click can't reach here — but a stale page loaded before the step
	// was blocked could still POST. Refuse it with an honest note instead of
	// seeding the gardener "work step X now — it's ready to start" onto a step that
	// is waiting on its own blocker (the terminal guard's blocked-step twin). The
	// gate reads the SAME Dispatchable() the button does, so the two can't drift.
	if !next.Dispatchable() {
		s.renderPlanRowWith(w, r, epic, func(row *planRowView) {
			row.GardenerDispatchNotice = fmt.Sprintf("Step #%d is blocked — it can't start until its blocker clears.", next.Number)
			row.GardenerDispatchError = true
		})
		return
	}

	// Targeted instruction, identical in shape to handlePlanDispatchStep so the
	// gardener's playbook treats an index dispatch and a timeline dispatch the same.
	seed := "Please dispatch a crew agent to work step " + next.ID
	if t := strings.TrimSpace(next.Title); t != "" {
		seed += " (\"" + t + "\")"
	}
	seed += " now — it's ready to start."

	_, outcome, name, perr := s.promptPlanAssistant(ctx, epic, email, seed)
	if perr != nil {
		s.logger.Error("plan row dispatch: prompt gardener", "plan", id, "step", next.ID, "actor", email, "error", perr)
		s.renderPlanRowWith(w, r, epic, func(row *planRowView) {
			row.GardenerDispatchNotice = "Failed to hand the step to the plan gardener — try again."
			row.GardenerDispatchError = true
		})
		return
	}
	s.logger.Info("plan_row_dispatch", "event", "plan_row_dispatch", "plan", id, "step", next.ID,
		"gardener", name, "outcome", string(outcome), "operator", email)
	s.renderPlanRowWith(w, r, epic, func(row *planRowView) {
		row.GardenerDispatchNotice = rowDispatchNoticeLabel(outcome, name, next.Number)
	})
}

// planRowNextStep resolves ONE plan's current/next open step via the shared
// index step-data walk (planIndexStepData over a single-element slice), so the
// index-card Dispatch button targets exactly the step the row's "now / next"
// chip renders. Returns nil when the plan has no open step. Best-effort: a
// resolution miss yields nil (the handler then shows the "nothing to dispatch"
// note) rather than erroring.
func (s *Server) planRowNextStep(ctx context.Context, epic *beadsapi.BeadDetail) *planCardStep {
	if epic == nil {
		return nil
	}
	data := s.planIndexStepData(ctx, []*beadsapi.BeadDetail{epic})
	if info := data[epic.ID]; info != nil {
		return info.NextStep
	}
	return nil
}

// rowDispatchNoticeLabel is the index-card sibling of dispatchNoticeLabel: it
// names the STEP NUMBER the button just handed off (the index card shows #N, not
// the bead id) so the operator sees which task went to the gardener and what
// happened to the hand-off.
func rowDispatchNoticeLabel(o planDeliveryOutcome, gardenerName string, stepNum int) string {
	who := "the gardener"
	if strings.TrimSpace(gardenerName) != "" {
		who = gardenerName
	}
	step := "the next step"
	if stepNum > 0 {
		step = fmt.Sprintf("step #%d", stepNum)
	}
	switch o {
	case planDeliverySent:
		return fmt.Sprintf("Handed %s to %s — it's live and will dispatch a crew agent.", step, who)
	case planDeliveryQueuedWaking:
		return fmt.Sprintf("Handed %s to %s — waking it now; it dispatches crew when it's back.", step, who)
	case planDeliverySpawning:
		return fmt.Sprintf("Spawning the plan gardener — it will dispatch a crew agent onto %s.", step)
	default:
		return fmt.Sprintf("Handed %s to the plan gardener.", step)
	}
}

// dispatchNoticeLabel renders the operator-facing confirmation for a just-fired
// step dispatch, keyed on promptPlanAssistant's delivery outcome so the wording
// is honest about what happened to the hand-off: nudged a live gardener, queued
// on a waking one, or cold-spawned the plan gardener on first contact.
func dispatchNoticeLabel(o planDeliveryOutcome, gardenerName string) string {
	who := "the plan gardener"
	if strings.TrimSpace(gardenerName) != "" {
		who = gardenerName
	}
	switch o {
	case planDeliverySent:
		return "Handed to " + who + " — it's live and will dispatch a crew agent onto this step."
	case planDeliveryQueuedWaking:
		return "Handed to " + who + " — waking it now; it dispatches a crew agent when it's back."
	case planDeliverySpawning:
		return "Spawning the plan gardener — it will dispatch a crew agent onto this step."
	default:
		return "Handed this step to the plan gardener."
	}
}

// handlePlanMark toggles the `plan` label on an epic — the one-click "mark as
// plan" / "unmark" affordance. Redirects back to the plan page so the nav +
// index pick it up. Operator-gated at the route layer.
//
// Browser write-proxy (carve-out ka-0000004319; canonical banner in
// plans_lifecycle.go). Owns no plan invariant — a plain Add/RemoveLabel on the
// epic; the gardener bind on promotion is reactive collaboration (best-effort),
// not a plan-state write.
func (s *Server) handlePlanMark(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "Missing plan id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	err := s.plansService(r).Mark(r.Context(), id, planActor(email))
	if respondPlanAction(w, r, id, err) {
		return
	}
	if err != nil {
		http.Error(w, plans.Message(err), planWriteStatus(err))
		return
	}
	http.Redirect(w, r, s.basePath+"/plans/"+id, http.StatusSeeOther)
}

// handlePlanNarrativeView returns the read-only narrative fragment (summary +
// bundle docs) — a manual-refresh HTMX target and the reconnect fallback for
// the SSE stream. The manual textarea editor is gone (S3, ka-0000002473):
// nudging the plan assistant is the in-page edit path (it rewrites the bundle
// doc beads via kd, and the SSE stream re-renders this fragment live); kd
// remains the out-of-page escape hatch.
func (s *Server) handlePlanNarrativeView(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "Missing plan id", http.StatusBadRequest)
		return
	}
	if fragment := s.renderPlanNarrativeFragment(r.Context(), r, id, viewerCanOperate(r), ""); fragment != "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, fragment)
		return
	}
	http.Error(w, "Plan not found", http.StatusNotFound)
}

// planStepsData builds the data bag the plan_steps partial expects, reading the
// plan's steps live and deriving the rollup counts. Shared by the detail page,
// the HTMX re-render after add/close/reopen, and the live-progress SSE stream so
// all three render byte-identical step timelines. isOperator is passed in (not
// derived) because the SSE path has no per-event *http.Request to read roles off.
//
// Progress counts come from planProgress — a count over exactly the rendered
// steps (flat + phased), the same rendered set the /plans index row computes —
// so every render path of the fragment shows the same % as the index row and
// the same denominator as the visible ✓ rows (ka-0000004410, S4 ka-0000001060).
// When the plan epic declares phase sub-epics (fields.phase_order), the flat
// Steps slice carries only the UNPHASED direct issue children and Phases holds
// the ordered phase groups (each with its own steps + per-phase progress). A
// plan with no phase_order renders exactly as before: all direct issue children
// in Steps, Phases nil (ka-0000005926).
func (s *Server) planStepsData(ctx context.Context, epicID string, isOperator bool, errMsg string) map[string]any {
	return s.planStepsDataNotice(ctx, epicID, isOperator, errMsg, "")
}

// planStepsDataNotice is planStepsData plus a transient success Notice — the
// positive-path counterpart to Error, rendered as a status line above the
// timeline (e.g. the "handed to the gardener" confirmation after a dispatch).
// Both are naturally transient: the next SSE re-render (which calls
// planStepsData with neither set) clears them.
func (s *Server) planStepsDataNotice(ctx context.Context, epicID string, isOperator bool, errMsg, notice string) map[string]any {
	steps, phases := s.planTimeline(ctx, epicID)
	total, closed, pct := planProgress(steps, phases)
	data := map[string]any{
		"PlanID":      epicID,
		"Steps":       steps,
		"Phases":      phases,
		"HasPhases":   len(phases) > 0,
		"StepsTotal":  total,
		"StepsClosed": closed,
		"StepsOpen":   total - closed,
		"PctDone":     pct,
		"IsOperator":  isOperator,
		"Error":       errMsg,
		"Notice":      notice,
		// Flat steps titled "Phase N" that were probably meant to be phase
		// sub-epics (ka-0000002630) — nil when none, so the hint self-suppresses.
		"PhaseHint": misfiledPhaseSteps(steps),
	}
	// The "⚗ Pour a formula" disclosure lazy-loads its own project-scoped picker
	// via GET /plans/{id}/pours/new (plan_pour.go), so the #plan-steps fragment
	// re-render injects no formula data — keeping the formula-list query off the
	// hot add/reorder/SSE re-render path (ka-0000001848).
	return data
}

// renderPlanSteps re-renders the editable step-list fragment (the HTMX swap
// target after add/close/reopen). Re-reads live so the list is authoritative.
func (s *Server) renderPlanSteps(w http.ResponseWriter, r *http.Request, epicID, errMsg string, status int) {
	s.renderPlanStepsNotice(w, r, epicID, errMsg, "", status)
}

// renderPlanStepsNotice is renderPlanSteps with an additional success Notice —
// the dispatch handler's confirmation path. Both re-read the steps live so the
// swapped-in fragment is authoritative.
func (s *Server) renderPlanStepsNotice(w http.ResponseWriter, r *http.Request, epicID, errMsg, notice string, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	data := s.planStepsDataNotice(r.Context(), epicID, viewerCanOperate(r), errMsg, notice)
	steps, _ := data["Steps"].([]planStepView)
	phases, _ := data["Phases"].([]planPhaseView)
	r = s.prewarmAgentLinksForNames(r, planTimelineAssignees(steps, phases))
	s.renderPartial(w, r, "templates/partials/plan_steps.html", "plan_steps", data)
}

// planNarrativeData builds the data bag the plan_narrative partial expects:
// the epic-description summary (markdown-rendered), the bundle view (nil when
// the plan has no bundle), and the optional edit attribution. Shared by the
// detail page, the standalone fragment route, and the SSE re-render so all
// three paths render the narrative identically. updatedBy is the acting agent
// from the triggering BeadUpdated event — non-empty only on SSE re-renders,
// where the attribution chip shows who just edited the plan.
func (s *Server) planNarrativeData(ctx context.Context, epicID string, isOperator bool, updatedBy string) (map[string]any, bool) {
	bead, err := s.daemon.GetBead(ctx, epicID)
	if err != nil || bead == nil {
		return nil, false
	}
	revDeps, derr := s.daemon.GetReverseDependencies(ctx, epicID)
	if derr != nil {
		s.logger.Debug("plan narrative: reverse deps", "id", epicID, "error", derr)
	}
	proseView, proseBeadID := s.planProse(ctx, epicID, revDeps)
	return map[string]any{
		"PlanID":      epicID,
		"ProseHTML":   s.renderMarkdownDescriptionForBead(ctx, bead, bead.Description, nil),
		"Description": bead.Description,
		"Prose":       proseView,
		"ProseBeadID": proseBeadID,
		"IsOperator":  isOperator,
		"UpdatedBy":   updatedBy,
	}, true
}

// renderPlanNarrativeFragment renders plan_narrative to a string for the SSE
// payload and the standalone fragment route. Mirrors renderPlanStepsFragment
// (buffer-render, "" on any error so the caller skips the frame).
func (s *Server) renderPlanNarrativeFragment(ctx context.Context, r *http.Request, epicID string, isOperator bool, updatedBy string) string {
	tmpl, ok := s.partials["templates/partials/plan_narrative.html"]
	if !ok {
		s.logger.Error("plan narrative: partial not found")
		return ""
	}
	data, ok := s.planNarrativeData(ctx, epicID, isOperator, updatedBy)
	if !ok {
		return ""
	}
	data["BasePath"] = s.basePath
	bound, err := s.bindAgentLink(tmpl, r)
	if err != nil {
		s.logger.Error("plan narrative: bind template", "error", err)
		return ""
	}
	var buf bytes.Buffer
	if err := bound.ExecuteTemplate(&buf, "plan_narrative", data); err != nil {
		s.logger.Error("plan narrative: render", "epic", epicID, "error", err)
		return ""
	}
	return buf.String()
}
