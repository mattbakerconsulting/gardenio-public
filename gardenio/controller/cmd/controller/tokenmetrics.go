// Agent-token lifecycle telemetry.
//
// These Prometheus metrics observe the per-agent token lifecycle described in
// gardenio/docs/research/per-agent-tokens-lifecycle.md (T4 ADR) and
// gardenio/docs/research/c1-token-ttl-refresh-design.md. They back the
// revocation SLO ("95% of revokes within 5 minutes, 99% within 60 minutes",
// customer-1-telemetry-plan.md §1.3/§5) and let operators see provision
// failures and orphan-sweep lag.
//
// Privacy invariant (HARD CONSTRAINT, telemetry-plan §5): the raw token bytes
// and the provider-side token id are NEVER used as label values. Labels are
// limited to the provider name and a small, bounded set of status /
// revocation-type enums so the metric cardinality stays flat and no secret
// material can leak through a label.
package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// namespaceTokenMetrics is the Prometheus namespace for the token metrics. It
// matches the controller's existing "gardenio" namespace so the emitted series
// are gardenio_agent_token_*.
const namespaceTokenMetrics = "gardenio"

// Provider label values. Today the controller only issues the beads-daemon
// per-agent token; the GitLab/GitHub providers in the ADR are future work
// (T6) and will reuse the same metrics with provider="gitlab"/"github".
// tokenProviderGithub (github_app_credentials.go) is the GitHub App provider.
const tokenProviderBeads = "beads"

// Provision status label values (gardenio_agent_token_provisions_total).
const (
	tokenProvisionSuccess = "success"
	tokenProvisionError   = "error"
)

// Revocation-type label values (gardenio_agent_token_revocations_total and the
// latency histogram). These map onto the ADR §6.1 trigger table:
//   - despawn:       synchronous revoke on AgentDone / AgentKill / AgentStop.
//   - stuck_restart: revoke-before-reissue on AgentStuck.
//   - async_janitor: the orphan-sweep reconciler (ADR §6.3).
const (
	revocationTypeDespawn      = "despawn"
	revocationTypeStuckRestart = "stuck_restart"
	revocationTypeAsyncJanitor = "async_janitor"
)

// tokenSLOBuckets are the revocation-latency histogram buckets, in seconds:
// 5s, 30s, 5m, 15m, 1h, 4h. The 5m (300) and 1h (3600) boundaries are chosen
// so the approved SLO query reads directly off bucket counts:
//
//	sum(rate(gardenio_agent_token_revocation_latency_seconds_bucket{le="300"}[1h]))
//	  / sum(rate(gardenio_agent_token_revocation_latency_seconds_count[1h]))   # 95%@5m
//	sum(rate(gardenio_agent_token_revocation_latency_seconds_bucket{le="3600"}[1h]))
//	  / sum(rate(gardenio_agent_token_revocation_latency_seconds_count[1h]))   # 99%@60m
var tokenSLOBuckets = []float64{5, 30, 300, 900, 3600, 14400}

// tokenMetrics holds the agent-token lifecycle counters and the revocation
// latency histogram. A nil *tokenMetrics is safe to call: every record method
// is a no-op on a nil receiver, so token issuance/revocation code paths work
// unchanged when metrics are not wired up (e.g. in unit tests).
type tokenMetrics struct {
	provisions        *prometheus.CounterVec   // labels: provider, status
	revocations       *prometheus.CounterVec   // labels: provider, revocation_type
	revocationLatency *prometheus.HistogramVec // labels: provider, revocation_type
}

// newTokenMetrics constructs the agent-token lifecycle metrics. Call Register
// to expose them on a registry.
func newTokenMetrics() *tokenMetrics {
	return &tokenMetrics{
		provisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespaceTokenMetrics,
			Name:      "agent_token_provisions_total",
			Help:      "Per-agent token issuance attempts by provider and outcome (success|error).",
		}, []string{"provider", "status"}),

		revocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespaceTokenMetrics,
			Name:      "agent_token_revocations_total",
			Help:      "Per-agent token revocations by provider and revocation type (despawn|stuck_restart|async_janitor).",
		}, []string{"provider", "revocation_type"}),

		revocationLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespaceTokenMetrics,
			Name:      "agent_token_revocation_latency_seconds",
			Help:      "Seconds from agent-done to revocation completion, by provider and revocation type. Buckets back the 95%@5m / 99%@60m revoke SLO.",
			Buckets:   tokenSLOBuckets,
		}, []string{"provider", "revocation_type"}),
	}
}

// Register registers the token metrics with the given registerer.
func (m *tokenMetrics) Register(reg prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{m.provisions, m.revocations, m.revocationLatency} {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// recordProvision increments the provision counter for the given provider and
// outcome. No-op on a nil receiver.
func (m *tokenMetrics) recordProvision(provider, status string) {
	if m == nil {
		return
	}
	m.provisions.WithLabelValues(provider, status).Inc()
}

// recordRevocation increments the revocation counter and, when doneAt is set,
// observes the revocation latency (now - doneAt) in the histogram. doneAt is
// the AgentDone reference point: the despawn-event receipt time for the
// synchronous path, or the bead's closed_at for the async janitor path. A zero
// doneAt records the count but skips the latency observation (the reference
// point was unavailable). No-op on a nil receiver.
func (m *tokenMetrics) recordRevocation(provider, revType string, doneAt time.Time) {
	if m == nil {
		return
	}
	m.revocations.WithLabelValues(provider, revType).Inc()
	if doneAt.IsZero() {
		return
	}
	latency := time.Since(doneAt).Seconds()
	if latency < 0 {
		latency = 0
	}
	m.revocationLatency.WithLabelValues(provider, revType).Observe(latency)
}
