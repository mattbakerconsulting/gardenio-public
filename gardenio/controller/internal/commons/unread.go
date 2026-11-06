package commons

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Read markers and unread (spec sections 2.6 and 8; Commons III step 5).
//
// A person's read position in a channel is the id of the newest top-level
// entry they have seen there, kept in read_cursors on their own user bead
// (kbeads debounces the write to one per 10 s per person). A channel's unread
// count is its top-level entries said after that one: messages, cards and
// thread heads, not thread replies (a thread is read by opening it), not the
// person's own words and not tombstones. A channel with no position counts
// from the start. A land's count is the sum of its channels'.
//
// Counts come from the daemon on the viewer's bearer, per channel (a land's
// DMs read in batches, batch.go), and stop at unreadWindow conversation
// beads; past that the count is a floor and renders with a "+".
//
// Mine colours the phone's Commons tab pip (design note 3.1 of the phone
// design): a reply by someone else, said after the channel's position, is the
// newest in a thread the viewer started or wrote in within the window. A
// thread keeps no read position of its own, so the viewer's own reply is what
// answers it.

// Cursors is the slice of the daemon client that keeps the viewer's read
// positions. *beadsapi.Client satisfies it.
type Cursors interface {
	ReadCursors(ctx context.Context) (map[string]string, error)
	SetReadCursors(ctx context.Context, cursors map[string]string) (map[string]string, error)
}

const (
	// unreadWindow is how many of a channel's newest conversation beads
	// (replies included) one count reads.
	unreadWindow = 100
	// unreadShown is the largest count a badge spells out.
	unreadShown = 99
	// unreadParallel bounds the channel counts read at once.
	unreadParallel = 8
	// unreadHeadReads bounds the thread heads older than the window one
	// count reads to learn whether the viewer started them.
	unreadHeadReads = 8
	// unreadEscalations bounds the escalations of a land read to tell its
	// in-hand escalation cards, which the counts skip (gd-bmOiteQi36). One past
	// it is counted, the way an owner one is.
	unreadEscalations = 500
)

// Unread is a channel's or a land's unread count. More means the count is a
// floor: the read stopped at unreadWindow. Mine means a thread of the
// viewer's has a reply they have not answered.
type Unread struct {
	N    int
	More bool
	Mine bool
}

// Label is the badge text: "" when nothing counted is unread, the count,
// "99+" past unreadShown, and a "+" on a floor.
func (u Unread) Label() string {
	switch {
	case u.N == 0:
		return ""
	case u.N > unreadShown:
		return strconv.Itoa(unreadShown) + "+"
	case u.More:
		return strconv.Itoa(u.N) + "+"
	}
	return strconv.Itoa(u.N)
}

func (u Unread) add(v Unread) Unread {
	return Unread{N: u.N + v.N, More: u.More || v.More, Mine: u.Mine || v.Mine}
}

// readCursors returns the viewer's positions. A viewer kbeads keeps none for
// (no user bead, not a person's token) has none, which is not an error.
func readCursors(ctx context.Context, cs Cursors) (map[string]string, error) {
	if cs == nil {
		return nil, nil
	}
	cur, err := cs.ReadCursors(ctx)
	var ae *beadsapi.APIError
	if errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cur, nil
}

// readUnread counts every channel of lands for the viewer, by channel id. A
// channel whose count the daemon did not answer is left out (no badge) rather
// than failing the rest. #machines is never counted: what machines say is
// there to read, not to answer, so it carries no badge and adds none to its
// land's.
func readUnread(ctx context.Context, rd Reader, lands []Land, cursors map[string]string, viewer string) map[string]Unread {
	out := map[string]Unread{}
	var chs []Channel
	for _, l := range lands {
		for _, ch := range l.Channels {
			if ch.Kind != kindMachines {
				chs = append(chs, ch)
			}
		}
	}
	slices.SortStableFunc(chs, byReadOrder)
	inHand := map[string]map[string]bool{}
	for _, l := range lands {
		if len(l.Channels) > 0 {
			inHand[l.Name] = inHandEscalations(ctx, rd, l.Name)
		}
	}
	counts := make([]Unread, len(chs))
	ok := make([]bool, len(chs))
	readChannels(ctx, rd, chs,
		func(i int, r channelRows) bool {
			ch := chs[i]
			counts[i], ok[i] = batchUnread(ctx, rd, ch, r, cursors[ch.ID], viewer, inHand[ch.Land])
			return ok[i]
		},
		func(i int) {
			ch := chs[i]
			u, err := channelUnread(ctx, rd, ch, cursors[ch.ID], viewer, inHand[ch.Land])
			counts[i], ok[i] = u, err == nil
		})
	for i, ch := range chs {
		if ok[i] {
			out[ch.ID] = counts[i]
		}
	}
	return out
}

// inHandEscalations are land's escalations that are not the owner's
// (beadsapi.EscalationAudience), by id: their cards are in hand, so the counts
// skip them. A read that fails folds nothing.
func inHandEscalations(ctx context.Context, rd Reader, land string) map[string]bool {
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"escalation"},
		Labels: []string{"project:" + land},
		Sort:   "-created_at",
		Limit:  unreadEscalations,
	})
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, b := range res.Beads {
		if b != nil && !beadsapi.EscalationForOwner(b.Fields) {
			out[b.ID] = true
		}
	}
	return out
}

// channelUnread counts ch's top-level entries after the cursor message. A
// cursor that no longer names an entry of ch counts the channel from the
// start. A card for one of inHand, an escalation the desk or the operators
// have, is not counted: it is in hand.
func channelUnread(ctx context.Context, rd Reader, ch Channel, cursor, viewer string, inHand map[string]bool) (Unread, error) {
	var since time.Time
	if cursor != "" {
		if c, err := readEntry(ctx, rd, ch, cursor); err == nil {
			since = c.At
		} else if !errors.Is(err, errNotFound) {
			return Unread{}, err
		}
	}
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels:       []string{"project:" + ch.Land},
		Fields:       map[string]string{fieldChannelID: ch.ID},
		CreatedAfter: since,
		Sort:         "-created_at",
		Limit:        unreadWindow,
	})
	if err != nil {
		return Unread{}, readErr("unread of "+ch.ID, err)
	}
	return countUnread(ctx, rd, ch, res.Beads, cursor, since, viewer, inHand), nil
}

// batchUnread is channelUnread answered from r, ch's rows of a batched read;
// false when r does not hold what channelUnread would read: the cursor
// message, and every bead since it (to unreadWindow).
func batchUnread(ctx context.Context, rd Reader, ch Channel, r channelRows, cursor, viewer string, inHand map[string]bool) (Unread, bool) {
	var since time.Time
	if cursor != "" {
		i := slices.IndexFunc(r.Beads, func(b *beadsapi.BeadDetail) bool { return b.ID == cursor })
		if i < 0 {
			return Unread{}, false
		}
		if m, ok := messageFromBead(r.Beads[i]); ok && m.Land == ch.Land && m.Channel == ch.ID && m.Thread == "" {
			since = m.At
		}
	}
	// channelUnread's created_after goes to the second, kept inclusively.
	from := since.Truncate(time.Second)
	n := 0
	for n < len(r.Beads) && n < unreadWindow && !r.Beads[n].CreatedAt.Before(from) {
		n++
	}
	switch {
	case r.Complete:
	case n == unreadWindow && r.Beads[n-1].CreatedAt.After(r.Cutoff):
	case n < unreadWindow && from.After(r.Cutoff):
	default:
		return Unread{}, false // what channelUnread reads may lie past the batch's oldest bead
	}
	return countUnread(ctx, rd, ch, r.Beads[:n], cursor, since, viewer, inHand), true
}

// countUnread counts ch's top-level entries in beads, its newest beads said
// since the cursor message (at since), newest first and at most unreadWindow.
func countUnread(ctx context.Context, rd Reader, ch Channel, beads []*beadsapi.BeadDetail, cursor string, since time.Time, viewer string, inHand map[string]bool) Unread {
	var u Unread
	byViewer := func(m Message) bool { return viewer != "" && strings.EqualFold(m.Author, viewer) }
	wrote := map[string]bool{} // threads the viewer started or replied in
	heads := map[string]bool{} // thread heads in the window
	seen := map[string]bool{}  // threads whose newest reply is known
	var answer []string        // threads whose newest reply is someone else's, newest first
	for _, b := range beads {
		if b == nil {
			continue
		}
		m, ok := messageFromBead(b)
		if !ok || m.Land != ch.Land || m.Channel != ch.ID || m.Deleted {
			continue
		}
		if m.Thread == "" {
			heads[m.ID] = true
			wrote[m.ID] = wrote[m.ID] || byViewer(m)
		} else {
			wrote[m.Thread] = wrote[m.Thread] || byViewer(m)
			if !seen[m.Thread] && m.At.After(since) { // newest first
				seen[m.Thread] = true
				if !byViewer(m) {
					answer = append(answer, m.Thread)
				}
			}
		}
		if b.ID != cursor && unreadBy(m, ch, since, viewer) && !(m.Kind == kindCard && inHand[m.SubjectID]) {
			u.N++
		}
	}
	u.More = len(beads) >= unreadWindow
	reads := 0
	for _, th := range answer {
		if u.Mine || viewer == "" {
			break
		}
		switch {
		case wrote[th]:
			u.Mine = true
		case !heads[th] && reads < unreadHeadReads:
			// the head is older than the window: read who started it
			reads++
			if h, err := readEntry(ctx, rd, ch, th); err == nil && byViewer(h) {
				u.Mine = true
			}
		}
	}
	return u
}

// unreadBy reports whether m counts as unread for viewer, whose position in
// ch was said at since: a top-level entry of ch said after it, not a
// tombstone and not the viewer's own words.
func unreadBy(m Message, ch Channel, since time.Time, viewer string) bool {
	if m.Land != ch.Land || m.Channel != ch.ID || m.Thread != "" || m.Deleted || !m.At.After(since) {
		return false
	}
	return viewer == "" || !strings.EqualFold(m.Author, viewer)
}

// readEntry returns id when it is a top-level entry of ch.
func readEntry(ctx context.Context, rd Reader, ch Channel, id string) (Message, error) {
	b, err := rd.GetBead(ctx, id)
	if err != nil {
		return Message{}, readErr("message "+id, err)
	}
	if b == nil {
		return Message{}, errNotFound
	}
	m, ok := messageFromBead(b)
	if !ok || m.Land != ch.Land || m.Channel != ch.ID || m.Thread != "" {
		return Message{}, errNotFound
	}
	return m, nil
}

// unreadJSON is GET /garden/c/unread's body. Label is the badge text, so the
// pane renders counts exactly as the server words them.
type unreadJSON struct {
	Unread int  `json:"unread"`
	More   bool `json:"more,omitempty"`
	// Mine: a thread of the viewer's has a reply, so the phone's Commons
	// tab pip is in the alert colour.
	Mine  bool             `json:"mine,omitempty"`
	Label string           `json:"label"`
	Lands []landUnreadJSON `json:"lands"`
}

type landUnreadJSON struct {
	Land     string              `json:"land"`
	Unread   int                 `json:"unread"`
	More     bool                `json:"more,omitempty"`
	Label    string              `json:"label"`
	Channels []channelUnreadJSON `json:"channels"`
}

type channelUnreadJSON struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Unread int    `json:"unread"`
	More   bool   `json:"more,omitempty"`
	Label  string `json:"label"`
}

// unread serves GET /garden/c/unread: the viewer's unread counts per channel,
// per land and in all, for the channel list, the collapsed rail and the
// camera-bar button.
func (h *Handler) unread(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	sc := h.scope(r)
	lands, err := readLands(ctx, h.Beads, sc)
	if err != nil {
		h.fail(w, err)
		return
	}
	cursors, err := readCursors(ctx, h.Cursors)
	if err != nil {
		h.fail(w, err)
		return
	}
	counts := readUnread(ctx, h.Beads, lands, cursors, sc.Principal)
	body := unreadJSON{Lands: []landUnreadJSON{}}
	var all Unread
	for _, l := range lands {
		var lu Unread
		lj := landUnreadJSON{Land: l.Name, Channels: []channelUnreadJSON{}}
		for _, ch := range l.Channels {
			u, ok := counts[ch.ID]
			if !ok {
				continue
			}
			lu = lu.add(u)
			lj.Channels = append(lj.Channels, channelUnreadJSON{ID: ch.ID, Name: ch.Name, Unread: u.N, More: u.More, Label: u.Label()})
		}
		lj.Unread, lj.More, lj.Label = lu.N, lu.More, lu.Label()
		body.Lands = append(body.Lands, lj)
		all = all.add(lu)
	}
	body.Unread, body.More, body.Mine, body.Label = all.N, all.More, all.Mine, all.Label()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, body)
}

// markRead serves POST /garden/c/{land}/{channel}/read with form value m, a
// top-level entry of the channel the viewer has seen. The position only moves
// forward: an m said before the stored one (an old tab catching up) is a
// no-op. Answers 204.
func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	if h.Cursors == nil {
		http.Error(w, "read markers not enabled", http.StatusServiceUnavailable)
		return
	}
	if crossOrigin(r) {
		http.Error(w, "Read markers must come from this page.", http.StatusForbidden)
		return
	}
	id := strings.TrimSpace(r.FormValue("m"))
	if id == "" || len(id) > 128 {
		http.Error(w, "bad message id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	ch, err := readChannel(ctx, h.Beads, h.scope(r), r.PathValue("land"), r.PathValue("channel"))
	if err != nil {
		h.fail(w, err)
		return
	}
	m, err := readEntry(ctx, h.Beads, ch, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	cursors, err := readCursors(ctx, h.Cursors)
	if err != nil {
		h.fail(w, err)
		return
	}
	if old := cursors[ch.ID]; old == m.ID {
		w.WriteHeader(http.StatusNoContent)
		return
	} else if old != "" {
		if prev, err := readEntry(ctx, h.Beads, ch, old); err == nil && prev.At.After(m.At) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if _, err := h.Cursors.SetReadCursors(ctx, map[string]string{ch.ID: m.ID}); err != nil {
		var ae *beadsapi.APIError
		if errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden) {
			http.Error(w, "no read position is kept for this sign-in", http.StatusConflict)
			return
		}
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
