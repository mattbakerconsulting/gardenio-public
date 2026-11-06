package tests

import (
	"strings"
	"testing"
)

// R6 legacy-pre-attribution sentinel cut-over alert (ka-0000006798, epic
// ka-0000006809 surface 6). The kbeads-operated-by PrometheusRule carries the
// LegacySentinelWriteAfterCutover alert alongside the three rollout alerts. It
// pages when ANY write binds operated_by=system:legacy-pre-attribution on or
// after the declared cut-over date, gated by a unix-timestamp threshold so it
// stays silent before the cut-over.

func TestOperatedByAlerts_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/beads/operated-by-alerts.yaml")
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when operatedBy.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(err.Error(), "could not find template") &&
		!strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestOperatedByAlerts_EnabledRendersAllAlerts(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/operated-by-alerts.yaml",
		"agents.metrics.prometheusRule.operatedBy.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	for _, want := range []string{
		"alert: OperatedByResolverErrors",
		"alert: OperatedByEmptyWriteRate",
		"alert: OperatedByAmbiguousAgent",
		"alert: LegacySentinelWriteAfterCutover",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in rendered rule, got:\n%s", want, out)
		}
	}
}

// TestOperatedByAlerts_LegacySentinelGatedByCutoverTime pins that the R6 alert
// (1) reads the correct metric, (2) is gated by the cut-over unix timestamp so
// it cannot page before the date, and (3) pages (severity: page) unconditionally
// — it must not inherit the configurable rollout severity because the counter
// must sit at zero after cut-over regardless of environment.
func TestOperatedByAlerts_LegacySentinelGatedByCutoverTime(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/operated-by-alerts.yaml",
		"agents.metrics.prometheusRule.operatedBy.enabled=true",
	)

	if !strings.Contains(out, "gardenio_beads_operated_by_legacy_sentinel_total") {
		t.Errorf("R6 alert must read gardenio_beads_operated_by_legacy_sentinel_total, got:\n%s", out)
	}
	// The default cut-over unix timestamp (2026-09-01T00:00:00Z) must be
	// rendered into the time() gate so the alert stays silent before it.
	if !strings.Contains(out, "time() >= 1788220800") {
		t.Errorf("R6 alert must gate on the default cut-over unix timestamp 1788220800, got:\n%s", out)
	}

	// Isolate the LegacySentinelWriteAfterCutover block and assert it pages.
	idx := strings.Index(out, "alert: LegacySentinelWriteAfterCutover")
	if idx < 0 {
		t.Fatalf("LegacySentinelWriteAfterCutover alert missing, got:\n%s", out)
	}
	block := out[idx:]
	if !strings.Contains(block, "severity: page") {
		t.Errorf("R6 alert must be severity: page (must not inherit rollout severity), got:\n%s", block)
	}
}

// TestOperatedByAlerts_CutoverTimestampOverridable pins that an env overlay can
// move the cut-over gate (e.g. staging forces it earlier) via the value.
func TestOperatedByAlerts_CutoverTimestampOverridable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/operated-by-alerts.yaml",
		"agents.metrics.prometheusRule.operatedBy.enabled=true",
		"agents.metrics.prometheusRule.operatedBy.legacySentinelCutoverUnix=1234567890",
	)
	if !strings.Contains(out, "time() >= 1234567890") {
		t.Errorf("legacySentinelCutoverUnix override must reach the alert expr, got:\n%s", out)
	}
}
