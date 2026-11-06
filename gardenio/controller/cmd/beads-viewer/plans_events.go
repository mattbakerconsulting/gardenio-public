package main

// plans_events.go — the plan live-progress SSE stream (/plans/{id}/events): interest filters, event relevance and fragment re-renders.
// Split out of plans.go (gd-Wl6LuCnvlH); see plans.go for the Plans surface overview.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// planEventsTopics is the NATS filter set the plan live-progress stream
// subscribes to. Bead lifecycle carries every child create / claim / close /
// update the plan timeline + narrative reflect; the dependency topics let the
// stream notice a NEW doc added to the plan bundle (or a new step linked to
// the epic) mid-session and recompute its interest set live — without them a
// mid-session `gb bundle add` would only surface on reconnect (S3,
// ka-0000002473). `beads.comment.added` widens the same subscription so a
// critique comment posted on a plan step live-refreshes an open critique
// overlay (T5, ka-0000007360) — no new EventSource, one stream for the whole
// cockpit. Comma-separated: the daemon splits the ?topics= param on commas and
// ORs the patterns.
const planEventsTopics = "beads.bead.>,beads.dependency.added,beads.dependency.removed,beads.comment.added"

// handlePlanEvents serves the plan cockpit's live-progress stream as Server-Sent
// Events (task ka-0000001793, Matt's ask ka-0000008267: "the progress on it
// should be visible immediately"). On every lifecycle event for the plan epic or
// one of its direct step children, it re-renders the WHOLE plan_steps fragment
// and writes it under the `plan-updated` event; plan_events.js swaps
// #plan-steps outerHTML in place, so the timeline + progress bar update the
// instant a crew agent closes a step. When the touched bead is the epic's
// description or a bundle doc child's content (S3, ka-0000002473), the
// plan_narrative fragment is re-rendered too under `plan-narrative-updated` —
// a nudge-driven assistant edit lands on-page in ~1s with attribution, no
// reload. Dependency add/remove events touching the epic or its bundle
// recompute the interest set live, so a NEW doc added to the bundle
// mid-session starts streaming immediately (not on reconnect).
//
// Read-only/viewer-scoped: the stream renders public plan state; the rendered
// fragment's operator-only mutation buttons are gated on the viewer's own role
// (viewerCanOperate), exactly as the first server render was.
func (s *Server) handlePlanEvents(w http.ResponseWriter, r *http.Request) {
	r, cancelStream := sseRequestUntilTokenExpiry(r)
	defer cancelStream()
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "Missing plan id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	bead, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		// A 403 is the daemon refusing this user the plan (a scoped principal
		// asking for another project's): shown as not-found, like the bead page.
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusForbidden) {
			http.Error(w, "Plan not found", http.StatusNotFound)
			return
		}
		s.logger.Error("plan events: get bead", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if bead == nil || bead.Type != "epic" {
		http.Error(w, "Plan not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	filters := s.buildPlanEventFilters(ctx, id, bead)
	isOperator := viewerCanOperate(r)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	events, err := s.daemon.EventStream(ctx, planEventsTopics)
	if err != nil {
		s.logger.Error("plan events: opening event stream", "id", id, "error", err)
		fmt.Fprintf(w, ": stream open failed\n\n")
		flusher.Flush()
		return
	}

	// Comment line so intermediate proxies flush the header chunk and the
	// browser EventSource fires onopen.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			s.writePlanEvent(ctx, w, r, flusher, evt, id, bead.Status, &filters, isOperator)
		}
	}
}

// planEventFilters partitions the plan SSE stream's interest set: `steps`
// gates the timeline re-render (the epic + its direct children + the
// assistant), `narrative` gates the narrative re-render (the epic description
// + the bundle bead + its doc children). One bead can be in both sets (the
// epic is). Kept as two sets so a doc-content save doesn't flash the step
// timeline and a step close doesn't flash the narrative.
type planEventFilters struct {
	steps     map[string]struct{}
	narrative map[string]struct{}
	// bundleID is the plan's narrative bundle bead, "" when none. Dependency
	// events targeting it (a doc added mid-session) trigger a filter rebuild.
	bundleID string
	// gardenerID is the plan's bound gardener trigger bead, "" when none. A
	// lifecycle event on it (enable/disable flip, retire/close) re-renders the
	// plan-agent card via the plan-agent-updated frame. It is added to the steps
	// interest set below so writePlanEvent lets it through, but tracked
	// separately so a gardener change flashes ONLY the plan-agent card, not the
	// step timeline.
	gardenerID string
	// phaseEpicIDs are the plan's phase sub-epics (from fields.phase_order,
	// ka-0000005926). A dependency event whose target is one of these means a
	// step was added/removed UNDER a phase — it must rebuild the interest set
	// so the new grandchild step streams, exactly as a step added directly
	// under the plan epic does.
	phaseEpicIDs map[string]struct{}
	// escalationIDs are the open escalations that `blocks`-block the plan's
	// steps or the epic itself (epic ka-0000005480, WS4). A lifecycle event on
	// one — the operator approving/closing it — re-renders the blocked-by-
	// escalation banner (plan-escalations-updated) AND the step timeline (the
	// step's blocker chip clears too), live, no reload.
	escalationIDs map[string]struct{}
	// decisionIDs are the plan's clarifying-question decisions (direct
	// parent-child children of the epic, task ka-0000003157). A lifecycle event
	// on one — a gardener asking a new question, or an operator answering /
	// dismissing one — re-renders the #plan-decisions section
	// (plan-decisions-updated) so the answer lands in place and the NEXT
	// question appears live. Decisions are also in `steps` (they are
	// parent-child children, so dedupedParentChildIDs sweeps them up) but
	// isIssueType excludes them from the step timeline; tracking them
	// separately lets a resolve flash ONLY the decisions section, not the
	// timeline, and lets the comment branch skip them.
	decisionIDs map[string]struct{}
	// crewIDs are the agent beads the plan gardener has dispatched onto steps —
	// the `assigned`-edge-linked crew (ka-0000000150). A lifecycle event on one
	// (a crew flipping working→done, or failing) re-renders the #plan-crew
	// roster (plan-crew-updated). A brand-new crew, spawned mid-session, arrives
	// as a dependency.added edge INTO a step (already in filters.steps), which
	// rebuilds the interest set and re-renders the roster so the new agent
	// appears live. Crew agent beads are neither steps nor the bound
	// assistant/gardener, so without this set their state changes fall through
	// the interest gate.
	crewIDs map[string]struct{}
}

// buildPlanEventFilters computes the interest sets at connect time (and again
// on any dependency change touching the epic or its bundle).
//
// epic is the already-fetched plan bead when the caller has one (the connect
// path in handlePlanEvents fetched it to validate type) — passing it here
// avoids a redundant GetBead on every non-phased plan's filter build. Pass nil
// on the rebuild path so the phase membership is re-read fresh from the daemon.
//
// steps: the epic itself plus its direct parent-child children AND the plan's
// bound assistant (S2, ka-0000007556) so the assistant lane's
// live/parked/waking chip flips as its agent_state changes. Any assigned
// reverse-dep row on the epic already surfaces the assistant here alongside
// the step children — one union walks both.
//
// narrative: the epic (its description is the plan summary) plus the child
// bundle bead and the bundle's doc children — the beads whose description /
// fields.content feed the plan_narrative fragment (S3, ka-0000002473).
func (s *Server) buildPlanEventFilters(ctx context.Context, epicID string, epic *beadsapi.BeadDetail) planEventFilters {
	f := planEventFilters{
		steps:         map[string]struct{}{epicID: {}},
		narrative:     map[string]struct{}{epicID: {}},
		phaseEpicIDs:  map[string]struct{}{},
		escalationIDs: map[string]struct{}{},
		decisionIDs:   map[string]struct{}{},
		crewIDs:       map[string]struct{}{},
	}
	// escWalk accumulates the blocked-work candidate ids (epic + children +
	// phase grandchildren) whose forward blocks-edges the escalation interest
	// set is derived from at the end (epic ka-0000005480, WS4).
	escWalk := []string{epicID}
	revDeps, err := s.daemon.GetReverseDependencies(ctx, epicID)
	if err != nil {
		s.logger.Debug("plan events: reverse deps", "id", epicID, "error", err)
	}
	childIDs := dedupedParentChildIDs(revDeps)
	for _, cid := range childIDs {
		f.steps[cid] = struct{}{}
	}
	escWalk = append(escWalk, childIDs...)
	for _, d := range revDeps {
		if d.Type == "assigned" && d.BeadID != "" {
			f.steps[d.BeadID] = struct{}{}
		}
	}
	// Also include the field-bound assistant (fields.plan_id) — a plan whose
	// assistant landed only via the plan_id field (assigned edge race) still
	// gets its lane re-rendered on state changes.
	if a := s.planAssistant(ctx, epicID); a != nil {
		f.steps[a.ID] = struct{}{}
	}
	// Include the plan's bound gardener (Step 2, ka-0000002775) so a pause/resume
	// or retire on it re-renders the gardener bar live. It rides in the steps
	// interest set (so writePlanEvent lets its lifecycle events through) but is
	// tracked separately in gardenerID so the event handler flashes ONLY the
	// gardener bar. The gardener trigger is not a parent-child child of the plan
	// epic (it's pinned by the gardener-epic label), so it would otherwise be
	// invisible to the stream.
	if c := s.planGardener(ctx, epicID); c != nil {
		f.gardenerID = c.ID
		f.steps[c.ID] = struct{}{}
	}
	// The plan's clarifying-question decisions (task ka-0000003157): the SAME
	// Types=[decision], Parent=epicID query planDecisions renders. Populated
	// here (not from childIDs) because a decision reaches the plan via the
	// daemon's transitive Parent filter, which may include decisions the
	// reverse-dep sweep above misses; keeping the interest set == the rendered
	// set means every question the cockpit shows also live-updates. Populated
	// BEFORE the childIDs==0 early return so a fresh plan that has only
	// questions (no steps yet) still streams a new/answered question live.
	for _, did := range s.planDecisionIDs(ctx, epicID) {
		f.decisionIDs[did] = struct{}{}
	}
	if len(childIDs) == 0 {
		s.resolvePlanEscalationInterest(ctx, &f, epicID, escWalk)
		s.resolvePlanCrewInterest(ctx, &f, epicID, escWalk)
		return f
	}
	children, berr := s.daemon.BatchGetBeads(ctx, childIDs)
	if berr != nil {
		s.logger.Debug("plan events: batch get children", "id", epicID, "error", berr)
		s.resolvePlanEscalationInterest(ctx, &f, epicID, escWalk)
		s.resolvePlanCrewInterest(ctx, &f, epicID, escWalk)
		return f
	}
	// The narrative set: find the child bundle among the children, then its
	// doc children. Mirrors planProse's walk; errors degrade to "epic only"
	// (the description-driven summary still streams).
	for _, cid := range childIDs {
		c := children[cid]
		if c == nil || c.Type != "bundle" {
			continue
		}
		f.bundleID = c.ID
		f.narrative[c.ID] = struct{}{}
		if bRevDeps, derr := s.daemon.GetReverseDependencies(ctx, c.ID); derr == nil {
			for _, did := range dedupedParentChildIDs(bRevDeps) {
				f.narrative[did] = struct{}{}
			}
		}
		break
	}
	// Phase descent (ka-0000005926): the plan's phase sub-epics (fields
	// .phase_order) and THEIR issue children are grandchildren of the plan
	// epic, so they are not in childIDs. Without adding them, a step closed
	// under a phase never re-renders the timeline live (writePlanEvent drops
	// the event as out-of-interest). Add each phase sub-epic (its own status
	// flip re-renders) and batch-descend into its children. Reuse the caller's
	// pre-fetched epic when supplied; only re-fetch when it wasn't (the rebuild
	// path passes nil so a just-changed phase_order is picked up).
	if epic == nil {
		if got, gerr := s.daemon.GetBead(ctx, epicID); gerr == nil {
			epic = got
		}
	}
	if epic != nil {
		subEpicIDs := make([]string, 0)
		for _, id := range planPhaseOrder(epic) {
			c := children[id]
			if !isPhaseContainer(c) {
				continue
			}
			f.phaseEpicIDs[id] = struct{}{}
			f.steps[id] = struct{}{}
			subEpicIDs = append(subEpicIDs, id)
		}
		if len(subEpicIDs) > 0 {
			if revBySub, derr := s.daemon.BatchGetDependencies(ctx, subEpicIDs, "reverse"); derr == nil {
				for _, subID := range subEpicIDs {
					for _, gid := range dedupedParentChildIDs(revBySub[subID]) {
						f.steps[gid] = struct{}{}
						escWalk = append(escWalk, gid)
					}
				}
			}
		}
	}
	s.resolvePlanEscalationInterest(ctx, &f, epicID, escWalk)
	s.resolvePlanCrewInterest(ctx, &f, epicID, escWalk)
	return f
}

// resolvePlanEscalationInterest fills the filter's escalation interest set
// (epic ka-0000005480, WS4) so the plan's blocking escalations' lifecycle events
// (the operator approving/closing) re-render the banner + timeline live. It
// unions the SAME two paths planEscalationsBulk renders, so the SSE interest set
// matches the rendered set and a pinned escalation's close streams live too:
//
//   - PATH 1 — one forward blocks-edge batch over the plan's blocked-work
//     candidates (walkIDs) + one hydration batch, keeping open type=escalation
//     targets.
//   - PATH 2 — the requesting_plan pin (Layer 2, ka-0000007801): open
//     escalations whose fields.requesting_plan == epicID, so an orphaned escalation
//     with no live blocks-edge still clears its banner without a reload. One
//     targeted single-epic field query (this is the per-plan SSE connect path,
//     not the N-plan index).
//
// Best-effort — an error on either path leaves that path's contribution empty
// and the banner clears on reload instead.
func (s *Server) resolvePlanEscalationInterest(ctx context.Context, f *planEventFilters, epicID string, walkIDs []string) {
	// PATH 1 — the forward blocks-edge walk.
	if len(walkIDs) > 0 {
		depsByID, err := s.daemon.BatchGetDependencies(ctx, walkIDs, "forward")
		if err != nil {
			s.logger.Debug("plan events: escalation blocks walk", "error", err)
		} else {
			targets := make([]string, 0)
			seen := make(map[string]struct{})
			for _, id := range walkIDs {
				for _, d := range depsByID[id] {
					if d.Type != "blocks" || d.DependsOnID == "" {
						continue
					}
					if _, dup := seen[d.DependsOnID]; dup {
						continue
					}
					seen[d.DependsOnID] = struct{}{}
					targets = append(targets, d.DependsOnID)
				}
			}
			if len(targets) > 0 {
				if blockers, berr := s.daemon.BatchGetBeads(ctx, targets); berr != nil {
					s.logger.Debug("plan events: escalation blocker beads", "error", berr)
				} else {
					for _, b := range blockers {
						if b == nil || b.Type != "escalation" || b.Status == "closed" {
							continue
						}
						f.escalationIDs[b.ID] = struct{}{}
					}
				}
			}
		}
	}

	// PATH 2 — the requesting_plan pin.
	if epicID == "" {
		return
	}
	pinned, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"escalation"},
		Statuses: []string{"open"},
		Fields:   map[string]string{"requesting_plan": epicID},
		Limit:    escalationPinListLimit,
	})
	if err != nil {
		s.logger.Debug("plan events: escalation requesting_plan pin", "epic", epicID, "error", err)
		return
	}
	if pinned == nil {
		return
	}
	for _, b := range pinned.Beads {
		if b == nil || b.Type != "escalation" || b.Status == "closed" {
			continue
		}
		f.escalationIDs[b.ID] = struct{}{}
	}
}

// resolvePlanCrewInterest fills the filter's crew interest set (ka-0000000150):
// the agent beads `assigned`-edge-linked to any of the plan's step ids, so a
// crew flipping working→done (an agent bead update) re-renders the roster live.
// walkIDs is the plan's blocked-work candidate set (the escalation walk: epic +
// children + phase grandchildren); the epic is skipped because it has no crew —
// its ONLY `assigned` reverse-dep is the bound plan agent (assistant/gardener),
// which is NOT crew. Sweeping the epic in misclassified that agent as crew, so
// its agent_state changes emitted a spurious plan-crew-updated frame (the "No
// crew yet." lane) instead of the plan-agent-updated frame carrying the
// transcript/typing bubble — the isCrew branch in writePlanEvent short-circuits
// before the plan-agent render (ka-0000002593). ONE reverse-dep batch; crew
// agent ids become interest. Best-effort: a daemon error leaves the set empty
// and the roster re-renders on the NEXT filter rebuild (a new-crew
// dependency.added edge) or on reload. A crew added mid-session lands via the
// dependency.added rebuild branch in writePlanEvent (the edge targets a step
// already in filters.steps).
func (s *Server) resolvePlanCrewInterest(ctx context.Context, f *planEventFilters, epicID string, walkIDs []string) {
	if len(walkIDs) == 0 {
		return
	}
	depsByStep, err := s.daemon.BatchGetDependencies(ctx, walkIDs, "reverse")
	if err != nil {
		s.logger.Debug("plan events: crew assigned-edge walk", "error", err)
		return
	}
	for _, sid := range walkIDs {
		if sid == epicID {
			continue // the epic's assigned reverse-dep is the plan agent, not crew
		}
		for _, d := range depsByStep[sid] {
			if d.Type == "assigned" && d.BeadID != "" {
				f.crewIDs[d.BeadID] = struct{}{}
			}
		}
	}
}

// planNarrativeChangeKeys are the BeadUpdated Changes keys that affect the
// rendered narrative: the epic/bundle description, a doc's fields.content
// (surfaced bare as "content" by the daemon's per-field overlay,
// ka-0000004185), and the opaque "fields" back-compat key (present on every
// fields patch — kept so a daemon predating the overlay still streams).
var planNarrativeChangeKeys = []string{"description", "content", "fields", "title", "headline", "position"}

// writePlanEvent decides whether one lifecycle event touches the plan's
// interest sets and, if so, re-renders the affected fragments and writes them
// as SSE messages: `plan-updated` (step timeline), `plan-agent-updated`
// (the merged plan-agent card — step 6 ka-0000000116), `plan-narrative-updated`
// (summary + bundle docs, S3 ka-0000002473). The fragments are rebuilt from live daemon
// state (not the event payload) so they always reflect the authoritative
// view, matching the HTMX POST re-render. A dependency add/remove on the
// epic or bundle rebuilds *filters in place so a doc added mid-session
// streams immediately.
func (s *Server) writePlanEvent(ctx context.Context, w http.ResponseWriter, r *http.Request, flusher http.Flusher, evt beadsapi.SSEEvent, epicID, planStatus string, filters *planEventFilters, isOperator bool) {
	// Dependency topics carry a different payload shape ({"dependency":{...}}).
	// A new parent-child edge INTO the epic (new step / new bundle) or INTO the
	// bundle (new doc) invalidates the connect-time interest sets — rebuild
	// them and re-render both fragments so the new child appears at once.
	if strings.HasPrefix(evt.Event, "beads.dependency.") {
		var dwrap struct {
			Dependency *struct {
				BeadID      string `json:"bead_id"`
				DependsOnID string `json:"depends_on_id"`
				Type        string `json:"type"`
			} `json:"dependency"`
			// beads.dependency.removed carries the ids at the top level.
			BeadID      string `json:"bead_id"`
			DependsOnID string `json:"depends_on_id"`
			Type        string `json:"type"`
		}
		if err := json.Unmarshal(evt.Data, &dwrap); err != nil {
			return
		}
		target := dwrap.DependsOnID
		source := dwrap.BeadID
		edgeType := dwrap.Type
		if dwrap.Dependency != nil {
			target = dwrap.Dependency.DependsOnID
			source = dwrap.Dependency.BeadID
			edgeType = dwrap.Dependency.Type
		}
		// An edge FROM a step (or the epic) to anything — e.g. the blocks edge
		// `gb escalation create` writes (epic ka-0000005480, WS1) — changes the
		// step's blocker chips and possibly the blocked-by-escalation banner.
		// Rebuild the interest set (picking up the new escalation) and
		// re-render the timeline + banner so a fresh escalation shouts
		// immediately, not on reload.
		_, fromStep := filters.steps[source]
		if fromStep && source != "" {
			*filters = s.buildPlanEventFilters(ctx, epicID, nil)
			s.writePlanSSEFragment(w, "plan-updated", s.renderPlanStepsFragment(ctx, r, epicID, isOperator))
			s.writePlanSSEFragment(w, "plan-escalations-updated", s.renderPlanEscalationsFragment(ctx, epicID))
			flusher.Flush()
			return
		}
		// A NEW CREW being dispatched: SpawnAgent writes an `assigned` edge FROM
		// the crew agent TO the step (source=crew, target=step). The crew is not
		// yet in any interest set, but the step it targets IS in filters.steps —
		// rebuild (picking the new crew into filters.crewIDs) and re-render the
		// roster so the freshly-spawned agent appears live, no reload
		// (ka-0000000150). Gated on the edge being an `assigned` one into a step
		// so an ordinary parent-child sub-task edge into a step (also a
		// filters.steps member) doesn't spuriously re-render the roster. A
		// dependency.removed carries no type on the daemon's top-level shape for
		// some edge kinds; treat an empty type conservatively as NOT a crew edge.
		_, intoStep := filters.steps[target]
		if edgeType == "assigned" && intoStep && target != "" && target != epicID {
			*filters = s.buildPlanEventFilters(ctx, epicID, nil)
			s.writePlanSSEFragment(w, "plan-crew-updated", s.renderPlanCrewFragment(ctx, r, epicID, planStatus, isOperator))
			flusher.Flush()
			return
		}
		// Rebuild when the edge targets the plan epic, its bundle, OR one of its
		// phase sub-epics (a step added/removed under a phase — ka-0000005926).
		_, intoPhase := filters.phaseEpicIDs[target]
		if target != epicID && (filters.bundleID == "" || target != filters.bundleID) && !intoPhase {
			return
		}
		// nil epic: re-read phase membership fresh — the edge event may itself be
		// a phase sub-epic being added to (or removed from) the plan.
		*filters = s.buildPlanEventFilters(ctx, epicID, nil)
		s.writePlanSSEFragment(w, "plan-updated", s.renderPlanStepsFragment(ctx, r, epicID, isOperator))
		s.writePlanSSEFragment(w, "plan-narrative-updated", s.renderPlanNarrativeFragment(ctx, r, epicID, isOperator, ""))
		// A new clarifying-question decision auto-parents to the plan epic as a
		// parent-child edge (task ka-0000003157): the create fires bead.created
		// (dropped — not yet in any interest set) THEN this dependency.added.
		// The rebuild above picked the new decision into filters.decisionIDs, so
		// re-render the section now to make the gardener's NEXT question appear
		// inline live. Harmless on a step/bundle add (the section re-renders
		// identically), which keeps the new-question-appears-live guarantee
		// simple and race-free.
		s.writePlanSSEFragment(w, "plan-decisions-updated", s.renderPlanDecisionsFragment(ctx, r, epicID, isOperator))
		flusher.Flush()
		return
	}

	// Comment topic carries a different payload shape ({"comment":{...}}) and is
	// NOT a bead create/update/close, so agentEventKind() returns "" for it — it
	// must be handled here, before the lifecycle parse below drops it. A comment
	// added to a step in the plan's interest set emits a `plan-comments-updated`
	// SSE event carrying the freshly-rendered critique thread fragment for that
	// step; the gutter shim swaps it into an open overlay when the step matches
	// (T5, ka-0000007360). The branch is unconditional (no flag gate).
	//
	// The critique overlay only ever binds to a real STEP bead (a gutter icon is
	// injected exclusively onto li.plan-tl-step[data-bead-id] rows), so a comment
	// on any non-step bead can never refresh an open overlay. filters.steps is a
	// SUPERSET of the step children: buildPlanEventFilters also seeds it with the
	// epic id itself, the bound assistant agent bead, and every phase sub-epic
	// (phaseEpicIDs) so those beads' lifecycle events re-render the timeline. To
	// avoid spending a GetComments round-trip + SSE frame on a comment posted to
	// one of those non-step members, drop the two members that are structurally
	// guaranteed NOT to be steps — the epic and its phase sub-epics — before
	// emitting. (The assistant bead is not separately tracked here and would only
	// receive critique comments in a degenerate flow; the client-side
	// frameStep===boundStep gate in plan_gutter.js is the final backstop that
	// keeps any stray frame from clobbering the thread the operator is reading.)
	if evt.Event == "beads.comment.added" {
		var cwrap struct {
			Comment *struct {
				BeadID string `json:"bead_id"`
			} `json:"comment"`
		}
		if err := json.Unmarshal(evt.Data, &cwrap); err != nil || cwrap.Comment == nil {
			return
		}
		stepID := cwrap.Comment.BeadID
		if stepID == "" {
			return
		}
		if _, inSteps := filters.steps[stepID]; !inSteps {
			return
		}
		if stepID == epicID {
			return // the plan epic is in filters.steps but is not a critique step
		}
		if _, isPhaseEpic := filters.phaseEpicIDs[stepID]; isPhaseEpic {
			return // phase sub-epics are timeline rows, not critique steps
		}
		if filters.gardenerID != "" && stepID == filters.gardenerID {
			return // the bound gardener trigger is in filters.steps but is not a step (Step 2, ka-0000002775)
		}
		if _, isDecision := filters.decisionIDs[stepID]; isDecision {
			return // clarifying-question decisions are parent-child children (so in filters.steps) but are not critique steps (task ka-0000003157)
		}
		if s.writePlanSSEFragment(w, "plan-comments-updated", s.renderPlanCritiqueFragment(ctx, r, epicID, stepID, isOperator)) {
			flusher.Flush()
		}
		return
	}

	var wrapper struct {
		Bead *struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			// Fields is kept RAW (not decoded to a typed map) so this shared
			// unmarshal can never fail on a bead whose fields carry a non-string
			// value (arrays like phase_order, numbers, …) — decoding to
			// map[string]string there would error the WHOLE wrapper and silently
			// drop the event, breaking the step/narrative/gardener re-renders. The
			// transcript branch below extracts fields.plan_id tolerantly, and only
			// for a type=message bead. The daemon streams the full bead (incl.
			// fields) on bead.created/updated (events.BeadCreated/BeadUpdated carry
			// *model.Bead), so this needs no GetBead round-trip (2c, ka-0000000311).
			Fields json.RawMessage `json:"fields"`
		} `json:"bead"`
		Changes map[string]any `json:"changes"`
		Actor   string         `json:"actor"`
	}
	if err := json.Unmarshal(evt.Data, &wrapper); err != nil || wrapper.Bead == nil {
		return
	}
	if agentEventKind(evt.Event) == "" {
		return // not a create/update/close — nothing the cockpit reflects.
	}
	// A lifecycle event on a blocking escalation (epic ka-0000005480, WS4) —
	// the operator approving/closing it — clears the blocked-by-escalation
	// banner AND the step rows' escalation chips live. The escalation is not a
	// step, so route it before the steps/narrative gate and return. The filter
	// rebuild drops a now-closed escalation from the interest set.
	if _, isEsc := filters.escalationIDs[wrapper.Bead.ID]; isEsc {
		*filters = s.buildPlanEventFilters(ctx, epicID, nil)
		wroteEsc := s.writePlanSSEFragment(w, "plan-escalations-updated", s.renderPlanEscalationsFragment(ctx, epicID))
		if s.writePlanSSEFragment(w, "plan-updated", s.renderPlanStepsFragment(ctx, r, epicID, isOperator)) || wroteEsc {
			flusher.Flush()
		}
		return
	}
	// A lifecycle event on one of the plan's clarifying-question decisions
	// (task ka-0000003157) — an operator answering/dismissing it here, or a
	// gardener closing it — re-renders the #plan-decisions section IN PLACE so
	// the answer lands and the header count updates without a reload. Decisions
	// are NOT steps (isIssueType excludes them), so route before the
	// steps/narrative gate and return; a resolve must flash ONLY the decisions
	// section, never the step timeline. A brand-new question (a fresh decision
	// bead created mid-session) is not in this connect-time set yet — it rides
	// the dependency.added rebuild branch above when it is auto-parented to the
	// epic, which now also emits a plan-decisions-updated frame.
	if _, isDecision := filters.decisionIDs[wrapper.Bead.ID]; isDecision {
		if s.writePlanSSEFragment(w, "plan-decisions-updated", s.renderPlanDecisionsFragment(ctx, r, epicID, isOperator)) {
			flusher.Flush()
		}
		return
	}
	// A lifecycle event on a crew agent the plan gardener dispatched onto a step
	// (ka-0000000150) — the agent flipping working→done, failing, or closing —
	// re-renders the #plan-crew roster IN PLACE so "how it's going" stays honest
	// without a reload. Crew agent beads are neither steps nor the bound
	// assistant/gardener, so route them before the steps/narrative gate and
	// return; a crew state flip must flash ONLY the roster, never the step
	// timeline. A brand-new crew rides the assigned-edge dependency.added branch
	// above (it emits plan-crew-updated on the edge INTO the step).
	if _, isCrew := filters.crewIDs[wrapper.Bead.ID]; isCrew {
		if s.writePlanSSEFragment(w, "plan-crew-updated", s.renderPlanCrewFragment(ctx, r, epicID, planStatus, isOperator)) {
			flusher.Flush()
		}
		return
	}
	// A plan-transcript turn bead (2c, ka-0000000311): a type=message bead bound
	// to THIS plan by fields.plan_id (minted by handlePlanAssistantPrompt for the
	// operator turn and the AssistantTurnMinter for the assistant turn, 2a). The
	// (type=message AND fields.plan_id==epicID) predicate is exactly the read-path
	// contract planTranscriptMessagesQuery relies on — a plan_id on a type=message
	// bead is, by 2a's invariant (TestSquawkBeadExcludedFromTranscriptQuery), the
	// transcript-turn marker; a squawk carries no plan_id so it never matches
	// either path. Turn beads are bound by a MENTION, not a parent-child edge, so
	// they are in neither filters.steps nor filters.narrative — without this
	// branch they fall through the interest gate below and never stream (the gap
	// 2b flagged). The transcript renders INSIDE the #plan-agent card, so a new
	// turn re-renders that card via the plan-agent-updated frame — the transcript
	// rides along, no separate frame needed. Recognise the turn straight off the
	// event payload (no GetBead) and re-render the card so the new line appears
	// live. Routed before the steps/narrative gate and returned so a turn never
	// also flashes the step timeline.
	if wrapper.Bead.Type == "message" && eventFieldEquals(wrapper.Bead.Fields, planTranscriptPlanIDField, epicID) {
		if s.writePlanSSEFragment(w, "plan-agent-updated", s.renderPlanAgentFragment(ctx, r, epicID, isOperator)) {
			flusher.Flush()
		}
		return
	}
	_, inSteps := filters.steps[wrapper.Bead.ID]
	_, inNarrative := filters.narrative[wrapper.Bead.ID]
	if !inSteps && !inNarrative {
		return
	}

	wrote := false
	// The plan's gardener (Step 2, ka-0000002775) is in the steps interest set so
	// its lifecycle events pass the gate above, but it is NOT a step — a
	// pause/resume or retire on it must flash ONLY the plan-agent card, never
	// the step timeline. Route it first and return so the generic step render
	// below doesn't also fire.
	if filters.gardenerID != "" && wrapper.Bead.ID == filters.gardenerID {
		if s.writePlanSSEFragment(w, "plan-agent-updated", s.renderPlanAgentFragment(ctx, r, epicID, isOperator)) {
			flusher.Flush()
		}
		return
	}
	// A phase_order change on the plan epic (ka-0000004061): a phase sub-epic
	// was added to / removed from / reordered within the plan's phase list
	// (UpdateBeadFieldsTyped fires beads.bead.updated on the epic, which lands
	// here — it is NOT a dependency event, so the dependency branch's rebuild
	// never covered it). The connect-time interest set (filters.phaseEpicIDs) is
	// now stale, so a step LATER added under a newly-listed phase sub-epic would
	// fail writePlanEvent's intoPhase interest check and never stream — it
	// appeared only on reload/reconnect. Rebuild the interest set fresh (nil
	// epic → re-read phase_order from the daemon) BEFORE the timeline re-renders
	// below, so both this render and every subsequent grandchild event see the
	// new membership. Only the epic's OWN updates reach here (it's in
	// filters.steps), so this rebuild is rare and idempotent.
	if wrapper.Bead.ID == epicID && planPhaseOrderChanged(wrapper.Changes) {
		*filters = s.buildPlanEventFilters(ctx, epicID, nil)
	}
	// The step timeline reflects everything in its interest set. The plan-agent
	// card only cares about the agent bead itself (agent-typed) and the epic's
	// plan_state — a step close shouldn't flash the card; an agent state change
	// should.
	if inSteps {
		if s.writePlanSSEFragment(w, "plan-updated", s.renderPlanStepsFragment(ctx, r, epicID, isOperator)) {
			wrote = true
		}
		if wrapper.Bead.Type == "agent" {
			// An agent_state change on an agent in the interest set re-renders the
			// #plan-agent card — which carries the transcript (2b), the "assistant
			// is typing…" indicator (2c, off the bound agent's agent_state
			// alive-bucket) AND the unified chip's liveness half (step 5,
			// ka-0000001332). One frame keeps all three honest; the pre-step-6
			// dual-emit (plan-assistant-updated + plan-gardener-updated) collapsed
			// into this single plan-agent-updated frame (step 6, ka-0000000116).
			if s.writePlanSSEFragment(w, "plan-agent-updated", s.renderPlanAgentFragment(ctx, r, epicID, isOperator)) {
				wrote = true
			}
		}
		// Approve→build lifecycle (task ka-0000005350): the plan's authoring state
		// lives on the EPIC's fields.plan_state, and the plan-agent card renders
		// the lifecycle chip + approve/edit levers off it. When the epic's
		// plan_state changes — an operator on another tab clicks "Mark ready" /
		// "Approve & build", or the assistant finalizes the draft — re-render the
		// card so the chip + levers flip live. The gardener bead's OWN events
		// already re-render the card via the gardenerID branch above; this covers
		// the epic as the state carrier.
		if wrapper.Bead.ID == epicID && planStateChanged(wrapper.Changes) {
			if s.writePlanSSEFragment(w, "plan-agent-updated", s.renderPlanAgentFragment(ctx, r, epicID, isOperator)) {
				wrote = true
			}
		}
	}
	// The narrative re-renders only when a content-bearing key actually
	// changed (or the event is a create/close reshaping the doc set) — a
	// status flip or assignee change on the epic shouldn't flash the prose.
	if inNarrative && planNarrativeEventRelevant(evt.Event, wrapper.Changes) {
		if s.writePlanSSEFragment(w, "plan-narrative-updated", s.renderPlanNarrativeFragment(ctx, r, epicID, isOperator, wrapper.Actor)) {
			wrote = true
		}
	}
	if wrote {
		flusher.Flush()
	}
}

// eventFieldEquals reports whether a bead event's raw fields blob carries
// key==want. The blob is decoded leniently into map[string]json.RawMessage so a
// sibling field holding a non-string value (an array like phase_order, a number)
// can never fail the whole decode — only the ONE key we care about is coerced to
// a string, and any parse hiccup returns false (the caller then treats the event
// as not-a-transcript-turn and falls through to the normal interest gate). Used
// by writePlanEvent to recognise a plan-transcript turn bead (fields.plan_id ==
// epicID) straight off the SSE payload, avoiding a GetBead round-trip
// (2c, ka-0000000311).
func eventFieldEquals(raw json.RawMessage, key, want string) bool {
	if len(raw) == 0 || key == "" {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return false
	}
	return s == want
}

// planStateChanged reports whether a BeadUpdated Changes map touched the plan's
// authoring lifecycle (task ka-0000005350). The daemon emits per-field change
// keys (the bare "plan_state" key) alongside the opaque "fields" key
// (kbeads beads_field_changes); either presence means the gardener bar's
// lifecycle chip + levers may have moved and should re-render.
func planStateChanged(changes map[string]any) bool {
	if changes == nil {
		return false
	}
	if _, ok := changes[planStateFieldKey]; ok {
		return true
	}
	// Back-compat: an older daemon (or a merged-field write) only emits the
	// opaque "fields" key. Re-render on any fields change rather than miss a
	// state flip — the gardener-bar render is cheap and idempotent.
	if _, ok := changes["fields"]; ok {
		return true
	}
	return false
}

// planPhaseOrderChanged reports whether a BeadUpdated Changes map on the plan
// epic touched fields.phase_order — a phase sub-epic was added to, removed
// from, or reordered within the plan's phase list (ka-0000004061). The daemon
// emits the bare "phase_order" per-field key (kbeads beads_field_changes)
// alongside the opaque "fields" key; either presence means filters.phaseEpicIDs
// is now stale and must be rebuilt so a later step added UNDER the freshly-listed
// phase sub-epic passes writePlanEvent's intoPhase interest check and streams
// live (pre-fix it appeared only on reload/reconnect).
func planPhaseOrderChanged(changes map[string]any) bool {
	if changes == nil {
		return false
	}
	if _, ok := changes[planPhaseFieldKey]; ok {
		return true
	}
	// Back-compat: an older daemon (or a merged-field write) only emits the
	// opaque "fields" key. Rebuild on any fields change rather than miss a
	// membership change — the filter rebuild is idempotent and only fires on
	// the epic's own updates (already a rare event).
	if _, ok := changes["fields"]; ok {
		return true
	}
	return false
}

// planNarrativeEventRelevant reports whether a lifecycle event on a
// narrative-set bead can change the rendered narrative. Updates must touch a
// content-bearing key; creates/closes always qualify (a doc bead appearing or
// a bundle closing reshapes the doc list).
func planNarrativeEventRelevant(topic string, changes map[string]any) bool {
	if agentEventKind(topic) != "updated" {
		return true
	}
	for _, k := range planNarrativeChangeKeys {
		if _, ok := changes[k]; ok {
			return true
		}
	}
	return false
}

// writePlanSSEFragment writes one rendered fragment as a single-line SSE
// message. No-op (returns false) when the fragment is empty — the render
// helpers return "" on error so the stream skips the frame rather than
// emitting a broken one.
func (s *Server) writePlanSSEFragment(w http.ResponseWriter, event, fragment string) bool {
	if fragment == "" {
		return false
	}
	fragment = strings.ReplaceAll(fragment, "\r", "")
	fragment = strings.ReplaceAll(fragment, "\n", " ")
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, fragment)
	return true
}

// renderPlanStepsFragment renders the plan_steps partial to a string for the SSE
// payload. Mirrors renderAgentsIndexRow's buffer-render idiom (bindAgentLink +
// ExecuteTemplate to a bytes.Buffer). Returns "" on any error so the caller skips
// the event rather than emitting a broken frame.
func (s *Server) renderPlanStepsFragment(ctx context.Context, r *http.Request, epicID string, isOperator bool) string {
	tmpl, ok := s.partials["templates/partials/plan_steps.html"]
	if !ok {
		s.logger.Error("plan events: plan_steps partial not found")
		return ""
	}
	data := s.planStepsData(ctx, epicID, isOperator, "")
	data["BasePath"] = s.basePath
	// Prewarm the agent-link resolver over the step assignees (flat + phased)
	// so the fragment's agentLink renders batch one daemon call, mirroring the
	// full-page path (ka-0000000899).
	if r != nil {
		steps, _ := data["Steps"].([]planStepView)
		phases, _ := data["Phases"].([]planPhaseView)
		r = s.prewarmAgentLinksForNames(r, planTimelineAssignees(steps, phases))
	}
	bound, err := s.bindAgentLink(tmpl, r)
	if err != nil {
		s.logger.Error("plan events: bind template", "error", err)
		return ""
	}
	var buf bytes.Buffer
	if err := bound.ExecuteTemplate(&buf, "plan_steps", data); err != nil {
		s.logger.Error("plan events: render plan_steps", "epic", epicID, "error", err)
		return ""
	}
	return buf.String()
}

// renderPlanEscalationsFragment renders the plan_escalations partial to a
// string for the SSE plan-escalations-updated payload (epic ka-0000005480,
// WS4). Recomputes the plan's open blocking escalations from live daemon state
// — the wrapper section always renders, so a swap after the last escalation
// closes clears the banner. Returns "" on any error so the caller skips the
// frame rather than emitting a broken one.
func (s *Server) renderPlanEscalationsFragment(ctx context.Context, epicID string) string {
	tmpl, ok := s.partials["templates/partials/plan_escalations.html"]
	if !ok {
		s.logger.Error("plan events: plan_escalations partial not found")
		return ""
	}
	steps, phases := s.planTimeline(ctx, epicID)
	escalations := s.planEscalationsBulk(ctx, map[string][]string{
		epicID: planEscalationWalkIDs(epicID, steps, phases),
	})[epicID]
	data := map[string]any{
		"PlanID":      epicID,
		"Escalations": escalations,
		"BasePath":    s.basePath,
	}
	// Clone (via bindAgentLink) before executing: the parsed partial is shared
	// across Server instances (loadTemplateAssets), and html/template forbids
	// Clone after a direct execute.
	bound, err := s.bindAgentLink(tmpl, nil)
	if err != nil {
		s.logger.Error("plan events: bind plan_escalations template", "error", err)
		return ""
	}
	var buf bytes.Buffer
	if err := bound.ExecuteTemplate(&buf, "plan_escalations", data); err != nil {
		s.logger.Error("plan events: render plan_escalations", "epic", epicID, "error", err)
		return ""
	}
	return buf.String()
}

// renderPlanCrewFragment renders the plan_crew partial to a string for the SSE
// plan-crew-updated payload (ka-0000000150). Recomputes the plan's crew roster
// from live daemon state over the freshly-fetched timeline — the wrapper
// section always renders, so a swap after a crew flips working→done, or a new
// crew is spawned, lands the change in place. Returns "" on any error so the
// caller skips the frame rather than emitting a broken one.
func (s *Server) renderPlanCrewFragment(ctx context.Context, r *http.Request, epicID, planStatus string, isOperator bool) string {
	tmpl, ok := s.partials["templates/partials/plan_crew.html"]
	if !ok {
		s.logger.Error("plan events: plan_crew partial not found")
		return ""
	}
	steps, phases := s.planTimeline(ctx, epicID)
	// epicID threaded so the SSE-swapped roster surfaces epic-anchored crew too
	// (delivery-drive-gardener pattern, ka-0000005352), matching first paint.
	crew := s.planCrew(ctx, epicID, steps, phases)
	// Quiet the LIVE-NOW crew signals on a TERMINAL plan (iteration 30,
	// ka-0000005352) so the SSE-swapped roster matches the first-paint cockpit +
	// index chip — a zombie "working" crew on a closed plan reads "done", no
	// pulse. planStatus is the connect-time epic status threaded from
	// handlePlanEvents (the epic is fetched once there); zero extra daemon call.
	crew = crew.reconcileTerminal(planStatus)
	// Prewarm agent-link resolvers over the crew names so any agentLink render
	// in the fragment resolves from cache, matching the full-page path.
	if r != nil {
		names := make([]string, 0, len(crew.Members))
		for _, m := range crew.Members {
			names = append(names, m.Name)
		}
		r = s.prewarmAgentLinksForNames(r, names)
	}
	bound, err := s.bindAgentLink(tmpl, r)
	if err != nil {
		s.logger.Error("plan events: bind plan_crew template", "error", err)
		return ""
	}
	data := map[string]any{
		"PlanID":     epicID,
		"Crew":       crew,
		"IsOperator": isOperator,
		"BasePath":   s.basePath,
	}
	var buf bytes.Buffer
	if err := bound.ExecuteTemplate(&buf, "plan_crew", data); err != nil {
		s.logger.Error("plan events: render plan_crew", "epic", epicID, "error", err)
		return ""
	}
	return buf.String()
}
