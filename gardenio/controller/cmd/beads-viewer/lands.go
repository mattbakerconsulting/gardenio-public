package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	"gardenio/controller/internal/beadsapi"
)

// The Lands front of house (Track B spec
// gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md §2.3, step
// 7.5): the landing page, the three legal documents and the acceptance page a
// stranger meets after signing in with GitHub.
//
//   - GET /lands/, /lands/terms, /lands/privacy, /lands/aup and the meadow and
//     Homestead stylesheets under /lands/static/meadow/ and
//     /lands/static/homestead/ are routePublic and carry nothing
//     about the caller, so main.go also serves them to a visitor with no
//     session (noSessionPatterns).
//   - GET and POST /lands/accept are routeApplicant: any signed-in OIDC caller,
//     including an applicant with no gardenio role. They read and write only
//     the caller's own terms_version / terms_accepted_at, on the caller's own
//     bearer (kbeads GET/POST /v1/users/me/terms), so they cannot show or touch
//     anyone else's.
//
// The documents are embedded from lands/legal/, a byte-for-byte copy of
// gardenio/docs/lands/legal/ (TestLandsLegalMirrorsDocs keeps them equal). The
// three move together under one version id (that directory's README, rule 1),
// and that id is what acceptance records, so changing any text (7.1b filling
// the {{PLACEHOLDER}}s) means a new version and every user accepts again.
// TestLandsLegalVersionPinsText fails when the text changes and the version
// does not.

//go:embed lands/legal/*.md
var landsLegalFS embed.FS

// landsDocSlugs are the three documents in the order the pages list them.
var landsDocSlugs = []string{"terms", "privacy", "aup"}

// noSessionPatterns are the routes main.go serves without a session as well
// as behind it: the Lands front of house (the landing page, the three legal
// documents and the skins they wear) and the splash (plan gd-EJGA3zDGCk) with
// the two files it fetches. Each is routePublic in viewerRouteClasses
// (TestNoSessionPatternsArePublic), and the public edge and viewer-authproxy
// list the same paths (gardenio/helm/gardenio/tests/public_edge_lands_test.go
// keeps the three lists equal).
var noSessionPatterns = []string{
	"GET /lands/{$}",
	"GET /lands/terms",
	"GET /lands/privacy",
	"GET /lands/aup",
	"GET /lands/static/meadow/",
	"GET /lands/static/homestead/",
	"GET /garden/splash",
	"GET /garden/splash/three.module.min.js",
	"GET /garden/splash/still.png",
}

// mountNoSession serves noSessionPatterns on outer through public (the route
// mux without the auth chain), so a visitor with no session reaches them.
// Every other path, /lands/accept included, stays on outer's existing
// authenticated handler.
func mountNoSession(outer *http.ServeMux, public http.Handler) {
	for _, p := range noSessionPatterns {
		outer.Handle(p, public)
	}
}

// landsDoc is one legal document, parsed and rendered.
type landsDoc struct {
	Slug                 string
	Title                string
	Version              string
	Status               string // draft | published
	Effective            string
	RequiresReacceptance bool
	Body                 template.HTML
}

// Draft reports whether the document is not yet in force.
func (d *landsDoc) Draft() bool { return d.Status != "published" }

// landsLegal is the set of three documents under its one version.
type landsLegal struct {
	Version string
	Draft   bool
	Docs    []*landsDoc
	bySlug  map[string]*landsDoc
}

func (l *landsLegal) doc(slug string) *landsDoc { return l.bySlug[slug] }

var (
	landsLegalOnce sync.Once
	landsLegalSet  *landsLegal
	errLandsLegal  error
)

// currentLandsLegal returns the embedded documents, parsed once.
func currentLandsLegal() (*landsLegal, error) {
	landsLegalOnce.Do(func() {
		sub, err := fs.Sub(landsLegalFS, "lands/legal")
		if err != nil {
			errLandsLegal = err
			return
		}
		landsLegalSet, errLandsLegal = loadLandsLegal(sub)
	})
	return landsLegalSet, errLandsLegal
}

// loadLandsLegal reads <slug>.md for each of landsDocSlugs from fsys. It fails
// when a document is missing, its front matter names another doc, or the three
// versions differ: one id must name the set, because that id is all a user
// bead records.
func loadLandsLegal(fsys fs.FS) (*landsLegal, error) {
	set := &landsLegal{bySlug: map[string]*landsDoc{}}
	for _, slug := range landsDocSlugs {
		raw, err := fs.ReadFile(fsys, slug+".md")
		if err != nil {
			return nil, err
		}
		meta, body, err := splitFrontMatter(raw)
		if err != nil {
			return nil, fmt.Errorf("%s.md: %w", slug, err)
		}
		if meta["doc"] != slug {
			return nil, fmt.Errorf("%s.md: front matter doc is %q", slug, meta["doc"])
		}
		if meta["version"] == "" || !landsVersionPattern.MatchString(meta["version"]) {
			return nil, fmt.Errorf("%s.md: version %q is not a version id", slug, meta["version"])
		}
		html, err := renderLandsMarkdown(body)
		if err != nil {
			return nil, fmt.Errorf("%s.md: %w", slug, err)
		}
		d := &landsDoc{
			Slug:                 slug,
			Title:                meta["title"],
			Version:              meta["version"],
			Status:               meta["status"],
			Effective:            meta["effective"],
			RequiresReacceptance: meta["requires_reacceptance"] == "true",
			Body:                 html,
		}
		if set.Version == "" {
			set.Version = d.Version
		} else if d.Version != set.Version {
			return nil, fmt.Errorf("%s.md is version %s but terms.md is %s: the three documents move together", slug, d.Version, set.Version)
		}
		set.Draft = set.Draft || d.Draft()
		set.Docs = append(set.Docs, d)
		set.bySlug[slug] = d
	}
	return set, nil
}

// landsVersionPattern is what kbeads accepts as a terms_version
// (termsVersionPattern in kbeads/internal/server/http_user_terms.go).
var landsVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// splitFrontMatter separates the leading "---" YAML block (flat key: value
// lines only, which is all the legal documents use) from the Markdown body.
func splitFrontMatter(raw []byte) (map[string]string, []byte, error) {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return nil, nil, errors.New("no front matter")
	}
	head, body, ok := strings.Cut(s[len("---\n"):], "\n---\n")
	if !ok {
		return nil, nil, errors.New("front matter is not closed")
	}
	meta := map[string]string{}
	for _, line := range strings.Split(head, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		meta[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return meta, []byte(body), nil
}

// landsPlaceholder is a value the owner has not set yet ({{ENTITY}}). The
// pages show it as it is, marked, never filled: step 7.1b owns the values.
var landsPlaceholder = regexp.MustCompile(`\{\{[A-Z][A-Z0-9_]*\}\}`)

var landsMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(
		parser.WithAutoHeadingID(),
		parser.WithASTTransformers(util.Prioritized(landsLinkRewriter{}, 100)),
	),
)

// renderLandsMarkdown renders a document body. Raw HTML in the source is
// dropped (goldmark's default), the leading H1 is dropped (the page prints the
// front matter title), links between the three documents point at their
// /lands/ pages, each {{PLACEHOLDER}} is wrapped in a mark and each table in
// a scroll box.
func renderLandsMarkdown(src []byte) (template.HTML, error) {
	var buf bytes.Buffer
	if err := landsMarkdown.Convert(src, &buf); err != nil {
		return "", err
	}
	out := landsPlaceholder.ReplaceAllString(buf.String(), `<mark class="lands-ph">$0</mark>`)
	// A table wider than a phone scrolls inside its own box; the box is a
	// focusable, named region so a keyboard can scroll it too.
	out = strings.ReplaceAll(out, "<table>", `<div class="lands-table" role="region" aria-label="Table (scrolls sideways)" tabindex="0"><table>`)
	out = strings.ReplaceAll(out, "</table>", "</table></div>")
	return template.HTML(out), nil
}

// landsLinkRewriter points "terms.md#x" style links at the served pages
// ("terms#x"). A relative link to any other file (the directory's README,
// which is a note for maintainers and is not served) keeps its text and loses
// the link, so a reader never meets a 404.
type landsLinkRewriter struct{}

func (landsLinkRewriter) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	// The page prints the title (front matter) above the version line, so the
	// document's own leading H1 is dropped rather than shown twice.
	if h, ok := doc.FirstChild().(*ast.Heading); ok && h.Level == 1 {
		doc.RemoveChild(doc, h)
	}
	var unlink []*ast.Link
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		link, ok := n.(*ast.Link)
		if !entering || !ok {
			return ast.WalkContinue, nil
		}
		dest := string(link.Destination)
		if strings.Contains(dest, ":") || strings.HasPrefix(dest, "#") || strings.HasPrefix(dest, "/") {
			return ast.WalkContinue, nil
		}
		file, frag, _ := strings.Cut(dest, "#")
		slug := strings.TrimSuffix(path.Base(file), ".md")
		if strings.HasSuffix(file, ".md") && !strings.Contains(file, "/") && slices.Contains(landsDocSlugs, slug) {
			if frag != "" {
				slug += "#" + frag
			}
			link.Destination = []byte(slug)
			return ast.WalkContinue, nil
		}
		unlink = append(unlink, link)
		return ast.WalkContinue, nil
	})
	for _, link := range unlink {
		parent := link.Parent()
		for c := link.FirstChild(); c != nil; {
			next := c.NextSibling()
			parent.InsertBefore(parent, link, c)
			c = next
		}
		parent.RemoveChild(parent, link)
	}
}

// landsTermsAPI is the daemon surface the acceptance page uses, always on the
// caller's own bearer. *beadsapi.Client implements it; NewServer picks it up
// from the daemon client, and tests set Server.landsTerms directly.
type landsTermsAPI interface {
	MyTermsAsUser(ctx context.Context, bearer string) (*beadsapi.UserTerms, error)
	AcceptTermsAsUser(ctx context.Context, bearer, version string) (*beadsapi.UserTerms, error)
}

// Acceptance states the page renders.
const (
	landsStateNone        = "none"        // never accepted
	landsStateCurrent     = "current"     // accepted the version on screen
	landsStateStale       = "stale"       // accepted an older version
	landsStateNoBearer    = "nobearer"    // signed in without a forwardable OIDC token (AVA, dev bypass)
	landsStateUnavailable = "unavailable" // the daemon could not say
)

// landsView is the data every front-of-house page renders.
type landsView struct {
	BasePath string
	Theme    string
	Title    string
	Legal    *landsLegal
	Doc      *landsDoc

	// The acceptance page.
	State      string
	Accepted   string // the version the caller accepted, if any
	AcceptedAt string // human form of terms_accepted_at
	Notice     string
	Error      string
	// GitHubReady: the Lands GitHub App is configured, so an applicant who
	// has accepted can go on to connect their repository (lands_github.go).
	GitHubReady bool
	// ApplyOpen: the plot application is configured (lands_apply.go), so a
	// connected repository leads on to the form.
	ApplyOpen bool

	// The GitHub setup page (lands_github.go).
	GitHub landsGitHubView

	// The apply and applications pages (lands_apply.go).
	Apply landsApplyView

	// DemoOpen: a demo land is configured (lands_demo.go), so the home page
	// offers it to someone with no repository to bring.
	DemoOpen bool
	// The demo page (lands_demo.go).
	Demo landsDemoView
	// Offer is the caller's draft with the gatehouse steward, quoting the
	// message it came from (lands_gatehouse_reactor.go offerFor), on the
	// apply and demo pages; nil, none. OfferUsed: the apply form is filled
	// from it, because the caller chose to use it.
	Offer     *gatehouseOffer
	OfferUsed bool
	// LivePlan: the href of a public Window on a plan (landsLivePlanURL),
	// offered on the home page beside the demo; empty, no link.
	LivePlan string
	// LiveFarm: the href of a public Window on a whole land (the same
	// check, LANDS_LIVE_FARM_URL), offered beside LivePlan; empty, no link.
	LiveFarm string
	// GatehouseOpen: a gatehouse is configured (lands_gatehouse.go), so the
	// home page tells someone already let in where they will land, and that
	// every newcomer reads its #general.
	GatehouseOpen bool
	// Lands: the caller's own lands, on /lands/mine (lands_home.go).
	Lands []landsMineLand
}

// landsLivePlanEnv names a public Window (window.go) the Lands home links to
// as "See a live plan". A window expires and can be revoked, so the link is
// configured, never baked in: unset or empty, the home page has no link.
const landsLivePlanEnv = "LANDS_LIVE_PLAN_URL"

// landsLiveFarmEnv names a public land Window the Lands home links to as
// "See the whole farm" (LW.6, decision gd-ShRny4U2Ad), beside the live plan.
// Configured the same way and for the same reason.
const landsLiveFarmEnv = "LANDS_LIVE_FARM_URL"

// landsLivePlanURL checks a LANDS_LIVE_PLAN_URL value (landsWindowLinkURL).
func landsLivePlanURL(raw string) (string, error) {
	return landsWindowLinkURL(landsLivePlanEnv, raw)
}

// landsLiveFarmURL checks a LANDS_LIVE_FARM_URL value (landsWindowLinkURL).
func landsLiveFarmURL(raw string) (string, error) {
	return landsWindowLinkURL(landsLiveFarmEnv, raw)
}

// landsWindowLinkURL checks the value of env, a home-page window link: a
// same-origin window path, /w/gw_<token>, or an https URL. Anything else is
// refused, so a typo or a javascript: URL never reaches the front page.
// Empty is no link.
func landsWindowLinkURL(env, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if token, ok := strings.CutPrefix(raw, "/w/"); ok {
		if !wellFormedWindowToken(token) {
			return "", fmt.Errorf("%s=%q: want /w/gw_<token> or an https URL", env, raw)
		}
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("%s=%q: want /w/gw_<token> or an https URL", env, raw)
	}
	return raw, nil
}

// registerLandsRoutes mounts the front of house. Nothing here is operator-gated:
// the scope middleware admits an applicant to exactly these routes.
func (s *Server) registerLandsRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET /lands/{$}", s.handleLandsHome)
	for _, slug := range landsDocSlugs {
		mux.HandleFunc("GET /lands/"+slug, s.handleLandsDoc(slug))
	}
	mux.HandleFunc("GET /lands/accept", s.handleLandsAcceptPage)
	mux.HandleFunc("POST /lands/accept", s.handleLandsAccept)
	mux.HandleFunc("GET /lands/github/install", s.handleLandsGitHubInstall)
	mux.HandleFunc("GET /lands/github/setup", s.handleLandsGitHubSetup)
	mux.HandleFunc("GET /lands/apply", s.handleLandsApplyPage)
	mux.HandleFunc("POST /lands/apply", s.handleLandsApply)
	mux.HandleFunc("GET /lands/applications", s.handleLandsApplications)
	mux.HandleFunc("POST /lands/applications/{id}/withdraw", s.handleLandsWithdraw)
	mux.HandleFunc("GET /lands/mine", s.handleLandsMine)
	mux.HandleFunc("GET /lands/demo", s.handleLandsDemoPage)
	mux.HandleFunc("POST /lands/demo", s.handleLandsDemoAsk)
	mux.Handle("GET /lands/static/meadow/", http.StripPrefix("/lands/static/", s.static))
	mux.Handle("GET /lands/static/homestead/", http.StripPrefix("/lands/static/", s.static))
}

func (s *Server) landsBase(r *http.Request, title string) (landsView, bool) {
	legal, err := currentLandsLegal()
	if err != nil {
		s.logger.Error("lands legal documents failed to load", "error", err)
		return landsView{}, false
	}
	return landsView{BasePath: s.basePath, Theme: s.theme, Title: title, Legal: legal, GitHubReady: s.landsGitHub != nil, ApplyOpen: s.landsGate != nil && s.landsGitHub != nil, DemoOpen: s.landsDemo != nil, LivePlan: s.landsWindowHref(s.landsLivePlan), LiveFarm: s.landsWindowHref(s.landsLiveFarm), GatehouseOpen: s.landsGatehouse != ""}, true
}

// landsWindowHref is a home-page window link's href: a window path is the
// viewer's own, so it is served under the base path like every other link.
func (s *Server) landsWindowHref(link string) string {
	if strings.HasPrefix(link, "/") {
		return s.basePath + link
	}
	return link
}

func (s *Server) handleLandsHome(w http.ResponseWriter, r *http.Request) {
	v, ok := s.landsBase(r, "Gardenio Lands")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	s.renderLands(w, http.StatusOK, "lands_home", v)
}

func (s *Server) handleLandsDoc(slug string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.landsBase(r, "")
		if !ok {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		v.Doc = v.Legal.doc(slug)
		v.Title = landsShortTitle(v.Doc.Title)
		s.renderLands(w, http.StatusOK, "lands_doc", v)
	}
}

// handleLandsAcceptPage shows the three documents' version and whether the
// caller has accepted it.
func (s *Server) handleLandsAcceptPage(w http.ResponseWriter, r *http.Request) {
	v, ok := s.landsBase(r, "Accept the terms")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	s.fillLandsAcceptState(r.Context(), &v)
	if r.URL.Query().Get("accepted") == "1" && v.State == landsStateCurrent {
		v.Notice = "Thank you. Your acceptance of version " + v.Legal.Version + " is recorded."
	}
	s.renderLands(w, http.StatusOK, "lands_accept", v)
}

// handleLandsAccept records acceptance of the version the form showed. The
// form carries that version, and a mismatch with the current one (the
// documents changed while the page was open) is refused, so nobody is
// recorded as accepting text they did not see.
func (s *Server) handleLandsAccept(w http.ResponseWriter, r *http.Request) {
	v, ok := s.landsBase(r, "Accept the terms")
	if !ok {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// SameSite on the edge session cookie already keeps a cross-site form from
	// arriving signed in; refuse one that says it is cross-site anyway.
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	bearer := beadsapi.UserBearerFromContext(r.Context())
	if s.landsTerms == nil || bearer == "" {
		s.fillLandsAcceptState(r.Context(), &v)
		s.renderLands(w, http.StatusForbidden, "lands_accept", v)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if got := r.PostForm.Get("version"); got != v.Legal.Version {
		s.fillLandsAcceptState(r.Context(), &v)
		v.Error = "The documents changed while you were reading them. Read version " + v.Legal.Version + ", then accept it."
		s.renderLands(w, http.StatusConflict, "lands_accept", v)
		return
	}
	if _, err := s.landsTerms.AcceptTermsAsUser(r.Context(), bearer, v.Legal.Version); err != nil {
		s.logger.Warn("lands accept: daemon refused", "error", err)
		s.fillLandsAcceptState(r.Context(), &v)
		v.Error = "Your acceptance could not be recorded. Nothing was saved; try again in a minute."
		s.renderLands(w, http.StatusBadGateway, "lands_accept", v)
		return
	}
	http.Redirect(w, r, s.basePath+"/lands/accept?accepted=1", http.StatusSeeOther)
}

// fillLandsAcceptState asks the daemon, as the caller, what they accepted.
func (s *Server) fillLandsAcceptState(ctx context.Context, v *landsView) {
	bearer := beadsapi.UserBearerFromContext(ctx)
	if bearer == "" {
		v.State = landsStateNoBearer
		return
	}
	if s.landsTerms == nil {
		v.State = landsStateUnavailable
		return
	}
	mine, err := s.landsTerms.MyTermsAsUser(ctx, bearer)
	if err != nil {
		s.logger.Warn("lands accept page: reading own terms failed", "error", err)
		v.State = landsStateUnavailable
		return
	}
	v.Accepted = mine.TermsVersion
	v.AcceptedAt = landsWhen(mine.TermsAcceptedAt)
	switch mine.TermsVersion {
	case "":
		v.State = landsStateNone
	case v.Legal.Version:
		v.State = landsStateCurrent
	default:
		v.State = landsStateStale
	}
}

// landsShortTitle drops the product name from a document title: the pages
// already say where you are.
func landsShortTitle(title string) string {
	return strings.TrimPrefix(title, "Gardenio Lands ")
}

// landsWhen renders an RFC 3339 timestamp as "19 October 2026, 15:20 UTC".
func landsWhen(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.UTC().Format("2 January 2006, 15:04 UTC")
}

func (s *Server) renderLands(w http.ResponseWriter, status int, name string, v landsView) {
	tmpl, ok := s.pages["lands.html"]
	if !ok {
		s.logger.Error("lands template not loaded")
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// Clone before executing, as handleLogin does: the parsed set is shared
	// across Server instances.
	bound, err := tmpl.Clone()
	if err != nil {
		s.logger.Error("cloning lands template", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := bound.ExecuteTemplate(&buf, name, v); err != nil {
		s.logger.Error("lands template render error", "page", name, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(buf.Bytes()); err != nil && !isClientDisconnect(err) {
		s.logger.Debug("lands page write failed", "error", err)
	}
}
