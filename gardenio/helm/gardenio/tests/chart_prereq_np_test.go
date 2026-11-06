package tests

import (
	"strings"
	"testing"
)

// Chart prereqs before live NetworkPolicy enforcement on prod (ka-0000005077,
// epic ka-0000002011). The C6 prod flip (MR !4969) shipped
// networkPolicies{enabled:true, defaultDenyEgress:true} into
// deploy-values/gardenio.yaml. NP enforcement is currently DISABLED cluster-wide
// on america-e2e-eks (verified 2026-06-29), so those policies render but are
// inert today. Two chart gaps would break prod the day the cluster turns NP
// enforcement on:
//
//  1. beads-egress only allowed TCP/5432 to the in-cluster postgres pod, NATS,
//     and kube-dns — no ipBlock. Prod beads.databaseURL points at an EXTERNAL
//     Aurora endpoint resolving to RFC1918 10.30.155.42. Without a
//     values-driven external-DB allow, beads loses its DB the day enforcement
//     flips on (every internet allow excludes RFC1918, so even an internet
//     escape hatch would not cover it).
//
//  2. grafana-bridge had NO NetworkPolicy at all even though grafanaBridge is
//     on in prod (backfill.enabled=true, talks to beads HTTP+SSE, writes the
//     grafana_dashboard ConfigMap via the kube-apiserver). Under live
//     defaultDenyEgress it would lose DNS+beads+apiserver and crashloop.
//
// These tests pin the chart shape that fixes both gaps via --set-driven renders.
// They are FORWARD-LOOKING regression guards. (The former prod-overlay render
// test was dropped when deploy-values/gardenio.yaml was deleted — see the
// ka-0000002553 note below where TestProdOverlay_NetworkPolicyShape stood.)

// ── Gap 1: beads-egress external-DB allow ───────────────────────────────────

// Default chart shape: networkPolicies.beads.externalDatabaseCIDRs is empty,
// so the beads-egress policy must NOT render an ipBlock allow on TCP/5432.
// This guards against accidentally widening the default egress posture.
func TestBeadsEgress_NoExternalDBAllow_WhenCIDRsEmpty(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"postgres.enabled=true",
	)
	beadsDoc := sliceDoc(out, "name: test-beads-egress")
	if beadsDoc == "" {
		t.Fatalf("beads-egress NetworkPolicy must render when defaultDenyEgress=true, got:\n%s", out)
	}
	if strings.Contains(beadsDoc, "ipBlock:") {
		t.Errorf("beads-egress must NOT render an ipBlock allow when externalDatabaseCIDRs is empty (default-egress hygiene), got:\n%s", beadsDoc)
	}
}

// Setting networkPolicies.beads.externalDatabaseCIDRs renders an ipBlock
// allow on TCP/5432 only (no DNS, no other ports). This is the explicit
// off-cluster RDS / Aurora allow that prod needs.
func TestBeadsEgress_ExternalDBAllow_RendersScoped(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"postgres.enabled=true",
		`networkPolicies.beads.externalDatabaseCIDRs[0]=10.30.155.42/32`,
	)
	beadsDoc := sliceDoc(out, "name: test-beads-egress")
	if beadsDoc == "" {
		t.Fatalf("beads-egress NetworkPolicy must render, got:\n%s", out)
	}
	if !strings.Contains(beadsDoc, "cidr: 10.30.155.42/32") {
		t.Errorf("beads-egress must allow the configured external-DB CIDR, got:\n%s", beadsDoc)
	}
	// The new allow is scoped to TCP/5432 — not 443, not 80, not all ports.
	// Guard: assert no internet-443/80 escape hatch slipped in.
	if strings.Contains(beadsDoc, "port: 443") || strings.Contains(beadsDoc, "port: 80") {
		t.Errorf("beads-egress must NOT carry an internet-443/80 escape hatch (RFC1918 ENI would be missed anyway), got:\n%s", beadsDoc)
	}
	if !strings.Contains(beadsDoc, "port: 5432") {
		t.Errorf("beads-egress external-DB allow must use TCP/5432, got:\n%s", beadsDoc)
	}
	// Adjacency guard: the ipBlock and the port:5432 must be co-located in the
	// SAME egress rule, not split into two rules (which could let a regression
	// pair the ipBlock with the wrong port). The rendered shape is:
	//   - to:
	//       - ipBlock:
	//           cidr: 10.30.155.42/32
	//     ports:
	//       - protocol: TCP
	//         port: 5432
	// So everything between `cidr: 10.30.155.42/32` and the next `- to:` (or
	// end of doc) must contain `port: 5432`.
	cidrIdx := strings.Index(beadsDoc, "cidr: 10.30.155.42/32")
	rest := beadsDoc[cidrIdx:]
	nextToIdx := strings.Index(rest[1:], "- to:")
	if nextToIdx > 0 {
		rest = rest[:nextToIdx]
	}
	if !strings.Contains(rest, "port: 5432") {
		t.Errorf("beads-egress external-DB ipBlock must sit in the SAME rule as port:5432 (not split), got rule slice:\n%s", rest)
	}
}

// Multiple CIDRs (e.g. multi-AZ failover, reader endpoint) render as multiple
// ipBlock entries under a single ports: [5432] rule — preserving "DB only".
func TestBeadsEgress_ExternalDBAllow_MultiCIDR(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"postgres.enabled=true",
		`networkPolicies.beads.externalDatabaseCIDRs[0]=10.30.155.42/32`,
		`networkPolicies.beads.externalDatabaseCIDRs[1]=10.30.156.0/24`,
	)
	beadsDoc := sliceDoc(out, "name: test-beads-egress")
	if !strings.Contains(beadsDoc, "cidr: 10.30.155.42/32") ||
		!strings.Contains(beadsDoc, "cidr: 10.30.156.0/24") {
		t.Errorf("beads-egress must render every configured external-DB CIDR, got:\n%s", beadsDoc)
	}
}

// Default-deny-egress is off => the beads-egress policy doesn't render at all
// and there is no external-DB allow to leak even when CIDRs are set. Guards
// against accidentally minting an egress allow on an otherwise-unenforced
// cluster.
func TestBeadsEgress_DoesNotRender_WhenDefaultDenyEgressOff(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=false",
		`networkPolicies.beads.externalDatabaseCIDRs[0]=10.30.155.42/32`,
	)
	// The whole default-deny-egress.yaml is gated on both flags being true;
	// helm --show-only will error with "could not find template" when nothing
	// renders OR return empty. Either is acceptable; what is NOT acceptable
	// is for it to render and carry the ipBlock allow.
	if err == nil && strings.Contains(out, "cidr: 10.30.155.42/32") {
		t.Errorf("default-deny-egress + per-component allows must NOT render when defaultDenyEgress=false, got:\n%s", out)
	}
}

// ── Gap 2: grafana-bridge NP pair ───────────────────────────────────────────

// Default chart shape: grafanaBridge.enabled is false => no grafana-bridge
// NetworkPolicy pair renders, even with networkPolicies.enabled=true.
func TestGrafanaBridge_NoNetworkPolicies_WhenDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"grafanaBridge.enabled=false",
	)
	if strings.Contains(out, "test-grafana-bridge-egress") {
		t.Errorf("grafana-bridge-egress must NOT render when grafanaBridge.enabled=false, got:\n%s", out)
	}
	if strings.Contains(out, "test-grafana-bridge-ingress") {
		t.Errorf("grafana-bridge-ingress must NOT render when grafanaBridge.enabled=false, got:\n%s", out)
	}
}

// Pair renders when the bridge is on. Each policy selects only the bridge pod.
func TestGrafanaBridge_NetworkPolicyPair_Renders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"grafanaBridge.enabled=true",
	)

	ingressDoc := sliceDoc(out, "name: test-grafana-bridge-ingress")
	if ingressDoc == "" {
		t.Fatalf("grafana-bridge-ingress must render when enabled, got:\n%s", out)
	}
	if !strings.Contains(ingressDoc, "app.kubernetes.io/component: grafana-bridge") {
		t.Errorf("grafana-bridge-ingress must select component=grafana-bridge, got:\n%s", ingressDoc)
	}
	// Defensive ingress: no `ingress:` rule list — selects the pod into the
	// Ingress policyType so the union is deny-all-ingress (kubelet probes
	// bypass NP). Guards against accidentally opening this pod to in-cluster
	// callers when no Service exists.
	if strings.Contains(ingressDoc, "\n  ingress:\n") {
		t.Errorf("grafana-bridge-ingress must NOT render any ingress rules (no in-cluster dialers), got:\n%s", ingressDoc)
	}

	egressDoc := sliceDoc(out, "name: test-grafana-bridge-egress")
	if egressDoc == "" {
		t.Fatalf("grafana-bridge-egress must render when enabled, got:\n%s", out)
	}
	if !strings.Contains(egressDoc, "app.kubernetes.io/component: grafana-bridge") {
		t.Errorf("grafana-bridge-egress must select component=grafana-bridge, got:\n%s", egressDoc)
	}
	// Beads HTTP (the bridge subscribes to SSE + posts).
	if !strings.Contains(egressDoc, "app.kubernetes.io/component: beads") {
		t.Errorf("grafana-bridge-egress must allow beads (SSE + HTTP), got:\n%s", egressDoc)
	}
	// kube-dns.
	if !strings.Contains(egressDoc, "k8s-app: kube-dns") {
		t.Errorf("grafana-bridge-egress must allow kube-dns, got:\n%s", egressDoc)
	}
	// kube-apiserver path (ConfigMap write). The default chart leaves
	// kubeApiServer.cidrs empty, so only the internet-443-minus-RFC1918 rule
	// renders. Either form is valid — assert *some* :443 egress exists.
	if !strings.Contains(egressDoc, "port: 443") {
		t.Errorf("grafana-bridge-egress must reach the kube-apiserver on :443, got:\n%s", egressDoc)
	}
	// No port 80 — the bridge does HTTPS to apiserver only.
	if strings.Contains(egressDoc, "port: 80\n") {
		t.Errorf("grafana-bridge-egress must NOT carry a port-80 allow (apiserver is TLS), got:\n%s", egressDoc)
	}
}

// Pinned apiserver CIDR override (the prod posture: private apiserver IP
// excluded from the internet-443 rule) renders the ipBlock allow.
func TestGrafanaBridge_EgressPinnedApiserverCIDR(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"grafanaBridge.enabled=true",
		`networkPolicies.controller.kubeApiServer.cidrs[0]=172.20.0.1/32`,
	)
	egressDoc := sliceDoc(out, "name: test-grafana-bridge-egress")
	if !strings.Contains(egressDoc, "cidr: 172.20.0.1/32") {
		t.Errorf("grafana-bridge-egress must honor controller.kubeApiServer.cidrs (private apiserver), got:\n%s", egressDoc)
	}
}

// beads-ingress admits grafana-bridge so the bridge can reach the daemon under
// default-deny-ingress.
func TestBeadsIngress_AdmitsGrafanaBridge(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"grafanaBridge.enabled=true",
	)
	beadsIngress := sliceDoc(out, "name: test-beads-ingress")
	if beadsIngress == "" {
		t.Fatalf("beads-ingress must render, got:\n%s", out)
	}
	if !strings.Contains(beadsIngress, "app.kubernetes.io/component: grafana-bridge") {
		t.Errorf("beads-ingress must admit grafana-bridge (so the bridge can subscribe to SSE), got:\n%s", beadsIngress)
	}
}

// ── Prod overlay shape: the live composition the bead is forward-looking for.

// NOTE (ka-0000002553): the former TestProdOverlay_NetworkPolicyShape rendered
// the prod overlay deploy-values/gardenio.yaml and asserted no prod component was
// left without an egress path under defaultDenyEgress (beads -> external Aurora
// 10.30.155.42/32:5432, the grafana-bridge NP pair, the pinned apiserver CIDR
// 172.20.0.1/32, and beads-ingress admitting grafana-bridge). That legacy-909
// overlay was DELETED 2026-07-08 (prod values now live in
// deploy-values/gardenio-k3s.yaml). Every chart-testable assertion it held is
// re-homed onto --set-driven sibling tests in this file:
//   - beads -> external Aurora (10.30.155.42/32 on TCP/5432, scoped): see
//     TestBeadsEgress_ExternalDBAllow_RendersScoped.
//   - grafana-bridge ingress+egress pair reaching beads + kube-dns: see
//     TestGrafanaBridge_NetworkPolicyPair_Renders.
//   - grafana-bridge egress pinning the private apiserver CIDR (172.20.0.1/32):
//     see TestGrafanaBridge_EgressPinnedApiserverCIDR.
//   - beads-ingress admitting grafana-bridge (SSE subscriber): see
//     TestBeadsIngress_AdmitsGrafanaBridge.
// The prod-specific enablement (which of these the terraform render actually
// turns on, and NP enforcement itself — still disabled cluster-wide) is a
// terraform-composition concern, not a chart test.

// ── Post-!1754 components: default-deny-egress coverage (ka-0000001605) ──────
//
// Six components shipped AFTER MR !1754 added the default-deny-egress family,
// so they had NO egress allow rule and would be fully blocked the day NP
// enforcement is enabled cluster-wide (it is still disabled on prod-gardenio-eks
// — aws-eks-nodeagent --enable-network-policy=false — so these are inert today).
// These tests pin each new per-component egress policy's shape (scif-portal,
// one of the six, was deleted 2026-10-31, gd-Y4ZCGr2m2t):
//   trigger-engine              → in-cluster beads + kube-dns
//   vlsingle, vtsingle          → storage sinks, kube-dns only (no off-box push)
//   perf/dev-traces-relay       → in-cluster vtsingle :10428 + kube-dns + :443
// All internet allows exclude RFC1918 + IMDS (169.254/16) + CGNAT so the escape
// hatch never re-opens in-cluster reach or the node metadata endpoint.

func TestTriggerEngineEgress_RendersScoped(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"triggerEngine.enabled=true",
	)
	doc := sliceDoc(out, "name: test-trigger-engine-egress")
	if doc == "" {
		t.Fatalf("trigger-engine-egress must render when triggerEngine.enabled=true, got:\n%s", out)
	}
	if !strings.Contains(doc, "app.kubernetes.io/component: trigger-engine") {
		t.Errorf("trigger-engine-egress must select the trigger-engine component, got:\n%s", doc)
	}
	if !strings.Contains(doc, "app.kubernetes.io/component: beads") {
		t.Errorf("trigger-engine-egress must allow the in-cluster beads daemon (bead-lifecycle subscription), got:\n%s", doc)
	}
	if !strings.Contains(doc, "k8s-app: kube-dns") {
		t.Errorf("trigger-engine-egress must allow kube-dns, got:\n%s", doc)
	}
	// No off-cluster reach: the trigger-engine is fully in-cluster.
	if strings.Contains(doc, "ipBlock:") {
		t.Errorf("trigger-engine-egress must NOT open any internet allow (in-cluster only), got:\n%s", doc)
	}
}

func TestTriggerEngineEgress_AbsentWhenDisabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"triggerEngine.enabled=false",
	)
	if strings.Contains(out, "name: test-trigger-engine-egress") {
		t.Errorf("trigger-engine-egress must NOT render when triggerEngine.enabled=false, got:\n%s", out)
	}
}

func TestVlsingleEgress_DNSOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"vlsingle.enabled=true",
	)
	doc := sliceDoc(out, "name: test-vlsingle-egress")
	if doc == "" {
		t.Fatalf("vlsingle-egress must render when vlsingle.enabled=true, got:\n%s", out)
	}
	if !strings.Contains(doc, "app.kubernetes.io/component: vlsingle") {
		t.Errorf("vlsingle-egress must select the vlsingle component, got:\n%s", doc)
	}
	if !strings.Contains(doc, "k8s-app: kube-dns") {
		t.Errorf("vlsingle-egress must allow kube-dns, got:\n%s", doc)
	}
	// Pure storage sink — no internet allow, no :443.
	if strings.Contains(doc, "ipBlock:") {
		t.Errorf("vlsingle-egress is a storage sink and must NOT open any internet allow, got:\n%s", doc)
	}
}

func TestVtsingleEgress_DNSOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"vtsingle.enabled=true",
	)
	doc := sliceDoc(out, "name: test-vtsingle-egress")
	if doc == "" {
		t.Fatalf("vtsingle-egress must render when vtsingle.enabled=true, got:\n%s", out)
	}
	if !strings.Contains(doc, "app.kubernetes.io/component: vtsingle") {
		t.Errorf("vtsingle-egress must select the vtsingle component, got:\n%s", doc)
	}
	if !strings.Contains(doc, "k8s-app: kube-dns") {
		t.Errorf("vtsingle-egress must allow kube-dns, got:\n%s", doc)
	}
	if strings.Contains(doc, "ipBlock:") {
		t.Errorf("vtsingle-egress is a storage sink and must NOT open any internet allow, got:\n%s", doc)
	}
}

// The relays are gated on BOTH vtsingle.enabled AND their own relay toggle; the
// egress policy must appear only when both are set, and must allow the
// in-cluster vtsingle OTLP insert (:10428) plus an external :443 source pull.
func TestPerfTracesRelayEgress_RendersScoped(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"vtsingle.enabled=true",
		"vtsingle.relay.enabled=true",
	)
	doc := sliceDoc(out, "name: test-perf-traces-relay-egress")
	if doc == "" {
		t.Fatalf("perf-traces-relay-egress must render when vtsingle.relay.enabled=true, got:\n%s", out)
	}
	if !strings.Contains(doc, "app.kubernetes.io/component: perf-traces-relay") {
		t.Errorf("perf-traces-relay-egress must select the perf-traces-relay component, got:\n%s", doc)
	}
	if !strings.Contains(doc, "port: 10428") {
		t.Errorf("perf-traces-relay-egress must allow the in-cluster vtsingle OTLP insert (:10428), got:\n%s", doc)
	}
	if !strings.Contains(doc, "port: 443") {
		t.Errorf("perf-traces-relay-egress must allow :443 for the external source pull, got:\n%s", doc)
	}
	if !strings.Contains(doc, "169.254.0.0/16") {
		t.Errorf("perf-traces-relay-egress :443 allow must exclude IMDS/link-local (169.254.0.0/16), got:\n%s", doc)
	}
}

func TestPerfTracesRelayEgress_AbsentWithoutVtsingle(t *testing.T) {
	t.Parallel()
	// relay toggle on but vtsingle off → no policy (the destination does not exist).
	out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"vtsingle.enabled=false",
		"vtsingle.relay.enabled=true",
	)
	if strings.Contains(out, "name: test-perf-traces-relay-egress") {
		t.Errorf("perf-traces-relay-egress must NOT render when vtsingle.enabled=false, got:\n%s", out)
	}
}

func TestDevTracesRelayEgress_RendersScoped(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
		"vtsingle.enabled=true",
		"vtsingle.devRelay.enabled=true",
	)
	doc := sliceDoc(out, "name: test-dev-traces-relay-egress")
	if doc == "" {
		t.Fatalf("dev-traces-relay-egress must render when vtsingle.devRelay.enabled=true, got:\n%s", out)
	}
	if !strings.Contains(doc, "app.kubernetes.io/component: dev-traces-relay") {
		t.Errorf("dev-traces-relay-egress must select the dev-traces-relay component, got:\n%s", doc)
	}
	if !strings.Contains(doc, "port: 10428") {
		t.Errorf("dev-traces-relay-egress must allow the in-cluster vtsingle OTLP insert (:10428), got:\n%s", doc)
	}
	if !strings.Contains(doc, "port: 443") {
		t.Errorf("dev-traces-relay-egress must allow :443 for the external source pull, got:\n%s", doc)
	}
}
