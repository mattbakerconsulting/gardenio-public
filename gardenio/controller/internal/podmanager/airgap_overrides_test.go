package podmanager

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Air-gap blocker P4.3 (ka-0000007177): the agent pull policy and DinD image
// gained controller-side override knobs (env AGENT_PULL_POLICY / DIND_IMAGE,
// Helm agents.agentPullPolicy / agents.dindImage). These tests pin both the
// default (cloud) behavior and the override path.

// TestAgentPullPolicy_DefaultIsAlways verifies the agent container keeps the
// historical PullAlways default when no override is set.
func TestAgentPullPolicy_DefaultIsAlways(t *testing.T) {
	m := newTestManager()
	c := m.buildContainer(minimalSpec())
	if c.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("default ImagePullPolicy = %q, want %q", c.ImagePullPolicy, corev1.PullAlways)
	}
}

// TestAgentPullPolicy_Override verifies AgentPullPolicy replaces the default.
func TestAgentPullPolicy_Override(t *testing.T) {
	m := newTestManager()
	m.AgentPullPolicy = corev1.PullIfNotPresent
	c := m.buildContainer(minimalSpec())
	if c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("ImagePullPolicy = %q, want %q (override)", c.ImagePullPolicy, corev1.PullIfNotPresent)
	}
}

// TestDindImage_DefaultIsConstant verifies the DinD sidecar uses the compiled-in
// DindImage when no override is set.
func TestDindImage_DefaultIsConstant(t *testing.T) {
	m := newTestManager()
	c := m.buildDindSidecar()
	if c.Image != DindImage {
		t.Errorf("default DinD image = %q, want %q", c.Image, DindImage)
	}
}

// TestDindImage_Override verifies DindImageOverride replaces the default so an
// air-gapped box can pin a ref the in-box registry mirror resolves.
func TestDindImage_Override(t *testing.T) {
	m := newTestManager()
	m.DindImageOverride = "registry.example.com/docker:dind-pinned"
	c := m.buildDindSidecar()
	if c.Image != "registry.example.com/docker:dind-pinned" {
		t.Errorf("DinD image = %q, want override value", c.Image)
	}
}
