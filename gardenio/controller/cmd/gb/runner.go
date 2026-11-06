package main

// gb runner register|join|status|leave (local-agents spec §5, §10).
//
// A runner is this machine, registered once with an invite. `join` wraps the
// low-level `gb agent start --local` in a child process and keeps the runner
// bead's heartbeat going while the agent runs.
//
// The session runs on the runner token, not the owner's credentials (spec
// §4.2, §6): its writes are capped by the runner route allowlist and the
// runner's projects, and resolve actor=owner, operated_by=owner (D6). join
// hands the token to the child under runnerTokenEnv; the child unsets it
// (takeRunnerEnv), uses it for its own daemon client, and gives the session
// a 0600 GARDENIO_TOKEN_FILE holding it, GARDENIO_NO_LOGIN_SESSION=1 and
// BEADS_HTTP_URL=<runner URL> (runnerSessionCreds), so the session's kd and
// gb never send the owner's login session or a token from the owner's shell.
//
// The token is in a file, not the session's environment, so anything that
// dumps or forwards the env does not carry it. That is not a security
// boundary: the session runs under the same uid as gb and can read the file.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/localreg"
	"gardenio/controller/internal/runnerreg"

	"github.com/spf13/cobra"
)

// runnerHeartbeatInterval is how often join marks the runner online.
var runnerHeartbeatInterval = 30 * time.Second

// runnerLeaveWait bounds how long leave waits for its agents to exit.
var runnerLeaveWait = 30 * time.Second

const (
	// runnerTokenEnv and runnerURLEnv carry the runner bearer and the daemon
	// URL it is valid for from `gb runner join` to its `gb agent start` child.
	runnerTokenEnv = "GB_RUNNER_TOKEN"
	runnerURLEnv   = "GB_RUNNER_URL"
)

var runnerCmd = &cobra.Command{
	Use:     "runner",
	Short:   "Register this machine as a gardenio runner and run local agents on it",
	GroupID: "agent",
	Long: `Register this machine once with an invite, then join it as an agent as
often as you like. The agent runs on this host with your own Claude login;
gb keeps the runner online while it runs.

  gb runner register --invite <code> --daemon <url>   once per machine
  gb runner join [--project p] [--dir .]             start an agent (attached)
  gb runner status                                   runner bead and local agents
  gb runner leave                                    stop local agents, mark away

gb agent start --local stays the low-level form; join calls it.`,
}

var runnerRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register this machine with an invite code",
	Args:  cobra.NoArgs,
	RunE:  runRunnerRegister,
}

var runnerJoinCmd = &cobra.Command{
	Use:   "join",
	Short: "Start a local agent on this runner (attached; ctrl-C leaves)",
	Args:  cobra.NoArgs,
	RunE:  runRunnerJoin,
}

var runnerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show this runner's bead and its local agents",
	Args:  cobra.NoArgs,
	RunE:  runRunnerStatus,
}

var runnerLeaveCmd = &cobra.Command{
	Use:   "leave",
	Short: "Stop this runner's local agents and mark the runner away",
	Args:  cobra.NoArgs,
	RunE:  runRunnerLeave,
}

func init() {
	runnerRegisterCmd.Flags().String("invite", "", "Invite code from /runner invite (required)")
	runnerRegisterCmd.Flags().String("daemon", "", "Runner URL of the beads daemon, from the invite (required)")
	runnerRegisterCmd.Flags().StringSlice("tags", nil, "Tags for placement, comma-separated (e.g. gpu,mac)")
	_ = runnerRegisterCmd.MarkFlagRequired("invite")
	_ = runnerRegisterCmd.MarkFlagRequired("daemon")

	runnerJoinCmd.Flags().String("project", "", "Project to work in (default: KD_PROJECT, or the runner's only project)")
	runnerJoinCmd.Flags().String("dir", ".", "Working directory")
	runnerJoinCmd.Flags().String("name", "", "Agent name (default: derived from hostname)")
	runnerJoinCmd.Flags().String("command", "", "Command to run (default: claude --dangerously-skip-permissions)")

	runnerCmd.AddCommand(runnerRegisterCmd, runnerJoinCmd, runnerStatusCmd, runnerLeaveCmd)
}

func runRunnerRegister(cmd *cobra.Command, _ []string) error {
	invite, _ := cmd.Flags().GetString("invite")
	daemonURL, _ := cmd.Flags().GetString("daemon")
	tags, _ := cmd.Flags().GetStringSlice("tags")
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return fmt.Errorf("read hostname: %w", err)
	}
	client := runnerreg.Client{BaseURL: strings.TrimRight(daemonURL, "/")}
	reg, token, err := client.Register(cmd.Context(), strings.TrimSpace(invite), hostname, runtime.GOOS+"/"+runtime.GOARCH, tags)
	if err != nil {
		return err
	}
	if err := runnerreg.Save(reg, token); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Registered %s as %s for %s (projects: %s)\n",
		reg.Hostname, reg.RunnerID, reg.Owner, strings.Join(reg.Projects, ", "))
	return nil
}

// joinArgs is the `gb agent start --local` command line join runs.
func joinArgs(runnerID, owner, project, dir, name, command string) []string {
	args := []string{"agent", "start", "--local", "--role", "local", "--runner", runnerID, "--owner", owner, "--dir", dir}
	if project != "" {
		args = append(args, "--project", project)
	}
	if name != "" {
		args = append(args, "--name", name)
	}
	if command != "" {
		args = append(args, "--command", command)
	}
	return args
}

// joinProject settles the project join starts the agent in: the flag (or
// KD_PROJECT / BOAT_PROJECT), else the runner's only project. The daemon
// refuses runner writes outside the allowlist, so a project outside it fails
// here instead.
func joinProject(project string, allowlist []string) (string, error) {
	project = resolveProject(project)
	switch {
	case project == "" && len(allowlist) == 1:
		return allowlist[0], nil
	case project == "":
		return "", fmt.Errorf("this runner serves projects %s: pass --project", strings.Join(allowlist, ", "))
	case len(allowlist) > 0 && !slices.Contains(allowlist, project):
		return "", fmt.Errorf("this runner is not allowed project %q (allowed: %s)", project, strings.Join(allowlist, ", "))
	}
	return project, nil
}

// joinEnv is the child's environment: the parent's, minus any inherited
// runner variables, plus the runner token and URL for the child's own client.
func joinEnv(parent []string, daemonURL, token string) []string {
	env := make([]string, 0, len(parent)+2)
	for _, e := range parent {
		key, _, _ := strings.Cut(e, "=")
		if key == runnerTokenEnv || key == runnerURLEnv {
			continue
		}
		env = append(env, e)
	}
	return append(env, runnerURLEnv+"="+daemonURL, runnerTokenEnv+"="+token)
}

func runRunnerJoin(cmd *cobra.Command, _ []string) error {
	reg, token, err := runnerreg.Load()
	if err != nil {
		return err
	}
	project, _ := cmd.Flags().GetString("project")
	dir, _ := cmd.Flags().GetString("dir")
	name, _ := cmd.Flags().GetString("name")
	command, _ := cmd.Flags().GetString("command")
	if project, err = joinProject(project, reg.Projects); err != nil {
		return err
	}
	if dir == "" || dir == "." {
		if dir, err = os.Getwd(); err != nil {
			return fmt.Errorf("get cwd: %w", err)
		}
	}
	client := runnerreg.Client{BaseURL: reg.DaemonURL, Token: token}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// A leave from here on stops this join's heartbeats; an earlier one is
	// undone by the heartbeat just below.
	joined := time.Now()
	left := func() bool { return runnerreg.LeftSince(joined) }

	// Fail fast on a revoked token (spec §10): the first heartbeat happens
	// before coop starts.
	if err := client.Heartbeat(ctx, reg.RunnerID, ""); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	child := exec.CommandContext(ctx, self, joinArgs(reg.RunnerID, reg.Owner, project, dir, name, command)...)
	child.Env = joinEnv(os.Environ(), reg.DaemonURL, token)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Forward SIGINT/SIGTERM as one SIGTERM, which gb agent start handles as
	// a clean leave (coop stopped, agent bead closed).
	child.Cancel = func() error { return child.Process.Signal(syscall.SIGTERM) }
	child.WaitDelay = 30 * time.Second
	if err := child.Start(); err != nil {
		return fmt.Errorf("start local agent: %w", err)
	}

	hbErr := make(chan error, 1)
	go func() {
		hbErr <- runnerHeartbeatLoop(ctx, client, reg.RunnerID, runnerHeartbeatInterval, left, os.Stderr)
	}()
	waitErr := make(chan error, 1)
	go func() { waitErr <- child.Wait() }()

	for {
		select {
		case err := <-hbErr:
			if err == nil {
				// ctx is done or the runner left: wait for the child.
				hbErr = nil
				continue
			}
			cancel()
			<-waitErr
			return err
		case err := <-waitErr:
			if ctx.Err() != nil {
				// Left via a signal: a non-zero exit from the SIGTERM is the
				// clean path, not a failure.
				return nil
			}
			return err
		}
	}
}

// runnerHeartbeatLoop marks runnerID online every interval until ctx is done
// or left reports a `gb runner leave` (returns nil), or the daemon rejects
// the token (returns ErrTokenRejected). Other failures are logged to w and
// retried on the next tick.
func runnerHeartbeatLoop(ctx context.Context, client runnerreg.Client, runnerID string, interval time.Duration, left func() bool, w io.Writer) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if left() {
				fmt.Fprintln(w, "[gb runner] gb runner leave ran; no longer marking the runner online")
				return nil
			}
			err := client.Heartbeat(ctx, runnerID, "")
			switch {
			case errors.Is(err, runnerreg.ErrTokenRejected):
				return err
			case err != nil && ctx.Err() == nil:
				fmt.Fprintf(w, "[gb runner] heartbeat failed (will retry): %v\n", err)
			}
		}
	}
}

// runnerEntries returns the local registry entries started on runnerID.
func runnerEntries(entries []localreg.Entry, runnerID string) []localreg.Entry {
	var out []localreg.Entry
	for _, e := range entries {
		if e.RunnerID == runnerID {
			out = append(out, e)
		}
	}
	return out
}

func runRunnerStatus(cmd *cobra.Command, _ []string) error {
	reg, token, err := runnerreg.Load()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	client := runnerreg.Client{BaseURL: reg.DaemonURL, Token: token}
	bead, getErr := client.Get(cmd.Context(), reg.RunnerID)

	fmt.Fprintf(out, "Runner:     %s\n", reg.RunnerID)
	fmt.Fprintf(out, "Daemon:     %s\n", reg.DaemonURL)
	fields, _ := bead["fields"].(map[string]any)
	for _, f := range []struct{ label, key, local string }{
		{"Hostname:", "hostname", reg.Hostname},
		{"Owner:", "owner", reg.Owner},
		{"State:", "state", ""},
		{"Last seen:", "last_seen_at", ""},
		{"Projects:", "projects", strings.Join(reg.Projects, ", ")},
	} {
		v := runnerField(fields, f.key)
		if v == "" {
			v = f.local
		}
		if v != "" {
			fmt.Fprintf(out, "%-11s %s\n", f.label, v)
		}
	}
	switch {
	case getErr == nil:
		fmt.Fprintln(out, "Token:      valid")
	case errors.Is(getErr, runnerreg.ErrTokenRejected):
		fmt.Fprintf(out, "Token:      %v\n", getErr)
	default:
		fmt.Fprintf(out, "Token:      unknown (%v)\n", getErr)
	}

	entries, _, err := localreg.List()
	if err != nil {
		return fmt.Errorf("read local registry: %w", err)
	}
	mine := runnerEntries(entries, reg.RunnerID)
	fmt.Fprintf(out, "Local agents: %d\n", len(mine))
	for _, e := range mine {
		state := "dead"
		if localreg.Alive(e) {
			state = "alive"
		}
		fmt.Fprintf(out, "  %-22s %-5s pid=%d bead=%s project=%s dir=%s\n", e.Name, state, e.PID, e.BeadID, e.Project, e.Dir)
	}
	if errors.Is(getErr, runnerreg.ErrTokenRejected) {
		return getErr
	}
	return nil
}

// runnerField renders a runner bead field: strings as-is, lists joined.
func runnerField(fields map[string]any, key string) string {
	switch v := fields[key].(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, p := range v {
			parts = append(parts, fmt.Sprint(p))
		}
		return strings.Join(parts, ", ")
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func runRunnerLeave(cmd *cobra.Command, _ []string) error {
	reg, token, err := runnerreg.Load()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	// Before any agent is stopped, so a join heartbeating for one sees it on
	// its next tick and stops marking the runner online.
	if err := runnerreg.MarkLeft(); err != nil {
		return fmt.Errorf("mark runner left: %w", err)
	}
	entries, _, err := localreg.List()
	if err != nil {
		return fmt.Errorf("read local registry: %w", err)
	}
	var live []localreg.Entry
	for _, e := range runnerEntries(entries, reg.RunnerID) {
		if !localreg.Alive(e) {
			continue
		}
		// The registry PID is coop's, which gb agent start runs in its own
		// process group. Signal coop alone, once: a second SIGTERM (as a
		// group signal would add for its child) makes coop force-exit.
		if err := syscall.Kill(e.PID, syscall.SIGTERM); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "[gb runner] stop %s (pid %d): %v\n", e.Name, e.PID, err)
			continue
		}
		fmt.Fprintf(out, "Stopping %s (pid %d)...\n", e.Name, e.PID)
		live = append(live, e)
	}
	stuck := waitForExit(cmd.Context(), live, runnerLeaveWait)
	for _, e := range stuck {
		fmt.Fprintf(cmd.ErrOrStderr(), "[gb runner] %s (pid %d) still running after %s\n", e.Name, e.PID, runnerLeaveWait)
	}

	// Mark away even with agents left running: placement must stop sending
	// work here, and the marker keeps their joins from heartbeating it back.
	client := runnerreg.Client{BaseURL: reg.DaemonURL, Token: token}
	if err := client.Heartbeat(cmd.Context(), reg.RunnerID, "away"); err != nil {
		return err
	}
	fmt.Fprintf(out, "Runner %s is away.\n", reg.RunnerID)
	if len(stuck) > 0 {
		pids := make([]string, len(stuck))
		for i, e := range stuck {
			pids[i] = fmt.Sprint(e.PID)
		}
		return fmt.Errorf("%d local agent(s) still running after %s; stop them with kill %s, then run gb runner leave again",
			len(stuck), runnerLeaveWait, strings.Join(pids, " "))
	}
	return nil
}

// waitForExit polls until every entry's PID has exited or timeout passes,
// and returns the entries still running.
func waitForExit(ctx context.Context, entries []localreg.Entry, timeout time.Duration) []localreg.Entry {
	deadline := time.Now().Add(timeout)
	for {
		var running []localreg.Entry
		for _, e := range entries {
			if localreg.Alive(e) {
				running = append(running, e)
			}
		}
		if len(running) == 0 || !time.Now().Before(deadline) || ctx.Err() != nil {
			return running
		}
		entries = running
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// takeRunnerEnv returns the runner token and URL join passed in runnerTokenEnv
// and runnerURLEnv, and unsets both, so they never reach the coop session's
// environment (localAgentEnv copies os.Environ).
func takeRunnerEnv() (token, url string) {
	token, url = os.Getenv(runnerTokenEnv), os.Getenv(runnerURLEnv)
	_ = os.Unsetenv(runnerTokenEnv)
	_ = os.Unsetenv(runnerURLEnv)
	return token, url
}

// runnerSessionCreds writes token to a fresh 0600 file in the runner's
// registration directory and returns the session credentials for it: the
// runner URL and that file, and no other token. remove deletes the file; call
// it once the session has exited.
func runnerSessionCreds(token, url string) (creds localCreds, remove func(), err error) {
	if url == "" {
		return localCreds{}, nil, fmt.Errorf("%s is set without %s", runnerTokenEnv, runnerURLEnv)
	}
	dir, err := runnerreg.Dir()
	if err != nil {
		return localCreds{}, nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return localCreds{}, nil, fmt.Errorf("runner session token: %w", err)
	}
	// CreateTemp makes the file 0600.
	f, err := os.CreateTemp(dir, "session-*.token")
	if err != nil {
		return localCreds{}, nil, fmt.Errorf("runner session token: %w", err)
	}
	path := f.Name()
	remove = func() { _ = os.Remove(path) }
	_, werr := f.WriteString(token)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		remove()
		return localCreds{}, nil, fmt.Errorf("runner session token: %w", werr)
	}
	return localCreds{HTTPURL: normalizeBeadsURL(url), TokenFile: path, Source: "runner"}, remove, nil
}

// useRunnerDaemon points gb agent start's daemon client at the runner token
// from takeRunnerEnv. Without a token (a hand-run `gb agent start --local
// --runner`) the client is unchanged.
func useRunnerDaemon(token, url string) error {
	if token == "" {
		return nil
	}
	if url == "" {
		return fmt.Errorf("%s is set without %s", runnerTokenEnv, runnerURLEnv)
	}
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: url, Token: token})
	if err != nil {
		return fmt.Errorf("runner daemon client: %w", err)
	}
	c.SetActor(actor)
	if daemon != nil {
		daemon.Close()
	}
	daemon = c
	return nil
}
