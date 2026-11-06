package main

import (
	"embed"
	"html/template"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// newTestWidgetCtx parses every widget partial from the templateFS so
// per-widget tests can call renderers without spinning up a Server. The
// goldmark instance matches the GFM-with-hard-wraps shape used in production
// for short widget markdown. Phase 5+ added new partials; we glob them all
// in here so empty-payload / invalid-JSON test loops over registeredWidgets()
// don't bomb out on missing template names every time a phase lands.
func newTestWidgetCtx(t *testing.T) *widgetRenderCtx {
	t.Helper()
	tpl, err := template.New("").Funcs(template.FuncMap{
		"sparkConfig":    MarshalSparkConfig,
		"timelineConfig": MarshalTimelineConfig,
	}).ParseFS(getTestTemplateFS(),
		"templates/partials/widgets_phase4.html",
		"templates/partials/widgets_phase5.html",
		"templates/partials/widgets_phase6.html",
		"templates/partials/widgets_phase7.html")
	if err != nil {
		t.Fatalf("parse widget partials: %v", err)
	}
	return &widgetRenderCtx{
		md:        goldmark.New(goldmark.WithExtensions(extension.GFM)),
		templates: tpl,
		hl:        newCodeHighlighter(),
	}
}

// getTestTemplateFS returns the package-level templateFS embed.
// The package already has a `var templateFS embed.FS` — we reach into it via
// a tiny helper so the test stays in one file even if templateFS moves.
func getTestTemplateFS() embed.FS { return templateFS }

// TestRegisteredWidgets_CatalogComplete asserts the 11 Phase 4 slugs are all
// present and that no spurious entries snuck into the registry.
func TestRegisteredWidgets_CatalogComplete(t *testing.T) {
	want := []string{
		"activity-log",
		"affected-files",
		"ai-recommended-actions",
		"header-incident-summary",
		"related-tickets",
		"reporter-info",
		"reproduction-steps",
		"service-health",
		"suggested-test-cases",
		"user-reported-issue-header",
		"workaround",
	}
	// Phase 4 owns the slugs in `want`; later phases append. We only assert
	// that every Phase 4 slug is registered (and in lexical order relative
	// to itself), not that registeredWidgets() length == 11. That keeps this
	// test from breaking every time a later phase adds widgets.
	got := registeredWidgets()
	prev := -1
	for _, slug := range want {
		idx := indexOf(got, slug)
		if idx < 0 {
			t.Errorf("Phase 4 slug %q missing from registry: %v", slug, got)
			continue
		}
		if idx <= prev {
			t.Errorf("Phase 4 slug %q out of lexical order in registry: %v", slug, got)
		}
		prev = idx
	}
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}

// TestConfidenceBand covers the D2 numeric→band mapping.
func TestConfidenceBand(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"missing", nil, ""},
		{"high-string", "high", "high"},
		{"HIGH-uppercase", "HIGH", "high"},
		{"medium-string", "medium", "medium"},
		{"low-string", "low", "low"},
		{"unrecognised-string", "yolo", ""},
		{"high-number-0.9", 0.9, "high"},
		{"high-boundary-0.8", 0.8, "high"},
		{"medium-number-0.6", 0.6, "medium"},
		{"medium-boundary-0.5", 0.5, "medium"},
		{"low-number-0.2", 0.2, "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{}
			if tc.in != nil {
				payload["confidence"] = tc.in
			}
			if got := confidenceBand(payload); got != tc.want {
				t.Errorf("confidenceBand(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRenderHeaderIncidentSummary_Basics — happy path + acknowledge action.
func TestRenderHeaderIncidentSummary(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderHeaderIncidentSummary(`{
		"title":"study-web 502 spike",
		"severity":"P1",
		"status":"open",
		"started_at":"2026-05-06T12:00:00Z",
		"incident_id":"gd-incident-001"
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	for _, want := range []string{"study-web 502 spike", `data-severity="p1"`, `data-status="open"`, "2026-05-06T12:00:00Z", "Acknowledge"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output:\n%s", want, s)
		}
	}
}

func TestRenderAIRecommendedActions(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderAIRecommendedActions(`{
		"actions":[
			{"headline":"Roll back !2185","detail":"Probable cause is the auth refactor","confidence":0.9,"hotkey":"r","open_mr":"https://gitlab.com/x/y/-/merge_requests/2185","button_label":"Open MR"},
			{"headline":"Check Mezmo errors","confidence":"medium","open_mezmo":"app:beads-viewer level:error","button_label":"Open logs"}
		]
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	for _, want := range []string{
		"Roll back !2185", "Check Mezmo errors",
		`data-confidence="high"`, `data-confidence="medium"`,
		"high confidence", "medium confidence",
		// D2: numeric scores must NOT leak into rendered output
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output:\n%s", want, s)
		}
	}
	if strings.Contains(s, "0.9") {
		t.Errorf("D2 violation: numeric confidence 0.9 leaked into output:\n%s", s)
	}
}

func TestRenderServiceHealth(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderServiceHealth(`{
		"services":[
			{"service":"study-web","state":"critical","detail":"5xx 12%"},
			{"service":"sponsor-api","state":"healthy","detail":"5xx 0%"}
		]
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	for _, want := range []string{"study-web", "sponsor-api", `data-state="critical"`, `data-state="healthy"`} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output:\n%s", want, s)
		}
	}
}

func TestRenderAffectedFiles(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderAffectedFiles(`{
		"files":[
			{"path":"gardenio/controller/cmd/beads-viewer/server.go","line":442,"reason":"new auth middleware"}
		]
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	for _, want := range []string{"gardenio/controller/cmd/beads-viewer/server.go", ":442", "new auth middleware", `data-ii-action`, "Open in IDE"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output:\n%s", want, s)
		}
	}
}

func TestRenderSuggestedTestCases(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderSuggestedTestCases(`{
		"cases":[
			{"title":"Verify login redirect on stale session","detail":"Token < 60s","coverage":"missing"}
		]
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	for _, want := range []string{"Verify login redirect on stale session", `data-state="missing"`, "Token &lt; 60s"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output:\n%s", want, s)
		}
	}
}

func TestRenderUserReportedIssueHeader(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderUserReportedIssueHeader(`{
		"title":"Cannot upload TMF document",
		"ticket_id":"FSM-12345",
		"ticket_url":"https://acme.atlassian.net/browse/FSM-12345",
		"severity":"P2",
		"reported_at":"2026-05-05T10:00:00Z"
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	for _, want := range []string{"Cannot upload TMF document", "FSM-12345", `data-severity="p2"`, "Open ticket"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output:\n%s", want, s)
		}
	}
}

func TestRenderReporterInfo(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderReporterInfo(`{
		"name":"Perry Tate",
		"email":"paulo@example.com",
		"org":"Acme",
		"role":"Site coordinator"
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	for _, want := range []string{"Perry Tate", "paulo@example.com", "Acme", "Site coordinator", "Copy email"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output:\n%s", want, s)
		}
	}
}

func TestRenderReproductionSteps_Markdown(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderReproductionSteps(`{
		"markdown":"1. Open the upload dialog\n2. Pick a 50 MB PDF\n3. Click **Upload** — see the spinner hang"
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	if !strings.Contains(s, "<ol") {
		t.Errorf("expected <ol> from markdown:\n%s", s)
	}
	if !strings.Contains(s, "<strong>Upload</strong>") {
		t.Errorf("expected bold rendering:\n%s", s)
	}
}

func TestRenderReproductionSteps_StepsArrayFallback(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderReproductionSteps(`{
		"steps":["Step alpha","Step beta"]
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	if !strings.Contains(s, "Step alpha") || !strings.Contains(s, "Step beta") {
		t.Errorf("steps[] fallback failed:\n%s", s)
	}
}

func TestRenderWorkaround(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderWorkaround(`{
		"headline":"Use the legacy upload path",
		"markdown":"Send the URL ` + "`?legacy=1`" + ` to affected users."
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	for _, want := range []string{"Use the legacy upload path", "<code>?legacy=1</code>"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output:\n%s", want, s)
		}
	}
}

func TestRenderRelatedTickets(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderRelatedTickets(`{
		"tickets":[
			{"id":"FSM-12345","kind":"jira","title":"TMF upload broken","state":"open"},
			{"id":"!2185","kind":"mr","title":"Fast-track merge","url":"https://gitlab.com/x/-/merge_requests/2185","state":"merged"}
		]
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	for _, want := range []string{"FSM-12345", "TMF upload broken", `data-source-kind="jira"`, `data-source-kind="gitlab"`, "Open MR"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output:\n%s", want, s)
		}
	}
}

func TestRenderActivityLog(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	html, err := renderActivityLog(`{
		"entries":[
			{"when":"2026-05-06T12:00:00Z","actor":"gardenio","kind":"acknowledged","text":"Auto-ack on alert"},
			{"when":"2026-05-06T12:05:00Z","actor":"matt","kind":"comment","text":"Investigating"}
		]
	}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	s := string(html)
	for _, want := range []string{"gardenio", "matt", "Auto-ack on alert", "Investigating", `data-state="acknowledged"`, `data-state="comment"`} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output:\n%s", want, s)
		}
	}
}

// TestRenderMissingPayload — empty JSON should render the widget's empty state
// (the "<p class=\"empty\">" fallback) rather than panic.
func TestRender_EmptyPayload(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	for _, slug := range registeredWidgets() {
		t.Run(slug, func(t *testing.T) {
			r, _ := lookupWidget(slug)
			html, err := r("{}", ctx)
			if err != nil {
				t.Fatalf("%s: empty payload err: %v", slug, err)
			}
			if string(html) == "" {
				t.Errorf("%s: empty html for empty payload", slug)
			}
		})
	}
}

// TestRender_InvalidJSON — every renderer should return an error (not panic)
// on bad JSON; the dispatcher in rich_report.go is responsible for falling
// back to the placeholder card.
func TestRender_InvalidJSON(t *testing.T) {
	ctx := newTestWidgetCtx(t)
	for _, slug := range registeredWidgets() {
		t.Run(slug, func(t *testing.T) {
			r, _ := lookupWidget(slug)
			_, err := r("{not json", ctx)
			if err == nil {
				t.Errorf("%s: expected error on invalid JSON", slug)
			}
		})
	}
}

// TestActionButtonJSON_EscapesProperly — descriptor JSON must not break out of
// the data-ii-action attribute.
func TestActionButtonJSON_Escapes(t *testing.T) {
	a := OpenJiraAction("", "FSM-12345")
	got := actionButtonJSON("Open ticket", a)
	if !strings.Contains(got, `data-ii-action='`) {
		t.Errorf("missing single-quoted descriptor attr: %s", got)
	}
	if !strings.Contains(got, ">Open ticket<") {
		t.Errorf("button label not in output: %s", got)
	}
}
