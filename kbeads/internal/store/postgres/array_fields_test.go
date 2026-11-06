package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// TestQueryAppendBeadArrayField_RejectsInvalidFieldKey guards the same
// AUDIT-2026-04-18 §C1 injection surface increment closes: even though the
// field key flows into the jsonb path as a bound text[] parameter (not
// Sprintf-interpolated), a non-identifier key is rejected up front so the
// error is uniform with the increment path and the grammar stays conservative.
func TestQueryAppendBeadArrayField_RejectsInvalidFieldKey(t *testing.T) {
	db, _ := newMockDB(t)

	bad := []string{
		"phase order",
		"phase-order",
		`x')); DROP TABLE beads; --`,
		`x','}',(SELECT 1); --`,
		"",
	}
	for _, key := range bad {
		t.Run(key, func(t *testing.T) {
			_, _, err := queryAppendBeadArrayField(context.Background(), db, "gd-test", key, "gd-x")
			if !errors.Is(err, store.ErrInvalidFieldKey) {
				t.Fatalf("expected ErrInvalidFieldKey for key %q, got %v", key, err)
			}
		})
	}
	// No SQL expectations set — validation must short-circuit before any query.
}

// TestQueryCASBeadArrayField_RejectsInvalidFieldKey is the CAS-side mirror.
func TestQueryCASBeadArrayField_RejectsInvalidFieldKey(t *testing.T) {
	db, _ := newMockDB(t)

	_, err := queryCASBeadArrayField(context.Background(), db, "gd-test", "bad key", []string{"a"}, []string{"a"})
	if !errors.Is(err, store.ErrInvalidFieldKey) {
		t.Fatalf("expected ErrInvalidFieldKey, got %v", err)
	}
	if !strings.Contains(err.Error(), "bad key") {
		t.Errorf("expected error to name the offending key; got %v", err)
	}
}

// TestQueryAppendBeadArrayField_AppendsWhenChanged verifies the happy path: the
// UPDATE returns a row (element was absent), so the primitive reports changed=true
// and hands back the scanned bead without a follow-up read.
func TestQueryAppendBeadArrayField_AppendsWhenChanged(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsBase).AddRow(
		"gd-plan", nil, nil, nil, "issue", "epic", "Plan", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil,
		[]byte(`{"phase_order":["gd-a","gd-b"]}`), "default", nil, nil, nil,
	)
	// $1=id, $2=field, $3=element; the WHERE NOT(@>) guard makes it a no-op when present.
	mock.ExpectQuery("UPDATE beads SET").
		WithArgs("gd-plan", "phase_order", "gd-b").
		WillReturnRows(rows)

	bead, changed, err := queryAppendBeadArrayField(context.Background(), db, "gd-plan", "phase_order", "gd-b")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true (UPDATE returned a row)")
	}
	if bead == nil || bead.ID != "gd-plan" {
		t.Errorf("bead = %+v, want gd-plan", bead)
	}
}

// TestQueryAppendBeadArrayField_NoOpWhenPresent verifies the idempotent no-op:
// the UPDATE matches 0 rows (element already listed), so the primitive falls
// back to a plain read, returns the live bead, and reports changed=false —
// no updated_at bump, no event.
func TestQueryAppendBeadArrayField_NoOpWhenPresent(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	// UPDATE ... RETURNING matches no row (guard failed → already present).
	mock.ExpectQuery("UPDATE beads SET").
		WithArgs("gd-plan", "phase_order", "gd-a").
		WillReturnRows(sqlmock.NewRows(beadRowColumnsBase))

	// Fallback read to disambiguate no-op vs missing bead.
	readRows := sqlmock.NewRows(beadRowColumnsBase).AddRow(
		"gd-plan", nil, nil, nil, "issue", "epic", "Plan", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil,
		[]byte(`{"phase_order":["gd-a"]}`), "default", nil, nil, nil,
	)
	mock.ExpectQuery("SELECT .+ FROM beads WHERE id = \\$1").
		WithArgs("gd-plan").
		WillReturnRows(readRows)

	bead, changed, err := queryAppendBeadArrayField(context.Background(), db, "gd-plan", "phase_order", "gd-a")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if changed {
		t.Errorf("changed = true, want false (element already present)")
	}
	if bead == nil || bead.ID != "gd-plan" {
		t.Errorf("bead = %+v, want gd-plan", bead)
	}
}

// TestQueryCASBeadArrayField_Swaps verifies a matching CAS returns the updated
// bead. The expected/next arrays are bound as jsonb-encoded params ($3/$4).
func TestQueryCASBeadArrayField_Swaps(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsBase).AddRow(
		"gd-plan", nil, nil, nil, "issue", "epic", "Plan", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil,
		[]byte(`{"phase_order":["gd-b","gd-a"]}`), "default", nil, nil, nil,
	)
	mock.ExpectQuery("UPDATE beads SET").
		WithArgs("gd-plan", "phase_order", `["gd-a","gd-b"]`, `["gd-b","gd-a"]`).
		WillReturnRows(rows)

	bead, err := queryCASBeadArrayField(context.Background(), db, "gd-plan", "phase_order",
		[]string{"gd-a", "gd-b"}, []string{"gd-b", "gd-a"})
	if err != nil {
		t.Fatalf("cas: %v", err)
	}
	if bead == nil || bead.ID != "gd-plan" {
		t.Errorf("bead = %+v, want gd-plan", bead)
	}
}

// TestQueryCASBeadArrayField_ConflictOnStaleExpected verifies the correctness
// rail: when the UPDATE matches 0 rows AND the bead still exists, the current
// value differs from `expected` (a concurrent writer changed it) → ErrCASConflict.
func TestQueryCASBeadArrayField_ConflictOnStaleExpected(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	// CAS predicate fails → 0 rows.
	mock.ExpectQuery("UPDATE beads SET").
		WithArgs("gd-plan", "phase_order", `["gd-a","gd-b"]`, `["gd-b","gd-a"]`).
		WillReturnRows(sqlmock.NewRows(beadRowColumnsBase))

	// Disambiguation read: bead exists → conflict, not 404.
	readRows := sqlmock.NewRows(beadRowColumnsBase).AddRow(
		"gd-plan", nil, nil, nil, "issue", "epic", "Plan", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil,
		[]byte(`{"phase_order":["gd-a","gd-b","gd-c"]}`), "default", nil, nil, nil,
	)
	mock.ExpectQuery("SELECT .+ FROM beads WHERE id = \\$1").
		WithArgs("gd-plan").
		WillReturnRows(readRows)

	_, err := queryCASBeadArrayField(context.Background(), db, "gd-plan", "phase_order",
		[]string{"gd-a", "gd-b"}, []string{"gd-b", "gd-a"})
	if !errors.Is(err, store.ErrCASConflict) {
		t.Fatalf("expected ErrCASConflict, got %v", err)
	}
}

// insertReadRow builds the FOR-UPDATE read result lockAndReadFields expects: a
// single "fields" column carrying the plan's raw fields blob.
func insertReadRow(fields string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"fields"}).AddRow([]byte(fields))
}

// TestQueryInsertBeadArrayField_RejectsInvalidFieldKey is the insert-side mirror
// of the append/CAS field-key guards.
func TestQueryInsertBeadArrayField_RejectsInvalidFieldKey(t *testing.T) {
	db, _ := newMockDB(t)
	_, _, err := queryInsertBeadArrayField(context.Background(), db, "gd-plan", "bad key", "gd-x", 0)
	if !errors.Is(err, store.ErrInvalidFieldKey) {
		t.Fatalf("expected ErrInvalidFieldKey, got %v", err)
	}
	// No SQL expectations — validation short-circuits before any query.
}

// TestQueryInsertBeadArrayField_InsertsAtIndex is the headline case: inserting at
// a mid slot shifts the tail right, and the written blob carries the exact new
// order. It reads the locked row, then expects the UPDATE with the spliced array.
func TestQueryInsertBeadArrayField_InsertsAtIndex(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COALESCE\(fields, '\{\}'::jsonb\) FROM beads WHERE id = \$1 AND deleted_at IS NULL FOR UPDATE`).
		WithArgs("gd-plan").
		WillReturnRows(insertReadRow(`{"phase_order":["gd-a","gd-b"]}`))
	// Insert gd-mid at slot 1 → ["gd-a","gd-mid","gd-b"], other fields preserved.
	mock.ExpectQuery(`UPDATE beads SET fields = \$2::jsonb, updated_at = NOW\(\)`).
		WithArgs("gd-plan", fieldsContain(t, `"phase_order":["gd-a","gd-mid","gd-b"]`)).
		WillReturnRows(beadRowWithFields(`{"phase_order":["gd-a","gd-mid","gd-b"]}`))

	bead, changed, err := queryInsertBeadArrayField(context.Background(), db, "gd-plan", "phase_order", "gd-mid", 1)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true")
	}
	if bead == nil {
		t.Fatalf("bead = nil")
	}
}

// TestQueryInsertBeadArrayField_InsertsAtHead covers slot 0 — the new phase
// becomes the first.
func TestQueryInsertBeadArrayField_InsertsAtHead(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("gd-plan").
		WillReturnRows(insertReadRow(`{"phase_order":["gd-a","gd-b"]}`))
	mock.ExpectQuery(`UPDATE beads SET fields = \$2::jsonb`).
		WithArgs("gd-plan", fieldsContain(t, `"phase_order":["gd-head","gd-a","gd-b"]`)).
		WillReturnRows(beadRowWithFields(`{"phase_order":["gd-head","gd-a","gd-b"]}`))

	_, changed, err := queryInsertBeadArrayField(context.Background(), db, "gd-plan", "phase_order", "gd-head", 0)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true")
	}
}

// TestQueryInsertBeadArrayField_IndexAtLenAppends verifies index==len is exactly
// a tail append — the equivalence that makes the default (append) a special case
// of the general (insert).
func TestQueryInsertBeadArrayField_IndexAtLenAppends(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("gd-plan").
		WillReturnRows(insertReadRow(`{"phase_order":["gd-a","gd-b"]}`))
	mock.ExpectQuery(`UPDATE beads SET fields = \$2::jsonb`).
		WithArgs("gd-plan", fieldsContain(t, `"phase_order":["gd-a","gd-b","gd-tail"]`)).
		WillReturnRows(beadRowWithFields(`{"phase_order":["gd-a","gd-b","gd-tail"]}`))

	_, changed, err := queryInsertBeadArrayField(context.Background(), db, "gd-plan", "phase_order", "gd-tail", 2)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true")
	}
}

// TestQueryInsertBeadArrayField_IndexPastLenClamps verifies an index beyond len
// folds to a tail append (defensive clamp) rather than panicking the splice.
func TestQueryInsertBeadArrayField_IndexPastLenClamps(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("gd-plan").
		WillReturnRows(insertReadRow(`{"phase_order":["gd-a"]}`))
	mock.ExpectQuery(`UPDATE beads SET fields = \$2::jsonb`).
		WithArgs("gd-plan", fieldsContain(t, `"phase_order":["gd-a","gd-x"]`)).
		WillReturnRows(beadRowWithFields(`{"phase_order":["gd-a","gd-x"]}`))

	_, changed, err := queryInsertBeadArrayField(context.Background(), db, "gd-plan", "phase_order", "gd-x", 99)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true")
	}
}

// TestQueryInsertBeadArrayField_AbsentFieldStartsEmpty verifies a first-ever
// insert works against a plan whose phase_order does not exist yet.
func TestQueryInsertBeadArrayField_AbsentFieldStartsEmpty(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("gd-plan").
		WillReturnRows(insertReadRow(`{}`))
	mock.ExpectQuery(`UPDATE beads SET fields = \$2::jsonb`).
		WithArgs("gd-plan", fieldsContain(t, `"phase_order":["gd-first"]`)).
		WillReturnRows(beadRowWithFields(`{"phase_order":["gd-first"]}`))

	_, changed, err := queryInsertBeadArrayField(context.Background(), db, "gd-plan", "phase_order", "gd-first", 0)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true")
	}
}

// TestQueryInsertBeadArrayField_AlreadyPresentIsNoOp verifies append-if-absent:
// an element already in the array is NOT relocated — no UPDATE, just a read-back,
// changed=false. This is what stops a re-submitted pour from reordering an
// existing phase.
func TestQueryInsertBeadArrayField_AlreadyPresentIsNoOp(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FOR UPDATE`).
		WithArgs("gd-plan").
		WillReturnRows(insertReadRow(`{"phase_order":["gd-a","gd-b"]}`))
	// No UPDATE — only the base-column read-back for the no-op return.
	mock.ExpectQuery(`SELECT .+ FROM beads WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("gd-plan").
		WillReturnRows(beadRowWithFields(`{"phase_order":["gd-a","gd-b"]}`))

	_, changed, err := queryInsertBeadArrayField(context.Background(), db, "gd-plan", "phase_order", "gd-a", 0)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if changed {
		t.Errorf("changed = true, want false (element already present is a no-op, never relocated)")
	}
}

// TestDecodeStringArrayField covers the empty, populated, and malformed shapes.
func TestDecodeStringArrayField(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{"empty raw", ``, []string{}, false},
		{"populated", `["gd-a","gd-b"]`, []string{"gd-a", "gd-b"}, false},
		{"empty array", `[]`, []string{}, false},
		{"malformed object", `{"x":1}`, nil, true},
		{"malformed number", `42`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeStringArrayField([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got[%d]=%q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestMarshalStringArray_NilIsEmptyArray guards the nil→[] normalisation that
// lets an absent field (COALESCE default []) compare-and-swap cleanly.
func TestMarshalStringArray_NilIsEmptyArray(t *testing.T) {
	b, err := marshalStringArray(nil)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "[]" {
		t.Errorf("marshalStringArray(nil) = %q, want []", string(b))
	}
}
