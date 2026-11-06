package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// DefaultRefreshTTL is the lifetime applied to a refresh token when the
// caller does not specify one. 30 days matches ADR §6.3 — long enough to
// cover a typical agent runtime without forcing daily re-issuance, short
// enough that a leaked refresh has a bounded blast radius.
const DefaultRefreshTTL = 30 * 24 * time.Hour

// DefaultAccessTTLFromRefresh is the lifetime applied to an access token
// minted by exchanging a refresh, when the caller does not pass an explicit
// TTL. 24h matches the PAT default in ADR §6.3.
const DefaultAccessTTLFromRefresh = 24 * time.Hour

// ErrRefreshNotFound is returned when no row matches the supplied refresh
// token. Callers translate this to 401 (deliberately indistinguishable from
// "expired" / "revoked" so a stolen-but-already-revoked refresh leaks no
// information).
var ErrRefreshNotFound = errors.New("refresh token not found")

// ErrRefreshExpired is returned when the row exists but its expires_at is
// in the past.
var ErrRefreshExpired = errors.New("refresh token expired")

// ErrRefreshRevoked is returned when the row exists but revoked_at is set.
var ErrRefreshRevoked = errors.New("refresh token revoked")

// refreshTokenScopes is the JSON-serialised payload stored in
// refresh_tokens.scopes_json. We snapshot the access-token scopes carried
// by the original issuance so a future exchange can mint an access token
// with identical scopes.
type refreshTokenScopes struct {
	AllowedTypes []string `json:"allowed_types,omitempty"`
	AllowedOps   []string `json:"allowed_ops,omitempty"`
	// Privileged marks the access tokens minted from this refresh as a
	// privileged service-fleet identity (ownership-bypass for system-managed
	// graph writes). Snapshotted here so a refresh exchange re-mints an
	// access token with the same privilege. See ka-0000001946.
	Privileged bool `json:"privileged,omitempty"`
}

// RefreshTokenManager owns refresh-token issuance, exchange, and
// revocation. It composes the persistent store with the in-memory
// FleetConfig so that exchanging a refresh produces an access token that
// is immediately accepted by the auth interceptor.
//
// The manager keeps an in-memory map from refresh-hash → set of
// access-token plaintexts it has minted, so a refresh revocation can also
// purge any descendant access tokens from FleetConfig (the cascade
// requirement). The map is intentionally NOT persisted: access tokens
// already carry an ExpiresAt, so a daemon restart loses at most one
// access-TTL window of cascade information for a refresh that was
// concurrently revoked from another daemon — a non-goal in single-daemon
// kbeads.
type RefreshTokenManager struct {
	store    store.Store
	fleetCfg *FleetConfig
	now      func() time.Time

	mu              sync.Mutex
	children        map[string][]string // refresh-hash → access-token plaintexts
	refreshChildren map[string][]string // refresh-hash → child refresh-hashes (rotation chain)
}

// NewRefreshTokenManager constructs a manager bound to a store + fleet
// config. Either argument may be nil for tests; nil store disables
// persistence (issuance returns ErrRefreshNotFound on exchange).
func NewRefreshTokenManager(s store.Store, fc *FleetConfig) *RefreshTokenManager {
	return &RefreshTokenManager{
		store:           s,
		fleetCfg:        fc,
		now:             time.Now,
		children:        make(map[string][]string),
		refreshChildren: make(map[string][]string),
	}
}

// SetNowFunc overrides the clock. Test-only.
func (m *RefreshTokenManager) SetNowFunc(f func() time.Time) {
	if m == nil || f == nil {
		return
	}
	m.now = f
}

// IssueRequest carries everything needed to mint a new refresh token. All
// fields are copied into the persisted row; the returned plaintext is the
// only place the secret exists.
type IssueRequest struct {
	Fleet             string
	Project           string
	Subject           string
	AllowedTypes      []string
	AllowedOps        []string
	TTL               time.Duration
	ParentRefreshHash string // empty for first-issue; set on rotation
	// Privileged snapshots the privileged service-fleet flag onto the
	// refresh token's scopes so a future exchange re-mints a privileged
	// access token. See ka-0000001946.
	Privileged bool
}

// IssueResult is what Issue returns: the plaintext refresh (returned to
// the caller, never stored) plus the persisted row.
type IssueResult struct {
	Plaintext string
	Row       *model.RefreshToken
}

// Issue mints a new refresh token, persists it, and returns the plaintext.
func (m *RefreshTokenManager) Issue(ctx context.Context, req IssueRequest) (*IssueResult, error) {
	if m == nil || m.store == nil {
		return nil, errors.New("refresh manager not initialised")
	}
	if req.Fleet == "" {
		return nil, errors.New("fleet is required")
	}
	plaintext, err := generateRefreshSecret()
	if err != nil {
		return nil, fmt.Errorf("generate refresh secret: %w", err)
	}
	hash := hashRefreshToken(plaintext)
	ttl := req.TTL
	if ttl <= 0 {
		ttl = DefaultRefreshTTL
	}
	scopes := refreshTokenScopes{
		AllowedTypes: req.AllowedTypes,
		AllowedOps:   req.AllowedOps,
		Privileged:   req.Privileged,
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return nil, fmt.Errorf("marshal scopes: %w", err)
	}
	now := m.now().UTC()
	row := &model.RefreshToken{
		TokenHash:         hash,
		Fleet:             req.Fleet,
		Project:           req.Project,
		ScopesJSON:        scopesJSON,
		Subject:           req.Subject,
		CreatedAt:         now,
		ExpiresAt:         now.Add(ttl),
		ParentRefreshHash: req.ParentRefreshHash,
	}
	if err := m.store.InsertRefreshToken(ctx, row); err != nil {
		return nil, fmt.Errorf("persist refresh token: %w", err)
	}
	if req.ParentRefreshHash != "" {
		m.recordRefreshChild(req.ParentRefreshHash, hash)
	}
	return &IssueResult{Plaintext: plaintext, Row: row}, nil
}

// ExchangeOptions controls Exchange behaviour at the call site.
type ExchangeOptions struct {
	// Rotate, when true, mints a NEW refresh token and revokes the old one
	// (RFC 6749 §10.4 single-use rotation). When false, the original refresh
	// stays valid until expiry.
	Rotate bool
	// AccessTTL is the lifetime of the minted access token. Zero defaults to
	// DefaultAccessTTLFromRefresh.
	AccessTTL time.Duration
}

// ExchangeResult reports what Exchange handed back to the caller.
type ExchangeResult struct {
	AccessToken     string
	AccessExpiresAt time.Time
	// NewRefreshToken is non-empty when Rotate=true; clients must replace
	// their stored refresh on next use.
	NewRefreshToken string
	NewRefreshRow   *model.RefreshToken
}

// Exchange validates a refresh-token plaintext and mints a new access
// token. When Rotate=true, also mints a fresh refresh and revokes the
// presented one; the new refresh's parent_refresh_hash points at the
// presented hash so RevokeRefreshTokenChain can cascade.
func (m *RefreshTokenManager) Exchange(ctx context.Context, plaintext string, opts ExchangeOptions) (*ExchangeResult, error) {
	if m == nil || m.store == nil {
		return nil, ErrRefreshNotFound
	}
	if plaintext == "" {
		return nil, ErrRefreshNotFound
	}
	hash := hashRefreshToken(plaintext)
	row, err := m.store.GetRefreshToken(ctx, hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRefreshNotFound
		}
		return nil, fmt.Errorf("load refresh token: %w", err)
	}
	if row.RevokedAt != nil {
		return nil, ErrRefreshRevoked
	}
	now := m.now().UTC()
	if !row.ExpiresAt.IsZero() && now.After(row.ExpiresAt) {
		return nil, ErrRefreshExpired
	}
	var scopes refreshTokenScopes
	if len(row.ScopesJSON) > 0 {
		_ = json.Unmarshal(row.ScopesJSON, &scopes)
	}

	accessTTL := opts.AccessTTL
	if accessTTL <= 0 {
		accessTTL = DefaultAccessTTLFromRefresh
	}
	accessPlaintext, err := generateRefreshSecret()
	if err != nil {
		return nil, fmt.Errorf("generate access token: %w", err)
	}
	accessExpiresAt := now.Add(accessTTL)

	if m.fleetCfg != nil {
		// Propagate the refresh-token row's Subject onto the minted access
		// token so audit attribution survives the refresh exchange (C1.4).
		// Empty Subject falls back to fleet name in the auth interceptor.
		// The privileged service-fleet flag (ka-0000001946) is snapshotted in
		// the refresh scopes and re-applied so the re-minted access token
		// keeps its ownership-bypass.
		m.fleetCfg.RegisterTokenFull(
			accessPlaintext, row.Fleet, row.Project,
			scopes.AllowedTypes, scopes.AllowedOps, accessExpiresAt,
			row.Subject, scopes.Privileged,
		)
	}

	res := &ExchangeResult{
		AccessToken:     accessPlaintext,
		AccessExpiresAt: accessExpiresAt,
	}

	// Track the access token under the presented refresh's hash so a later
	// revoke can cascade to it. We attribute access tokens to the ORIGINAL
	// refresh-hash even when we rotate, because that's the hash whose
	// chain RevokeRefreshTokenChain walks.
	m.recordChild(hash, accessPlaintext)

	if opts.Rotate {
		// Revoke the presented refresh BEFORE issuing the replacement, so
		// the chain-walk in RevokeRefreshTokenChain does not also catch the
		// new refresh (which would otherwise be a descendant via
		// parent_refresh_hash). At this point no descendants exist for the
		// presented hash, so the cascade is a no-op beyond the row itself.
		if _, err := m.store.RevokeRefreshTokenChain(ctx, hash, now); err != nil {
			return nil, fmt.Errorf("revoke rotated refresh: %w", err)
		}
		issue, err := m.Issue(ctx, IssueRequest{
			Fleet:             row.Fleet,
			Project:           row.Project,
			Subject:           row.Subject,
			AllowedTypes:      scopes.AllowedTypes,
			AllowedOps:        scopes.AllowedOps,
			TTL:               time.Until(row.ExpiresAt),
			ParentRefreshHash: hash,
			Privileged:        scopes.Privileged,
		})
		if err != nil {
			return nil, fmt.Errorf("rotate refresh: %w", err)
		}
		res.NewRefreshToken = issue.Plaintext
		res.NewRefreshRow = issue.Row
	}

	return res, nil
}

// Revoke marks the given refresh-token plaintext (and all rotation
// descendants) as revoked, and unregisters any access tokens minted from
// it from the in-memory FleetConfig. Returns the number of refresh-token
// rows newly marked revoked.
func (m *RefreshTokenManager) Revoke(ctx context.Context, plaintext string) (int, error) {
	if m == nil || m.store == nil {
		return 0, ErrRefreshNotFound
	}
	if plaintext == "" {
		return 0, ErrRefreshNotFound
	}
	return m.RevokeByHash(ctx, hashRefreshToken(plaintext))
}

// RevokeByHash is Revoke but accepts a hex-SHA256 hash directly (operator
// tooling that revokes by hash rather than plaintext).
func (m *RefreshTokenManager) RevokeByHash(ctx context.Context, hash string) (int, error) {
	if m == nil || m.store == nil {
		return 0, ErrRefreshNotFound
	}
	if hash == "" {
		return 0, ErrRefreshNotFound
	}
	now := m.now().UTC()
	n, err := m.store.RevokeRefreshTokenChain(ctx, hash, now)
	if err != nil {
		return 0, fmt.Errorf("revoke refresh chain: %w", err)
	}
	// Cascade to access tokens minted under this hash (and descendants).
	m.cascadeChildren(hash)
	return n, nil
}

// RevokeFleet revokes every unrevoked refresh token issued to the fleet, with
// the same rotation-chain and access-token cascade as RevokeByHash. Returns the
// number of refresh-token rows newly marked revoked. A nil manager or store is a
// no-op (nothing was ever issued). It keeps going past a failed hash so one bad
// row cannot shield the others, and returns the first error.
func (m *RefreshTokenManager) RevokeFleet(ctx context.Context, fleet string) (int, error) {
	if m == nil || m.store == nil || fleet == "" {
		return 0, nil
	}
	hashes, err := m.store.ListActiveRefreshTokenHashesByFleet(ctx, fleet)
	if err != nil {
		return 0, fmt.Errorf("list refresh tokens for fleet: %w", err)
	}
	total := 0
	var firstErr error
	for _, h := range hashes {
		n, err := m.RevokeByHash(ctx, h)
		total += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return total, firstErr
}

func (m *RefreshTokenManager) recordChild(refreshHash, accessPlaintext string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.children[refreshHash] = append(m.children[refreshHash], accessPlaintext)
}

func (m *RefreshTokenManager) recordRefreshChild(parentHash, childHash string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshChildren[parentHash] = append(m.refreshChildren[parentHash], childHash)
}

// cascadeChildren removes from the in-memory FleetConfig every access token
// minted under refreshHash OR any rotation descendant of refreshHash. Mirrors
// the SQL recursive-CTE walk performed by RevokeRefreshTokenChain so that a
// revoked refresh chain leaves no usable access tokens behind.
func (m *RefreshTokenManager) cascadeChildren(refreshHash string) {
	if m == nil || m.fleetCfg == nil {
		return
	}
	m.mu.Lock()
	queue := []string{refreshHash}
	visited := map[string]bool{refreshHash: true}
	var tokens []string
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		tokens = append(tokens, m.children[h]...)
		delete(m.children, h)
		for _, child := range m.refreshChildren[h] {
			if !visited[child] {
				visited[child] = true
				queue = append(queue, child)
			}
		}
		delete(m.refreshChildren, h)
	}
	m.mu.Unlock()
	for _, tok := range tokens {
		m.fleetCfg.removeToken(tok)
	}
}

// generateRefreshSecret returns a 32-byte cryptographically random hex
// string (64 hex chars). Mirrors the pattern in
// gardenio/controller/cmd/controller/agenttoken.go.
func generateRefreshSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// hashRefreshToken returns the lowercase hex SHA-256 of the plaintext.
// Matches the storage convention used by revoked_tokens (C3 pattern).
func hashRefreshToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
