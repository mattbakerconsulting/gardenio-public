package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"gardenio/clicreds/oidclogin"
)

// asMachineFixture is a fake Keycloak issuing "machine-tok" to client "coord"
// for secret "s3cret", and a fake daemon recording each Authorization header.
type asMachineFixture struct {
	mu    sync.Mutex
	seen  []string
	kc    *httptest.Server
	beads *httptest.Server
}

func newAsMachineFixture(t *testing.T) *asMachineFixture {
	t.Helper()
	f := &asMachineFixture{}
	f.kc = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": f.kc.URL, "token_endpoint": f.kc.URL + "/token"})
		case "/token":
			_ = r.ParseForm()
			if r.PostForm.Get("client_id") != "coord" || r.PostForm.Get("client_secret") != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized_client"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "machine-tok", "expires_in": 300})
		default:
			http.NotFound(w, r)
		}
	}))
	f.beads = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Header.Get("Authorization"))
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-x", "title": "x"})
	}))
	t.Cleanup(f.kc.Close)
	t.Cleanup(f.beads.Close)

	saved, savedURL, savedAs, savedTok := beadsClient, httpURL, asMachine, tokenFlag
	t.Cleanup(func() { beadsClient, httpURL, asMachine, tokenFlag = saved, savedURL, savedAs, savedTok })
	t.Setenv("HOME", t.TempDir())
	t.Setenv(oidclogin.NoSessionEnv, "1")
	for _, k := range []string{tokenFileEnv, "GARDENIO_TOKEN", "KBEADS_TOKEN", "BEADS_AUTH_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("BEADS_DAEMON_TOKEN", "agent-tok")
	t.Setenv("BEADS_REFRESH_TOKEN", "agent-refresh")
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(oidclogin.IssuerEnv, f.kc.URL)
	t.Setenv(oidclogin.MachineClientIDEnv, "coord")
	t.Setenv(oidclogin.MachineClientSecretFileEnv, secret)
	httpURL = f.beads.URL
	tokenFlag = ""
	return f
}

func (f *asMachineFixture) get(t *testing.T) string {
	t.Helper()
	cmd := &cobra.Command{Use: "probe"}
	cmd.SetContext(context.Background())
	if err := rootCmd.PersistentPreRunE(cmd, nil); err != nil {
		t.Fatalf("PersistentPreRunE: %v", err)
	}
	_, _ = beadsClient.GetBead(context.Background(), "gd-x")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		t.Fatal("the daemon saw no request")
	}
	return f.seen[len(f.seen)-1]
}

// --as-machine sends the minted machine bearer and not the agent's own token;
// the same fixture without the flag sends the agent's token.
func TestAsMachineSendsMachineBearer(t *testing.T) {
	f := newAsMachineFixture(t)

	asMachine = false
	if got := f.get(t); got != "Bearer agent-tok" {
		t.Fatalf("without --as-machine the daemon saw %q, want the agent's token", got)
	}
	asMachine = true
	if got := f.get(t); got != "Bearer machine-tok" {
		t.Fatalf("with --as-machine the daemon saw %q, want Bearer machine-tok", got)
	}
}

// The secret is never an argument, and a refused grant stops kd without
// falling back to the agent's token.
func TestAsMachineRefusedDoesNotFallBack(t *testing.T) {
	f := newAsMachineFixture(t)
	if fl := rootCmd.PersistentFlags().Lookup("as-machine"); fl == nil || fl.Value.Type() != "bool" {
		t.Fatalf("--as-machine must be a bool flag, so the secret can never ride argv: %+v", fl)
	}
	if err := os.WriteFile(os.Getenv(oidclogin.MachineClientSecretFileEnv), []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	asMachine = true
	cmd := &cobra.Command{Use: "probe"}
	cmd.SetContext(context.Background())
	err := rootCmd.PersistentPreRunE(cmd, nil)
	if err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("PersistentPreRunE = %v, want a refusal that does not echo the secret", err)
	}
	if len(f.seen) != 0 {
		t.Fatalf("the daemon saw %v after a refused grant", f.seen)
	}
}

func TestAsMachineExcludesToken(t *testing.T) {
	newAsMachineFixture(t)
	asMachine, tokenFlag = true, "explicit"
	cmd := &cobra.Command{Use: "probe"}
	cmd.SetContext(context.Background())
	if err := rootCmd.PersistentPreRunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("PersistentPreRunE = %v, want --as-machine/--token refused", err)
	}
}
