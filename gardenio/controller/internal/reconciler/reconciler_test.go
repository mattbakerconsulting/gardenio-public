package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// --- Mock implementations ---

// mockBeadCloser implements BeadCloser, recording calls.
type mockBeadCloser struct {
	closed []closedBead
	err    error
}

type closedBead struct {
	beadID string
	fields map[string]string
}

// errTestCloseFailed is a transient close failure used to assert the
// create-missing skip is keyed on the closer's actual closes, not agent_state.
var errTestCloseFailed = fmt.Errorf("simulated transient CloseBead failure")

func (m *mockBeadCloser) CloseBead(_ context.Context, beadID string, fields map[string]string) error {
	m.closed = append(m.closed, closedBead{beadID, fields})
	return m.err
}

// mockStateUpdater implements StateUpdater, recording calls.
type mockStateUpdater struct {
	updates []stateUpdate
	err     error
}

type stateUpdate struct {
	beadID string
	state  string
}

func (m *mockStateUpdater) UpdateAgentState(_ context.Context, beadID, state string) error {
	m.updates = append(m.updates, stateUpdate{beadID, state})
	return m.err
}

// mockFailedMarker implements FailedAgentMarker, recording calls.
type mockFailedMarker struct {
	calls []failedCall
	err   error
}

type failedCall struct {
	beadID string
	reason string
}

func (m *mockFailedMarker) MarkAgentFailed(_ context.Context, beadID, reason string) error {
	m.calls = append(m.calls, failedCall{beadID, reason})
	return m.err
}

// mockLister implements beadsapi.BeadLister.
type mockLister struct {
	beads []beadsapi.AgentBead
	err   error
}

func (m *mockLister) ListAgentBeads(_ context.Context) ([]beadsapi.AgentBead, error) {
	return m.beads, m.err
}

// mockManager implements podmanager.Manager, recording all calls.
type mockManager struct {
	pods            []corev1.Pod
	podsByNS        map[string][]corev1.Pod // optional per-namespace list; falls back to pods
	listErr         error
	createErr       error
	deleteErr       error
	created         []podmanager.AgentPodSpec
	deleted         []string          // pod names
	deletedNS       map[string]string // pod name → namespace
	getResult       *corev1.Pod
	getErr          error
	deleteAllCalled bool
	deleteAllForce  bool
	deleteAllResult int
	deleteAllErr    error
	deleteAllNS     []string // namespaces DeleteAllAgentPods was called for

	nodesByName map[string]*corev1.Node // GetNode lookup for node-drain tests
	getNodeErr  error
}

func (m *mockManager) CreateAgentPod(_ context.Context, spec podmanager.AgentPodSpec) error {
	m.created = append(m.created, spec)
	return m.createErr
}

func (m *mockManager) DeleteAgentPod(_ context.Context, name, namespace string) error {
	m.deleted = append(m.deleted, name)
	if m.deletedNS == nil {
		m.deletedNS = make(map[string]string)
	}
	m.deletedNS[name] = namespace
	return m.deleteErr
}

func (m *mockManager) DeleteAllAgentPods(_ context.Context, namespace string, force bool) (int, error) {
	m.deleteAllCalled = true
	m.deleteAllForce = force
	m.deleteAllNS = append(m.deleteAllNS, namespace)
	return m.deleteAllResult, m.deleteAllErr
}

func (m *mockManager) ListAgentPods(_ context.Context, namespace string, _ map[string]string) ([]corev1.Pod, error) {
	if m.podsByNS != nil {
		return m.podsByNS[namespace], m.listErr
	}
	return m.pods, m.listErr
}

func (m *mockManager) GetAgentPod(_ context.Context, name, _ string) (*corev1.Pod, error) {
	if m.getResult != nil {
		return m.getResult, nil
	}
	return nil, m.getErr
}

func (m *mockManager) GetNode(_ context.Context, name string) (*corev1.Node, error) {
	if m.getNodeErr != nil {
		return nil, m.getNodeErr
	}
	if n, ok := m.nodesByName[name]; ok {
		return n, nil
	}
	return nil, apierrors.NewNotFound(corev1.Resource("nodes"), name)
}

// --- Helpers ---

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testConfig(namespace string) *config.Config {
	return &config.Config{
		Namespace:           namespace,
		CoopBurstLimit:      3,
		CoopMaxPods:         0, // unlimited for tests unless overridden
		CoopRateLimitMax:    0, // disabled for tests unless overridden
		CoopRateLimitWindow: 5 * time.Minute,
	}
}

// simpleSpecBuilder returns a SpecBuilder that creates minimal AgentPodSpec values.
func simpleSpecBuilder(image string) SpecBuilder {
	return func(cfg *config.Config, project, mode, role, agentName string, metadata map[string]string) podmanager.AgentPodSpec {
		return podmanager.AgentPodSpec{
			Project:   project,
			Mode:      mode,
			Role:      role,
			AgentName: agentName,
			Image:     image,
			Namespace: cfg.Namespace,
		}
	}
}

// makePod creates a corev1.Pod with the given name, labels, and phase.
func makePod(name, namespace, mode, project, role, agent string, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				podmanager.LabelApp:     podmanager.LabelAppValue,
				podmanager.LabelAgent:   agent,
				podmanager.LabelMode:    mode,
				podmanager.LabelProject: project,
				podmanager.LabelRole:    role,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent", Image: "ghcr.io/org/agent:v1"},
			},
		},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}

// --- Normal reconcile tests ---

func TestReconcile_CreatesPodsForNewBeads(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
			{ID: "bd-2", Project: "proj", Mode: "crew", Role: "dev", AgentName: "beta"},
		},
	}
	mgr := &mockManager{pods: nil} // no existing pods

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 2 {
		t.Fatalf("expected 2 pods created, got %d", len(mgr.created))
	}
	// Verify created specs have correct metadata.
	names := map[string]bool{}
	for _, spec := range mgr.created {
		names[spec.AgentName] = true
		if spec.Image != "img:v1" {
			t.Errorf("expected image img:v1, got %s", spec.Image)
		}
		if spec.Namespace != "ns" {
			t.Errorf("expected namespace ns, got %s", spec.Namespace)
		}
	}
	if !names["alpha"] || !names["beta"] {
		t.Errorf("expected agents alpha and beta, got %v", names)
	}
}

// TestReconcile_RefusesProjectlessBead verifies the fail-closed credential
// backstop (ka-0000007608, guard #2): an agent bead with no project would
// resolve no Claude OAuth credential and spawn a loginless pod, so the
// reconciler must refuse to create it and hard-fail the bead instead.
func TestReconcile_RefusesProjectlessBead(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "", Mode: "crew", Role: "dev", AgentName: "orphan"},
			{ID: "bd-2", Project: "proj", Mode: "crew", Role: "dev", AgentName: "ok"},
		},
	}
	mgr := &mockManager{pods: nil}
	marker := &mockFailedMarker{}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetFailedAgentMarker(marker)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the projected ("ok") pod should be created.
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 pod created, got %d (%v)", len(mgr.created), mgr.created)
	}
	if mgr.created[0].AgentName != "ok" {
		t.Errorf("created pod = %q, want ok", mgr.created[0].AgentName)
	}
	// The project-less bead must have been marked failed.
	if len(marker.calls) != 1 || marker.calls[0].beadID != "bd-1" {
		t.Fatalf("expected bd-1 marked failed, got %v", marker.calls)
	}
	if !strings.Contains(marker.calls[0].reason, "no project") {
		t.Errorf("failure reason = %q, want it to mention 'no project'", marker.calls[0].reason)
	}
}

// TestReconcile_RefusesInvalidHarnessBead verifies the create-missing path
// honours spec.HarnessError (gd-helNySn0WA): a bead whose harness cannot be
// resolved or served by its account must not be realized as a pod (it would
// silently boot a Claude pod for a codex agent); it is hard-failed with the
// harness reason instead, and valid beads in the same pass still spawn.
func TestReconcile_RefusesInvalidHarnessBead(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "badharness"},
			{ID: "bd-2", Project: "proj", Mode: "crew", Role: "dev", AgentName: "ok"},
		},
	}
	mgr := &mockManager{pods: nil}
	marker := &mockFailedMarker{}

	base := simpleSpecBuilder("img:v1")
	builder := func(cfg *config.Config, project, mode, role, agentName string, metadata map[string]string) podmanager.AgentPodSpec {
		spec := base(cfg, project, mode, role, agentName, metadata)
		if agentName == "badharness" {
			spec.HarnessError = `harness "codex" needs an account with provider "openai"`
		}
		return spec
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), builder)
	r.SetFailedAgentMarker(marker)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 1 || mgr.created[0].AgentName != "ok" {
		t.Fatalf("expected only the valid pod created, got %v", mgr.created)
	}
	if len(marker.calls) != 1 || marker.calls[0].beadID != "bd-1" {
		t.Fatalf("expected bd-1 marked failed, got %v", marker.calls)
	}
	if !strings.Contains(marker.calls[0].reason, "invalid agent harness") ||
		!strings.Contains(marker.calls[0].reason, `provider "openai"`) {
		t.Errorf("failure reason = %q, want the invalid-harness reason", marker.calls[0].reason)
	}
}

func TestReconcile_DeletesOrphanPods(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	// Two pods exist, but only one is desired.
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", corev1.PodRunning),
			makePod("crew-proj-dev-orphan", "ns", "crew", "proj", "dev", "orphan", corev1.PodRunning),
		},
	}

	// Use the same image as makePod to avoid drift on the desired pod.
	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.deleted) != 1 {
		t.Fatalf("expected 1 pod deleted, got %d", len(mgr.deleted))
	}
	if mgr.deleted[0] != "crew-proj-dev-orphan" {
		t.Errorf("expected orphan pod deleted, got %s", mgr.deleted[0])
	}
	if len(mgr.created) != 0 {
		t.Errorf("expected 0 pods created, got %d", len(mgr.created))
	}
}

func TestReconcile_RecreatesFailedPods(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", corev1.PodFailed),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should delete the failed pod and create a replacement.
	if len(mgr.deleted) != 1 {
		t.Fatalf("expected 1 pod deleted, got %d", len(mgr.deleted))
	}
	if mgr.deleted[0] != "crew-proj-dev-alpha" {
		t.Errorf("expected failed pod deleted, got %s", mgr.deleted[0])
	}
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 pod created, got %d", len(mgr.created))
	}
}

func TestReconcile_RecreatesSucceededPods(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", corev1.PodSucceeded),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.deleted) != 1 {
		t.Fatalf("expected 1 deletion (terminal pod), got %d", len(mgr.deleted))
	}
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 creation (replacement), got %d", len(mgr.created))
	}
}

func TestReconcile_NoOpWhenConverged(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", corev1.PodRunning),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 0 {
		t.Errorf("expected 0 creations when converged, got %d", len(mgr.created))
	}
	if len(mgr.deleted) != 0 {
		t.Errorf("expected 0 deletions when converged, got %d", len(mgr.deleted))
	}
}

// --- Orphan protection tests ---

func TestReconcile_OrphanProtection_RefusesMassDelete(t *testing.T) {
	// Daemon returns zero beads but pods exist. Should NOT delete on first 2 cycles.
	lister := &mockLister{beads: nil}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", corev1.PodRunning),
			makePod("crew-proj-dev-beta", "ns", "crew", "proj", "dev", "beta", corev1.PodRunning),
			makePod("crew-proj-dev-gamma", "ns", "crew", "proj", "dev", "gamma", corev1.PodRunning),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	// First two cycles: deferred, no deletions.
	for i := 0; i < 2; i++ {
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("cycle %d: unexpected error: %v", i, err)
		}
		if len(mgr.deleted) != 0 {
			t.Errorf("cycle %d: expected 0 deletions, got %d", i, len(mgr.deleted))
		}
	}

	// Third cycle: proceeds with cleanup.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("cycle 3: unexpected error: %v", err)
	}
	if len(mgr.deleted) != 3 {
		t.Errorf("cycle 3: expected 3 deletions, got %d", len(mgr.deleted))
	}
}

func TestReconcile_OrphanProtection_ResetsCounterWhenBeadsReturn(t *testing.T) {
	// If beads reappear after empty cycles, counter resets.
	lister := &mockLister{beads: nil}
	pods := []corev1.Pod{
		makePod("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", corev1.PodRunning),
	}
	mgr := &mockManager{pods: pods}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	// Two empty cycles.
	for i := 0; i < 2; i++ {
		_ = r.Reconcile(context.Background())
	}

	// Beads return — counter should reset.
	lister.beads = []beadsapi.AgentBead{
		{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
	}
	_ = r.Reconcile(context.Background())

	// Go empty again — should need 3 more cycles, not 1.
	lister.beads = nil
	mgr.deleted = nil
	for i := 0; i < 2; i++ {
		_ = r.Reconcile(context.Background())
	}
	if len(mgr.deleted) != 0 {
		t.Errorf("expected 0 deletions after counter reset, got %d", len(mgr.deleted))
	}
}

func TestReconcile_PodNameNoDoublePrefix(t *testing.T) {
	// When agent names include the project as prefix (e.g. "proj-fix-xyz"),
	// the reconciler must strip it so the desired pod name matches the actual
	// pod name built by podmanager.PodName(). Without the TrimPrefix, the
	// desired key would be "crew-proj-dev-proj-fix-xyz" while the real pod is
	// "crew-proj-dev-fix-xyz", causing false orphan deletions.
	// Regression test for 92f57773.
	const img = "img:v1"
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "proj-fix-xyz"},
		},
	}
	// Build the actual pod with the same image the spec builder uses,
	// so drift detection does not trigger a spurious recreate.
	pod := makePod("crew-proj-dev-fix-xyz", "ns", "crew", "proj", "dev", "proj-fix-xyz", corev1.PodRunning)
	pod.Spec.Containers[0].Image = img
	mgr := &mockManager{pods: []corev1.Pod{pod}}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder(img))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Pod should NOT be deleted — it's in the desired state.
	if len(mgr.deleted) != 0 {
		t.Errorf("expected 0 deletions (pod matches desired), got %d: %v", len(mgr.deleted), mgr.deleted)
	}
	// Pod should NOT be (re)created — it already exists.
	if len(mgr.created) != 0 {
		t.Errorf("expected 0 creations (pod already exists), got %d", len(mgr.created))
	}
}

func TestReconcile_OrphanProtection_AllowsDeleteWhenSomeBeadsExist(t *testing.T) {
	// Daemon returns some beads (not zero). Orphan deletion should proceed normally.
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", corev1.PodRunning),
			makePod("crew-proj-dev-orphan", "ns", "crew", "proj", "dev", "orphan", corev1.PodRunning),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.deleted) != 1 {
		t.Fatalf("expected 1 orphan deletion, got %d", len(mgr.deleted))
	}
	if mgr.deleted[0] != "crew-proj-dev-orphan" {
		t.Errorf("expected crew-proj-dev-orphan deleted, got %s", mgr.deleted[0])
	}
}

func TestReconcile_OrphanProtection_EmptyStateIsNoOp(t *testing.T) {
	// Both beads and pods are empty. Nothing to do.
	lister := &mockLister{beads: nil}
	mgr := &mockManager{pods: nil}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 0 {
		t.Errorf("expected 0 creations, got %d", len(mgr.created))
	}
	if len(mgr.deleted) != 0 {
		t.Errorf("expected 0 deletions, got %d", len(mgr.deleted))
	}
}

// --- Freeze/thaw placeholder Pod B tests (ka-0000007549 / ka-0000004146) ---

// makeThawPlaceholderPod builds a placeholder Pod B as FreezeThawManager.Thaw
// would: name "<canonical>-thaw-b", carrying LabelThawPlaceholder=true and the
// agent label. Its spec deliberately differs from a cold-start spec (sleep
// placeholder image) so the drift-exemption assertion is meaningful.
func makeThawPlaceholderPod(canonicalName, namespace, agent string, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      canonicalName + podmanager.ThawPlaceholderSuffix,
			Namespace: namespace,
			Labels: map[string]string{
				podmanager.LabelApp:             podmanager.LabelAppValue,
				podmanager.LabelAgent:           agent,
				podmanager.LabelThawPlaceholder: "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				// Intentionally different from the desired cold-start image so a
				// naive drift check would flag it.
				{Name: "agent", Image: "ghcr.io/org/agent:PLACEHOLDER-sleep"},
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// TestReconcile_ThawPlaceholder_NotDeletedAsOrphan asserts the placeholder Pod B
// (…-thaw-b) is mapped to its canonical name and so is NOT swept as an orphan and
// does NOT trigger a duplicate cold-start. This is the in-flight-thaw race that
// tore down Pod B's netns mid-restore and caused thaw rc=128 (ka-0000004146).
func TestReconcile_ThawPlaceholder_NotDeletedAsOrphan(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	// The only actual pod is the placeholder Pod B for the desired agent.
	mgr := &mockManager{
		pods: []corev1.Pod{
			makeThawPlaceholderPod("crew-proj-dev-alpha", "ns", "alpha", corev1.PodRunning),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.deleted) != 0 {
		t.Errorf("placeholder Pod B must not be deleted; got deletions %v", mgr.deleted)
	}
	if len(mgr.created) != 0 {
		t.Errorf("placeholder Pod B satisfies the desired agent; expected 0 cold-starts, got %d", len(mgr.created))
	}
}

// TestReconcile_WarmGraftPlaceholder_MappedByAnnotation asserts the C2 warm-B
// graft (ka-0000005266): a placeholder grafted from the warm pool keeps its pool
// name ("warm-b-<node>-…"), which the -thaw-b suffix-trim CANNOT map back to the
// canonical agent name. The wake path stamps gardenio.io/canonical-pod at
// promotion; the reconciler must prefer it so the promoted pod satisfies the
// agent's desired entry — not swept as an orphan, no duplicate cold-start. This
// is the warm-B analogue of the ka-0000007549 / ka-0000004146 in-flight race.
func TestReconcile_WarmGraftPlaceholder_MappedByAnnotation(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	warm := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-b-node7-1717-3", // pool name — NOT "<canonical>-thaw-b"
			Namespace: "ns",
			Labels: map[string]string{
				podmanager.LabelApp:             podmanager.LabelAppValue,
				podmanager.LabelAgent:           "alpha",
				podmanager.LabelThawPlaceholder: "true",
			},
			Annotations: map[string]string{
				podmanager.AnnotationCanonicalPod: "crew-proj-dev-alpha",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "ghcr.io/org/agent:PLACEHOLDER-sleep"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	mgr := &mockManager{pods: []corev1.Pod{warm}}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgr.deleted) != 0 {
		t.Errorf("warm-graft pod must not be deleted as orphan; got deletions %v", mgr.deleted)
	}
	if len(mgr.created) != 0 {
		t.Errorf("warm-graft pod satisfies the desired agent; expected 0 cold-starts, got %d", len(mgr.created))
	}
}

// TestReconcile_ThawPlaceholder_TerminalRecreatesByRealName asserts that if a
// placeholder Pod B genuinely reaches a terminal phase, the reconciler deletes it
// by its REAL k8s name (…-thaw-b) — not the canonical key it is mapped under —
// and then cold-starts a replacement.
func TestReconcile_ThawPlaceholder_TerminalRecreatesByRealName(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makeThawPlaceholderPod("crew-proj-dev-alpha", "ns", "alpha", corev1.PodFailed),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.deleted) != 1 || mgr.deleted[0] != "crew-proj-dev-alpha"+podmanager.ThawPlaceholderSuffix {
		t.Errorf("terminal Pod B must be deleted by its real -thaw-b name, got %v", mgr.deleted)
	}
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 replacement cold-start, got %d", len(mgr.created))
	}
	if mgr.created[0].AgentName != "alpha" {
		t.Errorf("replacement must be for agent alpha, got %s", mgr.created[0].AgentName)
	}
}

// TestReconcile_ThawPlaceholder_InFlightThaw_NotReaped asserts the ka-0000002058
// fix (decision ka-0000005300: exempt_reaper): a placeholder Pod B that flips to
// phase=Failed seconds after the restore graft is NOT deleted by the terminal-pod
// reaper while its thaw is still in flight (IsThawing true). Reaping it would tear
// down the netns under the in-pod watchers mid-rehydration and fail-close the thaw.
func TestReconcile_ThawPlaceholder_InFlightThaw_NotReaped(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			// Pod B is terminal (Failed) — the post-restore-graft signature — but
			// the restored sandbox is live and rehydrating.
			makeThawPlaceholderPod("crew-proj-dev-alpha", "ns", "alpha", corev1.PodFailed),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	// The thaw for alpha is in flight: the reaper must stand off.
	r.SetThawChecker(func(agentName string) bool { return agentName == "alpha" })

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.deleted) != 0 {
		t.Errorf("in-flight thaw Pod B must not be reaped; got deletions %v", mgr.deleted)
	}
	if len(mgr.created) != 0 {
		t.Errorf("in-flight thaw Pod B must not trigger a cold-start; got %d creations", len(mgr.created))
	}
}

// TestReconcile_ThawPlaceholder_ThawDone_ReapedNormally asserts the exemption is
// narrow: once the thaw is no longer in flight (IsThawing false — e.g. the thaw
// fail-closed and tore down, or the controller restarted), a terminal Pod B that
// somehow survives is reaped normally by its real -thaw-b name and a replacement
// cold-starts. This proves the exemption cannot permanently leak an abandoned Pod B.
func TestReconcile_ThawPlaceholder_ThawDone_ReapedNormally(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makeThawPlaceholderPod("crew-proj-dev-alpha", "ns", "alpha", corev1.PodFailed),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	// No thaw in flight for alpha — the reaper proceeds as before.
	r.SetThawChecker(func(string) bool { return false })

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.deleted) != 1 || mgr.deleted[0] != "crew-proj-dev-alpha"+podmanager.ThawPlaceholderSuffix {
		t.Errorf("terminal Pod B with no in-flight thaw must be reaped by its real -thaw-b name, got %v", mgr.deleted)
	}
	if len(mgr.created) != 1 {
		t.Errorf("expected 1 replacement cold-start, got %d", len(mgr.created))
	}
}

// --- Burst limiting tests ---

func TestReconcile_BurstLimit_CapsCreationsPerPass(t *testing.T) {
	beads := make([]beadsapi.AgentBead, 5)
	for i := range beads {
		beads[i] = beadsapi.AgentBead{
			ID:        fmt.Sprintf("bd-%d", i),
			Project:   "proj",
			Mode:      "crew",
			Role:      "dev",
			AgentName: fmt.Sprintf("agent%d", i),
		}
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 2

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 2 {
		t.Errorf("expected burst limit of 2 pods created, got %d", len(mgr.created))
	}
}

func TestReconcile_BurstLimit_DefaultsToThreeWhenZero(t *testing.T) {
	beads := make([]beadsapi.AgentBead, 5)
	for i := range beads {
		beads[i] = beadsapi.AgentBead{
			ID:        fmt.Sprintf("bd-%d", i),
			Project:   "proj",
			Mode:      "crew",
			Role:      "dev",
			AgentName: fmt.Sprintf("agent%d", i),
		}
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 0 // should default to 3

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 3 {
		t.Errorf("expected default burst limit of 3 pods created, got %d", len(mgr.created))
	}
}

func TestReconcile_MaxPods_CapsActiveCount(t *testing.T) {
	beads := make([]beadsapi.AgentBead, 5)
	for i := range beads {
		beads[i] = beadsapi.AgentBead{
			ID:        fmt.Sprintf("bd-%d", i),
			Project:   "proj",
			Mode:      "crew",
			Role:      "dev",
			AgentName: fmt.Sprintf("agent%d", i),
		}
	}
	lister := &mockLister{beads: beads}

	// 2 pods already exist and are running.
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-agent0", "ns", "crew", "proj", "dev", "agent0", corev1.PodRunning),
			makePod("crew-proj-dev-agent1", "ns", "crew", "proj", "dev", "agent1", corev1.PodRunning),
		},
	}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10 // high burst limit
	cfg.CoopMaxPods = 3     // only 3 total allowed

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 2 active + 1 new = 3 (the max). Should only create 1.
	if len(mgr.created) != 1 {
		t.Errorf("expected 1 pod created (max pods cap), got %d", len(mgr.created))
	}
}

func TestReconcile_MaxPods_FIFOOrdering(t *testing.T) {
	now := time.Now()
	// Create 4 beads with staggered creation times. The oldest should be
	// created first when the concurrency cap is reached.
	beads := []beadsapi.AgentBead{
		{ID: "bd-new", Project: "proj", Mode: "crew", Role: "dev", AgentName: "newest", CreatedAt: now},
		{ID: "bd-old", Project: "proj", Mode: "crew", Role: "dev", AgentName: "oldest", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "bd-mid", Project: "proj", Mode: "crew", Role: "dev", AgentName: "middle", CreatedAt: now.Add(-30 * time.Minute)},
	}
	lister := &mockLister{beads: beads}

	// No existing pods.
	mgr := &mockManager{}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 2 // only 2 allowed — one bead will be deferred

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should create exactly 2 pods (the cap).
	if len(mgr.created) != 2 {
		t.Fatalf("expected 2 pods created, got %d", len(mgr.created))
	}

	// The 2 created pods should be the oldest beads (FIFO).
	createdNames := make(map[string]bool)
	for _, spec := range mgr.created {
		createdNames[spec.AgentName] = true
	}
	if !createdNames["oldest"] {
		t.Error("expected oldest agent to be created first, but it was deferred")
	}
	if !createdNames["middle"] {
		t.Error("expected middle agent to be created, but it was deferred")
	}
	if createdNames["newest"] {
		t.Error("expected newest agent to be deferred, but it was created")
	}
}

// ── Queue age warning tests ──────────────────────────────────────────────────

func TestReconcile_WarnsOnStaleDeferredAgents(t *testing.T) {
	now := time.Now()
	// Create 4 beads: 1 already running, 2 old waiting, 1 recent waiting.
	// The running bead occupies a slot, leaving room for 1 more (max=2).
	// The oldest waiting bead gets created; the other 2 are deferred.
	beads := []beadsapi.AgentBead{
		{ID: "bd-run", Project: "proj", Mode: "crew", Role: "dev", AgentName: "running", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "bd-old1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "old1", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "bd-old2", Project: "proj", Mode: "crew", Role: "dev", AgentName: "old2", CreatedAt: now.Add(-90 * time.Minute)},
		{ID: "bd-new", Project: "proj", Mode: "crew", Role: "dev", AgentName: "newagent", CreatedAt: now.Add(-5 * time.Minute)},
	}
	lister := &mockLister{beads: beads}

	// "running" pod already exists and matches a desired bead.
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-running", "ns", "crew", "proj", "dev", "running", corev1.PodRunning),
		},
	}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 2 // 1 existing + 1 new = 2, rest deferred

	// Use a logger that captures log records so we can verify warnings.
	var buf logBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	r := New(lister, mgr, cfg, logger, simpleSpecBuilder("ghcr.io/org/agent:v1"))
	r.SetTunables(&stubTunables{warnAge: 1 * time.Hour})
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only 1 pod should be created (oldest waiting: old1).
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 pod created, got %d", len(mgr.created))
	}

	// old2 (90m) and newagent (5m) are deferred.
	// old2 exceeds the 1h threshold; newagent does not.
	logs := buf.String()
	if !containsSubstring(logs, "deferred agent exceeds queue age threshold") {
		t.Error("expected warning about stale deferred agent, but none found in logs")
	}
	if !containsSubstring(logs, "stale_count=") {
		t.Error("expected stale deferred agents summary warning, but none found in logs")
	}
}

func TestReconcile_NoWarningWhenQueueAgeDisabled(t *testing.T) {
	now := time.Now()
	beads := []beadsapi.AgentBead{
		{ID: "bd-run", Project: "proj", Mode: "crew", Role: "dev", AgentName: "running", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "bd-old", Project: "proj", Mode: "crew", Role: "dev", AgentName: "old1", CreatedAt: now.Add(-2 * time.Hour)},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-running", "ns", "crew", "proj", "dev", "running", corev1.PodRunning),
		},
	}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 1 // full — old1 will be deferred

	var buf logBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	r := New(lister, mgr, cfg, logger, simpleSpecBuilder("ghcr.io/org/agent:v1"))
	r.SetTunables(&stubTunables{warnAge: 0}) // disabled
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logs := buf.String()
	if containsSubstring(logs, "deferred agent exceeds queue age threshold") {
		t.Error("expected no stale warning when tunable is 0, but found one")
	}
}

// stubTunables satisfies reconciler.Tunables with a fixed COOP_QUEUED_WARN_AGE.
// Other keys fall through to the compiled default. Used by the warn-age
// tests that previously set cfg.CoopQueuedWarnAge directly.
type stubTunables struct {
	warnAge time.Duration
}

func (s *stubTunables) GetDuration(key, envVar string, compiled time.Duration) time.Duration {
	if key == "COOP_QUEUED_WARN_AGE" {
		return s.warnAge
	}
	return compiled
}

// logBuffer is a simple thread-safe bytes buffer for capturing log output.
type logBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && strings.Contains(s, sub))
}

// ── Autodestruct ────────────────────────────────────────────────────────────

func TestAutodestruct(t *testing.T) {
	mgr := &mockManager{deleteAllResult: 5}
	lister := &mockLister{}
	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	deleted, err := r.Autodestruct(context.Background(), "operator")
	if err != nil {
		t.Fatalf("Autodestruct: %v", err)
	}
	if deleted != 5 {
		t.Errorf("deleted = %d, want 5", deleted)
	}
	if !mgr.deleteAllCalled {
		t.Error("expected DeleteAllAgentPods to be called")
	}
	if !mgr.deleteAllForce {
		t.Error("expected force=true")
	}
	if !r.IsDestructed() {
		t.Error("expected IsDestructed() = true after Autodestruct")
	}
}

func TestReconcile_SkipsWhenDestructed(t *testing.T) {
	mgr := &mockManager{deleteAllResult: 0}
	lister := &mockLister{beads: []beadsapi.AgentBead{
		{ID: "b1", Project: "gardenio", Mode: "crew", Role: "dev", AgentName: "a1"},
	}}
	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	// Activate autodestruct.
	_, _ = r.Autodestruct(context.Background(), "test")

	// Reconcile should be a no-op.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(mgr.created) != 0 {
		t.Errorf("expected 0 pods created during autodestruct, got %d", len(mgr.created))
	}
}

func TestClearAutodestruct(t *testing.T) {
	mgr := &mockManager{deleteAllResult: 0}
	lister := &mockLister{}
	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	_, _ = r.Autodestruct(context.Background(), "operator")
	if !r.IsDestructed() {
		t.Fatal("expected IsDestructed() = true")
	}

	r.ClearAutodestruct("operator")
	if r.IsDestructed() {
		t.Error("expected IsDestructed() = false after ClearAutodestruct")
	}
}

func TestIsDestructed_DefaultFalse(t *testing.T) {
	mgr := &mockManager{}
	lister := &mockLister{}
	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	if r.IsDestructed() {
		t.Error("expected IsDestructed() = false by default")
	}
}

// ── Rate limiter integration ────────────────────────────────────────────────

func TestReconcile_RateLimiter_StopsCreationWhenExceeded(t *testing.T) {
	// 10 beads want pods, rate limit is 2 per window, burst limit is high.
	beads := make([]beadsapi.AgentBead, 10)
	for i := range beads {
		beads[i] = beadsapi.AgentBead{
			ID:        fmt.Sprintf("bd-%d", i),
			Project:   "proj",
			Mode:      "crew",
			Role:      "dev",
			AgentName: fmt.Sprintf("agent%d", i),
		}
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10  // high burst limit
	cfg.CoopRateLimitMax = 2 // only 2 per window
	cfg.CoopRateLimitWindow = 5 * time.Minute

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should create exactly 2 pods (rate limit), then stop.
	if len(mgr.created) != 2 {
		t.Errorf("expected 2 pods created (rate limited), got %d", len(mgr.created))
	}
}

func TestReconcile_RateLimiter_Disabled(t *testing.T) {
	// With rate limit max=0 (disabled), burst limit controls creation.
	beads := make([]beadsapi.AgentBead, 5)
	for i := range beads {
		beads[i] = beadsapi.AgentBead{
			ID:        fmt.Sprintf("bd-%d", i),
			Project:   "proj",
			Mode:      "crew",
			Role:      "dev",
			AgentName: fmt.Sprintf("agent%d", i),
		}
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopRateLimitMax = 0 // disabled

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 5 {
		t.Errorf("expected 5 pods created (no rate limit), got %d", len(mgr.created))
	}
}

func TestReconcile_DeferredMetrics_MaxPods(t *testing.T) {
	now := time.Now()
	beads := []beadsapi.AgentBead{
		{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "a1", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "bd-2", Project: "proj", Mode: "crew", Role: "dev", AgentName: "a2", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "bd-3", Project: "proj", Mode: "crew", Role: "dev", AgentName: "a3", CreatedAt: now.Add(-30 * time.Minute)},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 1 // only 1 allowed — 2 deferred

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 pod created, got %d", len(mgr.created))
	}

	// Verify deferred gauge incremented for the 2 deferred agents.
	dm := r.DeferredMetrics()
	g, err := dm.DeferredTotal.GetMetricWithLabelValues("proj", "max_pods")
	if err != nil {
		t.Fatalf("getting deferred gauge: %v", err)
	}
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("writing metric: %v", err)
	}
	if got := m.GetGauge().GetValue(); got != 2 {
		t.Errorf("expected deferred_total=2, got %v", got)
	}
}

// ── Queue expiry tests ───────────────────────────────────────────────────────

func TestReconcile_QueueExpiry_ClosesExpiredBeads(t *testing.T) {
	beads := []beadsapi.AgentBead{
		{ID: "bd-0", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent0", CreatedAt: time.Now().Add(-3 * time.Hour)},
		{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent1", CreatedAt: time.Now().Add(-2 * time.Hour)},
		{ID: "bd-2", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent2", CreatedAt: time.Now().Add(-1 * time.Hour)},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 1
	cfg.CoopQueueExpiryTimeout = 30 * time.Minute

	closer := &mockBeadCloser{}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if len(mgr.created) != 1 {
		t.Fatalf("pass 1: expected 1 created, got %d", len(mgr.created))
	}
	if len(closer.closed) != 0 {
		t.Fatalf("pass 1: expected 0 closed, got %d", len(closer.closed))
	}

	for beadID := range r.deferredSince {
		r.deferredSince[beadID] = time.Now().Add(-45 * time.Minute)
	}

	mgr.created = nil
	mgr.pods = []corev1.Pod{
		makePod("crew-proj-dev-agent0", "ns", "crew", "proj", "dev", "agent0", corev1.PodRunning),
	}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if len(closer.closed) != 2 {
		t.Fatalf("expected 2 beads auto-closed, got %d", len(closer.closed))
	}
	for _, c := range closer.closed {
		if c.fields["agent_state"] != "expired" {
			t.Errorf("expected agent_state=expired, got %q for bead %s", c.fields["agent_state"], c.beadID)
		}
		if c.fields["close_reason"] == "" {
			t.Errorf("expected non-empty close_reason for bead %s", c.beadID)
		}
	}
}

func TestReconcile_QueueExpiry_ClearsTrackingWhenPodCreated(t *testing.T) {
	beads := []beadsapi.AgentBead{
		{ID: "bd-0", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent0"},
		{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent1"},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 1
	cfg.CoopQueueExpiryTimeout = time.Hour

	closer := &mockBeadCloser{}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if len(r.deferredSince) != 1 {
		t.Fatalf("expected 1 deferred bead tracked, got %d", len(r.deferredSince))
	}

	cfg.CoopBurstLimit = 10
	mgr.pods = []corev1.Pod{
		makePod("crew-proj-dev-agent0", "ns", "crew", "proj", "dev", "agent0", corev1.PodRunning),
	}
	mgr.created = nil

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if len(r.deferredSince) != 0 {
		t.Fatalf("expected 0 deferred beads after creation, got %d", len(r.deferredSince))
	}
	if len(closer.closed) != 0 {
		t.Fatalf("expected 0 closed (not expired), got %d", len(closer.closed))
	}
}

func TestReconcile_QueueExpiry_DisabledWhenZeroTimeout(t *testing.T) {
	beads := []beadsapi.AgentBead{
		{ID: "bd-0", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent0"},
		{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent1"},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 1
	cfg.CoopQueueExpiryTimeout = 0

	closer := &mockBeadCloser{}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}

	for beadID := range r.deferredSince {
		r.deferredSince[beadID] = time.Now().Add(-24 * time.Hour)
	}

	mgr.pods = []corev1.Pod{
		makePod("crew-proj-dev-agent0", "ns", "crew", "proj", "dev", "agent0", corev1.PodRunning),
	}
	mgr.created = nil

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if len(closer.closed) != 0 {
		t.Fatalf("expected 0 closed with timeout disabled, got %d", len(closer.closed))
	}
}

func TestReconcile_QueueExpiry_NilCloserDoesNotPanic(t *testing.T) {
	beads := []beadsapi.AgentBead{
		{ID: "bd-0", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent0"},
		{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent1"},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 1
	cfg.CoopQueueExpiryTimeout = time.Hour

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 created, got %d", len(mgr.created))
	}
}

// ── Queued state visibility ──────────────────────────────────────────────────

func TestReconcile_MaxPods_MarksDeferredAsQueued(t *testing.T) {
	// 5 beads, 2 already running, max 3 pods → 1 created, 2 deferred.
	beads := []beadsapi.AgentBead{
		{ID: "bd-0", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent0"},
		{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent1"},
		{ID: "bd-2", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent2"},
		{ID: "bd-3", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent3"},
		{ID: "bd-4", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent4"},
	}
	lister := &mockLister{beads: beads}

	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-agent0", "ns", "crew", "proj", "dev", "agent0", corev1.PodRunning),
			makePod("crew-proj-dev-agent1", "ns", "crew", "proj", "dev", "agent1", corev1.PodRunning),
		},
	}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 3

	updater := &mockStateUpdater{}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	r.SetStateUpdater(updater)

	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1 pod created (2 active + 1 = 3 max), 2 deferred.
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 pod created, got %d", len(mgr.created))
	}

	// The 2 deferred beads should have been marked as queued.
	if len(updater.updates) != 2 {
		t.Fatalf("expected 2 state updates (queued), got %d", len(updater.updates))
	}
	for _, u := range updater.updates {
		if u.state != "queued" {
			t.Errorf("expected state 'queued', got %q for bead %s", u.state, u.beadID)
		}
	}
}

func TestReconcile_BurstLimit_MarksDeferredAsQueued(t *testing.T) {
	beads := make([]beadsapi.AgentBead, 5)
	for i := range beads {
		beads[i] = beadsapi.AgentBead{
			ID:        fmt.Sprintf("bd-%d", i),
			Project:   "proj",
			Mode:      "crew",
			Role:      "dev",
			AgentName: fmt.Sprintf("agent%d", i),
		}
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 2

	updater := &mockStateUpdater{}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetStateUpdater(updater)

	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 2 {
		t.Fatalf("expected 2 pods created, got %d", len(mgr.created))
	}

	// 3 deferred beads should be marked queued.
	if len(updater.updates) != 3 {
		t.Fatalf("expected 3 state updates (queued), got %d", len(updater.updates))
	}
	for _, u := range updater.updates {
		if u.state != "queued" {
			t.Errorf("expected state 'queued', got %q for bead %s", u.state, u.beadID)
		}
	}
}

func TestReconcile_DeferredMetrics_BurstLimit(t *testing.T) {
	beads := make([]beadsapi.AgentBead, 5)
	for i := range beads {
		beads[i] = beadsapi.AgentBead{
			ID:        fmt.Sprintf("bd-%d", i),
			Project:   "proj",
			Mode:      "crew",
			Role:      "dev",
			AgentName: fmt.Sprintf("a%d", i),
			CreatedAt: time.Now().Add(-time.Duration(5-i) * time.Hour),
		}
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 2 // only 2 per pass
	cfg.CoopMaxPods = 100

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 2 {
		t.Fatalf("expected 2 pods created, got %d", len(mgr.created))
	}

	// 3 deferred due to burst limit.
	dm := r.DeferredMetrics()
	g, err := dm.DeferredTotal.GetMetricWithLabelValues("proj", "burst_limit")
	if err != nil {
		t.Fatalf("getting deferred gauge: %v", err)
	}
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("writing metric: %v", err)
	}
	if got := m.GetGauge().GetValue(); got != 3 {
		t.Errorf("expected deferred_total=3, got %v", got)
	}
}

func TestReconcile_NoStateUpdater_DoesNotPanic(t *testing.T) {
	// Ensure reconciler works fine without a state updater configured.
	beads := make([]beadsapi.AgentBead, 5)
	for i := range beads {
		beads[i] = beadsapi.AgentBead{
			ID:        fmt.Sprintf("bd-%d", i),
			Project:   "proj",
			Mode:      "crew",
			Role:      "dev",
			AgentName: fmt.Sprintf("agent%d", i),
		}
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 2
	// No SetStateUpdater call — stateUpdater is nil.

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 2 {
		t.Fatalf("expected 2 pods created, got %d", len(mgr.created))
	}
}

// ── Podless leak detection ───────────────────────────────────────────────────

func TestReconcile_ClosesLeakedBeadsWithTerminalStateAndNoPod(t *testing.T) {
	// A bead with agent_state="done" but no corresponding pod is a leak.
	// The reconciler should close it immediately.
	beads := []beadsapi.AgentBead{
		{
			ID:         "bd-done-leak",
			Project:    "proj",
			Mode:       "crew",
			Role:       "dev",
			AgentName:  "done-leak",
			AgentState: "done",
		},
		{
			ID:         "bd-failed-leak",
			Project:    "proj",
			Mode:       "crew",
			Role:       "dev",
			AgentName:  "failed-leak",
			AgentState: "failed",
		},
		{
			ID:         "bd-healthy",
			Project:    "proj",
			Mode:       "crew",
			Role:       "dev",
			AgentName:  "healthy",
			AgentState: "working",
		},
	}
	lister := &mockLister{beads: beads}

	// Only the "healthy" bead has a pod.
	healthyPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crew-proj-dev-healthy",
			Namespace: "ns",
			Labels: map[string]string{
				podmanager.LabelApp:     podmanager.LabelAppValue,
				podmanager.LabelAgent:   "healthy",
				podmanager.LabelProject: "proj",
				podmanager.LabelRole:    "dev",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	mgr := &mockManager{pods: []corev1.Pod{healthyPod}}

	closer := &mockBeadCloser{}
	cfg := testConfig("ns")

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The two leaked beads (done, failed) should be closed.
	if len(closer.closed) != 2 {
		t.Fatalf("expected 2 leaked beads closed, got %d", len(closer.closed))
	}
	closedIDs := map[string]bool{}
	for _, c := range closer.closed {
		closedIDs[c.beadID] = true
		if !strings.Contains(c.fields["close_reason"], "podless_leak") {
			t.Errorf("expected podless_leak close_reason, got %q", c.fields["close_reason"])
		}
	}
	if !closedIDs["bd-done-leak"] || !closedIDs["bd-failed-leak"] {
		t.Errorf("expected bd-done-leak and bd-failed-leak to be closed, got %v", closedIDs)
	}
}

// TestReconcile_PreservesRateLimitedStateOnPodlessLeakClose locks in the
// disposition the leaked-bead closer must write for a rate_limited agent with no
// pod: agent_state STAYS "rate_limited" (it is NOT rewritten to "done") and
// close_reason carries the podless_leak signature. This is by design — the Slack
// agent card's terminalCardState maps preserved agent_state=rate_limited to an
// honest ":warning: rate limited" glyph instead of a false ✅ "done"
// (ka-0000005513). A rate_limited→done rewrite here would regress that fix and
// repaint exhausted-account cycles green. This is the controller-side anchor for
// the cred-rotation-resume E2E module's Scenario 3(a) (gd-lAV87rAnjt), whose
// agent_state readback was softened to accept this disposition rather than
// asserting "done".
func TestReconcile_PreservesRateLimitedStateOnPodlessLeakClose(t *testing.T) {
	beads := []beadsapi.AgentBead{
		{
			ID:         "bd-rate-limited-leak",
			Project:    "proj",
			Mode:       "crew",
			Role:       "dev",
			AgentName:  "rate-limited-leak",
			AgentState: "rate_limited",
		},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{} // no pods — the rate_limited agent's pod is gone

	closer := &mockBeadCloser{}
	cfg := testConfig("ns")

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(closer.closed) != 1 {
		t.Fatalf("expected 1 leaked bead closed, got %d", len(closer.closed))
	}
	c := closer.closed[0]
	if c.beadID != "bd-rate-limited-leak" {
		t.Errorf("expected bd-rate-limited-leak closed, got %q", c.beadID)
	}
	// agent_state must be PRESERVED as rate_limited, never coerced to done.
	if got := c.fields["agent_state"]; got != "rate_limited" {
		t.Errorf("expected preserved agent_state=rate_limited, got %q", got)
	}
	if !strings.Contains(c.fields["close_reason"], "podless_leak") {
		t.Errorf("expected podless_leak close_reason, got %q", c.fields["close_reason"])
	}
	if !strings.Contains(c.fields["close_reason"], "rate_limited") {
		t.Errorf("expected close_reason to record agent_state=rate_limited, got %q", c.fields["close_reason"])
	}
}

// TestReconcile_PodlessTerminalLeakNotRecreatedSamePass locks in that a podless
// bead the leaked-bead closer closes this pass (agent_state done/rate_limited/
// stopping — the states the closer acts on beyond "failed") is NOT then
// cold-started again in the same create-missing loop. Before the fix, only the
// "failed" state was skipped, so a done/rate_limited/stopping podless leak was
// closed AND recreated in one pass — spawning a live pod behind a now-closed
// bead that the next pass orphan-reaps (pointless create→delete churn every
// cycle, and for rate_limited a spawn onto a known-exhausted account).
//
// Fault-injection: with the pre-fix create-missing guard (only `AgentState ==
// "failed"` skipped), this asserts `created == 0` and fails with the three
// recreated pods; with the fix (skip everything the closer closed this pass) it
// passes.
func TestReconcile_PodlessTerminalLeakNotRecreatedSamePass(t *testing.T) {
	beads := []beadsapi.AgentBead{
		{ID: "bd-done", Project: "proj", Mode: "crew", Role: "dev", AgentName: "adone", AgentState: "done"},
		{ID: "bd-rl", Project: "proj", Mode: "crew", Role: "dev", AgentName: "arl", AgentState: "rate_limited"},
		{ID: "bd-stop", Project: "proj", Mode: "crew", Role: "dev", AgentName: "astop", AgentState: "stopping"},
		{ID: "bd-failed", Project: "proj", Mode: "crew", Role: "dev", AgentName: "afailed", AgentState: "failed"},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{} // no pods → every terminal bead is a podless leak

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10

	closer := &mockBeadCloser{}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// All four terminal podless beads are closed as leaks.
	if len(closer.closed) != 4 {
		t.Fatalf("expected 4 leaked beads closed, got %d", len(closer.closed))
	}
	// None of them are cold-started again this pass.
	if len(mgr.created) != 0 {
		var got []string
		for _, c := range mgr.created {
			got = append(got, c.BeadID)
		}
		t.Errorf("expected 0 pods created for just-closed podless leaks, got %d: %v", len(mgr.created), got)
	}
}

// TestReconcile_PodlessLeakCloseFailureStillAttemptsCreate verifies the guard is
// keyed on the closer's ACTUAL closes, not on agent_state: when CloseBead fails
// transiently, the bead was NOT closed, so the create-missing loop must still be
// free to (re)create it rather than silently dropping it because of its terminal
// state. "failed" stays skipped by its own pre-existing guard; done/rate_limited
// are recreated (matching the pre-existing behavior for a done bead the closer
// couldn't retire).
func TestReconcile_PodlessLeakCloseFailureStillAttemptsCreate(t *testing.T) {
	beads := []beadsapi.AgentBead{
		{ID: "bd-done", Project: "proj", Mode: "crew", Role: "dev", AgentName: "adone", AgentState: "done"},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10

	closer := &mockBeadCloser{err: errTestCloseFailed}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Close was attempted but failed → bead is still "live" this pass, so the
	// create-missing loop recreates it (the same auto-restart it always did when
	// the closer couldn't retire a done bead).
	if len(mgr.created) != 1 {
		t.Errorf("expected 1 pod created when close failed, got %d", len(mgr.created))
	}
}

func TestReconcile_ExemptsFrozenAwaitingThawAgentFromReaper(t *testing.T) {
	// A frozen-awaiting-thaw agent legitimately has agent_state="done" and no
	// pod (Pod A was checkpointed and torn down) but has a persisted snapshot.
	// The reaper must NOT close it (bug ka-0000000114) — closing would race the
	// in-flight thaw. A terminal-state agent WITHOUT a snapshot is still a leak.
	beads := []beadsapi.AgentBead{
		{
			ID:         "bd-frozen",
			Project:    "proj",
			Mode:       "crew",
			Role:       "dev",
			AgentName:  "frozen-agent",
			AgentState: "done",
		},
		{
			ID:         "bd-real-leak",
			Project:    "proj",
			Mode:       "crew",
			Role:       "dev",
			AgentName:  "leak-agent",
			AgentState: "done",
		},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{} // no pods

	closer := &mockBeadCloser{}
	cfg := testConfig("ns")

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)
	// Only "frozen-agent" has a snapshot.
	r.SetSnapshotChecker(func(_ context.Context, agentName string) bool {
		return agentName == "frozen-agent"
	})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the real leak (no snapshot) should be closed; the frozen agent is exempt.
	if len(closer.closed) != 1 {
		t.Fatalf("expected 1 leaked bead closed, got %d", len(closer.closed))
	}
	if closer.closed[0].beadID != "bd-real-leak" {
		t.Errorf("expected bd-real-leak closed (frozen agent exempt), got %q", closer.closed[0].beadID)
	}
}

func TestReconcile_SkipsCreateMissingForFrozenAwaitingThawAgent(t *testing.T) {
	// A frozen-awaiting-thaw agent is terminal+podless WITH a snapshot; the wake
	// path (AgentSpawn -> ft.thaw) owns its re-creation through placeholder Pod B.
	// The create-missing path must NOT cold-start a duplicate under the canonical
	// name — that defeats the in-flight thaw's teardown-barrier (ka-0000006427,
	// decision ka-0000004662: fix_race_too). An agent WITHOUT a snapshot is a
	// normal desired pod and IS created.
	beads := []beadsapi.AgentBead{
		{ID: "bd-frozen", Project: "proj", Mode: "crew", Role: "dev", AgentName: "frozen-agent", AgentState: "done"},
		{ID: "bd-normal", Project: "proj", Mode: "crew", Role: "dev", AgentName: "normal-agent", AgentState: "in_progress"},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{} // no pods exist
	cfg := testConfig("ns")

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	// Only the frozen agent has a snapshot.
	r.SetSnapshotChecker(func(_ context.Context, agentName string) bool {
		return agentName == "frozen-agent"
	})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Exactly the normal agent's pod is created; the frozen agent is exempt.
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 pod created (normal agent only), got %d", len(mgr.created))
	}
	for _, spec := range mgr.created {
		if spec.AgentName == "frozen-agent" {
			t.Errorf("frozen-awaiting-thaw agent was cold-started by create-missing (defeats thaw); should be exempt")
		}
	}
}

func TestReconcile_ExemptsStoppingFrozenAgentFromReaper(t *testing.T) {
	// Post bug ka-0000007439: a SUCCESSFULLY-frozen agent rests OPEN at
	// agent_state=stopping (the controller no longer overwrites it to "done").
	// The leaked-bead reaper must exempt a podless stopping bead that still has a
	// snapshot exactly as it does the terminal states — closing it would race the
	// in-flight thaw. A stopping bead WITHOUT a snapshot is still a genuine leak.
	beads := []beadsapi.AgentBead{
		{ID: "bd-frozen-stopping", Project: "proj", Mode: "crew", Role: "dev", AgentName: "frozen-agent", AgentState: "stopping"},
		{ID: "bd-stopping-leak", Project: "proj", Mode: "crew", Role: "dev", AgentName: "leak-agent", AgentState: "stopping"},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{} // no pods

	closer := &mockBeadCloser{}
	cfg := testConfig("ns")

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)
	// Only "frozen-agent" has a snapshot.
	r.SetSnapshotChecker(func(_ context.Context, agentName string) bool {
		return agentName == "frozen-agent"
	})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the snapshotless stopping bead is closed; the frozen one is exempt.
	if len(closer.closed) != 1 {
		t.Fatalf("expected 1 leaked bead closed, got %d", len(closer.closed))
	}
	if closer.closed[0].beadID != "bd-stopping-leak" {
		t.Errorf("expected bd-stopping-leak closed (frozen stopping agent exempt), got %q", closer.closed[0].beadID)
	}
}

func TestReconcile_SkipsCreateMissingForStoppingFrozenAgent(t *testing.T) {
	// Post bug ka-0000007439: a frozen agent resting at agent_state=stopping WITH
	// a snapshot must NOT be cold-started by create-missing — the wake path
	// (AgentSpawn -> ft.thaw) owns its re-creation, and a duplicate cold pod
	// re-triggers the re-drive race (ka-0000006427). A normal desired agent (no
	// snapshot) IS created as usual.
	beads := []beadsapi.AgentBead{
		{ID: "bd-frozen", Project: "proj", Mode: "crew", Role: "dev", AgentName: "frozen-agent", AgentState: "stopping"},
		{ID: "bd-normal", Project: "proj", Mode: "crew", Role: "dev", AgentName: "normal-agent", AgentState: "in_progress"},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{} // no pods exist
	cfg := testConfig("ns")

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	// Only the frozen agent has a snapshot.
	r.SetSnapshotChecker(func(_ context.Context, agentName string) bool {
		return agentName == "frozen-agent"
	})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Exactly the snapshotless agent's pod is created; the frozen one is exempt.
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 pod created (normal agent only), got %d", len(mgr.created))
	}
	for _, spec := range mgr.created {
		if spec.AgentName == "frozen-agent" {
			t.Errorf("frozen stopping agent was cold-started by create-missing (defeats thaw); should be exempt")
		}
	}
}

func TestReconcile_NilSnapshotCheckerCreatesMissingAsBefore(t *testing.T) {
	// With no snapshot checker wired (freeze/thaw disabled), create-missing
	// behaves exactly as before — the exemption is opt-in and never blocks a
	// normal cold start.
	beads := []beadsapi.AgentBead{
		{ID: "bd-a", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent-a", AgentState: "in_progress"},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{}
	cfg := testConfig("ns")
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	// No SetSnapshotChecker — hasSnapshot stays nil.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 pod created (no snapshot checker), got %d", len(mgr.created))
	}
}

func TestReconcile_NilSnapshotCheckerClosesLeaksAsBefore(t *testing.T) {
	// With no snapshot checker wired (freeze/thaw disabled), the reaper closes
	// terminal-state podless beads exactly as before — the exemption is opt-in.
	beads := []beadsapi.AgentBead{
		{
			ID:         "bd-leak",
			Project:    "proj",
			Mode:       "crew",
			Role:       "dev",
			AgentName:  "leak-agent",
			AgentState: "done",
		},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{}

	closer := &mockBeadCloser{}
	cfg := testConfig("ns")

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)
	// SetSnapshotChecker deliberately NOT called.

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(closer.closed) != 1 || closer.closed[0].beadID != "bd-leak" {
		t.Fatalf("expected bd-leak closed, got %v", closer.closed)
	}
}

func TestReconcile_DoesNotCloseNewBeadsWithoutPod(t *testing.T) {
	// A brand new bead with empty agent_state and no pod is NOT a leak —
	// the reconciler will create the pod in this same pass.
	beads := []beadsapi.AgentBead{
		{
			ID:         "bd-new",
			Project:    "proj",
			Mode:       "crew",
			Role:       "dev",
			AgentName:  "new-agent",
			AgentState: "", // not yet set
		},
		{
			ID:         "bd-queued",
			Project:    "proj",
			Mode:       "crew",
			Role:       "dev",
			AgentName:  "queued-agent",
			AgentState: "queued",
		},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil} // no pods yet

	closer := &mockBeadCloser{}
	cfg := testConfig("ns")

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Neither bead should be closed — they're not leaks.
	if len(closer.closed) != 0 {
		t.Fatalf("expected 0 beads closed, got %d: %v", len(closer.closed), closer.closed)
	}

	// The new bead should get a pod created.
	if len(mgr.created) == 0 {
		t.Error("expected at least one pod to be created")
	}
}

// ── Permanent K8s validation errors ──────────────────────────────────────────

// newInvalidPodNameError returns an IsInvalid-matching error mirroring the
// apiserver response for a DNS-1123 pod name violation (e.g. the name contains
// uppercase characters). Constructed via apierrors.NewInvalid so
// apierrors.IsInvalid(err) is true in the reconciler.
func newInvalidPodNameError(badName string) error {
	gk := schema.GroupKind{Group: "", Kind: "Pod"}
	errs := field.ErrorList{field.Invalid(
		field.NewPath("metadata", "name"),
		badName,
		"a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters",
	)}
	return apierrors.NewInvalid(gk, badName, errs)
}

// TestReconcile_PermanentCreateError_HardFailsBead asserts that when
// CreateAgentPod returns a permanent K8s validation error (e.g. DNS-1123
// violation), the reconciler hard-fails the bead on the first attempt and
// does not retry CreateAgentPod for it on the next sync.
func TestReconcile_PermanentCreateError_HardFailsBead(t *testing.T) {
	beadID := "bd-invalid"
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{
				ID:        beadID,
				Project:   "proj",
				Mode:      "crew",
				Role:      "dev",
				AgentName: "BadName", // uppercase violates DNS-1123 — but we
				// don't rely on the reconciler rejecting it client-side; the
				// fake pod manager returns IsInvalid below regardless of name.
			},
		},
	}
	mgr := &mockManager{
		pods:      nil,
		createErr: newInvalidPodNameError("BadName"),
	}
	marker := &mockFailedMarker{}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetFailedAgentMarker(marker)

	// First sync: bead state is empty, CreateAgentPod is attempted and fails
	// with an Invalid error.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("first reconcile: unexpected error: %v", err)
	}
	if len(mgr.created) != 1 {
		t.Fatalf("expected exactly 1 CreateAgentPod attempt on first sync, got %d", len(mgr.created))
	}
	if len(marker.calls) != 1 {
		t.Fatalf("expected 1 MarkAgentFailed call, got %d", len(marker.calls))
	}
	if marker.calls[0].beadID != beadID {
		t.Errorf("expected MarkAgentFailed(beadID=%q), got %q", beadID, marker.calls[0].beadID)
	}
	if marker.calls[0].reason == "" {
		t.Errorf("expected non-empty failure reason, got %q", marker.calls[0].reason)
	}

	// Second sync: simulate the bead having been updated with
	// agent_state=failed (MarkAgentFailed would have set this via the
	// beadsapi client). The reconciler must NOT attempt CreateAgentPod
	// again for this bead.
	lister.beads[0].AgentState = "failed"
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile: unexpected error: %v", err)
	}
	if len(mgr.created) != 1 {
		t.Fatalf("expected no additional CreateAgentPod calls on second sync, total got %d", len(mgr.created))
	}
	if len(marker.calls) != 1 {
		t.Errorf("expected no additional MarkAgentFailed calls on second sync, got %d", len(marker.calls))
	}
}

// TestIsPermanentCreateError_MissingSecretsErrorIsTransient asserts that the
// admission-gate error returned by podmanager.CreateAgentPod (ka-0000001230)
// is NOT treated as permanent. If it were, the reconciler would hard-fail
// the agent bead the first time external-secrets-operator hasn't yet synced
// the project Secret — instead we want it to retry on the next sync pass.
func TestIsPermanentCreateError_MissingSecretsErrorIsTransient(t *testing.T) {
	err := &podmanager.MissingSecretsError{
		Pod:       "crew-proj-dev-alpha",
		Namespace: "ns",
		Names:     []string{"proj-jira"},
	}
	if isPermanentCreateError(err) {
		t.Errorf("isPermanentCreateError(MissingSecretsError) = true, want false")
	}
	if isPermanentCreateError(fmt.Errorf("wrap: %w", err)) {
		t.Errorf("isPermanentCreateError(wrapped MissingSecretsError) = true, want false")
	}
}

// TestReconcile_TransientCreateError_DoesNotHardFail asserts that transient
// (non-Invalid) errors from CreateAgentPod follow the existing retry behavior
// and do NOT call MarkAgentFailed.
func TestReconcile_TransientCreateError_DoesNotHardFail(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-transient", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods:      nil,
		createErr: fmt.Errorf("connection refused"),
	}
	marker := &mockFailedMarker{}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetFailedAgentMarker(marker)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(marker.calls) != 0 {
		t.Errorf("expected no MarkAgentFailed calls on transient error, got %d", len(marker.calls))
	}
}

// TestReconcile_TransientCreateError_MarksQueuedAndDefers is the regression
// guard for ka-0000006128: a transient CreateAgentPod error (e.g. a
// MissingSecretsError while a gVisor-namespace ExternalSecret is mid-sync) must
// NOT leave the bead in a silent black hole. The bead must be marked
// agent_state=queued and tracked as deferred (so stale-deferral warnings,
// deferred metrics, and queue-expiry all engage), exactly like the cap/pool
// deferral paths — instead of sitting open with empty agent_state until the 2h
// stale reaper closes it as failed with runs_completed=0.
func TestReconcile_TransientCreateError_MarksQueuedAndDefers(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-wedged", Project: "gardenio", Mode: "crew", Role: "plan-gardener", AgentName: "planner"},
		},
	}
	mgr := &mockManager{
		pods: nil,
		// Simulates verifySecretsExist deferring while a referenced secret is
		// not yet synced — a transient (non-Invalid) error.
		createErr: fmt.Errorf("1 secret(s) not yet synced in namespace gardenio-gvisor: gardenio-nats-agent-creds"),
	}
	marker := &mockFailedMarker{}
	updater := &mockStateUpdater{}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetFailedAgentMarker(marker)
	r.SetStateUpdater(updater)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Never hard-fail a transient error.
	if len(marker.calls) != 0 {
		t.Errorf("expected no MarkAgentFailed calls on transient error, got %d", len(marker.calls))
	}
	// The wedged bead must be marked queued (visible on /plans, un-black-holed).
	if len(updater.updates) != 1 {
		t.Fatalf("expected 1 state update (queued), got %d: %v", len(updater.updates), updater.updates)
	}
	if got := updater.updates[0]; got.beadID != "bd-wedged" || got.state != "queued" {
		t.Errorf("expected bd-wedged marked queued, got %+v", got)
	}
}

// TestReconcile_TransientCreateError_ExpiresAfterQueueTimeout asserts that a
// bead wedged by a persistent transient create error is eventually auto-closed
// by expireDeferredBeads (agent_state=expired) once it exceeds
// CoopQueueExpiryTimeout — proving the create-error deferral is bounded, not a
// silent leak until the 2h stale reaper (ka-0000006128). Follows the two-pass
// pattern of TestReconcile_QueueExpiry_ClosesExpiredBeads: pass 1 records the
// bead as deferred, we age its first-seen past the expiry, pass 2 closes it.
func TestReconcile_TransientCreateError_ExpiresAfterQueueTimeout(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-wedged", Project: "gardenio", Mode: "crew", Role: "plan-gardener", AgentName: "planner"},
		},
	}
	mgr := &mockManager{
		pods: nil,
		// Persistent transient error (secret never syncs) — mirrors the wedge.
		createErr: fmt.Errorf("secret not yet synced"),
	}
	closer := &mockBeadCloser{}

	cfg := testConfig("ns")
	cfg.CoopQueueExpiryTimeout = 30 * time.Minute

	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	// Pass 1: the create error defers the bead, recording it in deferredSince.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if len(r.deferredSince) != 1 {
		t.Fatalf("pass 1: expected 1 deferred-tracked bead, got %d", len(r.deferredSince))
	}
	if len(closer.closed) != 0 {
		t.Fatalf("pass 1: expected 0 closed (not yet aged out), got %d", len(closer.closed))
	}

	// Age the first-seen time past the expiry window.
	for beadID := range r.deferredSince {
		r.deferredSince[beadID] = time.Now().Add(-45 * time.Minute)
	}

	// Pass 2: still deferred (create still errors) AND now aged out → expired.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if len(closer.closed) != 1 {
		t.Fatalf("expected 1 bead auto-closed, got %d: %+v", len(closer.closed), closer.closed)
	}
	if got := closer.closed[0]; got.beadID != "bd-wedged" || got.fields["agent_state"] != "expired" {
		t.Errorf("expected bd-wedged closed as expired, got %+v", got)
	}
}

// ── Mirror-close on agent expiry (ka-0000003183) ────────────────────────────

// mockBeadCloserGetter implements BeadCloser AND beadGetter so the reconciler
// can also close the orphaned mirror task after an agent expires.
type mockBeadCloserGetter struct {
	mu     sync.Mutex
	closed []closedBead
	beads  map[string]*beadsapi.BeadDetail
	getErr error
}

func (m *mockBeadCloserGetter) CloseBead(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = append(m.closed, closedBead{beadID, fields})
	if b, ok := m.beads[beadID]; ok && b != nil {
		b.Status = "closed"
	}
	return nil
}

func (m *mockBeadCloserGetter) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	if b, ok := m.beads[beadID]; ok {
		return b, nil
	}
	return nil, nil
}

func TestReconcile_QueueExpiry_ClosesOrphanedMirrorTask(t *testing.T) {
	beads := []beadsapi.AgentBead{
		{ID: "bd-other", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent-other",
			CreatedAt: time.Now().Add(-2 * time.Hour)},
		{
			ID: "bd-agent", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent0",
			CreatedAt: time.Now().Add(-1 * time.Hour),
			Metadata: map[string]string{
				"task_id":    "bd-mirror",
				"trigger_id": "gd-trig-1",
			},
		},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 1
	cfg.CoopQueueExpiryTimeout = 30 * time.Minute

	closer := &mockBeadCloserGetter{
		beads: map[string]*beadsapi.BeadDetail{
			"bd-mirror": {
				ID:     "bd-mirror",
				Type:   "task",
				Status: "open",
				Fields: map[string]string{"trigger_id": "gd-trig-1"},
			},
		},
	}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	for beadID := range r.deferredSince {
		r.deferredSince[beadID] = time.Now().Add(-45 * time.Minute)
	}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}

	if len(closer.closed) != 2 {
		t.Fatalf("expected 2 closes (agent + mirror), got %d: %+v", len(closer.closed), closer.closed)
	}
	var agentClosed, mirrorClosed bool
	for _, c := range closer.closed {
		switch c.beadID {
		case "bd-agent":
			agentClosed = true
			if c.fields["agent_state"] != "expired" {
				t.Errorf("agent close: expected agent_state=expired, got %q", c.fields["agent_state"])
			}
		case "bd-mirror":
			mirrorClosed = true
			if got := c.fields["close_reason"]; got != "spawn expired before claim" {
				t.Errorf("mirror close_reason = %q, want %q", got, "spawn expired before claim")
			}
		}
	}
	if !agentClosed || !mirrorClosed {
		t.Errorf("expected both agent and mirror closed; agent=%v mirror=%v", agentClosed, mirrorClosed)
	}
}

func TestReconcile_QueueExpiry_SchedulerOwnedMirrorAlsoCloses(t *testing.T) {
	// Legacy scheduler path stamps schedule_id; the mirror-close logic must
	// accept either owner field.
	beads := []beadsapi.AgentBead{
		{ID: "bd-other", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent-other",
			CreatedAt: time.Now().Add(-2 * time.Hour)},
		{
			ID: "bd-agent", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent0",
			CreatedAt: time.Now().Add(-1 * time.Hour),
			Metadata: map[string]string{
				"task_id":     "bd-mirror",
				"schedule_id": "gd-sched-1",
			},
		},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 1
	cfg.CoopQueueExpiryTimeout = 30 * time.Minute

	closer := &mockBeadCloserGetter{
		beads: map[string]*beadsapi.BeadDetail{
			"bd-mirror": {
				ID: "bd-mirror", Type: "task", Status: "open",
				Fields: map[string]string{"schedule_id": "gd-sched-1"},
			},
		},
	}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	_ = r.Reconcile(context.Background())
	for beadID := range r.deferredSince {
		r.deferredSince[beadID] = time.Now().Add(-45 * time.Minute)
	}
	_ = r.Reconcile(context.Background())

	if len(closer.closed) != 2 {
		t.Fatalf("expected 2 closes (agent + mirror), got %d", len(closer.closed))
	}
}

func TestReconcile_QueueExpiry_SkipsMirrorWhenOwnerMismatch(t *testing.T) {
	// An agent pre-assigned to an unrelated task (no owner ID match) must NOT
	// trigger a mirror close. This guards against closing real work beads.
	beads := []beadsapi.AgentBead{
		{ID: "bd-other", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent-other",
			CreatedAt: time.Now().Add(-2 * time.Hour)},
		{
			ID: "bd-agent", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent0",
			CreatedAt: time.Now().Add(-1 * time.Hour),
			Metadata: map[string]string{
				"task_id":    "bd-real-task",
				"trigger_id": "gd-trig-1",
			},
		},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 1
	cfg.CoopQueueExpiryTimeout = 30 * time.Minute

	closer := &mockBeadCloserGetter{
		beads: map[string]*beadsapi.BeadDetail{
			"bd-real-task": {
				ID: "bd-real-task", Type: "bug", Status: "open",
				Fields: map[string]string{"trigger_id": "gd-trig-OTHER"},
			},
		},
	}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	_ = r.Reconcile(context.Background())
	for beadID := range r.deferredSince {
		r.deferredSince[beadID] = time.Now().Add(-45 * time.Minute)
	}
	_ = r.Reconcile(context.Background())

	if len(closer.closed) != 1 {
		t.Fatalf("expected 1 close (agent only), got %d: %+v", len(closer.closed), closer.closed)
	}
	if closer.closed[0].beadID != "bd-agent" {
		t.Errorf("expected only agent closed, got %s", closer.closed[0].beadID)
	}
}

func TestReconcile_QueueExpiry_SkipsAlreadyClosedMirror(t *testing.T) {
	beads := []beadsapi.AgentBead{
		{ID: "bd-other", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent-other",
			CreatedAt: time.Now().Add(-2 * time.Hour)},
		{
			ID: "bd-agent", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent0",
			CreatedAt: time.Now().Add(-1 * time.Hour),
			Metadata: map[string]string{
				"task_id":    "bd-mirror",
				"trigger_id": "gd-trig-1",
			},
		},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 1
	cfg.CoopQueueExpiryTimeout = 30 * time.Minute

	closer := &mockBeadCloserGetter{
		beads: map[string]*beadsapi.BeadDetail{
			"bd-mirror": {
				ID: "bd-mirror", Type: "task", Status: "closed",
				Fields: map[string]string{"trigger_id": "gd-trig-1"},
			},
		},
	}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	_ = r.Reconcile(context.Background())
	for beadID := range r.deferredSince {
		r.deferredSince[beadID] = time.Now().Add(-45 * time.Minute)
	}
	_ = r.Reconcile(context.Background())

	if len(closer.closed) != 1 || closer.closed[0].beadID != "bd-agent" {
		t.Errorf("expected only agent closed when mirror already closed, got %+v", closer.closed)
	}
}

func TestReconcile_QueueExpiry_NoMirrorCloseWhenCloserLacksGetBead(t *testing.T) {
	// Existing mockBeadCloser does NOT implement beadGetter — verifies the
	// no-op fallback path and that nothing panics.
	beads := []beadsapi.AgentBead{
		{ID: "bd-other", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent-other",
			CreatedAt: time.Now().Add(-2 * time.Hour)},
		{
			ID: "bd-agent", Project: "proj", Mode: "crew", Role: "dev", AgentName: "agent0",
			CreatedAt: time.Now().Add(-1 * time.Hour),
			Metadata:  map[string]string{"task_id": "bd-mirror", "trigger_id": "gd-trig-1"},
		},
	}
	lister := &mockLister{beads: beads}
	mgr := &mockManager{pods: nil}

	cfg := testConfig("ns")
	cfg.CoopBurstLimit = 10
	cfg.CoopMaxPods = 1
	cfg.CoopQueueExpiryTimeout = 30 * time.Minute

	closer := &mockBeadCloser{}
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)

	_ = r.Reconcile(context.Background())
	for beadID := range r.deferredSince {
		r.deferredSince[beadID] = time.Now().Add(-45 * time.Minute)
	}
	_ = r.Reconcile(context.Background())

	if len(closer.closed) != 1 || closer.closed[0].beadID != "bd-agent" {
		t.Errorf("expected only agent closed (no getter), got %+v", closer.closed)
	}
}

// TestReconcile_OrphanWarmGraftDeletedByRealName asserts the C2 orphan-delete
// fix (ka-0000005266): a promoted warm-B pod (real name "warm-b-…", mapped under
// its canonical name via the annotation) whose bead has disappeared must be
// deleted by its REAL k8s name, not the canonical map key — else DeleteAgentPod
// NotFound-noops and the running pod leaks.
func TestReconcile_OrphanWarmGraftDeletedByRealName(t *testing.T) {
	// No desired beads, but a promoted warm-B pod exists → orphan.
	lister := &mockLister{beads: nil}
	warm := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-b-node7-1717-3",
			Namespace: "ns",
			Labels: map[string]string{
				podmanager.LabelApp:             podmanager.LabelAppValue,
				podmanager.LabelAgent:           "alpha",
				podmanager.LabelThawPlaceholder: "true",
			},
			Annotations: map[string]string{podmanager.AnnotationCanonicalPod: "crew-proj-dev-alpha"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	mgr := &mockManager{pods: []corev1.Pod{warm}}
	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))

	// Empty-desired orphan cleanup defers for 2 cycles, then deletes on the 3rd.
	for i := 0; i < 3; i++ {
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if len(mgr.deleted) != 1 {
		t.Fatalf("expected 1 deletion, got %d (%v)", len(mgr.deleted), mgr.deleted)
	}
	if mgr.deleted[0] != "warm-b-node7-1717-3" {
		t.Errorf("orphan must be deleted by real name; got %q (canonical key would leak the pod)", mgr.deleted[0])
	}
}

// TestReconcile_OrphanProtection_MassOrphanDeferred verifies the partial-read
// guard (ka-0000005463 hardening): a NON-empty desired set that still orphans
// most of the fleet — the signature of a beads replica answering mid-rollout
// with a partial list — must be confirmed for 3 consecutive cycles before any
// deletion happens, then proceed.
func TestReconcile_OrphanProtection_MassOrphanDeferred(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", corev1.PodRunning),
			makePod("crew-proj-dev-o1", "ns", "crew", "proj", "dev", "o1", corev1.PodRunning),
			makePod("crew-proj-dev-o2", "ns", "crew", "proj", "dev", "o2", corev1.PodRunning),
			makePod("crew-proj-dev-o3", "ns", "crew", "proj", "dev", "o3", corev1.PodRunning),
			makePod("crew-proj-dev-o4", "ns", "crew", "proj", "dev", "o4", corev1.PodRunning),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	// Cycles 1 and 2: mass orphaning detected, nothing deleted.
	for cycle := 1; cycle <= 2; cycle++ {
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("cycle %d: unexpected error: %v", cycle, err)
		}
		if len(mgr.deleted) != 0 {
			t.Fatalf("cycle %d: mass orphaning must be deferred, deleted %v", cycle, mgr.deleted)
		}
	}
	// Cycle 3: state persisted — cleanup proceeds.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("cycle 3: unexpected error: %v", err)
	}
	if len(mgr.deleted) != 4 {
		t.Fatalf("cycle 3: expected 4 orphan deletions after persistence, got %v", mgr.deleted)
	}
}

// TestReconcile_OrphanProtection_MassOrphanCounterResets verifies a healthy
// pass (few/no orphans) resets the mass-orphan counter so a later mass event
// starts its 3-cycle confirmation from scratch.
func TestReconcile_OrphanProtection_MassOrphanCounterResets(t *testing.T) {
	fullBeads := []beadsapi.AgentBead{
		{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		{ID: "bd-2", Project: "proj", Mode: "crew", Role: "dev", AgentName: "o1"},
		{ID: "bd-3", Project: "proj", Mode: "crew", Role: "dev", AgentName: "o2"},
		{ID: "bd-4", Project: "proj", Mode: "crew", Role: "dev", AgentName: "o3"},
	}
	lister := &mockLister{beads: fullBeads[:1]} // partial read: 3 of 4 pods look orphaned
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", corev1.PodRunning),
			makePod("crew-proj-dev-o1", "ns", "crew", "proj", "dev", "o1", corev1.PodRunning),
			makePod("crew-proj-dev-o2", "ns", "crew", "proj", "dev", "o2", corev1.PodRunning),
			makePod("crew-proj-dev-o3", "ns", "crew", "proj", "dev", "o3", corev1.PodRunning),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	// Two partial-read cycles (deferred), then the daemon recovers.
	_ = r.Reconcile(context.Background())
	_ = r.Reconcile(context.Background())
	lister.beads = fullBeads
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("recovered cycle: unexpected error: %v", err)
	}
	if len(mgr.deleted) != 0 {
		t.Fatalf("recovered read must not delete anything, got %v", mgr.deleted)
	}

	// Partial read again: counter must have reset — still deferred, no delete.
	lister.beads = fullBeads[:1]
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("new partial cycle: unexpected error: %v", err)
	}
	if len(mgr.deleted) != 0 {
		t.Fatalf("counter must reset after a healthy pass; deleted %v", mgr.deleted)
	}
}

// mockHarnessStamper implements HarnessStamper, recording calls.
type mockHarnessStamper struct {
	calls []stampCall
	err   error
}

type stampCall struct {
	beadID  string
	harness string
}

func (m *mockHarnessStamper) StampResolvedHarness(_ context.Context, beadID, harness string) error {
	m.calls = append(m.calls, stampCall{beadID, harness})
	return m.err
}

// TestReconcile_StampsResolvedHarness verifies ADR 0004 section 4 on the
// create-missing path: the harness the spec builder resolved is written onto
// the agent bead before its pod is created; a bead refused for a bad harness is
// not stamped; and a failing stamp never blocks the spawn.
func TestReconcile_StampsResolvedHarness(t *testing.T) {
	lister := func() *mockLister {
		return &mockLister{beads: []beadsapi.AgentBead{
			{ID: "bd-codex", Project: "proj", Mode: "crew", Role: "dev", AgentName: "codexer"},
			{ID: "bd-bad", Project: "proj", Mode: "crew", Role: "dev", AgentName: "badharness"},
		}}
	}
	base := simpleSpecBuilder("img:v1")
	builder := func(cfg *config.Config, project, mode, role, agentName string, metadata map[string]string) podmanager.AgentPodSpec {
		spec := base(cfg, project, mode, role, agentName, metadata)
		if agentName == "badharness" {
			spec.HarnessError = "unknown harness"
		} else {
			spec.Harness = "codex"
		}
		return spec
	}

	t.Run("stamps before create, skips refused bead", func(t *testing.T) {
		mgr := &mockManager{}
		stamper := &mockHarnessStamper{}
		r := New(lister(), mgr, testConfig("ns"), testLogger(), builder)
		r.SetFailedAgentMarker(&mockFailedMarker{})
		r.SetHarnessStamper(stamper)
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(stamper.calls) != 1 || stamper.calls[0] != (stampCall{"bd-codex", "codex"}) {
			t.Fatalf("stamp calls = %v, want only bd-codex=codex", stamper.calls)
		}
		if len(mgr.created) != 1 {
			t.Fatalf("created = %v, want 1 pod", mgr.created)
		}
	})

	t.Run("stamp failure does not block the spawn", func(t *testing.T) {
		mgr := &mockManager{}
		stamper := &mockHarnessStamper{err: errors.New("daemon down")}
		r := New(lister(), mgr, testConfig("ns"), testLogger(), builder)
		r.SetFailedAgentMarker(&mockFailedMarker{})
		r.SetHarnessStamper(stamper)
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mgr.created) != 1 {
			t.Fatalf("created = %v, want the pod created despite the stamp error", mgr.created)
		}
	})
}

// mockSpawnTaskClaimer implements SpawnTaskClaimer, recording calls.
type mockSpawnTaskClaimer struct {
	calls []string // agent bead IDs
	err   error
}

func (m *mockSpawnTaskClaimer) ClaimSpawnTask(_ context.Context, agentBeadID string) (string, error) {
	m.calls = append(m.calls, agentBeadID)
	return beadsapi.SpawnTaskClaimed, m.err
}

// TestReconcile_ClaimsSpawnTask verifies gd-Yo28zKgkGx on the create-missing
// path: an agent bead naming a task gets that task claimed for it before its
// pod is created, an agent with no task is not, and a failing claim never
// blocks the spawn.
func TestReconcile_ClaimsSpawnTask(t *testing.T) {
	lister := func() *mockLister {
		return &mockLister{beads: []beadsapi.AgentBead{
			{ID: "bd-crew", Project: "proj", Mode: "crew", Role: "crew", AgentName: "crewer"},
			{ID: "bd-adhoc", Project: "proj", Mode: "crew", Role: "crew", AgentName: "adhoc"},
		}}
	}
	base := simpleSpecBuilder("img:v1")
	builder := func(cfg *config.Config, project, mode, role, agentName string, metadata map[string]string) podmanager.AgentPodSpec {
		spec := base(cfg, project, mode, role, agentName, metadata)
		if agentName == "crewer" {
			spec.TaskID = "gd-task1"
		}
		return spec
	}

	t.Run("claims the named task only", func(t *testing.T) {
		mgr := &mockManager{}
		claimer := &mockSpawnTaskClaimer{}
		r := New(lister(), mgr, testConfig("ns"), testLogger(), builder)
		r.SetSpawnTaskClaimer(claimer)
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(claimer.calls) != 1 || claimer.calls[0] != "bd-crew" {
			t.Fatalf("claim calls = %v, want only bd-crew", claimer.calls)
		}
		if len(mgr.created) != 2 {
			t.Fatalf("created = %v, want 2 pods", mgr.created)
		}
	})

	t.Run("claim failure does not block the spawn", func(t *testing.T) {
		mgr := &mockManager{}
		claimer := &mockSpawnTaskClaimer{err: errors.New("daemon down")}
		r := New(lister(), mgr, testConfig("ns"), testLogger(), builder)
		r.SetSpawnTaskClaimer(claimer)
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mgr.created) != 2 {
			t.Fatalf("created = %v, want both pods created despite the claim error", mgr.created)
		}
	})
}
