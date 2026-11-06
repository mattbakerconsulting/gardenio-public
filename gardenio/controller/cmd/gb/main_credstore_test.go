package main

import (
	"os"
	"path/filepath"
	"testing"

	"gardenio/clicreds/credstore"

	"github.com/spf13/cobra"
)

// Tests for the daemon-URL and operator resolution in main.go that reads the
// credstore context an older gb login left behind.

// --- httpURLExplicitlyConfigured -------------------------------------------

// TestHTTPURLExplicitlyConfigured covers the precedence rule that keeps a
// deliberately-configured daemon URL from being overridden by the server a
// stored credstore context names. An explicit
// --http-url flag OR a BEADS_HTTP_URL / BEADS_HTTP_ADDR env var counts as
// configured; an unset URL does not. See bug ka-0000005476.
func TestHTTPURLExplicitlyConfigured(t *testing.T) {
	newCmd := func() *cobra.Command {
		cmd := &cobra.Command{Use: "test"}
		cmd.Flags().String("http-url", "", "")
		return cmd
	}

	t.Run("flag", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "")
		t.Setenv("BEADS_HTTP_ADDR", "")
		cmd := newCmd()
		if err := cmd.Flags().Set("http-url", "http://gardenio-e2e-beads"); err != nil {
			t.Fatal(err)
		}
		if !httpURLExplicitlyConfigured(cmd) {
			t.Fatal("expected explicit http-url flag to count as configured")
		}
	})

	t.Run("BEADS_HTTP_URL", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "http://gardenio-e2e-beads")
		t.Setenv("BEADS_HTTP_ADDR", "")
		if !httpURLExplicitlyConfigured(newCmd()) {
			t.Fatal("expected BEADS_HTTP_URL to count as configured")
		}
	})

	t.Run("BEADS_HTTP_ADDR", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "")
		t.Setenv("BEADS_HTTP_ADDR", "gardenio-e2e-beads:8080")
		if !httpURLExplicitlyConfigured(newCmd()) {
			t.Fatal("expected BEADS_HTTP_ADDR to count as configured")
		}
	})

	t.Run("unset", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "")
		t.Setenv("BEADS_HTTP_ADDR", "")
		if httpURLExplicitlyConfigured(newCmd()) {
			t.Fatal("expected unset HTTP URL inputs to be unconfigured")
		}
	})

	t.Run("nil command with env", func(t *testing.T) {
		t.Setenv("BEADS_HTTP_URL", "http://gardenio-e2e-beads")
		t.Setenv("BEADS_HTTP_ADDR", "")
		if !httpURLExplicitlyConfigured(nil) {
			t.Fatal("expected nil cmd with BEADS_HTTP_URL to count as configured")
		}
	})
}

// --- defaultOperatedBy ----------------------------------------------------

func seedCredstoreWithUser(t *testing.T, dir, email string) {
	t.Helper()
	path := filepath.Join(dir, "gardenio", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{
  "current_context": "default",
  "contexts": {
    "default": {
      "server": "https://viewer.example",
      "user": {"email":"` + email + `","username":"u"}
    }
  }
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultOperatedBy_EnvWins(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("BEADS_OPERATED_BY", "slack:U0123ABCD")
	seedCredstoreWithUser(t, tmp, "user@acme.example")

	if got, want := defaultOperatedBy(), "slack:U0123ABCD"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDefaultOperatedBy_FallsBackToCredstoreEmail(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("BEADS_OPERATED_BY", "")
	seedCredstoreWithUser(t, tmp, "user@acme.example")

	if got, want := defaultOperatedBy(), "oidc:user@acme.example"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDefaultOperatedBy_EmptyWhenCredstoreAbsent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("BEADS_OPERATED_BY", "")
	// Neutralize the step-4 agent-bead fallback so this test exercises only the
	// env+credstore ladder. Reset the per-process memo so prior tests can't leak
	// a cached operator into this one.
	resetOperatedByBeadMemo()
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")

	if got := defaultOperatedBy(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDefaultOperatedBy_EmptyWhenNoUserEmail(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("BEADS_OPERATED_BY", "")
	resetOperatedByBeadMemo()
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")

	// Credstore present but no user.
	path := filepath.Join(tmp, "gardenio", "credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{
  "current_context": "default",
  "contexts": {
    "default": {"server":"https://viewer.example"}
  }
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := defaultOperatedBy(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestLegacyCookieContextHasNoBearer: a context an older gb login stored
// with only the retired cookie loads with no bearer, so gb skips it; one that
// also carries a bearer (e.g. a gb viewer-login token beside an old cookie)
// still authenticates.
func TestLegacyCookieContextHasNoBearer(t *testing.T) {
	cases := []struct {
		name string
		ctx  string
		want bool
	}{
		{"cookie only", `{"server":"https://viewer.example","cookie":"a=b"}`, false},
		{"cookie beside access token", `{"server":"https://viewer.example","cookie":"a=b","access_token":"tok"}`, true},
		{"cookie beside id token", `{"server":"https://viewer.example","cookie":"a=b","id_token":"jwt"}`, true},
		{"no cookie", `{"server":"https://viewer.example","access_token":"tok"}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", tmp)
			t.Setenv("GB_CREDSTORE_BACKEND", "file")
			path := filepath.Join(tmp, "gardenio", "credentials.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			body := `{"current_context":"default","contexts":{"default":` + c.ctx + `}}`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := credstore.Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			cur := store.Current()
			if cur == nil {
				t.Fatal("no current context")
			}
			if got := cur.HasBearer(); got != c.want {
				t.Errorf("HasBearer = %v, want %v", got, c.want)
			}
		})
	}
}
