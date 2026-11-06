package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureLCOV / fixtureDiff piggyback on the patch package's golden
// fixtures so the binary stays in lockstep with library behaviour.
const (
	fixtureLCOV = "../../internal/coverage/patch/testdata/sample.lcov"
	fixtureDiff = "../../internal/coverage/patch/testdata/sample.diff"
)

// writeJSON marshals v to path; t.Fatalf on any error.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(v); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
}

// baselinesJSON builds a baselines.Output-shaped map for tests; we keep
// it shape-loose so we can exercise both the formula-fallback and the
// explicit-threshold branches.
func baselinesJSON(controllerBaseline, kbeadsBaseline, coopBaseline float64) map[string]any {
	return map[string]any{
		"computed_at": "2026-05-07T00:00:00Z",
		"method":      "test",
		"components": map[string]any{
			"controller": map[string]float64{"baseline_pct": controllerBaseline, "threshold_pct": 0},
			"kbeads":     map[string]float64{"baseline_pct": kbeadsBaseline, "threshold_pct": 0},
			"coop":       map[string]float64{"baseline_pct": coopBaseline, "threshold_pct": 0},
		},
	}
}

func TestRun_Synthetic(t *testing.T) {
	out := filepath.Join(t.TempDir(), "result.json")
	res, err := run("", "", "", "", "tooling-broken", "merge dispatch failed (HTTP 500)")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Pass {
		t.Fatalf("synthetic bypass must produce Pass=true, got false")
	}
	if res.Bypass == nil {
		t.Fatalf("synthetic bypass must populate Bypass record")
	}
	if !res.Bypass.Synthetic {
		t.Fatalf("synthetic flag must be true")
	}
	if res.Bypass.Reason != "tooling-broken" {
		t.Fatalf("unexpected reason %q", res.Bypass.Reason)
	}
	if res.Bypass.SyntheticDetail == "" {
		t.Fatalf("synthetic detail must be preserved")
	}

	if err := writeResult(out, res); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var roundtrip ResultJSON
	if err := json.Unmarshal(got, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !roundtrip.Pass || roundtrip.Bypass == nil || !roundtrip.Bypass.Synthetic {
		t.Fatalf("roundtripped JSON lost synthetic-bypass shape: %+v", roundtrip)
	}
}

func TestRun_FixtureWithBaselines(t *testing.T) {
	dir := t.TempDir()
	baselinesPath := filepath.Join(dir, "baselines.json")
	// Set baselines low enough that the formula-fallback (baseline-10)
	// floors to MinThresholdPct=60 — so the gate's pass/fail mirrors the
	// library's golden behaviour rather than being baseline-sensitive.
	writeJSON(t, baselinesPath, baselinesJSON(50, 50, 50))

	res, err := run(fixtureLCOV, fixtureDiff, baselinesPath, "", "", "")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(res.Components) != 3 {
		t.Fatalf("expected 3 gated components, got %d", len(res.Components))
	}
	for _, c := range res.Components {
		if c.Threshold != 60.0 {
			t.Errorf("component %s: expected threshold 60.0 (formula floor), got %.1f", c.Component, c.Threshold)
		}
	}

	// Sanity: at least one component carries diffed lines.
	hasExec := false
	for _, c := range res.Components {
		if c.Executable > 0 {
			hasExec = true
			break
		}
	}
	if !hasExec {
		t.Fatalf("fixtures should produce executable lines on at least one component")
	}
}

func TestRun_BypassTrailerFlipsResult(t *testing.T) {
	dir := t.TempDir()
	baselinesPath := filepath.Join(dir, "baselines.json")
	// Baseline 100 → threshold 90 (after floor). With the fixture diff
	// hitting partially-uncovered lines, this guarantees a real failure
	// without the bypass.
	writeJSON(t, baselinesPath, baselinesJSON(100, 100, 100))

	resNoBypass, err := run(fixtureLCOV, fixtureDiff, baselinesPath, "", "", "")
	if err != nil {
		t.Fatalf("run no-bypass: %v", err)
	}
	if resNoBypass.Pass {
		t.Fatalf("expected fail without bypass at threshold 90; got pass")
	}

	commitsPath := filepath.Join(dir, "commits.json")
	writeJSON(t, commitsPath, []map[string]string{{
		"id":          "abc1234567890",
		"author_name": "Test User",
		"message":     "fix: large refactor\n\nCoverage-Bypass: refactor\n",
	}})

	resWithBypass, err := run(fixtureLCOV, fixtureDiff, baselinesPath, commitsPath, "", "")
	if err != nil {
		t.Fatalf("run with bypass: %v", err)
	}
	if !resWithBypass.Pass {
		t.Fatalf("bypass trailer must flip gate to pass; got fail")
	}
	if resWithBypass.Bypass == nil || resWithBypass.Bypass.Reason != "refactor" {
		t.Fatalf("expected refactor bypass record, got %+v", resWithBypass.Bypass)
	}
	if resWithBypass.Bypass.Synthetic {
		t.Fatalf("real bypass must not be marked synthetic")
	}
}

func TestRun_BypassParseErrorTreatedAsNoBypass(t *testing.T) {
	dir := t.TempDir()
	baselinesPath := filepath.Join(dir, "baselines.json")
	writeJSON(t, baselinesPath, baselinesJSON(100, 100, 100))

	commitsPath := filepath.Join(dir, "commits.json")
	// Per-component scope is rejected — must be treated as "no bypass".
	writeJSON(t, commitsPath, []map[string]string{{
		"id":      "deadbeef",
		"message": "subject\n\nCoverage-Bypass: emergency component=coop\n",
	}})

	res, err := run(fixtureLCOV, fixtureDiff, baselinesPath, commitsPath, "", "")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Pass {
		t.Fatalf("invalid bypass trailer must NOT auto-pass; got pass")
	}
	if res.Bypass != nil {
		t.Fatalf("invalid trailer must not produce a bypass record")
	}
}

func TestRun_MissingBaselinesIsHardFail(t *testing.T) {
	dir := t.TempDir()
	_, err := run(fixtureLCOV, fixtureDiff, filepath.Join(dir, "missing.json"), "", "", "")
	if err == nil {
		t.Fatalf("expected hard fail when baselines.json is missing")
	}
}

func TestRun_MissingFlagsHardFail(t *testing.T) {
	cases := []struct {
		name     string
		lcov     string
		diff     string
		baseline string
	}{
		{"no lcov", "", "x", "y"},
		{"no diff", "x", "", "y"},
		{"no baselines", "x", "y", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := run(c.lcov, c.diff, c.baseline, "", "", "")
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestAppendMarkdownSummary(t *testing.T) {
	res := &ResultJSON{
		Pass: false,
		Components: []ComponentJSON{
			{Component: "controller", Executable: 20, Covered: 5, Percent: 25, Threshold: 60},
			{Component: "kbeads"},
		},
		Files: []FileJSON{
			{Path: "gardenio/controller/a.go", Executable: 20, Covered: 5, Percent: 25},
			{Path: "gardenio/controller/b.go", Executable: 3, Covered: 3, Percent: 100},
		},
	}
	path := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(path, []byte("earlier step\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendMarkdownSummary(path, "Coverage / Patch (controller)", res); err != nil {
		t.Fatalf("append: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"earlier step\n### Coverage / Patch (controller): FAIL",
		"| controller | 20 | 5 | 25.0% | 60.0% | **fail** |",
		"| `gardenio/controller/a.go` | 5 / 20 |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
	// No changed lines: kbeads row and the fully covered file stay out.
	for _, unwanted := range []string{"| kbeads |", "b.go"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("summary should not contain %q:\n%s", unwanted, got)
		}
	}

	empty := markdownSummary("t", &ResultJSON{Pass: true, Components: []ComponentJSON{{Component: "coop"}}})
	if !strings.Contains(empty, "### t: PASS") || !strings.Contains(empty, "No changed executable lines") {
		t.Errorf("empty summary = %q", empty)
	}
}

func TestSanitize(t *testing.T) {
	for _, in := range []float64{nan(), inf(+1), inf(-1)} {
		if got := sanitize(in); got != 0 {
			t.Errorf("sanitize(%v) = %v, want 0", in, got)
		}
	}
	if got := sanitize(42.5); got != 42.5 {
		t.Errorf("sanitize(42.5) = %v, want 42.5", got)
	}
}

func nan() float64 { return zero() / zero() }
func inf(sign int) float64 {
	if sign > 0 {
		return 1.0 / zero()
	}
	return -1.0 / zero()
}
func zero() float64 { return 0 }
