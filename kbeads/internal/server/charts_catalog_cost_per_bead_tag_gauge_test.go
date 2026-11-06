package server

// Tests for ka-0000002556: the agent-cost-per-bead chart's by-{role,project,model}
// spend numerator reads the LIFETIME real-bill tag gauge gardenio_tag_cost_usd
// (not the per-agent gauge's ~48h slice), with a self-healing `or <per-agent>`
// fallback — the deferred kbeads-catalog half of ka-0000000478, the same shape
// the /costs page rollup (costs_rollup.go) and the Grafana "$rollup" panels 5/6
// (uid gb-cost-per-bead-rollup-preview) proved live against vmsingle.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCostPerBead_RollupReadsLifetimeTagGauge pins the numerator-source contract:
// the spend numerator MUST reference the lifetime tag gauge gardenio_tag_cost_usd
// and MUST carry the self-healing per-agent fallback; the denominator's fleet
// scalar (gardenio_beads_events_total) and its on() group_left() broadcast MUST be
// preserved. This is the guard that keeps a future edit from silently dropping the
// role/project/model rollup back to the per-agent gauge's recent-fleet slice.
func TestCostPerBead_RollupReadsLifetimeTagGauge(t *testing.T) {
	def := chartCatalog()["agent-cost-per-bead"]
	if def.ID == "" {
		t.Fatal("agent-cost-per-bead chart missing from catalog")
	}
	q := def.QueryTemplate

	// The lifetime real-bill tag gauge is now the by-{role,project,model} numerator.
	if !strings.Contains(q, "gardenio_tag_cost_usd") {
		t.Errorf("agent-cost-per-bead numerator must read the lifetime real-bill tag "+
			"gauge gardenio_tag_cost_usd for role/project/model, not only the ~48h "+
			"per-agent gauge:\n  %s", q)
	}
	// label_replace copies the gauge's shared `value` label onto a label named
	// after the roll-up dim, so the outer sum groups on the same label the
	// per-agent fallback arm does (the phantom-"Value" bug ka-0000002838 fixed).
	if !strings.Contains(q, `label_replace`) || !strings.Contains(q, `"value", "(.*)"`) {
		t.Errorf("agent-cost-per-bead must label_replace(value->{{.Rollup}}) so the "+
			"tag-gauge arm keys on the roll-up label like the per-agent arm:\n  %s", q)
	}
	// The tag gauge is lifetime-cumulative + monotonic, so increase() over an
	// aligned [:5m] subquery is the valid per-bucket flow (unlike the Reset+Set
	// per-agent gauge, where increase() reads garbage).
	if !strings.Contains(q, `gardenio_tag_cost_usd{dim="{{.Rollup}}"}`) {
		t.Errorf("agent-cost-per-bead tag-gauge arm must select dim=\"{{.Rollup}}\":\n  %s", q)
	}
	// Self-healing fallback: `or` the per-agent per-bucket flow so a missing key
	// (backfill window, or rollup=agent which the tag gauge excludes) falls
	// through to the per-agent gauge and auto-upgrades once the tag gauge fills.
	if !strings.Contains(q, " or ") || !strings.Contains(q, "gardenio_agent_spend_logs_usd") {
		t.Errorf("agent-cost-per-bead must carry the self-healing `or <per-agent gauge>` "+
			"fallback so the view is never blank during the tag-gauge backfill window "+
			"and rollup=agent still works:\n  %s", q)
	}
	// The dead at-close counter must never come back (ka-0000001663 / ka-0000002699).
	if strings.Contains(q, "gardenio_agent_spend_usd_total") {
		t.Errorf("agent-cost-per-bead must not read the dead at-close counter:\n  %s", q)
	}
	// Denominator preserved EXACTLY: the fleet-scalar closed-bead count broadcast
	// to every segment via on() group_left(), with the > 0 guard (ka-0000002556
	// caveat: only swap the spend numerator, keep the ratio denominator intact).
	if !strings.Contains(q, `on() group_left() (sum(increase(gardenio_beads_events_total{event="closed"}[{{.Step}}])) > 0)`) {
		t.Errorf("agent-cost-per-bead must preserve the fleet-scalar closed-bead "+
			"denominator broadcast via on() group_left() with the > 0 guard:\n  %s", q)
	}
	// PromQL's `or` binds looser than `/`, so the numerator must be parenthesized
	// or only the fallback arm would be divided by the denominator.
	if !strings.HasPrefix(strings.TrimSpace(q), "(") {
		t.Errorf("agent-cost-per-bead numerator `or` must be parenthesized before the "+
			"`/ denom` (else `or` binds looser than `/` and only the fallback divides):\n  %s", q)
	}
}

// TestCostPerBead_RollupRendersTagGaugeSeries drives the render end to end for a
// role rollup: the VM stub answers the tag-gauge arm (keyed by `role` after the
// label_replace) and the render keys its rows on the resolved roll-up label, so
// the operator sees the lifetime real-bill split by role — the recent-fleet-only
// undercount the per-agent numerator produced is gone.
func TestCostPerBead_RollupRendersTagGaugeSeries(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	// The tag-gauge arm renders the roll-up label (role) directly on the returned
	// series (label_replace copied value->role), so the stub keys by role and the
	// denominator answers a fleet scalar.
	stub := newVMRangeStub(t,
		vmRule{contains: "gardenio_tag_cost_usd", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s,%s]}}`,
			matrixSeries("role", "gardener", start, step, "20.0", "30.0"),
			matrixSeries("role", "crew", start, step, "10.0", "15.0"))},
		vmRule{contains: "gardenio_beads_events_total", body: fmt.Sprintf(
			`{"status":"success","data":{"resultType":"matrix","result":[%s]}}`,
			matrixSeries("", "", start, step, "5.0", "5.0"))},
	)
	s := &BeadsServer{}
	s.SetVMClient(stub)

	def := chartCatalog()["agent-cost-per-bead"]
	req := &chartRequest{
		Since:   time.Unix(start, 0).UTC(),
		Until:   time.Unix(start+2*step, 0).UTC(),
		Step:    time.Hour,
		Rollup:  "role",
		Echo:    map[string]any{"rollup": "role", "top_n": 10},
		rawVals: map[string][]string{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}

	// Two role series × two points → four rows, each keyed on the resolved roll-up.
	if render.IOSHint.SeriesField != "role" {
		t.Errorf("ios_hint series_field = %q, want role", render.IOSHint.SeriesField)
	}
	roles := map[string]bool{}
	for _, row := range render.Data {
		if _, ok := row["usd_per_bead"]; !ok {
			t.Errorf("row missing usd_per_bead: %+v", row)
		}
		if r, ok := row["role"].(string); ok {
			roles[r] = true
		}
	}
	if !roles["gardener"] || !roles["crew"] {
		t.Errorf("expected gardener+crew role series from the tag gauge, got %v", roles)
	}
}
