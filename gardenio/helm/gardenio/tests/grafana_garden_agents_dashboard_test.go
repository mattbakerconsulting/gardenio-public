package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// Garden agents dashboard (gd-g7Ms8Ddfmj, plan gd-gh5J0vVbJP step 4.2).
//
// gardenio-garden-agents is the board the agent face and the Gardener panel in
// /garden embed one solo panel at a time (/d-solo/gardenio-garden-agents?
// panelId=N&var-agent=<name>[|<name>...]). The garden's embed registry
// (internal/grafanaembed, sets agent and gardener) names panels by id, so this
// test pins every embeddable id to its type and title, as
// grafana_garden_costs_dashboard_test.go does for the costs board; the table is
// the one in gardenio/docs/garden-grafana-panels.md ("Step 4.2:
// gardenio-garden-agents"). Change all three in the same PR.
//
// It also pins what makes a card about the agent it names: an `agent` textbox
// (a query variable would fall back to its first option for a name its query
// does not list, and show some other agent), every target scoped by it, and
// the per-agent spend-logs gauge read as a level only.

const gardenAgentsFile = "gardenio-garden-agents.json"

func parseGardenAgentsBoard(t *testing.T) gardenCostsBoard {
	t.Helper()
	var b gardenCostsBoard
	if err := json.Unmarshal([]byte(readDashboardJSON(t, gardenAgentsFile)), &b); err != nil {
		t.Fatalf("%s is not valid JSON: %v", gardenAgentsFile, err)
	}
	return b
}

func TestGardenAgentsDashboard_UIDAndUTC(t *testing.T) {
	t.Parallel()
	b := parseGardenAgentsBoard(t)
	if b.UID != "gardenio-garden-agents" {
		t.Errorf("uid = %q, want gardenio-garden-agents (the garden builds /d-solo/<uid> URLs from it)", b.UID)
	}
	if b.Timezone != "utc" {
		t.Errorf("timezone = %q, want utc, as the garden's today chip (since 00:00 UTC) assumes", b.Timezone)
	}
}

// The embeddable panels, by id: 1-4 on the agent face, 5 then 1-4 on the
// Gardener panel. Changing a row here means changing the registry and the
// design note's table in the same PR.
var gardenAgentsPanelIDs = []struct {
	id    int
	typ   string
	title string
}{
	{1, "timeseries", "Cost over time"},
	{2, "timeseries", "Context size"},
	{3, "timeseries", "Turns"},
	{4, "timeseries", "Tokens"},
	{5, "bargauge", "Spend by run"},
}

func TestGardenAgentsDashboard_PanelIDsArePinned(t *testing.T) {
	t.Parallel()
	b := parseGardenAgentsBoard(t)
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
	for _, want := range gardenAgentsPanelIDs {
		var got *gardenCostsPanel
		for i := range b.Panels {
			if b.Panels[i].ID == want.id {
				got = &b.Panels[i]
			}
		}
		if got == nil {
			t.Errorf("%s has no panel id %d (the garden embeds it)", gardenAgentsFile, want.id)
			continue
		}
		if got.Type != want.typ || got.Title != want.title {
			t.Errorf("panel %d = %s %q, want %s %q (the garden embeds it by id)", want.id, got.Type, got.Title, want.typ, want.title)
		}
	}
}

// Every target reads VictoriaMetrics, is scoped to $agent on the label its
// series carries (agent on the spend-logs gauge, agent_name on the session
// series), never reads the dead at-close counter, and never increase()s the
// Reset+Set spend-logs gauge.
func TestGardenAgentsDashboard_EveryTargetIsScopedToTheAgent(t *testing.T) {
	t.Parallel()
	scope := map[string]string{
		"gardenio_agent_spend_logs_usd":   `agent=~"$agent"`,
		"gardenio_session_context_tokens": `agent_name=~"$agent"`,
		"gardenio_session_turns_total":    `agent_name=~"$agent"`,
		"gardenio_session_tokens_total":   `agent_name=~"$agent"`,
	}
	for _, p := range parseGardenAgentsBoard(t).Panels {
		if len(p.Targets) == 0 {
			continue
		}
		if p.Datasource == nil || p.Datasource.UID != "victoriametrics" {
			t.Errorf("panel %d %q must bind the victoriametrics datasource", p.ID, p.Title)
		}
		for _, tgt := range p.Targets {
			if strings.Contains(tgt.Expr, "gardenio_agent_spend_usd_total") {
				t.Errorf("panel %d %q reads gardenio_agent_spend_usd_total: %s", p.ID, p.Title, tgt.Expr)
			}
			if strings.Contains(tgt.Expr, "gardenio_agent_spend_logs_usd") && (strings.Contains(tgt.Expr, "increase(") || strings.Contains(tgt.Expr, "rate(")) {
				t.Errorf("panel %d %q takes increase()/rate() over the Reset+Set spend-logs gauge: %s", p.ID, p.Title, tgt.Expr)
			}
			n := 0
			for metric, want := range scope {
				for _, part := range strings.Split(tgt.Expr, metric+"{")[1:] {
					n++
					if sel, _, _ := strings.Cut(part, "}"); !strings.Contains(sel, want) {
						t.Errorf("panel %d %q reads %s{%s} without %s", p.ID, p.Title, metric, sel, want)
					}
				}
			}
			if n == 0 {
				t.Errorf("panel %d %q target %s reads none of the agent series: %s", p.ID, p.Title, tgt.RefID, tgt.Expr)
			}
		}
	}
}

// The agent variable is a textbox: the garden's names (one agent, or a
// gardener's runs joined by |) are always taken as given, never swapped for a
// first option. Its default .* is every agent, for the board opened by hand.
func TestGardenAgentsDashboard_AgentIsATextbox(t *testing.T) {
	t.Parallel()
	var b struct {
		Templating struct {
			List []struct {
				Name  string `json:"name"`
				Type  string `json:"type"`
				Query string `json:"query"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal([]byte(readDashboardJSON(t, gardenAgentsFile)), &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Templating.List) != 1 {
		t.Fatalf("variables = %+v, want only agent (the garden sends var-agent and nothing else)", b.Templating.List)
	}
	if v := b.Templating.List[0]; v.Name != "agent" || v.Type != "textbox" || v.Query != ".*" {
		t.Errorf("variable = %+v, want the agent textbox defaulting to .*", v)
	}
}
