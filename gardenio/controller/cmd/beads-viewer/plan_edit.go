package main

import (
	"net/http"
	"strings"

	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/plans"
)

// In-cockpit direct editing (Surface C, task 3e ka-0000007445; design
// ka-0000005773 §6.3). WRITE-THROUGH (D5, ka-0000003301): every verb here
// mutates the LIVE beads in place and persists immediately — there is no
// draft-store staging. The operator edits the plan directly instead of nudging
// the assistant to reword; an operator's direct edit is authoritative the
// moment it lands.
//
// Browser write-proxies (carve-out ka-0000004319; canonical banner in
// plans_lifecycle.go). RETAINED, not deleted — the browser cannot reach the
// daemon directly. These verbs own NO plan invariant: each is the same
// UpdateBead / CreateBead+AddDependency+AppendBeadArrayField / CAS-reorder
// composition `gb plan add-phase|reorder-phase` performs daemon-direct
// (cmd/gb/plan_write.go), and the daemon owns the atomic append + CAS-reorder
// primitives (kbeads http_beads.go). The step-membership + doc-in-bundle
// ownership checks here are URL-scoping guards (a browser POST can't retitle an
// arbitrary bead), not plan-domain invariants.
//
// Five verbs, all operator-gated at the route layer (server.go) and re-checking
// EmailFromContext defense-in-depth, all stamping the operator on the daemon ctx
// so the kbeads audit resolver does not fail closed (mirrors handlePlanAddStep,
// R2/ka-0000005772):
//
//   1. narrative summary edit — POST /plans/{id}/narrative          (epic.Description)
//   2. bundle-doc edit        — POST /plans/{id}/docs/{doc}          (doc fields.content)
//   3. step reword            — POST /plans/{id}/steps/{step}/title  (step.Title)
//   4. step reorder ▲▼        — POST /plans/{id}/steps/{step}/reorder (parent step_order)
//   5. create-phase           — POST /plans/{id}/phases             (mint epic + grow phase_order)
//
// Each re-renders the fragment the acting operator sees (the #plan-narrative or
// #plan-steps HTMX swap target); other connected viewers pick up the change via
// the existing /plans/{id}/events SSE stream — the writes fire the bead/dependency
// lifecycle events writePlanEvent already routes to plan-narrative-updated /
// plan-updated (plans.go), so 3e needs no new SSE frame.

// maxPlanNarrativeLen caps the plan-narrative summary the inline editor accepts
// (plans.MaxNarrativeLen). An oversized payload re-renders with an inline error.
const maxPlanNarrativeLen = plans.MaxNarrativeLen

// planEditActor is the shared preamble for every 3e write verb: it validates
// the plan id, enforces the operator gate defense-in-depth and parses the form.
// On any failure it writes the HTTP error and returns ok=false. The plan epic
// itself is loaded (and the operator stamped on the daemon ctx) by the plans
// service.
func planEditActor(w http.ResponseWriter, r *http.Request) (string, plans.Actor, bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "Missing plan id", http.StatusBadRequest)
		return "", plans.Actor{}, false
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return "", plans.Actor{}, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return "", plans.Actor{}, false
	}
	return id, planActor(email), true
}

// handlePlanNarrativeEdit persists a direct edit to the plan's narrative SUMMARY
// (the plan epic's description) and re-renders the #plan-narrative fragment
// (Surface C, ka-0000007445). Write-through: the description PATCH lands
// immediately and the SSE stream re-renders the fragment for every other viewer
// (planNarrativeEventRelevant lets "description" through). Empty input is
// allowed — an operator may deliberately clear the summary (the bundle docs
// carry the long-form narrative), which the fragment renders as its empty state.
//
// Browser write-proxy (carve-out ka-0000004319; see file header). Owns no plan
// invariant — a plain Description UpdateBead on the plan epic.
func (s *Server) handlePlanNarrativeEdit(w http.ResponseWriter, r *http.Request) {
	id, actor, ok := planEditActor(w, r)
	if !ok {
		return
	}
	s.respondPlanNarrativeWrite(w, r, id, s.plansService(r).EditNarrative(r.Context(), id, r.PostForm.Get("narrative"), actor))
}

// handlePlanDocEdit persists a direct edit to one bundle-doc body (the long-form
// narrative, ka-0000007445) via a write-through fields.content PATCH on the
// type=doc bead, then re-renders the #plan-narrative fragment. The doc must be a
// type=doc child of the plan's narrative bundle — verified so a stray id cannot
// rewrite an unrelated bead through this route.
//
// Browser write-proxy (carve-out ka-0000004319; see file header). Owns no plan
// invariant — a content-field UpdateBeadFieldsTyped on the doc bead; the
// doc-in-bundle check is URL-scoping only.
func (s *Server) handlePlanDocEdit(w http.ResponseWriter, r *http.Request) {
	id, actor, ok := planEditActor(w, r)
	if !ok {
		return
	}
	s.respondPlanNarrativeWrite(w, r, id, s.plansService(r).EditDoc(r.Context(), id, r.PathValue("doc"), r.PostForm.Get("content"), actor))
}

// handlePlanStepTitleEdit rewords one step directly (ka-0000007445): a
// write-through Title PATCH on the step bead, then a #plan-steps re-render. The
// step must be a rendered step of THIS plan (a direct issue child of the plan
// epic or of one of its phase sub-epics) so the route cannot retitle an
// arbitrary bead. Empty/whitespace is rejected inline (a step must keep a title).
//
// Browser write-proxy (carve-out ka-0000004319; see file header). Owns no plan
// invariant — a plain Title UpdateBead; the membership check is URL-scoping only.
func (s *Server) handlePlanStepTitleEdit(w http.ResponseWriter, r *http.Request) {
	id, actor, ok := planEditActor(w, r)
	if !ok {
		return
	}
	s.respondPlanStepsWrite(w, r, id, s.plansService(r).RetitleStep(r.Context(), id, r.PathValue("step"), r.PostForm.Get("title"), actor))
}

// handlePlanStepReorder moves one step up or down within its group's display
// order (ka-0000007445) by rewriting fields.step_order on the step's DIRECT
// parent epic — the plan epic for a flat step, the phase sub-epic for a phased
// step. This is the step-level analogue of handlePlanPhaseReorder. The order is
// SEEDED from the group's current rendered order when the field is absent (so
// the first ▲▼ click on a never-reordered plan produces a full, correct list,
// not a one-element list that would strand the other steps in creation order),
// then the target is swapped one slot in dir. Re-renders #plan-steps; a
// boundary no-op and a landed write both re-render the current state.
//
// Browser write-proxy (carve-out ka-0000004319; see file header). Owns no plan
// invariant — the CAS reorder primitive is the daemon's (ReorderBeadArrayField).
func (s *Server) handlePlanStepReorder(w http.ResponseWriter, r *http.Request) {
	id, actor, ok := planEditActor(w, r)
	if !ok {
		return
	}
	dir := strings.TrimSpace(r.URL.Query().Get("dir"))
	s.respondPlanStepsWrite(w, r, id, s.plansService(r).ReorderStep(r.Context(), id, r.PathValue("step"), dir, actor))
}

// handlePlanCreatePhase is the phase_order GROW verb (ka-0000007445): today
// handlePlanPhaseReorder only PERMUTES the set (plan_phases.go). This mints a new
// type=epic child of the plan, links it parent-child, and APPENDS its id to the
// plan's fields.phase_order — write-through, so the new (empty) phase renders on
// the timeline at once and the operator moves steps into it via the reorder/
// add-step verbs. Project label is stamped born-on from the plan (mirrors
// handlePlanAddStep) so the phase epic is project-scoped from creation.
//
// Browser write-proxy (carve-out ka-0000004319; see file header). Owns no plan
// invariant — the SAME CreateBead+AddDependency+AppendBeadArrayField compose
// `gb plan add-phase` performs daemon-direct (cmd/gb/plan_write.go addPlanPhase);
// the atomic append-if-absent is the daemon's primitive.
func (s *Server) handlePlanCreatePhase(w http.ResponseWriter, r *http.Request) {
	id, actor, ok := planEditActor(w, r)
	if !ok {
		return
	}
	_, err := s.plansService(r).CreatePhase(r.Context(), id, r.PostForm.Get("title"), actor)
	s.respondPlanStepsWrite(w, r, id, err)
}

// --- render helpers ----------------------------------------------------------

// respondPlanStepsWrite answers a step-list write: a plain error when there is
// nothing to re-render (bad address, missing plan or step), else the
// #plan-steps fragment with the failure inline (or clean on success).
func (s *Server) respondPlanStepsWrite(w http.ResponseWriter, r *http.Request, planID string, err error) {
	if respondPlanAction(w, r, planID, err) {
		return
	}
	if err == nil {
		s.renderPlanSteps(w, r, planID, "", http.StatusOK)
		return
	}
	if planWriteHTTPError(w, err) {
		return
	}
	s.renderPlanSteps(w, r, planID, plans.Message(err), planWriteStatus(err))
}

// respondPlanNarrativeWrite is respondPlanStepsWrite for the #plan-narrative
// fragment.
func (s *Server) respondPlanNarrativeWrite(w http.ResponseWriter, r *http.Request, planID string, err error) {
	if respondPlanAction(w, r, planID, err) {
		return
	}
	if err == nil {
		s.renderPlanNarrative(w, r, planID, "", http.StatusOK)
		return
	}
	if planWriteHTTPError(w, err) {
		return
	}
	s.renderPlanNarrative(w, r, planID, plans.Message(err), planWriteStatus(err))
}

// renderPlanNarrative re-renders the editable #plan-narrative fragment (the HTMX
// swap target after a narrative-summary or bundle-doc edit). Re-reads live so the
// prose is authoritative. errMsg surfaces inline above the narrative; status !=
// 200 sets the HTTP code so htmx still swaps the fragment (mirrors
// renderPlanSteps). The IsOperator flag drives the edit affordances.
func (s *Server) renderPlanNarrative(w http.ResponseWriter, r *http.Request, epicID, errMsg string, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	data, ok := s.planNarrativeData(r.Context(), epicID, viewerCanOperate(r), "")
	if !ok {
		// The plan vanished between the write and the re-render — degrade to a
		// minimal bag so the fragment renders its empty state rather than 500ing.
		data = map[string]any{"PlanID": epicID, "IsOperator": viewerCanOperate(r)}
	}
	data["Error"] = errMsg
	s.renderPartial(w, r, "templates/partials/plan_narrative.html", "plan_narrative", data)
}

// --- pure helpers -----------------------------------------------------------

// maxCASReorderAttempts bounds the reorder verbs' compare-and-swap retry loop
// (ka-0000000988); see plans.MaxCASReorderAttempts.
const maxCASReorderAttempts = plans.MaxCASReorderAttempts
