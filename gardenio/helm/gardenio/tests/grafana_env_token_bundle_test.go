package tests

import (
	"strings"
	"testing"
)

// Env-side Grafana Editor token bundle (task ka-0000002337, design §1).
//
// Two surfaces:
//   - templates/grafana-api-key-external-secret.yaml — the ExternalSecret that
//     syncs gardenio/grafana-api-key/<env> from AWS SM into the k8s Secret
//     `grafana-api-key` (keys token-<env>).
//   - templates/controller/deployment.yaml — the controller env wiring that
//     forwards GRAFANA_ENV_API_KEY_SECRET / GRAFANA_ENV_LIST / per-env
//     GRAFANA_BASE_URL_<ENV> onto agent pods.
//
// The bundle is OFF by default (zero blast radius); it lights up only when the
// operator sets the secret name + enables the ExternalSecret after landing the
// SM entries and cutting the Grafana SA tokens.

const grafanaBundleExtSecret = "templates/grafana-api-key-external-secret.yaml"

func TestGrafanaEnvBundle_ExternalSecret_DefaultDisabled(t *testing.T) {
	t.Parallel()
	// With the bundle off, the template renders nothing; helm then errors with
	// "could not find template" under --show-only. Either way, no ExternalSecret.
	out, _ := helmTemplateAllowFail(t, grafanaBundleExtSecret,
		"agents.enabled=true",
	)
	if strings.Contains(out, "kind: ExternalSecret") {
		t.Errorf("grafana-api-key ExternalSecret must NOT render by default; got:\n%s", out)
	}
}

func TestGrafanaEnvBundle_ExternalSecret_RendersPerEnvKeys(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, grafanaBundleExtSecret,
		"agents.enabled=true",
		"agents.grafanaApiKeySecret=grafana-api-key",
		"agents.grafanaApiKeyExternalSecret.enabled=true",
		"agents.grafanaApiKeyExternalSecret.secretStoreName=aws-secretsmanager",
	)
	if !strings.Contains(out, "kind: ExternalSecret") {
		t.Fatalf("ExternalSecret should render when enabled; got:\n%s", out)
	}
	for _, env := range []string{"devel", "perf", "prod"} {
		if !strings.Contains(out, "secretKey: token-"+env) {
			t.Errorf("missing data secretKey token-%s; got:\n%s", env, out)
		}
		if !strings.Contains(out, "gardenio/grafana-api-key/"+env) {
			t.Errorf("missing SM remoteRef for env %s; got:\n%s", env, out)
		}
	}
}

func TestGrafanaEnvBundle_ExternalSecret_RespectsEnvironmentsOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, grafanaBundleExtSecret,
		"agents.enabled=true",
		"agents.grafanaApiKeySecret=grafana-api-key",
		"agents.grafanaApiKeyExternalSecret.enabled=true",
		"agents.grafanaApiKeyExternalSecret.secretStoreName=aws-secretsmanager",
		"agents.grafanaApiKeyExternalSecret.environments={devel}",
	)
	if !strings.Contains(out, "secretKey: token-devel") {
		t.Errorf("devel key should render; got:\n%s", out)
	}
	if strings.Contains(out, "secretKey: token-prod") {
		t.Errorf("prod key should NOT render when environments={devel}; got:\n%s", out)
	}
}

func TestGrafanaEnvBundle_Controller_DefaultNotWired(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
	)
	if strings.Contains(out, "GRAFANA_ENV_API_KEY_SECRET") {
		t.Errorf("controller must NOT wire the grafana env bundle by default; got:\n%s", out)
	}
}

func TestGrafanaEnvBundle_Controller_WiresSecretAndBaseURLs(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"agents.grafanaApiKeySecret=grafana-api-key",
		"agents.grafanaBaseURLs.devel=https://lens.devel.acme.net/grafana",
		"agents.grafanaBaseURLs.prod=https://lens.acme.net/grafana",
	)
	if !strings.Contains(out, "name: GRAFANA_ENV_API_KEY_SECRET") {
		t.Errorf("GRAFANA_ENV_API_KEY_SECRET should be wired; got:\n%s", out)
	}
	if !strings.Contains(out, `value: "devel,perf,prod"`) {
		t.Errorf("GRAFANA_ENV_LIST should carry the env list; got:\n%s", out)
	}
	if !strings.Contains(out, "name: GRAFANA_BASE_URL_DEVEL") ||
		!strings.Contains(out, "https://lens.devel.acme.net/grafana") {
		t.Errorf("GRAFANA_BASE_URL_DEVEL should be wired; got:\n%s", out)
	}
	if !strings.Contains(out, "name: GRAFANA_BASE_URL_PROD") {
		t.Errorf("GRAFANA_BASE_URL_PROD should be wired; got:\n%s", out)
	}
	// perf has no base URL set → its var must be omitted.
	if strings.Contains(out, "name: GRAFANA_BASE_URL_PERF") {
		t.Errorf("GRAFANA_BASE_URL_PERF should be omitted when unset; got:\n%s", out)
	}
}
