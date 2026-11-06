package bridge

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestClassifyStepDispatch_ExecHintWins(t *testing.T) {
	tests := []struct {
		name     string
		hint     string
		wantRec  DispatchRecommendation
		wantHint ExecHint
	}{
		{"dynamic-workflow", "dynamic-workflow", DispatchDynamicWorkflow, ExecHintDynamicWorkflow},
		{"pod-fanout", "pod-fanout", DispatchPodFanout, ExecHintPodFanout},
		{"separate-mrs", "separate-mrs", DispatchPodFanout, ExecHintSeparateMRs},
		{"whitespace trimmed", "  dynamic-workflow  ", DispatchDynamicWorkflow, ExecHintDynamicWorkflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build a step that would otherwise score 0 (pod-fanout), to prove
			// the exec_hint overrides the score in both directions.
			step := &beadsapi.BeadDetail{
				Title:  "some step",
				Fields: map[string]string{"exec_hint": tt.hint},
			}
			rec, sig := ClassifyStepDispatch(step)
			if rec != tt.wantRec {
				t.Errorf("rec = %q, want %q", rec, tt.wantRec)
			}
			if !sig.FromExecHint {
				t.Errorf("FromExecHint = false, want true")
			}
			if sig.ExecHint != tt.wantHint {
				t.Errorf("ExecHint = %q, want %q", sig.ExecHint, tt.wantHint)
			}
		})
	}
}

func TestClassifyStepDispatch_InvalidExecHintFallsThrough(t *testing.T) {
	// An unrecognised exec_hint must not short-circuit; we fall back to scoring.
	// This step has uniform skills + a keyword (2 signals) so it should still
	// recommend dynamic-workflow via the score, NOT via exec_hint.
	step := &beadsapi.BeadDetail{
		Title:  "audit every component",
		Labels: []string{"skill:code-review"},
		Fields: map[string]string{"exec_hint": "bogus-value"},
	}
	rec, sig := ClassifyStepDispatch(step)
	if sig.FromExecHint {
		t.Fatalf("FromExecHint = true for invalid hint, want false")
	}
	if rec != DispatchDynamicWorkflow {
		t.Errorf("rec = %q, want %q (score path)", rec, DispatchDynamicWorkflow)
	}
}

func TestClassifyStepDispatch_FourSignalScore(t *testing.T) {
	tests := []struct {
		name          string
		step          *beadsapi.BeadDetail
		wantRec       DispatchRecommendation
		wantScore     int
		wantUniform   bool
		wantInbound   bool
		wantKeyword   bool
		wantAnnot     bool
		wantInboundN  int
	}{
		{
			name:    "zero signals -> pod-fanout",
			step:    &beadsapi.BeadDetail{Title: "deploy the controller", Description: "ship it"},
			wantRec: DispatchPodFanout,
		},
		{
			name: "one signal (keyword only) -> pod-fanout",
			step: &beadsapi.BeadDetail{
				Title: "sweep the repo for secrets",
			},
			wantRec:     DispatchPodFanout,
			wantScore:   1,
			wantKeyword: true,
		},
		{
			name: "two signals (uniform skills + keyword) -> dynamic-workflow",
			step: &beadsapi.BeadDetail{
				Title:  "audit each module",
				Labels: []string{"skill:code-review"},
			},
			wantRec:     DispatchDynamicWorkflow,
			wantScore:   2,
			wantUniform: true,
			wantKeyword: true,
		},
		{
			name: "two signals (inbound>=3 + annotation) -> dynamic-workflow",
			step: &beadsapi.BeadDetail{
				Title: "process items",
				Fields: map[string]string{
					"inbound_items": `["a","b","c"]`,
					"on_complete":   `{"for_each":"{{scan.output.items}}","var":"x","expand":"gd-x"}`,
				},
			},
			wantRec:      DispatchDynamicWorkflow,
			wantScore:    2,
			wantInbound:  true,
			wantAnnot:    true,
			wantInboundN: 3,
		},
		{
			name: "inbound exactly below threshold (2 items) does not fire",
			step: &beadsapi.BeadDetail{
				Title:  "process items",
				Fields: map[string]string{"inbound_items": "a, b"},
			},
			wantRec:      DispatchPodFanout,
			wantScore:    0,
			wantInboundN: 2,
		},
		{
			name: "all four signals -> dynamic-workflow",
			step: &beadsapi.BeadDetail{
				Title:  "classify per-file findings",
				Labels: []string{"skill:code-review", "dwf-candidate"},
				Fields: map[string]string{
					"requires_skills": `["code-review"]`,
					"inbound_items":   "f1,f2,f3,f4",
				},
			},
			wantRec:      DispatchDynamicWorkflow,
			wantScore:    4,
			wantUniform:  true,
			wantInbound:  true,
			wantKeyword:  true,
			wantAnnot:    true,
			wantInboundN: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, sig := ClassifyStepDispatch(tt.step)
			if rec != tt.wantRec {
				t.Errorf("rec = %q, want %q (signals=%+v)", rec, tt.wantRec, sig)
			}
			if sig.FromExecHint {
				t.Errorf("FromExecHint = true, want false for scored path")
			}
			if sig.Score != tt.wantScore {
				t.Errorf("Score = %d, want %d", sig.Score, tt.wantScore)
			}
			if sig.UniformSkills != tt.wantUniform {
				t.Errorf("UniformSkills = %v, want %v", sig.UniformSkills, tt.wantUniform)
			}
			if sig.InboundFanout != tt.wantInbound {
				t.Errorf("InboundFanout = %v, want %v", sig.InboundFanout, tt.wantInbound)
			}
			if sig.Keyword != tt.wantKeyword {
				t.Errorf("Keyword = %v, want %v", sig.Keyword, tt.wantKeyword)
			}
			if sig.Annotation != tt.wantAnnot {
				t.Errorf("Annotation = %v, want %v", sig.Annotation, tt.wantAnnot)
			}
			if tt.wantInboundN != 0 && sig.InboundCount != tt.wantInboundN {
				t.Errorf("InboundCount = %d, want %d", sig.InboundCount, tt.wantInboundN)
			}
		})
	}
}

// TestInboundFanoutItems_IgnoresOutput is the regression guard for the timing
// correction: the inbound width signal must read items threaded INTO the step,
// never the step's own (empty, for a ready step) output/dwf_result.
func TestInboundFanoutItems_IgnoresOutput(t *testing.T) {
	step := &beadsapi.BeadDetail{
		Title: "ready step, has not run yet",
		Fields: map[string]string{
			// These are the step's OWN output — must be IGNORED.
			"dwf_result": `{"succeeded":["a","b","c","d","e"]}`,
			"output":     `["x","y","z"]`,
		},
	}
	if items := inboundFanoutItems(step); len(items) != 0 {
		t.Fatalf("inboundFanoutItems read the step's own output: got %v, want none", items)
	}
	// And the full classifier must therefore NOT fire the inbound signal.
	_, sig := ClassifyStepDispatch(step)
	if sig.InboundFanout {
		t.Errorf("InboundFanout fired off the step's own output (timing bug)")
	}
	if sig.InboundCount != 0 {
		t.Errorf("InboundCount = %d off output fields, want 0", sig.InboundCount)
	}
}

func TestInboundFanoutItems_ParsesJSONAndCSV(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"json array", `["a","b","c"]`, 3},
		{"csv", "a, b, c, d", 4},
		{"json with blanks", `["a","","b"]`, 2},
		{"csv with blanks", "a,,b,", 2},
		{"malformed json falls back to csv", `[a,b`, 2},
		{"empty", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := &beadsapi.BeadDetail{Fields: map[string]string{"inbound_items": tt.raw}}
			if got := len(inboundFanoutItems(step)); got != tt.want {
				t.Errorf("len = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRequiresSkills_FieldAndLabels(t *testing.T) {
	// Field takes priority.
	step := &beadsapi.BeadDetail{
		Labels: []string{"skill:go", "skill:k8s", "project:gardenio"},
		Fields: map[string]string{"requires_skills": `["code-review"]`},
	}
	got := requiresSkills(step)
	if len(got) != 1 || got[0] != "code-review" {
		t.Errorf("requiresSkills = %v, want [code-review] (field priority)", got)
	}
	// Falls back to skill: labels when field absent.
	step2 := &beadsapi.BeadDetail{Labels: []string{"skill:go", "skill:k8s", "project:gardenio"}}
	got2 := requiresSkills(step2)
	if len(got2) != 2 {
		t.Errorf("requiresSkills from labels = %v, want 2 skills", got2)
	}
}

func TestClassifyStepDispatch_NilStep(t *testing.T) {
	rec, sig := ClassifyStepDispatch(nil)
	if rec != DispatchPodFanout {
		t.Errorf("nil step rec = %q, want pod-fanout (conservative default)", rec)
	}
	if sig.Score != 0 || sig.FromExecHint {
		t.Errorf("nil step signals = %+v, want zero value", sig)
	}
}

func TestKeywordPattern(t *testing.T) {
	hits := []string{
		"Audit the codebase", "weekly SWEEP", "sanitize public release",
		"classify the failures", "per-file rewrite", "per-component build",
		"embarrassingly parallel scan", "embarrassingly-parallel scan",
		"fan-out across modules", "fanout across modules",
	}
	for _, s := range hits {
		if !keywordPattern.MatchString(s) {
			t.Errorf("expected keyword match for %q", s)
		}
	}
	misses := []string{"deploy the controller", "fix the bug", "refactoring"}
	for _, s := range misses {
		if keywordPattern.MatchString(s) {
			t.Errorf("unexpected keyword match for %q", s)
		}
	}
}
