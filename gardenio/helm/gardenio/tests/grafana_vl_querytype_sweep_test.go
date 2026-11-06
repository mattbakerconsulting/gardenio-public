package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fleet-wide VictoriaLogs queryType sweep (bug ka-0000008177, epic ka-0000008066).
//
// The VictoriaLogs plugin routes a target on its queryType, and the DEFAULT
// (absent/empty) is raw-log retrieval. So a target whose expr is a `| stats ...`
// pipeline but which declares no queryType returns log lines to a panel that is
// trying to plot a number: the panel renders EMPTY against a perfectly live
// datasource, with no error to explain it.
//
// The per-board tests already assert that a queryType, when present, is a real
// plugin enum value (TestEdgeGeomapDashboard_ValidQueryTypes and its siblings) —
// but their valid-set includes "" for the raw-logs panels, so an ABSENT
// queryType on a stats panel passes them. The richness uplift shipped five such
// panels onto the VictoriaLogs boards and every per-board test
// stayed green; this sweep is what closes that hole, fleet-wide rather than on
// the two boards that happen to have hand-written per-board tests.
//
// The rule is narrow on purpose: it fires only where the plugin's routing is
// genuinely load-bearing — a VictoriaLogs-bound target whose expr pipes into
// `stats`. Raw-log panels (type "logs") legitimately carry no queryType, and
// prometheus targets have no queryType at all. The fleet measured 0 violations
// when this landed, so it is a hard gate with no baseline to keep honest.
func TestVictoriaLogsStatsTargets_DeclareQueryType(t *testing.T) {
	t.Parallel()
	type target struct {
		Expr       string          `json:"expr"`
		QueryType  string          `json:"queryType"`
		Datasource json.RawMessage `json:"datasource"`
	}
	type panel struct {
		Type       string          `json:"type"`
		Title      string          `json:"title"`
		Datasource json.RawMessage `json:"datasource"`
		Targets    []target        `json:"targets"`
		Panels     []panel         `json:"panels"`
	}
	var doc struct {
		Panels []panel `json:"panels"`
	}

	// instant / stats are the point-in-time forms; statsRange is the
	// time-series form. Which one a panel wants is its own business — this
	// sweep only insists that it says.
	valid := map[string]bool{"instant": true, "stats": true, "statsRange": true, "hits": true}

	paths, err := filepath.Glob(filepath.Join("..", "dashboards", "*.json"))
	if err != nil {
		t.Fatalf("glob dashboards: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no dashboards/*.json found — this sweep would pass vacuously")
	}

	checked := 0
	var walk func(file string, ps []panel)
	walk = func(file string, ps []panel) {
		for _, p := range ps {
			walk(file, p.Panels)
			if p.Type == "row" || p.Type == "text" || p.Type == "logs" {
				continue
			}
			for _, tg := range p.Targets {
				// The datasource may be bound on the target or inherited
				// from the panel; either spelling counts.
				ds := string(tg.Datasource) + string(p.Datasource)
				if !strings.Contains(ds, "victoriametrics-logs-datasource") {
					continue
				}
				if !strings.Contains(tg.Expr, "stats") {
					continue
				}
				checked++
				if !valid[tg.QueryType] {
					t.Errorf("%s: panel %q has a VictoriaLogs `stats` target with queryType %q — "+
						"the plugin defaults to RAW LOG retrieval, so this panel renders empty "+
						"against a live datasource. Set \"instant\" (point-in-time panels) or "+
						"\"statsRange\" (timeseries). expr:\n  %s",
						filepath.Base(file), p.Title, tg.QueryType, tg.Expr)
				}
			}
		}
	}

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		doc.Panels = nil
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		walk(path, doc.Panels)
	}

	if checked == 0 {
		t.Error("swept 0 VictoriaLogs stats targets — the datasource-type or `stats` match drifted, " +
			"so this gate is vacuous")
	}
	t.Logf("swept %d VictoriaLogs stats targets across %d boards", checked, len(paths))
}
