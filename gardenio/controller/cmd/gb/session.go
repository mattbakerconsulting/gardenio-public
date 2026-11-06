package main

// gb session — first-class session management.
//
// Sessions are tracked as beads (type=session) that record each coop session's
// lifecycle: start time, end time, exit code, session log path, and the linked
// agent bead. This replaces the previous approach where sessions were just JSONL
// files on disk with no metadata or tracking.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var sessionCmd = &cobra.Command{
	Use:     "session",
	Short:   "Manage agent sessions",
	GroupID: "session",
}

var sessionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions for the current agent or all agents",
	RunE:  runSessionList,
}

var sessionShowCmd = &cobra.Command{
	Use:   "show <session-id>",
	Short: "Show details of a session",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionShow,
}

var sessionRegisterCmd = &cobra.Command{
	Use:    "register",
	Short:  "Register a new session (called internally by the restart loop)",
	Hidden: true,
	RunE:   runSessionRegister,
}

var sessionCloseCmd = &cobra.Command{
	Use:    "close <session-id>",
	Short:  "Close a session bead (called internally)",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE:   runSessionClose,
}

var sessionReconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Recover lost costs from orphaned sessions (crashed agents)",
	Long: `Finds session beads with status=active that are older than the staleness
threshold (default 1h), indicating the agent crashed before closing the session.

For each orphaned session, if the JSONL session log is still accessible on disk,
usage is re-parsed and accumulated onto the agent and account beads. Sessions
without accessible logs are marked as crashed with no cost recovery.

This command is safe to run multiple times — each orphaned session is closed
on reconciliation, so it won't be processed again.`,
	RunE: runSessionReconcile,
}

var (
	sessionAgent    string
	sessionLimit    int
	sessionAll      bool
	sessionExitCode int
	sessionLog      string
	sessionStopped  bool
	reconcileMaxAge string
	reconcileDryRun bool
)

func init() {
	sessionCmd.AddCommand(sessionListCmd)
	sessionCmd.AddCommand(sessionShowCmd)
	sessionCmd.AddCommand(sessionRegisterCmd)
	sessionCmd.AddCommand(sessionCloseCmd)
	sessionCmd.AddCommand(sessionReconcileCmd)

	sessionListCmd.Flags().StringVar(&sessionAgent, "agent", "", "filter by agent name (default: current agent)")
	sessionListCmd.Flags().IntVar(&sessionLimit, "limit", 20, "max number of sessions to show")
	sessionListCmd.Flags().BoolVar(&sessionAll, "all", false, "show sessions for all agents")

	sessionRegisterCmd.Flags().StringVar(&sessionLog, "log", "", "path to session JSONL log")

	sessionCloseCmd.Flags().IntVar(&sessionExitCode, "exit-code", 0, "exit code of the session")
	sessionCloseCmd.Flags().StringVar(&sessionLog, "log", "", "path to session JSONL log")
	sessionCloseCmd.Flags().BoolVar(&sessionStopped, "stopped", false,
		"the teardown was requested (gb stop / pod terminate), so a signal-termination exit code "+
			"(143/130/129) records status=completed instead of crashed")

	sessionReconcileCmd.Flags().StringVar(&reconcileMaxAge, "max-age", "1h", "only reconcile sessions older than this duration")
	sessionReconcileCmd.Flags().BoolVar(&reconcileDryRun, "dry-run", false, "list orphaned sessions without reconciling")
	sessionReconcileCmd.Flags().StringVar(&sessionAgent, "agent", "", "filter by agent name (default: all agents)")
}

func runSessionList(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	agentFilter := sessionAgent
	if !sessionAll && agentFilter == "" {
		agentFilter = os.Getenv("BOAT_AGENT")
	}

	sessions, err := daemon.ListSessionBeads(ctx, agentFilter, sessionLimit)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sessions)
	}

	if len(sessions) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tAGENT\tSTATUS\tSTARTED\tDURATION\tCOST\tEXIT\n")
	for _, s := range sessions {
		agent := s.Fields["agent"]
		status := s.Fields["status"]
		startedAt := s.Fields["started_at"]
		endedAt := s.Fields["ended_at"]
		exitCode := s.Fields["exit_code"]

		duration := "-"
		if startedAt != "" && endedAt != "" {
			if st, err := time.Parse(time.RFC3339, startedAt); err == nil {
				if et, err := time.Parse(time.RFC3339, endedAt); err == nil {
					duration = et.Sub(st).Round(time.Second).String()
				}
			}
		} else if startedAt != "" && status == "active" {
			if st, err := time.Parse(time.RFC3339, startedAt); err == nil {
				duration = time.Since(st).Round(time.Second).String() + " (running)"
			}
		}

		startShort := ""
		if startedAt != "" {
			if t, err := time.Parse(time.RFC3339, startedAt); err == nil {
				startShort = t.Format("01-02 15:04")
			}
		}

		cost := "-"
		if c := s.Fields["total_cost_usd"]; c != "" && c != "0" {
			if v, err := strconv.ParseFloat(c, 64); err == nil {
				cost = fmt.Sprintf("$%.2f", v)
			}
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.ID, agent, status, startShort, duration, cost, exitCode)
	}
	return w.Flush()
}

func runSessionShow(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	sessionID := args[0]

	bead, err := daemon.GetBead(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("getting session %s: %w", sessionID, err)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(bead)
	}

	fmt.Printf("ID:           %s\n", bead.ID)
	fmt.Printf("Title:        %s\n", bead.Title)
	fmt.Printf("Status:       %s\n", bead.Fields["status"])
	fmt.Printf("Agent:        %s\n", bead.Fields["agent"])
	fmt.Printf("Agent Bead:   %s\n", bead.Fields["agent_bead_id"])
	fmt.Printf("Project:      %s\n", bead.Fields["project"])
	fmt.Printf("Role:         %s\n", bead.Fields["role"])
	fmt.Printf("Hostname:     %s\n", bead.Fields["hostname"])
	fmt.Printf("Started At:   %s\n", bead.Fields["started_at"])
	fmt.Printf("Ended At:     %s\n", bead.Fields["ended_at"])
	fmt.Printf("Exit Code:    %s\n", bead.Fields["exit_code"])
	fmt.Printf("Resumed:      %s\n", bead.Fields["resumed"])
	fmt.Printf("Session Log:  %s\n", bead.Fields["session_log"])

	if bead.Fields["started_at"] != "" && bead.Fields["ended_at"] != "" {
		if st, err := time.Parse(time.RFC3339, bead.Fields["started_at"]); err == nil {
			if et, err := time.Parse(time.RFC3339, bead.Fields["ended_at"]); err == nil {
				fmt.Printf("Duration:     %s\n", et.Sub(st).Round(time.Second))
			}
		}
	}

	// Usage metrics (only shown if populated).
	if bead.Fields["input_tokens"] != "" && bead.Fields["input_tokens"] != "0" {
		fmt.Println()
		fmt.Println("Usage:")
		fmt.Printf("  Input Tokens:       %s\n", bead.Fields["input_tokens"])
		fmt.Printf("  Output Tokens:      %s\n", bead.Fields["output_tokens"])
		fmt.Printf("  Cache Read Tokens:  %s\n", bead.Fields["cache_read_tokens"])
		fmt.Printf("  Cache Write Tokens: %s\n", bead.Fields["cache_write_tokens"])
		fmt.Printf("  Cost (USD):         $%s\n", bead.Fields["total_cost_usd"])
		fmt.Printf("  API Requests:       %s\n", bead.Fields["request_count"])
		fmt.Printf("  Tool Calls:         %s\n", bead.Fields["tool_call_count"])
		fmt.Printf("  Errors:             %s\n", bead.Fields["error_count"])
		fmt.Printf("  Compactions:        %s\n", bead.Fields["compaction_count"])
	}

	return nil
}

func runSessionRegister(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	agentName := os.Getenv("BOAT_AGENT")
	agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	project := defaultProject()
	role := envOr("BOAT_ROLE", "unknown")
	hostname, _ := os.Hostname()

	if agentName == "" {
		return fmt.Errorf("BOAT_AGENT not set")
	}

	fields := beadsapi.SessionFields{
		Agent:       agentName,
		AgentBeadID: agentBeadID,
		Project:     project,
		Role:        role,
		Hostname:    hostname,
		SessionLog:  sessionLog,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		Resumed:     localResumeLogLoadable(sessionLog),
		Status:      "active",
	}

	id, err := daemon.RegisterSession(ctx, fields)
	if err != nil {
		return fmt.Errorf("registering session: %w", err)
	}

	fmt.Println(id)
	return nil
}

func runSessionClose(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	sessionID := args[0]

	// Parse usage from session log if available.
	var usage *beadsapi.SessionUsage
	if canLoadSessionUsage(sessionLog) {
		u, err := loadSessionUsage(sessionLog)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse session usage: %v\n", err)
		} else {
			usage = u
		}
	}

	// Read session bead to get started_at for duration calculation.
	var durationSecs float64
	var startedAt time.Time
	if bead, err := daemon.GetBead(ctx, sessionID); err == nil {
		startedAt = sessionStartedAt(bead.Fields)
		if !startedAt.IsZero() {
			durationSecs = time.Since(startedAt).Seconds()
		}
	}

	// Per-session slowest tool calls, from coop's hook_events.jsonl (P4).
	recordSessionSlowestCalls(ctx, sessionID, defaultCoopStateDir(), startedAt)

	if err := daemon.CloseSessionWithShutdown(ctx, sessionID, sessionExitCode, sessionLog, usage, sessionStopped); err != nil {
		return fmt.Errorf("closing session %s: %w", sessionID, err)
	}

	// Accumulate session usage onto the agent bead.
	if usage != nil {
		agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
		if agentBeadID != "" {
			if err := daemon.UpdateAgentUsage(ctx, agentBeadID, usage); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not update agent bead usage: %v\n", err)
			}
		}

		// Accumulate session usage onto the account bead.
		project := os.Getenv("BOAT_PROJECT")
		if project != "" {
			acctCtx, acctCancel := context.WithTimeout(ctx, 15*time.Second)
			updateAccountUsage(acctCtx, project, usage)
			acctCancel()
		}

		// Push final metrics to vmagent so they land even if the pod
		// is torn down before the next Prometheus scrape.
		pushSessionMetrics(ctx, sessionMetricsLabels{
			AgentName:  envOr("BOAT_AGENT", "unknown"),
			Project:    defaultProject(),
			Account:    os.Getenv("BOAT_ACCOUNT"),
			SessionID:  sessionID,
			OperatorID: resolveOperatorID(ctx),
			Model:      os.Getenv("CLAUDE_MODEL"),
		}, usage, durationSecs)
	}

	fmt.Printf("Session %s closed (exit code: %d).\n", sessionID, sessionExitCode)
	return nil
}

// registerSessionBead creates a session bead for the current coop session.
// Called from the restart loop in agent_start_k8s.go.
func registerSessionBead(ctx context.Context, cfg k8sConfig, resumeLog string) string {
	if daemon == nil {
		return ""
	}

	agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	hostname, _ := os.Hostname()

	// resumed must reflect ground truth: prior conversation state was
	// observably available at registration time. A flag/env-var being set
	// is just intent — the file or S3 object must actually exist.
	sessionLog := ""
	resumed := false
	if localResumeLogLoadable(resumeLog) {
		sessionLog = resumeLog
		resumed = true
	} else if sid := os.Getenv("COOP_S3_RESUME_SESSION"); sid != "" {
		// S3 resume path: COOP_S3_RESUME_SESSION is a manual operator
		// override (see coop/crates/cli/src/s3.rs resolve_session_key
		// doc-comment + audit ka-0000006166). The live production wire
		// is COOP_S3_SESSION_KEY, set by init_agent_loop.go from the
		// agent bead ID. This branch handles the legacy/operator-set
		// COOP_S3_RESUME_SESSION value, which can be stale (or never
		// have been written), so verify the S3 prefix actually has
		// objects before claiming resumed=true.
		if s3ResumeSessionExistsFn(ctx, sid) {
			sessionLog = "s3:" + sid
			resumed = true
		}
	} else if key := agentS3SessionKey(); key != "" && os.Getenv("COOP_S3_BUCKET") != "" {
		// Production S3 resume path: init_agent_loop.go wires
		// COOP_S3_SESSION_KEY=<agent-bead-id> on the coop subprocess.
		// gb itself doesn't carry that env var, but the underlying key
		// is just agentS3SessionKey() (KD_AGENT_ID / BOAT_AGENT_BEAD_ID).
		// Probe S3 for a pre-existing archive under that prefix —
		// non-empty means coop will have prior state to restore.
		// See ka-0000002486 — without this branch, every production
		// session bead reports resumed=false even when coop has
		// demonstrably restored prior state from S3.
		if s3ResumeSessionExistsFn(ctx, key) {
			sessionLog = "s3:" + key
			resumed = true
		}
	}

	fields := beadsapi.SessionFields{
		Agent:       cfg.agent,
		AgentBeadID: agentBeadID,
		Project:     cfg.project,
		Role:        cfg.role,
		Hostname:    hostname,
		SessionLog:  sessionLog,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		Resumed:     resumed,
		Status:      "active",
	}

	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	id, err := daemon.RegisterSession(regCtx, fields)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not register session bead: %v\n", err)
		return ""
	}
	fmt.Printf("[gb agent start] session bead registered: %s\n", id)
	return id
}

// sessionShutdownRequested resolves the graceful-shutdown bit for a session that
// exited with a signal-termination code, so closeSessionBead can tell a clean
// `gb stop` / pod-terminate teardown from a genuine mid-task kill
// (ka-0000003165). Any one of these signals is sufficient:
//
//  1. sigCtx is done — this process caught SIGTERM/SIGINT/SIGHUP itself (the
//     restart loops run under signal.NotifyContext), i.e. a pod terminate.
//  2. mailKilled — a control:kill mail asked coop to shut down.
//  3. stop_requested=true on the agent bead — set by `gb stop` BEFORE it asks
//     coop to shut down (cmd/gb/stop.go). This is the in-pod agent-initiated
//     path, where the process is never signalled at all and (1) stays nil.
//  4. sigCtx is done within podTerminateSignalWait. On a pod terminate the
//     preStop hook (podmanager.buildAgentContainer) presses Escape and asks
//     coop to shut down, and kubelet signals PID 1 only after the hook ends,
//     so coop exits 143 seconds before (1) can hold (gd-Ga8iHjLaYR).
//
// Signal (3) has to be read here rather than reused from the restart loop's own
// isStopRequested call, because that check runs AFTER closeSessionBead — by
// which point the status has already been written.
//
// Only consulted for the codes whose classification actually depends on it
// (SessionStatusDependsOnShutdown), so the common exit-0 / exit-10 closes stay
// read-free, and only an unexplained signal exit pays the wait.
func sessionShutdownRequested(ctx, sigCtx context.Context, mailKilled bool) bool {
	if sigCtx.Err() != nil || mailKilled {
		return true
	}
	agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if agentBeadID != "" && isStopRequested(ctx, agentBeadID) {
		return true
	}
	select {
	case <-sigCtx.Done():
		return true
	case <-time.After(podTerminateSignalWait):
		return false
	}
}

// podTerminateSignalWait bounds how long an unexplained signal exit waits for
// the pod-terminate SIGTERM. The preStop hook sleeps 3s after asking coop to
// shut down; the rest is slack for kubelet. A genuine crash restarts this much
// later. Overridable for tests.
var podTerminateSignalWait = 10 * time.Second

// closeSessionBead closes the session bead after coop exits.
//
// shutdownRequested tells the status classifier whether this teardown was asked
// for, so a 143 (128+SIGTERM) from the normal `gb stop` / pod-terminate path is
// recorded as "completed" instead of "crashed" (ka-0000003165). See
// beadsapi.SessionStatusForExit.
func closeSessionBead(ctx context.Context, sessionBeadID string, exitCode int, resumeLog string, shutdownRequested bool) {
	if daemon == nil || sessionBeadID == "" {
		return
	}

	// Try to find the actual session log path if we started fresh.
	logPath := resumeLog
	if logPath == "" {
		logPath = findLatestSessionLog()
	}

	// Parse usage from session log if available.
	var usage *beadsapi.SessionUsage
	if canLoadSessionUsage(logPath) {
		u, err := loadSessionUsage(logPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not parse session usage: %v\n", err)
		} else {
			usage = u
		}
	}

	// Read session bead to get started_at for duration calculation.
	var durationSecs float64
	var startedAt time.Time
	if bead, err := daemon.GetBead(ctx, sessionBeadID); err == nil {
		startedAt = sessionStartedAt(bead.Fields)
		if !startedAt.IsZero() {
			durationSecs = time.Since(startedAt).Seconds()
		}
	}

	closeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Per-session slowest tool calls, from coop's hook_events.jsonl (P4).
	// Written before the close so the field lands on a still-open bead.
	recordSessionSlowestCalls(closeCtx, sessionBeadID, defaultCoopStateDir(), startedAt)

	if err := daemon.CloseSessionWithShutdown(closeCtx, sessionBeadID, exitCode, logPath, usage, shutdownRequested); err != nil {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not close session bead %s: %v\n", sessionBeadID, err)
	} else {
		fmt.Printf("[gb agent start] session bead closed: %s (exit code: %d)\n", sessionBeadID, exitCode)
	}

	// Accumulate session usage onto the agent bead for per-agent cost visibility.
	if usage != nil {
		agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
		if agentBeadID != "" {
			usageCtx, usageCancel := context.WithTimeout(ctx, 10*time.Second)
			defer usageCancel()
			if err := daemon.UpdateAgentUsage(usageCtx, agentBeadID, usage); err != nil {
				fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not update agent bead usage: %v\n", err)
			}
		}

		// Accumulate session usage onto the account bead.
		// Use a dedicated timeout — account resolution requires 6 sequential HTTP
		// calls (3 resolution + 3 update) vs 3 for agent, so it needs more headroom.
		project := os.Getenv("BOAT_PROJECT")
		if project != "" {
			acctCtx, acctCancel := context.WithTimeout(ctx, 15*time.Second)
			updateAccountUsage(acctCtx, project, usage)
			acctCancel()
		}

		// Push final metrics to vmagent so they land even if the pod
		// is torn down before the next Prometheus scrape.
		pushSessionMetrics(ctx, sessionMetricsLabels{
			AgentName:  envOr("BOAT_AGENT", "unknown"),
			Project:    defaultProject(),
			Account:    os.Getenv("BOAT_ACCOUNT"),
			SessionID:  sessionBeadID,
			OperatorID: resolveOperatorID(ctx),
			Model:      os.Getenv("CLAUDE_MODEL"),
		}, usage, durationSecs)
	}
}

// findLatestSessionLog returns the path of the most recently modified .jsonl
// file under ~/.claude/projects/ (the session log coop just wrote).
func findLatestSessionLog() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projectsDir := homeDir + "/.claude/projects"

	var bestPath string
	var bestTime time.Time

	_ = walkSessionLogs(projectsDir, func(path string, modTime time.Time) {
		if modTime.After(bestTime) {
			bestPath = path
			bestTime = modTime
		}
	})

	return bestPath
}

// walkSessionLogs walks the projects directory for .jsonl files,
// excluding subagent sessions.
func walkSessionLogs(projectsDir string, fn func(path string, modTime time.Time)) error {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		subDir := projectsDir + "/" + entry.Name()
		subEntries, err := os.ReadDir(subDir)
		if err != nil {
			continue
		}
		for _, se := range subEntries {
			if se.IsDir() {
				continue
			}
			name := se.Name()
			if !strings.HasSuffix(name, ".jsonl") || strings.Contains(name, "subagent") {
				continue
			}
			info, err := se.Info()
			if err != nil {
				continue
			}
			fn(subDir+"/"+name, info.ModTime())
		}
	}
	return nil
}

// updateAccountUsage resolves the project's active account and accumulates
// session usage onto the account bead. Best-effort: errors are logged as
// warnings and do not block session close.
func updateAccountUsage(ctx context.Context, project string, usage *beadsapi.SessionUsage) {
	if daemon == nil || usage == nil {
		return
	}
	accountBeadID, err := daemon.FindAccountBeadIDForProject(ctx, project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not resolve account for project %q: %v\n", project, err)
		return
	}
	if accountBeadID == "" {
		fmt.Fprintf(os.Stderr, "warning: no account bead found for project %q (cost will not be tracked on account)\n", project)
		return
	}
	if err := daemon.UpdateAccountUsage(ctx, accountBeadID, usage); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not update account bead usage: %v\n", err)
	}
}

func runSessionReconcile(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	maxAge, err := time.ParseDuration(reconcileMaxAge)
	if err != nil {
		return fmt.Errorf("invalid --max-age: %w", err)
	}

	agentFilter := sessionAgent // empty = all agents

	orphaned, err := daemon.ListOrphanedSessions(ctx, agentFilter, maxAge)
	if err != nil {
		return fmt.Errorf("listing orphaned sessions: %w", err)
	}

	if len(orphaned) == 0 {
		if jsonOutput {
			printJSON([]any{})
		} else {
			cmd.Println("No orphaned sessions found.")
		}
		return nil
	}

	type reconcileResult struct {
		SessionID     string  `json:"session_id"`
		Agent         string  `json:"agent"`
		StartedAt     string  `json:"started_at"`
		LogAvailable  bool    `json:"log_available"`
		CostRecovered float64 `json:"cost_recovered"`
		Status        string  `json:"status"` // recovered, crashed, dry-run, error
		Error         string  `json:"error,omitempty"`
	}

	var results []reconcileResult
	var totalRecovered float64

	for _, session := range orphaned {
		agent := session.Fields["agent"]
		startedAt := session.Fields["started_at"]
		logPath := session.Fields["session_log"]

		if reconcileDryRun {
			results = append(results, reconcileResult{
				SessionID:    session.ID,
				Agent:        agent,
				StartedAt:    startedAt,
				LogAvailable: logPath != "" && fileExists(logPath),
				Status:       "dry-run",
			})
			continue
		}

		// Try to parse usage from the session log if accessible.
		var usage *beadsapi.SessionUsage
		if logPath != "" && fileExists(logPath) {
			u, parseErr := parseSessionUsage(logPath)
			if parseErr != nil {
				cmd.PrintErrf("warning: could not parse session log %s: %v\n", logPath, parseErr)
			} else {
				usage = u
			}
		}

		cost, reconcileErr := daemon.ReconcileSession(ctx, session, usage)
		if reconcileErr != nil {
			results = append(results, reconcileResult{
				SessionID:    session.ID,
				Agent:        agent,
				StartedAt:    startedAt,
				LogAvailable: logPath != "" && fileExists(logPath),
				Status:       "error",
				Error:        reconcileErr.Error(),
			})
			cmd.PrintErrf("error reconciling session %s: %v\n", session.ID, reconcileErr)
			continue
		}

		status := "crashed"
		if usage != nil {
			status = "recovered"
			totalRecovered += cost
		}

		results = append(results, reconcileResult{
			SessionID:     session.ID,
			Agent:         agent,
			StartedAt:     startedAt,
			LogAvailable:  logPath != "" && fileExists(logPath),
			CostRecovered: cost,
			Status:        status,
		})
	}

	if jsonOutput {
		printJSON(results)
		return nil
	}

	cmd.Printf("%-16s %-25s %-12s %-10s %10s\n", "SESSION", "AGENT", "STARTED", "STATUS", "COST")
	cmd.Printf("%-16s %-25s %-12s %-10s %10s\n",
		strings.Repeat("-", 16), strings.Repeat("-", 25), strings.Repeat("-", 12),
		strings.Repeat("-", 10), strings.Repeat("-", 10))
	for _, r := range results {
		startShort := r.StartedAt
		if t, err := time.Parse(time.RFC3339, r.StartedAt); err == nil {
			startShort = t.Format("01-02 15:04")
		}
		costStr := "-"
		if r.CostRecovered > 0 {
			costStr = fmt.Sprintf("$%.4f", r.CostRecovered)
		}
		cmd.Printf("%-16s %-25s %-12s %-10s %10s\n",
			truncate(r.SessionID, 16), truncate(r.Agent, 25),
			startShort, r.Status, costStr)
	}

	if totalRecovered > 0 {
		cmd.Printf("\nTotal recovered: $%.4f\n", totalRecovered)
	}

	return nil
}

// fileExists returns true if the path points to an existing regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// reconcileOrphanedSessions is called from the restart loop before registering
// a new session. It finds session beads from this agent that are still "active"
// (never closed — indicating a prior crash) and recovers their cost data.
//
// Uses a short max-age (5 minutes) since in the restart loop context we know
// the previous session just ended. The 5-minute window avoids racing with
// sessions that are still legitimately running on other pods for the same agent.
func reconcileOrphanedSessions(ctx context.Context, agentName string) {
	if daemon == nil || agentName == "" {
		return
	}

	reconcileCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	orphaned, err := daemon.ListOrphanedSessions(reconcileCtx, agentName, 5*time.Minute)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not list orphaned sessions: %v\n", err)
		return
	}

	if len(orphaned) == 0 {
		return
	}

	fmt.Printf("[gb agent start] found %d orphaned session(s) to reconcile\n", len(orphaned))

	for _, session := range orphaned {
		logPath := session.Fields["session_log"]

		var usage *beadsapi.SessionUsage
		if logPath != "" && fileExists(logPath) {
			u, parseErr := parseSessionUsage(logPath)
			if parseErr != nil {
				fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not parse orphaned session log %s: %v\n", logPath, parseErr)
			} else {
				usage = u
			}
		}

		cost, reconcileErr := daemon.ReconcileSession(reconcileCtx, session, usage)
		if reconcileErr != nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] warning: could not reconcile session %s: %v\n", session.ID, reconcileErr)
			continue
		}

		if usage != nil {
			fmt.Printf("[gb agent start] reconciled session %s: recovered $%.4f\n", session.ID, cost)
		} else {
			fmt.Printf("[gb agent start] reconciled session %s: marked as crashed (no log available)\n", session.ID)
		}
	}
}
