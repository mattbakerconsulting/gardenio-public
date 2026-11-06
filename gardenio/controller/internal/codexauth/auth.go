// Package codexauth models the Codex CLI's ChatGPT-subscription auth.json and
// the two upstream calls the controller's refresh keeper makes with it: the
// OAuth refresh at auth.openai.com and the wham/usage limits read (ADR 0004 §C,
// contract gd-3gTWiNEhg1).
//
// Nothing here logs or formats a token. Errors carry HTTP status and the
// upstream error CODE only, never a response body, so a credential cannot leak
// through an error string.
package codexauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// PlaceholderRefreshToken replaces the real refresh token in the pod-facing
	// -session document. A pod-side codex that tries its own refresh with it
	// fails harmlessly: it cannot consume the real single-use refresh token.
	PlaceholderRefreshToken = "gardenio-broker-managed"

	// AuthModeChatGPT is the auth_mode codex writes for a subscription login.
	AuthModeChatGPT = "chatgpt"
)

// Auth is a parsed auth.json. It keeps every key it does not understand so a
// rewrite never drops a field a newer codex added.
type Auth struct {
	top    map[string]json.RawMessage
	tokens map[string]json.RawMessage
}

// Parse reads an auth.json document. It requires the ChatGPT-subscription
// shape: a tokens object carrying an access_token and a refresh_token.
func Parse(data []byte) (*Auth, error) {
	a := &Auth{}
	if err := json.Unmarshal(data, &a.top); err != nil {
		return nil, errors.New("codex auth.json is not a JSON object")
	}
	if raw, ok := a.top["auth_mode"]; ok {
		var mode string
		if err := json.Unmarshal(raw, &mode); err != nil || (mode != "" && !strings.EqualFold(mode, AuthModeChatGPT)) {
			return nil, fmt.Errorf("codex auth.json auth_mode is not %q", AuthModeChatGPT)
		}
	}
	raw, ok := a.top["tokens"]
	if !ok {
		return nil, errors.New("codex auth.json has no tokens object")
	}
	if err := json.Unmarshal(raw, &a.tokens); err != nil || a.tokens == nil {
		return nil, errors.New("codex auth.json tokens is not an object")
	}
	if a.AccessToken() == "" {
		return nil, errors.New("codex auth.json has no access_token")
	}
	if a.RefreshToken() == "" {
		return nil, errors.New("codex auth.json has no refresh_token")
	}
	return a, nil
}

func (a *Auth) tokenString(key string) string {
	raw, ok := a.tokens[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// AccessToken, RefreshToken, IDToken and AccountID read the tokens object.
func (a *Auth) AccessToken() string  { return a.tokenString("access_token") }
func (a *Auth) RefreshToken() string { return a.tokenString("refresh_token") }
func (a *Auth) IDToken() string      { return a.tokenString("id_token") }
func (a *Auth) AccountID() string    { return a.tokenString("account_id") }

// RefreshFingerprint identifies the refresh token without revealing it. The
// keeper records the fingerprint of a token OpenAI rejected, so a re-login (a
// different token in the same Secret) is recognised and a rejected one is never
// retried. 64 bits of SHA-256 is plenty for equality and leaks nothing usable.
func (a *Auth) RefreshFingerprint() string {
	sum := sha256.Sum256([]byte(a.RefreshToken()))
	return hex.EncodeToString(sum[:8])
}

// LastRefresh is the document's last_refresh timestamp; zero when absent or
// unparseable.
func (a *Auth) LastRefresh() time.Time {
	raw, ok := a.top["last_refresh"]
	if !ok {
		return time.Time{}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// AccessExpiry returns the access token JWT's exp claim. ok is false when the
// token is not a decodable JWT with an exp.
func (a *Auth) AccessExpiry() (time.Time, bool) {
	return JWTExpiry(a.AccessToken())
}

// NeedsRefresh reports whether the keeper should rotate now: the access token
// expires within window, its expiry is unknown, or last_refresh is older than
// maxAge. An unreadable expiry refreshes rather than waiting, because the
// alternative is serving a token that may already be dead.
//
// minInterval is a floor that wins over all of that: a document refreshed less
// than minInterval ago is never refreshed again, even when its expiry is
// unreadable. Without it a token whose exp the keeper cannot decode would burn a
// single-use refresh token every pass.
func (a *Auth) NeedsRefresh(now time.Time, window, maxAge, minInterval time.Duration) bool {
	if lr := a.LastRefresh(); !lr.IsZero() && now.Sub(lr) < minInterval {
		return false
	}
	exp, ok := a.AccessExpiry()
	if !ok || exp.Sub(now) < window {
		return true
	}
	if lr := a.LastRefresh(); !lr.IsZero() && now.Sub(lr) > maxAge {
		return true
	}
	return false
}

// Rotated returns a copy of the document carrying the refreshed tokens and
// last_refresh=now. An empty IDToken or RefreshToken in tr keeps the existing
// value (OAuth servers may omit an unchanged one).
func (a *Auth) Rotated(tr Tokens, now time.Time) (*Auth, error) {
	if tr.AccessToken == "" {
		return nil, errors.New("refresh response has no access_token")
	}
	n := &Auth{top: copyRaw(a.top), tokens: copyRaw(a.tokens)}
	set := func(m map[string]json.RawMessage, key, val string) error {
		b, err := json.Marshal(val)
		if err != nil {
			return err
		}
		m[key] = b
		return nil
	}
	if err := set(n.tokens, "access_token", tr.AccessToken); err != nil {
		return nil, err
	}
	if tr.RefreshToken != "" {
		if err := set(n.tokens, "refresh_token", tr.RefreshToken); err != nil {
			return nil, err
		}
	}
	if tr.IDToken != "" {
		if err := set(n.tokens, "id_token", tr.IDToken); err != nil {
			return nil, err
		}
	}
	if err := set(n.top, "last_refresh", now.UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	return n, nil
}

// Session-document whitelists: the only keys a pod-facing document may carry.
var (
	sessionTopKeys   = []string{"auth_mode", "last_refresh"}
	sessionTokenKeys = []string{"access_token", "id_token", "account_id"}
)

// SessionDocument returns the document an agent pod is allowed to hold: a
// whitelist copy (auth_mode, last_refresh, tokens.access_token|id_token|
// account_id) whose refresh_token is the placeholder. Unlike the controller-side
// document it drops every key it does not recognise, so a field a newer codex
// adds (an API key, say) never reaches a pod by default.
func (a *Auth) SessionDocument() *Auth {
	n := &Auth{top: map[string]json.RawMessage{}, tokens: map[string]json.RawMessage{}}
	for _, k := range sessionTopKeys {
		if v, ok := a.top[k]; ok {
			n.top[k] = v
		}
	}
	for _, k := range sessionTokenKeys {
		if v, ok := a.tokens[k]; ok {
			n.tokens[k] = v
		}
	}
	b, _ := json.Marshal(PlaceholderRefreshToken)
	n.tokens["refresh_token"] = b
	return n
}

// Marshal serialises the document. Output is deterministic (sorted keys), so
// "is the -session Secret already current" is a plain byte comparison.
func (a *Auth) Marshal() ([]byte, error) {
	top := copyRaw(a.top)
	tok, err := json.Marshal(a.tokens)
	if err != nil {
		return nil, err
	}
	top["tokens"] = tok
	return json.Marshal(top)
}

func copyRaw(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// JWTExpiry decodes a JWT's exp claim without verifying the signature (the
// keeper only schedules with it; OpenAI is the verifier).
func JWTExpiry(jwt string) (time.Time, bool) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0), true
}
