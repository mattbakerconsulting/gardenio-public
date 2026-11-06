package tests

import (
	"strings"
	"testing"
)

// Agent-pod → external e2e-cloud Keycloak egress regression guard (ka-0000006885).
//
// Background. During the FSM-468 monorepo-repro run (gd-cQUI4Yetg0) an agent
// pod could not reach the e2e-cloud Keycloak OIDC endpoint at
// https://e2e.dev.acme.example/auth and the run silently fell back to devel —
// a soft failure of the pipeline invariant (gd-2CzVbJlYxW) that the canonical
// default is a fresh deploy-MR on e2e, and a HARD false-negative for any repro
// that needs MR-scope isolation.
//
// Live diagnosis (2026-08-13, probed from a gardenio.io/agent pod): the network
// path is HEALTHY. e2e.dev.acme.example / auth.e2e.dev.acme.example resolve to PUBLIC AWS
// IPs (203.0.113.10 / 203.0.113.11 / 203.0.113.12 here; RFC 5737 stand-ins) and
//   curl https://e2e.dev.acme.example/auth/realms/acme/.well-known/openid-configuration
// returns 200 in <200ms. What carries that traffic is the broad-internet leg of
// agent egress: the `<release>-agent-internet-egress` NetworkPolicy allows
// 0.0.0.0/0 on 443/80 minus the RFC1918/IMDS/CGNAT except-list. e2e Keycloak is
// a PUBLIC endpoint, so it is NOT in the except-list and the path is open. The
// original 2026-07-23 hang was an upstream Keycloak availability wedge, not a
// dropped SYN.
//
// The single chart-level change that WOULD turn this into a real, permanent
// network fault is any edit that stops non-dark agent pods from egressing to
// public HTTPS — e.g. dropping the internet leg, or (the subtle one)
// accidentally widening the ipBlock `except` list to swallow the public CIDRs
// that e2e.dev.acme.example lives in. This test fails closed on exactly that
// regression so a future netpol tightening cannot silently re-strand e2e repro.
//
// It asserts the DEFAULT reachability that the e2e pipeline invariant depends
// on. (The opt-in claudemore.isolation.egressAllowlist tightening that used to
// drop this leg left the chart with claudemore, gd-Y4ZCGr2m2t; the leg is now
// unconditional.)

// The public IPs e2e.dev.acme.example / auth.e2e.dev.acme.example currently resolve to.
// The guard does not require these exact addresses to be reachable — it asserts
// the egress policy does not carve them (or the public space they sit in) out
// of the broad-internet allow.
var e2eKeycloakPublicIPs = []string{
	"203.0.113.10",
	"203.0.113.11",
	"203.0.113.12",
}

// On a default deploy, a non-dark agent pod MUST retain a broad
// external-HTTPS egress leg so it can reach the public e2e-cloud Keycloak. This
// is the exact leg whose removal reproduces ka-0000006885 as a network fault.
func TestAgentEgress_ReachesExternalE2EKeycloak_ByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"agents.enabled=true",
	)

	seg := sliceNetPol(out, "-agent-internet-egress")
	if seg == "" {
		t.Fatal("expected the agent-internet-egress NetworkPolicy to render on a default deploy — without it, agent pods cannot reach public e2e-cloud Keycloak (https://e2e.dev.acme.example/auth) and monorepo-repro falls back to devel (ka-0000006885)")
	}

	// The broad public-internet allow must be present…
	if !strings.Contains(seg, "cidr: 0.0.0.0/0") {
		t.Error("agent-internet-egress MUST allow the public internet (0.0.0.0/0) so agent pods reach the public e2e Keycloak endpoint — dropping this leg re-strands e2e repro (ka-0000006885)")
	}
	// …on 443 (the Keycloak OIDC endpoint is HTTPS).
	if !strings.Contains(seg, "port: 443") {
		t.Error("agent-internet-egress MUST allow TCP :443 — the e2e Keycloak OIDC endpoint (https://e2e.dev.acme.example/auth/realms/acme) is HTTPS (ka-0000006885)")
	}

	// The except-list is the subtle regression surface: it must exclude ONLY
	// private/metadata ranges, never a public CIDR that would swallow the e2e
	// Keycloak ALB. RFC1918 + IMDS + CGNAT are the sanctioned carve-outs; none
	// of them contain the e2e public IPs, so all must remain reachable.
	for _, ip := range e2eKeycloakPublicIPs {
		if excludesPublicIP(seg, ip) {
			t.Errorf("agent-internet-egress except-list swallows the public e2e Keycloak IP %s — a future netpol tightening must not carve public CIDRs out of the agent broad-internet allow or it re-strands e2e repro (ka-0000006885)", ip)
		}
	}
}

// The e2e Keycloak reachability leg must NOT depend on the dark marker: monorepo
// repro agents are non-dark, and the dark-agent-egress fence (ka-0000005242)
// only removes Atlassian CIDRs, never the public e2e Keycloak. Guard that the
// non-dark internet policy is the one that carries this traffic and is scoped to
// exclude dark pods (so the two legs stay disjoint and neither loses the e2e path).
func TestAgentEgress_E2EKeycloakLeg_IsNonDarkScoped(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"agents.enabled=true",
	)
	seg := sliceNetPol(out, "-agent-internet-egress")
	if seg == "" {
		t.Fatal("expected the agent-internet-egress NetworkPolicy to render")
	}
	if !strings.Contains(seg, "gardenio.io/agent") {
		t.Error("agent-internet-egress must select agent pods via gardenio.io/agent so monorepo-repro pods inherit the e2e Keycloak egress leg (ka-0000006885)")
	}
	if !strings.Contains(seg, "gardenio.io/dark") || !strings.Contains(seg, "DoesNotExist") {
		t.Error("agent-internet-egress must exclude dark pods (gardenio.io/dark DoesNotExist) so the non-dark and dark internet legs stay disjoint (ka-0000005242); both must keep the public e2e Keycloak path (ka-0000006885)")
	}
}

// excludesPublicIP reports whether the rendered NetworkPolicy segment carves the
// given IP out of its broad-internet allow. It is a conservative textual check:
// it flags an exact "- <ip>" line or an "- <ip>/NN" CIDR entry inside an
// except: list. The sanctioned carve-outs (10./172.16./192.168./169.254./100.64.)
// never match a public IP, so a hit here means a real public-space carve-out.
func excludesPublicIP(seg, ip string) bool {
	for line := range strings.SplitSeq(seg, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "- ") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(t, "- "))
		// Strip an optional /NN suffix — only the exact-IP or /32 forms could
		// name a single public host; broader public CIDRs would be an even
		// clearer regression, but the sanctioned except-list has none.
		if slash := strings.IndexByte(v, '/'); slash >= 0 {
			v = v[:slash]
		}
		if v == ip {
			return true
		}
	}
	return false
}
