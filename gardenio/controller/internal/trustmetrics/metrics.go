package trustmetrics

import "github.com/prometheus/client_golang/prometheus"

// namespace prefixes every R5 series so the dashboard (ka-0000007085) and gate
// (ka-0000007571) can select the whole family with `gardenio_trust_*`.
const namespace = "gardenio_trust"

// Metric model. Each R5 metric (§3.4) is a recomputed window aggregate, so all
// series are GAUGES — set, not incremented — eliminating double-counting when
// the same MR appears in successive windows. Each §3.4 row is its own named
// `gardenio_trust_*` series; distribution metrics carry a {quantile="p50"|"p90"}
// label, and per-reviewer load carries a {reviewer} label. The leading-vs-lagging
// split (§3.2) is documented per field below and recorded in each metric's Help
// string, so the dashboard (ka-0000007085) selects the whole family with the
// `gardenio_trust_` name prefix.
type Collector struct {
	// trustedThroughput {} — #1 ⭐, lagging. Per-week rate.
	trustedThroughput prometheus.Gauge
	// escapedDefectRate / Count {} — #1/#4 balancing.
	escapedDefectRate  prometheus.Gauge
	escapedDefectCount prometheus.Gauge

	// reviewEffort {quantile} — #2, lagging (review rounds to merge).
	reviewEffort *prometheus.GaugeVec

	// trustIndex {} — #3, lagging (median survey 1-5).
	trustIndex      prometheus.Gauge
	trustIndexCount prometheus.Gauge
	// approvalWithoutComment {} — #3 balancing.
	approvalWithoutComment prometheus.Gauge

	// reworkCycles {quantile} — #4, lagging.
	reworkCycles *prometheus.GaugeVec

	// timeToMerge {quantile} seconds — #5, lagging.
	timeToMergeSeconds *prometheus.GaugeVec

	// diffSize {quantile} — #6, leading.
	diffSize *prometheus.GaugeVec

	// verbosityRatio {quantile} — #7, leading.
	verbosityRatio *prometheus.GaugeVec

	// provenanceCompleteness {} — #8, leading. Fraction 0-1.
	provenanceCompleteness prometheus.Gauge

	// reviewerLoad {reviewer} per-week — #9, lagging. Plus max/mean roll-ups.
	reviewerLoad     *prometheus.GaugeVec
	reviewerLoadMax  prometheus.Gauge
	reviewerLoadMean prometheus.Gauge

	// peelLatency {quantile} seconds — #10, leading.
	peelLatencySeconds *prometheus.GaugeVec

	// Context series.
	deliveredMRCount prometheus.Gauge
	totalMRCount     prometheus.Gauge
	windowDays       prometheus.Gauge
	lastCollectUnix  prometheus.Gauge

	// reviewerSet tracks reviewer labels published last cycle so stale series
	// are deleted when a reviewer drops out of the window (avoids unbounded
	// cardinality and stuck gauges).
	reviewerSet map[string]struct{}
}

// NewCollector constructs the R5 metric gauges.
func NewCollector() *Collector {
	q := []string{"quantile"}
	g := func(name, help string) prometheus.Gauge {
		return prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help})
	}
	gv := func(name, help string, labels []string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
	}

	return &Collector{
		trustedThroughput:      g("trusted_throughput_per_week", "R5 #1 north-star: merged upstream MRs passing review with <=1 rework cycle, per week."),
		escapedDefectRate:      g("escaped_defect_rate", "R5 #1/#4 balancing: reverts/incidents per delivered MR."),
		escapedDefectCount:     g("escaped_defect_count", "R5 #1/#4 balancing: count of delivered MRs later reverted / causing an incident in window."),
		reviewEffort:           gv("review_effort_rounds", "R5 #2: human review rounds to merge a trickle MR (quantile).", q),
		trustIndex:             g("trust_index_median", "R5 #3: median reviewer-reported confidence (1-5) from merge-time survey."),
		trustIndexCount:        g("trust_index_responses", "R5 #3: number of merge-time survey responses in window."),
		approvalWithoutComment: g("approval_without_comment_rate", "R5 #3 balancing: fraction of approved MRs with zero human review notes (rubber-stamp signal)."),
		reworkCycles:           gv("rework_cycles", "R5 #4: review rounds beyond the first before merge (quantile).", q),
		timeToMergeSeconds:     gv("time_to_merge_seconds", "R5 #5: open->merged delivery latency for a trickle MR (quantile).", q),
		diffSize:               gv("diff_size_lines", "R5 #6 leading: lines changed per trickle MR (quantile).", q),
		verbosityRatio:         gv("verbosity_ratio", "R5 #7 leading: description prose / diff substance, provenance excluded (quantile).", q),
		provenanceCompleteness: g("provenance_completeness", "R5 #8 leading: fraction of trickle MRs with full structured provenance + bead links."),
		reviewerLoad:           gv("reviewer_load_per_week", "R5 #9: reviews per engineer per week (per reviewer).", []string{"reviewer"}),
		reviewerLoadMax:        g("reviewer_load_max_per_week", "R5 #9: load of the most-pinned reviewer, per week (flatter is better)."),
		reviewerLoadMean:       g("reviewer_load_mean_per_week", "R5 #9: mean reviewer load per week."),
		peelLatencySeconds:     gv("peel_latency_seconds", "R5 #10 leading: peel-request bead -> trickle MR opened (quantile).", q),
		deliveredMRCount:       g("delivered_mr_count", "Number of merged trickle MRs in the window."),
		totalMRCount:           g("total_mr_count", "Total trickle MRs (merged+open+closed) in the window."),
		windowDays:             g("window_days", "Length of the metric collection window in days."),
		lastCollectUnix:        g("last_collect_timestamp_seconds", "Unix time of the last successful R5 metric collection."),
		reviewerSet:            map[string]struct{}{},
	}
}

// Register registers all R5 gauges with r.
func (c *Collector) Register(r prometheus.Registerer) error {
	for _, m := range c.collectors() {
		if err := r.Register(m); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collector) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		c.trustedThroughput, c.escapedDefectRate, c.escapedDefectCount,
		c.reviewEffort, c.trustIndex, c.trustIndexCount, c.approvalWithoutComment,
		c.reworkCycles, c.timeToMergeSeconds, c.diffSize, c.verbosityRatio,
		c.provenanceCompleteness, c.reviewerLoad, c.reviewerLoadMax, c.reviewerLoadMean,
		c.peelLatencySeconds, c.deliveredMRCount, c.totalMRCount, c.windowDays,
		c.lastCollectUnix,
	}
}

// Publish copies a computed Metrics snapshot into the gauges. collectedAtUnix is
// the unix timestamp stamped on last_collect_timestamp_seconds; pass 0 to leave
// it unchanged (lets pure tests publish without a clock).
func (c *Collector) Publish(m Metrics, collectedAtUnix int64) {
	c.trustedThroughput.Set(m.TrustedThroughputPerWeek)
	c.escapedDefectRate.Set(m.EscapedDefectRate)
	c.escapedDefectCount.Set(float64(m.EscapedDefectCount))

	c.reviewEffort.WithLabelValues("p50").Set(m.ReviewEffortP50)
	c.reviewEffort.WithLabelValues("p90").Set(m.ReviewEffortP90)

	c.trustIndex.Set(m.TrustIndexMedian)
	c.trustIndexCount.Set(float64(m.TrustIndexCount))
	c.approvalWithoutComment.Set(m.ApprovalWithoutCommentRate)

	c.reworkCycles.WithLabelValues("p50").Set(m.ReworkCyclesP50)
	c.reworkCycles.WithLabelValues("p90").Set(m.ReworkCyclesP90)

	c.timeToMergeSeconds.WithLabelValues("p50").Set(m.TimeToMergeP50Seconds)
	c.timeToMergeSeconds.WithLabelValues("p90").Set(m.TimeToMergeP90Seconds)

	c.diffSize.WithLabelValues("p50").Set(m.DiffSizeP50)
	c.diffSize.WithLabelValues("p90").Set(m.DiffSizeP90)

	c.verbosityRatio.WithLabelValues("p50").Set(m.VerbosityRatioP50)
	c.verbosityRatio.WithLabelValues("p90").Set(m.VerbosityRatioP90)

	c.provenanceCompleteness.Set(m.ProvenanceCompleteness)

	c.peelLatencySeconds.WithLabelValues("p50").Set(m.PeelLatencyP50Seconds)
	c.peelLatencySeconds.WithLabelValues("p90").Set(m.PeelLatencyP90Seconds)

	// Reviewer load: refresh per-reviewer series and prune reviewers that
	// dropped out of this window so their stale gauge does not linger.
	next := make(map[string]struct{}, len(m.PerReviewerLoadPerWeek))
	for rev, load := range m.PerReviewerLoadPerWeek {
		c.reviewerLoad.WithLabelValues(rev).Set(load)
		next[rev] = struct{}{}
	}
	for rev := range c.reviewerSet {
		if _, ok := next[rev]; !ok {
			c.reviewerLoad.DeleteLabelValues(rev)
		}
	}
	c.reviewerSet = next
	c.reviewerLoadMax.Set(m.ReviewerLoadMaxPerWeek)
	c.reviewerLoadMean.Set(m.ReviewerLoadMeanPerWeek)

	c.deliveredMRCount.Set(float64(m.DeliveredMRCount))
	c.totalMRCount.Set(float64(m.TotalMRCount))
	c.windowDays.Set(m.WindowDays)
	if collectedAtUnix > 0 {
		c.lastCollectUnix.Set(float64(collectedAtUnix))
	}
}
