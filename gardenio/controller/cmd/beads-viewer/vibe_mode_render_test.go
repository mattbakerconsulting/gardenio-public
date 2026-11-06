package main

import (
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// countUserBeadQueries tallies the Types=[user] Kinds=[config] scans a request
// issued — the shape findUserBeadByEmail uses. The per-request memo
// (userBeadCache) should collapse repeat lookups within one request to a
// single such scan.
func countUserBeadQueries(qs []beadsapi.ListBeadsQuery) int {
	n := 0
	for _, q := range qs {
		if slices.Contains(q.Types, "user") && slices.Contains(q.Kinds, "config") {
			n++
		}
	}
	return n
}

// TestViewerVibeMode_ResolvesFromUserBead exercises the render-seam resolver
// directly across every branch: a vibe user resolves true; a default user, a
// missing user bead, and a daemon error all fail soft to false; and an
// anonymous (logged-out) request short-circuits to false without touching the
// daemon.
func TestViewerVibeMode_ResolvesFromUserBead(t *testing.T) {
	// authedReq builds a GET request whose viewer identity is promoted into the
	// verified context, exactly as the auth/dev-bypass middleware does before
	// viewerVibeMode runs (ka-0000002979 — it reads oidcauth.EmailFromContext).
	authedReq := func() *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-Auth-Request-Email", authorEmail)
		return forwardAuthTrustedEmailToContext(r)
	}

	t.Run("vibe-user-true", func(t *testing.T) {
		srv := NewServer(settingsFake(Preferences{VibeMode: true}), slog.Default(), "", "", "", nil)
		if !srv.viewerVibeMode(authedReq()) {
			t.Errorf("viewerVibeMode = false, want true for a vibe_mode user")
		}
	})

	t.Run("default-user-false", func(t *testing.T) {
		// A user bead exists (email matches) but carries no vibe_mode pref —
		// the Go zero value makes the simple default free.
		srv := NewServer(settingsFake(Preferences{}), slog.Default(), "", "", "", nil)
		if srv.viewerVibeMode(authedReq()) {
			t.Errorf("viewerVibeMode = true, want false for a default (no vibe_mode) user")
		}
	})

	t.Run("logged-out-false-no-lookup", func(t *testing.T) {
		f := settingsFake(Preferences{VibeMode: true})
		srv := NewServer(f, slog.Default(), "", "", "", nil)
		// No X-Auth-Request-Email header → anonymous.
		req := httptest.NewRequest("GET", "/", nil)
		if srv.viewerVibeMode(req) {
			t.Errorf("viewerVibeMode = true, want false for a logged-out viewer")
		}
		// Anonymous must not even hit the daemon — email == "" short-circuits.
		if n := countUserBeadQueries(f.listAllQ); n != 0 {
			t.Errorf("logged-out viewer issued %d user-bead queries, want 0", n)
		}
	})

	t.Run("no-user-bead-false", func(t *testing.T) {
		// Authenticated, but no user bead matches the email (brand-new operator
		// before the provisioner ran).
		f := &fakeDaemon{listResp: &beadsapi.ListBeadsResult{}}
		srv := NewServer(f, slog.Default(), "", "", "", nil)
		if srv.viewerVibeMode(authedReq()) {
			t.Errorf("viewerVibeMode = true, want false when no user bead matches")
		}
	})

	t.Run("daemon-error-false", func(t *testing.T) {
		// A lookup error must fail soft — a preference read never breaks a page.
		f := &fakeDaemon{listErr: errors.New("daemon unavailable")}
		srv := NewServer(f, slog.Default(), "", "", "", nil)
		if srv.viewerVibeMode(authedReq()) {
			t.Errorf("viewerVibeMode = true, want false on daemon error")
		}
	})
}

// TestRender_InjectsVibeMode proves the central render seam actually threads
// VibeMode into template data for every full-page render — the deliverable of
// gb-ux #2 (ka-0000007201). It injects a probe page whose layout echoes
// {{.VibeMode}} and asserts the resolved value surfaces, for both a vibe user
// and a default/logged-out viewer (which get the zero-value false).
func TestRender_InjectsVibeMode(t *testing.T) {
	probe := template.Must(template.New("__probe_vibe__").Parse(
		`{{define "layout"}}VIBE={{.VibeMode}}{{end}}`))

	render := func(t *testing.T, daemon beadAPI, email string) string {
		t.Helper()
		srv := NewServer(daemon, slog.Default(), "", "", "", nil)
		srv.pages["__probe_vibe__"] = probe
		req := httptest.NewRequest("GET", "/", nil)
		if email != "" {
			req.Header.Set("X-Auth-Request-Email", email)
			// render reads the viewer identity from the verified context
			// (ka-0000002979); promote the header exactly as the auth/dev-bypass
			// middleware does before a handler runs.
			req = forwardAuthTrustedEmailToContext(req)
		}
		w := httptest.NewRecorder()
		srv.render(w, req, "__probe_vibe__", nil)
		return w.Body.String()
	}

	t.Run("vibe-user-true", func(t *testing.T) {
		if got := render(t, settingsFake(Preferences{VibeMode: true}), authorEmail); got != "VIBE=true" {
			t.Errorf("render body = %q, want VIBE=true", got)
		}
	})

	t.Run("default-user-false", func(t *testing.T) {
		if got := render(t, settingsFake(Preferences{}), authorEmail); got != "VIBE=false" {
			t.Errorf("render body = %q, want VIBE=false", got)
		}
	})

	t.Run("logged-out-false", func(t *testing.T) {
		// Even with a vibe user seeded, an anonymous request gets the default.
		if got := render(t, settingsFake(Preferences{VibeMode: true}), ""); got != "VIBE=false" {
			t.Errorf("render body = %q, want VIBE=false for logged-out viewer", got)
		}
	})
}

// TestFindUserBeadByEmail_MemoizedPerRequest proves the per-request memo
// collapses the settings handler's own user-bead lookup and the render seam's
// VibeMode lookup into a single daemon scan when the request runs through
// operatorMiddleware (which installs the cache). Without the middleware — as in
// unit tests that hit handlers directly — the lookups are un-memoized, which is
// still correct, just not deduplicated. Both arms are asserted so the memo's
// effect is unambiguous.
func TestFindUserBeadByEmail_MemoizedPerRequest(t *testing.T) {
	newReq := func() *http.Request {
		r := httptest.NewRequest("GET", "/settings", nil)
		r.Header.Set("X-Auth-Request-Email", authorEmail)
		return r
	}

	t.Run("with-middleware-single-scan", func(t *testing.T) {
		f := settingsFake(Preferences{VibeMode: true})
		mux := mkServer(t, f)
		w := httptest.NewRecorder()
		operatorMiddleware(mux).ServeHTTP(w, newReq())

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		if n := countUserBeadQueries(f.listAllQ); n != 1 {
			t.Errorf("with memo, expected exactly 1 user-bead scan, got %d (queries=%v)", n, f.listAllQ)
		}
	})

	t.Run("without-middleware-not-memoized", func(t *testing.T) {
		// Contrast case: no cache in context, so the handler lookup and the
		// render-seam lookup each hit the daemon (2 scans). This is what the
		// middleware-installed memo above eliminates.
		f := settingsFake(Preferences{VibeMode: true})
		mux := mkServer(t, f)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, newReq())

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		if n := countUserBeadQueries(f.listAllQ); n != 2 {
			t.Errorf("without memo, expected 2 user-bead scans (handler + render), got %d", n)
		}
	})
}

// TestRender_VibeModeMemoReusesHandlerLookup is an integration check that the
// probe body and the query count agree: a settings-shaped request that already
// resolved the user bead in its handler still renders the correct VibeMode via
// the shared memo, with no extra daemon round-trip. Guards against a future
// change that satisfies the memo by dropping the render-seam injection entirely.
func TestRender_VibeModeMemoReusesHandlerLookup(t *testing.T) {
	f := settingsFake(Preferences{VibeMode: true})
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/settings", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	operatorMiddleware(mux).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	// The settings read-only block still surfaces the vibe state (its own
	// template already consumes .Prefs.VibeMode) — sanity that we didn't break
	// the existing surface while adding the render-seam injection.
	if !strings.Contains(w.Body.String(), "full functionality") {
		t.Errorf("settings page lost its vibe_mode row\nbody=%s", w.Body.String())
	}
	if n := countUserBeadQueries(f.listAllQ); n != 1 {
		t.Errorf("expected the memo to serve both lookups from 1 scan, got %d", n)
	}
}
