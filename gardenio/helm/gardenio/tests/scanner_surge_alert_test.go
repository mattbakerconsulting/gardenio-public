package tests

import (
	"strings"
	"testing"
)

// Off-domain scanner source-IP SURGE step-change ticket — the OPTIONAL companion
// to the containment-breach alert (task ka-0000007505, epic ka-0000000572,
// scanner-response block+alert policy).
//
// The public webhooks edge sees a steady ~32-IP/day (max ~40, empirically
// measured over 7d — ka-0000005585) background of internet scanner noise —
// unauthenticated probes bouncing off the 404 containment wall.
// That background radiation MUST NEVER page anyone. A SINGLE `type: vlogs` rule
// TICKETS — never pages — when the count of DISTINCT off-apex source IPs in the
// window crosses a high multiple of that baseline: a coordinated surge is a
// TREND signal, not an incident (the breach itself is caught by
// EdgeContainmentBreach).
//
// The rule lives in templates/vmalert-logs/configmap.yaml (shared define
// gardenio.scannerSurge.rules) and rides the SAME dedicated vmalert-logs instance
// + notifier gate as the eks-api / edge-containment LogsQL detections. Off by
// default; the chart is byte-identical until an overlay opts in.

func TestScannerSurge_DefaultDisabled_NoRuleKey(t *testing.T) {
	t.Parallel()
	// scannerSurge.enabled=false (the default): even with vmalert-logs on and a
	// notifier wired, the surge rule key must NOT render.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
	)
	if strings.Contains(out, "scanner-surge-logsql.yaml") ||
		strings.Contains(out, "ScannerSourceIPSurge") {
		t.Errorf("scanner-surge rule must NOT render when scannerSurge.enabled=false, got:\n%s", out)
	}
	// With no LogsQL keys active at all, the placeholder must still be present so
	// the Deployment's rules mount always exists.
	if !strings.Contains(out, "placeholder.yaml: |") {
		t.Errorf("empty-but-valid placeholder must render when no LogsQL rules are active, got:\n%s", out)
	}
}

func TestScannerSurge_EnabledRendersSurgeRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"scannerSurge.enabled=true",
	)
	if !strings.Contains(out, "scanner-surge-logsql.yaml: |") {
		t.Fatalf("vmalert-logs ConfigMap must carry the scanner-surge rule key when enabled + notifier, got:\n%s", out)
	}
	if !strings.Contains(out, "alert: ScannerSourceIPSurge") {
		t.Errorf("expected the ScannerSourceIPSurge alert, got:\n%s", out)
	}
	// The GROUP must declare type: vlogs so vmalert queries the VictoriaLogs
	// stats API, not vmsingle's PromQL API. type is a group-level field ONLY —
	// vmalert (v1.108.1) rejects a per-rule `type` with "unknown fields in
	// rule: type" and crashloops, which wedges the prod helm --wait (gd-zxhZS96UHl).
	if strings.Count(out, "\n        type: vlogs\n") != 1 { // GROUP field only, never per-rule
		t.Errorf("exactly the group must carry `type: vlogs` (no per-rule type), got:\n%s", out)
	}
	// The expr must be the prototyped surge rule VERBATIM: negated apex Host
	// predicate, the per-ClientHost count, the distinct-source count, and the
	// >threshold filter. Any drift here silently changes what tickets.
	for _, want := range []string{
		`_time:1d -RequestHost:~"gardenio.example.com"`,
		"| stats by (ClientHost) count() as n",
		"| stats count() as distinct_src",
		"| filter distinct_src:>150",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("surge expr must contain %q, got:\n%s", want, out)
		}
	}
}

func TestScannerSurge_TicketsNeverPages(t *testing.T) {
	t.Parallel()
	// The brief is explicit: the day-to-day background noise must NEVER page. This is
	// trend visibility with a «1/week page-rate target, so it TICKETS.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"scannerSurge.enabled=true",
	)
	idx := strings.Index(out, "alert: ScannerSourceIPSurge")
	if idx < 0 {
		t.Fatalf("ScannerSourceIPSurge alert missing, got:\n%s", out)
	}
	surge := out[idx:]
	if !strings.Contains(surge, `severity: "ticket"`) {
		t.Errorf("ScannerSourceIPSurge must default to TICKET severity (never page), got:\n%s", surge)
	}
	if strings.Contains(surge, `severity: "page"`) {
		t.Errorf("ScannerSourceIPSurge must NOT page — background scanner noise must never page, got:\n%s", surge)
	}
	if !strings.Contains(surge, "detection: scanner-surge") ||
		!strings.Contains(surge, "component: edge") {
		t.Errorf("ScannerSourceIPSurge must carry the edge/scanner-surge routing labels, got:\n%s", surge)
	}
}

func TestScannerSurge_FieldsApexAndThresholdConfigurable(t *testing.T) {
	t.Parallel()
	// The edge log-field contract + the apex constant + the surge threshold are
	// values, aligned with whatever the transport publishes and re-baselineable
	// WITHOUT editing LogsQL.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"scannerSurge.enabled=true",
		"scannerSurge.fields.requestHost=req_host",
		"scannerSurge.fields.clientHost=client_ip",
		"scannerSurge.apexHost=example.test",
		"scannerSurge.thresholds.surge=1000",
		"scannerSurge.window=6h",
	)
	if !strings.Contains(out, `_time:6h -req_host:~"example.test"`) {
		t.Errorf("field/apex/window overrides must flow into the expr predicate, got:\n%s", out)
	}
	if !strings.Contains(out, "| stats by (client_ip) count() as n") {
		t.Errorf("clientHost override must flow into the stats grouping, got:\n%s", out)
	}
	if !strings.Contains(out, "| filter distinct_src:>1000") {
		t.Errorf("surge threshold override not applied, got:\n%s", out)
	}
}

func TestScannerSurge_RuleKeyOmittedWithoutNotifier(t *testing.T) {
	t.Parallel()
	// Alerting rules with no notifier crashloop vmalert; the surge rule key MUST
	// be gated off (placeholder only) when enabled but no notifier is wired.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=false",
		"vmalertLogs.notifierURL=",
		"vmalertLogs.notifierBlackhole=false",
		"scannerSurge.enabled=true",
	)
	if strings.Contains(out, "scanner-surge-logsql.yaml") {
		t.Errorf("surge rule key must be omitted when no notifier is available, got:\n%s", out)
	}
	if !strings.Contains(out, "placeholder.yaml: |") {
		t.Errorf("placeholder must render when enabled but no notifier, got:\n%s", out)
	}
}

func TestScannerSurge_CoexistsWithOtherDetections(t *testing.T) {
	t.Parallel()
	// Both LogsQL rule sets ride the same ConfigMap + notifier gate; enabling
	// them together must render every key independently and drop the placeholder.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"scannerSurge.enabled=true",
		"edgeContainment.enabled=true",
	)
	for _, key := range []string{
		"scanner-surge-logsql.yaml: |",
		"edge-containment-logsql.yaml: |",
	} {
		if !strings.Contains(out, key) {
			t.Errorf("both LogsQL rule keys must render when both are enabled; missing %q, got:\n%s", key, out)
		}
	}
	if strings.Contains(out, "placeholder.yaml: |") {
		t.Errorf("placeholder must be omitted once a real LogsQL rule key renders, got:\n%s", out)
	}
}
