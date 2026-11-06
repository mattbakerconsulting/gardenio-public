package landmap

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"

	"gardenio/kbeads/internal/model"
	"gardenio/lands/contract/landsv1"
)

// The land feed (D.9 gd-Z9VVGjvzBk; contract design gd-EWSKPH7pa5 §2
// "Streaming", §8 step 7; boundary gd-uYNbyW4a9Z §6 step 6): GET
// /lands/v1/lands/{land}/events, one resumable SSE stream per land.
//
// The daemon's event stream is tapped once (Changes). The feed keeps, per
// land, a short log of which bead changed and how (ChangeKind and id, never
// the payload, which carries the whole fields blob) under a per-land
// sequence, the cursor. Nothing about the bead is decided when the event
// happens: each subscriber's goroutine reads the bead through the daemon's
// GET route with the subscriber's own request context as it writes the
// frame, so the daemon's read gate (scope, taints) judges the subscriber at
// delivery, and the frame carries the bead as it is then, projected to its
// contract noun. A bead that is out of the subscriber's scope, gone, or not a
// noun the event allowlist (EventTypes) streams is dropped and counted.

// Change is one daemon event as the feed takes it: what happened, the bead
// it happened to and that bead's labels when it was published (only to file
// it under a land; delivery reads the bead again).
type Change struct {
	Kind   ChangeKind
	BeadID string
	Labels []string
}

// ChangeKind is what a daemon event did to its bead, in the feed's terms. The
// server maps its topics onto these (changesForMap); a topic no land event
// comes from is ChangeOther and is dropped before it reaches a land's log.
type ChangeKind int

const (
	ChangeOther ChangeKind = iota
	ChangeCreated
	// ChangeUpdated is any edit short of a close: fields, status, labels, a
	// new comment.
	ChangeUpdated
	ChangeClosed
	// ChangePurse is an update to a land's lands-<land> account bead, where
	// the purse watcher writes the purse snapshot. A land member may not read
	// the account bead, so delivery reads the snapshot (Lands.Budget) instead,
	// and sends budget.updated when it reads differently from the last one.
	ChangePurse
)

// Changes is the daemon's event stream. Tap registers fn to see every event;
// fn runs on the publisher's goroutine and must not block.
type Changes interface {
	Tap(fn func(Change))
}

const (
	// feedRingSize is how many changes each land's log keeps for resuming.
	// A cursor older than the log resumes from now.
	feedRingSize = 256
	// feedSubscriberBuffer is the per-subscriber cap: changes waiting for one
	// slow reader. A reader that falls further behind is cut and resumes
	// from its last cursor.
	feedSubscriberBuffer = 64
	// maxLandFeedSubscribers caps one land's concurrent streams, and
	// landFeedRate and landFeedBurst its new streams a second; past either
	// the stream is answered rate_limited. These are the per-land limits:
	// they run after the caller is admitted to the land, which the edge's
	// (per-IP only) cannot, so a stranger cannot spend a land's share.
	maxLandFeedSubscribers = 32
	landFeedRate           = 2
	landFeedBurst          = 10
	// feedMaxLifetime ends a stream after the access-token lifetime: the
	// bearer was checked once, at connect, so the client reconnects with a
	// fresh one and its cursor.
	feedMaxLifetime = 5 * time.Minute
	// feedKeepalive is the comment line that keeps idle proxies open.
	feedKeepalive = 15 * time.Second
)

// FeedDropped counts what the land feed did not stream, by reason: topic (a
// daemon event no land event comes from, ChangeOther), noun (a bead no allowlisted event
// carries), unreadable (out of the reader's scope at delivery, or gone), cut
// (a reader that overflowed its buffer) and unchanged (a purse rewrite whose
// budget reads as the one the reader last got, such as the watcher's
// heartbeat).
var FeedDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_lands_feed_dropped_total",
	Help: "Daemon events the land feed did not stream, by reason (topic, noun, unreadable, cut, unchanged).",
}, []string{"reason"})

type feedEntry struct {
	seq    uint64
	kind   ChangeKind
	beadID string
}

// landLog is one land's recent changes, its live readers and its new-stream
// limiter.
type landLog struct {
	seq     uint64
	ring    []feedEntry // oldest first, at most feedRingSize
	subs    map[*feedSub]struct{}
	opening *rate.Limiter
}

type feedSub struct {
	ch      chan feedEntry
	cut     chan struct{}
	cutOnce sync.Once
}

// feed is every land's log. A land gets a log when someone first streams it
// and keeps it for the life of the process, so a reader that reconnects
// finds what it missed.
type feed struct {
	// epoch names this process's sequences: a cursor minted by another
	// process (before a restart) is unknown and resumes from now.
	epoch string
	mu    sync.Mutex
	lands map[string]*landLog
}

func newFeed(c Changes) *feed {
	var b [6]byte
	_, _ = rand.Read(b[:])
	f := &feed{epoch: hex.EncodeToString(b[:]), lands: map[string]*landLog{}}
	if c != nil {
		c.Tap(f.publish)
	}
	return f
}

// publish files a change under each land it is labelled with that has a log.
func (f *feed) publish(c Change) {
	if c.BeadID == "" || c.Kind == ChangeOther {
		FeedDropped.WithLabelValues("topic").Inc()
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range c.Labels {
		land, ok := strings.CutPrefix(l, "project:")
		if !ok {
			continue
		}
		lg := f.lands[land]
		if lg == nil {
			continue
		}
		lg.seq++
		e := feedEntry{seq: lg.seq, kind: c.Kind, beadID: c.BeadID}
		if len(lg.ring) == feedRingSize {
			lg.ring = append(lg.ring[:0], lg.ring[1:]...)
		}
		lg.ring = append(lg.ring, e)
		for s := range lg.subs {
			select {
			case s.ch <- e:
			default:
				s.cutOnce.Do(func() {
					close(s.cut)
					FeedDropped.WithLabelValues("cut").Inc()
				})
			}
		}
	}
}

var errFeedFull = errors.New("land feed: too many readers, or too many new ones")

// subscribe registers a reader of land and returns the changes after cursor
// still in the log. An unknown, foreign or expired cursor replays nothing.
func (f *feed) subscribe(land string, cursor *landsv1.Cursor) (*feedSub, []feedEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lg := f.lands[land]
	if lg == nil {
		lg = &landLog{subs: map[*feedSub]struct{}{}, opening: rate.NewLimiter(landFeedRate, landFeedBurst)}
		f.lands[land] = lg
	}
	if len(lg.subs) >= maxLandFeedSubscribers || !lg.opening.Allow() {
		return nil, nil, errFeedFull
	}
	var replay []feedEntry
	if after, ok := f.decodeCursor(cursor); ok && after <= lg.seq && len(lg.ring) > 0 && after+1 >= lg.ring[0].seq {
		for _, e := range lg.ring {
			if e.seq > after {
				replay = append(replay, e)
			}
		}
	}
	s := &feedSub{ch: make(chan feedEntry, feedSubscriberBuffer), cut: make(chan struct{})}
	lg.subs[s] = struct{}{}
	return s, replay, nil
}

func (f *feed) unsubscribe(land string, s *feedSub) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if lg := f.lands[land]; lg != nil {
		delete(lg.subs, s)
	}
}

// Event cursors are opaque to clients; inside they are this process's epoch
// and the land's sequence.

func (f *feed) encodeCursor(seq uint64) landsv1.Cursor {
	return base64.RawURLEncoding.EncodeToString([]byte("e" + f.epoch + "." + strconv.FormatUint(seq, 10)))
}

func (f *feed) decodeCursor(c *landsv1.Cursor) (uint64, bool) {
	if c == nil {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(*c)
	if err != nil {
		return 0, false
	}
	rest, ok := strings.CutPrefix(string(raw), "e"+f.epoch+".")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(rest, 10, 64)
	return n, err == nil
}

// feedHeaders are the stream's headers beyond the ones every map response
// carries: no caching, and no buffering in a proxy in front.
var feedHeaders = map[string]string{"Cache-Control": "no-cache", "X-Accel-Buffering": "no"}

func (m *Map) StreamEvents(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.StreamEventsParams) {
	if !m.admit(w, r, land) {
		return
	}
	rc := http.NewResponseController(w)
	cursor := params.LastEventID
	if cursor == nil {
		cursor = params.Since
	}
	sub, replay, err := m.feed.subscribe(land, cursor)
	if err != nil {
		problem(w, landsv1.RateLimited, "")
		return
	}
	defer m.feed.unsubscribe(land, sub)

	h := w.Header()
	for k := range h {
		if !slices.Contains(ResponseHeaders, k) {
			h.Del(k)
		}
	}
	h.Set("Content-Type", "text/event-stream")
	h.Set(landsv1.HeaderContract, landsv1.ContractVersion)
	for k, v := range feedHeaders {
		h.Set(k, v)
	}
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}

	st := &streamState{}
	for _, e := range replay {
		if !m.deliver(w, r, land, st, e) {
			return
		}
	}
	_ = rc.Flush()

	ctx := r.Context()
	lifetime := time.NewTimer(feedMaxLifetime)
	defer lifetime.Stop()
	keepalive := time.NewTicker(feedKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-lifetime.C:
			return
		case <-sub.cut:
			// Overflowed its buffer: the client resumes from its cursor.
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ":keepalive\n\n"); err != nil {
				return
			}
		case e := <-sub.ch:
			if !m.deliver(w, r, land, st, e) {
				return
			}
		}
		if rc.Flush() != nil {
			return
		}
	}
}

// streamState is what one reader's stream remembers between frames: the budget
// it last sent, so a purse rewrite that changes nothing the contract shows
// sends nothing.
type streamState struct {
	budget *landsv1.Budget
}

// deliver writes one frame for e, if the reader may see it now and it is an
// allowlisted event. It returns false only when the write failed.
func (m *Map) deliver(w http.ResponseWriter, r *http.Request, land string, st *streamState, e feedEntry) bool {
	typ, obj, ok := m.feedEvent(r, land, e)
	if !ok {
		return true
	}
	if b, isBudget := obj.(landsv1.Budget); isBudget {
		if st.budget != nil && *st.budget == b {
			FeedDropped.WithLabelValues("unchanged").Inc()
			return true
		}
		st.budget = &b
	}
	data, err := json.Marshal(obj)
	if err != nil {
		slog.Warn("landmap: feed encode failed", "type", string(typ), "error", err)
		return true
	}
	// The frame's data is the answer; its id is the feed's own cursor and
	// its event a contract enum.
	scanLive(tallyFromWriter(w), nil, data, "")
	_, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", m.feed.encodeCursor(e.seq), typ, data)
	return err == nil
}

// feedEvent reads e's bead as the reader and projects it to the event the
// allowlist streams for it, if any.
func (m *Map) feedEvent(r *http.Request, land string, e feedEntry) (landsv1.EventType, any, bool) {
	if e.kind == ChangePurse {
		return m.purseEvent(r, land)
	}
	var b beadRecord
	if err := m.d.call(r, rGetBead, []string{e.beadID}, nil, nil, &b); err != nil {
		var ue *upstreamError
		if !errors.As(err, &ue) {
			slog.Warn("landmap: feed read failed", "error", err)
		}
		FeedDropped.WithLabelValues("unreadable").Inc()
		return "", nil, false
	}
	if !b.inLand(land) {
		FeedDropped.WithLabelValues("unreadable").Inc()
		return "", nil, false
	}
	switch {
	case isTicketType(b.Type):
		waiting, err := m.waiting(r, land, []beadRecord{b})
		if err != nil {
			FeedDropped.WithLabelValues("unreadable").Inc()
			return "", nil, false
		}
		return landsv1.TicketUpdated, toTicket(&b, waiting[b.ID]), true
	case b.Type == model.TypeDecision && e.kind == ChangeCreated:
		return landsv1.DecisionOpened, toDecision(&b), true
	case b.Type == model.TypeDecision && e.kind == ChangeClosed:
		return landsv1.DecisionResolved, toDecision(&b), true
	case b.Type == model.TypeEscalation && e.kind == ChangeCreated:
		return landsv1.EscalationOpened, toEscalation(&b), true
	case b.Type == model.TypeRelease:
		if typ, rel, ok := m.releaseEvent(r, land, &b, e.kind); ok {
			return typ, rel, true
		}
	case slices.Contains(planTypes, b.Type) && slices.Contains(b.Labels, planLabel):
		steps, waiting, err := m.readSteps(r, land, b.ID)
		if err != nil {
			FeedDropped.WithLabelValues("unreadable").Inc()
			return "", nil, false
		}
		return landsv1.PlanUpdated, toPlan(&b, steps, waiting), true
	}
	FeedDropped.WithLabelValues("noun").Inc()
	return "", nil, false
}

// purseEvent reads the land's purse snapshot for a ChangePurse. The reader
// cannot read the account bead the change was to, so the daemon's read gate
// does not judge it: the map asks again, at delivery, whether the reader is
// still admitted to the land, as GetBudget does on every call.
func (m *Map) purseEvent(r *http.Request, land string) (landsv1.EventType, any, bool) {
	if !m.lands.Admits(r.Context(), land) {
		FeedDropped.WithLabelValues("unreadable").Inc()
		return "", nil, false
	}
	rec, ok, err := m.lands.Budget(r.Context(), land)
	if err != nil {
		slog.Warn("landmap: feed purse read failed", "error", err)
	}
	b, shown := toBudget(rec)
	if err != nil || !ok || !shown {
		FeedDropped.WithLabelValues("unreadable").Inc()
		return "", nil, false
	}
	return landsv1.BudgetUpdated, b, true
}
