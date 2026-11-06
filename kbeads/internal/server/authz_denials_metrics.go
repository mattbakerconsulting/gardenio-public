package server

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// gardenio_authz_denials_total counts write-authorization rejections at the
// kbeads audit denial site (ka-0000006352). It increments on every write
// rejection that produces a 403 (in auditDenial, before writeAuthzDenial),
// alongside the best-effort audit bead.
//
// Labels:
//   - action:    the attempted write op (create/update/close/delete/hook-emit/...).
//   - reason:    a bounded enum derived from the *AuthzDenial — one of
//     insufficient_role / project_mismatch / escalation_required / denied.
//   - bead_type: the request bead type, capped to a hard allowlist; any value
//     outside the allowlist collapses to "custom" so the label cardinality
//     cannot blow up from user-defined types.
//   - principal_kind: what kind of caller was refused (authzPrincipalKind):
//     human / agent / fleet / runner / land / static / server / anonymous /
//     other, or "machine:<clientId>" for a registered machine principal.
//     Added after a ~12.6k/day loop of epic-update refusals could not be
//     tied to a caller from the metric alone (gd-Hk70sS1XuS).
//
// There is intentionally NO operator_id or agent-name label — per-operator
// and per-agent cardinality is unbounded and the caller is already captured
// in the audit bead.
//
// Exposed via /metrics on the internal-only metrics port.
var authzDenialsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_authz_denials_total",
		Help: "Number of write-authorization rejections, labelled by action, derived reason (insufficient_role/project_mismatch/escalation_required/denied), request bead_type (allowlisted; overflow collapses to \"custom\"), and principal_kind (the refused caller's kind; machine:<clientId> for a registered machine principal).",
	},
	[]string{"action", "reason", "bead_type", "principal_kind"},
)

// Bounded reason enum. AuthzDenial carries no reason field of its own, so the
// reason is DERIVED from the denial's structured fields (and, for the token
// project-scope branch, its message) at emit time.
const (
	reasonInsufficientRole   = "insufficient_role"
	reasonProjectMismatch    = "project_mismatch"
	reasonEscalationRequired = "escalation_required"
	reasonDenied             = "denied"

	// reasonTaint is the third read/write gate ADR docs/adr/taints.md
	// §5.1/§9.4/§10.1 adds: taintsOf(bead) ⊆ grantsOf(caller). Unlike the
	// other reasons, it fires from TWO call sites — recordAuthzDenial (write
	// path, via deriveAuthzDenialReason) AND recordTaintReadDenial (read
	// path, which has no *AuthzDenial to derive from at all).
	reasonTaint = "taint"
)

// msgTokenProjectMismatchFragment is the stable fragment of the token
// project-scope denial message (beads.go create/update). Those denials carry
// no RequiredRole/RequiredScope, so the reason is derived from the message.
// Kept as a shared constant so the construction site and the derivation cannot
// drift apart.
const msgTokenProjectMismatchFragment = "not authorized for this bead's project"

// deriveAuthzDenialReason maps an *AuthzDenial onto the bounded reason enum.
//
// Mapping (checked in order):
//   - TaintNames set     -> taint               (authorizeWriteTaintGate, §9.4)
//   - RequiredRole set   -> insufficient_role    (write_authz.go min_role branch)
//   - RequiredScope set  -> project_mismatch     (checkProjectScope branches)
//   - token-project msg  -> project_mismatch     (beads.go create/update branches)
//   - Escalatable        -> escalation_required  (ownership branch)
//   - otherwise          -> denied               (token type/op scope, fallthrough)
//
// TaintNames is checked first: authorizeWriteTaintGate runs only after
// authorizeWriteTitled/authorizeOwnership already passed (write_authz.go), so a
// taint denial never carries any of the other fields — but checking it first
// keeps that invariant from being load-bearing here too.
func deriveAuthzDenialReason(d *AuthzDenial) string {
	if d == nil {
		return reasonDenied
	}
	switch {
	case len(d.TaintNames) > 0:
		return reasonTaint
	case d.RequiredRole != "":
		return reasonInsufficientRole
	case d.RequiredScope != "":
		return reasonProjectMismatch
	case strings.Contains(d.Message, msgTokenProjectMismatchFragment):
		return reasonProjectMismatch
	case d.Escalatable:
		return reasonEscalationRequired
	default:
		return reasonDenied
	}
}

// authzBeadTypeAllowlist is the HARD allowlist of bead types permitted as the
// bead_type label. Anything outside this set collapses to "custom" to bound
// label cardinality (bead types are user-extensible). Covers the well-known
// model types plus the system/service types that show up on write paths.
var authzBeadTypeAllowlist = map[string]struct{}{
	"epic":       {},
	"task":       {},
	"feature":    {},
	"chore":      {},
	"bug":        {},
	"advice":     {},
	"jack":       {},
	"decision":   {},
	"escalation": {},
	"report":     {},
	"formula":    {},
	"fleet":      {},
	"project":    {},
	"account":    {},
	"molecule":   {},
	"agent":      {},
	"audit":      {},
	"schedule":   {},
	"trigger":    {},
	"gate":       {},
}

// authzBeadTypeLabel returns the bead type if it is on the hard allowlist, or
// "custom" otherwise. An empty type also maps to "custom".
func authzBeadTypeLabel(beadType string) string {
	if _, ok := authzBeadTypeAllowlist[beadType]; ok {
		return beadType
	}
	return "custom"
}

// principalKindOther is the principal_kind of a caller none of the known
// identity types describe.
const principalKindOther = "other"

// authzPrincipalKinds maps an AuthIdentity.Type onto the principal_kind
// label. A per-agent token is a fleet identity too, so "agent" is told apart
// by its fleet (authzPrincipalKind); "edge" is the viewer's HMAC envelope for
// a signed-in person and "window" a person's window principal.
var authzPrincipalKinds = map[string]string{
	"oidc":                     "human",
	"edge":                     "human",
	"window":                   "human",
	"fleet":                    "fleet",
	runnerIdentityType:         "runner",
	runnerRegisterIdentityType: "runner",
	"land":                     "land",
	"static":                   "static",
	auditRecorderIdentityType:  "server",
	landAgentIdentityType:      "server",
	"anonymous":                "anonymous",
	"none":                     "anonymous",
	"":                         "anonymous",
}

// authzPrincipalKind returns the bounded principal_kind label for the caller
// on ctx. A machine principal is named by its client ("machine:<clientId>",
// its audit subject): only clients registered with the daemon authenticate as
// machines, so the set is bounded by that registry and the label says which
// component is looping. Everything else collapses to its kind.
func authzPrincipalKind(ctx context.Context) string {
	id := AuthIdentityFromContext(ctx)
	if id.Type == "machine" {
		if strings.HasPrefix(id.Subject, machineSubjectPrefix) && len(id.Subject) > len(machineSubjectPrefix) {
			return id.Subject
		}
		return "machine"
	}
	if id.Type == "fleet" && isAgentFleet(FleetFromContext(ctx)) {
		return "agent"
	}
	if kind, ok := authzPrincipalKinds[id.Type]; ok {
		return kind
	}
	return principalKindOther
}

// recordAuthzDenial increments gardenio_authz_denials_total for a denial. It is
// invoked from auditDenial so the counter fires on every write rejection
// before the 403 is written, with the audit bead recorded alongside.
func recordAuthzDenial(ctx context.Context, d *AuthzDenial) {
	if d == nil {
		return
	}
	authzDenialsTotal.WithLabelValues(
		d.Action,
		deriveAuthzDenialReason(d),
		authzBeadTypeLabel(d.BeadType),
		authzPrincipalKind(ctx),
	).Inc()
}

// recordTaintReadDenial increments gardenio_authz_denials_total for a
// taint-gated READ denial (ADR docs/adr/taints.md §5.3/§10.1) — the read-path
// counterpart to recordAuthzDenial, which only ever fires from a write-path
// *AuthzDenial via auditDenial. The read path has no such struct (a taint
// denial there is a plain 404, not a structured body), so it records
// directly instead of going through deriveAuthzDenialReason.
func recordTaintReadDenial(ctx context.Context, action, beadType string) {
	authzDenialsTotal.WithLabelValues(action, reasonTaint, authzBeadTypeLabel(beadType), authzPrincipalKind(ctx)).Inc()
}
