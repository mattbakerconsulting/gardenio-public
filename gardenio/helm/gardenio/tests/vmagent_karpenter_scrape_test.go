package tests

import (
	"strings"
	"testing"
)

// Karpenter cross-namespace scrape (GAP-2 prereq ka-0000006900, epic
// ka-0000000770). The in-chart vmagent otherwise discovers ONLY
// prometheus.io/scrape=true pods in its OWN namespace, so Karpenter (which
// runs in the `karpenter` namespace on prod-gardenio-eks) is never scraped and
// vmsingle carries no karpenter_* series — the GAP-2 AgentPoolMemoryNearLimit
// alert then evaluates a missing series and can never fire. Enabling
// vmagent.karpenterScrape adds THREE coordinated pieces, all gated on the same
// knob: a `karpenter` scrape job (endpoints SD, keep-filtered), a ClusterRole
// for cross-namespace discovery, and a vmagent→karpenter egress allowance.
// These tests pin both the opt-in render AND the default-off invariant so the
// chart stays byte-identical for every env that does not set the knob.

// --- default-off invariant --------------------------------------------------

func TestKarpenterScrape_ConfigMap_DefaultAbsent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
	)
	if strings.Contains(out, "job_name: karpenter") {
		t.Errorf("karpenter scrape job must NOT render when karpenterScrape.enabled is false; got:\n%s", out)
	}
	// The default kubernetes-pods job must always be present.
	if !strings.Contains(out, "job_name: kubernetes-pods") {
		t.Error("kubernetes-pods discovery job should always render")
	}
}

func TestKarpenterScrape_RBAC_DefaultNoClusterRole(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/rbac.yaml",
		"vmagent.enabled=true",
	)
	if strings.Contains(out, "ClusterRole") {
		t.Errorf("no ClusterRole/ClusterRoleBinding may render when karpenterScrape.enabled is false; got:\n%s", out)
	}
	// The namespace-scoped Role must still be present.
	if !strings.Contains(out, "kind: Role") {
		t.Error("the namespace-scoped vmagent Role should always render")
	}
}

func TestKarpenterScrape_NetworkPolicy_DefaultNoKarpenterEgress(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"vmagent.enabled=true",
		"networkPolicies.enabled=true",
	)
	// The karpenter-namespace egress selector must not appear by default.
	if strings.Contains(out, "kubernetes.io/metadata.name: \"karpenter\"") {
		t.Errorf("vmagent→karpenter egress must NOT render when karpenterScrape.enabled is false; got:\n%s", out)
	}
}

// --- opt-in render ----------------------------------------------------------

func TestKarpenterScrape_ConfigMap_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
		"vmagent.karpenterScrape.enabled=true",
	)
	if !strings.Contains(out, "job_name: karpenter") {
		t.Fatalf("expected karpenter scrape job; got:\n%s", out)
	}
	// Endpoints SD (NOT the Service ClusterIP): Karpenter runs 2 HA replicas
	// behind one Service and only the leader emits the nodepool gauges, so a
	// ClusterIP scrape would round-robin and flap the series. Scraping each
	// Endpoint keeps the leader's series continuously present.
	if !strings.Contains(out, "role: endpoints") {
		t.Error("karpenter job must use the endpoints SD role, not a static ClusterIP target, to avoid HA leader-flap")
	}
	// Scoped to the karpenter namespace.
	if !strings.Contains(out, "- \"karpenter\"") {
		t.Error("karpenter job SD must be scoped to the karpenter namespace")
	}
	// The keep-filter must retain the GAP-2 primary series and drop the bulk
	// of the ~45k-sample raw endpoint.
	for _, fam := range []string{"karpenter_nodepools_usage", "karpenter_nodepools_limit"} {
		if !strings.Contains(out, fam) {
			t.Errorf("keep-filter must retain the GAP-2 primary family %q", fam)
		}
	}
	if !strings.Contains(out, "action: keep") {
		t.Error("expected a metric_relabel keep action bounding karpenter cardinality")
	}
	// Extra jobs append — the default discovery job survives.
	if !strings.Contains(out, "job_name: kubernetes-pods") {
		t.Error("the karpenter job must append to, not replace, the kubernetes-pods job")
	}
	// The metrics-port keep filter must match on the scrape __address__ port
	// suffix, NOT concatenate two SD port labels. A `source_labels:
	// [<port_name>, <port_number>]` keep joins them under the default `;`
	// separator ("http-metrics;8080"), which an anchored `http-metrics|8080`
	// regex can never match — that would silently drop EVERY karpenter target
	// and land zero series. Pin the address-suffix form so that regression
	// cannot come back.
	if !strings.Contains(out, "source_labels: [__address__]") {
		t.Error("metrics-port keep filter must select on __address__ (its port is the actual scrape port), not on concatenated SD port labels")
	}
	if !strings.Contains(out, `regex: ".+:8080"`) {
		t.Errorf("expected the __address__ keep regex to bound the scrape to the metrics port (.+:8080); got:\n%s", out)
	}
}

// An operator who deliberately empties keepMetrics must get an UNBOUNDED scrape
// (no keep rule), NOT a rule that drops everything. `join "|"` on an empty list
// renders `regex: "()"` == `^()$`, which a `keep` action matches only against
// the empty string — dropping every karpenter series silently. The template
// guards the metric_relabel block on a non-empty keepMetrics, so an emptied
// list omits the keep entirely (and the whole metric_relabel_configs key, since
// it holds only that rule).
func TestKarpenterScrape_ConfigMap_EmptyKeepMetricsOmitsKeep(t *testing.T) {
	t.Parallel()
	out := helmTemplateJSON(t, "templates/vmagent/configmap.yaml",
		map[string]string{"vmagent.karpenterScrape.keepMetrics": "[]"},
		"vmagent.enabled=true",
		"vmagent.karpenterScrape.enabled=true",
	)
	i := strings.Index(out, "job_name: karpenter")
	if i < 0 {
		t.Fatalf("expected karpenter scrape job to still render with an empty keepMetrics; got:\n%s", out)
	}
	// Scope the assertion to the karpenter job region only. The default
	// kubernetes-pods job (earlier in the file) always carries its own
	// metric_relabel_configs (the PII backstop), so a whole-file substring
	// check would false-positive. The karpenter job is last before the
	// "Extra scrape jobs" trailer, so slice from its job_name to end-of-file.
	karpenterBlock := out[i:]
	if strings.Contains(karpenterBlock, `regex: "()"`) {
		t.Errorf("empty keepMetrics must NOT render a match-nothing keep regex `()` that drops every series; got:\n%s", karpenterBlock)
	}
	if strings.Contains(karpenterBlock, "metric_relabel_configs:") {
		t.Errorf("empty keepMetrics must omit the karpenter metric_relabel keep block entirely (unbounded scrape), not render an empty-regex keep; got:\n%s", karpenterBlock)
	}
}

func TestKarpenterScrape_RBAC_ClusterRoleRendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/rbac.yaml",
		"vmagent.enabled=true",
		"vmagent.karpenterScrape.enabled=true",
	)
	if !strings.Contains(out, "kind: ClusterRole") {
		t.Fatalf("expected a ClusterRole for cross-namespace discovery; got:\n%s", out)
	}
	if !strings.Contains(out, "kind: ClusterRoleBinding") {
		t.Error("expected a ClusterRoleBinding binding the vmagent SA to the cross-namespace ClusterRole")
	}
	// Verbs must stay minimal + read-only (no writes, no secrets, no exec).
	if !strings.Contains(out, `verbs: ["get", "list", "watch"]`) {
		t.Error("cross-namespace discovery ClusterRole must grant only get/list/watch")
	}
	// The granted resource list must be exactly the promscrape discovery set —
	// pods/endpoints/services — and NOT include secrets (checked on the
	// resources: rule line, not the whole render, whose comments mention the
	// word). A cluster-wide secrets read would be a real privilege escalation.
	if !strings.Contains(out, `resources: ["pods", "endpoints", "services"]`) {
		t.Error("cross-namespace discovery ClusterRole must grant only pods/endpoints/services")
	}
	if strings.Contains(out, `resources: ["secrets"`) || strings.Contains(out, `"secrets"]`) {
		t.Error("cross-namespace discovery ClusterRole must NOT grant access to secrets")
	}
}

func TestKarpenterScrape_NetworkPolicy_EgressRendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"vmagent.enabled=true",
		"vmagent.karpenterScrape.enabled=true",
		"networkPolicies.enabled=true",
	)
	// The vmagent-egress policy must gain a rule selecting the karpenter
	// namespace on the metrics port; without it default-deny-egress drops the
	// cross-namespace scrape connection.
	if !strings.Contains(out, "kubernetes.io/metadata.name: \"karpenter\"") {
		t.Fatalf("expected vmagent→karpenter namespace egress selector; got:\n%s", out)
	}
	if !strings.Contains(out, "port: 8080") {
		t.Error("expected the karpenter metrics port (8080) in the vmagent egress allowance")
	}
}
