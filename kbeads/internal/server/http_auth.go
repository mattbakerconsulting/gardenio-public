package server

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// refreshRequest is the JSON body for POST /v1/auth/refresh.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// refreshResponse is what POST /v1/auth/refresh returns. RefreshToken is
// only populated when the rotation policy mints a new refresh on each
// exchange (RFC 6749 §10.4 single-use refresh).
type refreshResponse struct {
	AccessToken          string `json:"access_token"`
	ExpiresIn            int    `json:"expires_in"` // seconds until access-token expiry
	TokenType            string `json:"token_type"` // always "Bearer"
	RefreshToken         string `json:"refresh_token,omitempty"`
	RefreshTokenRotated  bool   `json:"refresh_token_rotated,omitempty"`
	RefreshTokenExpiresIn int   `json:"refresh_token_expires_in,omitempty"`
}

// handleRefreshToken handles POST /v1/auth/refresh.
//
// Exchanges a refresh token for a new access token (and, when rotation is
// enabled, a new refresh token). Authentication for this endpoint is the
// refresh-token plaintext itself — the request must NOT carry a Bearer
// access-token header. RBAC is bypassed for this exchange because the
// refresh token's own scopes are inherited by the minted access token.
//
// Errors are deliberately uniform (401 + WWW-Authenticate) so that a
// stolen-but-revoked refresh leaks no information about whether the
// secret was correct.
func (s *BeadsServer) handleRefreshToken(w http.ResponseWriter, r *http.Request) {
	if s.refreshTokens == nil {
		writeError(w, http.StatusServiceUnavailable, "refresh-token manager not configured")
		return
	}
	var req refreshRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	req.RefreshToken = strings.TrimSpace(req.RefreshToken)
	if req.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "refresh_token is required")
		return
	}

	res, err := s.refreshTokens.Exchange(r.Context(), req.RefreshToken, ExchangeOptions{
		Rotate: s.refreshRotate,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrRefreshNotFound),
			errors.Is(err, ErrRefreshRevoked):
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_grant", error_description="refresh token invalid"`)
			writeError(w, http.StatusUnauthorized, "refresh token invalid")
			return
		case errors.Is(err, ErrRefreshExpired):
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_grant", error_description="refresh token expired"`)
			writeError(w, http.StatusUnauthorized, "refresh token expired")
			return
		}
		writeServerError(r.Context(), w, "handleRefreshToken", err, "failed to exchange refresh token")
		return
	}

	resp := refreshResponse{
		AccessToken: res.AccessToken,
		ExpiresIn:   int(time.Until(res.AccessExpiresAt).Round(time.Second).Seconds()),
		TokenType:   "Bearer",
	}
	if res.NewRefreshToken != "" {
		resp.RefreshToken = res.NewRefreshToken
		resp.RefreshTokenRotated = true
		if res.NewRefreshRow != nil {
			resp.RefreshTokenExpiresIn = int(time.Until(res.NewRefreshRow.ExpiresAt).Round(time.Second).Seconds())
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
