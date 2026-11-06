package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// Negative-matrix acceptance for the viewer half of project-scoped human
// access (gd-6q0cRd9V7Q): the outcome the owner asked for, stated as one table.
// Each refusal row is paired with the unrestricted caller on the same request,
// so a row cannot pass because the route is simply broken.
//
// The daemon half lives in kbeads/internal/server/scoped_acceptance_test.go
// (TestScopedAcceptance). A viewer test cannot import that module's internal
// packages, so the daemon here is scopedDaemon (scope_test.go), a recorded fake
// that applies the same rule kbeads applies to a bearer's project groups: a
// scoped bearer reads only beads labelled with one of its projects, a bearer with
// no project reads nothing. The viewer's own scope is deliberately not consulted
// by the fake, so the rows prove that reads go out as the user.

const (
	acceptScopedBearer  = scopedBearer // grouped /project-claimlayer
	acceptNoGroupBearer = "tok-nogroup"
)

func acceptNoGroupPrincipal() principal {
	return principal{
		email: "nogroup@claimlayer.example",
		roles: []string{oidcauth.RoleProjectMember},
	}
}

// acceptViewer is the full chain a request meets in production: the scope
// middleware in front of the registered routes, over a daemon that cuts reads
// by bearer.
type acceptViewer struct {
	d   *scopedDaemon
	h   http.Handler
	srv *Server
}

func newAcceptViewer(t *testing.T) *acceptViewer {
	t.Helper()
	f := twoProjectFake()
	// An unlabelled bead: nobody's, so a scoped caller must not read it.
	f.beads["nl-1"] = &beadsapi.BeadDetail{ID: "nl-1", Title: "Unlabelled thing", Type: "task", Status: "open"}
	f.semanticErr = context.Canceled // semantic search off: /search takes the scoped list path
	t0 := time.Now().Add(-time.Hour)
	payload := func(id, project string) json.RawMessage {
		data, _ := json.Marshal(map[string]any{"bead": map[string]any{
			"id": id, "type": "task", "title": id + " recent", "status": "open", "labels": []string{"project:" + project},
		}})
		return data
	}
	f.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{Events: []beadsapi.Event{
		{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: payload("gd-1", "gardenio")},
		{ID: 2, Topic: "beads.bead.created", CreatedAt: t0.Add(time.Minute), Payload: payload("cl-1", "claimlayer")},
	}}
	d := newScopedDaemon(f)
	d.scopes[acceptNoGroupBearer] = nil // a scoped bearer with no project group: sees nothing

	hub := NewBeadFeedHub(f, testLogger())
	hub.broadcast(feedEventFor(t, "gd-1", "gardenio"))
	hub.broadcast(feedEventFor(t, "cl-1", "claimlayer"))
	srv := NewServer(d, testLogger(), "", "", "", &fakeScreenClient{})
	srv.SetBeadFeedHub(hub)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	h := scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })
	return &acceptViewer{d: d, h: h, srv: srv}
}

// do runs one request as p carrying bearer and returns the status and body.
func (v *acceptViewer) do(method, path, bearer string, p principal) (int, string) {
	r := requestAs(method, path, p)
	r = r.WithContext(beadsapi.WithUserBearer(r.Context(), bearer))
	rec := httptest.NewRecorder()
	v.h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// strip removes static copy that is not data: the search form's example filter
// and the nav's product wordmark (the Dawn Meadow sign, S1j gd-cxU8eyiGyR).
func strip(body string) string {
	body = strings.ReplaceAll(body, `placeholder="project:gardenio"`, "")
	return navSignRe.ReplaceAllString(body, "")
}

var navSignRe = regexp.MustCompile(`<a class="gd-sign" href="[^"]*">gardenio</a>`)

func TestScopedAcceptance(t *testing.T) {
	// Content that must never reach a scoped caller, and what it must see.
	leaks := []string{"gardenio", "gd-1", "Gardenio secret thing", "Unlabelled thing", "nl-1"}

	type row struct {
		name       string
		method     string
		path       string
		wantScoped int  // status the scoped claimlayer caller gets
		noLeak     bool // scoped body must carry none of leaks
		wantInBody string
	}
	rows := []row{
		{name: "list: front page carries only claimlayer", method: "GET", path: "/", wantScoped: 200, noLeak: true, wantInBody: "cl-1 recent"},
		{name: "list: search carries only claimlayer", method: "GET", path: "/search?q=thing&operated_by=", wantScoped: 200, noLeak: true, wantInBody: "cl-1"},
		{name: "list: active carries only claimlayer", method: "GET", path: "/active", wantScoped: 200, noLeak: true},
		{name: "get: gardenio bead is not found", method: "GET", path: "/gd-1", wantScoped: 404, noLeak: true},
		{name: "get: unlabelled bead is not found", method: "GET", path: "/nl-1", wantScoped: 404, noLeak: true},
		{name: "get: own bead is served", method: "GET", path: "/cl-1", wantScoped: 200, wantInBody: "Claimlayer thing"},
		{name: "own project page is served", method: "GET", path: "/p/claimlayer", wantScoped: 200},
		{name: "another project's page is refused", method: "GET", path: "/p/gardenio", wantScoped: 403},
		{name: "/costs", method: "GET", path: "/costs", wantScoped: 403},
		{name: "/grafana", method: "GET", path: "/grafana", wantScoped: 403},
		{name: "/taints", method: "GET", path: "/taints", wantScoped: 403},
		{name: "/triggers", method: "GET", path: "/triggers", wantScoped: 403},
		{name: "/telepathy", method: "GET", path: "/telepathy", wantScoped: 403},
		{name: "agent input", method: "POST", path: "/agents/gd-agent-1/input", wantScoped: 403},
		{name: "agent respond", method: "POST", path: "/agents/gd-agent-1/respond", wantScoped: 403},
	}

	v := newAcceptViewer(t)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			// Positive control first, so every shared cache is warm with the full
			// picture before the scoped caller asks (a cache keyed wrongly would
			// hand it over).
			adminCode, adminBody := v.do(r.method, r.path, adminBearer, adminPrincipal())
			if adminCode == http.StatusForbidden {
				t.Errorf("control: admin %s %s refused by the scope gate", r.method, r.path)
			}
			if r.wantScoped == 200 && r.noLeak && !strings.Contains(adminBody, "gd-1") && !strings.Contains(strip(adminBody), "ardenio") {
				t.Errorf("control: admin %s %s does not carry the content the scoped caller must not see; the row is vacuous", r.method, r.path)
			}

			code, body := v.do(r.method, r.path, acceptScopedBearer, scopedPrincipal())
			if code != r.wantScoped {
				t.Fatalf("scoped %s %s = %d, want %d", r.method, r.path, code, r.wantScoped)
			}
			if r.noLeak {
				body = strip(body)
				for _, l := range leaks {
					if strings.Contains(body, l) {
						t.Errorf("scoped %s %s leaks %q", r.method, r.path, l)
					}
				}
			}
			if r.wantInBody != "" && !strings.Contains(body, r.wantInBody) {
				t.Errorf("scoped %s %s lost its own project's %q", r.method, r.path, r.wantInBody)
			}
		})
	}

	t.Run("scoped, no group: list and get are empty or denied", func(t *testing.T) {
		for _, path := range []string{"/", "/search?q=thing&operated_by=", "/active"} {
			code, body := v.do("GET", path, acceptNoGroupBearer, acceptNoGroupPrincipal())
			if code != http.StatusOK {
				t.Errorf("no-group GET %s = %d, want an empty 200", path, code)
			}
			for _, l := range append(leaks, "cl-1", "Claimlayer thing") {
				if strings.Contains(strip(body), l) {
					t.Errorf("no-group GET %s carries %q", path, l)
				}
			}
		}
		for _, id := range []string{"cl-1", "gd-1", "nl-1"} {
			if code, _ := v.do("GET", "/"+id, acceptNoGroupBearer, acceptNoGroupPrincipal()); code != http.StatusNotFound {
				t.Errorf("no-group GET /%s = %d, want 404", id, code)
			}
		}
		if code, _ := v.do("GET", "/p/claimlayer", acceptNoGroupBearer, acceptNoGroupPrincipal()); code != http.StatusForbidden {
			t.Errorf("no-group GET /p/claimlayer = %d, want 403", code)
		}
	})

	t.Run("feed SSE carries only claimlayer events", func(t *testing.T) {
		run := func(bearer string, p principal) string {
			ctx, cancel := context.WithTimeout(requestAs("GET", "/feed/events", p).Context(), 300*time.Millisecond)
			defer cancel()
			ctx = beadsapi.WithUserBearer(ctx, bearer)
			r := httptest.NewRequest("GET", "/feed/events", nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			v.srv.handleFeedEvents(rec, r.WithContext(withViewerScope(r.Context(), scopeFromRequest(requestAs("GET", "/", p)))))
			return rec.Body.String()
		}
		scoped, admin := run(acceptScopedBearer, scopedPrincipal()), run(adminBearer, adminPrincipal())
		if !strings.Contains(scoped, "cl-1") || strings.Contains(scoped, "gd-1") {
			t.Errorf("scoped SSE should carry cl-1 only:\n%s", scoped)
		}
		if !strings.Contains(admin, "cl-1") || !strings.Contains(admin, "gd-1") {
			t.Errorf("control: admin SSE should carry both:\n%s", admin)
		}
	})

	t.Run("operator (global) keeps every page it had", func(t *testing.T) {
		op := principal{email: "op@example.com", roles: []string{oidcauth.RoleOperator}}
		for _, path := range []string{"/", "/gd-1", "/nl-1", "/p/gardenio", "/active"} {
			if code, _ := v.do("GET", path, adminBearer, op); code != http.StatusOK {
				t.Errorf("operator GET %s = %d, want 200", path, code)
			}
		}
		for _, path := range []string{"/costs", "/grafana", "/taints", "/triggers", "/telepathy"} {
			if code, _ := v.do("GET", path, adminBearer, op); code == http.StatusForbidden {
				t.Errorf("operator GET %s refused by the scope gate", path)
			}
		}
	})

	// The token's groups lose /project-claimlayer (the 300 s window in the
	// plan): the next request carries a project-member token with no group, and
	// the daemon cuts that bearer to nothing.
	t.Run("group removal: the next request sees the empty result", func(t *testing.T) {
		v := newAcceptViewer(t)
		if code, body := v.do("GET", "/cl-1", acceptScopedBearer, scopedPrincipal()); code != http.StatusOK || !strings.Contains(body, "Claimlayer thing") {
			t.Fatalf("control: before removal GET /cl-1 = %d, want 200 with the bead", code)
		}
		v.d.scopes[acceptScopedBearer] = nil
		revoked := scopedPrincipal()
		revoked.groups = nil
		if code, body := v.do("GET", "/cl-1", acceptScopedBearer, revoked); code != http.StatusNotFound || strings.Contains(body, "Claimlayer thing") {
			t.Errorf("after removal GET /cl-1 = %d, want 404 without the bead", code)
		}
		if code, body := v.do("GET", "/", acceptScopedBearer, revoked); code != http.StatusOK || strings.Contains(body, "cl-1") || strings.Contains(body, "Claimlayer") {
			t.Errorf("after removal GET / = %d carrying the project's content", code)
		}
		if code, _ := v.do("GET", "/p/claimlayer", acceptScopedBearer, revoked); code != http.StatusForbidden {
			t.Errorf("after removal GET /p/claimlayer = %d, want 403", code)
		}
	})
}
