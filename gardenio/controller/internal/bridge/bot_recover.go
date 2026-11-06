package bridge

import (
	"log/slog"
	"runtime/debug"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// gardenio_bridge_handler_panic_total counts panics recovered at a bridge
// handler-goroutine boundary, broken out by the handler that panicked.
//
// The failure this makes observable (ka-0000007939): the bridge dispatches
// every inbound Slack event — Events API, Interactive callbacks, and slash
// commands — into a bare goroutine (see dispatchHandler call sites in bot.go).
// Go does NOT confine a goroutine panic: a panic anywhere in the (very large)
// handler call tree propagates to the goroutine top and terminates the whole
// process. net/http's per-request recover does not cover these goroutines —
// they are Socket-Mode driven and spawned by bridge code directly. Because the
// slack-bridge runs at a single replica (helm replicaCount: 1, see
// slack_event_dedup.go), one such panic is a TOTAL outage of Slack
// notifications, decision delivery, and the trigger engine — and a redelivered
// poison event can re-crash the pod on restart.
//
// dispatchHandler installs a recover boundary so a latent panic degrades to a
// logged error + this counter increment instead of a controller crash. A
// non-zero value on this counter is the alert signal that some handler path is
// panicking (and points at which one via the `handler` label).
//
// Labels:
//   - handler: the logical handler name passed to dispatchHandler
//     (events_api, interactive, slash_command). Low, bounded cardinality.
var handlerPanicTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_bridge_handler_panic_total",
		Help: "Panics recovered at a bridge handler-goroutine boundary, by handler.",
	},
	[]string{"handler"},
)

// dispatchHandler runs fn inside a recover boundary so a panic in an
// externally-triggered handler goroutine degrades to a logged error + a
// handlerPanicTotal increment rather than crashing the single-replica bridge.
//
// It is the panic boundary that net/http gives HTTP handlers for free but that
// the Socket-Mode handler goroutines (spawned directly by bot.go) otherwise
// lack. Call it as the body of each dispatched goroutine; it does NOT itself
// start a goroutine, so the caller keeps ownership of handlerWg / context
// lifetimes.
func (b *Bot) dispatchHandler(handler string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			handlerPanicTotal.WithLabelValues(handler).Inc()
			b.logger.Error("recovered panic in bridge handler goroutine",
				"handler", handler,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()
	fn()
}

// recoverHandlerGoroutine is the *Bot-independent twin of dispatchHandler for
// the SSE dispatch path (SSEStream.dispatch), which spawns a bare goroutine per
// registered bead-lifecycle handler and has no *Bot to hang the method off.
//
// It is the SAME class of gap the dispatchHandler recover boundary closes for
// the Socket-Mode entry points (ka-0000007939): a panic anywhere in an SSE
// handler's call tree (decisions, escalations, agents, chat, mail, squawk,
// commentFwd, formula step-dispatch, alert-rules-sync, …) propagates to the
// goroutine top and terminates the whole single-replica bridge process — a
// total outage of Slack notifications, decision/escalation delivery, and the
// trigger engine. The SSEStream also persists Last-Event-ID and replays on
// restart, so a poison event re-crashes the pod on every reconnect.
//
// Call it as a deferred statement at the top of the dispatched goroutine body
// (defer recoverHandlerGoroutine(logger, topic)); it does NOT start a goroutine
// and shares the handlerPanicTotal counter so the same alert covers both paths.
// The `handler` label carries the SSE topic (bounded cardinality).
func recoverHandlerGoroutine(logger *slog.Logger, handler string) {
	if r := recover(); r != nil {
		handlerPanicTotal.WithLabelValues(handler).Inc()
		if logger != nil {
			logger.Error("recovered panic in bridge SSE handler goroutine",
				"handler", handler,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}
}
