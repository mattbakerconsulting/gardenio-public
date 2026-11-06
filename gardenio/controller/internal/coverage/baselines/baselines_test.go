package baselines

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/coverage/components"
	"gardenio/controller/internal/coverage/lcov"
)

// makeSample synthesises a PipelineSample whose LCOV records pin the
// per-component covered/executable line counts for a given component.
func makeSample(pipeline, commit string, perComponent map[components.Component][2]int) PipelineSample {
	f := lcov.New()
	pathPrefix := map[components.Component]string{
		components.Controller: "gardenio/controller/foo.go",
		components.Kbeads:     "kbeads/foo.go",
		components.Coop:       "coop/crates/cli/src/foo.rs",
	}
	for comp, ce := range perComponent {
		covered, exec := ce[0], ce[1]
		rec := f.Get(pathPrefix[comp])
		for i := 1; i <= exec; i++ {
			if i <= covered {
				rec.Lines[i] = 1
			} else {
				rec.Lines[i] = 0
			}
		}
	}
	return PipelineSample{Pipeline: pipeline, Commit: commit, LCOV: f}
}

func TestCompute_MedianAcrossPipelines(t *testing.T) {
	// 3 pipelines: controller covers 60%, 70%, 80% → median 70%.
	// kbeads covers 90%, 50%, 70% → sorted 50,70,90 → median 70%.
	samples := []PipelineSample{
		makeSample("p1", "sha1", map[components.Component][2]int{
			components.Controller: {60, 100},
			components.Kbeads:     {90, 100},
			components.Coop:       {80, 100},
		}),
		makeSample("p2", "sha2", map[components.Component][2]int{
			components.Controller: {70, 100},
			components.Kbeads:     {50, 100},
			components.Coop:       {80, 100},
		}),
		makeSample("p3", "sha3", map[components.Component][2]int{
			components.Controller: {80, 100},
			components.Kbeads:     {70, 100},
			components.Coop:       {80, 100},
		}),
	}
	out := Compute(samples, time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	if got := out.Components[components.Controller].BaselinePct; got != 70.0 {
		t.Errorf("controller baseline: got %v, want 70.0", got)
	}
	if got := out.Components[components.Kbeads].BaselinePct; got != 70.0 {
		t.Errorf("kbeads baseline: got %v, want 70.0", got)
	}
	if got := out.Components[components.Coop].BaselinePct; got != 80.0 {
		t.Errorf("coop baseline: got %v, want 80.0", got)
	}
}

func TestCompute_MedianAbsorbsOutlier(t *testing.T) {
	// The §8.1 motivating example: 9/10 pipelines @ 65%, 1 @ 90%.
	// Median says 65% (sustainable threshold); mean would say ~67.5%.
	samples := make([]PipelineSample, 0, 10)
	for i := range 9 {
		samples = append(samples, makeSample(itoa(i), "sha", map[components.Component][2]int{
			components.Controller: {65, 100},
		}))
	}
	samples = append(samples, makeSample("9", "sha", map[components.Component][2]int{
		components.Controller: {90, 100},
	}))
	out := Compute(samples, time.Now())
	if got := out.Components[components.Controller].BaselinePct; got != 65.0 {
		t.Errorf("expected median 65.0 to absorb single 90%% outlier, got %v", got)
	}
}

func TestCompute_ThresholdFormula(t *testing.T) {
	// baseline 71.4 → threshold max(60, 71.4 - 10) = 61.4.
	// baseline 65   → threshold max(60, 55)        = 60.
	// baseline 95   → threshold max(60, 85)        = 85.
	cases := []struct {
		covered, exec int
		wantBaseline  float64
		wantThreshold float64
	}{
		{714, 1000, 71.4, 61.4},
		{650, 1000, 65.0, 60.0},
		{950, 1000, 95.0, 85.0},
	}
	for _, tc := range cases {
		out := Compute([]PipelineSample{
			makeSample("p", "sha", map[components.Component][2]int{
				components.Controller: {tc.covered, tc.exec},
			}),
		}, time.Now())
		got := out.Components[components.Controller]
		if got.BaselinePct != tc.wantBaseline {
			t.Errorf("baseline: got %v, want %v", got.BaselinePct, tc.wantBaseline)
		}
		if got.ThresholdPct != tc.wantThreshold {
			t.Errorf("threshold: got %v, want %v", got.ThresholdPct, tc.wantThreshold)
		}
	}
}

func TestCompute_EvenLengthMedian(t *testing.T) {
	// 4 samples at 60, 70, 80, 90 → median = (70+80)/2 = 75.
	pcts := []int{60, 70, 80, 90}
	samples := make([]PipelineSample, 0, len(pcts))
	for i, p := range pcts {
		samples = append(samples, makeSample(itoa(i), "sha", map[components.Component][2]int{
			components.Controller: {p, 100},
		}))
	}
	out := Compute(samples, time.Now())
	if got := out.Components[components.Controller].BaselinePct; got != 75.0 {
		t.Errorf("even-length median: got %v, want 75.0", got)
	}
}

func TestCompute_SkipsPipelinesMissingComponent(t *testing.T) {
	// Pipeline 2 has no controller code at all (component didn't exist
	// yet, or was fully excluded). Median should be over the 2 pipelines
	// that did record controller coverage, not pulled to 0 by pipeline 2.
	samples := []PipelineSample{
		makeSample("p1", "sha1", map[components.Component][2]int{
			components.Controller: {70, 100},
			components.Kbeads:     {80, 100},
		}),
		makeSample("p2", "sha2", map[components.Component][2]int{
			// No controller entry at all.
			components.Kbeads: {80, 100},
		}),
		makeSample("p3", "sha3", map[components.Component][2]int{
			components.Controller: {72, 100},
			components.Kbeads:     {80, 100},
		}),
	}
	out := Compute(samples, time.Now())
	// median of [70, 72] = 71
	if got := out.Components[components.Controller].BaselinePct; got != 71.0 {
		t.Errorf("expected pipeline-without-component to be skipped, got %v", got)
	}
}

func TestCompute_NonGatedComponentExcluded(t *testing.T) {
	// Even if a non-gated component shows up in LCOV, it is not in
	// components.Gated and must not appear in the output.
	const nonGated = components.Component("ungated")
	samples := []PipelineSample{
		makeSample("p1", "sha1", map[components.Component][2]int{
			components.Controller: {70, 100},
			nonGated:              {99, 100},
		}),
	}
	out := Compute(samples, time.Now())
	if _, ok := out.Components[nonGated]; ok {
		t.Error("non-gated component must not appear in baselines output")
	}
	if _, ok := out.Components[components.Controller]; !ok {
		t.Error("controller should appear")
	}
}

func TestCompute_UnmeasuredComponentOmitted(t *testing.T) {
	// ci.yml uploads no coop LCOV, so coop has no samples. It must be left
	// out (the gate then uses MinThresholdPct), not written as threshold 0.
	out := Compute([]PipelineSample{
		makeSample("p1", "sha1", map[components.Component][2]int{components.Controller: {70, 100}}),
	}, time.Now())
	if got, ok := out.Components[components.Coop]; ok {
		t.Errorf("coop has no samples and must be omitted, got %+v", got)
	}
}

func TestCompute_MetadataFields(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 34, 56, 0, time.UTC)
	samples := []PipelineSample{
		makeSample("5355", "abc1111", map[components.Component][2]int{components.Controller: {70, 100}}),
		makeSample("5358", "abc2222", map[components.Component][2]int{components.Controller: {70, 100}}),
	}
	out := Compute(samples, now)
	if !out.ComputedAt.Equal(now) {
		t.Errorf("computed_at: got %v, want %v", out.ComputedAt, now)
	}
	if !strings.Contains(out.Method, "median") {
		t.Errorf("method should mention median: %q", out.Method)
	}
	if !strings.Contains(out.Method, "2 ") {
		t.Errorf("method should mention sample count: %q", out.Method)
	}
	if len(out.SourcePipelines) != 2 || out.SourcePipelines[0] != "5355" {
		t.Errorf("source_pipelines: %v", out.SourcePipelines)
	}
	if len(out.SourceCommits) != 2 || out.SourceCommits[1] != "abc2222" {
		t.Errorf("source_commits: %v", out.SourceCommits)
	}
}

func TestCompute_JSONShape(t *testing.T) {
	// Verify the marshalled JSON has the field names the patch-cov library
	// (W2b-1) and the §8.1 spec example expect.
	now := time.Date(2026, 5, 7, 12, 34, 56, 0, time.UTC)
	out := Compute([]PipelineSample{
		makeSample("p1", "sha1", map[components.Component][2]int{
			components.Controller: {70, 100},
		}),
	}, now)
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		`"computed_at":"2026-05-07T12:34:56Z"`,
		`"method":"median over 1 green release-tag or nightly ci.yml run"`,
		`"source_pipelines":["p1"]`,
		`"source_commits":["sha1"]`,
		`"components":{`,
		`"controller":{"baseline_pct":70,"threshold_pct":60}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected JSON to contain %q\nactual: %s", want, got)
		}
	}
}

func TestMedian_Empty(t *testing.T) {
	if got := median(nil); !math.IsNaN(got) {
		t.Errorf("expected NaN for empty input, got %v", got)
	}
}
