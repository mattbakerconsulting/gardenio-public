package tests

import (
	"strings"
	"testing"
)

// gd-WXY7Jm5ugY: an agent fetches a Commons attachment it was nudged with
// (`gb commons file`) from beads-viewer's machine file route, on its own token,
// at the viewer's in-cluster URL. Under default-deny that hop needs an egress
// rule on the agent pods and an ingress rule on the viewer, and the agent pods
// need the URL.

// agentToViewerRule is the agent egress rule that reaches the viewer, without
// the comments around it (the next rule's comment names its selectors).
func agentToViewerRule(egress string) string {
	for r := range strings.SplitSeq(egress, "\n    - to:") {
		if !strings.Contains(r, "app.kubernetes.io/component: beads-viewer") {
			continue
		}
		var kept []string
		for line := range strings.SplitSeq(r, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") {
				kept = append(kept, line)
			}
		}
		return strings.Join(kept, "\n")
	}
	return ""
}

func TestCommonsFiles_AgentEgressReachesOnlyTheViewerPort(t *testing.T) {
	t.Parallel()
	docs := commonsFilesNPDocs(t, "beadsViewer.service.port=9123")
	egress := docs["NetworkPolicy/test-agent-egress"]
	if egress == "" {
		t.Fatal("agent egress NetworkPolicy did not render")
	}
	rule := agentToViewerRule(egress)
	if rule == "" {
		t.Fatalf("agent egress has no rule to beads-viewer:\n%s", egress)
	}
	if !strings.Contains(rule, "port: 9123") || strings.Count(rule, "port:") != 1 {
		t.Errorf("the agents' viewer rule must allow exactly the viewer's http port 9123:\n%s", rule)
	}
	for _, bad := range []string{"namespaceSelector", "ipBlock"} {
		if strings.Contains(rule, bad) {
			t.Errorf("the agents' viewer rule must name the viewer pod only, found %q:\n%s", bad, rule)
		}
	}
}

func TestCommonsFiles_ViewerIngressAdmitsAgents(t *testing.T) {
	t.Parallel()
	docs := commonsFilesNPDocs(t, "beadsViewer.service.port=9123")
	var found string
	for _, r := range ingressRules(docs["NetworkPolicy/test-beads-viewer-ingress"]) {
		if strings.Contains(r, "key: gardenio.io/agent") {
			found = r
		}
	}
	if found == "" {
		t.Fatalf("beads-viewer ingress has no rule from agent pods:\n%s", docs["NetworkPolicy/test-beads-viewer-ingress"])
	}
	if !strings.Contains(found, "port: 9123") || strings.Count(found, "port:") != 1 || strings.Contains(found, "namespaceSelector") {
		t.Errorf("the viewer's agent rule must admit agent pods on the http port 9123 only:\n%s", found)
	}
}

// With no viewer the agents get no rule to it.
func TestCommonsFiles_AgentRuleGoesWithTheViewer(t *testing.T) {
	t.Parallel()
	docs := commonsFilesNPDocs(t, "beadsViewer.enabled=false")
	if rule := agentToViewerRule(docs["NetworkPolicy/test-agent-egress"]); rule != "" {
		t.Errorf("agent egress reaches beads-viewer with the viewer disabled:\n%s", rule)
	}
}

// The controller hands every agent pod the viewer's in-cluster URL, which
// `gb commons file` reads.
func TestCommonsFiles_AgentsGetTheViewerServiceURL(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml", "agents.enabled=true", "beadsViewer.enabled=true", "beadsViewer.service.port=9123")
	if v, ok := envValueAfter(out, "BEADS_VIEWER_INTERNAL_URL"); !ok ||
		!strings.HasPrefix(v, "http://test-beads-viewer.") || !strings.HasSuffix(v, ".svc.cluster.local:9123") {
		t.Errorf("controller BEADS_VIEWER_INTERNAL_URL = %s (set %v), want http://test-beads-viewer.<namespace>.svc.cluster.local:9123", v, ok)
	}
}
