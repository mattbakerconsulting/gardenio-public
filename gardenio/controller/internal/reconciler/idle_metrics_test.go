package reconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
)

func TestClassifyActivity(t *testing.T) {
	active := []string{"working", "spawning", "assigning", "resuming"}
	for _, s := range active {
		if got := classifyActivity(s); got != "active" {
			t.Errorf("classifyActivity(%q) = %q, want active", s, got)
		}
	}
	// Everything else — resting states AND unknown future states — is idle.
	idle := []string{
		"idle", "prewarmed", "queued", "yielding", "rate_limited",
		"budget_exceeded", "stopping", "", "some_future_state",
	}
	for _, s := range idle {
		if got := classifyActivity(s); got != "idle" {
			t.Errorf("classifyActivity(%q) = %q, want idle", s, got)
		}
	}
}

func TestPodMemoryRequestBytes(t *testing.T) {
	t.Run("sums app containers", func(t *testing.T) {
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			}}},
			{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			}}},
		}}}
		want := int64(8*1024*1024*1024) + int64(64*1024*1024)
		if got := podMemoryRequestBytes(pod); got != want {
			t.Errorf("podMemoryRequestBytes = %d, want %d", got, want)
		}
	})

	t.Run("container with no memory request contributes zero", func(t *testing.T) {
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "no-requests"},
			{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			}}},
		}}}
		if got := podMemoryRequestBytes(pod); got != int64(1024*1024*1024) {
			t.Errorf("podMemoryRequestBytes = %d, want 1Gi", got)
		}
	})

	t.Run("no containers is zero", func(t *testing.T) {
		if got := podMemoryRequestBytes(&corev1.Pod{}); got != 0 {
			t.Errorf("podMemoryRequestBytes = %d, want 0", got)
		}
	})
}

func TestIdleMetrics_Register(t *testing.T) {
	m := NewIdleMetrics()
	reg := prometheus.NewRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Duplicate register must fail (validates both collectors registered).
	if err := m.Register(reg); err == nil {
		t.Fatal("expected duplicate-register error, got nil")
	}
}

func TestIdleMetrics_RegisterReservedMemConflict(t *testing.T) {
	m := NewIdleMetrics()
	reg := prometheus.NewRegistry()
	// Seed only the reserved-memory collector to force the second branch to fail.
	conflict := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "agent_reserved_memory_bytes",
		Help:      "conflict",
	}, []string{"project", "activity"})
	if err := reg.Register(conflict); err != nil {
		t.Fatalf("seed register: %v", err)
	}
	if err := m.Register(reg); err == nil {
		t.Fatal("expected conflict error from ReservedMemoryBytes register")
	}
}

// memPod builds a live agent pod requesting `mem` memory on its single app
// container, keyed to `podName` (the same key desired uses).
func memPodForActivity(name, namespace, mode, project, role, agent, mem string, phase corev1.PodPhase) corev1.Pod {
	p := makePod(name, namespace, mode, project, role, agent, phase)
	p.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(mem)},
	}
	return p
}

func TestRecordIdleMetrics_SplitsAndSumsMemory(t *testing.T) {
	r := New(&mockLister{}, &mockManager{}, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	// Two agents in project "proj": one working (active), one idle. Pod names
	// follow the reconciler's convention mode-project-role-agent.
	desired := map[string]beadsapi.AgentBead{
		"crew-proj-dev-alpha": {ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha", AgentState: "working"},
		"crew-proj-dev-bravo": {ID: "bd-2", Project: "proj", Mode: "crew", Role: "dev", AgentName: "bravo", AgentState: "idle"},
	}
	actualMap := map[string]corev1.Pod{
		"crew-proj-dev-alpha": memPodForActivity("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", "8Gi", corev1.PodRunning),
		"crew-proj-dev-bravo": memPodForActivity("crew-proj-dev-bravo", "ns", "crew", "proj", "dev", "bravo", "8Gi", corev1.PodRunning),
	}

	r.recordIdleMetrics(desired, actualMap)

	if got := testutil.ToFloat64(r.idleMetrics.AgentsByActivity.WithLabelValues("proj", "active")); got != 1 {
		t.Errorf("active count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.idleMetrics.AgentsByActivity.WithLabelValues("proj", "idle")); got != 1 {
		t.Errorf("idle count = %v, want 1", got)
	}
	wantMem := float64(8 * 1024 * 1024 * 1024)
	if got := testutil.ToFloat64(r.idleMetrics.ReservedMemoryBytes.WithLabelValues("proj", "idle")); got != wantMem {
		t.Errorf("idle reserved mem = %v, want %v", got, wantMem)
	}
	if got := testutil.ToFloat64(r.idleMetrics.ReservedMemoryBytes.WithLabelValues("proj", "active")); got != wantMem {
		t.Errorf("active reserved mem = %v, want %v", got, wantMem)
	}
}

func TestRecordIdleMetrics_PodlessIdleReservesNothing(t *testing.T) {
	r := New(&mockLister{}, &mockManager{}, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	// A deferred (podless) idle agent: counted, but reserves no memory yet.
	desired := map[string]beadsapi.AgentBead{
		"crew-proj-dev-parked": {ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "parked", AgentState: "queued"},
	}
	r.recordIdleMetrics(desired, map[string]corev1.Pod{})

	if got := testutil.ToFloat64(r.idleMetrics.AgentsByActivity.WithLabelValues("proj", "idle")); got != 1 {
		t.Errorf("idle count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.idleMetrics.ReservedMemoryBytes.WithLabelValues("proj", "idle")); got != 0 {
		t.Errorf("podless idle reserved mem = %v, want 0", got)
	}
}

func TestRecordIdleMetrics_TerminalPodExcludedFromMemory(t *testing.T) {
	r := New(&mockLister{}, &mockManager{}, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	// An idle agent whose pod is Succeeded (terminating this pass): its
	// reservation is releasing, so it must not be booked as wasted memory.
	desired := map[string]beadsapi.AgentBead{
		"crew-proj-dev-gone": {ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "gone", AgentState: "idle"},
	}
	actualMap := map[string]corev1.Pod{
		"crew-proj-dev-gone": memPodForActivity("crew-proj-dev-gone", "ns", "crew", "proj", "dev", "gone", "8Gi", corev1.PodSucceeded),
	}
	r.recordIdleMetrics(desired, actualMap)

	if got := testutil.ToFloat64(r.idleMetrics.AgentsByActivity.WithLabelValues("proj", "idle")); got != 1 {
		t.Errorf("idle count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.idleMetrics.ReservedMemoryBytes.WithLabelValues("proj", "idle")); got != 0 {
		t.Errorf("terminal-pod idle reserved mem = %v, want 0", got)
	}
}

func TestRecordIdleMetrics_ResetClearsDepartedAgents(t *testing.T) {
	r := New(&mockLister{}, &mockManager{}, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	// First pass: one idle agent.
	r.recordIdleMetrics(map[string]beadsapi.AgentBead{
		"crew-proj-dev-x": {ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "x", AgentState: "idle"},
	}, map[string]corev1.Pod{
		"crew-proj-dev-x": memPodForActivity("crew-proj-dev-x", "ns", "crew", "proj", "dev", "x", "8Gi", corev1.PodRunning),
	})
	if got := testutil.CollectAndCount(r.idleMetrics.AgentsByActivity); got == 0 {
		t.Fatal("expected series after first pass")
	}

	// Second pass: empty fleet. Reset must clear all series (drained project → 0).
	r.recordIdleMetrics(map[string]beadsapi.AgentBead{}, map[string]corev1.Pod{})
	if got := testutil.CollectAndCount(r.idleMetrics.AgentsByActivity); got != 0 {
		t.Errorf("expected 0 series after drain, got %d", got)
	}
	if got := testutil.CollectAndCount(r.idleMetrics.ReservedMemoryBytes); got != 0 {
		t.Errorf("expected 0 mem series after drain, got %d", got)
	}
}

func TestRecordIdleMetrics_EmitsZeroForEmptyBucket(t *testing.T) {
	r := New(&mockLister{}, &mockManager{}, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	// Only idle agents in the project — the active bucket must still emit an
	// explicit 0 so the idle:active ratio is a well-defined division.
	r.recordIdleMetrics(map[string]beadsapi.AgentBead{
		"crew-proj-dev-y": {ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "y", AgentState: "idle"},
	}, map[string]corev1.Pod{})

	if got := testutil.ToFloat64(r.idleMetrics.AgentsByActivity.WithLabelValues("proj", "active")); got != 0 {
		t.Errorf("active bucket = %v, want explicit 0", got)
	}
}

func TestRecordIdleMetrics_EmptyProjectLabelledUnknown(t *testing.T) {
	r := New(&mockLister{}, &mockManager{}, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	r.recordIdleMetrics(map[string]beadsapi.AgentBead{
		"crew--dev-z": {ID: "bd-1", Project: "", Mode: "crew", Role: "dev", AgentName: "z", AgentState: "working"},
	}, map[string]corev1.Pod{})
	if got := testutil.ToFloat64(r.idleMetrics.AgentsByActivity.WithLabelValues("unknown", "active")); got != 1 {
		t.Errorf("empty-project active count under 'unknown' = %v, want 1", got)
	}
}

func TestRecordIdleMetrics_NilMetricsNoPanic(t *testing.T) {
	r := &Reconciler{logger: testLogger()} // no idleMetrics wired
	// Must be a no-op, not a panic.
	r.recordIdleMetrics(map[string]beadsapi.AgentBead{
		"crew-proj-dev-a": {ID: "bd-1", Project: "proj", AgentState: "idle"},
	}, map[string]corev1.Pod{})
}

func TestIdleActiveSummary(t *testing.T) {
	desired := map[string]beadsapi.AgentBead{
		"crew-proj-dev-a": {Project: "proj", Mode: "crew", Role: "dev", AgentName: "a", AgentState: "working"},
		"crew-proj-dev-b": {Project: "proj", Mode: "crew", Role: "dev", AgentName: "b", AgentState: "idle"},
		"crew-proj-dev-c": {Project: "proj", Mode: "crew", Role: "dev", AgentName: "c", AgentState: "yielding"},
	}
	actualMap := map[string]corev1.Pod{
		"crew-proj-dev-a": memPodForActivity("crew-proj-dev-a", "ns", "crew", "proj", "dev", "a", "8Gi", corev1.PodRunning),
		"crew-proj-dev-b": memPodForActivity("crew-proj-dev-b", "ns", "crew", "proj", "dev", "b", "8Gi", corev1.PodRunning),
		"crew-proj-dev-c": memPodForActivity("crew-proj-dev-c", "ns", "crew", "proj", "dev", "c", "4Gi", corev1.PodRunning),
	}
	active, idle, idleMem := idleActiveSummary(desired, actualMap)
	if active != 1 {
		t.Errorf("active = %d, want 1", active)
	}
	if idle != 2 {
		t.Errorf("idle = %d, want 2 (idle + yielding)", idle)
	}
	// idle (8Gi) + yielding (4Gi) reserved memory.
	wantMem := int64(8*1024*1024*1024) + int64(4*1024*1024*1024)
	if idleMem != wantMem {
		t.Errorf("idleMem = %d, want %d", idleMem, wantMem)
	}
}

// TestRecordIdleMetrics_ViaReconcile exercises the full wiring: a real
// Reconcile pass must populate the idle gauges from the listed beads + pods.
func TestRecordIdleMetrics_ViaReconcile(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{
		{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha", AgentState: "working"},
		{ID: "bd-2", Project: "proj", Mode: "crew", Role: "dev", AgentName: "bravo", AgentState: "idle"},
	}}
	mgr := &mockManager{pods: []corev1.Pod{
		memPodForActivity("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", "8Gi", corev1.PodRunning),
		memPodForActivity("crew-proj-dev-bravo", "ns", "crew", "proj", "dev", "bravo", "8Gi", corev1.PodRunning),
	}}
	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := testutil.ToFloat64(r.idleMetrics.AgentsByActivity.WithLabelValues("proj", "idle")); got != 1 {
		t.Errorf("idle count after Reconcile = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.idleMetrics.ReservedMemoryBytes.WithLabelValues("proj", "idle")); got != float64(8*1024*1024*1024) {
		t.Errorf("idle reserved mem after Reconcile = %v, want 8Gi", got)
	}
}
