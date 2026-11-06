package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseConfig_Defaults(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("GITLAB_BASE_URL", "")
	t.Setenv("EDGE_AUTH_GITLAB_GROUP", "")
	t.Setenv("EDGE_AUTH_HMAC_KEY", "")
	t.Setenv("EDGE_AUTH_UPSTREAM_KBEADS", "")
	t.Setenv("EDGE_AUTH_UPSTREAM_COOPMUX", "")
	t.Setenv("EDGE_AUTH_ENTRA_ISSUER_URL", "")
	t.Setenv("EDGE_AUTH_ENTRA_AUDIENCE", "")
	t.Setenv("EDGE_AUTH_ENTRA_TENANT_ID", "")
	t.Setenv("EDGE_AUTH_ENTRA_REQUIRED_SCOPE", "")
	t.Setenv("EDGE_AUTH_ENTRA_REQUIRED_ROLE", "")
	t.Setenv("EDGE_AUTH_ENTRA_ALLOWED_CLIENT_IDS", "")
	t.Setenv("EDGE_AUTH_OAUTH2_PROXY_ROLE_SOURCE", "")

	cfg := parseConfig()

	if cfg.listenAddr != ":8096" {
		t.Errorf("listenAddr=%q, want :8096", cfg.listenAddr)
	}
	if cfg.logLevel != "info" {
		t.Errorf("logLevel=%q, want info", cfg.logLevel)
	}
	if cfg.gitlabBaseURL != "https://gitlab.com" {
		t.Errorf("gitlabBaseURL=%q, want https://gitlab.com", cfg.gitlabBaseURL)
	}
	if cfg.gitlabAuthGroup != "gardenio" {
		t.Errorf("gitlabAuthGroup=%q, want gardenio", cfg.gitlabAuthGroup)
	}
	if cfg.hmacKey != "" {
		t.Errorf("hmacKey=%q, want empty (scaffold)", cfg.hmacKey)
	}
	if cfg.dryRun {
		t.Error("dryRun must default false (ka-0000002544 safe-deploy)")
	}
	if cfg.oauth2ProxyRoleSource != oauth2ProxyRoleSourceProviderGroups {
		t.Errorf("oauth2ProxyRoleSource=%q, want %q", cfg.oauth2ProxyRoleSource, oauth2ProxyRoleSourceProviderGroups)
	}
}

func TestParseConfig_DryRunFlag(t *testing.T) {
	// Truthy values opt in.
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		t.Setenv("EDGE_AUTH_DRY_RUN", v)
		cfg := parseConfig()
		if !cfg.dryRun {
			t.Errorf("EDGE_AUTH_DRY_RUN=%q: dryRun=false, want true", v)
		}
	}
	// Anything else is off (safe-deploy default).
	for _, v := range []string{"", "0", "false", "no", "bogus"} {
		t.Setenv("EDGE_AUTH_DRY_RUN", v)
		cfg := parseConfig()
		if cfg.dryRun {
			t.Errorf("EDGE_AUTH_DRY_RUN=%q: dryRun=true, want false", v)
		}
	}
}

func TestParseConfig_Overrides(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":9000")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.com/")
	t.Setenv("EDGE_AUTH_GITLAB_GROUP", "my-group")
	t.Setenv("EDGE_AUTH_HMAC_KEY", "deadbeef")
	t.Setenv("EDGE_AUTH_UPSTREAM_KBEADS", "http://kbeads:8080/")
	t.Setenv("EDGE_AUTH_UPSTREAM_COOPMUX", "http://coopmux:8090")
	t.Setenv("EDGE_AUTH_DRY_RUN", "true")
	t.Setenv("EDGE_AUTH_ENTRA_ISSUER_URL", "https://login.microsoftonline.com/tenant/v2.0")
	t.Setenv("EDGE_AUTH_ENTRA_AUDIENCE", "gardenio-client")
	t.Setenv("EDGE_AUTH_ENTRA_TENANT_ID", "tenant")
	t.Setenv("EDGE_AUTH_ENTRA_REQUIRED_SCOPE", "user_impersonation")
	t.Setenv("EDGE_AUTH_ENTRA_REQUIRED_ROLE", "gardenio-viewer")
	t.Setenv("EDGE_AUTH_ENTRA_ALLOWED_CLIENT_IDS", "piid-client, OTHER-CLIENT ")
	t.Setenv("EDGE_AUTH_OAUTH2_PROXY_ROLE_SOURCE", " ENTRA-APP-ROLES ")

	cfg := parseConfig()

	cases := map[string]struct{ got, want string }{
		"listenAddr":       {cfg.listenAddr, ":9000"},
		"logLevel":         {cfg.logLevel, "debug"},
		"gitlabBaseURL":    {cfg.gitlabBaseURL, "https://gitlab.example.com"},
		"gitlabAuthGroup":  {cfg.gitlabAuthGroup, "my-group"},
		"hmacKey":          {cfg.hmacKey, "deadbeef"},
		"upstreamKbeads":   {cfg.upstreamKbeads, "http://kbeads:8080"},
		"upstreamCoopmux":  {cfg.upstreamCoopmux, "http://coopmux:8090"},
		"entraIssuerURL":   {cfg.entraIssuerURL, "https://login.microsoftonline.com/tenant/v2.0"},
		"entraAudience":    {cfg.entraAudience, "gardenio-client"},
		"entraTenantID":    {cfg.entraTenantID, "tenant"},
		"entraScope":       {cfg.entraRequiredScope, "user_impersonation"},
		"entraRole":        {cfg.entraRequiredRole, "gardenio-viewer"},
		"cookieRoleSource": {cfg.oauth2ProxyRoleSource, oauth2ProxyRoleSourceEntraAppRoles},
	}
	if !cfg.dryRun {
		t.Error("dryRun=false, want true after EDGE_AUTH_DRY_RUN=true")
	}
	for name, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s=%q, want %q", name, tc.got, tc.want)
		}
	}
	if got := cfg.entraAllowedClientIDs; len(got) != 2 || got[0] != "piid-client" || got[1] != "other-client" {
		t.Errorf("entraAllowedClientIDs=%v, want [piid-client other-client]", got)
	}
}

func TestSetupLogger(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", "bogus"} {
		if got := setupLogger(level); got == nil {
			t.Errorf("setupLogger(%q) returned nil", level)
		}
	}
}

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("EDGE_AUTH_TEST_KEY", "")
	if got := envOrDefault("EDGE_AUTH_TEST_KEY", "fallback"); got != "fallback" {
		t.Errorf("envOrDefault unset returned %q, want fallback", got)
	}
	t.Setenv("EDGE_AUTH_TEST_KEY", "value")
	if got := envOrDefault("EDGE_AUTH_TEST_KEY", "fallback"); got != "value" {
		t.Errorf("envOrDefault set returned %q, want value", got)
	}
}

func TestHealthEndpoints(t *testing.T) {
	cfg := &config{
		gitlabBaseURL:   "https://gitlab.example.com",
		gitlabAuthGroup: "gardenio",
	}
	srv := newServer(cfg, "test-1.2.3", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	t.Run("healthz", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["status"] != "ok" {
			t.Errorf("status=%q, want ok", body["status"])
		}
		if body["version"] != "test-1.2.3" {
			t.Errorf("version=%q, want test-1.2.3", body["version"])
		}
	})

	t.Run("readyz", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["status"] != "ok" {
			t.Errorf("status=%q, want ok", body["status"])
		}
	})
}

func TestReadyz_503WhenValidatorUnconfigured(t *testing.T) {
	cfg := &config{} // no gitlab config -> no validator
	srv := newServer(cfg, "test", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", resp.StatusCode)
	}
}

func TestExtractBearerToken(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"", ""},
		{"Bearer abc123", "abc123"},
		{"bearer abc123", "abc123"},
		{"Token abc123", "abc123"},
		{"Basic abc123", ""},
		{"BearerNoSpace", ""},
		{"Bearer   spaced  ", "spaced"},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest("GET", "/", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		if got := extractBearerToken(req); got != tc.want {
			t.Errorf("extractBearerToken(%q)=%q, want %q", tc.header, got, tc.want)
		}
	}
}

func TestHandleAuthCheck_HappyPath(t *testing.T) {
	// Stub GitLab.
	gitlab := newStubGitLab(t)
	defer gitlab.Close()

	cfg := &config{gitlabBaseURL: gitlab.URL, gitlabAuthGroup: "gardenio"}
	srv := newServer(cfg, "test", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/auth/check", nil)
	req.Header.Set("Authorization", "Bearer good-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["email"] != "alice@example.com" {
		t.Errorf("email=%v, want alice@example.com", body["email"])
	}

}
