package tests

import (
	"strings"
	"testing"
)

// VPA crew right-sizing guardrails (epic ka-0000005189, task ka-0000004597).
// The chart renders, behind agents.metrics.prometheusRule.vpaGuardrails.enabled
// (default false):
//   - a PrometheusRule with 4 alerts (templates/vpa-guardrails/alerts.yaml),
//   - a recommender Service + ServiceMonitor (recommenderServiceMonitor.enabled), and
//   - a namespace-scoped KSM CRS recommendation-value exporter
//     (recommendationExporter.enabled).
//
// The Grafana dashboard is no longer a chart ConfigMap (removed in
// ka-0000002799); it rides in the grafana-bridge image and provisions at runtime.
// All four sub-toggles are independently gated and default off.

const vpaEnabled = "agents.metrics.prometheusRule.vpaGuardrails.enabled=true"

func TestVPAGuardrails_EnabledRendersAllFourAlerts(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vpa-guardrails/alerts.yaml", vpaEnabled)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled, got:\n%s", out)
	}
	for _, alert := range []string{
		"alert: CrewOOMKilled",
		"alert: CrewMemoryNearLimit",
		"alert: VPARecommenderDown",
		"alert: VPARecommenderNoRecommendation",
	} {
		if !strings.Contains(out, alert) {
			t.Errorf("missing %q, got:\n%s", alert, out)
		}
	}
	// The OOM gate must select crew pods by name prefix + the OOMKilled reason —
	// the cadvisor/KSM series carry no gardenio.io/* labels.
	for _, want := range []string{
		`pod=~"crew-.*"`,
		`reason="OOMKilled"`,
		"kube_pod_container_status_last_terminated_reason",
		"vpa_recommender_vpa_objects_count",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("alerts expr must contain %q, got:\n%s", want, out)
		}
	}
	// CrewOOMKilled must page immediately (for: 0m), not wait.
	if !strings.Contains(out, "for: 0m") {
		t.Errorf("CrewOOMKilled must be for: 0m, got:\n%s", out)
	}
}

func TestVPAGuardrails_NearLimitFractionAndSeverityConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vpa-guardrails/alerts.yaml",
		vpaEnabled,
		"agents.metrics.prometheusRule.vpaGuardrails.nearLimitFraction=0.85",
		"agents.metrics.prometheusRule.vpaGuardrails.severity=ticket",
	)
	if !strings.Contains(out, "> 0.85") {
		t.Errorf("nearLimitFraction override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, `severity: "ticket"`) {
		t.Errorf("severity override not applied, got:\n%s", out)
	}
}

func TestVPAGuardrails_NamespaceOverrideScopesExpr(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vpa-guardrails/alerts.yaml",
		vpaEnabled,
		"agents.metrics.prometheusRule.vpaGuardrails.namespace=gardenio-e2e",
	)
	if !strings.Contains(out, `namespace="gardenio-e2e"`) {
		t.Errorf("namespace override must scope the alert exprs, got:\n%s", out)
	}
}

func TestVPAGuardrails_RuleSelectorLabelsApplied(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vpa-guardrails/alerts.yaml",
		vpaEnabled,
		"agents.metrics.prometheusRule.vpaGuardrails.prometheusRuleSelectorLabels.release=kube-prometheus-stack",
	)
	if !strings.Contains(out, "release: kube-prometheus-stack") {
		t.Errorf("prometheusRuleSelectorLabels must land on PrometheusRule metadata, got:\n%s", out)
	}
}

func TestVPAGuardrails_RecommenderServiceMonitorGated(t *testing.T) {
	t.Parallel()
	// Off even when the parent toggle is on. The guarded template renders to
	// nothing, so helm exits non-zero with "could not find template" — that is
	// the gated-off success path.
	off, err := helmTemplateAllowFail(t, "templates/vpa-guardrails/recommender-servicemonitor.yaml", vpaEnabled)
	if err == nil && strings.Contains(off, "kind: ServiceMonitor") {
		t.Errorf("recommender ServiceMonitor must NOT render unless recommenderServiceMonitor.enabled=true, got:\n%s", off)
	}
	out := helmTemplate(t, "templates/vpa-guardrails/recommender-servicemonitor.yaml",
		vpaEnabled,
		"agents.metrics.prometheusRule.vpaGuardrails.recommenderServiceMonitor.enabled=true",
	)
	if !strings.Contains(out, "kind: ServiceMonitor") || !strings.Contains(out, "kind: Service") {
		t.Fatalf("recommender Service+ServiceMonitor must render when enabled, got:\n%s", out)
	}
	// The metrics endpoint must be the recommender's :8942 and pin the job label
	// so VPARecommenderDown's up{job=~".*vpa-recommender.*"} matches.
	for _, want := range []string{"8942", "replacement: vpa-recommender"} {
		if !strings.Contains(out, want) {
			t.Errorf("recommender SM must contain %q, got:\n%s", want, out)
		}
	}
}

func TestVPAGuardrails_RecommendationExporterGatedAndScoped(t *testing.T) {
	t.Parallel()
	off, err := helmTemplateAllowFail(t, "templates/vpa-guardrails/recommendation-exporter.yaml", vpaEnabled)
	if err == nil && strings.Contains(off, "kind: Deployment") {
		t.Errorf("reco exporter must NOT render unless recommendationExporter.enabled=true, got:\n%s", off)
	}
	out := helmTemplate(t, "templates/vpa-guardrails/recommendation-exporter.yaml",
		vpaEnabled,
		"agents.metrics.prometheusRule.vpaGuardrails.recommendationExporter.enabled=true",
	)
	for _, want := range []string{
		"kind: Deployment",
		"--custom-resource-state-only=true",
		"--namespaces=", // namespace-scoped: never reads product VPAs
		"vpa_status_recommendation_target",
		"kind: Role", // VPA list/watch is namespace-scoped via a Role, not ClusterRole
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reco exporter must contain %q, got:\n%s", want, out)
		}
	}
	// The only cluster-scoped grant is CRD discovery, never VPA data.
	if strings.Contains(out, `resources: ["verticalpodautoscalers"]`) &&
		!strings.Contains(out, "kind: Role\n") {
		t.Errorf("VPA list/watch must be namespace-scoped (Role), got:\n%s", out)
	}
}

// The vpa-guardrails dashboard ConfigMap was removed in ka-0000002799 (the chart
// no longer renders dashboard ConfigMaps — they double-stored ~200KB per revision
// in the release Secret and overflowed the K8s 1 MiB limit). The
// gardenio-vpa-right-sizing board now rides in the grafana-bridge image and is
// provisioned at runtime. Its JSON source of truth stays under dashboards/ and
// is validated by the shared dashboards JSON-validity test.
