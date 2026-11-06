package main

import (
	"html/template"
	"strings"
	"testing"
	"time"
)

func TestWidgetStageFromLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{"draft", []string{"project:gardenio", "widget-stage:draft"}, "draft"},
		{"stable", []string{"widget-stage:stable"}, "stable"},
		{"published-aliases-stable", []string{"widget-stage:published"}, "stable"},
		{"deprecated", []string{"widget-stage:deprecated"}, "deprecated"},
		{"none", []string{"project:gardenio"}, ""},
		{"unknown-pass-through", []string{"widget-stage:beta"}, "beta"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := widgetStageFromLabels(c.labels); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestWidgetStageChipText(t *testing.T) {
	if got := widgetStageChipText("draft"); got != "DRAFT" {
		t.Errorf("draft → %q, want DRAFT", got)
	}
	if got := widgetStageChipText("deprecated"); got != "DEPRECATED" {
		t.Errorf("deprecated → %q, want DEPRECATED", got)
	}
	if got := widgetStageChipText("stable"); got != "stable" {
		t.Errorf("stable → %q, want stable (lowercase, quiet)", got)
	}
	if got := widgetStageChipText(""); got != "" {
		t.Errorf("empty → %q, want empty", got)
	}
}

func TestDerivePreviewPayload(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		want   string
	}{
		{"empty", "", "{}"},
		{"null", "null", "{}"},
		{"invalid-json", "{not json", "{}"},
		{"examples-first", `{"examples":[{"a":1},{"a":2}]}`, `{"a":1}`},
		{"default-fallback", `{"default":{"x":"y"}}`, `{"x":"y"}`},
		{"no-hint", `{"type":"object"}`, "{}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := derivePreviewPayload(c.schema); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestWidgetDiagnosticsRingBuffer(t *testing.T) {
	d := newWidgetDiagnostics()
	for range widgetDiagnosticsRingSize + 5 {
		d.Record("alert-summary", "render", "boom")
	}
	snap := d.Snapshot("alert-summary")
	if len(snap) != widgetDiagnosticsRingSize {
		t.Fatalf("ring size after %d records = %d, want %d",
			widgetDiagnosticsRingSize+5, len(snap), widgetDiagnosticsRingSize)
	}
	// Newest-first: When values must be monotonically nonincreasing.
	for i := 1; i < len(snap); i++ {
		if snap[i].When.After(snap[i-1].When) {
			t.Fatalf("snapshot not newest-first at i=%d", i)
		}
	}
	// Other slugs unaffected.
	if got := d.Snapshot("other-slug"); len(got) != 0 {
		t.Errorf("unrelated slug should be empty, got %d entries", len(got))
	}
}

func TestWidgetDiagnosticsNilSafe(t *testing.T) {
	var d *widgetDiagnostics
	d.Record("x", "render", "y") // must not panic
	if got := d.Snapshot("x"); got != nil {
		t.Errorf("nil snapshot want nil, got %v", got)
	}
}

func TestSafeExecuteTemplateRecoversFuncPanic(t *testing.T) {
	// A custom func that panics during Execute exercises the recover()
	// guard — without it the request goroutine would die. Plan §(5)
	// requires per-render recover() for free-htmx widgets.
	tmpl, err := template.New("t").Funcs(template.FuncMap{
		"boom": func() string { panic("kaboom") },
	}).Parse(`{{boom}}`)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	var buf strings.Builder
	err = safeExecuteTemplate(tmpl, &buf, nil)
	if err == nil {
		t.Fatal("expected error from panicking func, got nil")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("error should propagate panic message, got %q", err.Error())
	}
}

func TestWidgetDiagnosticsTimestampedRecent(t *testing.T) {
	d := newWidgetDiagnostics()
	d.Record("s", "render", "boom")
	snap := d.Snapshot("s")
	if len(snap) != 1 || time.Since(snap[0].When) > time.Minute {
		t.Fatalf("expected one fresh entry, got %v", snap)
	}
}
