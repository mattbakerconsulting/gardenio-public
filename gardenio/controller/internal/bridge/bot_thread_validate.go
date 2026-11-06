package bridge

import (
	"context"
	"errors"
	"net/http"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// threadValidationInterval is how often thread bindings are validated.
const threadValidationInterval = 6 * time.Hour

// startPeriodicThreadValidation runs ValidateThreadBindings at a fixed interval
// until ctx is cancelled. Invalid bindings (deleted threads, archived channels)
// are removed from state to prevent unbounded growth.
func (b *Bot) startPeriodicThreadValidation(ctx context.Context) {
	ticker := time.NewTicker(threadValidationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.validateThreadBindings(ctx)
		}
	}
}

// validateThreadBindings checks all thread-agent mappings and removes entries
// whose thread no longer exists: a Slack thread against the Slack API (channel
// archived, thread deleted, or an API error indicating inaccessibility), a
// Commons thread with no Slack copy against its head bead (gone, or closed).
func (b *Bot) validateThreadBindings(ctx context.Context) {
	if b.state == nil {
		return
	}

	bindings := b.state.AllThreadBindings()
	if len(bindings) == 0 {
		return
	}

	b.logger.Info("thread-validate: starting scan", "bindings", len(bindings))

	removed := 0
	failed := 0
	for _, tb := range bindings {
		var err error
		switch {
		case tb.Channel != "" && tb.ThreadTS != "":
			if b.api == nil || b.isThreadAccessible(ctx, tb.Channel, tb.ThreadTS) {
				continue
			}
			// Remove the stale binding and its listen flag.
			if err = b.state.RemoveThreadAgent(tb.Channel, tb.ThreadTS); err == nil {
				_ = b.state.RemoveListenThread(tb.Channel, tb.ThreadTS)
			}
		case tb.Head != "":
			if b.isHeadOpen(ctx, tb.Head) {
				continue
			}
			err = b.state.RemoveThreadAgentByHead(tb.Head)
		default:
			continue
		}
		if err != nil {
			b.logger.Warn("thread-validate: failed to remove stale binding",
				"head", tb.Head, "channel", tb.Channel, "thread_ts", tb.ThreadTS, "error", err)
			failed++
			continue
		}
		removed++
	}

	if removed > 0 || failed > 0 {
		b.logger.Info("thread-validate: scan complete",
			"checked", len(bindings), "removed", removed, "errors", failed)
	} else {
		b.logger.Info("thread-validate: all bindings valid", "checked", len(bindings))
	}
}

// isHeadOpen reports whether a Commons thread's head still stands: it exists
// and is not closed. Any read failure but a not-found keeps the binding.
func (b *Bot) isHeadOpen(ctx context.Context, head string) bool {
	if b.daemon == nil {
		return true
	}
	bead, err := b.daemon.GetBead(ctx, head)
	if err != nil {
		var apiErr *beadsapi.APIError
		return !(errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound)
	}
	return bead != nil && bead.Status != "closed"
}

// parseThreadAgentKey splits a "channel:thread_ts" key into its components.
func parseThreadAgentKey(key string) (channel, threadTS string) {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			return key[:i], key[i+1:]
		}
	}
	return "", ""
}

// isThreadAccessible checks if a Slack thread is still accessible by fetching
// exactly one reply. Returns false if the channel is archived, the thread is
// deleted, or the API returns channel_not_found / thread_not_found.
func (b *Bot) isThreadAccessible(ctx context.Context, channel, threadTS string) bool {
	_, _, _, err := b.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: threadTS,
		Limit:     1,
		Inclusive: true,
	})
	if err == nil {
		return true
	}

	// Slack API errors that indicate the thread/channel is gone.
	errStr := err.Error()
	switch errStr {
	case "channel_not_found", "thread_not_found", "is_archived",
		"not_in_channel", "missing_scope":
		return false
	}

	// Treat other errors (rate limit, network) as "accessible" to avoid
	// false-positive removals. We'll catch them on the next cycle.
	b.logger.Debug("thread-validate: API error treated as accessible",
		"channel", channel, "thread_ts", threadTS, "error", err)
	return true
}
