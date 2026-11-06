package main

// Operator preview surface for free-htmx widget-type beads.
//
// Plan-of-record: ka-0000001812 (synthesis runbook
// gardenio/docs/research/free-htmx-widget-authoring-runbook.md). Implements
// section (4) of the plan: /widget-types index + /widget-types/{slug}
// tabbed page (Overview / Preview / Diagnostics / History) + the htmx
// fragment endpoints that back the Preview and Diagnostics tabs +
// DRAFT/DEPRECATED chips on rich_report.go widget cards.

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// widgetStage values mirror the lifecycle vocab defined by the synthesis
// decision (ka-0000006220). The plan recommends three stages — `draft →
// stable → deprecated`. We accept both `published` and `stable` since the
// synthesis runbook landed on `stable`; the chip text is normalized.
const (
	widgetStageDraft      = "draft"
	widgetStageStable     = "stable"
	widgetStagePublished  = "published"
	widgetStageDeprecated = "deprecated"
)

// widgetStageLabelPrefix is the label prefix used by `gb widget` (see
// gardenio/controller/cmd/gb/widget.go) to record the lifecycle stage on a
// widget-type bead.
const widgetStageLabelPrefix = "widget-stage:"

// widgetTypesListLimit caps results pulled from the daemon for the
// /widget-types index. 500 is the same cap saved-views uses; widget-types
// are unlikely to grow past that for years (catalog sprawl is the #1
// open question, but the plan caps it via `gb widget similar` reuse).
const widgetTypesListLimit = 500

// widgetDiagnosticsRingSize bounds the per-slug error ring buffer (plan
// ka-0000001812 §(9) — open question 3 recommends 50). Errors evict
// FIFO so one noisy widget can't crowd out diagnostics for the rest.
const widgetDiagnosticsRingSize = 50

// widgetTypeRow is one row on the /widget-types/ index page.
type widgetTypeRow struct {
	BeadID      string
	Slug        string
	Version     string
	Summary     string
	Kind        string // "htmx" or "builtin"
	Stage       string // normalized: draft / stable / deprecated / ""
	StageChip   string // human label for the chip
	StageClass  string // css class suffix
	Title       string
	Status      string
	UpdatedAgo  string
	Href        string
}

// widgetTypeIndexView is the top-level view model for /widget-types/.
type widgetTypeIndexView struct {
	Rows         []widgetTypeRow
	KindFilter   string // "", "htmx", "builtin"
	StageFilter  string // "", "draft", "stable", "deprecated"
	KindChips    []widgetTypeFilterChip
	StageChips   []widgetTypeFilterChip
	Truncated    bool
	Limit        int
	Total        int
	Error        string
}

// widgetTypeFilterChip is one chip in a horizontal filter strip on the
// index page. Mirrors the canonical filterChip (filter_strip.go) so the
// existing chip CSS applies without a new rule set. (This page still renders
// its strip inline; folding it onto the shared filter_strip partial is a
// follow-up, tracked under the F6 consolidation ka-0000005357.)
type widgetTypeFilterChip struct {
	Label  string
	Value  string
	Href   string
	Active bool
}

// widgetTypeView is the per-bead view model for /widget-types/{slug}. The
// Tab field selects one of overview/preview/diagnostics/history; the
// preview/diagnostics partials are also served standalone via the
// /htmx/widget-types/{slug}/{preview,diagnostics} fragment endpoints.
type widgetTypeView struct {
	BeadID         string
	Slug           string
	Version        string
	Summary        string
	Kind           string
	Stage          string
	StageChip      string
	StageClass     string
	TemplateSource string
	SchemaJSON     string
	RenderEndpoint string
	HasTemplate    bool
	Tab            string
	Tabs           []widgetTypeTab
	Preview        *widgetTypePreviewView
	Diagnostics    *widgetTypeDiagnosticsView
	History        []widgetTypeHistoryRow
	BeadURL        string
}

// widgetTypeTab is one tab in the /widget-types/{slug} tab strip. Active
// marks the currently-selected tab so the template can apply a styling
// override without per-tab branches.
type widgetTypeTab struct {
	Label  string
	Slug   string
	Href   string
	Active bool
}

// widgetTypePreviewView is the htmx fragment payload behind the Preview
// tab. RenderedHTML is the template_source executed against PayloadJSON;
// when execution fails, ErrorClass + ErrorMsg are set and RenderedHTML
// is empty so the template can render the diagnostic chip in place.
type widgetTypePreviewView struct {
	Slug         string
	PayloadJSON  string
	PayloadEdit  string
	RenderedHTML template.HTML
	ErrorClass   string // "compile" | "render" | "data" | "trust" | ""
	ErrorMsg     string
	HasTemplate  bool
}

// widgetTypeDiagnosticsView is the htmx fragment payload behind the
// Diagnostics tab — the per-slug ring buffer + the sanitizer-rejection
// histogram. SanitizerHistogram is keyed by error class so the template
// can render a small bar chart without sorting in the view layer.
type widgetTypeDiagnosticsView struct {
	Slug              string
	Errors            []widgetDiagnosticEntry
	SanitizerCount    int
	CompileCount      int
	RenderCount       int
	DataCount         int
	Total             int
}

// widgetDiagnosticEntry is one row in the diagnostics ring buffer.
type widgetDiagnosticEntry struct {
	When     time.Time
	Class    string // compile | sanitizer | data | render
	Message  string
	WhenAgo  string
}

// widgetTypeHistoryRow is one row in the History tab — it summarizes a
// widget-type bead's version history. v1 lists every widget-type bead
// that shares the same slug (regardless of widget_version) ordered by
// version descending; later phases can wire in the bead audit-log.
type widgetTypeHistoryRow struct {
	BeadID    string
	Version   string
	Stage     string
	StageChip string
	UpdatedAgo string
	Href       string
}

// widgetDiagnostics is a per-slug ring buffer of recent render errors,
// shared across requests. Held on *Server (see newWidgetDiagnostics).
// Plan §(5) calls out four error classes — compile / sanitizer
// rejection / schema mismatch / render-time panic — surfaced as a single
// inline chip on rich-report bead-show. This struct stores them per slug
// and exposes them via /htmx/widget-types/{slug}/diagnostics.
type widgetDiagnostics struct {
	mu      sync.Mutex
	bySlug  map[string][]widgetDiagnosticEntry
}

func newWidgetDiagnostics() *widgetDiagnostics {
	return &widgetDiagnostics{bySlug: make(map[string][]widgetDiagnosticEntry)}
}

// Record appends a new diagnostic entry for the widget-type slug,
// trimming the ring buffer to widgetDiagnosticsRingSize entries (FIFO).
// Safe for concurrent use.
func (d *widgetDiagnostics) Record(slug, class, message string) {
	if d == nil || slug == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	entry := widgetDiagnosticEntry{When: time.Now(), Class: class, Message: message}
	buf := d.bySlug[slug]
	buf = append(buf, entry)
	if len(buf) > widgetDiagnosticsRingSize {
		buf = buf[len(buf)-widgetDiagnosticsRingSize:]
	}
	d.bySlug[slug] = buf
}

// Snapshot returns a copy of the ring buffer for slug, newest first.
func (d *widgetDiagnostics) Snapshot(slug string) []widgetDiagnosticEntry {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	buf := d.bySlug[slug]
	if len(buf) == 0 {
		return nil
	}
	out := make([]widgetDiagnosticEntry, len(buf))
	for i, e := range buf {
		out[len(buf)-1-i] = e // reverse for newest-first
	}
	return out
}

// widgetStageFromLabels picks the first widget-stage:* label and
// normalizes synonyms (`published` → `stable`). Returns "" when no
// stage label is set so callers can render the empty case.
func widgetStageFromLabels(labels []string) string {
	for _, l := range labels {
		v, ok := strings.CutPrefix(l, widgetStageLabelPrefix)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(v)) {
		case widgetStageDraft:
			return widgetStageDraft
		case widgetStageStable, widgetStagePublished:
			return widgetStageStable
		case widgetStageDeprecated:
			return widgetStageDeprecated
		default:
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	return ""
}

// widgetStageChipText returns the human-facing chip label. The plan
// emphasises DRAFT and DEPRECATED chips; STABLE renders quietly because
// it is the default expected state.
func widgetStageChipText(stage string) string {
	switch stage {
	case widgetStageDraft:
		return "DRAFT"
	case widgetStageDeprecated:
		return "DEPRECATED"
	case widgetStageStable:
		return "stable"
	case "":
		return ""
	default:
		return strings.ToUpper(stage)
	}
}

// widgetStageChipClass returns the CSS class suffix for the stage chip.
// Mapped onto the existing chip-stage-* CSS rules (see layout.html).
func widgetStageChipClass(stage string) string {
	switch stage {
	case widgetStageDraft:
		return "draft"
	case widgetStageDeprecated:
		return "deprecated"
	case widgetStageStable:
		return "stable"
	default:
		return ""
	}
}

// widgetTypeRendererKind extracts the renderer_kind field. Defaults to
// "builtin" when the field is missing — pre-P1 widget-type beads predate
// the field and were all builtin.
func widgetTypeRendererKind(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	if v := strings.TrimSpace(b.Fields["renderer_kind"]); v != "" {
		return v
	}
	return "builtin"
}

// listWidgetTypeBeads queries the daemon for every widget-type bead.
// Filters apply at the HTTP layer; renderer_kind / stage filters apply
// in-process because they're not yet first-class query params.
func (s *Server) listWidgetTypeBeads(ctx context.Context) ([]*beadsapi.BeadDetail, bool, error) {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"widget-type"},
		Kinds: []string{"data"},
		Limit: widgetTypesListLimit,
	})
	if err != nil {
		return nil, false, err
	}
	return res.Beads, res.Total > len(res.Beads), nil
}

// findWidgetTypeBySlug returns the widget-type bead with the matching
// slug, preferring beads with widget_version set (newest-first by
// version string). Returns nil if not found.
func (s *Server) findWidgetTypeBySlug(ctx context.Context, slug string) (*beadsapi.BeadDetail, []*beadsapi.BeadDetail, error) {
	beads, _, err := s.listWidgetTypeBeads(ctx)
	if err != nil {
		return nil, nil, err
	}
	var matches []*beadsapi.BeadDetail
	for _, b := range beads {
		if b == nil {
			continue
		}
		if strings.EqualFold(b.Fields["slug"], slug) {
			matches = append(matches, b)
		}
	}
	if len(matches) == 0 {
		return nil, nil, nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Fields["widget_version"] > matches[j].Fields["widget_version"]
	})
	return matches[0], matches, nil
}

// handleWidgetTypesIndex renders the /widget-types/ gallery page. Filters
// are read from the query string (kind=htmx|builtin, stage=draft|stable|
// deprecated). The page lists every widget-type bead currently visible to
// the daemon.
func (s *Server) handleWidgetTypesIndex(w http.ResponseWriter, r *http.Request) {
	kindFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	stageFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("stage")))

	beads, truncated, err := s.listWidgetTypeBeads(r.Context())
	view := widgetTypeIndexView{
		KindFilter:  kindFilter,
		StageFilter: stageFilter,
		Limit:       widgetTypesListLimit,
		Truncated:   truncated,
		KindChips:   widgetTypeKindChips(s.basePath, kindFilter, stageFilter),
		StageChips:  widgetTypeStageChips(s.basePath, kindFilter, stageFilter),
	}
	if err != nil {
		view.Error = err.Error()
		s.render(w, r, "widget_types_index.html", map[string]any{"Index": view})
		return
	}
	for _, b := range beads {
		if b == nil {
			continue
		}
		row := widgetTypeRowFromBead(s.basePath, b)
		if kindFilter != "" && row.Kind != kindFilter {
			continue
		}
		if stageFilter != "" && row.Stage != stageFilter {
			continue
		}
		view.Rows = append(view.Rows, row)
	}
	view.Total = len(view.Rows)
	sort.SliceStable(view.Rows, func(i, j int) bool {
		if view.Rows[i].Slug != view.Rows[j].Slug {
			return view.Rows[i].Slug < view.Rows[j].Slug
		}
		return view.Rows[i].Version > view.Rows[j].Version
	})
	s.render(w, r, "widget_types_index.html", map[string]any{"Index": view})
}

// widgetTypeRowFromBead builds an index-row view from a widget-type
// bead, consolidating field+label parsing in one place.
func widgetTypeRowFromBead(basePath string, b *beadsapi.BeadDetail) widgetTypeRow {
	stage := widgetStageFromLabels(b.Labels)
	slug := b.Fields["slug"]
	href := basePath + "/widget-types/" + url.PathEscape(slug)
	return widgetTypeRow{
		BeadID:     b.ID,
		Slug:       slug,
		Version:    b.Fields["widget_version"],
		Summary:    b.Fields["summary"],
		Kind:       widgetTypeRendererKind(b),
		Stage:      stage,
		StageChip:  widgetStageChipText(stage),
		StageClass: widgetStageChipClass(stage),
		Title:      b.Title,
		Status:     b.Status,
		UpdatedAgo: humanizeTime(b.UpdatedAt),
		Href:       href,
	}
}

func widgetTypeKindChips(basePath, currentKind, currentStage string) []widgetTypeFilterChip {
	values := []struct{ label, value string }{
		{"All kinds", ""}, {"htmx", "htmx"}, {"builtin", "builtin"},
	}
	chips := make([]widgetTypeFilterChip, 0, len(values))
	for _, v := range values {
		params := url.Values{}
		if v.value != "" {
			params.Set("kind", v.value)
		}
		if currentStage != "" {
			params.Set("stage", currentStage)
		}
		href := basePath + "/widget-types/"
		if q := params.Encode(); q != "" {
			href += "?" + q
		}
		chips = append(chips, widgetTypeFilterChip{
			Label:  v.label,
			Value:  v.value,
			Href:   href,
			Active: v.value == currentKind,
		})
	}
	return chips
}

func widgetTypeStageChips(basePath, currentKind, currentStage string) []widgetTypeFilterChip {
	values := []struct{ label, value string }{
		{"All stages", ""}, {"draft", "draft"}, {"stable", "stable"}, {"deprecated", "deprecated"},
	}
	chips := make([]widgetTypeFilterChip, 0, len(values))
	for _, v := range values {
		params := url.Values{}
		if currentKind != "" {
			params.Set("kind", currentKind)
		}
		if v.value != "" {
			params.Set("stage", v.value)
		}
		href := basePath + "/widget-types/"
		if q := params.Encode(); q != "" {
			href += "?" + q
		}
		chips = append(chips, widgetTypeFilterChip{
			Label:  v.label,
			Value:  v.value,
			Href:   href,
			Active: v.value == currentStage,
		})
	}
	return chips
}

// handleWidgetTypeShow renders /widget-types/{slug} — the tabbed type-bead
// page. The active tab is selected by ?tab=overview|preview|diagnostics|
// history; default is overview. Preview and Diagnostics also render
// inline (server-side) so the page is functional without JavaScript; the
// htmx fragment endpoints upgrade them with auto-refresh.
func (s *Server) handleWidgetTypeShow(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}
	tab := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tab")))
	if tab == "" {
		tab = "overview"
	}

	bead, history, err := s.findWidgetTypeBySlug(r.Context(), slug)
	if err != nil {
		s.render(w, r, "widget_type_show.html", map[string]any{
			"WidgetType": &widgetTypeView{Slug: slug, Tab: tab},
			"Error":      err.Error(),
		})
		return
	}
	if bead == nil {
		s.render(w, r, "widget_type_show.html", map[string]any{
			"WidgetType": &widgetTypeView{Slug: slug, Tab: tab},
			"Error":      "no widget-type bead found with slug=" + slug,
		})
		return
	}

	view := buildWidgetTypeView(s.basePath, bead, history, tab)
	switch tab {
	case "preview":
		view.Preview = s.buildPreview(r.Context(), bead, r.URL.Query().Get("payload"))
	case "diagnostics":
		view.Diagnostics = s.buildDiagnostics(slug)
	case "history":
		// History rendered from the slice already attached.
	default:
		// Overview also pre-builds a small preview so the user sees one
		// without clicking through. A render error degrades to placeholder.
		view.Preview = s.buildPreview(r.Context(), bead, "")
	}

	s.render(w, r, "widget_type_show.html", map[string]any{"WidgetType": view})
}

// buildWidgetTypeView assembles the per-bead view model.
func buildWidgetTypeView(basePath string, bead *beadsapi.BeadDetail, history []*beadsapi.BeadDetail, activeTab string) *widgetTypeView {
	stage := widgetStageFromLabels(bead.Labels)
	slug := bead.Fields["slug"]
	view := &widgetTypeView{
		BeadID:         bead.ID,
		Slug:           slug,
		Version:        bead.Fields["widget_version"],
		Summary:        bead.Fields["summary"],
		Kind:           widgetTypeRendererKind(bead),
		Stage:          stage,
		StageChip:      widgetStageChipText(stage),
		StageClass:     widgetStageChipClass(stage),
		TemplateSource: bead.Fields["template_source"],
		SchemaJSON:     prettyJSON(bead.Fields["schema"]),
		RenderEndpoint: bead.Fields["render_endpoint"],
		HasTemplate:    strings.TrimSpace(bead.Fields["template_source"]) != "",
		Tab:            activeTab,
		BeadURL:        basePath + "/" + bead.ID,
	}
	view.Tabs = widgetTypeTabs(basePath, slug, activeTab)
	for _, h := range history {
		if h == nil {
			continue
		}
		histStage := widgetStageFromLabels(h.Labels)
		view.History = append(view.History, widgetTypeHistoryRow{
			BeadID:     h.ID,
			Version:    h.Fields["widget_version"],
			Stage:      histStage,
			StageChip:  widgetStageChipText(histStage),
			UpdatedAgo: humanizeTime(h.UpdatedAt),
			Href:       basePath + "/" + h.ID,
		})
	}
	return view
}

func widgetTypeTabs(basePath, slug, activeTab string) []widgetTypeTab {
	defs := []struct{ label, slugv string }{
		{"Overview", "overview"},
		{"Preview", "preview"},
		{"Diagnostics", "diagnostics"},
		{"History", "history"},
	}
	tabs := make([]widgetTypeTab, 0, len(defs))
	for _, d := range defs {
		params := url.Values{}
		if d.slugv != "overview" {
			params.Set("tab", d.slugv)
		}
		href := basePath + "/widget-types/" + url.PathEscape(slug)
		if q := params.Encode(); q != "" {
			href += "?" + q
		}
		tabs = append(tabs, widgetTypeTab{
			Label:  d.label,
			Slug:   d.slugv,
			Href:   href,
			Active: d.slugv == activeTab,
		})
	}
	return tabs
}

// buildPreview executes the widget-type's template_source against the
// supplied payload (or a derived sample) and packages the result for
// the Preview tab / fragment. Render errors do not panic the request —
// we capture them on the view and record them in the diagnostics ring
// buffer so the operator sees them on the Diagnostics tab. A widget-type
// that is not widgetTypeTrusted is never executed, and the output is
// sanitized as the rich-report dispatcher's is (gd-a2s0GZExxr).
func (s *Server) buildPreview(ctx context.Context, bead *beadsapi.BeadDetail, payloadOverride string) *widgetTypePreviewView {
	slug := bead.Fields["slug"]
	src := bead.Fields["template_source"]
	view := &widgetTypePreviewView{Slug: slug, HasTemplate: strings.TrimSpace(src) != ""}
	if !view.HasTemplate {
		view.ErrorClass = "compile"
		view.ErrorMsg = "widget-type has no template_source — nothing to preview"
		return view
	}
	projects, err := s.daemon.ListProjectBeads(ctx)
	if err != nil || !widgetTypeTrusted(bead, projects) {
		view.ErrorClass = "trust"
		view.ErrorMsg = "widget-type is not from an operator land; its template is not rendered"
		return view
	}

	payload := strings.TrimSpace(payloadOverride)
	if payload == "" {
		payload = derivePreviewPayload(bead.Fields["schema"])
	}
	view.PayloadJSON = prettyJSON(payload)
	view.PayloadEdit = payload

	var data any
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			view.ErrorClass = "data"
			view.ErrorMsg = "preview payload is not valid JSON: " + err.Error()
			s.recordWidgetDiagnostic(slug, "data", view.ErrorMsg)
			return view
		}
	}

	tmpl, err := template.New(slug).Parse(src)
	if err != nil {
		view.ErrorClass = "compile"
		view.ErrorMsg = "template_source failed to compile: " + err.Error()
		s.recordWidgetDiagnostic(slug, "compile", view.ErrorMsg)
		return view
	}
	var buf strings.Builder
	if err := safeExecuteTemplate(tmpl, &buf, data); err != nil {
		view.ErrorClass = "render"
		view.ErrorMsg = "template render failed: " + err.Error()
		s.recordWidgetDiagnostic(slug, "render", view.ErrorMsg)
		return view
	}
	view.RenderedHTML = template.HTML(inertWidgetHTML(buf.String()))
	return view
}

// recordWidgetDiagnostic records an error on s.widgetDiagnostics, no-op
// when the field is not initialized (test contexts).
func (s *Server) recordWidgetDiagnostic(slug, class, msg string) {
	if s == nil || s.widgetDiagnostics == nil {
		return
	}
	s.widgetDiagnostics.Record(slug, class, msg)
}

// safeExecuteTemplate runs Execute under recover() so a template panic
// (nil-deref on .Field, range over wrong type) surfaces as a returned
// error rather than crashing the request goroutine. Plan §(5) calls
// out per-render recover() as a hard requirement.
func safeExecuteTemplate(t *template.Template, w *strings.Builder, data any) (rerr error) {
	defer func() {
		if rec := recover(); rec != nil {
			rerr = fmt.Errorf("panic during template execution: %v", rec)
		}
	}()
	return t.Execute(w, data)
}

// derivePreviewPayload returns a JSON payload that satisfies the schema
// well enough to render. It checks the schema's `examples` array first,
// then `default`. Returns "{}" when the schema is empty or unparseable
// so the template still runs against a non-nil object.
func derivePreviewPayload(schema string) string {
	schema = strings.TrimSpace(schema)
	if schema == "" || schema == "null" {
		return "{}"
	}
	var s map[string]any
	if err := json.Unmarshal([]byte(schema), &s); err != nil {
		return "{}"
	}
	if examples, ok := s["examples"].([]any); ok && len(examples) > 0 {
		if b, err := json.Marshal(examples[0]); err == nil {
			return string(b)
		}
	}
	if def, ok := s["default"]; ok {
		if b, err := json.Marshal(def); err == nil {
			return string(b)
		}
	}
	return "{}"
}

// buildDiagnostics returns the per-slug ring buffer + the sanitizer-
// rejection histogram for the Diagnostics tab. Counts are derived
// directly from the entries so they always reflect what the operator
// sees in the table below.
func (s *Server) buildDiagnostics(slug string) *widgetTypeDiagnosticsView {
	entries := s.widgetDiagnostics.Snapshot(slug)
	view := &widgetTypeDiagnosticsView{Slug: slug, Total: len(entries)}
	for i := range entries {
		entries[i].WhenAgo = humanizeTime(entries[i].When)
		switch entries[i].Class {
		case "sanitizer":
			view.SanitizerCount++
		case "compile":
			view.CompileCount++
		case "render":
			view.RenderCount++
		case "data":
			view.DataCount++
		}
	}
	view.Errors = entries
	return view
}

// handleHTMXWidgetPreview is the htmx fragment endpoint behind the
// Preview tab. Returns just the preview partial so the page can swap
// it in without a full reload (e.g. when the operator edits the
// payload textarea + hits Refresh).
func (s *Server) handleHTMXWidgetPreview(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}
	bead, _, err := s.findWidgetTypeBySlug(r.Context(), slug)
	if err != nil {
		http.Error(w, "lookup failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if bead == nil {
		http.Error(w, "no widget-type with slug="+slug, http.StatusNotFound)
		return
	}
	payload := r.URL.Query().Get("payload")
	if payload == "" && r.Method == http.MethodPost {
		// Allow form-encoded POST so the textarea + button on the
		// Preview tab can deliver a fresh payload without re-encoding
		// it as a query param (hx-post + hx-target).
		_ = r.ParseForm()
		payload = r.PostFormValue("payload")
	}
	view := s.buildPreview(r.Context(), bead, payload)
	s.renderPartial(w, r, "templates/partials/widget_type_preview.html", "widget_type_preview", map[string]any{
		"Preview": view,
	})
}

// handleHTMXWidgetDiagnostics is the htmx fragment endpoint behind the
// Diagnostics tab.
func (s *Server) handleHTMXWidgetDiagnostics(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}
	view := s.buildDiagnostics(slug)
	s.renderPartial(w, r, "templates/partials/widget_type_diagnostics.html", "widget_type_diagnostics", map[string]any{
		"Diagnostics": view,
	})
}

// renderPartial is defined in server.go.
