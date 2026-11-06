package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// bead_feed_hub.go — viewer-side fan-out hub for the activity feed
// (task ka-0000000643, epic ka-0000004907 Phase 2). Modelled on ThinkingHub
// (thinking_hub.go): ONE upstream EventStream("beads.bead.>") to the daemon is
// fanned out to N browser SSE connections, with a bounded ring buffer for
// instant warm paint on connect.
//
// Why a hub at all: the original /feed handler opened a fresh
// daemon.EventStream per SSE connection (feed.go handleFeedEvents), so N
// viewers meant N upstream HTTP-SSE connections to the daemon, each
// re-filtering the same "beads.bead.>" firehose. That doesn't scale to a
// promoted front page. The hub collapses that to one upstream connection
// regardless of viewer count.
//
// Division of labour mirrors telepathy: this file owns the upstream
// subscription + fan-out + ring; feed.go owns the per-event card rendering and
// the SSE handler that consumes Subscribe().

// beadFeedTopics is the NATS topic glob the hub subscribes to upstream. Matches
// feedEventTopics (feed.go) — bead lifecycle covers create / claim / close / MR
// + decision bead events, the families the feed renders.
const beadFeedTopics = "beads.bead.>"

// beadFeedRingCapacity bounds the number of recent events the hub retains for
// warm paint. A fresh SSE client is replayed this many events (newest of them)
// so it paints immediately instead of waiting for the next fleet mutation.
// Bounded so memory stays flat under a busy fleet — old events fall off front.
const beadFeedRingCapacity = 200

// beadFeedClientBuffer is the per-subscriber channel depth. A slow browser tab
// backs up only this far before the hub drops its oldest queued event rather
// than blocking the upstream reader or starving other clients — the feed is a
// live view, so dropping stale events for one laggard is the right trade.
const beadFeedClientBuffer = 64

// beadFeedReconnectWait is how long the upstream loop waits before re-opening
// the daemon EventStream after it closes (daemon restart, network blip). The
// browser EventSources stay connected to the hub across upstream reconnects —
// they only see a gap in events, which their own ?since= backfill heals.
const beadFeedReconnectWait = 2 * time.Second

// BeadFeedHub subscribes once to the daemon's bead-lifecycle SSE stream and
// fans each event out to all in-process subscribers (the /feed/events SSE
// handler). It keeps a bounded ring of recent events so new subscribers get an
// instant warm paint.
//
// A nil *BeadFeedHub is safe to use: Subscribe returns a closed channel, so the
// SSE handler degrades to a live-only feed (no warm paint) when the hub is not
// wired. This mirrors the nil-safe ThinkingHub pattern.
type BeadFeedHub struct {
	daemon beadAPI
	logger *slog.Logger

	mu      sync.Mutex
	clients map[int]chan beadsapi.SSEEvent
	nextID  int
	ring    []beadsapi.SSEEvent // most-recent-last, bounded to beadFeedRingCapacity

	// projects memoizes bead id -> project labels for scoped subscribers, so each
	// bead is looked up through the system-read client once, not once per event.
	// Bounded: it is reset when it reaches beadFeedProjectCacheCap, and an entry
	// expires after beadFeedProjectTTL so a bead moved between projects stops
	// being attributed to its old one.
	projMu   sync.Mutex
	projects map[string]beadProjectMemo
	// projNow is the memo's clock; nil means time.Now (tests inject one).
	projNow func() time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

// NewBeadFeedHub builds a hub over the daemon client. Returns nil when daemon
// is nil (no upstream to subscribe to) — callers must treat a nil hub as
// "warm-paint disabled", which every method tolerates.
func NewBeadFeedHub(daemon beadAPI, logger *slog.Logger) *BeadFeedHub {
	if daemon == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &BeadFeedHub{
		daemon:  daemon,
		logger:  logger,
		clients: make(map[int]chan beadsapi.SSEEvent),
		done:    make(chan struct{}),
	}
}

// Start launches the upstream subscription loop in a background goroutine. The
// loop opens daemon.EventStream(beadFeedTopics), pumps events into the ring +
// fan-out, and re-opens the stream when it closes (daemon restart, blip) until
// Close cancels it. Safe to call on a nil hub (no-op).
func (h *BeadFeedHub) Start() {
	if h == nil {
		return
	}
	// No human behind the upstream stream: an explicit system read.
	ctx, cancel := context.WithCancel(beadsapi.WithSystemRead(context.Background()))
	h.cancel = cancel
	go h.run(ctx)
}

// run is the upstream pump: (re)connect to the daemon SSE stream and broadcast
// every event until the context is canceled. On stream close it waits
// beadFeedReconnectWait and retries, so a daemon restart self-heals without
// dropping the browser-facing subscribers.
func (h *BeadFeedHub) run(ctx context.Context) {
	defer close(h.done)
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := h.daemon.EventStream(ctx, beadFeedTopics)
		if err != nil {
			h.logger.Warn("bead feed hub: opening upstream stream — retrying", "error", err)
			if !sleepCtx(ctx, beadFeedReconnectWait) {
				return
			}
			continue
		}
		h.logger.Info("bead feed hub: upstream stream open", "topics", beadFeedTopics)
		h.pump(ctx, events)
		// pump returned: either ctx canceled or the upstream channel closed.
		if ctx.Err() != nil {
			return
		}
		h.logger.Info("bead feed hub: upstream stream closed — reconnecting")
		if !sleepCtx(ctx, beadFeedReconnectWait) {
			return
		}
	}
}

// pump drains one upstream stream into the broadcast path until it closes or
// the context is canceled.
func (h *BeadFeedHub) pump(ctx context.Context, events <-chan beadsapi.SSEEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			h.broadcast(evt)
		}
	}
}

// broadcast appends to the ring and pushes to every subscriber. A subscriber
// whose buffer is full has its oldest queued event dropped to make room, so a
// slow client never blocks the upstream reader or starves its peers.
func (h *BeadFeedHub) broadcast(evt beadsapi.SSEEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.ring = append(h.ring, evt)
	if len(h.ring) > beadFeedRingCapacity {
		h.ring = h.ring[len(h.ring)-beadFeedRingCapacity:]
	}

	for _, ch := range h.clients {
		select {
		case ch <- evt:
		default:
			// Buffer full: drop the oldest queued event, then enqueue the new
			// one. Best-effort so a wedged client degrades to "latest only".
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- evt:
			default:
			}
		}
	}
}

// Subscribe registers a new in-process consumer of the bead feed. It returns a
// receive-only channel of events and an unsubscribe func that must be called
// (typically via defer) when the consumer is done — the channel is closed on
// unsubscribe and when the hub closes.
//
// The returned channel is first backfilled with the hub's recent ring (oldest
// first) so a fresh SSE client paints immediately, then receives live events.
// On a nil hub it returns an already-closed channel and a no-op cancel.
func (h *BeadFeedHub) Subscribe(ctx context.Context) (<-chan beadsapi.SSEEvent, func()) {
	if h == nil {
		ch := make(chan beadsapi.SSEEvent)
		close(ch)
		return ch, func() {}
	}

	h.mu.Lock()
	id := h.nextID
	h.nextID++
	ch := make(chan beadsapi.SSEEvent, beadFeedClientBuffer)
	// Backfill the recent ring before going live. If the ring exceeds the
	// buffer, keep the most recent (the warm paint is a glance, not history;
	// the client's ?since= backfill covers the durable tail).
	backfill := h.ring
	if len(backfill) > cap(ch) {
		backfill = backfill[len(backfill)-cap(ch):]
	}
	for _, e := range backfill {
		select {
		case ch <- e:
		default:
		}
	}
	h.clients[id] = ch
	h.mu.Unlock()

	var once sync.Once
	unsubscribed := make(chan struct{})
	cancel := func() {
		once.Do(func() {
			close(unsubscribed)
			h.mu.Lock()
			defer h.mu.Unlock()
			if c, ok := h.clients[id]; ok {
				delete(h.clients, id)
				close(c)
			}
		})
	}

	// Auto-unsubscribe when the caller's context is done so a dropped SSE
	// connection doesn't leak a client entry / goroutine. ctx.Done() is nil
	// for non-cancellable contexts (context.Background()/TODO()) — receiving
	// from a nil channel blocks forever, so the watcher would park for the
	// life of the process (bug ka-0000005687); skip it, there is nothing to
	// watch. The unsubscribed arm unwinds the watcher as soon as the caller
	// cancels explicitly, so a finished subscriber never keeps a goroutine
	// parked until its (possibly much longer-lived) context fires.
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				cancel()
			case <-unsubscribed:
			}
		}()
	}

	return ch, cancel
}

// beadFeedProjectCacheCap bounds the bead id -> project memo behind scoped
// subscriptions. A full memo is dropped wholesale: a miss only costs one
// lookup, and the working set of beads active at once is far below this.
const beadFeedProjectCacheCap = 4096

// beadFeedProjectTTL is how long a bead's memoized project labels are trusted.
// Nothing invalidates the memo on a label change, so this bounds how long a
// scoped subscriber can keep seeing (or being denied) a bead after it moves
// between projects; the cost of expiry is one more system read per bead.
const beadFeedProjectTTL = time.Minute

// beadProjectMemo is one memoized project-label lookup.
type beadProjectMemo struct {
	labels  []string
	expires time.Time
}

// beadFeedScopedBuffer is the channel depth of a scoped subscriber's filtered
// stream, matching the unscoped client buffer.
const beadFeedScopedBuffer = beadFeedClientBuffer

// SubscribeFor is Subscribe narrowed to what sc may see. An unrestricted scope
// gets the raw stream. A scoped one gets only events whose bead carries a
// project: label in its scope: the hub's upstream stream and ring hold every
// project's events (they are read on the viewer's own token), so the cut has to
// happen here, per subscriber, before anything reaches a browser.
//
// The bead is identified from the event payload but its project is read from the
// daemon (system read, memoized per bead id), never from the payload's own
// labels. An event that cannot be attributed, because the payload has no bead id
// or the lookup fails, is dropped: the filter fails closed.
//
// A nil hub returns a closed channel, like Subscribe.
func (h *BeadFeedHub) SubscribeFor(ctx context.Context, sc viewerScope) (<-chan beadsapi.SSEEvent, func()) {
	raw, cancel := h.Subscribe(ctx)
	if h == nil || sc.Admin {
		return raw, cancel
	}
	out := make(chan beadsapi.SSEEvent, beadFeedScopedBuffer)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case evt, ok := <-raw:
				if !ok {
					return
				}
				if !h.eventVisible(ctx, evt, sc) {
					continue
				}
				select {
				case out <- evt:
				default:
					// Slow tab: drop its oldest, as the unscoped fan-out does.
					select {
					case <-out:
					default:
					}
					select {
					case out <- evt:
					default:
					}
				}
			}
		}
	}()
	var once sync.Once
	return out, func() {
		once.Do(func() {
			close(done)
			cancel()
		})
	}
}

// eventVisible reports whether sc may see evt, attributing the event to its
// bead's project labels via the daemon.
func (h *BeadFeedHub) eventVisible(ctx context.Context, evt beadsapi.SSEEvent, sc viewerScope) bool {
	var wrapper struct {
		Bead *struct {
			ID string `json:"id"`
		} `json:"bead"`
	}
	if err := json.Unmarshal(evt.Data, &wrapper); err != nil || wrapper.Bead == nil || wrapper.Bead.ID == "" {
		return false
	}
	labels, ok := h.beadProjectLabels(ctx, wrapper.Bead.ID)
	return ok && sc.allowsAny(labels)
}

// beadProjectLabels returns the project: labels of a bead, read through the
// daemon as the system (there is no per-subscriber bearer on a shared hub, and
// the answer is only ever compared with a subscriber's scope, never shown).
// ok is false when the lookup failed; failures are not memoized.
func (h *BeadFeedHub) beadProjectLabels(ctx context.Context, id string) ([]string, bool) {
	now := time.Now
	if h.projNow != nil {
		now = h.projNow
	}
	h.projMu.Lock()
	memo, hit := h.projects[id]
	h.projMu.Unlock()
	if hit && now().Before(memo.expires) {
		return memo.labels, true
	}
	b, err := h.daemon.GetBead(beadsapi.WithSystemRead(ctx), id)
	if err != nil || b == nil {
		return nil, false
	}
	var projects []string
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "project:") {
			projects = append(projects, l)
		}
	}
	h.projMu.Lock()
	if h.projects == nil || len(h.projects) >= beadFeedProjectCacheCap {
		h.projects = make(map[string]beadProjectMemo)
	}
	h.projects[id] = beadProjectMemo{labels: projects, expires: now().Add(beadFeedProjectTTL)}
	h.projMu.Unlock()
	return projects, true
}

// Recent returns a snapshot copy of the buffered events, oldest first. Used by
// tests and any non-streaming surface. Nil-safe.
func (h *BeadFeedHub) Recent() []beadsapi.SSEEvent {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ring) == 0 {
		return nil
	}
	out := make([]beadsapi.SSEEvent, len(h.ring))
	copy(out, h.ring)
	return out
}

// Close cancels the upstream subscription loop and closes every subscriber
// channel so SSE handlers blocked on a receive unwind. Safe on a nil hub.
func (h *BeadFeedHub) Close() {
	if h == nil {
		return
	}
	if h.cancel != nil {
		h.cancel()
		// Wait for the upstream loop to unwind so we don't race broadcast()
		// against the client teardown below.
		<-h.done
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.clients {
		close(ch)
		delete(h.clients, id)
	}
}

// sleepCtx sleeps for d or until ctx is canceled. Returns true if the full
// duration elapsed, false if the context was canceled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
