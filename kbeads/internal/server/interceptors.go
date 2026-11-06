package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Keycloak realm role constants for gardenio RBAC.
const (
	RoleViewer   = "gardenio-viewer"
	RoleOperator = "gardenio-operator"
	RoleAdmin    = "gardenio-admin"

	// RoleProjectMember marks a human whose read scope is the projects in their
	// verified token groups (see scopeFromContext). It is deliberately absent
	// from roleRank: a project member is NOT a global viewer or operator.
	RoleProjectMember = "gardenio-project-member"

	// RoleRunner is the role a runner bearer carries (local-agents spec
	// §4.2). It is not in roleRank on purpose: a runner is admitted route by
	// route (runnerRouteAllowed), never by outranking a required role.
	RoleRunner = "gardenio-runner"
)

// roleRank maps each gardenio role to its hierarchy rank.
var roleRank = map[string]int{
	RoleViewer:   1,
	RoleOperator: 2,
	RoleAdmin:    3,
}

const (
	rolesContextKey      contextKey = "oidc.roles"
	groupsContextKey     contextKey = "oidc.groups"
	authMethodContextKey contextKey = "auth.method"
)

// authMethod tracks how a request was authenticated.
type authMethod string

const (
	authMethodStatic authMethod = "static"
	authMethodOIDC   authMethod = "oidc"

	// authMethodNone marks a request served while the daemon has NO auth
	// configured at all (no static token, no fleet tokens, no OIDC
	// verifier) — a dev/test deployment mode, not a real caller presenting
	// a real credential. gd-ZjiSd9Kzio's static-token tightening targets a
	// genuine shared static token being trusted as admin everywhere; it was
	// never meant to make an auth-disabled daemon (where every route was
	// always open) start 403ing its own admin routes just because the
	// zero-value authMethod("") no longer matches authMethodOIDC. See
	// requireRole/requireRoleMachineAllowed/RBACInterceptor/
	// RBACStreamInterceptor, which all treat authMethodNone as a full
	// bypass, pinned by TestRequireRole_AuthDisabledBypassesAdmin /
	// TestRBACInterceptor_AuthDisabledBypassesAdmin.
	authMethodNone authMethod = "none"
)

// WithRoles returns a new context with OIDC roles set.
func WithRoles(ctx context.Context, roles []string) context.Context {
	return context.WithValue(ctx, rolesContextKey, roles)
}

// RolesFromContext returns the OIDC roles from the request context.
func RolesFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(rolesContextKey).([]string)
	return v
}

// WithGroups returns a new context with OIDC groups set.
func WithGroups(ctx context.Context, groups []string) context.Context {
	return context.WithValue(ctx, groupsContextKey, groups)
}

// GroupsFromContext returns the OIDC groups from the request context.
func GroupsFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(groupsContextKey).([]string)
	return v
}

// withAuthMethod returns a new context with the auth method set.
func withAuthMethod(ctx context.Context, m authMethod) context.Context {
	return context.WithValue(ctx, authMethodContextKey, m)
}

// authMethodFromContext returns the auth method from the context.
func authMethodFromContext(ctx context.Context) authMethod {
	v, _ := ctx.Value(authMethodContextKey).(authMethod)
	return v
}

// hasMinimumRole checks whether any of the user's roles meet or exceed the
// required role in the gardenio hierarchy (admin > operator > viewer).
func hasMinimumRole(roles []string, required string) bool {
	requiredRank := roleRank[required]
	if requiredRank == 0 {
		return false
	}
	for _, r := range roles {
		if roleRank[r] >= requiredRank {
			return true
		}
	}
	return false
}

// requireRole wraps an http.HandlerFunc with RBAC enforcement.
// EdgeAuthMiddleware stamps a role-bearing v2 edge identity as authMethodOIDC
// so it is role-enforced here. A verified v1 edge envelope (authMethodEdge)
// carries no roles, so it is not roleBearing and is handled like the static
// token below: admitted below RoleAdmin, refused at RoleAdmin
// (gd-AvSKOUyqRz retired decision ka-0000001641, which let it bypass every
// role check).
//
// A land principal (authMethodLand) is role-enforced like OIDC: roleBearing.
// A scoped principal (a Lands tenant) is admitted below RoleAdmin on the
// routes scopedRouteGate lets it reach, whatever its rank: scopedRouteAdmits.
//
// Static/fleet token auth (service-to-service — in practice the shared
// primary token every agent pod holds) keeps its historical bypass below
// RoleAdmin: routine controller/agent traffic (bead create/update/close,
// dependencies, comments — all RoleOperator or lower) depends on it and has
// no per-caller identity to check anyway. At RoleAdmin it is now enforced
// like everyone else (gd-ZjiSd9Kzio: the shared token was admin everywhere).
// A route with a genuine non-OIDC machine caller does NOT get an exception
// here — see requireRoleMachineAllowed below for the enumerated allow-list.
// Every other admin route (DeleteBead, DeleteConfig, revoke-user,
// revoke-token, the backfill/repair/audit family, embeddings) has no
// confirmed machine caller (inventoried under gd-ZjiSd9Kzio: controller,
// bridge, coopmux, and CLI call sites) — CreateTaint additionally has its
// own OIDC+admin self-gate (callerMayCreateTaint) that makes this redundant
// but harmless for it. A caller that does need one of these routes escalates
// through the normal OIDC/edge-v2 path instead. SetConfig is the one
// exception found on re-inventory (gd-ZjiSd9Kzio review round 4) — see
// requireRoleMachineAllowed.
func requireRole(role string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		method := authMethodFromContext(r.Context())
		if method == authMethodNone {
			handler(w, r)
			return
		}
		// A runner bearer is not roleBearing, but it is no service token
		// either: it reaches only the routes on its allowlist, at any rank.
		if method == authMethodRunner {
			if runnerRouteAllowed(r.Pattern) {
				handler(w, r)
				return
			}
			writeRunnerRouteDenied(w, r)
			return
		}
		if !roleBearing(method) {
			if roleRank[role] < roleRank[RoleAdmin] {
				handler(w, r)
				return
			}
			slog.Debug("RBAC denied (static/fleet token, admin route)", "path", r.URL.Path, "method", r.Method, "required", role)
			writeError(w, http.StatusForbidden, "insufficient permissions: requires "+role)
			return
		}
		roles := RolesFromContext(r.Context())
		if !hasMinimumRole(roles, role) && !scopedRouteAdmits(r, role) {
			slog.Debug("RBAC denied", "path", r.URL.Path, "method", r.Method, "required", role, "roles", roles)
			writeError(w, http.StatusForbidden, "insufficient permissions: requires "+role)
			return
		}
		handler(w, r)
	}
}

// requireRoleMachineAllowed wraps an http.HandlerFunc the same way
// requireRole does, except it keeps the pre-gd-ZjiSd9Kzio bypass for
// static/fleet callers at every role tier, including RoleAdmin (a verified v1
// edge envelope is NOT a machine caller and is refused at RoleAdmin here too,
// gd-AvSKOUyqRz). Use it ONLY
// for the enumerated routes below, confirmed (gd-ZjiSd9Kzio inventory) to
// have a genuine non-OIDC machine caller that authenticates with the shared
// primary static token — the same token every agent pod holds, so this is
// NOT a narrower identity check than requireRole's below-Admin bypass, only
// an explicit acknowledgment that these specific routes cannot yet require
// OIDC without breaking real traffic (Step 2 of gd-ZjiSd9Kzio — per-service
// credentials — is what will let this allow-list shrink to nothing):
//
// Machine principals (gd-whES7ZdkAy, checked at main 899839b4): a machine
// principal is neither OIDC nor edge, so it passes this wrapper like the static
// token does. What differs is the handler gate behind it. The fleet-token and
// bridge-token routes below admit a machine client holding
// gardenio-machine-token-mint (machineHasCapability(capTokenMint) alongside
// isAdminRequest); the controller is the one client with that role, and its
// minting is dormant on k3s (PER_AGENT_TOKENS and BRIDGE_TOKEN_ROTATION_ENABLED
// unset). No other route below has a machine-principal caller yet: slack-bridge
// and beads-viewer hit the operator-alias routes, and kd embed sync and kd
// config create hit the embeddings and SetConfig routes, all on the static or
// fleet token. This wrapper's behaviour for static callers is unchanged.
//
//   - POST/GET/DELETE /v1/bridge-tokens, /v1/fleet-tokens — the controller's
//     bridgetokenrotator (internal/bridgetokenrotator) and per-agent token
//     issuance (cmd/controller/agenttoken.go) mint/list/revoke bridge and
//     fleet tokens using BEADS_DAEMON_TOKEN. These handlers additionally
//     self-gate on isAdminRequest (http_fleet_tokens.go): a constant-time
//     comparison against the literal primary static token, which an OIDC
//     bearer (a JWT) can never satisfy — so routing them through requireRole
//     as OIDC-only would make them unreachable by ANYONE, not just tighten
//     them. isAdminRequest remains the real gate on these three; this
//     wrapper only stops requireRole from denying before isAdminRequest runs.
//   - GET /v1/admin/operators/slack-aliases, POST .../{id}/identity,
//     POST .../link-aliases — the Slack bridge's background resync jobs
//     (internal/bridge/slack_operator_resync.go,
//     slack_oidc_operator_alias_resync.go, cross_operator_notify.go,
//     decisions.go) keep operator display name/email/alias data in sync
//     with Slack on a timer — no human is in the loop to hold an OIDC
//     session for these ticks.
//   - POST /v1/embeddings/sync-batch — the chart's embedding-backfill
//     CronJob (gardenio/helm/gardenio/templates/beads/embedding-backfill-cronjob.yaml)
//     runs `kd embed sync` with the primary static token (BEADS_AUTH_TOKEN
//     from beads.tokenSecret — there is no per-CronJob OIDC identity to mint
//     for a scheduled batch job). Not deployed on every cluster today
//     (gated on BOTH beads.embedding.enabled and beads.embedding.backfill.enabled),
//     but the chart must not ship a route its own CronJob cannot call
//     (gd-ZjiSd9Kzio review).
//   - PUT /v1/configs/{key} (SetConfig only — DeleteConfig stays
//     requireRole/admin-only, no caller found) — the beads-viewer e2e seed
//     (gardenio/tests/beads-viewer-e2e/seed/seed.mjs `ensureTypeConfig`)
//     registers two non-builtin custom types, type:widget-instance and
//     type:view, before seeding fixtures that use them. The e2e daemon runs
//     with real auth configured (BEADS_AUTH_TOKEN=e2e-primary-static-token,
//     .github/workflows/e2e.yml), so `kd config create` authenticates with
//     the shared primary static token exactly as any agent pod would — this
//     is the one non-OIDC caller of SetConfig the gd-ZjiSd9Kzio round-4
//     re-inventory found (config beads created via EnsureConfigs /
//     gb config load are a DIFFERENT storage path — resolveTypeConfig never
//     reads them, per config_bridge_subset_guard_test.go — so they cannot
//     substitute for this write).
func requireRoleMachineAllowed(role string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		method := authMethodFromContext(r.Context())
		if method == authMethodRunner && !runnerRouteAllowed(r.Pattern) {
			// A runner is not one of the enumerated machine callers.
			writeRunnerRouteDenied(w, r)
			return
		}
		if method == authMethodEdge && roleRank[role] >= roleRank[RoleAdmin] {
			// A v1 edge envelope is not one of the enumerated machine
			// callers above; it is refused at RoleAdmin as in requireRole.
			slog.Debug("RBAC denied (v1 edge envelope, admin route)", "path", r.URL.Path, "method", r.Method, "required", role)
			writeError(w, http.StatusForbidden, "insufficient permissions: requires "+role)
			return
		}
		if !roleBearing(method) {
			handler(w, r)
			return
		}
		roles := RolesFromContext(r.Context())
		if !hasMinimumRole(roles, role) {
			slog.Debug("RBAC denied", "path", r.URL.Path, "method", r.Method, "required", role, "roles", roles)
			writeError(w, http.StatusForbidden, "insufficient permissions: requires "+role)
			return
		}
		handler(w, r)
	}
}

// LoggingInterceptor logs the method name, duration, auth identity, and error
// (if any) for every unary RPC call.
func LoggingInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	start := time.Now()
	recordRequestByAuthType(AuthIdentityFromContext(ctx).Type, grpcMethodIsWrite(info.FullMethod))
	resp, err := handler(ctx, req)
	duration := time.Since(start)

	id := AuthIdentityFromContext(ctx)
	attrs := []slog.Attr{
		slog.String("method", info.FullMethod),
		slog.Duration("duration", duration),
		slog.String("auth_type", id.Type),
	}
	if id.Subject != "" {
		attrs = append(attrs, slog.String("auth_subject", id.Subject))
	}
	fleet := FleetFromContext(ctx)
	if fleet != "default" {
		attrs = append(attrs, slog.String("fleet", fleet))
	}

	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
		slog.LogAttrs(ctx, slog.LevelError, "rpc completed", attrs...)
	} else {
		slog.LogAttrs(ctx, slog.LevelInfo, "rpc completed", attrs...)
	}

	return resp, err
}

// RequestCountStreamInterceptor counts authenticated streaming RPCs in the
// per-auth-type request counter; unary RPCs are counted by LoggingInterceptor.
func RequestCountStreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	recordRequestByAuthType(AuthIdentityFromContext(ss.Context()).Type, grpcMethodIsWrite(info.FullMethod))
	return handler(srv, ss)
}

// RecoveryInterceptor catches panics in downstream handlers, logs the stack
// trace, and returns a codes.Internal error instead of crashing the server.
func RecoveryInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic recovered in gRPC handler",
				"method", info.FullMethod,
				"panic", fmt.Sprintf("%v", r),
				"stack", string(debug.Stack()),
			)
			err = status.Errorf(codes.Internal, "internal server error")
		}
	}()
	return handler(ctx, req)
}

// windowAuthInterceptor returns a gRPC unary interceptor that checks the
// "authorization" metadata header for a valid Bearer token. When no fleet
// tokens or OIDC verifier are configured, auth is disabled and all requests
// pass through. The Health RPC is always exempt. When fleetCfg is non-nil, the
// resolved fleet is injected into the request context. When verifier is
// non-nil, tokens that don't match the fleet map are verified as OIDC JWTs.
// There is no shared static bearer (gd-ke6PC7bRto). It recognises live window
// bearers through windows, to refuse them as HTTP-only rather than as unknown.
func windowAuthInterceptor(windows windowResolver, fleetCfg *FleetConfig, verifier TokenVerifier) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if !fleetCfg.HasTokens() && verifier == nil {
			ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "none"})
			ctx = withAuthMethod(ctx, authMethodNone)
			if fleetCfg != nil {
				ctx = WithFleet(ctx, fleetCfg.ResolveFleet(""))
			}
			return handler(ctx, req)
		}

		// Exempt health check.
		if info.FullMethod == "/beads.v1.BeadsService/Health" {
			return handler(ctx, req)
		}

		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing metadata")
		}

		vals := md.Get("authorization")
		if len(vals) == 0 {
			return nil, status.Error(codes.Unauthenticated, "missing authorization header")
		}

		provided := vals[0]
		if !strings.HasPrefix(provided, "Bearer ") {
			return nil, status.Error(codes.Unauthenticated, "invalid authorization scheme")
		}
		provided = strings.TrimPrefix(provided, "Bearer ")

		// Window principals are HTTP-only (window_principal.go); a gw_ bearer
		// never reaches the static, fleet or OIDC paths.
		if isWindowBearer(provided) {
			return nil, windowAuthGRPC(ctx, windows, provided)
		}

		if provided, ok = resolveFleetToken(provided, fleetCfg); ok {
			// Reject expired fleet tokens before any further processing. Tokens
			// without an ExpiresAt set (zero value) never expire — preserving
			// seed/admin tokens.
			if fleetCfg.IsExpired(provided) {
				slog.Warn("rejected expired fleet token (gRPC)", "hash_prefix", tokenHashPrefix(provided))
				return nil, status.Error(codes.Unauthenticated, "token expired")
			}
			// Land principals are HTTP-only, like machine principals: the
			// scoped-principal route classes are HTTP's.
			if _, ok, err := landPrincipalForToken(fleetCfg, provided); err != nil {
				return nil, status.Error(codes.Unauthenticated, err.Error())
			} else if ok {
				return nil, status.Error(codes.PermissionDenied, "land principals are HTTP-only")
			}
			// Fleet token match — inject fleet, scopes, and identity from token mapping.
			fleet := fleetCfg.ResolveFleet(provided)
			// Per-identity subject (C1.4): prefer the token's Subject when set,
			// fall back to fleet name for back-compat with tokens issued
			// before C1.4.
			subject := fleetCfg.ResolveSubject(provided)
			if subject == "" {
				subject = fleet
			}
			id := AuthIdentity{Type: "fleet", Subject: subject, Privileged: fleetCfg.ResolvePrivileged(provided)}
			if proj := fleetCfg.ResolveProject(provided); proj != "" {
				ctx = WithTokenProject(ctx, proj)
			}
			if ts := fleetCfg.ResolveTypeScope(provided); len(ts) > 0 {
				ctx = WithTokenTypeScope(ctx, ts)
			}
			if os := fleetCfg.ResolveOpScope(provided); len(os) > 0 {
				ctx = WithTokenOpScope(ctx, os)
			}
			// X-Fleet and X-Actor are not honored: a fleet token's fleet and
			// subject come from its credential, never from a header. The shared
			// static token that once honored them is retired (gd-ke6PC7bRto).
			ctx = WithFleet(ctx, fleet)
			// Fleet-token bridge attribution (ka-0000008252): mirrors the HTTP
			// interceptor. The slack/jira/gitlab bridges authenticate with a
			// FLEET token and re-attribute the external-platform human via
			// X-Operated-By; resolveOperatedByPure's fleet case honors it, but
			// the header has to be stamped into ctx here first or the resolver
			// falls through to the fleet:slack-bridge sentinel and the human
			// creator's attribution is lost. sanitizeOperatedByHeader keeps the
			// value human-shaped. Agent-pod-only headers are honoured only for a
			// per-agent fleet token (below).
			if id.Type == "fleet" {
				if vals := md.Get("x-operated-by"); len(vals) > 0 {
					ctx = applyOperatedByHeader(ctx, vals[0], id.Type, "grpc:"+info.FullMethod)
				}
				if vals := md.Get("x-user-bead-id"); len(vals) > 0 {
					if v, ok := sanitizeUserBeadIDHeader(vals[0]); ok {
						ctx = WithUserBeadID(ctx, v)
					}
				}
				if vals := md.Get("x-no-owner-default"); len(vals) > 0 && parseNoOwnerDefaultHeader(vals[0]) {
					ctx = WithNoOwnerDefault(ctx)
				}
				if vals := md.Get("x-owner-override"); len(vals) > 0 {
					if v, ok := sanitizeOwnerOverrideHeader(vals[0]); ok {
						ctx = WithOwnerOverride(ctx, v)
					}
				}
			}
			// Per-agent tokens (P7 Task 1): mirrors the HTTP interceptor. The
			// agent's name comes from its credential; only the agent-pod
			// headers below are honoured for it.
			if id.Type == "fleet" && isAgentFleet(FleetFromContext(ctx)) {
				if vals := md.Get("x-trigger-bead"); len(vals) > 0 {
					if v, ok := sanitizeTriggerBeadHeader(vals[0]); ok {
						ctx = WithTriggerBead(ctx, v)
					}
				}
				if vals := md.Get("x-no-auto-parent"); len(vals) > 0 && parseNoAutoParentHeader(vals[0]) {
					ctx = WithNoAutoParent(ctx)
				}
			}
			// X-No-Auto-Inherit opt-out is honored on every auth method.
			if vals := md.Get("x-no-auto-inherit"); len(vals) > 0 && vals[0] == "1" {
				ctx = WithNoAutoInherit(ctx, true)
			}
			// X-Force-Parent-Edge overrides the closed-parent guard on an
			// explicit dep-add (ka-0000005756). Scoped to the static-token
			// branch, like X-No-Auto-Parent: `kd dep add --force` authenticates
			// with the primary token.
			if vals := md.Get("x-force-parent-edge"); len(vals) > 0 && parseForceParentEdgeHeader(vals[0]) {
				ctx = WithForceParentEdge(ctx)
			}
			recordHeaderArrival(id.Type, headerPresent(md.Get("x-operated-by")))
			ctx = WithAuthIdentity(ctx, id)
			return handler(ctx, req)
		}

		// Not a fleet token — try OIDC JWT verification.
		if verifier != nil {
			// Machine principals are HTTP-only (gRPC is frozen): refuse a
			// registered machine client's token here instead of letting it
			// fall into the OIDC human path.
			if _, isMachine, err := classifyMachineToken(ctx, fleetCfg, verifier, provided); err != nil {
				return nil, status.Error(codes.Unauthenticated, err.Error())
			} else if isMachine {
				return nil, status.Error(codes.PermissionDenied, "machine principals are HTTP-only")
			}
			// Prefer RoleExtractor path so RBAC can enforce per-RPC roles.
			if re, ok := verifier.(RoleExtractor); ok {
				if roles, err := re.ExtractRoles(ctx, provided); err == nil {
					email := verifyAndIdentify(ctx, verifier, provided)
					ctx = withAuthMethod(ctx, authMethodOIDC)
					// A land token holds no global role (land_audience.go).
					lands, err := oidcTokenLands(ctx, verifier, provided)
					if err != nil {
						return nil, status.Error(codes.Unauthenticated, err.Error())
					}
					ctx = withTokenLands(ctx, lands)
					ctx = WithRoles(ctx, landAudienceRoles(roles, lands))
					if ge, ok := verifier.(GroupExtractor); ok {
						if groups, err := ge.ExtractGroups(ctx, provided); err == nil {
							ctx = WithGroups(ctx, groups)
						}
					}
					ctx = WithAuthIdentity(ctx, oidcAuthIdentity(ctx, verifier, provided, email))
					if fleetCfg != nil {
						ctx = WithFleet(ctx, fleetCfg.DefaultFleet)
					}
					recordHeaderArrival("oidc", headerPresent(md.Get("x-operated-by")))
					return handler(ctx, req)
				}
			}
			email := verifyAndIdentify(ctx, verifier, provided)
			if email != "" || verifier.Verify(ctx, provided) == nil {
				lands, err := oidcTokenLands(ctx, verifier, provided)
				if err != nil {
					return nil, status.Error(codes.Unauthenticated, err.Error())
				}
				ctx = withAuthMethod(ctx, authMethodOIDC)
				ctx = withTokenLands(ctx, lands)
				ctx = WithAuthIdentity(ctx, oidcAuthIdentity(ctx, verifier, provided, email))
				if fleetCfg != nil {
					ctx = WithFleet(ctx, fleetCfg.DefaultFleet)
				}
				recordHeaderArrival("oidc", headerPresent(md.Get("x-operated-by")))
				return handler(ctx, req)
			}
		}

		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}
}

// rpcMinRole maps each BeadsService RPC to the minimum OIDC role required.
// RPCs not listed here default to RoleAdmin (fail-closed for new RPCs) — see
// TestRPCMinRole_CoversEveryDescriptorMethod, which walks
// beadsv1.BeadsService_ServiceDesc and fails if a new RPC lands without an
// explicit entry here, so the fail-closed default can never silently catch a
// genuine non-OIDC machine caller again (gd-ZjiSd9Kzio merge-desk review
// round 3: GetChildrenStatusHistogram, ListMentionedBy, ListMentions,
// MyWorkSummary, and Whoami were missing from this map and defaulted to
// RoleAdmin, denying static/fleet gRPC callers even though every one of
// them is RoleViewer on the HTTP side — see http.go's registrations of
// handleListMentionedBy/handleListMentions/handleMyWorkSummary/handleWhoami
// and http_batch.go's handleBatchChildrenByStatus). Static-token and
// unauthenticated paths bypass this check (mirroring HTTP requireRole).
var rpcMinRole = map[string]string{
	"/beads.v1.BeadsService/Health":                     "",
	"/beads.v1.BeadsService/GetBead":                    RoleViewer,
	"/beads.v1.BeadsService/ListBeads":                  RoleViewer,
	"/beads.v1.BeadsService/GetDependencies":            RoleViewer,
	"/beads.v1.BeadsService/GetLabels":                  RoleViewer,
	"/beads.v1.BeadsService/GetComments":                RoleViewer,
	"/beads.v1.BeadsService/GetEvents":                  RoleViewer,
	"/beads.v1.BeadsService/GetConfig":                  RoleViewer,
	"/beads.v1.BeadsService/ListConfigs":                RoleViewer,
	"/beads.v1.BeadsService/SemanticSearch":             RoleViewer,
	"/beads.v1.BeadsService/GetChildrenStatusHistogram": RoleViewer,
	"/beads.v1.BeadsService/ListMentionedBy":            RoleViewer,
	"/beads.v1.BeadsService/ListMentions":               RoleViewer,
	"/beads.v1.BeadsService/MyWorkSummary":              RoleViewer,
	"/beads.v1.BeadsService/Whoami":                     RoleViewer,
	"/beads.v1.BeadsService/CreateBead":                 RoleOperator,
	"/beads.v1.BeadsService/UpdateBead":                 RoleOperator,
	"/beads.v1.BeadsService/CloseBead":                  RoleOperator,
	"/beads.v1.BeadsService/AddDependency":              RoleOperator,
	"/beads.v1.BeadsService/RemoveDependency":           RoleOperator,
	"/beads.v1.BeadsService/AddLabel":                   RoleOperator,
	"/beads.v1.BeadsService/RemoveLabel":                RoleOperator,
	"/beads.v1.BeadsService/AddComment":                 RoleOperator,
	"/beads.v1.BeadsService/DeleteComment":              RoleOperator,
	"/beads.v1.BeadsService/TrimComments":               RoleOperator,
	"/beads.v1.BeadsService/DeleteBead":                 RoleAdmin,
	"/beads.v1.BeadsService/SetConfig":                  RoleAdmin,
	"/beads.v1.BeadsService/DeleteConfig":               RoleAdmin,
}

// RBACInterceptor enforces per-RPC role requirements for OIDC-authenticated
// callers. A verified v1 edge envelope (authMethodEdge) is not roleBearing
// and is handled like the static token (gd-AvSKOUyqRz). A
// static/fleet token bypasses below RoleAdmin — matching HTTP requireRole —
// but is enforced at RoleAdmin (gd-ZjiSd9Kzio: no legitimate machine caller
// needs DeleteBead/SetConfig/DeleteConfig with the shared token). RPCs not in
// rpcMinRole default to RoleAdmin to fail closed for new RPCs.
func RBACInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	method := authMethodFromContext(ctx)
	if method == authMethodNone {
		return handler(ctx, req)
	}
	required, known := rpcMinRole[info.FullMethod]
	if !known {
		required = RoleAdmin
	}
	if required == "" {
		return handler(ctx, req)
	}
	if !roleBearing(method) {
		if roleRank[required] < roleRank[RoleAdmin] {
			return handler(ctx, req)
		}
		slog.Debug("RBAC denied (static/fleet token, admin RPC)", "method", info.FullMethod, "required", required)
		return nil, status.Errorf(codes.PermissionDenied, "insufficient permissions: requires %s", required)
	}
	roles := RolesFromContext(ctx)
	if !hasMinimumRole(roles, required) {
		slog.Debug("RBAC denied", "method", info.FullMethod, "required", required, "roles", roles)
		return nil, status.Errorf(codes.PermissionDenied, "insufficient permissions: requires %s", required)
	}
	return handler(ctx, req)
}

// RecoveryStreamInterceptor catches panics in streaming handlers.
func RecoveryStreamInterceptor(
	srv any,
	ss grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic recovered in gRPC stream handler",
				"method", info.FullMethod,
				"panic", fmt.Sprintf("%v", r),
				"stack", string(debug.Stack()),
			)
			err = status.Errorf(codes.Internal, "internal server error")
		}
	}()
	return handler(srv, ss)
}

// AuthStreamInterceptor is the streaming equivalent of AuthInterceptor. It
// wraps the incoming stream so that handlers see the auth-populated context.
func AuthStreamInterceptor(fleetCfg *FleetConfig, verifier TokenVerifier) grpc.StreamServerInterceptor {
	return windowAuthStreamInterceptor(nil, fleetCfg, verifier)
}

// windowAuthStreamInterceptor is AuthStreamInterceptor over windowAuthInterceptor.
func windowAuthStreamInterceptor(windows windowResolver, fleetCfg *FleetConfig, verifier TokenVerifier) grpc.StreamServerInterceptor {
	unary := windowAuthInterceptor(windows, fleetCfg, verifier)
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		_, err := unary(ss.Context(), nil, &grpc.UnaryServerInfo{FullMethod: info.FullMethod}, func(ctx context.Context, _ any) (any, error) {
			return nil, handler(srv, &wrappedServerStream{ServerStream: ss, ctx: ctx})
		})
		return err
	}
}

// RBACStreamInterceptor is the streaming equivalent of RBACInterceptor.
func RBACStreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := ss.Context()
	method := authMethodFromContext(ctx)
	if method == authMethodNone {
		return handler(srv, ss)
	}
	required, known := rpcMinRole[info.FullMethod]
	if !known {
		required = RoleAdmin
	}
	if required == "" {
		return handler(srv, ss)
	}
	if !roleBearing(method) {
		if roleRank[required] < roleRank[RoleAdmin] {
			return handler(srv, ss)
		}
		slog.Debug("RBAC denied (stream, static/fleet token, admin RPC)", "method", info.FullMethod, "required", required)
		return status.Errorf(codes.PermissionDenied, "insufficient permissions: requires %s", required)
	}
	roles := RolesFromContext(ctx)
	if !hasMinimumRole(roles, required) {
		slog.Debug("RBAC denied (stream)", "method", info.FullMethod, "required", required, "roles", roles)
		return status.Errorf(codes.PermissionDenied, "insufficient permissions: requires %s", required)
	}
	return handler(srv, ss)
}

// RevocationStreamInterceptor is the streaming equivalent of RevocationInterceptor.
func RevocationStreamInterceptor(rs *TokenRevocationStore) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if info.FullMethod == "/beads.v1.BeadsService/Health" {
			return handler(srv, ss)
		}
		md, ok := metadata.FromIncomingContext(ss.Context())
		if !ok {
			return handler(srv, ss)
		}
		vals := md.Get("authorization")
		if len(vals) > 0 && strings.HasPrefix(vals[0], "Bearer ") {
			token := strings.TrimPrefix(vals[0], "Bearer ")
			if rs.IsRevokedToken(token) {
				slog.Warn("rejected revoked token (gRPC stream)", "hash_prefix", tokenHashPrefix(token))
				return status.Error(codes.Unauthenticated, "token has been revoked")
			}
		}
		return handler(srv, ss)
	}
}

// wrappedServerStream lets AuthStreamInterceptor replace the stream context.
type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedServerStream) Context() context.Context { return w.ctx }

// AuthMiddleware is windowAuthMiddleware with no window resolver and no edge
// forward-auth clients: the auth layer alone, for a caller outside this
// package that wraps its own handler (cmd/kd's bridge-token resync test).
func AuthMiddleware(fleetCfg *FleetConfig, verifier TokenVerifier, next http.Handler) http.Handler {
	return windowAuthMiddleware(nil, nil, fleetCfg, verifier, next)
}

// forwardAuthClientTrusted reports whether the X-Auth-Request-* identity
// headers (Email, Groups) may be honoured: only when the token's verified azp
// is an edge forward-auth client (the oauth2-proxy / browser SSO client). The
// headers are client-controllable, so any other realm token (a service
// account, the device-grant client, a gardenio-cli viewer) must not be able to
// claim a human identity or widen its own groups with them. Fails closed: empty
// azp or empty set => false.
func forwardAuthClientTrusted(edgeClients map[string]struct{}, clientID string) bool {
	if clientID == "" {
		return false
	}
	_, ok := edgeClients[clientID]
	return ok
}

// windowAuthMiddleware wraps an http.Handler and checks the Authorization
// header for a valid Bearer token. When no fleet tokens or OIDC verifier are
// configured, auth is disabled and all requests pass through. GET /v1/health
// is always exempt. When fleetCfg is non-nil, the resolved fleet is injected
// into the request context (X-Fleet is not honoured). When verifier is
// non-nil, tokens that don't match the fleet map are verified as OIDC JWTs.
// There is no shared static bearer (gd-ke6PC7bRto). The
// X-Auth-Request-Access-Token header (set by oauth2-proxy ForwardAuth) is also
// accepted as a token source. edgeClients is the edge forward-auth client
// allowlist (BeadsServer.forwardAuthClients) that gates the
// X-Auth-Request-Email and X-Auth-Request-Groups headers. It resolves gw_
// bearers through windows (window_principal.go); with nil windows a gw_ bearer
// is 401.
func windowAuthMiddleware(windows windowResolver, edgeClients map[string]struct{}, fleetCfg *FleetConfig, verifier TokenVerifier, next http.Handler) http.Handler {
	if !fleetCfg.HasTokens() && verifier == nil {
		if fleetCfg != nil {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := r.Context()
				if _, ok := ctx.Value(authIdentityKey).(AuthIdentity); !ok {
					ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "none"})
				}
				ctx = withAuthMethod(ctx, authMethodNone)
				if _, ok := ctx.Value(fleetContextKey).(string); !ok {
					ctx = WithFleet(ctx, fleetCfg.ResolveFleet(""))
				}
				if raw := r.Header.Get("X-Operated-By"); raw != "" {
					ctx = applyOperatedByHeader(ctx, raw, "none", "http:"+r.Method+" "+r.URL.Path)
				}
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if _, ok := ctx.Value(authIdentityKey).(AuthIdentity); !ok {
				ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "none"})
			}
			ctx = withAuthMethod(ctx, authMethodNone)
			if raw := r.Header.Get("X-Operated-By"); raw != "" {
				ctx = applyOperatedByHeader(ctx, raw, "none", "http:"+r.Method+" "+r.URL.Path)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exempt health checks (/v1/health + cheap /v1/health/live, ka-0000001764).
		if isProbeHealthPath(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// Exempt refresh-token exchange — the refresh-token plaintext in
		// the JSON body is itself the credential, so the request must not
		// require a Bearer access-token header.
		if r.Method == http.MethodPost && r.URL.Path == "/v1/auth/refresh" {
			ctx := WithAuthIdentity(r.Context(), AuthIdentity{Type: "refresh"})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// Exempt runner registration (local-agents spec §4.2): the single-use
		// invite code in the body is the credential, and a laptop being
		// registered holds no bearer yet. handleRegisterRunner rate-limits per
		// IP and answers every bad code with the same 404.
		if r.Method == http.MethodPost && r.URL.Path == "/v1/runners/register" {
			ctx := WithAuthIdentity(r.Context(), AuthIdentity{Type: runnerRegisterIdentityType})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Extract token from Authorization header, falling back to the
		// X-Auth-Request-Access-Token header set by oauth2-proxy ForwardAuth.
		auth := r.Header.Get("Authorization")
		if auth == "" {
			if fwd := r.Header.Get("X-Auth-Request-Access-Token"); fwd != "" {
				auth = "Bearer " + fwd
			}
		}

		// Also check X-Auth-Request-Email and X-Auth-Request-Groups
		// (set by oauth2-proxy ForwardAuth). Both headers are honoured only
		// for edge-client tokens (forwardAuthClientTrusted), and only as a
		// fallback when the token carries no email / groups claim of its own.
		forwardAuthEmail := r.Header.Get("X-Auth-Request-Email")
		forwardAuthGroups := r.Header.Get("X-Auth-Request-Groups")

		if auth == "" {
			writeError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}

		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "invalid authorization scheme")
			return
		}

		provided := strings.TrimPrefix(auth, "Bearer ")

		// A gw_ bearer is a window principal (window_principal.go) or nothing:
		// it is classified here, before the land-prefix and fleet fallthrough,
		// and never compared with FleetConfig or verified as a JWT.
		if isWindowBearer(provided) {
			windowAuthHTTP(w, r, windows, fleetCfg, next, provided)
			return
		}

		if provided, ok := resolveFleetToken(provided, fleetCfg); ok {
			// Reject expired fleet tokens before any further processing. Tokens
			// without an ExpiresAt set (zero value) never expire — preserving
			// seed/admin tokens.
			if fleetCfg.IsExpired(provided) {
				slog.Warn("rejected expired fleet token", "hash_prefix", tokenHashPrefix(provided))
				// Hint clients toward POST /v1/auth/refresh so long-lived
				// PAT pods can recover from access-token expiry without
				// the operator re-issuing.
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="token expired", refresh_endpoint="/v1/auth/refresh"`)
				writeError(w, http.StatusUnauthorized, "token expired")
				return
			}
			// A land-* token is a land principal (land_principal.go), never a
			// fleet token: it gets the scoped-principal context and no header.
			if land, ok, err := landPrincipalForToken(fleetCfg, provided); err != nil {
				writeError(w, http.StatusUnauthorized, err.Error())
				return
			} else if ok {
				next.ServeHTTP(w, r.WithContext(landPrincipalContext(r.Context(), fleetCfg, land)))
				return
			}
			// Fleet token match — inject fleet, auth method, and identity.
			ctx := withAuthMethod(r.Context(), authMethodStatic)
			fleet := fleetCfg.ResolveFleet(provided)
			// Per-identity subject (C1.4): prefer the token's Subject when set,
			// fall back to fleet name for back-compat with tokens issued
			// before C1.4.
			subject := fleetCfg.ResolveSubject(provided)
			if subject == "" {
				subject = fleet
			}
			id := AuthIdentity{Type: "fleet", Subject: subject, Privileged: fleetCfg.ResolvePrivileged(provided)}
			// Set project scope if the fleet token is project-bound.
			if proj := fleetCfg.ResolveProject(provided); proj != "" {
				ctx = WithTokenProject(ctx, proj)
			}
			// Set type/op scopes if the fleet token has restrictions.
			if ts := fleetCfg.ResolveTypeScope(provided); len(ts) > 0 {
				ctx = WithTokenTypeScope(ctx, ts)
			}
			if os := fleetCfg.ResolveOpScope(provided); len(os) > 0 {
				ctx = WithTokenOpScope(ctx, os)
			}
			// X-Fleet and X-Actor are not honored: a fleet token's fleet and
			// subject come from its credential, never from a header. The shared
			// static token that once honored them is retired (gd-ke6PC7bRto).
			ctx = WithFleet(ctx, fleet)
			// Fleet-token bridge attribution (ka-0000008252): the slack/jira/
			// gitlab bridges authenticate with a FLEET token and re-attribute
			// the external-platform human to a kbeads operator via X-Operated-By.
			// resolveOperatedByPure's fleet case is written to honor that header
			// (TestResolveAuditOperator_Fleet_HonorsHeader, ka-0000001777), but
			// without stamping it into ctx here the header never reaches the
			// resolver and every bridge-spawned bead's operated_by column falls
			// through to the fleet:slack-bridge sentinel — losing the human
			// creator's attribution. sanitizeOperatedByHeader restricts
			// X-Operated-By to human shapes (slack:U… / oidc:…@…), so a fleet
			// token with write access self-attributing the human stays inside
			// a bounded trust boundary. Agent-pod-only headers (X-Trigger-Bead,
			// X-No-Auto-Parent) are honoured only for a per-agent fleet token
			// (below); X-Actor and X-System-Operator are honoured for no one
			// since the shared static token was retired (gd-ke6PC7bRto).
			if id.Type == "fleet" {
				if raw := r.Header.Get("X-Operated-By"); raw != "" {
					ctx = applyOperatedByHeader(ctx, raw, id.Type, "http:"+r.Method+" "+r.URL.Path)
				}
				if v, ok := sanitizeUserBeadIDHeader(r.Header.Get("X-User-Bead-Id")); ok {
					ctx = WithUserBeadID(ctx, v)
				}
				if parseNoOwnerDefaultHeader(r.Header.Get("X-No-Owner-Default")) {
					ctx = WithNoOwnerDefault(ctx)
				}
				if raw := r.Header.Get("X-Owner-Override"); raw != "" {
					if v, ok := sanitizeOwnerOverrideHeader(raw); ok {
						ctx = WithOwnerOverride(ctx, v)
					}
				}
			}
			// Per-agent tokens (P7 Task 1): an agent pod that authenticates with
			// its own fleet token keeps the agent-pod headers. X-Actor, X-Fleet,
			// X-System-Operator and X-Spawn-Privileged-Override are honoured for
			// no one: the agent's name comes from its credential, not a header.
			if id.Type == "fleet" && isAgentFleet(FleetFromContext(ctx)) {
				if raw := r.Header.Get("X-Trigger-Bead"); raw != "" {
					if v, ok := sanitizeTriggerBeadHeader(raw); ok {
						ctx = WithTriggerBead(ctx, v)
					}
				}
				if parseNoAutoParentHeader(r.Header.Get("X-No-Auto-Parent")) {
					ctx = WithNoAutoParent(ctx)
				}
			}
			// X-No-Auto-Inherit opt-out is honored on every auth method —
			// the trust model is "the caller asked for no inheritance,
			// stop inheriting" regardless of who they authenticated as.
			if r.Header.Get("X-No-Auto-Inherit") == "1" {
				ctx = WithNoAutoInherit(ctx, true)
			}
			// X-Force-Parent-Edge overrides the closed-parent guard on an
			// explicit dep-add (ka-0000005756). Scoped to the static-token
			// branch, like X-No-Auto-Parent: `kd dep add --force` runs in agent
			// pods / operator CLIs that authenticate with the primary token.
			if parseForceParentEdgeHeader(r.Header.Get("X-Force-Parent-Edge")) {
				ctx = WithForceParentEdge(ctx)
			}
			recordHeaderArrival(id.Type, headerPresent([]string{r.Header.Get("X-Operated-By")}))
			ctx = WithAuthIdentity(ctx, id)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Not a fleet token — try OIDC JWT verification.
		if verifier != nil {
			// Machine principal (service-account JWT from a registered client).
			// Inert unless BEADS_MACHINE_CLIENTS is set; no request header is
			// honored for a machine (machine_identity.go).
			if mctx, handled := machineAuthHTTP(w, r, fleetCfg, verifier, provided); handled {
				if mctx != nil {
					next.ServeHTTP(w, r.WithContext(mctx))
				}
				return
			}
			// Use RoleExtractor if available to inject roles for RBAC.
			if re, ok := verifier.(RoleExtractor); ok {
				if roles, err := re.ExtractRoles(r.Context(), provided); err == nil {
					email := verifyAndIdentify(r.Context(), verifier, provided)
					ctx := withAuthMethod(r.Context(), authMethodOIDC)
					// A land token holds no global role (land_audience.go).
					lands, err := oidcTokenLands(ctx, verifier, provided)
					if err != nil {
						writeError(w, http.StatusUnauthorized, err.Error())
						return
					}
					ctx = withTokenLands(ctx, lands)
					ctx = WithRoles(ctx, landAudienceRoles(roles, lands))
					id := oidcAuthIdentity(ctx, verifier, provided, email)
					// Inject groups: the token's own groups claim (GroupExtractor)
					// wins. The ForwardAuth header is only a fallback for an
					// edge-client token that carries no groups claim
					// (gd-Wmom75wBdl, same rule as the email header) — it is
					// client-controllable, so a signed-in caller who bypasses the
					// proxy with their own edge-client token must not widen their
					// groups, which feed allowed_projects and taint read grants.
					var claimGroups []string
					claimOK := false
					if ge, ok := verifier.(GroupExtractor); ok {
						if groups, err := ge.ExtractGroups(ctx, provided); err == nil {
							claimGroups, claimOK = groups, true
						}
					}
					switch {
					case len(claimGroups) > 0:
						ctx = WithGroups(ctx, claimGroups)
					case forwardAuthGroups != "" && forwardAuthClientTrusted(edgeClients, id.ClientID):
						ctx = WithGroups(ctx, parseForwardAuthGroups(forwardAuthGroups))
					case claimOK:
						ctx = WithGroups(ctx, claimGroups)
					}
					if id.Subject == "" && forwardAuthEmail != "" && forwardAuthClientTrusted(edgeClients, id.ClientID) {
						id.Subject = forwardAuthEmail
					}
					ctx = WithAuthIdentity(ctx, id)
					if fleetCfg != nil {
						ctx = WithFleet(ctx, fleetCfg.DefaultFleet)
					}
					recordHeaderArrival("oidc", headerPresent([]string{r.Header.Get("X-Operated-By")}))
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			email := verifyAndIdentify(r.Context(), verifier, provided)
			if email != "" || verifier.Verify(r.Context(), provided) == nil {
				lands, err := oidcTokenLands(r.Context(), verifier, provided)
				if err != nil {
					writeError(w, http.StatusUnauthorized, err.Error())
					return
				}
				ctx := withAuthMethod(r.Context(), authMethodOIDC)
				ctx = withTokenLands(ctx, lands)
				id := oidcAuthIdentity(ctx, verifier, provided, email)
				if id.Subject == "" && forwardAuthEmail != "" && forwardAuthClientTrusted(edgeClients, id.ClientID) {
					id.Subject = forwardAuthEmail
				}
				ctx = WithAuthIdentity(ctx, id)
				if fleetCfg != nil {
					ctx = WithFleet(ctx, fleetCfg.DefaultFleet)
				}
				recordHeaderArrival("oidc", headerPresent([]string{r.Header.Get("X-Operated-By")}))
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		writeError(w, http.StatusUnauthorized, "invalid token")
	})
}

// normalizeBearerToken tolerates a token value delivered as the JSON object
// {"token":"<value>"} instead of the raw token. An ExternalSecret mirror
// materialized without `property` stores the whole SM JSON value, so
// depending on which namespace/mirror a component read, the SAME logical
// token circulates in both wrapped and raw forms — the daemon env
// (BEADS_AUTH_TOKEN) and the client env can disagree. kd/gb normalize
// client-side (their unwrapTokenJSON); this is the server-side mirror so
// every wrapped/raw combination of configured-vs-provided still matches
// (bug ka-0000003595 lineage; staging E2E 401s on release 2026.190.5).
// Anything not exactly that shape is returned unchanged.
func normalizeBearerToken(v string) string {
	trimmed := strings.TrimSpace(v)
	if !strings.HasPrefix(trimmed, "{") {
		return v
	}
	var obj struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil || obj.Token == "" {
		return v
	}
	return obj.Token
}

// resolveFleetToken reports whether the provided bearer is a registered fleet
// token and returns the form that is registered. The provided value is also
// tried in its normalizeBearerToken form, so a JSON-wrapped form of the same
// token matches its raw form. On no match it returns provided unchanged. There
// is no shared primary or rotation token any more (gd-ke6PC7bRto).
func resolveFleetToken(provided string, fleetCfg *FleetConfig) (string, bool) {
	if fleetCfg.HasToken(provided) {
		return provided, true
	}
	if norm := normalizeBearerToken(provided); fleetCfg.HasToken(norm) {
		return norm, true
	}
	return provided, false
}

// TokenRevocationMiddleware rejects any Bearer token whose SHA-256 hash
// appears in the revocation set. This runs before AuthMiddleware so that
// revoked tokens are rejected without checking validity. A nil store
// passes every request straight through (no revocation configured).
func TokenRevocationMiddleware(rs *TokenRevocationStore, next http.Handler) http.Handler {
	if rs == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exempt health checks (/v1/health + cheap /v1/health/live, ka-0000001764).
		if isProbeHealthPath(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			token := strings.TrimPrefix(auth, "Bearer ")
			if rs.IsRevokedToken(token) {
				slog.Warn("rejected revoked token", "hash_prefix", tokenHashPrefix(token))
				writeError(w, http.StatusUnauthorized, "token has been revoked")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// RevocationInterceptor is the gRPC equivalent of TokenRevocationMiddleware.
func RevocationInterceptor(rs *TokenRevocationStore) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if info.FullMethod == "/beads.v1.BeadsService/Health" {
			return handler(ctx, req)
		}
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return handler(ctx, req)
		}
		vals := md.Get("authorization")
		if len(vals) > 0 && strings.HasPrefix(vals[0], "Bearer ") {
			token := strings.TrimPrefix(vals[0], "Bearer ")
			if rs.IsRevokedToken(token) {
				slog.Warn("rejected revoked token (gRPC)", "hash_prefix", tokenHashPrefix(token))
				return nil, status.Error(codes.Unauthenticated, "token has been revoked")
			}
		}
		return handler(ctx, req)
	}
}

// tokenHashPrefix returns the first 12 hex chars of a token's SHA-256 hash
// for safe logging (enough to correlate, not enough to reverse).
func tokenHashPrefix(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:6])
}

// parseForwardAuthGroups parses the X-Auth-Request-Groups header value.
// oauth2-proxy encodes multiple groups as a comma-separated string.
// Each value may be a Keycloak group path like "/project-alpha".
func parseForwardAuthGroups(header string) []string {
	var groups []string
	for _, g := range strings.Split(header, ",") {
		g = strings.TrimSpace(g)
		if g != "" {
			groups = append(groups, g)
		}
	}
	return groups
}
