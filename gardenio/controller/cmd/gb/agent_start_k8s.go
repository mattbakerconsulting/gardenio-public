package main

// agent_start_k8s.go — gb agent start --k8s
//
// Replaces entrypoint.sh as the PID-1 process in the K8s agent pod.
// Handles all workspace setup, credential provisioning, coop startup,
// background goroutines, signal forwarding, and the restart loop.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
	"gardenio/controller/internal/harness"

	"github.com/spf13/cobra"
)

// k8sConfig holds all configuration for the K8s agent start command,
// resolved from flags and environment variables at startup.
type k8sConfig struct {
	workspace       string
	coopPort        int
	coopHealthPort  int
	coopMetricsPort int
	maxRestarts     int
	command         string
	sessionResume   bool

	// from env
	role     string
	project  string
	agent    string
	podIP    string
	hostname string
}

func runAgentStartK8s(cmd *cobra.Command, args []string) error {
	workspace, _ := cmd.Flags().GetString("workspace")
	coopPort, _ := cmd.Flags().GetInt("coop-port")
	coopHealthPort, _ := cmd.Flags().GetInt("coop-health-port")
	coopMetricsPort, _ := cmd.Flags().GetInt("coop-metrics-port")
	maxRestarts, _ := cmd.Flags().GetInt("max-restarts")
	if maxRestarts == 0 {
		maxRestarts = intEnvOr("COOP_MAX_RESTARTS", 10)
	}

	// An unknown GARDENIO_HARNESS is an error, never a silent fallback to claude.
	h, err := harness.FromEnv()
	if err != nil {
		return err
	}

	hostname := resolveMuxSessionID()
	podIP := os.Getenv("POD_IP")
	if podIP == "" {
		podIP = "localhost"
	}

	cfg := k8sConfig{
		workspace:       workspace,
		coopPort:        coopPort,
		coopHealthPort:  coopHealthPort,
		coopMetricsPort: coopMetricsPort,
		maxRestarts:     maxRestarts,
		command:         envOr("BOAT_COMMAND", harness.DefaultCommand(h)),
		sessionResume:   envOr("BOAT_SESSION_RESUME", "1") == "1",
		role:            envOr("BOAT_ROLE", "unknown"),
		project:         os.Getenv("BOAT_PROJECT"),
		agent:           envOr("BOAT_AGENT", "unknown"),
		podIP:           podIP,
		hostname:        hostname,
	}

	fmt.Printf("[gb agent start] starting %s agent (mode: k8s): %s (project: %s)\n",
		cfg.role, cfg.agent, orStr(cfg.project, "none"))

	// ── One-time setup (idempotent on restart) ──────────────────────────

	if err := setupWorkspace(cfg); err != nil {
		return fmt.Errorf("workspace setup: %w", err)
	}

	claudeStateDir, err := setupPVC(cfg)
	if err != nil {
		return fmt.Errorf("PVC setup: %w", err)
	}

	stateDir := cfg.workspace + "/.state"
	coopStateDir := stateDir + "/coop"

	credMode := provisionCredentials(claudeStateDir)

	// Materialize user settings + workspace hooks from config beads.
	// runSetupClaude writes user-level settings (from beads or defaults)
	// and workspace hooks to workspace/.claude/settings.local.json.
	if isCodexHarness() {
		setupCodexSettings(workspace, cfg.role, false)
	} else if isGeminiHarness() {
		setupGeminiSettings(workspace, cfg.role, false)
	} else if err := runSetupClaude(context.Background(), workspace, cfg.role, false); err != nil {
		fmt.Printf("[gb agent start] config beads not found, installing default hooks\n")
		if err2 := runSetupClaudeDefaults(workspace, cfg.role, false); err2 != nil {
			fmt.Printf("[gb agent start] warning: could not write workspace .claude/settings.local.json: %v\n", err2)
		}
	}

	writeOnboardingSkip(resolveCoopWorkdir(cfg), cfg.workspace)

	// ── Context + signal handling ────────────────────────────────────────

	// SIGHUP is included so that pod/shell hangups still run the closeSessionBead
	// path below (see ka-0000008184 — orphan FSM-* sessions). Without it, exit=129
	// terminates the process before the restart loop's defer/cleanup runs.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()

	// ── Terminated-mid-session fallback wrap-up (Fix C, ka-0000007005) ────
	// A SIGTERM/SIGINT/SIGHUP graceful exit (deploy roll, node drain, pod
	// eviction) drops out of the restart loop via `return nil` while the agent
	// bead is still OPEN — these agents are meant to resume, so the loop does
	// NOT close them. Without a wrap-up the resumed pod (and the Slack card)
	// see an abrupt end-of-conversation instead of "here's where I was". This
	// deferred best-effort write synthesizes a "Pod terminated mid-session"
	// fallback wrap-up onto the STILL-OPEN agent bead when it carries none, so
	// a restart reads the last progress. It only fires on the ctx-cancelled
	// (graceful shutdown) path, never overwrites a real wrap-up, and never
	// closes the bead. Bounded well inside the K8s termination grace period.
	agentBeadForWrapUp := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	defer writeTerminatedMidSessionWrapUp(ctx, agentBeadForWrapUp)

	// ── Mux registration ─────────────────────────────────────────────────

	mux := newMuxClient()
	coopURL := fmt.Sprintf("http://%s:%d", cfg.podIP, cfg.coopPort)
	go func() {
		if err := mux.Register(ctx, cfg.hostname, coopURL, cfg.role, cfg.agent, cfg.project, cfg.hostname, cfg.podIP); err != nil {
			fmt.Printf("[gb agent start] mux register error: %v\n", err)
		}
	}()
	defer mux.Deregister(cfg.hostname)

	// ── gb-side thaw watcher (ConsumerGB) ────────────────────────────────
	// On gVisor agent pods (COOP_THAW_STATE_DIR set), arm thaw-epoch.gb.armed at
	// boot and ack thaw-epoch.gb.done at thaw so the controller's {gb,coop}
	// rehydrate gate can release. Mirrors the runLocal call site (agent_start.go);
	// a no-op on non-gVisor pods. See agent_start_thaw.go and bug ka-0000004330.
	startK8sGBThawWatcher(
		ctx,
		envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID")),
		cfg.agent,
		resolveCoopWorkdir(cfg),
	)

	// ── OAuth refresh (survives coop restarts) ───────────────────────────

	go oauthRefreshLoop(ctx, claudeStateDir, credMode)

	// ── GitHub credential refresh (rotating installation tokens) ─────────
	// Keeps ~/.git-credentials and gh's hosts.yml in step with the projected
	// Secret the controller rotates (gd-uPJ6AEsfyn).

	startGitCredsRefresh(ctx, func(format string, args ...any) {
		fmt.Printf("[gb agent start] "+format+"\n", args...)
	})

	// ── Restart loop ──────────────────────────────────────────────────────

	const minRuntimeSecs = 30
	restarts := 0

	// Mail subscriber (spec §3.1), shared with gb init: see init_agent_mail.go.
	mail := startPodMail(ctx, initMailDaemon(), mailCoop(coopapi.New(fmt.Sprintf("http://localhost:%d", cfg.coopPort)), cfg.command),
		envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID")), cfg.agent, func(format string, args ...any) {
			fmt.Printf("[gb agent start] "+format+"\n", args...)
		})
	defer mail.stop()

	for {
		if ctx.Err() != nil {
			return nil // SIGTERM/SIGINT/SIGHUP — clean exit
		}
		if restarts >= cfg.maxRestarts {
			return fmt.Errorf("max restarts (%d) reached", cfg.maxRestarts)
		}

		cleanStalePipes(coopStateDir)
		reconcileOrphanedSessions(ctx, cfg.agent)
		resumeLog := findResumeSession(claudeStateDir, cfg.sessionResume, true)
		// gb init is the single authority on resume — when it decides fresh
		// (no eligible local log), purge any leftover .jsonl so Claude Code
		// can't auto-resume from it. See ka-0000004439.
		if resumeLog == "" {
			cleanAllSessionLogs(claudeStateDir)
		}

		sessionBeadID := registerSessionBead(ctx, cfg, resumeLog)

		start := time.Now()

		// Periodic in-session metrics flush (ka-0000003327): push
		// gardenio_session_* every few minutes while coop runs so turn-count
		// and context-size are real time series, not one close-time sample.
		// The final (authoritative) flush still happens in closeSessionBead.
		flushCtx, flushCancel := context.WithCancel(ctx)
		go sessionMetricsFlushLoop(flushCtx, sessionMetricsLabels{
			AgentName:  envOr("BOAT_AGENT", cfg.agent),
			Project:    cfg.project,
			Account:    os.Getenv("BOAT_ACCOUNT"),
			SessionID:  sessionBeadID,
			OperatorID: resolveOperatorID(ctx),
			Model:      os.Getenv("CLAUDE_MODEL"),
		}, start, func() string {
			if resumeLog != "" {
				return resumeLog
			}
			return findLatestSessionLog()
		})

		stopUsageSnapshot := startCoopUsageSnapshot(ctx, cfg.coopPort)
		exitCode, _ := runCoopOnce(mail.sessionCtx, cfg, coopStateDir, resumeLog)
		stopUsageSnapshot()
		flushCancel()
		elapsed := time.Since(start)

		// Resolve the graceful-shutdown bit BEFORE closing the session bead: the
		// stop_requested check further down runs too late to inform the status
		// the close writes (ka-0000003165). Uses a fresh context for the bead
		// read — ctx is already cancelled on the pod-terminate path.
		shutdownRequested := false
		if beadsapi.SessionStatusDependsOnShutdown(exitCode) {
			shutdownRequested = sessionShutdownRequested(context.Background(), ctx, mail.killRequested())
		}

		closeSessionBead(context.Background(), sessionBeadID, exitCode, resumeLog, shutdownRequested)

		if ctx.Err() != nil {
			return nil // clean SIGTERM/SIGINT/SIGHUP exit
		}

		fmt.Printf("[gb agent start] coop exited (code %d) after %s\n", exitCode, elapsed.Round(time.Second))

		if resumeLog != "" && (exitCode != 0 || elapsed < time.Duration(minRuntimeSecs)*time.Second) {
			retireStaleSession(resumeLog)
		}

		// A control:kill mail stopped coop: close the bead and exit rather
		// than restart.
		if mail.killRequested() {
			fmt.Printf("[gb agent start] control:kill mail — closing bead and exiting\n")
			closeAgentBeadOnMailKill(func(format string, args ...any) {
				fmt.Printf("[gb agent start] "+format+"\n", args...)
			})
			return nil
		}

		// Check if the agent requested a polite stop before restarting.
		// Close the bead so it transitions to closed status (it remains
		// queryable for historical purposes — closed beads are not deleted).
		// The reconciler excludes closed beads from desired state, so no
		// new pod will be spawned. A daemon that denies this agent its own
		// bead parks the loop instead of respawning coop (gd-WKjtvzPl6g).
		if handleAgentStartStop(ctx, envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))) {
			return nil
		}

		// If rate-limited, sleep until the reset time before restarting.
		if sleepDur := rateLimitSleepDuration(); sleepDur > 0 {
			fmt.Printf("[gb agent start] rate limited — sleeping %s before restart\n", sleepDur.Round(time.Second))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(sleepDur):
			}
			restarts = 0
			continue
		}

		if elapsed >= time.Duration(minRuntimeSecs)*time.Second {
			restarts = 0
		}
		restarts++
		fmt.Printf("[gb agent start] restarting (attempt %d/%d) in 2s...\n", restarts, cfg.maxRestarts)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// resolveMuxSessionID resolves the coopmux session ID this pod registers under.
// It MUST be the full, untruncated pod name so the full-name terminal deep-link
// on the Slack agent card resolves to a live session.
//
// Precedence: POD_NAME (injected via the downward API, fieldPath: metadata.name)
// then os.Hostname(). The kernel caps gethostname()/$HOSTNAME at 63 bytes
// (HOST_NAME_MAX), so a pod whose name exceeds 63 chars — ~51% of agents, since
// long role+thread names (crew-<project>-<role>-thread-<ts>) routinely blow the
// budget — registers under a TRUNCATED session ID that can never match the
// full-name hash the card emits, leaving the Terminal link silently dead
// (ka-0000003655). POD_NAME comes from the downward API and is not length-capped,
// so preferring it fixes registration at the source. This also mirrors coop's
// own mux_client, which already prefers POD_NAME over HOSTNAME for its metadata.
func resolveMuxSessionID() string {
	if podName := os.Getenv("POD_NAME"); podName != "" {
		return podName
	}
	hostname, _ := os.Hostname()
	return hostname
}

// runCoopOnce starts coop for a single session, launches per-session
// goroutines, waits for coop to exit, and returns its exit code.
func runCoopOnce(ctx context.Context, cfg k8sConfig, coopStateDir, resumeLog string) (int, error) {
	h := harness.MustFromEnv()
	coopArgs := []string{
		"--agent=" + harness.CoopAgent(h, cfg.command),
		fmt.Sprintf("--port=%d", cfg.coopPort),
		fmt.Sprintf("--port-health=%d", cfg.coopHealthPort),
		fmt.Sprintf("--port-metrics=%d", cfg.coopMetricsPort),
		"--cols=200", "--rows=50",
	}
	if resumeLog != "" {
		coopArgs = append(coopArgs, "--resume", resumeLog)
		fmt.Printf("[gb agent start] starting coop (%s/%s) with resume\n", cfg.role, cfg.agent)
	} else {
		fmt.Printf("[gb agent start] starting coop (%s/%s)\n", cfg.role, cfg.agent)
	}
	coopWorkdir := resolveCoopWorkdir(cfg)
	command := cfg.command
	logf := func(msg string, args ...any) {
		fmt.Printf("[gb agent start] "+msg+"\n", args...)
	}
	// Codex gets AGENTS.md (prepareAgentPrompt) but no CLI flag.
	if path, ok := prepareAgentPrompt(h, coopWorkdir, logf); ok {
		command = injectSystemPromptShellCommand(command, path)
	}
	coopArgs = append(coopArgs, "--", "sh", "-c", command)

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	coopCmd := exec.CommandContext(sessionCtx, "coop", coopArgs...)
	coopCmd.Dir = coopWorkdir
	coopCmd.Stdout = os.Stdout
	coopCmd.Stderr = os.Stderr
	// Strip denied CLAUDE_CODE_* keys (anything outside claudeCodeEnvAllowlist)
	// from the inherited env. Keeps init_agent_loop.go:buildCoopEnv and this
	// path in sync — see gardenio/docs/audits/buildcoopenv-pod-env-audit.md and
	// ka-0000007438.
	parentEnv := os.Environ()
	coopEnv := make([]string, 0, len(parentEnv)+2)
	for _, kv := range parentEnv {
		if isDeniedClaudeCodeEnv(kv) || isDeniedControllerSecretEnv(kv) {
			logDeniedClaudeCodeEnv(kv)
			continue
		}
		if harness.IsDeniedEnv(h, kv) {
			logDeniedHarnessEnv(kv)
			continue
		}
		coopEnv = append(coopEnv, kv)
	}
	coopEnv = append(coopEnv, "COOP_LOG_LEVEL="+envOr("COOP_LOG_LEVEL", "info"))
	// Keep Claude Code's messaging sockets off the FSGroup'd /tmp EmptyDir,
	// which is 2777 and fails its ancestor vet (ka-0000002604). Best-effort:
	// an empty value means the agent launches unchanged, without the override.
	if kv := messagingTmpDirEnv(logf); kv != "" {
		coopEnv = append(coopEnv, kv)
	}
	// For non-memory-eligible agents (one-shot crew, thread agents) suppress
	// Claude Code's ~3 KB auto-memory preamble. They get a short pointer in
	// the system-prompt.md instead. See ka-0000006599.
	agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if !isMemoryEligible(context.Background(), agentBeadID, cfg.role, nil) {
		coopEnv = append(coopEnv, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1")
	}
	coopCmd.Env = coopEnv
	// Send SIGTERM on context cancellation instead of the default SIGKILL.
	// This gives coop a chance to save session state before being killed.
	coopCmd.Cancel = func() error {
		fmt.Printf("[gb agent start] sending SIGTERM to coop (pid %d)\n", coopCmd.Process.Pid)
		return coopCmd.Process.Signal(syscall.SIGTERM)
	}
	coopCmd.WaitDelay = 20 * time.Second

	if err := coopCmd.Start(); err != nil {
		return 1, fmt.Errorf("start coop: %w", err)
	}

	// Per-session lifecycle: bypass startup, inject prompt, monitor exit.
	// Uses the same lifecycle functions as gb init (init_agent.go) to avoid
	// divergent behavior between the two code paths.
	coop := coopapi.New(fmt.Sprintf("http://localhost:%d", cfg.coopPort))
	lifeCfg := initConfig{coopPort: cfg.coopPort, role: cfg.role}
	_ = resumeLog
	// The local (gb agent start) path has no durable cross-restart switch
	// tracker, so it passes a nil productive sink — the terminal-cap reset is a
	// gb-init-only concern (ka-0000001718 Part B). monitorExit nil-guards it.
	// It also has no startup-login failure budget (that is a gb-init supervisor
	// concern, ka-0000007536), so the login-blocked sink is nil too. The
	// warm-pool coop-resume relaunch sink is nil as well: this path is not a
	// prewarmed-pool pod (no standby), so there is no warm assignment to graft a
	// resume onto (ka-0000000983).
	go runSessionLifecycle(sessionCtx, coop, lifeCfg, nil, nil, nil)
	go monitorIdle(sessionCtx, coop)

	waitErr := coopCmd.Wait()
	sessionCancel()

	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				exitCode = status.ExitStatus()
			} else {
				exitCode = 1
			}
		}
	}
	return exitCode, nil
}

// resolveCoopWorkdirBotHome is the mount root under which init-clone pods
// place the primary project checkout ({botHome}/{project}/work). Production
// always uses the real path; tests override it to a t.TempDir() so a repo's
// own reviewing checkout can't accidentally satisfy (or fail to satisfy) the
// .git probe below and make assertions depend on ambient filesystem state
// (gd-5W5zqD7HdR).
var resolveCoopWorkdirBotHome = "/home/agent/bot"

// resolveCoopWorkdir determines the working directory for coop/Claude Code.
// If the primary project repo was cloned by init-clone (at /home/agent/bot/{project}/work),
// use that as the cwd so agents start inside the actual codebase. Falls back to
// the scaffold workspace if no project repo exists.
func resolveCoopWorkdir(cfg k8sConfig) string {
	if cfg.project != "" {
		// init-clone puts the primary repo at /home/agent/bot/{project}/work.
		repoDir := filepath.Join(resolveCoopWorkdirBotHome, cfg.project, "work")
		if info, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil && info.IsDir() {
			fmt.Printf("[gb agent start] using project repo as cwd: %s\n", repoDir)
			// Set KD_WORKSPACE so hooks and commands can find the repo.
			os.Setenv("KD_WORKSPACE", repoDir)
			os.Setenv("WORKSPACE", repoDir)
			return repoDir
		}
	}

	fmt.Printf("[gb agent start] no project repo found, using scaffold workspace: %s\n", cfg.workspace)
	return cfg.workspace
}

// ── terminated-mid-session fallback wrap-up (Fix C, ka-0000007005) ──────────

// terminatedMidSessionCloseReason is stamped in the synthesized wrap-up's
// custom fields so the render side (Slack card) and any later reconciler can
// tell a graceful-teardown wrap-up apart from a real agent-authored one and
// from the stop-gate fallbacks. It is NOT written to close_reason on the bead:
// the bead is deliberately left OPEN so the agent resumes.
const terminatedMidSessionCloseReason = "terminated_mid_session"

// writeTerminatedMidSessionWrapUp best-effort stamps a synthesized
// "terminated mid-session" wrap-up onto the STILL-OPEN agent bead when the pod
// is gracefully torn down (SIGTERM/SIGINT/SIGHUP) mid-session without one.
//
// It is registered as a defer in runAgentStartK8s and fires on every return,
// but does real work ONLY on the graceful-shutdown path (ctx.Err() != nil).
// The point: these agents are meant to RESUME (the restart loop deliberately
// does not close the bead on shutdown), so on the next boot the agent — and
// the Slack agent card — can read "here's where I was" instead of an abrupt
// end-of-conversation. Invariants:
//
//   - Never CLOSES the bead — only patches the wrapup field. The bead stays
//     open so the reconciler respawns/resumes it.
//   - Never OVERWRITES an existing wrap-up: a real `gb stop --wrapup`, a
//     `gb done`, or an already-synthesized fallback always wins.
//   - Only writes when the bead is still OPEN (a closed bead already had its
//     terminal close path run, which synthesizes its own fallback).
//   - Bounded by a short fresh-context timeout (ctx is already cancelled on
//     this path) so it completes well inside the K8s termination grace period.
//
// Best-effort throughout: any missing prerequisite or daemon error is logged
// and swallowed — a teardown must never block on this.
func writeTerminatedMidSessionWrapUp(ctx context.Context, agentBeadID string) {
	// Only the graceful-shutdown (signal-cancelled) path synthesizes a
	// mid-session wrap-up. A normal error return (max-restarts, setup failure)
	// leaves ctx uncancelled and is handled by its own paths.
	if ctx.Err() == nil {
		return
	}
	if agentBeadID == "" || daemon == nil {
		return
	}

	// ctx is cancelled on this path — use a fresh, tightly-bounded context so
	// the write still lands within the K8s termination grace window (<5s).
	writeCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	bead, err := daemon.GetBead(writeCtx, agentBeadID)
	if err != nil || bead == nil {
		if err != nil {
			fmt.Printf("[gb agent start] terminated-mid-session wrap-up: read bead %s: %v\n", agentBeadID, err)
		}
		return
	}
	// A closed bead already ran its terminal close path (which writes its own
	// fallback wrap-up); don't touch it.
	if bead.Status == "closed" {
		return
	}
	// A real wrap-up (or an earlier synthesized one) always wins.
	if strings.TrimSpace(bead.Fields[WrapUpFieldName]) != "" {
		return
	}

	wrapup := synthesizeTerminatedMidSessionWrapUp(bead)
	if wrapup == "" {
		return
	}
	if err := daemon.UpdateBeadFields(writeCtx, agentBeadID, map[string]string{
		WrapUpFieldName: wrapup,
	}); err != nil {
		fmt.Printf("[gb agent start] terminated-mid-session wrap-up: write bead %s: %v\n", agentBeadID, err)
		return
	}
	fmt.Printf("[gb agent start] stamped terminated-mid-session wrap-up on open bead %s (resume will read last progress)\n", agentBeadID)
}

// synthesizeTerminatedMidSessionWrapUp builds the minimal wrap-up JSON stamped
// on an open agent bead at graceful teardown. The tl;dr is the task-specified
// "Pod terminated mid-session before wrap-up" headline; when the bead recorded
// tool calls before teardown, the count is folded in so the partial progress is
// visible rather than reading as a no-op. Marked synthesized=true so the render
// side (hasHumanWrapUp / bot_agents_wrapup.go) never mistakes it for an
// agent-authored headline, and terminated_mid_session=true so a resume or
// reconciler can recognize the teardown class. Returns "" only defensively (a
// marshal failure).
func synthesizeTerminatedMidSessionWrapUp(bead *beadsapi.BeadDetail) string {
	toolCalls := beadToolCallCount(bead)

	tldr := "Pod terminated mid-session before wrap-up"
	accomplishments := "Pod was gracefully terminated (deploy/node-drain/eviction) mid-session before the agent submitted a wrap-up; the bead is left open to resume."
	if toolCalls > 0 {
		tldr = fmt.Sprintf("Pod terminated mid-session before wrap-up (%d tool call%s so far)", toolCalls, plural(toolCalls))
		accomplishments = fmt.Sprintf("Pod was gracefully terminated mid-session after %d tool call%s, before the agent submitted a wrap-up; the bead is left open to resume from this partial progress.", toolCalls, plural(toolCalls))
	}
	if title := strings.TrimSpace(beadDisplayTitle(bead)); title != "" {
		tldr = fmt.Sprintf("%s — %s", tldr, title)
	}

	wu := WrapUp{
		TLDR:            tldr,
		Accomplishments: accomplishments,
		HandoffNotes:    "Terminated by pod teardown, not by the agent. Resume this bead to continue; no wrap-up was authored, so prior progress lives in the session transcript.",
		Custom: map[string]string{
			"synthesized":            "true",
			"terminated_mid_session": "true",
			"close_reason":           terminatedMidSessionCloseReason,
		},
	}
	// MarshalWrapUp stamps Timestamp (time.Now) when zero — the canonical
	// serializer for the wrapup field.
	data, err := MarshalWrapUp(&wu)
	if err != nil {
		return ""
	}
	return data
}

// ── helpers ───────────────────────────────────────────────────────────────

func intEnvOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}

func orStr(s, def string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return def
}

// rateLimitSleepDuration reads the sentinel file written by monitorAgentExit
// when a rate limit is detected, parses the reset time from the message,
// and returns how long to sleep. Returns 0 if no sentinel file exists.
func rateLimitSleepDuration() time.Duration {
	const sentinelPath = "/tmp/rate_limit_reset"
	data, err := os.ReadFile(sentinelPath)
	if err != nil {
		return 0
	}
	os.Remove(sentinelPath)

	msg := string(data)

	// Extract reset hour from message (e.g., "resets 9pm (UTC)" -> "9pm").
	dur := parseResetDuration(msg)
	if dur > 0 {
		return dur + time.Minute // buffer
	}

	// Fallback: sleep 30 minutes if we can't parse the reset time.
	fmt.Printf("[gb agent start] could not parse reset time from: %s\n", msg)
	return 30 * time.Minute
}

// parseResetDuration extracts a reset time like "9pm" from the rate limit
// message and returns the duration until that time (UTC).
func parseResetDuration(msg string) time.Duration {
	// Look for patterns like "9pm", "10am", "12pm"
	idx := strings.Index(msg, "resets ")
	if idx < 0 {
		return 0
	}
	rest := msg[idx+len("resets "):]

	var hour int
	var ampm string
	n, _ := fmt.Sscanf(rest, "%d%s", &hour, &ampm)
	if n < 2 || hour < 1 || hour > 12 {
		return 0
	}
	ampm = strings.ToLower(strings.TrimSpace(ampm))
	// Strip trailing non-alpha (e.g., "pm" from "pm (UTC)")
	if strings.HasPrefix(ampm, "pm") {
		if hour != 12 {
			hour += 12
		}
	} else if strings.HasPrefix(ampm, "am") {
		if hour == 12 {
			hour = 0
		}
	} else {
		return 0
	}

	now := time.Now().UTC()
	target := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.UTC)
	if target.Before(now) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}
