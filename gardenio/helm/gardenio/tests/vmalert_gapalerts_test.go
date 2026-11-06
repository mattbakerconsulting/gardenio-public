package tests

import (
	"strings"
	"testing"
)

// Starter alert pack (task ka-0000001461, epic ka-0000000770): the fireable
// gap alerts (beads health, bridge liveness, dead-man's-switch) are authored as
// vmalert ConfigMap rule keys — the ONLY rule path that fires on new prod
// (PrometheusRule/VMRule CRDs are converted but never evaluated). The contract
// enforced here:
//   - the gap-alert key ONLY renders when a notifier is wired (recording-only
//     vmalert must stay clean, or vmalert crashloops on alerting-rules-without-
//     notifier),
//   - the fireable alerts render with severity + runbook_url + bead annotations,
//   - the blocked gaps (pending/nodepool) are OFF by default and only
//     render when their per-alert toggle flips,
//   - vmalert self-scrape annotation + the Alertmanager watchdog route/receiver
//     render as the dead-man's-switch delivery path.

// gapAlertsNotifierArgs is the minimal --set to wire a notifier so the gap
// alert key renders (alertmanager.enabled auto-wires vmalert -notifier.url).
var gapAlertsNotifierArgs = []string{
	"vmalert.enabled=true",
	"vmsingle.enabled=true",
	"alertmanager.enabled=true",
}

func TestGapAlerts_RecordingOnlyStaysClean(t *testing.T) {
	t.Parallel()
	// vmalert enabled WITHOUT a notifier: the gap-alert key must NOT render.
	// vmalert refuses to start if it loads alerting rules with no notifier, so
	// a recording-only deployment must remain recording-only.
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		"vmalert.enabled=true",
		"vmsingle.enabled=true",
	)
	if strings.Contains(out, "gap-alerts.yaml") {
		t.Errorf("gap-alerts key must NOT render without a notifier, got:\n%s", out)
	}
	if strings.Contains(out, "alert: ") {
		t.Errorf("no alerting rules may render in recording-only vmalert, got:\n%s", out)
	}
	// The recording rules must still be present (no regression).
	if !strings.Contains(out, "record: gardenio:beads_events:rate5m") {
		t.Errorf("recording rules must still render, got:\n%s", out)
	}
}

func TestGapAlerts_RenderWhenNotifierWired(t *testing.T) {
	t.Parallel()
	// BridgeAbsent renders only for enabled bridges (gd-YmPhRPJIfp), and every
	// bridge is off by default, so enable one here.
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(append([]string{}, gapAlertsNotifierArgs...), "slackBridge.enabled=true")...)
	if !strings.Contains(out, "gap-alerts.yaml") {
		t.Fatalf("gap-alerts key must render when a notifier is wired, got:\n%s", out)
	}
	// The fireable-today alerts.
	for _, alert := range []string{
		"alert: BeadsDaemonDown",
		"alert: BeadsDaemonAbsent",
		"alert: BeadsDBAuthErrors",
		"alert: BridgeDown",
		"alert: BridgeAbsent",
		"alert: Watchdog",
		"alert: VMAlertNotEvaluating",
		"alert: VMAlertDown",
	} {
		if !strings.Contains(out, alert) {
			t.Errorf("fireable alert %q missing, got:\n%s", alert, out)
		}
	}
	// The blocked gaps must NOT render by default.
	for _, alert := range []string{
		"alert: AgentPodsPendingTooLong",
		"alert: AgentPoolMemoryNearLimit",
		"alert: AgentFleetSizeHigh",
		"alert: AgentFleetIdleNodes",
	} {
		if strings.Contains(out, alert) {
			t.Errorf("blocked gap %q must be disabled by default, got:\n%s", alert, out)
		}
	}
}

// TestGapAlerts_FleetSizeHigh covers the P1b fleet-size alert (ka-0000003023):
// a blocked gap (default OFF, shares the node-lifecycle collector prereq) that
// keys on the controller-emitted gardenio_agent_node_count gauge, is
// warn-severity, and carries subsystem="cost" so it routes to the cost channel.
func TestGapAlerts_FleetSizeHigh(t *testing.T) {
	t.Parallel()
	// Off by default.
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if strings.Contains(out, "alert: AgentFleetSizeHigh") {
		t.Fatalf("AgentFleetSizeHigh must be OFF by default (blocked gap), got:\n%s", out)
	}

	// Renders when toggled on.
	out = helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.fleetSize.enabled=true")...)
	if !strings.Contains(out, "alert: AgentFleetSizeHigh") {
		t.Fatalf("AgentFleetSizeHigh must render when fleetSize.enabled=true, got:\n%s", out)
	}
	// Keys on the controller-emitted current-state gauge.
	if !strings.Contains(out, "gardenio_agent_node_count >") {
		t.Errorf("AgentFleetSizeHigh must select gardenio_agent_node_count, got:\n%s", out)
	}
	// Scope the assertions to this alert's block (from its alert: line to the
	// annotations, before the next alert).
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	fleet := gap[strings.Index(gap, "alert: AgentFleetSizeHigh"):]
	if i := strings.Index(fleet, "runbook_url:"); i >= 0 {
		fleet = fleet[:i]
	}
	if !strings.Contains(fleet, `severity: "warn"`) {
		t.Errorf("AgentFleetSizeHigh must be severity warn, got:\n%s", fleet)
	}
	// subsystem=cost is what routes it to #aws-cost-management (costRoute).
	if !strings.Contains(fleet, "subsystem: cost") {
		t.Errorf("AgentFleetSizeHigh must carry subsystem: cost for cost-channel routing, got:\n%s", fleet)
	}
}

// TestGapAlerts_IdleNodes covers the P1b-follow-up idle-node alert
// (ka-0000001398): a blocked gap (default OFF) that keys on the
// controller-emitted gardenio_agent_idle_node_count gauge, is warn-severity, and
// carries subsystem="cost" so it routes to the cost channel — the UNBLOCKED half
// of the node-utilization ask (provisioned-but-nothing-scheduled idle nodes).
func TestGapAlerts_IdleNodes(t *testing.T) {
	t.Parallel()
	// Off by default.
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if strings.Contains(out, "alert: AgentFleetIdleNodes") {
		t.Fatalf("AgentFleetIdleNodes must be OFF by default (blocked gap), got:\n%s", out)
	}

	// Renders when toggled on.
	out = helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.idleNodes.enabled=true")...)
	if !strings.Contains(out, "alert: AgentFleetIdleNodes") {
		t.Fatalf("AgentFleetIdleNodes must render when idleNodes.enabled=true, got:\n%s", out)
	}
	// Keys on the controller-emitted current-state gauge.
	if !strings.Contains(out, "gardenio_agent_idle_node_count >") {
		t.Errorf("AgentFleetIdleNodes must select gardenio_agent_idle_node_count, got:\n%s", out)
	}
	// Scope the assertions to this alert's block.
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	idle := gap[strings.Index(gap, "alert: AgentFleetIdleNodes"):]
	if i := strings.Index(idle, "runbook_url:"); i >= 0 {
		idle = idle[:i]
	}
	if !strings.Contains(idle, `severity: "warn"`) {
		t.Errorf("AgentFleetIdleNodes must be severity warn, got:\n%s", idle)
	}
	// subsystem=cost is what routes it to #aws-cost-management (costRoute).
	if !strings.Contains(idle, "subsystem: cost") {
		t.Errorf("AgentFleetIdleNodes must carry subsystem: cost for cost-channel routing, got:\n%s", idle)
	}
}

func TestGapAlerts_FireableExprsUseAvailableSeries(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	// GAP-4 selects on the beads component (up{} synthesized by vmagent).
	if !strings.Contains(out, `up{component="beads"} == 0`) {
		t.Errorf("BeadsDaemonDown must select up{component=\"beads\"}, got:\n%s", out)
	}
	// GAP-5 covers exactly the three scraped bridges.
	if !strings.Contains(out, `up{component=~"slack-bridge|jira-bridge|gitlab-bridge"} == 0`) {
		t.Errorf("BridgeDown must cover the three scraped bridges, got:\n%s", out)
	}
	// GAP-6 self-liveness keys on recording-rule freshness via absent_over_time
	// with an EXPLICIT range window — NOT time()-timestamp(), which is silent on
	// failure because the stale sample ages out of the 1m implicit lookback
	// before the staleness threshold is reachable (ka-0000001461 review finding).
	if !strings.Contains(out, "absent_over_time(gardenio:beads_events:rate5m[") {
		t.Errorf("VMAlertNotEvaluating must use absent_over_time with an explicit range, got:\n%s", out)
	}
	if strings.Contains(out, "timestamp(gardenio:beads_events:rate5m)") {
		t.Errorf("VMAlertNotEvaluating must NOT use the silent time()-timestamp() form, got:\n%s", out)
	}
}

// TestGapAlerts_BeadsDBAuth covers the beads DB-auth-failure alert
// (ka-0000006554): armed by default, keys on the gardenio_beads_db_auth_errors_total
// counter, pages, and has a per-alert disable toggle.
func TestGapAlerts_BeadsDBAuth(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if !strings.Contains(out, "alert: BeadsDBAuthErrors") {
		t.Fatalf("BeadsDBAuthErrors must render by default when a notifier is wired, got:\n%s", out)
	}
	// Must key on the counter the daemon actually emits (increase over 5m).
	if !strings.Contains(out, "increase(gardenio_beads_db_auth_errors_total[5m])") {
		t.Errorf("BeadsDBAuthErrors must select gardenio_beads_db_auth_errors_total, got:\n%s", out)
	}
	// P1 outage → page.
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	dbAuth := gap[strings.Index(gap, "alert: BeadsDBAuthErrors"):]
	dbAuth = dbAuth[:strings.Index(dbAuth, "- alert:")+len("- alert:")]
	if !strings.Contains(dbAuth, `severity: "page"`) {
		t.Errorf("BeadsDBAuthErrors must be severity page, got:\n%s", dbAuth)
	}

	out = helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.beadsDBAuth.enabled=false")...)
	if strings.Contains(out, "alert: BeadsDBAuthErrors") {
		t.Errorf("beadsDBAuth.enabled=false must suppress the alert, got:\n%s", out)
	}
}

func TestGapAlerts_EveryAlertHasRunbookAndBead(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	// Every alert in the pack must carry a runbook link and the incident bead.
	// Count alerts vs runbook_url/bead annotation occurrences within the key.
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	alerts := strings.Count(gap, "- alert:")
	runbooks := strings.Count(gap, "runbook_url:")
	beads := strings.Count(gap, "bead:")
	if alerts == 0 {
		t.Fatalf("expected gap alerts to render, got none:\n%s", out)
	}
	if runbooks < alerts {
		t.Errorf("every alert needs a runbook_url: %d alerts but %d runbook_url", alerts, runbooks)
	}
	if beads < alerts {
		t.Errorf("every alert needs a bead link: %d alerts but %d bead", alerts, beads)
	}
	if !strings.Contains(gap, "alerting-runbook.md") {
		t.Errorf("runbook_url must point at the alerting runbook doc, got:\n%s", gap)
	}
}

// TestGapAlerts_UnattributedSpend covers the Approach B residual alert
// (ka-0000003988): armed by default, ratio expr over the attribution-split
// gauges published together by tagSpendSync, and a per-alert disable toggle.
func TestGapAlerts_UnattributedSpend(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if !strings.Contains(out, "alert: UnattributedSpendHigh") {
		t.Fatalf("UnattributedSpendHigh must render by default when a notifier is wired, got:\n%s", out)
	}
	// The ratio must be computed over BOTH split gauges (same poller tick — the
	// denominator reconstructs the bill as unattributed + attributed).
	if !strings.Contains(out, "gardenio_unattributed_cost_usd") ||
		!strings.Contains(out, "gardenio_agent_attributed_cost_usd") {
		t.Errorf("UnattributedSpendHigh must use both attribution-split gauges, got:\n%s", out)
	}
	// Same gate caveat as the tag-cost validation rules: the minSpend gate must
	// be a label-less VECTOR (`and on() (...)`), not a bare scalar comparison.
	if !strings.Contains(out, "and on()") {
		t.Errorf("UnattributedSpendHigh minSpend gate must be `and on() (...)`, got:\n%s", out)
	}

	out = helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.unattributedSpend.enabled=false")...)
	if strings.Contains(out, "alert: UnattributedSpendHigh") {
		t.Errorf("unattributedSpend.enabled=false must suppress the alert, got:\n%s", out)
	}
}

// TestGapAlerts_VlsingleDiskUsage covers the VictoriaLogs disk-usage-over-cap
// alert (ka-0000001302): armed by default within gapAlerts, keys on the ratio
// of vl_data_size_bytes to the cap metric vl_max_disk_space_usage_bytes (NOT a
// hardcoded 8GiB), guards the denominator against divide-by-zero, is
// warn-severity, and has a per-alert disable toggle.
func TestGapAlerts_VlsingleDiskUsage(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if !strings.Contains(out, "alert: VlsingleDiskUsageHigh") {
		t.Fatalf("VlsingleDiskUsageHigh must render by default when a notifier is wired, got:\n%s", out)
	}
	// Ratio against the cap METRIC, so the alert tracks the
	// -retention.maxDiskSpaceUsageBytes flag automatically (no hardcoded cap
	// that could drift from the flag — advice ka-0000005924).
	if !strings.Contains(out, "vl_data_size_bytes{component=\"vlsingle\"}") ||
		!strings.Contains(out, "vl_max_disk_space_usage_bytes{component=\"vlsingle\"}") {
		t.Errorf("VlsingleDiskUsageHigh must divide vl_data_size_bytes by the cap metric vl_max_disk_space_usage_bytes, got:\n%s", out)
	}
	// storage + indexdb both count against the cap, so the numerator must sum
	// across the type label rather than pick one series.
	if !strings.Contains(out, "sum without(type) (vl_data_size_bytes") {
		t.Errorf("VlsingleDiskUsageHigh numerator must sum without(type) over vl_data_size_bytes, got:\n%s", out)
	}
	// Denominator guard: `> 0` makes the rule inert (empty vector) rather than
	// dividing by zero if the cap metric is ever 0/absent.
	if !strings.Contains(out, "vl_max_disk_space_usage_bytes{component=\"vlsingle\"} > 0") {
		t.Errorf("VlsingleDiskUsageHigh must guard the denominator with `> 0`, got:\n%s", out)
	}
	// Slow-moving capacity signal → warn, not page.
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	vl := gap[strings.Index(gap, "alert: VlsingleDiskUsageHigh"):]
	vl = vl[:strings.Index(vl, "- alert:")+len("- alert:")]
	if !strings.Contains(vl, `severity: "warn"`) {
		t.Errorf("VlsingleDiskUsageHigh must be severity warn, got:\n%s", vl)
	}

	out = helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.vlsingleDiskUsage.enabled=false")...)
	if strings.Contains(out, "alert: VlsingleDiskUsageHigh") {
		t.Errorf("vlsingleDiskUsage.enabled=false must suppress the alert, got:\n%s", out)
	}
}

func TestGapAlerts_MasterSwitchDisables(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.enabled=false")...)
	if strings.Contains(out, "gap-alerts.yaml") {
		t.Errorf("gapAlerts.enabled=false must suppress the whole key, got:\n%s", out)
	}
}

func TestGapAlerts_BlockedGapsRenderWhenToggled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"vmalert.gapAlerts.pendingPods.enabled=true",
			"vmalert.gapAlerts.nodepoolMemory.enabled=true",
		)...)
	for _, alert := range []string{
		"alert: AgentPodsPendingTooLong",
		"alert: AgentPoolMemoryNearLimit",
	} {
		if !strings.Contains(out, alert) {
			t.Errorf("blocked gap %q must render when toggled on, got:\n%s", alert, out)
		}
	}
}

func TestGapAlerts_NodepoolMemoryNoDivByZero(t *testing.T) {
	t.Parallel()
	// GAP-2 must be written as `usage >= threshold*limit`, NOT `usage/limit >=
	// threshold`: a 0/absent limit makes usage/0 = +Inf which satisfies the
	// threshold and spuriously pages (ka-0000001461 review finding).
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.nodepoolMemory.enabled=true")...)
	gap := out[strings.Index(out, "AgentPoolMemoryNearLimit"):]
	gap = gap[:strings.Index(gap, "- alert:")+len("- alert:")+200]
	if strings.Contains(gap, "/") && strings.Contains(gap, "karpenter_nodepools_limit") {
		// crude: the expr line should not divide by the limit series
		exprStart := strings.Index(gap, "expr:")
		exprEnd := strings.Index(gap, "for:")
		if exprStart >= 0 && exprEnd > exprStart && strings.Contains(gap[exprStart:exprEnd], "/") {
			t.Errorf("AgentPoolMemoryNearLimit must not divide by the limit (div-by-zero → +Inf spurious page), got:\n%s", gap[exprStart:exprEnd])
		}
	}
	if !strings.Contains(gap, "* karpenter_nodepools_limit") {
		t.Errorf("AgentPoolMemoryNearLimit must use `threshold * limit` form, got:\n%s", gap)
	}
}

func TestGapAlerts_NodepoolMemoryUsesResourceTypeLabel(t *testing.T) {
	t.Parallel()
	// GAP-2 must select on the LABEL KEY `resource_type`, not the pre-v1
	// `resource`: live Karpenter v1.3.3 labels the resource dimension
	// `resource_type` on both karpenter_nodepools_{usage,limit} (verified on
	// prod-gardenio-eks + karpenter source + acme-helm-chart karpenter-dashboard).
	// A wrong label key selects an EMPTY vector so the alert can NEVER fire
	// (ka-0000005173). This assertion prevents that silent regression.
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.nodepoolMemory.enabled=true")...)
	gap := out[strings.Index(out, "AgentPoolMemoryNearLimit"):]
	gap = gap[:strings.Index(gap, "for:")]
	// BOTH selectors (usage + limit) must carry resource_type="memory".
	if got := strings.Count(gap, `resource_type="memory"`); got != 2 {
		t.Errorf("GAP-2 expr must select resource_type=\"memory\" on BOTH usage and limit (want 2, got %d):\n%s", got, gap)
	}
	// The pre-v1 `resource="memory"` label must be gone — it selects an empty
	// vector against live karpenter v1.3.3 and the alert never fires.
	if strings.Contains(gap, `resource="memory"`) {
		t.Errorf("GAP-2 expr must NOT use the pre-v1 resource=\"memory\" label (never fires on live karpenter v1.3.3), got:\n%s", gap)
	}
}

func TestGapAlerts_EmptyIncidentBeadOmitsDeadLink(t *testing.T) {
	t.Parallel()
	// A blank incidentBead must OMIT the bead annotation, not emit a dead
	// trailing-slash link https://.../beads/ (ka-0000001461 review nit).
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.incidentBead=")...)
	if strings.Contains(out, `beads/"`) {
		t.Errorf("empty incidentBead must not emit a dead trailing-slash bead link, got:\n%s", out)
	}
}

func TestVMAlert_SelfScrapeAnnotation(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/deployment.yaml",
		"vmalert.enabled=true", "vmsingle.enabled=true")
	// selfScrape defaults true → the pod is annotated for vmagent discovery.
	if !strings.Contains(out, `prometheus.io/scrape: "true"`) {
		t.Errorf("vmalert pod must carry prometheus.io/scrape when selfScrape on, got:\n%s", out)
	}
	if !strings.Contains(out, `prometheus.io/port: "8880"`) {
		t.Errorf("vmalert self-scrape must target :8880, got:\n%s", out)
	}
	// Off switch honored.
	off := helmTemplate(t, "templates/vmalert/deployment.yaml",
		"vmalert.enabled=true", "vmsingle.enabled=true", "vmalert.selfScrape=false")
	if strings.Contains(off, "prometheus.io/scrape") {
		t.Errorf("selfScrape=false must drop the scrape annotation, got:\n%s", off)
	}
}

func TestAlertmanager_WatchdogRouteAndReceiver(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true")
	if !strings.Contains(out, `severity="watchdog"`) {
		t.Errorf("Alertmanager must route severity=watchdog, got:\n%s", out)
	}
	if !strings.Contains(out, "receiver: slack-watchdog") {
		t.Errorf("watchdog route must target slack-watchdog receiver, got:\n%s", out)
	}
	if !strings.Contains(out, "name: slack-watchdog") {
		t.Errorf("slack-watchdog receiver must be defined, got:\n%s", out)
	}
	// Off switch: no watchdog wiring when disabled.
	off := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true", "alertmanager.watchdog.enabled=false")
	if strings.Contains(off, "slack-watchdog") {
		t.Errorf("watchdog.enabled=false must drop the receiver/route, got:\n%s", off)
	}
}

// ka-0000008039: both Slack receivers (quiet warn + paging) must render the
// `dashboard` annotation as a clickable Slack "📊 Graph" link. Before this the
// template only emitted summary/description/runbook_url/bead, so a dashboard
// annotation (incl. the FSM alert's) was silently dropped.
func TestAlertmanager_SlackTemplatesRenderGraphLink(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true")
	// Slack-link syntax <url|label> built from the dashboard annotation, in
	// both the warn and page receiver text bodies.
	got := strings.Count(out, "{{ if .Annotations.dashboard }}📊 <{{ .Annotations.dashboard }}|Graph>")
	if got < 2 {
		t.Errorf("both slack-warn and slack-page must render the Graph link (want >=2, got %d):\n%s", got, out)
	}
}

// AdviceContextOverBudget (ka-0000004716, context-crisis plan ka-0000003733):
// fires when gardenio_prime_hook_bytes{source="advice"} exceeds the hard
// per-archetype budget for any (project, role). Enabled by default alongside
// the fireable pack; keyed on max by (project, role) so the firing series
// names the offending archetype.
func TestGapAlerts_AdviceBudgetRendersByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if !strings.Contains(out, "alert: AdviceContextOverBudget") {
		t.Fatalf("AdviceContextOverBudget must render with the fireable pack, got:\n%s", out)
	}
	if !strings.Contains(out, `max by (project, role) (gardenio_prime_hook_bytes{source="advice"}) > 81920`) {
		t.Errorf("AdviceContextOverBudget must threshold the advice source at the default 81920 bytes, got:\n%s", out)
	}
}

// ka-0000008039 (Matt 2026-07-14): the Slack alert links were dead (runbook_url
// → a private GitLab doc that 403s; bead → one stale incident). Every fireable
// alert now also carries a `dashboard` annotation deep-linking to the System
// Health Grafana board panel that shows the signal that fired, rendered by the
// Alertmanager→Slack template as a "📊 Graph" link.
func TestGapAlerts_EveryAlertHasGrafanaDashboardLink(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	alerts := strings.Count(gap, "- alert:")
	dashboards := strings.Count(gap, "dashboard:")
	if alerts == 0 {
		t.Fatalf("expected gap alerts to render, got none:\n%s", out)
	}
	// Every rendered alert must carry a dashboard link (default grafanaBaseURL
	// is non-empty, so the annotation is present on all of them).
	if dashboards < alerts {
		t.Errorf("every alert needs a dashboard link: %d alerts but %d dashboard", alerts, dashboards)
	}
	// The deep link points at the System Health board, pre-scoped to the last
	// hour, at a specific panel.
	if !strings.Contains(gap, "/d/gardenio-system-health/gardenio-system-health?viewPanel=") {
		t.Errorf("dashboard link must deep-link into the System Health board at a panel, got:\n%s", gap)
	}
	if !strings.Contains(gap, "from=now-1h&to=now") {
		t.Errorf("dashboard link must be pre-scoped to the last hour, got:\n%s", gap)
	}
}

// An empty gapAlerts.grafanaBaseURL must OMIT the dashboard annotation entirely
// (no dead "/grafana/d/..." link), mirroring the incidentBead empty-omit rule.
func TestGapAlerts_EmptyGrafanaBaseOmitsDashboard(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.grafanaBaseURL=")...)
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	if strings.Contains(gap, "dashboard:") {
		t.Errorf("empty grafanaBaseURL must omit the dashboard annotation, got:\n%s", gap)
	}
}

// The three NEW alerts (ka-0000008039) render by default and select on series
// that are live in prod vmsingle today.
func TestGapAlerts_NewAlertsRenderByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	for _, alert := range []string{
		"alert: AgentInvoluntaryDeaths",
		"alert: ThreadReplyDLQBacklog",
		"alert: TriggerStarved",
	} {
		if !strings.Contains(out, alert) {
			t.Errorf("new alert %q must render by default, got:\n%s", alert, out)
		}
	}
	// Exprs select the live series. The involuntary-deaths aggregate excludes
	// the coarse context_cancelled_unclaimed completeness family (C9
	// ka-0000001146) so rollout/recycle churn of taskless agents cannot re-arm
	// the ka-0000008254 false-positives on the alert.
	for _, expr := range []string{
		`increase(gardenio_agent_involuntary_deaths_total{family!="context_cancelled_unclaimed"}[1h])`,
		"gardenio_thread_reply_dlq_depth",
		"gardenio_trigger_starved",
	} {
		if !strings.Contains(out, expr) {
			t.Errorf("new alert expr %q missing, got:\n%s", expr, out)
		}
	}
}

// Each new alert honors its per-alert enable toggle.
func TestGapAlerts_NewAlertsToggleOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"vmalert.gapAlerts.involuntaryDeaths.enabled=false",
			"vmalert.gapAlerts.threadReplyDLQ.enabled=false",
			"vmalert.gapAlerts.triggerStarved.enabled=false",
		)...)
	for _, alert := range []string{
		"alert: AgentInvoluntaryDeaths",
		"alert: ThreadReplyDLQBacklog",
		"alert: TriggerStarved",
	} {
		if strings.Contains(out, alert) {
			t.Errorf("new alert %q must be suppressed when toggled off, got:\n%s", alert, out)
		}
	}
}

// The WS4 node-reclaim sentinel (ka-0000007155) renders by default, filters the
// live involuntary-deaths series to the context_cancelled family, and honors its
// own enable toggle + threshold independently of the broad AgentInvoluntaryDeaths
// aggregate.
func TestGapAlerts_NodeReclaimDeathsSentinel(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)

	// Renders by default.
	if !strings.Contains(out, "alert: AgentNodeReclaimDeaths") {
		t.Errorf("AgentNodeReclaimDeaths must render by default, got:\n%s", out)
	}
	// Filters the live series to the context_cancelled family at the default 2/h.
	if !strings.Contains(out, `gardenio_agent_involuntary_deaths_total{family="context_cancelled"}[1h]`) {
		t.Errorf("sentinel expr must filter to family=context_cancelled, got:\n%s", out)
	}
	if !strings.Contains(out, `{family="context_cancelled"}[1h])) > 2`) {
		t.Errorf("sentinel must default to a 2/h threshold below the 5/h aggregate, got:\n%s", out)
	}
	// Deep-links the family breakdown on the dedicated involuntary-deaths board,
	// not System Health.
	if !strings.Contains(out, "/d/gardenio-involuntary-deaths/gardenio-killed-mid-work-agents-involuntary-deaths?") {
		t.Errorf("sentinel dashboard link must point at the involuntary-deaths board, got:\n%s", out)
	}

	// Honors its own toggle without disabling the broad aggregate.
	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.nodeReclaimDeaths.enabled=false")...)
	if strings.Contains(off, "alert: AgentNodeReclaimDeaths") {
		t.Errorf("nodeReclaimDeaths.enabled=false must suppress the sentinel, got:\n%s", off)
	}
	if !strings.Contains(off, "alert: AgentInvoluntaryDeaths") {
		t.Errorf("disabling the sentinel must NOT disable the broad aggregate, got:\n%s", off)
	}

	// Custom threshold flows into the expr.
	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.nodeReclaimDeaths.threshold=4")...)
	if !strings.Contains(tuned, `{family="context_cancelled"}[1h])) > 4`) {
		t.Errorf("nodeReclaimDeaths.threshold must parameterize the expr, got:\n%s", tuned)
	}
}

// The DEFECT 2 rate-based mass-failure sentinel (ka-0000002985). Renders by
// default; computes a per-project involuntary-death-to-spawn RATIO with a
// small-absolute floor; honors its own toggle + tunables. The count-based
// AgentInvoluntaryDeaths cannot catch a project failing nearly every spawn at a
// modest absolute volume (the 2026-08 site-deployment loop: 137 deaths on 3
// items at ~3/h, never breaching 5/h) — this rule closes that hole.
func TestGapAlerts_ProjectFailureRateSentinel(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)

	if !strings.Contains(out, "alert: AgentProjectFailureRate") {
		t.Fatalf("AgentProjectFailureRate must render by default, got:\n%s", out)
	}
	block := alertBlock(t, out, "AgentProjectFailureRate")

	// Ratio of deaths / spawns, both per project over the same 24h window, with
	// the coarse context_cancelled_unclaimed family excluded (same as the broad
	// aggregate).
	for _, want := range []string{
		`sum by (project) (increase(gardenio_agent_involuntary_deaths_total{family!="context_cancelled_unclaimed"}[24h]))`,
		`sum by (project) (increase(gardenio_agent_lifecycle_events_total{event="spawned"}[24h]))`,
		") > 0.5",     // ratioThreshold
		"[24h])) > 2", // minDeaths absolute floor
		"and",         // ratio AND floor joined on project
	} {
		if !strings.Contains(block, want) {
			t.Errorf("AgentProjectFailureRate expr missing %q, got:\n%s", want, block)
		}
	}
	// warn, so it routes to slack-warn (not a page).
	if !strings.Contains(block, `severity: "warn"`) {
		t.Errorf("AgentProjectFailureRate must be warn severity, got:\n%s", block)
	}

	// Honors its own toggle without disabling the broad aggregate.
	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.projectFailureRate.enabled=false")...)
	if strings.Contains(off, "alert: AgentProjectFailureRate") {
		t.Errorf("projectFailureRate.enabled=false must suppress the sentinel, got:\n%s", off)
	}
	if !strings.Contains(off, "alert: AgentInvoluntaryDeaths") {
		t.Errorf("disabling the rate sentinel must NOT disable the broad aggregate, got:\n%s", off)
	}

	// Tunables flow into the expr.
	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"vmalert.gapAlerts.projectFailureRate.ratioThreshold=0.8",
			"vmalert.gapAlerts.projectFailureRate.minDeaths=5",
			"vmalert.gapAlerts.projectFailureRate.window=12h",
		)...)
	tunedBlock := alertBlock(t, tuned, "AgentProjectFailureRate")
	for _, want := range []string{") > 0.8", "[12h])) > 5", "[12h]))"} {
		if !strings.Contains(tunedBlock, want) {
			t.Errorf("projectFailureRate tunables must parameterize the expr (missing %q), got:\n%s", want, tunedBlock)
		}
	}
}

func TestGapAlerts_AdviceBudgetToggleAndThreshold(t *testing.T) {
	t.Parallel()
	// Disabled per-toggle.
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.adviceBudget.enabled=false")...)
	if strings.Contains(out, "alert: AdviceContextOverBudget") {
		t.Errorf("adviceBudget.enabled=false must suppress the alert, got:\n%s", out)
	}
	// Custom threshold flows into the expr.
	out = helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.adviceBudget.maxBytes=100000")...)
	if !strings.Contains(out, `> 100000`) {
		t.Errorf("adviceBudget.maxBytes must parameterize the expr, got:\n%s", out)
	}
}

// Track D (ka-0000001509, operator ask ka-0000006448): the DIRECT agent-OOM
// Slack alert. It renders by default on the fireable path, reads the live
// oom_killed lifecycle series (NOT the ECS-joined fsm-composite rule, which is
// an inert PrometheusRule on new prod), and honors its per-alert toggle +
// threshold. severity=warn routes it to the slack-warn receiver.
func TestGapAlerts_OOMKilledFiresToSlack(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)

	// Renders by default (always-live series — not a blocked gap).
	if !strings.Contains(out, "alert: AgentOOMKilled") {
		t.Fatalf("AgentOOMKilled must render by default, got:\n%s", out)
	}
	// Reads the controller-emitted OOM lifecycle counter, per (project, role) so
	// the firing series names who OOMed. This is the un-joined, always-fireable
	// sibling of fsm-composite FSMAgentOOMWithECSRestart.
	if !strings.Contains(out, `sum by (project, role) (increase(gardenio_agent_lifecycle_events_total{event="oom_killed"}[1h]))`) {
		t.Errorf("AgentOOMKilled must select the oom_killed lifecycle series by (project, role), got:\n%s", out)
	}
	// severity=warn routes to the slack-warn receiver (the default Slack path).
	block := alertBlock(t, out, "AgentOOMKilled")
	if !strings.Contains(block, `severity: "warn"`) {
		t.Errorf("AgentOOMKilled must carry severity=warn so it routes to slack-warn, got:\n%s", block)
	}

	// Honors its own toggle.
	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.oomKilled.enabled=false")...)
	if strings.Contains(off, "alert: AgentOOMKilled") {
		t.Errorf("oomKilled.enabled=false must suppress the alert, got:\n%s", off)
	}

	// Custom threshold + severity flow into the render.
	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"vmalert.gapAlerts.oomKilled.threshold=3",
			"vmalert.gapAlerts.oomKilled.severity=page")...)
	if !strings.Contains(tuned, `[1h])) > 3`) {
		t.Errorf("oomKilled.threshold must parameterize the expr, got:\n%s", tuned)
	}
	tunedBlock := alertBlock(t, tuned, "AgentOOMKilled")
	if !strings.Contains(tunedBlock, `severity: "page"`) {
		t.Errorf("oomKilled.severity must parameterize the label, got:\n%s", tunedBlock)
	}
}

// Track D (ka-0000001509): the node-lifecycle churn alert. A BLOCKED gap — its
// series (the new controller-emitted gardenio_node_lifecycle_events_total
// counter) exists only when the collector is enabled, so the alert is OFF by
// default and renders only when toggled. When on it counts reason="terminated"
// ONLY — the once-per-node vanish edge (ka-0000005966: per-node, not per-edge; a
// graceful teardown emits disrupting+deleting+terminated, so summing all three
// counted ~3 events per node gone). It EXCLUDES disrupting/deleting/not_ready
// from the alert expr (those stay emitted for the dashboard breakdown).
// severity=warn routes it to slack-warn.
func TestGapAlerts_NodeLifecycleFiresToSlack(t *testing.T) {
	t.Parallel()
	// BLOCKED gap: off by default (would otherwise evaluate a missing series).
	def := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if strings.Contains(def, "alert: AgentNodeLifecycleEvents") {
		t.Errorf("AgentNodeLifecycleEvents must be OFF by default (blocked gap), got:\n%s", def)
	}

	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.nodeLifecycle.enabled=true")...)
	if !strings.Contains(out, "alert: AgentNodeLifecycleEvents") {
		t.Fatalf("AgentNodeLifecycleEvents must render when toggled on, got:\n%s", out)
	}
	// Counts the once-per-node terminated edge over the new counter (per-node,
	// not per-edge — ka-0000005966).
	if !strings.Contains(out, `sum(increase(gardenio_node_lifecycle_events_total{reason="terminated"}[1h]))`) {
		t.Errorf("AgentNodeLifecycleEvents must count the terminated (per-node) reason over the node-lifecycle counter, got:\n%s", out)
	}
	// The old per-edge form (summing disrupting|deleting|terminated) must be gone:
	// a graceful teardown emits all three, so summing counts ~3 events per node
	// gone and trips the per-node threshold on ~2 clean consolidations/h.
	if strings.Contains(out, `reason=~"disrupting|deleting|terminated"`) {
		t.Errorf("AgentNodeLifecycleEvents must NOT sum the per-edge reasons (per-node terminated count only, ka-0000005966), got:\n%s", out)
	}
	block := alertBlock(t, out, "AgentNodeLifecycleEvents")
	if !strings.Contains(block, `severity: "warn"`) {
		t.Errorf("AgentNodeLifecycleEvents must carry severity=warn so it routes to slack-warn, got:\n%s", block)
	}

	// Custom threshold flows into the expr.
	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"vmalert.gapAlerts.nodeLifecycle.enabled=true",
			"vmalert.gapAlerts.nodeLifecycle.threshold=12")...)
	if !strings.Contains(tuned, `[1h])) > 12`) {
		t.Errorf("nodeLifecycle.threshold must parameterize the expr, got:\n%s", tuned)
	}
}

// ka-0000004184 (operator ask Matt 2026-07-24): the child-process-OOM-aware
// sibling of AgentOOMKilled. A BLOCKED gap — container_oom_events_total is in
// vmsingle only when the cadvisor scrape is on, so the alert is OFF by default
// and renders only when toggled. When on it counts the cadvisor cgroup oom_kill
// counter by (namespace, pod) so a workflow-CHILD OOM (invisible to the kubelet
// PID1 reason=OOMKilled that AgentOOMKilled reads) is caught. severity=warn
// routes it to slack-warn.
func TestGapAlerts_ContainerOOMKilledFiresToSlack(t *testing.T) {
	t.Parallel()
	// BLOCKED gap: off by default (would otherwise evaluate a missing series).
	def := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if strings.Contains(def, "alert: AgentContainerOOMKilled") {
		t.Errorf("AgentContainerOOMKilled must be OFF by default (blocked gap), got:\n%s", def)
	}

	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.containerOOMKilled.enabled=true")...)
	if !strings.Contains(out, "alert: AgentContainerOOMKilled") {
		t.Fatalf("AgentContainerOOMKilled must render when toggled on, got:\n%s", out)
	}
	// Reads the cadvisor cgroup OOM-kill counter by (namespace, pod) — the
	// signal that ALSO counts workflow child-process OOMs under coop (PID1),
	// unlike the kubelet reason=OOMKilled AgentOOMKilled reads.
	if !strings.Contains(out, `sum by (namespace, pod) (increase(container_oom_events_total{namespace=~"gardenio|gardenio-e2e.*"}[1h]))`) {
		t.Errorf("AgentContainerOOMKilled must select container_oom_events_total by (namespace, pod), got:\n%s", out)
	}
	block := alertBlock(t, out, "AgentContainerOOMKilled")
	if !strings.Contains(block, `severity: "warn"`) {
		t.Errorf("AgentContainerOOMKilled must carry severity=warn so it routes to slack-warn, got:\n%s", block)
	}

	// Custom threshold + namespaceRegex + severity flow into the render.
	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"vmalert.gapAlerts.containerOOMKilled.enabled=true",
			"vmalert.gapAlerts.containerOOMKilled.threshold=2",
			"vmalert.gapAlerts.containerOOMKilled.namespaceRegex=gardenio-agents.*",
			"vmalert.gapAlerts.containerOOMKilled.severity=page")...)
	if !strings.Contains(tuned, `[1h])) > 2`) {
		t.Errorf("containerOOMKilled.threshold must parameterize the expr, got:\n%s", tuned)
	}
	if !strings.Contains(tuned, `container_oom_events_total{namespace=~"gardenio-agents.*"}`) {
		t.Errorf("containerOOMKilled.namespaceRegex must parameterize the expr, got:\n%s", tuned)
	}
	tunedBlock := alertBlock(t, tuned, "AgentContainerOOMKilled")
	if !strings.Contains(tunedBlock, `severity: "page"`) {
		t.Errorf("containerOOMKilled.severity must parameterize the label, got:\n%s", tunedBlock)
	}
}

// TestGapAlerts_SlackBridgeRespawnPhaseSlow covers the cold-respawn phase
// anomaly alert (task ka-0000001457): armed by default, an increase() over the
// per-phase slow counter, a `phase`-labelled summary, and a per-alert disable
// toggle. This is what turns a silent multi-minute respawn into a page that
// localizes the slow leg (bridge dispatch vs pod boot).
func TestGapAlerts_SlackBridgeRespawnPhaseSlow(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if !strings.Contains(out, "alert: SlackBridgeRespawnPhaseSlow") {
		t.Fatalf("SlackBridgeRespawnPhaseSlow must render by default when a notifier is wired, got:\n%s", out)
	}
	block := alertBlock(t, out, "SlackBridgeRespawnPhaseSlow")
	// The expr must be an increase() over the per-phase slow counter, grouped by
	// phase so the firing series names the slow leg (dispatch/boot/total).
	if !strings.Contains(block, "increase(gardenio_slackbridge_respawn_phase_slow_total[1h])") {
		t.Errorf("expr must be an increase() over the phase slow counter, got:\n%s", block)
	}
	if !strings.Contains(block, "sum by (phase)") {
		t.Errorf("expr must group by phase so the alert localizes the slow leg, got:\n%s", block)
	}
	// The $labels.phase runtime template must survive Helm rendering (double-brace
	// preserved for vmalert to interpolate at fire time).
	if !strings.Contains(block, "{{ $labels.phase }}") {
		t.Errorf("summary/description must interpolate $labels.phase at fire time, got:\n%s", block)
	}
	// Per-alert disable toggle must suppress the render.
	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.respawnPhaseSlow.enabled=false")...)
	if strings.Contains(off, "alert: SlackBridgeRespawnPhaseSlow") {
		t.Errorf("respawnPhaseSlow.enabled=false must suppress the alert, got:\n%s", off)
	}
}

// alertBlock returns the rendered YAML for a single alert stanza (from its
// `- alert: <name>` line up to the next `- alert:` or the end), so a test can
// assert on labels/annotations scoped to one alert without matching a sibling.
func alertBlock(t *testing.T, out, name string) string {
	t.Helper()
	start := strings.Index(out, "- alert: "+name)
	if start < 0 {
		t.Fatalf("alert %q not found in render:\n%s", name, out)
	}
	rest := out[start+len("- alert: "+name):]
	if next := strings.Index(rest, "- alert:"); next >= 0 {
		return out[start : start+len("- alert: "+name)+next]
	}
	return out[start:]
}

// PlatformPodCrashLooping (ka-0000003606) — regression guard for a measured
// 4-day prod blind spot. The litellm proxy (the fleet's LLM data-plane) sat in
// an OOMKill crashloop at 159 restarts/24h with NO rule able to fire:
// AgentOOMKilled reads an agent-pod lifecycle series a platform singleton never
// emits, AgentContainerOOMKilled is default-off and its namespaceRegex
// full-match excludes the gardenio-system namespace where the singletons run,
// and up{}-based liveness misses a pod that dies every ~9min but is scrape-UP
// in between.
func TestGapAlerts_PlatformPodCrashLooping(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	block := alertBlock(t, out, "PlatformPodCrashLooping")

	// Must be armed by DEFAULT — the whole point is that the previous coverage
	// was default-off. It reads kube_pod_container_status_restarts_total, which
	// pod-state-metrics already puts in vmsingle, so it belongs in the
	// fireable-today group and needs no new scrape (contrast the blocked gaps).
	if !strings.Contains(block, "kube_pod_container_status_restarts_total") {
		t.Errorf("expr must read the already-scraped restarts counter, got:\n%s", block)
	}
	// A RATE over a window, not a cumulative count: a pod that restarted twice
	// last week must not alert forever.
	if !strings.Contains(block, "increase(") {
		t.Errorf("expr must use increase() so an old restart does not alert forever, got:\n%s", block)
	}
	// PLATFORM-scoped: the release namespace also runs ephemeral agent pods and
	// short-lived Job pods. Without this exclusion the rule double-reports agent
	// churn the Agent*OOMKilled rules already own, and fans out per-pod — one
	// 08-05 crew-e2e burst produced 12 simultaneous firing series for a single
	// root cause when the expression was replayed over real data.
	if !strings.Contains(block, `container!~"agent|reap|stamp|adopt|migrate"`) {
		t.Errorf("expr must exclude agent + Job containers to stay platform-scoped, got:\n%s", block)
	}
	// by (namespace, pod, container) so the firing series names the exact
	// container instead of collapsing every singleton into one alert.
	if !strings.Contains(block, "sum by (namespace, pod, container)") {
		t.Errorf("expr must group by namespace/pod/container, got:\n%s", block)
	}
	// Triage affordances the on-call actually needs.
	if !strings.Contains(block, "runbook_url:") {
		t.Errorf("alert must carry runbook_url, got:\n%s", block)
	}
	if !strings.Contains(block, "{{ $labels.pod }}") {
		t.Errorf("summary/description must interpolate $labels.pod at fire time, got:\n%s", block)
	}
	// The description must keep pointing at lastState.terminated.reason/exitCode —
	// that is what distinguishes an OOMKill (sizing/leak) from a crash (logs).
	if !strings.Contains(block, "OOMKilled") {
		t.Errorf("description must tell the on-call to check for OOMKilled, got:\n%s", block)
	}

	// Per-alert disable toggle must suppress the render.
	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.platformPodRestarts.enabled=false")...)
	if strings.Contains(off, "alert: PlatformPodCrashLooping") {
		t.Errorf("platformPodRestarts.enabled=false must suppress the alert, got:\n%s", off)
	}
}

// The litellm memory ceiling that caused ka-0000003606. Prod steady-state
// working set measured 1.1-1.2Gi with routine peaks to ~1.9Gi, so the old 2Gi
// limit sat ~5% above the normal peak and fleet growth walked it into a
// sustained OOMKill crashloop. Guard both numbers: the limit must stay clear of
// the observed peak, and the request must not under-declare the real footprint
// (a 1Gi request placed the pod as if it were half its true size).
func TestLitellm_MemorySizingClearsObservedPeak(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/deployment.yaml", "litellm.enabled=true")
	if !strings.Contains(out, "memory: 4Gi") {
		t.Errorf("litellm memory LIMIT must be 4Gi (~2x the observed ~1.9Gi peak); a 2Gi limit is what OOM-crashlooped, got:\n%s", out)
	}
	if !strings.Contains(out, "memory: 2Gi") {
		t.Errorf("litellm memory REQUEST must be 2Gi to match the real steady-state footprint, got:\n%s", out)
	}
}
