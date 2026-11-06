package grafanagrade

import (
	"encoding/json"
	"testing"
)

// unitPanel decodes a panel from raw JSON so the tests exercise the same decode
// path the fleet scan uses — an exemption that only works because a test built
// the struct by hand is not an exemption.
func unitPanel(t *testing.T, raw string) *Dashboard {
	t.Helper()
	d, err := ParseDashboard([]byte(`{"title":"t","panels":[` + raw + `]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return d
}

// TestCheckUnits_FalsePositiveShapes is the anti-vacuity core, and the reason
// this detector exists: it replays the four panel shapes the 2026-09-02 audit
// scorer counted as unit-less because it only read
// `fieldConfig.defaults.unit`. Every one is copied from a panel that actually
// ships in gardenio/helm/gardenio/dashboards. If a future "simplification" of
// unit.go makes any of these fire, the 91/338 finding is back and the next
// sweep writes 91 wrong assertions (ka-0000002317).
func TestCheckUnits_FalsePositiveShapes(t *testing.T) {
	cases := []struct {
		name string
		want string // the exemption UnitedBy must name
		json string
	}{
		{
			name: "heatmap bucket axis (control-plane-liveness #3 prewarm-assign latency)",
			want: "options.yAxis.unit (heatmap bucket axis)",
			json: `{"type":"heatmap","title":"Prewarm-assign latency heatmap",
			        "fieldConfig":{"defaults":{}},
			        "options":{"yAxis":{"unit":"s"}}}`,
		},
		{
			name: "table with per-column units (perf-traces Recent traces)",
			want: "fieldConfig.overrides (per-field)",
			json: `{"type":"table","title":"Recent traces",
			        "fieldConfig":{"defaults":{},"overrides":[
			          {"matcher":{"id":"byName","options":"Trace name"},"properties":[{"id":"custom.minWidth","value":320}]},
			          {"matcher":{"id":"byName","options":"Duration"},"properties":[{"id":"unit","value":"µs"}]}]}}`,
		},
		{
			name: "value-mapped state-timeline (cost-overview Account Throttle Status)",
			want: "value mappings (renders state text, not a number)",
			json: `{"type":"state-timeline","title":"Account Throttle Status",
			        "fieldConfig":{"defaults":{"mappings":[{"type":"value","options":{
			          "0":{"text":"OK","color":"green"},"1":{"text":"ACTIVE","color":"red"}}}]}}}`,
		},
		{
			name: "label-only table (Alert rules)",
			want: "label-only table (numeric column excluded)",
			json: `{"type":"table","title":"Alert rules — current state by detection",
			        "fieldConfig":{"defaults":{}},
			        "targets":[{"refId":"A","expr":"max by (alertname) (ALERTS)"}],
			        "transformations":[
			          {"id":"filterFieldsByName","options":{"include":{"names":["alertname","Value"]}}},
			          {"id":"organize","options":{"excludeByName":{"Value":true}}}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := unitPanel(t, tc.json)
			if got := UnitedBy(&d.Panels[0]); got != tc.want {
				t.Errorf("UnitedBy = %q, want %q", got, tc.want)
			}
			if fs := CheckUnits("test.json", d); len(fs) != 0 {
				t.Errorf("flagged an exempt panel: %v", fs)
			}
		})
	}
}

// TestCheckUnits_StillFires is the positive control: the exemptions above are
// narrow, not a blanket pass for their panel types. Each case here is the same
// shape with the one thing that made it honest removed.
func TestCheckUnits_StillFires(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{
			name: "heatmap with no axis unit at all",
			json: `{"type":"heatmap","title":"latency heatmap","fieldConfig":{"defaults":{}},"options":{}}`,
		},
		{
			name: "table whose overrides set colors but no unit",
			json: `{"type":"table","title":"Top countries","fieldConfig":{"defaults":{},"overrides":[
			         {"matcher":{"id":"byName","options":"requests"},"properties":[{"id":"color","value":{"mode":"continuous-YlOrRd"}}]}]}}`,
		},
		{
			name: "special (null) mapping is empty-branch text, not a value rendering",
			json: `{"type":"stat","title":"Deferred agents","fieldConfig":{"defaults":{"mappings":[
			         {"type":"special","options":{"match":"null","result":{"text":"no samples"}}}]}}}`,
		},
		{
			name: "organize transform that excludes a label, not the Value column",
			json: `{"type":"table","title":"Declines by reason","fieldConfig":{"defaults":{}},
			        "targets":[{"refId":"A","expr":"sum by (reason) (increase(x_total[1h]))"}],
			        "transformations":[{"id":"organize","options":{"excludeByName":{"Time":true}}}]}`,
		},
		{
			name: "multi-target table where only one query's Value is dropped",
			json: `{"type":"table","title":"Delivery outcomes","fieldConfig":{"defaults":{}},
			        "targets":[{"refId":"A","expr":"a"},{"refId":"B","expr":"b"}],
			        "transformations":[{"id":"organize","options":{"excludeByName":{"Value #A":true}}}]}`,
		},
		{
			name: "unit set to whitespace is not a unit",
			json: `{"type":"stat","title":"Spend","fieldConfig":{"defaults":{"unit":"  "}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := unitPanel(t, tc.json)
			if got := UnitedBy(&d.Panels[0]); got != "" {
				t.Errorf("UnitedBy = %q, want \"\" (no unit)", got)
			}
			if fs := CheckUnits("test.json", d); len(fs) != 1 {
				t.Fatalf("got %d findings, want 1: %v", len(fs), fs)
			}
		})
	}
}

// TestCheckUnits_NonNumericPanels: rows and narrative are chrome, and logs /
// dashlist / alertlist render no numeric field of their own. A unit on any of
// them is nonsense, so they are not part of the population at all.
func TestCheckUnits_NonNumericPanels(t *testing.T) {
	for _, typ := range []string{"row", "text", "logs", "news", "dashlist", "alertlist", "annolist", "nodeGraph", "traces"} {
		d := unitPanel(t, `{"type":"`+typ+`","title":"x","fieldConfig":{"defaults":{}}}`)
		if fs := CheckUnits("test.json", d); len(fs) != 0 {
			t.Errorf("%s panel flagged: %v", typ, fs)
		}
	}
}

// TestCheckUnits_WalksCollapsedRows: half the fleet's tables live inside
// collapsed rows, so a detector that only walks top-level panels would report a
// confident, wrong zero.
func TestCheckUnits_WalksCollapsedRows(t *testing.T) {
	d := unitPanel(t, `{"type":"row","title":"Principal history","collapsed":true,"panels":[
	    {"type":"table","title":"nested unit-less","fieldConfig":{"defaults":{}}},
	    {"type":"stat","title":"nested united","fieldConfig":{"defaults":{"unit":"s"}}}]}`)
	fs := CheckUnits("test.json", d)
	if len(fs) != 1 || fs[0].Panel != "nested unit-less" {
		t.Fatalf("got %v, want exactly the nested unit-less panel", fs)
	}
	if fs[0].Key() != "test.json\x00nested unit-less" {
		t.Errorf("Key() = %q", fs[0].Key())
	}
}

// TestUnitFinding_JSONRoundTrip keeps the finding shape stable for the report
// pipeline that consumes it.
func TestUnitFinding_JSONRoundTrip(t *testing.T) {
	in := UnitFinding{Dashboard: "a.json", Panel: "p", PanelType: "table", Detail: "d"}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out UnitFinding
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round trip: got %+v, want %+v", out, in)
	}
}
