package main

import (
	"strings"
	"testing"
)

func TestRenderWidgetPreview_OK(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<p>{{ .Payload.title }}</p>`,
	}
	html, code, err := renderWidgetPreview(doc, []byte(`{"title":"hello"}`), false)
	if err != nil || code != previewOK {
		t.Fatalf("expected OK, got code=%d err=%v", code, err)
	}
	if !strings.Contains(html, "hello") {
		t.Errorf("missing rendered title: %q", html)
	}
}

func TestRenderWidgetPreview_CompileError(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<p>{{ .Payload.title </p>`,
	}
	_, code, err := renderWidgetPreview(doc, nil, false)
	if err == nil || code != previewCompileError {
		t.Fatalf("expected compile error, got code=%d err=%v", code, err)
	}
}

func TestRenderWidgetPreview_RenderRecover(t *testing.T) {
	// .Payload.items is nil so .missing on nil panics in html/template;
	// the render-time recover catches it and surfaces a render error.
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<p>{{ .Payload.items.missing }}</p>`,
	}
	_, _, err := renderWidgetPreview(doc, []byte(`{"items":null}`), false)
	if err == nil {
		t.Fatalf("expected an error for nil-deref render")
	}
}

func TestRenderWidgetPreview_SchemaMismatch(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<p>{{ .Payload.title }}</p>`,
		Schema:   `{"type":"object","required":["title"],"properties":{"title":{"type":"string"}}}`,
	}
	_, code, err := renderWidgetPreview(doc, []byte(`{}`), true)
	if err == nil || code != previewSchemaMismatch {
		t.Fatalf("expected schema-mismatch, got code=%d err=%v", code, err)
	}
}

// Regression for ka-0000005250: `.Payload.<field>` MUST resolve under
// `gb widget preview` because that's the scope prod `compileHtmxRenderer`
// renders against. The pre-fix scope passed the raw payload at root, so
// the same template would render empty here while working in prod.
func TestRenderWidgetPreview_PayloadScopeMatchesProd(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "demo",
		Template: `<p>{{ .Payload.cycle_no }} / {{ .Widget.ID }}</p>`,
	}
	html, code, err := renderWidgetPreview(doc, []byte(`{"cycle_no":"42"}`), false)
	if err != nil || code != previewOK {
		t.Fatalf("expected OK, got code=%d err=%v", code, err)
	}
	if !strings.Contains(html, "42") {
		t.Errorf("expected `.Payload.cycle_no` (42) in output, got %q", html)
	}
	if !strings.Contains(html, "demo") {
		t.Errorf("expected `.Widget.ID` (demo) in output, got %q", html)
	}
}

func TestPreviewFuncMap_URLHelpers(t *testing.T) {
	fm := previewFuncMap()
	if _, ok := fm["panelURL"]; !ok {
		t.Errorf("panelURL missing")
	}
	if _, ok := fm["actionURL"]; !ok {
		t.Errorf("actionURL missing")
	}
	if _, ok := fm["apiURL"]; !ok {
		t.Errorf("apiURL missing")
	}
	if _, ok := fm["decisionOptionURL"]; !ok {
		t.Errorf("decisionOptionURL missing")
	}
}

func TestPreviewWithHelpers_RendersClean(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<button hx-post="{{ actionURL "go" }}">go</button>`,
	}
	html, code, err := renderWidgetPreview(doc, nil, false)
	if err != nil || code != previewOK {
		t.Fatalf("expected OK, got code=%d err=%v", code, err)
	}
	if !strings.Contains(html, "/htmx/action/go") {
		t.Errorf("expected stub URL in output: %q", html)
	}
}

func TestParseScaffoldFields(t *testing.T) {
	got, err := parseScaffoldFields("a:string,b:integer,c:")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(got))
	}
	if got[0].Name != "a" || got[0].Type != "string" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Type != "integer" {
		t.Errorf("got[1] = %+v", got[1])
	}
	if got[2].Name != "c" || got[2].Type != "string" {
		t.Errorf("default type for empty:should be string, got %+v", got[2])
	}
}

func TestParseScaffoldFields_BadType(t *testing.T) {
	_, err := parseScaffoldFields("a:nope")
	if err == nil {
		t.Errorf("expected error for unsupported type")
	}
}

func TestScaffoldHTMXDoc_BasicShape(t *testing.T) {
	body := scaffoldHTMXDoc("foo", "summary", []scaffoldField{{Name: "headline", Type: "string"}}, false, false)
	for _, want := range []string{"## Summary", "## Schema", "## Sample payload", "## Template", `id="widget-foo"`} {
		if !strings.Contains(body, want) {
			t.Errorf("scaffold body missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestScaffoldHTMXDoc_WithLiveAndAction(t *testing.T) {
	body := scaffoldHTMXDoc("foo", "", nil, true, true)
	if !strings.Contains(body, "apiURL") {
		t.Errorf("expected apiURL when --with-live: %s", body)
	}
	if !strings.Contains(body, "actionURL") {
		t.Errorf("expected actionURL when --with-action: %s", body)
	}
}

func TestValidateWidget_StrictRequiresVersionAndSummary(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<p>ok</p>`,
	}
	res := validateWidget(doc, true)
	if res.OK {
		t.Errorf("expected strict failure")
	}
	if !findHasRule(res.Findings, "widget-version") {
		t.Errorf("expected widget-version rule, got %+v", res.Findings)
	}
	if !findHasRule(res.Findings, "summary") {
		t.Errorf("expected summary rule, got %+v", res.Findings)
	}
}

func TestValidateWidget_StrictPasses(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Version:  "1",
		Summary:  "Real summary text.",
		Template: `<p>{{ .Payload.title }}</p>`,
		Schema:   `{"type":"object","required":["title"],"properties":{"title":{"type":"string"}}}`,
		Sample:   `{"title":"hi"}`,
	}
	res := validateWidget(doc, true)
	if !res.OK {
		t.Errorf("expected pass, got %+v", res.Findings)
	}
}
