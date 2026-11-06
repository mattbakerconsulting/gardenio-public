package grafanagrade

import "strings"

// The third GRADE detector: does a panel that renders a NUMBER say what the
// number is measured in? (bug ka-0000002317, split out of ka-0000004933,
// epic ka-0000008066.)
//
// The finding this replaces was produced by the 2026-09-02 iteration's scorer
// (docs/grafana-grade/2026-09-02T07:23:00Z/build_verdicts.py), which counted a
// panel as united iff `fieldConfig.defaults.unit` was non-empty:
//
//	if st["unit"] < nviz:
//	    iss.append(f"[aesthetics] {nviz - st['unit']}/{nviz} panel(s) carry no unit")
//
// That read 91 of 338 viz panels as unit-less. Most of those were the scorer
// looking in ONE place while the panel legitimately carries its unit somewhere
// else — and a mechanical sweep to satisfy it would have written 91 assertions
// that are variously redundant, wrong, or nonsense, which is the same class of
// mistake as the three failed passes on cost-overview's "Deferred Agents"
// (ka-0000007164 / ka-0000002930): swapping one false reading for another.
//
// So this detector knows the four shapes where the default is legitimately
// absent. Each is an exemption a REAL panel in the fleet needs, not a
// hypothetical:
//
//   - PER-FIELD units (fieldConfig.overrides). A table's columns are
//     heterogeneous — `requests` is a count, `last_seen` is a timestamp, `p95`
//     is seconds. A panel default would be applied to ALL of them, so on a
//     table the default being empty is correct and the unit belongs on a
//     `byName`/`byRegexp`/`byFrameRefID` override. 8 fleet tables do exactly
//     this (e.g. perf-traces "Recent traces" units its Duration column in µs).
//
//   - HEATMAP bucket axis (options.yAxis.unit). A heatmap's scale is the
//     BUCKET axis, which lives outside fieldConfig entirely. All 11 fleet
//     heatmaps set it; none sets a fieldConfig default, and one should not —
//     the cell value is a count of observations, not a duration.
//
//   - VALUE-MAPPED panels (fieldConfig.defaults.mappings of type value/range).
//     A state-timeline that maps 0→"OK" and 1→"ACTIVE", or a stat that maps
//     0→"OFF"/1→"ON", never renders the raw number at all. A unit there is
//     nonsense: there is no quantity, only a state name. Note the narrowness —
//     a `special` mapping (null/NaN → text) is empty-branch text, NOT a value
//     rendering, and does not exempt.
//
//   - LABEL-ONLY tables. A Prometheus `format: table` query yields one numeric
//     column ("Value", or "Value #<refID>" once frames merge); a panel that
//     drops it in an `organize` transformation is rendering label columns only.
//     An "Alert rules" table is this: alertname / detection / severity /
//     alertstate, with Value excluded. Nothing numeric survives for a unit to
//     attach to.
//
// What is left after those four is the honest residue: a panel that renders a
// raw number with no scale. The remedy is per panel and comes from the METRIC's
// dimension, read off the exporter and confirmed against gardenio-vmsingle — a
// `_seconds` histogram is `s`, a `_bytes` gauge is `bytes`, an `increase()` of
// a `_total` counter is a dimensionless count (`short`), and a ratio is
// `percentunit` unless the series is already 0-100. Guessing from the panel
// TITLE is how "Request rate by source IP" — whose column is a `count()` over
// the range, not a rate — would have acquired a wrong `reqps`.

// UnitFinding is one panel that renders numbers with no unit reachable from
// any of the four places Grafana looks.
type UnitFinding struct {
	Dashboard string `json:"dashboard"`
	Panel     string `json:"panel"`
	PanelType string `json:"panel_type"`
	Detail    string `json:"detail"`
}

func (f UnitFinding) String() string {
	return f.Dashboard + " / \"" + f.Panel + "\" [" + f.PanelType + "]: " + f.Detail
}

// Key identifies a finding across runs, for baselining. It omits Detail so
// that rewording a message does not invalidate a baseline.
func (f UnitFinding) Key() string {
	return f.Dashboard + "\x00" + f.Panel
}

// unitlessPanelKinds render no numeric field of their own, so the question
// this detector asks does not apply to them. `row` and `text` are chrome and
// narrative (isViz already excludes them); the rest render log lines, links or
// spans.
var unitlessPanelKinds = map[string]bool{
	"logs": true, "news": true, "dashlist": true, "alertlist": true,
	"annolist": true, "nodegraph": true, "traces": true,
}

// hasOverrideUnit reports whether any per-field override sets a unit. The
// matcher kind is deliberately not inspected: byName, byRegexp and
// byFrameRefID are all legitimate ways to unit a subset of columns, and which
// one is right depends on whether the panel's frames merge.
func hasOverrideUnit(p *Panel) bool {
	for _, o := range p.FieldConfig.Overrides {
		for _, prop := range o.Properties {
			if prop.ID == "unit" {
				return true
			}
		}
	}
	return false
}

// valueMapped reports whether the panel replaces its numeric readings with
// text. Only `value` and `range` mappings do that; `special` matches null/NaN
// and is empty-branch text (emptybranch.go), which leaves the numbers rendering
// exactly as before.
func valueMapped(p *Panel) bool {
	for _, m := range p.FieldConfig.Defaults.Mappings {
		switch strings.ToLower(m.Type) {
		case "value", "range":
			return true
		}
	}
	return false
}

// labelOnlyTable reports whether an `organize` transformation drops the numeric
// column of EVERY query on the panel, leaving only label columns. Grafana names
// that column "Value" on a single frame and "Value #<refID>" once frames are
// joined, so both spellings count.
func labelOnlyTable(p *Panel) bool {
	excluded := map[string]bool{}
	for _, tr := range p.Transformations {
		if !strings.EqualFold(tr.ID, "organize") {
			continue
		}
		for name, drop := range tr.Options.ExcludeByName {
			if drop {
				excluded[name] = true
			}
		}
	}
	if len(excluded) == 0 {
		return false
	}
	live := 0
	for _, t := range p.Targets {
		if t.Hide {
			continue
		}
		live++
		if !excluded["Value"] && !excluded["Value #"+t.RefID] {
			return false
		}
	}
	return live > 0
}

// UnitedBy names the place this panel's unit comes from, or "" if it has none.
// Exported so a report can say WHY a panel was not flagged rather than leaving
// the exemption invisible — the failure mode of the scorer this replaces.
func UnitedBy(p *Panel) string {
	switch {
	case strings.TrimSpace(p.FieldConfig.Defaults.Unit) != "":
		return "fieldConfig.defaults.unit"
	case hasOverrideUnit(p):
		return "fieldConfig.overrides (per-field)"
	case strings.TrimSpace(p.Options.YAxis.Unit) != "":
		return "options.yAxis.unit (heatmap bucket axis)"
	case valueMapped(p):
		return "value mappings (renders state text, not a number)"
	case labelOnlyTable(p):
		return "label-only table (numeric column excluded)"
	}
	return ""
}

// CheckUnits reports every viz panel on the dashboard that renders a number
// with no unit. name is the file name, used only for reporting.
func CheckUnits(name string, d *Dashboard) []UnitFinding {
	var out []UnitFinding
	var walk func([]Panel)
	walk = func(ps []Panel) {
		for i := range ps {
			p := &ps[i]
			if strings.EqualFold(p.Type, "row") {
				walk(p.Panels)
				continue
			}
			if isViz(p) && !unitlessPanelKinds[strings.ToLower(p.Type)] && UnitedBy(p) == "" {
				out = append(out, UnitFinding{
					Dashboard: name,
					Panel:     p.Title,
					PanelType: p.Type,
					Detail: "renders a number with no unit — set fieldConfig.defaults.unit, " +
						"or a per-field override if the columns are heterogeneous; " +
						"choose it from the metric's dimension, not the panel title",
				})
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)
	return out
}
