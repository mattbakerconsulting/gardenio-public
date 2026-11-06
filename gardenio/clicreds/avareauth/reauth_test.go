package avareauth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}

func TestIsSignInURL(t *testing.T) {
	tests := []struct {
		name string
		u    *url.URL
		want bool
	}{
		{name: "nil", u: nil, want: false},
		{name: "canonical path", u: mustParseURL(t, "https://gitlab.com/users/sign_in"), want: true},
		{name: "two-factor child", u: mustParseURL(t, "https://gitlab.com/users/sign_in/two_factor"), want: true},
		{name: "with query string", u: mustParseURL(t, "https://gitlab.com/users/sign_in?redirect_to=/foo"), want: true},
		{name: "self-hosted host", u: mustParseURL(t, "https://gitlab.example.com/users/sign_in"), want: true},

		// Negatives: paths that LOOK similar but are not sign-in.
		{name: "sign_up", u: mustParseURL(t, "https://gitlab.com/users/sign_up"), want: false},
		{name: "user profile", u: mustParseURL(t, "https://gitlab.com/users/alice"), want: false},
		{name: "users root", u: mustParseURL(t, "https://gitlab.com/users"), want: false},
		{name: "AVA callback", u: mustParseURL(t, "https://ava.example.com/auth/oidc/callback"), want: false},
		{name: "beads-viewer root", u: mustParseURL(t, "https://beads.gardenio.app.e2e.dev.acme.example/"), want: false},
		// Case-sensitive match: GitLab serves lowercase, and we don't want
		// false positives against unrelated /Users/Sign_In paths.
		{name: "uppercase variant rejected", u: mustParseURL(t, "https://gitlab.com/Users/Sign_In"), want: false},
		// Prefix without separator must not match.
		{name: "sign_in_required", u: mustParseURL(t, "https://gitlab.com/users/sign_in_required"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSignInURL(tt.u); got != tt.want {
				t.Errorf("IsSignInURL(%v) = %v, want %v", tt.u, got, tt.want)
			}
		})
	}
}

func TestLooksLikeSignInPage(t *testing.T) {
	const signInPage = `<!DOCTYPE html>
<html><head><title>Sign in · GitLab</title></head>
<body>
<form class="new_user gl-show-field-errors" id="new_user" aria-live="assertive"
      action="/users/sign_in" accept-charset="UTF-8" method="post">
  <input type="text" name="user[login]" id="user_login" />
  <input type="password" name="user[password]" id="user_password" />
</form>
</body></html>`

	tests := []struct {
		name        string
		contentType string
		body        []byte
		want        bool
	}{
		{name: "canonical sign-in HTML", contentType: "text/html; charset=utf-8", body: []byte(signInPage), want: true},
		{name: "bare text/html", contentType: "text/html", body: []byte(signInPage), want: true},
		{name: "case-folded content type", contentType: "TEXT/HTML; CHARSET=UTF-8", body: []byte(signInPage), want: true},
		{name: "form action only", contentType: "text/html", body: []byte(`<form action="/users/sign_in" method="post"></form>`), want: true},
		{name: "user_login input only", contentType: "text/html", body: []byte(`<input name="user[login]" />`), want: true},
		{name: "single-quoted action", contentType: "text/html", body: []byte(`<form action='/users/sign_in'></form>`), want: true},
		{name: "mixed-case markup", contentType: "text/html", body: []byte(`<FORM ACTION="/USERS/SIGN_IN">`), want: true},

		// Negatives.
		{name: "json response", contentType: "application/json", body: []byte(`{"action":"/users/sign_in"}`), want: false},
		{name: "plain text response", contentType: "text/plain", body: []byte(`/users/sign_in`), want: false},
		{name: "empty body", contentType: "text/html", body: nil, want: false},
		{name: "html without markers", contentType: "text/html", body: []byte(`<html><body><h1>Hello</h1></body></html>`), want: false},
		{name: "html mentions sign_in without form", contentType: "text/html", body: []byte(`<html><a href="/users/sign_in">login</a></html>`), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LooksLikeSignInPage(tt.contentType, tt.body); got != tt.want {
				t.Errorf("LooksLikeSignInPage(%q, %dB) = %v, want %v", tt.contentType, len(tt.body), got, tt.want)
			}
		})
	}
}

// TestLooksLikeSignInPage_ScanLimit confirms the bodyScanLimit boundary: a
// marker placed before the limit is found; a marker placed entirely beyond
// the limit is missed. This is the documented behaviour — we trade
// completeness for a bounded-CPU sniff.
func TestLooksLikeSignInPage_ScanLimit(t *testing.T) {
	marker := `<form action="/users/sign_in">`

	// Within limit: prefix padding + marker, total < bodyScanLimit.
	withinLimit := strings.Repeat("x", bodyScanLimit-len(marker)-100) + marker
	if !LooksLikeSignInPage("text/html", []byte(withinLimit)) {
		t.Errorf("marker within scan limit should be detected")
	}

	// Beyond limit: pad past bodyScanLimit, marker after the cap.
	beyondLimit := strings.Repeat("x", bodyScanLimit+1) + marker
	if LooksLikeSignInPage("text/html", []byte(beyondLimit)) {
		t.Errorf("marker beyond scan limit should NOT be detected")
	}
}

func TestDetectFromResponse(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		contentType string
		body        []byte
		want        error
	}{
		{
			name: "nil response",
			url:  "",
			want: nil,
		},
		{
			name: "URL match short-circuits",
			url:  "https://gitlab.com/users/sign_in",
			want: ErrReauthRequired,
		},
		{
			name:        "body match without URL match",
			url:         "https://beads.example.com/some/page",
			contentType: "text/html",
			body:        []byte(`<form action="/users/sign_in"></form>`),
			want:        ErrReauthRequired,
		},
		{
			name:        "neither matches",
			url:         "https://beads.example.com/v1/beads",
			contentType: "application/json",
			body:        []byte(`{"beads":[]}`),
			want:        nil,
		},
		{
			name:        "html without sign-in markers",
			url:         "https://beads.example.com/about",
			contentType: "text/html",
			body:        []byte(`<html><body>about</body></html>`),
			want:        nil,
		},
		{
			name:        "cloudflare challenge body",
			url:         "https://gitlab.com/users/sign_in",
			contentType: "text/html; charset=UTF-8",
			body:        []byte(`<!DOCTYPE html><html><head><title>Just a moment...</title><script src="/cdn-cgi/challenge-platform/h/g/orchestrate/chl_page/v1"></script></head></html>`),
			want:        ErrReauthRequired,
		},
		{
			name:        "cloudflare cf_chl marker on non-signin url",
			url:         "https://gitlab.com/",
			contentType: "text/html",
			body:        []byte(`<html><script>window._cf_chl_opt={cvId:'3'};</script></html>`),
			want:        ErrReauthRequired,
		},
		{
			name:        "json from cloudflare-fronted host is not a challenge",
			url:         "https://gardenio.app.e2e.dev.acme.example/kd/v1/beads",
			contentType: "application/json",
			body:        []byte(`{"beads":[],"note":"served via cloudflare"}`),
			want:        nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "nil response" {
				if err := DetectFromResponse(nil, nil); err != nil {
					t.Errorf("nil response should not detect: got %v", err)
				}
				return
			}
			req, err := http.NewRequest(http.MethodGet, tt.url, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp := &http.Response{
				Request: req,
				Header:  http.Header{},
			}
			if tt.contentType != "" {
				resp.Header.Set("Content-Type", tt.contentType)
			}
			got := DetectFromResponse(resp, tt.body)
			if !errors.Is(got, tt.want) && got != tt.want {
				t.Errorf("DetectFromResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDetectFromResponse_CloudflareMitigatedHeader verifies the cf-mitigated
// response header alone (no body markers) is enough to surface re-auth — it is
// Cloudflare's definitive challenge signal and survives an empty/minified body.
func TestDetectFromResponse_CloudflareMitigatedHeader(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://gitlab.com/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp := &http.Response{Request: req, Header: http.Header{}}
	resp.Header.Set("cf-mitigated", "challenge")
	resp.Header.Set("Content-Type", "text/html")
	if got := DetectFromResponse(resp, nil); !errors.Is(got, ErrReauthRequired) {
		t.Errorf("cf-mitigated:challenge should detect re-auth, got %v", got)
	}
}

// TestErrReauthRequired_Message guards the user-facing wording — this is
// what a CLI prints when it surfaces the error. If the message changes,
// agents and operators reading "please run `gb login`" docs need to know.
func TestErrReauthRequired_Message(t *testing.T) {
	msg := ErrReauthRequired.Error()
	if !strings.Contains(msg, "gb login") {
		t.Errorf("ErrReauthRequired message must mention `gb login`, got: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "expired") {
		t.Errorf("ErrReauthRequired message should explain the failure mode, got: %q", msg)
	}
}
