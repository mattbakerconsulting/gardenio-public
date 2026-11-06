package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// renderBundleContent parses templates/bundle.html with a stub `layout` that
// just executes the page's `content` block, so we can assert the doc-card region
// in isolation without booting the full page/render pipeline. bundle.html's doc
// cards use only template builtins (if/with/range), so no FuncMap is needed; the
// garden banner partial (R6, garden_retire.go) is parsed alongside.
func renderBundleContent(t *testing.T, data map[string]any) string {
	t.Helper()
	tmpl := template.Must(template.New("stub").Parse(`{{define "layout"}}{{template "content" .}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFS(templateFS, "templates/bundle.html", "templates/partials/garden_banner.html"))
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "bundle.html", data); err != nil {
		t.Fatalf("render bundle.html: %v", err)
	}
	return buf.String()
}

// TestBundleTemplate_BlankDocRendersNamingPlaceholder is the render-guard half of
// ka-0000003524: an existing doc with no content/content_url/RenderedHTML must
// render a VISIBLE placeholder that NAMES the bead (so an orphaned mis-authored
// doc is debuggable) rather than a silent blank card.
func TestBundleTemplate_BlankDocRendersNamingPlaceholder(t *testing.T) {
	view := &bundleView{
		BundleID: "gd-0hwiram2qr",
		Title:    "Blank doc bundle",
		Docs: []bundleDocCard{
			{BeadID: "gd-blank0000001", Format: "md"}, // no Content / ContentURL / RenderedHTML
		},
	}
	html := renderBundleContent(t, map[string]any{"Bundle": view, "BasePath": ""})

	if !strings.Contains(html, "bundle-doc-nocontent") {
		t.Errorf("expected the no-content placeholder class, got:\n%s", html)
	}
	if !strings.Contains(html, "gd-blank0000001") {
		t.Errorf("placeholder must name the bead id, got:\n%s", html)
	}
	if !strings.Contains(html, "fields.content") {
		t.Errorf("placeholder should point at fields.content as the fix, got:\n%s", html)
	}
}

// TestBundleTemplate_DocWithContentDoesNotShowPlaceholder guards the blast
// radius: a doc that DOES have content renders its body, never the placeholder.
func TestBundleTemplate_DocWithContentDoesNotShowPlaceholder(t *testing.T) {
	view := &bundleView{
		BundleID: "gd-bpldforyp9",
		Title:    "Real doc bundle",
		Docs: []bundleDocCard{
			{BeadID: "gd-real00000001", Format: "md", Content: "# Real body"},
		},
	}
	html := renderBundleContent(t, map[string]any{"Bundle": view, "BasePath": ""})

	if strings.Contains(html, "bundle-doc-nocontent") {
		t.Errorf("a doc with content must not render the no-content placeholder, got:\n%s", html)
	}
	if !strings.Contains(html, "# Real body") {
		t.Errorf("expected the doc content to render, got:\n%s", html)
	}
}
