package reconciler

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestReconciler_DeferredMetrics(t *testing.T) {
	r := New(&mockLister{}, &mockManager{}, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	if r.DeferredMetrics() == nil {
		t.Error("DeferredMetrics should return non-nil metrics")
	}
}

func TestReconciler_MarkAgentFailed(t *testing.T) {
	t.Run("no marker is no-op", func(t *testing.T) {
		r := New(&mockLister{}, &mockManager{}, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
		// Must not panic when no FailedAgentMarker is wired up.
		r.markAgentFailed(context.Background(), beadsapi.AgentBead{ID: "bd-1"}, errors.New("boom"))
	})

	t.Run("marker called on failure", func(t *testing.T) {
		r := New(&mockLister{}, &mockManager{}, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
		m := &mockFailedMarker{}
		r.SetFailedAgentMarker(m)

		r.markAgentFailed(context.Background(), beadsapi.AgentBead{ID: "bd-1"}, errors.New("create failed"))
		if len(m.calls) != 1 || m.calls[0].beadID != "bd-1" || m.calls[0].reason != "create failed" {
			t.Errorf("unexpected calls: %+v", m.calls)
		}
	})

	t.Run("marker error is logged, not surfaced", func(t *testing.T) {
		r := New(&mockLister{}, &mockManager{}, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
		m := &mockFailedMarker{err: errors.New("daemon down")}
		r.SetFailedAgentMarker(m)
		// Should not panic or surface error.
		r.markAgentFailed(context.Background(), beadsapi.AgentBead{ID: "bd-1"}, errors.New("boom"))
		if len(m.calls) != 1 {
			t.Errorf("expected one call, got %d", len(m.calls))
		}
	})
}
