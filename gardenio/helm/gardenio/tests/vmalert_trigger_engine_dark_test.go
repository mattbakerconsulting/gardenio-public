package tests

import (
	"strings"
	"testing"
)

// TriggerEngineScheduleDark (task ka-0000005325) — the trigger-engine ENGINE-LOOP
// DEADMAN, authored as a vmalert ConfigMap rule key (the fireable-on-prod path,
// mirroring vmalert_gapalerts_test.go) rather than a PrometheusRule CRD, because
// on new prod the CRDs are converted but never evaluated — a bare PrometheusRule
// would reproduce the exact 30h-blackout invisibility this alert exists to close.
//
// THE GAP THIS GUARDS. On 2026-08-09→08-10 the trigger-engine fired ZERO
// scheduled triggers for ~30h while its pod stayed Ready with restartCount=0. It
// was invisible to every existing guard: the old /healthz was STATIC (always ok),
// and every engine-side liveness sweep — including the sibling TriggerStarved
// alert — reads a series computed INSIDE the very Evaluate() loop that went dark,
// so all of them froze at their last value. gardenio_trigger_engine_evaluations_
// total is different: the engine beats it once per COMPLETED cycle on the poll
// timer regardless of trigger state, so increase(...)==0 (or absent()) catches
// the wedge the derived-from-the-loop signals cannot. These tests pin that the
// alert renders on the fireable path, uses the un-suppressible deadman expr
// shape, pages, and honors its toggle.

func TestTriggerEngineDark_RendersByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if !strings.Contains(out, "alert: TriggerEngineScheduleDark") {
		t.Fatalf("TriggerEngineScheduleDark must render by default when a notifier is wired, got:\n%s", out)
	}
	// It must live inside the FIREABLE gap-alerts key (the path that actually
	// evaluates on prod), not merely somewhere in the ConfigMap.
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	if !strings.Contains(gap, "alert: TriggerEngineScheduleDark") {
		t.Errorf("TriggerEngineScheduleDark must render inside the fireable gap-alerts key, got:\n%s", gap)
	}
}

func TestTriggerEngineDark_DeadmanExprNotSuppressibleByAbsentSeries(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	block := alertBlock(t, out, "TriggerEngineScheduleDark")
	// The heartbeat the engine beats once per completed Evaluate() cycle.
	if !strings.Contains(block, "increase(gardenio_trigger_engine_evaluations_total[15m]) == 0") {
		t.Errorf("expr must fire when the evaluations heartbeat stops increasing over the window, got:\n%s", block)
	}
	// absent() MUST be OR-ed in so a never-scraped / crash-before-first-scrape /
	// pod-gone engine still pages — a bare increase() over a never-written series
	// evaluates to no-data, not 0, and would leave the alert un-fireable in
	// exactly the total-outage case it exists for.
	if !strings.Contains(block, "absent(gardenio_trigger_engine_evaluations_total)") {
		t.Errorf("deadman expr must OR in absent() so a never-scraped engine still pages, got:\n%s", block)
	}
	// The verdict must be a self-contained deadman leg — no `and`-joined second
	// term whose absent series could silently suppress the page (the same folded
	// discipline the webhook-deadman siblings enforce).
	exprStart := strings.Index(block, "expr:")
	exprEnd := strings.Index(block, "for:")
	if exprStart >= 0 && exprEnd > exprStart {
		expr := block[exprStart:exprEnd]
		if strings.Contains(expr, " and ") || strings.Contains(expr, "\n            and") {
			t.Errorf("deadman expr must not `and`-join a term an absent series could suppress, got expr:\n%s", expr)
		}
	}
}

func TestTriggerEngineDark_Pages(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	block := alertBlock(t, out, "TriggerEngineScheduleDark")
	// A dark engine silently halts every scheduled trigger — this is a page, not
	// a warn.
	if !strings.Contains(block, `severity: "page"`) {
		t.Errorf("TriggerEngineScheduleDark must be severity page (a dark engine halts all scheduled automation), got:\n%s", block)
	}
	if !strings.Contains(block, "component: trigger-engine") {
		t.Errorf("alert must carry component: trigger-engine, got:\n%s", block)
	}
	// Triage affordances: runbook link + the incident bead annotation.
	if !strings.Contains(block, "runbook_url:") {
		t.Errorf("alert must carry a runbook_url, got:\n%s", block)
	}
}

func TestTriggerEngineDark_ToggleOffAndConfigurable(t *testing.T) {
	t.Parallel()
	// Per-alert disable toggle must suppress the render without touching siblings.
	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.triggerEngineDark.enabled=false")...)
	if strings.Contains(off, "alert: TriggerEngineScheduleDark") {
		t.Errorf("triggerEngineDark.enabled=false must suppress the alert, got:\n%s", off)
	}
	if !strings.Contains(off, "alert: TriggerStarved") {
		t.Errorf("disabling triggerEngineDark must NOT disable the sibling TriggerStarved, got:\n%s", off)
	}

	// window + for flow into the render.
	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"vmalert.gapAlerts.triggerEngineDark.window=45m",
			"vmalert.gapAlerts.triggerEngineDark.for=20m")...)
	if !strings.Contains(tuned, "gardenio_trigger_engine_evaluations_total[45m]") {
		t.Errorf("triggerEngineDark.window must parameterize the expr, got:\n%s", tuned)
	}
	block := alertBlock(t, tuned, "TriggerEngineScheduleDark")
	if !strings.Contains(block, `for: "20m"`) {
		t.Errorf("triggerEngineDark.for must parameterize the for-hold, got:\n%s", block)
	}
}

// The master gapAlerts switch must also suppress this alert (it is part of the
// pack, not a standalone key).
func TestTriggerEngineDark_MasterSwitchDisables(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.enabled=false")...)
	if strings.Contains(out, "alert: TriggerEngineScheduleDark") {
		t.Errorf("gapAlerts.enabled=false must suppress TriggerEngineScheduleDark, got:\n%s", out)
	}
}

// TriggerEngineMachineAuthUnavailable (gd-Hc34EENbO9). The engine fails closed
// when it has no machine token (the Secret mount is optional), but a failed
// cycle still completes, so the heartbeat TriggerEngineScheduleDark reads stays
// green. This sibling reads gardenio_machine_token_unavailable_total, which
// counts exactly the daemon calls the engine could not send.
func TestTriggerEngineMachineAuth_RendersAndPages(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	if !strings.Contains(gap, "alert: TriggerEngineMachineAuthUnavailable") {
		t.Fatalf("TriggerEngineMachineAuthUnavailable must render inside the fireable gap-alerts key, got:\n%s", gap)
	}
	block := alertBlock(t, out, "TriggerEngineMachineAuthUnavailable")
	if !strings.Contains(block, `increase(gardenio_machine_token_unavailable_total{client="gardenio-trigger-engine"}[10m])) > 0`) {
		t.Errorf("expr must fire on the engine client's auth_unavailable counter increasing, got:\n%s", block)
	}
	if strings.Contains(block, "absent(") {
		t.Errorf("no absent() leg: the series only exists after a first failure, got:\n%s", block)
	}
	for _, want := range []string{`severity: "page"`, "component: trigger-engine", "runbook_url:"} {
		if !strings.Contains(block, want) {
			t.Errorf("alert must carry %q, got:\n%s", want, block)
		}
	}
}

func TestTriggerEngineMachineAuth_ClientIdToggleAndSiblings(t *testing.T) {
	t.Parallel()
	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"triggerEngine.machinePrincipal.clientId=custom-engine",
			"vmalert.gapAlerts.triggerEngineMachineAuth.window=20m")...)
	block := alertBlock(t, tuned, "TriggerEngineMachineAuthUnavailable")
	if !strings.Contains(block, `{client="custom-engine"}[20m]`) {
		t.Errorf("clientId and window must parameterize the expr, got:\n%s", block)
	}

	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.triggerEngineMachineAuth.enabled=false")...)
	if strings.Contains(off, "alert: TriggerEngineMachineAuthUnavailable") {
		t.Errorf("triggerEngineMachineAuth.enabled=false must suppress the alert, got:\n%s", off)
	}
	if !strings.Contains(off, "alert: TriggerEngineScheduleDark") {
		t.Errorf("disabling it must NOT disable the sibling TriggerEngineScheduleDark")
	}
}

// TriggerEngineMachineForbidden (gd-cr3BJRmyTr). A valid token whose role is not
// yet attached is denied (403) on every write; 155 denials in 20 minutes raised
// no alert on the #370 roll. It reads the client-side per-principal 403 counter.
func TestTriggerEngineMachineForbidden_RendersAndPages(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	if !strings.Contains(gap, "alert: TriggerEngineMachineForbidden") {
		t.Fatalf("TriggerEngineMachineForbidden must render inside the fireable gap-alerts key, got:\n%s", gap)
	}
	block := alertBlock(t, out, "TriggerEngineMachineForbidden")
	if !strings.Contains(block, `increase(gardenio_machine_forbidden_total{client="gardenio-trigger-engine"}[5m])) > 20`) {
		t.Errorf("expr must fire on the engine client's 403 counter exceeding the threshold, got:\n%s", block)
	}
	if strings.Contains(block, "absent(") {
		t.Errorf("no absent() leg: the series only exists after a first 403, got:\n%s", block)
	}
	for _, want := range []string{`severity: "page"`, "component: trigger-engine", "runbook_url:", "#triggerenginemachineforbidden"} {
		if !strings.Contains(block, want) {
			t.Errorf("alert must carry %q, got:\n%s", want, block)
		}
	}
}

func TestTriggerEngineMachineForbidden_TuningAndToggle(t *testing.T) {
	t.Parallel()
	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"triggerEngine.machinePrincipal.clientId=custom-engine",
			"vmalert.gapAlerts.triggerEngineMachineForbidden.window=10m",
			"vmalert.gapAlerts.triggerEngineMachineForbidden.threshold=5")...)
	block := alertBlock(t, tuned, "TriggerEngineMachineForbidden")
	if !strings.Contains(block, `{client="custom-engine"}[10m])) > 5`) {
		t.Errorf("clientId, window and threshold must parameterize the expr, got:\n%s", block)
	}

	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.triggerEngineMachineForbidden.enabled=false")...)
	if strings.Contains(off, "alert: TriggerEngineMachineForbidden") {
		t.Errorf("triggerEngineMachineForbidden.enabled=false must suppress the alert, got:\n%s", off)
	}
	if !strings.Contains(off, "alert: TriggerEngineMachineAuthUnavailable") {
		t.Errorf("disabling it must NOT disable the sibling TriggerEngineMachineAuthUnavailable")
	}
}

// SlackBridgeMachineAuthUnavailable (gd-Hc34EENbO9 component 4). The bridge fails
// its machine-principal writes (config seed, dm delivery) closed without a token
// while staying Ready on its fleet token, so only the auth_unavailable counter,
// scoped to its client, shows it.
func TestSlackBridgeMachineAuth_RendersAndPages(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	if !strings.Contains(gap, "alert: SlackBridgeMachineAuthUnavailable") {
		t.Fatalf("SlackBridgeMachineAuthUnavailable must render inside the fireable gap-alerts key, got:\n%s", gap)
	}
	block := alertBlock(t, out, "SlackBridgeMachineAuthUnavailable")
	if !strings.Contains(block, `increase(gardenio_machine_token_unavailable_total{client="gardenio-slack-bridge"}[10m])) > 0`) {
		t.Errorf("expr must fire on the bridge client's auth_unavailable counter increasing, got:\n%s", block)
	}
	if strings.Contains(block, "absent(") {
		t.Errorf("no absent() leg: the series only exists after a first failure, got:\n%s", block)
	}
	for _, want := range []string{`severity: "page"`, "component: slack-bridge", "runbook_url:"} {
		if !strings.Contains(block, want) {
			t.Errorf("alert must carry %q, got:\n%s", want, block)
		}
	}
}

func TestSlackBridgeMachineAuth_ClientIdToggleAndSiblings(t *testing.T) {
	t.Parallel()
	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"slackBridge.machinePrincipal.clientId=custom-bridge",
			"vmalert.gapAlerts.slackBridgeMachineAuth.window=20m")...)
	block := alertBlock(t, tuned, "SlackBridgeMachineAuthUnavailable")
	if !strings.Contains(block, `{client="custom-bridge"}[20m]`) {
		t.Errorf("clientId and window must parameterize the expr, got:\n%s", block)
	}

	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.slackBridgeMachineAuth.enabled=false")...)
	if strings.Contains(off, "alert: SlackBridgeMachineAuthUnavailable") {
		t.Errorf("slackBridgeMachineAuth.enabled=false must suppress the alert, got:\n%s", off)
	}
	if !strings.Contains(off, "alert: TriggerEngineMachineAuthUnavailable") {
		t.Errorf("disabling it must NOT disable the sibling TriggerEngineMachineAuthUnavailable")
	}
}

// SlackBridgeMachineForbidden (gd-Hc34EENbO9 component 4). A valid token whose
// roles are not yet attached is denied (403) on every write while the pod stays
// Ready; dm traffic is light, so the threshold is low.
func TestSlackBridgeMachineForbidden_RendersAndPages(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	if !strings.Contains(gap, "alert: SlackBridgeMachineForbidden") {
		t.Fatalf("SlackBridgeMachineForbidden must render inside the fireable gap-alerts key, got:\n%s", gap)
	}
	block := alertBlock(t, out, "SlackBridgeMachineForbidden")
	if !strings.Contains(block, `increase(gardenio_machine_forbidden_total{client="gardenio-slack-bridge"}[10m])) > 3`) {
		t.Errorf("expr must fire on the bridge client's 403 counter exceeding the low threshold, got:\n%s", block)
	}
	if strings.Contains(block, "absent(") {
		t.Errorf("no absent() leg: the series only exists after a first 403, got:\n%s", block)
	}
	for _, want := range []string{`severity: "page"`, "component: slack-bridge", "runbook_url:", "#slackbridgemachineforbidden"} {
		if !strings.Contains(block, want) {
			t.Errorf("alert must carry %q, got:\n%s", want, block)
		}
	}
}

func TestSlackBridgeMachineForbidden_TuningAndToggle(t *testing.T) {
	t.Parallel()
	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"slackBridge.machinePrincipal.clientId=custom-bridge",
			"vmalert.gapAlerts.slackBridgeMachineForbidden.window=20m",
			"vmalert.gapAlerts.slackBridgeMachineForbidden.threshold=7")...)
	block := alertBlock(t, tuned, "SlackBridgeMachineForbidden")
	if !strings.Contains(block, `{client="custom-bridge"}[20m])) > 7`) {
		t.Errorf("clientId, window and threshold must parameterize the expr, got:\n%s", block)
	}

	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.slackBridgeMachineForbidden.enabled=false")...)
	if strings.Contains(off, "alert: SlackBridgeMachineForbidden") {
		t.Errorf("slackBridgeMachineForbidden.enabled=false must suppress the alert, got:\n%s", off)
	}
	if !strings.Contains(off, "alert: SlackBridgeMachineAuthUnavailable") {
		t.Errorf("disabling it must NOT disable the sibling SlackBridgeMachineAuthUnavailable")
	}
}
