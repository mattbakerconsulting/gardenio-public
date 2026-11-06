package gardenhub

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planspend"
)

// The Costs panel (web/costs.js, plan gd-gh5J0vVbJP step 2.1, gd-CTbawhpC3m): the Workbench's costs kind and the
// panel-card stack the Project panel's cost tab shares.

// costs.js's pure half and its card stack run under node (testdata/costs_test.js): the route an id names, the range
// chips and the one kept per viewer, the header's figures from the farm's cost rows, the panel's own failure words, and
// the stack's cards, lazy frames, slow line, range change and degraded states.
func TestCostsUnderNode(t *testing.T) {
	r := nodeScript(t, "costs_test.js")
	for _, k := range []string{"pure", "openers", "lazy", "update", "unset", "noObserver", "rangeOff", "unrecorded", "land"} {
		if r[k] != true {
			t.Errorf("costs_test.js did not reach %s: %v", k, r)
		}
	}
}

// A cost move over /garden/farm/events repaints every figure in place, no reload (gd-rtE07tg6Nh;
// testdata/costs_patch_browser.js). On a clock stopped at goldenNow, POST /test/farm/cost?step=up moves it past
// farmCostEvery and raises the fleet's spend, g-ask-b's and plan A's (its crew still at work); step=down takes the rollup
// down and moves the clock past planspend.StaleAfter, so the purse and plan A go stale. Each answers once the farm has
// published the patch.
func TestCostPatchUnderChromium(t *testing.T) {
	var clock atomic.Int64
	clock.Store(goldenNow.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	underChromium(t, "costs_patch_browser.js", func() *httptest.Server {
		srv, rig := cardWorldRig(t, now, "", nil, nil, false)
		rig.mux.HandleFunc("POST /test/farm/cost", func(w http.ResponseWriter, r *http.Request) {
			v0, _, _ := rig.farm.Snapshot()
			sp := rig.spend
			sp.mu.Lock()
			if r.URL.Query().Get("step") == "down" {
				sp.down = true
				clock.Add(int64(planspend.StaleAfter + time.Minute))
			} else {
				clock.Add(int64(farmCostEvery))
				today, _ := costWindows(now())
				sp.byWin[today], sp.byWin[7*24*time.Hour] = map[string]float64{"gardenio": 12.5}, map[string]float64{"gardenio": 40}
				rig.src.mutate("gd-planA", func(b *beadsapi.BeadDetail) { setPlanSpend(b, "19.2500", now().Unix(), "2", "1") })
				rig.src.mutate("gd-gAskB", func(b *beadsapi.BeadDetail) { b.Fields["total_cost_usd"] = "4.10" })
			}
			sp.mu.Unlock()
			rig.farm.Invalidate()
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if v, _, _ := rig.farm.Snapshot(); v != v0 {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			http.Error(w, "the farm published no patch", http.StatusGatewayTimeout)
		})
		return srv
	})
}

// The page inlines costs.js after the Workbench host and before the project panel (whose cost tab calls
// COSTS.paintCostCards), the page data carries it, and its renderer registers the costs kind.
func TestPageCarriesCosts(t *testing.T) {
	w, c, p := strings.Index(pageHTML, `{{asset "workbench.js"}}`), strings.Index(pageHTML, `{{asset "costs.js"}}`), strings.Index(pageHTML, `{{asset "project.js"}}`)
	if c < 0 || c < w || c > p {
		t.Fatalf("costs.js must be inlined after workbench.js and before project.js: %d %d %d", w, c, p)
	}
	if pageCostsJS == "" {
		t.Fatal("web/costs.js is not embedded")
	}
	mux := http.NewServeMux()
	(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	for _, want := range []string{"var COSTS = ", "W.register('costs',", "paintCostCards: paintCostCards", "cost:function(){return farmDoc&&farmDoc.cost||null;}"} {
		if !strings.Contains(inlined(t, rec.Body.String()), want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// The openers (step 2.3, gd-6vGD02rgka): the Menu's Costs item and the header purse (gd-H9LerJ0v3g) are in the page, hidden, and
// the body says data-costs only to a viewer CanSeeCosts lets read the charts, so a scoped viewer is never offered them.
func TestPageOffersCostsOpenersToOperatorsOnly(t *testing.T) {
	for _, may := range []bool{false, true} {
		mux := http.NewServeMux()
		(&Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", CanSeeCosts: func(*http.Request) bool { return may }}).Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
		body := rec.Body.String()
		for _, want := range []string{`<button class="btn mi" id="cam-costs" type="button" hidden>`, `<a class="purse" id="purse" data-href="/v/garden?costs=fleet" hidden>`} {
			if !strings.Contains(body, want) {
				t.Errorf("may=%v: the page lacks %q", may, want)
			}
		}
		if got := strings.Contains(body, ` data-costs="1"`); got != may {
			t.Errorf("may=%v: data-costs on the body = %v", may, got)
		}
	}
}

// Every route the panel asks for (COSTS.chartsURL, run under node) is one GET /garden/charts/{set} answers: the fleet
// and a project, over every chip, in the dark theme, each with the range it asked for.
func TestCostsURLsAreTheRoutes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := filepath.Abs(filepath.Join("web", "costs.js"))
	if err != nil {
		t.Fatal(err)
	}
	script := `var C = require(process.argv[1]); ["fleet", "gardenio"].forEach(function (id) { C.RANGES.forEach(function (r) { console.log(r[0] + " " + C.chartsURL(id, r[0])); }); });`
	out, err := exec.Command(node, "-e", script, src).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 6 {
		t.Fatalf("want 6 routes, got %q", lines)
	}
	h := &Handler{GrafanaBaseURL: "https://grafana.example"}
	for _, l := range lines {
		rng, path, _ := strings.Cut(l, " ")
		code, doc := getQueue(t, h, path)
		if code != http.StatusOK {
			t.Errorf("%s: status %d", path, code)
			continue
		}
		if doc["range"] != rng || doc["theme"] != "dark" {
			t.Errorf("%s: range %v theme %v", path, doc["range"], doc["theme"])
		}
		if panels, _ := doc["panels"].([]any); len(panels) == 0 {
			t.Errorf("%s: no panels", path)
		}
	}
}

// credits.js (plan gd-gh5J0vVbJP step 3.3) runs under node (testdata/credits_test.js): a tenant land's tiles, meter, burn
// line and per-agent bars from GET /garden/charts/costs-land, painted through createElementNS and textContent only, with
// the readout on a pointer and the arrows, and the degraded answers painting only the purse's meter.
func TestCreditsUnderNode(t *testing.T) {
	r := nodeScript(t, "credits_test.js")
	for _, k := range []string{"pure", "paint", "degraded"} {
		if r[k] != true {
			t.Errorf("credits_test.js did not reach %s: %v", k, r)
		}
	}
}
