package server

// charts_audit_test.go — W3b: the machine-checkable data-correctness gate.
//
// Task ka-0000000418 (epic ka-0000000756, W3 "strict rubric"). This wires the
// rubric in gardenio/docs/data-correctness-rubric.md (C1–C6 + R1–R5) as a
// *binary acceptance gate* over the beads-viewer chart catalog, so a perf /
// trace panel (W2b, W2c) cannot ship as a dashboard-bead unless every gate
// passes. "Strict" = no averaging, no partial credit: one failing gate blocks
// the panel.
//
// The gates here are the STATIC ones — decidable from the ChartDef alone (no
// live store): structural integrity, C2 (the active-agents silent-sum bug,
// ka-0000002809), C3 unit-scale, C6 window-integrity, R1 legible-label, R4
// self-locating, R5 drill-path. TestCatalog_DataCorrectnessGate runs them over
// every entry in chartCatalog() and fails `go test` on any violation, so a new
// W2b/W2c catalog entry that violates a gate cannot merge until it is fixed.
// TestGate_BitesOnBadDefs proves each gate fires on a deliberately bad def — a
// guard against a vacuously-passing gate, the failure mode the cost guard test
// warns about.
//
// The live gates (C1 replay-match, C4 freshness, C5 existence-honesty and the
// trace reconciliations) need the store and are not decided here.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// FailClass is a rubric verdict. VerdictOK means every gate passed; any other
// value is the first failing fail-class for a panel (rubric §1 fail-class
// column + §2.1 SPANMETRIC-DRIFT).
type FailClass string

const (
	VerdictOK FailClass = "OK"

	// Correctness (C1–C6).
	FailReplayMismatch   FailClass = "REPLAY-MISMATCH"   // C1
	FailAggregationDrift FailClass = "AGGREGATION-DRIFT" // C2
	FailUnitScale        FailClass = "UNIT-SCALE"        // C3
	FailStaleFrozen      FailClass = "STALE-FROZEN"      // C4
	FailMissingSeries    FailClass = "MISSING-SERIES"    // C5
	FailWindowArtifact   FailClass = "WINDOW-ARTIFACT"   // C6
	FailSpanmetricDrift  FailClass = "SPANMETRIC-DRIFT"  // §2.1 (trace AGGREGATION-DRIFT)

	// Readability (R1–R5).
	FailUnreadableLabel FailClass = "UNREADABLE-LABEL" // R1
	FailBlankEmpty      FailClass = "BLANK-EMPTY"      // R2
	FailScaleCrush      FailClass = "SCALE-CRUSH"      // R3
	FailNoContext       FailClass = "NO-CONTEXT"       // R4
	FailDeadEnd         FailClass = "DEAD-END"         // R5

	// Structural — a malformed ChartDef cannot be audited, so it cannot ship.
	FailMalformedDef FailClass = "MALFORMED-DEF"
)

// GateViolation is one failed gate for one panel. Gate is the rubric gate id
// ("C3", "R1", …); Class is the fail class; Detail explains the specific
// violation so the bug bead body is self-evident.
type GateViolation struct {
	Gate   string
	Class  FailClass
	Detail string
}

func (v GateViolation) String() string {
	return fmt.Sprintf("[%s %s] %s", v.Gate, v.Class, v.Detail)
}

// --- static gates over a ChartDef -------------------------------------------

// knownYFormats / knownMarks / knownIOSKinds are the closed vocabularies a
// catalog entry must draw from. Anything else is a typo that would render
// wrong, so it is a MALFORMED-DEF rather than a soft warning.
var (
	knownYFormats = map[string]bool{
		"currency_usd": true, "rate_per_hour": true, "rate": true,
		"count": true, "duration_ms": true, "duration_s": true,
		// duration_seconds is the spelling the perf-RED catalog entries (W2b,
		// ka-0000003074) and grafanaUnit() actually use; it was missing from
		// this allowlist, so TestCatalog_DataCorrectnessGate failed on main as
		// soon as those entries landed (surfaced fixing ka-0000008091).
		"duration_seconds": true,
		"percent":          true, "ratio": true, "bytes": true, "": true,
	}
	knownMarks    = map[string]bool{"line": true, "bar": true, "area": true}
	knownIOSKinds = map[string]bool{
		"line": true, "line-multi": true, "bar": true,
		"bar-stacked": true, "area-stacked": true, "area": true,
	}
)

// counterSuffixRe matches a metric name with a counter suffix (`_total`, or the
// `_usd_total` spend counters). A counter displayed raw (not wrapped in
// rate()/increase()) is the classic monotonic-ramp bug, so C3 requires every
// counter to be rate/increase-wrapped.
var counterSuffixRe = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*_total\b`)

// rawMetricNameRe matches identifiers that look like a Prometheus metric name
// the title should NOT be (R1): a snake_case token carrying a metric suffix.
var rawMetricNameRe = regexp.MustCompile(`\b[a-z_][a-z0-9_]*_(total|seconds|bucket|count|sum|usd|bytes|milliseconds)\b`)

// rangeSelectorRe finds rate()/increase()/<func>_over_time range selectors and
// captures the duration token inside the brackets, for C6.
var rangeSelectorRe = regexp.MustCompile(`(?:rate|irate|increase|delta|[a-z_]+_over_time)\s*\([^]]*\[([^]]+)\]`)

// bareRateRe matches a rate/increase call with no range selector at all — a
// hard C6 footgun (rate over an instant vector is meaningless / errors).
var bareRateRe = regexp.MustCompile(`(?:rate|irate|increase|delta)\s*\(\s*[a-zA-Z_:][a-zA-Z0-9_:]*(?:\{[^}]*\})?\s*\)`)

// auditChartDefStatic returns every static-gate violation for one ChartDef.
// Empty result ⇔ the def passes every statically-decidable gate.
func auditChartDefStatic(def ChartDef) []GateViolation {
	var vs []GateViolation
	add := func(gate string, class FailClass, format string, args ...any) {
		vs = append(vs, GateViolation{Gate: gate, Class: class, Detail: fmt.Sprintf(format, args...)})
	}

	// --- Structural integrity (MALFORMED-DEF): a def we cannot audit cannot
	// ship. These also catch the field-collision invariants the catalog
	// comments assert informally.
	if strings.TrimSpace(def.ID) == "" {
		add("struct", FailMalformedDef, "chart has empty ID")
	}
	if strings.TrimSpace(def.QueryTemplate) == "" {
		add("struct", FailMalformedDef, "chart %q has empty QueryTemplate", def.ID)
	}
	if def.XField == "" || def.ValueField == "" {
		add("struct", FailMalformedDef, "chart %q missing XField/ValueField", def.ID)
	}
	if def.XField == def.ValueField {
		add("struct", FailMalformedDef, "chart %q XField==ValueField (%q): x and value columns must differ", def.ID, def.XField)
	}
	if def.Category == "" || len(def.Surfaces) == 0 {
		add("struct", FailMalformedDef, "chart %q missing Category or Surfaces", def.ID)
	}
	if !knownMarks[def.Mark] {
		add("struct", FailMalformedDef, "chart %q unknown Mark %q (want line|bar|area)", def.ID, def.Mark)
	}
	if !knownYFormats[def.YFormat] {
		add("struct", FailMalformedDef, "chart %q unknown YFormat %q", def.ID, def.YFormat)
	}
	if def.IOSKind != "" && !knownIOSKinds[def.IOSKind] {
		add("struct", FailMalformedDef, "chart %q unknown IOSKind %q", def.ID, def.IOSKind)
	}
	// A keyed series needs a label to read the key off each returned series,
	// and vice versa — one without the other silently produces a single
	// unlabelled lump or an unused label.
	if (def.SeriesField == "") != (def.SeriesLabel == "") {
		add("struct", FailMalformedDef, "chart %q SeriesField/SeriesLabel must both be set or both empty (got field=%q label=%q)", def.ID, def.SeriesField, def.SeriesLabel)
	}
	// The "two measures never collide" invariant: each overlay's ValueField
	// must differ from the primary and from every other overlay, or a row's
	// measures overwrite each other.
	usedVals := map[string]string{def.ValueField: "primary"}
	for _, ex := range def.ExtraSeries {
		if ex.ValueField == "" || ex.QueryTemplate == "" || ex.Name == "" {
			add("struct", FailMalformedDef, "chart %q overlay missing Name/ValueField/QueryTemplate", def.ID)
			continue
		}
		if owner, dup := usedVals[ex.ValueField]; dup {
			add("struct", FailMalformedDef, "chart %q overlay %q reuses ValueField %q (already %s): measures would collide", def.ID, ex.Name, ex.ValueField, owner)
		}
		usedVals[ex.ValueField] = ex.Name
	}
	// The QueryTemplate (and overlays) must render with the substitution
	// context the handler uses; a parse/exec error would be a per-chart 500.
	for _, q := range allQueryTemplates(def) {
		if _, err := renderTemplate(q, sampleQueryContext()); err != nil {
			add("struct", FailMalformedDef, "chart %q query template does not render: %v", def.ID, err)
		}
	}

	// --- C2 Honest-aggregation: the active-agents silent-sum bug
	// (ka-0000002809). A panel that claims to count *active* agents but sums
	// gardenio_agents_active without excluding the non-producing states inflates
	// the count. Enforce: any query on gardenio_agents_active that is labelled
	// "active"/"running" must constrain `state`.
	for _, sq := range allSeriesQueries(def) {
		nameClaimsActive := mentionsActive(sq.label)
		if strings.Contains(sq.rendered, "gardenio_agents_active") && nameClaimsActive {
			if !strings.Contains(sq.rendered, "state!~") && !strings.Contains(sq.rendered, "state=~") && !strings.Contains(sq.rendered, "state=") {
				add("C2", FailAggregationDrift,
					"chart %q series %q claims ACTIVE agents but sums gardenio_agents_active without a state filter — would inflate by counting prewarmed/idle/queued (the ka-0000002809 bug):\n    %s",
					def.ID, sq.label, sq.rendered)
			}
		}
	}

	// --- C3 Unit-scale.
	for _, sq := range allSeriesQueries(def) {
		// A counter must never be displayed raw.
		if loc := counterSuffixRe.FindString(sq.rendered); loc != "" {
			wrapped := strings.Contains(sq.rendered, "rate(") || strings.Contains(sq.rendered, "irate(") ||
				strings.Contains(sq.rendered, "increase(") || strings.Contains(sq.rendered, "delta(")
			if !wrapped {
				add("C3", FailUnitScale,
					"chart %q series %q displays counter %q raw (no rate()/increase()) — a monotonic counter graphed directly is a ramp, not a rate:\n    %s",
					def.ID, sq.label, loc, sq.rendered)
			}
		}
		// currency_usd must read a USD series; a $ axis on a non-dollar series
		// is a unit lie.
		if sq.yformat == "currency_usd" && !strings.Contains(sq.rendered, "_usd") {
			add("C3", FailUnitScale,
				"chart %q series %q is formatted currency_usd but its query reads no *_usd series:\n    %s",
				def.ID, sq.label, sq.rendered)
		}
		// rate_per_hour must actually be a per-hour quantity: an increase()
		// divided by an hours count (the catalog's increase()/WindowHours form)
		// or a rate()*3600. A bare rate() formatted per-hour is off by 3600.
		if sq.yformat == "rate_per_hour" {
			perHour := dividesByHours(sq.rendered) || strings.Contains(sq.rendered, "3600")
			if !perHour {
				add("C3", FailUnitScale,
					"chart %q series %q is formatted rate_per_hour but the query is not a per-hour quantity (expected increase(...)/<hours> or rate(...)*3600):\n    %s",
					def.ID, sq.label, sq.rendered)
			}
		}
	}

	// --- C6 Window-integrity: no bare rate/increase, and every range selector
	// uses a sane window token (the parameterized {{.Step}}/{{.Window}} forms
	// scale with the request; a literal must parse as a duration).
	for _, q := range allQueryTemplates(def) {
		if loc := bareRateRe.FindString(q); loc != "" {
			add("C6", FailWindowArtifact,
				"chart %q has a rate/increase with no range selector %q — needs a [window]:\n    %s",
				def.ID, loc, q)
		}
		for _, m := range rangeSelectorRe.FindAllStringSubmatch(q, -1) {
			tok := strings.TrimSpace(m[1])
			if !validWindowToken(tok) {
				add("C6", FailWindowArtifact,
					"chart %q range selector window %q is neither a {{.Step}}/{{.Window}} template nor a parseable duration:\n    %s",
					def.ID, tok, q)
			}
		}
	}

	// --- R1 Legible-at-a-glance: the title is a claim, not a metric name.
	if rawMetricNameRe.MatchString(strings.ToLower(def.Title)) || strings.Contains(def.Title, "gardenio_") {
		add("R1", FailUnreadableLabel,
			"chart %q Title %q reads like a raw metric name — titles must state the question, not the series id", def.ID, def.Title)
	}

	// --- R4 Self-locating: axis label + (for keyed charts) a series legend
	// label, and a subtitle that carries window/units context.
	if strings.TrimSpace(def.YTitle) == "" {
		add("R4", FailNoContext, "chart %q has no YTitle — the axis is unlabelled", def.ID)
	}
	if strings.TrimSpace(def.Subtitle) == "" {
		add("R4", FailNoContext, "chart %q has no Subtitle — the panel carries no window/unit context", def.ID)
	}

	// --- R5 Drill-path: an aggregate panel must declare where it lives so a
	// reader can navigate from it (catalog charts are embedded on the surfaces
	// they list; an aggregate with nowhere to live is a dead end).
	if len(def.Surfaces) == 0 {
		add("R5", FailDeadEnd, "chart %q lists no Surfaces — no drill/embed path", def.ID)
	}

	// Stable order so test output and bug-bead bodies are deterministic.
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].Gate != vs[j].Gate {
			return vs[i].Gate < vs[j].Gate
		}
		return vs[i].Detail < vs[j].Detail
	})
	return vs
}

// seriesQuery is one rendered query on a chart (the primary or an overlay),
// paired with the legend label and y-format that apply to it — the unit the
// query is *displayed* as, which C2/C3 check against the query itself.
type seriesQuery struct {
	label    string
	rendered string
	yformat  string
}

// allSeriesQueries renders the primary and every overlay query against the
// sample context and pairs each with its display label + y-format.
func allSeriesQueries(def ChartDef) []seriesQuery {
	ctx := sampleQueryContext()
	out := make([]seriesQuery, 0, 1+len(def.ExtraSeries))
	primLabel := def.PrimarySeriesName
	if primLabel == "" {
		primLabel = def.Title
	}
	if r, err := renderTemplate(def.QueryTemplate, ctx); err == nil {
		out = append(out, seriesQuery{label: primLabel, rendered: r, yformat: def.YFormat})
	}
	for _, ex := range def.ExtraSeries {
		if r, err := renderTemplate(ex.QueryTemplate, ctx); err == nil {
			out = append(out, seriesQuery{label: ex.Name, rendered: r, yformat: ex.YFormat})
		}
	}
	return out
}

// allQueryTemplates returns the raw (unrendered) primary + overlay templates.
func allQueryTemplates(def ChartDef) []string {
	out := make([]string, 0, 1+len(def.ExtraSeries))
	out = append(out, def.QueryTemplate)
	for _, ex := range def.ExtraSeries {
		out = append(out, ex.QueryTemplate)
	}
	return out
}

// sampleQueryContext is a representative substitution context for static
// rendering — concrete enough that the rendered PromQL is realistic (a real
// step/window, an empty selector). Mirrors the handler's queryContext.
func sampleQueryContext() queryContext {
	return queryContext{Selector: "", Step: "5m", Window: "1d", WindowHours: "24", TopN: "10", Rollup: "role"}
}

func mentionsActive(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "active") || strings.Contains(l, "running")
}

// dividesByHours reports whether a rendered query divides by an hours quantity
// — the catalog's `increase(...)/{{.WindowHours}}` (rendered to `/24`, `/168`)
// or `/1` (the 1h companion). It is the per-hour-rate fingerprint for C3.
func dividesByHours(rendered string) bool {
	// Match `/ <number>` where the number is a plausible hours divisor.
	re := regexp.MustCompile(`/\s*(\d+(?:\.\d+)?)`)
	for _, m := range re.FindAllStringSubmatch(rendered, -1) {
		if m[1] != "" && m[1] != "0" {
			return true
		}
	}
	return false
}

// validWindowToken accepts the parameterized window placeholders the handler
// substitutes, or a literal Prometheus duration.
func validWindowToken(tok string) bool {
	switch tok {
	case "{{.Step}}", "{{.Window}}":
		return true
	}
	// Subquery form `<range>:<resolution>` (e.g. 1h:5m, {{.Window}}:5m). Both
	// halves must each be a valid window token — the range is the look-back, the
	// resolution is the aligned step the inner series is re-sampled on. Used by
	// the fleet-spend-rate / spend-by-account charts to dedup+de-bump the
	// LiteLLM account-cost gauge (ka-0000000170). A bare `:` or an empty half is
	// rejected by the recursion (empty string parses as neither template nor
	// duration).
	if r, res, ok := strings.Cut(tok, ":"); ok {
		return validWindowToken(strings.TrimSpace(r)) && validWindowToken(strings.TrimSpace(res))
	}
	// Already-rendered sample context turns {{.Step}}->5m, {{.Window}}->1d.
	if _, err := time.ParseDuration(promToGoDuration(tok)); err == nil {
		return true
	}
	// Prometheus day/week units (d, w, y) that Go's ParseDuration rejects.
	return regexp.MustCompile(`^\d+(?:\.\d+)?[smhdwy]$`).MatchString(tok)
}

// promToGoDuration maps the subset of Prometheus durations Go understands
// (s/m/h) straight through; d/w/y are handled by the regex in
// validWindowToken since Go's time.ParseDuration rejects them.
func promToGoDuration(tok string) string { return tok }

// TestCatalog_DataCorrectnessGate is the enforcement point: the live catalog
// must pass every statically-decidable rubric gate.
func TestCatalog_DataCorrectnessGate(t *testing.T) {
	cat := chartCatalog()
	if len(cat) == 0 {
		t.Fatal("chartCatalog() is empty — nothing to gate (catalog likely broke)")
	}
	for id, def := range cat {
		if vs := auditChartDefStatic(def); len(vs) > 0 {
			for _, v := range vs {
				t.Errorf("chart %q fails the data-correctness gate: %s", id, v)
			}
		}
	}
}

// TestGate_BitesOnBadDefs proves each static gate fires on a def that violates
// it — so a green TestCatalog_DataCorrectnessGate means the catalog is clean,
// not that the gate is asleep.
func TestGate_BitesOnBadDefs(t *testing.T) {
	// A valid baseline def the cases below mutate one gate at a time.
	base := func() ChartDef {
		return ChartDef{
			ID:            "x",
			Title:         "Spend per agent",
			Subtitle:      "USD per bucket",
			Category:      "spend",
			Surfaces:      []string{"dashboard"},
			QueryTemplate: `sum by(agent)(increase(gardenio_agent_spend_usd_total{}[{{.Step}}]))`,
			XField:        "t",
			ValueField:    "usd",
			SeriesField:   "agent",
			SeriesLabel:   "agent",
			Mark:          "bar",
			YTitle:        "USD",
			YFormat:       "currency_usd",
			IOSKind:       "bar-stacked",
		}
	}

	// Baseline must be clean, or the mutations below prove nothing.
	if vs := auditChartDefStatic(base()); len(vs) > 0 {
		t.Fatalf("baseline def should pass but got: %v", vs)
	}

	cases := []struct {
		name     string
		mutate   func(*ChartDef)
		want     FailClass
		wantGate string
	}{
		{
			name: "C2 active-agents silent sum (ka-0000002809)",
			mutate: func(d *ChartDef) {
				d.ExtraSeries = []ExtraSeriesDef{{
					Name: "active agents", ValueField: "agents", YFormat: "count",
					QueryTemplate: `sum(gardenio_agents_active{})`,
				}}
			},
			want:     FailAggregationDrift,
			wantGate: "C2",
		},
		{
			name:     "C3 raw counter, no rate()",
			mutate:   func(d *ChartDef) { d.QueryTemplate = `gardenio_agent_spend_usd_total{}` },
			want:     FailUnitScale,
			wantGate: "C3",
		},
		{
			name:     "C3 currency unit on non-USD series",
			mutate:   func(d *ChartDef) { d.QueryTemplate = `sum(rate(gardenio_beads_events_total{}[{{.Step}}]))` },
			want:     FailUnitScale,
			wantGate: "C3",
		},
		{
			name: "C3 per-hour format on a bare rate",
			mutate: func(d *ChartDef) {
				d.YFormat = "rate_per_hour"
				d.QueryTemplate = `sum(rate(gardenio_account_cost_usd{}[{{.Window}}]))`
			},
			want:     FailUnitScale,
			wantGate: "C3",
		},
		{
			name:     "C6 bare rate, no window",
			mutate:   func(d *ChartDef) { d.QueryTemplate = `sum(rate(gardenio_agent_spend_usd_total))` },
			want:     FailWindowArtifact,
			wantGate: "C6",
		},
		{
			name:     "R1 title is a raw metric name",
			mutate:   func(d *ChartDef) { d.Title = "gardenio_agent_spend_usd_total" },
			want:     FailUnreadableLabel,
			wantGate: "R1",
		},
		{
			name:     "R4 no axis label",
			mutate:   func(d *ChartDef) { d.YTitle = "" },
			want:     FailNoContext,
			wantGate: "R4",
		},
		{
			name:     "R4 no subtitle context",
			mutate:   func(d *ChartDef) { d.Subtitle = "" },
			want:     FailNoContext,
			wantGate: "R4",
		},
		{
			name:     "MALFORMED empty query",
			mutate:   func(d *ChartDef) { d.QueryTemplate = "" },
			want:     FailMalformedDef,
			wantGate: "struct",
		},
		{
			name:     "MALFORMED unknown unit",
			mutate:   func(d *ChartDef) { d.YFormat = "parsecs" },
			want:     FailMalformedDef,
			wantGate: "struct",
		},
		{
			name:     "MALFORMED series field without label",
			mutate:   func(d *ChartDef) { d.SeriesLabel = "" },
			want:     FailMalformedDef,
			wantGate: "struct",
		},
		{
			name: "MALFORMED overlay value collision",
			mutate: func(d *ChartDef) {
				d.ExtraSeries = []ExtraSeriesDef{{
					Name: "dup", ValueField: "usd", YFormat: "count",
					QueryTemplate: `sum(gardenio_agents_active{state!~"idle"})`,
				}}
			},
			want:     FailMalformedDef,
			wantGate: "struct",
		},
		{
			name:     "MALFORMED unrenderable template",
			mutate:   func(d *ChartDef) { d.QueryTemplate = `sum({{.Nope` },
			want:     FailMalformedDef,
			wantGate: "struct",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := base()
			tc.mutate(&d)
			vs := auditChartDefStatic(d)
			if len(vs) == 0 {
				t.Fatalf("expected gate %s/%s to fire, but def passed", tc.wantGate, tc.want)
			}
			found := false
			for _, v := range vs {
				if v.Class == tc.want && (tc.wantGate == "" || v.Gate == tc.wantGate) {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected violation %s/%s, got: %v", tc.wantGate, tc.want, vs)
			}
		})
	}
}
