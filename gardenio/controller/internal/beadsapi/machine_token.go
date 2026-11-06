package beadsapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// Machine principal (gd-s7aPIf0Bgp, design gardenio/docs/machine-principal-design.md).
//
// A platform component (controller first; bridges, viewer and trigger-engine
// follow) authenticates to the beads daemon as the Keycloak service account of
// its own confidential client instead of the shared static BEADS_DAEMON_TOKEN.
// kbeads classifies that token as a machine principal (client registered in
// BEADS_MACHINE_CLIENTS) and admits exactly the writes its gardenio-machine-*
// realm roles grant — no allow-list entry per call site.
//
// Environment contract (rendered by templates/controller/deployment.yaml when
// agents.machinePrincipal.enabled):
//
//	BEADS_OIDC_TOKEN_URL           in-cluster token endpoint
//	                               (<discoveryURL>/protocol/openid-connect/token)
//	BEADS_OIDC_CLIENT_ID           the confidential client (gardenio-controller)
//	BEADS_OIDC_CLIENT_SECRET_FILE  file holding the client secret, mounted from
//	                               a Secret the operator creates by hand
//	BEADS_OIDC_CA_FILE             optional PEM CA the token endpoint's cert
//	                               chains to, trusted beside the system roots
//	                               (the keycloak-runtime in-cluster CA,
//	                               gd-lYPZlslOM7)
const (
	envMachineTokenURL         = "BEADS_OIDC_TOKEN_URL"
	envMachineClientID         = "BEADS_OIDC_CLIENT_ID"
	envMachineClientSecretFile = "BEADS_OIDC_CLIENT_SECRET_FILE"
	envMachineCAFile           = "BEADS_OIDC_CA_FILE"

	// machineAuthUnavailable is the marker every fetch failure carries. Callers
	// (and the controller log) can grep for it; it never carries the secret or
	// a token. There is deliberately NO fallback to the static token: a write
	// that should be attributed to the machine principal fails closed instead
	// of silently running as the shared identity.
	machineAuthUnavailable = "auth_unavailable"

	// machineDefaultLifetime is assumed when the token endpoint reports no
	// expiry at all (no expires_in and no exp/iat in the JWT). Keycloak always
	// sends expires_in, so this is a belt-and-braces floor.
	machineDefaultLifetime = 5 * time.Minute

	// machineWarmBackoffMin/Max bound the exponential backoff Warm uses between
	// failed first-token fetches: 1s, 2s, 4s, ... capped at 30s. The first
	// fetch on an enabling roll races the Keycloak operator reconciling the
	// new KeycloakClient (gd-pj5hT3V1la: invalid_client two seconds before the
	// client existed), so the early retries are quick and the cap keeps a
	// long Keycloak outage from hammering the endpoint.
	machineWarmBackoffMin = time.Second
	machineWarmBackoffMax = 30 * time.Second

	// machineMissingRoleRetry is how soon a token that lacks an expected
	// realm role is re-fetched. A chart that adds a gardenio-machine-* role and
	// the client that uses it in one release races the Keycloak operator: the
	// client can get a token before the role is attached to its service
	// account, and without this the half-life cache would pin that role-less
	// token for ~15 minutes (30-minute lifetime). Doubles as the rate limit on
	// re-fetching for that reason.
	machineMissingRoleRetry = 30 * time.Second

	// machineForbiddenRefreshMin is the minimum gap between two
	// 403-triggered re-acquisitions (OnForbidden). A genuinely unauthorised
	// write must not turn into a token-endpoint request per denied call.
	machineForbiddenRefreshMin = time.Minute
)

// MachineTokenOption configures a machine token source.
type MachineTokenOption func(*machineTokenSource)

// WithCAFile trusts the PEM CA in path, beside the system roots, when dialing
// the token endpoint (BEADS_OIDC_CA_FILE; the keycloak-runtime in-cluster CA,
// gd-lYPZlslOM7). The file is re-read on every fetch, like the secret, so a
// pod that starts before cert-manager has issued (the CA volume is optional)
// fails closed until kubelet projects it and then recovers without a restart.
// Empty path = system roots only.
func WithCAFile(path string) MachineTokenOption {
	return func(m *machineTokenSource) { m.caFile = strings.TrimSpace(path) }
}

// WithExpectedRoles declares the gardenio-machine-* realm roles the component
// needs on its token (each component knows its own role). A token fetched
// without one of them is still served, but is re-fetched after
// machineMissingRoleRetry instead of at half-life, and
// gardenio_machine_token_missing_role_total counts each such fetch.
func WithExpectedRoles(roles ...string) MachineTokenOption {
	return func(m *machineTokenSource) {
		m.expectedRoles = append(m.expectedRoles, roles...)
	}
}

// ErrMachineAuthUnavailable is the sentinel every machine token fetch failure
// wraps (its text is the "auth_unavailable" marker). Callers that want to
// retry only the credential failures, and not a daemon 4xx, test for it with
// IsAuthUnavailable.
var ErrMachineAuthUnavailable = errors.New(machineAuthUnavailable)

// IsAuthUnavailable reports whether err is (or wraps) a machine token fetch
// failure. It also matches on the "auth_unavailable" text so an error that
// crossed a boundary which flattened the chain (a logged-and-rewrapped string,
// a backfill result) is still recognised.
func IsAuthUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrMachineAuthUnavailable) {
		return true
	}
	return strings.Contains(err.Error(), machineAuthUnavailable)
}

// MachineForbiddenHandler is implemented by a TokenSource that can react to a
// 403 from the daemon: the cached token may predate a role Keycloak has since
// attached. OnForbidden reports whether usedToken was one of this source's
// tokens and a different, fresh token is now cached, so the caller should
// retry the request once. It is rate-limited and never loops.
type MachineForbiddenHandler interface {
	OnForbidden(ctx context.Context, usedToken string) (retry bool)
}

// MachineTokenWarmer is implemented by a TokenSource that can be primed at
// startup: Warm blocks, retrying with backoff, until the first token is held
// or ctx ends. WarmMachineToken is the nil-safe way to call it.
type MachineTokenWarmer interface {
	Warm(ctx context.Context) error
}

// WarmMachineToken primes ts at startup (see machineTokenSource.Warm). A nil
// ts, or one that cannot be warmed, returns nil immediately, so a component
// can call it unconditionally right after MachineTokenSourceFromEnv.
func WarmMachineToken(ctx context.Context, ts TokenSource) error {
	w, ok := ts.(MachineTokenWarmer)
	if !ok || ts == nil {
		return nil
	}
	return w.Warm(ctx)
}

// ErrMachineTokenEnvPartial is returned when only some of the three
// BEADS_OIDC_* variables are set. A half-configured principal is a
// misconfiguration, not "disabled".
var ErrMachineTokenEnvPartial = errors.New("machine principal: BEADS_OIDC_TOKEN_URL, BEADS_OIDC_CLIENT_ID and BEADS_OIDC_CLIENT_SECRET_FILE must all be set, or none")

// MachineTokenSourceFromEnv returns a TokenSource for the component's Keycloak
// service account when BEADS_OIDC_TOKEN_URL, BEADS_OIDC_CLIENT_ID and
// BEADS_OIDC_CLIENT_SECRET_FILE are all set; (nil, false, nil) when none are
// set (the component behaves as today, on the static token); and an error when
// only some are set.
//
// The source runs the OAuth 2.0 client-credentials grant
// (golang.org/x/oauth2/clientcredentials) and caches the token, refreshing it
// once about half its lifetime has elapsed so a daemon call never carries a
// token that is about to expire. The secret file is re-read on every fetch, so
// a rotated Secret is picked up without a restart. It never logs the token or
// the secret. A fetch failure returns an error whose text contains
// "auth_unavailable"; there is no fallback to a static token.
//
// On the first successful fetch it logs once, at Info, the token's metadata
// (azp, preferred_username, aud, realm role names and exp-iat seconds) so the
// live rollout can confirm the client, mappers and lifespan reconciled
// (design §8 items 2 and 5). The token itself is never logged.
//
// ctx is the component's lifetime context: it bounds the HTTP calls the source
// makes on behalf of later Token calls.
func MachineTokenSourceFromEnv(ctx context.Context, logger *slog.Logger, opts ...MachineTokenOption) (TokenSource, bool, error) {
	tokenURL := strings.TrimSpace(os.Getenv(envMachineTokenURL))
	clientID := strings.TrimSpace(os.Getenv(envMachineClientID))
	secretFile := strings.TrimSpace(os.Getenv(envMachineClientSecretFile))
	set := 0
	for _, v := range []string{tokenURL, clientID, secretFile} {
		if v != "" {
			set++
		}
	}
	switch set {
	case 0:
		return nil, false, nil
	case 3:
	default:
		return nil, false, ErrMachineTokenEnvPartial
	}
	if caFile := os.Getenv(envMachineCAFile); strings.TrimSpace(caFile) != "" {
		opts = append([]MachineTokenOption{WithCAFile(caFile)}, opts...)
	}
	return NewMachineTokenSource(ctx, logger, tokenURL, clientID, secretFile, opts...), true, nil
}

// NewMachineTokenSource is MachineTokenSourceFromEnv with the three values
// passed explicitly. Exported for components that read their configuration
// through something other than the environment.
func NewMachineTokenSource(ctx context.Context, logger *slog.Logger, tokenURL, clientID, secretFile string, opts ...MachineTokenOption) TokenSource {
	if logger == nil {
		logger = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m := &machineTokenSource{
		ctx:        ctx,
		logger:     logger.With("component", "machine-principal", "client", clientID),
		tokenURL:   tokenURL,
		clientID:   clientID,
		secretFile: secretFile,
		now:        time.Now,
		sleep:      sleepCtx,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// sleepCtx is the production sleep for Warm's backoff.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// machineTokenSource caches one client-credentials token and refreshes it at
// half-life. It is safe for concurrent use; concurrent callers that find the
// cache stale funnel through one fetch.
type machineTokenSource struct {
	ctx        context.Context
	logger     *slog.Logger
	tokenURL   string
	clientID   string
	secretFile string
	caFile     string

	// caPEM and caClient cache the HTTP client built from caFile's contents,
	// rebuilt only when the file changes so fetches reuse one Transport.
	caPEM    []byte
	caClient *http.Client

	// now is the clock; tests pin it to drive the half-life refresh.
	now func() time.Time
	// sleep waits d or until ctx ends; tests replace it so Warm's backoff
	// runs instantly and the delays can be asserted.
	sleep func(ctx context.Context, d time.Duration) error

	// expectedRoles are the realm roles the token must carry (WithExpectedRoles).
	expectedRoles []string

	mu         sync.Mutex
	token      string
	prevToken  string    // the token before the last re-fetch (a peer's stale bearer)
	lastForce  time.Time // last 403-triggered re-acquisition
	issuedAt   time.Time // when the cached token was fetched
	refreshAt  time.Time // issuedAt + lifetime/2
	expiresAt  time.Time
	loggedOnce bool
}

// Token implements TokenSource. ctx is the caller's request context and bounds
// the fetch when one is needed.
func (m *machineTokenSource) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.token != "" && now.Before(m.refreshAt) {
		return m.token, nil
	}
	tok, err := m.fetch(ctx)
	if err != nil {
		// A stale-but-unexpired token is still better than failing a write
		// when the IdP blips: serve it until it actually expires, then fail
		// closed. Never serve past expiry.
		if m.token != "" && now.Before(m.expiresAt) {
			m.logger.Warn("machine token refresh failed; serving cached token until expiry", "error", err)
			return m.token, nil
		}
		machineTokenUnavailableTotal.WithLabelValues(m.clientID).Inc()
		return "", err
	}
	return tok, nil
}

// OnForbidden implements MachineForbiddenHandler. A 403 on a machine principal
// can mean the cached token predates a role Keycloak has since attached (the
// first-roll race), and the token would otherwise keep every write denied until
// it expires. It counts the 403 (gardenio_machine_forbidden_total) when
// usedToken is ours, then — at most once per machineForbiddenRefreshMin —
// drops the cache and re-acquires. It returns true when a token different from
// usedToken is now cached, so the caller retries the request once. A fetch
// failure keeps the old token and returns false.
func (m *machineTokenSource) OnForbidden(ctx context.Context, usedToken string) bool {
	if usedToken == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if usedToken != m.token && usedToken != m.prevToken {
		return false // not this source's bearer (e.g. a per-request user token)
	}
	machineForbiddenTotal.WithLabelValues(m.clientID).Inc()
	if usedToken != m.token {
		return true // a concurrent caller already re-acquired
	}
	now := m.now()
	if !m.lastForce.IsZero() && now.Sub(m.lastForce) < machineForbiddenRefreshMin {
		return false
	}
	m.lastForce = now
	m.logger.Warn("daemon returned 403 to the machine principal; re-acquiring its token once")
	if _, err := m.fetch(ctx); err != nil {
		m.logger.Warn("machine token re-acquire after 403 failed; keeping cached token", "error", err)
		return false
	}
	return m.token != usedToken
}

// missingRoles returns the entries of want that are absent from have.
func missingRoles(want, have []string) []string {
	var out []string
	for _, w := range want {
		if !slices.Contains(have, w) {
			out = append(out, w)
		}
	}
	return out
}

// Warm implements MachineTokenWarmer: it fetches the first token, retrying
// with exponential backoff (machineWarmBackoffMin doubling to
// machineWarmBackoffMax) until one is held or ctx ends. A component calls it
// once at startup, before its first daemon write, so the seed writes that run
// only once (EnsureConfigs, a backfill) never fire against a Keycloak client
// the operator has not reconciled yet (gd-pj5hT3V1la). Each failed attempt is
// logged at Warn with the attempt number and the next delay; the secret and
// the token are never logged.
//
// Warm returns nil as soon as a token is cached (including one another caller
// fetched meanwhile). When ctx ends first it returns an error wrapping both
// ErrMachineAuthUnavailable and ctx.Err(). It never gives up on its own: the
// design (machine-principal-design.md §6) says a cold start without Keycloak
// retries and surfaces auth_unavailable rather than crash-looping the pod, and
// the caller's readiness gate is what makes the wait visible.
func (m *machineTokenSource) Warm(ctx context.Context) error {
	if ctx == nil {
		ctx = m.ctx
	}
	delay := machineWarmBackoffMin
	for attempt := 1; ; attempt++ {
		m.mu.Lock()
		if m.token != "" && m.now().Before(m.expiresAt) {
			m.mu.Unlock()
			return nil
		}
		_, err := m.fetch(ctx)
		m.mu.Unlock()
		if err == nil {
			if attempt > 1 {
				m.logger.Info("machine token warm-up succeeded", "attempts", attempt)
			}
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w: machine token warm-up for client %s abandoned after %d attempt(s): %w", ErrMachineAuthUnavailable, m.clientID, attempt, ctx.Err())
		}
		m.logger.Warn("machine token warm-up failed; retrying",
			"attempt", attempt,
			"retry_in", delay,
			"error", err,
		)
		if serr := m.sleep(ctx, delay); serr != nil {
			return fmt.Errorf("%w: machine token warm-up for client %s abandoned after %d attempt(s): %w", ErrMachineAuthUnavailable, m.clientID, attempt, serr)
		}
		delay = min(delay*2, machineWarmBackoffMax)
	}
}

// fetch performs one client-credentials exchange. Caller holds m.mu.
func (m *machineTokenSource) fetch(ctx context.Context) (string, error) {
	secret, err := m.readSecret()
	if err != nil {
		return "", err
	}
	cfg := &clientcredentials.Config{
		ClientID:     m.clientID,
		ClientSecret: secret,
		TokenURL:     m.tokenURL,
		AuthStyle:    oauth2.AuthStyleInHeader,
	}
	if ctx == nil {
		ctx = m.ctx
	}
	fetchCtx, cancel := mergeCancel(ctx, m.ctx)
	defer cancel()
	if m.caFile != "" {
		hc, err := m.caHTTPClient()
		if err != nil {
			return "", err
		}
		fetchCtx = context.WithValue(fetchCtx, oauth2.HTTPClient, hc)
	}
	tok, err := cfg.Token(fetchCtx)
	if err != nil {
		return "", fmt.Errorf("%w: machine token fetch for client %s failed: %w", ErrMachineAuthUnavailable, m.clientID, err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("%w: machine token fetch for client %s returned an empty access token", ErrMachineAuthUnavailable, m.clientID)
	}
	now := m.now()
	claims := parseJWTClaims(tok.AccessToken)
	// oauth2 stamps Expiry from expires_in against the wall clock, so measure
	// the lifetime against the wall clock too (m.now may be a test clock) and
	// then lay it onto m.now's timeline.
	lifetime := time.Until(tok.Expiry)
	if tok.Expiry.IsZero() || lifetime <= 0 {
		lifetime = claims.lifetime()
	}
	if lifetime <= 0 {
		lifetime = machineDefaultLifetime
	}
	if m.token != "" && m.token != tok.AccessToken {
		m.prevToken = m.token
	}
	m.token = tok.AccessToken
	m.issuedAt = now
	m.expiresAt = now.Add(lifetime)
	m.refreshAt = now.Add(lifetime / 2)
	if missing := missingRoles(m.expectedRoles, claims.roles()); len(missing) > 0 {
		retry := min(machineMissingRoleRetry, lifetime/2)
		m.refreshAt = now.Add(retry)
		for _, r := range missing {
			machineTokenMissingRoleTotal.WithLabelValues(m.clientID, r).Inc()
		}
		m.logger.Warn("machine token lacks expected realm role(s); will re-fetch soon (Keycloak may not have attached the role yet)",
			"missing_roles", missing,
			"realm_roles", claims.roles(),
			"retry_in", retry,
		)
	}
	if !m.loggedOnce {
		m.loggedOnce = true
		m.logger.Info("machine token acquired",
			"azp", claims.AZP,
			"preferred_username", claims.PreferredUsername,
			"aud", claims.audience(),
			"realm_roles", claims.roles(),
			"exp_minus_iat_seconds", claims.expMinusIat(),
			"lifetime_seconds", int64(lifetime/time.Second),
		)
	}
	return m.token, nil
}

// readSecret re-reads the client secret from disk on every call so a rotated
// Secret takes effect at the next refresh without a restart.
func (m *machineTokenSource) readSecret() (string, error) {
	b, err := os.ReadFile(m.secretFile)
	if err != nil {
		return "", fmt.Errorf("%w: machine client secret for %s unreadable: %w", ErrMachineAuthUnavailable, m.clientID, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%w: machine client secret file for %s is empty", ErrMachineAuthUnavailable, m.clientID)
	}
	return s, nil
}

// caHTTPClient returns an HTTP client whose RootCAs are the system roots plus
// the PEM CA in m.caFile. The file is re-read on every call; the client is
// rebuilt only when its contents change. Caller holds m.mu.
func (m *machineTokenSource) caHTTPClient() (*http.Client, error) {
	pem, err := os.ReadFile(m.caFile)
	if err != nil {
		return nil, fmt.Errorf("%w: machine token CA for %s unreadable: %w", ErrMachineAuthUnavailable, m.clientID, err)
	}
	if m.caClient != nil && bytes.Equal(pem, m.caPEM) {
		return m.caClient, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%w: machine token CA file %s for %s holds no PEM certificate", ErrMachineAuthUnavailable, m.caFile, m.clientID)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if m.caClient != nil {
		m.caClient.CloseIdleConnections()
	}
	m.caPEM, m.caClient = pem, &http.Client{Transport: tr}
	return m.caClient, nil
}

// mergeCancel returns a context derived from primary that is also cancelled
// when secondary is done, so a fetch stops on either the caller's deadline or
// the component shutting down.
func mergeCancel(primary, secondary context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(primary)
	if secondary == nil || secondary == primary {
		return ctx, cancel
	}
	stop := context.AfterFunc(secondary, cancel)
	return ctx, func() { stop(); cancel() }
}

// machineTokenClaims is the subset of a Keycloak service-account access token
// the first-fetch log line reports. Decoded WITHOUT signature verification —
// it is for operator-visible metadata only; kbeads verifies the signature.
type machineTokenClaims struct {
	AZP               string          `json:"azp"`
	PreferredUsername string          `json:"preferred_username"`
	Aud               json.RawMessage `json:"aud"`
	Exp               int64           `json:"exp"`
	Iat               int64           `json:"iat"`
	RealmRoles        []string        `json:"realm_roles"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func parseJWTClaims(raw string) machineTokenClaims {
	var c machineTokenClaims
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return c
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c
	}
	_ = json.Unmarshal(payload, &c)
	return c
}

// audience renders aud (a string or an array in OIDC) as a flat string list.
func (c machineTokenClaims) audience() []string {
	if len(c.Aud) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(c.Aud, &many)
	return many
}

// roles merges realm_roles (the chart's realm-roles mapper) with
// realm_access.roles (Keycloak's default), deduplicated, order preserved.
func (c machineTokenClaims) roles() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range append(append([]string{}, c.RealmRoles...), c.RealmAccess.Roles...) {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

func (c machineTokenClaims) expMinusIat() int64 {
	if c.Exp == 0 || c.Iat == 0 {
		return 0
	}
	return c.Exp - c.Iat
}

func (c machineTokenClaims) lifetime() time.Duration {
	s := c.expMinusIat()
	if s <= 0 {
		return 0
	}
	return time.Duration(s) * time.Second
}
