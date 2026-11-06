// Package oidcauth provides HTTP middleware for OIDC token validation.
package oidcauth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Tunables for OIDC provider initialization retries. Exported as package
// variables so tests can bound/shorten the retry loop; production code should
// leave these at their defaults. Retry schedule is exponential backoff
// starting at providerRetryInitialDelay and doubling each attempt, capped at
// providerRetryMaxDelay.
//
// providerRetryAttempts == 0 means retry INDEFINITELY (until the supplied
// context is cancelled). This is the production default: discovery runs in a
// background goroutine (see New) so a briefly-unreachable issuer no longer
// fails the pod closed forever — it keeps retrying and flips Ready() true
// once discovery finally succeeds, without a pod restart (ka-0000001839).
// A positive value bounds the loop to that many attempts (used by tests).
var (
	providerRetryAttempts     = 0
	providerRetryInitialDelay = 1 * time.Second
	providerRetryMaxDelay     = 30 * time.Second
)

type contextKey string

const (
	ctxEmail contextKey = "oidcauth.email"
	ctxRoles contextKey = "oidcauth.roles"
	// ctxGroups carries the verified token's groups claim. The viewer derives a
	// scoped human's project set from it (cmd/beads-viewer scope.go), the same
	// claim kbeads reads, so both sides agree on what the user may see.
	ctxGroups contextKey = "oidcauth.groups"
	// ctxOIDCVerified marks an identity that came from a verified OIDC token
	// (verifyingMiddleware), as opposed to AVA, a k8s ServiceAccount token, the
	// dev bypass or no auth at all. Only such a caller can be a Lands applicant:
	// a signed-in realm user with no gardenio role (cmd/beads-viewer scope.go).
	ctxOIDCVerified contextKey = "oidcauth.oidc_verified"
	// ctxGitHubID carries the verified token's github_id claim: the numeric id
	// of the GitHub account a user signed in through (Keycloak's GitHub IdP
	// mapper, Lands Track B §2.2 item 1). The Lands setup check compares it with
	// the GitHub user who authorized the Lands App (cmd/beads-viewer
	// lands_github.go).
	ctxGitHubID contextKey = "oidcauth.github_id"
	// ctxGitHubLogin carries the verified token's github_login claim, the
	// GitHub username of the same account (a label: it can be renamed, the id
	// cannot).
	ctxGitHubLogin contextKey = "oidcauth.github_login"
	// ctxSubject carries the verified token's sub claim, the user's stable
	// Keycloak id. A Lands plot application is keyed by it (applicant_sub,
	// cmd/beads-viewer lands_apply.go): the email is only a label.
	ctxSubject contextKey = "oidcauth.subject"
	// ctxAudiences carries the verified token's aud claim. landgate reads the
	// per-land land-<name> audience from it (owner decision gd-zf6eceweED):
	// a land door admits only a token minted for its own land.
	ctxAudiences contextKey = "oidcauth.audiences"
	// ctxImpersonator carries the verified token's impersonator claim: the
	// admin acting behind an impersonated Keycloak session (one-click Become,
	// plan gd-0SUIdKXWV3). It is set only from a verified edge bearer
	// (cmd/beads-viewer attachEdgeBearerRoles), never from a request header.
	ctxImpersonator contextKey = "oidcauth.impersonator"
)

// EmailFromContext returns the authenticated user's email from the request context.
func EmailFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxEmail).(string)
	return v
}

// RolesFromContext returns the authenticated user's OIDC roles from the request context.
func RolesFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(ctxRoles).([]string)
	return v
}

// GroupsFromContext returns the verified token's groups claim, or nil when the
// token carried none (or the request did not come through the Keycloak path).
func GroupsFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(ctxGroups).([]string)
	return v
}

// OIDCVerifiedFromContext reports whether the request's identity came from a
// verified OIDC token. AVA, ServiceAccount and dev-bypass identities, and
// requests with no auth, report false.
func OIDCVerifiedFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(ctxOIDCVerified).(bool)
	return v
}

// GitHubIDFromContext returns the verified token's github_id claim, or 0 when
// the token carried none (a user who did not sign in through GitHub).
func GitHubIDFromContext(ctx context.Context) int64 {
	v, _ := ctx.Value(ctxGitHubID).(int64)
	return v
}

// GitHubLoginFromContext returns the verified token's github_login claim, or ""
// when the token carried none or it is not a valid GitHub username.
func GitHubLoginFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxGitHubLogin).(string)
	return v
}

// SubjectFromContext returns the verified OIDC token's sub claim, or "" when
// the identity did not come from a verified OIDC token.
func SubjectFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxSubject).(string)
	return v
}

// AudiencesFromContext returns the verified OIDC token's aud claim, or nil
// when the identity did not come from a verified OIDC token.
func AudiencesFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(ctxAudiences).([]string)
	return v
}

// ImpersonatorFromContext returns the verified token's impersonator claim, or
// "" when the session is not impersonated (or its token was not verified).
func ImpersonatorFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxImpersonator).(string)
	return v
}

// ContextWithImpersonator returns a copy of ctx carrying an impersonator
// claim. Only a verified token's claim may be put here.
func ContextWithImpersonator(ctx context.Context, impersonator string) context.Context {
	return context.WithValue(ctx, ctxImpersonator, impersonator)
}

// ContextWithSubject returns a copy of ctx carrying a sub claim. Only a
// verified token's claim may be put here (the viewer's edge bearer path, and
// tests).
func ContextWithSubject(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, ctxSubject, sub)
}

// ContextWithGitHubID returns a copy of ctx carrying a github_id. Only a
// verified token's claim may be put here (the viewer's edge bearer path).
func ContextWithGitHubID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, ctxGitHubID, id)
}

// ContextWithGitHubLogin returns a copy of ctx carrying a github_login. Only a
// verified token's claim may be put here (the viewer's edge bearer path).
func ContextWithGitHubLogin(ctx context.Context, login string) context.Context {
	return context.WithValue(ctx, ctxGitHubLogin, login)
}

// ContextWithOIDCVerified returns a copy of ctx marked as carrying a verified
// OIDC identity. Intended for tests, like ContextWithRoles.
func ContextWithOIDCVerified(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxOIDCVerified, true)
}

// ContextWithGroups returns a copy of ctx with the given token groups attached.
// Intended for tests and internal callers, like ContextWithRoles.
func ContextWithGroups(ctx context.Context, groups []string) context.Context {
	return context.WithValue(ctx, ctxGroups, groups)
}

// ContextWithRoles returns a copy of ctx with the given OIDC roles attached.
// Intended for tests and internal callers that need to simulate an
// authenticated user without going through the OIDC middleware.
func ContextWithRoles(ctx context.Context, roles []string) context.Context {
	return context.WithValue(ctx, ctxRoles, roles)
}

// ContextWithEmail returns a copy of ctx with the given authenticated email
// attached — the same value the AVA/Keycloak middleware injects on success, and
// the value EmailFromContext reads back. Intended for tests
// and internal callers (e.g. the beads-viewer /grafana proxy tests) that need
// to simulate an authenticated user without standing up the OIDC middleware.
func ContextWithEmail(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, ctxEmail, email)
}

// AccessTokenFromRequest returns the caller's OIDC access token — the
// Authorization Bearer value, or the X-Auth-Request-Access-Token header set by
// the oauth2-proxy ForwardAuth edge. Returns "" when neither is present (e.g.
// AVA-gated requests, which carry an edge-signed user-context header instead of
// a forwardable bearer). Used by the viewer's write-bearer enforcement
// (ka-0000006379) to authenticate daemon writes as the human user.
func AccessTokenFromRequest(r *http.Request) string {
	return extractToken(r)
}

// There is deliberately no helper that reads the X-Auth-Request-Email header:
// a client can send it, and on the AVA edge nothing strips it. Identity comes
// from EmailFromContext only (ka-0000002979, gd-Fhizmbk9Ee).

// Config holds the OIDC middleware configuration.
type Config struct {
	IssuerURL                string   // OIDC issuer URL
	ClientID                 string   // Expected audience (client ID)
	ExpectedTenantID         string   // Required tid claim (empty = do not check)
	RequiredRole             string   // Required OIDC role (empty = any authenticated user)
	RequiredScopes           []string // Required exact values in the space-delimited scp claim
	AllowedAuthorizedParties []string // Allowed azp client IDs (empty = do not check)
	AllowedEmailDomains      []string // Allowed email domains (empty = any domain)

	// AVA configures the AWS Verified Access branch of the middleware. When
	// AVA.TrustSigner is set, requests carrying the AVA user-context header
	// are verified and trusted in place of a Keycloak bearer token; requests
	// without that header continue through the Keycloak path. The two paths
	// coexist so we can keep serving the legacy Keycloak-gated hostname
	// while migrating to the AVA-gated one.
	AVA AVAConfig

	// K8sSA configures the EKS ServiceAccount-token branch (ka-0000006288).
	// When K8sSA.IssuerURL is set, a bearer token whose (unverified) issuer
	// matches it is routed to a SECOND verifier — real JWKS + issuer + audience
	// + expiry validation against the EKS OIDC issuer — and, on success, its
	// allow-listed ServiceAccount subject is mapped to a synthetic domain-valid
	// email + a fixed role floor so new-prod audit pods clear the same
	// RequiredRole / AllowedEmailDomains gates a human does. Opt-in and
	// background-discovered exactly like the Keycloak path (fail-closed 503
	// until discovery); an unset config leaves the middleware byte-identical.
	K8sSA K8sSAConfig
}

// Handle wraps OIDC middleware state with a readiness check. Callers should
// wire Ready() into their /readyz probe so K8s does not route traffic to pods
// where the OIDC provider is still initializing.
//
// When IssuerURL is empty (OIDC disabled), Ready() returns true and
// Middleware is a passthrough.
//
// When IssuerURL is set, provider discovery runs in a BACKGROUND goroutine
// that retries (by default indefinitely, bounded only by the context passed
// to New). Until discovery succeeds, the OIDC request branch rejects every
// request with HTTP 503 — it MUST NEVER passthrough when auth was configured
// but is not yet initialized (see ka-0000006185 / ka-0000004369 F1).
// OIDC-only handles also report not-ready in that window. Once discovery
// succeeds, the verifying middleware is swapped in atomically and readiness
// flips true without a pod restart (ka-0000001839).
//
// The AVA branch (when configured) does NOT depend on Keycloak readiness: it
// is wired up synchronously in New and serves AVA-authenticated requests
// immediately, even while Keycloak discovery is still pending.
type Handle struct {
	// keycloakWrap holds the Keycloak middleware factory. It starts as
	// failClosedMiddleware (503) when discovery is pending and is swapped to
	// the verifying middleware once background discovery succeeds. Read on
	// every request via Middleware, so access is guarded by an atomic.
	keycloakWrap atomic.Pointer[func(http.Handler) http.Handler]

	// middleware is the request entrypoint. For the disabled and AVA-only
	// passthrough cases it is set once in New and never changes. For the
	// Keycloak (and AVA+Keycloak) cases it dereferences keycloakWrap on every
	// request so the background discovery swap takes effect live.
	middleware func(http.Handler) http.Handler

	ready atomic.Bool

	// oidcReady reports whether the OIDC/Keycloak request branch has finished
	// provider discovery and been swapped from fail-closed (503) to the
	// verifying middleware. It is true immediately when Keycloak is disabled
	// (there is no OIDC branch to discover). It exists SEPARATELY from ready
	// because a dual AVA+OIDC handle sets ready true synchronously via its AVA
	// branch — BEFORE background discovery completes — so ready alone cannot
	// tell whether the Keycloak path is verifying yet. Consumed by tests via
	// oidcDiscovered() to gate on the actual discovery swap rather than racing
	// it (ka-0000003670).
	oidcReady atomic.Bool

	// saWrap holds the EKS SA-token middleware factory (ka-0000006288). Like
	// keycloakWrap it starts as failClosedMiddleware (503) and is swapped to
	// the verifying middleware once background discovery of the EKS issuer
	// succeeds. Only used when cfg.K8sSA is enabled; the SA request branch
	// dereferences it per request via loadSAWrap so the swap takes effect live.
	saWrap atomic.Pointer[func(http.Handler) http.Handler]

	// saReady mirrors oidcReady for the EKS SA-token branch: true once SA
	// discovery has swapped fail-closed (503) for the verifier, or immediately
	// when the SA branch is disabled. Consumed by tests via saDiscovered().
	saReady atomic.Bool
}

// loadKeycloakWrap returns the current Keycloak middleware factory (503
// fail-closed until background discovery succeeds, then the verifier).
func (h *Handle) loadKeycloakWrap() func(http.Handler) http.Handler {
	return *h.keycloakWrap.Load()
}

// Middleware wraps next so it is invoked only after successful OIDC token
// validation. When OIDC is disabled it is a passthrough; while provider
// discovery is still pending (or has been cancelled) it returns 503 for every
// request on the Keycloak path.
func (h *Handle) Middleware(next http.Handler) http.Handler {
	return h.middleware(next)
}

// Ready reports whether at least one configured authentication branch can
// serve traffic. OIDC-only handles become ready after provider discovery. A
// dual AVA+OIDC handle is ready immediately because AVA verification is wired
// synchronously, while its OIDC branch remains fail-closed until discovery.
func (h *Handle) Ready() bool {
	return h.ready.Load()
}

// New initializes an OIDC Handle. When IssuerURL is set, provider discovery
// runs in a BACKGROUND goroutine that retries with exponential backoff (see
// providerRetryAttempts / providerRetryInitialDelay / providerRetryMaxDelay;
// the production default is indefinite retry, bounded only by ctx). Until
// discovery succeeds the OIDC branch fails closed with HTTP 503. OIDC-only
// handles report not-ready; dual AVA+OIDC handles remain ready through their
// synchronous AVA branch. Once discovery succeeds the verifying middleware is
// swapped in and OIDC requests work WITHOUT a pod restart (ka-0000001839).
// Callers should check Ready() from their readiness probe.
//
// ctx controls the lifetime of the background discovery loop: cancel it
// (e.g. on SIGTERM) to interrupt an in-flight discovery so shutdown is not
// blocked by an uninterruptible retry budget. If discovery has not yet
// succeeded when ctx is cancelled, the OIDC branch stays fail-closed.
//
// When cfg.AVA.TrustSigner is set, an AVA branch is also installed
// synchronously: requests carrying X-Amzn-Ava-User-Context are routed through
// ES384 JWT verification against the pinned AVA instance signer; requests
// without that header continue through the Keycloak path (or are passed
// through when Keycloak is also disabled). The AVA branch does NOT depend on
// Keycloak readiness and serves requests immediately, even while Keycloak
// discovery is still pending.
func New(ctx context.Context, cfg Config, logger *slog.Logger) *Handle {
	h := &Handle{}
	// Start fail-closed: the Keycloak path returns 503 until background
	// discovery succeeds. This is the load-bearing invariant — a configured
	// but not-yet-initialized auth path MUST NEVER passthrough.
	failClosed := failClosedMiddleware
	h.keycloakWrap.Store(&failClosed)
	// The EKS SA-token branch (ka-0000006288) starts fail-closed too, for the
	// same reason: a request claimed by the SA branch before discovery
	// completes gets a 503, never a passthrough.
	saFailClosed := failClosedMiddleware
	h.saWrap.Store(&saFailClosed)

	var avaVer *avaVerifier
	if cfg.AVA.enabled() {
		avaVer = newAVAVerifier(cfg.AVA, logger)
		logger.Info("AVA JWT auth branch enabled", "trust_signer", cfg.AVA.TrustSigner, "region", cfg.AVA.region())
	}

	keycloakEnabled := cfg.IssuerURL != ""

	// base is the existing auth chain (Keycloak / AVA / passthrough). The EKS
	// SA-token branch, when enabled, wraps base: it CLAIMS a bearer token whose
	// unverified issuer matches the EKS issuer and delegates everything else to
	// base unchanged, so the human Entra/AVA path is byte-identical.
	var base func(http.Handler) http.Handler
	switch {
	case !keycloakEnabled:
		// Keycloak disabled. If AVA is also disabled, passthrough.
		// If only AVA is enabled, requests without the AVA header pass
		// through unauthenticated — beads-viewer pairs this with the IP
		// whitelist + the avaMiddleware short-circuit, but other callers
		// should make sure they actually want this combination.
		if avaVer == nil {
			base = func(next http.Handler) http.Handler { return next }
		} else {
			base = func(next http.Handler) http.Handler {
				return authBranch(avaVer, cfg.AVA.DefaultRole, next, next, logger)
			}
		}
		h.ready.Store(true)
		// No OIDC branch to discover: the "discovery complete" signal is
		// satisfied immediately so waitReady() does not block for a handle that
		// has no Keycloak path.
		h.oidcReady.Store(true)
	case avaVer == nil:
		// Keycloak enabled, AVA disabled. The request entrypoint dereferences
		// keycloakWrap on every request (via loadKeycloakWrap) so the background
		// discovery swap below takes effect on live traffic without re-wiring
		// the mux.
		base = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.loadKeycloakWrap()(next).ServeHTTP(w, r)
			})
		}
	default:
		// Keycloak + AVA both enabled.
		base = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				keycloakChain := h.loadKeycloakWrap()(next)
				authBranch(avaVer, cfg.AVA.DefaultRole, keycloakChain, next, logger).ServeHTTP(w, r)
			})
		}
		// AVA can serve the existing public edge immediately. Keep the OIDC
		// request branch fail-closed until discovery succeeds, but do not make
		// a transient Entra outage remove the shared viewer pod from AVA.
		h.ready.Store(true)
	}

	if keycloakEnabled {
		// Run provider discovery off the synchronous critical path. The OIDC
		// request branch returns immediately fail-closed; the goroutine installs
		// verification once discovery succeeds. OIDC-only handles also become
		// ready then; dual AVA+OIDC handles are already ready via AVA.
		// Snapshot the retry policy here (in the caller's goroutine) so the
		// background goroutine never races test teardown that rewrites the globals.
		go h.discoverKeycloak(ctx, cfg, currentRetryPolicy(), logger)
	}

	if cfg.K8sSA.enabled() {
		// The SA branch is ADDITIVE: it never widens what base admits, only
		// routes EKS-issued bearer tokens to a second verifier. gatesReady is
		// true only when there is NO base auth path (defensive — Validate
		// requires one, so in practice base owns readiness); in that case SA
		// discovery flips the handle ready on success.
		gatesReady := !keycloakEnabled && avaVer == nil
		saCfg := cfg.K8sSA
		logger.Info("EKS SA-token auth branch enabled", "issuer", saCfg.IssuerURL, "audience", saCfg.Audience, "allowed_subjects", len(saCfg.AllowedSubjects))
		h.middleware = func(next http.Handler) http.Handler {
			return saBranch(h, saCfg, base(next), next)
		}
		go h.discoverSA(ctx, saCfg, currentRetryPolicy(), gatesReady, logger)
	} else {
		h.middleware = base
		// No SA branch to discover: satisfy the "discovery complete" signal
		// immediately so saDiscovered() does not block for a handle with no
		// SA path.
		h.saReady.Store(true)
	}

	return h
}

// saBranch routes a request to the EKS SA-token verifier when the bearer
// token's UNVERIFIED issuer matches the configured EKS issuer, otherwise
// delegates to the base auth chain (Keycloak/AVA/passthrough) unchanged.
//
// Selection is issuer-claim-based, NOT header-based — a projected SA token and
// an Entra token both arrive on Authorization: Bearer, so a custom "machine
// token" header would be exactly the over-trust shape ka-0000002979 fixed. The
// unverified issuer peek grants no trust: a request the SA branch CLAIMS is
// fully verified by loadSAWrap()(next) (JWKS + issuer + audience + expiry +
// signature) and NEVER falls through to base — a forged iss that fails
// verification is a hard 401/403, not a downgrade to a different auth path.
// While SA discovery is pending, loadSAWrap() is the fail-closed 503 middleware.
func saBranch(h *Handle, cfg K8sSAConfig, baseChain, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawToken := extractToken(r)
		if rawToken == "" || unverifiedIssuer(rawToken) != cfg.IssuerURL {
			baseChain.ServeHTTP(w, r)
			return
		}
		h.loadSAWrap()(next).ServeHTTP(w, r)
	})
}

// discoverKeycloak runs OIDC provider discovery in the background and, on
// success, atomically swaps the OIDC middleware from fail-closed (503) to the
// verifying middleware and marks the Handle ready. If ctx is cancelled before
// discovery succeeds, the OIDC branch stays fail-closed; a synchronous AVA
// branch, when configured, can still keep the shared service ready.
func (h *Handle) discoverKeycloak(ctx context.Context, cfg Config, policy retryPolicy, logger *slog.Logger) {
	provider, err := newProviderWithRetry(ctx, cfg.IssuerURL, policy, logger)
	if err != nil {
		// Only reachable when ctx is cancelled (default attempts==0 retries
		// forever) or a bounded attempt budget is exhausted (tests). Keep the
		// OIDC branch fail-closed; readiness may still be true via AVA.
		logger.Error("OIDC provider discovery stopped before success — OIDC branch stays fail closed (503)", "issuer", cfg.IssuerURL, "service_ready", h.Ready(), "error", err)
		return
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	keycloakWrap := verifyingMiddleware(verifier, cfg, logger)
	h.keycloakWrap.Store(&keycloakWrap)
	h.ready.Store(true)
	// Signal the OIDC branch is now verifying (fail-closed 503 -> verifier
	// swap has landed). Store AFTER keycloakWrap so any reader that observes
	// oidcReady==true is guaranteed to also see the swapped-in verifier.
	h.oidcReady.Store(true)
	logger.Info("OIDC provider discovery succeeded — middleware now verifying; /readyz ready", "issuer", cfg.IssuerURL)
}

// authBranch routes requests to the AVA verifier when the AVA header is
// present, otherwise to the Keycloak chain. On AVA success, finalNext is
// invoked with email + groups injected into the request context (skipping
// keycloakChain so it does not demand a bearer token). On AVA failure, the
// request is rejected with 401 — falling through to Keycloak would silently
// downgrade a tampered AVA request to a different auth flow.
func authBranch(verifier *avaVerifier, defaultRole string, keycloakChain, finalNext http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get(AVAHeaderName)
		if raw == "" {
			keycloakChain.ServeHTTP(w, r)
			return
		}
		claims, err := verifier.verify(r.Context(), raw)
		if err != nil {
			logger.Debug("AVA token verification failed", "error", err, "path", r.URL.Path)
			http.Error(w, "invalid AVA token", http.StatusUnauthorized)
			return
		}
		ctx := r.Context()
		ctx = context.WithValue(ctx, ctxEmail, claims.effectiveEmail())
		// AVA forwards GitLab groups as full subgroup paths; normalize them to
		// the bare gardenio-* role names the RBAC hierarchy matches BEFORE
		// applying the viewer floor, so real operators/admins clear the
		// operatorOnly/adminOnly gates instead of collapsing to viewer
		// (ka-0000001646). normalizeAvaGroups is a no-op for bare-name groups
		// (Keycloak shape), so this is safe for any trust provider.
		roles := applyAvaDefaultRole(normalizeAvaGroups(claims.effectiveGroups()), defaultRole)
		ctx = context.WithValue(ctx, ctxRoles, roles)
		finalNext.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Middleware is a thin wrapper around New for callers that do not need the
// readiness handle. Prefer New so /readyz can be gated on OIDC readiness.
//
// Because the returned closure goes through the Handle's request entrypoint,
// the background discovery swap (fail-closed 503 -> verifying) still takes
// effect on live traffic for callers that use this helper.
func Middleware(ctx context.Context, cfg Config, logger *slog.Logger) func(http.Handler) http.Handler {
	return New(ctx, cfg, logger).Middleware
}

func verifyingMiddleware(verifier *oidc.IDTokenVerifier, cfg Config, logger *slog.Logger) func(http.Handler) http.Handler {
	requireEmailVerified := requiresEmailVerifiedClaim(cfg)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Also accept tokens from X-Auth-Request-Access-Token (set by ForwardAuth).
			rawToken := extractToken(r)
			if rawToken == "" {
				http.Error(w, "missing authorization token", http.StatusUnauthorized)
				return
			}

			idToken, err := verifier.Verify(r.Context(), rawToken)
			if err != nil {
				logger.Debug("OIDC token verification failed", "error", err)
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}

			var claims oidcClaims
			if err := idToken.Claims(&claims); err != nil {
				logger.Debug("failed to parse token claims", "error", err)
				http.Error(w, "invalid token claims", http.StatusUnauthorized)
				return
			}

			// The email is the identity every downstream surface keys on, so a
			// token naming an email its issuer does not vouch for is refused
			// outright (401), never downgraded to an email-less identity
			// (gd-ddpTGbl4Te, mirrors kbeads gd-rnklckVFKS).
			if err := claims.checkEmailVerified(requireEmailVerified); err != nil {
				logger.Debug("OIDC email not verified", "error", err)
				http.Error(w, "email not verified", http.StatusUnauthorized)
				return
			}

			roles := claims.effectiveRoles()
			if cfg.RequiredRole != "" && !HasMinimumRole(roles, cfg.RequiredRole) {
				http.Error(w, "insufficient permissions", http.StatusForbidden)
				return
			}

			if cfg.ExpectedTenantID != "" && claims.TenantID != cfg.ExpectedTenantID {
				logger.Debug("OIDC tenant not allowed", "tenant_id", claims.TenantID)
				http.Error(w, "tenant not allowed", http.StatusForbidden)
				return
			}

			if !containsAllScopes(claims.Scope, cfg.RequiredScopes) {
				logger.Debug("OIDC scope not allowed", "scope", claims.Scope)
				http.Error(w, "insufficient scope", http.StatusForbidden)
				return
			}

			if !authorizedPartyAllowed(claims.AuthorizedParty, cfg.AllowedAuthorizedParties) {
				logger.Debug("OIDC authorized party not allowed", "authorized_party", claims.AuthorizedParty)
				http.Error(w, "authorized party not allowed", http.StatusForbidden)
				return
			}

			email := claims.effectiveEmail()
			if len(cfg.AllowedEmailDomains) > 0 && !isAllowedEmailDomain(email, cfg.AllowedEmailDomains) {
				logger.Debug("email domain not allowed", "email", email)
				http.Error(w, "email domain not allowed", http.StatusForbidden)
				return
			}

			// Inject email and roles into context for downstream handlers.
			ctx := r.Context()
			ctx = context.WithValue(ctx, ctxEmail, email)
			ctx = context.WithValue(ctx, ctxRoles, roles)
			ctx = context.WithValue(ctx, ctxGroups, claims.Groups)
			ctx = context.WithValue(ctx, ctxOIDCVerified, true)
			if id := claims.gitHubID(); id > 0 {
				ctx = context.WithValue(ctx, ctxGitHubID, id)
			}
			if login := claims.gitHubLogin(); login != "" {
				ctx = context.WithValue(ctx, ctxGitHubLogin, login)
			}
			if claims.Subject != "" {
				ctx = context.WithValue(ctx, ctxSubject, claims.Subject)
			}
			ctx = context.WithValue(ctx, ctxAudiences, idToken.Audience)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// oidcClaims accepts both the Entra app-role claim and the legacy Keycloak
// realm-role shape. A pointer distinguishes an absent top-level roles claim
// from an explicitly present empty claim, so legacy roles are only a fallback
// for tokens that do not carry the Entra claim at all.
type oidcClaims struct {
	Subject           string            `json:"sub"`
	Email             string            `json:"email"`
	PreferredUsername string            `json:"preferred_username"`
	Roles             *[]string         `json:"roles"`
	RealmAccess       realmAccessClaims `json:"realm_access"`
	Groups            []string          `json:"groups"`
	TenantID          string            `json:"tid"`
	Scope             string            `json:"scp"`
	AuthorizedParty   string            `json:"azp"`
	// EmailVerified is kept raw: Keycloak and GitLab send a JSON bool, some
	// issuers the string "true", and Entra never sends it.
	EmailVerified json.RawMessage `json:"email_verified"`
	// GitHubID is a Keycloak user attribute mapped into the token, so it can
	// arrive as a JSON number or a string depending on the mapper's type.
	GitHubID json.RawMessage `json:"github_id"`
	// GitHubLogin is kept raw so a mistyped claim reads as no login instead
	// of failing the whole token.
	GitHubLogin json.RawMessage `json:"github_login"`
}

type realmAccessClaims struct {
	Roles []string `json:"roles"`
}

// gitHubID parses the github_id claim, a positive integer as a number or a
// string. Anything else is no GitHub id (0).
func (c oidcClaims) gitHubID() int64 {
	raw := strings.Trim(strings.TrimSpace(string(c.GitHubID)), `"`)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// gitHubLoginPattern is GitHub's username rule: alphanumerics or single
// hyphens, not starting or ending with a hyphen. The 39-character cap is
// checked separately.
var gitHubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:-?[A-Za-z0-9])*$`)

// gitHubLogin returns the github_login claim when it is a valid GitHub
// username, else "".
func (c oidcClaims) gitHubLogin() string {
	var login string
	if json.Unmarshal(c.GitHubLogin, &login) != nil {
		return ""
	}
	login = strings.TrimSpace(login)
	if len(login) > 39 || !gitHubLoginPattern.MatchString(login) {
		return ""
	}
	return login
}

func (c oidcClaims) effectiveRoles() []string {
	if c.Roles != nil {
		return *c.Roles
	}
	return c.RealmAccess.Roles
}

func (c oidcClaims) effectiveEmail() string {
	if c.Email != "" {
		return c.Email
	}
	return c.PreferredUsername
}

// emailVerified reports whether the token carries an email_verified claim
// and whether that claim asserts true (JSON true, or the string "true").
// An explicit null counts as present and not true.
func (c oidcClaims) emailVerified() (verified, present bool) {
	return EmailVerifiedClaim(c.EmailVerified)
}

// EmailVerifiedClaim parses a raw email_verified claim: verified is true only
// for JSON true or the string "true"; present is false only when the claim is
// absent. Exported so other ID-token readers (the bridge Slack link flow)
// apply the same rule.
func EmailVerifiedClaim(raw json.RawMessage) (verified, present bool) {
	if len(raw) == 0 {
		return false, false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, true
	}
	switch ev := v.(type) {
	case bool:
		return ev, true
	case string:
		return strings.EqualFold(ev, "true"), true
	}
	return false, true
}

// checkEmailVerified refuses a token whose email the issuer does not vouch
// for. Any token with email_verified present and not true is refused. When
// required (every issuer except Entra, see requiresEmailVerifiedClaim) the
// claim must also be present, and an email-shaped preferred_username cannot
// stand in for a missing email claim: it is the same unverified address,
// minus the claim that would say so. A username with no "@" (a Keycloak
// service-account token) still falls back as before.
func (c oidcClaims) checkEmailVerified(required bool) error {
	verified, present := c.emailVerified()
	if present && !verified {
		return errors.New("email_verified is not true")
	}
	if !required {
		return nil
	}
	if c.Email != "" && !present {
		return errors.New("email_verified claim absent")
	}
	if c.Email == "" && strings.Contains(c.PreferredUsername, "@") {
		return errors.New("email-shaped preferred_username without a verified email claim")
	}
	return nil
}

// entraIssuerHosts are the Microsoft identity platform token issuers
// (public cloud v2 and v1, US Government, China).
var entraIssuerHosts = map[string]bool{
	"login.microsoftonline.com":        true,
	"sts.windows.net":                  true,
	"login.microsoftonline.us":         true,
	"login.partner.microsoftonline.cn": true,
	"login.chinacloudapi.cn":           true,
}

// requiresEmailVerifiedClaim decides, per configured issuer, whether a token
// with an email claim must also carry email_verified=true. Entra never sends
// email_verified, so an Entra issuer only gets the weaker rule (refuse when it
// is present and not true). An issuer is Entra when its host is a Microsoft
// identity platform host, or when the config pins a tenant (ExpectedTenantID):
// only Entra tokens carry tid, so a pinned tenant already refuses every other
// issuer's tokens. Every other issuer (Keycloak, GitLab, anything unknown)
// must send email_verified: fail closed.
func requiresEmailVerifiedClaim(cfg Config) bool {
	if cfg.ExpectedTenantID != "" {
		return false
	}
	u, err := url.Parse(cfg.IssuerURL)
	if err != nil {
		return true
	}
	return !entraIssuerHosts[strings.ToLower(u.Hostname())]
}

func containsAllScopes(scopeClaim string, required []string) bool {
	if len(required) == 0 {
		return true
	}
	granted := make(map[string]struct{})
	for _, scope := range strings.Fields(scopeClaim) {
		granted[scope] = struct{}{}
	}
	for _, scope := range required {
		if _, ok := granted[scope]; !ok {
			return false
		}
	}
	return true
}

func authorizedPartyAllowed(authorizedParty string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, clientID := range allowed {
		if authorizedParty == clientID {
			return true
		}
	}
	return false
}

// retryPolicy is an immutable snapshot of the package-level retry tunables.
// New snapshots it synchronously before launching the background discovery
// goroutine so the goroutine never reads the mutable globals (which tests
// rewrite via t.Cleanup) — avoiding a data race between discovery and test
// teardown.
type retryPolicy struct {
	attempts     int
	initialDelay time.Duration
	maxDelay     time.Duration
}

// currentRetryPolicy snapshots the package-level retry tunables. Call it from
// the goroutine that owns the tunables' lifecycle (the caller of New), never
// from the background discovery goroutine.
func currentRetryPolicy() retryPolicy {
	return retryPolicy{
		attempts:     providerRetryAttempts,
		initialDelay: providerRetryInitialDelay,
		maxDelay:     providerRetryMaxDelay,
	}
}

// newProviderWithRetry wraps oidc.NewProvider with an exponential backoff
// loop so that transient IdP unavailability (DNS flap, Keycloak restart) does
// not force a fail-closed state. When policy.attempts <= 0 it retries
// INDEFINITELY until ctx is cancelled (the production default — discovery runs
// in a background goroutine, see New). When policy.attempts > 0 it is bounded
// to that many attempts and returns the last error if all fail.
//
// ctx cancellation is honored both while sleeping between attempts AND is
// passed to oidc.NewProvider itself, so a SIGTERM-cancelled context interrupts
// an in-flight discovery HTTP call rather than blocking shutdown.
func newProviderWithRetry(ctx context.Context, issuer string, policy retryPolicy, logger *slog.Logger) (*oidc.Provider, error) {
	indefinite := policy.attempts <= 0
	var lastErr error
	delay := policy.initialDelay
	for attempt := 1; indefinite || attempt <= policy.attempts; attempt++ {
		provider, err := oidc.NewProvider(ctx, issuer)
		if err == nil {
			return provider, nil
		}
		lastErr = err
		maxAttempts := "indefinite"
		if !indefinite {
			maxAttempts = strconv.Itoa(policy.attempts)
		}
		logger.Warn("OIDC provider init failed", "attempt", attempt, "max_attempts", maxAttempts, "issuer", issuer, "next_delay", delay, "error", err)
		if !indefinite && attempt == policy.attempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < policy.maxDelay {
			delay *= 2
			if delay > policy.maxDelay {
				delay = policy.maxDelay
			}
		}
	}
	return nil, lastErr
}

// failClosedMiddleware rejects every request with 503. Used when OIDC
// provider discovery failed after retries; the alternative (passthrough)
// would expose protected endpoints unauthenticated.
func failClosedMiddleware(_ http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "authentication provider unavailable", http.StatusServiceUnavailable)
	})
}

// extractToken gets the Bearer token from the Authorization header,
// or falls back to the X-Auth-Request-Access-Token header (set by oauth2-proxy ForwardAuth).
func extractToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		scheme, token, ok := strings.Cut(auth, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}
	}
	return r.Header.Get("X-Auth-Request-Access-Token")
}

// Keycloak realm role constants for gardenio RBAC.
const (
	RoleViewer   = "gardenio-viewer"
	RoleOperator = "gardenio-operator"
	RoleAdmin    = "gardenio-admin"
	// RoleProjectMember marks a human whose read scope is the projects in their
	// token groups. It is deliberately outside roleRank: it grants no rank, so
	// HasMinimumRole never admits it to a viewer/operator/admin gate. Mirrors
	// kbeads server.RoleProjectMember.
	RoleProjectMember = "gardenio-project-member"
)

// roleRank maps each gardenio role to its hierarchy rank.
// Higher rank implies all permissions of lower ranks.
var roleRank = map[string]int{
	RoleViewer:   1,
	RoleOperator: 2,
	RoleAdmin:    3,
}

// HasMinimumRole checks whether any of the user's roles meet or exceed the
// required role in the gardenio hierarchy (admin > operator > viewer).
func HasMinimumRole(roles []string, required string) bool {
	requiredRank := roleRank[required]
	if requiredRank == 0 {
		// Preserve legacy Keycloak/OIDC consumers that configure an arbitrary
		// realm role outside the Gardenio hierarchy.
		for _, role := range roles {
			if role == required {
				return true
			}
		}
		return false
	}
	for _, r := range roles {
		if roleRank[r] >= requiredRank {
			return true
		}
	}
	return false
}

// RequireRole returns middleware that rejects requests whose context roles
// do not meet the minimum required role. When no roles are present in
// context (e.g. OIDC not configured or internal caller), the request is
// allowed through — RBAC is only enforced for OIDC-authenticated users.
func RequireRole(required string, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			roles := RolesFromContext(r.Context())
			if len(roles) == 0 {
				// No OIDC roles in context — allow (internal/unauthenticated caller).
				next.ServeHTTP(w, r)
				return
			}
			if !HasMinimumRole(roles, required) {
				logger.Debug("RBAC denied", "path", r.URL.Path, "required", required, "roles", roles)
				http.Error(w, "insufficient permissions", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isAllowedEmailDomain(email string, allowed []string) bool {
	_, domain, ok := strings.Cut(email, "@")
	if !ok {
		return false
	}
	domain = strings.ToLower(domain)
	for _, d := range allowed {
		if strings.ToLower(d) == domain {
			return true
		}
	}
	return false
}
