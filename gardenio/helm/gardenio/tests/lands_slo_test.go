package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Lands SLOs (Track B spec §5.8, step 10.8, gd-rqtbyqCMZJ): SLI recording
// rules and multi-window burn-rate alerts per objective, one shared define
// rendered as a PrometheusRule and as the vmalert ConfigMap key lands-slo.yaml;
// and the vmagent job that scrapes the off-cluster prober's metrics file.

const statusMetricsURL = "https://gardenio-status-111111111111.s3.us-east-1.amazonaws.com/metrics"

func TestLandsSLO_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/lands-slo-rules.yaml")
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when landsSLO.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestLandsSLO_EnabledRendersEveryObjective(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/lands-slo-rules.yaml",
		"agents.metrics.prometheusRule.landsSLO.enabled=true",
	)
	for _, want := range []string{
		"kind: PrometheusRule",
		"name: gardenio.lands_slo.sli",
		"name: gardenio.lands_slo.sli_long",
		"name: gardenio.lands_slo.alerts",
		// Every window the alerts read is recorded.
		"record: gardenio:lands_slo_errors:ratio_rate5m",
		"record: gardenio:lands_slo_errors:ratio_rate30m",
		"record: gardenio:lands_slo_errors:ratio_rate1h",
		"record: gardenio:lands_slo_errors:ratio_rate2h",
		"record: gardenio:lands_slo_errors:ratio_rate6h",
		"record: gardenio:lands_slo_errors:ratio_rate1d",
		"record: gardenio:lands_slo_errors:ratio_rate3d",
		"record: gardenio:lands_slo_errors:ratio_rate30d",
		"record: gardenio:lands_slo:objective",
		// The three SLI kinds.
		`gardenio_uptime_probe_failures_total{target="lands"}`,
		`gardenio_uptime_probe_total{target="garden"}`,
		`gardenio_uptime_probe_total{target="commons_api"}`,
		`gardenio_commons_event_lag_seconds_bucket{le=~"5|5\\.0"}`,
		`gardenio_user_request_latency_seconds_count{endpoint=~"(GET|POST) /garden/c(/.*)?",status_code=~"5.."}`,
		// The alert pair, with the workbook's burn rates.
		"alert: LandsSLOFastBurn",
		"alert: LandsSLOSlowBurn",
		`gardenio:lands_slo_errors:ratio_rate1h{slo="lands_availability"} > (14.4 * (1 - 0.995))`,
		`gardenio:lands_slo_errors:ratio_rate6h{slo="lands_availability"} > (6 * (1 - 0.995))`,
		`gardenio:lands_slo_errors:ratio_rate1d{slo="lands_availability"} > (3 * (1 - 0.995))`,
		`gardenio:lands_slo_errors:ratio_rate3d{slo="lands_availability"} > (1 * (1 - 0.995))`,
		`severity: "page"`,
		`severity: "ticket"`,
		"subsystem: lands-slo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing, got:\n%s", want, out)
		}
	}
	for _, slo := range []string{
		"lands_availability", "garden_availability", "commons_api_availability",
		"garden_requests", "commons_api_requests", "commons_event_latency",
		"tenant_spawn_to_working", "steward_response",
	} {
		if !strings.Contains(out, "slo: "+slo) {
			t.Errorf("objective %s must render by default", slo)
		}
	}
	// The stale-prober alert needs the scrape that feeds it.
	if strings.Contains(out, "LandsUptimeProbeStale") {
		t.Errorf("LandsUptimeProbeStale must not render without vmagent.uptimeProbeScrape")
	}
}

func TestLandsSLO_Overrides(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/lands-slo-rules.yaml",
		"agents.metrics.prometheusRule.landsSLO.enabled=true",
		"agents.metrics.prometheusRule.landsSLO.objectives.lands_availability.target=0.999",
		"agents.metrics.prometheusRule.landsSLO.objectives.garden_requests.enabled=false",
		"agents.metrics.prometheusRule.landsSLO.objectives.steward_response.enabled=true",
		"agents.metrics.prometheusRule.landsSLO.pageSeverity=critical",
		"agents.metrics.prometheusRule.landsSLO.runbookURL=https://docs.example/slo",
		"agents.metrics.prometheusRule.landsSLO.prometheusRuleSelectorLabels.release=kube-prometheus-stack",
		"vmagent.uptimeProbeScrape.enabled=true",
		"vmagent.uptimeProbeScrape.url="+statusMetricsURL,
	)
	for _, want := range []string{
		`gardenio:lands_slo_errors:ratio_rate1h{slo="lands_availability"} > (14.4 * (1 - 0.999))`,
		`gardenio_lands_spawn_to_working_seconds_bucket{le=~"300|300\\.0"}`,
		`gardenio_lands_steward_response_seconds_bucket{le=~"900|900\\.0"}`,
		`severity: "critical"`,
		`runbook_url: "https://docs.example/slo#lands_availability"`,
		"release: kube-prometheus-stack",
		"alert: LandsUptimeProbeStale",
		"(time() - max(gardenio_uptime_probe_last_run_timestamp_seconds)) > 600",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "slo: garden_requests") {
		t.Errorf("a disabled objective must not render")
	}
}

func TestLandsSLO_RejectsBadObjectives(t *testing.T) {
	t.Parallel()
	base := "agents.metrics.prometheusRule.landsSLO."
	for name, flags := range map[string][]string{
		"target 1":        {base + "objectives.lands_availability.target=1"},
		"target 0":        {base + "objectives.lands_availability.target=0"},
		"unknown kind":    {base + "objectives.lands_availability.kind=magic"},
		"probe no target": {base + "objectives.lands_availability.probeTarget="},
		"latency no le":   {base + "objectives.commons_event_latency.thresholdSeconds="},
		"requests no re":  {base + "objectives.garden_requests.endpoints="},
		"key not snake":   {base + "objectives.BadKey.enabled=true", base + "objectives.BadKey.kind=probe", base + "objectives.BadKey.probeTarget=x", base + "objectives.BadKey.target=0.9"},
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string{base + "enabled=true"}, flags...)
			out, err := helmTemplateAllowFail(t, "templates/lands-slo-rules.yaml", args...)
			if err == nil {
				t.Fatalf("render must fail, got:\n%s", out)
			}
			if !strings.Contains(out, "landsSLO.objectives") {
				t.Errorf("failure must name landsSLO.objectives, got:\n%s", out)
			}
		})
	}
}

func TestLandsSLO_VmalertConfigMapKey(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		"vmalert.enabled=true",
		"alertmanager.enabled=true",
		"agents.metrics.prometheusRule.landsSLO.enabled=true",
	)
	if !strings.Contains(out, "lands-slo.yaml: |") {
		t.Fatalf("vmalert ConfigMap must carry the lands-slo key when enabled + notifier, got:\n%s", out)
	}
	for _, want := range []string{"alert: LandsSLOFastBurn", "record: gardenio:lands_slo_errors:ratio_rate30d"} {
		if !strings.Contains(out, want) {
			t.Errorf("shared rules must render on the ConfigMap path too: %q missing", want)
		}
	}

	out = helmTemplate(t, "templates/vmalert/configmap.yaml",
		"vmalert.enabled=true",
		"alertmanager.enabled=false",
		"vmalert.notifierURL=",
		"vmalert.notifierBlackhole=false",
		"agents.metrics.prometheusRule.landsSLO.enabled=true",
	)
	if strings.Contains(out, "lands-slo.yaml") {
		t.Errorf("lands-slo key must NOT render without a notifier (vmalert would crashloop), got:\n%s", out)
	}
}

func TestLandsSLO_VmagentScrapesStatusBucket(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
		"vmagent.uptimeProbeScrape.enabled=true",
		"vmagent.uptimeProbeScrape.url="+statusMetricsURL,
	)
	for _, want := range []string{
		"job_name: uptime-probe",
		"scheme: https",
		`metrics_path: "/metrics"`,
		"honor_labels: true",
		`- targets: ["gardenio-status-111111111111.s3.us-east-1.amazonaws.com"]`,
		"component: uptime-probe",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing, got:\n%s", want, out)
		}
	}

	off := helmTemplate(t, "templates/vmagent/configmap.yaml", "vmagent.enabled=true")
	if strings.Contains(off, "uptime-probe") {
		t.Errorf("the uptime-probe job must not render by default")
	}

	bad, err := helmTemplateAllowFail(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
		"vmagent.uptimeProbeScrape.enabled=true",
		"vmagent.uptimeProbeScrape.url=http://insecure.example/metrics",
	)
	if err == nil || !strings.Contains(bad, "uptimeProbeScrape.url") {
		t.Errorf("a non-https url must fail the render, got err=%v:\n%s", err, bad)
	}
}

// With network policies on, vmagent may reach the bucket on 443 but never a
// private range through that rule.
func TestLandsSLO_VmagentEgressToStatusBucket(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"vmagent.enabled=true",
		"vmagent.uptimeProbeScrape.enabled=true",
		"vmagent.uptimeProbeScrape.url="+statusMetricsURL,
	)
	doc := renderedDocNamed(out, "test-vmagent-egress")
	if doc == "" {
		t.Fatalf("vmagent-egress NetworkPolicy missing, got:\n%s", out)
	}
	for _, want := range []string{"cidr: 0.0.0.0/0", "- 10.0.0.0/8", "- 172.16.0.0/12", "- 192.168.0.0/16", "- 169.254.0.0/16", "port: 443"} {
		if !strings.Contains(doc, want) {
			t.Errorf("%q missing from vmagent-egress:\n%s", want, doc)
		}
	}
	off := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true", "vmagent.enabled=true")
	if strings.Contains(renderedDocNamed(off, "test-vmagent-egress"), "0.0.0.0/0") {
		t.Errorf("vmagent must not get internet egress unless uptimeProbeScrape is on")
	}
}

func renderedDocNamed(out, name string) string {
	for _, doc := range strings.Split(out, "\n---") {
		if strings.Contains(doc, "name: "+name+"\n") {
			return doc
		}
	}
	return ""
}

// TestLandsSLO_Fire feeds promtool synthetic series (lands_slo_rules_test.yaml):
// each burn-rate alert fires on its signal and stays quiet for a healthy
// surface. Skips when helm/yq/promtool are not on PATH.
func TestLandsSLO_Fire(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"helm", "yq", "promtool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping promtool firing proof", bin)
		}
	}
	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "lands_slo_rules.yaml")
	render := exec.Command("bash", "-c",
		"helm template test "+chartDir(t)+" --show-only templates/lands-slo-rules.yaml "+
			"--set agents.metrics.prometheusRule.landsSLO.enabled=true "+
			"--set vmagent.uptimeProbeScrape.enabled=true "+
			"--set vmagent.uptimeProbeScrape.url="+statusMetricsURL+" | yq eval '.spec' -")
	out, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("render rules failed: %v\n%s", err, out)
	}
	if err := os.WriteFile(rulesPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := exec.Command("promtool", "check", "rules", rulesPath).CombinedOutput(); err != nil {
		t.Fatalf("promtool check rules failed: %v\n%s", err, res)
	}
	tmpl, err := os.ReadFile(filepath.Join(chartDir(t), "tests", "lands_slo_rules_test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	testPath := filepath.Join(tmp, "lands_slo_test.yaml")
	if err := os.WriteFile(testPath, []byte(strings.ReplaceAll(string(tmpl), "__RULES_FILE__", rulesPath)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := exec.Command("promtool", "test", "rules", testPath).CombinedOutput()
	if err != nil {
		t.Fatalf("promtool firing proof failed: %v\n%s", err, res)
	}
}
