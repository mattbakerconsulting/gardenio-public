// Package oidclogin is the Keycloak sign-in shared by kd and gb: the OAuth 2.0
// device authorization grant (RFC 8628) against the public gardenio-cli client,
// the session cache it writes, and the refresh that keeps it alive.
//
// The person runs `kd login` or `gb login`; the CLI prints a verification URL
// and a short code and the person completes the sign-in in their own browser,
// so no password (or MFA secret) ever passes through the CLI. The resulting
// access token carries the person's realm roles; the CLI sends it as the
// Bearer token and refreshes it when it expires.
//
// Both CLIs read and write the same file, so one sign-in serves both. The
// session is bound to the host of the daemon it was minted for and is never
// sent anywhere else.
//
// The session holds live credentials: the file is written 0600 and its token
// fields are never printed or logged.
package oidclogin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	DefaultIssuer   = "https://auth.clonk.tv/auth/realms/gardenio"
	DefaultClientID = "gardenio-cli"
	Scope           = "openid email profile"

	// A cached access token is refreshed this long before it expires so a
	// request in flight never carries a token that lapses mid-call.
	ExpirySkew = 30 * time.Second

	HTTPTimeout = 30 * time.Second

	// A refresh that fails for any reason other than Keycloak refusing the
	// grant (the edge answering 502 while Keycloak restarts, a reset
	// connection, a timeout) is retried this many times with a doubling
	// backoff: 2s then 4s, about 6s in all. Long enough to ride out a blip,
	// short enough that the CLI never hangs for a Keycloak restart
	// (gd-NrzwHXUxV0).
	RefreshAttempts = 3
	RefreshBackoff  = 2 * time.Second

	// NoSessionEnv set to 1 keeps Bearer from sending any cached sign-in.
	// gl agent start sets it for a land agent, which runs kd and gb under its
	// own token in the same home as its person: without it their session for
	// the land, which both CLIs send before any token in the environment,
	// would make the agent act as them (gd-QV2WuUjely, gd-6yW5zvZSbz).
	NoSessionEnv = "GARDENIO_NO_LOGIN_SESSION"
)

// RetrySleep is time.Sleep; a var so tests do not wait out the backoff.
var RetrySleep = time.Sleep

// FilePath is where the signed-in session is cached. A var so tests can point
// it at a temp dir.
var FilePath = DefaultFilePath

func DefaultFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "beads", "oidc-login.json"), nil
}

// Session is the cached result of a device-grant sign-in. The JSON field names
// are a contract: kd and gb binaries of every version read this file.
type Session struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	// Daemon is the host of the daemon this session was minted for. Bearer
	// sends the token only to that host, so a CLI pointed at another daemon
	// never receives it (gd-bFFquoScHy).
	Daemon string `json:"daemon"`
	// Land is the land this session was minted for: its token carries the
	// one audience land-<Land> (owner decision gd-zf6eceweED) and is sent
	// only through that land's door (a daemon URL ending /land/<Land>). ""
	// for an ordinary session. Each land's session has its own file
	// (SessionPath), so signing in for a land never replaces another.
	Land         string    `json:"land,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// LandAudiencePrefix prefixes a land's client scope and the audience it adds:
// scope land-acme puts aud land-acme in the token.
const LandAudiencePrefix = "land-"

// landPath matches a land door URL path, /land/<name> with anything after
// it; <name> is the Lands contract's LandName, as the edge captures it.
var landPath = regexp.MustCompile(`^/land/([a-z][a-z0-9-]{0,62})(/|$)`)

// landName is the Lands contract's LandName.
var landName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// LandFromURL returns the land a daemon URL goes through the land door for
// (https://api.clonk.tv/land/acme -> "acme"), or "" for any other URL.
func LandFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if m := landPath.FindStringSubmatch(u.EscapedPath()); m != nil {
		return m[1]
	}
	return ""
}

// ScopeFor is the scope a sign-in requests: Scope, plus land-<land> for a
// land, so the token is minted for that one land.
func ScopeFor(land string) string {
	if land == "" {
		return Scope
	}
	return Scope + " " + LandAudiencePrefix + land
}

// SessionPath is the file the session for land is cached in: FilePath for
// "", and oidc-login-land-<land>.json next to it for a land.
func SessionPath(land string) (string, error) {
	path, err := FilePath()
	if err != nil || land == "" {
		return path, err
	}
	if !landName.MatchString(land) {
		return "", fmt.Errorf("invalid land name %q", land)
	}
	return filepath.Join(filepath.Dir(path), "oidc-login-"+LandAudiencePrefix+land+".json"), nil
}

// Fresh reports whether the access token is usable now without a refresh.
func (s *Session) Fresh(now time.Time) bool {
	return s.AccessToken != "" && now.Add(ExpirySkew).Before(s.Expiry)
}

// Endpoints is the part of the issuer's discovery document a sign-in uses.
type Endpoints struct {
	Issuer                      string `json:"issuer"`
	TokenEndpoint               string `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
}

type deviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// DiscoveryDocError is a discovery document that answered but cannot be used
// (wrong issuer, no token endpoint): a misconfiguration, not an outage, so a
// refresh is not retried for it.
type DiscoveryDocError struct{ msg string }

func (e *DiscoveryDocError) Error() string { return e.msg }

// OAuthError is an RFC 6749 error response from the token endpoint.
type OAuthError struct {
	Code        string
	Description string
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Description)
	}
	return e.Code
}

// Prompt is how DeviceLogin talks to the person.
type Prompt struct {
	// CLI is the command name ("kd", "gb") used in messages.
	CLI string
	// Out receives the verification URL, the code and the progress lines.
	Out io.Writer
	// Sleep waits out the poll interval; injected so tests do not.
	Sleep func(time.Duration)
	// OpenBrowser, when set, is offered the verification URL (with the code
	// when the issuer provides one). A failure is reported and ignored: the
	// URL is printed either way.
	OpenBrowser func(string) error
}

// DeviceLogin drives the device grant end to end and returns the session. The
// caller binds it to a daemon (Session.Daemon) and saves it.
func DeviceLogin(ctx context.Context, hc *http.Client, issuer, clientID string, p Prompt) (*Session, error) {
	return DeviceLoginForLand(ctx, hc, issuer, clientID, "", p)
}

// DeviceLoginForLand is DeviceLogin for a token minted for one land: it
// requests the land-<land> scope and returns a session with Land set. ""
// is DeviceLogin.
func DeviceLoginForLand(ctx context.Context, hc *http.Client, issuer, clientID, land string, p Prompt) (*Session, error) {
	if land != "" && !landName.MatchString(land) {
		return nil, fmt.Errorf("invalid land name %q", land)
	}
	s, err := deviceLogin(ctx, hc, issuer, clientID, land, p)
	if err != nil {
		return nil, err
	}
	s.Land = land
	return s, nil
}

func deviceLogin(ctx context.Context, hc *http.Client, issuer, clientID, land string, p Prompt) (*Session, error) {
	ep, err := Discover(ctx, hc, issuer)
	if err != nil {
		return nil, err
	}
	if ep.DeviceAuthorizationEndpoint == "" {
		return nil, fmt.Errorf("issuer %s does not advertise a device_authorization_endpoint", issuer)
	}
	da, err := requestDeviceCode(ctx, hc, ep, issuer, clientID, land)
	if err != nil {
		return nil, err
	}
	showDeviceCode(p, da)
	return pollDeviceToken(ctx, hc, ep, issuer, clientID, da, p)
}

// requestDeviceCode starts the device grant, naming the Keycloak setting
// behind the refusals a misconfigured client or land scope produce.
func requestDeviceCode(ctx context.Context, hc *http.Client, ep *Endpoints, issuer, clientID, land string) (*deviceAuthResponse, error) {
	var da deviceAuthResponse
	if err := postForm(ctx, hc, ep.DeviceAuthorizationEndpoint, url.Values{
		"client_id": {clientID},
		"scope":     {ScopeFor(land)},
	}, &da); err != nil {
		var oe *OAuthError
		if land != "" && errors.As(err, &oe) && oe.Code == "invalid_scope" {
			return nil, fmt.Errorf("client %q cannot request scope %s%s on %s (has the land's client scope been created and attached?): %w", clientID, LandAudiencePrefix, land, issuer, err)
		}
		if errors.As(err, &oe) && (oe.Code == "invalid_client" || oe.Code == "unauthorized_client") {
			return nil, fmt.Errorf("client %q cannot use the device grant on %s (is it declared in keycloak.clients with oauth2.device.authorization.grant.enabled?): %w", clientID, issuer, err)
		}
		return nil, fmt.Errorf("device authorization request: %w", err)
	}
	if da.DeviceCode == "" || da.UserCode == "" || da.VerificationURI == "" {
		return nil, errors.New("device authorization response is missing device_code, user_code or verification_uri")
	}
	return &da, nil
}

// showDeviceCode tells the person where to enter the code, and opens a
// browser on it when the prompt can.
func showDeviceCode(p Prompt, da *deviceAuthResponse) {
	w := p.Out
	fmt.Fprintf(w, "To sign in, open:\n\n    %s\n\nand enter the code:\n\n    %s\n\n", da.VerificationURI, da.UserCode)
	if da.VerificationURIComplete != "" {
		fmt.Fprintf(w, "Or open this link, which already includes the code:\n\n    %s\n\n", da.VerificationURIComplete)
	}
	fmt.Fprintln(w, "Only enter this code if you started this command yourself. Never enter a code")
	fmt.Fprintln(w, "that someone else, or an agent, asks you to confirm.")
	fmt.Fprintln(w)
	if p.OpenBrowser != nil {
		target := da.VerificationURIComplete
		if target == "" {
			target = da.VerificationURI
		}
		if err := p.OpenBrowser(target); err != nil {
			fmt.Fprintf(w, "(could not open a browser: %v; open the link above yourself)\n", err)
		}
	}
	fmt.Fprintln(w, "Waiting for you to finish signing in...")
}

// pollDeviceToken polls the token endpoint until the person answers, the
// code expires or ctx ends.
func pollDeviceToken(ctx context.Context, hc *http.Client, ep *Endpoints, issuer, clientID string, da *deviceAuthResponse, p Prompt) (*Session, error) {
	w := p.Out
	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expires := da.ExpiresIn
	if expires <= 0 {
		expires = 600
	}
	deadline := time.Now().Add(time.Duration(expires) * time.Second)
	expiredMsg := fmt.Sprintf("the sign-in code expired before it was used; run '%s login' again", p.CLI)

	// backoff replaces the poll interval while the token endpoint is failing
	// transiently (0 otherwise). lastTransient is the latest such failure,
	// named if the code expires before Keycloak answers again.
	var backoff time.Duration
	var lastTransient error
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			if lastTransient != nil {
				return nil, fmt.Errorf("%s (the last token request failed: %v)", expiredMsg, lastTransient)
			}
			return nil, errors.New(expiredMsg)
		}
		if backoff > 0 {
			p.Sleep(backoff)
		} else {
			p.Sleep(interval)
		}

		var tr tokenResponse
		err := postForm(ctx, hc, ep.TokenEndpoint, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {da.DeviceCode},
			"client_id":   {clientID},
		}, &tr)
		if err == nil {
			return sessionFromToken(issuer, clientID, &tr, time.Now())
		}
		var oe *OAuthError
		if !errors.As(err, &oe) {
			// A reset connection, a timeout or an edge 5xx while the person is
			// approving the code is not their answer: dying here throws away
			// an Allow they already clicked (twice on 2026-11-01, "read:
			// connection reset by peer"). Keep polling, backing off, until
			// the code expires; anything else is fatal as before.
			if ctx.Err() != nil || !transientTokenError(err) {
				return nil, fmt.Errorf("token request: %w", err)
			}
			if lastTransient == nil {
				fmt.Fprintf(w, "(the sign-in server did not answer: %v; still waiting)\n", err)
			}
			lastTransient = err
			backoff = min(2*max(backoff, interval), maxTransientBackoff)
			continue
		}
		backoff, lastTransient = 0, nil
		switch oe.Code {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return nil, errors.New("sign-in was denied")
		case "expired_token":
			return nil, errors.New(expiredMsg)
		default:
			return nil, fmt.Errorf("token request: %w", err)
		}
	}
}

func sessionFromToken(issuer, clientID string, tr *tokenResponse, now time.Time) (*Session, error) {
	if tr.AccessToken == "" {
		return nil, errors.New("token response has no access_token")
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Session{
		Issuer:       issuer,
		ClientID:     clientID,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		Expiry:       now.Add(ttl),
	}, nil
}

// Discover fetches the issuer's discovery document and checks that it
// describes that issuer.
func Discover(ctx context.Context, hc *http.Client, issuer string) (*Endpoints, error) {
	return DiscoverAt(ctx, hc, issuer, issuer)
}

// DiscoverAt fetches the discovery document under base rather than under the
// issuer (an in-cluster Service for a public issuer) and, like Discover,
// accepts it only when it names issuer.
func DiscoverAt(ctx context.Context, hc *http.Client, base, issuer string) (*Endpoints, error) {
	issuer = strings.TrimRight(issuer, "/")
	base = strings.TrimRight(base, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery: %s returned %s", base, resp.Status)
	}
	var ep Endpoints
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ep); err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	// The document must describe the issuer we asked for; otherwise whoever
	// answered could point us at a token endpoint of their choosing.
	if strings.TrimRight(ep.Issuer, "/") != issuer {
		return nil, &DiscoveryDocError{fmt.Sprintf("oidc discovery: document issuer %q does not match %q", ep.Issuer, issuer)}
	}
	if ep.TokenEndpoint == "" {
		return nil, &DiscoveryDocError{"oidc discovery: no token_endpoint"}
	}
	return &ep, nil
}

// postForm POSTs a urlencoded form and decodes a JSON response into out. A
// non-2xx response carrying an RFC 6749 error body becomes an *OAuthError.
// maxTransientBackoff caps the wait between token polls while the token
// endpoint is failing transiently.
const maxTransientBackoff = 30 * time.Second

// HTTPStatusError is a non-2xx answer that carried no OAuth error body (an
// edge or proxy page, say).
type HTTPStatusError struct {
	Endpoint   string
	Status     string
	StatusCode int
}

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("%s returned %s", e.Endpoint, e.Status) }

// transientTokenError reports whether a token request failed in a way that
// says nothing about the grant: the request never got an answer (connection
// reset or refused, timeout, EOF), or the answer was a 5xx or 429 without an
// OAuth error body. A 4xx without one, or a body that is not JSON, is a
// misconfiguration and stays fatal.
func transientTokenError(err error) bool {
	var se *HTTPStatusError
	if errors.As(err, &se) {
		return se.StatusCode >= 500 || se.StatusCode == http.StatusTooManyRequests
	}
	// Not any net.Error: *url.Error, which wraps every hc.Do failure, is
	// one, and a bad URL or an untrusted certificate must stay fatal.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	var oe *net.OpError
	return errors.As(err, &oe) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

func postForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var er tokenResponse
		if json.Unmarshal(body, &er) == nil && er.Error != "" {
			return &OAuthError{Code: er.Error, Description: er.ErrorDescription}
		}
		return &HTTPStatusError{Endpoint: endpoint, Status: resp.Status, StatusCode: resp.StatusCode}
	}
	return json.Unmarshal(body, out)
}

// Save writes the session 0600 (directory 0700) via a temp file and rename, so
// a crash never leaves a half-written or world-readable file. A land's
// session goes to its own file (SessionPath).
func Save(s *Session) error {
	path, err := SessionPath(s.Land)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".oidc-login-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Load reads the cached session; (nil, nil) when not signed in.
func Load() (*Session, error) {
	return LoadFor("")
}

// LoadFor reads the cached session for land ("" for the ordinary one);
// (nil, nil) when not signed in for it.
func LoadFor(land string) (*Session, error) {
	path, err := SessionPath(land)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// Remove deletes the cached session. It reports false when there was none.
func Remove() (bool, error) {
	return RemoveFor("")
}

// RemoveFor deletes the cached session for land ("" for the ordinary one).
// It reports false when there was none.
func RemoveFor(land string) (bool, error) {
	path, err := SessionPath(land)
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// DaemonHost reduces a daemon URL or host:port address (HTTP or gRPC) to its
// lower-cased hostname, the unit a session is bound to. It returns "" when no
// host can be read.
func DaemonHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Bearer returns the cached access token, refreshing it first when it is
// (about to be) expired. It returns "" when the person is not signed in, when
// the session was minted for a different daemon than target, or when the
// session can no longer be refreshed (a one-line hint goes to warn); in all
// cases the caller falls back to its other credentials. target is the daemon
// URL/address this invocation will talk to; cli ("kd", "gb") names the
// command in the hints. A target through a land door (/land/<name>) uses
// that land's session and no other: the door refuses a token not minted for
// its land. With NoSessionEnv=1 it returns "" and reads nothing.
func Bearer(ctx context.Context, hc *http.Client, now time.Time, target, cli string, warn io.Writer) string {
	if os.Getenv(NoSessionEnv) == "1" {
		return ""
	}
	land := LandFromURL(target)
	s, err := LoadFor(land)
	if err != nil {
		fmt.Fprintf(warn, "%s: ignoring unreadable login session: %v\n", cli, err)
		return ""
	}
	if s == nil {
		return ""
	}
	// Fail closed: a session with no recorded daemon, or one for another host,
	// is never sent. This runs before any refresh so nothing about the session
	// touches the network for the wrong daemon.
	if want := DaemonHost(target); s.Daemon == "" || want == "" || s.Daemon != want {
		fmt.Fprintf(warn, "%s: login session is not for %q (signed in for %q); not sending it. Run '%s login' against this daemon\n", cli, want, s.Daemon, cli)
		return ""
	}
	if s.Land != land {
		fmt.Fprintf(warn, "%s: login session is for land %q, not %q; not sending it. Run '%s login' against this daemon\n", cli, s.Land, land, cli)
		return ""
	}
	if s.Fresh(now) {
		return s.AccessToken
	}
	if s.RefreshToken == "" {
		fmt.Fprintf(warn, "%s: login session expired; run '%s login' to sign in again\n", cli, cli)
		return ""
	}
	err = refreshRetrying(ctx, hc, s, now)
	switch {
	case err == nil:
		if err := Save(s); err != nil {
			// The refreshed token is still good for this call; the next run
			// refreshes again.
			fmt.Fprintf(warn, "%s: could not cache the refreshed login session: %v\n", cli, err)
		}
		return s.AccessToken
	case RefreshRefused(err):
		// Keycloak answered and said no: the session idled out, was revoked
		// or hit its lifetime. Only a new sign-in helps.
		fmt.Fprintf(warn, "%s: Keycloak ended the login session (%v); run '%s login' to sign in again\n", cli, err, cli)
		return ""
	case now.Before(s.Expiry):
		// Keycloak could not be reached, but the cached token has not
		// actually lapsed (it is refreshed ExpirySkew early): it is still
		// good at the daemon, which verifies tokens without Keycloak.
		fmt.Fprintf(warn, "%s: could not refresh the login session (%v); using the cached token, still valid for %s\n", cli, err, s.Expiry.Sub(now).Truncate(time.Second))
		return s.AccessToken
	default:
		// Keycloak could not be reached and the token has lapsed. The session
		// is NOT expired and is kept on disk; the next run refreshes it. A
		// Keycloak restart is about two minutes of this (gd-NrzwHXUxV0).
		fmt.Fprintf(warn, "%s: could not reach %s to refresh the login session (%v); the session is kept, retry in a moment\n", cli, s.Issuer, err)
		return ""
	}
}

// RefreshRefused reports whether err is Keycloak refusing the refresh grant
// (an RFC 6749 §5.2 error from the token endpoint): the session is gone and
// only a new sign-in brings it back. Anything else, a dial error, a timeout, a
// 502 from the edge while Keycloak restarts, a discovery failure, is
// transient and leaves the session intact.
func RefreshRefused(err error) bool {
	var oe *OAuthError
	if !errors.As(err, &oe) {
		return false
	}
	switch oe.Code {
	case "invalid_grant", "invalid_client", "unauthorized_client", "invalid_scope", "unsupported_grant_type", "invalid_request":
		return true
	}
	return false
}

// Refresh renews s through Keycloak with the same retries Bearer uses and
// updates it in place on success; the caller saves it. It is for a CLI that
// words its own hints (gl speaks to tenants about lands, not daemons): a
// RefreshRefused error means only a new sign-in helps, anything else is
// transient.
func Refresh(ctx context.Context, hc *http.Client, s *Session, now time.Time) error {
	return refreshRetrying(ctx, hc, s, now)
}

// refreshRetrying is refresh with RefreshAttempts tries for transient
// failures. A refusal returns at once; s is updated only on success.
func refreshRetrying(ctx context.Context, hc *http.Client, s *Session, now time.Time) error {
	backoff := RefreshBackoff
	for attempt := 1; ; attempt++ {
		err := refresh(ctx, hc, s, now)
		var docErr *DiscoveryDocError
		if err == nil || RefreshRefused(err) || errors.As(err, &docErr) || attempt >= RefreshAttempts || ctx.Err() != nil {
			return err
		}
		RetrySleep(backoff)
		backoff *= 2
	}
}

// Refresher is the hook a CLI installs on its HTTP client when the bearer came
// from the cached session. On a 401 the client calls it once: it forces a
// refresh through Keycloak, caches the result and returns the new access
// token, and the client retries the request with it. This recovers a token
// the edge or daemon rejected although the CLI thought it fresh (a verifier
// whose key cache was cold during a restart, clock skew, a rotated realm key),
// and when the refresh fails it tells the person which it was: a session
// Keycloak ended (sign in again) or a Keycloak that cannot be reached (retry).
// It returns ("", err) when there is nothing to retry with. target is the
// daemon URL this invocation talks to; the session must be bound to it.
func Refresher(hc *http.Client, target, cli string, warn io.Writer) func(ctx context.Context) (string, error) {
	land := LandFromURL(target)
	return func(ctx context.Context) (string, error) {
		s, err := LoadFor(land)
		if err != nil {
			return "", err
		}
		if s == nil {
			return "", errors.New("not signed in")
		}
		if want := DaemonHost(target); s.Daemon == "" || want == "" || s.Daemon != want || s.Land != land {
			return "", errors.New("login session is for another daemon")
		}
		if s.RefreshToken == "" {
			return "", errors.New("login session has no refresh token")
		}
		if err := refreshRetrying(ctx, hc, s, time.Now()); err != nil {
			if RefreshRefused(err) {
				fmt.Fprintf(warn, "%s: request refused (401) and Keycloak ended the login session (%v); run '%s login' to sign in again\n", cli, err, cli)
			} else {
				fmt.Fprintf(warn, "%s: request refused (401) and %s cannot be reached to refresh the login session (%v); retry in a moment\n", cli, s.Issuer, err)
			}
			return "", err
		}
		if err := Save(s); err != nil {
			fmt.Fprintf(warn, "%s: could not cache the refreshed login session: %v\n", cli, err)
		}
		fmt.Fprintf(warn, "%s: request refused (401); refreshed the login session, retrying once\n", cli)
		return s.AccessToken, nil
	}
}

func refresh(ctx context.Context, hc *http.Client, s *Session, now time.Time) error {
	ep, err := Discover(ctx, hc, s.Issuer)
	if err != nil {
		return err
	}
	var tr tokenResponse
	if err := postForm(ctx, hc, ep.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.RefreshToken},
		"client_id":     {s.ClientID},
	}, &tr); err != nil {
		return err
	}
	ns, err := sessionFromToken(s.Issuer, s.ClientID, &tr, now)
	if err != nil {
		return err
	}
	// Keycloak rotates refresh tokens; keep the old one if none came back.
	if ns.RefreshToken == "" {
		ns.RefreshToken = s.RefreshToken
	}
	ns.Daemon = s.Daemon // the refreshed token stays bound to the same daemon
	// and to the same land: dropping Land would Save a land token over the
	// ordinary session file, and leave the land's own file expired.
	ns.Land = s.Land
	*s = *ns
	return nil
}

// DescribeIdentity renders " as <user> (<roles>)" from the access token's
// claims, for the post-login confirmation. The token is NOT verified here:
// the daemon verifies it; this is display only. Returns "" if unreadable.
func DescribeIdentity(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Email             string   `json:"email"`
		PreferredUsername string   `json:"preferred_username"`
		RealmRoles        []string `json:"realm_roles"`
		RealmAccess       struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return ""
	}
	who := c.Email
	if who == "" {
		who = c.PreferredUsername
	}
	seen := map[string]bool{}
	var roles []string
	for _, r := range append(c.RealmRoles, c.RealmAccess.Roles...) {
		if strings.HasPrefix(r, "gardenio-") && !seen[r] {
			seen[r] = true
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	out := ""
	if who != "" {
		out = " as " + who
	}
	if len(roles) > 0 {
		out += " (" + strings.Join(roles, ", ") + ")"
	}
	return out
}
