package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/natsauth"
)

// thinkingSubject is the NATS subject glob the telepathy feed subscribes to.
// coop's NATS forwarder publishes one message per agent turn that contains a
// thinking/reasoning block to `coop.events.messages.{actor}.thinking`
// (coop/crates/cli/src/transport/nats_forward.rs). The wildcard captures every
// agent's actor segment so the viewer gets a single unified cross-agent feed.
const thinkingSubject = "coop.events.messages.*.thinking"

// saySubject is where coop forwards each assistant text block (nats_forward.rs, `assistant_text`), the same wire shape
// as a thought. The hub only hands each one to OnSay (the Homestead's say row and bird, gd-HHGjxt59Mr): it is not ringed,
// not backfilled and not on the telepathy feed.
const saySubject = "coop.events.messages.*.assistant_text"

// thinkingStreamName is the JetStream stream that durably retains
// coop.events.messages.> (the beads daemon creates it; see kbeads
// internal/eventbus/streams.go StreamMessageEvents). The hub replays the tail
// of this stream at startup so the ring is populated across viewer restarts
// instead of starting empty.
const thinkingStreamName = "MESSAGE_EVENTS"

// thinkingRingCapacity is how many recent ThinkingEvents the hub retains so a
// freshly-connected SSE client can be backfilled with the latest activity
// instead of opening to a blank screen. Bounded so memory stays flat under a
// busy fleet — old events fall off the front.
const thinkingRingCapacity = 200

// thinkingClientBuffer is the per-subscriber channel depth. A slow client
// (e.g. a wedged browser tab) backs up only this far before the hub starts
// dropping its oldest queued events rather than blocking the NATS callback or
// every other client. Telepathy is a live feed — dropping stale events for one
// laggard is the right trade.
const thinkingClientBuffer = 64

// ThinkingEvent is one agent thinking/reasoning block lifted off NATS and
// normalized for the telepathy feed. Field names mirror coop's forwarder
// payload (nats_forward.rs publish_event + detect_metadata) so unmarshalling
// is a straight map.
type ThinkingEvent struct {
	// Agent is the gardenio agent name (actor) that emitted the thinking block,
	// e.g. "gardenio-foo-bar-123". Sourced from the payload `actor`/`agent`
	// field, falling back to the subject's actor segment.
	Agent string `json:"agent"`
	// Text is the thinking/reasoning content. coop currently truncates this to
	// ~512 bytes on the wire (MAX_TEXT_BYTES); FullLength carries the
	// pre-truncation length so the UI can flag elision.
	Text string `json:"text"`
	// FullLength is the byte length of the thinking block before any
	// truncation coop applied. Zero when absent.
	FullLength int `json:"full_length"`
	// SessionID is coop's session identifier for the emitting agent. Useful
	// for grouping a single agent's turns; display-optional.
	SessionID string `json:"session_id"`
	// Project is the BOAT_PROJECT the agent runs under (e.g. "gardenio").
	Project string `json:"project"`
	// Role is the comma-separated controller-assigned role(s) (e.g. "crew").
	Role string `json:"role"`
	// AgentBeadID is the agent's own config bead (BOAT_AGENT_BEAD_ID), letting
	// the UI deep-link a thinking line back to the agent page.
	AgentBeadID string `json:"agent_bead_id"`
	// PodName is the k8s pod the agent runs in, when available.
	PodName string `json:"pod_name"`
	// Source is the capture provenance. The LiteLLM-callback capture
	// (spike ka-0000004553, envelope gardenio/docs/thinking-capture-spike.md)
	// stamps source="litellm-callback" — that path carries the actual reasoning
	// plaintext. coop's Tier-2 log path publishes .thinking with empty text
	// (Anthropic strips persisted reasoning to a signature) and no source; the
	// hub already drops those empties, so a populated event with empty Source is
	// simply an older/other producer. Display-optional.
	Source string `json:"source"`
	// TS is the producer-side timestamp (unix millis) coop stamped on publish.
	TS int64 `json:"ts"`
	// ReceivedAt is the viewer-side receive time, set by the hub. Used as the
	// display timestamp so the feed is monotonic even if producer clocks skew.
	ReceivedAt time.Time `json:"-"`
}

// thinkingWire is the raw on-wire shape. The forwarder flattens k8s metadata
// into a nested object, so we decode it separately and lift pod_name onto the
// normalized event.
type thinkingWire struct {
	EventType   string `json:"event_type"`
	Agent       string `json:"agent"`
	Actor       string `json:"actor"`
	Text        string `json:"text"`
	FullLength  int    `json:"full_length"`
	SessionID   string `json:"session_id"`
	Project     string `json:"project"`
	Role        string `json:"role"`
	AgentBeadID string `json:"agent_bead_id"`
	Source      string `json:"source"`
	TS          int64  `json:"ts"`
	K8s         struct {
		Pod string `json:"pod"`
	} `json:"k8s"`
}

// ThinkingHub subscribes to the NATS thinking subject and fans each event out
// to all in-process subscribers (the telepathy SSE handler). It also keeps a
// bounded ring of recent events so new subscribers can be backfilled.
//
// A nil *ThinkingHub is safe to use: Subscribe returns a closed channel and
// Recent returns nil, so the SSE page degrades to an empty live feed when NATS
// is not configured. This mirrors the nil-safe daemon-client pattern elsewhere
// in beads-viewer.
type ThinkingHub struct {
	natsURL   string
	natsToken string
	natsUser  string
	logger    *slog.Logger

	conn   *nats.Conn
	sub    *nats.Subscription
	saySub *nats.Subscription

	mu      sync.Mutex
	clients map[int]chan ThinkingEvent
	nextID  int
	ring    []ThinkingEvent // most-recent-last, bounded to thinkingRingCapacity
	// onThought is told the agent bead of each live thought (the Homestead activity feed's think,
	// gardenhub.Farm.ObserveThinking). Set after Start, so the JetStream backfill never reaches it.
	onThought func(agentBeadID string)
	// onSay is told the agent bead and text of each assistant text block (gardenhub.Farm.ObserveSay).
	onSay func(agentBeadID, text string)
}

// OnSay registers fn to be told the agent bead id and text of every assistant text block that names one. Safe on a nil hub.
func (h *ThinkingHub) OnSay(fn func(agentBeadID, text string)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.onSay = fn
	h.mu.Unlock()
}

// OnThought registers fn to be told the agent bead id of every live thought that names one. Safe on a nil hub.
func (h *ThinkingHub) OnThought(fn func(agentBeadID string)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.onThought = fn
	h.mu.Unlock()
}

// NewThinkingHub builds a hub from the NATS connection parameters. Returns nil
// when natsURL is empty (NATS not configured) — callers must treat a nil hub
// as "telepathy feed disabled", which every method on *ThinkingHub tolerates.
func NewThinkingHub(natsURL, natsToken, natsUser string, logger *slog.Logger) *ThinkingHub {
	if natsURL == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ThinkingHub{
		natsURL:   natsURL,
		natsToken: natsToken,
		natsUser:  natsUser,
		logger:    logger,
		clients:   make(map[int]chan ThinkingEvent),
	}
}

// Start connects to NATS and subscribes to the thinking subject. The
// subscription runs asynchronously via the NATS client's own goroutine — this
// returns once the subscription is established (or errors). Safe to call on a
// nil hub (no-op).
func (h *ThinkingHub) Start() error {
	if h == nil {
		return nil
	}

	opts := []nats.Option{
		nats.Name("beads-viewer-telepathy"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * nats.DefaultReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				h.logger.Warn("telepathy: NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			h.logger.Info("telepathy: NATS reconnected")
		}),
	}
	opts = append(opts, natsauth.Options(h.natsUser, h.natsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)

	conn, err := nats.Connect(h.natsURL, opts...)
	if err != nil {
		return err
	}
	h.conn = conn

	// Backfill the ring from JetStream BEFORE going live so a freshly-(re)started
	// viewer always has the last N retained thoughts to paint — the in-memory
	// ring alone is wiped on every pod restart, which is why the feed looked
	// near-empty in prod (ka-0000005574). The MESSAGE_EVENTS stream durably
	// retains coop.events.messages.> (thinking included), so we replay the tail
	// of it once at startup. Best-effort: JetStream may be unconfigured (the
	// stream is created by the beads daemon, not the viewer), in which case we
	// log and fall back to a live-only feed. A viewer that boots before NATS
	// is up skips it: the replay would reach OnThought, which is registered
	// by the time the retrying connect succeeds, and paint old thoughts as live.
	if conn.IsConnected() {
		h.backfillFromJetStream(conn)
	}

	sub, err := conn.Subscribe(thinkingSubject, h.handleMessage)
	if err != nil {
		conn.Close()
		h.conn = nil
		return err
	}
	h.sub = sub
	saySub, err := conn.Subscribe(saySubject, h.handleSay)
	if err != nil {
		_ = sub.Unsubscribe()
		conn.Close()
		h.conn, h.sub = nil, nil
		return err
	}
	h.saySub = saySub

	// Flush so the SUB interest is registered on the server before Start
	// returns. conn.Subscribe only buffers the protocol command and kicks the
	// async flusher — without this round-trip, an event published by a separate
	// connection immediately after Start returns can reach the server before our
	// interest does and be silently dropped (core NATS is fire-and-forget). This
	// is what made TestThinkingHub_BackfillNoStreamIsLiveOnly flake under CI load
	// (ka-0000002773); the JetStream-backfill path never saw it because its
	// stream round-trips implicitly flush first. A failed flush leaves the sub
	// unusable, so tear down and surface it rather than return a half-live hub.
	if !conn.IsConnected() {
		// NATS is not up yet: both subscriptions are buffered and sent once
		// the retrying connect succeeds (without the backfill, above).
		h.logger.Warn("telepathy: NATS not reachable yet — subscribing once it is", "url", h.natsURL)
		return nil
	}
	if err := conn.FlushTimeout(5 * time.Second); err != nil {
		_ = sub.Unsubscribe()
		_ = saySub.Unsubscribe()
		conn.Close()
		h.conn = nil
		h.sub = nil
		h.saySub = nil
		return err
	}

	h.logger.Info("telepathy: subscribed to thinking and assistant text events",
		"url", h.natsURL, "subject", thinkingSubject, "say_subject", saySubject)
	return nil
}

// backfillFromJetStream replays the tail of the MESSAGE_EVENTS stream (filtered
// to the thinking subject) into the ring so the feed paints immediately on a
// fresh viewer process, surviving restarts. It is best-effort: any error
// (JetStream not enabled, stream absent, consumer setup failure) is logged and
// swallowed — the live core-NATS subscription still works without it.
//
// We use an ephemeral ordered push consumer with DeliverLastPerSubject so the
// backfill is bounded to one most-recent thought per agent rather than every
// retained turn — that's exactly the "always populate w something" guarantee
// without flooding the ring with a single chatty agent's history. Events are
// fed through handleMessage so dedup/empty-drop/normalization stay identical to
// the live path; broadcast()'s ring trim keeps the ring bounded.
func (h *ThinkingHub) backfillFromJetStream(conn *nats.Conn) {
	js, err := conn.JetStream()
	if err != nil {
		h.logger.Info("telepathy: JetStream unavailable — live-only feed", "error", err)
		return
	}

	// Confirm the stream exists before subscribing so a non-JetStream cluster
	// (or a daemon that hasn't created the stream yet) doesn't spam errors.
	if _, err := js.StreamInfo(thinkingStreamName); err != nil {
		h.logger.Info("telepathy: message stream absent — live-only feed",
			"stream", thinkingStreamName, "error", err)
		return
	}

	// Collect the backfill messages synchronously, then replay them in
	// stream order. An ephemeral push consumer with DeliverLastPerSubject hands
	// us at most one (the newest) thought per agent subject.
	collected := make(chan *nats.Msg, thinkingRingCapacity)
	sub, err := js.Subscribe(thinkingSubject, func(msg *nats.Msg) {
		select {
		case collected <- msg:
		default:
		}
	}, nats.DeliverLastPerSubject(), nats.OrderedConsumer())
	if err != nil {
		h.logger.Info("telepathy: backfill subscribe failed — live-only feed", "error", err)
		return
	}
	defer func() { _ = sub.Unsubscribe() }()

	// Drain until quiescent: JetStream delivers the retained set in a burst, so
	// once we go ~250ms without a new message we assume the tail is exhausted.
	// Bounded by an overall deadline so a misbehaving stream can't stall Start.
	deadline := time.After(3 * time.Second)
	idle := time.NewTimer(250 * time.Millisecond)
	defer idle.Stop()
	var msgs []*nats.Msg
drain:
	for {
		select {
		case msg := <-collected:
			msgs = append(msgs, msg)
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(250 * time.Millisecond)
		case <-idle.C:
			break drain
		case <-deadline:
			break drain
		}
	}

	for _, msg := range msgs {
		h.handleMessage(msg)
	}
	if len(msgs) > 0 {
		h.logger.Info("telepathy: backfilled ring from JetStream",
			"stream", thinkingStreamName, "events", len(msgs))
	}
}

// Close unsubscribes, closes the NATS connection, and closes every subscriber
// channel so SSE handlers blocked on a receive unwind. Safe on a nil hub.
func (h *ThinkingHub) Close() {
	if h == nil {
		return
	}
	if h.sub != nil {
		_ = h.sub.Unsubscribe()
	}
	if h.saySub != nil {
		_ = h.saySub.Unsubscribe()
	}
	if h.conn != nil {
		h.conn.Close()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.clients {
		close(ch)
		delete(h.clients, id)
	}
}

// handleMessage is the NATS callback: parse, normalize, store in the ring, and
// broadcast. Malformed or empty payloads are dropped silently — the live feed
// is best-effort.
func (h *ThinkingHub) handleMessage(msg *nats.Msg) {
	var w thinkingWire
	if err := json.Unmarshal(msg.Data, &w); err != nil {
		return
	}
	// coop forwards thinking events even when the persisted log yields no
	// plaintext (signature-only blocks); skip empties so the feed isn't padded
	// with blank lines. See epic ka-0000007154 / spike ka-0000004553.
	if w.Text == "" {
		return
	}

	agent := w.Actor
	if agent == "" {
		agent = w.Agent
	}
	if agent == "" {
		// Fall back to the subject's actor segment:
		// coop.events.messages.<actor>.thinking
		agent = actorFromSubject(msg.Subject)
	}

	evt := ThinkingEvent{
		Agent:       agent,
		Text:        w.Text,
		FullLength:  w.FullLength,
		SessionID:   w.SessionID,
		Project:     w.Project,
		Role:        w.Role,
		AgentBeadID: w.AgentBeadID,
		PodName:     w.K8s.Pod,
		Source:      w.Source,
		TS:          w.TS,
		ReceivedAt:  time.Now().UTC(),
	}
	h.broadcast(evt)
	h.mu.Lock()
	fn := h.onThought
	h.mu.Unlock()
	if fn != nil && evt.AgentBeadID != "" {
		fn(evt.AgentBeadID)
	}
}

// handleSay is the NATS callback of the say subject: the agent bead and text of an assistant text block go to OnSay.
// One with no text or no agent bead is dropped: the farm keys gardeners by bead id, and an empty text is nothing said.
func (h *ThinkingHub) handleSay(msg *nats.Msg) {
	var w thinkingWire
	if err := json.Unmarshal(msg.Data, &w); err != nil || w.Text == "" || w.AgentBeadID == "" {
		return
	}
	h.mu.Lock()
	fn := h.onSay
	h.mu.Unlock()
	if fn != nil {
		fn(w.AgentBeadID, w.Text)
	}
}

// broadcast appends to the ring and pushes to every subscriber. A subscriber
// whose buffer is full has its oldest queued event dropped to make room, so a
// slow client never blocks the NATS callback or starves its peers.
func (h *ThinkingHub) broadcast(evt ThinkingEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.ring = append(h.ring, evt)
	if len(h.ring) > thinkingRingCapacity {
		h.ring = h.ring[len(h.ring)-thinkingRingCapacity:]
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

// SubscribeFor is Subscribe for a caller with a known scope. The feed is every
// agent's raw reasoning, keyed by agent name rather than by bead, so there is no
// per-project cut to make: a scoped principal gets an already-closed channel
// (the feed is off for them) and only an unrestricted caller is subscribed.
func (h *ThinkingHub) SubscribeFor(ctx context.Context, sc viewerScope) (<-chan ThinkingEvent, func()) {
	if h == nil || sc.Scoped() {
		ch := make(chan ThinkingEvent)
		close(ch)
		return ch, func() {}
	}
	return h.Subscribe(ctx)
}

// Subscribe registers a new in-process consumer of the thinking feed. It
// returns a receive-only channel of events and an unsubscribe func that must
// be called (typically via defer) when the consumer is done — the channel is
// closed on unsubscribe and when the hub closes.
//
// The returned channel is first backfilled with the hub's recent ring (oldest
// first) so a fresh SSE client paints immediately, then receives live events.
// On a nil hub it returns an already-closed channel and a no-op cancel.
func (h *ThinkingHub) Subscribe(ctx context.Context) (<-chan ThinkingEvent, func()) {
	if h == nil {
		ch := make(chan ThinkingEvent)
		close(ch)
		return ch, func() {}
	}

	h.mu.Lock()
	id := h.nextID
	h.nextID++
	ch := make(chan ThinkingEvent, thinkingClientBuffer)
	// Backfill the recent ring before going live. The buffer is sized to hold
	// a useful backlog; if the ring exceeds it, keep the most recent.
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

// Recent returns a snapshot copy of the buffered thinking events, oldest first.
// Used for tests and any non-streaming surface. Nil-safe.
func (h *ThinkingHub) Recent() []ThinkingEvent {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ring) == 0 {
		return nil
	}
	out := make([]ThinkingEvent, len(h.ring))
	copy(out, h.ring)
	return out
}

// actorFromSubject extracts the actor segment from a thinking subject of the
// form `coop.events.messages.<actor>.thinking`. Returns "" if the shape
// doesn't match.
func actorFromSubject(subject string) string {
	// Tokens: coop . events . messages . <actor> . thinking
	const prefix = "coop.events.messages."
	const suffix = ".thinking"
	if len(subject) <= len(prefix)+len(suffix) {
		return ""
	}
	if subject[:len(prefix)] != prefix {
		return ""
	}
	if subject[len(subject)-len(suffix):] != suffix {
		return ""
	}
	return subject[len(prefix) : len(subject)-len(suffix)]
}
