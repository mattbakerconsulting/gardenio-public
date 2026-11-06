package podmanager

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestBuildWarmPlaceholderPod_Shape(t *testing.T) {
	pod := BuildWarmPlaceholderPod(WarmPlaceholderSpec{
		Name:      "warm-b-node-a-1",
		Namespace: "gvisor",
		Node:      "node-a",
		Image:     "agent:latest",
	})

	if pod.Name != "warm-b-node-a-1" || pod.Namespace != "gvisor" {
		t.Fatalf("unexpected name/ns: %s/%s", pod.Name, pod.Namespace)
	}
	if pod.Spec.NodeName != "node-a" {
		t.Errorf("expected node pin node-a, got %q", pod.Spec.NodeName)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != RuntimeClassRunsc {
		t.Errorf("expected runsc runtime class")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("expected RestartPolicyNever, got %v", pod.Spec.RestartPolicy)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("expected AutomountServiceAccountToken=false")
	}

	// The warm placeholder must NOT carry the agent label, so the reconciler's
	// orphan sweep (which only considers LabelAgent pods) leaves it alone.
	if _, ok := pod.Labels[LabelAgent]; ok {
		t.Errorf("warm placeholder must not carry the agent label")
	}
	if pod.Labels[LabelWarmPlaceholder] != WarmPlaceholderReady {
		t.Errorf("expected warm-placeholder=ready, got %q", pod.Labels[LabelWarmPlaceholder])
	}
	if pod.Labels[LabelApp] != LabelAppValue {
		t.Errorf("expected app label %q", LabelAppValue)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected exactly 1 container, got %d", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Image != "agent:latest" {
		t.Errorf("expected image agent:latest, got %q", c.Image)
	}
	// Sleep shell so the body never races a future restore.
	if len(c.Command) == 0 || c.Command[0] != "/bin/sh" {
		t.Errorf("expected sleep shell command, got %v", c.Command)
	}
	// /work emptyDir must exist and be mounted (the restore repoints into /work).
	foundWorkMount := false
	for _, m := range c.VolumeMounts {
		if m.MountPath == WarmPlaceholderWorkPath {
			foundWorkMount = true
		}
	}
	if !foundWorkMount {
		t.Errorf("expected /work mount in container")
	}
	foundWorkVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == VolumeWork && v.EmptyDir != nil {
			foundWorkVol = true
		}
	}
	if !foundWorkVol {
		t.Errorf("expected /work emptyDir volume")
	}

	// PSA-restricted: nonroot, no privilege escalation, drop ALL caps, seccomp.
	if c.SecurityContext == nil {
		t.Fatal("expected container security context")
	}
	if c.SecurityContext.RunAsNonRoot == nil || !*c.SecurityContext.RunAsNonRoot {
		t.Errorf("expected RunAsNonRoot=true")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("expected AllowPrivilegeEscalation=false")
	}
	if c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) == 0 {
		t.Errorf("expected dropped capabilities")
	}
}

func TestBuildWarmPlaceholderPod_DefaultResources(t *testing.T) {
	pod := BuildWarmPlaceholderPod(WarmPlaceholderSpec{
		Name: "w", Namespace: "gvisor", Node: "n", Image: "img",
	})
	req := pod.Spec.Containers[0].Resources.Requests
	if req.Cpu().IsZero() || req.Memory().IsZero() {
		t.Errorf("expected default cpu/memory requests, got %v", req)
	}
}

func TestApplyWarmPlaceholderPromotion(t *testing.T) {
	pod := BuildWarmPlaceholderPod(WarmPlaceholderSpec{
		Name: "warm-b-node-a-1", Namespace: "gvisor", Node: "node-a", Image: "agent:latest",
	})
	// Sanity: a fresh warm placeholder is labeled ready and carries no agent id.
	if pod.Labels[LabelWarmPlaceholder] != WarmPlaceholderReady {
		t.Fatalf("precondition: expected ready label, got %q", pod.Labels[LabelWarmPlaceholder])
	}

	ApplyWarmPlaceholderPromotion(pod, WarmPlaceholderPromotion{
		AgentName:    "gardenio-agent-x",
		BeadID:       "gd-bead",
		CanonicalPod: "crew-gardenio-crew-agent-x",
	})

	// warm-placeholder label must be dropped entirely so the pool reaper can
	// never delete the now-live agent pod.
	if _, ok := pod.Labels[LabelWarmPlaceholder]; ok {
		t.Errorf("warm-placeholder label not dropped: %q", pod.Labels[LabelWarmPlaceholder])
	}
	if pod.Labels[LabelAgent] != "gardenio-agent-x" {
		t.Errorf("agent label = %q, want gardenio-agent-x", pod.Labels[LabelAgent])
	}
	if pod.Labels[LabelThawPlaceholder] != "true" {
		t.Errorf("thaw-placeholder label = %q, want true", pod.Labels[LabelThawPlaceholder])
	}
	if pod.Annotations[AnnotationBeadID] != "gd-bead" {
		t.Errorf("bead-id annotation = %q, want gd-bead", pod.Annotations[AnnotationBeadID])
	}
	if pod.Annotations[AnnotationCanonicalPod] != "crew-gardenio-crew-agent-x" {
		t.Errorf("canonical-pod annotation = %q", pod.Annotations[AnnotationCanonicalPod])
	}
	// LabelApp is preserved (still a gardenio pod).
	if pod.Labels[LabelApp] != LabelAppValue {
		t.Errorf("app label clobbered: %q", pod.Labels[LabelApp])
	}
}

func TestApplyWarmPlaceholderPromotion_OmitsEmptyAnnotations(t *testing.T) {
	pod := &corev1.Pod{}
	ApplyWarmPlaceholderPromotion(pod, WarmPlaceholderPromotion{AgentName: "a"})
	if _, ok := pod.Annotations[AnnotationBeadID]; ok {
		t.Errorf("empty BeadID must not write a bead-id annotation")
	}
	if _, ok := pod.Annotations[AnnotationCanonicalPod]; ok {
		t.Errorf("empty CanonicalPod must not write a canonical-pod annotation")
	}
	if pod.Labels[LabelAgent] != "a" {
		t.Errorf("agent label = %q, want a", pod.Labels[LabelAgent])
	}
}

// TestApplyWarmPlaceholderPromotion_KarpenterDoNotDisrupt verifies the
// do-not-disrupt protection survives the warm-graft fast path. A bare warm
// placeholder carries no do-not-disrupt stamp, so the graft must apply it
// unconditionally — every agent pod is protected (investigation ka-0000006340).
func TestApplyWarmPlaceholderPromotion_KarpenterDoNotDisrupt(t *testing.T) {
	pod := &corev1.Pod{}
	ApplyWarmPlaceholderPromotion(pod, WarmPlaceholderPromotion{AgentName: "a"})
	if got := pod.Annotations[AnnotationKarpenterDoNotDisrupt]; got != "true" {
		t.Errorf("grafted pod: do-not-disrupt = %q, want %q", got, "true")
	}
}

// A warm placeholder carries the configured low-priority class so a cold real
// agent preempts it under node pressure; empty leaves it at default priority
// (ka-0000000722).
func TestBuildWarmPlaceholderPod_PriorityClass(t *testing.T) {
	withClass := BuildWarmPlaceholderPod(WarmPlaceholderSpec{
		Name:              "warm-b-node-a-1",
		Namespace:         "gvisor",
		Node:              "node-a",
		Image:             "agent:latest",
		PriorityClassName: "gardenio-prewarmed",
	})
	if got := withClass.Spec.PriorityClassName; got != "gardenio-prewarmed" {
		t.Errorf("PriorityClassName = %q, want %q", got, "gardenio-prewarmed")
	}

	noClass := BuildWarmPlaceholderPod(WarmPlaceholderSpec{
		Name:      "warm-b-node-a-2",
		Namespace: "gvisor",
		Node:      "node-a",
		Image:     "agent:latest",
	})
	if got := noClass.Spec.PriorityClassName; got != "" {
		t.Errorf("PriorityClassName = %q, want empty when unset", got)
	}
}
