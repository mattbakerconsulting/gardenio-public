package jqout

import (
	"bytes"
	"strings"
	"testing"
)

func apply(t *testing.T, filter string, v any) (string, error) {
	t.Helper()
	f, err := Parse(filter)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = f.Apply(&buf, v)
	return buf.String(), err
}

func TestParseInvalidFilter(t *testing.T) {
	if _, err := Parse(".["); err == nil {
		t.Fatal("expected parse error for incomplete filter, got nil")
	}
}

func TestBareArrayProjection(t *testing.T) {
	// The shape kd list/ready/search returns: a bare array.
	in := []any{
		map[string]any{"id": "gd-1", "status": "open"},
		map[string]any{"id": "gd-2", "status": "closed"},
	}
	out, err := apply(t, ".[].id", in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out != "gd-1\ngd-2\n" {
		t.Fatalf("got %q", out)
	}
}

func TestSingleObjectField(t *testing.T) {
	// The shape kd show returns: a single object. Raw-string output.
	in := map[string]any{"id": "gd-1", "status": "in_progress"}
	out, err := apply(t, ".status", in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out != "in_progress\n" {
		t.Fatalf("got %q", out)
	}
}

func TestLengthProjection(t *testing.T) {
	// count-then-detail collapses to one call: `length` off the same capture.
	in := []any{1, 2, 3}
	out, err := apply(t, "length", in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out != "3\n" {
		t.Fatalf("got %q", out)
	}
}

func TestCompositeOutputNoHTMLEscape(t *testing.T) {
	// Real jq prints <, >, & literally; json.Marshal would emit < etc.
	in := map[string]any{"title": "a <b> & c"}
	out, err := apply(t, "{title}", in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := `{"title":"a <b> & c"}` + "\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestEnvelopeKeyOnBareArrayHint(t *testing.T) {
	// The exact mistake advice ka-0000003286 warns about: `.beads[]` against a
	// bare-array response. Apply must surface the shape hint.
	in := []any{map[string]any{"id": "gd-1"}}
	_, err := apply(t, ".beads[].id", in)
	if err == nil {
		t.Fatal("expected runtime error for .beads[] on a bare array")
	}
	msg := err.Error()
	if !strings.Contains(msg, "BARE JSON array") || !strings.Contains(msg, "ka-0000003286") {
		t.Fatalf("hint missing shape guidance: %q", msg)
	}
}

func TestNoHintForEnvelopeKeyOnObject(t *testing.T) {
	// A correct `gb news --jq '.agents[]'` — envelope key against an OBJECT —
	// must NOT be flagged, even though the filter names an envelope key.
	in := map[string]any{"agents": []any{
		map[string]any{"name": "a"},
		map[string]any{"name": "b"},
	}}
	out, err := apply(t, ".agents[].name", in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "a\nb\n" {
		t.Fatalf("got %q", out)
	}
}

func TestNonEnvelopeRuntimeErrorNoHint(t *testing.T) {
	// A runtime error that is NOT the envelope-on-array mistake gets the plain
	// --jq prefix, not the shape hint. Indexing a bare array with a
	// non-envelope key ("foo") errors, but "foo" is not an envelope key so the
	// hint must stay silent.
	in := []any{map[string]any{"id": "gd-1"}}
	_, err := apply(t, ".foo", in)
	if err == nil {
		t.Fatal("expected runtime error indexing an array with a string key")
	}
	if strings.Contains(err.Error(), "BARE JSON array") {
		t.Fatalf("unexpected shape hint on unrelated error: %q", err.Error())
	}
}

func TestStructInputRoundTrip(t *testing.T) {
	// Apply must accept Go structs (the real callers pass *model.Bead etc.),
	// round-tripping them through JSON so gojq sees plain maps.
	type bead struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	out, err := apply(t, ".status", bead{ID: "gd-1", Status: "open"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out != "open\n" {
		t.Fatalf("got %q", out)
	}
}

func TestMultipleResultsOnePerLine(t *testing.T) {
	in := []any{
		map[string]any{"id": "gd-1", "n": 1.0},
		map[string]any{"id": "gd-2", "n": 2.0},
	}
	out, err := apply(t, ".[] | [.id, .n]", in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := `["gd-1",1]` + "\n" + `["gd-2",2]` + "\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}
