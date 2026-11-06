package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"gardenio/controller/internal/beadsapi"
)

func newTestMarkdown() goldmark.Markdown {
	return goldmark.New(goldmark.WithExtensions(extension.GFM))
}

// TestBuildReportView_RendersMarkdownContent guards the headline of
// ka-0000000186: a report bead with format=markdown surfaces ContentHTML
// with the markdown body rendered, not the raw text. The escaped-<pre>
// fallback path is the BUG — anything that looks like markdown source in the
// rendered output means we regressed.
func TestBuildReportView_RendersMarkdownContent(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID:   "gd-test",
		Type: "report",
		Fields: map[string]string{
			"format":      "markdown",
			"content":     "# Heading\n\nBody with **bold** and `code`.\n",
			"report_type": "design",
			"decision_id": "ka-0000002333",
		},
	}
	v := buildReportView(bead, newTestMarkdown(), "")
	if v == nil {
		t.Fatal("buildReportView returned nil")
	}
	if v.Format != "markdown" {
		t.Errorf("Format=%q want markdown", v.Format)
	}
	if v.ReportType != "design" {
		t.Errorf("ReportType=%q want design", v.ReportType)
	}
	html := string(v.ContentHTML)
	if !strings.Contains(html, "<h1") {
		t.Errorf("ContentHTML missing <h1>: %q", html)
	}
	if !strings.Contains(html, "<strong>bold</strong>") {
		t.Errorf("ContentHTML missing <strong>bold</strong>: %q", html)
	}
	if !strings.Contains(html, "<code>code</code>") {
		t.Errorf("ContentHTML missing inline <code>: %q", html)
	}
	// OtherFields must include report_type + decision_id but never `content`
	// or `format` — those are promoted into the report block above the table.
	keys := map[string]string{}
	for _, f := range v.OtherFields {
		keys[f.Key] = f.Value
	}
	if _, has := keys["content"]; has {
		t.Errorf("OtherFields leaked `content` (would double-render): %v", keys)
	}
	if _, has := keys["format"]; has {
		t.Errorf("OtherFields leaked `format`: %v", keys)
	}
	if keys["report_type"] != "design" {
		t.Errorf("OtherFields[report_type]=%q want design", keys["report_type"])
	}
	if keys["decision_id"] != "ka-0000002333" {
		t.Errorf("OtherFields[decision_id]=%q want ka-0000002333", keys["decision_id"])
	}
}

// TestBuildReportView_RendersGFMTable covers the comparison-table use case
// from ka-0000007408 — operators read these reports for the tables, so a
// markdown table must come through as a real <table>.
func TestBuildReportView_RendersGFMTable(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Type: "report",
		Fields: map[string]string{
			"format":  "markdown",
			"content": "| Option | Cost |\n|---|---|\n| A | $0 |\n| B | $100 |\n",
		},
	}
	v := buildReportView(bead, newTestMarkdown(), "")
	html := string(v.ContentHTML)
	if !strings.Contains(html, "<table>") {
		t.Errorf("ContentHTML missing <table>: %q", html)
	}
	if !strings.Contains(html, "<th>Option</th>") {
		t.Errorf("ContentHTML missing <th>Option</th>: %q", html)
	}
}

// TestBuildReportView_LinkifiesBeadIDs guards that bare bead IDs in the body
// get wrapped into anchors, mirroring the description-render path.
func TestBuildReportView_LinkifiesBeadIDs(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Type: "report",
		Fields: map[string]string{
			"format":  "markdown",
			"content": "See ka-0000002333 for the parent.",
		},
	}
	v := buildReportView(bead, newTestMarkdown(), "/base")
	html := string(v.ContentHTML)
	if !strings.Contains(html, "ka-0000002333") {
		t.Fatalf("ContentHTML lost bead reference: %q", html)
	}
	if !strings.Contains(html, "href=") {
		t.Errorf("ContentHTML did not linkify bead ID: %q", html)
	}
}

// TestBuildReportView_NonMarkdownFormatKeepsRawContent — when format is
// missing or non-markdown, ContentHTML is empty so the template degrades to
// the escaped <pre>. The raw Content must still be available for that path.
func TestBuildReportView_NonMarkdownFormatKeepsRawContent(t *testing.T) {
	for _, format := range []string{"", "text", "html-source"} {
		bead := &beadsapi.BeadDetail{
			Type: "report",
			Fields: map[string]string{
				"format":  format,
				"content": "# not actually rendered",
			},
		}
		v := buildReportView(bead, newTestMarkdown(), "")
		if v.ContentHTML != "" {
			t.Errorf("format=%q: ContentHTML=%q, want empty (template falls back to <pre>)", format, v.ContentHTML)
		}
		if v.Content == "" {
			t.Errorf("format=%q: Content lost; the <pre> fallback would render empty", format)
		}
	}
}

// TestBuildReportView_NilMarkdownDegradesSafely — when the goldmark instance
// is missing (test contexts, defensive path), the view must not panic and
// ContentHTML stays empty so the template falls through to escaped <pre>.
func TestBuildReportView_NilMarkdownDegradesSafely(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Type: "report",
		Fields: map[string]string{
			"format":  "markdown",
			"content": "# header",
		},
	}
	v := buildReportView(bead, nil, "")
	if v.ContentHTML != "" {
		t.Errorf("ContentHTML=%q with nil md, want empty", v.ContentHTML)
	}
	if v.Content != "# header" {
		t.Errorf("Content lost: %q", v.Content)
	}
}

// TestBuildReportView_EmptyContentNoCrash — bead with format but no content
// should not call goldmark at all and OtherFields still surfaces siblings.
func TestBuildReportView_EmptyContentNoCrash(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		Type: "report",
		Fields: map[string]string{
			"format":      "markdown",
			"report_type": "post-mortem",
		},
	}
	v := buildReportView(bead, newTestMarkdown(), "")
	if v == nil {
		t.Fatal("buildReportView returned nil")
	}
	if v.ContentHTML != "" {
		t.Errorf("ContentHTML=%q with empty content, want empty", v.ContentHTML)
	}
}

func TestBuildReportView_NilBead(t *testing.T) {
	if v := buildReportView(nil, newTestMarkdown(), ""); v != nil {
		t.Errorf("buildReportView(nil) = %v, want nil", v)
	}
}

// TestBeadShow_ReportType_RendersMarkdown is the end-to-end guard: serving
// /<id> for a report bead with format=markdown must produce HTML where the
// content body is rendered (real <h1>) and not surfaced as raw markdown
// (literal "# header" in a <pre>). Mirrors the ka-0000007408 reading view.
func TestBeadShow_ReportType_RendersMarkdown(t *testing.T) {
	f := newFake()
	f.beads["gd-rpt"] = &beadsapi.BeadDetail{
		ID:     "gd-rpt",
		Title:  "Design report: example",
		Type:   "report",
		Status: "open",
		Fields: map[string]string{
			"format":      "markdown",
			"report_type": "design",
			"content":     "# Header\n\nBody with **bold**.\n",
			"decision_id": "gd-Other",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-rpt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"report-card",
		"<h1",
		"<strong>bold</strong>",
		"design",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("report render missing %q\nbody: %s", want, body)
		}
	}
	// The raw markdown source must NOT appear inside a <pre> — that would
	// indicate we degraded to the escaped path and the bug remains.
	if strings.Contains(body, "<pre class=\"report-content-raw\"># Header") {
		t.Errorf("report rendered as raw <pre>; markdown path failed\nbody: %s", body)
	}
	// Sibling field (decision_id) must still surface in the Fields table.
	if !strings.Contains(body, "decision_id") {
		t.Errorf("OtherFields decision_id missing from rendered page")
	}
	if !strings.Contains(body, "gd-Other") {
		t.Errorf("OtherFields decision_id value missing from rendered page")
	}
}

// TestBeadShow_ReportType_NonMarkdownFallsBackToPre — when format is not
// "markdown" the body still has to surface, but as escaped <pre> not as
// rendered HTML. Guards the format hint contract.
func TestBeadShow_ReportType_NonMarkdownFallsBackToPre(t *testing.T) {
	f := newFake()
	f.beads["gd-rpt-txt"] = &beadsapi.BeadDetail{
		ID:     "gd-rpt-txt",
		Title:  "Raw text report",
		Type:   "report",
		Status: "open",
		Fields: map[string]string{
			"content": "# not actually markdown",
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-rpt-txt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "report-content-raw") {
		t.Errorf("expected escaped <pre> fallback (report-content-raw class)\nbody: %s", body)
	}
	if strings.Contains(body, "<h1>not actually markdown</h1>") {
		t.Errorf("unexpectedly rendered as markdown despite missing format hint\nbody: %s", body)
	}
}
