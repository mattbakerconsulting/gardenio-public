package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// Perf→gardenio metrics federation (epic ka-0000004560, task ka-0000005301).
// A second vmagent scrape job pulls the public perf VM /federate endpoint and
// remote-writes the copy into the in-chart vmsingle. The wiring is three new,
// default-empty knobs (vmagent.extraScrapeConfigs / extraVolumes /
// extraVolumeMounts) plus an externalSecrets.perfVmFederation ExternalSecret.
// These tests pin both the opt-in render AND the default-off invariant so the
// chart stays byte-identical for every env that does not set them.

// helmTemplateJSON is helmTemplate with --set-json support for the nested
// extraScrapeConfigs value (a list of maps that --set cannot express cleanly).
func helmTemplateJSON(t *testing.T, showOnly string, setJSON map[string]string, setFlags ...string) string {
	t.Helper()
	args := []string{"template", "test", chartDir(t)}
	if showOnly != "" {
		args = append(args, "--show-only", showOnly)
	}
	for k, v := range setJSON {
		args = append(args, "--set-json", k+"="+v)
	}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// perfScrapeJSON mirrors the live deploy-values/gardenio.yaml perf-federation
// job. source="perf" is stamped via relabel_configs (action: replace), NOT
// write_relabel_configs — the latter is only valid under remote_write and the
// vmagent configmap template hard-rejects it on a scrape entry (ka-0000007382).
// The match[] allowlist now includes the four RED spanmetrics families
// (span_calls_total + span_duration_milliseconds_{bucket,count,sum}) so the
// chart catalog's span-derived RED panels have a VM-side source (ka-0000008091);
// there is no blanket span_.+ drop, since federation is gated entirely by the
// match[] allowlist.
const perfScrapeJSON = `[{"job_name":"perf-federation","honor_labels":true,"scheme":"https","metrics_path":"/victoria-metrics/federate","basic_auth":{"username_file":"/etc/vmagent-perf/username","password_file":"/etc/vmagent-perf/password"},"params":{"match[]":["{__name__=~\"pg_.+|fastapi_.+|span_calls_total|span_duration_milliseconds_bucket|span_duration_milliseconds_count|span_duration_milliseconds_sum\"}"]},"static_configs":[{"targets":["perf.acme.example"]}],"relabel_configs":[{"target_label":"source","replacement":"perf","action":"replace"}]}]`

func TestVmagent_ExtraScrapeConfigs_DefaultAbsent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/configmap.yaml",
		"vmagent.enabled=true",
	)
	if strings.Contains(out, "perf-federation") {
		t.Errorf("perf-federation job must NOT render when extraScrapeConfigs is empty; got:\n%s", out)
	}
}

func TestVmagent_ExtraScrapeConfigs_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplateJSON(t, "templates/vmagent/configmap.yaml",
		map[string]string{"vmagent.extraScrapeConfigs": perfScrapeJSON},
		"vmagent.enabled=true",
	)
	if !strings.Contains(out, "job_name: perf-federation") {
		t.Fatalf("expected perf-federation job in scrape config; got:\n%s", out)
	}
	// The four RED spanmetrics families must be in the match[] allowlist so the
	// chart catalog's span-derived RED panels have a VM-side source
	// (ka-0000008091). Federation is gated entirely by match[]; only families
	// listed here ever federate.
	for _, fam := range []string{"span_calls_total", "span_duration_milliseconds_bucket"} {
		if !strings.Contains(out, fam) {
			t.Errorf("expected RED spanmetrics family %q in the perf-federation match[] allowlist", fam)
		}
	}
	// source="perf" namespaces every copied series so it never aliases gardenio_*.
	// It is stamped via relabel_configs (action: replace) — write_relabel_configs
	// would be rejected by the template (ka-0000007382).
	if !strings.Contains(out, "target_label: source") || !strings.Contains(out, "replacement: perf") {
		t.Error("expected relabel_configs stamping source=perf on federated series")
	}
	// The kubernetes-pods discovery job must still be present — extra jobs append,
	// they do not replace.
	if !strings.Contains(out, "job_name: kubernetes-pods") {
		t.Error("extraScrapeConfigs must append to, not replace, the default kubernetes-pods job")
	}
}

func TestVmagent_ExtraVolumes_DefaultAbsent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmagent/deployment.yaml",
		"vmagent.enabled=true",
	)
	if strings.Contains(out, "perf-vm-federation") {
		t.Errorf("perf federation volume must NOT render by default; got:\n%s", out)
	}
}

func TestVmagent_ExtraVolumes_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplateJSON(t, "templates/vmagent/deployment.yaml",
		map[string]string{
			"vmagent.extraVolumes":      `[{"name":"perf-vm-federation","secret":{"secretName":"gardenio-perf-vm-federation"}}]`,
			"vmagent.extraVolumeMounts": `[{"name":"perf-vm-federation","mountPath":"/etc/vmagent-perf","readOnly":true}]`,
		},
		"vmagent.enabled=true",
	)
	if !strings.Contains(out, "mountPath: /etc/vmagent-perf") {
		t.Error("expected perf federation secret mount at /etc/vmagent-perf")
	}
	if !strings.Contains(out, "secretName: gardenio-perf-vm-federation") {
		t.Error("expected perf federation secret volume")
	}
}

func TestExternalSecret_PerfVmFederation_DefaultAbsent(t *testing.T) {
	t.Parallel()
	// enabled externalSecrets but no remoteRef → the ExternalSecret must not
	// render. With no other remoteRefs set the whole file renders nothing, so
	// --show-only errors; allow that and assert the secret name is absent.
	out, _ := helmTemplateAllowFail(t, "templates/external-secrets.yaml",
		"vmagent.enabled=true",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=secretstore",
	)
	if strings.Contains(out, "gardenio-perf-vm-federation") {
		t.Errorf("perf federation ExternalSecret must NOT render without a remoteRef; got:\n%s", out)
	}
}

func TestExternalSecret_PerfVmFederation_Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/external-secrets.yaml",
		"vmagent.enabled=true",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=secretstore",
		"externalSecrets.perfVmFederation.remoteRef=gardenio/perf-vm-federation",
	)
	if !strings.Contains(out, "name: gardenio-perf-vm-federation") {
		t.Fatalf("expected perf federation ExternalSecret; got:\n%s", out)
	}
	// Both username and password keys must be projected from the one store entry.
	if !strings.Contains(out, "property: \"username\"") || !strings.Contains(out, "property: \"password\"") {
		t.Error("expected username + password properties from the perf-vm-federation store entry")
	}
}
