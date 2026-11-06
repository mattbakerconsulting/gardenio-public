package main

// plans_agent_panel.go — Step 4 of the unify-plan-gardener plan (epic
// ka-0000000081, task ka-0000001218). Collapses the two previously-independent
// plan-cockpit data bags — the gardener control bar's loop-state half
// (planGardenerBarData → {Gardener: planGardenerView}) and the assistant lane's
// agent-state half (planAssistantLaneData → a flat map) — into ONE
// planAgentPanelData structure, so handlePlanDetail builds a single bag rather
// than two and the derived unified state lives in exactly one place. Step 6
// (ka-0000000116) then collapsed the two render partials into the ONE
// plan_agent.html card this file also hosts the render/handler seams for.
//
// The unify plan (Matt's direction) is: a plan carries ONE first-class
// plan-gardener — one role, one chip, one control surface, both watchdog and
// interactive agent. Steps 1-3 merged the plumbing (a real plan-gardener role +
// the resolver + the spawn seams flipping to it — the transitional
// plan-assistant dual-read those steps added was later hard-removed, decision
// ka-0000007411); step 4 merged the DATA into the one bag built here; step 5
// derived the single chip; and step 6 (ka-0000000116) collapsed the two render
// partials into the ONE plan_agent.html card — this file also hosts that card's
// render/handler seams (renderPlanAgentPanel, renderPlanAgentFragment,
// handlePlanAgentView).

import (
	"context"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// planAgentState is the unified state the merged plan-agent panel derives — it
// folds the gardener trigger's LOOP lifecycle and the bound iteration's AGENT
// liveness into one structure. The two facets are ORTHOGONAL and must stay
// separate: a running loop can have a parked agent between iterations, and a
// paused loop can still have a live hand-woken run — so collapsing them to a
// single enum here would lose information. Step 5 (ka-0000001332) renders these
// two facets as ONE coherent chip ("running · live", "paused", …); this struct
// is the honest source it reads from, so the chip can never show two
// contradictory states.
type planAgentState struct {
	// Loop is the gardener trigger's lifecycle: running (bound + enabled),
	// paused (bound + disabled), retired (bound but closed), or none (no
	// gardener bound). Mirrors planGardenerView.State.
	Loop planGardenerState
	// Agent is the bound iteration's liveness: live (alive + fresh), waking
	// (just spawned/nudged), parked (exists but not live), or none (no
	// assistant bead yet). Mirrors the assistant lane's State.
	Agent planAssistantState
	// FiringMode is the gardener trigger's timed⇄event firing mode: "cron" (⏱),
	// "event" (⚡), or "" (no gardener bound / condition-gated). A THIRD orthogonal
	// facet (Task 8, epic ka-0000006020): the loop can be running-or-paused
	// independent of whether a clock or subtree activity fires it. Carried here so
	// the chip renders the glyph next to the loop/agent state without a second
	// derivation, and so all three chip fold sites (detail panel, gardener-bar
	// re-render, /plans index) stay in lockstep.
	FiringMode string
}

// planAgentStateFrom folds the gardener view's loop state and the assistant
// lane's agent state into the unified planAgentState. Pure (no I/O) so the
// derivation is exhaustively truth-tabled in the unit test; empty facets
// normalise to their "none" sentinel so a partially-bound plan (a gardener but
// no live iteration yet, or vice versa) still yields a well-formed state.
func planAgentStateFrom(gardener planGardenerView, agent planAssistantState) planAgentState {
	loop := gardener.State
	if loop == "" {
		loop = planGardenerStateNone
	}
	if agent == "" {
		agent = planAssistantStateNone
	}
	// FiringMode is a third orthogonal facet (Task 8) — it rides through verbatim
	// from the gardener view; the empty string (no gardener / condition-gated) means
	// "no glyph". It is NOT normalised to a "none" sentinel like the two lifecycle
	// facets, because "" is the meaningful "render nothing" value for the glyph.
	return planAgentState{Loop: loop, Agent: agent, FiringMode: gardener.FiringMode}
}

// planAgentChipView is the render projection of the unified plan-agent chip
// (step 5, ka-0000001332) — ONE compound chip conveying what previously needed
// two (the gardener loop chip + the assistant liveness chip). It is derived from
// the unified planAgentState so a plan can never show two contradictory chips.
type planAgentChipView struct {
	// Label is the compound chip text: "running · live", "paused", "retired",
	// "none", etc. Loop-primary — the durable "is this plan being driven" word —
	// with the agent liveness folded in as a "· live" / "· waking" sub-signal
	// when it adds information the loop word doesn't already carry.
	Label string
	// Tone drives the pill color class (.plan-agent-chip-<Tone>). It follows the
	// LOOP state when a gardener is bound (running/paused/retired) and falls back
	// to the AGENT state for a gardener-less plan (live/waking/parked/none), so a
	// legacy assistant-only plan still reads correctly.
	Tone string
	// Dot is the pulsing liveness dot's state (.plan-agent-chip-dot-<Dot>):
	// "live" (green pulse), "waking" (amber pulse), or "queued" (violet pulse —
	// a woken iteration whose spawn is deferred on an exhausted pool) when a
	// bound loop has an active iteration, "" (no dot) otherwise. Only shown when
	// the loop is bound, because for a gardener-less plan the Tone already IS the
	// agent state.
	Dot string
	// Title is the hover explanation — a loop clause plus an agent clause — so the
	// two folded facets are legible on hover even though the label is compact.
	Title string
	// FiringMode is the gardener's timed⇄event firing mode: "cron" (⏱), "event"
	// (⚡), or "" (no glyph). Rendered as a small glyph beside the loop/agent
	// state in the shared plan_agent_chip partial (Task 8, epic ka-0000006020).
	// Purely a display facet — it does not affect Tone/Dot/Label, which stay
	// loop-primary.
	FiringMode string
	// NextFire is the forward-looking "· fires in 12m" facet on an ARMED-IDLE
	// cron gardener (ka-0000005352 iter-56): the humanized time UNTIL the loop's
	// next scheduled run ("in 12m" / "in 3h" / "in 2d", via fmtTimeUntil). It is
	// the temporal INVERSE of the whole backward "how long since" wait-age family
	// every other card signal already carries (decision "· 32d", escalation
	// "· 40d", stall "· 3d", stuck/queued crew "· 6h"/"· 12m", MR "· 8d", now-step
	// "· 5d", ready "· 4d"): those all answer "how long has this been waiting",
	// while an armed-idle cron gardener's card answered NEITHER "how long idle" nor
	// "when does it next move the plan". A running loop with nothing executing
	// right now reads "idle" (planAgentChip re-labels a running loop with a
	// parked/none agent to the calm "idle" so green never over-claims activity) —
	// honest, but silent about WHEN the schedule next fires.
	//
	// Populated ONLY by setNextFire, gated on Tone=="idle" (the armed-but-not-
	// executing state — a paused/retired/none loop is not scheduled to fire, and a
	// running·live loop is already working so its next-fire is moot) AND
	// FiringMode=="cron" (an event gardener fires on subtree activity, not a clock,
	// so there is no meaningful countdown). NOT set inside the pure planAgentChip:
	// it needs s.now() + the cron/tz off the gardener bead, so it is folded at the
	// enrich seam (index) / panel-build seam (cockpit) where those live, keeping
	// planAgentChip's 4×4 truth-table byte-identical (the iter-54 precision).
	// Empty for every non-idle / non-cron chip, an unparseable cron, or a gardener-
	// less plan — the chip then renders exactly as before, no facet.
	NextFire string
}

// loopChipWord maps a gardener loop state to its chip word.
func loopChipWord(s planGardenerState) string {
	switch s {
	case planGardenerStateRunning:
		return "running"
	case planGardenerStatePaused:
		return "paused"
	case planGardenerStateRetired:
		return "retired"
	default:
		return "none"
	}
}

// agentChipWord maps an assistant liveness state to its chip word. "none"
// renders as "not started" (matching the pre-unification assistant chip copy)
// when it is the whole label; as a "· <word>" sub-signal it is never used for
// none/parked.
func agentChipWord(s planAssistantState) string {
	switch s {
	case planAssistantStateLive:
		return "live"
	case planAssistantStateWaking:
		return "waking"
	case planAssistantStateQueued:
		return "rate-limited"
	case planAssistantStateParked:
		return "parked"
	default:
		return "not started"
	}
}

// loopTitleClause / agentTitleClause build the two halves of the chip's hover
// title so the folded facets stay legible.
func loopTitleClause(s planGardenerState) string {
	switch s {
	case planGardenerStateRunning:
		return "The plan's gardener loop is running — it spawns iterations on its schedule."
	case planGardenerStatePaused:
		return "The plan's gardener loop is paused — it won't spawn new iterations until resumed."
	case planGardenerStateRetired:
		return "The plan's gardener loop is retired (closed) — no further runs."
	default:
		return "No gardener loop is bound to this plan yet."
	}
}

func agentTitleClause(s planAssistantState) string {
	switch s {
	case planAssistantStateLive:
		return "An iteration is live and working now."
	case planAssistantStateWaking:
		return "An iteration is coming online."
	case planAssistantStateQueued:
		return "An iteration was woken but its spawn is deferred — the project's account pool is rate-limited; it will retry when a key frees up."
	case planAssistantStateParked:
		return "No iteration is active right now."
	default:
		return "No iteration has run yet."
	}
}

// planAgentChip folds the unified planAgentState into the single chip view. Pure
// (no I/O) so the whole 4×4 truth table is exercised in the unit test. The
// design (pinned on task ka-0000001332): the chip is LOOP-primary with the AGENT
// liveness as a secondary signal, because the loop is the durable "is this plan
// being driven" state and the agent liveness is the transient "is something
// happening right now" state — orthogonal facets that must never contradict.
func planAgentChip(st planAgentState) planAgentChipView {
	loop := st.Loop
	if loop == "" {
		loop = planGardenerStateNone
	}
	agent := st.Agent
	if agent == "" {
		agent = planAssistantStateNone
	}

	v := planAgentChipView{
		Title:      loopTitleClause(loop) + " " + agentTitleClause(agent),
		FiringMode: st.FiringMode,
	}

	if loop == planGardenerStateNone {
		// Gardener-less plan: the chip reflects the assistant alone (a legacy /
		// ad-hoc plan with an assistant but no bound gardener loop), so the Tone
		// follows the agent and there is no separate liveness dot.
		v.Tone = string(agent)
		if agent == planAssistantStateNone {
			v.Tone = "none"
		}
		v.Label = agentChipWord(agent)
		return v
	}

	// Loop bound: Tone follows the loop; the agent liveness folds in as a
	// "· live"/"· waking" sub-signal + a pulsing dot when there is an active
	// iteration (a parked/none agent adds nothing the loop word doesn't already
	// convey, so it is omitted rather than shown as "· parked").
	v.Tone = loopChipWord(loop)
	label := loopChipWord(loop)
	switch agent {
	case planAssistantStateLive:
		label += " · live"
		v.Dot = "live"
	case planAssistantStateWaking:
		label += " · waking"
		v.Dot = "waking"
	case planAssistantStateQueued:
		// A woken iteration whose spawn was deferred on an exhausted pool
		// (ka-0000003525). Unlike parked, this DOES add information the loop
		// word lacks — the loop is driving but capacity is blocking the wake —
		// so it folds in as a sub-signal with a queued dot rather than being
		// dropped.
		label += " · rate-limited"
		v.Dot = "queued"
	default:
		// No active iteration (parked / none). For a RUNNING loop, the bare
		// "running" word over-claims activity — Matt's ask (thread
		// ka-0000001290): "plans without a running agent show as running". The
		// loop is armed and will fire on its schedule, but nothing is executing
		// right now, so re-label + re-tone to the calm "idle". This makes the
		// green "running" tone mean exactly one thing — an agent is genuinely
		// live — instead of conflating "the loop is enabled" with "work is
		// happening". Paused/retired keep their own honest word (they are not
		// "running"), so only the running-but-idle case is rewritten.
		if loop == planGardenerStateRunning {
			label = "idle"
			v.Tone = "idle"
		}
	}
	v.Label = label
	return v
}

// reconcileTerminal suppresses the plan-agent chip's "not started" NONE-face on a
// TERMINAL (closed/deferred) plan (ka-0000005352 iter-54). It is the last
// unreconciled automation signal on a terminal card — the twin of the iter-27/28
// lifecycle chip (planLifecycleChipState), iter-38 freshness stamp
// (planFreshnessStale), iter-39 ⛔ escalation badge (ShowEscalationBadge), iter-45
// MR-pill stale tint (planMRInfo.reconcileTerminal), and iter-46 decision chip
// (ShowDecisionChip) terminal-reconcile family.
//
// THE CONTRADICTION (measured live, prod board, status=all, 128 cards): the
// none-tone chip renders the literal word "not started" (agentChipWord's
// planAssistantStateNone arm) on 66 cards — 38 of them CLOSED plans and 1 blocked,
// many at N/N steps · 100% with a full-green bar, and 3 also carrying iter-41's
// calm-green "✓ steps done" marker. So a FINISHED plan read "closed · 100% ·
// ✓ steps done · not started" simultaneously. "not started" honestly describes the
// GARDENER-AUTOMATION lifecycle (no gardener loop / assistant iteration ever ran on
// this plan) — but on a terminal card that is a pure card-vs-truth alarm: whether
// automation drove the plan is moot history once the plan is closed/parked, and the
// word flatly contradicts the closed status chip + the 100% progress bar beside it.
//
// SCOPE (deliberately narrow, mirroring the family's per-signal boundaries): ONLY
// the none-tone "not started" face is cleared, and ONLY on a terminal plan. Three
// load-bearing guards:
//   - Terminal only: an ACTIVE gardener-less plan KEEPS "not started" — there it is
//     honest, useful signal (nothing is driving this plan; an operator may want to
//     bind a gardener). Same non-terminal-stays-honest boundary as every sibling.
//   - None-tone only: a terminal plan with a real loop state (retired/paused, or a
//     gardener-less plan whose assistant reached live/parked) keeps its own honest
//     word — "retired" on a closed plan is truthful terminal history (like iter-27's
//     genuine `complete` lifecycle chip), NOT a contradiction, so it is untouched.
//   - Idempotent: a non-terminal plan, or an already-empty chip, returns unchanged.
//
// Display-only: the underlying planAgentState is untouched, so the cockpit header
// (which renders the SAME chip via planAgentPanelData off the live panel state) and
// any state-keyed logic are byte-identical — only this board-scan index face is
// quieted. Applied at the SAME shared enrich seam (planIndexEnrichTimed) as the
// crew/MR reconciles so the page render and the single-row SSE re-render
// (renderPlanRowWith) agree. Value receiver returning a fresh value (the chip is a
// small value type embedded on the row, not a shared pointer) — no aliasing risk.
func (v planAgentChipView) reconcileTerminal(status string) planAgentChipView {
	if !planStatusTerminal(status) || v.Tone != "none" {
		return v
	}
	v.Label = ""
	return v
}

// setNextFire folds the forward-looking "· fires in 12m" facet onto an ARMED-IDLE
// cron gardener's chip (ka-0000005352 iter-56) — the temporal INVERSE of the whole
// backward "how long since" wait-age family. It parses the gardener trigger's cron
// spec (in its timezone) to the next scheduled run and humanizes the countdown
// (fmtTimeUntil: "in 12m" / "in 3h" / "in 2d"), reusing the SAME computeNextFire2
// + fmtTimeUntil helpers the /triggers next-fire column renders, so the two
// surfaces can never disagree about when a schedule fires.
//
// Gated on Tone=="idle" (the armed-but-not-executing state: a running loop whose
// agent is parked/none — a paused/retired/none loop is NOT scheduled to fire, and
// a running·live loop is already working so its next-fire is moot) AND
// firingMode=="cron" (an event gardener fires on subtree activity, not a clock, so
// there is no meaningful countdown). Applied at the enrich seam (index) and the
// panel-build seam (cockpit) where s.now() + the gardener bead's cron/tz live —
// NOT inside the pure planAgentChip, so its 4×4 truth-table stays byte-identical
// (the iter-54 precision). A parse error (unparseable cron / bad tz) or a
// non-positive delta leaves NextFire empty, so the chip reads exactly as before.
//
// Value receiver returning a fresh value (the chip is a small embedded value
// type, not a shared pointer) — no aliasing risk, matching reconcileTerminal.
func (v planAgentChipView) setNextFire(cron, tz string, now time.Time) planAgentChipView {
	if v.Tone != "idle" || v.FiringMode != "cron" || strings.TrimSpace(cron) == "" {
		return v
	}
	next, _, err := computeNextFire2(cron, tz, now)
	if err != nil {
		return v
	}
	v.NextFire = fmtTimeUntil(next, now)
	return v
}

// planAgentPanelData builds the ONE data bag the unified plan-agent panel
// renders from (step 4, ka-0000001218). It composes the two existing builders —
// planAssistantLaneData (the agent-state half: chip + transcript + activity +
// compose box) and planGardenerBarData (the loop-state half: the gardener view) —
// and folds them into a single map so handlePlanDetail passes one structure.
//
// Shape reconciliation (the scout gotcha): the two builders return DIFFERENT
// map shapes. planAssistantLaneData returns a FLAT map (PlanID/State/Assistant/
// Transcript/Activity/MaxLen/Delivery/Typing/…); planGardenerBarData returns a
// NESTED map ({Gardener: planGardenerView}). The merged bag is the flat assistant
// map with the gardener VIEW folded in under "Gardener" plus the derived
// "PanelState" and "BasePath". That keeps BOTH render partials unchanged: the
// plan_gardener partial reads .Gardener (the view) + $.BasePath, and the
// plan_assistant partial reads the flat keys + .BasePath — every field each
// partial needs is present on this ONE bag.
//
// Round-trip cost is identical to the prior two-call site in handlePlanDetail
// (which already called both builders): this composes the same two, so there is
// no added daemon traffic. The two builders are kept intact — the standalone
// GET /plans/{id}/gardener + /plans/{id}/assistant fragments, the POST-response
// re-renders, and the two SSE fragment paths still each render their own half,
// so their targeted-refresh behaviour and tests are unchanged. Merging the two
// SSE frames into one #plan-agent target is step 6's job (ka-0000000116, which
// explicitly owns "reconcile the SSE swap").
func (s *Server) planAgentPanelData(ctx context.Context, epicID string, isOperator bool, forceState planAssistantState, delivery planDeliveryOutcome, gardenerErr string) map[string]any {
	// Agent-state half — the flat lane bag is the base we fold the gardener
	// view into, so all its keys (Transcript, Activity, MaxLen, Delivery,
	// Typing, …) survive verbatim onto the merged bag.
	panel := s.planAssistantLaneData(ctx, epicID, isOperator, forceState, delivery)

	// Loop-state half — planGardenerBarData nests the view under "Gardener"; pull
	// the view out so we can both fold it in for the plan_gardener partial AND
	// feed it to the unified-state derivation. A malformed/missing value (should
	// never happen — the builder always sets it) degrades to a zero view, which
	// planAgentStateFrom normalises to the "none" loop state.
	gardenerData := s.planGardenerBarData(ctx, epicID, isOperator, gardenerErr)
	gardenerView, _ := gardenerData["Gardener"].(planGardenerView)
	gardenerBead, _ := gardenerData["gardenerBead"].(*beadsapi.BeadDetail)
	gardenerView.Park = s.planGardenerParkFor(ctx, gardenerBead, gardenerView.State)

	// Derive the unified state off the two halves. Read the agent facet back
	// from the lane bag (it is the authoritative string the assistant chip
	// renders) so the unified state can never disagree with the rendered lane.
	agentState, _ := panel["State"].(string)

	panelState := planAgentStateFrom(gardenerView, planAssistantState(agentState))
	panel["Gardener"] = gardenerView
	panel["PanelState"] = panelState
	// The unified chip (step 5, ka-0000001332): the ONE compound chip rendered in
	// the merged card's header, derived from the unified state so it can never
	// contradict itself. Also stamped onto the gardener view (below, in
	// planGardenerBarData) so the standalone gardener fragment + POST re-renders
	// carry it too — but recomputed here off the FULL panel state so the
	// page-load render has the agent half even before an SSE frame lands.
	// Fold the armed-idle cron gardener's next-fire facet (iter-56) here too — the
	// page-load render path — so "· fires in 12m" shows on first paint, not only
	// after an SSE frame. gardenerView carries the cron/tz; setNextFire no-ops off
	// idle+cron, so a running·live / paused / event gardener is untouched.
	panel["Chip"] = planAgentChip(panelState).setNextFire(gardenerView.Cron, gardenerView.Timezone, s.now())
	panel["BasePath"] = s.basePath
	return panel
}

// planAgentPartialPath / planAgentPartialName identify the merged plan-agent
// card partial (step 6, ka-0000000116) — the single render target that replaced
// plan_gardener.html + plan_assistant.html.
const (
	planAgentPartialPath = "templates/partials/plan_agent.html"
	planAgentPartialName = "plan_agent"
)

// renderPlanAgentPanel writes the merged plan-agent card for one plan — the
// single HTMX swap target every gardener lever (toggle/wake/state/build) and the
// compose form re-render (step 6, ka-0000000116). It subsumes the two former
// seams: renderPlanGardenerBar (gardenerErr + non-200 status on a stale click)
// and renderPlanAssistantLane (forceState + delivery on the prompt POST).
// Callers pass only the half they know about and zero values for the rest.
func (s *Server) renderPlanAgentPanel(w http.ResponseWriter, r *http.Request, epicID string, forceState planAssistantState, delivery planDeliveryOutcome, gardenerErr string, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	data := s.planAgentPanelData(r.Context(), epicID, viewerCanOperate(r), forceState, delivery, gardenerErr)
	s.renderPartial(w, r, planAgentPartialPath, planAgentPartialName, data)
}

// renderPlanGardenerErr re-renders the merged card from a gardener-lever handler
// (toggle / wake / state / build) — gardener-side inline message, no
// assistant-side forceState or delivery ack. errMsg may be empty on a clean
// lever response; a non-200 status marks a stale click (e.g. gardener unbound
// between page load and click).
func (s *Server) renderPlanGardenerErr(w http.ResponseWriter, r *http.Request, epicID, errMsg string, status int) {
	s.renderPlanAgentPanel(w, r, epicID, "", planDeliveryNone, errMsg, status)
}

// renderPlanAgentFragment renders the merged plan-agent card to a string for
// the SSE payload — the ONE `plan-agent-updated` frame that replaced the two
// plan-gardener-updated / plan-assistant-updated frames (step 6). Returns "" on
// any error so the caller skips the frame rather than emitting a broken one.
// SSE re-renders carry no just-sent message: no forced chip state, no delivery
// ack — those belong only to the synchronous POST response.
func (s *Server) renderPlanAgentFragment(ctx context.Context, r *http.Request, epicID string, isOperator bool) string {
	tmpl, ok := s.partials[planAgentPartialPath]
	if !ok {
		s.logger.Error("plan events: plan_agent partial not found")
		return ""
	}
	bound, err := s.bindAgentLink(tmpl, r)
	if err != nil {
		s.logger.Error("plan events: bind plan_agent template", "error", err)
		return ""
	}
	data := s.planAgentPanelData(ctx, epicID, isOperator, "", planDeliveryNone, "")
	var buf strings.Builder
	if err := bound.ExecuteTemplate(&buf, planAgentPartialName, data); err != nil {
		s.logger.Error("plan events: render plan_agent", "epic", epicID, "error", err)
		return ""
	}
	return buf.String()
}

// handlePlanAgentView renders the merged plan-agent card standalone (GET
// /plans/{id}/agent) — the manual-refresh / direct-fetch sibling of the SSE
// swap. Replaces the two former GET fragments (/plans/{id}/gardener and
// /plans/{id}/assistant).
func (s *Server) handlePlanAgentView(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "Missing plan id", http.StatusBadRequest)
		return
	}
	if _, err := s.daemon.GetBead(r.Context(), id); err != nil {
		if beadMissing(err) {
			http.Error(w, "Plan not found", http.StatusNotFound)
			return
		}
		s.logger.Error("plan agent view: get bead", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	s.renderPlanAgentPanel(w, r, id, "", planDeliveryNone, "", http.StatusOK)
}
