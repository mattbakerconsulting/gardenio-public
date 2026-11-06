package server

import (
	"context"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// The home steward re-arms a plan's own sealed gardener (gd-miuZqFACwL, owner
// 11-04; row steward-rearm in the grant table).
//
// When the engine closes a plan's gardener (a spent budget, a hard stop) the
// trigger keeps its seal: closing only removes authority. Before this the home
// steward could not reopen it (it does not own the owner's trigger), and any
// other caller's reopen strips the seal (voidSpawnLineageOnReactivation), so a
// stalled plan got a freshly minted trigger or an owner escalation. A re-arm
// buys nothing new: it is the trigger the owner sealed, with the same spawn
// content (the digest still holds), running the same plan. So the update path
// admits it, and the seal is kept with its original author, when ALL hold:
//
//   - the caller is the home garden's steward in the trigger's project
//     (landsTenantCallerFrom: Home). The owner's own OIDC session needs no
//     row: its reopen re-stamps the seal (resealSpawnLineage);
//   - the patch reopens the trigger and turns it on, and does nothing else:
//     status open, enabled true, no other column or field;
//   - the trigger is closed, spawns the plan-gardener role in its labelled
//     project and is pinned to a plan by gardener-epic:<plan>;
//   - the engine closed it: it still carries a terminate marker
//     (gardener_terminated_kind/slot or gardener_stuck_escalated). The
//     engine's operator-reopen normalization (bridge
//     gardenerRearmFromOperatorReopen) keys on those markers to refill the run
//     budget and zero the resume counters, and the patch cannot clear them;
//   - its seal is valid and within its TTL;
//   - the plan is an epic labelled plan in that project with
//     plan_state=building, and no other open trigger for it is sealed (one
//     lane per plan).
//
// The row can never create a seal, change who it names, or touch spawn
// content.

const stewardRearmGrant = "steward-rearm"

// stewardRearmFields are the only fields the row lets the steward write.
var stewardRearmFields = []string{"enabled"}

// gardenerTerminateMarkers are the fields the engine stamps only in the write
// that closes a gardener (bridge gardener_operator_reopen.go).
var gardenerTerminateMarkers = []string{"gardener_terminated_kind", "gardener_terminated_slot", "gardener_stuck_escalated"}

// homeStewardMayRearm reports whether the patch is the home steward re-arming
// an engine-closed plan gardener (file header). bead is the trigger before the
// patch.
func (s *BeadsServer) homeStewardMayRearm(ctx context.Context, bead *model.Bead, in *updateBeadInput) bool {
	if bead == nil || bead.Status != model.StatusClosed || !isRearmPatch(in) {
		return false
	}
	return s.homeStewardRearmsPlanGardener(ctx, bead)
}

// homeStewardRearmsPlanGardener is the bead and caller side of the row: the
// caller is the home steward and bead an engine-closed, validly sealed
// plan-gardener of a building plan in its project.
func (s *BeadsServer) homeStewardRearmsPlanGardener(ctx context.Context, bead *model.Bead) bool {
	if bead == nil || bead.Status != model.StatusClosed || !isAutonomousBeadType(bead.Type) ||
		triggerSpawnRole(bead) != planGardenerSpawnRole || !carriesTerminateMarker(bead) {
		return false
	}
	seal := validSpawnSeal(bead)
	if seal == nil || !s.spawnSealFresh(seal) {
		return false
	}
	planID := gardenerEpicFromLabels(bead.Labels)
	project := projectFromLabelsOrEmpty(bead.Labels)
	if planID == "" || project == "" || strings.TrimSpace(fieldsStringValue(bead.Fields, "project")) != project {
		return false
	}
	caller, err := landsTenantCallerFrom(ctx, s.store)
	if err != nil || caller == nil || !caller.Home || !caller.isSteward() || caller.Project != project {
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
	slog.Info("plan gardener re-armed by the home steward, seal kept",
		"trigger", bead.ID, "plan", planID, "author", seal.Author, "agent", caller.AgentBeadID)
	return true
}

// isRearmPatch reports whether in sets status open and enabled true, and
// nothing else.
func isRearmPatch(in *updateBeadInput) bool {
	if in == nil || in.Status == nil || model.Status(*in.Status) != model.StatusOpen {
		return false
	}
	if in.Title != nil || in.ShortTitle != nil || in.Heraldry != nil || in.Description != nil || in.Notes != nil ||
		in.Priority != nil || in.Assignee != nil || in.Owner != nil || in.dueAtSet || in.deferUntilSet || in.labelsSet {
		return false
	}
	patch := fieldsAsMap(in.Fields)
	if len(patch) != len(stewardRearmFields) {
		return false
	}
	v, ok := patch["enabled"]
	return ok && (v == true || v == "true")
}

func carriesTerminateMarker(bead *model.Bead) bool {
	f := parseFieldsToStringMap(bead.Fields)
	for _, k := range gardenerTerminateMarkers {
		if v := strings.TrimSpace(f[k]); v != "" && v != "false" {
			return true
		}
	}
	return false
}
