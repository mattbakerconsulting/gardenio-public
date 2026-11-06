// Package conversation is the seam between the bridge's handlers and the
// places a conversation is shown (Slack today, the Commons next).
//
// Beads are the record of a conversation: every durable thing said in a land is
// a message, card or thread-head bead. A Surface is one projection of those
// beads (post, reply, update in place, delete, react, DM, ephemeral, modal).
// Handlers call the Router, which writes the bead and then projects it to every
// surface bound to the channel. Inbound, each surface turns its own events into
// one Inbound value.
//
// Design: gardenio/docs/superpowers/specs/2026-10-19-lands-commons-design.md
// (sections 2 and 3). This package depends on beadsapi only; Block Kit and the
// slack-go client stay inside the Slack surface in internal/bridge.
package conversation

import (
	"context"
	"errors"
	"strings"
)

// Surface names.
const (
	SurfaceSlack   = "slack"
	SurfaceCommons = "commons"
)

// OriginGlass is the origin of the message a Glass nudge writes into its
// agent's thread (gardenhub.GlassOrigin): a person's words the agent already
// has, so no inbound path delivers it, and the bridge's Commons mirror posts
// it into the agent's Slack thread (internal/bridge/commons_mirror.go).
const OriginGlass = "glass"

// Mirror modes of a channel bead (`mirror` field).
const (
	MirrorOff  = "off"
	MirrorOut  = "out"  // Commons → Slack
	MirrorBoth = "both" // Commons ↔ Slack
)

// MirrorsOut reports whether a channel bead's mirror mode copies Commons
// messages into its Slack channel: out or both. Unset is off, never both: a
// slack_channel_id alone must not open a copy either way (gd-t3s76tjnz9).
func MirrorsOut(mode string) bool { return mode == MirrorOut || mode == MirrorBoth }

// MirrorsIn reports whether a channel bead's mirror mode copies Slack messages
// into the Commons: only both. Unset is off.
func MirrorsIn(mode string) bool { return mode == MirrorBoth }

// Kind is a message bead's `kind` field.
type Kind string

const (
	KindText   Kind = "text"
	KindCard   Kind = "card"
	KindNotice Kind = "notice"
	KindAsk    Kind = "ask"
)

// Errors a Surface returns for a request it cannot take. Surfaces wrap them, so
// callers test with errors.Is.
var (
	ErrEmptyMessage = errors.New("conversation: message has no text, subject or files")
	ErrNoChannel    = errors.New("conversation: channel has no id on this surface")
	ErrNoRef        = errors.New("conversation: empty ref")
	ErrNoRecipient  = errors.New("conversation: principal has no id on this surface")
	ErrNoTrigger    = errors.New("conversation: trigger is not from this surface")
	ErrUnsupported  = errors.New("conversation: not supported by this surface")
)

// Ref names one rendered copy of a message on one surface.
type Ref struct {
	Surface string // "slack" | "commons"
	Channel string // surface channel id (Slack C…/D…; Commons channel bead id)
	TS      string // surface message id (Slack ts; Commons message bead id)
}

// IsZero reports whether r names no message.
func (r Ref) IsZero() bool { return r.Channel == "" || r.TS == "" }

// ChannelRef names a channel: its bead and the surface-native ids it is bound
// to. ID is empty for a Slack channel that has no channel bead yet (a land with
// no Commons), in which case Slack is the only surface it projects to.
type ChannelRef struct {
	ID     string // channel bead id
	Land   string // project
	Slack  string // slack_channel_id
	Mirror string // off | out | both ("" = off)
}

// For returns the channel's id on the named surface: the Slack channel for
// Slack, the channel bead id for every other surface.
func (c ChannelRef) For(surface string) string {
	if surface == SurfaceSlack {
		return c.Slack
	}
	return c.ID
}

// Principal is a person or an agent.
type Principal struct {
	ID        string // "oidc:<email>" | "agent:<name>"
	Name      string // display name
	SlackUser string // Slack user id, when the account link resolves one
}

// File is an attachment on a message.
type File struct {
	Name     string
	Title    string
	MimeType string
	AltText  string // accessibility alt text, for images
	Data     []byte
}

// Message is what the core asks a surface to show. Cards carry only the
// subject; each surface renders the subject bead itself (D1), so the Block Kit
// builders stay inside the Slack surface.
type Message struct {
	BeadID    string // the message/card bead this is a copy of ("" for unrecorded notices)
	Kind      Kind   // "" reads as KindText
	SubjectID string // Card only
	Text      string // markdown; also the fallback/notification text (rubric U12)
	Author    Principal
	Files     []File
	// Detail, Footnote and Rich are a Notice's face. Detail is the markdown
	// body; Text stays the one-line notification/fallback. Rich asks for the
	// full face (Slack: a section block, plus a context block for Footnote)
	// rather than the terse face (Slack: a plain message of Detail alone,
	// with Text not sent). A notice with no Detail is Text alone.
	Detail   string
	Footnote string
	Rich     bool
	// Fields are extra fields written on the message bead, for a face the
	// Commons renders from the bead (a death notice's cause, a storm roll-up's
	// count). They never override the router's own fields, and the Slack
	// surface ignores them.
	Fields map[string]string
	// Origin is the surface the message was first said on ("" or "cli" for an
	// agent or the bridge). The router never projects a message back to its
	// origin surface (mirror loop guard).
	Origin string
	// As, when set, is who the message shows as on a surface that can post
	// under another name and avatar (Slack: username and icon_url, which need
	// the chat:write.customize scope). The poster stays the bot, so the
	// mirror loop guard still knows its own post. A caller sets it only where
	// the surface will honor it: Text then need not name the author.
	As *Persona
}

// Persona is the name and avatar a message shows as (Message.As).
type Persona struct {
	Name    string
	IconURL string // "" leaves the poster's own avatar
}

// KindOrText returns m.Kind, with "" read as KindText.
func (m Message) KindOrText() Kind {
	if m.Kind == "" {
		return KindText
	}
	return m.Kind
}

// Empty reports whether m has nothing a surface could show.
func (m Message) Empty() bool {
	return strings.TrimSpace(m.Text) == "" && m.SubjectID == "" && len(m.Files) == 0
}

// Trigger carries what a surface needs to answer one inbound event: Slack's
// trigger_id and the channel/thread the event came from; the Commons' request
// id.
type Trigger struct {
	Surface   string
	TriggerID string // Slack trigger_id (modals)
	Channel   string // surface channel the event came from (ephemerals)
	ThreadTS  string // thread the event came from, when it was in one
	RequestID string // Commons request id
}

// Surface is one place a conversation is shown.
type Surface interface {
	Name() string
	Post(ctx context.Context, ch ChannelRef, m Message) (Ref, error)
	Reply(ctx context.Context, thread Ref, m Message, broadcast bool) (Ref, error)
	Update(ctx context.Context, at Ref, m Message) error
	Delete(ctx context.Context, at Ref) error
	React(ctx context.Context, at Ref, reaction string, on bool) error
	DM(ctx context.Context, to Principal, m Message) (Ref, error)
	// Ephemeral and OpenModal answer an Inbound from this surface; Trigger
	// carries what the surface needs (Slack trigger_id / response channel,
	// Commons request id).
	Ephemeral(ctx context.Context, t Trigger, to Principal, m Message) error
	OpenModal(ctx context.Context, t Trigger, v Modal) error
}
