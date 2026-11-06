package main

// plans_lifecycle.go — the from-scratch plan authoring lifecycle + the
// approve→build gate (task ka-0000005350, Phase 3 of the from-scratch plan
// authoring plan ka-0000002918; design of record ka-0000005773 §6.4; risk R6).
//
// D5 (ka-0000003301) resolved the draft-mode fork to WRITE-THROUGH: the cockpit
// persists every bead immediately as the assistant/operator author — there is
// NO read-only draft store. The ONLY gate in the authoring flow is on the
// gardener BUILD. This file implements that single gate as an explicit plan-epic
// lifecycle stamped on fields.plan_state:
//
//	draft   — the assistant is still researching / clarifying; steps are being
//	          drafted. The gardener build is BLOCKED.
//	ready   — the plan is finalized and awaiting the operator's deliberate
//	          "Approve & build". The gardener build is still BLOCKED.
//	building— the operator approved; the bound gardener loop is enabled and
//	          dispatching crew. The gardener toggle is UNBLOCKED (pause/resume).
//
// The transition ready→building is the single deliberate "Approve & build"
// action (ExitPlanMode-analogous, §6.4). It flips the state AND enables the
// bound gardener in one operator gesture. draft↔ready is a lighter "Mark ready"
// / "Keep editing" toggle so an operator can finalize (or reopen) without
// building.
//
// BACKWARD COMPATIBILITY is load-bearing: an epic with NO plan_state field
// never opted into the lifecycle (every plan minted before this field, plus any
// epic an operator marked a plan by hand). Such a plan is treated as UNGATED —
// the gardener toggle behaves exactly as it did before this task. Only a plan
// that has explicitly entered the lifecycle (plan_state ∈ {draft, ready,
// building}) is gated. The from-scratch create verb (task 3a, ka-0000006081)
// stamps plan_state=draft at mint time, so every browser-originated plan is
// gated; hand-marked and legacy plans opt in via "Mark ready" (which is what
// first writes the field).
//
// ── CARVE-OUT (operator decision ka-0000004319, chosen=carveout; P4 epic ──────
// ka-0000006936). This is the canonical banner for the browser plan-write
// PROXIES; the sibling write-proxy files (plans_create.go, plan_edit.go,
// plan_phases.go, and the add-step handler in plans.go) point here.
//
// These handlers are RETAINED as thin browser write-proxies, NOT deleted. The
// browser cannot reach the kbeads daemon directly — unifiedHost is off in every
// committed env (helm _helpers.tpl) and the browser holds no daemon operator
// bearer — so a browser plan write MUST land through a same-origin viewer POST,
// which the viewer forwards to the daemon on its own s.daemon bearer +
// X-Operated-By. Deleting the proxies would break the browser plan cockpit with
// no browser-reachable replacement.
//
// What the carve-out GUARANTEES is that these proxies own NO plan-domain
// INVARIANT of their own. The daemon (P1) is the sole authority for every plan
// invariant, enforced unconditionally at its own choke points regardless of
// caller:
//   - plan_state transition legality — validatePlanStateTransition at the
//     updateBead choke point (kbeads plan_state_validation.go); an illegal jump
//     like draft→complete is rejected even if a proxy tried to write it.
//   - the draft/ready gardener-enable gate — validateGardenerEnable{OnCreate,
//     Transition} (kbeads gardener_enable_gate.go).
//   - born-on-label create + in-transaction read-back (kbeads createBead).
//   - the atomic phase_order/step_order append + CAS reorder primitives
//     (kbeads http_beads.go).
//   - the first-class beads.plan.step.added signal (kbeads recordAndPublish).
//
// So each handler below only COMPOSES daemon primitives + RENDERS the HTMX
// fragment — the byte-for-byte SAME composition `gb plan <verb>` performs
// daemon-direct (cmd/gb/plan_write.go). The compositions themselves live in
// internal/plans (task 5.8), shared with the Workbench; the handlers parse the
// request and render the result (plans_service.go). The proxies' UX pre-checks (e.g. the
// ready-before-build guard) are convenience only; the daemon rejects an illegal
// write regardless, so removing them would change the error message, never the
// invariant. The P4 exit therefore reads "no viewer-OWNED plan-write logic
// remains" (no invariant), not "no write PATH remains".
// ─────────────────────────────────────────────────────────────────────────────

import (
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/plans"
)

// planStateFieldKey is the epic field holding the authoring lifecycle state
// (plans.StateField).
const planStateFieldKey = plans.StateField

// planState is the authoring lifecycle a plan epic moves through
// (plans.State). The zero value (planStateUnset) means the epic never entered
// the lifecycle and is treated as ungated for backward compatibility (see file
// header).
type planState = plans.State

const (
	// planStateUnset is a plan that never opted into the lifecycle — legacy
	// plans and hand-marked epics. Treated as ungated.
	planStateUnset = plans.StateUnset
	// planStateDraft — assistant still researching/clarifying; build blocked.
	planStateDraft = plans.StateDraft
	// planStateReady — finalized, awaiting "Approve & build"; build blocked.
	planStateReady = plans.StateReady
	// planStateBuilding — approved; the gardener loop is enabled; toggle unblocked.
	planStateBuilding = plans.StateBuilding
	// planStateComplete — a TERMINAL state: the plan is done. Stamped by
	// "Mark complete" (handlePlanComplete / gb plan complete), which also closes
	// the plan epic and disables the bound gardener so the loop stops firing.
	// Reopen (handlePlanReopen) is the inverse: status=open + plan_state=building.
	planStateComplete = plans.StateComplete
	// planStateAbandoned — the DISTINCT terminal state for a plan dropped WITHOUT
	// finishing (the "cheap exit", task ka-0000001377). Stamped by "Abandon"
	// (handlePlanAbandon / gb plan abandon), which — like complete — closes the
	// plan epic and disables the bound gardener, but records the plan as GIVEN UP
	// ON rather than DONE so /plans can render the two terminal states distinctly.
	// A plan can be abandoned from any non-terminal working state
	// (draft/ready/building); reopen restores status=open + plan_state=building.
	planStateAbandoned = plans.StateAbandoned
)

// planStateOf reads the plan_state off an epic bead, normalizing an unknown or
// missing value to planStateUnset (ungated). A nil bead is unset.
func planStateOf(bead *beadsapi.BeadDetail) planState {
	return plans.StateOf(bead)
}

// planStateCanComplete reports whether plan_state=complete is a LEGAL next
// state from st — the gate the cockpit + peek-drawer "Mark complete" levers
// render behind. It mirrors the daemon's validatePlanStateTransition
// legal-into-complete set (kbeads plan_state_validation.go) exactly:
//
//	unset    → complete  LEGAL (the backward-compat carve-out: any edge FROM
//	                     unset is admitted; a legacy/ungated plan may retire.
//	                     This is the DOMINANT open-plan shape — ~39 of 56 live
//	                     open plans are unset — and the case bug ka-0000007777
//	                     is about: the cockpit + drawer used to hide the lever
//	                     here even though the daemon accepts the write.)
//	building → complete  LEGAL (the cockpit's own approve→build→complete edge)
//	draft    → complete  REJECTED by the daemon (400)
//	ready    → complete  REJECTED by the daemon (400)
//	complete → complete  no-op (already terminal; Reopen renders instead)
//
// Mirroring rather than importing is deliberate: the viewer is a separate Go
// module from kbeads, the daemon enforces the invariant unconditionally at its
// own choke point regardless of caller (carve-out ka-0000004319), and this
// predicate is a UX pre-check only — if the two ever drift, the daemon still
// rejects the illegal write, so the failure mode is a button that errors inline,
// never a corrupted lifecycle.
func planStateCanComplete(st planState) bool {
	return st == planStateBuilding || st == planStateUnset
}

// planLifecycleChipState reconciles the authoring-lifecycle state DISPLAYED as
// the /plans lifecycle chip (index card + cockpit header) against the plan
// epic's OWN terminal status (ka-0000005352 iteration 27). It is a display-only
// projection — the raw plan_state (planStateOf) still drives the board lane
// (planBoardLaneKey) and the Reopen/complete levers, which must keep seeing the
// true stored value.
//
// THE DEFECT it closes (the follow-up iteration 25 flagged): a plan can be
// CLOSED or DEFERRED while its plan_state field still reads a NON-terminal
// lifecycle value — draft/ready/building — because a hand-close, a drain
// cascade, or a "Mark complete" that closed the epic without stamping
// plan_state=complete never rewinds the field. The chip then renders that stale
// value beside the closed status chip, most egregiously "building" whose tooltip
// claims "the bound gardener is dispatching crew step by step" on a FINISHED
// plan. Live proof (2026-08-07 daemon pour, 143 plan epics): 15 closed plans
// carried a non-terminal chip — 6 building, 6 draft, 3 ready. This is the same
// card-vs-truth family iter-19/23 (stall), iter-24 (freshness) and iter-25
// (terminal CTA) closed — a terminal plan must not advertise an in-flight
// lifecycle.
//
// The reconcile: on a terminal plan (planStatusTerminal), a non-terminal
// display state (draft/ready/building) is SUPPRESSED to planStateUnset — the
// chip vanishes, exactly as it does for a legacy/ungated plan, so a closed plan
// reads by its status chip alone. A genuine terminal plan_state (complete) is
// PRESERVED: "complete" is the truthful terminal signal and the cockpit still
// needs it to render the Reopen affordance context. A non-terminal plan is
// returned unchanged, so every open/in_progress plan's chip reads exactly as
// before. Pure function of the two inputs already in hand at both render sites —
// zero extra daemon call.
func planLifecycleChipState(st planState, status string) planState {
	if !planStatusTerminal(status) {
		return st
	}
	switch st {
	case planStateDraft, planStateReady, planStateBuilding:
		return planStateUnset
	default: // planStateUnset (no chip) or planStateComplete/planStateAbandoned (truthful terminal)
		return st
	}
}

// gardenerEnableGatedByState reports whether the plan's lifecycle state forbids
// ENABLING the bound gardener. This is the R6 gate: a still-authoring plan
// (draft or ready) must not have a running gardener, so a half-baked plan can
// never auto-build. It gates only the ENABLE direction — pausing (disable) is
// always allowed, and a plan that never opted into the lifecycle (unset) is
// ungated so legacy/hand-marked plans behave exactly as before.
//
// building is the only lifecycle state in which the gardener may be enabled;
// "Approve & build" is the sole transition into it, so enabling never happens
// behind the operator's back.
func gardenerEnableGatedByState(st planState) bool {
	switch st {
	case planStateDraft, planStateReady:
		return true
	default: // planStateUnset (ungated) or planStateBuilding (approved)
		return false
	}
}

// handlePlanState toggles the plan between draft and ready — the lighter "Mark
// ready" / "Keep editing" lever that finalizes (or reopens for editing) a plan
// WITHOUT building it. Operator-gated at the route layer. Posts to
// /plans/{id}/state with an optional `state` form value; when absent it toggles
// draft↔ready. Re-renders the gardener bar in place (the bar carries the
// lifecycle chip + the approve/edit levers), matching the toggle/wake response
// shape so HTMX swaps #plan-gardener.
//
// A building plan cannot be flipped here — that would strand an enabled gardener
// on a non-building plan. To reopen a building plan for editing the operator
// pauses the gardener first (which is always allowed); reverting building→ready
// is intentionally not a one-click action.
//
// Browser write-proxy (carve-out ka-0000004319; see file header). Owns no plan
// invariant: the draft↔ready transition legality is enforced by the daemon's
// validatePlanStateTransition regardless of this handler's pre-checks. Mirrors
// `gb plan` daemon-direct; the building-plan guard is UX only.
func (s *Server) handlePlanState(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	// An explicit form value wins (so the two buttons post their intent);
	// absent it, the service toggles draft↔ready.
	requested := planState(strings.TrimSpace(r.FormValue("state")))
	s.respondPlanBarWrite(w, r, id, s.plansService(r).SetState(r.Context(), id, requested, planActor(email)))
}

// handlePlanBuild is the single deliberate "Approve & build" action (§6.4,
// ExitPlanMode-analogous). It flips a ready plan to building AND enables the
// bound gardener in one operator gesture, then re-renders the gardener bar.
// Operator-gated at the route layer.
//
// Preconditions, each surfaced inline in the bar rather than as a bare HTTP
// error so a stale click degrades gracefully:
//   - the plan must be `ready` (a draft still has open questions; a building
//     plan is already approved).
//   - a gardener must be bound (there is nothing to enable otherwise).
//
// Ordering: the state write happens FIRST, then the gardener enable. If the
// enable fails the plan is left in building with a paused gardener — a state the
// operator can recover from with one Resume click — which is safer than the
// inverse (an enabled gardener on a still-`ready` plan would defeat the gate).
//
// Browser write-proxy (carve-out ka-0000004319; see file header). Owns no plan
// invariant: the ready→building transition legality AND the R6 gardener-enable
// gate are both enforced by the daemon (validatePlanStateTransition +
// validateGardenerEnableTransition) no matter who writes. The ready-before-build
// pre-check here is UX only.
func (s *Server) handlePlanBuild(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	err := s.plansService(r).Build(r.Context(), id, planActor(email))
	if respondPlanAction(w, r, id, err) {
		return
	}
	if err == nil {
		s.respondPlanBuild(w, r, id, "", http.StatusOK)
		return
	}
	if planWriteHTTPError(w, err) {
		return
	}
	// Failures render inline in the bar; only a missing gardener carries a 409.
	status := http.StatusOK
	if plans.KindOf(err) == plans.KindConflict {
		status = http.StatusConflict
	}
	s.respondPlanBuild(w, r, id, plans.Message(err), status)
}

// respondPlanBuild writes the "Approve & build" response on WHICHEVER surface
// invoked handlePlanBuild — the cockpit or the /plans index card (task
// ka-0000006826). It mirrors respondPlanLifecycle's peek=1 seam: the cockpit
// (#plan-agent) is the default, but a fleet-view click carries row=1 so the
// WHOLE /plans row re-renders in place, its lifecycle chip + gardener state
// updating live (the index has no #plan-agent card to swap).
//
// The row re-render re-reads the epic (renderPlanRow → planIndexEnrich), so the
// post-mutation plan_state=building + running gardener render straight from the
// freshly-written epic. errMsg (empty on a clean approve) threads into the
// row's inline per-gardener note, the SAME channel the row toggle uses.
func (s *Server) respondPlanBuild(w http.ResponseWriter, r *http.Request, planID, errMsg string, status int) {
	// from_card=1 marks an Approve & build tapped on a deck card (Step 4's
	// /deck, task ka-0000000701). Like the row=1 seam below, it re-renders on the
	// surface that issued the POST — the lean deck_card fragment — so the build
	// action refreshes the same card shape in place instead of swapping the
	// cockpit gardener bar into the deck slot (decision ka-0000001748's bug). A
	// failed build (e.g. "no gardener bound") returns as an error ON the card,
	// which the deck's JS deliberately does not auto-advance past.
	if strings.TrimSpace(r.FormValue("from_card")) != "" {
		s.respondDeckWrite(w, r, planID, errMsg)
		return
	}
	if strings.TrimSpace(r.FormValue("row")) != "" {
		if epic, err := s.daemon.GetBead(r.Context(), planID); err == nil && epic != nil {
			s.renderPlanRow(w, r, epic, errMsg)
			return
		}
	}
	s.renderPlanGardenerErr(w, r, planID, errMsg, status)
}

// handlePlanComplete is the terminal "Mark complete" action (task ka-0000000616,
// operator decision ka-0000004595 chose PROPOSE — the operator confirms; the
// gardener only proposes closure). The mutation itself lives in completePlanCore
// (shared with the bulk verb); this handler resolves the plan, short-circuits the
// already-terminal case, and picks the response surface. Operator-gated at the
// route layer.
//
// Idempotent: a plan already complete (or already closed) re-renders without a
// second close. A soft guard on open steps lives in the UI (the button carries
// an "N steps still open — complete anyway?" confirm); the handler itself does
// not block on open children, so an operator can always retire a plan.
//
// Browser write-proxy (carve-out ka-0000004319; see file header) — see
// completePlanCore for the composition + ordering rationale and the
// no-viewer-owned-invariant argument.
func (s *Server) handlePlanComplete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	s.respondPlanLifecycleWrite(w, r, id, s.plansService(r).Complete(r.Context(), id, planActor(email)))
}

// completePlanCore is the shared "retire this plan" mutation: disable the bound
// gardener, stamp plan_state=complete, close the epic (plans.CompleteEpic). The
// single-plan lever and the BULK Mark-complete verb (plans_bulk.go) are the
// same composition rather than two implementations that could drift on
// ordering or attribution (task ka-0000005361).
//
// This is the byte-for-byte SAME composition `gb plan complete` performs
// daemon-direct (cmd/gb/plan_write.go completePlan) — a browser write-proxy that
// owns NO plan invariant (carve-out ka-0000004319; see the file header). The
// daemon enforces the building→complete transition legality regardless of caller;
// callers that pre-check (planCompleteReachable) do so for the ERROR MESSAGE, not
// for the invariant.
func (s *Server) completePlanCore(r *http.Request, epic *beadsapi.BeadDetail, email string) error {
	return s.plansService(r).CompleteEpic(r.Context(), epic, planActor(email))
}

// handlePlanAbandon is the terminal "Abandon" action — the DISTINCT "cheap exit"
// (task ka-0000001377, epic ka-0000007470) for a plan dropped WITHOUT finishing.
// It is structurally identical to handlePlanComplete (disable the bound gardener,
// stamp the terminal plan_state, close the epic) but stamps plan_state=abandoned
// so the board and cockpit render the plan as GIVEN UP ON rather than DONE.
// Operator-gated at the route layer.
//
// Ordering + failure semantics are IDENTICAL to handlePlanComplete: disable the
// gardener FIRST (so the loop is quiet even if a later write fails), then stamp
// abandoned, then close the epic. Idempotent: an already-abandoned (and closed)
// plan re-renders without a second close.
//
// Unlike complete, abandon deliberately does NOT gate on open child steps — the
// whole point of the cheap exit is to give up on the leftover work, so leftover
// open steps are expected. The daemon accepts draft/ready/building→abandoned, so
// a plan can be abandoned from any non-terminal working state.
//
// Browser write-proxy (carve-out ka-0000004319; see file header). Owns no plan
// invariant: this is the SAME disable-gardener → stamp abandoned → close-epic
// composition `gb plan abandon` performs daemon-direct (cmd/gb/plan_write.go
// abandonPlan). The daemon enforces the →abandoned transition legality; the
// ordering here is a safe-failure convenience, not an invariant the viewer owns.
func (s *Server) handlePlanAbandon(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	s.respondPlanLifecycleWrite(w, r, id, s.plansService(r).Abandon(r.Context(), id, planActor(email)))
}

// abandonPlanCore is the shared "give up on this plan" mutation: disable the
// bound gardener, stamp plan_state=abandoned, close the epic
// (plans.AbandonEpic). The single-plan lever and the BULK Abandon verb
// (plans_bulk.go) are the SAME composition (task ka-0000004351), exactly the
// arrangement completePlanCore gives the complete verb, and the same one `gb
// plan abandon` performs daemon-direct.
func (s *Server) abandonPlanCore(r *http.Request, epic *beadsapi.BeadDetail, email string) error {
	return s.plansService(r).AbandonEpic(r.Context(), epic, planActor(email))
}

// handlePlanReopen is the inverse of handlePlanComplete / handlePlanAbandon: it
// REOPENS a terminal plan (status=open, plan_state=building) so work can resume.
// Operator-gated. The bound gardener is left as-is (paused) — reopening restores
// the buildable state without silently restarting the loop; the operator resumes
// the gardener deliberately (Resume), consistent with the R6 "enabling never
// happens behind the operator's back" gate. Re-renders the card.
//
// building (not ready) is the reopen target because a terminal plan was, by
// definition, past the approve→build gate; dropping it back to ready would
// force a redundant re-approval. Both complete and abandoned reopen to building
// — the single back-edge. An operator who wants to re-author instead can "Keep
// editing" (building→… ) from there.
//
// Browser write-proxy (carve-out ka-0000004319; see file header). Owns no plan
// invariant: the same reopen-epic → restore-building composition `gb plan reopen`
// performs daemon-direct (cmd/gb/plan_write.go reopenPlan). The daemon enforces
// the complete/abandoned→building transition legality; leaving the gardener paused
// is the shared R6 policy, not a viewer-owned invariant.
func (s *Server) handlePlanReopen(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	s.respondPlanLifecycleWrite(w, r, id, s.plansService(r).Reopen(r.Context(), id, planActor(email)))
}

// respondPlanBarWrite answers a gardener-bar write (Mark ready / Keep
// editing): a plain error for a missing plan or a failed plan read, else the
// re-rendered #plan-gardener bar with any failure inline. The bar always
// answers 200 so htmx swaps it.
func (s *Server) respondPlanBarWrite(w http.ResponseWriter, r *http.Request, planID string, err error) {
	if respondPlanAction(w, r, planID, err) {
		return
	}
	if err != nil && planWriteHTTPError(w, err) {
		return
	}
	msg := ""
	if err != nil {
		msg = plans.Message(err)
	}
	s.renderPlanGardenerErr(w, r, planID, msg, http.StatusOK)
}

// respondPlanLifecycleWrite answers complete / abandon / reopen: on success the
// surface that issued the POST re-renders (respondPlanLifecycle); a failure
// renders inline in the gardener bar.
func (s *Server) respondPlanLifecycleWrite(w http.ResponseWriter, r *http.Request, planID string, err error) {
	if respondPlanAction(w, r, planID, err) {
		return
	}
	if err == nil {
		s.respondPlanLifecycle(w, r, planID)
		return
	}
	s.respondPlanBarWrite(w, r, planID, err)
}

// respondPlanLifecycle writes the browser success response for a lifecycle
// mutation (complete / reopen). Three shapes, one seam:
//   - an /plans INDEX CARD click (form value row=1) → the re-rendered WHOLE plan
//     card, because the index has no #plan-agent card to swap. This is the seam
//     the close-out lever on the index uses (task ka-0000004467); it mirrors
//     respondPlanBuild's identical row=1 arm, which the index's Approve & build
//     already relies on. Without it a one-tap Mark complete from the index would
//     swap a cockpit gardener-bar fragment into the card slot — decision
//     ka-0000001748's bug.
//   - a peek-drawer click (form value peek=1) → the re-rendered #plan-peek-body
//     fragment (the drawer has no #plan-agent card to swap);
//   - a cockpit click → the re-rendered #plan-agent card.
//
// All three re-read the plan live, so the post-mutation state/status render
// straight from the freshly-written epic — the handler already persisted both.
// The row re-render goes through renderPlanRow → planIndexEnrich, the same pass
// the page render uses, so the retired card (now closed, lifecycle chip
// "complete", close-out badge gone) cannot disagree with a fresh page load.
func (s *Server) respondPlanLifecycle(w http.ResponseWriter, r *http.Request, planID string) {
	if strings.TrimSpace(r.FormValue("row")) != "" {
		if epic, err := s.daemon.GetBead(r.Context(), planID); err == nil && epic != nil {
			s.renderPlanRow(w, r, epic, "")
			return
		}
	}
	if strings.TrimSpace(r.FormValue("peek")) != "" {
		// Re-read the epic so the drawer reflects the post-mutation status +
		// plan_state (this handler already wrote both). A miss falls back to the
		// cockpit card render rather than erroring the drawer.
		if bead, err := s.daemon.GetBead(r.Context(), planID); err == nil && bead != nil {
			s.renderPlanPeek(w, r, bead)
			return
		}
	}
	s.renderPlanGardenerErr(w, r, planID, "", http.StatusOK)
}
