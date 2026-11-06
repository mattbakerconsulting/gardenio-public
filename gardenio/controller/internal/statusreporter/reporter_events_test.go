package statusreporter

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeEventRecorder captures the metric calls SyncAll makes so tests can assert
// the E2.4/E2.5/GAP-1 emission behaviour without a real Prometheus registry.
type fakeEventRecorder struct {
	phaseTransitions []phaseCall
	lifecycleEvents  []lifecycleCall
	// pendingSnapshots records every SetPodsPending call in order. Each entry is
	// a copy of the map SyncAll passed, so a test can assert the exact per-sweep
	// snapshot (including the empty-map "cleared" case).
	pendingSnapshots []map[[2]string]int
	// upstreamSnapshots records every SetUpstreamErrorActive call in order, same
	// copy-per-call shape as pendingSnapshots so a test can assert the exact
	// per-sweep gauge snapshot including the cleared/empty case.
	upstreamSnapshots []map[[3]string]int
	// upstreamTransitions records every edge-triggered transition call in order.
	upstreamTransitions []upstreamCall
	// landObserved records every ObserveLandSpawnToWorking call in order.
	landObserved []landObservation
}

type phaseCall struct{ from, to, project, role string }
type lifecycleCall struct{ event, project, role, sessionID string }
type upstreamCall struct{ category, project, role string }

func (f *fakeEventRecorder) RecordPodPhaseTransition(from, to, project, role string) bool {
	if from == to || from == "" || to == "" {
		return false
	}
	f.phaseTransitions = append(f.phaseTransitions, phaseCall{from, to, project, role})
	return true
}

func (f *fakeEventRecorder) RecordLifecycleEvent(event, project, role, sessionID string) bool {
	f.lifecycleEvents = append(f.lifecycleEvents, lifecycleCall{event, project, role, sessionID})
	return true
}

func (f *fakeEventRecorder) SetPodsPending(counts map[[2]string]int) {
	copied := make(map[[2]string]int, len(counts))
	for k, v := range counts {
		copied[k] = v
	}
	f.pendingSnapshots = append(f.pendingSnapshots, copied)
}

// lastPending returns the most recent SetPodsPending snapshot, or nil if none.
func (f *fakeEventRecorder) lastPending() map[[2]string]int {
	if len(f.pendingSnapshots) == 0 {
		return nil
	}
	return f.pendingSnapshots[len(f.pendingSnapshots)-1]
}

func (f *fakeEventRecorder) SetUpstreamErrorActive(counts map[[3]string]int) {
	copied := make(map[[3]string]int, len(counts))
	for k, v := range counts {
		copied[k] = v
	}
	f.upstreamSnapshots = append(f.upstreamSnapshots, copied)
}

// Mirrors the real emitter's contract: an empty category is dropped and
// increments nothing, so a test asserting on upstreamTransitions sees only
// genuine entries.
func (f *fakeEventRecorder) RecordUpstreamErrorTransition(category, project, role string) bool {
	if category == "" {
		return false
	}
	f.upstreamTransitions = append(f.upstreamTransitions, upstreamCall{category, project, role})
	return true
}

func (f *fakeEventRecorder) ObserveLandSpawnToWorking(land string, seconds float64) bool {
	f.landObserved = append(f.landObserved, landObservation{land, seconds})
	return true
}

// lastUpstream returns the most recent SetUpstreamErrorActive snapshot, or nil.
func (f *fakeEventRecorder) lastUpstream() map[[3]string]int {
	if len(f.upstreamSnapshots) == 0 {
		return nil
	}
	return f.upstreamSnapshots[len(f.upstreamSnapshots)-1]
}

// TestSyncAll_EmitsPodPhaseTransitions drives SyncAll across three observed
// phases for the same pod and asserts a transition is emitted only on change
// (not on the first observation), with the right project/role labels (E2.5).
func TestSyncAll_EmitsPodPhaseTransitions(t *testing.T) {
	rec := &fakeEventRecorder{}

	// First sync: pod is Pending — establishes baseline, no transition.
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodPending,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	if len(rec.phaseTransitions) != 0 {
		t.Fatalf("first observation must not emit a transition, got %+v", rec.phaseTransitions)
	}

	// Second sync: same reporter, pod now Running → one Pending->Running.
	pod.Status.Phase = corev1.PodRunning
	if _, err := client.CoreV1().Pods("ns").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if len(rec.phaseTransitions) != 1 {
		t.Fatalf("expected 1 transition after phase change, got %+v", rec.phaseTransitions)
	}
	got := rec.phaseTransitions[0]
	if got.from != "Pending" || got.to != "Running" || got.project != "proj" || got.role != "dev" {
		t.Fatalf("unexpected transition: %+v", got)
	}

	// Third sync: phase unchanged → no new transition.
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 3: %v", err)
	}
	if len(rec.phaseTransitions) != 1 {
		t.Fatalf("unchanged phase must not emit a transition, got %+v", rec.phaseTransitions)
	}
}

// TestSyncAll_EmitsOOMLifecycleEvent verifies an OOMKilled container surfaces a
// single oom_killed lifecycle event carrying the bead's session_id, deduped
// across repeated sync cycles (E2.4 + FSM contract).
func TestSyncAll_EmitsOOMLifecycleEvent(t *testing.T) {
	rec := &fakeEventRecorder{}
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodFailed,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: "agent",
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
			},
		},
	}
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{
		sessionByID: map[string]string{"crew-proj-dev-alpha": "sess-abc"},
	}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	if len(rec.lifecycleEvents) != 1 {
		t.Fatalf("expected 1 OOM lifecycle event, got %+v", rec.lifecycleEvents)
	}
	ev := rec.lifecycleEvents[0]
	if ev.event != "oom_killed" || ev.project != "proj" || ev.role != "dev" || ev.sessionID != "sess-abc" {
		t.Fatalf("unexpected OOM event: %+v", ev)
	}

	// Second sync over the same lingering Failed pod must NOT double-count.
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if len(rec.lifecycleEvents) != 1 {
		t.Fatalf("OOM must be deduped across syncs, got %+v", rec.lifecycleEvents)
	}
}

// TestSyncAll_EmitsPodsPendingGauge drives SyncAll over a mix of Pending and
// Running pods across two (project,role) groups and asserts the per-sweep
// gardenio_agent_pods_pending snapshot counts ONLY Pending pods, grouped
// correctly, and that the count falls to zero (cleared) once the pods advance
// to Running. This is the SyncAll-integration derivation of the count (GAP-1,
// ka-0000005418); TestEventMetrics_PodsPendingGauge checks the same quantity
// independently at the gauge-method layer — the two must agree.
func TestSyncAll_EmitsPodsPendingGauge(t *testing.T) {
	rec := &fakeEventRecorder{}

	// Two pods Pending for (proj,dev), one Pending for (proj,gardener), and one
	// Running for (proj,dev) that must NOT be counted.
	pods := []*corev1.Pod{
		makePod("crew-proj-dev-a", "ns", corev1.PodPending, agentLabels("proj", "dev", "a"), "10.0.0.1"),
		makePod("crew-proj-dev-b", "ns", corev1.PodPending, agentLabels("proj", "dev", "b"), "10.0.0.2"),
		makePod("crew-proj-gardener-c", "ns", corev1.PodPending, agentLabels("proj", "gardener", "c"), "10.0.0.3"),
		makePod("crew-proj-dev-d", "ns", corev1.PodRunning, agentLabels("proj", "dev", "d"), "10.0.0.4"),
	}
	objs := make([]runtime.Object, len(pods))
	for i, p := range pods {
		objs[i] = p
	}
	client := fake.NewSimpleClientset(objs...)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	// Exactly one snapshot per sweep.
	if len(rec.pendingSnapshots) != 1 {
		t.Fatalf("expected 1 pending snapshot after 1 sweep, got %d", len(rec.pendingSnapshots))
	}
	got := rec.lastPending()
	want := map[[2]string]int{
		{"proj", "dev"}:      2,
		{"proj", "gardener"}: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pending snapshot mismatch:\n got  %v\n want %v", got, want)
	}

	// Advance the two dev pods to Running; gardener pod stays Pending.
	for _, name := range []string{"crew-proj-dev-a", "crew-proj-dev-b"} {
		p, err := client.CoreV1().Pods("ns").Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		p.Status.Phase = corev1.PodRunning
		if _, err := client.CoreV1().Pods("ns").Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("update %s: %v", name, err)
		}
	}
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	got = rec.lastPending()
	want = map[[2]string]int{{"proj", "gardener"}: 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pending snapshot after dev→Running mismatch:\n got  %v\n want %v", got, want)
	}

	// Advance the last Pending pod; the snapshot must now be empty (gauge
	// clears → sum() no-data → alert resets). An empty non-nil map is the
	// contract.
	p, err := client.CoreV1().Pods("ns").Get(context.Background(), "crew-proj-gardener-c", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get gardener pod: %v", err)
	}
	p.Status.Phase = corev1.PodRunning
	if _, err := client.CoreV1().Pods("ns").Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update gardener pod: %v", err)
	}
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 3: %v", err)
	}
	got = rec.lastPending()
	if len(got) != 0 {
		t.Fatalf("expected empty pending snapshot once nothing is Pending, got %v", got)
	}
	if got == nil {
		t.Fatal("SetPodsPending must be called with a non-nil (empty) map, not skipped, so the gauge clears")
	}
}

// TestSyncAll_NoEventRecorder ensures SyncAll is a clean no-op for metrics when
// no recorder is wired (the default).
func TestSyncAll_NoEventRecorder(t *testing.T) {
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	// No SetEventRecorder — events stays nil.
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func TestPodWasOOMKilled(t *testing.T) {
	if podWasOOMKilled(nil) {
		t.Fatal("nil pod should not be OOM")
	}
	clean := makePod("p", "ns", corev1.PodRunning, agentLabels("proj", "dev", "a"), "")
	if podWasOOMKilled(clean) {
		t.Fatal("clean pod should not be OOM")
	}
	oomCurrent := makePod("p", "ns", corev1.PodFailed, agentLabels("proj", "dev", "a"), "")
	oomCurrent.Status.ContainerStatuses = []corev1.ContainerStatus{
		{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}},
	}
	if !podWasOOMKilled(oomCurrent) {
		t.Fatal("current-terminated OOMKilled should be detected")
	}
	oomInit := makePod("p", "ns", corev1.PodFailed, agentLabels("proj", "dev", "a"), "")
	oomInit.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}},
	}
	if !podWasOOMKilled(oomInit) {
		t.Fatal("init-container OOMKilled should be detected")
	}
	otherReason := makePod("p", "ns", corev1.PodFailed, agentLabels("proj", "dev", "a"), "")
	otherReason.Status.ContainerStatuses = []corev1.ContainerStatus{
		{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error"}}},
	}
	if podWasOOMKilled(otherReason) {
		t.Fatal("non-OOM termination should not be detected as OOM")
	}
}

// TestSyncAll_EmitsStartedLifecycleEvent: the witnessed Pending→Running phase
// transition emits exactly one "started" lifecycle event (ka-0000000550), and
// a pod FIRST observed already Running (controller restart) establishes a
// baseline without emitting — so a controller roll never replays a started
// burst for the fleet.
func TestSyncAll_EmitsStartedLifecycleEvent(t *testing.T) {
	rec := &fakeEventRecorder{}
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodPending,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)

	// Sweep 1: Pending baseline — nothing.
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	if len(rec.lifecycleEvents) != 0 {
		t.Fatalf("no lifecycle event expected on baseline, got %+v", rec.lifecycleEvents)
	}

	// Sweep 2: Running — one "started".
	pod.Status.Phase = corev1.PodRunning
	if _, err := client.CoreV1().Pods("ns").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if len(rec.lifecycleEvents) != 1 {
		t.Fatalf("expected 1 started event, got %+v", rec.lifecycleEvents)
	}
	ev := rec.lifecycleEvents[0]
	if ev.event != "started" || ev.project != "proj" || ev.role != "dev" || ev.sessionID != "" {
		t.Fatalf("unexpected started event: %+v", ev)
	}

	// Sweep 3: still Running — no re-emit.
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 3: %v", err)
	}
	if len(rec.lifecycleEvents) != 1 {
		t.Fatalf("started must not re-emit while Running, got %+v", rec.lifecycleEvents)
	}
}

// TestSyncAll_NoStartedOnFirstObservedRunning: a pod already Running at first
// observation (controller restart over a live fleet) must not emit started.
func TestSyncAll_NoStartedOnFirstObservedRunning(t *testing.T) {
	rec := &fakeEventRecorder{}
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	r.SetEventRecorder(rec)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	for _, ev := range rec.lifecycleEvents {
		if ev.event == "started" {
			t.Fatalf("first-observed Running pod must not emit started: %+v", rec.lifecycleEvents)
		}
	}
}
