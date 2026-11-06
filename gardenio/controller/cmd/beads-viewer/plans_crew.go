package main

// plans_crew.go — the plan's CREW roster (ka-0000000150): every crew agent the
// plan gardener spawned onto a step, with its working/done/failed state.
//
// A crew agent bead carries an `assigned` edge to its step (written by
// SpawnAgent, client.go) and fields.task_id == that step id. Cost is bounded:
// one reverse `assigned`-edge batch over the step ids plus one hydration batch
// over the distinct crew ids, no per-step fan-out.

import (
	"context"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planagent"
)

// planCrewMember is one crew agent the plan gardener spawned onto a step,
// projected for the roster card (lighter than agentRowView: no usage meter, no
// MR resolver).
type planCrewMember struct {
	// ID is the crew agent bead id; Name is its agent handle.
	ID   string
	Name string
	// Role is the crew role (crew / reviewer / …), rendered as a chip.
	Role string
	// AgentState is the raw agent_state (working / done / failed / …); Bucket is
	// the alive/stuck/idle fleet classification the roster groups + tints by.
	AgentState string
	Bucket     string
	// Live is true while the agent is in an alive state and not closed (drives
	// the pulsing liveness dot).
	Live bool
	// StepID / StepTitle name the plan step this crew is working (its task_id).
	// StepNumber is the step's stable 1..N timeline number when resolvable, else
	// 0 (rendered only when > 0).
	StepID     string
	StepTitle  string
	StepNumber int
	// Outcome summarizes a finished run: the wrapup tldr/accomplishments, or a
	// derived "closed N beads / opened MR" line. Empty for a still-running crew
	// (its live state is the status). Only set on closed agents.
	Outcome string
	// Failed marks a crew whose run ended in a blocker/failed state (tinted red).
	Failed bool
	// URL is the crew's coopmux terminal page when derivable, else its bead
	// page; External opens it in a new tab. Ago is the humanized last-activity
	// stamp.
	URL      string
	External bool
	Ago      string
	AgoRFC   string
	// createdAt / updatedAt order the roster (live first, then newest activity);
	// not rendered directly.
	createdAt time.Time
	updatedAt time.Time
}

// planCrewInfo is the derived per-plan crew roster: the members plus rolled-up
// counts for the header ("3 working · 5 done"). Total is the number of DISTINCT
// crew agents surfaced (== len(Members) before any display cap).
type planCrewInfo struct {
	Members []planCrewMember
	Total   int
	Working int
	// Starting is the count of LIVE crew still BOOTING (spawning agent_state), a
	// subset of Working. Cockpit twin of planCrewRollup.Starting (ka-0000005352);
	// both derive from classifyCrewBead, so the two surfaces agree.
	Starting int
	// Idle is the count of OPEN crew dispatched but not yet working (idle/queued
	// agent_state, pod capacity-deferred). Why separate from Done: queued crew
	// used to read "N done" before anything started (ka-0000000177, iter-31).
	// Cockpit twin of planCrewRollup.Idle.
	Idle   int
	Done   int
	Failed int
	// queuedOldestAge / queuedOldestStale carry the WAIT-AGE of the oldest OPEN
	// queued crew (ka-0000005352 iter-32); cockpit twin of
	// planCrewRollup.queuedOldestAge. Read via QueuedOldestAge()/QueuedOldestStale().
	queuedOldestAge   string
	queuedOldestStale bool
	// workingOldestAge carries the elapsed RUNTIME (now - CreatedAt) of the
	// longest-running LIVE crew (ka-0000005352); cockpit twin of
	// planCrewRollup.workingOldestAge. Why CreatedAt: a working crew heartbeats,
	// so its updated_at is always ~0s old. No stale flag: a wedged crew surfaces
	// via the stuck re-bucket. Empty when no working crew is datable.
	workingOldestAge string
	// openStuck is the count of crew stuck AND still OPEN (rate-limited /
	// failing / budget-exceeded); cockpit twin of planCrewRollup.stuckOpen
	// (ka-0000005352 iter-33). Always ≤ Failed, which also counts CLOSED-failed
	// crew. Splits Failed into loud OpenStuck and calm ClosedFailed, as iter-29
	// (ka-0000003213) did for the index card.
	openStuck int
	// spendUSD is the roster's cumulative crew spend (ka-0000004218); cockpit
	// twin of planCrewRollup.spendUSD. Read via HasSpend()/SpendLabel().
	spendUSD float64
	// newestActivityAt is the newest updated_at across the crew beads
	// (ka-0000004310, iter-24); cockpit twin of planCrewRollup.newestActivityAt,
	// folded into the header's planLastActivity stamp. Monotonic via laterTime.
	newestActivityAt time.Time
}

// HasLive reports whether any crew is working the plan right now; cockpit twin
// of planCrewRollup.HasLive. The detail page's stall reconcile reads it
// (ka-0000005352 iteration 19).
func (i planCrewInfo) HasLive() bool { return i.Working > 0 }

// HasStarting gates the "(K starting)" qualifier on the "· M working" facet:
// true only when 0 < Starting < Working (if all are booting the bare count
// already says so). Cockpit twin of planCrewRollup.HasStarting.
func (i planCrewInfo) HasStarting() bool { return i.Starting > 0 && i.Starting < i.Working }

// HasQueued reports whether the roster has crew DISPATCHED but not yet working
// (Idle > 0); cockpit twin of planCrewRollup.HasQueued (ka-0000005352
// iteration 51). Feeds reconcileStallWithCrew's queued arm.
func (i planCrewInfo) HasQueued() bool { return i.Idle > 0 }

// QueuedOldestAge is the humanized wait-age of the OLDEST OPEN queued crew;
// cockpit twin of planCrewRollup.QueuedOldestAge (ka-0000005352 iter-32). Empty
// when no queued crew is datable.
func (i planCrewInfo) QueuedOldestAge() string { return i.queuedOldestAge }

// QueuedOldestStale reports whether the oldest OPEN queued crew has waited past
// planCrewQueuedStaleThreshold — the cockpit-side twin of
// planCrewRollup.QueuedOldestStale.
func (i planCrewInfo) QueuedOldestStale() bool { return i.queuedOldestStale }

// WorkingOldestAge is the humanized elapsed RUNTIME (now - CreatedAt) of the
// longest-running LIVE crew; cockpit twin of planCrewRollup.WorkingOldestAge
// (ka-0000005352). Empty when no working crew is datable or after
// reconcileTerminal.
func (i planCrewInfo) WorkingOldestAge() string { return i.workingOldestAge }

// reconcileTerminal quiets the LIVE-NOW crew signals on a TERMINAL (closed /
// deferred) plan; cockpit twin of planCrewRollup.reconcileTerminal
// (ka-0000005352 iter-30). Working and Idle fold into Done, Starting and
// workingOldestAge clear, and live/queued members are re-bucketed "done" with
// Live=false. Why: a zombie crew (stale alive agent_state) kept pulsing on a
// closed plan. Total, spend, Failed and each member's raw AgentState are left
// as the historical record. Returns i unchanged when non-terminal or nothing
// is live/queued.
func (i planCrewInfo) reconcileTerminal(status string) planCrewInfo {
	if !planStatusTerminal(status) || (i.Working == 0 && i.Idle == 0) {
		return i
	}
	// Queued crew fold too (ka-0000000177, iter-31).
	i.Done += i.Working + i.Idle
	i.Working = 0
	i.Idle = 0
	// Starting is a subset of the Working just folded.
	i.Starting = 0
	// No live crew left to age.
	i.workingOldestAge = ""
	for idx := range i.Members {
		if i.Members[idx].Live {
			i.Members[idx].Live = false
			// A failed member is never Live, so this only touches the zombie.
			i.Members[idx].Bucket = "done"
		} else if i.Members[idx].Bucket == "idle" && !i.Members[idx].Failed {
			// A queued crew re-buckets too, matching the header tally.
			i.Members[idx].Bucket = "done"
		}
	}
	return i
}

// HasSpend reports whether the roster has any known crew spend (gates the "$N"
// chip); mirrors planCrewRollup.HasSpend.
func (i planCrewInfo) HasSpend() bool { return i.spendUSD > 0 }

// SpendLabel is the roster's cumulative crew spend formatted via fmtUSD (shared
// with the index chip and agent card). Empty when no spend is known.
func (i planCrewInfo) SpendLabel() string {
	if i.spendUSD <= 0 {
		return ""
	}
	return fmtUSD(i.spendUSD)
}

// HasDoneFacet reports whether a compact crew chip should surface a "· N done"
// facet; cockpit twin of planCrewRollup.HasDoneFacet (ka-0000005352 iter-64).
// True only for the mixed case 0 < Done < Total: when all are done the bare
// "N crew" already says so.
func (i planCrewInfo) HasDoneFacet() bool { return i.Done > 0 && i.Done < i.Total }

// OpenStuck is the count of crew stuck AND still OPEN: the loud "act now"
// subset of Failed; cockpit twin of planCrewRollup.OpenStuck (ka-0000005352
// iter-33). Always ≤ Failed.
func (i planCrewInfo) OpenStuck() int { return i.openStuck }

// HasOpenStuck gates the roster header's loud "⚠ K stuck" chip.
func (i planCrewInfo) HasOpenStuck() bool { return i.openStuck > 0 }

// ClosedFailed is the count of crew that failed and are now CLOSED (Failed -
// openStuck, never negative); rendered calm, like the index card's "✕ K"
// marker (iter-29).
func (i planCrewInfo) ClosedFailed() int {
	n := i.Failed - i.openStuck
	if n < 0 {
		return 0
	}
	return n
}

// HasClosedFailed gates the roster header's calm "✕ K failed" chip.
func (i planCrewInfo) HasClosedFailed() bool { return i.ClosedFailed() > 0 }

// planCrewMaxRender caps how many crew rows the roster renders (live first,
// then newest; the remainder is noted as "+N more").
const planCrewMaxRender = 24

// HasMore reports whether the roster was capped (Total > len(Members)).
func (i planCrewInfo) HasMore() bool { return i.Total > len(i.Members) }

// MoreCount is how many crew are elided past the render cap.
func (i planCrewInfo) MoreCount() int {
	n := i.Total - len(i.Members)
	if n < 0 {
		return 0
	}
	return n
}

// planCrew resolves the cockpit crew roster for one plan from its rendered
// steps/phases (so roster and timeline agree on which step a crew is on).
//
// Two batched daemon calls: one reverse BatchGetDependencies over every step id
// plus epicID (crew→step `assigned` edges point INTO the step), one
// BatchGetBeads over the distinct crew ids. A crew is deduped by agent id. The
// epic sweep (ka-0000005352) surfaces crew anchored directly to the epic (the
// delivery-drive-gardener pattern); the plan-gardener also sits on that edge
// and is dropped by isPlanGardenerAgent. epicID may be "" (steps only).
// Best-effort: any daemon error returns an empty roster, never a partial one.
func (s *Server) planCrew(ctx context.Context, epicID string, steps []planStepView, phases []planPhaseView) planCrewInfo {
	stepMeta := crewStepMeta(steps, phases)
	// The epic is NOT a stepMeta key, so an epic-anchored crew gets no step
	// number/title (ka-0000005352).
	walkIDs := make([]string, 0, len(stepMeta)+1)
	for id := range stepMeta {
		walkIDs = append(walkIDs, id)
	}
	if epicID != "" {
		walkIDs = append(walkIDs, epicID)
	}
	if len(walkIDs) == 0 {
		return planCrewInfo{}
	}

	depsByStep, err := s.daemon.BatchGetDependencies(ctx, walkIDs, "reverse")
	if err != nil {
		s.logger.Debug("plan crew: assigned-edge batch", "error", err)
		return planCrewInfo{}
	}

	// stepForAgent maps a crew agent id → the step (or epic) id it is assigned
	// to. Normally 1:1 (SpawnAgent's single_claim_task guard); if one agent has
	// edges to two steps the last one iterated wins, and walkIDs comes from map
	// iteration, so which one is unspecified.
	stepForAgent := make(map[string]string)
	agentIDs := make([]string, 0, len(walkIDs))
	seen := make(map[string]struct{})
	for _, sid := range walkIDs {
		for _, d := range depsByStep[sid] {
			if d.Type != "assigned" || d.BeadID == "" {
				continue
			}
			// d.BeadID is the crew agent; d.DependsOnID is the step/epic (== sid).
			if _, dup := seen[d.BeadID]; !dup {
				seen[d.BeadID] = struct{}{}
				agentIDs = append(agentIDs, d.BeadID)
			}
			stepForAgent[d.BeadID] = sid
		}
	}
	if len(agentIDs) == 0 {
		return planCrewInfo{}
	}

	agents, err := s.daemon.BatchGetBeads(ctx, agentIDs)
	if err != nil {
		s.logger.Debug("plan crew: agent hydration batch", "error", err)
		return planCrewInfo{}
	}

	now := s.now()
	info := planCrewInfo{Members: make([]planCrewMember, 0, len(agentIDs))}
	// queued collects the OPEN queued crew beads for queuedOldestAge
	// (ka-0000005352 iter-32).
	var queued []*beadsapi.BeadDetail
	// working collects the LIVE crew beads for workingOldestAge (ka-0000005352).
	var working []*beadsapi.BeadDetail
	for _, aid := range agentIDs {
		b := agents[aid]
		if b == nil || b.Type != "agent" {
			continue // hydration miss or a non-agent target on a stray edge
		}
		if isPlanGardenerAgent(b) {
			// The epic's reverse edges include the plan-gardener; it is not crew
			// (ka-0000005352).
			continue
		}
		m := s.planCrewMemberFrom(b, stepForAgent[aid], stepMeta, now)
		info.Members = append(info.Members, m)
		info.Total++
		// ka-0000004310, iter-24.
		info.newestActivityAt = laterTime(info.newestActivityAt, b.UpdatedAt)
		// Same agentSpendUSD + live semantics as the index rollup and agent card.
		info.spendUSD += agentSpendUSD(b.Fields, m.Live)
		switch {
		case m.Failed:
			info.Failed++
			// Only an OPEN stuck crew has a reachable terminal; same split as
			// planCrewBulk's stuckOpen (ka-0000005352 iter-33).
			if b.Status != "closed" {
				info.openStuck++
			}
		case m.Bucket == "alive":
			info.Working++
			// Same candidate set the index rollup ages (ka-0000005352).
			working = append(working, b)
			if b.Fields["agent_state"] == agentStateSpawning {
				// Pod still BOOTING; a subset of Working (ka-0000005352).
				info.Starting++
			}
		case b.Status != "closed" && m.Bucket == "idle":
			// OPEN but not yet working (pod capacity-deferred); not Done
			// (ka-0000000177, iter-31).
			info.Idle++
			queued = append(queued, b)
		default:
			// A CLOSED crew that finished cleanly.
			info.Done++
		}
	}
	if info.Total == 0 {
		return planCrewInfo{}
	}
	// Same clock + helpers as the index rollup (ka-0000005352 iter-32).
	info.queuedOldestAge, info.queuedOldestStale = oldestOpenCrewAge(queued, now, planCrewQueuedStaleThreshold)
	// Aged on CreatedAt (runtime), ka-0000005352.
	info.workingOldestAge = oldestCrewRuntime(working, now)

	sortCrew(info.Members)
	if len(info.Members) > planCrewMaxRender {
		info.Members = info.Members[:planCrewMaxRender]
	}
	return info
}

// classifyCrewBead is the tally-only crew classification shared by the cockpit
// roster (planCrewMemberFrom) and the index rollup (planCrewBulk), so both
// surfaces count a crew identically. Returns the agent-state bucket, failed (a
// closed run with wrapup outcome "fail", any stuck bucket, or a stale-heartbeat
// open crew) and live (an alive agent_state on an open bead with a fresh
// heartbeat).
//
// Freshness gate (ka-0000005352 iter-42): an OPEN crew is stale when
// isAgentStale says so (updated_at older than agentStaleThreshold); it is then
// re-bucketed "stuck" and marked failed. Why: a crashed crew keeps its alive
// agent_state until reaped (audit ka-0000007486), and the stall badge already
// freshness-gates via isAgentStale, so the two must agree (advice
// ka-0000005924). A zero updated_at is never stale.
func classifyCrewBead(b *beadsapi.BeadDetail, now time.Time) (bucket string, failed, live bool) {
	state := b.Fields["agent_state"]
	bucket = agentStateBucket(state)
	open := b.Status != "closed"
	aliveState := isAgentStateIn(state, agentStateAlive)
	// Same predicate assistantIsLive/Engine.IsLive gate on.
	staleHeartbeat := open && isAgentStale(b, now)
	if staleHeartbeat {
		bucket = "stuck"
	}
	live = open && aliveState && !staleHeartbeat
	if b.Status == "closed" {
		// A wrapup outcome "fail", or a stuck bucket.
		if agentRunOutcome(b.Fields["wrapup"]) == "fail" || bucket == "stuck" {
			failed = true
		}
	} else if bucket == "stuck" {
		// Open but stuck (rate-limited / failed state, or stale heartbeat).
		failed = true
	}
	return bucket, failed, live
}

// isPlanGardenerAgent reports whether a crew-candidate agent bead is actually
// the plan-GARDENER (ka-0000005352), which is also `assigned` to the epic the
// crew walk sweeps. Matches either the planGardenerRoleLabel label or the raw
// `role` field == planagent.SpawnRole, so a bead carrying only one is still
// recognized. Nil is false.
func isPlanGardenerAgent(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	return hasLabel(b, planGardenerRoleLabel) ||
		strings.TrimSpace(b.Fields["role"]) == planagent.SpawnRole
}

// planCrewMemberFrom projects one crew agent bead into a roster row: live
// state, step context, terminal/bead link and (for a closed run) a one-line
// outcome. Mirrors agentRowFromBeadCtx's field reads.
func (s *Server) planCrewMemberFrom(b *beadsapi.BeadDetail, stepID string, stepMeta map[string]crewStep, now time.Time) planCrewMember {
	name := b.Fields["agent"]
	if name == "" {
		name = b.Title
	}
	bucket, failed, live := classifyCrewBead(b, now)

	url, external := s.assistantLinkURL(b)

	m := planCrewMember{
		ID:         b.ID,
		Name:       name,
		Role:       strings.TrimSpace(b.Fields["role"]),
		AgentState: b.Fields["agent_state"],
		Bucket:     bucket,
		Live:       live,
		Failed:     failed,
		StepID:     stepID,
		URL:        url,
		External:   external,
		Ago:        fmtTimeAgo(b.UpdatedAt, now),
		AgoRFC:     rfcTime(b.UpdatedAt),
		createdAt:  b.CreatedAt,
		updatedAt:  b.UpdatedAt,
	}
	if meta, ok := stepMeta[stepID]; ok {
		m.StepTitle = meta.Title
		m.StepNumber = meta.Number
	}
	// Closed crew: outcome comes purely from the wrapup blob. Why not
	// ResolveWrapupMRs: it costs one daemon call per closed crew.
	if b.Status == "closed" {
		wrapup := b.Fields["wrapup"]
		if sum := parseWrapUpSummary(wrapup); sum != "" {
			m.Outcome = sum
		} else {
			m.Outcome = derivedRunOutcome(0, wrapUpBeadsClosedCount(wrapup), agentRunOutcome(wrapup))
		}
	}
	return m
}

// planCrewFace is one crew agent surfaced as a click-through avatar on the
// index card (ka-0000002189): Initials as a pill linking to URL (coopmux
// terminal when derivable, else bead page), Name as the hover title. Projected
// in planCrewBulk from the already-hydrated agent beads.
type planCrewFace struct {
	ID       string
	Name     string
	Initials string
	URL      string
	External bool
	// Stuck marks a face projected from an OPEN stuck crew (ka-0000003793);
	// the template tints it red (plan-card-crew-face-stuck).
	Stuck bool
	// StepNumber / StepTitle name the plan STEP this crew is working
	// (ka-0000004037, iter-15), rendered as a "#N" tag plus hover title.
	// StepNumber uses the card's mini task-grid numbering (gridStepMeta), so a
	// "#3" tag matches the "#3" square. 0 / "" when the step is unresolvable.
	StepNumber int
	StepTitle  string
}

// planCrewStuckStaleThreshold is the wait-age past which the crew chip's stuck
// facet tints loud (ka-0000002016, iter-13). Why agentStaleThreshold: a
// stuck-but-open crew is a live agent that stopped progressing, the same thing
// /agents flags as frozen. Same two-tier idea as planStallStaleThreshold and
// planEscalationStaleThreshold.
const planCrewStuckStaleThreshold = agentStaleThreshold

// planCrewQueuedStaleThreshold is the wait past which the crew chip's queued
// facet tints loud (ka-0000005352 iter-32). Queuing is a normal,
// self-recovering wait, but a crew still waiting after agentStaleThreshold
// signals a starved pool.
const planCrewQueuedStaleThreshold = agentStaleThreshold

// planCrewFacesMax caps how many crew avatars a card strip renders inline; the
// count chip plus the "+N" overflow cover the rest.
const planCrewFacesMax = 4

// planCrewRollup is the compact per-plan crew tally on the /plans index card
// (ka-0000004801): counts plus capped avatar strips (ka-0000002189), no full
// roster or outcome text. Classified by the same classifyCrewBead as
// planCrewInfo, so the index chip and cockpit roster agree.
type planCrewRollup struct {
	Total   int
	Working int
	// Starting is the count of LIVE crew still in the "spawning" agent_state
	// (pod BOOTING; ka-0000005352). A SUBSET of Working (spawning ∈
	// agentStateAlive), not a separate disposition, so HasLive, faces and the
	// stall exemption are unaffected; it only feeds the "(K starting)"
	// qualifier (see HasStarting). Index twin of planCrewInfo.Starting.
	Starting int
	// Idle is the count of OPEN crew dispatched but not yet working (idle/queued
	// agent_state, pod capacity-deferred, ka-0000003525), rendered as "· N
	// queued". Why not Done: queued crew read as finished (ka-0000000177,
	// iter-31). Index twin of planCrewInfo.Idle.
	Idle   int
	Done   int
	Failed int
	// Faces is the capped strip of LIVE crew avatars (live-first, newest-first),
	// each a click-through to the agent's terminal/bead page. len(Faces) ≤
	// min(Working, planCrewFacesMax); a plan with no live crew carries none.
	Faces []planCrewFace
	// StuckFaces is the capped strip of OPEN STUCK crew avatars (ka-0000003793),
	// rendered red. Closed-failed crew get no avatar (no reachable terminal).
	// Newest-first, capped at planCrewFacesMax.
	StuckFaces []planCrewFace
	// QueuedFaces is the capped strip of OPEN QUEUED crew avatars (ka-0000005352
	// iter-53), rendered calm. Newest-first, capped at planCrewFacesMax, from the
	// same queued set the wait-age uses.
	QueuedFaces []planCrewFace
	// stuckOpen is the number of OPEN stuck crew before the render cap; always
	// ≤ Failed. Feeds StuckFacesHidden's "+N" overflow.
	stuckOpen int
	// stuckOldestAge / stuckOldestStale carry the last-activity WAIT-AGE of the
	// oldest OPEN stuck crew (ka-0000002016, iter-13), shown as "· ⚠ 2 · 6h" and
	// tinted loud past planCrewStuckStaleThreshold. Read via
	// StuckOldestAge()/StuckOldestStale().
	stuckOldestAge   string
	stuckOldestStale bool
	// queuedOldestAge / queuedOldestStale carry the WAIT-AGE of the oldest OPEN
	// queued crew (ka-0000005352 iter-32), loud past
	// planCrewQueuedStaleThreshold. Why updated_at is a valid basis: it freezes
	// at queue-entry, since UpdateBeadFields skips an unchanged "queued" value.
	// Empty when no queued crew is datable.
	queuedOldestAge   string
	queuedOldestStale bool
	// spendUSD is the plan's cumulative crew SPEND (ka-0000004218, iter-12),
	// summed via agentSpendUSD (reconciled total_cost_usd plus the live estimate
	// only while alive, so a closed crew is not double-counted).
	spendUSD float64
	// newestActivityAt is the newest updated_at across the plan's crew beads
	// (ka-0000004310, iter-24), folded into the row's lastActivity. Why: crew
	// heartbeat continuously while their step beads stay still, so steps alone
	// made a live plan read "updated 1 month ago". Monotonic via laterTime; zero
	// when the plan has no crew.
	newestActivityAt time.Time
	// workingOldestAge carries the elapsed RUNTIME (now - CreatedAt) of the
	// longest-running LIVE crew (ka-0000005352), the "· 4h" on "· M working".
	// Why CreatedAt, unlike the queued/stuck ages: a working crew heartbeats, so
	// now - updatedAt is always ~0s. Calm-only (no stale tint); a wedged crew is
	// already re-bucketed stuck. Empty when no working crew is datable.
	workingOldestAge string
}

// Any reports whether the plan has any crew at all (gates the crew chip).
func (r planCrewRollup) Any() bool { return r.Total > 0 }

// HasLive reports whether any crew is working right now (the green live-dot).
func (r planCrewRollup) HasLive() bool { return r.Working > 0 }

// HasStarting gates the calm "(K starting)" qualifier on the "· M working"
// facet (ka-0000005352): true only when 0 < Starting < Working (if all are
// booting the bare count already says so).
func (r planCrewRollup) HasStarting() bool { return r.Starting > 0 && r.Starting < r.Working }

// HasQueued reports whether the plan has any crew DISPATCHED but not yet
// working (Idle > 0, the "· N queued" facet, iter-31). Why it matters:
// reconcileStallWithCrew (iter-51) clears a gardener-state ⚠ stall when crew
// are queued, since the gardener evidently acted.
func (r planCrewRollup) HasQueued() bool { return r.Idle > 0 }

// HasDoneFacet reports whether the index crew chip should surface a "· N done"
// facet (ka-0000005352 iter-64). True only for the mixed case 0 < Done < Total:
// when all are done the bare "N crew" already says so (iter-60..63 redundant-ink
// rule), which also keeps it off terminal plans after reconcileTerminal.
func (r planCrewRollup) HasDoneFacet() bool { return r.Done > 0 && r.Done < r.Total }

// reconcileTerminal quiets the LIVE-NOW crew signals on a TERMINAL (closed /
// deferred) plan (ka-0000005352 iter-30). Display-only: Working and Idle fold
// into Done, and the live signals derived from them are cleared; Total, spend
// and Failed stay as history. Why: an unreaped zombie crew with an alive
// agent_state made closed plans pulse "working" (e.g. ka-0000001281). Returns r
// unchanged when non-terminal or nothing is live/queued (idempotent).
func (r planCrewRollup) reconcileTerminal(status string) planCrewRollup {
	if !planStatusTerminal(status) || (r.Working == 0 && r.Idle == 0) {
		return r
	}
	// Queued crew fold too (ka-0000000177, iter-31).
	r.Done += r.Working + r.Idle
	r.Working = 0
	r.Idle = 0
	// Starting is a subset of the Working just folded.
	r.Starting = 0
	r.Faces = nil
	// No live crew left to age (ka-0000005352).
	r.workingOldestAge = ""
	// Queued faces go too (ka-0000005352 iter-53). StuckFaces stay: they are
	// open crew with a reachable terminal.
	r.QueuedFaces = nil
	return r
}

// HasSpend reports whether the plan's crew has any known dollar spend (gates
// the "· $N" facet, ka-0000004218).
func (r planCrewRollup) HasSpend() bool { return r.spendUSD > 0 }

// SpendLabel is the plan's cumulative crew spend via fmtUSD ("$12.34"). Empty
// when no spend is known.
func (r planCrewRollup) SpendLabel() string {
	if r.spendUSD <= 0 {
		return ""
	}
	return fmtUSD(r.spendUSD)
}

// FacesHidden is the number of working crew beyond the avatar cap (the "+N"
// overflow chip). Never negative.
func (r planCrewRollup) FacesHidden() int {
	n := r.Working - len(r.Faces)
	if n < 0 {
		return 0
	}
	return n
}

// HasStuck reports whether any OPEN stuck crew surfaces as a red avatar (gates
// the stuck-faces strip, ka-0000003793).
func (r planCrewRollup) HasStuck() bool { return len(r.StuckFaces) > 0 }

// StuckFacesHidden is the number of OPEN stuck crew beyond the avatar cap.
// Counted off stuckOpen, not Failed, since closed-failed crew never get an
// avatar. Never negative.
func (r planCrewRollup) StuckFacesHidden() int {
	n := r.stuckOpen - len(r.StuckFaces)
	if n < 0 {
		return 0
	}
	return n
}

// HasQueuedFaces reports whether any OPEN queued crew surfaces as an avatar
// (gates the queued-faces strip, ka-0000005352 iter-53); HasQueued gates the
// count facet.
func (r planCrewRollup) HasQueuedFaces() bool { return len(r.QueuedFaces) > 0 }

// QueuedFacesHidden is the number of OPEN queued crew beyond the avatar cap.
// Idle is exactly the queued-faces candidate set. Never negative.
func (r planCrewRollup) QueuedFacesHidden() int {
	n := r.Idle - len(r.QueuedFaces)
	if n < 0 {
		return 0
	}
	return n
}

// OpenStuck is the count of crew stuck AND still OPEN: the loud "act now"
// subset of Failed, the same set as the stuck avatars and StuckOldestAge.
// Always ≤ Failed. Why split from ClosedFailed (ka-0000005352 iter-29): a
// single red Failed count screamed "act now" for closed history.
func (r planCrewRollup) OpenStuck() int { return r.stuckOpen }

// HasOpenStuck gates the loud open-stuck facet (pre-cap count, so it renders
// together with the HasStuck avatar strip).
func (r planCrewRollup) HasOpenStuck() bool { return r.stuckOpen > 0 }

// ClosedFailed is the count of crew that failed and are now CLOSED (Failed -
// stuckOpen), rendered calm. Never negative.
func (r planCrewRollup) ClosedFailed() int {
	n := r.Failed - r.stuckOpen
	if n < 0 {
		return 0
	}
	return n
}

// HasClosedFailed gates the calm historical-failure facet.
func (r planCrewRollup) HasClosedFailed() bool { return r.ClosedFailed() > 0 }

// StuckOldestAge is the humanized last-activity age of the OLDEST OPEN stuck
// crew (ka-0000002016, iter-13), the "· 6h" on the stuck facet. Open-only: a
// closed crew's age is its death, not a wait. Empty when none is datable.
func (r planCrewRollup) StuckOldestAge() string { return r.stuckOldestAge }

// StuckOldestStale reports whether the oldest OPEN stuck crew has been wedged
// past planCrewStuckStaleThreshold (tints the facet loud). False when none is
// datable.
func (r planCrewRollup) StuckOldestStale() bool { return r.stuckOldestStale }

// QueuedOldestAge is the humanized wait-age of the OLDEST OPEN queued crew
// (ka-0000005352 iter-32), the "· Xh" on "· N queued". Empty when none is
// datable.
func (r planCrewRollup) QueuedOldestAge() string { return r.queuedOldestAge }

// QueuedOldestStale reports whether the oldest OPEN queued crew has waited past
// planCrewQueuedStaleThreshold (tints the facet loud). False when none is
// datable.
func (r planCrewRollup) QueuedOldestStale() bool { return r.queuedOldestStale }

// WorkingOldestAge is the humanized elapsed RUNTIME (now - CreatedAt) of the
// longest-running LIVE crew (ka-0000005352), the "· 4h" on "· M working".
// Empty when no working crew is datable.
func (r planCrewRollup) WorkingOldestAge() string { return r.workingOldestAge }

// planCrewBulk derives the compact crew tally for every plan on the /plans
// index: one reverse `assigned`-edge batch over the union of every plan's step
// ids and epic id, plus one hydration batch over the distinct crew ids, no
// per-row fan-out (like planEscalationsBulk / planStallBulk). stepIDsByPlan maps
// a plan epic id → its rendered step ids. A crew is deduped per plan.
// stepMeta maps a step id → its (title, number) in the card's mini task-grid
// numbering (gridStepMeta, planGridSquare) for the faces' "#N" tag
// (ka-0000004037, iter-15); nil or a missing entry gives an initials-only face.
// Best-effort: a daemon error returns an empty map (no crew chips).
func (s *Server) planCrewBulk(ctx context.Context, stepIDsByPlan map[string][]string, stepMeta map[string]crewStep) map[string]planCrewRollup {
	out := make(map[string]planCrewRollup, len(stepIDsByPlan))
	if len(stepIDsByPlan) == 0 {
		return out
	}
	// walkByPlan is each plan's candidate set: its step ids plus the epic id
	// (epic-anchored crew, ka-0000005352). Built without mutating the caller's
	// slices.
	walkByPlan := make(map[string][]string, len(stepIDsByPlan))
	// crew --assigned--> step|epic, so REVERSE deps surface the crew.
	union := make([]string, 0, len(stepIDsByPlan)*4)
	seen := make(map[string]struct{})
	addToUnion := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		union = append(union, id)
	}
	for planID, ids := range stepIDsByPlan {
		walk := make([]string, 0, len(ids)+1)
		for _, id := range ids {
			if id == "" {
				continue
			}
			walk = append(walk, id)
			addToUnion(id)
		}
		// The epic is not a stepMeta key, so epic-anchored crew read initials-only.
		if planID != "" {
			walk = append(walk, planID)
			addToUnion(planID)
		}
		walkByPlan[planID] = walk
	}
	if len(union) == 0 {
		return out
	}
	depsByStep, err := s.daemon.BatchGetDependencies(ctx, union, "reverse")
	if err != nil {
		s.logger.Debug("plan crew rollup: assigned-edge batch", "error", err)
		return out
	}
	// Distinct crew agent ids across the whole page → one hydration batch.
	agentIDs := make([]string, 0)
	agentSeen := make(map[string]struct{})
	for _, sid := range union {
		for _, d := range depsByStep[sid] {
			if d.Type != "assigned" || d.BeadID == "" {
				continue
			}
			if _, dup := agentSeen[d.BeadID]; dup {
				continue
			}
			agentSeen[d.BeadID] = struct{}{}
			agentIDs = append(agentIDs, d.BeadID)
		}
	}
	if len(agentIDs) == 0 {
		return out
	}
	agents, err := s.daemon.BatchGetBeads(ctx, agentIDs)
	if err != nil {
		s.logger.Debug("plan crew rollup: agent hydration batch", "error", err)
		return out
	}
	// One clock read for the freshness gate (iter-42) and every wait-age.
	now := s.now()
	for planID := range stepIDsByPlan {
		ids := walkByPlan[planID] // rendered steps + the epic id (ka-0000005352)
		var roll planCrewRollup
		crewSeen := make(map[string]struct{})
		// working / stuck (ka-0000003793) / queued (ka-0000005352 iter-32) collect
		// the LIVE, OPEN-stuck and OPEN-queued crew beads, projected to faces and
		// ages after the walk. stepForAgent records each crew's step id for the
		// face's step tag (ka-0000004037, iter-15).
		var working, stuck, queued []*beadsapi.BeadDetail
		stepForAgent := make(map[string]string)
		for _, sid := range ids {
			for _, d := range depsByStep[sid] {
				if d.Type != "assigned" || d.BeadID == "" {
					continue
				}
				b := agents[d.BeadID]
				if b == nil || b.Type != "agent" {
					continue // hydration miss or a non-agent target on a stray edge
				}
				if isPlanGardenerAgent(b) {
					// The epic edge also carries the plan-gardener (ka-0000005352).
					continue
				}
				if _, dup := crewSeen[b.ID]; dup {
					continue // one crew across two steps/edges counts once
				}
				crewSeen[b.ID] = struct{}{}
				stepForAgent[b.ID] = sid
				bucket, failed, live := classifyCrewBead(b, now)
				roll.Total++
				// ka-0000004310, iter-24.
				roll.newestActivityAt = laterTime(roll.newestActivityAt, b.UpdatedAt)
				// Same agentSpendUSD + live semantics as the agent-card cost chip.
				roll.spendUSD += agentSpendUSD(b.Fields, live)
				switch {
				case failed:
					roll.Failed++
					// Only an OPEN stuck crew has a reachable terminal for an avatar.
					if b.Status != "closed" {
						roll.stuckOpen++
						stuck = append(stuck, b)
					}
				case bucket == "alive":
					roll.Working++
					if b.Fields["agent_state"] == agentStateSpawning {
						// Pod still BOOTING; a subset of Working (ka-0000005352).
						roll.Starting++
					}
					working = append(working, b)
				case b.Status != "closed" && bucket == "idle":
					// OPEN but not yet working (pod capacity-deferred); not Done
					// (ka-0000000177, iter-31).
					roll.Idle++
					// For the queued wait-age (ka-0000005352 iter-32).
					queued = append(queued, b)
				default:
					// A CLOSED crew that finished cleanly.
					roll.Done++
				}
			}
		}
		if roll.Total == 0 {
			continue
		}
		roll.Faces = s.planCrewFaces(working, false, stepForAgent, stepMeta)
		roll.StuckFaces = s.planCrewFaces(stuck, true, stepForAgent, stepMeta)
		// Queued avatars (ka-0000005352 iter-53); Idle == len(queued).
		roll.QueuedFaces = s.planCrewFaces(queued, false, stepForAgent, stepMeta)
		// Ages are stamped here against the injected s.now() (ka-0000002016, iter-13).
		roll.stuckOldestAge, roll.stuckOldestStale = oldestOpenCrewAge(stuck, now, planCrewStuckStaleThreshold)
		// ka-0000005352 iter-32.
		roll.queuedOldestAge, roll.queuedOldestStale = oldestOpenCrewAge(queued, now, planCrewQueuedStaleThreshold)
		// Aged on CreatedAt (runtime), ka-0000005352.
		roll.workingOldestAge = oldestCrewRuntime(working, now)
		out[planID] = roll
	}
	return out
}

// oldestOpenCrewAge returns the humanized age (now - earliest UpdatedAt) of the
// oldest crew in the set and whether it is ≥ staleThreshold; "" and false when
// no crew is datable. Shared by the stuck (iter-13) and queued (ka-0000005352
// iter-32) facets; a stuck or queued crew's updated_at freezes, so it measures
// the wait.
func oldestOpenCrewAge(crew []*beadsapi.BeadDetail, now time.Time, staleThreshold time.Duration) (string, bool) {
	var oldest time.Time
	for _, b := range crew {
		if b.UpdatedAt.IsZero() {
			continue // an undatable crew cannot contribute an age
		}
		if oldest.IsZero() || b.UpdatedAt.Before(oldest) {
			oldest = b.UpdatedAt
		}
	}
	if oldest.IsZero() {
		return "", false
	}
	return fmtTimeAgo(oldest, now), now.Sub(oldest) >= staleThreshold
}

// oldestCrewRuntime returns the humanized now - earliest CreatedAt over the set,
// the working facet's "· 4h" (ka-0000005352). Why not oldestOpenCrewAge: a
// working crew heartbeats, so updated_at reads ~0s. No stale flag by design.
// Empty when no crew is datable.
func oldestCrewRuntime(crew []*beadsapi.BeadDetail, now time.Time) string {
	var oldest time.Time
	for _, b := range crew {
		if b.CreatedAt.IsZero() {
			continue // an undatable crew cannot contribute a runtime
		}
		if oldest.IsZero() || b.CreatedAt.Before(oldest) {
			oldest = b.CreatedAt
		}
	}
	if oldest.IsZero() {
		return ""
	}
	return fmtTimeAgo(oldest, now)
}

// planCrewFaces projects crew beads into the capped avatar strip
// (ka-0000002189): sorts crew IN PLACE newest-activity-first with an id
// tie-break (stable across SSE re-renders), keeps the first planCrewFacesMax,
// and builds each face via crewAvatarInitials and assistantLinkURL
// (daemon-free). stuck marks every face as an OPEN stuck crew (ka-0000003793).
// stepForAgent and stepMeta resolve each face's step (ka-0000004037, iter-15);
// a miss leaves StepNumber 0 / StepTitle "". Returns nil for an empty set.
func (s *Server) planCrewFaces(crew []*beadsapi.BeadDetail, stuck bool, stepForAgent map[string]string, stepMeta map[string]crewStep) []planCrewFace {
	if len(crew) == 0 {
		return nil
	}
	sort.SliceStable(crew, func(i, j int) bool {
		if !crew[i].UpdatedAt.Equal(crew[j].UpdatedAt) {
			return crew[i].UpdatedAt.After(crew[j].UpdatedAt)
		}
		return crew[i].ID < crew[j].ID
	})
	if len(crew) > planCrewFacesMax {
		crew = crew[:planCrewFacesMax]
	}
	faces := make([]planCrewFace, 0, len(crew))
	for _, b := range crew {
		name := b.Fields["agent"]
		if name == "" {
			name = b.Title
		}
		initials := crewAvatarInitials(name, projectFromLabels(b.Labels))
		if initials == "" {
			// Empty / pure-punctuation name: still render a clickable glyph.
			initials = "•"
		}
		url, external := s.assistantLinkURL(b)
		// A missing step leaves both zero-valued (initials-only face).
		var stepNum int
		var stepTitle string
		if meta, ok := stepMeta[stepForAgent[b.ID]]; ok {
			stepNum = meta.Number
			stepTitle = meta.Title
		}
		faces = append(faces, planCrewFace{
			ID:         b.ID,
			Name:       name,
			Initials:   initials,
			URL:        url,
			External:   external,
			Stuck:      stuck,
			StepNumber: stepNum,
			StepTitle:  stepTitle,
		})
	}
	return faces
}

// crewAvatarInitials returns actorInitials of name after
// stripLeadingProjectSegments (ka-0000005352 iter-35). Why: crew names start
// with their project, often doubled ("gardenio-gardenio-gardener-…"), which
// wasted the leading glyphs on a constant.
func crewAvatarInitials(name, project string) string {
	stripped := stripLeadingProjectSegments(name, project)
	return actorInitials(stripped)
}

// stripLeadingProjectSegments removes the leading run of hyphen-delimited
// segments equal to project from name (e.g. project="gardenio",
// "gardenio-gardenio-gardener-bugfix-8jyv" → "gardener-bugfix-8jyv"). Matches
// whole segments, not substrings. Returns name unchanged when project is empty,
// there is no such prefix, or stripping would leave nothing.
func stripLeadingProjectSegments(name, project string) string {
	if project == "" || name == "" {
		return name
	}
	parts := strings.Split(name, "-")
	i := 0
	for i < len(parts) && parts[i] == project {
		i++
	}
	if i == 0 || i >= len(parts) {
		// No prefix, or nothing would remain.
		return name
	}
	return strings.Join(parts[i:], "-")
}

// crewStep is the minimal per-step context a crew row needs: the step's title
// and stable timeline number.
type crewStep struct {
	Title  string
	Number int
}

// crewStepMeta flattens the plan's rendered steps (flat + every phase) into a
// step-id → (title, number) map: the crew walk's candidate set and each row's
// step label.
func crewStepMeta(steps []planStepView, phases []planPhaseView) map[string]crewStep {
	meta := make(map[string]crewStep, len(steps)+8)
	add := func(rows []planStepView) {
		for _, st := range rows {
			if st.ID == "" {
				continue
			}
			meta[st.ID] = crewStep{Title: st.Title, Number: st.Number}
		}
	}
	add(steps)
	for _, p := range phases {
		add(p.Steps)
	}
	return meta
}

// sortCrew orders the roster in place: live, then failed, then the rest, each
// group newest-activity-first with an id tie-break (deterministic across SSE
// re-renders).
func sortCrew(members []planCrewMember) {
	rank := func(m planCrewMember) int {
		switch {
		case m.Live:
			return 0
		case m.Failed:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(members, func(i, j int) bool {
		ri, rj := rank(members[i]), rank(members[j])
		if ri != rj {
			return ri < rj
		}
		if !members[i].updatedAt.Equal(members[j].updatedAt) {
			return members[i].updatedAt.After(members[j].updatedAt)
		}
		return members[i].ID < members[j].ID
	})
}
