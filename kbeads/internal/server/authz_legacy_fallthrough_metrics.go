package server

import (
	"github.com/prometheus/client_golang/prometheus"
)

// gardenio_authz_legacy_fallthrough_total counts write-authorization checks
// that hit the legacy ownership fall-through in authorizeOwnership — the
// permissive branch that skips the ownership gate when a bead carries NO
// provenance at all (CreatedBy, Assignee, and Owner are ALL empty). See
// write_authz.go and ka-0000000671.
//
// This is a write-TIME hit counter, not a population count: it increments
// once per update/close/delete that actually reaches the fall-through, so it
// measures how often the escape hatch is genuinely exercised — which is the
// number that matters for deciding whether the branch can be tightened
// (admin-only), scoped to an allowlist, or removed entirely. A large dormant
// population of pre-gate legacy rows that are never written again produces
// zero hits here.
//
// Labels:
//   - action:    the attempted write op (create/update/close/delete/...).
//   - bead_type: the bead's type, capped to the same hard allowlist as
//     gardenio_authz_denials_total (authzBeadTypeLabel); any value outside the
//     allowlist collapses to "custom" so label cardinality cannot blow up
//     from user-defined types.
//
// There is intentionally NO per-bead or per-operator label — that cardinality
// is unbounded. If a non-zero rate shows up and the specific rows need
// identifying, query mutation_history / the audit trail for the exact IDs.
//
// Exposed via /metrics on the internal-only metrics port.
var authzLegacyFallthroughTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_authz_legacy_fallthrough_total",
		Help: "Number of write-authorization checks that hit the permissive legacy ownership fall-through (bead has no CreatedBy/Assignee/Owner), labelled by action and request bead_type (allowlisted; overflow collapses to \"custom\").",
	},
	[]string{"action", "bead_type"},
)

// recordAuthzLegacyFallthrough increments the legacy fall-through counter for
// a write that reached the permissive branch. Invoked from authorizeOwnership
// at the fall-through site, before it returns nil (allow).
func recordAuthzLegacyFallthrough(action, beadType string) {
	authzLegacyFallthroughTotal.WithLabelValues(
		action,
		authzBeadTypeLabel(beadType),
	).Inc()
}
