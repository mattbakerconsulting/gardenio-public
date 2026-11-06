package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// conciergeDebounceWindow is the minimum interval between concierge prompts
// for the same user in the same channel. Prevents button spam on rapid/multi-line pastes.
const conciergeDebounceWindow = 2 * time.Second

// conciergeDebouncer tracks the last prompt time per user+channel to prevent
// duplicate prompts on rapid successive messages.
type conciergeDebouncer struct {
	mu   sync.Mutex
	last map[string]time.Time // key: "user:channel"
}

func newConciergeDebouncer() *conciergeDebouncer {
	return &conciergeDebouncer{last: make(map[string]time.Time)}
}

// Allow returns true if enough time has passed since the last prompt for this
// user+channel pair. If allowed, it records the current time.
func (d *conciergeDebouncer) Allow(userID, channelID string) bool {
	key := userID + ":" + channelID
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if last, ok := d.last[key]; ok && now.Sub(last) < conciergeDebounceWindow {
		return false
	}
	d.last[key] = now
	return true
}

// Cleanup removes entries older than the debounce window to prevent memory leaks.
// Called periodically by the bot's Run loop.
func (d *conciergeDebouncer) Cleanup() {
	cutoff := time.Now().Add(-conciergeDebounceWindow * 2)
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range d.last {
		if v.Before(cutoff) {
			delete(d.last, k)
		}
	}
}

// conciergeChannelInfo checks if a channel is in concierge mode for any project.
// Returns the project name and true if the channel is in concierge mode.
// Uses the project channel cache (30s TTL) to avoid HTTP calls on every message.
func (b *Bot) conciergeChannelInfo(ctx context.Context, channelID string) (string, bool) {
	b.mu.Lock()
	if b.conciergeChannelCache != nil && time.Since(b.conciergeChannelCacheAt) < projectChannelCacheTTL {
		project, ok := b.conciergeChannelCache[channelID]
		b.mu.Unlock()
		return project, ok
	}
	b.mu.Unlock()

	projects, err := b.daemon.ListProjectBeads(ctx)
	if err != nil {
		b.logger.Error("concierge: failed to list projects", "error", err)
		return "", false
	}

	cache := make(map[string]string) // channelID → project name
	seen := make(map[string]string)  // channelID → first project name (conflict detection)
	for name, info := range projects {
		for _, ch := range info.SlackChannels {
			if info.ChannelMode(ch) == "concierge" {
				if prev, dup := seen[ch]; dup {
					b.logger.Error("multiple projects claim the same Slack channel in concierge mode — skipping",
						"channel", ch, "projects", []string{prev, name})
					delete(cache, ch) // remove the first mapping too
					continue
				}
				seen[ch] = name
				cache[ch] = name
			}
		}
	}

	b.mu.Lock()
	b.conciergeChannelCache = cache
	b.conciergeChannelCacheAt = time.Now()
	b.mu.Unlock()

	project, ok := cache[channelID]
	return project, ok
}

// handleConciergeMessage processes a top-level channel message in concierge mode.
// It posts a thread reply with Start/Dismiss buttons under the original message.
func (b *Bot) handleConciergeMessage(ctx context.Context, ev *slackevents.MessageEvent, project string) {
	// Encode context into button values: project|channel|message_ts|user_id
	value := fmt.Sprintf("%s|%s|%s|%s", project, ev.Channel, ev.TimeStamp, ev.User)

	blocks := []slack.Block{
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", ":robot_face: Want an agent to help with this?", false, false),
		),
		slack.NewActionBlock("concierge_actions",
			slack.NewButtonBlockElement("concierge_start", value,
				slack.NewTextBlockObject("plain_text", "Start", false, false),
			).WithStyle(slack.StylePrimary),
			slack.NewButtonBlockElement("concierge_dismiss", value,
				slack.NewTextBlockObject("plain_text", "Dismiss", false, false),
			),
		),
	}

	_, _, err := b.api.PostMessageContext(ctx, ev.Channel,
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionTS(ev.TimeStamp), // reply in thread under the original message
	)
	if err != nil {
		b.logger.Error("concierge: failed to post buttons", "channel", ev.Channel, "error", err)
	}
}

// handleConciergeStart handles the Start button click in a concierge prompt.
// It spawns a thread-bound agent using the original message as the prompt.
func (b *Bot) handleConciergeStart(ctx context.Context, value string, callback slack.InteractionCallback) {
	project, channel, messageTS, userID, err := parseConciergeValue(value)
	if err != nil {
		b.logger.Error("concierge start: invalid button value", "value", value, "error", err)
		return
	}

	// Guard against double-click: check if thread is already bound to an agent.
	spawnKey := channel + ":" + messageTS
	b.mu.Lock()
	if b.spawnInFlight[spawnKey] {
		b.mu.Unlock()
		return
	}
	b.spawnInFlight[spawnKey] = true
	b.mu.Unlock()
	defer func() {
		time.AfterFunc(10*time.Second, func() {
			b.mu.Lock()
			delete(b.spawnInFlight, spawnKey)
			b.mu.Unlock()
		})
	}()

	// Update the button message to show "Spawning..." status.
	statusBlocks := []slack.Block{
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", ":hourglass_flowing_sand: Spawning agent...", false, false),
		),
	}
	_, _, _, _ = b.api.UpdateMessageContext(ctx,
		callback.Channel.ID, callback.Message.Timestamp,
		slack.MsgOptionBlocks(statusBlocks...),
	)

	// Fetch the original message text to use as the agent's prompt.
	// The text isn't available in the interaction callback, so we retrieve it.
	prompt := b.fetchMessageText(ctx, channel, messageTS)
	if prompt == "" {
		prompt = "(no message text)"
	}

	// Spawn the agent using the existing spawn machinery.
	agentName := b.generateAgentName(project)
	beadID, spawnErr := b.daemon.SpawnAgent(ctx, agentName, project, "", "thread", prompt)
	if spawnErr != nil {
		b.logger.Error("concierge start: spawn failed", "project", project, "error", spawnErr)
		errBlocks := []slack.Block{
			slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn", ":x: Failed to spawn agent", false, false),
			),
		}
		_, _, _, _ = b.api.UpdateMessageContext(ctx,
			callback.Channel.ID, callback.Message.Timestamp,
			slack.MsgOptionBlocks(errBlocks...),
		)
		return
	}

	// Set thread-spawn fields on the agent bead. operated_by is recorded
	// alongside slack_user_id so the originating Slack operator is
	// discoverable on the bead even after audit-row dibs handoffs
	// (ka-0000001582).
	conciergeFields := map[string]string{
		"slack_thread_channel": channel,
		"slack_thread_ts":      messageTS,
		"spawn_source":         "concierge",
		"slack_user_id":        userID,
		"mode":                 "job",
	}
	if userID != "" {
		conciergeFields["operated_by"] = b.operatorFromSlackUser(ctx, userID)
	}
	_ = b.daemon.UpdateBeadFields(ctx, beadID, conciergeFields)

	// Bind the thread to the agent in state.
	if b.state != nil {
		_ = b.state.SetThreadAgent(channel, messageTS, agentName)
		_ = b.state.SetListenThread(channel, messageTS) // auto-enable --listen
	}

	// Update button message to show the agent is running.
	runningBlocks := []slack.Block{
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf(":white_check_mark: Agent *%s* is working on this", agentName),
				false, false),
		),
	}
	_, _, _, _ = b.api.UpdateMessageContext(ctx,
		callback.Channel.ID, callback.Message.Timestamp,
		slack.MsgOptionBlocks(runningBlocks...),
	)

	b.logger.Info("concierge: agent spawned",
		"agent", agentName, "project", project,
		"channel", channel, "thread_ts", messageTS)
}

// handleConciergeDismiss handles the Dismiss button click in a concierge prompt.
// It removes the button message from the thread.
func (b *Bot) handleConciergeDismiss(ctx context.Context, callback slack.InteractionCallback) {
	// Try to delete the message. If we can't (permissions), update to collapsed text.
	_, _, err := b.api.DeleteMessageContext(ctx, callback.Channel.ID, callback.Message.Timestamp)
	if err != nil {
		// Fallback: update to a minimal dismissed message.
		dismissedBlocks := []slack.Block{
			slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn", "_Dismissed_", false, false),
			),
		}
		_, _, _, _ = b.api.UpdateMessageContext(ctx,
			callback.Channel.ID, callback.Message.Timestamp,
			slack.MsgOptionBlocks(dismissedBlocks...),
		)
	}
}

// fetchMessageText retrieves the text of a message by channel and timestamp.
// Reuses the same conversations.replies pattern as fetchMessageFiles.
func (b *Bot) fetchMessageText(ctx context.Context, channel, messageTS string) string {
	msgs, _, _, err := b.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: messageTS,
		Limit:     1,
		Inclusive: true,
	})
	if err != nil || len(msgs) == 0 {
		b.logger.Warn("concierge: failed to fetch original message", "channel", channel, "ts", messageTS, "error", err)
		return ""
	}
	return msgs[0].Text
}

// generateAgentName creates a unique agent name for a concierge-spawned agent.
func (b *Bot) generateAgentName(project string) string {
	ts := time.Now().UnixMilli()
	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	return fmt.Sprintf("concierge-%s-%d-%s", project, ts, hex.EncodeToString(suffix[:]))
}

// parseConciergeValue parses the encoded button value: "project|channel|message_ts|user_id".
func parseConciergeValue(value string) (project, channel, messageTS, userID string, err error) {
	parts := strings.SplitN(value, "|", 4)
	if len(parts) != 4 {
		return "", "", "", "", fmt.Errorf("expected 4 parts, got %d", len(parts))
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}
