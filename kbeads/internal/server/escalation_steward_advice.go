package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
)

// The home steward writes its project's advice (owner pick on decision
// gd-c0b7m4dErB, bug gd-kEis8bURm5; row steward-advice in the grant table).
// An advice ask for a project goes to that project's steward lane, and the
// home garden's steward holds, approves and executes it:
//
//   - lane: the home garden's steward (landsTenantCallerFrom: Home, so role
//     exactly steward, its project the home garden, read from the project
//     bead the server holds) is a member of steward:<its project> for an
//     advice-class escalation (kbeads_advice_write and its aliases), and that
//     lane is staffed while the project is a home garden. A tenant steward
//     keeps land_steward_approves for every class (landDelegatesToSteward).
//   - write: while it holds the claim on an open, server-approved, off-floor,
//     non-operator-only advice escalation labelled with its project whose
//     execute_by is steward:<its project>, it may update or close an advice
//     bead of that project that the escalation's approved_targets lists. The
//     approval stands in for advice's min_role and for ownership; every other
//     write gate still applies. Creating advice needs no grant: every agent
//     token may create it (machineAllowedWrites advice/create).
//   - never: global advice (no project label, several, or the global label),
//     another project's advice, a label change that takes advice out of its
//     project, a hook_* field (a hook runs in every session the advice
//     matches), or any other bead type through this path.
//
// A steward that is the requester cannot approve its own ask (D4,
// checkCoordinatorMayApprove), so its own ask needs another approver lane.

const stewardAdviceGrant = "steward-advice"

// stewardAdviceActions are the writes the grant admits.
var stewardAdviceActions = map[string]bool{"update": true, "close": true}

func isAdviceOperation(op string) bool { return operationClassOf(op) == "advice" }

// projectAdviceLabels reports whether labels scope an advice bead to project
// alone: one project, project:<project>, no global label, and every group
// selector (g<n>:...) inside that project.
func projectAdviceLabels(labels []string, project string) bool {
	if project == "" || escalationProject(labels) != project || hasLabel(labels, "global") {
		return false
	}
	for _, l := range labels {
		inner := model.StripGroupPrefix(l)
		if inner == l {
			continue
		}
		if p, ok := strings.CutPrefix(inner, "project:"); inner == "global" || (ok && p != project) {
			return false
		}
	}
	return true
}

// homeStewardProject returns the caller's project when it is the home garden's
// steward, else "". A lookup error is returned so the caller fails closed.
func (s *BeadsServer) homeStewardProject(ctx context.Context) (string, error) {
	if s == nil || s.store == nil {
		return "", nil
	}
	caller, err := landsTenantCallerFrom(ctx, s.store)
	if err != nil || caller == nil || !caller.Home || !caller.isSteward() {
		return "", err
	}
	return caller.Project, nil
}

// homeStewardAdviceLaneMember reports whether the caller, already checked to
// be a steward agent of project, is in steward:<project> for esc as the home
// garden's steward: esc is an advice ask and the caller is the home steward
// of project.
func (s *BeadsServer) homeStewardAdviceLaneMember(ctx context.Context, project string, esc *model.Bead) (bool, error) {
	if !isAdviceOperation(parseSpawnEscalationFields(esc).OperationType) {
		return false, nil
	}
	home, err := s.homeStewardProject(ctx)
	return err == nil && home != "" && home == project, err
}

// homeStewardAdviceLaneStaffed reports whether steward:<project> is staffed for
// esc by a home garden's steward: esc is an advice ask and project is a home
// garden.
func (s *BeadsServer) homeStewardAdviceLaneStaffed(ctx context.Context, project string, esc *model.Bead) (bool, error) {
	if !isAdviceOperation(parseSpawnEscalationFields(esc).OperationType) {
		return false, nil
	}
	owner, err := homeGardenOperator(ctx, s.store, project)
	return err == nil && owner != "", err
}

// isStewardAdviceTarget reports whether b may be stamped into esc's
// approved_targets as advice: esc is an advice ask and b is advice of esc's
// one project. Only the steward-advice grant reads an advice target; the
// escalation-executor grant reaches executor types only.
func isStewardAdviceTarget(esc, b *model.Bead, project string) bool {
	return b.Type == model.TypeAdvice && isAdviceOperation(parseSpawnEscalationFields(esc).OperationType) &&
		projectAdviceLabels(b.Labels, project)
}

// anyApprovedTarget stands for the bead in the write-policy check, which sees
// only the labels; the ownership row then names the bead.
const anyApprovedTarget = "*"

// stewardAdviceEscalation returns the id of the escalation that lets the caller
// write advice bead beadID, labelled labels, as action, or "" when none does
// (file header). approved_targets must list beadID (anyApprovedTarget: any).
// Every input is read server-side: the claim from claimed_by, the approval
// from approved_at, the lane from execute_by.
func (s *BeadsServer) stewardAdviceEscalation(ctx context.Context, action string, labels []string, beadID string) (string, error) {
	if !stewardAdviceActions[action] || beadID == "" || s == nil || s.store == nil {
		return "", nil
	}
	project, err := s.homeStewardProject(ctx)
	if err != nil || project == "" || !projectAdviceLabels(labels, project) {
		return "", err
	}
	actor := laneActor(ctx)
	escs, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeEscalation},
		Status: openEscalationStatuses,
		Fields: map[string]string{"claimed_by": actor},
	})
	if err != nil {
		return "", fmt.Errorf("list claimed escalations: %w", err)
	}
	for _, esc := range escs {
		if esc == nil || esc.Type != model.TypeEscalation || !slices.Contains(openEscalationStatuses, esc.Status) {
			continue
		}
		if claimed, _, _ := exactFieldString(esc.Fields, "claimed_by"); claimed != actor {
			continue
		}
		if !escalationHasServerApproval(esc) || escalationProject(esc.Labels) != project || escalationStampedOperatorOnly(esc) {
			continue
		}
		if op, _, _ := exactFieldString(esc.Fields, "operation_type"); !isAdviceOperation(op) || escalationFloorOperation(normOp(op)) != "" {
			continue
		}
		execBy, _, _ := exactFieldString(esc.Fields, "execute_by")
		if lane, err := parseLaneRef(strings.TrimSpace(execBy)); err != nil || lane.Kind != laneSteward || lane.Name != project {
			continue
		}
		if beadID != anyApprovedTarget && !slices.Contains(escalationApprovedTargetIDs(esc.Fields), beadID) {
			continue
		}
		return esc.ID, nil
	}
	return "", nil
}

// stewardAdviceWriteHeld is the write-policy side of the grant, which stands
// in for advice's min_role: the caller may update or close advice labelled
// labels under an advice escalation it holds. The steward-advice ownership row
// then holds the write to that escalation's approved_targets.
func (s *BeadsServer) stewardAdviceWriteHeld(ctx context.Context, action string, beadType model.BeadType, labels []string) bool {
	if beadType != model.TypeAdvice {
		return false
	}
	id, err := s.stewardAdviceEscalation(ctx, action, labels, anyApprovedTarget)
	if err != nil || id == "" {
		return false
	}
	slog.Info("advice write by the home steward under an approved advice escalation",
		"action", action, "escalation", id, "agent", laneActor(ctx))
	return true
}

// stewardAdviceLabelsDenial refuses the home steward a label set that takes an
// advice bead out of its project (after: the labels the write leaves). Any
// other caller or type passes; their labels are gated elsewhere.
func (s *BeadsServer) stewardAdviceLabelsDenial(ctx context.Context, bead *model.Bead, after []string) *AuthzDenial {
	if bead == nil || bead.Type != model.TypeAdvice || isMachineWrite(ctx) {
		return nil
	}
	project, err := s.homeStewardProject(ctx)
	if err != nil || project == "" || projectAdviceLabels(after, project) {
		return nil
	}
	return &AuthzDenial{
		Action:   "update",
		BeadType: string(bead.Type),
		Message:  "the home steward writes only its own project's advice: the labels must keep project:" + project + " and add no global or other project label",
	}
}

// stewardAdviceLabelChangeDenial is stewardAdviceLabelsDenial for the
// single-label routes, which do not say whether label is added or removed:
// the home steward may change neither global nor a project label on advice,
// in a group or not.
func (s *BeadsServer) stewardAdviceLabelChangeDenial(ctx context.Context, target *model.Bead, label string) *AuthzDenial {
	if inner := model.StripGroupPrefix(label); inner != "global" && !strings.HasPrefix(inner, "project:") {
		return nil
	}
	return s.stewardAdviceLabelsDenial(ctx, target, []string{"global"})
}

// stewardAdviceHookDenial refuses the home steward an advice patch that sets
// any hook_* field: an advice hook runs a command in every session the advice
// matches, which an advice ask does not buy. Any other caller or type passes.
func (s *BeadsServer) stewardAdviceHookDenial(ctx context.Context, bead *model.Bead, patch json.RawMessage) *AuthzDenial {
	if bead == nil || bead.Type != model.TypeAdvice || len(patch) == 0 || isMachineWrite(ctx) {
		return nil
	}
	hook := ""
	for k := range fieldsAsMap(patch) {
		if strings.HasPrefix(strings.ToLower(k), "hook_") {
			hook = k
			break
		}
	}
	if hook == "" {
		return nil
	}
	if project, err := s.homeStewardProject(ctx); err != nil || project == "" {
		return nil
	}
	return &AuthzDenial{
		Action:   "update",
		BeadType: string(bead.Type),
		Message:  "the home steward may not set " + hook + " on advice: a hook runs in every session the advice matches; ask the owner",
	}
}
