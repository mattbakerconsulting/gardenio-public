// Package baselines computes per-component coverage baselines from a window
// of merged LCOV tracefiles produced by green release-tag and nightly ci.yml
// runs (scripts/coverage/regen-baselines.sh).
//
// One-shot use: per docs/research/coverage-phase2b-design.md §8, this package
// is invoked once at the 2a→2b boundary (and re-run by an operator on demand
// after major refactors). It does NOT recalibrate continuously — that would
// move the patch-cov gate's bar under reviewers' feet (anti-pattern called
// out in §8.2). Continuous ratcheting is a 2c concern with a different
// storage shape and read/write semantics.
//
// Algorithm (per spec §8.1):
//
//  1. For each LCOV (one per pipeline), bucket lines by component using
//     internal/coverage/components.Of. Beads3d is excluded — only the
//     gated components (controller, kbeads, coop) are baselined.
//  2. Per component per pipeline, compute line-coverage % as
//     covered / executable * 100, where executable = count of DA: lines
//     in the LCOV under the component's path prefix.
//  3. Across the per-pipeline samples, take the **median** (robust to a
//     single outlier pipeline; see §8.1 step 3 for the rationale vs mean).
//  4. A component with no executable lines in any sample is left out of
//     Components, so the gate falls back to MinThresholdPct for it rather
//     than reading a 0% baseline that was never measured.
//  5. Threshold per component: max(MinThresholdPct, baseline - 10), where
//     MinThresholdPct mirrors patch.MinThresholdPct (60.0). Imported via
//     a constant kept in this package so the schema can be authored
//     without depending on the patch package.
//
// Output schema mirrors the §8.1 example. The patch-cov gate (W2b-1) reads
// only Components[<name>].{BaselinePct, ThresholdPct}; top-level fields are
// metadata for human review and the audit log.
package baselines

import (
	"math"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/coverage/components"
	"gardenio/controller/internal/coverage/lcov"
)

// MinThresholdPct mirrors patch.MinThresholdPct (ka-0000006002). Defined
// here too so the baseline computation does not import the patch package.
const MinThresholdPct = 60.0

// PipelineSample is one (pipeline, commit, LCOV) tuple in the 10-pipeline
// window. Pipeline and Commit are recorded in the output for audit.
type PipelineSample struct {
	Pipeline string
	Commit   string
	LCOV     *lcov.File
}

// ComponentBaseline is one row of the components map in the output JSON.
type ComponentBaseline struct {
	BaselinePct  float64 `json:"baseline_pct"`
	ThresholdPct float64 `json:"threshold_pct"`
}

// Output is the full baselines.json schema. Only Components is consumed by
// the patch-cov gate; the rest is metadata.
type Output struct {
	ComputedAt      time.Time                                  `json:"computed_at"`
	Method          string                                     `json:"method"`
	SourcePipelines []string                                   `json:"source_pipelines"`
	SourceCommits   []string                                   `json:"source_commits"`
	Components      map[components.Component]ComponentBaseline `json:"components"`
}

// Compute runs the baseline algorithm over samples. samples must be
// non-empty; an empty slice returns a zero-component output and the caller
// is expected to treat that as a hard error (no baseline can be derived
// from zero pipelines).
//
// Sample order is preserved in SourcePipelines/SourceCommits.
func Compute(samples []PipelineSample, computedAt time.Time) Output {
	out := Output{
		ComputedAt: computedAt.UTC(),
		Method:     methodLabel(len(samples)),
		Components: map[components.Component]ComponentBaseline{},
	}
	for _, s := range samples {
		out.SourcePipelines = append(out.SourcePipelines, s.Pipeline)
		out.SourceCommits = append(out.SourceCommits, s.Commit)
	}
	for _, c := range components.Gated {
		var pcts []float64
		for _, s := range samples {
			if s.LCOV == nil {
				continue
			}
			exec, covered := tally(s.LCOV, c)
			if exec == 0 {
				// Pipeline emitted no executable lines for this component
				// (e.g. component code didn't exist yet, or was fully
				// excluded). Skip rather than recording 0% — a missing
				// sample should not pull the median down.
				continue
			}
			pcts = append(pcts, 100.0*float64(covered)/float64(exec))
		}
		if len(pcts) == 0 {
			continue
		}
		baseline := median(pcts)
		threshold := math.Max(MinThresholdPct, baseline-10)
		out.Components[c] = ComponentBaseline{
			BaselinePct:  round1(baseline),
			ThresholdPct: round1(threshold),
		}
	}
	return out
}

// tally returns (executable, covered) line counts for one LCOV tracefile
// projected onto component c. Mirrors the per-component bucketing the
// patch-cov library does on its diff inputs (W2b-1) — the path-routing is
// the single source of truth in components.Of.
func tally(f *lcov.File, c components.Component) (exec, covered int) {
	for _, rec := range f.Records {
		if components.Of(rec.Path) != c {
			continue
		}
		for _, hits := range rec.Lines {
			exec++
			if hits > 0 {
				covered++
			}
		}
	}
	return exec, covered
}

// median returns the median of v. Empty input → NaN. Even-length input
// returns the average of the two middle values.
func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2.0
}

// round1 rounds to 1 decimal place — matches the §8.1 example output.
func round1(x float64) float64 {
	if math.IsNaN(x) {
		return 0
	}
	return math.Round(x*10) / 10
}

func methodLabel(n int) string {
	var b strings.Builder
	b.WriteString("median over ")
	if n == 1 {
		b.WriteString("1 green release-tag or nightly ci.yml run")
	} else {
		b.WriteString(itoa(n))
		b.WriteString(" green release-tag or nightly ci.yml runs")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
