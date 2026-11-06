// Tests for the stuck-agent heuristic (ka-0000001882). The heuristic flags
// pods that are Running+Ready, reporting a non-idle/non-error coop state, and
// whose since_seq/screen_seq do not advance for the configured threshold.
package statusreporter

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// recordingPublisher captures published StuckEvents for assertions.
type recordingPublisher struct {
	mu     sync.Mutex
	events []StuckEvent
}

func (p *recordingPublisher) PublishStuck(_ context.Context, ev StuckEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
}

func (p *recordingPublisher) snapshot() []StuckEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]StuckEvent, len(p.events))
	copy(out, p.events)
	return out
}

// stuckCoopServer serves a programmable response from /api/v1/agent and
// returns a Ready pod wired to reach it. body is mutable across requests.
func stuckCoopServer(t *testing.T) (*httptest.Server, *string, *corev1.Pod) {
	t.Helper()
	body := `{"state":"working","since_seq":1,"screen_seq":1}`
	bodyPtr := &body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(*bodyPtr))
	}))
	t.Cleanup(srv.Close)

	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split server URL %q: %v", srv.URL, err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crew-proj-dev-alpha",
			Namespace: "ns",
			Labels:    agentLabels("proj", "dev", "alpha"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent"},
				{
					Name:  "coop",
					Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: host,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	return srv, bodyPtr, pod
}

// newReporterWithStuck wires a reporter with the heuristic enabled and a
// mockable clock. Returns the reporter, the publisher, and a pointer to the
// virtual clock so tests can advance time deterministically.
func newReporterWithStuck(t *testing.T, threshold time.Duration, srv *httptest.Server, pod *corev1.Pod) (*HTTPReporter, *recordingPublisher, *time.Time) {
	t.Helper()
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	wireReporterToCoopServer(r, srv)

	pub := &recordingPublisher{}
	// Multiplier 1 disables the long-lived role bias so these base-threshold
	// tests are unaffected by role; the role bias has its own tests below.
	r.SetStuckDetection(pub, threshold, 1)

	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	return r, pub, &now
}

func TestSyncAll_StuckAgent_FirstObservationDoesNotPublish(t *testing.T) {
	srv, _, pod := stuckCoopServer(t)
	r, pub, _ := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("expected no stuck events on first observation, got %d: %+v", len(got), got)
	}
}

func TestSyncAll_StuckAgent_PublishesAfterThreshold(t *testing.T) {
	srv, _, pod := stuckCoopServer(t)
	r, pub, now := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	// First sync: seeds the tracker.
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Advance clock past threshold; sequences unchanged.
	*now = now.Add(6 * time.Minute)
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	got := pub.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 stuck event after threshold, got %d: %+v", len(got), got)
	}
	ev := got[0]
	if ev.BeadID != "crew-proj-dev-alpha" {
		t.Errorf("BeadID: got %q want crew-proj-dev-alpha", ev.BeadID)
	}
	if ev.Project != "proj" || ev.Role != "dev" || ev.AgentName != "alpha" || ev.Mode != "crew" {
		t.Errorf("identifiers: %+v", ev)
	}
	if ev.Reason == "" {
		t.Errorf("expected non-empty Reason")
	}
}

func TestSyncAll_StuckAgent_AdvancingSequenceResetsTracker(t *testing.T) {
	srv, body, pod := stuckCoopServer(t)
	r, pub, now := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Halfway through window, sequences advance.
	*now = now.Add(3 * time.Minute)
	*body = `{"state":"working","since_seq":2,"screen_seq":1}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	// Another 3m later (6m total since first sync) sequences hold steady.
	// This is < threshold from the *advance* at 3m, so no publish yet.
	*now = now.Add(3 * time.Minute)
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("third sync: %v", err)
	}

	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("expected no stuck events after sequence advance, got %d: %+v", len(got), got)
	}
}

func TestSyncAll_StuckAgent_IdleStateResetsTracker(t *testing.T) {
	srv, body, pod := stuckCoopServer(t)
	r, pub, now := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Coop transitions to idle; tracker should be cleared.
	*body = `{"state":"idle","since_seq":1,"screen_seq":1}`
	*now = now.Add(1 * time.Minute)
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("idle sync: %v", err)
	}
	// Coop returns to working; this counts as a fresh first-observation.
	*body = `{"state":"working","since_seq":1,"screen_seq":1}`
	*now = now.Add(10 * time.Minute) // well past threshold of original observation
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("recover sync: %v", err)
	}

	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("expected idle to reset tracker, got %d events: %+v", len(got), got)
	}
}

func TestSyncAll_StuckAgent_ErrorCategoryResetsTracker(t *testing.T) {
	srv, body, pod := stuckCoopServer(t)
	r, pub, now := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	*body = `{"state":"working","error_category":"rate_limited","since_seq":1,"screen_seq":1}`
	*now = now.Add(10 * time.Minute)
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("rate_limited sync: %v", err)
	}

	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("expected error_category to suppress stuck publish, got %d: %+v", len(got), got)
	}
}

func TestSyncAll_StuckAgent_NotReadyDoesNotPublish(t *testing.T) {
	srv, _, pod := stuckCoopServer(t)
	// Strip Ready condition.
	pod.Status.Conditions = nil
	r, pub, now := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	*now = now.Add(10 * time.Minute)
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("expected no stuck events for non-Ready pod, got %d: %+v", len(got), got)
	}
}

func TestSyncAll_StuckAgent_DisabledWhenThresholdZero(t *testing.T) {
	srv, _, pod := stuckCoopServer(t)
	r, pub, now := newReporterWithStuck(t, 0, srv, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	*now = now.Add(60 * time.Minute)
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("expected heuristic disabled when threshold=0, got %d: %+v", len(got), got)
	}
}

func TestSyncAll_StuckAgent_PodRemovalGCsTracker(t *testing.T) {
	srv, _, pod := stuckCoopServer(t)
	r, _, _ := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	r.stagnationMu.Lock()
	if _, ok := r.stagnation["crew-proj-dev-alpha"]; !ok {
		r.stagnationMu.Unlock()
		t.Fatalf("expected entry seeded after first sync")
	}
	r.stagnationMu.Unlock()

	// Delete the pod from the cluster — next SyncAll should GC the tracker.
	if err := r.client.CoreV1().Pods("ns").Delete(context.Background(), "crew-proj-dev-alpha", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("post-delete sync: %v", err)
	}

	r.stagnationMu.Lock()
	defer r.stagnationMu.Unlock()
	if _, ok := r.stagnation["crew-proj-dev-alpha"]; ok {
		t.Errorf("expected tracker entry GC'd after pod removal, still present")
	}
}

func TestSyncAll_StuckAgent_PublishClearsTracker(t *testing.T) {
	srv, _, pod := stuckCoopServer(t)
	r, pub, now := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	*now = now.Add(6 * time.Minute)
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(pub.snapshot()) != 1 {
		t.Fatalf("expected first publish after threshold")
	}

	// Tracker entry should have been reset by publishStuck. The next
	// observation should re-seed lastAdvanceAt to "now" and not double-publish.
	*now = now.Add(1 * time.Minute)
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if got := pub.snapshot(); len(got) != 1 {
		t.Fatalf("expected publishStuck to reset tracker, got %d events", len(got))
	}
}
