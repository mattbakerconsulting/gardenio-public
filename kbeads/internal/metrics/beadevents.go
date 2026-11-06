package metrics

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Beads-events counters at the source (epic E4, ka-0000001152).
//
// These ride the single bead-domain emission chokepoint
// (server.BeadsServer.recordAndPublish) and the eventbus dispatch chokepoint
// (eventbus.Bus.Dispatch) — exactly where the event is already published —
// rather than introducing a second consumer, a JetStream durable, or any
// postgres dedup state. One increment per emitted event, counted
// UNCONDITIONALLY at the chokepoint so the counter reflects true emission
// volume independent of the migration-026 fail-closed audit skip (see
// gardenio/docs/research/applicability/R4-delta.md §2.1). The events-table
// backfill (E4b) is therefore a best-effort historical lower bound, never an
// authoritative replay — do not assert events-table-count == counter-count.
//
// Decision lifecycle events are already instrumented separately via
// gardenio_decisions_lifecycle_total (decisions.go); this file deliberately
// does NOT re-count them.

// Bead-domain `event` label values. Bounded enum: every recognized topic (and
// the inferred sub-type of an `updated` event) maps onto one of these; the
// long tail buckets to BeadEventOther so a new or misspelled topic can never
// inflate cardinality.
const (
	BeadEventCreated   = "created"
	BeadEventClosed    = "closed"
	BeadEventDeleted   = "deleted"
	BeadEventUpdated   = "updated"
	BeadEventClaimed   = "claimed"        // updated w/ assignee set non-empty
	BeadEventReleased  = "released"       // updated w/ assignee cleared
	BeadEventStatus    = "status_changed" // updated w/ status field
	BeadEventOwner     = "owner_changed"  // updated w/ owner field
	BeadEventTransfer  = "transferred"
	BeadEventDepAdd    = "dependency_added"
	BeadEventDepRemove = "dependency_removed"
	BeadEventLabelAdd  = "label_added"
	BeadEventLabelRem  = "label_removed"
	BeadEventComment   = "comment_added"
	BeadEventCommentRm = "comment_deleted"
	BeadEventFollowAdd = "follower_added"
	BeadEventFollowRem = "follower_removed"
	BeadEventAdviceNew = "advice_created"
	BeadEventAdviceUpd = "advice_updated"
	BeadEventAdviceDel = "advice_deleted"
	BeadEventJackOn    = "jack_on"
	BeadEventJackOff   = "jack_off"
	BeadEventJackExt   = "jack_extended"
	BeadEventJackExp   = "jack_expired"
	BeadEventMailSent  = "mail_sent"
	BeadEventSession   = "session"
	BeadEventOther     = "other"
)

// beadEventLabels is the closed set of `event` label values. Anything not in
// this set is rejected by NormalizeBeadEvent and bucketed to BeadEventOther.
var beadEventLabels = map[string]struct{}{
	BeadEventCreated: {}, BeadEventClosed: {}, BeadEventDeleted: {},
	BeadEventUpdated: {}, BeadEventClaimed: {}, BeadEventReleased: {},
	BeadEventStatus: {}, BeadEventOwner: {}, BeadEventTransfer: {},
	BeadEventDepAdd: {}, BeadEventDepRemove: {}, BeadEventLabelAdd: {},
	BeadEventLabelRem: {}, BeadEventComment: {}, BeadEventCommentRm: {},
	BeadEventFollowAdd: {}, BeadEventFollowRem: {}, BeadEventAdviceNew: {},
	BeadEventAdviceUpd: {}, BeadEventAdviceDel: {}, BeadEventJackOn: {},
	BeadEventJackOff: {}, BeadEventJackExt: {}, BeadEventJackExp: {},
	BeadEventMailSent: {}, BeadEventSession: {}, BeadEventOther: {},
}

// knownBeadTypes is the bounded allow-list of bead `type` values for the
// bead_type label. Custom / operator-defined types and the empty string
// bucket to "other" so cardinality stays bounded regardless of type-config
// proliferation. Mirrors model.BeadType constants; kept as plain strings here
// to avoid the metrics package importing model.
var knownBeadTypes = map[string]struct{}{
	"epic": {}, "task": {}, "feature": {}, "chore": {}, "bug": {},
	"advice": {}, "jack": {}, "decision": {}, "escalation": {}, "report": {},
	"formula": {}, "agent": {}, "schedule": {}, "trigger": {}, "mail": {},
	"fleet": {}, "project": {}, "account": {},
}

// Agent-lifecycle `event` label values for gardenio_agents_lifecycle_total.
// Heartbeat is split into its own high-volume counter and is NOT a value here.
const (
	AgentEventStarted = "started"
	AgentEventStopped = "stopped"
	AgentEventCrashed = "crashed"
	AgentEventIdle    = "idle"
)

var (
	// BeadEvents counts bead-domain events at the recordAndPublish chokepoint,
	// broken out by inferred event sub-type, project, and bead type. The
	// project label is "unknown" when the event payload does not carry the
	// bead row (dependency/label/comment-only payloads).
	BeadEvents = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_events_total",
			Help: "Count of bead-domain events emitted by kbeads, by event sub-type, project, and bead type.",
		},
		[]string{"event", "project", "bead_type"},
	)

	// AgentLifecycle counts agent lifecycle events (started/stopped/crashed/
	// idle) dispatched through the eventbus, by event and role. Heartbeats are
	// excluded here (see AgentHeartbeats).
	AgentLifecycle = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_agents_lifecycle_total",
			Help: "Count of agent lifecycle events (started/stopped/crashed/idle), by event and role.",
		},
		[]string{"event", "role"},
	)

	// AgentHeartbeats counts agent heartbeat events, by role. Split out from
	// the lifecycle counter because heartbeats are high-volume and would
	// otherwise dominate the lifecycle series (R4-delta §4).
	AgentHeartbeats = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_agents_heartbeats_total",
			Help: "Count of agent heartbeat events, by role.",
		},
		[]string{"role"},
	)

	// BeadsCreatedByOperator counts bead.created events by resolved operator,
	// giving /charts one time-series per human who creates beads (operator ask
	// ka-0000003758, task ka-0000007678). Deliberately a SEPARATE counter from
	// gardenio_beads_events_total: the operator dimension is only meaningful for
	// the created family, and attaching an operator label to the 25-family
	// events counter would multiply its series count by the operator set across
	// every family. Scoped to bead.created, the series count equals the number
	// of distinct operator labels that ever create a bead.
	//
	// Cardinality is bounded by NormalizeOperatorLabel: a real human keeps its
	// resolver id ("slack:U…" / "oidc:…@…") — the human roster is a bounded set
	// (dozens, not thousands) — while every non-human resolver output buckets
	// to one of the fixed sentinels {agent, system, fleet, unknown}. A new
	// human adds exactly one series; agent/automation traffic can never inflate
	// it.
	BeadsCreatedByOperator = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_created_by_operator_total",
			Help: "Count of bead.created events by resolved operator (human id, or one of agent/system/fleet/unknown). One series per human who creates beads.",
		},
		[]string{"operated_by"},
	)
)

// RegisterBeadEvents registers the bead-events and agent-lifecycle counters on
// the given registry. Safe to call once per registry.
func RegisterBeadEvents(reg prometheus.Registerer) {
	reg.MustRegister(BeadEvents, AgentLifecycle, AgentHeartbeats, BeadsCreatedByOperator)
}

// ObserveBeadCreatedByOperator bumps gardenio_beads_created_by_operator_total
// for one bead.created event. operatorLabel must already be a bounded label
// value (the caller classifies the raw resolver output via the server-side
// operator-shape helpers, then passes the bucketed string here so this package
// stays free of the operator-classification logic).
func ObserveBeadCreatedByOperator(operatorLabel string) {
	if operatorLabel == "" {
		operatorLabel = "unknown"
	}
	BeadsCreatedByOperator.WithLabelValues(operatorLabel).Inc()
}

// ObserveBeadEvent bumps gardenio_beads_events_total for one bead-domain event.
// event/project/beadType are normalized to their bounded label values. An
// empty project becomes "unknown"; an empty or unknown event becomes "other".
func ObserveBeadEvent(event, project, beadType string) {
	BeadEvents.WithLabelValues(
		NormalizeBeadEvent(event),
		normalizeProject(project),
		NormalizeBeadType(beadType),
	).Inc()
}

// ObserveAgentLifecycle bumps gardenio_agents_lifecycle_total for one agent
// lifecycle event. event must be one of the AgentEvent* constants; role is
// normalized via NormalizeRole.
func ObserveAgentLifecycle(event, role string) {
	if event == "" {
		return
	}
	AgentLifecycle.WithLabelValues(event, NormalizeRole(role)).Inc()
}

// ObserveAgentHeartbeat bumps gardenio_agents_heartbeats_total for one agent
// heartbeat, by normalized role.
func ObserveAgentHeartbeat(role string) {
	AgentHeartbeats.WithLabelValues(NormalizeRole(role)).Inc()
}

// NormalizeBeadEvent maps an arbitrary event string onto the bounded enum.
// Unrecognized / empty → BeadEventOther.
func NormalizeBeadEvent(event string) string {
	e := strings.ToLower(strings.TrimSpace(event))
	if _, ok := beadEventLabels[e]; ok {
		return e
	}
	return BeadEventOther
}

// NormalizeBeadType maps an arbitrary bead-type string onto the bounded
// allow-list. Unrecognized / empty → "other".
func NormalizeBeadType(beadType string) string {
	t := strings.ToLower(strings.TrimSpace(beadType))
	if _, ok := knownBeadTypes[t]; ok {
		return t
	}
	return "other"
}

// normalizeProject returns the project label value, defaulting an empty
// project to "unknown" so the label is always present and bounded.
func normalizeProject(project string) string {
	return NormalizeProjectLabel(project)
}

// NormalizeProjectLabel is the exported form of normalizeProject, used by the
// E4b events-table backfill (kd events backfill) so a replayed sample carries
// the exact same project label value the live ObserveBeadEvent would produce —
// empty → "unknown".
func NormalizeProjectLabel(project string) string {
	p := strings.TrimSpace(project)
	if p == "" {
		return "unknown"
	}
	return p
}
