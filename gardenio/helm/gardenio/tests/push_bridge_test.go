package tests

import (
	"strings"
	"testing"
)

// --- pushBridge: BEADS_VIEWER_URL is where an alert's deep link points (Commons on a phone M6) ---

func TestPushBridge_BeadsViewerURL_DefaultNotPresent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/push-bridge/deployment.yaml", "pushBridge.enabled=true")
	if strings.Contains(out, "BEADS_VIEWER_URL") {
		t.Error("BEADS_VIEWER_URL should NOT be present when the chart knows no viewer URL")
	}
}

func TestPushBridge_BeadsViewerURL_FallbackFromIngress(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/push-bridge/deployment.yaml",
		"pushBridge.enabled=true",
		"beadsViewer.ingress.enabled=true",
		"beadsViewer.ingress.host=viewer.example.com",
	)
	if !strings.Contains(out, "- name: BEADS_VIEWER_URL\n              value: \"https://viewer.example.com\"") {
		t.Errorf("BEADS_VIEWER_URL should be the viewer's URL, the same the slack-bridge gets, got:\n%s", out)
	}
}
