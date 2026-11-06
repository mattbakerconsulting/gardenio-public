package conversation

import "strings"

// LandRole is a person's role in one land (design section 7.3 step 4.2). It
// comes from the realm groups on the person's verified token, the same groups
// kbeads reads (kbeads/internal/server/land_role.go), so the Commons and the
// daemon agree on who may do what:
//
//	/project-<land>          member
//	/project-<land>/owners   owner
//	/project-<land>/guests   guest
//
// An admin principal is not confined to lands; callers give it
// LandRoleOperator.
type LandRole string

const (
	LandRoleNone   LandRole = ""
	LandRoleGuest  LandRole = "guest"
	LandRoleMember LandRole = "member"
	LandRoleOwner  LandRole = "owner"
	// LandRoleOperator is an admin principal acting in any land. It is never
	// read from a group.
	LandRoleOperator LandRole = "operator"
)

// rank orders roles; an unknown role ranks with none.
func (r LandRole) rank() int {
	switch r {
	case LandRoleGuest:
		return 1
	case LandRoleMember:
		return 2
	case LandRoleOwner:
		return 3
	case LandRoleOperator:
		return 4
	}
	return 0
}

// Realm group naming. A subgroup under /project-<land> other than owners or
// guests reads as guest: Keycloak still gives it the project-member role
// through the parent, and the least role is the safe reading of a name nobody
// decided on.
const (
	groupProjectPrefix = "project-"
	groupLandOwners    = "owners"
	groupLandGuests    = "guests"
)

// landRoleOfGroup reads one group path; ok is false for a group that is not a
// project group.
func landRoleOfGroup(g string) (land string, role LandRole, ok bool) {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(g, "/"), groupProjectPrefix)
	if !ok {
		return "", LandRoleNone, false
	}
	land, sub, nested := strings.Cut(rest, "/")
	switch {
	case land == "":
		return "", LandRoleNone, false
	case !nested:
		return land, LandRoleMember, true
	case sub == groupLandOwners:
		return land, LandRoleOwner, true
	case sub == groupLandGuests:
		return land, LandRoleGuest, true
	default:
		return land, LandRoleGuest, true
	}
}

// LandRolesFromGroups maps each land the groups name to the highest role they
// give in it. Groups that are not project groups are ignored.
func LandRolesFromGroups(groups []string) map[string]LandRole {
	roles := map[string]LandRole{}
	for _, g := range groups {
		if land, r, ok := landRoleOfGroup(g); ok && r.rank() > roles[land].rank() {
			roles[land] = r
		}
	}
	return roles
}

// Action is something a person does in a land through a surface. The
// /api/conversation/inbound endpoint and the Commons write routes check it
// against the action table before doing anything as the fleet (design section
// 6.3); kbeads enforces the same roles on the person's own bearer.
type Action string

const (
	ActionSay              Action = "say"               // a new top-level message in a channel
	ActionReply            Action = "reply"             // a reply in a thread
	ActionReact            Action = "react"             // add or remove one's own reaction
	ActionDone             Action = "done"              // close a thread head (the Done button, wish-close)
	ActionSpawn            Action = "spawn"             // mention or compose that spawns an agent
	ActionAgentControl     Action = "agent_control"     // clear, kill or restart an agent
	ActionDecisionRespond  Action = "decision_respond"  // pick an option, Other, a free-text answer
	ActionDecisionDismiss  Action = "decision_dismiss"  // dismiss a decision card
	ActionDecisionPromote  Action = "decision_promote"  // promote a decision
	ActionPermissionAnswer Action = "permission_answer" // Allow or Deny a tool permission
	ActionEscalationAnswer Action = "escalation_answer" // approve or reject (kbeads narrows the operations)
	ActionChannelJoin      Action = "channel_join"      // join a user channel (MembershipChangeAllows)
	ActionChannelManage    Action = "channel_manage"    // create, rename or archive a channel; add or remove its members
	ActionMemberManage     Action = "member_manage"     // invite or remove a person, change a role
	ActionStewardDelegate  Action = "steward_delegate"  // set the operation types the steward may approve (land_steward_approves)
	ActionEditHolds        Action = "edit_holds"        // edit the Holds section of the land's steward notes (owner Q3; kbeads edit_holds)
)

// actionTable is the least land role each action needs. Guests are invited
// bystanders: they talk and react. Owners and members do the work, including
// spawning (owner decision Q8c, gd-fPXoeHDQS3: owner and members, not guests,
// within the purse). Only the owner shapes the land itself. An action missing
// here is denied to everyone but the operator.
var actionTable = map[Action]LandRole{
	ActionSay:              LandRoleGuest,
	ActionReply:            LandRoleGuest,
	ActionReact:            LandRoleGuest,
	ActionDone:             LandRoleMember,
	ActionSpawn:            LandRoleMember,
	ActionAgentControl:     LandRoleMember,
	ActionDecisionRespond:  LandRoleMember,
	ActionDecisionDismiss:  LandRoleMember,
	ActionDecisionPromote:  LandRoleMember,
	ActionPermissionAnswer: LandRoleMember,
	ActionEscalationAnswer: LandRoleMember,
	ActionChannelJoin:      LandRoleMember,
	ActionChannelManage:    LandRoleOwner,
	ActionMemberManage:     LandRoleOwner,
	ActionStewardDelegate:  LandRoleOwner,
	ActionEditHolds:        LandRoleMember,
}

// Allows reports whether a person with role may take action in the land. The
// operator may take every action; nobody else may take an action the table
// does not list.
func Allows(role LandRole, action Action) bool {
	if role == LandRoleOperator {
		return true
	}
	least, ok := actionTable[action]
	return ok && role.rank() > 0 && role.rank() >= least.rank()
}
