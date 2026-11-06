package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/githubapp"
)

// The Lands GitHub App's setup leg (lands_github.go, Track B B-D3, step 7.6).

const ghApplicantID = int64(4242)

// fakeLandsGitHubApp records each check the setup handler asks for.
type fakeLandsGitHubApp struct {
	checks []githubapp.SetupCheck
	result githubapp.VerifiedRepo
	err    error
}

func (f *fakeLandsGitHubApp) VerifyLandsSetup(_ context.Context, c githubapp.SetupCheck) (githubapp.VerifiedRepo, error) {
	f.checks = append(f.checks, c)
	return f.result, f.err
}

func (f *fakeLandsGitHubApp) InstallURL(slug, state string) string {
	return "https://github.com/apps/" + slug + "/installations/new?" + url.Values{"state": {state}}.Encode()
}

func (f *fakeLandsGitHubApp) AuthorizeURL(state string) string {
	return "https://github.com/login/oauth/authorize?" + url.Values{"client_id": {"Iv1.test"}, "state": {state}}.Encode()
}

var ghNow = time.Date(2026, 10, 19, 18, 0, 0, 0, time.UTC)

func ghVerified() githubapp.VerifiedRepo {
	return githubapp.VerifiedRepo{
		InstallationID: 777, Repo: githubapp.RepoRef{Owner: "octo", Name: "garden"}, RepoID: 1001,
		Visibility: "public", GitHubID: ghApplicantID, GitHubLogin: "octocat",
		AccountCreated: time.Date(2015, 1, 2, 0, 0, 0, 0, time.UTC), PublicRepos: 12, Followers: 3,
	}
}

func ghApplicant() principal {
	p := applicantPrincipal()
	p.githubID = ghApplicantID
	return p
}

// ghViewer is the viewer as production chains it, with the Lands App set to
// app (nil: unconfigured).
func ghViewer(t *testing.T, app landsGitHubAPI) (http.Handler, *landsGitHub) {
	t.Helper()
	h, g, _ := ghViewerServer(t, app)
	return h, g
}

func ghViewerServer(t *testing.T, app landsGitHubAPI) (http.Handler, *landsGitHub, *Server) {
	t.Helper()
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	srv.landsTerms = &fakeLandsTerms{}
	var g *landsGitHub
	if app != nil {
		g = &landsGitHub{app: app, slug: "gardenio-lands", allowPrivate: true, maxRepoSizeMB: landsMaxRepoSizeDefaultMB, cookieKey: landsGitHubCookieKey([]byte("client-secret")), now: func() time.Time { return ghNow }}
		srv.landsGitHub = g
	}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return operatorMiddleware(scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })), g, srv
}

func ghDo(h http.Handler, path string, p principal, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := requestAs(http.MethodGet, path, p)
	r.Header.Set("X-Auth-Request-Access-Token", "stranger-access-token")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// ghCookie is the cookie a browser keeps: the last Set-Cookie of that name.
func ghCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	var got *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			got = c
		}
	}
	return got
}

// ghStart runs the install leg and returns the state cookie it set.
func ghStart(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := ghDo(h, "/lands/github/install", ghApplicant())
	c := ghCookie(rec, landsGitHubStateCookie)
	if rec.Code != http.StatusSeeOther || c == nil || c.Value == "" {
		t.Fatalf("install = %d, state cookie %v", rec.Code, c)
	}
	return c
}

func ghSetupPath(state string, extra url.Values) string {
	q := url.Values{"installation_id": {"777"}, "code": {"one-time-code"}, "setup_action": {"install"}, "state": {state}}
	for k, v := range extra {
		q[k] = v
	}
	return "/lands/github/setup?" + q.Encode()
}

// With the Lands App unconfigured, both legs answer 503, say so, and nothing
// is checked or stored. The accept page offers no step that leads there.
func TestLandsGitHub_FailsClosedWhenUnconfigured(t *testing.T) {
	h, _, srv := ghViewerServer(t, nil)
	state := &http.Cookie{Name: landsGitHubStateCookie, Value: "s"}
	for _, path := range []string{"/lands/github/install", ghSetupPath("s", nil), ghSetupPath("s", url.Values{"setup_action": {"request"}})} {
		rec := ghDo(h, path, ghApplicant(), state)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not open yet") {
			t.Fatalf("unconfigured GET %s = %d:\n%s", path, rec.Code, rec.Body.String())
		}
		if c := ghCookie(rec, landsGitHubResultCookie); c != nil {
			t.Fatalf("unconfigured GET %s set a result cookie", path)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Fatalf("unconfigured GET %s redirected to %s", path, loc)
		}
	}

	srv.landsTerms = &fakeLandsTerms{version: mustLandsLegal(t).Version, acceptedAt: "2026-10-19T15:20:00Z"}
	rec := ghDo(h, "/lands/accept", ghApplicant())
	if strings.Contains(rec.Body.String(), "/lands/github/install") || !strings.Contains(rec.Body.String(), "opens here soon") {
		t.Fatalf("accepted, App unconfigured: the accept page should not offer the GitHub step:\n%s", rec.Body.String())
	}
	srv.landsGitHub = &landsGitHub{app: &fakeLandsGitHubApp{}, slug: "gardenio-lands", now: time.Now}
	rec = ghDo(h, "/lands/accept", ghApplicant())
	if !strings.Contains(rec.Body.String(), `href="/lands/github/install">Connect your repository</a>`) {
		t.Fatalf("accepted, App configured: want the Connect step:\n%s", rec.Body.String())
	}
}

// landsGitHubFromEnv: nothing set is off, a partial config is an error, and
// both leave the server unconfigured (main.go).
func TestLandsGitHubFromEnv(t *testing.T) {
	read := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	if g, err := landsGitHubFromEnv(func(string) string { return "" }, read); g != nil || err != nil {
		t.Fatalf("empty env = %v, %v; want nil, nil", g, err)
	}
	partial := map[string]string{"LANDS_GITHUB_APP_ID": "42"}
	if g, err := landsGitHubFromEnv(func(k string) string { return partial[k] }, read); g != nil || err == nil {
		t.Fatalf("partial env = %v, %v; want nil and an error", g, err)
	}
}

func TestLandsGitHub_InstallRedirectsWithState(t *testing.T) {
	h, _ := ghViewer(t, &fakeLandsGitHubApp{})
	rec := ghDo(h, "/lands/github/install", ghApplicant())
	c := ghCookie(rec, landsGitHubStateCookie)
	if rec.Code != http.StatusSeeOther || c == nil {
		t.Fatalf("install = %d, cookie %v", rec.Code, c)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Host != "github.com" || loc.Path != "/apps/gardenio-lands/installations/new" || loc.Query().Get("state") != c.Value {
		t.Fatalf("install redirect = %s, state cookie %q", loc, c.Value)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/lands/github/" || c.MaxAge <= 0 || len(c.Value) < 32 {
		t.Fatalf("state cookie = %+v", c)
	}
	if again := ghStart(t, h); again.Value == c.Value {
		t.Fatal("two installs got the same state")
	}

	// No GitHub sign-in: the check could only fail, so say so instead of sending them to GitHub.
	rec = ghDo(h, "/lands/github/install", applicantPrincipal())
	if rec.Code != http.StatusForbidden || rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), "Sign in with GitHub first") {
		t.Fatalf("install without github_id = %d -> %q:\n%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
}

func TestLandsGitHub_SetupVerifies(t *testing.T) {
	app := &fakeLandsGitHubApp{result: ghVerified()}
	h, g := ghViewer(t, app)
	state := ghStart(t, h)

	rec := ghDo(h, ghSetupPath(state.Value, nil), ghApplicant(), state)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Repository connected") || !strings.Contains(body, "<code>octo/garden</code>") || !strings.Contains(body, "octocat") {
		t.Fatalf("setup = %d:\n%s", rec.Code, body)
	}
	want := githubapp.SetupCheck{InstallationID: 777, Code: "one-time-code", ApplicantGitHubID: ghApplicantID, AllowPrivate: true, MaxRepoSizeKB: 1024 * 1024}
	if len(app.checks) != 1 || app.checks[0] != want {
		t.Fatalf("checks = %+v, want [%+v]", app.checks, want)
	}
	if c := ghCookie(rec, landsGitHubStateCookie); c == nil || c.MaxAge >= 0 {
		t.Fatalf("the state cookie was not spent: %+v", c)
	}
	res := ghCookie(rec, landsGitHubResultCookie)
	if res == nil || !res.HttpOnly || res.SameSite != http.SameSiteLaxMode || res.Path != "/lands/" {
		t.Fatalf("result cookie = %+v", res)
	}
	if strings.Contains(res.Value, "one-time-code") {
		t.Fatal("the result cookie carries the code")
	}

	// update (the applicant changed the repository selection) is a setup too.
	state = ghStart(t, h)
	if rec := ghDo(h, ghSetupPath(state.Value, url.Values{"setup_action": {"update"}}), ghApplicant(), state); rec.Code != http.StatusOK {
		t.Fatalf("setup_action=update = %d", rec.Code)
	}

	// The result reads back for the applicant who passed, and nobody else.
	read := func(p principal, c *http.Cookie) (*landsGitHubResult, bool) {
		r := requestAs(http.MethodGet, "/lands/apply", p)
		if c != nil {
			r.AddCookie(c)
		}
		return g.verifiedFor(r.Context(), r)
	}
	got, ok := read(ghApplicant(), res)
	if !ok || got.Repo != "octo/garden" || got.InstallationID != 777 || got.GitHubID != ghApplicantID || got.Email != "stranger@example.com" {
		t.Fatalf("read back = %+v, %v", got, ok)
	}
	if days := got.AccountAgeDays(ghNow); days != 4308 {
		t.Errorf("account age = %d days", days)
	}
	other := ghApplicant()
	other.email = "someone@example.com"
	otherGH := ghApplicant()
	otherGH.githubID = 1
	noGH := applicantPrincipal()
	tampered := *res
	tampered.Value = strings.Replace(res.Value, res.Value[:4], "AAAA", 1)
	swapped := *res
	body64, sig, _ := strings.Cut(res.Value, ".")
	swapped.Value = body64 + "x." + sig
	for name, tc := range map[string]struct {
		p principal
		c *http.Cookie
	}{
		"another email":     {other, res},
		"another github id": {otherGH, res},
		"no github id":      {noGH, res},
		"tampered":          {ghApplicant(), &tampered},
		"body edited":       {ghApplicant(), &swapped},
		"no cookie":         {ghApplicant(), nil},
		"garbage":           {ghApplicant(), &http.Cookie{Name: landsGitHubResultCookie, Value: "nope"}},
	} {
		if _, ok := read(tc.p, tc.c); ok {
			t.Errorf("%s: the result read back", name)
		}
	}
	g.now = func() time.Time { return ghNow.Add(landsGitHubResultTTL) }
	if _, ok := read(ghApplicant(), res); ok {
		t.Error("an expired result read back")
	}
	var nilG *landsGitHub
	if _, ok := nilG.verifiedFor(context.Background(), requestAs(http.MethodGet, "/", ghApplicant())); ok {
		t.Error("an unconfigured App read a result")
	}
}

// Changing the repository selection on an existing install comes back as
// setup_action=update with no code (GitHub skips the OAuth step on updates).
// The handler sends the applicant through the OAuth authorize page once, and
// the return leg (code + state, no installation_id) verifies the installation
// remembered in the update cookie.
func TestLandsGitHub_SetupUpdateWithoutCode(t *testing.T) {
	app := &fakeLandsGitHubApp{result: ghVerified()}
	h, _ := ghViewer(t, app)
	state := ghStart(t, h)

	q := url.Values{"installation_id": {"777"}, "setup_action": {"update"}, "state": {state.Value}}
	rec := ghDo(h, "/lands/github/setup?"+q.Encode(), ghApplicant(), state)
	fresh := ghCookie(rec, landsGitHubStateCookie)
	upd := ghCookie(rec, landsGitHubUpdateCookie)
	if rec.Code != http.StatusSeeOther || fresh == nil || fresh.Value == "" || fresh.Value == state.Value || upd == nil || upd.Value != "777" {
		t.Fatalf("update without code = %d, state %v, update %v:\n%s", rec.Code, fresh, upd, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Host != "github.com" || loc.Path != "/login/oauth/authorize" || loc.Query().Get("state") != fresh.Value {
		t.Fatalf("update redirect = %s", loc)
	}
	if !upd.HttpOnly || upd.SameSite != http.SameSiteLaxMode || upd.Path != "/lands/github/" || upd.MaxAge <= 0 {
		t.Fatalf("update cookie = %+v", upd)
	}
	if len(app.checks) != 0 {
		t.Fatalf("checked before the code came back: %+v", app.checks)
	}

	back := url.Values{"code": {"one-time-code"}, "state": {fresh.Value}}
	rec = ghDo(h, "/lands/github/setup?"+back.Encode(), ghApplicant(), fresh, upd)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Repository connected") {
		t.Fatalf("authorize return = %d:\n%s", rec.Code, rec.Body.String())
	}
	want := githubapp.SetupCheck{InstallationID: 777, Code: "one-time-code", ApplicantGitHubID: ghApplicantID, AllowPrivate: true, MaxRepoSizeKB: 1024 * 1024}
	if len(app.checks) != 1 || app.checks[0] != want {
		t.Fatalf("checks = %+v, want [%+v]", app.checks, want)
	}
	if c := ghCookie(rec, landsGitHubUpdateCookie); c == nil || c.MaxAge >= 0 {
		t.Fatalf("the update cookie was not spent: %+v", c)
	}

	// The return leg without the update cookie has no installation to check.
	state = ghStart(t, h)
	back.Set("state", state.Value)
	if rec := ghDo(h, "/lands/github/setup?"+back.Encode(), ghApplicant(), state); rec.Code != http.StatusBadRequest {
		t.Fatalf("return leg without the update cookie = %d", rec.Code)
	}
	// A bad state on the update leg is refused before any redirect.
	q.Set("state", "forged")
	if rec := ghDo(h, "/lands/github/setup?"+q.Encode(), ghApplicant(), state); rec.Code == http.StatusSeeOther {
		t.Fatalf("forged state on update redirected to %s", rec.Header().Get("Location"))
	}
}

// Every way the setup can be refused: the status, the words, no result
// cookie, and (before the check) no call to GitHub.
func TestLandsGitHub_SetupRefusals(t *testing.T) {
	type tc struct {
		path    func(state string) string
		p       principal
		noState bool
		err     error
		code    int
		says    string
		checked bool // whether the GitHub check runs
	}
	refusal := func(r githubapp.RefuseReason) error { return &githubapp.SetupRefusal{Reason: r, Detail: "x"} }
	setup := func(extra url.Values) func(string) string {
		return func(s string) string { return ghSetupPath(s, extra) }
	}
	cases := map[string]tc{
		"no state cookie":       {path: setup(nil), noState: true, code: 403, says: "opened without the check we started"},
		"state mismatch":        {path: func(string) string { return ghSetupPath("forged", nil) }, code: 403, says: "opened without the check we started"},
		"no state param":        {path: setup(url.Values{"state": {""}}), code: 403, says: "opened without the check we started"},
		"no github sign-in":     {path: setup(nil), p: applicantPrincipal(), code: 403, says: "Sign in with GitHub first"},
		"no installation id":    {path: setup(url.Values{"installation_id": {""}}), code: 400, says: "without the installation details"},
		"bad installation id":   {path: setup(url.Values{"installation_id": {"12abc"}}), code: 400, says: "without the installation details"},
		"zero installation id":  {path: setup(url.Values{"installation_id": {"0"}}), code: 400, says: "without the installation details"},
		"no code":               {path: setup(url.Values{"code": {""}}), code: 400, says: "without the installation details"},
		"unknown setup_action":  {path: setup(url.Values{"setup_action": {"delete"}}), code: 400, says: "without the installation details"},
		"code refused":          {path: setup(nil), err: refusal(githubapp.RefuseCode), code: 403, says: "did not confirm your authorization", checked: true},
		"wrong github user":     {path: setup(nil), err: refusal(githubapp.RefuseWrongUser), code: 403, says: "different GitHub account", checked: true},
		"foreign installation":  {path: setup(nil), err: refusal(githubapp.RefuseInstallation), code: 403, says: "not one we can use", checked: true},
		"suspended":             {path: setup(nil), err: refusal(githubapp.RefuseSuspended), code: 403, says: "suspended", checked: true},
		"not exactly one repo":  {path: setup(nil), err: refusal(githubapp.RefuseRepoCount), code: 403, says: "exactly one repository", checked: true},
		"not admin":             {path: setup(nil), err: refusal(githubapp.RefuseNotAdmin), code: 403, says: "need admin access", checked: true},
		"visibility":            {path: setup(nil), err: refusal(githubapp.RefuseVisibility), code: 403, says: "visibility is not accepted", checked: true},
		"repository too large":  {path: setup(nil), err: refusal(githubapp.RefuseRepoSize), code: 403, says: "larger than a land takes (1024 MB at most)", checked: true},
		"wrong app":             {path: setup(nil), err: refusal(githubapp.RefuseWrongApp), code: 403, says: "did not pass", checked: true},
		"github unreachable":    {path: setup(nil), err: errors.New("dial tcp: timeout"), code: 502, says: "could not reach GitHub", checked: true},
		"org owner must accept": {path: setup(url.Values{"setup_action": {"request"}, "code": {""}, "installation_id": {""}}), code: 200, says: "Waiting for an organization owner"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			app := &fakeLandsGitHubApp{result: ghVerified(), err: c.err}
			if c.err != nil {
				app.result = githubapp.VerifiedRepo{}
			}
			h, _ := ghViewer(t, app)
			state := ghStart(t, h)
			p := c.p
			if p.email == "" {
				p = ghApplicant()
			}
			var cookies []*http.Cookie
			if !c.noState {
				cookies = append(cookies, state)
			}
			rec := ghDo(h, c.path(state.Value), p, cookies...)
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.says) {
				t.Fatalf("= %d, want %d saying %q:\n%s", rec.Code, c.code, c.says, rec.Body.String())
			}
			if res := ghCookie(rec, landsGitHubResultCookie); res != nil {
				t.Fatalf("a refused setup set the result cookie: %+v", res)
			}
			if (len(app.checks) > 0) != c.checked {
				t.Fatalf("GitHub checks = %d, want checked=%v", len(app.checks), c.checked)
			}
			if c.code != 200 && !strings.Contains(rec.Body.String(), `href="/lands/github/install">Start again</a>`) && c.says != "Sign in with GitHub first" {
				t.Fatalf("a refusal page should offer Start again:\n%s", rec.Body.String())
			}
		})
	}

	// The visibility refusal says what is accepted.
	app := &fakeLandsGitHubApp{err: refusal(githubapp.RefuseVisibility)}
	h, g := ghViewer(t, app)
	g.allowPrivate = false
	state := ghStart(t, h)
	rec := ghDo(h, ghSetupPath(state.Value, nil), ghApplicant(), state)
	if !strings.Contains(rec.Body.String(), "private repositories are not accepted yet") || app.checks[0].AllowPrivate {
		t.Fatalf("private off: check %+v, page:\n%s", app.checks, rec.Body.String())
	}

	// A state is spent by its first use, pass or fail.
	app = &fakeLandsGitHubApp{err: refusal(githubapp.RefuseCode)}
	h, _ = ghViewer(t, app)
	state = ghStart(t, h)
	rec = ghDo(h, ghSetupPath(state.Value, nil), ghApplicant(), state)
	if c := ghCookie(rec, landsGitHubStateCookie); c == nil || c.MaxAge >= 0 {
		t.Fatalf("a refused setup did not spend the state: %+v", c)
	}
}

func TestLandsMaxRepoSizeMB(t *testing.T) {
	for v, want := range map[string]int64{"": 1024, "200": 200, "0": 0, " 50 ": 50, "-3": 1024, "lots": 1024} {
		if got := githubapp.MaxRepoSizeMB(func(string) string { return v }); got != want {
			t.Errorf("%s=%q: %d, want %d", landsMaxRepoSizeEnv, v, got, want)
		}
	}
}

func TestCommonsMessageRate(t *testing.T) {
	for v, want := range map[string]int{"": 20, "5": 5, "0": 0, "-1": 20, "many": 20} {
		k := commonsMessageRate(func(string) string { return v })
		if k.Max() != want || k.Window() != time.Minute {
			t.Errorf("%s=%q: %d per %s, want %d per minute", commonsMessagesPerMinuteEnv, v, k.Max(), k.Window(), want)
		}
	}
}
