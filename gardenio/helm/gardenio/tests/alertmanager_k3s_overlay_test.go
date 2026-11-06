package tests

import (
	"strings"
	"testing"
)

// k3s alert delivery (decision gd-gMjUuvb3DT option slack, task gd-YmPhRPJIfp).
// Until 2026-10-25 the k3s overlay ran vmalert with notifierBlackhole and no
// Alertmanager, so every gap alert evaluated and reached nobody. The contract
// locked here: the overlay renders one Alertmanager, vmalert sends to it (not
// the blackhole), delivery is the Slack incoming webhook read from the
// hand-seeded Secret (no ESO on k3s, and never through the slack-bridge pod),
// and BridgeAbsent no longer fires forever for bridges k3s does not run.

func TestAlertmanager_K3sOverlay_VMAlertSendsToAlertmanager(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/vmalert/deployment.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "-notifier.url=http://test-alertmanager:9093") {
		t.Errorf("k3s vmalert must send to the in-chart Alertmanager, got:\n%s", out)
	}
	if strings.Contains(out, "-notifier.blackhole=true") {
		t.Errorf("k3s vmalert must not blackhole its alerts any more, got:\n%s", out)
	}
}

func TestAlertmanager_K3sOverlay_SingleReplicaSlackWebhook(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/alertmanager/deployment.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "kind: Deployment") {
		t.Fatalf("k3s overlay must render the Alertmanager Deployment, got:\n%s", out)
	}
	// One small Alertmanager, no HA pair: no gossip mesh is wired, so a second
	// replica would double every Slack message.
	if !strings.Contains(out, "replicas: 1") {
		t.Errorf("k3s Alertmanager must run exactly one replica, got:\n%s", out)
	}
	// The webhook comes from the hand-seeded Secret, and the volume is
	// optional so a not-yet-seeded Secret does not crashloop the pod.
	if !strings.Contains(out, "secretName: alertmanager-slack-webhook") {
		t.Errorf("k3s Alertmanager must mount the alertmanager-slack-webhook Secret, got:\n%s", out)
	}
	if !strings.Contains(out, "optional: true") {
		t.Errorf("the webhook Secret volume must stay optional, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio.io/lifecycle") {
		t.Errorf("k3s Alertmanager must prefer the on-demand pair like vmalert, got:\n%s", out)
	}
}

func TestAlertmanager_K3sOverlay_NoExternalSecret(t *testing.T) {
	t.Parallel()
	// k3s has no External Secrets Operator. An ExternalSecret here would be an
	// unknown kind and fail the helm upgrade.
	out := helmTemplateShowOnlyOrEmpty(t,
		"templates/alertmanager-slack-webhook-external-secret.yaml", []string{k3sOverlay})
	if strings.Contains(out, "kind: ExternalSecret") {
		t.Errorf("k3s overlay must not render an ExternalSecret, got:\n%s", out)
	}
}

func TestAlertmanager_K3sOverlay_DeliversByWebhookNotBridge(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/alertmanager/configmap.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "slack_api_url_file: /etc/alertmanager/slack/url") {
		t.Errorf("delivery must read the Slack incoming webhook from the mounted Secret, got:\n%s", out)
	}
	// A dead slack-bridge must still page (gd-rGmdUS0cxw), so no receiver
	// may point at the bridge.
	if strings.Contains(out, "slack-bridge") {
		t.Errorf("no Alertmanager receiver may route through the slack-bridge pod, got:\n%s", out)
	}
	// The Watchdog route is the delivery test after a roll.
	if !strings.Contains(out, "receiver: slack-watchdog") {
		t.Errorf("the Watchdog route must render, got:\n%s", out)
	}
}

func TestAlertmanager_K3sOverlay_BridgeAbsentOnlyForRunningBridges(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/vmalert/configmap.yaml", []string{k3sOverlay})
	if !strings.Contains(out, `absent(up{component="slack-bridge"})`) {
		t.Errorf("BridgeAbsent must still watch slack-bridge on k3s, got:\n%s", out)
	}
	for _, b := range []string{"jira-bridge", "gitlab-bridge"} {
		if strings.Contains(out, `absent(up{component="`+b+`"})`) {
			t.Errorf("BridgeAbsent must not watch %s, which k3s does not run (fires forever), got:\n%s", b, out)
		}
	}
}

func TestGapAlerts_BridgeAbsentFollowsEnabledBridges(t *testing.T) {
	t.Parallel()
	none := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if strings.Contains(none, "alert: BridgeAbsent") {
		t.Errorf("with no bridge enabled BridgeAbsent must not render, got:\n%s", none)
	}
	all := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(append([]string{}, gapAlertsNotifierArgs...),
			"slackBridge.enabled=true", "jiraBridge.enabled=true", "gitlabBridge.enabled=true")...)
	want := `absent(up{component="slack-bridge"})
              or absent(up{component="jira-bridge"})
              or absent(up{component="gitlab-bridge"})`
	if !strings.Contains(all, want) {
		t.Errorf("BridgeAbsent must chain every enabled bridge with or, want:\n%s\ngot:\n%s", want, all)
	}
}
