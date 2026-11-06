package glass

// The Wall (spec §3.4 #2, §3.5, §6 step 7; GL-5): one upstream coopmux
// /ws/mux subscription for every browser showing a land's live sessions,
// filtered per viewer before it is fanned out, like the per-principal
// BeadFeedHub.
//
// Each viewer is handed the sessions it may see, as a session -> agent bead
// map that gardenhub resolves from the viewer's own agent beads and land
// scope; the Wall never decides scope. The upstream is subscribed to the union
// of those sessions (subscribing is what makes coopmux poll a session's
// screen), so a session nobody may see is never polled for the Wall. A
// viewer's events name the agent bead, never the session.
//
// coopmux pushes a screen_batch at most once a second with the thumbnails that
// changed; each becomes one "screens" event per viewer holding only its tiles,
// so the browser gets its updates batched as coopmux batches them. A viewer too
// slow to keep up has its queue replaced by a "tiles" snapshot: every
// thumbnail is a whole screen, so a snapshot loses nothing but the frames in
// between.

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"time"

	"gardenio/controller/internal/coopmuxapi"
)

// MuxUpstream is the open /ws/mux feed; *coopmuxapi.MuxStream is one.
type MuxUpstream interface {
	Recv() (coopmuxapi.Frame, error)
	SubscribeSessions(sessions ...string) error
	UnsubscribeSessions(sessions ...string) error
	Close() error
}

// DialMux opens the /ws/mux feed for the Identity on ctx
// (coopmuxapi.WithIdentity). ctx bounds the feed's life.
type DialMux func(ctx context.Context) (MuxUpstream, error)

// DialCoopmuxMux is the DialMux over a coopmux client.
func DialCoopmuxMux(c *coopmuxapi.Client) DialMux {
	return func(ctx context.Context) (MuxUpstream, error) {
		s, err := c.SubscribeMux(ctx)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
}

// Wall event kinds sent to a viewer. Every payload names agent beads.
const (
	// WallTiles is a snapshot: {"tiles":[tile...]}, every session the viewer
	// may see that coopmux has now. It replaces whatever the viewer shows.
	WallTiles = "tiles"
	// WallScreens is a batch of changed tiles: {"screens":[tile...]}, each
	// with its new screen and, when the state moved with it, its state.
	WallScreens = "screens"
	// WallState is one tile's state machine moving: {"agent", "state"}.
	WallState = "state"
	// WallOnline is a session the viewer may see coming online: {"agent"}.
	// Its first screen follows in a screens event.
	WallOnline = "online"
	// WallOffline is one going away: {"agent"}.
	WallOffline = "offline"
	// WallEnded is the Wall giving up on coopmux (it refused the feed, or is
	// not reachable from here): {"reason"}. Nothing follows it.
	WallEnded = "ended"
)

// Thumb is a tile's screen as coopmux's screen_batch carries it: the lines
// with their SGR colour codes when coopmux sent them (ANSI), else plain.
type Thumb struct {
	Lines []string `json:"lines"`
	ANSI  bool     `json:"ansi,omitempty"`
	Cols  int      `json:"cols"`
	Rows  int      `json:"rows"`
	Seq   uint64   `json:"seq"`
}

// thumbRowsMax bounds the rows of a thumbnail kept and sent.
const thumbRowsMax = 300

const (
	wallQueueMaxEvents = 256
	wallQueueMaxBytes  = 4 << 20
)

// Wall holds the shared /ws/mux upstream and every viewer of it.
type Wall struct {
	dial   DialMux
	logger *slog.Logger
	// Linger overrides DefaultLinger when positive; set before the first Join.
	Linger time.Duration

	mu      sync.Mutex
	id      coopmuxapi.Identity // a (re)dial is signed for the newest viewer
	gen     int                 // the run frames are applied for; bumped when a run stops
	cancel  context.CancelFunc  // the run, nil when no upstream is open
	viewers map[*WallViewer]struct{}
	idle    *time.Timer
	// primed: the connection's sessions list has arrived.
	primed   bool
	sessions map[string]*wallSession
	// want counts the viewers that may see each session; sent is what the
	// current connection is subscribed to, kick wakes its subscriber.
	want map[string]int
	sent map[string]bool
	kick chan struct{}
}

type wallSession struct {
	word   string // coop's state word, to tell a thumbnail's state is new
	state  []byte // the State payload, nil before any
	screen []byte // the Thumb payload, nil before the first thumbnail
}

// NewWall returns a Wall that opens its upstream with dial.
func NewWall(dial DialMux, logger *slog.Logger) *Wall {
	if logger == nil {
		logger = slog.Default()
	}
	return &Wall{dial: dial, logger: logger, viewers: map[*WallViewer]struct{}{}, want: map[string]int{}}
}

// Open reports whether the upstream is open (or opening).
func (w *Wall) Open() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cancel != nil
}

// Subscribed is the sessions the upstream is asked for, sorted.
func (w *Wall) Subscribed() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.want))
	for s := range w.want {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func (w *Wall) linger() time.Duration {
	if w.Linger > 0 {
		return w.Linger
	}
	return DefaultLinger
}

// Join adds a viewer for the human id that may see the sessions in allow
// (session -> agent bead), opening the upstream if there is none. The viewer
// must be closed.
func (w *Wall) Join(id coopmuxapi.Identity, allow map[string]string) *WallViewer {
	w.mu.Lock()
	defer w.mu.Unlock()
	v := &WallViewer{w: w, notify: make(chan struct{}, 1), allow: map[string]string{}}
	w.id = id
	if w.idle != nil {
		w.idle.Stop()
		w.idle = nil
	}
	w.viewers[v] = struct{}{}
	if w.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		w.cancel = cancel
		go w.run(ctx, w.gen)
	}
	v.setAllow(allow)
	return v
}

// stop ends the current run and forgets what it knew. Called with w.mu held.
func (w *Wall) stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.cancel, w.kick, w.sent = nil, nil, nil
	w.gen++
	w.primed = false
	w.sessions = nil
}

func (w *Wall) current(gen int) bool { return w.gen == gen && w.cancel != nil }

// run dials the upstream and relays it until ctx ends, redialling a dropped
// feed with backoff.
func (w *Wall) run(ctx context.Context, gen int) {
	backoff := redialMin
	for ctx.Err() == nil {
		w.mu.Lock()
		id := w.id
		w.mu.Unlock()
		up, err := w.dial(coopmuxapi.WithIdentity(ctx, id))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if reason := terminal(err); reason != "" {
				w.logger.Info("glass wall: upstream refused", "error", err)
				w.end(gen, reason)
				return
			}
			w.logger.Warn("glass wall: upstream dial failed", "error", err)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, redialMax)
			continue
		}
		backoff = redialMin
		w.relay(ctx, gen, up)
		_ = up.Close()
		if !sleep(ctx, redialMin) {
			return
		}
	}
}

// relay reads one connection until it fails, with a subscriber beside it that
// keeps the connection's subscriptions equal to what the viewers may see.
func (w *Wall) relay(ctx context.Context, gen int, up MuxUpstream) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	kick := make(chan struct{}, 1)
	kick <- struct{}{} // a new connection subscribes to everything wanted
	w.mu.Lock()
	if !w.current(gen) {
		w.mu.Unlock()
		return
	}
	w.kick, w.sent, w.primed = kick, map[string]bool{}, false
	w.mu.Unlock()
	go w.subscriber(cctx, gen, up, kick)
	for {
		fr, err := up.Recv()
		if err != nil {
			if ctx.Err() == nil {
				w.logger.Info("glass wall: upstream dropped", "error", err)
			}
			return
		}
		w.frame(gen, fr)
	}
}

// subscriber sends the subscribe and unsubscribe frames that bring the
// connection's subscriptions to want, each time it is kicked. Sends happen
// outside the lock, so a slow upstream write never holds up the viewers.
func (w *Wall) subscriber(ctx context.Context, gen int, up MuxUpstream, kick chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		}
		w.mu.Lock()
		if !w.current(gen) || w.kick != kick {
			w.mu.Unlock()
			return
		}
		var add, drop []string
		for s := range w.want {
			if !w.sent[s] {
				add = append(add, s)
				w.sent[s] = true
			}
		}
		for s := range w.sent {
			if w.want[s] == 0 {
				drop = append(drop, s)
				delete(w.sent, s)
			}
		}
		w.mu.Unlock()
		sort.Strings(add)
		sort.Strings(drop)
		if err := up.SubscribeSessions(add...); err != nil {
			w.logger.Warn("glass wall: subscribe failed", "error", err)
			_ = up.Close() // the reader sees it and the next connection subscribes afresh
			return
		}
		if err := up.UnsubscribeSessions(drop...); err != nil {
			w.logger.Warn("glass wall: unsubscribe failed", "error", err)
			_ = up.Close()
			return
		}
	}
}

// wake asks the current connection's subscriber to sync. Called with w.mu held.
func (w *Wall) wake() {
	if w.kick == nil {
		return
	}
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// /ws/mux frame shapes (coop/crates/mux/src/transport/ws_mux.rs, state.rs MuxEvent).
type (
	muxSessionsFrame struct {
		Sessions []struct {
			ID    string  `json:"id"`
			State *string `json:"state"`
		} `json:"sessions"`
	}
	muxSessionFrame struct {
		Session string `json:"session"`
	}
	muxTransitionFrame struct {
		Session string `json:"session"`
		stateFrame
	}
	muxScreenBatchFrame struct {
		Screens []struct {
			Session string   `json:"session"`
			Lines   []string `json:"lines"`
			ANSI    []string `json:"ansi"`
			Cols    int      `json:"cols"`
			Rows    int      `json:"rows"`
			Seq     uint64   `json:"seq"`
			State   *string  `json:"state"`
		} `json:"screens"`
	}
)

// wordState is the State a bare state word says: coopmux's cached status on
// its sessions list and thumbnails, which carry no prompt or error category.
func wordState(word string) []byte {
	b, _ := json.Marshal(State{State: word, Attention: word == "prompt" || word == "error"})
	return b
}

// frame folds one upstream frame into the Wall and fans it out.
func (w *Wall) frame(gen int, fr coopmuxapi.Frame) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.current(gen) {
		return
	}
	switch fr.Event {
	case "sessions":
		var m muxSessionsFrame
		if json.Unmarshal(fr.Data, &m) != nil {
			return
		}
		// A new connection's list: what is gone went offline while the
		// Wall was not listening, so every viewer gets a fresh snapshot.
		old, next := w.sessions, map[string]*wallSession{}
		for _, s := range m.Sessions {
			if s.ID == "" {
				continue
			}
			ns := &wallSession{}
			if o := old[s.ID]; o != nil {
				*ns = *o
			}
			if s.State != nil && *s.State != ns.word {
				ns.word, ns.state = *s.State, wordState(*s.State)
			}
			next[s.ID] = ns
		}
		w.sessions, w.primed = next, true
		for v := range w.viewers {
			v.unknown = true // a session it may now see could be among them
			v.reset()
		}
	case "session:online":
		var m muxSessionFrame
		if json.Unmarshal(fr.Data, &m) != nil || m.Session == "" || !w.primed {
			return
		}
		if w.sessions[m.Session] == nil {
			w.sessions[m.Session] = &wallSession{}
		}
		w.each(m.Session, func(agent string) Event { return agentEvent(WallOnline, agent) }, true)
	case "session:offline":
		var m muxSessionFrame
		if json.Unmarshal(fr.Data, &m) != nil || m.Session == "" || !w.primed {
			return
		}
		if w.sessions[m.Session] == nil {
			return
		}
		delete(w.sessions, m.Session)
		w.each(m.Session, func(agent string) Event { return agentEvent(WallOffline, agent) }, false)
	case "transition":
		var m muxTransitionFrame
		if json.Unmarshal(fr.Data, &m) != nil || !w.primed {
			return
		}
		s := w.sessions[m.Session]
		if s == nil {
			return
		}
		st := stateOf(m.stateFrame)
		s.word = st.State
		s.state, _ = json.Marshal(st)
		w.each(m.Session, func(agent string) Event {
			b, _ := json.Marshal(struct {
				Agent string          `json:"agent"`
				State json.RawMessage `json:"state"`
			}{agent, s.state})
			return Event{Kind: WallState, ID: -1, Data: b}
		}, false)
	case "screen_batch":
		var m muxScreenBatchFrame
		if json.Unmarshal(fr.Data, &m) != nil || !w.primed {
			return
		}
		changed := map[string]bool{} // sessions whose state moved with the screen
		var moved []string
		for _, t := range m.Screens {
			s := w.sessions[t.Session]
			if s == nil {
				continue
			}
			th := Thumb{Lines: t.Lines, Cols: t.Cols, Rows: t.Rows, Seq: t.Seq}
			if len(t.ANSI) > 0 && len(t.ANSI) == len(t.Lines) {
				th.Lines, th.ANSI = t.ANSI, true
			}
			if len(th.Lines) > thumbRowsMax {
				th.Lines = th.Lines[len(th.Lines)-thumbRowsMax:]
			}
			if th.Lines == nil {
				th.Lines = []string{}
			}
			s.screen, _ = json.Marshal(th)
			if t.State != nil && *t.State != s.word {
				s.word, s.state = *t.State, wordState(*t.State)
				changed[t.Session] = true
			}
			moved = append(moved, t.Session)
		}
		if len(moved) == 0 {
			return
		}
		for v := range w.viewers {
			var tiles []json.RawMessage
			for _, sid := range moved {
				if agent, ok := v.allow[sid]; ok {
					s := w.sessions[sid]
					var state []byte
					if changed[sid] {
						state = s.state
					}
					tiles = append(tiles, tileJSON(agent, state, s.screen))
				}
			}
			if len(tiles) == 0 {
				continue
			}
			b, _ := json.Marshal(struct {
				Screens []json.RawMessage `json:"screens"`
			}{tiles})
			v.offer(Event{Kind: WallScreens, ID: -1, Data: b})
		}
	case "error":
		w.logger.Warn("glass wall: upstream error frame", "frame", string(fr.Data))
	}
}

// each offers the event ev makes to every viewer that may see session; with
// hint, a viewer that may not is told a session it does not know came online,
// so its handler can look again. Called with w.mu held.
func (w *Wall) each(session string, ev func(agent string) Event, hint bool) {
	for v := range w.viewers {
		if agent, ok := v.allow[session]; ok {
			v.offer(ev(agent))
		} else if hint {
			v.unknown = true
			v.wake()
		}
	}
}

func agentEvent(kind, agent string) Event {
	b, _ := json.Marshal(struct {
		Agent string `json:"agent"`
	}{agent})
	return Event{Kind: kind, ID: -1, Data: b}
}

// tileJSON is one tile: the agent bead, and its state and screen when known.
func tileJSON(agent string, state, screen []byte) json.RawMessage {
	b, _ := json.Marshal(struct {
		Agent  string          `json:"agent"`
		State  json.RawMessage `json:"state,omitempty"`
		Screen json.RawMessage `json:"screen,omitempty"`
	}{agent, state, screen})
	return b
}

// end gives up on coopmux: every viewer gets an ended event and is closed,
// and the next Join dials afresh.
func (w *Wall) end(gen int, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.current(gen) {
		return
	}
	b, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{reason})
	for v := range w.viewers {
		v.push(Event{Kind: WallEnded, ID: -1, Data: b})
		v.closed = true
		v.dropAllow()
		delete(w.viewers, v)
	}
	w.stop()
}

// leave removes v; the last viewer out starts the linger timer.
func (w *Wall) leave(v *WallViewer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.viewers[v]; !ok {
		return
	}
	v.dropAllow()
	delete(w.viewers, v)
	w.wake()
	if len(w.viewers) > 0 || w.cancel == nil {
		return
	}
	gen := w.gen
	w.idle = time.AfterFunc(w.linger(), func() { w.expire(gen) })
}

// expire closes the upstream if nobody came back while it lingered.
func (w *Wall) expire(gen int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.viewers) > 0 || w.gen != gen {
		return
	}
	w.idle = nil
	w.stop()
}

// WallViewer is one browser's Wall: the sessions it may see and a bounded
// queue of its events.
type WallViewer struct {
	w      *Wall
	notify chan struct{}

	// Guarded by w.mu.
	allow   map[string]string // session -> agent bead
	q       []Event
	qBytes  int
	closed  bool
	unknown bool
	resyncs int
}

// Ready is signalled when events are waiting, or a session the viewer does
// not know came online.
func (v *WallViewer) Ready() <-chan struct{} { return v.notify }

// Take returns the waiting events; unknown, whether a session the viewer may
// not see came online since the last Take (its handler may want to resolve
// the viewer's sessions again); and finished, whether the Wall gave up (the
// events then end with an ended one and nothing follows).
func (v *WallViewer) Take() (evs []Event, unknown, finished bool) {
	v.w.mu.Lock()
	defer v.w.mu.Unlock()
	evs, unknown = v.q, v.unknown
	v.q, v.qBytes, v.unknown = nil, 0, false
	return evs, unknown, v.closed
}

// Allow replaces the sessions the viewer may see (session -> agent bead) and
// queues a fresh snapshot in place of what was waiting.
func (v *WallViewer) Allow(allow map[string]string) {
	v.w.mu.Lock()
	defer v.w.mu.Unlock()
	if v.closed {
		return
	}
	v.setAllow(allow)
}

// Resyncs counts the times the viewer fell behind and was sent a snapshot.
func (v *WallViewer) Resyncs() int {
	v.w.mu.Lock()
	defer v.w.mu.Unlock()
	return v.resyncs
}

// Close removes the viewer.
func (v *WallViewer) Close() { v.w.leave(v) }

// setAllow is Allow with w.mu held.
func (v *WallViewer) setAllow(allow map[string]string) {
	w := v.w
	v.dropAllow()
	v.allow = make(map[string]string, len(allow))
	for s, a := range allow {
		if s == "" || a == "" {
			continue
		}
		v.allow[s] = a
		w.want[s]++
	}
	w.wake()
	if w.primed {
		v.reset() // a fresh snapshot supersedes whatever was queued
	}
}

// dropAllow takes the viewer's sessions out of want. Called with w.mu held.
func (v *WallViewer) dropAllow() {
	w := v.w
	for s := range v.allow {
		if w.want[s]--; w.want[s] <= 0 {
			delete(w.want, s)
		}
	}
	v.allow = map[string]string{}
}

// snapshot is the viewer's tiles event: every session it may see that coopmux
// has, by agent. Called with w.mu held.
func (v *WallViewer) snapshot() Event {
	tiles := []json.RawMessage{}
	ids := make([]string, 0, len(v.allow))
	for s := range v.allow {
		ids = append(ids, s)
	}
	sort.Slice(ids, func(i, j int) bool { return v.allow[ids[i]] < v.allow[ids[j]] })
	for _, sid := range ids {
		if s := v.w.sessions[sid]; s != nil {
			tiles = append(tiles, tileJSON(v.allow[sid], s.state, s.screen))
		}
	}
	b, _ := json.Marshal(struct {
		Tiles []json.RawMessage `json:"tiles"`
	}{tiles})
	return Event{Kind: WallTiles, ID: -1, Data: b}
}

// offer queues ev, or replaces the queue with a snapshot when the viewer has
// fallen behind. Called with w.mu held.
func (v *WallViewer) offer(ev Event) {
	if v.closed {
		return
	}
	if len(v.q) > 0 && (len(v.q) >= wallQueueMaxEvents || v.qBytes+len(ev.Data) > wallQueueMaxBytes) {
		v.resyncs++
		v.reset()
		return
	}
	v.push(ev)
}

// reset replaces the queue with a snapshot. Called with w.mu held.
func (v *WallViewer) reset() {
	if v.closed {
		return
	}
	v.q, v.qBytes = nil, 0
	v.push(v.snapshot())
}

func (v *WallViewer) push(ev Event) {
	v.q = append(v.q, ev)
	v.qBytes += len(ev.Data)
	v.wake()
}

func (v *WallViewer) wake() {
	select {
	case v.notify <- struct{}{}:
	default:
	}
}
