package main

// agent_start_creds.go — credential discovery for `gb agent start --local`.
//
// On dev hosts the BEADS_HTTP_URL / BEADS_AUTH_TOKEN env vars are typically
// not exported — the user has run `kd remote use <name>` and the active
// remote lives in ~/.local/state/beads/remotes.toml. To make local-mode
// usable without manually exporting env vars, we read that file as a
// fallback and use the active remote's URL + token. An owner with neither,
// only a `gb login` / `kd login` session, runs on that session (gd-O28rNijtUM).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gardenio/clicreds/oidclogin"

	"github.com/BurntSushi/toml"
)

// localCreds is the resolved bundle of beads credentials for a --local spawn.
//
// Priority order (first non-empty wins):
//  1. Environment variable (BEADS_HTTP_URL / BEADS_AUTH_TOKEN, with the
//     gardenio-controller fallback names BEADS_HTTP_ADDR / BEADS_DAEMON_TOKEN
//     handled by the existing localAgentEnv code).
//  2. Active remote from ~/.local/state/beads/remotes.toml.
//  3. No token from either: the owner's `gb login` / `kd login` session for
//     HTTPURL (OwnerLogin). Nothing is exported for it; the session's own kd
//     and gb read and refresh it from the shared HOME, as the owner's do
//     (spec D6: a local agent acts as its owner).
//
// A token from 1 or 2 is the session's only credential: localAgentEnv sets
// oidclogin.NoSessionEnv so its kd and gb never send the owner's login
// session in its place (gd-2bK7yhURFo).
//
// The HTTPURL is guaranteed non-empty (falls back to http://localhost:8080,
// matching kbeads/cmd/kd defaults). Token may be empty when talking to a
// daemon that doesn't require auth (e.g. a local dev daemon).
type localCreds struct {
	HTTPURL   string // e.g. https://gardenio.e2e.example.com/kd
	AuthToken string // bearer token (may be empty)
	NATSURL   string // optional explicit NATS WS URL from the remote
	Source    string // "env" or "remote:<name>" or "default", "+login" with OwnerLogin
	// OwnerLogin: no static token, but the owner's login session signs
	// requests to HTTPURL.
	OwnerLogin bool
	// TokenFile, when set, is exported as GARDENIO_TOKEN_FILE: the session's
	// kd and gb read their token from it, ahead of every token variable, on
	// every request. A runner session's only credential (runnerSessionCreds).
	TokenFile string
}

// ownerLoginFor reports whether the owner's `gb login` / `kd login` session
// can sign requests to url right now (refreshing it if it has lapsed). A var
// so tests do not read the real session file.
var ownerLoginFor = func(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*oidclogin.HTTPTimeout)
	defer cancel()
	return loginBearer(ctx, &http.Client{Timeout: oidclogin.HTTPTimeout}, time.Now(), url, io.Discard) != ""
}

// resolveLocalCreds returns the credential bundle for a --local spawn.
//
// The lookup never fails: missing files / parse errors just yield an
// empty bundle with HTTPURL defaulted. Callers should run preflightLocalCreds
// on the result to surface actionable errors before spawning coop.
func resolveLocalCreds() localCreds {
	out := localCreds{}

	// 1. Environment overrides.
	if u := firstNonEmptyEnv("BEADS_HTTP_URL", "BEADS_HTTP_ADDR"); u != "" {
		out.HTTPURL = normalizeBeadsURL(u)
		out.Source = "env"
	}
	if t := firstNonEmptyEnv("BEADS_AUTH_TOKEN", "BEADS_DAEMON_TOKEN"); t != "" {
		out.AuthToken = t
		if out.Source == "" {
			out.Source = "env"
		}
	}

	// 2. Active remote fallback (only fill in fields still empty).
	if out.HTTPURL == "" || out.AuthToken == "" {
		if remote, name, ok := readActiveRemote(); ok {
			if out.HTTPURL == "" && remote.URL != "" {
				out.HTTPURL = normalizeBeadsURL(remote.URL)
				out.Source = "remote:" + name
			}
			if out.AuthToken == "" && remote.Token != "" {
				out.AuthToken = remote.Token
				if !strings.HasPrefix(out.Source, "remote:") {
					out.Source = "remote:" + name
				}
			}
			if out.NATSURL == "" && remote.NATSURL != "" {
				out.NATSURL = remote.NATSURL
			}
		}
	}

	// 3. Final default (matches kbeads/cmd/kd defaultHTTPURL).
	if out.HTTPURL == "" {
		out.HTTPURL = "http://localhost:8080"
		if out.Source == "" {
			out.Source = "default"
		}
	}

	// 4. The owner's login session, when there is no static token.
	if out.AuthToken == "" && !isLoopbackBeadsURL(out.HTTPURL) && ownerLoginFor(out.HTTPURL) {
		out.OwnerLogin = true
		out.Source += "+login"
	}

	return out
}

// preflightLocalCreds returns an actionable error if the resolved credentials
// would cause coop to fail authentication. The check is intentionally narrow:
// only refuse when we're pointing at a non-loopback URL with no token, since
// that's the case that produces the unhelpful "HTTP 401 on WebSocket upgrade"
// from coop's NATS client.
func preflightLocalCreds(c localCreds) error {
	if c.AuthToken != "" || c.OwnerLogin {
		return nil
	}
	if isLoopbackBeadsURL(c.HTTPURL) {
		return nil
	}
	return fmt.Errorf(`no beads auth token found for %s

gb agent start --local needs your login or a bearer token for the kbeads
daemon, and checks before it registers the agent.

Fix one of:

  0. Sign in as yourself (the session the agent's kd and gb will use):
       gb login --http-url %s

  1. Configure a remote and select it:
       kd remote add <name> <url> --token <token>
       kd remote use <name>

  2. Export the token in the current shell:
       export BEADS_AUTH_TOKEN=<token>
       export BEADS_HTTP_URL=%s

  3. Point at a local daemon that does not require auth:
       export BEADS_HTTP_URL=http://localhost:8080
`, c.HTTPURL, c.HTTPURL, c.HTTPURL)
}

// coopLookPath finds coop; a var so tests need no coop on PATH.
var coopLookPath = func() error {
	_, err := exec.LookPath("coop")
	return err
}

// preflightLocalCoop returns an actionable error when coop, which runs the
// agent session, is not installed. Checked before the agent bead is created,
// so a missing coop does not register an agent that dies a second later.
func preflightLocalCoop() error {
	if err := coopLookPath(); err != nil {
		return fmt.Errorf(`coop is not installed (%v)

gb agent start --local runs the agent session in coop. On Linux take
coop-linux-amd64.tar.gz (coop and coopmux) from the latest gardenio release
into a directory on PATH; on macOS build it from source:
  cd gardenio/coop && cargo build --release
  install -m 0755 target/release/coop target/release/coopmux ~/.local/bin/
See docs/quickstart.md`, err)
	}
	return nil
}

// isLoopbackBeadsURL returns true when url targets localhost / 127.0.0.1.
// Loopback daemons are typically dev sandboxes that don't require auth.
func isLoopbackBeadsURL(url string) bool {
	lower := strings.ToLower(url)
	for _, host := range []string{
		"://localhost", "://127.0.0.1", "://[::1]",
	} {
		if strings.Contains(lower, host) {
			return true
		}
	}
	return false
}

// normalizeBeadsURL prepends http:// when the value is a bare host:port,
// matching kbeads/cmd/kd defaultHTTPURL semantics.
func normalizeBeadsURL(u string) string {
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return "http://" + u
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// remoteEntry mirrors the on-disk shape of kbeads/cmd/kd Remote.
// Keep field names in sync with kbeads/cmd/kd/remote.go.
type remoteEntry struct {
	URL         string `toml:"url"`
	Token       string `toml:"token,omitempty"`
	NATSURL     string `toml:"nats_url,omitempty"`
	Description string `toml:"description,omitempty"`
}

// remotesFile mirrors the on-disk shape of kbeads/cmd/kd RemotesConfig.
type remotesFile struct {
	Active  string                 `toml:"active"`
	Remotes map[string]remoteEntry `toml:"remotes"`
}

// remotesConfigPath returns the path to the kd remotes file.
// Mirrors kbeads/cmd/kd/remote.go: ~/.local/state/beads/remotes.toml.
var remotesConfigPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "beads", "remotes.toml"), nil
}

// readActiveRemote returns the active remote entry and its name, or ok=false.
func readActiveRemote() (remoteEntry, string, bool) {
	path, err := remotesConfigPath()
	if err != nil {
		return remoteEntry{}, "", false
	}
	var cfg remotesFile
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return remoteEntry{}, "", false
	}
	if cfg.Active == "" {
		return remoteEntry{}, "", false
	}
	r, ok := cfg.Remotes[cfg.Active]
	if !ok {
		return remoteEntry{}, "", false
	}
	return r, cfg.Active, true
}
