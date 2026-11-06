package main

// plans_attention.go — the attention-first ranking over the plan rows the
// /plans enrich pass already computes (Step 2, ka-0000004961; epic
// ka-0000000492's mobile card-deck). Every ingredient is ALREADY on the
// planRowView by the time these run (OpenDecisions, Escalations.Count,
// PlanState, Stall.Stalled, lastActivityAt) — planIndexEnrich derives them once
// per page, so ranking adds NO daemon call. This is the plan analogue of
// sortGardenerRows (gardeners.go): a pure, table-tested ordering that pins what a
// human must act on to the top.
//
// The board (plans_board.go) already sections cards into a Stalled/Blocked
// LANE, but that is bucketing, not ranking — within a lane, and on the flat
// grid + the mobile deck, the rows still need an order that answers "what do I
// touch first?". planAttention supplies the tier; sortPlanRows applies it.

import (
	"sort"
	"time"
)

// planAttentionTier ranks a plan by how urgently it needs a HUMAN's eye. Lower
// values sort FIRST (most attention), so the zero value is the top tier. The
// tiers are a strict priority order — a plan that matches several is ranked by
// its most-urgent match (planAttention is first-match-wins), so a stalled plan
// that ALSO has an open decision reads as a decision, not a stall.
//
// The ordering was chosen to drain the measured backlog (decision ka-0000000932:
// 85 open decisions / 53 escalations, 23 decisions >30 days old): the tiers put
// the human-blocking work up top, and WITHIN those tiers sortPlanRows drains
// oldest-waiting-first so the >30d tail surfaces instead of rotting under a
// freshest-first feed.
type planAttentionTier int

const (
	// planAttentionDecision — an open clarifying-question decision is waiting on
	// the operator. A human is LITERALLY the blocker: nothing moves until they
	// answer, so this pins to the very top.
	planAttentionDecision planAttentionTier = iota
	// planAttentionEscalation — the plan (or one of its steps) is blocked by an
	// open escalation: a privileged op only a human/system can perform. Age is
	// the urgency signal here (escalations.go), which the within-tier
	// oldest-first order surfaces.
	planAttentionEscalation
	// planAttentionReady — the plan is finalized and sitting in `ready`, awaiting
	// the operator's one-tap "Approve & build" (planStateReady). One tap unblocks
	// a whole plan, so it outranks a stall.
	planAttentionReady
	// planAttentionStalled — open work with nothing watching it, or an
	// in_progress step under a dead/absent agent (deriveStall, plans_stall.go).
	// The plan is wedged but not (yet) directly asking the operator for an input,
	// so it sits below the three "one human action unblocks it" tiers.
	planAttentionStalled
	// planAttentionCloseout — the plan is DRAINED and awaiting a retire: the
	// daemon flagged it `autoclose-ready`, or every rendered step is closed while
	// the epic is still open (planNeedsCloseout, plans_closeout.go). Matt's ask
	// (thread ka-0000001785) is precisely that these stop being something you have
	// to hunt for, so they rank ABOVE the residual feed — one tap retires a plan
	// and permanently shrinks the pile, which is the highest-leverage action on
	// the whole board per unit of operator effort.
	//
	// Ranked BELOW stalled, mirroring planBoardLaneKey's own precedence so the
	// lane and the rank can never tell different stories: a wedged plan needs a
	// human to unwedge it (real forward work), while a drained one needs only a
	// confirmation. Cheap ≠ urgent.
	planAttentionCloseout
	// planAttentionNone — everything else: healthy, moving, or simply idle plans.
	// The residual feed, ordered freshest-first by sortPlanRows.
	planAttentionNone
)

// planAttention maps a fully-enriched plan row to its attention tier. Pure over
// the already-computed row fields — no daemon call, no clock read — so it is
// trivially table-testable and the deck, the flat grid, and the board lanes all
// rank identically. First-match-wins encodes the priority order documented on
// planAttentionTier.
func planAttention(r planRowView) planAttentionTier {
	switch {
	case r.OpenDecisions > 0:
		return planAttentionDecision
	case r.Escalations.Count > 0:
		return planAttentionEscalation
	case planState(r.PlanState) == planStateReady:
		return planAttentionReady
	case r.Stall.Stalled:
		return planAttentionStalled
	case planNeedsCloseout(r):
		return planAttentionCloseout
	default:
		return planAttentionNone
	}
}

// sortPlanRows orders plan cards attention-first, in place and stably (Step 2,
// ka-0000004961). It is the plan analogue of sortGardenerRows: the primary key is
// the planAttention tier (decision → escalation → ready → stalled → closeout →
// rest), and the within-tier key is the plan's last-activity instant, but in
// OPPOSITE directions by tier:
//
//   - Attention tiers (decision/escalation/ready/stalled/closeout): OLDEST-WAITING FIRST.
//     A plan that has sat untouched longest is the one most in danger of rotting
//     in the >30d tail, so it leads its tier — this is the key that drains the
//     backlog rather than burying it under fresher arrivals.
//   - The residual tier (none): FRESHEST FIRST, so healthy plans read like a
//     normal recent-activity feed.
//
// A zero (unknown) activity stamp always sinks to the bottom of its tier in
// BOTH directions — an unstamped plan is neither the freshest nor the one that
// has waited longest. Ties break deterministically by id so the order is stable
// across renders regardless of the input order.
func sortPlanRows(rows []planRowView) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]

		// Primary key: the attention tier. Lower tier == more attention == first.
		ta, tb := planAttention(a), planAttention(b)
		if ta != tb {
			return ta < tb
		}

		// Within-tier: the residual feed reads freshest-first; every attention
		// tier drains oldest-waiting-first (the >30d-tail key).
		freshest := ta == planAttentionNone
		if less, decided := planRowActivityLess(a.lastActivityAt, b.lastActivityAt, freshest); decided {
			return less
		}

		// Deterministic, order-independent tie-break.
		return a.ID < b.ID
	})
}

// planRowActivityLess reports whether activity a sorts before activity b within
// a tier. When freshest is true the tier reads newest-first; otherwise
// oldest-first. A zero stamp always sinks to the bottom of its tier in BOTH
// directions (a stamped row sorts before an unstamped one). decided is false
// only when the two are indistinguishable on this key (equal, or both zero), so
// the caller falls through to the id tie-break.
func planRowActivityLess(a, b time.Time, freshest bool) (less, decided bool) {
	if a.IsZero() || b.IsZero() {
		if a.IsZero() != b.IsZero() {
			return !a.IsZero(), true // the stamped row sorts first
		}
		return false, false // both zero: undecided → id tie-break
	}
	if a.Equal(b) {
		return false, false
	}
	if freshest {
		return a.After(b), true
	}
	return a.Before(b), true
}
