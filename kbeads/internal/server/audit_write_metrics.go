package server

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// gardenio_audit_writes_total and gardenio_audit_write_failures_total count the
// outcome of every recordAuditEntry write (gd-jZuL63txCs). The audit bead is
// best-effort, so a failed write is only a slog.Warn; without these counters a
// refusal can go unnoticed for days. Nor can an agent check the store instead:
// audit beads carry no project label (auditRecorderContext), so a project-bound
// token never sees them, and `kd count --type audit` reads 0 from any agent
// whether the writes land or not. Compare the two with
// gardenio_authz_denials_total, which every denial increments first.
var auditWritesTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "gardenio_audit_writes_total",
	Help: "Number of authz audit beads written by recordAuditEntry.",
})

var auditWriteFailuresTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_audit_write_failures_total",
		Help: "Number of authz audit bead writes that failed, labelled by reason (invalid/denied/operator/marshal/store).",
	},
	[]string{"reason"},
)

// Bounded reason enum for gardenio_audit_write_failures_total.
const (
	auditWriteFailInvalid  = "invalid"  // inputError: schema or validation refusal (e.g. an action outside the type:audit enum)
	auditWriteFailDenied   = "denied"   // *AuthzDenial from a createBead gate
	auditWriteFailOperator = "operator" // resolveAuditOperatorRequired failed closed
	auditWriteFailMarshal  = "marshal"  // the fields map did not marshal
	auditWriteFailStore    = "store"    // anything else: config lookup, insert, transaction
)

// auditWriteFailureReason maps a createBead error onto the bounded reason enum.
func auditWriteFailureReason(err error) string {
	var ie inputError
	var denial *AuthzDenial
	var refusal *operatorRefusalError
	switch {
	case errors.As(err, &ie):
		return auditWriteFailInvalid
	case errors.As(err, &denial):
		return auditWriteFailDenied
	case errors.As(err, &refusal):
		return auditWriteFailOperator
	default:
		return auditWriteFailStore
	}
}
