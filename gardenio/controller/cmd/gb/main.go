// Command gb is the Gardenio agent orchestration CLI — a thin client that
// manages agent lifecycle, decisions, gates, hooks, mail, and session control.
//
// gb is to agents what kd is to beads: kd handles data (CRUD, queries, types),
// gb handles orchestration (spawn, gate, decide, prime, mail).
//
// All data access goes through the kbeads HTTP API via the shared beadsapi client.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"gardenio/clicreds/credstore"
	"gardenio/clicreds/oidclogin"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/jqout"

	"github.com/spf13/cobra"
)

var (
	httpURL    string
	jsonOutput bool
	jqFilter   string
	actor      string
	operatedBy string
	tokenFlag  string
	asMachine  bool

	zitiIdentityPath  string
	zitiService       string
	zitiTLSServerName string

	daemon *beadsapi.Client
)

// resolveBearer is the bearer for --http-url, steps 1-3 of the precedence in
// PersistentPreRunE: --token, then the `gb login` session (refreshed when it
// has expired), then defaultToken; a local agent session with its own token
// skips step 2. The land feed calls it again on each
// reconnect, so a stream outliving one access token resumes with the next.
func resolveBearer(ctx context.Context, warn io.Writer) string {
	if tokenFlag != "" {
		return tokenFlag
	}
	// Inside a `gb agent start --local` session (its hooks included) that
	// was given a token of its own, that token is the agent's identity: the
	// owner's login session in the shared HOME is never sent, nor warned
	// about. A session started on the owner's login exports no token and
	// falls through to it, as before (gd-17BiVe2oC6).
	if os.Getenv(localAgentStartPIDEnv) != "" {
		if tok := defaultToken(); tok != "" {
			return tok
		}
	}
	if tok := loginBearer(ctx, &http.Client{Timeout: oidclogin.HTTPTimeout}, time.Now(), httpURL, warn); tok != "" {
		return tok
	}
	return defaultToken()
}

// defaultToken resolves the bearer token from environment variables, in
// priority order (mirrors kbeads/cmd/kd defaultToken). The --token flag
// overrides this when set.
//
//  1. GARDENIO_TOKEN_FILE — a file holding the token, which its writer may
//     replace while the session runs: 'gl agent start' renews a land
//     agent's token into it (gd-Vbvd3EgqTI). A set but unreadable or empty
//     file resolves no token rather than falling through to another one.
//  2. GARDENIO_TOKEN     — preferred for the edge-auth (cli.gardenio.<env>) path
//     where the token is a user-scoped credential (e.g. GitLab PAT).
//  3. KBEADS_TOKEN      — legacy alias for GARDENIO_TOKEN.
//  4. BEADS_AUTH_TOKEN  — direct-CLI canonical env var.
//  5. BEADS_DAEMON_TOKEN — set by the gardenio controller in agent pods.
func defaultToken() string {
	if p := os.Getenv(tokenFileEnv); p != "" {
		tok, _ := readTokenFile(p)
		return tok
	}
	for _, k := range []string{"GARDENIO_TOKEN", "KBEADS_TOKEN", "BEADS_AUTH_TOKEN", "BEADS_DAEMON_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return unwrapTokenJSON(v)
		}
	}
	return ""
}

// tokenFileEnv names the file defaultToken reads ahead of the token
// variables (mirrors kbeads/cmd/kd).
const tokenFileEnv = "GARDENIO_TOKEN_FILE"

// readTokenFile is the token in the file at path, trimmed; an empty file is
// an error.
func readTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := unwrapTokenJSON(strings.TrimSpace(string(b)))
	if tok == "" {
		return "", fmt.Errorf("%s %s is empty", tokenFileEnv, path)
	}
	return tok, nil
}

// tokenFile is a TokenSource that reads the file on every request, so a gb
// that outlives one token (agent start, yield, a feed) sends the next one
// its writer renewed into the file.
type tokenFile string

func (p tokenFile) Token(context.Context) (string, error) { return readTokenFile(string(p)) }

// unwrapTokenJSON tolerates a bearer value delivered as the JSON object
// {"token":"<value>"} instead of the raw token (mirrors kbeads/cmd/kd). A
// misconfigured ExternalSecret mirror (secretKey without property) injects
// the whole SM JSON value into the env var, and sending it verbatim as
// "Authorization: Bearer {\"token\":...}" 401s every call the pod makes
// (bug ka-0000003595 / ka-0000001051, hit live by gVisor crew). Anything
// that doesn't parse to exactly that shape is returned unchanged.
func unwrapTokenJSON(v string) string {
	trimmed := strings.TrimSpace(v)
	if !strings.HasPrefix(trimmed, "{") {
		return v
	}
	var obj struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil || obj.Token == "" {
		return v
	}
	return obj.Token
}

func defaultActor() string {
	if s := os.Getenv("KD_ACTOR"); s != "" {
		return s
	}
	out, err := exec.Command("git", "config", "user.name").Output()
	if err == nil {
		name := strings.TrimSpace(string(out))
		if name != "" {
			return name
		}
	}
	return "unknown"
}

// defaultOperatedBy reads the human-operator id from the environment, falling
// back to the email captured during `gb login`. The gardenio controller sets
// BEADS_OPERATED_BY in agent pods at spawn time (Slack U-id for Slack-spawned
// agents, OIDC subject for direct-CLI users, empty for scheduled agents).
// Direct human users may set it explicitly via env or --operated-by.
//
// Precedence (highest first):
//  1. --operated-by flag (handled by cobra)
//  2. $BEADS_OPERATED_BY
//  3. credstore current-context User.Email, rendered as "oidc:<email>"
//  4. the calling agent's own bead — fields.operated_by — when it is a
//     well-formed human shape (slack:U…/oidc:…@…). This is the load-bearing
//     fallback for prewarmed pods, where the controller has NOT yet injected
//     BEADS_OPERATED_BY into the process environment but the agent bead
//     (BOAT_AGENT_BEAD_ID/KD_AGENT_ID) already carries the resolved human
//     operator. Without it those pods stamp no operator and the daemon
//     refuses live-state writes with a 400 "operator could not be resolved".
//
// We intentionally do NOT fall back to git config — operated_by is an opaque
// id (slack:U…, oidc:…), not a free-form display name.
// See docs/research/human-attribution-api.md §3.4 and the prewarmed-pod
// attribution fix (ka-0000007812 / epic ka-0000002395).
func defaultOperatedBy() string {
	if s := os.Getenv("BEADS_OPERATED_BY"); s != "" {
		return s
	}
	if s := operatedByFromCredstore(); s != "" {
		return s
	}
	return operatedByFromAgentBead()
}

// operatedByFromCredstore returns "oidc:<email>" from the current credstore
// context, or "" if no email is captured. Errors loading the credstore are
// swallowed: the CLI must still work when the file is absent or unreadable.
func operatedByFromCredstore() string {
	store, err := credstore.Load("")
	if err != nil {
		return ""
	}
	cur := store.Current()
	if cur == nil || cur.User == nil {
		return ""
	}
	email := strings.TrimSpace(cur.User.Email)
	if email == "" {
		return ""
	}
	return "oidc:" + email
}

// defaultProject returns the default project from environment variables.
// Precedence: KD_PROJECT > BOAT_PROJECT > "" (none).
func defaultProject() string { return resolveProject("") }

// defaultGardenioHost is intentionally EMPTY in the public tree: there is no
// compiled-in daemon. Set it per environment at build time
// (-ldflags "-X main.defaultGardenioHost=gardenio.example.com") or at runtime
// ($GARDENIO_DEFAULT_HOST). With nothing set, daemon-touching commands refuse to
// run with a one-line error (requireDaemonURL) instead of dialing a hostname
// that may be dead or wrong. This replaces the 2026-07-07 split-brain lock
// (ka-0000001598): "never silently hit the wrong daemon" is now "never
// silently hit any daemon". In-cluster agents (BEADS_HTTP_ADDR) and CI
// (BEADS_HTTP_URL) are unaffected: those precedence-winning overrides are
// unchanged.
var defaultGardenioHost = ""

// resolveGardenioHost returns the effective default host, preferring the
// $GARDENIO_DEFAULT_HOST runtime override over the build-time defaultGardenioHost.
// A scheme prefix and trailing slashes are tolerated and stripped. Empty when
// neither is set: there is no compiled-in fallback host.
func resolveGardenioHost() string {
	h := defaultGardenioHost
	if env := strings.TrimSpace(os.Getenv("GARDENIO_DEFAULT_HOST")); env != "" {
		h = env
	}
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	return strings.Trim(h, "/")
}

// daemonURLForHost derives the /kd daemon URL for a host; empty host → "".
// It is the final fallback of defaultHTTPURL, so a gb built or run with a
// configured host targets that host's AVA-gated /kd path with no other config.
// Local-daemon dev sets BEADS_HTTP_URL=http://localhost:8080 (or --http-url).
func daemonURLForHost(host string) string {
	if host == "" {
		return ""
	}
	return "https://" + host + "/kd"
}

// errNoDaemonURL is the one-line refusal of an unconfigured client.
var errNoDaemonURL = errors.New("no gardenio daemon URL configured: set BEADS_HTTP_URL (for example https://gardenio.example.com/kd) or pass --http-url")

// requireDaemonURL refuses daemon-touching commands when no URL is configured.
// version, help and completion (and its per-shell children) never dial and are
// exempt, as is a bare group (`gb advice`, no args), which only prints its help.
func requireDaemonURL(cmd *cobra.Command, url string) error {
	if cmd.HasSubCommands() && cmd.Flags().NArg() == 0 {
		return nil
	}
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "version", "help", "completion":
			return nil
		}
	}
	if strings.TrimSpace(url) == "" {
		return errNoDaemonURL
	}
	return nil
}

// defaultGardenioViewerURL is the fallback viewer URL when $BEADS_VIEWER_URL is
// unset, on the same env-aware host as daemonURLForHost. The /beads path prefix
// routes to beads-viewer. Empty when no host is configured.
var defaultGardenioViewerURL = viewerURLForHost(resolveGardenioHost())

// viewerURLForHost derives the /beads viewer URL for a host; empty host → "".
func viewerURLForHost(host string) string {
	if host == "" {
		return ""
	}
	return "https://" + host + "/beads"
}

func defaultHTTPURL() string {
	if s := os.Getenv("BEADS_HTTP_URL"); s != "" {
		return s
	}
	if s := os.Getenv("BEADS_HTTP_ADDR"); s != "" {
		if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
			return "http://" + s
		}
		return s
	}
	return daemonURLForHost(resolveGardenioHost())
}

// agentRefreshToken is the refresh token for the agent's own bearer; a
// --as-machine run never uses it.
func agentRefreshToken() string {
	if asMachine {
		return ""
	}
	return os.Getenv("BEADS_REFRESH_TOKEN")
}

var rootCmd = &cobra.Command{
	Use:   "gb <command>",
	Short: "Gardenio agent orchestration CLI",
	Long: `gb manages agent lifecycle, decisions, gates, hooks, mail, and session control.

It is a client of the kbeads HTTP API — all data access goes through kd serve.
Use kd for bead CRUD (create, show, list, close); use gb for agent orchestration.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Suppress cobra's usage/flags dump for any error that surfaces from
		// here on (PersistentPreRunE, RunE, RPC/HTTP failures). Flag-parse and
		// arg-count errors fire EARLIER — cobra runs ParseFlags + ValidateArgs
		// before invoking this hook — so genuine `gb badflag` mistakes still
		// get the usage block, but the `gb stop` decision-gate refusal, any
		// 4xx, or any runtime failure prints only its one-line cause, not 25
		// lines of flags. Mirrors the per-leaf SilenceUsage already set in
		// dm.go/mr.go/hook.go. See fix-task ka-0000001023 / bug ka-0000003634.
		cmd.SilenceUsage = true
		if err := requireDaemonURL(cmd, httpURL); err != nil {
			return err
		}
		// --jq applies an embedded jq filter to the JSON output, so it
		// implies --json and must compile before any daemon round-trip: a
		// bad filter should fail fast, not after fetching. compiledJQ is
		// consumed by emitJSON (output.go). Graduates advice ka-0000002706
		// (fetch-once, project-many) and ka-0000003286 (bare-array shape hint).
		if jqFilter != "" {
			jsonOutput = true
			f, err := jqout.Parse(jqFilter)
			if err != nil {
				return err
			}
			compiledJQ = f
		}
		if err := validateOperatedBy(operatedBy); err != nil {
			return err
		}
		// Resolve auth token. Precedence:
		//   1. --token flag (explicit override).
		//   2. the `gb login` / `kd login` session (Keycloak access token),
		//      sent only to the daemon it was minted for; empty when not
		//      signed in.
		//   3. defaultToken() — GARDENIO_TOKEN_FILE, then GARDENIO_TOKEN
		//      (edge-auth/PAT path), KBEADS_TOKEN, BEADS_AUTH_TOKEN,
		//      BEADS_DAEMON_TOKEN (agent pod).
		//   4. credstore (OAuth/bearer context left by an older gb login)
		//      when still empty.
		// --as-machine replaces all four with the pod's machine client
		// (Task 15 C5), minted in memory for this one run.
		var token string
		if asMachine {
			if tokenFlag != "" {
				return errors.New("--as-machine and --token are mutually exclusive")
			}
			tok, err := oidclogin.MachineToken(cmd.Context(), &http.Client{Timeout: oidclogin.HTTPTimeout})
			if err != nil {
				return err
			}
			token = tok
		} else {
			token = resolveBearer(cmd.Context(), cmd.ErrOrStderr())
		}
		// A token from GARDENIO_TOKEN_FILE is read again on every request,
		// so a long-lived gb sends the token its writer renewed.
		var fileSource beadsapi.TokenSource
		if p := os.Getenv(tokenFileEnv); p != "" && tokenFlag == "" && !asMachine {
			tok, err := readTokenFile(p)
			switch {
			case token == "" && err != nil:
				// The file is the session's own credential: stop rather
				// than send the credential store's.
				return fmt.Errorf("no token: %w", err)
			case token == "" || token == tok:
				token, fileSource = tok, tokenFile(p)
			}
		}
		cfg := beadsapi.Config{
			HTTPAddr:          httpURL,
			Token:             token,
			TokenSource:       fileSource,
			RefreshToken:      agentRefreshToken(),
			ZitiIdentityPath:  zitiIdentityPath,
			ZitiService:       zitiService,
			ZitiTLSServerName: zitiTLSServerName,
		}
		// When no static token is set, fall back to an OAuth/bearer context an
		// older gb login (or gb viewer-login) left in the local credential
		// store. A context with no bearer, such as one that held only the
		// retired AVA cookie (gd-5FThTGbOOP), is skipped: run gb login.
		if cfg.Token == "" {
			if store, err := credstore.Load(""); err == nil {
				if cur := store.Current(); cur != nil && !cur.HasBearer() {
					fmt.Fprintln(cmd.ErrOrStderr(), "gb: ignoring the stored context with no bearer token (a retired gb login cookie) in "+store.Describe()+"; run gb login (gb logout clears it)")
				} else if cur != nil {
					cfg.TokenSource = credstore.NewTokenSource(store)
					// If the credstore has a server URL and the user didn't
					// override --http-url, prefer the stored server.
					if cur.Server != "" && !httpURLExplicitlyConfigured(cmd) {
						cfg.HTTPAddr = cur.Server
					}
				}
			}
		}
		c, err := beadsapi.New(cfg)
		if err != nil {
			return fmt.Errorf("failed to connect to beads daemon: %w", err)
		}
		// Emit a one-line version-skew warning to stderr the first time the
		// daemon reports a materially-newer build than this binary — the root
		// cause of the friction report was a stale laptop CLI silently
		// re-discovering long-fixed bugs (ka-0000002965).
		c.SetOnDaemonVersion(warnOnVersionSkew)
		if actor != "" {
			c.SetActor(actor)
		}
		if operatedBy != "" {
			c.SetOperatedBy(operatedBy)
		}
		daemon = c
		return nil
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		if daemon != nil {
			daemon.Close()
		}
	},
}

func init() {
	// Append a corrective "…takes that value positionally" hint to unknown-flag
	// errors. Set on the root only — cobra inherits FlagErrorFunc down the whole
	// command tree. See flag_hint.go / bug ka-0000006140.
	rootCmd.SetFlagErrorFunc(flagErrorWithPositionalHint)

	rootCmd.PersistentFlags().StringVar(&httpURL, "http-url", defaultHTTPURL(), "kbeads HTTP server URL")
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output as JSON")
	rootCmd.PersistentFlags().StringVar(&jqFilter, "jq", "", "apply an embedded jq filter to the JSON output (implies --json; projects the response in-process — no external jq, no re-fetch per projection)")
	rootCmd.PersistentFlags().StringVar(&actor, "actor", defaultActor(), "actor name for operations")
	rootCmd.PersistentFlags().StringVar(&operatedBy, "operated-by", defaultOperatedBy(), "human operator id (slack:U…, oidc:…) on whose behalf this call is made; defaults to BEADS_OPERATED_BY env var")
	rootCmd.PersistentFlags().StringVar(&tokenFlag, "token", "", "bearer token for outbound requests (Authorization: Bearer …); overrides GARDENIO_TOKEN / KBEADS_TOKEN / BEADS_AUTH_TOKEN / BEADS_DAEMON_TOKEN")
	rootCmd.PersistentFlags().BoolVar(&asMachine, "as-machine", false, "act as this pod's machine client: mint a client-credentials token in memory from GARDENIO_MACHINE_CLIENT_ID and GARDENIO_MACHINE_CLIENT_SECRET_FILE and send only that")
	rootCmd.PersistentFlags().StringVar(&zitiIdentityPath, "ziti-identity", os.Getenv("GARDENIO_ZITI_IDENTITY"), "enrolled OpenZiti identity JSON for daemon transport; requires --ziti-service")
	rootCmd.PersistentFlags().StringVar(&zitiService, "ziti-service", os.Getenv("GARDENIO_ZITI_SERVICE"), "OpenZiti service name for daemon transport; requires --ziti-identity")
	rootCmd.PersistentFlags().StringVar(&zitiTLSServerName, "ziti-tls-server-name", os.Getenv("GARDENIO_ZITI_TLS_SERVER_NAME"), "optional TLS server name override when dialing the daemon through OpenZiti")

	rootCmd.AddGroup(
		&cobra.Group{ID: "agent", Title: "Agent Lifecycle:"},
		&cobra.Group{ID: "orchestration", Title: "Orchestration:"},
		&cobra.Group{ID: "session", Title: "Session Control:"},
	)

	cobra.EnableCommandSorting = false

	// Agent Lifecycle
	rootCmd.AddCommand(agentCmd)
	rootCmd.AddCommand(spawnCmd)
	rootCmd.AddCommand(wakeCmd)
	rootCmd.AddCommand(waitCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(initAgentCmd)
	rootCmd.AddCommand(gitCredentialEnvCmd)
	rootCmd.AddCommand(gitCredentialProjectedCmd)
	rootCmd.AddCommand(tokenCmd)

	// Orchestration
	rootCmd.AddCommand(gateCmd)
	rootCmd.AddCommand(salvageCmd)
	rootCmd.AddCommand(decisionCmd)
	rootCmd.AddCommand(bundleCmd)
	rootCmd.AddCommand(escalationCmd)
	rootCmd.AddCommand(busCmd)
	rootCmd.AddCommand(hookCmd)
	rootCmd.AddCommand(mailCmd)
	rootCmd.AddCommand(inboxCmd)
	rootCmd.AddCommand(newsCmd)
	rootCmd.AddCommand(pingCmd)
	rootCmd.AddCommand(crewCmd)
	rootCmd.AddCommand(adviceCmd)
	rootCmd.AddCommand(attachmentsCmd)
	rootCmd.AddCommand(mrCmd)
	rootCmd.AddCommand(prCmd)
	rootCmd.AddCommand(pipelineCmd)
	rootCmd.AddCommand(ciCmd)
	rootCmd.AddCommand(releaseCmd)
	rootCmd.AddCommand(slackCmd)
	rootCmd.AddCommand(runnerCmd)
	rootCmd.AddCommand(squawkCmd)
	rootCmd.AddCommand(sayCmd)
	rootCmd.AddCommand(threadCmd)
	rootCmd.AddCommand(probeCmd)
	rootCmd.AddCommand(dmCmd)
	rootCmd.AddCommand(identityCmd)
	rootCmd.AddCommand(prefsCmd)
	rootCmd.AddCommand(userCmd)
	rootCmd.AddCommand(poolCmd)
	rootCmd.AddCommand(peekCmd)
	rootCmd.AddCommand(jiraCmd)
	rootCmd.AddCommand(fleetCmd)
	rootCmd.AddCommand(auditCmd)
	rootCmd.AddCommand(imageCmd)
	rootCmd.AddCommand(grafanaCmd)
	rootCmd.AddCommand(gardenerAuditCmd)
	rootCmd.AddCommand(openChildrenCmd)
	rootCmd.AddCommand(accountCmd)
	rootCmd.AddCommand(usageCmd)
	rootCmd.AddCommand(litellmKeyCmd)
	rootCmd.AddCommand(landsCmd)
	rootCmd.AddCommand(moleculeCmd)
	rootCmd.AddCommand(adminCmd)
	rootCmd.AddCommand(gardenerCmd)
	rootCmd.AddCommand(planCmd)
	rootCmd.AddCommand(telemetryCmd)
	rootCmd.AddCommand(toolsCmd)

	// Session Control
	rootCmd.AddCommand(loginCmd)
	rootCmd.AddCommand(viewerLoginCmd)
	rootCmd.AddCommand(logoutCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(workspaceCmd)
	rootCmd.AddCommand(yieldCmd)
	rootCmd.AddCommand(readyCmd)
	rootCmd.AddCommand(primeCmd)
	rootCmd.AddCommand(nudgePromptCmd)
	rootCmd.AddCommand(prerunCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(sessionCmd)
	rootCmd.AddCommand(stateCmd)
	rootCmd.AddCommand(workflowCmd)
}

// httpURLExplicitlyConfigured reports whether the daemon HTTP URL was set
// deliberately by the user, rather than inherited from the built-in default.
//
// The --http-url flag's default already reads BEADS_HTTP_URL / BEADS_HTTP_ADDR
// (see defaultHTTPURL), but cobra's Flags().Changed() only reflects an explicit
// flag, NOT a default sourced from the environment. Without this, a user who
// configures the daemon via BEADS_HTTP_URL (e.g. an OpenZiti logical origin, or
// any direct daemon address) but ALSO has a stored credstore context would have
// their configured URL silently overwritten by the stored server. Treat an
// env-configured URL as explicit so the credstore branch does not override it.
// See bug ka-0000005476.
func httpURLExplicitlyConfigured(cmd *cobra.Command) bool {
	if cmd != nil && cmd.Flags().Changed("http-url") {
		return true
	}
	return strings.TrimSpace(os.Getenv("BEADS_HTTP_URL")) != "" ||
		strings.TrimSpace(os.Getenv("BEADS_HTTP_ADDR")) != ""
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		var ec *exitCodeError
		if errors.As(err, &ec) {
			os.Exit(ec.code)
		}
		os.Exit(1)
	}
}
