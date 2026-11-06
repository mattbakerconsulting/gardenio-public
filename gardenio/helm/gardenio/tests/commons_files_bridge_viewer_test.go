package tests

import (
	"strings"
	"testing"
)

// gd-qnF6mJZtkQ: the slack-bridge's Commons mirror fetches a message's
// attachments from beads-viewer's machine file route and uploads them into
// Slack. Under default-deny that one hop needs an egress rule on the bridge
// and an ingress rule on the viewer, and the bridge needs the viewer's
// in-cluster URL.

const commonsFilesNPTemplate = "templates/networkpolicies/networkpolicies.yaml"

func commonsFilesNPDocs(t *testing.T, extra ...string) map[string]string {
	t.Helper()
	return splitDocs(helmTemplate(t, commonsFilesNPTemplate, append([]string{
		"networkPolicies.enabled=true",
		"slackBridge.enabled=true",
		"beadsViewer.enabled=true",
	}, extra...)...))
}

// bridgeToViewerRule is the slack-bridge egress rule that reaches the viewer.
func bridgeToViewerRule(egress string) string {
	for r := range strings.SplitSeq(egress, "\n    - to:") {
		if strings.Contains(r, "app.kubernetes.io/component: beads-viewer") {
			return r
		}
	}
	return ""
}

func TestCommonsFiles_BridgeEgressReachesOnlyTheViewerPort(t *testing.T) {
	t.Parallel()
	docs := commonsFilesNPDocs(t, "beadsViewer.service.port=9123")
	egress := docs["NetworkPolicy/test-slack-bridge-egress"]
	if egress == "" {
		t.Fatal("slack-bridge egress NetworkPolicy did not render")
	}
	rule := bridgeToViewerRule(egress)
	if rule == "" {
		t.Fatalf("slack-bridge egress has no rule to beads-viewer:\n%s", egress)
	}
	if !strings.Contains(rule, "port: 9123") || strings.Count(rule, "port:") != 1 {
		t.Errorf("the bridge's viewer rule must allow exactly the viewer's http port 9123:\n%s", rule)
	}
	for _, bad := range []string{"namespaceSelector", "ipBlock"} {
		if strings.Contains(rule, bad) {
			t.Errorf("the bridge's viewer rule must name the viewer pod only, found %q:\n%s", bad, rule)
		}
	}
	// The private ranges stay excluded from the internet rule.
	if !strings.Contains(egress, "- 10.0.0.0/8") {
		t.Errorf("the bridge's internet rule must still exclude private ranges:\n%s", egress)
	}
}

func TestCommonsFiles_ViewerIngressAdmitsTheBridge(t *testing.T) {
	t.Parallel()
	docs := commonsFilesNPDocs(t, "beadsViewer.service.port=9123")
	ingress := docs["NetworkPolicy/test-beads-viewer-ingress"]
	var found string
	for _, r := range ingressRules(ingress) {
		if strings.Contains(r, "app.kubernetes.io/component: slack-bridge") {
			found = r
		}
	}
	if found == "" {
		t.Fatalf("beads-viewer ingress has no rule from slack-bridge:\n%s", ingress)
	}
	if !strings.Contains(found, "port: 9123") || strings.Contains(found, "namespaceSelector") {
		t.Errorf("the viewer's slack-bridge rule must admit the bridge pod on the http port 9123 only:\n%s", found)
	}
}

// With no slack-bridge (or no viewer) neither rule renders.
func TestCommonsFiles_RulesGoWithTheirComponents(t *testing.T) {
	t.Parallel()
	docs := commonsFilesNPDocs(t, "slackBridge.enabled=false")
	for _, r := range ingressRules(docs["NetworkPolicy/test-beads-viewer-ingress"]) {
		if strings.Contains(r, "slack-bridge") {
			t.Errorf("viewer ingress admits slack-bridge with the bridge disabled:\n%s", r)
		}
	}
	docs = commonsFilesNPDocs(t, "beadsViewer.enabled=false")
	if rule := bridgeToViewerRule(docs["NetworkPolicy/test-slack-bridge-egress"]); rule != "" {
		t.Errorf("bridge egress reaches beads-viewer with the viewer disabled:\n%s", rule)
	}
}

func TestCommonsFiles_BridgeGetsTheViewerServiceURL(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true", "beadsViewer.enabled=true", "beadsViewer.service.port=9123")
	// The release namespace is whatever helm template defaults to (the
	// kube context's), so only its shape is pinned.
	if v, ok := envValueAfter(out, "BEADS_VIEWER_INTERNAL_URL"); !ok ||
		!strings.HasPrefix(v, "http://test-beads-viewer.") || !strings.HasSuffix(v, ".svc:9123") || strings.Count(v, ".") != 2 {
		t.Errorf("BEADS_VIEWER_INTERNAL_URL = %s (set %v), want http://test-beads-viewer.<namespace>.svc:9123", v, ok)
	}
	out = helmTemplate(t, "templates/slack-bridge/deployment.yaml", "slackBridge.enabled=true", "beadsViewer.enabled=false")
	if strings.Contains(out, "BEADS_VIEWER_INTERNAL_URL") {
		t.Error("BEADS_VIEWER_INTERNAL_URL renders with the viewer disabled")
	}
}
