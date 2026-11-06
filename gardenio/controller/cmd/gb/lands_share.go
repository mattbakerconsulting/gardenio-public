// Command `gb lands share` mints, lists, previews and revokes land windows
// (Lands: the land window, plan gd-4iVYF9tGsn, step LW.5 gd-aUQRspcbeS): a
// read-only public URL, https://<host>/w/gw_…, on which anyone holding the
// link watches a whole land's World with no account. It is gb plan share's
// land arm, on the same daemon routes (POST /v1/windows with a land, GET
// /v1/windows, DELETE /v1/windows/{id}, GET /v1/windows/preview?land=), with
// the same gate: only the land's owner or an operator, on their own sign-in
// (gb login). The land's Share panel in /garden calls the same routes.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planwindow"

	"github.com/spf13/cobra"
)

var landsShareExpires string

var landsShareCmd = &cobra.Command{
	Use:   "share <land>",
	Short: "Mint a read-only public window on a whole land (list, preview, revoke)",
	Long: `Mint a window on the land <land>: a read-only public URL,
https://<host>/w/gw_…, on which anyone holding the link watches the land's
World (its plans as crops, the crew at work, what waits on the operator) with
no account. They can pan, zoom and tap a plan for its read-only card; nothing
on the page can be pressed. It refreshes itself every 20 seconds.

Every open plan's title becomes visible, so look first: 'gb lands share
preview <land>' lists exactly what the page would show and links the page
itself, and mints nothing.

The URL is printed once: the daemon keeps only a hash of its token. A window
expires after --expires (default 30d, at most 365d) and can be revoked at any
time with 'gb lands share revoke <window-id>'; 'gb lands share list <land>'
shows each window's views, expiry and state.

Only the land's owner or an operator may share, on their own sign-in (gb
login): an agent pod's token is refused.`,
	Example: `  gb lands share preview gardenio
  gb lands share gardenio
  gb lands share gardenio --expires 7d
  gb lands share list gardenio
  gb lands share revoke gd-AbCdEf1234`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsShare,
}

var landsShareListCmd = &cobra.Command{
	Use:   "list <land>",
	Short: "List a land's land windows with their views, expiry and state",
	Long: `List the windows on the whole of <land>, newest first: live, expired and
revoked, each with its views, when it was last viewed, when it expires and who
made it. The token is never shown again. Windows on one plan are listed by gb
plan share list.`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsShareList,
}

var landsShareRevokeCmd = &cobra.Command{
	Use:   "revoke <window-id>",
	Short: "Revoke a window: its link stops opening on the next request",
	Args:  cobra.ExactArgs(1),
	RunE:  runLandsShareRevoke,
}

var landsSharePreviewCmd = &cobra.Command{
	Use:   "preview <land>",
	Short: "Show what a window on the land would show, without minting one",
	Long: `Show what a window minted now on <land> would show a visitor: the
land's plans as the page lists them (state, current phase, progress, what
waits, crew at work) and every field the window withholds because it
contains an internal reference. It also prints the link to the page itself in
the viewer, the exact World a visitor would see, which opens on your sign-in.
Nothing is minted.`,
	Args: cobra.ExactArgs(1),
	RunE: runLandsSharePreview,
}

func init() {
	for _, c := range []*cobra.Command{landsShareCmd, landsSharePreviewCmd} {
		c.Flags().StringVar(&landsShareExpires, "expires", "30d", "how long the window opens: <n>d, <n>w or <n>y (at most 365d)")
	}
	landsShareCmd.AddCommand(landsShareListCmd, landsShareRevokeCmd, landsSharePreviewCmd)
	landsCmd.AddCommand(landsShareCmd)
	for _, c := range []*cobra.Command{landsShareCmd, landsShareListCmd, landsShareRevokeCmd, landsSharePreviewCmd} {
		c.SilenceUsage = true
	}
}

func runLandsShare(cmd *cobra.Command, args []string) error {
	land := strings.TrimSpace(args[0])
	days, err := parseShareExpiry(landsShareExpires)
	if err != nil {
		return err
	}
	minted, err := daemon.CreateLandWindow(cmd.Context(), land, days)
	if err != nil {
		return shareError(err)
	}
	link := windowURL(minted.Path)
	out := cmd.OutOrStdout()
	if jsonOutput {
		return writeShareJSON(out, map[string]any{"window": minted.Window, "url": link, "grants": planwindow.LandGrants})
	}
	w := minted.Window
	fmt.Fprintf(out, "Window %s on the whole of land %s, expires %s.\n\n  %s\n\n",
		w.ID, w.Land, w.ExpiresAt.Local().Format("2006-01-02 15:04"), link)
	fmt.Fprintf(out, "This link is shown once: copy it now. %s\nRevoke it with: gb lands share revoke %s\n", planwindow.LandGrants, w.ID)
	return nil
}

// landWindows keeps the land windows of a land's window list.
func landWindows(all []beadsapi.Window) []beadsapi.Window {
	kept := make([]beadsapi.Window, 0, len(all))
	for _, w := range all {
		if w.TargetKind == "land" {
			kept = append(kept, w)
		}
	}
	return kept
}

func runLandsShareList(cmd *cobra.Command, args []string) error {
	land := strings.TrimSpace(args[0])
	all, err := daemon.ListWindows(cmd.Context(), land)
	if err != nil {
		return shareError(err)
	}
	windows := landWindows(all)
	out := cmd.OutOrStdout()
	if jsonOutput {
		return writeShareJSON(out, windows)
	}
	if len(windows) == 0 {
		fmt.Fprintf(out, "No windows on the whole of land %s.\n", land)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WINDOW\tSTATE\tVIEWS\tLAST VIEWED\tEXPIRES\tCREATED BY")
	for _, w := range windows {
		last := "never"
		if w.LastViewedAt != nil {
			last = w.LastViewedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", w.ID, w.State, w.ViewCount, last,
			w.ExpiresAt.Local().Format("2006-01-02"), w.CreatedBy)
	}
	return tw.Flush()
}

func runLandsShareRevoke(cmd *cobra.Command, args []string) error {
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
	what := w.PlanID
	if w.TargetKind == "land" {
		what = "the whole of land " + w.Land
	}
	fmt.Fprintf(out, "Revoked window %s on %s at %s: its link no longer opens (%d views).\n",
		w.ID, what, at.Local().Format("2006-01-02 15:04"), w.ViewCount)
	return nil
}

// landSharePreviewURL is the viewer page that renders the land window a mint
// would make, as a visitor would see it (beads-viewer land_share.go), on the
// viewer under $BEADS_VIEWER_URL (default the gardenio viewer).
func landSharePreviewURL(land string, days int) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("BEADS_VIEWER_URL")), "/")
	if base == "" {
		base = strings.TrimRight(defaultGardenioViewerURL, "/")
	}
	return base + "/p/" + url.PathEscape(land) + "/share/preview?days=" + strconv.Itoa(days)
}

func runLandsSharePreview(cmd *cobra.Command, args []string) error {
	land := strings.TrimSpace(args[0])
	days, err := parseShareExpiry(landsShareExpires)
	if err != nil {
		return err
	}
	raw, err := daemon.PreviewLandWindow(cmd.Context(), land, days)
	if err != nil {
		return shareError(err)
	}
	p, err := planwindow.DecodeLand(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("the daemon's land view did not decode: %w", err)
	}
	v := planwindow.CleanLand(p)
	page := landSharePreviewURL(land, days)
	out := cmd.OutOrStdout()
	if jsonOutput {
		withheld := make([]map[string]string, 0, len(v.Withheld))
		for _, w := range v.Withheld {
			withheld = append(withheld, map[string]string{"path": w.Path, "rule": w.Rule})
		}
		return writeShareJSON(out, map[string]any{"view": v.LandPayload, "withheld": withheld, "page": page, "grants": planwindow.LandGrants})
	}
	return writeLandPreview(out, v, page)
}

// writeLandPreview prints the land as a window's page lists it, then what it
// withholds and where the page itself is.
func writeLandPreview(out io.Writer, v planwindow.LandView, page string) error {
	fmt.Fprintf(out, "A window on land %s would show this (nothing is minted). %s\n\n", v.Land, planwindow.LandGrants)
	if len(v.Plans) == 0 {
		fmt.Fprintln(out, "  No plans: the World would show the land empty.")
	} else {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  PLAN\tSTATE\tNOW\tSTEPS\tWAITING\tCREW AT WORK")
		for _, pl := range v.Plans {
			title := pl.Title
			if title == "" {
				title = pl.ShortTitle
			}
			state := planwindow.StateLabel(pl.State)
			if pl.Status == "closed" {
				state = "Finished · " + state
			}
			now := ""
			for _, ph := range pl.Phases {
				if pl.CurrentPhase > 0 && ph.Number == pl.CurrentPhase {
					now = ph.Title
				}
			}
			live := 0
			for _, a := range pl.Crew {
				if a.Live {
					live++
				}
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d of %d\t%d\t%d\n", title, state, now, pl.StepsDone, pl.StepsTotal,
				pl.OpenDecisions+pl.OpenEscalations, live)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	shown := 0
	for _, pl := range v.Plans {
		if pl.Status != "closed" {
			shown++
		}
	}
	if more := v.OpenPlans - shown; more > 0 {
		fmt.Fprintf(out, "  …and %d more open plans the window does not show.\n", more)
	}
	if len(v.Withheld) == 0 {
		fmt.Fprintln(out, "\nNothing is withheld: every field passes the window's checks.")
	} else {
		fmt.Fprintf(out, "\n%d withheld (the page reads %q in place of each):\n", len(v.Withheld), planwindow.WithheldText)
		for _, w := range v.Withheld {
			fmt.Fprintf(out, "  %s (%s)\n", w.Path, w.Rule)
		}
	}
	fmt.Fprintf(out, "\nThe page itself, the World as a visitor sees it, on your viewer sign-in:\n  %s\n", page)
	return nil
}
