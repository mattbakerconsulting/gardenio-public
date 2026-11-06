package server

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Dispatched-task writes (gd-GrJRP7kLBY, programme step P-G1; escalation
// gd-UgoSte3qGx, decision gd-pjrfJOLdiY).
//
// A crew dispatched with `gb spawn --task B` is often none of B's created_by,
// assignee or owner: a thread or another crew filed B, or a send-back cleared
// its assignee. Every kd claim, kd update, kd dep add and kd close it made on
// B was an ownership 403. Two grants close that gap:
//
//   - the row dispatched-task: a per-agent token whose open agent bead names B
//     in task_id may update and close B, and the direct children of B when B is
//     itself a work item, in the token's project, while the bead written is
//     unassigned, held by the caller or held by a gone agent. task_id is fixed
//     after spawn for an agent token (planScopeBindingKeys), but any agent may
//     spawn a crew onto any task in its project, so the assignee test is what
//     keeps a crew off a task a live agent or a human holds;
//   - the controller's DispatchClaim (machine_grants.go): at pod create it may
//     claim an open, unassigned bug, feature or chore for the one live agent
//     whose spawn opted into single-claim on it (machineMayClaimDispatchedTask).
//     A task is already in its Ownership.

// dispatchedTaskTypes are the work items a crew is dispatched onto. An epic is
// left to the plan-scope rule, so a crew pinned to a phase gains no steps here.
var dispatchedTaskTypes = []model.BeadType{model.TypeBug, model.TypeChore, model.TypeFeature, model.TypeTask}

// callerDispatchedOnto reports whether the caller's per-agent token belongs to
// an open agent bead whose task_id is bead, or is the work-item parent of bead,
// with the token's, the agent's and the bead's project all the same, and bead
// is unassigned, assigned to the caller, or assigned to a gone agent.
func (s *BeadsServer) callerDispatchedOnto(ctx context.Context, bead *model.Bead) (bool, error) {
	project := TokenProjectFromContext(ctx)
	if project == "" || projectFromLabelsOrEmpty(bead.Labels) != project {
		return false, nil
	}
	if a := bead.Assignee; a != "" && a != AuthIdentityFromContext(ctx).Subject && !s.assigneeIsGoneAgent(ctx, a) {
		return false, nil
	}
	agent, err := s.callerAgentBead(ctx)
	if err != nil || agent == nil || projectFromFields(agent.Fields) != project {
		return false, err
	}
	taskID := strings.TrimSpace(fieldsStringValue(agent.Fields, "task_id"))
	if taskID == "" {
		return false, nil
	}
	if bead.ID == taskID {
		return true, nil
	}
	deps, err := s.store.GetDependencies(ctx, bead.ID)
	if err != nil {
		return false, err
	}
	for _, d := range deps {
		if d == nil || d.DependsOnID != taskID || !model.IsParentChildDepType(string(d.Type)) {
			continue
		}
		parent, err := s.store.GetBead(ctx, taskID)
		if err != nil || parent == nil {
			return false, err
		}
		return slices.Contains(dispatchedTaskTypes, parent.Type) && projectFromLabelsOrEmpty(parent.Labels) == project, nil
	}
	return false, nil
}

// machineMayClaimDispatchedTask reports whether the request's machine principal
// may apply this update under a DispatchClaim grant: the patch is exactly a
// claim (status in_progress and an agent assignee, optionally clearing
// reaped_assignee) and dispatchClaimHeld holds for that agent.
func (s *BeadsServer) machineMayClaimDispatchedTask(ctx context.Context, bead *model.Bead, in *updateBeadInput) bool {
	if bead == nil || authMethodFromContext(ctx) != authMethodMachine {
		return false
	}
	granted := slices.ContainsFunc(machineRolesFromContext(ctx), func(role string) bool {
		return slices.Contains(machineCapabilityGrants[role].DispatchClaim, bead.Type)
	})
	if !granted {
		return false
	}
	agentName, ok := dispatchClaimPatch(in)
	return ok && s.dispatchClaimHeld(ctx, bead, agentName)
}

// dispatchClaimPatch returns the assignee of a patch that is exactly what the
// controller's claim at pod create writes, and false for any other patch.
func dispatchClaimPatch(in *updateBeadInput) (string, bool) {
	if in.Status == nil || model.Status(*in.Status) != model.StatusInProgress || in.Assignee == nil {
		return "", false
	}
	if in.Title != nil || in.ShortTitle != nil || in.Heraldry != nil || in.Description != nil || in.Notes != nil ||
		in.Priority != nil || in.Owner != nil || in.dueAtSet || in.deferUntilSet || in.labelsSet {
		return "", false
	}
	if len(in.Fields) > 0 {
		var patch map[string]any
		if err := json.Unmarshal(in.Fields, &patch); err != nil {
			return "", false
		}
		for k, v := range patch {
			if k != reapedAssigneeField || v != nil {
				return "", false
			}
		}
	}
	name := strings.TrimSpace(*in.Assignee)
	if name == "" || strings.Contains(name, "@") {
		return "", false
	}
	return name, true
}

// dispatchClaimHeld is the bead side of a dispatch claim: bead is open with no
// assignee, and agentName's live agent bead names bead as its task_id, opted
// into single-claim, and is in bead's project.
func (s *BeadsServer) dispatchClaimHeld(ctx context.Context, bead *model.Bead, agentName string) bool {
	if bead.Status != model.StatusOpen || bead.Assignee != "" {
		return false
	}
	project := projectFromLabelsOrEmpty(bead.Labels)
	if project == "" {
		return false
	}
	agent, err := s.store.FindAgentByName(ctx, agentName, activeAgentStatuses)
	if err != nil || agent == nil || agent.Type != model.BeadType("agent") {
		return false
	}
	claim := strings.TrimSpace(fieldsStringValue(agent.Fields, "claim_task_at_spawn"))
	return strings.TrimSpace(fieldsStringValue(agent.Fields, "task_id")) == bead.ID &&
		(claim == "true" || claim == "1") && projectFromFields(agent.Fields) == project
}
