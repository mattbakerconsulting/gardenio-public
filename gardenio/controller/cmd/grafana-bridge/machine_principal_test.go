package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// Machine principal for grafana-bridge (gd-Hc34EENbO9): with BEADS_OIDC_* set,
// every daemon call the bridge's client makes carries the service-account
// token from the Keycloak token endpoint; without them the static token is
// used, exactly as before. A token-endpoint failure fails the call closed
// (no request reaches the daemon, no static fallback) and the secret never
// appears on the daemon wire or in the error.

func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "none"}) + "." + enc(claims) + ".sig"
}

type recordingDaemon struct {
	mu   sync.Mutex
	auth []string
}

func (d *recordingDaemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.auth = append(d.auth, r.Header.Get("Authorization"))
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gd-cfg","beads":[]}`))
	})
}

func (d *recordingDaemon) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.auth...)
}

func TestNewBeadsClient_MachinePrincipalCarriesEveryCall(t *testing.T) {
	const secret = "grafana-s3cr3t"
	tok := unsignedJWT(t, map[string]any{
		"azp": "gardenio-grafana-bridge", "preferred_username": "service-account-gardenio-grafana-bridge",
		"aud": "gardenio-traefik-forwardauth", "realm_roles": []string{"gardenio-machine-config-seed"},
		"iat": 1700000000, "exp": 1700001800,
	})
	var fetches int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id, sec, ok := r.BasicAuth()
		if !ok {
			id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		if id != "gardenio-grafana-bridge" || sec != secret {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		fetches++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": 1800})
	}))
	defer tokenSrv.Close()
	d := &recordingDaemon{}
	daemonSrv := httptest.NewServer(d.handler())
	defer daemonSrv.Close()

	secretFile := filepath.Join(t.TempDir(), "clientSecret")
	if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_OIDC_TOKEN_URL", tokenSrv.URL+"/token")
	t.Setenv("BEADS_OIDC_CLIENT_ID", "gardenio-grafana-bridge")
	t.Setenv("BEADS_OIDC_CLIENT_SECRET_FILE", secretFile)

	ts, ok, err := beadsapi.MachineTokenSourceFromEnv(context.Background(), nil)
	if err != nil || !ok {
		t.Fatalf("MachineTokenSourceFromEnv: ok=%v err=%v", ok, err)
	}
	c, err := newBeadsClient(daemonSrv.URL, "static-token", ts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()
	// A read and a write: both ride the machine token (EnsureConfigs does both).
	if _, err := c.GetBead(ctx, "gd-cfg"); err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if _, err := c.CreateBead(ctx, beadsapi.CreateBeadRequest{Title: "context/grafana", Type: "config", Kind: "config"}); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	seen := d.seen()
	if len(seen) != 2 {
		t.Fatalf("daemon saw %d requests, want 2", len(seen))
	}
	for i, a := range seen {
		if a != "Bearer "+tok {
			t.Errorf("request %d Authorization=%q, want the machine token", i, a)
		}
		if strings.Contains(a, secret) || strings.Contains(a, "static-token") {
			t.Errorf("request %d carried the secret or the static token: %q", i, a)
		}
	}
	if fetches != 1 {
		t.Errorf("token endpoint fetched %d times for 2 calls, want 1 (cached)", fetches)
	}
}

func TestNewBeadsClient_StaticWhenMachineUnset(t *testing.T) {
	d := &recordingDaemon{}
	daemonSrv := httptest.NewServer(d.handler())
	defer daemonSrv.Close()
	c, err := newBeadsClient(daemonSrv.URL, "static-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.GetBead(context.Background(), "gd-cfg"); err != nil {
		t.Fatal(err)
	}
	if got := d.seen(); len(got) != 1 || got[0] != "Bearer static-token" {
		t.Fatalf("Authorization=%v, want the static token", got)
	}
}

func TestNewBeadsClient_MachineFetchFailureFailsClosed(t *testing.T) {
	const secret = "grafana-s3cr3t"
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
	}))
	defer tokenSrv.Close()
	d := &recordingDaemon{}
	daemonSrv := httptest.NewServer(d.handler())
	defer daemonSrv.Close()
	secretFile := filepath.Join(t.TempDir(), "clientSecret")
	if err := os.WriteFile(secretFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := beadsapi.NewMachineTokenSource(context.Background(), nil, tokenSrv.URL+"/token", "gardenio-grafana-bridge", secretFile)
	c, err := newBeadsClient(daemonSrv.URL, "static-token", ts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.CreateBead(context.Background(), beadsapi.CreateBeadRequest{Title: "x", Type: "config", Kind: "config"})
	if err == nil || !strings.Contains(err.Error(), "auth_unavailable") {
		t.Fatalf("err=%v, want auth_unavailable", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error text leaked the client secret")
	}
	if n := len(d.seen()); n != 0 {
		t.Fatalf("daemon saw %d requests after a failed token fetch, want 0 (no static fallback)", n)
	}
}
