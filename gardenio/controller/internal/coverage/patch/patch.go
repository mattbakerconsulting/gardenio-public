// Package patch computes per-component patch coverage from a merged LCOV
// tracefile, a unified diff (target...HEAD), per-component baselines, and
// a (possibly nil) Coverage-Bypass record.
//
// The algorithm follows docs/research/coverage-phase2b-design.md §3:
//
//  1. Identify changed-executable lines: from each diff'd post-change
//     file, intersect AddedLines with the LCOV record's DA: line set.
//     Whitespace and comment lines are silently dropped because they have
//     no DA: entry. Files absent from LCOV contribute zero executable
//     lines (e.g. .md, .yml).
//  2. Filter exclusions via the W3 shared list (gen/, *.pb.go, vendor/,
//     *_test.go, testdata/, fixtures/, ...). See exclusions.DefaultExcludes.
//  3. Project onto per-component buckets via components.Of. Cross-cutting
//     paths (scripts/, docs/, .github/, etc.) are dropped.
//  4. Per gated component, compute executable_lines, covered_lines,
//     percent.
//  5. Apply per-component thresholds from baselines (max(60, baseline-10)
//     per ka-0000006002).
//  6. Apply 10-line floor: components with <10 executable lines are
//     exempt for this PR.
//  7. Aggregate: PR passes iff every gated component is exempt or meets
//     its threshold.
//  8. Bypass override: a non-nil Bypass forces whole-PR pass while still
//     surfacing per-component numbers.
//
// The function is pure (no I/O, no globals); same inputs produce the same
// Result.
package patch

import (
	"math"
	"sort"

	"gardenio/controller/internal/coverage/components"
	"gardenio/controller/internal/coverage/diff"
	"gardenio/controller/internal/coverage/exclusions"
	"gardenio/controller/internal/coverage/lcov"
)

// LineFloor is the per-component floor below which patch coverage is
// not enforced (the component is "exempt" for this PR). See spec §3.2
// step 6.
const LineFloor = 10

// MinThresholdPct is the floor in the per-component threshold formula
// `max(MinThresholdPct, baseline_pct - 10)`. Per ka-0000006002.
const MinThresholdPct = 60.0

// Baselines holds per-component baseline + threshold percentages, as
// produced by W2b-6 (one-shot computation). Keys are component names
// (controller/kbeads/coop). Empty / missing entries cause Compute to
// fall back to MinThresholdPct.
type Baselines struct {
	Components map[components.Component]ComponentBaseline
}

// ComponentBaseline pins the baseline + threshold for one component.
type ComponentBaseline struct {
	BaselinePct  float64
	ThresholdPct float64
}

// Bypass is the Coverage-Bypass: trailer record produced by W2b-5. A
// non-nil pointer here means "operator has approved skipping the gate";
// the per-component numbers are still computed for reporting.
type Bypass struct {
	Reason     string
	Author     string
	CommitSHA  string
	RawTrailer string
}

// FileResult is per-file detail useful to the bot comment (W2b-3) — the
// "lowest-coverage files" surface.
type FileResult struct {
	Path       string
	Component  components.Component
	Executable int
	Covered    int
	Percent    float64
}

// ComponentResult is per-component aggregate.
type ComponentResult struct {
	Component  components.Component
	Executable int
	Covered    int
	Percent    float64
	Threshold  float64
	Exempt     bool
	Pass       bool
}

// Result is the full output of Compute.
type Result struct {
	Pass       bool
	Bypass     *Bypass
	Components []ComponentResult
	// Files is the per-file list, sorted ascending by Percent (lowest
	// first) so callers can take the head N for "lowest coverage files".
	Files []FileResult
}

// Compute runs the patch-coverage algorithm. merged and df must be
// non-nil; baselines may be nil (defaults to MinThresholdPct everywhere)
// and bypass may be nil.
func Compute(merged *lcov.File, df *diff.Diff, baselines *Baselines, bypass *Bypass) Result {
	type bucket struct {
		exec    int
		covered int
	}
	buckets := map[components.Component]*bucket{}
	for _, c := range components.Gated {
		buckets[c] = &bucket{}
	}
	var files []FileResult

	for filePath, fd := range df.Files {
		if exclusions.IsExcludedDefault(filePath) {
			continue
		}
		comp := components.Of(filePath)
		if !components.IsGated(comp) {
			continue
		}
		rec := merged.Records[filePath]
		if rec == nil {
			// File not in LCOV — either non-source (md, yml) or never
			// reached by the test harness. Either way, no executable
			// lines contributed.
			continue
		}
		var exec, covered int
		for _, ln := range fd.AddedLines {
			hits, ok := rec.Lines[ln]
			if !ok {
				continue
			}
			exec++
			if hits > 0 {
				covered++
			}
		}
		if exec == 0 {
			continue
		}
		buckets[comp].exec += exec
		buckets[comp].covered += covered
		files = append(files, FileResult{
			Path:       filePath,
			Component:  comp,
			Executable: exec,
			Covered:    covered,
			Percent:    pct(covered, exec),
		})
	}

	res := Result{Bypass: bypass}
	allPass := true
	for _, c := range components.Gated {
		b := buckets[c]
		thr := thresholdFor(c, baselines)
		exempt := b.exec < LineFloor
		percent := pct(b.covered, b.exec)
		pass := exempt || percent >= thr
		if !pass {
			allPass = false
		}
		res.Components = append(res.Components, ComponentResult{
			Component:  c,
			Executable: b.exec,
			Covered:    b.covered,
			Percent:    percent,
			Threshold:  thr,
			Exempt:     exempt,
			Pass:       pass,
		})
	}
	res.Pass = allPass
	if bypass != nil {
		res.Pass = true
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].Percent != files[j].Percent {
			return files[i].Percent < files[j].Percent
		}
		// Stable tie-breaker: by path.
		return files[i].Path < files[j].Path
	})
	res.Files = files
	return res
}

func pct(num, den int) float64 {
	if den == 0 {
		return math.NaN()
	}
	return 100.0 * float64(num) / float64(den)
}

func thresholdFor(c components.Component, b *Baselines) float64 {
	if b == nil {
		return MinThresholdPct
	}
	cb, ok := b.Components[c]
	if !ok {
		return MinThresholdPct
	}
	if cb.ThresholdPct > 0 {
		return cb.ThresholdPct
	}
	// Fall back to the formula if only baseline_pct supplied.
	t := cb.BaselinePct - 10
	if t < MinThresholdPct {
		t = MinThresholdPct
	}
	return t
}
