// Package bridge provides the trigger-notice publisher and watcher.
//
// Trigger-notice decouples trigger actions from direct Slack API calls.
// Producers (trigger-engine, gitlab-bridge, etc.) emit a notice via a
// closed message bead with the "trigger-notice" label. slack-bridge picks
// it up via SSE and posts to Slack using its own bot credentials — so
// trigger-running services don't need their own Slack token.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"

	slackapi "github.com/slack-go/slack"
)

// NoticeLabel marks a message bead as a trigger notice for slack-bridge.
const NoticeLabel = "trigger-notice"

// NoticeChannelField, if set on a notice bead, overrides the slack-bridge
// default channel for that notice. Empty falls back to the watcher's
// configured default.
//
// This MUST be a field the daemon's type:message schema declares, or the
// create that sets the override is rejected 400 "<field>: unknown field"
// before the bead is ever closed — so the Notice watcher never sees it and
// the override silently no-ops. `channel` is the schema-declared name
// (kbeads config.go type:message, guarded by
// TestBuiltinMessageProducerFieldsDeclared); `slack_channel` is NOT declared
// and is actively rejected ("did you mean slack_thread_channel?"). Reading
// the rejected name is what left agents with no path to post a top-level
// message to an arbitrary channel (ka-0000002479): an agent can now create a
// closed trigger-notice message bead with `channel=<id>` and `text=<msg>`,
// and slack-bridge posts it channel-root on the bot's chat:write.
const NoticeChannelField = "channel"

// noticeProjectKey carries a notice's land on the ctx handed to a
// TriggerNotifier (see WithNoticeProject).
type noticeProjectKey struct{}

// WithNoticeProject returns ctx tagged with the land a notice published under
// it belongs to. NewNoticeNotifier labels the notice bead project:<project> so
// it lands in that land rather than in none. Carried on the ctx, not as a
// TriggerNotifier argument, so the many fleet-wide watchdog callers that have
// no single land need no change and keep publishing unlabelled. An empty
// project is a no-op.
func WithNoticeProject(ctx context.Context, project string) context.Context {
	project = strings.TrimSpace(project)
	if project == "" {
		return ctx
	}
	return context.WithValue(ctx, noticeProjectKey{}, project)
}

// noticeProjectFrom returns the land WithNoticeProject stamped on ctx, or "".
func noticeProjectFrom(ctx context.Context) string {
	p, _ := ctx.Value(noticeProjectKey{}).(string)
	return p
}

// noticeBeadProject is the land of the one bead a watchdog notice is about
// (a trigger or agent bead): its project field, else its project label, else
// "". Nil-safe for a watched id that no longer resolves.
func noticeBeadProject(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	if p := strings.TrimSpace(b.Fields["project"]); p != "" {
		return p
	}
	for _, l := range b.Labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// NoticePublisherClient is the subset of beadsapi.Client used to publish
// trigger notices. Mirrored as an interface so the trigger-engine can wire
// in any compatible daemon client (and tests can fake it).
type NoticePublisherClient interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

// NewNoticeNotifier returns a TriggerNotifier that publishes notices as
// closed message beads. slack-bridge's Notice watcher picks them up and
// posts to Slack — keeping the Slack credential surface in one place.
//
// source is the producer identity (e.g. "trigger-engine") recorded on the
// bead's source_agent field and used as the bead actor.
func NewNoticeNotifier(daemon NoticePublisherClient, source string, logger *slog.Logger) TriggerNotifier {
	if daemon == nil {
		return nil
	}
	return func(ctx context.Context, triggerTitle, message, channel string) {
		if message == "" {
			return
		}
		title := message
		if len(title) > 500 {
			title = title[:497] + "..."
		}
		noticeFields := map[string]string{
			"source_agent":  source,
			"text":          message,
			"trigger_title": triggerTitle,
		}
		// A non-empty channel overrides slack-bridge's default channel for this
		// notice; empty leaves NoticeChannelField unset so the Notice watcher
		// falls through to its configured default (ka-0000008204).
		if channel != "" {
			noticeFields[NoticeChannelField] = channel
		}
		fields, err := json.Marshal(noticeFields)
		if err != nil {
			if logger != nil {
				logger.Warn("notice publish: marshal fields", "error", err)
			}
			return
		}
		labels := []string{NoticeLabel, fmt.Sprintf("from:%s", source)}
		// Exactly one project label, from the caller's land. Notices route by
		// the Slack `channel` field and never set fields.channel_id, so kbeads'
		// channel/land rule (channel_validation.go) cannot reject the label.
		if project := noticeProjectFrom(ctx); project != "" {
			labels = append(labels, "project:"+project)
		}
		id, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:     title,
			Type:      "message",
			Kind:      "data",
			Labels:    labels,
			Fields:    json.RawMessage(fields),
			CreatedBy: source,
		})
		if err != nil {
			if logger != nil {
				logger.Warn("notice publish: create bead", "error", err, "source", source)
			}
			return
		}
		if err := daemon.CloseBead(ctx, id, nil); err != nil && logger != nil {
			logger.Warn("notice publish: close bead", "error", err, "bead", id)
		}
	}
}

// NoticeSlackAPI is the subset of slack-go used by the Notice watcher.
// Matches the shape of slack-go's Client.PostMessageContext.
type NoticeSlackAPI interface {
	PostMessageContext(ctx context.Context, channelID string, opts ...slackapi.MsgOption) (string, string, error)
}

// NoticeConfig configures the Notice watcher.
type NoticeConfig struct {
	API            NoticeSlackAPI
	DefaultChannel string
	Logger         *slog.Logger
}

// Notice watches for closed trigger-notice message beads and posts them to
// Slack on slack-bridge's bot credentials.
type Notice struct {
	api            NoticeSlackAPI
	defaultChannel string
	logger         *slog.Logger

	mu   sync.Mutex
	seen map[string]bool
}

// NewNotice creates a Notice watcher. Returns nil if api is nil — the
// watcher is a no-op without a Slack client.
func NewNotice(cfg NoticeConfig) *Notice {
	if cfg.API == nil {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Notice{
		api:            cfg.API,
		defaultChannel: cfg.DefaultChannel,
		logger:         logger,
		seen:           make(map[string]bool),
	}
}

// RegisterHandlers wires the watcher onto an SSE stream.
func (n *Notice) RegisterHandlers(stream *SSEStream) {
	if n == nil {
		return
	}
	stream.On("beads.bead.closed", n.handleClosed)
	n.logger.Info("notice watcher registered SSE handlers",
		"default_channel", n.defaultChannel)
}

func (n *Notice) handleClosed(ctx context.Context, data []byte) {
	if n == nil {
		return
	}
	bead := ParseBeadEvent(data)
	if bead == nil {
		return
	}
	if bead.Type != "message" || !hasLabel(bead.Labels, NoticeLabel) {
		return
	}
	if n.alreadySeen(bead.ID) {
		return
	}

	text := bead.Fields["text"]
	if text == "" {
		text = bead.Title
	}
	if text == "" {
		n.logger.Warn("notice bead has no text", "id", bead.ID)
		return
	}

	channel := bead.Fields[NoticeChannelField]
	if channel == "" {
		channel = n.defaultChannel
	}
	if channel == "" {
		n.logger.Warn("notice bead has no channel and no default configured",
			"id", bead.ID)
		return
	}

	if _, _, err := n.api.PostMessageContext(ctx, channel,
		slackapi.MsgOptionText(text, false)); err != nil {
		n.logger.Warn("notice post failed",
			"id", bead.ID, "channel", channel, "error", err)
		return
	}
	n.logger.Info("notice posted",
		"id", bead.ID, "channel", channel, "text_length", len(text))
}

func (n *Notice) alreadySeen(beadID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen[beadID] {
		return true
	}
	n.seen[beadID] = true
	return false
}
