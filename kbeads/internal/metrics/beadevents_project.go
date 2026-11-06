package metrics

import "gardenio/kbeads/internal/events"

// ProjectBeadEventLabel maps a bead-domain event topic — and, for the generic
// update topic, its Changes map — onto the bounded gardenio_beads_events_total
// `event` label. It is the SINGLE SOURCE OF TRUTH for the topic→label
// projection, shared by the live emission path
// (server.recordAndPublish → recordBeadEventMetric) and the historical
// events-table replay (kd events backfill, E4b). Both call this function, so
// the live counter and a backfill of the same window can never drift on how a
// topic is labeled. The long tail (a new or unrecognized topic) buckets to
// BeadEventOther so a backfill never silently drops an event family.
//
// `changes` is only consulted for events.TopicBeadUpdated (to refine
// claimed/released/status_changed/owner_changed); pass nil for every other
// topic. Note this deliberately does NOT infer a sub-type for
// TopicAdviceUpdated — that topic always labels as advice_updated, mirroring
// the live path.
func ProjectBeadEventLabel(topic string, changes map[string]any) string {
	switch topic {
	case events.TopicBeadCreated:
		return BeadEventCreated
	case events.TopicBeadClosed:
		return BeadEventClosed
	case events.TopicBeadDeleted:
		return BeadEventDeleted
	case events.TopicBeadTransferred:
		return BeadEventTransfer
	case events.TopicBeadUpdated:
		return InferUpdateSubtype(changes)
	case events.TopicDependencyAdded:
		return BeadEventDepAdd
	case events.TopicDependencyRemoved:
		return BeadEventDepRemove
	case events.TopicLabelAdded:
		return BeadEventLabelAdd
	case events.TopicLabelRemoved:
		return BeadEventLabelRem
	case events.TopicCommentAdded:
		return BeadEventComment
	case events.TopicCommentDeleted:
		return BeadEventCommentRm
	case events.TopicFollowerAdded:
		return BeadEventFollowAdd
	case events.TopicFollowerRemoved:
		return BeadEventFollowRem
	case events.TopicAdviceCreated:
		return BeadEventAdviceNew
	case events.TopicAdviceUpdated:
		return BeadEventAdviceUpd
	case events.TopicAdviceDeleted:
		return BeadEventAdviceDel
	case events.TopicJackOn:
		return BeadEventJackOn
	case events.TopicJackOff:
		return BeadEventJackOff
	case events.TopicJackExtended:
		return BeadEventJackExt
	case events.TopicJackExpired:
		return BeadEventJackExp
	case events.TopicMailSent:
		return BeadEventMailSent
	case events.TopicSessionEnd, events.TopicSessionPreCommit,
		events.TopicSessionPrePush, events.TopicSessionHandoff:
		return BeadEventSession
	}
	// Long tail (new/unknown topic): bucket to the catch-all so neither the
	// live counter nor a backfill ever silently drops an event family it
	// doesn't recognize.
	return BeadEventOther
}

// InferUpdateSubtype refines a beads.bead.updated event into a finer event
// label from its Changes map, per the v1 family list (claimed / released /
// status_changed / owner_changed). When several fields changed, the most
// operationally meaningful one wins: claim/release > status > owner > generic
// updated. A nil/empty map (or a non-update event whose Changes were not
// extracted) yields the generic BeadEventUpdated.
func InferUpdateSubtype(changes map[string]any) string {
	if changes == nil {
		return BeadEventUpdated
	}
	if v, ok := changes["assignee"]; ok {
		// The change map stores the new assignee as a plain string
		// (beads.go assignee change). A non-empty value is a claim; an empty
		// or non-string value is a release.
		if s, ok := v.(string); ok && s != "" {
			return BeadEventClaimed
		}
		return BeadEventReleased
	}
	if _, ok := changes["status"]; ok {
		return BeadEventStatus
	}
	if _, ok := changes["owner"]; ok {
		return BeadEventOwner
	}
	return BeadEventUpdated
}
