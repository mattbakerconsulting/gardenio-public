package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// newRequestWithEmail returns a GET request whose forwardauth-style email
// header is set, plus optionally a dismiss-cookie attached. Mirrors what
// shouldRenderMigrationBanner sees in production through oidcauth.EmailFromContext:
// the email is promoted into the verified-identity context exactly as the
// auth/dev-bypass middleware does before the handler runs (ka-0000002979).
func newRequestWithEmail(t *testing.T, email string, withDismissCookie bool) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/active", nil)
	if email != "" {
		r.Header.Set("X-Auth-Request-Email", email)
		r = forwardAuthTrustedEmailToContext(r)
	}
	if withDismissCookie {
		r.AddCookie(&http.Cookie{Name: migrationBannerDismissCookie, Value: "1"})
	}
	return r
}

// newWatcherWithEnv constructs a real ConfigWatcher whose only knob is the
// env-var lookup table the test passes in. The watcher is backed by an
// httptest daemon that returns zero tunable beads, so resolveRaw falls
// through bead → env → compiled, exercising the env-override path. This
// matches the pattern in cmd/controller/tunables_endpoint_test.go and
// internal/beadsapi/config_watcher_test.go.
func newWatcherWithEnv(t *testing.T, env map[string]string) *beadsapi.ConfigWatcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/beads" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": []any{},
				"total": 0,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	t.Cleanup(func() { _ = daemon.Close() })

	watcher, err := beadsapi.NewConfigWatcher(daemon, "beads-viewer",
		beadsapi.WithPollInterval(50*time.Millisecond),
		beadsapi.WithLookupEnv(func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		}),
	)
	if err != nil {
		t.Fatalf("NewConfigWatcher: %v", err)
	}
	if err := watcher.Start(context.Background()); err != nil {
		t.Fatalf("watcher.Start: %v", err)
	}
	t.Cleanup(watcher.Stop)
	return watcher
}

// TestShouldRenderMigrationBanner_AllGates exercises every gate of
// shouldRenderMigrationBanner against a happy-path baseline that flips one
// gate at a time. The matt-email env var is set on every subtest because
// gate 2 is fail-closed when GARDENIO_VIEWER_MATT_EMAIL is unset.
func TestShouldRenderMigrationBanner_AllGates(t *testing.T) {
	const mattEmail = "matt@example.com"
	const otherEmail = "alice@example.com"

	cases := []struct {
		name         string
		setupMatt    bool   // sets GARDENIO_VIEWER_MATT_EMAIL=mattEmail
		viewerEmail  string // X-Auth-Request-Email
		dismissed    bool   // attach gb_migration_banner_dismissed=1 cookie
		flagOverride string // if non-empty, set GARDENIO_VIEWER_SHOW_MIGRATION_BANNER
		want         bool
	}{
		{
			name:        "happy path: non-matt OIDC user, no cookie, flag default true",
			setupMatt:   true,
			viewerEmail: otherEmail,
			want:        true,
		},
		{
			name:        "gate 1: anonymous (no OIDC email) → suppressed",
			setupMatt:   true,
			viewerEmail: "",
			want:        false,
		},
		{
			name:        "gate 2: matt-email env unset → fail-closed",
			setupMatt:   false,
			viewerEmail: otherEmail,
			want:        false,
		},
		{
			name:        "gate 2: viewer email matches matt → suppressed",
			setupMatt:   true,
			viewerEmail: mattEmail,
			want:        false,
		},
		{
			name:        "gate 2: case-insensitive matt match → suppressed",
			setupMatt:   true,
			viewerEmail: "MATT@EXAMPLE.COM",
			want:        false,
		},
		{
			name:        "gate 3: dismiss cookie present → suppressed",
			setupMatt:   true,
			viewerEmail: otherEmail,
			dismissed:   true,
			want:        false,
		},
		{
			name:         "gate 4: tunable flag false (env override) → suppressed",
			setupMatt:    true,
			viewerEmail:  otherEmail,
			flagOverride: "false",
			want:         false,
		},
		{
			name:         "gate 4: tunable flag true (env override) → renders",
			setupMatt:    true,
			viewerEmail:  otherEmail,
			flagOverride: "true",
			want:         true,
		},
	}

	// Pin the clock a full day before the V-R4 §3.2 calendar backstop so the
	// sunset gate is open for every subtest. Injecting s.now() here is what
	// decouples these gate assertions from the real wall-clock — before the
	// seam landed they silently flipped to want=false once the process date
	// crossed 2026-08-01 (ka-0000006078).
	preBackstop := migrationBannerBackstop.Add(-24 * time.Hour)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.setupMatt {
				t.Setenv(migrationBannerMattEmailEnv, mattEmail)
			} else {
				t.Setenv(migrationBannerMattEmailEnv, "")
			}

			s := &Server{now: func() time.Time { return preBackstop }}
			if c.flagOverride != "" {
				s.tunables = newWatcherWithEnv(t, map[string]string{
					migrationBannerFlagEnv: c.flagOverride,
				})
			}

			r := newRequestWithEmail(t, c.viewerEmail, c.dismissed)
			got := s.shouldRenderMigrationBanner(r)
			if got != c.want {
				t.Errorf("shouldRenderMigrationBanner = %v, want %v", got, c.want)
			}
		})
	}
}

// TestShouldRenderMigrationBanner_CalendarBackstopConstant locks the V-R4
// §3.2 hard date gate. It (1) sanity-checks the compiled backstop constant
// has not drifted from the planned 2026-08-01 UTC sunset window, and (2)
// drives the gate across that boundary via the injected s.now() clock seam.
//
// The clock injection is what makes this deterministic: earlier this test
// asserted !time.Now().Before(backstop), which was coupled to the real
// wall-clock and hard-failed the moment the process date rolled over to
// 2026-08-01 (ka-0000006078). The gate seam now lets us pin instants on
// both sides of the boundary instead.
func TestShouldRenderMigrationBanner_CalendarBackstopConstant(t *testing.T) {
	want := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if !migrationBannerBackstop.Equal(want) {
		t.Errorf("migrationBannerBackstop = %v, want %v (V-R4 §3.2 sunset)", migrationBannerBackstop, want)
	}

	// All other gates open: OIDC email present, non-matt, no dismiss cookie,
	// flag defaults true (nil tunables → compiled default). Only the calendar
	// gate varies with the injected clock.
	t.Setenv(migrationBannerMattEmailEnv, "matt@example.com")

	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"one day before backstop → renders", migrationBannerBackstop.Add(-24 * time.Hour), true},
		{"one second before backstop → renders", migrationBannerBackstop.Add(-time.Second), true},
		{"exactly at backstop → sunset (Before is exclusive)", migrationBannerBackstop, false},
		{"one second after backstop → sunset", migrationBannerBackstop.Add(time.Second), false},
		{"one day after backstop → sunset", migrationBannerBackstop.Add(24 * time.Hour), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{now: func() time.Time { return c.now }}
			r := newRequestWithEmail(t, "alice@example.com", false)
			if got := s.shouldRenderMigrationBanner(r); got != c.want {
				t.Errorf("shouldRenderMigrationBanner at now=%v = %v, want %v", c.now, got, c.want)
			}
		})
	}
}

// TestHandleMigrationBannerDismiss_CookieAndStatusContract locks the V-R2
// §5.4 cookie/204 contract that the htmx button on the partial relies on:
// 204 No Content + a HttpOnly, SameSite=Lax, Path=/, Max-Age≈1y cookie.
// Anything looser (missing HttpOnly, missing Max-Age, wrong path) silently
// breaks the per-device dismiss persistence.
func TestHandleMigrationBannerDismiss_CookieAndStatusContract(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/banner/migration/dismiss", nil)

	s.handleMigrationBannerDismiss(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}

	cookies := w.Result().Cookies()
	var dismiss *http.Cookie
	for _, c := range cookies {
		if c.Name == migrationBannerDismissCookie {
			dismiss = c
			break
		}
	}
	if dismiss == nil {
		t.Fatalf("missing %q cookie in response; got %d cookies", migrationBannerDismissCookie, len(cookies))
	}
	if dismiss.Value != "1" {
		t.Errorf("cookie value = %q, want \"1\" (presence-only signal)", dismiss.Value)
	}
	if !dismiss.HttpOnly {
		t.Error("cookie missing HttpOnly — banner-dismiss must not be JS-readable")
	}
	if dismiss.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", dismiss.SameSite)
	}
	if dismiss.Path != "/" {
		t.Errorf("cookie Path = %q, want \"/\"", dismiss.Path)
	}
	if dismiss.MaxAge != migrationBannerDismissMaxAgeSeconds {
		t.Errorf("cookie Max-Age = %d, want %d (~1 year)", dismiss.MaxAge, migrationBannerDismissMaxAgeSeconds)
	}
	// Sanity: ~1y in seconds, not infinite/zero.
	if dismiss.MaxAge < 30*24*3600 {
		t.Errorf("cookie Max-Age = %d sec is shorter than 30d — too short for the V-R4 sunset horizon", dismiss.MaxAge)
	}
}

// TestHandleMigrationBannerDismiss_IdempotentReplay verifies the dismiss
// handler's contract holds across repeated calls (htmx may retry on slow
// network). The cookie/status are identical and no body is emitted.
func TestHandleMigrationBannerDismiss_IdempotentReplay(t *testing.T) {
	s := &Server{}
	for i := range 3 {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/banner/migration/dismiss", nil)
		s.handleMigrationBannerDismiss(w, r)
		if w.Code != http.StatusNoContent {
			t.Errorf("attempt %d: status = %d, want 204", i, w.Code)
		}
		if body := w.Body.Bytes(); len(body) != 0 {
			t.Errorf("attempt %d: non-empty body %q", i, body)
		}
		found := false
		for _, c := range w.Result().Cookies() {
			if c.Name == migrationBannerDismissCookie {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("attempt %d: missing dismiss cookie", i)
		}
	}
}
