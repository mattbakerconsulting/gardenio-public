package tests

import (
	"testing"
)

// ka-0000003176: on k3s the pre-install/pre-upgrade hook pods reach the
// apiserver via 10.43.0.1:443, which kube-proxy DNATs to <node-RFC1918-IP>:6443
// BEFORE kube-router evaluates egress ipBlocks. The hook egress policies must
// therefore allow the private ranges on TCP/6443, or the psa-labels Job
// crash-loops "connection refused" → BackoffLimitExceeded → helm aborts the
// whole install with 0 pods (tarboat BYO sweep db0b7a9d, calver 2026.200.10).

// Gating is unchanged: without defaultDenyEgress no hook egress policy (and
// no private-6443 allow) renders at all.
func TestHookEgress_StillGatedOnDefaultDenyEgress(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, []string{"values-test.yaml"},
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=false",
	)
	if sliceNetPol(out, "-psa-labels-egress") != "" {
		t.Error("psa-labels-egress must not render when defaultDenyEgress is off")
	}
}
