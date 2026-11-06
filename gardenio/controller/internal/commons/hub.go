package commons

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
)

// Invalidation names one conversation bead that changed, or a node under a
// root a stream watches. The hub moves ids only; the stream dresses each with
// its content for the viewer it serves (stream.go).
type Invalidation struct {
	Kind    string    `json:"-"` // message | card | channel | presence | typing | node | job: the SSE event name
	ID      string    `json:"id"`
	Channel string    `json:"channel,omitempty"`
	Thread  string    `json:"thread,omitempty"`
	Land    string    `json:"land"`
	At      time.Time `json:"-"`
	// subject is a card's subject_id: a new card drops its subject's memo
	// (cards.go).
	subject string
	// written is the bead's updated_at from the event payload: when kbeads
	// wrote the change. Zero when the payload has none.
	written time.Time
	// created is true for the bead's creation event (steward_sli.go).
	created bool
	// links is what a node hangs from: its parent-child parents, its task_id
	// and thread_id (nodeEvent).
	links []string
	// job is a job event's job (jobs.go); ID is its agent.
	job *Job
}

const (
	// kindNode is the event of a bead under a root a stream watches.
	kindNode = "node"
	// watchMax bounds one stream's watched ids: the root and nodes it names,
	// and the beads learned to hang from them.
	watchMax = 512
)

// Source opens a stream of daemon bead events; the returned func ends it.
// beads-viewer passes its feed hub's Subscribe, so the Commons rides the one
// upstream connection the viewer already holds.
type Source func(ctx context.Context) (<-chan beadsapi.SSEEvent, func())

// BeadGetter reads one bead. The hub reads as the system (there is no viewer
// behind a shared upstream stream); the answer only decides which subscribers
// get an id, it is never shown.
type BeadGetter interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
}

const (
	// hubQueue is the depth between the upstream reader, which only parses, and
	// the worker, which attributes each conversation event to a land with a
	// daemon read. Conversation events are a small share of the bead firehose,
	// so the reader never waits on the daemon.
	hubQueue = 1024
	// hubClientBuffer is one subscriber's channel depth; a full one drops its
	// oldest id, and the client's ?since= replay heals the gap.
	hubClientBuffer = 64
	// hubReconnectWait is the pause before re-opening a closed source.
	hubReconnectWait = 2 * time.Second
	// landMemoCap and landMemoTTL bound the bead id -> land memo: a full memo is
	// dropped wholesale, and an entry expires so a bead moved between lands stops
	// being attributed to its old one.
	landMemoCap = 4096
	landMemoTTL = time.Minute
	// lookupTimeout bounds one attribution read.
	lookupTimeout = 3 * time.Second
)

// Hub fans the conversation subset of the daemon's bead events out to the
// Commons event streams, cut per subscriber: a subscriber gets an id only when
// its scope admits the bead's land, and the land is read from the daemon,
// never taken from the event payload. An event that cannot be attributed is
// dropped (fail closed).
//
// A nil *Hub is safe: Subscribe returns a closed channel.
type Hub struct {
	source Source
	beads  BeadGetter
	logger *slog.Logger
	now    func() time.Time

	mu   sync.Mutex
	subs map[int]*hubSub
	next int
	// watchers counts the subscribers with a watch, so the reader looks for
	// node events only while someone has a root open.
	watchers atomic.Int64

	memoMu sync.Mutex
	memo   map[string]landMemo
	// cards memoizes a subject's card invalidations (cards.go), under memoMu.
	cards map[string]cardMemo

	queue  chan Invalidation
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// lag is gardenio_commons_event_lag_seconds, the Commons event latency SLI
	// (Lands Track B §5.8): kbeads write to broadcast, once per event.
	lag *prometheus.HistogramVec
	// steward is gardenio_lands_steward_response_seconds, the steward response
	// SLI (steward_sli.go).
	steward *stewardSLI
}

type hubSub struct {
	scope Scope
	ch    chan Invalidation
	// watch is the ids whose node events this subscriber gets, under Hub.mu.
	watch map[string]bool
}

// sees reports whether a node event is under s's watch, and learns its id
// when it hangs from a watched bead, so the beads under that one follow (a
// step's agent, after the step). Under Hub.mu.
func (s *hubSub) sees(inv Invalidation) bool {
	if s.watch[inv.ID] {
		return true
	}
	for _, l := range inv.links {
		if s.watch[l] {
			if len(s.watch) < watchMax {
				s.watch[inv.ID] = true
			}
			return true
		}
	}
	return false
}

type landMemo struct {
	land    string
	expires time.Time
}

// NewHub returns a hub over source, attributing events with beads. It returns
// nil when either is nil.
func NewHub(source Source, beads BeadGetter, logger *slog.Logger) *Hub {
	if source == nil || beads == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		source: source, beads: beads, logger: logger, now: time.Now,
		subs: map[int]*hubSub{}, memo: map[string]landMemo{}, cards: map[string]cardMemo{}, queue: make(chan Invalidation, hubQueue),
		lag: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gardenio_commons_event_lag_seconds",
			Help: "Seconds from kbeads writing a Commons bead (its updated_at) to the viewer's Commons hub broadcasting it to the event streams, once per event (not per viewer), by land and kind (message, card, channel). The Commons event latency SLI of the Lands SLOs.",
			// The landsSLO commons_event_latency threshold (5) must stay a bound.
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"land", "kind"}),
		steward: newStewardSLI(),
	}
}

// LagCollector is the hub's event-lag histogram, for the viewer to register.
// Nil-safe: a nil hub has none.
func (h *Hub) LagCollector() prometheus.Collector {
	if h == nil {
		return nil
	}
	return h.lag
}

// StewardResponseCollector is the hub's steward response histogram, for the
// viewer to register. Nil-safe: a nil hub has none.
func (h *Hub) StewardResponseCollector() prometheus.Collector {
	if h == nil {
		return nil
	}
	return h.steward.hist
}

// Start runs the hub until Close. Nil-safe.
func (h *Hub) Start() {
	if h == nil {
		return
	}
	ctx, cancel := context.WithCancel(beadsapi.WithSystemRead(context.Background()))
	h.cancel = cancel
	h.wg.Add(2)
	go h.read(ctx)
	go h.work(ctx)
}

// Close stops the hub and closes every subscriber's channel. Nil-safe.
func (h *Hub) Close() {
	if h == nil {
		return
	}
	if h.cancel != nil {
		h.cancel()
		h.wg.Wait()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, s := range h.subs {
		close(s.ch)
		delete(h.subs, id)
		if s.watch != nil {
			h.watchers.Add(-1)
		}
	}
}

// read pumps the source into the queue, re-opening it when it closes.
func (h *Hub) read(ctx context.Context) {
	defer h.wg.Done()
	for ctx.Err() == nil {
		events, stop := h.source(ctx)
		h.drain(ctx, events)
		stop()
		t := time.NewTimer(hubReconnectWait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (h *Hub) drain(ctx context.Context, events <-chan beadsapi.SSEEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			if h.watchers.Load() > 0 {
				if n, ok := nodeEvent(evt.Data); ok && h.watched(n) {
					n.At = h.now()
					h.enqueue(n)
				}
			}
			inv, ok := classify(evt.Data)
			if !ok {
				continue
			}
			inv.At = h.now()
			inv.created = evt.Event == "beads.bead.created"
			h.enqueue(inv)
		}
	}
}

func (h *Hub) enqueue(inv Invalidation) {
	select {
	case h.queue <- inv:
	default:
		h.logger.Warn("commons hub: queue full, dropping an invalidation", "bead", inv.ID)
	}
}

// watched reports whether any subscriber watches n or what it hangs from.
func (h *Hub) watched(n Invalidation) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		if s.watch == nil {
			continue
		}
		if s.watch[n.ID] {
			return true
		}
		for _, l := range n.links {
			if s.watch[l] {
				return true
			}
		}
	}
	return false
}

// work attributes each queued invalidation to its land and broadcasts it.
func (h *Hub) work(ctx context.Context) {
	defer h.wg.Done()
	sweep := time.NewTicker(stewardSweep)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			h.steward.expire(h.now())
		case inv := <-h.queue:
			if inv.Kind == kindSubject {
				// A card's subject changed: every card of it re-renders (D1).
				for _, c := range h.subjectCards(ctx, inv.ID) {
					c.At = inv.At
					h.broadcast(c)
				}
				continue
			}
			if inv.Kind == "card" {
				h.forgetSubject(inv.subject)
			}
			land, b, ok := h.attribute(ctx, inv.ID)
			if !ok {
				continue
			}
			inv.Land = land
			h.observeLag(inv)
			h.broadcast(inv)
			if inv.Kind == "message" && inv.created {
				h.observeStewardResponse(ctx, land, inv.ID, b)
			}
		}
	}
}

// observeLag records how long the event took from its write in kbeads to
// here. A card's subject fan-out carries no write time and is skipped; a clock
// a little behind kbeads' reads as zero, not negative.
func (h *Hub) observeLag(inv Invalidation) {
	if inv.written.IsZero() {
		return
	}
	lag := h.now().Sub(inv.written).Seconds()
	if lag < 0 {
		lag = 0
	}
	h.lag.WithLabelValues(inv.Land, inv.Kind).Observe(lag)
}

// observeStewardResponse feeds a new message to the steward response SLI. b
// is the bead when attribution just read it; a memo hit reads it here.
func (h *Hub) observeStewardResponse(ctx context.Context, land, id string, b *beadsapi.BeadDetail) {
	if b == nil {
		rctx, cancel := context.WithTimeout(beadsapi.WithSystemRead(ctx), lookupTimeout)
		b, _ = h.beads.GetBead(rctx, id)
		cancel()
		if b == nil {
			return
		}
	}
	h.steward.message(ctx, h.beads, land, b, h.now())
}

func (h *Hub) broadcast(inv Invalidation) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		if !s.scope.Allows(inv.Land) || ((inv.Kind == kindNode || inv.Kind == kindJob) && (s.watch == nil || !s.sees(inv))) {
			continue
		}
		select {
		case s.ch <- inv:
		default:
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- inv:
			default:
			}
		}
	}
}

// Publish sends an invalidation that did not come from a bead event (presence,
// typing and jobs) to the subscribers whose scope admits inv.Land, and a job
// only to those that watch its agent. An empty land reaches nobody. Nil-safe.
func (h *Hub) Publish(inv Invalidation) {
	if h == nil || inv.Land == "" {
		return
	}
	h.broadcast(inv)
}

// Subscribe returns the invalidations sc may see, until ctx ends or the
// returned func is called; the channel is closed then. Node events come for
// the beads in watch and those that hang from them (none without a watch).
func (h *Hub) Subscribe(ctx context.Context, sc Scope, watch ...string) (<-chan Invalidation, func()) {
	if h == nil {
		ch := make(chan Invalidation)
		close(ch)
		return ch, func() {}
	}
	s := &hubSub{scope: sc, ch: make(chan Invalidation, hubClientBuffer)}
	if len(watch) > 0 {
		s.watch = make(map[string]bool, len(watch))
		for _, id := range watch {
			s.watch[id] = true
		}
		h.watchers.Add(1)
	}
	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = s
	h.mu.Unlock()

	var once sync.Once
	done := make(chan struct{})
	cancel := func() {
		once.Do(func() {
			close(done)
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, ok := h.subs[id]; ok {
				delete(h.subs, id)
				close(s.ch)
				if s.watch != nil {
					h.watchers.Add(-1)
				}
			}
		})
	}
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				cancel()
			case <-done:
			}
		}()
	}
	return s.ch, cancel
}

// Attribute returns the land of a bead, read from the daemon as the system and
// memoized. ok is false when the read fails or the bead has no single land;
// failures are not memoized.
func (h *Hub) Attribute(ctx context.Context, id string) (string, bool) {
	land, _, ok := h.attribute(ctx, id)
	return land, ok
}

// attribute is Attribute that also returns the bead when it read one (nil on
// a memo hit).
func (h *Hub) attribute(ctx context.Context, id string) (string, *beadsapi.BeadDetail, bool) {
	now := h.now()
	h.memoMu.Lock()
	m, hit := h.memo[id]
	h.memoMu.Unlock()
	if hit && now.Before(m.expires) {
		return m.land, nil, m.land != ""
	}
	ctx, cancel := context.WithTimeout(beadsapi.WithSystemRead(ctx), lookupTimeout)
	defer cancel()
	b, err := h.beads.GetBead(ctx, id)
	if err != nil || b == nil {
		return "", nil, false
	}
	land := landOf(b.Labels)
	h.memoMu.Lock()
	if len(h.memo) >= landMemoCap {
		h.memo = map[string]landMemo{}
	}
	h.memo[id] = landMemo{land: land, expires: now.Add(landMemoTTL)}
	h.memoMu.Unlock()
	return land, b, land != ""
}

// classify reads a bead event payload ({"bead": {...}}) and returns the
// invalidation for a conversation bead: a channel, a message (or card) with a
// channel or thread, a wish head with a channel, or a bead a card renders (an
// agent), which work turns into its cards. Anything else is not the Commons'
// business. The payload's labels are not trusted for the land.
func classify(data json.RawMessage) (Invalidation, bool) {
	var p struct {
		Bead *struct {
			ID        string                     `json:"id"`
			Type      string                     `json:"type"`
			Labels    []string                   `json:"labels"`
			Fields    map[string]json.RawMessage `json:"fields"`
			UpdatedAt string                     `json:"updated_at"` // parsed leniently: a bad stamp must not drop the event
		} `json:"bead"`
	}
	if err := json.Unmarshal(data, &p); err != nil || p.Bead == nil || p.Bead.ID == "" {
		return Invalidation{}, false
	}
	b := p.Bead
	field := func(k string) string {
		var s string
		if raw, ok := b.Fields[k]; ok && json.Unmarshal(raw, &s) == nil {
			return strings.TrimSpace(s)
		}
		return ""
	}
	inv := Invalidation{ID: b.ID, Channel: field(fieldChannelID), Thread: field(fieldThreadID)}
	switch {
	case b.Type == typeChannel:
		inv.Kind, inv.Channel, inv.Thread = "channel", b.ID, ""
	case b.Type == typeMessage && (inv.Channel != "" || inv.Thread != ""):
		inv.Kind = "message"
		if field(fieldKind) == kindCard {
			inv.Kind, inv.subject = "card", field(fieldSubjectID)
		}
	case cardSubjectTypes[b.Type]:
		inv = Invalidation{Kind: kindSubject, ID: b.ID}
	case inv.Channel != "" && hasLabel(b.Labels, labelThread):
		inv.Kind, inv.Thread = "message", ""
	default:
		return Invalidation{}, false
	}
	if inv.Kind != kindSubject {
		if t, err := time.Parse(time.RFC3339Nano, b.UpdatedAt); err == nil {
			inv.written = t
		}
	}
	return inv, true
}

// nodeEvent reads a bead event payload as a node: the bead's id and what it
// hangs from (its parent-child parents, task_id and thread_id). Any bead is a
// node; the hub keeps those a stream watches.
func nodeEvent(data json.RawMessage) (Invalidation, bool) {
	var p struct {
		Bead *struct {
			ID       string `json:"id"`
			ParentID string `json:"parent_id"`
			Deps     []struct {
				BeadID      string `json:"bead_id"`
				DependsOnID string `json:"depends_on_id"`
				Type        string `json:"type"`
			} `json:"dependencies"`
			Fields map[string]json.RawMessage `json:"fields"`
		} `json:"bead"`
	}
	if err := json.Unmarshal(data, &p); err != nil || p.Bead == nil || p.Bead.ID == "" {
		return Invalidation{}, false
	}
	b := p.Bead
	inv := Invalidation{Kind: kindNode, ID: b.ID}
	add := func(id string) {
		if id = strings.TrimSpace(id); id != "" && id != b.ID {
			inv.links = append(inv.links, id)
		}
	}
	add(b.ParentID)
	for _, d := range b.Deps {
		if d.Type == "parent-child" && d.BeadID == b.ID {
			add(d.DependsOnID)
		}
	}
	for _, k := range []string{"task_id", fieldThreadID} {
		var s string
		if raw, ok := b.Fields[k]; ok && json.Unmarshal(raw, &s) == nil {
			add(s)
		}
	}
	return inv, true
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
