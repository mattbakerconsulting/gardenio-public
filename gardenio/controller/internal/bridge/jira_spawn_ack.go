// Package bridge — Jira mention spawn acknowledgment.
//
// When an operator @mentions @gardenio in Slack, an agent card appears in the
// channel the instant the mention lands — immediate, conversational proof that
// the fleet heard them. On a Jira ticket that feedback was MISSING: the mention
// handler spawns an agent and creates a task bead, but the TICKET itself stays
// silent until the spawned agent posts its first `gb jira reply`. That reply can
// be minutes away — or never arrive, if the boot cycle is interrupted mid-flight
// (common in this fleet). To the human on the ticket the @gardenio mention looked
// like it fell into a void.
//
// JiraSpawnAcknowledger closes that gap. It is wired onto the mention handler's
// OnSpawn extension point (JiraMentionHandlerConfig.OnSpawn — previously nil in
// production) and posts ONE internal-note comment on the ticket immediately
// after a successful mention-spawn: the Jira analog of the Slack agent card.
// "👋 On it — <agent> is on this (bead <id>)." plus the same operator
// attribution line every other outbound path stamps.
//
// SECURITY / INVARIANT NOTES (this is an outbound Jira write path):
//
//   - The acknowledger is a SEPARATE collaborator, not a field on the mention
//     handler. The mention handler still holds no *JiraClient, so the structural
//     no-write-surface invariant on the mention path (TestJiraMention_NoJiraWriteSurface,
//     ka-0000004831) is preserved: this file, not jira_mentions.go, owns the write.
//   - Every write routes through the shared WritePolicyStore using the
//     REPLY-predicate gates (AllowReplyProject / AllowReplyComment), exactly like
//     the agent reply endpoint (jira_reply.go): FSM and unknown projects are
//     hard-blocked in every mode, GBFSM is eligible in both outbox-toggle states
//     (an ack is not an FSM/outbox write — ka-0000007628), and public comments
//     are never posted. A nil store denies everything (fail closed).
//   - Operator attribution (withAttribution / operatorAttributionLine) is stamped
//     so the Jira history stays honest about which human drove the spawn despite
//     the shared bot service account (epic ka-0000004904).
//   - This file is in noBypassFiles (jira_field_gate_nobypass_test.go), so the
//     source-level scan proves the AddCommentWithVisibility call sits inside a
//     function that also consults a write gate.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// JiraSpawnAckJiraClient is the subset of *JiraClient the acknowledger needs:
// a single visibility-aware comment post. An ack is always an internal note, so
// the visibility-aware method is used to stamp sd.public.comment internal:true
// (epic ka-0000004904), matching the squawk forwarder.
type JiraSpawnAckJiraClient interface {
	AddCommentWithVisibility(ctx context.Context, key, text string, vis CommentVisibility) error
}

// jiraSpawnAckMaxLen caps the ack comment body (the summary is echoed back), so
// one ack cannot post an unbounded comment to Jira. Well under the 4000-char
// squawk ceiling — an ack is a one-liner, not a transcript.
const jiraSpawnAckMaxLen = 1000

// JiraSpawnAcknowledger posts an immediate "on it" internal note to a Jira
// ticket right after a @gardenio mention spawns an agent. It is the Jira analog
// of the Slack agent card (plan ka-0000007401).
type JiraSpawnAcknowledger struct {
	jira   JiraSpawnAckJiraClient
	logger *slog.Logger

	// writePolicy is the config-driven, project-agnostic write policy shared
	// with every other outbound Jira path (ka-0000007252). It is the SOLE
	// decision path for both the project verdict and the per-role comment
	// verdict. A nil store denies everything (fail closed) — the acknowledger
	// never posts without an explicit allow.
	writePolicy *WritePolicyStore
}

// JiraSpawnAcknowledgerConfig holds configuration for JiraSpawnAcknowledger.
type JiraSpawnAcknowledgerConfig struct {
	// Jira posts the internal-note comment. Required; a nil client disables the
	// acknowledger (Acknowledge becomes a no-op) so a partially-wired bridge
	// never panics.
	Jira JiraSpawnAckJiraClient
	// WritePolicy gates every ack. Required in production; a nil store fails
	// closed (no ack is ever posted), so an unwired policy cannot open an
	// ungated write surface.
	WritePolicy *WritePolicyStore
	Logger      *slog.Logger
}

// NewJiraSpawnAcknowledger constructs a JiraSpawnAcknowledger.
func NewJiraSpawnAcknowledger(cfg JiraSpawnAcknowledgerConfig) *JiraSpawnAcknowledger {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &JiraSpawnAcknowledger{
		jira:        cfg.Jira,
		logger:      logger,
		writePolicy: cfg.WritePolicy,
	}
}

// OnSpawn returns a JiraMentionNotifyFunc suitable for
// JiraMentionHandlerConfig.OnSpawn. It adapts the acknowledger's Acknowledge
// method to the notify-callback shape the mention handler invokes after a
// successful spawn.
func (a *JiraSpawnAcknowledger) OnSpawn() JiraMentionNotifyFunc {
	return a.Acknowledge
}

// Acknowledge posts the immediate "on it" internal note for a mention spawn.
// It is safe to call with a nil acknowledger or a nil Jira client (both no-op),
// so the mention handler can always invoke OnSpawn without a guard.
//
// The write is gate-guarded exactly like the reply path: the mode-independent
// AllowReplyProject pre-gate rejects FSM/unknown projects first, then the full
// AllowReplyComment verdict (project + role + internal visibility) must pass.
// Failures are logged and swallowed — the agent has already been spawned, so a
// failed ack must never fail the mention (the ack is UX, not control flow).
func (a *JiraSpawnAcknowledger) Acknowledge(ctx context.Context, event JiraMentionSpawnEvent) {
	if a == nil || a.jira == nil {
		return
	}
	jiraKey := strings.TrimSpace(event.JiraKey)
	if jiraKey == "" {
		return
	}

	// Mention-time --quiet intent hint (design-doc F-E, epic ka-0000007401): the
	// operator asked for a low-noise ticket, so suppress the automatic "👋 On it"
	// note. The agent still does the work and posts its explicit `gb jira reply`
	// answer; only the lifecycle chatter is silenced. The squawk-forward and
	// completion-ack forwarders read the same intent off the agent bead's
	// jira_quiet field.
	if event.Quiet {
		recordJiraSpawnAck(jiraAckQuiet)
		a.logger.Info("jira-spawn-ack: suppressed ack on --quiet mention",
			"agent", event.AgentName, "jira_key", jiraKey)
		return
	}

	// Hard project pre-gate — mode-independent, exactly like the reply resolver
	// and the squawk forwarder. AllowReplyProject uses the spawn predicate
	// (projectSpawnable, SyncToggle ignored): an ack is not an FSM/outbox write,
	// so it must not be coupled to the outbox toggle — GBFSM is ack-eligible in
	// both toggle states while FSM/unknown projects stay denied. A nil store
	// denies everything (fail closed).
	if a.writePolicy == nil || !a.writePolicy.AllowReplyProject(jiraKey) {
		a.logger.Warn("jira-spawn-ack: blocked ack on non-replyable project",
			"agent", event.AgentName, "jira_key", jiraKey,
			"project_key", jiraProjectKey(jiraKey))
		recordJiraSpawnAck(jiraAckBlocked)
		return
	}

	// Full comment verdict: project + role + internal visibility. Mirrors the
	// reply path (AllowReplyComment) so an ack is governed by the SAME per-role
	// allowlist as `gb jira reply`; the internal-only visibility means the
	// AllowPublicComment ceiling is never in play, but keeping the verdict here
	// means a role with no comment grant (e.g. fsm-triage) posts no ack.
	if !a.writePolicy.AllowReplyComment(jiraKey, event.Role, CommentInternal) {
		a.logger.Warn("jira-spawn-ack: blocked ack by write policy",
			"agent", event.AgentName, "jira_key", jiraKey, "role", event.Role)
		recordJiraSpawnAck(jiraAckBlocked)
		return
	}

	body := buildJiraSpawnAckBody(event)
	if err := a.jira.AddCommentWithVisibility(ctx, jiraKey, body, CommentInternal); err != nil {
		a.logger.Error("jira-spawn-ack: failed to post ack comment",
			"agent", event.AgentName, "jira_key", jiraKey, "error", err)
		recordJiraSpawnAck(jiraAckError)
		return
	}
	recordJiraSpawnAck(jiraAckPosted)
	a.logger.Info("jira-spawn-ack: posted ack comment",
		"agent", event.AgentName, "jira_key", jiraKey,
		"task", event.TaskBeadID, "role", event.Role)
}

// buildJiraSpawnAckBody formats the immediate-acknowledgment comment body. It
// names the spawned agent and its task bead so a human on the ticket can follow
// the work, and appends the operator attribution line every outbound path
// stamps (withAttribution) so the Jira history stays honest about the driving
// human. The [gardenio agent] prefix matches the squawk-forward body so all
// bot-authored comments read uniformly in the ticket history.
func buildJiraSpawnAckBody(event JiraMentionSpawnEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[gardenio agent] 👋 On it — %s is picking this up.", event.AgentName)
	if event.TaskBeadID != "" {
		fmt.Fprintf(&b, "\nTracking bead: %s", event.TaskBeadID)
	}
	b.WriteString("\nI'll reply here as I make progress.")

	body := b.String()
	if len(body) > jiraSpawnAckMaxLen {
		body = body[:jiraSpawnAckMaxLen-3] + "..."
	}

	// Attribution appended AFTER truncation so the "Acted by <operator>" line is
	// never clipped (mirrors jira_squawk_forward.go). operatorAttributionLine
	// returns "" for a non-human / empty operator, in which case withAttribution
	// leaves the body unchanged.
	return withAttribution(body, operatorAttributionLine(event.Operator, event.OperatorDisplay))
}
