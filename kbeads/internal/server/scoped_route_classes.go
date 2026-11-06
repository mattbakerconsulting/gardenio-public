package server

import (
	"context"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gardenio/kbeads/internal/store"
)

// routeScopeClass is how one HTTP route behaves for a scoped principal (an OIDC
// human holding RoleProjectMember; see isScopedPrincipal).
type routeScopeClass int

const (
	// classFiltered: the handler narrows its output (or its target) to the
	// caller's project set itself, via scopeListFilter / oidcAllowedProjects /
	// eventReadScope / authorizeBeadRead / authorizeSubresourceRead, or, for a
	// per-bead write, via the write gate. scopedRouteGate lets it through.
	classFiltered routeScopeClass = iota + 1
	// classAdminOnly: the route is global, cross-project or privileged, with no
	// per-project cut. scopedRouteGate refuses it for a scoped principal.
	classAdminOnly
	// classScopedSafe: the route returns no bead or cross-project data.
	classScopedSafe
)

// scopedRouteClasses classifies every route registerRoutes registers, keyed by
// the exact mux pattern string. TestEveryRouteHasScopedClass fails the build
// when a registered route is missing here, and scopedRouteGate treats a missing
// route as classAdminOnly, so a new route is closed to scoped principals until
// someone classifies it.
//
// When a handler gains a per-project cut, move its route to classFiltered and
// add it to the TestScopedRoutes_Matrix list cases.
var scopedRouteClasses = map[string]routeScopeClass{
	// filtered to the scoped project set (handler consults scopeListFilter / oidcAllowedProjects / authorizeBeadRead / authorizeSubresourceRead, or is a per-bead write confined by the write gate).
	"GET /v1/beads":                              classFiltered,
	"GET /v1/beads/{id}":                         classFiltered,
	"GET /v1/beads/{id}/dependencies":            classFiltered,
	"GET /v1/beads/{id}/dependents":              classFiltered,
	"GET /v1/beads/{id}/mentioned-by":            classFiltered,
	"GET /v1/beads/{id}/mentions":                classFiltered,
	"GET /v1/beads/{id}/labels":                  classFiltered,
	"GET /v1/beads/{id}/comments":                classFiltered,
	"GET /v1/beads/{id}/followers":               classFiltered,
	"GET /v1/beads/{id}/taints":                  classFiltered,
	"GET /v1/beads/{id}/events":                  classFiltered,
	"GET /v1/beads/{id}/mutations":               classFiltered,
	"GET /v1/beads/{id}/operator-history":        classFiltered,
	"GET /v1/events":                             classFiltered,
	"GET /v1/beads/{id}/similar":                 classFiltered,
	"POST /v1/search/semantic":                   classFiltered,
	"GET /v1/graph":                              classFiltered,
	"POST /v1/graph":                             classFiltered,
	"GET /v1/ready":                              classFiltered,
	"GET /v1/decisions/{id}":                     classFiltered,
	"GET /v1/decisions":                          classFiltered,
	"GET /v1/escalations/{id}":                   classFiltered,
	"GET /v1/escalations/{id}/permission-diff":   classFiltered,
	"GET /v1/escalations":                        classFiltered,
	"GET /v1/releases":                           classFiltered, // scopeListFilter
	"GET /v1/releases/{id}":                      classFiltered, // authorizeBeadRead
	"POST /v1/releases/{id}/deployments":         classFiltered, // authorizeBeadRead + releaseDeployDenial (land owner only)
	"GET /v1/batch/beads":                        classFiltered,
	"GET /v1/batch/children-by-status":           classFiltered,
	"GET /v1/batch/dependencies":                 classFiltered,
	"GET /v1/agents/roster":                      classFiltered,
	"GET /v1/agents-by-name/{name}":              classFiltered,
	"GET /v1/lands/{land}/members":               classFiltered, // landAdmitsCaller (notification.go): 404 outside the scope
	"POST /v1/lands/{land}/steward-delegation":   classFiltered, // stewardDelegationDenial: the land's owner tier only
	"GET /v1/agents-by-name:batch":               classFiltered,
	"GET /v1/taints/{name}/beads":                classFiltered,
	"GET /v1/taints/{name}/scrub-requests":       classFiltered,
	"POST /v1/beads":                             classFiltered,
	"PATCH /v1/beads/{id}":                       classFiltered,
	"POST /v1/beads/{id}/close":                  classFiltered,
	"POST /v1/beads/{id}/increment":              classFiltered,
	"POST /v1/beads/{id}/fields/{field}/append":  classFiltered,
	"POST /v1/beads/{id}/fields/{field}/reorder": classFiltered,
	"POST /v1/beads/{id}/fields/{field}/edit":    classFiltered,
	"POST /v1/beads/{id}/dependencies":           classFiltered,
	"DELETE /v1/beads/{id}/dependencies":         classFiltered,
	"POST /v1/beads/{id}/labels":                 classFiltered,
	"DELETE /v1/beads/{id}/labels/{label}":       classFiltered,
	"POST /v1/beads/{id}/comments":               classFiltered,
	"POST /v1/scrub-requests":                    classFiltered,
	"POST /v1/decisions/{id}/resolve":            classFiltered,
	"POST /v1/decisions/{id}/cancel":             classFiltered,
	"POST /v1/escalations/{id}/can-approve":      classFiltered,
	"POST /v1/escalations/{id}/approve":          classFiltered,
	"POST /v1/escalations/{id}/reject":           classFiltered,
	"POST /v1/escalations/{id}/result":           classFiltered,
	"POST /v1/escalations/{id}/audience":         classFiltered,
	"POST /v1/escalations/{id}/claim":            classFiltered,
	"POST /v1/escalations/{id}/claim-for":        classAdminOnly,
	"POST /v1/escalations/{id}/hand-up":          classFiltered,
	"POST /v1/scrub-requests/{id}/approve":       classFiltered,
	"POST /v1/scrub-requests/{id}/reject":        classFiltered,
	"POST /v1/scrub-requests/{id}/attest":        classFiltered,
	"GET /v1/events/stream":                      classFiltered,
	// Windows (window_routes.go): mint, list, revoke and preview admit only an
	// unrestricted operator or the owner tier of the plan's land
	// (windowManageDenial). The view route and its live stream serve only a
	// window principal, the two patterns scopedRouteGate lets a window reach.
	"POST /v1/windows":        classFiltered,
	"GET /v1/windows":         classFiltered,
	"DELETE /v1/windows/{id}": classFiltered,
	"GET /v1/windows/preview": classFiltered,
	windowViewPattern:         classFiltered,
	windowLivePattern:         classFiltered,
	// The Lands API (Door D.6, lands_api.go): the map admits only a caller
	// landAdmitsCaller lets read the path's land (404 otherwise) and reads and
	// writes through the daemon routes in landmap's route table, each cut by
	// its own scope gate. The verbs landmap defers answer 404.
	"GET /v1/lands/v1/lands/{land}":                                 classFiltered,
	"GET /v1/lands/v1/lands/{land}/plots":                           classFiltered,
	"GET /v1/lands/v1/lands/{land}/plots/{plot}":                    classFiltered,
	"GET /v1/lands/v1/lands/{land}/crew":                            classFiltered,
	"GET /v1/lands/v1/lands/{land}/crew/{member}":                   classFiltered,
	"GET /v1/lands/v1/lands/{land}/whoami":                          classFiltered,
	"GET /v1/lands/v1/lands/{land}/tickets":                         classFiltered,
	"POST /v1/lands/v1/lands/{land}/tickets":                        classFiltered,
	"GET /v1/lands/v1/lands/{land}/tickets/{ticket}":                classFiltered,
	"PATCH /v1/lands/v1/lands/{land}/tickets/{ticket}":              classFiltered,
	"POST /v1/lands/v1/lands/{land}/tickets/{ticket}/comments":      classFiltered,
	"POST /v1/lands/v1/lands/{land}/tickets/{ticket}/close":         classFiltered,
	"POST /v1/lands/v1/lands/{land}/tickets/{ticket}/reopen":        classFiltered,
	"GET /v1/lands/v1/lands/{land}/search/tickets":                  classFiltered,
	"GET /v1/lands/v1/lands/{land}/decisions":                       classFiltered,
	"GET /v1/lands/v1/lands/{land}/decisions/{decision}":            classFiltered,
	"POST /v1/lands/v1/lands/{land}/decisions/{decision}/resolve":   classFiltered,
	"POST /v1/lands/v1/lands/{land}/decisions/{decision}/cancel":    classFiltered,
	"POST /v1/lands/v1/lands/{land}/decisions/{decision}/comments":  classFiltered,
	"GET /v1/lands/v1/lands/{land}/escalations":                     classFiltered,
	"POST /v1/lands/v1/lands/{land}/escalations":                    classFiltered,
	"GET /v1/lands/v1/lands/{land}/escalations/{escalation}":        classFiltered,
	"GET /v1/lands/v1/lands/{land}/plans":                           classFiltered,
	"GET /v1/lands/v1/lands/{land}/plans/{plan}":                    classFiltered,
	"POST /v1/lands/v1/lands/{land}/plans/{plan}/answer":            classFiltered,
	"GET /v1/lands/v1/lands/{land}/threads":                         classFiltered,
	"GET /v1/lands/v1/lands/{land}/threads/{thread}":                classFiltered,
	"GET /v1/lands/v1/lands/{land}/threads/{thread}/messages":       classFiltered,
	"POST /v1/lands/v1/lands/{land}/threads/{thread}/messages":      classFiltered,
	"GET /v1/lands/v1/lands/{land}/budget":                          classFiltered,
	"GET /v1/lands/v1/lands/{land}/budget/history":                  classFiltered,
	"GET /v1/lands/v1/lands/{land}/releases":                        classFiltered,
	"GET /v1/lands/v1/lands/{land}/releases/{release}":              classFiltered,
	"POST /v1/lands/v1/lands/{land}/releases/{release}/deployments": classFiltered,
	"GET /v1/lands/v1/lands/{land}/events":                          classFiltered,
	// A member's own-machine agents (lands_agents.go): each judges the caller
	// itself (landAgentMayStart, landAgentMayRevoke) after the map's admit.
	"GET /v1/lands/v1/lands/{land}/agents":                  classFiltered,
	"POST /v1/lands/v1/lands/{land}/agents":                 classFiltered,
	"GET /v1/lands/v1/lands/{land}/agents/{agent}":          classFiltered,
	"POST /v1/lands/v1/lands/{land}/agents/{agent}/refresh": classFiltered,
	"POST /v1/lands/v1/lands/{land}/agents/{agent}/revoke":  classFiltered,
	// One picture of a decision's evidence (landmap/evidence.go), served only
	// when the decision's evidence, read as the caller, names it.
	"GET /v1/lands/v1/lands/{land}/decisions/{decision}/evidence/{image}": classFiltered,

	// returns no cross-project data.
	"GET /v1/whoami":           classScopedSafe,
	"GET /v1/heraldry/library": classScopedSafe,
	"GET /v1/me":               classScopedSafe,
	"POST /v1/auth/refresh":    classScopedSafe,
	// Pre-auth: the invite code is the credential; returns only the new runner.
	"POST /v1/runners/register": classScopedSafe,
	"GET /v1/health":            classScopedSafe,
	"GET /v1/health/live":       classScopedSafe,
	// Push devices (Lands 4.7b gd-a7U4AgwgCA): the handlers cut every
	// non-admin OIDC caller to their OWN device rows and refuse every
	// non-OIDC caller but machine:gardenio-push-bridge (http_devices.go), so a
	// scoped human can enrol a phone and sees nobody else's, and a per-agent
	// token gets 403. No bead data.
	"POST /v1/devices":           classScopedSafe,
	"GET /v1/devices":            classScopedSafe,
	"DELETE /v1/devices/{token}": classScopedSafe,

	// writes two fields on the caller's own user bead and returns only those;
	// a tenant must be able to accept a new terms version.
	"POST /v1/users/me/terms": classScopedSafe,
	"GET /v1/users/me/terms":  classScopedSafe,
	// reads and writes consent_push_to_mr_auto on the caller's own user bead.
	"POST /v1/users/me/consent-push": classScopedSafe,
	"GET /v1/users/me/consent-push":  classScopedSafe,
	// reads and writes read_cursors on the caller's own user bead only; a
	// tenant's unread badges need it.
	"GET /v1/users/me/read-cursors":  classScopedSafe,
	"POST /v1/users/me/read-cursors": classScopedSafe,
	// reads and stamps welcome_tour_seen_at on the caller's own user bead; a
	// tenant's first /garden visit runs the tour once.
	"GET /v1/users/me/welcome-tour":  classScopedSafe,
	"POST /v1/users/me/welcome-tour": classScopedSafe,

	// global, cross-project or privileged: refused for scoped principals.
	"GET /v1/stats": classAdminOnly,
	// Runners (local-agents Task 7): fleet machinery, not project data.
	"GET /v1/runners/{id}":                             classAdminOnly,
	"POST /v1/runners/{id}/heartbeat":                  classAdminOnly,
	"POST /v1/runners/{id}/revoke":                     classAdminOnly,
	"GET /v1/configs/{key...}":                         classAdminOnly,
	"GET /v1/configs":                                  classAdminOnly,
	"GET /v1/agents/{id}/gates":                        classAdminOnly,
	"GET /v1/health/snapshot":                          classAdminOnly,
	"GET /v1/bus/status":                               classAdminOnly,
	"GET /v1/ui/telemetry":                             classAdminOnly,
	"GET /v1/metrics/acceptance":                       classAdminOnly,
	"GET /v1/metrics/slow-tool-calls":                  classAdminOnly,
	"GET /v1/charts":                                   classAdminOnly,
	"GET /v1/charts/{chart_id}":                        classAdminOnly,
	"GET /v1/charts/uniqueness":                        classAdminOnly,
	"GET /v1/charts/engagement/activity":               classAdminOnly,
	"GET /v1/charts/engagement/spend":                  classAdminOnly,
	"GET /v1/charts/security/authz":                    classAdminOnly,
	"GET /v1/charts/security/decisions":                classAdminOnly,
	"GET /v1/charts/security/token-slo":                classAdminOnly,
	"GET /v1/charts/engagement/sessions":               classAdminOnly,
	"GET /v1/taints":                                   classAdminOnly,
	"GET /v1/taints/{name}":                            classAdminOnly,
	"GET /v1/teams/{id}/taint-summary":                 classAdminOnly,
	"POST /v1/releases":                                classAdminOnly, // release writers only (releaseWriterDenial)
	"POST /v1/releases/repair-deployments":             classAdminOnly, // release writers only (releaseWriterDenial)
	"POST /v1/releases/{id}/contents":                  classAdminOnly, // release writers only (releaseWriterDenial)
	"GET /v1/taints/{name}/grants":                     classAdminOnly,
	"GET /v1/taints/{name}/audit":                      classAdminOnly,
	"POST /v1/beads/{id}/transfer":                     classAdminOnly,
	"POST /v1/escalations/reroute":                     classAdminOnly,
	"POST /v1/beads/{id}/comments/trim":                classAdminOnly,
	"DELETE /v1/beads/{id}/comments/{comment_id}":      classAdminOnly,
	"POST /v1/beads/{id}/followers":                    classAdminOnly,
	"DELETE /v1/beads/{id}/followers/{operator_id}":    classAdminOnly,
	"PATCH /v1/taints/{name}":                          classAdminOnly,
	"POST /v1/taints/{name}/grants":                    classAdminOnly,
	"DELETE /v1/taints/{name}/grants/{principal}":      classAdminOnly,
	"POST /v1/beads/{id}/taints":                       classAdminOnly,
	"POST /v1/taints/{name}/grant-requests":            classAdminOnly,
	"POST /v1/formulas/{id}/pour":                      classAdminOnly,
	"POST /v1/hooks/execute":                           classAdminOnly,
	"POST /v1/hooks/emit":                              classAdminOnly,
	"POST /v1/hooks/publish":                           classAdminOnly,
	"GET /v1/bus/events":                               classAdminOnly,
	"POST /v1/agents/{id}/gates/{gate_id}/satisfy":     classAdminOnly,
	"DELETE /v1/agents/{id}/gates/{gate_id}":           classAdminOnly,
	"POST /v1/ui/telemetry":                            classAdminOnly,
	"POST /v1/metrics/slow-tool-calls":                 classAdminOnly,
	"POST /v1/taints":                                  classAdminOnly,
	"DELETE /v1/beads/{id}":                            classAdminOnly,
	"POST /v1/beads/{id}/restore":                      classAdminOnly,
	"POST /v1/mutations/{id}/undo":                     classAdminOnly,
	"PUT /v1/configs/{key...}":                         classAdminOnly,
	"DELETE /v1/configs/{key...}":                      classAdminOnly,
	"POST /v1/bridge-tokens":                           classAdminOnly,
	"GET /v1/bridge-tokens":                            classAdminOnly,
	"DELETE /v1/bridge-tokens/{bridge}":                classAdminOnly,
	"POST /v1/lands/purge":                             classAdminOnly,
	"POST /v1/fleet-tokens":                            classAdminOnly,
	"GET /v1/fleet-tokens":                             classAdminOnly,
	"DELETE /v1/fleet-tokens/{fleet}":                  classAdminOnly,
	"POST /v1/admin/revoke-user":                       classAdminOnly,
	"POST /v1/admin/revoke-token":                      classAdminOnly,
	"POST /v1/admin/reextract-references":              classAdminOnly,
	"POST /v1/admin/backfill-search-tsv":               classAdminOnly,
	"POST /v1/admin/backfill-category":                 classAdminOnly,
	"POST /v1/admin/backfill-project-labels":           classAdminOnly,
	"POST /v1/admin/backfill-operators":                classAdminOnly,
	"POST /v1/admin/backfill-operated-by-legacy":       classAdminOnly,
	"POST /v1/admin/backfill-slack-user-id":            classAdminOnly,
	"POST /v1/admin/backfill-operator-slugs":           classAdminOnly,
	"POST /v1/admin/backfill-owner":                    classAdminOnly,
	"POST /v1/admin/backfill-empty-owner":              classAdminOnly,
	"POST /v1/admin/backfill-operated-by-agents":       classAdminOnly,
	"POST /v1/admin/backfill-parent-child-deps":        classAdminOnly,
	"POST /v1/admin/repair-reversed-parent-child-deps": classAdminOnly,
	"POST /v1/admin/backfill-triggered-by-deps":        classAdminOnly,
	"GET /v1/admin/audit-blocks-cycles":                classAdminOnly,
	"POST /v1/admin/close-trigger-audit-beads":         classAdminOnly,
	"POST /v1/admin/operators/{id}/forget":             classAdminOnly,
	"GET /v1/operators":                                classAdminOnly,
	"GET /v1/operators/stats":                          classAdminOnly,
	"GET /v1/operators/by-alias":                       classAdminOnly,
	"GET /v1/operator-slugs/{slug}":                    classAdminOnly,
	"GET /v1/operator-slugs/by-id/{id}":                classAdminOnly,
	"POST /v1/operators/me/aliases":                    classAdminOnly,
	// Slack account linking (gd-OZJ6H2S3xx): writes the caller's OWN user bead
	// only, but like /operators/me/aliases it is an identity-binding route;
	// scoped humans linking Slack is a follow-up of plan gd-507PTO0036.
	"POST /v1/identity-links/slack":          classAdminOnly,
	"DELETE /v1/identity-links/slack":        classAdminOnly,
	"GET /v1/operators/{id}/followed-beads":  classAdminOnly,
	"GET /v1/admin/operators/slack-aliases":  classAdminOnly,
	"POST /v1/admin/operators/{id}/identity": classAdminOnly,
	"POST /v1/admin/operators/link-aliases":  classAdminOnly,
	"POST /v1/admin/operators/prune-aliases": classAdminOnly,
	"POST /v1/embeddings/upsert":             classAdminOnly,
	"POST /v1/embeddings/hashes":             classAdminOnly,
	"POST /v1/embeddings/sync-batch":         classAdminOnly,
	"POST /v1/embeddings/migrate":            classAdminOnly,
	"POST /v1/embeddings/reindex":            classAdminOnly,
}

// tenantAgentRoutes are the classAdminOnly routes a tenant land's per-agent
// token keeps on the bare daemon, because each handler confines it to its own
// agent bead: its Stop gate (gateTargetAllowed) and its hook events
// (tenantAgentHookDenial). A scoped principal reaches none of them.
var tenantAgentRoutes = map[string]bool{
	"GET /v1/agents/{id}/gates":                    true,
	"POST /v1/agents/{id}/gates/{gate_id}/satisfy": true,
	"DELETE /v1/agents/{id}/gates/{gate_id}":       true,
	"POST /v1/hooks/emit":                          true,
}

// scopedRouteGate refuses a classAdminOnly route (and any unclassified one) for
// a tenant with 403: a scoped principal, or a per-agent token bound to a tenant
// land (callerIsTenant), which keeps only tenantAgentRoutes beyond the
// classFiltered and classScopedSafe routes (gd-c1DOWNYy7W). It refuses every
// route but windowViewPattern and windowLivePattern for a window principal,
// and passes every other caller and class through. tenant resolves the
// per-agent half; nil reads every agent token as a fleet one.
// It reads r.Pattern, so it must be installed per route by the registrar (see
// scopeGatedMux), inside the mux match.
func scopedRouteGate(tenant func(context.Context) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authMethodFromContext(r.Context()) == authMethodWindow && r.Pattern != windowViewPattern && r.Pattern != windowLivePattern {
			writeError(w, http.StatusForbidden, "a window opens only "+windowViewPattern)
			return
		}
		if class, ok := scopedRouteClasses[r.Pattern]; !ok || class == classAdminOnly {
			if isScopedPrincipal(r.Context()) {
				writeError(w, http.StatusForbidden, "not available to project-scoped principals")
				return
			}
			if !tenantAgentRoutes[r.Pattern] && tenant != nil && tenant(r.Context()) {
				writeError(w, http.StatusForbidden, "not available to a tenant land's agents")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// rpcScopeTwins maps each BeadsService RPC to the HTTP route it mirrors, so
// gRPC reads its scope class from scopedRouteClasses and cannot drift from
// HTTP. TestRPCScopeTwins_CoverEveryRPC fails the build when an RPC is missing
// here; scopedRPCGate treats a missing one as classAdminOnly.
var rpcScopeTwins = map[string]string{
	"/beads.v1.BeadsService/Health":                     "GET /v1/health",
	"/beads.v1.BeadsService/Whoami":                     "GET /v1/whoami",
	"/beads.v1.BeadsService/GetBead":                    "GET /v1/beads/{id}",
	"/beads.v1.BeadsService/ListBeads":                  "GET /v1/beads",
	"/beads.v1.BeadsService/GetDependencies":            "GET /v1/beads/{id}/dependencies",
	"/beads.v1.BeadsService/GetLabels":                  "GET /v1/beads/{id}/labels",
	"/beads.v1.BeadsService/GetComments":                "GET /v1/beads/{id}/comments",
	"/beads.v1.BeadsService/GetEvents":                  "GET /v1/beads/{id}/events",
	"/beads.v1.BeadsService/ListMentionedBy":            "GET /v1/beads/{id}/mentioned-by",
	"/beads.v1.BeadsService/ListMentions":               "GET /v1/beads/{id}/mentions",
	"/beads.v1.BeadsService/SemanticSearch":             "POST /v1/search/semantic",
	"/beads.v1.BeadsService/GetChildrenStatusHistogram": "GET /v1/batch/children-by-status",
	"/beads.v1.BeadsService/MyWorkSummary":              "GET /v1/beads",
	"/beads.v1.BeadsService/CreateBead":                 "POST /v1/beads",
	"/beads.v1.BeadsService/UpdateBead":                 "PATCH /v1/beads/{id}",
	"/beads.v1.BeadsService/CloseBead":                  "POST /v1/beads/{id}/close",
	"/beads.v1.BeadsService/AddDependency":              "POST /v1/beads/{id}/dependencies",
	"/beads.v1.BeadsService/RemoveDependency":           "DELETE /v1/beads/{id}/dependencies",
	"/beads.v1.BeadsService/AddLabel":                   "POST /v1/beads/{id}/labels",
	"/beads.v1.BeadsService/RemoveLabel":                "DELETE /v1/beads/{id}/labels/{label}",
	"/beads.v1.BeadsService/AddComment":                 "POST /v1/beads/{id}/comments",
	"/beads.v1.BeadsService/DeleteComment":              "DELETE /v1/beads/{id}/comments/{comment_id}",
	"/beads.v1.BeadsService/TrimComments":               "POST /v1/beads/{id}/comments/trim",
	"/beads.v1.BeadsService/DeleteBead":                 "DELETE /v1/beads/{id}",
	"/beads.v1.BeadsService/GetConfig":                  "GET /v1/configs/{key...}",
	"/beads.v1.BeadsService/ListConfigs":                "GET /v1/configs",
	"/beads.v1.BeadsService/SetConfig":                  "PUT /v1/configs/{key...}",
	"/beads.v1.BeadsService/DeleteConfig":               "DELETE /v1/configs/{key...}",
}

// rpcScopeDenied reports whether a tenant (callerIsTenant) is refused method:
// the gRPC twin of scopedRouteGate, so a scoped principal who also holds a
// realm rank, or a tenant land's per-agent token, which RBACInterceptor passes
// below admin, reaches no RPC whose HTTP twin it could not reach.
func (s *BeadsServer) rpcScopeDenied(ctx context.Context, method string) bool {
	class, ok := scopedRouteClasses[rpcScopeTwins[method]]
	if ok && class != classAdminOnly {
		return false
	}
	return s.callerIsTenant(ctx)
}

// tenantRPCRefusal is scopedRPCGate's PermissionDenied message.
const tenantRPCRefusal = "not available to a tenant"

// scopedRPCGate is the unary interceptor for rpcScopeDenied. It runs after
// authentication, beside RBACInterceptor.
func (s *BeadsServer) scopedRPCGate(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if s.rpcScopeDenied(ctx, info.FullMethod) {
		return nil, status.Error(codes.PermissionDenied, tenantRPCRefusal)
	}
	return handler(ctx, req)
}

// scopedRPCStreamGate is scopedRPCGate for streams.
func (s *BeadsServer) scopedRPCStreamGate(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if s.rpcScopeDenied(ss.Context(), info.FullMethod) {
		return status.Error(codes.PermissionDenied, tenantRPCRefusal)
	}
	return handler(srv, ss)
}

// scopedRouteAdmits reports whether requireRole admits a scoped principal on
// r's route without the realm role rank role asks for. A Lands tenant holds
// gardenio-project-member and nothing else (the /lands-tenants and
// /project-<land> Keycloak groups grant only that role), and project-member has
// no rank, so without this every land-door read and write would 403 before the
// handler's scope cut ran (gd-35s9RXARRB). The allow-list is the routes
// scopedRouteGate already lets a scoped principal reach (classFiltered and
// classScopedSafe): their handlers confine it to its token projects, and its
// writes to its actionTable tier, which is the authority for a scoped
// principal, not the global rank. RoleAdmin is never waived, and no route a
// scoped principal may reach is registered at it (TestScopedRouteAdmits_Matrix).
// A token minted for a land is waived only while it holds project-member, as
// landgate requires (land_audience.go landTokenIsMember).
func scopedRouteAdmits(r *http.Request, role string) bool {
	if roleRank[role] >= roleRank[RoleAdmin] || !isScopedPrincipal(r.Context()) {
		return false
	}
	if hasLandAudience(r.Context()) && !landTokenIsMember(r.Context()) {
		return false
	}
	class := scopedRouteClasses[r.Pattern]
	return class == classFiltered || class == classScopedSafe
}

// routeRegistrar is the slice of *http.ServeMux that registerRoutes uses.
// NewHTTPHandler hands it a scopeGatedMux; the route-class guard test hands it
// a recorder, so both see the same registrations.
type routeRegistrar interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// scopeGatedMux registers each route behind landDoorRouteGate,
// landDoorAgentWriteGate (which reads beads from st), scopedRouteGate (which
// asks tenant whether an agent token is a tenant land's) and runnerRouteGate.
type scopeGatedMux struct {
	mux    *http.ServeMux
	st     store.Store
	tenant func(context.Context) bool
}

func (g scopeGatedMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	g.mux.Handle(pattern, landDoorRouteGate(landDoorAgentWriteGate(g.st, scopedRouteGate(g.tenant, runnerRouteGate(http.HandlerFunc(handler))))))
}

// ServeHTTP serves the mux. The Lands map (lands_api.go) calls the daemon's
// routes in-process through it, so each call passes the same route gate.
func (g scopeGatedMux) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.mux.ServeHTTP(w, r) }
