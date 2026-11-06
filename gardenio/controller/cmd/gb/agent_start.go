package main

// gb agent start --local / --docker
//
// Spawns a local coop+claude agent session that is fully registered with the
// kbeads server and coopmux — indistinguishable from a K8s agent.
//
// --local  Run claude directly on the host inside a coop session. Attached.
// --docker Build the gardenio agent image locally and run in a container.
//          Mounts $PWD into /home/agent/workspace. Detachable.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gardenio/clicreds/oidclogin"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
	"gardenio/controller/internal/harness"
	"gardenio/controller/internal/localreg"
	"gardenio/controller/internal/mailsub"

	"github.com/spf13/cobra"
)

var agentStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start a local agent session (--local or --docker)",
	Long: `Start a coop-based agent session registered with the kbeads server.

  --local   Run claude on the host via coop. Attached (ctrl-C to stop).
  --docker  Build the gardenio agent image and run in a container.
            Mounts $PWD into /home/agent/workspace. Detachable by default.

The agent is registered with the kbeads server on startup and appears in
'gb agent roster'. It is deregistered and its bead closed on exit.

Mock / claudeless (--local):
  --test-scenario <name|path>
      Run claudeless with a scenario TOML instead of claude. Useful for e2e
      coverage of the local-agent path without hitting the Anthropic API.

      Bare names resolve under --test-scenario-dir, or (by default)
      gardenio/tests/e2e/claudeless/<name>.toml. A path with '/' or a '.toml'
      suffix is used verbatim (absolutized against --dir).

Pre-assigned task:
  --task <bead-id>
      Set BOAT_TASK_ID on the agent so the initial nudge prompt directs the
      agent to ka-claim the given task. Mirrors K8s pre-assignment.

An agent registered for you (--local):
  --agent-id <bead-id> --name <name>
      Run as an agent bead that already exists, with its own token: a land
      agent that 'gl agent start' minted. Nothing is created, superseded or
      closed; the process that registered it stops it. The token comes from
      --token, GARDENIO_TOKEN_FILE (which 'gl agent start' renews) or
      GARDENIO_TOKEN at --http-url, and GARDENIO_NO_LOGIN_SESSION=1 must be
      set so neither gb nor the agent's kd and gb send a login session.`,
	RunE: runAgentStart,
}

func init() {
	agentCmd.AddCommand(agentStartCmd)

	agentStartCmd.Flags().Bool("k8s", false, "Run as K8s pod entrypoint (replaces entrypoint.sh)")
	agentStartCmd.Flags().Bool("local", false, "Run agent on the host via coop (attached)")
	agentStartCmd.Flags().Bool("docker", false, "Run agent in a Docker container")
	agentStartCmd.Flags().String("name", "", "Agent name (default: derived from hostname)")
	agentStartCmd.Flags().String("role", "crew", "Agent role (e.g. crew, gardener)")
	agentStartCmd.Flags().String("dir", "", "Working directory (default: $PWD)")
	agentStartCmd.Flags().String("image-dir", "", "Path to gardenio agent Dockerfile dir (--docker only)")
	agentStartCmd.Flags().String("image", "", "Pre-built image name to skip rebuild (--docker only)")
	agentStartCmd.Flags().Bool("attach", false, "Follow container output (--docker only; --local is always attached)")
	agentStartCmd.Flags().String("command", "", "Command to run (default: claude --dangerously-skip-permissions)")
	agentStartCmd.Flags().String("test-scenario", "", "Claudeless scenario to run instead of claude (--local only). Name (resolved under --test-scenario-dir) or path to .toml.")
	agentStartCmd.Flags().String("test-scenario-dir", "", "Directory to resolve --test-scenario names in (default: <dir>/gardenio/tests/e2e/claudeless or <dir>/tests/e2e/claudeless)")
	agentStartCmd.Flags().Bool("watch-escalations", false, "Deprecated, does nothing: with --runner the agent always runs the local escalations the daemon approves for that runner (its control:escalation-approved mail); without --runner none reach it")
	agentStartCmd.Flags().String("project", "", "Project the agent registers under: labels its bead project:<p> (default: KD_PROJECT / BOAT_PROJECT; empty = all)")
	agentStartCmd.Flags().String("task", "", "Pre-assign a task bead to the agent (sets BOAT_TASK_ID so the nudge prompt steers the agent to `kd claim <id>`)")
	agentStartCmd.Flags().String("runner", "", "Runner bead this agent runs on (--local only; set by `gb runner join`)")
	agentStartCmd.Flags().String("owner", "", "Operator the runner acts for (--local only; set by `gb runner join`)")
	agentStartCmd.Flags().String("agent-id", "", "Run as this already-registered agent bead on its own token, leaving the bead to whoever registered it (--local only; set by `gl agent start`)")

	// K8s-only flags.
	agentStartCmd.Flags().String("workspace", "/home/agent/workspace", "Workspace path (--k8s only)")
	agentStartCmd.Flags().Int("coop-port", 8080, "Coop HTTP port (--k8s only)")
	agentStartCmd.Flags().Int("coop-health-port", 9090, "Coop health port (--k8s only)")
	agentStartCmd.Flags().Int("coop-metrics-port", 9093, "Coop Prometheus metrics port (--k8s only)")
	agentStartCmd.Flags().Int("max-restarts", 0, "Max consecutive restarts — 0 uses COOP_MAX_RESTARTS env or 10 (--k8s only)")
}

func runAgentStart(cmd *cobra.Command, args []string) error {
	isK8s, _ := cmd.Flags().GetBool("k8s")
	if isK8s {
		return runAgentStartK8s(cmd, args)
	}

	isLocal, _ := cmd.Flags().GetBool("local")
	isDocker, _ := cmd.Flags().GetBool("docker")

	if isLocal == isDocker {
		return fmt.Errorf("specify exactly one of --local, --docker, or --k8s")
	}

	agentName, _ := cmd.Flags().GetString("name")
	nameExplicit := agentName != ""
	adoptID, _ := cmd.Flags().GetString("agent-id")
	if err := checkAdoptedAgent(adoptID, isLocal, nameExplicit, os.Getenv(oidclogin.NoSessionEnv)); err != nil {
		return err
	}
	// Before the daemon is asked anything: an agent with no token of its own
	// must not reach it on another credential.
	var creds localCreds
	if adoptID != "" {
		var err error
		if creds, err = adoptedAgentCreds(httpURL, tokenFlag, defaultToken()); err != nil {
			return err
		}
		if tokenFlag == "" {
			// Keep the file 'gl agent start' renews the token into.
			creds.TokenFile = os.Getenv(tokenFileEnv)
		}
	}
	if agentName == "" {
		h, _ := os.Hostname()
		agentName = "local-" + h
	}
	// For --local spawns, disambiguate against the on-disk registry so two
	// agents started back-to-back on the same host cannot collide on name.
	// An explicit --name opts out (caller is responsible for uniqueness).
	if isLocal && !nameExplicit {
		agentName = localreg.PickName(agentName)
	}
	role, _ := cmd.Flags().GetString("role")
	dir, _ := cmd.Flags().GetString("dir")
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get cwd: %w", err)
		}
	}
	dir, _ = filepath.Abs(dir)

	agentCommand, _ := cmd.Flags().GetString("command")
	scenario, _ := cmd.Flags().GetString("test-scenario")
	scenarioDir, _ := cmd.Flags().GetString("test-scenario-dir")

	var scenarioPath string
	if scenario != "" {
		if isDocker {
			return fmt.Errorf("--test-scenario is only supported with --local (for a K8s mock pod, set mock_scenario on the agent bead)")
		}
		if agentCommand != "" {
			return fmt.Errorf("--test-scenario and --command are mutually exclusive")
		}
		resolvedPath, err := resolveScenarioPath(scenario, scenarioDir, dir)
		if err != nil {
			return err
		}
		scenarioPath = resolvedPath
		// claudeless 0.4.0 strict-rejects the gardenio-side [test_scenario]
		// table (ka-0000002111). Hand it a stripped copy; resolveStartupPrompt
		// still reads the original via scenarioPath.
		claudelessPath, cleanup, err := materializeClaudelessScenario(resolvedPath)
		if err != nil {
			return fmt.Errorf("prepare claudeless scenario: %w", err)
		}
		defer cleanup()
		agentCommand = fmt.Sprintf("claudeless --scenario %s --dangerously-skip-permissions", shellQuote(claudelessPath))
		fmt.Printf("[gb agent start] Test scenario mode: %s\n", resolvedPath)
		if claudelessPath != resolvedPath {
			fmt.Printf("[gb agent start] Stripped [test_scenario] for claudeless: %s\n", claudelessPath)
		}
	}

	h, err := harness.FromEnv()
	if err != nil {
		return err
	}
	if agentCommand == "" {
		agentCommand = harness.DefaultCommand(h)
	}

	runnerID, _ := cmd.Flags().GetString("runner")
	owner, _ := cmd.Flags().GetString("owner")
	// Take the runner env with or without --runner, so a GB_RUNNER_TOKEN
	// exported in the shell never reaches coop's environment either.
	runnerToken, runnerURL := takeRunnerEnv()
	if runnerID != "" {
		if !isLocal {
			return fmt.Errorf("--runner is only supported with --local")
		}
		if adoptID != "" {
			return fmt.Errorf("--runner and --agent-id are mutually exclusive")
		}
		if err := useRunnerDaemon(runnerToken, runnerURL); err != nil {
			return err
		}
	} else if runnerToken != "" {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: ignoring %s without --runner (unset it in your shell)\n", runnerTokenEnv)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Settle the project before the agent bead exists: a project-scoped token
	// can only create a bead labelled with its own project (gd-zQ1SDfKUX5).
	// /v1/me is best-effort; a daemon that cannot answer it leaves the create
	// to report any refusal itself.
	project, _ := cmd.Flags().GetString("project")
	project = resolveProject(project)
	var tokenProject, operatorID string
	if me, meErr := daemon.Me(ctx); meErr == nil {
		tokenProject = me.TokenProject
		operatorID = me.OperatorID
	}
	if project, err = localAgentProject(project, tokenProject); err != nil {
		return err
	}

	// Resolve the session's beads credentials and find coop before the agent
	// bead exists: either one missing used to register an agent that was
	// closed a second later (gd-O28rNijtUM).
	if isLocal {
		if runnerID != "" && runnerToken != "" {
			// The session runs on the runner token, not the owner's
			// credentials (spec §4.2, §6).
			var removeTokenFile func()
			if creds, removeTokenFile, err = runnerSessionCreds(runnerToken, runnerURL); err != nil {
				return err
			}
			defer removeTokenFile()
		} else if adoptID == "" {
			creds = resolveLocalCreds()
			if err := preflightLocalCreds(creds); err != nil {
				return err
			}
		}
		if err := preflightLocalCoop(); err != nil {
			return err
		}
	}

	watchEscalations, _ := cmd.Flags().GetBool("watch-escalations")
	taskID, _ := cmd.Flags().GetString("task")

	if adoptID != "" {
		// Registered, and stopped, by whoever minted its token.
		fmt.Printf("[gb agent start] Running as agent '%s' (bead %s), registered for this session\n", agentName, adoptID)
		defer fmt.Printf("[gb agent start] Agent bead %s is left to the process that registered it.\n", adoptID)
		return runLocal(ctx, cancel, creds, agentName, adoptID, role, dir, agentCommand, watchEscalations, project, taskID, scenarioPath, "", "")
	}

	// 1. Create agent bead.
	fmt.Printf("[gb agent start] Registering agent '%s' with server...\n", agentName)
	createReq := dockerAgentCreateRequest(agentName, project)
	if isLocal {
		// A runner session with no explicit --role takes the local role
		// (spec D6); the legacy non-runner path keeps the crew default.
		if runnerID != "" && !cmd.Flags().Changed("role") {
			role = "local"
		}
		createReq = localAgentCreateRequest(agentName, role, project, runnerID, owner)
	}
	agentBeadID, err := daemon.CreateBead(ctx, createReq)
	if err != nil {
		return fmt.Errorf("create agent bead: %w", err)
	}
	fmt.Printf("[gb agent start] Agent bead: %s\n", agentBeadID)

	// Ensure bead is closed on exit.
	defer func() {
		if !isLocal {
			dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer dcancel()
			if err := daemon.CloseBead(dctx, agentBeadID, nil); err != nil {
				fmt.Fprintf(os.Stderr, "[gb agent start] warning: failed to close agent bead: %v\n", err)
			} else {
				fmt.Printf("[gb agent start] Agent bead %s closed.\n", agentBeadID)
			}
			return
		}
		closeLocalAgentBeadAtExit(daemon, agentName, agentBeadID, freshExitDaemon(runnerID != ""))
	}()

	if isLocal {
		sctx, scancel := context.WithTimeout(ctx, 15*time.Second)
		supersedeStaleLaptopAgentBeads(sctx, daemon, agentName, agentBeadID, []string{owner, operatorID, operatedBy, actor})
		scancel()
	}

	if isLocal {
		return runLocal(ctx, cancel, creds, agentName, agentBeadID, role, dir, agentCommand, watchEscalations, project, taskID, scenarioPath, runnerID, owner)
	}

	imageDir, _ := cmd.Flags().GetString("image-dir")
	imageName, _ := cmd.Flags().GetString("image")
	attach, _ := cmd.Flags().GetBool("attach")
	return runDocker(ctx, agentName, agentBeadID, role, dir, imageDir, imageName, agentCommand, attach, taskID)
}

// checkAdoptedAgent refuses an --agent-id start that could not run as that
// agent: it is a laptop session with the bead's own name, and noSession (the
// oidclogin.NoSessionEnv value) must be 1. The agent's kd and gb inherit this
// process's environment and home, and both send a login session for the
// daemon before any token in the environment, so without it a land agent
// would act as the person who started it (gd-QV2WuUjely).
func checkAdoptedAgent(adoptID string, isLocal, nameExplicit bool, noSession string) error {
	switch {
	case adoptID == "":
		return nil
	case !isLocal:
		return fmt.Errorf("--agent-id is only supported with --local")
	case !nameExplicit:
		return fmt.Errorf("--agent-id needs --name, the agent bead's name")
	case noSession != "1":
		return fmt.Errorf("--agent-id runs an agent on its own token: set %s=1 so neither gb nor the agent's kd and gb send your login session ('gl agent start' sets it)", oidclogin.NoSessionEnv)
	}
	return nil
}

// adoptedAgentCreds is an --agent-id session's credentials: the agent's own
// token (--token, else the environment's) at --http-url. No remote, no login
// session and no anonymous loopback fallback: an agent with no token of its
// own does not start.
func adoptedAgentCreds(url, flagToken, envToken string) (localCreds, error) {
	tok, source := flagToken, "token flag"
	if tok == "" {
		tok, source = envToken, "env"
	}
	if tok == "" {
		return localCreds{}, fmt.Errorf("--agent-id needs the agent's own token: pass --token or set GARDENIO_TOKEN_FILE or GARDENIO_TOKEN")
	}
	if url == "" {
		return localCreds{}, errNoDaemonURL
	}
	return localCreds{HTTPURL: normalizeBeadsURL(url), AuthToken: tok, Source: "agent-id:" + source}, nil
}

// localAgentProject settles the project a local agent registers under.
// kbeads refuses a project-scoped token any bead that lacks its project:<p>
// label (beadMatchesTokenProject), so a scoped token with no project, or with
// a different one, would 403 on the agent-bead create. tokenProject is the
// scope /v1/me reports; "" means unscoped or unknown.
func localAgentProject(project, tokenProject string) (string, error) {
	switch {
	case tokenProject == "" || project == tokenProject:
		return project, nil
	case project == "":
		return "", fmt.Errorf("the beads token is scoped to project %q: pass --project %s (or set KD_PROJECT)", tokenProject, tokenProject)
	default:
		return "", fmt.Errorf("--project %q does not match the beads token's project %q, which the daemon would refuse", project, tokenProject)
	}
}

// localAgentCreateRequest is the agent bead for a laptop session (spec §5,
// §6). It is a type=agent bead, like a pod's, so the roster, cards and
// ListAgentBeads see it; execution_target=laptop tells the controller it runs
// nothing for it, and runtime_id ties it to its runner. operated_by is the
// human whose capabilities it runs with (D6). No cost fields: the owner's own
// Claude login pays (D1). project:<p> is labelled when set, so a
// project-scoped token may create and update it.
func localAgentCreateRequest(agentName, role, project, runnerID, owner string) beadsapi.CreateBeadRequest {
	if role == "" {
		role = "local"
	}
	fields := map[string]string{
		"agent":            agentName,
		"role":             role,
		"project":          project,
		"mode":             "crew",
		"agent_state":      "working",
		"execution_target": "laptop",
	}
	if runnerID != "" {
		fields["runtime_id"] = runnerID
	}
	createdBy := actor
	if owner != "" {
		fields["operated_by"] = owner
		createdBy = owner
	}
	raw, _ := json.Marshal(fields)
	labels := []string{"execution_target:laptop"}
	if project != "" {
		labels = append(labels, "project:"+project)
	}
	return beadsapi.CreateBeadRequest{
		Title:     agentName,
		Type:      "agent",
		Assignee:  agentName,
		CreatedBy: createdBy,
		Labels:    labels,
		Fields:    json.RawMessage(raw),
	}
}

// dockerAgentCreateRequest is the legacy kd:agent task bead a --docker
// session registers under; the controller never sees it as an agent.
func dockerAgentCreateRequest(agentName, project string) beadsapi.CreateBeadRequest {
	labels := []string{"kd:agent"}
	if project != "" {
		labels = append(labels, "project:"+project)
	}
	return beadsapi.CreateBeadRequest{
		Title:     agentName,
		Type:      "task",
		Kind:      "issue",
		Assignee:  agentName,
		CreatedBy: actor,
		Labels:    labels,
	}
}

// ── --local ───────────────────────────────────────────────────────────────

// creds are the session's beads credentials, resolved and preflighted by the
// caller before the agent bead was created.
func runLocal(ctx context.Context, cancel context.CancelFunc, creds localCreds, agentName, agentBeadID, role, dir, agentCommand string, watchEscalations bool, project, taskID, scenarioPath, runnerID, owner string) error {
	fmt.Printf("[gb agent start] beads daemon: %s (creds: %s)\n", creds.HTTPURL, creds.Source)

	// When the controller writes a thaw-epoch marker (gVisor agent pods set
	// COOP_THAW_STATE_DIR), run the gb-side thaw watcher: on thaw it fires an
	// immediate heartbeat (N6). A no-op when the env is unset (the common
	// --local case). See agent_start_thaw.go.
	if thawDir := os.Getenv("COOP_THAW_STATE_DIR"); thawDir != "" {
		startGBThawWatcher(ctx, thawDir, thawDeps{
			heartbeat: func(hctx context.Context) error {
				return emitAgentHook(hctx, agentBeadID, agentName, "PreToolUse", dir)
			},
			// On thaw, evict the daemon HTTP client's keep-alive pool BEFORE
			// the first heartbeat so a conn pooled against Pod-A's dead
			// 4-tuple is never reused on Pod-B's netns (ka-0000001637 RANK3).
			evictIdleConns: func() {
				if daemon != nil {
					daemon.CloseIdleConnections()
				}
			},
			logger: slog.Default(),
		})
	}

	// Find a free port for coop (health probe gets port+1).
	coopPort, err := freePort()
	if err != nil {
		return fmt.Errorf("find free port: %w", err)
	}
	coopURL := fmt.Sprintf("http://127.0.0.1:%d", coopPort)
	fmt.Printf("[gb agent start] Starting coop on port %d...\n", coopPort)

	// Materialize settings via gb setup claude (or gb setup codex).
	// In local mode the daemon is typically unavailable, so this falls
	// back to the *Defaults variant which only writes workspace config.
	// local=true confines writes to <dir>/.claude/ (or <dir>/.codex/) and
	// <dir>/.mcp.json so the operator's own ~/.claude/ and ~/.codex/ are never
	// touched (ka-0000002300).
	if harness.IsCodex(harness.MustFromEnv()) {
		fmt.Printf("[gb agent start] Materializing config (gb setup codex)...\n")
		if err := runSetupCodex(ctx, dir, role, true); err != nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] config beads not found, installing codex defaults...\n")
			if err2 := runSetupCodexDefaults(dir, role, true); err2 != nil {
				fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not write .codex/config.toml: %v\n", err2)
			}
		}
	} else if harness.IsGemini(harness.MustFromEnv()) {
		fmt.Printf("[gb agent start] Materializing config (gb setup gemini)...\n")
		if err := runSetupGemini(ctx, dir, role, true); err != nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] config beads not found, installing gemini defaults...\n")
			if err2 := runSetupGeminiDefaults(dir, role, true); err2 != nil {
				fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not write .gemini/settings.json: %v\n", err2)
			}
		}
	} else {
		fmt.Printf("[gb agent start] Materializing hooks (gb setup claude)...\n")
		if err := runSetupClaude(ctx, dir, role, true); err != nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] config beads not found, installing defaults...\n")
			if err2 := runSetupClaudeDefaults(dir, role, true); err2 != nil {
				fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not write .claude/settings.local.json: %v\n", err2)
			}
		}
		// An unanswered folder-trust dialog makes claude exit 1 (gd-bJeLOYSWZG).
		if scenarioPath == "" {
			if err := preTrustClaudeDir(dir); err != nil {
				fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not pre-trust %s (%v); if Claude Code asks whether you trust this folder, answer yes or open claude there once\n", dir, err)
			}
		}
	}

	// Build env for the coop+claude subprocess.
	env := localAgentEnv(agentName, agentBeadID, role, dir, coopURL, taskID, creds)
	if project != "" {
		// The agent's own kd/gb calls ride the same token, so they need the
		// project too when it came from --project rather than the env.
		env = append(env, "KD_PROJECT="+project, "BOAT_PROJECT="+project)
	}
	if harness.IsGemini(harness.MustFromEnv()) {
		// Local mode wrote <dir>/.gemini (runSetupGemini local=true); point the
		// CLI at it so the operator's own ~/.gemini is never read or touched.
		env = append(env, "GEMINI_CLI_HOME="+dir)
	}
	// In test-scenario mode, scrub COOP_S3_* env vars inherited from the parent.
	// Otherwise coop tries to restore a prior session from S3 and injects
	// `--resume <id>` into the claudeless 0.4.0 command line, which it rejects —
	// claudeless exits ~500ms after launch, the startup-prompt nudge is queued
	// against a dead agent, and scenario response rules never fire.
	//
	// Also allocate the scenario result file (ka-0000006600) and export
	// GB_SCENARIO_RESULT_FILE so the body can write its pass/fail verdict.
	// Without this, scenario bash `exit 1` is invisible to gb-agent because
	// it only kills the bash subprocess inside a claudeless tool call.
	var scenarioResultPath string
	if scenarioPath != "" {
		env = stripS3EnvForScenario(env)
		path, cleanup, err := allocScenarioResultFile()
		if err != nil {
			return err
		}
		defer cleanup()
		scenarioResultPath = path
		env = append(env, scenarioResultEnv+"="+path)
		fmt.Printf("[gb agent start] Scenario result file: %s\n", path)
	}

	// Start coop as a background daemon.
	coopProc := exec.CommandContext(ctx, "coop",
		"--agent="+harness.CoopAgent(harness.MustFromEnv(), agentCommand),
		fmt.Sprintf("--port=%d", coopPort),
		fmt.Sprintf("--port-health=%d", coopPort+1),
		"--cols=220", "--rows=50",
		"--", "sh", "-c", agentCommand,
	)
	coopProc.Dir = dir
	coopProc.Env = env
	coopProc.Stdout = os.Stderr
	coopProc.Stderr = os.Stderr
	// Send SIGTERM on context cancellation instead of the default SIGKILL.
	coopProc.Cancel = func() error {
		if coopProc.Process != nil {
			fmt.Printf("[gb agent start] sending SIGTERM to coop (pid %d)\n", coopProc.Process.Pid)
			return coopProc.Process.Signal(syscall.SIGTERM)
		}
		return nil
	}
	coopProc.WaitDelay = 20 * time.Second
	// Place coop in its own process group so a pgid-targeted signal aimed at
	// gb (e.g. operator Ctrl-C in a foreground shell, supervisor sending to
	// the runner's pgid) does NOT also hit coop directly. We want the single
	// SIGTERM dispatched by Cancel above to be the only one coop sees —
	// otherwise coop's two-phase signal handler treats the duplicate as
	// "force exit" and SIGKILLs the child shell before its TERM trap runs.
	coopProc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := coopProc.Start(); err != nil {
		return fmt.Errorf("start coop: %w", err)
	}
	fmt.Printf("[gb agent start] Coop started (pid %d)\n", coopProc.Process.Pid)

	// Register in on-disk local registry so `gb agent list --local` can
	// enumerate spawns without hitting the daemon. Best-effort: a failure
	// here is logged but does not abort the spawn.
	if regPath, regErr := localreg.Register(localreg.Entry{
		Name:     agentName,
		BeadID:   agentBeadID,
		Role:     role,
		Project:  project,
		Dir:      dir,
		PID:      coopProc.Process.Pid,
		CoopURL:  coopURL,
		RunnerID: runnerID,
		Owner:    owner,
	}); regErr != nil {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: localreg register failed: %v\n", regErr)
	} else {
		fmt.Printf("[gb agent start] Local registry: %s\n", regPath)
	}
	defer func() { _ = localreg.Deregister(agentName) }()

	// Ensure coop is stopped if we exit early.
	defer func() {
		if coopProc.ProcessState == nil {
			_ = coopProc.Process.Signal(syscall.SIGTERM)
			_ = coopProc.Wait()
		}
	}()

	// Wait for coop health.
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", coopPort+1)
	if err := waitForHealth(ctx, healthURL, 30*time.Second); err != nil {
		return fmt.Errorf("coop health check: %w", err)
	}

	// Emit SessionStart hook.
	if err := emitAgentHook(ctx, agentBeadID, agentName, "SessionStart", dir); err != nil {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: SessionStart emit failed: %v\n", err)
	}

	// Inject startup prompt (test-scenario only). Mirrors K8s
	// runSessionLifecycle: wait for the agent to reach idle/working, clear
	// any stale input with Ctrl-U, then deliver the nudge via coop.Nudge with
	// a small retry. Runs in parallel with `coop attach` below so the
	// interactive TTY is unaffected. Non-scenario interactive runs keep their
	// current UX (no injection).
	// Tracks whether scenario-mode self-exit asked coop to shut down. When
	// set, a non-zero coop exit reflects clean teardown — coop SIGTERMs the
	// child as part of /api/v1/shutdown (ka-0000007677) and propagates the
	// child's non-zero exit, but we initiated it so it isn't a failure.
	var scenarioShutdownRequested atomic.Bool
	if startupPrompt := resolveStartupPrompt(scenarioPath); startupPrompt != "" {
		coopClient := coopapi.New(coopURL)
		followups := resolveNudges(scenarioPath)
		sidecar := nudgesSidecarPath(scenarioPath)
		go func() {
			runLocalNudgeSequence(ctx, coopClient, startupPrompt, followups, sidecar)
			// Scenario mode is the only path that sets a startup prompt
			// today (resolveStartupPrompt returns "" without a sidecar,
			// which only test scenarios ship). Self-exit by asking coop
			// to shut down once the final nudge has drained — otherwise
			// runLocal would block on coopProc.Wait() forever and the
			// harness has to send SIGTERM at 90s (ka-0000003566).
			//
			// Mark intent BEFORE the Shutdown POST so the main waiter
			// always sees the flag set before coopProc.Wait() returns.
			scenarioShutdownRequested.Store(true)
			requestScenarioShutdown(ctx, coopClient)
		}()
	} else {
		// A --command session (gb runner join) has no startup prompt, but
		// claudeless still opens on the bypass_permissions dialog (gd-inbwLZfEYe).
		go bypassLocalStartup(ctx, coopapi.New(coopURL))
	}

	// Start heartbeat in background.
	go agentHeartbeatLoop(ctx, agentBeadID, agentName, dir)

	// coop outlives an exited agent (it waits for a switch or shutdown), so
	// shut it down ourselves; otherwise the registry entry and bead stay ALIVE
	// for a dead agent (gd-bJeLOYSWZG). Scenario mode owns its own shutdown.
	if scenarioPath == "" {
		go stopCoopWhenAgentExits(ctx, coopapi.New(coopURL), localAgentExitPoll)
	}

	// The escalation executor (agent_start_escalation.go) runs the local
	// escalations the daemon approves for this runner: it is on whenever the
	// agent joined a runner, since execution_target=local names a runner and
	// the daemon mails the agent on it (spec §8.1). --watch-escalations no
	// longer selects anything. The executor reads /v1/escalations/{id}, which
	// a land door does not serve (gd-2iqSr9ANOH).
	var escalations *escalationWatcher
	switch {
	case watchEscalations && runnerID == "":
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: --watch-escalations is deprecated and does nothing without --runner: a local escalation runs only on the runner it names (join one with `gb runner join`)\n")
	case runnerID != "" && landDoor() != "":
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: local escalations are not run through a land door; escalation executor not started\n")
	case runnerID != "":
		escalations = newEscalationWatcher(escalationWatcherConfig{
			Daemon:    daemon,
			CoopURL:   coopURL,
			AgentName: agentName,
			RunnerID:  runnerID,
			Logger:    slog.Default(),
		})
		fmt.Printf("[gb agent start] Escalation executor started (runner %s)\n", runnerID)
	}
	// The subscriber claims the runner's parked mail only when an executor
	// will run it: behind a land door none does, and a claimed mail would be
	// re-addressed to this agent, left open, and stranded from any agent that
	// joins the runner later.
	var onEscalation func(context.Context, string, beadsapi.BeadDetail)
	subRunnerID := ""
	if escalations != nil {
		onEscalation = escalations.HandleControl
		subRunnerID = runnerID
	}

	// Subscribe to this agent's own mail and deliver it to coop on loopback
	// (spec §3.1): one outbound stream, nothing dials in. control:kill
	// cancels ctx, the same clean exit as Ctrl-C (coop gets SIGTERM, the
	// deferred CloseBead runs); control:permission goes to the loopback
	// gardenio-channel server (mail_permission.go); control:escalation-approved
	// goes to the escalation executor. With an executor it also claims mail
	// the daemon parked for the runner while no agent was joined on it.
	mailSub := mailsub.New(mailsub.Config{
		Daemon:    daemon,
		Coop:      coopapi.New(coopURL),
		AgentBead: agentBeadID,
		AgentName: agentName,
		RunnerID:  subRunnerID,
		Logger:    slog.Default(),
		OnControl: localControlHandler(onEscalation, newPermissionControl(daemon, func(format string, args ...any) {
			fmt.Printf("[gb agent start] "+format+"\n", args...)
		}).OnControl),
		Exit: cancel,
		Poll: localMailPoll(),
	})
	go func() {
		fmt.Printf("[gb agent start] Mail subscriber started (agent bead %s)\n", agentBeadID)
		if err := mailSub.Run(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] mail subscriber stopped: %v\n", err)
		}
	}()

	// Attach an interactive terminal.
	fmt.Printf("[gb agent start] Attaching to session (detach: ctrl-\\ )...\n")
	attachProc := exec.CommandContext(ctx, "coop", "attach", coopURL)
	attachProc.Stdin = os.Stdin
	attachProc.Stdout = os.Stdout
	attachProc.Stderr = os.Stderr
	_ = attachProc.Run()

	// After detach: wait for coop session to finish naturally.
	fmt.Printf("[gb agent start] Detached. Coop session continues in background.\n")
	fmt.Printf("[gb agent start] Re-attach: coop attach %s\n", coopURL)
	fmt.Printf("[gb agent start] Agent bead: %s\n", agentBeadID)

	waitErr := coopProc.Wait()

	// Emit Stop hook (best-effort).
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	_ = emitAgentHook(stopCtx, agentBeadID, agentName, "Stop", dir)

	if waitErr != nil && ctx.Err() == nil {
		if scenarioShutdownRequested.Load() {
			// We told coop to shut down (scenario-mode self-exit). Coop
			// SIGTERMs the child and its exit status surfaces here as
			// non-zero — that's clean teardown from coop's perspective
			// (ka-0000007173). Per ka-0000006600 the "clean" rule is now
			// narrowed: the scenario body must also have signalled pass
			// via the result file. Otherwise we propagate non-zero so CI
			// can see real regressions instead of silent-green smokes.
			if resultErr := readScenarioResult(scenarioResultPath); resultErr != nil {
				fmt.Fprintf(os.Stderr, "[gb agent start] coop exited %v after scenario-mode shutdown, but scenario body failed: %v\n", waitErr, resultErr)
				return resultErr
			}
			fmt.Printf("[gb agent start] coop exited %v after scenario-mode shutdown — scenario passed (treated as clean)\n", waitErr)
			return nil
		}
		return fmt.Errorf("coop exited: %w", waitErr)
	}
	// Even when coop exits cleanly (waitErr == nil) we still want a positive
	// pass signal from the scenario body in scenario mode — a green coop
	// exit alone does not mean the assertions ran successfully.
	if scenarioResultPath != "" && ctx.Err() == nil {
		if resultErr := readScenarioResult(scenarioResultPath); resultErr != nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] coop exited cleanly but scenario body failed: %v\n", resultErr)
			return resultErr
		}
	}
	return nil
}

// ── --docker ──────────────────────────────────────────────────────────────

func runDocker(ctx context.Context, agentName, agentBeadID, role, dir, imageDir, imageName string, agentCommand string, attach bool, taskID string) error {
	if imageName == "" {
		if imageDir == "" {
			candidates := []string{
				filepath.Join(os.Getenv("HOME"), "gardenio", "images", "agent"),
				"./images/agent",
			}
			for _, c := range candidates {
				if _, err := os.Stat(filepath.Join(c, "Dockerfile")); err == nil {
					imageDir = c
					break
				}
			}
			if imageDir == "" {
				return fmt.Errorf("could not find gardenio agent Dockerfile; use --image-dir or --image")
			}
		}

		imageName = "gb-agent-local:latest"
		fmt.Printf("[gb agent start] Building agent image from %s...\n", imageDir)
		buildCmd := exec.CommandContext(ctx, "docker", "build", "-t", imageName, imageDir)
		buildCmd.Stdout = os.Stdout
		buildCmd.Stderr = os.Stderr
		if err := buildCmd.Run(); err != nil {
			return fmt.Errorf("docker build: %w", err)
		}
		fmt.Printf("[gb agent start] Image built: %s\n", imageName)
	}

	beadsURL := os.Getenv("BEADS_HTTP_URL")
	if beadsURL == "" {
		beadsURL = httpURL
	}

	runArgs := []string{
		"run", "--rm",
		"--name", containerName(agentName),
		"-v", dir + ":/home/agent/workspace",
		"-e", "ANTHROPIC_API_KEY=" + os.Getenv("ANTHROPIC_API_KEY"),
		"-e", "BEADS_HTTP_URL=" + beadsURL,
		"-e", "BEADS_HTTP_ADDR=" + beadsURL,
		"-e", "KD_AGENT_ID=" + agentBeadID,
		"-e", "KD_ACTOR=" + agentName,
		"-e", "BOAT_ROLE=" + role,
		"-e", "BOAT_AGENT=" + agentName,
		"-e", "BOAT_AGENT_BEAD_ID=" + agentBeadID,
		"-e", "BOAT_COMMAND=" + agentCommand,
	}

	if taskID != "" {
		runArgs = append(runArgs, "-e", "BOAT_TASK_ID="+taskID)
	}

	for _, k := range []string{
		"COOP_MUX_URL", "COOP_MUX_AUTH_TOKEN",
		"GIT_AUTHOR_NAME", "GIT_USERNAME", "GIT_TOKEN",
		"BEADS_DAEMON_TOKEN",
	} {
		if v := os.Getenv(k); v != "" {
			runArgs = append(runArgs, "-e", k+"="+v)
		}
	}

	if attach {
		runArgs = append(runArgs, imageName)
		fmt.Printf("[gb agent start] Running container '%s' (attached)...\n", containerName(agentName))
		dockerCmd := exec.CommandContext(ctx, "docker", runArgs...)
		dockerCmd.Stdout = os.Stdout
		dockerCmd.Stderr = os.Stderr
		dockerCmd.Stdin = os.Stdin

		go func() {
			time.Sleep(5 * time.Second)
			_ = emitAgentHook(ctx, agentBeadID, agentName, "SessionStart", dir)
			agentHeartbeatLoop(ctx, agentBeadID, agentName, dir)
		}()

		waitErr := dockerCmd.Run()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = emitAgentHook(stopCtx, agentBeadID, agentName, "Stop", dir)
		return waitErr
	}

	// Detached mode.
	runArgs = append([]string{runArgs[0], "-d"}, runArgs[1:]...)
	runArgs = append(runArgs, imageName)

	fmt.Printf("[gb agent start] Starting container '%s' (detached)...\n", containerName(agentName))
	out, err := exec.CommandContext(ctx, "docker", runArgs...).Output()
	if err != nil {
		return fmt.Errorf("docker run: %w", err)
	}
	containerID := strings.TrimSpace(string(out))
	fmt.Printf("[gb agent start] Container started: %s\n", containerID[:12])
	fmt.Printf("[gb agent start] Agent bead:       %s\n", agentBeadID)
	fmt.Printf("[gb agent start] Logs:              docker logs -f %s\n", containerName(agentName))
	fmt.Printf("[gb agent start] Stop:              docker stop %s\n", containerName(agentName))

	go func() {
		time.Sleep(5 * time.Second)
		_ = emitAgentHook(context.Background(), agentBeadID, agentName, "SessionStart", dir)
	}()

	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────

// localAgentEnv builds the environment slice for a --local coop subprocess.
// When taskID is non-empty, BOAT_TASK_ID is exported so the nudge prompt pre-assigns
// the task (mirrors the K8s path; see podmanager.manager Spec.TaskID).
//
// creds carries the resolved beads URL + bearer token (see resolveLocalCreds).
// We export BEADS_AUTH_TOKEN explicitly so child `kd`/`gb` invocations from
// hooks pick it up, and we export COOP_NATS_TOKEN so coop's NATS WebSocket
// upgrade authenticates against the remote's nats_url, when one is set.
//
// The launching Claude Code session's own env is dropped (see
// isLaunchingClaudeSessionEnv) so the agent never joins that session.
func localAgentEnv(agentName, agentBeadID, role, dir, coopAddr, taskID string, creds localCreds) []string {
	beadsURL := creds.HTTPURL

	env := os.Environ()
	overrides := map[string]string{
		"KD_AGENT_ID":           agentBeadID,
		"KD_ACTOR":              agentName,
		"BOAT_ROLE":             role,
		"BOAT_AGENT":            agentName,
		"BOAT_AGENT_BEAD_ID":    agentBeadID,
		"BEADS_HTTP_URL":        beadsURL,
		"BEADS_HTTP_ADDR":       beadsURL,
		"BEADS_DAEMON_HTTP_URL": beadsURL,
		"XDG_STATE_HOME":        filepath.Join(dir, ".state"),
		"COOP_NATS_URL":         creds.NATSURL,
		// Split subject spaces (ka-0000005381): the coopmux discovery relay rides
		// coop.mux.session.>, while general events + message forwarding stay on
		// coop.events.> (the space the beads-viewer subscribes to and the NATS
		// agent grant allows). A single prefix on coop.mux would push
		// state/messages.* onto coop.mux.* — unsubscribed and an auth violation.
		"COOP_NATS_PREFIX":       "coop.events",
		"COOP_NATS_RELAY_PREFIX": "coop.mux",
		"COOP_NATS_RELAY":        "1",
		"COOP_NATS_FORWARD":      "1",
		"COOP_URL":               coopAddr,
		// The stop gate leaves the agent bead to this process while it lives
		// (gd-3s1xZJyeNI).
		localAgentStartPIDEnv: strconv.Itoa(os.Getpid()),
	}
	addLocalTokenOverrides(overrides, creds)
	if taskID != "" {
		overrides["BOAT_TASK_ID"] = taskID
	}

	// In --local mode there is no coopmux sidecar to talk to. If the operator
	// did not explicitly set COOP_MUX_URL, disable mux registration entirely
	// by passing an empty value so coop's mux client never spins up — this
	// avoids the shutdown-time "mux deregistration failed" WARN against the
	// 127.0.0.1:9800 default (ka-0000002670). Operators who want to point a
	// local coop at a real mux can still set COOP_MUX_URL explicitly.
	if _, set := os.LookupEnv("COOP_MUX_URL"); !set {
		overrides["COOP_MUX_URL"] = ""
	}

	// Keys dropped from the parent env and not set at all.
	unset := map[string]struct{}{}
	// Without an explicit remote nats_url, leave coop's NATS off. Nothing
	// serves <beads URL>/nats: kbeads has no such route and the chart's opt-in
	// natsWebsocketRoute is off everywhere, so coop would retry a refused
	// upgrade over the attached screen on the token path as on the owner
	// login (gd-DTyKMgQXyz). The agent's inbound path is its mail stream
	// (spec D2).
	if creds.NATSURL == "" {
		for _, k := range []string{"COOP_NATS_URL", "COOP_NATS_PREFIX", "COOP_NATS_RELAY_PREFIX", "COOP_NATS_RELAY", "COOP_NATS_FORWARD"} {
			delete(overrides, k)
			unset[k] = struct{}{}
		}
	}

	// Credential keys are always controlled by `creds`: a parent process must
	// not leak a token into the spawned coop session when we resolved no token
	// (e.g. loopback dev daemon allowing anonymous access — exporting an empty
	// or unrelated token there would actively break the connection).
	// The other names kd and gb read a token from go too: GARDENIO_TOKEN_FILE
	// and GARDENIO_TOKEN outrank BEADS_AUTH_TOKEN, so one exported in the
	// owner's shell would replace the token resolved here.
	credKeys := map[string]struct{}{
		"BEADS_AUTH_TOKEN":   {},
		"BEADS_DAEMON_TOKEN": {},
		"COOP_NATS_TOKEN":    {},
		"GARDENIO_TOKEN":     {},
		"KBEADS_TOKEN":       {},
		tokenFileEnv:         {},
	}

	filtered := env[:0]
	for _, e := range env {
		key := strings.SplitN(e, "=", 2)[0]
		if _, skip := overrides[key]; skip {
			continue
		}
		if _, skip := credKeys[key]; skip {
			continue
		}
		if _, skip := unset[key]; skip {
			continue
		}
		if isLaunchingClaudeSessionEnv(e) {
			continue
		}
		filtered = append(filtered, e)
	}
	for k, v := range overrides {
		filtered = append(filtered, k+"="+v)
	}
	return filtered
}

// addLocalTokenOverrides exports the resolved credential under every name
// the agent stack reads:
//   - GARDENIO_TOKEN_FILE: creds.TokenFile, which kd and gb read ahead of
//     every token variable
//   - BEADS_AUTH_TOKEN: canonical name read by kd / gb hooks
//   - BEADS_DAEMON_TOKEN: legacy fallback the K8s controller still emits
//   - COOP_NATS_TOKEN: bearer auth on coop's NATS WS upgrade to an
//     explicit nats_url
//
// and sets oidclogin.NoSessionEnv, so the session runs on this credential,
// not the owner's: kd and gb send a login session from the shared HOME ahead
// of any token in the env, so without it the owner's `gb login` would sign
// every write the moment BEADS_HTTP_URL names the daemon it was minted for
// (gd-2bK7yhURFo). With neither (OwnerLogin) nothing is set and the session
// keeps the owner's login by design (gd-O28rNijtUM).
func addLocalTokenOverrides(overrides map[string]string, creds localCreds) {
	if creds.TokenFile != "" {
		overrides[tokenFileEnv] = creds.TokenFile
		overrides[oidclogin.NoSessionEnv] = "1"
	}
	if creds.AuthToken == "" {
		return
	}
	overrides["BEADS_AUTH_TOKEN"] = creds.AuthToken
	overrides["BEADS_DAEMON_TOKEN"] = creds.AuthToken
	overrides["COOP_NATS_TOKEN"] = creds.AuthToken
	overrides[oidclogin.NoSessionEnv] = "1"
}

// isLaunchingClaudeSessionEnv reports whether kv belongs to the Claude Code
// session that ran `gb agent start --local` (gd-6Nrazv7Waz). An operator who
// launches from inside Claude Code, or from a tmux server started there,
// carries CLAUDECODE, AI_AGENT and CLAUDE_CODE_* (session id, messaging
// socket, bridge session id, CLAUDE_CODE_CHILD_SESSION); passed through,
// the agent's claude registers as a child of the operator's session and can
// receive peer messages meant for the operator. CLAUDE_CODE_* keys use the
// same provider-auth/routing allowlist as the K8s coop env
// (isDeniedClaudeCodeEnv), so an exported CLAUDE_CODE_OAUTH_TOKEN or
// CLAUDE_CODE_USE_BEDROCK still reaches the agent.
func isLaunchingClaudeSessionEnv(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	switch key {
	case "CLAUDECODE", "AI_AGENT":
		return true
	}
	return isDeniedClaudeCodeEnv(kv)
}

// stripS3EnvForScenario removes COOP_S3_* keys from an env slice. Used in
// --test-scenario mode so the spawned coop does not attempt to restore a
// session from S3 and inject `--resume <id>` into the claudeless command
// line (claudeless 0.4.0 silently exits on unknown args). Test scenarios
// must always start with a clean session.
func stripS3EnvForScenario(env []string) []string {
	out := env[:0]
	for _, e := range env {
		key := strings.SplitN(e, "=", 2)[0]
		if strings.HasPrefix(key, "COOP_S3_") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// emitAgentHook sends a hook event to the kbeads server. A land door serves
// no hook route (gd-2iqSr9ANOH): there the door records the agent's presence
// from its own traffic, so SessionStart and the heartbeat are a GET /v1/me,
// and Stop sends nothing (gb closes the agent bead on exit).
func emitAgentHook(ctx context.Context, agentBeadID, agentName, hookType, cwd string) error {
	if landDoor() != "" {
		if hookType == "Stop" {
			return nil
		}
		_, err := daemon.Me(ctx)
		return err
	}
	_, err := daemon.EmitHook(ctx, beadsapi.EmitHookRequest{
		AgentBeadID: agentBeadID,
		HookType:    hookType,
		Actor:       agentName,
		CWD:         cwd,
	})
	return err
}

// landDoorMailPoll is how often a local agent behind a land door drains its
// mail: the door serves no event stream and the land feed carries no mail.
const landDoorMailPoll = 30 * time.Second

// localMailPoll is the mail subscriber's poll interval: landDoorMailPoll
// behind a land door, else 0 (stream).
func localMailPoll() time.Duration {
	if landDoor() != "" {
		return landDoorMailPoll
	}
	return 0
}

// agentHeartbeatLoop emits periodic hook events to keep the agent alive in the
// presence tracker (reaper threshold: 15 min).
func agentHeartbeatLoop(ctx context.Context, agentBeadID, agentName, cwd string) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = emitAgentHook(ctx, agentBeadID, agentName, "PreToolUse", cwd)
		}
	}
}

// waitForHealth polls an HTTP health endpoint until it returns 200 or times out.
func waitForHealth(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if reqErr != nil {
			return reqErr
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", url)
}

// freePort finds a free TCP port on localhost.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

// containerName returns a stable Docker container name for an agent.
func containerName(agentName string) string {
	safe := strings.NewReplacer(" ", "-", "/", "-", ":", "-").Replace(agentName)
	return "gb-agent-" + safe
}

// resolveScenarioPath turns a --scenario value into an absolute .toml path.
//
// Resolution order:
//  1. If value is already a path (contains '/' or ends in .toml) — treat as a path
//     (absolutize relative to dir if not already absolute). Must exist.
//  2. If scenarioDir is set — look for <scenarioDir>/<name>.toml.
//  3. Fall back to <dir>/gardenio/tests/e2e/claudeless/<name>.toml
//     (monorepo layout), then <dir>/tests/e2e/claudeless/<name>.toml
//     (component-local layout).
func resolveScenarioPath(value, scenarioDir, dir string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("empty scenario")
	}

	looksLikePath := strings.Contains(value, "/") || strings.HasSuffix(value, ".toml")
	if looksLikePath {
		p := value
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		p, _ = filepath.Abs(p)
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("scenario file not found: %s: %w", p, err)
		}
		return p, nil
	}

	name := value + ".toml"
	var candidates []string
	if scenarioDir != "" {
		d := scenarioDir
		if !filepath.IsAbs(d) {
			d = filepath.Join(dir, d)
		}
		candidates = append(candidates, filepath.Join(d, name))
	} else {
		candidates = append(candidates,
			filepath.Join(dir, "gardenio", "tests", "e2e", "claudeless", name),
			filepath.Join(dir, "tests", "e2e", "claudeless", name),
		)
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}
	return "", fmt.Errorf("scenario %q not found in: %s", value, strings.Join(candidates, ", "))
}

// shellQuote wraps a string in single quotes for safe use in `sh -c`.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
