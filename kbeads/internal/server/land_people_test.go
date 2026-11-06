package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// P10.C (gd-wi44L8Qy4I): a tenant land's owner manages the land's own people.
// keycloak_group_membership leaves the owner floor only for /project-<land>
// and its subgroups, in that one tenant land; global roles, gardenio's own
// groups and another land's groups stay on it.

func peopleFields(spec any) map[string]any {
	return map[string]any{"operation_type": "keycloak_group_membership", "operation_spec": spec}
}

func TestLandPeople_OffTheFloorOnlyForTheLandsOwnGroups(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands(nil)
	addTo := func(groups ...any) map[string]any {
		return map[string]any{"groups": groups, "user": "new@example.com", "action": "add"}
	}
	for _, c := range []struct {
		name   string
		labels []string
		spec   any
		extra  map[string]any
		floor  bool
	}{
		{"member group", []string{"project:claimlayer"}, addTo("/project-claimlayer"), nil, false},
		{"owners and guests", []string{"project:claimlayer"}, addTo("/project-claimlayer/owners", "/project-claimlayer/guests"), nil, false},
		{"one group, removed", []string{"project:claimlayer"}, map[string]any{"group": "/project-claimlayer", "user": "x@example.com", "action": "remove"}, nil, false},
		{"spec as a JSON string", []string{"project:claimlayer"}, `{"group":"/project-claimlayer/owners","user":"x@example.com"}`, nil, false},
		{"another land's group", []string{"project:claimlayer"}, addTo("/project-acme"), nil, true},
		{"one of two groups elsewhere", []string{"project:claimlayer"}, addTo("/project-claimlayer", "/gardenio-admins"), nil, true},
		{"a prefix, not the land", []string{"project:claimlayer"}, addTo("/project-claimlayerx"), nil, true},
		{"two levels down", []string{"project:claimlayer"}, addTo("/project-claimlayer/owners/x"), nil, true},
		{"a realm role too", []string{"project:claimlayer"}, map[string]any{"group": "/project-claimlayer", "realm_roles": []string{"gardenio-admin"}}, nil, true},
		{"not add or remove", []string{"project:claimlayer"}, map[string]any{"group": "/project-claimlayer", "action": "grant"}, nil, true},
		{"no group", []string{"project:claimlayer"}, map[string]any{"user": "x@example.com"}, nil, true},
		{"gardenio's own project", []string{"project:gardenio"}, addTo("/project-gardenio"), nil, true},
		{"no project label", nil, addTo("/project-claimlayer"), nil, true},
		{"two project labels", []string{"project:claimlayer", "project:acme"}, addTo("/project-claimlayer"), nil, true},
		{"secret", []string{"project:claimlayer"}, addTo("/project-claimlayer"), map[string]any{"sensitivity": "secret"}, true},
	} {
		fields := peopleFields(c.spec)
		for k, v := range c.extra {
			fields[k] = v
		}
		raw, _ := json.Marshal(fields)
		b := &model.Bead{ID: "gd-esc", Type: model.TypeEscalation, Labels: c.labels, Fields: raw}
		reason := f.srv.escalationForcedReason(context.Background(), b)
		if got := isEscalationFloorReason(reason); got != c.floor {
			t.Errorf("%s: on the floor = %v (reason %q), want %v", c.name, got, reason, c.floor)
		}
	}
}

// The acceptance test: the real tenant token shape (gardenio-project-member,
// /project-claimlayer/owners) asks for a member add from its CLI login and
// approves it through each door. The page approves; the same gl / kd login is
// refused with the page to go to, and a member of the land is refused.
func TestLandPeople_TenantOwnerAddsAMemberOnThePage(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands(nil)
	esc := f.fileEscalation(t, tokScopedOwnerCLI, "claimlayer", peopleFields(map[string]any{
		"group": "/project-claimlayer", "user": "new@example.com", "action": "add",
	}))
	for k, want := range map[string]string{
		"approvers": `["land-owner:claimlayer"]`, "held_by": "land-owner:claimlayer",
		"policy_rule": landPeopleRule, "operator_only": "false",
	} {
		if got := f.field(t, esc, k); got != want {
			t.Errorf("filed: %s = %s, want %s", k, got, want)
		}
	}

	c, b := f.approve(t, tokScopedWriter, esc)
	f.wantStatus(t, "a member approves", c, b, http.StatusForbidden)
	requireUnapproved(t, f.ms, esc)

	c, b = f.approve(t, tokScopedOwnerCLI, esc)
	f.wantStatus(t, "the owner approves from gl / kd", c, b, http.StatusForbidden)
	if !strings.Contains(b, "on its page") {
		t.Errorf("the CLI refusal must hand over the page, got %s", b)
	}
	requireUnapproved(t, f.ms, esc)

	c, b = f.approve(t, tokScopedOwner, esc)
	f.wantStatus(t, "the owner approves on the page", c, b, http.StatusOK)
	if got := f.field(t, esc, "approved_by"); got != "clo@example.com" {
		t.Errorf("approved_by = %q, want the land owner", got)
	}
	if got := f.escalationStatus(esc); got != model.StatusInProgress {
		t.Errorf("status = %s after approval, want in_progress", got)
	}
}

// A land's owner never reaches past the land: a group outside it keeps the
// escalation on the floor, so the owner's page approval is refused.
func TestLandPeople_GlobalGroupStaysWithTheOwner(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands(nil)
	esc := f.fileEscalation(t, tokScopedOwnerCLI, "claimlayer", peopleFields(map[string]any{
		"group": "/gardenio-admins", "user": "new@example.com",
	}))
	if got := f.field(t, esc, "approvers"); got != `["operator"]` {
		t.Errorf("approvers = %s, want the owner floor", got)
	}
	c, b := f.approve(t, tokScopedOwner, esc)
	f.wantStatus(t, "the land owner approves a global group", c, b, http.StatusForbidden)
	requireUnapproved(t, f.ms, esc)
}

// The steward's delegation is unchanged: it may not take a land's people.
func TestLandPeople_NotDelegableToTheSteward(t *testing.T) {
	f := newScopedFixture(t)
	f.seedStewardLands(nil)
	c, b := f.delegate(t, tokScopedOwner, "claimlayer", []string{landPeopleOperation})
	f.wantStatus(t, "owner delegates the land's people", c, b, http.StatusBadRequest)
}
