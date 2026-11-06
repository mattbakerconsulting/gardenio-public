package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/slack-go/slack"
)

// conversationSeam holds the Bot's conversation Router and the Slack surface
// under it (design spec 2026-10-19-lands-commons-design.md, section 3). The
// slice-1 leaf handlers (squawk, comment and chat-close relays, thread message
// forward, death report, crash notice) post through it; the rest of the bridge
// still calls Slack directly until its slice moves. The slice-2 agent cards
// post and edit through it too (agent_cards.go).
type conversationSeam struct {
	mu     sync.Mutex
	api    *slack.Client // the client the surface was built over
	slack  *slackSurface
	router *conversation.Router

	// dispatcher runs every Inbound (inbound_reply.go); claims holds the
	// Commons message beads being, or recently, dispatched, each with when
	// its claim lapses.
	dispatcher *conversation.Dispatcher
	claims     map[string]time.Time

	// cards memoizes each agent's card bead, and noCard when an agent was
	// last found to have none (agent_cards.go).
	cards  map[string]agentCardMemo
	noCard map[string]time.Time
}

// Conversation returns the Bot's conversation Router, building it over the
// Bot's Slack client and daemon on first use (and again when a test swaps
// b.api). The Router's only surface is Slack until the Commons lands.
func (b *Bot) Conversation() *conversation.Router {
	b.conv.mu.Lock()
	defer b.conv.mu.Unlock()
	if b.conv.router == nil || b.conv.api != b.api {
		s := newSlackSurface(b.api)
		s.openDM = b.openDMChannel
		s.render = renderSlackFace
		var store conversation.Store
		if b.daemon != nil {
			store = b.daemon
		}
		b.conv.api, b.conv.slack = b.api, s
		b.conv.router = conversation.NewRouter(store, b.logger, s)
	}
	return b.conv.router
}

// conversationSlack returns the Slack surface under Conversation(), for the
// Slack-only edits that have no message bead (the spawn status line).
func (b *Bot) conversationSlack() *slackSurface {
	b.Conversation()
	b.conv.mu.Lock()
	defer b.conv.mu.Unlock()
	return b.conv.slack
}

// gardenio_conversation_bead_write_failed_total counts messages routeSlackIn
// posted to Slack alone because their message bead could not be written, so
// the Commons never shows them. The kind label is the message kind
// (conversation.Kind: text, card, notice, ask), a closed set.
var conversationBeadWriteFailedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_conversation_bead_write_failed_total",
		Help: "Messages the slack-bridge posted to Slack only because the message bead write failed (missing from the Commons), by message kind.",
	},
	[]string{"kind"},
)

// routeToSlack routes m into a Slack channel of land, as a reply in threadTS
// when it is set (see routeSlackMessage). A Bot without a daemon has nowhere
// to write the bead, so it posts to Slack directly.
func (b *Bot) routeToSlack(ctx context.Context, land, channel, threadTS string, m conversation.Message) error {
	if b.daemon == nil {
		land = ""
	}
	_, err := routeSlackMessage(ctx, b.Conversation(), b.logger, land, channel, threadTS, m)
	return err
}

// routeSlackMessage writes m as a message bead in land and projects it into
// the Slack channel (a reply in threadTS when set) through r. A message with
// a BeadID already has its bead (an agent or the viewer wrote it), so only the
// projection runs and the Slack ref is stamped back on that bead.
//
// The channel has no channel bead here, so Slack is its only surface and the
// post is exactly today's (routeSlackIn takes the channel bead). Degrade,
// never drop: with no land (a fleet-wide notice, or a bead with no project
// label) there is no bead to write, and when the bead write fails the message
// still posts, both straight to the Slack surface.
func routeSlackMessage(ctx context.Context, r *conversation.Router, logger *slog.Logger, land, channel, threadTS string, m conversation.Message) (conversation.Ref, error) {
	return routeSlackIn(ctx, r, logger, conversation.ChannelRef{Land: land, Slack: channel}, "", threadTS, m)
}

// routeSlackIn is routeSlackMessage into ch: the Slack channel ch.Slack and,
// when ch.ID is set, the channel bead bound to it, so the bead carries its
// channel_id and the Commons shows it. head is the thread's head bead when
// threadTS is set and the head is known; the bead then carries its thread_id.
func routeSlackIn(ctx context.Context, r *conversation.Router, logger *slog.Logger, ch conversation.ChannelRef, head, threadTS string, m conversation.Message) (conversation.Ref, error) {
	s, ok := r.Surface(conversation.SurfaceSlack)
	if !ok {
		return conversation.Ref{}, fmt.Errorf("conversation: no slack surface: %w", conversation.ErrUnsupported)
	}
	land, channel := ch.Land, ch.Slack
	root := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: channel, TS: threadTS}
	if land != "" {
		var d conversation.Delivery
		var err error
		if threadTS != "" {
			d, err = r.Reply(ctx, ch, conversation.Thread{Head: head, Refs: []conversation.Ref{root}}, m, false)
		} else {
			d, err = r.Post(ctx, ch, m)
		}
		if err == nil {
			// A copy that landed counts even if storing its ref failed (the
			// router logged that); only a missing copy is an error.
			if ref, ok := d.Ref(conversation.SurfaceSlack); ok {
				return ref, nil
			}
			if ferr := d.Failed[conversation.SurfaceSlack]; ferr != nil {
				return conversation.Ref{}, ferr
			}
			return conversation.Ref{}, fmt.Errorf("conversation: %s not projected to slack: %w", channel, conversation.ErrNoChannel)
		}
		if errors.Is(err, conversation.ErrEmptyMessage) {
			return conversation.Ref{}, err
		}
		// Loud on purpose: the Slack copy still lands, so nothing else shows
		// the Commons lost the message. Death and crash notices went
		// Slack-only for days behind a Warn no one read (gd-JGmXQuSndB).
		conversationBeadWriteFailedTotal.WithLabelValues(string(m.KindOrText())).Inc()
		if logger != nil {
			logger.Error("conversation: message bead write failed, posting to Slack directly",
				"land", land, "channel", channel, "thread_ts", threadTS, "kind", m.KindOrText(), "error", err)
		}
	}
	if threadTS != "" {
		return s.Reply(ctx, root, m, false)
	}
	return s.Post(ctx, conversation.ChannelRef{Land: land, Slack: channel}, m)
}

// commonsChannel returns the channel of land bound to the Slack channel
// slackChannel: its channel bead when the land has one (none when the land has
// no Commons yet, or on a failed read), always projecting to slackChannel. The
// bridge's own routing chose that Slack channel, so a copy goes there whatever
// the channel bead's mirror says (unset reads as off), until placement
// (conversation.Place) takes that routing over.
func (b *Bot) commonsChannel(ctx context.Context, land, slackChannel string) conversation.ChannelRef {
	ch := conversation.ChannelRef{Land: land, Slack: slackChannel}
	if b.daemon == nil || land == "" || slackChannel == "" {
		return ch
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"channel"},
		Labels: []string{"project:" + land},
		Fields: map[string]string{conversation.ChannelSlackID: slackChannel},
		Limit:  1,
	})
	if err != nil {
		b.logger.Debug("conversation: channel bead lookup failed", "land", land, "channel", slackChannel, "error", err)
		return ch
	}
	if len(res.Beads) > 0 && res.Beads[0] != nil {
		ch = conversation.ChannelFromBead(res.Beads[0], land)
		ch.Slack, ch.Mirror = slackChannel, conversation.MirrorOut
	}
	return ch
}

// machinesChannel is commonsChannel for a top-level machine post (an agent
// card, a death or crash notice, an escalation card no person is asked to
// answer): land's #machines channel bead when the land has one, so the Commons
// keeps it out of #general (single voice, steward amendment 2026-10-23 §2).
// The Slack copy still goes to slackChannel, exactly as before: #machines is
// never mirrored, but the bridge's own Slack routing is not the mirror. A
// land with no #machines, or a failed read, gets commonsChannel.
func (b *Bot) machinesChannel(ctx context.Context, land, slackChannel string) conversation.ChannelRef {
	if b.daemon != nil && land != "" && slackChannel != "" {
		mc, err := conversation.MachinesChannel(ctx, b.daemon, land)
		if err != nil {
			b.logger.Debug("conversation: #machines lookup failed", "land", land, "error", err)
		}
		if mc != nil {
			ch := conversation.ChannelFromBead(mc, land)
			ch.Slack, ch.Mirror = slackChannel, conversation.MirrorOut
			return ch
		}
	}
	return b.commonsChannel(ctx, land, slackChannel)
}

// landOf returns the land (project) a conversation bead about agent belongs
// to: the project label on the subject bead, else the agent's cached project.
func (b *Bot) landOf(labels []string, agent string) string {
	if land := projectFromLabels(labels); land != "" {
		return land
	}
	agent = extractAgentName(agent)
	if agent == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.agentProject[agent]
}

// agentEventLand is landOf for an agent bead event, which may carry its
// project as a field rather than a label.
func (b *Bot) agentEventLand(bead BeadEvent, agent string) string {
	if land := projectFromLabels(bead.Labels); land != "" {
		return land
	}
	if land := bead.Fields["project"]; land != "" {
		return land
	}
	return b.landOf(nil, agent)
}

// agentPrincipal is the conversation author for an agent.
func agentPrincipal(agent string) conversation.Principal {
	name := extractAgentName(agent)
	if name == "" {
		return conversation.Principal{}
	}
	return conversation.Principal{ID: "agent:" + name, Name: name}
}
