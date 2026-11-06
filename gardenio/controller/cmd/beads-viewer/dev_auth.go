package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"gardenio/controller/internal/oidcauth"
)

// devAuthBypassEnv is the opt-in flag that replaces the OIDC middleware with a
// passthrough so beads-viewer can be run locally against a port-forwarded
// beads daemon without standing up Keycloak.
const devAuthBypassEnv = "BEADS_VIEWER_DEV_AUTH_BYPASS"

// devAuthProxySecretEnv carries the shared secret the trusted forwardauth edge
// (nginx viewer-authproxy) stamps in devAuthProxySecretHeader. When set, the
// dev-bypass path trusts X-Auth-Request-Email ONLY on requests that present the
// matching secret (gd-kRyMsWiDNZ). Unset keeps the legacy trust-the-header
// behaviour for local dev and the Traefik/oauth2-proxy box, where the edge is
// the only route to the pod.
const devAuthProxySecretEnv = "BEADS_VIEWER_DEV_AUTH_PROXY_SECRET"

// devAuthProxySecretHeader is the request header the edge uses to prove it is
// the edge. The viewer removes it before handlers run.
const devAuthProxySecretHeader = "X-Authproxy-Secret"

// The BEADS_VIEWER_DEV_AUTH_BEARER_* variables name the Keycloak realm whose
// access tokens the edge forwards as Authorization (gd-BmYmugDc4x): the issuer
// (required), an optional in-cluster discovery URL and CA file, and the
// audience (required). They are deliberately not OIDC_ISSUER_URL/OIDC_CLIENT_ID:
// those turn on the OIDC middleware, which the bypass replaces.
const (
	devAuthBearerIssuerURLEnv    = "BEADS_VIEWER_DEV_AUTH_BEARER_ISSUER_URL"
	devAuthBearerDiscoveryURLEnv = "BEADS_VIEWER_DEV_AUTH_BEARER_DISCOVERY_URL"
	devAuthBearerCAFileEnv       = "BEADS_VIEWER_DEV_AUTH_BEARER_CA_FILE"
	devAuthBearerAudienceEnv     = "BEADS_VIEWER_DEV_AUTH_BEARER_AUDIENCE"
)

// devAuthBypassAckAgentPodEnv is a second opt-in flag that downgrades the
// prod-env (SA-token) guard from refuse-to-start to allow-with-loopback-only.
// It exists so audit/automation formulas (ka-0000006568) can run
// beads-viewer inside an agent pod, which always has a SA token mounted.
// Setting this also forces LISTEN_ADDR to a loopback interface so the bypass
// surface can never accidentally listen on the pod network.
const devAuthBypassAckAgentPodEnv = "BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD"

// k8sServiceAccountTokenPath is the default location K8s mounts the pod's
// ServiceAccount token. Its presence is our prod-like-env signal — if a pod
// has a SA token mounted we refuse the bypass unless the ACK env above is set.
const k8sServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// errBypassInProdLikeEnv is returned when BEADS_VIEWER_DEV_AUTH_BYPASS=1 is
// set on a pod that looks like it is running in a real cluster and the ACK
// env is not set.
var errBypassInProdLikeEnv = errors.New("BEADS_VIEWER_DEV_AUTH_BYPASS refused: K8s service account token present — this looks like a prod-like environment (set BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD=1 to opt in from an agent pod; LISTEN_ADDR will be forced to loopback)")

// devAuthBypassEnabled reports whether the caller explicitly opted into the
// local-dev OIDC bypass. Accepts "1", "true", "yes" (case-insensitive).
func devAuthBypassEnabled() bool {
	return parseBoolEnv(os.Getenv(devAuthBypassEnv))
}

// devAuthBypassAckAgentPod reports whether the caller has acknowledged the
// agent-pod opt-out. When true, the SA-token guard is downgraded but
// LISTEN_ADDR is forced to a loopback interface.
func devAuthBypassAckAgentPod() bool {
	return parseBoolEnv(os.Getenv(devAuthBypassAckAgentPodEnv))
}

func parseBoolEnv(v string) bool {
	switch v {
	case "1", "true", "True", "TRUE", "yes", "Yes", "YES":
		return true
	default:
		return false
	}
}

// checkDevAuthBypassAllowed returns nil if the process is allowed to run with
// the bypass active, or an error if it looks like we are in a prod-like
// environment (K8s service account token mounted) AND the agent-pod ACK env
// is not set.
//
// The check is intentionally conservative: any file at the canonical SA token
// path trips it unless the operator has set BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD.
// Callers can override file detection via fileExists for tests.
func checkDevAuthBypassAllowed(fileExists func(string) bool) error {
	if !fileExists(k8sServiceAccountTokenPath) {
		return nil
	}
	if devAuthBypassAckAgentPod() {
		return nil
	}
	return errBypassInProdLikeEnv
}

// fileExists is the production probe used by checkDevAuthBypassAllowed.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// forceLoopbackListenAddr returns the listen address rewritten to bind only
// loopback. A bind-all host ("", "0.0.0.0", "::") is rewritten to 127.0.0.1.
// An explicit loopback host is returned unchanged. Any other host is rejected.
//
// This is called only when the agent-pod ACK is honored: it guarantees the
// bypass surface can never be reached over the pod network, matching the
// documented local-dev path.
func forceLoopbackListenAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid LISTEN_ADDR %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		return "127.0.0.1:" + port, nil
	}
	if host == "localhost" {
		return addr, nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return addr, nil
	}
	return "", fmt.Errorf("LISTEN_ADDR %q must be a loopback address when %s is set", addr, devAuthBypassAckAgentPodEnv)
}

// cookieSecure reports whether a viewer-set cookie should carry the Secure
// flag (ka-0000004498, source ka-0000002836 §5.A). In production TLS
// terminates at Traefik/AVA so r.TLS is nil at the pod; we treat
// X-Forwarded-Proto: https from the trusted ingress as TLS. The default is
// fail-secure (Secure=true) so cookies never accidentally cross a non-TLS hop;
// only the local dev-auth bypass over plain HTTP downgrades to Secure=false
// so the browser actually accepts the cookie.
func cookieSecure(r *http.Request) bool {
	if r != nil {
		if r.TLS != nil {
			return true
		}
		if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			return true
		}
	}
	return !devAuthBypassEnabled()
}

// devBypassMiddleware returns the auth middleware used when OIDC is DISABLED at
// the pod (BEADS_VIEWER_DEV_AUTH_BYPASS=1). It logs a loud WARN on every
// request's first call so bypass mode cannot be used without leaving a trail in
// the server logs, and it establishes the request's verified identity from the
// edge-stamped X-Auth-Request-Email header (see forwardAuthTrustedEmailToContext).
//
// This is the ONLY place in the viewer that trusts the raw X-Auth-Request-Email
// header, and it is deliberately confined to the dev-bypass path. Dev-bypass
// means "the pod does not authenticate; a trusted forwardauth edge does"
// (Gardenio-in-a-Box: the in-cluster oauth2-proxy ForwardAuth middleware
// OVERWRITES X-Auth-Request-Email from its verified auth response — Traefik
// authResponseHeaders — so a client-forged value never survives to the pod).
// It is mutually exclusive with the AVA/Keycloak path (main.go wires exactly one
// of devBypassMiddleware OR oidcauth.New().Middleware), so on the AVA edge —
// where the header IS client-supplied and spoofable — this injection never runs
// and handlers see only the AVA-verified ctx email (ka-0000002979).
//
// proxySecret, when non-empty, closes the "any pod can reach the viewer
// Service directly" hole (gd-kRyMsWiDNZ): the header is promoted only if the
// request also carries devAuthProxySecretHeader equal to proxySecret, so a pod
// that bypasses the edge and forges X-Auth-Request-Email gets no identity (the
// same as sending no header). The secret header is always stripped.
//
// Such a request is also marked withNoEdgeProof, and scopeMiddleware lets it
// reach only the public routes and the routeMachine routes (gd-ozc7DSwCXO).
// With no identity it would otherwise be an unrestricted caller, and the
// shared hubs built on the viewer's system read (/garden/events, the feed hub,
// /index) would serve it every land's data.
//
// edgeBearer, when non-nil and proxySecret is set, attaches the signed-in
// user's realm roles and groups from the bearer the edge forwards (see
// attachEdgeBearerRoles, gd-BmYmugDc4x). Without a proxy secret there is no
// edge proof, so no roles are ever attached.
func devBypassMiddleware(logger *slog.Logger, proxySecret string, edgeBearer edgeBearerVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.Debug("dev auth bypass: request passthrough",
				"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			presented := r.Header.Get(devAuthProxySecretHeader)
			r.Header.Del(devAuthProxySecretHeader)
			if proxySecret != "" && !proxySecretMatches(presented, proxySecret) {
				if r.Header.Get("X-Auth-Request-Email") != "" {
					logger.Warn("dev auth bypass: X-Auth-Request-Email without a valid edge secret ignored",
						"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
				}
				next.ServeHTTP(w, r.WithContext(withNoEdgeProof(r.Context())))
				return
			}
			r = forwardAuthTrustedEmailToContext(r)
			if proxySecret != "" && edgeBearer != nil {
				r = attachEdgeBearerRoles(r, edgeBearer, logger)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// edgeBearerVerifier verifies the access token the edge forwards
// (*oidcauth.EdgeBearerVerifier).
type edgeBearerVerifier interface {
	Verify(ctx context.Context, raw string) (oidcauth.EdgeBearerIdentity, error)
}

// attachEdgeBearerRoles gives a dev-bypass request the realm roles and groups
// of the signed-in user (gd-BmYmugDc4x). The edge (viewer-authproxy) forwards
// the user's Keycloak access token as Authorization; it is the same token
// kbeads verifies for the user's writes. Without this the bypass path carries
// no roles at all, so every RolesFromContext gate (the Glass tiers among them)
// refuses everyone, the owner included.
//
// Only devBypassMiddleware calls it, and only once the edge secret matched:
// roles never come from a request that skipped the edge, from a header, or
// from an unverified token. The token must pass signature (issuer JWKS), iss,
// aud and exp checks and carry a verified email equal to the edge's
// X-Auth-Request-Email, which stays the request's identity either way. Any
// failure leaves r as it was: the edge's email and no roles, today's
// behaviour. The OIDC-verified mark (applicant detection) is not set: that
// stays the OIDC middleware's alone.
func attachEdgeBearerRoles(r *http.Request, v edgeBearerVerifier, logger *slog.Logger) *http.Request {
	ctx := r.Context()
	email := oidcauth.EmailFromContext(ctx)
	if email == "" {
		return r
	}
	raw := oidcauth.AccessTokenFromRequest(r)
	if raw == "" {
		return r
	}
	id, err := v.Verify(ctx, raw)
	if err != nil {
		if !errors.Is(err, oidcauth.ErrEdgeBearerNotReady) {
			logger.Info("dev auth bypass: edge bearer failed verification; no roles attached",
				"method", r.Method, "path", r.URL.Path, "error", err)
		}
		return r
	}
	if !strings.EqualFold(id.Email, email) {
		logger.Warn("dev auth bypass: edge bearer email differs from X-Auth-Request-Email; no roles attached",
			"method", r.Method, "path", r.URL.Path, "edge_email", email, "token_email", id.Email)
		return r
	}
	ctx = oidcauth.ContextWithRoles(ctx, id.Roles)
	ctx = oidcauth.ContextWithGroups(ctx, id.Groups)
	if id.Impersonator != "" {
		// an impersonated session (one-click Become): /garden draws its banner from this, the token's claim alone
		ctx = oidcauth.ContextWithImpersonator(ctx, id.Impersonator)
	}
	// the GitHub account a Lands applicant signed in with, and their stable
	// sub: /lands/github needs the first, /lands/apply and /lands/applications
	// both, and refuse a request without them as "did not come with a GitHub
	// sign-in"
	if id.Subject != "" {
		ctx = oidcauth.ContextWithSubject(ctx, id.Subject)
	}
	if id.GitHubID > 0 {
		ctx = oidcauth.ContextWithGitHubID(ctx, id.GitHubID)
	}
	if id.GitHubLogin != "" {
		ctx = oidcauth.ContextWithGitHubLogin(ctx, id.GitHubLogin)
	}
	return r.WithContext(ctx)
}

// newDevAuthEdgeBearer builds the dev-bypass edge bearer verifier from cfg, or
// returns nil (no roles are ever attached) when it is not configured, when
// there is no edge secret to prove a request came through the edge, or when
// the CA file cannot be read. None of these stops the viewer: the bypass then
// behaves as it did before gd-BmYmugDc4x.
func newDevAuthEdgeBearer(ctx context.Context, cfg *config, logger *slog.Logger) edgeBearerVerifier {
	bc := cfg.devAuthEdgeBearer
	if bc.IssuerURL == "" && bc.Audience == "" {
		return nil
	}
	if !bc.Enabled() {
		logger.Warn("dev auth bypass: edge bearer roles need both " + devAuthBearerIssuerURLEnv + " and " + devAuthBearerAudienceEnv + "; no roles are attached")
		return nil
	}
	if cfg.devAuthProxySecret == "" {
		logger.Warn("dev auth bypass: edge bearer roles need " + devAuthProxySecretEnv + " (no edge proof, no roles); no roles are attached")
		return nil
	}
	v, err := oidcauth.NewEdgeBearerVerifier(ctx, bc, logger)
	if err != nil {
		logger.Error("dev auth bypass: edge bearer verifier not started; no roles are attached", "error", err)
		return nil
	}
	logger.Info("dev auth bypass: edge bearer roles enabled", "issuer", bc.IssuerURL, "discovery_url", bc.DiscoveryURL, "audience", bc.Audience)
	return v
}

type noEdgeProofKey struct{}

// withNoEdgeProof marks ctx as a request that reached the viewer directly: the
// edge secret is configured and the request did not carry it.
func withNoEdgeProof(ctx context.Context) context.Context {
	return context.WithValue(ctx, noEdgeProofKey{}, true)
}

// noEdgeProofFromContext reports whether devBypassMiddleware marked ctx as a
// direct call with no edge proof.
func noEdgeProofFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(noEdgeProofKey{}).(bool)
	return v
}

// proxySecretMatches compares the presented edge secret to the configured one
// in constant time. An empty presented value never matches.
func proxySecretMatches(presented, want string) bool {
	if presented == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

// forwardAuthTrustedEmailToContext promotes the edge-stamped X-Auth-Request-Email
// header into the verified-identity context slot (oidcauth.EmailFromContext), so
// downstream handlers read identity exclusively from the context and never touch
// the raw header. Returns r unchanged when the header is absent or the context
// already carries a verified email (which the dev-bypass path never sets, but
// guarding keeps the helper composition-safe).
//
// Trust boundary: the caller — devBypassMiddleware — is what makes trusting the
// header sound. It runs only when OIDC is bypassed at the pod, i.e. only behind a
// forwardauth edge that overwrites this header from a verified auth response (or
// in local dev, where the operator controls their own machine). It MUST NOT be
// wired on any path where the header is client-supplied (the AVA edge); on that
// path the request is authenticated by oidcauth's AVA branch, which populates the
// context directly. Mirrors grafana_proxy.go / edge-auth: the header is only ever
// trusted where a proxy has already validated it.
func forwardAuthTrustedEmailToContext(r *http.Request) *http.Request {
	if oidcauth.EmailFromContext(r.Context()) != "" {
		return r
	}
	email := strings.TrimSpace(r.Header.Get("X-Auth-Request-Email"))
	if email == "" {
		return r
	}
	return r.WithContext(oidcauth.ContextWithEmail(r.Context(), email))
}
