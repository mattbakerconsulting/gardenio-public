package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// glassBeads is a gardenhub.BeadReader holding one agent per land.
type glassBeads struct{}

func (glassBeads) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	project := map[string]string{"gd-ours": "claimlayer", "gd-theirs": "gardenio"}[id]
	if project == "" {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound}
	}
	return &beadsapi.BeadDetail{ID: id, Type: "agent", Labels: []string{"project:" + project}, Fields: map[string]string{"project": project}}, nil
}
func (glassBeads) GetComments(context.Context, string) ([]beadsapi.Comment, error) { return nil, nil }
func (glassBeads) GetDependencies(context.Context, string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (glassBeads) GetReverseDependencies(context.Context, string) ([]beadsapi.Dependency, error) {
	return nil, nil
}

// The Glass reads through the viewer's own scope middleware and wiring: a scoped principal sees an agent of its own
// land and gets a 404 for another land's, the same answer as for a missing agent; an unrestricted operator sees both.
func TestGlassRoutesScopedPerLand(t *testing.T) {
	g := &gardenhub.Handler{Beads: glassBeads{}, Glass: &gardenhub.Glass{
		InScope: func(r *http.Request, project string) bool { return viewerScopeFromContext(r.Context()).allows(project) },
		Who:     glassIdentity,
	}}
	mux := http.NewServeMux()
	g.Register(mux)
	h := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })

	scopedOperator := principal{email: "cl@claimlayer.example", roles: []string{oidcauth.RoleProjectMember, oidcauth.RoleOperator},
		groups: []string{"/project-claimlayer"}}
	operator := principal{email: "op@example.com", roles: []string{oidcauth.RoleOperator}}
	for _, c := range []struct {
		name  string
		p     principal
		agent string
		want  int
	}{
		{"scoped, own land", scopedOperator, "gd-ours", http.StatusOK},
		{"scoped, other land", scopedOperator, "gd-theirs", http.StatusNotFound},
		{"scoped, missing", scopedOperator, "gd-nope", http.StatusNotFound},
		{"scoped member without rank", scopedPrincipal(), "gd-ours", http.StatusForbidden},
		{"operator, any land", operator, "gd-theirs", http.StatusOK},
	} {
		for _, route := range []string{"", "/stream"} {
			want := c.want
			if route == "/stream" && want == http.StatusOK {
				want = http.StatusNotFound // these agents have no session; the stream says so after the checks pass
			}
			if code := serve(h, http.MethodGet, "/garden/glass/"+c.agent+route, c.p); code != want {
				t.Errorf("%s%s: %d, want %d", c.name, route, code, want)
			}
		}
	}
}

func TestGlassIdentityKeepsOnlyCanonicalRoles(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/garden/glass/gd-x", nil)
	ctx := oidcauth.ContextWithEmail(r.Context(), "Op@Example.com")
	ctx = oidcauth.ContextWithRoles(ctx, []string{"offline_access", oidcauth.RoleProjectMember, oidcauth.RoleOperator, "default-roles-gardenio", oidcauth.RoleViewer})
	got := glassIdentity(r.WithContext(ctx))
	want := coopmuxapi.Identity{Operator: "op@example.com", AuthMode: "cookie", Roles: []string{oidcauth.RoleOperator, oidcauth.RoleViewer}}
	if got.Operator != want.Operator || got.AuthMode != want.AuthMode || !slices.Equal(got.Roles, want.Roles) {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}

// glassPoolFake is coopmux's credential pool behind the Glass.
type glassPoolFake struct{}

func (glassPoolFake) CredentialPool(context.Context) (*coopmuxapi.CredentialPool, error) {
	return &coopmuxapi.CredentialPool{Accounts: []coopmuxapi.PoolAccount{{Name: "max-a", Status: "healthy", HasAPIKey: true}}, TotalAccounts: 1, HealthyAccounts: 1, Strategy: "least_loaded"}, nil
}

// The credential pool (GL-15) is the whole fleet's: a scoped principal is refused by the scope middleware, an
// unrestricted operator by the handler's admin tier, and only an admin reads it. It does not shadow an agent
// whose id would be "credentials": the route is its own, before {agent}.
func TestGlassCredentialsAdminOnly(t *testing.T) {
	g := &gardenhub.Handler{Beads: glassBeads{}, Glass: &gardenhub.Glass{
		InScope: func(r *http.Request, project string) bool { return viewerScopeFromContext(r.Context()).allows(project) },
		Who:     glassIdentity,
		Pool:    glassPoolFake{},
	}}
	mux := http.NewServeMux()
	g.Register(mux)
	h := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })

	scopedOperator := principal{email: "cl@claimlayer.example", roles: []string{oidcauth.RoleProjectMember, oidcauth.RoleOperator},
		groups: []string{"/project-claimlayer"}}
	for _, c := range []struct {
		name string
		p    principal
		want int
	}{
		{"scoped operator", scopedOperator, http.StatusForbidden},
		{"scoped member", scopedPrincipal(), http.StatusForbidden},
		{"operator", principal{email: "op@example.com", roles: []string{oidcauth.RoleOperator}}, http.StatusForbidden},
		{"admin", adminPrincipal(), http.StatusOK},
	} {
		if code := serve(h, http.MethodGet, "/garden/glass/credentials", c.p); code != c.want {
			t.Errorf("%s: %d, want %d", c.name, code, c.want)
		}
	}
	if _, p := mux.Handler(httptest.NewRequest(http.MethodGet, "/garden/glass/credentials", nil)); p != "GET /garden/glass/credentials" {
		t.Errorf("the pool is served by %q, want its own route", p)
	}
}
