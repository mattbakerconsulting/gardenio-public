package conversation

import "testing"

// Channel membership (design section 7.3 step 4.8). The same cases as kbeads'
// channel_membership_test.go, so the Commons and the daemon agree.

const (
	pOwner  = "oidc:owner@example.com"
	pMember = "oidc:member@example.com"
	pGuest  = "oidc:guest@example.com"
)

func TestChannel_MembersOnly(t *testing.T) {
	for kind, want := range map[string]bool{"user": true, "dm": true, "general": false, "crew": false, "decisions": false, "escalations": false, "": false} {
		if got := (Channel{Kind: kind}).MembersOnly(); got != want {
			t.Errorf("MembersOnly(%q) = %v, want %v", kind, got, want)
		}
	}
	if (Channel{Members: []string{pOwner}}).HasMember("") {
		t.Error("the empty principal is never a member")
	}
}

func TestChannelAllows(t *testing.T) {
	general := Channel{Kind: "general"}
	design := Channel{Kind: "user", Members: []string{pOwner}}
	dm := Channel{Kind: "dm", Members: []string{pMember, "agent:x"}}
	for _, c := range []struct {
		name      string
		role      LandRole
		principal string
		ch        Channel
		action    Action
		want      bool
	}{
		{"member says in general", LandRoleMember, pMember, general, ActionSay, true},
		{"guest replies in general", LandRoleGuest, pGuest, general, ActionReply, true},
		{"owner member of design says", LandRoleOwner, pOwner, design, ActionSay, true},
		{"member not in design says", LandRoleMember, pMember, design, ActionSay, false},
		{"member not in design replies", LandRoleMember, pMember, design, ActionReply, false},
		{"member not in design reacts", LandRoleMember, pMember, design, ActionReact, true},
		{"owner not in dm says", LandRoleOwner, pOwner, dm, ActionSay, false},
		{"member in dm says", LandRoleMember, pMember, dm, ActionSay, true},
		{"operator says anywhere", LandRoleOperator, "", design, ActionSay, true},
		{"no role in the land", LandRoleNone, pOwner, design, ActionSay, false},
		{"guest member of design spawns", LandRoleGuest, pGuest, Channel{Kind: "user", Members: []string{pGuest}}, ActionSpawn, false},
	} {
		if got := ChannelAllows(c.role, c.principal, c.ch, c.action); got != c.want {
			t.Errorf("%s: ChannelAllows = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMembershipChangeAllows(t *testing.T) {
	design := Channel{Kind: "user", Members: []string{pOwner}}
	withGuest := Channel{Kind: "user", Members: []string{pOwner, pGuest}}
	for _, c := range []struct {
		name      string
		role      LandRole
		principal string
		ch        Channel
		next      []string
		want      bool
	}{
		{"member joins", LandRoleMember, pMember, design, []string{pOwner, pMember}, true},
		{"member leaves", LandRoleMember, pMember, Channel{Kind: "user", Members: []string{pOwner, pMember}}, []string{pOwner}, true},
		{"member adds someone else", LandRoleMember, pMember, design, []string{pOwner, pGuest}, false},
		{"member removes someone else", LandRoleMember, pMember, design, []string{}, false},
		{"guest joins", LandRoleGuest, pGuest, design, []string{pOwner, pGuest}, false},
		{"guest leaves", LandRoleGuest, pGuest, withGuest, []string{pOwner}, true},
		{"owner adds two", LandRoleOwner, pOwner, design, []string{pOwner, pMember, pGuest}, true},
		{"owner removes a guest", LandRoleOwner, pOwner, withGuest, []string{pOwner}, true},
		{"owner joins a dm", LandRoleOwner, pOwner, Channel{Kind: "dm", Members: []string{pMember, "agent:x"}}, []string{pMember, "agent:x", pOwner}, false},
		{"owner joins general", LandRoleOwner, pOwner, Channel{Kind: "general"}, []string{pOwner}, false},
		{"no role joins", LandRoleNone, pMember, design, []string{pOwner, pMember}, false},
		{"no principal", LandRoleMember, "", design, []string{pOwner}, false},
		{"operator changes a dm", LandRoleOperator, "", Channel{Kind: "dm"}, []string{pOwner, pMember}, true},
	} {
		if got := MembershipChangeAllows(c.role, c.principal, c.ch, c.next); got != c.want {
			t.Errorf("%s: MembershipChangeAllows = %v, want %v", c.name, got, c.want)
		}
	}
}
