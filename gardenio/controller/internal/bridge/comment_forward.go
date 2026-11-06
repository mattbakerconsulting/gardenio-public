// Package bridge provides the comment forwarding handler.
//
// CommentForward watches the kbeads SSE event stream for beads.comment.added
// events and relays comments into the originating agent's thread through the
// conversation Router. This closes the gap where agent-written bead comments
// (e.g., audit reports) were silently dropped because no handler subscribed to
// the comment topic.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

// CommentForwardConfig holds configuration for the CommentForward handler.
type CommentForwardConfig struct {
	Daemon BeadClient
	Bot    *Bot
	Logger *slog.Logger
}

// CommentForward watches for bead comment events and relays them to Slack.
type CommentForward struct {
	daemon BeadClient
	bot    *Bot
	logger *slog.Logger

	mu   sync.Mutex
	seen map[int64]bool // dedup on SSE reconnect (keyed by comment ID)
}

// NewCommentForward creates a new comment forwarding handler.
func NewCommentForward(cfg CommentForwardConfig) *CommentForward {
	return &CommentForward{
		daemon: cfg.Daemon,
		bot:    cfg.Bot,
		logger: cfg.Logger,
		seen:   make(map[int64]bool),
	}
}

// RegisterHandlers registers the SSE event handler for comment events.
func (c *CommentForward) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.comment.added", c.handleCommentAdded)
	c.logger.Info("comment-forward registered SSE handlers",
		"topics", []string{"beads.comment.added"})
}

// sseCommentPayload mirrors the kbeads CommentAdded event structure.
type sseCommentPayload struct {
	Comment *sseComment `json:"comment"`
}

type sseComment struct {
	ID        int64     `json:"id"`
	BeadID    string    `json:"bead_id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	// OperatedBy is the server-resolved human operator of record for the
	// comment (e.g. "slack:U…" or "oidc:matt@acme.example"). It is set when a
	// real person — not an agent — left the comment. OperatedByDisplay /
	// OperatedByEmail are the operators-table-resolved name and email.
	OperatedBy        string `json:"operated_by,omitempty"`
	OperatedByDisplay string `json:"operated_by_display,omitempty"`
	OperatedByEmail   string `json:"operated_by_email,omitempty"`
}

func (c *CommentForward) handleCommentAdded(ctx context.Context, data []byte) {
	var payload sseCommentPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		c.logger.Debug("comment-forward: failed to parse comment event", "error", err)
		return
	}
	if payload.Comment == nil || payload.Comment.BeadID == "" {
		return
	}

	comment := payload.Comment

	// Dedup on SSE reconnect.
	if c.alreadySeen(comment.ID) {
		return
	}

	// Look up the parent bead to find the assignee (agent name).
	parent, err := c.daemon.GetBead(ctx, comment.BeadID)
	if err != nil {
		c.logger.Warn("comment-forward: failed to get parent bead",
			"bead_id", comment.BeadID, "error", err)
		return
	}

	// Resolve the agent name: prefer assignee, fall back to comment author.
	agent := extractAgentName(parent.Assignee)
	if agent == "" {
		agent = extractAgentName(comment.Author)
	}
	if agent == "" {
		c.logger.Debug("comment-forward: no agent for comment, skipping",
			"bead_id", comment.BeadID, "comment_id", comment.ID)
		return
	}

	// A trigger-fired run's wrap-up that changed nothing ("Preflight clean,
	// queued=0") is not relayed: a cicd watchdog every 20 min made it ~80
	// cards a day in a people-facing channel (gd-rar4hYRbGC). The wrap-up
	// stays on the agent bead and its comment.
	if quietScheduledWrapUp(parent, comment.Text) {
		c.logger.Debug("comment-forward: suppressed no-progress scheduled wrap-up",
			"bead_id", comment.BeadID, "comment_id", comment.ID, "agent", agent)
		return
	}

	// Verbosity seam (gb-ux epic ka-0000005059, plan §5 + operator scope-add
	// "beads comments and updates such should be suppressed as well"): forwarded
	// bead comments are agent-thread chatter — suppressed per-recipient in the
	// simple default, restored in vibe mode. This relay is operator-facing Slack
	// visibility only; agents receive comments via their own nudge path, so
	// dropping the Slack echo loses no agent signal. Reply-tracked DM
	// questions/reviews are a separate path (dm_delivery.go) and stay actionable.
	if c.bot == nil || !c.bot.vibeModeForAgent(ctx, agent) {
		c.logger.Debug("comment-forward: suppressed comment relay (simple mode)",
			"bead_id", comment.BeadID, "comment_id", comment.ID, "agent", agent)
		return
	}

	// When a real person left the comment, surface their name so the thread
	// message doesn't read as if the agent wrote it. Agent-authored comments
	// stay unattributed — printing the agent's own name on its own comment is
	// noise (the message already lands under the agent's card).
	commenter := c.humanCommenterName(comment)

	c.logger.Info("comment-forward: relaying comment to Slack",
		"bead_id", comment.BeadID,
		"comment_id", comment.ID,
		"agent", agent,
		"commenter", commenter,
		"text_length", len(comment.Text))

	if c.bot != nil {
		beadRef := c.bot.beadLink(comment.BeadID, fmt.Sprintf("`%s`", comment.BeadID))
		var formatted string
		if commenter != "" {
			// Human commenter: attribute the comment to the person by name.
			formatted = fmt.Sprintf(":speech_balloon: *%s* commented on %s:\n\n%s",
				commenter, beadRef, comment.Text)
		} else {
			// Agent comment: keep the original agent-attributed framing.
			agentLink := c.bot.agentThreadLink(agent)
			formatted = fmt.Sprintf(":memo: %s commented on %s:\n\n%s",
				agentLink, beadRef, comment.Text)
		}
		// The relayed comment becomes a message bead in the agent's thread,
		// projected through the conversation Router.
		c.bot.routeAgentThreadMessage(ctx, agent, c.bot.landOf(parent.Labels, agent), conversation.Message{
			Text:   formatted,
			Author: commentPrincipal(comment, agent),
		})
	}
}

// quietScheduledWrapUp reports whether text is the gb stop wrap-up comment of
// a trigger-fired run (spawn_source=schedule, or a trigger_id/schedule_id on
// the agent bead) that changed nothing: the wrap-up gb stop stored on the
// agent bead first says progress_score 0 and names no blocker. A wrap-up with
// no progress_score, or one that does not parse, is relayed as before.
func quietScheduledWrapUp(agent *beadsapi.BeadDetail, text string) bool {
	if agent == nil || agent.Type != "agent" || !strings.HasPrefix(text, "gb stop wrap-up:") {
		return false
	}
	f := agent.Fields
	if f["spawn_source"] != "schedule" && f["trigger_id"] == "" && f["schedule_id"] == "" {
		return false
	}
	var w struct {
		ProgressScore *int   `json:"progress_score"`
		Blockers      string `json:"blockers"`
	}
	if json.Unmarshal([]byte(f["wrapup"]), &w) != nil || w.ProgressScore == nil {
		return false
	}
	return *w.ProgressScore == 0 && strings.TrimSpace(w.Blockers) == ""
}

// humanCommenterName returns a display name when the comment was left by a real
// person, or "" when it was authored by an agent. The discriminator is the
// comment's Author (the audit actor): a human commenter's Author is a typed
// human-operator id ("slack:U…", "oidc:<email>") or a bare email, while an
// agent's Author is its bare agent name. (Agents carry a human OperatedBy — the
// spawning operator — so OperatedBy alone cannot tell the two apart.)
//
// The name is resolved best-effort: the read-time OperatedByDisplay when the
// event carries it, then the Slack real name for a "slack:U…" operator, then an
// email, then the cleaned-up Author. A non-human author yields "".
func (c *CommentForward) humanCommenterName(comment *sseComment) string {
	if comment == nil || !isHumanCommentAuthor(comment.Author) {
		return ""
	}
	if comment.OperatedByDisplay != "" {
		return comment.OperatedByDisplay
	}
	// Prefer the Slack real name when we have a Slack operator id and a bot to
	// resolve it through.
	if uid, ok := slackUserIDFromOperator(comment.OperatedBy); ok && c.bot != nil {
		if name := c.bot.resolveSlackUser(uid); name != "" {
			return name
		}
	}
	if comment.OperatedByEmail != "" {
		return comment.OperatedByEmail
	}
	// Fall back to the author with any typed-operator prefix stripped.
	return stripOperatorPrefix(comment.Author)
}

// commentPrincipal is the conversation author of a relayed comment: the
// person (oidc:<email> when the event resolves one) for a human comment, the
// agent otherwise.
func commentPrincipal(comment *sseComment, agent string) conversation.Principal {
	if comment == nil || !isHumanCommentAuthor(comment.Author) {
		return agentPrincipal(agent)
	}
	p := conversation.Principal{ID: comment.Author, Name: comment.OperatedByDisplay}
	switch {
	case strings.HasPrefix(comment.Author, "oidc:"):
	case comment.OperatedByEmail != "":
		p.ID = "oidc:" + comment.OperatedByEmail
	case strings.Contains(comment.Author, "@") && !strings.Contains(comment.Author, ":"):
		p.ID = "oidc:" + comment.Author
	}
	return p
}

// isHumanCommentAuthor reports whether a comment Author identifies a real
// person rather than an agent. Human authors are typed human-operator ids
// ("slack:…" / "oidc:…") or bare emails; agent authors are bare agent names
// that never contain "@" or a typed prefix.
func isHumanCommentAuthor(author string) bool {
	if author == "" {
		return false
	}
	if strings.HasPrefix(author, "slack:") || strings.HasPrefix(author, "oidc:") {
		return true
	}
	return strings.Contains(author, "@")
}

// slackUserIDFromOperator extracts the Slack user id from a "slack:U…"
// operator string, returning ("", false) for any other shape.
func slackUserIDFromOperator(operatedBy string) (string, bool) {
	const prefix = "slack:"
	if !strings.HasPrefix(operatedBy, prefix) {
		return "", false
	}
	uid := operatedBy[len(prefix):]
	if uid == "" {
		return "", false
	}
	return uid, true
}

// stripOperatorPrefix removes a leading "slack:" / "oidc:" operator prefix so a
// raw author renders as a plain id/email rather than an internal typed string.
func stripOperatorPrefix(author string) string {
	if kind, rest, ok := strings.Cut(author, ":"); ok {
		switch kind {
		case "slack", "oidc":
			return rest
		}
	}
	return author
}

// alreadySeen returns true if this comment has already been processed.
func (c *CommentForward) alreadySeen(commentID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen[commentID] {
		return true
	}
	c.seen[commentID] = true
	return false
}
