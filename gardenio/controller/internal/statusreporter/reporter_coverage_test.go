package statusreporter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/podmanager"
)

// TestSyncAll_PodListError verifies that SyncAll surfaces an error and bumps
// the SyncAllErrors metric when the K8s client fails to list pods.
func TestSyncAll_PodListError(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "pods",
		func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, nil, errors.New("apiserver unavailable")
		})
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.SyncAll(context.Background())
	if err == nil {
		t.Fatal("expected error from list failure")
	}
	if !strings.Contains(err.Error(), "listing agent pods") {
		t.Errorf("expected wrapped list error, got: %v", err)
	}
	m := r.Metrics()
	if m.SyncAllErrors != 1 {
		t.Errorf("expected SyncAllErrors=1, got %d", m.SyncAllErrors)
	}
	if m.SyncAllRuns != 1 {
		t.Errorf("expected SyncAllRuns=1, got %d", m.SyncAllRuns)
	}
}

// TestBeadIsClosed_StatusErrorTreatedAsOpen verifies that a daemon error when
// fetching status is treated as "not closed" so transient daemon failures do
// not silently drop reporter writes (the server-side filter in kbeads is the
// safety net).
func TestBeadIsClosed_StatusErrorTreatedAsOpen(t *testing.T) {
	daemon := &mockBeadUpdater{statusErr: errors.New("daemon timeout")}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportPodStatus(context.Background(), "agent-1", PodStatus{
		PodName: "pod-1", Phase: "Running",
	}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// State update should still happen because GetBeadStatus error is treated
	// as not-closed.
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update when GetBeadStatus errors, got %d", len(daemon.stateCalls))
	}
}

// --- queryCoopAgentState branch coverage ---

func TestSyncAll_CoopReturns404_FallsThroughToPodPhase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	t.Cleanup(srv.Close)

	_, pod := newCoopTestServer(t, `{"state":"working"}`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	// Rewire to the 404 server.
	host := strings.TrimPrefix(srv.URL, "http://")
	r.coopClient.Transport = &rewriteTransport{host: host, base: http.DefaultTransport}

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty coopAgentInfo → falls through to normal pod-phase reporting.
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update (pod-phase fallback), got %d", len(daemon.stateCalls))
	}
	if daemon.stateCalls[0].state != "working" {
		t.Errorf("expected state working, got %s", daemon.stateCalls[0].state)
	}
}

func TestSyncAll_CoopReturnsMalformedJSON_FallsThroughToPodPhase(t *testing.T) {
	srv, pod := newCoopTestServer(t, `not-valid-json{{`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Malformed JSON → empty coopAgentInfo → normal pod-phase reporting.
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update (pod-phase fallback), got %d", len(daemon.stateCalls))
	}
	if daemon.stateCalls[0].state != "working" {
		t.Errorf("expected state working, got %s", daemon.stateCalls[0].state)
	}
}

func TestSyncAll_CoopBudgetExceeded_PropagatesState(t *testing.T) {
	srv, pod := newCoopTestServer(t, `{"state":"working","error_category":"budget_exceeded"}`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update for budget_exceeded override, got %d", len(daemon.stateCalls))
	}
	if daemon.stateCalls[0].state != "budget_exceeded" {
		t.Errorf("expected state budget_exceeded, got %s", daemon.stateCalls[0].state)
	}
	// Backend metadata still written.
	if len(daemon.notesCalls) != 1 {
		t.Errorf("expected 1 notes update (backend metadata), got %d", len(daemon.notesCalls))
	}
}

// TestSyncAll_CoopErrorStatePropagationFailureMetric verifies that a daemon
// PATCH failure during coop error-state propagation increments reportErrors
// and does not crash SyncAll.
func TestSyncAll_CoopErrorStatePropagationFailureMetric(t *testing.T) {
	srv, pod := newCoopTestServer(t, `{"state":"working","error_category":"rate_limited"}`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{stateErr: errors.New("daemon down")}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 attempted state update, got %d", len(daemon.stateCalls))
	}
	m := r.Metrics()
	if m.StatusReportErrors != 1 {
		t.Errorf("expected StatusReportErrors=1 after propagation failure, got %d", m.StatusReportErrors)
	}
}

// TestSyncAll_NonRunningCoopPod_SkipsCoopQuery verifies that the coop /agent
// query is NOT made for pods in non-Running phases — they fall straight to
// pod-phase reporting.
func TestSyncAll_NonRunningCoopPod_SkipsCoopQuery(t *testing.T) {
	// Use a transport that fails loudly if any HTTP call is made.
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodPending,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:  "coop",
		Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
	})
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.coopClient.Transport = &failingTransport{t: t}

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update for Pending pod, got %d", len(daemon.stateCalls))
	}
	if daemon.stateCalls[0].state != "spawning" {
		t.Errorf("expected state spawning, got %s", daemon.stateCalls[0].state)
	}
}

type failingTransport struct {
	t *testing.T
}

func (ft *failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ft.t.Errorf("unexpected HTTP call to %s", req.URL.String())
	return nil, errors.New("no HTTP allowed")
}

// TestSyncAll_PrewarmedCoopPodSkipsCoopQuery verifies that prewarmed pods skip
// the coop /agent query entirely (their state is not flipped to working until
// assignment).
func TestSyncAll_PrewarmedCoopPodSkipsCoopQuery(t *testing.T) {
	labels := agentLabels("proj", "dev", "alpha")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crew-proj-dev-alpha",
			Namespace: "ns",
			Labels:    labels,
			Annotations: map[string]string{
				podmanager.AnnotationPrewarmed: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent"},
				{Name: "coop", Ports: []corev1.ContainerPort{{ContainerPort: 8080}}},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
	}
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.coopClient.Transport = &failingTransport{t: t}

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Prewarmed Running pods skip the working state update entirely.
	if len(daemon.stateCalls) != 0 {
		t.Errorf("expected 0 state updates for prewarmed pod, got %d (%+v)",
			len(daemon.stateCalls), daemon.stateCalls)
	}
	// Backend metadata IS still written so ResolveBackend works.
	if len(daemon.notesCalls) != 1 {
		t.Errorf("expected 1 backend metadata write, got %d", len(daemon.notesCalls))
	}
}

// TestSyncAll_StalePrewarmedAnnotationReportsNormally verifies the ka-0000004312
// fix: a pod still carrying gardenio.io/prewarmed (never cleared on assignment)
// whose bead agent_state has already moved off "prewarmed" is treated as a
// regular assigned pod, so its Running phase IS reported as working.
func TestSyncAll_StalePrewarmedAnnotationReportsNormally(t *testing.T) {
	labels := agentLabels("proj", "dev", "alpha")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crew-proj-dev-alpha",
			Namespace: "ns",
			Labels:    labels,
			Annotations: map[string]string{
				podmanager.AnnotationPrewarmed: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ""},
	}
	client := fake.NewSimpleClientset(pod)
	// Bead was assigned off the pool — agent_state moved to "assigning",
	// but the pod annotation was never cleared.
	daemon := &mockBeadUpdater{
		stateByID: map[string]string{"crew-proj-dev-alpha": "assigning"},
	}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 1 || daemon.stateCalls[0].state != "working" {
		t.Errorf("expected 1 working state update for stale-prewarmed pod, got %+v",
			daemon.stateCalls)
	}
}

// TestSyncAll_GenuinelyPrewarmedAnnotationStillSuppressed verifies the converse:
// when the bead agent_state is still "prewarmed", the annotation is honored and
// no working state is written (the pod really is idle in the pool).
func TestSyncAll_GenuinelyPrewarmedAnnotationStillSuppressed(t *testing.T) {
	labels := agentLabels("proj", "dev", "alpha")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crew-proj-dev-alpha",
			Namespace: "ns",
			Labels:    labels,
			Annotations: map[string]string{
				podmanager.AnnotationPrewarmed: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ""},
	}
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{
		stateByID: map[string]string{"crew-proj-dev-alpha": "prewarmed"},
	}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 0 {
		t.Errorf("expected 0 state updates for genuinely-prewarmed pod, got %+v",
			daemon.stateCalls)
	}
}
