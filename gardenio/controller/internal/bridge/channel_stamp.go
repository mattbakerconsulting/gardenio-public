package bridge

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Commons channel_id dual-write (spec gardenio/docs/superpowers/specs/
// 2026-10-19-lands-commons-design.md §2.3, §7.1 step 4).
//
// Every thread head and forwarded thread reply the bridge writes is stamped
// with fields.channel_id: the type=channel bead bound to the Slack channel it
// was said in (fields.slack_channel_id, unique across channel beads). Nothing
// reads channel_id yet; it is written beside the existing slack_thread_channel
// so the Commons read model has it when it lands. A Slack channel no channel
// bead is bound to (the land is not seeded yet) gets no stamp, and the
// periodic backfill below picks up its open heads once it is. Squawks are
// stamped by gb at create time (cmd/gb/channel_stamp.go): the bridge's fleet
// token cannot update a message an agent created.
//
// kbeads enforces the land rule on a message that names a channel: exactly one
// project:<land> label, equal to the channel's (kbeads channel_validation.go).
// So a message is stamped only when it carries no project label (the channel's
// land label is added with the stamp) or already carries the channel's land. A
// thread head (type=task) is not validated, but gets the same compatibility
// check and never gains a project label: its project is the one its minting
// caller chose, and the stamp only records where it was said.
//
// A thread head said in a Slack channel no channel bead is bound to (a DM with
// the bot, a role channel such as a research channel) is filed under its
// land's default channel (the project bead's land_default_channel, normally
// #general) instead of no channel (headChannelFor). Before, those heads were in
// no channel, so their conversations surfaced in the Commons rather than the
// land (owner 2026-11-03: new thread agents go to the gardenio channel).
// Messages are not given the fallback: a reply follows its head's channel
// (replyInRootChannel), and the Slack-mirror and say paths keep reading only
// the real binding.

// channelBeadCacheTTL bounds how long a Slack channel's resolved channel bead
// (or the absence of one) is reused. Bindings change only when an operator
// seeds or re-binds a land, so a few minutes of staleness costs at most a
// missed stamp the backfill repairs.
const channelBeadCacheTTL = 5 * time.Minute

// channelBackfillInterval is how often open thread heads are swept for a
// missing channel_id. Lands are seeded by hand, so the sweep keeps running
// after the first pass and stamps a land's heads within one interval of its
// seed.
const channelBackfillInterval = time.Hour

// channelBackfillInitialDelay keeps the first sweep off the startup path.
const channelBackfillInitialDelay = 2 * time.Minute

// channelBackfillPageSize is the page size of the open-head listing.
const channelBackfillPageSize = 200

// channelBeadRef is the channel bead a Slack channel is bound to, its land
// and its mirror mode. The zero value means "no channel bead bound".
type channelBeadRef struct {
	ID     string
	Land   string
	Mirror string // fields.mirror: off | out | both ("" reads as off)
}

// stampFor decides whether a bead carrying labels may be stamped with this
// channel. ok is false when the bead belongs to another land (or to several),
// which the daemon would refuse for a message. addLabel is the project label
// to add with the stamp, "" when the bead already carries the channel's land.
func (r channelBeadRef) stampFor(labels []string) (addLabel string, ok bool) {
	lands := labelLands(labels)
	switch {
	case len(lands) == 0:
		return "project:" + r.Land, true
	case len(lands) == 1 && lands[0] == r.Land:
		return "", true
	}
	return "", false
}

// labelLands returns every distinct non-empty project:<land> value on labels.
func labelLands(labels []string) []string {
	var lands []string
	for _, l := range labels {
		if v, found := strings.CutPrefix(l, "project:"); found && v != "" && !slices.Contains(lands, v) {
			lands = append(lands, v)
		}
	}
	return lands
}

type channelBeadCacheEntry struct {
	ref     channelBeadRef
	expires time.Time
}

// channelBeadCache is a per-Slack-channel TTL cache for resolveChannelBead.
// Misses are cached too, so an unseeded land costs one listing per TTL, not
// one per message. The zero value is ready to use.
type channelBeadCache struct {
	mu      sync.Mutex
	entries map[string]channelBeadCacheEntry
}

func (c *channelBeadCache) get(slackChannel string, now time.Time) (channelBeadRef, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[slackChannel]
	if !ok || !now.Before(e.expires) {
		return channelBeadRef{}, false
	}
	return e.ref, true
}

func (c *channelBeadCache) put(slackChannel string, ref channelBeadRef, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]channelBeadCacheEntry)
	}
	c.entries[slackChannel] = channelBeadCacheEntry{ref: ref, expires: now.Add(channelBeadCacheTTL)}
}

// resolveChannelBead returns the channel bead bound to a Slack channel. ok is
// false when none is bound, the channel bead has no single land, or the lookup
// failed; a failed lookup is not cached, so the next call retries.
func (b *Bot) resolveChannelBead(ctx context.Context, slackChannel string) (channelBeadRef, bool) {
	ref, err := b.lookupChannelBead(ctx, slackChannel)
	return ref, err == nil && ref.ID != ""
}

// lookupChannelBead is resolveChannelBead that tells a failed lookup (err) from
// a Slack channel no channel bead is bound to (the zero ref, nil err).
func (b *Bot) lookupChannelBead(ctx context.Context, slackChannel string) (channelBeadRef, error) {
	if b == nil || b.daemon == nil || slackChannel == "" {
		return channelBeadRef{}, nil
	}
	now := time.Now()
	if ref, ok := b.channelBeads.get(slackChannel, now); ok {
		return ref, nil
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"channel"},
		Fields: map[string]string{"slack_channel_id": slackChannel},
		Limit:  1,
	})
	if err != nil {
		b.logger.Warn("channel-stamp: channel bead lookup failed",
			"slack_channel", slackChannel, "error", err)
		return channelBeadRef{}, err
	}
	var ref channelBeadRef
	if res != nil && len(res.Beads) > 0 {
		ch := res.Beads[0]
		// The daemon guarantees exactly one project label on a channel bead;
		// anything else is not a land we can stamp into.
		if lands := labelLands(ch.Labels); len(lands) == 1 {
			ref = channelBeadRef{ID: ch.ID, Land: lands[0], Mirror: ch.Fields["mirror"]}
		}
	}
	b.channelBeads.put(slackChannel, ref, now)
	return ref, nil
}

// landDefaultChannelKey keys a land's default channel in channelBeads, apart
// from Slack channel ids (which never contain a colon).
func landDefaultChannelKey(land string) string { return "land:" + land }

// landDefaultChannel returns the channel bead id a land opens on (its project
// bead's land_default_channel), "" when the land has none or the projects
// cannot be read. Hits and misses are cached like Slack channel bindings.
func (b *Bot) landDefaultChannel(ctx context.Context, land string) string {
	if b == nil || b.daemon == nil || land == "" {
		return ""
	}
	now := time.Now()
	key := landDefaultChannelKey(land)
	if ref, ok := b.channelBeads.get(key, now); ok {
		return ref.ID
	}
	projects, err := b.daemon.ListProjectBeads(ctx)
	if err != nil {
		b.logger.Warn("channel-stamp: reading the land's default channel failed",
			"land", land, "error", err)
		return ""
	}
	id := projects[land].LandDefaultChannel
	b.channelBeads.put(key, channelBeadRef{ID: id, Land: land}, now)
	return id
}

// headChannelFor is the channel_id for a thread head with labels said in
// slackChannel: the channel bead bound to the Slack channel when it may stamp
// the head (stampFor), else, when no channel bead is bound to it at all, the
// head's land's default channel. "" when neither applies or the binding could
// not be read (a later backfill retries).
func (b *Bot) headChannelFor(ctx context.Context, slackChannel string, labels []string) string {
	ref, err := b.lookupChannelBead(ctx, slackChannel)
	if err != nil {
		return ""
	}
	if ref.ID != "" {
		if _, ok := ref.stampFor(labels); ok {
			return ref.ID
		}
		return ""
	}
	if lands := labelLands(labels); len(lands) == 1 {
		return b.landDefaultChannel(ctx, lands[0])
	}
	return ""
}

// stampHeadChannelID stamps channel_id on an existing thread head, resolving
// the channel from its slack_thread_channel (headChannelFor). A head never gains a project
// label. Best-effort: a failure is logged and returned, and the head is left
// as it was.
func (b *Bot) stampHeadChannelID(ctx context.Context, head *beadsapi.BeadDetail) (bool, error) {
	if head == nil || head.ID == "" || head.Fields["channel_id"] != "" {
		return false, nil
	}
	chID := b.headChannelFor(ctx, head.Fields["slack_thread_channel"], head.Labels)
	if chID == "" {
		return false, nil
	}
	if err := b.daemon.UpdateBeadFields(ctx, head.ID, map[string]string{"channel_id": chID}); err != nil {
		b.logger.Warn("channel-backfill: stamping channel_id failed",
			"bead", head.ID, "channel", chID, "error", err)
		return false, err
	}
	return true, nil
}

// startChannelBackfill stamps channel_id on open thread heads that predate the
// dual-write, once shortly after startup and then every
// channelBackfillInterval.
func (b *Bot) startChannelBackfill(ctx context.Context) {
	timer := time.NewTimer(channelBackfillInitialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			b.backfillThreadHeadChannels(ctx)
			timer.Reset(channelBackfillInterval)
		}
	}
}

// backfillThreadHeadChannels stamps channel_id on every open thread head that
// lacks it. It returns the number of heads stamped. A head the daemon refuses
// is not retried for the life of the process. Only the backfill goroutine (and
// tests) call it.
func (b *Bot) backfillThreadHeadChannels(ctx context.Context) int {
	if b == nil || b.daemon == nil {
		return 0
	}
	stamped := 0
	for offset := 0; ; offset += channelBackfillPageSize {
		res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"task"},
			Labels:   []string{threadKindLabel},
			Statuses: []string{"open", "in_progress", "blocked", "deferred"},
			Sort:     "created_at",
			Limit:    channelBackfillPageSize,
			Offset:   offset,
		})
		if err != nil {
			b.logger.Warn("channel-backfill: listing open thread heads failed",
				"offset", offset, "error", err)
			break
		}
		if res == nil {
			break
		}
		for _, head := range res.Beads {
			if ctx.Err() != nil {
				return stamped
			}
			if b.channelBackfillRefused[head.ID] {
				continue
			}
			ok, err := b.stampHeadChannelID(ctx, head)
			if ok {
				stamped++
			} else if err != nil && !isTransientDaemonError(err) {
				// Typically a head a linked person minted before the bridge
				// was named its assignee (gd-PQCcv8sndp): the bridge does not
				// own it, so every later sweep would be refused the same way.
				if b.channelBackfillRefused == nil {
					b.channelBackfillRefused = make(map[string]bool)
				}
				b.channelBackfillRefused[head.ID] = true
			}
		}
		if len(res.Beads) < channelBackfillPageSize {
			break
		}
	}
	if stamped > 0 {
		b.logger.Info("channel-backfill: stamped channel_id on open thread heads", "count", stamped)
	}
	return stamped
}
