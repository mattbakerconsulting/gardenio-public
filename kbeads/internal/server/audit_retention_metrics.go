package server

import "github.com/prometheus/client_golang/prometheus"

// gardenio_audit_retention_closed_total and gardenio_audit_retention_deleted_total
// count what the audit-retention sweep (audit_retention.go) retires: open
// type=audit beads it closes past the retention window, and closed ones it
// hard-deletes past retention plus auditRetentionDeleteGrace (gd-GJzCfUmbQC).
// Read against gardenio_audit_writes_total to see whether retention keeps up
// with the write rate. No labels: every increment is the same system sweep.
// Registered on /metrics in MetricsHandler (http_metrics.go).
var (
	auditRetentionClosedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gardenio_audit_retention_closed_total",
		Help: "Number of open audit beads the audit-retention sweep closed past the retention window.",
	})
	auditRetentionDeletedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gardenio_audit_retention_deleted_total",
		Help: "Number of closed audit beads the audit-retention sweep hard-deleted past retention plus the 30-day delete grace.",
	})
)
