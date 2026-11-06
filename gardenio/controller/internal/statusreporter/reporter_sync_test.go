package statusreporter

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/podmanager"
)

// newCoopTestServer starts an httptest server that serves the given JSON body
// from /api/v1/agent, and returns a pod wired to reach it. The pod's PodIP is
// set to the server's host and the coop container is given the server's port;
// the reporter's coopClient is rewired via a transport that rewrites the
// destination host so any IP:8080 URL reaches the test server.
func newCoopTestServer(t *testing.T, body string) (*httptest.Server, *corev1.Pod) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
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
		},
	}
	return srv, pod
}

// rewriteTransport forces all requests to hit the given host:port regardless
// of the URL's original host — used to redirect the reporter's IP:8080 coop
// URL onto an httptest server bound to a random port.
type rewriteTransport struct {
	host string
	base http.RoundTripper
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Host = rt.host
	clone.Host = rt.host
	return rt.base.RoundTrip(clone)
}

func wireReporterToCoopServer(r *HTTPReporter, srv *httptest.Server) {
	host := strings.TrimPrefix(srv.URL, "http://")
	r.coopClient.Transport = &rewriteTransport{host: host, base: http.DefaultTransport}
}


// --- SyncAll helpers ---

func makePod(name, ns string, phase corev1.PodPhase, labels map[string]string, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent"},
			},
		},
		Status: corev1.PodStatus{
			Phase: phase,
			PodIP: ip,
		},
	}
}

func agentLabels(project, role, agent string) map[string]string {
	return map[string]string{
		podmanager.LabelApp:     podmanager.LabelAppValue,
		podmanager.LabelProject: project,
		podmanager.LabelRole:    role,
		podmanager.LabelAgent:   agent,
		podmanager.LabelMode:    "crew",
	}
}

// --- SyncAll tests ---

func TestSyncAll_ReportsStatusForAgentPods(t *testing.T) {
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update, got %d", len(daemon.stateCalls))
	}
	if daemon.stateCalls[0].state != "working" {
		t.Errorf("expected state working, got %s", daemon.stateCalls[0].state)
	}
}

// TestSyncAll_SkipsClosedBeads verifies SyncAll early-exits per pod when the
// pod's bead is already closed, avoiding wasted PATCH RPCs (and the resulting
// done→working flip in the activity feed) during the K8s pod grace period
// after `gb stop`. Regression guard for ka-0000005253.
func TestSyncAll_SkipsClosedBeads(t *testing.T) {
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	// Add coop container so SyncAll would also try to write backend metadata —
	// both PATCH paths must be suppressed.
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:  "coop",
		Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
	})
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{
		statusByID: map[string]string{"crew-proj-dev-alpha": "closed"},
	}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 0 {
		t.Errorf("expected 0 state updates for closed bead, got %d (%+v)",
			len(daemon.stateCalls), daemon.stateCalls)
	}
	if len(daemon.notesCalls) != 0 {
		t.Errorf("expected 0 notes updates for closed bead, got %d (%+v)",
			len(daemon.notesCalls), daemon.notesCalls)
	}
}

func TestSyncAll_SkipsPodsMissingLabels(t *testing.T) {
	// Pod has gardenio app label but missing agent/project/role labels.
	pod := makePod("gardenio-controller-xyz", "ns", corev1.PodRunning,
		map[string]string{
			podmanager.LabelApp: podmanager.LabelAppValue,
		}, "10.0.0.1")
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.stateCalls) != 0 {
		t.Errorf("expected 0 state updates for pod without agent labels, got %d", len(daemon.stateCalls))
	}
}

func TestSyncAll_ReportsBackendMetadataForCoopPods(t *testing.T) {
	labels := agentLabels("proj", "dev", "alpha")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crew-proj-dev-alpha",
			Namespace: "ns",
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "coop",
					Ports: []corev1.ContainerPort{
						{ContainerPort: 8080},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.5",
		},
	}
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.notesCalls) != 1 {
		t.Fatalf("expected 1 notes update for coop pod, got %d", len(daemon.notesCalls))
	}
	notes := daemon.notesCalls[0].notes
	if !strings.Contains(notes, "backend: coop") {
		t.Errorf("expected backend: coop, got: %s", notes)
	}
	if strings.Contains(notes, "coop_url") {
		t.Errorf("notes must not publish coop_url, got: %s", notes)
	}
}

func TestSyncAll_NoMetadataForNonCoopPods(t *testing.T) {
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.notesCalls) != 0 {
		t.Errorf("expected 0 notes updates for non-coop pod, got %d", len(daemon.notesCalls))
	}
}

func TestSyncAll_NoMetadataForCoopPodWithoutIP(t *testing.T) {
	labels := agentLabels("proj", "dev", "alpha")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crew-proj-dev-alpha",
			Namespace: "ns",
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "coop",
					Ports: []corev1.ContainerPort{
						{ContainerPort: 8080},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			PodIP: "", // no IP yet
		},
	}
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.notesCalls) != 0 {
		t.Errorf("expected 0 notes updates for coop pod without IP, got %d", len(daemon.notesCalls))
	}
}

func TestSyncAll_MultiplePods(t *testing.T) {
	pod1 := makePod("crew-proj-dev-alpha", "ns", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	pod2 := makePod("crew-proj-dev-beta", "ns", corev1.PodPending,
		agentLabels("proj", "dev", "beta"), "")
	client := fake.NewSimpleClientset(pod1, pod2)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.stateCalls) != 2 {
		t.Fatalf("expected 2 state updates, got %d", len(daemon.stateCalls))
	}

	// Verify we see both working and spawning states.
	states := map[string]bool{}
	for _, c := range daemon.stateCalls {
		states[c.state] = true
	}
	if !states["working"] || !states["spawning"] {
		t.Errorf("expected working and spawning states, got calls: %+v", daemon.stateCalls)
	}
}

func TestSyncAll_NoPods(t *testing.T) {
	client := fake.NewSimpleClientset()
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.stateCalls) != 0 {
		t.Errorf("expected 0 state updates for no pods, got %d", len(daemon.stateCalls))
	}
}

func TestSyncAll_StatusReportError_ContinuesProcessing(t *testing.T) {
	// Even if one pod's status report fails, SyncAll should continue with others.
	pod1 := makePod("crew-proj-dev-alpha", "ns", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	pod2 := makePod("crew-proj-dev-beta", "ns", corev1.PodRunning,
		agentLabels("proj", "dev", "beta"), "10.0.0.2")

	callCount := 0
	daemon := &mockBeadUpdater{}
	// Override to fail on first call only.
	origErr := errors.New("transient")
	daemon.stateErr = origErr

	client := fake.NewSimpleClientset(pod1, pod2)
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.SyncAll(context.Background())
	// SyncAll does not return errors for individual pod failures.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = callCount

	// Both pods should have been attempted.
	if len(daemon.stateCalls) != 2 {
		t.Errorf("expected 2 state update attempts, got %d", len(daemon.stateCalls))
	}
}

func TestSyncAll_MetricsTracking(t *testing.T) {
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	_ = r.SyncAll(context.Background())

	m := r.Metrics()
	if m.SyncAllRuns != 1 {
		t.Errorf("expected 1 SyncAllRuns, got %d", m.SyncAllRuns)
	}
	if m.SyncAllErrors != 0 {
		t.Errorf("expected 0 SyncAllErrors, got %d", m.SyncAllErrors)
	}
	if m.StatusReportsTotal != 1 {
		t.Errorf("expected 1 StatusReportsTotal, got %d", m.StatusReportsTotal)
	}
}

func TestSyncAll_UsesBeadIDFromAnnotation(t *testing.T) {
	labels := agentLabels("proj", "dev", "alpha")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crew-proj-dev-alpha",
			Namespace: "ns",
			Labels:    labels,
			Annotations: map[string]string{
				podmanager.AnnotationBeadID: "gd-custom-bead",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.1",
		},
	}
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	_ = r.SyncAll(context.Background())

	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state call, got %d", len(daemon.stateCalls))
	}
	if daemon.stateCalls[0].beadID != "gd-custom-bead" {
		t.Errorf("expected bead ID from annotation, got %s", daemon.stateCalls[0].beadID)
	}
}

// --- SyncAll label selector test ---

func TestSyncAll_OnlyMatchesGardenioPods(t *testing.T) {
	// A pod in the same namespace but without the gardenio app label should not be listed.
	gardenioPod := makePod("crew-proj-dev-alpha", "ns", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	otherPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-other-pod",
			Namespace: "ns",
			Labels: map[string]string{
				"app": "something-else",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewSimpleClientset(gardenioPod, otherPod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the gardenio pod should trigger a state update.
	if len(daemon.stateCalls) != 1 {
		t.Errorf("expected 1 state update (only gardenio pod), got %d", len(daemon.stateCalls))
	}
}

// --- Integration-style: SyncAll with coop metadata ---

func TestSyncAll_CoopMetadataUsesCorrectURL(t *testing.T) {
	labels := agentLabels("gardenio", "crew", "furiosa")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crew-gardenio-crew-furiosa",
			Namespace: "ns",
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent"},
				{
					Name: "coop",
					Ports: []corev1.ContainerPort{
						{ContainerPort: 8080},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "172.16.0.42",
		},
	}
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	_ = r.SyncAll(context.Background())

	if len(daemon.notesCalls) != 1 {
		t.Fatalf("expected 1 notes call, got %d", len(daemon.notesCalls))
	}

	if !strings.Contains(daemon.notesCalls[0].notes, "backend: coop") {
		t.Errorf("expected notes to contain backend: coop, got: %s", daemon.notesCalls[0].notes)
	}

	// beadID should be constructed from labels since no annotation.
	if daemon.notesCalls[0].beadID != "crew-gardenio-crew-furiosa" {
		t.Errorf("expected beadID crew-gardenio-crew-furiosa, got %s", daemon.notesCalls[0].beadID)
	}
}

// --- Coop idle: skip updated_at bumps so stale reaper fires (ka-0000003615) ---

func TestSyncAll_SkipsStateBumpWhenCoopIdle(t *testing.T) {
	// A running pod whose coop reports state=idle, and whose bead has ALREADY
	// reached "working" (the genuine parked case — finished work, never called
	// `gb stop`), should NOT have its bead agent_state re-asserted; otherwise
	// updated_at keeps ticking and the stale reaper's wall-clock check never
	// fires (ka-0000003615).
	srv, pod := newCoopTestServer(t, `{"state":"idle"}`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{stateByID: map[string]string{agentBeadID(pod): "working"}}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.stateCalls) != 0 {
		t.Errorf("expected 0 state updates for idle coop pod, got %d: %+v",
			len(daemon.stateCalls), daemon.stateCalls)
	}
	// Backend metadata should still be written so ResolveBackend continues to
	// work after controller restarts.
	if len(daemon.notesCalls) != 1 {
		t.Errorf("expected 1 notes update (backend metadata), got %d", len(daemon.notesCalls))
	}
}

// Regression guard for ka-0000006338: an idle agent with non-zero
// context/cost must NOT get live-usage fields written — the resulting
// updated_at bump defeats the stale reaper's wall-clock check.
func TestSyncAll_SkipsLiveUsageWhenCoopIdle(t *testing.T) {
	srv, pod := newCoopTestServer(t, `{"state":"idle","context_tokens":90000,"cost_usd":1.5}`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{stateByID: map[string]string{agentBeadID(pod): "working"}}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, fc := range daemon.fieldCalls {
		if _, ok := fc.fields["context_tokens"]; ok {
			t.Errorf("live-usage field context_tokens written for idle agent — defeats stale reaper")
		}
		if _, ok := fc.fields["live_cost_usd"]; ok {
			t.Errorf("live-usage field live_cost_usd written for idle agent — defeats stale reaper")
		}
	}
}

// TestSyncAll_CoopIdleDuringStartupAllowsWorking is the regression guard for
// ka-0000000427: a freshly-Running pod whose coop briefly reports state=idle
// (startup window, before Claude's first hook) while the bead is still at
// "spawning" MUST still get the one-time spawning→working write — otherwise
// the bead is trapped at "spawning" and every consumer (Slack card, nudge
// gating, reaper, beads-viewer) shows "starting" for an actively-running agent.
func TestSyncAll_CoopIdleDuringStartupAllowsWorking(t *testing.T) {
	srv, pod := newCoopTestServer(t, `{"state":"idle"}`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{stateByID: map[string]string{agentBeadID(pod): "spawning"}}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update (spawning→working) for startup-idle pod, got %d: %+v",
			len(daemon.stateCalls), daemon.stateCalls)
	}
	if daemon.stateCalls[0].state != "working" {
		t.Errorf("expected state working, got %s", daemon.stateCalls[0].state)
	}
}

// TestSyncAll_CoopIdleWithEmptyBeadStateAllowsWorking covers the fail-safe:
// when the bead fetch returns an empty state, treat it as a startup state and
// write working rather than risk leaving a card stuck. ka-0000000427.
func TestSyncAll_CoopIdleWithEmptyBeadStateAllowsWorking(t *testing.T) {
	srv, pod := newCoopTestServer(t, `{"state":"idle"}`)
	client := fake.NewSimpleClientset(pod)
	// stateByID nil → GetAgentState returns "" (e.g. fetch failed / field unset).
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update for empty-bead-state idle pod, got %d: %+v",
			len(daemon.stateCalls), daemon.stateCalls)
	}
	if daemon.stateCalls[0].state != "working" {
		t.Errorf("expected state working, got %s", daemon.stateCalls[0].state)
	}
}

func TestSyncAll_PropagatesCoopErrorStateEvenWhenIdle(t *testing.T) {
	// If coop reports an error category (rate_limited / budget_exceeded),
	// propagate that state even if coop's plain state field is "idle" —
	// the error takes precedence over the idle-skip optimization.
	srv, pod := newCoopTestServer(t, `{"state":"idle","error_category":"rate_limited"}`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update (rate_limited override), got %d", len(daemon.stateCalls))
	}
	if daemon.stateCalls[0].state != "rate_limited" {
		t.Errorf("expected state rate_limited, got %s", daemon.stateCalls[0].state)
	}
}

func TestSyncAll_BumpsStateWhenCoopBusy(t *testing.T) {
	// When coop reports a non-idle state and no error, SyncAll should fall
	// through to normal pod-phase reporting (state=working for PodRunning).
	srv, pod := newCoopTestServer(t, `{"state":"working"}`)
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	wireReporterToCoopServer(r, srv)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update for non-idle coop pod, got %d", len(daemon.stateCalls))
	}
	if daemon.stateCalls[0].state != "working" {
		t.Errorf("expected state working, got %s", daemon.stateCalls[0].state)
	}
}

// TestSyncAll_WritesPodSchedulingBlockedTrue verifies SyncAll calls
// UpdateBeadFields with pod_scheduling_blocked=true when the Pending pod has
// PodScheduled=Unschedulable. The bridge reads this field to render
// "stuck: cluster capacity" instead of the spawning spinner.
func TestSyncAll_WritesPodSchedulingBlockedTrue(t *testing.T) {
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodPending,
		agentLabels("proj", "dev", "alpha"), "")
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable},
	}
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.fieldCalls) != 1 {
		t.Fatalf("expected 1 field update, got %d", len(daemon.fieldCalls))
	}
	if got := daemon.fieldCalls[0].fields["pod_scheduling_blocked"]; got != "true" {
		t.Errorf("expected pod_scheduling_blocked=true, got %q", got)
	}
}

// TestSyncAll_WritesPodSchedulingBlockedFalseWhilePending verifies the field
// is set to "false" while the pod is still Pending but scheduling is fine
// (e.g., ContainerCreating). This clears any stale "true" written earlier in
// the same Pending lifecycle if the pod eventually got scheduled.
func TestSyncAll_WritesPodSchedulingBlockedFalseWhilePending(t *testing.T) {
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodPending,
		agentLabels("proj", "dev", "alpha"), "")
	// PodScheduled=True means the pod has been scheduled to a node and is
	// just waiting on container init.
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
	}
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.fieldCalls) != 1 {
		t.Fatalf("expected 1 field update, got %d", len(daemon.fieldCalls))
	}
	if got := daemon.fieldCalls[0].fields["pod_scheduling_blocked"]; got != "false" {
		t.Errorf("expected pod_scheduling_blocked=false, got %q", got)
	}
}

// TestSyncAll_NoPodSchedulingBlockedWriteForRunningPods verifies SyncAll does
// not bother writing the field once the pod is Running — the value can no
// longer be true, and skipping the write avoids one bead GET per Running pod
// per sync pass.
func TestSyncAll_NoPodSchedulingBlockedWriteForRunningPods(t *testing.T) {
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.fieldCalls) != 0 {
		t.Errorf("expected no pod_scheduling_blocked writes for Running pod, got %d (%+v)",
			len(daemon.fieldCalls), daemon.fieldCalls)
	}
}
