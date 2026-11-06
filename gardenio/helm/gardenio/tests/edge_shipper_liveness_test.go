package tests

import (
	"strings"
	"testing"
)

// Edge log-shipper DEAD-MAN'S-SWITCH — EdgeLogShipperStalled (task ka-0000006616,
// epic ka-0000000472, sibling bug ka-0000002843).
//
// The edge log shipper (traefik-log-shipper) can be Running 1/1 and still
// deliver ZERO rows (ka-0000002843). Process liveness is NOT shipper liveness, so
// the switch watches ROWS DELIVERED — a count that increments on success (advice
// ka-0000002586) — never a static liveness field. One `type: vlogs` rule per
// enabled+deployed shipper, in the same vmalert-logs ConfigMap + notifier gate
// as the edgeContainment / scannerSurge LogsQL rules. Off by default;
// byte-identical chart until an overlay opts in.

func TestEdgeShipperLiveness_DefaultDisabled_NoRuleKey(t *testing.T) {
	t.Parallel()
	// edgeShipperLiveness.enabled=false (the default): even with vmalert-logs on,
	// a notifier wired, and the shipper deployed, the rule key must NOT render.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"traefikLogShipper.enabled=true",
	)
	if strings.Contains(out, "edge-shipper-liveness-logsql.yaml") ||
		strings.Contains(out, "EdgeLogShipperStalled") {
		t.Errorf("edge-shipper-liveness rule must NOT render when edgeShipperLiveness.enabled=false, got:\n%s", out)
	}
	if !strings.Contains(out, "placeholder.yaml: |") {
		t.Errorf("empty-but-valid placeholder must render when no LogsQL rules are active, got:\n%s", out)
	}
}

func TestEdgeShipperLiveness_EnabledRendersRowsDeliveredRule(t *testing.T) {
	t.Parallel()
	// Enabled + notifier + shipper deployed: one EdgeLogShipperStalled rule per
	// shipper, each a rows-delivered LogsQL count over the staleness window.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeShipperLiveness.enabled=true",
		"traefikLogShipper.enabled=true",
	)
	if !strings.Contains(out, "edge-shipper-liveness-logsql.yaml: |") {
		t.Fatalf("vmalert-logs ConfigMap must carry the edge-shipper-liveness rule key when enabled + notifier, got:\n%s", out)
	}
	// The GROUP must declare type: vlogs so vmalert queries the VictoriaLogs
	// stats API, not vmsingle's PromQL API. type is a group-level field ONLY —
	// vmalert (v1.108.1) rejects a per-rule `type` with "unknown fields in
	// rule: type" and crashloops, which wedges the prod helm --wait (gd-zxhZS96UHl).
	// One group carries all shipper rules, so exactly one `type: vlogs`.
	if strings.Count(out, "\n        type: vlogs\n") != 1 { // GROUP field only, never per-rule
		t.Errorf("exactly the group must carry `type: vlogs` (no per-rule type), got:\n%s", out)
	}
	// THE SIGNAL: rows delivered, counted per shipper with its own selector,
	// firing at/below the floor. A bare `stats count()` (NOT `stats by`) is
	// load-bearing: it emits a 0-row even when the selector field is absent, so
	// the floor fires on a fully-gone pipeline. Any drift here silently changes
	// what pages.
	for _, want := range []string{
		"_time:15m entryPointName:*", // traefik selector, traefik window
		"| stats count() as rows",
		"| filter rows:<=0",
		`shipper: "traefik"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("edge-shipper-liveness expr must contain %q, got:\n%s", want, out)
		}
	}
	// It must NOT use the `| stats by` LogsQL operator — that would be silent on
	// empty input (the no-op trap) and could never fire on a fully-gone pipeline.
	// (Match the piped operator, not the phrase in the explanatory comment.)
	if strings.Contains(out, "| stats by") {
		t.Errorf("edge-shipper-liveness must use a bare `stats count()`, never `| stats by` (silent-on-empty trap), got:\n%s", out)
	}
	// The placeholder must NOT appear once a real rule key is active.
	if strings.Contains(out, "placeholder.yaml: |") {
		t.Errorf("placeholder must be omitted once the edge-shipper-liveness key renders, got:\n%s", out)
	}
}

func TestEdgeShipperLiveness_PerShipperStalenessWindow(t *testing.T) {
	t.Parallel()
	// A safe staleness window is a property of THAT shipper's own traffic shape,
	// so it must be settable per shipper and must override the top-level default
	// (bug ka-0000006504).
	//
	// Here the top-level default is set to a value the shipper does not use,
	// proving the window comes from the per-shipper key rather than the default.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeShipperLiveness.enabled=true",
		"edgeShipperLiveness.stalenessWindow=99m",
		"traefikLogShipper.enabled=true",
	)
	if !strings.Contains(out, "_time:15m entryPointName:*") {
		t.Errorf("traefik must keep its per-shipper 15m window (fast detection), got:\n%s", out)
	}
	if strings.Contains(out, "_time:99m") {
		t.Errorf("top-level stalenessWindow must not override an explicit per-shipper window, got:\n%s", out)
	}

	// With the per-shipper key cleared, the shipper falls back to the top-level
	// default — the override is a genuine default-with-override, not a hardcode.
	out = helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeShipperLiveness.enabled=true",
		"edgeShipperLiveness.stalenessWindow=42m",
		"edgeShipperLiveness.shippers.traefik.stalenessWindow=null",
		"traefikLogShipper.enabled=true",
	)
	if !strings.Contains(out, "_time:42m entryPointName:*") {
		t.Errorf("the shipper must fall back to the top-level stalenessWindow when its own is unset, got:\n%s", out)
	}
}

func TestEdgeShipperLiveness_Pages(t *testing.T) {
	t.Parallel()
	// A blind edge detective control must never be mistaken for a quiet one
	// (ka-0000002843), so it PAGES and carries the edge/shipper-stalled routing
	// labels.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeShipperLiveness.enabled=true",
		"traefikLogShipper.enabled=true",
	)
	idx := strings.Index(out, "alert: EdgeLogShipperStalled")
	if idx < 0 {
		t.Fatalf("EdgeLogShipperStalled alert missing, got:\n%s", out)
	}
	if !strings.Contains(out[idx:], `severity: "page"`) {
		t.Errorf("EdgeLogShipperStalled must default to page severity, got:\n%s", out[idx:])
	}
	if !strings.Contains(out[idx:], "detection: shipper-stalled") ||
		!strings.Contains(out[idx:], "component: edge") {
		t.Errorf("EdgeLogShipperStalled must carry the edge/shipper-stalled routing labels, got:\n%s", out[idx:])
	}
}

func TestEdgeShipperLiveness_UndeployedShipperNoRule(t *testing.T) {
	t.Parallel()
	// A rule for an UNDEPLOYED shipper would false-fire forever on a legitimately
	// absent series. So the rule renders ONLY for a shipper the chart deploys: a
	// shippers.<name> entry with no chart toggle behind it (here an overlay that
	// still names the deleted ava shipper, gd-R4Ao2mKEwZ) renders nothing, while
	// traefik (deployed) still does.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeShipperLiveness.enabled=true",
		"edgeShipperLiveness.shippers.ava.enabled=true",
		"edgeShipperLiveness.shippers.ava.selector=activity_name:*",
		"traefikLogShipper.enabled=true",
	)
	if !strings.Contains(out, `shipper: "traefik"`) {
		t.Errorf("deployed traefik shipper rule must render, got:\n%s", out)
	}
	if strings.Contains(out, `shipper: "ava"`) {
		t.Errorf("a shipper the chart does not deploy must NOT get a rule (would false-fire on an absent series), got:\n%s", out)
	}
}

func TestEdgeShipperLiveness_NoShipperDeployed_PlaceholderOnly(t *testing.T) {
	t.Parallel()
	// Feature on but no shipper deployed: the whole key is gated off (an empty
	// rule group is invalid), and only the placeholder renders.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeShipperLiveness.enabled=true",
		"traefikLogShipper.enabled=false",
	)
	if strings.Contains(out, "edge-shipper-liveness-logsql.yaml") ||
		strings.Contains(out, "EdgeLogShipperStalled") {
		t.Errorf("no rule key may render when no shipper is deployed, got:\n%s", out)
	}
	if !strings.Contains(out, "placeholder.yaml: |") {
		t.Errorf("placeholder must render when no rule key is active, got:\n%s", out)
	}
}

func TestEdgeShipperLiveness_RuleKeyOmittedWithoutNotifier(t *testing.T) {
	t.Parallel()
	// Alerting rules with no notifier crashloop vmalert; the rule key MUST be
	// gated off (placeholder only) when enabled + shippers deployed but no
	// notifier is wired.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=false",
		"vmalertLogs.notifierURL=",
		"vmalertLogs.notifierBlackhole=false",
		"edgeShipperLiveness.enabled=true",
		"traefikLogShipper.enabled=true",
	)
	if strings.Contains(out, "edge-shipper-liveness-logsql.yaml") {
		t.Errorf("rule key must be omitted when no notifier is available, got:\n%s", out)
	}
	if !strings.Contains(out, "placeholder.yaml: |") {
		t.Errorf("placeholder must render when enabled but no notifier, got:\n%s", out)
	}
}

func TestEdgeShipperLiveness_WindowSelectorFloorConfigurable(t *testing.T) {
	t.Parallel()
	// The staleness window, selector, and floor are all values — aligned with
	// whatever the transport publishes WITHOUT editing LogsQL. A nonzero floor
	// also catches a partial stall below a throughput floor on a busy shipper.
	// The window is set per shipper (it must be derived from that shipper's own
	// measured traffic — see ka-0000006504); top-level is only the fallback,
	// covered by TestEdgeShipperLiveness_PerShipperStalenessWindow.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeShipperLiveness.enabled=true",
		"traefikLogShipper.enabled=true",
		"edgeShipperLiveness.shippers.traefik.stalenessWindow=30m",
		"edgeShipperLiveness.shippers.traefik.selector=custom_field:*",
		"edgeShipperLiveness.shippers.traefik.floor=50",
	)
	if !strings.Contains(out, "_time:30m custom_field:*") {
		t.Errorf("window + selector overrides must flow into the expr, got:\n%s", out)
	}
	if !strings.Contains(out, "| filter rows:<=50") {
		t.Errorf("floor override must flow into the filter, got:\n%s", out)
	}
}

func TestEdgeShipperLiveness_CoexistsWithOtherLogsQLRules(t *testing.T) {
	t.Parallel()
	// All LogsQL rule sets ride the same ConfigMap + notifier gate; enabling
	// several must render each key independently and drop the placeholder.
	out := helmTemplate(t, "templates/vmalert-logs/configmap.yaml",
		"vmalertLogs.enabled=true",
		"alertmanager.enabled=true",
		"edgeShipperLiveness.enabled=true",
		"traefikLogShipper.enabled=true",
		"edgeContainment.enabled=true",
		"scannerSurge.enabled=true",
	)
	for _, want := range []string{
		"edge-shipper-liveness-logsql.yaml: |",
		"edge-containment-logsql.yaml: |",
		"scanner-surge-logsql.yaml: |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected rule key %q when all LogsQL rules enabled, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "placeholder.yaml: |") {
		t.Errorf("placeholder must be omitted once real rule keys render, got:\n%s", out)
	}
}
