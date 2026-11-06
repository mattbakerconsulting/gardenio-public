package gardenhub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/grafanaembed"
)

// The garden's Grafana chart sets (GET /garden/charts/{set}, plan gd-gh5J0vVbJP
// 1.2). These read the registry through grafanaembed and never name a panel id,
// so the registry can move under them (1.5) without touching this file. getQueue
// (queue_test.go) checks the JSON content type and no-store on every 200.

func chartsStatus(t *testing.T, h *Handler, path string) int {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code
}

// The fleet set answers every registry panel as a card with its words, a src
// and open_url on the configured base, and the normalised inputs; the request's
// range and theme reach every URL.
func TestChartsFleetSet(t *testing.T) {
	set, ok := grafanaembed.LookupSet("costs-fleet")
	if !ok {
		t.Fatal("registry has no costs-fleet")
	}
	h := &Handler{GrafanaBaseURL: "https://grafana.example/"}
	code, doc := getQueue(t, h, "/garden/charts/costs-fleet?range=today&theme=light")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if doc["set"] != "costs-fleet" || doc["project"] != "" || doc["range"] != "today" || doc["theme"] != "light" {
		t.Errorf("inputs %v", doc)
	}
	if u, _ := doc["open_dashboard_url"].(string); !strings.HasPrefix(u, "https://grafana.example/d/"+set.UID+"?") {
		t.Errorf("open_dashboard_url %q", u)
	}
	panels, _ := doc["panels"].([]any)
	if len(panels) != len(set.Panels) || len(panels) == 0 {
		t.Fatalf("%d panels, want the registry's %d", len(panels), len(set.Panels))
	}
	for i, p := range panels {
		c := p.(map[string]any)
		if c["title"] != set.Panels[i].Title || c["scope"] != set.Panels[i].Scope {
			t.Errorf("card %d words %v, want the registry's", i, c)
		}
		src, _ := c["src"].(string)
		if !strings.HasPrefix(src, "https://grafana.example/d-solo/"+set.UID+"?panelId=") ||
			!strings.Contains(src, "&theme=light&from=now%2Fd&to=now&timezone=utc") {
			t.Errorf("card %d src %q", i, src)
		}
		if o, _ := c["open_url"].(string); !strings.HasPrefix(o, "https://grafana.example/d/"+set.UID+"?viewPanel=") {
			t.Errorf("card %d open_url %q", i, o)
		}
		if e, _ := c["embed_html"].(string); !strings.Contains(e, "<iframe") {
			t.Errorf("card %d embed_html %q", i, e)
		}
	}
}

// With no Grafana base URL the set still answers 200: every card keeps its
// words, with no src or open_url and the Unconfigured note as embed_html.
func TestChartsUnconfiguredDegrades(t *testing.T) {
	code, doc := getQueue(t, &Handler{}, "/garden/charts/costs-fleet")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200 with degraded cards", code)
	}
	if doc["range"] != grafanaembed.DefaultRange || doc["theme"] != string(grafanaembed.DefaultTheme) || doc["open_dashboard_url"] != "" {
		t.Errorf("defaults %v", doc)
	}
	panels, _ := doc["panels"].([]any)
	if len(panels) == 0 {
		t.Fatal("no cards")
	}
	for i, p := range panels {
		c := p.(map[string]any)
		e, _ := c["embed_html"].(string)
		if c["title"] == "" || c["src"] != "" || c["open_url"] != "" || !strings.Contains(e, "grafana-embed--unconfigured") || strings.Contains(e, "<iframe") {
			t.Errorf("card %d %v, want its words and the Unconfigured note", i, c)
		}
	}
}

// An unknown set is a 404; any other input outside its closed set is a 400,
// never a card built anyway.
func TestChartsRejects(t *testing.T) {
	h := &Handler{GrafanaBaseURL: "https://grafana.example"}
	for path, want := range map[string]int{
		"/garden/charts/nope":                               http.StatusNotFound,
		"/garden/charts/costs-fleet?project=gardenio":       http.StatusBadRequest, // fleet-wide takes no project
		"/garden/charts/costs-fleet?project=%24__all":       http.StatusBadRequest,
		"/garden/charts/costs-fleet?range=1y":               http.StatusBadRequest,
		"/garden/charts/costs-fleet?theme=sepia":            http.StatusBadRequest,
		"/garden/charts/costs-fleet?range=7d%26var-x%3D1":   http.StatusBadRequest,
		"/garden/charts/costs-fleet?theme=dark&range=today": http.StatusOK,
	} {
		if got := chartsStatus(t, h, path); got != want {
			t.Errorf("%s = %d, want %d", path, got, want)
		}
	}
}

// costs-project needs a project shaped as a DNS label, and one the farm knows
// once the farm has listed its projects; the project reaches every URL as
// var-project.
func TestChartsProjectSet(t *testing.T) {
	farm := &Farm{cost: []byte(`{"projects":[{"project":"gardenio"},{"project":"claimlayer"}]}`)}
	h := &Handler{GrafanaBaseURL: "https://grafana.example", Farm: farm}
	for path, want := range map[string]int{
		"/garden/charts/costs-project":                     http.StatusBadRequest,
		"/garden/charts/costs-project?project=Gardenio":    http.StatusBadRequest,
		"/garden/charts/costs-project?project=%24__all":    http.StatusBadRequest,
		"/garden/charts/costs-project?project=a%26var-x=1": http.StatusBadRequest,
		"/garden/charts/costs-project?project=nosuchland":  http.StatusBadRequest,
	} {
		if got := chartsStatus(t, h, path); got != want {
			t.Errorf("%s = %d, want %d", path, got, want)
		}
	}
	code, doc := getQueue(t, h, "/garden/charts/costs-project?project=claimlayer")
	if code != http.StatusOK || doc["project"] != "claimlayer" {
		t.Fatalf("%d %v", code, doc)
	}
	for i, p := range doc["panels"].([]any) {
		if src, _ := p.(map[string]any)["src"].(string); !strings.HasSuffix(src, "&var-project=claimlayer") {
			t.Errorf("card %d src %q, want var-project=claimlayer", i, src)
		}
	}

	// A farm that has not listed its projects yet cannot say; the shape check
	// alone decides.
	h.Farm = &Farm{}
	if got := chartsStatus(t, h, "/garden/charts/costs-project?project=nosuchland"); got != http.StatusOK {
		t.Errorf("unlisted farm: %d, want 200", got)
	}
}

// KnowsProject answers from the published cost blob, and says when it cannot.
func TestFarmKnowsProject(t *testing.T) {
	if known, listed := (&Farm{}).KnowsProject("gardenio"); known || listed {
		t.Errorf("empty farm: known %v listed %v, want neither", known, listed)
	}
	f := &Farm{cost: []byte(`{"projects":[{"project":"gardenio"}]}`)}
	if known, listed := f.KnowsProject("gardenio"); !known || !listed {
		t.Errorf("gardenio: known %v listed %v", known, listed)
	}
	if known, listed := f.KnowsProject("other"); known || !listed {
		t.Errorf("other: known %v listed %v", known, listed)
	}
	// A project the farm draws but has no cost row for (no type:project bead
	// yet) is still known (gd-vViKmjtrTj); with no blob the farm still cannot say.
	f.drawn = map[string]bool{"other": true}
	if known, listed := f.KnowsProject("other"); !known || !listed {
		t.Errorf("drawn other: known %v listed %v", known, listed)
	}
	if known, listed := (&Farm{drawn: map[string]bool{"other": true}}).KnowsProject("other"); known || listed {
		t.Errorf("drawn, no blob: known %v listed %v, want neither", known, listed)
	}
}

// The plot on the map and the Costs panel agree (gd-vViKmjtrTj): a project the
// farm draws a plan on charts with a 200 though no project bead gives it a cost
// row, and a project it neither draws nor costs is still a 400.
func TestChartsProjectDrawnWithoutCostRow(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	f.SetSpendSource(&fakeSpend{byWin: map[time.Duration]map[string]float64{}})
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.ProjectCost("gardenio") != nil {
		t.Fatal("the fixture world gives gardenio a cost row; this test wants a project without one")
	}
	h := &Handler{GrafanaBaseURL: "https://grafana.example", Farm: f}
	if got := chartsStatus(t, h, "/garden/charts/costs-project?project=gardenio"); got != http.StatusOK {
		t.Errorf("gardenio (drawn, no cost row) = %d, want 200", got)
	}
	if got := chartsStatus(t, h, "/garden/charts/costs-project?project=nosuchland"); got != http.StatusBadRequest {
		t.Errorf("nosuchland = %d, want 400", got)
	}
}

// A tenant land's costs-project and health-land sets carry the land-only
// panels, which the route takes from the farm's cost row (a purse means a
// tenant land), never from a request param; an operator project's set leaves
// them out.
func TestChartsProjectSetLandPanels(t *testing.T) {
	farm := &Farm{cost: []byte(`{"projects":[{"project":"gardenio"},{"project":"my-land","purse":{"state":"ok"}}]}`)}
	h := &Handler{GrafanaBaseURL: "https://grafana.example", Farm: farm}
	for _, name := range []string{"costs-project", "health-land"} {
		t.Run(name, func(t *testing.T) {
			set, ok := grafanaembed.LookupSet(name)
			if !ok {
				t.Skipf("%s is not in the registry", name)
			}
			landOnly := map[string]bool{}
			for _, p := range set.Panels {
				if p.LandOnly {
					landOnly[p.Title] = true
				}
			}
			if len(landOnly) == 0 {
				t.Skipf("%s has no land-only panel", name)
			}
			titles := func(path string) map[string]bool {
				t.Helper()
				code, doc := getQueue(t, h, path)
				if code != http.StatusOK {
					t.Fatalf("%s: status %d", path, code)
				}
				out := map[string]bool{}
				for _, p := range doc["panels"].([]any) {
					out[p.(map[string]any)["title"].(string)] = true
				}
				return out
			}
			land := titles("/garden/charts/" + name + "?project=my-land")
			if len(land) != len(set.Panels) {
				t.Errorf("land: %d panels, want all %d", len(land), len(set.Panels))
			}
			for title := range landOnly {
				if !land[title] {
					t.Errorf("land: no %q", title)
				}
			}
			op := titles("/garden/charts/" + name + "?project=gardenio&land=1")
			if len(op) == 0 {
				t.Error("operator project: no panel at all")
			}
			for title := range op {
				if landOnly[title] {
					t.Errorf("operator project: %q is land-only (a land= param must not add it)", title)
				}
			}
		})
	}
	if !farm.IsTenantLand("my-land") || farm.IsTenantLand("gardenio") || farm.IsTenantLand("nope") || (&Farm{}).IsTenantLand("my-land") {
		t.Error("IsTenantLand: want true only for the row with a purse")
	}
}
