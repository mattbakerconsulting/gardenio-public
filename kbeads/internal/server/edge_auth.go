// EdgeAuthMiddleware implements the daemon side of the Stage 3 / W-B-11
// trust handshake (ka-0000001678). It sits at the top of the HTTP chain
// and inspects the headers stamped by gardenio-edge-auth
// (gardenio/controller/cmd/edge-auth/proxy.go):
//
//	X-Operator        : authenticated operator email
//	X-Auth-Audit-Id   : correlation id
//	X-Edge-Auth-Ts    : unix seconds
//	X-Edge-Auth-Sig   : base64 HMAC-SHA256 signature
//
// A verified v2 envelope also carries auth mode and canonical Gardenio roles,
// and binds the HTTP method. Legacy v1 envelopes remain read-compatible but
// can never authorize a mutation.
//
// When the envelope verifies (and HMAC verification is configured), the
// middleware skips the legacy AuthMiddleware entirely: it stamps the
// AuthIdentity{Type:"edge", Subject:operator} into the request context,
// re-uses applyOperatedByHeader so audit and operator-attribution paths
// pick the operator up, and dispatches to the downstream handler.
//
// When the envelope is missing (no X-Operator), the middleware delegates
// to the fallback (legacy AuthMiddleware) so static-token / fleet-token /
// OIDC paths continue to work — this is what keeps in-cluster gRPC peers
// and existing PAT-based pods working through the rollout.
//
// When the envelope is present but does not verify, behavior depends on
// the Enforce flag (mirrors daemonHmacVerify.enforce in the chart):
//
//   - Enforce=false (burn-in, default): the daemon trusts the asserted
//     operator anyway and logs the failed verification. This lets the
//     operator confirm a clean ≥7d soak per env (per runbook ka-0000000026)
//     before flipping.
//   - Enforce=true: the middleware returns 401 and never reaches the
//     downstream chain.
package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/edgeauth"
)

const authMethodEdge authMethod = "edge"

// EdgeAuthMiddleware returns a middleware that verifies the gardenio-edge-auth
// HMAC envelope on inbound HTTP requests.
//
//   - cfg controls the HMAC key, enforce mode, and skew window. When
//     cfg.Enabled() is false (no key configured) AND Enforce is false, the
//     middleware is a no-op pass-through to fallback — this is the
//     pre-rollout state. When Enforce is true with no key, every X-Operator
//     request 401s (defence-in-depth: an enforce-mode daemon must have the
//     key).
//   - trustedHandler is the downstream chain to dispatch to AFTER the
//     envelope verifies (e.g. the chain WITHOUT AuthMiddleware).
//   - fallback is the chain to dispatch to when X-Operator is absent
//     (e.g. the chain WITH AuthMiddleware) so non-edge-auth callers
//     continue to work.
//
// The middleware never overwrites an AuthIdentity already in the context.
func EdgeAuthMiddleware(cfg edgeauth.Config, trustedHandler, fallback http.Handler) http.Handler {
	enforceLabel := strconv.FormatBool(cfg.Enforce)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health is exempt to keep readiness/liveness probes drama-free
		// (both /v1/health and the cheap /v1/health/live, ka-0000001764).
		if isProbeHealthPath(r.Method, r.URL.Path) {
			fallback.ServeHTTP(w, r)
			return
		}

		// No X-Operator → defer to the legacy auth chain.
		if r.Header.Get(edgeauth.HeaderOperator) == "" {
			fallback.ServeHTTP(w, r)
			return
		}

		res := cfg.Verify(r)
		edgeAuthVerifyTotal.WithLabelValues(string(res.Outcome), enforceLabel).Inc()
		edgeauth.LogResult(slog.Default(), res, cfg.Enforce)

		if !res.Trusted {
			// Enforce mode + non-OK outcome → reject.
			writeError(w, http.StatusUnauthorized, "edge-auth verification failed")
			return
		}

		// Version 2 carries roles and is role-enforced; a cryptographically
		// verified v1 envelope carries none. Decision ka-0000001641 once let
		// a v1 envelope keep its pre-MR authority (a bypass of every
		// OIDC-gated RBAC check); gd-AvSKOUyqRz retired it, so a v1 envelope
		// is now non-role-bearing, like the static token. Three cases, gated
		// on the cryptographic Outcome, never on res.Trusted:
		//
		//   - verified v2 (Outcome==OK && Version==2): OIDC-equivalent role
		//     enforcement, exactly as the MR intends for the Entra/Ziti paths.
		//   - verified v1 (Outcome==OK && Version==1): authMethodEdge, which is
		//     not roleBearing. requireRole, the RBAC interceptors, write_policy
		//     and the admin field gates treat it like the static token:
		//     operator-level routes admit it, RoleAdmin routes and admin-only
		//     fields refuse it. Its signer holds only the shared HMAC key, a
		//     credential no stronger than the primary static token.
		//   - trusted only because burn-in is on (Enforce=false, non-OK
		//     outcome): identity attribution only, pinned to viewer via
		//     authMethodOIDC so it can never mutate. Operator invariant:
		//     "burn-in-trusted invalid envelopes must not receive legacy
		//     mutation authority." (Prod runs Enforce=true, so this branch is
		//     unreachable there; it exists for correctness in a burn-in env.)
		//
		// A request carrying X-Edge-Auth-Version:2 whose v2 signature fails
		// verification is NOT verified v1 (its Version is 2) and NOT VerifiedV2,
		// so it falls to the burn-in branch (viewer-only) or is rejected under
		// enforce — it never downgrades to v1 authority.
		roleExempt := edgeRoleExempt(r)
		verifiedV1 := res.Outcome == edgeauth.OutcomeOK && res.Version == 1

		// Use authType "edge" for applyOperatedByHeader so the existing
		// static-only X-Operated-By trust check does NOT short-circuit edge-auth
		// callers — operator trust here is derived from the HMAC, not the
		// X-Operated-By header itself.
		var (
			method authMethod
			roles  []string
		)
		switch {
		case res.VerifiedV2:
			// Treat a verified role-bearing edge identity as OIDC-equivalent for
			// role, project, type-policy, ownership, and provisioning checks.
			// AuthIdentity remains Type=edge so attribution still records the
			// actual trust path and resolveOperatedBy keeps its edge behavior.
			method = authMethodOIDC
			roles = res.Roles
		case verifiedV1:
			// No role is attached and no floor applies below: authMethodEdge
			// is gated downstream like the static token (gd-AvSKOUyqRz).
			method = authMethodEdge
		default:
			// Burn-in-trusted but not cryptographically verified: read-only.
			method = authMethodOIDC
			roles = []string{RoleViewer}
		}

		// Global operator floor before dispatching a role-bearing (OIDC-
		// equivalent) mutation. Most native REST routes also use requireRole,
		// but protocol adapters such as the Connect shim dispatch directly to
		// handlers and must not become an RBAC bypass. verified-v1 is
		// exempt — authMethodEdge carries no roles, so downstream gates treat
		// it like the static token instead. Route-specific admin and policy
		// checks still run downstream after this minimum gate.
		if method == authMethodOIDC && !legacyEdgeReadAllowed(r) && !roleExempt && !hasMinimumRole(roles, RoleOperator) {
			slog.Warn("edge-auth mutation denied",
				"operator", res.Operator,
				"audit_id", res.AuditID,
				"auth_mode", res.AuthMode,
				"roles", roles,
				"method", r.Method,
				"path", r.URL.Path,
				"reason", "operator_role_required",
			)
			writeError(w, http.StatusForbidden, "insufficient permissions: requires "+RoleOperator)
			return
		}
		ctx := withAuthMethod(r.Context(), method)
		ctx = WithRoles(ctx, roles)
		ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: res.Operator})
		// Operator attribution: the sanitizer requires a human-operator
		// shape ("oidc:<email>" or "slack:U…"). gardenio-edge-auth resolves
		// the caller from a GitLab PAT (an OIDC-equivalent credential), so
		// the email maps to oidc:<email> for audit / X-Operated-By
		// purposes. Pre-prefix and bypass the sanitizer's no-prefix path so
		// the bare email shape doesn't get rejected.
		operator := res.Operator
		if !strings.HasPrefix(operator, "oidc:") && !strings.HasPrefix(operator, "slack:") {
			operator = "oidc:" + operator
		}
		ctx = applyOperatedByHeader(ctx, operator, "edge", "http:"+r.Method+" "+r.URL.Path)

		if res.Outcome == edgeauth.OutcomeOK && res.AuditID != "" {
			w.Header().Set(edgeauth.HeaderAuditID, res.AuditID)
		}

		if legacyEdgeReadAllowed(r) {
			trustedHandler.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		trustedHandler.ServeHTTP(rec, r.WithContext(ctx))
		slog.Info("edge-auth mutation completed",
			"operator", res.Operator,
			"audit_id", res.AuditID,
			"auth_mode", res.AuthMode,
			"roles", roles,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.statusCode,
		)
	})
}

// legacyEdgeReadAllowed is deliberately narrower than "all POSTs are writes":
// KBeads has POST endpoints whose bodies are read-only queries. Everything else
// requires a verified v2 role-bearing envelope and clears the global operator
// mutation floor above.
func legacyEdgeReadAllowed(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	case http.MethodPost:
		if r.URL.Path == "/v1/search/semantic" || r.URL.Path == "/v1/graph" {
			return true
		}
		return false
	default:
		return false
	}
}

// edgeRoleExempt identifies mutation-shaped endpoints whose authorization
// boundary is deliberately not the caller's bearer identity. The refresh
// exchange authenticates the plaintext refresh token in handleRefreshToken;
// requiring a v2 operator envelope before that handler would break fleet and
// cookie clients without strengthening the exchange.
func edgeRoleExempt(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/auth/refresh"
}
