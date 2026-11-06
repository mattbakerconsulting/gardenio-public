package patch

import (
	"math"
	"strings"
	"testing"

	"gardenio/controller/internal/coverage/components"
	"gardenio/controller/internal/coverage/diff"
	"gardenio/controller/internal/coverage/lcov"
)

// loadFixtures parses testdata/sample.lcov and testdata/sample.diff. The
// fixture spans controller/kbeads/coop diffs (per the spec's "fixture
// coverage spans controller/kbeads/coop diffs" requirement) and includes
// excluded paths (gen/, *_test.go, beads3d/, docs/) to verify filtering.
func loadFixtures(t *testing.T) (*lcov.File, *diff.Diff) {
	t.Helper()
	merged, err := lcov.ParseFile("testdata/sample.lcov")
	if err != nil {
		t.Fatalf("parse lcov: %v", err)
	}
	df, err := diff.ParseFile("testdata/sample.diff")
	if err != nil {
		t.Fatalf("parse diff: %v", err)
	}
	return merged, df
}

func componentByName(res Result, c components.Component) *ComponentResult {
	for i := range res.Components {
		if res.Components[i].Component == c {
			return &res.Components[i]
		}
	}
	return nil
}

func TestComputeGolden(t *testing.T) {
	merged, df := loadFixtures(t)
	res := Compute(merged, df, nil, nil)

	wantComp := map[components.Component]struct {
		exec, covered int
		passLow       bool // pass at MinThresholdPct=60
	}{
		// bar.go: 8/11 covered, baz.go: 3/4 → 11/15
		components.Controller: {15, 11, true},
		// store.go: 12/12 fully covered
		components.Kbeads: {12, 12, true},
		// main.rs: 0/12 covered
		components.Coop: {12, 0, false},
	}
	for c, want := range wantComp {
		got := componentByName(res, c)
		if got == nil {
			t.Fatalf("missing component result for %s", c)
		}
		if got.Executable != want.exec {
			t.Errorf("%s executable = %d; want %d", c, got.Executable, want.exec)
		}
		if got.Covered != want.covered {
			t.Errorf("%s covered = %d; want %d", c, got.Covered, want.covered)
		}
		if got.Pass != want.passLow {
			t.Errorf("%s pass = %v; want %v", c, got.Pass, want.passLow)
		}
		if got.Exempt {
			t.Errorf("%s should not be exempt (>= floor)", c)
		}
	}

	if res.Pass {
		t.Error("PR should fail when coop is at 0% (below 60%)")
	}
	if res.Bypass != nil {
		t.Error("no bypass passed in; Bypass should be nil")
	}

	// Non-gated paths (e.g. beads3d/ in the fixtures) → no entry in Components.
	if got := componentByName(res, components.Component("beads3d")); got != nil {
		t.Errorf("non-gated component should not appear in gated results, got %+v", got)
	}

	// Files are sorted ascending by Percent — coop main.rs (0%) first.
	if len(res.Files) == 0 {
		t.Fatal("expected at least one FileResult")
	}
	if res.Files[0].Path != "coop/crates/cli/src/main.rs" {
		t.Errorf("lowest-coverage file = %s; want coop/crates/cli/src/main.rs", res.Files[0].Path)
	}
	// Excluded paths must NEVER appear in Files.
	for _, f := range res.Files {
		if strings.Contains(f.Path, "_test.go") ||
			strings.Contains(f.Path, "/gen/") ||
			strings.HasPrefix(f.Path, "beads3d/") ||
			strings.HasPrefix(f.Path, "docs/") {
			t.Errorf("excluded path leaked into Files: %s", f.Path)
		}
	}
}

func TestComputeBypass(t *testing.T) {
	merged, df := loadFixtures(t)
	bp := &Bypass{Reason: "emergency", CommitSHA: "abc123"}
	res := Compute(merged, df, nil, bp)
	if !res.Pass {
		t.Error("bypass should force overall Pass=true")
	}
	if res.Bypass == nil || res.Bypass.Reason != "emergency" {
		t.Error("Bypass record should be surfaced verbatim")
	}
	// Per-component numbers still computed.
	coop := componentByName(res, components.Coop)
	if coop == nil || coop.Executable == 0 {
		t.Error("per-component numbers should still be computed under bypass")
	}
}

func TestComputeBaselinesThreshold(t *testing.T) {
	merged, df := loadFixtures(t)
	// Set explicit thresholds; controller@73% and kbeads@100% pass at 70%,
	// coop@0% fails at 70%.
	b := &Baselines{Components: map[components.Component]ComponentBaseline{
		components.Controller: {BaselinePct: 80, ThresholdPct: 70},
		components.Kbeads:     {BaselinePct: 90, ThresholdPct: 70},
		components.Coop:       {BaselinePct: 80, ThresholdPct: 70},
	}}
	res := Compute(merged, df, b, nil)
	for _, cr := range res.Components {
		if cr.Threshold != 70 {
			t.Errorf("%s threshold = %v; want 70", cr.Component, cr.Threshold)
		}
	}
	ctl := componentByName(res, components.Controller)
	if !ctl.Pass {
		t.Error("controller (73%) should pass at threshold 70")
	}
	coop := componentByName(res, components.Coop)
	if coop.Pass {
		t.Error("coop (0%) should fail at threshold 70")
	}
}

func TestComputeFloorExempt(t *testing.T) {
	// Construct a tiny diff with <10 changed executable lines per
	// component → all gated components must be exempt and PR passes.
	merged := lcov.New()
	rec := merged.Get("gardenio/controller/internal/foo/bar.go")
	for i := 1; i <= 5; i++ {
		rec.Lines[i] = 0 // executable, uncovered
	}
	df := &diff.Diff{Files: map[string]*diff.FileDiff{
		"gardenio/controller/internal/foo/bar.go": {
			Path:       "gardenio/controller/internal/foo/bar.go",
			AddedLines: []int{1, 2, 3, 4, 5},
		},
	}}
	res := Compute(merged, df, nil, nil)
	for _, cr := range res.Components {
		if !cr.Exempt {
			t.Errorf("%s should be exempt (exec=%d <%d)", cr.Component, cr.Executable, LineFloor)
		}
		if !cr.Pass {
			t.Errorf("%s should pass via exemption", cr.Component)
		}
	}
	if !res.Pass {
		t.Error("PR should pass when every component is exempt under floor")
	}
}

func TestComputeAllCovered(t *testing.T) {
	merged := lcov.New()
	rec := merged.Get("gardenio/controller/internal/foo/bar.go")
	for i := 1; i <= 20; i++ {
		rec.Lines[i] = 1
	}
	df := &diff.Diff{Files: map[string]*diff.FileDiff{
		"gardenio/controller/internal/foo/bar.go": {
			Path:       "gardenio/controller/internal/foo/bar.go",
			AddedLines: lineRange(1, 20),
		},
	}}
	res := Compute(merged, df, nil, nil)
	ctl := componentByName(res, components.Controller)
	if ctl == nil || ctl.Percent != 100 || !ctl.Pass {
		t.Errorf("controller should be 100%% pass, got %+v", ctl)
	}
}

func TestComputeZeroChangedLines(t *testing.T) {
	// All diff'd files are non-executable (md, yml) — no DA entries.
	merged := lcov.New()
	df := &diff.Diff{Files: map[string]*diff.FileDiff{
		"docs/research/foo.md": {
			Path:       "docs/research/foo.md",
			AddedLines: []int{1, 2, 3},
		},
	}}
	res := Compute(merged, df, nil, nil)
	for _, cr := range res.Components {
		if !cr.Exempt {
			t.Errorf("%s should be exempt with no executable lines", cr.Component)
		}
	}
	if !res.Pass {
		t.Error("PR with only non-executable changes should pass")
	}
}

func TestComputeExclusionsRespected(t *testing.T) {
	// A diff that only touches generated paths must not contribute to
	// any component's executable count.
	merged := lcov.New()
	rec := merged.Get("kbeads/gen/proto/foo.pb.go")
	for i := 1; i <= 20; i++ {
		rec.Lines[i] = 0
	}
	df := &diff.Diff{Files: map[string]*diff.FileDiff{
		"kbeads/gen/proto/foo.pb.go": {
			Path:       "kbeads/gen/proto/foo.pb.go",
			AddedLines: lineRange(1, 20),
		},
	}}
	res := Compute(merged, df, nil, nil)
	for _, cr := range res.Components {
		if cr.Executable != 0 {
			t.Errorf("%s should have 0 exec lines (all excluded), got %d", cr.Component, cr.Executable)
		}
	}
	if !res.Pass {
		t.Error("PR with only generated-path changes should pass")
	}
}

func TestComputeFailWithEnoughLines(t *testing.T) {
	merged := lcov.New()
	rec := merged.Get("coop/crates/cli/src/main.rs")
	for i := 1; i <= 12; i++ {
		rec.Lines[i] = 0
	}
	df := &diff.Diff{Files: map[string]*diff.FileDiff{
		"coop/crates/cli/src/main.rs": {
			Path:       "coop/crates/cli/src/main.rs",
			AddedLines: lineRange(1, 12),
		},
	}}
	res := Compute(merged, df, nil, nil)
	coop := componentByName(res, components.Coop)
	if coop == nil || coop.Pass || coop.Exempt {
		t.Errorf("coop should fail (0%% with 12 lines), got %+v", coop)
	}
	if res.Pass {
		t.Error("PR must fail")
	}
}

func TestThresholdFromBaselineFormula(t *testing.T) {
	cases := []struct {
		baseline, threshold, want float64
	}{
		{80, 0, 70},               // baseline-10 = 70 (above floor)
		{55, 0, MinThresholdPct},  // baseline-10 = 45 → floored to 60
		{0, 75, 75},               // explicit threshold honored
	}
	for _, c := range cases {
		b := &Baselines{Components: map[components.Component]ComponentBaseline{
			components.Coop: {BaselinePct: c.baseline, ThresholdPct: c.threshold},
		}}
		got := thresholdFor(components.Coop, b)
		if got != c.want {
			t.Errorf("thresholdFor(coop) baseline=%v threshold=%v = %v; want %v",
				c.baseline, c.threshold, got, c.want)
		}
	}
}

func TestPctZeroDen(t *testing.T) {
	if !math.IsNaN(pct(0, 0)) {
		t.Error("pct(0,0) should be NaN")
	}
}

func lineRange(lo, hi int) []int {
	out := make([]int, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}
