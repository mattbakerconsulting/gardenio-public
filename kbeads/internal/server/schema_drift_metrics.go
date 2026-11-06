package server

import (
	"errors"
	"strings"

	"gardenio/kbeads/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

// gardenio_beads_writes_rejected_total counts bead create/update writes rejected
// at the per-type field-schema choke point (model.ValidateTypeFields /
// ValidateTypeFieldsForUpdate in beads.go). It is the RUNTIME half of the
// schema-drift defense (epic ka-0000002054, task ka-0000007249): the CI
// drift-guard catches a bridge/daemon field-registration mismatch at merge
// time; this counter surfaces the drift that snuck past CI *while it is
// happening in prod*, so a VM alert pages within a minute instead of an
// operator noticing five hours and 53 error beads later.
//
// The motivating outage (ka-0000003673, 2026-08-21): the bridge started
// writing `owner`/`resolved_by` on type=agent beads before the daemon's
// builtinConfigs declared them, so every trigger-engine spawn write 400'd
// "owner: unknown field". All trigger-spawned agents went dark for ~5h. The
// error beads existed; nothing alarmed on them. `reason="unknown_field"` is the
// precise fingerprint of that class — a field the writer sends that the daemon
// does not know — and the paired PrometheusRule
// (helm/gardenio/templates/beads/schema-drift-alert.yaml) pages on its rate.
//
// Labels:
//   - reason:    a bounded enum derived from the *model.ValidationError —
//     unknown_field / required_field / type_mismatch / malformed / other. A
//     rejection carrying several field errors is classified by its most
//     drift-relevant reason (see rejectionReasonPrecedence): unknown_field is
//     the schema-drift fingerprint and wins over the others.
//   - bead_type: the request bead type, capped to the same hard allowlist as
//     gardenio_authz_denials_total (authzBeadTypeLabel); any value outside the
//     allowlist collapses to "custom" so label cardinality cannot blow up from
//     user-defined types.
//
// Exposed via /metrics on the internal-only metrics port, registered alongside
// the other write-path counters in MetricsHandler.
var beadsWritesRejectedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_beads_writes_rejected_total",
		Help: "Number of bead create/update writes rejected at the per-type field-schema validation choke point, labelled by derived reason (unknown_field/required_field/type_mismatch/malformed/other) and request bead_type (allowlisted; overflow collapses to \"custom\").",
	},
	[]string{"reason", "bead_type"},
)

// Bounded reason enum for beadsWritesRejectedTotal. Each maps 1:1 to a shape of
// model.FieldError produced by validateFieldsImpl:
//   - unknown_field:  a key not in the type schema (the schema-drift signal).
//   - required_field: a declared-required field absent from the write.
//   - type_mismatch:  a present field whose value has the wrong JSON type.
//   - malformed:      the whole `fields` blob was not a JSON object.
//   - other:          a field-validation rejection that matched none of the
//     above message shapes (defensive; keeps the enum bounded).
const (
	rejectReasonUnknownField  = "unknown_field"
	rejectReasonRequiredField = "required_field"
	rejectReasonTypeMismatch  = "type_mismatch"
	rejectReasonMalformed     = "malformed"
	rejectReasonOther         = "other"
)

// rejectionReasonPrecedence orders the reasons from most to least
// drift-relevant. A single rejected write can carry multiple field errors (e.g.
// one unknown key AND one missing required field); classifyWrites picks the
// highest-precedence reason present so the schema-drift fingerprint
// (unknown_field) is never masked by a co-occurring required/type error.
var rejectionReasonPrecedence = []string{
	rejectReasonUnknownField,
	rejectReasonRequiredField,
	rejectReasonTypeMismatch,
	rejectReasonMalformed,
}

// classifyFieldRejection maps a field-validation error onto the bounded reason
// enum. It unwraps to a *model.ValidationError and inspects the field-error
// messages by their stable prefixes (the same message text validateFieldsImpl
// produces, and that existing callers/tests already key on). A non-validation
// error, or a ValidationError whose messages match no known shape, classifies
// as "other" so the counter still fires (a rejection happened) without
// inventing an unbounded label.
func classifyFieldRejection(err error) string {
	var ve *model.ValidationError
	if !errors.As(err, &ve) || ve == nil || len(ve.Errors) == 0 {
		return rejectReasonOther
	}
	present := make(map[string]bool, len(ve.Errors))
	for _, fe := range ve.Errors {
		present[fieldErrorReason(fe)] = true
	}
	for _, r := range rejectionReasonPrecedence {
		if present[r] {
			return r
		}
	}
	return rejectReasonOther
}

// fieldErrorReason classifies a single FieldError by its message shape. The
// prefixes mirror validateFieldsImpl exactly:
//   - unknownFieldMessage always begins "unknown field".
//   - a required field's message is exactly "is required".
//   - a whole-blob decode failure is Field=="fields", Message=="must be a JSON
//     object".
//   - every value-type mismatch begins "must be " (a string/integer/boolean/…),
//     optionally decorated by typeMismatchMessage.
func fieldErrorReason(fe model.FieldError) string {
	switch {
	case fe.Field == "fields" && fe.Message == "must be a JSON object":
		return rejectReasonMalformed
	case strings.HasPrefix(fe.Message, "unknown field"):
		return rejectReasonUnknownField
	case fe.Message == "is required":
		return rejectReasonRequiredField
	case strings.HasPrefix(fe.Message, "must be "):
		return rejectReasonTypeMismatch
	default:
		return rejectReasonOther
	}
}

// recordBeadWriteRejection increments gardenio_beads_writes_rejected_total for a
// field-schema rejection. Called from createBead/updateBead at the point the
// ValidateTypeFields[ForUpdate] error is turned into a 400, so the counter
// fires on exactly the writes the daemon refuses for a schema reason — and on
// every write path (gRPC, HTTP, escalation replay, formula pour) that funnels
// through those choke points.
func recordBeadWriteRejection(beadType string, err error) {
	if err == nil {
		return
	}
	beadsWritesRejectedTotal.WithLabelValues(
		classifyFieldRejection(err),
		authzBeadTypeLabel(beadType),
	).Inc()
}
