package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// TokenRevocationStore is the auth-token revocation blocklist. It combines a
// persistent DB-backed row set (table revoked_tokens) with an in-process cache
// of SHA-256 hashes so the hot path (every authenticated request) stays in
// memory. Runtime revocations go through Revoke, which writes the row first
// and only adds to the cache on success — so the cache never diverges from
// the DB in the direction of "thinks revoked but isn't".
type TokenRevocationStore struct {
	store store.Store

	mu       sync.RWMutex
	revoked  map[string]struct{} // hex(sha256(token)) → present
	onRevoke func(revokedBy, reason string)
}

// NewTokenRevocationStore constructs a store with an empty cache. Callers
// should normally follow up with Load(ctx) to warm the cache from the DB.
func NewTokenRevocationStore(s store.Store) *TokenRevocationStore {
	return &TokenRevocationStore{
		store:   s,
		revoked: make(map[string]struct{}),
	}
}

// Load pulls every persisted revoked-token hash from the DB into the cache.
// Safe to call on startup and after the store has been swapped in.
func (t *TokenRevocationStore) Load(ctx context.Context) error {
	hashes, err := t.store.ListRevokedTokenHashes(ctx)
	if err != nil {
		return fmt.Errorf("list revoked tokens: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, h := range hashes {
		t.revoked[h] = struct{}{}
	}
	return nil
}

// Seed adds the given hashes to the in-memory cache only. Used for the legacy
// BEADS_REVOKED_TOKENS env-var seed, which is operator-managed and must not
// be persisted (otherwise rotating an env seed would leave stale rows in the
// DB forever).
func (t *TokenRevocationStore) Seed(hashes []string) {
	if len(hashes) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, h := range hashes {
		if h == "" {
			continue
		}
		t.revoked[h] = struct{}{}
	}
}

// IsRevokedToken returns whether the SHA-256 hash of the given plaintext
// token appears in the revocation set.
func (t *TokenRevocationStore) IsRevokedToken(token string) bool {
	if t == nil || token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return t.IsRevokedHash(hex.EncodeToString(sum[:]))
}

// IsRevokedHash returns whether the given hex-encoded SHA-256 hash is in the
// revocation set.
func (t *TokenRevocationStore) IsRevokedHash(hash string) bool {
	if t == nil || hash == "" {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.revoked[hash]
	return ok
}

// Size returns the current cache size. Exposed for tests and metrics.
func (t *TokenRevocationStore) Size() int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.revoked)
}

// RevokeHash persists a token-hash revocation and adds it to the cache.
// Returns true if the row was newly inserted (i.e. the hash was not already
// revoked before this call). Callers supply the already-hashed value — the
// plaintext helper RevokeToken is available for convenience.
func (t *TokenRevocationStore) RevokeHash(ctx context.Context, hash, revokedBy, reason string) (bool, error) {
	if hash == "" {
		return false, fmt.Errorf("token hash is required")
	}

	t.mu.RLock()
	_, already := t.revoked[hash]
	t.mu.RUnlock()

	if err := t.store.InsertRevokedToken(ctx, &model.RevokedToken{
		TokenHash: hash,
		RevokedBy: revokedBy,
		Reason:    reason,
	}); err != nil {
		return false, fmt.Errorf("persist revoked token: %w", err)
	}

	t.mu.Lock()
	t.revoked[hash] = struct{}{}
	t.mu.Unlock()

	if !already {
		slog.Warn("token revoked",
			"hash_prefix", hashPrefix(hash),
			"revoked_by", revokedBy,
			"reason", reason,
		)
		if t.onRevoke != nil {
			t.onRevoke(revokedBy, reason)
		}
	}
	return !already, nil
}

// RevokeToken is RevokeHash for a plaintext token. The plaintext is hashed
// locally and never sent to the DB.
func (t *TokenRevocationStore) RevokeToken(ctx context.Context, token, revokedBy, reason string) (bool, error) {
	if token == "" {
		return false, fmt.Errorf("token is required")
	}
	sum := sha256.Sum256([]byte(token))
	return t.RevokeHash(ctx, hex.EncodeToString(sum[:]), revokedBy, reason)
}

// SetOnRevoke registers a callback fired once per newly-inserted revocation.
// Used to bump the Prometheus counter without coupling this store to metrics.
func (t *TokenRevocationStore) SetOnRevoke(fn func(revokedBy, reason string)) {
	t.onRevoke = fn
}

// hashPrefix returns the first 12 chars of a hex-encoded hash for safe logs.
func hashPrefix(hash string) string {
	if len(hash) >= 12 {
		return hash[:12]
	}
	return hash
}
