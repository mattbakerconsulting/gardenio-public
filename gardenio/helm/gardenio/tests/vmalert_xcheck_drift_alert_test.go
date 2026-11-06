package tests

import (
	"strings"
	"testing"
)

// CostXCheckDrift (C5 ka-0000006644, plan ka-0000003747): the cost x-check
// corroboration alert rides the same gated gap-alerts.yaml key as the rest of
// the fireable pack. The contract enforced here:
//   - renders by default (notifier wired) with the default 0.95 floor and the
//     6h sustain, selecting the C2 series gardenio_xcheck_agreement,
//   - the default-floor rule EXCLUDES pairs listed in pairThresholds, and a
//     zero-floor override renders NO extra rule — agreement is clamped to [0,1]
//     so `< 0` would be dead code,
//   - a non-zero pairThresholds override renders its own per-pair rule at the
//     custom floor alongside the default rule,
//   - the per-alert toggle suppresses it without touching the rest of the pack,
//   - the dashboard annotation deep-links the xcheck-trust board (24h window —
//     a 6h-sustained drift is invisible at the pack's default 1h window).

func TestGapAlerts_CostXCheckDriftRendersByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if !strings.Contains(out, "alert: CostXCheckDrift") {
		t.Fatalf("CostXCheckDrift must render by default when a notifier is wired, got:\n%s", out)
	}
	// Default-floor rule: the C2 agreement series, excluding the one shipped
	// per-pair override (sorted alpha in the exclusion regex), below 0.95.
	// (settled-vs-bill was retired in ka-0000000543 and estimate-vs-settled in
	// ka-0000007296, so neither is an override nor appears in the regex — only
	// bill-conservation remains.)
	if !strings.Contains(out, `gardenio_xcheck_agreement{pair!~"bill-conservation"} < 0.95`) {
		t.Errorf("default rule must select gardenio_xcheck_agreement excluding all overridden pairs at the 0.95 floor, got:\n%s", out)
	}
	// The one remaining structural cost pair (ka-0000002467) gets its own rule at
	// a floor tuned below its observed steady-state — bill-conservation (P1, the
	// /key/info-vs-/global/spend topology gap) at 0.85 — so a flat 0.95 no
	// longer fires forever on a healthy fleet.
	if !strings.Contains(out, `gardenio_xcheck_agreement{pair="bill-conservation"} < 0.85`) {
		t.Errorf("bill-conservation must get its own per-pair rule at the 0.85 floor, got:\n%s", out)
	}
	// settled-vs-bill (P3) was RETIRED (ka-0000000543): its side A counter went
	// dead, so no per-pair rule must render for it any longer.
	if strings.Contains(out, `gardenio_xcheck_agreement{pair="settled-vs-bill"}`) {
		t.Errorf("retired settled-vs-bill pair must render NO alert rule, got:\n%s", out)
	}
	// estimate-vs-settled (P7) was RETIRED (ka-0000007296): its side A producer
	// went away, so no per-pair rule must render for it any longer.
	if strings.Contains(out, `gardenio_xcheck_agreement{pair="estimate-vs-settled"}`) {
		t.Errorf("retired estimate-vs-settled pair must render NO alert rule, got:\n%s", out)
	}
	// 6h sustain — ~24 consecutive 15m sweep cycles, not one odd sample.
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	drift := gap[strings.Index(gap, "alert: CostXCheckDrift"):]
	if !strings.Contains(drift, `for: "6h"`) {
		t.Errorf("CostXCheckDrift must sustain 6h before firing, got:\n%s", drift)
	}
	if !strings.Contains(drift, "severity: \"ticket\"") {
		t.Errorf("CostXCheckDrift is a data-trust signal, severity ticket, got:\n%s", drift)
	}
}

func TestGapAlerts_CostXCheckDriftPairOverrideRendersOwnRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"vmalert.gapAlerts.xcheckDrift.pairThresholds.token-parity=0.8",
		)...)
	// The override gets its own rule at the custom floor...
	if !strings.Contains(out, `gardenio_xcheck_agreement{pair="token-parity"} < 0.8`) {
		t.Errorf("non-zero pairThresholds override must render a per-pair rule at its floor, got:\n%s", out)
	}
	// ...and is excluded from the default-floor rule alongside the one shipped
	// override (both sorted alpha in the exclusion regex; settled-vs-bill retired
	// in ka-0000000543 and estimate-vs-settled in ka-0000007296).
	if !strings.Contains(out, `gardenio_xcheck_agreement{pair!~"bill-conservation|token-parity"} < 0.95`) {
		t.Errorf("default rule must exclude ALL overridden pairs, got:\n%s", out)
	}
}

func TestGapAlerts_CostXCheckDriftToggleOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.xcheckDrift.enabled=false")...)
	if strings.Contains(out, "alert: CostXCheckDrift") {
		t.Errorf("xcheckDrift.enabled=false must suppress the alert, got:\n%s", out)
	}
	if !strings.Contains(out, "alert: UnattributedSpendHigh") {
		t.Errorf("disabling xcheckDrift must not disable the rest of the pack, got:\n%s", out)
	}
}

func TestGapAlerts_CostXCheckDriftDashboardLinksXCheckTrustBoard(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	drift := gap[strings.Index(gap, "alert: CostXCheckDrift"):]
	if !strings.Contains(drift, "/d/gardenio-xcheck-trust/") {
		t.Errorf("CostXCheckDrift dashboard link must point at the xcheck-trust board, got:\n%s", drift)
	}
	if !strings.Contains(drift, "from=now-24h&to=now") {
		t.Errorf("CostXCheckDrift dashboard link must open a 24h window (a 6h drift is invisible at 1h), got:\n%s", drift)
	}
}

func TestGapAlerts_CostXCheckDriftCustomDefaultThreshold(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs, "vmalert.gapAlerts.xcheckDrift.threshold=0.99")...)
	if !strings.Contains(out, `gardenio_xcheck_agreement{pair!~"bill-conservation"} < 0.99`) {
		t.Errorf("xcheckDrift.threshold must parameterize the default-floor expr, got:\n%s", out)
	}
}
