package gardenhub

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/grafanaembed"
)

// The per-agent chart sets (plan gd-gh5J0vVbJP step 4.2, gd-g7Ms8Ddfmj): GET /garden/charts/agent?agent=<name> for an
// agent's face and GET /garden/charts/gardener?gardener=<id> for a gardener loop's panel, whose agents are its runs as
// the daemon names them, never a request param.

// varAgent is the var-agent value a card's src carries.
func varAgent(t *testing.T, src string) string {
	t.Helper()
	u, err := url.Parse(src)
	if err != nil {
		t.Fatalf("src %q: %v", src, err)
	}
	return u.Query().Get("var-agent")
}

// One agent: its name on every card of the agent set, and back in the answer.
func TestChartsAgentSet(t *testing.T) {
	h := &Handler{GrafanaBaseURL: "https://grafana.example"}
	code, doc := getQueue(t, h, "/garden/charts/agent?agent=gardenio-guard-run-12&range=today")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	set, _ := grafanaembed.LookupSet("agent")
	panels, _ := doc["panels"].([]any)
	if len(panels) != len(set.Panels) || doc["set"] != "agent" || doc["range"] != "today" {
		t.Fatalf("answer %v", doc)
	}
	if a, _ := doc["agents"].([]any); len(a) != 1 || a[0] != "gardenio-guard-run-12" {
		t.Errorf("agents %v", doc["agents"])
	}
	for i, p := range panels {
		if v := varAgent(t, p.(map[string]any)["src"].(string)); v != "gardenio-guard-run-12" {
			t.Errorf("card %d var-agent %q", i, v)
		}
	}
}

// A gardener: its runs' names, newest first, joined by '|' (the spawn that never ran and a repeat left out), and its
// gardener param is the only way in: an agent param on the gardener set changes nothing.
func TestChartsGardenerSetIsItsRuns(t *testing.T) {
	g := fixtureGardenerPanels(time.Unix(baseUnix, 0)).(*fakeGardeners)
	g.docs["gd-GardRuns02"] = &GardenerDoc{ID: "gd-GardRuns02", Runs: []GardenerRun{
		{Name: "sweep-run-3"}, {Name: "sweep-run-2", DidNotRun: true}, {Name: "sweep-run-3"}, {Name: ""}, {Name: "Not A Name"}, {Name: "sweep-run-1"},
	}}
	h := &Handler{GrafanaBaseURL: "https://grafana.example", Gardeners: g}
	for path, want := range map[string]string{
		"/garden/charts/gardener?gardener=gd-GardTrig01":                    "gardenio-guard-run-12|gardenio-guard-run-11|gardenio-guard-run-10",
		"/garden/charts/gardener?gardener=gd-GardTrig01&agent=someone-else": "gardenio-guard-run-12|gardenio-guard-run-11|gardenio-guard-run-10",
		"/garden/charts/gardener?gardener=gd-GardRuns02":                    "sweep-run-3|sweep-run-1",
	} {
		code, doc := getQueue(t, h, path)
		if code != http.StatusOK {
			t.Errorf("%s: %d", path, code)
			continue
		}
		panels, _ := doc["panels"].([]any)
		if len(panels) == 0 || panels[0].(map[string]any)["title"] != "Spend by run" {
			t.Errorf("%s: cards %v", path, panels)
		}
		for i, p := range panels {
			if v := varAgent(t, p.(map[string]any)["src"].(string)); v != want {
				t.Errorf("%s card %d var-agent %q, want %q", path, i, v, want)
			}
		}
		if u, _ := doc["open_dashboard_url"].(string); !strings.Contains(u, "var-agent="+url.QueryEscape(want)) {
			t.Errorf("%s: open_dashboard_url %q", path, u)
		}
	}
}

// A gardener with no run yet answers no cards and its inputs, a 200 the panel says in words; its range is still checked.
func TestChartsGardenerWithNoRuns(t *testing.T) {
	g := fixtureGardenerPanels(time.Unix(baseUnix, 0))
	h := &Handler{GrafanaBaseURL: "https://grafana.example", Gardeners: g}
	code, doc := getQueue(t, h, "/garden/charts/gardener?gardener=gd-GardSched1&range=30d")
	if code != http.StatusOK || doc["set"] != "gardener" || doc["range"] != "30d" || doc["theme"] != "dark" {
		t.Fatalf("%d %v", code, doc)
	}
	if p, ok := doc["panels"].([]any); !ok || len(p) != 0 {
		t.Errorf("panels %v, want []", doc["panels"])
	}
	if got := chartsStatus(t, h, "/garden/charts/gardener?gardener=gd-GardSched1&range=1y"); got != http.StatusBadRequest {
		t.Errorf("a bad range on a gardener with no runs = %d, want 400", got)
	}
}

// Refusals: an agent name that is not one, a missing agent or gardener, a gardener the viewer may not see, a daemon that
// does not answer, no gardener reader, and an agent param on a costs set.
func TestChartsAgentSetsReject(t *testing.T) {
	g := fixtureGardenerPanels(time.Unix(baseUnix, 0))
	farm := &Farm{cost: []byte(`{"projects":[{"project":"gardenio"}]}`)}
	h := &Handler{GrafanaBaseURL: "https://grafana.example", Gardeners: g, Farm: farm}
	for path, want := range map[string]int{
		"/garden/charts/agent":                                         http.StatusBadRequest,
		"/garden/charts/agent?agent=":                                  http.StatusBadRequest,
		"/garden/charts/agent?agent=.%2A":                              http.StatusBadRequest,
		"/garden/charts/agent?agent=a%7Cb":                             http.StatusBadRequest,
		"/garden/charts/agent?agent=Gardenio-Run":                      http.StatusBadRequest,
		"/garden/charts/agent?agent=run-1%26var-x%3D1":                 http.StatusBadRequest,
		"/garden/charts/agent?agent=run-1&project=gardenio":            http.StatusBadRequest,
		"/garden/charts/agent?agent=run-1&range=90d":                   http.StatusBadRequest,
		"/garden/charts/gardener":                                      http.StatusBadRequest,
		"/garden/charts/gardener?gardener=gd-Nope":                     http.StatusNotFound,
		"/garden/charts/gardener?gardener=" + strings.Repeat("x", 201): http.StatusBadRequest,
		"/garden/charts/costs-fleet?agent=run-1":                       http.StatusOK, // a costs set never reads agent
		"/garden/charts/costs-project?project=gardenio&agent=run-1":    http.StatusOK,
	} {
		if got := chartsStatus(t, h, path); got != want {
			t.Errorf("%s = %d, want %d", path, got, want)
		}
	}
	if got := chartsStatus(t, &Handler{GrafanaBaseURL: "https://grafana.example"}, "/garden/charts/gardener?gardener=gd-GardTrig01"); got != http.StatusServiceUnavailable {
		t.Errorf("no gardener reader = %d, want 503", got)
	}
	down := &Handler{GrafanaBaseURL: "https://grafana.example", Gardeners: &fakeGardeners{err: errors.New("daemon down")}}
	if got := chartsStatus(t, down, "/garden/charts/gardener?gardener=gd-GardTrig01"); got != http.StatusBadGateway {
		t.Errorf("daemon down = %d, want 502", got)
	}
	// The viewer answers the gardener's header with Partial runs when its iterations did not read
	// (gardener_garden.go): that is a 502 the tab can retry, never "it has not run yet".
	partial := fixtureGardenerPanels(time.Unix(baseUnix, 0)).(*fakeGardeners)
	d := *partial.docs["gd-GardTrig01"]
	d.Runs, d.Partial = nil, []string{"runs", "beads"}
	partial.docs["gd-GardTrig01"] = &d
	if got := chartsStatus(t, &Handler{GrafanaBaseURL: "https://grafana.example", Gardeners: partial}, "/garden/charts/gardener?gardener=gd-GardTrig01"); got != http.StatusBadGateway {
		t.Errorf("runs did not read = %d, want 502", got)
	}
}

// With no Grafana base the cards keep their words and carry no src, for both sets.
func TestChartsAgentSetsUnconfigured(t *testing.T) {
	h := &Handler{Gardeners: fixtureGardenerPanels(time.Unix(baseUnix, 0))}
	for _, path := range []string{"/garden/charts/agent?agent=gardenio-guard-run-12", "/garden/charts/gardener?gardener=gd-GardTrig01"} {
		code, doc := getQueue(t, h, path)
		panels, _ := doc["panels"].([]any)
		if code != http.StatusOK || len(panels) == 0 {
			t.Fatalf("%s: %d %v", path, code, doc)
		}
		for i, p := range panels {
			if c := p.(map[string]any); c["src"] != "" || c["title"] == "" {
				t.Errorf("%s card %d: %v", path, i, c)
			}
		}
	}
}
