package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestReadDataArg_Inline(t *testing.T) {
	got, err := readDataArg(`{"a":1}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("got %q", got)
	}
}

func TestReadDataArg_Empty(t *testing.T) {
	got, err := readDataArg("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %q", got)
	}
}

func TestReadDataArg_FilePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(path, []byte(`{"hello":"world"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readDataArg("@" + path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"hello":"world"}` {
		t.Errorf("got %q", got)
	}
}

func TestPickWidgetTypeVersion_Empty(t *testing.T) {
	if got := pickWidgetTypeVersion(nil, ""); got != nil {
		t.Errorf("expected nil for empty list, got %+v", got)
	}
}

func TestPickWidgetTypeVersion_LatestWhenUnspecified(t *testing.T) {
	all := []*beadsapi.BeadDetail{
		{ID: "gd-1", Fields: map[string]string{"widget_version": "1"}},
		{ID: "gd-2", Fields: map[string]string{"widget_version": "2"}},
		{ID: "gd-3", Fields: map[string]string{"widget_version": "3"}},
	}
	got := pickWidgetTypeVersion(all, "")
	if got == nil || got.ID != "gd-3" {
		t.Errorf("expected gd-3, got %+v", got)
	}
}

func TestPickWidgetTypeVersion_Specific(t *testing.T) {
	all := []*beadsapi.BeadDetail{
		{ID: "gd-1", Fields: map[string]string{"widget_version": "1"}},
		{ID: "gd-2", Fields: map[string]string{"widget_version": "2"}},
	}
	got := pickWidgetTypeVersion(all, "1")
	if got == nil || got.ID != "gd-1" {
		t.Errorf("expected gd-1, got %+v", got)
	}
	if pickWidgetTypeVersion(all, "99") != nil {
		t.Errorf("expected nil for missing version")
	}
}

func TestValidateAgainstSchema_Required(t *testing.T) {
	wt := &beadsapi.BeadDetail{
		ID: "gd-w",
		Fields: map[string]string{
			"schema": `{"type":"object","required":["title"],"properties":{"title":{"type":"string"}}}`,
		},
	}
	msgs := validateAgainstSchema(wt, map[string]any{})
	if len(msgs) == 0 {
		t.Fatalf("expected required violation, got none")
	}
	if !msgs[0].fatal || !strings.Contains(msgs[0].msg, "title") {
		t.Errorf("unexpected first msg: %+v", msgs[0])
	}
}

func TestValidateAgainstSchema_TypeMismatch(t *testing.T) {
	wt := &beadsapi.BeadDetail{
		Fields: map[string]string{
			"schema": `{"properties":{"count":{"type":"integer"}}}`,
		},
	}
	msgs := validateAgainstSchema(wt, map[string]any{"count": "not-a-number"})
	if len(msgs) == 0 || !msgs[0].fatal {
		t.Fatalf("expected type-mismatch fatal, got %+v", msgs)
	}
}

func TestValidateAgainstSchema_AdditionalProperties(t *testing.T) {
	wt := &beadsapi.BeadDetail{
		Fields: map[string]string{
			"schema": `{"properties":{"name":{"type":"string"}},"additionalProperties":false}`,
		},
	}
	msgs := validateAgainstSchema(wt, map[string]any{"name": "ok", "extra": 42})
	if len(msgs) == 0 {
		t.Fatalf("expected unknown-property warning")
	}
	for _, m := range msgs {
		if m.fatal {
			t.Errorf("additionalProperties violation should not be fatal, got %+v", m)
		}
	}
}

func TestValidateAgainstSchema_NilOrMissing(t *testing.T) {
	if msgs := validateAgainstSchema(nil, map[string]any{}); msgs != nil {
		t.Errorf("nil widget-type should produce no msgs, got %+v", msgs)
	}
	wt := &beadsapi.BeadDetail{Fields: map[string]string{}}
	if msgs := validateAgainstSchema(wt, map[string]any{}); msgs != nil {
		t.Errorf("missing schema should produce no msgs, got %+v", msgs)
	}
}

func TestJSONTypeMatches(t *testing.T) {
	cases := []struct {
		v    any
		want string
		ok   bool
	}{
		{"hi", "string", true},
		{42.0, "number", true},
		{42.0, "integer", true},
		{42.5, "integer", false},
		{true, "boolean", true},
		{map[string]any{}, "object", true},
		{[]any{}, "array", true},
		{nil, "null", true},
		{"hi", "integer", false},
	}
	for _, c := range cases {
		if got := jsonTypeMatches(c.v, c.want); got != c.ok {
			t.Errorf("jsonTypeMatches(%v, %q) = %v, want %v", c.v, c.want, got, c.ok)
		}
	}
}

func TestSampleFromSchema_Required(t *testing.T) {
	wt := &beadsapi.BeadDetail{
		Fields: map[string]string{
			"schema": `{"required":["title","count"],"properties":{"title":{"type":"string"},"count":{"type":"integer","example":3}}}`,
		},
	}
	got := sampleFromSchema(wt)
	if !strings.Contains(got, `"title"`) || !strings.Contains(got, `"count"`) {
		t.Errorf("expected sample to mention required fields, got %q", got)
	}
	if !strings.Contains(got, "3") {
		t.Errorf("expected example value to surface, got %q", got)
	}
}

func TestSampleFromSchema_Empty(t *testing.T) {
	wt := &beadsapi.BeadDetail{Fields: map[string]string{}}
	if sampleFromSchema(wt) != "" {
		t.Errorf("expected empty sample for missing schema")
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(errString("status 404 not found")) {
		t.Errorf("expected 404 to be not-found")
	}
	if !isNotFound(errString("bead not found")) {
		t.Errorf("expected 'not found' to be not-found")
	}
	if isNotFound(nil) {
		t.Errorf("nil should not be not-found")
	}
	if isNotFound(errString("server error")) {
		t.Errorf("unrelated error should not be not-found")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
