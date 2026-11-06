package bridge

// Ingestion-poller stall/liveness metrics for the jira-bridge (prevention alert
// of epic ka-0000005107, task ka-0000001629).
//
// Sibling of the trigger-engine starvation detector (trigger_starve.go): where
// gardenio_trigger_starved guards the TRIGGER path (a cooldown-skip on
// last_checked, the actual 2026-07-01 outage mechanism), this guards the
// INGESTION poll() path — the loop that turns matching JIRA issues into source
// beads. The ingestion poller has no last_checked/cooldown gate, so the
// outage's exact mechanism cannot recur here; this is the residual DETECTION
// net so any FUTURE 0-new-beads ingestion stall pages instead of being noticed
// only via missing downstream activity.
//
// FIDELITY TO THE BEAD SIGNAL. The bead asks to fire when the bridge "polled
// AND 0 new beads created for N > threshold minutes WHILE JQL ApproximateCount
// > tracked count." The literal comparison (ApproximateCount > len(tracked)) is
// UNFIREABLE as written: the ingestion JQL is open-status-only (prod
// JIRA_STATUSES="To Do,Ready for Development") but the tracked map is seeded by
// CatchUp with closed/deferred/blocked beads too, so in any mature project
// len(tracked) >> the open-only upstream count and the comparison never holds
// (the AssistantPaneSilent unfireable-alert lesson, ka-0000000664). And in the
// ingestion path a search that RETURNS untracked matches always attempts
// creation — the only way "search saw matches but created 0" happens is a
// per-issue create error, a self-healing condition whose gauge a single
// poison-pill issue would pin forever (a false page). So the faithful, fireable
// rendering of the same intent — "matter exists upstream but none is being
// materialized" — is the blind-search signature: a poll whose ingestion JQL
// search returned ZERO rows while an independent approximate-count of the SAME
// ingestion JQL still reports matching issues. That is exactly "polled, 0 new
// beads, yet matches exist," expressed on a signal that can actually fire and
// stays quiet in the healthy states (empty upstream => count 0; quiet project
// with tracked-but-open issues => search returns those rows, so not a zero-row
// cycle).
//
// Registered globally against the default registerer (promauto), matching the
// other bridge metrics (jira_metrics.go, trigger_starve.go). The jira-bridge
// serves /metrics from prometheus.DefaultGatherer on the internal :9093 server,
// so these series are scrapeable (Path A pod annotations + Path B
// VMServiceScrape); in the controller binary the poller never runs, so the
// families stay at their zero baseline. The paging PrometheusRule lives in
// helm/gardenio/templates/jira-bridge/poller-stall-alert.yaml.

import (
	"context"
	"regexp"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Poll-result labels for gardenio_jira_poller_polls_total. A poll cycle is
// counted on EVERY terminal path so the deadman (increase == 0) fires only when
// the loop has genuinely stopped ticking — a still-running-but-erroring loop
// must not trip it, and a hung/dead loop must not be able to suppress its own
// page by never incrementing.
const (
	pollResultOK          = "ok"            // search completed (returned or not)
	pollResultSearchError = "search_error"  // SearchIssues failed (Jira-side/transient)
	pollResultNotCaughtUp = "not_caught_up" // fail-closed catch-up gate not yet passed
)

var (
	// gardenio_jira_poller_stalled is the Go-computed stall verdict: 1 while an
	// ingestion poll cycle's JQL search returned zero rows AND an independent
	// approximate-count of the same ingestion JQL reports matching issues
	// upstream — matter exists to materialize but the poller's own search is
	// blind to it. 0 in every healthy state. The verdict is folded entirely
	// into this gauge (rather than left to a multi-term PromQL join) so the
	// alert expr is a trivial ==1: a set-per-cycle gauge cannot silently
	// suppress the page via an absent series inside an `and`, the failure mode
	// the sibling trigger-starved Helm test explicitly guards against.
	jiraPollerStalled = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gardenio_jira_poller_stalled",
		Help: "1 while a JIRA ingestion poll returned zero rows but an approximate-count of the same ingestion JQL reports matching issues upstream (no new beads are being materialized while matter exists); 0 when healthy.",
	})

	// gardenio_jira_poller_polls_total counts completed ingestion poll cycles by
	// terminal result. The deadman leg of the alert reads this: increase over
	// the window == 0 (or the series absent) means the poll loop is not ticking
	// at all — a hung goroutine, a crash-loop, or a poller wedged before its
	// first cycle.
	jiraPollerPolls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gardenio_jira_poller_polls_total",
		Help: "JIRA ingestion poll cycles by terminal result (ok|search_error|not_caught_up); the deadman signal for the poll loop.",
	}, []string{"result"})

	// gardenio_jira_poller_beads_created_total counts source beads materialized
	// by the ingestion poller (both the per-cycle poll and the startup full
	// scan). Context/observability for the stall gauge — the direct measure of
	// the "0 new beads" the bead names — not itself paged on. Note a
	// server-side dedup hit (an already-existing bead found during the
	// duplicate check) is counted here too: it repairs the tracked map and is
	// indistinguishable from a fresh create at this layer, so treat this as
	// "beads materialized or dedup-repaired," not strictly "brand-new beads."
	jiraPollerBeadsCreated = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gardenio_jira_poller_beads_created_total",
		Help: "Source beads materialized by the JIRA ingestion poller (poll + full scan); context for the stall gauge, not paged on directly.",
	})

	// gardenio_jira_poller_upstream_matches is the most recent approximate-count
	// of the ingestion JQL observed on a zero-row poll cycle — the upstream
	// side of the stall verdict, exposed as context so an operator can see how
	// many issues were sitting unmaterialized when the gauge tripped. Populated
	// only on the (rare) zero-row branch; not paged on directly.
	jiraPollerUpstreamMatches = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gardenio_jira_poller_upstream_matches",
		Help: "Approximate-count of the ingestion JQL observed on the most recent zero-row poll cycle (context for the stall gauge).",
	})
)

// jqlOrderByRE matches a trailing `ORDER BY ...` clause (buildJQL always
// appends `ORDER BY created DESC`). The /search/approximate-count endpoint
// rejects an ORDER BY with HTTP 400, so it MUST be stripped before the stall
// probe calls ApproximateCount — otherwise every probe errors and the stall
// arm is silently dead.
var jqlOrderByRE = regexp.MustCompile(`(?i)\s+order\s+by\s+.+$`)

// stripOrderBy removes a trailing ORDER BY clause from a JQL string so it is
// accepted by /search/approximate-count.
func stripOrderBy(jql string) string {
	return jqlOrderByRE.ReplaceAllString(jql, "")
}

// initPollerMetrics touches every poller-metric series to its zero baseline so
// increase()/absent() have a sample the instant the pod is first scraped —
// without a baseline, increase() over a counter that only appears mid-window is
// subtly wrong, and absent() over a never-written series would mis-classify a
// just-started pod. Called once at the top of Run before the poll loop.
func initPollerMetrics() {
	jiraPollerPolls.WithLabelValues(pollResultOK).Add(0)
	jiraPollerPolls.WithLabelValues(pollResultSearchError).Add(0)
	jiraPollerPolls.WithLabelValues(pollResultNotCaughtUp).Add(0)
	jiraPollerStalled.Set(0)
	jiraPollerUpstreamMatches.Set(0)
	jiraPollerBeadsCreated.Add(0)
}

// recordPollOutcome accounts one completed (non-errored) ingestion search cycle
// and (re)computes the stall verdict. Called from poll after SearchIssues
// returns without error, with the already-built ingestion JQL and the number of
// rows it returned.
//
//   - rows > 0  → the search saw matching issues; the ingestion path attempts
//     creation for every untracked one, so this is definitionally not the
//     blind-stall case. Verdict cleared to 0.
//   - rows == 0 → confirm whether matter actually exists upstream via the cheap
//     approximate-count endpoint (ORDER BY stripped). count > 0 with a zero-row
//     search is the silent-stall signature → 1. count == 0 (genuinely empty
//     upstream) → 0. A count ERROR clears the verdict to 0 and logs: a single
//     transient count failure is not evidence of a stall, a real stall persists
//     to the next cycle, and firing on an unconfirmed guess would false-page.
//
// The approximate-count call happens only on the zero-row branch — rare (an
// empty ingestion result) and one cheap POST, comfortably inside the Jira
// rate-limit budget the JQL-count collector already respects.
func (p *JiraPoller) recordPollOutcome(ctx context.Context, ingestionJQL string, rows int) {
	jiraPollerPolls.WithLabelValues(pollResultOK).Inc()

	if rows > 0 {
		jiraPollerStalled.Set(0)
		return
	}

	countJQL := stripOrderBy(ingestionJQL)
	count, err := p.jira.ApproximateCount(ctx, countJQL)
	if err != nil {
		p.cfg.Logger.Warn("jira-poller stall check: approximate-count failed; not treating zero-row poll as a stall",
			"jql", countJQL, "error", err)
		jiraPollerStalled.Set(0)
		return
	}

	jiraPollerUpstreamMatches.Set(float64(count))
	if count > 0 {
		jiraPollerStalled.Set(1)
		p.cfg.Logger.Warn("jira-poller STALLED: ingestion search returned zero rows while approximate-count reports matching issues upstream — no new beads are being materialized (runbook: epic ka-0000005107)",
			"jql", countJQL, "approximate_count", count)
		return
	}
	jiraPollerStalled.Set(0)
}
