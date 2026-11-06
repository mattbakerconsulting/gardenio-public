package server

import (
	"github.com/prometheus/client_golang/prometheus"
)

// gardenio_authz_ownership_admits_total counts the grant-table row that
// admitted each ownership check (ownership_grants.go). Labels:
//   - grant: the row id; bounded by the code and machine rows plus
//     role:<title>/<n> for each role bead's rows (maxRoleGrantRows each).
//   - principal_kind: authzPrincipalKind, as on gardenio_authz_denials_total.
//   - action, bead_type: as on gardenio_authz_denials_total (bead_type through
//     authzBeadTypeLabel).
//
// A refusal is counted in gardenio_authz_denials_total. Exposed via /metrics on
// the internal-only metrics port.
var authzOwnershipAdmitsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_authz_ownership_admits_total",
		Help: "Ownership checks the grant table admits, by the admitting row (grant), principal_kind, action and bead_type (allowlisted; overflow collapses to \"custom\").",
	},
	[]string{"grant", "principal_kind", "action", "bead_type"},
)

// gardenio_authz_ownership_condition_errors_total counts a grant-table
// condition that failed closed on a store error.
var authzOwnershipConditionErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_authz_ownership_condition_errors_total",
		Help: "Grant-table conditions that failed closed on a store error, by condition.",
	},
	[]string{"condition"},
)

func recordOwnershipConditionError(condition string) {
	authzOwnershipConditionErrorsTotal.WithLabelValues(condition).Inc()
}
