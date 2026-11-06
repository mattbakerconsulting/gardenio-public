package store

import (
	"errors"
	"regexp"
)

// ErrInvalidFieldKey is returned when a caller passes a field name that
// cannot be safely interpolated into a jsonb path expression.
var ErrInvalidFieldKey = errors.New("invalid field key")

// ErrCASConflict is returned by CASBeadArrayField when the bead's current
// array value does not equal the caller's `expected` snapshot — a concurrent
// writer changed the array in the read→write window. The caller must re-read
// the bead, re-derive its intended array against the fresh value, and retry.
// This is the correctness rail that makes an append (one writer) and a reorder
// (another writer) safe against each other: the reorder's blind full-array
// rewrite can no longer clobber the append (ka-0000000988).
var ErrCASConflict = errors.New("array field compare-and-swap conflict: bead was modified concurrently")

// ErrInvalidSubFieldEdit is returned by the sub-field edit primitives
// (MergeBeadMapField / EditBeadCSVField) when the requested edit is malformed:
// an unknown op, an empty map key, an empty CSV element, a CSV element
// containing a comma (which would corrupt the CSV), or a stored field value
// whose shape is not a JSON map / CSV string. It maps to 400 at the HTTP layer.
var ErrInvalidSubFieldEdit = errors.New("invalid sub-field edit")

var fieldKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidFieldKey reports whether key is safe to use as a bead jsonb field
// name in IncrementBeadFields. The grammar is intentionally conservative:
// anything outside identifier characters is rejected so the key can be
// safely interpolated into a Postgres jsonb path literal.
func ValidFieldKey(key string) bool {
	return fieldKeyRe.MatchString(key)
}
