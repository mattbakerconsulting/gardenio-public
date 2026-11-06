package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Server-side idempotency_key dedup for bead creation (ka-0000002856).
//
// `kd create --idempotency-key <k>` and the proto/HTTP equivalents make a
// create lookup-or-return: when the caller supplies a key, the server returns
// any existing non-deleted bead in the same project that already carries that
// key, rather than inserting a duplicate (and without emitting a second
// BeadCreated event). The key is persisted in Fields[idempotency_key]; a
// per-fleet partial unique index (migration 029) backstops concurrent-insert
// races. The call sites live in createBead (internal/server/beads.go).

// idempotencyKeyField is the Fields[] key under which a create's
// idempotency_key is persisted. Declared in the shared auditFields block
// (internal/server/config.go) so every issue-kind and audit-bearing type
// accepts it through ValidateTypeFields.
const idempotencyKeyField = "idempotency_key"

// setField returns the fields JSON with key set to a string value, preserving
// any existing keys. A nil/empty/`null` input becomes a fresh object. Mirrors
// the map-roundtrip merge used by applyAutoInherit — key order is not part of
// the Fields contract. On a malformed (non-object) input the original bytes
// are returned unchanged so downstream validation surfaces the right error.
func setField(fields json.RawMessage, key, value string) json.RawMessage {
	m := make(map[string]any)
	trimmed := bytes.TrimSpace(fields)
	if len(trimmed) > 0 && string(trimmed) != "null" {
		if err := json.Unmarshal(trimmed, &m); err != nil || m == nil {
			return fields
		}
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		return fields
	}
	return out
}

// setFieldIfAbsent returns the fields JSON with key set to value ONLY when the
// key is missing or holds an empty string, preserving any existing keys and a
// non-empty explicit value. Like setField, a nil/empty/`null` input becomes a
// fresh object and a malformed (non-object) input is returned unchanged so
// downstream validation surfaces the right error. Used to default an
// optional-but-required type field (e.g. type=doc `format`) without masking an
// explicit caller value, which must still validate against the field's enum.
func setFieldIfAbsent(fields json.RawMessage, key, value string) json.RawMessage {
	m := make(map[string]any)
	trimmed := bytes.TrimSpace(fields)
	if len(trimmed) > 0 && string(trimmed) != "null" {
		if err := json.Unmarshal(trimmed, &m); err != nil || m == nil {
			return fields
		}
	}
	if !isMissingOrEmptyString(m, key) {
		return fields
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		return fields
	}
	return out
}

// lookupByIdempotencyKey returns the existing non-deleted bead carrying the
// given idempotency key within the same project, or nil if none exists.
//
// Uniqueness scope is per-PROJECT: the lookup is filtered by the new bead's
// project:<name> label so two different projects can reuse the same key
// without colliding. When the bead carries no project label the lookup is
// project-unscoped (matches any keyed bead in the caller's fleet) — this is
// the conservative choice for project-less beads, which the gardenio fleet
// effectively never creates. The fleet filter is implicit: ListBeads scopes
// to the caller's fleet unless FleetAll is requested. The migration-029
// partial unique index is per-fleet over fields->>idempotency_key and serves
// as the concurrent-insert race backstop on top of this lookup.
func (s *BeadsServer) lookupByIdempotencyKey(ctx context.Context, labels []string, fleet, key string) (*model.Bead, error) {
	filter := model.BeadFilter{
		Fields: map[string]string{idempotencyKeyField: key},
		Fleet:  fleet,
		Limit:  1,
	}
	if project := projectFromLabels(labels); project != "" && project != "unknown" {
		filter.Labels = append(filter.Labels, "project:"+project)
	}
	beads, _, err := s.store.ListBeads(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed to look up bead by idempotency key: %w", err)
	}
	if len(beads) > 0 {
		return beads[0], nil
	}
	return nil, nil
}

// isUniqueViolation reports whether err is a Postgres unique-violation
// (SQLSTATE 23505), surfaced through the store layer. Used by the
// idempotency-key create path to convert a lost insert race into a
// return-the-winner outcome rather than a 500. Mirrors the postgres-package
// helper of the same name; duplicated here because the server layer must not
// import the concrete store implementation.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pqe interface{ SQLState() string }
	if errors.As(err, &pqe) {
		return pqe.SQLState() == "23505"
	}
	return strings.Contains(err.Error(), "unique constraint")
}
