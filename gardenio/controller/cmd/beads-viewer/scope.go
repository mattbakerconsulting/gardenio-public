package main

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// viewerScope is what the signed-in human may see through the viewer.
//
// It mirrors kbeads' scoped-principal rule (kbeads/internal/server
// scoped_principal.go) so the viewer and the daemon agree on who is confined:
// a *scoped principal* is an OIDC human that is not a gardenio-admin and holds
// gardenio-project-member. Everyone else (operators, viewers, admins, the dev
// bypass, anonymous probes) is unrestricted here exactly as before; the daemon
// still applies its own per-token rules to them.
//
// Admin is therefore "not a scoped principal", not literally the admin role:
// the field name follows the task's interface, and Scoped() is the spelling to
// use at call sites.
//
// An *applicant* (Lands Track B B-D1) is the narrowest caller: a human signed
// in through a verified OIDC token who holds no gardenio role at all (no
// viewer, operator, admin or project-member), a stranger who registered with
// GitHub. kbeads refuses such a caller on every bead route; the viewer lets
// them reach only the public and routeApplicant routes (the Lands front of
// house). An applicant is scoped, with no projects.
type viewerScope struct {
	Admin     bool
	Applicant bool
	Projects  []string
	// Roles is the scoped principal's land role in each of Projects, from the
	// same groups (conversation.LandRolesFromGroups). Nil when Admin.
	Roles map[string]conversation.LandRole
}

// Scoped reports whether the caller is confined to Projects.
func (s viewerScope) Scoped() bool { return !s.Admin }

// allows reports whether the scope admits a bead of the given project. An
// unrestricted caller admits every project; a scoped one only its own.
func (s viewerScope) allows(project string) bool {
	if s.Admin {
		return true
	}
	return slices.Contains(s.Projects, project)
}

// landRole is the caller's role in the land: LandRoleOperator for an
// unrestricted caller, LandRoleNone for a land outside a scoped caller's
// projects.
func (s viewerScope) landRole(project string) conversation.LandRole {
	if s.Admin {
		return conversation.LandRoleOperator
	}
	return s.Roles[project]
}

// may reports whether the caller may take action in the land, by the action
// table (conversation.Allows). A Commons write route checks it for the land it
// writes into; kbeads applies the same roles to the caller's own bearer.
func (s viewerScope) may(project string, action conversation.Action) bool {
	return conversation.Allows(s.landRole(project), action)
}

// mayInChannel is may for an action inside one channel of the land: saying or
// replying in a members-only channel (a user channel or a DM) also needs self,
// the caller's oidc:<email>, among its members (conversation.ChannelAllows,
// Lands Commons 4.8). kbeads refuses the same message on the caller's bearer.
func (s viewerScope) mayInChannel(project, self string, ch conversation.Channel, action conversation.Action) bool {
	return conversation.ChannelAllows(s.landRole(project), self, ch, action)
}

// allowsAny reports whether any of the labels is a project: label the scope
// admits. A scoped caller's bead with no project label is not admitted: nothing
// says whose it is (the daemon's read filter treats it the same way).
func (s viewerScope) allowsAny(labels []string) bool {
	if s.Admin {
		return true
	}
	for _, l := range labels {
		if p, ok := strings.CutPrefix(l, "project:"); ok && s.allows(p) {
			return true
		}
	}
	return false
}

// scopeFromRequest resolves the caller's scope from the verified roles and
// groups the OIDC middleware put in the request context. It never reads a raw
// header: those are client-supplied on some edges. A scoped principal whose
// token carries no project groups gets an empty Projects, which admits nothing.
// A group "/project-claimlayer" (member), "/project-claimlayer/owners" or
// "/project-claimlayer/guests" names project "claimlayer", the same reading as
// kbeads' groupsToProjects.
//
// A verified OIDC caller with no gardenio role is an applicant. The check needs
// the OIDC-verified mark, not just an empty role list: the dev bypass, AVA with
// no floor and unauthenticated internal calls also arrive with no roles, and
// stay unrestricted as before. (A dev-bypass call that skipped the edge is
// unrestricted here too, but scopeMiddleware lets it reach only the public and
// machine routes.)
func scopeFromRequest(r *http.Request) viewerScope {
	ctx := r.Context()
	roles := oidcauth.RolesFromContext(ctx)
	if isApplicant(ctx, roles) {
		return viewerScope{Applicant: true}
	}
	if oidcauth.HasMinimumRole(roles, oidcauth.RoleAdmin) || !slices.Contains(roles, oidcauth.RoleProjectMember) {
		return viewerScope{Admin: true}
	}
	landRoles := conversation.LandRolesFromGroups(oidcauth.GroupsFromContext(ctx))
	var projects []string
	for p := range landRoles {
		projects = append(projects, p)
	}
	slices.Sort(projects)
	return viewerScope{Projects: projects, Roles: landRoles}
}

// isApplicant reports whether a caller with these roles is a Lands applicant:
// OIDC-verified and holding neither a ranked gardenio role nor
// gardenio-project-member. Keycloak's own realm roles (offline_access,
// default-roles-*) grant nothing and do not count.
func isApplicant(ctx context.Context, roles []string) bool {
	return oidcauth.OIDCVerifiedFromContext(ctx) &&
		!oidcauth.HasMinimumRole(roles, oidcauth.RoleViewer) &&
		!slices.Contains(roles, oidcauth.RoleProjectMember)
}

// gardenFarmCut is the requester's cut of the farm document and stream (gardenhub.Handler.Cut, gd-6UG1OGlqYg,
// gd-UKO0ZpXeha): the whole farm for an unrestricted caller, else its projects. An applicant is scoped with no
// projects, an empty farm.
func gardenFarmCut(r *http.Request) gardenhub.FarmCut {
	sc := viewerScopeFromContext(r.Context())
	if !sc.Scoped() {
		return gardenhub.FarmCut{}
	}
	return gardenhub.FarmCut{Scoped: true, Projects: sc.Projects}
}

// gardenInLand is gardenhub.Handler.InLand, the cut of a tenant land's credit charts (GET /garden/charts/costs-land,
// plan gd-gh5J0vVbJP step 3.3): every land for an unrestricted caller, a scoped one's own lands, none for an applicant.
func gardenInLand(r *http.Request, land string) bool {
	return viewerScopeFromContext(r.Context()).allows(land)
}

type viewerScopeKey struct{}

// withViewerScope returns ctx carrying sc.
func withViewerScope(ctx context.Context, sc viewerScope) context.Context {
	return context.WithValue(ctx, viewerScopeKey{}, sc)
}

// viewerScopeFromContext returns the scope scopeMiddleware attached. A context
// without one (startup, reactors, tests that skip the middleware) is
// unrestricted: those paths have no signed-in human to confine.
func viewerScopeFromContext(ctx context.Context) viewerScope {
	if sc, ok := ctx.Value(viewerScopeKey{}).(viewerScope); ok {
		return sc
	}
	return viewerScope{Admin: true}
}

// isScopedCtx reports whether ctx belongs to a request from a scoped principal.
// The shared caches and hubs use it as their single switch: they serve and fill
// only for unrestricted callers, so a value built for one audience is never
// handed to another.
func isScopedCtx(ctx context.Context) bool {
	return viewerScopeFromContext(ctx).Scoped()
}

// scopeMiddleware attaches the caller's viewerScope to the request context and
// enforces viewerRouteClasses: a scoped caller reaches only scoped, applicant
// and public routes (and, for the per-project pages, only its own project); an
// applicant reaches only applicant and public routes; an admin-only or
// unclassified route is a 403.
//
// patternOf returns the mux pattern the request will match. It cannot be read
// from r.Pattern here: the mux fills that in on the request it dispatches, which
// is downstream of this middleware. main passes appMux.Handler.
//
// A routeMachine route is refused to every caller machineMiddleware did not
// admit, unrestricted ones included; without machineMiddleware in the chain it
// is refused to everyone.
//
// A direct call with no edge proof (withNoEdgeProof: the dev bypass has an edge
// secret and the request did not carry it) reaches only routePublic and the
// routeMachine routes machineMiddleware admitted it to (gd-ozc7DSwCXO). It has
// no identity, so its scope is unrestricted, and every other route would serve
// it the shared hubs built on the viewer's system read: every land's beads.
// The one exception is a read-only verifier verifierMiddleware admitted (a GET
// on a fleet agent's own unrestricted token, verifier.go, gd-6ujfeTZfNL), and
// only on a route verifierAdmits: a stream a verifier's GET /garden/live/events
// carries comes back through here on that request, past verifierMiddleware.
//
// It runs after operatorMiddleware, which has already put the user bearer on the
// context, and fails closed: a route missing from the table is admin-only.
func scopeMiddleware(next http.Handler, patternOf func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc := scopeFromRequest(r)
		r = r.WithContext(withViewerScope(r.Context(), sc))
		pattern := patternOf(r)
		class := viewerRouteClasses[pattern]
		if class == routeMachine {
			if _, ok := machineCallerFromContext(r.Context()); !ok {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}
		if noEdgeProofFromContext(r.Context()) && class != routePublic && class != routeMachine && !(isVerifier(r.Context()) && verifierAdmits(r.Method, pattern)) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if sc.Scoped() && !scopedRouteAllowed(sc, pattern, r.URL.Path) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
