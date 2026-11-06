package main

// Phase 4 — 11 plain-HTML widget renderers.
// Plan-of-record: docs/research/incident-intelligence/synthesis.md §Phase 4.
// Bead: ka-0000002496.
//
// Each widget is a (name, render) pair installed into the package-level
// widgetRegistry. The dispatcher in rich_report.go looks up the renderer by
// `widget_type` field on the widget-instance bead; missing renderers fall
// through to the placeholder card from Phase 0.
//
// D2 — Confidence UX: bands only (high/medium/low). Numeric scores are
// accepted in the payload (so future calibration work has somewhere to land)
// but never surfaced — the renderer maps the band, ignores the number.

import (
	"encoding/json"
	"fmt"
	"html/template"
	"strings"

	"github.com/yuin/goldmark"
)

// widgetRenderer is the contract every Phase 4+ widget implements. It receives
// the raw JSON payload from the widget-instance bead and a render context
// (currently just the markdown renderer, but extension-shaped). It returns the
// rendered HTML fragment that drops into <article class="widget-card"> in the
// rich-report page.
//
// Errors degrade the card to the placeholder; they do NOT fail the page render
// (see rich_report.go).
type widgetRenderer func(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error)

// widgetRenderCtx carries cross-widget services. Phase 4 only needs goldmark
// for markdown widgets (#16, #17); later phases will add chroma, time-series
// helpers, etc.
//
// The templates field holds the parsed `widget_*` sub-templates. Tests build
// a ctx via newTestWidgetCtx (see widgets_phase4_test.go); production wires
// it via newWidgetRenderCtx in server.go.
type widgetRenderCtx struct {
	md        goldmark.Markdown
	templates *template.Template
	// hl is the server-side code highlighter used by Phase 5+ widgets that
	// render raw code outside a markdown fence. Nil in tests that don't
	// exercise code-snippet widgets.
	hl *codeHighlighter
	// mezmo is the live-data client for widget-instance beads with a
	// mezmo_source field. Nil-safe: callers route through applyMezmoSource
	// which short-circuits when the client is missing.
	mezmo      MezmoClient
	mezmoCache *mezmoCache
}

// widgetRegistry resolves widget type slugs to renderers. Populated by
// init() — registration is package-level so tests don't have to wire it up.
var widgetRegistry = map[string]widgetRenderer{}

// registerWidget installs a renderer. Duplicate slugs panic so a typo in
// init() blows up at startup rather than silently shadowing.
func registerWidget(slug string, r widgetRenderer) {
	if _, dup := widgetRegistry[slug]; dup {
		// PANIC: programmer error at init-time — duplicate slug in
		// widget catalog means two registrar funcs raced or a slug
		// was copy-pasted. Better to crash loud at startup than to
		// silently shadow one renderer with another.
		panic("incident-intel widget already registered: " + slug)
	}
	widgetRegistry[slug] = r
}

// lookupWidget returns the renderer for the given slug. ok=false when no
// renderer is registered (caller should fall back to the placeholder).
func lookupWidget(slug string) (widgetRenderer, bool) {
	r, ok := widgetRegistry[slug]
	return r, ok
}

// renderWidgetTemplate runs a named sub-template against the payload and
// returns the buffered HTML. Centralised so every widget gets the same
// template-execution error handling.
func renderWidgetTemplate(ctx *widgetRenderCtx, name string, data any) (template.HTML, error) {
	if ctx == nil || ctx.templates == nil {
		return "", fmt.Errorf("widget %s: render context is missing templates", name)
	}
	var buf strings.Builder
	if err := ctx.templates.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("widget %s: %w", name, err)
	}
	return template.HTML(buf.String()), nil
}

// confidenceBand normalises a payload's confidence field to one of
// "", "high", "medium", "low". D2: numbers go through the band map and are
// not surfaced. Empty string when unset.
func confidenceBand(payload map[string]any) string {
	switch v := payload["confidence"].(type) {
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		switch s {
		case "high", "medium", "low":
			return s
		}
	case float64:
		// JSON numbers decode as float64. Map per the synthesis doc:
		// >=0.8 high, >=0.5 medium, otherwise low.
		switch {
		case v >= 0.8:
			return "high"
		case v >= 0.5:
			return "medium"
		default:
			return "low"
		}
	}
	return ""
}

// widgetString pulls a string payload field, trimming whitespace. Returns
// empty when missing or the wrong type. Distinct from the broader
// `stringField` helper in type_views.go (which JSON-encodes non-strings) —
// widgets want a strict string-or-empty contract.
func widgetString(payload map[string]any, key string) string {
	if v, ok := payload[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// widgetStringSlice pulls a []string payload field, ignoring non-strings.
// Returns nil when missing.
func widgetStringSlice(payload map[string]any, key string) []string {
	raw, ok := payload[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// objectSlice pulls a []map[string]any payload field. Skips non-objects.
func widgetObjectSlice(payload map[string]any, key string) []map[string]any {
	raw, ok := payload[key].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// decodePayload unmarshals the raw JSON string into a map. Empty input
// returns an empty map (not an error) so widgets can render their empty
// state without special-casing the call site.
func decodePayload(rawJSON string) (map[string]any, error) {
	rawJSON = strings.TrimSpace(rawJSON)
	if rawJSON == "" {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &out); err != nil {
		return nil, fmt.Errorf("invalid widget payload JSON: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// renderMarkdown runs goldmark over the input string. Used by widgets that
// accept author-provided markdown (#16 reproduction-steps, #17 workaround).
// Falls back to plain-text wrap when the renderer is unset or fails.
func renderMarkdown(md goldmark.Markdown, src string) template.HTML {
	if src == "" {
		return ""
	}
	if md == nil {
		// Defensive: in tests the ctx may be nil. Escape and wrap so we still
		// produce safe HTML rather than panic.
		return template.HTML("<p>" + template.HTMLEscapeString(src) + "</p>")
	}
	var buf strings.Builder
	if err := md.Convert([]byte(src), &buf); err != nil {
		return template.HTML("<p>" + template.HTMLEscapeString(src) + "</p>")
	}
	return template.HTML(buf.String())
}

func init() {
	registerWidget("header-incident-summary", renderHeaderIncidentSummary)
	registerWidget("ai-recommended-actions", renderAIRecommendedActions)
	registerWidget("service-health", renderServiceHealth)
	registerWidget("affected-files", renderAffectedFiles)
	registerWidget("suggested-test-cases", renderSuggestedTestCases)
	registerWidget("user-reported-issue-header", renderUserReportedIssueHeader)
	registerWidget("reporter-info", renderReporterInfo)
	registerWidget("reproduction-steps", renderReproductionSteps)
	registerWidget("workaround", renderWorkaround)
	registerWidget("related-tickets", renderRelatedTickets)
	registerWidget("activity-log", renderActivityLog)
}

// ---- W1 — header-incident-summary -----------------------------------------

type headerIncidentSummary struct {
	Title         string
	Severity      string // p1/p2/p3 lowercased
	Status        string
	StartedISO    string
	PrimaryAction template.HTML
	AckAction     template.HTML
}

func renderHeaderIncidentSummary(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := headerIncidentSummary{
		Title:      widgetString(p, "title"),
		Severity:   strings.ToLower(widgetString(p, "severity")),
		Status:     strings.ToLower(widgetString(p, "status")),
		StartedISO: widgetString(p, "started_at"),
	}
	if a := widgetString(p, "primary_action_url"); a != "" {
		v.PrimaryAction = template.HTML(actionButtonJSON("Investigate",
			OpenMRAction(widgetString(p, "primary_action_label"), a)))
	}
	if id := widgetString(p, "incident_id"); id != "" {
		v.AckAction = template.HTML(actionButtonJSON("Acknowledge",
			AckIncidentAction(id, "")))
	}
	return renderWidgetTemplate(ctx, "widget_header_incident_summary", v)
}

// actionButtonJSON renders one descriptor as a button HTML fragment. Kept
// here (not in the template) so the descriptor MustJSON only runs once and
// the button-shape stays consistent across widgets.
func actionButtonJSON(label string, a ActionDescriptor) string {
	if label == "" {
		label = a.Label
	}
	return fmt.Sprintf(
		`<button type="button" class="ii-action-btn" data-ii-action='%s'>%s</button>`,
		template.HTMLEscapeString(a.MustJSON()),
		template.HTMLEscapeString(label),
	)
}

// ---- W2 — ai-recommended-actions ------------------------------------------

type aiRecommendedAction struct {
	Headline   string
	Detail     string
	Confidence string // band only — D2
	Hotkey     string
	Button     template.HTML
}

type aiRecommendedActions struct {
	Actions []aiRecommendedAction
}

func renderAIRecommendedActions(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := aiRecommendedActions{}
	for _, a := range widgetObjectSlice(p, "actions") {
		row := aiRecommendedAction{
			Headline:   widgetString(a, "headline"),
			Detail:     widgetString(a, "detail"),
			Confidence: confidenceBand(a),
			Hotkey:     widgetString(a, "hotkey"),
		}
		// Each action carries its own descriptor. We accept the simplest
		// shapes here (open-mr, open-jira, open-mezmo) and leave more exotic
		// kinds to later phases.
		if mrURL := widgetString(a, "open_mr"); mrURL != "" {
			row.Button = template.HTML(actionButtonJSON(widgetString(a, "button_label"),
				OpenMRAction(widgetString(a, "button_label"), mrURL)))
		} else if ticket := widgetString(a, "open_jira"); ticket != "" {
			row.Button = template.HTML(actionButtonJSON(widgetString(a, "button_label"),
				OpenJiraAction("", ticket)))
		} else if q := widgetString(a, "open_mezmo"); q != "" {
			row.Button = template.HTML(actionButtonJSON(widgetString(a, "button_label"),
				OpenMezmoAction("", q)))
		}
		v.Actions = append(v.Actions, row)
	}
	return renderWidgetTemplate(ctx, "widget_ai_recommended_actions", v)
}

// ---- W3 — service-health --------------------------------------------------

type serviceHealthRow struct {
	Service string
	State   string // healthy / warn / critical
	Detail  string
}

type serviceHealth struct {
	Rows []serviceHealthRow
}

func renderServiceHealth(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := serviceHealth{}
	for _, s := range widgetObjectSlice(p, "services") {
		v.Rows = append(v.Rows, serviceHealthRow{
			Service: widgetString(s, "service"),
			State:   strings.ToLower(widgetString(s, "state")),
			Detail:  widgetString(s, "detail"),
		})
	}
	return renderWidgetTemplate(ctx, "widget_service_health", v)
}

// ---- W5 — affected-files --------------------------------------------------

type affectedFile struct {
	Path   string
	Line   int
	Reason string
	Button template.HTML
}

type affectedFiles struct {
	Files []affectedFile
}

func renderAffectedFiles(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := affectedFiles{}
	for _, f := range widgetObjectSlice(p, "files") {
		path := widgetString(f, "path")
		line := 0
		if n, ok := f["line"].(float64); ok {
			line = int(n)
		}
		row := affectedFile{
			Path:   path,
			Line:   line,
			Reason: widgetString(f, "reason"),
		}
		if path != "" {
			row.Button = template.HTML(actionButtonJSON("Open in IDE", OpenIDEAction(path, line)))
		}
		v.Files = append(v.Files, row)
	}
	return renderWidgetTemplate(ctx, "widget_affected_files", v)
}

// ---- W6 — suggested-test-cases --------------------------------------------

type suggestedTestCase struct {
	Title    string
	Detail   string
	Coverage string
}

type suggestedTestCases struct {
	Cases []suggestedTestCase
}

func renderSuggestedTestCases(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := suggestedTestCases{}
	for _, c := range widgetObjectSlice(p, "cases") {
		v.Cases = append(v.Cases, suggestedTestCase{
			Title:    widgetString(c, "title"),
			Detail:   widgetString(c, "detail"),
			Coverage: widgetString(c, "coverage"),
		})
	}
	return renderWidgetTemplate(ctx, "widget_suggested_test_cases", v)
}

// ---- W14 — user-reported-issue-header -------------------------------------

type userReportedIssueHeader struct {
	Title       string
	TicketID    string
	TicketURL   string
	Severity    string
	ReportedISO string
	Button      template.HTML
}

func renderUserReportedIssueHeader(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := userReportedIssueHeader{
		Title:       widgetString(p, "title"),
		TicketID:    widgetString(p, "ticket_id"),
		TicketURL:   widgetString(p, "ticket_url"),
		Severity:    strings.ToLower(widgetString(p, "severity")),
		ReportedISO: widgetString(p, "reported_at"),
	}
	if v.TicketID != "" {
		v.Button = template.HTML(actionButtonJSON("Open ticket", OpenJiraAction("", v.TicketID)))
	}
	return renderWidgetTemplate(ctx, "widget_user_reported_issue_header", v)
}

// ---- W15 — reporter-info --------------------------------------------------

type reporterInfo struct {
	Name        string
	Email       string
	Org         string
	Role        string
	ContactSlug string // used for action descriptor target
	CopyButton  template.HTML
}

func renderReporterInfo(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := reporterInfo{
		Name:  widgetString(p, "name"),
		Email: widgetString(p, "email"),
		Org:   widgetString(p, "org"),
		Role:  widgetString(p, "role"),
	}
	if v.Email != "" {
		v.CopyButton = template.HTML(actionButtonJSON("Copy email",
			CopyToClipboardAction("Copy email", v.Email)))
	}
	return renderWidgetTemplate(ctx, "widget_reporter_info", v)
}

// ---- W16 — reproduction-steps (markdown) ----------------------------------

type reproductionSteps struct {
	HTML template.HTML
}

func renderReproductionSteps(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := reproductionSteps{}
	src := widgetString(p, "markdown")
	if src == "" {
		// Fall back to a numbered-list shape if the payload uses `steps: []string`.
		if steps := widgetStringSlice(p, "steps"); len(steps) > 0 {
			lines := make([]string, 0, len(steps))
			for i, s := range steps {
				lines = append(lines, fmt.Sprintf("%d. %s", i+1, s))
			}
			src = strings.Join(lines, "\n")
		}
	}
	var md goldmark.Markdown
	if ctx != nil {
		md = ctx.md
	}
	v.HTML = renderMarkdown(md, src)
	return renderWidgetTemplate(ctx, "widget_reproduction_steps", v)
}

// ---- W17 — workaround (markdown) ------------------------------------------

type workaround struct {
	Headline string
	HTML     template.HTML
}

func renderWorkaround(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := workaround{
		Headline: widgetString(p, "headline"),
	}
	var md goldmark.Markdown
	if ctx != nil {
		md = ctx.md
	}
	v.HTML = renderMarkdown(md, widgetString(p, "markdown"))
	return renderWidgetTemplate(ctx, "widget_workaround", v)
}

// ---- W18 — related-tickets ------------------------------------------------

type relatedTicketRow struct {
	ID     string
	Kind   string // "jira" | "bead" | "mr"
	Title  string
	URL    string
	State  string
	Button template.HTML
}

type relatedTickets struct {
	Rows []relatedTicketRow
}

func renderRelatedTickets(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := relatedTickets{}
	for _, t := range widgetObjectSlice(p, "tickets") {
		row := relatedTicketRow{
			ID:    widgetString(t, "id"),
			Kind:  strings.ToLower(widgetString(t, "kind")),
			Title: widgetString(t, "title"),
			URL:   widgetString(t, "url"),
			State: strings.ToLower(widgetString(t, "state")),
		}
		switch row.Kind {
		case "jira":
			if row.ID != "" {
				row.Button = template.HTML(actionButtonJSON("Open", OpenJiraAction("", row.ID)))
			}
		case "mr":
			if row.URL != "" {
				row.Button = template.HTML(actionButtonJSON("Open MR", OpenMRAction(row.ID, row.URL)))
			}
		}
		v.Rows = append(v.Rows, row)
	}
	return renderWidgetTemplate(ctx, "widget_related_tickets", v)
}

// ---- W19 — activity-log ---------------------------------------------------

type activityLogEntry struct {
	WhenISO string
	Actor   string
	Kind    string
	Text    string
}

type activityLog struct {
	Entries []activityLogEntry
}

func renderActivityLog(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := activityLog{}
	for _, e := range widgetObjectSlice(p, "entries") {
		v.Entries = append(v.Entries, activityLogEntry{
			WhenISO: widgetString(e, "when"),
			Actor:   widgetString(e, "actor"),
			Kind:    strings.ToLower(widgetString(e, "kind")),
			Text:    widgetString(e, "text"),
		})
	}
	return renderWidgetTemplate(ctx, "widget_activity_log", v)
}
