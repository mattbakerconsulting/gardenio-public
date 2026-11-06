package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"gardenio/clicreds/oidclogin"
	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/jqout"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExitCodeOperatorRefusal is the EX_USAGE-family sentinel exit code used
// when the kbeads daemon refuses a write because no human operator could
// be attributed to the request. The gardenio agent harness intercepts this
// exit code, posts a `gb squawk`, and files a type=bug bead so the
// refusal is loudly surfaced instead of silently swallowed. See
// ka-0000006731.
const ExitCodeOperatorRefusal = 64

// exitCodeError lets a command request a specific process exit code while still
// printing a human-readable message. classifyAndExit honors it. Used by
// `kd admin audit-blocks-cycles` to exit 2 when the blocks graph is dirty so a
// cron/CI gate can act on it (ka-0000007984).
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }
func (e *exitCodeError) ExitCode() int { return e.code }

var (
	serverAddr string
	httpURL    string
	transport  string
	jsonOutput bool
	jqFilter   string
	actor      string
	fleetFlag  string
	operatedBy string
	tokenFlag  string
	asMachine  bool

	zitiIdentityPath  string
	zitiService       string
	zitiTLSServerName string

	beadsClient client.BeadsClient
)

// defaultToken resolves the bearer token from environment variables, in
// priority order. The --token flag overrides this when set.
//
//  1. GARDENIO_TOKEN_FILE — a file holding the token, which its writer may
//     replace while the session runs: 'gl agent start' renews a land
//     agent's token into it (gd-Vbvd3EgqTI). A set but unreadable or empty
//     file resolves no token rather than falling through to another one.
//  2. GARDENIO_TOKEN  — preferred for the edge-auth (cli.gardenio.<env>) path
//     where the token is a user-scoped credential (e.g. GitLab PAT).
//  3. KBEADS_TOKEN   — legacy alias for GARDENIO_TOKEN.
//  4. BEADS_AUTH_TOKEN — direct-CLI canonical env var.
//  5. BEADS_DAEMON_TOKEN — set by the gardenio controller in agent pods.
//
// See plans/stage3-ava-cli-eb-rollout.md ("CLI surface") for the rationale.
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
// variables (mirrors gardenio/controller/cmd/gb).
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

// unwrapTokenJSON tolerates a bearer value delivered as the JSON object
// {"token":"<value>"} instead of the raw token. A misconfigured
// ExternalSecret mirror (secretKey without property) injects the whole SM
// JSON value into the env var, and sending it verbatim as
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

func defaultFleet() string {
	return os.Getenv("BEADS_FLEET")
}

// defaultOperatedBy reads the human-operator id from the environment, falling
// back to the email captured during `gb login` (rendered as "oidc:<email>").
// The gardenio controller sets BEADS_OPERATED_BY in agent pods at spawn time
// (Slack U-id for Slack-spawned agents, OIDC subject for direct-CLI users,
// empty for scheduled agents). Direct human users may set it explicitly.
//
// Precedence (highest first):
//  1. --operated-by flag (handled by cobra)
//  2. $BEADS_OPERATED_BY
//  3. credstore current-context User.Email, rendered as "oidc:<email>"
//  4. the calling agent's own bead — fields.operated_by — when it is a
//     well-formed human shape (slack:U…/oidc:…@…). Load-bearing fallback for
//     prewarmed pods, where the controller has NOT yet injected
//     BEADS_OPERATED_BY but the agent bead (KD_AGENT_ID/BOAT_AGENT_BEAD_ID)
//     already carries the resolved human operator. Without it those pods
//     stamp no operator and the daemon refuses live-state writes with a 400
//     "operator could not be resolved".
//
// We intentionally do NOT fall back to git config — operated_by is an opaque
// id (slack:U…, oidc:…), not a free-form display name. See R6 §3.4. The
// credstore fallback mirrors gb so kd stamps the same operator id gb would
// from the email an older gb login stored. The bead fallback mirrors gb too
// (ka-0000007812 / epic ka-0000002395).
func defaultOperatedBy() string {
	if s := os.Getenv("BEADS_OPERATED_BY"); s != "" {
		return s
	}
	if s := operatedByFromCredstore(); s != "" {
		return s
	}
	return operatedByFromAgentBead()
}

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
// It is the final fallback of defaultHTTPURL, so a kd built or run with a
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

// noDaemonAnnotation marks a command that never dials the daemon (it talks to
// the database directly, like `kd migrate` in the pre-upgrade Job), so
// requireDaemonURL lets it run with no daemon URL configured.
const noDaemonAnnotation = "gardenio.io/no-daemon"

// requireDaemonURL refuses daemon-touching commands when no URL is configured.
// version, help and completion (and its per-shell children) never dial and are
// exempt, as is a bare group (`kd admin`, no args), which only prints its help,
// and any command annotated with noDaemonAnnotation.
func requireDaemonURL(cmd *cobra.Command, url string) error {
	if cmd.HasSubCommands() && cmd.Flags().NArg() == 0 {
		return nil
	}
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "version", "help", "completion":
			return nil
		}
		if c.Annotations[noDaemonAnnotation] == "true" {
			return nil
		}
	}
	if strings.TrimSpace(url) == "" {
		return errNoDaemonURL
	}
	return nil
}

func defaultHTTPURL() string {
	if s := os.Getenv("BEADS_HTTP_URL"); s != "" {
		return s
	}
	// BEADS_HTTP_ADDR is set by the gardenio controller in agent pods.
	// It contains the full daemon URL (e.g., http://host:8080).
	if s := os.Getenv("BEADS_HTTP_ADDR"); s != "" {
		if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
			return "http://" + s
		}
		return s
	}
	return daemonURLForHost(resolveGardenioHost())
}

func defaultServer() string {
	if s := os.Getenv("BEADS_SERVER"); s != "" {
		return s
	}
	if u := activeRemoteURL(); u != "" {
		return u
	}
	return "localhost:9090"
}

func defaultZitiIdentityPath() string {
	return os.Getenv("GARDENIO_ZITI_IDENTITY")
}

func defaultZitiService() string {
	return os.Getenv("GARDENIO_ZITI_SERVICE")
}

func defaultZitiTLSServerName() string {
	return os.Getenv("GARDENIO_ZITI_TLS_SERVER_NAME")
}

var rootCmd = &cobra.Command{
	Use:   "kd <command>",
	Short: "CLI client for the Beads service",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Suppress cobra's usage/flags dump for any error that surfaces from
		// here on (PersistentPreRunE, RunE, RPC/HTTP failures). Flag-parse and
		// arg-count errors fire EARLIER — cobra runs ParseFlags + ValidateArgs
		// before invoking this hook — so genuine `kd badflag` mistakes still
		// get the usage block, but an operator-refusal 400, any 4xx, or any
		// runtime failure prints only its one-line cause + structured payload,
		// not 25 lines of flags. Mirrors the per-leaf SilenceUsage pattern in
		// gb (dm.go/mr.go/hook.go). See fix-task ka-0000001023 / bug
		// ka-0000003634.
		cmd.SilenceUsage = true
		// The gRPC transport dials --server, not --http-url.
		if transport != "grpc" {
			if err := requireDaemonURL(cmd, httpURL); err != nil {
				return err
			}
		}
		// --jq applies an embedded jq filter to the JSON output, so it
		// implies --json and must compile before any server round-trip: a
		// bad filter should fail fast, not after fetching. compiledJQ is
		// consumed by emitJSON (output.go). Graduates advice ka-0000002706
		// (fetch-once, project-many) and ka-0000003286 (shape hint).
		if jqFilter != "" {
			jsonOutput = true
			f, err := jqout.Parse(jqFilter)
			if err != nil {
				return err
			}
			compiledJQ = f
		}
		// Resolve auth token. Precedence:
		//   1. --token flag (explicit override).
		//   2. defaultToken() — reads GARDENIO_TOKEN_FILE, then GARDENIO_TOKEN
		//      (preferred, edge-auth path), then KBEADS_TOKEN (legacy alias), then BEADS_AUTH_TOKEN
		//      (direct-CLI canonical), then BEADS_DAEMON_TOKEN (agent-pod).
		//   3. activeRemoteToken() — token from the active `kd remote`.
		// A cookie an older gb login left in the credstore is never sent: the
		// AVA cookie handoff is retired (gd-5FThTGbOOP), so kd skips a
		// cookie-only context the way gb does and falls through to step 3.
		token := tokenFlag
		// fromLogin: the bearer is the `kd login` session, so a 401 is retried
		// once with a token refreshed through Keycloak (gd-NrzwHXUxV0).
		fromLogin := false
		loginTarget := httpURL
		if transport == "grpc" {
			loginTarget = serverAddr
		}
		if asMachine {
			// The pod's machine client (Task 15 C5), minted in memory for
			// this one run. No other credential is consulted or sent.
			if tokenFlag != "" {
				return errors.New("--as-machine and --token are mutually exclusive")
			}
			tok, err := oidclogin.MachineToken(cmd.Context(), &http.Client{Timeout: loginHTTPTimeout})
			if err != nil {
				return err
			}
			token = tok
		}
		if token == "" {
			// A `kd login` session (Keycloak access token carrying the
			// person's realm roles) outranks the env/static tokens: signing
			// in is the explicit act, and the shared static token is what
			// admin-gated writes refuse. Empty when not signed in.
			// It is sent only to the daemon it was minted for.
			token = loginBearer(cmd.Context(), &http.Client{Timeout: loginHTTPTimeout}, time.Now(), loginTarget, cmd.ErrOrStderr())
			fromLogin = token != ""
		}
		if token == "" {
			token = defaultToken()
		}
		if p := os.Getenv(tokenFileEnv); token == "" && p != "" {
			// The file is the session's own credential: when it cannot be
			// read, stop rather than send the active remote's.
			tok, err := readTokenFile(p)
			if err != nil {
				return fmt.Errorf("no token: %w", err)
			}
			token = tok
		}
		if token == "" {
			token = activeRemoteToken()
		}

		// Optional refresh token co-located with the access token. Set by
		// the gardenio controller in agent pods (BEADS_REFRESH_TOKEN) so a
		// long-lived `kd` invocation can recover transparently when its
		// access token's TTL expires mid-run.
		// It belongs to the agent's own token, so a machine run never uses it.
		refreshToken := os.Getenv("BEADS_REFRESH_TOKEN")
		if asMachine {
			refreshToken = ""
		}

		switch transport {
		case "http":
			var c *client.HTTPClient
			if zitiIdentityPath != "" || zitiService != "" || zitiTLSServerName != "" {
				// Dial the daemon through an OpenZiti overlay while keeping
				// httpURL as the logical request origin. Ziti gates reachability;
				// bearer auth is unchanged (Option A, ka-0000004050).
				var err error
				c, err = client.NewHTTPClientWithZiti(httpURL, token, client.ZitiConfig{
					IdentityPath:  zitiIdentityPath,
					Service:       zitiService,
					TLSServerName: zitiTLSServerName,
				})
				if err != nil {
					return err
				}
			} else {
				c = client.NewHTTPClient(httpURL, token)
			}
			// Emit a one-line version-skew warning to stderr the first time the
			// daemon reports a materially-newer build than this binary. Root
			// cause of the friction report: a stale laptop CLI silently
			// re-discovering long-fixed bugs (ka-0000002965).
			c.SetOnDaemonVersion(warnOnVersionSkew)
			if refreshToken != "" {
				c.SetRefreshToken(refreshToken)
			}
			if fromLogin {
				c.SetBearerRefresher(loginSessionRefresher(&http.Client{Timeout: loginHTTPTimeout}, loginTarget, cmd.ErrOrStderr()))
			}
			if fleetFlag != "" {
				c.SetFleet(fleetFlag)
			}
			if actor != "" {
				c.SetActor(actor)
			}
			if operatedBy != "" {
				c.SetOperatedBy(operatedBy)
			}
			beadsClient = c
		case "grpc":
			c, err := client.NewGRPCClient(serverAddr, token)
			if err != nil {
				return fmt.Errorf("failed to connect to server: %w", err)
			}
			if fleetFlag != "" {
				c.SetFleet(fleetFlag)
			}
			if actor != "" {
				c.SetActor(actor)
			}
			if operatedBy != "" {
				c.SetOperatedBy(operatedBy)
			}
			beadsClient = c
		default:
			return fmt.Errorf("unknown transport %q (must be http or grpc)", transport)
		}
		return nil
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		if beadsClient != nil {
			beadsClient.Close()
		}
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&httpURL, "http-url", defaultHTTPURL(), "HTTP server URL")
	rootCmd.PersistentFlags().StringVar(&serverAddr, "server", defaultServer(), "gRPC server address")
	rootCmd.PersistentFlags().StringVar(&transport, "transport", "http", "transport protocol (http or grpc)")
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output as JSON")
	rootCmd.PersistentFlags().StringVar(&jqFilter, "jq", "", "apply an embedded jq filter to the JSON output (implies --json; projects the response in-process — no external jq, no re-fetch per projection)")
	rootCmd.PersistentFlags().StringVar(&actor, "actor", defaultActor(), "actor name for created_by fields")
	rootCmd.PersistentFlags().StringVar(&fleetFlag, "fleet", defaultFleet(), "fleet to scope queries to (default: from BEADS_FLEET env var)")
	rootCmd.PersistentFlags().StringVar(&operatedBy, "operated-by", defaultOperatedBy(), "human operator id (slack:U…, oidc:…) on whose behalf this call is made; defaults to BEADS_OPERATED_BY env var")
	rootCmd.PersistentFlags().StringVar(&tokenFlag, "token", "", "bearer token for outbound requests (Authorization: Bearer …); overrides GARDENIO_TOKEN / KBEADS_TOKEN / BEADS_AUTH_TOKEN / BEADS_DAEMON_TOKEN")
	rootCmd.PersistentFlags().BoolVar(&asMachine, "as-machine", false, "act as this pod's machine client: mint a client-credentials token in memory from GARDENIO_MACHINE_CLIENT_ID and GARDENIO_MACHINE_CLIENT_SECRET_FILE and send only that")
	rootCmd.PersistentFlags().StringVar(&zitiIdentityPath, "ziti-identity", defaultZitiIdentityPath(), "enrolled OpenZiti identity JSON for HTTP transport (env: GARDENIO_ZITI_IDENTITY)")
	rootCmd.PersistentFlags().StringVar(&zitiService, "ziti-service", defaultZitiService(), "OpenZiti service name to dial for HTTP transport (env: GARDENIO_ZITI_SERVICE)")
	rootCmd.PersistentFlags().StringVar(&zitiTLSServerName, "ziti-tls-server-name", defaultZitiTLSServerName(), "optional TLS SNI override for Ziti-backed HTTPS origins (env: GARDENIO_ZITI_TLS_SERVER_NAME)")

	rootCmd.AddGroup(
		&cobra.Group{ID: "beads", Title: "Beads:"},
		&cobra.Group{ID: "workflow", Title: "Workflows:"},
		&cobra.Group{ID: "views", Title: "Views:"},
		&cobra.Group{ID: "system", Title: "System:"},
	)

	cobra.EnableCommandSorting = false
	rootCmd.SetHelpFunc(colorizedHelpFunc())

	// Beads
	rootCmd.AddCommand(showCmd)
	rootCmd.AddCommand(fieldsCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(countCmd)
	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(similarCmd)
	rootCmd.AddCommand(createCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(deleteCmd)
	rootCmd.AddCommand(restoreCmd)
	rootCmd.AddCommand(depCmd)
	rootCmd.AddCommand(reparentCmd)
	rootCmd.AddCommand(refsCmd)
	rootCmd.AddCommand(mentionCmd)
	rootCmd.AddCommand(labelCmd)
	rootCmd.AddCommand(commentCmd)
	rootCmd.AddCommand(auditCmd)
	rootCmd.AddCommand(undoCmd)
	rootCmd.AddCommand(taintCmd)
	rootCmd.AddCommand(scrubCmd)
	rootCmd.AddCommand(heraldryCmd)

	// Workflows
	rootCmd.AddCommand(claimCmd)
	rootCmd.AddCommand(unclaimCmd)
	rootCmd.AddCommand(closeCmd)
	rootCmd.AddCommand(reopenCmd)
	rootCmd.AddCommand(doneCmd)
	rootCmd.AddCommand(deferCmd)
	rootCmd.AddCommand(undeferCmd)
	rootCmd.AddCommand(escalationCmd)
	rootCmd.AddCommand(scrubCmd)
	rootCmd.AddCommand(releaseCmd)

	// Views
	rootCmd.AddCommand(orphansCmd)
	rootCmd.AddCommand(viewCmd)
	rootCmd.AddCommand(contextCmd)
	rootCmd.AddCommand(watchCmd)
	rootCmd.AddCommand(treeCmd)
	rootCmd.AddCommand(childrenCmd)
	rootCmd.AddCommand(adviceCmd)
	rootCmd.AddCommand(jackCmd)
	rootCmd.AddCommand(formulaCmd)
	rootCmd.AddCommand(molCmd)

	// Fleet
	rootCmd.AddCommand(fleetCmd)
	rootCmd.AddCommand(teamCmd)
	rootCmd.AddCommand(taintCmd)

	// System
	rootCmd.AddCommand(bridgeCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(healthCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(remoteCmd)
	rootCmd.AddCommand(loginCmd)
	rootCmd.AddCommand(logoutCmd)
	rootCmd.AddCommand(adminCmd)
	rootCmd.AddCommand(embedCmd)
}

// classifyAndExit inspects an error returned from cobra command execution and
// translates it into a process exit code. The operator-refusal sentinel (exit
// code 64) is emitted with a structured JSON stderr payload so the gardenio
// agent harness can decode it without parsing free-form text. All other
// errors get the default exit code 1.
//
// We suppress cobra's own "Error: ..." print for refusals — the structured
// payload is the canonical surface for agents — but pass other errors
// through untouched.
func classifyAndExit(err error) {
	if err == nil {
		return
	}
	// A command that requested a specific exit code (e.g. audit-blocks-cycles
	// exit 2 on a dirty graph). cobra has already printed the message unless
	// the command set SilenceErrors, so just exit with the requested code.
	var ec *exitCodeError
	if errors.As(err, &ec) {
		os.Exit(ec.ExitCode())
	}
	if source, ok := detectOperatorRefusal(err); ok {
		// Emit a single-line JSON object on stderr. Keys are stable so the
		// gardenio agent harness can key on them without regex tricks. The
		// resolver_source is the only field that varies between refusals.
		payload := map[string]string{
			"error":           "operator_refusal",
			"resolver_source": source,
			"message":         "operator could not be resolved",
			"command":         commandPathFromArgs(os.Args),
		}
		if data, mErr := json.Marshal(payload); mErr == nil {
			fmt.Fprintln(os.Stderr, string(data))
		} else {
			fmt.Fprintf(os.Stderr, "operator_refusal: %s\n", source)
		}
		os.Exit(ExitCodeOperatorRefusal)
	}
	os.Exit(1)
}

// detectOperatorRefusal inspects an error tree for either:
//   - a typed *client.OperatorRefusalError from the HTTP transport
//   - a gRPC status whose message embeds the canonical JSON refusal payload
//
// On a match, returns the resolver_source label and true; otherwise false.
func detectOperatorRefusal(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var refusal *client.OperatorRefusalError
	if errors.As(err, &refusal) {
		src := refusal.ResolverSource
		if src == "" {
			src = "unknown"
		}
		return src, true
	}
	// gRPC path: server embeds {"code":"operator_refusal","resolver_source":"X"}
	// inside the status message string.
	if st, ok := status.FromError(err); ok && st.Code() == codes.InvalidArgument {
		if src, matched := parseGRPCRefusalMessage(st.Message()); matched {
			return src, true
		}
	}
	return "", false
}

// parseGRPCRefusalMessage extracts the resolver_source from a gRPC
// InvalidArgument status message that embeds the canonical JSON refusal
// payload. Returns (source, true) on a match.
func parseGRPCRefusalMessage(msg string) (string, bool) {
	start := strings.Index(msg, "{")
	if start < 0 {
		return "", false
	}
	var payload struct {
		Code           string `json:"code"`
		ResolverSource string `json:"resolver_source"`
	}
	if err := json.Unmarshal([]byte(msg[start:]), &payload); err != nil {
		return "", false
	}
	if payload.Code != client.OperatorRefusalErrorCode {
		return "", false
	}
	src := payload.ResolverSource
	if src == "" {
		src = "unknown"
	}
	return src, true
}

// commandPathFromArgs returns a sanitized "kd <subcommand> <subsubcommand>"
// summary suitable for the structured refusal payload. We intentionally
// drop flag values to avoid leaking secrets that might appear in
// --description or --notes — only the binary name and bare positional
// subcommand path are emitted.
func commandPathFromArgs(argv []string) string {
	if len(argv) == 0 {
		return "kd"
	}
	parts := []string{"kd"}
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "-") {
			break
		}
		parts = append(parts, a)
		if len(parts) >= 4 {
			break
		}
	}
	return strings.Join(parts, " ")
}

func main() {
	// SilenceErrors=false would cause cobra to also print the error before
	// our handler runs. We let cobra's default behaviour stand for normal
	// errors (so users still see the message) and only suppress it for the
	// refusal case — handled inline in classifyAndExit via the structured
	// stderr payload (cobra's free-form print is benign extra context).
	err := rootCmd.Execute()
	classifyAndExit(err)
}
