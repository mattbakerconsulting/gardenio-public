package bridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// SLANudgeNotifier delivers rung-2 ("repeat on the operator channel") and
// rung-3 ("overdue") SLA nudges to Slack. *Bot satisfies it via
// NotifySLANudge; a narrow interface keeps SLANudgeSweep testable without the
// whole EscalationNotifier surface (mirrors StaleDigestNotifier).
type SLANudgeNotifier interface {
	NotifySLANudge(ctx context.Context, bead BeadEvent, rung SLARung, age time.Duration) error
}

// NotifySLANudge posts a single rung-2/rung-3 SLA alert to the operator queue
// channel (falling back to the default channel), for either an escalation or
// a decision bead. Rung 1 needs no direct call here: it stamps fields that
// existing SSE watchers already react to (Escalations' pingback_requested_at
// handler, Decisions' "escalated"-label handler) — see sla_nudge_sweep.go.
func (b *Bot) NotifySLANudge(ctx context.Context, bead BeadEvent, rung SLARung, age time.Duration) error {
	targetChannel := b.escalationQueueChannel
	if targetChannel == "" {
		targetChannel = b.channel
	}
	if targetChannel == "" {
		b.logger.Warn("no queue or default channel for SLA nudge alert", "bead", bead.ID, "rung", rung.String())
		return nil
	}

	var headline string
	switch rung {
	case SLARung3:
		headline = ":rotating_light: *SLA Breached*"
	default:
		headline = ":alarm_clock: *SLA Nudge — operator action needed*"
	}
	if b.escalationQueueMention != "" {
		headline += fmt.Sprintf(" <%s>", b.escalationQueueMention)
	}

	text := headline + fmt.Sprintf("\n*%s*", b.beadTitleLink(bead.ID, bead.Title))

	var detailParts []string
	detailParts = append(detailParts, fmt.Sprintf("Type: `%s`", bead.Type))
	detailParts = append(detailParts, fmt.Sprintf("Priority: `P%d`", bead.Priority))
	detailParts = append(detailParts, fmt.Sprintf("Age: %s", formatStallDuration(age)))
	if task := bead.Fields["requesting_task"]; task != "" {
		detailParts = append(detailParts, fmt.Sprintf("Blocks: %s", b.beadLink(task, fmt.Sprintf("`%s`", task))))
	}
	text += "\n" + strings.Join(detailParts, " | ")
	if rung == SLARung3 {
		text += "\n_This item has been awaiting operator action past its full SLA window._"
	} else {
		text += "\n_This item has been awaiting operator action past its SLA rung-2 threshold._"
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
	}
	if reviewURL := b.escalationReviewURL(bead.ID); reviewURL != "" {
		reviewBtn := slack.NewButtonBlockElement("sla_nudge_open_ui", bead.ID,
			slack.NewTextBlockObject("plain_text", "Review in Web UI", false, false))
		reviewBtn.URL = reviewURL
		reviewBtn.Style = slack.StyleDanger
		blocks = append(blocks, slack.NewActionBlock("", reviewBtn))
	}

	fallbackText := fmt.Sprintf("SLA nudge (%s): %s", rung.String(), bead.Title)
	_, _, err := b.api.PostMessageContext(ctx, targetChannel,
		slack.MsgOptionText(fallbackText, false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		return fmt.Errorf("post SLA nudge alert to Slack: %w", err)
	}

	b.logger.Info("posted SLA nudge alert",
		"bead", bead.ID, "type", bead.Type, "rung", rung.String(), "channel", targetChannel)
	return nil
}
