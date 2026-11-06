package conversation

import "testing"

func TestLandRolesFromGroups(t *testing.T) {
	got := LandRolesFromGroups([]string{
		"/project-a",
		"/project-b/owners", "/project-b",
		"/project-c/guests",
		"/project-d/guests", "/project-d",
		"/project-e/admins", // undecided subgroup: least role
		"project-f",         // no leading slash
		"/project-", "/project-/owners", "/gardenio-admins", "acme/project-x",
	})
	want := map[string]LandRole{
		"a": LandRoleMember, "b": LandRoleOwner, "c": LandRoleGuest,
		"d": LandRoleMember, "e": LandRoleGuest, "f": LandRoleMember,
	}
	if len(got) != len(want) {
		t.Fatalf("LandRolesFromGroups = %v, want %v", got, want)
	}
	for land, r := range want {
		if got[land] != r {
			t.Errorf("land %s: %q, want %q", land, got[land], r)
		}
	}
}

// allActions is every Action constant. A constant missing here, or one here
// with no actionTable row, fails TestActionTable_EveryActionDecided.
var allActions = []Action{
	ActionSay, ActionReply, ActionReact, ActionDone, ActionSpawn, ActionAgentControl,
	ActionDecisionRespond, ActionDecisionDismiss, ActionDecisionPromote,
	ActionPermissionAnswer, ActionEscalationAnswer, ActionChannelJoin, ActionChannelManage,
	ActionMemberManage, ActionStewardDelegate, ActionEditHolds,
}

func TestActionTable_EveryActionDecided(t *testing.T) {
	if len(actionTable) != len(allActions) {
		t.Errorf("actionTable has %d rows, allActions %d: add the new action to both", len(actionTable), len(allActions))
	}
	for _, a := range allActions {
		if _, ok := actionTable[a]; !ok {
			t.Errorf("action %s has no actionTable row", a)
		}
	}
}

// The reviewed matrix: G guest, M member, O owner. The operator may do all of
// it and a person with no role in the land none of it.
func TestActionTable_Matrix(t *testing.T) {
	want := map[Action]string{
		ActionSay:              "GMO",
		ActionReply:            "GMO",
		ActionReact:            "GMO",
		ActionDone:             "MO",
		ActionSpawn:            "MO", // Q8c, gd-fPXoeHDQS3
		ActionAgentControl:     "MO",
		ActionDecisionRespond:  "MO",
		ActionDecisionDismiss:  "MO",
		ActionDecisionPromote:  "MO",
		ActionPermissionAnswer: "MO",
		ActionEscalationAnswer: "MO",
		ActionChannelJoin:      "MO",
		ActionChannelManage:    "O",
		ActionMemberManage:     "O",
		ActionStewardDelegate:  "O",  // escalation-routing D8
		ActionEditHolds:        "MO", // owner Q3, kbeads memberActions doc edit_holds
	}
	roles := map[byte]LandRole{'G': LandRoleGuest, 'M': LandRoleMember, 'O': LandRoleOwner}
	for _, a := range allActions {
		for code, role := range roles {
			wantOK := false
			for i := range len(want[a]) {
				wantOK = wantOK || want[a][i] == code
			}
			if got := Allows(role, a); got != wantOK {
				t.Errorf("Allows(%s, %s) = %v, want %v", role, a, got, wantOK)
			}
		}
		if !Allows(LandRoleOperator, a) {
			t.Errorf("operator denied %s", a)
		}
		if Allows(LandRoleNone, a) || Allows("stranger", a) {
			t.Errorf("no role allowed %s", a)
		}
	}
	if Allows(LandRoleOwner, "unlisted") || !Allows(LandRoleOperator, "unlisted") {
		t.Errorf("an unlisted action must be operator-only")
	}
}
