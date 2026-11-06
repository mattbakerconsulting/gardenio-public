package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gardenio/clicreds/oidclogin"
	"gardenio/lands/contract/landsv1"

	"github.com/spf13/cobra"
)

// loginHint is the command that signs in again for land.
func (a *app) loginHint(land string) string {
	if a.asTenant != "" {
		return "gl --as-tenant " + land + " login"
	}
	return "gl login --land " + land
}

// signInError means only a new sign-in helps.
type signInError struct{ msg string }

func (e *signInError) Error() string { return e.msg }

// bearer is the access token for land, renewed first when it is about to
// lapse (or always, with force: the land refused it). The session must have
// been minted for this land at this edge; a session for anything else is
// never sent.
func (a *app) bearer(ctx context.Context, edge, land string, force bool) (string, error) {
	s, err := oidclogin.LoadFor(land)
	if err != nil {
		return "", fmt.Errorf("reading your sign-in for land %s: %w", land, err)
	}
	if s == nil || s.Land != land {
		return "", &signInError{fmt.Sprintf("you are not signed in to land %s: run '%s'", land, a.loginHint(land))}
	}
	if host := oidclogin.DaemonHost(edge); s.Daemon == "" || s.Daemon != host {
		return "", &signInError{fmt.Sprintf("your sign-in for land %s is for %s, not %s: run '%s'", land, s.Daemon, host, a.loginHint(land))}
	}
	now := a.now()
	if !force && s.Fresh(now) {
		return s.AccessToken, nil
	}
	ended := &signInError{fmt.Sprintf("your sign-in for land %s has ended: run '%s'", land, a.loginHint(land))}
	if s.RefreshToken == "" {
		return "", ended
	}
	err = oidclogin.Refresh(ctx, a.hc, s, now)
	switch {
	case err == nil:
		if err := oidclogin.Save(s); err != nil {
			fmt.Fprintf(a.errOut, "gl: could not keep the renewed sign-in (%v); the next command renews it again\n", err)
		}
		return s.AccessToken, nil
	case oidclogin.RefreshRefused(err):
		return "", ended
	case !force && now.Before(s.Expiry):
		// The sign-in service is unreachable but the token has not lapsed.
		return s.AccessToken, nil
	default:
		return "", fmt.Errorf("could not reach the sign-in service to renew your sign-in for land %s (%v); try again in a moment", land, err)
	}
}

// banner tells a support operator, once per command, that they are looking
// through a tenant's eyes and that anything they change carries their own
// name.
func (a *app) banner(land string) {
	if a.asTenant == "" || a.bannerDone {
		return
	}
	a.bannerDone = true
	who := ""
	if s, err := oidclogin.LoadFor(land); err == nil && s != nil {
		who = oidclogin.DescribeIdentity(s.AccessToken)
	}
	fmt.Fprintf(a.errOut, "gl: support view of land %s%s. You see what its members see; anything you change is recorded under your own sign-in.\n", land, who)
}

func newLoginCmd(a *app) *cobra.Command {
	var noBrowser bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to a land (in your browser, with a one-time code)",
		Long: `Sign in to a land. gl prints a link and a short code; you finish signing in
in your own browser, so no password passes through gl. The sign-in is for
this one land only. The first land you sign in to becomes the default.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			land, err := a.land()
			if err != nil {
				return err
			}
			edge, err := a.edge()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			p := oidclogin.Prompt{CLI: "gl", Out: a.errOut, Sleep: a.sleep}
			if !noBrowser {
				p.OpenBrowser = a.openBrowser
			}
			sess, err := oidclogin.DeviceLoginForLand(ctx, a.hc, a.issuer, a.clientID, land, p)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return fmt.Errorf("sign-in timed out after %s", timeout)
				}
				return err
			}
			sess.Daemon = oidclogin.DaemonHost(edge)
			if err := oidclogin.Save(sess); err != nil {
				return fmt.Errorf("signed in, but could not keep the sign-in: %w", err)
			}
			if a.asTenant == "" {
				c, err := a.config()
				if err != nil {
					return err
				}
				if c.Land == "" {
					c.Land = land
					if err := a.saveConfig(c); err != nil {
						return err
					}
				}
			}
			fmt.Fprintf(a.out, "Signed in%s to land %s.\n", oidclogin.DescribeIdentity(sess.AccessToken), land)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the link without opening a browser")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "give up waiting for the browser after this long")
	return cmd
}

func newLogoutCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget your sign-in for the land",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			land, err := a.land()
			if err != nil {
				return err
			}
			removed, err := oidclogin.RemoveFor(land)
			if err != nil {
				return err
			}
			if removed {
				fmt.Fprintf(a.out, "Signed out of land %s.\n", land)
			} else {
				fmt.Fprintf(a.out, "You were not signed in to land %s.\n", land)
			}
			return nil
		},
	}
}

func newUseCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "use <land>",
		Short: "Make a land the default for later commands",
		Long: `Make a land the default for later commands. With --edge or --web, remember
those addresses too.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			land := args[0]
			if !landName.MatchString(land) {
				return fmt.Errorf("%q is not a land name (lower-case letters, digits and dashes)", land)
			}
			c, err := a.config()
			if err != nil {
				return err
			}
			c.Land = land
			if a.edgeFlag != "" {
				edge, err := checkEdge(a.edgeFlag)
				if err != nil {
					return err
				}
				c.Edge = edge
			}
			if a.webFlag != "" {
				web, err := checkEdge(a.webFlag)
				if err != nil {
					return err
				}
				c.Web = web
			}
			if err := a.saveConfig(c); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Using land %s.\n", land)
			if s, err := oidclogin.LoadFor(land); err == nil && s == nil {
				fmt.Fprintf(a.out, "You are not signed in to it yet: run 'gl login'.\n")
			}
			return nil
		},
	}
}

// whoamiView is gl whoami --json.
type whoamiView struct {
	Land            string          `json:"land"`
	Member          *landsv1.Member `json:"member"`
	ContractClient  string          `json:"contract_client"`
	ContractLand    string          `json:"contract_land,omitempty"`
	SupportAsTenant bool            `json:"support_as_tenant,omitempty"`
}

func newWhoamiCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Who you are on the land",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.WhoamiWithResponse(cmd.Context(), l.land)
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			v := whoamiView{
				Land:            l.land,
				Member:          r.JSON200,
				ContractClient:  landsv1.ContractVersion,
				ContractLand:    r.HTTPResponse.Header.Get(landsv1.HeaderContract),
				SupportAsTenant: a.asTenant != "",
			}
			return a.emit(v, func(w io.Writer) {
				m := r.JSON200
				fmt.Fprintf(w, "Land:      %s\n", l.land)
				fmt.Fprintf(w, "You:       %s (%s, %s)\n", m.Name, m.Role, m.Status)
				fmt.Fprintf(w, "Member id: %s\n", m.Id)
				if s, err := oidclogin.LoadFor(l.land); err == nil && s != nil {
					if who := strings.TrimPrefix(oidclogin.DescribeIdentity(s.AccessToken), " as "); who != "" {
						fmt.Fprintf(w, "Signed in: %s\n", who)
					}
				}
				land := v.ContractLand
				if land == "" {
					land = "not stated"
				}
				fmt.Fprintf(w, "Contract:  gl %s, land %s\n", v.ContractClient, land)
				if v.SupportAsTenant {
					fmt.Fprintln(w, "Support:   viewing as a tenant")
				}
			})
		},
	}
}

func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "The contract version gl speaks",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintf(a.out, "gl, Lands contract %s\n", landsv1.ContractVersion)
			return nil
		},
	}
}
