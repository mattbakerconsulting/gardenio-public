package main

// Escalation executor for gb agent start --local --runner.
//
// The daemon approves an execution_target=local escalation for one named
// runner and mails the agent joined on it control:escalation-approved (spec
// §8.1, §8.2). The mail subscriber hands that mail to HandleControl, which
// nudges the local coop session with the escalation prompt and records the
// delivery on the escalation bead. It never closes an escalation on
// delivery; the agent reports the outcome with `gb escalation close`.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/coopapi"
	"gardenio/controller/internal/mailproto"
)

// Knobs for the readiness gate + nudge retry loop in executeEscalation.
// Variables (not constants) so tests can override them to keep runtimes fast
// without spinning up a real coop session.
var (
	escalationReadyPollInterval = 2 * time.Second
	escalationReadyMaxPolls     = 60 // ~120s, generous for agent setup dialogs
	escalationNudgeRetryDelay   = 2 * time.Second
	escalationNudgeMaxAttempts  = 3
)

// escalationWaitForReady polls coop until the agent reaches idle/working
// state, or escalationReadyMaxPolls iterations elapse. Returns true on
// success, false on timeout or ctx cancellation. Mirrors the readiness gate
// used by the startup-prompt sequence (see waitForReady in
// agent_start_prompt.go) but with its own overridable interval so tests can
// drive it without depending on the prompt-side knobs.
func escalationWaitForReady(ctx context.Context, coop *coopapi.Client) bool {
	for range escalationReadyMaxPolls {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		state, err := coop.GetAgentState(ctx)
		if err == nil && (state.State == "idle" || state.State == "working") {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(escalationReadyPollInterval):
		}
	}
	return false
}

// escalationDeliverNudge sends `prompt` to coop with up to
// escalationNudgeMaxAttempts attempts, treating both transport errors and
// Delivered=false responses as retryable. Returns true on a Delivered=true
// response. Mirrors deliverNudge in agent_start_prompt.go but with its own
// overridable backoff so escalation-side tests stay isolated.
func escalationDeliverNudge(ctx context.Context, coop *coopapi.Client, prompt, label string) bool {
	for attempt := 1; attempt <= escalationNudgeMaxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		resp, err := coop.Nudge(ctx, prompt)
		if err == nil && resp != nil && resp.Delivered {
			return true
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] warning: %s nudge attempt %d/%d failed: %v\n",
				label, attempt, escalationNudgeMaxAttempts, err)
		} else if resp != nil {
			fmt.Fprintf(os.Stderr, "[gb agent start] warning: %s nudge attempt %d/%d not delivered: %s\n",
				label, attempt, escalationNudgeMaxAttempts, resp.Reason)
		}
		if attempt == escalationNudgeMaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(escalationNudgeRetryDelay):
		}
	}
	return false
}

// escalationWatcher runs the approved local escalations the daemon mails this
// agent, delivering each to the local agent's coop session.
type escalationWatcher struct {
	daemon    *beadsapi.Client
	coop      *coopapi.Client
	agentName string
	runnerID  string // the runner this agent joined on (gb runner join)
	logger    *slog.Logger
	// execute runs one escalation; executeEscalation, stubbed in tests.
	execute    func(ctx context.Context, escalationID string)
	mu         sync.Mutex      // guards processing
	processing map[string]bool // dedup: escalation IDs currently being processed
}

// escalationWatcherConfig holds the configuration for the escalation watcher.
type escalationWatcherConfig struct {
	Daemon    *beadsapi.Client
	CoopURL   string
	AgentName string
	RunnerID  string
	Logger    *slog.Logger
}

// newEscalationWatcher creates a new escalation watcher.
func newEscalationWatcher(cfg escalationWatcherConfig) *escalationWatcher {
	w := &escalationWatcher{
		daemon:     cfg.Daemon,
		coop:       coopapi.New(cfg.CoopURL),
		agentName:  cfg.AgentName,
		runnerID:   cfg.RunnerID,
		logger:     cfg.Logger,
		processing: make(map[string]bool),
	}
	w.execute = w.executeEscalation
	return w
}

// HandleControl is the mailsub OnControl hook: a control:escalation-approved
// mail addressed to this agent names an escalation the daemon has approved
// for THIS runner (spec §8.1). The daemon sends it once per approval, to the
// executor it recorded (the agent joined on target_runner), or parks it for
// the runner, which the subscriber claims on its drain. Nothing else
// triggers local execution. The mail is closed once the escalation has run,
// so a later session on this agent name does not drain and run it again.
func (w *escalationWatcher) HandleControl(ctx context.Context, verb string, mail beadsapi.BeadDetail) {
	if verb != mailproto.ControlEscalationApproved {
		w.logger.Warn("unhandled control mail", "verb", verb, "mail", mail.ID)
		return
	}
	escalationID := mail.Fields["escalation_id"]
	if escalationID == "" {
		w.logger.Warn("escalation-approved mail without escalation_id", "mail", mail.ID)
		return
	}
	w.mu.Lock()
	if w.processing[escalationID] {
		w.mu.Unlock()
		return
	}
	w.processing[escalationID] = true
	w.mu.Unlock()
	w.logger.Info("escalation approved for local execution", "escalation_id", escalationID, "mail", mail.ID)
	go func() {
		w.execute(ctx, escalationID)
		w.closeMail(ctx, mail)
	}()
}

// localControlHandler is the local session's one mailsub OnControl hook:
// control:escalation-approved goes to escalation (the executor, nil when none
// runs), every other verb to other (the permission handler, which logs a verb
// it does not know).
func localControlHandler(escalation, other func(context.Context, string, beadsapi.BeadDetail)) func(context.Context, string, beadsapi.BeadDetail) {
	return func(ctx context.Context, verb string, mail beadsapi.BeadDetail) {
		if verb == mailproto.ControlEscalationApproved && escalation != nil {
			escalation(ctx, verb, mail)
			return
		}
		other(ctx, verb, mail)
	}
}

// closeMail closes handled escalation-approved mail stamped control_acted,
// without which the daemon refuses this agent's token a close of control
// mail. Best-effort: left open, a later session re-drains it, and
// executeEscalation skips an escalation already delivered or closed.
func (w *escalationWatcher) closeMail(ctx context.Context, mail beadsapi.BeadDetail) {
	if w.daemon == nil || mail.Status == "closed" || mail.ID == "" {
		return
	}
	acted := map[string]string{mailproto.FieldControlActed: time.Now().UTC().Format(time.RFC3339)}
	if err := w.daemon.CloseBead(ctx, mail.ID, acted); err != nil {
		w.logger.Warn("failed to close escalation-approved mail", "mail", mail.ID, "error", err)
	}
}

// executeEscalation fetches the full escalation, nudges the local agent
// with the operation prompt, and captures the result.
func (w *escalationWatcher) executeEscalation(ctx context.Context, escalationID string) {
	defer func() {
		w.mu.Lock()
		delete(w.processing, escalationID)
		w.mu.Unlock()
	}()

	// Fetch full escalation details.
	detail, err := w.daemon.GetEscalation(ctx, escalationID)
	if err != nil {
		w.logger.Error("failed to fetch escalation details",
			"escalation_id", escalationID, "error", err)
		w.captureFailure(ctx, escalationID, fmt.Sprintf("failed to fetch escalation: %v", err))
		return
	}

	// kbeads' GET /v1/escalations/{id} returns two views: `escalation` carries
	// just the extracted escalation-specific fields (operation_type,
	// approved_by, …) and has no bead id/title, while `issue` carries the full
	// bead. Prefer `issue` for metadata; fall back to `escalation` if the
	// server shape ever changes to put a full bead there.
	if detail.Escalation == nil || detail.Escalation.ID == "" {
		if detail.Issue != nil && detail.Issue.ID != "" {
			detail.Escalation = detail.Issue
		} else {
			w.logger.Error("escalation detail is nil", "escalation_id", escalationID)
			w.captureFailure(ctx, escalationID, "escalation bead not found")
			return
		}
	}

	// A redrained mail (the close below it failed, or the session restarted
	// before it) must not run an escalation twice.
	if detail.Escalation.Status == "closed" {
		w.logger.Info("escalation already closed; not delivering", "escalation_id", escalationID)
		return
	}
	// The mail's escalation_id is not proof of approval: the daemon's control
	// mail guard admits senders other than the approval path. Run only what
	// the escalation itself says was approved for this runner. No failure is
	// captured on a refusal, since the escalation may not be this agent's.
	if reason := w.refuseReason(detail.Escalation); reason != "" {
		w.logger.Warn("refusing escalation-approved mail", "escalation_id", escalationID, "reason", reason)
		return
	}
	if to := fieldValue(detail.Escalation.Fields, "prompt_delivered_at"); to != "" {
		w.logger.Info("escalation already delivered; not delivering again",
			"escalation_id", escalationID, "delivered_at", to,
			"delivered_to", fieldValue(detail.Escalation.Fields, "prompt_delivered_to"))
		return
	}

	// Build the nudge prompt from the escalation.
	prompt := w.buildEscalationPrompt(detail)

	// Wait for the local agent (coop) to be ready before injecting the
	// prompt. Without this gate the watcher can race the startup-prompt
	// sequence (runLocalNudgeSequence): the runner sees the "Escalation
	// watcher started" banner and immediately approves the escalation, but
	// coop is still in setup state (e.g. bypass_permissions dialog) and
	// returns 503 NotReady from POST /api/v1/agent/nudge, dropping the
	// nudge entirely.
	if !escalationWaitForReady(ctx, w.coop) {
		w.logger.Error("local agent not ready for escalation nudge",
			"escalation_id", escalationID)
		w.captureFailure(ctx, escalationID, "local agent not ready for nudge")
		return
	}

	// Nudge the local coop session with the escalation prompt.
	w.logger.Info("nudging local agent with escalation",
		"escalation_id", escalationID)
	if !escalationDeliverNudge(ctx, w.coop, prompt, fmt.Sprintf("escalation %s", escalationID)) {
		w.logger.Error("failed to nudge local agent",
			"escalation_id", escalationID)
		w.captureFailure(ctx, escalationID, "failed to nudge local agent after retries")
		return
	}

	w.logger.Info("escalation nudge delivered",
		"escalation_id", escalationID)

	// Record the delivery WITHOUT closing. Delivering the prompt is not the
	// operation succeeding: the result endpoint closes the escalation (and a
	// success releases its close-on-execute wrappers and unblocks dependents),
	// so capturing here marked work done before any of it ran (gd-X8q4N828KL).
	// The escalation stays in_progress until the agent itself reports via
	// `gb escalation close`. result, result_status and executor are the
	// daemon's (spec D8, P7 Tasks 11 and 13), so the record has its own fields.
	if err := w.daemon.UpdateBeadFieldsTyped(ctx, escalationID, map[string]any{
		"prompt_delivered_at": time.Now().UTC().Format(time.RFC3339),
		"prompt_delivered_to": w.agentName,
	}); err != nil {
		w.logger.Warn("failed to record escalation prompt delivery",
			"escalation_id", escalationID, "error", err)
	}
}

// refuseReason reports why this agent must not run esc, or "" when it may.
// It reads only fields the daemon guards (escalation_field_guard.go), which a
// crew cannot forge: status, the daemon's execution_dispatch and executor, and
// target_runner, fixed at filing as the approver saw it. esc must be approved
// (in_progress) and claimed for local dispatch, and a target_runner or stamped executor it names must
// be this agent or its runner. With neither named (target_owner resolved no
// joined agent at approval) the mail was parked for a runner, and the
// subscriber claims only its own runner's.
func (w *escalationWatcher) refuseReason(esc *beadsapi.BeadDetail) string {
	if esc.Status != "in_progress" {
		return fmt.Sprintf("status is %q, not in_progress (not approved)", esc.Status)
	}
	if d := fieldValue(esc.Fields, "execution_dispatch"); d != "local" {
		return fmt.Sprintf("execution_dispatch is %q, not local", d)
	}
	if r := fieldValue(esc.Fields, "target_runner"); r != "" && r != w.runnerID {
		return fmt.Sprintf("target_runner %q is not this agent's runner %q", r, w.runnerID)
	}
	if ex := fieldValue(esc.Fields, "executor"); ex != "" && ex != w.agentName && (w.runnerID == "" || ex != w.runnerID) {
		return fmt.Sprintf("executor %q is neither this agent nor its runner", ex)
	}
	return ""
}

// buildEscalationPrompt constructs the nudge message from escalation details.
func (w *escalationWatcher) buildEscalationPrompt(detail *beadsapi.EscalationDetail) string {
	esc := detail.Escalation
	var b strings.Builder

	fmt.Fprintf(&b, "ESCALATION APPROVED: %s (ID: %s)\n\n", esc.Title, esc.ID)

	opType := fieldValue(esc.Fields, "operation_type")
	if opType != "" {
		fmt.Fprintf(&b, "Operation type: %s\n", opType)
	}

	opSpec := fieldValue(esc.Fields, "operation_spec")
	if opSpec != "" {
		fmt.Fprintf(&b, "Operation spec:\n%s\n\n", opSpec)
	}

	escalationCtx := fieldValue(esc.Fields, "context")
	if escalationCtx != "" {
		fmt.Fprintf(&b, "Context: %s\n\n", escalationCtx)
	}

	approvedBy := fieldValue(esc.Fields, "approved_by")
	if approvedBy != "" {
		fmt.Fprintf(&b, "Approved by: %s\n", approvedBy)
	}

	fmt.Fprintf(&b, "\nExecute this approved escalation. Report results via: %s",
		bridge.EscalationCloseCommand(esc.ID))

	return b.String()
}

// captureFailure records a failure result on the escalation bead.
func (w *escalationWatcher) captureFailure(ctx context.Context, escalationID, reason string) {
	if err := w.daemon.CaptureEscalationResult(ctx, escalationID, beadsapi.CaptureEscalationResultRequest{
		Executor:        w.agentName,
		ExecutionTarget: "local",
		ResultStatus:    "failure",
		Result:          map[string]string{"error": reason},
	}); err != nil {
		w.logger.Error("failed to capture escalation failure",
			"escalation_id", escalationID, "error", err)
	}
}

// fieldValue extracts a field value from a bead's fields map.
func fieldValue(fields map[string]string, key string) string {
	if fields == nil {
		return ""
	}
	return fields[key]
}
