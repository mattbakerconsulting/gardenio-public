package main

import (
	"context"
	"hash/fnv"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/planagent"
)

// plans_assistant_prompt.go — S2 of Plans v1 (epic ka-0000001409, task
// ka-0000007556). The in-page assistant lane on /plans/{id}: a
// live/waking/parked chip, a prompt textarea, a compact recent-activity
// feed, and the POST /plans/{id}/assistant/prompt verb that either nudges
// the live assistant (instant) or wakes/spawns one via S1's primitives.
//
// One POST verb handles all three states — live/parked/none. Coopmux's
// Nudge is the sub-second path for a live assistant; spawnPlanAssistant is
// the wake path for a parked assistant (its runtime_class=runsc means the
// ordinary spawn event thaws the checkpointed pod when freeze/thaw is
// enabled — S5) and the first-message path for a plan that has never had
// one. Both leave the plan wired to exactly one plan-assistant identity
// (the wake path revives the prior assistant bead IN PLACE — stable id =>
// stable S3 session key => wake continuity — S1, ka-0000002360).

// maxPlanAssistantMessageLen caps a single assistant prompt. Same ceiling
// as agent-nudge so an accidental huge paste is rejected here rather than
// blowing up in coopmux/kbeads.
const maxPlanAssistantMessageLen = maxAgentNudgeLen

// maxPlanAssistantActivity is the compact-feed row cap. The full activity
// stream lives at /agents/{assistant-id} — the plan lane shows only the
// most recent items so it stays glanceable.
const maxPlanAssistantActivity = 8

// planAssistantState enumerates the five UI states the lane renders under.
// Names ("live" | "waking" | "queued" | "parked" | "none") are UI strings baked
// into the template's chip class + placeholder copy — do not renumber lightly.
type planAssistantState string

const (
	planAssistantStateNone   planAssistantState = "none"   // no assistant bead yet
	planAssistantStateLive   planAssistantState = "live"   // assistant_state ∈ alive AND fresh
	planAssistantStateWaking planAssistantState = "waking" // just spawned / just nudged into wake
	// planAssistantStateQueued is a woken assistant whose spawn the controller
	// DECLINED because the project's account pool is fully rate-limited/exhausted
	// (cmd/controller/main.go SpawnBlocked path stamps agent_state=queued, files
	// no escalation — the reconciler re-attempts when a key frees up). It is
	// deliberately distinct from parked: parked is an idle assistant nobody woke,
	// queued is a woken one WAITING on capacity, so the chip must not collapse it
	// to a silent "parked" with no feedback (ka-0000003525).
	planAssistantStateQueued planAssistantState = "queued" // woken but spawn deferred — pool exhausted, will retry
	planAssistantStateParked planAssistantState = "parked" // assistant exists but is not live
)

// planDeliveryOutcome is the HONEST fate of a just-submitted operator message,
// deliberately distinct from the assistant-liveness chip (planAssistantState).
// The lane renders it as a transient acknowledgment line immediately after a
// send so the operator learns what actually happened to their words —
// mitigating R1 ("message into the void", ka-0000005794#10-risks-mitigations):
// a bare chip flip to "waking" used to hide whether the message was *delivered*
// to a live pod, merely *queued* on a parked one (delivered later on wake), or
// kicking off a cold *spawn*. Empty on page-load / SSE re-render (no just-sent
// message), so the ack is naturally transient — the next SSE settle clears it.
//
// It is a type ALIAS for the shared engine's DeliveryOutcome (task ka-0000002739):
// promptPlanAssistant now returns the engine's value verbatim, and the viewer's
// render helpers keep their planDelivery* spellings. Alias (not a fresh defined
// type) so the two are interchangeable at every call site.
type planDeliveryOutcome = planagent.DeliveryOutcome

const (
	planDeliveryNone         = planagent.DeliveryNone         // not a just-sent render
	planDeliverySent         = planagent.DeliverySent         // nudged a LIVE session — delivered now
	planDeliveryQueuedWaking = planagent.DeliveryQueuedWaking // queued on a parked/unreachable assistant; delivered on wake
	planDeliverySpawning     = planagent.DeliverySpawning     // first message — cold-starting a fresh assistant
)

// deliveryLabel renders the operator-facing acknowledgment copy for a just-sent
// message. Kept beside the constants so the honest wording lives with the state
// it describes. Empty string for planDeliveryNone so the template's {{if}} hides
// the line on non-send renders.
func deliveryLabel(o planDeliveryOutcome) string {
	switch o {
	case planDeliverySent:
		return "Sent — the plan agent is live and has your message."
	case planDeliveryQueuedWaking:
		return "Queued — waking the plan agent; it picks up your message when it's back."
	case planDeliverySpawning:
		return "Spawning the plan agent — your message is its first instruction."
	default:
		return ""
	}
}

// sendButtonLabel returns the operator-facing verb the compose form's submit
// button shows, adapted to the plan assistant's liveness so the button says what
// pressing it will DO — not a flat "Send" that hides whether a message spawns a
// cold agent, thaws a parked one, or nudges a live one (S5, ka-0000003325). It is
// the button-face twin of the adaptive placeholder + hint copy the compose form
// already carries, keeping the three in lockstep from ONE derivation:
//
//   - none   → "Spawn" — no assistant bead yet; the first message cold-starts one.
//   - parked → "Wake"  — an assistant exists but is not live; the message thaws it.
//   - live / waking / queued → "Send" — a reachable assistant (live now, coming
//     online, or woken-and-waiting-on-capacity) that takes the message directly.
//
// Naming the action on the button is also what lets the template gate the one
// sensitive transition — Wake (parked → thaw a dormant pod) — behind an operator
// confirm (sendButtonNeedsConfirm), while Spawn (the expected first message) and
// Send (an instant live nudge) stay single-tap. An empty/unknown state defaults
// to "Send" — the safe, non-state-changing verb.
func sendButtonLabel(state planAssistantState) string {
	switch state {
	case planAssistantStateNone:
		return "Spawn"
	case planAssistantStateParked:
		return "Wake"
	default:
		return "Send"
	}
}

// sendButtonNeedsConfirm reports whether pressing the compose button in this
// state performs a sensitive-enough action to warrant an operator confirmation
// before it fires. It gates exactly the WAKE transition (parked → thaw a dormant
// pod): reviving a checkpointed agent that someone deliberately let go idle is
// the one send that is neither expected-by-construction nor instant, so it earns
// a deliberate confirm (the operator-gating half of S5, ka-0000003325).
//
// It deliberately does NOT gate Spawn (none) or Send (live/waking/queued): the
// FIRST message to a plan with no agent is understood to start one (a confirm
// there is friction on the expected path), and a live Send must stay a single
// tap — nudging a live agent is instant and high-frequency. Gating those two
// would also block htmx's fire path under a headless dialog-dismiss, breaking
// the existing spawn/compose send flows for no safety gain.
func sendButtonNeedsConfirm(state planAssistantState) bool {
	return state == planAssistantStateParked
}

// planAssistantView is one row of the assistant lane's recent-activity feed.
// A compact projection of the underlying bead — the plan cockpit shows the
// most recent activity across the assistant's task subtree (which for a
// plan-assistant IS the plan's subtree), enough to answer "did it do
// something?" at a glance without pulling the full /agents page.
type planAssistantActivityRow struct {
	BeadID string
	Type   string
	Title  string
	Kind   string // created | updated | closed
	Ago    string
}

// planAssistantLaneData builds the agent-state half of the merged plan-agent
// card's data bag (the base planAgentPanelData folds the gardener view into).
// Kept as a plain map[string]any at the render seam so it lines up with the
// other partial helpers (planStepsData, planNarrativeData).
//
// There is deliberately NO server-rendered error field: a POST failure returns
// a non-2xx status, which htmx's default responseHandling ([45].. → swap:false)
// does NOT swap in — so any error rendered into this partial would be silently
// discarded, and re-rendering the partial on error would reset the textarea and
// lose the operator's typed message (the original R1 "message into the void").
// Errors surface client-side via the form's hx-on::response-error hook instead,
// leaving the textarea untouched. This helper is used only for successful /
// idempotent renders (page load, SSE re-render, post-send ack).
func (s *Server) planAssistantLaneData(ctx context.Context, epicID string, isOperator bool, forceState planAssistantState, delivery planDeliveryOutcome) map[string]any {
	assistant := s.planAssistant(ctx, epicID)
	state := planAssistantStateNone
	if assistant != nil {
		switch {
		case s.assistantIsLive(assistant):
			state = planAssistantStateLive
		case assistantIsQueued(assistant):
			// Woken but spawn deferred on an exhausted pool — a distinct wait,
			// not idle parked (ka-0000003525).
			state = planAssistantStateQueued
		default:
			state = planAssistantStateParked
		}
	}
	// forceState wins over the derived state on the render immediately after a
	// spawn/nudge — the assistant bead's agent_state hasn't reached "working"
	// yet, but we know we just kicked it. Chip flips to "live" or "parked"
	// naturally on the SSE re-render a moment later.
	if forceState != "" {
		state = forceState
	}
	activity := s.planAssistantActivity(ctx, epicID, assistant)

	// The persistent chat transcript (task 2b, ka-0000004607): every operator +
	// assistant turn that task 2a (ka-0000004125) persisted as a durable
	// type=message bead, projected through the SAME direction-bubble renderer the
	// bead-show chat panel and user inbox use (buildPlanTranscript → the bead_chat
	// partial). Read-only, so it is built for every viewer (not just operators),
	// exactly like the activity feed. Replaces the fire-and-forget lane's inability
	// to show what was asked and answered — the operator can now scroll the
	// conversation instead of guessing from an 8-row lifecycle feed.
	transcript := s.planTranscriptView(ctx, epicID)

	// Where the "Open →" affordance points: the coopmux terminal page for the
	// assistant's pod when derivable (per ka-0000007019), else the assistant's
	// bead page. Empty when no assistant is bound. NEVER /agents/{id} — that
	// route 404s (there is no GET /agents/{id} handler).
	assistantURL, assistantExternal := "", false
	if assistant != nil {
		assistantURL, assistantExternal = s.assistantLinkURL(assistant)
	}

	return map[string]any{
		"PlanID":            epicID,
		"State":             string(state),
		"Assistant":         assistant,
		"AssistantURL":      assistantURL,
		"AssistantExternal": assistantExternal,
		// Transcript is the mapChatView the bead_chat partial renders (the
		// direction-bubble conversation log). Empty view (HasMessages=false) on a
		// plan that has never had a turn, so the panel simply doesn't render.
		"Transcript": transcript,
		"Activity":   activity,
		"IsOperator": isOperator,
		// SendLabel is the state-adaptive verb the compose button shows (Spawn /
		// Wake / Send), and SendNeedsConfirm gates an operator confirm on the
		// heavier Spawn/Wake transitions (S5, ka-0000003325). Both are pure
		// projections of the SAME resolved `state` the chip + placeholder read, so
		// the button face, the placeholder, and the hint can never disagree.
		"SendLabel":        sendButtonLabel(state),
		"SendNeedsConfirm": sendButtonNeedsConfirm(state),
		// MaxLen binds the textarea maxlength to the server-enforced ceiling
		// so the client and server limits can't drift (input hardening) — the
		// browser stops overshoot before the POST, the server still rejects it
		// defensively (maxPlanAssistantMessageLen).
		"MaxLen": maxPlanAssistantMessageLen,
		// Delivery is the honest fate of a just-sent message (sent /
		// queued-waking / spawning), rendered as a transient ack line
		// distinct from the liveness chip. Empty on page-load / SSE
		// re-render, so the ack clears itself on the next settle (R1).
		"Delivery":      string(delivery),
		"DeliveryLabel": deliveryLabel(delivery),
		// Typing drives the "assistant is typing…" bubble at the foot of the
		// transcript (task 2c, ka-0000000311): true whenever the bound assistant is
		// actively producing a turn (agent_state in the alive bucket), replacing
		// the dead static "waking" chip so a cold-thaw / mid-reply reads as
		// "working", not "stuck" (§10-R1). Computed off the already-resolved
		// assistant (no extra lookup); the whole card re-renders on the assistant's
		// agent_state change via the plan-agent-updated frame, so the bubble
		// flips live.
		"Typing": planTranscriptTyping(assistant),
	}
}

// planTranscriptTyping reports whether the plan's bound assistant is actively
// producing a turn right now — the signal behind the "assistant is typing…"
// bubble (task 2c, ka-0000000311). True when the assistant's agent_state is in
// the alive bucket (working / spawning / yielding), which covers BOTH the
// cold-thaw window (a just-woken/just-spawned assistant that has not yet emitted
// its first reply) and an in-flight reply. It deliberately does NOT gate on
// freshness the way assistantIsLive does: a spawning assistant whose bead was
// stamped a while ago is still "coming online", and showing "typing" through
// that window is exactly the R1 mitigation (a silent gap between send and first
// reply reads as stuck). A nil assistant, an empty state, or a terminal/idle
// state → not typing.
func planTranscriptTyping(assistant *beadsapi.BeadDetail) bool {
	if assistant == nil || assistant.Fields == nil {
		return false
	}
	_, alive := agentStateAlive[assistant.Fields["agent_state"]]
	return alive
}

// planAssistantActivity returns the most recent create/update/close events
// across the plan's subtree, projected as compact activity rows. Best-effort:
// a daemon error degrades to an empty feed (the lane still renders). The
// assistant param is optional and used only to seed the interest set — a
// plan with no assistant still shows step activity so the lane has *something*
// to render on first render.
func (s *Server) planAssistantActivity(ctx context.Context, epicID string, assistant *beadsapi.BeadDetail) []planAssistantActivityRow {
	if epicID == "" {
		return nil
	}
	// The assistant's own bead + the plan epic + the plan's direct children
	// are the interest set. That mirrors /agents/{id}/events for the assistant
	// PLUS the plan's step subtree, so activity from crew agents driving steps
	// surfaces on the lane too.
	ids := make([]string, 0, 8)
	seen := map[string]struct{}{}
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	add(epicID)
	if assistant != nil {
		add(assistant.ID)
	}
	revDeps, err := s.daemon.GetReverseDependencies(ctx, epicID)
	if err == nil {
		for _, d := range revDeps {
			if d.Type == "parent-child" || d.Type == "assigned" {
				add(d.BeadID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	beads, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		s.logger.Debug("plan assistant activity: batch get", "epic", epicID, "error", err)
		return nil
	}
	rows := make([]planAssistantActivityRow, 0, len(ids))
	for _, id := range ids {
		b := beads[id]
		if b == nil {
			continue
		}
		kind := "updated"
		switch strings.ToLower(b.Status) {
		case "closed":
			kind = "closed"
		}
		row := planAssistantActivityRow{
			BeadID: b.ID,
			Type:   b.Type,
			Title:  b.Title,
			Kind:   kind,
			Ago:    humanizeTime(b.UpdatedAt),
		}
		rows = append(rows, row)
	}
	// Freshest first, capped so the lane stays glanceable.
	sort.SliceStable(rows, func(i, j int) bool {
		bi := beads[rows[i].BeadID]
		bj := beads[rows[j].BeadID]
		if bi == nil || bj == nil {
			return false
		}
		return bi.UpdatedAt.After(bj.UpdatedAt)
	})
	if len(rows) > maxPlanAssistantActivity {
		rows = rows[:maxPlanAssistantActivity]
	}
	return rows
}

// planTranscriptView pulls the plan's durable chat transcript — the operator +
// assistant turn beads task 2a (ka-0000004125) mints — and projects them into
// the mapChatView the bead_chat direction-bubble partial renders (task 2b,
// ka-0000004607). It closes the "renderer ready, unfed" gap the research brief
// named: buildPlanTranscript (the 1d spike, plan_transcript_spike.go) is reused
// VERBATIM — this method is only the daemon read that feeds it.
//
// Collection is by fields.plan_id (planTranscriptMessagesQuery), NOT by mention:
// the turn beads carry the plan binding in a purpose-named field, the SAME field
// planAssistant() binds the assistant agent bead on, so the daemon capability is
// already load-bearing in shipped code (plans_assistant.go path b). The list
// returns the full fields JSONB — turn text is not truncated — so buildPlanTranscript
// consumes the ListBeads result directly, no BatchGetBeads re-hydration needed.
//
// Best-effort: a daemon error (or a daemon that can't field-filter) degrades to
// an empty transcript — the lane still renders its chip + compose box + activity
// feed. Empty view (HasMessages=false) on a plan with no turns yet, so the panel
// simply doesn't paint.
func (s *Server) planTranscriptView(ctx context.Context, epicID string) mapChatView {
	if epicID == "" {
		return mapChatView{}
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, planTranscriptMessagesQuery(epicID))
	if err != nil {
		s.logger.Debug("plan transcript: list turns", "epic", epicID, "error", err)
		return mapChatView{}
	}
	if res == nil || len(res.Beads) == 0 {
		return mapChatView{}
	}
	return buildPlanTranscript(res.Beads, s.basePath, s.now())
}

// handlePlanAssistantPrompt is the ONE verb for messaging the plan assistant.
//   - If the plan has a live assistant → nudge it via coopmux (instant).
//   - If parked → wake it by reviving the prior assistant bead IN PLACE (same
//     bead id => stable S3 session key => wake continuity; session_resume=true
//     is flagged on the revived bead). The runtime_class=runsc on the role
//     means the spawn event thaws the checkpointed pod when S5's freeze/thaw is
//     enabled.
//   - If none → spawn a fresh assistant with the message as its first prompt.
//
// Operator-gated at the route layer; re-checks EmailFromContext as
// defense-in-depth. Returns the assistant lane partial for HTMX outerHTML swap.
func (s *Server) handlePlanAssistantPrompt(w http.ResponseWriter, r *http.Request) {
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
	// Stamp the operator on the daemon ctx so spawnPlanAssistant's write calls
	// (CreateBead, AddLabel, AddDependency) carry X-Operated-By. operatorMiddleware
	// already stamps this for browser traffic, but re-stamping here mirrors
	// quick_spawn.go and makes the handler correct on any call path (tests,
	// future direct mounts) — without it the kbeads audit resolver fail-closes
	// with HTTP 400 "operator could not be resolved" (R2, ka-0000005772), which
	// surfaces to the browser as the generic "Failed to start the plan
	// assistant" 502 (ka-0000000017).
	ctx := beadsapi.WithOperator(r.Context(), canonicalOIDC(email))

	epic, err := s.daemon.GetBead(ctx, id)
	if err != nil || epic == nil || epic.Type != "epic" {
		http.Error(w, "Plan not found", http.StatusNotFound)
		return
	}

	// Every failure below returns a non-2xx status with a concise plain-text
	// body. htmx's default responseHandling does NOT swap 4xx/5xx responses, so
	// the operator's textarea is left intact (no re-render → no data loss, the
	// original R1); the form's hx-on::response-error hook reads the body and
	// shows it inline. This is why error paths use http.Error, not a partial
	// re-render.
	message := strings.TrimSpace(r.PostForm.Get("message"))
	if message == "" {
		http.Error(w, "Message is required.", http.StatusBadRequest)
		return
	}
	if len(message) > maxPlanAssistantMessageLen {
		http.Error(w, "Message is too long.", http.StatusRequestEntityTooLarge)
		return
	}

	// Persist the operator's turn as a durable transcript bead BEFORE delivery,
	// so the message survives even if the assistant delivery below fails (the
	// D1=hybrid "beads are the source of truth" premise; task 2a ka-0000004125).
	// This is the OPERATOR half of the plan-chat transcript; the AssistantTurnMinter
	// mints the assistant half from coop's assistant_text events. It is done HERE,
	// in the handler that holds the operator's LITERAL message — NOT in the shared
	// promptPlanAssistant, which the reactive add-step path (the PlanStepReactor)
	// also calls with a SYNTHESIZED seed that must not render as an operator turn.
	// Best-effort: a mint failure is logged and swallowed so the message still
	// delivers (the transient render below is unaffected).
	s.mintOperatorTurn(ctx, epic.ID, canonicalOIDC(email), message)

	// Native (non-htmx) submit fallback (ka-0000005067): the composer form
	// carries a real method+action so a submit that fires before htmx wired the
	// form (defer-load gap, a swapped-in card htmx.process missed, or htmx not
	// loading at all) POSTs here instead of doing a bare native submit that would
	// reload the page and drop the message. htmx sets HX-Request:true on its
	// AJAX submit; its absence means this is the native fallback, so we deliver
	// the message and 303-redirect back to the plan (PRG) rather than returning a
	// fragment the browser would render as a bare page.
	htmxReq := r.Header.Get("HX-Request") == "true"

	path, outcome, _, err := s.promptPlanAssistant(ctx, epic, email, message)
	if err != nil {
		if !htmxReq {
			// Native fallback can't surface an inline error hook — land back on
			// the plan (the composer + any server-rendered error re-render) so
			// the operator is never stranded on a blank error page.
			http.Redirect(w, r, s.basePath+"/plans/"+url.PathEscape(id), http.StatusSeeOther)
			return
		}
		if path == planPromptPathNudge {
			// A hard nudge failure (not a recoverable session-gone) — surface
			// the upstream write error exactly as handleAgentNudge does.
			translateUpstreamWriteError(w, err)
			return
		}
		http.Error(w, "Failed to start the plan assistant.", http.StatusBadGateway)
		return
	}
	if !htmxReq {
		// Post/Redirect/Get for the native fallback: the message is already
		// delivered (nudged/queued/spawned above); redirect back to the plan so
		// a browser reload never re-submits and the operator sees the live card.
		http.Redirect(w, r, s.basePath+"/plans/"+url.PathEscape(id), http.StatusSeeOther)
		return
	}
	if path == planPromptPathNudge {
		s.renderPlanAgentPanel(w, r, id, planAssistantStateLive, outcome, "", http.StatusOK)
		return
	}
	// Spawn/wake: the freshly-created bead won't reach the "working" state
	// until the pod registers a session; render "waking" so the chip and hint
	// match reality until the SSE re-render arrives. The delivery outcome
	// (queued-waking vs spawning) is the honest ack for what happened to the
	// message, rendered as a transient line distinct from the chip.
	s.renderPlanAgentPanel(w, r, id, planAssistantStateWaking, outcome, "", http.StatusOK)
}

// planPromptPath enumerates which delivery path promptPlanAssistant took. The
// caller uses it to pick the right HTTP translation (a nudge error is an
// upstream-write error; a spawn error is a bad-gateway) and the right lane
// chip state (live vs waking). Type + values are aliases for the shared engine's
// PromptPath so the handler keeps comparing against these names (task
// ka-0000002739).
type planPromptPath = planagent.PromptPath

const (
	planPromptPathNudge = planagent.PromptPathNudge
	planPromptPathSpawn = planagent.PromptPathSpawn
)

// promptPlanAssistant is the single source of truth for the live/parked/none
// decision that delivers a message to a plan's assistant. It is shared by the
// explicit prompt verb (handlePlanAssistantPrompt) and the reactive add-step
// path (handlePlanAddStep):
//
//   - live assistant + a usable coopmux session → Nudge it (sub-second). Same
//     audit shape as handleAgentNudge so the /agents-scoped audit tools still
//     find it. Returns path=nudge.
//   - live but coopmux is nil/Disabled, or the assistant has no coopmux session
//     id yet (pod-up race), or the session is gone upstream → degrade to the spawn
//     path so the operator's message is never lost. Returns path=spawn.
//   - a hard (non-session-gone) nudge error → returns path=nudge + err so the
//     caller can surface it; NO spawn (mirrors the prior handler's behavior).
//   - parked or no assistant → spawn path: for a parked assistant, revive the
//     prior assistant bead IN PLACE (same bead id => stable S3 session key,
//     session_resume=true for wake continuity); on first contact, mint a fresh
//     assistant. Returns path=spawn.
//
// Returns (path, outcome, agentName, err). path drives the caller's HTTP error
// translation (a nudge error is an upstream-write error, a spawn error is a
// bad-gateway). outcome is the HONEST delivery fate for the ack line — sent
// (nudged a live pod), queued-waking (message parked on the assistant, delivered
// when it wakes), or spawning (first cold-start) — distinct from the liveness
// chip so the operator learns what actually happened to their words (R1).
// agentName is the assistant's name on the nudge path and the (re)spawned name
// on the spawn path; it is "" only when the spawn itself failed.
//
// The decision engine itself lives in internal/planagent (task ka-0000002739);
// this method is the viewer's thin adapter, unpacking the engine's PromptResult
// into the (path, outcome, name) triple the HTTP handlers already thread. The
// controller-side reactive-collab reactor calls Engine.Prompt directly.
func (s *Server) promptPlanAssistant(ctx context.Context, epic *beadsapi.BeadDetail, actor, seed string) (planPromptPath, planDeliveryOutcome, string, error) {
	res, err := s.planEngine().Prompt(ctx, epic, actor, seed)
	return res.Path, res.Outcome, res.AgentName, err
}

// mintOperatorTurn persists one operator message as a durable plan-transcript
// turn bead (task 2a, ka-0000004125). It is the OPERATOR counterpart to the
// AssistantTurnMinter's assistant turns, reusing the same planTranscriptTurnBead
// shape and dedup contract:
//
//   - turn_role = operator, so buildPlanTranscript renders it on the operator
//     (own) side of the direction-bubble log (planTurnDirection override).
//   - turn_id = "op:<hash(actor+message)>" and IdempotencyKey =
//     "plan-turn:<planID>:op:<hash>" so an htmx double-submit or a browser retry
//     of the same POST collapses to ONE bead (operator turns have no competing
//     observer, so a content+actor hash is a sufficient stable id — no coop ts).
//
// Best-effort: any daemon error is logged and swallowed. The caller must NOT
// fail the prompt POST on a mint failure — the operator's message still delivers
// to the assistant; only its transcript row is (temporarily) missing.
func (s *Server) mintOperatorTurn(ctx context.Context, epicID, actor, message string) {
	if epicID == "" || strings.TrimSpace(message) == "" {
		return
	}
	turnID := "op:" + operatorTurnHash(actor, message, s.now())
	req := planTranscriptTurnBead(epicID, actor, planTurnRoleOperator, message, turnID)
	req.IdempotencyKey = planTurnIdempotencyKey(epicID, turnID)
	if _, err := s.daemon.CreateBead(ctx, req); err != nil {
		s.logger.Warn("plan operator turn: mint transcript bead", "plan", epicID, "actor", actor, "error", err)
	}
}

// operatorTurnDedupWindow is the coarse time bucket the operator turn id folds
// the send time into. It must be wide enough to collapse an htmx double-submit
// (a double-click fires two POSTs milliseconds apart) yet narrow enough that a
// LEGITIMATELY-repeated short message ("go", "yes", "ok") sent seconds later is a
// DISTINCT turn that persists. Without a time component the content-only hash
// permanently collapses every repeat of the same message for the plan's lifetime
// (a real transcript gap for common affirmations).
const operatorTurnDedupWindow = 3 * time.Second

// operatorTurnHash returns a short stable hex digest of (actor, message,
// time-bucket) used as the operator turn id. FNV-1a is sufficient — this is a
// dedup key for htmx-double-submit collapse, not a security digest. Folding the
// send time (bucketed to operatorTurnDedupWindow) into the digest means a
// double-submit within the window collapses to one bead while the same message
// sent in a later bucket is a distinct, separately-persisted turn.
func operatorTurnHash(actor, message string, now time.Time) string {
	bucket := now.UnixNano() / int64(operatorTurnDedupWindow)
	h := fnv.New64a()
	_, _ = h.Write([]byte(actor))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(message))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.FormatInt(bucket, 10)))
	return strconv.FormatUint(h.Sum64(), 16)
}
