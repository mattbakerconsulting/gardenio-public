package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Land roles (Lands Commons spec §7.3 step 4.2, gd-9aCcG1zCCY): owner, member
// and guest come from /project-<land>[/owners|/guests] groups; a guest may
// only say things and react, and may not spawn or approve.

func TestParseProjectGroup(t *testing.T) {
	for _, tc := range []struct {
		group   string
		project string
		role    landRole
		ok      bool
	}{
		{"/project-claimlayer", "claimlayer", landRoleMember, true},
		{"project-claimlayer", "claimlayer", landRoleMember, true},
		{"/project-claimlayer/owners", "claimlayer", landRoleOwner, true},
		{"/project-claimlayer/guests", "claimlayer", landRoleGuest, true},
		// A subgroup nobody decided on is the least role, not a member.
		{"/project-claimlayer/admins", "claimlayer", landRoleGuest, true},
		{"/project-claimlayer/owners/extra", "claimlayer", landRoleGuest, true},
		{"/project-", "", landRoleNone, false},
		{"/project-/owners", "", landRoleNone, false},
		{"/gardenio-admins", "", landRoleNone, false},
		{"acme/project-x", "", landRoleNone, false},
	} {
		p, r, ok := parseProjectGroup(tc.group)
		if p != tc.project || r != tc.role || ok != tc.ok {
			t.Errorf("parseProjectGroup(%q) = (%q, %s, %v), want (%q, %s, %v)", tc.group, p, r, ok, tc.project, tc.role, tc.ok)
		}
	}
}

func TestGroupsToLandRoles_HighestWins(t *testing.T) {
	got := groupsToLandRoles([]string{
		"/project-a/guests", "/project-a",
		"/project-b/owners", "/project-b/guests",
		"/project-c/guests",
		"/gardenio-admins",
	})
	want := map[string]landRole{"a": landRoleMember, "b": landRoleOwner, "c": landRoleGuest}
	if len(got) != len(want) {
		t.Fatalf("groupsToLandRoles = %v, want %v", got, want)
	}
	for p, r := range want {
		if got[p] != r {
			t.Errorf("land %s: %s, want %s", p, got[p], r)
		}
	}
}

// A subgroup names its land once, never "<land>/owners": before land roles,
// the read scope of an owner or guest was the nonexistent project
// "claimlayer/owners".
func TestGroupsToProjects_Subgroups(t *testing.T) {
	got := groupsToProjects([]string{"/project-claimlayer/owners", "/project-claimlayer", "/project-other/guests"})
	if !slices.Equal(got, []string{"claimlayer", "other"}) {
		t.Errorf("groupsToProjects = %v, want [claimlayer other]", got)
	}
	for _, groups := range [][]string{{"/project-claimlayer/owners"}, {"/project-claimlayer/guests"}} {
		if d := scopeFromContext(scopedCtx(t, groups)); !slices.Equal(d.Projects, []string{"claimlayer"}) {
			t.Errorf("scope of %v = %v, want [claimlayer]", groups, d.Projects)
		}
	}
}

// Owners and guests read their land like a member does.
func TestLandRoles_ReadScope(t *testing.T) {
	f := newScopedFixture(t)
	for _, tok := range []string{tokScopedOwner, tokScopedGuest, tokScopedWriter} {
		ids := f.listIDs(t, tok)
		if !ids[beadClaimlayer] || ids[beadGardenio] || ids[beadOther] || ids[beadNoLabel] {
			t.Errorf("%s list = %v, want only %s", tok, ids, beadClaimlayer)
		}
	}
}

func TestLandRoles_GuestSaysAndReactsOnly(t *testing.T) {
	f := newScopedFixture(t)
	create := func(tok string, typ model.BeadType, project string) (int, string) {
		body := map[string]any{"title": "t", "type": string(typ), "status": "closed", "labels": []string{"project:" + project}}
		if typ == model.TypeEscalation {
			body["fields"] = map[string]any{"operation_type": "x", "operation_spec": map[string]any{"a": "b"}}
		}
		return f.do(t, tok, http.MethodPost, "/v1/beads", body)
	}

	// Positive controls: the guest says things; owner and member do the rest.
	if code, body := create(tokScopedGuest, "message", "claimlayer"); code != http.StatusCreated {
		t.Fatalf("guest create message: %d %s, want 201", code, body)
	}
	for _, tok := range []string{tokScopedOwner, tokScopedWriter} {
		if code, body := create(tok, model.TypeTask, "claimlayer"); code != http.StatusCreated {
			t.Errorf("%s create task: %d %s, want 201", tok, code, body)
		}
	}

	for _, typ := range []model.BeadType{model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeEscalation} {
		if code, body := create(tokScopedGuest, typ, "claimlayer"); code != http.StatusForbidden || !strings.Contains(body, "guest in land claimlayer") {
			t.Errorf("guest create %s: %d %s, want 403 naming the guest rule", typ, code, body)
		}
	}

	f.seedTyped("cl-dec", model.TypeDecision, "project:claimlayer")
	f.seedTyped("cl-dec2", model.TypeDecision, "project:claimlayer")
	if code, body := f.do(t, tokScopedGuest, http.MethodPatch, "/v1/beads/cl-dec", map[string]any{"description": "d"}); code != http.StatusForbidden {
		t.Errorf("guest update decision: %d %s, want 403", code, body)
	}
	if code, body := f.do(t, tokScopedGuest, http.MethodPost, "/v1/beads/cl-dec/close", map[string]any{}); code != http.StatusForbidden {
		t.Errorf("guest close decision: %d %s, want 403", code, body)
	}
	if code, body := f.do(t, tokScopedWriter, http.MethodPost, "/v1/beads/cl-dec2/close", map[string]any{}); code == http.StatusForbidden {
		t.Errorf("member close decision: %d %s, want allowed", code, body)
	}
	if code, body := f.do(t, tokScopedGuest, http.MethodPatch, "/v1/beads/"+beadClaimlayer, map[string]any{"description": "d"}); code != http.StatusForbidden {
		t.Errorf("guest update task: %d %s, want 403", code, body)
	}

	// The guest reacts with its own principal, as a member does.
	f.seedTyped("m-1", "message", "project:claimlayer")
	if code, body := f.do(t, tokScopedGuest, http.MethodPatch, "/v1/beads/m-1", map[string]any{
		"fields": map[string]any{"reactions": map[string][]string{"eyes": {"oidc:clg@example.com"}}},
	}); code != http.StatusOK {
		t.Errorf("guest react: %d %s, want 200", code, body)
	}
}

// The guest rule is per land: a guest in one land is a full member in another.
func TestLandRoles_GuestIsPerLand(t *testing.T) {
	f := newScopedFixture(t)
	create := func(project string) (int, string) {
		return f.do(t, tokScopedGuestOther, http.MethodPost, "/v1/beads", map[string]any{
			"title": "t", "type": "task", "labels": []string{"project:" + project},
		})
	}
	if code, body := create("other"); code != http.StatusCreated {
		t.Errorf("member-in-other create task: %d %s, want 201", code, body)
	}
	if code, body := create("claimlayer"); code != http.StatusForbidden {
		t.Errorf("guest-in-claimlayer create task: %d %s, want 403", code, body)
	}
}

// Q8c (gd-fPXoeHDQS3): owners and members spawn in their land, guests do not.
func TestLandRoles_SpawnRights(t *testing.T) {
	f := newScopedFixture(t)
	makeUserBead(f.ms, "gd-user-clw", "clw@example.com", "*", "")
	spawn := func(groups []string) error {
		return authorizeAgentSpawn(scopedCtx(t, groups), f.ms, []byte(`{"project":"claimlayer","role":"crew"}`))
	}
	for _, groups := range [][]string{{"/project-claimlayer"}, {"/project-claimlayer/owners"}, {"/project-claimlayer/guests", "/project-claimlayer"}} {
		if err := spawn(groups); err != nil {
			t.Errorf("spawn with %v: %v, want allowed", groups, err)
		}
	}
	if err := spawn([]string{"/project-claimlayer/guests"}); err == nil || !strings.Contains(err.Error(), "guest in land claimlayer") {
		t.Errorf("guest spawn: %v, want denied naming the guest rule", err)
	}
}

func TestLandRoles_GuestCannotApproveEscalation(t *testing.T) {
	f := newScopedFixture(t)
	f.seedEscalation("cl-esc-g", "project:claimlayer", "create", taskSpec("claimlayer"), nil)
	f.seedEscalation("cl-esc-o", "project:claimlayer", "create", taskSpec("claimlayer"), nil)

	c, b := f.approve(t, tokScopedGuest, "cl-esc-g")
	f.wantStatus(t, "guest approve", c, b, http.StatusForbidden)
	c, b = f.reject(t, tokScopedGuest, "cl-esc-g")
	f.wantStatus(t, "guest reject", c, b, http.StatusForbidden)
	if got := f.escalationStatus("cl-esc-g"); got == model.StatusClosed {
		t.Errorf("guest closed the escalation")
	}
	c, b = f.approve(t, tokScopedOwner, "cl-esc-o")
	f.wantStatus(t, "owner approve", c, b, http.StatusOK)
}
