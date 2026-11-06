package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestScopedPrincipal_ListsOnlyOwnProject(t *testing.T) {
	f := newScopedFixture(t)

	if got := sortedKeys(f.listIDs(t, tokScopedClaimlayer)); !reflect.DeepEqual(got, []string{beadClaimlayer}) {
		t.Errorf("scoped claimlayer saw %v, want [cl-1]", got)
	}
	want := []string{beadClaimlayer, beadGardenio, beadNoLabel, beadOther}
	if got := sortedKeys(f.listIDs(t, tokOwnerAdmin)); !reflect.DeepEqual(got, want) {
		t.Errorf("admin control saw %v, want %v", got, want)
	}
}

// A bead outside the principal's projects answers as a missing one
// (gd-7APTAIhdC5).
func TestScopedPrincipal_GetOtherProject404(t *testing.T) {
	f := newScopedFixture(t)
	for id, want := range map[string]int{
		beadGardenio:   http.StatusNotFound,
		beadNoLabel:    http.StatusNotFound,
		beadOther:      http.StatusNotFound,
		beadClaimlayer: http.StatusOK,
	} {
		resp := f.get(t, tokScopedClaimlayer, "/v1/beads/"+id)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: status %d, want %d", id, resp.StatusCode, want)
		}
	}
}

// The fail-closed pin: a scoped token with no project group is not
// "unrestricted", it sees nothing.
func TestScopedPrincipal_NoGroupDeniesAll(t *testing.T) {
	f := newScopedFixture(t)
	if got := f.listIDs(t, tokScopedNoGroup); len(got) != 0 {
		t.Errorf("no-group scoped principal listed %v, want nothing", got)
	}
	resp := f.get(t, tokScopedNoGroup, "/v1/beads/"+beadClaimlayer)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET cl-1 as no-group: status %d, want 404", resp.StatusCode)
	}
}

// A stale user bead must not widen a scoped principal.
func TestScopedPrincipal_GroupRemovalRevokes(t *testing.T) {
	f := newScopedFixture(t)
	makeUserBead(f.ms, "gd-user-stale", "nogroup@example.com", "claimlayer", "")

	if got := f.listIDs(t, tokScopedNoGroup); len(got) != 0 {
		t.Errorf("stale bead widened the list to %v", got)
	}
	resp := f.get(t, tokScopedNoGroup, "/v1/beads/"+beadClaimlayer)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET cl-1 with stale bead: status %d, want 404", resp.StatusCode)
	}
	// The same call through the provisioner has cleared the stale entry.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(f.ms.beads["gd-user-stale"].Fields, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["allowed_projects"]) != "[]" {
		t.Errorf("user bead allowed_projects = %s, want []", fields["allowed_projects"])
	}
}

func TestSyncGroupProjects_EmptyGroupsClearsBead(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})
	makeUserBead(ms, "gd-user-clr", "clr@example.com", "claimlayer", "")

	// Prime the cache with the old project, then revoke it.
	if err := up.syncGroupProjects(context.Background(), "clr@example.com", "gd-user-clr", []string{"/project-claimlayer"}); err != nil {
		t.Fatal(err)
	}
	if err := up.syncGroupProjects(context.Background(), "clr@example.com", "gd-user-clr", []string{}); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(ms.beads["gd-user-clr"].Fields, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["allowed_projects"]) != "[]" {
		t.Errorf("allowed_projects = %s, want []", fields["allowed_projects"])
	}
}

// A sign-in records each land's role on the user bead, and a role change
// alone (member to owner, same lands) is written too.
func TestSyncGroupProjects_RecordsLandRoles(t *testing.T) {
	ms := newMockStore()
	up := NewUserProvisioner(ms, &events.NoopPublisher{})
	makeUserBead(ms, "gd-user-clr", "clr@example.com", "claimlayer", "")
	landRoles := func() string {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(ms.beads["gd-user-clr"].Fields, &fields); err != nil {
			t.Fatal(err)
		}
		return string(fields["land_roles"])
	}
	for _, c := range []struct {
		groups []string
		want   string
	}{
		{[]string{"/project-claimlayer", "/project-other/guests"}, `{"claimlayer":"member","other":"guest"}`},
		{[]string{"/project-claimlayer/owners", "/project-other/guests"}, `{"claimlayer":"owner","other":"guest"}`},
		{[]string{}, `{}`},
	} {
		if err := up.syncGroupProjects(context.Background(), "clr@example.com", "gd-user-clr", c.groups); err != nil {
			t.Fatal(err)
		}
		if got := landRoles(); got != c.want {
			t.Errorf("groups %v: land_roles = %s, want %s", c.groups, got, c.want)
		}
	}
}

func TestScopedPrincipal_TwoProjectsUnion(t *testing.T) {
	f := newScopedFixture(t)
	want := []string{beadClaimlayer, beadOther}
	if got := sortedKeys(f.listIDs(t, tokScopedTwo)); !reflect.DeepEqual(got, want) {
		t.Errorf("two-project principal saw %v, want %v", got, want)
	}
	resp := f.get(t, tokScopedTwo, "/v1/beads/"+beadGardenio)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET gd-1: status %d, want 404", resp.StatusCode)
	}
}

// Pins that the change is scoped to the new role: an operator with no
// allowed_projects keeps seeing everything.
func TestOperatorUnchanged(t *testing.T) {
	f := newScopedFixture(t)
	want := []string{beadClaimlayer, beadGardenio, beadNoLabel, beadOther}
	if got := sortedKeys(f.listIDs(t, tokOperator)); !reflect.DeepEqual(got, want) {
		t.Errorf("operator saw %v, want %v", got, want)
	}
}

// Every per-bead read sub-resource routes through getBeadFiltered. The list is
// explicit so a new sub-resource that skips the gate has to be added here.
func TestScopedPrincipal_SubresourcesGated(t *testing.T) {
	f := newScopedFixture(t)
	for _, sub := range []string{
		"comments", "events", "mutations", "labels", "dependencies", "dependents",
		"followers", "mentions", "mentioned-by", "operator-history", "taints",
	} {
		t.Run(sub, func(t *testing.T) {
			denied := f.get(t, tokScopedClaimlayer, "/v1/beads/"+beadGardenio+"/"+sub)
			denied.Body.Close()
			if denied.StatusCode != http.StatusNotFound {
				t.Errorf("gd-1/%s: status %d, want 404", sub, denied.StatusCode)
			}
			allowed := f.get(t, tokScopedClaimlayer, "/v1/beads/"+beadClaimlayer+"/"+sub)
			allowed.Body.Close()
			if allowed.StatusCode != http.StatusOK {
				t.Errorf("cl-1/%s: status %d, want 200", sub, allowed.StatusCode)
			}
		})
	}
}

func TestScopeFromContext(t *testing.T) {
	scoped := func(roles, groups []string) context.Context {
		ctx := oidcCtx("x@example.com", roles)
		return WithGroups(ctx, groups)
	}
	cases := []struct {
		name         string
		ctx          context.Context
		unrestricted bool
		projects     []string
	}{
		{"static", withAuthMethod(context.Background(), authMethodStatic), true, nil},
		{"admin", scoped([]string{RoleAdmin, RoleProjectMember}, nil), true, nil},
		{"operator", scoped([]string{RoleOperator}, nil), true, nil},
		{"member", scoped([]string{RoleProjectMember}, []string{"/project-b", "project-a", "/project-a", "/other"}), false, []string{"a", "b"}},
		{"member-no-groups", scoped([]string{RoleProjectMember}, nil), false, nil},
	}
	for _, c := range cases {
		d := scopeFromContext(c.ctx)
		if d.Unrestricted != c.unrestricted || len(d.Projects) != len(c.projects) || (len(c.projects) > 0 && !reflect.DeepEqual(d.Projects, c.projects)) {
			t.Errorf("%s: got %+v, want unrestricted=%v projects=%v", c.name, d, c.unrestricted, c.projects)
		}
	}
	if d := scopeFromContext(scoped([]string{RoleProjectMember}, []string{"/project-a"})); !d.allows("a") || d.allows("b") {
		t.Errorf("allows() wrong for %+v", d)
	}
	if got := oidcAllowedProjects(scoped([]string{RoleProjectMember}, nil), nil); got == nil || len(got) != 0 {
		t.Errorf("empty scoped principal: oidcAllowedProjects = %#v, want non-nil empty", got)
	}
}

var _ = model.StatusOpen
