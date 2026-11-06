package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBuildLoginRedirect(t *testing.T) {
	cases := []struct {
		name           string
		forwardAuthURL string
		viewerBase     string
		next           string
		idpHint        string
		wantStartsWith string
		wantRD         string
		wantHint       string
	}{
		{
			name:           "no hint",
			forwardAuthURL: "https://gardenio.example.com",
			viewerBase:     "https://beads.example.com",
			idpHint:        "",
			wantStartsWith: "https://gardenio.example.com/oauth2/start?",
			wantRD:         "https://beads.example.com/",
			wantHint:       "",
		},
		{
			name:           "gitlab hint",
			forwardAuthURL: "https://gardenio.example.com",
			viewerBase:     "https://beads.example.com",
			idpHint:        "gitlab",
			wantStartsWith: "https://gardenio.example.com/oauth2/start?",
			wantRD:         "https://beads.example.com/",
			wantHint:       "gitlab",
		},
		{
			name:           "trailing slash on forwardAuthURL is normalized",
			forwardAuthURL: "https://gardenio.example.com/",
			viewerBase:     "https://beads.example.com",
			idpHint:        "",
			wantStartsWith: "https://gardenio.example.com/oauth2/start?",
			wantRD:         "https://beads.example.com/",
		},
		{
			name:           "viewer base missing slash gets one appended",
			forwardAuthURL: "https://gardenio.example.com",
			viewerBase:     "https://beads.example.com/foo",
			idpHint:        "",
			wantStartsWith: "https://gardenio.example.com/oauth2/start?",
			wantRD:         "https://beads.example.com/foo/",
		},
		{
			name:           "next returns to the deep link, query kept (gd-kNV6Eos4ON)",
			forwardAuthURL: "https://gardenio.example.com",
			viewerBase:     "https://beads.example.com/foo",
			next:           "/foo/garden?at=gardenio/gd-UZKZ9H25UD&z=2",
			idpHint:        "gitlab",
			wantStartsWith: "https://gardenio.example.com/oauth2/start?",
			wantRD:         "https://beads.example.com/foo/garden?at=gardenio/gd-UZKZ9H25UD&z=2",
			wantHint:       "gitlab",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildLoginRedirect(tc.forwardAuthURL, tc.viewerBase, tc.next, tc.idpHint)
			if !strings.HasPrefix(got, tc.wantStartsWith) {
				t.Fatalf("got %q, want prefix %q", got, tc.wantStartsWith)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("parse %q: %v", got, err)
			}
			if rd := u.Query().Get("rd"); rd != tc.wantRD {
				t.Errorf("rd: got %q, want %q", rd, tc.wantRD)
			}
			if hint := u.Query().Get("kc_idp_hint"); hint != tc.wantHint {
				t.Errorf("kc_idp_hint: got %q, want %q", hint, tc.wantHint)
			}
		})
	}
}

func TestViewerBaseFromRequest(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		proto    string
		fwdHost  string
		basePath string
		want     string
	}{
		{
			name:     "tls inferred",
			host:     "beads.example.com",
			proto:    "https",
			basePath: "",
			want:     "https://beads.example.com",
		},
		{
			name:     "X-Forwarded-Host wins",
			host:     "internal.svc",
			fwdHost:  "beads.example.com",
			proto:    "https",
			basePath: "",
			want:     "https://beads.example.com",
		},
		{
			name:     "with base path",
			host:     "beads.example.com",
			proto:    "https",
			basePath: "/viewer",
			want:     "https://beads.example.com/viewer",
		},
		{
			name:     "trailing slash trimmed from basePath",
			host:     "beads.example.com",
			proto:    "https",
			basePath: "/viewer/",
			want:     "https://beads.example.com/viewer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/login", nil)
			r.Host = tc.host
			if tc.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if tc.fwdHost != "" {
				r.Header.Set("X-Forwarded-Host", tc.fwdHost)
			}
			got := viewerBaseFromRequest(r, tc.basePath)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func newLoginTestServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewServer(nil, logger, "", "", "", nil)
}

func TestHandleLogin_Disabled_Redirects(t *testing.T) {
	s := newLoginTestServer(t)
	cfg := loginPickerConfig{
		enabled:        false,
		forwardAuthURL: "https://gardenio.example.com",
	}
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.Host = "beads.example.com"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()

	s.handleLogin(cfg)(w, r)

	if w.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://gardenio.example.com/oauth2/start?") {
		t.Errorf("Location prefix mismatch: %q", loc)
	}
	if strings.Contains(loc, "kc_idp_hint") {
		t.Errorf("disabled picker must not send kc_idp_hint, got %q", loc)
	}
}

func TestHandleLogin_AllProvidersDisabled_Redirects(t *testing.T) {
	s := newLoginTestServer(t)
	cfg := loginPickerConfig{
		enabled:        true,
		forwardAuthURL: "https://gardenio.example.com",
		// both keycloakLocalEnabled and gitlabEnabled false
	}
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.Host = "beads.example.com"
	w := httptest.NewRecorder()

	s.handleLogin(cfg)(w, r)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 when no providers enabled, got %d", w.Code)
	}
}

func TestHandleLogin_BothProviders_RendersPicker(t *testing.T) {
	s := newLoginTestServer(t)
	cfg := loginPickerConfig{
		enabled:              true,
		forwardAuthURL:       "https://gardenio.example.com",
		keycloakLocalEnabled: true,
		keycloakLocalLabel:   "Keycloak (local)",
		gitlabEnabled:        true,
		gitlabLabel:          "GitLab",
	}
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.Host = "beads.example.com"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()

	s.handleLogin(cfg)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Keycloak (local)") {
		t.Errorf("missing keycloak-local button: %s", body)
	}
	if !strings.Contains(body, "GitLab") {
		t.Errorf("missing gitlab button: %s", body)
	}
	if !strings.Contains(body, "kc_idp_hint=gitlab") {
		t.Errorf("gitlab button must carry kc_idp_hint=gitlab: %s", body)
	}
	// Local Keycloak button must NOT carry kc_idp_hint.
	// Count occurrences — there should be exactly one (the gitlab button).
	if strings.Count(body, "kc_idp_hint") != 1 {
		t.Errorf("expected exactly one kc_idp_hint param (gitlab only), got body: %s", body)
	}
}

func TestHandleLogin_OnlyKeycloakLocal(t *testing.T) {
	s := newLoginTestServer(t)
	cfg := loginPickerConfig{
		enabled:              true,
		forwardAuthURL:       "https://gardenio.example.com",
		keycloakLocalEnabled: true,
		keycloakLocalLabel:   "Local",
		gitlabEnabled:        false,
		gitlabLabel:          "GitLab",
	}
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.Host = "beads.example.com"
	w := httptest.NewRecorder()

	s.handleLogin(cfg)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Local") {
		t.Errorf("missing local button: %s", body)
	}
	if strings.Contains(body, ">GitLab<") {
		t.Errorf("gitlab button must be hidden when disabled: %s", body)
	}
	if strings.Contains(body, "kc_idp_hint") {
		t.Errorf("keycloak-local-only picker must not emit kc_idp_hint: %s", body)
	}
}

func TestHandleLogin_OnlyGitLab(t *testing.T) {
	s := newLoginTestServer(t)
	cfg := loginPickerConfig{
		enabled:              true,
		forwardAuthURL:       "https://gardenio.example.com",
		keycloakLocalEnabled: false,
		gitlabEnabled:        true,
		gitlabLabel:          "GitLab",
	}
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	r.Host = "beads.example.com"
	w := httptest.NewRecorder()

	s.handleLogin(cfg)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "GitLab") {
		t.Errorf("missing gitlab button: %s", body)
	}
	if !strings.Contains(body, "kc_idp_hint=gitlab") {
		t.Errorf("gitlab-only button must carry kc_idp_hint=gitlab: %s", body)
	}
}

// /login?next= is honoured only for a rooted path under the base path: a deep link into /garden comes back after sign-in
// (gd-kNV6Eos4ON), and nothing that a browser reads as another origin is followed.
func TestLoginNext(t *testing.T) {
	for _, tc := range []struct{ base, next, want string }{
		{"", "/garden?at=gardenio/gd-X", "/garden?at=gardenio/gd-X"},
		{"", "/garden?at=gardenio%2Fgd-X&z=2", "/garden?at=gardenio%2Fgd-X&z=2"},
		{"/beads", "/beads/garden?at=gardenio/gd-X", "/beads/garden?at=gardenio/gd-X"},
		{"/beads", "/beads", "/beads"},
		{"/beads", "/other/garden", ""},
		{"", "", ""},
		{"", "garden", ""},
		{"", "//evil.example/x", ""},
		{"", "/\\evil.example/x", ""},
		{"", "https://evil.example/x", ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "/login?next="+url.QueryEscape(tc.next), nil)
		if got := loginNext(r, tc.base); got != tc.want {
			t.Errorf("loginNext(base %q, next %q) = %q, want %q", tc.base, tc.next, got, tc.want)
		}
	}
	// the picker's pass-through carries it into oauth2-proxy's rd
	s := newLoginTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/login?next="+url.QueryEscape("/garden?at=gardenio/gd-X"), nil)
	r.Host = "beads.example.com"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	s.handleLogin(loginPickerConfig{forwardAuthURL: "https://gardenio.example.com"})(w, r)
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil || u.Query().Get("rd") != "https://beads.example.com/garden?at=gardenio/gd-X" {
		t.Errorf("pass-through rd = %q (Location %q), want the deep link", u.Query().Get("rd"), w.Header().Get("Location"))
	}
}
