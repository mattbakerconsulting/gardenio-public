package server

// Guard tests for the ka-0000001436 E2E-account exclusion on the account-cost
// charts. The bug: the Grafana chart catalog is a SEPARATE source of truth from
// the beads-viewer native by-account rollup, and it lacked the
// account!~"(?i:e2e-.*|probe-acct-.*)" exclusion the viewer already carries
// (ka-0000005167). These tests pin (a) that every chart reading the account-cost
// gauge always renders the exclusion, even with no request params, and (b) that
// the exclusion literal stays byte-identical to the controller's
// costsRollupE2EAccountExclusion so the Grafana surface can never drift from the
// /costs headline sitting beside it.

import (
	"strings"
	"testing"
)

// accountCostGaugeMetric is the LiteLLM account-cost gauge every by-account /
// fleet-rate chart reads. A chart that references it MUST carry the E2E exclusion.
const accountCostGaugeMetric = "gardenio_account_cost_usd"

// controllerCostsRollupE2EAccountExclusion is a COPY of the controller's
// costsRollupE2EAccountExclusion constant
// (gardenio/controller/cmd/beads-viewer/costs_rollup.go). The two modules do not
// cross-import, so this string is duplicated on purpose and pinned here — if the
// controller's matcher ever changes, this test reds and forces this module's
// accountCostE2EExclusion to be updated in lockstep. The controller side pins the
// reverse direction in costs_rate_test.go.
const controllerCostsRollupE2EAccountExclusion = `account!~"(?i:e2e-.*|probe-acct-.*)"`

// TestAccountCostE2EExclusion_MatchesControllerConstant is the cross-module drift
// guard: kbeads' accountCostE2EExclusion must equal the controller's
// costsRollupE2EAccountExclusion byte-for-byte, so the Grafana chart catalog, the
// viewer's native by-account rollup, and the fleet-rate headline all filter the
// exact same E2E series (ka-0000001436 / ka-0000005167).
func TestAccountCostE2EExclusion_MatchesControllerConstant(t *testing.T) {
	if accountCostE2EExclusion != controllerCostsRollupE2EAccountExclusion {
		t.Errorf("accountCostE2EExclusion drifted from the controller's costsRollupE2EAccountExclusion:\n"+
			"  kbeads     = %q\n  controller = %q\n"+
			"these MUST stay byte-identical (the two modules do not cross-import); "+
			"update both or the Grafana surface diverges from the /costs headline",
			accountCostE2EExclusion, controllerCostsRollupE2EAccountExclusion)
	}
	// The matcher must be case-insensitive to match beadsapi.IsE2EAccountBead
	// (which lowercases before prefix-matching) and hyphen-anchored so a real
	// account merely beginning with "e2e" is not swept in.
	if !strings.Contains(accountCostE2EExclusion, "(?i:") {
		t.Errorf("accountCostE2EExclusion must be case-insensitive ((?i:…)): %q", accountCostE2EExclusion)
	}
	if !strings.Contains(accountCostE2EExclusion, "e2e-.*") || !strings.Contains(accountCostE2EExclusion, "probe-acct-.*") {
		t.Errorf("accountCostE2EExclusion must anchor the e2e-/probe-acct- prefixes: %q", accountCostE2EExclusion)
	}
}

// TestAccountCostCharts_AlwaysExcludeE2E pins the PromQL-side layer: every chart
// (primary or overlay) that reads the account-cost gauge must render the E2E
// exclusion with an EMPTY request selector — the retention-tail e2e series are
// dropped unconditionally, not only when the operator happens to filter. This is
// the analogue of the controller's TestBuildCostsRollupQuery_AccountAlwaysExcludesE2E.
func TestAccountCostCharts_AlwaysExcludeE2E(t *testing.T) {
	// Empty selector — the no-filter default the operator sees on page load.
	ctx := queryContext{Selector: "", Step: "5m", Window: "1d", WindowHours: "24", TopN: "8", Rollup: "account"}
	checked := 0
	for id, def := range chartCatalog() {
		for _, tmpl := range allQueryTemplates(def) {
			if !strings.Contains(tmpl, accountCostGaugeMetric) {
				continue
			}
			rendered, err := renderTemplate(tmpl, ctx)
			if err != nil {
				t.Fatalf("chart %q: render account-cost query: %v", id, err)
			}
			checked++
			if !strings.Contains(rendered, accountCostE2EExclusion) {
				t.Errorf("chart %q reads the account-cost gauge but its rendered query is "+
					"missing the E2E exclusion %q (even with no filter):\n  %s",
					id, accountCostE2EExclusion, rendered)
			}
			// Guard against a real-account false positive: the anchored e2e-/probe-acct-
			// prefixes must not have been widened into a bare "e2e" substring match.
			if strings.Contains(rendered, `"e2e"`) || strings.Contains(rendered, `=~"e2e"`) {
				t.Errorf("chart %q exclusion looks un-anchored (would match real accounts):\n  %s", id, rendered)
			}
		}
	}
	// Guard against a vacuous pass: the fleet-spend-rate and spend-by-account
	// charts (plus fleet-rate overlays) must actually be present and read the gauge.
	if checked < 3 {
		t.Fatalf("expected at least 3 account-cost queries to check (fleet-spend-rate primary + "+
			"companion overlays + spend-by-account), checked only %d — catalog likely changed shape", checked)
	}
}
