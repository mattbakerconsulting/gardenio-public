package server

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
)

// revokeTokenRequest is the JSON body for POST /v1/admin/revoke-token.
// Exactly one of token_hash or token must be supplied — token_hash is
// preferred for operator tooling so the plaintext never crosses the wire.
type revokeTokenRequest struct {
	TokenHash string `json:"token_hash"`
	Token     string `json:"token"`
	Reason    string `json:"reason"`
}

// revokeTokenResponse is returned by POST /v1/admin/revoke-token.
type revokeTokenResponse struct {
	TokenHashPrefix string `json:"token_hash_prefix"`
	AlreadyRevoked  bool   `json:"already_revoked"`
	Revoked         bool   `json:"revoked"`
}

// handleRevokeToken persists a revoked-token hash to the DB so it survives
// daemon restarts, updates the in-process cache so subsequent requests are
// rejected immediately, bumps the revocation counter, and writes an audit
// bead. Admin-only.
func (s *BeadsServer) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if s.revocation == nil {
		writeError(w, http.StatusServiceUnavailable, "token revocation store not initialised")
		return
	}

	var req revokeTokenRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	hash := strings.TrimSpace(req.TokenHash)
	if hash == "" && req.Token != "" {
		sum := sha256.Sum256([]byte(req.Token))
		hash = hex.EncodeToString(sum[:])
	}
	if hash == "" {
		writeError(w, http.StatusBadRequest, "token_hash or token is required")
		return
	}
	if !isHexSHA256(hash) {
		writeError(w, http.StatusBadRequest, "token_hash must be a 64-character hex-encoded SHA-256 digest")
		return
	}

	ctx := r.Context()
	actor := AuthIdentityFromContext(ctx).Subject
	if actor == "" {
		actor = "admin"
	}

	inserted, err := s.revocation.RevokeHash(ctx, hash, actor, req.Reason)
	if err != nil {
		writeServerError(ctx, w, "handleRevokeToken", err, "failed to persist token revocation")
		return
	}

	if inserted {
		tokenRevocationsTotal.Inc()
		revokedTokensCurrent.Set(float64(s.revocation.Size()))
		s.recordAuditEntry(ctx, auditEntry{
			Actor:        actor,
			Action:       "revoke_token",
			Resource:     hashPrefix(hash),
			ResourceType: "auth_token",
			Outcome:      "allow",
			Reason:       req.Reason,
			AuthMethod:   AuthIdentityFromContext(ctx).Type,
			SourceIP:     clientIP(r, s.trustedProxies),
		})
	} else {
		slog.Info("revoke-token no-op: hash already revoked", "hash_prefix", hashPrefix(hash))
	}

	writeJSON(w, http.StatusOK, &revokeTokenResponse{
		TokenHashPrefix: hashPrefix(hash),
		AlreadyRevoked:  !inserted,
		Revoked:         true,
	})
}

// isHexSHA256 returns true if s is a 64-character lowercase hex string.
func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
