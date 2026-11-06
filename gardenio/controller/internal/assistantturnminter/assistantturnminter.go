// Package assistantturnminter is the always-on, watcher-independent WRITE side
// of the /plans chat transcript: a background NATS consumer that observes every
// plan-assistant turn (coop.events.messages.*.{assistant_text,tool_use}) and
// mints it as a durable type=message turn bead. The operator half of the
// conversation is minted synchronously by the viewer's prompt POST; THIS is the
// assistant half — both its prose replies (turn_role=assistant) AND its tool
// calls (turn_role=tool), so the /plans transcript shows a plan agent's tool
// calls inline (data slice ka-0000004690; render side + write contract already
// shipped under ka-0000004601).
//
// REHOMED OFF THE VIEWER (task ka-0000006891, P3 of plan ka-0000000187): this
// minter used to live in package main under cmd/beads-viewer, coupling plan
// transcript persistence to the viewer process lifetime — a reply emitted while
// no beads-viewer replica was running (or while it was restarting) was silently
// dropped. Lifting it into this importable package lets the CONTROLLER own the
// subscription, so transcripts mint for the whole cluster lifetime regardless
// of whether an operator has a cockpit open. The write contract it mints
// through (turn-bead shape, dedup key, plan-assistant identity) is the shared
// internal/plantranscript package, so the controller's assistant-turn mint and
// the viewer's operator-turn mint stay byte-identical.
//
// WHY THIS SEAM (chosen over a slack-bridge Stop-hook mint and an agent-pod
// Stop-hook mint via a judge panel + adversarial verification):
//   - ALWAYS-ON / watcher-independent: it is a background subscription that runs
//     for the whole host process lifetime, NOT a per-cockpit SSE stream. So a
//     reply is persisted even when nobody is watching — the completeness
//     guarantee squawk-only could not give (a crashed/lazy assistant silently
//     dropped turns).
//   - FULL TEXT with NO agent-image change: coop's assistant_text forward carries
//     the reply up to the NATS text budget (~256KB at the 1MiB default), so for
//     plan-chat replies the event ALONE has the complete text. The 512-byte cap
//     that afflicts the gb Stop-hook relay does NOT apply here. Only a >256KB
//     reply needs the coopmux GetAgentState hydration fallback below.
//   - OUT-OF-POD / server-side: the host is not the agent and not
//     model-volitional, so it satisfies the "the server mints it" contract.
//
// This mirrors the viewer's ThinkingHub closely — same NATS connect options,
// same MESSAGE_EVENTS JetStream backfill, same wire-envelope decode — but
// instead of fanning events to an SSE feed it resolves the emitting agent to
// its plan and mints a turn bead, AND it replays the FULL retained tail (not
// just the newest per subject) so a multi-turn host outage is recovered.
//
// DURABLE FULL-TAIL REPLAY (closes the multi-turn-outage gap): the JetStream
// backfill uses DeliverAll (the whole retained assistant_text tail), NOT
// DeliverLastPerSubject (which recovered only the newest turn per agent-subject
// and permanently dropped the intermediate turns of a multi-turn outage). The
// mint-time process-independent IdempotencyKey (kbeads migration 029,
// lookup-or-return) makes re-minting an already-minted turn a no-op, so
// replaying the entire tail — on every boot, and on every replica — is safe. To
// keep this off the boot critical path, the replay runs in a background
// goroutine that Start() launches AFTER the live subscription is established, so
// host readiness never waits on the (unbounded, fleet-wide) tail replay; the
// goroutine is tracked by a WaitGroup and bounded by a lifetime context Close()
// cancels. End-of-tail is detected DETERMINISTICALLY from the stream's LastSeq /
// the delivered messages' NumPending (not a wall-clock idle heuristic, which the
// ordered consumer's 5s heartbeats and slow-daemon gaps would truncate early).
//
// HONEST COMPLETENESS FLOOR: MESSAGE_EVENTS is ONE fleet-wide JetStream stream
// (kbeads eventbus/streams.go) carrying assistant_text + thinking + usage for
// EVERY agent. It uses per-subject retention (MaxMsgsPerSubject=500, MaxMsgs=-1,
// MaxBytes=512MB backstop, MaxAge=72h, DiscardOld), so each agent-subject
// retains its own recent assistant_text independent of fleet volume. This replay
// recovers everything still retained (strictly more than DeliverLastPerSubject);
// the per-subject floor is what makes "still retained" reliably span a realistic
// outage under load. See the collector-sizing note in backfillFromJetStream for
// the coupling: dropping the 10k count cap means the collector must size from
// live stream depth, not a fixed 10k, or it would silently drop the oldest turns
// it just protected.
package assistantturnminter

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/natsauth"
	"gardenio/controller/internal/plantranscript"
)

// turnEventSubject is the NATS subject glob coop's forwarder publishes an
// assistant turn event to — one per text-producing OR tool-calling agent turn,
// at coop.events.messages.<actor>.<event> (nats_forward.rs). The minter
// subscribes to the FULL per-actor wildcard (not just .assistant_text) so it
// sees both assistant_text and tool_use events on one subscription, then
// dispatches by the envelope's event_type in-process. The trailing "*" captures
// EVERY event suffix (assistant_text, thinking, usage, tool_use, refusal…);
// handleMessageCtx routes only the two turn kinds it mints and drops the rest.
// The actor wildcard captures every agent; the plan-assistant filter runs
// in-process so the non-plan fleet costs no daemon lookups.
const turnEventSubject = "coop.events.messages.*.*"

// Event-type discriminators the minter mints turns for. coop stamps event_type
// on every envelope (nats_forward.rs publish_event); the minter routes an
// assistant_text event to an assistant (prose) turn and a tool_use event to a
// tool turn, and drops every other event kind (thinking/usage/refusal) so the
// shared subscription stays cheap for the non-transcript signals.
const (
	eventTypeAssistantText = "assistant_text"
	eventTypeToolUse       = "tool_use"
)

// assistantTurnStreamName is the JetStream stream the beads daemon creates to
// durably retain coop.events.messages.> (kbeads internal/eventbus/streams.go
// StreamMessageEvents). Same stream ThinkingHub backfills from.
const assistantTurnStreamName = "MESSAGE_EVENTS"

// assistantTurnSystemOperator is the service-identity source label the minter
// stamps (via WithSystemOperator) when the plan epic carries no operated_by, so
// a background turn mint still authenticates without a human OIDC identity. Must
// satisfy the daemon's sanitizeSystemOperatorHeader rules (lowercase ASCII /
// digits / "-", 1..64, no leading/trailing/double "-").
const assistantTurnSystemOperator = "assistant-turn-minter"

// planAssistantResolveTTL bounds how long a resolved agent→plan binding (or a
// negative "not a plan assistant" result) is cached before a fresh daemon
// lookup. Short enough that a mid-life role/plan_id change is picked up quickly,
// long enough that a chatty assistant emitting many turns — and a cold-cache
// host restart replaying a JetStream burst — do not hammer the daemon.
const planAssistantResolveTTL = 5 * time.Minute

// BeadAPI is the daemon surface the minter needs — satisfied by
// *beadsapi.Client. Kept as a narrow interface so tests can inject a fake
// without a live daemon.
type BeadAPI interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// ScreenClient is the coopmux surface the minter needs for the >256KB
// full-text hydration fallback — satisfied by *coopmuxapi.Client. Nil / a
// disabled client cleanly degrades to "persist the clipped copy, marked
// truncated".
type ScreenClient interface {
	Disabled() bool
	GetAgentState(ctx context.Context, sessionID string) (*coopmuxapi.AgentState, error)
}

// turnEventWire is the raw on-wire shape of a coop turn event — a superset that
// covers BOTH the assistant_text and tool_use envelopes. Field names mirror
// coop's forwarder payload (nats_forward.rs publish_event + detect_metadata) so
// unmarshalling is a straight map; each event kind populates only its own
// fields (Text/FullLength for assistant_text, ToolName/ToolArgsSummary for
// tool_use), and the shared identity/routing fields are common to both. Extra
// keys are ignored.
type turnEventWire struct {
	EventType   string `json:"event_type"`
	Agent       string `json:"agent"`
	Actor       string `json:"actor"`
	Text        string `json:"text"`
	FullLength  int    `json:"full_length"`
	SessionID   string `json:"session_id"`
	Project     string `json:"project"`
	Role        string `json:"role"`
	AgentBeadID string `json:"agent_bead_id"`
	TS          int64  `json:"ts"`
	// Tool-use fields (event_type == tool_use). ToolName is the tool invoked
	// ("Bash", "Read", …); ToolArgsSummary is coop's extract_tool_label compact
	// summary (omitted on the wire when the tool exposes no summarizable input).
	ToolName        string `json:"tool_name"`
	ToolArgsSummary string `json:"tool_args_summary"`
}

// planBinding is a cached resolution of one agent bead to its plan.
type planBinding struct {
	isPlanAssistant bool
	planID          string
	// operator is the plan epic's operated_by (e.g. "oidc:user@x"), stamped on
	// the mint ctx so the assistant turn attributes to the human the plan
	// belongs to — the SAME operator the operator-turn mint carries — and so the
	// daemon's audit resolver does not fail-closed on a background write with no
	// operator. Empty when the epic carries none; the mint then falls back to a
	// system-operator sentinel.
	operator string
	// project is the land the turn beads are labelled into (project:<land>):
	// the plan epic's project label, else the agent bead's. Empty when neither
	// carries one; the mint then falls back to the wire envelope's project.
	project string
	at      time.Time
}

// Minter subscribes to the assistant_text NATS subject and mints a durable
// plan-transcript turn bead for each reply emitted by a plan assistant.
//
// A nil *Minter is safe: Start/Close are no-ops, so the host degrades to "no
// assistant-turn persistence" when NATS is not configured — mirroring the
// nil-safe ThinkingHub / daemon-client pattern.
type Minter struct {
	natsURL   string
	natsToken string
	natsUser  string
	daemon    BeadAPI
	coopmux   ScreenClient
	logger    *slog.Logger
	// now is injected so tests can drive the resolver cache TTL deterministically.
	now func() time.Time

	conn *nats.Conn
	sub  *nats.Subscription

	// backfillCtx bounds the background full-tail replay goroutine's lifetime;
	// Close() calls backfillCancel to unwind an in-flight replay (its drain loop,
	// its post-drain mint loop, and any in-flight resolvePlan/CreateBead derived
	// from this ctx) promptly on shutdown. wg tracks the goroutine so Close()
	// joins it (bounded) before returning.
	backfillCtx    context.Context
	backfillCancel context.CancelFunc
	wg             sync.WaitGroup

	mu    sync.Mutex
	cache map[string]planBinding // agent bead id -> binding (positive AND negative)
	// lastSweep gates the opportunistic expired-entry sweep in storeBinding so
	// the map size tracks the live working set, not the cumulative all-time set.
	lastSweep time.Time
}

// New builds a minter from the NATS connection parameters and the daemon +
// coopmux clients it mints/hydrates through. Returns nil when natsURL is empty
// (NATS not configured) — callers treat a nil minter as "assistant-turn
// persistence disabled", which every method tolerates.
//
// coopmux may be nil (a typed-nil ScreenClient or a genuinely nil interface):
// hydrateFullText tolerates both, so a host with no coopmux client simply keeps
// the clipped wire copy for a >256KB reply and marks it truncated.
func New(natsURL, natsToken, natsUser string, daemon BeadAPI, coopmux ScreenClient, logger *slog.Logger) *Minter {
	if natsURL == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Minter{
		natsURL:        natsURL,
		natsToken:      natsToken,
		natsUser:       natsUser,
		daemon:         daemon,
		coopmux:        coopmux,
		logger:         logger,
		now:            func() time.Time { return time.Now() },
		backfillCtx:    ctx,
		backfillCancel: cancel,
		cache:          make(map[string]planBinding),
	}
}

// Start connects to NATS, subscribes LIVE to the assistant_text subject FIRST,
// then launches the full-tail JetStream replay in a BACKGROUND goroutine and
// returns. Ordering matters: the live subscription is cheap (one round-trip) and
// establishing it before the replay means no NEW turn is missed while the tail
// replays; the DeliverAll replay (unbounded, fleet-wide, one CreateBead
// round-trip per plan turn) runs off the boot critical path so host readiness
// never waits on it. Both paths converge safely on one bead per turn via the
// process-independent IdempotencyKey. Safe on a nil minter (no-op).
func (m *Minter) Start() error {
	if m == nil {
		return nil
	}

	// connected is closed on the first connect, immediate or retried
	// (natsauth.StartupRetry): the replay below needs a live server.
	connected := make(chan struct{})
	var connectedOnce sync.Once
	opts := []nats.Option{
		nats.Name("gardenio-assistant-turn-minter"),
		nats.ConnectHandler(func(_ *nats.Conn) { connectedOnce.Do(func() { close(connected) }) }),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * nats.DefaultReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				m.logger.Warn("assistant-turn-minter: NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			m.logger.Info("assistant-turn-minter: NATS reconnected")
		}),
	}
	opts = append(opts, natsauth.Options(m.natsUser, m.natsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)

	conn, err := nats.Connect(m.natsURL, opts...)
	if err != nil {
		return err
	}
	m.conn = conn

	// Go LIVE first so a turn emitted during the (potentially long) tail replay
	// is never missed. Live delivery and the concurrent replay both mint against
	// the same IdempotencyKey, so a turn seen by both converges on one bead.
	sub, err := conn.Subscribe(turnEventSubject, m.handleMessage)
	if err != nil {
		conn.Close()
		m.conn = nil
		return err
	}
	m.sub = sub

	// Full-tail replay in the background: a freshly-(re)started host mints any
	// retained turn it missed while down — INCLUDING the intermediate turns of a
	// multi-turn outage (the gap DeliverLastPerSubject left open). Off the boot
	// path (WaitGroup-tracked, backfillCtx-bounded) so it never delays readiness.
	// Best-effort: JetStream may be unconfigured (the stream is created by the
	// daemon), in which case it logs and the live subscription carries on alone.
	m.wg.Go(func() {
		select {
		case <-connected:
		case <-m.backfillCtx.Done():
			return
		}
		m.backfillFromJetStream(m.backfillCtx, conn)
	})

	m.logger.Info("assistant-turn-minter: subscribed to turn events",
		"url", m.natsURL, "subject", turnEventSubject)
	return nil
}

// backfillPendingMsgsLimit / backfillPendingBytesLimit raise the NATS async
// subscription's internal pending buffer at/above the MESSAGE_EVENTS stream cap
// (per-subject retention: MaxMsgs=-1, MaxBytes 512MB, MaxMsgsPerSubject 500,
// streams.go). The nats.go default pending limits are 500k msgs / 64MB;
// 64MB is BELOW the stream's byte cap, so a large retained tail delivered in a
// burst could overflow the buffer and processMsg would silently drop the OLDEST
// messages with ErrSlowConsumer — an ephemeral AckNone ordered consumer never
// redelivers them, so those dropped messages are exactly the intermediate outage
// turns this replay exists to recover. These are CEILINGS, not allocations:
// nats.go grows the pending buffer only as needed up to the limit, and the
// fast/non-blocking collector callback (no daemon call) keeps actual usage far
// below them — it appends and returns, so the buffer drains as fast as NATS
// fills it. Sizing the byte ceiling to the stream's own byte cap guarantees the
// whole retained tail fits before any drop even in the worst case where the
// subscription delivers the entire stream (it now subscribes to the full
// coop.events.messages.*.* glob — assistant_text + tool_use + thinking + usage +
// refusal — and routes by event_type in-process, so the byte cap must cover the
// whole retained stream, which this ceiling does).
const (
	backfillPendingMsgsLimit  = 200_000
	backfillPendingBytesLimit = 512 << 20 // 512MB — matches the MESSAGE_EVENTS MaxBytes cap
)

// backfillCollectorMinCap is the floor for the live-depth-sized collector
// channel (see backfillFromJetStream). A tiny/quiet stream sizes the buffer from
// its handful of retained messages; the floor keeps a small stream behaving as
// the old fixed buffer did and absorbs the assistant_text tail without a drop.
const backfillCollectorMinCap = 10_000

// backfillMaxDuration is a generous wall-clock ceiling on the whole background
// replay, a backstop against a churning/never-quiescent stream (e.g. a mid-drain
// ordered-consumer reset that keeps re-delivering under a persistently slow
// daemon). It is NOT the primary end-of-tail signal (that is the deterministic
// LastSeq / NumPending check below) — only a deterministic terminator so the
// goroutine always exits and is observable ("replay incomplete") rather than
// running until SIGKILL. Off the boot path, so a large legitimate tail has room.
const backfillMaxDuration = 2 * time.Minute

// backfillIdleTimeout bounds how long the collector waits for the NEXT delivered
// message before concluding the tail is drained, used ONLY as a fallback when the
// deterministic LastSeq/NumPending signal is unavailable (an older server whose
// JetStream metadata this consumer cannot read). It is generous because the
// ordered consumer emits 5s idle heartbeats between deliveries — a short idle
// window would falsely declare the tail exhausted mid-replay. The subscription
// now delivers EVERY stream message (the full glob), so on any modern server the
// delivered LastSeq is always reached and this timer never fires; it stays only
// as the metadata-less backstop. The collector callback does no daemon work, so
// waiting here costs nothing but wall-clock and never blocks the host.
const backfillIdleTimeout = 8 * time.Second

// backfillFromJetStream replays the FULL retained tail of MESSAGE_EVENTS
// (the whole coop.events.messages.*.* glob; handleMessageCtx routes each event
// by event_type and mints only the assistant_text + tool_use turns) through the
// mint path so a fresh host process mints every turn it missed while restarting
// — including the intermediate turns of a MULTI-TURN outage, the gap
// DeliverLastPerSubject left
// permanently open. Best-effort: any error (JetStream not enabled, stream
// absent, consumer setup failure) is logged and swallowed; the live subscription
// still works without it.
//
// Mechanics that make full-tail replay correct (each closes an adversarially-
// identified hole):
//   - DeliverAll (not DeliverLastPerSubject) replays every retained turn, not
//     just the newest per subject — THE core fix.
//   - COLLECT-FAST / PROCESS-AFTER: the ordered-consumer callback only appends to
//     a slice (no daemon call), so the async subscription's pending buffer never
//     backs up and drops; the CreateBead mints run in a SEPARATE loop after the
//     tail is drained. Inline minting in the callback would let a slow daemon
//     overflow the 64MB pending buffer and silently drop the oldest turns.
//   - DETERMINISTIC END-OF-TAIL: stop when a delivered message reports
//     NumPending==0 or its stream sequence reaches the up-front StreamInfo
//     LastSeq — NOT a wall-clock idle heuristic (the 5s ordered-consumer
//     heartbeats and slow-daemon gaps truncate an idle-timer replay early).
//   - ctx-BOUNDED: the drain and the post-drain mint loop both abort on
//     backfillCtx cancellation (Close()), so shutdown is prompt.
func (m *Minter) backfillFromJetStream(ctx context.Context, conn *nats.Conn) {
	js, err := conn.JetStream()
	if err != nil {
		m.logger.Info("assistant-turn-minter: JetStream unavailable — live-only", "error", err)
		return
	}
	info, err := js.StreamInfo(assistantTurnStreamName)
	if err != nil {
		m.logger.Info("assistant-turn-minter: message stream absent — live-only",
			"stream", assistantTurnStreamName, "error", err)
		return
	}
	// LastSeq is the definitive end-of-tail marker: once a delivered message's
	// stream sequence reaches it (or NumPending hits 0), the retained tail is
	// exhausted. Captured before subscribing so a live write during replay does
	// not move the target we drain toward. An empty stream (LastSeq 0) has no
	// tail to replay.
	lastSeq := info.State.LastSeq
	if lastSeq == 0 {
		m.logger.Info("assistant-turn-minter: message stream empty — nothing to replay",
			"stream", assistantTurnStreamName)
		return
	}

	// Buffer the collector generously and independently of the raised pending
	// limits: a non-blocking send that finds the channel full would DROP, so the
	// channel must comfortably hold the retained tail.
	//
	// Sizing is derived from the LIVE stream depth, NOT a hardcoded constant.
	// MESSAGE_EVENTS used to cap total messages at MaxMsgs=10000, so a fixed
	// 10k buffer could never overflow. That cap was DROPPED (MaxMsgs=-1) when the
	// stream moved to per-subject retention (MaxMsgsPerSubject, kbeads
	// eventbus/streams.go) so the whole-fleet thinking/usage volume can no longer
	// evict a plan's turn events — but that also means the retained tail can
	// now exceed 10000 messages, and a fixed 10k buffer would SILENTLY drop the
	// OLDEST turns: exactly the intermediate-outage-turn gap this replay exists to
	// close. This subscription now delivers the WHOLE stream (all event kinds), so
	// info.State.Msgs is an EXACT bound on what the collector receives; sizing to
	// it (clamped) means the collector holds the whole tail. Clamped to a floor
	// (small streams behave as before) and to backfillPendingMsgsLimit (beyond
	// that the async pending
	// buffer itself is the binding limit, so a larger channel buys nothing).
	collectorCap := min(max(int(info.State.Msgs), backfillCollectorMinCap), backfillPendingMsgsLimit)
	collected := make(chan *nats.Msg, collectorCap)
	var collectorDrops atomic.Int64
	sub, err := js.Subscribe(turnEventSubject, func(msg *nats.Msg) {
		// FAST path only: append and return. No daemon call here — that is what
		// keeps the async pending buffer from backing up and dropping the tail.
		select {
		case collected <- msg:
		default:
			// Collector full — the tail exceeded even the live-depth-sized buffer.
			// Count it so the drop is never silent; the post-drain log surfaces a
			// non-zero count as a completeness-floor breach to investigate rather
			// than a mystery missing turn.
			collectorDrops.Add(1)
		}
	}, nats.DeliverAll(), nats.OrderedConsumer())
	if err != nil {
		m.logger.Info("assistant-turn-minter: backfill subscribe failed — live-only", "error", err)
		return
	}
	defer func() { _ = sub.Unsubscribe() }()
	// Raise the async pending buffer above the 100MB stream cap so a burst
	// delivery cannot overflow the default 64MB limit and drop the oldest turns
	// before the collector sees them. Best-effort (older nats.go always supports
	// this; ignore any error rather than fail the whole replay).
	_ = sub.SetPendingLimits(backfillPendingMsgsLimit, backfillPendingBytesLimit)

	// PHASE 1 — DRAIN: collect the full retained tail into a slice. Terminate
	// deterministically on the LastSeq/NumPending signal; fall back to the idle
	// timeout only if that signal never arrives; hard-bound by the overall
	// wall-clock ceiling and ctx cancellation.
	overall := time.NewTimer(backfillMaxDuration)
	defer overall.Stop()
	idle := time.NewTimer(backfillIdleTimeout)
	defer idle.Stop()
	var msgs []*nats.Msg
	reachedTail := false
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
			idle.Reset(backfillIdleTimeout)
			if backfillReachedTail(msg, lastSeq) {
				reachedTail = true
				break drain
			}
		case <-idle.C:
			break drain
		case <-overall.C:
			break drain
		case <-ctx.Done():
			return
		}
	}

	// PHASE 2 — MINT: run the CreateBead round-trips AFTER the burst is drained,
	// so a slow daemon here can never overflow the drain buffer. Abort promptly on
	// shutdown so Close() is not held by a large remaining slice.
	minted := 0
	for _, msg := range msgs {
		if ctx.Err() != nil {
			break
		}
		// Replay mints derive from the backfill lifetime ctx so Close() unwinds an
		// in-flight replay CreateBead promptly (the live path uses Background).
		m.handleMessageCtx(ctx, msg)
		minted++
	}
	drops := collectorDrops.Load()
	if len(msgs) > 0 || drops > 0 {
		m.logger.Info("assistant-turn-minter: replayed retained tail from JetStream",
			"stream", assistantTurnStreamName,
			"events", len(msgs),
			"processed", minted,
			"reached_tail", reachedTail,
			"last_seq", lastSeq,
			"collector_cap", collectorCap,
			"collector_drops", drops)
		if drops > 0 {
			// Non-zero drops mean the retained assistant_text tail exceeded even the
			// live-depth-sized collector — a completeness-floor breach: some oldest
			// turns were NOT replayed. Surface it loudly rather than leave a mystery
			// missing turn (the exact failure mode the per-subject retention change
			// was meant to prevent).
			m.logger.Warn("assistant-turn-minter: collector dropped replay messages — tail incomplete",
				"stream", assistantTurnStreamName,
				"collector_drops", drops,
				"collector_cap", collectorCap)
		}
	}
}

// backfillReachedTail reports whether a delivered replay message is the last one
// the retained tail will hand us — the deterministic end-of-tail signal that
// replaces a fragile wall-clock idle heuristic. Two independent conditions, from
// the JetStream message metadata: NumPending==0 (the server has no further
// messages pending for this consumer) OR the message's stream sequence has
// reached the LastSeq captured before the subscription (the tail we set out to
// drain is fully delivered). A message with no JetStream metadata (should not
// happen on a JS-delivered message) yields false, so the drain falls back to the
// idle/overall timers rather than terminating early.
func backfillReachedTail(msg *nats.Msg, lastSeq uint64) bool {
	md, err := msg.Metadata()
	if err != nil || md == nil {
		return false
	}
	return tailReached(md.NumPending, md.Sequence.Stream, lastSeq)
}

// tailReached is the pure decision behind backfillReachedTail, split out so the
// end-of-tail policy is unit-testable without a broker-bound JetStream message
// (whose Metadata() requires a live subscription). The retained tail is
// exhausted when the server reports no further pending messages for this
// consumer (numPending==0) OR the delivered message's stream sequence has caught
// up to the LastSeq captured before the subscription.
func tailReached(numPending, streamSeq, lastSeq uint64) bool {
	if numPending == 0 {
		return true
	}
	return streamSeq >= lastSeq
}

// backfillJoinTimeout bounds how long Close() waits for the background replay
// goroutine to unwind, so a wedged/slow daemon during replay cannot make Close()
// (a deferred shutdown step) eat into the k8s termination grace period.
// backfillCancel unblocks the drain and the post-drain mint loop and unwinds an
// in-flight ctx-derived CreateBead, so the join is normally instant; this is only
// the backstop for the rare case a mint is mid-flight on a non-cancellable hop.
const backfillJoinTimeout = 2 * time.Second

// Close cancels the background replay, unsubscribes, closes the NATS connection,
// and joins the replay goroutine (bounded). Order matters: backfillCancel FIRST
// so the drain/mint loops and any in-flight ctx-derived CreateBead unwind, then
// tear down NATS, then wait (bounded) for the goroutine to exit. Safe on a nil
// minter.
func (m *Minter) Close() {
	if m == nil {
		return
	}
	if m.backfillCancel != nil {
		m.backfillCancel()
	}
	if m.sub != nil {
		_ = m.sub.Unsubscribe()
	}
	if m.conn != nil {
		m.conn.Close()
	}
	// Bounded join: normally instant (backfillCancel unblocked the goroutine);
	// the deadline guards against a mint mid-flight on a non-cancellable hop so
	// shutdown never hangs past the grace period.
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(backfillJoinTimeout):
		m.logger.Warn("assistant-turn-minter: background replay did not exit within join deadline")
	}
}

// handleMessage is the NATS callback: decode, route by event kind, cheap-filter
// to plan assistants, resolve the plan, and mint the matching turn bead (an
// assistant prose turn from assistant_text, a tool turn from tool_use). Any
// failure is logged and swallowed — never block the NATS callback goroutine or
// crash on a malformed payload.
//
// This is the LIVE-path entry point (the nats.MsgHandler conn.Subscribe passes),
// so its per-message context derives from context.Background() (bounded by the
// connection close). The background replay path calls handleMessageCtx directly
// with the backfill lifetime context so Close() cancellation unwinds an
// in-flight replay mint.
func (m *Minter) handleMessage(msg *nats.Msg) {
	m.handleMessageCtx(context.Background(), msg)
}

// handleMessageCtx is the shared decode→route→filter→resolve→mint pipeline,
// parameterized by the parent context so the live path (Background) and the
// replay path (backfillCtx) can each control cancellation. It does the common
// work (decode, event-kind routing, actor resolution, plan-assistant filter,
// plan resolution) once, then dispatches to the assistant-text or tool-use mint
// path. Any failure is logged and swallowed — never block the caller's
// goroutine or crash on a malformed payload.
func (m *Minter) handleMessageCtx(parent context.Context, msg *nats.Msg) {
	var w turnEventWire
	if err := json.Unmarshal(msg.Data, &w); err != nil {
		return
	}

	// Route by event kind FIRST: the shared subscription (coop.events.messages.*.*)
	// delivers every coop event suffix, but the minter only mints two kinds —
	// assistant_text → an assistant prose turn, tool_use → a tool turn. Drop
	// thinking/usage/refusal (and any future kind) cheaply so the non-transcript
	// firehose costs nothing. Fall back to the subject suffix when the envelope
	// omits event_type (older producer).
	eventType := w.EventType
	if eventType == "" {
		eventType = eventTypeFromSubject(msg.Subject)
	}
	if eventType != eventTypeAssistantText && eventType != eventTypeToolUse {
		return
	}

	actor := w.Actor
	if actor == "" {
		actor = w.Agent
	}
	if actor == "" {
		actor = assistantActorFromSubject(msg.Subject)
	}
	if actor == "" {
		return
	}

	// CHEAP in-process filter FIRST: BOAT_ROLE is a BARE, comma-separated role
	// name ("plan-gardener", or "crew,thread") — NOT the role:-prefixed label
	// form. Skip anything that is not a plan assistant/gardener so the non-plan
	// fleet costs zero daemon lookups. The authoritative check is the agent-bead
	// resolution below; this is only the fast reject.
	if !roleIsPlanAssistant(w.Role) {
		return
	}

	if w.AgentBeadID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	planID, operator, project, ok := m.resolvePlan(ctx, w.AgentBeadID)
	if !ok {
		return
	}
	// The beads' project label is the authority; the envelope's project (the
	// agent's BOAT_PROJECT) only covers a plan and agent that carry none.
	if project == "" {
		project = strings.TrimSpace(w.Project)
	}

	switch eventType {
	case eventTypeAssistantText:
		m.handleAssistantText(ctx, planID, operator, project, actor, &w)
	case eventTypeToolUse:
		m.handleToolUse(ctx, planID, operator, project, actor, &w)
	}
}

// handleAssistantText mints one assistant (prose) turn bead from an
// assistant_text event: it computes the hydration-invariant turn_id, hydrates
// the full body from coopmux when coop clipped the wire copy, and mints the
// turn. Assumes the caller already resolved the plan + operator and passed a
// deadline-bounded ctx.
func (m *Minter) handleAssistantText(ctx context.Context, planID, operator, project, actor string, w *turnEventWire) {
	// An empty reply carries no prose to persist (coop already drops
	// whitespace-only assistant content; this is belt-and-suspenders).
	if strings.TrimSpace(w.Text) == "" {
		return
	}

	// turn_id is computed from the HYDRATION-INVARIANT wire fields (w.Text +
	// w.FullLength), NOT the post-hydration body, so the live path (which
	// hydrates the full body while the turn is current) and a later full-tail
	// replay of the SAME turn (which sees a clipped wire copy once last_message
	// has advanced and hydration is refused) produce the IDENTICAL turn_id and
	// thus the identical IdempotencyKey — converging on ONE bead via migration
	// 029. Deriving turn_id from the hydrated `text` instead would let those two
	// observations mint two beads with different hashes (the oversized-reply
	// double-mint hole). Hydration therefore only affects the STORED body/
	// truncation flag, never the dedup identity.
	turnID := assistantTurnID(w.SessionID, w.TS, w.Text, w.FullLength)

	// Full text: the assistant_text envelope IS the full reply for anything
	// under the NATS text budget. Only when coop clipped the wire copy do we
	// hydrate the untruncated body from coopmux. If that hydration is unavailable
	// (coopmux disabled / session torn down / error), persist the clipped
	// envelope text but STAMP the truncation so the degradation is visible, never
	// silently persisted as complete.
	text := w.Text
	truncated := false
	if wireTextTruncated(w.Text, w.FullLength) {
		// Hydrate the full body from coopmux — BUT coopmux's GetAgentState returns
		// the session's CURRENT last_message, which on a JetStream backfill replay
		// of an OLDER turn is a NEWER reply, not this turn's. Guard against
		// splicing the wrong text in: coop's wire copy is a truncated PREFIX of
		// the true reply (firstBytes + "..."), so the hydrated body is only valid
		// for THIS turn if it starts with that prefix. Otherwise treat the turn as
		// truncated (visible degradation) rather than persist a mismatched body.
		prefix := strings.TrimSuffix(w.Text, truncationEllipsis)
		if full, ok := m.hydrateFullText(ctx, w.SessionID); ok && len(full) >= len(prefix) && strings.HasPrefix(full, prefix) {
			text = full
		} else {
			truncated = true
		}
	}

	m.mintAssistantTurn(ctx, planID, operator, project, actor, turnID, text, w.FullLength, truncated)
}

// handleToolUse mints one tool turn bead (turn_role=tool) from a tool_use
// event: the plan agent invoked a tool. Mirrors handleAssistantText's dedup
// contract (a stable turn_id + the process-independent IdempotencyKey so a
// re-delivered event, a concurrent live+replay observation, and a second
// replica converge on one bead), but the turn_id is derived from the tool name
// + args summary rather than reply text, and there is no >256KB hydration — a
// tool label is always small, so the wire event IS the whole turn. Assumes the
// caller already resolved the plan + operator and passed a deadline-bounded ctx.
func (m *Minter) handleToolUse(ctx context.Context, planID, operator, project, actor string, w *turnEventWire) {
	toolName := strings.TrimSpace(w.ToolName)
	if toolName == "" {
		// A tool_use event with no tool name is unusable as a tool turn — the
		// render side (decorateToolTurn) drops a nameless tool chip anyway. Skip
		// rather than mint an empty entry.
		return
	}
	argsSummary := strings.TrimSpace(w.ToolArgsSummary)

	// resultSummary is empty: the tool RESULT is a phase-2 concern (it arrives on
	// a LATER user turn, not the assistant's tool_use block, so coop's tool_use
	// forward does not carry it). The render side already treats the result pane
	// as optional, so a result-less tool turn renders cleanly.
	turnID := toolTurnID(w.SessionID, w.TS, toolName, argsSummary)
	m.mintToolTurn(ctx, planID, operator, project, actor, turnID, toolName, argsSummary)
}

// truncationEllipsis is what coop appends to a clipped reply (truncate_str,
// nats_forward.rs pushes "..."). Detecting it lets us recognise a turn clipped
// by only 1-2 bytes, where a naive fullLength>len(text) misses because the
// 3-byte ellipsis makes the wire copy as long as (or longer than) the true
// reply.
const truncationEllipsis = "..."

// wireTextTruncated reports whether coop clipped this reply on the wire. coop's
// truncate_str (nats_forward.rs) returns the string UNCHANGED when it fits the
// budget and otherwise emits firstBytes + "...", while full_length always
// carries the TRUE byte length. So the exact, unambiguous signal is
// full_length != len(wireText): an untruncated reply is byte-for-byte the true
// text (lengths equal), and a clipped reply reports a true length that differs
// from the shortened wire copy. This beats a bare `full_length > len(text)`,
// which misses a reply clipped to within the 3-byte ellipsis of its true length
// (there the "..." can make the wire copy as long as, or longer than, the true
// text). full_length == 0 means coop reported no length (older producer) — treat
// as not truncated. Note the pathological case full_length == end+3 (a reply
// clipped by exactly the ellipsis width) is indistinguishable from an untruncated
// reply on the wire and is accepted as-is — a 3-byte fidelity edge, not a gap.
func wireTextTruncated(wireText string, fullLength int) bool {
	if fullLength == 0 {
		return false
	}
	return fullLength != len(wireText)
}

// assistantActorFromSubject extracts the <actor> segment from a coop message
// subject of the form "coop.events.messages.<actor>.<event>" for ANY event
// suffix (assistant_text, thinking, usage, tool_use, …). Returns "" when the
// subject does not match the expected shape. Only a fallback: coop populates the
// envelope's `actor` field directly, so this fires only if that is ever absent.
func assistantActorFromSubject(subject string) string {
	const prefix = "coop.events.messages."
	rest, ok := strings.CutPrefix(subject, prefix)
	if !ok {
		return ""
	}
	// rest = "<actor>.<event>"; the event segment is the last dotted token, the
	// actor is everything before it. Agent names contain no dots, but split from
	// the right defensively so a future dotted event suffix still parses.
	i := strings.LastIndexByte(rest, '.')
	if i <= 0 {
		return ""
	}
	return rest[:i]
}

// eventTypeFromSubject extracts the <event> suffix from a coop message subject
// "coop.events.messages.<actor>.<event>". Only a fallback for the event_type
// envelope field (which coop always stamps) so a legacy producer that omitted
// event_type still routes. Returns "" when the subject does not match the shape.
func eventTypeFromSubject(subject string) string {
	const prefix = "coop.events.messages."
	rest, ok := strings.CutPrefix(subject, prefix)
	if !ok {
		return ""
	}
	// The event segment is the last dotted token (mirrors assistantActorFromSubject,
	// which takes everything before it).
	i := strings.LastIndexByte(rest, '.')
	if i < 0 || i+1 >= len(rest) {
		return ""
	}
	return rest[i+1:]
}

// roleIsPlanAssistant reports whether a BARE, comma-separated BOAT_ROLE value
// names the plan-gardener role. Compares against the bare role name
// (plantranscript.PlanGardenerRole), NOT the role:-prefixed label — the wire
// envelope carries the unprefixed role (podmanager stamps BOAT_ROLE verbatim,
// e.g. "plan-gardener" or "crew,thread").
func roleIsPlanAssistant(role string) bool {
	for r := range strings.SplitSeq(role, ",") {
		if strings.TrimSpace(r) == plantranscript.PlanGardenerRole {
			return true
		}
	}
	return false
}

// resolvePlan returns the plan epic id an agent bead is bound to, the plan's
// operator (for mint attribution), the plan's project (for the turn's
// project:<land> label), and whether it is genuinely a plan assistant.
// Results (positive AND negative) are cached with a short TTL so a chatty
// assistant and a cold-cache backfill burst do not hammer the daemon. A daemon
// error degrades to "not a plan assistant" (skip the turn) rather than blocking.
func (m *Minter) resolvePlan(ctx context.Context, agentBeadID string) (string, string, string, bool) {
	if cached, ok := m.cachedBinding(agentBeadID); ok {
		return cached.planID, cached.operator, cached.project, cached.isPlanAssistant && cached.planID != ""
	}

	bead, err := m.daemon.GetBead(ctx, agentBeadID)
	if err != nil {
		// A transient daemon error must NOT be cached as a negative binding —
		// doing so would silently suppress a real plan-assistant's turns for the
		// full TTL after a single blip. Skip THIS turn and retry on the next one
		// (the retry is idempotent; the missed turn is recovered by JetStream
		// backfill or a later re-delivery, both dedup-safe via IdempotencyKey).
		m.logger.Debug("assistant-turn-minter: resolve agent bead", "agent_bead", agentBeadID, "error", err)
		return "", "", "", false
	}
	if bead == nil {
		// A genuine "no such bead" (not an error) IS a stable negative — cache it.
		m.storeBinding(agentBeadID, planBinding{isPlanAssistant: false, at: m.now()})
		return "", "", "", false
	}

	planID := ""
	if bead.Fields != nil {
		planID = strings.TrimSpace(bead.Fields[plantranscript.PlanIDField])
		if planID == "" {
			planID = strings.TrimSpace(bead.Fields[plantranscript.TaskIDField])
		}
	}
	if planID == "" || !plantranscript.IsPlanAssistantBead(bead, planID) {
		m.storeBinding(agentBeadID, planBinding{isPlanAssistant: false, at: m.now()})
		return "", "", "", false
	}

	// Resolve the plan's operator from the epic (operated_by), so the assistant
	// turn attributes to the same human as the operator turns and the background
	// write carries an operator. A fetch failure leaves it empty and the mint
	// falls back to a system-operator sentinel — but in that case we do NOT cache
	// the binding, so the operator is retried on the next turn rather than pinned
	// to the sentinel for the full TTL (the epic is stable; only its fetch
	// failed). A successful fetch with a genuinely-empty operated_by IS cached.
	//
	// The project comes off the two beads already in hand — the plan epic's
	// project label first (the transcript is the plan's), else the agent bead's
	// — so labelling the turn costs no extra daemon read.
	operator := ""
	project := ""
	epicResolved := false
	if epic, eerr := m.daemon.GetBead(ctx, planID); eerr == nil && epic != nil {
		operator = strings.TrimSpace(epic.OperatedBy)
		project = projectFromLabels(epic.Labels)
		epicResolved = true
	}
	if project == "" {
		project = projectFromLabels(bead.Labels)
	}

	if epicResolved {
		m.storeBinding(agentBeadID, planBinding{isPlanAssistant: true, planID: planID, operator: operator, project: project, at: m.now()})
	}
	return planID, operator, project, true
}

// projectFromLabels returns the first non-empty project:<land> label value, or
// "" when there is none.
func projectFromLabels(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// withProjectLabel adds the turn's single project:<land> label so the turn
// lands in its plan's land instead of being projectless (and so absent from
// every land-scoped view). An empty project leaves the turn unlabelled — never
// guessed. Turn beads carry no fields.channel_id, so kbeads' channel/land rule
// (channel_validation.go) does not apply and this one label cannot clash.
func withProjectLabel(req beadsapi.CreateBeadRequest, project string) beadsapi.CreateBeadRequest {
	if project != "" {
		req.Labels = append(req.Labels, "project:"+project)
	}
	return req
}

func (m *Minter) cachedBinding(agentBeadID string) (planBinding, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.cache[agentBeadID]
	if !ok {
		return planBinding{}, false
	}
	if m.now().Sub(b.at) > planAssistantResolveTTL {
		delete(m.cache, agentBeadID)
		return planBinding{}, false
	}
	return b, true
}

func (m *Minter) storeBinding(agentBeadID string, b planBinding) {
	m.mu.Lock()
	m.cache[agentBeadID] = b
	m.sweepExpiredLocked(b.at)
	m.mu.Unlock()
}

// sweepExpiredLocked drops every entry whose TTL has elapsed. Eviction is
// otherwise lazy-on-read (cachedBinding), so an agent bead that emits a turn
// then despawns — never re-read after its TTL — would be orphaned in the map
// forever, growing it without bound over the always-on controller's lifetime
// (a slow OOM in a memory-capped container). This bounds the map to the live
// working set. It is time-gated to fire at most once per TTL so the O(n) scan
// is amortized to O(1) per store; the caller must hold m.mu. `now` is passed in
// so the sweep shares the store's clock (injectable for tests).
func (m *Minter) sweepExpiredLocked(now time.Time) {
	if now.Sub(m.lastSweep) < planAssistantResolveTTL {
		return
	}
	m.lastSweep = now
	for id, binding := range m.cache {
		if now.Sub(binding.at) > planAssistantResolveTTL {
			delete(m.cache, id)
		}
	}
}

// hydrateFullText fetches the full untruncated last assistant reply from coopmux
// for a >256KB turn whose NATS copy was clipped. Returns ("", false) when
// coopmux is disabled, the session is gone, or any error — the caller then keeps
// the clipped text and marks it truncated.
func (m *Minter) hydrateFullText(ctx context.Context, sessionID string) (string, bool) {
	if m.coopmux == nil || m.coopmux.Disabled() || sessionID == "" {
		return "", false
	}
	st, err := m.coopmux.GetAgentState(ctx, sessionID)
	if err != nil || st == nil {
		m.logger.Debug("assistant-turn-minter: hydrate full text", "session", sessionID, "error", err)
		return "", false
	}
	if strings.TrimSpace(st.LastMessage) == "" {
		return "", false
	}
	return st.LastMessage, true
}

// mintAssistantTurn writes one durable assistant-turn bead for the given
// (already-computed, hydration-invariant) turnID. The
// CreateBeadRequest.IdempotencyKey = "plan-turn:<planID>:<turn_id>" is the
// authoritative, process-independent dedup guard (lookup-or-return; migration
// 029), so a re-delivered event, a concurrent live+replay observation of the
// same turn, a second replica, and a future additional observer all converge on
// ONE bead. Best-effort: a daemon error is logged, never propagated.
func (m *Minter) mintAssistantTurn(ctx context.Context, planID, operator, project, actor, turnID, text string, fullLength int, truncated bool) {
	req := withProjectLabel(plantranscript.TurnBead(planID, actor, plantranscript.TurnRoleAssistant, text, turnID), project)
	req.IdempotencyKey = plantranscript.IdempotencyKey(planID, turnID)

	if truncated {
		// Re-marshal the fields to add the visible-degradation markers without
		// duplicating TurnBead's field set. Unmarshal → set → re-marshal is cheap
		// (a handful of keys) and keeps the turn-bead shape authoritative in one
		// place.
		var fields map[string]string
		if err := json.Unmarshal(req.Fields, &fields); err == nil {
			fields["text_truncated"] = "true"
			fields["full_length"] = strconv.Itoa(fullLength)
			if remarshalled, err := json.Marshal(fields); err == nil {
				req.Fields = remarshalled
			}
		}
	}

	// Stamp the operator on the ctx so the daemon audit resolver attributes the
	// mint to the human the assistant is working for (consistent with the
	// operator-turn mint) AND so a background write with no OIDC identity does
	// not fail-closed with a 400. Prefer the plan's operator; fall back to a
	// service-identity system sentinel when the epic carries none.
	if operator != "" {
		ctx = beadsapi.WithOperator(ctx, operator)
	} else {
		ctx = beadsapi.WithSystemOperator(ctx, assistantTurnSystemOperator)
	}

	id, err := m.daemon.CreateBead(ctx, req)
	if err != nil {
		m.logger.Warn("assistant-turn-minter: mint turn bead", "plan", planID, "actor", actor, "turn_id", turnID, "error", err)
		return
	}
	m.logger.Info("assistant_turn_minted",
		"event", "assistant_turn_minted",
		"plan", planID,
		"actor", actor,
		"turn_id", turnID,
		"bead", id,
		"len", len(text),
		"truncated", truncated)
}

// mintToolTurn writes one durable TOOL turn bead (turn_role=tool) for the given
// (already-computed) turnID, via the shared plantranscript.ToolTurnBead write
// contract so the controller's tool-turn mint and the viewer's render side stay
// byte-identical. The IdempotencyKey = "plan-turn:<planID>:<turn_id>" is the
// SAME authoritative dedup namespace as assistant turns, so a re-delivered
// event, a concurrent live+replay observation, and a second replica all
// converge on ONE bead. resultSummary is empty (phase 2). Best-effort: a daemon
// error is logged, never propagated.
func (m *Minter) mintToolTurn(ctx context.Context, planID, operator, project, actor, turnID, toolName, argsSummary string) {
	req := withProjectLabel(plantranscript.ToolTurnBead(planID, actor, toolName, argsSummary, "", turnID), project)
	req.IdempotencyKey = plantranscript.IdempotencyKey(planID, turnID)

	// Stamp the operator on the ctx exactly as the assistant-turn mint does, so
	// the daemon audit resolver attributes the tool turn to the human the
	// assistant works for and a background write with no OIDC identity does not
	// fail-closed with a 400. Prefer the plan's operator; fall back to the
	// service-identity system sentinel when the epic carries none.
	if operator != "" {
		ctx = beadsapi.WithOperator(ctx, operator)
	} else {
		ctx = beadsapi.WithSystemOperator(ctx, assistantTurnSystemOperator)
	}

	id, err := m.daemon.CreateBead(ctx, req)
	if err != nil {
		m.logger.Warn("assistant-turn-minter: mint tool turn bead", "plan", planID, "actor", actor, "turn_id", turnID, "tool", toolName, "error", err)
		return
	}
	m.logger.Info("tool_turn_minted",
		"event", "tool_turn_minted",
		"plan", planID,
		"actor", actor,
		"turn_id", turnID,
		"bead", id,
		"tool", toolName)
}

// assistantTurnID builds the per-turn id for an assistant reply:
// "<session_id>:<ts>:<content-hash>". The (session, ts) pair identifies the
// coop event (stable across live + JetStream re-delivery); the content hash
// disambiguates two DISTINCT replies published in the same epoch-millisecond so
// they do not collide and drop one.
//
// HYDRATION-INVARIANT: the hash is over the WIRE fields (wireText + fullLength),
// NOT the post-hydration body. coop stamps a byte-stable wire copy and the true
// full_length at publish time, so both are identical on live delivery and on a
// later JetStream replay of the same event — even when the >256KB hydration
// outcome differs between the two (the live path splices the full body; a replay
// after last_message advanced keeps the clipped copy). Hashing the wire fields
// makes both observations produce the SAME turn_id → SAME IdempotencyKey → ONE
// bead, closing the oversized-reply double-mint hole. fullLength is folded in so
// two replies that share a wire prefix but differ in true length still hash
// distinctly.
func assistantTurnID(sessionID string, ts int64, wireText string, fullLength int) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(wireText))
	_, _ = h.Write([]byte{0}) // separator so (wireText,fullLength) is unambiguous
	_, _ = h.Write([]byte(strconv.Itoa(fullLength)))
	return sessionID + ":" + strconv.FormatInt(ts, 10) + ":" + strconv.FormatUint(h.Sum64(), 16)
}

// toolTurnID builds the per-turn id for a TOOL call:
// "<session_id>:<ts>:<hash(tool+args)>", the tool-turn analogue of
// assistantTurnID. The (session, ts) pair identifies the coop event (stable
// across live + JetStream re-delivery); the hash over the tool name + args
// summary disambiguates two DISTINCT tool calls a plan agent published in the
// same epoch-millisecond (e.g. parallel tool_use blocks in one assistant turn,
// which coop forwards as separate tool_use events sharing a ts) so they do not
// collide and drop one. A separator byte keeps (tool, args) unambiguous — so
// ("Bash","x")+("","y") and ("Bash","xy")+("","") never hash alike. Matches the
// turnID contract the task specifies (<session>:<ts>:<hash(tool+args)>).
func toolTurnID(sessionID string, ts int64, toolName, argsSummary string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(toolName))
	_, _ = h.Write([]byte{0}) // separator so (toolName,argsSummary) is unambiguous
	_, _ = h.Write([]byte(argsSummary))
	return sessionID + ":" + strconv.FormatInt(ts, 10) + ":" + strconv.FormatUint(h.Sum64(), 16)
}
