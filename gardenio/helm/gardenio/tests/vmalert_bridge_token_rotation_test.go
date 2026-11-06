package tests

import (
	"strings"
	"testing"
)

// BridgeTokenRotationOverdue + BridgeTokenRotatorStale (task ka-0000002353 plan
// item 3) — the fireable "rotation overdue" pack for the controller-side bridge
// fleet-token rotator. Authored as vmalert ConfigMap rule keys (the ONLY path
// that fires on new prod, mirroring the rest of the gap-alerts pack) rather than
// a PrometheusRule CRD. The contract enforced here:
//   - both alerts render by default when a notifier is wired, inside the
//     fireable gap-alerts key (not merely somewhere in the ConfigMap),
//   - both stay clean when the block is toggled off,
//   - the overdue alert keys on the absolute expires_at series (clock-skew
//     immune) with the 7d default threshold,
//   - the stale deadman OR-s in absent() so a never-scraped rotator still pages,
//   - both page and carry the subsystem + runbook annotations.

// rotatorEnabledArgs wires a notifier AND turns the rotator feature on. The
// alert rules gate on BOTH vmalert.gapAlerts.bridgeTokenRotation.enabled (the
// alert flag, default true) AND agents.bridgeTokenRotation.enabled (the rotator
// feature flag, default false). The deadman's absent() clause is designed to
// page when the rotator is meant to be running but never exported its series;
// that premise only holds once the rotator feature is enabled, so the rules
// must not render while it is off (ka-0000003055 — the alert paged prod for a
// deliberately-disabled rotator).
var rotatorEnabledArgs = append(append([]string{}, gapAlertsNotifierArgs...),
	"agents.bridgeTokenRotation.enabled=true")

func TestBridgeTokenRotation_RendersByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", rotatorEnabledArgs...)
	for _, alert := range []string{"BridgeTokenRotationOverdue", "BridgeTokenRotatorStale"} {
		if !strings.Contains(out, "alert: "+alert) {
			t.Fatalf("%s must render by default when a notifier is wired, got:\n%s", alert, out)
		}
		// Must live inside the FIREABLE gap-alerts key, not just anywhere.
		gap := out[strings.Index(out, "gap-alerts.yaml"):]
		if !strings.Contains(gap, "alert: "+alert) {
			t.Errorf("%s must render inside the fireable gap-alerts key, got:\n%s", alert, gap)
		}
	}
}

func TestBridgeTokenRotation_OverdueExprAndThreshold(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", rotatorEnabledArgs...)
	block := alertBlock(t, out, "BridgeTokenRotationOverdue")

	// Keys on the ABSOLUTE expiry series (clock-skew immune), not the
	// convenience remaining_seconds gauge.
	if !strings.Contains(block, "gardenio_bridge_token_expires_at_timestamp_seconds - time()") {
		t.Errorf("overdue expr must read expires_at - time(), got:\n%s", block)
	}
	// Default threshold is 7d (604800s) — inside the rotator's 14d refresh
	// window so it fires only once rotation is genuinely stuck.
	if !strings.Contains(block, "< 604800") {
		t.Errorf("overdue threshold must default to 7d (604800s), got:\n%s", block)
	}
	if !strings.Contains(block, "severity: \"page\"") {
		t.Errorf("overdue must page (fleet-wide write-auth loss), got:\n%s", block)
	}
	if !strings.Contains(block, "subsystem: bridge-token") {
		t.Errorf("overdue must carry subsystem: bridge-token, got:\n%s", block)
	}
	if !strings.Contains(block, "runbook_url:") {
		t.Errorf("overdue must carry a runbook_url annotation, got:\n%s", block)
	}
}

func TestBridgeTokenRotation_StaleDeadmanNotSuppressibleByAbsentSeries(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", rotatorEnabledArgs...)
	block := alertBlock(t, out, "BridgeTokenRotatorStale")

	// The last-check heartbeat, stamped once per successful rotator check tick.
	if !strings.Contains(block, "time() - gardenio_bridge_token_rotator_last_check_timestamp_seconds") {
		t.Errorf("stale expr must fire when last_check stops advancing, got:\n%s", block)
	}
	// absent() MUST be OR-ed in so a never-scraped / crash-before-first-check /
	// pod-gone rotator still pages — a bare time()-series over a never-written
	// series is no-data, not a large number, and would leave the deadman
	// un-fireable in exactly the total-outage case it exists for.
	if !strings.Contains(block, "absent(gardenio_bridge_token_rotator_last_check_timestamp_seconds)") {
		t.Errorf("stale expr must OR-in absent() so a dark rotator still pages, got:\n%s", block)
	}
	if !strings.Contains(block, "severity: \"page\"") {
		t.Errorf("stale deadman must page, got:\n%s", block)
	}
}

func TestBridgeTokenRotation_TogglesOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(append([]string{}, rotatorEnabledArgs...), "vmalert.gapAlerts.bridgeTokenRotation.enabled=false")...)
	for _, alert := range []string{"BridgeTokenRotationOverdue", "BridgeTokenRotatorStale"} {
		if strings.Contains(out, "alert: "+alert) {
			t.Errorf("%s must NOT render when bridgeTokenRotation.enabled=false, got:\n%s", alert, out)
		}
	}
}

// TestBridgeTokenRotation_SuppressedWhileRotatorDisabled guards the false-page
// fix (ka-0000003055): even with the alert flag on and a notifier wired, the
// rotator rules must NOT render while the rotator feature itself is off (the
// chart default, and prod's steady state during the staged rollout
// ka-0000005123). Otherwise the deadman's absent() clause pages continuously
// for a series that will never exist because the feature is deliberately not
// running — precisely the prod false page this fix removes.
func TestBridgeTokenRotation_SuppressedWhileRotatorDisabled(t *testing.T) {
	t.Parallel()
	// gapAlertsNotifierArgs leaves agents.bridgeTokenRotation.enabled at its
	// false default: alert flag on, rotator feature off.
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	for _, alert := range []string{"BridgeTokenRotationOverdue", "BridgeTokenRotatorStale"} {
		if strings.Contains(out, "alert: "+alert) {
			t.Errorf("%s must NOT render while agents.bridgeTokenRotation.enabled=false (would false-page on absent series), got:\n%s", alert, out)
		}
	}
}
