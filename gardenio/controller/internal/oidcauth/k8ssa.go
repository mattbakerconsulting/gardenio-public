package oidcauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// saSubjectPrefix is the fixed prefix every Kubernetes ServiceAccount token
// carries in its verified `sub` claim: system:serviceaccount:<namespace>:<name>.
const saSubjectPrefix = "system:serviceaccount:"

// forbiddenSAAudiences are audiences a projected ServiceAccount token MUST NOT
// be accepted with — the default apiserver audience and the IRSA/STS audience.
// Both are live on gardenio agent pods today (a pod's default projected token
// has aud=["https://kubernetes.default.svc"], and IRSA web-identity tokens
// carry aud=["sts.amazonaws.com"]), so accepting either would let a repurposed
// infra token authenticate to the viewer. The pinned-audience check in the
// verifier already rejects them (they do not carry our dedicated audience), and
// Validate refuses configuring one of these as the pinned audience — this list
// backstops both as defense in depth.
var forbiddenSAAudiences = []string{
	"https://kubernetes.default.svc",
	"sts.amazonaws.com",
}

func isForbiddenSAAudience(aud string) bool {
	return slices.Contains(forbiddenSAAudiences, aud)
}

// K8sSAConfig configures the EKS ServiceAccount-token branch of the oidcauth
// middleware (ka-0000006288). The branch is OPT-IN: when IssuerURL is empty it
// is disabled and the middleware behaves exactly as before (byte-identical for
// an unconfigured deploy).
//
// It teaches beads-viewer a SECOND verified machine-token issuer — the EKS OIDC
// issuer for a cluster — so new-prod audit pods can present an audience-pinned
// projected ServiceAccount token instead of a human Entra session. A verified
// `sub` (system:serviceaccount:<ns>:<sa>) on the allow-list is mapped to a
// synthetic, domain-valid email plus a fixed role floor, so the request clears
// the same RequiredRole / AllowedEmailDomains gates a human would — WITHOUT
// relaxing those global gates for the human Entra path.
//
// The verification is real: JWKS discovery + issuer + audience + expiry +
// signature, via the same go-oidc primitives the Keycloak/Entra path uses. It
// is NOT a trusted header.
type K8sSAConfig struct {
	// IssuerURL is the EKS OIDC issuer, e.g.
	// https://oidc.eks.us-east-1.amazonaws.com/id/<cluster-id>. Setting it
	// enables the branch. Discovery (/.well-known/openid-configuration + JWKS)
	// runs in the background exactly like the Keycloak path; until it succeeds
	// SA-routed requests fail closed with 503.
	IssuerURL string

	// Audience is the dedicated audience the projected SA token MUST be minted
	// with (the go-oidc ClientID check). It gets its OWN field rather than
	// overloading Config.ClientID because one verifier pins exactly one
	// audience and the EKS audience differs from the Entra client id. MUST NOT
	// be the apiserver or IRSA audience (see forbiddenSAAudiences); Validate
	// enforces this.
	Audience string

	// AllowedSubjects is the explicit allow-list of verified `sub` values
	// (system:serviceaccount:<ns>:<sa>) permitted to authenticate. `sub` is the
	// only identity claim a projected SA token carries, so this is the identity
	// gate — an empty list disables the branch's usefulness and is rejected by
	// Validate. NEVER accept any-SA-in-the-cluster.
	AllowedSubjects []string

	// SyntheticEmailDomain is the domain used to build the synthetic,
	// domain-valid email (<ns>.<sa>@<domain>) that a verified subject maps to.
	// It MUST be one of the viewer's AllowedEmailDomains so the machine
	// identity clears the same domain gate as a human (Validate cross-checks
	// this). The provisioned operators(kind='oidc') row + oidc:<email> alias
	// must use the SAME email string this produces, or the viewer resolver
	// returns not-found.
	SyntheticEmailDomain string

	// RoleFloor is the gardenio role granted to a verified SA identity, mirroring
	// applyAvaDefaultRole's floor for AVA. Typically gardenio-viewer. A projected
	// SA token carries no roles claim, so without a floor it would 403 on every
	// RequiredRole gate. Required (non-empty) when the branch is enabled.
	RoleFloor string
}

func (c K8sSAConfig) enabled() bool {
	return c.IssuerURL != ""
}

// subjectAllowed reports whether the verified subject is on the explicit
// allow-list. Exact match only — there is no wildcard, by design.
func (c K8sSAConfig) subjectAllowed(sub string) bool {
	return slices.Contains(c.AllowedSubjects, sub)
}

// syntheticSAEmail maps a verified ServiceAccount subject
// (system:serviceaccount:<ns>:<sa>) to a deterministic, domain-valid email
// local-part <ns>.<sa>@<domain>. Kubernetes namespaces are DNS-1123 labels
// (no dots), so the first dot always separates namespace from ServiceAccount
// name and the mapping is unambiguous across the (allow-list-bounded) set of
// accepted subjects. Returns ok=false when sub is not a well-formed
// serviceaccount subject, so a token whose verified sub is some other shape is
// refused rather than mapped to a malformed address.
func syntheticSAEmail(sub, domain string) (string, bool) {
	rest, ok := strings.CutPrefix(sub, saSubjectPrefix)
	if !ok {
		return "", false
	}
	ns, sa, ok := strings.Cut(rest, ":")
	if !ok || ns == "" || sa == "" {
		return "", false
	}
	return ns + "." + sa + "@" + domain, true
}

// unverifiedIssuer decodes — WITHOUT verifying — the `iss` claim of a compact
// JWT, for ROUTING ONLY. A projected SA token and an Entra token both arrive on
// the same Authorization: Bearer header, so the middleware peeks at the
// unverified issuer to decide which verifier to hand the token to. This peek
// grants NO trust: the SA path fully verifies the token (JWKS + issuer +
// audience + expiry + signature) before reading any claim, and a token that
// selected the SA path but fails verification is a hard 401 with no fallthrough
// — a forged `iss` cannot downgrade a token onto a different, weaker path.
// Returns "" for anything that is not a well-formed three-segment JWT.
func unverifiedIssuer(rawToken string) string {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Iss
}

// UnverifiedIssuer returns the iss claim from a compact JWT without verifying
// its signature. It is only suitable for selecting which configured verifier
// must claim a bearer token. The selected verifier must still validate the
// signature, issuer, audience, and expiry, and a claimed token must never fall
// through to another authentication path when verification fails.
func UnverifiedIssuer(rawToken string) string {
	return unverifiedIssuer(rawToken)
}

// loadSAWrap returns the current SA middleware factory (503 fail-closed until
// background discovery succeeds, then the verifier).
func (h *Handle) loadSAWrap() func(http.Handler) http.Handler {
	return *h.saWrap.Load()
}

// discoverSA runs OIDC provider discovery for the EKS SA-token issuer in the
// background and, on success, atomically swaps the SA middleware from
// fail-closed (503) to the verifying middleware. It mirrors discoverKeycloak.
// If ctx is cancelled before discovery succeeds, the SA branch stays
// fail-closed. When gatesReady is true (SA is the ONLY configured auth path)
// it also flips the handle ready on success; otherwise readiness is owned by
// the AVA/Keycloak branch and this leaves it untouched.
func (h *Handle) discoverSA(ctx context.Context, cfg K8sSAConfig, policy retryPolicy, gatesReady bool, logger *slog.Logger) {
	provider, err := newProviderWithRetry(ctx, cfg.IssuerURL, policy, logger)
	if err != nil {
		logger.Error("EKS SA-token OIDC provider discovery stopped before success — SA branch stays fail closed (503)", "issuer", cfg.IssuerURL, "error", err)
		return
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.Audience})
	wrap := saVerifyingMiddleware(verifier, cfg, logger)
	h.saWrap.Store(&wrap)
	if gatesReady {
		h.ready.Store(true)
	}
	// Store saReady LAST so any reader that observes saDiscovered()==true is
	// guaranteed to also see the swapped-in verifier.
	h.saReady.Store(true)
	logger.Info("EKS SA-token OIDC provider discovery succeeded — SA branch now verifying", "issuer", cfg.IssuerURL)
}

// saVerifyingMiddleware verifies an EKS projected ServiceAccount token and, on
// success, injects a synthetic domain-valid email + the configured role floor
// into the request context — exactly the two context values verifyingMiddleware
// (Keycloak) and authBranch (AVA) inject, so the viewer.Resolver and every
// EmailFromContext / RolesFromContext call site downstream work unchanged.
//
// It is reached ONLY for a bearer token whose unverified issuer matched the
// configured SA issuer. Every failure is a hard 401/403 with NO fallthrough to
// the Keycloak/AVA chain: a token that claimed to be an EKS token but fails
// verification does not get a second chance on a different path.
func saVerifyingMiddleware(verifier *oidc.IDTokenVerifier, cfg K8sSAConfig, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawToken := extractToken(r)
			if rawToken == "" {
				http.Error(w, "missing authorization token", http.StatusUnauthorized)
				return
			}

			// Real verification: signature against the discovered JWKS, issuer
			// equals the discovered EKS issuer, audience contains cfg.Audience,
			// and the token is unexpired.
			idToken, err := verifier.Verify(r.Context(), rawToken)
			if err != nil {
				logger.Debug("EKS SA token verification failed", "error", err)
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}

			// Defense in depth: refuse a token that ALSO carries the apiserver
			// or IRSA audience even if it somehow also carries our pinned
			// audience. A correctly-minted audit token has only the dedicated
			// audience, so this never trips for a legitimate caller.
			for _, aud := range idToken.Audience {
				if isForbiddenSAAudience(aud) {
					logger.Debug("EKS SA token carries forbidden audience", "audience", aud)
					http.Error(w, "invalid token audience", http.StatusUnauthorized)
					return
				}
			}

			// `sub` is the only identity claim a projected SA token carries.
			// Gate it against the explicit allow-list; never accept an
			// arbitrary in-cluster ServiceAccount.
			if !cfg.subjectAllowed(idToken.Subject) {
				logger.Debug("EKS SA token subject not allow-listed", "subject", idToken.Subject)
				http.Error(w, "subject not allowed", http.StatusForbidden)
				return
			}

			email, ok := syntheticSAEmail(idToken.Subject, cfg.SyntheticEmailDomain)
			if !ok {
				logger.Debug("EKS SA token subject is not a serviceaccount subject", "subject", idToken.Subject)
				http.Error(w, "invalid subject", http.StatusForbidden)
				return
			}

			ctx := r.Context()
			ctx = context.WithValue(ctx, ctxEmail, email)
			ctx = context.WithValue(ctx, ctxRoles, []string{cfg.RoleFloor})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
