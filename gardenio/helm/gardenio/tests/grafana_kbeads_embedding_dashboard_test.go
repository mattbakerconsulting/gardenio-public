package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// kbeads-embedding dashboard "Coverage ratio by fleet" panel (bug gd-79CKOmTvRc).
//
// The panel legends by {{fleet}}, but every series carries the constant label
// fleet="default" — the emitter never sets a distinct fleet, so pod churn over
// the window produces 20+ separate series that differ only by pod/instance and
// ALL render as an identical "default" legend entry (a useless wall of dupes).
//
// The fix aggregates the raw metric with `avg by (fleet)` so the churn collapses
// into ONE line per fleet, matching the sibling "Coverage (SLO ≥ 90%)" gauge
// which already reduces with avg(). This test guards that the coverage-ratio
// timeseries stays aggregated: a raw, un-aggregated selector would regress the
// legend back to the duplicate wall.

// kbeadsEmbeddingBoard is the minimal panel shape this test asserts against.
type kbeadsEmbeddingBoard struct {
	Panels []struct {
		ID      int    `json:"id"`
		Type    string `json:"type"`
		Title   string `json:"title"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
}

func parseKbeadsEmbeddingBoard(t *testing.T) kbeadsEmbeddingBoard {
	t.Helper()
	data := dashboardConfigMapData(t)
	const key = "kbeads-embedding.json"
	raw, ok := data[key]
	if !ok {
		t.Fatalf("dashboard ConfigMap is missing key %q; keys present: %v", key, keysOf(data))
	}
	var b kbeadsEmbeddingBoard
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("embedded %s is not valid JSON (nindent likely broke it): %v", key, err)
	}
	return b
}

func TestKbeadsEmbeddingDashboard_CoverageRatioAggregatesByFleet(t *testing.T) {
	t.Parallel()
	b := parseKbeadsEmbeddingBoard(t)
	var panel struct {
		found bool
		expr  string
	}
	for _, p := range b.Panels {
		if p.ID == 100 {
			panel.found = true
			for _, tg := range p.Targets {
				panel.expr = tg.Expr
			}
		}
	}
	if !panel.found {
		t.Fatalf("coverage-ratio panel id=100 not found on kbeads-embedding board")
	}
	// Must reduce over pod/instance churn into one series per fleet. A raw
	// `gardenio_beads_embedding_coverage_ratio{...}` selector with no aggregation
	// is the regressed shape (20+ identical "default" legend entries).
	if !strings.Contains(panel.expr, "by (fleet)") {
		t.Errorf("coverage-ratio panel must aggregate `by (fleet)` to collapse pod churn "+
			"into one series per fleet; got expr %q", panel.expr)
	}
	if !strings.Contains(panel.expr, "gardenio_beads_embedding_coverage_ratio") {
		t.Errorf("coverage-ratio panel must query gardenio_beads_embedding_coverage_ratio; got expr %q", panel.expr)
	}
}
