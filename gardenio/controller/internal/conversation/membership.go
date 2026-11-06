package conversation

import "slices"

// Channel is what the membership rules read from a type=channel bead (design
// section 2.2): its channel_kind and fields.members.
type Channel struct {
	Kind    string
	Members []string
}

// MembersOnly reports whether only the channel's members may post in it (design
// section 7.3 step 4.8): a user channel or a DM. Everyone in the land still
// reads it; a channel hidden from non-members is a private channel, LATER. The
// default channels (general, crew, decisions, escalations) are open to the land.
func (c Channel) MembersOnly() bool {
	return c.Kind == "user" || c.Kind == "dm"
}

// HasMember reports whether principal (oidc:<email> or agent:<name>) is listed.
func (c Channel) HasMember(principal string) bool {
	return principal != "" && slices.Contains(c.Members, principal)
}

// ChannelAllows reports whether principal, holding role in the channel's land,
// may take action in the channel: the action table first, then, to say or
// reply in a members-only channel, membership. The operator is never held to
// membership. kbeads enforces the same rule on the person's own bearer
// (kbeads/internal/server/channel_membership.go).
func ChannelAllows(role LandRole, principal string, ch Channel, action Action) bool {
	if !Allows(role, action) {
		return false
	}
	if role == LandRoleOperator || !ch.MembersOnly() {
		return true
	}
	if action == ActionSay || action == ActionReply {
		return ch.HasMember(principal)
	}
	return true
}

// MembershipChangeAllows reports whether principal, holding role in the
// channel's land, may change a channel's members from ch.Members to next. Only
// a user channel's members change (a DM's are fixed at create). Anyone with a
// role may leave; joining is ActionChannelJoin (members and owners, not
// guests, whom an owner adds); adding or removing anyone else is
// ActionChannelManage. The operator may make any change.
func MembershipChangeAllows(role LandRole, principal string, ch Channel, next []string) bool {
	if role == LandRoleOperator {
		return true
	}
	if ch.Kind != "user" || role.rank() == 0 || principal == "" {
		return false
	}
	need := map[Action]bool{}
	for _, m := range next {
		if !ch.HasMember(m) {
			if m == principal {
				need[ActionChannelJoin] = true
			} else {
				need[ActionChannelManage] = true
			}
		}
	}
	for _, m := range ch.Members {
		if m != principal && !slices.Contains(next, m) {
			need[ActionChannelManage] = true
		}
	}
	for a := range need {
		if !Allows(role, a) {
			return false
		}
	}
	return true
}
