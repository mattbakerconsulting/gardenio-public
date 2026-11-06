package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// jsonPatchEquals is a sqlmock.Argument matcher for a jsonb parameter: it
// unmarshals the driver value and compares it key-for-key against want,
// failing if the caller's patch carries any key beyond what want lists (or
// is missing one). Used below to pin that queryMergeBeadFields' UPDATE
// carries ONLY the caller-supplied extra keys — never anything pulled from a
// prior read of the row.
type jsonPatchEquals struct{ want map[string]any }

func (m jsonPatchEquals) Match(v driver.Value) bool {
	var raw []byte
	switch vv := v.(type) {
	case []byte:
		raw = vv
	case string:
		raw = []byte(vv)
	default:
		return false
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	if len(got) != len(m.want) {
		return false
	}
	for k, wantV := range m.want {
		gotV, ok := got[k]
		if !ok {
			return false
		}
		gb, _ := json.Marshal(gotV)
		wb, _ := json.Marshal(wantV)
		if string(gb) != string(wb) {
			return false
		}
	}
	return true
}

// TestQueryMergeBeadFields_SingleAtomicUpdate is the regression test for
// gd-P6uCMzQqU9 (merge desk send-back on PR #169,
// https://github.com/joyousaversion/gardenio/pull/169#issuecomment-5807750051):
// the old mergeBeadFields helper (internal/server/http.go) did a
// GetBead-then-UpdateBead read-modify-write, a two-round-trip window in
// which a concurrent atomic claim (e.g. ClaimScrubExecution setting
// scrub_execution_claimed) landing between the read and the write got
// silently clobbered by the stale snapshot's overwrite. queryMergeBeadFields
// closes that window by folding the merge into ONE `fields =
// COALESCE(fields, '{}'::jsonb) || $2::jsonb` UPDATE — this test pins that
// shape: exactly one round trip (no preceding SELECT/GetBead), so there is
// no window for another writer's atomic update to land in between.
// newMockDB's t.Cleanup asserts every expectation was met AND that no
// unexpected call was made, so a regression back to Get+UpdateBead (two
// round trips) fails this test either by leaving the single expectation
// unmet or by an unexpected second call.
func TestQueryMergeBeadFields_SingleAtomicUpdate(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsBase).AddRow(
		"gd-scrub-1", nil, nil, nil, "data", "scrub_request", "scrub gd-scrub-1", nil, nil,
		"in_progress", 0, nil, nil, now, nil, now, nil, nil, nil, nil,
		[]byte(`{"taint":"t1","kind":"purge","approval_status":"approved","scrub_execution_claimed":true}`),
		"default", nil, nil, nil,
	)
	mock.ExpectQuery(`UPDATE beads SET\s+fields = COALESCE\(fields, '\{\}'::jsonb\) \|\| \$2::jsonb`).
		WithArgs("gd-scrub-1", sqlmock.AnyArg()).
		WillReturnRows(rows)

	bead, err := queryMergeBeadFields(context.Background(), db, "gd-scrub-1", map[string]any{
		"approval_status": "approved",
		"approved_by":     "someone@example.com",
	})
	if err != nil {
		t.Fatalf("queryMergeBeadFields: %v", err)
	}
	if bead == nil || bead.ID != "gd-scrub-1" {
		t.Fatalf("bead = %+v, want gd-scrub-1", bead)
	}
}

// TestQueryMergeBeadFields_PatchIsExtraOnly confirms the JSON blob sent as
// $2 is built ONLY from the caller's extra map — never from a prior read of
// the bead's existing fields. A patch built from a stale read is exactly
// what let a concurrent claim get silently overwritten pre-fix; a patch
// built purely from `extra` (verified here via a custom sqlmock argument
// matcher, not just a regex on the query text) can only ever carry the keys
// the caller actually named.
func TestQueryMergeBeadFields_PatchIsExtraOnly(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	extra := map[string]any{"approval_status": "approved", "approved_by": "a@example.com"}
	mock.ExpectQuery(`UPDATE beads SET`).
		WithArgs("gd-scrub-2", jsonPatchEquals{want: extra}).
		WillReturnRows(sqlmock.NewRows(beadRowColumnsBase).AddRow(
			"gd-scrub-2", nil, nil, nil, "data", "scrub_request", "scrub gd-scrub-2", nil, nil,
			"in_progress", 0, nil, nil, now, nil, now, nil, nil, nil, nil,
			[]byte(`{}`), "default", nil, nil, nil,
		))

	if _, err := queryMergeBeadFields(context.Background(), db, "gd-scrub-2", extra); err != nil {
		t.Fatalf("queryMergeBeadFields: %v", err)
	}
}

// TestQueryMergeBeadFields_NotFound confirms a merge against a nonexistent
// bead surfaces sql.ErrNoRows (the UPDATE ... RETURNING matches zero rows),
// matching MergeBeadFields' documented contract on store.Store.
func TestQueryMergeBeadFields_NotFound(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`UPDATE beads SET`).
		WithArgs("gd-missing", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(beadRowColumnsBase))

	_, err := queryMergeBeadFields(context.Background(), db, "gd-missing", map[string]any{"x": 1})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("queryMergeBeadFields: err = %v, want sql.ErrNoRows", err)
	}
}

// TestQueryMergeBeadFields_ExcludesSoftDeleted confirms the UPDATE gates on
// deleted_at IS NULL, matching the old GetBead-based path's contract that a
// merge against a soft-deleted bead surfaces not-found rather than silently
// resurrecting its fields (round-4 send-back on PR #169,
// https://github.com/joyousaversion/gardenio/pull/169#issuecomment-5808957484).
func TestQueryMergeBeadFields_ExcludesSoftDeleted(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`UPDATE beads SET\s+fields = COALESCE\(fields, '\{\}'::jsonb\) \|\| \$2::jsonb, \s*updated_at = NOW\(\)\s+WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs("gd-deleted", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(beadRowColumnsBase))

	_, err := queryMergeBeadFields(context.Background(), db, "gd-deleted", map[string]any{"x": 1})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("queryMergeBeadFields: err = %v, want sql.ErrNoRows", err)
	}
}

// TestPostgresStoreMergeBeadFields_ReplaysReferences pins that
// PostgresStore.MergeBeadFields — the store-interface entry point every
// caller (scrub fork successor_bead_id, escalation replay result.bead_id,
// decisions) actually uses — runs replaceBeadReferences on the RETURNING
// row inside the SAME transaction as the merge UPDATE, exactly like
// UpdateBead does. Round-4 send-back on PR #169
// (https://github.com/joyousaversion/gardenio/pull/169#issuecomment-5808957484):
// before this fix, mergeBeadFields' switch from Get+UpdateBead to a bare
// queryMergeBeadFields call silently stopped producing bead_references rows
// for any bead-id written into fields. The mocked bead is returned with a
// fields blob carrying a gd-id so extractFieldsRefs finds a target and the
// DELETE is followed by an INSERT — a regression back to the bare
// queryMergeBeadFields call leaves the INSERT expectation unmet.
func TestPostgresStoreMergeBeadFields_ReplaysReferences(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}
	now := time.Now().UTC()

	mock.ExpectBegin()
	mock.ExpectQuery(`UPDATE beads SET\s+fields = COALESCE`).
		WithArgs("gd-scrub-3", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(beadRowColumnsBase).AddRow(
			"gd-scrub-3", nil, nil, nil, "data", "scrub_request", "scrub gd-scrub-3", nil, nil,
			"in_progress", 0, nil, nil, now, nil, now, nil, nil, nil, nil,
			[]byte(`{"successor_bead_id":"gd-abc1234567"}`), "default", nil, nil, nil,
		))
	mock.ExpectExec(`DELETE FROM bead_references`).
		WithArgs("gd-scrub-3", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-scrub-3", "fields", "gd-abc1234567").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	bead, err := st.MergeBeadFields(context.Background(), "gd-scrub-3", map[string]any{
		"successor_bead_id": "gd-abc1234567",
	})
	if err != nil {
		t.Fatalf("MergeBeadFields: %v", err)
	}
	if bead == nil || bead.ID != "gd-scrub-3" {
		t.Fatalf("bead = %+v, want gd-scrub-3", bead)
	}
}

// TestPostgresStoreMergeBeadFields_ReadCursorsWriteNoReferences: the read
// cursor write (kbeads POST /v1/users/me/read-cursors) merges a map of
// channel id to message id onto the person's user bead. It must clear the
// fields bucket and insert no bead_references row, or each message's
// mentioned-by would name who has read up to it. sqlmock fails the call on
// the unexpected INSERT.
func TestPostgresStoreMergeBeadFields_ReadCursorsWriteNoReferences(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}
	now := time.Now().UTC()

	mock.ExpectBegin()
	mock.ExpectQuery(`UPDATE beads SET\s+fields = COALESCE`).
		WithArgs("gd-alice12345", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(beadRowColumnsBase).AddRow(
			"gd-alice12345", nil, nil, nil, "config", "user", "alice", nil, nil,
			"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil,
			[]byte(`{"email":"alice@example.com","read_cursors":{"gd-chan123456":"gd-msg1234567"}}`), "default", nil, nil, nil,
		))
	mock.ExpectExec(`DELETE FROM bead_references`).
		WithArgs("gd-alice12345", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	if _, err := st.MergeBeadFields(context.Background(), "gd-alice12345", map[string]any{
		"read_cursors": map[string]any{"gd-chan123456": "gd-msg1234567"},
	}); err != nil {
		t.Fatalf("MergeBeadFields: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestTxStoreMergeBeadFields_ReplaysReferences is the txStore analogue of
// TestPostgresStoreMergeBeadFields_ReplaysReferences — the transaction-bound
// entry point used inside RunInTransaction callers gets the same
// reference-replay treatment, using the caller's existing *sql.Tx rather
// than opening a new one.
func TestTxStoreMergeBeadFields_ReplaysReferences(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectBegin()
	sqlTx, err := db.Begin()
	if err != nil {
		t.Fatalf("db.Begin: %v", err)
	}
	txs := &txStore{tx: sqlTx}

	mock.ExpectQuery(`UPDATE beads SET\s+fields = COALESCE`).
		WithArgs("gd-scrub-4", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(beadRowColumnsBase).AddRow(
			"gd-scrub-4", nil, nil, nil, "data", "scrub_request", "scrub gd-scrub-4", nil, nil,
			"in_progress", 0, nil, nil, now, nil, now, nil, nil, nil, nil,
			[]byte(`{"successor_bead_id":"gd-def1234567"}`), "default", nil, nil, nil,
		))
	mock.ExpectExec(`DELETE FROM bead_references`).
		WithArgs("gd-scrub-4", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-scrub-4", "fields", "gd-def1234567").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	bead, err := txs.MergeBeadFields(context.Background(), "gd-scrub-4", map[string]any{
		"successor_bead_id": "gd-def1234567",
	})
	if err != nil {
		t.Fatalf("txStore.MergeBeadFields: %v", err)
	}
	if bead == nil || bead.ID != "gd-scrub-4" {
		t.Fatalf("bead = %+v, want gd-scrub-4", bead)
	}
	if err := sqlTx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
