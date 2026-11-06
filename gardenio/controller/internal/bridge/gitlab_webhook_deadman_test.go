package bridge

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// webhookFakeGroupMRLister is a ListGroupMergeRequests stub for the GitLab
// webhook monitor tests. It records the arguments it was asked and returns a
// fixed slice/error. The confirmation arm keys on len(mrs), so a slice of the
// desired length (contents irrelevant) is enough.
type webhookFakeGroupMRLister struct {
	mrs             []GitLabMR
	err             error
	calls           int
	gotGroupID      int
	gotState        string
	gotUpdatedAfter time.Time
}

func (f *webhookFakeGroupMRLister) ListGroupMergeRequests(_ context.Context, groupID int, state string, updatedAfter time.Time) ([]GitLabMR, error) {
	f.calls++
	f.gotGroupID = groupID
	f.gotState = state
	f.gotUpdatedAfter = updatedAfter
	return f.mrs, f.err
}

// makeMRs returns a slice of n zero-value MRs — the monitor only counts them.
func makeMRs(n int) []GitLabMR {
	if n <= 0 {
		return nil
	}
	return make([]GitLabMR, n)
}

// resetGitLabWebhookDeadmanMetrics clears every deadman series so each test
// starts from a known baseline (the promauto families are process-global). The
// deliveries counter (gitlab_webhook_metrics.go) is reset separately where a
// test exercises recordGitLabWebhookOutcome.
func resetGitLabWebhookDeadmanMetrics() {
	gitlabWebhookDeliveries.Reset()
	gitlabWebhookEvaluations.Reset()
	gitlabWebhookDark.Set(0)
	gitlabWebhookUpstreamMatches.Set(0)
	lastGitLabWebhookReceivedNano.Store(0)
}

func testGitLabMonitor(lister gitlabGroupMRLister, window time.Duration, now time.Time) *GitLabWebhookMonitor {
	return NewGitLabWebhookMonitor(GitLabWebhookMonitorConfig{
		GitLab:  lister,
		GroupID: 42,
		Window:  window,
		Logger:  slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		Clock:   func() time.Time { return now },
	})
}

// recordGitLabWebhookOutcome stamps the receive-liveness clock for every
// authenticated, well-formed delivery (processed|ignored|error) — all three
// passed the X-Gitlab-Token check and decoded, so they prove the hook is
// registered and correctly secreted. token_failed and bad_request must NOT stamp
// it — a wrong-secret or malformed delivery is not proof of a working hook, so it
// cannot keep the deadman quiet. (error is a 500 recorded only after decode — a
// downstream failure, not a receive-path failure — so it counts as liveness; the
// Jira handler has no error outcome, so this arm is GitLab-specific.)
func TestRecordGitLabWebhookOutcome_OnlyAuthenticatedStampsClock(t *testing.T) {
	for _, outcome := range []string{gitlabWebhookOutcomeProcessed, gitlabWebhookOutcomeIgnored, gitlabWebhookOutcomeError} {
		resetGitLabWebhookDeadmanMetrics()
		recordGitLabWebhookOutcome(outcome, time.Time{})
		if lastGitLabWebhookReceivedNano.Load() == 0 {
			t.Errorf("outcome %q must stamp the receive-liveness clock (authenticated + decoded proves the hook is live)", outcome)
		}
		if got := testutil.ToFloat64(gitlabWebhookDeliveries.WithLabelValues(outcome)); got != 1 {
			t.Errorf("deliveries{outcome=%q} = %v, want 1", outcome, got)
		}
	}
	for _, outcome := range []string{gitlabWebhookOutcomeTokenFailed, gitlabWebhookOutcomeBadRequest} {
		resetGitLabWebhookDeadmanMetrics()
		recordGitLabWebhookOutcome(outcome, time.Time{})
		if lastGitLabWebhookReceivedNano.Load() != 0 {
			t.Errorf("outcome %q must NOT stamp the receive-liveness clock (rejected traffic is not proof of a working hook)", outcome)
		}
		if got := testutil.ToFloat64(gitlabWebhookDeliveries.WithLabelValues(outcome)); got != 1 {
			t.Errorf("deliveries{outcome=%q} = %v, want 1", outcome, got)
		}
	}
}

// A recent authenticated delivery is the healthy state: the verdict clears to 0
// and no upstream scan is made (cheap early-out for the common case).
func TestGitLabEvaluate_RecentDelivery_NotDark(t *testing.T) {
	resetGitLabWebhookDeadmanMetrics()
	now := time.Unix(1_700_000_000, 0)
	lastGitLabWebhookReceivedNano.Store(now.Add(-30 * time.Minute).UnixNano())
	lister := &webhookFakeGroupMRLister{mrs: makeMRs(999)}
	m := testGitLabMonitor(lister, 2*time.Hour, now)

	m.evaluate(context.Background())

	if got := testutil.ToFloat64(gitlabWebhookDark); got != 0 {
		t.Errorf("dark gauge = %v, want 0 (recent delivery)", got)
	}
	if lister.calls != 0 {
		t.Errorf("upstream scan must NOT be called when a delivery is recent, got %d calls", lister.calls)
	}
	if got := testutil.ToFloat64(gitlabWebhookEvaluations.WithLabelValues(gitlabWebhookEvalOK)); got != 1 {
		t.Errorf("ok evaluations = %v, want 1", got)
	}
}

// No delivery in the window AND upstream activity exists is the dark signature —
// matter arrived upstream while the receive path stayed silent. The gauge trips
// to 1, the scan is scoped to the group + "all" state, and the updated_after
// bound is window-back from now.
func TestGitLabEvaluate_NoDeliveryWithUpstreamActivity_Dark(t *testing.T) {
	resetGitLabWebhookDeadmanMetrics()
	now := time.Unix(1_700_000_000, 0)
	lastGitLabWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano())
	lister := &webhookFakeGroupMRLister{mrs: makeMRs(7)}
	m := testGitLabMonitor(lister, 2*time.Hour, now)

	m.evaluate(context.Background())

	if got := testutil.ToFloat64(gitlabWebhookDark); got != 1 {
		t.Errorf("dark gauge = %v, want 1 (no delivery in window, upstream activity)", got)
	}
	if got := testutil.ToFloat64(gitlabWebhookUpstreamMatches); got != 7 {
		t.Errorf("upstream_matches = %v, want 7", got)
	}
	if lister.gotGroupID != 42 {
		t.Errorf("scan group id = %d, want 42", lister.gotGroupID)
	}
	if lister.gotState != "all" {
		t.Errorf("scan state = %q, want all", lister.gotState)
	}
	if want := now.Add(-2 * time.Hour); !lister.gotUpdatedAfter.Equal(want) {
		t.Errorf("scan updated_after = %v, want window-back %v", lister.gotUpdatedAfter, want)
	}
}

// The wrong-secret case: the hook is registered and GitLab is delivering, but
// the X-Gitlab-Token is wrong, so every delivery is token_failed and NONE stamps
// the clock. With upstream activity present, this must page — not read as
// healthy. (The GitLab analog of the Jira ACCEPTANCE clause.)
func TestGitLabEvaluate_WrongSecretTokenFailuresOnly_Dark(t *testing.T) {
	resetGitLabWebhookDeadmanMetrics()
	now := time.Unix(1_700_000_000, 0)
	// Seed the startup grace to well before the window so the pod is past grace.
	lastGitLabWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano())
	// A stream of token failures arrives — these increment the counter but never
	// stamp the liveness clock. recordGitLabWebhookOutcome stamps time.Now(), so
	// re-seed the clock afterward to keep the deterministic test clock.
	for range 20 {
		recordGitLabWebhookOutcome(gitlabWebhookOutcomeTokenFailed, time.Time{})
	}
	lastGitLabWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano())
	lister := &webhookFakeGroupMRLister{mrs: makeMRs(5)}
	m := testGitLabMonitor(lister, 2*time.Hour, now)

	m.evaluate(context.Background())

	if got := testutil.ToFloat64(gitlabWebhookDark); got != 1 {
		t.Errorf("dark gauge = %v, want 1 (token-failures-only must page, not read as healthy)", got)
	}
}

// No delivery in the window but genuinely quiet upstream (0 recent MRs) is the
// healthy-quiet state the unfireable-alert trap warns about: no activity =>
// no page. The gauge stays 0.
func TestGitLabEvaluate_NoDeliveryQuietUpstream_NotDark(t *testing.T) {
	resetGitLabWebhookDeadmanMetrics()
	now := time.Unix(1_700_000_000, 0)
	lastGitLabWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano())
	lister := &webhookFakeGroupMRLister{mrs: nil}
	m := testGitLabMonitor(lister, 2*time.Hour, now)

	m.evaluate(context.Background())

	if got := testutil.ToFloat64(gitlabWebhookDark); got != 0 {
		t.Errorf("dark gauge = %v, want 0 (quiet upstream must not page)", got)
	}
	if got := testutil.ToFloat64(gitlabWebhookEvaluations.WithLabelValues(gitlabWebhookEvalOK)); got != 1 {
		t.Errorf("ok evaluations = %v, want 1", got)
	}
}

// A scan error on a no-delivery cycle must NOT page: darkness cannot be
// confirmed, a real dark state persists to the next cycle, and firing on an
// unconfirmed guess would false-page. The verdict clears to 0 and the cycle is
// counted as scan_error (so the deadman leg still sees a heartbeat).
func TestGitLabEvaluate_ScanError_NotDark(t *testing.T) {
	resetGitLabWebhookDeadmanMetrics()
	now := time.Unix(1_700_000_000, 0)
	lastGitLabWebhookReceivedNano.Store(now.Add(-3 * time.Hour).UnixNano())
	gitlabWebhookDark.Set(1) // must be cleared even from a prior tripped state
	lister := &webhookFakeGroupMRLister{err: errors.New("boom")}
	m := testGitLabMonitor(lister, 2*time.Hour, now)

	m.evaluate(context.Background())

	if got := testutil.ToFloat64(gitlabWebhookDark); got != 0 {
		t.Errorf("dark gauge = %v, want 0 (unconfirmed scan error must not page)", got)
	}
	if got := testutil.ToFloat64(gitlabWebhookEvaluations.WithLabelValues(gitlabWebhookEvalScanError)); got != 1 {
		t.Errorf("scan_error evaluations = %v, want 1 (heartbeat must still beat)", got)
	}
}

// initGitLabWebhookDeadmanMetrics touches the deadman series to zero and seeds
// the liveness clock to startup (only if unset), so increase()/absent() have a
// baseline the instant the pod is first scraped and a fresh pod gets one window
// of grace.
func TestInitGitLabWebhookDeadmanMetrics_BaselinesAndSeedsGrace(t *testing.T) {
	resetGitLabWebhookDeadmanMetrics()
	now := time.Unix(1_700_000_000, 0)
	initGitLabWebhookDeadmanMetrics(now)

	for _, r := range []string{gitlabWebhookEvalOK, gitlabWebhookEvalScanError} {
		if got := testutil.ToFloat64(gitlabWebhookEvaluations.WithLabelValues(r)); got != 0 {
			t.Errorf("evaluations{result=%q} baseline = %v, want 0", r, got)
		}
	}
	if lastGitLabWebhookReceivedNano.Load() != now.UnixNano() {
		t.Errorf("liveness clock = %v, want seeded to startup %v", lastGitLabWebhookReceivedNano.Load(), now.UnixNano())
	}

	// A subsequent init must NOT clobber a clock a real delivery already stamped.
	later := now.Add(time.Hour)
	recordGitLabWebhookOutcome(gitlabWebhookOutcomeProcessed, time.Time{})
	stamped := lastGitLabWebhookReceivedNano.Load()
	initGitLabWebhookDeadmanMetrics(later)
	if lastGitLabWebhookReceivedNano.Load() != stamped {
		t.Errorf("init must not overwrite a real delivery's clock: got %v, want %v", lastGitLabWebhookReceivedNano.Load(), stamped)
	}
}

// A fresh pod (clock seeded to startup, no delivery yet) must be within grace on
// the first evaluation — a restart must never immediately page.
func TestGitLabEvaluate_FreshPodWithinGrace_NotDark(t *testing.T) {
	resetGitLabWebhookDeadmanMetrics()
	start := time.Unix(1_700_000_000, 0)
	lister := &webhookFakeGroupMRLister{mrs: makeMRs(999)}
	m := testGitLabMonitor(lister, 2*time.Hour, start)
	initGitLabWebhookDeadmanMetrics(start) // seeds clock to start

	m.evaluate(context.Background()) // evaluated at the same instant

	if got := testutil.ToFloat64(gitlabWebhookDark); got != 0 {
		t.Errorf("dark gauge = %v, want 0 (fresh pod is within its grace window)", got)
	}
	if lister.calls != 0 {
		t.Errorf("a within-grace pod must not scan upstream, got %d calls", lister.calls)
	}
}

// NewGitLabWebhookMonitor applies its defaults when the config leaves Window /
// Interval / Logger / Clock unset.
func TestNewGitLabWebhookMonitor_Defaults(t *testing.T) {
	m := NewGitLabWebhookMonitor(GitLabWebhookMonitorConfig{GroupID: 1})
	if m.window != defaultGitLabWebhookDeadmanWindow {
		t.Errorf("window = %v, want default %v", m.window, defaultGitLabWebhookDeadmanWindow)
	}
	if m.interval != defaultGitLabWebhookDeadmanInterval {
		t.Errorf("interval = %v, want default %v", m.interval, defaultGitLabWebhookDeadmanInterval)
	}
	if m.logger == nil {
		t.Error("logger must default to non-nil")
	}
	if m.clock == nil {
		t.Error("clock must default to non-nil")
	}
}
