package trustmetrics

import (
	"sort"
	"time"
)

// Metrics is the computed R5 metric table for one window of trickle MRs. Each
// field maps to a numbered row of §3.4; the collector copies these into the
// Prometheus gauges. Computation is PURE (no I/O, no clock) so it is fully unit
// tested.
//
// Window semantics: rate metrics (#1, #9) are reported as per-week rates scaled
// from the actual window length so the published series is window-independent.
// Distribution metrics (#2, #4, #5, #6, #10) expose p50 and p90. Ratio/level
// metrics (#3, #7, #8 + balancing) are window aggregates.
type Metrics struct {
	WindowDays float64

	// #1 Trusted Throughput ⭐ — merged upstream MRs that passed review with
	// <=1 rework cycle, per week.
	TrustedThroughputPerWeek float64
	// #1/#4 balancing: escaped-defect rate (reverts/incidents per delivered MR).
	EscapedDefectRate float64
	// EscapedDefectCount is the raw count behind the rate (monotone-friendly view).
	EscapedDefectCount int

	// #2 Review effort per delivered MR — human review rounds to merge.
	// (Active-minutes is not derivable from the API; rounds + latency proxy it.)
	ReviewEffortP50 float64
	ReviewEffortP90 float64

	// #3 Trust index — median 1-5 reviewer survey score (responses only).
	TrustIndexMedian float64
	TrustIndexCount  int
	// #3 balancing: approval-without-comment rate (rubber-stamp signal).
	ApprovalWithoutCommentRate float64

	// #4 Rework cycles — review rounds before merge.
	ReworkCyclesP50 float64
	ReworkCyclesP90 float64

	// #5 Time-to-merge (delivery latency), seconds.
	TimeToMergeP50Seconds float64
	TimeToMergeP90Seconds float64

	// #6 Diff size — lines changed per trickle MR.
	DiffSizeP50 float64
	DiffSizeP90 float64

	// #7 Verbosity ratio — description prose / diff substance (provenance
	// excluded). Reported as the median across the window.
	VerbosityRatioP50 float64
	VerbosityRatioP90 float64

	// #8 Provenance completeness — fraction of trickle MRs with full structured
	// provenance + bead links.
	ProvenanceCompleteness float64

	// #9 Reviewer load — reviews per engineer per week, reported as the max
	// (the "most-pinned reviewer") and the mean; a flatter distribution drives
	// max toward mean. PerReviewer carries the full distribution for the gauge.
	ReviewerLoadMaxPerWeek  float64
	ReviewerLoadMeanPerWeek float64
	PerReviewerLoadPerWeek  map[string]float64

	// #10 Peel latency — peel-request bead → MR opened, seconds.
	PeelLatencyP50Seconds float64
	PeelLatencyP90Seconds float64

	// DeliveredMRCount is the number of merged trickle MRs in the window (the
	// denominator for the rate metrics; surfaced for context).
	DeliveredMRCount int
	// TotalMRCount is every trickle MR in the window (merged + open + closed).
	TotalMRCount int
}

// Compute reduces a window of MRRecords into the R5 metric table. windowDays is
// the length of the collection window in days, used to scale the per-week rate
// metrics. A windowDays <= 0 disables rate scaling (rates reported as raw
// counts over the window).
func Compute(records []MRRecord, windowDays float64) Metrics {
	m := Metrics{
		WindowDays:             windowDays,
		PerReviewerLoadPerWeek: map[string]float64{},
		TotalMRCount:           len(records),
	}

	weeks := windowDays / 7.0
	perWeek := func(count int) float64 {
		if weeks <= 0 {
			return float64(count)
		}
		return float64(count) / weeks
	}

	// Distribution accumulators.
	var (
		reviewEffort  []float64
		reworkCycles  []float64
		timeToMerge   []float64
		diffSizes     []float64
		verbosity     []float64
		peelLatencies []float64
		trustScores   []float64
	)

	var (
		merged              int
		trustedThroughput   int
		escapedDefects      int
		provenanceComplete  int
		approvedNoComment   int
		approvedTotal       int
		reviewerReviewCount = map[string]int{}
	)

	for _, r := range records {
		// Diff size & verbosity are properties of every produced MR (leading
		// indicators), independent of merge state.
		diffSizes = append(diffSizes, float64(r.LinesChanged()))
		verbosity = append(verbosity, r.VerbosityRatio())

		if r.ProvenanceComplete {
			provenanceComplete++
		}

		// Peel latency applies to any opened MR with a linked peel request.
		if pl := r.PeelLatency(); pl > 0 {
			peelLatencies = append(peelLatencies, pl.Seconds())
		}

		// Trust survey responses (any state — captured at merge time).
		if r.TrustScore > 0 {
			trustScores = append(trustScores, float64(r.TrustScore))
		}

		// Reviewer load: every distinct reviewer that touched the MR.
		for _, rev := range r.Reviewers {
			if rev != "" {
				reviewerReviewCount[rev]++
			}
		}

		if r.EscapedDefect {
			escapedDefects++
		}

		// Approval-without-comment (rubber-stamp) balancing: among approved MRs,
		// how many had zero human review notes.
		if r.Approved {
			approvedTotal++
			if r.HumanReviewNotes == 0 {
				approvedNoComment++
			}
		}

		if !r.IsMerged() {
			continue
		}
		merged++

		// Lagging metrics are defined on delivered (merged) MRs only.
		reviewEffort = append(reviewEffort, float64(r.ReviewRounds))
		reworkCycles = append(reworkCycles, float64(r.ReworkCycles()))
		if ttm := r.TimeToMerge(); ttm > 0 {
			timeToMerge = append(timeToMerge, ttm.Seconds())
		}

		// #1 Trusted Throughput: merged AND <=1 rework cycle. (The "passed human
		// review" qualifier is satisfied by reaching merged state on the dark
		// instance, which requires the trickle MR to clear review.)
		if r.ReworkCycles() <= 1 {
			trustedThroughput++
		}
	}

	m.DeliveredMRCount = merged
	m.TrustedThroughputPerWeek = perWeek(trustedThroughput)
	m.EscapedDefectCount = escapedDefects
	if merged > 0 {
		m.EscapedDefectRate = float64(escapedDefects) / float64(merged)
	}

	m.ReviewEffortP50 = percentile(reviewEffort, 50)
	m.ReviewEffortP90 = percentile(reviewEffort, 90)
	m.ReworkCyclesP50 = percentile(reworkCycles, 50)
	m.ReworkCyclesP90 = percentile(reworkCycles, 90)
	m.TimeToMergeP50Seconds = percentile(timeToMerge, 50)
	m.TimeToMergeP90Seconds = percentile(timeToMerge, 90)
	m.DiffSizeP50 = percentile(diffSizes, 50)
	m.DiffSizeP90 = percentile(diffSizes, 90)
	m.VerbosityRatioP50 = percentile(verbosity, 50)
	m.VerbosityRatioP90 = percentile(verbosity, 90)
	m.PeelLatencyP50Seconds = percentile(peelLatencies, 50)
	m.PeelLatencyP90Seconds = percentile(peelLatencies, 90)

	m.TrustIndexMedian = percentile(trustScores, 50)
	m.TrustIndexCount = len(trustScores)

	if approvedTotal > 0 {
		m.ApprovalWithoutCommentRate = float64(approvedNoComment) / float64(approvedTotal)
	}

	if len(records) > 0 {
		m.ProvenanceCompleteness = float64(provenanceComplete) / float64(len(records))
	}

	// Reviewer load distribution (#9).
	var loads []float64
	for rev, cnt := range reviewerReviewCount {
		pw := perWeek(cnt)
		m.PerReviewerLoadPerWeek[rev] = pw
		loads = append(loads, pw)
	}
	if len(loads) > 0 {
		var sum, max float64
		for _, l := range loads {
			sum += l
			if l > max {
				max = l
			}
		}
		m.ReviewerLoadMaxPerWeek = max
		m.ReviewerLoadMeanPerWeek = sum / float64(len(loads))
	}

	return m
}

// percentile returns the p-th percentile (0-100) of vs using the
// nearest-rank method on a sorted copy. Returns 0 for an empty slice. The input
// is not mutated.
func percentile(vs []float64, p float64) float64 {
	n := len(vs)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return vs[0]
	}
	sorted := make([]float64, n)
	copy(sorted, vs)
	sort.Float64s(sorted)

	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}
	// Nearest-rank: rank = ceil(p/100 * n), 1-based, clamped to [1, n].
	rank := int((p/100.0)*float64(n) + 0.999999)
	rank = max(rank, 1)
	rank = min(rank, n)
	return sorted[rank-1]
}

// WindowDaysFor converts a collection window duration to fractional days, the
// shape Compute expects.
func WindowDaysFor(d time.Duration) float64 {
	return d.Hours() / 24.0
}
