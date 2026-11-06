package main

import "testing"

// TestLint_BannedAttribute: hx-on* attributes are banned from widget templates
// (ka-0000000873).
func TestLint_BannedAttribute(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<div hx-on:click="alert(1)">hi</div>`,
	}
	findings := lintWidgetTemplate(doc)
	if !findHasRule(findings, "banned-attr") {
		t.Errorf("expected banned-attr, got %+v", findings)
	}
}

// TestLint_StyleAttribute: a style= attribute is banned from widget templates
// (ka-0000000873).
func TestLint_StyleAttribute(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<div style="color:red">hi</div>`,
	}
	findings := lintWidgetTemplate(doc)
	if !findHasRule(findings, "banned-attr") {
		t.Errorf("expected banned-attr for style=, got %+v", findings)
	}
}

// TestLint_DataStyleNotBanned: a data-style-* attribute is not mistaken for
// style= (ka-0000000873).
func TestLint_DataStyleNotBanned(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<div data-style-foo="bar">ok</div>`,
	}
	findings := lintWidgetTemplate(doc)
	if findHasRule(findings, "banned-attr") {
		t.Errorf("data-style-foo should not trigger banned-attr, got %+v", findings)
	}
}

// TestLint_URLPrefixWithLiteral: a literal /htmx/ URL passes with a
// prefer-a-helper warning (ka-0000000776).
func TestLint_URLPrefixWithLiteral(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<div hx-get="/htmx/foo">x</div>`,
	}
	findings := lintWidgetTemplate(doc)
	// literal /htmx/ is allowed but soft-warned to prefer helpers.
	if !findHasRule(findings, "url-prefix-helper") {
		t.Errorf("expected url-prefix-helper warning, got %+v", findings)
	}
	if findHasRule(findings, "url-prefix") {
		t.Errorf("literal /htmx/ should not error url-prefix, got %+v", findings)
	}
}

// TestLint_URLPrefixWithHelper: a URL built by a known helper is clean
// (ka-0000000776).
func TestLint_URLPrefixWithHelper(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<div hx-post="{{ actionURL "refresh" }}">x</div>`,
	}
	findings := lintWidgetTemplate(doc)
	for _, f := range findings {
		if f.Rule == "url-prefix" || f.Rule == "url-prefix-helper" {
			t.Errorf("helper URL should be clean, got finding %+v", f)
		}
	}
}

// TestLint_URLPrefixViolation: an hx-get to any URL outside /htmx/ is an error
// (ka-0000000776).
func TestLint_URLPrefixViolation(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<div hx-get="https://evil.example/x">x</div>`,
	}
	findings := lintWidgetTemplate(doc)
	if !findHasRule(findings, "url-prefix") {
		t.Errorf("expected url-prefix violation, got %+v", findings)
	}
}

// TestLint_HxTargetAllowed: hx-target may be this, closest, find, or a
// #widget-*/#w-* id (ka-0000000776).
func TestLint_HxTargetAllowed(t *testing.T) {
	cases := []string{
		`<div hx-target="this">x</div>`,
		`<div hx-target="closest .widget">x</div>`,
		`<div hx-target="find .row">x</div>`,
		`<div hx-target="#widget-foo">x</div>`,
		`<div hx-target="#w-bar">x</div>`,
	}
	for _, tpl := range cases {
		doc := &widgetDoc{Slug: "x", Template: tpl}
		findings := lintWidgetTemplate(doc)
		if findHasRule(findings, "hx-target") {
			t.Errorf("hx-target false-positive on %q: %+v", tpl, findings)
		}
	}
}

// TestLint_HxTargetRejected: hx-target may not reach outside the widget
// (ka-0000000776).
func TestLint_HxTargetRejected(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<div hx-target="body">x</div>`,
	}
	findings := lintWidgetTemplate(doc)
	if !findHasRule(findings, "hx-target") {
		t.Errorf("expected hx-target rejection for 'body', got %+v", findings)
	}
}

// TestLint_TemplateMissing: a widget with no template is flagged
// (ka-0000000776).
func TestLint_TemplateMissing(t *testing.T) {
	doc := &widgetDoc{Slug: "x"}
	findings := lintWidgetTemplate(doc)
	if !findHasRule(findings, "template-missing") {
		t.Errorf("expected template-missing, got %+v", findings)
	}
}

// TestLint_SchemaParseError: a schema that is not JSON is flagged
// (ka-0000000776).
func TestLint_SchemaParseError(t *testing.T) {
	doc := &widgetDoc{
		Slug:     "x",
		Template: `<div>ok</div>`,
		Schema:   `{not json`,
	}
	findings := lintWidgetTemplate(doc)
	if !findHasRule(findings, "schema-parse") {
		t.Errorf("expected schema-parse, got %+v", findings)
	}
}

func TestStageFromLabels(t *testing.T) {
	cases := []struct {
		labels []string
		want   string
	}{
		{[]string{"widget-stage:stable"}, "stable"},
		{[]string{"widget-stage:draft", "project:foo"}, "draft"},
		{[]string{"widget-stage:deprecated"}, "deprecated"},
		{[]string{"project:foo"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := stageFromLabels(c.labels); got != c.want {
			t.Errorf("stageFromLabels(%v) = %q, want %q", c.labels, got, c.want)
		}
	}
}

func findHasRule(findings []lintFinding, rule string) bool {
	for _, f := range findings {
		if f.Rule == rule {
			return true
		}
	}
	return false
}
