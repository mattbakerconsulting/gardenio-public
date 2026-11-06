package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Store is the slice of the beads daemon client the router writes through.
// *beadsapi.Client satisfies it.
type Store interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// Message bead fields the router reads and writes (design section 2.3).
const (
	BeadChannelID = "channel_id"
	BeadThreadID  = "thread_id"
	BeadAuthor    = "author"
	BeadKind      = "kind"
	BeadSubjectID = "subject_id"
	BeadOrigin    = "origin"
	BeadText      = "text"
	BeadDetail    = "detail"   // a notice's body (Message.Detail)
	BeadFootnote  = "footnote" // a notice's footnote (Message.Footnote)
	// BeadFiles is a message's attachments as a JSON array of
	// {name, mimetype, size, url}: where each file opens, never its bytes.
	// The Slack mirror writes it and the Commons shows it
	// (internal/bridge/commons_mirror.go).
	BeadFiles = "files"
	// BeadSnap is a farm snapshot's context beside its picture in files: a
	// JSON object naming where the camera was and the view's absolute link
	// (internal/commons/snap.go). The Slack mirror links the view.
	BeadSnap = "snap"
)

// maxTitle is the bead title limit; longer text is truncated in the title and
// kept whole in the text field (as gb squawk does).
const maxTitle = 500

// ErrNoLand is returned for a channel with no land. Every conversation bead
// carries exactly one project label: a label-less message is visible to every
// scoped principal, so it would leak across lands (design section 6.2 rule 1).
var ErrNoLand = errors.New("conversation: channel has no land")

// Thread names a thread to reply in: its head bead and the head's copy on each
// surface.
type Thread struct {
	Head string
	Refs []Ref
}

// On returns the thread's root on the named surface. The Commons root is the
// head bead itself, so it needs no stored ref.
func (t Thread) On(surface string, ch ChannelRef) (Ref, bool) {
	for _, r := range t.Refs {
		if r.Surface == surface && !r.IsZero() {
			return r, true
		}
	}
	if surface == SurfaceCommons && t.Head != "" && ch.ID != "" {
		return Ref{Surface: SurfaceCommons, Channel: ch.ID, TS: t.Head}, true
	}
	return Ref{}, false
}

// Delivery is the outcome of one routed message. A surface failure never fails
// the bead write: the bead is the record, Failed names the surfaces whose
// projection must be retried from it ("degrade, never drop").
type Delivery struct {
	BeadID string
	Refs   []Ref
	Failed map[string]error
}

// Ref returns the delivery's copy on the named surface.
func (d Delivery) Ref(surface string) (Ref, bool) {
	for _, r := range d.Refs {
		if r.Surface == surface {
			return r, true
		}
	}
	return Ref{}, false
}

func (d *Delivery) fail(surface string, err error) {
	if d.Failed == nil {
		d.Failed = map[string]error{}
	}
	d.Failed[surface] = err
}

// Router is what handlers call. It writes the bead first, then projects it to
// every surface bound to the channel, and stores each surface's ref back on
// the bead.
type Router struct {
	store    Store
	surfaces []Surface
	logger   *slog.Logger
}

// NewRouter returns a router over store projecting to surfaces.
func NewRouter(store Store, logger *slog.Logger, surfaces ...Surface) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	return &Router{store: store, surfaces: surfaces, logger: logger}
}

// Surface returns the registered surface with that name. Ephemerals and modals
// answer an Inbound on the surface it came from and are never beads (C5), so
// handlers call the surface directly for them.
func (r *Router) Surface(name string) (Surface, bool) {
	for _, s := range r.surfaces {
		if s.Name() == name {
			return s, true
		}
	}
	return nil, false
}

// bound returns the surfaces a message in ch projects to, minus the message's
// origin surface. The Commons is bound whenever the channel has a bead. Slack
// is bound when the channel has a Slack id and its bead mirrors out (out or
// both; unset is off), or when the channel has no bead yet (the land has no
// Commons).
func (r *Router) bound(ch ChannelRef, origin string) []Surface {
	var out []Surface
	for _, s := range r.surfaces {
		name := s.Name()
		if name == origin {
			continue
		}
		switch name {
		case SurfaceSlack:
			if ch.Slack == "" || (ch.ID != "" && !MirrorsOut(ch.Mirror)) {
				continue
			}
		default:
			if ch.ID == "" {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// Post writes m as a top-level message in ch and projects it. A Card is
// upserted: unique per (subject, channel), and re-rendered in place when it
// already exists (D1). When m.BeadID is set the bead already exists (an agent
// or the Commons wrote it) and only the projection runs.
func (r *Router) Post(ctx context.Context, ch ChannelRef, m Message) (Delivery, error) {
	return r.route(ctx, ch, nil, m, false)
}

// Reply writes m as a reply in th and projects it into th on every bound
// surface that has a copy of the thread root.
func (r *Router) Reply(ctx context.Context, ch ChannelRef, th Thread, m Message, broadcast bool) (Delivery, error) {
	if th.Head == "" && len(th.Refs) == 0 {
		return Delivery{}, fmt.Errorf("conversation: reply: %w", ErrNoRef)
	}
	return r.route(ctx, ch, &th, m, broadcast)
}

func (r *Router) route(ctx context.Context, ch ChannelRef, th *Thread, m Message, broadcast bool) (Delivery, error) {
	if ch.Land == "" {
		return Delivery{}, ErrNoLand
	}
	if m.Empty() {
		return Delivery{}, ErrEmptyMessage
	}
	if m.KindOrText() == KindCard && m.SubjectID == "" {
		return Delivery{}, fmt.Errorf("conversation: card without subject: %w", ErrEmptyMessage)
	}

	var existing map[string]string
	if m.BeadID == "" && m.KindOrText() == KindCard {
		card, err := r.findCard(ctx, ch, m.SubjectID)
		if err != nil {
			return Delivery{}, err
		}
		if card != nil {
			m.BeadID, existing = card.ID, card.Fields
		}
	}
	if m.BeadID == "" {
		id, err := r.store.CreateBead(ctx, messageBead(ch, th, m, nil))
		if err != nil {
			return Delivery{}, fmt.Errorf("conversation: write message bead: %w", err)
		}
		m.BeadID = id
	}

	d := Delivery{BeadID: m.BeadID}
	stamp := map[string]string{}
	for _, s := range r.bound(ch, m.Origin) {
		name := s.Name()
		if at, ok := storedRef(name, m.BeadID, existing); ok {
			if err := s.Update(ctx, at, m); err != nil {
				d.fail(name, err)
				continue
			}
			d.Refs = append(d.Refs, at)
			continue
		}
		var ref Ref
		var err error
		if th != nil {
			root, ok := th.On(name, ch)
			if !ok {
				d.fail(name, fmt.Errorf("thread %s has no root on %s: %w", th.Head, name, ErrNoRef))
				continue
			}
			ref, err = s.Reply(ctx, root, m, broadcast)
		} else {
			ref, err = s.Post(ctx, ch, m)
		}
		if err != nil {
			d.fail(name, err)
			continue
		}
		d.Refs = append(d.Refs, ref)
		// A file share has no message ts to store (Slack's upload API returns
		// only the file), so there is no copy to edit later.
		if name != SurfaceCommons && !ref.IsZero() {
			stamp[name+"_channel"] = ref.Channel
			stamp[name+"_ts"] = ref.TS
		}
	}
	if len(stamp) > 0 {
		if err := r.store.UpdateBeadFields(ctx, m.BeadID, stamp); err != nil {
			// The copy exists but its ref is not on the bead: the next update of
			// a card would re-post instead of editing in place. Report it as a
			// failure of every surface whose ref was lost.
			for k := range stamp {
				if name, ok := strings.CutSuffix(k, "_ts"); ok {
					d.fail(name, fmt.Errorf("store ref: %w", err))
				}
			}
		}
	}
	for name, err := range d.Failed {
		r.logger.Warn("conversation: projection failed", "bead", m.BeadID, "surface", name, "error", err)
	}
	return d, nil
}

// UpdateCards re-renders every card whose subject is subjectID, on every
// surface it was projected to. It is what a subject bead's change calls; the
// two cards of a decision (its channel and the addressee's DM) stay in sync
// because both are found here.
func (r *Router) UpdateCards(ctx context.Context, subjectID string) (Delivery, error) {
	if subjectID == "" {
		return Delivery{}, fmt.Errorf("conversation: update cards: %w", ErrEmptyMessage)
	}
	res, err := r.store.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Fields: map[string]string{BeadKind: string(KindCard), BeadSubjectID: subjectID},
	})
	if err != nil {
		return Delivery{}, fmt.Errorf("conversation: find cards of %s: %w", subjectID, err)
	}
	var d Delivery
	for _, card := range res.Beads {
		m := Message{BeadID: card.ID, Kind: KindCard, SubjectID: subjectID, Text: card.Fields[BeadText]}
		for _, s := range r.surfaces {
			at, ok := storedRef(s.Name(), card.ID, card.Fields)
			if !ok {
				continue
			}
			if err := s.Update(ctx, at, m); err != nil {
				d.fail(s.Name(), fmt.Errorf("card %s: %w", card.ID, err))
				continue
			}
			d.Refs = append(d.Refs, at)
		}
	}
	return d, nil
}

// UpdateCard re-renders the card m in place at at, a copy a surface already
// shows, and makes sure the card bead exists and records that copy. It is the
// card path for a handler that holds the copy itself, as the bridge's agent
// cards do from before card beads existed (design section 5.2, slice 2): the
// card is never re-posted, and its bead is upserted with at as its ref.
//
// When m.BeadID is set the caller knows the card bead and that it records at,
// so only the re-render runs. A failed bead read or write never stops the
// re-render ("degrade, never drop"): it is logged and the Delivery has no
// BeadID.
func (r *Router) UpdateCard(ctx context.Context, ch ChannelRef, th *Thread, m Message, at Ref) (Delivery, error) {
	if ch.Land == "" {
		return Delivery{}, ErrNoLand
	}
	if m.SubjectID == "" {
		return Delivery{}, fmt.Errorf("conversation: card without subject: %w", ErrEmptyMessage)
	}
	if at.IsZero() {
		return Delivery{}, fmt.Errorf("conversation: update card of %s: %w", m.SubjectID, ErrNoRef)
	}
	s, ok := r.Surface(at.Surface)
	if !ok {
		return Delivery{}, fmt.Errorf("conversation: no %s surface: %w", at.Surface, ErrUnsupported)
	}
	m.Kind = KindCard
	if m.BeadID == "" {
		m.BeadID = r.upsertCardAt(ctx, ch, th, m, at)
	}
	d := Delivery{BeadID: m.BeadID}
	if err := s.Update(ctx, at, m); err != nil {
		d.fail(at.Surface, err)
		r.logger.Warn("conversation: projection failed", "bead", m.BeadID, "surface", at.Surface, "error", err)
		return d, nil
	}
	d.Refs = append(d.Refs, at)
	return d, nil
}

// RecordCard makes sure the card bead of (m.SubjectID, ch) exists and records
// at, a copy the caller already posted on that surface itself, without
// touching the surface. It is for a copy the router cannot post, such as the
// bridge's DM-first decision card, which goes out through the Slack DM path.
// It returns the card bead's id, or "" when the bead could not be read or
// written ("degrade, never drop": the copy is posted either way).
func (r *Router) RecordCard(ctx context.Context, ch ChannelRef, th *Thread, m Message, at Ref) string {
	if ch.Land == "" || m.SubjectID == "" || at.IsZero() {
		return ""
	}
	m.Kind = KindCard
	return r.upsertCardAt(ctx, ch, th, m, at)
}

// upsertCardAt returns the id of the card bead of (m.SubjectID, ch), creating
// it with at as its ref when there is none and re-stamping at on one that
// records another copy. "" when the bead could not be read or written.
func (r *Router) upsertCardAt(ctx context.Context, ch ChannelRef, th *Thread, m Message, at Ref) string {
	stamp := map[string]string{}
	if at.Surface != SurfaceCommons {
		stamp[at.Surface+"_channel"] = at.Channel
		stamp[at.Surface+"_ts"] = at.TS
	}
	card, err := r.findCard(ctx, ch, m.SubjectID)
	if err != nil {
		r.logger.Warn("conversation: card bead lookup failed", "subject", m.SubjectID, "error", err)
		return ""
	}
	if card == nil {
		id, err := r.store.CreateBead(ctx, messageBead(ch, th, m, stamp))
		if err != nil {
			r.logger.Warn("conversation: card bead write failed", "subject", m.SubjectID, "error", err)
			return ""
		}
		return id
	}
	if stored, ok := storedRef(at.Surface, card.ID, card.Fields); len(stamp) > 0 && (!ok || stored != at) {
		if err := r.store.UpdateBeadFields(ctx, card.ID, stamp); err != nil {
			r.logger.Warn("conversation: card ref write failed", "bead", card.ID, "surface", at.Surface, "error", err)
		}
	}
	return card.ID
}

// FindCard returns the card bead of subjectID in ch, or nil when it has none.
func (r *Router) FindCard(ctx context.Context, ch ChannelRef, subjectID string) (*beadsapi.BeadDetail, error) {
	return r.findCard(ctx, ch, subjectID)
}

// CardRef reads a card bead's copy on the named surface off its fields.
func CardRef(surface string, card *beadsapi.BeadDetail) (Ref, bool) {
	if card == nil {
		return Ref{}, false
	}
	return storedRef(surface, card.ID, card.Fields)
}

// findCard returns the card bead for (subject, channel), or nil.
func (r *Router) findCard(ctx context.Context, ch ChannelRef, subjectID string) (*beadsapi.BeadDetail, error) {
	fields := map[string]string{BeadKind: string(KindCard), BeadSubjectID: subjectID}
	if ch.ID != "" {
		fields[BeadChannelID] = ch.ID
	} else {
		fields[SurfaceSlack+"_channel"] = ch.Slack
	}
	res, err := r.store.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Labels: []string{"project:" + ch.Land},
		Fields: fields,
		Limit:  1,
	})
	if err != nil {
		return nil, fmt.Errorf("conversation: find card of %s: %w", subjectID, err)
	}
	if len(res.Beads) == 0 {
		return nil, nil
	}
	return res.Beads[0], nil
}

// storedRef reads a surface's copy of a message bead off its fields
// (`<surface>_channel`, `<surface>_ts`, e.g. slack_channel / slack_ts). The
// Commons copy is the bead itself, in its channel_id.
func storedRef(surface, beadID string, fields map[string]string) (Ref, bool) {
	ref := Ref{Surface: surface, Channel: fields[surface+"_channel"], TS: fields[surface+"_ts"]}
	if surface == SurfaceCommons {
		ref.Channel, ref.TS = fields[BeadChannelID], beadID
	}
	return ref, !ref.IsZero()
}

// messageBead is the closed type=message bead for m, with extra fields (a
// surface ref the caller already holds) written in the same create.
func messageBead(ch ChannelRef, th *Thread, m Message, extra map[string]string) beadsapi.CreateBeadRequest {
	fields := map[string]string{
		BeadKind:   string(m.KindOrText()),
		BeadText:   m.Text,
		BeadAuthor: m.Author.ID,
	}
	if ch.ID != "" {
		fields[BeadChannelID] = ch.ID
	} else {
		// No channel bead yet: the Slack channel is the only key a card
		// upsert can find this bead by.
		fields[SurfaceSlack+"_channel"] = ch.Slack
	}
	if th != nil && th.Head != "" {
		fields[BeadThreadID] = th.Head
	}
	if m.SubjectID != "" {
		fields[BeadSubjectID] = m.SubjectID
	}
	if m.Origin != "" {
		fields[BeadOrigin] = m.Origin
	}
	fields[BeadDetail] = m.Detail
	fields[BeadFootnote] = m.Footnote
	for k, v := range extra {
		fields[k] = v
	}
	for k, v := range fields {
		if v == "" {
			delete(fields, k)
		}
	}
	for k, v := range m.Fields {
		if _, set := fields[k]; !set && v != "" {
			fields[k] = v
		}
	}
	raw, _ := json.Marshal(fields) // a map[string]string always marshals

	title := strings.TrimSpace(m.Text)
	if title == "" && len(m.Files) > 0 {
		names := make([]string, len(m.Files))
		for i, f := range m.Files {
			names[i] = f.Name
		}
		title = "Uploaded " + strings.Join(names, ", ")
	}
	if title == "" {
		title = string(m.KindOrText()) + " " + m.SubjectID
	}
	if len(title) > maxTitle {
		title = title[:maxTitle-3] + "..."
	}
	req := beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "message",
		Kind:      "data",
		Status:    "closed",
		Labels:    []string{"project:" + ch.Land},
		CreatedBy: m.Author.ID,
		Fields:    raw,
	}
	if m.KindOrText() == KindCard {
		// Two racers upserting the same card converge on one bead.
		req.IdempotencyKey = "card:" + m.SubjectID + ":" + channelKey(ch)
	}
	return req
}

func channelKey(ch ChannelRef) string {
	if ch.ID != "" {
		return ch.ID
	}
	return SurfaceSlack + ":" + ch.Slack
}
