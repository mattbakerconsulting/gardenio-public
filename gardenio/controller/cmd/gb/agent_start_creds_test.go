package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/clicreds/oidclogin"
)

func TestResolveLocalCreds(t *testing.T) {
	// Redirect the remotes config file lookup to a temp dir for the suite.
	orig := remotesConfigPath
	t.Cleanup(func() { remotesConfigPath = orig })
	// No owner login unless a case says so.
	origLogin := ownerLoginFor
	t.Cleanup(func() { ownerLoginFor = origLogin })
	ownerLoginFor = func(string) bool { return false }

	t.Run("env BEADS_HTTP_URL + BEADS_AUTH_TOKEN take priority", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "https://env.example/")
		t.Setenv("BEADS_AUTH_TOKEN", "env-token")
		// remotes.toml is also set; env still wins.
		dir := t.TempDir()
		remotesConfigPath = func() (string, error) {
			return seedRemote(t, dir, "remote-token", "https://remote.example/"), nil
		}
		c := resolveLocalCreds()
		if c.HTTPURL != "https://env.example/" {
			t.Errorf("HTTPURL = %q", c.HTTPURL)
		}
		if c.AuthToken != "env-token" {
			t.Errorf("AuthToken = %q", c.AuthToken)
		}
		if c.Source != "env" {
			t.Errorf("Source = %q, want env", c.Source)
		}
	})

	t.Run("BEADS_HTTP_ADDR is a synonym for BEADS_HTTP_URL", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "")
		t.Setenv("BEADS_HTTP_ADDR", "https://addr.example/")
		t.Setenv("BEADS_AUTH_TOKEN", "tok")
		remotesConfigPath = func() (string, error) {
			return filepath.Join(t.TempDir(), "missing.toml"), nil
		}
		c := resolveLocalCreds()
		if c.HTTPURL != "https://addr.example/" {
			t.Errorf("HTTPURL = %q", c.HTTPURL)
		}
	})

	t.Run("BEADS_DAEMON_TOKEN is a fallback for BEADS_AUTH_TOKEN", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "https://x.example/")
		t.Setenv("BEADS_AUTH_TOKEN", "")
		t.Setenv("BEADS_DAEMON_TOKEN", "daemon-tok")
		remotesConfigPath = func() (string, error) {
			return filepath.Join(t.TempDir(), "missing.toml"), nil
		}
		c := resolveLocalCreds()
		if c.AuthToken != "daemon-tok" {
			t.Errorf("AuthToken = %q, want daemon-tok", c.AuthToken)
		}
	})

	t.Run("falls through to active remote when env is empty", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "")
		t.Setenv("BEADS_HTTP_ADDR", "")
		t.Setenv("BEADS_AUTH_TOKEN", "")
		t.Setenv("BEADS_DAEMON_TOKEN", "")
		dir := t.TempDir()
		remotesConfigPath = func() (string, error) {
			return seedRemote(t, dir, "remote-token", "https://prod.beads.example/"), nil
		}
		c := resolveLocalCreds()
		if c.HTTPURL != "https://prod.beads.example/" {
			t.Errorf("HTTPURL = %q", c.HTTPURL)
		}
		if c.AuthToken != "remote-token" {
			t.Errorf("AuthToken = %q", c.AuthToken)
		}
		if !strings.HasPrefix(c.Source, "remote:") {
			t.Errorf("Source = %q, want remote:*", c.Source)
		}
	})

	t.Run("partial env merges with remote", func(t *testing.T) {
		// User exported URL only; remote provides the token.
		t.Setenv("BEADS_HTTP_URL", "https://explicit.example/")
		t.Setenv("BEADS_AUTH_TOKEN", "")
		t.Setenv("BEADS_DAEMON_TOKEN", "")
		dir := t.TempDir()
		remotesConfigPath = func() (string, error) {
			return seedRemote(t, dir, "remote-token", "https://prod.beads.example/"), nil
		}
		c := resolveLocalCreds()
		if c.HTTPURL != "https://explicit.example/" {
			t.Errorf("HTTPURL = %q, want env value", c.HTTPURL)
		}
		if c.AuthToken != "remote-token" {
			t.Errorf("AuthToken = %q, want remote value", c.AuthToken)
		}
	})

	t.Run("default URL when env + remote absent", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "")
		t.Setenv("BEADS_HTTP_ADDR", "")
		t.Setenv("BEADS_AUTH_TOKEN", "")
		t.Setenv("BEADS_DAEMON_TOKEN", "")
		remotesConfigPath = func() (string, error) {
			return filepath.Join(t.TempDir(), "missing.toml"), nil
		}
		c := resolveLocalCreds()
		if c.HTTPURL != "http://localhost:8080" {
			t.Errorf("HTTPURL = %q, want default", c.HTTPURL)
		}
		if c.AuthToken != "" {
			t.Errorf("AuthToken = %q, want empty", c.AuthToken)
		}
	})

	t.Run("owner login stands in for a missing token", func(t *testing.T) {
		// gd-O28rNijtUM: a laptop with only `kd login` / `gb login`.
		t.Setenv("BEADS_HTTP_URL", "https://api.clonk.tv")
		t.Setenv("BEADS_AUTH_TOKEN", "")
		t.Setenv("BEADS_DAEMON_TOKEN", "")
		remotesConfigPath = func() (string, error) {
			return filepath.Join(t.TempDir(), "missing.toml"), nil
		}
		var asked string
		ownerLoginFor = func(url string) bool { asked = url; return true }
		t.Cleanup(func() { ownerLoginFor = func(string) bool { return false } })
		c := resolveLocalCreds()
		if asked != "https://api.clonk.tv" {
			t.Errorf("login probed for %q, want the resolved URL", asked)
		}
		if !c.OwnerLogin || c.AuthToken != "" {
			t.Errorf("OwnerLogin = %v, AuthToken = %q; want login and no token", c.OwnerLogin, c.AuthToken)
		}
		if c.Source != "env+login" {
			t.Errorf("Source = %q, want env+login", c.Source)
		}
		if err := preflightLocalCreds(c); err != nil {
			t.Errorf("preflight refused the owner login: %v", err)
		}
	})

	t.Run("a static token wins over the owner login", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "https://api.clonk.tv")
		t.Setenv("BEADS_AUTH_TOKEN", "tok")
		remotesConfigPath = func() (string, error) {
			return filepath.Join(t.TempDir(), "missing.toml"), nil
		}
		ownerLoginFor = func(string) bool { t.Error("login probed despite a token"); return true }
		t.Cleanup(func() { ownerLoginFor = func(string) bool { return false } })
		if c := resolveLocalCreds(); c.OwnerLogin || c.AuthToken != "tok" {
			t.Errorf("OwnerLogin = %v, AuthToken = %q", c.OwnerLogin, c.AuthToken)
		}
	})

	t.Run("bare host:port gets http:// prefix", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "")
		t.Setenv("BEADS_HTTP_ADDR", "kbeads.svc:8080")
		t.Setenv("BEADS_AUTH_TOKEN", "tok")
		remotesConfigPath = func() (string, error) {
			return filepath.Join(t.TempDir(), "missing.toml"), nil
		}
		c := resolveLocalCreds()
		if c.HTTPURL != "http://kbeads.svc:8080" {
			t.Errorf("HTTPURL = %q", c.HTTPURL)
		}
	})
}

func TestPreflightLocalCreds(t *testing.T) {
	cases := []struct {
		name    string
		creds   localCreds
		wantErr bool
	}{
		{name: "token present, non-loopback OK", creds: localCreds{HTTPURL: "https://prod/", AuthToken: "t"}, wantErr: false},
		{name: "loopback without token OK", creds: localCreds{HTTPURL: "http://localhost:8080"}, wantErr: false},
		{name: "127.0.0.1 without token OK", creds: localCreds{HTTPURL: "http://127.0.0.1:8080"}, wantErr: false},
		{name: "ipv6 loopback without token OK", creds: localCreds{HTTPURL: "http://[::1]:8080"}, wantErr: false},
		{name: "non-loopback without token rejected", creds: localCreds{HTTPURL: "https://prod.beads.example/"}, wantErr: true},
		{name: "owner login without token OK", creds: localCreds{HTTPURL: "https://prod.beads.example/", OwnerLogin: true}, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := preflightLocalCreds(tc.creds)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				// Error message must mention the URL and suggest the fixes.
				if !strings.Contains(err.Error(), tc.creds.HTTPURL) {
					t.Errorf("error missing URL: %v", err)
				}
				if !strings.Contains(err.Error(), "kd remote") {
					t.Errorf("error missing `kd remote` hint: %v", err)
				}
				if !strings.Contains(err.Error(), "BEADS_AUTH_TOKEN") {
					t.Errorf("error missing BEADS_AUTH_TOKEN hint: %v", err)
				}
				if !strings.Contains(err.Error(), "gb login") {
					t.Errorf("error missing `gb login` hint: %v", err)
				}
			}
		})
	}
}

func TestLocalAgentEnv_ExportsResolvedToken(t *testing.T) {
	creds := localCreds{
		HTTPURL:   "https://prod.beads.example/",
		AuthToken: "secret-tok",
	}
	env := localAgentEnv("agent", "gd-bead", "crew", "/tmp", "http://127.0.0.1:8080", "", creds)

	checks := map[string]string{
		"BEADS_AUTH_TOKEN":   "secret-tok",
		"BEADS_DAEMON_TOKEN": "secret-tok",
		"COOP_NATS_TOKEN":    "secret-tok",
		"BEADS_HTTP_URL":     "https://prod.beads.example/",
	}
	for k, want := range checks {
		if got := findEnv(env, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestLocalAgentEnv_TokenLeavesCoopNATSOff(t *testing.T) {
	// A bearer token is no reason to dial <beads URL>/nats: nothing serves
	// it, and coop would retry the refused upgrade for the whole session
	// (gd-DTyKMgQXyz). A stale parent-shell NATS env must not leak through.
	t.Setenv("COOP_NATS_URL", "wss://stale.example/nats")
	t.Setenv("COOP_NATS_FORWARD", "1")
	creds := localCreds{HTTPURL: "https://prod.beads.example/", AuthToken: "tok"}
	env := localAgentEnv("agent", "gd-bead", "crew", "/tmp", "http://127.0.0.1:8080", "", creds)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "COOP_NATS_") && key != "COOP_NATS_TOKEN" {
			t.Errorf("env carries %s with no remote nats_url", kv)
		}
	}
}

func TestLocalAgentEnv_NoTokenWhenAbsent(t *testing.T) {
	// Loopback dev daemon without a token: we must not export an empty token,
	// since that would actively break a daemon that allows anonymous access.
	creds := localCreds{HTTPURL: "http://localhost:8080"}
	env := localAgentEnv("agent", "gd-bead", "crew", "/tmp", "http://127.0.0.1:8080", "", creds)

	for _, k := range []string{"BEADS_AUTH_TOKEN", "BEADS_DAEMON_TOKEN", "COOP_NATS_TOKEN"} {
		if v := findEnv(env, k); v != "" {
			t.Errorf("%s = %q, want empty", k, v)
		}
	}
}

func TestLocalAgentEnv_RemoteNATSURLOverridesDefault(t *testing.T) {
	creds := localCreds{
		HTTPURL:   "https://prod.beads.example/",
		AuthToken: "tok",
		NATSURL:   "wss://nats.prod.example/nats",
	}
	env := localAgentEnv("agent", "gd-bead", "crew", "/tmp", "http://127.0.0.1:8080", "", creds)
	if got := findEnv(env, "COOP_NATS_URL"); got != "wss://nats.prod.example/nats" {
		t.Errorf("COOP_NATS_URL = %q, want explicit nats url from remote", got)
	}
}

func TestLocalAgentEnv_OwnerLoginLeavesCoopNATSOff(t *testing.T) {
	// No bearer for coop's NATS upgrade: coop must not be pointed at a
	// /nats nothing serves, even if the parent shell exported one.
	t.Setenv("COOP_NATS_URL", "wss://stale.example/nats")
	t.Setenv("COOP_NATS_FORWARD", "1")
	creds := localCreds{HTTPURL: "https://api.clonk.tv", OwnerLogin: true}
	env := localAgentEnv("agent", "gd-bead", "local", "/tmp", "http://127.0.0.1:8080", "", creds)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "COOP_NATS_") {
			t.Errorf("env carries %s on the owner login", kv)
		}
		if key == "BEADS_AUTH_TOKEN" || key == "BEADS_DAEMON_TOKEN" {
			t.Errorf("env exports %s on the owner login; the session reads the login itself", key)
		}
	}
	if got := findEnv(env, "BEADS_HTTP_URL"); got != "https://api.clonk.tv" {
		t.Errorf("BEADS_HTTP_URL = %q", got)
	}

	// An explicit nats_url from the remote still wins.
	creds.NATSURL = "wss://nats.example/nats"
	env = localAgentEnv("agent", "gd-bead", "local", "/tmp", "http://127.0.0.1:8080", "", creds)
	if got := findEnv(env, "COOP_NATS_URL"); got != "wss://nats.example/nats" {
		t.Errorf("COOP_NATS_URL = %q, want the remote's nats_url", got)
	}
}

func TestPreflightLocalCoop(t *testing.T) {
	orig := coopLookPath
	t.Cleanup(func() { coopLookPath = orig })

	coopLookPath = func() error { return nil }
	if err := preflightLocalCoop(); err != nil {
		t.Errorf("coop found: err = %v", err)
	}
	coopLookPath = func() error { return errors.New(`exec: "coop": executable file not found in $PATH`) }
	err := preflightLocalCoop()
	if err == nil {
		t.Fatal("coop missing: want an error")
	}
	for _, want := range []string{"coop-linux-amd64.tar.gz", "cargo build --release", "docs/quickstart.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// seedRemote writes a minimal kd remotes.toml under dir and returns its path.
// The remote is named "test" and marked active.
func seedRemote(t *testing.T, dir, token, url string) string {
	t.Helper()
	path := filepath.Join(dir, "remotes.toml")
	body := "active = \"test\"\n\n[remotes.test]\nurl = \"" + url + "\"\ntoken = \"" + token + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	return path
}

// gd-2bK7yhURFo: the owner is signed in for the daemon the agent talks to,
// and kd and gb send that session ahead of any token in the env. A session
// handed a token must run on it, so its env turns the login session off; a
// session with no token (OwnerLogin) keeps the owner's login.
func TestLocalAgentEnv_TokenSessionNeverSendsOwnerLogin(t *testing.T) {
	useTempGBLoginFile(t)
	now := time.Now()
	if err := oidclogin.Save(&oidclogin.Session{Daemon: testGBDaemonHost, AccessToken: "owner-session", Expiry: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(oidclogin.NoSessionEnv, "")
	os.Unsetenv(oidclogin.NoSessionEnv)

	// bearerIn is the login bearer a gb in the session would send.
	bearerIn := func(env []string) string {
		t.Helper()
		t.Setenv(oidclogin.NoSessionEnv, findEnv(env, oidclogin.NoSessionEnv))
		return loginBearer(context.Background(), http.DefaultClient, now, findEnv(env, "BEADS_HTTP_URL"), io.Discard)
	}

	tok := localCreds{HTTPURL: testGBDaemonURL, AuthToken: "fleet-token", Source: "remote:laptop"}
	login := localCreds{HTTPURL: testGBDaemonURL, OwnerLogin: true, Source: "env+login"}
	// Both envs are built before bearerIn sets anything in this process.
	env := localAgentEnv("agent", "gd-bead", "local", "/tmp", "http://127.0.0.1:8080", "", tok)
	loginEnv := localAgentEnv("agent", "gd-bead", "local", "/tmp", "http://127.0.0.1:8080", "", login)
	if got := findEnv(env, oidclogin.NoSessionEnv); got != "1" {
		t.Errorf("%s = %q with a token, want 1", oidclogin.NoSessionEnv, got)
	}
	if got := bearerIn(env); got != "" {
		t.Errorf("session with a token sends the owner's login %q", got)
	}
	if got := findEnv(env, "BEADS_AUTH_TOKEN"); got != "fleet-token" {
		t.Errorf("BEADS_AUTH_TOKEN = %q, want the resolved token", got)
	}

	// Positive control: with no token the session runs on the owner's login.
	if got := findEnv(loginEnv, oidclogin.NoSessionEnv); got != "" {
		t.Errorf("%s = %q on the owner login, want unset", oidclogin.NoSessionEnv, got)
	}
	if got := bearerIn(loginEnv); got != "owner-session" {
		t.Errorf("owner-login session bearer = %q, want owner-session", got)
	}
}
