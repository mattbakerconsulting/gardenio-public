// Unit tests for push-bridge helper functions and health endpoints.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

func TestEnvOr(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		value    string
		fallback string
		unset    bool
		want     string
	}{
		{name: "env set", key: "PUSH_BRIDGE_TEST_X", value: "from-env", fallback: "fb", want: "from-env"},
		{name: "env empty falls back", key: "PUSH_BRIDGE_TEST_X", value: "", fallback: "fb", want: "fb"},
		{name: "env unset falls back", key: "PUSH_BRIDGE_TEST_UNSET", unset: true, fallback: "fb", want: "fb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.unset {
				t.Setenv(tt.key, "")
			} else {
				t.Setenv(tt.key, tt.value)
			}
			if got := envOr(tt.key, tt.fallback); got != tt.want {
				t.Errorf("envOr(%q, %q) = %q, want %q", tt.key, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestSetupLogger(t *testing.T) {
	tests := []struct {
		level string
		// We can't easily read the level off a slog.Logger, but we can
		// check Enabled() for each level threshold.
		wantDebug bool
		wantInfo  bool
		wantWarn  bool
		wantError bool
	}{
		{level: "debug", wantDebug: true, wantInfo: true, wantWarn: true, wantError: true},
		{level: "info", wantDebug: false, wantInfo: true, wantWarn: true, wantError: true},
		{level: "warn", wantDebug: false, wantInfo: false, wantWarn: true, wantError: true},
		{level: "error", wantDebug: false, wantInfo: false, wantWarn: false, wantError: true},
		{level: "INFO", wantDebug: false, wantInfo: true, wantWarn: true, wantError: true},
		{level: "bogus", wantDebug: false, wantInfo: true, wantWarn: true, wantError: true}, // default = info
		{level: "", wantDebug: false, wantInfo: true, wantWarn: true, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			lg := setupLogger(tt.level)
			if lg == nil {
				t.Fatal("setupLogger returned nil")
			}
			h := lg.Handler()
			if h.Enabled(context.Background(), slog.LevelDebug) != tt.wantDebug {
				t.Errorf("debug enabled = %v, want %v", !tt.wantDebug, tt.wantDebug)
			}
			if h.Enabled(context.Background(), slog.LevelInfo) != tt.wantInfo {
				t.Errorf("info enabled = %v, want %v", !tt.wantInfo, tt.wantInfo)
			}
			if h.Enabled(context.Background(), slog.LevelWarn) != tt.wantWarn {
				t.Errorf("warn enabled = %v, want %v", !tt.wantWarn, tt.wantWarn)
			}
			if h.Enabled(context.Background(), slog.LevelError) != tt.wantError {
				t.Errorf("error enabled = %v, want %v", !tt.wantError, tt.wantError)
			}
		})
	}
}

func TestParseConfig_DefaultsFromEnv(t *testing.T) {
	t.Setenv("BEADS_HTTP_ADDR", "http://beads.example:9090")
	t.Setenv("PUSH_BRIDGE_LISTEN", ":7777")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("APNS_KEY_PATH", "/etc/apns/key.p8")
	t.Setenv("APNS_KEY_PEM", "pem-contents")
	t.Setenv("APNS_KEY_ID", "KEYID")
	t.Setenv("APNS_TEAM_ID", "TEAMID")
	t.Setenv("APNS_BUNDLE_ID", "com.example.app")
	t.Setenv("APNS_PRODUCTION", "TRUE")

	cfg := parseConfig(nil)

	if cfg.beadsHTTPAddr != "http://beads.example:9090" {
		t.Errorf("beadsHTTPAddr=%q", cfg.beadsHTTPAddr)
	}
	if cfg.listenAddr != ":7777" {
		t.Errorf("listenAddr=%q", cfg.listenAddr)
	}
	if cfg.logLevel != "debug" {
		t.Errorf("logLevel=%q", cfg.logLevel)
	}
	if cfg.apns.KeyPath != "/etc/apns/key.p8" {
		t.Errorf("apns.KeyPath=%q", cfg.apns.KeyPath)
	}
	if cfg.apns.KeyPEM != "pem-contents" {
		t.Errorf("apns.KeyPEM=%q", cfg.apns.KeyPEM)
	}
	if cfg.apns.KeyID != "KEYID" {
		t.Errorf("apns.KeyID=%q", cfg.apns.KeyID)
	}
	if cfg.apns.TeamID != "TEAMID" {
		t.Errorf("apns.TeamID=%q", cfg.apns.TeamID)
	}
	if cfg.apns.BundleID != "com.example.app" {
		t.Errorf("apns.BundleID=%q", cfg.apns.BundleID)
	}
	if !cfg.apns.Production {
		t.Error("apns.Production=false, want true (case-insensitive TRUE)")
	}
	if !cfg.apns.Enabled() {
		t.Error("apns.Enabled() false, want true — all fields populated")
	}
}

func TestParseConfig_DefaultsWhenUnset(t *testing.T) {
	// Clear env so the flag default kicks in.
	for _, k := range []string{
		"BEADS_HTTP_ADDR", "PUSH_BRIDGE_LISTEN", "LOG_LEVEL",
		"APNS_KEY_PATH", "APNS_KEY_PEM", "APNS_KEY_ID",
		"APNS_TEAM_ID", "APNS_BUNDLE_ID", "APNS_PRODUCTION",
	} {
		t.Setenv(k, "")
	}

	cfg := parseConfig(nil)

	if cfg.beadsHTTPAddr != "http://localhost:8080" {
		t.Errorf("beadsHTTPAddr=%q, want default", cfg.beadsHTTPAddr)
	}
	if cfg.listenAddr != ":8091" {
		t.Errorf("listenAddr=%q, want default", cfg.listenAddr)
	}
	if cfg.logLevel != "info" {
		t.Errorf("logLevel=%q, want default info", cfg.logLevel)
	}
	if cfg.apns.Production {
		t.Error("apns.Production=true without env, want false")
	}
	if cfg.apns.Enabled() {
		t.Error("apns.Enabled()=true with empty config, want false")
	}
}

func TestParseConfig_FlagsOverrideEnv(t *testing.T) {
	t.Setenv("BEADS_HTTP_ADDR", "http://env:1234")
	t.Setenv("PUSH_BRIDGE_LISTEN", ":9999")
	t.Setenv("LOG_LEVEL", "warn")

	cfg := parseConfig([]string{
		"-beads-http", "http://flag:5555",
		"-listen", ":6666",
		"-log-level", "error",
	})

	if cfg.beadsHTTPAddr != "http://flag:5555" {
		t.Errorf("beadsHTTPAddr=%q, want flag override", cfg.beadsHTTPAddr)
	}
	if cfg.listenAddr != ":6666" {
		t.Errorf("listenAddr=%q, want flag override", cfg.listenAddr)
	}
	if cfg.logLevel != "error" {
		t.Errorf("logLevel=%q, want flag override", cfg.logLevel)
	}
}

func TestNewHealthMux_Healthz(t *testing.T) {
	mux := newHealthMux("v1.2.3")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type=%q", ct)
	}
	var payload struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v; body=%s", err, rec.Body.String())
	}
	if payload.Status != "ok" {
		t.Errorf("status=%q", payload.Status)
	}
	if payload.Version != "v1.2.3" {
		t.Errorf("version=%q", payload.Version)
	}
}

func TestNewHealthMux_Readyz(t *testing.T) {
	mux := newHealthMux("ignored")
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type=%q", ct)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body=%q", rec.Body.String())
	}
}

func TestNewHealthMux_UnknownPath(t *testing.T) {
	mux := newHealthMux("v")
	req := httptest.NewRequest(http.MethodGet, "/not-a-real-path", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rec.Code)
	}
}

// TestRun_APNsDisabled exercises the early-exit branch of run() when the
// APNs configuration is incomplete.
func TestRun_APNsDisabled(t *testing.T) {
	cfg := config{
		beadsHTTPAddr: "http://localhost:8080",
		listenAddr:    ":0",
		apns:          bridge.PushNotifierConfig{}, // empty → Enabled() = false
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), cfg, logger)
	if !errors.Is(err, errAPNsDisabled) {
		t.Fatalf("run() err=%v, want errAPNsDisabled", err)
	}
}

// TestRun_BeadsClientError exercises the beadsapi.New error path. With
// APNs "enabled" (all fields non-empty) and an empty beadsHTTPAddr, the
// beads daemon client constructor fails fast.
func TestRun_BeadsClientError(t *testing.T) {
	setMachineEnv(t)
	cfg := config{
		beadsHTTPAddr: "", // triggers beadsapi.New "HTTPAddr is required"
		listenAddr:    ":0",
		apns: bridge.PushNotifierConfig{
			KeyPEM:   "not-a-real-pem",
			KeyID:    "K",
			TeamID:   "T",
			BundleID: "b",
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), cfg, logger)
	if err == nil {
		t.Fatal("run() err=nil, want beadsapi error")
	}
	if !strings.Contains(err.Error(), "beads daemon client") {
		t.Errorf("err=%q, want to mention beads daemon client", err)
	}
}

// TestRun_PushNotifierError exercises the bridge.NewPushNotifier error
// path. All APNs fields are set (so Enabled()=true and we proceed past
// the early exit) but the PEM is garbage, so parse fails.
func TestRun_PushNotifierError(t *testing.T) {
	setMachineEnv(t)
	cfg := config{
		beadsHTTPAddr: "http://localhost:1", // valid form, no connection needed for New()
		listenAddr:    ":0",
		apns: bridge.PushNotifierConfig{
			KeyPEM:   "not-a-real-pem",
			KeyID:    "K",
			TeamID:   "T",
			BundleID: "b",
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), cfg, logger)
	if err == nil {
		t.Fatal("run() err=nil, want apns notifier error")
	}
	if !strings.Contains(err.Error(), "apns notifier") {
		t.Errorf("err=%q, want to mention apns notifier", err)
	}
}

// setMachineEnv configures the bridge's machine principal so run() gets past
// the machine-principal check. The token source is lazy: nothing is fetched.
func setMachineEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BEADS_OIDC_TOKEN_URL", "http://keycloak.invalid/token")
	t.Setenv("BEADS_OIDC_CLIENT_ID", "gardenio-push-bridge")
	t.Setenv("BEADS_OIDC_CLIENT_SECRET_FILE", "/nonexistent/clientSecret")
}

// TestRun_MachinePrincipalMissing: without its machine principal the bridge
// cannot read the device registry (kbeads refuses the daemon token), so run()
// fails at startup instead of dropping every alert on a 403.
func TestRun_MachinePrincipalMissing(t *testing.T) {
	t.Setenv("BEADS_OIDC_TOKEN_URL", "")
	t.Setenv("BEADS_OIDC_CLIENT_ID", "")
	t.Setenv("BEADS_OIDC_CLIENT_SECRET_FILE", "")
	cfg := config{
		beadsHTTPAddr: "http://localhost:1",
		listenAddr:    ":0",
		apns:          bridge.PushNotifierConfig{KeyPEM: "not-a-real-pem", KeyID: "K", TeamID: "T", BundleID: "b"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run(context.Background(), cfg, logger); !errors.Is(err, errMachinePrincipalMissing) {
		t.Fatalf("run() err=%v, want errMachinePrincipalMissing", err)
	}

	t.Setenv("BEADS_OIDC_CLIENT_ID", "gardenio-push-bridge")
	if err := run(context.Background(), cfg, logger); err == nil || !strings.Contains(err.Error(), "machine principal") {
		t.Fatalf("run() with a partial machine env err=%v, want a machine principal error", err)
	}
}

type staticTokenSource string

func (s staticTokenSource) Token(context.Context) (string, error) { return string(s), nil }

// TestDeviceRegistry_DeviceCallsUseMachinePrincipal: /v1/devices goes out as
// the bridge's machine principal; bead reads keep the daemon token.
func TestDeviceRegistry_DeviceCallsUseMachinePrincipal(t *testing.T) {
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.Method+" "+r.URL.Path] = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/devices":
			_, _ = io.WriteString(w, `{"devices":[]}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = io.WriteString(w, `{"id":"gd-1"}`)
		}
	}))
	defer srv.Close()

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL, Token: "daemon-token"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	reg := deviceRegistry{Client: client, machine: staticTokenSource("machine-jwt")}

	ctx := context.Background()
	if _, err := reg.ListDevices(ctx, []string{"owner@example.com"}, "ios"); err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if err := reg.DeleteDevice(ctx, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	if _, err := reg.GetBead(ctx, "gd-1"); err != nil {
		t.Fatalf("GetBead: %v", err)
	}

	if got := seen["GET /v1/devices"]; got != "Bearer machine-jwt" {
		t.Errorf("GET /v1/devices auth = %q, want the machine principal", got)
	}
	if got := seen["DELETE /v1/devices/"+strings.Repeat("a", 64)]; got != "Bearer machine-jwt" {
		t.Errorf("DELETE /v1/devices auth = %q, want the machine principal", got)
	}
	if got := seen["GET /v1/beads/gd-1"]; got != "Bearer daemon-token" {
		t.Errorf("GET bead auth = %q, want the daemon token (seen %v)", got, seen)
	}
}
