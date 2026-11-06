package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// Become (plan gd-0SUIdKXWV3 step 3): the /garden body says data-admin to a gardenio admin only. The flag is the admin
// role itself, not "unscoped": a scoped tenant is no admin, and neither is an operator or a viewer, whom scopeFromRequest
// leaves unscoped.
func TestGardenAdminFlagIsTheAdminRoleNotUnscoped(t *testing.T) {
	cases := []struct {
		name          string
		roles, groups []string
		scoped, admin bool
	}{
		{"scoped tenant", []string{oidcauth.RoleProjectMember}, []string{"/project-phonobaloon/owners"}, true, false},
		{"unscoped operator", []string{oidcauth.RoleOperator}, nil, false, false},
		{"unscoped viewer", []string{oidcauth.RoleViewer}, nil, false, false},
		{"admin", []string{oidcauth.RoleAdmin}, nil, false, true},
	}
	h := &gardenhub.Handler{Hub: gardenhub.New(nil, "/v", nil), BasePath: "/v", IsAdmin: viewerCanAdmin, BecomeURL: defaultBecomeURL}
	mux := http.NewServeMux()
	h.Register(mux)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/garden", nil)
			ctx := oidcauth.ContextWithOIDCVerified(r.Context())
			ctx = oidcauth.ContextWithRoles(ctx, c.roles)
			ctx = oidcauth.ContextWithGroups(ctx, c.groups)
			r = r.WithContext(ctx)
			if got := scopeFromRequest(r).Scoped(); got != c.scoped {
				t.Fatalf("scoped = %v, want %v", got, c.scoped)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			body := rec.Body.String()
			if got := strings.Contains(body, ` data-admin="1"`); got != c.admin {
				t.Errorf("data-admin on the body = %v, want %v", got, c.admin)
			}
			if got := strings.Contains(body, ` data-become="`+defaultBecomeURL+`"`); got != c.admin {
				t.Errorf("data-become on the body = %v, want %v", got, c.admin)
			}
		})
	}
}
