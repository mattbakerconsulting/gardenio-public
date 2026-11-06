package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"gardenio/controller/internal/gardenhub"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// The Lands front of house (lands.go, Track B §2.3, step 7.5).

// landsLegalPins is the sha256 of terms.md + privacy.md + aup.md, keyed by the
// version their front matter carries. Changing any of the text without a new
// version would record acceptance of words nobody accepted, so the test fails
// until the version moves. Filling the placeholders (step 7.1b) is a text
// change: bump the version in all three documents (docs/lands/legal/README.md,
// rules 1 and 2), copy them here, and add the new pin. Old pins stay, as the
// record of what each version said.
var landsLegalPins = map[string]string{
	"2026-10-19-draft.1": "b23bf1dbcbccff63fb89a0c4b4fbbe597016cfa6fb481557d56d9a507fd4c042",
}

const landsDocsDir = "../../../docs/lands/legal"

func mustLandsLegal(t *testing.T) *landsLegal {
	t.Helper()
	l, err := currentLandsLegal()
	if err != nil {
		t.Fatalf("embedded legal documents: %v", err)
	}
	return l
}

// The embedded copy is the docs directory, byte for byte.
func TestLandsLegalMirrorsDocs(t *testing.T) {
	for _, slug := range landsDocSlugs {
		want, err := os.ReadFile(filepath.Join(landsDocsDir, slug+".md"))
		if err != nil {
			t.Fatalf("reading the source of %s: %v", slug, err)
		}
		got, err := landsLegalFS.ReadFile("lands/legal/" + slug + ".md")
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("cmd/beads-viewer/lands/legal/%s.md differs from gardenio/docs/lands/legal/%s.md; copy the docs over: "+
				"cp gardenio/docs/lands/legal/{terms,privacy,aup}.md gardenio/controller/cmd/beads-viewer/lands/legal/", slug, slug)
		}
	}
}

func TestLandsLegalVersionPinsText(t *testing.T) {
	l := mustLandsLegal(t)
	h := sha256.New()
	for _, slug := range landsDocSlugs {
		raw, err := landsLegalFS.ReadFile("lands/legal/" + slug + ".md")
		if err != nil {
			t.Fatal(err)
		}
		h.Write(raw)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	pin, ok := landsLegalPins[l.Version]
	switch {
	case !ok:
		t.Fatalf("version %s has no pin; add %q: %q to landsLegalPins once the text is final", l.Version, l.Version, sum)
	case pin != sum:
		t.Fatalf("the legal text changed but its version is still %s: acceptance records the version, so every change "+
			"needs a new one (docs/lands/legal/README.md, rule 2). Bump version in all three documents and pin %q", l.Version, sum)
	}
}

func TestLandsLegalLoads(t *testing.T) {
	l := mustLandsLegal(t)
	if l.Version != "2026-10-19-draft.1" || !l.Draft || len(l.Docs) != 3 {
		t.Fatalf("legal set = version %q draft %v docs %d", l.Version, l.Draft, len(l.Docs))
	}
	ids := map[string]map[string]bool{}
	idRe := regexp.MustCompile(`\sid="([^"]+)"`)
	for _, d := range l.Docs {
		ids[d.Slug] = map[string]bool{}
		for _, m := range idRe.FindAllStringSubmatch(string(d.Body), -1) {
			ids[d.Slug][m[1]] = true
		}
	}
	hrefRe := regexp.MustCompile(`href="([^"]+)"`)
	for _, d := range l.Docs {
		body := string(d.Body)
		if d.Title == "" || strings.Contains(body, "<h1") {
			t.Errorf("%s: want a front matter title and the leading H1 dropped (the page prints the title)", d.Slug)
		}
		// Rendered as they are: the placeholders are shown, marked, not filled.
		if !strings.Contains(body, `<mark class="lands-ph">{{`) {
			t.Errorf("%s: placeholders are not shown marked", d.Slug)
		}
		if strings.Contains(body, "README") && strings.Contains(body, `href="README`) {
			t.Errorf("%s: links to the unserved README", d.Slug)
		}
		if strings.Count(body, "<table>") != strings.Count(body, `<div class="lands-table" role="region"`) {
			t.Errorf("%s: a table is not in its scroll region", d.Slug)
		}
		if strings.Contains(body, "<script") {
			t.Errorf("%s: raw HTML reached the page", d.Slug)
		}
		for _, m := range hrefRe.FindAllStringSubmatch(body, -1) {
			href := m[1]
			if strings.Contains(href, ":") {
				continue
			}
			file, frag, _ := strings.Cut(href, "#")
			if file == "" {
				file = d.Slug
			}
			if _, ok := ids[file]; !ok {
				t.Errorf("%s: link %q points at no served document", d.Slug, href)
				continue
			}
			if frag != "" && !ids[file][frag] {
				t.Errorf("%s: link %q points at no heading in %s", d.Slug, href, file)
			}
		}
	}
}

func TestLoadLandsLegal_RefusesMixedVersions(t *testing.T) {
	doc := func(slug, version string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("---\ndoc: " + slug + "\ntitle: T\nversion: " + version + "\nstatus: draft\n---\n# T\n")}
	}
	fsys := fstest.MapFS{"terms.md": doc("terms", "v2"), "privacy.md": doc("privacy", "v2"), "aup.md": doc("aup", "v1")}
	if _, err := loadLandsLegal(fsys); err == nil || !strings.Contains(err.Error(), "move together") {
		t.Fatalf("mixed versions loaded: %v", err)
	}
	fsys["aup.md"] = doc("aup", "v2")
	if l, err := loadLandsLegal(fsys); err != nil || l.Version != "v2" {
		t.Fatalf("matching versions: %v, %+v", err, l)
	}
	fsys["aup.md"] = doc("terms", "v2")
	if _, err := loadLandsLegal(fsys); err == nil {
		t.Fatal("a document whose front matter names another doc loaded")
	}
}

// fakeLandsTerms records the calls the acceptance page makes.
type fakeLandsTerms struct {
	version, acceptedAt string
	err                 error
	accepted            []string // "<bearer> <version>"
	readBearers         []string
}

func (f *fakeLandsTerms) MyTermsAsUser(_ context.Context, bearer string) (*beadsapi.UserTerms, error) {
	f.readBearers = append(f.readBearers, bearer)
	if f.err != nil {
		return nil, f.err
	}
	return &beadsapi.UserTerms{UserBeadID: "gd-user", TermsVersion: f.version, TermsAcceptedAt: f.acceptedAt}, nil
}

func (f *fakeLandsTerms) AcceptTermsAsUser(_ context.Context, bearer, version string) (*beadsapi.UserTerms, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.accepted = append(f.accepted, bearer+" "+version)
	f.version, f.acceptedAt = version, "2026-10-19T15:20:00Z"
	return &beadsapi.UserTerms{UserBeadID: "gd-user", TermsVersion: version, TermsAcceptedAt: f.acceptedAt}, nil
}

// landsViewer is the viewer as production chains it (operator then scope), with
// the given terms backend.
func landsViewer(t *testing.T, terms landsTermsAPI) http.Handler {
	t.Helper()
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	srv.landsTerms = terms
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return operatorMiddleware(scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p }))
}

// landsDo serves one request as p; bearer is the forwarded access token.
func landsDo(h http.Handler, method, path string, p principal, bearer string, form url.Values) *httptest.ResponseRecorder {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r = r.WithContext(requestAs(method, path, p).Context())
	if bearer != "" {
		r.Header.Set("X-Auth-Request-Access-Token", bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestLandsPublicPages(t *testing.T) {
	l := mustLandsLegal(t)
	h := landsViewer(t, nil)
	cases := map[string][]string{
		"/lands/":        {"A land for your repository", "prepaid credits granted by the operator", `href="/lands/accept"`, `href="/lands/terms"`, `href="/lands/privacy"`, `href="/lands/aup"`, l.Version, "Draft, not in force"},
		"/lands/terms":   {"Terms of Service", l.Version, `<mark class="lands-ph">{{ENTITY}}</mark>`, `href="aup"`},
		"/lands/privacy": {"Privacy Policy", l.Version, `<mark class="lands-ph">{{RETENTION_DAYS}}</mark>`},
		"/lands/aup":     {"Acceptable Use Policy", l.Version},
	}
	// The teensy music player (internal/tunes, gd-z8nt2TpS6o) on every page: the corner root and the script.
	for path := range cases {
		cases[path] = append(cases[path], `<div class="tunes corner" id="tunes" data-base="https://play.clonk.tv/"></div>`, "var TUNES = (function")
	}
	for path, wants := range cases {
		// Anonymous (no identity at all) and as an applicant: the same page.
		for _, p := range []principal{{}, applicantPrincipal()} {
			rec := landsDo(h, http.MethodGet, path, p, "", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s as %q = %d", path, p.email, rec.Code)
			}
			for _, w := range wants {
				if !strings.Contains(rec.Body.String(), w) {
					t.Errorf("GET %s as %q: missing %q", path, p.email, w)
				}
			}
		}
	}
	rec := landsDo(h, http.MethodGet, "/lands/static/meadow/meadow.css", principal{}, "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gd-login") {
		t.Fatalf("meadow stylesheet under /lands/static = %d", rec.Code)
	}
	// The Homestead skin the pages wear (step 4.1), and the art and font it reaches by relative url().
	for _, p := range []string{"/lands/static/homestead/homestead.css", "/lands/static/homestead/img/leaf.svg", "/lands/static/meadow/fonts/pixelify-sans-latin-600-normal.woff2"} {
		if rec := landsDo(h, http.MethodGet, p, principal{}, "", nil); rec.Code != http.StatusOK {
			t.Errorf("anonymous GET %s = %d", p, rec.Code)
		}
	}
}

// TestLandsLivePlanLink: the home page's "See a live plan" link (gd-PswcRiwYwS)
// is the configured href when set and absent when not, and a value that is
// neither a well-formed window path nor an https URL is refused.
func TestLandsLivePlanLink(t *testing.T) {
	const tok = "gw_AAAAAAAAAAAAAAAAAAAAAA"
	for _, c := range []struct{ base, raw, href string }{
		{"", "", ""},
		{"", "/w/" + tok, `href="/w/` + tok + `"`},
		{"/viewer", "/w/" + tok, `href="/viewer/w/` + tok + `"`},
		{"", " https://gardenio.example.com/w/" + tok + " ", `href="https://gardenio.example.com/w/` + tok + `"`},
	} {
		srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
		srv.basePath = c.base
		live, err := landsLivePlanURL(c.raw)
		if err != nil {
			t.Fatalf("landsLivePlanURL(%q): %v", c.raw, err)
		}
		srv.landsLivePlan = live
		mux := http.NewServeMux()
		srv.RegisterRoutes(mux)
		h := operatorMiddleware(scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p }))
		rec := landsDo(h, http.MethodGet, "/lands/", principal{}, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /lands/ with %q = %d", c.raw, rec.Code)
		}
		body := rec.Body.String()
		if c.href == "" {
			// No demo either: the whole "Just looking?" block is gone.
			if strings.Contains(body, "See a live plan") || strings.Contains(body, "Just looking?") {
				t.Errorf("GET /lands/ with no live plan: has the link or its heading")
			}
			continue
		}
		if !strings.Contains(body, c.href+">See a live plan →</a>") || !strings.Contains(body, "Just looking?") {
			t.Errorf("GET /lands/ with %q: want the link %s under Just looking?", c.raw, c.href)
		}
	}
	for _, bad := range []string{
		"/w/gw_short",
		"/w/" + tok + "/plan.json",
		"/w/" + tok + "?x=1",
		"/lands/demo",
		"w/" + tok,
		"http://gardenio.example.com/w/" + tok,
		"https://",
		"https://user:pw@gardenio.example.com/w/" + tok,
		"javascript:alert(1)",
		"//evil.example.com/w/" + tok,
	} {
		if got, err := landsLivePlanURL(bad); err == nil {
			t.Errorf("landsLivePlanURL(%q) = %q, want refused", bad, got)
		}
	}
}

// TestLandsLiveFarmLink: the home page's "See the whole farm" link (LW.6,
// gd-ShRny4U2Ad) shows under "Just looking?" on its own or beside the live
// plan, and its value is checked like the live plan's, naming its own env.
func TestLandsLiveFarmLink(t *testing.T) {
	const plan, farm = "gw_AAAAAAAAAAAAAAAAAAAAAA", "gw_BBBBBBBBBBBBBBBBBBBBBB"
	for _, c := range []struct{ base, plan, farm string }{
		{"", "", "/w/" + farm},
		{"/viewer", "/w/" + plan, "/w/" + farm},
		{"", "/w/" + plan, ""},
	} {
		srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
		srv.basePath = c.base
		srv.landsLivePlan = c.plan
		live, err := landsLiveFarmURL(c.farm)
		if err != nil {
			t.Fatalf("landsLiveFarmURL(%q): %v", c.farm, err)
		}
		srv.landsLiveFarm = live
		mux := http.NewServeMux()
		srv.RegisterRoutes(mux)
		h := operatorMiddleware(scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p }))
		rec := landsDo(h, http.MethodGet, "/lands/", principal{}, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /lands/ with farm %q = %d", c.farm, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Just looking?") {
			t.Errorf("GET /lands/ with farm %q plan %q: no Just looking? heading", c.farm, c.plan)
		}
		if c.farm == "" {
			if strings.Contains(body, "See the whole farm") {
				t.Errorf("GET /lands/ with no live farm: has the link")
			}
		} else if want := `href="` + c.base + c.farm + `">See the whole farm →</a>`; !strings.Contains(body, want) {
			t.Errorf("GET /lands/ with farm %q: want %s", c.farm, want)
		}
		if hasPlan := strings.Contains(body, "See a live plan"); hasPlan != (c.plan != "") {
			t.Errorf("GET /lands/ with plan %q: live plan link shown = %v", c.plan, hasPlan)
		}
	}
	for _, bad := range []string{"/w/gw_short", "javascript:alert(1)", "http://gardenio.example.com/w/" + farm} {
		if got, err := landsLiveFarmURL(bad); err == nil || !strings.Contains(err.Error(), landsLiveFarmEnv) {
			t.Errorf("landsLiveFarmURL(%q) = %q, %v; want refused naming %s", bad, got, err, landsLiveFarmEnv)
		}
	}
}

func TestLandsAccept_Flow(t *testing.T) {
	l := mustLandsLegal(t)
	terms := &fakeLandsTerms{}
	h := landsViewer(t, terms)
	app := applicantPrincipal()
	const bearer = "stranger-access-token"

	// Never accepted: the form, carrying the current version.
	rec := landsDo(h, http.MethodGet, "/lands/accept", app, bearer, nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `name="version" value="`+l.Version+`"`) || !strings.Contains(body, ">Accept</button>") {
		t.Fatalf("first visit = %d, want the accept form:\n%s", rec.Code, body)
	}
	if len(terms.readBearers) != 1 || terms.readBearers[0] != bearer {
		t.Fatalf("the page read terms as %v, want the caller's own bearer", terms.readBearers)
	}

	// A version the page did not show (the documents changed meanwhile): refused, nothing written.
	rec = landsDo(h, http.MethodPost, "/lands/accept", app, bearer, url.Values{"version": {"2026-01-01"}})
	if rec.Code != http.StatusConflict || len(terms.accepted) != 0 {
		t.Fatalf("stale version POST = %d, writes %v; want 409 and none", rec.Code, terms.accepted)
	}

	// The current version: recorded on the caller's bearer, then redirected.
	rec = landsDo(h, http.MethodPost, "/lands/accept", app, bearer, url.Values{"version": {l.Version}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/lands/accept?accepted=1" {
		t.Fatalf("accept POST = %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(terms.accepted) != 1 || terms.accepted[0] != bearer+" "+l.Version {
		t.Fatalf("recorded %v, want [%s %s]", terms.accepted, bearer, l.Version)
	}

	rec = landsDo(h, http.MethodGet, "/lands/accept?accepted=1", app, bearer, nil)
	body = rec.Body.String()
	if !strings.Contains(body, "Your acceptance of version "+l.Version+" is recorded") || strings.Contains(body, `name="version"`) {
		t.Fatalf("after accepting, want the confirmation and no form:\n%s", body)
	}
	rec = landsDo(h, http.MethodGet, "/lands/accept", app, bearer, nil)
	if !strings.Contains(rec.Body.String(), "You accepted version <code>"+l.Version+"</code> on 19 October 2026, 15:20 UTC") {
		t.Fatalf("returning visit, want the accepted line:\n%s", rec.Body.String())
	}

	// A later version of the documents (7.1b's fill bumps it): asked again.
	terms.version = "2026-10-01-draft.0"
	rec = landsDo(h, http.MethodGet, "/lands/accept", app, bearer, nil)
	body = rec.Body.String()
	if !strings.Contains(body, "The documents have changed since") || !strings.Contains(body, `value="`+l.Version+`"`) {
		t.Fatalf("stale acceptance, want the re-accept note and form:\n%s", body)
	}
}

func TestLandsAccept_Refusals(t *testing.T) {
	l := mustLandsLegal(t)
	app := applicantPrincipal()
	form := url.Values{"version": {l.Version}}

	// No forwarded bearer (AVA edge, dev bypass): nothing to write as.
	terms := &fakeLandsTerms{}
	h := landsViewer(t, terms)
	rec := landsDo(h, http.MethodGet, "/lands/accept", app, "", nil)
	if !strings.Contains(rec.Body.String(), "Accepting needs your own GitHub sign-in") || strings.Contains(rec.Body.String(), `name="version"`) {
		t.Fatalf("no bearer, want the sign-in note and no form:\n%s", rec.Body.String())
	}
	if rec := landsDo(h, http.MethodPost, "/lands/accept", app, "", form); rec.Code != http.StatusForbidden || len(terms.accepted) != 0 {
		t.Fatalf("POST with no bearer = %d, writes %v", rec.Code, terms.accepted)
	}

	// A form that says it came from another site.
	r := httptest.NewRequest(http.MethodPost, "/lands/accept", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("X-Auth-Request-Access-Token", "tok")
	r = r.WithContext(requestAs(http.MethodPost, "/lands/accept", app).Context())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || len(terms.accepted) != 0 {
		t.Fatalf("cross-site POST = %d, writes %v", rec.Code, terms.accepted)
	}

	// The daemon refuses or is down: say so, claim nothing.
	terms.err = errors.New("daemon down")
	rec = landsDo(h, http.MethodPost, "/lands/accept", app, "tok", form)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "could not be recorded") {
		t.Fatalf("daemon error POST = %d:\n%s", rec.Code, rec.Body.String())
	}
	rec = landsDo(h, http.MethodGet, "/lands/accept", app, "tok", nil)
	if !strings.Contains(rec.Body.String(), "cannot check or record your acceptance") {
		t.Fatalf("daemon error GET:\n%s", rec.Body.String())
	}
}

// TestApplicantReachesOnlyLandsRoutes is the Gate's viewer half (B-D1): a
// stranger signed in with GitHub, holding no gardenio role and no group,
// reaches the front of house and the viewer's static assets, and gets 403 from
// every other route the viewer registers. kbeads' half is
// TestApplicant_ForbiddenOnEveryBeadRoute.
func TestApplicantReachesOnlyLandsRoutes(t *testing.T) {
	m := registeredViewerRoutes(t)
	h := scopeChain(m)

	var reached []string
	refused := 0
	for _, pattern := range m.patterns {
		method, path := concretePath(pattern, "claimlayer")
		code := serve(h, method, path, applicantPrincipal())
		switch code {
		case http.StatusOK:
			reached = append(reached, pattern)
		case http.StatusForbidden:
			refused++
		default:
			t.Errorf("applicant %s %s = %d, want 200 or 403", method, path, code)
		}
		class := viewerRouteClasses[pattern]
		if want := class == routePublic || class == routeApplicant; (code == http.StatusOK) != want {
			t.Errorf("applicant %s (class %d) = %d", pattern, class, code)
		}
	}
	sort.Strings(reached)
	want := []string{
		"/w/", // the Window (PW.3): public, authorized on the window token alone
		"GET /assets/static/", "GET /garden/assets/{file}", "GET /garden/icon-180.png", "GET /garden/icon-192.png", "GET /garden/icon-512.png", "GET /garden/manifest.webmanifest",
		"GET /garden/splash", "GET /garden/splash/still.png", "GET /garden/splash/three.module.min.js", // the splash is public (gd-hzRHm1KB5r)
		"GET /lands/accept", "GET /lands/applications", "GET /lands/apply", "GET /lands/aup", "GET /lands/demo", "GET /lands/github/install", "GET /lands/github/setup", "GET /lands/mine", "GET /lands/privacy",
		"GET /lands/static/homestead/", "GET /lands/static/meadow/", "GET /lands/terms", "GET /lands/{$}", "GET /manifest.webmanifest",
		"GET /w/static/land.js", "GET /w/static/window.js", "GET /w/{token}", "GET /w/{token}/land.json", "GET /w/{token}/live", "GET /w/{token}/plan.json",
		"POST /lands/accept", "POST /lands/applications/{id}/withdraw", "POST /lands/apply", "POST /lands/demo",
	}
	if strings.Join(reached, "\n") != strings.Join(want, "\n") {
		t.Errorf("an applicant reaches:\n  %s\nwant exactly:\n  %s", strings.Join(reached, "\n  "), strings.Join(want, "\n  "))
	}
	if refused < 250 {
		t.Fatalf("only %d routes refused the applicant; the walk is not seeing the route table", refused)
	}

	// The applicant rule needs the verified-OIDC mark: the same empty role list
	// from the dev bypass or an internal caller stays unrestricted, as before.
	if code := serve(h, http.MethodGet, "/active", principal{email: "dev@example.com"}); code != http.StatusOK {
		t.Errorf("unverified no-role caller on /active = %d, want 200 (not an applicant)", code)
	}
	// And a role makes an OIDC user something other than an applicant.
	viewer := principal{email: "v@example.com", roles: []string{oidcauth.RoleViewer}, oidc: true}
	if code := serve(h, http.MethodGet, "/active", viewer); code != http.StatusOK {
		t.Errorf("OIDC viewer on /active = %d, want 200", code)
	}
	member := scopedPrincipal()
	member.oidc = true
	if code := serve(h, http.MethodGet, "/p/claimlayer", member); code != http.StatusOK {
		t.Errorf("OIDC project member on own project = %d, want 200", code)
	}
}

func TestNoSessionPatternsArePublic(t *testing.T) {
	m := registeredViewerRoutes(t)
	for _, p := range noSessionPatterns {
		if viewerRouteClasses[p] != routePublic {
			t.Errorf("%s is served without a session but is not routePublic", p)
		}
		if !slicesContains(m.patterns, p) {
			t.Errorf("%s is not registered", p)
		}
	}
}

// mountNoSession serves the public front of house and the splash without the
// auth chain and leaves everything else, /lands/accept included, on it.
func TestMountNoSession(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	srv.garden = &gardenhub.Handler{} // registers the splash
	app := http.NewServeMux()
	srv.RegisterRoutes(app)
	outer := http.NewServeMux()
	outer.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing authorization token", http.StatusUnauthorized)
	}))
	mountNoSession(outer, app)
	for path, want := range map[string]int{
		"/lands/": 200, "/lands/terms": 200, "/lands/privacy": 200, "/lands/aup": 200,
		"/lands/static/meadow/meadow.css":       200,
		"/lands/static/homestead/homestead.css": 200,
		"/garden/splash":                        200, // the splash and its two files (gd-hzRHm1KB5r)
		"/garden/splash/three.module.min.js":    200,
		"/garden/splash/still.png":              200,
		"/lands":                                301, // the mux's redirect to /lands/
		"/lands/accept":                         401, "/lands/apply": 401, "/lands/terms/x": 401,
		"/garden/splash/x": 401, "/garden": 401, "/garden/": 401,
		"/assets/static/meadow/meadow.css": 401, "/active": 401,
	} {
		rec := httptest.NewRecorder()
		outer.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("anonymous GET %s = %d, want %d", path, rec.Code, want)
		}
	}
	rec := httptest.NewRecorder()
	outer.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/lands/accept", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous POST /lands/accept = %d, want 401", rec.Code)
	}
}

func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
