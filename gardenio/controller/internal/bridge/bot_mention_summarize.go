package bridge

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// summarizeMaxChars is the TLDR character budget for the in-thread summarize
// command. Matches the default `tldr` artifact convention.
const summarizeMaxChars = 500

// summarizeMaxThreadMsgs caps how many thread messages we pull for context.
const summarizeMaxThreadMsgs = 50

// summarizeMaxComments caps how many recent agent-bead comments we pull.
const summarizeMaxComments = 20

// summarizeBeadIDPattern matches beads IDs like "ka-0000004973" so we can
// surface them as links in the TLDR.
var summarizeBeadIDPattern = regexp.MustCompile(`\b(?:gd|ka)-[A-Za-z0-9]{8,}\b`)

// summarizeMRURLPattern matches GitLab MR URLs.
var summarizeMRURLPattern = regexp.MustCompile(`https?://[^\s<>]+/-/merge_requests/\d+`)

// summaryInputs is the payload that buildSummaryTLDR works on. Kept as a
// pure-data struct so the formatter is unit-testable without spinning up
// Slack or the daemon.
type summaryInputs struct {
	Agent    string             // bound agent short name, "" if no binding
	BeadID   string             // tracking bead ID for the agent, "" if none
	Status   string             // agent state ("running", "done", etc), "" if unknown
	Messages []summaryMessage   // recent thread messages (chronological)
	Comments []beadsapi.Comment // recent agent-bead comments (chronological)
}

// summaryMessage is the trimmed-down representation of a Slack message used
// when building the TLDR. We keep it minimal so tests don't need to construct
// full slack.Message values.
type summaryMessage struct {
	Author string
	Text   string
	BotID  string
}

// handleMentionSummarize posts a short TLDR of the current thread back into
// the thread when "@gardenio summarize" is mentioned. Works synchronously
// (no agent spawn) and bounded to ~500 chars.
func (b *Bot) handleMentionSummarize(ctx context.Context, ev *slackevents.AppMentionEvent) {
	channel := ev.Channel
	threadTS := ev.ThreadTimeStamp

	inputs := summaryInputs{}

	// Pull thread messages from Slack.
	if b.api != nil {
		msgs, _, _, err := b.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadTS,
			Limit:     summarizeMaxThreadMsgs,
		})
		if err != nil {
			b.logger.Warn("summarize: failed to fetch thread messages",
				"channel", channel, "thread_ts", threadTS, "error", err)
		} else {
			for _, m := range msgs {
				inputs.Messages = append(inputs.Messages, summaryMessage{
					Author: m.User,
					Text:   m.Text,
					BotID:  m.BotID,
				})
			}
		}
	}

	// Resolve thread→agent if any. Carries on if no agent is bound.
	if agent := b.getAgentByThread(channel, threadTS); agent != "" {
		agentName := extractAgentName(agent)
		inputs.Agent = agentName

		if b.daemon != nil {
			if bead, err := b.daemon.FindAgentBead(ctx, agentName); err == nil && bead != nil {
				inputs.BeadID = bead.ID
				if bead.Fields != nil {
					inputs.Status = bead.Fields["agent_state"]
				}

				// Pull recent comments from the agent's tracking bead.
				if comments, err := b.daemon.GetComments(ctx, bead.ID); err == nil {
					inputs.Comments = trimComments(comments, summarizeMaxComments)
				} else {
					b.logger.Debug("summarize: failed to fetch comments",
						"bead", bead.ID, "error", err)
				}
			}
		}
	}

	tldr := buildSummaryTLDR(inputs, summarizeMaxChars)

	if b.api != nil {
		_, _, _ = b.api.PostMessage(channel,
			slack.MsgOptionText(tldr, false),
			slack.MsgOptionTS(threadTS),
		)
	}

	b.logger.Info("summarize: posted thread TLDR",
		"channel", channel, "thread_ts", threadTS,
		"agent", inputs.Agent, "bead", inputs.BeadID,
		"msgs", len(inputs.Messages), "comments", len(inputs.Comments),
		"chars", len(tldr))
}

// trimComments returns the most recent `n` comments in chronological order.
func trimComments(comments []beadsapi.Comment, n int) []beadsapi.Comment {
	// Sort by CreatedAt ascending so the most recent are at the end.
	sorted := append([]beadsapi.Comment(nil), comments...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})
	if len(sorted) <= n {
		return sorted
	}
	return sorted[len(sorted)-n:]
}

// buildSummaryTLDR formats the collected inputs as a ≤maxChars Slack message.
// Pure function — exposed for unit tests.
//
// Output shape:
//
//	:memo: *Thread TLDR* — agent: <name> (status)
//	• <recent activity bullets>
//	Links: <bead ids> | <MR urls>
//
// When no agent is bound, the header omits the agent line.
func buildSummaryTLDR(in summaryInputs, maxChars int) string {
	var header string
	if in.Agent != "" {
		statusSuffix := ""
		if in.Status != "" {
			statusSuffix = fmt.Sprintf(" (%s)", in.Status)
		}
		header = fmt.Sprintf(":memo: *Thread TLDR* — agent: *%s*%s", in.Agent, statusSuffix)
	} else {
		header = ":memo: *Thread TLDR* — no agent bound"
	}

	bullets := summarizeBullets(in)
	links := summarizeLinks(in)

	// Assemble and clamp. Leave room for a truncation marker.
	parts := []string{header}
	if len(bullets) > 0 {
		parts = append(parts, strings.Join(bullets, "\n"))
	} else {
		parts = append(parts, "_(no recent activity in this thread)_")
	}
	if links != "" {
		parts = append(parts, links)
	}

	out := strings.Join(parts, "\n")
	return clampTLDR(out, maxChars)
}

// summarizeBullets condenses thread messages and bead comments into short
// bullet lines, in chronological order, dropping bot/system noise. Bead IDs
// and MR URLs are stripped from bullet text since they're surfaced separately
// in the trailing Links: line.
func summarizeBullets(in summaryInputs) []string {
	var bullets []string

	for _, m := range in.Messages {
		// Skip bot/system messages — they're noise for a human TLDR.
		if m.BotID != "" {
			continue
		}
		text := compactText(stripLinkTokens(m.Text))
		if text == "" {
			continue
		}
		who := m.Author
		if who == "" {
			who = "?"
		}
		bullets = append(bullets, fmt.Sprintf("• <@%s>: %s", who, text))
	}

	for _, c := range in.Comments {
		text := compactText(stripLinkTokens(c.Text))
		if text == "" {
			continue
		}
		who := c.OperatedByDisplay
		if who == "" {
			who = c.Author
		}
		if who == "" {
			who = "agent"
		}
		bullets = append(bullets, fmt.Sprintf("• %s (bead): %s", who, text))
	}

	return bullets
}

// stripLinkTokens removes bead IDs and MR URLs from text so they're not
// duplicated between bullet lines and the Links: footer.
func stripLinkTokens(s string) string {
	s = summarizeBeadIDPattern.ReplaceAllString(s, "")
	s = summarizeMRURLPattern.ReplaceAllString(s, "")
	return s
}

// summarizeLinks pulls bead IDs and MR URLs out of the thread + comments
// for the trailing "Links:" line.
func summarizeLinks(in summaryInputs) string {
	seen := map[string]bool{}
	var beads, mrs []string

	collect := func(text string) {
		for _, id := range summarizeBeadIDPattern.FindAllString(text, -1) {
			if !seen[id] {
				seen[id] = true
				beads = append(beads, id)
			}
		}
		for _, url := range summarizeMRURLPattern.FindAllString(text, -1) {
			if !seen[url] {
				seen[url] = true
				mrs = append(mrs, url)
			}
		}
	}

	for _, m := range in.Messages {
		collect(m.Text)
	}
	for _, c := range in.Comments {
		collect(c.Text)
	}
	if in.BeadID != "" && !seen[in.BeadID] {
		seen[in.BeadID] = true
		beads = append([]string{in.BeadID}, beads...)
	}

	var parts []string
	if len(beads) > 0 {
		parts = append(parts, "beads: "+strings.Join(beads, ", "))
	}
	if len(mrs) > 0 {
		parts = append(parts, "MRs: "+strings.Join(mrs, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Links: " + strings.Join(parts, " | ")
}

// compactText strips Slack mention tokens, collapses whitespace, and trims to
// a short fragment so a single message doesn't dominate the TLDR.
func compactText(s string) string {
	// Drop the "@gardenio summarize" trigger noise from the first user message
	// without trying to be clever — just remove standalone "summarize" tokens
	// that follow a bot mention.
	s = strings.ReplaceAll(s, "<!channel>", "")
	s = strings.ReplaceAll(s, "<!here>", "")
	s = strings.Join(strings.Fields(s), " ")
	const maxFragment = 120
	if len(s) > maxFragment {
		s = s[:maxFragment-1] + "…"
	}
	return s
}

// clampTLDR enforces the ≤maxChars budget. When truncation is needed we cut
// on a rune boundary and append a single-character ellipsis so the result
// stays valid UTF-8.
func clampTLDR(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}
	if maxChars <= 1 {
		return s[:maxChars]
	}
	cut := maxChars - 1
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
