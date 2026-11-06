package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/githubapp"
	"gardenio/controller/internal/golden"
	"gardenio/controller/internal/mailer"
)

// landsGoldenSurfaces is the tolerance of each surface testdata/lands_golden_browser.js shoots (golden.Tol says what
// the two numbers mean).
var landsGoldenSurfaces = map[string]golden.Tol{
	"lands":       golden.Default, // the landing page: what a land is, what it costs, Get started, the documents
	"landsterms":  golden.Default, // the Terms of Service page: version, draft badge, the first section
	"landsaccept": golden.Default, // the acceptance page for someone who has not accepted: the three documents and Accept
	// The GitHub App setup page (step 7.6): the repository connected, and a refusal (more than one repository).
	"landsgithub":        golden.Default,
	"landsgithubrefused": golden.Default,
	// The plot application (step 7.7): the form, and the applications page with an open application the operator asked
	// about and a denied one.
	"landsapply":        golden.Default,
	"landsapplications": golden.Default,
	// The form sent back with a refusal about one field (M2.4, gd-KiRStJd56I): that field in focus, its error above it.
	"landsapplyerror": golden.Default,
	// The sign-in picker (templates/login.html, plan gd-JaMxRKsc8z step 4.1): standalone like the Lands pages, and in
	// the same Homestead skin.
	"login": golden.Default,
	// The Window (plan gd-pz87vf3xsB, PW.3): a plan read through a window link with no session, and the one page every
	// window that does not open gets.
	"window":        golden.Default,
	"windowmissing": golden.Default,
}

// landsGoldenTerms is the acceptance state per bearer: the acceptance page's visitor has not accepted, the applicants
// on the apply pages have.
type landsGoldenTerms struct{ version string }

func (f *landsGoldenTerms) MyTermsAsUser(_ context.Context, bearer string) (*beadsapi.UserTerms, error) {
	if bearer == "applicant-access-token" {
		return &beadsapi.UserTerms{UserBeadID: "gd-user", TermsVersion: f.version, TermsAcceptedAt: "2026-10-19T15:20:00Z"}, nil
	}
	return &beadsapi.UserTerms{UserBeadID: "gd-user"}, nil
}

func (f *landsGoldenTerms) AcceptTermsAsUser(_ context.Context, _, version string) (*beadsapi.UserTerms, error) {
	return &beadsapi.UserTerms{UserBeadID: "gd-user", TermsVersion: version}, nil
}

// windowGoldenAPI opens testWindowTok on its payload and refuses every other token with kbeads' 401.
type windowGoldenAPI struct{ payload []byte }

func (f windowGoldenAPI) WindowView(_ context.Context, token string, _ int64) (json.RawMessage, error) {
	if token != testWindowTok {
		return nil, &beadsapi.APIError{StatusCode: http.StatusUnauthorized, Message: "invalid token"}
	}
	return f.payload, nil
}

// landsGoldenGitHub answers the setup page's check by installation: 1 passes, anything else is a repo_count refusal.
type landsGoldenGitHub struct{ fakeLandsGitHubApp }

func (f *landsGoldenGitHub) VerifyLandsSetup(_ context.Context, c githubapp.SetupCheck) (githubapp.VerifiedRepo, error) {
	if c.InstallationID == 1 {
		return ghVerified(), nil
	}
	return githubapp.VerifiedRepo{}, &githubapp.SetupRefusal{Reason: githubapp.RefuseRepoCount, Detail: "golden"}
}

// TestLandsGoldens is the looking-glass gate for the Lands front of house (step 7.5) and the Window's page (PW.3). Chromium opens the landing page and
// the Terms anonymously, the acceptance page as an applicant (verified OIDC, no role) who has not accepted yet, the
// GitHub App setup page (step 7.6) passed and refused for that applicant signed in with GitHub, and the plot
// application form and applications page (step 7.7), at a
// 390 px phone and a 1280 px desktop, in the system's light and dark schemes. Each PNG is held against
// testdata/golden/lands/<same name> by golden.Check; failing shots go to golden.ArtifactsDir("lands").
//
// The same switches as TestComposerGoldens: GARDENHUB_GOLDENS=1 to run (make test-garden-goldens), Linux only,
// GARDENHUB_GOLDEN_UPDATE=1 to rewrite goldens, GARDENHUB_BROWSER_REQUIRED=1 to fail instead of skip.
// LANDS_GOLDEN_FULL=<dir> also writes full-page shots there, for grading.
func TestLandsGoldens(t *testing.T) {
	skip := t.Skipf
	if os.Getenv("GARDENHUB_BROWSER_REQUIRED") == "1" {
		skip = t.Fatalf
	}
	node, mods, why := golden.Browser()
	if why != "" {
		skip("%s", why)
	}
	// A directory of their own: golden.Check treats every PNG in its Goldens dir as one of its shots, and
	// testdata/golden/ holds the composer's.
	goldens, _ := filepath.Abs(filepath.Join("testdata", "golden", "lands"))
	script, _ := filepath.Abs(filepath.Join("testdata", "lands_golden_browser.js"))
	fonts, _ := filepath.Abs(filepath.Join("..", "..", "internal", "gardenhub", "testdata", "golden", "fonts"))

	h, gh, viewer := ghViewerServer(t, &landsGoldenGitHub{})
	viewer.landsTerms = &landsGoldenTerms{version: mustLandsLegal(t).Version}
	// The plot application (step 7.7): a Gate over a fake intake. The applications page's applicant has an open
	// application the operator asked about and an older, denied one; the form's applicant has none.
	intake := newFakeIntake()
	asked := intake.seedApplication("sub-gardener", "tomato-patch", "octo/garden", "submitted")
	_ = intake.UpdateBeadFieldsTyped(context.Background(), asked, map[string]any{"question": "Which branch should the agents open pull requests against?", "asked_at": "2026-10-19T17:00:00Z"})
	denied := intake.seedApplication("sub-gardener", "bean-row", "octo/beans", "denied")
	_ = intake.UpdateBeadFieldsTyped(context.Background(), denied, map[string]any{"deny_reason": "The repository is empty, so there is nothing for agents to work on yet. Push some code and apply again."})
	gate, err := landsGateFromEnv(func(string) string { return "" }, intake, nil, mailer.NewRecorder(), mailer.NewTemplates(mailer.Config{BaseURL: "https://gardenio.example.com"}), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	viewer.landsGate = gate
	// The Window (PW.3): testWindowTok opens testdata/window_golden_payload.json (planwindow's phased fixture, eight days
	// earlier), read at the browser's frozen noon so the page's "3 d ago"s are the ones window.js would say; any other
	// token is refused, as kbeads refuses one.
	payload, err := os.ReadFile(filepath.Join("testdata", "window_golden_payload.json"))
	if err != nil {
		t.Fatal(err)
	}
	viewer.window = newWindowCache(windowGoldenAPI{payload})
	viewer.window.now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }
	// The home page's "See a live plan" link (gd-PswcRiwYwS) opens that same window.
	viewer.landsLivePlan = "/w/" + testWindowTok
	applicant := stranger("stranger@example.com", "sub-stranger", ghApplicantID)
	result, err := gh.sign(landsGitHubResult{Email: applicant.email, GitHubID: ghApplicantID, GitHubLogin: "octocat", InstallationID: 777,
		Repo: "octo/garden", Visibility: "public", Expires: ghNow.Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	// /lands/accept, /lands/github/setup and the apply pages are behind sign-in: every request to them is an applicant's
	// (signed in with GitHub), with a forwarded access token, as the edge would send it. The setup page also carries the
	// state cookie its install leg would have set, and the form the setup result. The public pages are anonymous.
	login := viewer.handleLogin(loginPickerConfig{enabled: true, forwardAuthURL: "https://auth.example.com",
		keycloakLocalEnabled: true, keycloakLocalLabel: "Sign in with password", gitlabEnabled: true, gitlabLabel: "Sign in with GitLab"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			// main.go serves the picker on the public mux, outside RegisterRoutes.
			login.ServeHTTP(w, r)
			return
		case "/lands/accept", "/lands/github/setup":
			r = r.WithContext(requestAs(r.Method, r.URL.Path, ghApplicant()).Context())
			r.Header.Set("X-Auth-Request-Access-Token", "stranger-access-token")
			r.AddCookie(&http.Cookie{Name: landsGitHubStateCookie, Value: "golden"})
		case "/lands/apply":
			r = r.WithContext(requestAs(r.Method, r.URL.Path, applicant).Context())
			r.Header.Set("X-Auth-Request-Access-Token", "applicant-access-token")
			r.AddCookie(&http.Cookie{Name: landsGitHubResultCookie, Value: result})
		case "/lands/applications":
			r = r.WithContext(requestAs(r.Method, r.URL.Path, stranger("gardener@example.com", "sub-gardener", 77)).Context())
			r.Header.Set("X-Auth-Request-Access-Token", "applicant-access-token")
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	shots := t.TempDir()
	args := []string{script, srv.URL, mods, shots, fonts}
	if full := os.Getenv("LANDS_GOLDEN_FULL"); full != "" {
		args = append(args, full)
	}
	cmd := exec.Command(node, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
		skip("no Chromium for playwright-core: %s", strings.TrimSpace(string(out)))
	}
	if err != nil {
		t.Fatalf("lands capture failed: %v\n%s", err, out)
	}
	t.Logf("capture:\n%s", out)

	arts := golden.ArtifactsDir("lands")
	r, err := golden.Check(golden.Options{Shots: shots, Goldens: goldens, Artifacts: arts, Update: os.Getenv("GARDENHUB_GOLDEN_UPDATE") == "1", Surfaces: landsGoldenSurfaces})
	for _, l := range r.Logs {
		t.Log(l)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Failed) > 0 {
		t.Fatalf("%d lands golden(s) differ; actual/expected/diff PNGs are in %s. An intentional change? Commit no golden: the window bless PR (bless-goldens.yml) re-shoots them on main.\n  %s",
			len(r.Failed), arts, strings.Join(r.Failed, "\n  "))
	}
	t.Logf("%d shots match their goldens", r.Shots)
}
