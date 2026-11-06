package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// gardenio_assistant_event_total counts Slack agent-surface (DM / AI pane)
// events the bridge actually handled, broken out by event kind. It exists to
// make the "agent pane goes silent" failure observable (ka-0000000892).
//
// The failure mode: after an agent_view / assistant_view enable + workspace
// reinstall, Slack keeps delivering the lifecycle events (agent_view:
// app_home_opened / app_context_changed; legacy assistant_view:
// assistant_thread_started / assistant_thread_context_changed) but stops
// delivering the message.im event that carries the user's typed text. Visually
// the pane sets up fine — the suggested prompts post — but every typed message
// bounces with "Slack couldn't send this message". There is no error to grep
// for, only an absence, which is exactly what a counter makes alertable.
//
// The label VALUES are intentionally stable across the agent_view migration
// (ka-0000004985) so the shipped dashboard + AssistantPaneSilent alert keep
// working: app_home_opened records "thread_started" (its agent_view successor),
// app_context_changed records "context_changed", and a delivered user DM
// records "message" (recorded at the message.im choke point in
// handleMessageEvent under agent_view, or handleAssistantMessage on the legacy
// container — mutually exclusive, so no double count).
//
// The smoke signal a dashboard/alert watches for: thread_started increasing
// while message stays flat over the same window means inbound delivery is
// broken. See gardenio/docs/assistant-pane-smoke-check.md for the PromQL.
//
// Labels:
//   - event: HARD enum {thread_started, context_changed, message}. The enum
//     guard rejects anything else so a future caller cannot inflate
//     cardinality.
//
// Registered globally against the controller's default registerer (promauto),
// matching the other bridge metrics (see operator_activity_metrics.go).
var assistantEventTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_assistant_event_total",
		Help: "Slack assistant-container events handled by the bridge, labelled by event kind (enum: thread_started/context_changed/message). thread_started rising while message stays flat is the 'pane silently eats typed messages' signature (ka-0000000892).",
	},
	[]string{"event"},
)

// Bounded event enum — the only members allowed as the event label.
// recordAssistantEvent refuses anything outside this set.
const (
	assistantEventThreadStarted  = "thread_started"
	assistantEventContextChanged = "context_changed"
	assistantEventMessage        = "message"
)

// assistantEventValid reports whether e is a member of the bounded event enum.
func assistantEventValid(e string) bool {
	switch e {
	case assistantEventThreadStarted, assistantEventContextChanged, assistantEventMessage:
		return true
	default:
		return false
	}
}

// recordAssistantEvent increments gardenio_assistant_event_total for one handled
// assistant-container event. An out-of-enum event is DROPPED (no increment,
// returns false) so the metric's cardinality stays bounded.
func (b *Bot) recordAssistantEvent(event string) bool {
	if !assistantEventValid(event) {
		b.logger.Warn("dropping assistant event metric: invalid event", "event", event)
		return false
	}
	assistantEventTotal.WithLabelValues(event).Inc()
	return true
}

// gardenio_mention_delivery_total counts mention deliveries to thread/DM agents
// split by how the bridge reached the agent — the proof artifact for Matt's
// warm-idle lower-latency goal (epic ka-0000006987, task ka-0000001565).
//
// When a Slack mention/follow-up lands for a thread-bound agent the bridge
// forks one of two ways (bot_mentions.go):
//   - WARM: the agent bead is still active (FindAgentBead succeeds) → the bridge
//     delivers a live nudge via nudgeAgentForMention. The pod is already up, so
//     the user gets a near-instant reply.
//   - COLD: the agent bead is gone (completed/despawned) → respawnThreadAgent
//     re-creates the bead with --resume, the pod boots from scratch, and the
//     user waits out a full cold-start before the first token.
//
// The warm-idle keepalive (ka-0000005793) exists to convert cold hits into warm
// hits by keeping chat-window pods alive through the idle window. The warm-hit
// ratio computed from this counter is exactly the before/after KPI for that
// work. See gardenio/docs/assistant-pane-smoke-check.md for the PromQL.
//
// Labels:
//   - delivery_path: HARD enum {warm, cold}. The enum guard rejects anything
//     else so a future caller cannot inflate cardinality.
var mentionDeliveryTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_mention_delivery_total",
		Help: "Slack mention deliveries to thread/DM agents by delivery path (enum: warm=live nudge via nudgeAgentForMention / cold=respawnThreadAgent boot). warm/(warm+cold) is the warm-hit ratio — the KPI for warm-idle keepalive (epic ka-0000006987).",
	},
	[]string{"delivery_path"},
)

// gardenio_mention_cold_respawn_seconds is the cold-respawn boot latency — the
// wall-clock from the follow-up arriving (top of respawnThreadAgent) to the
// respawned agent's first proof-of-life (SessionStart hook firing, i.e. Claude
// booted in the container; HandleSessionStart flips the stream session live).
// This is the "follow-up -> first token" latency the warm path eliminates;
// histogram buckets span a few seconds (fast resume) to a minute+ (image pull /
// PVC restore). Cold path only — warm nudges never create a new session here.
var mentionColdRespawnSeconds = promauto.NewHistogram(
	prometheus.HistogramOpts{
		Name:    "gardenio_mention_cold_respawn_seconds",
		Help:    "Cold-respawn boot latency for thread/DM agents: seconds from follow-up arrival (respawnThreadAgent) to first proof-of-life (SessionStart). The latency warm-idle keepalive (ka-0000005793) removes.",
		Buckets: []float64{1, 2, 5, 10, 15, 20, 30, 45, 60, 90, 120, 180},
	},
)

// Bounded delivery-path enum — the only members allowed as the delivery_path
// label. recordMentionDelivery refuses anything outside this set.
const (
	mentionDeliveryWarm = "warm"
	mentionDeliveryCold = "cold"
)

// mentionDeliveryPathValid reports whether p is a member of the bounded
// delivery_path enum.
func mentionDeliveryPathValid(p string) bool {
	switch p {
	case mentionDeliveryWarm, mentionDeliveryCold:
		return true
	default:
		return false
	}
}

// recordMentionDelivery increments gardenio_mention_delivery_total for one
// mention delivery. An out-of-enum path is DROPPED (no increment, returns
// false) so the metric's cardinality stays bounded.
func (b *Bot) recordMentionDelivery(path string) bool {
	if !mentionDeliveryPathValid(path) {
		b.logger.Warn("dropping mention delivery metric: invalid delivery_path", "delivery_path", path)
		return false
	}
	mentionDeliveryTotal.WithLabelValues(path).Inc()
	return true
}
