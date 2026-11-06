package provision

import (
	"context"
	"sort"
	"strings"

	"gardenio/controller/internal/keycloakadmin"
	"gardenio/controller/internal/lands/landscope"
)

// The Keycloak admin fake for the world in provision_test.go: groups are
// w.groups, users w.users, and every membership change is one world write.

type fakeGroups struct{ w *world }

func roleGroup(land string, role keycloakadmin.LandRole) string {
	switch role {
	case keycloakadmin.RoleOwner:
		return "project-" + land + "/owners"
	case keycloakadmin.RoleGuest:
		return "project-" + land + "/guests"
	}
	return "project-" + land
}

func (g fakeGroups) EnsureProjectGroup(_ context.Context, land string) (string, error) {
	name, err := keycloakadmin.GroupName(land)
	if err != nil {
		return "", err
	}
	if _, ok := g.w.groups[name]; ok {
		if !g.w.marked[name] {
			return "", keycloakadmin.ErrNotLandGroup
		}
		return name, g.w.read("kc:ensure")
	}
	if err := g.w.write("kc:group"); err != nil {
		return "", err
	}
	g.w.groups[name], g.w.marked[name] = map[string]bool{}, true
	return name, nil
}

// fakeLandScopes is the land's KeycloakClientScope resource (landscope).
type fakeLandScopes struct{ w *world }

func (f fakeLandScopes) Ensure(_ context.Context, land string) error {
	if _, err := landscope.Name(land); err != nil {
		return err
	}
	if f.w.landScopes[land] {
		return f.w.read("kc:scope-ensure")
	}
	if err := f.w.write("kc:scope"); err != nil {
		return err
	}
	f.w.landScopes[land] = true
	return nil
}

func (g fakeGroups) EnsureLandRoleGroup(_ context.Context, land string, role keycloakadmin.LandRole) (string, error) {
	if _, ok := g.w.groups["project-"+land]; !ok {
		return "", keycloakadmin.ErrGroupNotFound
	}
	name := roleGroup(land, role)
	if _, ok := g.w.groups[name]; ok {
		return name, g.w.read("kc:ensure")
	}
	if err := g.w.write("kc:subgroup"); err != nil {
		return "", err
	}
	g.w.groups[name] = map[string]bool{}
	return name, nil
}

func (g fakeGroups) AddLandMember(_ context.Context, land string, role keycloakadmin.LandRole, user string) error {
	grp, ok := g.w.groups[roleGroup(land, role)]
	if !ok {
		return keycloakadmin.ErrGroupNotFound
	}
	if err := g.w.write("kc:add:" + string(role)); err != nil {
		return err
	}
	grp[user] = true
	return nil
}

func (g fakeGroups) AddTenant(_ context.Context, user string) error {
	grp, ok := g.w.groups[keycloakadmin.TenantsGroup]
	if !ok {
		return keycloakadmin.ErrGroupNotFound
	}
	if err := g.w.write("kc:add:tenant"); err != nil {
		return err
	}
	grp[user] = true
	return nil
}

func (g fakeGroups) LandMembers(_ context.Context, land string) ([]keycloakadmin.LandMember, error) {
	if err := g.w.read("kc:members"); err != nil {
		return nil, err
	}
	if _, ok := g.w.groups["project-"+land]; !ok {
		return nil, keycloakadmin.ErrGroupNotFound
	}
	var out []keycloakadmin.LandMember
	seen := map[string]bool{}
	for _, role := range []keycloakadmin.LandRole{keycloakadmin.RoleOwner, keycloakadmin.RoleMember, keycloakadmin.RoleGuest} {
		var ids []string
		for id := range g.w.groups[roleGroup(land, role)] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, keycloakadmin.LandMember{User: keycloakadmin.User{ID: id}, Role: role})
			}
		}
	}
	return out, nil
}

func (g fakeGroups) RemoveFromLand(_ context.Context, land, user string) error {
	if err := g.w.write("kc:remove"); err != nil {
		return err
	}
	for _, role := range []keycloakadmin.LandRole{keycloakadmin.RoleOwner, keycloakadmin.RoleMember, keycloakadmin.RoleGuest} {
		delete(g.w.groups[roleGroup(land, role)], user)
	}
	return nil
}

func (g fakeGroups) UserLands(_ context.Context, user string) ([]string, error) {
	if err := g.w.read("kc:lands"); err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for name, members := range g.w.groups {
		if top, _, _ := strings.Cut(name, "/"); strings.HasPrefix(top, "project-") && members[user] {
			set[strings.TrimPrefix(top, "project-")] = true
		}
	}
	var out []string
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out, nil
}

func (g fakeGroups) RemoveTenant(_ context.Context, user string) error {
	if err := g.w.write("kc:remove:tenant"); err != nil {
		return err
	}
	delete(g.w.groups[keycloakadmin.TenantsGroup], user)
	return nil
}

func (g fakeGroups) FindUserByGitHubID(_ context.Context, id int64) (keycloakadmin.User, error) {
	if err := g.w.read("kc:find"); err != nil {
		return keycloakadmin.User{}, err
	}
	if g.w.lookup != nil {
		return g.w.lookup(id)
	}
	var match []keycloakadmin.User
	for _, u := range g.w.users {
		if u.GitHubID == id {
			match = append(match, u)
		}
	}
	switch len(match) {
	case 0:
		return keycloakadmin.User{}, keycloakadmin.ErrUserNotFound
	case 1:
		return match[0], nil
	}
	return keycloakadmin.User{}, keycloakadmin.ErrAmbiguousUser
}

func (g fakeGroups) FindUserByEmail(_ context.Context, email string) (keycloakadmin.User, error) {
	if err := g.w.read("kc:find-email"); err != nil {
		return keycloakadmin.User{}, err
	}
	if g.w.emailLookup != nil {
		return g.w.emailLookup(email)
	}
	var match []keycloakadmin.User
	for _, u := range g.w.users {
		if u.Email == email {
			match = append(match, u)
		}
	}
	switch len(match) {
	case 0:
		return keycloakadmin.User{}, keycloakadmin.ErrUserNotFound
	case 1:
		return match[0], nil
	}
	return keycloakadmin.User{}, keycloakadmin.ErrAmbiguousUser
}

func (g fakeGroups) AddInvited(_ context.Context, user string) error {
	grp, ok := g.w.groups[keycloakadmin.InvitedGroup]
	if !ok {
		return keycloakadmin.ErrGroupNotFound
	}
	if err := g.w.write("kc:add:invited"); err != nil {
		return err
	}
	grp[user] = true
	return nil
}

func (g fakeGroups) InvitedMembers(_ context.Context) ([]keycloakadmin.User, error) {
	if err := g.w.read("kc:invited"); err != nil {
		return nil, err
	}
	grp, ok := g.w.groups[keycloakadmin.InvitedGroup]
	if !ok {
		return nil, keycloakadmin.ErrGroupNotFound
	}
	var ids []string
	for id := range grp {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]keycloakadmin.User, 0, len(ids))
	for _, id := range ids {
		out = append(out, keycloakadmin.User{ID: id})
	}
	return out, nil
}
