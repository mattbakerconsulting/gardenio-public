package tests

import (
	"strings"
	"testing"
)

// vmalert recording-rule evaluator (E8, epic ka-0000006536).
//
// vmalert evaluates the two demonstrably-hot recording rules against the
// in-chart vmsingle and remote-writes the recorded series back. Disabled by
// default; enabled alongside vmsingle + vmagent in prod deploy-values. The
// contract enforced here:
//   - default-disabled (no manifests render),
//   - when enabled, Deployment + ConfigMap + Service render,
//   - the two charter-named recording rules are present,
//   - datasource + remoteWrite auto-wire to the in-chart vmsingle,
//   - no notifier flag leaks in (rule file is recording-only),
//   - the vmsingle ingress + vmalert egress NetworkPolicies open the
//     vmalert→vmsingle path.

func TestVMAlert_DefaultDisabled(t *testing.T) {
	t.Parallel()
	// With vmalert.enabled=false (the default) the deployment template
	// renders nothing. helm exits non-zero with "could not find template"
	// when --show-only points at an all-empty template; that's the signal.
	out, err := helmTemplateAllowFail(t, "templates/vmalert/deployment.yaml")
	if err == nil {
		if strings.Contains(out, "kind: Deployment") {
			t.Errorf("vmalert Deployment must NOT render when vmalert.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(err.Error(), "could not find template") &&
		!strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestVMAlert_EnabledRendersWorkload(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/deployment.yaml",
		"vmalert.enabled=true",
		"vmsingle.enabled=true",
	)
	if !strings.Contains(out, "kind: Deployment") {
		t.Fatalf("vmalert Deployment must render when enabled, got:\n%s", out)
	}
	// Recording rules require a remote-write target or evaluated series are
	// discarded — both query + write must auto-wire to the in-chart vmsingle.
	if !strings.Contains(out, "-datasource.url=http://test-vmsingle:8428") {
		t.Errorf("datasource.url must auto-wire to in-chart vmsingle, got:\n%s", out)
	}
	if !strings.Contains(out, "-remoteWrite.url=http://test-vmsingle:8428") {
		t.Errorf("remoteWrite.url must auto-wire to in-chart vmsingle, got:\n%s", out)
	}
	// Recording-rules-only config: vmalert must NOT carry a notifier flag
	// (it would demand an Alertmanager that v1 does not deploy).
	if strings.Contains(out, "-notifier.") {
		t.Errorf("recording-only vmalert must not set any -notifier flag, got:\n%s", out)
	}
	// Hardened runtime parity with vmagent/vmsingle.
	if !strings.Contains(out, "readOnlyRootFilesystem: true") {
		t.Errorf("vmalert must run with readOnlyRootFilesystem, got:\n%s", out)
	}
	if !strings.Contains(out, "runAsNonRoot: true") {
		t.Errorf("vmalert must run as non-root, got:\n%s", out)
	}
}

func TestVMAlert_RecordingRulesPresent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		"vmalert.enabled=true",
		"vmsingle.enabled=true",
	)
	if !strings.Contains(out, "kind: ConfigMap") {
		t.Fatalf("vmalert rules ConfigMap must render when enabled, got:\n%s", out)
	}
	// The two charter-named hot rules (timeseries-platform.md §2.E8).
	if !strings.Contains(out, "record: gardenio:agent_cost_usd:rate1h") {
		t.Errorf("agent_cost_usd:rate1h recording rule missing, got:\n%s", out)
	}
	if !strings.Contains(out, "record: gardenio:beads_events:rate5m") {
		t.Errorf("beads_events:rate5m recording rule missing, got:\n%s", out)
	}
	// Each rule must target the real E3/E4 source counters.
	if !strings.Contains(out, "gardenio_agent_spend_usd_total") {
		t.Errorf("rate1h rule must aggregate the E3 spend counter, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_beads_events_total") {
		t.Errorf("rate5m rule must aggregate the E4 beads-events counter, got:\n%s", out)
	}
}

func TestVMAlert_EvaluationIntervalConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/deployment.yaml",
		"vmalert.enabled=true",
		"vmsingle.enabled=true",
		"vmalert.evaluationInterval=30s",
	)
	if !strings.Contains(out, "-evaluationInterval=30s") {
		t.Errorf("evaluationInterval override (30s) not applied, got:\n%s", out)
	}
}

func TestVMAlert_NetworkPolicyOpensVMSinglePath(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"vmalert.enabled=true",
		"vmsingle.enabled=true",
		"networkPolicies.enabled=true",
	)
	if !strings.Contains(out, "test-vmalert-egress") {
		t.Errorf("vmalert egress NetworkPolicy must render when enabled, got:\n%s", out)
	}
	// The vmsingle ingress policy must explicitly admit vmalert (read+write).
	if !strings.Contains(out, "vmalert (rule evaluation queries") {
		t.Errorf("vmsingle ingress must admit vmalert, got:\n%s", out)
	}
}

// vmalert globs its rules dir and auto-reloads, so any extra key written into
// the rules ConfigMap is picked up alongside the chart's recording.yaml
// without a pod restart. (Moved from the deleted alertmanager_bridge_test.go,
// gd-Y4ZCGr2m2t.)
func TestVMAlert_RulesGlobAndAutoReload(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/deployment.yaml",
		"vmalert.enabled=true",
		"vmsingle.enabled=true",
	)
	if !strings.Contains(out, "-rule=/config/*.yaml") {
		t.Errorf("vmalert must glob its rules dir so every rules key loads, got:\n%s", out)
	}
	// Auto-reload so runtime rule changes take effect without a pod restart.
	if !strings.Contains(out, "-configCheckInterval=") {
		t.Errorf("vmalert must set -configCheckInterval for runtime rule reload, got:\n%s", out)
	}
}

func TestVMAlert_NotifierURLConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/deployment.yaml",
		"vmalert.enabled=true",
		"vmsingle.enabled=true",
		"vmalert.notifierURL=http://alertmanager.monitoring.svc:9093",
	)
	if !strings.Contains(out, "-notifier.url=http://alertmanager.monitoring.svc:9093") {
		t.Errorf("vmalert.notifierURL override must render the -notifier.url flag, got:\n%s", out)
	}
}
