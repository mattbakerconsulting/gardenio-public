// Package githubapp mints short-lived, repo-scoped GitHub App installation
// tokens for agent pods (plan gardenio/plans/github-app-onboarding.md §4.2,
// epic gd-BHLDnjX1Hw).
//
// Flow: sign a 10-minute RS256 App JWT with the App private key, exchange it at
// POST /app/installations/{id}/access_tokens for a 1-hour installation token
// narrowed to the named repositories, and revoke it with DELETE
// /installation/token when the agent finishes.
//
// Credential hygiene (hard invariant, asserted by the tests): the private key,
// the App JWT and installation-token values are never logged, never placed in
// an error string and never exposed through fmt/slog/JSON. Token carries its
// value in an unexported field behind Reveal(); every formatting path prints a
// redaction marker. Error text carries the HTTP status and GitHub's `message`
// field only.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAPIBaseURL is the public GitHub REST API.
	DefaultAPIBaseURL = "https://api.github.com"
	// DefaultWebURL is github.com, where the OAuth code exchange and the App
	// install pages live.
	DefaultWebURL = "https://github.com"

	// jwtLifetime is the App JWT validity. GitHub rejects exp more than 10
	// minutes after iat; 9 leaves margin for clock skew.
	jwtLifetime = 9 * time.Minute
	// jwtBackdate backdates iat to tolerate clock drift (GitHub's own advice).
	jwtBackdate = 60 * time.Second

	apiVersion   = "2022-11-28"
	maxErrorBody = 1024
)

// Redacted is what every formatting path prints in place of a secret value.
const Redacted = "[REDACTED]"

// Token is an installation access token. The value is reachable only through
// Reveal so that an accidental %v, %+v, %#v, slog attribute or json.Marshal
// cannot leak it.
type Token struct {
	value     string
	ExpiresAt time.Time
}

// NewToken wraps a raw token value (used when re-reading a stored Secret).
func NewToken(value string, expiresAt time.Time) Token {
	return Token{value: value, ExpiresAt: expiresAt}
}

// Reveal returns the raw token. Call it only where the value is written to its
// destination (the per-agent K8s Secret, an Authorization header).
func (t Token) Reveal() string { return t.value }

func (t Token) String() string               { return Redacted }
func (t Token) GoString() string             { return Redacted }
func (t Token) LogValue() slog.Value         { return slog.StringValue(Redacted) }
func (t Token) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (t Token) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// Config configures a Client.
type Config struct {
	// Name is which App this is (identity.go). Empty means Fleet, so the
	// fleet's existing callers keep working unchanged.
	Name       AppName
	AppID      string // numeric App id (JWT iss)
	PrivateKey []byte // PEM, PKCS#1 or PKCS#8 RSA; never logged
	BaseURL    string // default DefaultAPIBaseURL
	HTTP       *http.Client
	Now        func() time.Time // test seam

	// The OAuth half, used only for the user-to-server leg (user.go): the
	// Lands App exchanges the code GitHub sends to its setup URL. Optional for
	// the fleet App, which has no user-authorization leg.
	ClientID     string
	ClientSecret []byte // never logged
	WebURL       string // default DefaultWebURL
}

// Client talks to the GitHub App API. Safe for concurrent use.
type Client struct {
	name    AppName
	appID   string
	key     *rsa.PrivateKey
	baseURL string
	webURL  string
	hc      *http.Client
	now     func() time.Time

	clientID     string
	clientSecret []byte

	mu    sync.Mutex
	instc map[string]int64 // owner/repo -> installation id
}

// New builds a Client. The returned error never contains key material.
func New(cfg Config) (*Client, error) {
	name := cfg.Name
	if name == "" {
		name = Fleet
	}
	if !name.known() {
		return nil, fmt.Errorf("githubapp: unknown app name %q", name)
	}
	appID := strings.TrimSpace(cfg.AppID)
	if appID == "" {
		return nil, errors.New("githubapp: app id is required")
	}
	if _, err := strconv.ParseInt(appID, 10, 64); err != nil {
		return nil, errors.New("githubapp: app id must be numeric")
	}
	key, err := parsePrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = DefaultAPIBaseURL
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	web := strings.TrimRight(strings.TrimSpace(cfg.WebURL), "/")
	if web == "" {
		web = DefaultWebURL
	}
	return &Client{
		name: name, appID: appID, key: key, baseURL: base, webURL: web, hc: hc, now: now,
		clientID: strings.TrimSpace(cfg.ClientID), clientSecret: bytes.TrimSpace(cfg.ClientSecret),
		instc: map[string]int64{},
	}, nil
}

// Name is which App this client speaks for.
func (c *Client) Name() AppName { return c.name }

// parsePrivateKey accepts PKCS#1 ("RSA PRIVATE KEY", what GitHub issues) or
// PKCS#8 PEM. Errors are deliberately generic.
func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(bytes.TrimSpace(pemBytes))
	if block == nil {
		return nil, errors.New("githubapp: private key is not valid PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
	}
	return nil, errors.New("githubapp: private key is not a parseable RSA key")
}

// JWT returns a freshly signed App JWT (RS256, iat now-60s, exp ≤ 10 min).
// The return value is a credential: callers must not log it.
func (c *Client) JWT() (string, error) {
	now := c.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": c.appID,
	})
	if err != nil {
		return "", errors.New("githubapp: marshal jwt claims")
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("githubapp: sign jwt")
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// RepoRef names a repository as owner/name.
type RepoRef struct{ Owner, Name string }

func (r RepoRef) String() string { return r.Owner + "/" + r.Name }

// ParseRepoURL extracts owner/name from an https clone URL on host
// (e.g. github.com), tolerating a .git suffix and trailing slash. ok is false
// for any other host, scheme, or shape.
func ParseRepoURL(gitURL, host string) (RepoRef, bool) {
	u, err := url.Parse(strings.TrimSpace(gitURL))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, host) {
		return RepoRef{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return RepoRef{}, false
	}
	name := strings.TrimSuffix(parts[1], ".git")
	if parts[0] == "" || name == "" {
		return RepoRef{}, false
	}
	return RepoRef{Owner: parts[0], Name: name}, true
}

// InstallationFor resolves the installation id covering owner/repo
// (GET /repos/{o}/{r}/installation, App-JWT authenticated). Cached.
func (c *Client) InstallationFor(ctx context.Context, repo RepoRef) (int64, error) {
	key := strings.ToLower(repo.String())
	c.mu.Lock()
	id, ok := c.instc[key]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	id, err := c.RepoInstallation(ctx, repo)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.instc[key] = id
	c.mu.Unlock()
	return id, nil
}

// RepoInstallation is InstallationFor without the cache: it asks GitHub, every
// call, which of this App's installations owner/repo is in. A repo the App is
// not installed on, or that a selected-repositories installation does not
// include, is a 404 (*HTTPError). Callers that check a pinned installation
// against the repo use this, so a repo removed from the installation fails at
// once rather than on the cached answer.
func (c *Client) RepoInstallation(ctx context.Context, repo RepoRef) (int64, error) {
	jwt, err := c.JWT()
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/installation", c.baseURL, url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
	if err := c.do(ctx, http.MethodGet, endpoint, jwt, nil, http.StatusOK, &out); err != nil {
		return 0, fmt.Errorf("githubapp: look up installation for %s: %w", repo, err)
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("githubapp: installation lookup for %s returned no id", repo)
	}
	return out.ID, nil
}

// AgentTokenPermissions is the permission set requested on every agent token:
// the agents manifest set (plan §4.2 step 3). A fresh map is returned per call
// so callers cannot mutate the shared default.
func AgentTokenPermissions() map[string]string {
	return map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"issues":        "write",
		"actions":       "write",
		"workflows":     "write",
		"checks":        "read",
		"statuses":      "read",
		"packages":      "read",
	}
}

// MintInstallationToken mints a token for installation, narrowed to the named
// repositories (names only, same owner as the installation), with the
// permission set of the client's App (TokenPermissions). It fails closed if
// repoNames is empty and if GitHub's response is not repository_selection
// "selected" covering exactly the requested repos — a token wider than asked
// for is revoked immediately and reported as an error.
func (c *Client) MintInstallationToken(ctx context.Context, installationID int64, repoNames []string) (Token, error) {
	return c.MintInstallationTokenWithPermissions(ctx, installationID, repoNames, TokenPermissions(c.name))
}

// BridgeTokenPermissions is the permission set the github-bridge requests: read
// the repo and its checks/Actions runs, and write PR/issue comments. Notably no
// contents:write — the bridge never pushes code. A fresh map per call.
func BridgeTokenPermissions() map[string]string {
	return map[string]string{
		"contents":      "read",
		"pull_requests": "write",
		"issues":        "write",
		"checks":        "read",
		"actions":       "read",
	}
}

// ConflictResolverTokenPermissions is the permission set of the github-bridge
// mechanical conflict resolver's push token (gd-mBJXcHcBxb), minted apart from
// the bridge's own read token and used only for its `git push`.
// contents:write pushes the merge-of-main commit to the PR branch; GitHub
// refuses a push whose commits change .github/workflows without
// workflows:write, and a merge of main carries main's workflow edits. Nothing
// else: comments and PR reads stay on the bridge token. A fresh map per call.
func ConflictResolverTokenPermissions() map[string]string {
	return map[string]string{
		"contents":  "write",
		"workflows": "write",
	}
}

// ReleaseDeployerTokenPermissions is the permission set of the release
// deployer's token (gd-q48ldwLspl): list tags and clone (contents), read the
// tag's commit and its workflow runs and jobs (actions), and nothing it could
// write with. A fresh map per call.
func ReleaseDeployerTokenPermissions() map[string]string {
	return map[string]string{
		"contents": "read",
		"actions":  "read",
		"metadata": "read",
	}
}

// MintInstallationTokenWithPermissions is MintInstallationToken with an
// explicit permission set. permissions must be non-empty: an empty request
// would inherit every permission the App holds, which is exactly the widening
// the pinned set exists to prevent.
func (c *Client) MintInstallationTokenWithPermissions(ctx context.Context, installationID int64, repoNames []string, permissions map[string]string) (Token, error) {
	if len(permissions) == 0 {
		return Token{}, errors.New("githubapp: refusing to mint a token without an explicit permission set")
	}
	if installationID <= 0 {
		return Token{}, errors.New("githubapp: installation id is required")
	}
	if len(repoNames) == 0 {
		return Token{}, errors.New("githubapp: refusing to mint an unscoped installation token (no repositories)")
	}
	jwt, err := c.JWT()
	if err != nil {
		return Token{}, err
	}
	body, err := json.Marshal(map[string]any{
		"repositories": repoNames,
		// Pin the permission set instead of inheriting every permission the App
		// holds, so a permission added to the App later does not silently widen
		// every agent token.
		"permissions": permissions,
	})
	if err != nil {
		return Token{}, errors.New("githubapp: marshal mint request")
	}
	var out struct {
		Token        string    `json:"token"`
		ExpiresAt    time.Time `json:"expires_at"`
		RepoSelect   string    `json:"repository_selection"`
		Repositories []struct {
			Name string `json:"name"`
		} `json:"repositories"`
	}
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.baseURL, installationID)
	if err := c.do(ctx, http.MethodPost, endpoint, jwt, body, http.StatusCreated, &out); err != nil {
		return Token{}, fmt.Errorf("githubapp: mint installation token: %w", err)
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return Token{}, errors.New("githubapp: mint response missing token or expires_at")
	}
	tok := Token{value: out.Token, ExpiresAt: out.ExpiresAt}

	if out.RepoSelect != "selected" || !sameRepoSet(out.Repositories, repoNames) {
		// Never hand out a token wider (or different) than requested.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = c.Revoke(rctx, tok)
		return Token{}, errors.New("githubapp: minted token is not scoped to exactly the requested repositories; revoked")
	}
	return tok, nil
}

func sameRepoSet(got []struct {
	Name string `json:"name"`
}, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	set := make(map[string]bool, len(want))
	for _, w := range want {
		set[strings.ToLower(w)] = true
	}
	for _, g := range got {
		if !set[strings.ToLower(g.Name)] {
			return false
		}
	}
	return true
}

// Revoke deletes an installation token (DELETE /installation/token). It is
// idempotent: 401/404 (already revoked or expired) count as success.
func (c *Client) Revoke(ctx context.Context, tok Token) error {
	if tok.value == "" {
		return nil
	}
	endpoint := c.baseURL + "/installation/token"
	err := c.do(ctx, http.MethodDelete, endpoint, tok.value, nil, http.StatusNoContent, nil)
	var he *HTTPError
	if errors.As(err, &he) && (he.Status == http.StatusUnauthorized || he.Status == http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("githubapp: revoke installation token: %w", err)
	}
	return nil
}

// DefaultBranch reads repo's default branch through installationID (GET
// /repos/{owner}/{name}) with a token that can read the repository's metadata
// and nothing else, revoked once read. The Lands provisioner uses it to set a
// land's default_branch (Track B §3.3 step 1).
func (c *Client) DefaultBranch(ctx context.Context, installationID int64, repo RepoRef) (string, error) {
	out, err := c.readRepo(ctx, installationID, repo)
	if err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "", fmt.Errorf("githubapp: %s has no default branch", repo)
	}
	return out.DefaultBranch, nil
}

// RepoSizeKB reads GitHub's size of repo, in kilobytes, through
// installationID, as DefaultBranch reads the branch. The controller checks it
// at every Lands token mint and refresh against LANDS_MAX_REPO_SIZE_MB, since
// a repository can grow after the setup check (gd-i74esUHCTg).
func (c *Client) RepoSizeKB(ctx context.Context, installationID int64, repo RepoRef) (int64, error) {
	out, err := c.readRepo(ctx, installationID, repo)
	if err != nil {
		return 0, err
	}
	return out.Size, nil
}

// repoMetadata is the part of GET /repos/{owner}/{name} readRepo returns.
type repoMetadata struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Size          int64  `json:"size"` // kilobytes
}

// readRepo reads repo's metadata through installationID with a token that
// can read the repository's metadata and nothing else, revoked once read.
func (c *Client) readRepo(ctx context.Context, installationID int64, repo RepoRef) (repoMetadata, error) {
	if repo.Owner == "" || repo.Name == "" {
		return repoMetadata{}, errors.New("githubapp: repository owner and name are required")
	}
	tok, err := c.MintInstallationTokenWithPermissions(ctx, installationID, []string{repo.Name}, map[string]string{"metadata": "read"})
	if err != nil {
		return repoMetadata{}, err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = c.Revoke(rctx, tok)
	}()
	var out repoMetadata
	endpoint := fmt.Sprintf("%s/repos/%s/%s", c.baseURL, url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
	if err := c.do(ctx, http.MethodGet, endpoint, tok.value, nil, http.StatusOK, &out); err != nil {
		return repoMetadata{}, fmt.Errorf("githubapp: read repository %s: %w", repo, err)
	}
	if !strings.EqualFold(out.FullName, repo.String()) {
		return repoMetadata{}, fmt.Errorf("githubapp: asked for %s, GitHub answered for %q", repo, out.FullName)
	}
	return out, nil
}

// ParsePullRequestURL extracts the repository and number from a pull request
// URL on host (https://github.com/<owner>/<name>/pull/<n>). ok is false for
// any other host, scheme, or shape.
func ParsePullRequestURL(prURL, host string) (repo RepoRef, number int, ok bool) {
	u, err := url.Parse(strings.TrimSpace(prURL))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, host) {
		return RepoRef{}, 0, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] != "pull" {
		return RepoRef{}, 0, false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return RepoRef{}, 0, false
	}
	return RepoRef{Owner: parts[0], Name: parts[1]}, n, true
}

// PullRequestState reads pull request number in repo through installationID
// (GET /repos/{owner}/{name}/pulls/{number}) with a token that can read pull
// requests and nothing else, revoked once read. It returns "open", "closed" or
// "merged": GitHub reports a merged PR as state=closed with merged=true, and
// merged wins. The stale reaper uses it to tell a dead crew's task whose PR is
// still up for review from one whose PR is gone (gd-ovzZSuuHny).
func (c *Client) PullRequestState(ctx context.Context, installationID int64, repo RepoRef, number int) (string, error) {
	if repo.Owner == "" || repo.Name == "" {
		return "", errors.New("githubapp: repository owner and name are required")
	}
	if number <= 0 {
		return "", errors.New("githubapp: pull request number is required")
	}
	tok, err := c.MintInstallationTokenWithPermissions(ctx, installationID, []string{repo.Name}, map[string]string{"pull_requests": "read"})
	if err != nil {
		return "", err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = c.Revoke(rctx, tok)
	}()
	var out struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, url.PathEscape(repo.Owner), url.PathEscape(repo.Name), number)
	if err := c.do(ctx, http.MethodGet, endpoint, tok.value, nil, http.StatusOK, &out); err != nil {
		return "", fmt.Errorf("githubapp: read pull request %s#%d: %w", repo, number, err)
	}
	if out.Number != number {
		return "", fmt.Errorf("githubapp: asked for %s#%d, GitHub answered for #%d", repo, number, out.Number)
	}
	switch {
	case out.Merged:
		return "merged", nil
	case out.State == "open" || out.State == "closed":
		return out.State, nil
	default:
		return "", fmt.Errorf("githubapp: %s#%d has unknown state %q", repo, number, out.State)
	}
}

// GitHubUser is a GitHub account as UserByLogin reads it.
type GitHubUser struct {
	ID    int64
	Login string // GitHub's spelling, which may differ in case from the one asked for
	Type  string // "User", "Organization" or "Bot"
}

// ErrNoGitHubUser is returned by UserByLogin when GitHub has no account by
// that login.
var ErrNoGitHubUser = errors.New("githubapp: no GitHub account has that login")

// UserByLogin resolves a GitHub login to the account's numeric id (GET
// /users/{login}) through installationID, with a token that can read repo's
// metadata and nothing else, revoked once read. The Lands member invite (Track
// B §3.7, step 8.9) uses it: the id is what joins, so a later rename of the
// login never lets someone else in.
func (c *Client) UserByLogin(ctx context.Context, installationID int64, repo RepoRef, login string) (GitHubUser, error) {
	if repo.Owner == "" || repo.Name == "" {
		return GitHubUser{}, errors.New("githubapp: repository owner and name are required")
	}
	login = strings.TrimSpace(login)
	if login == "" || strings.ContainsAny(login, "/?#%") {
		return GitHubUser{}, ErrNoGitHubUser
	}
	tok, err := c.MintInstallationTokenWithPermissions(ctx, installationID, []string{repo.Name}, map[string]string{"metadata": "read"})
	if err != nil {
		return GitHubUser{}, err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = c.Revoke(rctx, tok)
	}()
	var out struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	endpoint := fmt.Sprintf("%s/users/%s", c.baseURL, url.PathEscape(login))
	if err := c.do(ctx, http.MethodGet, endpoint, tok.value, nil, http.StatusOK, &out); err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			return GitHubUser{}, ErrNoGitHubUser
		}
		return GitHubUser{}, fmt.Errorf("githubapp: read user: %w", err)
	}
	if out.ID <= 0 || !strings.EqualFold(out.Login, login) {
		return GitHubUser{}, fmt.Errorf("githubapp: asked for user %q, GitHub answered for %q", login, out.Login)
	}
	return GitHubUser{ID: out.ID, Login: out.Login, Type: out.Type}, nil
}

// HTTPError is a non-success GitHub response. Message is GitHub's `message`
// field only, truncated; headers and raw bodies never reach the error.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// do issues one request with bearer credential bearer. A transport error is
// reduced to its URL-free cause so nothing request-derived beyond the method
// and path leaks.
func (c *Client) do(ctx context.Context, method, endpoint, bearer string, body []byte, wantStatus int, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return errors.New("build request")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			return fmt.Errorf("%s %s: %w", method, req.URL.Path, ue.Err)
		}
		return fmt.Errorf("%s %s: request failed", method, req.URL.Path)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &m)
		return &HTTPError{Status: resp.StatusCode, Message: strings.TrimSpace(m.Message)}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return errors.New("decode response")
		}
	}
	return nil
}
