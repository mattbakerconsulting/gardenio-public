package server

import "strings"

// landRole is a scoped principal's role in one land (project), Lands Commons
// spec §7.3 step 4.2 (gd-9aCcG1zCCY). It is carried by the realm group the
// token names, never by a bead field, for the same reason the project set is
// (scopeFromContext): a stale bead must not widen access.
//
//	/project-<land>          member
//	/project-<land>/owners   owner
//	/project-<land>/guests   guest
//
// Keycloak subgroups inherit the parent's realm roles, so an owner or guest
// holds gardenio-project-member through /project-<land> without being put in
// it directly. The values are ordered: a principal in several groups of one
// land gets the highest.
type landRole int

const (
	landRoleNone landRole = iota
	landRoleGuest
	landRoleMember
	landRoleOwner
)

// Subgroup names under /project-<land> that carry a land role other than
// member. The controller's twin is conversation.LandRolesFromGroups.
const (
	groupLandOwners = "owners"
	groupLandGuests = "guests"
)

// tier is the actionTable row (action_table.go) the role reads as. A caller
// with no role in a land has no row, which denies everything.
func (r landRole) tier() tier {
	switch r {
	case landRoleGuest:
		return tierGuest
	case landRoleMember:
		return tierMember
	case landRoleOwner:
		return tierOwner
	}
	return ""
}

func (r landRole) String() string {
	if t := r.tier(); t != "" {
		return string(t)
	}
	return "none"
}

// parseProjectGroup reads one group path. ok is false for a group that is not
// a project group. A subgroup this code does not know (/project-x/anything)
// is a guest: Keycloak still gives it the project-member role through the
// parent, and the least role is the safe reading of a name nobody decided on.
func parseProjectGroup(g string) (project string, role landRole, ok bool) {
	g = strings.TrimPrefix(g, "/")
	rest, ok := strings.CutPrefix(g, groupProjectPrefix)
	if !ok {
		return "", landRoleNone, false
	}
	project, sub, nested := strings.Cut(rest, "/")
	if project == "" {
		return "", landRoleNone, false
	}
	switch {
	case !nested:
		return project, landRoleMember, true
	case sub == groupLandOwners:
		return project, landRoleOwner, true
	case sub == groupLandGuests:
		return project, landRoleGuest, true
	default:
		return project, landRoleGuest, true
	}
}

// groupsToLandRoles maps each project the groups name to the caller's highest
// role in it.
func groupsToLandRoles(groups []string) map[string]landRole {
	roles := map[string]landRole{}
	for _, g := range groups {
		if p, r, ok := parseProjectGroup(g); ok && r > roles[p] {
			roles[p] = r
		}
	}
	return roles
}
