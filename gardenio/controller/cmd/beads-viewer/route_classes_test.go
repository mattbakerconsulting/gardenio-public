package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// recordingMux is the routeRegistrar the route-class tests hand to
// RegisterRoutes: it records every pattern the Server registers and mounts a
// stub 200 handler for it on a real ServeMux, so the same registration drives
// both the "every route is classified" walk and the middleware matrix (real
// handlers would need a daemon fixture per route and could fail for reasons
// that have nothing to do with the scope gate).
type recordingMux struct {
	mux      *http.ServeMux
	patterns []string
}

func newRecordingMux() *recordingMux { return &recordingMux{mux: http.NewServeMux()} }

func (m *recordingMux) add(pattern string) {
	m.patterns = append(m.patterns, pattern)
	m.mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func (m *recordingMux) Handle(pattern string, _ http.Handler) { m.add(pattern) }
func (m *recordingMux) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	m.add(pattern)
}

// registeredViewerRoutes registers the viewer's full route table (garden and
// Commons routes included, they are mounted conditionally) on a recording mux.
func registeredViewerRoutes(t *testing.T) *recordingMux {
	t.Helper()
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	srv.garden = &gardenhub.Handler{}
	srv.commons = &commons.Handler{}
	m := newRecordingMux()
	srv.RegisterRoutes(m)
	if len(m.patterns) < 100 {
		t.Fatalf("RegisterRoutes recorded only %d patterns; the walk is not seeing the route table", len(m.patterns))
	}
	return m
}

// unclassifiedViewerRoutes returns the patterns with no entry in table.
func unclassifiedViewerRoutes(patterns []string, table map[string]routeClass) []string {
	var out []string
	for _, p := range patterns {
		if _, ok := table[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// TestEveryViewerRouteHasClass fails the build when a route the viewer registers
// has no class, or a class entry names a route that is no longer registered, so
// the table cannot drift from the mux and a new route cannot reach a scoped
// principal by being forgotten.
func TestEveryViewerRouteHasClass(t *testing.T) {
	m := registeredViewerRoutes(t)

	if missing := unclassifiedViewerRoutes(m.patterns, viewerRouteClasses); len(missing) > 0 {
		t.Errorf("viewer routes with no entry in viewerRouteClasses (add each as routeScoped, routeAdminOnly, routeApplicant, routePublic or routeMachine):\n  %s",
			strings.Join(missing, "\n  "))
	}

	registered := map[string]bool{}
	for _, p := range m.patterns {
		registered[p] = true
	}
	var stale []string
	for p, class := range viewerRouteClasses {
		if !registered[p] {
			stale = append(stale, p)
		}
		if class != routeAdminOnly && class != routeScoped && class != routePublic && class != routeApplicant && class != routeMachine {
			t.Errorf("%q has an unknown route class %d", p, class)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("viewerRouteClasses entries that are not registered routes:\n  %s", strings.Join(stale, "\n  "))
	}
}

// TestViewerRouteClassGuard_ReportsUnclassified is the positive control for the
// walk above: a route the table does not know about must be reported, and a
// classified one must not, so the guard cannot go green by matching nothing
// (gd-PXWguyutzL).
func TestViewerRouteClassGuard_ReportsUnclassified(t *testing.T) {
	got := unclassifiedViewerRoutes(
		[]string{"GET /active", "GET /brand-new-page", "POST /brand-new-write"},
		viewerRouteClasses,
	)
	want := []string{"GET /brand-new-page", "POST /brand-new-write"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unclassified = %v, want %v", got, want)
	}

	// And end to end: a scoped principal gets 403 on a route nobody classified.
	m := newRecordingMux()
	m.HandleFunc("GET /brand-new-page", nil)
	h := scopeMiddleware(m.mux, func(r *http.Request) string { _, p := m.mux.Handler(r); return p })
	if code := serve(h, "GET", "/brand-new-page", scopedPrincipal()); code != http.StatusForbidden {
		t.Errorf("scoped GET on an unclassified route = %d, want 403", code)
	}
	if code := serve(h, "GET", "/brand-new-page", adminPrincipal()); code != http.StatusOK {
		t.Errorf("admin GET on an unclassified route = %d, want 200", code)
	}
}

// principal is the verified identity a test request carries, in the shape the
// OIDC middleware leaves it in the request context.
type principal struct {
	email  string
	roles  []string
	groups []string
	// oidc marks the identity as coming from a verified OIDC token, as the
	// Keycloak middleware does. Only such a caller can be an applicant.
	oidc bool
	// githubID is the token's github_id claim (0: none), as oidcauth reads it.
	githubID int64
	// sub is the token's sub claim ("": none).
	sub string
	// githubLogin is the token's github_login claim ("": none).
	githubLogin string
}

func scopedPrincipal() principal {
	return principal{
		email:  "cl@claimlayer.example",
		roles:  []string{oidcauth.RoleProjectMember},
		groups: []string{"/project-claimlayer"},
	}
}

func adminPrincipal() principal {
	return principal{email: "matt@example.com", roles: []string{oidcauth.RoleAdmin}}
}

// applicantPrincipal is a stranger signed in with GitHub: a verified OIDC
// token, no gardenio role, no group (Lands Track B B-D1). Keycloak's own realm
// roles ride along and grant nothing.
func applicantPrincipal() principal {
	return principal{email: "stranger@example.com", roles: []string{"offline_access", "default-roles-gardenio"}, oidc: true}
}

// requestAs builds a request carrying p's verified identity and a user bearer.
func requestAs(method, path string, p principal) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	return r.WithContext(withPrincipal(r.Context(), p))
}

// withPrincipal returns ctx carrying p's verified identity.
func withPrincipal(ctx context.Context, p principal) context.Context {
	ctx = oidcauth.ContextWithEmail(ctx, p.email)
	ctx = oidcauth.ContextWithRoles(ctx, p.roles)
	ctx = oidcauth.ContextWithGroups(ctx, p.groups)
	if p.oidc {
		ctx = oidcauth.ContextWithOIDCVerified(ctx)
	}
	if p.githubID != 0 {
		ctx = oidcauth.ContextWithGitHubID(ctx, p.githubID)
	}
	if p.sub != "" {
		ctx = oidcauth.ContextWithSubject(ctx, p.sub)
	}
	if p.githubLogin != "" {
		ctx = oidcauth.ContextWithGitHubLogin(ctx, p.githubLogin)
	}
	return ctx
}

// serve runs one request through h as p and returns the status code.
func serve(h http.Handler, method, path string, p principal) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestAs(method, path, p))
	return rec.Code
}

// concretePath turns a mux pattern into a request (method, path) that matches
// it, filling wildcards with a fixed token (or project for the per-project
// routes, so the request names the principal's own project).
func concretePath(pattern, project string) (method, path string) {
	method = http.MethodGet
	path = pattern
	if m, p, ok := strings.Cut(pattern, " "); ok {
		method, path = m, p
	}
	path = strings.ReplaceAll(path, "{$}", "")
	if param, ok := viewerProjectParams[pattern]; ok {
		path = strings.ReplaceAll(path, "{"+param+"}", project)
	}
	for {
		i := strings.Index(path, "{")
		if i < 0 {
			break
		}
		j := strings.Index(path[i:], "}")
		name := path[i+1 : i+j]
		fill := "x"
		if strings.HasSuffix(name, "...") {
			fill = "x/y"
		}
		path = path[:i] + fill + path[i+j+1:]
	}
	if path == "" {
		path = "/"
	}
	return method, path
}

func scopeChain(m *recordingMux) http.Handler {
	return scopeMiddleware(m.mux, func(r *http.Request) string {
		_, pattern := m.mux.Handler(r)
		return pattern
	})
}

// TestScopedViewer_AdminOnlyRoutes403 drives every classified route through the
// scope middleware as a scoped and as an unrestricted identity. Admin-only
// routes are 403 for the scoped principal and served for the admin; scoped and
// public routes are served for both; machine routes are 403 for both.
func TestScopedViewer_AdminOnlyRoutes403(t *testing.T) {
	m := registeredViewerRoutes(t)
	h := scopeChain(m)

	var adminOnly, scoped, public, applicant, machine int
	for _, pattern := range m.patterns {
		method, path := concretePath(pattern, "claimlayer")
		class := viewerRouteClasses[pattern]

		code := serve(h, method, path, adminPrincipal())
		if class == routeMachine {
			machine++
			if code != http.StatusForbidden {
				t.Errorf("admin %s %s (%s) = %d, want 403", method, path, pattern, code)
			}
		} else if code != http.StatusOK {
			t.Errorf("admin %s %s = %d, want 200", method, path, code)
		}
		code = serve(h, method, path, scopedPrincipal())
		switch class {
		case routeMachine:
			if code != http.StatusForbidden {
				t.Errorf("scoped %s %s (%s) = %d, want 403", method, path, pattern, code)
			}
		case routeAdminOnly:
			adminOnly++
			if code != http.StatusForbidden {
				t.Errorf("scoped %s %s (%s) = %d, want 403", method, path, pattern, code)
			}
		case routeScoped:
			scoped++
			if code != http.StatusOK {
				t.Errorf("scoped %s %s (%s) = %d, want 200", method, path, pattern, code)
			}
		case routePublic:
			public++
			if code != http.StatusOK {
				t.Errorf("scoped %s %s (%s) = %d, want 200", method, path, pattern, code)
			}
		case routeApplicant:
			applicant++
			if code != http.StatusOK {
				t.Errorf("scoped %s %s (%s) = %d, want 200", method, path, pattern, code)
			}
		}
	}
	// Positive control on the matrix itself: every class was exercised.
	if adminOnly == 0 || scoped == 0 || public == 0 || applicant == 0 || machine == 0 {
		t.Fatalf("matrix exercised admin-only=%d scoped=%d public=%d applicant=%d machine=%d; each must be > 0", adminOnly, scoped, public, applicant, machine)
	}
}

// TestScopedViewer_StartingClassification pins the classification the task
// fixed up front, so loosening any of them is a visible test edit.
func TestScopedViewer_StartingClassification(t *testing.T) {
	adminOnly := []string{
		"GET /costs", "GET /costs/rollup", "GET /grafana", "/grafana/{path...}",
		"GET /taints", "GET /taints/{name}", "POST /taints", "GET /triggers",
		"POST /triggers/{id}/toggle", "GET /telepathy", "GET /telepathy/events",
		"GET /teams/{team}", "GET /settings", "POST /settings",
		"GET /claude-config", "GET /advice", "POST /advice", "GET /gardeners",
		"POST /gardener/{id}/enable", "POST /agents/{id}/input",
		"POST /agents/{id}/respond",
	}
	scoped := []string{
		"GET /{id}", "GET /active", "GET /decisions", "GET /escalations",
		"GET /plans", "GET /plans/{id}", "GET /agents", "GET /p/{project}",
	}
	public := []string{"GET /assets/static/", "GET /manifest.webmanifest"}

	for _, p := range adminOnly {
		if c := viewerRouteClasses[p]; c != routeAdminOnly {
			t.Errorf("%s class = %d, want admin-only", p, c)
		}
	}
	for _, p := range scoped {
		if c := viewerRouteClasses[p]; c != routeScoped {
			t.Errorf("%s class = %d, want scoped", p, c)
		}
	}
	for _, p := range public {
		if c := viewerRouteClasses[p]; c != routePublic {
			t.Errorf("%s class = %d, want public", p, c)
		}
	}
	// Every write is admin-only but the Commons' own (Lands Commons I step 9,
	// and the typing signal of Commons III step 4.4) and the Lands terms
	// acceptance: viewer writes are operator-gated, a Commons write route is
	// scoped and gated in its handler by the land-role action table
	// (commonsGate), with TestCommonsWritesScoped walking each one, and
	// POST /lands/accept is routeApplicant, writing only the caller's own two
	// terms fields on their own bearer (lands.go). Marking a channel read
	// (Commons III step 5) is scoped too: it writes only the viewer's own read
	// position, on the viewer's bearer (TestCommonsScopedAcceptance).
	writeClass := map[string]routeClass{
		"POST /garden/c/{land}/{channel}/say": routeScoped, "POST /garden/c/t/{thread}/reply": routeScoped,
		"POST /garden/c/{land}/{channel}/join": routeScoped, "POST /garden/c/{land}/{channel}/leave": routeScoped,
		"POST /garden/c/{land}/{channel}/read":   routeScoped,
		"POST /garden/c/{land}/{channel}/typing": routeScoped,
		// the Lands kill switch: scoped, and answered 403 for anyone but an admin
		"POST /garden/c/{land}/{channel}/freeze": routeScoped, "POST /garden/c/{land}/{channel}/thaw": routeScoped,
		// member invites (Lands step 8.9): scoped, answered 403 for anyone
		// without member_manage in the land (its owner, or an admin); the
		// land_invite write goes out as the Gate's machine principal
		"POST /garden/c/{land}/{channel}/invite": routeScoped, "POST /garden/c/{land}/{channel}/revoke": routeScoped,
		// card buttons and lightboxes (Commons II step 5): gated by the action
		// table for the card's land, acted on the viewer's bearer
		"POST /garden/c/action/{id}": routeScoped, "POST /garden/c/submit/{id}": routeScoped,
		// a wish head's Done (Commons II step 6): gated by the action table
		// for the head's land, closed by the bridge on the viewer's bearer
		"POST /garden/c/t/{thread}/done": routeScoped,
		// the page's one connection (gardenhub/livemux.go, P2.4): adds or lets
		// go streams on the caller's own connection and reads nothing itself;
		// each stream it opens is re-authorised by its own GET route through
		// scopeMiddleware on that request (Handler.Live, main.go)
		"POST /garden/live/sub": routeScoped,
		// the farm stream's view (gd-l7yYxZpoZo): writes nothing, only narrows
		// which calls a stream sends; each call and transcript row is still
		// cut by that stream's own subscriber (farm_cut_stream.go, transcript.go)
		"POST /garden/farm/events/view": routeScoped,
		"POST /garden/welcome":          routeScoped, // the welcome tour was had (9.14b, garden_welcome.go): own user bead, own bearer
		// a land's steward-notes Holds (gd-q4NgYNGpy8, gardenhub/land_notes.go):
		// gated by viewerScope.may(edit_holds) for the land, patched on the
		// viewer's bearer, which kbeads holds to the Holds section
		"POST /garden/notes/{land}/holds": routeScoped,
		"POST /lands/accept":              routeApplicant,
		// The plot application (lands_apply.go): the Gate writes as its machine
		// principal, keyed on the caller's own subject, never their bearer.
		"POST /lands/apply": routeApplicant, "POST /lands/applications/{id}/withdraw": routeApplicant,
		// The demo land's door (lands_demo.go): a guest pass for the caller's
		// own verified github_id, filed as the same machine principal.
		"POST /lands/demo": routeApplicant,
		// The plan cockpit's Share panel (PW.4, plan_share.go): written on the
		// person's own bearer alone; kbeads admits only the land's owner or an
		// operator (TestPlanShare_ScopedMember walks a refusal).
		"POST /plans/{id}/share": routeScoped, "POST /plans/{id}/share/{window}/revoke": routeScoped,
		// The land's Share tab (LW.5, land_share.go): the same gate on a whole
		// land (TestLandShare_ScopedMember walks a refusal).
		"POST /p/{project}/share": routeScoped, "POST /p/{project}/share/{window}/revoke": routeScoped,
	}
	for p, c := range viewerRouteClasses {
		if strings.HasPrefix(p, "GET ") || strings.HasPrefix(p, "/") {
			continue
		}
		want, ok := writeClass[p]
		if !ok {
			want = routeAdminOnly
		}
		if c != want {
			t.Errorf("write route %s is %d, want %d", p, c, want)
		}
	}
}

// TestScopedViewer_ProjectPagesOnlyForOwnProject: the per-project pages are
// reachable for the principal's own project and refused for any other.
func TestScopedViewer_ProjectPagesOnlyForOwnProject(t *testing.T) {
	m := registeredViewerRoutes(t)
	h := scopeChain(m)
	for pattern := range viewerProjectParams {
		_, own := concretePath(pattern, "claimlayer")
		_, other := concretePath(pattern, "gardenio")
		method := strings.SplitN(pattern, " ", 2)[0]
		if code := serve(h, method, own, scopedPrincipal()); code != http.StatusOK {
			t.Errorf("%s own project = %d, want 200", own, code)
		}
		if code := serve(h, method, other, scopedPrincipal()); code != http.StatusForbidden {
			t.Errorf("%s other project = %d, want 403", other, code)
		}
		if code := serve(h, method, other, adminPrincipal()); code != http.StatusOK {
			t.Errorf("%s other project as admin = %d, want 200", other, code)
		}
	}
	for pattern, param := range viewerProjectParams {
		if viewerRouteClasses[pattern] != routeScoped {
			t.Errorf("viewerProjectParams[%q]=%q but the route is not routeScoped", pattern, param)
		}
	}
}

func TestPatternPathValue(t *testing.T) {
	cases := []struct {
		pattern, path, name string
		want                string
		ok                  bool
	}{
		{"GET /p/{project}", "/p/claimlayer", "project", "claimlayer", true},
		{"GET /b/project/{name}/agents", "/b/project/gardenio/agents", "name", "gardenio", true},
		{"GET /p/{project}", "/p/", "project", "", false},
		{"GET /p/{project}", "/p/a/b", "project", "", false},
		{"GET /p/{project}", "/p/x", "other", "", false},
	}
	for _, c := range cases {
		got, ok := patternPathValue(c.pattern, c.path, c.name)
		if got != c.want || ok != c.ok {
			t.Errorf("patternPathValue(%q,%q,%q) = %q,%v want %q,%v", c.pattern, c.path, c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestGardenChartsAndGrafanaStayAdminOnly pins the garden's chart sets
// (GET /garden/charts/{set}, plan gd-gh5J0vVbJP 1.2) and the viewer's Grafana
// proxy to routeAdminOnly, end to end through the scope middleware: both frame
// a Grafana that runs with defaultOrgRole Admin, and costs-project and
// health-land take any project. A tenant's own chart set is a new scoped route, never a loosening
// of these.
func TestGardenChartsAndGrafanaStayAdminOnly(t *testing.T) {
	for _, p := range []string{"GET /garden/charts/{set}", "GET /grafana", "/grafana/{path...}"} {
		if c := viewerRouteClasses[p]; c != routeAdminOnly {
			t.Errorf("%s class = %d, want admin-only", p, c)
		}
	}
	h := scopeChain(registeredViewerRoutes(t))
	for _, path := range []string{
		"/garden/charts/costs-fleet",
		"/garden/charts/costs-project?project=claimlayer",
		"/garden/charts/health-land?project=claimlayer",
		"/garden/charts/agent?agent=claimlayer-run-1",
		"/garden/charts/gardener?gardener=gd-Gard",
		"/grafana/d-solo/gardenio-garden-costs?panelId=1",
	} {
		if code := serve(h, "GET", path, scopedPrincipal()); code != http.StatusForbidden {
			t.Errorf("scoped GET %s = %d, want 403", path, code)
		}
		if code := serve(h, "GET", path, adminPrincipal()); code != http.StatusOK {
			t.Errorf("admin GET %s = %d, want 200", path, code)
		}
	}
}

// TestGardenLandCostsIsScoped: a tenant's own credit charts (GET /garden/charts/costs-land, plan gd-gh5J0vVbJP 3.3)
// are the new scoped route the test above points at. The scope middleware lets a scoped principal through, and the
// /garden/charts/{set} pattern does not swallow it; the land cut itself is the handler's (gardenhub/land_credits.go).
func TestGardenLandCostsIsScoped(t *testing.T) {
	if c := viewerRouteClasses["GET /garden/charts/costs-land"]; c != routeScoped {
		t.Fatalf("costs-land class = %d, want scoped", c)
	}
	h := scopeChain(registeredViewerRoutes(t))
	for _, p := range []principal{scopedPrincipal(), adminPrincipal()} {
		if code := serve(h, "GET", "/garden/charts/costs-land?land=claimlayer", p); code != http.StatusOK {
			t.Errorf("%s GET costs-land = %d, want 200", p.email, code)
		}
	}
}

// TestGardenLandCostsCutsToOwnLand pins the land cut main wires into the credit charts (gardenInLand, gardenhub
// Handler.InLand) end to end through the scope middleware, as the signed-in principal: a tenant of claimlayer reaches its
// own land and no other, also when its token carries gardenio-viewer beside gardenio-project-member (the Keycloak
// default-roles drift of gd-llFKMS8Pkc); an applicant is refused at the gate; an admin reaches every land. Security
// review of the tenant charts, step 3.5 (gd-E7FphpZNMI).
func TestGardenLandCostsCutsToOwnLand(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /garden/charts/costs-land", func(w http.ResponseWriter, r *http.Request) {
		if !gardenInLand(r, r.URL.Query().Get("land")) {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	h := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })
	drifted := scopedPrincipal()
	drifted.roles = append(drifted.roles, oidcauth.RoleViewer)
	for _, c := range []struct {
		name string
		p    principal
		land string
		want int
	}{
		{"tenant, own land", scopedPrincipal(), "claimlayer", http.StatusOK},
		{"tenant, other land", scopedPrincipal(), "acme", http.StatusNotFound},
		{"tenant, land repeated", scopedPrincipal(), "acme&land=claimlayer", http.StatusNotFound},
		{"tenant with viewer, other land", drifted, "acme", http.StatusNotFound},
		{"applicant", applicantPrincipal(), "claimlayer", http.StatusForbidden},
		{"admin, any land", adminPrincipal(), "acme", http.StatusOK},
	} {
		if code := serve(h, "GET", "/garden/charts/costs-land?land="+c.land, c.p); code != c.want {
			t.Errorf("%s: GET costs-land?land=%s = %d, want %d", c.name, c.land, code, c.want)
		}
	}
}

// TestScopedViewer_LiveSubStaysInScope: POST /garden/live/sub is routeScoped
// because a stream it adds is served through scopeMiddleware again, on the
// connection's own request (Handler.Live, main.go). A scoped principal that
// names an admin-only stream on its connection gets that stream refused
// (:err 403) and none of its events; an in-scope stream opens, and the same
// admin-only stream opens for an admin.
func TestScopedViewer_LiveSubStaysInScope(t *testing.T) {
	// Each stream is classed by the viewer's real route table, and one that
	// passes the scope gate answers as an event stream that ends at once.
	routes := registeredViewerRoutes(t)
	stream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})
	g := &gardenhub.Handler{Live: scopeMiddleware(stream, func(r *http.Request) string { _, p := routes.mux.Handler(r); return p })}
	gm := http.NewServeMux()
	g.Register(gm)
	outer := scopeMiddleware(gm, func(r *http.Request) string { _, p := gm.Handler(r); return p })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := scopedPrincipal()
		if r.Header.Get("X-As") == "admin" {
			p = adminPrincipal()
		}
		outer.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	}))
	t.Cleanup(srv.Close)

	// connect opens the shared connection as who and returns its sid and a
	// channel of its data lines.
	connect := func(who string) (string, <-chan string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/garden/live/events", nil)
		req.Header.Set("X-As", who)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s connect: %d", who, resp.StatusCode)
		}
		lines := make(chan string, 64)
		go func() {
			defer close(lines)
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				if v, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
					lines <- v
				}
			}
		}()
		if h := next(t, lines); h != "* :hello" {
			t.Fatalf("%s: first message %q, want the hello", who, h)
		}
		sid, _ := strings.CutSuffix(strings.TrimPrefix(next(t, lines), `{"sid":"`), `"}`)
		return sid, lines
	}
	sub := func(who, sid, key, u string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/garden/live/sub",
			strings.NewReader(`{"sid":"`+sid+`","add":[{"k":"`+key+`","u":"`+u+`"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-As", who)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s POST /garden/live/sub %s: %d, want 204", who, u, resp.StatusCode)
		}
	}
	// first returns the first lifecycle head the connection sends for key.
	first := func(lines <-chan string, key string) string {
		for {
			l := next(t, lines)
			if strings.HasPrefix(l, key+" :") {
				return l
			}
			if strings.HasPrefix(l, key+" ") {
				t.Fatalf("stream %s sent %q before its route answered", key, l)
			}
		}
	}

	sid, lines := connect("scoped")
	sub("scoped", sid, "a", "/garden/events") // admin-only
	if got := first(lines, "a"); got != "a :err 403" {
		t.Errorf("scoped /garden/events on the shared connection: %q, want a :err 403", got)
	}
	sub("scoped", sid, "c", "/garden/c/events") // scoped
	if got := first(lines, "c"); got != "c :open" {
		t.Errorf("scoped /garden/c/events on the shared connection: %q, want c :open", got)
	}

	sid, lines = connect("admin")
	sub("admin", sid, "a", "/garden/events")
	if got := first(lines, "a"); got != "a :open" {
		t.Errorf("admin /garden/events on the shared connection: %q, want a :open", got)
	}
}

// next reads one data line, failing the test after 5 s.
func next(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case l, ok := <-lines:
		if !ok {
			t.Fatal("connection closed")
		}
		return l
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	return ""
}

// TestGardenHomesteadRouteClasses pins the Homestead's opening to project-scoped humans (gd-l7yYxZpoZo): the page, its
// farm document, the farm stream and its view POST are scoped, each cut per viewer in gardenhub; the hub's stream and
// stats and every other /garden read without its own cut stay admin-only, and so does every /garden write outside the
// Commons (/garden/c), the page's one connection (/garden/live), the welcome tour's record of the caller's own tour and a
// land's steward-notes Holds (/garden/notes).
func TestGardenHomesteadRouteClasses(t *testing.T) {
	for _, p := range []string{"GET /garden", "GET /garden/farm", "GET /garden/farm/events", "POST /garden/farm/events/view"} {
		if c := viewerRouteClasses[p]; c != routeScoped {
			t.Errorf("%s class = %d, want scoped", p, c)
		}
	}
	for _, p := range []string{
		"GET /garden/events", "GET /garden/state", "GET /garden/farm/activity", "GET /garden/farm/activity/{id}",
		"GET /garden/bead/{id}", "GET /garden/plan/{id}", "GET /garden/project/{name}", "GET /garden/gardener/{id}",
		"GET /garden/queue/{kind}", "GET /garden/search", "GET /garden/charts/{set}",
	} {
		if c := viewerRouteClasses[p]; c != routeAdminOnly {
			t.Errorf("%s class = %d, want admin-only", p, c)
		}
	}
	for p, c := range viewerRouteClasses {
		// The land header's Holds save (gd-q4NgYNGpy8) is a tenant's write by design (owner Q3): an owner or member edits
		// the Holds of their own land's steward notes, gated in-handler and again by kbeads.
		if !strings.HasPrefix(p, "POST /garden/") || strings.HasPrefix(p, "POST /garden/c/") || p == "POST /garden/live/sub" || p == "POST /garden/farm/events/view" ||
			p == "POST /garden/welcome" || p == "POST /garden/notes/{land}/holds" {
			continue
		}
		if c != routeAdminOnly {
			t.Errorf("%s class = %d, want admin-only: a Homestead write stays with the operators", p, c)
		}
	}
}
