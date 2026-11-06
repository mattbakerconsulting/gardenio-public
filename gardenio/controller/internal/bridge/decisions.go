// Package bridge provides the decision lifecycle watcher.
//
// Decisions subscribes to kbeads SSE event stream for bead create/close events,
// filters for type=decision beads, and:
//   - On create: notifies an optional Notifier (e.g., Slack).
//   - On close: reads the agent field and mails the agent a nudge
//     (NudgeAgent) so the idle agent wakes up and reads the result.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/agentwake"
	"gardenio/controller/internal/beadsapi"
)

// BeadClient is the subset of beadsapi.Client used by the bridge package.
type BeadClient interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	FindAgentBeadAnyStatus(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
	// CaptureEscalationResult records an execution result on an escalation and
	// closes it through the dedicated result verb (POST /v1/escalations/{id}/result).
	// Escalation termination MUST route here, not through CloseBead: the daemon's
	// escalationBlocksGenericClose guard rejects a generic close of an open
	// escalation (409/FailedPrecondition), so a CloseBead-based termination of a
	// failure result silently never persists and never closes (ka-0000006558).
	CaptureEscalationResult(ctx context.Context, escalationID string, req beadsapi.CaptureEscalationResultRequest) error
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	// RevokeRunner revokes a laptop runner's tokens and tells its agents to
	// stop (POST /v1/runners/{id}/revoke); /runner revoke.
	RevokeRunner(ctx context.Context, runnerID string) (revokedTokens, agentsKilled int, err error)
	// UpdateBead applies a status change and a partial fields patch in ONE
	// atomic daemon write (used by respawnThreadAgent to REVIVE the prior thread
	// agent bead in place — keeping KD_AGENT_ID stable so coop's S3 resume key
	// resolves the prior transcript instead of cold-starting under a fresh id;
	// ka-0000006807, the thread-path twin of the wakePlanAssistant fix).
	UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error
	SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error)
	ListDecisionBeads(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	ListAgentBeads(ctx context.Context) ([]beadsapi.AgentBead, error)
	ListAccountBeads(ctx context.Context) (map[string]beadsapi.AccountInfo, error)
	ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error)
	ListAssignedTask(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	// AddLabel appends a label to a bead (idempotent server-side: a no-op when
	// the label is already present). Used by handleClosed to stamp
	// gardener-retired-dead-wood on a dismissed revival decision's subject.
	AddLabel(ctx context.Context, beadID, label string) error
	// GetMentionedBy returns the beads whose text references beadID (reverse
	// mention lookup). Used by the C4 auto-close reactor to find a retired
	// gardener's remediation/tune-tasks, which reference their subject gardener
	// only by free-text id mention (no machine edge exists — see ka-0000004166).
	GetMentionedBy(ctx context.Context, beadID string, limit int) ([]beadsapi.BeadReferenceSummary, error)
	// GetReverseDependencies lists beads that depend on beadID. Used by the C4
	// auto-close reactor to skip any candidate that is a PARENT of open work,
	// so auto-close never orphans children (advice ka-0000000396).
	GetReverseDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	ResolveTicket(ctx context.Context, ticketKey string) (*beadsapi.BeadDetail, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	AddComment(ctx context.Context, beadID, author, text string) error
	GetComments(ctx context.Context, beadID string) ([]beadsapi.Comment, error)
	ListSlackOperatorAliases(ctx context.Context) (*beadsapi.ListSlackOperatorAliasesResponse, error)
	ResyncOperatorIdentity(ctx context.Context, operatorID int64) (*beadsapi.UpdateOperatorIdentityResponse, error)
	ResyncUserBeadAliases(ctx context.Context, beadID string) (*beadsapi.LinkOperatorAliasesResponse, error)
}

// BeadEvent is the JSON payload published on beads.bead.created / beads.bead.closed.
type BeadEvent struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Title     string            `json:"title"`
	Status    string            `json:"status"`
	Assignee  string            `json:"assignee"`
	CreatedBy string            `json:"created_by"`
	Labels    []string          `json:"labels"`
	Fields    map[string]string `json:"fields"`
	Priority  int               `json:"priority"`
	ClosedBy  string            `json:"closed_by,omitempty"` // present on close events
	Changes   map[string]any    `json:"changes,omitempty"`   // present on update events
	// OperatedBy is the bead's top-level operated_by: the human operator the
	// daemon attributes the bead to (an "oidc:<email>" or "slack:<id>" alias),
	// as distinct from any operated_by key in Fields. Empty when the daemon
	// attributed it to no one (a fleet or system write).
	OperatedBy string `json:"operated_by,omitempty"`
	// Actor is the server-resolved subject that performed the mutation
	// (agent name for X-Actor static-token writes, email for OIDC).
	// Present on update events; empty when auth is disabled or the caller
	// is anonymous.
	Actor string `json:"actor,omitempty"`
	// DecisionAnswer carries the operator's response when a type=decision bead
	// is resolved or cancelled. nil for non-decision closes. trigger:bead
	// watchers can match on Fields["chosen"], Fields["response_text"], etc.
	// (these mirrors are also injected into Fields by ParseBeadEvent so the
	// generic condition DSL Just Works).
	DecisionAnswer *DecisionAnswer `json:"decision_answer,omitempty"`
	// Heraldry is the bead's arms in the canonical stored form, "" for none
	// (heraldry spec §4.1); prose shows its emoji run (beadTitleLinkArms).
	Heraldry string `json:"heraldry,omitempty"`
}

// DecisionAnswer mirrors events.DecisionAnswer from the kbeads daemon —
// duplicated here to keep the bridge module decoupled from the daemon's
// internal events package.
type DecisionAnswer struct {
	Chosen       string `json:"chosen,omitempty"`
	ResponseText string `json:"response_text,omitempty"`
	RespondedBy  string `json:"responded_by,omitempty"`
	RespondedAt  string `json:"responded_at,omitempty"`
	Cancelled    bool   `json:"cancelled,omitempty"`
}

// DecisionRespawner can respawn a dead agent to act on a resolved decision.
// Implementations must handle deduplication to avoid double-spawns.
type DecisionRespawner interface {
	RespawnForDecision(ctx context.Context, agentName string, bead BeadEvent, decisionMessage string)
}

// Notifier sends decision lifecycle notifications to an external system.
type Notifier interface {
	// NotifyDecision is called when a new decision bead is created.
	NotifyDecision(ctx context.Context, bead BeadEvent) error
	// UpdateDecision is called when a decision bead is closed/resolved.
	UpdateDecision(ctx context.Context, beadID, chosen, rationale string) error
	// NotifyEscalation is called when a decision bead is escalated.
	NotifyEscalation(ctx context.Context, bead BeadEvent) error
	// DismissDecision is called when a decision bead expires (removes Slack message).
	DismissDecision(ctx context.Context, beadID string) error
	// PostReport posts a report as a thread reply on the linked decision's Slack message.
	PostReport(ctx context.Context, reportID, decisionID, reportType, content string) error
}

// escalationTTL is the maximum age of entries in the escalated dedup map.
const escalationTTL = 1 * time.Hour

// decisionSpawnCooldown is the minimum interval between spawns for the same
// decision bead. Prevents rapid respawn loops when the agent can't process the
// decision quickly enough.
const decisionSpawnCooldown = 10 * time.Minute

// shutdownKeywords are substrings that indicate a decision option means the
// agent should shut down. Matched case-insensitively against the "chosen" field.
var shutdownKeywords = []string{"shut down", "shutdown", "stand down"}

const (
	// gardenerRetiredDeadWoodLabel marks a gardener/trigger subject bead whose
	// revival-review decision was DISMISSED by an operator — i.e. the operator
	// declined to revive it. The downstream gardener-reviver loop reads this
	// label to distinguish "no live agent AND operator judged it dead" from "no
	// live agent, never reviewed" so it can archive rather than re-revive.
	gardenerRetiredDeadWoodLabel = "gardener-retired-dead-wood"

	// gardenerRetiredDeadWoodActor is the synthetic author stamped on the audit
	// comment the reactor posts on the subject at dismissal.
	gardenerRetiredDeadWoodActor = "bridge-decision-close-reactor"

	// gardenerRetiredDeadWoodSystemOperator is the X-System-Operator source label
	// for the reactor's writes (AddLabel + AddComment). This is a true
	// machine-origin reaction to a close event with no human behind THIS write
	// (the operator's judgement is the dismissal itself), so the daemon's
	// fail-closed audit-operator resolver would otherwise reject these writes.
	// Mirrors gardenerDisarmOnClose / closed-enabled-drain attribution.
	gardenerRetiredDeadWoodSystemOperator = "bridge-decision-close-reactor"

	// deadWoodTaskCloseReason is stamped as close_reason on a remediation/tune-
	// task auto-closed by the C4 reactor when its subject gardener is retired as
	// dead wood. The literal decision id is appended at close time.
	deadWoodTaskCloseReason = "obsolete: subject gardener retired as dead wood"

	// deadWoodAutoCloseMentionLimit bounds the reverse-mention fan-out the C4
	// reactor scans for a retired subject. A retired gardener is referenced by a
	// long tail of prose beads; only the handful that self-identify as its
	// remediation/tune-tasks are ever closed (see isGardenerRemediationTaskFor),
	// but we cap the candidate scan so a heavily-referenced subject cannot make
	// the reactor page the entire mention index.
	deadWoodAutoCloseMentionLimit = 200
)

// planAnswerNote tells an agent woken on a plan's "run it?" card to apply the
// answer with gb lands plan answer (gd-I9ktuYVV2Y). The card is the steward's
// plan decision (internal/steward planPrompt, "Plan <id>: <title> - run it?"),
// filed with the plan as its requesting_task, by gb lands plan write or by hand
// for a Nursery pot. Without the note a nursery backfill resume closed done on
// an approve and the plan stayed ready with no gardener: only the answer
// command moves it to building and starts its gardener. It is "" for any other
// decision, or when no land is known to name in the command.
func planAnswerNote(question, requestingTask, land, decisionID string) string {
	if land == "" || !strings.HasPrefix(question, "Plan "+requestingTask+": ") ||
		!strings.HasSuffix(strings.TrimSpace(question), " - run it?") {
		return ""
	}
	cmd := fmt.Sprintf("gb lands plan answer %s %s", land, decisionID)
	return fmt.Sprintf(
		"\n\nThis is the run-it card for plan %s. Before anything else, run `%s`: "+
			"approve moves the plan to building and starts its gardener, change and drop abandon it. "+
			"Do not apply the answer by hand, and do not close your task or stop without running it. "+
			"If it is refused (403), file `gb escalation create --operation-type beads_admin_write` "+
			"asking an admin to run exactly `%s`, and say so in your wrap-up.",
		requestingTask, cmd, cmd)
}

// isShutdownDecision returns true if the chosen option indicates the agent
// should be shut down. This prevents respawning a dead agent just to tell it
// to stop.
func isShutdownDecision(chosen string) bool {
	lower := strings.ToLower(chosen)
	for _, kw := range shutdownKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// Decisions watches the kbeads SSE event stream for decision bead lifecycle events.
type Decisions struct {
	daemon    BeadClient
	notifier  Notifier            // nil = no notifications
	streamMgr *AgentStreamManager // nil = no streaming
	logger    *slog.Logger

	escalatedMu sync.Mutex
	escalated   map[string]time.Time // bead ID → notification time (dedup with TTL)

	spawnMu     sync.Mutex
	recentSpawn map[string]time.Time // decision bead ID → last spawn time (dedup)

	// crossOperator posts the accountability DM when a decision is resolved
	// by someone other than the operator it was addressed to (ka-0000002131,
	// operator ruling on ka-0000004303). nil = disabled.
	crossOperator *CrossOperatorNotify
}

// DecisionsConfig holds configuration for the Decisions watcher.
type DecisionsConfig struct {
	Daemon        BeadClient
	Notifier      Notifier
	StreamManager *AgentStreamManager
	Logger        *slog.Logger
	// CrossOperator, when set, DMs the addressed operator whenever a decision
	// is resolved by a different identity (ka-0000002131).
	CrossOperator *CrossOperatorNotify
}

// NewDecisions creates a new decision lifecycle watcher.
func NewDecisions(cfg DecisionsConfig) *Decisions {
	return &Decisions{
		daemon:      cfg.Daemon,
		notifier:    cfg.Notifier,
		streamMgr:   cfg.StreamManager,
		logger:      cfg.Logger,
		escalated:   make(map[string]time.Time),
		recentSpawn: make(map[string]time.Time),

		crossOperator: cfg.CrossOperator,
	}
}

// SetStreamManager sets the stream manager for decision task_update streaming.
// Called after the stream manager is created (may be nil if streaming is disabled).
func (d *Decisions) SetStreamManager(mgr *AgentStreamManager) {
	d.streamMgr = mgr
}

// RegisterHandlers registers SSE event handlers on the given stream for
// decision bead created, closed, and updated events.
func (d *Decisions) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.created", d.handleCreated)
	stream.On("beads.bead.closed", d.handleClosed)
	stream.On("beads.bead.closed", d.handleReportClosed)
	stream.On("beads.bead.updated", d.handleUpdated)
	d.logger.Info("decisions watcher registered SSE handlers",
		"topics", []string{"beads.bead.created", "beads.bead.closed", "beads.bead.updated"})
}

func (d *Decisions) handleCreated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		d.logger.Debug("skipping malformed bead created event")
		return
	}
	if bead.Type != "decision" {
		return
	}

	// Skip E2E test decisions to prevent flooding production Slack channels.
	if hasLabel(bead.Labels, "e2e:true") {
		d.logger.Debug("skipping e2e decision", "id", bead.ID)
		return
	}

	d.logger.Info("decision bead created",
		"id", bead.ID,
		"title", bead.Title,
		"assignee", bead.Assignee)

	if d.notifier != nil {
		if err := d.notifier.NotifyDecision(ctx, *bead); err != nil {
			d.logger.Error("failed to notify decision", "id", bead.ID, "error", err)
		}
	}

	// Stream task_update chunk for the decision checkpoint.
	if d.streamMgr != nil {
		agentName := resolveDecisionAgent(bead)
		if agentName != "" {
			prompt := decisionQuestion(bead.Fields)
			d.streamMgr.HandleDecisionCreated(ctx, agentName, bead.ID, prompt)
		}
	}

	// Notify the gardener that spawned the requesting agent.
	d.notifySpawner(ctx, *bead)
}

// notifySpawner sends a mail bead to the gardener that spawned the requesting
// agent, so the gardener is notified immediately when its crew creates a decision.
// If the agent has no spawned_by field, or the spawner is no longer active, this
// is a no-op.
func (d *Decisions) notifySpawner(ctx context.Context, bead BeadEvent) {
	if d.daemon == nil {
		return
	}

	reqAgentBeadID := bead.Fields["requesting_agent_bead_id"]
	if reqAgentBeadID == "" {
		return
	}

	agentBead, err := d.daemon.GetBead(ctx, reqAgentBeadID)
	if err != nil {
		d.logger.Debug("notifySpawner: could not resolve requesting agent",
			"decision", bead.ID, "agent_bead", reqAgentBeadID, "error", err)
		return
	}

	spawner := agentBead.Fields["spawned_by"]
	if spawner == "" {
		return
	}

	// Verify the spawner agent is still active.
	spawnerBead, err := d.daemon.FindAgentBead(ctx, spawner)
	if err != nil || spawnerBead == nil {
		d.logger.Debug("notifySpawner: spawner agent not found, skipping",
			"spawner", spawner, "error", err)
		return
	}
	if state := spawnerBead.Fields["agent_state"]; state == "done" || state == "failed" {
		return
	}

	crewAgent := bead.Fields["requested_by"]
	if crewAgent == "" {
		crewAgent = bead.Assignee
	}
	question := decisionQuestion(bead.Fields)
	title := fmt.Sprintf("Crew decision from %s: %s", crewAgent, truncate(question, 80))

	mailFields := map[string]string{
		"decision_bead_id": bead.ID,
		"source_agent":     crewAgent,
	}
	fieldsJSON, _ := json.Marshal(mailFields)

	desc := fmt.Sprintf("Decision %s needs resolution.\n\nQuestion: %s",
		bead.ID, question)
	if opts := bead.Fields["options"]; opts != "" {
		desc += fmt.Sprintf("\n\nOptions: %s", opts)
	}

	_, err = d.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "mail",
		Assignee:    spawner,
		Labels:      []string{"from:" + crewAgent, "delivery:interrupt"},
		Description: desc,
		Fields:      json.RawMessage(fieldsJSON),
	})
	if err != nil {
		d.logger.Error("notifySpawner: failed to create gardener mail",
			"spawner", spawner, "decision", bead.ID, "error", err)
		return
	}

	d.logger.Info("notified spawner gardener of crew decision",
		"spawner", spawner, "crew", crewAgent, "decision", bead.ID)
}

func (d *Decisions) handleClosed(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		d.logger.Debug("skipping malformed bead closed event")
		return
	}
	if bead.Type != "decision" {
		return
	}

	chosen := bead.Fields["chosen"]

	// SSE close events may not include close-time fields (chosen, rationale, prompt).
	// Fetch the full bead when chosen is missing so nudgeAgent has complete data.
	if chosen == "" {
		if detail, err := d.daemon.GetBead(ctx, bead.ID); err == nil {
			if bead.Fields == nil {
				bead.Fields = make(map[string]string)
			}
			if v := detail.Fields["chosen"]; v != "" {
				bead.Fields["chosen"] = v
				chosen = v
			}
			// Free-text resolutions carry no `chosen` — the operator's answer
			// is in `response_text`. Backfill it so the nudge still conveys the
			// reply (ka-0000000003).
			if v := detail.Fields["response_text"]; v != "" && bead.Fields["response_text"] == "" {
				bead.Fields["response_text"] = v
			}
			if v := detail.Fields["rationale"]; v != "" {
				bead.Fields["rationale"] = v
			}
			// Backfill prompt/question so nudge message includes the decision context.
			if decisionQuestion(bead.Fields) == "" {
				if v := detail.Fields["prompt"]; v != "" {
					bead.Fields["prompt"] = v
				} else if v := detail.Fields["question"]; v != "" {
					bead.Fields["question"] = v
				}
			}
			if bead.Assignee == "" {
				bead.Assignee = detail.Assignee
			}
		}
	}

	d.logger.Info("decision bead closed",
		"id", bead.ID,
		"chosen", chosen,
		"assignee", bead.Assignee)

	// Detect expiry: system:timeout closures dismiss the Slack message.
	rationale := bead.Fields["rationale"]
	if chosen == "_expired" || chosen == "dismissed" {
		if d.notifier != nil {
			if err := d.notifier.DismissDecision(ctx, bead.ID); err != nil {
				d.logger.Error("failed to dismiss expired decision", "id", bead.ID, "error", err)
			}
		}
		d.logger.Info("decision expired/dismissed, Slack message removed",
			"id", bead.ID, "chosen", chosen, "rationale", rationale)
		// Gardener-revival reactor: when an operator DISMISSES a revival-review
		// decision that carries a machine-readable subject (subject_trigger_id,
		// stamped by C1's `gb decision create --subject`), stamp the subject
		// trigger bead so the downstream gardener-reviver loop knows the operator
		// judged it dead wood — not merely "no live agent, never reviewed". Gate
		// strictly on chosen == "dismissed": _expired shares this branch but is a
		// timeout, not an operator judgement, and must NOT stamp. Best-effort:
		// a stamp failure logs and never breaks the dismissal or the nudge below.
		if chosen == "dismissed" {
			d.stampGardenerRetiredDeadWood(ctx, *bead)
		}
		// Still nudge the agent even on expiry so it knows the gate is closed.
		d.nudgeAgent(ctx, *bead)
		return
	}

	// Notify external system (e.g., update Slack message).
	if d.notifier != nil {
		if err := d.notifier.UpdateDecision(ctx, bead.ID, chosen, rationale); err != nil {
			d.logger.Error("failed to update decision notification", "id", bead.ID, "error", err)
		}
	}

	// Tell the addressed operator when somebody ELSE answered for them
	// (ka-0000002131). This is an addition rather than an extension of
	// UpdateDecision above on purpose: UpdateDecision EDITS the existing card
	// via chat.update, and Slack does not deliver a notification for an edited
	// message — an @-mention added there would reach nobody. Delivery needs a
	// new message, so this posts a DM. Fires at most once per bead and never
	// when the resolver IS the addressed operator.
	d.crossOperator.DecisionResolved(ctx, *bead)

	// Stream task_update chunk marking decision as complete.
	if d.streamMgr != nil {
		agentName := resolveDecisionAgent(bead)
		if agentName != "" {
			d.streamMgr.HandleDecisionResolved(ctx, agentName, bead.ID, chosen)
		}
	}

	// Nudge the agent via coop so it wakes up and reads the decision result.
	d.nudgeAgent(ctx, *bead)
}

// stampGardenerRetiredDeadWood marks the subject trigger bead of a DISMISSED
// gardener-revival decision so the downstream gardener-reviver loop can tell that
// the operator judged the gardener dead wood (declined to revive it), rather than
// "no live agent, never reviewed". It adds the gardener-retired-dead-wood label
// and posts an audit comment naming the dismissing decision.
//
// This is the reactor half of the C1/C3 slice: C1 (`gb decision create
// --subject`) writes the machine-readable subject_trigger_id onto the revival
// decision; this reads it back at dismissal. It fires ONLY when subject_trigger_id
// is present, so decisions filed before C1 (or with no subject) are inert rather
// than mis-stamped.
//
// Discipline is modeled on gardenerDisarmOnClose / closed-enabled-drain:
//   - WithSystemOperator attribution — this is a true machine-origin reaction to
//     a close event, so the daemon's fail-closed audit-operator resolver would
//     otherwise reject the AddLabel/AddComment writes.
//   - Idempotent — AddLabel is a server-side no-op when the label is present.
//   - Best-effort — every failure logs a Warn and returns; it must never break
//     the caller's dismissal handling or the agent nudge.
func (d *Decisions) stampGardenerRetiredDeadWood(ctx context.Context, bead BeadEvent) {
	subject := strings.TrimSpace(bead.Fields["subject_trigger_id"])
	if subject == "" {
		// No machine-readable subject (pre-C1 decision, or not a revival review).
		// Inert by design — nothing to react to.
		return
	}
	if d.daemon == nil {
		return
	}

	// True machine-origin write: the operator's judgement IS the dismissal; this
	// reactive stamp has no separate human behind it. Attribute to the system
	// operator so the daemon does not fail-closed on operator resolution.
	ctx = beadsapi.WithSystemOperator(ctx, gardenerRetiredDeadWoodSystemOperator)

	d.logger.Info("gardener-revival reactor: dismissed decision has subject, stamping dead-wood",
		"decision", bead.ID, "subject", subject)

	if err := d.daemon.AddLabel(ctx, subject, gardenerRetiredDeadWoodLabel); err != nil {
		// Best-effort: log and continue. Do not fail the dismissal.
		d.logger.Warn("gardener-revival reactor: failed to stamp dead-wood label on subject",
			"decision", bead.ID, "subject", subject, "label", gardenerRetiredDeadWoodLabel, "error", err)
		return
	}

	audit := fmt.Sprintf(
		"%s: stamped `%s` — operator DISMISSED revival-review decision %s, so this gardener/trigger "+
			"is judged dead wood (declined for revival, no live agent). The gardener-reviver loop reads "+
			"this label to archive rather than re-revive. Reversible: remove the label to re-admit it to "+
			"the revival survey pool.",
		gardenerRetiredDeadWoodActor, gardenerRetiredDeadWoodLabel, bead.ID)
	if err := d.daemon.AddComment(ctx, subject, gardenerRetiredDeadWoodActor, audit); err != nil {
		// The label is the load-bearing signal; a missing audit comment is
		// cosmetic. Log and move on.
		d.logger.Warn("gardener-revival reactor: failed to post dead-wood audit comment on subject",
			"decision", bead.ID, "subject", subject, "error", err)
	}

	// C4 (ka-0000004166): now that the subject is marked dead wood, auto-close
	// its still-open remediation/tune-tasks obsolete so a delivery-drive gardener
	// does not re-dispatch them, blind to the dismissal. This is the automated
	// form of the manual C5 delivery.md dead-wood dispatch gate. Runs only after
	// the label stamp above succeeds — the subject IS retired at this point.
	d.autoCloseObsoleteRemediationTasks(ctx, subject, bead.ID)
}

// autoCloseObsoleteRemediationTasks finds the open gardener remediation/tune-tasks
// of a just-retired subject gardener and closes them obsolete, naming the
// dismissing decision. It is the reactor half of C4 (ka-0000004166): a dismissed
// revival leaves the subject's tune-task open+blocked on its epic, and a
// delivery-drive gardener scanning that epic would re-dispatch it — re-litigating
// a settled call. Closing the task at dismissal time removes that re-dispatch
// surface.
//
// TRAVERSAL. There is no machine edge from a subject gardener to its tune-task
// (ka-0000004166 / ka-0000006502): the tune-task references the gardener only by a
// free-text id mention in its title. We therefore walk the reverse-mention index
// (GetMentionedBy) and keep only candidates that self-identify as a remediation/
// tune-task FOR THIS subject — see isGardenerRemediationTaskFor. The predicate is
// deliberately narrow (id must be in the candidate's title AND the title must
// carry a tune/remediation marker) so an arbitrary bead that merely discusses the
// gardener in prose is never closed.
//
// SAFETY. Every close is guarded:
//   - only open/in_progress/blocked task|bug candidates,
//   - the subject id must appear in the candidate's own title,
//   - the title must carry a remediation/tune marker,
//   - the candidate must be a LEAF (no parent-child children) so auto-close never
//     orphans open children (advice ka-0000000396) — a non-leaf is left for a human.
//
// Best-effort throughout: a lookup or close failure logs a Warn and moves on; it
// must never break the caller's dismissal handling or the agent nudge. Attribution
// is already set to the system operator by the caller (stampGardenerRetiredDeadWood).
func (d *Decisions) autoCloseObsoleteRemediationTasks(ctx context.Context, subject, decisionID string) {
	if d.daemon == nil {
		return
	}

	refs, err := d.daemon.GetMentionedBy(ctx, subject, deadWoodAutoCloseMentionLimit)
	if err != nil {
		d.logger.Warn("gardener-revival reactor: failed to list mentions of retired subject; skipping auto-close",
			"decision", decisionID, "subject", subject, "error", err)
		return
	}

	for _, ref := range refs {
		if !isGardenerRemediationTaskFor(ref, subject) {
			continue
		}

		// Leaf guard: never close a bead that still parents open children — that
		// would leave them dangling (advice ka-0000000396). A tune-task is a leaf
		// by construction; if this one is not, leave it for a human.
		if deps, derr := d.daemon.GetReverseDependencies(ctx, ref.ID); derr != nil {
			d.logger.Warn("gardener-revival reactor: failed to check children of remediation task; skipping to be safe",
				"decision", decisionID, "subject", subject, "task", ref.ID, "error", derr)
			continue
		} else if hasParentChildChild(deps, ref.ID) {
			d.logger.Warn("gardener-revival reactor: remediation task has children; leaving for manual close (no orphaning)",
				"decision", decisionID, "subject", subject, "task", ref.ID)
			continue
		}

		audit := fmt.Sprintf(
			"%s: closing obsolete — subject gardener %s was retired as dead wood (operator DISMISSED "+
				"revival-review decision %s, which stamped `%s` on the subject). Re-dispatching this "+
				"remediation/tune-task would re-tune a gardener the operator retired. Reversible: a human "+
				"removing the label re-admits the gardener, after which this task can be reopened.",
			gardenerRetiredDeadWoodActor, subject, decisionID, gardenerRetiredDeadWoodLabel)
		if err := d.daemon.AddComment(ctx, ref.ID, gardenerRetiredDeadWoodActor, audit); err != nil {
			// Cosmetic — the close below is the load-bearing action. Log and continue.
			d.logger.Warn("gardener-revival reactor: failed to post obsolete audit comment on remediation task",
				"decision", decisionID, "subject", subject, "task", ref.ID, "error", err)
		}

		closeReason := fmt.Sprintf("%s (dismissing decision %s)", deadWoodTaskCloseReason, decisionID)
		if err := d.daemon.CloseBead(ctx, ref.ID, map[string]string{"close_reason": closeReason}); err != nil {
			d.logger.Warn("gardener-revival reactor: failed to auto-close obsolete remediation task",
				"decision", decisionID, "subject", subject, "task", ref.ID, "error", err)
			continue
		}
		d.logger.Info("gardener-revival reactor: auto-closed obsolete remediation task",
			"decision", decisionID, "subject", subject, "task", ref.ID)
	}
}

// isGardenerRemediationTaskFor reports whether ref is an OPEN gardener remediation/
// tune-task whose subject is the retired gardener subjectID. It mirrors the C5
// delivery.md dead-wood dispatch gate's definition — "a bead whose job is to fix,
// re-config, or re-enable a specific gardener (title like `Tune: … gardener (gd-…)`,
// … naming a subject gardener/trigger id)".
//
// Both conditions must hold, keeping the match narrow enough to auto-close:
//   - the candidate is an open/in_progress/blocked task or bug (never an epic,
//     doc, agent, the decision itself, or already-closed work), and
//   - the subject id appears in the candidate's OWN title AND that title carries a
//     tune/remediation marker — so the candidate is a remediation task FOR THIS
//     subject, not merely a bead that mentions it in prose or a tune-task for a
//     DIFFERENT gardener that happens to reference this one.
func isGardenerRemediationTaskFor(ref beadsapi.BeadReferenceSummary, subjectID string) bool {
	switch ref.Type {
	case "task", "bug":
	default:
		return false
	}
	switch ref.Status {
	case "open", "in_progress", "blocked":
	default:
		return false
	}
	if !strings.Contains(ref.Title, subjectID) {
		return false
	}
	t := strings.ToLower(ref.Title)
	return strings.HasPrefix(t, "tune:") ||
		strings.Contains(t, "fix broken gardener") ||
		strings.Contains(t, "remediat")
}

// hasParentChildChild reports whether parentID parents any bead via a
// parent-child edge, per a reverse-dependency listing. GetReverseDependencies
// returns rows reading "BeadID depends on DependsOnID"; a parent-child row where
// DependsOnID == parentID means BeadID is a CHILD of parentID.
func hasParentChildChild(deps []beadsapi.Dependency, parentID string) bool {
	for _, dep := range deps {
		if dep.Type == "parent-child" && dep.DependsOnID == parentID {
			return true
		}
	}
	return false
}

func (d *Decisions) handleUpdated(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}
	if bead.Type != "decision" {
		return
	}

	// Detect escalation: decision updated with "escalated" label.
	isEscalated := false
	for _, label := range bead.Labels {
		if label == "escalated" {
			isEscalated = true
			break
		}
	}
	if !isEscalated {
		return
	}

	// Deduplicate escalation notifications (with periodic TTL cleanup).
	d.escalatedMu.Lock()
	d.cleanupEscalatedLocked()
	if _, seen := d.escalated[bead.ID]; seen {
		d.escalatedMu.Unlock()
		return
	}
	d.escalated[bead.ID] = time.Now()
	d.escalatedMu.Unlock()

	d.logger.Info("decision escalated",
		"id", bead.ID,
		"title", bead.Title,
		"priority", bead.Priority,
		"assignee", bead.Assignee)

	if d.notifier != nil {
		if err := d.notifier.NotifyEscalation(ctx, *bead); err != nil {
			d.logger.Error("failed to notify escalation", "id", bead.ID, "error", err)
		}
	}
}

// cleanupEscalatedLocked removes entries older than escalationTTL.
// Caller must hold d.escalatedMu.
func (d *Decisions) cleanupEscalatedLocked() {
	now := time.Now()
	for id, t := range d.escalated {
		if now.Sub(t) > escalationTTL {
			delete(d.escalated, id)
		}
	}
}

// nudgeAgent resolves the target agent and delivers a decision nudge with retry.
// Resolves agent from Assignee first, falling back to CreatedBy.
// If the agent is dead (done/failed/stopping), it spawns a new agent to act
// on the decision response.
func (d *Decisions) nudgeAgent(ctx context.Context, bead BeadEvent) {
	agentName := bead.Assignee
	if agentName == "" {
		agentName = bead.CreatedBy
	}
	if agentName == "" {
		d.logger.Warn("decision bead has no assignee or created_by, cannot nudge", "id", bead.ID)
		return
	}
	if isMachineFiledDecision(agentName) {
		d.logger.Info("decision was filed by a machine principal, not an agent: nothing to nudge or respawn",
			"decision", bead.ID, "filer", agentName)
		return
	}
	if isHerderWishDecision(ctx, d.daemon, &bead) {
		// The bridge acts on the owner's answer (bot_wish_herder_decision.go);
		// the herder that asked is a scheduled pass, long stopped, and a respawn
		// would run under its trigger's live-run lease (gd-g6lGiK8KXv).
		d.logger.Info("decision is a herder's stale-wish question: the bridge acts on it, nothing to nudge or respawn",
			"decision", bead.ID, "filer", agentName)
		return
	}

	chosen := bead.Fields["chosen"]
	responseText := bead.Fields["response_text"]
	rationale := bead.Fields["rationale"]
	prompt := decisionQuestion(bead.Fields)

	// Build a nudge message that gives the agent full context about the
	// resolved decision: the original prompt, the chosen option, and the
	// rationale. Without the prompt, the agent has no idea which decision
	// was resolved or why.
	message := fmt.Sprintf("Decision %s resolved", bead.ID)
	if prompt != "" {
		message += fmt.Sprintf(": %s", prompt)
	}
	// A free-text answer has no `chosen` (the reply matched no option); its
	// text is in response_text. Show whichever is present so the agent always
	// sees the operator's answer (ka-0000000003).
	if chosen != "" {
		message += fmt.Sprintf("\nChosen: %s", chosen)
	}
	if responseText != "" {
		message += fmt.Sprintf("\nResponse: %s", responseText)
	}
	if rationale != "" {
		message += fmt.Sprintf("\nRationale: %s", rationale)
	}

	// If the chosen option requires an artifact, append requirement to nudge.
	if ra, ok := bead.Fields["required_artifact"]; ok && ra != "" {
		message += fmt.Sprintf("\nArtifact required (%s). Use `gb decision report %s` to submit.", ra, bead.ID)
	}

	if err := NudgeAgent(ctx, d.daemon, d.logger, agentName, message); err != nil {
		// For shutdown decisions, don't respawn — the agent is already stopped,
		// which is the desired end state.
		if isShutdownDecision(chosen) {
			d.logger.Info("agent already stopped for shutdown decision, skipping respawn",
				"agent", agentName, "decision", bead.ID, "chosen", chosen)
			return
		}
		d.logger.Warn("nudge failed, attempting to respawn agent for decision",
			"agent", agentName, "decision", bead.ID, "error", err)
		d.respawnAgentForDecision(ctx, agentName, bead, message)
		return
	}

	d.logger.Info("nudged agent after decision resolved",
		"agent", agentName, "decision", bead.ID, "chosen", chosen)
}

// RespawnForDecision implements DecisionRespawner. It spawns a new agent to
// act on a resolved decision when the original agent is no longer running.
// Safe for concurrent callers — uses an internal dedup map with cooldown.
func (d *Decisions) RespawnForDecision(ctx context.Context, agentName string, bead BeadEvent, decisionMessage string) {
	d.respawnAgentForDecision(ctx, agentName, bead, decisionMessage)
}

// humanOperatorFromAgentBead lifts the HUMAN operator that ultimately drove a
// prior agent bead, so a respawn spawned to act on that agent's resolved
// decision inherits the real human instead of dead-ending on the
// fleet:slack-bridge sentinel (ka-0000007919).
//
// Resolution order mirrors the daemon's own per-hop attribution (kbeads
// audit.go operatorFromAgentFields → operatorFromSlackUserID):
//
//  1. the prior bead's persisted operated_by, when it is already a well-formed
//     human id (oidc:<email> or slack:U…);
//  2. otherwise the prior bead's slack_user_id, promoted to the daemon-accepted
//     slack:U… shape.
//
// Returns "" when neither yields a human — the caller then leaves operated_by
// unset rather than stamping a value the daemon fails closed on. This runs in
// the slack-bridge, which (unlike the /spawn command path) has no Bot handle to
// resolve slack→email, so it never mints oidc:<email> here; the daemon's OIDC
// map and the spawned_by chain-walk (ka-0000001404) upgrade slack:U… to the
// email downstream when one exists.
func humanOperatorFromAgentBead(agentBead *beadsapi.BeadDetail) string {
	if agentBead == nil {
		return ""
	}
	if op := agentBead.Fields["operated_by"]; IsHumanOperator(op) {
		return op
	}
	if uid := agentBead.Fields["slack_user_id"]; uid != "" {
		if candidate := "slack:" + uid; IsHumanOperator(candidate) {
			return candidate
		}
	}
	return ""
}

// stampResumeLineage sets the lineage fields on a respawn of agentBead (the
// decision-resume and escalation-resume paths). spawned_by is always the prior
// agent bead id: the daemon's operatedByFromAgentBead chain-walk follows it back
// to the human (ka-0000001404), and spawn authz admits the resume of a
// privileged role (gardener, plan-gardener) only as a same-role continuation of
// the bead spawned_by names.
//
// When agentBead holds a trigger's live-run lease (the engine fired it, so its
// spawned_by is "trigger:<id>", or it is itself such a resume), the respawn
// carries triggerLeaseField so the engine's single-live-run gates still count it.
// Without it the trigger fired a second session for the same plan beside the
// respawn (gd-H17IlFclcl). trigger_id and schedule_id are deliberately not
// carried: they mark an engine iteration for run accounting, and a resume is
// not a scheduled run.
func stampResumeLineage(extraFields map[string]string, agentBead *beadsapi.BeadDetail) {
	if agentBead == nil || agentBead.ID == "" {
		return
	}
	extraFields["spawned_by"] = agentBead.ID
	if trig := triggerLeaseOf(agentBead.Fields); trig != "" {
		extraFields[triggerLeaseField] = trig
	}
	// The successor keeps its predecessor's human owner (stampHumanOwner).
	if extraFields["owner"] == "" {
		if ow := agentBead.Fields["owner"]; IsHumanOperator(ow) {
			extraFields["owner"] = ow
		} else if op := humanOperatorFromAgentBead(agentBead); op != "" {
			extraFields["owner"] = op
		}
	}
}

// resumeExtraFields adds stampResumeLineage's fields to a decision- or
// escalation-resume's spawn fields; agentwake.Wake stamps the same spawned_by.
func resumeExtraFields(agentBead *beadsapi.BeadDetail, fields map[string]string) map[string]string {
	stampResumeLineage(fields, agentBead)
	return fields
}

// triggerLeaseOf returns the trigger whose live-run lease an agent bead holds,
// or "" when it holds none.
func triggerLeaseOf(fields map[string]string) string {
	if trig, ok := strings.CutPrefix(strings.TrimSpace(fields["spawned_by"]), "trigger:"); ok && trig != "" {
		return trig
	}
	return strings.TrimSpace(fields[triggerLeaseField])
}

// decisionResumeActor resolves the audit-comment author for an event-wake driven
// by a resolved decision: the human operator who drove the original decision when
// resolvable, else empty (eventWakeDormantAgent falls back to the system operator).
func decisionResumeActor(agentBead *beadsapi.BeadDetail) string {
	return humanOperatorFromAgentBead(agentBead)
}

// e2eTestProject is the throwaway project the E2E suites seed fixtures under.
// No real agent should ever be spawned in it — it has no git credentials, so a
// pod spawned there CrashLoops forever (ka-0000000809).
const e2eTestProject = "e2e-test"

// isE2EDecisionRespawn reports whether a resolved decision bead is an E2E test
// fixture that must NOT trigger a real agent respawn. It mirrors the label
// heuristics the trigger-source and escalation guards already use, and adds a
// hard refusal to respawn into project=e2e-test:
//   - `fixture:e2e:` label prefix (the same marker isE2EFixture keys on),
//   - the `e2e:true` label / `e2e-*` created_by heuristics (isE2EEscalation),
//   - project=e2e-test resolved from the bead's project:* label.
//
// Any one match is sufficient; the checks are deliberately redundant so a
// fixture is inert even if a future module forgets one labelling convention.
func isE2EDecisionRespawn(bead BeadEvent) bool {
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, e2eFixtureLabelPrefix) {
			return true
		}
	}
	if isE2EEscalation(bead.CreatedBy, bead.Labels) {
		return true
	}
	return projectFromLabels(bead.Labels) == e2eTestProject
}

// isMachineFiledDecision reports whether a decision's assignee or filer is a
// Keycloak machine principal ("machine:<clientId>"). No agent stands behind
// such a decision: the component that filed it acts on the answer itself (the
// Lands Gate's approval decisions are read back by the beads-viewer apply
// reactor, cmd/beads-viewer lands_apply_reactor.go). Nudging it fails, and the
// respawn fallback would mint a crew pod named after the client.
func isMachineFiledDecision(agentName string) bool {
	return strings.HasPrefix(strings.TrimSpace(agentName), "machine:")
}

// isHumanFiledDecision reports whether a decision's assignee or filer is a
// human principal rather than an agent: an OIDC id ("oidc:<email>"), a bare
// email, or a slack:U… operator id. A decision the owner files with kd from a
// laptop carries created_by=<email> and no agent requester, so the respawn
// fallback would mint a crew bead named after the human (gd-Iii8U3m2pt).
func isHumanFiledDecision(agentName string) bool {
	name := strings.TrimSpace(agentName)
	return strings.HasPrefix(name, "oidc:") || strings.Contains(name, "@") || IsHumanOperator(name)
}

// respawnAgentForDecision spawns a new agent to act on a resolved decision
// when the original agent is no longer running. The new agent is given full
// context about the decision and its original task.
func (d *Decisions) respawnAgentForDecision(ctx context.Context, agentName string, bead BeadEvent, decisionMessage string) {
	if d.daemon == nil || isMachineFiledDecision(agentName) {
		return
	}
	if isHumanFiledDecision(agentName) {
		d.logger.Info("skipping respawn — decision requester is a human principal, not an agent",
			"requester", agentName, "decision", bead.ID)
		return
	}
	// A human session that files with `kd --actor <name>` puts the actor in the
	// assignee and itself in created_by, so agentName above is the actor, not the
	// human. The laptop operator files as laptop-claude: every answer spawned a
	// crew pod named laptop-claude, and each of those left a closed agent bead,
	// so the any-status lookup below kept finding one (gd-xQ9Vi6jBmS). Only an
	// explicit requesting_agent_bead_id names an agent lane behind such a decision.
	if isHumanFiledDecision(bead.CreatedBy) && strings.TrimSpace(bead.Fields["requesting_agent_bead_id"]) == "" {
		d.logger.Info("skipping respawn — decision was created by a human principal under an actor name",
			"requester", agentName, "created_by", bead.CreatedBy, "decision", bead.ID)
		return
	}

	// Never respawn a REAL agent pod for an E2E test fixture (ka-0000000809).
	// This is a second, independent spawn path that the trigger-source
	// isE2EFixture guards never covered: the auto-close-cascade e2e module
	// (test-auto-close-cascade.sh scenario 4) creates a decision fixture and
	// closes it, whose handleClosed → nudgeAgent → here path spawned a live
	// crew pod on the shared prod cluster. That pod has no git credentials, so
	// it CrashLoops forever while holding ~2 CPU / 8Gi hostage. The fixture's
	// created_by is a throwaway e2e actor (never a live agent), so the nudge
	// ALWAYS fails and this respawn ALWAYS fired — one fixture close = one
	// guaranteed leaked pod.
	//
	// Two belt-and-suspenders checks, either one sufficient:
	//   1. the `fixture:e2e:` / `e2e:true` label heuristics (shared with the
	//      escalation and trigger-source guards), and
	//   2. a hard refusal to respawn into project=e2e-test, since no real agent
	//      should ever be spawned in that throwaway project even absent a label.
	if isE2EDecisionRespawn(bead) {
		d.logger.Info("skipping respawn for e2e-fixture decision — no real pod for a test fixture",
			"agent", agentName, "decision", bead.ID,
			"created_by", bead.CreatedBy, "labels", strings.Join(bead.Labels, ","))
		return
	}

	// Expired/dismissed decisions normally have nothing for the agent to act on,
	// so we skip the respawn to avoid pod churn. The EXCEPTION is a decision that
	// carries a requesting_task: the requesting agent stopped on the documented
	// auto-respawn-on-resolution contract (advice ka-0000003244), so if we skip
	// the respawn its task stays in_progress with a dead assignee forever and its
	// parent molecule stays blocked (ka-0000005665 — an operator DISMISSED a
	// cross-post gate, the requesting task never respawned, the molecule wedged).
	//
	// A DISMISSAL is still an operator RESOLUTION (the daemon's cancel path runs
	// the same close cascade as a resolve and parks the gated bead — ka-0000005620),
	// so respawn the requesting agent so it wakes, learns the gate was declined,
	// and disposes of its parked task rather than rotting silently. _expired is a
	// pure timeout with no operator input — re-spawning would just re-file the
	// same decision and time out again, so it still skips even with a
	// requesting_task (the task is left for the intake/janitor sweep, not looped).
	chosen := bead.Fields["chosen"]
	responseText := bead.Fields["response_text"]
	// SSE close events can drop fields, so an event with no answer is checked
	// against the stored decision before it is treated as unanswered.
	if chosen == "" && strings.TrimSpace(responseText) == "" {
		if stored, err := d.daemon.GetBead(ctx, bead.ID); err == nil && stored != nil {
			chosen, responseText = stored.Fields["chosen"], stored.Fields["response_text"]
		}
	}
	// A decision closed with neither an option nor a text answer was withdrawn,
	// not resolved: its filer can close it bare, so respawning on it would let an
	// agent wake a lane of its own choosing with no human in the loop
	// (gd-VuYwGGYLLT). chosen and response_text are written only by a resolver
	// principal (kbeads authorizeDecisionAnswerFields), so their presence is the
	// answer. The daemon re-checks the same evidence on the privileged spawn
	// itself (spawn_continuation_authz.go), so this is not the only gate.
	if chosen == "" && strings.TrimSpace(responseText) == "" {
		d.logger.Info("skipping respawn — decision was closed without an answer (withdrawn, not resolved)",
			"agent", agentName, "decision", bead.ID)
		return
	}
	requestingTask := strings.TrimSpace(bead.Fields["requesting_task"])
	skipDismissed := chosen == "dismissed" && requestingTask == ""
	if chosen == "_expired" || skipDismissed {
		d.logger.Debug("skipping respawn for expired/dismissed decision with no parked requesting task",
			"agent", agentName, "decision", bead.ID, "chosen", chosen, "requesting_task", requestingTask)
		return
	}
	if chosen == "dismissed" {
		d.logger.Info("respawning requesting agent for DISMISSED decision — parked task must be disposed, not stranded",
			"agent", agentName, "decision", bead.ID, "requesting_task", requestingTask)
	}

	// Don't respawn for shutdown decisions — starting a dead agent just to
	// tell it to stop is wasteful and causes unnecessary pod churn.
	if isShutdownDecision(chosen) {
		d.logger.Info("skipping respawn for shutdown decision",
			"agent", agentName, "decision", bead.ID, "chosen", chosen)
		return
	}

	// Dedup: don't respawn if we already spawned for this decision recently.
	d.spawnMu.Lock()
	// Prune stale entries.
	now := time.Now()
	for k, t := range d.recentSpawn {
		if now.Sub(t) > decisionSpawnCooldown {
			delete(d.recentSpawn, k)
		}
	}
	if t, ok := d.recentSpawn[bead.ID]; ok && now.Sub(t) < decisionSpawnCooldown {
		d.spawnMu.Unlock()
		d.logger.Info("skipping respawn — already spawned recently for this decision",
			"agent", agentName, "decision", bead.ID)
		return
	}
	d.recentSpawn[bead.ID] = now
	d.spawnMu.Unlock()

	// Resolve the original agent's project and task from the agent bead.
	agentName = extractAgentName(agentName)
	project := ""
	taskID := ""
	role := "crew"

	// Try to resolve agent context from the requesting_agent_bead_id first,
	// then fall back to FindAgentBead, then to FindAgentBeadAnyStatus.
	// FindAgentBead sees active beads only, but the usual requester here has
	// gb-stopped and so has a CLOSED bead: an agent that filed with
	// kd create --type decision (no requesting_agent_bead_id) is found only by
	// the any-status lookup. GetBead above returns closed beads too, so the
	// code below already handles a closed agentBead.
	var agentBead *beadsapi.BeadDetail
	if reqBeadID := bead.Fields["requesting_agent_bead_id"]; reqBeadID != "" {
		if b, err := d.daemon.GetBead(ctx, reqBeadID); err == nil {
			agentBead = b
		}
	}
	if agentBead == nil {
		if b, err := d.daemon.FindAgentBead(ctx, agentName); err == nil {
			agentBead = b
		}
	}
	if agentBead == nil {
		b, err := d.daemon.FindAgentBeadAnyStatus(ctx, agentName)
		switch {
		case err == nil:
			agentBead = b
		case errors.Is(err, beadsapi.ErrAgentNotFound):
			// No agent bead behind the requester in any status: it is not an
			// agent (an unknown actor name, e.g. a laptop kd session), so there
			// is no lane to resume. Spawning would mint a fresh crew named after
			// that actor (gd-Iii8U3m2pt).
			d.logger.Info("skipping respawn — requester resolves to no agent bead",
				"requester", agentName, "decision", bead.ID,
				"requesting_agent_bead_id", bead.Fields["requesting_agent_bead_id"])
			return
		default:
			// A lookup failure is not evidence the requester is not an agent;
			// respawn without its context rather than strand a parked task.
			d.logger.Warn("decision-resume: agent bead lookup failed; respawning without agent context",
				"agent", agentName, "decision", bead.ID, "error", err)
		}
	}

	// A laptop agent is never respawned as a pod: the answer waits for it as
	// mail (spec §7, gd-XXYOPF1sCs).
	if reviveLocalAgentByMail(ctx, d.daemon, d.logger, agentBead,
		fmt.Sprintf("Decision %s was resolved while you were not running.", bead.ID), decisionMessage) {
		return
	}

	if agentBead != nil {
		project = agentBead.Fields["project"]
		taskID = agentBead.Fields["task_id"]
		if r := agentBead.Fields["role"]; r != "" {
			role = r
		}
	}
	// If we can't determine the project from the agent bead, try the decision labels.
	if project == "" {
		project = projectFromLabels(bead.Labels)
	}

	// The shared wake (internal/agentwake, gd-T7aFA3tFoW): a DORMANT persistent
	// identity (closed, successor=same) is revived IN PLACE with
	// wake_reason=decision, keeping its name and transcript; because events
	// reset TTL, not budget, respawn_count is untouched (persistent-agents
	// Phase 3, ka-0000000027). A FROZEN one (gb yield checkpointed it, bead
	// open at agent_state=stopping) is thawed (Δ4/A2, ka-0000000241). Anything
	// else gets a fresh SpawnAgent carrying the Slack routing (gd-jOHtHdkdIt),
	// plan_id (ka-0000000352) and the human operator (ka-0000007919).
	res := agentwake.Wake(ctx, d.daemon, d.logger, agentwake.Request{
		AgentName: agentName,
		AgentBead: agentBead,
		Reason:    beadsapi.WakeReasonDecision,
		Operator:  decisionResumeActor(agentBead),
		Project:   project,
		TaskID:    taskID,
		Role:      role,
		ExtraFields: resumeExtraFields(agentBead, map[string]string{
			"spawn_source":     "decision-resume",
			"decision_bead_id": bead.ID,
		}),
		CarrySlackRouting: true,
		Prepare: func(ctx context.Context) (string, bool) {
			return d.decisionResumePrompt(ctx, agentName, bead, project, taskID, chosen, requestingTask, decisionMessage)
		},
		LogAttrs: []any{"decision", bead.ID},
	})
	if res.Outcome == agentwake.Spawned {
		d.logger.Info("respawned agent to act on resolved decision",
			"agent", agentName, "agent_bead", res.AgentBeadID,
			"decision", bead.ID, "chosen", chosen,
			"project", project, "task", taskID, "thawed", res.Thawed)
	}
}

// decisionResumePrompt builds the prompt for an agent spawned fresh to act on
// a resolved decision, or returns false when no pod may be spawned.
func (d *Decisions) decisionResumePrompt(ctx context.Context, agentName string, bead BeadEvent, project, taskID, chosen, requestingTask, decisionMessage string) (string, bool) {
	// Belt-and-suspenders (ka-0000000809): even if the close event arrived with
	// its labels stripped (SSE close events can drop fields), the RESOLVED
	// project is authoritative — never spawn a real pod into the throwaway
	// e2e-test project. This is the last gate before SpawnAgent, so it holds
	// regardless of how the project was derived (agent bead OR decision labels).
	if project == e2eTestProject {
		d.logger.Info("skipping respawn — resolved project is the e2e-test fixture project, no real pod",
			"agent", agentName, "decision", bead.ID, "project", project)
		return "", false
	}

	// Build a prompt that gives the new agent full decision context.
	prompt := fmt.Sprintf(
		"You have been respawned because decision %s was resolved while you were not running.\n\n%s",
		bead.ID, decisionMessage)
	if taskID != "" {
		prompt += fmt.Sprintf("\n\nYour previous task was %s. Run `kd show %s` to see the full task context.", taskID, taskID)
	}
	// A dismissal is a DECLINE, not a green light. Tell the woken agent to
	// dispose of the parked gate task (cancel/close it) so its parent
	// molecule unwedges — the exact failure this respawn exists to fix
	// (ka-0000005665). Do NOT re-file the same gate.
	if chosen == "dismissed" && requestingTask != "" {
		prompt += fmt.Sprintf(
			"\n\nNOTE: this decision was DISMISSED (declined), not answered — the gate is OFF. "+
				"Do NOT re-file it or proceed as if approved. Instead, dispose of the parked requesting task %s "+
				"(cancel or close it as appropriate with `kd show %s` first) so its parent molecule unblocks.",
			requestingTask, requestingTask)
	}
	if chosen != "dismissed" && requestingTask != "" {
		question := decisionQuestion(bead.Fields)
		if question == "" {
			if stored, err := d.daemon.GetBead(ctx, bead.ID); err == nil && stored != nil {
				question = decisionQuestion(stored.Fields)
			}
		}
		land := projectFromLabels(bead.Labels)
		if land == "" {
			land = project
		}
		prompt += planAnswerNote(question, requestingTask, land, bead.ID)
	}
	return prompt, true
}

// handleReportClosed is called when a report bead is closed. It posts the
// report content as a thread reply on the linked decision's Slack message.
func (d *Decisions) handleReportClosed(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}
	if bead.Type != "report" {
		return
	}

	decisionID := bead.Fields["decision_id"]
	reportType := bead.Fields["report_type"]
	content := bead.Fields["content"]

	// SSE close events may strip large fields. Fetch the full bead when
	// content or decision_id is missing so the report can still be posted.
	if (content == "" || decisionID == "") && d.daemon != nil {
		if detail, err := d.daemon.GetBead(ctx, bead.ID); err == nil {
			if bead.Fields == nil {
				bead.Fields = make(map[string]string)
			}
			if v := detail.Fields["decision_id"]; v != "" && decisionID == "" {
				decisionID = v
			}
			if v := detail.Fields["report_type"]; v != "" && reportType == "" {
				reportType = v
			}
			if v := detail.Fields["content"]; v != "" && content == "" {
				content = v
			}
		}
	}

	if decisionID == "" {
		d.logger.Debug("report bead has no decision_id", "id", bead.ID)
		return
	}

	d.logger.Info("report bead closed",
		"id", bead.ID, "decision_id", decisionID, "report_type", reportType)

	if d.notifier != nil && content != "" {
		if err := d.notifier.PostReport(ctx, bead.ID, decisionID, reportType, content); err != nil {
			d.logger.Error("failed to post report to Slack",
				"report", bead.ID, "decision", decisionID, "error", err)
		}
	}
}

// resolveDecisionAgent extracts the agent name from a decision bead.
// Checks requested_by field first, then assignee.
func resolveDecisionAgent(bead *BeadEvent) string {
	if agent := bead.Fields["requested_by"]; agent != "" {
		return extractAgentName(agent)
	}
	if bead.Assignee != "" {
		return extractAgentName(bead.Assignee)
	}
	return ""
}
