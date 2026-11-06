package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The alert → Slack delivery path (epic ka-0000000770, decision ka-0000006149
// option (a)): a genuine Prometheus Alertmanager receives firing alerts from
// vmalert (-notifier.url) and posts them to Slack via an incoming webhook whose
// URL is sourced from AWS SM via ESO and mounted as a file. These tests lock
// the wiring the operator relies on.

func TestAlertmanager_DisabledByDefault(t *testing.T) {
	t.Parallel()
	// With alertmanager.enabled unset, none of the alertmanager objects render.
	for _, tmpl := range []string{
		"templates/alertmanager/deployment.yaml",
		"templates/alertmanager/service.yaml",
		"templates/alertmanager/configmap.yaml",
	} {
		out, err := helmTemplateAllowFail(t, tmpl)
		// --show-only on a template that renders nothing errors with
		// "could not find template" — that is the expected "not rendered".
		if err == nil && strings.Contains(out, "kind:") {
			t.Errorf("%s must NOT render when alertmanager.enabled is unset, got:\n%s", tmpl, out)
		}
	}
}

func TestAlertmanager_EnabledRendersDeploymentAndService(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/alertmanager/deployment.yaml",
		"alertmanager.enabled=true",
	)
	if !strings.Contains(out, "kind: Deployment") {
		t.Fatalf("alertmanager Deployment must render when enabled, got:\n%s", out)
	}
	// Config file + writable storage path (readOnlyRootFilesystem needs the
	// emptyDir/PVC mount for nflog+silences).
	for _, want := range []string{
		"--config.file=/etc/alertmanager/config/alertmanager.yml",
		"--storage.path=/alertmanager",
		"readOnlyRootFilesystem: true",
		"mountPath: /etc/alertmanager/slack",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("alertmanager deployment missing %q, got:\n%s", want, out)
		}
	}
	// The Slack webhook Secret is mounted optional so the pod starts (not-ready)
	// before the operator seeds the credential — never a config-error crashloop.
	if !strings.Contains(out, "optional: true") {
		t.Errorf("slack-webhook secret volume must be optional, got:\n%s", out)
	}

	svc := helmTemplate(t, "templates/alertmanager/service.yaml",
		"alertmanager.enabled=true",
	)
	if !strings.Contains(svc, "port: 9093") {
		t.Errorf("alertmanager Service must expose 9093, got:\n%s", svc)
	}
}

func TestAlertmanager_ConfigHasSeverityRouting(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
	)
	// Severity-based routing contract: default quiet receiver + a page route
	// to a loud receiver.
	for _, want := range []string{
		"receiver: slack-warn",
		"receiver: slack-page",
		`severity="page"`,
		// Anti-storm grouping defaults.
		`group_wait: "30s"`,
		`group_interval: "5m"`,
		`repeat_interval: "4h"`,
		// The page route re-notifies faster than the default.
		`repeat_interval: "1h"`,
		// The webhook URL is read from a mounted file, never inlined.
		"slack_api_url_file: /etc/alertmanager/slack/url",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("alertmanager config missing %q, got:\n%s", want, out)
		}
	}
	// The page receiver prepends the on-call mention.
	if !strings.Contains(out, "<!here>") {
		t.Errorf("slack-page receiver must prepend the page mention, got:\n%s", out)
	}
	// The raw webhook URL must NOT appear in the ConfigMap (defense-in-depth:
	// the credential only ever lives in the mounted Secret).
	if strings.Contains(out, "hooks.slack.com") {
		t.Errorf("webhook URL must never be inlined in the config ConfigMap, got:\n%s", out)
	}
}

// TestAlertmanager_CostRoute covers the P1b cost route (ka-0000003023): OFF by
// default, and when enabled it adds a terminal subsystem="cost" route to a
// dedicated slack-cost receiver targeting #aws-cost-management.
func TestAlertmanager_CostRoute(t *testing.T) {
	t.Parallel()
	// OFF by default: no cost route or receiver.
	base := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
	)
	if strings.Contains(base, "receiver: slack-cost") || strings.Contains(base, `subsystem="cost"`) {
		t.Fatalf("cost route must be OFF by default, got:\n%s", base)
	}

	// Enabled: route + receiver render, targeting the cost channel.
	out := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
		"alertmanager.costRoute.enabled=true",
	)
	for _, want := range []string{
		`subsystem="cost"`,
		"receiver: slack-cost",
		"- name: slack-cost",
		`channel: "#aws-cost-management"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cost route missing %q, got:\n%s", want, out)
		}
	}

	// With a dedicated cost webhook Secret, the receiver reads it via
	// api_url_file and the deployment mounts it.
	cfg := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
		"alertmanager.costRoute.enabled=true",
		"alertmanager.costRoute.slack.secretName=gardenio-cost-webhook",
	)
	if !strings.Contains(cfg, "api_url_file: /etc/alertmanager/slack-cost/url") {
		t.Errorf("slack-cost receiver must read the dedicated webhook via api_url_file, got:\n%s", cfg)
	}
	dep := helmTemplate(t, "templates/alertmanager/deployment.yaml",
		"alertmanager.enabled=true",
		"alertmanager.costRoute.enabled=true",
		"alertmanager.costRoute.slack.secretName=gardenio-cost-webhook",
	)
	if !strings.Contains(dep, "mountPath: /etc/alertmanager/slack-cost") {
		t.Errorf("alertmanager deployment must mount the cost webhook, got:\n%s", dep)
	}
	if !strings.Contains(dep, "secretName: gardenio-cost-webhook") {
		t.Errorf("cost webhook volume must reference the configured Secret, got:\n%s", dep)
	}
}

func TestAlertmanager_VMAlertNotifierAutoWires(t *testing.T) {
	t.Parallel()
	// When alertmanager.enabled, vmalert auto-wires -notifier.url to the
	// in-chart Alertmanager Service.
	out := helmTemplate(t, "templates/vmalert/deployment.yaml",
		"vmalert.enabled=true",
		"vmsingle.enabled=true",
		"alertmanager.enabled=true",
	)
	if !strings.Contains(out, "-notifier.url=http://test-alertmanager:9093") {
		t.Errorf("vmalert must auto-wire -notifier.url to the in-chart alertmanager, got:\n%s", out)
	}
	if strings.Contains(out, "-notifier.blackhole=true") {
		t.Errorf("blackhole fallback must NOT be set when a notifier is wired, got:\n%s", out)
	}
}

func TestAlertmanager_ExplicitNotifierURLWins(t *testing.T) {
	t.Parallel()
	// An explicit vmalert.notifierURL always overrides the auto-wired
	// Alertmanager Service (operator escape hatch, e.g. an external AM).
	out := helmTemplate(t, "templates/vmalert/deployment.yaml",
		"vmalert.enabled=true",
		"vmsingle.enabled=true",
		"alertmanager.enabled=true",
		"vmalert.notifierURL=http://external-am.monitoring.svc:9093",
	)
	if !strings.Contains(out, "-notifier.url=http://external-am.monitoring.svc:9093") {
		t.Errorf("explicit vmalert.notifierURL must win over auto-wire, got:\n%s", out)
	}
	if strings.Contains(out, "test-alertmanager:9093") {
		t.Errorf("auto-wire must not fire when notifierURL is explicit, got:\n%s", out)
	}
}

func TestAlertmanager_SlackWebhookExternalSecret(t *testing.T) {
	t.Parallel()
	// ESO gated on alertmanager.enabled + externalSecrets.enabled + remoteRef.
	out := helmTemplate(t, "templates/alertmanager-slack-webhook-external-secret.yaml",
		"alertmanager.enabled=true",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secretsmanager",
		"externalSecrets.alertmanagerSlackWebhook.remoteRef=gardenio/alertmanager-slack-webhook",
	)
	if !strings.Contains(out, "kind: ExternalSecret") {
		t.Fatalf("ExternalSecret must render when remoteRef is set, got:\n%s", out)
	}
	for _, want := range []string{
		`key: "gardenio/alertmanager-slack-webhook"`,
		"name: alertmanager-slack-webhook",
		"secretKey: url",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("slack-webhook ExternalSecret missing %q, got:\n%s", want, out)
		}
	}
}

func TestAlertmanager_SlackWebhookESONotRenderedWithoutRemoteRef(t *testing.T) {
	t.Parallel()
	// Break-glass path: no remoteRef → no ExternalSecret (operator seeds the
	// K8s Secret out-of-band), but the Deployment still renders.
	out, err := helmTemplateAllowFail(t, "templates/alertmanager-slack-webhook-external-secret.yaml",
		"alertmanager.enabled=true",
		"externalSecrets.enabled=true",
	)
	if err == nil && strings.Contains(out, "kind: ExternalSecret") {
		t.Errorf("ExternalSecret must NOT render without a remoteRef, got:\n%s", out)
	}
}

func TestAlertmanager_ChannelOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	// Empty channel must OMIT the channel field (not fall back to a default) so
	// an operator can rely on the incoming webhook's locked channel.
	out := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
		"alertmanager.channel=",
	)
	if strings.Contains(out, "channel:") {
		t.Errorf("empty channel must omit the channel field, got:\n%s", out)
	}
	// Non-empty channel renders it.
	out2 := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
		"alertmanager.channel=#custom-alerts",
	)
	if !strings.Contains(out2, "channel: \"#custom-alerts\"") {
		t.Errorf("non-empty channel must render, got:\n%s", out2)
	}
}

// --- Security-channel route (ka-0000002447, epic ka-0000004743) ---
//
// The EKS public-API intrusion-detection pack (subsystem="intrusion-detection":
// detections #1/#2/#4, the #3 cross-check, the Phase-2 LogsQL detections, and
// the dead-man's-switch pair) routes to a DEDICATED security channel, isolated
// from the #gardenio-alerts ops firehose. A Slack incoming webhook is
// channel-locked, so real separate-channel delivery needs its own webhook; the
// route fails safe to the global webhook + a channel override when none is wired.

func TestAlertmanager_SecurityRoute_DisabledByDefault(t *testing.T) {
	t.Parallel()
	// With securityRoute.enabled unset (the default), no security route/receiver
	// renders — the intrusion pack routes by severity into the shared receivers
	// exactly as before, so enabling the pack does not silently change delivery.
	out := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
	)
	for _, gone := range []string{
		"slack-security-warn",
		"slack-security-page",
		`subsystem="intrusion-detection"`,
	} {
		if strings.Contains(out, gone) {
			t.Errorf("security route/receiver %q must NOT render when securityRoute.enabled is unset, got:\n%s", gone, out)
		}
	}
}

func TestAlertmanager_SecurityRoute_RoutesIntrusionSubsystem(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
		"alertmanager.securityRoute.enabled=true",
	)
	// The intrusion-detection subsystem routes to the security receiver, with a
	// nested severity=page child to the loud security receiver.
	for _, want := range []string{
		`subsystem="intrusion-detection"`,
		"receiver: slack-security-warn",
		"receiver: slack-security-page",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("security route missing %q, got:\n%s", want, out)
		}
	}
	// The subsystem route must be placed BEFORE the general severity=page route,
	// so a page-severity intrusion alert lands in the security channel (via the
	// nested child), not in the general slack-page receiver. Alertmanager routing
	// is first-match on sibling routes, so order is load-bearing.
	subIdx := strings.Index(out, `subsystem="intrusion-detection"`)
	pageRouteIdx := strings.Index(out, "receiver: slack-page")
	if subIdx < 0 || pageRouteIdx < 0 || subIdx > pageRouteIdx {
		t.Errorf("subsystem=intrusion-detection route must precede the general severity=page route (first-match ordering), subIdx=%d pageRouteIdx=%d, got:\n%s", subIdx, pageRouteIdx, out)
	}
}

func TestAlertmanager_SecurityRoute_FallsBackToGlobalWebhook(t *testing.T) {
	t.Parallel()
	// No dedicated security webhook wired → the receivers deliver via the GLOBAL
	// webhook (no api_url_file override) with a best-effort channel override, so
	// intrusion alerts are never dropped — just delivered to the global webhook's
	// locked channel until the dedicated webhook lands.
	out := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
		"alertmanager.securityRoute.enabled=true",
	)
	if strings.Contains(out, "/etc/alertmanager/slack-security/") {
		t.Errorf("without a dedicated security webhook, the receivers must NOT set api_url_file (fall back to the global webhook), got:\n%s", out)
	}
	// Default security channel override is present.
	if !strings.Contains(out, `channel: "#gardenio-security"`) {
		t.Errorf("security receivers must carry the #gardenio-security channel override in fallback mode, got:\n%s", out)
	}
	// The deployment must NOT mount a security webhook volume when none is wired.
	dep := helmTemplate(t, "templates/alertmanager/deployment.yaml",
		"alertmanager.enabled=true",
		"alertmanager.securityRoute.enabled=true",
	)
	if strings.Contains(dep, "slack-security-webhook") {
		t.Errorf("deployment must NOT mount a security webhook volume when securityRoute.slack.secretName is empty, got:\n%s", dep)
	}
}

func TestAlertmanager_SecurityRoute_DedicatedWebhook(t *testing.T) {
	t.Parallel()
	// A dedicated security webhook wired → the receivers deliver via that webhook
	// (api_url_file) so the intrusion pack reaches a GENUINELY separate channel,
	// independent of the ops webhook's channel-lock.
	out := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
		"alertmanager.securityRoute.enabled=true",
		"alertmanager.securityRoute.slack.secretName=alertmanager-security-slack-webhook",
		"alertmanager.securityRoute.channel=#soc",
		"alertmanager.securityRoute.pageMention=<!subteam^SSEC>",
	)
	if !strings.Contains(out, "api_url_file: /etc/alertmanager/slack-security/url") {
		t.Errorf("security receivers must read the dedicated webhook via api_url_file when wired, got:\n%s", out)
	}
	if !strings.Contains(out, `channel: "#soc"`) {
		t.Errorf("securityRoute.channel override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, "<!subteam^SSEC>") {
		t.Errorf("securityRoute.pageMention override not applied to the page receiver, got:\n%s", out)
	}
	// The raw webhook URL must never be inlined in the ConfigMap.
	if strings.Contains(out, "hooks.slack.com") {
		t.Errorf("security webhook URL must never be inlined in the config ConfigMap, got:\n%s", out)
	}
	// The deployment mounts the dedicated security webhook Secret (optional, so a
	// missing Secret never crashloops the pod).
	dep := helmTemplate(t, "templates/alertmanager/deployment.yaml",
		"alertmanager.enabled=true",
		"alertmanager.securityRoute.enabled=true",
		"alertmanager.securityRoute.slack.secretName=alertmanager-security-slack-webhook",
	)
	for _, want := range []string{
		"name: slack-security-webhook",
		"mountPath: /etc/alertmanager/slack-security",
		"secretName: alertmanager-security-slack-webhook",
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("deployment missing dedicated security webhook wiring %q, got:\n%s", want, dep)
		}
	}
}

func TestAlertmanager_SecurityRoute_PageMentionFallsBackToGlobal(t *testing.T) {
	t.Parallel()
	// An empty securityRoute.pageMention falls back to alertmanager.pageMention so
	// the security page still pings someone — the mention is not silently dropped.
	out := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
		"alertmanager.securityRoute.enabled=true",
		"alertmanager.securityRoute.pageMention=",
		"alertmanager.pageMention=<!here>",
	)
	secIdx := strings.Index(out, "name: slack-security-page")
	if secIdx < 0 {
		t.Fatalf("slack-security-page receiver missing, got:\n%s", out)
	}
	if !strings.Contains(out[secIdx:], "<!here>") {
		t.Errorf("empty securityRoute.pageMention must fall back to alertmanager.pageMention on the page receiver, got:\n%s", out[secIdx:])
	}
}

func TestAlertmanager_SecurityRoute_SlackWebhookExternalSecret(t *testing.T) {
	t.Parallel()
	// ESO gated on alertmanager.enabled + securityRoute.enabled + a non-empty
	// securityRoute.slack.secretName + externalSecrets.enabled + remoteRef.
	out := helmTemplate(t, "templates/alertmanager-security-slack-webhook-external-secret.yaml",
		"alertmanager.enabled=true",
		"alertmanager.securityRoute.enabled=true",
		"alertmanager.securityRoute.slack.secretName=alertmanager-security-slack-webhook",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secretsmanager",
		"externalSecrets.alertmanagerSecuritySlackWebhook.remoteRef=gardenio/alertmanager-security-slack-webhook",
	)
	if !strings.Contains(out, "kind: ExternalSecret") {
		t.Fatalf("security-webhook ExternalSecret must render when remoteRef is set, got:\n%s", out)
	}
	for _, want := range []string{
		`key: "gardenio/alertmanager-security-slack-webhook"`,
		"name: alertmanager-security-slack-webhook",
		"secretKey: url",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("security-webhook ExternalSecret missing %q, got:\n%s", want, out)
		}
	}
}

func TestAlertmanager_SecurityRoute_ESONotRenderedWhenRouteDisabled(t *testing.T) {
	t.Parallel()
	// Even with a remoteRef + secretName set, the security-webhook ExternalSecret
	// must NOT render while the route itself is disabled (no consumer for it).
	out, err := helmTemplateAllowFail(t, "templates/alertmanager-security-slack-webhook-external-secret.yaml",
		"alertmanager.enabled=true",
		"alertmanager.securityRoute.slack.secretName=alertmanager-security-slack-webhook",
		"externalSecrets.enabled=true",
		"externalSecrets.alertmanagerSecuritySlackWebhook.remoteRef=gardenio/alertmanager-security-slack-webhook",
	)
	if err == nil && strings.Contains(out, "kind: ExternalSecret") {
		t.Errorf("security-webhook ExternalSecret must NOT render when securityRoute.enabled is unset, got:\n%s", out)
	}
}

// TestAlertmanager_SecurityRoute_AmtoolRoutesSyntheticIntrusionAlert is the
// routing acceptance proof for ka-0000002447's success criterion ("a synthetic
// anomaly triggers a Slack alert"). The live end-to-end (a metric filter firing
// through vmalert) waits on the in-progress metric-filter bead (ka-0000005580),
// but the ROUTING half — that an intrusion-detection alert actually reaches the
// security receiver rather than the ops firehose — is provable now: render the
// real Alertmanager config and ask amtool which receiver a synthetic
// subsystem="intrusion-detection" alert routes to. Both the page-severity
// (sensitive-verb / dead-man's-switch) and the ticket-severity (#1/#2) cases
// must land on a security receiver, and a non-intrusion page must NOT.
//
// Skips gracefully when helm/yq/amtool are not on PATH — the render tests above
// still lock the wiring without the binary.
func TestAlertmanager_SecurityRoute_AmtoolRoutesSyntheticIntrusionAlert(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"helm", "yq", "amtool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping amtool routing proof", bin)
		}
	}

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "alertmanager.yml")

	// Render the real config (dedicated security webhook wired) and extract the
	// alertmanager.yml key amtool consumes.
	render := exec.Command("bash", "-c",
		"helm template test "+chartDir(t)+" --show-only templates/alertmanager/configmap.yaml "+
			"--set alertmanager.enabled=true "+
			"--set alertmanager.securityRoute.enabled=true "+
			"--set alertmanager.securityRoute.slack.secretName=alertmanager-security-slack-webhook "+
			`| yq eval '.data."alertmanager.yml"' -`)
	out, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("render alertmanager.yml failed: %v\n%s", err, out)
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		t.Fatal(err)
	}

	// amtool config routes test --config.file <cfg> <label=value...> prints the
	// receiver the alert with those labels routes to.
	route := func(labels ...string) string {
		args := append([]string{"config", "routes", "test", "--config.file", cfgPath}, labels...)
		res, err := exec.Command("amtool", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("amtool routes test %v failed: %v\n%s", labels, err, res)
		}
		return strings.TrimSpace(string(res))
	}

	// A page-severity intrusion alert (sensitive-verb / dead-man's-switch) must
	// route to the LOUD security receiver, not the general slack-page firehose.
	if got := route("subsystem=intrusion-detection", "severity=page"); !strings.Contains(got, "slack-security-page") {
		t.Errorf("page-severity intrusion alert must route to slack-security-page, got %q", got)
	}
	// A ticket-severity intrusion alert (#1/#2, cross-check) must route to the
	// quiet security receiver.
	if got := route("subsystem=intrusion-detection", "severity=ticket"); !strings.Contains(got, "slack-security-warn") {
		t.Errorf("ticket-severity intrusion alert must route to slack-security-warn, got %q", got)
	}
	// A non-intrusion page must still route to the general ops page receiver —
	// the security route must not steal unrelated pages.
	if got := route("severity=page"); !strings.Contains(got, "slack-page") || strings.Contains(got, "security") {
		t.Errorf("non-intrusion page must route to slack-page (not a security receiver), got %q", got)
	}
}

func TestAlertmanager_IngressFailsClosedWithoutPeers(t *testing.T) {
	t.Parallel()
	// When alertmanager is enabled but BOTH vmalert and vmagent are disabled,
	// the ingress policy must fail CLOSED (ingress: []), never render a
	// null-`from` allow-all-on-9093 hole.
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"alertmanager.enabled=true",
		"vmalert.enabled=false",
		"vmagent.enabled=false",
	)
	if !strings.Contains(out, "release-name-alertmanager-ingress") &&
		!strings.Contains(out, "test-alertmanager-ingress") {
		t.Fatalf("alertmanager-ingress policy must render, got:\n%s", out)
	}
	if !strings.Contains(out, "ingress: []") {
		t.Errorf("alertmanager-ingress must fail closed (ingress: []) with no peers, got:\n%s", out)
	}
	// With a peer, it renders the port allow.
	out2 := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"networkPolicies.enabled=true",
		"alertmanager.enabled=true",
		"vmalert.enabled=true",
		"vmsingle.enabled=true",
		"vmagent.enabled=false",
	)
	if !strings.Contains(out2, "port: 9093") {
		t.Errorf("alertmanager-ingress must allow :9093 from a peer, got:\n%s", out2)
	}
}

func TestAlertmanager_PDBGatedOnReplicas(t *testing.T) {
	t.Parallel()
	// Single replica: no PDB (a 1-replica AM cannot satisfy a PDB and would
	// wedge node drains).
	out, err := helmTemplateAllowFail(t, "templates/alertmanager/pdb.yaml",
		"alertmanager.enabled=true",
	)
	if err == nil && strings.Contains(out, "kind: PodDisruptionBudget") {
		t.Errorf("PDB must NOT render at replicaCount=1, got:\n%s", out)
	}
	// >1 replica: PDB renders.
	out2 := helmTemplate(t, "templates/alertmanager/pdb.yaml",
		"alertmanager.enabled=true",
		"alertmanager.replicaCount=2",
	)
	if !strings.Contains(out2, "kind: PodDisruptionBudget") {
		t.Errorf("PDB must render at replicaCount>1, got:\n%s", out2)
	}
}

// alertmanager.extraReceivers / extraRoutes render into the config, and the
// extra routes land AFTER the chart's terminal severity="page" child, so a
// page alert never reaches an extra route. (Generic part of the deleted
// alertmanager_bridge_receiver_test.go, gd-Y4ZCGr2m2t.)
func TestAlertmanager_ExtraReceiversAndRoutes(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/alertmanager/configmap.yaml",
		"alertmanager.enabled=true",
		"alertmanager.extraReceivers[0].name=extra-webhook",
		"alertmanager.extraReceivers[0].webhook_configs[0].url=http://example.invalid/hook",
		"alertmanager.extraReceivers[0].webhook_configs[0].send_resolved=true",
		"alertmanager.extraRoutes[0].receiver=extra-webhook",
		`alertmanager.extraRoutes[0].matchers[0]=subsystem="acme-perf"`,
		"alertmanager.extraRoutes[0].continue=true",
	)
	for _, want := range []string{
		"name: extra-webhook",
		"webhook_configs",
		"http://example.invalid/hook",
		"send_resolved: true",
		"continue: true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("extraReceivers/extraRoutes must render %q, got:\n%s", want, out)
		}
	}
	pageIdx := strings.Index(out, "receiver: slack-page")
	extraIdx := strings.Index(out, "receiver: extra-webhook")
	if pageIdx < 0 || extraIdx < 0 {
		t.Fatalf("expected both the page route and the extra route to render; page=%d extra=%d\n%s", pageIdx, extraIdx, out)
	}
	if pageIdx > extraIdx {
		t.Fatalf("the severity=page route is expected BEFORE extraRoutes (it is terminal).\n%s", out)
	}
}
