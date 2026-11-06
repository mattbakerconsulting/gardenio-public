package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// queryAppendBeadArrayField atomically appends `element` to the string-array
// JSONB field `field`, skipping the write when the element is already present
// (append-if-absent). It runs as a single UPDATE guarded by a NOT-contains
// predicate, so there is no read-modify-write window and two concurrent
// appends of distinct elements can never clobber one another — the append leg
// of the phase_order/step_order correctness rail (ka-0000000988).
//
// The field key flows into the jsonb path as a BOUND text[] parameter
// (ARRAY[$2]::text[]) and the `->` operand as a bound text parameter, never
// Sprintf-interpolated — so the AUDIT-2026-04-18 §C1 injection surface that
// forced queryIncrementBeadFields to string-build simply does not exist here.
// We still validate the key with store.ValidFieldKey for defense-in-depth and
// a friendly error, matching the increment path.
//
// Returns (bead, true) on a real append, (bead, false) on an idempotent no-op
// (element already listed — no updated_at bump, no event), and sql.ErrNoRows
// when the bead does not exist.
func queryAppendBeadArrayField(ctx context.Context, db executor, id, field, element string) (*model.Bead, bool, error) {
	if !store.ValidFieldKey(field) {
		return nil, false, fmt.Errorf("%w: %q", store.ErrInvalidFieldKey, field)
	}

	// jsonb_set(target, ARRAY[field], oldArray || element, create_if_missing).
	// The WHERE clause's NOT (@>) makes an already-present element a no-op
	// (0 rows) so the write is idempotent AND the returned bead reflects a real
	// state change only when a change actually happened.
	row := db.QueryRowContext(ctx, `
		UPDATE beads SET
			fields = jsonb_set(
				COALESCE(fields, '{}'::jsonb),
				ARRAY[$2]::text[],
				COALESCE(fields -> $2, '[]'::jsonb) || to_jsonb($3::text),
				true
			),
			updated_at = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
		  AND NOT (COALESCE(fields -> $2, '[]'::jsonb) @> to_jsonb($3::text))
		RETURNING `+beadColumns,
		id, field, element,
	)
	bead, err := scanBead(row)
	if err == nil {
		return bead, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	// 0 rows: either the bead is absent, or the element is already present
	// (no-op). Disambiguate with a plain read; a live bead means idempotent
	// no-op, a missing one bubbles sql.ErrNoRows.
	existing, gerr := queryGetBeadBaseColumns(ctx, db, id)
	if gerr != nil {
		return nil, false, gerr
	}
	return existing, false, nil
}

// queryInsertBeadArrayField inserts `element` at `index` in the string-array
// JSONB field `field`, shifting the tail right — the POSITIONAL sibling of
// queryAppendBeadArrayField (append is index==len). It is append-if-absent
// idempotent: an element already present is a no-op (changed=false), so a
// re-submitted pour can never relocate an existing phase to a new slot.
//
// Unlike the append/CAS siblings this is NOT a single pure-SQL jsonb_set: a
// positional insert into a POSSIBLY-ABSENT field needs the array read back to
// clamp `index` into [0, len] and to check membership, and jsonb_insert's path
// navigation is itself undefined when the key is missing. So it takes the same
// Go read-modify-write shape as MergeBeadMapField (subfield_edits.go): read the
// whole fields object under `FOR UPDATE`, splice in Go, write it back — both
// statements in ONE transaction so the row lock serializes concurrent
// append/insert writers and no update is lost. `db` MUST therefore be a live
// transaction, which is why the PostgresStore entry point opens one.
//
// Returns (bead, true) on a real insert, (bead, false) on the idempotent
// already-present no-op (no updated_at bump, no event), and sql.ErrNoRows when
// the bead does not exist.
func queryInsertBeadArrayField(ctx context.Context, db executor, id, field, element string, index int) (*model.Bead, bool, error) {
	if !store.ValidFieldKey(field) {
		return nil, false, fmt.Errorf("%w: %q", store.ErrInvalidFieldKey, field)
	}

	fields, err := lockAndReadFields(ctx, db, id)
	if err != nil {
		return nil, false, err
	}

	cur, err := decodeStringArrayField(fields[field])
	if err != nil {
		return nil, false, fmt.Errorf("decoding array field %q: %w", field, err)
	}

	// Append-if-absent: an element already listed is a no-op — never relocated.
	if slices.Contains(cur, element) {
		return scanBeadNoOp(ctx, db, id)
	}

	// Clamp index into [0, len]; index>=len (and any negative, defensively) fold
	// to a tail append / head insert so an out-of-range slot never panics the
	// splice — callers validate the operator-facing range separately.
	if index < 0 {
		index = 0
	}
	if index > len(cur) {
		index = len(cur)
	}
	next := slices.Insert(cur, index, element)

	encoded, err := marshalStringArray(next)
	if err != nil {
		return nil, false, fmt.Errorf("encoding inserted array field: %w", err)
	}
	fields[field] = encoded
	bead, err := writeFields(ctx, db, id, fields)
	if err != nil {
		return nil, false, err
	}
	return bead, true, nil
}

// decodeStringArrayField decodes a bead field holding a string array (the
// phase_order / step_order shape). An absent/empty field decodes to an empty
// slice, so a first-ever insert works against a plan that has no phase_order
// yet. A malformed field (not a JSON string array) is an error, surfaced to the
// caller rather than silently dropped.
func decodeStringArrayField(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("value is not a JSON string array: %w", err)
	}
	return out, nil
}

// queryCASBeadArrayField atomically sets the string-array JSONB field `field`
// to `next`, but ONLY IF its current value equals `expected` (compare-and-swap).
// It is the reorder leg of the correctness rail: a reorder is a blind
// full-array rewrite that, under naive read-modify-write, silently drops a
// concurrent append. The CAS predicate makes the rewrite conditional — a stale
// `expected` matches no row, so the write is skipped and store.ErrCASConflict
// is returned; the caller re-reads, re-permutes against the fresh array, and
// retries (ka-0000000988).
//
// jsonb equality (`=`) is order-sensitive for arrays, so `["a","b"]` never
// equals `["b","a"]` — exactly the semantics a reorder CAS needs. `expected`
// and `next` are marshalled to JSON and bound as jsonb parameters; the field
// key is a bound text[] path parameter (no interpolation), validated for a
// friendly error.
func queryCASBeadArrayField(ctx context.Context, db executor, id, field string, expected, next []string) (*model.Bead, error) {
	if !store.ValidFieldKey(field) {
		return nil, fmt.Errorf("%w: %q", store.ErrInvalidFieldKey, field)
	}
	expectedJSON, err := marshalStringArray(expected)
	if err != nil {
		return nil, fmt.Errorf("marshal expected: %w", err)
	}
	nextJSON, err := marshalStringArray(next)
	if err != nil {
		return nil, fmt.Errorf("marshal next: %w", err)
	}

	row := db.QueryRowContext(ctx, `
		UPDATE beads SET
			fields = jsonb_set(
				COALESCE(fields, '{}'::jsonb),
				ARRAY[$2]::text[],
				$4::jsonb,
				true
			),
			updated_at = NOW()
		WHERE id = $1
		  AND deleted_at IS NULL
		  AND COALESCE(fields -> $2, '[]'::jsonb) = $3::jsonb
		RETURNING `+beadColumns,
		id, field, string(expectedJSON), string(nextJSON),
	)
	bead, err := scanBead(row)
	if err == nil {
		return bead, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// 0 rows: bead missing, or the CAS predicate failed. Disambiguate so the
	// caller can tell "retry me" (conflict) from "no such bead" (404).
	if _, gerr := queryGetBeadBaseColumns(ctx, db, id); gerr != nil {
		return nil, gerr // sql.ErrNoRows when absent, or a real error
	}
	return nil, store.ErrCASConflict
}

// queryGetBeadBaseColumns reads a single live (non-soft-deleted) bead via the
// unjoined base-column projection — no labels/deps/comments hydration. Both
// primitives use it only to DISAMBIGUATE a 0-row write (absent bead vs. no-op /
// CAS conflict), and their success responses are base-column beads too, so the
// heavy JOINed queryGetBead would be wasted work. Returns sql.ErrNoRows when the
// bead does not exist or is soft-deleted.
func queryGetBeadBaseColumns(ctx context.Context, db executor, id string) (*model.Bead, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+beadColumns+` FROM beads WHERE id = $1 AND deleted_at IS NULL`, id)
	return scanBead(row)
}

// marshalStringArray renders a []string as a JSON array, normalising nil to the
// empty array `[]` (never JSON null) so it round-trips through jsonb equality
// against an absent field's COALESCE default of `[]`.
func marshalStringArray(v []string) ([]byte, error) {
	if v == nil {
		v = []string{}
	}
	return json.Marshal(v)
}
