package podmanager

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// These tests cover the adopt-or-replace resolution CreateAgentPod applies when
// K8s returns AlreadyExists on the deterministic (stable-identity) pod name
// (ka-0000006228). Before this fix the collision was swallowed as success, so a
// wake over a prior incarnation's Terminating husk started no coop process and
// silently reverted the /plans chip from "waking" back to "parked".

// markerAnnotation lets a test distinguish the pre-seeded pod object from the
// fresh pod CreateAgentPod builds (buildPod stamps only AnnotationBeadID +
// scrape annotations, never this key), so "was it replaced?" is observable
// without relying on the fake client assigning UIDs (it does not).
const markerAnnotation = "test.gardenio.io/seeded"

// seededPod builds a pod occupying the canonical name for spec, with the given
// bead-id annotation and phase, tagged with markerAnnotation so a replacement
// (which drops the marker) is detectable.
func seededPod(spec AgentPodSpec, beadID string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.PodName(),
			Namespace: spec.Namespace,
			UID:       "seeded-uid",
			Annotations: map[string]string{
				AnnotationBeadID: beadID,
				markerAnnotation: "1",
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func getPod(t *testing.T, client *fake.Clientset, spec AgentPodSpec) *corev1.Pod {
	t.Helper()
	pod, err := client.CoreV1().Pods(spec.Namespace).Get(context.Background(), spec.PodName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s: %v", spec.PodName(), err)
	}
	return pod
}

// TestCreateAgentPod_AdoptsLivePodForSameBead: the canonical name is held by a
// genuinely live pod for THIS bead (running, not terminating, bead-id matches).
// CreateAgentPod must adopt it (return nil, no delete/recreate) so an at-least-
// once redelivered spawn stays idempotent instead of killing a healthy pod.
func TestCreateAgentPod_AdoptsLivePodForSameBead(t *testing.T) {
	spec := minimalSpec()
	existing := seededPod(spec, spec.BeadID, corev1.PodRunning)
	client := fake.NewClientset(existing)
	m := New(client, testLogger())

	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("CreateAgentPod (adopt): %v", err)
	}

	pod := getPod(t, client, spec)
	if pod.UID != "seeded-uid" {
		t.Errorf("live pod was replaced (UID %q); expected the seeded pod to be adopted", pod.UID)
	}
	if pod.Annotations[markerAnnotation] == "" {
		t.Errorf("live pod was replaced (marker dropped); expected adoption")
	}
}

// TestCreateAgentPod_ReplacesTerminatingHusk is the core regression guard: the
// prior incarnation's pod is mid-termination (DeletionTimestamp set) under the
// same canonical name. CreateAgentPod must force-delete it and create a fresh
// pod in the same call, so the wake actually starts a new coop process.
func TestCreateAgentPod_ReplacesTerminatingHusk(t *testing.T) {
	spec := minimalSpec()
	husk := seededPod(spec, spec.BeadID, corev1.PodRunning)
	now := metav1.Now()
	husk.DeletionTimestamp = &now
	husk.Finalizers = []string{"gardenio.io/test-hold"} // keep the fake tracker from auto-reaping at seed time
	client := fake.NewClientset(husk)
	m := New(client, testLogger())

	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("CreateAgentPod (replace terminating): %v", err)
	}

	pod := getPod(t, client, spec)
	if pod.Annotations[markerAnnotation] != "" {
		t.Errorf("terminating husk was adopted, not replaced (marker still present)")
	}
	if pod.DeletionTimestamp != nil {
		t.Errorf("resulting pod still has DeletionTimestamp; expected a fresh pod")
	}
	if pod.Annotations[AnnotationBeadID] != spec.BeadID {
		t.Errorf("bead-id annotation = %q, want %q", pod.Annotations[AnnotationBeadID], spec.BeadID)
	}
}

// TestCreateAgentPod_ReplacesTerminalPhasePod: a Succeeded/Failed pod squatting
// on the name is a dead husk the reconciler would otherwise skip. Replace it.
func TestCreateAgentPod_ReplacesTerminalPhasePod(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
		t.Run(string(phase), func(t *testing.T) {
			spec := minimalSpec()
			client := fake.NewClientset(seededPod(spec, spec.BeadID, phase))
			m := New(client, testLogger())

			if err := m.CreateAgentPod(context.Background(), spec); err != nil {
				t.Fatalf("CreateAgentPod (replace %s): %v", phase, err)
			}
			if pod := getPod(t, client, spec); pod.Annotations[markerAnnotation] != "" {
				t.Errorf("terminal-phase (%s) pod was adopted, not replaced", phase)
			}
		})
	}
}

// TestCreateAgentPod_ReplacesForeignBeadPod: the name is held by a running pod
// belonging to a DIFFERENT bead (bead recreated under the same agent name, or a
// truncation collision). It is not this agent's pod — replace it so this bead's
// pod actually comes up.
func TestCreateAgentPod_ReplacesForeignBeadPod(t *testing.T) {
	spec := minimalSpec()
	client := fake.NewClientset(seededPod(spec, "some-other-bead", corev1.PodRunning))
	m := New(client, testLogger())

	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("CreateAgentPod (replace foreign bead): %v", err)
	}
	pod := getPod(t, client, spec)
	if pod.Annotations[markerAnnotation] != "" {
		t.Errorf("foreign-bead pod was adopted, not replaced")
	}
	if pod.Annotations[AnnotationBeadID] != spec.BeadID {
		t.Errorf("bead-id annotation = %q, want %q", pod.Annotations[AnnotationBeadID], spec.BeadID)
	}
}

// TestCreateAgentPod_ConflictYieldsRetryableError: if a DIFFERENT pod object
// claims the canonical name between our inspect and our delete (UID precondition
// fails), CreateAgentPod must NOT fight it — it returns a *PodNameConflictError
// (retryable) so the reconciler re-inspects on a later pass.
func TestCreateAgentPod_ConflictYieldsRetryableError(t *testing.T) {
	spec := minimalSpec()
	client := fake.NewClientset(seededPod(spec, "some-other-bead", corev1.PodRunning))
	// Make the force-delete fail with Conflict, as if the UID precondition
	// mismatched (a fresh pod raced onto the name).
	client.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			corev1.Resource("pods"), spec.PodName(), errors.New("uid precondition failed"))
	})
	m := New(client, testLogger())

	err := m.CreateAgentPod(context.Background(), spec)
	var conflict *PodNameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("CreateAgentPod: error = %v (%T), want *PodNameConflictError", err, err)
	}
	if apierrors.IsInvalid(err) {
		t.Errorf("PodNameConflictError must not classify as a permanent (Invalid) create error")
	}
}

// TestStalePodReason exercises the classifier directly across every branch,
// including precedence: a terminating pod whose bead-id also matches must be
// classified as terminating (liveness checks run before the mismatch check).
func TestStalePodReason(t *testing.T) {
	now := metav1.Now()
	pod := func(mut func(p *corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationBeadID: "bead-abc"}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		if mut != nil {
			mut(p)
		}
		return p
	}

	tests := []struct {
		name     string
		pod      *corev1.Pod
		beadID   string
		wantHint string // substring the reason must contain; "" means empty reason (live/adopt)
	}{
		{"live matching bead", pod(nil), "bead-abc", ""},
		{"terminating", pod(func(p *corev1.Pod) { p.DeletionTimestamp = &now }), "bead-abc", "terminating"},
		{"succeeded", pod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }), "bead-abc", "terminal phase"},
		{"failed", pod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }), "bead-abc", "terminal phase"},
		{"foreign bead", pod(nil), "bead-xyz", "belongs to bead"},
		{"empty spec bead skips mismatch", pod(nil), "", ""},
		{"terminating beats mismatch", pod(func(p *corev1.Pod) { p.DeletionTimestamp = &now }), "bead-xyz", "terminating"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stalePodReason(tt.pod, tt.beadID)
			if tt.wantHint == "" {
				if got != "" {
					t.Errorf("stalePodReason = %q, want empty (adopt)", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantHint) {
				t.Errorf("stalePodReason = %q, want a reason containing %q", got, tt.wantHint)
			}
		})
	}
}
