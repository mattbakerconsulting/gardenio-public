package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// StoreMethodOther is the method label for a query issued from outside the
// postgres store package (no store frame on the stack).
const StoreMethodOther = "other"

var (
	// StoreQueryDuration is the wall time of one SQL statement, labelled by
	// the store method that issued it (ListBeads, GetLabelsForBeads, ...). A
	// query is timed from the driver call until its rows are closed, so a
	// large result set's streaming time counts. pg_stat_statements groups by
	// normalized SQL text; this is the per-call-site view (gd-rXqkv4rSmG).
	StoreQueryDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gardenio_beads_store_query_duration_seconds",
			Help:    "Wall time of one SQL statement issued by the postgres store, by store method.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"method"},
	)

	// StoreRowsReturned is the number of rows one SELECT (or RETURNING)
	// statement handed back to the store, by store method. Exec statements
	// are not observed here.
	StoreRowsReturned = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gardenio_beads_store_rows_returned",
			Help:    "Rows returned by one SQL query issued by the postgres store, by store method.",
			Buckets: []float64{0, 1, 10, 100, 1000, 10000, 100000},
		},
		[]string{"method"},
	)
)

// RegisterStore registers the store query metrics on the given registry.
// Safe to call once per registry.
func RegisterStore(reg prometheus.Registerer) {
	reg.MustRegister(StoreQueryDuration, StoreRowsReturned)
}

// ObserveStoreQuery records one statement's wall time under method.
func ObserveStoreQuery(method string, d time.Duration) {
	StoreQueryDuration.WithLabelValues(method).Observe(d.Seconds())
}

// ObserveStoreRows records how many rows one query returned under method.
func ObserveStoreRows(method string, n int) {
	StoreRowsReturned.WithLabelValues(method).Observe(float64(n))
}
