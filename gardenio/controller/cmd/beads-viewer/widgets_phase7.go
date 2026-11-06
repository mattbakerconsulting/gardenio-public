package main

// Phase 7 — time-series widgets backed by uPlot.
// Plan-of-record: docs/research/incident-intelligence/synthesis.md §Phase 7.
// Bead: ka-0000005419.
//
// Two widgets land here:
//   #10 vm-sparklines  — stacked inline metric series per service (latency,
//                        error rate, etc.). Rendered as <canvas> per series
//                        with a JSON config the client picks up after
//                        uPlot loads.
//   #7  event-timeline — interleaved cross-source event timeline (logs,
//                        traces, deploys, alerts). Rendered as a single
//                        full-width chart with one point series per kind,
//                        with a fallback <ol> for SSR/no-JS.
//
// Hover sync: each chart broadcasts pointer-x to a shared bus
// (window.IncidentIntel.timeseries), and the other charts on the page move
// their cursor. D4 ships informational badge only — no cross-filter substrate.
//
// Server side stays small: the renderer normalises the payload, embeds it as
// a base64-safe JSON blob in `data-uplot-config`, and lets the client
// (incident_intel_uplot.js) instantiate uPlot once the bundle has loaded.
// Widgets degrade to a static fallback when JS is off.

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"sort"
	"strings"
	"time"
)

func init() {
	registerWidget("vm-sparklines", renderVMSparklines)
	registerWidget("event-timeline", renderEventTimeline)
}

// ---- vm-sparklines ---------------------------------------------------------

type sparkSeries struct {
	Service  string         `json:"service"`
	Metric   string         `json:"metric"`
	Unit     string         `json:"unit,omitempty"`
	State    string         `json:"state,omitempty"` // healthy/warn/critical -> ii-pill
	X        []int64        `json:"x"`               // unix seconds
	Y        []float64      `json:"y"`
	Latest   float64        `json:"latest"`
	Min      float64        `json:"min"`
	Max      float64        `json:"max"`
	ConfigID string         `json:"config_id"`
	Extras   map[string]any `json:"extras,omitempty"`
}

type vmSparklinesView struct {
	Title    string
	Subtitle string
	GroupID  string // hover-sync group
	Series   []sparkSeries
	Empty    bool
	// CrossFilterBadge is rendered as the informational D4 badge; when empty
	// the strip is omitted.
	CrossFilterBadge string
}

func renderVMSparklines(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := vmSparklinesView{
		Title:    widgetString(p, "title"),
		Subtitle: widgetString(p, "subtitle"),
		GroupID:  widgetString(p, "group_id"),
	}
	if v.GroupID == "" {
		v.GroupID = "ii-ts-default"
	}
	if badge, ok := p["cross_filter_badge"].(string); ok {
		v.CrossFilterBadge = strings.TrimSpace(badge)
	}
	for i, raw := range widgetObjectSlice(p, "series") {
		s := sparkSeries{
			Service: widgetString(raw, "service"),
			Metric:  widgetString(raw, "metric"),
			Unit:    widgetString(raw, "unit"),
			State:   strings.ToLower(widgetString(raw, "state")),
		}
		s.X, s.Y = parseTimePoints(raw)
		if len(s.X) == 0 {
			continue
		}
		s.Latest = s.Y[len(s.Y)-1]
		s.Min, s.Max = minMax(s.Y)
		s.ConfigID = fmt.Sprintf("ii-spark-%s-%d", v.GroupID, i)
		v.Series = append(v.Series, s)
	}
	v.Empty = len(v.Series) == 0
	return renderWidgetTemplate(ctx, "widget_vm_sparklines", v)
}

// ---- event-timeline --------------------------------------------------------

type timelineEvent struct {
	When       time.Time `json:"-"`
	WhenISO    string    `json:"when"`
	Unix       int64     `json:"unix"`
	Kind       string    `json:"kind"` // mezmo/jira/gitlab/slack/sentry/code/victoria-traces/victoria-metrics
	Title      string    `json:"title"`
	Detail     string    `json:"detail,omitempty"`
	URL        string    `json:"url,omitempty"`
	SourceLink string    `json:"source_link,omitempty"`
	Lane       int       `json:"lane"` // y-axis row, derived from kind
}

type eventTimelineView struct {
	Title    string
	Subtitle string
	GroupID  string
	Events   []timelineEvent
	Lanes    []timelineLane // ordered, deterministic
	Empty    bool
	// MinUnix/MaxUnix are surfaced into the JSON config as defaults; the
	// client uses them when no zoom range is provided.
	MinUnix          int64
	MaxUnix          int64
	CrossFilterBadge string
}

type timelineLane struct {
	Index    int    `json:"index"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Count    int    `json:"count"`
	IconHint string `json:"icon_hint,omitempty"`
}

func renderEventTimeline(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := eventTimelineView{
		Title:    widgetString(p, "title"),
		Subtitle: widgetString(p, "subtitle"),
		GroupID:  widgetString(p, "group_id"),
	}
	if v.GroupID == "" {
		v.GroupID = "ii-ts-default"
	}
	if badge, ok := p["cross_filter_badge"].(string); ok {
		v.CrossFilterBadge = strings.TrimSpace(badge)
	}
	raw := widgetObjectSlice(p, "events")
	laneIdx := map[string]int{}
	for _, r := range raw {
		ev := timelineEvent{
			Title:      widgetString(r, "title"),
			Detail:     widgetString(r, "detail"),
			URL:        widgetString(r, "url"),
			SourceLink: widgetString(r, "source_link"),
		}
		ev.Kind = sanitizeSourceKind(widgetString(r, "kind"))
		when := widgetString(r, "when")
		if t, ok := parseAnyTime(when); ok {
			ev.When = t
			ev.WhenISO = t.UTC().Format(time.RFC3339)
			ev.Unix = t.Unix()
		}
		if ev.Unix == 0 || ev.Title == "" {
			continue
		}
		if _, seen := laneIdx[ev.Kind]; !seen {
			laneIdx[ev.Kind] = len(laneIdx)
		}
		ev.Lane = laneIdx[ev.Kind]
		v.Events = append(v.Events, ev)
	}
	sort.Slice(v.Events, func(i, j int) bool { return v.Events[i].Unix < v.Events[j].Unix })
	if len(v.Events) > 0 {
		v.MinUnix = v.Events[0].Unix
		v.MaxUnix = v.Events[len(v.Events)-1].Unix
		// pad 5% on each side so endpoints aren't flush against the axis
		span := v.MaxUnix - v.MinUnix
		if span <= 0 {
			span = 60
		}
		pad := max(span/20, 30)
		v.MinUnix -= pad
		v.MaxUnix += pad
	}
	for kind, idx := range laneIdx {
		count := 0
		for _, ev := range v.Events {
			if ev.Kind == kind {
				count++
			}
		}
		v.Lanes = append(v.Lanes, timelineLane{
			Index: idx,
			Kind:  kind,
			Label: laneLabel(kind),
			Count: count,
		})
	}
	sort.Slice(v.Lanes, func(i, j int) bool { return v.Lanes[i].Index < v.Lanes[j].Index })
	v.Empty = len(v.Events) == 0
	return renderWidgetTemplate(ctx, "widget_event_timeline", v)
}

// ---- helpers ---------------------------------------------------------------

// sanitizeSourceKind constrains the value to the eight Phase 1 source-kind
// chips so the rendered chip's data attribute always picks up styling.
func sanitizeSourceKind(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "mezmo", "victoria-metrics", "victoria-traces", "sentry",
		"jira", "gitlab", "code", "slack":
		return s
	}
	return "code" // generic catch-all that has a chip
}

func laneLabel(kind string) string {
	switch kind {
	case "mezmo":
		return "Logs (Mezmo)"
	case "victoria-metrics":
		return "Metrics"
	case "victoria-traces":
		return "Traces"
	case "sentry":
		return "Errors (Sentry)"
	case "jira":
		return "Tickets"
	case "gitlab":
		return "Code (GitLab)"
	case "slack":
		return "Slack"
	case "code":
		return "Code"
	}
	return kind
}

// parseTimePoints turns a series payload's "points" array into x[]/y[].
// Accepts either {points:[{when,value}]} or parallel {x:[unix],y:[float]}
// arrays. Skips invalid entries.
func parseTimePoints(raw map[string]any) ([]int64, []float64) {
	if pts, ok := raw["points"].([]any); ok {
		xs := make([]int64, 0, len(pts))
		ys := make([]float64, 0, len(pts))
		for _, p := range pts {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			t, tok := parseAnyTime(widgetString(m, "when"))
			val, vok := numberField(m, "value")
			if !tok || !vok {
				continue
			}
			xs = append(xs, t.Unix())
			ys = append(ys, val)
		}
		return xs, ys
	}
	xRaw, _ := raw["x"].([]any)
	yRaw, _ := raw["y"].([]any)
	n := min(len(xRaw), len(yRaw))
	xs := make([]int64, 0, n)
	ys := make([]float64, 0, n)
	for i := range n {
		var x int64
		switch v := xRaw[i].(type) {
		case float64:
			x = int64(v)
		case string:
			if t, ok := parseAnyTime(v); ok {
				x = t.Unix()
			}
		}
		y, ok := xToFloat(yRaw[i])
		if x == 0 || !ok {
			continue
		}
		xs = append(xs, x)
		ys = append(ys, y)
	}
	return xs, ys
}

func numberField(m map[string]any, key string) (float64, bool) {
	return xToFloat(m[key])
}

func xToFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) {
			return 0, false
		}
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	}
	return 0, false
}

// parseAnyTime accepts RFC3339 strings or unix-seconds numerics.
func parseAnyTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func minMax(ys []float64) (float64, float64) {
	if len(ys) == 0 {
		return 0, 0
	}
	mn, mx := ys[0], ys[0]
	for _, y := range ys[1:] {
		if y < mn {
			mn = y
		}
		if y > mx {
			mx = y
		}
	}
	return mn, mx
}

// configJSON is what the client reads. Each chart has its own config blob.
type sparkConfigJSON struct {
	GroupID string    `json:"group_id"`
	Service string    `json:"service"`
	Metric  string    `json:"metric"`
	Unit    string    `json:"unit,omitempty"`
	X       []int64   `json:"x"`
	Y       []float64 `json:"y"`
	Min     float64   `json:"min"`
	Max     float64   `json:"max"`
	Extras  any       `json:"extras,omitempty"`
}

// MarshalSparkConfig is exported (well, package-internal) for the template's
// FuncMap to call on each series.
func MarshalSparkConfig(s sparkSeries, groupID string) string {
	cfg := sparkConfigJSON{
		GroupID: groupID,
		Service: s.Service,
		Metric:  s.Metric,
		Unit:    s.Unit,
		X:       s.X,
		Y:       s.Y,
		Min:     s.Min,
		Max:     s.Max,
		Extras:  s.Extras,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "{}"
	}
	return string(b)
}

type timelineConfigJSON struct {
	GroupID string          `json:"group_id"`
	MinUnix int64           `json:"min_unix"`
	MaxUnix int64           `json:"max_unix"`
	Lanes   []timelineLane  `json:"lanes"`
	Events  []timelineEvent `json:"events"`
}

func MarshalTimelineConfig(v eventTimelineView) string {
	cfg := timelineConfigJSON{
		GroupID: v.GroupID,
		MinUnix: v.MinUnix,
		MaxUnix: v.MaxUnix,
		Lanes:   v.Lanes,
		Events:  v.Events,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "{}"
	}
	return string(b)
}
