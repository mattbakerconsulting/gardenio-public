package tests

import (
	"strings"
	"testing"
)

// ka-0000004987: on k3s the LONG-RUNNING apiserver clients (controller,
// grafana-bridge, pod-state-metrics) dial the apiserver
// via 10.43.0.1:443, which kube-proxy DNATs to <node-RFC1918-IP>:6443 BEFORE
// kube-router evaluates egress ipBlocks — the exact mechanism that broke the
// pre-install hook Jobs (ka-0000003176, hook_egress_k3s_apiserver_test.go).
// Their egress policies render apiserver allows on 443 only, so with
// defaultDenyEgress on a k3s box the controller cannot reconcile or spawn
// agents. The fix is the networkPolicies.apiServerPostDNAT switch: when true,
// each of the three policies gains a private-range TCP/6443-only allow
// (gardenio.netpol.k3sApiServerPostDNATEgress); the 443 no-pivot posture
// (RFC1918/IMDS/CGNAT excluded) is unchanged. The box overlay enables it;
// EKS overlays stay off (their CNI matches ipBlocks pre-DNAT, so the pinned
// kubeApiServer.cidrs 172.20.0.1/32 covers the apiserver there).

// longRunningClientPolicies are the egress policies that carry an apiserver
// allow for a long-running pod (values-test.yaml + these sets renders all 3).
var longRunningClientPolicies = []string{
	"-controller-egress",
	"-grafana-bridge-egress",
	"-pod-state-metrics-egress",
}

// allClientsStack renders the chart with every long-running apiserver client
// enabled, parameterized on the apiServerPostDNAT switch.
func allClientsStack(t *testing.T, postDNAT string) string {
	t.Helper()
	return helmTemplateFull(t, []string{"values-test.yaml"},
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"networkPolicies.apiServerPostDNAT="+postDNAT,
		"agents.enabled=true",
		"grafanaBridge.enabled=true",
		"monitoring.podStateMetrics.enabled=true",
	)
}

// With the switch on, all three client policies carry the private-range
// TCP/6443 allow.
func TestLongRunningEgress_K3sPostDNATApiserverAllowed(t *testing.T) {
	t.Parallel()
	out := allClientsStack(t, "true")
	for _, np := range longRunningClientPolicies {
		seg := sliceNetPol(out, np)
		if seg == "" {
			t.Fatalf("expected NetworkPolicy %s to render", np)
		}
		for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
			if !strings.Contains(seg, "cidr: "+cidr+"\n") {
				t.Errorf("%s must allow private range %s (k3s post-DNAT apiserver path), got:\n%s", np, cidr, seg)
			}
		}
		if !strings.Contains(seg, "port: 6443") {
			t.Errorf("%s must allow TCP/6443 (k3s apiserver port), got:\n%s", np, seg)
		}
	}
}

// The private-6443 allow must NOT relax the 443 posture: the internet rule
// keeps its RFC1918/IMDS/CGNAT exceptions, and IMDS/CGNAT never appear as a
// standalone allowed destination.
func TestLongRunningEgress_NoPivotPostureSurvives(t *testing.T) {
	t.Parallel()
	out := allClientsStack(t, "true")
	for _, np := range longRunningClientPolicies {
		seg := sliceNetPol(out, np)
		if seg == "" {
			t.Fatalf("expected NetworkPolicy %s to render", np)
		}
		if !strings.Contains(seg, "cidr: 0.0.0.0/0") {
			t.Fatalf("%s must keep the public-endpoint internet rule, got:\n%s", np, seg)
		}
		for _, exc := range []string{"- 10.0.0.0/8", "- 172.16.0.0/12", "- 192.168.0.0/16", "- 169.254.0.0/16", "- 100.64.0.0/10"} {
			if !strings.Contains(seg, exc) {
				t.Errorf("%s internet rule must keep exception %q, got:\n%s", np, exc, seg)
			}
		}
		for _, bad := range []string{"cidr: 169.254.0.0/16\n", "cidr: 100.64.0.0/10\n"} {
			if strings.Contains(seg, bad) {
				t.Errorf("%s must NOT allow %q as a destination, got:\n%s", np, bad, seg)
			}
		}
	}
}

// Default off: without apiServerPostDNAT none of the client policies gains
// private-6443 reach — the EKS posture is byte-for-byte what it was.
func TestLongRunningEgress_PostDNATOffByDefault(t *testing.T) {
	t.Parallel()
	out := allClientsStack(t, "false")
	for _, np := range longRunningClientPolicies {
		seg := sliceNetPol(out, np)
		if seg == "" {
			t.Fatalf("expected NetworkPolicy %s to render", np)
		}
		if strings.Contains(seg, "port: 6443") {
			t.Errorf("%s must NOT allow TCP/6443 when apiServerPostDNAT is off, got:\n%s", np, seg)
		}
	}
}
