package server

import (
	"strings"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/model"
)

// recordBeadEventMetric bumps gardenio_beads_events_total for one bead-domain
// event at the recordAndPublish chokepoint. It is called UNCONDITIONALLY at
// the top of recordAndPublish — before the migration-026 fail-closed audit
// skip — so the counter reflects true emission volume, not the subset of
// events that reach the durable events table (R4-delta §2.1). The events-table
// backfill (E4b) is a best-effort historical lower bound, not a replay; never
// assert that the two agree.
//
// project / bead_type labels are pulled from the marshaled bead when the
// payload carries one (the create/update/close/transfer/advice/jack family);
// dependency/label/comment/follower payloads do not carry the bead row, so
// those events count with project="unknown", bead_type="other".
func recordBeadEventMetric(topic string, event any) {
	eventLabel, ok := beadEventLabel(topic, event)
	if !ok {
		return
	}
	project, beadType := beadEventLabelsFromPayload(event)
	metrics.ObserveBeadEvent(eventLabel, project, beadType)
}

// recordBeadCreatedByOperator bumps gardenio_beads_created_by_operator_total for
// a bead.created event, attributing it to the resolved operator. Unlike
// recordBeadEventMetric (counted unconditionally at function entry on every
// topic), this is called only for TopicBeadCreated and only after the operator
// is resolved, because the whole point of the counter is the per-human
// breakdown. operatedBy is the resolver output (possibly "" when the operator
// could not be attributed); it is bucketed to a bounded label by
// operatorChartLabel before reaching the metrics package.
func recordBeadCreatedByOperator(topic, operatedBy string) {
	if topic != events.TopicBeadCreated {
		return
	}
	metrics.ObserveBeadCreatedByOperator(operatorChartLabel(operatedBy))
}

// operatorChartLabel buckets a resolver-emitted operated_by string onto the
// bounded label set for gardenio_beads_created_by_operator_total. A well-formed
// human identity ("slack:U…" / "oidc:…@…") is kept verbatim — the human roster
// is small and bounded, so one series per human is the desired behaviour. Every
// other shape collapses to a fixed sentinel so agent/automation traffic can
// never inflate the series count:
//   - ""              → "unknown"   (operator unresolvable / fail-closed)
//   - "system:…"      → "system"    (sentinel actors)
//   - "fleet:…"       → "fleet"
//   - anything else   → "agent"     (bare agent ids, "static", "thread-…", …)
func operatorChartLabel(operatedBy string) string {
	if operatedBy == "" {
		return "unknown"
	}
	if isHumanOperator(operatedBy) {
		return operatedBy
	}
	if strings.HasPrefix(operatedBy, "system:") {
		return "system"
	}
	if strings.HasPrefix(operatedBy, "fleet:") {
		return "fleet"
	}
	return "agent"
}

// beadEventLabel maps a topic (and, for updates, the inferred sub-type from
// the BeadUpdated.Changes map) onto the bounded metric `event` label. The bool
// is false for topics that should not be counted as a bead event (none today —
// every known topic maps to a label, and the long tail falls through to
// metrics.BeadEventOther).
//
// The topic→label projection itself lives in metrics.ProjectBeadEventLabel,
// shared verbatim with the historical events-table replay (kd events backfill,
// E4b), so the live counter and a backfill of the same window can never drift
// on how a topic is labeled. This wrapper only unwraps the live
// BeadUpdated.Changes map out of the typed event payload before delegating.
func beadEventLabel(topic string, event any) (string, bool) {
	var changes map[string]any
	if upd, ok := event.(events.BeadUpdated); ok {
		changes = upd.Changes
	}
	return metrics.ProjectBeadEventLabel(topic, changes), true
}

// beadEventLabelsFromPayload extracts the project and bead-type labels from a
// bead-carrying event payload. Returns ("", "") for payloads that do not embed
// the bead row, which ObserveBeadEvent normalizes to ("unknown", "other").
func beadEventLabelsFromPayload(event any) (project, beadType string) {
	b := beadFromPayload(event)
	if b == nil {
		return "", ""
	}
	return projectFromLabels(b.Labels), string(b.Type)
}

// beadFromPayload returns the embedded *model.Bead from any of the bead-domain
// event payload structs that carry one, or nil if the payload is bead-less.
func beadFromPayload(event any) *model.Bead {
	switch e := event.(type) {
	case events.BeadCreated:
		return e.Bead
	case events.BeadUpdated:
		return e.Bead
	case events.BeadClosed:
		return e.Bead
	case events.BeadTransferred:
		return e.Bead
	case events.AdviceCreated:
		return e.Bead
	case events.AdviceUpdated:
		return e.Bead
	case events.JackOn:
		return e.Bead
	case events.JackOff:
		return e.Bead
	case events.JackExtended:
		return e.Bead
	}
	return nil
}
