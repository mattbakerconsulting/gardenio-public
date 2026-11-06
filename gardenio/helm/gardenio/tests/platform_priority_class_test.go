package tests

import (
	"strings"
	"testing"
)

// Regression guards for gd-cTobdw0kAL: on 2026-09-23 a thread pod preempted
// gardenio-trigger-engine, and litellm and the controller were exposed to the
// same eviction. The platform services agents depend on carry the top tier;
// interactive agent pods now sit below infra (gd-2qjxTVMBkC,
// priority_class_coverage_test.go).

// Each platform template, keyed to the value that enables it (all are off in
// the chart defaults).
var platformPriorityTemplates = map[string]string{
	"templates/controller/deployment.yaml":     "agents.enabled=true",
	"templates/litellm/deployment.yaml":        "litellm.enabled=true",
	"templates/coopmux/deployment.yaml":        "coopmux.enabled=true",
	"templates/trigger-engine/deployment.yaml": "triggerEngine.enabled=true",
	"templates/minio/deployment.yaml":          "objectStore.enabled=true",
}

// podPriorityClass matches the pod-spec field only, not prose in comments.
const podPriorityClass = "\n      priorityClassName: "

func TestPlatformPriorityClass_SetWhenEnabled(t *testing.T) {
	t.Parallel()
	for tmpl, enable := range platformPriorityTemplates {
		t.Run(tmpl, func(t *testing.T) {
			out := helmTemplate(t, tmpl, enable, "global.systemPriorityClass.create=true")
			if !strings.Contains(out, podPriorityClass+"gardenio-system-critical\n") {
				t.Errorf("expected priorityClassName gardenio-system-critical; got:\n%s", out)
			}
		})
	}
}

func TestPlatformPriorityClass_AbsentByDefault(t *testing.T) {
	t.Parallel()
	for tmpl, enable := range platformPriorityTemplates {
		t.Run(tmpl, func(t *testing.T) {
			out := helmTemplate(t, tmpl, enable)
			if strings.Contains(out, podPriorityClass) {
				t.Errorf("expected no priorityClassName with the class disabled; got:\n%s", out)
			}
		})
	}
}
