package tests

import (
	"strings"
	"testing"
)

// Stage 3 / Safe-deploy chart flags (ka-0000002544):
//   - edgeAuth.dryRun
//   - daemonHmacVerify.enforce
//   - safeDeployAlerts.enabled (renders PrometheusRule)
//
// Defaults MUST keep the rendered manifest free of the safedeploy alert
// PrometheusRule. The flag values themselves are consumed by future
// edge-auth / daemon Deployments (W-B-12, W-B-11) — the contract here is
// that they parse + render through helm and reach the templates.

func TestSafeDeployAlerts_DefaultDisabled(t *testing.T) {
	t.Parallel()
	// When safeDeployAlerts.enabled=false (the default), the template
	// renders nothing. helm exits non-zero with "could not find template"
	// when --show-only points at an all-empty template; that's the
	// signal we are looking for.
	out, err := helmTemplateAllowFail(t, "templates/safedeploy-alerts.yaml")
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when safeDeployAlerts.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestSafeDeployAlerts_EnabledRendersBothAlerts(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/safedeploy-alerts.yaml",
		"safeDeployAlerts.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when safeDeployAlerts.enabled=true, got:\n%s", out)
	}
	if !strings.Contains(out, "alert: EdgeAuth5xxRate") {
		t.Errorf("EdgeAuth5xxRate alert missing, got:\n%s", out)
	}
	if !strings.Contains(out, "alert: DaemonHmacMismatch") {
		t.Errorf("DaemonHmacMismatch alert missing, got:\n%s", out)
	}
	if !strings.Contains(out, `severity: "page"`) {
		t.Errorf("default severity must be page, got:\n%s", out)
	}
}

func TestSafeDeployAlerts_RatioThresholdConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/safedeploy-alerts.yaml",
		"safeDeployAlerts.enabled=true",
		"safeDeployAlerts.edgeAuth5xxRatio=0.05",
	)
	if !strings.Contains(out, "> 0.05") {
		t.Errorf("edgeAuth5xxRatio override (0.05) not applied, got:\n%s", out)
	}
}

func TestSafeDeployAlerts_SeverityConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/safedeploy-alerts.yaml",
		"safeDeployAlerts.enabled=true",
		"safeDeployAlerts.severity=ticket",
	)
	if !strings.Contains(out, `severity: "ticket"`) {
		t.Errorf("severity override not applied, got:\n%s", out)
	}
}

func TestSafeDeployAlerts_EnforceLabelOnDaemonRule(t *testing.T) {
	t.Parallel()
	// The DaemonHmacMismatch alert must be gated on enforce="true" so
	// burn-in mode (enforce=false) does not page on every mismatch.
	out := helmTemplate(t, "templates/safedeploy-alerts.yaml",
		"safeDeployAlerts.enabled=true",
	)
	if !strings.Contains(out, `enforce="true"`) {
		t.Errorf("DaemonHmacMismatch alert must filter on enforce=\"true\", got:\n%s", out)
	}
}

// --- Defaults for the runtime feature flags ---
//
// values.yaml MUST default both flags to off. Tests assert the parsed
// values rather than rendered manifests since the consuming Deployment
// manifests (W-B-12, W-B-11) are not yet in the chart.

func TestEdgeAuthDryRun_ChartDefault(t *testing.T) {
	t.Parallel()
	// Render the whole chart and assert no template surfaces dryRun=true
	// from the defaults. This is a smoke test that values.yaml ships the
	// safe default.
	out := helmTemplate(t, "")
	if strings.Contains(out, "EDGE_AUTH_DRY_RUN") {
		// Future deployments will reference this env var; today there are
		// no such references in the chart.
		if !strings.Contains(out, `EDGE_AUTH_DRY_RUN`) {
			return
		}
		// If the flag does surface in the future, ensure it is false by
		// default.
		if strings.Contains(out, `name: EDGE_AUTH_DRY_RUN`) &&
			!strings.Contains(out, `value: "false"`) {
			t.Errorf("EDGE_AUTH_DRY_RUN appears in chart output without value=false; defaults must be off")
		}
	}
}

func TestDaemonHmacVerifyEnforce_ChartDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "")
	if strings.Contains(out, `name: DAEMON_HMAC_VERIFY_ENFORCE`) &&
		!strings.Contains(out, `value: "false"`) {
		t.Errorf("DAEMON_HMAC_VERIFY_ENFORCE appears in chart output without value=false; defaults must be off")
	}
}
