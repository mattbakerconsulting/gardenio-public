package server

import (
	"context"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Seal a plan's gardener the home garden's steward binds (owner, 11-02).
//
// The home steward keeps the home garden's building plans running: when a
// plan's gardener stops (a budget, a pause, refused spawns) it binds a fresh one
// with gb plan gardener, and when one is still timed it switches it to event
// mode. It writes those triggers on its own agent token, so stampSpawnLineage
// never seals them, and every fire of the privileged plan-gardener role would
// 403 into an escalation the owner has to answer: the asks the owner called
// superfluous. The owner already stands behind every building plan of the home
// garden (plan_state building is their Approve & build), and the home garden is
// theirs (its project bead's operated_by), so the server seals such a trigger
// to that owner when ALL hold:
//
//   - the caller is the home garden's steward (landsTenantCallerFrom: role
//     exactly steward, its project the home garden, read from the project bead
//     the server holds, never a header);
//   - the trigger is open, enabled, spawns the plan-gardener role in the
//     steward's project, and is pinned to a plan by its gardener-epic:<plan>
//     label;
//   - the plan is an epic labelled plan in that project with
//     plan_state=building;
//   - no other open trigger for the plan carries a valid seal, so the steward
//     keeps one gardener lane per plan, as an approval buys one.
//
// What the steward can buy is the plan-gardener role, in its own land, for a
// plan the owner approved: nothing a tenant steward, another agent or another
// project can reach.

// stampSpawnLineageFromHomeSteward seals bead to the home garden's owner when
// the conditions in the file header hold. It must run on the final merged
// fields.
func (s *BeadsServer) stampSpawnLineageFromHomeSteward(ctx context.Context, bead *model.Bead) bool {
	if !isAutonomousBeadType(bead.Type) || isOIDCAdminSession(ctx) || validSpawnSeal(bead) != nil {
		return false
	}
	if bead.Status == model.StatusClosed || !effectiveGardenerEnabled(bead.Type, bead.Fields) {
		return false
	}
	if triggerSpawnRole(bead) != planGardenerSpawnRole {
		return false
	}
	caller, err := landsTenantCallerFrom(ctx, s.store)
	if err != nil || caller == nil || !caller.Home || caller.HomeOwner == "" {
		return false
	}
	planID := gardenerEpicFromLabels(bead.Labels)
	project := projectFromLabelsOrEmpty(bead.Labels)
	if planID == "" || project == "" || project != caller.Project ||
		strings.TrimSpace(fieldsStringValue(bead.Fields, "project")) != project {
		return false
	}
	plan, err := s.store.GetBead(ctx, planID)
	if err != nil || plan == nil || plan.Type != model.TypeEpic || !hasLabel(plan.Labels, "plan") ||
		projectFromLabelsOrEmpty(plan.Labels) != project || planStateFromFields(plan.Fields) != planStateBuilding {
		return false
	}
	if s.planHasSealedGardener(ctx, planID, bead.ID) {
		return false
	}
	if !s.sealSpawnLineageAs(bead, caller.HomeOwner) {
		return false
	}
	slog.Info("spawn lineage: plan gardener sealed for the home steward",
		"trigger", bead.ID, "plan", planID, "author", caller.HomeOwner, "agent", caller.AgentBeadID)
	return true
}
