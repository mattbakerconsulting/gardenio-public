package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// Claude-outage detector panel group on the System Health board (ka-0000004886,
// epic ka-0000000487).
//
// The four ClaudeUpstreamOutage-family alert rules carry a `dashboard`
// annotation that the Alertmanager→Slack template renders as the 📊 Graph link
// — the link the runbook tells the oncall to click FIRST. Those links deep-link
// into the panel group these tests guard:
//   - viewPanel=11  ClaudeUpstreamOutage        leg A (agents erroring upstream)
//   - viewPanel=12  ClaudeStatusPageOutage      leg B (status-page component severity)
//   - viewPanel=17  ClaudeStatusPollerStale     deadman (poller staleness)
//   - viewPanel=1   AgentUpstreamErrorEmitterDown (unchanged — component scrape health)
//
// If the panel ids or the series they plot drift, the Slack graph link lands the
// oncall on a graph that is not the signal that fired — worse than the board
// root. These tests pin both.

// systemHealthPanel is the minimal per-panel shape these tests assert against.
type systemHealthPanel struct {
	ID      int    `json:"id"`
	Title   string `json:"title"`
	Type    string `json:"type"`
	GridPos struct {
		H int `json:"h"`
		W int `json:"w"`
		X int `json:"x"`
		Y int `json:"y"`
	} `json:"gridPos"`
	FieldConfig struct {
		Defaults struct {
			Mappings json.RawMessage `json:"mappings"`
		} `json:"defaults"`
	} `json:"fieldConfig"`
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
}

type systemHealthDashboard struct {
	UID    string              `json:"uid"`
	Panels []systemHealthPanel `json:"panels"`
}

func parseSystemHealthDashboard(t *testing.T) systemHealthDashboard {
	t.Helper()
	data := dashboardConfigMapData(t)
	const key = "gardenio-system-health.json"
	raw, ok := data[key]
	if !ok {
		t.Fatalf("dashboard ConfigMap is missing key %q; keys present: %v", key, keysOf(data))
	}
	var d systemHealthDashboard
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("embedded %s is not valid JSON (nindent likely broke it): %v", key, err)
	}
	if d.UID != "gardenio-system-health" {
		t.Errorf("system-health board must carry its stable uid; got %q", d.UID)
	}
	return d
}

func systemHealthPanelByID(t *testing.T, d systemHealthDashboard, id int) systemHealthPanel {
	t.Helper()
	for _, p := range d.Panels {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("system-health board has no panel with id=%d", id)
	return systemHealthPanel{} // unreachable
}

func systemHealthPanelExprs(p systemHealthPanel) []string {
	out := make([]string, 0, len(p.Targets))
	for _, tg := range p.Targets {
		out = append(out, tg.Expr)
	}
	return out
}

// The alert deep-links target panel ids 11 (leg A), 12 (leg B), 17 (deadman).
// Each must exist, plot the exact series its rule thresholds on, and match the
// panel type that reads well for that signal.
func TestSystemHealthDashboard_ClaudeOutageAlertTargets(t *testing.T) {
	t.Parallel()
	d := parseSystemHealthDashboard(t)

	cases := []struct {
		id       int
		wantType string
		wantExpr string
		desc     string
	}{
		{11, "timeseries", "sum by (category) (gardenio_agent_upstream_error_active)", "leg A — ClaudeUpstreamOutage target"},
		{12, "state-timeline", "max by (component) (gardenio_claude_status_component)", "leg B — ClaudeStatusPageOutage target"},
		{17, "timeseries", "time() - gardenio_claude_status_last_check_timestamp_seconds", "deadman — ClaudeStatusPollerStale target"},
	}
	for _, c := range cases {
		p := systemHealthPanelByID(t, d, c.id)
		if p.Type != c.wantType {
			t.Errorf("panel %d (%s): type = %q, want %q", c.id, c.desc, p.Type, c.wantType)
		}
		var found bool
		for _, tg := range p.Targets {
			if strings.Contains(tg.Expr, c.wantExpr) {
				found = true
			}
		}
		if !found {
			t.Errorf("panel %d (%s): no target plots %q; targets=%v", c.id, c.desc, c.wantExpr, systemHealthPanelExprs(p))
		}
	}
}

// The whole design premise: leg A and leg B must be adjacent so the oncall reads
// them together to tell "us" from "them". Assert panels 11 and 12 share a row.
func TestSystemHealthDashboard_LegAAndLegBAreAdjacent(t *testing.T) {
	t.Parallel()
	d := parseSystemHealthDashboard(t)
	a := systemHealthPanelByID(t, d, 11)
	b := systemHealthPanelByID(t, d, 12)
	if a.GridPos.Y != b.GridPos.Y {
		t.Errorf("leg A (panel 11, y=%d) and leg B (panel 12, y=%d) must be on the SAME row so they read together",
			a.GridPos.Y, b.GridPos.Y)
	}
	if a.GridPos.X == b.GridPos.X {
		t.Errorf("leg A (panel 11) and leg B (panel 12) must sit side-by-side, not stacked; both at x=%d", a.GridPos.X)
	}
}

// Leg B's severity panel must render the ordinal ladder as named value mappings
// (0=operational … 4=under_maintenance), not bare numbers — the oncall must not
// have to remember what "2" means during a handoff.
func TestSystemHealthDashboard_LegBRendersSeverityLadderMappings(t *testing.T) {
	t.Parallel()
	d := parseSystemHealthDashboard(t)
	p := systemHealthPanelByID(t, d, 12)
	mappings := string(p.FieldConfig.Defaults.Mappings)
	if mappings == "" || mappings == "null" {
		t.Fatalf("leg B panel (12) must define value mappings for the severity ladder; got none")
	}
	for _, want := range []string{"operational", "degraded", "partial_outage", "major_outage", "under_maintenance"} {
		if !strings.Contains(mappings, want) {
			t.Errorf("leg B severity ladder mappings must name %q; mappings=%s", want, mappings)
		}
	}
}

// The board must also carry the source/role breakdown (acceptance: "broken down
// by source/role") and the leg-B open-incidents panel, so the group visually
// answers "who is affected" and "does the provider corroborate".
func TestSystemHealthDashboard_HasOutageBreakdownPanels(t *testing.T) {
	t.Parallel()
	d := parseSystemHealthDashboard(t)

	wantExprs := []string{
		`sum by (project, role) (gardenio_agent_upstream_error_active{category="server_error"})`,
		`sum by (impact) (gardenio_claude_status_open_incidents)`,
	}
	for _, want := range wantExprs {
		var found bool
		for _, p := range d.Panels {
			for _, tg := range p.Targets {
				if strings.Contains(tg.Expr, want) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("system-health board must carry a panel plotting %q", want)
		}
	}
}
