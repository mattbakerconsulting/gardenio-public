package tests

import (
	"strings"
	"testing"
)

// --- karpenter.doNotDisrupt: consolidation pins on spine pod templates ---
//
// Single-replica spine services take a real outage on every Karpenter
// consolidation eviction (2026-06-12 eviction storm, gd-SRM04602WO). Their
// pod templates carry karpenter.sh/do-not-disrupt unless the kill switch
// karpenter.doNotDisrupt=false is set.

const doNotDisruptAnnotation = `karpenter.sh/do-not-disrupt: "true"`

var pinnedTemplates = []struct {
	name     string
	template string
	setFlags []string
}{
	{"slack-bridge", "templates/slack-bridge/deployment.yaml", []string{
		"slackBridge.enabled=true",
		"slackBridge.slack.secretName=slack-secrets",
	}},
	{"beads", "templates/beads/deployment.yaml", nil},
	{"coopmux", "templates/coopmux/deployment.yaml", []string{
		"coopmux.enabled=true",
	}},
	{"controller", "templates/controller/deployment.yaml", []string{
		"agents.enabled=true",
	}},
	{"trigger-engine", "templates/trigger-engine/deployment.yaml", []string{
		"triggerEngine.enabled=true",
	}},
	{"postgres", "templates/postgres/statefulset.yaml", []string{
		"postgres.enabled=true",
	}},
	{"nats", "templates/nats/statefulset.yaml", []string{
		"nats.enabled=true",
	}},
}

func TestKarpenterPin_DefaultOnAllSpineTemplates(t *testing.T) {
	t.Parallel()
	for _, tc := range pinnedTemplates {
		t.Run(tc.name, func(t *testing.T) {
			out := helmTemplate(t, tc.template, tc.setFlags...)
			if !strings.Contains(out, doNotDisruptAnnotation) {
				t.Errorf("%s should carry %s by default, got:\n%s",
					tc.template, doNotDisruptAnnotation, out)
			}
		})
	}
}

func TestKarpenterPin_KillSwitch(t *testing.T) {
	t.Parallel()
	for _, tc := range pinnedTemplates {
		t.Run(tc.name, func(t *testing.T) {
			out := helmTemplate(t, tc.template,
				append([]string{"karpenter.doNotDisrupt=false"}, tc.setFlags...)...)
			if strings.Contains(out, "do-not-disrupt") {
				t.Errorf("%s should not carry the pin when karpenter.doNotDisrupt=false, got:\n%s",
					tc.template, out)
			}
		})
	}
}
