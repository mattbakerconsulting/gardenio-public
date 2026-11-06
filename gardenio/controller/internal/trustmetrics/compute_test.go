package trustmetrics

import (
	"math"
	"testing"
	"time"
)

func approx(t *testing.T, got, want float64, label string) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

func TestPercentile(t *testing.T) {
	tests := []struct {
		name string
		vs   []float64
		p    float64
		want float64
	}{
		{"empty", nil, 50, 0},
		{"single", []float64{7}, 90, 7},
		{"median odd", []float64{1, 2, 3}, 50, 2},
		{"median even nearest-rank", []float64{1, 2, 3, 4}, 50, 2},
		{"p90", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 90, 9},
		{"p0 is min", []float64{5, 1, 9}, 0, 1},
		{"p100 is max", []float64{5, 1, 9}, 100, 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentile(tc.vs, tc.p); got != tc.want {
				t.Errorf("percentile(%v, %v) = %v, want %v", tc.vs, tc.p, got, tc.want)
			}
		})
	}
}

func TestPercentileDoesNotMutateInput(t *testing.T) {
	in := []float64{3, 1, 2}
	_ = percentile(in, 50)
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Errorf("percentile mutated input: %v", in)
	}
}

// baseRecord builds a merged, clean trickle MR record at a fixed clock.
func baseRecord(opened time.Time) MRRecord {
	return MRRecord{
		ProjectPath:           "dark/repo",
		IID:                   1,
		State:                 "merged",
		OpenedAt:              opened,
		MergedAt:              opened.Add(2 * time.Hour),
		LinesAdded:            10,
		LinesRemoved:          5,
		FilesChanged:          2,
		CodeDiffChars:         400,
		DescriptionChars:      100,
		Approved:              true,
		ApprovedBy:            []string{"alice"},
		ReviewRounds:          1,
		HumanReviewNotes:      1,
		Reviewers:             []string{"alice"},
		PeelRequestedAt:       opened.Add(-1 * time.Hour),
		ProvenanceComplete:    true,
		HasProvenanceBeadLink: true,
		TrustScore:            5,
	}
}

func TestComputeNorthStarAndDistributions(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// 4 merged MRs over a 14-day window: 3 clean (<=1 rework), 1 with 3 rounds
	// (2 rework cycles → NOT trusted-throughput).
	recs := []MRRecord{}
	for i := range 3 {
		r := baseRecord(now.Add(time.Duration(i) * time.Hour))
		r.IID = i + 1
		recs = append(recs, r)
	}
	heavy := baseRecord(now)
	heavy.IID = 4
	heavy.ReviewRounds = 3 // 2 rework cycles
	heavy.LinesAdded = 200
	heavy.LinesRemoved = 100
	heavy.Reviewers = []string{"bob"}
	recs = append(recs, heavy)

	m := Compute(recs, 14)

	if m.DeliveredMRCount != 4 {
		t.Errorf("DeliveredMRCount = %d, want 4", m.DeliveredMRCount)
	}
	if m.TotalMRCount != 4 {
		t.Errorf("TotalMRCount = %d, want 4", m.TotalMRCount)
	}
	// 3 trusted over 2 weeks = 1.5 / week.
	approx(t, m.TrustedThroughputPerWeek, 1.5, "TrustedThroughputPerWeek")

	// Rework cycles: [0,0,0,2] → p50 = 0, p90 = 2.
	approx(t, m.ReworkCyclesP50, 0, "ReworkCyclesP50")
	approx(t, m.ReworkCyclesP90, 2, "ReworkCyclesP90")

	// Diff sizes: [15,15,15,300] → p50=15, p90=300.
	approx(t, m.DiffSizeP50, 15, "DiffSizeP50")
	approx(t, m.DiffSizeP90, 300, "DiffSizeP90")

	// Time-to-merge: all 2h = 7200s.
	approx(t, m.TimeToMergeP50Seconds, 7200, "TimeToMergeP50Seconds")

	// Trust index median: all 5.
	approx(t, m.TrustIndexMedian, 5, "TrustIndexMedian")
	if m.TrustIndexCount != 4 {
		t.Errorf("TrustIndexCount = %d, want 4", m.TrustIndexCount)
	}

	// Provenance completeness: 4/4 = 1.0.
	approx(t, m.ProvenanceCompleteness, 1.0, "ProvenanceCompleteness")
}

func TestComputeEscapedDefectRate(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	a := baseRecord(now)
	b := baseRecord(now)
	b.EscapedDefect = true
	m := Compute([]MRRecord{a, b}, 7)
	if m.EscapedDefectCount != 1 {
		t.Errorf("EscapedDefectCount = %d, want 1", m.EscapedDefectCount)
	}
	approx(t, m.EscapedDefectRate, 0.5, "EscapedDefectRate")
}

func TestComputeApprovalWithoutComment(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// One approved+commented, one approved-without-comment (rubber-stamp).
	a := baseRecord(now)
	a.HumanReviewNotes = 2
	b := baseRecord(now)
	b.HumanReviewNotes = 0
	m := Compute([]MRRecord{a, b}, 7)
	approx(t, m.ApprovalWithoutCommentRate, 0.5, "ApprovalWithoutCommentRate")
}

func TestComputeReviewerLoadDistribution(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// alice reviews 3 MRs, bob 1, over 1 week.
	recs := []MRRecord{}
	for range 3 {
		r := baseRecord(now)
		r.Reviewers = []string{"alice"}
		recs = append(recs, r)
	}
	rb := baseRecord(now)
	rb.Reviewers = []string{"bob"}
	recs = append(recs, rb)

	m := Compute(recs, 7)
	approx(t, m.PerReviewerLoadPerWeek["alice"], 3, "alice load")
	approx(t, m.PerReviewerLoadPerWeek["bob"], 1, "bob load")
	approx(t, m.ReviewerLoadMaxPerWeek, 3, "ReviewerLoadMaxPerWeek")
	approx(t, m.ReviewerLoadMeanPerWeek, 2, "ReviewerLoadMeanPerWeek")
}

func TestComputeVerbosityAndPeelLatency(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	r := baseRecord(now)
	r.DescriptionChars = 200
	r.CodeDiffChars = 99 // ratio = 200/100 = 2.0
	r.PeelRequestedAt = now.Add(-30 * time.Minute)
	r.OpenedAt = now
	r.MergedAt = now.Add(time.Hour)

	m := Compute([]MRRecord{r}, 7)
	approx(t, m.VerbosityRatioP50, 2.0, "VerbosityRatioP50")
	approx(t, m.PeelLatencyP50Seconds, 1800, "PeelLatencyP50Seconds")
}

func TestComputeOpenMRContributesLeadingOnly(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	open := baseRecord(now)
	open.State = "opened"
	open.MergedAt = time.Time{}
	open.LinesAdded = 40
	open.LinesRemoved = 0

	m := Compute([]MRRecord{open}, 7)
	if m.DeliveredMRCount != 0 {
		t.Errorf("DeliveredMRCount = %d, want 0 (MR still open)", m.DeliveredMRCount)
	}
	if m.TotalMRCount != 1 {
		t.Errorf("TotalMRCount = %d, want 1", m.TotalMRCount)
	}
	// Diff size (leading) still computed from the open MR.
	approx(t, m.DiffSizeP50, 40, "DiffSizeP50")
	// No delivered MR → throughput zero.
	approx(t, m.TrustedThroughputPerWeek, 0, "TrustedThroughputPerWeek")
}

func TestComputeEmptyWindow(t *testing.T) {
	m := Compute(nil, 7)
	if m.TotalMRCount != 0 || m.DeliveredMRCount != 0 {
		t.Errorf("empty window should have zero counts, got %+v", m)
	}
	approx(t, m.ProvenanceCompleteness, 0, "ProvenanceCompleteness")
	approx(t, m.TrustedThroughputPerWeek, 0, "TrustedThroughputPerWeek")
}

func TestComputeZeroWindowDisablesRateScaling(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	r := baseRecord(now)
	m := Compute([]MRRecord{r}, 0)
	// windowDays<=0 → rate is raw count (1 trusted MR).
	approx(t, m.TrustedThroughputPerWeek, 1, "TrustedThroughputPerWeek raw")
}

func TestRecordDerivedFields(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	r := MRRecord{
		State:           "merged",
		OpenedAt:        now,
		MergedAt:        now.Add(3 * time.Hour),
		PeelRequestedAt: now.Add(-90 * time.Minute),
		ReviewRounds:    3,
		LinesAdded:      7,
		LinesRemoved:    3,
	}
	if !r.IsMerged() {
		t.Error("IsMerged should be true")
	}
	if r.ReworkCycles() != 2 {
		t.Errorf("ReworkCycles = %d, want 2", r.ReworkCycles())
	}
	if r.LinesChanged() != 10 {
		t.Errorf("LinesChanged = %d, want 10", r.LinesChanged())
	}
	if r.TimeToMerge() != 3*time.Hour {
		t.Errorf("TimeToMerge = %v, want 3h", r.TimeToMerge())
	}
	if r.PeelLatency() != 90*time.Minute {
		t.Errorf("PeelLatency = %v, want 90m", r.PeelLatency())
	}
}

// TestRecordNegativeDurationsGuarded pins that TimeToMerge and PeelLatency
// clamp out-of-order timestamps to zero, never negative (ka-0000000493).
func TestRecordNegativeDurationsGuarded(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// merged_at before opened_at, peel after open — both should clamp to 0.
	r := MRRecord{
		State:           "merged",
		OpenedAt:        now,
		MergedAt:        now.Add(-time.Hour),
		PeelRequestedAt: now.Add(time.Hour),
	}
	if r.TimeToMerge() != 0 {
		t.Errorf("TimeToMerge negative not clamped: %v", r.TimeToMerge())
	}
	if r.PeelLatency() != 0 {
		t.Errorf("PeelLatency negative not clamped: %v", r.PeelLatency())
	}
}

func TestWindowDaysFor(t *testing.T) {
	approx(t, WindowDaysFor(48*time.Hour), 2, "WindowDaysFor 48h")
	approx(t, WindowDaysFor(7*24*time.Hour), 7, "WindowDaysFor 7d")
}
