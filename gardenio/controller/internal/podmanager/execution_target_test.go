package podmanager

import "testing"

// TestIsKubernetesTarget covers the backend-selection predicate. Empty and the
// explicit "kubernetes" value both mean the pod path (so pre-existing agents,
// which never set the field, keep spawning pods); anything else is a non-pod
// backend.
func TestIsKubernetesTarget(t *testing.T) {
	tests := []struct {
		target string
		want   bool
	}{
		{"", true},
		{ExecutionTargetKubernetes, true},
		{ExecutionTargetLambdaMicroVM, false},
		{"something-else", false},
	}
	for _, tt := range tests {
		if got := IsKubernetesTarget(tt.target); got != tt.want {
			t.Errorf("IsKubernetesTarget(%q) = %v, want %v", tt.target, got, tt.want)
		}
	}
}

// TestIsProbeTarget covers the probe backend-selection predicate. Only the
// explicit "probe" value selects the scripted one-off pod backend; crucially,
// probe is NOT a Kubernetes target (it realizes as a DIFFERENT pod, built by
// BuildProbePod, not CreateAgentPod), so the controller spawn door routes it
// away from CreateAgentPod.
func TestIsProbeTarget(t *testing.T) {
	tests := []struct {
		target    string
		wantProbe bool
		wantK8s   bool
	}{
		{"", false, true},
		{ExecutionTargetKubernetes, false, true},
		{ExecutionTargetLambdaMicroVM, false, false},
		{ExecutionTargetProbe, true, false},
		{"something-else", false, false},
	}
	for _, tt := range tests {
		if got := IsProbeTarget(tt.target); got != tt.wantProbe {
			t.Errorf("IsProbeTarget(%q) = %v, want %v", tt.target, got, tt.wantProbe)
		}
		// Invariant: a probe target must never also be a Kubernetes target, or
		// the spawn door would hand it to CreateAgentPod.
		if got := IsKubernetesTarget(tt.target); got != tt.wantK8s {
			t.Errorf("IsKubernetesTarget(%q) = %v, want %v", tt.target, got, tt.wantK8s)
		}
	}
}

// TestIsLaptopTarget: only "laptop" is a laptop agent, and it is never a
// Kubernetes target, so nothing hands it to CreateAgentPod. "local" is the
// escalation bridge's fleet-executor stamp, not a laptop (gd-SzPJDNTBXS).
func TestIsLaptopTarget(t *testing.T) {
	for target, want := range map[string]bool{
		"":                           false,
		ExecutionTargetKubernetes:    false,
		ExecutionTargetLambdaMicroVM: false,
		ExecutionTargetProbe:         false,
		ExecutionTargetLaptop:        true,
		"local":                      false,
	} {
		if got := IsLaptopTarget(target); got != want {
			t.Errorf("IsLaptopTarget(%q) = %v, want %v", target, got, want)
		}
	}
	if IsKubernetesTarget(ExecutionTargetLaptop) {
		t.Error("a local target must never be a Kubernetes target")
	}
}
