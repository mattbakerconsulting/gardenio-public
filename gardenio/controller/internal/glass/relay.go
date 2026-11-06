package glass

// The read side of the Glass relay (spec §3.5, §6 step 3; GL-1, GL-4, GL-6): one
// upstream coopmux /ws/{session} subscription per watched session, shared by
// every browser watching it, fanned out as SSE events by gardenhub's
// /garden/glass/{agent}/stream.
//
// The relay keeps a byte ring of the session's recent PTY output, seeded by the
// upstream replay and extended by its pty frames with coop's replay-gate rule
// (coop/crates/web/src/lib/replay-gate.ts): each byte is relayed once, in
// order. Every byte-bearing event carries the stream offset after it as its SSE
// id, so a browser that reconnects with Last-Event-ID resumes from the ring;
// one that fell further back than the ring, or one too slow to keep up, gets a
// "screen" resync (the latest screen snapshot) instead of partial bytes, so one
// phone on a bad network never stalls the upstream for anyone else.
//
// A replay or screen event also carries "modes": the DEC private modes the app
// has set, as the bytes that set them (alternate screen, mouse tracking, SGR
// mouse reports), taken from coop's screen frame. The ring is a window, so the
// bytes that set them are usually long gone, and a terminal that resets and
// paints from the ring would never learn that the app takes the mouse (its
// wheel would scroll a local scrollback instead of reaching the app,
// gd-bftIGCMNVc). The browser writes them right after its reset.
//
// Scope and tier checks are the caller's: the relay is handed a session only
// after gardenhub decided the human may watch it.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"gardenio/controller/internal/coopmuxapi"
)

// Upstream is one open per-session stream to coopmux; *coopmuxapi.Stream is one.
type Upstream interface {
	Recv() (coopmuxapi.Frame, error)
	Send(frame any) error
	Close() error
}

// Dial opens the per-session upstream for the Identity on ctx
// (coopmuxapi.WithIdentity). ctx bounds the stream's life.
type Dial func(ctx context.Context, session string, topics []string) (Upstream, error)

// DialCoopmux is the Dial over a coopmux client.
func DialCoopmux(c *coopmuxapi.Client) Dial {
	return func(ctx context.Context, session string, topics []string) (Upstream, error) {
		s, err := c.Subscribe(ctx, session, topics)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
}

// Topics are the upstream subscription flags the relay asks for: the bytes, the
// screen snapshots it resyncs with, the state machine and the usage counters.
var Topics = []string{"pty", "screen", "state", "usage"}

// Event kinds sent to a watcher.
const (
	// EventReplay is the ring from Offset to NextOffset: the watcher resets its
	// terminal and writes the bytes. Sent first to a new watcher, and to every
	// watcher when the upstream came back after losing bytes.
	EventReplay = "replay"
	// EventPty is live bytes at Offset, contiguous with what the watcher has.
	EventPty = "pty"
	// EventScreen is a resync: the watcher resets its terminal and paints the
	// lines; the bytes that follow continue from the event's id.
	EventScreen = "screen"
	// EventState is the agent's state machine (GL-4).
	EventState = "state"
	// EventUsage is coop's usage frame as coop sent it, "event" tag and all
	// ("usage" or "usage:update").
	EventUsage = "usage"
	// EventExit is the agent process exiting.
	EventExit = "exit"
	// EventEnded is the relay giving up on the session (it is gone, or coopmux
	// refused the stream); nothing follows it.
	EventEnded = "ended"
)

// Event is one SSE event for a watcher.
type Event struct {
	Kind string
	// ID is the stream offset after this event's bytes, the SSE id a reconnect
	// sends back as Last-Event-ID; -1 on an event that carries no bytes.
	ID int64
	// Data is the event's JSON payload.
	Data []byte
}

const (
	// DefaultLinger is how long an upstream stays open after its last watcher
	// leaves, so a reload or a quick reopen does not redial (spec §3.5).
	DefaultLinger = 30 * time.Second
	// ringCap bounds the PTY bytes kept per session.
	ringCap = 512 << 10
	// A watcher's queue past either bound is replaced by a resync.
	queueMaxEvents = 512
	queueMaxBytes  = 2 * ringCap
	redialMin      = time.Second
	redialMax      = 15 * time.Second
)

// Relay holds the shared upstream of every watched session.
type Relay struct {
	dial   Dial
	logger *slog.Logger
	// Linger overrides DefaultLinger when positive; set before the first Watch.
	Linger time.Duration
	// Lease overrides DefaultLease, how long a keyboard claim lasts unrenewed
	// (control.go), when positive; set before the first Claim.
	Lease time.Duration

	mu       sync.Mutex
	feeds    map[string]*feed
	controls map[string]*claim // who has each session's keyboard (control.go)
}

// NewRelay returns a relay that opens upstreams with dial.
func NewRelay(dial Dial, logger *slog.Logger) *Relay {
	if logger == nil {
		logger = slog.Default()
	}
	return &Relay{dial: dial, logger: logger, feeds: map[string]*feed{}, controls: map[string]*claim{}}
}

// Upstreams is the number of sessions with an open (or opening) upstream.
func (r *Relay) Upstreams() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.feeds)
}

// Watch adds a watcher of session for the human id, sharing the session's
// upstream with every other watcher and opening it if there is none.
// lastEventID is the browser's Last-Event-ID, or -1 for a fresh watch. The
// watcher must be closed.
func (r *Relay) Watch(session string, id coopmuxapi.Identity, lastEventID int64) *Watcher {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.feeds[session]
	if f == nil {
		f = r.newFeed(session, id)
		r.feeds[session] = f
	}
	w := &Watcher{f: f, notify: make(chan struct{}, 1)}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.id = id // a redial is signed for the newest watcher
	if f.idle != nil {
		f.idle.Stop()
		f.idle = nil
	}
	f.watchers[w] = struct{}{}
	f.control = nil
	if cl := r.controls[session]; cl != nil {
		f.control = controlEvent(cl.c).Data
	}
	f.join(w, lastEventID)
	return w
}

func (r *Relay) linger() time.Duration {
	if r.Linger > 0 {
		return r.Linger
	}
	return DefaultLinger
}

// feed is one session's shared upstream and the terminal state it has relayed.
type feed struct {
	r       *Relay
	session string
	cancel  context.CancelFunc

	mu       sync.Mutex
	id       coopmuxapi.Identity
	watchers map[*Watcher]struct{}
	idle     *time.Timer
	// primed: the first replay has arrived; until then pty frames are dropped.
	primed bool
	// ring holds the PTY bytes [ringStart, ringStart+len(ring)).
	ring      []byte
	ringStart int64
	// asked: a replay:get is outstanding after a pty frame skipped ahead.
	asked  bool
	screen []byte // the last screen payload, for resyncs
	modes  string // the last screen's DEC private modes, as the bytes that set them (screenFrame.Modes)
	// cols, rows are the last screen's geometry, 0 before one arrives.
	cols, rows int
	state      []byte // the last state payload
	usage      []byte // the last usage payload
	// control is who holds the keyboard (control.go), nil while nobody does;
	// set by the relay under its own lock, then this one.
	control []byte
	ended   []byte // the ended payload, once the feed has given up
}

func (r *Relay) newFeed(session string, id coopmuxapi.Identity) *feed {
	ctx, cancel := context.WithCancel(context.Background())
	f := &feed{r: r, session: session, cancel: cancel, id: id, watchers: map[*Watcher]struct{}{}}
	go f.run(ctx)
	return f
}

func (f *feed) next() int64 { return f.ringStart + int64(len(f.ring)) }

// run dials the upstream and relays it until ctx ends or the session is gone,
// redialling a dropped stream with backoff.
func (f *feed) run(ctx context.Context) {
	backoff := redialMin
	for ctx.Err() == nil {
		f.mu.Lock()
		id := f.id
		f.mu.Unlock()
		up, err := f.r.dial(coopmuxapi.WithIdentity(ctx, id), f.session, Topics)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if reason := terminal(err); reason != "" {
				f.r.logger.Info("glass: upstream refused", "session", f.session, "error", err)
				f.end(reason)
				return
			}
			f.r.logger.Warn("glass: upstream dial failed", "session", f.session, "error", err)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, redialMax)
			continue
		}
		backoff = redialMin
		f.relay(ctx, up)
		_ = up.Close()
		if !sleep(ctx, redialMin) { // a flapping upstream is not redialled in a hot loop
			return
		}
	}
}

// terminal names why a dial error ends the feed, or "" for one worth retrying.
func terminal(err error) string {
	switch {
	case errors.Is(err, coopmuxapi.ErrSessionNotFound):
		return "gone"
	case errors.Is(err, coopmuxapi.ErrUnauthorized), errors.Is(err, coopmuxapi.ErrNoIdentity):
		return "refused"
	case errors.Is(err, coopmuxapi.ErrNoRelayKey), errors.Is(err, coopmuxapi.ErrDisabled):
		return "unavailable"
	}
	return ""
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// relay reads one upstream connection until it fails. It first asks for the
// agent's state, its usage and the replay from where the ring ends, so a
// redial fills the bytes the dropped connection missed.
func (f *feed) relay(ctx context.Context, up Upstream) {
	f.mu.Lock()
	from := int64(0)
	if f.primed {
		from = f.next()
	}
	f.asked = true
	f.mu.Unlock()
	for _, q := range []map[string]any{
		{"event": "agent:get"},
		{"event": "usage:get"},
		{"event": "screen:get", "cursor": true},
		{"event": "replay:get", "offset": from},
	} {
		if err := up.Send(q); err != nil {
			f.r.logger.Warn("glass: upstream send failed", "session", f.session, "error", err)
			return
		}
	}
	for {
		fr, err := up.Recv()
		if err != nil {
			if ctx.Err() == nil {
				f.r.logger.Info("glass: upstream dropped", "session", f.session, "error", err)
			}
			return
		}
		if req := f.frame(fr); req != nil {
			if err := up.Send(req); err != nil {
				return
			}
		}
	}
}

// Upstream frame shapes (coop/crates/cli/src/transport/ws_msg.rs ServerMessage).
type (
	replayFrame struct {
		Data       string `json:"data"`
		Offset     int64  `json:"offset"`
		NextOffset int64  `json:"next_offset"`
	}
	ptyFrame struct {
		Data   string `json:"data"`
		Offset int64  `json:"offset"`
	}
	screenFrame struct {
		Lines     []string `json:"lines"`
		Cols      int      `json:"cols"`
		Rows      int      `json:"rows"`
		AltScreen bool     `json:"alt_screen"`
		// MouseTracking is the tracking mode the app set (9, 1000, 1002, 1003; 0 for none), MouseSGR whether it
		// asked for SGR reports (1006): what coop's input/mouse encodes by (coop mouse.rs ModeScanner).
		MouseTracking int             `json:"mouse_tracking,omitempty"`
		MouseSGR      bool            `json:"mouse_sgr,omitempty"`
		Cursor        json.RawMessage `json:"cursor,omitempty"`
		Seq           uint64          `json:"seq"`
		// Modes is what the relay adds for the browser: the bytes that set the modes above, "" when none is set.
		Modes string `json:"modes,omitempty"`
	}
	// stateFrame covers both "agent" (the agent:get answer) and "transition".
	stateFrame struct {
		State         string          `json:"state"`
		Prev          string          `json:"prev"`
		Next          string          `json:"next"`
		Prompt        json.RawMessage `json:"prompt"`
		ErrorDetail   string          `json:"error_detail"`
		ErrorCategory string          `json:"error_category"`
		ParkedReason  string          `json:"parked_reason"`
		ResumeAtMs    uint64          `json:"resume_at_epoch_ms"`
		LastMessage   string          `json:"last_message"`
	}
)

// State is the EventState payload (GL-4): coop's state machine, with
// rate_limited lifted out of error_category and attention set for the states
// that pulse.
type State struct {
	// State is coop's state: starting, working, idle, prompt, error, parked,
	// restarting, exited or unknown.
	State         string          `json:"state"`
	Prev          string          `json:"prev,omitempty"`
	RateLimited   bool            `json:"rate_limited,omitempty"`
	Attention     bool            `json:"attention,omitempty"`
	ErrorCategory string          `json:"error_category,omitempty"`
	ErrorDetail   string          `json:"error_detail,omitempty"`
	ParkedReason  string          `json:"parked_reason,omitempty"`
	ResumeAtMs    uint64          `json:"resume_at_epoch_ms,omitempty"`
	Prompt        json.RawMessage `json:"prompt,omitempty"`
}

func stateOf(s stateFrame) State {
	st := State{State: s.State, Prev: s.Prev, ErrorCategory: s.ErrorCategory, ErrorDetail: s.ErrorDetail,
		ParkedReason: s.ParkedReason, ResumeAtMs: s.ResumeAtMs}
	if s.Next != "" { // a transition
		st.State = s.Next
	}
	if len(s.Prompt) > 0 && string(s.Prompt) != "null" {
		st.Prompt = s.Prompt
	}
	st.RateLimited = s.ErrorCategory == "rate_limited" || s.ErrorCategory == "model_rate_limited"
	st.Attention = st.State == "prompt" || st.State == "error"
	return st
}

// frame folds one upstream frame into the feed and fans it out. It returns a
// request to send upstream, or nil.
func (f *feed) frame(fr coopmuxapi.Frame) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch fr.Event {
	case "replay":
		var m replayFrame
		data, ok := decodeBytes(fr.Data, &m, &m.Data)
		if !ok {
			return nil
		}
		f.asked = false
		f.onReplay(data, m.NextOffset)
	case "pty", "output":
		var m ptyFrame
		data, ok := decodeBytes(fr.Data, &m, &m.Data)
		if !ok || !f.primed {
			return nil // pre-replay bytes are covered by the replay
		}
		if m.Offset > f.next() {
			// Bytes went missing between frames: ask coop for them once and
			// drop what cannot be placed until the replay answers.
			if f.asked {
				return nil
			}
			f.asked = true
			return map[string]any{"event": "replay:get", "offset": f.next()}
		}
		f.onPty(data, m.Offset)
	case "screen":
		var m screenFrame
		if json.Unmarshal(fr.Data, &m) != nil {
			return nil
		}
		m.Modes = modesOf(m)
		f.modes = m.Modes
		f.screen, _ = json.Marshal(m)
		resized := f.cols != 0 && (m.Cols != f.cols || m.Rows != f.rows)
		f.cols, f.rows = m.Cols, m.Rows
		if resized && f.primed {
			// The PTY was resized (the person attached fitted it to their screen, GL-9): coop says so only in its
			// screens, so every watcher repaints at the new geometry, as a resync, before the bytes drawn at it.
			f.broadcast(Event{Kind: EventScreen, ID: f.next(), Data: f.screen})
		}
	case "agent", "transition":
		var m stateFrame
		if json.Unmarshal(fr.Data, &m) != nil {
			return nil
		}
		f.state, _ = json.Marshal(stateOf(m))
		f.broadcast(Event{Kind: EventState, ID: -1, Data: f.state})
	case "usage", "usage:update":
		f.usage = fr.Data // kept whole: the tag tells a snapshot from a cumulative update
		f.broadcast(Event{Kind: EventUsage, ID: -1, Data: f.usage})
	case "exit":
		f.broadcast(Event{Kind: EventExit, ID: -1, Data: stripEvent(fr.Data)})
	case "error":
		f.r.logger.Warn("glass: upstream error frame", "session", f.session, "frame", string(fr.Data))
	}
	return nil
}

// decodeBytes decodes a frame into v and the base64 field b64 points at.
func decodeBytes(raw json.RawMessage, v any, b64 *string) ([]byte, bool) {
	if json.Unmarshal(raw, v) != nil {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(*b64)
	return data, err == nil
}

// stripEvent returns the frame without its "event" tag, the SSE event name.
func stripEvent(raw json.RawMessage) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return []byte("{}")
	}
	delete(m, "event")
	out, _ := json.Marshal(m)
	return out
}

// onReplay applies coop's replay-gate rule to a replay ending at next.
func (f *feed) onReplay(data []byte, next int64) {
	start := next - int64(len(data))
	if f.primed && next <= f.next() {
		return // entirely behind the ring
	}
	if !f.primed || start > f.next() {
		// The first replay, or one past a gap: the ring restarts at it and
		// every watcher resets its terminal.
		f.primed = true
		f.ring, f.ringStart = nil, start
		f.appendRing(data)
		f.broadcast(f.replayEvent())
		return
	}
	skip := f.next() - start
	f.onPty(data[skip:], f.next())
}

// onPty appends bytes at offset to the ring, after the bytes the ring already
// has, and fans out what was new.
func (f *feed) onPty(data []byte, offset int64) {
	end := offset + int64(len(data))
	cur := f.next()
	if end <= cur {
		return
	}
	data = data[cur-offset:]
	f.appendRing(data)
	f.broadcast(ptyEvent(data, cur))
}

func (f *feed) appendRing(data []byte) {
	f.ring = append(f.ring, data...)
	if over := len(f.ring) - ringCap; over > 0 {
		f.ring = append([]byte(nil), f.ring[over:]...)
		f.ringStart += int64(over)
	}
}

func ptyEvent(data []byte, offset int64) Event {
	b, _ := json.Marshal(struct {
		Data   string `json:"data"`
		Offset int64  `json:"offset"`
	}{base64.StdEncoding.EncodeToString(data), offset})
	return Event{Kind: EventPty, ID: offset + int64(len(data)), Data: b}
}

// replayEvent is the whole ring as one replay event, with the modes the
// screen before it named.
func (f *feed) replayEvent() Event {
	b, _ := json.Marshal(struct {
		Data       string `json:"data"`
		Offset     int64  `json:"offset"`
		NextOffset int64  `json:"next_offset"`
		Modes      string `json:"modes,omitempty"`
	}{base64.StdEncoding.EncodeToString(f.ring), f.ringStart, f.next(), f.modes})
	return Event{Kind: EventReplay, ID: f.next(), Data: b}
}

// modesOf is the DEC private mode sets a terminal must have seen to be in the
// state coop's screen frame reports: the alternate screen (1049), the mouse
// tracking mode and SGR reports (1006). Only the modes coop names are set, in
// the order an app sets them; "" when none is.
func modesOf(m screenFrame) string {
	var s string
	if m.AltScreen {
		s += "\x1b[?1049h"
	}
	switch m.MouseTracking {
	case 9, 1000, 1002, 1003:
		s += "\x1b[?" + strconv.Itoa(m.MouseTracking) + "h"
	}
	if m.MouseSGR {
		s += "\x1b[?1006h"
	}
	return s
}

// resyncEvents is what a watcher that lost its place is sent: the state and
// usage, then the screen at the ring's end (the whole ring before any screen
// has arrived).
func (f *feed) resyncEvents() []Event {
	evs := f.statusEvents()
	if !f.primed {
		return evs
	}
	if f.screen != nil {
		return append(evs, Event{Kind: EventScreen, ID: f.next(), Data: f.screen})
	}
	return append(evs, f.replayEvent())
}

// statusEvents is the last state and usage, and who has the keyboard, the
// events with no bytes.
func (f *feed) statusEvents() []Event {
	var evs []Event
	if f.state != nil {
		evs = append(evs, Event{Kind: EventState, ID: -1, Data: f.state})
	}
	if f.usage != nil {
		evs = append(evs, Event{Kind: EventUsage, ID: -1, Data: f.usage})
	}
	if f.control != nil {
		evs = append(evs, Event{Kind: EventControl, ID: -1, Data: f.control})
	}
	return evs
}

// join queues a new watcher's opening events: a resumed one gets the bytes
// after its Last-Event-ID from the ring, a fresh one the whole ring as a
// replay, and one whose place the ring no longer holds a screen resync.
func (f *feed) join(w *Watcher, lastEventID int64) {
	if f.ended != nil {
		w.push(Event{Kind: EventEnded, ID: -1, Data: f.ended})
		w.closed = true
		return
	}
	evs := f.statusEvents()
	switch {
	case !f.primed:
		// the first replay reaches every watcher as it arrives
	case lastEventID >= f.ringStart && lastEventID <= f.next():
		if lastEventID < f.next() {
			evs = append(evs, ptyEvent(f.ring[lastEventID-f.ringStart:], lastEventID))
		}
	case lastEventID < 0:
		evs = append(evs, f.replayEvent()) // a fresh watcher writes the bytes, not the screen
	default:
		evs = f.resyncEvents()
	}
	for _, ev := range evs {
		w.push(ev)
	}
}

// broadcast offers ev to every watcher and resyncs one that fell behind. A
// replay that does not fit is resent alone after the state: it already is the
// resync, and the screen may predate it.
func (f *feed) broadcast(ev Event) {
	for w := range f.watchers {
		if w.offer(ev) {
			continue
		}
		if ev.Kind == EventReplay {
			w.reset(append(f.statusEvents(), ev)...)
		} else {
			w.reset(f.resyncEvents()...)
		}
	}
}

// end gives up on the session: every watcher gets an ended event and is
// closed, and the feed leaves the relay so the next watch dials afresh.
func (f *feed) end(reason string) {
	f.r.mu.Lock()
	if f.r.feeds[f.session] == f {
		delete(f.r.feeds, f.session)
	}
	f.r.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended, _ = json.Marshal(struct {
		Reason string `json:"reason"`
	}{reason})
	for w := range f.watchers {
		w.push(Event{Kind: EventEnded, ID: -1, Data: f.ended})
		w.closed = true
		w.wake()
	}
	f.cancel()
}

// leave removes w; the last watcher out starts the linger timer.
func (f *feed) leave(w *Watcher) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.watchers[w]; !ok {
		return
	}
	delete(f.watchers, w)
	if len(f.watchers) > 0 || f.ended != nil {
		return
	}
	f.idle = time.AfterFunc(f.r.linger(), f.expire)
}

// expire closes the upstream if nobody came back while it lingered.
func (f *feed) expire() {
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.watchers) > 0 || f.r.feeds[f.session] != f {
		return
	}
	delete(f.r.feeds, f.session)
	f.cancel()
}

// Watcher is one browser's view of a session: a bounded queue of events the
// feed fills and the SSE handler drains.
type Watcher struct {
	f      *feed
	notify chan struct{}

	// Guarded by f.mu.
	q       []Event
	qBytes  int
	closed  bool
	resyncs int
}

// Ready is signalled when events are waiting.
func (w *Watcher) Ready() <-chan struct{} { return w.notify }

// Take returns the waiting events and whether the watcher is finished (the
// session ended); nothing follows a finished watcher's last events.
func (w *Watcher) Take() ([]Event, bool) {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	evs := w.q
	w.q, w.qBytes = nil, 0
	return evs, w.closed
}

// Resyncs counts the times the watcher fell behind and was resynced.
func (w *Watcher) Resyncs() int {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	return w.resyncs
}

// Close removes the watcher from its session.
func (w *Watcher) Close() { w.f.leave(w) }

// offer queues ev unless the queue is full; false means the watcher fell
// behind. Called with f.mu held.
func (w *Watcher) offer(ev Event) bool {
	if w.closed {
		return true
	}
	if len(w.q) > 0 && (len(w.q) >= queueMaxEvents || w.qBytes+len(ev.Data) > queueMaxBytes) {
		return false
	}
	w.push(ev)
	return true
}

// reset replaces the queue with evs: what is queued is superseded. Called with
// f.mu held.
func (w *Watcher) reset(evs ...Event) {
	if w.closed {
		return
	}
	if len(w.q) > 0 {
		w.resyncs++
	}
	w.q, w.qBytes = nil, 0
	for _, ev := range evs {
		w.push(ev)
	}
}

func (w *Watcher) push(ev Event) {
	w.q = append(w.q, ev)
	w.qBytes += len(ev.Data)
	w.wake()
}

func (w *Watcher) wake() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}
