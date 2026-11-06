package main

import (
	"strings"
	"testing"
)

func TestParseWidgetDoc_FourSections(t *testing.T) {
	body := `# Widget: foo

## Summary

Short summary line.

## Schema

` + "```json\n" + `{"type":"object","required":["title"]}
` + "```\n\n" + `## Sample payload

` + "```json\n" + `{"title":"hi"}
` + "```\n\n" + `## Template

` + "```html\n" + `<section>{{ .Title }}</section>
` + "```\n"

	doc, err := parseWidgetDoc([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.Summary != "Short summary line." {
		t.Errorf("summary = %q", doc.Summary)
	}
	if !strings.Contains(doc.Schema, `"type":"object"`) {
		t.Errorf("schema = %q", doc.Schema)
	}
	if !strings.Contains(doc.Sample, `"title":"hi"`) {
		t.Errorf("sample = %q", doc.Sample)
	}
	if !strings.Contains(doc.Template, "{{ .Title }}") {
		t.Errorf("template = %q", doc.Template)
	}
}

func TestParseWidgetDoc_HeadingAliases(t *testing.T) {
	body := `## Summary
hello

## Sample Payload

` + "```json\n" + `{"x":1}
` + "```\n\n" + `## Body

<div>x</div>
`
	doc, err := parseWidgetDoc([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.Summary != "hello" {
		t.Errorf("summary = %q", doc.Summary)
	}
	if !strings.Contains(doc.Sample, `"x":1`) {
		t.Errorf("sample = %q", doc.Sample)
	}
	if !strings.Contains(doc.Template, "<div>x</div>") {
		t.Errorf("template (Body alias) = %q", doc.Template)
	}
}

func TestParseWidgetDoc_HeadingInsideFenceIsText(t *testing.T) {
	body := `## Template

` + "```html\n" + `## not a heading
<p>ok</p>
` + "```\n"
	doc, err := parseWidgetDoc([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(doc.Template, "## not a heading") {
		t.Errorf("expected fenced text to remain, got %q", doc.Template)
	}
}

func TestUnwrapFence_Json(t *testing.T) {
	got := unwrapFence("```json\n{\"a\":1}\n```")
	if got != `{"a":1}` {
		t.Errorf("got %q", got)
	}
}

func TestUnwrapFence_NoFence(t *testing.T) {
	got := unwrapFence("plain\ntext")
	if got != "plain\ntext" {
		t.Errorf("got %q", got)
	}
}
