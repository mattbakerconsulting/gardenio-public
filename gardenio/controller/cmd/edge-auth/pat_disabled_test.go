package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Box auth profile (ka-0000006087 §6): EDGE_AUTH_PAT_DISABLED turns the
// GitLab-PAT auth path off so the air-gapped box authenticates the CLI/agent
// edge solely via the in-box Keycloak (oauth2-proxy cookie). These tests pin
// the three behaviours the chart relies on: the flag parses, the GitLab
// validator is never built (no gitlab.com calls possible), and the readiness
// gate accepts cookie/AVA mode in its place.

func TestParseConfig_PATDisabledFlag(t *testing.T) {
	// Truthy values opt in.
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		t.Setenv("EDGE_AUTH_PAT_DISABLED", v)
		cfg := parseConfig()
		if !cfg.patDisabled {
			t.Errorf("EDGE_AUTH_PAT_DISABLED=%q: patDisabled=false, want true", v)
		}
	}
	// Anything else leaves the PAT edge intact (back-compat default).
	for _, v := range []string{"", "0", "false", "no", "bogus"} {
		t.Setenv("EDGE_AUTH_PAT_DISABLED", v)
		cfg := parseConfig()
		if cfg.patDisabled {
			t.Errorf("EDGE_AUTH_PAT_DISABLED=%q: patDisabled=true, want false", v)
		}
	}
}

// With patDisabled the server must NOT build a GitLab validator even when the
// base URL + group are present, so the shim makes no gitlab.com API calls and a
// stale-membership PAT cannot authenticate. A PAT-bearing /auth/check then
// returns 503 (validator_not_configured) via the existing nil-validator guard.
func TestPATDisabled_NoValidatorBuilt(t *testing.T) {
	cfg := &config{
		patDisabled:     true,
		gitlabBaseURL:   "https://gitlab.com",
		gitlabAuthGroup: "Acme/Platform",
	}
	srv := newServer(cfg, "test", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if srv.validator != nil {
		t.Fatal("patDisabled: GitLab validator must not be built")
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/auth/check", nil)
	req.Header.Set("Authorization", "Bearer some-stale-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("PAT against patDisabled shim: status=%d, want 503", resp.StatusCode)
	}
}

// Readiness: patDisabled is ready only when another identity source is wired.
// Cookie auth (the in-box profile) and AVA both satisfy it; neither => NotReady
// so a misconfigured cookieless box surfaces loudly instead of admitting no one.
func TestPATDisabled_Readiness(t *testing.T) {
	cases := []struct {
		name      string
		cfg       *config
		wantReady bool
	}{
		{
			name:      "patDisabled + cookie auth -> ready",
			cfg:       &config{patDisabled: true, oauth2ProxyAuthURL: "http://gardenio-forwardauth:4180/oauth2/auth", oauth2ProxyCookieName: "_gardenio_oauth2"},
			wantReady: true,
		},
		{
			name:      "patDisabled + AVA -> ready",
			cfg:       &config{patDisabled: true, avaTrustSigner: "arn:aws:ec2:us-east-1:1:verified-access-instance/vai-x"},
			wantReady: true,
		},
		{
			name:      "patDisabled + no other auth mode -> not ready",
			cfg:       &config{patDisabled: true},
			wantReady: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(tc.cfg, "test", slog.New(slog.NewJSONHandler(io.Discard, nil)))
			got, reason := srv.ready()
			if got != tc.wantReady {
				t.Errorf("ready()=%v (%q), want %v", got, reason, tc.wantReady)
			}
		})
	}
}
