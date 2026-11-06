package bridge

// Inbound-webhook receive-path DARK-HOOK DEADMAN monitor for the gitlab-bridge
// (GAP-C of the non-AVA webhook carve-out ka-0000005632; port of
// jira_webhook_deadman.go). This is the GitLab-door twin of the Jira deadman:
// same failure it detects, same fold-the-verdict-into-a-gauge discipline, same
// heartbeat/deadman leg — adapted to GitLab's primitives.
//
// THE DETECTION GAP THIS CLOSES. gitlab_webhook_metrics.go already added
// gardenio_gitlab_webhook_deliveries_total{outcome} — the raw receive-path
// liveness COUNTER, so a rejected-but-arriving stream (token_failed/bad_request)
// is distinguishable from a healthy one. But a COUNTER alone cannot tell
// "receiving nothing because the hook is dark" from "receiving nothing because
// upstream is genuinely quiet": in the fully-dark case (deregistered hook /
// wrong X-Gitlab-Token at the GitLab console producing no delivery at all)
// nothing increments the counter — exactly the state to detect — so there is no
// sample to alert on. The Jira side of this same carve-out went 100% dark in
// prod for 8 days while every dashboard stayed green (bug ka-0000000426); this
// gives the GitLab door the same deadman the Jira door already has.
//
// WHY A MONITOR AND NOT A HANDLER-COMPUTED GAUGE. The webhook path is
// event-driven and — in exactly the failure state we are detecting — SILENT: no
// request arrives to run any handler code, so the verdict cannot be computed
// there. GitLabWebhookMonitor is a periodic evaluator (mirroring the mention-
// catchup / MR-backstop sweep loops in this same package) that folds the "0
// authenticated deliveries in the window WHILE upstream activity exists" verdict
// into a single set-per-cycle gauge, gardenio_gitlab_webhook_dark.
//
// THE CONFIRMATION ARM (the GitLab adaptation). The Jira monitor confirms
// "matter existed upstream" with an ApproximateCount over a JQL scope. GitLab
// has no JQL; the parallel primitive is ListGroupMergeRequests(groupID, "all",
// updatedAfter) — the group's merge requests touched within the window. GitLab
// bumps an MR's updated_at on new notes/discussions, approvals, pushes, and
// state changes — precisely the events the merge_request/note/pipeline webhooks
// deliver — so a non-empty result means webhook-worthy activity happened
// upstream in the window while the receive path stayed silent. The count is the
// length of the (windowed, hence small) list; no separate count endpoint exists.
//
// THE UNFIREABLE-ALERT TRAP (see jira_webhook_deadman.go / the sibling
// poller-stall lesson). The alert must stay QUIET in the genuinely-quiet-upstream
// state — no MR activity in the window => no page — and fire only on "matter
// existed upstream and we received nothing." So the verdict has two arms: (a) no
// authenticated, well-formed delivery received within the deadman window, AND
// (b) the independent group-MR activity scan still reports activity. Arm (a)
// keys on the last delivery that passed the X-Gitlab-Token check and decoded
// (processed|ignored|error) — NOT on total request count. That is deliberate: a
// wrong secret in the GitLab console looks like a working hook from GitLab's side
// but only ever produces token_failed here, so keying "alive" on authenticated
// deliveries makes the wrong-secret case page rather than read as healthy
// (mirrors the Jira ACCEPTANCE clause). error (a 500 recorded only after decode —
// a downstream bead-list failure) counts as liveness too: it proves GitLab is
// delivering correctly-secreted, well-formed hooks, so a daemon outage must not
// false-page as a dark hook. See recordGitLabWebhookOutcome for the full split.
//
// FOLDED, NOT JOINED. The whole verdict lives in the gauge (a trivial ==1 in the
// rule) rather than a multi-term PromQL join, because an absent series inside an
// `and` silently suppresses the page — the exact failure the sibling Helm tests
// guard against. A companion heartbeat counter
// (gardenio_gitlab_webhook_evaluations_total) backs a deadman leg that catches
// the OTHER failure shape the gauge alone cannot see: the monitor not running at
// all (pod down / hung goroutine), which would otherwise freeze the dark gauge
// at its last value and silently suppress the page.
//
// Registered globally against the default registerer (promauto), matching the
// deliveries counter and the other bridge metrics. gitlab-bridge serves /metrics
// from prometheus.DefaultGatherer on its internal :9092 server (never the public
// /webhook listener), so these series are scrapeable via the pod annotations.
// The paging PrometheusRule lives in
// helm/gardenio/templates/gitlab-bridge/webhook-deadman-alert.yaml.

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Heartbeat labels for gardenio_gitlab_webhook_evaluations_total — the deadman
// signal for the monitor loop itself. Incremented on EVERY evaluation cycle so a
// live-but-erroring monitor keeps the deadman quiet while a dead/absent monitor
// fires. Mirrors the Jira webhookEval* vocabulary.
const (
	gitlabWebhookEvalOK        = "ok"         // evaluation completed (verdict set)
	gitlabWebhookEvalScanError = "scan_error" // upstream activity scan failed; verdict left cleared this cycle
)

// defaultGitLabWebhookDeadmanWindow is how long the receive path may go without
// an authenticated delivery before it is judged dark (given upstream activity
// exists). 2h is long enough that a healthy but momentarily quiet hook is never
// flagged, while the real dark case (deregistered / wrong-secret) persists for
// hours and pages. Overridable via GITLAB_WEBHOOK_DEADMAN_WINDOW. Matches the
// Jira default.
const defaultGitLabWebhookDeadmanWindow = 2 * time.Hour

// defaultGitLabWebhookDeadmanInterval is the monitor's evaluation cadence. Also
// the heartbeat cadence the deadman leg reads. Overridable via
// GITLAB_WEBHOOK_DEADMAN_INTERVAL. Matches the Jira default.
const defaultGitLabWebhookDeadmanInterval = 5 * time.Minute

var (
	// gardenio_gitlab_webhook_dark is the Go-computed dark verdict: 1 while no
	// authenticated, well-formed inbound delivery has been received within the
	// deadman window AND an independent scan of the group's recently-updated
	// merge requests still reports upstream activity — matter existed upstream
	// and we received nothing. 0 in every healthy state (a delivery arrived
	// recently) and in the genuinely-quiet-upstream state (no activity => no
	// page). The verdict is folded entirely into this gauge (not a multi-term
	// PromQL join) so the alert expr is a trivial ==1 that an absent series
	// cannot suppress.
	gitlabWebhookDark = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gardenio_gitlab_webhook_dark",
		Help: "1 while no authenticated well-formed inbound GitLab webhook delivery was received within the deadman window while upstream MR activity exists on the group scope; 0 when healthy or genuinely quiet upstream (ka-0000005632 GAP-C).",
	})

	// gardenio_gitlab_webhook_evaluations_total counts completed monitor
	// evaluation cycles by result. The deadman leg reads this: increase over the
	// window == 0 (or the series absent) means the monitor is not evaluating at
	// all — a dead/hung monitor or a down pod that would otherwise freeze the
	// dark gauge and silently suppress the page.
	gitlabWebhookEvaluations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gardenio_gitlab_webhook_evaluations_total",
		Help: "GitLab webhook deadman monitor evaluation cycles by result (ok|scan_error); the deadman signal for the monitor loop.",
	}, []string{"result"})

	// gardenio_gitlab_webhook_upstream_matches is the most recent count of
	// recently-updated group merge requests observed on a dark evaluation — the
	// upstream side of the verdict, exposed as context so an operator can see how
	// much activity was arriving upstream while the receive path stayed silent.
	// Populated only on the (rare) no-delivery branch; not paged on directly.
	gitlabWebhookUpstreamMatches = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gardenio_gitlab_webhook_upstream_matches",
		Help: "Count of recently-updated group merge requests observed on the most recent dark evaluation (context for the dark gauge).",
	})

	// lastGitLabWebhookReceivedNano is the wall-clock (UnixNano) of the most
	// recent authenticated, well-formed delivery (processed|ignored). Written by
	// recordGitLabWebhookOutcome; read by the monitor. Seeded to pod startup
	// (only if still unset) so a fresh pod gets one deadman window of grace
	// before it can be judged dark — a restart must not immediately page.
	lastGitLabWebhookReceivedNano atomic.Int64
)

// gitlabGroupMRLister is the slice of *GitLabClient the monitor needs for its
// upstream-activity confirmation arm. Tests inject a tiny fake. Mirrors the
// jiraCounter interface (jira_metrics.go) in spirit.
type gitlabGroupMRLister interface {
	// ListGroupMergeRequests lists MRs in a group filtered by state, newest
	// first, restricted to those updated after the given time.
	ListGroupMergeRequests(ctx context.Context, groupID int, state string, updatedAfter time.Time) ([]GitLabMR, error)
}

// Compile-time assertion that the production GitLab client satisfies the surface.
var _ gitlabGroupMRLister = (*GitLabClient)(nil)

// initGitLabWebhookDeadmanMetrics touches the deadman-specific series to their
// zero baseline so increase()/absent() have a sample the instant the pod is
// first scraped, and seeds the receive-liveness clock to startup (only if no
// delivery has already stamped it), giving a fresh pod one deadman window of
// grace. The deliveries counter itself is seeded separately by
// InitGitLabWebhookMetrics (gitlab_webhook_metrics.go), called at bridge
// startup; this seeds only the monitor's own families. Called once at the top of
// the monitor's Run. Mirrors initWebhookMetrics (jira_webhook_deadman.go).
func initGitLabWebhookDeadmanMetrics(now time.Time) {
	gitlabWebhookEvaluations.WithLabelValues(gitlabWebhookEvalOK).Add(0)
	gitlabWebhookEvaluations.WithLabelValues(gitlabWebhookEvalScanError).Add(0)
	gitlabWebhookDark.Set(0)
	gitlabWebhookUpstreamMatches.Set(0)
	lastGitLabWebhookReceivedNano.CompareAndSwap(0, now.UnixNano())
}

// GitLabWebhookMonitorConfig configures the inbound-webhook deadman monitor.
type GitLabWebhookMonitorConfig struct {
	GitLab   gitlabGroupMRLister // group-MR listing for the upstream-activity confirmation arm
	GroupID  int                 // GitLab group to scan for recent MR activity
	Window   time.Duration       // deadman window; 0 uses defaultGitLabWebhookDeadmanWindow
	Interval time.Duration       // evaluation cadence; 0 uses defaultGitLabWebhookDeadmanInterval
	Logger   *slog.Logger        // default slog.Default()
	Clock    func() time.Time    // injectable clock; default time.Now
}

// GitLabWebhookMonitor periodically folds the inbound-webhook dark verdict into
// gardenio_gitlab_webhook_dark. It is the receive-path analog of the Jira
// bridge's deadman monitor — necessary because the receive path is event-driven
// and silent in exactly the failure state being detected.
type GitLabWebhookMonitor struct {
	gitlab   gitlabGroupMRLister
	groupID  int
	window   time.Duration
	interval time.Duration
	logger   *slog.Logger
	clock    func() time.Time
}

// NewGitLabWebhookMonitor builds the monitor. Defaults: Window 2h, Interval 5m,
// Logger slog.Default(), Clock time.Now.
func NewGitLabWebhookMonitor(cfg GitLabWebhookMonitorConfig) *GitLabWebhookMonitor {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	window := cfg.Window
	if window <= 0 {
		window = defaultGitLabWebhookDeadmanWindow
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultGitLabWebhookDeadmanInterval
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &GitLabWebhookMonitor{
		gitlab:   cfg.GitLab,
		groupID:  cfg.GroupID,
		window:   window,
		interval: interval,
		logger:   logger,
		clock:    clock,
	}
}

// Run baselines the metrics, evaluates once immediately, then on every tick
// until ctx is cancelled. Mirrors JiraWebhookMonitor.Run / the sweep loops in
// this package.
func (m *GitLabWebhookMonitor) Run(ctx context.Context) error {
	initGitLabWebhookDeadmanMetrics(m.clock())
	m.evaluate(ctx)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			m.evaluate(ctx)
		}
	}
}

// evaluate (re)computes the dark verdict for one cycle and beats the heartbeat.
//
//   - An authenticated delivery within the window → healthy; verdict cleared to
//     0. No upstream scan (the cheap early-out for the common case).
//   - Otherwise confirm whether matter actually exists upstream via the group-MR
//     activity scan. matches > 0 with no delivery in the window is the dark
//     signature → 1. matches == 0 (genuinely quiet upstream) → 0. A scan ERROR
//     clears the verdict to 0 and logs: a single transient scan failure is not
//     evidence of darkness, a real dark state persists to the next cycle, and
//     firing on an unconfirmed guess would false-page (same discipline as the
//     Jira monitor's count-error arm).
func (m *GitLabWebhookMonitor) evaluate(ctx context.Context) {
	now := m.clock()
	last := time.Unix(0, lastGitLabWebhookReceivedNano.Load())
	if now.Sub(last) < m.window {
		gitlabWebhookEvaluations.WithLabelValues(gitlabWebhookEvalOK).Inc()
		gitlabWebhookDark.Set(0)
		return
	}

	updatedAfter := now.Add(-m.window)
	mrs, err := m.gitlab.ListGroupMergeRequests(ctx, m.groupID, "all", updatedAfter)
	if err != nil {
		gitlabWebhookEvaluations.WithLabelValues(gitlabWebhookEvalScanError).Inc()
		m.logger.Warn("gitlab-webhook deadman: upstream MR activity scan failed; not treating the receive path as dark",
			"group_id", m.groupID, "error", err)
		gitlabWebhookDark.Set(0)
		return
	}

	gitlabWebhookEvaluations.WithLabelValues(gitlabWebhookEvalOK).Inc()
	matches := len(mrs)
	gitlabWebhookUpstreamMatches.Set(float64(matches))
	if matches > 0 {
		gitlabWebhookDark.Set(1)
		m.logger.Warn("gitlab-webhook DARK: no authenticated inbound webhook delivery received within the deadman window while upstream MR activity exists — the inbound webhook path is receiving nothing (runbook: task ka-0000005632 / bug ka-0000000426; likely root cause: unregistered/mis-secreted GitLab webhook)",
			"group_id", m.groupID, "upstream_matches", matches, "window", m.window, "last_received", last)
		return
	}
	gitlabWebhookDark.Set(0)
}
