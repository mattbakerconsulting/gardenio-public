package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gardenio/kbeads/internal/model"
)

// makeUserBead creates a user bead in the mock store with spawn permissions.
// Uses comma-separated strings for allowed_projects/roles so the mockStore's
// map[string]string field filter works, while parseStringList handles both.
func makeUserBead(ms *mockStore, id, email, projects, roles string) {
	fields := map[string]string{"email": email}
	if projects != "" {
		fields["allowed_projects"] = projects
	}
	if roles != "" {
		fields["allowed_roles"] = roles
	}
	fieldsJSON, _ := json.Marshal(fields)
	ms.beads[id] = &model.Bead{
		ID:     id,
		Title:  email,
		Type:   "user",
		Kind:   "config",
		Status: model.StatusOpen,
		Fields: fieldsJSON,
	}
}

// makeRoleBead creates a type:role bead in the mock store, titled name, with
// privileged set as given.
func makeRoleBead(ms *mockStore, id, name string, privileged bool) {
	fields := map[string]string{}
	if privileged {
		fields["privileged"] = "true"
		fields["needs_k8s_api"] = "true"
	}
	fieldsJSON, _ := json.Marshal(fields)
	ms.beads[id] = &model.Bead{
		ID:     id,
		Title:  name,
		Type:   "role",
		Kind:   "config",
		Status: model.StatusOpen,
		Fields: fieldsJSON,
	}
}

func oidcCtx(email string, roles []string) context.Context {
	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, roles)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "oidc", Subject: email})
	return ctx
}

func agentFields(project, role string) json.RawMessage {
	f, _ := json.Marshal(map[string]string{"project": project, "role": role, "agent": "test"})
	return f
}

func TestAuthorizeAgentSpawn_StaticTokenBypass(t *testing.T) {
	ms := newMockStore()
	ctx := withAuthMethod(context.Background(), authMethodStatic)

	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "crew"))
	if err != nil {
		t.Fatalf("static token should bypass spawn auth, got: %v", err)
	}
}

func TestAuthorizeAgentSpawn_StaticTokenPrivilegedRoleDenied(t *testing.T) {
	ms := newMockStore()
	makeRoleBead(ms, "gd-role-k8s-live", "k8s-live", true)
	ctx := withAuthMethod(context.Background(), authMethodStatic)

	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "k8s-live"))
	if err == nil {
		t.Fatal("static token spawning a privileged role should be denied")
	}
	var denial *AuthzDenial
	if !errors.As(err, &denial) {
		t.Fatalf("expected *AuthzDenial, got %T: %v", err, err)
	}
	if !denial.Escalatable {
		t.Fatal("privileged-role spawn denial should be escalatable")
	}
	if denial.SpawnRole != "k8s-live" {
		t.Fatalf("expected SpawnRole=k8s-live, got %q", denial.SpawnRole)
	}
}

func TestAuthorizeAgentSpawn_StaticTokenNonPrivilegedRoleAllowed(t *testing.T) {
	ms := newMockStore()
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	ctx := withAuthMethod(context.Background(), authMethodStatic)

	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "crew"))
	if err != nil {
		t.Fatalf("non-privileged role should be unaffected, got: %v", err)
	}
}

func TestAuthorizeAgentSpawn_FleetTokenPrivilegedRoleDenied(t *testing.T) {
	ms := newMockStore()
	makeRoleBead(ms, "gd-role-k8s-live", "k8s-live", true)
	ctx := withAuthMethod(context.Background(), authMethodStatic)

	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "k8s-live"))
	if err == nil {
		t.Fatal("fleet-token caller spawning a privileged role should be denied")
	}
}

func TestAuthorizeAgentSpawn_AdminBypass(t *testing.T) {
	ms := newMockStore()
	ctx := oidcCtx("admin@example.com", []string{RoleAdmin})

	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "crew"))
	if err != nil {
		t.Fatalf("admin should bypass spawn auth, got: %v", err)
	}
}

func TestAuthorizeAgentSpawn_ViewerDenied(t *testing.T) {
	ms := newMockStore()
	ctx := oidcCtx("viewer@example.com", []string{RoleViewer})

	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "crew"))
	if err == nil {
		t.Fatal("viewer should be denied spawn")
	}
}

func TestAuthorizeAgentSpawn_NoUserBead(t *testing.T) {
	ms := newMockStore()
	ctx := oidcCtx("nobody@example.com", []string{RoleOperator})

	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "crew"))
	if err == nil {
		t.Fatal("should deny spawn when no user bead exists")
	}
}

func TestAuthorizeAgentSpawn_ProjectAllowed(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-user1", "alice@example.com", "gardenio,monorepo", "crew")

	ctx := oidcCtx("alice@example.com", []string{RoleOperator})
	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "crew"))
	if err != nil {
		t.Fatalf("should allow spawn for authorized project/role, got: %v", err)
	}
}

func TestAuthorizeAgentSpawn_ProjectDenied(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-user2", "bob@example.com", "monorepo", "crew")

	ctx := oidcCtx("bob@example.com", []string{RoleOperator})
	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "crew"))
	if err == nil {
		t.Fatal("should deny spawn for unauthorized project")
	}
}

func TestAuthorizeAgentSpawn_RoleDenied(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-user3", "carol@example.com", "gardenio", "thread")

	ctx := oidcCtx("carol@example.com", []string{RoleOperator})
	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "crew"))
	if err == nil {
		t.Fatal("should deny spawn for unauthorized role")
	}
}

func TestAuthorizeAgentSpawn_WildcardProject(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-user4", "dave@example.com", "*", "crew,gardener")

	ctx := oidcCtx("dave@example.com", []string{RoleOperator})
	err := authorizeAgentSpawn(ctx, ms, agentFields("any-project", "crew"))
	if err != nil {
		t.Fatalf("wildcard project should allow any project, got: %v", err)
	}
}

func TestAuthorizeAgentSpawn_WildcardRole(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-user6", "frank@example.com", "gardenio", "*")

	ctx := oidcCtx("frank@example.com", []string{RoleOperator})
	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "anything"))
	if err != nil {
		t.Fatalf("wildcard role should allow any role, got: %v", err)
	}
}

func TestAuthorizeAgentSpawn_EmptyPermissionsAllowAll(t *testing.T) {
	ms := newMockStore()
	makeUserBead(ms, "gd-user5", "eve@example.com", "", "")

	ctx := oidcCtx("eve@example.com", []string{RoleOperator})
	err := authorizeAgentSpawn(ctx, ms, agentFields("gardenio", "crew"))
	if err != nil {
		t.Fatalf("empty permissions should allow all, got: %v", err)
	}
}

func TestParseStringList(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"json_array", `["a","b","c"]`, []string{"a", "b", "c"}},
		{"comma_string", `"a,b,c"`, []string{"a", "b", "c"}},
		{"single_string", `"gardenio"`, []string{"gardenio"}},
		{"wildcard", `"*"`, []string{"*"}},
		{"empty_string", `""`, nil},
		{"null", `null`, nil},
		{"empty", ``, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseStringList(json.RawMessage(tc.raw))
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d; got=%v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestContainsOrWildcard(t *testing.T) {
	tests := []struct {
		vals []string
		item string
		want bool
	}{
		{[]string{"a", "b"}, "a", true},
		{[]string{"a", "b"}, "c", false},
		{[]string{"*"}, "anything", true},
		{nil, "anything", false},
		{[]string{}, "anything", false},
	}
	for _, tc := range tests {
		got := containsOrWildcard(tc.vals, tc.item)
		if got != tc.want {
			t.Errorf("containsOrWildcard(%v, %q) = %v, want %v", tc.vals, tc.item, got, tc.want)
		}
	}
}
