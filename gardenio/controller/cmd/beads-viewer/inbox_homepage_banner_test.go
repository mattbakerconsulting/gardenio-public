package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// newInboxBannerRequest builds a GET /inbox request with optional OIDC email
// header + optional dismiss cookie, mirroring what shouldRenderInboxHomepageBanner
// sees in production via oidcauth.EmailFromContext. The email is promoted into
// the verified-identity context (forwardAuthTrustedEmailToContext) exactly as
// the auth/dev-bypass middleware does before the handler runs (ka-0000002979).
func newInboxBannerRequest(t *testing.T, email string, withDismissCookie bool) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/inbox", nil)
	if email != "" {
		r.Header.Set("X-Auth-Request-Email", email)
		r = forwardAuthTrustedEmailToContext(r)
	}
	if withDismissCookie {
		r.AddCookie(&http.Cookie{Name: inboxHomepageBannerDismissCookie, Value: "1"})
	}
	return r
}

// TestShouldRenderInboxHomepageBanner_AllGates exercises every gate against a
// happy-path baseline flipped one at a time.
func TestShouldRenderInboxHomepageBanner_AllGates(t *testing.T) {
	const viewerEmail = "alice@example.com"

	cases := []struct {
		name         string
		viewerEmail  string
		dismissed    bool
		flagOverride string
		want         bool
	}{
		{
			name:        "happy path: OIDC user, no cookie, flag default true",
			viewerEmail: viewerEmail,
			want:        true,
		},
		{
			name:        "gate 1: anonymous viewer → suppressed",
			viewerEmail: "",
			want:        false,
		},
		{
			name:        "gate 2: dismiss cookie present → suppressed",
			viewerEmail: viewerEmail,
			dismissed:   true,
			want:        false,
		},
		{
			name:         "gate 3: tunable flag false (env override) → suppressed",
			viewerEmail:  viewerEmail,
			flagOverride: "false",
			want:         false,
		},
		{
			name:         "gate 3: tunable flag true (env override) → renders",
			viewerEmail:  viewerEmail,
			flagOverride: "true",
			want:         true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{}
			if c.flagOverride != "" {
				s.tunables = newWatcherWithEnv(t, map[string]string{
					inboxHomepageBannerFlagEnv: c.flagOverride,
				})
			}

			r := newInboxBannerRequest(t, c.viewerEmail, c.dismissed)
			got := s.shouldRenderInboxHomepageBanner(r)
			if got != c.want {
				t.Errorf("shouldRenderInboxHomepageBanner = %v, want %v", got, c.want)
			}
		})
	}
}

// TestHandleInboxHomepageBannerDismiss_CookieAndStatusContract locks the
// 204 + HttpOnly/SameSite=Lax/Path=/ /~1y cookie contract that the htmx
// button on the partial relies on.
func TestHandleInboxHomepageBannerDismiss_CookieAndStatusContract(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/banner/inbox-homepage/dismiss", nil)

	s.handleInboxHomepageBannerDismiss(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}

	var dismiss *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == inboxHomepageBannerDismissCookie {
			dismiss = c
			break
		}
	}
	if dismiss == nil {
		t.Fatalf("missing %q cookie", inboxHomepageBannerDismissCookie)
	}
	if dismiss.Value != "1" {
		t.Errorf("cookie value = %q, want \"1\"", dismiss.Value)
	}
	if !dismiss.HttpOnly {
		t.Error("cookie missing HttpOnly")
	}
	if dismiss.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", dismiss.SameSite)
	}
	if dismiss.Path != "/" {
		t.Errorf("cookie Path = %q, want \"/\"", dismiss.Path)
	}
	if dismiss.MaxAge != inboxHomepageBannerDismissMaxAgeSeconds {
		t.Errorf("cookie Max-Age = %d, want %d", dismiss.MaxAge, inboxHomepageBannerDismissMaxAgeSeconds)
	}
}
