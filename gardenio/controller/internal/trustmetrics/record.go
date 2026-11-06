// Package trustmetrics lands the R5 "Trusted Throughput" success-metrics
// pipeline (bead ka-0000000493, source bundle ka-0000008221 §3.4) as queryable
// Prometheus time series.
//
// It collects, for the trickle MRs that deliver dark work upstream:
//   - from the dark GitLab MR API: diff stats, open/merge timestamps,
//     approvals, and review notes;
//   - from beads: the peel-request timestamp and provenance bead links.
//
// Those facts are joined into one MRRecord per trickle MR (record.go), reduced
// by PURE functions into the R5 metric table 1-10 plus their anti-gaming
// balancing metrics (compute.go), and exported as gauges (metrics.go) on a
// periodic collection loop (collector.go).
//
// Faithfulness to §3.4: every metric here maps 1:1 to a numbered row of the
// metric table, including the leading/lagging split and the paired balancing
// metric. The package computes the metric VALUES; the dashboard (ka-0000007085)
// and terseness gate (ka-0000007571) are downstream consumers of the series it
// publishes — this is the blocking infra for both.
package trustmetrics

import "time"

// MRRecord is the joined per-MR fact row the R5 metrics are computed from. One
// record per delivered (trickle) MR. Fields left zero/empty when the source did
// not provide them; the compute functions are written to tolerate partial rows
// (e.g. an MR still open has no MergedAt, so it contributes to open-state
// metrics only).
type MRRecord struct {
	// ProjectPath / IID identify the MR on the dark GitLab instance.
	ProjectPath string
	IID         int

	// --- GitLab MR facts ---

	// State is the GitLab MR state: opened, closed, merged, locked.
	State string
	// OpenedAt / MergedAt are the MR lifecycle timestamps. MergedAt is the zero
	// time for an MR that has not merged.
	OpenedAt time.Time
	MergedAt time.Time

	// LinesAdded / LinesRemoved / FilesChanged are the diff stats (#6).
	LinesAdded   int
	LinesRemoved int
	FilesChanged int
	// CodeDiffChars is the byte length of the full diff, the verbosity-ratio
	// (#7) denominator.
	CodeDiffChars int

	// DescriptionChars is the prose length of the MR description with the
	// structured provenance block REMOVED (§3.4: "provenance excluded"). It is
	// the verbosity-ratio (#7) numerator.
	DescriptionChars int

	// Approved / ApprovedBy capture the approval state (#3 balancing, #4).
	Approved   bool
	ApprovedBy []string

	// ReviewRounds is the number of human review rounds before merge (#4): the
	// count of distinct human (non-bot, non-system) review notes that requested
	// changes / opened a discussion. ReworkCycles = max(0, ReviewRounds-1) — the
	// first round is the baseline review, not "rework".
	ReviewRounds int
	// HumanReviewNotes is the total count of human review notes (engaged review
	// signal for the approval-without-comment balancing metric, #3).
	HumanReviewNotes int
	// Reviewers is the set of distinct human reviewers who left notes/approved
	// (reviewer load distribution, #9).
	Reviewers []string

	// --- beads facts ---

	// PeelRequestedAt is the timestamp the peel-request bead was created (the
	// "request (bead)" half of peel latency #10). Zero when the MR has no linked
	// peel-request bead.
	PeelRequestedAt time.Time

	// ProvenanceComplete reports whether the MR carries a complete structured
	// provenance block AND is linked to its producing bead(s) (#8). The collector
	// sets this from the MR-template lint + bead-link check.
	ProvenanceComplete bool

	// HasProvenanceBeadLink is true when at least one producing/peel-request bead
	// is linked to this MR (the "bead links" half of #8).
	HasProvenanceBeadLink bool

	// EscapedDefect marks an MR whose delivered change was later reverted or
	// caused a post-merge incident (#1 / #4 balancing: escaped-defect rate). Set
	// by the collector from a revert/incident label or linked revert MR.
	EscapedDefect bool

	// TrustScore is the 1-5 reviewer-reported confidence captured by the
	// merge-time micro-survey (#3). Zero means "no survey response yet" and is
	// excluded from the trust-index median.
	TrustScore int
}

// IsMerged reports whether the MR reached the merged terminal state.
func (r MRRecord) IsMerged() bool { return r.State == "merged" && !r.MergedAt.IsZero() }

// ReworkCycles is the number of rework rounds: review rounds beyond the first.
// p50=1 / p90<=2 in §3.4 refers to ReviewRounds; rework is ReviewRounds-1.
func (r MRRecord) ReworkCycles() int {
	if r.ReviewRounds <= 1 {
		return 0
	}
	return r.ReviewRounds - 1
}

// LinesChanged is added+removed lines — the diff-size (#6) quantity.
func (r MRRecord) LinesChanged() int { return r.LinesAdded + r.LinesRemoved }

// TimeToMerge is merged_at - opened_at (#5). Zero when not merged or timestamps
// missing.
func (r MRRecord) TimeToMerge() time.Duration {
	if !r.IsMerged() || r.OpenedAt.IsZero() {
		return 0
	}
	d := r.MergedAt.Sub(r.OpenedAt)
	if d < 0 {
		return 0
	}
	return d
}

// PeelLatency is mr_opened_at - peel_request_at (#10). Zero when either
// timestamp is missing.
func (r MRRecord) PeelLatency() time.Duration {
	if r.OpenedAt.IsZero() || r.PeelRequestedAt.IsZero() {
		return 0
	}
	d := r.OpenedAt.Sub(r.PeelRequestedAt)
	if d < 0 {
		return 0
	}
	return d
}

// VerbosityRatio is description_chars / (1 + code_diff_chars) (#7). The +1 keeps
// it defined for a zero-diff MR.
func (r MRRecord) VerbosityRatio() float64 {
	return float64(r.DescriptionChars) / float64(1+r.CodeDiffChars)
}
