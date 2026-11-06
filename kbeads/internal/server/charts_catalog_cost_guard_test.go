package server

// Cost-chart guard (T1, epic ka-0000000758 — "LiteLLM-only cost estimation", §6 G5).
//
// The kbeads chart catalog is a READ-ONLY surface: its spend charts must reshape
// a LiteLLM-rooted Prometheus series with pure PromQL aggregation only —
// sum / rate / increase / division / scalar multiply for wall-clock projection.
// A spend chart may NEVER reference a token series or embed a sub-dollar
// per-token price literal; doing so would reintroduce a token×rate cost estimate
// on a surface the design doc (§2.3) declares cost read-only. See
// gardenio/docs/litellm-only-cost-estimation-design.md.
//
// Two LiteLLM-rooted series are permitted (ka-0000004779, ka-0000001663):
//   - gardenio_agent_spend_logs_usd — the per-agent REAL-BILL spend gauge,
//     recomputed statelessly from LiteLLM /spend/logs each sweep (carries
//     {agent,account,model,role,project}); the only per-agent-labelled real-bill
//     source, used by the per-agent charts. It replaced the dead at-close counter
//     gardenio_agent_spend_usd_total, which read ~1000x low (ka-0000001663).
//   - gardenio_account_cost_usd      — the LiteLLM account-cost gauge synced every
//     ~5 min (carries {account,env}); the authoritative fleet bill, used by the
//     fleet-spend-rate chart. Both are LiteLLM-sourced; neither is a token×rate
//     estimate, which is what this guard actually forbids.

import (
	"regexp"
	"strings"
	"testing"
)

// subDollarPriceLiteral matches a bare fractional-dollar literal like 0.000003
// (>=4 leading fractional zeros) — the shape of a per-token price constant. The
// legitimate scalar in the catalog is `* 3600` (seconds/hour), which this does
// not match.
var subDollarPriceLiteral = regexp.MustCompile(`0\.0{3,}[0-9]`)

func TestCostCharts_OnlyReshapeLiteLLMCounter(t *testing.T) {
	catalog := chartCatalog()

	// Every spend-category chart must be rooted in the LiteLLM-sourced counter
	// and must not touch token series or carry a price literal.
	foundSpend := 0
	for id, def := range catalog {
		if def.Category != "spend" {
			continue
		}
		foundSpend++
		q := def.QueryTemplate

		if !strings.Contains(q, "gardenio_agent_spend_logs_usd") &&
			!strings.Contains(q, "gardenio_account_cost_usd") {
			t.Errorf("spend chart %q does not read a LiteLLM-sourced spend series "+
				"(gardenio_agent_spend_logs_usd or gardenio_account_cost_usd); "+
				"cost must come from LiteLLM, not elsewhere:\n  %s", id, q)
		}
		// The dead at-close counter must never come back — it read ~1000x low
		// (ka-0000001663 / ka-0000002699). This is the tripwire that fails if a
		// future edit re-points any spend chart back onto it.
		if strings.Contains(q, "gardenio_agent_spend_usd_total") {
			t.Errorf("spend chart %q reads the DEAD at-close counter "+
				"gardenio_agent_spend_usd_total (~1000x low) — re-point to the real-bill "+
				"gauge gardenio_agent_spend_logs_usd:\n  %s", id, q)
		}
		if strings.Contains(strings.ToLower(q), "token") {
			t.Errorf("spend chart %q references a token series — cost charts must "+
				"reshape spend, never tokens×rate:\n  %s", id, q)
		}
		if loc := subDollarPriceLiteral.FindString(q); loc != "" {
			t.Errorf("spend chart %q embeds a sub-dollar price literal %q — no "+
				"hardcoded per-token price is allowed:\n  %s", id, loc, q)
		}

		// Overlay (ExtraSeries) queries on a spend chart are part of the same
		// read-only surface, so they must also never reintroduce a token×rate
		// estimate. We do NOT require an overlay to read a spend series — a
		// legitimate overlay reshapes a non-spend gauge (the active-agent count
		// on gardenio_agents_active) — but the token/price-literal prohibition
		// applies to every query the chart issues, including the 1h spend
		// companion line.
		for _, ex := range def.ExtraSeries {
			eq := ex.QueryTemplate
			if strings.Contains(strings.ToLower(eq), "token") {
				t.Errorf("spend chart %q overlay %q references a token series — cost "+
					"charts must reshape spend, never tokens×rate:\n  %s", id, ex.Name, eq)
			}
			if loc := subDollarPriceLiteral.FindString(eq); loc != "" {
				t.Errorf("spend chart %q overlay %q embeds a sub-dollar price literal "+
					"%q — no hardcoded per-token price is allowed:\n  %s", id, ex.Name, loc, eq)
			}
		}
	}

	// Guard against a vacuous pass: the catalog must actually contain the spend
	// charts the design inventory (§3 kbeads cluster) enumerates.
	if foundSpend < 3 {
		t.Fatalf("found only %d spend-category charts; expected the spend-by-agent, "+
			"agent-cost-per-bead, and hourly-rate charts (catalog likely changed shape)", foundSpend)
	}
}
