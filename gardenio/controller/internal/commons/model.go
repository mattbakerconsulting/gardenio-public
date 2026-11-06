package commons

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/kin"
)

// Read-model bounds.
const (
	// channelListMax caps the channel beads read for the channel list, across
	// every land the viewer sees (tens per land, spec 2.7).
	channelListMax = 2000
	// timelineWindow is how many of #general's newest conversation beads the
	// steward node reads (stewardnode.go).
	timelineWindow = 400
	// threadMax caps the replies one thread read pulls.
	threadMax = 500
	// textMax is the rune cap on one message's rendered text.
	textMax = 20000
)

// errNotFound is a channel, thread or message the viewer cannot see. A bead
// outside the viewer's lands is reported exactly like a missing one.
var errNotFound = errors.New("commons: not found")

// Channel is one channel bead (spec 2.2).
type Channel struct {
	ID     string
	Land   string
	Name   string // slug, unique per land
	Title  string // display name
	Kind   string // channel_kind
	Topic  string
	Mirror string
	// Members are who may post in a members-only channel (a user channel or a
	// DM): oidc:<email> or agent:<name>. The default channels carry none.
	Members []string
	// Created is when the channel bead was made; it orders a fan-out's reads
	// (byReadOrder), never the display.
	Created time.Time
}

// byReadOrder orders the channels of a per-channel fan-out (inbox, readUnread):
// every channel but a DM first, then the DMs newest first. A land has a DM for
// each agent that has written to a person, hundreds of them, so the fan-out can
// outrun its read budget; the DMs of agents long gone are then the ones left
// with no last line or badge, not a random few (gd-WUojuuXdbc).
func byReadOrder(a, b Channel) int {
	if ad, bd := a.Kind == "dm", b.Kind == "dm"; ad != bd {
		if ad {
			return 1
		}
		return -1
	} else if ad {
		return b.Created.Compare(a.Created)
	}
	return 0
}

// MembersOnly reports whether only the channel's members post in it (spec
// 2.2.1). Everyone in the land still reads it.
func (c Channel) MembersOnly() bool { return c.Kind == "user" || c.Kind == "dm" }

// HasMember reports whether principal is one of the channel's members.
func (c Channel) HasMember(principal string) bool {
	return principal != "" && slices.Contains(c.Members, principal)
}

// Land is a land and its open channels, in display order.
type Land struct {
	Name     string
	Channels []Channel
}

// Message is one entry of a timeline or thread: a message bead, a card bead or
// a thread (wish) head.
type Message struct {
	ID        string
	Land      string
	Channel   string
	Thread    string // thread head, for a reply
	Kind      string // text | card | notice | ask; "wish" for a thread head
	Head      bool   // a wish head (type=task, label thread)
	Status    string // the wish head's status
	Author    string // oidc:<email> | agent:<name> | slack:<user id> | the creating actor
	Voice     string // kin.Voice*: who it speaks as (kin.Speaker), a squawk its agent's, a Slack reply its person's
	Via       string // kin.Via*: how it came to the thread
	Text      string
	SubjectID string // card only
	At        time.Time
	Deleted   bool
	Files     []File    // attachments: inline images and links
	Snap      *Snap     // a farm snapshot's context, captioned under its files
	Replies   int       // a root's replies (countReplies)
	LastReply time.Time // a root's newest reply
	// Notice is a death report's, crash notice's or storm roll-up's face
	// (notices.go); nil for any other message.
	Notice *Notice
	// PromotedFrom is, on a wish head a mention made of a chat root, that root;
	// PromotedTo is, on the root, its head (best-effort: the head always names
	// the root).
	PromotedFrom string
	PromotedTo   string
	// ImpersonatedBy is the admin who wrote the message while signed in as
	// Author (one-click Become, gd-L8LeYDLACc): fields.created_impersonated_by,
	// which only the daemon stamps, from the verified token, and only on the
	// create. "" for anyone else's.
	ImpersonatedBy string
	// Nonce is the client nonce the message was sent with (sendonce.go), ""
	// for none.
	Nonce string
}

// File is one attachment of a message: where it opens, never its bytes. The
// bridge's Slack mirror records a Slack file's permalink
// (internal/bridge/commons_mirror.go).
type File struct {
	Name  string
	Image bool
	Size  int // bytes; 0 when unknown
	URL   string
	// Src is where a raster image on the file route (files.go) shows inline
	// from: its path under the viewer's base, /garden/c/f/..., so it is always
	// this origin's. Empty for anything else, a Slack permalink included (it
	// opens only signed in to Slack), which stays a link.
	Src string
	// W and H are the image's own size in pixels when the upload recorded it;
	// 0 when not known.
	W, H int
}

// imgMaxH is how tall an inline image is laid out at most, in CSS pixels
// (garden.css .cm-img).
const imgMaxH = 320

// BoxW and BoxH are the size an inline image is laid out at before it loads,
// so the timeline does not move when it does: its own, scaled to imgMaxH
// tall at most. The pane's CSS narrows it to the column, keeping the ratio.
// 0 when the size is not known; the CSS then reserves a fixed box.
func (f File) BoxW() int { w, _ := f.box(); return w }
func (f File) BoxH() int { _, h := f.box(); return h }

func (f File) box() (int, int) {
	if f.W <= 0 || f.H <= 0 {
		return 0, 0
	}
	if f.H <= imgMaxH {
		return f.W, f.H
	}
	return max(1, (f.W*imgMaxH+f.H/2)/f.H), imgMaxH
}

// filesMax caps the attachments one message shows (Slack's per-message limit).
const filesMax = 10

// filesFrom reads the files field of a message bead in land's channel chID, a
// JSON array of {name, mimetype, size, url, width, height}. An entry whose URL
// is not absolute http(s) is dropped, and a malformed field reads as none.
func filesFrom(raw, land, chID string) []File {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var refs []fileRef
	if json.Unmarshal([]byte(raw), &refs) != nil {
		return nil
	}
	var out []File
	for _, f := range refs {
		if len(out) == filesMax {
			break
		}
		u, err := url.Parse(f.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			continue
		}
		name := strings.TrimSpace(f.Name)
		if name == "" {
			name = "file"
		}
		file := File{Name: clipRunes(name, 200), Image: strings.HasPrefix(f.MimeType, "image/"), Size: max(f.Size, 0), URL: f.URL}
		if file.Src = inlineSrc(f.MimeType, u, land, chID); file.Src != "" && f.Width > 0 && f.Height > 0 &&
			f.Width <= imgMaxSide && f.Height <= imgMaxSide {
			file.W, file.H = f.Width, f.Height
		}
		out = append(out, file)
	}
	return out
}

// imgMaxSide bounds a recorded image size; a larger one reads as not known.
const imgMaxSide = 1 << 15

// fileRoute is where the file route's paths start below the base path.
const fileRoute = "/garden/c/f/"

// inlineSrc is the path an attachment shows inline from, or "" when it shows
// as a link: it must be a raster image (inlineTypes) on this Commons's file
// route, filed under the message's own land and channel, which is where the
// route serves it from and whose readers may open it.
func inlineSrc(mimeType string, u *url.URL, land, chID string) string {
	if !inlineTypes[strings.ToLower(strings.TrimSpace(mimeType))] {
		return ""
	}
	p := u.EscapedPath()
	i := strings.Index(p, fileRoute)
	if i < 0 {
		return ""
	}
	segs := strings.Split(p[i+len(fileRoute):], "/")
	if len(segs) != 4 || !validFileID(segs[2]) {
		return ""
	}
	l, err1 := url.PathUnescape(segs[0])
	c, err2 := url.PathUnescape(segs[1])
	n, err3 := url.PathUnescape(segs[3])
	if err1 != nil || err2 != nil || err3 != nil || l != land || c != chID || n == "" || n != fileName(n) {
		return ""
	}
	return p[i:]
}

// channelKindOrder is the display order of a land's channels by kind; a kind
// not listed sorts after them. #machines is listed, so a curious person can
// read it, after the channels people talk in.
var channelKindOrder = map[string]int{"general": 0, "crew": 1, "decisions": 2, "escalations": 3, "user": 4, "dm": 5, kindMachines: 6}

// kindMachines is a land's machine channel (steward amendment 2026-10-23 §2,
// single voice): agent, controller and bridge traffic kept out of #general.
// It is never the default view and counts no unread (unread.go).
const kindMachines = "machines"

func channelFromBead(b *beadsapi.BeadDetail) (Channel, bool) {
	land := landOf(b.Labels)
	if b.Type != typeChannel || land == "" {
		return Channel{}, false
	}
	name := strings.TrimSpace(b.Fields[fieldName])
	if name == "" {
		name = b.ID
	}
	title := strings.TrimSpace(b.Title)
	if title == "" {
		title = name
	}
	// The daemon returns fields.members as the JSON text of the array; anything
	// unreadable is no members.
	var members []string
	if raw := b.Fields[fieldMembers]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &members)
	}
	return Channel{
		ID: b.ID, Land: land, Name: name, Title: title, Kind: b.Fields[fieldChanKind],
		Topic: strings.TrimSpace(b.Description), Mirror: b.Fields[fieldMirror], Members: members,
		Created: b.CreatedAt,
	}, true
}

func sortChannels(chs []Channel) {
	rank := func(k string) int {
		if r, ok := channelKindOrder[k]; ok {
			return r
		}
		return len(channelKindOrder)
	}
	sort.SliceStable(chs, func(i, j int) bool {
		if ri, rj := rank(chs[i].Kind), rank(chs[j].Kind); ri != rj {
			return ri < rj
		}
		return chs[i].Name < chs[j].Name
	})
}

// readLands returns the viewer's lands and their open channels. A land with no
// channel bead has no Commons yet and is not listed.
func readLands(ctx context.Context, rd Reader, sc Scope) ([]Land, error) {
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{typeChannel},
		Statuses: []string{"open"},
		Limit:    channelListMax,
	})
	if err != nil {
		return nil, fmt.Errorf("commons: list channels: %w", err)
	}
	byLand := map[string][]Channel{}
	for _, b := range res.Beads {
		if b == nil || b.Status == "closed" {
			continue
		}
		ch, ok := channelFromBead(b)
		if !ok || !sc.Allows(ch.Land) {
			continue
		}
		byLand[ch.Land] = append(byLand[ch.Land], ch)
	}
	lands := make([]Land, 0, len(byLand))
	for name, chs := range byLand {
		sortChannels(chs)
		lands = append(lands, Land{Name: name, Channels: chs})
	}
	sort.Slice(lands, func(i, j int) bool { return lands[i].Name < lands[j].Name })
	return lands, nil
}

// readChannel resolves {channel} in land: a channel name first, then a channel
// bead id. Archived channels still resolve, so an old link keeps its history.
func readChannel(ctx context.Context, rd Reader, sc Scope, land, ref string) (Channel, error) {
	if !sc.Allows(land) || ref == "" {
		return Channel{}, errNotFound
	}
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{typeChannel},
		Labels: []string{"project:" + land},
		Fields: map[string]string{fieldName: ref},
		Limit:  2,
	})
	if err != nil {
		return Channel{}, readErr("channel "+ref, err)
	}
	// A DM reopens rather than being made again (conversation.EnsureDM); were
	// both a closed and an open one named ref, the open one is the live one.
	var found *Channel
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if ch, ok := channelFromBead(b); ok && ch.Land == land && ch.Name == ref {
			if b.Status != "closed" {
				return ch, nil
			}
			found = cmp.Or(found, &ch)
		}
	}
	if found != nil {
		return *found, nil
	}
	b, err := rd.GetBead(ctx, ref)
	if err != nil {
		return Channel{}, readErr("channel "+ref, err)
	}
	if b == nil {
		return Channel{}, errNotFound
	}
	if ch, ok := channelFromBead(b); ok && ch.Land == land {
		return ch, nil
	}
	return Channel{}, errNotFound
}

// messageFromBead reads a conversation bead. ok is false for a bead that is not
// one: not a message or a wish head, no channel_id, or no single land.
func messageFromBead(b *beadsapi.BeadDetail) (Message, bool) {
	return messageIn(b, "")
}

// messageIn is messageFromBead with a channel for a bead that carries none: a
// reply forwarded before channel_id was stamped belongs to its thread's channel.
func messageIn(b *beadsapi.BeadDetail, channel string) (Message, bool) {
	land := landOf(b.Labels)
	chID := firstNonEmpty(b.Fields[fieldChannelID], channel)
	head := kin.IsWishHead(b)
	if land == "" || chID == "" || (b.Type != typeMessage && !head) {
		return Message{}, false
	}
	m := Message{
		ID: b.ID, Land: land, Channel: chID, Thread: strings.TrimSpace(b.Fields[fieldThreadID]),
		Kind: b.Fields[fieldKind], Head: head,
		SubjectID: b.Fields[fieldSubjectID], At: b.CreatedAt, Deleted: b.Fields[fieldDeleted] == "true",
		ImpersonatedBy: strings.TrimSpace(b.Fields[fieldCreatedImpersonatedBy]),
		Nonce:          b.Fields[fieldNonce],
	}
	if head {
		ask := kin.AskLine(b)
		m.Author, m.Voice, m.Via = ask.Author, ask.Voice, ask.Via
		m.Kind, m.Status, m.Thread = "wish", b.Status, ""
		m.PromotedFrom = strings.TrimSpace(b.Fields[fieldPromotedFrom])
	} else {
		m.Author, m.Voice, m.Via = kin.Speaker(b)
		m.PromotedTo = strings.TrimSpace(b.Fields[fieldPromotedTo])
	}
	if m.Kind == "" {
		m.Kind = "text"
	}
	if !m.Deleted {
		m.Files = filesFrom(b.Fields[fieldFiles], land, chID)
		if len(m.Files) > 0 {
			m.Snap = snapFrom(b.Fields[fieldSnap], m.Files)
		}
		text := firstNonEmpty(b.Fields[fieldText], b.Fields[fieldRootText])
		if text == "" && len(m.Files) == 0 {
			text = b.Title // a file-only message's title only names its files
		}
		m.Text = clipRunes(text, textMax)
		if m.Kind == "notice" {
			m.Notice = noticeFrom(b.Fields)
		}
	}
	return m, true
}

// readThread returns a thread's root and its replies, oldest first. The root
// must be a top-level entry of ch. A chat root a mention promoted reads as
// its wish head's thread, and a head promoted from a chat root has the root's
// words as its first entry, then every reply under either.
func readThread(ctx context.Context, rd Reader, ch Channel, rootID string) (Message, []Message, error) {
	b, err := rd.GetBead(ctx, rootID)
	if err != nil {
		return Message{}, nil, readErr("thread "+rootID, err)
	}
	if b == nil {
		return Message{}, nil, errNotFound
	}
	root, ok := messageFromBead(b)
	if !ok || root.Land != ch.Land || root.Channel != ch.ID || root.Thread != "" {
		return Message{}, nil, errNotFound
	}
	if head, ok := promotedHeadOf(ctx, rd, root); ok {
		adoptRoot(&head, root)
		root = head
	} else if root.PromotedFrom != "" {
		adoptRootOf(ctx, rd, &root)
	}
	ids := []string{root.ID}
	if root.PromotedFrom != "" {
		ids = append(ids, root.PromotedFrom)
	}
	var out []Message
	for _, id := range ids {
		res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Labels: []string{"project:" + ch.Land},
			Fields: map[string]string{fieldThreadID: id},
			Sort:   "-created_at", // the newest threadMax: a thread past it shows its latest replies, sorted below
			Limit:  threadMax,
		})
		if err != nil {
			return Message{}, nil, readErr("replies of "+id, err)
		}
		for _, b := range res.Beads {
			if b == nil {
				continue
			}
			// Only messages are replies; the thread's tasks and agents are its
			// work, not its conversation.
			m, ok := messageIn(b, ch.ID)
			if !ok || m.Head || m.Land != ch.Land || m.Channel != ch.ID || m.Thread != id {
				continue
			}
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if len(out) > threadMax {
		out = out[len(out)-threadMax:]
	}
	return root, out, nil
}

// adoptRoot gives a promoted wish head the chat root's place and words: who
// said it, when, the whole text and the files. The head carries only a
// clipped copy of the text, and was written by whoever mentioned.
func adoptRoot(head *Message, root Message) {
	if root.Deleted {
		return
	}
	head.Author, head.Voice, head.Via, head.At = root.Author, root.Voice, root.Via, root.At
	if root.Text != "" {
		head.Text = root.Text
	}
	head.Files, head.Snap = root.Files, root.Snap
}

// adoptRootOf reads a promoted head's chat root and adopts it. A root gone or
// out of the head's channel leaves the head as it is.
func adoptRootOf(ctx context.Context, rd Reader, head *Message) {
	b, err := rd.GetBead(ctx, head.PromotedFrom)
	if err != nil || b == nil {
		return
	}
	if root, ok := messageFromBead(b); ok && !root.Head && root.Land == head.Land && root.Channel == head.Channel {
		adoptRoot(head, root)
	}
}

// promotedHeadOf returns the wish head a chat root was promoted to: by the
// root's promoted_to, else by a head of its land that names it. ok is false
// for a head, a reply, and a root no mention promoted.
func promotedHeadOf(ctx context.Context, rd Reader, root Message) (Message, bool) {
	if root.Head || root.Thread != "" {
		return Message{}, false
	}
	match := func(b *beadsapi.BeadDetail) (Message, bool) {
		if b == nil {
			return Message{}, false
		}
		head, ok := messageFromBead(b)
		if !ok || !head.Head || head.PromotedFrom != root.ID || head.Land != root.Land || head.Channel != root.Channel {
			return Message{}, false
		}
		return head, true
	}
	if root.PromotedTo != "" {
		if b, err := rd.GetBead(ctx, root.PromotedTo); err == nil {
			if head, ok := match(b); ok {
				return head, true
			}
		}
	}
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"project:" + root.Land, labelThread},
		Fields: map[string]string{fieldPromotedFrom: root.ID},
		Limit:  1,
	})
	if err != nil || res == nil {
		return Message{}, false
	}
	for _, b := range res.Beads {
		if head, ok := match(b); ok {
			return head, true
		}
	}
	return Message{}, false
}

// readMessage returns one conversation bead the viewer may see.
func readMessage(ctx context.Context, rd Reader, sc Scope, id string) (Message, error) {
	b, err := rd.GetBead(ctx, id)
	if err != nil {
		return Message{}, readErr("message "+id, err)
	}
	if b == nil {
		return Message{}, errNotFound
	}
	m, ok := messageFromBead(b)
	if !ok {
		m, ok = replyInRootChannel(ctx, rd, b)
	}
	if !ok || !sc.Allows(m.Land) {
		return Message{}, errNotFound
	}
	if m.Head && m.PromotedFrom != "" {
		adoptRootOf(ctx, rd, &m)
	}
	return m, nil
}

// replyInRootChannel reads a message bead with no channel_id as a reply in its
// thread root's channel, as readThread does (messageIn), so the reply the event
// stream names refetches as the thread shows it. ok is false when the bead has
// no thread, or its root is not a top-level entry of the bead's land.
func replyInRootChannel(ctx context.Context, rd Reader, b *beadsapi.BeadDetail) (Message, bool) {
	head := strings.TrimSpace(b.Fields[fieldThreadID])
	if b.Type != typeMessage || head == "" || head == b.ID || strings.TrimSpace(b.Fields[fieldChannelID]) != "" {
		return Message{}, false
	}
	rb, err := rd.GetBead(ctx, head)
	if err != nil || rb == nil {
		return Message{}, false
	}
	root, ok := messageFromBead(rb)
	if !ok || root.Thread != "" || root.Land != landOf(b.Labels) {
		return Message{}, false
	}
	return messageIn(b, root.Channel)
}

// countReplies gives a top-level message its reply count and last reply time,
// so a root refetched on its own (the event stream, after a reply event)
// moves its "N replies".
func countReplies(ctx context.Context, rd Reader, m *Message) error {
	m.Replies, m.LastReply = 0, time.Time{}
	ids := []string{m.ID}
	if m.Head && m.PromotedFrom != "" {
		ids = append(ids, m.PromotedFrom)
	}
	for _, id := range ids {
		res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Labels: []string{"project:" + m.Land},
			Fields: map[string]string{fieldThreadID: id},
			Limit:  threadMax,
		})
		if err != nil {
			return readErr("replies of "+id, err)
		}
		for _, b := range res.Beads {
			if b == nil {
				continue
			}
			r, ok := messageFromBead(b)
			if !ok || r.Head || r.Land != m.Land || r.Channel != m.Channel || r.Thread != id {
				continue
			}
			m.Replies++
			if r.At.After(m.LastReply) {
				m.LastReply = r.At
			}
		}
	}
	return nil
}

// readErr maps the daemon's refusal of a bead outside the viewer's projects, and
// its not-found, to errNotFound; anything else stays an upstream failure.
func readErr(what string, err error) error {
	var ae *beadsapi.APIError
	if errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden) {
		return errNotFound
	}
	return fmt.Errorf("commons: read %s: %w", what, err)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
