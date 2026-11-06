package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestValidateWidgetTypeFields_EmptyAndBuiltin(t *testing.T) {
	if err := validateWidgetTypeFields(nil); err != nil {
		t.Fatalf("nil fields should be no-op, got %v", err)
	}
	// Default (no renderer_kind) is treated as builtin — template_source not required.
	fields := json.RawMessage(`{"slug":"foo"}`)
	if err := validateWidgetTypeFields(fields); err != nil {
		t.Fatalf("default renderer should be no-op, got %v", err)
	}
	// Explicit builtin — same.
	fields = json.RawMessage(`{"slug":"foo","renderer_kind":"builtin"}`)
	if err := validateWidgetTypeFields(fields); err != nil {
		t.Fatalf("builtin renderer should be no-op, got %v", err)
	}
}

func TestValidateWidgetTypeFields_HTMXRequiresTemplateSource(t *testing.T) {
	fields := json.RawMessage(`{"slug":"foo","renderer_kind":"htmx"}`)
	err := validateWidgetTypeFields(fields)
	if err == nil {
		t.Fatalf("expected error: htmx without template_source")
	}
	if !strings.Contains(err.Error(), "template_source") {
		t.Errorf("error should mention template_source, got %v", err)
	}
}

func TestValidateWidgetTypeFields_HTMXTemplateCompiles(t *testing.T) {
	fields := json.RawMessage(`{"slug":"foo","renderer_kind":"htmx","template_source":"<div>{{ .headline }}</div>"}`)
	if err := validateWidgetTypeFields(fields); err != nil {
		t.Fatalf("valid htmx template should compile, got %v", err)
	}
}

func TestValidateWidgetTypeFields_HTMXMalformedTemplateRejected(t *testing.T) {
	// Unclosed action — html/template Parse fails.
	fields := json.RawMessage(`{"slug":"foo","renderer_kind":"htmx","template_source":"<div>{{ .headline </div>"}`)
	err := validateWidgetTypeFields(fields)
	if err == nil {
		t.Fatalf("expected template parse error")
	}
	if !strings.Contains(err.Error(), "compile") {
		t.Errorf("error should mention compile failure, got %v", err)
	}
}

func TestValidateWidgetTypeFields_UnsupportedEngineRejected(t *testing.T) {
	fields := json.RawMessage(`{"slug":"foo","renderer_kind":"htmx","template_source":"<div></div>","template_engine":"jinja2"}`)
	err := validateWidgetTypeFields(fields)
	if err == nil {
		t.Fatalf("expected unsupported engine error")
	}
	if !strings.Contains(err.Error(), "template_engine") {
		t.Errorf("error should mention template_engine, got %v", err)
	}
}

func TestValidateWidgetTypeFields_MalformedJSONIgnored(t *testing.T) {
	if err := validateWidgetTypeFields(json.RawMessage(`{not json`)); err != nil {
		t.Fatalf("malformed JSON should be ignored defensively, got %v", err)
	}
}

// --- widget-instance validation ---

func newWidgetTypeBead(t *testing.T, id, slug, version string, schema string) *model.Bead {
	t.Helper()
	fieldsMap := map[string]any{
		"slug":           slug,
		"widget_version": version,
	}
	if schema != "" {
		fieldsMap["schema"] = json.RawMessage(schema)
	}
	raw, err := json.Marshal(fieldsMap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &model.Bead{
		ID:     id,
		Type:   "widget-type",
		Kind:   model.KindData,
		Status: model.StatusOpen,
		Fields: raw,
	}
}

func newWidgetMockStore(beads ...*model.Bead) *mockStore {
	m := &mockStore{
		beads:    map[string]*model.Bead{},
		labels:   map[string][]string{},
		comments: map[string][]*model.Comment{},
		deps:     map[string][]*model.Dependency{},
		configs:  map[string]*model.Config{},
	}
	for _, b := range beads {
		m.beads[b.ID] = b
	}
	return m
}

func TestValidateWidgetInstanceFields_EmptyAndUnknown(t *testing.T) {
	ctx := context.Background()
	m := newWidgetMockStore()
	if err := validateWidgetInstanceFields(ctx, m, nil); err != nil {
		t.Fatalf("nil fields should be no-op, got %v", err)
	}
	// Missing widget_type — generic validation handles required-ness.
	if err := validateWidgetInstanceFields(ctx, m, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("missing widget_type should defer to generic validation, got %v", err)
	}
	// Unknown slug — reject.
	err := validateWidgetInstanceFields(ctx, m, json.RawMessage(`{"widget_type":"nope"}`))
	if err == nil {
		t.Fatalf("expected unknown widget_type error")
	}
	if !strings.Contains(err.Error(), "unknown widget_type") {
		t.Errorf("error should mention unknown widget_type, got %v", err)
	}
}

func TestValidateWidgetInstanceFields_NoSchemaAccepts(t *testing.T) {
	ctx := context.Background()
	tb := newWidgetTypeBead(t, "gd-wtype1", "spark", "v1", "")
	m := newWidgetMockStore(tb)
	fields := json.RawMessage(`{"widget_type":"spark","widget_version":"v1","data":{"any":"shape"}}`)
	if err := validateWidgetInstanceFields(ctx, m, fields); err != nil {
		t.Fatalf("no-schema type should accept any data, got %v", err)
	}
}

func TestValidateWidgetInstanceFields_SchemaValidates(t *testing.T) {
	ctx := context.Background()
	schema := `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"required":["title","count"],
		"properties":{"title":{"type":"string"},"count":{"type":"integer","minimum":0}}
	}`
	tb := newWidgetTypeBead(t, "gd-wtype2", "stat", "v1", schema)
	m := newWidgetMockStore(tb)

	good := json.RawMessage(`{"widget_type":"stat","widget_version":"v1","data":{"title":"hi","count":3}}`)
	if err := validateWidgetInstanceFields(ctx, m, good); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}

	missing := json.RawMessage(`{"widget_type":"stat","widget_version":"v1","data":{"title":"hi"}}`)
	if err := validateWidgetInstanceFields(ctx, m, missing); err == nil {
		t.Fatalf("expected missing-required field error")
	}

	wrongType := json.RawMessage(`{"widget_type":"stat","widget_version":"v1","data":{"title":"hi","count":-1}}`)
	if err := validateWidgetInstanceFields(ctx, m, wrongType); err == nil {
		t.Fatalf("expected schema violation on count<0")
	}
}

func TestValidateWidgetInstanceFields_VersionMismatch(t *testing.T) {
	ctx := context.Background()
	tb := newWidgetTypeBead(t, "gd-wtype3", "graph", "v1", "")
	m := newWidgetMockStore(tb)
	// Version mismatch — no matching widget-type bead, should reject.
	fields := json.RawMessage(`{"widget_type":"graph","widget_version":"v2"}`)
	err := validateWidgetInstanceFields(ctx, m, fields)
	if err == nil {
		t.Fatalf("expected unknown widget_type error for version mismatch")
	}
}

// --- startup compile ---

func TestCompileAllWidgetTemplates_AllValid(t *testing.T) {
	ctx := context.Background()
	tb1 := newWidgetTypeBead(t, "gd-w1", "a", "v1", "")
	tb2 := &model.Bead{
		ID: "gd-w2", Type: "widget-type", Kind: model.KindData, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"slug":"b","renderer_kind":"htmx","template_source":"<p>{{.x}}</p>"}`),
	}
	m := newWidgetMockStore(tb1, tb2)
	if err := CompileAllWidgetTemplates(ctx, m); err != nil {
		t.Fatalf("expected clean compile, got %v", err)
	}
}

func TestCompileAllWidgetTemplates_FailsOnBadTemplate(t *testing.T) {
	ctx := context.Background()
	bad := &model.Bead{
		ID: "gd-w3", Type: "widget-type", Kind: model.KindData, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"slug":"bad","renderer_kind":"htmx","template_source":"<p>{{ .x"}`),
	}
	m := newWidgetMockStore(bad)
	err := CompileAllWidgetTemplates(ctx, m)
	if err == nil {
		t.Fatalf("expected compile failure")
	}
	if !strings.Contains(err.Error(), "gd-w3") {
		t.Errorf("error should mention failing bead id, got %v", err)
	}
}

func TestCompileAllWidgetTemplates_FailsOnEmptyTemplate(t *testing.T) {
	ctx := context.Background()
	bad := &model.Bead{
		ID: "gd-w4", Type: "widget-type", Kind: model.KindData, Status: model.StatusOpen,
		Fields: json.RawMessage(`{"slug":"empty","renderer_kind":"htmx"}`),
	}
	m := newWidgetMockStore(bad)
	err := CompileAllWidgetTemplates(ctx, m)
	if err == nil {
		t.Fatalf("expected error for htmx with no template")
	}
}
