package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// ClientID is the Codex CLI's public OAuth client id, a literal string in the
	// pinned binary (contract gd-3gTWiNEhg1 §2).
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// DefaultTokenURL is the OAuth token endpoint. The codex binary honours
	// CODEX_REFRESH_TOKEN_URL_OVERRIDE for the same purpose the keeper's tests
	// use TokenURL for: a hermetic endpoint.
	DefaultTokenURL = "https://auth.openai.com/oauth/token"

	maxBody = 1 << 16
)

// Tokens is the refresh response. OpenAI rotates the refresh token on use.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

// RefreshError is a failed refresh. Permanent means OpenAI rejected the
// refresh token itself (reused, expired, revoked, invalid): the caller must
// NEVER retry it, because a retry of a reused token can burn the whole token
// family. Transient failures (network, 5xx, 429, 408) did not reject the token.
type RefreshError struct {
	Status    int
	Code      string // upstream error code, e.g. refresh_token_reused; "" if none
	Permanent bool
	cause     error
}

func (e *RefreshError) Error() string {
	switch {
	case e.cause != nil:
		return "codex token refresh: " + e.cause.Error()
	case e.Code != "":
		return fmt.Sprintf("codex token refresh rejected: HTTP %d (%s)", e.Status, e.Code)
	default:
		return fmt.Sprintf("codex token refresh failed: HTTP %d", e.Status)
	}
}

func (e *RefreshError) Unwrap() error { return e.cause }

// IsPermanent reports whether err is a RefreshError that must not be retried.
func IsPermanent(err error) bool {
	var re *RefreshError
	return errors.As(err, &re) && re.Permanent
}

// Refresher exchanges a refresh token for fresh tokens.
type Refresher struct {
	HTTP     *http.Client
	TokenURL string
}

// TokenURLAllowedHosts is the allowlist for TokenURL overrides.
var TokenURLAllowedHosts = []string{"auth.openai.com"}

// SafeOverrideURL validates a URL override taken from the environment. The refresh
// request body carries the single-use refresh token and the usage request carries
// the access token, so a stray override is a credential-exfiltration vector: it
// is honoured only for https, no userinfo, the default port, and a host in
// allowed. ok is false (and the caller keeps its default) otherwise.
func SafeOverrideURL(raw string, allowed []string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return "", false
	}
	if p := u.Port(); p != "" && p != "443" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range allowed {
		if host == strings.ToLower(h) {
			return u.String(), true
		}
	}
	return "", false
}

// NewRefresher builds a Refresher. An empty tokenURL means DefaultTokenURL.
func NewRefresher(tokenURL string) *Refresher {
	if tokenURL == "" {
		tokenURL = DefaultTokenURL
	}
	return &Refresher{HTTP: &http.Client{Timeout: 30 * time.Second}, TokenURL: tokenURL}
}

// Refresh performs one refresh_token grant. The request body is the shape the
// codex CLI sends (JSON; client_id, grant_type, refresh_token, scope).
func (r *Refresher) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	body, err := json.Marshal(map[string]string{
		"client_id":     ClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"scope":         "openid profile email",
	})
	if err != nil {
		return Tokens{}, &RefreshError{cause: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.TokenURL, bytes.NewReader(body))
	if err != nil {
		return Tokens{}, &RefreshError{cause: errors.New("building request")}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := r.HTTP.Do(req)
	if err != nil {
		// url.Error text includes the URL but never the body; the URL holds no
		// credential.
		return Tokens{}, &RefreshError{cause: err}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))

	if resp.StatusCode != http.StatusOK {
		return Tokens{}, &RefreshError{
			Status:    resp.StatusCode,
			Code:      errorCode(data),
			Permanent: permanentStatus(resp.StatusCode),
		}
	}
	var tr Tokens
	if err := json.Unmarshal(data, &tr); err != nil {
		// 200 but unreadable: the server may have rotated the token and we cannot
		// see the result. Not retryable with the old token either, so permanent.
		return Tokens{}, &RefreshError{Status: resp.StatusCode, Code: "unreadable_response", Permanent: true}
	}
	if tr.AccessToken == "" {
		return Tokens{}, &RefreshError{Status: resp.StatusCode, Code: "no_access_token", Permanent: true}
	}
	return tr, nil
}

// permanentStatus: any 4xx other than request-timeout and rate-limit is the
// server judging the refresh token (400 invalid_grant, 401 reused/expired,
// 403). 5xx, 408 and 429 say nothing about the token.
func permanentStatus(status int) bool {
	if status < 400 || status >= 500 {
		return false
	}
	return status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}

// errorCode extracts the upstream error code from either
// {"error":{"code":"refresh_token_reused"}} or {"error":"invalid_grant"} or a
// top-level {"code":"..."}. Only a short code-shaped value is returned.
func errorCode(body []byte) string {
	var v struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	if code := sanitizeCode(v.Code); code != "" {
		return code
	}
	var asString string
	if json.Unmarshal(v.Error, &asString) == nil {
		return sanitizeCode(asString)
	}
	var asObj struct {
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if json.Unmarshal(v.Error, &asObj) == nil {
		if c := sanitizeCode(asObj.Code); c != "" {
			return c
		}
		return sanitizeCode(asObj.Type)
	}
	return ""
}

// sanitizeCode keeps only short snake-case identifiers, so a body that echoes
// something unexpected can never reach a log line or bead field.
func sanitizeCode(s string) string {
	if len(s) == 0 || len(s) > 64 {
		return ""
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return ""
		}
	}
	return s
}
