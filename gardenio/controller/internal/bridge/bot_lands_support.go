package bridge

import (
	"context"
	"log/slog"
	"strings"

	slackapi "github.com/slack-go/slack"
)

// LandsSupport builds the tenant #support watcher (lands_support.go) bound to
// this bot: DMs on the bot's credentials, the operator resolved to a Slack id
// the way escalation DMs resolve theirs (the operators alias table, then a
// confirmed account link, then a user bead), and pointers to pointerChannel
// (the bot's default channel when empty).
//
// A nil bot still returns a watcher: support beads are filed without Slack,
// so a tenant's request is recorded even when the operator cannot be told.
func (b *Bot) LandsSupport(daemon BeadClient, operator, pointerChannel string, logger *slog.Logger) *LandsSupport {
	cfg := LandsSupportConfig{Daemon: daemon, Operator: operator, Logger: logger}
	if b == nil {
		return NewLandsSupport(cfg)
	}
	if logger == nil {
		logger = slog.Default()
	}
	index := newOperatorSlackIndex(daemon, logger)
	users := b.UserDirectory()
	cfg.Sender = b
	cfg.LinkBead = b.beadTitleLink
	cfg.ResolveSlackID = func(ctx context.Context, operator string) string {
		op := index.Resolve(ctx, operator, nil, "")
		if op.SlackID != "" || op.Email == "" {
			return op.SlackID
		}
		if l := b.slackLinker(); l != nil {
			if ids := l.SlackIDsForEmail(op.Email); len(ids) == 1 && slackUserIDShape.MatchString(ids[0]) {
				return ids[0]
			}
		}
		if users != nil {
			ids := map[string]bool{}
			for _, u := range users.All(ctx) {
				if normalizeEmail(u.Email) == op.Email && slackUserIDShape.MatchString(strings.TrimSpace(u.SlackID)) {
					ids[strings.TrimSpace(u.SlackID)] = true
				}
			}
			if id, ok := soleSlackID(ids); ok {
				return id
			}
		}
		return ""
	}
	if pointerChannel == "" {
		pointerChannel = b.DefaultChannel()
	}
	if pointerChannel != "" && b.API() != nil {
		cfg.PostPointer = func(ctx context.Context, text string) error {
			_, _, err := b.API().PostMessageContext(ctx, pointerChannel, slackapi.MsgOptionText(text, false))
			return err
		}
	}
	return NewLandsSupport(cfg)
}
