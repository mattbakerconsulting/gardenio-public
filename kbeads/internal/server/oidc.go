package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// TokenVerifier verifies bearer tokens that are not static API tokens.
// Used for OIDC JWT validation as an alternative auth method.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) error
}

// IdentityVerifier extends TokenVerifier with the ability to extract the
// authenticated user's email from the token. Implementations that only
// satisfy TokenVerifier will still work for auth; the email will be empty
// in audit logs.
type IdentityVerifier interface {
	TokenVerifier
	// VerifyAndIdentify verifies the token and returns the user's email.
	// Returns ("", nil) if the token is valid but has no email claim.
	VerifyAndIdentify(ctx context.Context, rawToken string) (email string, err error)
}

// RoleExtractor extends TokenVerifier to also extract realm roles from a JWT.
// When the verifier implements this interface, AuthMiddleware injects the
// extracted roles into the request context for downstream RBAC checks.
type RoleExtractor interface {
	TokenVerifier
	ExtractRoles(ctx context.Context, rawToken string) ([]string, error)
}

// GroupExtractor extends TokenVerifier to extract group membership from a JWT
// (Keycloak "groups" or GitLab "groups_direct"; see ExtractGroups).
// When the verifier implements this interface, AuthMiddleware
// injects the extracted groups into the request context so that downstream
// middleware (e.g. UserProvisioner) can sync group membership to user beads.
type GroupExtractor interface {
	TokenVerifier
	ExtractGroups(ctx context.Context, rawToken string) ([]string, error)
}

// ClientExtractor extends TokenVerifier to extract the OAuth client the token
// was issued to (the "azp" authorized-party claim). kbeads uses it to tell a
// token minted through the public `kd login` device-grant client apart from one
// minted through the browser SSO client, because a device-code sign-in can be
// started from anywhere kd runs (every agent pod) and so must not carry
// approval authority (gd-bFFquoScHy). Returns "" when the token has no azp.
type ClientExtractor interface {
	TokenVerifier
	ExtractClientID(ctx context.Context, rawToken string) (string, error)
}

// ServiceAccountExtractor extends TokenVerifier to extract the token's
// "preferred_username" claim. For a Keycloak service-account token it is
// "service-account-<clientId>"; the machine classifier (machine_identity.go)
// uses it, with the azp claim, to tell a client's own service account from a
// human who signed in through the same client. It deliberately does not
// depend on the email claim, which a service-account token does not carry.
type ServiceAccountExtractor interface {
	TokenVerifier
	ExtractServiceAccount(ctx context.Context, rawToken string) (string, error)
}

// GitHubExtractor extends TokenVerifier to extract the GitHub account a user
// signed in through: the github_id and github_login claims that the Keycloak
// GitHub IdP copies into user attributes and the chart's github-identity
// client scope maps into the token (Lands Track B §2.2 item 1). Both are zero
// values for a user who did not sign in through GitHub.
type GitHubExtractor interface {
	TokenVerifier
	ExtractGitHub(ctx context.Context, rawToken string) (id int64, login string, err error)
}

// ImpersonationExtractor extends TokenVerifier to extract who stands behind an
// impersonated session: the "impersonator" claim (the admin's Keycloak
// username) and the "sid" claim (the Keycloak session id). Keycloak sets the
// impersonator only on a session an admin opened as another user, so it is ""
// on every other token (impersonation.go).
type ImpersonationExtractor interface {
	TokenVerifier
	ExtractImpersonation(ctx context.Context, rawToken string) (impersonator, sessionID string, err error)
}

// DeviceGrantClientID is the public Keycloak client behind `kd login`
// (deploy-values/gardenio-k3s.yaml keycloak.clients). Tokens it issues are
// OIDC human tokens carrying gardenio-admin, but they are refused for
// escalation approval (escalationApprover allowlists approval-capable
// clients; this constant only selects the refusal message).
const DeviceGrantClientID = "gardenio-cli"

// OIDCCAContext returns ctx carrying an HTTP client that trusts the PEM CA in
// caFile beside the system roots, for go-oidc's discovery and JWKS fetches
// (BEADS_OIDC_CA_FILE: the keycloak-runtime in-cluster CA, gd-lYPZlslOM7,
// when BEADS_OIDC_DISCOVERY_URL is its https Service port). The provider keeps
// that client for later JWKS refreshes. An empty caFile returns ctx unchanged.
// The file is read on every call, so a caller retrying discovery recovers once
// a not-yet-issued CA appears.
func OIDCCAContext(ctx context.Context, caFile string) (context.Context, error) {
	if caFile == "" {
		return ctx, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading the OIDC CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("OIDC CA file %s holds no PEM certificate", caFile)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return oidc.ClientContext(ctx, &http.Client{Transport: tr}), nil
}

// NewOIDCVerifierWithDiscovery creates a TokenVerifier that validates JWTs
// against the OIDC provider at issuerURL. The audience parameter is the
// expected token audience (typically the Keycloak client ID). When
// allowedDomains is non-empty, the token's email claim must have a domain in
// the allowed list. The returned verifier also implements IdentityVerifier,
// RoleExtractor, and GroupExtractor.
//
// discoveryURL serves an issuer whose discovery document is not reachable at
// the issuer URL itself. The token's "iss" claim and the discovery document's
// "issuer" must both equal issuerURL (go-oidc enforces this), but the document
// is fetched from discoveryURL
// (<discoveryURL>/.well-known/openid-configuration). That is the shape of a
// Keycloak served behind a public hostname (KC_HOSTNAME=https://auth.example/auth)
// whose public edge is not what an in-cluster daemon should dial: the daemon
// fetches discovery over the Service URL and the issuer stays the public one.
// An empty discoveryURL, or one equal to issuerURL, is plain discovery.
func NewOIDCVerifierWithDiscovery(ctx context.Context, issuerURL, discoveryURL, audience string, allowedDomains ...string) (TokenVerifier, error) {
	if audience == "" {
		return nil, fmt.Errorf("OIDC audience must not be empty — set BEADS_OIDC_AUDIENCE to the expected client ID")
	}
	fetchURL := issuerURL
	if discoveryURL != "" && discoveryURL != issuerURL {
		fetchURL = discoveryURL
		ctx = oidc.InsecureIssuerURLContext(ctx, issuerURL)
	}
	provider, err := oidc.NewProvider(ctx, fetchURL)
	if err != nil {
		return nil, fmt.Errorf("oidc provider discovery: %w", err)
	}
	cfg := &oidc.Config{ClientID: audience}
	return &oidcVerifier{
		verifier:       provider.Verifier(cfg),
		allowedDomains: allowedDomains,
	}, nil
}

type oidcVerifier struct {
	verifier       *oidc.IDTokenVerifier
	allowedDomains []string
}

// verify checks the token's signature, issuer, audience and expiry, then
// refuses a token whose email claim the issuer does not mark verified. The
// email is the identity kbeads keys an operator, team membership and approver
// matching on, so a token naming an unverified (or merely unasserted) email
// must not authenticate at all (gd-rnklckVFKS). Every human-identity method
// goes through here; a token with no email claim is unaffected.
func (v *oidcVerifier) verify(ctx context.Context, rawToken string) (*oidc.IDToken, error) {
	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	if err := requireVerifiedEmail(idToken); err != nil {
		return nil, err
	}
	return idToken, nil
}

// requireVerifiedEmail fails closed: an email claim needs email_verified set
// to true. An absent, false or malformed email_verified is refused. Keycloak
// (built-in "email" scope) and GitLab both stamp the claim as a JSON bool; the
// string "true" some issuers send is accepted as the same assertion.
func requireVerifiedEmail(idToken *oidc.IDToken) error {
	var claims struct {
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return fmt.Errorf("parsing email claims: %w", err)
	}
	if claims.Email == "" {
		return nil
	}
	switch ev := claims.EmailVerified.(type) {
	case bool:
		if ev {
			return nil
		}
	case string:
		if strings.EqualFold(ev, "true") {
			return nil
		}
	}
	return fmt.Errorf("email not verified")
}

func (v *oidcVerifier) Verify(ctx context.Context, rawToken string) error {
	idToken, err := v.verify(ctx, rawToken)
	if err != nil {
		return err
	}

	if len(v.allowedDomains) > 0 {
		var claims struct {
			Email string `json:"email"`
		}
		if err := idToken.Claims(&claims); err != nil {
			return fmt.Errorf("parsing email claim: %w", err)
		}
		if !isAllowedEmailDomain(claims.Email, v.allowedDomains) {
			return fmt.Errorf("email domain not allowed")
		}
	}

	return nil
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

// VerifyAndIdentify verifies the JWT and extracts the email claim. A token
// whose email is not verified fails (see verify).
func (v *oidcVerifier) VerifyAndIdentify(ctx context.Context, rawToken string) (string, error) {
	tok, err := v.verify(ctx, rawToken)
	if err != nil {
		return "", err
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := tok.Claims(&claims); err != nil {
		return "", nil // token valid but claims unparseable — don't fail auth
	}
	return claims.Email, nil
}

// ExtractRoles verifies the token and extracts Keycloak realm roles. Two
// claims carry them depending on which mapper put them in the token, and both
// are read (merged, deduped):
//
//   - realm_access.roles — Keycloak's built-in "roles" client scope. Its
//     realm-roles mapper writes the ACCESS token only, so an ID token (what
//     oauth2-proxy forwards, and the only token whose aud is the client)
//     carries none unless a client mapper adds them.
//   - realm_roles — the flat claim the chart's per-client "realm-roles"
//     mapper stamps into the ID, access and userinfo tokens.
func (v *oidcVerifier) ExtractRoles(ctx context.Context, rawToken string) ([]string, error) {
	idToken, err := v.verify(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	var claims struct {
		RealmAccess struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
		RealmRoles []string `json:"realm_roles"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("parse realm_access claims: %w", err)
	}
	return mergeGroupClaims(claims.RealmAccess.Roles, claims.RealmRoles), nil
}

// ExtractClientID verifies the token and returns its "azp" (authorized party)
// claim: the client_id of the OAuth client the token was issued to.
func (v *oidcVerifier) ExtractClientID(ctx context.Context, rawToken string) (string, error) {
	idToken, err := v.verify(ctx, rawToken)
	if err != nil {
		return "", err
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", fmt.Errorf("parse azp claim: %w", err)
	}
	return claims.AuthorizedParty, nil
}

// ExtractGitHub verifies the token and returns its github_id and github_login
// claims. A Keycloak user-attribute mapper sends github_id as a JSON number or
// a string depending on its type, so both are accepted. An id that is not a
// positive integer, or a login that is not a valid GitHub username, reads as
// absent rather than failing the request.
func (v *oidcVerifier) ExtractGitHub(ctx context.Context, rawToken string) (int64, string, error) {
	idToken, err := v.verify(ctx, rawToken)
	if err != nil {
		return 0, "", err
	}
	var claims struct {
		GitHubID    json.RawMessage `json:"github_id"`
		GitHubLogin json.RawMessage `json:"github_login"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return 0, "", fmt.Errorf("parse github claims: %w", err)
	}
	return parseGitHubIDClaim(claims.GitHubID), parseGitHubLoginClaim(claims.GitHubLogin), nil
}

// parseGitHubIDClaim parses a github_id claim, a positive integer as a JSON
// number or a string. Anything else is no GitHub id (0).
func parseGitHubIDClaim(raw json.RawMessage) int64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// gitHubLoginPattern is GitHub's username rule: alphanumerics or single
// hyphens, not starting or ending with a hyphen. The 39-character cap is
// checked separately.
var gitHubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:-?[A-Za-z0-9])*$`)

// parseGitHubLoginClaim returns the github_login claim when it is a JSON
// string holding a valid GitHub username, else "".
func parseGitHubLoginClaim(raw json.RawMessage) string {
	var login string
	if json.Unmarshal(raw, &login) != nil {
		return ""
	}
	login = strings.TrimSpace(login)
	if len(login) > 39 || !gitHubLoginPattern.MatchString(login) {
		return ""
	}
	return login
}

// ExtractImpersonation verifies the token and returns its impersonator and sid
// claims (parseImpersonatorClaim). A sid that is not a string reads as none,
// so it cannot turn an impersonated session into the tenant's own.
func (v *oidcVerifier) ExtractImpersonation(ctx context.Context, rawToken string) (string, string, error) {
	idToken, err := v.verify(ctx, rawToken)
	if err != nil {
		return "", "", err
	}
	var claims struct {
		Impersonator json.RawMessage `json:"impersonator"`
		SessionID    json.RawMessage `json:"sid"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", "", fmt.Errorf("parse impersonator claim: %w", err)
	}
	var sid string
	_ = json.Unmarshal(claims.SessionID, &sid)
	return parseImpersonatorClaim(claims.Impersonator), sid, nil
}

// ExtractAudiences verifies the token and returns its "aud" claim, every
// audience, including the per-land land-<name> ones (land_audience.go).
func (v *oidcVerifier) ExtractAudiences(ctx context.Context, rawToken string) ([]string, error) {
	idToken, err := v.verify(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	return idToken.Audience, nil
}

// ExtractServiceAccount verifies the token's signature (not the email-domain
// check in Verify, which an email-less service-account token would fail, nor
// the email_verified check in verify) and returns its "preferred_username"
// claim ("" when absent). It names no human, so it needs neither.
func (v *oidcVerifier) ExtractServiceAccount(ctx context.Context, rawToken string) (string, error) {
	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return "", err
	}
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", fmt.Errorf("parse preferred_username claim: %w", err)
	}
	return claims.PreferredUsername, nil
}

// ExtractGroups verifies the token and extracts the caller's group membership,
// handling both issuers we validate against (epic ka-0000004118, T6a):
//
//   - Keycloak stamps a "groups" claim of group paths (e.g. ["/project-alpha"])
//     when the client has a "groups" protocol mapper configured.
//   - GitLab (the direct-issuer cutover) does NOT emit "groups"; its ID token
//     carries "groups_direct", the full paths of groups the user is a direct
//     member of (e.g. ["acme/platform"]). It is present only when the user
//     is a direct member of at least one group.
//
// Both claims are read and merged (deduped) so the same verifier works for a
// Keycloak- or GitLab-issued token. An absent claim is not an error — it just
// contributes no groups (group membership feeds best-effort UserProvisioner
// project sync, never the auth decision itself).
func (v *oidcVerifier) ExtractGroups(ctx context.Context, rawToken string) ([]string, error) {
	idToken, err := v.verify(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	var claims struct {
		Groups       []string `json:"groups"`        // Keycloak
		GroupsDirect []string `json:"groups_direct"` // GitLab
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("parse groups claim: %w", err)
	}
	return mergeGroupClaims(claims.Groups, claims.GroupsDirect), nil
}

// mergeGroupClaims unions the Keycloak "groups" and GitLab "groups_direct"
// claims, preserving first-seen order and dropping duplicates and empties.
// Returns nil when both are empty so callers can treat "no groups" uniformly.
func mergeGroupClaims(groups, groupsDirect []string) []string {
	var merged []string
	seen := make(map[string]struct{})
	for _, g := range append(append([]string{}, groups...), groupsDirect...) {
		if g == "" {
			continue
		}
		if _, ok := seen[g]; ok {
			continue
		}
		seen[g] = struct{}{}
		merged = append(merged, g)
	}
	return merged
}

// multiOIDCVerifier tries multiple OIDC issuers in order, succeeding if any
// issuer validates the token. This enables accepting JWTs from both Keycloak
// (web SSO via ForwardAuth) and GitLab (CLI SSO via gb login).
type multiOIDCVerifier struct {
	verifiers []TokenVerifier
}

// NewMultiOIDCVerifierWithDiscovery creates a verifier that accepts tokens
// from any of the provided issuers. Each issuer is initialized via OIDC
// discovery; at least one issuer must be provided. discoveryURLs is an optional
// per-issuer discovery URL override (issuer URL -> URL to fetch the discovery
// document from; see NewOIDCVerifierWithDiscovery). Issuers absent from the
// map use plain discovery.
func NewMultiOIDCVerifierWithDiscovery(ctx context.Context, issuers []string, discoveryURLs map[string]string, audience string, allowedDomains ...string) (TokenVerifier, error) {
	if len(issuers) == 0 {
		return nil, fmt.Errorf("at least one OIDC issuer URL is required")
	}
	if len(issuers) == 1 {
		return NewOIDCVerifierWithDiscovery(ctx, issuers[0], discoveryURLs[issuers[0]], audience, allowedDomains...)
	}
	var verifiers []TokenVerifier
	for _, issuer := range issuers {
		v, err := NewOIDCVerifierWithDiscovery(ctx, issuer, discoveryURLs[issuer], audience, allowedDomains...)
		if err != nil {
			return nil, fmt.Errorf("oidc provider %s: %w", issuer, err)
		}
		verifiers = append(verifiers, v)
	}
	return &multiOIDCVerifier{verifiers: verifiers}, nil
}

func (m *multiOIDCVerifier) Verify(ctx context.Context, rawToken string) error {
	var lastErr error
	for _, v := range m.verifiers {
		if err := v.Verify(ctx, rawToken); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func (m *multiOIDCVerifier) VerifyAndIdentify(ctx context.Context, rawToken string) (string, error) {
	var lastErr error
	for _, v := range m.verifiers {
		if iv, ok := v.(IdentityVerifier); ok {
			if email, err := iv.VerifyAndIdentify(ctx, rawToken); err == nil {
				return email, nil
			} else {
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("no verifier supports identity extraction")
}

func (m *multiOIDCVerifier) ExtractRoles(ctx context.Context, rawToken string) ([]string, error) {
	var lastErr error
	for _, v := range m.verifiers {
		if rv, ok := v.(RoleExtractor); ok {
			if roles, err := rv.ExtractRoles(ctx, rawToken); err == nil {
				return roles, nil
			} else {
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

func (m *multiOIDCVerifier) ExtractGroups(ctx context.Context, rawToken string) ([]string, error) {
	var lastErr error
	for _, v := range m.verifiers {
		if gv, ok := v.(GroupExtractor); ok {
			if groups, err := gv.ExtractGroups(ctx, rawToken); err == nil {
				return groups, nil
			} else {
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

func (m *multiOIDCVerifier) ExtractClientID(ctx context.Context, rawToken string) (string, error) {
	var lastErr error
	for _, v := range m.verifiers {
		if cv, ok := v.(ClientExtractor); ok {
			if id, err := cv.ExtractClientID(ctx, rawToken); err == nil {
				return id, nil
			} else {
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", nil
}

func (m *multiOIDCVerifier) ExtractServiceAccount(ctx context.Context, rawToken string) (string, error) {
	var lastErr error
	for _, v := range m.verifiers {
		if sv, ok := v.(ServiceAccountExtractor); ok {
			if name, err := sv.ExtractServiceAccount(ctx, rawToken); err == nil {
				return name, nil
			} else {
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", nil
}

func (m *multiOIDCVerifier) ExtractGitHub(ctx context.Context, rawToken string) (int64, string, error) {
	var lastErr error
	for _, v := range m.verifiers {
		if gv, ok := v.(GitHubExtractor); ok {
			if id, login, err := gv.ExtractGitHub(ctx, rawToken); err == nil {
				return id, login, nil
			} else {
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return 0, "", lastErr
	}
	return 0, "", nil
}

func (m *multiOIDCVerifier) ExtractImpersonation(ctx context.Context, rawToken string) (string, string, error) {
	var lastErr error
	for _, v := range m.verifiers {
		if iv, ok := v.(ImpersonationExtractor); ok {
			if imp, sid, err := iv.ExtractImpersonation(ctx, rawToken); err == nil {
				return imp, sid, nil
			} else {
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return "", "", lastErr
	}
	return "", "", nil
}

func (m *multiOIDCVerifier) ExtractAudiences(ctx context.Context, rawToken string) ([]string, error) {
	var lastErr error
	for _, v := range m.verifiers {
		if av, ok := v.(AudienceExtractor); ok {
			if auds, err := av.ExtractAudiences(ctx, rawToken); err == nil {
				return auds, nil
			} else {
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}
