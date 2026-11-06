package main

import (
	"strings"
	"testing"
)

func TestValidateProgressWrapup(t *testing.T) {
	score := 1
	cases := []struct {
		name      string
		w         *WrapUp
		wantCount int
	}{
		{"nil wrap-up", nil, 1},
		{"missing both", &WrapUp{Accomplishments: "x"}, 2},
		{"missing score", &WrapUp{ProgressSummary: "did a thing"}, 1},
		{"missing summary", &WrapUp{ProgressScore: &score}, 1},
		{"both present", &WrapUp{ProgressScore: &score, ProgressSummary: "shipped one MR"}, 0},
		{"score zero is valid", &WrapUp{ProgressScore: ptrInt(0), ProgressSummary: "no progress"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := validateProgressWrapup(tc.w)
			if len(issues) != tc.wantCount {
				t.Errorf("got %d issues, want %d: %v", len(issues), tc.wantCount, issues)
			}
		})
	}
}

func ptrInt(v int) *int { return &v }

func TestFormatProgressIssues(t *testing.T) {
	out := formatProgressIssues([]string{"a", "b"})
	if !strings.Contains(out, "progress.surface.in_wrapup") {
		t.Errorf("expected header, got: %s", out)
	}
	if !strings.Contains(out, "  - a") || !strings.Contains(out, "  - b") {
		t.Errorf("expected issues bullets, got: %s", out)
	}
	if !strings.Contains(out, "progress_score") || !strings.Contains(out, "progress_summary") {
		t.Errorf("expected example to mention both fields, got: %s", out)
	}
}

func TestWrapUp_RoundTripWithProgressFields(t *testing.T) {
	score := 2
	w := &WrapUp{
		Accomplishments: "x",
		ProgressScore:   &score,
		ProgressSummary: "shipped two MRs",
	}
	s, err := MarshalWrapUp(w)
	if err != nil {
		t.Fatalf("MarshalWrapUp: %v", err)
	}
	got, err := UnmarshalWrapUp(s)
	if err != nil {
		t.Fatalf("UnmarshalWrapUp: %v", err)
	}
	if got.ProgressScore == nil || *got.ProgressScore != 2 {
		t.Errorf("ProgressScore = %v, want 2", got.ProgressScore)
	}
	if got.ProgressSummary != "shipped two MRs" {
		t.Errorf("ProgressSummary = %q", got.ProgressSummary)
	}
}

func TestWrapUpFieldPresent_ProgressFields(t *testing.T) {
	score := 0
	cases := []struct {
		name  string
		w     *WrapUp
		field string
		want  bool
	}{
		{"score nil", &WrapUp{}, "progress_score", false},
		{"score zero present", &WrapUp{ProgressScore: &score}, "progress_score", true},
		{"summary empty", &WrapUp{}, "progress_summary", false},
		{"summary set", &WrapUp{ProgressSummary: "x"}, "progress_summary", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wrapUpFieldPresent(tc.w, tc.field); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
