package oidcauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"github.com/coreos/go-oidc/v3/oidc"
)

// EdgeBearerConfig configures EdgeBearerVerifier: the Keycloak realm whose
// access tokens a trusted forwardauth edge forwards as Authorization
// (viewer-authproxy, gd-epq9jTtIxV). It is the same issuer and audience kbeads
// verifies those tokens against (BEADS_OIDC_ISSUER_URL, BEADS_OIDC_DISCOVERY_URL,
// BEADS_OIDC_CA_FILE, BEADS_OIDC_AUDIENCE).
type EdgeBearerConfig struct {
	// IssuerURL is the realm issuer every token's iss must equal.
	IssuerURL string
	// DiscoveryURL, when set and different from IssuerURL, is where the
	// discovery document (and through it the JWKS) is fetched: the in-cluster
	// Service in front of a Keycloak whose issuer is its public hostname.
	DiscoveryURL string
	// CAFile is an optional PEM CA, added to the system roots, for the
	// discovery and JWKS fetches (the keycloak-runtime in-cluster CA).
	CAFile string
	// Audience is the client ID the token's aud must contain.
	Audience string
}

// Enabled reports whether the config names an issuer and an audience. Both
// are required; one without the other leaves the verifier off.
func (c EdgeBearerConfig) Enabled() bool {
	return c.IssuerURL != "" && c.Audience != ""
}

// EdgeBearerIdentity is what a verified edge bearer asserts.
type EdgeBearerIdentity struct {
	// Subject is the token's sub claim, the user's stable id (Lands apply
	// files and lists applications by it).
	Subject string
	Email   string
	Roles   []string
	Groups  []string
	// Impersonator is the admin acting behind an impersonated session (the
	// impersonator claim, one-click Become, plan gd-0SUIdKXWV3), or "".
	Impersonator string
	// GitHubID and GitHubLogin are the github_id / github_login claims of a
	// user who signed in through GitHub, or 0 and "" (Lands repository
	// connect and apply need them).
	GitHubID    int64
	GitHubLogin string
}

// ErrEdgeBearerNotReady is returned by Verify before discovery has succeeded.
var ErrEdgeBearerNotReady = errors.New("edge bearer verifier not ready: OIDC discovery has not succeeded")

// EdgeBearerVerifier verifies a Keycloak access token forwarded by a trusted
// forwardauth edge and reads the identity it asserts. It exists for a pod that
// does not run the OIDC middleware (beads-viewer's dev-bypass path, where the
// edge authenticates and the pod trusts its email header) but still needs the
// signed-in user's realm roles. It never gates a request: a token it cannot
// verify yields an error, and the caller simply attaches no roles.
//
// Discovery runs in the background with the same retry loop as New, so a
// briefly unreachable Keycloak never blocks startup; until it succeeds Verify
// returns ErrEdgeBearerNotReady.
type EdgeBearerVerifier struct {
	verifier atomic.Pointer[oidc.IDTokenVerifier]
}

// NewEdgeBearerVerifier starts discovery for cfg and returns the verifier.
// ctx bounds discovery (cancel it on shutdown). cfg must be Enabled.
func NewEdgeBearerVerifier(ctx context.Context, cfg EdgeBearerConfig, logger *slog.Logger) (*EdgeBearerVerifier, error) {
	if !cfg.Enabled() {
		return nil, errors.New("edge bearer verifier needs both an issuer URL and an audience")
	}
	discoveryCtx, err := edgeBearerCAContext(ctx, cfg.CAFile)
	if err != nil {
		return nil, err
	}
	fetchURL := cfg.IssuerURL
	if cfg.DiscoveryURL != "" && cfg.DiscoveryURL != cfg.IssuerURL {
		// go-oidc still requires the document's issuer, and every token's
		// iss, to equal IssuerURL; only the fetch goes to DiscoveryURL.
		fetchURL = cfg.DiscoveryURL
		discoveryCtx = oidc.InsecureIssuerURLContext(discoveryCtx, cfg.IssuerURL)
	}
	v := &EdgeBearerVerifier{}
	policy := currentRetryPolicy()
	go func() {
		provider, err := newProviderWithRetry(discoveryCtx, fetchURL, policy, logger)
		if err != nil {
			logger.Error("edge bearer OIDC discovery stopped before success; no roles are attached from edge bearers", "issuer", cfg.IssuerURL, "error", err)
			return
		}
		v.verifier.Store(provider.Verifier(&oidc.Config{ClientID: cfg.Audience}))
		logger.Info("edge bearer OIDC discovery succeeded; verified edge bearers now carry realm roles", "issuer", cfg.IssuerURL, "audience", cfg.Audience)
	}()
	return v, nil
}

// Ready reports whether discovery has succeeded.
func (v *EdgeBearerVerifier) Ready() bool {
	return v != nil && v.verifier.Load() != nil
}

// edgeBearerClaims is the Keycloak access-token shape. Roles come from both
// claims Keycloak may carry them in, merged as kbeads' ExtractRoles does:
// realm_access.roles (the built-in roles scope, access token only) and the
// chart's flat realm_roles mapper.
type edgeBearerClaims struct {
	oidcClaims
	RealmRoles []string `json:"realm_roles"`
	// Impersonator is the IMPERSONATOR_USERNAME session note the chart's
	// impersonator mapper puts in a token Keycloak mints for an impersonated
	// session (gd-rbML2txvGG); absent on any other session.
	Impersonator string `json:"impersonator"`
}

// Verify checks raw's signature against the issuer's JWKS, its iss, aud and
// exp, and that the issuer vouches for its email (email_verified true, the
// rule kbeads and the Keycloak OIDC path apply). It returns the token's sub, email,
// realm roles, groups, impersonator and GitHub account.
func (v *EdgeBearerVerifier) Verify(ctx context.Context, raw string) (EdgeBearerIdentity, error) {
	verifier := v.verifier.Load()
	if verifier == nil {
		return EdgeBearerIdentity{}, ErrEdgeBearerNotReady
	}
	tok, err := verifier.Verify(ctx, raw)
	if err != nil {
		return EdgeBearerIdentity{}, err
	}
	var claims edgeBearerClaims
	if err := tok.Claims(&claims); err != nil {
		return EdgeBearerIdentity{}, fmt.Errorf("parse token claims: %w", err)
	}
	if err := claims.checkEmailVerified(true); err != nil {
		return EdgeBearerIdentity{}, err
	}
	return EdgeBearerIdentity{
		Subject:      claims.Subject,
		Email:        strings.TrimSpace(claims.Email),
		Roles:        mergeRoleClaims(claims.RealmAccess.Roles, claims.RealmRoles),
		Groups:       claims.Groups,
		Impersonator: strings.TrimSpace(claims.Impersonator),
		GitHubID:     claims.gitHubID(),
		GitHubLogin:  claims.gitHubLogin(),
	}, nil
}

// mergeRoleClaims concatenates role lists, dropping empties and duplicates.
func mergeRoleClaims(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, r := range list {
			if r == "" || seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// edgeBearerCAContext returns ctx carrying an HTTP client that trusts caFile on
// top of the system roots, for go-oidc's discovery and JWKS fetches. An empty
// caFile returns ctx unchanged.
func edgeBearerCAContext(ctx context.Context, caFile string) (context.Context, error) {
	if caFile == "" {
		return ctx, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading the edge bearer OIDC CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("edge bearer OIDC CA file %s holds no PEM certificate", caFile)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return oidc.ClientContext(ctx, &http.Client{Transport: tr}), nil
}
