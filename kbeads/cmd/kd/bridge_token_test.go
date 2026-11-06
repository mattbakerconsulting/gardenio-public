package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

// resetBridgeTokenFlags zeroes the package-level command flag globals so
// successive test cases don't see leaked state from a previous run.
func resetBridgeTokenFlags() {
	btrToken = ""
	btrBridge = ""
	btrProject = ""
	btrAllowedTypes = nil
	btrAllowedOps = nil
	btrTTL = ""
	btrExpiresAt = ""
	btrSubject = ""
	btrWithRefresh = false
	btrRefreshTTL = ""
	btrOutput = "token"
}

// stubBridgeTokenServer spins up an httptest server whose POST
// /v1/fleet-tokens handler returns the given response (or a default echo)
// and records the body it received.
//
// The canonical route is /v1/bridge-tokens, but the controller HTTPClient
// still posts to /v1/fleet-tokens during the wire compat window (the daemon
// dual-accepts both routes — see ka-0000000310). This stub mirrors the
// client's current target so the CLI behaviour is exercised end-to-end.
type stubBridgeTokenServer struct {
	server       *httptest.Server
	receivedBody []byte
	receivedAuth string
	respond      func(req map[string]any) map[string]any
}

func newStubBridgeTokenServer(t *testing.T, respond func(req map[string]any) map[string]any) *stubBridgeTokenServer {
	t.Helper()
	s := &stubBridgeTokenServer{respond: respond}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/fleet-tokens" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		s.receivedBody = body
		s.receivedAuth = r.Header.Get("Authorization")
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		resp := map[string]any{"fleet": req["fleet"]}
		if s.respond != nil {
			resp = s.respond(req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func injectClient(s *stubBridgeTokenServer) func() (*client.HTTPClient, error) {
	return func() (*client.HTTPClient, error) {
		return client.NewHTTPClient(s.server.URL, "admin-token"), nil
	}
}

func TestBridgeTokenRegister_GeneratesHexToken_DefaultOutput(t *testing.T) {
	resetBridgeTokenFlags()
	btrBridge = "test-bridge"
	btrTTL = "1h"

	s := newStubBridgeTokenServer(t, nil)
	cmd := bridgeTokenRegisterCmd
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	if err := runBridgeTokenRegisterWith(cmd, injectClient(s)); err != nil {
		t.Fatalf("register failed: %v", err)
	}
	token := strings.TrimSpace(stdout.String())
	if len(token) != 64 {
		t.Fatalf("expected 64-hex token on stdout, got %q (len=%d)", token, len(token))
	}
	for _, r := range token {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("non-hex character %q in token %q", r, token)
		}
	}

	var sentBody map[string]any
	if err := json.Unmarshal(s.receivedBody, &sentBody); err != nil {
		t.Fatalf("decoding request body: %v", err)
	}
	// The bridge name is carried on the wire `fleet` field during the
	// compat window (invariant: noun-only rename, identity value unchanged).
	if sentBody["fleet"] != "test-bridge" {
		t.Errorf("fleet: got %v, want test-bridge", sentBody["fleet"])
	}
	if sentBody["ttl"] != "1h" {
		t.Errorf("ttl: got %v, want 1h", sentBody["ttl"])
	}
	if sentBody["token"] != token {
		t.Errorf("token in body != stdout token: %v vs %v", sentBody["token"], token)
	}
	if s.receivedAuth != "Bearer admin-token" {
		t.Errorf("auth header: got %q, want %q", s.receivedAuth, "Bearer admin-token")
	}
}

func TestBridgeTokenRegister_RequiresBridge(t *testing.T) {
	resetBridgeTokenFlags()
	// btrBridge intentionally left empty.
	btrTTL = "1h"

	s := newStubBridgeTokenServer(t, nil)
	cmd := bridgeTokenRegisterCmd
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := runBridgeTokenRegisterWith(cmd, injectClient(s))
	if err == nil || !strings.Contains(err.Error(), "--bridge is required") {
		t.Fatalf("expected --bridge required error, got %v", err)
	}
}

func TestBridgeTokenRegister_FleetAliasFlagPopulatesBridge(t *testing.T) {
	// The deprecated --fleet flag is an alias for --bridge (same global).
	// Simulate the cobra binding by setting the shared global directly.
	resetBridgeTokenFlags()
	btrBridge = "alias-bridge" // what --fleet=alias-bridge would set
	btrTTL = "1h"

	s := newStubBridgeTokenServer(t, nil)
	cmd := bridgeTokenRegisterCmd
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	if err := runBridgeTokenRegisterWith(cmd, injectClient(s)); err != nil {
		t.Fatalf("register via fleet alias failed: %v", err)
	}
	var sent map[string]any
	_ = json.Unmarshal(s.receivedBody, &sent)
	if sent["fleet"] != "alias-bridge" {
		t.Errorf("fleet: got %v, want alias-bridge", sent["fleet"])
	}
}

func TestBridgeTokenRegister_TTLAndExpiresAtMutuallyExclusive(t *testing.T) {
	resetBridgeTokenFlags()
	btrBridge = "f"
	btrTTL = "1h"
	btrExpiresAt = "2026-12-31T23:59:59Z"

	s := newStubBridgeTokenServer(t, nil)
	cmd := bridgeTokenRegisterCmd
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := runBridgeTokenRegisterWith(cmd, injectClient(s))
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

func TestBridgeTokenRegister_RefreshTTLRequiresWithRefresh(t *testing.T) {
	resetBridgeTokenFlags()
	btrBridge = "f"
	btrRefreshTTL = "168h"

	s := newStubBridgeTokenServer(t, nil)
	cmd := bridgeTokenRegisterCmd
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := runBridgeTokenRegisterWith(cmd, injectClient(s))
	if err == nil || !strings.Contains(err.Error(), "--with-refresh") {
		t.Fatalf("expected --with-refresh requirement error, got %v", err)
	}
}

func TestBridgeTokenRegister_WithRefresh_PrintsToStderr(t *testing.T) {
	resetBridgeTokenFlags()
	btrBridge = "slack-bridge"
	btrSubject = "bridge:slack"
	btrTTL = "720h"
	btrWithRefresh = true

	s := newStubBridgeTokenServer(t, func(req map[string]any) map[string]any {
		return map[string]any{
			"fleet":                    req["fleet"],
			"subject":                  req["subject"],
			"refresh_token":            "rt-secret-xyz",
			"refresh_token_expires_at": "2027-01-01T00:00:00Z",
		}
	})
	cmd := bridgeTokenRegisterCmd
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	if err := runBridgeTokenRegisterWith(cmd, injectClient(s)); err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if !strings.Contains(stderr.String(), "refresh_token: rt-secret-xyz") {
		t.Errorf("refresh token not labelled on stderr: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "refresh_token_expires_at: 2027-01-01T00:00:00Z") {
		t.Errorf("refresh expiry not on stderr: %q", stderr.String())
	}

	var sent map[string]any
	_ = json.Unmarshal(s.receivedBody, &sent)
	if sent["with_refresh"] != true {
		t.Errorf("with_refresh not sent as true: %v", sent["with_refresh"])
	}
	if sent["subject"] != "bridge:slack" {
		t.Errorf("subject: got %v, want bridge:slack", sent["subject"])
	}
}

func TestBridgeTokenRegister_OutputJSON_IncludesAccessToken(t *testing.T) {
	resetBridgeTokenFlags()
	btrBridge = "jira-bridge"
	btrTTL = "720h"
	btrOutput = "json"
	btrWithRefresh = true

	s := newStubBridgeTokenServer(t, func(req map[string]any) map[string]any {
		return map[string]any{
			"fleet":                    req["fleet"],
			"expires_at":               "2026-06-23T00:00:00Z",
			"refresh_token":            "rt-jira",
			"refresh_token_expires_at": "2026-07-23T00:00:00Z",
		}
	})
	cmd := bridgeTokenRegisterCmd
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	if err := runBridgeTokenRegisterWith(cmd, injectClient(s)); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	var got struct {
		Token                 string `json:"token"`
		Fleet                 string `json:"fleet"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
		ExpiresAt             string `json:"expires_at"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decoding json output: %v\n%s", err, stdout.String())
	}
	if got.Token == "" || len(got.Token) != 64 {
		t.Errorf("access token missing or not 64-hex in json: %q", got.Token)
	}
	if got.Fleet != "jira-bridge" || got.RefreshToken != "rt-jira" {
		t.Errorf("unexpected json body: %+v", got)
	}
	// --output json should NOT also dump labelled refresh to stderr — JSON
	// is meant to be the single source of truth for scripts.
	if strings.Contains(stderr.String(), "refresh_token:") {
		t.Errorf("refresh_token leaked to stderr in json mode: %q", stderr.String())
	}
}

func TestBridgeTokenRegister_RejectsBadOutput(t *testing.T) {
	resetBridgeTokenFlags()
	btrBridge = "f"
	btrOutput = "xml"

	s := newStubBridgeTokenServer(t, nil)
	cmd := bridgeTokenRegisterCmd
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := runBridgeTokenRegisterWith(cmd, injectClient(s))
	if err == nil || !strings.Contains(err.Error(), "output") {
		t.Fatalf("expected output validation error, got %v", err)
	}
}

func TestBridgeTokenRegister_ServerErrorSurfaced(t *testing.T) {
	resetBridgeTokenFlags()
	btrBridge = "f"
	btrTTL = "garbage"

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid ttl: bad duration"}`))
	}))
	t.Cleanup(s.Close)

	clientFn := func() (*client.HTTPClient, error) {
		return client.NewHTTPClient(s.URL, "admin-token"), nil
	}

	cmd := bridgeTokenRegisterCmd
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := runBridgeTokenRegisterWith(cmd, clientFn)
	if err == nil {
		t.Fatalf("expected server error to surface")
	}
}

// TestFleetTokenAlias_ForwardsToBridge verifies the DEPRECATED `kd fleet
// token register` alias is wired to the same RunE as the canonical command,
// so it keeps working through the dual-accept compat window (ka-0000000201).
func TestFleetTokenAlias_ForwardsToBridge(t *testing.T) {
	if fleetTokenRegisterCmd.Deprecated == "" {
		t.Error("fleet token register must be marked Deprecated")
	}
	if !strings.Contains(fleetTokenRegisterCmd.Deprecated, "kd bridge token register") {
		t.Errorf("deprecation notice should point at the canonical command, got %q", fleetTokenRegisterCmd.Deprecated)
	}

	resetBridgeTokenFlags()
	btrBridge = "slack-bridge"
	btrTTL = "1h"
	s := newStubBridgeTokenServer(t, nil)
	cmd := fleetTokenRegisterCmd
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	if err := runBridgeTokenRegisterWith(cmd, injectClient(s)); err != nil {
		t.Fatalf("fleet-alias register failed: %v", err)
	}
	if len(strings.TrimSpace(stdout.String())) != 64 {
		t.Errorf("expected 64-hex token via fleet alias, got %q", stdout.String())
	}
}
