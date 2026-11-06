package healthsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type fakePodLister struct {
	pods []corev1.Pod
	err  error
}

func (f *fakePodLister) ListAgentPods(_ context.Context, _ string, _ map[string]string) ([]corev1.Pod, error) {
	return f.pods, f.err
}

func TestSnapshot_Empty(t *testing.T) {
	snap := NewSnapshotter(NewTracker(), &fakePodLister{}, "agents")
	got := snap.Build(context.Background())

	if got.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
	// Pod tile: zero counts across all 5 phases.
	rows, ok := got.Tiles.PodCounts.Data.([]PodPhaseCount)
	if !ok || len(rows) != 5 {
		t.Fatalf("pod_counts data = %T %v, want []PodPhaseCount of len 5", got.Tiles.PodCounts.Data, got.Tiles.PodCounts.Data)
	}
	for _, r := range rows {
		if r.Count != 0 {
			t.Errorf("empty cluster but phase %s has count %d", r.Phase, r.Count)
		}
	}
	if total := got.Tiles.PodCounts.Meta["total"]; total != 0 {
		t.Errorf("pod_counts.meta.total = %v, want 0", total)
	}
	// Reap and rotation tiles: empty slices, not nil (so JSON is `[]` not `null`).
	if reaps, ok := got.Tiles.RecentReaps.Data.([]ReapEvent); !ok || len(reaps) != 0 {
		t.Errorf("recent_reaps data = %T %v, want empty []ReapEvent", got.Tiles.RecentReaps.Data, got.Tiles.RecentReaps.Data)
	}
	if rots, ok := got.Tiles.RecentRotations.Data.([]RotationEvent); !ok || len(rots) != 0 {
		t.Errorf("recent_rotations data = %T %v, want empty []RotationEvent", got.Tiles.RecentRotations.Data, got.Tiles.RecentRotations.Data)
	}
	if got.Tiles.LastReconcile.Data != nil {
		t.Errorf("last_reconcile data = %v, want nil before any reconcile", got.Tiles.LastReconcile.Data)
	}

	// Round-trip JSON to confirm empty slices marshal to `[]`.
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !contains(b, []byte(`"recent_reaps":{"data":[]`)) {
		t.Errorf("empty recent_reaps did not marshal as []: %s", b)
	}
}

func TestSnapshot_Populated(t *testing.T) {
	tr := NewTracker()
	tr.RecordReap(50, 2)
	tr.RecordRotation("rotated", "gardenio", "acct-a", "acct-b", "rate_limited")
	t0 := time.Now().Add(-time.Second)
	tr.RecordReconcile(t0, t0.Add(40*time.Millisecond), nil)

	pods := []corev1.Pod{
		{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		{Status: corev1.PodStatus{Phase: corev1.PodPending}},
	}
	snap := NewSnapshotter(tr, &fakePodLister{pods: pods}, "agents")
	got := snap.Build(context.Background())

	rows := got.Tiles.PodCounts.Data.([]PodPhaseCount)
	byPhase := map[string]int{}
	for _, r := range rows {
		byPhase[r.Phase] = r.Count
	}
	if byPhase["Running"] != 2 || byPhase["Pending"] != 1 {
		t.Errorf("pod counts wrong: %+v", byPhase)
	}
	if got.Tiles.PodCounts.Meta["total"] != 3 {
		t.Errorf("total meta = %v, want 3", got.Tiles.PodCounts.Meta["total"])
	}

	reaps := got.Tiles.RecentReaps.Data.([]ReapEvent)
	if len(reaps) != 1 || reaps[0].Reaped != 2 {
		t.Errorf("reaps wrong: %+v", reaps)
	}

	rots := got.Tiles.RecentRotations.Data.([]RotationEvent)
	if len(rots) != 1 || rots[0].Event != "rotated" || rots[0].FromAccount != "acct-a" {
		t.Errorf("rotations wrong: %+v", rots)
	}

	lr, ok := got.Tiles.LastReconcile.Data.(*ReconcileEvent)
	if !ok || lr == nil || lr.DurationMs != 40 {
		t.Errorf("last_reconcile wrong: %T %v", got.Tiles.LastReconcile.Data, got.Tiles.LastReconcile.Data)
	}
	if got.Tiles.LastReconcile.Meta["last_ok_at"] == nil {
		t.Errorf("expected last_ok_at meta after a successful reconcile")
	}
}

func TestSnapshot_PodListError(t *testing.T) {
	snap := NewSnapshotter(NewTracker(), &fakePodLister{err: errors.New("api server down")}, "agents")
	got := snap.Build(context.Background())
	if got.Tiles.PodCounts.Meta["error"] != "api server down" {
		t.Errorf("expected error in pod_counts meta, got %+v", got.Tiles.PodCounts.Meta)
	}
	// Other tiles still render.
	if got.Tiles.RecentReaps.Data == nil {
		t.Errorf("recent_reaps should still render when pod listing fails")
	}
}

func TestHandler_Caches(t *testing.T) {
	calls := 0
	pods := &countingPodLister{onCall: func() { calls++ }}
	snap := NewSnapshotter(NewTracker(), pods, "agents")
	h := NewHandler(snap, 100*time.Millisecond)

	for range 5 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz/snapshot", nil))
		if w.Code != 200 {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
	}
	if calls != 1 {
		t.Errorf("pod lister called %d times across 5 quick requests, want 1 (cached)", calls)
	}

	// Wait past TTL — next request should rebuild.
	time.Sleep(150 * time.Millisecond)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz/snapshot", nil))
	if calls != 2 {
		t.Errorf("after TTL expiry, pod lister called %d times, want 2", calls)
	}

	// Body is valid JSON with schema_version.
	var decoded map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["schema_version"] != float64(SchemaVersion) {
		t.Errorf("schema_version = %v, want %d", decoded["schema_version"], SchemaVersion)
	}
}

type countingPodLister struct {
	onCall func()
}

func (c *countingPodLister) ListAgentPods(_ context.Context, _ string, _ map[string]string) ([]corev1.Pod, error) {
	if c.onCall != nil {
		c.onCall()
	}
	return nil, nil
}

func contains(b, sub []byte) bool {
	for i := 0; i+len(sub) <= len(b); i++ {
		match := true
		for j := range sub {
			if b[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
