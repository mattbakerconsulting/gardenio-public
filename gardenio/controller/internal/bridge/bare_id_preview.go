package bridge

// bare_id_preview.go implements the optional Phase 2 (stretch) surface of the
// Slack link-unfurl feature: previewing a *bare* ka-XXX id typed in a human
// message.
//
// Bare tokens never produce a link_shared event (Slack does not auto-link
// them), so — unlike the pasted-URL path in link_unfurl.go — they cannot be
// unfurled. Instead we scan the text of incoming human message.* events for
// ka-XXX ids and post a compact preview card as a threaded reply, reusing the
// Phase 1 card builder (buildUnfurlAttachment).
//
// The entire behavior is gated behind bareIDPreviewEnabled (env
// SLACK_BARE_ID_PREVIEW_ENABLED, default OFF) because it is inherently noisier
// than the URL path: every bead-id mention would otherwise draw a bot reply.
// Guards: human messages only (skip the bot's own and other apps' posts);
// ignore ids already inside a URL or Slack mrkdwn span (so a pasted viewer link
// handled by Phase 1 is not double-carded); per-message dedupe; a
// per-(channel,bead,window) throttle; and skip unknown/closed ids (no card).
// See the design doc (bead ka-0000004532, Phase 2 section) for the rationale.

import (
	"context"
	"regexp"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// bareBeadIDPattern matches a bare kbeads id in free text. It is the
// non-anchored sibling of beadIDPattern (^gd-[A-Za-z0-9]+$): the \b word
// boundaries keep it from matching mid-token while still finding ids surrounded
// by punctuation or whitespace. It intentionally mirrors the ka-XXX rule the
// linkify transport already uses (buildLinkRules in slack_linkify.go).
var bareBeadIDPattern = regexp.MustCompile(`\b(?:gd|ka)-[A-Za-z0-9]+\b`)

// maxBarePreviewIDs caps how many distinct bare ids one message may preview, so
// a message pasted with many ids can't fan out into unbounded fetches / cards.
const maxBarePreviewIDs = 10

// bareIDPreviewWindow is the per-(channel,bead) throttle window: once a bead is
// previewed in a channel, further mentions of the same id in that channel are
// skipped for this long, so a channel discussing one bead repeatedly is not
// spammed with duplicate cards.
const bareIDPreviewWindow = 10 * time.Minute

// bareIDPreviewTimeout bounds the detached preview goroutine (daemon fetch +
// chat.postMessage). The parent message handler's 30s context is canceled when
// it returns, so the goroutine runs on its own budget.
const bareIDPreviewTimeout = 15 * time.Second

// bareIDPreviewThrottle rate-limits bare-id preview cards per (channel, bead).
// A nil throttle allows everything (test convenience).
type bareIDPreviewThrottle struct {
	mu     sync.Mutex
	last   map[string]time.Time
	window time.Duration
}

func newBareIDPreviewThrottle(window time.Duration) *bareIDPreviewThrottle {
	return &bareIDPreviewThrottle{last: make(map[string]time.Time), window: window}
}

// allow reports whether a preview card for (channel, id) may post at time now.
// When it returns true it records now as the last-post time so a repeat within
// the window is suppressed. Only call it for ids that actually produce a card,
// so unknown/closed mentions never burn a throttle slot.
func (t *bareIDPreviewThrottle) allow(channel, id string, now time.Time) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := channel + "\x00" + id
	if last, ok := t.last[key]; ok && now.Sub(last) < t.window {
		return false
	}
	t.last[key] = now
	return true
}

// scanBareBeadIDs returns the distinct bare ka-XXX ids in text, in first-seen
// order, ignoring any id that falls inside an existing Slack mrkdwn span
// (<...>) or a bare http(s) URL. This span-avoidance (shared with the linkify
// transport, see nextProtectedSpan) is what keeps a pasted viewer link — already
// handled by the Phase 1 URL unfurler — from being double-carded here, and it
// de-dupes repeated ids within the one message. The result is capped at
// maxBarePreviewIDs.
func scanBareBeadIDs(text string) []string {
	if text == "" {
		return nil
	}
	var ids []string
	seen := make(map[string]bool)
	s := text
	for len(s) > 0 {
		start, length := nextProtectedSpan(s)
		var chunk string
		if start < 0 {
			chunk, s = s, ""
		} else {
			chunk, s = s[:start], s[start+length:]
		}
		for _, m := range bareBeadIDPattern.FindAllString(chunk, -1) {
			if seen[m] {
				continue
			}
			seen[m] = true
			ids = append(ids, m)
			if len(ids) >= maxBarePreviewIDs {
				return ids
			}
		}
	}
	return ids
}

// maybePreviewBareBeadIDs is the gate called from handleMessageEvent. When the
// toggle is off (default) it is a single bool check and returns immediately —
// zero behavior change. When on, it runs the scan + fetch + post in a tracked
// background goroutine so the preview's daemon round-trip and chat.postMessage
// never delay the message's existing routing (thread nudges, mentions,
// forwards). The parent handler's context is canceled when it returns, so the
// goroutine detaches with WithoutCancel (preserving the operator stamp) under a
// fresh timeout, and joins handlerWg so graceful shutdown waits for it.
func (b *Bot) maybePreviewBareBeadIDs(ctx context.Context, ev *slackevents.MessageEvent) {
	if b == nil || !b.bareIDPreviewEnabled {
		return
	}
	previewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bareIDPreviewTimeout)
	b.handlerWg.Add(1)
	go func() {
		defer b.handlerWg.Done()
		defer cancel()
		b.postBareBeadPreview(previewCtx, ev)
	}()
}

// postBareBeadPreview scans a human message for bare ka-XXX ids and posts a
// compact preview card per resolvable, non-closed bead as a threaded reply,
// reusing the Phase 1 card builder. Synchronous; run off the routing path by
// maybePreviewBareBeadIDs. Never consumes the event or alters existing routing.
func (b *Bot) postBareBeadPreview(ctx context.Context, ev *slackevents.MessageEvent) {
	atts := b.bareBeadPreviewAttachments(ctx, ev)
	if len(atts) == 0 || b.api == nil {
		return
	}
	// Threaded reply: under the existing thread if there is one, otherwise as
	// the first reply below the top-level message the id was typed in.
	threadTS := ev.ThreadTimeStamp
	if threadTS == "" {
		threadTS = ev.TimeStamp
	}
	if _, _, err := b.api.PostMessageContext(ctx, ev.Channel,
		slack.MsgOptionTS(threadTS),
		slack.MsgOptionAttachments(atts...),
	); err != nil {
		b.logger.Warn("bare-id preview post failed",
			"channel", ev.Channel, "thread_ts", threadTS, "cards", len(atts), "error", err)
		return
	}
	b.logger.Debug("posted bare-id preview",
		"channel", ev.Channel, "thread_ts", threadTS, "cards", len(atts))
}

// bareBeadPreviewAttachments is the testable seam of maybePreviewBareBeadIDs:
// it applies every guard and returns the preview cards to post (nil when the
// toggle is off, the message is not a previewable human message, or no id
// resolves to an open bead). Extracted from the poster so the decision logic —
// toggle, human-only/IM guards, span-avoiding scan, fetch, unknown/closed skip,
// and throttle — is unit-testable without a live *slack.Client. Mirrors the
// buildUnfurls/handleLinkShared split in link_unfurl.go.
func (b *Bot) bareBeadPreviewAttachments(ctx context.Context, ev *slackevents.MessageEvent) []slack.Attachment {
	if b == nil || ev == nil || !b.bareIDPreviewEnabled {
		return nil
	}
	// Human messages only. handleMessageEvent already dropped the bot's own
	// posts and empty-user events, but guard again (this method is unit tested
	// directly) and additionally skip *other* apps/bots so the bridge never
	// previews ids inside another integration's — or its own — rich cards.
	if ev.User == "" || ev.User == b.botUserID || ev.BotID != "" {
		return nil
	}
	// Skip 1:1 DMs with the bot: a bare id typed there is a command context
	// (it becomes a synthesized @mention routed to an agent), not a channel
	// discussion, so a threaded preview card would just be noise.
	if ev.ChannelType == "im" {
		return nil
	}

	ids := scanBareBeadIDs(ev.Text)
	if len(ids) == 0 {
		return nil
	}

	beads := b.batchGetBeads(ctx, ids)
	if len(beads) == 0 {
		return nil
	}

	th := b.themeFor(ctx, ev.Channel)
	now := time.Now()
	var atts []slack.Attachment
	for _, id := range ids {
		bead := beads[id]
		// Skip unknown (unresolved) and closed ids gracefully — no card.
		if bead == nil || bead.Status == "closed" {
			continue
		}
		// Per-(channel,bead,window) throttle: consumed only for ids that
		// actually produce a card, so unknown/closed mentions never burn a slot.
		if !b.bareIDPreview.allow(ev.Channel, id, now) {
			continue
		}
		atts = append(atts, b.buildUnfurlAttachment(th, bead, b.beadViewerLink(id)))
	}
	return atts
}

// beadViewerLink returns the canonical beads-viewer deep link for id (base +
// "/" + id), used as the bare-id card's title link. Unlike the Phase 1 path
// there is no pasted URL to link back to, so we point at the viewer's own deep
// link. Falls back to the bare id when the viewer URL is unset (dev/test) so
// buildUnfurlAttachment still has a non-empty label target.
func (b *Bot) beadViewerLink(id string) string {
	if b == nil || b.beadsViewerURL == "" {
		return id
	}
	return b.beadsViewerURL + "/" + id
}
