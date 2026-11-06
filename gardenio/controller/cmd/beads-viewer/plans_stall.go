package main

// plans_stall.go — Step 4 of epic ka-0000002022 (task ka-0000005581): the
// stalled-plan warning signal. A plan is STALLED when its work can no longer
// make progress without someone noticing:
//
//   - a dead-agent step: a step in_progress for over an hour whose agent is
//     dead or absent (advice ka-0000007726) — the exact S5 shape that orphaned
//     plan ka-0000004457; or
//   - open rendered work while the plan's gardener is paused or unbound AND its
//     bound agent is not live — nothing is watching, so a blocked step would sit
//     silently (the ka-0000000136 parking footgun).
//
// A live agent working the plan RIGHT NOW exempts the gardener-paused/none
// condition: either the bound plan-assistant (the plan-gardener role is one
// lineage, Step 1 ka-0000002366, so a live iteration IS the bound agent working
// the plan) OR a live crew agent the gardener dispatched onto a step
// (ka-0000000734: a plan with active crew must not read "stalled" just because
// its gardener is paused). Flagging either would cry wolf on healthy work.
//
// The signal is derived in the same batched enrich seam as the plan-agent chip
// (planIndexEnrich): ONE gardeners list + ONE agent-liveness batch across the
// whole /plans page, joined in Go — no per-row fan-out. The detail page
// computes the identical predicate from its already-fetched timeline.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// planStallDeadAgentGrace is how long a step may sit in_progress with a
// dead/absent agent before it counts as orphaned. Mirrors the stale-claim
// convention (advice ka-0000007726: in_progress >1h with no live agent is up
// for grabs) and agentStaleThreshold, so "the /agents page would call this
// agent stale" and "the /plans page calls its step dead" agree.
const planStallDeadAgentGrace = time.Hour

// stallStepRef is the minimal projection of an in_progress step the stall
// detector needs: who claims to be working it, and when it last moved.
type stallStepRef struct {
	Assignee  string
	UpdatedAt time.Time
}

// planStallInfo is the derived stall signal for one plan.
type planStallInfo struct {
	Stalled bool
	// Reason is the human string the badge tooltip carries — which condition
	// fired, so the operator knows what to fix without clicking through.
	Reason string
	// GardenerState is "running" | "paused" | "none" — the plan's bound gardener
	// trigger's effective enabled state (buildTriggerRow semantics).
	GardenerState string
	// GardenerID is the bound gardener trigger bead id ("" when none).
	GardenerID string
	// DeadSteps counts in_progress steps with a dead/absent agent past grace.
	DeadSteps int
	// AgeLabel is the humanized wait-age of the OLDEST dead-agent orphaned step
	// ("3d"), folded onto the ⚠ badge FACE (ka-0000005352 iteration 11) so the
	// stall badge — the last attention-tier badge whose face carried no temporal
	// signal — finally reads its urgency like its siblings: the decision chip
	// (iter-4, "· 32d") and the escalation badge (iter-8, "· 40d"). It is the
	// instant the longest-frozen orphaned in_progress step last moved
	// (stallStepRef.UpdatedAt), so it answers "how long has this work sat frozen
	// with a dead agent?" — the question the grace-gated dead-step count alone
	// can't (every dead-agent stall is ≥1h old; days-frozen vs hours-frozen is
	// the actionable difference).
	//
	// It carries information the card's "updated Xh ago" meta actively HIDES:
	// that stamp is the NEWEST activity across the whole plan (planLastActivity),
	// so a plan with one live-churning step AND one dead-agent orphan reads
	// "updated 5m ago" (misleadingly fresh) while the orphan has been frozen for
	// days — this surfaces the orphan's true age. Set ONLY for a dead-agent stall
	// (DeadSteps ≥ 1) with a datable orphaned step; empty for the
	// gardener-paused/no-gardener conditions (where the "updated" meta beside the
	// badge already IS the "stalled since", so re-stating it would be redundant)
	// and for an undatable orphan (a step with a zero UpdatedAt — reads exactly as
	// before, reason only). Zero extra daemon call: it rides the SAME
	// stallStepRef.UpdatedAt the stall pass already collected.
	AgeLabel string
	// AgeStale is true when the oldest dead-agent orphaned step has sat frozen
	// past planStallStaleThreshold — the mark that tints the ⚠ badge loud so an
	// aged, work-blocking orphan pops out of the board instead of hiding behind a
	// bare reason. False for a fresh orphan, a non-dead-agent stall, or an
	// undatable orphan. Mirrors OpenDecisionStale / EscalationStale.
	AgeStale bool
	// Orphaned re-scopes the LOUD plan-level "⚠ stalled" verdict to a quieter
	// per-step "⚠ N orphaned" when a dead-agent stall COEXISTS with live crew
	// (ka-0000005352 iteration 23). iteration 19 (reconcileStallWithCrew) killed
	// the "N working + stalled" self-contradiction only for the
	// gardener-paused/no-gardener branch (DeadSteps == 0), monotonically CLEARING
	// the stall; it left the dead-agent branch loud on purpose — an orphaned step
	// under a dead agent is a real stall even while OTHER steps have live crew, so
	// the plan must stay flagged (attention lane + ranker). But "● 6 crew · 5
	// working" beside "⚠ stalled · 1 dead step" still reads as "working AND
	// stalled" — the exact self-contradiction the mission names. This is the other
	// half of iteration 19's own steer: when live crew coexist with the orphan,
	// keep Stalled true (do NOT suppress the signal — that would regress
	// iteration 10/11) but flip the badge FACE from the plan-level "⚠ stalled" to
	// the per-step "⚠ N orphaned", so the two facts read as coherent siblings
	// ("5 working · ⚠ 1 orphaned") rather than a contradiction. Set ONLY for a
	// dead-agent stall (DeadSteps ≥ 1) reconciled against live crew; a
	// genuinely-unwatched orphan (no live crew) keeps the loud "⚠ stalled" exactly
	// as before. The dead-step count, reason discriminator, and orphan wait-age
	// all still ride along under the re-scoped label.
	Orphaned bool
	// EscalationBlocked suppresses the ⚠ stalled BADGE on a card that is ALSO
	// ⛔ blocked by an open escalation, for the gardener-state stall only
	// (ka-0000005352 iteration 26). The two badges give CONTRADICTORY
	// remediation: the ⛔ escalation badge — the harder stop, which already
	// OUTRANKS stall in planAttention (escalation tier > stalled tier) — says "a
	// privileged operator must act", while "⚠ stalled · no gardener" says "bind a
	// gardener". Binding a gardener cannot clear an escalation, so a plan blocked
	// on a human is not "stalled for lack of a watcher". On the live board this
	// double-signal fired on 16 of 39 stalled-lane cards (41% of the attention
	// lane). Set by reconcileStallWithEscalation ONLY when Stalled AND the stall
	// is a gardener-state stall (DeadSteps == 0) AND an escalation blocks the
	// plan. It suppresses ONLY the badge FACE (ShowBadge): Stalled itself stays
	// true so the board lane (planBoardLaneKey already unions escalation+stall
	// into ONE lane) and the attention ranker are untouched — this is a pure
	// display de-dup, the exact iteration-19/23 "kill the self-contradictory
	// double-badge" move applied to the escalation signal. A dead-agent orphan
	// (DeadSteps ≥ 1) is a real system failure independent of the escalation, so
	// it keeps its loud badge; a stall with no escalation is unaffected.
	EscalationBlocked bool
	// PreBuildLifecycle suppresses the ⚠ stalled BADGE on a plan still in its
	// PRE-BUILD authoring lifecycle (draft/ready), for the gardener-state stall
	// only (ka-0000005352 iteration 49). It is the lifecycle twin of
	// EscalationBlocked: both drop a gardener-state stall FACE whose implied
	// remediation contradicts a louder signal ALREADY on the same card. Here the
	// contradiction is with the plan's own lifecycle: a draft/ready plan has NO
	// enabled gardener BY DESIGN — gardenerEnableGatedByState (the R6 build gate)
	// forbids enabling one until the operator clicks "Approve & build" — so
	// "⚠ stalled · no gardener" / "· gardener paused" cries wolf, telling the
	// operator to bind/resume a gardener that the lifecycle deliberately holds
	// back. The card already renders the READY/DRAFT lifecycle chip (and, for a
	// ready plan, the "Approve & build" button) saying exactly that: nothing
	// builds until you approve. On the live board (2026-08-11 pour) this
	// double-signal fired on 7 of 25 gardener-state stall cards (28%) — e.g.
	// ka-0000003709 rendered "ready · 23d" AND "⚠ stalled · no gardener" AND the
	// Approve&build button, three signals two of which contradict the third. Set
	// by reconcileStallWithLifecycle ONLY when Stalled AND the stall is a
	// gardener-state stall (DeadSteps == 0) AND the plan is build-gated
	// (gardenerEnableGatedByState). It suppresses ONLY the badge FACE (ShowBadge):
	// Stalled itself stays true so the board lane + attention ranker are
	// byte-identical (a ready plan already outranks stalled via planAttentionReady;
	// a draft plan keeps its stalled tier + lane slot) — a pure display de-dup, the
	// exact iteration-26 move applied to the lifecycle signal. A dead-agent orphan
	// (DeadSteps ≥ 1) is a real system failure independent of the lifecycle, so it
	// keeps its loud badge; a building/complete/legacy-unset plan is unaffected
	// (its gardener is not lifecycle-gated, so a missing/paused gardener IS a real
	// stall).
	PreBuildLifecycle bool
}

// ShowBadge reports whether the ⚠ stalled badge should render on the card
// (ka-0000005352 iteration 26 + iteration 49). It is Stalled EXCEPT when a
// gardener-state stall has been suppressed as redundant/contradictory by a
// louder signal already on the same card — either EscalationBlocked (the ⛔
// escalation badge owns the attention, iter-26) or PreBuildLifecycle (the plan
// is draft/ready and has no gardener BY DESIGN, so the READY/DRAFT lifecycle chip
// already explains it, iter-49). In both cases the contradictory "⚠ stalled ·
// no gardener" / "· gardener paused" face is dropped. Stalled stays the source of
// truth for the board lane + attention ranker (both untouched); only the badge
// render gates on this, so a suppressed stall vanishes from the FACE while the
// card keeps its Stalled/Blocked lane slot and tier.
func (i planStallInfo) ShowBadge() bool {
	return i.Stalled && !i.EscalationBlocked && !i.PreBuildLifecycle
}

// HeadlineLabel is the badge's leading verdict word — "stalled" (loud,
// plan-level, nothing is watching) or "orphaned" (quiet, per-step, live crew
// coexist with a dead-agent orphan) — the iteration-23 re-scope
// (ka-0000005352). It is a pure projection of Orphaned so the template stays a
// thin render and the index card + cockpit header read the identical headline.
// A stall re-scoped to "orphaned" is still Stalled (the board lane + attention
// ranker still treat it as attention-worthy); only the FACE word softens.
func (i planStallInfo) HeadlineLabel() string {
	if i.Orphaned {
		return "orphaned"
	}
	return "stalled"
}

// FaceLabel is the compact discriminator the badge renders after the headline —
// BadgeLabel for a loud "stalled" verdict ("· 1 dead step" / "· gardener
// paused"), BUT SUPPRESSED for the re-scoped "orphaned" verdict
// (ka-0000005352 iteration 23). When Orphaned, the headline already carries the
// dead-step COUNT ("⚠ 1 orphaned" / "⚠ 2 orphaned"), so re-stating "· 1 dead
// step" beside it would be redundant and re-introduce the "dead step" phrasing
// the softer label deliberately drops. The orphan wait-age (AgeLabel) still
// rides along separately. For every non-orphaned stall this is exactly
// BadgeLabel, so the loud badge reads identically to before.
func (i planStallInfo) FaceLabel() string {
	if i.Orphaned {
		return ""
	}
	return i.BadgeLabel()
}

// HeadlineCount is the dead-step count folded INTO the re-scoped "orphaned"
// headline ("⚠ 2 orphaned"), so the softer verdict still carries how many steps
// are orphaned without the loud "N dead steps" phrasing (ka-0000005352 iteration
// 23). Zero (rendered as no number) for a non-orphaned stall, where the count
// lives in FaceLabel's "· N dead steps" instead.
func (i planStallInfo) HeadlineCount() int {
	if i.Orphaned {
		return i.DeadSteps
	}
	return 0
}

// planStallStaleThreshold is the frozen-duration past which the dead-agent stall
// badge tints loud (ka-0000005352 iteration 11). It is set TIGHTER than the
// decision (14d) and escalation (7d) thresholds because a dead-agent orphan is a
// different KIND of stop: those two wait on a HUMAN who may legitimately take
// days to answer an approval or a clarifying question, whereas a dead-agent step
// is a SYSTEM failure — the agent died and no human may even know the work is
// frozen. Every hour past the 1h grace is wasted, and an orphaned in_progress
// step still frozen a full day is not respawning on its own; it needs a human to
// rescue it. So one day is the "someone forgot this is broken" bar for a frozen
// orphan.
const planStallStaleThreshold = 24 * time.Hour

// BadgeLabel is the compact discriminator folded onto the ⚠ stalled badge FACE
// (ka-0000001697, iteration 10): WHICH of the stall conditions fired, in three
// or four words, so an operator scanning the board can tell a dead-agent orphan
// (rescue the step) from a paused gardener (resume it) from an unbound plan (bind
// one) WITHOUT hovering the tooltip. The stall badge was the ONLY attention-tier
// badge still rendering a bare generic label while its four distinct conditions
// (each with a different remediation) all read identically; this is the same
// "fold the discriminator onto the badge face" density win the decision chip
// (iter-4, "· 32d") and the escalation badge (iter-8, "· 40d") already shipped.
//
// Pure over the SAME fields deriveStall set (DeadSteps then GardenerState), in the
// SAME first-match precedence order deriveStall fired them (dead-agent steps
// outrank gardener state), so the face label can NEVER disagree with the Reason
// tooltip: whenever Reason names the dead-agent condition, DeadSteps ≥ 1 and this
// returns the dead-step label; whenever Reason names a gardener condition,
// DeadSteps == 0 and this returns the gardener label. Guarded on Stalled so a
// non-stalled info (GardenerState can be "paused"/"none" on a healthy plan a live
// assistant/crew exempts) yields no misleading label; empty for an
// unclassifiable stall too, in which case the badge falls back to a bare
// "⚠ stalled" exactly as before.
func (i planStallInfo) BadgeLabel() string {
	if !i.Stalled {
		return ""
	}
	switch {
	case i.DeadSteps == 1:
		return "1 dead step"
	case i.DeadSteps > 1:
		return strconv.Itoa(i.DeadSteps) + " dead steps"
	case i.GardenerState == "paused":
		return "gardener paused"
	case i.GardenerState == "none":
		return "no gardener"
	}
	return ""
}

// collectInProgressStepRefs walks the SAME batched child data
// renderedStepRollup walks (phase grandchildren in phase_order, then unphased
// direct children) and returns the in_progress steps' stall projections. Kept
// as a sibling of renderedStepRollup rather than a fourth return value so the
// rollup's widely-asserted signature stays put; both must mirror the timeline's
// membership rules or the badge would flag steps the page doesn't render.
func collectInProgressStepRefs(order, directChildIDs []string, grandchildIDsBySub map[string][]string, direct, grandchildren map[string]*beadsapi.BeadDetail) []stallStepRef {
	var refs []stallStepRef
	phaseStepIDs := make(map[string]struct{})
	phaseSet := make(map[string]struct{}, len(order))
	appendInProgress := func(ids []string, beads map[string]*beadsapi.BeadDetail, record map[string]struct{}) {
		for _, id := range ids {
			c := beads[id]
			if c == nil || !isIssueType(c.Type) {
				continue
			}
			if record != nil {
				record[id] = struct{}{}
			}
			if c.Status == "in_progress" {
				refs = append(refs, stallStepRef{Assignee: c.Assignee, UpdatedAt: c.UpdatedAt})
			}
		}
	}
	for _, pid := range order {
		c := direct[pid]
		if !isPhaseContainer(c) {
			continue
		}
		if _, dup := phaseSet[pid]; dup {
			continue
		}
		phaseSet[pid] = struct{}{}
		appendInProgress(grandchildIDsBySub[pid], grandchildren, phaseStepIDs)
	}
	for _, cid := range directChildIDs {
		if _, isPhase := phaseSet[cid]; isPhase {
			continue
		}
		if _, inPhase := phaseStepIDs[cid]; inPhase {
			continue
		}
		appendInProgress([]string{cid}, direct, nil)
	}
	return refs
}

// planGardenersByEpic lists the gardener trigger beads once for the whole page
// and joins them to their pinned epic (gardenerOwnedEpicID: the
// gardener-epic:<id> label, or legacy assign_bead). When an epic somehow has
// several gardeners, an open+enabled one wins over a paused one, which wins
// over a closed one — the state the badge reports must be the state of the
// gardener that would actually fire. Best-effort: a daemon error returns an
// empty map and every plan reads "none" (the badge degrades toward flagging,
// never toward silence).
func (s *Server) planGardenersByEpic(ctx context.Context, want map[string]struct{}) map[string]*beadsapi.BeadDetail {
	out := make(map[string]*beadsapi.BeadDetail)
	// NOT memoized. The gardener trigger beads this lists are exactly what the
	// /plans index MUTATES — the per-row toggle, the bulk enable/disable bar, and
	// the master switch all flip a gardener's enabled state and then re-render the
	// row (or the page) through this same enrich pass. A short-TTL memo here breaks
	// read-after-write: a just-toggled gardener, or one just bound to a plan, would
	// keep serving its stale/absent state for the TTL window, so the row chip and
	// stall badge disagree with the write the operator just made (ka-0000003113 —
	// the assistant list IS cached, it is the dominant ~730 ms/4.0 MB leg and is
	// never mutated from /plans; this ~207 ms leg is not).
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"trigger", "schedule"},
		Labels: []string{"role:gardener"},
		Sort:   "-updated_at",
		Limit:  gardenersListLimit,
	})
	if err != nil {
		s.logger.Debug("plans stall: gardeners list", "error", err)
		return out
	}
	if res == nil {
		return out
	}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		epicID := gardenerOwnedEpicID(b)
		if epicID == "" {
			continue
		}
		if _, wanted := want[epicID]; !wanted {
			continue
		}
		out[epicID] = pickBetterGardener(out[epicID], b, s.now())
	}
	return out
}

// pickBetterGardener prefers the gardener bead whose state should represent the
// plan: open beats closed, then enabled beats paused, then freshest.
func pickBetterGardener(cur, cand *beadsapi.BeadDetail, now time.Time) *beadsapi.BeadDetail {
	if cur == nil {
		return cand
	}
	if cand == nil {
		return cur
	}
	curOpen := statusOpenSortKey(cur.Status) == 0
	candOpen := statusOpenSortKey(cand.Status) == 0
	if curOpen != candOpen {
		if candOpen {
			return cand
		}
		return cur
	}
	curEnabled := buildTriggerRow(cur, now).Enabled
	candEnabled := buildTriggerRow(cand, now).Enabled
	if curEnabled != candEnabled {
		if candEnabled {
			return cand
		}
		return cur
	}
	if cand.UpdatedAt.After(cur.UpdatedAt) {
		return cand
	}
	return cur
}

// gardenerStateOf projects a plan's gardener bead into the badge vocabulary:
// "none" (nil or closed/retired), "paused" (open, enabled=false), or
// "running". A closed gardener is "none" on purpose — a retired watchdog
// watches nothing, which is exactly the condition the badge exists to flag.
func (s *Server) gardenerStateOf(b *beadsapi.BeadDetail) (state, id string) {
	if b == nil || statusOpenSortKey(b.Status) != 0 {
		return "none", ""
	}
	if buildTriggerRow(b, s.now()).Enabled {
		return "running", b.ID
	}
	return "paused", b.ID
}

// anyStepAgentLive reports whether any in_progress step is claimed by a live
// agent — a crew the gardener dispatched onto a step and that is actively
// working it. A dispatched crew single-claims its step (SpawnAgent's
// single_claim_task guard), so the step's assignee IS the crew agent name and
// alive() answers "is that crew live". This is the crew analogue of a live
// plan-assistant: either signal means someone is moving the work right now, so
// the unwatched (gardener paused/none) condition must NOT flag stalled — that
// was the ka-0000000734 false-stall (stalled despite active crew). No grace
// window: liveness is a right-now property, unlike deadStepCount's "has it sat
// too long" check.
func anyStepAgentLive(refs []stallStepRef, alive func(name string) bool) bool {
	for _, ref := range refs {
		if name := strings.TrimSpace(ref.Assignee); name != "" && alive(name) {
			return true
		}
	}
	return false
}

// deadStepCount counts the in_progress steps whose agent is dead or absent
// past the grace window. alive reports the liveness of an assignee name; a
// step with no assignee at all has no agent by definition. Steps that moved
// within the grace window are never counted, so a just-claimed step or an
// agent mid-respawn doesn't flap the badge.
func deadStepCount(refs []stallStepRef, now time.Time, alive func(name string) bool) int {
	dead := 0
	for _, ref := range refs {
		if !ref.UpdatedAt.IsZero() && now.Sub(ref.UpdatedAt) < planStallDeadAgentGrace {
			continue
		}
		name := strings.TrimSpace(ref.Assignee)
		if name == "" || !alive(name) {
			dead++
		}
	}
	return dead
}

// oldestDeadStepUpdatedAt returns the earliest non-zero UpdatedAt among the
// in_progress steps deadStepCount counts as dead — the instant the
// longest-frozen orphaned step last moved (ka-0000005352 iteration 11). It
// applies the SAME "is this step dead?" predicate deadStepCount uses (past the
// grace window AND no live agent), so the age can never describe a step
// deadStepCount didn't count — the anti-drift guarantee that keeps the badge's
// "· Nd" age agreeing with its "N dead steps" label. A dead step with a zero
// UpdatedAt is counted dead by deadStepCount but is UNDATABLE, so it is skipped
// here (earlierNonZeroTime never lets a zero win); a plan whose only dead step
// has a zero stamp yields a zero instant and renders no age — exactly the
// count-only fallback the decision (iter-4) and escalation (iter-8) ages use for
// an unset stamp. Returns zero when there is no datable dead step.
func oldestDeadStepUpdatedAt(refs []stallStepRef, now time.Time, alive func(name string) bool) time.Time {
	var oldest time.Time
	for _, ref := range refs {
		if !ref.UpdatedAt.IsZero() && now.Sub(ref.UpdatedAt) < planStallDeadAgentGrace {
			// Within the grace window — deadStepCount does not count it, so its
			// age must not surface here either.
			continue
		}
		name := strings.TrimSpace(ref.Assignee)
		if name == "" || !alive(name) {
			oldest = earlierNonZeroTime(oldest, ref.UpdatedAt)
		}
	}
	return oldest
}

// withDeadAgentAge stamps the dead-agent orphan wait-age (AgeLabel) + stale flag
// (AgeStale) onto a derived stall info, computed from the SAME in_progress step
// refs and liveness predicate deriveStall's deadSteps count came from (zero
// extra daemon call). It is a no-op unless the info is a dead-agent stall
// (Stalled AND DeadSteps ≥ 1) with a datable orphaned step: the
// gardener-paused/no-gardener conditions carry no age (the card's "updated Xh ago"
// meta already IS their "stalled since"), and an undatable orphan (zero stamp)
// reads count-only exactly as before. Shared by both the index bulk pass and the
// cockpit detail pass so the two surfaces render the identical age.
func withDeadAgentAge(info planStallInfo, refs []stallStepRef, now time.Time, alive func(name string) bool) planStallInfo {
	if !info.Stalled || info.DeadSteps < 1 {
		return info
	}
	oldest := oldestDeadStepUpdatedAt(refs, now, alive)
	if oldest.IsZero() {
		return info
	}
	info.AgeLabel = fmtTimeAgo(oldest, now)
	info.AgeStale = now.Sub(oldest) >= planStallStaleThreshold
	return info
}

// planStatusTerminal reports whether a plan epic's OWN status means the plan is
// finished (closed) or deliberately parked (deferred) — the two terminal lanes
// planBoardLaneKey short-circuits on. A terminal plan is never "stalled": the
// stall signal exists to catch open work that nothing is watching, but a closed
// plan's work is over and a deferred plan's work is intentionally on hold, so
// there is nothing to watch by definition. Case-insensitive to match the board
// (planBoardLaneKey lower-trims the raw status).
func planStatusTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "closed", "deferred":
		return true
	default:
		return false
	}
}

// deriveStall folds the inputs into the badge signal. planTerminal is whether
// the plan epic's OWN status is terminal (closed/deferred) — a terminal plan is
// never stalled (planStatusTerminal): its work is over or intentionally parked,
// so there is nothing left to watch. This closes the false-stall the audit
// found (ka-0000005352 iteration): a CLOSED plan with one leftover un-closed
// child step read "open work, but no gardener is bound to watch this plan" and
// rendered a loud ⚠ badge in the Closed lane — crying wolf on a finished plan.
// openWork is whether the plan has rendered steps still open. The unwatched
// conditions (gardener paused / none) are exempted whenever someone is actively
// working the plan RIGHT NOW — either assistantLive (a live plan-assistant IS
// the plan's bound agent) OR crewLive (a live crew agent the gardener dispatched
// onto a step). Threading crewLive fixes the ka-0000000734 false-stall:
// deriveStall used to exempt only the bound assistant, so a plan whose gardener
// was paused but whose crew was live-and-working still flagged "stalled" —
// crying wolf on healthy work. The dead-agent conditions are NOT exempted by a
// live assistant/crew (an orphaned in_progress step is stalled regardless of
// whether OTHER steps have live agents) — but they ARE suppressed on a terminal
// plan, since a closed plan's leftover in_progress step is not live work anyone
// is expected to rescue.
func deriveStall(gardenerState, gardenerID string, deadSteps int, openWork, assistantLive, crewLive, planTerminal bool) planStallInfo {
	info := planStallInfo{GardenerState: gardenerState, GardenerID: gardenerID, DeadSteps: deadSteps}
	if planTerminal {
		// A closed/deferred plan is terminal: the stall signal never fires,
		// whatever its leftover child steps or gardener state say.
		info.DeadSteps = 0
		return info
	}
	watched := assistantLive || crewLive
	switch {
	case deadSteps == 1:
		info.Stalled = true
		info.Reason = "1 step in progress with a dead or absent agent"
	case deadSteps > 1:
		info.Stalled = true
		info.Reason = strconv.Itoa(deadSteps) + " steps in progress with a dead or absent agent"
	case openWork && !watched && gardenerState == "paused":
		info.Stalled = true
		info.Reason = "open work, but the plan's gardener is paused"
	case openWork && !watched && gardenerState == "none":
		info.Stalled = true
		info.Reason = "open work, but no gardener is bound to watch this plan"
	}
	return info
}

// reconcileStallWithCrew makes the ⚠ stalled badge agree with the crew chip on
// the SAME card (ka-0000005352 iteration 19). The badge and the crew chip were
// each deciding "is this plan being worked right now" from a DIFFERENT liveness
// source, so a card could render "● N crew · M working" AND "⚠ stalled · no
// gardener" at the same time — a self-contradiction (the exact mission-named
// false-stall: "plans with active crew but a paused/absent gardener are flagged
// stalled"). deriveStall's crewLive input came from anyStepAgentLive, which
// inspects ONLY the in_progress steps' Assignee field; the crew CHIP counts
// every crew linked by an `assigned` edge across ALL rendered steps (open +
// in_progress) via planCrewBulk. So a gardener-dispatched crew working an open
// step — or one linked only by an assigned edge with no step Assignee — is
// "working" on the chip but INVISIBLE to the stall proxy, and the paused/none
// branch fires anyway.
//
// The fix is to feed the stall the SAME authoritative signal the chip renders:
// crewLive (planCrewRollup.HasLive() == Working > 0). This runs in the index
// assembly loop AFTER wave 2, where both the stall and the crew rollup are in
// hand — so it costs zero extra daemon call and keeps the two batches parallel.
// It handles the two crew-coexisting cases DIFFERENTLY, both monotonic (never
// CREATE a stall, only soften an existing one):
//
//   - DeadSteps == 0 (a gardener-paused or no-gardener stall — the only two
//     branches deriveStall fires without a dead step): CLEAR it outright.
//     Nothing is actually wedged — the crew rollup proves work is moving OR
//     pending — so the false paused/none alarm is fully retracted, returning the
//     same shape deriveStall yields for a healthy plan (Stalled false). This is
//     the iteration-19 behavior, extended in iteration 51 to fire on QUEUED crew
//     too (see the queued-arm note below the re-scope case).
//   - DeadSteps ≥ 1 (a dead-agent orphan) WITH live crew (iteration 23): the
//     orphan is REAL — a live crew on step B does not rescue a dead agent on
//     step A — so the plan must stay Stalled (board lane + attention ranker
//     still treat it as attention-worthy, no iteration-10/11 regression). But
//     the LOUD plan-level "⚠ stalled" verdict is wrong when the crew chip shows
//     the plan being worked, so RE-SCOPE it: flip Orphaned true, which softens
//     the badge FACE to the per-step "⚠ N orphaned" while every other field
//     (Stalled, DeadSteps, Reason, AgeLabel, AgeStale) rides along untouched.
//     A dead-agent stall with NO live crew is genuinely unwatched, so it keeps
//     the loud "⚠ stalled" exactly as before.
//
// The QUEUED arm (ka-0000005352 iteration 51): the DeadSteps==0 CLEAR also fires
// when the plan has crew merely QUEUED (crewQueued: an open idle crew the gardener
// dispatched but the pool has not yet slotted, Idle>0/Working==0), not only when
// crew are actively working. iteration 19 cleared the false gardener-state stall
// only for LIVE crew (Working>0), so a plan whose gardener dispatched crew now
// sitting in the pool queue still rendered "⚠ stalled · no gardener" beside the
// card's own "● N crew · K queued" facet — the SAME self-contradiction the
// mission names ("plans with active crew but a paused/absent gardener are flagged
// stalled"). A queued crew is unambiguous proof the gardener ALREADY acted: the
// "no gardener / gardener paused" remediation is wrong when the card is literally
// showing dispatched-and-pending crew. So a gardener-state stall is a false alarm
// whenever the plan has EITHER working OR queued crew.
//
// The dead-agent orphan RE-SCOPE stays gated on LIVE crew only: a queued crew is
// NOT watching an orphaned in_progress step (it has not started), so a real
// dead-agent orphan must keep its loud plan-level "stalled" verdict when the only
// other crew are queued — a queued crew does not rescue a wedged step the way a
// working one arguably contextualizes it (iter-23). Only the gardener-state (no
// dead step) alarm is cleared by queued crew.
func reconcileStallWithCrew(info planStallInfo, crewLive, crewQueued bool) planStallInfo {
	if !info.Stalled {
		return info
	}
	if info.DeadSteps == 0 {
		// Gardener-paused / no-gardener false alarm: the gardener dispatched crew
		// that is either working now OR queued to start → clear it. A queued crew
		// proves the gardener acted, so the "bind/resume a gardener" remediation the
		// badge implies is wrong (iter-51 extends the iter-19 live-only clear).
		if crewLive || crewQueued {
			return planStallInfo{GardenerState: info.GardenerState, GardenerID: info.GardenerID}
		}
		return info
	}
	// A real dead-agent orphan: re-scope to the quiet per-step "orphaned" ONLY
	// when LIVE crew coexist (a working crew contextualizes the badge beside the
	// chip's "N working"). A merely-queued crew is not watching the orphaned step,
	// so it does not soften a genuine orphan — the loud verdict stands.
	if crewLive {
		info.Orphaned = true
	}
	return info
}

// reconcileStallWithEscalation suppresses the ⚠ stalled BADGE on a card that is
// ALSO ⛔ blocked by an open escalation, for the gardener-state stall only
// (ka-0000005352 iteration 26). It is the escalation analogue of
// reconcileStallWithCrew: both kill a self-contradictory double-badge by feeding
// the stall a signal already in hand at the enrich seam (there the crew rollup,
// here the escalation count), and both are MONOTONIC — they only soften an
// existing stall, never create one.
//
// The contradiction: an escalation-blocked plan carries "⛔ blocked by
// escalation · Nd" (a privileged operator must act — the HARDER stop that
// already OUTRANKS stall in planAttention). Rendering "⚠ stalled · no gardener"
// beside it tells the operator to bind a gardener, which cannot clear the
// escalation — two badges, two different remediations, only one of them right.
// On the live board this fired on 16 of 39 stalled-lane cards. The board lane is
// unaffected either way: planBoardLaneKey already unions Escalations.Count>0 and
// Stall.Stalled into the SAME stalled/blocked lane, so the card keeps its slot;
// this only drops the redundant FACE.
//
// It handles the two stall kinds DIFFERENTLY:
//
//   - DeadSteps == 0 (a gardener-paused or no-gardener stall — the two branches
//     whose whole remediation is "a gardener should be watching"): SUPPRESS the
//     badge. The escalation, not a missing gardener, is why the plan is wedged;
//     the ⛔ badge already says so. Set EscalationBlocked so ShowBadge() drops
//     the face while Stalled stays true (ranker + lane untouched).
//   - DeadSteps ≥ 1 (a dead-agent orphan): keep the loud badge. A dead or absent
//     agent on an in_progress step is a real SYSTEM failure that persists
//     regardless of the escalation — suppressing it would hide a genuinely
//     orphaned step behind an unrelated human-approval wait. Untouched.
//
// A stall on a plan with no blocking escalation (blocked == false) is unaffected.
func reconcileStallWithEscalation(info planStallInfo, blocked bool) planStallInfo {
	if !info.Stalled || !blocked || info.DeadSteps != 0 {
		return info
	}
	info.EscalationBlocked = true
	return info
}

// reconcileStallWithLifecycle suppresses the ⚠ stalled BADGE on a plan still in
// its PRE-BUILD authoring lifecycle (draft/ready), for the gardener-state stall
// only (ka-0000005352 iteration 49). It is the lifecycle twin of
// reconcileStallWithEscalation: both kill a self-contradictory gardener-state
// stall FACE by feeding the stall a signal already in hand at the enrich seam
// (there the escalation count, here the plan's lifecycle state), and both are
// MONOTONIC — they only soften an existing stall, never create one.
//
// The contradiction: a plan in draft/ready has NO enabled gardener BY DESIGN.
// gardenerEnableGatedByState (the R6 build gate) forbids enabling a gardener on a
// draft/ready plan — "Approve & build" is the sole transition that unblocks it —
// so "⚠ stalled · no gardener" / "· gardener paused" implies a remediation ("bind
// / resume a gardener") that the lifecycle deliberately holds back. The card
// already carries the READY/DRAFT lifecycle chip (and, for a ready plan, the
// "Approve & build" button) saying exactly that: nothing builds until you
// approve. Two signals, contradictory remediation, only one right — the exact
// iteration-26 shape applied to the lifecycle. On the live board (2026-08-11
// pour) this fired on 7 of 25 gardener-state stall cards (28%).
//
// buildGated is gardenerEnableGatedByState(planStateOf(epic)) — true for
// draft/ready, false for building/complete/unset. It handles the two stall kinds
// DIFFERENTLY, matching reconcileStallWithEscalation:
//
//   - DeadSteps == 0 (a gardener-paused or no-gardener stall — the two branches
//     whose whole remediation is "a gardener should be watching"): SUPPRESS the
//     badge FACE (set PreBuildLifecycle → ShowBadge() drops it) while Stalled
//     stays true (ranker + lane untouched). The lifecycle, not a missing gardener,
//     is why no gardener is bound.
//   - DeadSteps ≥ 1 (a dead-agent orphan): keep the loud badge. A dead or absent
//     agent on an in_progress step is a real SYSTEM failure that persists
//     regardless of the authoring lifecycle — even a draft plan can have crew
//     dispatched onto a step that then dies — so suppressing it would hide a
//     genuinely orphaned step. Untouched.
//
// A stall on a plan NOT in a build-gated lifecycle (buildGated == false:
// building/complete/legacy-unset) is unaffected — such a plan's gardener is not
// lifecycle-gated, so a missing/paused gardener IS a real "nothing is watching"
// stall the operator should see.
func reconcileStallWithLifecycle(info planStallInfo, buildGated bool) planStallInfo {
	if !info.Stalled || !buildGated || info.DeadSteps != 0 {
		return info
	}
	info.PreBuildLifecycle = true
	return info
}

// planStallBulk derives the stall signal for every plan on the /plans index in
// ONE batched call across the whole page (the agent-liveness batch), joined
// against the step data planIndexStepData already walked AND the gardeners map
// planIndexEnrich now resolves ONCE and shares (ka-0000004720) — the "one more
// derived field, no new fan-out" contract of Step 4. The gardeners list used to
// be fetched here too; hoisting it to the enrich seam lets the unified
// loop+agent chip and the stall badge read from the same single gardener lookup.
// statusByEpic maps each plan epic id → its OWN raw status, so a terminal
// (closed/deferred) plan is exempted from the stall signal (planStatusTerminal).
// Without it the badge fired on a CLOSED plan carrying a leftover un-closed
// child step — the false-stall the audit surfaced (ka-0000005352 iteration).
func (s *Server) planStallBulk(ctx context.Context, epicIDs []string, statusByEpic map[string]string, stepData map[string]*planIndexStepInfo, assistants, gardeners map[string]*beadsapi.BeadDetail) map[string]planStallInfo {
	out := make(map[string]planStallInfo, len(epicIDs))
	if len(epicIDs) == 0 {
		return out
	}

	// One liveness batch over every in_progress step assignee on the page.
	names := make([]string, 0, 8)
	seen := make(map[string]struct{})
	for _, id := range epicIDs {
		sd := stepData[id]
		if sd == nil {
			continue
		}
		for _, ref := range sd.InProgressSteps {
			n := strings.TrimSpace(ref.Assignee)
			if n == "" || !looksLikeAgentName(n) {
				continue
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			names = append(names, n)
		}
	}
	liveByName := make(map[string]bool, len(names))
	if len(names) > 0 {
		beads, err := s.daemon.FindAgentBeadsByNames(ctx, names, nil)
		if err != nil {
			s.logger.Debug("plans stall: agent liveness batch", "error", err)
		} else {
			for name, b := range beads {
				// assistantIsLive is the /agents liveness predicate (alive
				// state + fresh) — the same bar the assistant chip uses.
				liveByName[name] = s.assistantIsLive(b)
			}
		}
	}
	alive := func(name string) bool { return liveByName[name] }

	now := s.now()
	for _, id := range epicIDs {
		state, capID := s.gardenerStateOf(gardeners[id])
		var deadSteps int
		openWork := false
		crewLive := false
		var refs []stallStepRef
		if sd := stepData[id]; sd != nil {
			refs = sd.InProgressSteps
			deadSteps = deadStepCount(refs, now, alive)
			openWork = sd.StepsTotal > sd.StepsClosed
			// A live crew agent on an in_progress step means the plan IS being
			// worked even if the gardener is paused/unbound — reuses the same
			// liveness batch deadStepCount consulted, no extra daemon call.
			crewLive = anyStepAgentLive(refs, alive)
		}
		info := deriveStall(state, capID, deadSteps, openWork, s.assistantIsLive(assistants[id]), crewLive, planStatusTerminal(statusByEpic[id]))
		// Fold the oldest dead-agent orphan's wait-age onto the badge (iter 11) —
		// rides the SAME refs + liveness predicate, zero extra daemon call.
		out[id] = withDeadAgentAge(info, refs, now, alive)
	}
	return out
}

// planStallForDetail computes the identical signal for one plan's cockpit page
// from the timeline it already fetched. Liveness rides the request's prewarmed
// agentResolver (every step assignee is already in its cache), so the only
// extra daemon call is the single gardeners-by-epic lookup. epicStatus is the
// plan epic's OWN raw status: a terminal (closed/deferred) plan is never
// stalled (planStatusTerminal), mirroring the index — so the two surfaces agree
// on a closed plan showing no ⚠ badge.
func (s *Server) planStallForDetail(ctx context.Context, epicID, epicStatus string, steps []planStepView, phases []planPhaseView, openWork, assistantLive bool, alive func(name string) bool) planStallInfo {
	gardeners := s.planGardenersByEpic(ctx, map[string]struct{}{epicID: {}})
	state, capID := s.gardenerStateOf(gardeners[epicID])

	refs := make([]stallStepRef, 0, 4)
	addSteps := func(rows []planStepView) {
		for _, st := range rows {
			if st.Status == "in_progress" {
				refs = append(refs, stallStepRef{Assignee: st.Assignee, UpdatedAt: st.UpdatedAt})
			}
		}
	}
	addSteps(steps)
	for _, p := range phases {
		addSteps(p.Steps)
	}
	now := s.now()
	deadSteps := deadStepCount(refs, now, alive)
	crewLive := anyStepAgentLive(refs, alive)
	info := deriveStall(state, capID, deadSteps, openWork, assistantLive, crewLive, planStatusTerminal(epicStatus))
	// Same dead-agent orphan wait-age the index card carries (iter 11), so the
	// cockpit header and the index badge render the identical "· Nd".
	return withDeadAgentAge(info, refs, now, alive)
}
