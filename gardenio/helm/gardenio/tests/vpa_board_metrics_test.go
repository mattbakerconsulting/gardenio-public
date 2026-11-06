package tests

import (
	"strings"
	"testing"
)

// In-chart sources for the vpa-right-sizing board series (ka-0000000341).
// prod-gardenio-eks runs NO observability kube-prometheus stack, no cluster
// kube-state-metrics, and no cadvisor scrape, so the board's kube_*/
// container_* targets exist nowhere unless the chart provides them itself.
// Two default-off knobs close the gap:
//
//   - vmagent.kubeletScrape: node-SD scrape of every kubelet's
//     /metrics/resource → container_memory_working_set_bytes ("actual"),
//     namespace- and family-bounded before remote_write.
//   - monitoring.podStateMetrics: namespace-scoped KSM (pods resource,
//     --metric-allowlist) → kube_pod_container_resource_requests/limits +
//     OOM/restart series, scraped by a dedicated honor_labels vmagent job.
//
// These tests pin the opt-in render AND the default-off invariant so the
// chart stays byte-identical for every env that does not set the knobs.

// --- default-off invariants -------------------------------------------------

func TestVpaBoardMetrics_ConfigMap_DefaultAbsent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
	)
	if strings.Contains(out, "job_name: kubelet-resources") {
		t.Errorf("kubelet-resources job must NOT render when kubeletScrape.enabled is false; got:\n%s", out)
	}
	if strings.Contains(out, "job_name: pod-state-metrics") {
		t.Errorf("pod-state-metrics job must NOT render when podStateMetrics.enabled is false; got:\n%s", out)
	}
}

func TestVpaBoardMetrics_RBAC_DefaultNoKubeletClusterRole(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/rbac.yaml",
		"vmagent.enabled=true",
	)
	if strings.Contains(out, "kubelet-scrape") {
		t.Errorf("kubelet-scrape ClusterRole/Binding must NOT render when kubeletScrape.enabled is false; got:\n%s", out)
	}
}

func TestVpaBoardMetrics_Exporter_DefaultAbsent(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/monitoring/pod-state-metrics.yaml",
		"vmagent.enabled=true",
	)
	// helm -s errors when a template renders empty — that IS the default-off
	// invariant here.
	if err == nil && strings.Contains(out, "kind: Deployment") {
		t.Errorf("pod-state-metrics exporter must NOT render by default; got:\n%s", out)
	}
}

func TestVpaBoardMetrics_NetworkPolicy_DefaultAbsent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"vmagent.enabled=true",
		"networkPolicies.enabled=true",
	)
	if strings.Contains(out, "port: 10250") {
		t.Errorf("vmagent→kubelet 10250 egress must NOT render when kubeletScrape.enabled is false; got:\n%s", out)
	}
	if strings.Contains(out, "pod-state-metrics") {
		t.Errorf("pod-state-metrics policies must NOT render when podStateMetrics.enabled is false; got:\n%s", out)
	}
}

// --- kubelet scrape opt-in ---------------------------------------------------

func TestKubeletScrape_ConfigMap_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
		"vmagent.kubeletScrape.enabled=true",
	)
	if !strings.Contains(out, "job_name: kubelet-resources") {
		t.Fatalf("expected kubelet-resources scrape job; got:\n%s", out)
	}
	// /metrics/resource (small, metrics-server pipeline), NOT the ~10x-larger
	// /metrics/cadvisor whose id/image label cardinality would balloon
	// vmsingle's 1y retention.
	if !strings.Contains(out, "metrics_path: /metrics/resource") {
		t.Error("kubelet job must scrape /metrics/resource, not /metrics/cadvisor")
	}
	if !strings.Contains(out, "role: node") {
		t.Error("kubelet job must use the node SD role")
	}
	// The kubelet serves :10250 over TLS and authorizes via Webhook — the job
	// needs the SA bearer token or every scrape 401s.
	if !strings.Contains(out, "bearer_token_file: /var/run/secrets/kubernetes.io/serviceaccount/token") {
		t.Error("kubelet job must authenticate with the vmagent SA bearer token")
	}
	if !strings.Contains(out, "scheme: https") {
		t.Error("kubelet job must scrape over https (kubelet serves no plaintext metrics port)")
	}
	// The namespace keep is what stops whole-cluster product pods from
	// entering vmsingle. The literal value tracks .Release.Namespace (which
	// depends on the render context), so assert the rule shape: a keep on the
	// namespace label inside the kubelet job's metric_relabel_configs.
	block := out[strings.Index(out, "job_name: kubelet-resources"):]
	if !strings.Contains(block, "source_labels: [namespace]") {
		t.Errorf("expected a namespace keep filter in the kubelet job; got:\n%s", block)
	}
	if !strings.Contains(out, "container_memory_working_set_bytes") {
		t.Error("keep-filter must retain container_memory_working_set_bytes — the series the board exists for")
	}
}

// An emptied keepMetrics must omit the name filter (unbounded families, still
// namespace-bounded), NOT render an anchored `()` regex whose keep action
// silently drops every series — same guard as karpenterScrape.keepMetrics.
func TestKubeletScrape_ConfigMap_EmptyKeepMetricsOmitsKeep(t *testing.T) {
	t.Parallel()
	out := helmTemplateJSON(t, "templates/vmagent/configmap.yaml",
		map[string]string{"vmagent.kubeletScrape.keepMetrics": "[]"},
		"vmagent.enabled=true",
		"vmagent.kubeletScrape.enabled=true",
	)
	i := strings.Index(out, "job_name: kubelet-resources")
	if i < 0 {
		t.Fatalf("expected kubelet-resources job to still render with an empty keepMetrics; got:\n%s", out)
	}
	block := out[i:]
	if j := strings.Index(block, "job_name:"); j > 0 {
		if k := strings.Index(block[1:], "job_name:"); k > 0 {
			block = block[:k+1]
		}
	}
	if strings.Contains(block, `regex: "()"`) {
		t.Errorf("empty keepMetrics must NOT render a match-nothing keep regex; got:\n%s", block)
	}
	// The namespace keep must survive — it is the cardinality backstop.
	if !strings.Contains(block, `source_labels: [namespace]`) {
		t.Errorf("the namespace keep filter must remain even with an empty keepMetrics; got:\n%s", block)
	}
}

func TestKubeletScrape_RBAC_ClusterRoleRendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/rbac.yaml",
		"vmagent.enabled=true",
		"vmagent.kubeletScrape.enabled=true",
	)
	if !strings.Contains(out, "kubelet-scrape") {
		t.Fatalf("expected the kubelet-scrape ClusterRole/Binding; got:\n%s", out)
	}
	// node SD role needs nodes list/watch; the kubelet's Webhook authorizer
	// maps /metrics/* to a get on nodes/metrics. Nothing else — in particular
	// no secrets and no nodes/proxy (which would allow arbitrary kubelet API
	// calls including exec).
	if !strings.Contains(out, `resources: ["nodes"]`) {
		t.Error("kubelet-scrape ClusterRole must grant nodes get/list/watch for node SD")
	}
	if !strings.Contains(out, `resources: ["nodes/metrics"]`) {
		t.Error("kubelet-scrape ClusterRole must grant nodes/metrics get for the Webhook authorizer")
	}
	if strings.Contains(out, "nodes/proxy") {
		t.Error("kubelet-scrape ClusterRole must NOT grant nodes/proxy (kubelet exec/attach surface)")
	}
}

func TestKubeletScrape_NetworkPolicy_EgressRendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"vmagent.enabled=true",
		"vmagent.kubeletScrape.enabled=true",
		"networkPolicies.enabled=true",
	)
	// The kubelet is a host process — the allowance must be ipBlock based and
	// bounded to the kubelet port.
	if !strings.Contains(out, "port: 10250") {
		t.Fatalf("expected a vmagent egress allowance on the kubelet port 10250; got:\n%s", out)
	}
	if !strings.Contains(out, "cidr: 10.0.0.0/8") {
		t.Error("expected the default RFC1918 node CIDRs in the kubelet egress allowance")
	}
}

// --- pod-state exporter opt-in ------------------------------------------------

func TestPodStateMetrics_Exporter_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/monitoring/pod-state-metrics.yaml",
		"monitoring.podStateMetrics.enabled=true",
	)
	// Namespace scoping is the isolation guarantee — without it this KSM
	// could read every product workload on a shared cluster. The value tracks
	// .Release.Namespace, so assert the flag is present rather than a literal.
	if !strings.Contains(out, "--namespaces=") {
		t.Fatalf("exporter must be namespace-scoped via --namespaces; got:\n%s", out)
	}
	if !strings.Contains(out, "--resources=pods") {
		t.Error("exporter must be restricted to the pods resource")
	}
	// The allow-list bounds what enters vmsingle's 1y retention; the four
	// board/guardrail families must be present.
	for _, fam := range []string{
		"kube_pod_container_resource_requests",
		"kube_pod_container_resource_limits",
		"kube_pod_container_status_restarts_total",
		"kube_pod_container_status_last_terminated_reason",
	} {
		if !strings.Contains(out, fam) {
			t.Errorf("--metric-allowlist must retain %q", fam)
		}
	}
	// Deliberately NOT annotation-scraped: the default kubernetes-pods job
	// would clobber the subject namespace/pod labels (honor_labels=false).
	if strings.Contains(out, "prometheus.io/scrape") {
		t.Error("exporter pod must NOT carry prometheus.io/scrape — the dedicated honor_labels job is its only scraper")
	}
}

// An emptied metricAllowlist must omit the flag entirely (every pods-resource
// family, still namespace-scoped) — `--metric-allowlist=` with an empty value
// would make KSM emit nothing.
func TestPodStateMetrics_Exporter_EmptyAllowlistOmitsFlag(t *testing.T) {
	t.Parallel()
	out := helmTemplateJSON(t, "templates/monitoring/pod-state-metrics.yaml",
		map[string]string{"monitoring.podStateMetrics.metricAllowlist": "[]"},
		"monitoring.podStateMetrics.enabled=true",
	)
	if strings.Contains(out, "--metric-allowlist") {
		t.Errorf("empty metricAllowlist must omit the --metric-allowlist flag entirely; got:\n%s", out)
	}
}

func TestPodStateMetrics_ScrapeJob_HonorLabels(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
		"monitoring.podStateMetrics.enabled=true",
	)
	i := strings.Index(out, "job_name: pod-state-metrics")
	if i < 0 {
		t.Fatalf("expected the pod-state-metrics scrape job; got:\n%s", out)
	}
	block := out[i:]
	// honor_labels is the whole point of the dedicated job: KSM series label
	// the SUBJECT pod via their own namespace/pod labels, which the default
	// annotation job's target relabels would overwrite with the exporter's
	// identity — silently corrupting every board query.
	if !strings.Contains(block, "honor_labels: true") {
		t.Errorf("pod-state-metrics job must set honor_labels: true; got:\n%s", block)
	}
	if !strings.Contains(block, `regex: ".+:8080"`) {
		t.Errorf("pod-state-metrics job must keep only the :8080 metrics target; got:\n%s", block)
	}
}

func TestPodStateMetrics_NetworkPolicy_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"vmagent.enabled=true",
		"monitoring.podStateMetrics.enabled=true",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	)
	if !strings.Contains(out, "pod-state-metrics-ingress") {
		t.Fatalf("expected the pod-state-metrics ingress policy; got:\n%s", out)
	}
	// Egress to the API server is load-bearing: without it default-deny-egress
	// silently blanks the exporter (no pods watch → no series, no error).
	if !strings.Contains(out, "pod-state-metrics-egress") {
		t.Error("expected the pod-state-metrics egress policy (API server + kube-dns)")
	}
	// The vmagent in-namespace scrape allowance must gain 8080.
	if !strings.Contains(out, "port: 8080") {
		t.Error("expected the vmagent egress allowance on the exporter metrics port 8080")
	}
}

// --- VPA CustomResource + recommender scrape (ka-0000000525) ------------------
//
// The two VPA metric families that power the board's right-sizing RECOMMENDATION
// view — kube_customresource_vpa_* (reco-value exporter) and vpa_recommender_*
// (Fairwinds recommender) — ship ServiceMonitors, but the in-chart vmagent runs
// a STATIC -promscrape.config with no prometheus-operator to honor them, and
// prod has no observability Prometheus. So the chart adds dedicated static
// vmagent scrape jobs (mirroring pod-state-metrics) that are the only path these
// series reach vmsingle on prod. These tests pin those jobs + their netpol
// allowances AND the default-off invariant.

// vpaGuardrails scrape jobs must NOT render when the exporter/recommender knobs
// are off, even if vpaGuardrails.enabled is true (the exporter Deployments are
// separately gated, so a stray scrape job would target nothing).
func TestVpaRecoScrape_ConfigMap_DefaultAbsent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
		"agents.metrics.prometheusRule.vpaGuardrails.enabled=true",
	)
	if strings.Contains(out, "job_name: vpa-reco-exporter") {
		t.Errorf("vpa-reco-exporter job must NOT render when recommendationExporter.enabled is false; got:\n%s", out)
	}
	if strings.Contains(out, "job_name: vpa-recommender") {
		t.Errorf("vpa-recommender job must NOT render when recommenderServiceMonitor.enabled is false; got:\n%s", out)
	}
}

func TestVpaRecoExporter_ScrapeJob_HonorLabels(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
		"agents.metrics.prometheusRule.vpaGuardrails.enabled=true",
		"agents.metrics.prometheusRule.vpaGuardrails.recommendationExporter.enabled=true",
	)
	i := strings.Index(out, "job_name: vpa-reco-exporter")
	if i < 0 {
		t.Fatalf("expected the vpa-reco-exporter scrape job; got:\n%s", out)
	}
	block := out[i:]
	// honor_labels is load-bearing: the CRS series carry their own
	// namespace/verticalpodautoscaler/container labels identifying the SUBJECT
	// VPA, which the default annotation job's relabels would clobber.
	if !strings.Contains(block, "honor_labels: true") {
		t.Errorf("vpa-reco-exporter job must set honor_labels: true; got:\n%s", block)
	}
	if !strings.Contains(block, `regex: ".+:8080"`) {
		t.Errorf("vpa-reco-exporter job must keep only the :8080 metrics target; got:\n%s", block)
	}
	// The reco-exporter Deployment must also render under the same knob, or the
	// scrape job targets nothing.
	dep := helmTemplate(t, "templates/vpa-guardrails/recommendation-exporter.yaml",
		"vmagent.enabled=true",
		"agents.metrics.prometheusRule.vpaGuardrails.enabled=true",
		"agents.metrics.prometheusRule.vpaGuardrails.recommendationExporter.enabled=true",
	)
	if !strings.Contains(dep, "vpa-reco-exporter") {
		t.Error("expected the reco-value exporter Deployment to render under the same knob")
	}
}

func TestVpaRecommender_ScrapeJob_StampsNamespace(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
		"agents.metrics.prometheusRule.vpaGuardrails.enabled=true",
		"agents.metrics.prometheusRule.vpaGuardrails.recommenderServiceMonitor.enabled=true",
	)
	i := strings.Index(out, "job_name: vpa-recommender")
	if i < 0 {
		t.Fatalf("expected the vpa-recommender scrape job; got:\n%s", out)
	}
	block := out[i:]
	if !strings.Contains(block, `regex: ".+:8942"`) {
		t.Errorf("vpa-recommender job must keep only the :8942 metrics target; got:\n%s", block)
	}
	// namespace must be stamped from SD meta — the recommender series carry no
	// subject-namespace label, and the board queries filter on namespace=~$ns.
	if !strings.Contains(block, "__meta_kubernetes_namespace") {
		t.Errorf("vpa-recommender job must stamp namespace from SD meta; got:\n%s", block)
	}
	// job pinned so the board's up{job=~".*vpa-recommender.*"} panel + the
	// VPARecommenderDown alert match deterministically.
	if !strings.Contains(block, "replacement: vpa-recommender") {
		t.Errorf("vpa-recommender job must pin the job label; got:\n%s", block)
	}
}

func TestVpaRecoScrape_NetworkPolicy_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"vmagent.enabled=true",
		"networkPolicies.enabled=true",
		"agents.metrics.prometheusRule.vpaGuardrails.enabled=true",
		"agents.metrics.prometheusRule.vpaGuardrails.recommendationExporter.enabled=true",
		"agents.metrics.prometheusRule.vpaGuardrails.recommenderServiceMonitor.enabled=true",
	)
	if !strings.Contains(out, "vpa-reco-exporter-ingress") {
		t.Fatalf("expected the vpa-reco-exporter ingress policy; got:\n%s", out)
	}
	// Egress to the API server is load-bearing: the CRS pod list/watches the
	// VPA CRD + objects; without it the exporter emits nothing, silently.
	if !strings.Contains(out, "vpa-reco-exporter-egress") {
		t.Error("expected the vpa-reco-exporter egress policy (API server + kube-dns)")
	}
	// The recommender pods (installed by the vpa subchart) fall under
	// default-deny-ingress, so vmagent needs an explicit allow to :8942.
	if !strings.Contains(out, "vpa-recommender-ingress") {
		t.Error("expected the vpa-recommender ingress policy")
	}
	if !strings.Contains(out, "port: 8942") {
		t.Error("expected the vmagent egress allowance on the recommender metrics port 8942")
	}
}

// Default-off invariant for the netpols: no VPA-scrape policy or :8942 allowance
// when the knobs are off.
func TestVpaRecoScrape_NetworkPolicy_DefaultAbsent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"vmagent.enabled=true",
		"networkPolicies.enabled=true",
	)
	if strings.Contains(out, "vpa-reco-exporter") || strings.Contains(out, "vpa-recommender-ingress") {
		t.Errorf("VPA-scrape network policies must NOT render when the knobs are off; got:\n%s", out)
	}
	if strings.Contains(out, "port: 8942") {
		t.Errorf("vmagent→recommender 8942 egress must NOT render when recommenderServiceMonitor.enabled is false; got:\n%s", out)
	}
}

// ka-0000004184 (operator ask Matt 2026-07-24): the cAdvisor OOM-events scrape.
// A SECOND metrics_path (/metrics/cadvisor) on the SAME kubelet node-SD target
// as kubelet-resources, keep-filtered to container_oom_events_total — the cgroup
// oom_kill counter that catches workflow-CHILD OOMs the kubelet PID1
// reason=OOMKilled misses. Nested INSIDE the kubeletScrape.enabled guard so it
// shares that job's nodes/metrics RBAC + :10250 egress (no new grant). OFF by an
// inner cadvisorOOM toggle so the chart stays byte-identical where unwanted.
func TestCadvisorOOMScrape_DefaultAbsentEvenWithKubeletScrape(t *testing.T) {
	t.Parallel()
	// kubeletScrape ON but cadvisorOOM OFF (the vpa-board posture): the cadvisor
	// job must NOT render — the extra target is opt-in.
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
		"vmagent.kubeletScrape.enabled=true",
	)
	if strings.Contains(out, "job_name: kubelet-cadvisor-oom") {
		t.Errorf("kubelet-cadvisor-oom job must NOT render when cadvisorOOM.enabled is false; got:\n%s", out)
	}
	if strings.Contains(out, "/metrics/cadvisor") {
		t.Errorf("no /metrics/cadvisor scrape unless cadvisorOOM.enabled; got:\n%s", out)
	}
}

func TestCadvisorOOMScrape_RequiresKubeletScrape(t *testing.T) {
	t.Parallel()
	// cadvisorOOM ON but kubeletScrape OFF: the job nests inside the
	// kubeletScrape.enabled guard, so it renders NOTHING (and no RBAC/egress
	// leaks in without the parent scrape either).
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
		"vmagent.kubeletScrape.cadvisorOOM.enabled=true",
	)
	if strings.Contains(out, "job_name: kubelet-cadvisor-oom") {
		t.Errorf("kubelet-cadvisor-oom must not render without kubeletScrape.enabled; got:\n%s", out)
	}
}

func TestCadvisorOOMScrape_RenderedSharesKubeletTarget(t *testing.T) {
	t.Parallel()
	out := helmTemplateJSON(t, "templates/vmagent/configmap.yaml",
		map[string]string{"vmagent.kubeletScrape.cadvisorOOM.namespaces": `["gardenio","gardenio-e2e-.+"]`},
		"vmagent.enabled=true",
		"vmagent.kubeletScrape.enabled=true",
		"vmagent.kubeletScrape.cadvisorOOM.enabled=true",
	)
	if !strings.Contains(out, "job_name: kubelet-cadvisor-oom") {
		t.Fatalf("expected kubelet-cadvisor-oom scrape job; got:\n%s", out)
	}
	block := out[strings.Index(out, "job_name: kubelet-cadvisor-oom"):]
	// The DIRECT cgroup OOM path — /metrics/cadvisor, not /metrics/resource.
	if !strings.Contains(block, "metrics_path: /metrics/cadvisor") {
		t.Error("cadvisor OOM job must scrape /metrics/cadvisor")
	}
	// Same node-SD target + auth posture as kubelet-resources (so it shares the
	// nodes/metrics RBAC + :10250 egress the kubeletScrape gate already grants).
	if !strings.Contains(block, "role: node") {
		t.Error("cadvisor OOM job must use the node SD role (shared kubelet target)")
	}
	if !strings.Contains(block, "bearer_token_file: /var/run/secrets/kubernetes.io/serviceaccount/token") {
		t.Error("cadvisor OOM job must authenticate with the vmagent SA bearer token")
	}
	if !strings.Contains(block, "scheme: https") {
		t.Error("cadvisor OOM job must scrape over https")
	}
	// Pinned to the single OOM family + the agent namespaces — no id/image
	// cardinality from the ~10x-larger cadvisor endpoint.
	if !strings.Contains(block, "container_oom_events_total") {
		t.Error("cadvisor OOM job must keep-filter to container_oom_events_total")
	}
	if !strings.Contains(block, "(gardenio|gardenio-e2e-.+)") {
		t.Errorf("cadvisor OOM job must keep the agent namespaces from cadvisorOOM.namespaces; got:\n%s", block)
	}
	// It must NOT drag in the working-set family — this job is OOM-only.
	if strings.Contains(block, "container_memory_working_set_bytes") {
		t.Errorf("cadvisor OOM job must be OOM-family-only, not the working-set series; got:\n%s", block)
	}
}
