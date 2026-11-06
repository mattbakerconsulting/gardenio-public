package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// The escalation-executor grant (escalation routing Task 15, C4; owner
// decision gd-B71zgAalvu option a; no blanket machine ownership, decision
// gd-ZsfxvXRiyv). coordinator:cluster executes owner-approved bead-only
// escalations as a machine principal that owns none of the beads it writes, so
// its authority is scoped to one escalation at a time:
//
//   - approved_targets: the first approval stamps the beads the escalation
//     names in its immutable fields (operation_spec.bead_ids, target_bead_id;
//     escalationCloseBeadIDs) that are executor types in the escalation's own
//     project. Those fields cannot change after filing, so the stamp is what
//     the approver saw named; editable text (description, context, comments)
//     names nothing. It is server-only and frozen, and a later approve never
//     re-stamps it.
//   - a bundle (escalationCloseOnlyTypes) is stamped only by a bead-close
//     escalation, and the grant admits only its close (gd-dNYrSP9KkW).
//   - the grant: a principal holding gardenio-machine-escalation-executor may
//     update or close (and so label, link and unlink) bead B it does not own,
//     only while it holds the claim on an open, server-approved, bead-only,
//     non-floor escalation whose execute_by is a coordinator lane it is a
//     member of, whose approved_targets lists B, and whose project is B's.
//
// Comments need no grant: comment create does not run the ownership gate.

// approvedTargetsField is the escalation field the approve paths stamp.
const approvedTargetsField = "approved_targets"

// escalationProject is the one project an escalation or target is labelled
// with, or "" for none or several (which match nothing).
func escalationProject(labels []string) string {
	var p string
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" {
			if p != "" && p != v {
				return ""
			}
			p = v
		}
	}
	return p
}

// escalationApprovedTargets returns the sorted ids to stamp as approved_targets
// at esc's first approval: the ids its immutable fields name that exist, are an
// executor type (or, for an advice ask, advice; isStewardAdviceTarget) and
// carry esc's one project. A lookup error drops that id, so
// the grant admits less, never more (fail closed).
func (s *BeadsServer) escalationApprovedTargets(ctx context.Context, esc *model.Bead) []string {
	if s == nil || s.store == nil || esc == nil {
		return nil
	}
	project := escalationProject(esc.Labels)
	if project == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range escalationCloseBeadIDs(esc.Fields) {
		if id == esc.ID || seen[id] {
			continue
		}
		seen[id] = true
		b, err := s.store.GetBead(ctx, id)
		if err != nil || b == nil {
			slog.Warn("escalation approve: named bead unreadable; not an approved target", "escalation", esc.ID, "bead", id, "error", err)
			continue
		}
		if (escalationExecutorTypes[b.Type] || (escalationCloseOnlyTypes[b.Type] && isBeadCloseEscalation(esc)) ||
			isStewardAdviceTarget(esc, b, project)) && escalationProject(b.Labels) == project {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// isBeadCloseEscalation reports whether esc's operation_type, read by its
// exact key, is a bead-close type.
func isBeadCloseEscalation(esc *model.Bead) bool {
	op, _, err := exactFieldString(esc.Fields, "operation_type")
	return err == nil && beadCloseOperationTypes[normalizeOperationType(op)]
}

// escalationApprovedTargetIDs reads the approved_targets stamp by its exact
// key. Anything but a list of strings names nothing.
func escalationApprovedTargetIDs(fields json.RawMessage) []string {
	var m map[string]json.RawMessage
	if json.Unmarshal(fields, &m) != nil {
		return nil
	}
	var ids []string
	if json.Unmarshal(m[approvedTargetsField], &ids) != nil {
		return nil
	}
	return ids
}

// escalationStampedOperatorOnly reports the frozen operator_only stamp (D3's
// floor, stamped at create from escalationForcedReason). A floor operation
// type is never bead-only, so this is a second fence, not the first.
func escalationStampedOperatorOnly(esc *model.Bead) bool {
	for _, v := range escalationFieldValues(esc.Fields, []string{"operator_only"}) {
		if b, ok := v.(bool); ok && b {
			return true
		}
	}
	return false
}

// openEscalationStatuses are the statuses of an escalation that is not closed.
var openEscalationStatuses = []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred}

// escalationExecutorGrant returns the id of the escalation whose claim lets the
// caller write bead under the escalation-executor grant, or "" when none does.
// Every input is read server-side: the claim from claimed_by (written only by
// the claim endpoints: the caller's own, or an agent of its machine role,
// claimHeldByCaller), the approval from approved_at, the lane
// from execute_by and the policy. An error is returned so the condition counts
// it, and denies.
func (s *BeadsServer) escalationExecutorGrant(ctx context.Context, bead *model.Bead) (string, error) {
	if bead == nil || !(escalationExecutorTypes[bead.Type] || escalationCloseOnlyTypes[bead.Type]) || s == nil || s.store == nil {
		return "", nil
	}
	project := escalationProject(bead.Labels)
	if project == "" {
		return "", nil
	}
	actor := laneActor(ctx)
	if actor == "" {
		return "", nil
	}
	escs, err := s.callerClaimCandidates(ctx, actor)
	if err != nil {
		return "", fmt.Errorf("list claimed escalations: %w", err)
	}
	var policy *escalationPolicy
	for _, esc := range escs {
		if esc == nil || esc.Type != model.TypeEscalation || !slices.Contains(openEscalationStatuses, esc.Status) {
			continue
		}
		claimed, _, _ := exactFieldString(esc.Fields, "claimed_by")
		if held, err := s.claimHeldByCaller(ctx, claimed); err != nil {
			return "", err
		} else if !held {
			continue
		}
		if !escalationHasServerApproval(esc) || !slices.Contains(escalationApprovedTargetIDs(esc.Fields), bead.ID) ||
			escalationProject(esc.Labels) != project {
			continue
		}
		if op, _, _ := exactFieldString(esc.Fields, "operation_type"); !isBeadOnlyOperation(op) || escalationStampedOperatorOnly(esc) {
			continue
		}
		if escalationCloseOnlyTypes[bead.Type] && !isBeadCloseEscalation(esc) {
			continue
		}
		execBy, _, _ := exactFieldString(esc.Fields, "execute_by")
		lane, err := parseLaneRef(strings.TrimSpace(execBy))
		if err != nil || lane.Kind != laneCoordinator {
			continue
		}
		if policy == nil {
			p, err := s.activeEscalationPolicy(ctx)
			if err != nil {
				return "", fmt.Errorf("escalation policy: %w", err)
			}
			policy = &p
		}
		in, err := s.callerInLane(ctx, lane, esc, *policy)
		if err != nil {
			return "", fmt.Errorf("lane %s membership: %w", lane.String(), err)
		}
		if in {
			return esc.ID, nil
		}
	}
	return "", nil
}
