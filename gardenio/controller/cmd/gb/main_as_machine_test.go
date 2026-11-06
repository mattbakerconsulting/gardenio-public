package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"gardenio/clicreds/oidclogin"
)

// newAsMachineFixture serves a fake Keycloak issuing "machine-tok" to client
// "coord" for secret "s3cret", and a fake daemon whose Authorization headers
// land in *seen. The pod env carries the agent's own token too.
func newAsMachineFixture(t *testing.T) *[]string {
	t.Helper()
	var kc *httptest.Server
	kc = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": kc.URL, "token_endpoint": kc.URL + "/token"})
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
	t.Cleanup(kc.Close)
	seen := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	saved, savedURL, savedAs, savedTok := daemon, httpURL, asMachine, tokenFlag
	t.Cleanup(func() { daemon, httpURL, asMachine, tokenFlag = saved, savedURL, savedAs, savedTok })
	t.Setenv("HOME", t.TempDir())
	t.Setenv(oidclogin.NoSessionEnv, "1")
	for _, k := range []string{tokenFileEnv, "GARDENIO_TOKEN", "KBEADS_TOKEN", "BEADS_AUTH_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("BEADS_DAEMON_TOKEN", "agent-tok")
	t.Setenv("BEADS_REFRESH_TOKEN", "agent-refresh")
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(oidclogin.IssuerEnv, kc.URL)
	t.Setenv(oidclogin.MachineClientIDEnv, "coord")
	t.Setenv(oidclogin.MachineClientSecretFileEnv, secret)
	httpURL = srv.URL
	tokenFlag = ""
	return seen
}

func asMachineProbe() *cobra.Command {
	cmd := &cobra.Command{Use: "probe"}
	cmd.SetContext(context.Background())
	return cmd
}

// --as-machine sends the minted machine bearer and not the agent's own token;
// the same fixture without the flag sends the agent's token.
func TestAsMachineSendsMachineBearer(t *testing.T) {
	seen := newAsMachineFixture(t)
	for _, tc := range []struct {
		as   bool
		want string
	}{{false, "Bearer agent-tok"}, {true, "Bearer machine-tok"}} {
		asMachine = tc.as
		if err := rootCmd.PersistentPreRunE(asMachineProbe(), nil); err != nil {
			t.Fatalf("as-machine=%v: PersistentPreRunE: %v", tc.as, err)
		}
		_, _ = daemon.Me(t.Context())
		if len(*seen) == 0 || (*seen)[len(*seen)-1] != tc.want {
			t.Fatalf("as-machine=%v: daemon saw %q, want %q", tc.as, *seen, tc.want)
		}
	}
}

// The secret is never an argument, and a refused grant stops gb without
// falling back to the agent's token.
func TestAsMachineRefusedDoesNotFallBack(t *testing.T) {
	seen := newAsMachineFixture(t)
	if fl := rootCmd.PersistentFlags().Lookup("as-machine"); fl == nil || fl.Value.Type() != "bool" {
		t.Fatalf("--as-machine must be a bool flag, so the secret can never ride argv: %+v", fl)
	}
	if err := os.WriteFile(os.Getenv(oidclogin.MachineClientSecretFileEnv), []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	asMachine = true
	err := rootCmd.PersistentPreRunE(asMachineProbe(), nil)
	if err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("PersistentPreRunE = %v, want a refusal that does not echo the secret", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("the daemon saw %q after a refused grant", *seen)
	}
}

func TestAsMachineExcludesToken(t *testing.T) {
	newAsMachineFixture(t)
	asMachine, tokenFlag = true, "explicit"
	if err := rootCmd.PersistentPreRunE(asMachineProbe(), nil); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("PersistentPreRunE = %v, want --as-machine/--token refused", err)
	}
}
