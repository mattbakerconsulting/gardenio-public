// Command gl is the tenant's CLI for a land: sign in, choose the land, and
// work its tickets, decisions, escalations, plans, threads, budget, releases
// and live feed. It speaks only the Lands contract v1 (lands/contract/openapi.yaml)
// through the land door, https://<edge>/land/<name>/v1, with a session minted
// for that one land. It never holds any other credential or address.
//
// Design: contract design gd-EWSKPH7pa5 section 4 and boundary
// recommendation gd-uYNbyW4a9Z section 6; task gd-bR8MiyOpbe.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"time"

	"gardenio/clicreds/oidclogin"

	"github.com/spf13/cobra"
)

func main() {
	// Ctrl-C ends a command cleanly; gl watch stops without an error.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	a := newApp(os.Stdout, os.Stderr)
	if err := newRoot(a).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "gl:", err)
		os.Exit(1)
	}
}

// app is one invocation's options, output and clock. Commands read it; tests
// build their own.
type app struct {
	out, errOut io.Writer

	// Persistent flags.
	edgeFlag, landFlag, asTenant, webFlag string
	issuer, clientID                      string
	jsonOut                               bool

	hc          *http.Client // every call but the live feed
	streamHC    *http.Client // the live feed: no overall timeout
	now         func() time.Time
	sleep       func(time.Duration)
	openBrowser func(string) error
	getenv      func(string) string

	cfg        *config
	skewWarned bool
	bannerDone bool
}

func newApp(out, errOut io.Writer) *app {
	return &app{
		out:         out,
		errOut:      errOut,
		hc:          &http.Client{Timeout: oidclogin.HTTPTimeout},
		streamHC:    &http.Client{},
		now:         time.Now,
		sleep:       time.Sleep,
		openBrowser: openBrowser,
		getenv:      os.Getenv,
	}
}

func newRoot(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:   "gl",
		Short: "Work a land from the terminal",
		Long: `gl works one land: its tickets, decisions, escalations, plans, threads,
budget, releases and live feed.

Start with 'gl login --land <name>'. The first land you sign in to becomes
the default; 'gl use <name>' switches it, and --land picks one for a single
command. Every command takes --json for the raw contract objects. 'gl agent start' runs an
agent for the land on this machine.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&a.landFlag, "land", "", "the land to work (default: GL_LAND, then 'gl use')")
	pf.StringVar(&a.asTenant, "as-tenant", "", "support: see `land` exactly as its members do; what you change is recorded under your own sign-in")
	pf.StringVar(&a.edgeFlag, "edge", "", "the land door's address (default: GL_EDGE, then 'gl use --edge', then "+defaultEdge+")")
	pf.StringVar(&a.webFlag, "web", "", "the web address approvals open in (default: GL_WEB, then "+defaultWeb+")")
	pf.BoolVar(&a.jsonOut, "json", false, "print the contract objects as JSON")
	pf.StringVar(&a.issuer, "issuer", oidclogin.DefaultIssuer, "sign-in issuer")
	pf.StringVar(&a.clientID, "client-id", oidclogin.DefaultClientID, "sign-in client")
	_ = pf.MarkHidden("issuer")
	_ = pf.MarkHidden("client-id")

	root.AddCommand(
		newLoginCmd(a), newLogoutCmd(a), newWhoamiCmd(a), newUseCmd(a), newVersionCmd(a),
		newTicketCmd(a), newDecisionCmd(a), newEscalationCmd(a), newPlanCmd(a),
		newThreadCmd(a), newSayCmd(a), newBudgetCmd(a), newReleaseCmd(a), newWatchCmd(a), newAgentCmd(a),
	)
	return root
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
