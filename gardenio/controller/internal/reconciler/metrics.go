package reconciler

import "github.com/prometheus/client_golang/prometheus"

const metricsNamespace = "gardenio"

// DeferredMetrics exposes Prometheus metrics about agents deferred during
// reconciliation (i.e., agents that have an open bead but no pod because
// the cluster is at capacity).
type DeferredMetrics struct {
	// Gauge: number of agents deferred this reconcile pass, by project.
	DeferredTotal *prometheus.GaugeVec

	// Histogram: time in seconds since bead creation for each deferred agent.
	DeferredWaitSeconds *prometheus.HistogramVec
}

// NewDeferredMetrics creates the deferred-agent Prometheus metrics.
func NewDeferredMetrics() *DeferredMetrics {
	return &DeferredMetrics{
		DeferredTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "agents_deferred_total",
			Help:      "Number of agents deferred (waiting for pod capacity) per reconcile pass.",
		}, []string{"project", "reason"}),

		DeferredWaitSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Name:      "agent_deferred_wait_seconds",
			Help:      "Time since bead creation for each deferred agent.",
			Buckets:   []float64{60, 300, 600, 1800, 3600, 7200, 14400},
		}, []string{"project"}),
	}
}

// Register registers the deferred metrics with the given registerer.
func (d *DeferredMetrics) Register(reg prometheus.Registerer) error {
	if err := reg.Register(d.DeferredTotal); err != nil {
		return err
	}
	return reg.Register(d.DeferredWaitSeconds)
}
