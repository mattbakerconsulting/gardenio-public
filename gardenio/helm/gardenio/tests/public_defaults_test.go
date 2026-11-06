package tests

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// docsLinkedAlerts turns on every alert template whose runbook_url is built
// from global.docsBaseURL. None of them render on a default install, so without
// these the docsBaseURL half of the test below would have nothing to check.
var docsLinkedAlerts = []string{
	"agents.metrics.prometheusRule.spendReconciliation.enabled=true",
	"agents.metrics.prometheusRule.tagCostValidation.enabled=true",
	"agents.metrics.prometheusRule.adviceBudget.enabled=true",
	"agents.metrics.prometheusRule.schemaDrift.enabled=true",
	"agents.metrics.prometheusRule.harnessOverhead.enabled=true",
	"agents.metrics.prometheusRule.resumeLatency.enabled=true",
	"agents.metrics.prometheusRule.vpaGuardrails.enabled=true",
	"agents.metrics.prometheusRule.serviceFieldCrossFleet.enabled=true",
	"agents.metrics.prometheusRule.operatedBy.enabled=true",
	"agents.metrics.prometheusRule.restartChurn.enabled=true",
	"safeDeployAlerts.enabled=true",
	"fsmCompositeAlert.enabled=true",
	"slackBridge.metrics.prometheusRule.assistantPane.enabled=true",
	"jiraBridge.metrics.prometheusRule.triggerStarved.enabled=true",
	"jiraBridge.metrics.prometheusRule.webhookDeadman.enabled=true",
	"jiraBridge.metrics.prometheusRule.rateLimitRecovery.enabled=true",
	"jiraBridge.metrics.prometheusRule.pollerStall.enabled=true",
	"gitlabBridge.enabled=true",
	"gitlabBridge.metrics.prometheusRule.webhookDeadman.enabled=true",
	"gitlabBridge.releaseDrought.enabled=true",
	"gitlabBridge.releaseDrought.prometheusRule.enabled=true",
}

// TestChartDefaultsNameNoDeadForgeOrHost renders the chart with its DEFAULT
// values (plus the docs-linked alerts) and asserts no Acme forge path or dead
// host survives into any manifest, and that runbook links use
// global.docsBaseURL. Let's Encrypt's "acme" (acmeServer, acme-challenge) is
// legitimate and excluded.
func TestChartDefaultsNameNoDeadForgeOrHost(t *testing.T) {
	t.Parallel()
	bad := regexp.MustCompile(`gitlab\.com/[Aa]cme|Acme/Platform|registry\.gitlab\.com/acme|\.acme\.example|matthew\.baker@|C0BU22C1U8H`)
	for _, tc := range []struct {
		name string
		sets []string
	}{
		{"defaults", nil},
		{"docs-linked alerts on", docsLinkedAlerts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"template", "t", "..",
				"--set", "global.admissionPolicies.restrictRegistries.enabled=true"}
			for _, s := range tc.sets {
				args = append(args, "--set", s)
			}
			out, err := exec.Command("helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, out)
			}
			for i, line := range strings.Split(string(out), "\n") {
				if bad.MatchString(line) {
					t.Errorf("rendered line %d still names the dead forge/host: %s", i+1, strings.TrimSpace(line))
				}
			}
			if tc.sets != nil && !strings.Contains(string(out), "https://github.com/mattbakerconsulting/gardenio/blob/main/") {
				t.Errorf("expected at least one runbook_url built from global.docsBaseURL")
			}
		})
	}
}
