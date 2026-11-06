package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// Per-land Keycloak audiences (owner decision gd-zf6eceweED, task
// gd-eAfptUBh9Z). Every land has an optional client scope land-<name> whose
// audience mapper adds aud "land-<name>" to the access token, so a token can
// be minted for one land. A land audience narrows the token:
//
//   - exactly one land audience: the principal is scoped to that land alone,
//     whatever /project-* groups the token lists, and only while the token's
//     groups still make it a member of that land (scopeFromContext);
//   - two or more: the token is worth no land at all (empty scope);
//   - none: unchanged, the token's groups decide as before.
//
// A token with a land audience is always a scoped principal and carries no
// global role (landAudienceRoles), so an admin who asks for a land token gets
// a land token, not an admin one.
//
// The land door (/land/<name>/v1/* on the edge, and each land's landgate) sets
// LandDoorHeader to the land in the path. landDoorMiddleware then requires the
// token's one land audience to be that land. The header can only take access
// away, never grant it, so it needs no signature: a caller that sets it on
// another path narrows itself, and one that omits it is no wider than today.

// LandAudiencePrefix prefixes every per-land audience: aud "land-claimlayer"
// is a token minted for the land claimlayer.
const LandAudiencePrefix = "land-"

// LandDoorHeader names the land a request came through the land door for. The
// edge sets it from the path (overwriting anything the client sent) and
// landgate sets it to its own land.
const LandDoorHeader = "X-Land-Door"

// LandDoorCheckHeader marks the land door's own admission check, the GET
// /v1/me the edge's /oauth2/auth-land-token and landgate's admitLandToken
// send, so the rate limit can tell it from a caller's (isLandDoorAuthCheck).
// Both blank it on every request they forward.
const LandDoorCheckHeader = "X-Land-Door-Check"

// landDoorNamePattern is the Lands contract's LandName, the pattern the edge
// captures the door's land with (public-edge.yaml $land_door_name).
var landDoorNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// AudienceExtractor extends TokenVerifier to return the token's whole "aud"
// claim, which is where the per-land audiences live.
type AudienceExtractor interface {
	TokenVerifier
	ExtractAudiences(ctx context.Context, rawToken string) ([]string, error)
}

// landAudiences returns the lands named by the land- audiences in auds,
// sorted and deduped. Any land- audience counts, even one that is not a valid
// land name: it still makes the token a land token, and names no land a
// caller can be a member of.
func landAudiences(auds []string) []string {
	var lands []string
	for _, a := range auds {
		if land, ok := strings.CutPrefix(a, LandAudiencePrefix); ok {
			lands = append(lands, land)
		}
	}
	slices.Sort(lands)
	return slices.Compact(lands)
}

const tokenLandsContextKey contextKey = "oidc.token_lands"

// withTokenLands records the lands the request's OIDC token was minted for.
func withTokenLands(ctx context.Context, lands []string) context.Context {
	return context.WithValue(ctx, tokenLandsContextKey, lands)
}

// tokenLandsFromContext returns the lands the caller's OIDC token was minted
// for (its land- audiences), sorted; nil for every other caller.
func tokenLandsFromContext(ctx context.Context) []string {
	if authMethodFromContext(ctx) != authMethodOIDC {
		return nil
	}
	v, _ := ctx.Value(tokenLandsContextKey).([]string)
	return v
}

// errTokenAudiences refuses a token whose audiences cannot be read: it may be
// a land token, and treating it as one without a land would keep its global
// roles, so the auth paths reject it instead (fail closed).
var errTokenAudiences = errors.New("invalid token: cannot read its audiences")

// oidcTokenLands returns the lands a verified OIDC token was minted for. A
// verifier that cannot report audiences at all names none; one that fails to
// read this token's audiences returns errTokenAudiences, and the caller must
// refuse the token.
func oidcTokenLands(ctx context.Context, v TokenVerifier, rawToken string) ([]string, error) {
	av, ok := v.(AudienceExtractor)
	if !ok {
		return nil, nil
	}
	auds, err := av.ExtractAudiences(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errTokenAudiences, err)
	}
	return landAudiences(auds), nil
}

// hasLandAudience reports whether the caller's OIDC token named any land.
func hasLandAudience(ctx context.Context) bool {
	return len(tokenLandsFromContext(ctx)) > 0
}

// tokenLand returns the one land an OIDC token was minted for, and false for a
// token with none or with several.
func tokenLand(ctx context.Context) (string, bool) {
	lands := tokenLandsFromContext(ctx)
	if len(lands) != 1 {
		return "", false
	}
	return lands[0], true
}

// landAudienceRoles drops every global role (viewer, operator, admin and any
// other realm role) from a land token's roles, keeping only RoleProjectMember.
// A token without a land audience keeps its roles.
func landAudienceRoles(roles, lands []string) []string {
	if len(lands) == 0 {
		return roles
	}
	if slices.Contains(roles, RoleProjectMember) {
		return []string{RoleProjectMember}
	}
	return []string{}
}

// landTokenIsMember reports whether a land token's caller holds
// RoleProjectMember, the role landgate also requires (cmd/landgate gate.go
// admit). A land token without it is still a scoped principal, so it is never
// unrestricted, but it reaches no land and no scoped route.
func landTokenIsMember(ctx context.Context) bool {
	return slices.Contains(RolesFromContext(ctx), RoleProjectMember)
}

// narrowToTokenLand applies a land token's audience to its group-derived
// scope: one land audience keeps that land only (if the groups still admit
// the caller to it and it holds RoleProjectMember), and several keep nothing.
func narrowToTokenLand(ctx context.Context, d scopeDecision) scopeDecision {
	if !hasLandAudience(ctx) {
		return d
	}
	out := scopeDecision{Tiers: map[string]tier{}}
	land, ok := tokenLand(ctx)
	if !ok || !landTokenIsMember(ctx) {
		return out
	}
	if t, member := d.Tiers[land]; member {
		out.Projects = []string{land}
		out.Tiers[land] = t
	}
	return out
}

// landDoorMiddleware refuses a request that came through the land door
// (LandDoorHeader set) unless its credential is for exactly that land: an
// OIDC token whose one land audience is the door's land, a land principal
// of that land, or the per-agent token of that tenant land's own-machine
// agent (land_door_agent.go). Every other caller, a token with no land audience or several
// included, gets 403. A request without the header passes untouched.
func (s *BeadsServer) landDoorMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vals := r.Header.Values(LandDoorHeader)
		if len(vals) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if denial := s.landDoorDenial(r.Context(), vals); denial != "" {
			writeError(w, http.StatusForbidden, denial)
			return
		}
		next.ServeHTTP(w, r.WithContext(withLandDoor(r.Context(), vals[0])))
	})
}

// landDoorDenial returns why a land-door request is refused, or "" to admit
// it. vals is every LandDoorHeader value on the request.
func (s *BeadsServer) landDoorDenial(ctx context.Context, vals []string) string {
	if len(vals) != 1 || !landDoorNamePattern.MatchString(vals[0]) {
		return "land door: malformed " + LandDoorHeader
	}
	door := vals[0]
	if land, ok := landPrincipalFromContext(ctx); ok && authMethodFromContext(ctx) == authMethodLand {
		if land == door {
			return ""
		}
		return "land door: credential is for another land"
	}
	if denial, ok := s.landDoorAgentDenial(ctx, door); ok {
		return denial
	}
	if authMethodFromContext(ctx) != authMethodOIDC {
		return "land door: a land bearer token is required"
	}
	switch lands := tokenLandsFromContext(ctx); {
	case len(lands) == 0:
		return "land door: token was not minted for a land (request scope " + LandAudiencePrefix + door + ")"
	case len(lands) > 1:
		return "land door: token was minted for more than one land"
	case lands[0] != door:
		return "land door: token was minted for another land"
	}
	return ""
}
