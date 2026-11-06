// Package bridge provides Canvas document creation for long-form content.
//
// When a decision report exceeds Slack's Block Kit text limits, we create a
// Canvas document with the full content and post a link instead of truncating.
// This preserves the full report in a searchable, shareable format.
package bridge

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/slack-go/slack"
)

// canvasContentThreshold is the content length above which we create a Canvas
// instead of inlining the text in a Slack message. Set below the Slack section
// text limit (3000) to provide breathing room for formatting overhead.
const canvasContentThreshold = 2000

// CanvasCreator wraps Slack's Canvas API for creating report documents.
type CanvasCreator struct {
	api    *slack.Client
	logger *slog.Logger
}

// NewCanvasCreator creates a new Canvas creator.
func NewCanvasCreator(api *slack.Client, logger *slog.Logger) *CanvasCreator {
	return &CanvasCreator{api: api, logger: logger}
}

// CreateReportCanvas creates a Canvas document with the given report content.
// Returns the canvas ID on success. The canvas content is formatted as markdown.
func (c *CanvasCreator) CreateReportCanvas(ctx context.Context, title, reportType, content string) (string, error) {
	// Build markdown content with a header.
	markdown := fmt.Sprintf("# %s\n\n**Type:** %s\n\n---\n\n%s", title, reportType, content)

	canvasID, err := c.api.CreateCanvasContext(ctx, title, slack.DocumentContent{
		Type:     "markdown",
		Markdown: markdown,
	})
	if err != nil {
		return "", fmt.Errorf("create canvas: %w", err)
	}

	c.logger.Info("canvas: created report canvas",
		"canvas_id", canvasID, "title", title, "report_type", reportType,
		"content_length", len(content))
	return canvasID, nil
}

// SetCanvasChannelAccess grants read access to the canvas for users in the
// given channel. This ensures thread participants can view the canvas.
func (c *CanvasCreator) SetCanvasChannelAccess(ctx context.Context, canvasID, channelID string) error {
	return c.api.SetCanvasAccessContext(ctx, slack.SetCanvasAccessParams{
		CanvasID:    canvasID,
		AccessLevel: "read",
		ChannelIDs:  []string{channelID},
	})
}

// canvasReportBlocks builds Block Kit blocks for a Canvas-backed report.
// Instead of inlining the full text, it shows a summary with a link.
func canvasReportBlocks(emoji, reportType, decisionTitle, canvasID, contentPreview string) []slack.Block {
	blocks := []slack.Block{
		slack.NewDividerBlock(),
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("%s *Report (%s)*", emoji, reportType), false, false),
			nil, nil),
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("Decision _%s_ · %s · :notebook: <https://slack.com/docs/%s|View full report in Canvas>",
					decisionTitle, reportType, canvasID), false, false)),
	}

	// Add a short preview of the content.
	if contentPreview != "" {
		preview := truncateText(contentPreview, 500)
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("```\n%s\n```", preview), false, false),
				nil, nil))
	}

	return blocks
}

// shouldUseCanvas returns true if the content is long enough to benefit
// from being stored in a Canvas document instead of inlined.
func shouldUseCanvas(content string) bool {
	return len(content) > canvasContentThreshold
}
