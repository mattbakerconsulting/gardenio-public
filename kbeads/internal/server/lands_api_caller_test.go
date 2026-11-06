package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/landmap"
)

// The map's request counter labels each caller by class (gd-AQQaiE8IMd): a
// land's own token is its agent, a land member's sign-in a member, and every
// other caller an operator, whose reads are not a land's use.
func TestLandsForMap_Caller(t *testing.T) {
	oidc := func(roles ...string) context.Context {
		return WithRoles(withAuthMethod(context.Background(), authMethodOIDC), roles)
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want landmap.Caller
	}{
		{"land principal", withAuthMethod(context.Background(), authMethodLand), landmap.CallerAgent},
		{"land member", oidc(RoleProjectMember), landmap.CallerMember},
		{"land-audience token", withTokenLands(oidc(RoleProjectMember), []string{"acme"}), landmap.CallerMember},
		{"admin", oidc(RoleAdmin, RoleProjectMember), landmap.CallerOperator},
		{"operator sign-in", oidc(RoleOperator), landmap.CallerOperator},
		{"static token", withAuthMethod(context.Background(), authMethodStatic), landmap.CallerOperator},
		{"edge envelope", withAuthMethod(context.Background(), authMethodEdge), landmap.CallerOperator},
		{"machine", withAuthMethod(context.Background(), authMethodMachine), landmap.CallerOperator},
	} {
		if got := (landsForMap{}).Caller(tc.ctx); got != tc.want {
			t.Errorf("%s: Caller = %q, want %q", tc.name, got, tc.want)
		}
	}
}
