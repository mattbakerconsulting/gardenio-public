package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// Two-way Slack mirroring (design spec 2026-10-19-lands-commons-design.md,
// section 7.2 step 9). Slack stays a permanent, optional mirror of a land
// (gd-78Tb2kbQrR); a channel bead's `mirror` field says which way it runs:
//
//   - Slack → Commons (mirror=both): a person's Slack message in the bound
//     channel becomes a closed type=message bead with origin=slack and its
//     Slack copy as slack_channel/slack_ts, so the Commons shows it. A thread
//     reply under a wish head is already recorded by the thread forward
//     (bot_thread_forward.go) and is left to it; a reply under any other root
//     the Commons knows (a mirrored message, a card) is written here with
//     that root as its thread_id. Mentions are composes and stay on the
//     mention path, which mints the wish head.
//   - Commons → Slack (mirror=out or both): CommonsMirror projects a person's
//     origin=commons message into the bound Slack channel through the Router,
//     as its author (commons_persona.go: their name and avatar with
//     chat:write.customize, else attributed "*Name* (via Commons)"), and the
//     Router stores the Slack ref on the bead. A reply into a Slack-bound wish
//     head carries thread-say and is posted by ThreadMessageForward instead,
//     the same way.
//
// The origin loop guard is three checks, one per hop: a bot's Slack post (the
// mirror posts as the bot) is never mirrored in; only origin=commons and
// origin=glass beads are projected out, so an origin=slack bead never goes back
// to Slack; and the Router skips a message's origin surface.
//
// A Glass nudge (gardenhub glass_send.go) is delivered to its agent first, then
// written into the agent's thread as the person's words with origin=glass
// (gd-Ik3rTCqfxp). The mirror posts it into the agent's Slack thread, the Slack
// copy of its thread root, as "*Name* via the Glass: ...", whatever the
// channel's mirror mode: that thread is the agent's, as its own posts are
// (machinesChannel). Nothing delivers it to the agent again: the inbound paths
// take only origin=commons and thread-say messages (commonsMessage.fromCommons),
// and its Slack copy is the bot's post, which no Slack path hands on.
//
// An unset mirror is off in both directions (conversation.MirrorsIn and
// MirrorsOut): binding slack_channel_id alone copies nothing until mirror is
// set, so a channel bound to a Slack channel it should not see stays dark.
//
// Attachments travel as links, never bytes (gd-2Ie1cDAm4K). A Slack message's
// files are recorded in the bead's `files` field with their Slack permalinks,
// and the Commons shows them; a file-only Slack post is mirrored too. A Commons
// message's `files` go out as links under its text.
//
// CommonsMirror runs off SSE, so Sweep is the safety net for a message written
// while the bridge was down: it posts an origin=commons message that has no
// Slack ref, after first looking in Slack for the bot's copy, so a post whose
// ref stamp was lost is repaired rather than posted twice. It posts only a
// message written after its channel bead last changed, so turning a dark
// channel's mirror on never posts what was said while it was off.

// Message bead fields of a message's Slack copy (the Router's ref stamp).
const (
	beadSlackChannel = conversation.SurfaceSlack + "_channel"
	beadSlackTS      = conversation.SurfaceSlack + "_ts"
)

// slackMirrorLabel marks a message bead written from a Slack message by the
// mirror, for the per-user inbox and for operators reading the daemon.
const slackMirrorLabel = "slack-mirror"

// slackMirrorIdempotencyKey keys a mirrored top-level Slack message, so a
// redelivered event lands on the same bead. A reply reuses the thread
// forward's key (threadReplyIdempotencyKey).
func slackMirrorIdempotencyKey(channel, ts string) string {
	return "slack-message:" + channel + ":" + ts
}

// mirrorSlackMessage writes a person's Slack message in a mirror=both channel
// as a message bead, and returns its id ("" when the message is not mirrored
// or the write failed). Best-effort: the Slack message is already said, so a
// failure is logged and never stops the event's own handling.
func (b *Bot) mirrorSlackMessage(ctx context.Context, ev *slackevents.MessageEvent, operator string) string {
	if b == nil || b.daemon == nil || ev == nil {
		return ""
	}
	// Loop guard: a bot's post, ours included, is never mirrored in. The
	// Commons mirror posts as the bot, so this is what stops an echo.
	if ev.BotID != "" || ev.User == "" || ev.User == b.botUserID {
		return ""
	}
	if ev.ChannelType == "im" || ev.ChannelType == "mpim" || !isUtteranceSubtype(ev.SubType) {
		return ""
	}
	text := strings.TrimSpace(ev.Text)
	if ev.TimeStamp == "" {
		return ""
	}
	if b.botUserID != "" && strings.Contains(text, "<@"+b.botUserID+">") {
		return "" // a mention is a compose: the mention path records it
	}
	ref, ok := b.resolveChannelBead(ctx, ev.Channel)
	if !ok || !conversation.MirrorsIn(ref.Mirror) {
		return ""
	}
	files := b.slackEventFiles(ctx, ev)
	if text == "" && len(files) == 0 {
		return ""
	}

	fields := map[string]string{
		conversation.BeadChannelID: ref.ID,
		conversation.BeadAuthor:    operator,
		conversation.BeadKind:      string(conversation.KindText),
		conversation.BeadText:      text,
		conversation.BeadOrigin:    conversation.SurfaceSlack,
		beadSlackChannel:           ev.Channel,
		beadSlackTS:                ev.TimeStamp,
		"slack_user_id":            ev.User,
	}
	for k, v := range slackFilesToFields(files) {
		fields[k] = v
	}
	key := slackMirrorIdempotencyKey(ev.Channel, ev.TimeStamp)
	if ev.ThreadTimeStamp != "" && ev.ThreadTimeStamp != ev.TimeStamp {
		// Runs after the reply was dispatched, so a head the thread forward
		// minted for it is already there.
		if b.findThreadHeadByThreadLabel(ctx, ev.Channel, ev.ThreadTimeStamp) != "" {
			return ""
		}
		root, rootChannel := b.slackCopyBead(ctx, ref.Land, ev.Channel, ev.ThreadTimeStamp)
		if root == "" {
			return "" // said before the mirror ran: the Commons has no root
		}
		fields[conversation.BeadThreadID] = root
		if rootChannel != "" {
			// A reply is in its root's channel: an agent card's thread is in
			// #machines, not the channel bound to the Slack one (single voice).
			fields[conversation.BeadChannelID] = rootChannel
		}
		key = threadReplyIdempotencyKey(ev.Channel, ev.TimeStamp)
	}
	if text == "" {
		delete(fields, conversation.BeadText) // the files are the message
	}
	raw, _ := json.Marshal(fields) // a map[string]string always marshals
	title := text
	if title == "" {
		title = "Shared " + fileNames(files)
	}

	var id string
	err := b.retryTransientDaemon(ctx, "mirror", func() error {
		var e error
		id, e = b.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:          truncateText(title, 500),
			Type:           "message",
			Kind:           "data",
			Status:         "closed", // said once; nothing to work
			Labels:         []string{"project:" + ref.Land, slackMirrorLabel},
			CreatedBy:      operator,
			Fields:         raw,
			IdempotencyKey: key,
		})
		return e
	})
	if err != nil {
		b.logger.Warn("commons-mirror: writing Slack message bead failed",
			"channel", ev.Channel, "ts", ev.TimeStamp, "channel_bead", ref.ID, "error", err)
		return ""
	}
	return id
}

// slackEventFiles returns a Slack message's files. The Events API carries them
// on the message (slack-go unmarshals the top level into ev.Message); a
// file_share event without them is re-fetched.
func (b *Bot) slackEventFiles(ctx context.Context, ev *slackevents.MessageEvent) []slack.File {
	if ev.Message != nil && len(ev.Message.Files) > 0 {
		return ev.Message.Files
	}
	if ev.SubType == "file_share" {
		return b.fetchMessageFiles(ctx, ev.Channel, ev.TimeStamp)
	}
	return nil
}

// maxMirroredFiles caps the attachments one message records; Slack allows ten
// per message.
const maxMirroredFiles = 10

// fileRef is one entry of a message bead's files field
// (conversation.BeadFiles).
type fileRef struct {
	Name     string `json:"name"`
	MimeType string `json:"mimetype,omitempty"`
	Size     int    `json:"size,omitempty"`
	URL      string `json:"url"`
}

// slackFileRefs is the files field of a Slack message's attachments: each
// file's Slack permalink, which opens it for anyone in the channel. A file
// with no permalink (a deleted or external one) is left out.
func slackFileRefs(files []slack.File) []fileRef {
	var out []fileRef
	for _, f := range files {
		if len(out) == maxMirroredFiles {
			break
		}
		link := f.Permalink
		if link == "" || !openableURL(link) {
			continue
		}
		name := firstNonEmpty(f.Name, f.Title, "file")
		out = append(out, fileRef{Name: name, MimeType: f.Mimetype, Size: f.Size, URL: link})
	}
	return out
}

// decodeFileRefs reads a files field, dropping an entry with no openable URL.
// A malformed field reads as no files.
func decodeFileRefs(raw string) []fileRef {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var refs []fileRef
	if json.Unmarshal([]byte(raw), &refs) != nil {
		return nil
	}
	out := refs[:0]
	for _, f := range refs {
		if openableURL(f.URL) && len(out) < maxMirroredFiles {
			out = append(out, f)
		}
	}
	return out
}

// withSnapLink is a message's files with its farm snapshot's view link after
// them (conversation.BeadSnap), so whoever reads the post in Slack, or an
// agent nudged by it, opens the view the picture shows as well as the
// picture. A snap with no files, or no openable link, adds nothing.
func withSnapLink(files []fileRef, raw string) []fileRef {
	if len(files) == 0 || strings.TrimSpace(raw) == "" {
		return files
	}
	var snap struct {
		URL string `json:"url"`
		At  string `json:"at"`
	}
	if json.Unmarshal([]byte(raw), &snap) != nil || !openableURL(snap.URL) {
		return files
	}
	place := snap.At
	if p, err := url.PathUnescape(place); err == nil {
		place = p
	}
	switch place = strings.TrimPrefix(place, "~/"); place {
	case "":
		place = "the wild"
	case "yard":
		place = "the Yard"
	}
	return append(files, fileRef{Name: "Open the farm view at " + place, URL: snap.URL})
}

// openableURL reports whether u is an absolute http(s) URL that is safe to put
// inside a Slack link (no angle brackets, pipe or whitespace).
func openableURL(u string) bool {
	if strings.ContainsAny(u, "<>| \t\n") {
		return false
	}
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "https" || p.Scheme == "http") && p.Host != ""
}

func fileNames(files []slack.File) string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, firstNonEmpty(f.Name, f.Title, "a file"))
	}
	return strings.Join(names, ", ")
}

// slackCopyBead returns the message bead in land whose Slack copy is
// (channel, ts): a mirrored or projected message, or a card; and the channel
// bead it is in ("" when it names none).
func (b *Bot) slackCopyBead(ctx context.Context, land, channel, ts string) (id, channelID string) {
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Labels: []string{"project:" + land},
		Fields: map[string]string{beadSlackChannel: channel, beadSlackTS: ts},
		Limit:  1,
	})
	if err != nil || res == nil || len(res.Beads) == 0 || res.Beads[0] == nil {
		return "", ""
	}
	return res.Beads[0].ID, strings.TrimSpace(res.Beads[0].Fields[conversation.BeadChannelID])
}

// slackCopyOf reads where a thread root is shown in Slack: its own copy
// (slack_channel/slack_ts), else a wish head's Slack thread.
func slackCopyOf(fields map[string]string) (conversation.Ref, bool) {
	if c, ts := fields[beadSlackChannel], fields[beadSlackTS]; isValidThreadBinding(c, ts) {
		return conversation.Ref{Surface: conversation.SurfaceSlack, Channel: c, TS: ts}, true
	}
	return slackThreadRoot(fields)
}

// viaCommonsWithFiles is viaCommons with a message's files as links under its
// text, one per line.
func viaCommonsWithFiles(name, text string, files []fileRef) string {
	name = strings.ReplaceAll(strings.TrimSpace(name), "*", "")
	if name == "" {
		name = "Someone"
	}
	return "*" + escapeSlackMrkdwn(name) + "* (via Commons)\n" + commonsWords(text, files)
}

// commonsWords is a Commons message's escaped words with its files as links
// under them: the whole text of a post made as its author.
func commonsWords(text string, files []fileRef) string {
	out := escapeSlackMrkdwn(text)
	if len(files) == 0 {
		return out
	}
	lines := make([]string, 0, len(files))
	for _, f := range files {
		label := strings.ReplaceAll(firstNonEmpty(f.Name, "file"), "|", "/")
		lines = append(lines, "<"+f.URL+"|"+escapeSlackMrkdwn(label)+">")
	}
	if strings.TrimSpace(text) == "" {
		return out + strings.Join(lines, "\n")
	}
	return out + "\n" + strings.Join(lines, "\n")
}

// fileLinks is a message's files as plain text, one per line: the name, what
// it is and how big when the files field says, then where it opens, as in
// "farm.png (image/png, 12.0 KB): <url>". It is how an agent reads them.
func fileLinks(files []fileRef) string {
	lines := make([]string, len(files))
	for i, f := range files {
		var about []string
		if f.MimeType != "" {
			about = append(about, f.MimeType)
		}
		if f.Size > 0 {
			about = append(about, fileSize(f.Size))
		}
		line := firstNonEmpty(f.Name, "file")
		if len(about) > 0 {
			line += " (" + strings.Join(about, ", ") + ")"
		}
		lines[i] = line + ": " + f.URL
	}
	return strings.Join(lines, "\n")
}

// fetchHint is the line under a Commons message's files that tells the agent
// it reaches how to open them (gd-WXY7Jm5ugY): the viewer link needs a signed
// in browser, but `gb commons file` fetches the bytes on the agent's own token
// for it to Read. A message with no Commons attachment has none.
func fetchHint(messageID string, files []fileRef) string {
	if messageID == "" {
		return ""
	}
	for _, f := range files {
		if _, _, _, ok := CommonsFileRoute(f.URL); ok {
			return "fetch with: gb commons file " + messageID
		}
	}
	return ""
}

// fileSize is n bytes for a person to read: 512 B, 12.0 KB, 3.4 MB.
func fileSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return strconv.Itoa(n) + " B"
}

// commonsAuthor is the name a Commons author shows with in Slack, the user
// directory's display name else the email's local part (an email is never
// posted whole), and their Slack user id when the directory links one.
func (b *Bot) commonsAuthor(ctx context.Context, author string) (name, slackID string) {
	email := author
	if _, rest, ok := strings.Cut(author, ":"); ok {
		email = rest // oidc:<email>
	}
	if b != nil && b.userDir != nil && strings.Contains(email, "@") {
		if u := b.UserDirectory().LookupByEmail(ctx, email); u != nil {
			if strings.TrimSpace(u.DisplayName) != "" {
				return u.DisplayName, u.SlackID
			}
			slackID = u.SlackID
		}
	}
	return emailLocal(author), slackID
}

// emailLocal is an author's email local part (oidc:<email>), the name a
// Commons author shows with when the directory has none.
func emailLocal(author string) string {
	email := author
	if _, rest, ok := strings.Cut(author, ":"); ok {
		email = rest
	}
	local, _, _ := strings.Cut(email, "@")
	return local
}

// slackAuthor is who m shows as in Slack, and whether it may post under their
// persona. A message its author wrote (writtenByAuthor) shows as them. Any
// other is attributed to its writer, on the bot's own post: its author field
// is free text, and an agent that set it to a person's principal would
// otherwise put words in Slack under that person's name and avatar
// (gd-Wu4U4hZbY0). kbeads refuses such a write now; this covers the ones
// written before it did.
func (m commonsMessage) slackAuthor() (who string, persona bool) {
	if m.writtenByAuthor() {
		return m.author(), true
	}
	return asPrincipal(m.CreatedBy), false
}

// commonsSlackText is what Commons message m posts into Slack, and who it
// shows as: the words alone under the author's persona when the bot may
// customize its posts, else the bot's post attributed to them by name.
func (b *Bot) commonsSlackText(ctx context.Context, m commonsMessage, text string, files []fileRef) (string, *conversation.Persona) {
	who, persona := m.slackAuthor()
	name, slackID := b.commonsAuthor(ctx, who)
	if persona {
		if as := b.commonsPersona(ctx, name, slackID); as != nil {
			return commonsWords(text, files), as
		}
	}
	return viaCommonsWithFiles(name, text, files), nil
}

// CommonsMirrorConfig configures the CommonsMirror watcher.
type CommonsMirrorConfig struct {
	// Router projects into Slack: the Bot's Conversation().
	Router *conversation.Router
	// Bot reads channel and root beads and names authors.
	Bot *Bot
	// Limiter, when non-nil, gates every post through the shared
	// slackCardLimiter. Nil disables rate limiting (tests).
	Limiter *rate.Limiter
	// Files, when non-nil, fetches a message's attachments so they are
	// uploaded into Slack (commons_files.go). Nil posts them as links.
	Files  *CommonsFileFetcher
	Logger *slog.Logger
}

// CommonsMirror projects a person's Commons messages into the Slack channel
// their channel bead mirrors to.
type CommonsMirror struct {
	router  *conversation.Router
	bot     *Bot
	limiter *rate.Limiter
	files   *CommonsFileFetcher
	logger  *slog.Logger

	mu       sync.Mutex
	seen     map[string]bool // dedup on SSE reconnect (keyed by bead ID)
	inflight map[string]bool // a projection under way, from SSE or the sweep
	sweeps   map[string]int  // sweep attempts per bead
}

// The Commons → Slack sweep (Sweep).
const (
	// commonsMirrorSweepWindow bounds how late the sweep posts a message: one
	// older than this stays in the Commons only.
	commonsMirrorSweepWindow = 6 * time.Hour
	// commonsMirrorSweepTries caps the sweep's attempts at one message in a
	// process, so one Slack keeps refusing is not retried every interval.
	commonsMirrorSweepTries = 3
	// commonsMirrorLookupPages caps the Slack history pages the double-post
	// guard reads for one message.
	commonsMirrorLookupPages = 5
	// commonsMirrorSweepPageSize and commonsMirrorSweepPages bound one
	// sweep's read of the window's messages.
	commonsMirrorSweepPageSize = 200
	commonsMirrorSweepPages    = 5
)

// NewCommonsMirror returns the watcher, or nil without a router or a Bot with
// a daemon.
func NewCommonsMirror(cfg CommonsMirrorConfig) *CommonsMirror {
	if cfg.Router == nil || cfg.Bot == nil || cfg.Bot.daemon == nil {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &CommonsMirror{
		router: cfg.Router, bot: cfg.Bot, limiter: cfg.Limiter, files: cfg.Files, logger: logger,
		seen: map[string]bool{}, inflight: map[string]bool{}, sweeps: map[string]int{},
	}
}

// RegisterHandlers wires the watcher onto an SSE stream. The Commons creates
// its messages closed (a person may not close one), so the create event is
// the one that matters; the close covers any writer that closes after.
func (c *CommonsMirror) RegisterHandlers(stream *SSEStream) {
	if c == nil {
		return
	}
	stream.On("beads.bead.created", c.handleEvent)
	stream.On("beads.bead.closed", c.handleEvent)
	c.logger.Info("commons-mirror registered SSE handlers",
		"topics", []string{"beads.bead.created", "beads.bead.closed"})
}

// Start runs Sweep every commonsSweepInterval until ctx is done.
func (c *CommonsMirror) Start(ctx context.Context) {
	if c == nil {
		return
	}
	ticker := time.NewTicker(commonsSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Sweep(ctx, time.Now())
		}
	}
}

func (c *CommonsMirror) handleEvent(ctx context.Context, data []byte) {
	if c == nil {
		return
	}
	if bead := ParseBeadEvent(data); bead != nil && bead.Status == "closed" {
		c.project(ctx, bead)
	}
}

// mirrorCandidate reports whether bead is a person's words said in the
// Commons that have not reached Slack. This is the loop guard: an
// origin=slack bead came from Slack, and a thread-say reply is
// ThreadMessageForward's to post.
//
// A message with a slack_channel but no slack_ts reached Slack once: the two
// are stamped together (stampRef, the Router), so its ts was cleared after.
// It is not posted again. kbeads refuses that clear from anyone but the
// message's writer (gd-74wnmpQgyT); this keeps a regression there from
// re-posting rewritten words under the author's name (gd-oWVBI5NFvK). A
// writer that clears both fields is not caught here.
//
// An origin=glass message is one only as a reply: a Glass nudge's copy is
// always in its agent's thread.
func mirrorCandidate(bead *BeadEvent) bool {
	if bead.Type != "message" || hasLabel(bead.Labels, ThreadSayLabel) || bead.Fields[beadSlackTS] != "" || bead.Fields[beadSlackChannel] != "" {
		return false
	}
	switch bead.Fields[conversation.BeadOrigin] {
	case conversation.SurfaceCommons:
		return true
	case conversation.OriginGlass:
		return bead.Fields[conversation.BeadThreadID] != ""
	}
	return false
}

// writtenByCreator reports whether the change bead's event reports was made
// by the message's creator: its actor (an update) or else its closer, against
// created_by, both as principals (asPrincipal). An event naming no writer (a
// create, or auth disabled) is its creator's.
func writtenByCreator(bead *BeadEvent) bool {
	writer := bead.Actor
	if writer == "" {
		writer = bead.ClosedBy
	}
	return writer == "" || strings.EqualFold(asPrincipal(writer), asPrincipal(bead.CreatedBy))
}

// projection is one Commons message ready to post into Slack.
type projection struct {
	id     string
	ch     conversation.ChannelRef
	thread *conversation.Thread // nil for a top-level message
	msg    conversation.Message
	// The two texts a Slack copy of it may have, whichever way it was
	// posted (the scope can come or go between a post and its sweep): the
	// bot's attributed text, and the words a post as name carries.
	attributed, words, name string
	// attributedLocal is the attributed text under the author's email local
	// part: what the live post carried when the directory lookup missed and
	// the sweep's now answers.
	attributedLocal string
	// text, files and local are what the texts above are made of: the
	// message's words, its files, and the author's email local part. post
	// uploads the files it can fetch and links the rest, and isCopyOf knows
	// that copy by the files Slack shows on it.
	text  string
	files []fileRef
	local string
	// chChanged is when the channel bead last changed; the sweep posts only
	// a message written after it (Sweep).
	chChanged time.Time
	// glass is a Glass nudge's copy, posted into its agent's Slack thread
	// whatever its channel's mirror mode (planGlass), so the sweep does not
	// hold it to its channel's last change.
	glass bool
}

// mirrorChannel is a channel bead as the mirror reads it.
type mirrorChannel struct {
	ref     conversation.ChannelRef
	changed time.Time // the bead's updated_at
}

// project posts one Commons message into Slack. It reports whether it posted.
//
// A message last written by anyone but its creator is not posted: kbeads
// refuses another writer's change to its words (gd-74wnmpQgyT), and this keeps
// a regression there from posting them under the author's name
// (gd-oWVBI5NFvK). The sweep reads beads, which carry no last writer, so it
// relies on mirrorCandidate's cleared-ref check alone.
func (c *CommonsMirror) project(ctx context.Context, bead *BeadEvent) bool {
	if !mirrorCandidate(bead) {
		return false
	}
	if !writtenByCreator(bead) {
		c.logger.Warn("commons-mirror: not posting a message its creator did not last write",
			"id", bead.ID, "created_by", bead.CreatedBy, "actor", bead.Actor, "closed_by", bead.ClosedBy)
		return false
	}
	if !c.claim(bead.ID, true) {
		return false
	}
	defer c.release(bead.ID)
	p, ok := c.plan(ctx, bead, nil)
	return ok && c.post(ctx, p)
}

// plan reads where bead goes and what it says. channels, when non-nil,
// memoizes channel bead reads across one sweep.
func (c *CommonsMirror) plan(ctx context.Context, bead *BeadEvent, channels map[string]*mirrorChannel) (projection, bool) {
	land := projectFromLabels(bead.Labels)
	if land == "" || len(labelLands(bead.Labels)) != 1 {
		return projection{}, false
	}
	if bead.Fields[conversation.BeadOrigin] == conversation.OriginGlass {
		return c.planGlass(ctx, bead, land)
	}
	chID := bead.Fields[conversation.BeadChannelID]
	if chID == "" {
		return projection{}, false
	}
	mc, ok := c.cachedChannel(ctx, chID, channels)
	ch := mc.ref
	if !ok || ch.Land != land || ch.Slack == "" || !conversation.MirrorsOut(ch.Mirror) {
		return projection{}, false
	}
	text := bead.Fields[conversation.BeadText]
	if text == "" {
		text = bead.Title
	}
	files := withSnapLink(decodeFileRefs(bead.Fields[conversation.BeadFiles]), bead.Fields[conversation.BeadSnap])
	if strings.TrimSpace(text) == "" && len(files) == 0 {
		return projection{}, false
	}
	if len(files) > 0 && bead.Fields[conversation.BeadText] == "" {
		text = "" // the title of a file-only message only names its files
	}
	author, persona := commonsMessageFromEvent(bead).slackAuthor()
	name, slackID := c.bot.commonsAuthor(ctx, author)
	p := projection{id: bead.ID, ch: ch, chChanged: mc.changed, msg: conversation.Message{
		BeadID: bead.ID,
		Text:   viaCommonsWithFiles(name, text, files),
		Author: conversation.Principal{ID: author, Name: name},
		Origin: conversation.SurfaceCommons,
	}, attributed: viaCommonsWithFiles(name, text, files), words: commonsWords(text, files), name: personaName(name), attributedLocal: viaCommonsWithFiles(emailLocal(author), text, files),
		text: text, files: files, local: emailLocal(author)}
	if persona {
		if as := c.bot.commonsPersona(ctx, name, slackID); as != nil {
			p.msg.Text, p.msg.As = p.words, as
		}
	}
	if head := bead.Fields[conversation.BeadThreadID]; head != "" {
		root, ok := c.slackRoot(ctx, head)
		if !ok {
			// The thread lives only in the Commons (its root never reached
			// Slack), so there is nothing to reply under.
			c.logger.Debug("commons-mirror: thread root has no Slack copy", "id", bead.ID, "thread", head)
			return projection{}, false
		}
		p.thread = &conversation.Thread{Head: head, Refs: []conversation.Ref{root}}
	}
	return p, true
}

// planGlass is plan for a Glass nudge's copy in its agent's thread: a reply
// under the Slack copy of the thread's root (the agent's wish thread, or its
// card's), posted on the Slack surface alone (a ChannelRef with no channel
// bead), so the channel's mirror mode does not gate it and the Commons, which
// has the bead already, is not written again. Only a person's own write is
// posted: the copy names them, and a message any other writer set
// origin=glass on would put words in Slack as theirs.
func (c *CommonsMirror) planGlass(ctx context.Context, bead *BeadEvent, land string) (projection, bool) {
	m := commonsMessageFromEvent(bead)
	head := bead.Fields[conversation.BeadThreadID]
	text := bead.Fields[conversation.BeadText]
	if head == "" || strings.TrimSpace(text) == "" {
		return projection{}, false
	}
	if !m.writtenByAuthor() {
		c.logger.Warn("commons-mirror: not posting a Glass nudge no person is shown to have written",
			"id", bead.ID, "created_by", bead.CreatedBy, "author", bead.Fields[conversation.BeadAuthor])
		return projection{}, false
	}
	root, ok := c.slackRoot(ctx, head)
	if !ok {
		c.logger.Debug("commons-mirror: the Glass nudge's thread has no Slack copy", "id", bead.ID, "thread", head)
		return projection{}, false
	}
	author := m.author()
	name, _ := c.bot.commonsAuthor(ctx, author)
	said := viaGlass(name, text)
	return projection{
		id:     bead.ID,
		ch:     conversation.ChannelRef{Land: land, Slack: root.Channel},
		thread: &conversation.Thread{Head: head, Refs: []conversation.Ref{root}},
		msg: conversation.Message{
			BeadID: bead.ID,
			Text:   said,
			Author: conversation.Principal{ID: author, Name: name},
			Origin: conversation.OriginGlass,
		},
		attributed: said, attributedLocal: viaGlass(emailLocal(author), text),
		text: text, local: emailLocal(author), glass: true,
	}, true
}

// viaGlass is a Glass nudge's text as the Slack mirror posts it into the
// agent's thread: who said it, and that it reached the agent through the
// Glass. Both are escaped, as viaCommons's are.
func viaGlass(name, text string) string {
	name = strings.ReplaceAll(strings.TrimSpace(name), "*", "")
	if name == "" {
		name = "Someone"
	}
	return "*" + escapeSlackMrkdwn(name) + "* via the Glass: " + escapeSlackMrkdwn(strings.TrimSpace(text))
}

// post sends p through the Router, which stores the Slack ref on the bead. It
// reports whether the message reached Slack.
//
// The files it can fetch are uploaded with the message. Slack posts an upload
// as the bot itself, so such a post is attributed in its text ("*Name* (via
// Commons)") rather than shown as its author, and the rest of its files are
// links under its words. An upload has no message ts to store (the Router
// says so), so the sweep finds the copy in Slack later and stores its ref.
func (c *CommonsMirror) post(ctx context.Context, p projection) bool {
	msg := p.msg
	if got, linked := c.files.Fetch(ctx, p.ch.Land, p.ch.ID, p.files); len(got) > 0 {
		msg.Files, msg.As = got, nil
		msg.Text = viaCommonsWithFiles(msg.Author.Name, p.text, linked)
	}
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			c.logger.Warn("commons-mirror: rate limiter wait failed, dropping", "id", p.id, "error", err)
			return false
		}
	}
	var d conversation.Delivery
	var err error
	if p.thread != nil {
		d, err = c.router.Reply(ctx, p.ch, *p.thread, msg, false)
	} else {
		d, err = c.router.Post(ctx, p.ch, msg)
	}
	if err != nil {
		c.logger.Warn("commons-mirror: post failed", "id", p.id, "channel", p.ch.Slack, "error", err)
		return false
	}
	ref, posted := d.Ref(conversation.SurfaceSlack)
	if ferr := d.Failed[conversation.SurfaceSlack]; ferr != nil {
		if !posted {
			c.logger.Warn("commons-mirror: post failed", "id", p.id, "channel", p.ch.Slack, "error", ferr)
			return false
		}
		// The copy is in Slack but its ref did not reach the bead. Stamp it
		// again here; if that fails too, the sweep finds the copy in Slack.
		c.stampRef(ctx, p.id, ref)
	}
	c.logger.Info("commons-mirror: posted commons message to Slack",
		"id", p.id, "channel", ref.Channel, "ts", ref.TS, "reply", p.thread != nil, "uploaded", len(msg.Files))
	return posted
}

// stampRef stores a Slack copy's ref on its message bead.
func (c *CommonsMirror) stampRef(ctx context.Context, id string, ref conversation.Ref) bool {
	stamp := map[string]string{beadSlackChannel: ref.Channel, beadSlackTS: ref.TS}
	err := c.bot.retryTransientDaemon(ctx, "mirror-ref", func() error {
		return c.bot.daemon.UpdateBeadFields(ctx, id, stamp)
	})
	if err != nil {
		c.logger.Warn("commons-mirror: storing the Slack ref failed", "id", id, "channel", ref.Channel, "ts", ref.TS, "error", err)
		return false
	}
	return true
}

// Sweep posts origin=commons and origin=glass messages from the last
// commonsMirrorSweepWindow that never reached Slack: the safety net for a
// message written while the bridge was down, as sweepCommonsInbound is for
// the inbound call.
//
// A message is posted only when it was written after its channel bead last
// changed. The bead does not record when its mirror or slack_channel_id was
// set, and the daemon's event log is best-effort, so its updated_at is the
// bound: any later binding change moved it. A message said while the channel
// was dark (mirror off, or bound elsewhere) stays dark when the mirror is
// turned on. An unrelated edit (a rename, a join) also holds back a message
// written before it; that message stays in the Commons only, as it would
// without the sweep.
//
// A message with no Slack ref may still have been posted (the ref stamp
// failed, or the bridge stopped in between), so before posting one Sweep
// looks in Slack for the bot's copy and, when it finds one, stores its ref
// instead. When Slack cannot be read the message waits for the next sweep: a
// late copy is better than two. It returns how many it posted.
//
// A Glass nudge's copy is not held to its channel's last change: it goes to
// its agent's Slack thread whatever the channel's mirror mode (planGlass).
func (c *CommonsMirror) Sweep(ctx context.Context, now time.Time) int {
	if c == nil {
		return 0
	}
	channels := map[string]*mirrorChannel{}
	listed := map[string]bool{}
	n := 0
	for _, origin := range []string{conversation.SurfaceCommons, conversation.OriginGlass} {
		n += c.sweepOrigin(ctx, now, origin, channels, listed)
	}
	return n
}

// sweepOrigin is Sweep for the messages of one origin. It returns how many it
// posted.
func (c *CommonsMirror) sweepOrigin(ctx context.Context, now time.Time, origin string, channels map[string]*mirrorChannel, listed map[string]bool) int {
	n := 0
	for page := range commonsMirrorSweepPages {
		res, err := c.bot.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:        []string{"message"},
			Statuses:     []string{"closed"},
			Fields:       map[string]string{conversation.BeadOrigin: origin},
			CreatedAfter: now.Add(-commonsMirrorSweepWindow),
			Sort:         "created_at", // oldest first: a new message only appends
			Limit:        commonsMirrorSweepPageSize,
			Offset:       page * commonsMirrorSweepPageSize,
		})
		if err != nil || res == nil {
			if err != nil {
				c.logger.Warn("commons-mirror: sweep list failed", "origin", origin, "error", err)
			}
			return n
		}
		for _, d := range res.Beads {
			if d == nil || d.Status != "closed" || listed[d.ID] {
				continue
			}
			listed[d.ID] = true
			bead := &BeadEvent{ID: d.ID, Type: d.Type, Title: d.Title, Status: d.Status, CreatedBy: d.CreatedBy, Labels: d.Labels, Fields: d.Fields}
			if !mirrorCandidate(bead) || d.CreatedAt.IsZero() {
				continue // no creation time: it cannot be placed after its channel's change
			}
			if now.Sub(d.CreatedAt) < commonsSweepGrace {
				continue // fresh: its SSE projection may still be on the way
			}
			if c.sweepOne(ctx, bead, d.CreatedAt, channels) {
				n++
			}
		}
		if len(res.Beads) < commonsMirrorSweepPageSize {
			return n
		}
	}
	c.logger.Warn("commons-mirror: sweep stopped at its page cap; older messages wait for the next sweep",
		"origin", origin, "pages", commonsMirrorSweepPages, "page_size", commonsMirrorSweepPageSize)
	return n
}

// sweepOne is Sweep for one message, written at created. It reports whether
// it posted.
func (c *CommonsMirror) sweepOne(ctx context.Context, bead *BeadEvent, created time.Time, channels map[string]*mirrorChannel) bool {
	if !c.claim(bead.ID, false) {
		return false
	}
	defer c.release(bead.ID)
	p, ok := c.plan(ctx, bead, channels)
	if !ok {
		return false
	}
	if !p.glass && (p.chChanged.IsZero() || !created.After(p.chChanged)) {
		// Said before the channel last changed: perhaps while it was dark.
		c.logger.Debug("commons-mirror: sweep skips a message older than its channel's last change",
			"id", p.id, "channel_bead", p.ch.ID, "created", created, "channel_changed", p.chChanged)
		return false
	}
	if !c.sweepTurn(bead.ID) {
		return false
	}
	ref, found, err := c.slackCopy(ctx, p, created)
	if err != nil {
		c.logger.Warn("commons-mirror: sweep could not read Slack, not posting", "id", p.id, "channel", p.ch.Slack, "error", err)
		return false
	}
	if found {
		if c.stampRef(ctx, p.id, ref) {
			c.logger.Info("commons-mirror: sweep stored a lost Slack ref", "id", p.id, "channel", ref.Channel, "ts", ref.TS)
		}
		return false
	}
	if !c.post(ctx, p) {
		return false
	}
	c.logger.Info("commons-mirror: sweep posted an unmirrored commons message", "id", p.id)
	return true
}

// slackCopy looks in Slack for the bot's post of p made since since: in the
// thread for a reply, else in the channel. It is the sweep's double-post
// guard, and knows a copy in either form (isCopyOf). A copy whose ts another
// message bead already holds is that message's, so two messages with the same
// words are not both stamped with one copy.
func (c *CommonsMirror) slackCopy(ctx context.Context, p projection, since time.Time) (conversation.Ref, bool, error) {
	api := c.bot.api
	if api == nil {
		return conversation.Ref{}, false, errors.New("no Slack client")
	}
	channel := p.ch.Slack
	if p.thread != nil {
		channel = p.thread.Refs[0].Channel
	}
	oldest := slackTSAt(since.Add(-time.Minute)) // Slack's clock and the daemon's differ
	cursor := ""
	for range commonsMirrorLookupPages {
		var msgs []slack.Message
		var more bool
		var next string
		var err error
		if p.thread != nil {
			msgs, more, next, err = api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
				ChannelID: channel, Timestamp: p.thread.Refs[0].TS, Oldest: oldest, Cursor: cursor, Limit: 200,
			})
		} else {
			var res *slack.GetConversationHistoryResponse
			res, err = api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
				ChannelID: channel, Oldest: oldest, Cursor: cursor, Limit: 200,
			})
			if res != nil {
				msgs, more, next = res.Messages, res.HasMore, res.ResponseMetaData.NextCursor
			}
		}
		if err != nil {
			return conversation.Ref{}, false, fmt.Errorf("read %s: %w", channel, err)
		}
		for _, m := range msgs {
			byBot := m.BotID != "" || (c.bot.botUserID != "" && m.User == c.bot.botUserID)
			if !byBot || !p.isCopyOf(m) {
				continue
			}
			held, err := c.heldRef(ctx, p, channel, m.Timestamp)
			if err != nil {
				return conversation.Ref{}, false, err
			}
			if !held {
				return conversation.Ref{Surface: conversation.SurfaceSlack, Channel: channel, TS: m.Timestamp}, true, nil
			}
		}
		if !more || next == "" {
			return conversation.Ref{}, false, nil
		}
		cursor = next
	}
	// More history than the guard reads: assume the copy is there.
	return conversation.Ref{}, false, fmt.Errorf("read %s: more than %d pages since %s", channel, commonsMirrorLookupPages, oldest)
}

// isCopyOf reports whether m, a bot's post, is a Slack copy of p: the
// attributed text (under the directory's name or the email fallback), or the
// words alone under p's author's name. The name keeps a short message ("ok")
// from matching another author's copy. A copy that uploaded some of p's files
// shows them on m and links only the others, so its text is the attributed
// text with those links.
func (p projection) isCopyOf(m slack.Message) bool {
	text := normalizeSlackText(m.Text)
	if text == normalizeSlackText(p.attributed) || (p.attributedLocal != "" && text == normalizeSlackText(p.attributedLocal)) {
		return true
	}
	if linked, ok := p.linkedBeside(m.Files); ok {
		if text == normalizeSlackText(viaCommonsWithFiles(p.msg.Author.Name, p.text, linked)) ||
			(p.local != "" && text == normalizeSlackText(viaCommonsWithFiles(p.local, p.text, linked))) {
			return true
		}
	}
	return p.name != "" && strings.TrimSpace(m.Username) == p.name && text == normalizeSlackText(p.words)
}

// linkedBeside is p's files that are not among shared, the files a Slack post
// carries; false when the post carries none of p's files.
func (p projection) linkedBeside(shared []slack.File) ([]fileRef, bool) {
	if len(shared) == 0 || len(p.files) == 0 {
		return nil, false
	}
	names := map[string]bool{}
	for _, f := range shared {
		names[f.Name] = true
	}
	var linked []fileRef
	for _, f := range p.files {
		if !names[f.Name] {
			linked = append(linked, f)
		}
	}
	return linked, len(linked) < len(p.files)
}

// heldRef reports whether a message bead other than p's already holds the
// Slack copy (channel, ts) as its ref.
func (c *CommonsMirror) heldRef(ctx context.Context, p projection, channel, ts string) (bool, error) {
	res, err := c.bot.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Labels: []string{"project:" + p.ch.Land},
		Fields: map[string]string{beadSlackChannel: channel, beadSlackTS: ts},
		Limit:  2,
	})
	if err != nil {
		return false, fmt.Errorf("look up the holder of %s/%s: %w", channel, ts, err)
	}
	if res == nil {
		return false, fmt.Errorf("look up the holder of %s/%s: no result", channel, ts)
	}
	for _, d := range res.Beads {
		if d != nil && d.ID != p.id {
			return true, nil
		}
	}
	return false, nil
}

// slackLink matches a Slack link, <url> or <url|label>.
var slackLink = regexp.MustCompile(`<([^<>|]+)(?:\|([^<>]*))?>`)

// normalizeSlackText is the form two copies of one message share: Slack may
// wrap a bare URL in a link, so every link reads as its label (else its URL),
// and whitespace runs are one space.
func normalizeSlackText(s string) string {
	s = slackLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := slackLink.FindStringSubmatch(m)
		if sub[2] != "" {
			return sub[2]
		}
		return sub[1]
	})
	return strings.Join(strings.Fields(s), " ")
}

// slackTSAt is t as a Slack ts.
func slackTSAt(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10) + ".000000"
}

func (c *CommonsMirror) cachedChannel(ctx context.Context, id string, channels map[string]*mirrorChannel) (mirrorChannel, bool) {
	if channels == nil {
		return c.channel(ctx, id)
	}
	if ch, ok := channels[id]; ok {
		if ch == nil {
			return mirrorChannel{}, false
		}
		return *ch, true
	}
	ch, ok := c.channel(ctx, id)
	if !ok {
		channels[id] = nil
		return mirrorChannel{}, false
	}
	channels[id] = &ch
	return ch, true
}

// channel reads a channel bead. An archived channel, or one with no single
// land, is not mirrored.
func (c *CommonsMirror) channel(ctx context.Context, id string) (mirrorChannel, bool) {
	b, err := c.bot.daemon.GetBead(ctx, id)
	if err != nil || b == nil || b.Type != "channel" || b.Status == "closed" {
		if err != nil {
			c.logger.Warn("commons-mirror: reading channel bead failed", "channel", id, "error", err)
		}
		return mirrorChannel{}, false
	}
	lands := labelLands(b.Labels)
	if len(lands) != 1 {
		return mirrorChannel{}, false
	}
	return mirrorChannel{ref: conversation.ChannelRef{
		ID:     b.ID,
		Land:   lands[0],
		Slack:  b.Fields[conversation.ChannelSlackID],
		Mirror: b.Fields["mirror"],
	}, changed: b.UpdatedAt}, true
}

// slackRoot returns where a thread's root is shown in Slack.
func (c *CommonsMirror) slackRoot(ctx context.Context, head string) (conversation.Ref, bool) {
	root, err := c.bot.daemon.GetBead(ctx, head)
	if err != nil || root == nil {
		return conversation.Ref{}, false
	}
	return slackCopyOf(root.Fields)
}

// claim reserves id for one projection at a time, SSE or sweep. An SSE
// projection also takes it once only (dedup on reconnect); the sweep may retry
// a message an SSE projection failed to post. Either marks it seen, so a late
// SSE replay does not post it again.
func (c *CommonsMirror) claim(id string, sse bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[id] || (sse && c.seen[id]) {
		return false
	}
	c.seen[id], c.inflight[id] = true, true
	return true
}

func (c *CommonsMirror) release(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, id)
}

// sweepTurn counts one sweep attempt at id, and reports false once it has had
// commonsMirrorSweepTries.
func (c *CommonsMirror) sweepTurn(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sweeps[id] >= commonsMirrorSweepTries {
		return false
	}
	c.sweeps[id]++
	return true
}
