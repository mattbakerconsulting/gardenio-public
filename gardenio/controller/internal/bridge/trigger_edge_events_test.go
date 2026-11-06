package bridge

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"testing"
)

func TestParseEdgeEventBeadIDs(t *testing.T) {
	tests := []struct {
		name    string
		topic   string
		payload string
		want    []string
	}{
		{
			name:    "dependency.added wrapped",
			topic:   "beads.dependency.added",
			payload: `{"dependency":{"bead_id":"gd-A","depends_on_id":"gd-B","type":"parent-child"}}`,
			want:    []string{"gd-A", "gd-B"},
		},
		{
			name:    "dependency.added self-edge dedupes",
			topic:   "beads.dependency.added",
			payload: `{"dependency":{"bead_id":"gd-A","depends_on_id":"gd-A"}}`,
			want:    []string{"gd-A"},
		},
		{
			name:    "dependency.removed flat shape",
			topic:   "beads.dependency.removed",
			payload: `{"bead_id":"gd-X","depends_on_id":"gd-Y","type":"blocks"}`,
			want:    []string{"gd-X", "gd-Y"},
		},
		{
			name:    "label.added",
			topic:   "beads.label.added",
			payload: `{"bead_id":"gd-Q","label":"project:gardenio"}`,
			want:    []string{"gd-Q"},
		},
		{
			name:    "label.removed",
			topic:   "beads.label.removed",
			payload: `{"bead_id":"gd-Q","label":"old"}`,
			want:    []string{"gd-Q"},
		},
		{
			name:    "comment.added wrapped",
			topic:   "beads.comment.added",
			payload: `{"comment":{"id":42,"bead_id":"gd-C","author":"alice","body":"hi"}}`,
			want:    []string{"gd-C"},
		},
		{
			name:    "comment.deleted flat",
			topic:   "beads.comment.deleted",
			payload: `{"bead_id":"gd-D","comment_id":7,"author":"bob","reason":"delete"}`,
			want:    []string{"gd-D"},
		},
		{
			name:    "follower.added",
			topic:   "beads.follower.added",
			payload: `{"bead_id":"gd-F","operator_id":17,"note":"ping"}`,
			want:    []string{"gd-F"},
		},
		{
			name:    "follower.removed",
			topic:   "beads.follower.removed",
			payload: `{"bead_id":"gd-F","operator_id":17}`,
			want:    []string{"gd-F"},
		},
		{
			name:    "bead.deleted",
			topic:   "beads.bead.deleted",
			payload: `{"bead_id":"gd-G","fleet":"prod"}`,
			want:    []string{"gd-G"},
		},
		{
			name:    "bead.transferred uses ParseBeadEvent wrapper",
			topic:   "beads.bead.transferred",
			payload: `{"bead":{"id":"gd-H","type":"task","title":"t","status":"open","fields":{}},"from_fleet":"a","to_fleet":"b"}`,
			want:    []string{"gd-H"},
		},
		{
			name:    "unknown topic returns nil",
			topic:   "beads.unrelated.x",
			payload: `{"bead_id":"gd-Z"}`,
			want:    nil,
		},
		{
			name:    "malformed payload returns nil",
			topic:   "beads.label.added",
			payload: `not json`,
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseEdgeEventBeadIDs(tc.topic, []byte(tc.payload))
			gotSorted := append([]string(nil), got...)
			wantSorted := append([]string(nil), tc.want...)
			sort.Strings(gotSorted)
			sort.Strings(wantSorted)
			if len(gotSorted) != len(wantSorted) {
				t.Fatalf("ParseEdgeEventBeadIDs(%s) = %v, want %v", tc.topic, got, tc.want)
			}
			for i := range gotSorted {
				if gotSorted[i] != wantSorted[i] {
					t.Fatalf("ParseEdgeEventBeadIDs(%s) = %v, want %v", tc.topic, got, tc.want)
				}
			}
		})
	}
}

// TestBeadStateTriggerSource_EdgeTopics_PendingBuffer verifies that edge-shaped
// SSE events get pushed into the source's pending buffer with id-only BeadEvent
// stubs (the affected bead's full state isn't available in the payload).
func TestBeadStateTriggerSource_EdgeTopics_PendingBuffer(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	// Simulate dependency.added directly via the same internal append path the
	// SSE handler uses. (The handler closure captures `t` from the topics
	// slice — exercising it requires a full SSEStream; the unit test instead
	// targets the parse + push semantics, which is what we changed.)
	ids := ParseEdgeEventBeadIDs("beads.dependency.added",
		[]byte(`{"dependency":{"bead_id":"gd-A","depends_on_id":"gd-B"}}`))
	if len(ids) != 2 {
		t.Fatalf("ParseEdgeEventBeadIDs returned %d ids, want 2", len(ids))
	}
	src.mu.Lock()
	for _, id := range ids {
		src.pending = append(src.pending, beadStateEvent{
			Topic: "beads.dependency.added",
			Bead:  &BeadEvent{ID: id},
		})
	}
	src.mu.Unlock()

	if src.PendingCount() != 2 {
		t.Errorf("PendingCount() = %d, want 2", src.PendingCount())
	}

	// Build a trigger that filters on event_types=dependency.added and id=gd-A.
	trig := TriggerDef{
		ID:     "edge-trigger",
		Source: "bead",
		SourceData: beadTriggerData{
			EventTypes: []string{"dependency.added"},
			Conditions: []beadCondition{
				{Field: "id", Op: "==", Value: "gd-A"},
			},
		},
	}
	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (gd-A), got %d: %+v", len(matches), matches)
	}
	if matches[0].Fields["source_bead_id"] != "gd-A" {
		t.Errorf("source_bead_id = %q, want gd-A", matches[0].Fields["source_bead_id"])
	}
	if matches[0].Fields["source_event"] != "dependency.added" {
		t.Errorf("source_event = %q, want dependency.added", matches[0].Fields["source_event"])
	}
}

type recordingInvalidator struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingInvalidator) InvalidateParents(ids ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, ids...)
}

// TestInvalidateParentsOnEdgeEvents: a dependency add/remove drops both
// endpoints' cached parents and a deletion drops the bead's; other edge
// events leave the cache alone (gd-ZaztjcPrDc).
func TestInvalidateParentsOnEdgeEvents(t *testing.T) {
	stream := NewSSEStream(SSEStreamConfig{BeadsHTTPAddr: "http://x", Logger: slog.Default()})
	inv := &recordingInvalidator{}
	InvalidateParentsOnEdgeEvents(stream, inv)
	ctx := context.Background()

	stream.dispatch(ctx, "1", "beads.dependency.added", `{"dependency":{"bead_id":"gd-child","depends_on_id":"gd-epic","type":"parent-child"}}`)
	stream.dispatch(ctx, "2", "beads.dependency.removed", `{"bead_id":"gd-child2","depends_on_id":"gd-epic2"}`)
	stream.dispatch(ctx, "3", "beads.bead.deleted", `{"bead_id":"gd-gone"}`)
	stream.dispatch(ctx, "4", "beads.label.added", `{"bead_id":"gd-labelled"}`)

	want := []string{"gd-child", "gd-epic", "gd-child2", "gd-epic2", "gd-gone"}
	if !slices.Equal(inv.ids, want) {
		t.Fatalf("invalidated %v, want %v", inv.ids, want)
	}
}
