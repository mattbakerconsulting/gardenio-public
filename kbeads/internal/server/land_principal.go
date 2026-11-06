package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Land principals (10.11 gd-x0kJF1wHk7, contract gd-EWSKPH7pa5 §8 step 1). A
// per-land process (landgate, the land map's agent listener) holds a token
// that kbeads scopes to that one land, so the process is a second wall rather
// than the only one: a bug in it cannot reach another land's beads.
//
// The credential is a bridge/fleet-token registration named "land-<land>"
// whose project binding is <land>; it is minted, expired, refreshed and
// revoked through /v1/bridge-tokens like any other. It never resolves as a
// fleet token: AuthMiddleware turns it into a scoped principal with its own
// auth method, authMethodLand, holding the roles and the one project group a
// land member holds, so every route class, read filter and write gate a
// scoped human goes through applies to it unchanged. Its tier is tierLand
// (action_table.go), and its identity type is "land".
//
// authMethodLand is not authMethodOIDC. A gate that asks "is this a human with
// a Keycloak bearer?" (== authMethodOIDC: escalation approval, decision
// answers, user provisioning, devices, terms, Slack linking, admin checks)
// refuses it without knowing it exists. Only the scoped-principal engine
// (isScopedPrincipal, scopeFromContext) admits it. The opposite test,
// "!= authMethodOIDC means a trusted service token", would let it bypass, so
// those gates ask roleBearing instead; TestAuthMethodOIDC_Guard pins both.

// authMethodLand marks a land principal (see above).
const authMethodLand authMethod = "land"

// roleBearing reports whether a caller's realm roles are server-verified and
// must be enforced: an OIDC human, a land principal (whose roles are
// landPrincipalRoles, set from the credential), or a window principal (which
// holds none, so every role gate refuses it: window_principal.go). Every other
// method is a service token, an edge envelope or auth-disabled mode, which the
// role and read gates bypass or scope by token instead. A gate that treats
// "not OIDC" as "trusted service" tests !roleBearing, never != authMethodOIDC.
func roleBearing(method authMethod) bool {
	return method == authMethodOIDC || method == authMethodLand || method == authMethodWindow
}

// landFleetPrefix names the fleet namespace of land-principal tokens.
const landFleetPrefix = "land-"

// landSubjectPrefix prefixes a land principal's AuthIdentity.Subject
// ("land:<land>"). It cannot collide with an OIDC subject, which is an email.
const landSubjectPrefix = "land:"

// landPrincipalContextKey holds the land a land principal is bound to.
const landPrincipalContextKey contextKey = "land.principal"

// landPrincipalRoles are the realm roles a land principal acts with: those of
// a land member who can write (route gates need RoleOperator for writes).
// Never RoleAdmin.
var landPrincipalRoles = []string{RoleViewer, RoleOperator, RoleProjectMember}

var (
	errLandTokenUnbound    = errors.New("land principal token is not bound to its land")
	errLandTokenPrivileged = errors.New("land principal token may not be privileged")
)

// landOfFleet returns the land a "land-<land>" fleet name is for.
func landOfFleet(fleet string) (string, bool) {
	land, ok := strings.CutPrefix(fleet, landFleetPrefix)
	return land, ok && land != ""
}

// landPrincipalFromContext returns the land the caller is a land principal
// for, false for every other caller.
func landPrincipalFromContext(ctx context.Context) (string, bool) {
	v, _ := ctx.Value(landPrincipalContextKey).(string)
	return v, v != ""
}

// landPrincipalForToken reports whether a registered token is a land
// principal and, if so, which land. An error means it is in the land-*
// namespace but its registration does not bind it to exactly that land (or
// marks it privileged): the request is refused rather than served as a
// fleet token.
func landPrincipalForToken(fleetCfg *FleetConfig, token string) (string, bool, error) {
	if fleetCfg == nil || !fleetCfg.HasToken(token) {
		return "", false, nil
	}
	land, ok := landOfFleet(fleetCfg.ResolveFleet(token))
	if !ok {
		return "", false, nil
	}
	if fleetCfg.ResolveProject(token) != land {
		return "", false, errLandTokenUnbound
	}
	if fleetCfg.ResolvePrivileged(token) {
		return "", false, errLandTokenPrivileged
	}
	return land, true, nil
}

// landPrincipalContext stamps a land principal for land onto ctx. No request
// header is honored for it: its identity and land come from the credential.
// It carries no fleet-token project binding (WithTokenProject): that is fleet
// semantics, and the scoped-principal checks already confine it to land.
func landPrincipalContext(ctx context.Context, fleetCfg *FleetConfig, land string) context.Context {
	ctx = withAuthMethod(ctx, authMethodLand)
	ctx = WithRoles(ctx, landPrincipalRoles)
	ctx = WithGroups(ctx, []string{"/" + groupProjectPrefix + land})
	ctx = context.WithValue(ctx, landPrincipalContextKey, land)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "land", Subject: landSubjectPrefix + land})
	if fleetCfg != nil {
		ctx = WithFleet(ctx, fleetCfg.DefaultFleet)
	}
	return ctx
}

// validateLandFleetRegistration refuses a land-* registration that would not
// resolve as a land principal, so a bad mint fails at the mint and not on
// first use. The project must be the land, and nothing may widen or blur it:
// no privileged flag, no subject (the identity is land:<land>), no type or
// operation scope (actionTable is its scope).
func validateLandFleetRegistration(req registerBridgeTokenRequest) error {
	if !strings.HasPrefix(req.Fleet, landFleetPrefix) {
		return nil
	}
	land, ok := landOfFleet(req.Fleet)
	switch {
	case !ok || strings.ContainsAny(land, "/:@"):
		return fmt.Errorf("bridge %q: a land principal is named %s<land>, where <land> is a project name", req.Fleet, landFleetPrefix)
	case req.Project != land:
		return fmt.Errorf("bridge %q is a land principal: project must be %q", req.Fleet, land)
	case req.Privileged:
		return fmt.Errorf("bridge %q is a land principal: it may not be privileged", req.Fleet)
	case req.Subject != "":
		return fmt.Errorf("bridge %q is a land principal: its subject is %s%s and may not be set", req.Fleet, landSubjectPrefix, land)
	case len(req.AllowedTypes) > 0 || len(req.AllowedOps) > 0:
		return fmt.Errorf("bridge %q is a land principal: allowed_types and allowed_ops do not apply, the land tier is its scope", req.Fleet)
	}
	return nil
}
