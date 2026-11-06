package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeCICDSource is an injectable GitLab surface for the collector tests. It
// serves canned pipelines/jobs per project and canned usage/quota, and records
// how many times each pipeline's jobs were fetched (to prove dedup).
type fakeCICDSource struct {
	mu sync.Mutex

	pipelines map[string][]GitLabPipeline // project -> pipelines
	jobs      map[int][]GitLabJob         // pipeline id -> jobs
	jobCalls  map[int]int                 // pipeline id -> ListPipelineJobsByPath calls

	listErr error

	months   []GitLabCIMinutesMonth
	quota    GitLabComputeQuota
	usageErr error
	quotaErr error
}

func newFakeCICDSource() *fakeCICDSource {
	return &fakeCICDSource{
		pipelines: map[string][]GitLabPipeline{},
		jobs:      map[int][]GitLabJob{},
		jobCalls:  map[int]int{},
	}
}

func (f *fakeCICDSource) ListPipelines(_ context.Context, projectPath, _, _ string, _ time.Time) ([]GitLabPipeline, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.pipelines[projectPath], nil
}

func (f *fakeCICDSource) ListPipelineJobsByPath(_ context.Context, _ string, pipelineID int) ([]GitLabJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobCalls[pipelineID]++
	return f.jobs[pipelineID], nil
}

func (f *fakeCICDSource) GetCIMinutesUsage(_ context.Context, _ string, _ int) ([]GitLabCIMinutesMonth, error) {
	if f.usageErr != nil {
		return nil, f.usageErr
	}
	return f.months, nil
}

func (f *fakeCICDSource) GetGroupComputeQuota(_ context.Context, _ string) (GitLabComputeQuota, error) {
	if f.quotaErr != nil {
		return GitLabComputeQuota{}, f.quotaErr
	}
	return f.quota, nil
}

func (f *fakeCICDSource) callsFor(pipelineID int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobCalls[pipelineID]
}

func (f *fakeCICDSource) setPipelines(project string, ps []GitLabPipeline) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pipelines[project] = ps
}

func fptr(v float64) *float64 { return &v }

func resetCICDMetrics() {
	cicdMinutesTotal.Reset()
	cicdRunsTotal.Reset()
	cicdQueuedSecondsTotal.Reset()
	cicdJobDurationSeconds.Reset()
	cicdGitLabMinutesUsed.Reset()
	cicdGitLabMinutesUsedCurrent.Reset()
	cicdGitLabQuotaTotalCurrent.Reset()
	cicdGitLabRemainingCurrent.Reset()
	cicdGitLabExceededCurrent.Reset()
	cicdGitLabProjectMinutesCurrent.Reset()
	cicdGitLabSubgroupMinutesCurrent.Reset()
	cicdScrapeDurationSeconds.Reset()
	cicdScrapeErrorsTotal.Reset()
	cicdLastSuccessTimestamp.Reset()
}

// TestCICD_JobDuration_CountsMinutes runs one job-duration cycle and asserts the
// compute-minutes counter, runs counter, queued counter, and histogram all
// carry the expected values from summed job durations.
func TestCICD_JobDuration_CountsMinutes(t *testing.T) {
	resetCICDMetrics()

	fake := newFakeCICDSource()
	fake.pipelines["grp/proj"] = []GitLabPipeline{
		{ID: 100, Status: "success", Ref: "main"},
	}
	fake.jobs[100] = []GitLabJob{
		{Name: "build", Status: "success", Duration: fptr(90), QueuedDuration: fptr(3), Runner: &GitLabRunner{RunnerType: "instance_type"}},
		{Name: "test", Status: "success", Duration: fptr(30), QueuedDuration: fptr(1.5), Runner: &GitLabRunner{RunnerType: "instance_type"}},
		{Name: "pending-job", Status: "created", Duration: nil}, // null duration skipped
	}

	c := NewCICDMetricsCollector(CICDMetricsConfig{
		Pipelines:                fake,
		Projects:                 []string{"grp/proj"},
		GitLabJobDurationEnabled: true,
	})
	c.collectJobDuration(context.Background())

	// 120 seconds total / 60 = 2.0 minutes.
	if got := testutil.ToFloat64(cicdMinutesTotal.WithLabelValues("gitlab", "grp/proj", "main", "success")); got != 2.0 {
		t.Errorf("minutes_total = %v, want 2.0", got)
	}
	if got := testutil.ToFloat64(cicdRunsTotal.WithLabelValues("gitlab", "grp/proj", "main", "success")); got != 1 {
		t.Errorf("runs_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(cicdQueuedSecondsTotal.WithLabelValues("gitlab", "grp/proj")); got != 4.5 {
		t.Errorf("queued_seconds_total = %v, want 4.5", got)
	}
	// Histogram observed two jobs with a duration.
	if n := testutil.CollectAndCount(cicdJobDurationSeconds); n != 2 {
		t.Errorf("job_duration series count = %d, want 2", n)
	}
}

// TestCICD_JobDuration_DedupAcrossPolls proves a pipeline is counted exactly
// once even when it reappears in a later poll window.
func TestCICD_JobDuration_DedupAcrossPolls(t *testing.T) {
	resetCICDMetrics()

	fake := newFakeCICDSource()
	fake.pipelines["grp/proj"] = []GitLabPipeline{{ID: 200, Status: "failed", Ref: "feature/x"}}
	fake.jobs[200] = []GitLabJob{{Name: "build", Duration: fptr(60), Runner: &GitLabRunner{RunnerType: "instance_type"}}}

	c := NewCICDMetricsCollector(CICDMetricsConfig{
		Pipelines:                fake,
		Projects:                 []string{"grp/proj"},
		GitLabJobDurationEnabled: true,
	})

	c.collectJobDuration(context.Background())
	c.collectJobDuration(context.Background()) // second poll, same pipeline

	if got := testutil.ToFloat64(cicdMinutesTotal.WithLabelValues("gitlab", "grp/proj", "branch", "failed")); got != 1.0 {
		t.Errorf("minutes_total after 2 polls = %v, want 1.0 (counted once)", got)
	}
	if calls := fake.callsFor(200); calls != 1 {
		t.Errorf("jobs fetched %d times, want 1 (deduped)", calls)
	}
}

// TestCICD_JobDuration_SeenPipelinesPruned proves the dedup map does not grow
// without bound: an entry not observed within the retention grace (2×window) is
// pruned, so a pipeline that has aged out of the poll window no longer occupies
// a permanent slot. Without pruning, seenPipelines would accumulate one entry
// per terminal pipeline ever seen — an unbounded heap leak in the long-lived
// gitlab-bridge.
func TestCICD_JobDuration_SeenPipelinesPruned(t *testing.T) {
	resetCICDMetrics()

	base := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	now := base
	fake := newFakeCICDSource()
	fake.jobs[400] = []GitLabJob{{Name: "build", Duration: fptr(60), Runner: &GitLabRunner{RunnerType: "instance_type"}}}
	fake.jobs[401] = []GitLabJob{{Name: "build", Duration: fptr(60), Runner: &GitLabRunner{RunnerType: "instance_type"}}}

	c := NewCICDMetricsCollector(CICDMetricsConfig{
		Pipelines:                fake,
		Projects:                 []string{"grp/proj"},
		GitLabJobDurationEnabled: true,
		Window:                   60 * time.Minute,
		Clock:                    func() time.Time { return now },
	})

	// Poll 1: pipeline 400 is terminal and gets counted + a dedup entry.
	fake.setPipelines("grp/proj", []GitLabPipeline{{ID: 400, Status: "success", Ref: "main"}})
	c.collectJobDuration(context.Background())
	if _, ok := c.seenPipelines["grp/proj#400"]; !ok {
		t.Fatalf("pipeline 400 should be in the dedup map after poll 1")
	}
	if got := len(c.seenPipelines); got != 1 {
		t.Fatalf("dedup map size after poll 1 = %d, want 1", got)
	}

	// Advance the clock past the retention grace (2×window = 120m) and poll a
	// different, newer pipeline. Pipeline 400 has aged out of the window and is
	// no longer listed, so its dedup entry must be pruned — the map stays
	// bounded at the newly-observed pipeline rather than growing to 2.
	now = base.Add(3 * time.Hour)
	fake.setPipelines("grp/proj", []GitLabPipeline{{ID: 401, Status: "success", Ref: "main"}})
	c.collectJobDuration(context.Background())

	if _, ok := c.seenPipelines["grp/proj#400"]; ok {
		t.Errorf("pipeline 400 dedup entry should have been pruned after aging out of the window")
	}
	if _, ok := c.seenPipelines["grp/proj#401"]; !ok {
		t.Errorf("pipeline 401 should be in the dedup map after poll 2")
	}
	if got := len(c.seenPipelines); got != 1 {
		t.Errorf("dedup map size after prune = %d, want 1 (bounded, not accumulating)", got)
	}
}

// TestCICD_JobDuration_DedupSurvivesWithinWindow confirms a pipeline that is
// still being re-listed within the window keeps its dedup entry (its
// observation time is refreshed each poll), so pruning never re-counts an
// actively-visible pipeline.
func TestCICD_JobDuration_DedupSurvivesWithinWindow(t *testing.T) {
	resetCICDMetrics()

	base := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	now := base
	fake := newFakeCICDSource()
	fake.pipelines["grp/proj"] = []GitLabPipeline{{ID: 500, Status: "success", Ref: "main"}}
	fake.jobs[500] = []GitLabJob{{Name: "build", Duration: fptr(60), Runner: &GitLabRunner{RunnerType: "instance_type"}}}

	c := NewCICDMetricsCollector(CICDMetricsConfig{
		Pipelines:                fake,
		Projects:                 []string{"grp/proj"},
		GitLabJobDurationEnabled: true,
		Window:                   60 * time.Minute,
		Clock:                    func() time.Time { return now },
	})

	c.collectJobDuration(context.Background())
	// Advance well past the retention grace but keep pipeline 500 listed each
	// poll (as a still-updating pipeline within the window would be). The
	// refreshed observation time must keep it deduped — no re-count.
	for i := 1; i <= 4; i++ {
		now = base.Add(time.Duration(i) * 90 * time.Minute)
		c.collectJobDuration(context.Background())
	}

	if calls := fake.callsFor(500); calls != 1 {
		t.Errorf("pipeline 500 jobs fetched %d times, want 1 (deduped across refreshed polls)", calls)
	}
	if got := testutil.ToFloat64(cicdMinutesTotal.WithLabelValues("gitlab", "grp/proj", "main", "success")); got != 1.0 {
		t.Errorf("minutes_total = %v, want 1.0 (counted once despite prune passes)", got)
	}
}

// TestCICD_JobDuration_SkipsRunningPipeline confirms a non-terminal pipeline is
// not counted (its durations are partial).
func TestCICD_JobDuration_SkipsRunningPipeline(t *testing.T) {
	resetCICDMetrics()

	fake := newFakeCICDSource()
	fake.pipelines["grp/proj"] = []GitLabPipeline{{ID: 300, Status: "running", Ref: "main"}}
	fake.jobs[300] = []GitLabJob{{Name: "build", Duration: fptr(45)}}

	c := NewCICDMetricsCollector(CICDMetricsConfig{
		Pipelines:                fake,
		Projects:                 []string{"grp/proj"},
		GitLabJobDurationEnabled: true,
	})
	c.collectJobDuration(context.Background())

	if n := testutil.CollectAndCount(cicdMinutesTotal); n != 0 {
		t.Errorf("minutes_total should have no series for a running pipeline, got %d", n)
	}
	if calls := fake.callsFor(300); calls != 0 {
		t.Errorf("jobs fetched for a running pipeline (%d), want 0", calls)
	}
}

// TestCICD_JobDuration_ListErrorIncrementsScrapeError confirms a ListPipelines
// failure bumps the gitlab scrape-error counter and does not stamp last-success.
func TestCICD_JobDuration_ListErrorIncrementsScrapeError(t *testing.T) {
	resetCICDMetrics()

	fake := newFakeCICDSource()
	fake.listErr = errors.New("boom")

	c := NewCICDMetricsCollector(CICDMetricsConfig{
		Pipelines:                fake,
		Projects:                 []string{"grp/proj"},
		GitLabJobDurationEnabled: true,
	})
	c.collectJobDuration(context.Background())

	if got := testutil.ToFloat64(cicdScrapeErrorsTotal.WithLabelValues("gitlab")); got != 1 {
		t.Errorf("scrape_errors{gitlab} = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(cicdLastSuccessTimestamp); n != 0 {
		t.Errorf("last_success should not be stamped after an error, got %d series", n)
	}
}

// TestCICD_FirstParty_PublishesUsageAndQuota runs one first-party cycle with a
// fixed clock and asserts the used/quota/remaining/exceeded + per-project +
// per-subgroup gauges.
func TestCICD_FirstParty_PublishesUsageAndQuota(t *testing.T) {
	resetCICDMetrics()

	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	fake := newFakeCICDSource()
	fake.months = []GitLabCIMinutesMonth{
		{
			Month: "July 2026", MonthISO8601: "2026-07-01", Minutes: 400,
			Projects: []GitLabCIMinutesProject{
				{Name: "gardenio", FullPath: "Acme/Platform/gardenio", Minutes: 250},
				{Name: "helm", FullPath: "Acme/Platform/acme-helm-chart", Minutes: 150},
			},
		},
		{Month: "June 2026", MonthISO8601: "2026-06-01", Minutes: 300},
	}
	fake.quota = GitLabComputeQuota{Base: fptr(1000), Additional: fptr(500)}

	c := NewCICDMetricsCollector(CICDMetricsConfig{
		Usage:                   fake,
		GitLabFirstPartyEnabled: true,
		FirstPartyNamespaceID:   "gid://gitlab/Group/1",
		FirstPartyGroupPath:     "Acme",
		Clock:                   func() time.Time { return now },
	})
	c.collectFirstParty(context.Background())

	ns := "Acme"
	if got := testutil.ToFloat64(cicdGitLabMinutesUsedCurrent.WithLabelValues(ns)); got != 400 {
		t.Errorf("used_current = %v, want 400", got)
	}
	if got := testutil.ToFloat64(cicdGitLabQuotaTotalCurrent.WithLabelValues(ns)); got != 1500 {
		t.Errorf("quota_total_current = %v, want 1500", got)
	}
	if got := testutil.ToFloat64(cicdGitLabRemainingCurrent.WithLabelValues(ns)); got != 1100 {
		t.Errorf("remaining_current = %v, want 1100", got)
	}
	if got := testutil.ToFloat64(cicdGitLabExceededCurrent.WithLabelValues(ns)); got != 0 {
		t.Errorf("exceeded_current = %v, want 0", got)
	}
	// Per-project current-month usage.
	if got := testutil.ToFloat64(cicdGitLabProjectMinutesCurrent.WithLabelValues(ns, "Acme/Platform/gardenio")); got != 250 {
		t.Errorf("project gardenio used = %v, want 250", got)
	}
	// Subgroup rollup: Acme/Platform gets both projects' minutes.
	if got := testutil.ToFloat64(cicdGitLabSubgroupMinutesCurrent.WithLabelValues(ns, "Acme/Platform")); got != 400 {
		t.Errorf("subgroup Platform used = %v, want 400", got)
	}
	// Historical month series still published.
	if got := testutil.ToFloat64(cicdGitLabMinutesUsed.WithLabelValues(ns, "2026-06-01")); got != 300 {
		t.Errorf("historical June used = %v, want 300", got)
	}
}

// TestCICD_FirstParty_ExceededWhenOverQuota confirms the exceeded flag flips to
// 1 and remaining floors at 0 when usage passes the total quota.
func TestCICD_FirstParty_ExceededWhenOverQuota(t *testing.T) {
	resetCICDMetrics()

	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	fake := newFakeCICDSource()
	fake.months = []GitLabCIMinutesMonth{{MonthISO8601: "2026-07-01", Minutes: 1600}}
	fake.quota = GitLabComputeQuota{Base: fptr(1000), Additional: fptr(500)}

	c := NewCICDMetricsCollector(CICDMetricsConfig{
		Usage:                   fake,
		GitLabFirstPartyEnabled: true,
		FirstPartyGroupPath:     "Acme",
		Clock:                   func() time.Time { return now },
	})
	c.collectFirstParty(context.Background())

	ns := "Acme"
	if got := testutil.ToFloat64(cicdGitLabExceededCurrent.WithLabelValues(ns)); got != 1 {
		t.Errorf("exceeded_current = %v, want 1", got)
	}
	if got := testutil.ToFloat64(cicdGitLabRemainingCurrent.WithLabelValues(ns)); got != 0 {
		t.Errorf("remaining_current = %v, want 0 (floored)", got)
	}
}

// TestCICD_FirstParty_ErrorIncrementsScrapeError confirms a usage-fetch failure
// (the expected state until the group-owner token lands) increments the
// first-party scrape-error counter and does not stamp last-success.
func TestCICD_FirstParty_ErrorIncrementsScrapeError(t *testing.T) {
	resetCICDMetrics()

	fake := newFakeCICDSource()
	fake.usageErr = errors.New("403 forbidden — needs group-owner token")

	c := NewCICDMetricsCollector(CICDMetricsConfig{
		Usage:                   fake,
		GitLabFirstPartyEnabled: true,
		FirstPartyGroupPath:     "Acme",
	})
	c.collectFirstParty(context.Background())

	if got := testutil.ToFloat64(cicdScrapeErrorsTotal.WithLabelValues("gitlab-firstparty")); got != 1 {
		t.Errorf("scrape_errors{gitlab-firstparty} = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(cicdLastSuccessTimestamp); n != 0 {
		t.Errorf("last_success should not be stamped after an error, got %d series", n)
	}
}

// TestCICD_OnDefaultRegistry proves the gardenio_cicd_* families are registered
// on the DEFAULT registry (via promauto) — the same gatherer the :9092
// /metrics handler serves. A private registry would increment but never scrape.
func TestCICD_OnDefaultRegistry(t *testing.T) {
	resetCICDMetrics()

	fake := newFakeCICDSource()
	fake.pipelines["grp/proj"] = []GitLabPipeline{{ID: 500, Status: "success", Ref: "main"}}
	fake.jobs[500] = []GitLabJob{{Name: "build", Duration: fptr(60), Runner: &GitLabRunner{RunnerType: "instance_type"}}}

	c := NewCICDMetricsCollector(CICDMetricsConfig{
		Pipelines:                fake,
		Projects:                 []string{"grp/proj"},
		GitLabJobDurationEnabled: true,
	})
	c.collectJobDuration(context.Background())

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default registry: %v", err)
	}
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{
		"gardenio_cicd_minutes_total",
		"gardenio_cicd_runs_total",
		"gardenio_cicd_job_duration_seconds",
		"gardenio_cicd_scrape_duration_seconds",
	} {
		if !names[want] {
			t.Errorf("metric family %q not on the default registry (would never be scraped via /metrics)", want)
		}
	}
}

// TestRefClass covers the ref-class coarsening that bounds counter cardinality.
func TestRefClass(t *testing.T) {
	cases := map[string]string{
		"main":                 "main",
		"master":               "main",
		"2026.202.1":           "tag",
		"v1.2.3":               "tag",
		"feature/gd-abc":       "branch",
		"chore/formula-ux/xyz": "branch",
		"":                     "unknown",
	}
	for in, want := range cases {
		if got := refClass(in); got != want {
			t.Errorf("refClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSubgroupTotals covers the nested-subgroup rollup helper.
func TestSubgroupTotals(t *testing.T) {
	projects := []GitLabCIMinutesProject{
		{FullPath: "Acme/Platform/gardenio", Minutes: 100},
		{FullPath: "Acme/Platform/acme-helm-chart", Minutes: 50},
		{FullPath: "Acme/Other/thing", Minutes: 25},
		{FullPath: "Elsewhere/x/y", Minutes: 999}, // outside the group prefix — ignored
	}
	got := subgroupTotals("Acme", projects)
	if got["Acme/Platform"] != 150 {
		t.Errorf("Platform rollup = %v, want 150", got["Acme/Platform"])
	}
	if got["Acme/Other"] != 25 {
		t.Errorf("Other rollup = %v, want 25", got["Acme/Other"])
	}
	if _, ok := got["Elsewhere/x"]; ok {
		t.Errorf("Elsewhere should be excluded (outside group prefix)")
	}
}
