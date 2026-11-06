package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// bridgeCmd is the canonical home for bridge-credential token management
// (ka-0000005788). It groups the admin-only bearer-token surface that the
// slack/jira/gitlab bridges use to authenticate to the beads daemon.
//
// NOTE: this is the bridge-CREDENTIAL surface only. It is unrelated to the
// tenancy `fleet` concept (`kd fleet list/show/transfer`, the fleet DB
// column, fleet maps/metrics). The deprecated `kd fleet token` alias
// (fleet_token.go) forwards here during the dual-accept compat window
// (decision ka-0000000201; alias removal is cleanup bead ka-0000004906).
var bridgeCmd = &cobra.Command{
	Use:     "bridge",
	Short:   "Bridge credential management (admin-only)",
	GroupID: "system",
}

var bridgeTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Bridge token management (admin-only)",
}

var bridgeTokenRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register a bridge token via POST /v1/bridge-tokens",
	Long: `Register a bridge token with the beads daemon. Wraps POST /v1/bridge-tokens
(the legacy /v1/fleet-tokens route remains an accepted alias during the
compat window).

The token grants any caller bearing it the privileges configured on the
bridge identity (and any --project / --allowed-types / --allowed-ops
scopes). Use --ttl to bound the access-token lifetime and --with-refresh to
mint a companion refresh token in the same round-trip.

Admin-only on the daemon side: the bearer of --token (or the env-var
fallback chain GARDENIO_TOKEN → KBEADS_TOKEN → BEADS_AUTH_TOKEN →
BEADS_DAEMON_TOKEN) must be the daemon's primary auth token.`,
	Example: `  # Mint a fresh token, scope to a bridge for 24h:
  kd bridge token register --bridge ops --ttl 24h --output token

  # Issue a hardened bridge token: TTL + subject + refresh.
  kd bridge token register --bridge slack-bridge --subject bridge:slack \
      --ttl 720h --with-refresh --output json`,
	RunE: runBridgeTokenRegister,
}

var (
	btrToken        string
	btrBridge       string
	btrProject      string
	btrAllowedTypes []string
	btrAllowedOps   []string
	btrTTL          string
	btrExpiresAt    string
	btrSubject      string
	btrWithRefresh  bool
	btrRefreshTTL   string
	btrOutput       string
	btrPrivileged   bool
)

// registerBridgeTokenRegisterFlags binds the register flags onto cmd. Both the
// canonical `bridge token register` command and the deprecated `fleet token
// register` alias call this so they share the same flag surface and the same
// package-level flag globals (only one command runs per invocation).
func registerBridgeTokenRegisterFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&btrToken, "token-value", "", "bearer token to register (default: generate a 64-hex random token)")
	cmd.Flags().StringVar(&btrBridge, "bridge", "", "bridge name to register the token under (required)")
	// Deprecated alias for --bridge (dual-accept compat window, ka-0000000201).
	cmd.Flags().StringVar(&btrBridge, "fleet", "", "deprecated alias for --bridge")
	_ = cmd.Flags().MarkDeprecated("fleet", "use --bridge")
	cmd.Flags().StringVar(&btrProject, "project", "", "optional project scope")
	cmd.Flags().StringSliceVar(&btrAllowedTypes, "allowed-types", nil, "optional bead type scope (comma-separated)")
	cmd.Flags().StringSliceVar(&btrAllowedOps, "allowed-ops", nil, "optional operation scope (comma-separated)")
	cmd.Flags().StringVar(&btrTTL, "ttl", "", "access-token TTL (Go duration, e.g. 24h, 720h); mutually exclusive with --expires-at")
	cmd.Flags().StringVar(&btrExpiresAt, "expires-at", "", "access-token absolute expiry (RFC3339); mutually exclusive with --ttl")
	cmd.Flags().StringVar(&btrSubject, "subject", "", "audit subject claim (e.g. bridge:slack); falls back to bridge name when empty")
	cmd.Flags().BoolVar(&btrWithRefresh, "with-refresh", false, "also mint a companion refresh token (printed labelled to stderr unless --output json)")
	cmd.Flags().StringVar(&btrRefreshTTL, "refresh-ttl", "", "refresh-token TTL (Go duration); requires --with-refresh; daemon default applies when empty")
	cmd.Flags().StringVar(&btrOutput, "output", "token", "output format: token (just the access token to stdout), json (full response)")
	cmd.Flags().BoolVar(&btrPrivileged, "privileged", false, "mark as a privileged service-bridge identity that bypasses per-bead ownership for system-managed graph writes (ka-0000001946); use only for bridges that write produces/triggered-by deps + mr_url back-fill (e.g. gitlab-bridge)")
}

func init() {
	registerBridgeTokenRegisterFlags(bridgeTokenRegisterCmd)
	bridgeTokenCmd.AddCommand(bridgeTokenRegisterCmd)
	bridgeCmd.AddCommand(bridgeTokenCmd)
}

func runBridgeTokenRegister(cmd *cobra.Command, args []string) error {
	return runBridgeTokenRegisterWith(cmd, newAdminBridgeTokenClient)
}

// runBridgeTokenRegisterWith is the test-injectable form of the command. The
// httpClientFn provides an *client.HTTPClient configured to hit the daemon;
// tests inject one wired to httptest.NewServer.
func runBridgeTokenRegisterWith(cmd *cobra.Command, httpClientFn func() (*client.HTTPClient, error)) error {
	if btrBridge == "" {
		return fmt.Errorf("--bridge is required")
	}
	if btrTTL != "" && btrExpiresAt != "" {
		return fmt.Errorf("--ttl and --expires-at are mutually exclusive")
	}
	if btrRefreshTTL != "" && !btrWithRefresh {
		return fmt.Errorf("--refresh-ttl requires --with-refresh")
	}
	switch btrOutput {
	case "token", "json":
	default:
		return fmt.Errorf("--output must be 'token' or 'json' (got %q)", btrOutput)
	}

	token := btrToken
	if token == "" {
		var err error
		token, err = generateHexToken(32)
		if err != nil {
			return fmt.Errorf("generating token: %w", err)
		}
	}

	c, err := httpClientFn()
	if err != nil {
		return err
	}
	defer c.Close()

	// Wire field stays `fleet` during the compat window — the daemon's
	// /v1/bridge-tokens handler dual-accepts {"bridge"} and {"fleet"}, and
	// the bead.fleet audit stamp is unchanged (invariant: noun-only rename).
	req := &client.RegisterFleetTokenRequest{
		Token:        token,
		Fleet:        btrBridge,
		Project:      btrProject,
		AllowedTypes: btrAllowedTypes,
		AllowedOps:   btrAllowedOps,
		TTL:          btrTTL,
		ExpiresAt:    btrExpiresAt,
		Subject:      btrSubject,
		WithRefresh:  btrWithRefresh,
		RefreshTTL:   btrRefreshTTL,
		Privileged:   btrPrivileged,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.RegisterFleetTokenFull(ctx, req)
	if err != nil {
		return fmt.Errorf("registering bridge token: %w", err)
	}

	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	switch btrOutput {
	case "token":
		fmt.Fprintln(out, token)
		if resp.RefreshToken != "" {
			fmt.Fprintf(errOut, "refresh_token: %s\n", resp.RefreshToken)
			if resp.RefreshTokenExpiresAt != "" {
				fmt.Fprintf(errOut, "refresh_token_expires_at: %s\n", resp.RefreshTokenExpiresAt)
			}
		}
	case "json":
		// Include the access token alongside the server response so
		// `--output json` is the single source of truth for scripts.
		full := struct {
			Token string `json:"token"`
			*client.RegisterFleetTokenResponse
		}{
			Token:                      token,
			RegisterFleetTokenResponse: resp,
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(full); err != nil {
			return fmt.Errorf("encoding response: %w", err)
		}
	}
	return nil
}

// newAdminBridgeTokenClient builds a fresh HTTPClient using the resolved
// admin token and the standard --http-url / env-var precedence chain. We
// don't reuse the global `beadsClient` because bridge-token management is
// admin-only and HTTP-only; this also keeps the command independent of the
// PersistentPreRun transport selection (gRPC would have no bridge-token
// surface to call).
func newAdminBridgeTokenClient() (*client.HTTPClient, error) {
	token := tokenFlag
	if token == "" {
		token = defaultToken()
	}
	if token == "" {
		token = activeRemoteToken()
	}
	if token == "" {
		return nil, fmt.Errorf("no admin token resolved (set GARDENIO_TOKEN / BEADS_AUTH_TOKEN / BEADS_DAEMON_TOKEN or pass --token)")
	}
	base := httpURL
	if base == "" {
		base = defaultHTTPURL()
	}
	return client.NewHTTPClient(strings.TrimRight(base, "/"), token), nil
}

// generateHexToken returns 2*n hex characters (n random bytes). Matches the
// `openssl rand -hex n` shape used in the 2026-05-24 bridge rollout.
func generateHexToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
