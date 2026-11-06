package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/githubapp"
	"gardenio/controller/internal/oidcauth"
)

// The Lands GitHub App's setup leg (Track B spec §2.3 and B-D3, step 7.6). An
// applicant proves they control the repository their land will work on by
// installing the Gardenio Lands GitHub App on it, authorizing it as
// themselves:
//
//   - GET /lands/github/install sets a one-time state cookie and sends the
//     applicant to the App's install page on github.com with that state.
//   - GitHub sends them back to GET /lands/github/setup (the App's callback URL)
//     with installation_id, a one-time code, setup_action and the state. A
//     change to the repository selection (setup_action=update) comes back
//     without a code; the handler then sends the applicant through the App's
//     authorization page, which returns with a code and state only. The
//     handler checks the state against the cookie, then
//     githubapp.VerifyLandsSetup checks the code's GitHub user is the
//     applicant's own github_id, the installation is the Lands App's on exactly
//     one repository, the user is an admin there, and the repository's
//     visibility is accepted and it is no larger than the configured size
//     (LANDS_MAX_REPO_SIZE_MB, Track B §5.4 step 10.4). A pass is stored in a short-lived signed cookie
//     (landsGitHubResult) for the application form (step 7.7) to read.
//
// Both routes are routeApplicant and touch nothing but the caller's own
// GitHub account; neither calls the daemon. With the Lands App unconfigured
// (Server.landsGitHub nil) both answer 503 and nothing reaches GitHub: the
// setup fails closed. The App manifest and permissions are in
// gardenio/docs/lands/github-app.md.

const (
	landsGitHubStateCookie = "lands_github_state"
	// landsGitHubUpdateCookie carries an installation_id across the
	// authorization detour an update takes (handleLandsGitHubSetup).
	landsGitHubUpdateCookie = "lands_github_update"
	landsGitHubResultCookie = "lands_github"
	landsGitHubStateTTL     = 15 * time.Minute
	landsGitHubResultTTL    = 30 * time.Minute
	// landsGitHubEnvPrefix names the viewer's Lands App settings
	// (githubapp.ConfigFromEnv): LANDS_GITHUB_APP_ID, _PRIVATE_KEY_PATH,
	// _CLIENT_ID, _CLIENT_SECRET_PATH, _SLUG, optional _API_URL and _WEB_URL.
	landsGitHubEnvPrefix = "LANDS_GITHUB_APP"
	// landsGitHubAllowPrivateEnv turns private repositories off ("false"). On by
	// default: the owner allowed them in v1 (OQ2, decision gd-IhEhhYTj4A).
	landsGitHubAllowPrivateEnv = "LANDS_GITHUB_ALLOW_PRIVATE_REPOS"
	// landsMaxRepoSizeEnv is githubapp.MaxRepoSizeEnv, the largest repository
	// a land takes; the controller checks it again at every token mint.
	landsMaxRepoSizeEnv       = githubapp.MaxRepoSizeEnv
	landsMaxRepoSizeDefaultMB = githubapp.MaxRepoSizeDefaultMB
)

// landsGitHubAPI is the slice of *githubapp.Client the setup leg uses; faked in
// tests.
type landsGitHubAPI interface {
	VerifyLandsSetup(ctx context.Context, check githubapp.SetupCheck) (githubapp.VerifiedRepo, error)
	InstallURL(slug, state string) string
	AuthorizeURL(state string) string
}

// landsGitHub is the configured Lands App.
type landsGitHub struct {
	app          landsGitHubAPI
	slug         string
	allowPrivate bool
	// maxRepoSizeMB is the largest repository a land takes; 0 is no limit.
	maxRepoSizeMB int64
	cookieKey     []byte // signs landsGitHubResult; derived from the App's client secret
	now           func() time.Time
}

// landsGitHubFromEnv builds the Lands App from the environment. It returns nil,
// nil when the App is not configured at all, and an error when it is only
// partly configured; either way the setup routes answer 503.
func landsGitHubFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (*landsGitHub, error) {
	cfg, ok, err := githubapp.ConfigFromEnv(githubapp.Lands, landsGitHubEnvPrefix, true, getenv, readFile)
	if err != nil || !ok {
		return nil, err
	}
	client, err := githubapp.New(cfg.Config)
	if err != nil {
		return nil, err
	}
	ids, err := githubapp.NewIdentities(client)
	if err != nil {
		return nil, err
	}
	app, err := ids.App(githubapp.Lands)
	if err != nil {
		return nil, err
	}
	return &landsGitHub{
		app:           app,
		slug:          cfg.Slug,
		allowPrivate:  !strings.EqualFold(strings.TrimSpace(getenv(landsGitHubAllowPrivateEnv)), "false"),
		maxRepoSizeMB: githubapp.MaxRepoSizeMB(getenv),
		cookieKey:     landsGitHubCookieKey(cfg.ClientSecret),
		now:           time.Now,
	}, nil
}

// landsGitHubCookieKey derives the result cookie's signing key from the App's
// client secret, so the setup leg needs no secret of its own. Rotating the
// client secret only voids results less than landsGitHubResultTTL old.
func landsGitHubCookieKey(clientSecret []byte) []byte {
	m := hmac.New(sha256.New, []byte(strings.TrimSpace(string(clientSecret))))
	m.Write([]byte("gardenio lands github setup result v1"))
	return m.Sum(nil)
}

// landsGitHubResult is a passed setup check, as the result cookie carries it.
// Email and GitHubID bind it to the applicant who passed: read back by anyone
// else, it is not theirs.
type landsGitHubResult struct {
	Email          string    `json:"email"`
	GitHubID       int64     `json:"github_id"`
	GitHubLogin    string    `json:"github_login"`
	InstallationID int64     `json:"installation_id"`
	Repo           string    `json:"repo"`
	RepoID         int64     `json:"repo_id"`
	Private        bool      `json:"repo_private"`
	Visibility     string    `json:"visibility"`
	RepoSizeKB     int64     `json:"repo_size_kb"`
	AccountCreated time.Time `json:"account_created"`
	PublicRepos    int       `json:"public_repos"`
	Followers      int       `json:"followers"`
	Expires        int64     `json:"exp"`
}

// AccountAgeDays is the applicant's GitHub account age on day now (§2.4
// github_account_age_days).
func (r *landsGitHubResult) AccountAgeDays(now time.Time) int {
	if r.AccountCreated.IsZero() {
		return 0
	}
	return int(now.Sub(r.AccountCreated).Hours() / 24)
}

func (g *landsGitHub) sign(res landsGitHubResult) (string, error) {
	payload, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	m := hmac.New(sha256.New, g.cookieKey)
	m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)), nil
}

// verifiedFor reads the result cookie for the caller of ctx: a valid
// signature, not expired, and bound to the caller's email and github_id.
// Step 7.7's POST /lands/apply takes the repository from here, never from
// the form.
func (g *landsGitHub) verifiedFor(ctx context.Context, r *http.Request) (*landsGitHubResult, bool) {
	if g == nil {
		return nil, false
	}
	c, err := r.Cookie(landsGitHubResultCookie)
	if err != nil {
		return nil, false
	}
	body, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return nil, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return nil, false
	}
	m := hmac.New(sha256.New, g.cookieKey)
	m.Write([]byte(body))
	if !hmac.Equal(got, m.Sum(nil)) {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, false
	}
	var res landsGitHubResult
	if err := json.Unmarshal(payload, &res); err != nil {
		return nil, false
	}
	if g.now().Unix() >= res.Expires {
		return nil, false
	}
	email, gid := oidcauth.EmailFromContext(ctx), oidcauth.GitHubIDFromContext(ctx)
	if res.Email == "" || res.Email != email || gid == 0 || res.GitHubID != gid {
		return nil, false
	}
	return &res, true
}

// What the setup page says, one state per outcome.
const (
	landsGitHubUnconfigured = "unconfigured" // the Lands App is not set up on this server
	landsGitHubNoGitHub     = "nogithub"     // the caller did not sign in through GitHub
	landsGitHubRestart      = "restart"      // missing or mismatched state: start from our button
	landsGitHubBadRequest   = "badrequest"   // the callback is missing installation_id or code
	landsGitHubRequested    = "requested"    // an org member asked an owner to install
	landsGitHubRefused      = "refused"      // a check failed (Refusal says which)
	landsGitHubUnavailable  = "unavailable"  // GitHub could not be asked
	landsGitHubVerified     = "verified"     // passed
)

// landsGitHubView is the setup page's data, inside landsView.GitHub.
type landsGitHubView struct {
	State   string
	Refusal string // githubapp.RefuseReason when State is refused
	// AllowPrivate shapes the visibility refusal's wording.
	AllowPrivate bool
	// MaxRepoSizeMB is the size limit the repo_size refusal names.
	MaxRepoSizeMB int64
	Result        *landsGitHubResult
}

func (s *Server) handleLandsGitHubInstall(w http.ResponseWriter, r *http.Request) {
	v, ok := s.landsBase(r, "Connect your repository")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	g := s.landsGitHub
	switch {
	case g == nil:
		s.renderLandsGitHub(w, http.StatusServiceUnavailable, v, landsGitHubView{State: landsGitHubUnconfigured})
		return
	case oidcauth.GitHubIDFromContext(r.Context()) == 0:
		// The check could only fail: say why before sending them to GitHub.
		s.renderLandsGitHub(w, http.StatusForbidden, v, landsGitHubView{State: landsGitHubNoGitHub})
		return
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(raw[:])
	http.SetCookie(w, &http.Cookie{
		Name: landsGitHubStateCookie, Value: state, Path: s.basePath + "/lands/github/",
		MaxAge: int(landsGitHubStateTTL / time.Second), HttpOnly: true, Secure: cookieSecure(r),
		// Lax, not Strict: GitHub's redirect back is a cross-site top-level GET,
		// which Lax lets the cookie ride and Strict would not.
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, g.app.InstallURL(g.slug, state), http.StatusSeeOther)
}

// handleLandsGitHubSetup is the Lands App's callback URL.
func (s *Server) handleLandsGitHubSetup(w http.ResponseWriter, r *http.Request) {
	v, ok := s.landsBase(r, "Connect your repository")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	g := s.landsGitHub
	if g == nil {
		s.renderLandsGitHub(w, http.StatusServiceUnavailable, v, landsGitHubView{State: landsGitHubUnconfigured})
		return
	}
	q := r.URL.Query()
	if q.Get("setup_action") == "request" {
		// A member of an organization asked its owners to install the App;
		// there is no installation yet and GitHub sends no code.
		s.renderLandsGitHub(w, http.StatusOK, v, landsGitHubView{State: landsGitHubRequested})
		return
	}

	// The state proves this browser started here: GitHub echoes back what
	// /lands/github/install sent. Whatever happens next, it is spent.
	stateCookie, err := r.Cookie(landsGitHubStateCookie)
	http.SetCookie(w, &http.Cookie{Name: landsGitHubStateCookie, Value: "", Path: s.basePath + "/lands/github/", MaxAge: -1, HttpOnly: true, Secure: cookieSecure(r), SameSite: http.SameSiteLaxMode})
	state := q.Get("state")
	if err != nil || state == "" || !hmac.Equal([]byte(stateCookie.Value), []byte(state)) {
		s.renderLandsGitHub(w, http.StatusForbidden, v, landsGitHubView{State: landsGitHubRestart})
		return
	}
	gid := oidcauth.GitHubIDFromContext(r.Context())
	if gid == 0 {
		s.renderLandsGitHub(w, http.StatusForbidden, v, landsGitHubView{State: landsGitHubNoGitHub})
		return
	}
	rawIID, action, code := q.Get("installation_id"), q.Get("setup_action"), q.Get("code")
	if action == "update" && code == "" && rawIID != "" {
		// A change to the repository selection comes back without a code,
		// so the applicant's identity is unproven. Get one from the App's
		// authorization page, which returns here with code and state only;
		// the installation_id waits in a cookie.
		s.landsGitHubAuthorize(w, r, g, rawIID)
		return
	}
	if rawIID == "" && action == "" && code != "" {
		if c, err := r.Cookie(landsGitHubUpdateCookie); err == nil {
			rawIID, action = c.Value, "update"
		}
	}
	http.SetCookie(w, &http.Cookie{Name: landsGitHubUpdateCookie, Value: "", Path: s.basePath + "/lands/github/", MaxAge: -1, HttpOnly: true, Secure: cookieSecure(r), SameSite: http.SameSiteLaxMode})
	iid, err := strconv.ParseInt(rawIID, 10, 64)
	if err != nil || iid <= 0 || code == "" || (action != "install" && action != "update") {
		s.renderLandsGitHub(w, http.StatusBadRequest, v, landsGitHubView{State: landsGitHubBadRequest})
		return
	}

	verified, err := g.app.VerifyLandsSetup(r.Context(), githubapp.SetupCheck{
		InstallationID: iid, Code: code, ApplicantGitHubID: gid, AllowPrivate: g.allowPrivate,
		MaxRepoSizeKB: g.maxRepoSizeMB * 1024,
	})
	var refusal *githubapp.SetupRefusal
	switch {
	case errors.As(err, &refusal):
		s.logger.Info("lands github setup refused", "reason", refusal.Reason, "detail", refusal.Detail, "installation_id", iid, "github_id", gid)
		s.renderLandsGitHub(w, http.StatusForbidden, v, landsGitHubView{State: landsGitHubRefused, Refusal: string(refusal.Reason), AllowPrivate: g.allowPrivate, MaxRepoSizeMB: g.maxRepoSizeMB})
		return
	case err != nil:
		s.logger.Warn("lands github setup: GitHub check failed", "error", err, "installation_id", iid)
		s.renderLandsGitHub(w, http.StatusBadGateway, v, landsGitHubView{State: landsGitHubUnavailable})
		return
	}

	res := landsGitHubResult{
		Email: oidcauth.EmailFromContext(r.Context()), GitHubID: verified.GitHubID, GitHubLogin: verified.GitHubLogin,
		InstallationID: verified.InstallationID, Repo: verified.Repo.String(), RepoID: verified.RepoID,
		Private: verified.Private, Visibility: verified.Visibility, RepoSizeKB: verified.SizeKB, AccountCreated: verified.AccountCreated,
		PublicRepos: verified.PublicRepos, Followers: verified.Followers,
		Expires: g.now().Add(landsGitHubResultTTL).Unix(),
	}
	signed, err := g.sign(res)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: landsGitHubResultCookie, Value: signed, Path: s.basePath + "/lands/",
		MaxAge: int(landsGitHubResultTTL / time.Second), HttpOnly: true, Secure: cookieSecure(r), SameSite: http.SameSiteLaxMode,
	})
	s.logger.Info("lands github setup verified", "repo", res.Repo, "installation_id", res.InstallationID, "github_id", res.GitHubID)
	s.renderLandsGitHub(w, http.StatusOK, v, landsGitHubView{State: landsGitHubVerified, Result: &res, AllowPrivate: g.allowPrivate})
}

// landsGitHubAuthorize sends an applicant whose installation update came back
// without a code to the App's authorization page, under a fresh state. The
// installation_id it carries is only a hint: VerifyLandsSetup still checks the
// user the code proves can see that installation of the Lands App.
func (s *Server) landsGitHubAuthorize(w http.ResponseWriter, r *http.Request, g *landsGitHub, rawIID string) {
	if iid, err := strconv.ParseInt(rawIID, 10, 64); err != nil || iid <= 0 {
		v, _ := s.landsBase(r, "Connect your repository")
		s.renderLandsGitHub(w, http.StatusBadRequest, v, landsGitHubView{State: landsGitHubBadRequest})
		return
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(raw[:])
	path, maxAge := s.basePath+"/lands/github/", int(landsGitHubStateTTL/time.Second)
	http.SetCookie(w, &http.Cookie{Name: landsGitHubStateCookie, Value: state, Path: path, MaxAge: maxAge, HttpOnly: true, Secure: cookieSecure(r), SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: landsGitHubUpdateCookie, Value: rawIID, Path: path, MaxAge: maxAge, HttpOnly: true, Secure: cookieSecure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, g.app.AuthorizeURL(state), http.StatusSeeOther)
}

func (s *Server) renderLandsGitHub(w http.ResponseWriter, status int, v landsView, gv landsGitHubView) {
	v.GitHub = gv
	s.renderLands(w, status, "lands_github", v)
}
