package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/slack-go/slack"
)

// dmChannelCacheTTL controls how long a cached userID→DM channel mapping is
// considered valid before re-calling conversations.open. Slack DM channel IDs
// are stable, so a long TTL is safe.
const dmChannelCacheTTL = 30 * time.Minute

// openDMChannel resolves a DM channel ID for the given Slack user, using a
// TTL-based cache to avoid redundant conversations.open API calls.
func (b *Bot) openDMChannel(ctx context.Context, userID string) (string, error) {
	// Check cache first.
	b.mu.Lock()
	if b.dmChannelCache != nil {
		if entry, ok := b.dmChannelCache[userID]; ok && time.Since(entry.cachedAt) < dmChannelCacheTTL {
			b.mu.Unlock()
			return entry.channelID, nil
		}
	}
	b.mu.Unlock()

	// Cache miss — call conversations.open.
	ch, _, _, err := b.api.OpenConversationContext(ctx, &slack.OpenConversationParameters{
		Users:    []string{userID},
		ReturnIM: true,
	})
	if err != nil {
		return "", fmt.Errorf("open conversation with %s: %w", userID, err)
	}

	// Update cache.
	b.mu.Lock()
	if b.dmChannelCache == nil {
		b.dmChannelCache = make(map[string]dmChannelEntry)
	}
	b.dmChannelCache[userID] = dmChannelEntry{channelID: ch.ID, cachedAt: time.Now()}
	b.mu.Unlock()

	return ch.ID, nil
}

// dmChannelEntry is a cached DM channel mapping.
type dmChannelEntry struct {
	channelID string
	cachedAt  time.Time
}

// SendDM sends a direct message to a Slack user by their user ID.
// It opens (or reopens) a DM channel via conversations.open (with caching),
// then posts the message. Returns the channel ID and message timestamp.
func (b *Bot) SendDM(ctx context.Context, userID, text string) (channelID, ts string, err error) {
	if userID == "" {
		return "", "", fmt.Errorf("SendDM: empty user ID")
	}
	if text == "" {
		return "", "", fmt.Errorf("SendDM: empty message text")
	}

	dmCh, err := b.openDMChannel(ctx, userID)
	if err != nil {
		return "", "", fmt.Errorf("SendDM: %w", err)
	}

	respChannel, respTS, err := b.api.PostMessageContext(ctx, dmCh,
		slack.MsgOptionText(text, false),
	)
	if err != nil {
		return "", "", fmt.Errorf("SendDM: post message to %s: %w", userID, err)
	}

	b.logger.Info("sent DM",
		"user_id", userID, "channel", respChannel, "ts", respTS)

	return respChannel, respTS, nil
}

// ReplyInBeadThread posts text in the Slack thread of the message the bridge
// recorded for beadID (a decision or escalation card). The bool reports whether
// a tracked message was found; false means nothing was posted.
//
// The reply target is the card's PARENT thread when it has one, and the card
// itself otherwise. Slack has no nested threads: a card that was posted as a
// reply (under a Slack conversation the agent was spawned from, or under an
// agent status card) carries ThreadTS, and replying to that ts keeps the notice
// in the same visible conversation. A top-level card has no ThreadTS, so its
// own Timestamp opens the thread.
func (b *Bot) ReplyInBeadThread(ctx context.Context, beadID, text string) (bool, error) {
	if b == nil {
		return false, nil
	}
	ref, ok := b.lookupMessage(beadID)
	if !ok || ref.ChannelID == "" {
		return false, nil
	}
	threadTS := ref.ThreadTS
	if threadTS == "" {
		threadTS = ref.Timestamp
	}
	if threadTS == "" {
		return false, nil
	}
	if err := b.replyInThread(ctx, ref.ChannelID, threadTS, text); err != nil {
		return false, err
	}
	return true, nil
}

// replyInThread posts a text reply in a specific Slack thread.
func (b *Bot) replyInThread(ctx context.Context, channel, threadTS, text string) error {
	if b.api == nil {
		return fmt.Errorf("replyInThread: api client is nil")
	}
	_, _, err := b.api.PostMessageContext(ctx, channel,
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(threadTS),
	)
	return err
}

// openGroupConversation opens a group conversation (MPIM) with the given Slack
// user IDs via conversations.open and returns the resulting mpim channel ID.
//
// Opening an mpim requires the bot token to carry the mpim:write scope (added
// to the app manifest in Phase 0). Without it Slack returns missing_scope here
// — the single failure point shared by SendGroupDM and the groups/open
// endpoint, so both surface an identical, actionable error.
func (b *Bot) openGroupConversation(ctx context.Context, userIDs []string) (string, error) {
	if len(userIDs) == 0 {
		return "", fmt.Errorf("openGroupConversation: empty user IDs")
	}
	ch, _, _, err := b.api.OpenConversationContext(ctx, &slack.OpenConversationParameters{
		Users:    userIDs,
		ReturnIM: true,
	})
	if err != nil {
		return "", fmt.Errorf("open group conversation with %v: %w", userIDs, err)
	}
	return ch.ID, nil
}

// SendGroupDM sends a direct message to a group of Slack users (MPIM).
// It opens a group conversation via conversations.open with multiple users,
// then posts the message. Returns the channel ID and message timestamp.
func (b *Bot) SendGroupDM(ctx context.Context, userIDs []string, text string) (channelID, ts string, err error) {
	if len(userIDs) == 0 {
		return "", "", fmt.Errorf("SendGroupDM: empty user IDs")
	}
	if text == "" {
		return "", "", fmt.Errorf("SendGroupDM: empty message text")
	}

	// For single user, delegate to regular SendDM.
	if len(userIDs) == 1 {
		return b.SendDM(ctx, userIDs[0], text)
	}

	// Open a group conversation (MPIM) with all users.
	chID, err := b.openGroupConversation(ctx, userIDs)
	if err != nil {
		return "", "", err
	}

	respChannel, respTS, err := b.api.PostMessageContext(ctx, chID,
		slack.MsgOptionText(text, false),
	)
	if err != nil {
		return "", "", fmt.Errorf("SendGroupDM: post message to %v: %w", userIDs, err)
	}

	b.logger.Info("sent group DM",
		"user_ids", userIDs, "channel", respChannel, "ts", respTS)

	return respChannel, respTS, nil
}

// SendDMBlocks sends a direct message with Block Kit blocks to a Slack user.
// Falls back to the provided fallbackText for notifications and accessibility.
func (b *Bot) SendDMBlocks(ctx context.Context, userID, fallbackText string, blocks ...slack.Block) (channelID, ts string, err error) {
	if userID == "" {
		return "", "", fmt.Errorf("SendDMBlocks: empty user ID")
	}

	dmCh, err := b.openDMChannel(ctx, userID)
	if err != nil {
		return "", "", fmt.Errorf("SendDMBlocks: %w", err)
	}

	respChannel, respTS, err := b.api.PostMessageContext(ctx, dmCh,
		slack.MsgOptionText(fallbackText, false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		return "", "", fmt.Errorf("SendDMBlocks: post message to %s: %w", userID, err)
	}

	b.logger.Info("sent DM with blocks",
		"user_id", userID, "channel", respChannel, "ts", respTS)

	return respChannel, respTS, nil
}
