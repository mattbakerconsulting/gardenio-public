package server

import (
	"context"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/model"
)

// stampedPolicyRule is the rule of p named by esc's policy_rule
// ("escalation#<i>"), or nil when esc names no rule of p.
func stampedPolicyRule(esc *model.Bead, p escalationPolicy) *escalationPolicyRule {
	rule, _, _ := exactFieldString(esc.Fields, "policy_rule")
	n, ok := strings.CutPrefix(rule, "escalation#")
	if !ok {
		return nil
	}
	i, err := strconv.Atoi(n)
	if err != nil || i < 1 || i > len(p.Rules) {
		return nil
	}
	return &p.Rules[i-1]
}

// policyLaneMayReject is D6(f) (gd-Gi140TvkbN): a member of a coordinator lane
// named by esc's stamped policy rule (its hold_by or approvers) may reject esc
// by recording a failure, while it has no approval and is not on the floor. A
// failure executes nothing, so this grants no approve authority, and it is the
// only reject a coordinator has: /reject is operator-only. A spawn ask carries
// its spawn_policy rule's approvers (the owner) and is held by the owner lane,
// so the rule is read, not held_by. A merge's send-back stays D6(e), held only.
func (s *BeadsServer) policyLaneMayReject(ctx context.Context, esc *model.Bead) bool {
	if operationClassOf(parseSpawnEscalationFields(esc).OperationType) == "merge" {
		return false
	}
	if escalationHasServerApproval(esc) || s.escalationOperatorOnly(ctx, esc) {
		return false
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		return false
	}
	// The rule must still match esc: a policy rewrite renumbers its rules.
	rule := stampedPolicyRule(esc, p)
	if rule == nil || !rule.When.matches(s.escalationFactsFor(ctx, esc)) {
		return false
	}
	for _, ref := range append([]string{rule.HoldBy}, rule.Approvers...) {
		lane, err := parseLaneRef(ref)
		if err != nil || lane.Kind != laneCoordinator {
			continue
		}
		if in, err := s.callerInLane(ctx, lane, esc, p); err == nil && in {
			return true
		}
	}
	return false
}
