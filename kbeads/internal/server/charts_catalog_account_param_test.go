package server

import (
	"strings"
	"testing"
)

// charts_catalog_account_param_test.go — guards the account-scoped catalog param
// added for the per-bead chart card on type=account beads (ka-0000005064, E7.4
// follow-up). The iOS BeadChartCard maps an account bead to account=<title> and
// gates that filter per-chart against the catalog's declared param schema, so
// these charts MUST declare an `account` param whose Label lands on the
// per-agent real-bill gauge's own `account` label — otherwise the card renders
// the account bead's charts fleet-wide instead of scoped.

// accountScopedProfileCharts are the agent-profile spend charts whose PRIMARY
// series reads gardenio_agent_spend_logs_usd (realBillAgentLevel), which carries
// the {agent,project,role,model,account} label set — so an account= selector
// scopes them honestly. These are exactly the charts the account-bead card
// renders.
var accountScopedProfileCharts = []string{"spend-by-agent", "agent-spend-cumulative"}

// TestAccountScopedSpendCharts_DeclareAccountParam pins that each account-scoped
// profile chart declares an `account` param bound to the VM `account` label, and
// that a supplied account value renders into the query's selector inside the
// per-agent real-bill gauge matcher (not silently dropped).
func TestAccountScopedSpendCharts_DeclareAccountParam(t *testing.T) {
	cat := chartCatalog()
	for _, id := range accountScopedProfileCharts {
		def, ok := cat[id]
		if !ok {
			t.Fatalf("catalog is missing chart %q — the account-bead card renders it (ka-0000005064)", id)
		}
		spec, ok := def.Params["account"]
		if !ok {
			t.Errorf("chart %q must declare an `account` param so the account-bead card's account=<identity> filter is accepted (else it renders fleet-wide)", id)
			continue
		}
		if spec.Label != "account" {
			t.Errorf("chart %q account param Label = %q, want %q (the VM label buildSelector injects)", id, spec.Label, "account")
		}

		// The account value must actually reach the selector, and the primary
		// query must read the per-agent real-bill gauge (the one carrying the
		// account label). A rename of the gauge or a stripped selector would make
		// the account param a silent no-op.
		sel := buildSelector(def, map[string][]string{"account": {"shannon"}})
		if !strings.Contains(sel, `account="shannon"`) {
			t.Errorf("chart %q: buildSelector dropped the account value, got selector %q", id, sel)
		}
		ctx := queryContext{Selector: sel, Step: "5m", Window: "7d", WindowHours: "168", TopN: "10"}
		rendered, err := renderTemplate(def.QueryTemplate, ctx)
		if err != nil {
			t.Fatalf("chart %q: render primary query: %v", id, err)
		}
		if !strings.Contains(rendered, "gardenio_agent_spend_logs_usd") {
			t.Errorf("chart %q: primary query no longer reads gardenio_agent_spend_logs_usd (the account-bearing gauge); account scoping would be a no-op:\n  %s", id, rendered)
		}
		if !strings.Contains(rendered, `account="shannon"`) {
			t.Errorf("chart %q: rendered primary query does not carry the account selector:\n  %s", id, rendered)
		}
	}
}

// TestCostPerBeadCharts_OmitAccountParam documents the deliberate exclusion: the
// cost-per-bead and total-cost charts read the LIFETIME tag gauge
// (realBillTagFlowOr, dim-scoped, no `account` label and no request selector on
// its primary arm) over a fleet bead-close denominator that also has no account
// label. An account= filter there would mis-scope (fleet-wide numerator, only
// the fallback arm scoped), so these charts intentionally do NOT declare it. If
// a future change makes the tag/denominator account-aware, add account here and
// flip this guard.
func TestCostPerBeadCharts_OmitAccountParam(t *testing.T) {
	cat := chartCatalog()
	for _, id := range []string{"agent-cost-per-bead", "total-cost"} {
		def, ok := cat[id]
		if !ok {
			t.Fatalf("catalog is missing chart %q", id)
		}
		if _, ok := def.Params["account"]; ok {
			t.Errorf("chart %q declares an `account` param, but its tag-gauge numerator and bead-close denominator carry no account label — the filter mis-scopes. Remove it, or update this guard if the query became account-aware (ka-0000005064)", id)
		}
	}
}
