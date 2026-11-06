package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newCompatTestServer builds a Server + real *http.ServeMux so every assertion
// below resolves through the SAME routing table production uses. basePath is
// exercised deliberately ("/beads" in prod) so the Location prefix is real.
func newCompatTestServer(t *testing.T, basePath string) *http.ServeMux {
	t.Helper()
	srv := NewServer(&fakeDaemon{}, slog.Default(), basePath, "http://test", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return mux
}

// TestLegacyUIRedirect_ValidPages asserts each allowlisted /ui/<file>.html
// permanently redirects to its canonical route, resolved through the real mux.
func TestLegacyUIRedirect_ValidPages(t *testing.T) {
	mux := newCompatTestServer(t, "/beads")
	for base, route := range legacyUIPages {
		path := "/ui/" + base + ".html"
		want := "/beads" + route
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusMovedPermanently {
			t.Errorf("%s: status = %d, want 301", path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("%s: Location = %q, want %q", path, got, want)
		}
	}
}

// TestLegacyUIRedirect_Escalations pins the confirmed backlog shape end to end
// (bar the client-side fragment hop the browser performs, which no server test
// can observe): /ui/escalations.html → 301 /beads/escalations.
func TestLegacyUIRedirect_Escalations(t *testing.T) {
	mux := newCompatTestServer(t, "/beads")
	req := httptest.NewRequest(http.MethodGet, "/ui/escalations.html", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	if got, want := rec.Header().Get("Location"), "/beads/escalations"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

// TestLegacyUIRedirect_PreservesQuery confirms an existing query string rides
// along (the fragment cannot — it never reaches the server — but a query can).
func TestLegacyUIRedirect_PreservesQuery(t *testing.T) {
	mux := newCompatTestServer(t, "/beads")
	req := httptest.NewRequest(http.MethodGet, "/ui/escalations.html?project=gardenio", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	if got, want := rec.Header().Get("Location"), "/beads/escalations?project=gardenio"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

// TestLegacyUIRedirect_EmptyBasePath confirms the no-prefix deployment produces
// a clean absolute route (no leading double slash, no dangling prefix).
func TestLegacyUIRedirect_EmptyBasePath(t *testing.T) {
	mux := newCompatTestServer(t, "")
	req := httptest.NewRequest(http.MethodGet, "/ui/escalations.html", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if got, want := rec.Header().Get("Location"), "/escalations"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

// TestLegacyUIRedirect_NegativeCases is the security bar. Two invariants,
// resolved through the real mux:
//
//  1. A non-allowlisted /ui/<file>.html never redirects to a content PAGE —
//     it 404s. (My handler is what refuses it; the allowlist is the gate.)
//  2. Whatever Location IS emitted is always same-origin — never an off-site or
//     protocol-relative URL. This is the open-redirect guard.
//
// Note on the traversal rows: net/http.ServeMux path-cleans an unclean request
// path and issues its OWN 301 to the cleaned, same-origin path BEFORE any
// handler runs (so /ui/../etc.html → 301 /etc.html, which then lands on the
// /{id} catch-all and 404s — my handler never sees it). That normalization
// redirect is benign; the test only requires it stay same-origin.
func TestLegacyUIRedirect_NegativeCases(t *testing.T) {
	mux := newCompatTestServer(t, "/beads")
	cases := []struct {
		name        string
		path        string
		mustNotPage bool // if true, a redirect to any allowlisted content page is a failure
	}{
		{"unknown page", "/ui/nonexistent.html", true},
		{"non-html suffix", "/ui/escalations", true},
		{"non-html suffix json", "/ui/escalations.json", true},
		{"parameterized template not a top-level route", "/ui/bead_show.html", true},
		{"detail template", "/ui/decision_detail.html", true},
		{"empty page trailing slash", "/ui/", true},
		{"traversal to root path", "/ui/../etc.html", false},
		{"traversal payload", "/ui/%2e%2e%2fetc.html", true},
		{"double dot base", "/ui/...html", true},
		{"empty base .html", "/ui/.html", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code < 300 || rec.Code >= 400 {
				return // not a redirect — nothing to smuggle
			}
			loc := rec.Header().Get("Location")
			if !isSameOrigin(loc) {
				t.Fatalf("%s (%s): OPEN REDIRECT — Location %q is not same-origin (code %d)",
					tc.name, tc.path, loc, rec.Code)
			}
			if tc.mustNotPage && isAllowlistedTarget(loc) {
				t.Fatalf("%s (%s): non-allowlisted input redirected to a content page %q (code %d)",
					tc.name, tc.path, loc, rec.Code)
			}
		})
	}
}

// isAllowlistedTarget reports whether loc is one of the canonical routes this
// compat surface is permitted to send a browser to (under the test basePath).
func isAllowlistedTarget(loc string) bool {
	for _, route := range legacyUIPages {
		if loc == "/beads"+route {
			return true
		}
	}
	return false
}

// isSameOrigin reports whether a redirect Location keeps the browser on this
// origin: an empty Location (no redirect), or an absolute-path reference that
// is neither protocol-relative ("//host") nor an absolute URL ("scheme:..").
// This is the property that distinguishes a benign in-app redirect from an
// open redirect to an attacker-controlled host.
func isSameOrigin(loc string) bool {
	if loc == "" {
		return true
	}
	if !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") {
		return false
	}
	if i := strings.IndexByte(loc, ':'); i >= 0 {
		// A ':' before any '/' would make this "scheme:..." (absolute URL).
		if s := strings.IndexByte(loc, '/'); s == -1 || i < s {
			return false
		}
	}
	return true
}

// TestLegacyUIRedirect_NoOpenRedirect specifically probes the open-redirect
// failure mode: a page value that tries to smuggle an absolute or
// protocol-relative URL must NOT become an off-origin Location. ServeMux
// path-cleaning + the single-segment {page} match + the closed allowlist
// should all refuse it.
func TestLegacyUIRedirect_NoOpenRedirect(t *testing.T) {
	mux := newCompatTestServer(t, "/beads")
	for _, path := range []string{
		"/ui/%2f%2fevil.com.html",
		"/ui/https:%2f%2fevil.com.html",
		"/ui/%5c%5cevil.com.html",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code >= 300 && rec.Code < 400 {
			if loc := rec.Header().Get("Location"); !isSameOrigin(loc) {
				t.Fatalf("%s: open redirect to %q (code %d)", path, loc, rec.Code)
			}
		}
	}
}

// TestLegacyGardenerRedirect pins the pre-rename captain UI routes (rename plan
// gd-5O9QUlA8zr phase 4): each already-sent link 301s to its gardener route,
// keeping the base path, the path remainder and the query string.
func TestLegacyGardenerRedirect(t *testing.T) {
	mux := newCompatTestServer(t, "/beads")
	for path, want := range map[string]string{
		"/captains":                             "/beads/gardeners",
		"/captains/":                            "/beads/gardener",
		"/captains/gd-abc":                      "/beads/gardener/gd-abc",
		"/captain/gd-abc":                       "/beads/gardener/gd-abc",
		"/captain/gd-abc/runs":                  "/beads/gardener/gd-abc/runs",
		"/captain/new":                          "/beads/gardener/new",
		"/captains?status=open":                 "/beads/gardeners?status=open",
		"/captain/gd-abc/runs?page=2":           "/beads/gardener/gd-abc/runs?page=2",
		"/b/project/gardenio/captains":          "/beads/b/project/gardenio/gardeners",
		"/b/project/gardenio/captains?tab=live": "/beads/b/project/gardenio/gardeners?tab=live",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMovedPermanently {
			t.Errorf("%s: status = %d, want 301", path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("%s: Location = %q, want %q", path, got, want)
		}
	}
}

// TestLegacyGardenerRedirect_NoOpenRedirect: a smuggled absolute or
// protocol-relative remainder must never become an off-origin Location.
func TestLegacyGardenerRedirect_NoOpenRedirect(t *testing.T) {
	mux := newCompatTestServer(t, "/beads")
	for _, path := range []string{
		"/captains//evil.com",
		"/captain/%2f%2fevil.com",
		"/captain/..%2f..%2fevil.com",
		"/captains/https:%2f%2fevil.com",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code >= 300 && rec.Code < 400 {
			if loc := rec.Header().Get("Location"); !isSameOrigin(loc) {
				t.Fatalf("%s: open redirect to %q (code %d)", path, loc, rec.Code)
			}
		}
	}
}
