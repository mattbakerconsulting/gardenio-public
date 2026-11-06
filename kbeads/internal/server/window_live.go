package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// The live window (Lands: the Window, plan gd-pz87vf3xsB, PW.7 gd-nBkInNpy3r;
// narrative §6 step 7, PW-D6; owner decision gd-b3Vx5jVtFh). GET
// /v1/windows/self/live is the window bearer's second route: an SSE stream of
// the projection GET /v1/windows/self/view returns, sent again each time it
// changes, so the /w/ page updates in about a second instead of up to 20.
//
// A frame is always a whole view, built at delivery by the view route's own
// code (windowPlanSubtree, readWindowClosure, projectWindowClosure) from the
// closure as it is then. No bead event, id or field leaves: the stream can
// show nothing the view route would not.
//
// The SSE hub is not changed: the feed is a tap (sseHub.addTap), as the land
// feed is, and sees only each event's topic, bead id and, for an edge, the
// bead it points at. Streams are keyed by window, and each watched window has
// one goroutine that builds its view, so daemon reads scale with watched
// windows, not with visitors:
//
//   - an event on a bead in the window's closure as of its last build (the
//     plan, its descendants, their crew, and the window bead itself), or an
//     edge added to or removed from one, marks the window dirty;
//   - the watcher waits a second (settle), so a burst of writes is one build,
//     and builds at most every 3 s (minGap);
//   - it builds every minute (heartbeat) anyway, which catches a change no
//     event names and an expiry; a revoke writes the window bead, which is in
//     the set, so it is seen at once.
//
// Each build reads the window bead again; a window that no longer admits its
// bearer (revoked, expired, closed, its plan gone or too large) ends every
// stream on it with a "gone" frame. A frame goes out only when the
// projection's bytes changed.
//
// A stream opens only once the window's first build (or its watcher's last
// view) has said the window still streams: a gone window is refused with 410
// before the 200, so the browser's EventSource closes instead of reconnecting
// every few seconds into another build. Keepalives are SSE comments every 15
// s, which the viewer relays to the page as proof the stream is flowing.
//
// Caps: windowLiveMaxPerWindow streams per window, past which the route
// answers 429 and the page keeps its 20 s poll. The viewer caps streams per
// visitor IP, which only it can see. A stream ends after 15 minutes
// (maxLifetime), so the client comes back through the edge's limits
// and this route's bearer check. Only a plan window streams; a land window's
// page polls (404 here).

// windowLivePattern is the window bearer's stream.
const windowLivePattern = "GET /v1/windows/self/live"

// windowLivePath is windowLivePattern's path.
const windowLivePath = "/v1/windows/self/live"

// windowLiveMaxPerWindow caps one window's concurrent streams.
const windowLiveMaxPerWindow = 32

// windowLiveBuildTimeout bounds one build.
const windowLiveBuildTimeout = 10 * time.Second

// windowLiveTiming is the feed's clock (see above); a test shrinks its own
// feed's.
type windowLiveTiming struct {
	settle, minGap, heartbeat, maxLifetime, keepalive time.Duration
}

var windowLiveDefaults = windowLiveTiming{
	settle:      time.Second,
	minGap:      3 * time.Second,
	heartbeat:   time.Minute,
	maxLifetime: 15 * time.Minute,
	keepalive:   15 * time.Second,
}

// errWindowLiveGone means the window no longer admits its bearer, or its plan
// can no longer be shown: every stream on it ends.
var errWindowLiveGone = errors.New("window no longer live")

// errWindowLiveFull means the window has windowLiveMaxPerWindow streams.
var errWindowLiveFull = errors.New("window has too many live streams")

// windowLiveBuilds counts the watchers' builds by outcome: sent (the view
// changed and went out), unchanged, gone (the streams ended) or error (kept
// the last view; the next build retries).
var windowLiveBuilds = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_window_live_builds_total",
	Help: "Live window view builds, by outcome (sent, unchanged, gone, error).",
}, []string{"outcome"})

// windowLiveBuild builds a window's view: the frame's data and the bead ids
// whose events should wake the window.
type windowLiveBuild func(ctx context.Context, windowID string) (frame []byte, watch []string, err error)

// windowLiveFeed is every watched window.
type windowLiveFeed struct {
	build  windowLiveBuild
	timing windowLiveTiming

	// mu guards windows and every watch's subs, ids and last, and is held
	// for every send on a stream's channel, so a channel has one sender at a
	// time.
	mu      sync.Mutex
	windows map[string]*windowWatch
}

// windowWatch is one watched window: its streams and its watcher's state.
type windowWatch struct {
	id    string
	subs  map[*windowLiveSub]struct{}
	ids   map[string]bool
	last  []byte
	dirty chan struct{}
	stop  chan struct{}
}

// windowLiveSub is one stream. ch holds the newest frame not yet written;
// an older one is replaced, never queued, since each frame is a whole view.
// gone is closed when the window ends.
type windowLiveSub struct {
	ch   chan []byte
	gone chan struct{}
}

func newWindowLiveFeed(build windowLiveBuild) *windowLiveFeed {
	return &windowLiveFeed{build: build, timing: windowLiveDefaults, windows: map[string]*windowWatch{}}
}

// publish is the hub tap: it marks dirty each watched window whose last
// closure holds the event's bead or, for an edge, the bead it points at. It
// runs on the publisher's goroutine and does not block.
func (f *windowLiveFeed) publish(evt *sseEvent) {
	f.mu.Lock()
	idle := len(f.windows) == 0
	f.mu.Unlock()
	if idle {
		return
	}
	other := edgeTarget(evt)
	if evt.BeadID == "" && other == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.windows {
		if w.ids[evt.BeadID] || w.ids[other] {
			select {
			case w.dirty <- struct{}{}:
			default:
			}
		}
	}
}

// edgeTarget is the bead an edge event points at (depends_on_id), "" for
// every other event. A step added under a phase, or crew assigned to a step,
// is an edge onto a closure bead from one that is not in it yet.
func edgeTarget(evt *sseEvent) string {
	switch evt.Topic {
	case events.TopicDependencyAdded:
		var p events.DependencyAdded
		if json.Unmarshal(evt.Data, &p) == nil && p.Dependency != nil {
			return p.Dependency.DependsOnID
		}
	case events.TopicDependencyRemoved:
		var p events.DependencyRemoved
		if json.Unmarshal(evt.Data, &p) == nil {
			return p.DependsOnID
		}
	}
	return ""
}

// subscribe opens a stream on window id, starting its watcher if it has none.
// The stream gets the window's last view at once when there is one.
func (f *windowLiveFeed) subscribe(id string) (*windowLiveSub, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.windows[id]
	if w == nil {
		w = &windowWatch{
			id: id, subs: map[*windowLiveSub]struct{}{}, ids: map[string]bool{id: true},
			dirty: make(chan struct{}, 1), stop: make(chan struct{}),
		}
		f.windows[id] = w
		go f.watch(w)
	}
	if len(w.subs) >= windowLiveMaxPerWindow {
		return nil, errWindowLiveFull
	}
	s := &windowLiveSub{ch: make(chan []byte, 1), gone: make(chan struct{})}
	if w.last != nil {
		s.ch <- w.last
	}
	w.subs[s] = struct{}{}
	return s, nil
}

// unsubscribe ends a stream, and the window's watcher with its last stream.
func (f *windowLiveFeed) unsubscribe(id string, s *windowLiveSub) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.windows[id]
	if w == nil {
		return
	}
	if _, ok := w.subs[s]; !ok {
		// Ended with its window; id may be watched again, by other streams.
		return
	}
	delete(w.subs, s)
	if len(w.subs) == 0 {
		delete(f.windows, id)
		close(w.stop)
	}
}

// watch is a window's watcher: build now, then whenever the window is dirty
// or the heartbeat fires, until its last stream ends or the window does.
func (f *windowLiveFeed) watch(w *windowWatch) {
	heartbeat := time.NewTicker(f.timing.heartbeat)
	defer heartbeat.Stop()
	var lastBuild time.Time
	for first := true; ; first = false {
		if !first {
			select {
			case <-w.stop:
				return
			case <-heartbeat.C:
			case <-w.dirty:
				wait := max(f.timing.settle, time.Until(lastBuild.Add(f.timing.minGap)))
				t := time.NewTimer(wait)
				select {
				case <-w.stop:
					t.Stop()
					return
				case <-t.C:
				}
			}
		}
		// A mark made before this build is covered by it.
		select {
		case <-w.dirty:
		default:
		}
		lastBuild = time.Now()
		if !f.rebuild(w) {
			return
		}
	}
}

// rebuild builds w's view and sends it to w's streams if it changed. It
// returns false when the window is gone and its streams have been ended.
func (f *windowLiveFeed) rebuild(w *windowWatch) bool {
	ctx, cancel := context.WithTimeout(context.Background(), windowLiveBuildTimeout)
	frame, watch, err := f.build(ctx, w.id)
	cancel()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case errors.Is(err, errWindowLiveGone):
		windowLiveBuilds.WithLabelValues("gone").Inc()
		for s := range w.subs {
			close(s.gone)
		}
		w.subs = map[*windowLiveSub]struct{}{}
		if f.windows[w.id] == w {
			delete(f.windows, w.id)
		}
		return false
	case err != nil:
		windowLiveBuilds.WithLabelValues("error").Inc()
		slog.Warn("window live: build failed, keeping the last view", "window", w.id, "error", err)
		return true
	}
	w.ids = make(map[string]bool, len(watch)+1)
	w.ids[w.id] = true
	for _, id := range watch {
		w.ids[id] = true
	}
	if bytes.Equal(frame, w.last) {
		windowLiveBuilds.WithLabelValues("unchanged").Inc()
		return true
	}
	windowLiveBuilds.WithLabelValues("sent").Inc()
	w.last = frame
	for s := range w.subs {
		select {
		case <-s.ch:
		default:
		}
		s.ch <- frame
	}
	return true
}

// windowLiveFrame is the feed's build over the store: the window bead read
// again, then its plan's closure projected exactly as GET
// /v1/windows/self/view projects it. The watch set is the closure and its
// crew.
func (s *BeadsServer) windowLiveFrame(ctx context.Context, windowID string) ([]byte, []string, error) {
	got, err := s.store.GetBeadsByIDs(ctx, []string{windowID})
	if err != nil {
		return nil, nil, fmt.Errorf("reading window: %w", err)
	}
	if len(got) != 1 || !windowLive(got[0], time.Now()) {
		return nil, nil, errWindowLiveGone
	}
	wf := windowFields(got[0])
	if wf.kind() != windowTargetPlan {
		return nil, nil, errWindowLiveGone
	}
	plan, err := s.store.GetBead(ctx, wf.Target)
	if err != nil || plan == nil || plan.Type != model.TypeEpic {
		return nil, nil, errWindowLiveGone
	}
	desc, err := s.windowPlanSubtree(ctx, plan, windowPlanFilter(), windowClosureMax)
	if errors.Is(err, errWindowClosureTooLarge) {
		return nil, nil, errWindowLiveGone
	}
	if err != nil {
		return nil, nil, err
	}
	ids := windowClosureIDs(plan, desc)
	reads, err := s.readWindowClosure(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	v := projectWindowClosure(plan, desc, reads)
	v.ExpiresAt = wf.expiresAt()
	frame, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return frame, append(ids, slices.Collect(maps.Keys(reads.agents))...), nil
}

// handleWindowLive handles GET /v1/windows/self/live (see above). Every
// caller but a plan window's bearer is refused; a window at its cap is 429,
// and a window that no longer streams is 410.
func (s *BeadsServer) handleWindowLive(w http.ResponseWriter, r *http.Request) {
	win, ok := windowPrincipalFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusForbidden, "only a window bearer reads this route")
		return
	}
	if s.windowLive == nil || windowFields(win).kind() != windowTargetPlan {
		writeError(w, http.StatusNotFound, "this window has no live stream; poll "+windowViewPath)
		return
	}
	sub, err := s.windowLive.subscribe(win.ID)
	if err != nil {
		writeError(w, http.StatusTooManyRequests, "this window has too many live readers; poll "+windowViewPath)
		return
	}
	defer s.windowLive.unsubscribe(win.ID, sub)

	// Wait for the first view, or for the build that finds the window gone,
	// before saying 200. A build that only errors keeps the stream waiting
	// for at most one keepalive; it then opens and the next build retries.
	var first []byte
	wait := time.NewTimer(s.windowLive.timing.keepalive)
	select {
	case <-r.Context().Done():
		wait.Stop()
		return
	case <-sub.gone:
		wait.Stop()
		writeError(w, http.StatusGone, "this window has no live stream any more; poll "+windowViewPath)
		return
	case first = <-sub.ch:
	case <-wait.C:
	}
	wait.Stop()

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "private, no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if first != nil {
		if _, err := fmt.Fprintf(w, "event: view\ndata: %s\n\n", first); err != nil {
			return
		}
	}
	if rc.Flush() != nil {
		return
	}

	ctx := r.Context()
	lifetime := time.NewTimer(s.windowLive.timing.maxLifetime)
	defer lifetime.Stop()
	keepalive := time.NewTicker(s.windowLive.timing.keepalive)
	defer keepalive.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case <-lifetime.C:
			return
		case <-sub.gone:
			// The window ended while streaming: say so, so the viewer tells
			// the page to stop; a reconnect would be the 410 above.
			_, _ = fmt.Fprint(w, "event: gone\ndata: {}\n\n")
			_ = rc.Flush()
			return
		case <-keepalive.C:
			_, err = fmt.Fprint(w, ":keepalive\n\n")
		case frame := <-sub.ch:
			_, err = fmt.Fprintf(w, "event: view\ndata: %s\n\n", frame)
		}
		if err != nil || rc.Flush() != nil {
			return
		}
	}
}
