package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b []string
		want int
	}{
		{nil, nil, 0},
		{[]string{"a", "b", "c"}, []string{"a", "b", "c"}, 0},
		{[]string{"a", "b", "c"}, []string{"a", "x", "c"}, 1},
		{[]string{"a", "b"}, []string{"a"}, 1},
		{[]string{"a"}, []string{"a", "b"}, 1},
		{[]string{"coopmux"}, []string{"coop", "mucks"}, 2}, // delete + substitute
		{nil, []string{"x"}, 1},
	}
	for _, c := range cases {
		got := editDistance(c.a, c.b)
		if got != c.want {
			t.Errorf("editDistance(%v, %v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestTokenize(t *testing.T) {
	got := tokenize("Hello, World!  gd-ABC.")
	want := []string{"hello", "world", "gd-abc"}
	if len(got) != len(want) {
		t.Fatalf("tokenize: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("tokenize[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEvaluate_AggregateAndExitDelta(t *testing.T) {
	ds := &dataset{
		Conditions: []string{"no_bias", "glossary", "custom_lm"},
		Voices:     []string{"v1"},
		Items: []item{
			{
				ID:        "gd-AAA",
				Reference: "gd-AAA",
				Hypotheses: map[string]map[string]string{
					"v1": {
						"no_bias":   "k dash triple a", // 4 edits for 1-word ref (insertions beyond reference length)
						"glossary":  "gd-AAA",          // perfect
						"custom_lm": "gd-AAA",          // perfect
					},
				},
			},
			{
				ID:        "coopmux",
				Reference: "coopmux terminal mux",
				Hypotheses: map[string]map[string]string{
					"v1": {
						"no_bias":   "coop mucks terminal mix", // 2 substitutions + 1 insertion = 3 edits over 3 ref words
						"glossary":  "coopmux terminal mux",    // 0 edits
						"custom_lm": "coopmux terminal mux",    // 0 edits
					},
				},
			},
		},
	}
	_, agg := evaluate(ds)

	if _, ok := agg["no_bias"]; !ok {
		t.Fatal("aggregate missing no_bias")
	}
	if agg["glossary"] != 0.0 {
		t.Errorf("glossary agg = %.3f, want 0.0", agg["glossary"])
	}
	if agg["custom_lm"] != 0.0 {
		t.Errorf("custom_lm agg = %.3f, want 0.0", agg["custom_lm"])
	}
	if agg["no_bias"] <= 0 {
		t.Errorf("no_bias agg = %.3f, want > 0", agg["no_bias"])
	}

	delta, ok := exitDelta(agg)
	if !ok {
		t.Fatal("exitDelta missing data")
	}
	if delta != 0.0 {
		t.Errorf("delta = %.3f, want 0.0 (custom_lm ties glossary here)", delta)
	}
}

func TestWriteReport_MentionsExitCriterion(t *testing.T) {
	ds := &dataset{
		Conditions: []string{"glossary", "custom_lm"},
		Voices:     []string{"v1"},
		Items: []item{
			{
				ID:        "gd-X",
				Reference: "one two three four",
				Hypotheses: map[string]map[string]string{
					"v1": {
						"glossary":  "one too tree four",  // 2 edits
						"custom_lm": "one two three four", // 0 edits
					},
				},
			},
		},
	}
	per, agg := evaluate(ds)
	var buf bytes.Buffer
	if err := writeReport(&buf, ds, per, agg, 1); err != nil {
		t.Fatalf("writeReport: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "E2 exit criterion met") {
		t.Errorf("expected exit criterion met banner, got:\n%s", out)
	}
	if !strings.Contains(out, "Aggregate WER") {
		t.Errorf("expected aggregate section, got:\n%s", out)
	}
}
