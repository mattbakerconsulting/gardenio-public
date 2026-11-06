package server

import (
	"context"
	"slices"

	"gardenio/kbeads/internal/model"
)

// A later occupant of a seat reflagging its seat's own decision or escalation
// (gd-Nu4lxqkWwL, decision gd-r0wmJH0IUL).
//
// Every plan-gardener run is a fresh pod with a new name suffix, and the
// playbook tells each run to reflag stale decisions an EARLIER run filed. gb
// decision reflag / gb escalation reflag (runReflag, gb/decision.go) writes
// through authorizeOwnership, which knows only created_by / assignee / owner,
// i.e. the earlier pod's name, so every reflag of a predecessor's decision
// 403'd and stale asks were never re-surfaced.
//
// So the seat that filed it may reflag it, with the identity binding of the
// artifact grant (decision_artifact_seat.go, gd-JPfZCDbKFp) and nothing wider:
//
//   - identity: the caller's open agent bead has seat_key == the bead's
//     owner_seat, AND the token's subject occupies that seat; or the caller
//     is the bound gardener of the open plan in the bead's requesting_plan
//     (callerGardensRequestingPlan);
//   - type: decision or escalation, not closed;
//   - patch: exactly one of the three writes runReflag makes, each its own
//     PATCH:
//     1. priority alone, strictly lower than the current one (toward P0);
//     2. fields alone, only the reflag_* keys (plus pingback_* on an
//     escalation, the re-surface handshake the bridge keys on);
//     3. labels alone, the current set plus "escalated" and nothing else.
//
// Answer fields (chosen, responded_by...), status, title, assignee and every
// other column stay refused: the patch shapes admit none of them, and
// authorizeDecisionAnswerFields runs before this regardless.

// reflagSeatFields are the fields any seat reflag may write.
var reflagSeatFields = []string{"reflag_count", "reflagged_at", "reflagged_by", "reflag_reason"}

// escalationReflagSeatFields adds the pingback handshake an escalation reflag
// stamps so the bridge re-posts the card.
var escalationReflagSeatFields = append(slices.Clone(reflagSeatFields),
	"pingback_requested_at", "pingback_count", "pingback_by", "pingback_source", "pingback_reason")

// reflagEscalatedLabel is the one label a seat reflag may add.
const reflagEscalatedLabel = "escalated"

// authorizeReflagBySeat reports whether this update is a later occupant of
// the bead's owner_seat reflagging it, and so may skip the ownership gate.
func (s *BeadsServer) authorizeReflagBySeat(ctx context.Context, bead *model.Bead, in *updateBeadInput) bool {
	if bead == nil || bead.Status == model.StatusClosed {
		return false
	}
	var allowed []string
	switch bead.Type {
	case model.TypeDecision:
		allowed = reflagSeatFields
	case model.TypeEscalation:
		allowed = escalationReflagSeatFields
	default:
		return false
	}
	if !isReflagPatch(bead, in, allowed) {
		return false
	}
	agent, err := s.callerAgentBead(ctx)
	if err != nil || agent == nil {
		return false
	}
	if s.callerGardensRequestingPlan(ctx, bead, agent) {
		return true
	}
	ownerSeat, _, _ := exactFieldString(bead.Fields, "owner_seat")
	if ownerSeat == "" {
		return false
	}
	seat, _, _ := exactFieldString(agent.Fields, "seat_key")
	if seat != ownerSeat {
		return false
	}
	return subjectOccupiesSeat(AuthIdentityFromContext(ctx).Subject, ownerSeat)
}

// callerGardensRequestingPlan reports whether agent is the bound gardener of
// the open plan named in bead's requesting_plan, in the token's project: the
// plan's gardener reflags stale handles its crews filed (gd-GrJRP7kLBY, the
// playbook's stale-handle step).
func (s *BeadsServer) callerGardensRequestingPlan(ctx context.Context, bead, agent *model.Bead) bool {
	planID, _, _ := exactFieldString(bead.Fields, "requesting_plan")
	project := TokenProjectFromContext(ctx)
	if planID == "" || project == "" || projectFromFields(agent.Fields) != project {
		return false
	}
	plan, err := s.store.GetBead(ctx, planID)
	if err != nil || plan == nil || plan.Status == model.StatusClosed || plan.Type != model.TypeEpic ||
		!hasLabel(plan.Labels, planMarkerLabel) || projectFromLabelsOrEmpty(plan.Labels) != project {
		return false
	}
	return s.agentIsPlanGardener(ctx, plan, agent)
}

// isReflagPatch reports whether in is exactly one of runReflag's three writes.
func isReflagPatch(bead *model.Bead, in *updateBeadInput, allowed []string) bool {
	if in.Title != nil || in.ShortTitle != nil || in.Heraldry != nil || in.Description != nil || in.Notes != nil ||
		in.Status != nil || in.Assignee != nil || in.Owner != nil || in.dueAtSet || in.deferUntilSet {
		return false
	}
	switch {
	case in.Priority != nil:
		return !in.labelsSet && len(in.Fields) == 0 &&
			*in.Priority >= 0 && *in.Priority < bead.Priority
	case in.labelsSet:
		return len(in.Fields) == 0 && labelsAddOnlyEscalated(bead.Labels, in.Labels)
	default:
		return patchOnlyChangesFields(allowed, in, bead.Fields)
	}
}

// labelsAddOnlyEscalated reports whether next is the current label set plus
// "escalated", which the bead did not already carry: no label removed, no
// other label added.
func labelsAddOnlyEscalated(current, next []string) bool {
	if slices.Contains(current, reflagEscalatedLabel) || !slices.Contains(next, reflagEscalatedLabel) {
		return false
	}
	for _, l := range current {
		if !slices.Contains(next, l) {
			return false
		}
	}
	for _, l := range next {
		if l != reflagEscalatedLabel && !slices.Contains(current, l) {
			return false
		}
	}
	return true
}
