package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"gardenio/clicreds/oidclogin"
	"gardenio/lands/contract/landsv1"

	"github.com/spf13/cobra"
)

// A member's own agent (Door D.16, task gd-6yW5zvZSbz; owner choice C, doc
// gd-K7BUSVVHJF). gl owns only the launch: it starts the
// agent on the land (createAgent, on the member's own sign-in), then runs gb,
// found on PATH, which runs the session exactly as a land pod does, kd and gb
// under the agent's own land token at the land door. gl never runs kd or gb
// code itself and never hands the agent the member's sign-in: a file named by
// GARDENIO_TOKEN_FILE carries the agent's token and oidclogin.NoSessionEnv
// keeps every kd and gb in the session off the member's cached sign-in, which
// both send before any token in the environment (gd-QV2WuUjely: a laptop
// agent must not answer decisions as its tenant). While gb runs gl renews the token in that file
// (refreshAgent, gd-Vbvd3EgqTI), holding the spent-once refresh in memory
// only. When gb exits gl stops the agent (revokeAgent).
//
// The agent pays for its own model use with whatever the machine's claude is
// signed in with (decision gd-09hIjRBKvf): gl sets no model address or key.

// agentName is the contract's AgentCreate.name.
var agentName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// lookPath finds a program on PATH; a var so tests use their own gb.
var lookPath = exec.LookPath

// agentRevokeTimeout bounds the revoke after gb exits.
const agentRevokeTimeout = 20 * time.Second

// agentRenewCheck is how often gl looks at the clock to see whether the
// agent's token is due for renewal; a var so tests renew in moments. It is a
// poll of the wall clock, not one long timer, so a machine that slept past
// the renewal renews when it wakes.
var agentRenewCheck = time.Minute

func newAgentCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "agent",
		Aliases: []string{"agents"},
		Short:   "Run an agent for the land on this machine: start, list, stop",
	}
	cmd.AddCommand(agentStart(a), agentList(a), agentStop(a))
	return cmd
}

func agentStart(a *app) *cobra.Command {
	var name, role, dir, ticket, command string
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start an agent on this machine that works the land's tickets",
		Long: `Start an agent on this machine. The land registers it on its crew and
answers a credential for it alone; gl then runs 'gb agent start --local'
(gb, kd and coop must be on PATH) with that credential at the land door.
The agent works as a land agent whatever your own role: it cannot answer
decisions, approve escalations or start other agents. It never sees your
sign-in.

The agent's model use is paid by whatever claude on this machine is signed
in with; the land's budget is not spent. The session is attached to this
terminal; when it ends gl stops the agent on the land. Its credential lasts
about a day, and gl renews it while the session runs.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.asTenant != "" {
				return errors.New("a support view cannot start agents; sign in as a member of the land")
			}
			if name == "" {
				name = defaultAgentName()
			}
			if name != "" && !agentName.MatchString(name) {
				return fmt.Errorf("%q is not an agent name (a lower-case letter, then up to 31 lower-case letters, digits and dashes)", name)
			}
			if dir == "" {
				dir = "."
			}
			dir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				return fmt.Errorf("--dir %s is not a directory", dir)
			}
			// Find the tools before the land is asked: a started agent holds
			// one of its live agent slots.
			gb, err := lookPath("gb")
			if err != nil {
				return fmt.Errorf("gb is not on PATH (%v); install gb, kd and coop first (see gl's README)", err)
			}
			if _, err := lookPath("coop"); err != nil {
				return fmt.Errorf("coop is not on PATH (%v); install gb, kd and coop first (see gl's README)", err)
			}
			l, err := a.connect()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			body := landsv1.AgentCreate{}
			if name != "" {
				body.Name = &name
			}
			r, err := l.c.CreateAgentWithResponse(ctx, l.land, &landsv1.CreateAgentParams{IdempotencyKey: newKey()}, body)
			if err != nil {
				return err
			}
			if r.JSON201 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			g := r.JSON201
			// From here the agent holds a slot: every way out stops it.
			defer l.revokeAgent(ctx, g.AgentId)

			door, err := agentDoor(l, g.DoorUrl)
			if err != nil {
				return err
			}
			ag, err := l.c.GetAgentWithResponse(ctx, l.land, g.AgentId)
			if err != nil {
				return err
			}
			if ag.JSON200 == nil {
				return l.fail(ag.StatusCode(), ag.ApplicationproblemJSONDefault)
			}
			fmt.Fprintf(a.errOut, "gl: started agent %s (%s) on land %s; gl renews its credential while the session runs\n",
				ag.JSON200.Name, g.AgentId, l.land)

			tok, err := newAgentToken(g.Bearer)
			if err != nil {
				return fmt.Errorf("keeping the agent's credential: %w", err)
			}
			defer tok.remove()
			rctx, stopRenew := context.WithCancel(ctx)
			renewed := make(chan struct{})
			go func() {
				defer close(renewed)
				l.renewAgent(rctx, *g, tok)
			}()
			defer func() { stopRenew(); <-renewed }()

			args := []string{"--http-url", door, "agent", "start", "--local",
				"--agent-id", g.AgentId, "--name", ag.JSON200.Name,
				"--role", role, "--project", l.land, "--dir", dir}
			if ticket != "" {
				args = append(args, "--task", ticket)
			}
			if command != "" {
				args = append(args, "--command", command)
			}
			return runAgentGB(gb, args, agentEnv(os.Environ(), tok.path, g.Bearer), a.out, a.errOut)
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "what to call the agent; the land adds a short suffix (default: this machine's name)")
	f.StringVar(&role, "role", "lands-gardener", "the role the session primes as")
	f.StringVar(&dir, "dir", "", "the directory the agent works in (default: the current one)")
	f.StringVar(&ticket, "ticket", "", "a ticket for the agent to take first")
	f.StringVar(&command, "command", "", "the agent command gb runs (default: gb's, claude)")
	_ = f.MarkHidden("command")
	return cmd
}

// defaultAgentName is this machine's name as an agent name, or "" (the land
// then calls it laptop).
func defaultAgentName() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	h = strings.Trim(regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(h, "-"), "-")
	if len(h) > 32 {
		h = strings.TrimRight(h[:32], "-")
	}
	if !agentName.MatchString(h) {
		return ""
	}
	return h
}

// agentDoor is the base URL the agent's kd and gb use: the land's door at the
// edge gl itself talks to. The land names its door from the host the request
// came through; a door anywhere else is refused, so the agent's token is
// never sent to an address gl did not choose.
func agentDoor(l *landConn, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("the land answered a door gl cannot read (%v); not starting the agent", err)
	}
	e, _ := url.Parse(l.edge) // checked by connect
	if !strings.EqualFold(u.Host, e.Host) || strings.TrimSuffix(u.Path, "/") != "/land/"+l.land {
		return "", fmt.Errorf("the land answered its door as %s, not at %s/land/%s; not sending the agent's credential there", raw, l.edge, l.land)
	}
	return l.edge + "/land/" + l.land, nil
}

// agentEnv is gb's environment: this one without anything kd, gb or coop
// would read in place of the agent's own credential and address (the
// member's tokens, daemon address, project, agent identity), plus the file
// gl renews the agent's token in and the switch that keeps every kd and gb
// in the session off the member's cached sign-in. kd and gb read the file
// ahead of GARDENIO_TOKEN; the first token stays in GARDENIO_TOKEN only for
// a gb or kd too old to know the file, which then stops after a day.
func agentEnv(environ []string, tokenFile, bearer string) []string {
	drop := []string{"BEADS_", "KBEADS_", "KD_", "BOAT_", "GB_", "COOP_"}
	out := make([]string, 0, len(environ)+3)
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if k == "GARDENIO_TOKEN" || k == agentTokenFileEnv || k == oidclogin.NoSessionEnv || hasAnyPrefix(k, drop) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, agentTokenFileEnv+"="+tokenFile, "GARDENIO_TOKEN="+bearer, oidclogin.NoSessionEnv+"=1")
}

// agentTokenFileEnv names the file kd and gb read their token from.
const agentTokenFileEnv = "GARDENIO_TOKEN_FILE"

// agentToken is the file the session's kd and gb read the agent's token
// from: 0600 in a 0700 directory of its own, replaced whole on each renewal
// so no reader sees half a token, and removed when the session ends.
type agentToken struct{ dir, path string }

func newAgentToken(bearer string) (*agentToken, error) {
	dir, err := os.MkdirTemp("", "gl-agent-")
	if err != nil {
		return nil, err
	}
	t := &agentToken{dir: dir, path: filepath.Join(dir, "token")}
	if err := t.write(bearer); err != nil {
		t.remove()
		return nil, err
	}
	return t, nil
}

func (t *agentToken) write(bearer string) error {
	f, err := os.CreateTemp(t.dir, ".token-")
	if err != nil {
		return err
	}
	_, err = f.WriteString(bearer + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), t.path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

func (t *agentToken) remove() { _ = os.RemoveAll(t.dir) }

// renewAgent keeps the agent's token in tok fresh until ctx ends: past two
// thirds of a token's life it spends the refresh for the next token and
// refresh. It asks at the door on the agent's own token, so a member whose
// sign-in has lapsed meanwhile does not end the session; only when the land
// refuses that token (the machine slept past its end) does it ask on the
// member's sign-in, which a 'gl login' elsewhere renews. A failed renewal is
// tried again on the next check; one the land will never grant (the agent
// was stopped, or the refresh is not its own) ends the renewals, said once.
func (l *landConn) renewAgent(ctx context.Context, g landsv1.AgentGrant, tok *agentToken) {
	door, err := landsv1.NewClientWithResponses(l.base, landsv1.WithHTTPClient(l.a.hc),
		landsv1.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+g.Bearer)
			return nil
		}))
	if err != nil {
		fmt.Fprintf(l.a.errOut, "gl: cannot renew agent %s's credential (%v); it lapses at %s\n", g.AgentId, err, g.ExpiresAt.Local().Format(time.RFC1123))
		return
	}
	// Times are wall-clock (Round(0)): a monotonic reading does not advance
	// while some machines sleep.
	issued := l.a.now().Round(0)
	key := newKey()
	unwritten, lapsed := false, false
	tick := time.NewTicker(agentRenewCheck)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if unwritten && tok.write(g.Bearer) == nil {
			unwritten = false
		}
		now := l.a.now().Round(0)
		if now.Before(issued.Add(g.ExpiresAt.Sub(issued) * 2 / 3)) {
			continue
		}
		params := &landsv1.RefreshAgentParams{IdempotencyKey: key}
		body := landsv1.AgentRefresh{Refresh: g.Refresh}
		r, err := door.RefreshAgentWithResponse(ctx, l.land, g.AgentId, params, body)
		if err == nil && r.StatusCode() == http.StatusUnauthorized {
			r, err = l.c.RefreshAgentWithResponse(ctx, l.land, g.AgentId, params, body)
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil && r.JSON200 == nil {
			err = l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			switch r.StatusCode() {
			case http.StatusNotFound, http.StatusConflict, http.StatusForbidden:
				fmt.Fprintf(l.a.errOut, "gl: the land will not renew agent %s's credential (%v); it lapses at %s\n",
					g.AgentId, err, g.ExpiresAt.Local().Format(time.RFC1123))
				return
			}
		}
		if err != nil {
			if !lapsed && !now.Before(g.ExpiresAt) {
				lapsed = true
				fmt.Fprintf(l.a.errOut, "gl: agent %s's credential has lapsed and gl could not renew it (%v); gl keeps trying, and 'gl login' in another terminal lets it\n", g.AgentId, err)
			}
			continue
		}
		next := *r.JSON200
		g.Bearer, g.Refresh, g.ExpiresAt = next.Bearer, next.Refresh, next.ExpiresAt
		issued, key, lapsed = now, newKey(), false
		if err := tok.write(g.Bearer); err != nil {
			// The refresh is spent: the new token lives in memory until the
			// file takes it, tried again on each check.
			unwritten = true
			fmt.Fprintf(l.a.errOut, "gl: could not write agent %s's renewed credential (%v); trying again\n", g.AgentId, err)
		}
	}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// runAgentGB runs gb attached to this terminal until it exits. Ctrl-C
// reaches gb from the terminal (gl's own interrupt only ends its context);
// a SIGTERM sent to gl alone is passed on, so gb shuts the session down.
func runAgentGB(gb string, args, env []string, out, errOut io.Writer) error {
	c := exec.Command(gb, args...)
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, out, errOut
	if err := c.Start(); err != nil {
		return fmt.Errorf("starting gb: %w", err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case s := <-sig:
				_ = c.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	if err := c.Wait(); err != nil {
		return fmt.Errorf("the agent session ended: gb %w", err)
	}
	return nil
}

// revokeAgent stops the agent on the land, on the member's sign-in, even
// after Ctrl-C.
func (l *landConn) revokeAgent(parent context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), agentRevokeTimeout)
	defer cancel()
	r, err := l.c.RevokeAgentWithResponse(ctx, l.land, id, &landsv1.RevokeAgentParams{IdempotencyKey: newKey()})
	if err == nil && r.JSON200 == nil {
		err = l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
	}
	if err != nil {
		fmt.Fprintf(l.a.errOut, "gl: could not stop agent %s on land %s (%v); run 'gl agent stop %s'\n", id, l.land, err, id)
		return
	}
	fmt.Fprintf(l.a.errOut, "gl: stopped agent %s on land %s.\n", id, l.land)
}

func agentList(a *app) *cobra.Command {
	var pg paging
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the agents members run on their own machines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.ListAgentsWithResponse(cmd.Context(), l.land, &landsv1.ListAgentsParams{Cursor: pg.cursorPtr(), Limit: pg.limitPtr()})
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			p := r.JSON200
			err = a.emit(p, func(w io.Writer) {
				if len(p.Items) == 0 {
					fmt.Fprintln(w, "No agents.")
					return
				}
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tSTARTED BY\tSTARTED")
				for _, g := range p.Items {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", g.Id, g.Name, g.Status, g.StartedBy, g.StartedAt.Local().Format(time.DateTime))
				}
				tw.Flush()
			})
			a.moreHint(p.NextCursor)
			return err
		},
	}
	pg.flags(cmd)
	return cmd
}

func agentStop(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <agent>",
		Short: "Stop an agent; its credential stops working at once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := a.connect()
			if err != nil {
				return err
			}
			r, err := l.c.RevokeAgentWithResponse(cmd.Context(), l.land, args[0], &landsv1.RevokeAgentParams{IdempotencyKey: newKey()})
			if err != nil {
				return err
			}
			if r.JSON200 == nil {
				return l.fail(r.StatusCode(), r.ApplicationproblemJSONDefault)
			}
			g := r.JSON200
			return a.emit(g, func(w io.Writer) {
				fmt.Fprintf(w, "Agent %s (%s) is %s.\n", g.Name, g.Id, g.Status)
			})
		},
	}
}
