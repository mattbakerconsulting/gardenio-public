// Package bridge provides the JIRA squawk forwarder.
//
// JiraSquawkForwarder watches for closed squawk message beads from agents
// that are bound to a JIRA ticket (have jira_key metadata) and posts the
// squawk text as JIRA comments. This gives agents a way to communicate with
// JIRA ticket watchers. Modeled after GitLabSquawkForwarder.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// JiraSquawkClient is the subset of beadsapi.Client used by JiraSquawkForwarder.
type JiraSquawkClient interface {
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
}

// JiraSquawkJiraClient is the subset of JiraClient used by JiraSquawkForwarder.
// A squawk is forwarded as an internal note (never a public/customer-facing
// reply), so the forwarder uses the visibility-aware comment method to stamp
// sd.public.comment internal:true (epic ka-0000004904).
type JiraSquawkJiraClient interface {
	AddCommentWithVisibility(ctx context.Context, key, text string, vis CommentVisibility) error
}

// JiraSquawkForwarder watches for squawk beads from JIRA-bound agents and
// posts them as JIRA comments.
type JiraSquawkForwarder struct {
	daemon JiraSquawkClient
	jira   JiraSquawkJiraClient
	logger *slog.Logger

	// writePolicy is the config-driven, project-agnostic write policy
	// (ka-0000007252). When non-nil it is the SOLE decision path for both the
	// project verdict and the per-role internal-note verdict — FSM is just a
	// configured toggle, not a compiled-in special case. The production
	// jira-bridge wires this. When nil (tests) the legacy gate pair below is used;
	// DefaultWritePolicy reproduces it byte-for-byte so the swap is behavior-safe.
	writePolicy *WritePolicyStore
	// gate enforces the per-role field-write allowlist on the squawk comment.
	// LEGACY fallback — used only when writePolicy is nil. nil ⇒ ungated
	// (preserves legacy behavior + gate-less test construction).
	gate *AllowlistGate
	// projectGate is the coordinated deny-by-default project gate. LEGACY
	// fallback — used only when writePolicy is nil. FSM is governed by the single
	// JIRA_FSM_SYNC_ENABLED toggle shared by all four outbound paths
	// (ka-0000001679). Never nil after construction — NewJiraSquawkForwarder
	// falls back to the package default gate (FSM off) so behavior is unchanged
	// when unwired.
	projectGate *ProjectSyncGate

	mu       sync.Mutex
	seen     map[string]bool      // dedup on SSE reconnect
	throttle map[string]time.Time // agent → last forward time
}

// JiraSquawkForwarderConfig holds configuration for the forwarder.
type JiraSquawkForwarderConfig struct {
	Daemon JiraSquawkClient
	Jira   JiraSquawkJiraClient
	Logger *slog.Logger
	// WritePolicy is the config-driven, project-agnostic write policy store
	// (ka-0000007252). When set it is the sole decision path (project + comment)
	// and the legacy Gate/ProjectGate below are ignored. The production
	// jira-bridge sets this; nil selects the legacy gate pair (tests).
	WritePolicy *WritePolicyStore
	// Gate is the per-role field-write allowlist. LEGACY fallback (used only when
	// WritePolicy is nil). nil ⇒ comments are ungated.
	Gate *AllowlistGate
	// ProjectGate is the coordinated project gate shared across all four
	// outbound Jira paths (ka-0000001679). LEGACY fallback (used only when
	// WritePolicy is nil). nil ⇒ the package default gate (FSM off, legacy
	// deny-by-default) is used, so behavior is unchanged.
	ProjectGate *ProjectSyncGate
}

// jiraSquawkThrottle is the minimum interval between JIRA comments per agent.
const jiraSquawkThrottle = 1 * time.Minute

// jiraSquawkMaxLen is the maximum length for a JIRA comment body.
const jiraSquawkMaxLen = 4000

// NewJiraSquawkForwarder creates a new JIRA squawk forwarder.
func NewJiraSquawkForwarder(cfg JiraSquawkForwarderConfig) *JiraSquawkForwarder {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	gate := cfg.ProjectGate
	if gate == nil {
		gate = defaultProjectSyncGate
	}
	return &JiraSquawkForwarder{
		daemon:      cfg.Daemon,
		jira:        cfg.Jira,
		logger:      logger,
		writePolicy: cfg.WritePolicy,
		gate:        cfg.Gate,
		projectGate: gate,
		seen:        make(map[string]bool),
		throttle:    make(map[string]time.Time),
	}
}

// allowProject reports whether the bridge may forward a squawk to the project
// owning jiraKey. Prefers the config-driven write policy; falls back to the
// legacy coordinated project gate when unwired.
func (f *JiraSquawkForwarder) allowProject(jiraKey string) bool {
	if f.writePolicy != nil {
		return f.writePolicy.AllowProject(jiraKey)
	}
	return f.projectGate.AllowProject(jiraKey)
}

// projectGateDiag returns a human-readable description of the project verdict
// source for the "blocked comment" warning logs.
func (f *JiraSquawkForwarder) projectGateDiag() any {
	if f.writePolicy != nil {
		return f.writePolicy.Projects()
	}
	return f.projectGate.FSMSyncEnabled()
}

// allowComment reports whether the squawk internal note may be posted for role
// on the project owning jiraKey. Prefers the config-driven write policy; falls
// back to the legacy per-role allowlist gate. Matching the legacy call site, an
// unwired legacy gate (nil) permits the comment.
func (f *JiraSquawkForwarder) allowComment(jiraKey, role string, vis CommentVisibility) bool {
	if f.writePolicy != nil {
		return f.writePolicy.AllowComment(jiraKey, role, vis)
	}
	if f.gate == nil {
		return true
	}
	return f.gate.AllowComment(role, vis)
}

// RegisterHandlers registers SSE event handlers on the given stream for
// closed message beads (squawk events).
func (f *JiraSquawkForwarder) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.closed", f.handleClosed)
	f.logger.Info("JIRA squawk forwarder registered SSE handlers",
		"topics", []string{"beads.bead.closed"})
}

func (f *JiraSquawkForwarder) handleClosed(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}

	// Only handle message beads with the "squawk" or "say" label.
	if bead.Type != "message" || (!hasLabel(bead.Labels, "squawk") && !hasLabel(bead.Labels, "say")) {
		return
	}

	// Dedup on SSE reconnect.
	if f.alreadySeen(bead.ID) {
		return
	}

	// Extract the source agent.
	agent := extractSquawkAgent(*bead)
	if agent == "" {
		return
	}

	// Look up the agent bead to find JIRA binding metadata.
	agentBead, err := f.daemon.FindAgentBead(ctx, agent)
	if err != nil {
		f.logger.Debug("jira-squawk: agent bead not found, skipping",
			"agent", agent, "squawk_id", bead.ID)
		return
	}

	// Check if the agent has JIRA key binding.
	jiraKey := agentBead.Fields["jira_key"]
	if jiraKey == "" {
		if notes := beadsapi.ParseNotes(agentBead.Notes); notes != nil {
			jiraKey = notes["jira_key"]
		}
	}
	if jiraKey == "" {
		return
	}

	// Mention-time --quiet intent hint (design-doc F-E, epic ka-0000007401): the
	// operator asked for a low-noise ticket, so do not mirror the agent's squawks
	// as internal notes. The agent's explicit `gb jira reply` answers still post
	// (that is a direct reply, not this automatic forward). Read off the agent
	// bead's durable jira_quiet field — the same fact the spawn-ack (via the
	// spawn event) and completion-ack gate on.
	if jiraQuietSuppressed(agentBead.Fields) {
		f.logger.Debug("jira-squawk: suppressed on --quiet ticket",
			"agent", agent, "jira_key", jiraKey)
		return
	}

	// Safety: deny-by-default project gate. The config-driven write policy folds
	// the former allow/block sets + FSM toggle into one project-agnostic verdict
	// (ka-0000007252); the legacy coordinated gate is the fallback when unwired.
	// This project verdict is mode-independent (always hard-enforced).
	projectKey := jiraProjectKey(jiraKey)
	if !f.allowProject(jiraKey) {
		f.logger.Warn("jira-squawk: blocked comment on non-writable project",
			"agent", agent, "jira_key", jiraKey, "project_key", projectKey,
			"writable_projects", f.projectGateDiag())
		return
	}

	// Bead→ticket scoping (ka-0000001679): the comment target jiraKey is resolved
	// solely from the acting agent's OWN binding (its jira_key field, else the
	// jira_key in its notes). There is no separate, untrusted target here — the
	// forwarder can only ever comment on the agent's own bound ticket, so the
	// "target == bound" invariant holds by construction. The genuinely
	// independent-source scope guard (polled key vs bound key) lives in the
	// poller back-off path.

	// Per-role field-write allowlist: a squawk is an internal note, gated on the
	// acting agent's role (epic ka-0000004904). An unknown role fails closed in
	// enforce mode; an unwired legacy gate (nil) permits the comment. The
	// config-driven write policy (when wired) folds project + role + visibility
	// into this one verdict (ka-0000007252).
	role := agentBead.Fields["role"]
	if !f.allowComment(jiraKey, role, CommentInternal) {
		f.logger.Warn("jira-squawk: blocked comment by field allowlist",
			"agent", agent, "jira_key", jiraKey, "role", role)
		return
	}

	// Throttle: max 1 JIRA comment per minute per agent.
	if f.isThrottled(agent) {
		f.logger.Info("jira-squawk: throttled",
			"agent", agent, "jira_key", jiraKey)
		return
	}

	// Get the message text.
	text := bead.Fields["text"]
	if text == "" {
		text = bead.Title
	}
	if text == "" {
		return
	}

	// Truncate long messages.
	if len(text) > jiraSquawkMaxLen {
		text = text[:jiraSquawkMaxLen-3] + "..."
	}

	// Format the comment for JIRA.
	body := fmt.Sprintf("[gardenio agent] %s:\n\n%s", agent, text)

	// Stamp operator attribution ("Acted by <operator>") so the Jira history
	// stays honest about which human drove the agent despite the shared service
	// account (epic ka-0000004904 decision #4 / ka-0000006003). The agent bead is
	// a daemon-resolved BeadDetail, so we get a human display name for free:
	// prefer the typed OperatedBy/OperatedByDisplay, falling back to the
	// operated_by field when the typed value is empty. Appended AFTER the
	// jiraSquawkMaxLen truncation above so the attribution line is never clipped.
	operatedBy := agentBead.OperatedBy
	if operatedBy == "" {
		operatedBy = agentBead.Fields["operated_by"]
	}
	body = withAttribution(body, operatorAttributionLine(operatedBy, agentBead.OperatedByDisplay))

	if err := f.jira.AddCommentWithVisibility(ctx, jiraKey, body, CommentInternal); err != nil {
		f.logger.Error("jira-squawk: failed to post JIRA comment",
			"agent", agent, "jira_key", jiraKey, "error", err)
	} else {
		f.markForwarded(agent)
		f.logger.Info("jira-squawk: posted JIRA comment",
			"agent", agent, "jira_key", jiraKey, "text_length", len(text))
	}
}

// alreadySeen returns true if this bead has already been processed.
func (f *JiraSquawkForwarder) alreadySeen(beadID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen[beadID] {
		return true
	}
	f.seen[beadID] = true
	return false
}

// isThrottled returns true if the agent has forwarded a squawk within the throttle window.
func (f *JiraSquawkForwarder) isThrottled(agent string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.throttle[agent]; ok && time.Since(t) < jiraSquawkThrottle {
		return true
	}
	return false
}

// markForwarded records that a squawk was just forwarded for this agent.
func (f *JiraSquawkForwarder) markForwarded(agent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Prune stale entries.
	for k, t := range f.throttle {
		if time.Since(t) > jiraSquawkThrottle {
			delete(f.throttle, k)
		}
	}
	f.throttle[agent] = time.Now()
}
