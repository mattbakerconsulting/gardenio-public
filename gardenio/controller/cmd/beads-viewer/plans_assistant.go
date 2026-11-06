package main

import (
	"context"
	"net/url"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planagent"
)

// plans_assistant.go — the viewer's thin adapter over internal/planagent, the
// SHARED plan-agent engine (resolve → decide → spawn/wake/nudge + the
// closed-plan backstop). The engine was lifted OUT of package main here (task
// ka-0000002739, epic ka-0000002170) so the controller-side reactive-collab
// reactor can call byte-identical logic through its OWN daemon + coopmux client.
// This file keeps the historical plans_assistant.go name and the planAssistant*
// symbol prefix so the rest of the viewer (and its tests) compile unchanged; the
// bodies now delegate to a per-call *planagent.Engine built from the server's
// daemon, coopmux, clock, and agent-name generator.
//
// UI-only concerns that did NOT move stay here: the /plans index bulk join
// (planAssistantBulk / planAssistantIndexCap) and the "Open →" affordance URL
// (assistantLinkURL), which depend on the viewer's coopmux terminal URL + base
// path. The per-plan debounce and the SSE step-added subscription also stay with
// their callers (the debounce is now DURABLE — its last-nudge timestamp lives on
// the plan epic, fields.last_nudge_at, so it survives a restart and coordinates
// concurrent writers, ka-0000002855; the reactor owns its own stream).

// Re-exported engine identifiers so callers across the viewer keep their
// existing spellings. planGardenerRole is the unified, first-class plan-agent
// role (epic ka-0000000081): one fable+runsc-pinned agent that is BOTH the
// plan's watchdog gardener and its interactive agent.
const (
	planGardenerRole      = planagent.SpawnRole
	planGardenerRoleLabel = planagent.RoleLabel
	// planIDField binds an agent bead to its plan epic — the reverse-lookup key
	// (plan -> agent), mirroring a thread agent's slack_thread_ts.
	planIDField = planagent.PlanIDField
)

// errPlanClosedNoWake is the closed-plan backstop (ka-0000000753): spawnPlanAssistant
// returns it instead of spawning/waking a gardener for a CLOSED plan, and
// promptPlanAssistant maps it to a graceful no-op delivery. Re-exported from the
// shared engine so callers (plans_assistant_prompt.go) keep matching on it.
var errPlanClosedNoWake = planagent.ErrPlanClosedNoWake

// planEngine builds the shared plan-agent engine over this server's daemon +
// coopmux client, clock, and agent-name generator. Cheap to construct (interface
// refs + funcs), so it is built per call rather than cached — which also keeps
// it honest against a test that swaps s.now via SetNow between calls.
func (s *Server) planEngine() *planagent.Engine {
	return planagent.NewEngine(planagent.Config{
		Daemon:  s.daemon,
		Nudge:   s.coopmux,
		NameGen: generateAgentName,
		Logger:  s.logger,
		Now:     s.now,
	})
}

// planAssistant locates the plan epic's bound agent bead, if any (or nil).
// Best-effort: a daemon error degrades to "no agent". Delegates to the shared
// engine's Resolve.
func (s *Server) planAssistant(ctx context.Context, epicID string) *beadsapi.BeadDetail {
	return s.planEngine().Resolve(ctx, epicID)
}

// planAssistantIndexCap bounds the plan-agent beads pulled for the /plans index
// bulk join (S4, ka-0000007888). Plans are a human-scale, curated surface, so at
// any realistic load the set of live + parked plan agents is well under this
// cap. If it is ever exceeded, the oldest-updated agents past the cap simply
// render "none" on their card — a graceful degradation, logged so it is visible
// rather than silent.
const planAssistantIndexCap = 400

// planAssistantBulk resolves the bound agent for every plan on the /plans index
// in a BOUNDED number of round-trips (S4), instead of the per-plan
// planAssistant() 3-call shape × N. It issues ONE role-scoped list query for
// plan-gardener agent beads (newest-updated first) and joins them to their epics
// in Go on fields.plan_id / fields.task_id. When two agent beads bind to the
// same plan (a respawn briefly leaves a stale terminal bead beside the new one),
// the one preferred by the engine's PickBetter wins — live over parked, then
// freshest — matching planAssistant()'s single-plan choice so the index chip and
// the detail lane agree.
//
// Caveat (documented degradation): the list is capped at planAssistantIndexCap;
// a plan whose agent sorts past the cap renders "none". Best-effort — a daemon
// error yields an empty map and every card renders "none".
//
// This stays in the viewer (not the shared engine) because it is an index-page
// rendering optimization, not part of the resolve/spawn/wake core.
func (s *Server) planAssistantBulk(ctx context.Context, epicIDs []string) map[string]*beadsapi.BeadDetail {
	out := make(map[string]*beadsapi.BeadDetail, len(epicIDs))
	if len(epicIDs) == 0 {
		return out
	}
	want := make(map[string]struct{}, len(epicIDs))
	for _, id := range epicIDs {
		want[id] = struct{}{}
	}
	// The list is page-INVARIANT (a fixed role-scoped, newest-first window joined
	// to the page's epics in Go), non-scoped, and the dominant measured leg of the
	// enrich pass (~4.0 MB / ~730 ms in prod). Memoize the raw result for a short
	// TTL so an index burst folds into one upstream fetch (ka-0000003113). Cache
	// key is a constant for this fixed query shape.
	res, err := s.listBeadsFilteredCached(ctx, planEnrichListKeyAssistants, beadsapi.ListBeadsQuery{
		Types:  []string{"agent"},
		Labels: []string{planGardenerRoleLabel},
		Sort:   "-updated_at",
		Limit:  planAssistantIndexCap,
	})
	if err != nil {
		s.logger.Debug("plans index: bulk assistant list", "error", err)
		return out
	}
	if res != nil && res.Total > len(res.Beads) {
		s.logger.Warn("plans index: plan-assistant set exceeds bulk cap; some cards may show no assistant",
			"cap", planAssistantIndexCap, "total", res.Total)
	}
	engine := s.planEngine()
	for _, b := range res.Beads {
		if b == nil || b.Type != "agent" {
			continue
		}
		// Bind to the plan the agent names (plan_id preferred, task_id as the
		// fallback the assigned-edge spawn also stamps). Skip agents bound to
		// plans not on this page.
		epicID := b.Fields[planIDField]
		if epicID == "" {
			epicID = b.Fields["task_id"]
		}
		if epicID == "" {
			continue
		}
		if _, ok := want[epicID]; !ok {
			continue
		}
		if !isPlanAssistantBead(b, epicID) {
			continue
		}
		out[epicID] = engine.PickBetter(out[epicID], b)
	}
	return out
}

// isPlanAssistantBead reports whether an agent bead is THIS plan's bound agent
// (role:plan-gardener label AND bound to the epic via fields.plan_id/task_id).
// Re-exported from the shared engine.
func isPlanAssistantBead(b *beadsapi.BeadDetail, epicID string) bool {
	return planagent.IsPlanAssistantBead(b, epicID)
}

// assistantIsLive mirrors the /agents liveness predicate: agent_state in the
// alive bucket AND recently updated (or no timestamp yet). Delegates to the
// shared engine (which reads the same alive set + stale threshold).
func (s *Server) assistantIsLive(b *beadsapi.BeadDetail) bool {
	return s.planEngine().IsLive(b)
}

// assistantIsQueued reports whether the plan's bound agent was WOKEN but its
// spawn DECLINED because the project account pool is fully exhausted — the
// controller stamps agent_state=queued on that path (SpawnBlocked) and files no
// escalation, so the reconciler silently re-attempts. This is a distinct,
// non-live state that must NOT collapse to the idle "parked" chip
// (ka-0000003525). Re-exported from the shared engine.
func assistantIsQueued(b *beadsapi.BeadDetail) bool {
	return planagent.IsQueued(b)
}

// assistantLinkURL is the single source of truth for where a plan's
// "assistant live / parked / Open →" affordance points. It prefers the public
// coopmux terminal page for the assistant's pod (the ask in ka-0000007019) and
// falls back to the assistant's own bead page (/{id}) when that URL can't be
// derived. The bead page itself embeds the same live coopmux terminal + input
// surface, so the fallback is still useful, not a dead end. Returns "" for a nil
// assistant so callers can gate the affordance. The bool reports whether the URL
// is the external coopmux page (so the template can open it in a new tab).
//
// This stays in the viewer: it depends on the coopmux terminal URL + base path,
// which are viewer render concerns, not part of the shared spawn/wake engine.
func (s *Server) assistantLinkURL(b *beadsapi.BeadDetail) (string, bool) {
	if b == nil {
		return "", false
	}
	if u := s.coopmuxTerminalURL(b); u != "" {
		return u, true
	}
	return s.basePath + "/" + url.PathEscape(b.ID), false
}

// spawnPlanAssistant creates (first contact) or WAKES IN PLACE (prior agent
// exists) the plan's agent, bound to the epic by fields.plan_id + an `assigned`
// edge, with `message` as its spawn/wake prompt. Returns (beadID, agentName,
// error); a CLOSED plan returns errPlanClosedNoWake and writes nothing. Delegates
// to the shared engine's Spawn (which itself does the revive-in-place wake when a
// prior agent exists — the former wakePlanAssistant wrapper moved into the
// engine's Wake, so the viewer no longer needs its own).
func (s *Server) spawnPlanAssistant(ctx context.Context, actor string, epic *beadsapi.BeadDetail, message string) (string, string, error) {
	return s.planEngine().Spawn(ctx, actor, epic, message)
}
