// Package grafanagrade holds the static detectors the Grafana GRADE audit
// (epic ka-0000008066, harness gardenio/docs/grafana-grade-audit-harness.md)
// runs over the chart-shipped dashboard JSON in gardenio/helm/gardenio/dashboards.
//
// The first detector here is the empty-branch liveness check. It replaces the
// "window-vs-cadence" check proposed in ka-0000002930 — flag a stat panel whose
// instant read is narrower than the emission cadence of the series it reads —
// which is wrong in both directions (ka-0000004423):
//
//   - It OVER-FIRES. A series absent between events is not necessarily
//     under-sampled. gardenio_agents_deferred_total is a GaugeVec the reconciler
//     Reset()s at the start of every pass and Inc()s once per deferred agent
//     (internal/reconciler/reconciler.go). Its absence is a semantically
//     meaningful real zero — "the last pass deferred nothing" — not a sampling
//     gap. Every correctly-written reset-gauge panel looks identical to a broken
//     one under a cadence check.
//
//   - It UNDER-FIRES. The detector's implied remedy is "widen the window", and
//     that remedy is itself the defect. Measured live 2026-09-02,
//     sum(last_over_time(gardenio_agents_deferred_total[6h])) rendered an orange
//     1 while the last reconcile pass deferred 0 and the newest sample was 57
//     min old. A windowed read does not recover a missing reading; it
//     resurrects a stale one and, on a stat with thresholds, presents it as
//     current. That panel passes a window-vs-cadence check while lying.
//
// The invariant that survives all three bad passes on that one panel is not
// about window width:
//
//	A panel's empty-result branch must be able to distinguish a real zero
//	from a blind emitter.
//
// Empty is reachable on essentially every panel. What makes it a defect is the
// panel ANSWERING that question anyway — coalescing empty to a confident zero,
// resurrecting a stale sample as the current value, or printing noValue text
// that names one of the two branches as fact — while the expression carries no
// independent liveness term to tell them apart.
//
// This subsumes the older standalone `or vector(0)` masking rule (ka-0000007164)
// rather than sitting beside it: an ungated `or vector(0)` is exactly the case
// where the empty branch was removed WITHOUT a liveness term, which is why it
// painted a confident green 0 over a dead gauge. Gated forms such as
// `... or (0 * (sum(increase(<control>[15m])) > 0))` are correct and are not
// flagged — there the zero exists only while the control is measurably
// advancing.
package grafanagrade

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Class names the shape of an empty-branch defect.
type Class string

const (
	// ClassUngatedZero: the expression coalesces an empty result into a
	// constant with no liveness term, so a blind emitter renders as a
	// confident number (ka-0000007164).
	ClassUngatedZero Class = "ungated-zero"

	// ClassStaleWindow: an instant-read panel resurrects the last sample
	// anywhere in the window and presents it as the current value
	// (ka-0000002930 pass 2).
	ClassStaleWindow Class = "stale-window"

	// ClassUnsupportedNoValue: the empty-branch text asserts which of the two
	// indistinguishable branches obtains — "counter not emitting", "no defer
	// events" — while the expression carries no liveness term that could
	// establish either (ka-0000002930 pass 1).
	ClassUnsupportedNoValue Class = "unsupported-novalue"

	// ClassNoValueClaimsZero: the expression DOES carry a liveness gate, but
	// the empty-branch text claims the real-zero reading. With a gate, a real
	// zero renders 0; noValue fires only when the gate itself is dark, so the
	// text describes a state it can never be shown in.
	ClassNoValueClaimsZero Class = "novalue-claims-zero"

	// ClassBlindEmpty (advisory): an instant-read panel with no liveness term
	// and no claim in its empty branch. Not a lie — the operator simply cannot
	// tell a real zero from a dead exporter. This is a worklist, not a gate.
	ClassBlindEmpty Class = "blind-empty"
)

// Severity separates defects (the panel asserts something it cannot know) from
// advisories (the panel is merely unable to answer the question).
type Severity string

const (
	SeverityDefect   Severity = "defect"
	SeverityAdvisory Severity = "advisory"
)

// Finding is one detector hit on one panel.
type Finding struct {
	Dashboard string   `json:"dashboard"`
	Panel     string   `json:"panel"`
	PanelType string   `json:"panel_type"`
	Class     Class    `json:"class"`
	Severity  Severity `json:"severity"`
	Detail    string   `json:"detail"`
	Expr      string   `json:"expr,omitempty"`
	NoValue   string   `json:"no_value,omitempty"`
}

func (f Finding) String() string {
	return fmt.Sprintf("%s / %q [%s] %s: %s", f.Dashboard, f.Panel, f.PanelType, f.Class, f.Detail)
}

// Key identifies a finding across runs, for baselining. It deliberately omits
// Detail and Expr so that rewording a message does not invalidate a baseline.
func (f Finding) Key() string {
	return f.Dashboard + "\x00" + f.Panel + "\x00" + string(f.Class)
}

// Dashboard is the slice of a Grafana dashboard model this detector reads.
type Dashboard struct {
	UID         string  `json:"uid"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Panels      []Panel `json:"panels"`
}

// Panel is the slice of a Grafana panel the detectors in this package read.
// Panels nest one level inside collapsed rows, so Panels is walked
// recursively. Fields past NoValue/Mappings are read by the richness scorer
// (richness.go) and the unit detector (unit.go) rather than the empty-branch
// detector.
type Panel struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Targets     []Target `json:"targets"`
	FieldConfig struct {
		Defaults struct {
			NoValue    string    `json:"noValue"`
			Unit       string    `json:"unit"`
			Mappings   []Mapping `json:"mappings"`
			Thresholds struct {
				Steps []struct {
					Value *float64 `json:"value"`
				} `json:"steps"`
			} `json:"thresholds"`
		} `json:"defaults"`
		// Overrides carry per-FIELD config. On a table whose columns are
		// heterogeneous, this — not Defaults — is where units legitimately
		// live (unit.go).
		Overrides []Override `json:"overrides"`
	} `json:"fieldConfig"`
	Options struct {
		Legend struct {
			Calcs []string `json:"calcs"`
		} `json:"legend"`
		// YAxis is the heatmap bucket axis, which carries its own unit
		// outside fieldConfig entirely (unit.go).
		YAxis struct {
			Unit string `json:"unit"`
		} `json:"yAxis"`
	} `json:"options"`
	Transformations []Transformation `json:"transformations"`
	Panels          []Panel          `json:"panels"`
}

// Target is one query on a panel.
type Target struct {
	Expr       string          `json:"expr"`
	RefID      string          `json:"refId"`
	Hide       bool            `json:"hide"`
	Datasource json.RawMessage `json:"datasource"`
}

// Override is one per-field config override: a matcher naming the fields and
// the properties applied to them. Only the property IDs are read here.
type Override struct {
	Matcher struct {
		ID string `json:"id"`
	} `json:"matcher"`
	Properties []struct {
		ID string `json:"id"`
	} `json:"properties"`
}

// Transformation is one panel transformation. Only `organize`'s field-exclusion
// map is read (unit.go), to spot a table that drops its numeric column.
type Transformation struct {
	ID      string `json:"id"`
	Options struct {
		ExcludeByName map[string]bool `json:"excludeByName"`
	} `json:"options"`
}

// Mapping is a Grafana value mapping. Only the `special` mappings that match
// null carry empty-branch text.
type Mapping struct {
	Type    string `json:"type"`
	Options struct {
		Match  string `json:"match"`
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
	} `json:"options"`
}

// ParseDashboard decodes a Grafana dashboard model.
func ParseDashboard(raw []byte) (*Dashboard, error) {
	var d Dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("parse dashboard: %w", err)
	}
	return &d, nil
}

// instantReadPanels render a single current value, so a resurrected or
// coalesced number is read as "right now". Time-series panels show the gap
// itself, which is why the stale-window and blind-empty rules are scoped here.
var instantReadPanels = map[string]bool{
	"stat": true, "gauge": true, "singlestat": true, "bargauge": true,
}

// emitterHealthClaims are noValue phrasings that assert the emitter's own
// health — the claim an empty vector cannot support in either direction.
//
// Deliberately NOT included: statements about deployment or scrape
// configuration ("recommender not deployed", "perf-federation scrape paused",
// "pending federation"). Those name an infrastructure state the author knows
// out of band and dates with a bead; they are not inferences drawn from the
// empty result.
var emitterHealthClaims = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bnot emitting\b`),
	regexp.MustCompile(`(?i)\bno samples\b`),
	regexp.MustCompile(`(?i)\bstale\b`),
	regexp.MustCompile(`(?i)\bdark counter\b`),
	regexp.MustCompile(`(?i)\bdead counter\b`),
	regexp.MustCompile(`(?i)\bemitter (is )?(live|dead|dark|idle|down)\b`),
	regexp.MustCompile(`(?i)\bemitter-idle\b`),
	regexp.MustCompile(`(?i)\bcheck server is up\b`),
	regexp.MustCompile(`(?i)\bno series\b`),
}

// realZeroClaims are noValue phrasings that assert the OTHER branch — that the
// measured thing genuinely did not happen. Equally unobservable from an empty
// vector, and additionally impossible to display on a gated panel, where a real
// zero renders 0 and noValue fires only when the gate is dark.
var realZeroClaims = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bno \w+([ -]\w+)? (events|calls|searches|errors|activity|traffic|spawns|resumes|deferrals|writes|evaluations|attempts|hits)\b`),
	regexp.MustCompile(`(?i)\bno (defer|spawn|thaw|freeze|embed|search|scrape) `),
	regexp.MustCompile(`(?i)\b0 in window\b`),
	regexp.MustCompile(`(?i)\bhealthy (state|0|zero)\b`),
	regexp.MustCompile(`(?i)—\s*healthy\b`),
	regexp.MustCompile(`(?i)\bnothing (deferred|queued|happened)\b`),
}

// resurrectingSumRe matches the shape that presents a windowed read as the
// current value: a FLEET TOTAL (sum/max/min across series) of last_over_time.
//
// The narrower match is deliberate. `count(last_over_time(m[$__range]))`,
// `avg(last_over_time(...))` and `quantile(0.95, last_over_time(...))` are
// population statistics ACROSS the window — "sessions in window", "boot p95" —
// where collapsing each series to its last sample is the point. Flagging those
// would repeat the over-firing that made the window-vs-cadence check unusable:
// a legitimate idiom and a defect look identical unless the panel is claiming
// the number is current. A sum/max/min over last_over_time IS that claim.
var resurrectingSumRe = regexp.MustCompile(
	`\b(sum|max|min)\s*(by\s*\([^)]*\)\s*|without\s*\([^)]*\)\s*)?\(\s*last_over_time\s*\(`)

// livenessGate reports whether expr carries an independent liveness term: a
// direct `up` scrape signal, or a gate branch that filters on an always-present
// sibling series the primary read does not itself depend on. The comparison is
// required — `0 * (sum(increase(c[15m])) > 0)` only yields its zero while c is
// measurably advancing, whereas `0 * c` would inherit c's own blindness.
func livenessGate(expr string) (bool, string) {
	if metricNames(expr)["up"] {
		return true, "reads the `up` scrape signal directly"
	}
	for _, branch := range gateOperands(expr) {
		if !comparisonRe.MatchString(branch) {
			continue
		}
		rest := strings.Replace(expr, branch, " ", 1)
		restMetrics := metricNames(rest)
		for m := range metricNames(branch) {
			if !restMetrics[m] {
				return true, fmt.Sprintf("gated on independent control %s", m)
			}
		}
	}
	return false, ""
}

// emptyBranchText is every string the panel can render in place of a value:
// noValue plus the text of any `special` mapping that matches null.
func emptyBranchText(p *Panel) []string {
	var out []string
	if s := strings.TrimSpace(p.FieldConfig.Defaults.NoValue); s != "" {
		out = append(out, s)
	}
	for _, m := range p.FieldConfig.Defaults.Mappings {
		if m.Type != "special" {
			continue
		}
		if !strings.Contains(strings.ToLower(m.Options.Match), "null") {
			continue
		}
		if s := strings.TrimSpace(m.Options.Result.Text); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func matchAny(res []*regexp.Regexp, texts []string) (string, bool) {
	for _, t := range texts {
		for _, re := range res {
			if m := re.FindString(t); m != "" {
				return strings.TrimSpace(m), true
			}
		}
	}
	return "", false
}

// promTarget reports whether a target is PromQL replayable. LogsQL/traces/Jaeger
// targets speak a different language and are excluded (ka-0000000446).
func promTarget(t Target) bool {
	if t.Hide || strings.TrimSpace(t.Expr) == "" {
		return false
	}
	if len(t.Datasource) == 0 {
		return true
	}
	var ds struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(t.Datasource, &ds); err != nil {
		return true
	}
	switch strings.ToLower(ds.Type) {
	case "jaeger", "loki", "tempo", "elasticsearch",
		"victoriametrics-logs-datasource", "victoriametrics-traces-datasource":
		return false
	}
	return true
}

// CheckPanel runs the empty-branch liveness detector over one panel and returns
// its findings, most severe shape first. dashboard is used only to label them.
func CheckPanel(dashboard string, p *Panel) []Finding {
	var exprs []string
	for _, t := range p.Targets {
		if promTarget(t) {
			exprs = append(exprs, t.Expr)
		}
	}
	if len(exprs) == 0 {
		return nil
	}
	joined := strings.Join(exprs, " ; ")
	texts := emptyBranchText(p)
	instant := instantReadPanels[strings.ToLower(p.Type)]

	gated := false
	for _, e := range exprs {
		if ok, _ := livenessGate(e); ok {
			gated = true
			break
		}
	}

	find := func(class Class, sev Severity, detail string) Finding {
		return Finding{
			Dashboard: dashboard,
			Panel:     p.Title,
			PanelType: p.Type,
			Class:     class,
			Severity:  sev,
			Detail:    detail,
			Expr:      joined,
			NoValue:   strings.Join(texts, " | "),
		}
	}

	var out []Finding

	// 1. Ungated zero — the empty branch was removed, not answered.
	if !gated {
		for _, e := range exprs {
			for _, site := range orSites(e) {
				if !isConstantBranch(site.RHS) {
					continue
				}
				detail := fmt.Sprintf(
					"`or %s` coalesces an empty result into a constant with no liveness term, "+
						"so a blind emitter renders as a confident number (ka-0000007164)",
					strings.TrimSpace(unwrapParens(site.RHS)))
				if len(texts) > 0 {
					detail += "; the empty-branch text can never render behind the coalesce"
				}
				out = append(out, find(ClassUngatedZero, SeverityDefect, detail))
				break
			}
			if len(out) > 0 {
				break
			}
		}
	}

	// 2. Stale window — a value from anywhere in the window shown as current.
	if instant && !gated {
		for _, e := range exprs {
			if resurrectingSumRe.MatchString(e) {
				out = append(out, find(ClassStaleWindow, SeverityDefect,
					"a fleet total over `last_over_time` on an instant-read panel resurrects the last "+
						"sample anywhere in the window and presents it as the current value; widening a "+
						"window does not recover a missing reading, it re-dates a stale one "+
						"(ka-0000002930)"))
				break
			}
		}
	}

	// 3/4. Empty-branch text that names one of the two branches as fact.
	if claim, ok := matchAny(emitterHealthClaims, texts); ok && !gated {
		out = append(out, find(ClassUnsupportedNoValue, SeverityDefect, fmt.Sprintf(
			"empty-branch text asserts the emitter's health (matched %q), which an empty result "+
				"cannot establish — add a liveness term or drop the claim (ka-0000002930)", claim)))
	}
	if claim, ok := matchAny(realZeroClaims, texts); ok {
		if gated {
			out = append(out, find(ClassNoValueClaimsZero, SeverityDefect, fmt.Sprintf(
				"empty-branch text asserts a real-zero reading (matched %q), but the panel is gated: "+
					"a real zero renders 0, and this text fires only when the gate itself is dark", claim)))
		} else {
			out = append(out, find(ClassUnsupportedNoValue, SeverityDefect, fmt.Sprintf(
				"empty-branch text asserts a real-zero reading (matched %q), which an empty result "+
					"cannot establish without a liveness term (ka-0000002930)", claim)))
		}
	}

	// 5. Advisory — cannot answer the question, but does not pretend to.
	if instant && !gated && len(out) == 0 {
		out = append(out, find(ClassBlindEmpty, SeverityAdvisory,
			"instant read with no liveness term: an empty result is indistinguishable from a "+
				"dead exporter, and the panel offers the operator no way to tell"))
	}

	// One finding per (panel, class): a panel whose empty-branch text asserts
	// both a dead emitter and a real zero has one defect to fix, not two, and
	// Key() — what the baseline is keyed on — cannot tell the copies apart.
	seen := make(map[string]bool, len(out))
	deduped := out[:0]
	for _, f := range out {
		if seen[f.Key()] {
			continue
		}
		seen[f.Key()] = true
		deduped = append(deduped, f)
	}
	return deduped
}

// CheckDashboard runs the detector over every panel of a dashboard, including
// panels nested inside rows.
func CheckDashboard(name string, d *Dashboard) []Finding {
	var out []Finding
	var walk func([]Panel)
	walk = func(ps []Panel) {
		for i := range ps {
			if strings.EqualFold(ps[i].Type, "row") {
				walk(ps[i].Panels)
				continue
			}
			out = append(out, CheckPanel(name, &ps[i])...)
			walk(ps[i].Panels)
		}
	}
	walk(d.Panels)
	return out
}

// Defects filters findings down to the gating severity.
func Defects(in []Finding) []Finding {
	var out []Finding
	for _, f := range in {
		if f.Severity == SeverityDefect {
			out = append(out, f)
		}
	}
	return out
}
