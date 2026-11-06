package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/oidcauth"
)

// agentTokenVerifier resolves a per-agent daemon token to its agent
// (bridge.AgentTokenVerifier).
type agentTokenVerifier interface {
	Verify(ctx context.Context, token string) (bridge.AgentCaller, error)
}

// bridgeRBAC wraps the bridge HTTP mux with OIDC authentication and
// per-route RBAC enforcement. OIDC auth is only applied to /api/ routes;
// health endpoints, Slack webhooks, and static UI files pass through.
//
// Agent-facing endpoints (/api/slack/*, /api/permission/*) are authenticated
// with a shared bearer token (SLACK_BRIDGE_AGENT_TOKEN). When the token is
// unset, these endpoints fall back to requiring an OIDC admin role so they
// are never open to unauthenticated callers (fixes C1 from AUDIT-2026-04-18).
//
// The routes that name an agent (bridge.IsAgentTokenRoute: the conversation
// routes and the permission callback) also admit
// an agent's own per-agent daemon token in bridge.AgentTokenHeader, which
// agentTokens verifies against the daemon (gd-P5wjHDweih): the request then
// carries the token's agent (bridge.WithAgentCaller), and the route takes the
// agent from it, never from ?agent=. A token the daemon refuses is a 401,
// whatever bearer rides beside it; a token that is not an agent's falls
// through to the bearer. An OIDC admin is marked bridge.WithOperatorCaller.
//
// When OIDC is not configured, regular /api/* routes fail closed (403): no
// OIDC means no verified identity, and X-Auth-Request-Email is a header any pod
// can send. insecureOpenAPI (SLACK_BRIDGE_INSECURE_OPEN_API=1) is the explicit,
// dev-only opt-out that restores the old open behaviour; a startup error is
// logged when it is set.
func bridgeRBAC(
	next http.Handler,
	oidcMW func(http.Handler) http.Handler,
	agentToken string,
	agentTokens agentTokenVerifier,
	oidcConfigured, insecureOpenAPI bool,
	logger *slog.Logger,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Non-API paths: no OIDC, no RBAC (health, Slack webhooks, UI).
		if !strings.HasPrefix(path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		required := bridgeRouteRole(r.Method, path)

		// Agent-facing endpoints: an agent's own token, the bearer token, then
		// OIDC admin fallback.
		if required == bridgeRoleAgent {
			if tok := r.Header.Get(bridge.AgentTokenHeader); tok != "" && agentTokens != nil && bridge.IsAgentTokenRoute(r.Method, path) {
				caller, err := agentTokens.Verify(r.Context(), tok)
				switch {
				case err == nil:
					next.ServeHTTP(w, r.WithContext(bridge.WithAgentCaller(r.Context(), caller)))
					return
				case errors.Is(err, bridge.ErrAgentTokenRejected):
					logger.Debug("bridge agent token rejected", "path", path, "method", r.Method)
					http.Error(w, "agent token rejected", http.StatusUnauthorized)
					return
				case !errors.Is(err, bridge.ErrNotAgentToken):
					logger.Warn("bridge agent token unverifiable", "path", path, "error", err)
					http.Error(w, "agent token could not be verified", http.StatusServiceUnavailable)
					return
				}
			}
			if agentToken != "" && bearerTokenMatches(r, agentToken) {
				next.ServeHTTP(w, r)
				return
			}
			// Fall back to OIDC admin (operators debugging agent endpoints).
			oidcMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				roles := oidcauth.RolesFromContext(r.Context())
				if len(roles) > 0 && oidcauth.HasMinimumRole(roles, oidcauth.RoleAdmin) {
					next.ServeHTTP(w, r.WithContext(bridge.WithOperatorCaller(r.Context())))
					return
				}
				// No bearer, no admin role — fail closed.
				// Insecure dev path: allow only if the operator explicitly
				// opted out of all auth and neither OIDC nor an agent token
				// is configured.
				if insecureOpenAPI && !oidcConfigured && agentToken == "" {
					next.ServeHTTP(w, r)
					return
				}
				logger.Debug("bridge agent endpoint denied",
					"path", path, "method", r.Method, "roles", roles)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			})).ServeHTTP(w, r)
			return
		}

		// Regular API routes: require OIDC role. Never fall open on empty
		// roles when OIDC is configured — that was the C1 bypass.
		oidcMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			roles := oidcauth.RolesFromContext(r.Context())
			if len(roles) == 0 {
				if !oidcConfigured && insecureOpenAPI {
					// Explicit dev-only open mode (SLACK_BRIDGE_INSECURE_OPEN_API=1).
					next.ServeHTTP(w, r)
					return
				}
				if !oidcConfigured {
					// No OIDC means no identity, so fail closed.
					// X-Auth-Request-Email is a header any pod can send.
					http.Error(w, "bridge web API requires OIDC (set OIDC_ISSUER_URL)", http.StatusForbidden)
					return
				}
				logger.Debug("bridge RBAC denied (no roles)",
					"path", path, "method", r.Method, "required", required)
				http.Error(w, "insufficient permissions", http.StatusForbidden)
				return
			}
			if required == bridgeRolePerson && oidcauth.EmailFromContext(r.Context()) != "" {
				next.ServeHTTP(w, r)
				return
			}
			if !oidcauth.HasMinimumRole(roles, required) {
				logger.Debug("bridge RBAC denied",
					"path", path, "method", r.Method,
					"required", required, "roles", roles)
				http.Error(w, "insufficient permissions", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})).ServeHTTP(w, r)
	})
}

// oidcOnlyOnCommonsInbound scopes an OIDC middleware to the Commons inbound
// route (POST /api/conversation/inbound, which the /garden card actions and
// lightbox submissions both call): there it verifies the bearer as before;
// on every other route it refuses with 403 and verifies nothing, so no OIDC
// identity reaches the role-gated web routes or the agent routes' OIDC-admin
// fallback (SLACK_BRIDGE_OIDC_COMMONS_ONLY, gd-na5unRTCaP). The agent routes'
// own tokens are checked before the fallback and are unaffected.
func oidcOnlyOnCommonsInbound(oidcMW func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		verified := oidcMW(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/api/conversation/inbound" {
				verified.ServeHTTP(w, r)
				return
			}
			http.Error(w, "bridge OIDC is scoped to the Commons inbound route", http.StatusForbidden)
		})
	}
}

// bridgeRoleAgent is a sentinel returned by bridgeRouteRole for endpoints
// that are authenticated by a shared bearer token rather than an OIDC role.
const bridgeRoleAgent = "__agent__"

// bridgeRolePerson is a sentinel returned by bridgeRouteRole for a route any
// OIDC-verified person may call, whatever their realm role: the handler
// gates each request itself (the Commons inbound call, by land role). A
// project member holds no rank in the gardenio hierarchy, so no role gate
// would admit them.
const bridgeRolePerson = "__person__"

// bridgeRouteRole returns the minimum Keycloak role required for a bridge
// API route, or bridgeRoleAgent for agent-facing endpoints authenticated by
// a shared bearer token.
func bridgeRouteRole(method, path string) string {
	switch {
	// --- viewer: read-only API access ---
	case method == http.MethodGet && strings.HasPrefix(path, "/api/decisions"):
		return oidcauth.RoleViewer
	case method == http.MethodGet && strings.HasPrefix(path, "/api/escalations"):
		return oidcauth.RoleViewer
	case method == http.MethodGet && path == "/api/unreleased":
		return oidcauth.RoleViewer

	// --- operator: mutations on decisions and escalations ---
	case method == http.MethodPost && strings.HasPrefix(path, "/api/decisions/"):
		return oidcauth.RoleOperator
	case method == http.MethodPost && strings.HasPrefix(path, "/api/escalations/"):
		return oidcauth.RoleOperator

	// --- any signed-in person: the Commons inbound call, which checks each
	// action against the caller's land role itself (the agent bearer never
	// acts for a human here) ---
	case strings.HasPrefix(path, "/api/conversation/inbound"):
		return bridgeRolePerson

	// --- agent-facing: shared bearer token (or OIDC admin fallback) ---
	case path == "/api/conversation/say" || path == "/api/conversation/thread":
		return bridgeRoleAgent
	case strings.HasPrefix(path, "/api/slack/"):
		return bridgeRoleAgent
	case strings.HasPrefix(path, "/api/permission/"):
		return bridgeRoleAgent

	default:
		// Unknown API route — require viewer as baseline.
		return oidcauth.RoleViewer
	}
}

// bearerTokenMatches reports whether the request carries a matching bearer
// token in Authorization or X-Auth-Request-Access-Token. Comparison is
// constant-time to avoid timing oracles.
func bearerTokenMatches(r *http.Request, expected string) bool {
	if expected == "" {
		return false
	}
	got := ""
	if auth := r.Header.Get("Authorization"); auth != "" {
		if tok, ok := strings.CutPrefix(auth, "Bearer "); ok {
			got = strings.TrimSpace(tok)
		}
	}
	if got == "" {
		got = r.Header.Get("X-Auth-Request-Access-Token")
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// landListener is the handler on the bridge's land listener
// (SLACK_BRIDGE_LAND_LISTEN_ADDR), the one port a tenant land's pods may reach
// (landns.BridgeLandPort, gd-pXik57wZA9). It serves only the routes that take
// an agent's identity from its per-agent token (bridge.IsAgentTokenRoute) and
// admits only such a token: no shared bearer, no OIDC, no insecure dev mode.
// Every other path is a 404, so a route added to the bridge later is never
// reachable from a land unless it joins IsAgentTokenRoute. Credential headers
// other than the agent token are dropped before next sees the request.
func landListener(next http.Handler, agentTokens agentTokenVerifier, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !bridge.IsAgentTokenRoute(r.Method, r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		tok := r.Header.Get(bridge.AgentTokenHeader)
		if tok == "" || agentTokens == nil {
			http.Error(w, "an agent token is required", http.StatusUnauthorized)
			return
		}
		caller, err := agentTokens.Verify(r.Context(), tok)
		switch {
		case err == nil:
		case errors.Is(err, bridge.ErrAgentTokenRejected), errors.Is(err, bridge.ErrNotAgentToken):
			logger.Debug("land listener: token refused", "path", r.URL.Path, "error", err)
			http.Error(w, "agent token rejected", http.StatusUnauthorized)
			return
		default:
			logger.Warn("land listener: agent token unverifiable", "path", r.URL.Path, "error", err)
			http.Error(w, "agent token could not be verified", http.StatusServiceUnavailable)
			return
		}
		for _, h := range []string{"Authorization", "X-Auth-Request-Access-Token", "X-Auth-Request-Email", "X-Auth-Request-User", "X-Auth-Request-Groups", "X-Forwarded-Email", "X-Forwarded-User"} {
			r.Header.Del(h)
		}
		next.ServeHTTP(w, r.WithContext(bridge.WithAgentCaller(r.Context(), caller)))
	})
}
