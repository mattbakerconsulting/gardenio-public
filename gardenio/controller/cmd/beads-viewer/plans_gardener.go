package main

// plans_gardener.go — the gardener (loop-state) half of the /plans/{id} plan-agent
// card: the gardener resolver, its view projection, and the pause/resume + Wake
// mutation handlers. Originally Step 2 of the per-plan-gardener epic
// (ka-0000002022, task ka-0000002775; decision ka-0000002222 option
// surface-bind) as a standalone control bar; step 6 of the unify plan
// (ka-0000000116) merged that bar into the single plan_agent.html card, whose
// render seams live in plans_agent_panel.go.
//
// This is SURFACING + WIRING, not net-new backend. Step 1 (ka-0000002366)
// established the binding: a plan's gardener is the ONE type=trigger /
// trigger:schedule bead pinned to the plan epic via a `gardener-epic:<plan-id>`
// label (plus a `plan-gardener` marker label), whose spawn role is plan-gardener,
// so its iterations ARE the plan's interactive agent. Every mutation here
// delegates to the SAME shared cores the /gardener/{id} control bar uses
// (setGardenerEnabledCore, spawnGardenerRunNowCore in gardener_ops.go), so the
// spawn/enable path is never forked. The Wake button is exactly the run-now
// verb — and because Step 1 aligned run-now to honour the trigger's spawn role
// and write the `assigned` edge, a Wake spawns an iteration that resolves as
// the plan's bound agent (planAssistant), just like a scheduled fire.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/scheduler"
)

// planGardenerMarkerLabel tags a trigger bead as a plan-bound gardener — the
// same shared marker `gb plan gardener` and the viewer's auto-bind stamp
// (internal/plangardener, the one gardener-spec builder). Presence is not
// required for the lookup (the gardener-epic label is authoritative), but it
// lets the resolver prefer a purpose-built plan gardener over a generic epic
// gardener that happens to share the epic.
const planGardenerMarkerLabel = plangardener.MarkerLabel

// gardenerEpicLabelFor is the label that pins a gardener trigger to the epic it
// drives — the shared plangardener helper (also behind cmd/gb's
// gardenerEpicLabel and the CutPrefix reads in gardeners_owned_epic.go). The
// plan's gardener carries this label with the plan epic id.
func gardenerEpicLabelFor(epicID string) string {
	return plangardener.GardenerEpicLabel(epicID)
}

// planGardenerState enumerates the four UI states the gardener bar renders under,
// mirroring the assistant lane's planAssistantState vocabulary so the two bars
// read consistently: "none" (no gardener bound), "running" (bound + enabled),
// "paused" (bound + disabled), "retired" (bound but closed/terminated).
type planGardenerState string

const (
	planGardenerStateNone    planGardenerState = "none"
	planGardenerStateRunning planGardenerState = "running"
	planGardenerStatePaused  planGardenerState = "paused"
	planGardenerStateRetired planGardenerState = "retired"
)

// planGardenerView is the gardener half of the merged plan-agent card's data
// bag — the loop's identity + state + the in-flight-run hint that gates the
// (implicit) cancel affordance. Deliberately smaller than gardenerControlsView: the plan
// bar exposes only the three levers Step 2 calls for (pause/resume + Wake),
// with a link out to the full /gardener/{id} page for retire/config/cancel.
type planGardenerView struct {
	// PlanID is the plan epic the bar lives on — the HTMX targets post to
	// /plans/{plan}/gardener/... so the bar re-renders in place.
	PlanID string
	// GardenerID is the bound gardener trigger bead id (empty when none). Used
	// for the "open the gardener page" deep link.
	GardenerID string
	// Title is the gardener trigger bead's title, shown on hover.
	Title string
	// State drives the chip + which levers render.
	State planGardenerState
	// FiringMode is the gardener's timed⇄event firing mode (Task 8, epic
	// ka-0000006020): "cron" (⏱ clock-fired), "event" (⚡ subtree-activity fired,
	// with a slow fallback-cron watchdog), or "" (no gardener bound / a
	// condition-gated gardener that is neither). Derived from the SAME
	// gardenerTriggerSubKind classifier the /gardeners list uses (Task 6), so the
	// /plans chip glyph and the /gardeners node color can never disagree. Threaded
	// through planAgentStateFrom → planAgentChip so the chip renders the glyph.
	FiringMode string
	// Cron / Timezone are the gardener trigger's raw schedule spec + tz, carried so
	// the cockpit chip-build seams (planGardenerBarData + planAgentPanelData) can
	// fold the forward-looking "· fires in 12m" next-fire facet onto an armed-idle
	// cron chip (ka-0000005352 iter-56) WITHOUT re-fetching the gardener bead — the
	// index enrich seam reads them straight off gardeners[b.ID].Fields, the cockpit
	// reads them off this view. Both feed the SAME chip.setNextFire, gated on
	// idle+cron, so index↔cockpit render the identical facet. Empty for a
	// gardener-less / condition-gated (no-cron) gardener, which setNextFire no-ops.
	Cron     string
	Timezone string
	// Cadence is the operator-editable firing cadence the plan cockpit surfaces
	// (task ka-0000008237): the flat cron in timed mode, or the fallback-watchdog
	// cron entry in event mode (empty ⇒ purely event-driven, no clock floor). It
	// is what the cadence editor form pre-fills and posts back to
	// /plans/{id}/gardener/cadence. Distinct from Cron above, which is ONLY the
	// flat cron field the chip's next-fire facet reads (empty in event mode).
	Cadence string
	// CadenceEditable is true when the operator can tune the cadence from the
	// cockpit: a bound, non-retired gardener with a resolvable firing mode (⏱/⚡).
	// A condition-gated gardener with neither mode (empty FiringMode) hides the
	// editor — its firing has no single cron knob to tune here.
	CadenceEditable bool
	// Paused mirrors State==paused, precomputed so the toggle template stays
	// declarative (matches gardenerPauseView.Paused).
	Paused bool
	// HasInflightRun is true when the gardener's last iteration is still live —
	// surfaced as a hint ("a run is in flight") so a Wake that would be
	// rejected by one_spawn_at_a_time is explained before the click.
	HasInflightRun bool
	// IsOperator gates the levers (matches the operatorOnly POST routes).
	IsOperator bool
	// Error, when non-empty, renders as an inline alert in the bar.
	Error string
	// Park is the gardener's LIVE park (gd-f2fc4jcRn3): set when a running
	// gardener provably fires nothing because its frontier waits on a blocker.
	// Filled only on the full cockpit render (planAgentPanelData), never on the
	// index or peek, since the probe walks the plan subtree.
	Park *planGardenerPark

	// --- Approve→build lifecycle (task ka-0000005350, D5, R6). The bar carries
	// the plan's authoring lifecycle chip + the approve/edit levers alongside
	// the gardener controls, because the lifecycle IS what gates the gardener
	// build. ---

	// PlanState is the plan epic's lifecycle (draft / ready / building), or ""
	// when the plan never opted into the lifecycle (legacy / hand-marked plans).
	// This is the RAW stored value — the approve/edit/reopen levers key off it, so
	// it stays unreconciled (a closed plan still needs to know its stored state to
	// offer Reopen).
	PlanState planState
	// LifecycleChip is PlanState reconciled against the epic's terminal status for
	// the DISPLAY chip only (ka-0000005352 iter-27, planLifecycleChipState): a
	// terminal (closed/deferred) plan suppresses a stale non-terminal chip
	// (draft/ready/building → "" so the header chip drops), while a genuine
	// `complete` stays. The cockpit header chip renders off THIS field so it agrees
	// with the /plans index card by construction (both reconcile the same way).
	LifecycleChip planState
	// LifecycleGated is true when the plan explicitly entered the lifecycle
	// (PlanState is one of draft/ready/building/complete) — the approve/edit/reopen
	// levers render only then, so ungated plans show the bar exactly as before.
	// Unreconciled (keyed off the raw PlanState) so a closed plan keeps its Reopen
	// lever; the CHIP visibility now gates on LifecycleChip, not this.
	LifecycleGated bool
	// ShowApproveBuild is true when the plan is `ready` AND a gardener is bound —
	// the single deliberate "Approve & build" button renders (§6.4).
	ShowApproveBuild bool
	// ShowMarkReady is true when the plan is `draft` — the "Mark ready" lever
	// that moves draft→ready renders.
	ShowMarkReady bool
	// ShowKeepEditing is true when the plan is `ready` — the "Keep editing"
	// lever that reverts ready→draft renders next to "Approve & build".
	ShowKeepEditing bool
	// Building mirrors PlanState==building, precomputed so the template's
	// "building" affordances stay declarative.
	Building bool

	// --- Terminal completion (task ka-0000000616, decision ka-0000004595:
	// PROPOSE — the operator confirms). The plan-gardener only proposes closure
	// in its playbook; these levers are how the operator acts on it. ---

	// ShowMarkComplete is true when plan_state=complete is a LEGAL next state —
	// `building` (the cockpit's own approve→build→complete edge) OR unset (a
	// legacy/ungated plan; the daemon's backward-compat carve-out admits any
	// transition FROM unset, so unset→complete is legal). The terminal "Mark
	// complete" button then renders (closes the plan epic + disables the
	// gardener). Mirrors the daemon's validatePlanStateTransition legal-into-
	// complete set (kbeads plan_state_validation.go) so the cockpit, the peek
	// drawer, and the index card all offer the lever on exactly the same plans
	// (bug ka-0000007777).
	ShowMarkComplete bool
	// ShowAbandon is true when the plan is in a non-terminal working state
	// (draft/ready/building) — the DISTINCT terminal "Abandon" cheap-exit button
	// renders (closes the plan epic + disables the gardener, stamps
	// plan_state=abandoned). It is the "give up on this" counterpart to Mark
	// complete (task ka-0000001377): unlike complete it is offered from any
	// working state, not just building, so a stalled draft can be retired directly.
	ShowAbandon bool
	// ShowReopen is true when the plan is terminal (plan_state=complete or
	// abandoned, OR the epic is closed) — the "Reopen" inverse renders.
	ShowReopen bool
	// OpenStepsRemaining is the count of still-open steps, threaded so the
	// "Mark complete" button's soft-guard confirm can say "N steps still open".
	// Zero (or unknown) renders an unconditional confirm.
	OpenStepsRemaining int
}

// planGardener locates the ONE gardener bound to a plan epic, if any. The binding
// is the `gardener-epic:<plan-id>` label Step 1 (gb plan gardener) stamps; the
// query is a single daemon-side label filter over trigger + schedule beads
// (the two gardener-bead types), so the lookup is one round-trip. Returns nil
// when the plan has no gardener. Best-effort: a daemon error degrades to "no
// gardener" (the bar renders the not-started state rather than erroring the
// whole cockpit).
//
// When more than one gardener carries the label (an operator ran gb plan gardener
// on an epic that already had a generic gardener, or a stale retired gardener
// lingers), we prefer: an OPEN gardener over a closed one, then a plan-gardener-
// marked gardener over an unmarked one, then the most recently updated. This
// mirrors planAssistant's "prefer the live, then the freshest" tie-break so the
// bar and the assistant lane never disagree about which gardener is THE plan's.
func (s *Server) planGardener(ctx context.Context, epicID string) *beadsapi.BeadDetail {
	if strings.TrimSpace(epicID) == "" {
		return nil
	}
	label := gardenerEpicLabelFor(epicID)
	var candidates []*beadsapi.BeadDetail
	seen := map[string]struct{}{}
	for _, typ := range []string{"trigger", "schedule"} {
		res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:  []string{typ},
			Labels: []string{label},
			Limit:  20,
		})
		if err != nil {
			s.logger.Debug("plan gardener: list", "epic", epicID, "type", typ, "error", err)
			continue
		}
		if res == nil {
			continue
		}
		for _, b := range res.Beads {
			if b == nil {
				continue
			}
			if _, dup := seen[b.ID]; dup {
				continue
			}
			// Defense-in-depth: the daemon filter already matched the label, but
			// re-check locally so a lax fake (or a future OR-widened query) can't
			// surface a non-gardener bead as the plan's gardener.
			if !isGardenerBead(b) || !hasLabel(b, label) {
				continue
			}
			seen[b.ID] = struct{}{}
			candidates = append(candidates, b)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0]
	for _, b := range candidates[1:] {
		if planGardenerBetter(b, best) {
			best = b
		}
	}
	return best
}

// planGardenerBetter reports whether candidate a is a better "the plan's
// gardener" pick than b: open beats closed, then plan-gardener-marked beats
// unmarked, then freshest-updated wins. Total order matching planAssistant's
// tie-break intent so the single-plan choice is deterministic.
func planGardenerBetter(a, b *beadsapi.BeadDetail) bool {
	aOpen := statusOpenSortKey(a.Status) == 0
	bOpen := statusOpenSortKey(b.Status) == 0
	if aOpen != bOpen {
		return aOpen
	}
	aMarked := hasLabel(a, planGardenerMarkerLabel)
	bMarked := hasLabel(b, planGardenerMarkerLabel)
	if aMarked != bMarked {
		return aMarked
	}
	return a.UpdatedAt.After(b.UpdatedAt)
}

// planGardenerViewFromBead projects the bound gardener bead (or nil) into the bar
// view. It reuses buildTriggerRow's enabled derivation (so the trigger:schedule
// default-on and legacy schedule default-off conventions both resolve in one
// place — matching gardenerPauseViewFromBead) and gardenerControlsViewFromBead's
// in-flight detection. `st` is the plan epic's authoring lifecycle state
// (task ka-0000005350) — it drives the lifecycle chip + the approve/edit levers
// the bar carries alongside the gardener controls.
func (s *Server) planGardenerViewFromBead(ctx context.Context, epicID string, gardener *beadsapi.BeadDetail, st planState, isOperator bool, errMsg string) planGardenerView {
	v := planGardenerView{
		PlanID:     epicID,
		State:      planGardenerStateNone,
		IsOperator: isOperator,
		Error:      errMsg,
	}
	// Lifecycle chip + approve/edit levers (§6.4). Only render for a plan that
	// explicitly entered the lifecycle; an unset state (legacy / hand-marked
	// plan) shows the bar exactly as before this task. A complete plan keeps the
	// chip too so the terminal state is visible + the Reopen lever renders.
	v.PlanState = st
	// LifecycleChip defaults to the raw state here (the pure projection has no
	// epic status in hand); planGardenerBarData reconciles it against the epic's
	// terminal status (ka-0000005352 iter-27) on the full-card render path — the
	// only path that renders the chip. The toggle callers that build this view
	// only to read v.Paused never render off LifecycleChip, so the raw default is
	// harmless for them.
	v.LifecycleChip = st
	v.LifecycleGated = st == planStateDraft || st == planStateReady || st == planStateBuilding || st == planStateComplete || st == planStateAbandoned
	v.Building = st == planStateBuilding
	v.ShowMarkReady = st == planStateDraft
	v.ShowKeepEditing = st == planStateReady
	// Terminal completion levers (task ka-0000000616, decision ka-0000004595:
	// PROPOSE — the operator confirms). "Mark complete" shows whenever
	// plan_state=complete is a LEGAL next state (building OR unset — see
	// planStateCanComplete / bug ka-0000007777); "Abandon" (the cheap exit, task
	// ka-0000001377) shows from ANY non-terminal working state; "Reopen" shows
	// once the plan_state is terminal (complete or abandoned). The epic-closed
	// nuance (a plan closed by hand with no terminal plan_state) is folded in by
	// the caller (planGardenerBarData) via markCompletionLevers, which has the
	// epic's status in hand.
	v.ShowMarkComplete = planStateCanComplete(st)
	v.ShowAbandon = st == planStateDraft || st == planStateReady || st == planStateBuilding
	v.ShowReopen = st == planStateComplete || st == planStateAbandoned
	// "Approve & build" needs a bound gardener to enable — computed below once we
	// know a gardener is bound.
	if gardener == nil {
		return v
	}
	v.GardenerID = gardener.ID
	v.Title = gardener.Title
	v.ShowApproveBuild = st == planStateReady
	// Firing-mode glyph (Task 8): reuse the shared classifier so ⏱/⚡ on the
	// /plans chip matches the /gardeners node color exactly. Empty for a legacy
	// schedule bead or a condition-gated gardener — the chip then shows no glyph.
	v.FiringMode = gardenerTriggerSubKind(gardener)
	// Carry the raw cron/tz so the chip's next-fire facet (iter-56) can compute
	// the countdown at the render seam without re-reading the bead. Only the cron
	// gardener's spec matters; an event/condition gardener has no cron and
	// setNextFire no-ops on it.
	v.Cron = gardener.Fields["cron"]
	v.Timezone = gardener.Fields["timezone"]
	// Cadence editor (task ka-0000008237): the effective cron the operator tunes.
	// In timed mode that is the flat cron; in event mode it is the fallback
	// watchdog entry (empty ⇒ purely event-driven). The editor renders for any
	// bound gardener whose firing mode resolves to timed (⏱) or event (⚡); a
	// condition-gated gardener (empty FiringMode) has no single cron knob here.
	v.Cadence = planGardenerCadence(gardener, v.FiringMode)

	controls := gardenerControlsViewFromBead(gardener)
	v.State = planGardenerLoopState(gardener)
	v.Paused = v.State == planGardenerStatePaused
	// The cadence editor renders for any live/paused gardener whose firing mode
	// resolves to timed (⏱) or event (⚡); a retired gardener has nothing to tune,
	// and a condition-gated gardener (empty FiringMode) has no single cron knob.
	v.CadenceEditable = v.State != planGardenerStateRetired && (v.FiringMode == "cron" || v.FiringMode == "event")

	// In-flight hint: re-read the last iteration's live status (the spec's
	// LastAgentID surface hint is only a pointer). Best-effort — a lookup miss
	// leaves HasInflightRun false, which is the safe default (the Wake button
	// still renders; the one_spawn guard in spawnGardenerRunNowCore is the
	// authoritative gate).
	if controls.HasInflightRun && controls.LastAgentID != "" {
		if prior, err := s.daemon.GetBead(ctx, controls.LastAgentID); err == nil && prior != nil {
			switch prior.Status {
			case "open", "in_progress":
				v.HasInflightRun = true
			}
		}
	}
	return v
}

// planGardenerLoopState projects a bound gardener trigger bead into the four-state
// loop vocabulary (none / running / paused / retired) the plan-agent chip reads.
// Pure (no I/O) so BOTH the /plans/{id} detail card (planGardenerViewFromBead)
// AND the /plans index enrich pass (planIndexEnrich) derive the loop state from
// the ONE place — the index chip and the detail chip can never disagree about a
// gardener's state. A nil bead (no gardener bound) is "none".
//
// The enabled derivation delegates to gardenerControlsViewFromBead, which honours
// the trigger:schedule default-on vs legacy schedule default-off conventions
// (buildTriggerRow semantics) — so a plan gardener (trigger:schedule) with no
// explicit `enabled` field reads "running", matching its actual firing gate.
// This is intentionally more precise than the stall signal's gardenerStateOf,
// which collapses a closed gardener to "none" (a retired watchdog watches
// nothing — exactly what that badge flags); here "retired" is a distinct state
// the chip renders, so a closed gardener is surfaced as retired, not none.
func planGardenerLoopState(gardener *beadsapi.BeadDetail) planGardenerState {
	if gardener == nil {
		return planGardenerStateNone
	}
	controls := gardenerControlsViewFromBead(gardener)
	switch {
	case controls.IsClosed:
		return planGardenerStateRetired
	case controls.Enabled:
		return planGardenerStateRunning
	default:
		return planGardenerStatePaused
	}
}

// planGardenerCadence returns the operator-editable firing cadence for a gardener
// given its resolved firing sub-kind (task ka-0000008237): the flat cron in
// timed ("cron") mode, or the fallback-watchdog cron entry in event mode (empty
// ⇒ purely event-driven, no clock floor). Mirrors the gb gardenerEffectiveCadence
// resolver so the cockpit editor and the CLI agree on what "the cadence" is.
// Returns "" for a gardener-less or condition-gated gardener.
func planGardenerCadence(gardener *beadsapi.BeadDetail, firingMode string) string {
	if gardener == nil {
		return ""
	}
	switch firingMode {
	case "cron":
		return gardener.Fields["cron"]
	case "event":
		entries, err := scheduler.ParseTriggers(gardener.Fields["triggers"])
		if err != nil {
			return ""
		}
		for _, e := range entries {
			if e.Kind == scheduler.TriggerKindCron {
				return e.Cron
			}
		}
		return ""
	default:
		return ""
	}
}

// planGardenerBarData builds the loop-state half of the merged plan-agent
// card's data bag ({Gardener: planGardenerView} + the unified chip). Kept as a
// plain map at the render seam so it lines up with the other plan partial
// helpers (planAssistantLaneData, planStepsData). It reads the plan epic's
// lifecycle state (fields.plan_state, task ka-0000005350) so the card renders
// the approve→build chip + levers; a GetBead miss degrades to an unset
// (ungated) state, which shows the card exactly as before.
func (s *Server) planGardenerBarData(ctx context.Context, epicID string, isOperator bool, errMsg string) map[string]any {
	gardener := s.planGardener(ctx, epicID)
	st := planStateUnset
	planClosed := false
	epicStatus := ""
	if epic, err := s.daemon.GetBead(ctx, epicID); err == nil && epic != nil {
		st = planStateOf(epic)
		epicStatus = epic.Status
		planClosed = statusOpenSortKey(epic.Status) != 0
	}
	view := s.planGardenerViewFromBead(ctx, epicID, gardener, st, isOperator, errMsg)
	// Reconcile the DISPLAY chip against the epic's terminal status (ka-0000005352
	// iter-27): a plan closed/deferred with a stale non-terminal plan_state
	// (draft/ready/building) drops its header chip here, exactly as the /plans
	// index card does off row.LifecycleChip — so the cockpit header and the index
	// agree by construction (both call planLifecycleChipState). This is the ONLY
	// render path for the cockpit chip; the pure projection defaulted LifecycleChip
	// to the raw st, so this is where the epic status (only in hand here) applies.
	view.LifecycleChip = planLifecycleChipState(st, epicStatus)
	// Fold the epic-closed nuance into the completion levers: a plan closed by
	// hand (status=closed) with no terminal plan_state should still offer Reopen,
	// and a closed plan must never offer Mark complete. Threads the open-step
	// count for the soft-guard confirm ("N steps still open — complete anyway?").
	s.markCompletionLevers(ctx, epicID, &view, planClosed)
	// The unified plan-agent chip (step 5, ka-0000001332) lives in the merged
	// card's header, so this half's data must carry both facets — the gardener
	// loop (view.State) AND the agent liveness — for every standalone re-render
	// path (the GET fragment, the POST toggle/wake responses, the lifecycle
	// handlers, and the SSE plan-agent-updated fragment). Resolving the
	// assistant here is what keeps the chip honest on a gardener-only re-render:
	// an operator who pauses the loop while an iteration is live sees
	// "paused · live", not a stale "paused". The lookup is the same planAssistant
	// resolution the lane uses; on the page-load path planAgentPanelData
	// recomputes the chip off the forceState-aware panel state, so the transient
	// "waking" signal still shows there.
	agentState := s.planChipAgentState(ctx, epicID)
	panelState := planAgentStateFrom(view, agentState)
	// Fold the armed-idle cron gardener's next-fire facet (iter-56) so the cockpit
	// header chip renders the SAME "· fires in 12m" the /plans index card does
	// (setNextFire no-ops off idle+cron, view.Cron/Timezone carry the spec).
	chip := planAgentChip(panelState).setNextFire(view.Cron, view.Timezone, s.now())
	return map[string]any{
		"Gardener":   view,
		"PanelState": panelState,
		"Chip":       chip,
		// The bound gardener bead itself, so planAgentPanelData can probe its
		// live park without a second lookup. No template reads it.
		"gardenerBead": gardener,
	}
}

// markCompletionLevers folds the epic-closed nuance into the terminal-completion
// levers (task ka-0000000616) and threads the open-step count for the "Mark
// complete" soft-guard confirm. planGardenerViewFromBead sets the levers off the
// plan_state alone; this refinement — which needs the epic's CLOSED status and a
// step-count walk the pure projection deliberately avoids — is applied only on
// the full-card render path (planGardenerBarData), where those are in hand:
//
//   - A plan closed BY HAND (status=closed) with no terminal plan_state should
//     still offer Reopen — so a legacy/hand-closed plan isn't stranded.
//   - A closed plan must NEVER offer Mark complete (it is already terminal).
//   - The open-step count drives the confirm text; a step walk failure leaves it
//     zero, which renders an unconditional confirm (the safe default).
func (s *Server) markCompletionLevers(ctx context.Context, epicID string, v *planGardenerView, planClosed bool) {
	if planClosed {
		v.ShowMarkComplete = false
		v.ShowAbandon = false
		v.ShowReopen = true
	}
	if v.ShowMarkComplete {
		steps, phases := s.planTimeline(ctx, epicID)
		total, closed, _ := planProgress(steps, phases)
		v.OpenStepsRemaining = total - closed
	}
}

// planChipAgentState resolves the plan's bound assistant into the coarse agent
// liveness the unified chip renders (live / parked / none). It mirrors the
// assistant lane's derived state (planAssistantLaneData) minus the transient
// "waking" — which is a forceState-only signal set immediately after a spawn/
// nudge on the lane's own POST path, never on a gardener-bar re-render — so a
// gardener-bar chip shows live/parked/none and the lane's own frame carries
// waking. Best-effort: no assistant bound (or a lookup miss) → none.
func (s *Server) planChipAgentState(ctx context.Context, epicID string) planAssistantState {
	assistant := s.planAssistant(ctx, epicID)
	if assistant == nil {
		return planAssistantStateNone
	}
	if s.assistantIsLive(assistant) {
		return planAssistantStateLive
	}
	// A woken assistant whose spawn was declined for a fully-exhausted pool is
	// queued, not parked — surface the wait distinctly (ka-0000003525).
	if assistantIsQueued(assistant) {
		return planAssistantStateQueued
	}
	return planAssistantStateParked
}

// loadPlanGardenerForMutation loads a plan epic and its bound gardener for a
// mutation handler, mapping the not-found / no-gardener cases to the right
// response. Returns (epic, gardener, true) when a live gardener is bound; on any
// miss it has already written the error/bar and callers should return. The epic
// is returned so callers can read its lifecycle state (fields.plan_state) — the
// approve→build gate (task ka-0000005350) reads it to block enabling a gardener
// on a still-authoring plan.
func (s *Server) loadPlanGardenerForMutation(w http.ResponseWriter, r *http.Request, planID string) (*beadsapi.BeadDetail, *beadsapi.BeadDetail, bool) {
	ctx := r.Context()
	epic, err := s.daemon.GetBead(ctx, planID)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Plan not found", http.StatusNotFound)
			return nil, nil, false
		}
		s.logger.Error("plan gardener: get plan", "id", planID, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return nil, nil, false
	}
	if epic == nil || epic.Type != "epic" {
		http.Error(w, "Plan not found", http.StatusNotFound)
		return nil, nil, false
	}
	gardener := s.planGardener(ctx, planID)
	if gardener == nil {
		// No gardener bound — re-render the card with an inline note instead of a
		// bare error, so a stale button (the plan lost its gardener between page
		// load and click) degrades gracefully in place.
		s.renderPlanGardenerErr(w, r, planID, "This plan has no bound gardener.", http.StatusConflict)
		return nil, nil, false
	}
	return epic, gardener, true
}

// handlePlanGardenerToggle flips the bound gardener's enabled field (pause ⇄
// resume) from the plan cockpit and re-renders the card. Operator-gated at the
// route layer. Delegates to setGardenerEnabledCore so the plan surface shares
// the exact enable/disable + loader-default semantics with the /gardener/{id}
// control bar.
func (s *Server) handlePlanGardenerToggle(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	epic, gardener, ok := s.loadPlanGardenerForMutation(w, r, id)
	if !ok {
		return
	}
	// Toggle to the inverse of the gardener's CURRENT effective enabled state,
	// derived the same way the bar renders it (buildTriggerRow), so a rapid
	// double-click can't desync the button from the write.
	st := planStateOf(epic)
	view := s.planGardenerViewFromBead(r.Context(), id, gardener, st, true, "")
	enableNext := view.Paused // paused → enable; running → disable
	// Approve→build gate (task ka-0000005350, R6): a still-authoring plan (draft
	// or ready) must not have a running gardener, so a half-baked plan never
	// auto-builds. Block ENABLING only — pausing is always allowed, and a plan
	// that never opted into the lifecycle (unset plan_state) is ungated so
	// legacy/hand-marked plans toggle exactly as before. "Approve & build"
	// (handlePlanBuild) is the sole path that enables a gardener on a gated plan.
	if enableNext && gardenerEnableGatedByState(st) {
		s.renderPlanGardenerErr(w, r, id, "Approve the plan for build before starting the gardener — use “Approve & build”.", http.StatusOK)
		return
	}
	if opErr := s.setGardenerEnabledCore(r, gardener, enableNext, email); opErr != nil {
		// A closed gardener (or write failure) — re-render the bar with the
		// message inline rather than a bare HTTP error, so the operator sees the
		// current state after a stale click.
		s.renderPlanGardenerErr(w, r, id, opErr.Message, http.StatusOK)
		return
	}
	s.renderPlanGardenerErr(w, r, id, "", http.StatusOK)
}

// handlePlanGardenerWake spawns one gardener iteration immediately (the Wake
// button) from the plan cockpit and re-renders the bar. Operator-gated.
// Delegates to spawnGardenerRunNowCore — the exact run-now verb — so the
// Wake-spawned iteration carries the trigger's spawn role (plan-gardener) + the
// assigned edge and resolves as the plan's bound agent, exactly as a scheduled
// fire does.
func (s *Server) handlePlanGardenerWake(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	_, gardener, ok := s.loadPlanGardenerForMutation(w, r, id)
	if !ok {
		return
	}
	// Wake is NOT gated on plan_state: it spawns one bounded iteration of the
	// plan-gardener (the same agent that drafts the plan), which is exactly the
	// manual nudge an operator uses WHILE authoring a draft. Only the standing
	// gardener LOOP (enable) is the R6 build gate; a single deliberate wake is
	// safe at any lifecycle state.
	if _, opErr := s.spawnGardenerRunNowCore(r, gardener, email); opErr != nil {
		// A one-spawn-at-a-time rejection or spawn failure — surface it inline in
		// the bar (HTTP 200 so HTMX swaps it) so the operator sees WHY nothing
		// spawned without losing the bar.
		s.renderPlanGardenerErr(w, r, id, opErr.Message, http.StatusOK)
		return
	}
	s.renderPlanGardenerErr(w, r, id, "", http.StatusOK)
}

// handlePlanGardenerCadence tunes the bound gardener's firing cadence from the
// plan cockpit (POST /plans/{id}/gardener/cadence, form value `cadence`) and
// re-renders the card (task ka-0000008237). This is the "expose cadence
// configuration in the plan interface so operators can set/tune the cadence"
// leg: an operator can dial a plan gardener's tempo — or, in event mode, clear
// the fallback watchdog entirely (empty cadence ⇒ purely event-driven) — without
// leaving /plans for the /gardener/{id} config page. Operator-gated at the route
// layer. Delegates to gardenermode.SetCadence so the mode-aware write (flat cron
// in timed mode, fallback-watchdog entry in event mode, conditions gate carried
// onto the watchdog) is never forked from the CLI path.
func (s *Server) handlePlanGardenerCadence(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	_, gardener, ok := s.loadPlanGardenerForMutation(w, r, id)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderPlanGardenerErr(w, r, id, "Bad form.", http.StatusOK)
		return
	}
	cadence := strings.TrimSpace(r.PostForm.Get("cadence"))

	// Detect the mode so we can validate the input against what the mode admits
	// (a timed gardener requires a non-empty cron; an event gardener may clear it).
	mode, err := gardenermode.Detect(gardener)
	if err != nil {
		s.renderPlanGardenerErr(w, r, id, "This gardener's trigger labels are ambiguous — resolve them on the gardener page before tuning cadence.", http.StatusOK)
		return
	}
	if cadence == "" && mode == gardenermode.ModeTimed {
		s.renderPlanGardenerErr(w, r, id, "A timed gardener needs a cron expression — clearing it would stop it firing. Switch to event mode to run purely on activity.", http.StatusOK)
		return
	}
	// Validate a non-empty cron BEFORE any write so a typo re-renders the card
	// with the message inline rather than persisting dead config that silently
	// never fires. Empty (event mode) is the intentional no-watchdog case.
	if cadence != "" {
		if _, perr := nextFireParser.Parse(cadence); perr != nil {
			s.renderPlanGardenerErr(w, r, id, "Invalid cron expression: "+perr.Error(), http.StatusOK)
			return
		}
	}

	if _, err := gardenermode.SetCadence(r.Context(), s.daemon, gardener, cadence, gardener.Fields["timezone"], gardenermode.Opts{
		Operator: canonicalOIDC(email),
	}); err != nil {
		s.logger.Error("plan gardener cadence", "id", id, "actor", email, "error", err)
		s.renderPlanGardenerErr(w, r, id, "Failed to update the cadence.", http.StatusOK)
		return
	}
	s.renderPlanGardenerErr(w, r, id, "", http.StatusOK)
}

// handlePlanGardenerRowToggle is the /plans INDEX-row sibling of
// handlePlanGardenerToggle: it flips the bound gardener's enabled field (pause ⇄
// resume) and re-renders the WHOLE index row (the plan_card partial) in place,
// so the row's unified loop+agent state chip updates live alongside the switch.
// Operator-gated at the route layer.
//
// It reuses the EXACT same mutation + gate as the cockpit toggle — the shared
// setGardenerEnabledCore (so the enable/disable + loader-default semantics are
// never forked) behind the SAME R6 build gate (gardenerEnableGatedByState), so
// the index can never become a bypass that enables a still-authoring plan's
// gardener the detail card would refuse. Only the RE-RENDER TARGET differs: the
// index row, not the #plan-agent cockpit card.
//
// This is the control-room "decide which gardeners are active" lever living ON
// the /plans row (epic ka-0000004792, step 3 ka-0000003881) — NOT a new page.
func (s *Server) handlePlanGardenerRowToggle(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	// Load the plan epic directly (not loadPlanGardenerForMutation) because the
	// no-gardener path must re-render the ROW, not the detail card — a stale click
	// (the plan lost its gardener between page load and click) then degrades to a
	// row with no lever rather than injecting a cockpit card into the row slot.
	epic, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Plan not found", http.StatusNotFound)
			return
		}
		s.logger.Error("plan row gardener toggle: get plan", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if epic == nil || epic.Type != "epic" {
		http.Error(w, "Plan not found", http.StatusNotFound)
		return
	}
	gardener := s.planGardener(ctx, id)
	if gardener == nil {
		// No gardener bound anymore — re-render the row (the lever self-drops since
		// GardenerID is empty) so a stale button degrades gracefully in place.
		s.renderPlanRow(w, r, epic, "")
		return
	}
	// Toggle to the inverse of the gardener's CURRENT effective enabled state,
	// derived the same way the row renders it (planGardenerLoopState via
	// planGardenerViewFromBead), so a rapid double-click can't desync the switch
	// from the write.
	st := planStateOf(epic)
	view := s.planGardenerViewFromBead(ctx, id, gardener, st, true, "")
	enableNext := view.Paused // paused → enable; running → disable
	// R6 build gate (task ka-0000005350): a still-authoring plan (draft or ready)
	// must not have a running gardener, so a half-baked plan never auto-builds.
	// Block ENABLING only — pausing is always allowed, and an ungated (unset)
	// plan_state toggles exactly as before. "Approve & build" (on the cockpit) is
	// the sole path that enables a gardener on a gated plan.
	if enableNext && gardenerEnableGatedByState(st) {
		s.renderPlanRow(w, r, epic, "Approve the plan for build before starting the gardener — use “Approve & build”.")
		return
	}
	if opErr := s.setGardenerEnabledCore(r, gardener, enableNext, email); opErr != nil {
		// A closed gardener (or write failure) — re-render the row with the message
		// inline rather than a bare HTTP error, so the operator sees the current
		// state after a stale click.
		s.renderPlanRow(w, r, epic, opErr.Message)
		return
	}
	s.renderPlanRow(w, r, epic, "")
}

// renderPlanRow re-renders ONE /plans index row (the plan_card partial) for the
// given plan epic, threading toggleErr (if any) into the row's inline
// per-gardener-toggle note. It reuses planIndexEnrich over a single-element slice
// so the re-rendered row's gardener state, unified chip, stall/escalation badges,
// and step rollup are derived by the EXACT same pass the page render uses — the
// index row after a toggle can never disagree with a fresh page load. Called by
// handlePlanGardenerRowToggle; the POST route is operator-gated, so the row
// always renders with the operator affordances.
func (s *Server) renderPlanRow(w http.ResponseWriter, r *http.Request, epic *beadsapi.BeadDetail, toggleErr string) {
	s.renderPlanRowWith(w, r, epic, func(row *planRowView) {
		row.GardenerToggleError = toggleErr
	})
}

// renderPlanRowWith is the general single-row re-render seam: it enriches the
// plan epic into a row the SAME way the page render does, lets the caller stamp
// its per-interaction note (toggle error, dispatch confirmation, …) via decorate,
// and swaps the plan_card partial in place. Both the per-gardener toggle and the
// index-card Dispatch button (Matt's ask, thread ka-0000001290) render through
// here so a post-interaction row can never drift from a fresh page load.
func (s *Server) renderPlanRowWith(w http.ResponseWriter, r *http.Request, epic *beadsapi.BeadDetail, decorate func(*planRowView)) {
	rows := s.planIndexEnrich(r.Context(), []*beadsapi.BeadDetail{epic})
	if len(rows) == 0 {
		s.logger.Error("plan row re-render: enrich produced no row", "id", epic.ID)
		http.Error(w, "Failed to render plan row", http.StatusInternalServerError)
		return
	}
	row := rows[0]
	if decorate != nil {
		decorate(&row)
	}
	s.renderPartial(w, r, "templates/partials/plan_card.html", "planCard", map[string]any{
		"Root": map[string]any{
			"BasePath":    s.basePath,
			"ViewerEmail": oidcauth.EmailFromContext(r.Context()),
			"IsOperator":  viewerCanOperate(r),
			// The active ?project= filter of the /plans page this row swaps into,
			// read from the Referer, so the card's project-chip suppression
			// (iter-60) matches its unswapped neighbours. Empty on an unfiltered
			// board (or a non-/plans Referer) → the chip renders as before.
			"ProjectFilter": s.refererPlansProject(r),
			// The active ?group= key of the /plans page this row swaps into (iter-62),
			// read from the Referer, so the card's project-chip suppression on a
			// ?group=project board matches its unswapped neighbours. Empty (no group)
			// on an ungrouped board (or a non-/plans Referer) → the chip renders as
			// before.
			"GroupKey": s.refererPlansGroup(r),
			// The resolved operator scope of the /plans page this row swaps into
			// (iter-61), recovered from the Referer with the SAME me-expansion +
			// cold-load-default precedence handlePlans uses, so the card's
			// creator-chip suppression matches its unswapped neighbours. Empty on
			// an "all"/anonymous board (or a non-/plans Referer) → the chip renders
			// as before.
			"EffectiveOperatedBy": s.refererPlansOperatedBy(r),
		},
		"Row": row,
	})
}
