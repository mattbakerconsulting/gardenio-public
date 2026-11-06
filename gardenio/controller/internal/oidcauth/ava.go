package oidcauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// AVAHeaderName is the request header AWS Verified Access uses to forward the
// signed JWT that contains the authenticated user identity. AVA places the
// JWT here after authenticating the user via its configured trust provider
// (e.g. GitLab OIDC); applications behind AVA verify the signature against
// the AVA instance public keys and trust the resulting claims.
const AVAHeaderName = "X-Amzn-Ava-User-Context"

// avaExpectedAlg is the only algorithm AVA uses to sign the user-context JWT
// (ECDSA P-384 + SHA-384). Rejecting any other alg defends against header
// substitution attacks (e.g. alg=none, alg=HS256 with the public key as the
// HMAC secret).
const avaExpectedAlg = "ES384"

// AVAConfig configures the AVA path of the oidcauth middleware. The AVA path
// is OPT-IN: when TrustSigner is empty, the AVA branch is disabled and the
// middleware behaves as a Keycloak-only OIDC gate.
type AVAConfig struct {
	// TrustSigner is the AVA instance ARN whose signed JWTs we trust, e.g.
	// "arn:aws:ec2:us-east-1:234567890123:verified-access-instance/vai-…".
	// MUST be set to enable the AVA branch. Defends against a token signed
	// by a different (untrusted) AVA instance: the signer claim in the JWT
	// header is compared with this value and the request rejected on
	// mismatch.
	TrustSigner string

	// Region is the AWS region used to construct the public-keys endpoint:
	// https://public-keys.prod.verified-access.<region>.amazonaws.com/<kid>.
	// Defaults to "us-east-1" when empty.
	Region string

	// DefaultRole, when non-empty, is unconditionally appended to the role
	// list extracted from a verified AVA token. This mirrors Keycloak's
	// `isDefault: true` realm-role behavior for AVA-authenticated users
	// whose GitLab group memberships do not include a literal `gardenio-*`
	// role name (decision ka-0000002146: "blanket viewer" — AVA is the auth
	// edge and any user reaching the controller is already authenticated).
	// Empty disables the mechanism (current pre-decision behavior).
	DefaultRole string

	// publicKeyURLFn overrides the public-key URL builder. Tests only.
	publicKeyURLFn func(region, kid string) string

	// httpClient overrides the JWKS fetcher's HTTP client. Tests only.
	httpClient *http.Client
}

func (c AVAConfig) enabled() bool {
	return c.TrustSigner != ""
}

func (c AVAConfig) region() string {
	if c.Region == "" {
		return "us-east-1"
	}
	return c.Region
}

func (c AVAConfig) publicKeyURL(kid string) string {
	if c.publicKeyURLFn != nil {
		return c.publicKeyURLFn(c.region(), kid)
	}
	return fmt.Sprintf("https://public-keys.prod.verified-access.%s.amazonaws.com/%s", c.region(), kid)
}

func (c AVAConfig) client() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// avaKeyCache is a process-wide kid→public-key cache. AVA rotates signing
// keys occasionally; on a cache miss we fetch from the AWS public-keys
// endpoint. Keys do not expire from the cache — a rotated kid means a new
// cache entry, the old one stays harmless. Cache is unbounded in principle
// but bounded in practice by the small number of active kids.
type avaKeyCache struct {
	mu   sync.RWMutex
	keys map[string]*ecdsa.PublicKey
}

func newAVAKeyCache() *avaKeyCache {
	return &avaKeyCache{keys: map[string]*ecdsa.PublicKey{}}
}

func (c *avaKeyCache) get(kid string) (*ecdsa.PublicKey, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	k, ok := c.keys[kid]
	return k, ok
}

func (c *avaKeyCache) put(kid string) func(*ecdsa.PublicKey) {
	return func(k *ecdsa.PublicKey) {
		c.mu.Lock()
		c.keys[kid] = k
		c.mu.Unlock()
	}
}

// avaVerifier verifies AWS Verified Access user-context JWTs. It is goroutine
// safe and may be shared across requests.
type avaVerifier struct {
	cfg    AVAConfig
	cache  *avaKeyCache
	logger *slog.Logger
}

func newAVAVerifier(cfg AVAConfig, logger *slog.Logger) *avaVerifier {
	return &avaVerifier{cfg: cfg, cache: newAVAKeyCache(), logger: logger}
}

// avaHeader is the JOSE header AVA sets on user-context JWTs.
//
// AVA puts expiry in the JOSE header (`exp`), NOT in the JWT payload — this
// is non-standard but matches what AWS Verified Access actually emits as of
// 2026-05. The payload-level exp is absent; we read header.Exp for the
// liveness check.
type avaHeader struct {
	Alg    string `json:"alg"`
	Kid    string `json:"kid"`
	Signer string `json:"signer"`
	Exp    int64  `json:"exp"`
}

// avaClaims is the subset of the AVA JWT payload that we propagate to
// downstream handlers. AVA passes through whatever the trust provider
// returned; GitLab OIDC populates email + sub + groups at top level AND
// duplicates them (along with the id-token exp) under
// "additional_user_context". `exp` lives on the header, not the payload.
type avaClaims struct {
	Iss   string `json:"iss"`
	Sub   string `json:"sub"`
	Email string `json:"email"`
	// EmailVerified is passed through when the trust provider sent it (GitLab
	// OIDC does). See checkEmailVerified.
	EmailVerified json.RawMessage `json:"email_verified"`
	// Realm-style roles. GitLab OIDC does not set these; reserved for future
	// trust providers (e.g. Cognito) that do.
	Groups []string `json:"groups"`
	// AVA wraps the trust-provider id-token claims under
	// additional_user_context. For GitLab those duplicate the top-level
	// fields. Kept here so future trust providers that ONLY populate this
	// nested object still work.
	AdditionalUserContext struct {
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"`
		Sub           string          `json:"sub"`
		Groups        []string        `json:"groups"`
	} `json:"additional_user_context"`
}

// checkEmailVerified refuses claims whose trust provider said the email is
// not verified: an email_verified claim, top-level or nested, that is present
// and not true (gd-ddpTGbl4Te). Presence is not required. The payload carries
// whatever the trust provider returned and no captured AVA payload pins
// email_verified as always forwarded, so requiring it could lock every AVA
// user out. The AVA instance's own trust-provider policy still gates who gets
// a signed header at all.
func (c *avaClaims) checkEmailVerified() error {
	for _, raw := range []json.RawMessage{c.EmailVerified, c.AdditionalUserContext.EmailVerified} {
		if verified, present := EmailVerifiedClaim(raw); present && !verified {
			return errors.New("email_verified is not true")
		}
	}
	return nil
}

// effectiveEmail returns the email claim, preferring the nested
// additional_user_context.email when present (GitLab via AVA puts it there).
func (c *avaClaims) effectiveEmail() string {
	if c.AdditionalUserContext.Email != "" {
		return c.AdditionalUserContext.Email
	}
	return c.Email
}

func (c *avaClaims) effectiveGroups() []string {
	if len(c.AdditionalUserContext.Groups) > 0 {
		return c.AdditionalUserContext.Groups
	}
	return c.Groups
}

// normalizeAvaGroups maps AVA-forwarded GitLab groups to the bare gardenio-*
// role names that the RBAC hierarchy (roleRank / HasMinimumRole) actually
// matches, returning the original groups with any recognized bare role
// appended.
//
// AVA forwards GitLab group memberships as FULL SUBGROUP PATHS, e.g.
// "example-org/gardenio-admin".
// Pod-side RBAC, however, matches BARE role names ("gardenio-admin",
// "gardenio-operator", "gardenio-viewer"), mirroring Keycloak's
// realm_access.roles posture. A full path is not equal to a bare name, so
// without this normalization every AVA operator/admin collapses to the
// AVA_DEFAULT_ROLE viewer floor (rank 1) and every operatorOnly/adminOnly
// surface in the viewer is forbidden to them (ka-0000001646).
//
// For each group whose TRAILING path segment exactly matches a known bare
// gardenio-* role (per roleRank, the single source of truth for the hierarchy),
// the bare role is appended — deduped and order-preserving. Original group
// entries are preserved unchanged; non-gardenio paths contribute nothing beyond
// the viewer floor that applyAvaDefaultRole adds separately. Matching is
// CASE-SENSITIVE: group names must match GitLab's on-server casing exactly.
// A bare-name group (Keycloak shape) is left unchanged: its trailing segment
// is itself and is already present, so nothing is appended.
func normalizeAvaGroups(groups []string) []string {
	out := slices.Clone(groups)
	for _, g := range groups {
		seg := g
		if i := strings.LastIndex(g, "/"); i >= 0 {
			seg = g[i+1:]
		}
		if _, known := roleRank[seg]; !known {
			continue
		}
		if !slices.Contains(out, seg) {
			out = append(out, seg)
		}
	}
	return out
}

// applyAvaDefaultRole returns groups with defaultRole appended when set and
// not already present. Empty defaultRole is a no-op (returns groups unchanged).
// Idempotent: re-applying with the same defaultRole does not duplicate the
// entry. Implements decision ka-0000002146 (epic ka-0000002784).
func applyAvaDefaultRole(groups []string, defaultRole string) []string {
	if defaultRole == "" {
		return groups
	}
	if slices.Contains(groups, defaultRole) {
		return groups
	}
	return append(groups, defaultRole)
}

// verify parses, signature-verifies and claim-checks an AVA JWT. It returns
// the trusted claims on success. Every failure mode returns a non-nil error
// — callers MUST NOT treat a partially-populated avaClaims as authenticated.
func (v *avaVerifier) verify(ctx context.Context, raw string) (*avaClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("ava jwt: malformed (need 3 segments)")
	}
	headerRaw, payloadRaw, sigRaw := parts[0], parts[1], parts[2]

	headerJSON, err := base64.RawURLEncoding.DecodeString(headerRaw)
	if err != nil {
		return nil, fmt.Errorf("ava jwt: header b64 decode: %w", err)
	}
	var header avaHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("ava jwt: header parse: %w", err)
	}
	if header.Alg != avaExpectedAlg {
		return nil, fmt.Errorf("ava jwt: unexpected alg %q (want %q)", header.Alg, avaExpectedAlg)
	}
	if header.Kid == "" {
		return nil, errors.New("ava jwt: missing kid")
	}
	if header.Signer == "" {
		return nil, errors.New("ava jwt: missing signer in header")
	}
	// Constant-time-ish equality on a short ARN is fine; this rejects tokens
	// signed by any AVA instance other than the one we trust.
	if header.Signer != v.cfg.TrustSigner {
		return nil, fmt.Errorf("ava jwt: signer mismatch (got %q)", header.Signer)
	}
	if header.Exp == 0 {
		return nil, errors.New("ava jwt: missing exp in header")
	}
	if time.Now().Unix() >= header.Exp {
		return nil, fmt.Errorf("ava jwt: expired at %d", header.Exp)
	}

	pub, err := v.publicKey(ctx, header.Kid)
	if err != nil {
		return nil, fmt.Errorf("ava jwt: fetch public key for kid %q: %w", header.Kid, err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(sigRaw)
	if err != nil {
		return nil, fmt.Errorf("ava jwt: signature b64 decode: %w", err)
	}
	// ES384 signatures are 96 bytes: R||S, each 48 bytes (P-384 coordinate).
	if len(sig) != 96 {
		return nil, fmt.Errorf("ava jwt: unexpected signature length %d (want 96)", len(sig))
	}
	r := new(big.Int).SetBytes(sig[:48])
	s := new(big.Int).SetBytes(sig[48:])

	signedInput := headerRaw + "." + payloadRaw
	sum := sha512.Sum384([]byte(signedInput))
	if !ecdsa.Verify(pub, sum[:], r, s) {
		return nil, errors.New("ava jwt: signature verification failed")
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(payloadRaw)
	if err != nil {
		return nil, fmt.Errorf("ava jwt: payload b64 decode: %w", err)
	}
	var claims avaClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, fmt.Errorf("ava jwt: payload parse: %w", err)
	}
	if err := claims.checkEmailVerified(); err != nil {
		return nil, fmt.Errorf("ava jwt: %w", err)
	}
	return &claims, nil
}

// publicKey returns the ECDSA P-384 public key for kid, fetching and caching
// it on first use. Concurrent misses for the same kid may fetch more than
// once; this is harmless and lighter than per-kid locking.
func (v *avaVerifier) publicKey(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	if k, ok := v.cache.get(kid); ok {
		return k, nil
	}
	url := v.cfg.publicKeyURL(kid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.cfg.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("public key endpoint returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	key, err := parseECDSAPublicKey(body)
	if err != nil {
		return nil, err
	}
	v.cache.put(kid)(key)
	return key, nil
}

func parseECDSAPublicKey(pemBytes []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("public key: not PEM-encoded")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("public key: parse PKIX: %w", err)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key: not ECDSA (got %T)", pub)
	}
	if ec.Curve != elliptic.P384() {
		return nil, fmt.Errorf("public key: unexpected curve %s (want P-384)", ec.Curve.Params().Name)
	}
	return ec, nil
}
