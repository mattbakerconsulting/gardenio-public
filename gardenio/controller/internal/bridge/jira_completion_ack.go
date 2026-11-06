// Package bridge — Jira mention completion/failure acknowledgment.
//
// The Slack bridge walks a @gardenio-mentioned message through a full reaction
// lifecycle: 👀 (seen) → ⏳ (working) → ✅ (done) / ❌ (failed), driven by
// Bot.NotifyAgentState off the spawned agent bead's terminal transition
// (bot_agents.go). Jira built only the FRONT of that lifecycle — the spawn-ack
// "👋 On it" note (jira_spawn_ack.go). There was no conversational DONE/FAILED
// signal tied to a mention-spawned agent: a mention agent that finished — or
// crashed mid-boot, common in this fleet — left the ticket silent after the
// "On it", so a human on the ticket never learned the work landed or died.
//
// JiraCompletionAcknowledger closes that back-half gap. It watches the mention-
// spawned agent bead's terminal CLOSE (beads.bead.closed) and posts ONE internal
// note naming the outcome: "✅ Done" for a clean finish, "❌ Failed" for a crash,
// "⚠️" for a rate-limit / reaped exit — the Jira analog of the Slack card's
// terminal ✅/❌. When the agent left a wrap-up, its one-line tldr is echoed so
// the ticket carries the outcome summary, not just a status glyph.
//
// It is the exact structural sibling of JiraSpawnAcknowledger (the front-half):
// a SEPARATE collaborator (not a field on the mention handler, so the mention
// path's no-write-surface invariant TestJiraMention_NoJiraWriteSurface holds),
// every write routed through the shared WritePolicyStore using the REPLY
// predicates (AllowReplyProject / AllowReplyComment) so front and back halves
// gate identically and neither couples to the FSM/outbox toggle (ka-0000007628),
// operator attribution stamped (epic ka-0000004904), and this file listed in
// noBypassFiles so the source scan proves the AddCommentWithVisibility call sits
// behind a write gate.
//
// The agent bead carries jira_key + spawn_source=jira-mention (stamped at spawn,
// jira_mentions.go), so the ticket is recovered from the close event's own
// Fields — no daemon lookup, exactly like the spawn-ack works off its event.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// JiraCompletionAckJiraClient is the subset of *JiraClient the acknowledger
// needs: a single visibility-aware comment post. A completion note is always an
// internal note, so the visibility-aware method stamps sd.public.comment
// internal:true (epic ka-0000004904), matching the spawn-ack and squawk paths.
type JiraCompletionAckJiraClient interface {
	AddCommentWithVisibility(ctx context.Context, key, text string, vis CommentVisibility) error
}

// jiraCompletionAckMaxLen caps the completion comment body (the tldr is echoed
// back). Well under the 4000-char squawk ceiling — a completion note is a one-
// liner plus an optional tldr, not a transcript.
const jiraCompletionAckMaxLen = 1000

// jiraCompletionSeenTTL bounds the terminal-close dedup window. SSE redelivers
// events on reconnect, so a terminal close can arrive more than once; a single
// agent bead reaches terminal exactly once, so a modest window suffices to
// suppress a redelivered duplicate without holding every closed bead forever.
const jiraCompletionSeenTTL = 30 * time.Minute

// JiraCompletionAcknowledger posts a terminal DONE/FAILED internal note to a
// Jira ticket when a @gardenio-mention-spawned agent reaches its terminal close.
// It is the Jira analog of the Slack card's terminal ✅/❌ (plan ka-0000007401,
// GAP 4 / ka-0000004899).
type JiraCompletionAcknowledger struct {
	jira   JiraCompletionAckJiraClient
	logger *slog.Logger

	// writePolicy is the config-driven, project-agnostic write policy shared
	// with every other outbound Jira path (ka-0000007252). It is the SOLE
	// decision path for both the project verdict and the per-role comment
	// verdict. A nil store denies everything (fail closed) — the acknowledger
	// never posts without an explicit allow.
	writePolicy *WritePolicyStore

	mu   sync.Mutex
	seen map[string]time.Time // agent bead ID → first terminal-close time (dedup)
}

// JiraCompletionAcknowledgerConfig holds configuration for
// JiraCompletionAcknowledger.
type JiraCompletionAcknowledgerConfig struct {
	// Jira posts the internal-note comment. Required; a nil client disables the
	// acknowledger (handleClosed becomes a no-op) so a partially-wired bridge
	// never panics.
	Jira JiraCompletionAckJiraClient
	// WritePolicy gates every note. Required in production; a nil store fails
	// closed (no note is ever posted), so an unwired policy cannot open an
	// ungated write surface.
	WritePolicy *WritePolicyStore
	Logger      *slog.Logger
}

// NewJiraCompletionAcknowledger constructs a JiraCompletionAcknowledger.
func NewJiraCompletionAcknowledger(cfg JiraCompletionAcknowledgerConfig) *JiraCompletionAcknowledger {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &JiraCompletionAcknowledger{
		jira:        cfg.Jira,
		logger:      logger,
		writePolicy: cfg.WritePolicy,
		seen:        make(map[string]time.Time),
	}
}

// RegisterHandlers subscribes the acknowledger to agent-bead terminal closes on
// the SSE stream. Only beads.bead.closed is watched: the agent bead reaches its
// terminal state exactly once at close, matching the squawk forwarder's close-
// only subscription and avoiding the transient working/rate_limited/spawning
// updates the Slack bridge's NotifyAgentState has to filter.
func (a *JiraCompletionAcknowledger) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.bead.closed", a.handleClosed)
	a.logger.Info("JIRA completion acknowledger registered SSE handlers",
		"topics", []string{"beads.bead.closed"})
}

// handleClosed posts the terminal completion note for a mention-spawned agent
// bead. It is scoped tightly: only a type=agent bead carrying
// spawn_source=jira-mention (the discriminator stamped at spawn,
// jira_mentions.go) and a jira_key binding is handled — every other closed bead
// (task beads, non-mention agents, dark beads) is ignored, and the mention TASK
// bead's generic "closed" note (jira_sync.handleClosed) is left untouched.
//
// The write is gate-guarded exactly like the reply/spawn-ack path: the mode-
// independent AllowReplyProject pre-gate rejects FSM/unknown projects first,
// then the full AllowReplyComment verdict (project + role + internal visibility)
// must pass. Failures are logged and swallowed — the agent has already finished,
// so a failed note must never surface as a panic or block the SSE stream.
func (a *JiraCompletionAcknowledger) handleClosed(ctx context.Context, data []byte) {
	if a == nil || a.jira == nil {
		return
	}
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}

	// Scope to mention-spawned agent beads only. spawn_source=jira-mention is
	// the precise discriminator: a mention TASK bead (type=task) keeps its
	// generic jira_sync close note, and a non-mention agent bead that happens to
	// carry a jira_key is not ours to comment on.
	if bead.Type != "agent" || bead.Fields["spawn_source"] != "jira-mention" {
		return
	}

	// Dark beads never flow upstream — mirror every other outbound path
	// (ka-0000005143).
	if hasLabel(bead.Labels, jiraDarkLabel) {
		return
	}

	// Recover the bound ticket from the close event's own Fields (jira_key,
	// stamped at spawn) — no daemon lookup, exactly like the spawn-ack works off
	// its event. Falls back to a jira:<KEY> label for older beads.
	jiraKey := jiraKeyFromBead(*bead)
	if jiraKey == "" {
		return
	}

	agent := bead.Assignee
	if agent == "" {
		agent = bead.Fields["agent"]
	}
	if agent == "" {
		agent = bead.Title // agent beads title == agent name (SpawnAgent)
	}

	// Mention-time --quiet intent hint (design-doc F-E, epic ka-0000007401): the
	// operator asked for a low-noise ticket, so suppress the terminal ✅/❌ note.
	// Read off the closing agent bead's durable jira_quiet field — the same fact
	// the spawn-ack (via the spawn event) and squawk forwarder gate on. Checked
	// before the dedup reservation so it stays a pure no-op.
	if jiraQuietSuppressed(bead.Fields) {
		a.logger.Debug("jira-completion-ack: suppressed on --quiet ticket",
			"agent", agent, "jira_key", jiraKey)
		return
	}

	// Dedup: one terminal note per agent bead, even if SSE redelivers the close
	// on reconnect.
	if a.alreadyAcked(bead.ID) {
		return
	}

	// Hard project pre-gate — mode-independent, exactly like the spawn-ack and
	// reply resolver. AllowReplyProject uses the spawn predicate (SyncToggle
	// ignored): a completion note is not an FSM/outbox write, so it must not be
	// coupled to the outbox toggle — GBFSM stays note-eligible in both toggle
	// states while FSM/unknown projects stay denied. A nil store fails closed.
	if a.writePolicy == nil || !a.writePolicy.AllowReplyProject(jiraKey) {
		a.logger.Warn("jira-completion-ack: blocked note on non-replyable project",
			"agent", agent, "jira_key", jiraKey, "project_key", jiraProjectKey(jiraKey))
		return
	}

	// Full comment verdict: project + role + internal visibility. Mirrors the
	// spawn-ack/reply path so a completion note is governed by the SAME per-role
	// allowlist as `gb jira reply`; a role with no comment grant (e.g.
	// fsm-triage) posts no note, and an unknown role fails closed.
	role := bead.Fields["role"]
	if !a.writePolicy.AllowReplyComment(jiraKey, role, CommentInternal) {
		a.logger.Warn("jira-completion-ack: blocked note by write policy",
			"agent", agent, "jira_key", jiraKey, "role", role)
		return
	}

	body := buildJiraCompletionBody(agent, bead.Fields)
	if err := a.jira.AddCommentWithVisibility(ctx, jiraKey, body, CommentInternal); err != nil {
		a.logger.Error("jira-completion-ack: failed to post completion note",
			"agent", agent, "jira_key", jiraKey, "error", err)
		return
	}
	a.logger.Info("jira-completion-ack: posted completion note",
		"agent", agent, "jira_key", jiraKey,
		"outcome", classifyJiraCompletion(bead.Fields), "role", role)
}

// alreadyAcked returns true if a terminal note was already posted for beadID
// within the dedup window, recording it (and pruning stale entries) otherwise.
func (a *JiraCompletionAcknowledger) alreadyAcked(beadID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	// Prune stale entries so the map stays bounded to the active window.
	for id, t := range a.seen {
		if now.Sub(t) > jiraCompletionSeenTTL {
			delete(a.seen, id)
		}
	}
	if t, ok := a.seen[beadID]; ok && now.Sub(t) < jiraCompletionSeenTTL {
		return true
	}
	a.seen[beadID] = now
	return false
}

// classifyJiraCompletion derives the terminal outcome for a closed mention-agent
// bead from its persisted fields. It mirrors the Slack bridge's crash detection
// (agents.go handleClosed) — a failed agent_state OR a failed pod_phase (unless
// a clean agent_state=done wins the race, and excluding benign reaper cleanups)
// is a crash — then defers to terminalCardState (the same helper the Slack card
// uses) for the done / rate_limited / reaped verdict. Returns one of "done",
// "failed", "rate_limited", "reaped".
func classifyJiraCompletion(fields map[string]string) string {
	agentState := fields["agent_state"]
	podPhase := fields["pod_phase"]
	closeReason := fields["close_reason"]
	if agentState != "done" && (agentState == "failed" || podPhase == "failed") && !isReaperCleanup(closeReason) {
		return "failed"
	}
	return terminalCardState(agentState, closeReason)
}

// buildJiraCompletionBody formats the terminal completion note. It leads with an
// outcome glyph + one-line status naming the agent (the Jira analog of the Slack
// card's ✅/❌), echoes the wrap-up tldr when present so the ticket carries the
// outcome summary and not just a glyph, and appends the operator attribution
// line every outbound path stamps. The [gardenio agent] prefix matches the
// spawn-ack/squawk bodies so all bot-authored comments read uniformly.
func buildJiraCompletionBody(agent string, fields map[string]string) string {
	if agent == "" {
		agent = "the agent"
	}
	var b strings.Builder
	switch classifyJiraCompletion(fields) {
	case "done":
		fmt.Fprintf(&b, "[gardenio agent] ✅ Done — %s finished work on this ticket.", agent)
	case "failed":
		fmt.Fprintf(&b, "[gardenio agent] ❌ Failed — %s stopped before completing.", agent)
	case "rate_limited":
		fmt.Fprintf(&b, "[gardenio agent] ⚠️ %s was rate-limited and stopped before completing.", agent)
	default: // reaped
		fmt.Fprintf(&b, "[gardenio agent] ⚠️ %s ended without wrapping up.", agent)
	}

	// Echo the wrap-up tldr (gb stop --wrapup) so the ticket carries the outcome
	// summary. Only the validated one-liner — the full wrap-up lives on the bead
	// and its lifecycle comment. A missing/filler tldr adds no line.
	if tldr := completionTLDR(fields["wrapup"]); tldr != "" {
		fmt.Fprintf(&b, "\n%s", tldr)
	}

	body := b.String()
	if len(body) > jiraCompletionAckMaxLen {
		body = body[:jiraCompletionAckMaxLen-3] + "..."
	}

	// Attribution appended AFTER truncation so the "Acted by <operator>" line is
	// never clipped (mirrors jira_spawn_ack.go / jira_squawk_forward.go). The SSE
	// close event carries only Fields — no daemon-resolved display name — so this
	// is the raw operated_by fallback, exactly like jira_sync.handleClosed.
	return withAttribution(body, operatorAttributionLine(fields["operated_by"], ""))
}

// completionTLDR extracts the validated one-line tldr from a wrap-up JSON
// payload, returning "" when the payload is empty, unparseable, or the tldr is a
// filler sentinel. Reuses the shared parseWrapUp decode + negative-sentinel
// filter so the ticket echo matches what the Slack card would feature.
func completionTLDR(wrapupJSON string) string {
	w, ok := parseWrapUp(wrapupJSON)
	if !ok || w == nil {
		return ""
	}
	tldr := strings.TrimSpace(w.TLDR)
	if wrapUpFieldEmpty(tldr) {
		return ""
	}
	return tldr
}
