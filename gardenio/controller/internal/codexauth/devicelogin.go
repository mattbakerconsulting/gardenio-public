package codexauth

// devicelogin.go — the OAuth device-code flow `codex login --device-auth` runs,
// spoken by the controller so a login never exists in a pod (ADR 0007).
//
// Wire shapes are those of the pinned codex binary (login/src/device_code_auth.rs):
//
//	POST {issuer}/api/accounts/deviceauth/usercode  {client_id}
//	  -> {device_auth_id, user_code (alias usercode), interval}
//	POST {issuer}/api/accounts/deviceauth/token     {device_auth_id, user_code}
//	  -> 200 {authorization_code, code_challenge, code_verifier}; 403/404 = not approved yet
//	POST {issuer}/oauth/token (form) grant_type=authorization_code, code,
//	  redirect_uri={issuer}/deviceauth/callback, client_id, code_verifier
//	  -> {id_token, access_token, refresh_token}
//
// The user visits {issuer}/codex/device and types user_code.
//
// Like the rest of the package, nothing here logs or formats a token or a user
// code, and errors carry an HTTP status and a sanitised upstream error code only.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultIssuer is the OpenAI auth origin the device flow lives under.
	DefaultIssuer = "https://auth.openai.com"

	// DeviceCodeLifetime is how long a user code stays valid (observed: 15 min).
	DeviceCodeLifetime = 15 * time.Minute

	defaultPollInterval = 5 * time.Second
	minPollInterval     = time.Second
)

// IssuerAllowedHosts is the allowlist for issuer overrides (the exchange body
// carries the authorization code and PKCE verifier).
var IssuerAllowedHosts = []string{"auth.openai.com"}

// DeviceCode is a started device login. UserCode and VerificationURL are shown to
// the operator; DeviceAuthID is the poll handle. Treat the whole value as secret
// until approved: it must stay in memory and off every log, bead and agent.
type DeviceCode struct {
	DeviceAuthID    string
	UserCode        string
	VerificationURL string
	Interval        time.Duration
	ExpiresAt       time.Time
}

// DeviceLoginError is a failed device-flow call. It never carries a body.
type DeviceLoginError struct {
	Stage  string // usercode | poll | exchange
	Status int
	Code   string
	cause  error
}

func (e *DeviceLoginError) Error() string {
	switch {
	case e.cause != nil:
		return fmt.Sprintf("codex device login (%s): %v", e.Stage, e.cause)
	case e.Code != "":
		return fmt.Sprintf("codex device login (%s): HTTP %d (%s)", e.Stage, e.Status, e.Code)
	default:
		return fmt.Sprintf("codex device login (%s): HTTP %d", e.Stage, e.Status)
	}
}

func (e *DeviceLoginError) Unwrap() error { return e.cause }

// ErrDeviceCodeExpired is returned by Poll when the code lapsed unapproved.
var ErrDeviceCodeExpired = errors.New("codex device code expired before approval")

// DeviceLogin runs the device flow against Issuer.
type DeviceLogin struct {
	HTTP   *http.Client
	Issuer string
	Now    func() time.Time
	// Sleep waits d or until ctx ends; injectable for tests.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewDeviceLogin builds a DeviceLogin. An empty issuer means DefaultIssuer.
func NewDeviceLogin(issuer string) *DeviceLogin {
	if issuer == "" {
		issuer = DefaultIssuer
	}
	return &DeviceLogin{
		HTTP:   &http.Client{Timeout: 30 * time.Second},
		Issuer: strings.TrimRight(issuer, "/"),
		Now:    time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

func (d *DeviceLogin) do(ctx context.Context, stage, path, contentType string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Issuer+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, &DeviceLoginError{Stage: stage, cause: errors.New("building request")}
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return 0, nil, &DeviceLoginError{Stage: stage, cause: err}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, data, nil
}

// Start requests a device code.
func (d *DeviceLogin) Start(ctx context.Context) (DeviceCode, error) {
	body, _ := json.Marshal(map[string]string{"client_id": ClientID})
	status, data, err := d.do(ctx, "usercode", "/api/accounts/deviceauth/usercode", "application/json", body)
	if err != nil {
		return DeviceCode{}, err
	}
	if status != http.StatusOK {
		return DeviceCode{}, &DeviceLoginError{Stage: "usercode", Status: status, Code: errorCode(data)}
	}
	var r struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		UserCodeAlt  string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"`
	}
	if json.Unmarshal(data, &r) != nil {
		return DeviceCode{}, &DeviceLoginError{Stage: "usercode", Status: status, Code: "unreadable_response"}
	}
	if r.UserCode == "" {
		r.UserCode = r.UserCodeAlt
	}
	if r.DeviceAuthID == "" || r.UserCode == "" {
		return DeviceCode{}, &DeviceLoginError{Stage: "usercode", Status: status, Code: "incomplete_response"}
	}
	return DeviceCode{
		DeviceAuthID:    r.DeviceAuthID,
		UserCode:        r.UserCode,
		VerificationURL: d.Issuer + "/codex/device",
		Interval:        parseInterval(r.Interval),
		ExpiresAt:       d.Now().Add(DeviceCodeLifetime),
	}, nil
}

// parseInterval accepts the poll interval as a JSON number or numeric string.
func parseInterval(raw json.RawMessage) time.Duration {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	secs, err := strconv.ParseFloat(s, 64)
	if err != nil || secs <= 0 {
		return defaultPollInterval
	}
	iv := time.Duration(secs * float64(time.Second))
	if iv < minPollInterval {
		return minPollInterval
	}
	return iv
}

type approval struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
}

// Poll waits for the operator to approve dc, then exchanges the approval for
// tokens. It returns ErrDeviceCodeExpired at dc.ExpiresAt and ctx.Err() when
// cancelled. 403/404 mean "not approved yet"; 429 and slow_down back the
// interval off; any other non-200 is a hard failure.
func (d *DeviceLogin) Poll(ctx context.Context, dc DeviceCode) (Tokens, error) {
	interval := dc.Interval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	body, _ := json.Marshal(map[string]string{"device_auth_id": dc.DeviceAuthID, "user_code": dc.UserCode})
	for {
		if !d.Now().Before(dc.ExpiresAt) {
			return Tokens{}, ErrDeviceCodeExpired
		}
		status, data, err := d.do(ctx, "poll", "/api/accounts/deviceauth/token", "application/json", body)
		if err != nil {
			if ctx.Err() != nil {
				return Tokens{}, ctx.Err()
			}
			// A transient network error says nothing about the code; keep polling.
			if err := d.Sleep(ctx, interval); err != nil {
				return Tokens{}, err
			}
			continue
		}
		switch {
		case status == http.StatusOK:
			var ap approval
			if json.Unmarshal(data, &ap) != nil || ap.AuthorizationCode == "" || ap.CodeVerifier == "" {
				return Tokens{}, &DeviceLoginError{Stage: "poll", Status: status, Code: "incomplete_response"}
			}
			return d.exchange(ctx, ap)
		case status == http.StatusForbidden || status == http.StatusNotFound:
			// not approved yet
		case status == http.StatusTooManyRequests || errorCode(data) == "slow_down":
			interval += 5 * time.Second
		default:
			return Tokens{}, &DeviceLoginError{Stage: "poll", Status: status, Code: errorCode(data)}
		}
		if err := d.Sleep(ctx, interval); err != nil {
			return Tokens{}, err
		}
	}
}

func (d *DeviceLogin) exchange(ctx context.Context, ap approval) (Tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {ap.AuthorizationCode},
		"redirect_uri":  {d.Issuer + "/deviceauth/callback"},
		"client_id":     {ClientID},
		"code_verifier": {ap.CodeVerifier},
	}
	status, data, err := d.do(ctx, "exchange", "/oauth/token", "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	if status != http.StatusOK {
		return Tokens{}, &DeviceLoginError{Stage: "exchange", Status: status, Code: errorCode(data)}
	}
	var tr Tokens
	if json.Unmarshal(data, &tr) != nil {
		return Tokens{}, &DeviceLoginError{Stage: "exchange", Status: status, Code: "unreadable_response"}
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" {
		// A login without a refresh token cannot be kept alive by the keeper.
		return Tokens{}, &DeviceLoginError{Stage: "exchange", Status: status, Code: "incomplete_response"}
	}
	return tr, nil
}

// ChatGPTAccountID reads chatgpt_account_id from an id_token's
// https://api.openai.com/auth claim, without verifying the signature (OpenAI is
// the verifier; the controller received the token directly from it over TLS).
func ChatGPTAccountID(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}

// NewAuth builds a fresh auth.json from a completed login, in the shape codex
// writes (auth_mode chatgpt, OPENAI_API_KEY null, tokens incl. account_id).
func NewAuth(tr Tokens, now time.Time) (*Auth, error) {
	if tr.AccessToken == "" || tr.RefreshToken == "" {
		return nil, errors.New("login has no access_token or refresh_token")
	}
	doc := map[string]any{
		"auth_mode":      AuthModeChatGPT,
		"OPENAI_API_KEY": nil,
		"tokens": map[string]string{
			"id_token":      tr.IDToken,
			"access_token":  tr.AccessToken,
			"refresh_token": tr.RefreshToken,
			"account_id":    ChatGPTAccountID(tr.IDToken),
		},
		"last_refresh": now.UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}
