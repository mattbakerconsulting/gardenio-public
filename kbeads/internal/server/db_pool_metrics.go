package server

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
)

// dbStatsSource is the narrow optional interface a store implements to expose
// its *sql.DB connection-pool statistics. PostgresStore satisfies it via
// PostgresStore.DBStats; test/mock stores that do not implement it simply get
// no pool metrics registered (see registerDBPoolMetrics).
type dbStatsSource interface {
	DBStats() sql.DBStats
}

// dbPoolCollector exposes the live database/sql connection-pool statistics as
// Prometheus gauges. It is the verification instrument for bug ka-0000004428:
// the bimodal ~4.08s latency tail was cold-dial churn caused by the pool
// discarding warm connections above a too-low MaxIdleConns. The cumulative
// max_idle_closed counter is the direct, incrementing-on-fault signal for that
// churn — a working (warm) pool keeps it flat, a churning pool grows it fast —
// so it doubles as the field that proves the warm-pool fix landed in prod
// (advice ka-0000002586: verify with a field that increments on the fault).
//
// Collect() reads sql.DB.Stats(), an in-memory snapshot with no DB round-trip,
// so it never adds DB latency to a /metrics scrape.
type dbPoolCollector struct {
	stats func() sql.DBStats

	maxOpen           *prometheus.Desc
	open              *prometheus.Desc
	inUse             *prometheus.Desc
	idle              *prometheus.Desc
	waitCount         *prometheus.Desc
	waitDuration      *prometheus.Desc
	maxIdleClosed     *prometheus.Desc
	maxIdleTimeClosed *prometheus.Desc
	maxLifetimeClosed *prometheus.Desc
}

func newDBPoolCollector(stats func() sql.DBStats) *dbPoolCollector {
	return &dbPoolCollector{
		stats: stats,
		maxOpen: prometheus.NewDesc(
			"kbeads_db_pool_max_open_connections",
			"Maximum number of open connections to the database (MaxOpenConns).",
			nil, nil),
		open: prometheus.NewDesc(
			"kbeads_db_pool_open_connections",
			"Number of established connections both in use and idle.",
			nil, nil),
		inUse: prometheus.NewDesc(
			"kbeads_db_pool_in_use_connections",
			"Number of connections currently in use.",
			nil, nil),
		idle: prometheus.NewDesc(
			"kbeads_db_pool_idle_connections",
			"Number of idle (parked, reusable) connections.",
			nil, nil),
		waitCount: prometheus.NewDesc(
			"kbeads_db_pool_wait_count_total",
			"Cumulative number of connection acquisitions that had to wait for a "+
				"free connection (pool saturated at MaxOpenConns). A rising rate is "+
				"pool exhaustion — the 503-under-load half of bug ka-0000004428.",
			nil, nil),
		waitDuration: prometheus.NewDesc(
			"kbeads_db_pool_wait_duration_seconds_total",
			"Cumulative time blocked waiting for a connection, in seconds.",
			nil, nil),
		maxIdleClosed: prometheus.NewDesc(
			"kbeads_db_pool_max_idle_closed_total",
			"Cumulative connections closed because the idle count exceeded "+
				"MaxIdleConns. This is the cold-dial CHURN signal for bug "+
				"ka-0000004428: a warm pool keeps it flat, a churning pool grows it.",
			nil, nil),
		maxIdleTimeClosed: prometheus.NewDesc(
			"kbeads_db_pool_max_idle_time_closed_total",
			"Cumulative connections closed because they exceeded ConnMaxIdleTime.",
			nil, nil),
		maxLifetimeClosed: prometheus.NewDesc(
			"kbeads_db_pool_max_lifetime_closed_total",
			"Cumulative connections closed because they exceeded ConnMaxLifetime "+
				"(the 5m rotation-recycle window).",
			nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *dbPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.maxOpen
	ch <- c.open
	ch <- c.inUse
	ch <- c.idle
	ch <- c.waitCount
	ch <- c.waitDuration
	ch <- c.maxIdleClosed
	ch <- c.maxIdleTimeClosed
	ch <- c.maxLifetimeClosed
}

// Collect implements prometheus.Collector.
func (c *dbPoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.stats()
	ch <- prometheus.MustNewConstMetric(c.maxOpen, prometheus.GaugeValue, float64(s.MaxOpenConnections))
	ch <- prometheus.MustNewConstMetric(c.open, prometheus.GaugeValue, float64(s.OpenConnections))
	ch <- prometheus.MustNewConstMetric(c.inUse, prometheus.GaugeValue, float64(s.InUse))
	ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(s.Idle))
	ch <- prometheus.MustNewConstMetric(c.waitCount, prometheus.CounterValue, float64(s.WaitCount))
	ch <- prometheus.MustNewConstMetric(c.waitDuration, prometheus.CounterValue, s.WaitDuration.Seconds())
	ch <- prometheus.MustNewConstMetric(c.maxIdleClosed, prometheus.CounterValue, float64(s.MaxIdleClosed))
	ch <- prometheus.MustNewConstMetric(c.maxIdleTimeClosed, prometheus.CounterValue, float64(s.MaxIdleTimeClosed))
	ch <- prometheus.MustNewConstMetric(c.maxLifetimeClosed, prometheus.CounterValue, float64(s.MaxLifetimeClosed))
}

// registerDBPoolMetrics registers the connection-pool collector on reg when the
// store exposes its pool stats (i.e. is a real PostgresStore). Stores that do
// not implement dbStatsSource — the sqlmock-backed test stores — register
// nothing, so /metrics stays valid without a live pool.
func registerDBPoolMetrics(reg prometheus.Registerer, s interface{}) {
	src, ok := s.(dbStatsSource)
	if !ok {
		return
	}
	reg.MustRegister(newDBPoolCollector(src.DBStats))
}
