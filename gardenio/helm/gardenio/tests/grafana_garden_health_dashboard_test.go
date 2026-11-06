package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// Garden health dashboard (gd-SKVLOuGToB, plan gd-gh5J0vVbJP step 4.1).
//
// gardenio-garden-health is the board the Project panel's Health tab embeds
// one solo panel at a time (/d-solo/gardenio-garden-health?panelId=N, the
// health-land set in internal/grafanaembed). As with gardenio-garden-costs
// (grafana_garden_costs_dashboard_test.go), an id that moves turns a garden
// card into a different chart with no error anywhere, so this board is
// hand-written with explicit ids and this test pins every embeddable one. The
// table is the one in gardenio/docs/garden-grafana-panels.md ("After 4.1:
// gardenio-garden-health"); change both in the same PR.
//
// It also pins what the cards say they read: the Lands SLO recordings of
// gardenio/docs/lands/slos.md and the off-cluster prober's series (fleet-wide),
// and the land's own series cut by the project variable.

const gardenHealthFile = "gardenio-garden-health.json"

func parseGardenHealthBoard(t *testing.T) gardenCostsBoard {
	t.Helper()
	var b gardenCostsBoard
	if err := json.Unmarshal([]byte(readDashboardJSON(t, gardenHealthFile)), &b); err != nil {
		t.Fatalf("%s is not valid JSON: %v", gardenHealthFile, err)
	}
	return b
}

func gardenHealthPanel(t *testing.T, b gardenCostsBoard, id int) gardenCostsPanel {
	t.Helper()
	for _, p := range b.Panels {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("%s has no panel id %d", gardenHealthFile, id)
	return gardenCostsPanel{}
}

func TestGardenHealthDashboard_UIDAndUTC(t *testing.T) {
	t.Parallel()
	b := parseGardenHealthBoard(t)
	if b.UID != "gardenio-garden-health" {
		t.Errorf("uid = %q, want gardenio-garden-health (the garden builds /d-solo/<uid> URLs from it)", b.UID)
	}
	// The garden's today chip sends now/d, meaning since 00:00 UTC.
	if b.Timezone != "utc" {
		t.Errorf("timezone = %q, want utc so the today chip is since 00:00 UTC", b.Timezone)
	}
}

// The embeddable panels, by id. Changing a row here means changing the garden's
// registry and the design note's table in the same PR.
var gardenHealthPanelIDs = []struct {
	id    int
	typ   string
	title string
	land  bool // cut by $project; false is fleet-wide
}{
	{1, "timeseries", "SLO burn rate", false},
	{2, "bargauge", "Error budget left (30 d)", false},
	{3, "timeseries", "SLO error ratio (5 m)", false},
	{4, "stat", "Uptime probes now", false},
	{5, "bargauge", "Probe availability (window)", false},
	{6, "stat", "Prober last ran", false},
	{7, "timeseries", "Land API requests by caller", true},
	{8, "timeseries", "Land API problems by code", true},
	{9, "stat", "Member calls, last 7 days", true},
	{10, "bargauge", "Land API calls by op (window)", true},
	{11, "timeseries", "Land API requests by land", false},
	{12, "timeseries", "Commons event lag (p50, p95)", true},
	{13, "timeseries", "Agent spawn to working (p50, p95)", true},
}

func TestGardenHealthDashboard_PanelIDsArePinned(t *testing.T) {
	t.Parallel()
	b := parseGardenHealthBoard(t)
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
	for _, want := range gardenHealthPanelIDs {
		p := gardenHealthPanel(t, b, want.id)
		if p.Type != want.typ || p.Title != want.title {
			t.Errorf("panel %d = %s %q, want %s %q (the garden embeds it by id)", want.id, p.Type, p.Title, want.typ, want.title)
		}
	}
}

// Every query panel binds VictoriaMetrics. The fleet-wide SLO panels read the
// SLO recordings or the prober and are never cut by a land; the land panels
// are cut by $project, so a land's card never shows another land's calls.
func TestGardenHealthDashboard_FleetAndLandSeries(t *testing.T) {
	t.Parallel()
	b := parseGardenHealthBoard(t)
	for _, want := range gardenHealthPanelIDs {
		p := gardenHealthPanel(t, b, want.id)
		if p.Datasource == nil || p.Datasource.UID != "victoriametrics" {
			t.Errorf("panel %d %q must bind the victoriametrics datasource", p.ID, p.Title)
		}
		if len(p.Targets) == 0 {
			t.Errorf("panel %d %q has no query", p.ID, p.Title)
		}
		for _, tgt := range p.Targets {
			cut := strings.Contains(tgt.Expr, `land=~"$project"`)
			switch {
			case want.land && !cut:
				t.Errorf("land panel %d %q is not cut by the project variable: %s", p.ID, p.Title, tgt.Expr)
			case !want.land && cut:
				t.Errorf("fleet panel %d %q is cut by the project variable: %s", p.ID, p.Title, tgt.Expr)
			case p.ID <= 6 && !strings.Contains(tgt.Expr, "gardenio:lands_slo_errors:ratio_rate") && !strings.Contains(tgt.Expr, "gardenio_uptime_probe_"):
				t.Errorf("SLO panel %d %q reads neither the SLO recordings nor the prober: %s", p.ID, p.Title, tgt.Expr)
			}
		}
	}
}

// The burn rate and the budget divide by the recorded objective rather than a
// target written into the board, so an overlay that moves a target
// (slos.md, "Changing a target") moves the charts with it.
func TestGardenHealthDashboard_BurnReadsTheObjective(t *testing.T) {
	t.Parallel()
	b := parseGardenHealthBoard(t)
	for _, id := range []int{1, 2} {
		p := gardenHealthPanel(t, b, id)
		if len(p.Targets) != 1 || !strings.Contains(p.Targets[0].Expr, "(1 - gardenio:lands_slo:objective)") {
			t.Errorf("panel %d %q targets = %+v, want one over (1 - gardenio:lands_slo:objective)", id, p.Title, p.Targets)
		}
	}
	if p := gardenHealthPanel(t, b, 2); !p.Targets[0].Instant || !strings.Contains(p.Targets[0].Expr, "ratio_rate30d") {
		t.Errorf("panel 2 must be an instant read of the 30d recording: %+v", p.Targets)
	}
	if p := gardenHealthPanel(t, b, 3); len(p.Targets) != 1 || p.Targets[0].Expr != "gardenio:lands_slo_errors:ratio_rate5m" {
		t.Errorf("panel 3 must chart gardenio:lands_slo_errors:ratio_rate5m as recorded: %+v", p.Targets)
	}
}

// Member calls is the v1-beta exit's criterion 2 read (lands/contract/
// V1_BETA_EXIT.md): members only, the trailing 7 days.
func TestGardenHealthDashboard_MemberCallsIsTheBetaExitRead(t *testing.T) {
	t.Parallel()
	p := gardenHealthPanel(t, parseGardenHealthBoard(t), 9)
	const want = `sum(increase(gardenio_lands_api_requests_total{land=~"$project", caller="member"}[7d]))`
	if len(p.Targets) != 1 || p.Targets[0].Expr != want || !p.Targets[0].Instant {
		t.Fatalf("panel 9 targets = %+v, want the single instant expr %s", p.Targets, want)
	}
}

// The garden sends var-project=<land>. All is `.+`, never `.*`: land="" is
// every call the map did not admit, which is no land's use.
func TestGardenHealthDashboard_ProjectVariableIsTheLand(t *testing.T) {
	t.Parallel()
	for _, v := range parseGardenHealthBoard(t).Templating.List {
		if v.Name != "project" {
			continue
		}
		if v.Definition != `label_values(gardenio_lands_api_requests_total{land!=""}, land)` {
			t.Errorf("variable project definition = %s", v.Definition)
		}
		if !v.IncludeAll || v.AllValue != ".+" {
			t.Errorf("variable project must include All with allValue .+ (got includeAll=%v allValue=%q)", v.IncludeAll, v.AllValue)
		}
		return
	}
	t.Error("board has no project variable")
}
