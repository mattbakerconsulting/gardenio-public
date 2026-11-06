package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// The tenant lands on their plot (step 8.8): "/" sends a scoped principal to
// its land's /garden view, or to /lands/mine with several; the gatehouse is not
// a land of one's own.
func TestHandleHome_LandsOnPlot(t *testing.T) {
	for name, c := range map[string]struct {
		sc   viewerScope
		gate string
		loc  string
	}{
		"one land":               {viewerScope{Projects: []string{"acme"}}, "", "/viewer/garden?land=acme"},
		"a name that needs care": {viewerScope{Projects: []string{"a&b"}}, "", "/viewer/garden?land=a%26b"},
		"several lands":          {viewerScope{Projects: []string{"acme", "zeta"}}, "", "/viewer/lands/mine"},
		"a land and the gatehouse": {viewerScope{Projects: []string{"acme", "gatehouse"}}, "gatehouse",
			"/viewer/garden?land=acme"},
		"two lands and the gatehouse": {viewerScope{Projects: []string{"acme", "gatehouse", "zeta"}}, "gatehouse",
			"/viewer/lands/mine"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
			srv.basePath = "/viewer"
			srv.garden = &gardenhub.Handler{}
			srv.landsGatehouse = c.gate
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req = req.WithContext(withViewerScope(req.Context(), c.sc))
			rec := httptest.NewRecorder()
			srv.handleHome(rec, req)
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != c.loc {
				t.Fatalf("%d %q, want 302 to %q", rec.Code, rec.Header().Get("Location"), c.loc)
			}
		})
	}
}

// /lands/mine lists the caller's own lands from its own token, as a page and
// as the JSON the Lands home reads; an operator is sent to /garden.
func TestLandsMine(t *testing.T) {
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	srv.garden = &gardenhub.Handler{}
	srv.landsGatehouse = "gatehouse"
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	h := operatorMiddleware(scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p }))

	tenant := principal{email: "owner@acme.example", roles: []string{oidcauth.RoleProjectMember},
		groups: []string{"/project-acme/owners", "/project-zeta", "/project-gatehouse/guests"}, oidc: true}
	asJSON := func(p principal) []landsMineLand {
		t.Helper()
		r := requestAs(http.MethodGet, "/lands/mine", p)
		r.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("GET /lands/mine (json) = %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
		}
		var d struct {
			Lands []landsMineLand `json:"lands"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil || d.Lands == nil {
			t.Fatalf("body %q: %v (lands must be a list, never null)", rec.Body.String(), err)
		}
		return d.Lands
	}

	got := asJSON(tenant)
	want := []landsMineLand{{"acme", "/garden?land=acme"}, {"zeta", "/garden?land=zeta"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("tenant's lands = %+v, want %+v (the gatehouse left out)", got, want)
	}
	if got := asJSON(applicantPrincipal()); len(got) != 0 {
		t.Errorf("applicant's lands = %+v, want none", got)
	}

	page := landsDo(h, http.MethodGet, "/lands/mine", tenant, "", nil)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, `href="/garden?land=acme">acme</a>`) || !strings.Contains(body, `href="/garden?land=zeta">zeta</a>`) {
		t.Errorf("tenant's /lands/mine = %d, want both lands linked:\n%s", page.Code, body)
	}
	page = landsDo(h, http.MethodGet, "/lands/mine", applicantPrincipal(), "", nil)
	if body := page.Body.String(); page.Code != http.StatusOK || !strings.Contains(body, "You have no land yet.") || !strings.Contains(body, "Go to the gatehouse →") {
		t.Errorf("applicant's /lands/mine = %d, want no land and the gatehouse:\n%s", page.Code, body)
	}
	if rec := landsDo(h, http.MethodGet, "/lands/mine", adminPrincipal(), "", nil); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/garden" {
		t.Errorf("admin's /lands/mine = %d %q, want 302 to /garden", rec.Code, rec.Header().Get("Location"))
	}
}

// The Lands home carries no session, so its script asks /lands/mine and says
// where an existing land is; the page itself names nobody.
func TestLandsHome_OpensYourLand(t *testing.T) {
	rec := landsDo(landsViewer(t, nil), http.MethodGet, "/lands/", principal{}, "", nil)
	body := rec.Body.String()
	for _, want := range []string{`<section id="lands-mine" hidden>`, `href="/lands/mine">Open your land →</a>`, `fetch(base + '/lands/mine'`, `location.replace(lands[0].href)`} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /lands/ lacks %q", want)
		}
	}
}
