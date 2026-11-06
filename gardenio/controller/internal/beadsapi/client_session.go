package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// SessionFields contains metadata for a session bead.
type SessionFields struct {
	Agent       string `json:"agent"`
	AgentBeadID string `json:"agent_bead_id"`
	Project     string `json:"project"`
	Role        string `json:"role"`
	Hostname    string `json:"hostname"`
	SessionLog  string `json:"session_log,omitempty"`
	StartedAt   string `json:"started_at"`
	EndedAt     string `json:"ended_at,omitempty"`
	ExitCode    int    `json:"exit_code,omitempty"`
	Resumed     bool   `json:"resumed"`
	Status      string `json:"status"` // active, completed, crashed, retired
}

// SessionUsage holds aggregated usage metrics extracted from a session log.
type SessionUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// ThinkingTokens is an ESTIMATE (chars/4 heuristic, see countThinkingTokens)
	// and is conceptually already counted inside OutputTokens. Surface it as an
	// estimate; never sum it into authoritative token totals.
	ThinkingTokens   int64   `json:"thinking_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	RequestCount     int64   `json:"request_count"`
	ToolCallCount    int64   `json:"tool_call_count"`
	ErrorCount       int64   `json:"error_count"`
	CompactionCount  int64   `json:"compaction_count"`
	// InterruptCount is the number of "[Request interrupted by user…]"
	// markers in the transcript, and LastInterruptAt the RFC3339 time of the
	// last one. They carry no prompt text: an interrupt between turns fires no
	// hook, so these let the session sweep see interrupt-then-crash
	// (gd-3HAERLWMLu). Claude transcripts only; coop-sourced harnesses leave
	// them zero.
	InterruptCount  int64  `json:"interrupt_count,omitempty"`
	LastInterruptAt string `json:"last_interrupt_at,omitempty"`
	// ContextTokens is the live context-window occupancy at the most recent
	// request: input + cache_read + cache_creation for that turn. Unlike the
	// cumulative counters above this is a *level*, not a sum — the latest
	// request wins (mirrors coop usage.rs::SessionUsage.context_tokens). It is
	// used for the gardenio_session_context_tokens gauge and deliberately NOT
	// accumulated by addSessionUsage/usageDeltas: summing levels across
	// sessions is meaningless.
	ContextTokens int64 `json:"context_tokens,omitempty"`
	// BootContextTokens is the resident context window of the FIRST turn:
	// input + cache_read + cache_creation of turn 0. This IS the tokenized boot
	// size — the base system prompt + MCP tool defs + skills + CLAUDE.md + gb
	// prime + thread ctx that every session pays before it does any work — and
	// it is the number the context-crisis epic (ka-0000003733) exists to make
	// visible. Deliberately the resident level (not cache_creation alone) so it
	// is honest on a warm-cache respawn, where the boot is mostly cache_read and
	// cache_creation would under-report it toward zero. Like ContextTokens this
	// is a per-session LEVEL, not a cumulative counter: it must NOT be summed by
	// usageDeltas. omitempty so a resumed session with no turn 0 serializes
	// nothing.
	BootContextTokens int64 `json:"boot_context_tokens,omitempty"`
}

// RegisterSession creates a new session bead linked to an agent bead.
// Returns the session bead ID.
func (c *Client) RegisterSession(ctx context.Context, fields SessionFields) (string, error) {
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("marshalling session fields: %w", err)
	}

	title := fmt.Sprintf("session:%s:%s", fields.Agent, fields.StartedAt)

	req := CreateBeadRequest{
		Title:  title,
		Type:   "session",
		Kind:   "config",
		Fields: json.RawMessage(fieldsJSON),
	}
	if fields.Project != "" {
		req.Labels = []string{"project:" + fields.Project}
	}

	id, err := c.CreateBead(ctx, req)
	if err != nil {
		return "", fmt.Errorf("creating session bead: %w", err)
	}

	// Link session to agent bead.
	if fields.AgentBeadID != "" {
		_ = c.AddDependency(ctx, id, fields.AgentBeadID, "session_of", fields.Agent)
	}

	return id, nil
}

// ExitCodeCredentialSwitch is the exit code coop uses to ask the restart loop to
// relaunch under fresh credentials (coop's EXIT_CODE_CREDENTIAL_SWITCH,
// coop/crates/cli/src/run.rs). The loop does exactly that, so it is a healthy
// mid-life restart, never a crash — which is why SessionStatusForExit treats it
// as a completed session. gb's exitCodeCredentialSwitch aliases this so the
// value cannot drift between the two packages that both act on it.
const ExitCodeCredentialSwitch = 10

// SessionStatusForExit classifies a session's terminal exit into one of the
// type:session `status` enum values — deliberately only {completed, crashed},
// the values the LIVE type config already accepts. See the enum note below.
//
// Why this exists: the old rule was `exitCode != 0 => "crashed"`, which labeled
// every clean `gb stop` a crash. 143 is 128+SIGTERM, and SIGTERM is the NORMAL
// teardown signal — `gb stop` asks coop to shut down, coop SIGTERMs the child,
// the child exits 143. So a clean stop was STRUCTURALLY guaranteed to be called
// a crash: measured over the 300 most recent session beads, 284 of 284 closed
// sessions read "crashed" and ZERO read "completed" (ka-0000003165). That made
// the field useless — a genuine crash was camouflaged among hundreds of false
// positives, and operators reading a session bead concluded a successful run had
// failed.
//
// This is the controller-side twin of ka-0000001328, the same defect in coop's
// exit_status_from_code. That fix folded in the graceful-shutdown bit; this is
// the same idea for the session-bead writer, so the two cannot drift:
//
//   - 0                     → completed (exited on its own terms)
//   - 10                    → completed (credential switch; the loop relaunches
//     deliberately — 54 of the 284 measured sessions)
//   - 143 / 130 / 129       → completed IFF the teardown was requested
//     (128 + SIGTERM/SIGINT/SIGHUP: the gb-stop and pod-terminate path). The
//     SAME code with shutdownRequested=false is a genuine mid-task kill and
//     stays crashed, so real terminations still surface.
//   - anything else          → crashed (incl. 137/SIGKILL even on the shutdown
//     path — that only happens when the child missed its grace window, which
//     coop also deliberately keeps as a failure, and -1, the reconciler's
//     unknown-exit sentinel for a session whose agent died without closing it).
//
// NOTE ON THE ENUM: the live type:session config declares
// status ∈ {active, completed, crashed, retired} and the daemon validates it
// strictly. A more expressive value ("stopped", mirroring coop) would be
// REJECTED, and because status travels in the same PATCH as the usage metrics,
// that rejection would fail the whole write and silently lose the session's
// token/cost data. So a clean stop is reported as "completed" rather than
// introducing a new value. Widening the enum is a separate, deploy-ordered
// change (write only after every daemon accepts it).
func SessionStatusForExit(exitCode int, shutdownRequested bool) string {
	switch exitCode {
	case 0, ExitCodeCredentialSwitch:
		return "completed"
	case 143, 130, 129: // 128 + {SIGTERM, SIGINT, SIGHUP}
		if shutdownRequested {
			return "completed"
		}
		return "crashed"
	default:
		return "crashed"
	}
}

// SessionStatusDependsOnShutdown reports whether SessionStatusForExit's verdict
// for this exit code actually varies with the shutdownRequested bit — i.e. this
// is one of the signal-termination codes. Callers use it to skip resolving the
// bit (which costs a bead read) for the codes that classify identically either
// way: 0 and 10 are always "completed", everything else always "crashed". Keeping
// the code list here means it cannot drift from the classifier's own switch.
func SessionStatusDependsOnShutdown(exitCode int) bool {
	return SessionStatusForExit(exitCode, true) != SessionStatusForExit(exitCode, false)
}

// Restart-cause label enum — the bounded set of values ClassifyRestartCause
// writes to a session bead's restart_cause field.
const (
	RestartCauseClean            = "clean"
	RestartCauseSIGTERM          = "sigterm"
	RestartCauseOOMKilled        = "oomkill"
	RestartCauseCredentialSwitch = "credential_switch"
	RestartCauseCrash            = "crash"
)

// ClassifyRestartCause classifies a session's terminal exit into the bounded
// restart_cause enum stamped on the session bead at close (gd-5MTDWjwNjm) —
// the queryable-over-time form of the exit-code split that previously
// required a manual bead scan. Distinct from SessionStatusForExit's
// completed/crashed `status`, which is frozen to what the daemon's
// type:session config strictly validates (ka-0000003165); restart_cause is a
// brand-new field free to carry a richer classification without touching
// that validated enum.
//
//   - 0                              → clean
//   - 10 (ExitCodeCredentialSwitch)  → credential_switch (the loop relaunches
//     deliberately under fresh credentials)
//   - 143 / 130 / 129 (128 + {SIGTERM,SIGINT,SIGHUP}) → sigterm IFF the
//     teardown was requested (gb stop / pod terminate); otherwise crash —
//     mirrors SessionStatusForExit's shutdownRequested nuance so a clean
//     `gb stop` is not miscounted as a crash here either.
//   - 137 (128 + SIGKILL)            → oomkill, a best-effort proxy: SIGKILL
//     is the kernel OOM-killer's signal of choice, and this in-pod close path
//     has no access to the K8s-level OOMKilled container-status signal
//     (statusreporter.podWasOOMKilled is server-side only, reading the pod's
//     ContainerStatuses via the K8s API). A non-OOM SIGKILL (e.g. an operator
//     `kill -9`) would also land here — an accepted approximation for a
//     starting-point classifier (thresholds/mapping are explicitly not final,
//     see the alert values.yaml comments).
//   - anything else                  → crash
func ClassifyRestartCause(exitCode int, shutdownRequested bool) string {
	switch exitCode {
	case 0:
		return RestartCauseClean
	case ExitCodeCredentialSwitch:
		return RestartCauseCredentialSwitch
	case 137: // 128 + SIGKILL
		return RestartCauseOOMKilled
	case 143, 130, 129: // 128 + {SIGTERM, SIGINT, SIGHUP}
		if shutdownRequested {
			return RestartCauseSIGTERM
		}
		return RestartCauseCrash
	default:
		return RestartCauseCrash
	}
}

// CloseSession closes a session bead with final metadata and optional usage stats.
//
// Prefer CloseSessionWithShutdown when the caller knows whether the teardown was
// requested; this wrapper assumes it was NOT, so a signal-termination exit is
// reported as a crash. That is the conservative reading for callers with no
// shutdown signal to offer (e.g. the orphan reconciler, whose sessions are by
// definition ones nobody closed cleanly).
//
// CloseBead is always attempted, even when the preceding field update fails —
// otherwise a transient PATCH failure (network blip, type-config validation
// error) leaves the bead's main Status at "open" forever, accumulating ghost
// session beads that the orphan reconciler never re-visits (ka-0000008049).
// The first non-nil error is returned; both legs are best-effort and
// independent.
func (c *Client) CloseSession(ctx context.Context, sessionBeadID string, exitCode int, sessionLog string, usage *SessionUsage) error {
	return c.CloseSessionWithShutdown(ctx, sessionBeadID, exitCode, sessionLog, usage, false)
}

// CloseSessionWithShutdown is CloseSession with the graceful-shutdown bit the
// status classifier needs. shutdownRequested must be true when THIS process
// initiated or observed the teardown (`gb stop` set stop_requested, or the
// restart loop's signal context fired) — see SessionStatusForExit.
func (c *Client) CloseSessionWithShutdown(ctx context.Context, sessionBeadID string, exitCode int, sessionLog string, usage *SessionUsage, shutdownRequested bool) error {
	fields := map[string]any{
		"ended_at":      time.Now().UTC().Format(time.RFC3339),
		"exit_code":     exitCode,
		"status":        SessionStatusForExit(exitCode, shutdownRequested),
		"restart_cause": ClassifyRestartCause(exitCode, shutdownRequested),
	}
	if sessionLog != "" {
		fields["session_log"] = sessionLog
	}
	if usage != nil {
		fields["input_tokens"] = usage.InputTokens
		fields["output_tokens"] = usage.OutputTokens
		// thinking_tokens is an ESTIMATE (chars/4 heuristic; see
		// countThinkingTokens) and is conceptually already counted inside
		// output_tokens. It is written here for parity with the agent rollup
		// (usageDeltas), not as an authoritative token total — do not sum it
		// into "real" token counts.
		fields["thinking_tokens"] = usage.ThinkingTokens
		fields["cache_read_tokens"] = usage.CacheReadTokens
		fields["cache_write_tokens"] = usage.CacheWriteTokens
		// Only write the in-pod cost estimate when it carries information: a $0
		// estimate (the common case — coop rarely passes response_cost through)
		// must not clobber a cost another writer already put on the session bead
		// (the controller's settlement writes settled_cost_usd, and may in the
		// future write total_cost_usd too).
		if usage.TotalCostUSD > 0 {
			fields["total_cost_usd"] = usage.TotalCostUSD
		}
		fields["request_count"] = usage.RequestCount
		fields["tool_call_count"] = usage.ToolCallCount
		fields["error_count"] = usage.ErrorCount
		fields["compaction_count"] = usage.CompactionCount
		// Only when non-zero, so a daemon older than the type:session
		// declaration of these keys does not 400 every close.
		if usage.InterruptCount > 0 {
			fields["interrupt_count"] = usage.InterruptCount
		}
		if usage.LastInterruptAt != "" {
			fields["last_interrupt_at"] = usage.LastInterruptAt
		}
	}

	updateErr := c.UpdateBeadFieldsTyped(ctx, sessionBeadID, fields)
	closeErr := c.CloseBead(ctx, sessionBeadID, nil)

	if updateErr != nil {
		return fmt.Errorf("updating session bead %s: %w", sessionBeadID, updateErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing session bead %s: %w", sessionBeadID, closeErr)
	}
	return nil
}

// usageDeltas converts a SessionUsage into a map of field deltas for atomic
// increment. This is shared by UpdateAgentUsage and UpdateAccountUsage.
func usageDeltas(usage *SessionUsage) map[string]float64 {
	return map[string]float64{
		"total_input_tokens":       float64(usage.InputTokens),
		"total_output_tokens":      float64(usage.OutputTokens),
		"total_thinking_tokens":    float64(usage.ThinkingTokens),
		"total_cache_read_tokens":  float64(usage.CacheReadTokens),
		"total_cache_write_tokens": float64(usage.CacheWriteTokens),
		"total_cost_usd":           usage.TotalCostUSD,
		"total_request_count":      float64(usage.RequestCount),
		"total_tool_calls":         float64(usage.ToolCallCount),
		"total_errors":             float64(usage.ErrorCount),
		"total_compactions":        float64(usage.CompactionCount),
		"session_count":            1,
	}
}

// UpdateAgentUsage atomically accumulates session usage metrics onto the agent
// bead using the server-side increment API. This eliminates the previous
// read-modify-write race where two sessions closing simultaneously could lose
// one update.
func (c *Client) UpdateAgentUsage(ctx context.Context, agentBeadID string, usage *SessionUsage) error {
	if usage == nil {
		return nil
	}
	if err := c.IncrementBeadFields(ctx, agentBeadID, usageDeltas(usage)); err != nil {
		return fmt.Errorf("incrementing agent bead %s usage: %w", agentBeadID, err)
	}
	return nil
}

// UpdateAccountUsage atomically accumulates session usage metrics onto the
// account bead. Uses the same atomic increment as UpdateAgentUsage.
func (c *Client) UpdateAccountUsage(ctx context.Context, accountBeadID string, usage *SessionUsage) error {
	if usage == nil {
		return nil
	}
	if err := c.IncrementBeadFields(ctx, accountBeadID, usageDeltas(usage)); err != nil {
		return fmt.Errorf("incrementing account bead %s usage: %w", accountBeadID, err)
	}
	return nil
}

// FindAccountBeadIDForProject resolves a project name to its active account
// bead ID. The resolution chain is:
//  1. List account beads labeled project:<name> with status=active — take the
//     alphabetically-first match (same ordering as refreshProjectCache's
//     buildProjectAccountOverride so pod-spawn and usage accounting agree).
//  2. Fall back to the project bead's "account" field.
//  3. Look up the account bead by name and return its ID.
//
// Returns ("", nil) if no account is configured for the project.
func (c *Client) FindAccountBeadIDForProject(ctx context.Context, project string) (string, error) {
	if project == "" {
		return "", nil
	}

	// Step 1: Prefer the label-scoped account pool. ListAccountBeadsByProject
	// returns active accounts sorted alphabetically by name.
	labeled, err := c.ListAccountBeadsByProject(ctx, project)
	if err != nil {
		return "", fmt.Errorf("listing account beads by project: %w", err)
	}

	var accountName string
	if len(labeled) > 0 {
		accountName = labeled[0].Name
	}

	// Step 2: Fall back to project bead's account field.
	if accountName == "" {
		projects, err := c.ListProjectBeads(ctx)
		if err != nil {
			return "", fmt.Errorf("listing project beads: %w", err)
		}
		if p, ok := projects[project]; ok {
			accountName = p.Account
		}
	}

	if accountName == "" {
		return "", nil // no account configured
	}

	// Step 3: Find the account bead by name (need the bead ID).
	resp, err := c.listBeads(ctx, []string{"account"}, activeStatuses)
	if err != nil {
		return "", fmt.Errorf("listing account beads: %w", err)
	}

	for _, b := range resp.Beads {
		if b.Title == accountName {
			return b.ID, nil
		}
	}

	return "", nil // account bead not found by name
}

// ListSessionBeads returns session beads, optionally filtered by agent name.
func (c *Client) ListSessionBeads(ctx context.Context, agentName string, limit int) ([]*BeadDetail, error) {
	q := ListBeadsQuery{
		Types: []string{"session"},
		Sort:  "-created_at",
		Limit: limit,
	}
	if agentName != "" {
		q.Search = agentName
	}

	result, err := c.ListBeadsFiltered(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing session beads: %w", err)
	}
	return result.Beads, nil
}

// ListOrphanedSessions returns session beads that still have status=active
// but are older than maxAge. These represent sessions whose agent crashed
// before closeSessionBead could run, leaving cost data unrecovered.
func (c *Client) ListOrphanedSessions(ctx context.Context, agentName string, maxAge time.Duration) ([]*BeadDetail, error) {
	q := ListBeadsQuery{
		Types:    []string{"session"},
		Statuses: activeStatuses,
		Sort:     "-created_at",
		Limit:    100,
	}
	if agentName != "" {
		q.Search = agentName
	}

	result, err := c.ListBeadsFiltered(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing session beads: %w", err)
	}

	cutoff := time.Now().Add(-maxAge)
	var orphaned []*BeadDetail
	for _, b := range result.Beads {
		if b.Fields["status"] != "active" {
			continue
		}
		startedAt := b.Fields["started_at"]
		if startedAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, startedAt)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			orphaned = append(orphaned, b)
		}
	}
	return orphaned, nil
}

// ReconcileSession closes an orphaned session bead and accumulates its usage
// onto the agent and account beads. If usage is nil, the session is marked as
// crashed with no cost recovery. Returns the recovered cost (0 if none).
func (c *Client) ReconcileSession(ctx context.Context, session *BeadDetail, usage *SessionUsage) (float64, error) {
	exitCode := -1 // unknown exit code for crashed sessions
	sessionLog := session.Fields["session_log"]

	if err := c.CloseSession(ctx, session.ID, exitCode, sessionLog, usage); err != nil {
		return 0, fmt.Errorf("closing orphaned session %s: %w", session.ID, err)
	}

	var recoveredCost float64
	if usage != nil {
		recoveredCost = usage.TotalCostUSD

		agentBeadID := session.Fields["agent_bead_id"]
		if agentBeadID != "" {
			if err := c.UpdateAgentUsage(ctx, agentBeadID, usage); err != nil {
				return recoveredCost, fmt.Errorf("updating agent usage for %s: %w", agentBeadID, err)
			}
		}

		project := session.Fields["project"]
		if project != "" {
			accountBeadID, err := c.FindAccountBeadIDForProject(ctx, project)
			if err == nil && accountBeadID != "" {
				if err := c.UpdateAccountUsage(ctx, accountBeadID, usage); err != nil {
					return recoveredCost, fmt.Errorf("updating account usage for %s: %w", accountBeadID, err)
				}
			}
		}
	}

	return recoveredCost, nil
}
