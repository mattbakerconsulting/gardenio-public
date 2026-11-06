package tests

import (
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// gd-VgdO1sx1Ju: under networkPolicies.defaultDenyEgress the release-deployer
// CronJob pod is selected by the namespace-wide deny, so every callout of the
// entrypoint and the tagged release-deploy.sh needs an allow, or an autonomous
// roll fails on the first release that turns NPs on:
//
//   - the kube API (kubectl + helm): kubeApiServer.cidrs:443 on EKS, the
//     post-DNAT private-range :6443 on k3s
//   - GitHub (git ls-remote/clone, gh api) and a public EKS endpoint: the
//     internet-443 rule, RFC1918/IMDS/CGNAT still excluded
//   - the beads daemon http port (KD_URL reports), with the matching
//     beads-ingress peer, http only
//   - kube-dns, and keycloak-runtime:8443 only when the machine principal
//     fetches its token there

type rdNetPolPeer struct {
	PodSelector       map[string]any `json:"podSelector"`
	NamespaceSelector map[string]any `json:"namespaceSelector"`
	IPBlock           *struct {
		CIDR   string   `json:"cidr"`
		Except []string `json:"except"`
	} `json:"ipBlock"`
}

type rdNetPolPort struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
}

type rdNetPolRule struct {
	To    []rdNetPolPeer `json:"to"`
	From  []rdNetPolPeer `json:"from"`
	Ports []rdNetPolPort `json:"ports"`
}

type rdNetPol struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		PodSelector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"podSelector"`
		PolicyTypes []string       `json:"policyTypes"`
		Egress      []rdNetPolRule `json:"egress"`
		Ingress     []rdNetPolRule `json:"ingress"`
	} `json:"spec"`
}

// rdNetPolicy parses the NetworkPolicy whose name ends in nameSuffix, or nil.
func rdNetPolicy(t *testing.T, out, nameSuffix string) *rdNetPol {
	t.Helper()
	seg := sliceNetPol(out, nameSuffix)
	if seg == "" {
		return nil
	}
	var np rdNetPol
	if err := yaml.Unmarshal([]byte(seg), &np); err != nil {
		t.Fatalf("parse %s: %v\n%s", nameSuffix, err, seg)
	}
	return &np
}

func rdPeerComponent(p rdNetPolPeer) string {
	ml, _ := p.PodSelector["matchLabels"].(map[string]any)
	c, _ := ml["app.kubernetes.io/component"].(string)
	return c
}

func rdPorts(r rdNetPolRule) []int {
	var ps []int
	for _, p := range r.Ports {
		ps = append(ps, p.Port)
	}
	return ps
}

// rdReachesComponent reports whether an egress rule allows the pod with the
// given component label on port.
func rdReachesComponent(np *rdNetPol, component string, port int) bool {
	for _, r := range np.Spec.Egress {
		for _, to := range r.To {
			if rdPeerComponent(to) == component && slices.Contains(rdPorts(r), port) {
				return true
			}
		}
	}
	return false
}

// rdReachesCIDR reports whether an egress rule allows ipBlock cidr on port.
func rdReachesCIDR(np *rdNetPol, cidr string, port int) bool {
	for _, r := range np.Spec.Egress {
		for _, to := range r.To {
			if to.IPBlock != nil && to.IPBlock.CIDR == cidr && slices.Contains(rdPorts(r), port) {
				return true
			}
		}
	}
	return false
}

// releaseDeployerNPRender renders the live k3s overlay with NPs and
// default-deny egress on, plus extra --set flags.
func releaseDeployerNPRender(t *testing.T, sets ...string) string {
	t.Helper()
	return helmTemplateFull(t, releaseDeployerRepoRootValues(), append([]string{
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	}, sets...)...)
}

func requireReleaseDeployerEgress(t *testing.T, out string) *rdNetPol {
	t.Helper()
	np := rdNetPolicy(t, out, "-release-deployer-egress")
	if np == nil {
		t.Fatalf("release-deployer-egress NetworkPolicy did not render under defaultDenyEgress")
	}
	if np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"] != "release-deployer" {
		t.Fatalf("release-deployer-egress selects %v, want component=release-deployer", np.Spec.PodSelector.MatchLabels)
	}
	if !slices.Equal(np.Spec.PolicyTypes, []string{"Egress"}) {
		t.Fatalf("release-deployer-egress policyTypes = %v, want [Egress]", np.Spec.PolicyTypes)
	}
	return np
}

// k3s: the deployer's kubectl/helm dial 10.43.0.1:443, which kube-proxy DNATs
// to <node-ip>:6443 before the egress ipBlocks are matched, so the
// apiServerPostDNAT allow is what reaches the kube API there.
func TestReleaseDeployerEgress_K3sReachesKubeAPI(t *testing.T) {
	t.Parallel()
	np := requireReleaseDeployerEgress(t, releaseDeployerNPRender(t, "networkPolicies.apiServerPostDNAT=true"))
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if !rdReachesCIDR(np, cidr, 6443) {
			t.Errorf("release-deployer cannot reach the k3s apiserver (post-DNAT %s:6443): %+v", cidr, np.Spec.Egress)
		}
	}
}

// EKS private endpoint: kubeApiServer.cidrs on 443, the controller's list.
func TestReleaseDeployerEgress_EKSReachesKubeAPI(t *testing.T) {
	t.Parallel()
	np := requireReleaseDeployerEgress(t, releaseDeployerNPRender(t,
		"networkPolicies.apiServerPostDNAT=false",
		"networkPolicies.controller.kubeApiServer.cidrs={172.20.0.1/32}"))
	if !rdReachesCIDR(np, "172.20.0.1/32", 443) {
		t.Errorf("release-deployer cannot reach the kube API at kubeApiServer.cidrs:443: %+v", np.Spec.Egress)
	}
	for _, r := range np.Spec.Egress {
		if slices.Contains(rdPorts(r), 6443) {
			t.Errorf("apiServerPostDNAT off: release-deployer must not get private :6443: %+v", r)
		}
	}
}

// GitHub (git + gh api) and a public EKS endpoint ride one internet-443 rule
// that keeps the no-pivot exceptions; nothing else leaves the cluster.
func TestReleaseDeployerEgress_InternetIs443WithNoPivot(t *testing.T) {
	t.Parallel()
	np := requireReleaseDeployerEgress(t, releaseDeployerNPRender(t))
	found := false
	for _, r := range np.Spec.Egress {
		for _, to := range r.To {
			if to.IPBlock == nil || to.IPBlock.CIDR != "0.0.0.0/0" {
				continue
			}
			found = true
			if ps := rdPorts(r); !slices.Equal(ps, []int{443}) {
				t.Errorf("internet rule ports = %v, want [443] only", ps)
			}
			for _, exc := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"} {
				if !slices.Contains(to.IPBlock.Except, exc) {
					t.Errorf("internet rule lost the %s exception: %v", exc, to.IPBlock.Except)
				}
			}
		}
	}
	if !found {
		t.Errorf("release-deployer has no internet-443 rule: git ls-remote/clone and gh api to GitHub are default-denied: %+v", np.Spec.Egress)
	}
}

// Beads http both ways, kube-dns, and keycloak-runtime only with the machine
// principal's https token fetch.
func TestReleaseDeployerEgress_BeadsDNSAndKeycloak(t *testing.T) {
	t.Parallel()
	out := releaseDeployerNPRender(t,
		"releaseDeployer.machinePrincipal.enabled=false")
	np := requireReleaseDeployerEgress(t, out)
	if !rdReachesComponent(np, "beads", 8080) {
		t.Errorf("release-deployer has no egress to beads:8080 (KD_URL reports): %+v", np.Spec.Egress)
	}
	dns := false
	for _, r := range np.Spec.Egress {
		for _, to := range r.To {
			if to.NamespaceSelector != nil && slices.Contains(rdPorts(r), 53) {
				dns = true
			}
		}
	}
	if !dns {
		t.Errorf("release-deployer has no kube-dns egress: %+v", np.Spec.Egress)
	}
	if rdReachesComponent(np, "keycloak-runtime", 8443) {
		t.Errorf("machinePrincipal off: release-deployer must not reach keycloak-runtime:8443")
	}

	ing := rdNetPolicy(t, out, "-beads-ingress")
	if ing == nil {
		t.Fatalf("beads-ingress did not render")
	}
	admitted := false
	for _, r := range ing.Spec.Ingress {
		for _, from := range r.From {
			if rdPeerComponent(from) != "release-deployer" {
				continue
			}
			admitted = true
			if ps := rdPorts(r); !slices.Equal(ps, []int{8080}) {
				t.Errorf("beads-ingress admits release-deployer on %v, want http [8080] only", ps)
			}
		}
	}
	if !admitted {
		t.Errorf("beads-ingress does not admit release-deployer: its KD_URL reports are default-denied")
	}

	np = requireReleaseDeployerEgress(t, releaseDeployerNPRender(t,
		"releaseDeployer.machinePrincipal.enabled=true"))
	if !rdReachesComponent(np, "keycloak-runtime", 8443) {
		t.Errorf("machinePrincipal on: release-deployer has no egress to keycloak-runtime:8443")
	}
}

// Off by default: no deployer, no allow (and no beads-ingress peer).
func TestReleaseDeployerEgress_AbsentWhenDisabled(t *testing.T) {
	t.Parallel()
	out := releaseDeployerNPRender(t, "releaseDeployer.enabled=false")
	if strings.Contains(out, "-release-deployer-egress\n") {
		t.Errorf("release-deployer-egress rendered with releaseDeployer.enabled=false")
	}
	if ing := rdNetPolicy(t, out, "-beads-ingress"); ing != nil {
		for _, r := range ing.Spec.Ingress {
			for _, from := range r.From {
				if rdPeerComponent(from) == "release-deployer" {
					t.Errorf("beads-ingress admits release-deployer with it disabled")
				}
			}
		}
	}
}
