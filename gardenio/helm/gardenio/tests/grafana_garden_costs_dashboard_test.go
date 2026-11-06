package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// Garden costs dashboard (gd-lz8YGWtsY8, plan gd-gh5J0vVbJP step 1.4).
//
// gardenio-garden-costs is the board /garden embeds one solo panel at a time
// (/d-solo/gardenio-garden-costs?panelId=N). The garden's embed registry
// (internal/grafanaembed, step 1.1) names panels by id, so an id that moves or
// disappears turns a garden card into a different chart or an empty frame with
// no error anywhere. That is why this board is hand-written JSON with explicit
// ids rather than catalog-generated (costs.go's "PANEL IDS ARE POSITIONAL"
// note), and why this test pins every embeddable id to its type and title. The
// table here is the one in gardenio/docs/garden-grafana-panels.md
// ("After 1.4: gardenio-garden-costs"); change both in the same PR.
//
// It also pins the accuracy contract the design note sets for the numbers in a
// card to equal the farm's cost coins: UTC timezone (so now/d is 00:00 UTC, the
// farm's today_usd window), the farm's own tag-gauge expression on the window
// stat, the /costs headline query on the fleet spend rate, and real-bill series
// only.

const gardenCostsFile = "gardenio-garden-costs.json"

type gardenCostsPanel struct {
	ID          int    `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Datasource  *struct {
		UID string `json:"uid"`
	} `json:"datasource"`
	Targets []struct {
		Expr    string `json:"expr"`
		RefID   string `json:"refId"`
		Instant bool   `json:"instant"`
	} `json:"targets"`
}

type gardenCostsBoard struct {
	UID        string             `json:"uid"`
	Timezone   string             `json:"timezone"`
	Panels     []gardenCostsPanel `json:"panels"`
	Templating struct {
		List []struct {
			Name       string `json:"name"`
			Definition string `json:"definition"`
			IncludeAll bool   `json:"includeAll"`
			AllValue   string `json:"allValue"`
		} `json:"list"`
	} `json:"templating"`
}

func parseGardenCostsBoard(t *testing.T) gardenCostsBoard {
	t.Helper()
	var b gardenCostsBoard
	if err := json.Unmarshal([]byte(readDashboardJSON(t, gardenCostsFile)), &b); err != nil {
		t.Fatalf("%s is not valid JSON: %v", gardenCostsFile, err)
	}
	return b
}

func (b gardenCostsBoard) panel(t *testing.T, id int) gardenCostsPanel {
	t.Helper()
	for _, p := range b.Panels {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("%s has no panel id %d", gardenCostsFile, id)
	return gardenCostsPanel{}
}

func TestGardenCostsDashboard_UIDAndUTC(t *testing.T) {
	t.Parallel()
	b := parseGardenCostsBoard(t)
	if b.UID != "gardenio-garden-costs" {
		t.Errorf("uid = %q, want gardenio-garden-costs (the garden builds /d-solo/<uid> URLs from it)", b.UID)
	}
	// now/d rounds to the dashboard timezone; the garden's "today" chip must
	// mean since 00:00 UTC, the farm's today_usd window.
	if b.Timezone != "utc" {
		t.Errorf("timezone = %q, want utc so now/d matches the farm's today_usd window", b.Timezone)
	}
}

// The embeddable panels, by id. Changing a row here means changing the garden's
// registry and the design note's table in the same PR.
var gardenCostsPanelIDs = []struct {
	id    int
	typ   string
	title string
}{
	{1, "stat", "Spend (window)"},
	{2, "timeseries", "Fleet spend rate"},
	{3, "timeseries", "Spend by project over time"},
	{4, "bargauge", "Spend by model"},
	{5, "bargauge", "Spend by account"},
	{6, "table", "Spend by agent"},
	{7, "bargauge", "Spend by role"},
	{8, "bargauge", "Spend by model (project)"},
	{9, "stat", "Land purse spend, USD"},
	{10, "timeseries", "Project spend rate"},
	{11, "timeseries", "Per-agent spend spread"},
	{12, "timeseries", "Land spend rate"},
}

func TestGardenCostsDashboard_PanelIDsArePinned(t *testing.T) {
	t.Parallel()
	b := parseGardenCostsBoard(t)
	seen := map[int]bool{}
	for _, p := range b.Panels {
		if p.ID == 0 {
			t.Errorf("panel %q has no explicit id; Grafana would assign one by position", p.Title)
		}
		if seen[p.ID] {
			t.Errorf("panel id %d is used twice", p.ID)
		}
		seen[p.ID] = true
	}
	for _, want := range gardenCostsPanelIDs {
		p := b.panel(t, want.id)
		if p.Type != want.typ || p.Title != want.title {
			t.Errorf("panel %d = %s %q, want %s %q (the garden embeds it by id)", want.id, p.Type, p.Title, want.typ, want.title)
		}
	}
}

func TestGardenCostsDashboard_RealBillOnVictoriaMetrics(t *testing.T) {
	t.Parallel()
	b := parseGardenCostsBoard(t)
	realBill := []string{"gardenio_tag_cost_usd", "gardenio_account_cost_usd", "gardenio_agent_spend_logs_usd"}
	for _, p := range b.Panels {
		if len(p.Targets) == 0 {
			continue
		}
		if p.Datasource == nil || p.Datasource.UID != "victoriametrics" {
			t.Errorf("panel %d %q must bind the victoriametrics datasource", p.ID, p.Title)
		}
		for _, tgt := range p.Targets {
			// The dead at-close counter behind the old "Settled cost" P0.
			if strings.Contains(tgt.Expr, "gardenio_agent_spend_usd_total") {
				t.Errorf("panel %d %q reads gardenio_agent_spend_usd_total: %s", p.ID, p.Title, tgt.Expr)
			}
			ok := false
			for _, m := range realBill {
				if strings.Contains(tgt.Expr, m) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("panel %d %q target %s reads no real-bill gauge: %s", p.ID, p.Title, tgt.RefID, tgt.Expr)
			}
		}
	}
}

// Panel 1 is the farm's expression (farm_spend.go buildFarmProjectSpendQuery →
// costsTagGaugeSpendExpr) over $__range, with each project floored at 0 as
// ProjectSpendUSD does, so the card equals the coin for the same window.
func TestGardenCostsDashboard_WindowStatIsTheFarmExpression(t *testing.T) {
	t.Parallel()
	p := parseGardenCostsBoard(t).panel(t, 1)
	const want = `sum(clamp_min(sum by (value) (increase((max by (dim, value, env) (gardenio_tag_cost_usd{dim="project", value=~"$project"}))[$__range:5m])), 0))`
	if len(p.Targets) != 1 || p.Targets[0].Expr != want {
		t.Fatalf("panel 1 targets = %+v, want the single expr %s", p.Targets, want)
	}
	if !p.Targets[0].Instant {
		t.Error("panel 1 must be an instant query, so $__range is the whole picked window")
	}
}

// Panel 2 is the /costs headline (costs_rate.go fleetHourlyRateQuery and its 1h
// companion), scoped by $account. With account=All it is the headline exactly.
func TestGardenCostsDashboard_FleetRateIsTheCostsHeadline(t *testing.T) {
	t.Parallel()
	p := parseGardenCostsBoard(t).panel(t, 2)
	const sel = `gardenio_account_cost_usd{account!~"(?i:e2e-.*|probe-acct-.*)", account=~"$account"}`
	want := map[string]string{
		"A": "sum(increase((max by (account, env) (" + sel + "))[1d:5m])) / 24",
		"B": "sum(increase((max by (account, env) (" + sel + "))[1h:5m])) / 1",
	}
	if len(p.Targets) != len(want) {
		t.Fatalf("panel 2 has %d targets, want %d", len(p.Targets), len(want))
	}
	for _, tgt := range p.Targets {
		if tgt.Expr != want[tgt.RefID] {
			t.Errorf("panel 2 target %s = %s, want %s", tgt.RefID, tgt.Expr, want[tgt.RefID])
		}
	}
}

// The per-agent spend-logs gauge is Reset+Set each sweep: increase() over it is
// garbage (costs_rollup.go buildCostsRollupAgentGaugeQuery), so it is only ever
// read as an instant or per-step level.
func TestGardenCostsDashboard_SpendLogsGaugeIsNeverIncreased(t *testing.T) {
	t.Parallel()
	for _, p := range parseGardenCostsBoard(t).Panels {
		for _, tgt := range p.Targets {
			if strings.Contains(tgt.Expr, "gardenio_agent_spend_logs_usd") &&
				(strings.Contains(tgt.Expr, "increase(") || strings.Contains(tgt.Expr, "rate(")) {
				t.Errorf("panel %d %q takes increase()/rate() over the Reset+Set spend-logs gauge: %s", p.ID, p.Title, tgt.Expr)
			}
		}
	}
}

func TestGardenCostsDashboard_ProjectAndAccountVariables(t *testing.T) {
	t.Parallel()
	b := parseGardenCostsBoard(t)
	want := map[string]string{
		"project": `label_values(gardenio_tag_cost_usd{dim="project"}, value)`,
		"account": `label_values(gardenio_account_cost_usd{account!~"(?i:e2e-.*|probe-acct-.*)"}, account)`,
	}
	got := map[string]bool{}
	for _, v := range b.Templating.List {
		def, ok := want[v.Name]
		if !ok {
			continue
		}
		got[v.Name] = true
		if v.Definition != def {
			t.Errorf("variable %s definition = %s, want %s", v.Name, v.Definition, def)
		}
		// The garden sends var-project=$__all for the fleet cards.
		if !v.IncludeAll || v.AllValue != ".*" {
			t.Errorf("variable %s must include All with allValue .* (got includeAll=%v allValue=%q)", v.Name, v.IncludeAll, v.AllValue)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("board has no %s variable", name)
		}
	}
}
