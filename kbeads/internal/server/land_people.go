package server

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"gardenio/kbeads/internal/model"
)

// A land owner runs their own land's people (P10.C gd-wi44L8Qy4I, owner
// ruling gd-CJUlVSMzEe). keycloak_group_membership is on the D3 owner floor
// for global roles and gardenio's own groups, but an escalation that only
// adds or removes people in one tenant land's own groups is not: it is routed
// to land-owner:<land> in code beside the floor (escalationRouteFieldsFor,
// policy_rule landPeopleRule), and an owner of that land approves it on the
// page, like the land's merges. Anything this file cannot read as exactly
// that stays on the floor.

const (
	landPeopleOperation = "keycloak_group_membership"
	landPeopleRule      = "land:people"
)

// landPeopleSpecKeys are the only operation_spec keys a land-people
// escalation may carry. Any other key (realm_roles, client_roles, a group
// path under another name) could reach past the land, so it keeps the
// escalation on the floor.
var landPeopleSpecKeys = map[string]bool{"group": true, "groups": true, "user": true, "users": true, "action": true, "reason": true}

// landSubgroupSegment is one subgroup under /project-<land> (owners, guests),
// as groupsToLandRoles reads them.
var landSubgroupSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// escalationLandPeople returns the land when esc is a keycloak_group_membership
// escalation in exactly one tenant land whose operation_spec names only that
// land's groups (/project-<land> or /project-<land>/<subgroup>), else "". A
// lookup error reads as "" (the floor).
func (s *BeadsServer) escalationLandPeople(ctx context.Context, esc *model.Bead) string {
	if esc == nil || esc.Type != model.TypeEscalation {
		return ""
	}
	f := parseSpawnEscalationFields(esc)
	if canonicalOperationType(normalizeOperationType(f.OperationType)) != landPeopleOperation {
		return ""
	}
	projects := projectLabelValues(esc.Labels)
	if len(projects) != 1 {
		return ""
	}
	land := projects[0]
	if p := projectFromFields(esc.Fields); p != "" && p != land {
		return ""
	}
	if !landPeopleSpec(land, f.OperationSpec) {
		return ""
	}
	if tenant, err := s.isTenantLand(ctx, land); err != nil || !tenant {
		return ""
	}
	return land
}

// landPeopleSpec reports whether raw (an object, or a JSON string holding
// one) names at least one group, every group is land's own, action is add or
// remove when given, and no key is outside landPeopleSpecKeys.
func landPeopleSpec(land string, raw json.RawMessage) bool {
	var spec map[string]any
	if json.Unmarshal(raw, &spec) != nil {
		var inner string
		if json.Unmarshal(raw, &inner) != nil || json.Unmarshal([]byte(inner), &spec) != nil {
			return false
		}
	}
	var groups []any
	for k, v := range spec {
		if !landPeopleSpecKeys[k] {
			return false
		}
		switch k {
		case "group":
			groups = append(groups, v)
		case "groups":
			list, ok := v.([]any)
			if !ok {
				return false
			}
			groups = append(groups, list...)
		case "action":
			if a, _ := v.(string); a != "add" && a != "remove" {
				return false
			}
		}
	}
	if len(groups) == 0 {
		return false
	}
	for _, g := range groups {
		path, _ := g.(string)
		if !landOwnGroup(land, path) {
			return false
		}
	}
	return true
}

// landOwnGroup reports whether path is /project-<land> or one subgroup of it.
func landOwnGroup(land, path string) bool {
	base := "/project-" + land
	if path == base {
		return true
	}
	sub, ok := strings.CutPrefix(path, base+"/")
	return ok && landSubgroupSegment.MatchString(sub)
}

// landOwnerPeopleApproval is landOwnerMergeApproval's twin for a land-people
// escalation: an owner of its land, with land-owner:<land> in the stamped
// approvers, may resolve it although keycloak_group_membership is not on
// scopedApprovableOperations.
func (s *BeadsServer) landOwnerPeopleApproval(ctx context.Context, esc *model.Bead) bool {
	land := s.escalationLandPeople(ctx, esc)
	return land != "" && landOwnerLaneApproval(ctx, esc, land)
}
