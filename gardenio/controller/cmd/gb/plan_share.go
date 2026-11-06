// Command `gb plan share` mints, lists and revokes windows (Lands: the Window,
// PW.4 gd-tJe0GrtFDw; narrative PW-D7): a read-only public URL,
// https://<host>/w/gw_…, on which anyone holding the link can watch a plan with
// no account. kbeads admits a mint, list or revoke only from the owner of the
// plan's land or an operator, on their own sign-in (gb login): an agent pod's
// token is refused, by design. The cockpit's Share panel on /plans/{id} calls
// the same daemon routes.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var (
	planShareExpires string
	planShareLand    string
	planSharePlan    string
)

// planShareGrants is the plain sentence about what a window link grants,
// printed with every mint (and shown by the cockpit's Share panel).
const planShareGrants = "Anyone who has this link can read the plan: its title, state, steps, decisions, crew and narrative, with no account, until it expires or is revoked. Chat apps that unfurl links (Slack, Discord, iMessage) will fetch the page too."

var planShareCmd = &cobra.Command{
	Use:   "share <plan-epic-id>",
	Short: "Mint a read-only public window on a plan (list, revoke)",
	Long: `Mint a window on the plan epic <plan-epic-id>: a read-only public URL,
https://<host>/w/gw_…, on which anyone holding the link can watch the plan
(title, state, phases and steps, what waits on the operator, the crew, a Recent
strip and the narrative) with no account. The page refreshes itself every 20
seconds. Nothing on it reaches into the plan.

The URL is printed once: the daemon keeps only a hash of its token. A window
expires after --expires (default 30d, at most 365d) and can be revoked at any
time with 'gb plan share revoke <window-id>'; 'gb plan share list' shows each
window's views, expiry and state.

Only the owner of the plan's land or an operator may share, on their own
sign-in (gb login): an agent pod's token is refused.`,
	Example: `  gb plan share gd-pz87vf3xsB
  gb plan share gd-pz87vf3xsB --expires 7d
  gb plan share list --land gardenio
  gb plan share list --plan gd-pz87vf3xsB
  gb plan share revoke gd-AbCdEf1234`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanShare,
}

var planShareListCmd = &cobra.Command{
	Use:   "list",
	Short: "List a land's windows with their views, expiry and state",
	Long: `List the windows of a land, newest first: live, expired and revoked, each
with its plan, views, when it was last viewed, when it expires and who made it.
Name the land with --land, or a plan with --plan (its land is read from the
plan, and only that plan's windows are listed). A window on the whole land (gb
lands share) shows "(whole land)" for its plan. The token is never shown again.`,
	Args: cobra.NoArgs,
	RunE: runPlanShareList,
}

var planShareRevokeCmd = &cobra.Command{
	Use:   "revoke <window-id>",
	Short: "Revoke a window: its link stops opening on the next request",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlanShareRevoke,
}

func init() {
	planShareCmd.Flags().StringVar(&planShareExpires, "expires", "30d", "how long the window opens: <n>d, <n>w or <n>y (at most 365d)")
	planShareListCmd.Flags().StringVar(&planShareLand, "land", "", "the land whose windows to list")
	planShareListCmd.Flags().StringVar(&planSharePlan, "plan", "", "list only this plan's windows (its land is read from the plan)")
	planShareCmd.AddCommand(planShareListCmd, planShareRevokeCmd)
	planCmd.AddCommand(planShareCmd)
	for _, c := range []*cobra.Command{planShareCmd, planShareListCmd, planShareRevokeCmd} {
		c.SilenceUsage = true
	}
}

// parseShareExpiry reads --expires as whole days: <n>, <n>d, <n>w or <n>y.
func parseShareExpiry(s string) (int, error) {
	in := s
	s = strings.ToLower(strings.TrimSpace(s))
	unit := 1
	switch {
	case strings.HasSuffix(s, "d"):
		s = strings.TrimSuffix(s, "d")
	case strings.HasSuffix(s, "w"):
		s, unit = strings.TrimSuffix(s, "w"), 7
	case strings.HasSuffix(s, "y"):
		s, unit = strings.TrimSuffix(s, "y"), 365
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n*unit > 365 {
		return 0, fmt.Errorf("--expires %q: want <n>d, <n>w or <n>y, from 1d to 365d", in)
	}
	return n * unit, nil
}

// windowURL is the absolute URL of a window path (/w/<token>). Windows are
// served at the root of the viewer's public host: the host of
// $BEADS_VIEWER_URL when set, else the default gardenio host.
func windowURL(path string) string {
	if u, err := url.Parse(strings.TrimSpace(os.Getenv("BEADS_VIEWER_URL"))); err == nil && u.Host != "" {
		scheme := u.Scheme
		if scheme == "" {
			scheme = "https"
		}
		return scheme + "://" + u.Host + path
	}
	return "https://" + resolveGardenioHost() + path
}

// shareError explains a refusal: the routes take a person's own sign-in.
func shareError(err error) error {
	var apiErr *beadsapi.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusUnauthorized) {
		return fmt.Errorf("%w\nwindows are managed on the land owner's or an operator's own sign-in: run gb login (an agent token is refused)", err)
	}
	return err
}

func runPlanShare(cmd *cobra.Command, args []string) error {
	planID := strings.TrimSpace(args[0])
	days, err := parseShareExpiry(planShareExpires)
	if err != nil {
		return err
	}
	minted, err := daemon.CreateWindow(cmd.Context(), planID, days)
	if err != nil {
		return shareError(err)
	}
	link := windowURL(minted.Path)
	out := cmd.OutOrStdout()
	if jsonOutput {
		return writeShareJSON(out, map[string]any{"window": minted.Window, "url": link})
	}
	w := minted.Window
	fmt.Fprintf(out, "Window %s on %s (land %s), expires %s.\n\n  %s\n\n",
		w.ID, w.PlanID, w.Land, w.ExpiresAt.Local().Format("2006-01-02 15:04"), link)
	fmt.Fprintf(out, "This link is shown once: copy it now. %s\nRevoke it with: gb plan share revoke %s\n", planShareGrants, w.ID)
	return nil
}

func runPlanShareList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	land, planID := strings.TrimSpace(planShareLand), strings.TrimSpace(planSharePlan)
	if land == "" && planID == "" {
		return errors.New("name the land (--land <land>) or a plan (--plan <plan-epic-id>)")
	}
	if land == "" {
		plan, err := daemon.GetBead(ctx, planID)
		if err != nil {
			return err
		}
		lands := projectLabels(plan.Labels)
		if len(lands) != 1 {
			return fmt.Errorf("plan %s needs exactly one project label to say whose land it is in", planID)
		}
		land = strings.TrimPrefix(lands[0], "project:")
	}
	windows, err := daemon.ListWindows(ctx, land)
	if err != nil {
		return shareError(err)
	}
	if planID != "" {
		kept := windows[:0]
		for _, w := range windows {
			if w.PlanID == planID {
				kept = append(kept, w)
			}
		}
		windows = kept
	}
	out := cmd.OutOrStdout()
	if jsonOutput {
		return writeShareJSON(out, windows)
	}
	if len(windows) == 0 {
		fmt.Fprintf(out, "No windows in land %s.\n", land)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WINDOW\tPLAN\tSTATE\tVIEWS\tLAST VIEWED\tEXPIRES\tCREATED BY")
	for _, w := range windows {
		last := "never"
		if w.LastViewedAt != nil {
			last = w.LastViewedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", w.ID, windowTarget(w), w.State, w.ViewCount, last,
			w.ExpiresAt.Local().Format("2006-01-02"), w.CreatedBy)
	}
	return tw.Flush()
}

func runPlanShareRevoke(cmd *cobra.Command, args []string) error {
	w, err := daemon.RevokeWindow(cmd.Context(), strings.TrimSpace(args[0]))
	if err != nil {
		return shareError(err)
	}
	out := cmd.OutOrStdout()
	if jsonOutput {
		return writeShareJSON(out, w)
	}
	at := time.Now()
	if w.RevokedAt != nil {
		at = *w.RevokedAt
	}
	fmt.Fprintf(out, "Revoked window %s on %s at %s: its link no longer opens (%d views).\n",
		w.ID, windowTarget(*w), at.Local().Format("2006-01-02 15:04"), w.ViewCount)
	return nil
}

// windowTarget names what a window shows: its plan, or "(whole land)" for a
// land window (gb lands share).
func windowTarget(w beadsapi.Window) string {
	if w.TargetKind == "land" {
		return "(whole land)"
	}
	return w.PlanID
}

func writeShareJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// projectLabels returns the non-empty project:* labels in all, sorted.
func projectLabels(all []string) []string {
	var out []string
	for _, l := range all {
		if strings.HasPrefix(l, "project:") && l != "project:" {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}
