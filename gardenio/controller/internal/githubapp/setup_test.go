package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	landsClientID     = "Iv23liLANDSclient"
	landsClientSecret = "landsCLIENTsecretVALUE0123456789"
	landsCode         = "one-time-setup-code"
	landsUserToken    = "ghu_FAKEuserTOKENvalue0123456789"
	applicantGitHubID = int64(4242)
	landsInstallation = int64(777)
)

// fakeLandsGitHub serves both github.com (the code exchange) and the REST API
// for the Lands setup check. Each field is one thing a test can make go wrong.
type fakeLandsGitHub struct {
	t   *testing.T
	srv *httptest.Server
	app *fakeGitHub // JWT verification

	mu        sync.Mutex
	revoked   []string
	exchanged int
	userReads int

	oauthError   string // non-empty: the exchange answers 200 {"error": ...}
	userID       int64
	instStatus   int // GET /app/installations/{id}
	selection    string
	suspended    bool
	reposStatus  int // GET /user/installations/{id}/repositories
	repos        []Repository
	total        int // total_count; -1 = len(repos)
	accountLogin string
	apiDown      bool // GET /user answers 502
}

func newFakeLandsGitHub(t *testing.T) (*Client, *fakeLandsGitHub) {
	t.Helper()
	k, pemBytes := genKey(t)
	f := &fakeLandsGitHub{
		t: t, app: &fakeGitHub{t: t, pub: &k.PublicKey},
		userID: applicantGitHubID, instStatus: http.StatusOK, selection: "selected",
		reposStatus: http.StatusOK, total: -1, accountLogin: "octo",
		repos: []Repository{repoFixture("octo/garden", true, false, "public")},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	c, err := New(Config{
		Name: Lands, AppID: "99", PrivateKey: pemBytes, BaseURL: f.srv.URL, WebURL: f.srv.URL,
		ClientID: landsClientID, ClientSecret: []byte(landsClientSecret), Now: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func repoFixture(full string, admin, private bool, visibility string) Repository {
	var r Repository
	r.ID = 1001
	r.FullName = full
	r.Owner.Login, r.Name, _ = strings.Cut(full, "/")
	r.Permissions.Admin = admin
	r.Private = private
	r.Visibility = visibility
	return r
}

func (f *fakeLandsGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/login/oauth/access_token":
		f.exchanged++
		_ = r.ParseForm()
		if r.PostForm.Get("client_id") != landsClientID || r.PostForm.Get("client_secret") != landsClientSecret {
			writeJSON(http.StatusOK, map[string]string{"error": "incorrect_client_credentials"})
			return
		}
		if f.oauthError != "" || r.PostForm.Get("code") != landsCode {
			e := f.oauthError
			if e == "" {
				e = "bad_verification_code"
			}
			writeJSON(http.StatusOK, map[string]string{"error": e})
			return
		}
		writeJSON(http.StatusOK, map[string]string{"access_token": landsUserToken, "token_type": "bearer"})
	case r.Method == http.MethodGet && r.URL.Path == "/user":
		f.userReads++
		if bearer != landsUserToken {
			writeJSON(http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		if f.apiDown {
			writeJSON(http.StatusBadGateway, map[string]string{"message": "upstream"})
			return
		}
		writeJSON(http.StatusOK, map[string]any{"id": f.userID, "login": "octocat", "created_at": "2015-01-02T03:04:05Z", "public_repos": 12, "followers": 3})
	case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/app/installations/%d", landsInstallation):
		if !f.app.verifyJWT(bearer) {
			writeJSON(http.StatusUnauthorized, map[string]string{"message": "bad jwt"})
			return
		}
		if f.instStatus != http.StatusOK {
			writeJSON(f.instStatus, map[string]string{"message": "Not Found"})
			return
		}
		inst := map[string]any{"id": landsInstallation, "app_id": 99, "repository_selection": f.selection,
			"account": map[string]any{"id": 5, "login": f.accountLogin, "type": "User"}}
		if f.suspended {
			inst["suspended_at"] = "2026-10-01T00:00:00Z"
		}
		writeJSON(http.StatusOK, inst)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/app/installations/"):
		writeJSON(http.StatusNotFound, map[string]string{"message": "Not Found"})
	case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/user/installations/%d/repositories", landsInstallation):
		if bearer != landsUserToken {
			writeJSON(http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		if f.reposStatus != http.StatusOK {
			writeJSON(f.reposStatus, map[string]string{"message": "nope"})
			return
		}
		total := f.total
		if total < 0 {
			total = len(f.repos)
		}
		writeJSON(http.StatusOK, map[string]any{"total_count": total, "repositories": f.repos})
	case r.Method == http.MethodDelete && r.URL.Path == "/applications/"+landsClientID+"/token":
		id, secret, ok := r.BasicAuth()
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		if !ok || id != landsClientID || secret != landsClientSecret {
			writeJSON(http.StatusUnauthorized, map[string]string{"message": "bad basic auth"})
			return
		}
		f.revoked = append(f.revoked, body["access_token"])
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(http.StatusNotFound, map[string]string{"message": "unexpected " + r.Method + " " + r.URL.Path})
	}
}

func goodCheck() SetupCheck {
	return SetupCheck{InstallationID: landsInstallation, Code: landsCode, ApplicantGitHubID: applicantGitHubID, AllowPrivate: true}
}

func TestVerifyLandsSetup_Passes(t *testing.T) {
	c, f := newFakeLandsGitHub(t)
	got, err := c.VerifyLandsSetup(context.Background(), goodCheck())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	want := VerifiedRepo{
		InstallationID: landsInstallation, Repo: RepoRef{Owner: "octo", Name: "garden"}, RepoID: 1001,
		Visibility: "public", GitHubID: applicantGitHubID, GitHubLogin: "octocat",
		AccountCreated: time.Date(2015, 1, 2, 3, 4, 5, 0, time.UTC), PublicRepos: 12, Followers: 3,
	}
	if got != want {
		t.Fatalf("verified\n got %+v\nwant %+v", got, want)
	}
	if len(f.revoked) != 1 || f.revoked[0] != landsUserToken {
		t.Fatalf("user token revocations = %v, want the one token", f.revoked)
	}

	// A private repository passes when private repos are allowed (the owner's
	// OQ2 answer) and records it.
	f.repos = []Repository{repoFixture("octo/secret", true, true, "private")}
	got, err = c.VerifyLandsSetup(context.Background(), goodCheck())
	if err != nil || !got.Private || got.Visibility != "private" {
		t.Fatalf("private repo, allowed: %+v, %v", got, err)
	}
}

// Every refusal path: each names its check, and the user token is revoked on
// every path that got one.
func TestVerifyLandsSetup_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *fakeLandsGitHub, c *SetupCheck)
		want   RefuseReason
	}{
		{"bad code", func(f *fakeLandsGitHub, c *SetupCheck) { c.Code = "stolen" }, RefuseCode},
		{"missing code", func(f *fakeLandsGitHub, c *SetupCheck) { c.Code = "" }, RefuseCode},
		{"code already used", func(f *fakeLandsGitHub, _ *SetupCheck) { f.oauthError = "bad_verification_code" }, RefuseCode},
		{"no installation id", func(f *fakeLandsGitHub, c *SetupCheck) { c.InstallationID = 0 }, RefuseInstallation},
		{"applicant has no github id", func(f *fakeLandsGitHub, c *SetupCheck) { c.ApplicantGitHubID = 0 }, RefuseWrongUser},
		{"authorized as someone else", func(f *fakeLandsGitHub, _ *SetupCheck) { f.userID = 9999 }, RefuseWrongUser},
		{"installation of another app", func(f *fakeLandsGitHub, c *SetupCheck) { c.InstallationID = 778 }, RefuseInstallation},
		{"installation not found", func(f *fakeLandsGitHub, _ *SetupCheck) { f.instStatus = http.StatusNotFound }, RefuseInstallation},
		{"suspended", func(f *fakeLandsGitHub, _ *SetupCheck) { f.suspended = true }, RefuseSuspended},
		{"all repositories", func(f *fakeLandsGitHub, _ *SetupCheck) { f.selection = "all" }, RefuseRepoCount},
		{"user cannot see installation", func(f *fakeLandsGitHub, _ *SetupCheck) { f.reposStatus = http.StatusNotFound }, RefuseInstallation},
		{"user forbidden on installation", func(f *fakeLandsGitHub, _ *SetupCheck) { f.reposStatus = http.StatusForbidden }, RefuseInstallation},
		{"two repositories", func(f *fakeLandsGitHub, _ *SetupCheck) {
			f.repos = append(f.repos, repoFixture("octo/other", true, false, "public"))
		}, RefuseRepoCount},
		{"more than one page", func(f *fakeLandsGitHub, _ *SetupCheck) { f.total = 2 }, RefuseRepoCount},
		{"no repositories", func(f *fakeLandsGitHub, _ *SetupCheck) { f.repos = nil }, RefuseRepoCount},
		{"repo outside the account", func(f *fakeLandsGitHub, _ *SetupCheck) { f.accountLogin = "someone-else" }, RefuseInstallation},
		{"not admin", func(f *fakeLandsGitHub, _ *SetupCheck) {
			f.repos = []Repository{repoFixture("octo/garden", false, false, "public")}
		}, RefuseNotAdmin},
		{"private, not allowed", func(f *fakeLandsGitHub, c *SetupCheck) {
			f.repos = []Repository{repoFixture("octo/garden", true, true, "private")}
			c.AllowPrivate = false
		}, RefuseVisibility},
		{"internal (enterprise)", func(f *fakeLandsGitHub, _ *SetupCheck) {
			f.repos = []Repository{repoFixture("octo/garden", true, true, "internal")}
		}, RefuseVisibility},
		{"visibility contradicts private flag", func(f *fakeLandsGitHub, _ *SetupCheck) {
			f.repos = []Repository{repoFixture("octo/garden", true, true, "public")}
		}, RefuseVisibility},
		{"repository too large", func(f *fakeLandsGitHub, c *SetupCheck) {
			f.repos[0].Size = 2048
			c.MaxRepoSizeKB = 1024
		}, RefuseRepoSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFakeLandsGitHub(t)
			check := goodCheck()
			tc.mutate(f, &check)
			got, err := c.VerifyLandsSetup(context.Background(), check)
			var ref *SetupRefusal
			if !errors.As(err, &ref) || ref.Reason != tc.want {
				t.Fatalf("err = %v, want a %s refusal", err, tc.want)
			}
			if got != (VerifiedRepo{}) {
				t.Fatalf("a refusal returned a repo: %+v", got)
			}
			if f.exchanged > 0 && f.userReads > 0 && (len(f.revoked) != 1 || f.revoked[0] != landsUserToken) {
				t.Fatalf("user token not revoked after a refusal: %v", f.revoked)
			}
			if strings.Contains(err.Error(), landsUserToken) || strings.Contains(err.Error(), landsCode) || strings.Contains(err.Error(), landsClientSecret) {
				t.Fatalf("refusal leaks a credential: %v", err)
			}
		})
	}
}

// The fleet App never verifies a Lands setup, even with OAuth configured.
func TestVerifyLandsSetup_RefusesFleetApp(t *testing.T) {
	_, pemBytes := genKey(t)
	fleet, err := New(Config{AppID: "1", PrivateKey: pemBytes, ClientID: landsClientID, ClientSecret: []byte(landsClientSecret), BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Name() != Fleet {
		t.Fatalf("empty Config.Name = %q, want fleet", fleet.Name())
	}
	_, err = fleet.VerifyLandsSetup(context.Background(), goodCheck())
	var ref *SetupRefusal
	if !errors.As(err, &ref) || ref.Reason != RefuseWrongApp {
		t.Fatalf("fleet verify = %v, want wrong_app", err)
	}
	var nilClient *Client
	if _, err := nilClient.VerifyLandsSetup(context.Background(), goodCheck()); !errors.As(err, &ref) {
		t.Fatalf("nil client verify = %v, want a refusal", err)
	}
}

// GitHub being down is an error to retry, not a refusal.
func TestVerifyLandsSetup_OutageIsNotARefusal(t *testing.T) {
	c, f := newFakeLandsGitHub(t)
	f.apiDown = true
	_, err := c.VerifyLandsSetup(context.Background(), goodCheck())
	var ref *SetupRefusal
	if err == nil || errors.As(err, &ref) {
		t.Fatalf("outage = %v, want a plain error", err)
	}
	if len(f.revoked) != 1 {
		t.Fatalf("user token not revoked after an outage: %v", f.revoked)
	}
}

func TestUserTokenNeverFormats(t *testing.T) {
	tok := UserToken{value: landsUserToken}
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok)} {
		if strings.Contains(s, landsUserToken) {
			t.Fatalf("user token leaked: %s", s)
		}
	}
	b, _ := json.Marshal(struct{ T UserToken }{tok})
	if strings.Contains(string(b), landsUserToken) {
		t.Fatalf("user token leaked in JSON: %s", b)
	}
}

func TestExchangeNeedsOAuth(t *testing.T) {
	_, pemBytes := genKey(t)
	c, err := New(Config{Name: Lands, AppID: "1", PrivateKey: pemBytes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExchangeUserCode(context.Background(), "x"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("exchange without client id/secret = %v, want ErrNotConfigured", err)
	}
}

func TestInstallURL(t *testing.T) {
	c, _ := newFakeLandsGitHub(t)
	c.webURL = DefaultWebURL
	got := c.InstallURL("gardenio-lands", "s t/ate")
	u, err := url.Parse(got)
	if err != nil || u.Host != "github.com" || u.Path != "/apps/gardenio-lands/installations/new" || u.Query().Get("state") != "s t/ate" {
		t.Fatalf("install URL = %s", got)
	}
}

func TestVerifyLandsSetup_RepoSizeLimit(t *testing.T) {
	c, f := newFakeLandsGitHub(t)
	f.repos[0].Size = 1024
	check := goodCheck()
	check.MaxRepoSizeKB = 1024
	got, err := c.VerifyLandsSetup(context.Background(), check)
	if err != nil || got.SizeKB != 1024 {
		t.Fatalf("a repository at the limit = %+v, %v; want verified with its size", got, err)
	}
	f.repos[0].Size = 1 << 30
	check.MaxRepoSizeKB = 0
	if _, err := c.VerifyLandsSetup(context.Background(), check); err != nil {
		t.Fatalf("no limit set: %v", err)
	}
}
