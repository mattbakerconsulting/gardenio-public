package metrics

import (
	"testing"

	"gardenio/kbeads/internal/events"
)

func TestProjectBeadEventLabel(t *testing.T) {
	tests := []struct {
		name  string
		topic string
		want  string
	}{
		{"created", events.TopicBeadCreated, BeadEventCreated},
		{"closed", events.TopicBeadClosed, BeadEventClosed},
		{"deleted", events.TopicBeadDeleted, BeadEventDeleted},
		{"transferred", events.TopicBeadTransferred, BeadEventTransfer},
		{"dep added", events.TopicDependencyAdded, BeadEventDepAdd},
		{"dep removed", events.TopicDependencyRemoved, BeadEventDepRemove},
		{"label added", events.TopicLabelAdded, BeadEventLabelAdd},
		{"label removed", events.TopicLabelRemoved, BeadEventLabelRem},
		{"comment added", events.TopicCommentAdded, BeadEventComment},
		{"comment deleted", events.TopicCommentDeleted, BeadEventCommentRm},
		{"follower added", events.TopicFollowerAdded, BeadEventFollowAdd},
		{"follower removed", events.TopicFollowerRemoved, BeadEventFollowRem},
		{"advice created", events.TopicAdviceCreated, BeadEventAdviceNew},
		{"advice updated", events.TopicAdviceUpdated, BeadEventAdviceUpd},
		{"advice deleted", events.TopicAdviceDeleted, BeadEventAdviceDel},
		{"jack on", events.TopicJackOn, BeadEventJackOn},
		{"jack off", events.TopicJackOff, BeadEventJackOff},
		{"jack extended", events.TopicJackExtended, BeadEventJackExt},
		{"jack expired", events.TopicJackExpired, BeadEventJackExp},
		{"mail sent", events.TopicMailSent, BeadEventMailSent},
		{"session end", events.TopicSessionEnd, BeadEventSession},
		{"session pre_commit", events.TopicSessionPreCommit, BeadEventSession},
		{"session pre_push", events.TopicSessionPrePush, BeadEventSession},
		{"session handoff", events.TopicSessionHandoff, BeadEventSession},
		{"unknown topic", "beads.brand.new", BeadEventOther},
		// updated with no changes → generic updated
		{"updated nil changes", events.TopicBeadUpdated, BeadEventUpdated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProjectBeadEventLabel(tt.topic, nil); got != tt.want {
				t.Errorf("ProjectBeadEventLabel(%q, nil) = %q, want %q", tt.topic, got, tt.want)
			}
		})
	}
}

func TestProjectBeadEventLabelUpdateSubtype(t *testing.T) {
	tests := []struct {
		name    string
		changes map[string]any
		want    string
	}{
		{"claim", map[string]any{"assignee": "agent-x"}, BeadEventClaimed},
		{"release empty", map[string]any{"assignee": ""}, BeadEventReleased},
		{"release non-string", map[string]any{"assignee": nil}, BeadEventReleased},
		{"status", map[string]any{"status": "in_progress"}, BeadEventStatus},
		{"owner", map[string]any{"owner": "matt"}, BeadEventOwner},
		{"generic", map[string]any{"title": "new"}, BeadEventUpdated},
		{"nil changes", nil, BeadEventUpdated},
		// claim/release wins over a co-occurring status change.
		{"claim+status", map[string]any{"assignee": "agent-x", "status": "in_progress"}, BeadEventClaimed},
		// status wins over owner.
		{"status+owner", map[string]any{"status": "closed", "owner": "matt"}, BeadEventStatus},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProjectBeadEventLabel(events.TopicBeadUpdated, tt.changes); got != tt.want {
				t.Errorf("ProjectBeadEventLabel(updated, %v) = %q, want %q", tt.changes, got, tt.want)
			}
			// InferUpdateSubtype is the same path directly.
			if got := InferUpdateSubtype(tt.changes); got != tt.want {
				t.Errorf("InferUpdateSubtype(%v) = %q, want %q", tt.changes, got, tt.want)
			}
		})
	}
}
