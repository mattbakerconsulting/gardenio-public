package bridge

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestResumeReasonLabel(t *testing.T) {
	cases := map[string]string{
		"max_runs":             "max_runs",
		"no_progress_for":      "no_progress_for",
		"wall_clock":           "wall_clock",
		"consecutive_failures": "consecutive_failures",
		"goal_satisfied":       "other", // not a premature kind
		"":                     "other",
		"weird_future_kind":    "other",
	}
	for in, want := range cases {
		if got := resumeReasonLabel(in); got != want {
			t.Errorf("resumeReasonLabel(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestRecordGardenerResume_Increments(t *testing.T) {
	gardenerResumeTotal.Reset()
	recordGardenerResume("gd-cap-x", resumeActionRearm, "max_runs")
	recordGardenerResume("gd-cap-x", resumeActionRearm, "max_runs")
	recordGardenerResume("gd-cap-x", resumeActionEscalate, "consecutive_failures")

	if got := testutil.ToFloat64(gardenerResumeTotal.WithLabelValues("gd-cap-x", resumeActionRearm, "max_runs")); got != 2 {
		t.Errorf("rearm/max_runs counter = %v, want 2", got)
	}
	if got := testutil.ToFloat64(gardenerResumeTotal.WithLabelValues("gd-cap-x", resumeActionEscalate, "consecutive_failures")); got != 1 {
		t.Errorf("escalate/consecutive_failures counter = %v, want 1", got)
	}
}

// TestGardenerResumeMetricsOnDefaultRegistry proves the gardener-resume metrics
// are registered on the DEFAULT Prometheus registry (via promauto). This is the
// contract the gardener-firing binaries depend on: trigger-engine and
// gitlab-bridge serve /metrics from prometheus.DefaultGatherer, so a metric that
// landed on a private registry would increment but never be scraped — exactly
// the gap that left the resume observability layer (bead ka-0000000971, decision
// ka-0000006918) invisible. Asserting via DefaultGatherer.Gather (not a fresh
// registry) is what ties this metric to the served endpoint. *Vec families emit
// no exposition until their first observed label set, so we record one resume
// first, then assert both families surface from the default gatherer.
func TestGardenerResumeMetricsOnDefaultRegistry(t *testing.T) {
	gardenerResumeTotal.Reset()
	gardenerResumesWithoutProgress.Reset()
	recordGardenerResume("gd-cap-reg", resumeActionRearm, "no_progress_for")
	setGardenerResumesWithoutProgress("gd-cap-reg", 3)

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default registry: %v", err)
	}
	names := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{"gardener_resume_total", "gardener_resumes_without_progress"} {
		if !names[want] {
			t.Errorf("metric family %q not on the default registry (would never be scraped via /metrics)", want)
		}
	}

	// Exposition sanity: the served text format carries the recorded series.
	text, err := testutil.GatherAndCount(prometheus.DefaultGatherer, "gardener_resume_total")
	if err != nil {
		t.Fatalf("gather gardener_resume_total: %v", err)
	}
	if text < 1 {
		t.Errorf("gardener_resume_total exposed %d series, want >=1", text)
	}
	if got := testutil.ToFloat64(gardenerResumesWithoutProgress.WithLabelValues("gd-cap-reg")); got != 3 {
		t.Errorf("gardener_resumes_without_progress{gd-cap-reg} = %v, want 3", got)
	}
}
