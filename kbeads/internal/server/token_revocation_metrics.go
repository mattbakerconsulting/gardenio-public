package server

import "github.com/prometheus/client_golang/prometheus"

// Prometheus metrics for the auth-token revocation blocklist. Registered with
// the metrics handler's registry in MetricsHandler. Kept as package-level
// vars so admin-endpoint code can Inc them without threading a collector
// through server state.
var (
	tokenRevocationsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gardenio_beads_token_revocations_total",
		Help: "Number of auth-token revocations persisted since process start (deduplicated — only newly-inserted hashes increment).",
	})

	revokedTokensCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gardenio_beads_revoked_tokens",
		Help: "Current number of revoked auth-token hashes in the in-process cache (mirrors revoked_tokens table plus any env-var seed).",
	})
)
