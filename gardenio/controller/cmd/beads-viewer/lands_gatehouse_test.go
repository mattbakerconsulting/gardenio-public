package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/gardenhub"
)

// The gatehouse (step 9.12): "/" opens its #general for a person whose only
// land it is; everyone else keeps the front door they had.
func TestHandleHome_Gatehouse(t *testing.T) {
	newSrv := func() *Server {
		srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
		srv.basePath = "/viewer"
		srv.garden = &gardenhub.Handler{}
		srv.commons = &commons.Handler{}
		srv.landsGatehouse = "gatehouse"
		return srv
	}
	home := func(srv *Server, sc *viewerScope) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if sc != nil {
			req = req.WithContext(withViewerScope(req.Context(), *sc))
		}
		rec := httptest.NewRecorder()
		srv.handleHome(rec, req)
		return rec
	}

	rec := home(newSrv(), &viewerScope{Projects: []string{"gatehouse"}})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/viewer/garden/c/s/gatehouse" {
		t.Fatalf("gatehouse guest: %d %q, want 302 to the gatehouse's steward", rec.Code, rec.Header().Get("Location"))
	}

	for name, c := range map[string]struct {
		srv  func() *Server
		sc   *viewerScope
		code int
		loc  string
	}{
		"a land of their own too": {newSrv, &viewerScope{Projects: []string{"acme", "gatehouse"}}, http.StatusFound, "/viewer/garden?land=acme"},
		"another land only":       {newSrv, &viewerScope{Projects: []string{"acme"}}, http.StatusFound, "/viewer/garden?land=acme"},
		"admin":                   {newSrv, nil, http.StatusFound, "/viewer/garden"},
		"no gatehouse": {func() *Server { s := newSrv(); s.landsGatehouse = ""; return s },
			&viewerScope{Projects: []string{"gatehouse"}}, http.StatusFound, "/viewer/garden?land=gatehouse"},
		"no Commons": {func() *Server { s := newSrv(); s.commons = nil; return s },
			&viewerScope{Projects: []string{"gatehouse"}}, http.StatusOK, ""},
	} {
		t.Run(name, func(t *testing.T) {
			rec := home(c.srv(), c.sc)
			if rec.Code != c.code || rec.Header().Get("Location") != c.loc {
				t.Fatalf("%d %q, want %d %q", rec.Code, rec.Header().Get("Location"), c.code, c.loc)
			}
		})
	}
}

func TestLandsGatehouseFromEnv(t *testing.T) {
	for raw, want := range map[string]string{"": "", " gatehouse ": "gatehouse", "front-door": "front-door"} {
		got, err := landsGatehouseFromEnv(func(string) string { return raw })
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{"Gate House", "claimlayer", "../x"} {
		if got, err := landsGatehouseFromEnv(func(string) string { return bad }); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}

// The Lands home says where someone let in will land, and that every
// newcomer reads the gatehouse's #general, only when there is a gatehouse.
func TestLandsHome_Gatehouse(t *testing.T) {
	for _, gatehouse := range []string{"", "gatehouse"} {
		srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
		srv.landsGatehouse = gatehouse
		mux := http.NewServeMux()
		srv.RegisterRoutes(mux)
		h := operatorMiddleware(scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p }))
		rec := landsDo(h, http.MethodGet, "/lands/", principal{}, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /lands/ = %d", rec.Code)
		}
		body := rec.Body.String()
		has := strings.Contains(body, "Already let in?") &&
			strings.Contains(body, "Every newcomer can read the gatehouse") &&
			strings.Contains(body, `href="/">Go to the gatehouse →</a>`)
		if has != (gatehouse != "") {
			t.Errorf("gatehouse %q: the gatehouse block shown = %v", gatehouse, has)
		}
	}
}
