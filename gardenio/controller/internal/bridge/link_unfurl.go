package bridge

// link_unfurl.go implements Slack link unfurling for gardenio viewer URLs.
//
// When a *human* pastes a gardenio link (e.g.
// https://gardenio.example.com/beads/ka-XXX) into a channel or thread reply,
// Slack delivers a link_shared event for our registered unfurl domain. This
// handler extracts the bead id from the URL path, fetches the bead from the
// daemon, and posts a rich inline preview card (type badge + title + status +
// assignee + snippet) via chat.unfurl.
//
// One handler covers every entity surface — /beads, /plans, /agents, /bundle,
// /gardener — because the path is just an id carrier: the bead's own TYPE drives
// the card, so all entity kinds come for free. It works in thread replies
// automatically (chat.unfurl targets the message ts regardless of whether it's
// a root message or a reply).
//
// Note: link_shared fires only on real URLs (never bare ka-XXX text), so this
// handler never previews typed ids. It DOES fire for the bridge's own posts:
// the linkify transport turns bare ids in a notice into viewer URLs, and Slack
// sends link_shared for them (message 1791734548.149499, 2026-10-11). See the
// design doc (bead ka-0000004532) for the full rationale.

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

// maxUnfurlLinks caps how many links a single link_shared event may unfurl, so
// a message pasted with dozens of URLs can't fan out into unbounded daemon
// fetches / attachments.
const maxUnfurlLinks = 10

// slackMessageTSPattern matches a real Slack message timestamp ("1784055212.286079").
// Compose-box previews put a UUID in MessageTimeStamp instead (see handleLinkShared).
var slackMessageTSPattern = regexp.MustCompile(`^\d+\.\d+$`)

// batchBeadFetcher is the optional daemon capability used to hydrate several
// shared links in one round-trip. The production *beadsapi.Client satisfies it;
// simpler test stubs that only implement BeadClient fall back to per-id GetBead.
type batchBeadFetcher interface {
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// newStringSet builds a lookup set from a slice, dropping empty/blank entries.
// Returns nil for an empty input so callers can treat "no allowlist" and
// "disabled everywhere" identically.
func newStringSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	set := make(map[string]bool, len(items))
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" {
			set[it] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// linkUnfurlEnabledForChannel reports whether URL unfurling is enabled in the
// given channel. Unfurling is an opt-in, per-channel feature: it fires only in
// channels explicitly listed in linkUnfurlChannels (env SLACK_LINK_UNFURL_CHANNELS).
// An empty/nil set — the default — means unfurling is OFF everywhere, so a
// pasted viewer link never draws a preview card until a channel is allowlisted.
func (b *Bot) linkUnfurlEnabledForChannel(channel string) bool {
	if b == nil || len(b.linkUnfurlChannels) == 0 {
		return false
	}
	return b.linkUnfurlChannels[channel]
}

// handleLinkShared responds to a Slack link_shared event by unfurling any
// gardenio viewer links it carries into rich preview cards.
func (b *Bot) handleLinkShared(ctx context.Context, ev *slackevents.LinkSharedEvent) {
	if b == nil || ev == nil {
		return
	}
	// Per-channel gate: unfurling is opt-in and OFF by default (empty
	// allowlist). Skip cleanly in any channel that has not been allowlisted so
	// the noisy always-on behavior never fires until an operator enables it for
	// a specific channel. See linkUnfurlEnabledForChannel.
	if !b.linkUnfurlEnabledForChannel(ev.Channel) {
		return
	}
	// Compose-box previews (a link typed but not yet sent) carry a UUID in
	// MessageTimeStamp rather than a real ts. slack-go v0.18.0's
	// LinkSharedEvent does not expose the unfurl_id/source that
	// UnfurlMessageByIDContext needs for that case, so skip cleanly rather
	// than post to a bogus ts. Only sent-message links are unfurled.
	if !slackMessageTSPattern.MatchString(ev.MessageTimeStamp) {
		b.logger.Debug("skipping link_shared compose-box preview (no message ts)",
			"channel", ev.Channel, "message_ts", ev.MessageTimeStamp)
		return
	}

	unfurls := b.buildUnfurls(ctx, ev)
	if len(unfurls) == 0 || b.api == nil {
		return
	}

	if _, _, _, err := b.api.UnfurlMessageContext(ctx, ev.Channel, ev.MessageTimeStamp, unfurls); err != nil {
		b.logger.Warn("chat.unfurl failed",
			"channel", ev.Channel, "message_ts", ev.MessageTimeStamp, "links", len(unfurls), "error", err)
		return
	}
	b.logger.Debug("unfurled gardenio links",
		"channel", ev.Channel, "message_ts", ev.MessageTimeStamp, "links", len(unfurls))
}

// buildUnfurls turns a link_shared event into the url->attachment map handed to
// chat.unfurl: it selects viewer-domain links, hydrates their bead ids, and
// renders one preview card per shared URL. Unknown / deleted ids are skipped
// (emit no card rather than an error), so the returned map can be smaller than
// the set of shared links — or empty. Extracted from handleLinkShared so the
// map shape is testable without a live *slack.Client.
func (b *Bot) buildUnfurls(ctx context.Context, ev *slackevents.LinkSharedEvent) map[string]slack.Attachment {
	host := b.viewerHost()
	if host == "" {
		return nil // viewer URL not configured — nothing to match against
	}

	// Collect viewer-domain links and the bead id each carries.
	urlToID, ids := selectUnfurlLinks(ev.Links, host)
	if len(urlToID) == 0 {
		return nil
	}

	beads := b.batchGetBeads(ctx, ids)

	// Build one attachment per shared URL. Unknown / deleted ids are skipped
	// (emit no card rather than an error).
	unfurls := make(map[string]slack.Attachment, len(urlToID))
	for linkURL, id := range urlToID {
		bead := beads[id]
		if bead == nil {
			continue
		}
		// Text blocks only. A plan card used to append a slack_file image block
		// (its badge, uploaded through files.upload v2 without a channel), and
		// chat.unfurl rejects the WHOLE call with cannot_parse_attachment when
		// any unfurl carries one: every message linking a plan lost all of its
		// previews. Proven live on 2026-10-11 against message
		// 1791734548.149499: the same two cards failed with the image block and
		// returned ok without it.
		unfurls[linkURL] = b.buildUnfurlAttachment(b.themeFor(ctx, ev.Channel), bead, linkURL)
	}
	return unfurls
}

// selectUnfurlLinks filters shared links down to those on the viewer host that
// carry a bead id, returning a url->id map (repeated urls collapse) and the
// de-duplicated id fetch list. It caps output at maxUnfurlLinks so one message
// pasted with many urls can't fan out into unbounded fetches / attachments.
func selectUnfurlLinks(links []slackevents.SharedLinks, host string) (map[string]string, []string) {
	urlToID := make(map[string]string)
	seenID := make(map[string]bool)
	var ids []string
	for _, link := range links {
		if len(urlToID) >= maxUnfurlLinks {
			break
		}
		if !strings.EqualFold(link.Domain, host) {
			continue
		}
		id := extractBeadID(link.URL)
		if id == "" {
			continue
		}
		if _, dup := urlToID[link.URL]; dup {
			continue
		}
		urlToID[link.URL] = id
		if !seenID[id] {
			seenID[id] = true
			ids = append(ids, id)
		}
	}
	return urlToID, ids
}

// viewerHost returns the host of the configured beads-viewer URL (e.g.
// "gardenio.example.com"), matched against each shared link's Domain. Empty
// when the viewer URL is unset (dev/test) or unparseable.
func (b *Bot) viewerHost() string {
	if b == nil || b.beadsViewerURL == "" {
		return ""
	}
	u, err := url.Parse(b.beadsViewerURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// extractBeadID returns the last path segment of rawURL matching beadIDPattern
// (^gd-[A-Za-z0-9]+$), or "" if none. This handles /beads/ka-X, /plans/ka-X,
// /agents/ka-X, /bundle/ka-X, and trailing sub-paths (/beads/ka-X/comments)
// uniformly — the path is just an id carrier.
func extractBeadID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		if beadIDPattern.MatchString(segs[i]) {
			return segs[i]
		}
	}
	return ""
}

// batchGetBeads hydrates ids into a map keyed by bead id. It prefers the
// daemon's batch endpoint (one round-trip) and falls back to per-id GetBead
// for daemons that don't implement it. ids not found are simply absent from
// the map. Never returns an error — a fetch failure yields no card.
func (b *Bot) batchGetBeads(ctx context.Context, ids []string) map[string]*beadsapi.BeadDetail {
	if b.daemon == nil || len(ids) == 0 {
		return nil
	}
	if bf, ok := b.daemon.(batchBeadFetcher); ok {
		beads, err := bf.BatchGetBeads(ctx, ids)
		if err != nil {
			b.logger.Warn("link_shared batch bead fetch failed", "ids", ids, "error", err)
			return nil
		}
		return beads
	}
	out := make(map[string]*beadsapi.BeadDetail, len(ids))
	for _, id := range ids {
		bead, err := b.daemon.GetBead(ctx, id)
		if err != nil || bead == nil {
			continue
		}
		out[id] = bead
	}
	return out
}

// buildUnfurlAttachment renders a lean Block Kit preview card for bead, with
// the title linking back to the pasted URL. linkURL is the exact URL the human
// shared (may be /beads, /plans, /agents, …) so the card links back to the
// surface they referenced.
func (b *Bot) buildUnfurlAttachment(th Theme, bead *beadsapi.BeadDetail, linkURL string) slack.Attachment {
	title := beadTitle(bead.ID, bead.Title)
	titleLine := fmt.Sprintf("<%s|%s>", linkURL, slackMrkdwnLabelEscaper.Replace(title))
	// Arms sit with the title as the emoji run (heraldry spec Q9,
	// heraldry_cards.go). The type badge keeps its own slot in the metadata
	// line (R9).
	if run := armsRun(bead.Heraldry); run != "" {
		titleLine = run + " " + titleLine
	}

	// Metadata line: type badge · status · assignee. Each glyph rides next to
	// its verbal label in the same text run (accessibility rule): the type
	// glyph beside the type word, the status glyph beside the status word.
	meta := []string{
		beadTypeBadge(th, bead),
		fmt.Sprintf("%s %s", beadStatusEmoji(th, bead.Status), beadDisplayStatus(bead.Status)),
	}
	if a := strings.TrimSpace(bead.Assignee); a != "" {
		meta = append(meta, fmt.Sprintf("%s %s", th.Semantic(SemAssignee), slackMrkdwnLabelEscaper.Replace(a)))
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", titleLine, false, false), nil, nil),
		slack.NewContextBlock("", slack.NewTextBlockObject("mrkdwn", strings.Join(meta, "  ·  "), false, false)),
	}
	if snip := beadSnippet(bead.Description); snip != "" {
		blocks = append(blocks,
			slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", snip, false, false), nil, nil))
	}

	// Blocks ONLY. chat.unfurl rejects an unfurl that mixes Block Kit blocks
	// with legacy attachment fields (color, fallback, title, ...) with
	// "cannot_parse_attachment": every unfurl failed that way while this card
	// carried a status color bar and a fallback string. Status is shown by the
	// themed glyph in the metadata line instead, so unfurls carry no color bar.
	return slack.Attachment{
		Blocks: slack.Blocks{BlockSet: blocks},
	}
}

// beadSnippet collapses a bead description to a single-line preview snippet.
func beadSnippet(desc string) string {
	s := strings.TrimSpace(desc)
	if s == "" {
		return ""
	}
	// Collapse all runs of whitespace (incl. newlines) into single spaces.
	s = strings.Join(strings.Fields(s), " ")
	return truncateText(s, 300)
}

// beadTypeBadge renders the type badge for the unfurl metadata line. A plan
// (type=epic + "plan" label) takes the theme's plan badge rather than the epic
// one.
func beadTypeBadge(th Theme, bead *beadsapi.BeadDetail) string {
	if beadIsPlan(bead) {
		return th.BeadTypeBadge("plan")
	}
	return th.BeadTypeBadge(bead.Type)
}

// beadDisplayType renders a bead type for the metadata badge, defaulting to
// "bead" when a bead somehow has no type.
func beadDisplayType(t string) string {
	if t == "" {
		return "bead"
	}
	return t
}

// beadStatusEmoji maps a bead status to the theme's lifecycle glyph. An
// unrecognized status gets a plain "?" in every theme.
func beadStatusEmoji(th Theme, status string) string {
	switch status {
	case "open":
		return th.Glyph(LifecycleSpawning)
	case "in_progress":
		return th.Glyph(LifecycleWorking)
	case "closed":
		return th.Glyph(LifecycleDone)
	case "blocked":
		return th.Glyph(LifecycleBlocked)
	default:
		return "?"
	}
}

// beadDisplayStatus renders a status for humans ("in_progress" -> "in progress").
func beadDisplayStatus(status string) string {
	if status == "" {
		return "unknown"
	}
	return strings.ReplaceAll(status, "_", " ")
}
