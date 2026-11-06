package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// This file implements the non-clobbering sub-field edit primitives for SHARED
// AGGREGATE bead fields — a JSON map (channel_roles) and a CSV string
// (slack_channel). A bare `kd update -f key=value` (and the PATCH it drives)
// replaces the ENTIRE field value, so for these aggregates any writer silently
// clobbers every OTHER agent/operator entry another writer put there
// (ka-0000007767). These primitives edit ONE key / ONE element atomically.
//
// Why a Go read-modify-write and not the pure-SQL jsonb_set of the array-field
// siblings (array_fields.go): those siblings assume the field is a NATIVE jsonb
// array. These aggregates have a messier real storage — slack_channel is a
// jsonb STRING holding CSV, and channel_roles is EITHER a native jsonb object
// OR a stringified-JSON jsonb string (some writers stored it via `-f
// channel_roles='{...}'` → native object, others as a quoted JSON string, some
// with a trailing newline). SQL cannot cleanly branch on both shapes, so we
// decode in Go. Atomicity against concurrent gardeners is preserved by running
// the read and the write in ONE explicit transaction with `SELECT … FOR
// UPDATE`, which serializes concurrent sub-field writers on the row lock — so
// two writes to DIFFERENT keys can never lose an update (the acceptance
// criterion). The `FOR UPDATE` lock only spans read+write when both statements
// share a transaction, which is why the PostgresStore entry points below open
// an explicit tx rather than handing the query an autocommitting *sql.DB.

// Sub-field edit ops. The op selects the aggregate shape AND the mutation:
// set/unset-key operate on a JSON MAP field; append/remove on a CSV field.
const (
	SubFieldOpSet    = "set"       // map: fields[field][key] = value
	SubFieldOpUnset  = "unset-key" // map: delete fields[field][key]
	SubFieldOpAppend = "append"    // csv: add element if absent
	SubFieldOpRemove = "remove"    // csv: drop all occurrences of element
)

// MergeBeadMapField atomically sets or deletes ONE key of the JSON-map field
// `field` without disturbing the field's other keys or the bead's other fields.
// See the file header for the atomicity contract. op is SubFieldOpSet or
// SubFieldOpUnset. The write is normalised to a native jsonb object, which both
// read paths in the controller (ParseFieldsJSON + unmarshalProjectField) accept.
// Returns (bead, false) on an idempotent no-op (set to the same value, or unset
// of an absent key — no updated_at bump), sql.ErrNoRows when the bead does not
// exist, and ErrInvalidFieldKey when `field` is not a safe identifier.
func (s *PostgresStore) MergeBeadMapField(ctx context.Context, id, field, op, key, value string) (*model.Bead, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit
	bead, changed, err := queryMergeBeadMapField(ctx, tx, id, field, op, key, value)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return bead, changed, nil
}

func (s *txStore) MergeBeadMapField(ctx context.Context, id, field, op, key, value string) (*model.Bead, bool, error) {
	return queryMergeBeadMapField(ctx, s.tx, id, field, op, key, value)
}

// queryMergeBeadMapField performs the map-field read-modify-write. db MUST be a
// live transaction (see the file header) so the `FOR UPDATE` lock spans the
// read and the write.
func queryMergeBeadMapField(ctx context.Context, db executor, id, field, op, key, value string) (*model.Bead, bool, error) {
	if !store.ValidFieldKey(field) {
		return nil, false, fmt.Errorf("%w: %q", store.ErrInvalidFieldKey, field)
	}
	if key == "" {
		return nil, false, fmt.Errorf("%w: map key must not be empty", store.ErrInvalidSubFieldEdit)
	}

	fields, err := lockAndReadFields(ctx, db, id)
	if err != nil {
		return nil, false, err
	}

	m, err := decodeStringMapField(fields[field])
	if err != nil {
		return nil, false, fmt.Errorf("%w: field %q: %v", store.ErrInvalidSubFieldEdit, field, err)
	}

	switch op {
	case SubFieldOpSet:
		if cur, ok := m[key]; ok && cur == value {
			return scanBeadNoOp(ctx, db, id) // already at target value → no-op
		}
		m[key] = value
	case SubFieldOpUnset:
		if _, ok := m[key]; !ok {
			return scanBeadNoOp(ctx, db, id) // key absent → no-op
		}
		delete(m, key)
	default:
		return nil, false, fmt.Errorf("%w: map op %q (want %q or %q)", store.ErrInvalidSubFieldEdit, op, SubFieldOpSet, SubFieldOpUnset)
	}

	encoded, err := json.Marshal(m)
	if err != nil {
		return nil, false, fmt.Errorf("encoding merged map field: %w", err)
	}
	fields[field] = encoded
	bead, err := writeFields(ctx, db, id, fields)
	if err != nil {
		return nil, false, err
	}
	return bead, true, nil
}

// EditBeadCSVField atomically appends or removes ONE element of the CSV string
// field `field` (e.g. slack_channel) without disturbing the field's other
// elements or the bead's other fields. See the file header for the atomicity
// contract. op is SubFieldOpAppend (add-if-absent, idempotent) or
// SubFieldOpRemove (drop every occurrence). The write is stored as a jsonb
// string holding the comma-joined elements, matching how the controller's
// parseSlackChannels reads it. Returns (bead, false) on an idempotent no-op,
// sql.ErrNoRows when the bead does not exist, ErrInvalidFieldKey when `field` is
// not a safe identifier, and ErrInvalidSubFieldEdit when the element is empty or
// contains a comma (which would corrupt the CSV).
func (s *PostgresStore) EditBeadCSVField(ctx context.Context, id, field, op, element string) (*model.Bead, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit
	bead, changed, err := queryEditBeadCSVField(ctx, tx, id, field, op, element)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return bead, changed, nil
}

func (s *txStore) EditBeadCSVField(ctx context.Context, id, field, op, element string) (*model.Bead, bool, error) {
	return queryEditBeadCSVField(ctx, s.tx, id, field, op, element)
}

// queryEditBeadCSVField performs the CSV-field read-modify-write. db MUST be a
// live transaction (see the file header).
func queryEditBeadCSVField(ctx context.Context, db executor, id, field, op, element string) (*model.Bead, bool, error) {
	if !store.ValidFieldKey(field) {
		return nil, false, fmt.Errorf("%w: %q", store.ErrInvalidFieldKey, field)
	}
	element = strings.TrimSpace(element)
	if element == "" {
		return nil, false, fmt.Errorf("%w: CSV element must not be empty", store.ErrInvalidSubFieldEdit)
	}
	if strings.Contains(element, ",") {
		return nil, false, fmt.Errorf("%w: CSV element %q must not contain a comma", store.ErrInvalidSubFieldEdit, element)
	}

	fields, err := lockAndReadFields(ctx, db, id)
	if err != nil {
		return nil, false, err
	}

	parts, err := decodeCSVField(fields[field])
	if err != nil {
		return nil, false, fmt.Errorf("%w: field %q: %v", store.ErrInvalidSubFieldEdit, field, err)
	}

	var next []string
	switch op {
	case SubFieldOpAppend:
		if slices.Contains(parts, element) {
			return scanBeadNoOp(ctx, db, id) // already present → no-op
		}
		next = append(parts, element)
	case SubFieldOpRemove:
		next = make([]string, 0, len(parts))
		for _, p := range parts {
			if p != element {
				next = append(next, p)
			}
		}
		if len(next) == len(parts) {
			return scanBeadNoOp(ctx, db, id) // element absent → no-op
		}
	default:
		return nil, false, fmt.Errorf("%w: csv op %q (want %q or %q)", store.ErrInvalidSubFieldEdit, op, SubFieldOpAppend, SubFieldOpRemove)
	}

	encoded, err := json.Marshal(strings.Join(next, ","))
	if err != nil {
		return nil, false, fmt.Errorf("encoding CSV field: %w", err)
	}
	fields[field] = encoded
	bead, err := writeFields(ctx, db, id, fields)
	if err != nil {
		return nil, false, err
	}
	return bead, true, nil
}

// lockAndReadFields reads a live (non-soft-deleted) bead's whole fields object
// under a row lock (`FOR UPDATE`), so a concurrent sub-field writer on the same
// row blocks until this transaction commits — no lost update. Returns the
// decoded top-level field map (each value kept as raw JSON so untouched keys
// round-trip byte-for-byte), or sql.ErrNoRows when the bead is absent.
func lockAndReadFields(ctx context.Context, db executor, id string) (map[string]json.RawMessage, error) {
	var raw []byte
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(fields, '{}'::jsonb) FROM beads WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`,
		id,
	).Scan(&raw)
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("decoding bead fields: %w", err)
		}
	}
	return fields, nil
}

// writeFields writes the (already-mutated) top-level field map back and returns
// the updated bead. Runs inside the same transaction as lockAndReadFields so the
// read→write pair is atomic.
func writeFields(ctx context.Context, db executor, id string, fields map[string]json.RawMessage) (*model.Bead, error) {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encoding bead fields: %w", err)
	}
	row := db.QueryRowContext(ctx, `
		UPDATE beads SET fields = $2::jsonb, updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING `+beadColumns,
		id, string(encoded),
	)
	return scanBead(row)
}

// scanBeadNoOp reads a single live bead by base columns for the no-op return
// path (the edit changed nothing, so there is no UPDATE to RETURNING-scan) and
// reports changed=false. It reads under the same locked transaction, so the
// returned snapshot is consistent with the (skipped) write.
func scanBeadNoOp(ctx context.Context, db executor, id string) (*model.Bead, bool, error) {
	bead, err := queryGetBeadBaseColumns(ctx, db, id)
	if err != nil {
		return nil, false, err
	}
	return bead, false, nil
}

// decodeStringMapField decodes a bead field that holds a string→string map,
// tolerating BOTH real storage shapes: a native jsonb object, and a jsonb
// STRING whose contents are JSON (optionally with surrounding whitespace/
// newline). An absent/empty field decodes to an empty map. See the file header
// for why both shapes exist for channel_roles.
func decodeStringMapField(raw json.RawMessage) (map[string]string, error) {
	m := map[string]string{}
	if len(raw) == 0 {
		return m, nil
	}
	// Shape 1: native jsonb object.
	if err := json.Unmarshal(raw, &m); err == nil {
		return m, nil
	}
	// Shape 2: jsonb string whose contents are JSON — unwrap one layer.
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("value is neither a JSON object nor a JSON string")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("JSON-string value does not contain a JSON object: %w", err)
	}
	return m, nil
}

// decodeCSVField decodes a bead field that holds a comma-separated value,
// tolerating the real storage shapes: a jsonb string ("C1,C2"), and defensively
// a native jsonb array (["C1","C2"]). An absent/empty field decodes to no
// elements. Elements are trimmed and empties dropped, matching the controller's
// parseSlackChannels.
func decodeCSVField(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Shape 1: jsonb string holding CSV.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return splitCSV(s), nil
	}
	// Shape 2 (defensive): native jsonb array of strings.
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]string, 0, len(arr))
		for _, v := range arr {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("value is neither a CSV string nor a JSON array of strings")
}

// splitCSV splits a comma-separated string into trimmed, non-empty elements.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
