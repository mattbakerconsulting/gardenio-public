// Package bridge provides the thread-message forwarder.
//
// ThreadMessageForward closes the viewer composer's bridge-mediated send loop
// (Phase 2 task ka-0000005643). The beads-viewer does NOT hold a Slack bot
// token — only slack-bridge does. So when an operator types a message into a
// wish thread in the /threads view, the viewer does not post to Slack itself;
// instead it writes a closed type=message bead carrying the wish head's
// slack_thread_channel/slack_thread_ts and the `thread-say` label, and THIS
// watcher (running inside slack-bridge, which already holds the token and the
// shared slackCardLimiter) relays it into the Slack thread.
//
// This mirrors the trigger-notice pattern (notice.go) and the squawk pattern
// (squawk.go) — a producer emits a closed message bead, slack-bridge picks it
// up via SSE and posts on its own credentials, keeping the Slack credential
// surface in exactly one binary. It differs from notice.go in one load-bearing
// way: the message is routed INTO a thread (slack.MsgOptionTS(threadTS)), with
// the (channel, threadTS) binding read off the message bead's OWN fields. The
// existing squawk/comment forwarders resolve the thread via an *agent* bead, so
// they cannot deliver a message into an agent-less wish head's thread — hence
// this dedicated, head-field-routed forwarder.
//
// The post is gated through the shared slackCardLimiter (the same Tier-3 token
// bucket squawk/agents posts use) so a flood of composer messages cannot push
// the bot past Slack's rate limits.
//
// The message bead already exists (the viewer wrote it), so the post goes
// through the conversation Router as a projection of that bead, which stamps
// the Slack ref back on it (design spec 2026-10-19-lands-commons-design.md,
// slice 1). This is the Commons write path in miniature.
package bridge

import (
	"context"
	"log/slog"
	"sync"

	"golang.org/x/time/rate"

	"gardenio/controller/internal/conversation"
)

// ThreadSayLabel marks a closed type=message bead as a viewer-composer message
// to be relayed into a Slack thread. It is deliberately distinct from the
// squawk watcher's "squawk"/"say" labels (squawk routes via an agent and posts
// without a thread ts) and from notice.go's "trigger-notice" (posts to a
// channel root, no thread ts).
const ThreadSayLabel = "thread-say"

// ThreadMessageForwardConfig configures the ThreadMessageForward watcher.
type ThreadMessageForwardConfig struct {
	// Router projects the message into Slack (slack-bridge's bot
	// credentials): the Bot's Conversation(), whose Slack surface is the only
	// code that posts.
	Router *conversation.Router
	// Limiter, when non-nil, gates every post through the shared
	// slackCardLimiter so composer traffic obeys the same Tier-3 token bucket
	// as the rest of the bridge's thread posts. Nil disables rate limiting
	// (test convenience).
	Limiter *rate.Limiter
	// Bot, when non-nil, is dispatched AFTER a successful Slack post to give a
	// viewer thread reply the same agent outcome an @gardenio mention would —
	// spawn / respawn / nudge (ka-0000005407). Nil = forward-only (the message
	// still relays into Slack, but no agent is spawned), preserving the pre-fix
	// behavior for tests and no-Bot deployments.
	Bot *Bot
	// Files, when non-nil, fetches a Commons reply's attachments so they are
	// uploaded into the thread (commons_files.go), as the Commons mirror's are:
	// a farm postcard shows in Slack as its picture (gd-WxRuyfdUtR). Nil posts
	// them as viewer links.
	Files *CommonsFileFetcher
	// Logger is the structured logger; defaults to slog.Default() when nil.
	Logger *slog.Logger
}

// ThreadMessageForward watches for closed `thread-say` message beads and posts
// them into the bound Slack thread on slack-bridge's bot credentials.
type ThreadMessageForward struct {
	router  *conversation.Router
	limiter *rate.Limiter
	bot     *Bot
	files   *CommonsFileFetcher
	logger  *slog.Logger

	mu   sync.Mutex
	seen map[string]bool // dedup on SSE reconnect (keyed by bead ID)
}

// NewThreadMessageForward creates a ThreadMessageForward watcher. Returns nil
// if the router is nil — the watcher is a no-op without a Slack client (parity
// with NewNotice).
func NewThreadMessageForward(cfg ThreadMessageForwardConfig) *ThreadMessageForward {
	if cfg.Router == nil {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ThreadMessageForward{
		router:  cfg.Router,
		limiter: cfg.Limiter,
		bot:     cfg.Bot,
		files:   cfg.Files,
		logger:  logger,
		seen:    make(map[string]bool),
	}
}

// RegisterHandlers wires the watcher onto an SSE stream.
func (t *ThreadMessageForward) RegisterHandlers(stream *SSEStream) {
	if t == nil {
		return
	}
	stream.On("beads.bead.closed", t.handleClosed)
	stream.On("beads.bead.created", t.handleCreated)
	t.logger.Info("thread-message-forward registered SSE handlers",
		"topics", []string{"beads.bead.closed", "beads.bead.created"}, "label", ThreadSayLabel)
}

// handleCreated forwards a message bead created already closed: the Commons
// pane writes its replies that way (a person may create a message but not
// close one, kbeads' action table), so no close event follows. The /threads
// composer creates open and then closes, and is handled on the close.
func (t *ThreadMessageForward) handleCreated(ctx context.Context, data []byte) {
	if t == nil {
		return
	}
	if bead := ParseBeadEvent(data); bead != nil && bead.Status == "closed" {
		t.forward(ctx, bead)
	}
}

func (t *ThreadMessageForward) handleClosed(ctx context.Context, data []byte) {
	if t == nil {
		return
	}
	if bead := ParseBeadEvent(data); bead != nil {
		t.forward(ctx, bead)
	}
}

func (t *ThreadMessageForward) forward(ctx context.Context, bead *BeadEvent) {
	// Only handle viewer-composer message beads. The label is unique to this
	// forwarder, so no other closed-handler (squawk/notice/beadActivity/agents)
	// double-posts the same bead.
	if bead.Type != "message" || !hasLabel(bead.Labels, ThreadSayLabel) {
		return
	}
	if t.alreadySeen(bead.ID) {
		return
	}

	// A Commons pane reply shows in Slack as its author's, as every mirrored
	// Commons message does (commons_persona.go): under their name and avatar,
	// else "*Name* (via Commons)", with its files as viewer links under its
	// words. A reply that is only files has no words: its title ("Shared
	// a.png") only names them. The /threads composer's carries no origin and
	// posts as before.
	fromCommons := bead.Fields[conversation.BeadOrigin] == conversation.SurfaceCommons
	var files []fileRef
	if fromCommons {
		// A farm snapshot's view link goes after its files, as the Commons
		// mirror posts them.
		files = withSnapLink(decodeFileRefs(bead.Fields[conversation.BeadFiles]), bead.Fields[conversation.BeadSnap])
	}
	text := bead.Fields["text"]
	if text == "" && len(files) == 0 {
		text = bead.Title
	}
	if text == "" && len(files) == 0 {
		t.logger.Warn("thread-message-forward: bead has no text", "id", bead.ID)
		return
	}

	// The (channel, threadTS) binding is copied onto the message bead at
	// compose time from the wish head's slack_thread_channel/slack_thread_ts
	// (thread_bead.go:110-111). A message bead with an invalid/missing binding
	// is dropped with a warn rather than guessed at — there is no channel-root
	// fallback for a thread message (unlike notice.go's default channel).
	channel := bead.Fields["slack_thread_channel"]
	threadTS := bead.Fields["slack_thread_ts"]
	if !isValidThreadBinding(channel, threadTS) {
		t.logger.Warn("thread-message-forward: invalid thread binding, dropping",
			"id", bead.ID, "channel", channel, "thread_ts", threadTS)
		return
	}

	land := t.land(ctx, bead)
	var as *conversation.Persona
	var up []conversation.File
	if fromCommons {
		// The files it can fetch are uploaded with the reply. Slack posts an
		// upload as the bot itself, so such a reply is attributed in its text
		// and the rest of its files are links under its words, as
		// CommonsMirror.post does.
		var linked []fileRef
		if up, linked = t.files.Fetch(ctx, land, bead.Fields[conversation.BeadChannelID], files); len(up) > 0 {
			who, _ := commonsMessageFromEvent(bead).slackAuthor()
			name, _ := t.bot.commonsAuthor(ctx, who)
			text = viaCommonsWithFiles(name, text, linked)
		} else {
			text, as = t.bot.commonsSlackText(ctx, commonsMessageFromEvent(bead), text, files)
		}
	}

	// Gate through the shared slackCardLimiter (same Tier-3 token bucket the
	// rest of the bridge's thread posts use) so composer traffic cannot push
	// the bot past Slack's rate limits.
	if t.limiter != nil {
		if err := t.limiter.Wait(ctx); err != nil {
			t.logger.Warn("thread-message-forward: rate limiter wait failed, dropping",
				"id", bead.ID, "error", err)
			return
		}
	}

	// Project the bead into the thread: the Slack surface's Reply is the
	// same PostMessage + MsgOptionTS(threadTS) postThreadStateReply
	// (bot_agents.go) and handlePostReply (bot_threads.go) use.
	if _, err := routeSlackMessage(ctx, t.router, t.logger, land, channel, threadTS, conversation.Message{
		BeadID: bead.ID,
		Text:   text,
		As:     as,
		Files:  up,
	}); err != nil {
		t.logger.Warn("thread-message-forward: post failed",
			"id", bead.ID, "channel", channel, "thread_ts", threadTS, "error", err)
		return
	}
	t.logger.Info("thread-message-forward: posted composer message to thread",
		"id", bead.ID, "channel", channel, "thread_ts", threadTS, "text_length", len(text), "uploaded", len(up))

	// Give the viewer reply the same agent outcome an @gardenio mention would:
	// spawn a fresh thread agent, respawn a dead one with session resume, or
	// nudge a live one (ka-0000005407). Gated on the successful post above so we
	// never spawn an agent for a message the operator never saw land in Slack.
	// The bot's OWN repost is dropped by handleMessageEvent's botUserID guard, so
	// this bead-close dispatch is the only spawn trigger — no echo loop. Nil bot
	// (forward-only deployments / tests) skips this, leaving relay-only behavior.
	// It goes through the dispatcher as a Commons Inbound{Reply}, so a composer
	// that also calls POST /api/conversation/inbound still wakes the agent once.
	// A Commons mention is the inbound call's alone, which checks the person's
	// land role (inbound_compose.go), and so is a Commons pane reply's spawn:
	// dispatched from here it only nudges a live agent (gd-Vc9dC61oKP).
	if t.bot != nil && !t.bot.commonsComposes(ctx, commonsMessageFromEvent(bead)) {
		_, _, _ = t.bot.dispatchCommonsMessage(ctx, commonsMessageFromEvent(bead))
	}
}

// land returns the composer message's land: its own project label, else the
// project label of the wish head its thread_id points at (the viewer stamps
// only thread_id; thread_bead.go labels every head with its project). ""
// posts straight to the Slack surface, as before.
func (t *ThreadMessageForward) land(ctx context.Context, bead *BeadEvent) string {
	if land := projectFromLabels(bead.Labels); land != "" {
		return land
	}
	head := bead.Fields["thread_id"]
	if head == "" || t.bot == nil || t.bot.daemon == nil {
		return ""
	}
	detail, err := t.bot.daemon.GetBead(ctx, head)
	if err != nil || detail == nil {
		return ""
	}
	return projectFromLabels(detail.Labels)
}

// alreadySeen returns true if this bead has already been processed (dedup on
// SSE reconnect), recording it as seen otherwise.
func (t *ThreadMessageForward) alreadySeen(beadID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen[beadID] {
		return true
	}
	t.seen[beadID] = true
	return false
}
