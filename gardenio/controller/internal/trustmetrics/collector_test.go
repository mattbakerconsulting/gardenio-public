package trustmetrics

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// fakeGitLab implements GitLabSource for collector tests.
type fakeGitLab struct {
	mrs       []MRMeta
	diff      map[int]DiffStats
	approvals map[int]Approvals
	notes     map[int][]Note
}

func (f *fakeGitLab) ListTrickleMRs(_ context.Context, _ time.Time) ([]MRMeta, error) {
	return f.mrs, nil
}
func (f *fakeGitLab) DiffStats(_ context.Context, _ string, iid int) (DiffStats, error) {
	return f.diff[iid], nil
}
func (f *fakeGitLab) Approvals(_ context.Context, _ string, iid int) (Approvals, error) {
	return f.approvals[iid], nil
}
func (f *fakeGitLab) Notes(_ context.Context, _ string, iid int) ([]Note, error) {
	return f.notes[iid], nil
}

// fakeBeads implements BeadSource.
type fakeBeads struct {
	byURL map[string]PeelInfo
}

func (f *fakeBeads) PeelInfoForMR(_ context.Context, url string) (PeelInfo, bool, error) {
	info, ok := f.byURL[url]
	return info, ok, nil
}

func gaugeValue(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 1)
	c.Collect(ch)
	close(ch)
	m := <-ch
	var dtoM dto.Metric
	if err := m.Write(&dtoM); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return dtoM.GetGauge().GetValue()
}

func TestCollectorJoinsGitLabAndBeads(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-3 * time.Hour)
	merged := now.Add(-1 * time.Hour)
	url := "https://dark.example/grp/repo/-/merge_requests/7"

	gl := &fakeGitLab{
		mrs: []MRMeta{{
			ProjectPath: "grp/repo",
			IID:         7,
			State:       "merged",
			WebURL:      url,
			OpenedAt:    opened,
			MergedAt:    merged,
			Description: "Terse fix.\n\nDark-Bead: gd-ABC123\nProvenance-Hash: deadbeef",
		}},
		diff:      map[int]DiffStats{7: {Additions: 12, Deletions: 3, FilesChanged: 1, CodeDiffChars: 200}},
		approvals: map[int]Approvals{7: {Approved: true, ApprovedBy: []string{"alice"}}},
		notes: map[int][]Note{7: {
			{Author: "alice", Body: "looks good"},
			{Author: "gardenio-bot", Body: "auto", System: false}, // bot — excluded
			{Author: "system", Body: "merged", System: true},      // system — excluded
		}},
	}
	beads := &fakeBeads{byURL: map[string]PeelInfo{
		url: {
			PeelRequestedAt:       opened.Add(-2 * time.Hour),
			HasProvenanceBeadLink: true,
			TrustScore:            4,
		},
	}}

	mc := NewCollector()
	pc := NewPeriodicCollector(CollectorConfig{
		GitLab:  gl,
		Beads:   beads,
		Metrics: mc,
		BotUser: "gardenio-bot",
		Window:  28 * 24 * time.Hour,
		Clock:   func() time.Time { return now },
	})

	if err := pc.collectOnce(context.Background()); err != nil {
		t.Fatalf("collectOnce: %v", err)
	}

	// One delivered MR.
	if got := gaugeValue(t, mc.deliveredMRCount); got != 1 {
		t.Errorf("deliveredMRCount = %v, want 1", got)
	}
	// Provenance complete: has block (Dark-Bead trailer) AND bead link.
	if got := gaugeValue(t, mc.provenanceCompleteness); got != 1 {
		t.Errorf("provenanceCompleteness = %v, want 1", got)
	}
	// Trust index median = 4 (the single survey response).
	if got := gaugeValue(t, mc.trustIndex); got != 4 {
		t.Errorf("trustIndex = %v, want 4", got)
	}
	// Diff size p50 = 15 (12+3).
	if got := gaugeValue(t, mc.diffSize.WithLabelValues("p50")); got != 15 {
		t.Errorf("diffSize p50 = %v, want 15", got)
	}
	// 1 human review note (bot + system excluded) → 1 review round, 0 rework.
	if got := gaugeValue(t, mc.reworkCycles.WithLabelValues("p50")); got != 0 {
		t.Errorf("reworkCycles p50 = %v, want 0", got)
	}
	// last collect timestamp stamped.
	if got := gaugeValue(t, mc.lastCollectUnix); got != float64(now.Unix()) {
		t.Errorf("lastCollectUnix = %v, want %v", got, now.Unix())
	}
}

func TestCollectorToleratesMissingBeadJoin(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	url := "https://dark.example/grp/repo/-/merge_requests/8"
	gl := &fakeGitLab{
		mrs: []MRMeta{{
			ProjectPath: "grp/repo", IID: 8, State: "merged", WebURL: url,
			OpenedAt: now.Add(-2 * time.Hour), MergedAt: now.Add(-time.Hour),
			Description: "No provenance here.",
		}},
		diff:      map[int]DiffStats{8: {Additions: 5, Deletions: 0}},
		approvals: map[int]Approvals{},
		notes:     map[int][]Note{},
	}
	// No bead entry for this URL → join miss.
	beads := &fakeBeads{byURL: map[string]PeelInfo{}}

	mc := NewCollector()
	pc := NewPeriodicCollector(CollectorConfig{
		GitLab: gl, Beads: beads, Metrics: mc,
		Window: 28 * 24 * time.Hour, Clock: func() time.Time { return now },
	})
	if err := pc.collectOnce(context.Background()); err != nil {
		t.Fatalf("collectOnce: %v", err)
	}
	// MR still counted; provenance incomplete (no block, no link).
	if got := gaugeValue(t, mc.deliveredMRCount); got != 1 {
		t.Errorf("deliveredMRCount = %v, want 1", got)
	}
	if got := gaugeValue(t, mc.provenanceCompleteness); got != 0 {
		t.Errorf("provenanceCompleteness = %v, want 0", got)
	}
}

func TestCollectorRegisterSucceeds(t *testing.T) {
	mc := NewCollector()
	reg := prometheus.NewRegistry()
	if err := mc.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Gather should not error and should include our namespace.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "gardenio_trust_trusted_throughput_per_week" {
			found = true
		}
	}
	if !found {
		t.Error("expected gardenio_trust_trusted_throughput_per_week to be registered")
	}
}

func TestPublishPrunesStaleReviewers(t *testing.T) {
	mc := NewCollector()
	// First window: alice + bob.
	mc.Publish(Metrics{PerReviewerLoadPerWeek: map[string]float64{"alice": 3, "bob": 1}}, 0)
	// Second window: only alice (bob dropped out).
	mc.Publish(Metrics{PerReviewerLoadPerWeek: map[string]float64{"alice": 2}}, 0)

	reg := prometheus.NewRegistry()
	if err := reg.Register(mc.reviewerLoad); err != nil {
		t.Fatalf("register reviewerLoad: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	reviewers := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reviewer" {
					reviewers[l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}
	if _, ok := reviewers["bob"]; ok {
		t.Errorf("stale reviewer bob not pruned: %v", reviewers)
	}
	if reviewers["alice"] != 2 {
		t.Errorf("alice load = %v, want 2", reviewers["alice"])
	}
}
