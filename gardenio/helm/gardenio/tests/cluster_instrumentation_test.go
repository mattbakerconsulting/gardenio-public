package tests

import (
	"strings"
	"testing"
)

// Cluster-instrumentation knobs for the vm-operator monitoring stack
// (ka-0000006163 — "Instrument the cluster"). prod-gardenio-eks's cluster
// observability store (the vm-operator `vmsingle-vm-single`) holds only
// application-level series: no kube_pod_container_resource_* (no cluster
// kube-state-metrics) and no container_cpu_usage_seconds_total /
// container_memory_working_set_bytes (no cadvisor scrape). Two default-off
// knobs close the gap, discovered by the monitoring vm-agent
// (selectAllByDefault:true) with no operator/RBAC change:
//
//   - monitoring.clusterMetrics.kubeStateMetrics: a CLUSTER-scoped KSM
//     (ClusterRole pods+nodes, --metric-allowlist) + a VMServiceScrape →
//     kube_pod_container_resource_requests/limits + node capacity/allocatable.
//     Distinct from the namespace-scoped monitoring.podStateMetrics, which
//     cannot see agent pods in gardenio / gardenio-e2e-* namespaces.
//   - monitoring.clusterMetrics.kubeletCadvisor: a VMNodeScrape of every
//     kubelet's /metrics/cadvisor → the two per-container CPU/memory usage
//     families, keep-filtered and id/image/name-labeldropped for bounded
//     cardinality.
//
// These tests pin the opt-in render AND the default-off invariant so the chart
// stays byte-identical for every env that does not set the knobs.

// --- default-off invariants -------------------------------------------------

func TestClusterInstrumentation_DefaultAbsent(t *testing.T) {
	t.Parallel()
	// helm -s errors when a template renders empty — that IS the default-off
	// invariant here (same posture as the pod-state-metrics exporter test).
	ksm, err := helmTemplateAllowFail(t, "templates/monitoring/cluster-state-metrics.yaml")
	if err == nil && strings.Contains(ksm, "kind: Deployment") {
		t.Errorf("cluster-state-metrics must NOT render by default; got:\n%s", ksm)
	}
	cad, err := helmTemplateAllowFail(t, "templates/monitoring/kubelet-cadvisor-vmnodescrape.yaml")
	if err == nil && strings.Contains(cad, "kind: VMNodeScrape") {
		t.Errorf("kubelet-cadvisor VMNodeScrape must NOT render by default; got:\n%s", cad)
	}
}

// --- cluster kube-state-metrics opt-in --------------------------------------

func TestClusterStateMetrics_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/monitoring/cluster-state-metrics.yaml",
		"monitoring.clusterMetrics.kubeStateMetrics.enabled=true",
	)
	// Cluster-scoped: a ClusterRole (not a namespace Role) over pods+nodes is
	// what lets this KSM attribute usage across the agent namespaces the
	// namespace-scoped sibling cannot see.
	if !strings.Contains(out, "kind: ClusterRole") {
		t.Fatalf("cluster-state-metrics must render a ClusterRole (cluster scope); got:\n%s", out)
	}
	if !strings.Contains(out, `resources: ["pods", "nodes"]`) {
		t.Error("ClusterRole must grant pods+nodes list/watch")
	}
	// No --namespaces filter — the whole point is cluster-wide visibility.
	// Match the rendered flag arg form, not the word in the comment prose.
	if strings.Contains(out, `"--namespaces=`) {
		t.Error("cluster KSM must NOT be namespace-scoped (--namespaces present)")
	}
	if !strings.Contains(out, "--resources=pods,nodes") {
		t.Errorf("KSM must scope to pods,nodes; got:\n%s", out)
	}
	// The allow-list bounds what enters the shared store's retention.
	for _, fam := range []string{
		"kube_pod_container_resource_requests",
		"kube_pod_container_resource_limits",
		"kube_node_status_allocatable",
		"kube_node_status_capacity",
	} {
		if !strings.Contains(out, fam) {
			t.Errorf("--metric-allowlist must retain %q", fam)
		}
	}
	// A VMServiceScrape with honorLabels is the only path these series reach
	// vmsingle-vm-single (honorLabels keeps the SUBJECT pod/node identity KSM
	// stamps, which the default target relabels would otherwise overwrite).
	if !strings.Contains(out, "kind: VMServiceScrape") {
		t.Error("cluster-state-metrics must emit a VMServiceScrape")
	}
	if !strings.Contains(out, "honorLabels: true") {
		t.Error("VMServiceScrape must set honorLabels: true")
	}
}

// An emptied metricAllowlist must omit the flag entirely (every pods/nodes
// family) — `--metric-allowlist=` with an empty value makes KSM emit nothing.
func TestClusterStateMetrics_EmptyAllowlistOmitsFlag(t *testing.T) {
	t.Parallel()
	out := helmTemplateJSON(t, "templates/monitoring/cluster-state-metrics.yaml",
		map[string]string{"monitoring.clusterMetrics.kubeStateMetrics.metricAllowlist": "[]"},
		"monitoring.clusterMetrics.kubeStateMetrics.enabled=true",
	)
	// Match the rendered flag arg, not the word's appearance in the template's
	// comment prose.
	if strings.Contains(out, `"--metric-allowlist=`) {
		t.Errorf("empty metricAllowlist must omit the --metric-allowlist flag; got:\n%s", out)
	}
}

// --- kubelet/cadvisor VMNodeScrape opt-in -----------------------------------

func TestKubeletCadvisor_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/monitoring/kubelet-cadvisor-vmnodescrape.yaml",
		"monitoring.clusterMetrics.kubeletCadvisor.enabled=true",
	)
	if !strings.Contains(out, "kind: VMNodeScrape") {
		t.Fatalf("expected a VMNodeScrape; got:\n%s", out)
	}
	// The cadvisor endpoint over the kubelet's TLS port: https + SA bearer +
	// insecureSkipVerify (self-signed kubelet cert, Webhook-authorized).
	if !strings.Contains(out, "scheme: https") {
		t.Error("VMNodeScrape must scrape over https (kubelet serves no plaintext metrics)")
	}
	if !strings.Contains(out, "path: /metrics/cadvisor") {
		t.Error("VMNodeScrape must target /metrics/cadvisor")
	}
	if !strings.Contains(out, "bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token") {
		t.Error("VMNodeScrape must authenticate with the SA bearer token")
	}
	if !strings.Contains(out, "insecureSkipVerify: true") {
		t.Error("VMNodeScrape must set insecureSkipVerify (self-signed kubelet cert)")
	}
	// keep-filter pins the two capacity families; labeldrop strips cadvisor's
	// high-cardinality id/image/name so the shared store's retention stays bounded.
	if !strings.Contains(out, "container_cpu_usage_seconds_total") ||
		!strings.Contains(out, "container_memory_working_set_bytes") {
		t.Errorf("keep-filter must retain both capacity families; got:\n%s", out)
	}
	if !strings.Contains(out, "action: labeldrop") || !strings.Contains(out, `regex: "id|image|name"`) {
		t.Errorf("VMNodeScrape must labeldrop id|image|name; got:\n%s", out)
	}
}
