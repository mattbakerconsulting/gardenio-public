package server

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/model"
)

func TestBeadEventLabel(t *testing.T) {
	tests := []struct {
		name  string
		topic string
		event any
		want  string
	}{
		{"created", events.TopicBeadCreated, events.BeadCreated{}, metrics.BeadEventCreated},
		{"closed", events.TopicBeadClosed, events.BeadClosed{}, metrics.BeadEventClosed},
		{"deleted", events.TopicBeadDeleted, events.BeadDeleted{}, metrics.BeadEventDeleted},
		{"transferred", events.TopicBeadTransferred, events.BeadTransferred{}, metrics.BeadEventTransfer},
		{"dep added", events.TopicDependencyAdded, events.DependencyAdded{}, metrics.BeadEventDepAdd},
		{"dep removed", events.TopicDependencyRemoved, events.DependencyRemoved{}, metrics.BeadEventDepRemove},
		{"label added", events.TopicLabelAdded, events.LabelAdded{}, metrics.BeadEventLabelAdd},
		{"comment added", events.TopicCommentAdded, events.CommentAdded{}, metrics.BeadEventComment},
		{"comment deleted", events.TopicCommentDeleted, events.CommentDeleted{}, metrics.BeadEventCommentRm},
		{"follower added", events.TopicFollowerAdded, events.FollowerAdded{}, metrics.BeadEventFollowAdd},
		{"advice created", events.TopicAdviceCreated, events.AdviceCreated{}, metrics.BeadEventAdviceNew},
		{"jack on", events.TopicJackOn, events.JackOn{}, metrics.BeadEventJackOn},
		{"mail sent", events.TopicMailSent, events.MailSent{}, metrics.BeadEventMailSent},
		{"session", events.TopicSessionEnd, nil, metrics.BeadEventSession},
		{"unknown topic", "beads.brand.new", nil, metrics.BeadEventOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := beadEventLabel(tt.topic, tt.event)
			if !ok {
				t.Fatalf("beadEventLabel(%q) returned ok=false", tt.topic)
			}
			if got != tt.want {
				t.Errorf("beadEventLabel(%q) = %q, want %q", tt.topic, got, tt.want)
			}
		})
	}
}

func TestBeadEventLabelUpdateSubtype(t *testing.T) {
	// The update sub-type inference now lives in metrics.InferUpdateSubtype
	// (shared with the E4b backfill); the server's beadEventLabel wrapper
	// unwraps BeadUpdated.Changes and delegates. Exercise the live path here.
	tests := []struct {
		name    string
		changes map[string]any
		want    string
	}{
		{"claim", map[string]any{"assignee": "agent-x"}, metrics.BeadEventClaimed},
		{"release", map[string]any{"assignee": ""}, metrics.BeadEventReleased},
		{"status", map[string]any{"status": model.StatusInProgress}, metrics.BeadEventStatus},
		{"owner", map[string]any{"owner": "matt"}, metrics.BeadEventOwner},
		{"generic", map[string]any{"title": "new"}, metrics.BeadEventUpdated},
		{"nil changes", nil, metrics.BeadEventUpdated},
		// claim/release takes precedence over a co-occurring status change.
		{"claim+status", map[string]any{"assignee": "agent-x", "status": model.StatusInProgress}, metrics.BeadEventClaimed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := beadEventLabel(events.TopicBeadUpdated, events.BeadUpdated{Changes: tt.changes})
			if !ok {
				t.Fatal("beadEventLabel returned ok=false")
			}
			if got != tt.want {
				t.Errorf("beadEventLabel(updated, %v) = %q, want %q", tt.changes, got, tt.want)
			}
		})
	}
}

func TestBeadEventLabelNonBeadUpdated(t *testing.T) {
	// A non-BeadUpdated payload on the updated topic (defensive) carries no
	// Changes map, so the wrapper passes nil and the projection falls back to
	// generic updated.
	got, ok := beadEventLabel(events.TopicBeadUpdated, events.BeadCreated{})
	if !ok || got != metrics.BeadEventUpdated {
		t.Errorf("beadEventLabel(updated, non-update) = %q (ok=%v), want %q", got, ok, metrics.BeadEventUpdated)
	}
}

func TestBeadEventLabelsFromPayload(t *testing.T) {
	bead := &model.Bead{Type: model.TypeTask, Labels: []string{"project:gardenio", "area:obs"}}
	proj, bt := beadEventLabelsFromPayload(events.BeadCreated{Bead: bead})
	if proj != "gardenio" || bt != "task" {
		t.Errorf("labelsFromPayload(created) = (%q,%q), want (gardenio,task)", proj, bt)
	}

	// Bead-less payload yields empty labels (normalized downstream).
	proj, bt = beadEventLabelsFromPayload(events.DependencyAdded{})
	if proj != "" || bt != "" {
		t.Errorf("labelsFromPayload(dep) = (%q,%q), want empty", proj, bt)
	}
}

// TestOperatorChartLabel pins the cardinality-bounding behaviour of the
// per-operator created-counter label: humans keep their resolver id; every
// non-human shape collapses to a fixed sentinel so agent/automation traffic
// cannot inflate the series count.
func TestOperatorChartLabel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"slack:U0OPERATOR1", "slack:U0OPERATOR1"},
		{"oidc:matthew.baker@acme.example", "oidc:matthew.baker@acme.example"},
		{"", "unknown"},
		{"system:legacy-pre-attribution", "system"},
		{"fleet:gardenio", "fleet"},
		{"gardenio-e6-beadsviewer-chart-gxr", "agent"},
		{"thread-1780583611-035649", "agent"},
		{"static", "agent"},
		{"oidc:nodomain", "agent"}, // malformed human shape → agent bucket
	}
	for _, tt := range tests {
		if got := operatorChartLabel(tt.in); got != tt.want {
			t.Errorf("operatorChartLabel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestRecordBeadCreatedByOperator verifies the counter only fires for the
// created topic and buckets the operator through operatorChartLabel.
func TestRecordBeadCreatedByOperator(t *testing.T) {
	// The counter is a package global other tests in this package also touch
	// (every recordAndPublish on a created topic increments it). Reset before
	// and after so this test reads only its own increments.
	metrics.BeadsCreatedByOperator.Reset()
	t.Cleanup(func() { metrics.BeadsCreatedByOperator.Reset() })

	recordBeadCreatedByOperator(events.TopicBeadClosed, "slack:U1") // ignored: wrong topic
	recordBeadCreatedByOperator(events.TopicBeadCreated, "slack:U1")
	recordBeadCreatedByOperator(events.TopicBeadCreated, "gardenio-agent-x") // → agent

	if got := testutil.ToFloat64(metrics.BeadsCreatedByOperator.WithLabelValues("slack:U1")); got != 1 {
		t.Errorf("created/slack:U1 = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.BeadsCreatedByOperator.WithLabelValues("agent")); got != 1 {
		t.Errorf("created/agent = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.BeadsCreatedByOperator.WithLabelValues("system")); got != 0 {
		t.Errorf("created/system = %v, want 0 (closed topic must not count)", got)
	}
}
