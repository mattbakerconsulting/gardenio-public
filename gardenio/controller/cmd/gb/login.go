package main

// gb login: sign in to Keycloak with the OAuth 2.0 device authorization grant
// (RFC 8628), the same grant and session cache `kd login` uses
// (gardenio/clicreds/oidclogin, gd-bFFquoScHy). One sign-in serves both CLIs:
// the session is written to ~/.local/state/beads/oidc-login.json, bound to the
// host of gb's --http-url, and sent as the Bearer token to that daemon only.
//
// This replaced the beads-viewer cookie handoff (/auth/cli-handoff +
// /auth/cli-exchange), which copied the browser's session cookies into the CLI
// (gd-5FThTGbOOP). The viewer now answers those paths 410 Gone with an
// "upgrade gb" message, so an old binary fails with a clear instruction.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"gardenio/clicreds/credstore"
	"gardenio/clicreds/oidclogin"

	"github.com/spf13/cobra"
)

var (
	loginIssuer    string
	loginClientID  string
	loginStatus    bool
	loginNoBrowser bool
	loginTimeout   time.Duration

	// Retired with the cookie handoff. Still parsed so existing scripts and
	// aliases keep working; cobra prints a deprecation note when they are set.
	loginViewerURL      string
	loginCLIExchangeURL string
	loginContext        string
)

// loginPollSleep waits out the device grant's poll interval; a var so tests
// do not.
var loginPollSleep = time.Sleep

var loginCmd = &cobra.Command{
	Use:     "login",
	Short:   "Sign in with Keycloak (device grant), shared with kd login",
	GroupID: "session",
	Long: `Sign in to Keycloak with the OAuth device authorization grant.

gb prints a verification URL and a code and opens the URL in your browser
(--no-browser to only print it). Enter the code there and sign in; no
password passes through gb. The access token (with your realm roles) is
cached 0600 in ~/.local/state/beads/oidc-login.json, bound to the host of
--http-url, and refreshed automatically. 'kd login' writes the same session,
so one sign-in serves gb and kd.

While signed in, gb sends that token to the bound daemon instead of
GARDENIO_TOKEN / BEADS_*_TOKEN (an explicit --token still wins). Run
'gb logout' to forget it.

Only enter a code you started yourself. A process on any machine or agent pod
that can run gb can start a sign-in and ask you to confirm its code; whoever
holds the code's token acts as you.

The issuer and client default to the gardenio realm and the public
"gardenio-cli" client; override with --issuer / --client-id or
GARDENIO_OIDC_ISSUER / GARDENIO_OIDC_CLIENT_ID.

Examples:
  gb login                              # sign in for the default daemon
  gb login --http-url https://api.example/kd
  gb login --no-browser                 # print the link instead of opening it
  gb login --status                     # show the current session`,
	Args: cobra.NoArgs,
	// Login is a local + Keycloak operation: skip the daemon client setup (and
	// the bearer resolution, with its "session expired" hints) the root hook
	// performs.
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { cmd.SilenceUsage = true; return nil },
	RunE:              runLogin,
}

func init() {
	loginCmd.Flags().StringVar(&loginIssuer, "issuer", envOr("GARDENIO_OIDC_ISSUER", oidclogin.DefaultIssuer),
		"Keycloak realm issuer URL (env: GARDENIO_OIDC_ISSUER)")
	loginCmd.Flags().StringVar(&loginClientID, "client-id", envOr("GARDENIO_OIDC_CLIENT_ID", oidclogin.DefaultClientID),
		"public Keycloak client with the device grant enabled (env: GARDENIO_OIDC_CLIENT_ID)")
	loginCmd.Flags().BoolVar(&loginStatus, "status", false, "show current authentication status")
	loginCmd.Flags().BoolVar(&loginNoBrowser, "no-browser", false,
		"print the sign-in link instead of opening a browser (headless environments)")
	loginCmd.Flags().DurationVar(&loginTimeout, "timeout", 10*time.Minute,
		"overall timeout for the sign-in (the code itself expires sooner if Keycloak says so)")

	const retired = "gb login no longer uses the beads-viewer cookie handoff; it signs in with Keycloak for --http-url"
	loginCmd.Flags().StringVar(&loginViewerURL, "viewer-url", "", "retired")
	loginCmd.Flags().StringVar(&loginCLIExchangeURL, "cli-exchange-url", "", "retired")
	loginCmd.Flags().StringVar(&loginContext, "context", "", "retired")
	for _, f := range []string{"viewer-url", "cli-exchange-url", "context"} {
		_ = loginCmd.Flags().MarkDeprecated(f, retired)
	}
}

func runLogin(cmd *cobra.Command, _ []string) error {
	if loginStatus {
		return showLoginStatus(cmd.OutOrStdout())
	}
	daemon := oidclogin.DaemonHost(httpURL)
	if daemon == "" {
		return fmt.Errorf("cannot bind the login session: no daemon host in --http-url %q", httpURL)
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	p := oidclogin.Prompt{CLI: "gb", Out: cmd.ErrOrStderr(), Sleep: loginPollSleep}
	if !loginNoBrowser {
		p.OpenBrowser = openBrowser
	}
	// A land door URL (…/land/<name>) signs in for that land alone.
	land := oidclogin.LandFromURL(httpURL)
	sess, err := oidclogin.DeviceLoginForLand(ctx, &http.Client{Timeout: oidclogin.HTTPTimeout}, loginIssuer, loginClientID, land, p)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("login timed out after %s", loginTimeout)
		}
		return err
	}
	sess.Daemon = daemon
	if err := oidclogin.Save(sess); err != nil {
		return fmt.Errorf("signed in, but could not cache the session: %w", err)
	}
	if land != "" {
		daemon += " (land " + land + ")"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Signed in%s for %s.\n", oidclogin.DescribeIdentity(sess.AccessToken), daemon)
	return nil
}

// loginBearer returns the access token cached by `gb login` (or `kd login`)
// for the daemon at target, refreshing it when needed; "" means fall back to
// the other credentials. See oidclogin.Bearer.
func loginBearer(ctx context.Context, hc *http.Client, now time.Time, target string, warn io.Writer) string {
	return oidclogin.Bearer(ctx, hc, now, target, "gb", warn)
}

func showLoginStatus(w io.Writer) error {
	sess, err := oidclogin.LoadFor(oidclogin.LandFromURL(httpURL))
	if err != nil {
		return fmt.Errorf("reading login session: %w", err)
	}
	if sess != nil {
		fmt.Fprintf(w, "Session:    signed in%s\n", oidclogin.DescribeIdentity(sess.AccessToken))
		fmt.Fprintf(w, "Daemon:     %s\n", sess.Daemon)
		if want := oidclogin.DaemonHost(httpURL); want != sess.Daemon {
			fmt.Fprintf(w, "            (not the current --http-url host %q; run 'gb login' to sign in for it)\n", want)
		}
		fmt.Fprintf(w, "Issuer:     %s\n", sess.Issuer)
		if sess.Fresh(time.Now()) {
			fmt.Fprintf(w, "Token:      valid (expires in %s)\n", time.Until(sess.Expiry).Truncate(time.Second))
		} else {
			fmt.Fprintln(w, "Token:      expired (refreshed on next use)")
		}
	}

	// A credstore context is what older gb logins (cookie handoff, GitLab
	// OAuth) left behind. Its bearer is still read as a fallback credential;
	// a cookie from the retired handoff is not.
	var cur *credstore.Context
	var store *credstore.Store
	if s, err := credstore.Load(""); err == nil {
		store, cur = s, s.Current()
	}
	if sess == nil && cur == nil {
		fmt.Fprintln(w, "Not logged in. Run 'gb login' to authenticate.")
		return nil
	}
	if cur != nil {
		fmt.Fprintf(w, "Context:    %s (stored by an older gb login)\n", store.CurrentContext())
		fmt.Fprintf(w, "Server:     %s\n", cur.Server)
		if cur.HasBearer() {
			fmt.Fprintln(w, "Auth:       OAuth bearer")
		} else {
			fmt.Fprintln(w, "Auth:       (none, ignored; gb logout clears it)")
		}
		if cur.User != nil {
			fmt.Fprintf(w, "User:       %s (%s)\n", cur.User.Username, cur.User.Email)
		}
	}
	if op := defaultOperatedBy(); op != "" {
		source := "credstore"
		if os.Getenv("BEADS_OPERATED_BY") != "" {
			source = "$BEADS_OPERATED_BY"
		}
		fmt.Fprintf(w, "Operator:   %s (from %s)\n", op, source)
	}
	if store != nil {
		if names := store.ContextNames(); len(names) > 1 {
			fmt.Fprintf(w, "Contexts:   %s\n", strings.Join(names, ", "))
		}
	}
	return nil
}

// openBrowserHook is the indirection point that tests substitute so the
// sign-in never shells out to xdg-open / open / rundll32.
var openBrowserHook = openBrowserNative

func openBrowser(rawURL string) error { return openBrowserHook(rawURL) }

func openBrowserNative(rawURL string) error {
	switch runtime.GOOS {
	case "linux":
		return exec.Command("xdg-open", rawURL).Start()
	case "darwin":
		return exec.Command("open", rawURL).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}
}
