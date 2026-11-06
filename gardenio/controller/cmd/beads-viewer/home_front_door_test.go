package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/gardenhub"
)

// "/" is the /garden front door (decision gd-e17Jl61EcP): unrestricted viewers
// are redirected, a scoped principal lands on its land (step 8.8,
// lands_home_test.go has the cases) or keeps the feed with none, and with no
// garden handler wired "/" is still the feed.
func TestHandleHome_FrontDoor(t *testing.T) {
	newSrv := func(garden bool) *Server {
		srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
		if garden {
			srv.garden = &gardenhub.Handler{}
		}
		return srv
	}

	t.Run("admin redirects to garden", func(t *testing.T) {
		srv := newSrv(true)
		srv.basePath = "/viewer"
		rec := httptest.NewRecorder()
		srv.handleHome(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("code = %d, want 302", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/viewer/garden" {
			t.Errorf("Location = %q, want /viewer/garden", loc)
		}
	})

	t.Run("scoped principal lands on its land", func(t *testing.T) {
		srv := newSrv(true)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(withViewerScope(req.Context(), viewerScope{Projects: []string{"claimlayer"}}))
		rec := httptest.NewRecorder()
		srv.handleHome(rec, req)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/garden?land=claimlayer" {
			t.Fatalf("%d %q, want 302 to /garden?land=claimlayer", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("scoped principal with no land keeps the feed", func(t *testing.T) {
		srv := newSrv(true)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(withViewerScope(req.Context(), viewerScope{}))
		rec := httptest.NewRecorder()
		srv.handleHome(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (feed)", rec.Code)
		}
	})

	t.Run("no garden handler serves the feed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newSrv(false).handleHome(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (feed)", rec.Code)
		}
	})
}
