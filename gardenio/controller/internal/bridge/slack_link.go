package bridge

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// Slack account linking (gd-OZJ6H2S3xx, decision gd-uPHB7YcHuI, option A).
//
// A person who acts in Slack (approves an escalation, answers a decision,
// spawns an agent) must reach kbeads with a token Keycloak issued for THAT
// person; the bridge never asserts a human identity in a header. The person
// links once: an ephemeral "Link your gardenio account" button opens a browser
// sign-in through the confidential gardenio-slack-link client (PKCE S256,
// scope openid email offline_access). The callback records the bind on the
// person's own user bead under the person's own token and the bridge keeps
// the person's OFFLINE REFRESH TOKEN, AEAD-encrypted at rest with a key only
// the bridge holds. Per write the bridge refreshes it into a short-lived
// access token and sends that as the Authorization bearer (WithUserBearer).
//
// Properties:
//   - The Slack user id never comes from the browser: it is read from the
//     state record the bridge created when it showed the button to exactly
//     that Slack user. A forged callback has no state; a replayed one finds
//     the state consumed. State TTL is 10 minutes, single use.
//   - The browser sign-in alone never saves a link (gd-L55f9EYFjN). The callback
//     parks the tokens in a pending-confirm record and the bridge DMs the Slack
//     user from the state "Link this Slack account to <email>? Confirm /
//     Cancel". Only a Confirm press from THAT Slack user, over Socket Mode,
//     binds the user bead and stores the row; Cancel, expiry (10 minutes) and
//     a second press all store nothing and revoke the fresh offline token. So
//     a start URL forwarded to someone else puts the question in the
//     forwarder's Slack, not the signer's. Identity is never inferred from
//     email: the Slack profile email is only a warning line on the prompt.
//   - Blast radius of a bridge compromise is exactly the linked people, each
//     capped at gardenio-operator by the kbeads azp role ceiling, until
//     revoked. Revocation is per person and Keycloak-enforced: unlink in
//     Slack (RFC 7009 revoke + row delete), revoke the gardenio-slack-link
//     consent/offline session in Keycloak, or disable the user. The next
//     refresh fails with invalid_grant and the bridge drops the link.
//   - The store is a file next to the bridge's state (slack-links.json),
//     ciphertext only; the key is a separate Secret. kbeads holds no
//     credential at all. Moving the rows to a kbeads table gated to
//     machine:gardenio-slack-bridge (design §6) is a follow-up once the
//     bridge's own machine principal exists (Task 5, gd-Hc34EENbO9); the
//     SlackLinkStore interface is the seam.

// ErrSlackNotLinked is returned when a Slack user has no usable link: never
// linked, unlinked, or the stored refresh token was revoked.
var ErrSlackNotLinked = errors.New("slack user is not linked to a gardenio account")

// slackLinkStateTTL bounds how long a "Link your account" button is valid.
const slackLinkStateTTL = 10 * time.Minute

// slackLinkConfirmTTL bounds how long a signed-in link waits for its Slack
// user to press Confirm; past it the offline token is revoked.
const slackLinkConfirmTTL = 10 * time.Minute

// slackLinkTokenSkew is how long before expiry a cached access token is
// treated as expired.
const slackLinkTokenSkew = 30 * time.Second

// slackLinkMaxPending caps outstanding link states so a flood of button
// presses cannot grow the map without bound (oldest are pruned first).
const slackLinkMaxPending = 1000

// SlackLinkConfig configures a SlackLinker.
type SlackLinkConfig struct {
	// PublicURL is the https origin the browser reaches the bridge's
	// /slack/link/ handlers through (the viewer-authproxy host on k3s).
	PublicURL string
	// IssuerURL is the Keycloak realm issuer, e.g.
	// https://auth.example.com/auth/realms/gardenio. The authorize, token and
	// revoke endpoints hang off it.
	IssuerURL string
	// ClientID / ClientSecret identify the confidential gardenio-slack-link
	// client.
	ClientID     string
	ClientSecret string
	// Key is the 32-byte AES-256-GCM key that seals refresh tokens at rest.
	Key []byte
	// StorePath is the JSON file the ciphertext rows live in.
	StorePath string
	// HTTPClient is used for Keycloak calls (default: 15 s timeout).
	HTTPClient *http.Client
	// Now overrides the clock in tests.
	Now func() time.Time
}

// SlackLink is one linked Slack user. Ciphertext is the sealed offline
// refresh token; the plaintext never leaves SlackLinker.
type SlackLink struct {
	SlackUserID string    `json:"slack_user_id"`
	SlackTeamID string    `json:"slack_team_id,omitempty"`
	Subject     string    `json:"subject"`
	Email       string    `json:"email"`
	Ciphertext  []byte    `json:"ciphertext"`
	LinkedAt    time.Time `json:"linked_at"`
	LastUsedAt  time.Time `json:"last_used_at,omitempty"`
}

// SlackLinkStore persists links. Implementations must be safe for concurrent
// use.
type SlackLinkStore interface {
	Get(slackUserID string) (*SlackLink, bool, error)
	Put(link *SlackLink) error
	Delete(slackUserID string) error
	// List returns every stored link, for the reverse (email -> Slack user)
	// lookup the escalation operator DM needs (gd-So39r4Fo4c).
	List() ([]*SlackLink, error)
}

// fileSlackLinkStore keeps links in one JSON file, rewritten atomically.
type fileSlackLinkStore struct {
	path string
	mu   sync.Mutex
}

type fileSlackLinkDoc struct {
	Version int                   `json:"version"`
	Links   map[string]*SlackLink `json:"links"`
}

// NewFileSlackLinkStore opens (or will create) the link file at path.
func NewFileSlackLinkStore(path string) (SlackLinkStore, error) {
	if path == "" {
		return nil, errors.New("slack link store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("slack link store dir: %w", err)
	}
	s := &fileSlackLinkStore{path: path}
	if _, err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *fileSlackLinkStore) load() (*fileSlackLinkDoc, error) {
	doc := &fileSlackLinkDoc{Version: 1, Links: map[string]*SlackLink{}}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read slack link store: %w", err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(raw, doc); err != nil {
		return nil, fmt.Errorf("parse slack link store %s: %w", s.path, err)
	}
	if doc.Links == nil {
		doc.Links = map[string]*SlackLink{}
	}
	return doc, nil
}

func (s *fileSlackLinkStore) save(doc *fileSlackLinkDoc) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write slack link store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("commit slack link store: %w", err)
	}
	return nil
}

func (s *fileSlackLinkStore) Get(slackUserID string) (*SlackLink, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if err != nil {
		return nil, false, err
	}
	l, ok := doc.Links[slackUserID]
	if !ok {
		return nil, false, nil
	}
	cp := *l
	return &cp, true, nil
}

func (s *fileSlackLinkStore) List() ([]*SlackLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]*SlackLink, 0, len(doc.Links))
	for _, l := range doc.Links {
		cp := *l
		out = append(out, &cp)
	}
	return out, nil
}

func (s *fileSlackLinkStore) Put(link *SlackLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if err != nil {
		return err
	}
	cp := *link
	doc.Links[link.SlackUserID] = &cp
	return s.save(doc)
}

func (s *fileSlackLinkStore) Delete(slackUserID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := doc.Links[slackUserID]; !ok {
		return nil
	}
	delete(doc.Links, slackUserID)
	return s.save(doc)
}

// pendingSlackLink is one outstanding "Link your account" button: the state
// value is the map key, bound to the Slack user it was shown to.
type pendingSlackLink struct {
	slackUserID  string
	slackTeamID  string
	codeVerifier string
	createdAt    time.Time
}

// pendingSlackConfirm is a finished browser sign-in waiting for its Slack
// user to press Confirm. It holds the fresh tokens in memory only; nothing is
// bound or stored until Confirm.
type pendingSlackConfirm struct {
	slackUserID  string
	slackTeamID  string
	claims       idTokenClaims
	accessToken  string
	refreshToken string
	expiresIn    int
	createdAt    time.Time
}

// cachedSlackToken is a refreshed access token kept until shortly before exp.
type cachedSlackToken struct {
	token   string
	email   string
	expires time.Time
}

// SlackLinkDaemon is the slice of the daemon client the linker needs.
type SlackLinkDaemon interface {
	LinkSlackIdentityAsUser(ctx context.Context, bearer string, req beadsapi.SlackIdentityLinkRequest) (*beadsapi.SlackIdentityLinkResponse, error)
	UnlinkSlackIdentityAsUser(ctx context.Context, bearer string, req beadsapi.SlackIdentityLinkRequest) (*beadsapi.SlackIdentityLinkResponse, error)
}

// SlackLinker runs the link flow and mints per-write access tokens for linked
// Slack users.
type SlackLinker struct {
	cfg    SlackLinkConfig
	store  SlackLinkStore
	daemon SlackLinkDaemon
	aead   cipher.AEAD
	http   *http.Client
	logger *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	pending  map[string]pendingSlackLink
	confirms map[string]pendingSlackConfirm
	tokens   map[string]cachedSlackToken

	// emails resolves a Slack user's profile email, used only to put a
	// warning on the confirm prompt when it differs from the signed-in email.
	// It never decides whether a link is allowed.
	emails SlackEmailResolver

	// onConfirm posts the "Link this Slack account to <email>?" prompt to the
	// Slack user. A link flow with no hook (or a hook that errors) is refused
	// and its tokens revoked: the confirm step cannot be skipped.
	onConfirm func(slackUserID, confirmID, email, warning string) error
}

// NewSlackLinker validates cfg and opens the store. Every field but
// HTTPClient and Now is required.
func NewSlackLinker(cfg SlackLinkConfig, store SlackLinkStore, daemon SlackLinkDaemon, logger *slog.Logger) (*SlackLinker, error) {
	cfg.PublicURL = strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/")
	cfg.IssuerURL = strings.TrimRight(strings.TrimSpace(cfg.IssuerURL), "/")
	switch {
	case cfg.PublicURL == "" || !strings.HasPrefix(cfg.PublicURL, "https://"):
		return nil, errors.New("slack link: PublicURL must be an https origin")
	case cfg.IssuerURL == "":
		return nil, errors.New("slack link: IssuerURL is required")
	case cfg.ClientID == "" || cfg.ClientSecret == "":
		return nil, errors.New("slack link: ClientID and ClientSecret are required")
	case len(cfg.Key) != 32:
		return nil, fmt.Errorf("slack link: Key must be 32 bytes, got %d", len(cfg.Key))
	case store == nil:
		return nil, errors.New("slack link: store is required")
	case daemon == nil:
		return nil, errors.New("slack link: daemon client is required")
	}
	block, err := aes.NewCipher(cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("slack link: key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("slack link: aead: %w", err)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SlackLinker{
		cfg:      cfg,
		store:    store,
		daemon:   daemon,
		aead:     aead,
		http:     hc,
		logger:   logger,
		now:      now,
		pending:  map[string]pendingSlackLink{},
		confirms: map[string]pendingSlackConfirm{},
		tokens:   map[string]cachedSlackToken{},
	}, nil
}

// SetOnConfirm registers the hook that posts the confirm prompt to the Slack
// user (the bot DMs Confirm / Cancel buttons carrying confirmID).
func (l *SlackLinker) SetOnConfirm(fn func(slackUserID, confirmID, email, warning string) error) {
	l.mu.Lock()
	l.onConfirm = fn
	l.mu.Unlock()
}

// SetEmailResolver registers the Slack profile-email lookup behind the soft
// warning on the confirm prompt. Without one the prompt carries no warning.
func (l *SlackLinker) SetEmailResolver(r SlackEmailResolver) {
	l.mu.Lock()
	l.emails = r
	l.mu.Unlock()
}

// emailWarning is the soft hint shown on the confirm prompt when the Slack
// profile email is not the signed-in email. Identity is never matched on
// email (gd-L55f9EYFjN): this only helps the person spot a wrong account
// before pressing Confirm, and "" means nothing to say.
func (l *SlackLinker) emailWarning(ctx context.Context, slackUserID, keycloakEmail string) string {
	l.mu.Lock()
	r := l.emails
	l.mu.Unlock()
	if r == nil {
		return ""
	}
	got, err := r.GetUserEmail(ctx, slackUserID)
	if err != nil || normalizeEmail(got) == "" {
		return ""
	}
	if normalizeEmail(got) == normalizeEmail(keycloakEmail) {
		return ""
	}
	return "Your Slack profile email is " + normalizeEmail(got) + ", not " + keycloakEmail + ". If you did not just sign in as " + keycloakEmail + ", press Cancel."
}

func (l *SlackLinker) authorizeURL() string { return l.cfg.IssuerURL + "/protocol/openid-connect/auth" }
func (l *SlackLinker) tokenURL() string     { return l.cfg.IssuerURL + "/protocol/openid-connect/token" }
func (l *SlackLinker) revokeURL() string    { return l.cfg.IssuerURL + "/protocol/openid-connect/revoke" }
func (l *SlackLinker) callbackURL() string  { return l.cfg.PublicURL + "/slack/link/callback" }

// randomURLSafe returns n random bytes, base64url without padding.
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// seal encrypts plaintext bound to slackUserID (as AEAD additional data, so a
// row copied under another Slack id does not decrypt).
func (l *SlackLinker) seal(slackUserID, plaintext string) ([]byte, error) {
	nonce := make([]byte, l.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := l.aead.Seal(nil, nonce, []byte(plaintext), []byte(slackUserID))
	return append(nonce, out...), nil
}

func (l *SlackLinker) open(slackUserID string, ciphertext []byte) (string, error) {
	ns := l.aead.NonceSize()
	if len(ciphertext) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := l.aead.Open(nil, ciphertext[:ns], ciphertext[ns:], []byte(slackUserID))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// prunePendingLocked drops expired states and, past the cap, the oldest.
func (l *SlackLinker) prunePendingLocked() {
	now := l.now()
	for k, p := range l.pending {
		if now.Sub(p.createdAt) > slackLinkStateTTL {
			delete(l.pending, k)
		}
	}
	for len(l.pending) >= slackLinkMaxPending {
		var oldestK string
		var oldest time.Time
		for k, p := range l.pending {
			if oldestK == "" || p.createdAt.Before(oldest) {
				oldestK, oldest = k, p.createdAt
			}
		}
		delete(l.pending, oldestK)
	}
}

// StartURL creates a single-use, time-bounded link state bound to
// slackUserID and returns the URL the "Link your gardenio account" button
// opens. Only that Slack user was shown the button (ephemeral message), so
// whoever completes the browser flow for this state links THAT Slack id.
func (l *SlackLinker) StartURL(slackUserID, slackTeamID string) (string, error) {
	if strings.TrimSpace(slackUserID) == "" {
		return "", errors.New("slack link: empty slack user id")
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return "", err
	}
	verifier, err := randomURLSafe(32)
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	l.prunePendingLocked()
	l.pending[state] = pendingSlackLink{slackUserID: slackUserID, slackTeamID: slackTeamID, codeVerifier: verifier, createdAt: l.now()}
	l.mu.Unlock()
	return l.cfg.PublicURL + "/slack/link/start?s=" + url.QueryEscape(state), nil
}

// peekPending returns the pending state without consuming it (start).
func (l *SlackLinker) peekPending(state string) (pendingSlackLink, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.pending[state]
	if !ok || l.now().Sub(p.createdAt) > slackLinkStateTTL {
		delete(l.pending, state)
		return pendingSlackLink{}, false
	}
	return p, true
}

// takePending consumes the state (callback): a second callback with the same
// state finds nothing.
func (l *SlackLinker) takePending(state string) (pendingSlackLink, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.pending[state]
	delete(l.pending, state)
	if !ok || l.now().Sub(p.createdAt) > slackLinkStateTTL {
		return pendingSlackLink{}, false
	}
	return p, true
}

// Handler serves /slack/link/start and /slack/link/callback. Mount it at
// /slack/link/ on the bridge's listener; on k3s the viewer-authproxy proxies
// exactly that prefix, without its admin-group auth_request (the state and the
// Keycloak sign-in are the authentication).
func (l *SlackLinker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slack/link/start", l.handleStart)
	mux.HandleFunc("GET /slack/link/callback", l.handleCallback)
	return mux
}

func (l *SlackLinker) handleStart(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("s")
	p, ok := l.peekPending(state)
	if !ok {
		l.logger.Info("slack link start refused: unknown or expired state")
		writeSlackLinkPage(w, http.StatusBadRequest, "Link request unknown or expired", "Go back to Slack and press the Link button again.")
		return
	}
	sum := sha256.Sum256([]byte(p.codeVerifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {l.cfg.ClientID},
		"redirect_uri":          {l.callbackURL()},
		"scope":                 {"openid email offline_access"},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"login"},
	}
	http.Redirect(w, r, l.authorizeURL()+"?"+q.Encode(), http.StatusFound)
}

// tokenResponse is the OIDC token endpoint reply.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// errInvalidGrant marks a refresh that Keycloak refused because the offline
// session is gone (revoked consent, disabled user, idle timeout).
var errInvalidGrant = errors.New("invalid_grant")

func (l *SlackLinker) postToken(ctx context.Context, form url.Values) (*tokenResponse, error) {
	form.Set("client_id", l.cfg.ClientID)
	form.Set("client_secret", l.cfg.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("keycloak token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr tokenResponse
	_ = json.Unmarshal(body, &tr)
	if resp.StatusCode != http.StatusOK {
		if tr.Error == "invalid_grant" {
			return nil, fmt.Errorf("%w: %s", errInvalidGrant, tr.ErrorDesc)
		}
		return nil, fmt.Errorf("keycloak token endpoint: %s %s %s", resp.Status, tr.Error, tr.ErrorDesc)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("keycloak token endpoint: no access_token in reply")
	}
	return &tr, nil
}

// idTokenClaims is the subset of the ID token the link flow uses.
type idTokenClaims struct {
	Issuer  string `json:"iss"`
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Expiry  int64  `json:"exp"`
	Aud     any    `json:"aud"`
	Azp     string `json:"azp"`
	// EmailVerified is kept raw so oidcauth.EmailVerifiedClaim applies the
	// same rule as the API middleware.
	EmailVerified json.RawMessage `json:"email_verified"`
}

// parseIDToken decodes the ID token the TOKEN ENDPOINT returned over TLS with
// client authentication. OIDC Core 3.1.3.7 (6) lets the client skip the
// signature check in exactly that case; iss, aud/azp and exp are still
// verified here so a token endpoint impersonation or a cross-client token is
// refused. The issuer is always Keycloak, which sends email_verified, so the
// claim must be present and true: the email is the identity the Slack user is
// linked to, and an unverified one could name someone else (gd-wVdGvh6VbB).
func (l *SlackLinker) parseIDToken(raw string) (idTokenClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return idTokenClaims{}, errors.New("id_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idTokenClaims{}, fmt.Errorf("id_token payload: %w", err)
	}
	var c idTokenClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return idTokenClaims{}, fmt.Errorf("id_token claims: %w", err)
	}
	if c.Issuer != l.cfg.IssuerURL {
		return idTokenClaims{}, fmt.Errorf("id_token issuer %q is not %q", c.Issuer, l.cfg.IssuerURL)
	}
	audOK := false
	switch a := c.Aud.(type) {
	case string:
		audOK = a == l.cfg.ClientID
	case []any:
		for _, v := range a {
			if s, _ := v.(string); s == l.cfg.ClientID {
				audOK = true
			}
		}
	}
	if !audOK {
		return idTokenClaims{}, errors.New("id_token audience is not this client")
	}
	if c.Azp != "" && c.Azp != l.cfg.ClientID {
		return idTokenClaims{}, errors.New("id_token azp is not this client")
	}
	if c.Expiry > 0 && time.Unix(c.Expiry, 0).Before(l.now()) {
		return idTokenClaims{}, errors.New("id_token expired")
	}
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	if c.Subject == "" || c.Email == "" {
		return idTokenClaims{}, errors.New("id_token has no sub or email")
	}
	if verified, _ := oidcauth.EmailVerifiedClaim(c.EmailVerified); !verified {
		return idTokenClaims{}, errors.New("id_token email_verified is not true")
	}
	return c, nil
}

func (l *SlackLinker) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("s")
	if state == "" {
		state = q.Get("state")
	}
	p, ok := l.takePending(state)
	if !ok {
		l.logger.Info("slack link callback refused: unknown, expired or already used state")
		writeSlackLinkPage(w, http.StatusBadRequest, "Link request unknown, expired or already used", "Go back to Slack and press the Link button again.")
		return
	}
	if e := q.Get("error"); e != "" {
		l.logger.Info("slack link callback: sign-in refused", "user", p.slackUserID, "error", e)
		writeSlackLinkPage(w, http.StatusBadRequest, "Sign-in was not completed", "Go back to Slack and press the Link button again.")
		return
	}
	code := q.Get("code")
	if code == "" {
		writeSlackLinkPage(w, http.StatusBadRequest, "Missing authorization code", "Go back to Slack and press the Link button again.")
		return
	}
	ctx := r.Context()
	tr, err := l.postToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {l.callbackURL()},
		"code_verifier": {p.codeVerifier},
	})
	if err != nil {
		l.logger.Error("slack link callback: code exchange failed", "user", p.slackUserID, "error", err)
		writeSlackLinkPage(w, http.StatusBadGateway, "Sign-in could not be completed", "Keycloak refused the code exchange. Go back to Slack and try again.")
		return
	}
	claims, err := l.parseIDToken(tr.IDToken)
	if err != nil {
		l.logger.Error("slack link callback: id_token rejected", "user", p.slackUserID, "error", err)
		writeSlackLinkPage(w, http.StatusBadGateway, "Sign-in could not be verified", "Go back to Slack and try again.")
		return
	}
	if tr.RefreshToken == "" {
		l.logger.Error("slack link callback: no refresh token (offline_access not granted?)", "user", p.slackUserID, "email", claims.Email)
		writeSlackLinkPage(w, http.StatusBadGateway, "No offline session was granted", "The gardenio-slack-link client must allow the offline_access scope. Ask an admin.")
		return
	}
	// Do not bind or store anything yet: park the fresh tokens and ask the
	// Slack user from the state whether this is the account they mean. Only
	// their Confirm press (ConfirmLink) completes the link.
	confirmID, err := randomURLSafe(32)
	if err != nil {
		l.logger.Error("slack link callback: confirm id", "error", err)
		l.revoke(ctx, tr.RefreshToken)
		writeSlackLinkPage(w, http.StatusInternalServerError, "Could not start the confirmation", "Try again.")
		return
	}
	now := l.now()
	l.mu.Lock()
	expired := l.pruneConfirmsLocked()
	l.confirms[confirmID] = pendingSlackConfirm{
		slackUserID: p.slackUserID, slackTeamID: p.slackTeamID, claims: claims,
		accessToken: tr.AccessToken, refreshToken: tr.RefreshToken, expiresIn: tr.ExpiresIn, createdAt: now,
	}
	hook := l.onConfirm
	l.mu.Unlock()
	l.revokeAll(expired)
	if hook == nil {
		l.logger.Error("slack link callback: no confirm hook configured; refusing (failing closed)", "user", p.slackUserID)
		l.dropConfirm(ctx, confirmID)
		writeSlackLinkPage(w, http.StatusServiceUnavailable, "Not linked", "The Slack confirmation step is not available. Ask an admin.")
		return
	}
	if err := hook(p.slackUserID, confirmID, claims.Email, l.emailWarning(ctx, p.slackUserID, claims.Email)); err != nil {
		l.logger.Error("slack link callback: could not post the confirm prompt", "user", p.slackUserID, "error", err)
		l.dropConfirm(ctx, confirmID)
		writeSlackLinkPage(w, http.StatusBadGateway, "Not linked", "Could not reach you in Slack to confirm. Go back to Slack and press the Link button again.")
		return
	}
	l.logger.Info("slack link awaiting confirmation in slack", "user", p.slackUserID, "signed_in", "oidc:"+claims.Email)
	writeSlackLinkPage(w, http.StatusOK, "Almost done: confirm in Slack", "You signed in as "+claims.Email+". Slack has sent the person who started this link a Confirm / Cancel question; nothing is linked until they confirm. You can close this tab.")
}

// Why a confirm press did not link.
var (
	// ErrLinkConfirmUnknown: no such pending confirm (never issued, already
	// confirmed or cancelled, or expired).
	ErrLinkConfirmUnknown = errors.New("link confirmation unknown, expired or already used")
	// ErrLinkConfirmNotYours: the press came from a Slack user other than the
	// one the link flow was started for. The pending confirm is left intact.
	ErrLinkConfirmNotYours = errors.New("link confirmation belongs to another slack user")
)

// LinkRefusedError is a confirm that reached the daemon and was refused; Reason
// is user-facing.
type LinkRefusedError struct{ Reason string }

func (e *LinkRefusedError) Error() string { return e.Reason }

// pruneConfirmsLocked drops expired confirms and, past the cap, the oldest, and
// returns their refresh tokens for the caller to revoke (outside the lock).
func (l *SlackLinker) pruneConfirmsLocked() []string {
	var out []string
	now := l.now()
	for k, c := range l.confirms {
		if now.Sub(c.createdAt) > slackLinkConfirmTTL {
			out = append(out, c.refreshToken)
			delete(l.confirms, k)
		}
	}
	for len(l.confirms) >= slackLinkMaxPending {
		var oldestK string
		var oldest time.Time
		for k, c := range l.confirms {
			if oldestK == "" || c.createdAt.Before(oldest) {
				oldestK, oldest = k, c.createdAt
			}
		}
		out = append(out, l.confirms[oldestK].refreshToken)
		delete(l.confirms, oldestK)
	}
	return out
}

// revokeAll revokes refresh tokens in the background (best effort).
func (l *SlackLinker) revokeAll(tokens []string) {
	for _, t := range tokens {
		go func(t string) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			l.revoke(ctx, t)
		}(t)
	}
}

// dropConfirm removes a pending confirm and revokes its offline token.
func (l *SlackLinker) dropConfirm(ctx context.Context, confirmID string) {
	l.mu.Lock()
	c, ok := l.confirms[confirmID]
	delete(l.confirms, confirmID)
	l.mu.Unlock()
	if ok {
		l.revoke(ctx, c.refreshToken)
	}
}

// takeConfirm consumes a pending confirm for slackUserID. A press from another
// Slack user leaves it in place. An expired one is consumed and its token
// revoked, then reported as unknown.
func (l *SlackLinker) takeConfirm(ctx context.Context, confirmID, slackUserID string) (pendingSlackConfirm, error) {
	l.mu.Lock()
	c, ok := l.confirms[confirmID]
	switch {
	case !ok:
		l.mu.Unlock()
		return pendingSlackConfirm{}, ErrLinkConfirmUnknown
	case c.slackUserID != slackUserID:
		l.mu.Unlock()
		l.logger.Warn("slack link confirm refused: pressed by a different slack user", "owner", c.slackUserID, "presser", slackUserID)
		return pendingSlackConfirm{}, ErrLinkConfirmNotYours
	}
	delete(l.confirms, confirmID)
	l.mu.Unlock()
	if l.now().Sub(c.createdAt) > slackLinkConfirmTTL {
		l.revoke(ctx, c.refreshToken)
		return pendingSlackConfirm{}, ErrLinkConfirmUnknown
	}
	return c, nil
}

// CancelLink discards a pending confirm: nothing is bound or stored and the
// fresh offline token is revoked. Returns the email that was not linked.
func (l *SlackLinker) CancelLink(ctx context.Context, confirmID, slackUserID string) (string, error) {
	c, err := l.takeConfirm(ctx, confirmID, slackUserID)
	if err != nil {
		return "", err
	}
	l.revoke(ctx, c.refreshToken)
	l.logger.Info("slack link cancelled by the slack user", "user", slackUserID, "signed_in", "oidc:"+c.claims.Email)
	return c.claims.Email, nil
}

// ConfirmLink completes a link: it consumes the pending confirm (so a second
// press finds nothing), records the bind on the person's own user bead under
// the person's token, and stores the sealed offline token. slackUserID is the
// Slack user who pressed Confirm and must be the one the flow was started for.
// Returns the linked email.
func (l *SlackLinker) ConfirmLink(ctx context.Context, confirmID, slackUserID string) (string, error) {
	c, err := l.takeConfirm(ctx, confirmID, slackUserID)
	if err != nil {
		return "", err
	}
	email := c.claims.Email
	// The access token was minted at the code exchange (createdAt) and lives
	// expiresIn seconds, which is shorter than slackLinkConfirmTTL. Past that
	// the daemon would 401 the bind, so refresh first; the expiry cached below
	// always counts from the exchange that produced the token.
	tokenAt := c.createdAt
	expiresIn := c.expiresIn
	if !l.now().Before(tokenAt.Add(time.Duration(expiresIn)*time.Second - slackLinkTokenSkew)) {
		tr, err := l.postToken(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.refreshToken}})
		if err != nil {
			l.logger.Warn("slack link confirm: could not refresh the access token", "user", c.slackUserID, "email", email, "error", err)
			l.revoke(ctx, c.refreshToken)
			return "", &LinkRefusedError{Reason: "Your sign-in expired before you confirmed. Press the Link button again."}
		}
		tokenAt, expiresIn = l.now(), tr.ExpiresIn
		c.accessToken = tr.AccessToken
		// Keycloak rotates refresh tokens only when revokeRefreshToken is on;
		// keep whatever it returned so the stored token is the live one.
		if tr.RefreshToken != "" {
			c.refreshToken = tr.RefreshToken
		}
	}
	// A 409 means the id or bead is bound elsewhere; nothing is stored.
	if _, err := l.daemon.LinkSlackIdentityAsUser(ctx, c.accessToken, beadsapi.SlackIdentityLinkRequest{
		SlackUserID: c.slackUserID, SlackTeamID: c.slackTeamID, LinkedVia: l.cfg.ClientID,
	}); err != nil {
		l.logger.Warn("slack link confirm: daemon refused the bind", "user", c.slackUserID, "email", email, "error", err)
		l.revoke(ctx, c.refreshToken)
		msg := "gardenio refused to bind this Slack account to " + email + "."
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			msg += " It is already bound to someone else, or your user is bound to a different Slack id; ask an admin."
		} else if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			msg += " There is no gardenio user for this email yet; ask an admin to create one."
		}
		return "", &LinkRefusedError{Reason: msg}
	}
	ct, err := l.seal(c.slackUserID, c.refreshToken)
	if err != nil {
		l.logger.Error("slack link confirm: seal failed", "error", err)
		l.revoke(ctx, c.refreshToken)
		return "", &LinkRefusedError{Reason: "Could not store the link. Try again."}
	}
	now := l.now()
	if err := l.store.Put(&SlackLink{
		SlackUserID: c.slackUserID, SlackTeamID: c.slackTeamID,
		Subject: c.claims.Subject, Email: email, Ciphertext: ct, LinkedAt: now,
	}); err != nil {
		l.logger.Error("slack link confirm: store failed", "error", err)
		l.revoke(ctx, c.refreshToken)
		return "", &LinkRefusedError{Reason: "Could not store the link. Try again."}
	}
	l.mu.Lock()
	l.tokens[c.slackUserID] = cachedSlackToken{token: c.accessToken, email: email, expires: tokenAt.Add(time.Duration(expiresIn) * time.Second)}
	l.mu.Unlock()
	l.logger.Info("slack account linked", "user", c.slackUserID, "operator", "oidc:"+email)
	return email, nil
}

// Linked reports whether slackUserID has a stored link and the email it maps to.
func (l *SlackLinker) Linked(slackUserID string) (string, bool) {
	link, ok, err := l.store.Get(slackUserID)
	if err != nil || !ok {
		return "", false
	}
	return link.Email, true
}

// SlackIDsForEmail returns the DISTINCT Slack user ids linked to email
// (case-insensitive). A link is confirmed by the person themselves
// (ConfirmLink), so it is the strongest email -> Slack id evidence the bridge
// holds. The caller decides what to do with 0 or >1 results.
func (l *SlackLinker) SlackIDsForEmail(email string) []string {
	want := normalizeEmail(email)
	if want == "" {
		return nil
	}
	links, err := l.store.List()
	if err != nil {
		l.logger.Warn("slack link store list failed", "error", err)
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, k := range links {
		if normalizeEmail(k.Email) == want && !seen[k.SlackUserID] {
			seen[k.SlackUserID] = true
			out = append(out, k.SlackUserID)
		}
	}
	return out
}

// AccessToken returns a fresh access token (and the person's email) for a
// linked Slack user, refreshing the offline token when the cached one is
// about to expire. ErrSlackNotLinked when there is no link or Keycloak
// refuses the refresh (the link is dropped then, so a revoke in Keycloak
// converges without the bridge being told).
func (l *SlackLinker) AccessToken(ctx context.Context, slackUserID string) (string, string, error) {
	l.mu.Lock()
	if c, ok := l.tokens[slackUserID]; ok && l.now().Before(c.expires.Add(-slackLinkTokenSkew)) {
		l.mu.Unlock()
		return c.token, c.email, nil
	}
	l.mu.Unlock()

	link, ok, err := l.store.Get(slackUserID)
	if err != nil {
		return "", "", fmt.Errorf("slack link store: %w", err)
	}
	if !ok {
		return "", "", ErrSlackNotLinked
	}
	refresh, err := l.open(slackUserID, link.Ciphertext)
	if err != nil {
		l.logger.Error("slack link: stored token does not decrypt (key rotated?); dropping link", "user", slackUserID, "error", err)
		_ = l.store.Delete(slackUserID)
		return "", "", ErrSlackNotLinked
	}
	tr, err := l.postToken(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if err != nil {
		if errors.Is(err, errInvalidGrant) {
			l.logger.Info("slack link revoked at keycloak; dropping link", "user", slackUserID, "operator", "oidc:"+link.Email, "error", err)
			_ = l.store.Delete(slackUserID)
			l.mu.Lock()
			delete(l.tokens, slackUserID)
			l.mu.Unlock()
			return "", "", ErrSlackNotLinked
		}
		return "", "", err
	}
	now := l.now()
	// Keycloak rotates refresh tokens only when revokeRefreshToken is on; keep
	// whatever it returned so a rotated token is never lost.
	if tr.RefreshToken != "" && tr.RefreshToken != refresh {
		if ct, err := l.seal(slackUserID, tr.RefreshToken); err == nil {
			link.Ciphertext = ct
		}
	}
	link.LastUsedAt = now
	if err := l.store.Put(link); err != nil {
		l.logger.Warn("slack link: could not record last use", "user", slackUserID, "error", err)
	}
	l.mu.Lock()
	l.tokens[slackUserID] = cachedSlackToken{token: tr.AccessToken, email: link.Email, expires: now.Add(time.Duration(tr.ExpiresIn) * time.Second)}
	l.mu.Unlock()
	return tr.AccessToken, link.Email, nil
}

// revoke calls the RFC 7009 endpoint for a refresh token; best effort.
func (l *SlackLinker) revoke(ctx context.Context, refreshToken string) {
	form := url.Values{
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {l.cfg.ClientID},
		"client_secret":   {l.cfg.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.revokeURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := l.http.Do(req)
	if err != nil {
		l.logger.Warn("slack link: revoke call failed", "error", err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		l.logger.Warn("slack link: revoke refused", "status", resp.Status)
	}
}

// Unlink removes the person's link: clears identity_link on their user bead
// (as the person, while the token still works), revokes the offline token at
// Keycloak, and deletes the row. The row is deleted even when the first two
// fail, so the bridge never keeps acting after an unlink. Returns the email
// that was linked ("" when nothing was).
func (l *SlackLinker) Unlink(ctx context.Context, slackUserID string) (string, error) {
	link, ok, err := l.store.Get(slackUserID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	if tok, _, err := l.AccessToken(ctx, slackUserID); err == nil {
		if _, err := l.daemon.UnlinkSlackIdentityAsUser(ctx, tok, beadsapi.SlackIdentityLinkRequest{SlackUserID: slackUserID}); err != nil {
			l.logger.Warn("slack unlink: daemon identity_link clear failed (row dropped anyway)", "user", slackUserID, "error", err)
		}
	}
	if refresh, err := l.open(slackUserID, link.Ciphertext); err == nil {
		l.revoke(ctx, refresh)
	}
	l.mu.Lock()
	delete(l.tokens, slackUserID)
	l.mu.Unlock()
	if err := l.store.Delete(slackUserID); err != nil {
		return link.Email, err
	}
	l.logger.Info("slack account unlinked", "user", slackUserID, "operator", "oidc:"+link.Email)
	return link.Email, nil
}

// writeSlackLinkPage renders the tiny result page the browser shows.
func writeSlackLinkPage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title>`+
		`<style>body{font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem;color:#222}h1{font-size:1.4rem}</style>`+
		`</head><body><h1>%s</h1><p>%s</p></body></html>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(body))
}
