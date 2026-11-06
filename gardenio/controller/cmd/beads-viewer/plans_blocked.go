package main

// plans_blocked.go — the plan-level "blocker state" signal (ka-0000005352
// iter-55). A plan epic can carry raw status=blocked with forward `blocks`
// dependency edges to sibling beads (a task/decision/etc. that must resolve
// before the plan can proceed). Such a plan lands in the loud STALLED board lane
// (planBoardLaneKey `case "blocked"`), but the index card rendered only a bare
// "blocked" status chip that (a) had no chip-status-blocked CSS rule at all —
// default gray, no border, unlike open/in_progress/closed — and (b) said NOTHING
// about WHY it was blocked or whether its blockers were still open.
//
// Measured on the live board (2026-08-11, 128 plan epics): 5 plan epics were raw
// status=blocked; 4 of the 5 had EVERY forward blocks-edge blocker already CLOSED
// (ka-0000000822, ka-0000006115, ka-0000004145, ka-0000004689), so 4 cards sat in
// the loud attention lane advertising "blocked" when the blockers were resolved
// and the plan was actually ready to be un-blocked; only ka-0000000306 still had
// 1 open blocker. That is a card-vs-truth P-dimension gap in the mission-named
// false-stalled family (iter-6/19/23/26/49/51), here applied to the plan epic's
// OWN raw blocked status — the one blocked signal never reconciled against the
// live state of the beads that block it.
//
// This aggregates the epic's forward blocks-edges per plan so the index card's
// status chip (plan_card.html) can fold a compact blocker facet: a LOUD
// "· N blocked · Xage" when open blockers exist (the ⛔ escalation-badge
// count+age idiom, iter-8), or a CALM "· ✓ cleared" marker when every blocker is
// closed (the iter-29/41 calm-marker idiom) telling the operator the plan can be
// unblocked. The signal is ADDITIVE — it never overrides the raw status or the
// board lane; a blocked plan stays in the STALLED lane (the operator has not yet
// flipped the status), the facet just tells the truth about the blockers.
//
// Derived in the SAME batched enrich idiom as planEscalationsBulk / planCrewBulk
// / planOpenMRsBulk — ONE forward blocks-edge batch over the candidate epic ids +
// ONE bead-hydration batch over the distinct blocker targets — but SCOPED to the
// handful of raw status=blocked plans on the page: a non-blocked plan carries no
// blocker facet, so walking it would be wasted work. When NO plan on the page is
// blocked (the common case), the join issues ZERO daemon calls.

import (
	"context"
	"time"
)

// planBlockedStaleThreshold is the wait-age past which a blocked plan's OPEN
// blocker facet tints loud. Set to 7d, matching the escalation badge
// (planEscalationStaleThreshold) and the open-MR pill (planMROpenStaleThreshold):
// a plan blocked on an unresolved sibling for a full week is squarely into
// "someone forgot about this" territory, the same "an aged blocker is holding
// this up" shape those signals tint on the same bar. Below it the facet reads
// calm (the age is context, not an alarm) — the two-tier calm-by-default /
// loud-when-egregious treatment every sibling wait-age gives.
const planBlockedStaleThreshold = 7 * 24 * time.Hour

// planBlockerInfo is the derived per-plan blocker-state signal. It is rendered
// ONLY on a card whose raw status is "blocked" (the template gate) — a plan in
// any other status carries the zero value and renders no facet.
type planBlockerInfo struct {
	// Total is how many forward `blocks`-edge blockers the plan epic has (open +
	// closed). Zero means the blocked plan carries no resolvable blocks-edge at
	// all (a hand-set status with no edge) — the facet then reads nothing extra,
	// exactly as before this iteration.
	Total int
	// Open is how many of those blockers are still non-closed — the count the
	// loud "· N blocked" facet renders. When Open == 0 but Total > 0 every
	// blocker is resolved and the calm "· ✓ cleared" marker renders instead.
	Open int
	// oldestOpenAt is the creation instant of the OLDEST still-open blocker,
	// feeding the "· Xage" wait-age + the loud stale tint. Zero when no open
	// blocker is datable (the facet then reads count-only, like a fresh
	// escalation). Unexported: it feeds the OldestAge()/OldestStale() projections
	// the template reads, never a raw field.
	oldestOpenAt time.Time
}

// Any reports whether the plan has any resolvable blocks-edge blocker at all —
// the render gate for the blocker facet's inner content. A blocked plan with no
// blocks-edge (Total == 0) adds no facet ink, exactly as before.
func (i planBlockerInfo) Any() bool { return i.Total > 0 }

// HasOpen reports whether the plan still has at least one non-closed blocker —
// the gate for the LOUD "· N blocked · Xage" facet. False when every blocker is
// closed (the calm "· ✓ cleared" marker renders instead) or the plan has none.
func (i planBlockerInfo) HasOpen() bool { return i.Open > 0 }

// OldestAge is the humanized wait-age of the OLDEST still-open blocker ("5d"),
// folded onto the loud facet so it carries the SAME temporal urgency every
// sibling wait-age surfaces (escalation "· 40d", stall "· 3d", MR "· 8d"). Empty
// when the plan has no open blocker or the oldest one's stamp is unset (the facet
// then reads count-only, exactly like a fresh escalation), computed against now
// so a test/handler shares one clock. Mirrors planEscalationInfo.OldestAge.
func (i planBlockerInfo) OldestAge(now time.Time) string {
	if i.Open == 0 || i.oldestOpenAt.IsZero() {
		return ""
	}
	return fmtTimeAgo(i.oldestOpenAt, now)
}

// OldestStale reports whether the oldest still-open blocker has waited past
// planBlockedStaleThreshold — the mark that tints the loud facet so an aged,
// plan-blocking dependency pops out of the board instead of hiding behind a bare
// count. False for a fresh blocker, a cleared/blocker-less plan, or an unset
// stamp. Mirrors planEscalationInfo.OldestStale.
func (i planBlockerInfo) OldestStale(now time.Time) bool {
	if i.Open == 0 || i.oldestOpenAt.IsZero() {
		return false
	}
	return now.Sub(i.oldestOpenAt) >= planBlockedStaleThreshold
}

// planBlockedBulk resolves the forward `blocks`-edge blocker state for every plan
// in blockedIDs (the raw status=blocked plan epic ids on the page). It is the
// blocker twin of planEscalationsBulk PATH 1, but hydrates ALL blocker targets
// (not only type=escalation ones) so a plan blocked on an ordinary task/decision
// is surfaced. Two bounded batched calls — ONE forward BatchGetDependencies over
// the blocked epic ids + ONE BatchGetBeads over the distinct blocks-targets — so
// the enrich budget stays plan-count invariant. Returns an empty map (ZERO daemon
// calls) when no plan on the page is blocked, the common case.
func (s *Server) planBlockedBulk(ctx context.Context, blockedIDs []string) map[string]planBlockerInfo {
	out := make(map[string]planBlockerInfo, len(blockedIDs))
	if len(blockedIDs) == 0 {
		return out
	}

	// ONE forward blocks-edge batch over the blocked epic ids.
	depsByID, err := s.daemon.BatchGetDependencies(ctx, blockedIDs, "forward")
	if err != nil {
		s.logger.Debug("plan blockers: blocks walk", "error", err)
		return out
	}

	// Collect the distinct blocks-targets across all blocked plans for one
	// hydration batch.
	targets := make([]string, 0)
	targetSeen := make(map[string]struct{})
	for _, id := range blockedIDs {
		for _, d := range depsByID[id] {
			if d.Type != "blocks" || d.DependsOnID == "" {
				continue
			}
			if _, dup := targetSeen[d.DependsOnID]; dup {
				continue
			}
			targetSeen[d.DependsOnID] = struct{}{}
			targets = append(targets, d.DependsOnID)
		}
	}
	if len(targets) == 0 {
		return out
	}

	blockers, berr := s.daemon.BatchGetBeads(ctx, targets)
	if berr != nil {
		s.logger.Debug("plan blockers: blocker beads", "error", berr)
		return out
	}

	// Assemble per plan: count total + open blocks-targets and track the oldest
	// still-open blocker's creation instant (earliest-real-stamp, so a zero-stamp
	// blocker never wins over a stamped one — the earlierNonZeroTime rule the
	// escalation refs use).
	for _, id := range blockedIDs {
		info := planBlockerInfo{}
		for _, d := range depsByID[id] {
			if d.Type != "blocks" || d.DependsOnID == "" {
				continue
			}
			b := blockers[d.DependsOnID]
			if b == nil {
				continue
			}
			info.Total++
			if b.Status != "closed" {
				info.Open++
				info.oldestOpenAt = earlierNonZeroTime(info.oldestOpenAt, b.CreatedAt)
			}
		}
		if info.Total > 0 {
			out[id] = info
		}
	}
	return out
}
