package main

import (
	"os/exec"
	"strings"
	"testing"
)

// echarts_catalog_overlay_test.go — behavioral coverage for the multi-query
// (secondary-Y-axis) extension in echarts_catalog.js (task ka-0000006625). The
// renderer is browser JS with no native Go test harness, so we drive its
// exported _toSeries / _buildOption entry points through `node`, stubbing the
// handful of window/document globals the IIFE reads at load. The test skips
// (rather than fails) when node is unavailable so `go test` stays green on a
// toolchain without it; CI's gardenio-deps image ships node, so the gate runs
// there.
//
// What it locks in: an ios_hint carrying an `overlays` entry bound to
// y_axis_index 1 must (a) yield a distinct overlay series tagged yAxisIndex 1,
// (b) keep the primary series on the implicit left axis, and (c) make
// buildOption emit a two-element yAxis with the overlay's own title — the
// active-agent count on its own right-hand "agents" scale, never squashed onto
// the spend axis. It also guards the single-series path so the four pre-existing
// charts keep their one-axis, no-legend shape.

// runRendererJS evaluates echarts_catalog.js under node with minimal DOM stubs,
// then runs `body` (which may reference the BeadsCharts export as `BC`) and
// prints whatever the body logs. Returns combined stdout.
func runRendererJS(t *testing.T, body string) string {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping JS renderer behavioral test")
	}
	prog := `
const fs = require('fs');
const src = fs.readFileSync('templates/static/echarts_catalog.js', 'utf8');
global.window = { addEventListener() {}, BEADS_VIEWER_BASE_PATH: '' };
global.document = {
  documentElement: { getAttribute() { return 'baseline'; } },
  querySelectorAll() { return []; },
};
global.getComputedStyle = () => ({ color: '#333' });
eval(src);
const BC = window.BeadsCharts;
` + body
	cmd := exec.Command("node", "-e", prog)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node eval failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestRenderer_AgentCountOverlay_SecondaryAxis(t *testing.T) {
	out := runRendererJS(t, `
const hint = {
  kind: 'line', x_field: 't', y_field: 'usd_per_hour', series_field: '',
  primary_name: 'spend', y_title: 'USD/hour',
  overlays: [{ name: 'active agents', y_field: 'agents', y_format: 'count', y_title: 'agents', y_axis_index: 1 }],
};
const data = [
  { t: '2026-06-09T00:00:00Z', usd_per_hour: 0.25 },
  { t: '2026-06-09T00:00:00Z', agents: 3 },
  { t: '2026-06-09T01:00:00Z', usd_per_hour: 0.40 },
  { t: '2026-06-09T01:00:00Z', agents: 5 },
];
const series = BC._toSeries(data, hint);
const agents = series.find(s => s.name === 'active agents');
const spend = series.find(s => s.name === 'spend');
console.log('NAMES=' + series.map(s => s.name).join(','));
console.log('AGENTS_AXIS=' + agents.yAxisIndex);
console.log('SPEND_AXIS=' + (spend.yAxisIndex === undefined ? 'left' : spend.yAxisIndex));
const opt = BC._buildOption({ ios_hint: hint, data, title: 'Hourly' }, {});
console.log('YAXIS_IS_ARRAY=' + Array.isArray(opt.yAxis));
console.log('YAXIS_LEN=' + opt.yAxis.length);
console.log('SECONDARY_NAME=' + opt.yAxis[1].name);
console.log('OPT_AGENTS_AXIS=' + opt.series.find(s => s.name === 'active agents').yAxisIndex);
console.log('OPT_SPEND_AXIS=' + (opt.series.find(s => s.name === 'spend').yAxisIndex === undefined ? 'left' : opt.series.find(s => s.name === 'spend').yAxisIndex));
console.log('HAS_LEGEND=' + (opt.legend ? 'yes' : 'no'));
`)
	want := []string{
		"NAMES=spend,active agents",
		"AGENTS_AXIS=1",
		"SPEND_AXIS=left",
		"YAXIS_IS_ARRAY=true",
		"YAXIS_LEN=2",
		"SECONDARY_NAME=agents",
		"OPT_AGENTS_AXIS=1",
		"OPT_SPEND_AXIS=left",
		"HAS_LEGEND=yes",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in renderer output:\n%s", w, out)
		}
	}
}

// TestRenderer_FleetSpendRate_1hCompanionAndAgents locks in the three-series
// fleet-spend-rate shape (gd-Uvo8TZ8Kei): the primary 24h spend line and the 1h
// spend companion both sit on the LEFT USD/hour axis, while the active-agent
// count binds to the right-hand secondary axis. A primary-axis (y_axis_index 0)
// overlay must NOT promote itself to the secondary axis — it shares the left
// scale with the primary because both are USD/hour.
func TestRenderer_FleetSpendRate_1hCompanionAndAgents(t *testing.T) {
	out := runRendererJS(t, `
const hint = {
  kind: 'line', x_field: 't', y_field: 'usd_per_hour', series_field: '',
  primary_name: 'spend', y_title: 'USD/hour',
  overlays: [
    { name: 'spend (1h)', y_field: 'usd_per_hour_1h', y_format: 'rate_per_hour', y_title: 'USD/hour', y_axis_index: 0 },
    { name: 'active agents', y_field: 'agents', y_format: 'count', y_title: 'agents', y_axis_index: 1 },
  ],
};
const data = [
  { t: '2026-06-09T00:00:00Z', usd_per_hour: 0.25 },
  { t: '2026-06-09T00:00:00Z', usd_per_hour_1h: 0.50 },
  { t: '2026-06-09T00:00:00Z', agents: 3 },
  { t: '2026-06-09T01:00:00Z', usd_per_hour: 0.40 },
  { t: '2026-06-09T01:00:00Z', usd_per_hour_1h: 0.90 },
  { t: '2026-06-09T01:00:00Z', agents: 5 },
];
const series = BC._toSeries(data, hint);
console.log('NAMES=' + series.map(s => s.name).join(','));
const oneh = series.find(s => s.name === 'spend (1h)');
console.log('ONEH_AXIS=' + (oneh.yAxisIndex === undefined ? 'left' : oneh.yAxisIndex));
const opt = BC._buildOption({ ios_hint: hint, data, title: 'Fleet' }, {});
console.log('YAXIS_LEN=' + opt.yAxis.length);
console.log('OPT_ONEH_AXIS=' + (opt.series.find(s => s.name === 'spend (1h)').yAxisIndex === undefined ? 'left' : opt.series.find(s => s.name === 'spend (1h)').yAxisIndex));
console.log('OPT_AGENTS_AXIS=' + opt.series.find(s => s.name === 'active agents').yAxisIndex);
console.log('OPT_SPEND_AXIS=' + (opt.series.find(s => s.name === 'spend').yAxisIndex === undefined ? 'left' : opt.series.find(s => s.name === 'spend').yAxisIndex));
`)
	want := []string{
		"NAMES=spend,spend (1h),active agents",
		"ONEH_AXIS=left", // 1h companion shares the primary left axis
		"YAXIS_LEN=2",    // one secondary axis (for the agent count) is still added
		"OPT_ONEH_AXIS=left",
		"OPT_AGENTS_AXIS=1",
		"OPT_SPEND_AXIS=left",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in renderer output:\n%s", w, out)
		}
	}
}

func TestRenderer_SingleSeries_Unchanged(t *testing.T) {
	out := runRendererJS(t, `
const hint = { kind: 'line', x_field: 't', y_field: 'usd_per_hour', series_field: '' };
const data = [
  { t: '2026-06-09T00:00:00Z', usd_per_hour: 0.25 },
  { t: '2026-06-09T01:00:00Z', usd_per_hour: 0.40 },
];
const series = BC._toSeries(data, hint);
console.log('SERIES_COUNT=' + series.length);
const opt = BC._buildOption({ ios_hint: hint, data, title: 'Hourly' }, {});
console.log('YAXIS_IS_ARRAY=' + Array.isArray(opt.yAxis));
console.log('HAS_LEGEND=' + (opt.legend ? 'yes' : 'no'));
console.log('SERIES_NAME=' + opt.series[0].name);
`)
	want := []string{
		"SERIES_COUNT=1",
		"YAXIS_IS_ARRAY=false",
		"HAS_LEGEND=no",
		"SERIES_NAME=Hourly",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in renderer output:\n%s", w, out)
		}
	}
}
