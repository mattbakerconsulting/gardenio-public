// Package bridge provides the escalation execution bridge.
//
// Escalations watches the kbeads SSE event stream for escalation bead
// lifecycle events: it posts and updates the Slack cards, re-surfaces stalled
// escalations, and nudges the requesting agent to continue once an escalation
// completes. It executes nothing. The daemon's approve handler claims the one
// executor of an approved escalation (execution_dispatch, P7 Task 12): its own
// replay, the local watcher, or the service-account executor it spawns.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/agentwake"
	"gardenio/controller/internal/beadsapi"
)

// Escalations watches the kbeads SSE event stream for escalation bead
// lifecycle events (Slack cards, pingbacks, follow-through nudges).
type Escalations struct {
	daemon             BeadClient
	users              *UserDirectory
	notifier           Notifier
	escalationNotifier EscalationNotifier
	logger             *slog.Logger

	// nudge delivers a nudge to a named agent; NudgeAgent in production, a fake
	// in tests.
	nudge func(ctx context.Context, agentName, message string) error

	// Track which escalations' approvals we've already handled, so an SSE
	// reconnect does not repeat the Slack update or requester nudge.
	triggeredMu sync.Mutex
	triggered   map[string]time.Time // escalation bead ID → trigger time

	// Track requesting agents that need nudge after execution completes.
	pendingMu sync.Mutex
	pending   map[string]string // escalation bead ID → requesting agent name

	// In-memory guard against double-posting a pingback while its
	// pingback_notified_at stamp is still in flight: SSE update events
	// between the Slack post and the stamp landing would otherwise re-fire.
	// Durable dedup is the requested_at == notified_at field handshake.
	pingbackMu   sync.Mutex
	pingbackSeen map[string]string // escalation bead ID → last pingback_requested_at handled
	queueSeen    map[string]string // escalation bead ID → last queue_alert_requested_at handled
	offlineSeen  map[string]string // escalation bead ID → held_by_offline_at handled

	// crossOperator DMs the addressed operator when an escalation is approved
	// or terminated by a different identity (ka-0000002131, operator ruling on
	// ka-0000004303). nil = disabled.
	crossOperator *CrossOperatorNotify

	// operatorDM DMs the addressed operator when an escalation is CREATED for
	// them, because the channel card routes by requesting agent and so reaches
	// that human only by luck (ka-0000002669). nil = disabled.
	operatorDM *EscalationOperatorDM

	// spawnMu/recentSpawn dedup follow-through respawns per escalation, mirroring
	// Decisions.spawnMu/recentSpawn: a burst of redelivered SSE close events for
	// the same escalation must not mint more than one follow-through crew.
	spawnMu     sync.Mutex
	recentSpawn map[string]time.Time // escalation bead ID → last respawn time (dedup)
}

// escalationSpawnCooldown is the minimum interval between follow-through
// respawns for the same escalation. Mirrors decisionSpawnCooldown.
const escalationSpawnCooldown = 10 * time.Minute

// EscalationsConfig holds configuration for the Escalations watcher.
type EscalationsConfig struct {
	Daemon             BeadClient
	Users              *UserDirectory
	Notifier           Notifier
	EscalationNotifier EscalationNotifier
	Logger             *slog.Logger
	// CrossOperator, when set, DMs the addressed operator whenever an
	// escalation is approved or closed by a different identity (ka-0000002131).
	CrossOperator *CrossOperatorNotify
	// OperatorDM, when set, DMs the addressed operator whenever an escalation
	// is created for them (ka-0000002669).
	OperatorDM *EscalationOperatorDM
}

// NewEscalations creates a new escalation execution watcher.
func NewEscalations(cfg EscalationsConfig) *Escalations {
	e := &Escalations{
		daemon:             cfg.Daemon,
		users:              cfg.Users,
		notifier:           cfg.Notifier,
		escalationNotifier: cfg.EscalationNotifier,
		logger:             cfg.Logger,
		triggered:          make(map[string]time.Time),
		pending:            make(map[string]string),
		pingbackSeen:       make(map[string]string),
		queueSeen:          make(map[string]string),
		offlineSeen:        make(map[string]string),
		crossOperator:      cfg.CrossOperator,
		operatorDM:         cfg.OperatorDM,
		recentSpawn:        make(map[string]time.Time),
	}
	e.nudge = func(ctx context.Context, agentName, message string) error {
		return NudgeAgent(ctx, e.daemon, e.logger, agentName, message)
	}
	return e
}

// RegisterHandlers registers SSE event handlers for escalation bead events.
func (e *Escalations) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.created", e.handleCreated)
	stream.On("beads.bead.updated", e.handleUpdated)
	stream.On("beads.bead.closed", e.handleClosed)
	e.logger.Info("escalations watcher registered SSE handlers",
		"topics", []string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed"})
}

// handleCreated logs when a new escalation request is created.
func (e *Escalations) handleCreated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != "escalation" {
		return
	}

	e.logger.Info("escalation bead created",
		"id", bead.ID,
		"title", bead.Title,
		"operation_type", bead.Fields["operation_type"],
		"requesting_agent", bead.Fields["requesting_agent"])

	// Track the requesting agent for later nudge on completion.
	if agent := bead.Fields["requesting_agent"]; agent != "" {
		e.pendingMu.Lock()
		e.pending[bead.ID] = agent
		e.pendingMu.Unlock()
	}

	// Notify Slack that a new escalation request has been created.
	if e.escalationNotifier != nil {
		if err := e.escalationNotifier.NotifyNewEscalation(ctx, *bead); err != nil {
			e.logger.Error("failed to notify escalation creation", "id", bead.ID, "error", err)
		}
	}

	// DM the addressed human, if any. Runs AFTER the card so the canonical
	// public record exists first, and independently of it: the card post can
	// fail (a channel the bot was removed from) without costing the operator
	// their personal ping. Nil-safe when there is no Slack bot.
	e.operatorDM.EscalationCreated(ctx, *bead)
	e.maybeNotifyHolderOffline(ctx, *bead)
}

// handleUpdated detects when an escalation is approved and updates its Slack
// card (it executes nothing: the daemon claims the executor). It also watches for
// pingback / queue-alert requests (ka-0000004385) stamped by `gb escalation
// pingback`, `gb escalation reflag`, or the EscalationPingbackLoop, and
// re-notifies Slack for still-unapproved escalations.
func (e *Escalations) handleUpdated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != "escalation" {
		return
	}

	// A PATCH that sets status=closed terminates the escalation while emitting
	// ONLY beads.bead.updated: kbeads updateBead (beads.go) publishes
	// TopicBeadUpdated and calls afterBeadClosed but never TopicBeadClosed, and
	// escalationBlocksGenericClose is not consulted on that path. So a
	// `kd update <esc> --status closed` is invisible to handleClosed. Catch it
	// here so the accountability DM cannot be dodged by using the wrong verb.
	// CrossOperatorNotify dedups by bead id, so a normal close that also emits
	// an update event still yields exactly one message.
	if statusChangedToClosed(bead) {
		e.crossOperator.EscalationResolved(ctx, *bead)
	}
	e.maybeNotifyHolderOffline(ctx, *bead)

	approvedBy := bead.Fields["approved_by"]
	if approvedBy == "" {
		// A hand-up (D5) is a personal ask of the human who now holds it.
		e.operatorDM.EscalationHandedUp(ctx, *bead)
		// Only unapproved owner-audience escalations are re-surfaced (#756
		// audience, which follows held_by, D11): an in-hand one is a
		// coordinator's queue, and its way to a human is hand-up, which makes
		// it the owner's and stamps its own pingback.
		if beadsapi.EscalationForOwner(bead.Fields) {
			e.maybeNotifyPingback(ctx, *bead)
			e.maybeNotifyQueueAlert(ctx, *bead)
		}
		return
	}

	// approved_by stays set for the rest of the escalation's life, so every
	// later update (a short_title fill, a label, a comment stamp) also arrives
	// here carrying it. The triggered map below is in memory and empty after a
	// bridge restart, so keying on approved_by alone replayed the approval on
	// any cosmetic write: a groomer's short_title pass woke three executors at
	// once, one on an owner-held merge (gd-TMYvBxNY6h). Act only on the event
	// that IS the approval.
	if !approvalTransition(bead) {
		return
	}

	// Approval is the privileged act — it authorizes the merge / apply /
	// credential mint. Notify the addressed operator HERE as well as on close,
	// because an approved escalation whose replay fails is left OPEN by the
	// daemon and may never emit a close event at all. Runs before the
	// triggered-dedup below so a redelivered approval still reaches the guard
	// in CrossOperatorNotify rather than being swallowed here.
	e.crossOperator.EscalationApproved(ctx, *bead)

	// P7 Task 12 (spec D10): the bridge executes nothing on approval. The
	// daemon's approve handler chooses and claims the one executor
	// (execution_dispatch); only the Slack update, the requester bookkeeping
	// and the execute_by=requester nudge stay here. Dedup so a redelivered
	// approval does not repeat them.
	e.triggeredMu.Lock()
	if _, seen := e.triggered[bead.ID]; seen {
		e.triggeredMu.Unlock()
		return
	}
	e.triggered[bead.ID] = time.Now()
	e.triggeredMu.Unlock()

	e.logger.Info("escalation approved",
		"id", bead.ID,
		"approved_by", approvedBy,
		"execution_dispatch", bead.Fields["execution_dispatch"],
		"operation_type", bead.Fields["operation_type"])

	// Notify Slack that escalation was approved.
	if e.escalationNotifier != nil {
		if err := e.escalationNotifier.UpdateEscalationApproved(ctx, *bead); err != nil {
			e.logger.Error("failed to notify escalation approval", "id", bead.ID, "error", err)
		}
	}

	// Track requesting agent for nudge on completion.
	if agent := bead.Fields["requesting_agent"]; agent != "" {
		e.pendingMu.Lock()
		e.pending[bead.ID] = agent
		e.pendingMu.Unlock()
	}

	// execute_by=requester (escalation routing D1, e.g. a land's merge its
	// owner approved): the requester does the work itself, so tell it. This
	// is a nudge, not an executor; nothing else on approval spawns or nudges.
	if strings.TrimSpace(bead.Fields["execute_by"]) == beadsapi.LaneRequester {
		e.nudgeRequesterToExecute(ctx, *bead)
	}
}

// approvalTransition reports whether an update event records the approval of
// an escalation, rather than some later write to an already-approved one. The
// daemon's approve paths (handleApproveEscalation, the spawn-policy auto-approve)
// publish Changes {"approved_by": ...} and/or {"status": "in_progress"}; a
// generic PATCH that sets approved_by shows it as a per-field change. Anything
// else, including an event with no Changes at all, is not an approval: on this
// path a missed wake costs a manual nudge, a false one spawns a privileged
// executor.
func approvalTransition(bead *BeadEvent) bool {
	if bead == nil {
		return false
	}
	changes := bead.Changes
	if len(changes) == 0 {
		return false
	}
	if v, ok := changes["approved_by"].(string); ok && strings.TrimSpace(v) != "" {
		return true
	}
	if s, ok := changes["status"].(string); ok && strings.EqualFold(strings.TrimSpace(s), "in_progress") {
		return true
	}
	return false
}

// handleClosed nudges the requesting agent when an escalation completes.
func (e *Escalations) handleClosed(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Type != "escalation" {
		return
	}

	e.logger.Info("escalation bead closed",
		"id", bead.ID,
		"result_status", bead.Fields["result_status"])

	// Backfill fields from the full bead if the SSE event is sparse.
	if bead.Fields["result_status"] == "" || bead.Fields["requesting_agent"] == "" {
		if detail, err := e.daemon.GetBead(ctx, bead.ID); err == nil {
			if bead.Fields == nil {
				bead.Fields = make(map[string]string)
			}
			for _, key := range []string{"result_status", "result", "requesting_agent", "requesting_task", "operation_type", "approved_by", "approved_by_email"} {
				if v := detail.Fields[key]; v != "" && bead.Fields[key] == "" {
					bead.Fields[key] = v
				}
			}
		}
	}

	// Nudge the requesting agent that the escalation is complete.
	e.nudgeRequestingAgent(ctx, *bead)

	// Notify Slack. A completed execution (success/partial, however it got
	// approved — e.g. closed directly by the merge desk with no approved_by)
	// always posts a result summary via NotifyEscalationResult. Only an
	// escalation closed without ever being approved AND without a completed
	// result is a rejection — emit a "Rejected" banner via DismissEscalation.
	if e.escalationNotifier != nil {
		resultStatus := bead.Fields["result_status"]
		if bead.Fields["approved_by"] != "" || resultStatus == "success" || resultStatus == "partial" {
			if err := e.escalationNotifier.NotifyEscalationResult(ctx, *bead); err != nil {
				e.logger.Error("failed to notify escalation result", "id", bead.ID, "error", err)
			}
		} else {
			if err := e.escalationNotifier.DismissEscalation(ctx, bead.ID); err != nil {
				e.logger.Error("failed to dismiss rejected escalation", "id", bead.ID, "error", err)
			}
		}
	}

	// Tell the addressed operator when somebody ELSE approved or closed their
	// escalation (ka-0000002131). Added rather than folded into
	// NotifyEscalationResult above because that path EDITS the existing card
	// (chat.update) and Slack delivers no notification for an edit, so a
	// mention added there would reach nobody. At most one DM per bead.
	e.crossOperator.EscalationResolved(ctx, *bead)

	// Clean up tracking state.
	e.triggeredMu.Lock()
	delete(e.triggered, bead.ID)
	e.triggeredMu.Unlock()
	e.pendingMu.Lock()
	delete(e.pending, bead.ID)
	e.pendingMu.Unlock()
	e.pingbackMu.Lock()
	delete(e.pingbackSeen, bead.ID)
	delete(e.queueSeen, bead.ID)
	e.pingbackMu.Unlock()
}

// maybeNotifyPingback re-posts a stalled escalation to Slack when a pingback
// has been requested (pingback_requested_at newer than pingback_notified_at).
// The requested/notified field pair is a restart-safe handshake: the loop or
// CLI stamps requested_at, this method posts and stamps notified_at, and a
// redelivered SSE event with requested == notified is a no-op (ka-0000004385).
func (e *Escalations) maybeNotifyPingback(ctx context.Context, bead BeadEvent) {
	requested := bead.Fields["pingback_requested_at"]
	if requested == "" || requested == bead.Fields["pingback_notified_at"] {
		return
	}
	if bead.Status == "closed" {
		return
	}

	// In-memory guard: SSE events arriving between the Slack post and the
	// notified_at stamp landing must not double-post.
	e.pingbackMu.Lock()
	if e.pingbackSeen[bead.ID] == requested {
		e.pingbackMu.Unlock()
		return
	}
	e.pingbackSeen[bead.ID] = requested
	e.pingbackMu.Unlock()

	if e.escalationNotifier == nil {
		return
	}
	if err := e.escalationNotifier.NotifyEscalationPingback(ctx, bead); err != nil {
		e.logger.Error("failed to post escalation pingback", "id", bead.ID, "error", err)
		// Clear the in-memory mark so a later event can retry.
		e.pingbackMu.Lock()
		delete(e.pingbackSeen, bead.ID)
		e.pingbackMu.Unlock()
		return
	}

	e.logger.Info("posted escalation pingback",
		"id", bead.ID,
		"source", bead.Fields["pingback_source"],
		"count", bead.Fields["pingback_count"])

	if err := e.daemon.UpdateBeadFields(ctx, bead.ID, map[string]string{
		"pingback_notified_at": requested,
	}); err != nil {
		// Non-fatal: the in-memory mark still prevents a double-post this
		// process lifetime; a bridge restart may re-post once.
		e.logger.Warn("failed to stamp pingback_notified_at", "id", bead.ID, "error", err)
	}
}

// maybeNotifyHolderOffline posts once per escalation that its presence lane
// holds it while offline (gd-t41kYFbLUt). The daemon stamps
// held_by_offline_at once; held_by_offline_notified_at is the restart-safe
// handshake, as for pingbacks.
func (e *Escalations) maybeNotifyHolderOffline(ctx context.Context, bead BeadEvent) {
	at := bead.Fields["held_by_offline_at"]
	if at == "" || at == bead.Fields["held_by_offline_notified_at"] || bead.Status == "closed" || e.escalationNotifier == nil {
		return
	}
	e.pingbackMu.Lock()
	if e.offlineSeen[bead.ID] == at {
		e.pingbackMu.Unlock()
		return
	}
	e.offlineSeen[bead.ID] = at
	e.pingbackMu.Unlock()
	if err := e.escalationNotifier.NotifyEscalationHolderOffline(ctx, bead); err != nil {
		e.logger.Error("failed to post escalation holder offline notice", "id", bead.ID, "error", err)
		e.pingbackMu.Lock()
		delete(e.offlineSeen, bead.ID)
		e.pingbackMu.Unlock()
		return
	}
	if err := e.daemon.UpdateBeadFields(ctx, bead.ID, map[string]string{"held_by_offline_notified_at": at}); err != nil {
		e.logger.Warn("failed to stamp held_by_offline_notified_at", "id", bead.ID, "error", err)
	}
}

// maybeNotifyQueueAlert posts a stale-escalation alert to the operator queue
// channel when one has been requested (queue_alert_requested_at newer than
// queue_alert_notified_at). Same handshake shape as maybeNotifyPingback.
func (e *Escalations) maybeNotifyQueueAlert(ctx context.Context, bead BeadEvent) {
	requested := bead.Fields["queue_alert_requested_at"]
	if requested == "" || requested == bead.Fields["queue_alert_notified_at"] {
		return
	}
	if bead.Status == "closed" {
		return
	}

	e.pingbackMu.Lock()
	if e.queueSeen[bead.ID] == requested {
		e.pingbackMu.Unlock()
		return
	}
	e.queueSeen[bead.ID] = requested
	e.pingbackMu.Unlock()

	if e.escalationNotifier == nil {
		return
	}
	if err := e.escalationNotifier.NotifyEscalationQueueAlert(ctx, bead); err != nil {
		e.logger.Error("failed to post escalation queue alert", "id", bead.ID, "error", err)
		e.pingbackMu.Lock()
		delete(e.queueSeen, bead.ID)
		e.pingbackMu.Unlock()
		return
	}

	e.logger.Info("posted escalation queue alert", "id", bead.ID)

	if err := e.daemon.UpdateBeadFields(ctx, bead.ID, map[string]string{
		"queue_alert_notified_at": requested,
	}); err != nil {
		e.logger.Warn("failed to stamp queue_alert_notified_at", "id", bead.ID, "error", err)
	}
}

// EscalationCloseCommand is the command an executing agent runs to report an
// escalation's outcome. It writes result_status and closes the escalation in
// one call, which releases close-on-execute wrappers on success. The local
// escalation executor (gb agent start --runner) names it from here so it
// cannot drift to a verb gb lacks (gd-aTfaK8HgTV).
func EscalationCloseCommand(escalationID string) string {
	return EscalationReportCommand(escalationID, "<success|failure>", "<json>")
}

// EscalationReportCommand is EscalationCloseCommand with a concrete status and
// result, for prompts that show the executor a worked success and failure
// line. Executors must never `kd close` an escalation: the daemon refuses a
// generic close of an open escalation (escalationBlocksGenericClose), so the
// escalation would stay open and close-on-execute wrappers never release
// (gd-1MT3ZL6vqN).
func EscalationReportCommand(escalationID, status, result string) string {
	return fmt.Sprintf("gb escalation close %s --status %s --result '%s'", escalationID, status, result)
}

// nudgeRequestingAgent notifies the agent that originally requested the
// escalation that results are available.
func (e *Escalations) nudgeRequestingAgent(ctx context.Context, bead BeadEvent) {
	// Try the requesting_agent field first, then fall back to pending map.
	agentBeadID := bead.Fields["requesting_agent"]
	if agentBeadID == "" {
		e.pendingMu.Lock()
		agentBeadID = e.pending[bead.ID]
		e.pendingMu.Unlock()
	}
	if agentBeadID == "" {
		e.logger.Debug("no requesting agent to nudge", "escalation", bead.ID)
		return
	}

	// Resolve the agent bead to get the agent name.
	agentBead, err := e.daemon.GetBead(ctx, agentBeadID)
	if err != nil {
		e.logger.Warn("could not resolve requesting agent bead",
			"escalation", bead.ID, "agent_bead", agentBeadID, "error", err)
		return
	}

	agentName := agentBead.Fields["agent"]
	if agentName == "" {
		agentName = agentBead.Assignee
	}
	if agentName == "" {
		e.logger.Debug("requesting agent bead has no agent name",
			"escalation", bead.ID, "agent_bead", agentBeadID)
		return
	}

	resultStatus := bead.Fields["result_status"]
	opType := bead.Fields["operation_type"]
	message := fmt.Sprintf("Escalation %s completed: %s operation %s",
		bead.ID, opType, resultStatus)

	sensitivity := bead.Fields["sensitivity"]
	if sensitivity == "sensitive" || sensitivity == "secret" {
		// For sensitive/secret escalations, don't include raw results in the
		// nudge. Point the agent to the report bead instead.
		if redacted := bead.Fields["redacted_summary"]; redacted != "" {
			message += fmt.Sprintf("\nSummary: %s", redacted)
		}
		message += fmt.Sprintf("\nSensitivity: %s — full results available via authenticated web UI only.", sensitivity)
	} else if result := bead.Fields["result"]; result != "" {
		message += fmt.Sprintf("\nResult: %s", truncateResult(result, 500))
	}

	if err := e.nudge(ctx, agentName, message); err != nil {
		e.logger.Warn("failed to nudge requesting agent with escalation result",
			"agent", agentName, "escalation", bead.ID, "error", err)

		// gd-t0gm8q62Ac: a nudge failure here almost always means the requesting
		// crew has already gone terminal (its pod is gone — NudgeAgent's own
		// FindAgentBead/agent_state checks are exactly what just failed above).
		// Without a fallback, a resolved success|partial verdict — the mechanical
		// "poll CI, merge, close task" follow-through a reviewer's conditional
		// verdict demands — reaches nobody: the task sits in_progress with a dead
		// assignee looking like healthy in-flight work. Scoped to success|partial
		// per the coordinator's interim rule; a failure result has no follow-
		// through to resume (the requester, if alive, would just re-escalate).
		if resultStatus == "success" || resultStatus == "partial" {
			e.respawnRequestingAgent(ctx, agentName, bead, agentBead, message)
		}
	} else {
		e.logger.Info("nudged requesting agent with escalation result",
			"agent", agentName, "escalation", bead.ID, "result_status", resultStatus)
	}
}

// nudgeRequesterToExecute tells the requesting agent its escalation is approved
// and it should do the work itself (execute_by=requester, e.g. a land's merge
// approved by its owner). If the agent cannot be nudged (its pod is gone), the
// usual follow-through respawn carries the same message.
func (e *Escalations) nudgeRequesterToExecute(ctx context.Context, bead BeadEvent) {
	agentBeadID := strings.TrimSpace(bead.Fields["requesting_agent"])
	if agentBeadID == "" {
		e.logger.Warn("execute_by=requester but no requesting_agent; nothing executed", "id", bead.ID)
		return
	}
	agentBead, err := e.daemon.GetBead(ctx, agentBeadID)
	if err != nil {
		e.logger.Warn("could not resolve requesting agent bead", "escalation", bead.ID, "agent_bead", agentBeadID, "error", err)
		return
	}
	agentName := agentBead.Fields["agent"]
	if agentName == "" {
		agentName = agentBead.Assignee
	}
	if agentName == "" {
		e.logger.Warn("requesting agent bead has no agent name", "escalation", bead.ID, "agent_bead", agentBeadID)
		return
	}
	message := fmt.Sprintf("Escalation %s (%s) is approved. Do the operation yourself now, then record the outcome with `gb escalation close %s`.",
		bead.ID, bead.Fields["operation_type"], bead.ID)
	if err := e.nudge(ctx, agentName, message); err != nil {
		e.logger.Warn("could not nudge requester to execute; respawning", "escalation", bead.ID, "agent", agentName, "error", err)
		e.respawnRequestingAgent(ctx, agentName, bead, agentBead, message)
		return
	}
	e.logger.Info("nudged requester to execute its approved escalation", "escalation", bead.ID, "agent", agentName)
}

// respawnRequestingAgent wakes or dispatches a follow-through crew on the
// requesting task when the escalation's requesting agent could not be nudged
// (its pod is gone). Mirrors Decisions.respawnAgentForDecision: prefer an
// in-place event-wake for a dormant PARKED persistent identity, else mint a
// fresh crew via SpawnAgent onto the same requesting_task, carrying the
// verdict as its prompt. Best-effort — logs and returns on any failure rather
// than surfacing an error, since the caller (an SSE handler) has nothing to
// retry against.
func (e *Escalations) respawnRequestingAgent(ctx context.Context, agentName string, bead BeadEvent, agentBead *beadsapi.BeadDetail, resultMessage string) {
	if e.daemon == nil {
		return
	}

	// Never respawn a real pod for an E2E test fixture (mirrors
	// isE2EDecisionRespawn's belt-and-suspenders guard in decisions.go).
	if isE2EEscalation(bead.CreatedBy, bead.Labels) || projectFromLabels(bead.Labels) == e2eTestProject {
		e.logger.Info("skipping follow-through respawn — e2e fixture escalation",
			"escalation", bead.ID, "created_by", bead.CreatedBy)
		return
	}

	// Dedup: don't respawn twice for the same escalation within the cooldown
	// window (a burst of redelivered SSE close events, or handleUpdated's
	// statusChangedToClosed path racing handleClosed).
	e.spawnMu.Lock()
	now := time.Now()
	for id, t := range e.recentSpawn {
		if now.Sub(t) > escalationSpawnCooldown {
			delete(e.recentSpawn, id)
		}
	}
	if t, ok := e.recentSpawn[bead.ID]; ok && now.Sub(t) < escalationSpawnCooldown {
		e.spawnMu.Unlock()
		e.logger.Info("skipping follow-through respawn — already spawned recently for this escalation",
			"escalation", bead.ID)
		return
	}
	e.recentSpawn[bead.ID] = now
	e.spawnMu.Unlock()

	// A laptop agent is never respawned as a pod: the result waits for it as
	// mail (spec §7, gd-XXYOPF1sCs). After the dedup, so a burst of
	// redelivered close events writes one mail.
	if reviveLocalAgentByMail(ctx, e.daemon, e.logger, agentBead,
		fmt.Sprintf("Escalation %s finished while you were not running.", bead.ID), resultMessage) {
		return
	}

	requestingTask := strings.TrimSpace(bead.Fields["requesting_task"])
	project := ""
	role := "crew"
	taskID := requestingTask
	if agentBead != nil {
		project = agentBead.Fields["project"]
		role = agentBead.Fields["role"]
		if role == "" {
			role = "crew"
		}
		if taskID == "" {
			taskID = agentBead.Fields["task_id"]
		}
	}
	if project == "" {
		project = projectFromLabels(bead.Labels)
	}
	if project == e2eTestProject {
		e.logger.Info("skipping follow-through respawn — resolved project is the e2e-test fixture project",
			"escalation", bead.ID, "project", project)
		return
	}
	if taskID == "" {
		// Nothing to hand the follow-through crew — without a task there is no
		// work item to resume onto, only a dead agent name.
		e.logger.Warn("cannot dispatch follow-through crew — no requesting_task on escalation",
			"escalation", bead.ID, "agent", agentName)
		return
	}
	agentName = extractAgentName(agentName)

	// Follow-through gate (gd-nlR2YIDhB7): a resume crew is noise when the task
	// is already done, and a second agent on one seat when the task is already
	// staffed. Runs before the event-wake too: a live incumbent outranks
	// reviving a dormant identity onto the same work.
	if e.followThroughHandled(ctx, bead, taskID, agentName, agentBead, resultMessage) {
		return
	}

	// The shared wake (internal/agentwake, gd-T7aFA3tFoW): revive a dormant
	// PARKED persistent identity in place under its own name and transcript,
	// thaw a frozen one, else mint a fresh crew onto the requesting task.
	res := agentwake.Wake(ctx, e.daemon, e.logger, agentwake.Request{
		AgentName: agentName,
		AgentBead: agentBead,
		Reason:    beadsapi.WakeReasonEscalation,
		Operator:  humanOperatorFromAgentBead(agentBead),
		Project:   project,
		TaskID:    taskID,
		Role:      role,
		ExtraFields: resumeExtraFields(agentBead, map[string]string{
			"spawn_source":       "escalation-resume",
			"escalation_bead_id": bead.ID,
		}),
		// The resumed task keeps posting in its thread (gd-Q553TCVjN4).
		CarrySlackRouting: true,
		Prepare: func(context.Context) (string, bool) {
			return fmt.Sprintf(
				"You have been dispatched as a follow-through crew because escalation %s resolved "+
					"(result_status=%s) while the requesting agent was no longer running.\n\n%s\n\n"+
					"Your task is %s — run `kd show %s` for full context, then carry out the "+
					"mechanical follow-through the verdict calls for (e.g. poll CI, merge, close the "+
					"task) rather than re-filing the same escalation.",
				bead.ID, bead.Fields["result_status"], resultMessage, taskID, taskID), true
		},
		LogAttrs: []any{"escalation", bead.ID},
	})
	if res.Outcome == agentwake.Spawned {
		e.logger.Info("dispatched follow-through crew for resolved escalation",
			"agent", agentName, "escalation", bead.ID, "task", taskID, "new_agent_bead", res.AgentBeadID)
	}
}

// escalationBridgeActor authors the follow-through gate's notes on an
// escalation; it matches the spawned_by the executor spawn path stamps.
const escalationBridgeActor = "escalation-bridge"

// followThroughHandled reports whether the follow-through for a resolved
// escalation is already taken care of, so respawnRequestingAgent must not
// spawn a crew (gd-nlR2YIDhB7). Two cases, each noted on the escalation:
//   - The requesting task is closed: the work finished (often the executor
//     closed it via close-on-execute), so there is nothing to follow through.
//   - A live agent already holds the task (its assignee, an agent bead
//     assigned to it, or the requester's seat): that agent is nudged with
//     the verdict instead of a second crew being spawned beside it
//     (gd-ku3EREJlf1: a plan gardener re-dispatched the seat, then the
//     resume spawned another 3 min later).
//
// Fails open: if the task cannot be read, or the incumbent cannot be nudged,
// it returns false and the usual spawn proceeds.
func (e *Escalations) followThroughHandled(ctx context.Context, bead BeadEvent, taskID, requester string, agentBead *beadsapi.BeadDetail, resultMessage string) bool {
	task, err := e.daemon.GetBead(ctx, taskID)
	if err != nil || task == nil {
		e.logger.Warn("follow-through gate could not read requesting task; spawning as before",
			"escalation", bead.ID, "task", taskID, "error", err)
		return false
	}
	if task.Status == "closed" {
		e.logger.Info("skipping follow-through respawn — requesting task is already closed",
			"escalation", bead.ID, "task", taskID)
		e.noteFollowThrough(ctx, bead.ID, fmt.Sprintf(
			"Follow-through: no crew spawned. Requesting task %s is already closed.", taskID))
		return true
	}

	seat := ""
	if agentBead != nil {
		seat = agentBead.Fields["seat_key"]
		if seat == "" {
			seat = beadsapi.SeatKey(requester, agentBead.Fields["project"], agentBead.Fields["role"])
		}
	}
	incumbent := e.liveAgentOnTask(ctx, task, seat, requester)
	if incumbent == "" {
		return false
	}
	message := fmt.Sprintf(
		"Escalation %s resolved for task %s, which you hold; its requesting agent %s is gone, "+
			"so the follow-through is yours (no second crew was spawned).\n\n%s",
		bead.ID, taskID, requester, resultMessage)
	if err := e.nudge(ctx, incumbent, message); err != nil {
		e.logger.Warn("could not nudge live agent on requesting task; spawning follow-through crew",
			"escalation", bead.ID, "task", taskID, "agent", incumbent, "error", err)
		return false
	}
	e.logger.Info("nudged live agent on requesting task instead of spawning a follow-through crew",
		"escalation", bead.ID, "task", taskID, "agent", incumbent)
	e.noteFollowThrough(ctx, bead.ID, fmt.Sprintf(
		"Follow-through: nudged live agent %s, already on task %s, instead of spawning a second crew.",
		incumbent, taskID))
	return true
}

// liveAgentOnTask returns the name of a live agent already working task, or ""
// when none is. Candidates, in order: the task's assignee, any agent bead whose
// task_id is the task, and any agent bead on the requester's seat. Liveness is
// SpawnAgent's single-claim test (an active bead whose fields pass
// beadsapi.AgentFieldsLive). The requester itself is skipped: its nudge has
// just failed, which is why the caller is here.
func (e *Escalations) liveAgentOnTask(ctx context.Context, task *beadsapi.BeadDetail, seat, requester string) string {
	now := time.Now()
	live := func(b *beadsapi.BeadDetail) bool {
		return b != nil && b.Status != "closed" && b.Fields["agent"] != "" &&
			b.Fields["agent"] != requester && beadsapi.AgentFieldsLive(b.Fields, now)
	}
	if a := strings.TrimSpace(task.Assignee); a != "" && a != requester {
		if b, err := e.daemon.FindAgentBead(ctx, a); err == nil && live(b) {
			return b.Fields["agent"]
		}
	}
	lookups := []struct{ field, value string }{{"task_id", task.ID}, {"seat_key", seat}}
	for _, l := range lookups {
		if l.value == "" {
			continue
		}
		res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"agent"},
			Statuses: beadsapi.ActiveStatuses(),
			Fields:   map[string]string{l.field: l.value},
		})
		if err != nil || res == nil {
			e.logger.Warn("follow-through gate agent lookup failed", "task", task.ID, l.field, l.value, "error", err)
			continue
		}
		for _, b := range res.Beads {
			if b != nil && b.Fields[l.field] == l.value && live(b) {
				return b.Fields["agent"]
			}
		}
	}
	return ""
}

// noteFollowThrough records the follow-through gate's choice on the
// escalation, so its thread says why no crew (or no new crew) appeared.
func (e *Escalations) noteFollowThrough(ctx context.Context, escalationID, text string) {
	if err := e.daemon.AddComment(ctx, escalationID, escalationBridgeActor, text); err != nil {
		e.logger.Warn("failed to note follow-through decision on escalation",
			"escalation", escalationID, "error", err)
	}
}

// truncateResult truncates a result string to maxLen, adding an ellipsis.
func truncateResult(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
