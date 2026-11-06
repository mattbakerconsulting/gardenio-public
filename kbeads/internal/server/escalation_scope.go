package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
)

// scopedApprovableOperations is the closed list of escalation operation_type
// values a scoped principal may approve or reject. true means the operation
// acts only inside the escalation's own project and needs no cluster, infra or
// secret access: create/update/close replay an ordinary bead write, which is
// re-checked against the scoped write rules (scopedReplayDenial) rather than
// run as a machine write. Everything else is false, and an operation_type
// absent from the map is denied, so a new type is off until someone decides it
// belongs here. Keep the table in the PR description in step with this map.
var scopedApprovableOperations = map[string]bool{
	"create": true,
	"update": true,
	"close":  true,

	// Taint grants widen what a principal may touch fleet-wide.
	"grant": false,
	// Merges, deploys and operator-run actions need access a project member
	// does not have; approving one is the privileged step itself.
	"gitlab_mr_merge":         false,
	"gitlab_merge":            false,
	"github_pr_merge":         false,
	"merge_window":            false,
	"helm_deploy":             false,
	"helm_deploy_values":      false,
	"terraform_apply":         false,
	"infrastructure":          false,
	"aws_iam":                 false,
	"credential_provisioning": false,
	"credential_rotation":     false,
	"vault_provisioning":      false,
	"create_role":             false,
	"manual_operator_action":  false,
	"general":                 false,
	"misc":                    false,
}

func normalizeOperationType(op string) string {
	return strings.ToLower(strings.TrimSpace(op))
}

// scopedApproverAllowed reports whether the caller may approve or reject esc.
// A caller that is not a scoped principal is always allowed (its own gates
// apply). A scoped principal needs all of: every project: label on the
// escalation in its token projects, with a tier there that may approve
// (actionTable), an operation_type on scopedApprovableOperations, and the
// escalation not spawning or re-roling an agent into a privileged role. reason
// is the 403 text when ok is false.
func (s *BeadsServer) scopedApproverAllowed(ctx context.Context, esc *model.Bead) (ok bool, reason string) {
	if !isScopedPrincipal(ctx) {
		return true, ""
	}
	if d := scopedProjectDenial(ctx, actionApprove, esc.Type, esc.Labels); d != nil {
		return false, d.Message
	}
	if d := scopedTierDenial(ctx, actionApprove, esc.Type, esc.Labels); d != nil {
		return false, d.Message
	}
	if d := scopedFieldsProjectDenial(ctx, actionApprove, esc.Type, esc.Fields); d != nil {
		return false, d.Message
	}
	// A frozen land's escalations wait for the thaw: the approved write would
	// replay as the server's own (withMachineWrite), past the freeze.
	if d := frozenLandDenial(ctx, s.store, actionApprove, esc.Type, esc.Labels); d != nil {
		return false, d.Message
	}
	f := parseSpawnEscalationFields(esc)
	if !scopedApprovableOperations[normalizeOperationType(f.OperationType)] && !landOwnerMergeApproval(ctx, esc, f.OperationType) && !s.landOwnerPeopleApproval(ctx, esc) {
		return false, "scoped principal may not resolve a " + strconvQuote(f.OperationType) + " escalation"
	}
	role, isSpawn, rerr := escalationSpawnTarget(f)
	if isSpawn {
		if rerr != nil {
			return false, "scoped principal may not resolve a spawn whose role cannot be resolved"
		}
		privileged := isNeverAutoApproveRole(role)
		if !privileged {
			var err error
			if privileged, err = roleIsPrivileged(ctx, s.store, role); err != nil {
				return false, "could not resolve whether the spawned role is privileged"
			}
		}
		if privileged {
			return false, "scoped principal may not resolve a spawn of a privileged role"
		}
	}
	return true, ""
}

// landOwnerMergeApproval is a carve-out from scopedApprovableOperations
// (escalation-routing D7 rule 2, Task 11): the owner of a tenant land may
// resolve a merge escalation in that land when the server-stamped approvers
// name land-owner:<land>. The merges stay false in the map, so a member, or an
// owner whose escalation does not name its land-owner lane (an operator
// project's merge never does: escalationFactsFor sets no land), is refused.
// The project, tier, fields and freeze checks run before it. The other
// carve-out is a land's own people (landOwnerPeopleApproval, P10.C).
func landOwnerMergeApproval(ctx context.Context, esc *model.Bead, opType string) bool {
	if !stewardDelegableMerges[normalizeOperationType(opType)] {
		return false
	}
	projects := projectLabelValues(esc.Labels)
	return len(projects) == 1 && landOwnerLaneApproval(ctx, esc, projects[0])
}

// landOwnerLaneApproval reports whether the caller is an owner of land and
// esc's stamped approvers name land-owner:<land>.
func landOwnerLaneApproval(ctx context.Context, esc *model.Bead, land string) bool {
	if scopeFromContext(ctx).Tiers[land] != tierOwner {
		return false
	}
	approvers, err := escalationApprovers(esc)
	if err != nil {
		return false
	}
	return slices.Contains(approvers, laneRef{Kind: laneLandOwner, Name: land}.String())
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// scopedReplayDenial vets, for a scoped approver, the write esc's
// operation_spec would replay, against the same scoped write rules a direct
// write by that principal meets (type allowlist, project labels, fields
// project, and the freeze of the land the write lands in). It returns "" when
// the write is allowed or the caller is not a scoped principal. A scoped
// approver's replay does not run as a machine write, so this is the early,
// readable refusal; the write path enforces it again. Without the freeze check
// here an approval into a frozen land was recorded, then its replay refused,
// leaving the escalation approved but stuck (gd-dMof1PW1oJ).
func (s *BeadsServer) scopedReplayDenial(ctx context.Context, esc *model.Bead) (string, error) {
	if !isScopedPrincipal(ctx) {
		return "", nil
	}
	f := parseSpawnEscalationFields(esc)
	switch normalizeOperationType(f.OperationType) {
	case "create":
		var spec createBeadInput
		if json.Unmarshal(f.OperationSpec, &spec) != nil {
			return "", nil
		}
		t := model.BeadType(spec.Type)
		if canonical, ok := model.TypeAliases[t]; ok {
			t = canonical
		}
		labels := labelsForAuthz(spec.Labels, spec.Fields)
		if d := scopedWriteDenial(ctx, "create", t, labels); d != nil {
			return d.Message, nil
		}
		if d := scopedFieldsProjectDenial(ctx, "create", t, spec.Fields); d != nil {
			return d.Message, nil
		}
		if d := frozenLandDenial(ctx, s.store, "create", t, labels); d != nil {
			return d.Message, nil
		}
	case "update", "close":
		action := normalizeOperationType(f.OperationType)
		var raw struct {
			TargetBeadID string `json:"target_bead_id"`
		}
		_ = json.Unmarshal(esc.Fields, &raw)
		if raw.TargetBeadID == "" {
			return "", nil
		}
		tb, err := s.store.GetBead(ctx, raw.TargetBeadID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && tb == nil) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if d := scopedWriteDenial(ctx, action, tb.Type, tb.Labels); d != nil {
			return d.Message, nil
		}
		if d := frozenLandDenial(ctx, s.store, action, tb.Type, tb.Labels); d != nil {
			return d.Message, nil
		}
		if action == "update" {
			var spec updateBeadInput
			if json.Unmarshal(f.OperationSpec, &spec) != nil {
				return "", nil
			}
			if spec.Labels != nil {
				if d := scopedWriteDenial(ctx, "update", tb.Type, spec.Labels); d != nil {
					return d.Message, nil
				}
				if d := frozenLandDenial(ctx, s.store, "update", tb.Type, spec.Labels); d != nil {
					return d.Message, nil
				}
			}
			if d := scopedFieldsProjectDenial(ctx, "update", tb.Type, spec.Fields); d != nil {
				return d.Message, nil
			}
		}
	}
	return "", nil
}
