package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Card placement (design section 2.5). One function decides where a subject's
// card lives, and both surfaces call it: the bridge before it routes a card,
// the Commons when it renders one. It replaces the Slack-only routing in
// Bot.resolveChannel.
//
//  1. A subject bound to a thread (thread_id) gets its card in that thread's
//     channel, as a reply in that thread.
//  2. Else the land's open channel whose channel_roles holds the subject's role.
//  3. Else the land's default channel for the subject: #machines for agents
//     when the land has one (single voice, steward amendment §2), else #crew;
//     #decisions for decisions, #escalations for escalations.
//
// Decisions and escalations addressed to a person get a second card in that
// person's DM channel (DM-first); that card is placed by the DM step, not here.
// For an owner-audience escalation, EscalationAddressees (escalation routing
// D9) says whose DM that is, from its held_by lane: the operator or the
// escalation's own land's owners, and nobody for an agent's lane.

// ChannelReader is the slice of the beads daemon client placement reads
// through. *beadsapi.Client satisfies it; the Commons passes a client on the
// viewer's own bearer, so placement never reads past the viewer's land.
type ChannelReader interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// Channel bead fields placement reads (design section 2.2).
const (
	ChannelName    = "name"
	ChannelKind    = "channel_kind"
	ChannelRoles   = "channel_roles"
	ChannelSlackID = "slack_channel_id"
	ChannelMirror  = "mirror"
)

// Default channel kinds (channel_kind) of a land.
const (
	ChannelKindCrew        = "crew"
	ChannelKindDecisions   = "decisions"
	ChannelKindEscalations = "escalations"
	// ChannelKindMachines is a land's machine channel (steward amendment
	// 2026-10-23 §2, single voice): what agents, the controller and the
	// bridge say for no person in particular, kept out of #general. It is
	// never mirrored to Slack.
	ChannelKindMachines = "machines"
)

// ErrUnplaced is returned when the land has no channel for the card: no thread
// binding, no channel_roles match and no default channel. A land with no
// Commons yet has no channel beads at all; the caller keeps its Slack routing.
var ErrUnplaced = errors.New("conversation: no channel in the land for this card")

// Subject is what placement needs to know about the bead a card renders.
type Subject struct {
	ID       string
	Type     string // bead type: agent | decision | escalation | ...
	Land     string // project
	Role     string // the agent's role (agent subjects)
	ThreadID string // thread head the subject is bound to
}

// Rules a Placement can come from, in precedence order.
const (
	PlacedByThread  = "thread"
	PlacedByRole    = "channel_roles"
	PlacedByDefault = "default"
)

// Placement is where a subject's card goes.
type Placement struct {
	Channel ChannelRef
	// Thread is the thread to reply in (rule 1), nil for a top-level post.
	Thread *Thread
	// Rule names the rule that placed the card.
	Rule string
}

// SubjectFromBead reads a Subject off the bead a card renders.
func SubjectFromBead(b *beadsapi.BeadDetail) Subject {
	s := Subject{ID: b.ID, Type: b.Type, Land: strings.TrimSpace(b.Fields["project"])}
	if s.Land == "" {
		s.Land = singleProjectLabel(b.Labels)
	}
	if b.Type == "agent" {
		s.Role = strings.TrimSpace(b.Fields["role"])
	}
	s.ThreadID = strings.TrimSpace(b.Fields[BeadThreadID])
	if s.ThreadID == "" {
		// Legacy alias of thread_id on agent beads.
		s.ThreadID = strings.TrimSpace(b.Fields["slack_thread_bead_id"])
	}
	return s
}

// defaultChannelKind is rule 3's channel for a subject type.
func defaultChannelKind(subjectType string) string {
	switch subjectType {
	case "decision":
		return ChannelKindDecisions
	case "escalation":
		return ChannelKindEscalations
	default:
		return ChannelKindCrew
	}
}

// Place returns where the card of s goes, by the rules of design section 2.5.
func Place(ctx context.Context, rd ChannelReader, s Subject) (Placement, error) {
	if s.Land == "" {
		return Placement{}, ErrNoLand
	}
	if s.ThreadID != "" {
		p, ok, err := placeInThread(ctx, rd, s)
		if err != nil || ok {
			return p, err
		}
	}

	channels, err := landChannels(ctx, rd, s.Land)
	if err != nil {
		return Placement{}, err
	}
	if s.Role != "" {
		for _, ch := range channels {
			if slices.Contains(stringList(ch.Fields[ChannelRoles]), s.Role) {
				return Placement{Channel: ChannelFromBead(ch, s.Land), Rule: PlacedByRole}, nil
			}
		}
	}
	// Rule 3: the channel of the default kind, preferring the one named after
	// it (#crew) when a land has several. An agent goes to #machines when the
	// land has one.
	want := defaultChannelKind(s.Type)
	pick := channelOfKind(channels, want)
	if s.Type == "agent" {
		if m := channelOfKind(channels, ChannelKindMachines); m != nil {
			pick = m
		}
	}
	if pick == nil {
		return Placement{}, fmt.Errorf("%w: land %q has no #%s channel for %s %s", ErrUnplaced, s.Land, want, s.Type, s.ID)
	}
	return Placement{Channel: ChannelFromBead(pick, s.Land), Rule: PlacedByDefault}, nil
}

// channelOfKind is the channel of channels whose channel_kind is kind,
// preferring the one named after it; nil when there is none.
func channelOfKind(channels []*beadsapi.BeadDetail, kind string) *beadsapi.BeadDetail {
	var pick *beadsapi.BeadDetail
	for _, ch := range channels {
		if ch.Fields[ChannelKind] != kind {
			continue
		}
		if ch.Fields[ChannelName] == kind {
			return ch
		}
		if pick == nil {
			pick = ch
		}
	}
	return pick
}

// MachinesChannel returns land's open #machines channel (channel_kind
// machines), or nil when the land has none: every machine post then goes
// where it went before single voice.
func MachinesChannel(ctx context.Context, rd ChannelReader, land string) (*beadsapi.BeadDetail, error) {
	if land == "" {
		return nil, ErrNoLand
	}
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"channel"},
		Labels: []string{"project:" + land},
		Fields: map[string]string{ChannelKind: ChannelKindMachines},
	})
	if err != nil {
		return nil, fmt.Errorf("conversation: #machines of land %s: %w", land, err)
	}
	var open []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b != nil && b.Status != "closed" && singleProjectLabel(b.Labels) == land {
			open = append(open, b)
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].ID < open[j].ID })
	return channelOfKind(open, ChannelKindMachines), nil
}

// placeInThread applies rule 1. The thread's channel is the head's
// channel_id; a head minted before channel_id was stamped is found through its
// Slack thread, which the land's channel beads map by slack_channel_id, and
// in a land with no channel beads the card still goes in the Slack thread.
// ok is false when the head cannot be placed, so the caller falls through to
// rules 2 and 3.
func placeInThread(ctx context.Context, rd ChannelReader, s Subject) (Placement, bool, error) {
	head, err := rd.GetBead(ctx, s.ThreadID)
	if err != nil || head == nil {
		// A dangling binding (pruned head, or one outside the reader's land)
		// is not the card's problem: it is placed as an unbound subject.
		return Placement{}, false, nil
	}
	th := &Thread{Head: head.ID}
	slackCh, slackTS := head.Fields["slack_thread_channel"], head.Fields["slack_thread_ts"]
	if slackCh == "" || slackTS == "" {
		slackCh, slackTS = slackThreadLabel(head.Labels)
	}
	if slackCh != "" && slackTS != "" {
		th.Refs = []Ref{{Surface: SurfaceSlack, Channel: slackCh, TS: slackTS}}
	}

	if chID := strings.TrimSpace(head.Fields[BeadChannelID]); chID != "" {
		ch, err := rd.GetBead(ctx, chID)
		// A thread in another land is not followed: agents speak only in their
		// own land (design section 6.2 rule 4).
		if err == nil && ch != nil && ch.Type == "channel" && singleProjectLabel(ch.Labels) == s.Land {
			return Placement{Channel: ChannelFromBead(ch, s.Land), Thread: th, Rule: PlacedByThread}, true, nil
		}
	}
	if slackCh == "" {
		return Placement{}, false, nil
	}
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"channel"},
		Labels: []string{"project:" + s.Land},
		Fields: map[string]string{ChannelSlackID: slackCh},
		Limit:  1,
	})
	if err != nil {
		return Placement{}, false, fmt.Errorf("conversation: channel of Slack thread %s: %w", slackCh, err)
	}
	if len(res.Beads) > 0 {
		return Placement{Channel: ChannelFromBead(res.Beads[0], s.Land), Thread: th, Rule: PlacedByThread}, true, nil
	}
	ch := ChannelRef{Land: s.Land, Slack: slackCh}
	return Placement{Channel: ch, Thread: th, Rule: PlacedByThread}, true, nil
}

// landChannels returns the land's open channels, ordered by name so a role in
// the channel_roles of two channels always places in the same one.
func landChannels(ctx context.Context, rd ChannelReader, land string) ([]*beadsapi.BeadDetail, error) {
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"channel"},
		Labels: []string{"project:" + land},
	})
	if err != nil {
		return nil, fmt.Errorf("conversation: channels of land %s: %w", land, err)
	}
	var out []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		// Closed is archived; dm channels never hold a role's cards.
		if b.Status == "closed" || b.Fields[ChannelKind] == "dm" {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fields[ChannelName] < out[j].Fields[ChannelName] })
	return out, nil
}

// ChannelFromBead is the ChannelRef of a channel bead. land is used when the
// bead's own project label cannot be read.
func ChannelFromBead(b *beadsapi.BeadDetail, land string) ChannelRef {
	if l := singleProjectLabel(b.Labels); l != "" {
		land = l
	}
	return ChannelRef{ID: b.ID, Land: land, Slack: b.Fields[ChannelSlackID], Mirror: b.Fields[ChannelMirror]}
}

// singleProjectLabel returns the one project:<land> value on labels, or "".
func singleProjectLabel(labels []string) string {
	land := ""
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" {
			if land != "" && land != v {
				return ""
			}
			land = v
		}
	}
	return land
}

// slackThreadLabel reads the slack-thread:<channel>:<ts> label of a wish head.
func slackThreadLabel(labels []string) (channel, ts string) {
	for _, l := range labels {
		if rest, ok := strings.CutPrefix(l, "slack-thread:"); ok {
			if c, t, ok := strings.Cut(rest, ":"); ok && c != "" && t != "" {
				return c, t
			}
		}
	}
	return "", ""
}

// stringList decodes a string[] field, which the daemon client hands back as
// its JSON text.
func stringList(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return []string{v}
	}
	return out
}

// PostCard posts (or, for an existing card, re-renders) the card m at p: a
// reply in p.Thread when the subject is bound to one, a top-level post in
// p.Channel otherwise.
func (r *Router) PostCard(ctx context.Context, p Placement, m Message) (Delivery, error) {
	m.Kind = KindCard
	if p.Thread != nil {
		return r.Reply(ctx, p.Channel, *p.Thread, m, false)
	}
	return r.Post(ctx, p.Channel, m)
}
