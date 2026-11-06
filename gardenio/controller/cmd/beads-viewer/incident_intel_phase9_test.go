package main

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

var errSinkBoom = errors.New("sink boom")

// Phase 9 tests — inbox link-out + rich-report back-link + telemetry.

func TestRichReportView_BackLinkFromParentMention(t *testing.T) {
	parentChild := "parent-child"
	related := "related"

	parent := &beadsapi.BeadDetail{
		ID:    "gd-rep00001",
		Type:  "rich-report",
		Title: "Active incident: study-web 502s",
		Fields: map[string]string{
			"headline": "study-web 502s in america-prod",
		},
	}
	mentions := []beadsapi.BeadReferenceSummary{
		// Untyped mention: must NOT win.
		{ID: "gd-untyped", Type: "task"},
		// related typed mention: must NOT win.
		{ID: "gd-related", Type: "task", RelationshipType: &related, Title: "Related ticket"},
		// parent-child: this is the back-link.
		{ID: "gd-intake01", Type: "task", RelationshipType: &parentChild, Title: "FSM-1234 — sponsor portal slow"},
		// Second parent-child: must be ignored (we take first).
		{ID: "gd-intake02", Type: "task", RelationshipType: &parentChild, Title: "second"},
	}

	v := buildRichReportView(context.Background(), parent, mentions, nil, &fakeDaemon{}, nil)
	if v == nil || v.BackLink == nil {
		t.Fatalf("expected BackLink, got %+v", v)
	}
	if v.BackLink.ID != "gd-intake01" {
		t.Errorf("BackLink.ID = %q, want gd-intake01", v.BackLink.ID)
	}
	if v.BackLink.Title != "FSM-1234 — sponsor portal slow" {
		t.Errorf("BackLink.Title = %q, want intake title", v.BackLink.Title)
	}
}

func TestRichReportView_BackLinkAbsentWhenNoParent(t *testing.T) {
	related := "related"
	parent := &beadsapi.BeadDetail{ID: "gd-rep01", Type: "rich-report"}
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-rel", Type: "task", RelationshipType: &related},
	}
	v := buildRichReportView(context.Background(), parent, mentions, nil, &fakeDaemon{}, nil)
	if v == nil {
		t.Fatal("nil view")
	}
	if v.BackLink != nil {
		t.Errorf("expected no BackLink, got %+v", v.BackLink)
	}
}

func TestRichReportView_BackLinkFallsBackToIDWhenTitleEmpty(t *testing.T) {
	parentChild := "parent-child"
	parent := &beadsapi.BeadDetail{ID: "gd-rep01", Type: "rich-report"}
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-intakeXY", Type: "task", RelationshipType: &parentChild, Title: ""},
	}
	v := buildRichReportView(context.Background(), parent, mentions, nil, &fakeDaemon{}, nil)
	if v == nil || v.BackLink == nil {
		t.Fatalf("expected BackLink, got %+v", v)
	}
	if v.BackLink.Title != "gd-intakeXY" {
		t.Errorf("Title fallback to ID failed: got %q", v.BackLink.Title)
	}
}

func TestRichReportView_BackLinkFallsBackToIDWhenTitleBlank(t *testing.T) {
	// A whitespace-only title (e.g. a bead titled with a stray tab/newline)
	// must fall back to the ID, not render an empty back-link label.
	parentChild := "parent-child"
	parent := &beadsapi.BeadDetail{ID: "gd-rep01", Type: "rich-report"}
	mentions := []beadsapi.BeadReferenceSummary{
		{ID: "gd-intakeXY", Type: "task", RelationshipType: &parentChild, Title: "  \t\n "},
	}
	v := buildRichReportView(context.Background(), parent, mentions, nil, &fakeDaemon{}, nil)
	if v == nil || v.BackLink == nil {
		t.Fatalf("expected BackLink, got %+v", v)
	}
	if v.BackLink.Title != "gd-intakeXY" {
		t.Errorf("blank Title should fall back to ID: got %q", v.BackLink.Title)
	}
}

func TestProjectViewFromBead_RichReportFlag(t *testing.T) {
	b := &beadsapi.BeadDetail{ID: "gd-x", Type: "rich-report", Title: "Rich-Report One", Status: "open"}
	v := projectViewFromBead(b, "user@example.com", "gardenio", "")
	if !v.IsRichReport {
		t.Errorf("expected IsRichReport=true for type=rich-report row, got false")
	}

	b2 := &beadsapi.BeadDetail{ID: "gd-y", Type: "task", Title: "Plain task", Status: "open"}
	v2 := projectViewFromBead(b2, "user@example.com", "gardenio", "")
	if v2.IsRichReport {
		t.Errorf("expected IsRichReport=false for type=task row")
	}
}

func TestRichReportPage_RendersBackLink(t *testing.T) {
	parentChild := "parent-child"
	fake := newFake()
	fake.beads["gd-rich01"] = &beadsapi.BeadDetail{
		ID:     "gd-rich01",
		Type:   "rich-report",
		Title:  "Test rich report",
		Status: "open",
		Fields: map[string]string{
			"headline":    "Investigation underway",
			"report_kind": "active-incident",
		},
		Labels: []string{"project:gardenio"},
	}
	fake.mentions = map[string][]beadsapi.BeadReferenceSummary{
		"gd-rich01": {
			{ID: "gd-intake42", Type: "task", RelationshipType: &parentChild, Title: "FSM-9000 user reports"},
		},
	}

	mux := mkServer(t, fake)
	req := httptest.NewRequest("GET", "/gd-rich01", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="rich-report-back"`,
		`href="/gd-intake42"`,
		`data-ii-telemetry="rich-report-backlink-click"`,
		`data-ii-telemetry-target="gd-intake42"`,
		"FSM-9000 user reports",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rich-report page missing %q in body", want)
		}
	}
}

func TestProjectRow_RichReportSurfacesViewReportCTA(t *testing.T) {
	v := projectBeadView{
		ID:           "gd-rep01",
		Title:        "Live incident",
		Type:         "rich-report",
		Status:       "open",
		IsRichReport: true,
	}
	out := renderProjectRow(t, v)
	for _, want := range []string{
		`class="bead-row-title bead-row-title-rich-report"`,
		`data-ii-telemetry="inbox-rich-report-click"`,
		`data-ii-telemetry-target="gd-rep01"`,
		`class="bead-row-rich-report-cta"`,
		`data-ii-telemetry="inbox-rich-report-cta-click"`,
		"View report →",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rich-report row missing %q in: %s", want, out)
		}
	}
}

func TestProjectRow_NonRichReportNoCTA(t *testing.T) {
	v := projectBeadView{
		ID:     "gd-task1",
		Title:  "Plain task",
		Type:   "task",
		Status: "open",
	}
	out := renderProjectRow(t, v)
	if strings.Contains(out, "bead-row-rich-report-cta") {
		t.Errorf("plain task row should not surface rich-report CTA: %s", out)
	}
	if strings.Contains(out, "data-ii-telemetry=") {
		t.Errorf("plain task row should not carry telemetry attr: %s", out)
	}
}

// renderProjectRow is a helper that runs the project_row partial with a
// minimal data shape so we can assert rendering side-effects without spinning
// up a full server.
func renderProjectRow(t *testing.T, row projectBeadView) string {
	t.Helper()
	funcs := template.FuncMap{
		"dict": func(pairs ...any) (map[string]any, error) {
			out := map[string]any{}
			for i := 0; i+1 < len(pairs); i += 2 {
				k, ok := pairs[i].(string)
				if !ok {
					return nil, errors.New("dict key not string")
				}
				out[k] = pairs[i+1]
			}
			return out, nil
		},
		"agentLiveIcon": func(name string) template.HTML { return "" },
		"displayTitle":  displayTitle,
		"heraldry":      heraldrySpan,
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS,
		"templates/partials/project_row.html",
		"templates/partials/row_badges.html",
	)
	if err != nil {
		t.Fatalf("parse project_row template: %v", err)
	}
	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, "project_row", map[string]any{
		"Row":      row,
		"OOB":      false,
		"BasePath": "",
	}); err != nil {
		t.Fatalf("render project_row: %v", err)
	}
	return buf.String()
}

func TestTelemetryEndpoint_AcceptsValidEventReturns204(t *testing.T) {
	fake := newFake()
	mux := mkServer(t, fake)
	body := strings.NewReader(`{"event":"inbox-rich-report-click","target":"gd-x","from":"/active","path":"a"}`)
	req := httptest.NewRequest("POST", "/dev/incident-intel/telemetry", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-Request-Email", "viewer@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d body=%s", w.Code, w.Body.String())
	}
	if fake.uiTelemetryCalls != 1 {
		t.Fatalf("expected 1 RecordUITelemetry call, got %d", fake.uiTelemetryCalls)
	}
	got := fake.uiTelemetryLast
	if got.Event != "inbox-rich-report-click" {
		t.Errorf("Event=%q, want inbox-rich-report-click", got.Event)
	}
	if got.Target != "gd-x" {
		t.Errorf("Target=%q, want gd-x", got.Target)
	}
	if got.From != "/active" {
		t.Errorf("From=%q, want /active", got.From)
	}
	if got.Path != "a" {
		t.Errorf("Path=%q, want a", got.Path)
	}
	if got.OperatorID != "viewer@example.com" {
		t.Errorf("OperatorID=%q, want viewer@example.com", got.OperatorID)
	}
}

func TestTelemetryEndpoint_SinkErrorStillReturns204(t *testing.T) {
	// Telemetry must never break the click that emitted it: a daemon-side
	// failure is logged but the response stays 204 so the JS module doesn't
	// retry-storm.
	fake := newFake()
	fake.uiTelemetryErr = errSinkBoom
	mux := mkServer(t, fake)
	body := strings.NewReader(`{"event":"inbox-rich-report-click","target":"gd-x"}`)
	req := httptest.NewRequest("POST", "/dev/incident-intel/telemetry", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204 even on sink error, got %d body=%s", w.Code, w.Body.String())
	}
	if fake.uiTelemetryCalls != 1 {
		t.Errorf("expected 1 RecordUITelemetry call, got %d", fake.uiTelemetryCalls)
	}
}

func TestTelemetryEndpoint_RejectsUnknownEvent(t *testing.T) {
	mux := mkServer(t, newFake())
	body := strings.NewReader(`{"event":"random-event","target":"x"}`)
	req := httptest.NewRequest("POST", "/dev/incident-intel/telemetry", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown event, got %d", w.Code)
	}
}

func TestTelemetryEndpoint_RejectsBadJSON(t *testing.T) {
	mux := mkServer(t, newFake())
	body := strings.NewReader(`not json`)
	req := httptest.NewRequest("POST", "/dev/incident-intel/telemetry", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad JSON, got %d", w.Code)
	}
}

func TestLayout_LinksTelemetryScript(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "/assets/static/incident_intel_telemetry.js") {
		t.Errorf("layout missing telemetry script tag")
	}
}

func TestStaticAsset_ServesTelemetryJS(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/incident_intel_telemetry.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"data-ii-telemetry",
		"sendBeacon",
		"IncidentIntel.telemetry",
		"/dev/incident-intel/telemetry",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("telemetry.js missing %q", want)
		}
	}
}
