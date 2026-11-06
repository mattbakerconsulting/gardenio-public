package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"gardenio/clicreds/oidclogin"

	"github.com/spf13/cobra"
)

// kd login: sign in to Keycloak with the OAuth 2.0 device authorization grant
// (RFC 8628) so a human can make admin-gated kbeads writes from a terminal.
// The grant, the session cache and its refresh live in
// gardenio/clicreds/oidclogin, shared with `gb login`: one sign-in serves both
// CLIs. This file is the kd command surface over it.
//
// The client is a PUBLIC Keycloak client (default "gardenio-cli") that has the
// device grant only; it is declared in deploy-values (keycloak.clients).

const (
	defaultLoginIssuer   = oidclogin.DefaultIssuer
	defaultLoginClientID = oidclogin.DefaultClientID
	loginHTTPTimeout     = oidclogin.HTTPTimeout
)

var (
	loginIssuer   string
	loginClientID string
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Sign in with Keycloak (device grant) for admin-gated writes",
	Long: `Sign in to Keycloak with the OAuth device authorization grant.

kd prints a verification URL and a code. Open the URL in your own browser,
enter the code and sign in; no password passes through kd. The access token
(with your realm roles, e.g. gardenio-admin) is cached 0600 in
~/.local/state/beads/oidc-login.json and refreshed automatically. The session
survives a Keycloak restart: while Keycloak is unreachable kd keeps the cached
session, says so, and refreshes it on the next run; only a refresh Keycloak
refuses (the session idled out, was revoked, or hit its 7-day lifetime) needs a
new 'kd login'. A request the daemon or the edge answers 401 is retried once
with a freshly refreshed token.

While signed in, kd sends that token instead of GARDENIO_TOKEN / BEADS_*_TOKEN
(an explicit --token still wins). Run 'kd logout' to forget it. 'gb login'
writes the same session, so one sign-in serves kd and gb.

Only enter a code you started yourself. A process on any machine or agent pod
that can run kd can start a sign-in and ask you to confirm its code; whoever
holds the code's token acts as you. The session is short-lived, and the daemon
refuses escalation approval from a 'kd login' session (approve in the web UI
or Slack).

The issuer and client default to the gardenio realm and the public
"gardenio-cli" client; override with --issuer / --client-id or
GARDENIO_OIDC_ISSUER / GARDENIO_OIDC_CLIENT_ID.`,
	GroupID: "system",
	Args:    cobra.NoArgs,
	// Login is a local + Keycloak operation: skip the daemon client setup (and
	// token resolution) the root hook performs.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error { cmd.SilenceUsage = true; return nil },
	RunE: func(cmd *cobra.Command, args []string) error {
		hc := &http.Client{Timeout: loginHTTPTimeout}
		// A land door URL (…/land/<name>) signs in for that land alone.
		land := oidclogin.LandFromURL(httpURL)
		sess, err := runDeviceLoginForLand(cmd.Context(), hc, loginIssuer, loginClientID, land, cmd.ErrOrStderr(), time.Sleep)
		if err != nil {
			return err
		}
		sess.Daemon = oidclogin.DaemonHost(httpURL)
		if sess.Daemon == "" {
			return fmt.Errorf("cannot bind the login session: no daemon host in --http-url %q", httpURL)
		}
		if err := oidclogin.Save(sess); err != nil {
			return fmt.Errorf("signed in, but could not cache the session: %w", err)
		}
		if land != "" {
			fmt.Fprintln(cmd.OutOrStdout(), "Signed in"+oidclogin.DescribeIdentity(sess.AccessToken)+" for land "+land+".")
			return nil
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Signed in"+oidclogin.DescribeIdentity(sess.AccessToken)+".")
		return nil
	},
}

var logoutCmd = &cobra.Command{
	Use:               "logout",
	Short:             "Forget the Keycloak session cached by 'kd login'",
	GroupID:           "system",
	Args:              cobra.NoArgs,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error { cmd.SilenceUsage = true; return nil },
	RunE: func(cmd *cobra.Command, args []string) error {
		removed, err := oidclogin.RemoveFor(oidclogin.LandFromURL(httpURL))
		if err != nil {
			return err
		}
		if !removed {
			fmt.Fprintln(cmd.OutOrStdout(), "Not signed in.")
			return nil
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Signed out.")
		return nil
	},
}

func init() {
	loginCmd.Flags().StringVar(&loginIssuer, "issuer", envOr("GARDENIO_OIDC_ISSUER", defaultLoginIssuer), "Keycloak realm issuer URL (env: GARDENIO_OIDC_ISSUER)")
	loginCmd.Flags().StringVar(&loginClientID, "client-id", envOr("GARDENIO_OIDC_CLIENT_ID", defaultLoginClientID), "public Keycloak client with the device grant enabled (env: GARDENIO_OIDC_CLIENT_ID)")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// runDeviceLoginForLand drives the device grant end to end and returns the
// session. land asks for a token minted for one land (scope land-<land>); ""
// is an ordinary sign-in. Prompts go to w; sleep is injected so tests do not
// wait out the poll interval.
func runDeviceLoginForLand(ctx context.Context, hc *http.Client, issuer, clientID, land string, w io.Writer, sleep func(time.Duration)) (*oidclogin.Session, error) {
	return oidclogin.DeviceLoginForLand(ctx, hc, issuer, clientID, land, oidclogin.Prompt{CLI: "kd", Out: w, Sleep: sleep})
}

// loginBearer returns the access token cached by `kd login` (or `gb login`)
// for the daemon at target, refreshing it when needed; "" means fall back to
// the other credentials. See oidclogin.Bearer.
func loginBearer(ctx context.Context, hc *http.Client, now time.Time, target string, warn io.Writer) string {
	return oidclogin.Bearer(ctx, hc, now, target, "kd", warn)
}

// loginSessionRefresher is the 401 retry hook kd installs on its HTTP client
// when the bearer came from the cached session. See oidclogin.Refresher.
func loginSessionRefresher(hc *http.Client, target string, warn io.Writer) func(ctx context.Context) (string, error) {
	return oidclogin.Refresher(hc, target, "kd", warn)
}
