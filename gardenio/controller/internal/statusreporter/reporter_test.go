package statusreporter

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/podmanager"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Mock implementations ---

type mockBeadUpdater struct {
	notesCalls     []noteCall
	stateCalls     []stateCall
	fieldCalls     []fieldCall
	typedFieldCall []typedFieldCall
	notesErr       error
	stateErr       error
	fieldErr       error
	typedFieldErr  error
	statusByID     map[string]string
	statusErr      error
	statusCalls    []string
	stateByID      map[string]string
	getStateErr    error
	sessionByID    map[string]string
	getSessionErr  error
}

type noteCall struct {
	beadID string
	notes  string
}

type stateCall struct {
	beadID string
	state  string
}

type fieldCall struct {
	beadID string
	fields map[string]string
}

type typedFieldCall struct {
	beadID string
	fields map[string]any
}

func (m *mockBeadUpdater) UpdateBeadNotes(_ context.Context, beadID, notes string) error {
	m.notesCalls = append(m.notesCalls, noteCall{beadID: beadID, notes: notes})
	return m.notesErr
}

func (m *mockBeadUpdater) UpdateAgentState(_ context.Context, beadID, state string) error {
	m.stateCalls = append(m.stateCalls, stateCall{beadID: beadID, state: state})
	return m.stateErr
}

func (m *mockBeadUpdater) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	copied := make(map[string]string, len(fields))
	maps.Copy(copied, fields)
	m.fieldCalls = append(m.fieldCalls, fieldCall{beadID: beadID, fields: copied})
	return m.fieldErr
}

func (m *mockBeadUpdater) UpdateBeadFieldsTyped(_ context.Context, beadID string, fields map[string]any) error {
	copied := make(map[string]any, len(fields))
	maps.Copy(copied, fields)
	m.typedFieldCall = append(m.typedFieldCall, typedFieldCall{beadID: beadID, fields: copied})
	return m.typedFieldErr
}

func (m *mockBeadUpdater) GetBeadStatus(_ context.Context, beadID string) (string, error) {
	m.statusCalls = append(m.statusCalls, beadID)
	if m.statusErr != nil {
		return "", m.statusErr
	}
	if m.statusByID == nil {
		return "open", nil
	}
	if s, ok := m.statusByID[beadID]; ok {
		return s, nil
	}
	return "open", nil
}

func (m *mockBeadUpdater) GetAgentState(_ context.Context, beadID string) (string, error) {
	if m.getStateErr != nil {
		return "", m.getStateErr
	}
	if m.stateByID == nil {
		return "", nil
	}
	return m.stateByID[beadID], nil
}

func (m *mockBeadUpdater) GetAgentSessionID(_ context.Context, beadID string) (string, error) {
	if m.getSessionErr != nil {
		return "", m.getSessionErr
	}
	if m.sessionByID == nil {
		return "", nil
	}
	return m.sessionByID[beadID], nil
}

func TestIsStartupState(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"", true},          // bead fetch failed / field unset — fail toward working
		{"spawning", true},  // Pending / early startup
		{"assigning", true}, // prewarmed-pool hand-off
		{"prewarmed", true}, // parked in pool, not yet assigned
		{"working", false},  // genuine parked case — honor idle-skip
		{"idle", false},
		{"done", false},
		{"failed", false},
		{"rate_limited", false},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			if got := isStartupState(tt.state); got != tt.want {
				t.Errorf("isStartupState(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- PhaseToAgentState tests ---

func TestPhaseToAgentState(t *testing.T) {
	tests := []struct {
		phase string
		want  string
	}{
		{"Pending", "spawning"},
		{"Running", "working"},
		{"Succeeded", "done"},
		{"Failed", "failed"},
		{"Stopped", "done"},
		{"Unknown", ""},
		{"", ""},
		{"SomethingElse", ""},
	}
	for _, tt := range tests {
		t.Run(tt.phase, func(t *testing.T) {
			got := PhaseToAgentState(tt.phase)
			if got != tt.want {
				t.Errorf("PhaseToAgentState(%q) = %q, want %q", tt.phase, got, tt.want)
			}
		})
	}
}

// --- IsPodSchedulingBlocked tests ---

func TestIsPodSchedulingBlocked(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "nil pod",
			pod:  nil,
			want: false,
		},
		{
			name: "pending with PodScheduled=Unschedulable",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable},
					},
				},
			},
			want: true,
		},
		{
			name: "pending without PodScheduled condition",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			},
			want: false,
		},
		{
			name: "pending with different reason (e.g. ContainerCreating)",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, Reason: ""},
					},
				},
			},
			want: false,
		},
		{
			name: "running pod with stale Unschedulable reason ignored",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodScheduled, Reason: corev1.PodReasonUnschedulable},
					},
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPodSchedulingBlocked(tt.pod); got != tt.want {
				t.Errorf("IsPodSchedulingBlocked() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- agentBeadID tests ---

func TestAgentBeadID_FromAnnotation(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				podmanager.AnnotationBeadID: "gd-explicit-id",
			},
			Labels: map[string]string{
				podmanager.LabelProject: "proj",
				podmanager.LabelRole:    "dev",
				podmanager.LabelAgent:   "alpha",
				podmanager.LabelMode:    "crew",
			},
		},
	}
	got := agentBeadID(pod)
	if got != "gd-explicit-id" {
		t.Errorf("expected annotation-based ID gd-explicit-id, got %s", got)
	}
}

func TestAgentBeadID_FromLabels(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				podmanager.LabelProject: "gardenio",
				podmanager.LabelRole:    "crew",
				podmanager.LabelAgent:   "furiosa",
				podmanager.LabelMode:    "crew",
			},
		},
	}
	got := agentBeadID(pod)
	if got != "crew-gardenio-crew-furiosa" {
		t.Errorf("expected crew-gardenio-crew-furiosa, got %s", got)
	}
}

func TestAgentBeadID_DefaultMode(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				podmanager.LabelProject: "proj",
				podmanager.LabelRole:    "dev",
				podmanager.LabelAgent:   "beta",
				// No LabelMode — defaults to "crew"
			},
		},
	}
	got := agentBeadID(pod)
	if got != "crew-proj-dev-beta" {
		t.Errorf("expected crew-proj-dev-beta, got %s", got)
	}
}

// --- detectCoopPort tests ---

func TestDetectCoopPort_CoopContainerWithPort(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "agent",
					Ports: []corev1.ContainerPort{
						{ContainerPort: 3000},
					},
				},
				{
					Name: "coop",
					Ports: []corev1.ContainerPort{
						{ContainerPort: 8080},
					},
				},
			},
		},
	}
	got := detectCoopPort(pod)
	if got != 8080 {
		t.Errorf("expected 8080 for coop container with port 8080, got %d", got)
	}
}

func TestDetectCoopPort_CoopContainerNoPort(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "coop"}, // no ports defined
			},
		},
	}
	got := detectCoopPort(pod)
	if got != 8080 {
		t.Errorf("expected default 8080 for coop container without ports, got %d", got)
	}
}

func TestDetectCoopPort_NonCoopContainerWith8080(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "agent",
					Ports: []corev1.ContainerPort{
						{ContainerPort: 8080},
					},
				},
			},
		},
	}
	got := detectCoopPort(pod)
	if got != 8080 {
		t.Errorf("expected 8080 for non-coop container exposing 8080, got %d", got)
	}
}

func TestDetectCoopPort_NoCoopCapability(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "agent",
					Ports: []corev1.ContainerPort{
						{ContainerPort: 3000},
					},
				},
			},
		},
	}
	got := detectCoopPort(pod)
	if got != 0 {
		t.Errorf("expected 0 for pod without coop capability, got %d", got)
	}
}

func TestDetectCoopPort_NoContainers(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{},
	}
	got := detectCoopPort(pod)
	if got != 0 {
		t.Errorf("expected 0 for pod with no containers, got %d", got)
	}
}

func TestDetectCoopPort_CoopContainerWithDifferentPort(t *testing.T) {
	// Coop container exists but exposes a non-8080 port — returns default 8080.
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "coop",
					Ports: []corev1.ContainerPort{
						{ContainerPort: 9090},
					},
				},
			},
		},
	}
	got := detectCoopPort(pod)
	if got != 8080 {
		t.Errorf("expected default 8080 for coop container with non-8080 port, got %d", got)
	}
}

// --- NewHTTPReporter tests ---

func TestNewHTTPReporter(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "test-ns", testLogger())
	if r == nil {
		t.Fatal("expected non-nil reporter")
	}
	if r.namespace != "test-ns" {
		t.Errorf("expected namespace test-ns, got %s", r.namespace)
	}
}

// --- ReportPodStatus tests ---

func TestReportPodStatus_Running(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportPodStatus(context.Background(), "agent-1", PodStatus{
		PodName: "pod-1", Namespace: "ns", Phase: "Running", Ready: true,
	}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update, got %d", len(daemon.stateCalls))
	}
	if daemon.stateCalls[0].beadID != "agent-1" {
		t.Errorf("expected beadID agent-1, got %s", daemon.stateCalls[0].beadID)
	}
	if daemon.stateCalls[0].state != "working" {
		t.Errorf("expected state working, got %s", daemon.stateCalls[0].state)
	}
}

func TestReportPodStatus_UnknownPhase_Skips(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportPodStatus(context.Background(), "agent-1", PodStatus{
		PodName: "pod-1", Namespace: "ns", Phase: "Unknown",
	}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 0 {
		t.Errorf("expected 0 state updates for unknown phase, got %d", len(daemon.stateCalls))
	}
}

func TestReportPodStatus_DaemonError(t *testing.T) {
	daemon := &mockBeadUpdater{stateErr: errors.New("daemon unreachable")}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportPodStatus(context.Background(), "agent-1", PodStatus{
		PodName: "pod-1", Namespace: "ns", Phase: "Running",
	}, false)
	if err == nil {
		t.Fatal("expected error when daemon fails")
	}
	if !strings.Contains(err.Error(), "reporting status for agent-1") {
		t.Errorf("expected wrapped error mentioning agent, got: %v", err)
	}
}

func TestReportPodStatus_AllPhases(t *testing.T) {
	phases := []struct {
		phase string
		state string
	}{
		{"Pending", "spawning"},
		{"Running", "working"},
		{"Succeeded", "done"},
		{"Failed", "failed"},
		{"Stopped", "done"},
	}
	for _, tt := range phases {
		t.Run(tt.phase, func(t *testing.T) {
			daemon := &mockBeadUpdater{}
			client := fake.NewSimpleClientset()
			r := NewHTTPReporter(daemon, client, "ns", testLogger())

			err := r.ReportPodStatus(context.Background(), "agent-1", PodStatus{
				PodName: "pod-1", Phase: tt.phase,
			}, false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(daemon.stateCalls) != 1 {
				t.Fatalf("expected 1 call, got %d", len(daemon.stateCalls))
			}
			if daemon.stateCalls[0].state != tt.state {
				t.Errorf("expected state %s, got %s", tt.state, daemon.stateCalls[0].state)
			}
		})
	}
}

func TestReportPodStatus_PrewarmedSkipsNonTerminal(t *testing.T) {
	// Prewarmed agents should not have their state overwritten by spawning or working.
	phases := []struct {
		phase string
	}{
		{"Running"},
		{"Pending"},
	}
	for _, tt := range phases {
		t.Run(tt.phase, func(t *testing.T) {
			daemon := &mockBeadUpdater{}
			client := fake.NewSimpleClientset()
			r := NewHTTPReporter(daemon, client, "ns", testLogger())

			err := r.ReportPodStatus(context.Background(), "agent-1", PodStatus{
				PodName: "pod-1", Namespace: "ns", Phase: tt.phase, Ready: true,
			}, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(daemon.stateCalls) != 0 {
				t.Errorf("expected 0 state updates for prewarmed+%s, got %d", tt.phase, len(daemon.stateCalls))
			}
		})
	}
}

func TestReportPodStatus_PrewarmedAllowsTerminal(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	// Prewarmed agent with Failed pod SHOULD still update to "failed".
	err := r.ReportPodStatus(context.Background(), "agent-1", PodStatus{
		PodName: "pod-1", Namespace: "ns", Phase: "Failed",
	}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update for prewarmed+Failed, got %d", len(daemon.stateCalls))
	}
	if daemon.stateCalls[0].state != "failed" {
		t.Errorf("expected state failed, got %s", daemon.stateCalls[0].state)
	}
}

// TestReportPodStatus_ClosedBead_Skips verifies the reporter does not PATCH
// agent_state on a bead that beads daemon reports as already closed. Regression
// guard for ka-0000005253 — without this, the K8s pod grace period after
// `gb stop` lets the periodic reconciler flip agent_state from "done" back
// to "working" in the activity feed.
func TestReportPodStatus_ClosedBead_Skips(t *testing.T) {
	daemon := &mockBeadUpdater{
		statusByID: map[string]string{"agent-1": "closed"},
	}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportPodStatus(context.Background(), "agent-1", PodStatus{
		PodName: "pod-1", Namespace: "ns", Phase: "Running", Ready: true,
	}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 0 {
		t.Errorf("expected 0 state updates for closed bead, got %d", len(daemon.stateCalls))
	}
}

// TestReportPodStatus_SuspendLatchedBead_Skips guards ka-0000006721: a
// freeze-eligible `gb stop` leaves the bead OPEN at agent_state=stopping as the
// frozen-awaiting-thaw signal while the controller checkpoints and tears the pod
// down. While the pod is still Running, ReportPodStatus maps Running->"working"
// and — without this guard — would PATCH it back over the stopping latch,
// un-suspending the agent (the controller-side twin of the in-pod monitorIdle
// un-latch this bug fixes). The bead is OPEN, so the closed-bead skip cannot
// cover it.
func TestReportPodStatus_SuspendLatchedBead_Skips(t *testing.T) {
	daemon := &mockBeadUpdater{
		stateByID: map[string]string{"agent-1": "stopping"},
	}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportPodStatus(context.Background(), "agent-1", PodStatus{
		PodName: "pod-1", Namespace: "ns", Phase: "Running", Ready: true,
	}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 0 {
		t.Errorf("expected 0 state updates for suspend-latched bead, got %d (%+v)",
			len(daemon.stateCalls), daemon.stateCalls)
	}
}

// TestReportPodStatus_SuspendLatchedBead_AllowsTerminal proves the guard does not
// suppress a truthful terminal write: if the freeze fails and the pod dies while
// the bead still reads stopping, the Failed phase must still be reported so the
// agent is not stuck at stopping forever.
func TestReportPodStatus_SuspendLatchedBead_AllowsTerminal(t *testing.T) {
	daemon := &mockBeadUpdater{
		stateByID: map[string]string{"agent-1": "stopping"},
	}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportPodStatus(context.Background(), "agent-1", PodStatus{
		PodName: "pod-1", Namespace: "ns", Phase: "Failed", Ready: false,
	}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 1 || daemon.stateCalls[0].state != "failed" {
		t.Errorf("expected the terminal 'failed' write to pass the suspend-latch guard, got %+v",
			daemon.stateCalls)
	}
}

// TestReportPodStatus_WorkingBead_NotSuppressed proves the guard does not regress
// the normal path: a live working agent (bead not stopping) still gets its
// Running->working report.
func TestReportPodStatus_WorkingBead_NotSuppressed(t *testing.T) {
	daemon := &mockBeadUpdater{
		stateByID: map[string]string{"agent-1": "working"},
	}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportPodStatus(context.Background(), "agent-1", PodStatus{
		PodName: "pod-1", Namespace: "ns", Phase: "Running", Ready: true,
	}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 1 || daemon.stateCalls[0].state != "working" {
		t.Errorf("expected 1 'working' write for a live agent, got %+v", daemon.stateCalls)
	}
}

// --- ReportBackendMetadata tests ---

func TestReportBackendMetadata_FullMetadata(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportBackendMetadata(context.Background(), "agent-1", BackendMetadata{
		PodName:   "pod-1",
		Namespace: "ns",
		Backend:   "coop",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.notesCalls) != 1 {
		t.Fatalf("expected 1 notes update, got %d", len(daemon.notesCalls))
	}
	notes := daemon.notesCalls[0].notes
	for _, expected := range []string{
		"backend: coop",
		"pod_name: pod-1",
		"pod_namespace: ns",
	} {
		if !strings.Contains(notes, expected) {
			t.Errorf("notes missing %q, got: %s", expected, notes)
		}
	}
	// Nothing dials the pod any more, so its address is not published.
	if strings.Contains(notes, "coop_url") {
		t.Errorf("notes must not contain coop_url, got: %s", notes)
	}
	if strings.Contains(notes, "coop_token") {
		t.Errorf("notes must not contain coop_token (credential leak), got: %s", notes)
	}
}

// TestReportBackendMetadata_NeverWritesCoopToken is a regression test for
// ka-0000007774 / audit gd-xswVHS1eEA §1.1: the bearer token must never be
// persisted to bead notes regardless of input shape.
func TestReportBackendMetadata_NeverWritesCoopToken(t *testing.T) {
	cases := []struct {
		name string
		meta BackendMetadata
	}{
		{name: "empty", meta: BackendMetadata{}},
		{name: "backend_only", meta: BackendMetadata{Backend: "coop"}},
		{
			name: "all_fields",
			meta: BackendMetadata{
				PodName:   "pod-1",
				Namespace: "ns",
				Backend:   "coop",
			},
		},
		{
			name: "values_with_token_substring",
			meta: BackendMetadata{
				PodName:   "coop_token-pod",
				Namespace: "ns",
				Backend:   "coop_token-backend",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := &mockBeadUpdater{}
			client := fake.NewSimpleClientset()
			r := NewHTTPReporter(daemon, client, "ns", testLogger())

			if err := r.ReportBackendMetadata(context.Background(), "agent-1", tc.meta); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, call := range daemon.notesCalls {
				for line := range strings.SplitSeq(call.notes, "\n") {
					if strings.HasPrefix(line, "coop_token") {
						t.Errorf("notes contained coop_token line (credential leak): %q", line)
					}
				}
			}
		})
	}
}

func TestReportBackendMetadata_PartialMetadata(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportBackendMetadata(context.Background(), "agent-1", BackendMetadata{
		Backend: "coop",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.notesCalls) != 1 {
		t.Fatalf("expected 1 notes update, got %d", len(daemon.notesCalls))
	}
	notes := daemon.notesCalls[0].notes
	if strings.Contains(notes, "pod_name:") {
		t.Errorf("notes should not contain pod_name for empty PodName, got: %s", notes)
	}
	if !strings.Contains(notes, "backend: coop") {
		t.Errorf("notes missing backend, got: %s", notes)
	}
}

// TestReportBackendMetadata_PodNameOnly_EarlyCreate covers the pod-create-time
// write added in ka-0000000553: the controller stamps pod_name + pod_namespace
// immediately (no backend/coop_url, which still need the pod IP) so the Slack
// "agent" coopmux link is valid on the first card render instead of waiting for
// the next SyncAll-with-IP tick. The write must emit pod_name (so coop can
// resolve the session id) and pod_namespace, and must NOT fabricate a
// backend/coop_url it doesn't yet have.
func TestReportBackendMetadata_PodNameOnly_EarlyCreate(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportBackendMetadata(context.Background(), "agent-1", BackendMetadata{
		PodName:   "crew-gardenio-crew-furiosa",
		Namespace: "gardenio",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.notesCalls) != 1 {
		t.Fatalf("expected 1 notes update, got %d", len(daemon.notesCalls))
	}
	notes := daemon.notesCalls[0].notes
	if !strings.Contains(notes, "pod_name: crew-gardenio-crew-furiosa") {
		t.Errorf("notes missing pod_name, got: %s", notes)
	}
	if !strings.Contains(notes, "pod_namespace: gardenio") {
		t.Errorf("notes missing pod_namespace, got: %s", notes)
	}
	if strings.Contains(notes, "backend:") {
		t.Errorf("notes should not contain backend before pod IP is known, got: %s", notes)
	}
	if strings.Contains(notes, "coop_url:") {
		t.Errorf("notes should never contain coop_url, got: %s", notes)
	}
}

func TestReportBackendMetadata_EmptyMetadata_NoOp(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportBackendMetadata(context.Background(), "agent-1", BackendMetadata{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.notesCalls) != 0 {
		t.Errorf("expected 0 notes updates for empty metadata, got %d", len(daemon.notesCalls))
	}
}

// TestReportBackendMetadata_ClosedBead_Skips verifies the reporter does not
// PATCH notes on a closed bead — ka-0000005253.
func TestReportBackendMetadata_ClosedBead_Skips(t *testing.T) {
	daemon := &mockBeadUpdater{
		statusByID: map[string]string{"agent-1": "closed"},
	}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportBackendMetadata(context.Background(), "agent-1", BackendMetadata{
		Backend: "coop",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.notesCalls) != 0 {
		t.Errorf("expected 0 notes updates for closed bead, got %d", len(daemon.notesCalls))
	}
}

func TestReportBackendMetadata_DaemonError(t *testing.T) {
	daemon := &mockBeadUpdater{notesErr: errors.New("daemon down")}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	err := r.ReportBackendMetadata(context.Background(), "agent-1", BackendMetadata{
		Backend: "coop",
	})
	if err == nil {
		t.Fatal("expected error when daemon fails")
	}
	if !strings.Contains(err.Error(), "reporting backend metadata for agent-1") {
		t.Errorf("expected wrapped error mentioning agent, got: %v", err)
	}
}

// --- Metrics tests ---

func TestMetrics_InitialState(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	m := r.Metrics()
	if m.StatusReportsTotal != 0 || m.StatusReportErrors != 0 ||
		m.SyncAllRuns != 0 || m.SyncAllErrors != 0 {
		t.Errorf("expected all zeros initially, got %+v", m)
	}
}

func TestMetrics_AfterReports(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	// Successful status report
	_ = r.ReportPodStatus(context.Background(), "a", PodStatus{Phase: "Running"}, false)
	// Successful metadata report
	_ = r.ReportBackendMetadata(context.Background(), "a", BackendMetadata{Backend: "coop"})

	m := r.Metrics()
	if m.StatusReportsTotal != 2 {
		t.Errorf("expected 2 total reports, got %d", m.StatusReportsTotal)
	}
	if m.StatusReportErrors != 0 {
		t.Errorf("expected 0 errors, got %d", m.StatusReportErrors)
	}
}

func TestMetrics_AfterErrors(t *testing.T) {
	daemon := &mockBeadUpdater{stateErr: errors.New("fail")}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	_ = r.ReportPodStatus(context.Background(), "a", PodStatus{Phase: "Running"}, false)

	m := r.Metrics()
	if m.StatusReportsTotal != 1 {
		t.Errorf("expected 1 total report, got %d", m.StatusReportsTotal)
	}
	if m.StatusReportErrors != 1 {
		t.Errorf("expected 1 error, got %d", m.StatusReportErrors)
	}
}

func TestMetrics_SkippedPhaseStillCounts(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	// Unknown phase — skipped, but still counted as a report
	_ = r.ReportPodStatus(context.Background(), "a", PodStatus{Phase: "Unknown"}, false)

	m := r.Metrics()
	if m.StatusReportsTotal != 1 {
		t.Errorf("expected 1 total report (even for skipped phase), got %d", m.StatusReportsTotal)
	}
	if m.StatusReportErrors != 0 {
		t.Errorf("expected 0 errors for skipped phase, got %d", m.StatusReportErrors)
	}
}

// --- reportLiveUsage tests ---

func TestReportLiveUsage_WritesContextAndCost(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	r.reportLiveUsage(context.Background(), "agent-1", "pod-1", coopAgentInfo{
		contextTokens: 123456,
		costUSD:       1.2345,
		turns:         42,
	})

	// Must write via the TYPED path (native JSON numbers) — the string-map
	// UpdateBeadFields sends never-seeded numeric fields as JSON strings, which
	// the daemon rejects ("must be an integer"), so the fields never land on
	// any agent bead (ka-0000008019).
	if len(daemon.fieldCalls) != 0 {
		t.Fatalf("expected 0 string-map field updates (would be rejected by schema), got %d", len(daemon.fieldCalls))
	}
	if len(daemon.typedFieldCall) != 1 {
		t.Fatalf("expected 1 typed field update, got %d", len(daemon.typedFieldCall))
	}
	got := daemon.typedFieldCall[0]
	if got.beadID != "agent-1" {
		t.Errorf("expected beadID agent-1, got %s", got.beadID)
	}
	// context_tokens must be a native integer type (uint64), NOT a string —
	// this is the exact assertion that would have caught the original bug.
	ctxVal, ok := got.fields["context_tokens"].(uint64)
	if !ok {
		t.Fatalf("expected context_tokens to be uint64, got %T (%v)", got.fields["context_tokens"], got.fields["context_tokens"])
	}
	if ctxVal != 123456 {
		t.Errorf("expected context_tokens 123456, got %d", ctxVal)
	}
	costVal, ok := got.fields["live_cost_usd"].(float64)
	if !ok {
		t.Fatalf("expected live_cost_usd to be float64, got %T (%v)", got.fields["live_cost_usd"], got.fields["live_cost_usd"])
	}
	if costVal != 1.2345 {
		t.Errorf("expected live_cost_usd 1.2345, got %v", costVal)
	}
	// live_turns is the live counterpart of total_request_count and must be
	// written as a native integer (uint64), never a string — same schema
	// requirement as context_tokens (ka-0000006449).
	turnsVal, ok := got.fields["live_turns"].(uint64)
	if !ok {
		t.Fatalf("expected live_turns to be uint64, got %T (%v)", got.fields["live_turns"], got.fields["live_turns"])
	}
	if turnsVal != 42 {
		t.Errorf("expected live_turns 42, got %d", turnsVal)
	}
}

func TestReportLiveUsage_SkipsWhenBothZero(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	// A freshly-started agent reports zero usage before its first request —
	// must not write a premature "0 tokens · $0.00".
	r.reportLiveUsage(context.Background(), "agent-1", "pod-1", coopAgentInfo{})

	if len(daemon.typedFieldCall) != 0 {
		t.Fatalf("expected no field update for zero usage, got %d", len(daemon.typedFieldCall))
	}
}

func TestReportLiveUsage_WritesWhenOnlyContextKnown(t *testing.T) {
	daemon := &mockBeadUpdater{}
	client := fake.NewSimpleClientset()
	r := NewHTTPReporter(daemon, client, "ns", testLogger())

	// Cost can lag context (turns missing response_cost) — context alone still
	// warrants a write.
	r.reportLiveUsage(context.Background(), "agent-1", "pod-1", coopAgentInfo{contextTokens: 9000})

	if len(daemon.typedFieldCall) != 1 {
		t.Fatalf("expected 1 typed field update, got %d", len(daemon.typedFieldCall))
	}
	if cost, ok := daemon.typedFieldCall[0].fields["live_cost_usd"].(float64); !ok || cost != 0 {
		t.Errorf("expected live_cost_usd float64 0, got %T %v", daemon.typedFieldCall[0].fields["live_cost_usd"], daemon.typedFieldCall[0].fields["live_cost_usd"])
	}
}
